//go:build integration

// This file proves §10's own per-channel refusal contract (§10 Phase
// 6, §32) for Slack specifically: a rollout refusal must take the SAME
// terminal in-thread ack idiom ackNotAuthorizedText's own branch already
// uses (resolveOrClaimSession, handler.go) -- post ackNotEnrolledText,
// return {Skip: true}, true, and answer 200 with BOTH webhook-delivery
// claims (the outer "slack" provider claim AND the inner "slack-message"
// one) KEPT, never released for a redelivery retry that would only
// reproduce this exact same refusal forever. Mirrors turn_integration_
// test.go's own slackAckTestRig/drainAckTexts and dualevent_integration_
// test.go's own "no default repo" claim-row-count precedent exactly, one
// Skip reason further.
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

// newSlackAckTestRigWithRolloutMode mirrors newSlackAckTestRig
// (turn_integration_test.go) exactly, with rolloutMode/repoSettings
// additionally set on slack.Deps -- every ORIGINAL newSlackAckTestRig
// caller stays on rollout.ModeOpen (Deps' own zero value), proven not to
// change any of their behavior by this package's own pre-existing tests
// continuing to pass unmodified.
func newSlackAckTestRigWithRolloutMode(t *testing.T, pool *pgxpool.Pool, mode platform.RolloutMode, repoSettings *narvipg.RepoSettingsStore) *slackAckTestRig {
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

	// prSessions (§31.4): this rig's own entitlement gate runs BEFORE the
	// rollout gate mode/repoSettings above exist to exercise -- see
	// newSlackTestRigWithEpistemicCheckDefault's own identical addition
	// (handler_integration_test.go). Entitlement and rollout enrollment are
	// orthogonal: this repo is made KNOWN here regardless of mode, so
	// TestHandler_NewMention_RolloutRefusal_PostsHonestAckAndKeepsClaims's
	// own refusal is demonstrably a ROLLOUT refusal, never an entitlement
	// one reaching the same observable (ack) shape for the wrong reason.
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, "narvidev/narvi", 1); err != nil {
		t.Fatalf("seed github_pr_sessions entitlement: %v", err)
	}

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
		RolloutMode:     mode,
		RepoSettings:    repoSettings,
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

// TestHandler_NewMention_RolloutRefusal_PostsHonestAckAndKeepsClaims is
// the MUTATION-TESTABLE guard for §32's own Slack refusal contract:
// rollout mode is armed to cohort and the default repo is NEVER enrolled,
// so httpapi.CreateSessionCore's own gate refuses with
// CreateSessionError.RolloutRefusal == true. Proves: (1) status 200,
// never 500 (mirrors ackNotAuthorizedText's own identical status); (2) an
// honest "not yet enrolled" in-thread ack was posted; (3) no
// session/thread-mapping was ever created; (4) BOTH the outer ("slack")
// and inner ("slack-message") webhook-delivery claims were kept, never
// released -- mirroring TestHandler_DualDelivery_NoDefaultRepoSkip_
// ClaimNotReleased's own identical "deliberate business skip" claim-row
// assertion (dualevent_integration_test.go).
func TestHandler_NewMention_RolloutRefusal_PostsHonestAckAndKeepsClaims(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	// The rig's own fixed default repo ("https://github.com/narvidev/narvi")
	// is deliberately left unenrolled -- no UpsertSessionsEnabled call.
	rig := newSlackAckTestRigWithRolloutMode(t, pool, platform.RolloutModeCohort, repoSettings)

	const channel = "C0ROLLOUT"
	const ts = "1700000040.000100"
	eventID := "Ev0ROLLOUT001"
	envelope := appMentionEnvelope(eventID, channel, ts, "", "<@U0BOT> please fix the build")

	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	texts := rig.drainAckTexts(t)
	var gotNotEnrolledAck bool
	for _, text := range texts {
		if strings.Contains(text, "not yet enrolled") {
			gotNotEnrolledAck = true
		}
	}
	if !gotNotEnrolledAck {
		t.Errorf("posted ack texts = %v, want one containing \"not yet enrolled\"", texts)
	}

	if _, err := rig.threads.Get(ctx, channel, ts); err == nil {
		t.Error("thread mapping was created -- want none: a rollout refusal must never create a session")
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
		t.Errorf("outer (event_id) claim row count = %d, want 1 (must stay held after a rollout refusal, mirroring the authz-denial/no-default-repo Skip branches)", outerClaimCount)
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

// TestHandler_NewMention_RolloutGate_EnrolledRepoStillCreatesSession is
// the refusal test's own positive control: the IDENTICAL setup, except
// the default repo IS enrolled -- proves cohort mode is a real,
// bidirectional gate here too.
func TestHandler_NewMention_RolloutGate_EnrolledRepoStillCreatesSession(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, "narvidev/narvi", true); err != nil {
		t.Fatalf("seed enrollment: %v", err)
	}
	rig := newSlackAckTestRigWithRolloutMode(t, pool, platform.RolloutModeCohort, repoSettings)

	envelope := appMentionEnvelope("Ev0ROLLOUT002", "C0ROLLOUT2", "1700000041.000100", "", "<@U0BOT> please fix the build")

	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	if _, err := rig.threads.Get(ctx, "C0ROLLOUT2", "1700000041.000100"); err != nil {
		t.Errorf("Get thread mapping: %v, want a real mapping -- an enrolled repo must not be refused", err)
	}
}
