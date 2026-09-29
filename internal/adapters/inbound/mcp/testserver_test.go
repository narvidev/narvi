package mcp

import (
	"bytes"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/narvidev/narvi/internal/platform"
)

// swapDefaultLogger points slog.SetDefault at a fresh handler writing into
// the returned *bytes.Buffer for the duration of the calling test (t.
// Cleanup restores the prior default) -- platform.Logger(ctx) always
// resolves to slog.Default() (internal/platform/logging.go), so this is
// how a test observes (or asserts the ABSENCE of) a log line this
// package's own handlers write, without a real logging backend.
func swapDefaultLogger(t testing.TB) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// testUser stands in for a real, authenticated platform.AuthenticatedUser
// -- an arbitrary but syntactically valid UUID, "member" role (an
// ordinary, non-privileged role -- see TestParity_ToolsListIsRoleIndependent
// in integration_test.go for the real, Postgres-backed proof that role
// never gates which tools tools/list returns in 180).
var testUser = platform.AuthenticatedUser{
	ID:    "11111111-1111-1111-1111-111111111111",
	Role:  "member",
	Email: "test@example.com",
}

// testGrantScopes is the grant every unit test's fakeAuth attaches unless
// a test chooses otherwise: every scope this build advertises, so the
// full tool table is visible -- what a user who approved everything gets.
var testGrantScopes = []string{"mcp:read", "mcp:write"}

// testGrantID is the grant fakeAuthWithGrant attaches: the key the create
// brake is consulted with.
const testGrantID = "33333333-3333-3333-3333-333333333333"

// allowAllBrake is a CreateBrake that never refuses, for every unit test
// whose subject is not the brake itself.
type allowAllBrake struct{}

func (allowAllBrake) Allow(string) (bool, time.Duration) { return true, 0 }

// recordingBrake is a CreateBrake that refuses once it has admitted allow
// starts, answering retryAfter, and records every key it was asked about.
type recordingBrake struct {
	allow      int
	retryAfter time.Duration

	mu   sync.Mutex
	keys []string
}

func (b *recordingBrake) Allow(key string) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.keys = append(b.keys, key)
	if len(b.keys) <= b.allow {
		return true, 0
	}
	return false, b.retryAfter
}

func (b *recordingBrake) asked() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.keys...)
}

// testConfig is the Config every unit test builds NewHandler with: baseURL
// as the one trusted origin, and a brake that never refuses.
func testConfig(baseURL string) Config {
	return Config{PublicBaseURL: baseURL, CreateBrake: allowAllBrake{}}
}

// fakeAuth is a MINIMAL stand-in for auth.RequireMCPBearer's own
// OBSERVABLE contract, used only by this package's own unit tests (never a
// reimplementation of its real, DB-backed logic -- that stays tested in
// its own package, and the real gate is what integration_test.go and
// oauth_integration_test.go mount): authenticated==true injects
// platform.WithUser AND platform.WithMCPGrant (testGrantScopes) before
// calling next, exactly like the real gate after a valid token resolves;
// authenticated==false writes the SAME generic 401 body the real gate
// writes on every rejection path. Unlike the real gate it does NOT strip
// the Authorization header -- deliberately, so the bridge's own
// empty-header request is proven on its own
// (TestBridge_NoAuthorizationHeaderReachesTwin).
func fakeAuth(authenticated bool, user platform.AuthenticatedUser) func(http.Handler) http.Handler {
	return fakeAuthWithGrant(authenticated, user, &testGrantScopes)
}

// fakeAuthWithGrant is fakeAuth with the grant's scopes chosen by the
// test; a nil scopes pointer attaches NO grant at all (the defect case
// buildServer must answer with an empty server).
func fakeAuthWithGrant(authenticated bool, user platform.AuthenticatedUser, scopes *[]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !authenticated {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			ctx := platform.WithUser(r.Context(), user)
			if scopes != nil {
				ctx = platform.WithMCPGrant(ctx, platform.MCPGrant{GrantID: testGrantID, ClientID: "narvi_mcp_c_unit", Scopes: *scopes})
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// testPublicBaseURL is the PublicBaseURL every unit test in this package
// builds NewHandler/RequireTrustedOrigin against -- "http://example.test"
// is therefore the ONE trusted origin for every test in this file and
// its siblings.
const testPublicBaseURL = "http://example.test"

// newTestHandler wires the SAME /mcp route SHAPE controlplane/serve.go
// registers (RequireTrustedOrigin, then RequireEnabled, then the bearer
// gate, then the real NewHandler -- §43.2/§43.6's own gate order) as a
// plain http.Handler -- so this package's own unit tests exercise the
// real gate ORDER and the real NewHandler, driven directly via ServeHTTP
// (httptest.NewRequest/NewRecorder, never a real listening
// httptest.Server + net/http client: tools/lint/narvichecks'
// httpclientban analyzer reserves net/http's CLIENT-side symbols for the
// outbound trees, exactly as httpapi's own *_test.go unit tests --
// distinct from its *_integration_test.go rig, which DOES dial a real
// httptest.Server -- already avoid them), substituting only auth.
// RequireMCPBearer's own Postgres-backed check for fakeAuth's equivalent,
// dependency-free one. Every unit test in this file and its siblings
// trusts testPublicBaseURL; a test that needs a DIFFERENT trusted origin
// (e.g. an https or IPv6 PublicBaseURL -- round 4 review of PR #324,
// finding S8) calls newTestHandlerWithBaseURL directly instead, and a
// test that needs to choose the tool input schema map calls
// newTestHandlerWithInputSchemas.
func newTestHandler(t testing.TB, enabled, authenticated bool, twins Twins) http.Handler {
	t.Helper()
	return newTestHandlerWithBaseURL(t, enabled, authenticated, twins, testPublicBaseURL)
}

// newTestHandlerWithBaseURL is newTestHandler, parameterized on
// PublicBaseURL -- round 4 review of PR #324, finding S8: every existing
// unit test trusts "http://example.test", so platform.CanonicalOrigin's own
// IPv6 re-bracketing and its https default-port branch (origin.go) were
// never exercised through a real RequireTrustedOrigin/NewHandler pair
// built against a base URL that actually needs either one.
func newTestHandlerWithBaseURL(t testing.TB, enabled, authenticated bool, twins Twins, baseURL string) http.Handler {
	t.Helper()
	cfg := testConfig(baseURL)
	mcpHandler, err := NewHandler(cfg, twins)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return mountTestRoute(t, cfg, enabled, authenticated, mcpHandler)
}

// newTestHandlerWithInputSchemas is newTestHandler(t, true, true, twins)
// built with newHandler and inputSchemas in place of NewHandler and the
// map NewHandler compiles itself (round 6 review of PR #324, findings
// U1/U2/U3): the same route shape, gate order, middleware chain,
// per-request closure, buildServer and registerTools, with only the
// schema map chosen by the test. newHandler refusing inputSchemas fails
// the calling test; a test that expects the refusal calls newHandler
// directly.
func newTestHandlerWithInputSchemas(t testing.TB, twins Twins, inputSchemas map[string]*jsonschema.Schema) http.Handler {
	t.Helper()
	cfg := testConfig(testPublicBaseURL)
	mcpHandler, err := newHandler(cfg, twins, inputSchemas)
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	return mountTestRoute(t, cfg, true, true, mcpHandler)
}

// mountTestRoute mounts mcpHandler at POST /mcp behind the /mcp route
// group's own gates, in controlplane/serve.go's order (newTestHandler's
// doc comment).
func mountTestRoute(t testing.TB, cfg Config, enabled, authenticated bool, mcpHandler http.Handler) http.Handler {
	t.Helper()
	return mountTestRouteWithAuth(t, cfg, enabled, fakeAuth(authenticated, testUser), mcpHandler)
}

// mountTestRouteWithAuth is mountTestRoute with the auth stand-in chosen
// by the test (e.g. fakeAuthWithGrant, for a specific scope set).
func mountTestRouteWithAuth(t testing.TB, cfg Config, enabled bool, authGate func(http.Handler) http.Handler, mcpHandler http.Handler) http.Handler {
	t.Helper()
	originGate, err := RequireTrustedOrigin(cfg)
	if err != nil {
		t.Fatalf("RequireTrustedOrigin: %v", err)
	}
	router := chi.NewRouter()
	router.Route("/mcp", func(r chi.Router) {
		r.Use(originGate)
		r.Use(RequireEnabled(enabled))
		r.Use(authGate)
		r.Post("/", mcpHandler.ServeHTTP)
	})
	return router
}

// newTestHandlerWithGrant is newTestHandler(t, true, true, twins) with
// the grant's scopes chosen by the test (nil: no grant at all).
func newTestHandlerWithGrant(t testing.TB, twins Twins, scopes *[]string) http.Handler {
	t.Helper()
	return newTestHandlerWithGrantAndBrake(t, twins, scopes, allowAllBrake{})
}

// newTestHandlerWithGrantAndBrake is newTestHandlerWithGrant with the
// create brake chosen by the test.
func newTestHandlerWithGrantAndBrake(t testing.TB, twins Twins, scopes *[]string, brake CreateBrake) http.Handler {
	t.Helper()
	cfg := Config{PublicBaseURL: testPublicBaseURL, CreateBrake: brake}
	mcpHandler, err := NewHandler(cfg, twins)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return mountTestRouteWithAuth(t, cfg, true, fakeAuthWithGrant(true, testUser, scopes), mcpHandler)
}

// stubHandler returns an http.HandlerFunc writing status and body
// verbatim -- a fake REST twin standing in for a real httpapi handler in
// every unit test that doesn't need Postgres (the real handlers are
// exercised by integration_test.go's own parity suite instead).
func stubHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}
