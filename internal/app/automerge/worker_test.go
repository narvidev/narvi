package automerge_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/app/automerge"
	"github.com/narvidev/narvi/internal/platform"
)

// TestNew_RefusesNilOutbound proves the auto-merge worker cannot be built
// without GitHub outbound (§12.5): it reads pull requests and merges as
// the bot, so a nil *platform.GitHubOutboundConfig is a construction error
// wrapping platform.ErrGitHubOutboundRequired -- never a worker polling
// GitHub with an empty token until its auth guard dead-letters it.
func TestNew_RefusesNilOutbound(t *testing.T) {
	worker, err := automerge.New(automerge.Deps{Timeouts: platform.DefaultTimeouts()})
	if !errors.Is(err, platform.ErrGitHubOutboundRequired) {
		t.Errorf("New(nil outbound) error = %v, want one wrapping platform.ErrGitHubOutboundRequired", err)
	}
	if worker != nil {
		t.Errorf("New(nil outbound) worker = %v, want nil", worker)
	}

	worker, err = automerge.New(automerge.Deps{Outbound: platform.MustNewGitHubOutboundConfig("tok"), Timeouts: platform.DefaultTimeouts()})
	if err != nil || worker == nil {
		t.Errorf("New(outbound) = (%v, %v), want a worker and no error", worker, err)
	}
}
