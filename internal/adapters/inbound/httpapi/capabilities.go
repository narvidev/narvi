// This file (capabilities.go) implements GET /api/capabilities: the
// derived read model behind the SPA's own runtime slot registry and
// GatekeeperAffordance (technical plan §34, docs/design/
// boundaries-design.md, section 4). One row per internal/domain/
// license.All entry, in that package's own fixed order -- see license.
// All's own doc comment for why order matters.
//
// GetCapabilities takes the ALREADY-BUILT restdtos.CapabilitiesResponse,
// via a func, never a *capability.Registry or a license.Capability. This
// package (internal/adapters/inbound/httpapi) imports neither
// internal/app/capability nor internal/domain/license anywhere -- see
// requirecapability.go's own doc comment for why: this file used to be
// one of two individually exempted, by name, on
// tools/lint/narvichecks/capabilityimportban's own allow-list, and that
// exemption is exactly what an exit audit of this guarantee bridged into
// scmcredentials.go, a real §30 suppression path elsewhere in this same
// package, through a package-level identifier this file could have
// declared with no import of its own. The row-building loop that used to
// live here moved to controlplane, next to where the registry itself is
// built (controlplane/modules.go, buildCapabilitiesResponse) -- the only
// place that legitimately imports both internal/app/capability and
// internal/domain/license.
//
// # What this handler MUST NEVER carry
//
// license.Grant's own KeyID/Subject fields never reach this file's own
// return value. controlplane.buildCapabilitiesResponse calls only
// *capability.Registry's own State and ExpiresAt -- neither one's own
// signature can return a key, a fingerprint of one, or a subject
// (Registry's own doc comment: "no method that can express a
// restriction" is the same shape argument for "no method that leaks the
// grant itself"). ExpiresAt is the one deliberate exception: a
// display-only *time.Time, never the key that produced it (technical
// plan §34.4's own "a read model may say that a capability is present,
// absent, or unlicensed; it may never carry a licence key, a subject, or
// a module's own vocabulary"). TestGetCapabilities_NeverCarriesTheKey
// pins this by scanning the raw response body for the configured key's
// own bytes.

package httpapi

import (
	"net/http"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/domain/authz"
)

// GetCapabilities backs GET /api/capabilities: 200 with
// restdtos.CapabilitiesResponse for every authenticated caller, viewer
// included (authz.ActionViewCapabilities, §13.3 row 1's "everyone,
// including viewer" shape -- see that action's own doc comment).
//
// build is called INSIDE the returned handler, once per request, never
// cached: controlplane closes over its own *capability.Registry and
// gatekeeperInstalled bool (buildCapabilitiesResponse's own doc comment
// covers why the latter is threaded in separately rather than derived
// from the registry), so a licence's state at the moment of the request
// -- never a snapshot taken at Mount time -- is what a caller gets back.
func GetCapabilities(build func() restdtos.CapabilitiesResponse) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionViewCapabilities, authz.Resource{}) {
			return
		}
		writeJSON(w, http.StatusOK, build())
	}
}
