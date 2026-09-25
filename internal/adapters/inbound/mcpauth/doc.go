// Package mcpauth is the MCP surface's own OAuth 2.1 authorization server
// (technical plan §43.13-§43.16): the discovery documents, the
// authorization endpoint, the consent page, and the token endpoint that
// issue the bearer tokens POST /mcp accepts. The resource-server half --
// verifying a bearer token on every /mcp call -- is
// internal/adapters/inbound/auth's RequireMCPBearer, because the mcp
// adapter may import auth but never a store.
//
// The name is deliberately none of the three it could be confused with:
// not auth (the browser cookie session), not mcp (the protocol endpoint),
// not chatgptoauth (Narvi as an OAuth CLIENT of a model provider). This
// package is Narvi as an OAuth SERVER, and its credentials are a family of
// their own: issued and verified here, stored only as platform.HashToken
// output, never encrypted, never logged.
//
// # What a request goes through
//
//	GET  /.well-known/oauth-protected-resource/mcp     metadata.go
//	GET  /.well-known/oauth-authorization-server/oauth metadata.go
//	GET  /oauth/authorize                              authorize.go
//	GET  /oauth/consent?request=<id>                   consent.go
//	POST /oauth/consent                                consent.go
//	POST /oauth/token                                  token.go
//
// Every route is mounted by controlplane/serve.go behind the MCP surface's
// own enabled-gate (503 when off) and none behind the cookie middleware:
// the consent routes authenticate the cookie themselves (auth.Authenticate)
// because a signed-out browser must be sent to sign in, not answered 401;
// the token endpoint reads no cookie at all.
//
// # Invariants this package is responsible for
//
//   - A bad client_id or redirect_uri renders a page and never redirects;
//     every redirect goes to a URI that matched the client's registration
//     (internal/domain/mcpclient), and the consent decision redirects to
//     the STORED URI, never one read from the form.
//   - PKCE S256 is mandatory; the verifier is checked in constant time.
//   - A code is single-use; a replayed code deletes the grant it produced.
//   - resource is required and bound at authorization, at exchange, and
//     (by auth.RequireMCPBearer) on every call.
//   - Only scopes the tool table requires are offered; the user can only
//     narrow them.
//   - Codes, tokens and consent nonces exist in plaintext only in the one
//     response that hands them out.
package mcpauth
