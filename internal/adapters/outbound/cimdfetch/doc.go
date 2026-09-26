// Package cimdfetch fetches OAuth client ID metadata documents for the MCP
// authorization server (technical plan §43.15): a client that identifies
// itself by an https URL is described by the JSON document at that URL,
// and Narvi fetches it when the client first asks a user for access.
//
// The URL is chosen by whoever sends the authorization request -- an
// unauthenticated party -- so every fetch is a server-side request an
// attacker aims. This package is the one place that request is made, and
// it is built so that it can only ever reach the public internet:
//
//   - The only way to get an HTTP client for a Fetcher is NewGuardedClient
//     (the same capability-token shape internal/adapters/outbound/githubapi's
//     NewGatedClient uses for its shadow gate): there is no exported way to
//     hand a Fetcher a transport without the guard, and a Fetcher built with
//     none refuses every fetch.
//   - The address check runs at DIAL time, in a net.Dialer Control hook, on
//     the exact address being connected to -- after resolution, for every
//     connection, redirects included. A name that resolved to a public
//     address when something else looked it up and to a private one now
//     (DNS rebinding) is refused, because nothing checks a name: only the
//     address a socket is about to connect to is checked. Keep-alives are
//     off, so every request dials, resolves and is checked afresh.
//   - Refused: loopback, private (RFC 1918, IPv6 ULA), link-local (which
//     holds the cloud metadata address), CGNAT shared space, multicast,
//     unspecified, documentation, benchmarking and every other
//     special-purpose block, and the IPv4-mapped and NAT64-embedded IPv6
//     forms of each (addressPolicy has the list).
//   - https only, on the first request and every redirect: never a
//     downgrade to http, never another scheme. At most MaxRedirects
//     redirects, each within the first URL's own origin (scheme, host,
//     port): the document is served by the origin its URL names, so the
//     host shown as the client's identity is the host it came from -- an
//     open redirect elsewhere cannot lend another host's name to it. Every
//     host -- the first URL's and each redirect's Location's -- must be
//     written in plain ASCII (no percent sign in the authority, the parsed
//     host exactly the bytes written), the client_id host rule, and hosts
//     are compared folding ASCII case only: net/http resolves a non-ASCII
//     host by its IDNA form, which no comparison of the host as parsed
//     can stand for (U+0130 lower-cases to "i", but IDNA makes it "i" and
//     a combining dot, another domain), so the host compared is always
//     the very string the transport resolves. No
//     proxy from the environment -- a proxy would dial on the guard's
//     behalf, out of its reach.
//   - The answer must be 200 with Content-Type application/json, say where
//     its body ends -- a Content-Length or chunked: a body only the
//     connection closing ends reads, cut short, exactly like a whole one --
//     and be at most MaxDocumentBytes long (one byte more is read, and
//     refused). One timeout (platform.Timeouts.MCPClientMetadataFetchTimeout)
//     bounds the whole fetch, body included, and the body must be read to
//     its end within it: a body its server ends only because the fetch gave
//     up is a timeout, never a document.
//
// What the document must SAY is not this package's business: parsing and
// validating it is internal/domain/mcpclient's ParseMetadataDocument, and
// how long a fetched document is trusted is the caller's (the response's
// own Cache-Control is reported, and may only shorten that).
package cimdfetch
