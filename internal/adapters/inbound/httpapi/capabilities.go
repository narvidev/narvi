// This file (capabilities.go) implements GET /api/capabilities: the
// derived read model behind the SPA's own runtime slot registry and
// GatekeeperAffordance (technical plan §34, docs/design/
// boundaries-design.md, section 4). One row per internal/domain/
// license.All entry, in that package's own fixed order -- see license.
// All's own doc comment for why order matters.
//
// This file and requirecapability.go are the only two files in this
// package tools/lint/narvichecks/capabilityimportban allows to import
// internal/app/capability or internal/domain/license at all -- pre-
// declared on that analyzer's own allow-list before this Step landed
// (see that analyzer's own doc comment, and docs/design/
// boundaries-design.md, section 1.4).
//
// # What this handler MUST NEVER carry
//
// license.Grant's own KeyID/Subject fields never reach this file's own
// return value. The only two *capability.Registry methods this handler
// calls are State and ExpiresAt -- neither one's own signature can
// return a key, a fingerprint of one, or a subject (Registry's own doc
// comment: "no method that can express a restriction" is the same shape
// argument for "no method that leaks the grant itself"). ExpiresAt is
// the one deliberate exception: a display-only *time.Time, never the key
// that produced it (technical plan §34.4's own "a read model may say
// that a capability is present, absent, or unlicensed; it may never
// carry a licence key, a subject, or a module's own vocabulary").
// TestGetCapabilities_NeverCarriesTheKey pins this by scanning the raw
// response body for the configured key's own bytes.

package httpapi

import (
	"net/http"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/app/capability"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/license"
)

// GetCapabilities backs GET /api/capabilities: 200 with
// restdtos.CapabilitiesResponse for every authenticated caller, viewer
// included (authz.ActionViewCapabilities, §13.3 row 1's "everyone,
// including viewer" shape -- see that action's own doc comment). reg may
// be nil (the public binary's own "no module composed" shape --
// capability.Registry.State/ExpiresAt are both nil-safe, so this handler
// needs no nil check of its own).
//
// gatekeeperInstalled is threaded in separately from reg, computed once
// at Build time from len(modules) (controlplane's own call site) --
// deliberately NOT derived from reg itself. "Any module is composed" and
// "capability c's own installed bit" are two different facts that only
// happen to coincide when every composed module declares at least one
// capability: reg's own installed set is the UNION of every module's
// declared Capabilities (internal/app/capability.New's own doc comment),
// so a module composed with zero declared capabilities (nothing in
// controlplane.validateModules forbids one) would leave every row below
// reading "not_installed" while gatekeeperInstalled is still, correctly,
// true. Keeping the two facts separate rather than inferring one from
// the other means this response stays honest even in that edge case.
func GetCapabilities(reg *capability.Registry, gatekeeperInstalled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, authz.ActionViewCapabilities, authz.Resource{}) {
			return
		}

		rows := make([]restdtos.CapabilityStatus, 0, len(license.All))
		for _, c := range license.All {
			rows = append(rows, restdtos.CapabilityStatus{
				Name:  restdtos.CapabilityStatusName(c),
				State: restdtos.CapabilityState(reg.State(c)),
			})
		}

		writeJSON(w, http.StatusOK, restdtos.CapabilitiesResponse{
			GatekeeperInstalled: gatekeeperInstalled,
			LicenseExpiresAt:    reg.ExpiresAt(),
			Capabilities:        rows,
		})
	}
}
