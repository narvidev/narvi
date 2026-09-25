// Package mcpauth is the MCP surface's own OAuth 2.1 authorization server
// (technical plan §43.13-§43.16): the discovery documents, the
// authorization endpoint, the consent page, the token endpoint that issues
// -- and, with a refresh token, renews -- the bearer tokens POST /mcp
// accepts, and the RFC 7009 endpoint a client gives them back through. The resource-server half --
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
//	POST /oauth/token                                  token.go, refresh.go
//	POST /oauth/revoke                                 revoke.go
//
// Every route is mounted by controlplane/serve.go behind the MCP surface's
// own enabled-gate (503 when off) and none behind the cookie middleware:
// the consent routes authenticate the cookie themselves (auth.Authenticate)
// because a signed-out browser must be sent to sign in, not answered 401;
// the token and revocation endpoints read no cookie at all.
//
// # Invariants this package is responsible for
//
//   - A bad client_id or redirect_uri renders a page and never redirects;
//     every redirect goes to a URI that matched the client's registration
//     (internal/domain/mcpclient), and the consent decision redirects to
//     the STORED URI, never one read from the form.
//   - PKCE S256 is mandatory; the verifier is checked in constant time.
//   - A code is single-use; a replayed code deletes the grant it produced.
//   - A refresh token rotates on every use; presenting a rotated one
//     deletes its grant. A refresh can only narrow the scopes the refresh
//     token holds, never widen them to the grant's.
//   - Revoking any token revokes its whole grant, and only the client it
//     was issued to can do so; the answer never says which happened.
//   - resource is required and bound at authorization and at the code
//     exchange; on a refresh it is optional, but one that is sent must
//     match, and the grant's own must still be this deployment's; and
//     auth.RequireMCPBearer checks it on every call.
//   - Only scopes the tool table requires are offered; the user can only
//     narrow them.
//   - Every token and code holds the scopes fixed when it was issued.
//   - Codes, tokens and consent nonces exist in plaintext only in the one
//     response that hands them out.
package mcpauth
