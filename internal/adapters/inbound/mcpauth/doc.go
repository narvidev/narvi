// Package mcpauth is the MCP surface's own OAuth 2.1 authorization server
// (technical plan §43.13-§43.16): the discovery documents, the
// authorization endpoint, the consent page, the token endpoint that issues
// -- and, with a refresh token, renews -- the bearer tokens POST /mcp
// accepts, the RFC 7009 endpoint a client gives them back through, and the
// two ways a client with no prior relationship registers (§43.15): a
// client ID metadata document, fetched through the SSRF-guarded
// internal/adapters/outbound/cimdfetch, and RFC 7591 dynamic registration. The resource-server half --
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
//	POST /oauth/register                               register.go, ratelimit.go
//
// A metadata-document client (an https client_id) is resolved at
// GET /oauth/authorize, from its cached or freshly fetched document
// (cimd.go); nothing else ever fetches one.
//
// Every route is mounted by controlplane/serve.go behind the MCP surface's
// own enabled-gate (503 when off) and none behind the cookie middleware:
// the consent routes authenticate the cookie themselves (auth.Authenticate)
// because a signed-out browser must be sent to sign in, not answered 401;
// the token, revocation and registration endpoints read no cookie at all.
// The registration endpoint is behind its own flag too (off by default)
// and a per-address rate limit.
//
// # Invariants this package is responsible for
//
//   - A bad client_id or redirect_uri renders a page and never redirects;
//     every redirect goes to a URI that matched the client's registration
//     (internal/domain/mcpclient), and the consent decision redirects to
//     the STORED URI, never one read from the form.
//   - PKCE S256 is mandatory; the verifier is checked in constant time.
//   - A code is single-use; a replayed code deletes the grant it produced.
//   - A refresh token rotates on every use; presenting a rotated one, or
//     one issued to another client, deletes its grant -- unless it, its
//     chain or its grant has expired, which refreshes nothing and revokes
//     nothing. A refresh can only narrow the scopes the refresh token
//     holds, never widen them to the grant's.
//   - A refresh chain's scopes, resource and absolute end are fixed when
//     its code is exchanged and carried unchanged through every rotation:
//     a later consent for the same client, which renews the grant in
//     place, never extends, rebinds or widens a chain an earlier consent
//     began.
//   - Revoking any token revokes its whole grant, and only the client it
//     was issued to can do so; the answer never says which happened.
//   - resource is required and bound at authorization and at the code
//     exchange; on a refresh it is optional, but one that is sent must
//     match, and the refresh chain's own must still be this deployment's;
//     and auth.RequireMCPBearer checks it on every call.
//   - Only scopes the tool table requires are offered; the user can only
//     narrow them.
//   - Every token and code holds the scopes fixed when it was issued.
//   - Codes, tokens and consent nonces exist in plaintext only in the one
//     response that hands them out.
//   - A client registered through a mechanism the deployment switched off
//     is refused everywhere, like a disabled one (Server.clientUsable, and
//     auth.RequireMCPBearer with the same mcpclient.Mechanisms) -- except
//     that it may still give its tokens back.
//   - The consent page's identity line is never the client's own claim: an
//     administrator's registration, the host a metadata document was
//     fetched from, or the plain statement that the app registered itself.
//     The name a client chose is shown second, escaped, and was refused at
//     registration if it could render deceptively.
//   - A metadata document is trusted for at most MCPClientMetadataCacheTTL
//     -- its own Cache-Control only shortens that -- and re-fetching it
//     changes what the next authorization sees, never what an issued code
//     or token can do.
package mcpauth
