// This file (registry.go) implements Registry -- see doc.go for the
// package-level "why".

package capability

import (
	"time"

	"github.com/narvidev/narvi/internal/domain/license"
)

// State names WHY a capability answers the way it does -- Enabled
// collapses all of this to a bare bool; State exists purely for
// diagnostics (boot logging, and GET /api/capabilities), never as a
// second way to decide behavior. The values mirror docs/design/
// boundaries-design.md, sections 1.2 and 4.2's own wire enum exactly --
// changing one is now a breaking contract change: that wire enum
// (CapabilityState, contracts/rest/v1/dtos.schema.json) has shipped.
type State string

// The six states a capability can be in -- see this type's own doc
// comment.
const (
	StateEnabled      State = "enabled"
	StateNotInstalled State = "not_installed"
	StateNotLicensed  State = "not_licensed"
	StateExpired      State = "license_expired"
	StateNotYetValid  State = "license_not_yet_valid"
	StateInvalid      State = "license_invalid"
)

// Registry is the single point of truth docs/design/boundaries-design.md,
// section 1.1, names: computed once, at boot, from the installed set (which
// modules the composition root composed) and the parsed licence grant
// (or lack of one), then re-evaluated per call against an injected clock
// so a grant's own time window is never stale for longer than one call.
//
// Every field is unexported. There is no constructor other than [New],
// and no method that can express a restriction -- see [Registry.Enabled]'s
// own doc comment.
type Registry struct {
	installed map[license.Capability]bool
	grant     *license.Grant
	parseErr  error
	now       func() time.Time
	nbfSkew   time.Duration
}

// New builds a Registry. installed is the union of every composed
// module's own declared capabilities (empty in the public binary, which
// composes none); grant is the result of parsing the configured licence
// key at boot (nil if absent or unparseable); parseErr is the error
// [license.Parse] itself returned (nil if the key was absent, or if it
// parsed cleanly) -- carried separately from grant purely so [Registry.
// State] can distinguish "no key configured" (StateNotLicensed) from "a
// key was configured but did not parse" (StateInvalid), a distinction
// [Registry.Enabled] itself has no need for (both cases are grant == nil,
// hence false, either way). now and nbfSkew are threaded straight to
// [license.Grant.ValidAt] on every [Registry.Enabled]/[Registry.State]
// call -- now is a func, never a fixed time.Time, specifically so it is
// re-read every call rather than captured once at construction (§34.5:
// "the registry is re-checked per call").
func New(installed []license.Capability, grant *license.Grant, parseErr error, now func() time.Time, nbfSkew time.Duration) *Registry {
	set := make(map[license.Capability]bool, len(installed))
	for _, c := range installed {
		set[c] = true
	}
	return &Registry{
		installed: set,
		grant:     grant,
		parseErr:  parseErr,
		now:       now,
		nbfSkew:   nbfSkew,
	}
}

// Enabled is the single point of truth: installed AND licensed AND
// valid-now, exactly, with no other input. Nil-safe -- a nil *Registry
// (never constructed via [New] at all, which is what a caller gets if it
// skips building one) answers false for every capability, matching the
// public binary's own "no module composed" behavior by construction
// rather than by a caller remembering to check for nil first.
//
// Returns a bare bool, never (bool, error): there is no second return
// value for a caller to discard on the way to accidentally treating an
// unparseable or expired key as enabled (mirrors internal/app/egressmode.
// Resolve's own identical reasoning for the same shape). Every failure
// direction -- no module installed, no grant, a grant that does not name
// c, a grant outside its own time window -- collapses to the same false a
// caller cannot tell apart without calling [Registry.State] separately,
// which is deliberate: a consumer of Enabled has exactly one branch to
// write, `if reg.Enabled(c) { private } else { public }`, and nothing
// about a licence's OWN failure mode belongs in that branch.
func (r *Registry) Enabled(c license.Capability) bool {
	if r == nil {
		return false
	}
	return r.installed[c] && r.grant != nil && r.grant.Has(c) && r.grant.ValidAt(r.now(), r.nbfSkew)
}

// State reports WHY c answers the way it does -- see this type's own doc
// comment for why this is diagnostics only, never a second decision
// point. Nil-safe, like [Registry.Enabled]: a nil *Registry reports
// [StateNotInstalled] for everything, matching "no module composed, so
// nothing is installed" exactly.
//
// The checks below are ordered to mirror [Registry.Enabled]'s own
// left-to-right conjunction exactly, so State(c) == StateEnabled if and
// only if Enabled(c) == true for every c and every Registry -- see
// TestState_MatchesEnabled.
func (r *Registry) State(c license.Capability) State {
	if r == nil || !r.installed[c] {
		return StateNotInstalled
	}
	if r.grant == nil {
		if r.parseErr != nil {
			return StateInvalid
		}
		return StateNotLicensed
	}
	if !r.grant.Has(c) {
		return StateNotLicensed
	}
	now := r.now()
	if now.Before(r.grant.NotBefore.Add(-r.nbfSkew)) {
		return StateNotYetValid
	}
	if !now.Before(r.grant.ExpiresAt) {
		return StateExpired
	}
	return StateEnabled
}

// ExpiresAt returns the configured grant's own expiry, or nil if there is
// no grant (no key, an unparseable key, or a nil *Registry) -- for
// display only (GET /api/capabilities' own licenseExpiresAt field), never
// consulted by [Registry.Enabled] itself
// (which re-derives validity from [license.Grant.ValidAt] on every call
// instead). Returns a pointer to a COPY of the grant's own ExpiresAt, so
// a caller mutating the returned value cannot reach this Registry's own
// state.
func (r *Registry) ExpiresAt() *time.Time {
	if r == nil || r.grant == nil {
		return nil
	}
	t := r.grant.ExpiresAt
	return &t
}
