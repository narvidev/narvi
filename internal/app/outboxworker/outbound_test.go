package outboxworker_test

import (
	"context"
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// TestGitHubNotifierConstructors_RefuseNilOutbound proves every notifier
// in this package that acts on GitHub as the bot refuses to exist without
// GitHub outbound (§12.5): handed a nil *platform.GitHubOutboundConfig,
// each constructor returns an error wrapping
// platform.ErrGitHubOutboundRequired and no notifier. Every other argument
// is nil or zero on purpose -- the refusal comes first.
func TestGitHubNotifierConstructors_RefuseNilOutbound(t *testing.T) {
	tests := []struct {
		name  string
		build func() (ports.Notifier, error)
	}{
		{"review check", func() (ports.Notifier, error) {
			return outboxworker.NewReviewCheckNotifier(nil, nil, nil, nil)
		}},
		{"description autofix", func() (ports.Notifier, error) {
			return outboxworker.NewDescriptionAutofixNotifier(nil, nil, nil, nil, platform.DefaultTimeouts())
		}},
		{"sentinel auto-fix", func() (ports.Notifier, error) {
			return outboxworker.NewSentinelAutoFixNotifier(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
				platform.DefaultTimeouts(), false, platform.RolloutModeOpen, nil, nil,
				func(context.Context, string) bool { return true }, nil)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := tc.build()
			if !errors.Is(err, platform.ErrGitHubOutboundRequired) {
				t.Errorf("constructor(nil outbound) error = %v, want one wrapping platform.ErrGitHubOutboundRequired", err)
			}
			if n != nil {
				t.Errorf("constructor(nil outbound) notifier = %v, want nil", n)
			}
		})
	}
}
