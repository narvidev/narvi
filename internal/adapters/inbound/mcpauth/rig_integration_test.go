//go:build integration

package mcpauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/mcpauth"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
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
// the stand-in echoes back, "what may it see".
// controlplane's own TestOAuth_ProductionRouter proves the production
// wiring itself.
type asRig struct {
	pool         *pgxpool.Pool
	users        *postgres.UserStore
	userSessions *postgres.UserSessionStore
	clients      *postgres.MCPOAuthClientStore
	grants       *postgres.MCPOAuthGrantStore
	server       *mcpauth.Server
	router       http.Handler
	client       sqlcgen.McpOauthClient
}

func newASRig(t *testing.T) *asRig {
	t.Helper()
	pool := IntegrationTestPool(t)
	r := &asRig{
		pool:         pool,
		users:        postgres.NewUserStore(pool),
		userSessions: postgres.NewUserSessionStore(pool),
		clients:      postgres.NewMCPOAuthClientStore(pool),
		grants:       postgres.NewMCPOAuthGrantStore(pool),
	}
	var err error
	r.server, err = mcpauth.New(mcpauth.Config{
		PublicBaseURL: rigBase,
		Enabled:       true,
		Scopes:        []mcpscope.Scope{mcpscope.Read},
		Timeouts:      platform.DefaultTimeouts(),
	}, mcpauth.Deps{
		Pool:         pool,
		Clients:      r.clients,
		Grants:       r.grants,
		UserSessions: r.userSessions,
		Users:        r.users,
		AuditLog:     postgres.NewAuditLogStore(pool),
	})
	if err != nil {
		t.Fatalf("mcpauth.New: %v", err)
	}
	r.client = r.newClient(t, "narvi_mcp_c_rig", "Editor Plugin", loopbackRedirect, httpsRedirect, localhostRedirect)

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
	})
	router.Route("/mcp", func(rt chi.Router) {
		rt.Use(auth.RequireMCPBearer(r.grants, auth.MCPBearerConfig{
			Resource:              ids.Resource,
			ResourceMetadataURL:   ids.ProtectedResourceMetadataURL,
			Scopes:                []string{"mcp:read"},
			LastUsedWriteInterval: platform.DefaultTimeouts().MCPGrantLastUsedWriteInterval,
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
	r.router = router
	return r
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
