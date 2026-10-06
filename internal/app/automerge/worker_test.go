package automerge_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/app/automerge"
	"github.com/narvidev/narvi/internal/app/autonomy"
	"github.com/narvidev/narvi/internal/platform"
)

// newGate builds an autonomy gate for construction-only tests: no pool,
// since nothing here reads the freeze.
func newGate(t *testing.T) *autonomy.Gate {
	t.Helper()
	gate, err := autonomy.NewGate(nil)
	if err != nil {
		t.Fatalf("autonomy.NewGate: %v", err)
	}
	return gate
}

// TestNew_RefusesNilAutonomyGate proves no auto-merge worker can be built
// that merges without consulting the autonomy freeze (technical plan
// §40.2): a nil gate is a construction error, never a worker that merges
// while frozen.
func TestNew_RefusesNilAutonomyGate(t *testing.T) {
	worker, err := automerge.New(automerge.Deps{Outbound: platform.MustNewGitHubOutboundConfig("tok"), Timeouts: platform.DefaultTimeouts()})
	if err == nil || !strings.Contains(err.Error(), "autonomy gate") {
		t.Fatalf("New() with no autonomy gate = (%v, %v), want a refusal naming the gate", worker, err)
	}
	if worker != nil {
		t.Errorf("New() worker = %v, want nil", worker)
	}
}

// TestNew_RefusesNilOutbound proves the auto-merge worker cannot be built
// without GitHub outbound (§12.5): it reads pull requests -- and their base
// branches' required checks (§21.2) -- and merges as the bot, so a nil
// *platform.GitHubOutboundConfig is a construction error wrapping
// platform.ErrGitHubOutboundRequired -- never a worker polling GitHub with
// an empty token until its auth guard dead-letters it.
func TestNew_RefusesNilOutbound(t *testing.T) {
	gate := newGate(t)
	tests := []struct {
		name    string
		deps    automerge.Deps
		wantErr bool
	}{
		{
			name:    "no outbound",
			deps:    automerge.Deps{Timeouts: platform.DefaultTimeouts(), Autonomy: gate},
			wantErr: true,
		},
		{
			name: "outbound",
			deps: automerge.Deps{Outbound: platform.MustNewGitHubOutboundConfig("tok"), Timeouts: platform.DefaultTimeouts(), Autonomy: gate},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			worker, err := automerge.New(tc.deps)
			if tc.wantErr {
				if !errors.Is(err, platform.ErrGitHubOutboundRequired) {
					t.Errorf("New() error = %v, want one wrapping platform.ErrGitHubOutboundRequired", err)
				}
				if worker != nil {
					t.Errorf("New() worker = %v, want nil", worker)
				}
				return
			}
			if err != nil || worker == nil {
				t.Errorf("New() = (%v, %v), want a worker and no error", worker, err)
			}
		})
	}
}
