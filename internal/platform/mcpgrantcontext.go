// This file (mcpgrantcontext.go) carries the MCP authorization a /mcp
// request was made under (technical plan §43.16/§43.17), alongside the
// AuthenticatedUser authcontext.go already carries. internal/adapters/
// inbound/auth's RequireMCPBearer attaches both after verifying a bearer
// access token; internal/adapters/inbound/mcp reads the grant to decide
// which tools the request may see. Nothing else reads it: the REST twins
// a tool invokes see only the AuthenticatedUser, exactly as for a cookie
// request, so a grant can only ever SUBTRACT (hide tools), never add a
// permission the user does not already hold.

package platform

import "context"

// MCPGrant is the authorization an MCP access token was issued under.
// Scopes holds the grant's scope strings verbatim (possibly empty: a
// scope-less grant is legitimate and sees no tools).
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
