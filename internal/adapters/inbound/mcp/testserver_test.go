package mcp

import (
	"bytes"
	"log/slog"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

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

// fakeAuth is a MINIMAL stand-in for auth.Middleware's own OBSERVABLE
// contract, used only by this package's own unit tests (never a
// reimplementation of its real, DB-backed logic -- that stays tested in
// its own package and reused UNCHANGED by the real parity tests in
// integration_test.go): authenticated==true injects platform.WithUser
// before calling next, exactly like the real middleware after a valid
// cookie resolves; authenticated==false writes the SAME generic 401 body
// auth.Middleware's own writeUnauthorized writes on every rejection path.
func fakeAuth(authenticated bool, user platform.AuthenticatedUser) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !authenticated {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			next.ServeHTTP(w, r.WithContext(platform.WithUser(r.Context(), user)))
		})
	}
}

// testPublicBaseURL is the PublicBaseURL every unit test in this package
// builds NewHandler/RequireTrustedOrigin against -- "http://example.test"
// is therefore the ONE trusted origin for every test in this file and
// its siblings.
const testPublicBaseURL = "http://example.test"

// newTestHandler wires the SAME /mcp route SHAPE controlplane/serve.go
// registers (RequireTrustedOrigin, then RequireEnabled, then the auth
// gate, then the real NewHandler -- §43.2/§43.6's own gate order) as a
// plain http.Handler -- so this package's own unit tests exercise the
// real gate ORDER and the real NewHandler, driven directly via ServeHTTP
// (httptest.NewRequest/NewRecorder, never a real listening
// httptest.Server + net/http client: tools/lint/narvichecks'
// httpclientban analyzer reserves net/http's CLIENT-side symbols for the
// outbound trees, exactly as httpapi's own *_test.go unit tests --
// distinct from its *_integration_test.go rig, which DOES dial a real
// httptest.Server -- already avoid them), substituting only auth.
// Middleware's own Postgres-backed check for fakeAuth's equivalent,
// dependency-free one. Every unit test in this file and its siblings
// trusts testPublicBaseURL; a test that needs a DIFFERENT trusted origin
// (e.g. an https or IPv6 PublicBaseURL -- round 4 review of PR #324,
// finding S8) calls newTestHandlerWithBaseURL directly instead.
func newTestHandler(t testing.TB, enabled, authenticated bool, twins Twins) http.Handler {
	t.Helper()
	return newTestHandlerWithBaseURL(t, enabled, authenticated, twins, testPublicBaseURL)
}

// newTestHandlerWithBaseURL is newTestHandler, parameterized on
// PublicBaseURL -- round 4 review of PR #324, finding S8: every existing
// unit test trusts "http://example.test", so canonicalOrigin's own IPv6
// re-bracketing and its https default-port branch (handler.go) were
// never exercised through a real RequireTrustedOrigin/NewHandler pair
// built against a base URL that actually needs either one.
func newTestHandlerWithBaseURL(t testing.TB, enabled, authenticated bool, twins Twins, baseURL string) http.Handler {
	t.Helper()
	cfg := Config{PublicBaseURL: baseURL}
	originGate, err := RequireTrustedOrigin(cfg)
	if err != nil {
		t.Fatalf("RequireTrustedOrigin: %v", err)
	}
	mcpHandler, err := NewHandler(cfg, twins)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	router := chi.NewRouter()
	router.Route("/mcp", func(r chi.Router) {
		r.Use(originGate)
		r.Use(RequireEnabled(enabled))
		r.Use(fakeAuth(authenticated, testUser))
		r.Post("/", mcpHandler.ServeHTTP)
	})
	return router
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
