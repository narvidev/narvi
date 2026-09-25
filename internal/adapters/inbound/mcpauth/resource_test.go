package mcpauth

import (
	"errors"
	"testing"
)

// TestDeriveIdentifiers pins every identifier's derivation from
// PublicBaseURL, including canonicalization (the SDK client compares the
// protected-resource document's resource to the URL it dialed byte for
// byte) and that each advertised URL lands on the literal route
// controlplane/serve.go registers.
func TestDeriveIdentifiers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		base string
		want Identifiers
	}{
		{
			base: "https://narvi.example",
			want: Identifiers{
				Origin:                         "https://narvi.example",
				Issuer:                         "https://narvi.example/oauth",
				Resource:                       "https://narvi.example/mcp",
				ProtectedResourceMetadataURL:   "https://narvi.example/.well-known/oauth-protected-resource/mcp",
				AuthorizationServerMetadataURL: "https://narvi.example/.well-known/oauth-authorization-server/oauth",
				AuthorizationEndpoint:          "https://narvi.example/oauth/authorize",
				TokenEndpoint:                  "https://narvi.example/oauth/token",
			},
		},
		{
			base: "HTTPS://Narvi.Example:443/",
			want: Identifiers{
				Origin:                         "https://narvi.example",
				Issuer:                         "https://narvi.example/oauth",
				Resource:                       "https://narvi.example/mcp",
				ProtectedResourceMetadataURL:   "https://narvi.example/.well-known/oauth-protected-resource/mcp",
				AuthorizationServerMetadataURL: "https://narvi.example/.well-known/oauth-authorization-server/oauth",
				AuthorizationEndpoint:          "https://narvi.example/oauth/authorize",
				TokenEndpoint:                  "https://narvi.example/oauth/token",
			},
		},
		{
			base: "http://127.0.0.1:52345",
			want: Identifiers{
				Origin:                         "http://127.0.0.1:52345",
				Issuer:                         "http://127.0.0.1:52345/oauth",
				Resource:                       "http://127.0.0.1:52345/mcp",
				ProtectedResourceMetadataURL:   "http://127.0.0.1:52345/.well-known/oauth-protected-resource/mcp",
				AuthorizationServerMetadataURL: "http://127.0.0.1:52345/.well-known/oauth-authorization-server/oauth",
				AuthorizationEndpoint:          "http://127.0.0.1:52345/oauth/authorize",
				TokenEndpoint:                  "http://127.0.0.1:52345/oauth/token",
			},
		},
		{
			base: "https://narvi.example/prefix/",
			want: Identifiers{
				Origin:                         "https://narvi.example",
				BasePath:                       "/prefix",
				Issuer:                         "https://narvi.example/prefix/oauth",
				Resource:                       "https://narvi.example/prefix/mcp",
				ProtectedResourceMetadataURL:   "https://narvi.example/.well-known/oauth-protected-resource/prefix/mcp",
				AuthorizationServerMetadataURL: "https://narvi.example/.well-known/oauth-authorization-server/prefix/oauth",
				AuthorizationEndpoint:          "https://narvi.example/prefix/oauth/authorize",
				TokenEndpoint:                  "https://narvi.example/prefix/oauth/token",
			},
		},
	}
	for _, tc := range tests {
		got, err := DeriveIdentifiers(tc.base)
		if err != nil {
			t.Errorf("DeriveIdentifiers(%q) err = %v", tc.base, err)
			continue
		}
		if got != tc.want {
			t.Errorf("DeriveIdentifiers(%q) =\n %+v\nwant\n %+v", tc.base, got, tc.want)
		}
	}

	for _, bad := range []string{"", "narvi.example", "/relative"} {
		if _, err := DeriveIdentifiers(bad); err == nil {
			t.Errorf("DeriveIdentifiers(%q) err = nil, want an error", bad)
		}
	}
}

func TestValidatePublicBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		base string
		ok   bool
	}{
		{"https://narvi.example", true},
		{"https://narvi.example/", true},
		{"http://127.0.0.1:8080", true},
		{"https://narvi.example/prefix", false},
		{"https://narvi.example/?x=1", false},
		{"https://narvi.example/#f", false},
		{"https://user@narvi.example", false},
		{"narvi.example", false},
	}
	for _, tc := range tests {
		err := ValidatePublicBaseURL(tc.base)
		if (err == nil) != tc.ok {
			t.Errorf("ValidatePublicBaseURL(%q) err = %v, want ok=%v", tc.base, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrPublicBaseURLNotAnOrigin) {
			t.Errorf("ValidatePublicBaseURL(%q) err = %v, want ErrPublicBaseURLNotAnOrigin", tc.base, err)
		}
	}
}

// TestMatchesResource is the resource-indicator comparison table: the
// canonical form and the normalizations the MCP spec asks a server to
// tolerate are accepted; every other difference is refused.
func TestMatchesResource(t *testing.T) {
	t.Parallel()

	ids, err := DeriveIdentifiers("https://narvi.example")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		raw  string
		want bool
	}{
		{"https://narvi.example/mcp", true},
		{"https://narvi.example/mcp/", true},
		{"HTTPS://NARVI.EXAMPLE/mcp", true},
		{"https://narvi.example:443/mcp", true},
		{"https://narvi.example/MCP", false},
		{"https://narvi.example/mcp//", false},
		{"https://narvi.example/mcp?x=1", false},
		{"https://narvi.example/mcp#f", false},
		{"https://narvi.example:8443/mcp", false},
		{"http://narvi.example/mcp", false},
		{"https://other.example/mcp", false},
		{"https://narvi.example", false},
		{"https://narvi.example/", false},
		{"https://user@narvi.example/mcp", false},
		{"https://narvi.example/%6Dcp", false},
		{"ftp://narvi.example/mcp", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := ids.matchesResource(tc.raw); got != tc.want {
			t.Errorf("matchesResource(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
