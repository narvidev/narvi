// Package capability is Narvi's single point of truth for "is capability
// c usable right now" -- the app-layer half of docs/design/
// boundaries-design.md, section 1, and technical plan §34.5. It sits directly on
// top of the pure internal/domain/license package, adding exactly the one
// thing a domain package cannot hold: an injected clock.
//
// [Registry.Enabled] is the ENTIRE contract: installed AND licensed AND
// valid-now. There is no method anywhere in this package that can
// express "turn a public behavior off" -- every consumer reads
// `if reg.Enabled(c) { private implementation } else { the public one }`,
// and the else branch is wiring that already exists in the public binary
// regardless of this package. That asymmetry is deliberate and is why
// this package is one of the three the capabilityimportban analyzer
// (tools/lint/narvichecks/capabilityimportban) refuses to let most of
// this codebase import at all: a licence state must never become an
// input to a shadow-mode suppression decision (§30), and the easiest way
// to guarantee that is to make sure almost nothing can reach this
// package's own types in the first place.
//
// Capability decisions are made in exactly two kinds of place: the
// composition root (controlplane), choosing which implementation to wire
// into an app-layer seam, and the two named HTTP route-group gates in
// internal/adapters/inbound/httpapi (RequireCapability, and
// GetCapabilities, the derived read model). Nowhere else.
package capability
