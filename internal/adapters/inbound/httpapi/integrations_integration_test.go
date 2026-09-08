//go:build integration

// Integration tests for §12.5's own ("integrations read model & routes"
// amendment) GET /api/integrations (integrations.go), against a real
// Postgres instance -- gated behind the "integration" build tag, sharing
// this package's own testRig (httpapi_integration_test.go).
package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/integrations"
	"github.com/narvidev/narvi/internal/platform"
)

// allIngressEnabled is the §12.5 fixture shorthand this file's own
// partially-configured/secret-leak tests reuse: every override below is
// exercising a MISSING SECRET while the surface itself is enabled, never
// the disabled-surface case (TestGetIntegrations_DisabledSurfaceReportsFalse
// below is the one that exercises that), so each needs all three surfaces
// enabled or configuredForProvider's own IngressEnabled short-circuit would
// report false for the "wrong" reason and the test would stop proving what
// its own name says it proves.
var allIngressEnabled = map[integrations.Provider]bool{
	integrations.ProviderSlack:  true,
	integrations.ProviderLinear: true,
	integrations.ProviderGitHub: true,
}

// TestGetIntegrations_MemberDenied proves an ordinary member is denied
// (403) -- authz.ActionManageIntegrations is admin only (§13.3 row 6),
// no maintainer/member carve-out at all.
func TestGetIntegrations_MemberDenied(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t) // default role: member.

	status := rig.doJSON(t, http.MethodGet, "/api/integrations", nil, nil, token)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestGetIntegrations_MaintainerDenied proves a maintainer -- who DOES
// hold plenty of §13.3 row-5 actions -- is still refused here: unlike
// GetRepoSettings' own authorizeAny widening, GetIntegrations has exactly
// ONE gate (ActionManageIntegrations, row 6, admin only), no read-side
// relaxation at all.
func TestGetIntegrations_MaintainerDenied(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	status := rig.doJSON(t, http.MethodGet, "/api/integrations", nil, nil, token)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestGetIntegrations_AdminAllowed_AllConfigured proves the happy path:
// an admin, with this rig's own default fully-configured cfg, sees all
// three surfaces (in internal/domain/integrations.Providers' own fixed
// order: slack, linear, github) reporting configured=true and every
// inbound/outbound field null (no webhook_deliveries/outbox rows have
// been seeded for this test).
func TestGetIntegrations_AdminAllowed_AllConfigured(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var got restdtos.ListIntegrationsResponse
	status := rig.doJSON(t, http.MethodGet, "/api/integrations", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(got.Integrations) != 3 {
		t.Fatalf("len(Integrations) = %d, want 3", len(got.Integrations))
	}
	wantOrder := []restdtos.IntegrationSurface{
		restdtos.IntegrationSurfaceSlack,
		restdtos.IntegrationSurfaceLinear,
		restdtos.IntegrationSurfaceGithub,
	}
	for i, row := range got.Integrations {
		if row.Surface != wantOrder[i] {
			t.Errorf("Integrations[%d].Surface = %q, want %q", i, row.Surface, wantOrder[i])
		}
		if !row.Configured {
			t.Errorf("Integrations[%d] (%s).Configured = false, want true (rig.cfg is fully configured)", i, row.Surface)
		}
		if row.LastInboundAt != nil {
			t.Errorf("Integrations[%d] (%s).LastInboundAt = %v, want nil (no webhook_deliveries row seeded)", i, row.Surface, row.LastInboundAt)
		}
		if row.LastOutboundAt != nil || row.LastOutboundStatus != nil || row.LastOutboundError != nil {
			t.Errorf("Integrations[%d] (%s) outbound fields not all nil: at=%v status=%v err=%v", i, row.Surface, row.LastOutboundAt, row.LastOutboundStatus, row.LastOutboundError)
		}
	}
}

// TestGetIntegrations_PartiallyConfigured_AllThreeReportFalse proves the
// end-to-end wiring for the "a partially-configured surface reads as NOT
// connected" rule (§12.5) through the REAL handler -- internal/domain/
// integrations's own TestConfiguredSlack/TestConfiguredLinear/
// TestConfiguredGitHub already prove the pure predicate exhaustively
// (one case per missing secret); this proves GetIntegrations actually
// wires platform.Config's own fields into those predicates correctly,
// one missing secret per surface.
func TestGetIntegrations_PartiallyConfigured_AllThreeReportFalse(t *testing.T) {
	rig := newTestRig(t, func(r *testRig) {
		r.cfg = &platform.Config{
			IngressEnabled: allIngressEnabled,

			SlackSigningSecret: "present",
			SlackBotToken:      "", // missing -- Slack incomplete.

			LinearWebhookSecret:     "present",
			LinearOAuthClientID:     "present",
			LinearOAuthClientSecret: "", // missing -- Linear incomplete.

			GitHubWebhookSecret: "", // missing -- GitHub incomplete.
			GitHubBotHandle:     "present",
			GitHubBotToken:      "present",
		}
	})
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var got restdtos.ListIntegrationsResponse
	status := rig.doJSON(t, http.MethodGet, "/api/integrations", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	for _, row := range got.Integrations {
		if row.Configured {
			t.Errorf("Integrations (%s).Configured = true, want false (only partially configured)", row.Surface)
		}
	}
}

// TestGetIntegrations_DisabledSurfaceReportsFalse proves the OTHER half of
// §12.5's now-genuinely-two-valued "configured" field: a surface with its
// own FULL credential set present but not named in NARVI_INGRESS_ENABLED
// (platform.Config.IngressEnabled) still reads configured=false -- the
// point of this Step, since before it "every secret present" was the only
// way this field could ever read true at all. Slack here has every secret
// set (proving the false reading comes from IngressEnabled, never from a
// missing secret this test forgot to set) while Linear/GitHub stay enabled
// and fully configured, proving the other two are unaffected by Slack's
// own disabled state.
func TestGetIntegrations_DisabledSurfaceReportsFalse(t *testing.T) {
	rig := newTestRig(t, func(r *testRig) {
		r.cfg = &platform.Config{
			IngressEnabled: map[integrations.Provider]bool{
				integrations.ProviderLinear: true,
				integrations.ProviderGitHub: true,
				// Slack deliberately absent -- not enabled on this
				// deployment, despite every secret below being present.
			},

			SlackSigningSecret: "present-but-disabled",
			SlackBotToken:      "present-but-disabled",

			LinearWebhookSecret:     "present",
			LinearOAuthClientID:     "present",
			LinearOAuthClientSecret: "present",

			GitHubWebhookSecret: "present",
			GitHubBotHandle:     "present",
			GitHubBotToken:      "present",
		}
	})
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var got restdtos.ListIntegrationsResponse
	status := rig.doJSON(t, http.MethodGet, "/api/integrations", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	for _, row := range got.Integrations {
		switch row.Surface {
		case restdtos.IntegrationSurfaceSlack:
			if row.Configured {
				t.Errorf("Integrations[slack].Configured = true, want false (not in IngressEnabled, despite every secret being present)")
			}
		case restdtos.IntegrationSurfaceLinear, restdtos.IntegrationSurfaceGithub:
			if !row.Configured {
				t.Errorf("Integrations[%s].Configured = false, want true (enabled and fully configured)", row.Surface)
			}
		}
	}
}

// TestGetIntegrations_NoSecretsInRawResponse asserts on the RAW response
// bytes -- a decoded-struct assertion could not tell a leaked value from an
// absent one the way a raw scan can.
//
// What it proves, precisely: no configured secret appears in full, and no
// PREFIX of one appears either (every prefix from 4 characters up is
// searched, which is what would leak a token's recognisable leading segment).
// It does NOT prove the absence of a length signal or of some other derived
// form -- a response cannot be scanned for a fact it does not spell out, and
// claiming otherwise would be the exact defect this codebase names most.
// What actually rules those out is that this handler never reads a secret's
// value at all: internal/domain/integrations.ConfiguredX takes the loaded
// config and returns a bool, and the bool is the only thing that reaches the
// DTO. The scan below is defence in depth against a future edit breaking
// that, not the guarantee itself.
func TestGetIntegrations_NoSecretsInRawResponse(t *testing.T) {
	// Opaque on purpose: a fixture secret that embeds its own surface name
	// ("github-bot-token-...") shares a prefix with legitimate response
	// content (the surface field is literally "github"), so a prefix scan
	// would fire on the surface name rather than on a leak. Secrets that
	// share no substring with anything the response may legitimately
	// contain are what make the scan below mean something.
	distinctiveSecrets := []string{
		"zq4f9a2c-signing-Wv71",
		"zq7b1e88-bottok-Wv72",
		"zq9d3f01-hookies-Wv73",
		"zq2a77bc-clid-Wv74",
		"zqe05f44-clsec-Wv75",
		"zqc81a90-hooksec-Wv76",
		"zq33dd12-bottok2-Wv77",
	}
	rig := newTestRig(t, func(r *testRig) {
		r.cfg = &platform.Config{
			IngressEnabled: allIngressEnabled,

			SlackSigningSecret:      distinctiveSecrets[0],
			SlackBotToken:           distinctiveSecrets[1],
			LinearWebhookSecret:     distinctiveSecrets[2],
			LinearOAuthClientID:     distinctiveSecrets[3],
			LinearOAuthClientSecret: distinctiveSecrets[4],
			GitHubWebhookSecret:     distinctiveSecrets[5],
			GitHubBotHandle:         "narvi-test-bot",
			GitHubBotToken:          distinctiveSecrets[6],
		}
	})
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	raw := rig.doRaw(t, http.MethodGet, "/api/integrations", nil, token)

	for _, secret := range distinctiveSecrets {
		// Every prefix from 4 characters up, not just the whole value: a
		// leak that truncates is still a leak, and the earlier version of
		// this test claimed to cover prefixes while only searching for
		// full values.
		for n := 4; n <= len(secret); n++ {
			if bytes.Contains(raw, []byte(secret[:n])) {
				t.Errorf("raw response body contains a %d-character prefix of the configured secret %q -- no part of a secret may appear: %s", n, secret, raw)
				break
			}
		}
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("raw response body contains a configured secret value %q -- must NEVER appear, not even shaped: %s", secret, raw)
		}
	}
}

// TestGetIntegrations_InboundOutboundIndependent proves inbound and
// outbound are independent facts: a surface (GitHub) with a recent
// inbound delivery AND a failed outbound attempt reports BOTH, and
// nothing in the response collapses them into a single verdict --
// per this Step's own "Tests that must exist" requirement. Also proves
// the outbox->provider prefix helper end to end against a REAL row (a
// genuine "github_verdict" kind).
func TestGetIntegrations_InboundOutboundIndependent(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	// Seed a real inbound delivery for GitHub.
	if _, err := rig.webhookDeliveries.Claim(ctx, "github", "delivery-1"); err != nil {
		t.Fatalf("seed webhook delivery: %v", err)
	}

	// Seed a real, FAILED outbound outbox row for GitHub
	// (kind="github_verdict", status left at its own 'pending' DEFAULT --
	// this table has no third in-flight status, migrations/
	// 000010_outbox.up.sql's own doc comment, so a row that has been
	// attempted-and-retried still reads 'pending' until it either
	// delivers or dead-letters; RecordFailure below moves it forward one
	// attempt with a real last_error, mirroring outboxworker.Builder's own
	// real attempt() sequence).
	entry, err := rig.outbox.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		Kind:    "github_verdict",
		Payload: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create outbox entry: %v", err)
	}
	if _, err := rig.outbox.RecordFailure(ctx, entry.ID, pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}, "posting to github failed: 503"); err != nil {
		t.Fatalf("record outbox failure: %v", err)
	}

	var got restdtos.ListIntegrationsResponse
	status := rig.doJSON(t, http.MethodGet, "/api/integrations", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	var github *restdtos.Integration
	for i := range got.Integrations {
		if got.Integrations[i].Surface == restdtos.IntegrationSurfaceGithub {
			github = &got.Integrations[i]
		}
	}
	if github == nil {
		t.Fatal("no github row in response")
	}

	if github.LastInboundAt == nil {
		t.Error("LastInboundAt = nil, want a real timestamp (a webhook delivery was seeded)")
	}
	if github.LastOutboundAt == nil {
		t.Fatal("LastOutboundAt = nil, want a real timestamp (an outbox row was seeded)")
	}
	if github.LastOutboundStatus == nil || *github.LastOutboundStatus != "pending" {
		t.Errorf("LastOutboundStatus = %v, want \"pending\" (RecordFailure leaves the row pending -- still eligible for retry, not yet dead-lettered)", github.LastOutboundStatus)
	}
	if github.LastOutboundError == nil || *github.LastOutboundError != "posting to github failed: 503" {
		t.Errorf("LastOutboundError = %v, want the recorded failure message", github.LastOutboundError)
	}
	// The two timestamps are independent facts from independent tables --
	// this response carries no derived "healthy"/"degraded" field at all
	// (restdtos.Integration's own schema, contracts/rest/v1/
	// dtos.schema.json) for either to feed into, which is itself the
	// proof that nothing here labels this surface healthy despite the
	// failed outbound attempt.
}

// TestGetIntegrations_OutboxPrefixFragility_ExcludesNonConformingKind
// proves the outbox->provider prefix helper's own documented fragility
// (internal/domain/integrations.ProviderForOutboxKind) against a REAL
// database row: a "sentinel_auto_fix" kind is a genuine GitHub-directed
// outbound call in this codebase, but does not literally begin with
// "github" -- it must be EXCLUDED from the github row here, never
// mis-attributed to some other surface and never silently counted as
// github's own last outbound activity, per this Step's own "excluded
// rather than mis-attributed" requirement.
func TestGetIntegrations_OutboxPrefixFragility_ExcludesNonConformingKind(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	// Only a NON-conforming kind exists in outbox -- no "github_*" row at
	// all -- so github's own lastOutboundAt must stay nil, proving the
	// prefix match genuinely excludes it rather than accidentally still
	// matching (e.g. via a naive substring search instead of a real
	// prefix check).
	if _, err := rig.outbox.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		Kind:    "sentinel_auto_fix",
		Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("create outbox entry: %v", err)
	}

	var got restdtos.ListIntegrationsResponse
	status := rig.doJSON(t, http.MethodGet, "/api/integrations", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	for _, row := range got.Integrations {
		if row.LastOutboundAt != nil {
			t.Errorf("Integrations (%s).LastOutboundAt = %v, want nil -- the only outbox row present (\"sentinel_auto_fix\") must not attribute to ANY surface by prefix", row.Surface, row.LastOutboundAt)
		}
	}
}

// TestGetIntegrations_DeliveredRowReportsNoStaleError pins the one place this
// read model could turn a fact into a wrong impression.
//
// MarkOutboxDelivered sets status and delivered_at and does NOT clear
// last_error (queries/outbox.sql), so a row that failed once and succeeded on
// retry still carries the old message. Reporting it verbatim renders a
// DELIVERED surface with an error beside it — the same fact-versus-verdict
// confusion §12.5 forbids, one field down. On a delivered row the error is
// history, not state.
func TestGetIntegrations_DeliveredRowReportsNoStaleError(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	session := rig.createSession(ctx, t)
	// A row that failed, then succeeded: status delivered, last_error still set.
	if _, err := rig.pool.Exec(ctx, `
		INSERT INTO outbox (id, session_id, kind, payload, status, attempts, last_error, delivered_at, created_at)
		VALUES (gen_random_uuid(), $1, 'slack_digest', '{}'::jsonb, 'delivered', 2, 'channel_not_found: transient', now(), now())`,
		session.ID); err != nil {
		t.Fatalf("insert delivered-with-stale-error outbox row: %v", err)
	}

	var resp restdtos.ListIntegrationsResponse
	if status := rig.doJSON(t, http.MethodGet, "/api/integrations", nil, &resp, token); status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	var slack *restdtos.Integration
	for i := range resp.Integrations {
		if resp.Integrations[i].Surface == restdtos.IntegrationSurfaceSlack {
			slack = &resp.Integrations[i]
		}
	}
	if slack == nil {
		t.Fatal("no slack row in the response")
	}
	if slack.LastOutboundStatus == nil || string(*slack.LastOutboundStatus) != "delivered" {
		t.Fatalf("LastOutboundStatus = %v, want delivered (the fixture must actually be the delivered case)", slack.LastOutboundStatus)
	}
	if slack.LastOutboundError != nil {
		t.Errorf("LastOutboundError = %q on a DELIVERED row, want nil -- last_error is history there, not state", *slack.LastOutboundError)
	}
}
