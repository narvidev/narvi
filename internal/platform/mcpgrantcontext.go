// This file (mcpgrantcontext.go) carries the MCP authorization a /mcp
// request was made under (technical plan §43.16/§43.17), alongside the
// AuthenticatedUser authcontext.go already carries. internal/adapters/
// inbound/auth's RequireMCPBearer attaches both after verifying a bearer
// access token; internal/adapters/inbound/mcp reads the grant to decide
// which tools the request may see. The REST twins a tool invokes authorize
// from the AuthenticatedUser alone, exactly as for a cookie request, so a
// grant can only ever SUBTRACT (hide tools), never add a permission the
// user does not already hold. Where else it is read, never as a permission:
// the status route's bounded wait (technical plan §43.20), whose per-caller
// cap counts against its GrantID; the /mcp call brake and
// narvi_create_session's brake, keyed by its GrantID (§43.6/§43.8); REST
// session creation, which records spawn_source mcp exactly when a grant is
// present (§43.1); and auditlog.Record, which stamps the GrantID and
// ClientID on the audit row of a change made through it (§43.18).

package platform

import "context"

// MCPGrant is the authorization an MCP access token was issued under.
// Scopes holds the ACCESS TOKEN's scope strings verbatim -- fixed when the
// token was issued to exactly what the user approved in that flow, never
// widened or narrowed by a later consent for the same client (possibly
// empty: a scope-less token is legitimate and sees no tools).
type MCPGrant struct {
	GrantID  string
	ClientID string
	Scopes   []string
}

// mcpGrantKey is the unexported context-key type for MCPGrant, mirroring
// userKey (authcontext.go).
type mcpGrantKey struct{}

// WithMCPGrant returns a copy of ctx carrying g. g.Scopes is copied, so a
// caller mutating its own slice afterwards cannot change what later
// readers see.
func WithMCPGrant(ctx context.Context, g MCPGrant) context.Context {
	g.Scopes = append([]string{}, g.Scopes...)
	return context.WithValue(ctx, mcpGrantKey{}, g)
}

// MCPGrantFromContext returns the MCPGrant stored in ctx and whether one
// was present. The returned Scopes is a fresh copy.
func MCPGrantFromContext(ctx context.Context) (MCPGrant, bool) {
	g, ok := ctx.Value(mcpGrantKey{}).(MCPGrant)
	if !ok {
		return MCPGrant{}, false
	}
	g.Scopes = append([]string{}, g.Scopes...)
	return g, true
}
