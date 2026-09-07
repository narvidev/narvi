//go:build integration

// This file proves §31.4's own per-channel refusal contract for Linear
// specifically -- Defect-2 audit fix: before this fix, handleCreated
// (webhook.go) branched only on CreateSessionError.RolloutRefusal, so a
// repo-entitlement denial fell into the generic transient-failure branch,
// releasing BOTH claims for a Linear redelivery that would only ever
// reproduce the SAME refusal forever (github_pr_sessions does not change
// between redeliveries), with no acknowledgement ever posted. Mirrors
// rolloutgate_integration_test.go's own
// TestWebhookHandler_Created_RolloutRefusal_AcknowledgesReleasesOnlyAgentSessionClaim
// exactly, one gate earlier: a repo-entitlement denial must take the SAME
// terminal, authz-denial shape -- acknowledge, release ONLY the
// linear_agent_sessions claim, and answer 200 (webhook-delivery claim
// KEPT, never released for a redelivery retry).
package linear_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// newHandlerDepsWithoutEntitlement mirrors newHandlerDeps
// (webhook_integration_test.go) exactly, EXCEPT it deliberately never
// seeds the fixed DefaultRepoURL below into github_pr_sessions -- every
// other test in this package uses newHandlerDeps specifically so §31.4's
// entitlement gate never interferes with whatever ELSE that test is
// exercising. This helper is the deliberate exception: it exists ONLY to
// exercise the entitlement gate's own refusal, so it must NOT make the
// default repo known.
func newHandlerDepsWithoutEntitlement(t *testing.T, pool *pgxpool.Pool) linear.Deps {
	t.Helper()
	ctx := context.Background()

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	// Deliberately NO prSessions.EnsureRow call -- DefaultRepoURL below is
	// never made known, so §31.4's entitlement gate refuses every
	// session-creation attempt this deps value's own tests make.
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	return linear.Deps{
		Pool:               pool,
		Sessions:           narvipg.NewSessionStore(pool),
		Turns:              narvipg.NewTurnStore(pool),
		Environments:       narvipg.NewEnvironmentStore(pool),
		Registry:           registry,
		Deliveries:         narvipg.NewWebhookDeliveryStore(pool),
		AgentSessions:      narvipg.NewLinearAgentSessionStore(pool),
		Installations:      narvipg.NewLinearInstallationStore(pool),
		AuditLog:           narvipg.NewAuditLogStore(pool),
		PRSessions:         prSessions,
		LinearClient:       linearapi.New(nil, "http://127.0.0.1:0"), // never actually called: no installation row exists for this test's organization, so postAcknowledgment skips before any HTTP call.
		WebhookSecret:      []byte(testWebhookSecret),
		TokenEncryptionKey: bytes.Repeat([]byte("k"), 32),
		DefaultRepoName:    "narvi",
		DefaultRepoURL:     "https://github.com/narvidev/narvi",
		Timeouts:           platform.DefaultTimeouts(),
	}
}

// TestWebhookHandler_Created_RepoEntitlementDenied_AcknowledgesReleasesOnlyAgentSessionClaim
// is the MUTATION-TESTABLE guard for §31.4's own Linear refusal contract
// (Defect-2 audit fix): deps.DefaultRepoURL has no github_pr_sessions row
// at all, so httpapi.CreateSessionCore's own ResolveRepoEntitlement call
// refuses with CreateSessionError.RepoEntitlementDenied == true. Proves,
// in one HTTP round trip: (1) the response is 200, never 500; (2) the
// webhook-delivery claim is KEPT (a redelivery of the SAME Linear-Delivery
// id must be treated as an already-claimed duplicate, not reprocessed);
// (3) the linear_agent_sessions claim IS released (a later, genuinely
// different agent session for this SAME repo, once it IS entitled, must
// not be blocked by a stale claim this refused attempt left behind); (4)
// no session was ever created.
func TestWebhookHandler_Created_RepoEntitlementDenied_AcknowledgesReleasesOnlyAgentSessionClaim(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newHandlerDepsWithoutEntitlement(t, pool)

	agentSessionID := "agent-session-" + t.Name()
	organizationID := "org-" + t.Name()
	deliveryID := "delivery-" + t.Name()
	body := agentSessionCreatedPayload(agentSessionID, organizationID)

	rec := postWebhook(t, linear.NewWebhookHandler(deps), body, deliveryID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (terminal acknowledgment, mirroring the rollout-refusal branch's identical status)", rec.Code, http.StatusOK)
	}

	// The webhook-delivery claim must be KEPT -- Claim on the SAME
	// (provider, deliveryID) must report a duplicate (Inserted == false),
	// never a fresh claim, proving this delivery id was never released.
	deliveries := narvipg.NewWebhookDeliveryStore(pool)
	claim, err := deliveries.Claim(ctx, "linear", deliveryID)
	if err != nil {
		t.Fatalf("re-claim webhook delivery: %v", err)
	}
	if claim.Inserted {
		t.Error("webhook delivery claim was re-claimable (Inserted = true) -- want it to remain held after a repo-entitlement denial, exactly like a rollout refusal")
	}

	// The linear_agent_sessions claim, by contrast, MUST have been
	// released -- GetByAgentSessionID must report it gone.
	agentSessions := narvipg.NewLinearAgentSessionStore(pool)
	if _, err := agentSessions.GetByAgentSessionID(ctx, agentSessionID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("agent session claim lookup: err=%v, want pgx.ErrNoRows (the claim must have been released, mirroring the rollout-refusal branch)", err)
	}

	// No session was ever created for this repo.
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE spawn_source = 'linear'`).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("sessions count = %d, want 0 -- a repo-entitlement denial must never create a session", count)
	}
}

// TestWebhookHandler_Created_RepoEntitlement_KnownRepoStillCreatesSession
// is the refusal test's own positive control: the IDENTICAL setup, except
// deps.DefaultRepoURL IS known (github_pr_sessions has a row) -- proves
// the entitlement gate is a real, bidirectional gate on this surface too,
// not something that happens to always refuse.
func TestWebhookHandler_Created_RepoEntitlement_KnownRepoStillCreatesSession(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newHandlerDepsWithoutEntitlement(t, pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, "narvidev/narvi", 1); err != nil {
		t.Fatalf("seed github_pr_sessions entitlement: %v", err)
	}

	agentSessionID := "agent-session-" + t.Name()
	organizationID := "org-" + t.Name()
	body := agentSessionCreatedPayload(agentSessionID, organizationID)

	rec := postWebhook(t, linear.NewWebhookHandler(deps), body, "delivery-"+t.Name())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	agentSessions := narvipg.NewLinearAgentSessionStore(pool)
	row, err := agentSessions.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("GetByAgentSessionID: %v (want a real, committed session id -- a known repo must not be refused)", err)
	}
	if !row.SessionID.Valid {
		t.Error("agent session row has no session_id -- want a real created session for a known repo")
	}
}
