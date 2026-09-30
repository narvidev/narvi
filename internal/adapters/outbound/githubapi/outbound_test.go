package githubapi_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/platform"
)

// mustBuild unwraps a notifier constructor's (value, error) pair for tests
// that always hand it a real GitHub outbound config, so the error can only
// be a test bug.
func mustBuild[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestNotifierConstructors_RefuseNilOutbound proves every notifier in this
// package that posts as the bot refuses to exist without GitHub outbound
// (§12.5): handed a nil *platform.GitHubOutboundConfig, each constructor
// returns an error wrapping platform.ErrGitHubOutboundRequired and no
// notifier -- so a deployment with the axis off cannot register one by
// accident, and no call can ever go out with an empty bearer token.
func TestNotifierConstructors_RefuseNilOutbound(t *testing.T) {
	adapter := githubapi.New(nil, "http://127.0.0.1:1")
	tests := []struct {
		name  string
		build func(*platform.GitHubOutboundConfig) (any, error)
	}{
		{"BotNotifier", func(o *platform.GitHubOutboundConfig) (any, error) { return githubapi.NewBotNotifier(adapter, o) }},
		{"VerdictNotifier", func(o *platform.GitHubOutboundConfig) (any, error) { return githubapi.NewVerdictNotifier(adapter, o) }},
		{"HandoffNotifier", func(o *platform.GitHubOutboundConfig) (any, error) { return githubapi.NewHandoffNotifier(adapter, o) }},
		{"ReleaseManifestNotifier", func(o *platform.GitHubOutboundConfig) (any, error) {
			return githubapi.NewReleaseManifestNotifier(adapter, o)
		}},
		{"PreviewLinkNotifier", func(o *platform.GitHubOutboundConfig) (any, error) {
			return githubapi.NewPreviewLinkNotifier(adapter, o)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.build(nil); !errors.Is(err, platform.ErrGitHubOutboundRequired) {
				t.Errorf("New%s(nil outbound) error = %v, want one wrapping platform.ErrGitHubOutboundRequired", tc.name, err)
			}
			got, err := tc.build(platform.MustNewGitHubOutboundConfig("tok"))
			if err != nil || got == nil {
				t.Errorf("New%s(outbound) = (%v, %v), want a notifier and no error", tc.name, got, err)
			}
		})
	}
}
