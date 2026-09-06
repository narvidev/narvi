//go:build integration

// This file proves the follow-up fix (§8.1, closing the "reply
// matching no verdict keyword dispatches an ordinary build turn anyway"
// hole found during design review): Slack's own POST /webhooks/slack
// (Events API ingress, handler.go) now blocks a plain-text thread reply
// from dispatching an ordinary (plan_mode=false) build turn while the
// mapped session has a plan in StatusAwaitingApproval, and instead either
// posts an honest clarification reply (ackPlanAwaitingText) or -- for a
// revise:-prefixed reply -- creates a REAL plan_mode=true "request changes"
// turn. Mirrors handler_integration_test.go's/identity_integration_test.go's
// own newTestPool/newSlackTestRig-adjacent/linkSlackIdentityForTest/
// signedSlackRequest/appMentionEnvelope/messageEnvelope/
// newFakeSlackRecordingWithUsersInfo conventions exactly (same package,
// same file set's own established helpers reused directly).
package slack_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/slack"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// newSlackPlanGateTestRig mirrors identity_integration_test.go's own
// newSlackHandlerRigForIdentityTests exactly, with additions: Plans is
// wired to a real *narvipg.PlanStore (Deps.Plans, handler.go's own field)
// -- every other test rig in this package leaves it nil (harmless, see
// that field's own doc comment), but this file's own tests need a real
// one for handleEvent's own awaiting-plan gate/verdict/revise-prefix check
// to find anything at all. Outbox/LinearAgentSessions (this batch's own
// addition, "honour a typed plan verdict") are likewise real, non-nil
// stores -- textverdict_integration_test.go's own tests (this package)
// exercise handlePlanVerdict's own httpapi.DecidePlan call, which needs
// both, mirroring interactive_integration_test.go's own
// newInteractiveTestRigWithTimeouts identical wiring.
func newSlackPlanGateTestRig(t *testing.T, pool *pgxpool.Pool, recordingSlackServer *httptest.Server, auditLog *narvipg.AuditLogStore) *slackTestRig {
	t.Helper()
	ctx := context.Background()

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	deliveries := narvipg.NewWebhookDeliveryStore(pool)
	threads := narvipg.NewSlackThreadSessionStore(pool)
	plans := narvipg.NewPlanStore(pool)
	events := narvipg.NewEventStore(pool)
	planDocuments := narvipg.NewPlanDocumentStore(pool)
	outbox := narvipg.NewOutboxStore(pool, false)
	linearAgentSessions := narvipg.NewLinearAgentSessionStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	handler := slack.NewHandler(slack.Deps{
		Pool:                pool,
		Sessions:            sessions,
		Turns:               turns,
		Environments:        environments,
		Registry:            registry,
		Deliveries:          deliveries,
		Threads:             threads,
		Plans:               plans,
		Events:              events,
		PlanDocuments:       planDocuments,
		Outbox:              outbox,
		LinearAgentSessions: linearAgentSessions,
		AuditLog:            auditLog,
		Participants:        narvipg.NewParticipantStore(pool),
		SigningSecret:       testSigningSecret,
		DefaultRepoName:     "narvi",
		DefaultRepoURL:      "https://github.com/narvidev/narvi",
		TimestampWindow:     5 * time.Minute,
		AckTimeout:          platform.DefaultTimeouts().SlackAckTimeout,
		SlackClient:         slackapi.New(recordingSlackServer.Client(), recordingSlackServer.URL, "test-bot-token"),
		Timeouts:            platform.DefaultTimeouts(),
		IdentityLink:        newIdentityLinkDepsForTest(pool, auditLog),
	})

	return &slackTestRig{handler: handler, pool: pool, sessions: sessions, turns: turns, threads: threads, plans: plans}
}

// seedAwaitingApprovalPlanForSlack seeds an awaiting_approval plans row atop
// turnID (already Completed/plan_mode=true) -- mirrors httpapi_test's own
// seedAwaitingApprovalPlan (planapprove_integration_test.go), duplicated
// here since this package cannot reach that one's unexported helper.
func seedAwaitingApprovalPlanForSlack(ctx context.Context, t *testing.T, plans *narvipg.PlanStore, sessionID, turnID pgtype.UUID) sqlcgen.Plan {
	t.Helper()
	plan, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: sessionID, TurnID: turnID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}
	return plan
}

// TestHandler_ReplyOnMappedThread_AwaitingPlan_OrdinaryText_PostsHonestReply
// proves a plain-text reply matching neither an approve/reject button click
// (Slack plan verdicts are buttons, never text) nor plandomain.MatchRevise,
// while the mapped session has a plan in StatusAwaitingApproval, is BLOCKED
// -- no new turn is created, the plan's own status is untouched, and the
// honest ackPlanAwaitingText reply is posted back to the thread instead of
// the ordinary ack.
func TestHandler_ReplyOnMappedThread_AwaitingPlan_OrdinaryText_PostsHonestReply(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)

	recordingServer, recordedBodies := newFakeSlackRecordingWithUsersInfo(t, "unused", "unused@example.com")
	linkSlackIdentityForTest(ctx, t, pool, "U0TESTUSER", sqlcgen.UserRoleMaintainer)
	linkSlackIdentityForTest(ctx, t, pool, "U0OTHERUSER", sqlcgen.UserRoleMaintainer)

	rig := newSlackPlanGateTestRig(t, pool, recordingServer, auditLog)

	firstEnvelope := appMentionEnvelope("Ev0PLANGATE001", "C0PLANGATE", "1700000040.000100", "", "start this task")
	req := signedSlackRequest(t, firstEnvelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first mention: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	mapping, err := rig.threads.Get(ctx, "C0PLANGATE", "1700000040.000100")
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}
	sessionID := mapping.SessionID

	firstTurns, err := rig.turns.ListForSession(ctx, sessionID)
	if err != nil || len(firstTurns) != 1 {
		t.Fatalf("ListForSession after first mention: turns=%v err=%v, want exactly 1", firstTurns, err)
	}

	// Move the producing turn to a terminal, plan_mode=true state and seed
	// an awaiting_approval plan atop it -- mirrors httpapi_test's own
	// seedAwaitingApprovalPlan precedent (planapprove_integration_test.go).
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:          firstTurns[0].ID,
		Status:      sqlcgen.TurnStatusCompleted,
		CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	plan := seedAwaitingApprovalPlanForSlack(ctx, t, rig.plans, sessionID, firstTurns[0].ID)

	replyEnvelope := messageEnvelope("Ev0PLANGATE002", "C0PLANGATE", "1700000041.000200", "1700000040.000100", "please also cover the edge case")
	req2 := signedSlackRequest(t, replyEnvelope)
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("reply: status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}

	finalTurns, err := rig.turns.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListForSession after reply: %v", err)
	}
	if len(finalTurns) != 1 {
		t.Fatalf("len(turns) after reply = %d, want exactly 1 (the gate must block the ordinary reply from creating a second one)", len(finalTurns))
	}

	var dbStatus sqlcgen.PlanStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("db status = %q, want %q (an ordinary reply must never decide the plan)", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
	}

	// The handler call above already ran synchronously (every ack call it
	// makes happens before rig.handler returns) -- requests is a buffered
	// channel, so every request it ever sent is already sitting in it by
	// now; drained non-blockingly, mirroring identity_integration_test.go's
	// own identical "already-populated buffered channel" drain pattern.
	var gotHonestReply bool
drain:
	for {
		select {
		case got := <-recordedBodies:
			if got.path != "/chat.postMessage" {
				continue
			}
			if text, ok := got.body["text"].(string); ok && strings.Contains(text, "awaiting approval for this session") {
				gotHonestReply = true
			}
		default:
			break drain
		}
	}
	if !gotHonestReply {
		t.Error("no chat.postMessage call carried the awaiting-plan honest reply")
	}
}

// TestHandler_ReplyOnMappedThread_AwaitingPlan_RevisePrefix_CreatesPlanModeTurn
// proves the deterministic revise: path (this batch's own new capability):
// a reply STARTING with the revise: prefix, while the mapped session has a
// plan in StatusAwaitingApproval, creates a REAL plan_mode=true turn
// carrying the stripped feedback as its prompt -- BEFORE this fix, a Slack
// chat reply had no way to request changes at all (only the "Request
// changes" Block Kit button did); this reply-based path is newly reachable.
func TestHandler_ReplyOnMappedThread_AwaitingPlan_RevisePrefix_CreatesPlanModeTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)

	recordingServer, _ := newFakeSlackRecordingWithUsersInfo(t, "unused", "unused@example.com")
	linkSlackIdentityForTest(ctx, t, pool, "U0TESTUSER", sqlcgen.UserRoleMaintainer)
	linkSlackIdentityForTest(ctx, t, pool, "U0OTHERUSER", sqlcgen.UserRoleMaintainer)

	rig := newSlackPlanGateTestRig(t, pool, recordingServer, auditLog)

	firstEnvelope := appMentionEnvelope("Ev0PLANREVISE001", "C0PLANREVISE", "1700000050.000100", "", "start this task")
	req := signedSlackRequest(t, firstEnvelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first mention: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	mapping, err := rig.threads.Get(ctx, "C0PLANREVISE", "1700000050.000100")
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}
	sessionID := mapping.SessionID

	firstTurns, err := rig.turns.ListForSession(ctx, sessionID)
	if err != nil || len(firstTurns) != 1 {
		t.Fatalf("ListForSession after first mention: turns=%v err=%v, want exactly 1", firstTurns, err)
	}
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:          firstTurns[0].ID,
		Status:      sqlcgen.TurnStatusCompleted,
		CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	plan := seedAwaitingApprovalPlanForSlack(ctx, t, rig.plans, sessionID, firstTurns[0].ID)

	replyEnvelope := messageEnvelope("Ev0PLANREVISE002", "C0PLANREVISE", "1700000051.000200", "1700000050.000100", "revise: drop the retry logic")
	req2 := signedSlackRequest(t, replyEnvelope)
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("reply: status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}

	finalTurns, err := rig.turns.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListForSession after reply: %v", err)
	}
	if len(finalTurns) != 2 {
		t.Fatalf("len(turns) after reply = %d, want 2 (the seeded producing turn + the new plan_mode=true revise turn)", len(finalTurns))
	}
	var newTurn *sqlcgen.Turn
	for i := range finalTurns {
		if finalTurns[i].ID != firstTurns[0].ID {
			newTurn = &finalTurns[i]
		}
	}
	if newTurn == nil {
		t.Fatal("no new turn found")
	}
	if !newTurn.PlanMode {
		t.Error("new turn PlanMode = false, want true")
	}
	if newTurn.Prompt == nil || *newTurn.Prompt != "drop the retry logic" {
		t.Errorf("new turn prompt = %v, want %q (the revise: prefix must be stripped)", newTurn.Prompt, "drop the retry logic")
	}

	var dbStatus sqlcgen.PlanStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("db status = %q, want %q (a revise reply must never itself decide the plan)", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
	}
}
