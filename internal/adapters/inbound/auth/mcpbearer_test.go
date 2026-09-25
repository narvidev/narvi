package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
)

const (
	testBearerToken     = "narvi_mcp_at_test-token_0123456789"
	testResource        = "https://narvi.example/mcp"
	testResourceMetaURL = "https://narvi.example/.well-known/oauth-protected-resource/mcp"
)

// fakeTokenStore is an in-memory MCPAccessTokenLookup whose rows a test
// can change between calls -- the whole point: RequireMCPBearer must
// observe every change on the very next call.
type fakeTokenStore struct {
	mu        sync.Mutex
	rows      map[string]postgres.MCPAccessTokenPrincipal
	lookupErr error
	lookups   int
	touches   []time.Time
}

func (f *fakeTokenStore) LookupAccessToken(_ context.Context, tokenHash string) (postgres.MCPAccessTokenPrincipal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.lookupErr != nil {
		return postgres.MCPAccessTokenPrincipal{}, f.lookupErr
	}
	p, ok := f.rows[tokenHash]
	if !ok {
		return postgres.MCPAccessTokenPrincipal{}, pgx.ErrNoRows
	}
	return p, nil
}

func (f *fakeTokenStore) TouchGrantLastUsed(_ context.Context, _ pgtype.UUID, staleBefore time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches = append(f.touches, staleBefore)
	return nil
}

func (f *fakeTokenStore) set(token string, p postgres.MCPAccessTokenPrincipal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[platform.HashToken(token)] = p
}

func (f *fakeTokenStore) remove(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, platform.HashToken(token))
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		t.Fatal(err)
	}
	return id
}

func livePrincipal(t *testing.T) postgres.MCPAccessTokenPrincipal {
	return postgres.MCPAccessTokenPrincipal{
		TokenExpiresAt: time.Now().Add(time.Hour),
		GrantID:        mustUUID(t, "22222222-2222-2222-2222-222222222222"),
		GrantScopes:    []string{"mcp:read"},
		GrantResource:  testResource,
		GrantExpiresAt: time.Now().Add(24 * time.Hour),
		ClientID:       "narvi_mcp_c_test",
		UserID:         mustUUID(t, "11111111-1111-1111-1111-111111111111"),
		UserRole:       "member",
		UserEmail:      "member@example.com",
	}
}

func testBearerConfig() MCPBearerConfig {
	return MCPBearerConfig{
		Resource:              testResource,
		ResourceMetadataURL:   testResourceMetaURL,
		Scopes:                []string{"mcp:read"},
		LastUsedWriteInterval: 5 * time.Minute,
	}
}

// seen records what reached the protected handler.
type seen struct {
	user    platform.AuthenticatedUser
	grant   platform.MCPGrant
	authHdr string
	calls   int
}

func protectedHandler(s *seen) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls++
		s.user, _ = platform.UserFromContext(r.Context())
		s.grant, _ = platform.MCPGrantFromContext(r.Context())
		s.authHdr = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
}

func doBearer(h http.Handler, headers map[string][]string, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, nil)
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRequireMCPBearer_Table covers every refusal and the one success,
// asserting the exact 401 body, whether the challenge carries
// error="invalid_token", and that the challenge parses with the official
// SDK's own RFC 9110 parser into the resource_metadata and scope a client
// needs.
func TestRequireMCPBearer_Table(t *testing.T) {
	t.Parallel()

	bearer := map[string][]string{"Authorization": {"Bearer " + testBearerToken}}
	tests := []struct {
		name         string
		mutate       func(*postgres.MCPAccessTokenPrincipal)
		headers      map[string][]string
		target       string
		wantStatus   int
		wantInvalid  bool
		wantPassThru bool
	}{
		{name: "valid token", headers: bearer, wantStatus: http.StatusOK, wantPassThru: true},
		{name: "lower-case scheme", headers: map[string][]string{"Authorization": {"bearer " + testBearerToken}}, wantStatus: http.StatusOK, wantPassThru: true},
		{name: "no credential", headers: nil, wantStatus: http.StatusUnauthorized},
		{name: "token only in the query string", headers: nil, target: "/mcp?access_token=" + testBearerToken, wantStatus: http.StatusUnauthorized},
		{name: "cookie is not a credential here", headers: map[string][]string{"Cookie": {platform.AuthSessionCookieName + "=whatever"}}, wantStatus: http.StatusUnauthorized},
		{name: "basic scheme", headers: map[string][]string{"Authorization": {"Basic dXNlcjpwYXNz"}}, wantStatus: http.StatusUnauthorized, wantInvalid: true},
		{name: "two authorization headers", headers: map[string][]string{"Authorization": {"Bearer " + testBearerToken, "Bearer " + testBearerToken}}, wantStatus: http.StatusUnauthorized, wantInvalid: true},
		{name: "malformed token", headers: map[string][]string{"Authorization": {"Bearer not a token"}}, wantStatus: http.StatusUnauthorized, wantInvalid: true},
		{name: "empty token", headers: map[string][]string{"Authorization": {"Bearer "}}, wantStatus: http.StatusUnauthorized, wantInvalid: true},
		{name: "unknown token", headers: map[string][]string{"Authorization": {"Bearer narvi_mcp_at_unknown"}}, wantStatus: http.StatusUnauthorized, wantInvalid: true},
		{name: "token expired", mutate: func(p *postgres.MCPAccessTokenPrincipal) { p.TokenExpiresAt = time.Now().Add(-time.Second) }, headers: bearer, wantStatus: http.StatusUnauthorized, wantInvalid: true},
		{name: "grant expired", mutate: func(p *postgres.MCPAccessTokenPrincipal) { p.GrantExpiresAt = time.Now().Add(-time.Second) }, headers: bearer, wantStatus: http.StatusUnauthorized, wantInvalid: true},
		{name: "client disabled", mutate: func(p *postgres.MCPAccessTokenPrincipal) { p.ClientDisabled = true }, headers: bearer, wantStatus: http.StatusUnauthorized, wantInvalid: true},
		{name: "user disabled", mutate: func(p *postgres.MCPAccessTokenPrincipal) { p.UserDisabled = true }, headers: bearer, wantStatus: http.StatusUnauthorized, wantInvalid: true},
		{name: "grant for another resource", mutate: func(p *postgres.MCPAccessTokenPrincipal) { p.GrantResource = "https://other.example/mcp" }, headers: bearer, wantStatus: http.StatusUnauthorized, wantInvalid: true},
		{name: "grant resource differs only by a trailing slash", mutate: func(p *postgres.MCPAccessTokenPrincipal) { p.GrantResource = testResource + "/" }, headers: bearer, wantStatus: http.StatusUnauthorized, wantInvalid: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeTokenStore{rows: map[string]postgres.MCPAccessTokenPrincipal{}}
			p := livePrincipal(t)
			if tc.mutate != nil {
				tc.mutate(&p)
			}
			store.set(testBearerToken, p)
			var got seen
			h := RequireMCPBearer(store, testBearerConfig())(protectedHandler(&got))
			target := tc.target
			if target == "" {
				target = "/mcp"
			}
			rec := doBearer(h, tc.headers, target)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantPassThru != (got.calls == 1) {
				t.Fatalf("protected handler calls = %d, want passthrough=%v", got.calls, tc.wantPassThru)
			}
			if tc.wantStatus == http.StatusOK {
				return
			}
			if body := rec.Body.String(); body != `{"error":"unauthorized"}` {
				t.Errorf("401 body = %q, want the generic unauthorized body", body)
			}
			challenges, err := oauthex.ParseWWWAuthenticate(rec.Header().Values("WWW-Authenticate"))
			if err != nil || len(challenges) != 1 || challenges[0].Scheme != "bearer" {
				t.Fatalf("WWW-Authenticate = %q, parsed %+v, err %v; want one Bearer challenge", rec.Header().Values("WWW-Authenticate"), challenges, err)
			}
			params := challenges[0].Params
			if params["resource_metadata"] != testResourceMetaURL || params["scope"] != "mcp:read" {
				t.Errorf("challenge params = %v, want resource_metadata %q and scope mcp:read", params, testResourceMetaURL)
			}
			if gotInvalid := params["error"] == "invalid_token"; gotInvalid != tc.wantInvalid {
				t.Errorf("challenge error param = %q, want invalid_token present=%v", params["error"], tc.wantInvalid)
			}
		})
	}
}

// TestRequireMCPBearer_AttachesPrincipalAndStripsToken proves success
// attaches the user and the grant exactly as stored, and that the request
// passed on no longer carries the Authorization header: nothing
// downstream ever holds the token.
func TestRequireMCPBearer_AttachesPrincipalAndStripsToken(t *testing.T) {
	t.Parallel()

	store := &fakeTokenStore{rows: map[string]postgres.MCPAccessTokenPrincipal{}}
	store.set(testBearerToken, livePrincipal(t))
	var got seen
	h := RequireMCPBearer(store, testBearerConfig())(protectedHandler(&got))
	rec := doBearer(h, map[string][]string{"Authorization": {"Bearer " + testBearerToken}}, "/mcp")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	wantUser := platform.AuthenticatedUser{ID: "11111111-1111-1111-1111-111111111111", Role: "member", Email: "member@example.com"}
	if got.user != wantUser {
		t.Errorf("user = %+v, want %+v", got.user, wantUser)
	}
	if got.grant.GrantID != "22222222-2222-2222-2222-222222222222" || got.grant.ClientID != "narvi_mcp_c_test" || len(got.grant.Scopes) != 1 || got.grant.Scopes[0] != "mcp:read" {
		t.Errorf("grant = %+v", got.grant)
	}
	if got.authHdr != "" {
		t.Errorf("downstream Authorization header = %q, want it stripped", got.authHdr)
	}
}

// TestBearer_NoCacheBetweenCalls is the revocation exit criterion's own
// unit proof (technical plan §43.16): the SAME handler instance, two calls
// with the SAME token -- the first succeeds, the row is deleted between
// them, the second is 401. Both calls reached the store; so does a role
// change, observed on the very next call.
func TestBearer_NoCacheBetweenCalls(t *testing.T) {
	t.Parallel()

	store := &fakeTokenStore{rows: map[string]postgres.MCPAccessTokenPrincipal{}}
	p := livePrincipal(t)
	store.set(testBearerToken, p)
	var got seen
	h := RequireMCPBearer(store, testBearerConfig())(protectedHandler(&got))
	hdr := map[string][]string{"Authorization": {"Bearer " + testBearerToken}}

	if rec := doBearer(h, hdr, "/mcp"); rec.Code != http.StatusOK || got.user.Role != "member" {
		t.Fatalf("first call: status %d role %q, want 200 as member", rec.Code, got.user.Role)
	}
	p.UserRole = "viewer"
	store.set(testBearerToken, p)
	if rec := doBearer(h, hdr, "/mcp"); rec.Code != http.StatusOK || got.user.Role != "viewer" {
		t.Fatalf("after a role change: status %d role %q, want 200 as viewer on the very next call", rec.Code, got.user.Role)
	}
	store.remove(testBearerToken)
	if rec := doBearer(h, hdr, "/mcp"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after revocation: status %d, want 401 on the very next call", rec.Code)
	}
	if store.lookups != 3 {
		t.Fatalf("store lookups = %d, want 3 (one per call, nothing cached)", store.lookups)
	}
	if got.calls != 2 {
		t.Fatalf("protected handler calls = %d, want 2", got.calls)
	}
}

// TestRequireMCPBearer_LookupFailureIs500 pins that a database fault is
// never presented as "your token is bad" (which would send the client
// back through consent).
func TestRequireMCPBearer_LookupFailureIs500(t *testing.T) {
	t.Parallel()

	store := &fakeTokenStore{rows: map[string]postgres.MCPAccessTokenPrincipal{}, lookupErr: errors.New("connection refused")}
	var got seen
	h := RequireMCPBearer(store, testBearerConfig())(protectedHandler(&got))
	rec := doBearer(h, map[string][]string{"Authorization": {"Bearer " + testBearerToken}}, "/mcp")
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("WWW-Authenticate") != "" || got.calls != 0 {
		t.Fatalf("status %d, challenge %q, calls %d; want 500, no challenge, handler not reached", rec.Code, rec.Header().Get("WWW-Authenticate"), got.calls)
	}
}

// TestRequireMCPBearer_LastUsedCoalesced pins the write coalescing: a
// never-used or stale grant is stamped, a recently stamped one is not.
func TestRequireMCPBearer_LastUsedCoalesced(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		lastUsed  time.Time
		wantTouch bool
	}{
		{"never used", time.Time{}, true},
		{"stale", time.Now().Add(-time.Hour), true},
		{"recent", time.Now().Add(-time.Minute), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeTokenStore{rows: map[string]postgres.MCPAccessTokenPrincipal{}}
			p := livePrincipal(t)
			p.GrantLastUsedAt = tc.lastUsed
			store.set(testBearerToken, p)
			var got seen
			h := RequireMCPBearer(store, testBearerConfig())(protectedHandler(&got))
			if rec := doBearer(h, map[string][]string{"Authorization": {"Bearer " + testBearerToken}}, "/mcp"); rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			if touched := len(store.touches) == 1; touched != tc.wantTouch {
				t.Fatalf("touches = %d, want touched=%v", len(store.touches), tc.wantTouch)
			}
		})
	}
}
