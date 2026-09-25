//go:build integration

package mcpauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestAuthorize_RedirectURI_Table is the redirect-substitution threat
// row's own endpoint-level proof: every accepted shape continues to
// consent, and every refused one renders a 400 page with NO Location
// header -- a bad redirect_uri is never redirected to, not even with an
// error.
func TestAuthorize_RedirectURI_Table(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	verifier := newVerifier(t)

	tests := []struct {
		name     string
		redirect string
		accepted bool
	}{
		{"exact https", httpsRedirect, true},
		{"loopback IP, registered without a port, any port", "http://127.0.0.1:61111/callback", true},
		{"loopback IP, no port", loopbackRedirect, true},
		{"localhost, exact port", localhostRedirect, true},
		{"localhost, wrong port", "http://localhost:9090/cb", false},
		{"https registered, http presented", "http://client.example/cb", false},
		{"fragment", httpsRedirect + "#frag", false},
		{"custom scheme", "com.example.app:/callback", false},
		{"prefix-match attempt", httpsRedirect + "/../evil", false},
		{"longer path", httpsRedirect + "x", false},
		{"host case", "https://CLIENT.example/cb", false},
		{"another host", "https://evil.test/cb", false},
		{"missing", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := r.authorizeParams(verifier)
			if tc.redirect == "" {
				params.Del("redirect_uri")
			} else {
				params.Set("redirect_uri", tc.redirect)
			}
			rec := r.authorize(params, cookie)
			if tc.accepted {
				if rec.Code != http.StatusFound || !consentRequestPattern.MatchString(rec.Header().Get("Location")) {
					t.Fatalf("status %d Location %q, want 302 to the consent page", rec.Code, rec.Header().Get("Location"))
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Fatalf("Location = %q, want none: a bad redirect_uri must never be redirected to", loc)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Fatalf("Content-Type = %q, want an HTML error page", ct)
			}
		})
	}

	t.Run("repeated redirect_uri", func(t *testing.T) {
		params := r.authorizeParams(verifier)
		params.Add("redirect_uri", httpsRedirect)
		rec := r.authorize(params, cookie)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Fatalf("status %d Location %q, want 400 and no redirect", rec.Code, rec.Header().Get("Location"))
		}
	})
}

// TestAuthorize_UnknownOrDisabledClientRendersPage: a bad client_id is the
// other half of "render, never redirect".
func TestAuthorize_UnknownOrDisabledClientRendersPage(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	verifier := newVerifier(t)
	disabled := r.newClient(t, "narvi_mcp_c_disabled", "Disabled", loopbackRedirect)
	if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_clients SET disabled_at = now() WHERE id = $1`, disabled.ID); err != nil {
		t.Fatal(err)
	}
	for _, clientID := range []string{"", "narvi_mcp_c_unknown", disabled.ClientID} {
		params := r.authorizeParams(verifier)
		params.Set("client_id", clientID)
		rec := r.authorize(params, cookie)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("client_id %q: status %d Location %q, want 400 page and no redirect", clientID, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// redirectErr asserts rec redirects to the client with OAuth error code,
// the client's state, and iss.
func redirectErr(t *testing.T, rec *httptest.ResponseRecorder, status int, wantCode, wantState string) {
	t.Helper()
	if status != http.StatusFound {
		t.Fatalf("status %d, want 302 back to the client", status)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), "http://127.0.0.1:51234/callback?") {
		t.Fatalf("Location = %q, want the validated redirect URI", rec.Header().Get("Location"))
	}
	q := loc.Query()
	if q.Get("error") != wantCode {
		t.Fatalf("error = %q, want %q (Location %s)", q.Get("error"), wantCode, loc)
	}
	if q.Get("state") != wantState {
		t.Fatalf("state = %q, want %q", q.Get("state"), wantState)
	}
	if q.Get("iss") != rigBase+"/oauth" {
		t.Fatalf("iss = %q, want %q", q.Get("iss"), rigBase+"/oauth")
	}
	if q.Get("code") != "" {
		t.Fatalf("an error redirect carries a code")
	}
}

// TestAuthorize_ErrorsRedirectWithStateAndIss covers every
// after-validation refusal, each as a redirect with error, state and iss.
func TestAuthorize_ErrorsRedirectWithStateAndIss(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	verifier := newVerifier(t)

	tests := []struct {
		name      string
		mutate    func(url.Values)
		wantCode  string
		wantState string
	}{
		{"response_type token", func(p url.Values) { p.Set("response_type", "token") }, "unsupported_response_type", "state-123"},
		{"response_type missing", func(p url.Values) { p.Del("response_type") }, "unsupported_response_type", "state-123"},
		{"repeated scope", func(p url.Values) { p.Add("scope", "mcp:read") }, "invalid_request", "state-123"},
		{"state too long is not echoed", func(p url.Values) { p.Set("state", strings.Repeat("s", 513)) }, "invalid_request", ""},
		{"repeated state is not echoed", func(p url.Values) { p.Add("state", "other") }, "invalid_request", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := r.authorizeParams(verifier)
			tc.mutate(params)
			rec := r.authorize(params, cookie)
			redirectErr(t, rec, rec.Code, tc.wantCode, tc.wantState)
		})
	}
}

// TestToken_PlainChallengeRefusedAtAuthorize: PKCE is mandatory and S256
// only -- a missing challenge, a missing method, and "plain" are all
// refused before any request is stored.
func TestToken_PlainChallengeRefusedAtAuthorize(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	verifier := newVerifier(t)

	tests := []struct {
		name   string
		mutate func(url.Values)
	}{
		{"method plain", func(p url.Values) { p.Set("code_challenge_method", "plain"); p.Set("code_challenge", verifier) }},
		{"method missing", func(p url.Values) { p.Del("code_challenge_method") }},
		{"method lower-case", func(p url.Values) { p.Set("code_challenge_method", "s256") }},
		{"challenge missing", func(p url.Values) { p.Del("code_challenge") }},
		{"challenge too short", func(p url.Values) { p.Set("code_challenge", "short") }},
		{"challenge with a forbidden character", func(p url.Values) { p.Set("code_challenge", strings.Repeat("a", 42)+"+") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := r.authorizeParams(verifier)
			tc.mutate(params)
			rec := r.authorize(params, cookie)
			redirectErr(t, rec, rec.Code, "invalid_request", "state-123")
		})
	}
	var n int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM mcp_oauth_authorization_requests`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stored authorization requests = %d (err %v), want 0: a refused request is never stored", n, err)
	}
}

// TestAuthorize_ResourceMismatchIsInvalidTarget: resource is required and
// must be this deployment's own canonical MCP resource, modulo the
// normalizations the MCP spec asks a server to tolerate.
func TestAuthorize_ResourceMismatchIsInvalidTarget(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	verifier := newVerifier(t)

	for _, bad := range []string{"", "http://other.test/mcp", rigBase, rigBase + "/mcp/extra", rigBase + "/mcp?x=1", "https://narvi.test/mcp"} {
		params := r.authorizeParams(verifier)
		if bad == "" {
			params.Del("resource")
		} else {
			params.Set("resource", bad)
		}
		rec := r.authorize(params, cookie)
		t.Run("refused "+bad, func(t *testing.T) { redirectErr(t, rec, rec.Code, "invalid_target", "state-123") })
	}
	for _, good := range []string{"HTTP://NARVI.TEST/mcp", rigBase + "/mcp/", "http://narvi.test:80/mcp"} {
		params := r.authorizeParams(verifier)
		params.Set("resource", good)
		rec := r.authorize(params, cookie)
		if rec.Code != http.StatusFound || !consentRequestPattern.MatchString(rec.Header().Get("Location")) {
			t.Errorf("resource %q: status %d Location %q, want accepted", good, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// TestAuthorize_UnadvertisedScopeRefused: mcp:write is declared but no
// tool requires it yet, so it is not offered -- invalid_scope, and a
// scope-less request is accepted.
func TestAuthorize_UnadvertisedScopeRefused(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	verifier := newVerifier(t)

	for _, bad := range []string{"mcp:write", "mcp:read mcp:write", "openid", "offline_access"} {
		params := r.authorizeParams(verifier)
		params.Set("scope", bad)
		rec := r.authorize(params, cookie)
		t.Run("refused "+bad, func(t *testing.T) { redirectErr(t, rec, rec.Code, "invalid_scope", "state-123") })
	}
	params := r.authorizeParams(verifier)
	params.Del("scope")
	if rec := r.authorize(params, cookie); rec.Code != http.StatusFound || !consentRequestPattern.MatchString(rec.Header().Get("Location")) {
		t.Fatalf("scope-less request: status %d Location %q, want accepted", rec.Code, rec.Header().Get("Location"))
	}
}

// TestAuthorize_SignedOutGoesThroughSignIn: without a session the browser
// is sent to the SPA sign-in with next = the consent path only (never the
// OAuth query), and that path is what the login handlers already accept.
func TestAuthorize_SignedOutGoesThroughSignIn(t *testing.T) {
	r := newASRig(t)
	verifier := newVerifier(t)
	rec := r.authorize(r.authorizeParams(verifier), "")
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Path != "/sign-in" {
		t.Fatalf("Location = %q, want /sign-in", rec.Header().Get("Location"))
	}
	next := loc.Query().Get("next")
	if !consentRequestPattern.MatchString(next) {
		t.Fatalf("next = %q, want exactly /oauth/consent?request=<uuid>", next)
	}
	// And the consent page itself, still signed out, sends the browser the
	// same way.
	rec = r.do(http.MethodGet, next, "", nil, "")
	loc2, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || loc2 == nil || loc2.Path != "/sign-in" || loc2.Query().Get("next") != next {
		t.Fatalf("signed-out consent: status %d Location %q, want the same sign-in redirect", rec.Code, rec.Header().Get("Location"))
	}
}
