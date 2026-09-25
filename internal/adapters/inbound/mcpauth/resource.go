package mcpauth

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/narvidev/narvi/internal/platform"
)

// The fixed paths every identifier below is built from. The route
// registrations in controlplane/serve.go are these same literals (the
// route scanner only reads literals); TestDeriveIdentifiers pins that the
// derived URLs land on them.
const (
	resourcePath                      = "/mcp"
	issuerPath                        = "/oauth"
	authorizePath                     = issuerPath + "/authorize"
	consentPath                       = issuerPath + "/consent"
	tokenPath                         = issuerPath + "/token"
	protectedResourceMetadataPrefix   = "/.well-known/oauth-protected-resource"
	authorizationServerMetadataPrefix = "/.well-known/oauth-authorization-server"
)

// Identifiers are the URIs this deployment's MCP resource server and
// authorization server are known by (technical plan §43.13), all derived
// from PublicBaseURL and never configured separately.
type Identifiers struct {
	// Origin is PublicBaseURL's canonical origin (platform.CanonicalOrigin).
	Origin string
	// BasePath is PublicBaseURL's own path with trailing slashes trimmed
	// -- "" for the usual origin-only value. New refuses a non-empty one
	// while the surface is enabled (ValidatePublicBaseURL).
	BasePath string
	// Issuer is the authorization server's issuer identifier.
	Issuer string
	// Resource is the MCP endpoint's canonical URI: the audience every
	// token is bound to.
	Resource string
	// ProtectedResourceMetadataURL is where the RFC 9728 document lives
	// (path insertion of Resource's path); the /mcp 401 challenge points
	// here.
	ProtectedResourceMetadataURL string
	// AuthorizationServerMetadataURL is where the RFC 8414 document lives
	// (path insertion of Issuer's path).
	AuthorizationServerMetadataURL string
	// AuthorizationEndpoint and TokenEndpoint are the two endpoints the
	// authorization server metadata advertises.
	AuthorizationEndpoint string
	TokenEndpoint         string
}

// DeriveIdentifiers computes Identifiers from publicBaseURL. It fails only
// for a value that is not an absolute URL -- the same boot-time
// requirement the /mcp Origin gate already imposes on PublicBaseURL.
func DeriveIdentifiers(publicBaseURL string) (Identifiers, error) {
	origin, err := platform.CanonicalOrigin(publicBaseURL)
	if err != nil {
		return Identifiers{}, fmt.Errorf("mcpauth: PublicBaseURL: %w", err)
	}
	u, err := url.Parse(publicBaseURL)
	if err != nil {
		return Identifiers{}, fmt.Errorf("mcpauth: PublicBaseURL: %w", err)
	}
	basePath := strings.TrimRight(u.EscapedPath(), "/")
	base := origin + basePath
	return Identifiers{
		Origin:                         origin,
		BasePath:                       basePath,
		Issuer:                         base + issuerPath,
		Resource:                       base + resourcePath,
		ProtectedResourceMetadataURL:   origin + protectedResourceMetadataPrefix + basePath + resourcePath,
		AuthorizationServerMetadataURL: origin + authorizationServerMetadataPrefix + basePath + issuerPath,
		AuthorizationEndpoint:          base + authorizePath,
		TokenEndpoint:                  base + tokenPath,
	}, nil
}

// ErrPublicBaseURLNotAnOrigin is returned by ValidatePublicBaseURL.
var ErrPublicBaseURLNotAnOrigin = errors.New("mcpauth: PublicBaseURL must be an origin (scheme and host, optional port, no path, query or fragment) while the MCP surface is enabled")

// ValidatePublicBaseURL reports whether publicBaseURL can host the MCP
// authorization server: both discovery documents are served at the ROOT
// of its origin (the route table's own literals), so a base URL carrying a
// path would advertise metadata URLs this binary does not serve, and a
// query or fragment has no place in an issuer or resource identifier.
func ValidatePublicBaseURL(publicBaseURL string) error {
	u, err := url.Parse(publicBaseURL)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("%w: %q is not an absolute URL", ErrPublicBaseURLNotAnOrigin, publicBaseURL)
	}
	if strings.Trim(u.EscapedPath(), "/") != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%w: got %q", ErrPublicBaseURLNotAnOrigin, publicBaseURL)
	}
	return nil
}

// canonicalResource normalizes a client-sent resource indicator for
// comparison (RFC 8707 section 2; the MCP authorization spec asks servers to
// accept upper-case scheme and host and the form with a trailing slash):
// scheme and host lower-cased, the default port elided, ONE trailing
// slash trimmed from the path. Path case and percent-encoding are NOT
// normalized -- a path is case-sensitive. A value with a query, a
// fragment, userinfo, a non-http(s) scheme, or no host is refused (ok ==
// false): RFC 8707 forbids a fragment, and nothing this deployment
// serves is identified with a query.
func canonicalResource(raw string) (string, bool) {
	if raw == "" || strings.ContainsAny(raw, "?#") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.Host == "" {
		return "", false
	}
	origin, err := platform.CanonicalOrigin(raw)
	if err != nil {
		return "", false
	}
	return origin + strings.TrimSuffix(u.EscapedPath(), "/"), true
}

// matchesResource reports whether raw, once canonicalized, is this
// deployment's own MCP resource.
func (ids Identifiers) matchesResource(raw string) bool {
	got, ok := canonicalResource(raw)
	return ok && got == ids.Resource
}
