package mcpauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/cimdfetch"
	"github.com/narvidev/narvi/internal/domain/mcpclient"
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

// TestMetadata_CacheLifetimeComesFromTimeouts: both discovery documents'
// max-age is platform.Timeouts.MCPDiscoveryCacheMaxAge (technical plan
// §43.14), never a literal of this package's own.
func TestMetadata_CacheLifetimeComesFromTimeouts(t *testing.T) {
	t.Parallel()

	to := platform.DefaultTimeouts()
	to.MCPDiscoveryCacheMaxAge = 90 * time.Second
	s, err := New(Config{PublicBaseURL: "https://narvi.example", Enabled: true, Scopes: []mcpscope.Scope{mcpscope.Read}, Timeouts: to}, Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for name, serve := range map[string]http.HandlerFunc{"prm": s.ProtectedResourceMetadata, "asm": s.AuthorizationServerMetadata} {
		rec := httptest.NewRecorder()
		serve(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if got := rec.Header().Get("Cache-Control"); got != "public, max-age=90" {
			t.Errorf("%s Cache-Control = %q, want %q", name, got, "public, max-age=90")
		}
	}
}

// TestMetadata_Documents pins both discovery documents field by field
// (technical plan §43.14): what they advertise -- the refresh-token grant
// and the RFC 7009 revocation endpoint included -- and, as importantly,
// what they must not (a jwks_uri, mcp:write, and -- with both
// client-registration mechanisms off, as here -- no registration endpoint
// and no client ID metadata document support;
// TestMetadata_ClientRegistrationAdvertisedOnlyWhenOn covers them on).
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
	const wantASM = `{"issuer":"https://narvi.example/oauth","authorization_endpoint":"https://narvi.example/oauth/authorize","token_endpoint":"https://narvi.example/oauth/token","revocation_endpoint":"https://narvi.example/oauth/revoke","scopes_supported":["mcp:read"],"response_types_supported":["code"],"response_modes_supported":["query"],"grant_types_supported":["authorization_code","refresh_token"],"token_endpoint_auth_methods_supported":["none"],"revocation_endpoint_auth_methods_supported":["none"],"code_challenge_methods_supported":["S256"],"authorization_response_iss_parameter_supported":true}`
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
		for _, absent := range []string{"jwks_uri", "registration_endpoint", "client_id_metadata_document_supported"} {
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

// refusingFetcher is a MetadataFetcher that fetches nothing.
type refusingFetcher struct{}

func (refusingFetcher) Fetch(context.Context, string) (cimdfetch.Result, error) {
	return cimdfetch.Result{}, errors.New("no fetch in this test")
}

// TestMetadata_ClientRegistrationAdvertisedOnlyWhenOn: the
// authorization-server metadata carries client_id_metadata_document_supported
// exactly while metadata documents are accepted, and registration_endpoint
// exactly while dynamic registration is -- each absent, never false or
// empty, when off (technical plan §43.15) -- and New refuses metadata
// documents with no fetcher wired.
func TestMetadata_ClientRegistrationAdvertisedOnlyWhenOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		mechanisms   mcpclient.Mechanisms
		cimd         bool
		registration string
	}{
		{"both off", mcpclient.Mechanisms{}, false, ""},
		{"metadata documents on", mcpclient.Mechanisms{MetadataDocuments: true}, true, ""},
		{"dynamic registration on", mcpclient.Mechanisms{DynamicRegistration: true}, false, "https://narvi.example/oauth/register"},
		{"both on", mcpclient.Mechanisms{MetadataDocuments: true, DynamicRegistration: true}, true, "https://narvi.example/oauth/register"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := New(Config{PublicBaseURL: "https://narvi.example", Enabled: true, Scopes: []mcpscope.Scope{mcpscope.Read}, Timeouts: platform.DefaultTimeouts(), Mechanisms: tc.mechanisms}, Deps{Metadata: refusingFetcher{}})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			rec := httptest.NewRecorder()
			s.AuthorizationServerMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server/oauth", nil))
			var doc map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if got, present := doc["client_id_metadata_document_supported"]; present != tc.cimd || (present && got != true) {
				t.Errorf("client_id_metadata_document_supported = %v (present %v), want present=%v and true", got, present, tc.cimd)
			}
			if got, present := doc["registration_endpoint"]; present != (tc.registration != "") || (present && got != tc.registration) {
				t.Errorf("registration_endpoint = %v (present %v), want %q", got, present, tc.registration)
			}
		})
	}
	if _, err := New(Config{PublicBaseURL: "https://narvi.example", Enabled: true, Timeouts: platform.DefaultTimeouts(), Mechanisms: mcpclient.Mechanisms{MetadataDocuments: true}}, Deps{}); err == nil {
		t.Error("New accepted metadata documents with no fetcher wired")
	}
}
