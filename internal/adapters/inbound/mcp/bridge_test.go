package mcp

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestCallTwin_URLParamsAndQueryPropagate proves callTwin wires urlParams
// through chi's own URLParam lookup (the same mechanism a real chi router
// would use for a path like "/api/sessions/{sessionID}") and query
// through the request's own URL query string -- both exactly as a real
// REST client's request would carry them.
func TestCallTwin_URLParamsAndQueryPropagate(t *testing.T) {
	var gotSessionID, gotFilter, gotMethod string
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSessionID = chi.URLParam(r, "sessionID")
		gotFilter = r.URL.Query().Get("filter")
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	tw := twin{method: http.MethodGet, pathTemplate: "/api/sessions/{sessionID}", handler: stub}
	query := url.Values{"filter": []string{"all"}}
	status, body := callTwin(context.Background(), tw, map[string]string{"sessionID": "abc-123"}, query)

	if gotSessionID != "abc-123" {
		t.Errorf("chi.URLParam(sessionID) = %q, want %q", gotSessionID, "abc-123")
	}
	if gotFilter != "all" {
		t.Errorf("query filter = %q, want %q", gotFilter, "all")
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want %q", body, `{"ok":true}`)
	}
}

// TestCallTwin_ContextCarriesAuthenticatedUser proves the context callTwin
// hands the twin's own handler is the SAME context passed to callTwin --
// specifically, that a value stashed on it (standing in for platform.
// WithUser's own real payload) survives the round trip through
// httptest.NewRequest/chi.RouteContext.
func TestCallTwin_ContextCarriesAuthenticatedUser(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "the-authenticated-user")

	var gotValue any
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotValue = r.Context().Value(ctxKey{})
		w.WriteHeader(http.StatusOK)
	})

	tw := twin{method: http.MethodGet, pathTemplate: "/api/models", handler: stub}
	callTwin(ctx, tw, nil, nil)

	if gotValue != "the-authenticated-user" {
		t.Errorf("handler's own context carried %v, want %q", gotValue, "the-authenticated-user")
	}
}

// TestCallTwin_NoQueryOmitsQuestionMark proves an empty/nil query never
// appends a bare "?" to the constructed request's own URL (cosmetic, but
// pins the "no query" branch explicitly).
func TestCallTwin_NoQueryOmitsQuestionMark(t *testing.T) {
	var gotURL string
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
	})
	tw := twin{method: http.MethodGet, pathTemplate: "/api/models", handler: stub}
	callTwin(context.Background(), tw, nil, nil)

	if gotURL != "/api/models" {
		t.Errorf("constructed URL = %q, want %q", gotURL, "/api/models")
	}
}
