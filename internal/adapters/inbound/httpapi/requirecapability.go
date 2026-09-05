// This file (requirecapability.go) implements RequireCapability, modelled
// exactly on RequireCloudIdentityCapability (cloudidentitycapability.go,
// see that file's own doc comment for the full "why a 503, not a 404"
// reasoning): a capability that is off must be OBSERVABLE as off, never
// indistinguishable from a route that was never built at all -- 404 would
// look identical to "this deployment's binary predates this capability
// entirely", where 503 unambiguously says "this deployment knows about
// this capability, and it is not usable here".
//
// RequireCapability takes the DECISION, never the decider: a func() bool,
// never a *capability.Registry or a license.Capability. This package
// (internal/adapters/inbound/httpapi) imports neither
// internal/app/capability nor internal/domain/license anywhere, and
// tools/lint/narvichecks/capabilityimportban no longer carries a
// file-level exemption for this file -- see that analyzer's own doc
// comment for why: a package-level identifier a single allow-listed file
// declares is reachable, with no import of its own, from every OTHER
// file in the SAME package, which is exactly the shape an exit audit of
// this guarantee found bridged into scmcredentials.go, a real §30
// suppression path, through this file's own former exemption. Closing
// that means this file must never hold a license.Capability or a
// *capability.Registry at all: controlplane (which legitimately imports
// both) evaluates the decision and hands this function only the already-
// evaluated bool.

package httpapi

import (
	"net/http"
)

// RequireCapability returns chi middleware that responds 503 (fail-closed,
// the same message shape every sibling capability gate in this package
// uses) for the ENTIRE route group it wraps whenever enabled() is false.
//
// enabled is called INSIDE the returned handler, once per request, never
// once at Mount time -- so a licence that expires mid-process closes
// every route this middleware wraps on the very next request, exactly
// like RequireCloudIdentityCapability's own identical per-request
// evaluation. The caller (controlplane) closes over its own
// *capability.Registry and license.Capability, typically as
// `func() bool { return reg.Enabled(c) }` -- capability.Registry.Enabled
// answers false for a nil receiver, so a nil registry (the public
// binary's own "no module composed" shape) needs no special case here
// either. Only a module's own routes (controlplane's own
// /api/ext/<name>/ mounting, extension.Runtime.RequireCapability) are
// ever wrapped with this.
func RequireCapability(enabled func() bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !enabled() {
				writeError(w, http.StatusServiceUnavailable, "this capability is not enabled on this deployment")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
