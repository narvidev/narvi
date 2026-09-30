package automerge_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/app/automerge"
	"github.com/narvidev/narvi/internal/platform"
)

// TestNew_RefusesNilOutbound proves the auto-merge worker cannot be built
// without GitHub outbound (§12.5): it reads pull requests -- and their base
// branches' required checks (§21.2) -- and merges as the bot, so a nil
// *platform.GitHubOutboundConfig is a construction error wrapping
// platform.ErrGitHubOutboundRequired -- never a worker polling GitHub with
// an empty token until its auth guard dead-letters it.
func TestNew_RefusesNilOutbound(t *testing.T) {
	tests := []struct {
		name    string
		deps    automerge.Deps
		wantErr bool
	}{
		{
			name:    "no outbound",
			deps:    automerge.Deps{Timeouts: platform.DefaultTimeouts()},
			wantErr: true,
		},
		{
			name: "outbound",
			deps: automerge.Deps{Outbound: platform.MustNewGitHubOutboundConfig("tok"), Timeouts: platform.DefaultTimeouts()},
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
