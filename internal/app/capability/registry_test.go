package capability_test

import (
	"errors"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/app/capability"
	"github.com/narvidev/narvi/internal/domain/license"
)

// window is a NotBefore/ExpiresAt pair relative to a fixed reference
// instant, used to build grants in each of the eight scenarios below.
type window struct{ nbf, exp time.Time }

// buildScenarios returns the eight Registry states docs/design/
// boundaries-design.md, section 1.6's own TestEnabled_Matrix names, keyed by
// name, all sharing nowFunc/skew=0. Every scenario installs (or omits)
// and grants (or omits) ONLY [license.CapabilityOrganizationGovernance] --
// never [license.CapabilityCompliance] or
// [license.CapabilityKnowledgeRetrieval] -- so across the full 8-scenario
// x 3-capability matrix exactly one cell (scenario "both", capability
// organization governance) is ever expected to be enabled.
func buildScenarios(now time.Time) map[string]*capability.Registry {
	nowFunc := func() time.Time { return now }
	governance := license.CapabilityOrganizationGovernance

	valid := window{now.Add(-time.Hour), now.Add(time.Hour)}
	expired := window{now.Add(-2 * time.Hour), now.Add(-time.Hour)}
	notYetValid := window{now.Add(time.Hour), now.Add(2 * time.Hour)}

	return map[string]*capability.Registry{
		"nil registry": nil,

		"no grant": capability.New(
			[]license.Capability{governance}, nil, nil, nowFunc, 0),

		"invalid": capability.New(
			[]license.Capability{governance}, nil, errors.New("malformed key"), nowFunc, 0),

		"expired": capability.New(
			[]license.Capability{governance},
			&license.Grant{Capabilities: []license.Capability{governance}, NotBefore: expired.nbf, ExpiresAt: expired.exp},
			nil, nowFunc, 0),

		"not-yet-valid": capability.New(
			[]license.Capability{governance},
			&license.Grant{Capabilities: []license.Capability{governance}, NotBefore: notYetValid.nbf, ExpiresAt: notYetValid.exp},
			nil, nowFunc, 0),

		"granted-not-installed": capability.New(
			nil,
			&license.Grant{Capabilities: []license.Capability{governance}, NotBefore: valid.nbf, ExpiresAt: valid.exp},
			nil, nowFunc, 0),

		"installed-not-granted": capability.New(
			[]license.Capability{governance},
			&license.Grant{Capabilities: nil, NotBefore: valid.nbf, ExpiresAt: valid.exp},
			nil, nowFunc, 0),

		"both": capability.New(
			[]license.Capability{governance},
			&license.Grant{Capabilities: []license.Capability{governance}, NotBefore: valid.nbf, ExpiresAt: valid.exp},
			nil, nowFunc, 0),
	}
}

// TestEnabled_Matrix is docs/design/boundaries-design.md, section 1.6's
// own named test: eight Registry states x license.All, exactly one cell true.
func TestEnabled_Matrix(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	scenarios := buildScenarios(now)

	trueCells := 0
	for name, reg := range scenarios {
		for _, c := range license.All {
			got := reg.Enabled(c)
			want := name == "both" && c == license.CapabilityOrganizationGovernance
			if got != want {
				t.Errorf("scenario %q, capability %q: Enabled() = %v, want %v", name, c, got, want)
			}
			if got {
				trueCells++
			}
		}
	}
	if trueCells != 1 {
		t.Errorf("exactly one (scenario, capability) cell should be enabled, got %d", trueCells)
	}
}

// TestState_MatchesEnabled proves State(c) == StateEnabled if and only if
// Enabled(c) == true, for every scenario/capability pair -- and pins the
// specific, more informative State each non-enabled scenario reports, so
// this test would fail if State's own checks were reordered in a way that
// broke that correspondence (e.g. checking the grant before the installed
// set).
func TestState_MatchesEnabled(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	scenarios := buildScenarios(now)

	wantState := map[string]capability.State{
		"nil registry":          capability.StateNotInstalled,
		"no grant":              capability.StateNotLicensed,
		"invalid":               capability.StateInvalid,
		"expired":               capability.StateExpired,
		"not-yet-valid":         capability.StateNotYetValid,
		"granted-not-installed": capability.StateNotInstalled,
		"installed-not-granted": capability.StateNotLicensed,
		"both":                  capability.StateEnabled,
	}

	for name, reg := range scenarios {
		for _, c := range license.All {
			enabled := reg.Enabled(c)
			state := reg.State(c)

			if (state == capability.StateEnabled) != enabled {
				t.Errorf("scenario %q, capability %q: State() = %q, Enabled() = %v -- these must agree", name, c, state, enabled)
			}

			// wantState above is keyed by scenario alone: every scenario
			// was built naming ONLY CapabilityOrganizationGovernance, so
			// every OTHER capability (CapabilityCompliance,
			// CapabilityKnowledgeRetrieval -- and any future addition to
			// license.All) is "not installed" in every one of them (never
			// granted, never installed) except where the scenario itself
			// has nothing installed at all.
			want := wantState[name]
			if c != license.CapabilityOrganizationGovernance {
				want = capability.StateNotInstalled
			}
			if state != want {
				t.Errorf("scenario %q, capability %q: State() = %q, want %q", name, c, state, want)
			}
		}
	}
}

// TestEnabled_ExpiryMidProcess proves Enabled re-derives validity from
// the injected clock on EVERY call, never caching a prior verdict -- the
// property that lets a licence expire mid-process and take effect at the
// very next call, with no restart (technical plan §34.5).
func TestEnabled_ExpiryMidProcess(t *testing.T) {
	current := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nowFunc := func() time.Time { return current }
	governance := license.CapabilityOrganizationGovernance

	grant := &license.Grant{
		Capabilities: []license.Capability{governance},
		NotBefore:    current.Add(-time.Hour),
		ExpiresAt:    current.Add(time.Hour),
	}
	reg := capability.New([]license.Capability{governance}, grant, nil, nowFunc, 0)

	if !reg.Enabled(governance) {
		t.Fatal("Enabled() = false before expiry, want true")
	}

	current = current.Add(2 * time.Hour) // same *Registry, same *Grant -- only the clock moved

	if reg.Enabled(governance) {
		t.Fatal("Enabled() = true after the grant's own ExpiresAt with no restart, want false -- Enabled must re-check ValidAt on every call, not cache its first answer")
	}
}

// TestRegistry_NilSafe proves every exported method tolerates a nil
// *Registry -- the shape a caller gets without ever touching [New],
// mirroring the public binary's own "no module composed" behavior.
func TestRegistry_NilSafe(t *testing.T) {
	var reg *capability.Registry

	for _, c := range license.All {
		if reg.Enabled(c) {
			t.Errorf("nil Registry: Enabled(%q) = true, want false", c)
		}
		if got := reg.State(c); got != capability.StateNotInstalled {
			t.Errorf("nil Registry: State(%q) = %q, want %q", c, got, capability.StateNotInstalled)
		}
	}
	if got := reg.ExpiresAt(); got != nil {
		t.Errorf("nil Registry: ExpiresAt() = %v, want nil", got)
	}
}

// TestRegistry_ExpiresAt covers the no-grant/nil-registry/with-grant
// cases, and proves the returned pointer is a copy: mutating it must
// never reach the Registry's own state.
func TestRegistry_ExpiresAt(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nowFunc := func() time.Time { return now }
	want := now.Add(30 * 24 * time.Hour)

	reg := capability.New(nil, &license.Grant{ExpiresAt: want}, nil, nowFunc, 0)

	got := reg.ExpiresAt()
	if got == nil || !got.Equal(want) {
		t.Fatalf("ExpiresAt() = %v, want %v", got, want)
	}

	*got = time.Time{} // mutate the returned pointer

	again := reg.ExpiresAt()
	if again == nil || !again.Equal(want) {
		t.Fatalf("ExpiresAt() after mutating a previously-returned pointer = %v, want unchanged %v -- ExpiresAt must return a copy", again, want)
	}
}
