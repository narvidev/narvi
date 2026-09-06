//go:build integration

// This file proves this batch's own addition ("honour a typed plan verdict
// in a Slack thread"): handleEvent (handler.go) now recognizes a plain-text
// thread reply matching plandomain.MatchVerdict, while the mapped session
// has a plan in StatusAwaitingApproval, as a REAL plan decision -- routed
// through handlePlanVerdict, which calls the EXACT SAME authorization check
// (Deps.authorizeSessionAction, authz.ActionApprovePlan) and the EXACT SAME
// shared httpapi.DecidePlan every other plan-decision entry point in this
// codebase already uses (interactive.go's own button-driven
// decideAndUpdateMessage) -- rather than deflecting the user to the
// Approve/Reject buttons or (worse) silently dispatching an unapproved
// build turn. Mirrors planapprovalgate_integration_test.go's own
// newSlackPlanGateTestRig/seedAwaitingApprovalPlanForSlack conventions and
// interactive_integration_test.go's own
// TestInteractivityHandler_BlockActions_ApprovePlan/RejectPlan for the
// underlying DecidePlan assertions this file's own text-driven twin checks
// identically (same package, same file sets' own established helpers
// reused directly).
package slack_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/inbound/slack"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// TestHandler_ReplyOnMappedThread_AwaitingPlan_TextVerdict_DecidesPlan is
// table-driven (§11) over every phrasing plandomain.MatchVerdict itself
// accepts (ApproveKeywords/RejectKeywords, verdict.go), plus a case-
// insensitive/whitespace-padded variant proving this path reuses
// MatchVerdict's own normalization rather than a second, looser parse of
// its own. Each phrasing decides the SAME awaiting_approval plan via the
// SAME shared httpapi.DecidePlan the Approve/Reject BUTTONS use
// (interactive.go): decided_by is asserted against the REAL, linked
// replying actor (never NULL/bot-attributed), proving invariant #1 of this
// batch's own brief -- a text verdict flows through the SAME authorization
// check the button path already performs, not a second, ungated one.
func TestHandler_ReplyOnMappedThread_AwaitingPlan_TextVerdict_DecidesPlan(t *testing.T) {
	cases := []struct {
		name             string
		channel          string
		replyText        string
		wantStatus       sqlcgen.PlanStatus
		wantTurns        int
		wantAckSubstring string
	}{
		{"approve", "C0VERDICTAPPROVE", "approve", sqlcgen.PlanStatusApproved, 2, "Approved"},
		{"approved", "C0VERDICTAPPROVED", "approved", sqlcgen.PlanStatusApproved, 2, "Approved"},
		{"lgtm", "C0VERDICTLGTM", "lgtm", sqlcgen.PlanStatusApproved, 2, "Approved"},
		{"reject", "C0VERDICTREJECT", "reject", sqlcgen.PlanStatusRejected, 1, "Rejected"},
		{"rejected", "C0VERDICTREJECTED", "rejected", sqlcgen.PlanStatusRejected, 1, "Rejected"},
		{"no", "C0VERDICTNO", "no", sqlcgen.PlanStatusRejected, 1, "Rejected"},
		// Case-insensitive, trimmed -- MatchVerdict's own doc comment
		// (verdict.go) documents this normalization; this proves
		// handlePlanVerdict's own caller (handleEvent) passes the RAW,
		// un-normalized text straight through to MatchVerdict, rather than
		// pre-normalizing (and potentially drifting from it) itself.
		{"uppercase and padded", "C0VERDICTPADDED", "  APPROVE  ", sqlcgen.PlanStatusApproved, 2, "Approved"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			auditLog := narvipg.NewAuditLogStore(pool)

			recordingServer, recordedBodies := newFakeSlackRecordingWithUsersInfo(t, "unused", "unused@example.com")
			linkSlackIdentityForTest(ctx, t, pool, "U0TESTUSER", sqlcgen.UserRoleMaintainer)
			replier := linkSlackIdentityForTest(ctx, t, pool, "U0OTHERUSER", sqlcgen.UserRoleMaintainer)

			rig := newSlackPlanGateTestRig(t, pool, recordingServer, auditLog)

			firstEnvelope := appMentionEnvelope("Ev0"+tc.channel+"001", tc.channel, "1700000070.000100", "", "start this task")
			req := signedSlackRequest(t, firstEnvelope)
			rec := httptest.NewRecorder()
			rig.handler(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("first mention: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}

			mapping, err := rig.threads.Get(ctx, tc.channel, "1700000070.000100")
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

			replyEnvelope := messageEnvelope("Ev0"+tc.channel+"002", tc.channel, "1700000070.000200", "1700000070.000100", tc.replyText)
			req2 := signedSlackRequest(t, replyEnvelope)
			rec2 := httptest.NewRecorder()
			rig.handler(rec2, req2)
			if rec2.Code != http.StatusOK {
				t.Fatalf("reply: status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
			}

			var dbStatus sqlcgen.PlanStatus
			var decidedBy pgtype.UUID
			if err := pool.QueryRow(ctx, `SELECT status, decided_by FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus, &decidedBy); err != nil {
				t.Fatalf("query plan row: %v", err)
			}
			if dbStatus != tc.wantStatus {
				t.Errorf("db status = %q, want %q", dbStatus, tc.wantStatus)
			}
			if !decidedBy.Valid || decidedBy != replier.ID {
				t.Errorf("decided_by = %v, want %v (the linked replying actor -- proves the text verdict went through Deps.authorizeSessionAction, not a second, ungated path)", decidedBy, replier.ID)
			}

			finalTurns, err := rig.turns.ListForSession(ctx, sessionID)
			if err != nil {
				t.Fatalf("ListForSession after reply: %v", err)
			}
			if len(finalTurns) != tc.wantTurns {
				t.Errorf("len(turns) after reply = %d, want %d", len(finalTurns), tc.wantTurns)
			}

			var gotAck bool
		drain:
			for {
				select {
				case got := <-recordedBodies:
					if got.path != "/chat.postMessage" {
						continue
					}
					if text, ok := got.body["text"].(string); ok && strings.Contains(text, tc.wantAckSubstring) {
						gotAck = true
					}
				default:
					break drain
				}
			}
			if !gotAck {
				t.Errorf("no chat.postMessage call carried the expected outcome reply (want substring %q)", tc.wantAckSubstring)
			}
		})
	}
}

// TestHandler_ReplyOnMappedThread_AwaitingPlan_TextVerdict_UnauthorizedActorDenied
// proves a REPLYING actor who IS linked, but whose role unconditionally
// fails domain/authz.Authorize(RoleViewer -- see authz/authorize_test.go's
// own "viewer cannot approve any plan" case), can never decide a plan by
// typing a verdict: the plan is left untouched, no turn is created, and an
// honest, PLAN-SPECIFIC denial is posted back to the thread -- never a
// silent decision, and never a generic denial that reads differently from
// the identical button-driven/Linear denial.
//
// LOW audit fix (confirmed finding, re-review): the denial here still
// fires at resolveOrClaimSession's own PRE-EXISTING authorizeExistingSessionReply
// gate (domain/authz.ActionPromptSession, handler.go), which still runs
// unconditionally for ANY reply on an already-mapped thread, BEFORE
// handleEvent's own plan-verdict check ever gets a chance to run --
// unlike Linear's webhook.go, where the verdict check runs FIRST. That
// ordering is unchanged (still safe, not a bypass -- authz/authorize.go's
// own matrix, "row 2" comment, gives ActionPromptSession and
// ActionApprovePlan the IDENTICAL allow/allowIfOwned role sets today, so
// this outer gate is never looser than handlePlanVerdict's own inner
// authorizeSessionAction(..., ActionApprovePlan) check). What this fix
// closes is the WORDING mismatch that ordering used to produce:
// authorizeExistingSessionReply now recognizes that this exact denied
// reply also matches plandomain.MatchVerdict against a session with a
// plan in StatusAwaitingApproval, and posts slackPlanForbiddenText (the
// SAME wording handlePlanVerdict's own inner denial branch, and the
// button path, and Linear's own text-verdict path all give for the
// identical underlying denial) instead of the generic
// ackNotAuthorizedReplyText a non-verdict reply still gets. This test now
// asserts that plan-specific text, matching
// TestHandlePlanVerdict_UnauthorizedActor_DeniedByOwnAuthorizationCheck's
// own assertion below byte-for-byte, rather than documenting the two as a
// deliberately-accepted mismatch.
func TestHandler_ReplyOnMappedThread_AwaitingPlan_TextVerdict_UnauthorizedActorDenied(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)

	recordingServer, recordedBodies := newFakeSlackRecordingWithUsersInfo(t, "unused", "unused@example.com")
	linkSlackIdentityForTest(ctx, t, pool, "U0TESTUSER", sqlcgen.UserRoleMaintainer)
	linkSlackIdentityForTest(ctx, t, pool, "U0VIEWERDENY", sqlcgen.UserRoleViewer)

	rig := newSlackPlanGateTestRig(t, pool, recordingServer, auditLog)

	channel := "C0VERDICTDENY"
	firstEnvelope := appMentionEnvelope("Ev0VERDICTDENY001", channel, "1700000071.000100", "", "start this task")
	req := signedSlackRequest(t, firstEnvelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first mention: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	mapping, err := rig.threads.Get(ctx, channel, "1700000071.000100")
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

	replyEnvelope := messageEnvelopeWithUser("Ev0VERDICTDENY002", channel, "1700000071.000200", "1700000071.000100", "approve", "U0VIEWERDENY")
	req2 := signedSlackRequest(t, replyEnvelope)
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("reply: status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}

	var dbStatus sqlcgen.PlanStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("db status = %q, want %q (an unauthorized actor's text verdict must never decide the plan)", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
	}

	finalTurns, err := rig.turns.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListForSession after reply: %v", err)
	}
	if len(finalTurns) != 1 {
		t.Errorf("len(turns) after reply = %d, want exactly 1 (a denied verdict must never create a turn)", len(finalTurns))
	}

	var gotDenialReply bool
drain:
	for {
		select {
		case got := <-recordedBodies:
			if got.path != "/chat.postMessage" {
				continue
			}
			if text, ok := got.body["text"].(string); ok && strings.Contains(text, "don't have permission to approve or reject") {
				gotDenialReply = true
			}
		default:
			break drain
		}
	}
	if !gotDenialReply {
		t.Error("no chat.postMessage call carried the plan-specific denial reply")
	}
}

// TestHandler_ReplyOnMappedThread_NoAwaitingPlan_TextVerdict_TreatedAsOrdinaryReply
// proves a verdict typed when NO plan awaits approval does not misfire:
// findAwaitingApprovalPlanID (turn.go) finds nothing for this session (no
// plan row exists at all), so plandomain.MatchVerdict is never even
// consulted -- the "approve" reply falls through to the ORDINARY reply
// path exactly like any other text would. That ordinary path's own
// pre-existing busy-session rule (DropIfOpen) is what this test actually
// observes: the producing turn from the mention above is still open
// (Pending -- deliberately left that way, unlike every other test in this
// file, which drives it to Completed first), so the "approve" text is
// correctly DROPPED as busy rather than queued as a second turn -- proving
// it was routed as an ordinary reply, never as a phantom plan decision
// (which would have had nothing to decide at all).
func TestHandler_ReplyOnMappedThread_NoAwaitingPlan_TextVerdict_TreatedAsOrdinaryReply(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)

	recordingServer, _ := newFakeSlackRecordingWithUsersInfo(t, "unused", "unused@example.com")
	linkSlackIdentityForTest(ctx, t, pool, "U0TESTUSER", sqlcgen.UserRoleMaintainer)
	linkSlackIdentityForTest(ctx, t, pool, "U0OTHERUSER", sqlcgen.UserRoleMaintainer)

	rig := newSlackPlanGateTestRig(t, pool, recordingServer, auditLog)

	channel := "C0VERDICTNOPLAN"
	firstEnvelope := appMentionEnvelope("Ev0VERDICTNOPLAN001", channel, "1700000072.000100", "", "start this task")
	req := signedSlackRequest(t, firstEnvelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first mention: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	mapping, err := rig.threads.Get(ctx, channel, "1700000072.000100")
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}
	sessionID := mapping.SessionID

	// Deliberately NO plan seeded, and the producing turn is left Pending
	// (non-terminal) -- see this test's own doc comment for why that is
	// exactly what makes the assertion below meaningful.
	replyEnvelope := messageEnvelope("Ev0VERDICTNOPLAN002", channel, "1700000072.000200", "1700000072.000100", "approve")
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
		t.Fatalf(`len(turns) after reply = %d, want exactly 1 (the text "approve" must be treated as an ordinary reply -- busy-dropped here, never a phantom plan decision)`, len(finalTurns))
	}
}

// TestHandlePlanVerdict_UnauthorizedActor_DeniedByOwnAuthorizationCheck
// proves handlePlanVerdict's OWN authorizeSessionAction(...,
// ActionApprovePlan) call independently denies an unauthorized-but-linked
// (RoleViewer) actor, calling it directly via export_test.go's own
// HandlePlanVerdictForTest bridge rather than through a full HTTP request.
//
// Why this can't be proved via a black-box HTTP request (see
// export_test.go's own top doc comment for the full reasoning):
// resolveOrClaimSession's own PRE-EXISTING authorizeExistingSessionReply
// gate (ActionPromptSession) always runs first for a reply on an
// already-mapped thread, and today's authz matrix gives it the IDENTICAL
// role rules ActionApprovePlan has -- so no actor a full HTTP request
// could reach handlePlanVerdict with would ever be denied BY it; the
// denial branch this test targets would be completely unreachable, and
// its own accidental deletion completely undetected, by this file's
// other, HTTP-level tests alone. Since the LOW audit fix above (see
// TestHandler_ReplyOnMappedThread_AwaitingPlan_TextVerdict_UnauthorizedActorDenied's
// own doc comment) makes authorizeExistingSessionReply post this SAME
// slackPlanForbiddenText wording for the equivalent HTTP-level scenario,
// the two tests now assert byte-identical text for two genuinely
// different code paths -- this one still the only test exercising
// handlePlanVerdict's own inner check directly.
//
// This test bypasses resolveOrClaimSession/authorizeExistingSessionReply
// entirely -- session/turn/plan rows are seeded directly (mirroring
// interactive_integration_test.go's own seedSessionTurnAndAwaitingPlan),
// and handlePlanVerdict is invoked directly with a pre-linked RoleViewer
// actor id, proving in isolation that: (1) the plan is left untouched, (2)
// no turn is created, (3) the honest slackPlanForbiddenText denial --
// interactive.go's own EXACT wording for the identical button-path denial
// -- is posted, and (4) the result reports OK=true (a genuine denial,
// never a backend failure that would misleadingly trigger a webhook-claim
// release/retry).
func TestHandlePlanVerdict_UnauthorizedActor_DeniedByOwnAuthorizationCheck(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	plans := narvipg.NewPlanStore(pool)
	events := narvipg.NewEventStore(pool)
	planDocuments := narvipg.NewPlanDocumentStore(pool)
	outbox := narvipg.NewOutboxStore(pool, false)
	linearAgentSessions := narvipg.NewLinearAgentSessionStore(pool)
	participants := narvipg.NewParticipantStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	viewer := linkSlackIdentityForTest(ctx, t, pool, "U0DIRECTVIEWER", sqlcgen.UserRoleViewer)

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	producingTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	plan, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producingTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}

	recordingServer, recordedBodies := newFakeSlackRecordingWithUsersInfo(t, "unused", "unused@example.com")

	deps := slack.Deps{
		Pool:                pool,
		Sessions:            sessions,
		Turns:               turns,
		Plans:               plans,
		Events:              events,
		PlanDocuments:       planDocuments,
		Outbox:              outbox,
		LinearAgentSessions: linearAgentSessions,
		AuditLog:            auditLog,
		Registry:            registry,
		Participants:        participants,
		IdentityLink:        newIdentityLinkDepsForTest(pool, auditLog),
		SlackClient:         slackapi.New(recordingServer.Client(), recordingServer.URL, "test-bot-token"),
		AckTimeout:          platform.DefaultTimeouts().SlackAckTimeout,
	}

	ok, _ := deps.HandlePlanVerdictForTest(ctx, "C0DIRECTVERDICT", "1700000073.000100", session.ID, plan.ID, "approve", viewer.ID)
	if !ok {
		t.Fatal("HandlePlanVerdictForTest ok = false, want true (a real authz denial must never be reported as a backend failure)")
	}

	var dbStatus sqlcgen.PlanStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("db status = %q, want %q (handlePlanVerdict's own authorization check must deny this actor BEFORE ever calling DecidePlan)", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
	}

	finalTurns, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(finalTurns) != 1 {
		t.Errorf("len(turns) = %d, want exactly 1 (the seeded producing turn only -- a denied verdict must never create an implementation turn)", len(finalTurns))
	}

	var gotDenialReply bool
drain:
	for {
		select {
		case got := <-recordedBodies:
			if got.path != "/chat.postMessage" {
				continue
			}
			if text, ok := got.body["text"].(string); ok && strings.Contains(text, "don't have permission to approve or reject") {
				gotDenialReply = true
			}
		default:
			break drain
		}
	}
	if !gotDenialReply {
		t.Error("no chat.postMessage call carried handlePlanVerdict's own plan-decision denial reply")
	}
}
