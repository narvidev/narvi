package mcp

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/narvidev/narvi/internal/platform"
)

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

// newTestHandler wires the SAME /mcp route SHAPE controlplane/serve.go
// registers (RequireEnabled, then the auth gate, then the real
// NewHandler) as a plain http.Handler -- so this package's own unit
// tests exercise the real gate ORDER and the real NewHandler, driven
// directly via ServeHTTP (httptest.NewRequest/NewRecorder, never a real
// listening httptest.Server + net/http client: tools/lint/narvichecks'
// httpclientban analyzer reserves net/http's CLIENT-side symbols for the
// outbound trees, exactly as httpapi's own *_test.go unit tests --
// distinct from its *_integration_test.go rig, which DOES dial a real
// httptest.Server -- already avoid them), substituting only auth.
// Middleware's own Postgres-backed check for fakeAuth's equivalent,
// dependency-free one.
func newTestHandler(t testing.TB, enabled, authenticated bool, twins Twins) http.Handler {
	t.Helper()
	mcpHandler, err := NewHandler(Config{PublicBaseURL: "http://example.test"}, twins)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	router := chi.NewRouter()
	router.Route("/mcp", func(r chi.Router) {
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
