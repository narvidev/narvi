package mcpauth

import (
	"net/http"

	"github.com/narvidev/narvi/internal/domain/mcpscope"
)

// protectedResourceMetadataDoc is the RFC 9728 document for /mcp.
type protectedResourceMetadataDoc struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name"`
}

// authorizationServerMetadataDoc is this server's own RFC 8414 document
// -- deliberately its own struct, not the SDK's oauthex.AuthServerMeta,
// whose jwks_uri field has no omitempty (no token here is a JWT, so
// there is no key set to point at). It advertises only what is built:
// the authorization-code and refresh-token grants and the RFC 7009
// revocation endpoint, but no registration endpoint and no client ID
// metadata document support.
type authorizationServerMetadataDoc struct {
	Issuer                                     string   `json:"issuer"`
	AuthorizationEndpoint                      string   `json:"authorization_endpoint"`
	TokenEndpoint                              string   `json:"token_endpoint"`
	RevocationEndpoint                         string   `json:"revocation_endpoint"`
	ScopesSupported                            []string `json:"scopes_supported"`
	ResponseTypesSupported                     []string `json:"response_types_supported"`
	ResponseModesSupported                     []string `json:"response_modes_supported"`
	GrantTypesSupported                        []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported          []string `json:"token_endpoint_auth_methods_supported"`
	RevocationEndpointAuthMethodsSupported     []string `json:"revocation_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported"`
	AuthorizationResponseIssParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
}

func (s *Server) protectedResourceMetadata() protectedResourceMetadataDoc {
	return protectedResourceMetadataDoc{
		Resource:               s.ids.Resource,
		AuthorizationServers:   []string{s.ids.Issuer},
		ScopesSupported:        mcpscope.Strings(s.scopes),
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Narvi MCP",
	}
}

func (s *Server) authorizationServerMetadata() authorizationServerMetadataDoc {
	return authorizationServerMetadataDoc{
		Issuer:                                     s.ids.Issuer,
		AuthorizationEndpoint:                      s.ids.AuthorizationEndpoint,
		TokenEndpoint:                              s.ids.TokenEndpoint,
		RevocationEndpoint:                         s.ids.RevocationEndpoint,
		ScopesSupported:                            mcpscope.Strings(s.scopes),
		ResponseTypesSupported:                     []string{"code"},
		ResponseModesSupported:                     []string{"query"},
		GrantTypesSupported:                        []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported:          []string{"none"},
		RevocationEndpointAuthMethodsSupported:     []string{"none"},
		CodeChallengeMethodsSupported:              []string{"S256"},
		AuthorizationResponseIssParameterSupported: true,
	}
}

// writeMetadata writes one precomputed discovery document. Both are
// public, identical for every caller, and cacheable for
// platform.Timeouts.MCPDiscoveryCacheMaxAge; Access-Control-Allow-Origin:
// * lets a browser-based tool read them even though /mcp itself still
// refuses a foreign Origin (technical plan §43.13).
func (s *Server) writeMetadata(w http.ResponseWriter, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", s.metadataCacheControl)
	h.Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// ProtectedResourceMetadata backs GET
// /.well-known/oauth-protected-resource/mcp.
func (s *Server) ProtectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	s.writeMetadata(w, s.prmJSON)
}

// AuthorizationServerMetadata backs GET
// /.well-known/oauth-authorization-server/oauth.
func (s *Server) AuthorizationServerMetadata(w http.ResponseWriter, _ *http.Request) {
	s.writeMetadata(w, s.asmJSON)
}
