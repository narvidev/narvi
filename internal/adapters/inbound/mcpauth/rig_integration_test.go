//go:build integration

package mcpauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/inbound/mcpauth"
	"github.com/narvidev/narvi/internal/adapters/outbound/cimdfetch"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/mcpclient"
	"github.com/narvidev/narvi/internal/domain/mcpscope"
	"github.com/narvidev/narvi/internal/platform"
)

// rigBase is the PublicBaseURL every test in this package runs against.
const rigBase = "http://narvi.test"

// Redirect URIs the rig's pre-registered client carries.
const (
	loopbackRedirect  = "http://127.0.0.1/callback"
	httpsRedirect     = "https://client.example/cb"
	localhostRedirect = "http://localhost:8080/cb"
)

// asRig is a real-Postgres authorization server mounted the way
// controlplane/serve.go mounts it (no cookie middleware on any /oauth
// route), plus a /mcp stand-in behind the real auth.RequireMCPBearer so a
// test can ask "does this token still work" -- and, through the scopes
// the stand-in echoes back, "what may it see" -- and the two Settings
// revocation routes, the real httpapi handlers behind the real cookie
// middleware, so a test can race a revocation against an issuance.
// controlplane's own TestOAuth_ProductionRouter proves the production
// wiring itself.
type asRig struct {
	pool         *pgxpool.Pool
	users        *postgres.UserStore
	userSessions *postgres.UserSessionStore
	clients      *postgres.MCPOAuthClientStore
	grants       *postgres.MCPOAuthGrantStore
	auditLog     *postgres.AuditLogStore
	server       *mcpauth.Server
	router       http.Handler
	client       sqlcgen.McpOauthClient
	// documents is the rig's metadata fetcher: the client ID metadata
	// documents it serves, from memory -- no test here reaches the network
	// (cimdfetch's own tests prove the real fetcher's guard).
	documents *fakeFetcher
}

// rigOptions configures newASRigWith.
type rigOptions struct {
	// mechanisms is the authorization server's (and the /mcp stand-in's)
	// accepted registration mechanisms.
	mechanisms mcpclient.Mechanisms
	// timeouts, when set, replaces platform.DefaultTimeouts().
	timeouts *platform.Timeouts
}

// newASRig is the rig with both client-registration mechanisms on.
func newASRig(t *testing.T) *asRig {
	t.Helper()
	return newASRigWith(t, rigOptions{mechanisms: mcpclient.Mechanisms{MetadataDocuments: true, DynamicRegistration: true}})
}

func newASRigWith(t *testing.T, opts rigOptions) *asRig {
	t.Helper()
	pool := IntegrationTestPool(t)
	r := &asRig{
		pool:         pool,
		users:        postgres.NewUserStore(pool),
		userSessions: postgres.NewUserSessionStore(pool),
		clients:      postgres.NewMCPOAuthClientStore(pool),
		grants:       postgres.NewMCPOAuthGrantStore(pool),
		auditLog:     postgres.NewAuditLogStore(pool),
		documents:    newFakeFetcher(),
	}
	r.build(t, opts)
	r.client = r.newClient(t, "narvi_mcp_c_rig", "Editor Plugin", loopbackRedirect, httpsRedirect, localhostRedirect)
	return r
}

// rebuilt is r with its server and router built again for opts, on the
// same database, stores, client and documents -- a deployment restarted
// with other settings.
func (r *asRig) rebuilt(t *testing.T, opts rigOptions) *asRig {
	t.Helper()
	next := *r
	next.build(t, opts)
	return &next
}

// build constructs r's authorization server and router for opts.
func (r *asRig) build(t *testing.T, opts rigOptions) {
	t.Helper()
	pool := r.pool
	timeouts := platform.DefaultTimeouts()
	if opts.timeouts != nil {
		timeouts = *opts.timeouts
	}
	var err error
	r.server, err = mcpauth.New(mcpauth.Config{
		PublicBaseURL: rigBase,
		Enabled:       true,
		Scopes:        []mcpscope.Scope{mcpscope.Read},
		Timeouts:      timeouts,
		Mechanisms:    opts.mechanisms,
	}, mcpauth.Deps{
		Pool:         pool,
		Clients:      r.clients,
		Grants:       r.grants,
		UserSessions: r.userSessions,
		Users:        r.users,
		AuditLog:     r.auditLog,
		Metadata:     r.documents,
	})
	if err != nil {
		t.Fatalf("mcpauth.New: %v", err)
	}

	ids := r.server.Identifiers()
	router := chi.NewRouter()
	router.Route("/.well-known/oauth-protected-resource", func(rt chi.Router) {
		rt.Get("/mcp", r.server.ProtectedResourceMetadata)
	})
	router.Route("/.well-known/oauth-authorization-server", func(rt chi.Router) {
		rt.Get("/oauth", r.server.AuthorizationServerMetadata)
	})
	router.Route("/oauth", func(rt chi.Router) {
		rt.Get("/authorize", r.server.Authorize)
		rt.Get("/consent", r.server.ConsentPage)
		rt.Post("/consent", r.server.ConsentDecision)
		rt.Post("/token", r.server.Token)
		rt.Post("/revoke", r.server.Revoke)
		// Mounted without controlplane's enabled-gate and rate limit:
		// TestOAuth_ProductionRouter proves both on the production router.
		rt.Post("/register", r.server.Register)
	})
	router.Route("/mcp", func(rt chi.Router) {
		rt.Use(auth.RequireMCPBearer(r.grants, auth.MCPBearerConfig{
			Resource:              ids.Resource,
			ResourceMetadataURL:   ids.ProtectedResourceMetadataURL,
			Scopes:                []string{"mcp:read"},
			LastUsedWriteInterval: platform.DefaultTimeouts().MCPGrantLastUsedWriteInterval,
			ClientMechanisms:      opts.mechanisms,
		}))
		// The stand-in answers 200 with the scopes the bearer gate
		// attached to the request: exactly what tool visibility is
		// decided from (technical plan §43.17).
		rt.Post("/", func(w http.ResponseWriter, req *http.Request) {
			g, _ := platform.MCPGrantFromContext(req.Context())
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(g.Scopes)
		})
	})
	// Mounted exactly like controlplane/serve.go (technical plan
	// §43.15/§43.18).
	router.Route("/api/me/mcp-authorizations", func(rt chi.Router) {
		rt.Use(auth.Middleware(r.userSessions, r.users))
		rt.Delete("/{authorizationID}", httpapi.RevokeMyMCPAuthorization(pool, r.grants, r.clients, r.auditLog))
	})
	router.Route("/api/mcp-clients", func(rt chi.Router) {
		rt.Use(auth.Middleware(r.userSessions, r.users))
		rt.Delete("/{clientID}", httpapi.DeleteMCPClient(pool, r.clients, r.grants, r.auditLog))
	})
	r.router = router
}

func (r *asRig) newClient(t *testing.T, clientID, name string, redirects ...string) sqlcgen.McpOauthClient {
	t.Helper()
	c, err := r.clients.Create(context.Background(), sqlcgen.CreateMCPOAuthClientParams{
		ClientID:     clientID,
		Kind:         sqlcgen.McpOauthClientKindPreregistered,
		ClientName:   name,
		RedirectUris: redirects,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	return c
}

// newUser creates a user of role and a live session cookie for them.
func (r *asRig) newUser(t *testing.T, role sqlcgen.UserRole) (sqlcgen.User, string) {
	t.Helper()
	ctx := context.Background()
	email := fmt.Sprintf("mcpauth-%s-%d@example.com", role, time.Now().UnixNano())
	u, err := r.users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: email, DisplayName: "MCP Auth Test", Role: role})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    u.ID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return u, token
}

// do serves one request through the rig's router.
func (r *asRig) do(method, target, body string, headers map[string]string, cookie string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: cookie})
	}
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func newVerifier(t *testing.T) string {
	t.Helper()
	v, err := platform.GenerateToken() // 43 base64url characters: a valid PKCE verifier
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// authorizeParams is a valid authorization request for the rig's client;
// tests override single parameters.
func (r *asRig) authorizeParams(verifier string) url.Values {
	return url.Values{
		"client_id":             {r.client.ClientID},
		"redirect_uri":          {"http://127.0.0.1:51234/callback"},
		"response_type":         {"code"},
		"code_challenge":        {s256(verifier)},
		"code_challenge_method": {"S256"},
		"resource":              {rigBase + "/mcp"},
		"scope":                 {"mcp:read"},
		"state":                 {"state-123"},
	}
}

func (r *asRig) authorize(params url.Values, cookie string) *httptest.ResponseRecorder {
	return r.do(http.MethodGet, "/oauth/authorize?"+params.Encode(), "", nil, cookie)
}

var consentRequestPattern = regexp.MustCompile(`^/oauth/consent\?request=([0-9a-f-]{36})$`)

// startConsent runs a valid authorization request as the signed-in user
// and returns the stored request's id.
func (r *asRig) startConsent(t *testing.T, params url.Values, cookie string) string {
	t.Helper()
	rec := r.authorize(params, cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize: status %d, body %s", rec.Code, rec.Body.String())
	}
	m := consentRequestPattern.FindStringSubmatch(rec.Header().Get("Location"))
	if m == nil {
		t.Fatalf("authorize: Location = %q, want the consent page", rec.Header().Get("Location"))
	}
	return m[1]
}

var noncePattern = regexp.MustCompile(`name="nonce" value="([^"]+)"`)

// renderConsent GETs the consent page and returns its body and nonce.
func (r *asRig) renderConsent(t *testing.T, requestID, cookie string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	rec := r.do(http.MethodGet, "/oauth/consent?request="+requestID, "", nil, cookie)
	if rec.Code != http.StatusOK {
		return rec, ""
	}
	m := noncePattern.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("consent page carries no nonce: %s", rec.Body.String())
	}
	return rec, m[1]
}

// sameOriginHeaders are what a browser sends on the consent page's own
// form POST.
func sameOriginHeaders() map[string]string {
	return map[string]string{
		"Content-Type":   "application/x-www-form-urlencoded",
		"Origin":         rigBase,
		"Sec-Fetch-Site": "same-origin",
	}
}

func (r *asRig) postConsent(form url.Values, headers map[string]string, cookie string) *httptest.ResponseRecorder {
	return r.do(http.MethodPost, "/oauth/consent", form.Encode(), headers, cookie)
}

// approve runs the whole browser half of the flow and returns the parsed
// redirect back to the client.
func (r *asRig) approve(t *testing.T, params url.Values, cookie string, scopes ...string) *url.URL {
	t.Helper()
	requestID := r.startConsent(t, params, cookie)
	_, nonce := r.renderConsent(t, requestID, cookie)
	if nonce == "" {
		t.Fatalf("consent page did not render")
	}
	form := url.Values{"request": {requestID}, "nonce": {nonce}, "decision": {"approve"}}
	for _, s := range scopes {
		form.Add("scope", s)
	}
	rec := r.postConsent(form, sameOriginHeaders(), cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("approve: status %d, body %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("approve: Location %q: %v", rec.Header().Get("Location"), err)
	}
	return loc
}

// exchangeForm is a valid token request for code.
func (r *asRig) exchangeForm(code, verifier string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"client_id":     {r.client.ClientID},
		"redirect_uri":  {"http://127.0.0.1:51234/callback"},
		"resource":      {rigBase + "/mcp"},
	}
}

func (r *asRig) exchange(form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for k, v := range headers {
		h[k] = v
	}
	return r.do(http.MethodPost, "/oauth/token", form.Encode(), h, "")
}

type tokenBody struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	Scope            string `json:"scope"`
	RefreshToken     string `json:"refresh_token"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func decodeToken(t *testing.T, rec *httptest.ResponseRecorder) tokenBody {
	t.Helper()
	var b tokenBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("token response is not JSON: %v (%s)", err, rec.Body.String())
	}
	return b
}

// issueToken runs one whole authorization flow approving exactly scopes
// and returns the live access token and the scope its token response
// named.
func (r *asRig) issueToken(t *testing.T, cookie string, scopes ...string) (token, scope string) {
	t.Helper()
	verifier := newVerifier(t)
	loc := r.approve(t, r.authorizeParams(verifier), cookie, scopes...)
	rec := r.exchange(r.exchangeForm(loc.Query().Get("code"), verifier), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange: status %d body %s", rec.Code, rec.Body.String())
	}
	body := decodeToken(t, rec)
	return body.AccessToken, body.Scope
}

// issuePair runs one whole authorization flow approving exactly scopes and
// returns the whole token response: the access token and the refresh token
// issued beside it.
func (r *asRig) issuePair(t *testing.T, cookie string, scopes ...string) tokenBody {
	t.Helper()
	verifier := newVerifier(t)
	loc := r.approve(t, r.authorizeParams(verifier), cookie, scopes...)
	rec := r.exchange(r.exchangeForm(loc.Query().Get("code"), verifier), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange: status %d body %s", rec.Code, rec.Body.String())
	}
	body := decodeToken(t, rec)
	if body.RefreshToken == "" {
		t.Fatalf("exchange issued no refresh token: %s", rec.Body.String())
	}
	return body
}

// refreshForm is a valid refresh request for refreshToken from the rig's
// client -- with no resource and no scope, exactly what a standard OAuth
// library sends.
func (r *asRig) refreshForm(refreshToken string) url.Values {
	return url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {r.client.ClientID},
	}
}

// revoke posts form to the RFC 7009 revocation endpoint.
func (r *asRig) revoke(form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for k, v := range headers {
		h[k] = v
	}
	return r.do(http.MethodPost, "/oauth/revoke", form.Encode(), h, "")
}

// refreshRow reads the refresh token row for plaintext (by its hash).
func (r *asRig) refreshRow(t *testing.T, plaintext string) sqlcgen.McpOauthRefreshToken {
	t.Helper()
	row, err := r.grants.GetRefreshTokenByHash(context.Background(), platform.HashToken(plaintext))
	if err != nil {
		t.Fatalf("read refresh token row: %v", err)
	}
	return row
}

// callMCP presents token at the rig's bearer-protected /mcp.
func (r *asRig) callMCP(token string) int {
	return r.do(http.MethodPost, "/mcp", "{}", map[string]string{"Authorization": "Bearer " + token}, "").Code
}

// mcpScopes presents token at the rig's /mcp and returns the status and
// the scopes the real bearer gate attached to the request (nil unless the
// call was accepted).
func (r *asRig) mcpScopes(t *testing.T, token string) (int, []string) {
	t.Helper()
	rec := r.do(http.MethodPost, "/mcp", "{}", map[string]string{"Authorization": "Bearer " + token}, "")
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var scopes []string
	if err := json.Unmarshal(rec.Body.Bytes(), &scopes); err != nil {
		t.Fatalf("/mcp stand-in body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, scopes
}

// auditActions returns (action, detail) for every audit row about one MCP
// authorization, oldest first.
func (r *asRig) auditRows(t *testing.T, grantID string) []auditRow {
	t.Helper()
	rows, err := r.pool.Query(context.Background(),
		`SELECT action, detail_json, actor_user_id FROM audit_log WHERE resource_type = 'mcp_authorization' AND resource_id = $1 ORDER BY created_at, id`, grantID)
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		var detail []byte
		if err := rows.Scan(&a.action, &detail, &a.actor); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		if err := json.Unmarshal(detail, &a.detail); err != nil {
			t.Fatalf("audit detail: %v", err)
		}
		out = append(out, a)
	}
	return out
}

type auditRow struct {
	action string
	detail map[string]any
	actor  pgtype.UUID
}

// grantIDs returns every grant id for userID.
func (r *asRig) grantIDs(t *testing.T, userID pgtype.UUID) []string {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `SELECT id::text FROM mcp_oauth_grants WHERE user_id = $1`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// fakeDocument is one document the fake fetcher serves.
type fakeDocument struct {
	body      string
	maxAge    time.Duration
	hasMaxAge bool
	err       error
}

// fakeFetcher is an in-memory mcpauth.MetadataFetcher: it serves the
// documents a test set, counting fetches per URL.
type fakeFetcher struct {
	mu      sync.Mutex
	docs    map[string]fakeDocument
	fetches map[string]int
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{docs: map[string]fakeDocument{}, fetches: map[string]int{}}
}

func (f *fakeFetcher) Fetch(_ context.Context, clientIDURL string) (cimdfetch.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches[clientIDURL]++
	d, ok := f.docs[clientIDURL]
	if !ok {
		return cimdfetch.Result{}, errors.New("fake fetcher: no document at this URL")
	}
	if d.err != nil {
		return cimdfetch.Result{}, d.err
	}
	return cimdfetch.Result{Body: []byte(d.body), MaxAge: d.maxAge, HasMaxAge: d.hasMaxAge}, nil
}

func (f *fakeFetcher) set(clientIDURL string, d fakeDocument) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs[clientIDURL] = d
}

func (f *fakeFetcher) count(clientIDURL string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches[clientIDURL]
}

// metadataDocument renders a valid client ID metadata document for
// clientIDURL naming name and redirects.
func metadataDocument(clientIDURL, name string, redirects ...string) string {
	b, err := json.Marshal(map[string]any{"client_id": clientIDURL, "client_name": name, "redirect_uris": redirects})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// docClientURL is the metadata-document client the rig's CIMD tests use.
const docClientURL = "https://client.example/mcp/client.json"

// serveDocument makes the fake fetcher serve a valid document for
// docClientURL naming name, with the rig's loopback and https redirect
// URIs.
func (r *asRig) serveDocument(name string) {
	r.documents.set(docClientURL, fakeDocument{body: metadataDocument(docClientURL, name, loopbackRedirect, httpsRedirect)})
}

// forClient returns params with client_id replaced.
func forClient(params url.Values, clientID string) url.Values {
	out := url.Values{}
	for k, v := range params {
		out[k] = append([]string(nil), v...)
	}
	out.Set("client_id", clientID)
	return out
}

// staleDocument marks clientIDURL's cached document stale now.
func (r *asRig) staleDocument(t *testing.T, clientIDURL string) {
	t.Helper()
	tag, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_clients SET metadata_stale_at = now() - interval '1 second', metadata_fetched_at = now() - interval '2 hours' WHERE client_id = $1`, clientIDURL)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("mark %s stale: rows %d err %v", clientIDURL, tag.RowsAffected(), err)
	}
}
