//go:build integration

// This file is §41.1 review round 2, findings Q2/Q7/Q10/Q11's own test
// obligation: prove, against the REAL composed router (Build), that the
// user-OAuth callback's API host is ALWAYS https://api.github.com, even
// when NARVI_GITHUB_API_BASE_URL is set to something else. Round 1 wired
// cfg.GitHubAPIBaseURL straight into auth.NewCallbackHandler's own
// apiBaseURL parameter, which meant a configured non-default value
// received the github.com-issued user OAuth token (scopes including
// "repo") this handler had just exchanged, while login itself never left
// github.com -- see controlplane/serve.go's own githubUserTokenAPIBaseURL
// doc comment for the full story. Round 2 fixed the wiring; this test
// pins it so it cannot regress silently.
package controlplane

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"

	"github.com/narvidev/narvi/internal/platform"
)

// apiHostRecordingTransport is an http.RoundTripper standing in for the
// real network (§13's own "apiBaseURL is an overridable parameter"
// design decision, extended one layer further here: rather than
// overriding apiBaseURL directly, this stubs the TRANSPORT the whole
// OAuth flow uses -- both the token exchange golang.org/x/oauth2 makes
// internally and every subsequent call oauthConfig.Client's own derived
// http.Client makes -- via oauth2.HTTPClient's own documented context-key
// seam, so this test observes exactly what host controlplane's REAL
// wiring dials, with no code under test aware it is being watched.
type apiHostRecordingTransport struct {
	t             *testing.T
	forbiddenHost string

	mu       sync.Mutex
	sawUser  bool
	sawEmail bool
}

func (rt *apiHostRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	host := req.URL.Hostname()
	switch {
	case host == "github.com" && req.URL.Path == "/login/oauth/access_token":
		// The OAuth code-exchange step -- unaffected by
		// NARVI_GITHUB_API_BASE_URL, and never should be (login stays on
		// github.com always). Confirms the baseline is unchanged.
		return jsonResponse(req, `{"access_token":"test-oauth-access-token","token_type":"bearer"}`), nil

	case host == "api.github.com" && req.URL.Path == "/user":
		rt.sawUser = true
		return jsonResponse(req, `{"id": 42, "login": "octocat", "name": "The Octocat", "email": null}`), nil

	case host == "api.github.com" && req.URL.Path == "/user/emails":
		rt.sawEmail = true
		return jsonResponse(req, `[{"email":"octocat@example.com","primary":true,"verified":true}]`), nil

	case host == rt.forbiddenHost:
		rt.t.Errorf("auth callback dialed %s%s -- the user-OAuth callback must always use https://api.github.com, regardless of what NARVI_GITHUB_API_BASE_URL names (§41.1 review round 2, findings Q2/Q7/Q10/Q11)", host, req.URL.Path)
		return nil, fmt.Errorf("test: refusing to actually dial forbidden host %q", host)

	default:
		rt.t.Errorf("auth callback dialed unexpected host %q path %q", host, req.URL.Path)
		return nil, fmt.Errorf("test: refusing to actually dial unexpected host %q", host)
	}
}

func jsonResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// TestAuthCallback_AlwaysUsesGitHubComForUserToken is this file's own top
// doc comment's proof obligation.
func TestAuthCallback_AlwaysUsesGitHubComForUserToken(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("NARVI_STAGE", "development") // required for NARVI_GITHUB_API_BASE_URL to be accepted at all
	// A syntactically well-formed, but deliberately WRONG, host --
	// canonicalGitHubAPIBaseURL accepts it (it is only ever meant to feed
	// the GitHub App client), and this test's own transport fails loudly
	// if the auth callback ever dials it.
	t.Setenv("NARVI_GITHUB_API_BASE_URL", "https://ghes.example.invalid")

	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}
	if cfg.GitHubAPIBaseURL != "https://ghes.example.invalid" {
		t.Fatalf("cfg.GitHubAPIBaseURL = %q, want the configured sentinel -- this test is meaningless if the config itself did not pick it up", cfg.GitHubAPIBaseURL)
	}

	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/github/callback?state=test-state&code=test-code", nil)
	req.AddCookie(&http.Cookie{Name: "narvi_oauth_state", Value: "test-state"})

	transport := &apiHostRecordingTransport{t: t, forbiddenHost: "ghes.example.invalid"}
	ctx := context.WithValue(req.Context(), oauth2.HTTPClient, &http.Client{Transport: transport})
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	app.Router.ServeHTTP(rec, req)

	// A first-time sign-in that clears the allowlist redirects (302) to
	// "/" -- see callback.go's own OutcomeFirstTimeAllowed branch. Any
	// other code means the flow didn't reach the point this test needs to
	// observe, and the assertions below would be checking nothing.
	if rec.Code != http.StatusFound {
		t.Fatalf("callback response = %d %s, want %d (redirect on first-time sign-in) -- body: %s", rec.Code, http.StatusText(rec.Code), http.StatusFound, rec.Body.String())
	}

	transport.mu.Lock()
	defer transport.mu.Unlock()
	if !transport.sawUser {
		t.Error("callback never called /user at all -- this test proves nothing about which host it would have used")
	}
	if !transport.sawEmail {
		t.Error("callback never called /user/emails at all -- this test proves nothing about which host it would have used")
	}
}
