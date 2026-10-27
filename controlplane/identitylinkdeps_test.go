package controlplane

import (
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
)

// TestNewAppIdentityLinkDeps pins the identity-link wiring Build shares
// between Slack, Linear and the magic-link route (§13.2): the configured
// link-prompt lifetime and email-evidence limit reach identitylink.Deps,
// with the stores they were given. An evidence limit left unset would make
// identitylink.MatchUserIDs refuse every match. No pool is opened: the
// stores are built on a nil pool and never queried.
func TestNewAppIdentityLinkDeps(t *testing.T) {
	t.Parallel()

	users := postgres.NewUserStore(nil)
	identities := postgres.NewIdentityStore(nil)
	linkPrompts := postgres.NewIdentityLinkPromptStore(nil)
	auditLog := postgres.NewAuditLogStore(nil)

	for _, tc := range []struct {
		name           string
		evidenceMaxAge time.Duration
		promptTTL      time.Duration
	}{
		{"the shipped defaults", platform.DefaultTimeouts().IdentityEmailEvidenceMaxAge, platform.DefaultTimeouts().IdentityLinkPromptTTL},
		{"configured values", 40 * 24 * time.Hour, 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &platform.Config{PublicBaseURL: "https://narvi.example.com", Timeouts: platform.DefaultTimeouts()}
			cfg.Timeouts.IdentityEmailEvidenceMaxAge = tc.evidenceMaxAge
			cfg.Timeouts.IdentityLinkPromptTTL = tc.promptTTL

			got := newAppIdentityLinkDeps(cfg, nil, users, identities, linkPrompts, auditLog)
			if got.EvidenceMaxAge != tc.evidenceMaxAge {
				t.Errorf("EvidenceMaxAge = %v, want %v", got.EvidenceMaxAge, tc.evidenceMaxAge)
			}
			if got.PromptTTL != tc.promptTTL {
				t.Errorf("PromptTTL = %v, want %v", got.PromptTTL, tc.promptTTL)
			}
			if got.PublicBaseURL != cfg.PublicBaseURL {
				t.Errorf("PublicBaseURL = %q, want %q", got.PublicBaseURL, cfg.PublicBaseURL)
			}
			if got.Users != users || got.Identities != identities || got.LinkPrompts != linkPrompts || got.AuditLog != auditLog {
				t.Errorf("stores = %p/%p/%p/%p, want the ones given (%p/%p/%p/%p)",
					got.Users, got.Identities, got.LinkPrompts, got.AuditLog, users, identities, linkPrompts, auditLog)
			}
		})
	}
}
