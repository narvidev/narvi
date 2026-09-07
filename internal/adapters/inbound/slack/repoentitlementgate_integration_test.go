//go:build integration

// This file proves §31.4's own per-channel refusal contract for Slack
// specifically -- Defect-2 audit fix: before this fix, resolveOrClaimSession
// (handler.go) branched only on CreateSessionError.RolloutRefusal, so a
// repo-entitlement denial fell into the generic transient-failure branch,
// releasing the webhook-delivery claim for a Slack retry that would only
// ever reproduce the SAME refusal forever (github_pr_sessions does not
// change between redeliveries), with no acknowledgement ever posted.
// Mirrors rolloutgate_integration_test.go's own
// TestHandler_NewMention_RolloutRefusal_PostsHonestAckAndKeepsClaims
// exactly, one gate earlier: a repo-entitlement denial must take the SAME
// terminal in-thread ack idiom -- post ackRepoNotEntitledText, return
// {Skip: true}, true, and answer 200 with BOTH webhook-delivery claims
// (the outer "slack" provider claim AND the inner "slack-message" one)
// KEPT, never released for a redelivery retry.
package slack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/slack"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// newSlackAckTestRigWithoutEntitlement mirrors newSlackAckTestRig
// (turn_integration_test.go) exactly, EXCEPT it deliberately never seeds
// the rig's own fixed default repo ("https://github.com/narvidev/narvi")
// into github_pr_sessions -- every other rig in this package's own test
// suite seeds that row specifically so §31.4's entitlement gate never
// interferes with whatever ELSE that rig's own tests are exercising (see
// e.g. newSlackAckTestRigWithRolloutMode's own doc comment, immediately
// above in rolloutgate_integration_test.go). This rig is the deliberate
// exception: it exists ONLY to exercise the entitlement gate's own
// refusal, so it must NOT make the default repo known.
func newSlackAckTestRigWithoutEntitlement(t *testing.T, pool *pgxpool.Pool) *slackAckTestRig {
	t.Helper()
	ctx := context.Background()

	linkSlackIdentityForTest(ctx, t, pool, "U0TESTUSER", sqlcgen.UserRoleMaintainer)
	linkSlackIdentityForTest(ctx, t, pool, "U0OTHERUSER", sqlcgen.UserRoleMaintainer)

	requests := make(chan recordedSlackRequestBody, 16)
	ackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests <- recordedSlackRequestBody{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(ackServer.Close)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	deliveries := narvipg.NewWebhookDeliveryStore(pool)
	threads := narvipg.NewSlackThreadSessionStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	// Deliberately NO prSessions.EnsureRow call -- the rig's own fixed
	// default repo is never made known, so §31.4's entitlement gate
	// refuses every session-creation attempt this rig's own tests make.
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	handler := slack.NewHandler(slack.Deps{
		Pool:            pool,
		Sessions:        sessions,
		Turns:           turns,
		Environments:    environments,
		Registry:        registry,
		Deliveries:      deliveries,
		Threads:         threads,
		AuditLog:        auditLog,
		Participants:    narvipg.NewParticipantStore(pool),
		PRSessions:      prSessions,
		SigningSecret:   testSigningSecret,
		DefaultRepoName: "narvi",
		DefaultRepoURL:  "https://github.com/narvidev/narvi",
		TimestampWindow: 5 * time.Minute,
		AckTimeout:      platform.DefaultTimeouts().SlackAckTimeout,
		SlackClient:     slackapi.New(ackServer.Client(), ackServer.URL, "test-bot-token"),
		Timeouts:        platform.DefaultTimeouts(),
		RolloutMode:     platform.RolloutModeOpen,
		IdentityLink: identitylink.Deps{
			Pool:          pool,
			Users:         narvipg.NewUserStore(pool),
			Identities:    narvipg.NewIdentityStore(pool),
			LinkPrompts:   narvipg.NewIdentityLinkPromptStore(pool),
			AuditLog:      auditLog,
			PublicBaseURL: "https://narvi.example.com",
			PromptTTL:     time.Hour,
		},
	})

	return &slackAckTestRig{
		handler: handler, pool: pool, sessions: sessions, turns: turns, threads: threads, auditLog: auditLog,
		requests: requests,
	}
}

// TestHandler_NewMention_RepoEntitlementDenied_PostsHonestAckAndKeepsClaims
// is the MUTATION-TESTABLE guard for §31.4's own Slack refusal contract
// (Defect-2 audit fix): rollout mode is Open (a byte-for-byte no-op, so
// this refusal is demonstrably an ENTITLEMENT one, never a rollout one)
// and the default repo has no github_pr_sessions row at all, so
// httpapi.CreateSessionCore's own ResolveRepoEntitlement call refuses
// with CreateSessionError.RepoEntitlementDenied == true. Proves: (1)
// status 200, never 500; (2) an honest "not configured" in-thread ack was
// posted; (3) no session/thread-mapping was ever created; (4) BOTH the
// outer ("slack") and inner ("slack-message") webhook-delivery claims
// were kept, never released -- mirroring
// TestHandler_NewMention_RolloutRefusal_PostsHonestAckAndKeepsClaims's own
// identical shape (rolloutgate_integration_test.go) exactly, one gate
// earlier.
func TestHandler_NewMention_RepoEntitlementDenied_PostsHonestAckAndKeepsClaims(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackAckTestRigWithoutEntitlement(t, pool)

	const channel = "C0ENTITLEMENT"
	const ts = "1700000050.000100"
	eventID := "Ev0ENTITLEMENT001"
	envelope := appMentionEnvelope(eventID, channel, ts, "", "<@U0BOT> please fix the build")

	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	texts := rig.drainAckTexts(t)
	var gotNotConfiguredAck bool
	for _, text := range texts {
		if strings.Contains(text, "not configured") {
			gotNotConfiguredAck = true
		}
	}
	if !gotNotConfiguredAck {
		t.Errorf("posted ack texts = %v, want one containing \"not configured\"", texts)
	}

	if _, err := rig.threads.Get(ctx, channel, ts); err == nil {
		t.Error("thread mapping was created -- want none: a repo-entitlement denial must never create a session")
	}
	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0", sessionCount)
	}

	// The outer "slack" provider claim (keyed on the event_id) must be
	// held.
	var outerClaimCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'slack' AND delivery_id = $1`, eventID,
	).Scan(&outerClaimCount); err != nil {
		t.Fatalf("count outer claim rows: %v", err)
	}
	if outerClaimCount != 1 {
		t.Errorf("outer (event_id) claim row count = %d, want 1 (must stay held after a repo-entitlement denial, mirroring the rollout-refusal branch)", outerClaimCount)
	}

	// The inner "slack-message" claim (keyed on channel:ts) must ALSO be
	// held.
	var innerClaimCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'slack-message' AND delivery_id = $1`, channel+":"+ts,
	).Scan(&innerClaimCount); err != nil {
		t.Fatalf("count inner claim rows: %v", err)
	}
	if innerClaimCount != 1 {
		t.Errorf("inner (channel:ts) claim row count = %d, want 1 (must stay held)", innerClaimCount)
	}
}

// TestHandler_NewMention_RepoEntitlement_KnownRepoStillCreatesSession is
// the refusal test's own positive control: the IDENTICAL setup, except
// the default repo IS known (github_pr_sessions has a row) -- proves the
// entitlement gate is a real, bidirectional gate on this surface too, not
// a refuse-everything regression.
func TestHandler_NewMention_RepoEntitlement_KnownRepoStillCreatesSession(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackAckTestRigWithoutEntitlement(t, pool)

	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, "narvidev/narvi", 1); err != nil {
		t.Fatalf("seed github_pr_sessions entitlement: %v", err)
	}

	envelope := appMentionEnvelope("Ev0ENTITLEMENT002", "C0ENTITLEMENT2", "1700000051.000100", "", "<@U0BOT> please fix the build")

	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	if _, err := rig.threads.Get(ctx, "C0ENTITLEMENT2", "1700000051.000100"); err != nil {
		t.Errorf("Get thread mapping: %v, want a real mapping -- a known repo must not be refused", err)
	}
}
