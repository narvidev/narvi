//go:build integration

// This file is the audit-remediation batch's own regression tests for the
// "revise: accepts empty feedback" finding: plandomain.MatchRevise documents
// ok=true, feedback=="" for a bare "revise:" (or whitespace-only feedback)
// as an EXPLICIT caller's-own-job case (verdict.go's own doc comment) --
// handlePrompted (webhook.go) now applies the SAME empty-feedback guard the
// pre-existing Slack "Request changes" Block Kit modal already applies
// (slack/interactive.go's own handleViewSubmission: "empty feedback text in
// view_submission, ignoring"), instead of silently dispatching a genuine
// plan_mode=true revision turn with nothing at all for the agent to act on.
// Mirrors planverdict_integration_test.go's own newHandlerDeps/postWebhook/
// agentSessionPromptedPayloadWithUser conventions and
// setsessionid_retry_integration_test.go's own capture-slog's-own-default-
// logger precedent exactly (same package, same file set's own established
// helpers reused directly).
package linear_test

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestWebhookHandler_Prompted_RevisePrefix_EmptyFeedback_BlockedNoTurnCreated
// is table-driven (§11) over a bare "revise:" and a whitespace-only
// "revise:   " reply -- both match plandomain.MatchRevise with ok=true,
// feedback=="" (verdict_test.go's own TestMatchRevise already pins this
// exact contract), yet handlePrompted must NOT dispatch a plan_mode=true
// turn with an empty prompt for either: no new turn is created, the plan's
// own status is untouched, and the DISTINCT, more specific
// emptyReviseFeedbackReplyText reply is posted -- LOW audit fix (SECOND
// fix-pass, "the honest reply reused for the new empty-feedback case is
// generic boilerplate") -- rather than the generic
// planAwaitingApprovalReplyText
// TestWebhookHandler_Prompted_NonKeywordNonReviseText_BlockedByAwaitingPlan
// proves for a non-revise ordinary reply.
//
// COSMETIC/LOW audit fix (confirmed finding, "whitespace-only-feedback
// coverage is limited to the plain-ASCII-space case ... tab, newline, and
// Unicode whitespace ... are never exercised, at either this
// integration-test layer or the pre-existing domain-level
// verdict_test.go"): the tab/newline/NBSP/ideographic-space/
// zero-width-space cases below close that gap at this full webhook-level
// integration layer, mirroring verdict_test.go's own TestIsBlankFeedback
// unit-level coverage of the exact same variants.
func TestWebhookHandler_Prompted_RevisePrefix_EmptyFeedback_BlockedNoTurnCreated(t *testing.T) {
	cases := []struct {
		name       string
		suffix     string
		text       string
		deliveryID string
	}{
		{name: "bare prefix, no feedback at all", suffix: "empty1", text: "revise:", deliveryID: "delivery-plan-revise-empty-1"},
		{name: "prefix followed only by whitespace", suffix: "empty2", text: "revise:   ", deliveryID: "delivery-plan-revise-empty-2"},
		{name: "prefix followed only by a tab", suffix: "empty3", text: "revise:\t\t", deliveryID: "delivery-plan-revise-empty-3"},
		{name: "prefix followed only by a newline", suffix: "empty4", text: "revise:\n\n", deliveryID: "delivery-plan-revise-empty-4"},
		{name: "prefix followed only by NBSP (U+00A0)", suffix: "empty5", text: "revise:\u00A0\u00A0", deliveryID: "delivery-plan-revise-empty-5"},
		{name: "prefix followed only by an ideographic space (U+3000)", suffix: "empty6", text: "revise:\u3000", deliveryID: "delivery-plan-revise-empty-6"},
		{name: "prefix followed only by zero-width-space runes (U+200B)", suffix: "empty7", text: "revise:\u200B\u200B", deliveryID: "delivery-plan-revise-empty-7"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			deps := newHandlerDeps(t, pool)
			deps.Plans = narvipg.NewPlanStore(pool)
			deps.Events = narvipg.NewEventStore(pool)
			deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
			deps.Outbox = narvipg.NewOutboxStore(pool, false)
			deps.Participants = narvipg.NewParticipantStore(pool)

			ctx := context.Background()
			agentSessionID := "agent-session-plan-revise-" + tc.suffix
			organizationID := "org-plan-revise-" + tc.suffix

			installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)
			stub, recordedBodies := newGenericLinearGraphQLStub(t)
			deps.LinearClient = linearapi.New(stub.Client(), stub.URL)
			deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
			replierID := "linear-planverdict-revise-" + tc.suffix
			linkLinearIdentityForTest(ctx, t, pool, replierID, sqlcgen.UserRoleMaintainer)

			handler := linear.NewWebhookHandler(deps)

			sessions := narvipg.NewSessionStore(pool)
			turns := narvipg.NewTurnStore(pool)
			plans := narvipg.NewPlanStore(pool)
			agentSessions := narvipg.NewLinearAgentSessionStore(pool)

			session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceLinear})
			if err != nil {
				t.Fatalf("create linear-origin session: %v", err)
			}
			if _, err := agentSessions.Claim(ctx, agentSessionID, organizationID); err != nil {
				t.Fatalf("claim agent session: %v", err)
			}
			if err := agentSessions.SetSessionID(ctx, agentSessionID, session.ID); err != nil {
				t.Fatalf("attach session id: %v", err)
			}
			producingTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
			if err != nil {
				t.Fatalf("seed producing turn: %v", err)
			}
			plan, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producingTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
			if err != nil {
				t.Fatalf("seed awaiting_approval plan: %v", err)
			}

			body := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, replierID, tc.text)
			rec := postWebhook(t, handler, body, tc.deliveryID)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}

			var dbStatus sqlcgen.PlanStatus
			if err := pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
				t.Fatalf("query plan row: %v", err)
			}
			if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
				t.Errorf("db status = %q, want %q (empty revise: feedback must never decide the plan)", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
			}

			allTurns, err := turns.ListForSession(ctx, session.ID)
			if err != nil {
				t.Fatalf("list turns: %v", err)
			}
			if len(allTurns) != 1 {
				t.Fatalf("len(turns) = %d, want exactly 1 (the seeded producing turn only -- empty revise: feedback must never create a turn)", len(allTurns))
			}

			var gotHonestReply bool
			for _, b := range recordedBodies() {
				if strings.Contains(b, "no feedback followed it") {
					gotHonestReply = true
				}
			}
			if !gotHonestReply {
				t.Error("no outbound activity contained the empty-revise-feedback honest reply")
			}
		})
	}
}

// TestWebhookHandler_Prompted_RevisePrefix_NonEmptyFeedback_LogsPlanModeTrueOnSuccess
// is this batch's own regression test for the SECOND, observability half of
// the finding ("neither Slack nor Linear logs the routing decision
// itself"): a non-empty revise: reply's own success log line
// ("linear: added turn") must now carry plan_mode=true, making the
// re-routing decision (an ordinary reply turned into a plan revision)
// observable -- mirrors setsessionid_retry_integration_test.go's own
// capture-slog's-own-default-logger precedent exactly.
func TestWebhookHandler_Prompted_RevisePrefix_NonEmptyFeedback_LogsPlanModeTrueOnSuccess(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Plans = narvipg.NewPlanStore(pool)
	deps.Events = narvipg.NewEventStore(pool)
	deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
	deps.Outbox = narvipg.NewOutboxStore(pool, false)
	deps.Participants = narvipg.NewParticipantStore(pool)

	ctx := context.Background()
	agentSessionID := "agent-session-plan-revise-log"
	organizationID := "org-plan-revise-log"

	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)
	stub, _ := newGenericLinearGraphQLStub(t)
	deps.LinearClient = linearapi.New(stub.Client(), stub.URL)
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
	const replierID = "linear-planverdict-revise-log"
	linkLinearIdentityForTest(ctx, t, pool, replierID, sqlcgen.UserRoleMaintainer)

	handler := linear.NewWebhookHandler(deps)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	plans := narvipg.NewPlanStore(pool)
	agentSessions := narvipg.NewLinearAgentSessionStore(pool)

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceLinear})
	if err != nil {
		t.Fatalf("create linear-origin session: %v", err)
	}
	if _, err := agentSessions.Claim(ctx, agentSessionID, organizationID); err != nil {
		t.Fatalf("claim agent session: %v", err)
	}
	if err := agentSessions.SetSessionID(ctx, agentSessionID, session.ID); err != nil {
		t.Fatalf("attach session id: %v", err)
	}
	producingTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	if _, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producingTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval}); err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}

	// syncLogBuffer (webhook_integration_test.go), not a bare
	// strings.Builder: this test's own reply below creates a plan-mode
	// turn, which fires the SAME fire-and-forget GetOrSpawn+EnsureDispatched
	// dispatch trigger every turn-creation call site uses -- the session's
	// Actor can still be mid-flight on its own background goroutine,
	// logging through this SAME redirected default logger, while this
	// test's own goroutine reads logOutput below. See syncLogBuffer's own
	// doc comment for the full race (an identical instance of the one
	// caught by -race in CI run 30887614911, in this package's own
	// TestWebhookHandler_Prompted_LogsSessionAndTurnID).
	logBuf := &syncLogBuffer{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	body := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, replierID, "revise: drop the retry logic")
	rec := postWebhook(t, handler, body, "delivery-plan-revise-log")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "linear: added turn") {
		t.Fatalf("log output missing the success log line; got: %s", logOutput)
	}
	if !strings.Contains(logOutput, `"plan_mode":true`) {
		t.Errorf("success log line missing plan_mode=true -- the re-routing decision (ordinary reply -> plan revision) must be observable; got: %s", logOutput)
	}
}

// TestWebhookHandler_Prompted_RevisePrefix_EmptyFeedback_LogsAtInfoNotWarn
// is the LOW audit finding's own regression test ("log-level inconsistency
// between the new empty-feedback-guard branch and the pre-existing ...
// 'blocked by awaiting-approval plan' branch"): the empty-feedback
// branch's own log line must be emitted at Info, matching the
// functionally identical ErrPlanAwaitingApproval branch's own pre-existing
// Info-level log ("linear: ordinary reply blocked by awaiting-approval
// plan") -- both are routine, expected user mistakes producing the exact
// same kind of honest reply and no adverse system state, so neither should
// out-rank the other on a Warn-level alert. Mirrors Slack's identical
// TestHandler_ReplyOnMappedThread_AwaitingPlan_RevisePrefix_EmptyFeedback_LogsAtInfoNotWarn.
func TestWebhookHandler_Prompted_RevisePrefix_EmptyFeedback_LogsAtInfoNotWarn(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Plans = narvipg.NewPlanStore(pool)
	deps.Events = narvipg.NewEventStore(pool)
	deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
	deps.Outbox = narvipg.NewOutboxStore(pool, false)
	deps.Participants = narvipg.NewParticipantStore(pool)

	ctx := context.Background()
	agentSessionID := "agent-session-plan-revise-loglvl"
	organizationID := "org-plan-revise-loglvl"

	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)
	stub, _ := newGenericLinearGraphQLStub(t)
	deps.LinearClient = linearapi.New(stub.Client(), stub.URL)
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
	const replierID = "linear-planverdict-revise-loglvl"
	linkLinearIdentityForTest(ctx, t, pool, replierID, sqlcgen.UserRoleMaintainer)

	handler := linear.NewWebhookHandler(deps)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	plans := narvipg.NewPlanStore(pool)
	agentSessions := narvipg.NewLinearAgentSessionStore(pool)

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceLinear})
	if err != nil {
		t.Fatalf("create linear-origin session: %v", err)
	}
	if _, err := agentSessions.Claim(ctx, agentSessionID, organizationID); err != nil {
		t.Fatalf("claim agent session: %v", err)
	}
	if err := agentSessions.SetSessionID(ctx, agentSessionID, session.ID); err != nil {
		t.Fatalf("attach session id: %v", err)
	}
	producingTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	if _, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producingTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval}); err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}

	// syncLogBuffer (webhook_integration_test.go), not a bare
	// strings.Builder -- this specific reply hits the emptyReviseFeedback
	// early-return branch (webhook.go), which never reaches CreateTurnCore
	// so no Actor is spawned for THIS call, but this file's sibling test
	// just above (identical capture pattern, one webhook call away) does
	// race exactly the way syncLogBuffer's own doc comment describes; kept
	// consistent here too rather than leaving a second, currently-dormant
	// instance of the same unsafe-shared-writer pattern in this file for a
	// future edit to wake back up.
	logBuf := &syncLogBuffer{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	body := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, replierID, "revise:")
	rec := postWebhook(t, handler, body, "delivery-plan-revise-loglvl")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var gotLine string
	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		if strings.Contains(line, "revise reply had empty feedback") {
			gotLine = line
			break
		}
	}
	if gotLine == "" {
		t.Fatalf("log output missing the empty-feedback-guard log line; got: %s", logBuf.String())
	}
	if !strings.Contains(gotLine, `"level":"INFO"`) {
		t.Errorf(`empty-feedback-guard log line level != INFO (got line: %s) -- must match the functionally identical ErrPlanAwaitingApproval branch's own Info level, not a higher Warn severity for a routine user mistake`, gotLine)
	}
}
