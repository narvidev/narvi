// This file (requirecapability.go) implements RequireCapability, modelled
// exactly on RequireCloudIdentityCapability (cloudidentitycapability.go,
// see that file's own doc comment for the full "why a 503, not a 404"
// reasoning): a capability that is off must be OBSERVABLE as off, never
// indistinguishable from a route that was never built at all -- 404 would
// look identical to "this deployment's binary predates this capability
// entirely", where 503 unambiguously says "this deployment knows about
// this capability, and it is not usable here".
//
// This file and capabilities.go (GET /api/capabilities, the derived read
// model) are the only two files in this package
// tools/lint/narvichecks/capabilityimportban allows to import
// internal/app/capability, internal/domain/license, or
// github.com/narvidev/narvi/extension at all -- see that analyzer's own
// doc comment, and docs/design/boundaries-design.md, section 1.4, for why this is
// an IMPORT ban rather than a ban on calling Registry.Enabled by name: a
// wrapper or a method value can dodge a call-name ban, never an import
// declaration.

package httpapi

import (
	"net/http"

	"github.com/narvidev/narvi/internal/app/capability"
	"github.com/narvidev/narvi/internal/domain/license"
)

// RequireCapability returns chi middleware that responds 503 (fail-closed,
// the same message shape every sibling capability gate in this package
// uses) for the ENTIRE route group it wraps whenever reg.Enabled(c) is
// false. Nil-safe: reg may be nil (the public binary's own "no module
// composed" shape, since nothing there ever builds a *capability.Registry
// at all) -- capability.Registry.Enabled itself answers false for a nil
// receiver, so this never needs its own nil check.
//
// Enabled is called INSIDE the returned handler, once per request, never
// once at Mount time -- so a licence that expires mid-process closes
// every route this middleware wraps on the very next request, exactly
// like RequireCloudIdentityCapability's own identical per-request
// evaluation. Only a module's own routes (controlplane's own
// /api/ext/<name>/ mounting, extension.Runtime.RequireCapability) are
// ever wrapped with this.
func RequireCapability(reg *capability.Registry, c license.Capability) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !reg.Enabled(c) {
				writeError(w, http.StatusServiceUnavailable, "this capability is not enabled on this deployment")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
