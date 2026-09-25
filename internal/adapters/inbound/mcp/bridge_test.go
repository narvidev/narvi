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

// TestCallTwin_MaliciousURLParamsNeverPanic reproduces findings M1/M2/M3
// of the adversarial review of PR #324 (a prior version of callTwin
// called httptest.NewRequest(method, target, nil), where target held the
// raw urlParams value spliced in unescaped: that function PANICS,
// rather than returning an error, on a target it cannot parse as an
// HTTP request line -- and the SDK runs every tool handler in its own
// goroutine with no recover anywhere in ITS call stack, so the panic
// killed the whole process, not merely this request). Every value below
// crashed the test binary before this fix (verified by temporarily
// reverting callTwin to the old construction: every subtest below
// crashes instead of failing cleanly). Each must now: reach the stub
// handler (proving no panic happened along the way), carry the value
// UNCHANGED in chi's own route context (the actual source of truth every
// real handler reads -- see replaceURLParam's own doc comment), and
// inject NO header/cookie into the synthesized request.
func TestCallTwin_MaliciousURLParamsNeverPanic(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"space", "a b"},
		{"invalid percent-escape", "%zz"},
		{"tab", "x\ty"},
		{"bare CRLF", "x\r\nX-Injected: yes"},
		{"header/cookie injection attempt", "x HTTP/1.1\r\nX-Injected: yes\r\nCookie: narvi_auth_session=forged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotSessionID string
			var gotHeader http.Header
			stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotSessionID = chi.URLParam(r, "sessionID")
				gotHeader = r.Header.Clone()
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			})
			tw := twin{method: http.MethodGet, pathTemplate: "/api/sessions/{sessionID}", handler: stub}

			// The call itself must not panic -- if it did, this
			// subtest (and the whole test binary, since the SDK's own
			// goroutine has no recover either) would never reach the
			// assertions below at all.
			status, body := callTwin(context.Background(), tw, map[string]string{"sessionID": tt.value}, nil)

			if status != http.StatusOK {
				t.Fatalf("status = %d, body = %s, want 200 (the stub handler must have run)", status, body)
			}
			if gotSessionID != tt.value {
				t.Errorf("chi.URLParam(sessionID) = %q, want the RAW value %q unchanged", gotSessionID, tt.value)
			}
			if gotHeader.Get("X-Injected") != "" {
				t.Errorf("synthesized request carried an injected header: X-Injected = %q, want none", gotHeader.Get("X-Injected"))
			}
			if gotHeader.Get("Cookie") != "" {
				t.Errorf("synthesized request carried an injected cookie: Cookie = %q, want none", gotHeader.Get("Cookie"))
			}
		})
	}
}
