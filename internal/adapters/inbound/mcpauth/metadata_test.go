package mcpauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/narvidev/narvi/internal/domain/mcpscope"
	"github.com/narvidev/narvi/internal/platform"
)

func newUnitServer(t *testing.T, base string, scopes []mcpscope.Scope) *Server {
	t.Helper()
	s, err := New(Config{PublicBaseURL: base, Enabled: true, Scopes: scopes, Timeouts: platform.DefaultTimeouts()}, Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestMetadata_Documents pins both discovery documents field by field
// (technical plan §43.14): what they advertise, and -- as importantly --
// what they must not (refresh tokens, revocation, registration, client ID
// metadata documents, a jwks_uri, mcp:write) before the pieces that build
// them exist.
func TestMetadata_Documents(t *testing.T) {
	t.Parallel()

	s := newUnitServer(t, "https://narvi.example", []mcpscope.Scope{mcpscope.Read})

	prm := httptest.NewRecorder()
	s.ProtectedResourceMetadata(prm, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil))
	const wantPRM = `{"resource":"https://narvi.example/mcp","authorization_servers":["https://narvi.example/oauth"],"scopes_supported":["mcp:read"],"bearer_methods_supported":["header"],"resource_name":"Narvi MCP"}`
	if got := prm.Body.String(); got != wantPRM {
		t.Errorf("protected resource metadata =\n %s\nwant\n %s", got, wantPRM)
	}

	asm := httptest.NewRecorder()
	s.AuthorizationServerMetadata(asm, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server/oauth", nil))
	const wantASM = `{"issuer":"https://narvi.example/oauth","authorization_endpoint":"https://narvi.example/oauth/authorize","token_endpoint":"https://narvi.example/oauth/token","scopes_supported":["mcp:read"],"response_types_supported":["code"],"response_modes_supported":["query"],"grant_types_supported":["authorization_code"],"token_endpoint_auth_methods_supported":["none"],"code_challenge_methods_supported":["S256"],"authorization_response_iss_parameter_supported":true}`
	if got := asm.Body.String(); got != wantASM {
		t.Errorf("authorization server metadata =\n %s\nwant\n %s", got, wantASM)
	}

	for name, rec := range map[string]*httptest.ResponseRecorder{"prm": prm, "asm": asm} {
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", name, rec.Code)
		}
		for header, want := range map[string]string{
			"Content-Type":                "application/json",
			"Access-Control-Allow-Origin": "*",
			"Cache-Control":               "public, max-age=300",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("%s %s = %q, want %q", name, header, got, want)
			}
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: not JSON: %v", name, err)
		}
		for _, absent := range []string{"jwks_uri", "revocation_endpoint", "registration_endpoint", "client_id_metadata_document_supported"} {
			if _, ok := doc[absent]; ok {
				t.Errorf("%s advertises %q, which this piece does not build", name, absent)
			}
		}
	}
}

// TestMetadata_ScopeLessBuildAdvertisesEmptyArray pins that an empty
// scope set is an empty JSON array, never null (a client reading
// scopes_supported must be able to range over it).
func TestMetadata_ScopeLessBuildAdvertisesEmptyArray(t *testing.T) {
	t.Parallel()

	s := newUnitServer(t, "https://narvi.example", nil)
	rec := httptest.NewRecorder()
	s.ProtectedResourceMetadata(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var doc struct {
		ScopesSupported []string `json:"scopes_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc.ScopesSupported == nil {
		t.Fatalf("scopes_supported = %#v (err %v), want an empty array", doc.ScopesSupported, err)
	}
}

func TestNew_RefusesBadConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"origin, enabled", Config{PublicBaseURL: "https://narvi.example", Enabled: true}, false},
		{"path, disabled: boots (routes answer 503 anyway)", Config{PublicBaseURL: "https://narvi.example/prefix"}, false},
		{"path, enabled: refused", Config{PublicBaseURL: "https://narvi.example/prefix", Enabled: true}, true},
		{"not absolute", Config{PublicBaseURL: "narvi.example"}, true},
		{"unknown scope", Config{PublicBaseURL: "https://narvi.example", Scopes: []mcpscope.Scope{"mcp:admin"}}, true},
	}
	for _, tc := range tests {
		_, err := New(tc.cfg, Deps{})
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: New err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
	if _, err := New(Config{PublicBaseURL: "https://narvi.example/prefix", Enabled: true}, Deps{}); !errors.Is(err, ErrPublicBaseURLNotAnOrigin) {
		t.Errorf("path while enabled: err = %v, want ErrPublicBaseURLNotAnOrigin", err)
	}
}

// TestScopeDescriptions_CoverVocabulary: the consent page never shows a
// scope it cannot explain.
func TestScopeDescriptions_CoverVocabulary(t *testing.T) {
	t.Parallel()

	for _, sc := range mcpscope.Vocabulary {
		if d, err := describeScope(sc); err != nil || d == "" {
			t.Errorf("describeScope(%q) = %q, %v, want a description", sc, d, err)
		}
	}
	if _, err := describeScope("mcp:admin"); !errors.Is(err, errNoDescription) {
		t.Errorf("describeScope(unknown) err = %v, want errNoDescription", err)
	}
}

// TestFormActionSource pins the consent page's CSP form-action: 'self'
// plus the stored redirect URI's own origin (browsers apply form-action to
// the redirect a form submission follows), with an IPv6 literal falling
// back to its scheme because CSP cannot express one.
func TestFormActionSource(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"https://client.example/cb", "'self' https://client.example"},
		{"https://client.example:8443/cb", "'self' https://client.example:8443"},
		{"http://127.0.0.1:52345/callback", "'self' http://127.0.0.1:52345"},
		{"http://localhost:8080/cb", "'self' http://localhost:8080"},
		{"http://[::1]:52345/callback", "'self' http:"},
	}
	for _, tc := range tests {
		if got := formActionSource(tc.in); got != tc.want {
			t.Errorf("formActionSource(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
