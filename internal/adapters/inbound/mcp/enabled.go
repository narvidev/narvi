package mcp

import "net/http"

// disabledBody is the exact JSON body httpapi.RequireCapability's own
// sibling gates write on a 503 -- reproduced verbatim (not imported: this
// package must not import httpapi's own RequireCapability, which takes a
// func() bool for a licence/registry whose state can change mid-process;
// NARVI_MCP_ENABLED is a plain, boot-time-fixed bool, so RequireEnabled
// below takes the value itself, not a decider function) so an operator
// sees the SAME "this capability is not enabled on this deployment"
// message regardless of which surface answered.
const disabledBody = `{"error":"this capability is not enabled on this deployment"}`

// RequireEnabled returns chi middleware that answers 503 for the ENTIRE
// /mcp route group whenever enabled is false (technical plan §43.11).
// Mounted SECOND in the group's own middleware chain -- after
// mcp.RequireTrustedOrigin (§43.2/§43.6: an invalid Origin is refused 403
// unconditionally, before this gate ever runs), but still BEFORE auth.
// Middleware -- so a deployment that has not opted in never touches the
// session store at all for this surface, mirroring the OIDC routes' own
// "503 unauthenticated when unconfigured" precedent (§43 D9). The route
// itself is still mounted unconditionally regardless of enabled's value,
// so routes.golden, the guide-omission register, and tools/contractscompat
// see the identical route table whether the flag is on or off.
func RequireEnabled(enabled bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !enabled {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(disabledBody))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
