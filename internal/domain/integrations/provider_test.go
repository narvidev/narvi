package integrations_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/integrations"
)

// TestProviderForOutboxKind is table-driven over every NotificationKind
// this codebase defines as of this package's own introduction (grepped
// from internal/app/ports/notifier.go), plus a kind that deliberately
// breaks the "<provider>_<what>" naming convention -- proving
// ProviderForOutboxKind's own documented fragility (provider.go's own doc
// comment) rather than merely asserting it in prose.
func TestProviderForOutboxKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind   string
		want   integrations.Provider
		wantOK bool
	}{
		// Bare provider-name kinds (no underscore suffix at all) --
		// still a prefix match against themselves.
		{"slack", integrations.ProviderSlack, true},
		{"linear", integrations.ProviderLinear, true},
		{"github", integrations.ProviderGitHub, true},

		// <provider>_<what> kinds that DO follow the convention.
		{"slack_plan_approval", integrations.ProviderSlack, true},
		{"slack_plan_decided", integrations.ProviderSlack, true},
		{"slack_workflow_decision", integrations.ProviderSlack, true},
		{"slack_digest", integrations.ProviderSlack, true},
		{"linear_progress", integrations.ProviderLinear, true},
		{"linear_workflow_decision", integrations.ProviderLinear, true},
		{"linear_digest", integrations.ProviderLinear, true},
		{"github_verdict", integrations.ProviderGitHub, true},
		{"github_workflow_decision", integrations.ProviderGitHub, true},
		{"github_preview_link", integrations.ProviderGitHub, true},
		{"github_description_autofix", integrations.ProviderGitHub, true},

		// Kinds that break the convention -- excluded, never
		// mis-attributed to some other provider. Real, existing
		// NotificationKind values, not hypothetical ones -- see
		// ProviderForOutboxKind's own doc comment for why each one fails
		// this match despite (for the first three) genuinely being
		// GitHub-directed outbound calls.
		{"sentinel_auto_fix", "", false},
		{"handoff_sentinel", "", false},
		{"release_manifest", "", false},
		{"blob_delete", "", false},
		// A provider not in this read model's own 3-surface scope at
		// all (§4.1's RWX preview dispatch) -- correctly excluded, not
		// a naming-convention failure.
		{"rwx_preview_dispatch", "", false},
		// A wholly made-up future kind, proving the general case (not
		// just today's known exceptions).
		{"notify_github_something", "", false},
		{"", "", false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			got, ok := integrations.ProviderForOutboxKind(tc.kind)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("ProviderForOutboxKind(%q) = (%q, %v), want (%q, %v)", tc.kind, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestParseProvider is table-driven over the three known spellings, a
// near-miss typo of one of them, and an empty string -- proving ParseProvider
// accepts exactly the canonical three and nothing else, the property
// platform.Load leans on to turn a typo'd NARVI_INGRESS_ENABLED entry into
// a loud boot failure rather than a silently-dropped one.
func TestParseProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw    string
		want   integrations.Provider
		wantOK bool
	}{
		{"slack", integrations.ProviderSlack, true},
		{"linear", integrations.ProviderLinear, true},
		{"github", integrations.ProviderGitHub, true},
		{"slcak", "", false}, // a real near-miss typo of "slack".
		{"Slack", "", false}, // case matters -- never normalized.
		{"", "", false},
		{"slack ", "", false}, // whitespace is the caller's job to trim, not this function's.
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			got, ok := integrations.ParseProvider(tc.raw)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("ParseProvider(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestConfiguredSlack proves a partially-configured surface (missing
// EITHER required secret) reads as NOT connected -- one case per missing
// secret, per this Step's own "Tests that must exist" requirement.
func TestConfiguredSlack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		signingSecret string
		botToken      string
		want          bool
	}{
		{"both present", "sig", "tok", true},
		{"missing signing secret", "", "tok", false},
		{"missing bot token", "sig", "", false},
		{"both missing", "", "", false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := integrations.ConfiguredSlack(tc.signingSecret, tc.botToken); got != tc.want {
				t.Errorf("ConfiguredSlack(%q, %q) = %v, want %v", tc.signingSecret, tc.botToken, got, tc.want)
			}
		})
	}
}

// TestConfiguredLinear mirrors TestConfiguredSlack, one case per missing
// secret among Linear's own three.
func TestConfiguredLinear(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		webhookSecret     string
		oauthClientID     string
		oauthClientSecret string
		want              bool
	}{
		{"all three present", "wh", "id", "secret", true},
		{"missing webhook secret", "", "id", "secret", false},
		{"missing oauth client id", "wh", "", "secret", false},
		{"missing oauth client secret", "wh", "id", "", false},
		{"all missing", "", "", "", false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := integrations.ConfiguredLinear(tc.webhookSecret, tc.oauthClientID, tc.oauthClientSecret)
			if got != tc.want {
				t.Errorf("ConfiguredLinear(%q, %q, %q) = %v, want %v", tc.webhookSecret, tc.oauthClientID, tc.oauthClientSecret, got, tc.want)
			}
		})
	}
}

// TestConfiguredGitHub mirrors TestConfiguredSlack/TestConfiguredLinear,
// one case per missing secret among GitHub's own three, plus proves
// GitHubClientID/GitHubClientSecret (OAuth login, deliberately excluded)
// have no bearing here by never passing them at all.
func TestConfiguredGitHub(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		webhookSecret string
		botHandle     string
		botToken      string
		want          bool
	}{
		{"all three present", "wh", "@narvi-bot", "tok", true},
		{"missing webhook secret", "", "@narvi-bot", "tok", false},
		{"missing bot handle", "wh", "", "tok", false},
		{"missing bot token", "wh", "@narvi-bot", "", false},
		{"all missing", "", "", "", false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := integrations.ConfiguredGitHub(tc.webhookSecret, tc.botHandle, tc.botToken)
			if got != tc.want {
				t.Errorf("ConfiguredGitHub(%q, %q, %q) = %v, want %v", tc.webhookSecret, tc.botHandle, tc.botToken, got, tc.want)
			}
		})
	}
}
