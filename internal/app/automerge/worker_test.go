package automerge_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/app/automerge"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/platform"
)

// TestNew_RefusesNilOutbound proves the auto-merge worker cannot be built
// without GitHub outbound (§12.5): it reads pull requests and merges as
// the bot, so a nil *platform.GitHubOutboundConfig is a construction error
// wrapping platform.ErrGitHubOutboundRequired -- never a worker polling
// GitHub with an empty token until its auth guard dead-letters it. The
// decision inbox deps it revalidates through need the same credential:
// they read each candidate's base branch's required checks with it
// (§21.2), and without it every candidate would read ineligible -- a
// worker that runs and never merges.
func TestNew_RefusesNilOutbound(t *testing.T) {
	outbound := platform.MustNewGitHubOutboundConfig("tok")

	tests := []struct {
		name    string
		deps    automerge.Deps
		wantErr bool
	}{
		{
			name:    "no outbound at all",
			deps:    automerge.Deps{Timeouts: platform.DefaultTimeouts()},
			wantErr: true,
		},
		{
			name:    "the worker's outbound, but none in the decision inbox deps",
			deps:    automerge.Deps{Outbound: outbound, Timeouts: platform.DefaultTimeouts()},
			wantErr: true,
		},
		{
			name:    "the decision inbox deps' outbound, but none for the worker",
			deps:    automerge.Deps{DecisionInbox: decisioninbox.Deps{GitHubOutbound: outbound}, Timeouts: platform.DefaultTimeouts()},
			wantErr: true,
		},
		{
			name: "both",
			deps: automerge.Deps{Outbound: outbound, DecisionInbox: decisioninbox.Deps{GitHubOutbound: outbound}, Timeouts: platform.DefaultTimeouts()},
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
