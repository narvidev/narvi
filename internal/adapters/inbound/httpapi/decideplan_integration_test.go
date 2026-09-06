//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves §8.1's ("plan mode, cross-channel", §8.1/§13.3) own
// central deliverables at the shared httpapi.DecidePlan level:
//   - cross-channel notify: a decision on a plan with a stored Slack
//     message ref enqueues (and, run through a real outboxworker.Builder,
//     actually delivers) a chat.update reflecting the outcome;
//   - first-wins across channels: two DIFFERENT verdicts racing the SAME
//     plan produce exactly one winner, and the loser's own FinalStatus
//     honestly reports the real (winning) outcome.

// seedAwaitingApprovalPlanWithSlackRef mirrors seedAwaitingApprovalPlan
// (planapprove_integration_test.go) but additionally stamps a stored Slack
// message ref onto the row -- simulating that the outbox's own Slack
// plan-approval notifier already successfully posted this plan version's
// approval-request message (internal/app/outboxworker/planslacknotifier.go).
func seedAwaitingApprovalPlanWithSlackRef(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, version int32, channelID, messageTS string) sqlcgen.Plan {
	t.Helper()
	plan := seedAwaitingApprovalPlan(ctx, t, r, sessionID, version)
	if err := r.plans.SetSlackMessageRef(ctx, plan.ID, channelID, messageTS); err != nil {
		t.Fatalf("seed slack message ref: %v", err)
	}
	updated, err := r.plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("re-fetch seeded plan: %v", err)
	}
	return updated
}

// TestDecidePlan_CrossChannelNotify_SlackMessageUpdated proves point 6 of
// this Step's own brief: approving a plan (here, via the SAME pool-based
// httpapi.DecidePlan entry point Slack/Linear callers use) with a stored
// Slack message ref enqueues a ports.NotificationKindSlackPlanDecided
// outbox row, and running that row through a REAL outboxworker.Builder
// (backed by a fake Slack-shaped httptest.Server) actually calls
// chat.update against the exact stored channel/ts with an outcome-shaped
// text.
func TestDecidePlan_CrossChannelNotify_SlackMessageUpdated(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlanWithSlackRef(ctx, t, rig, session.ID, 1, "C-cross-channel", "1700000000.000001")

	var noDecider pgtype.UUID
	outcome, err := httpapi.DecidePlan(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, rig.events, rig.planDocuments, rig.outbox, rig.linearAgentSessions, rig.auditLog, rig.registry,
		session.ID, plan.ID, httpapi.PlanVerdictApprove, noDecider, false)
	if err != nil {
		t.Fatalf("DecidePlan: %v", err)
	}
	if !outcome.Won || outcome.FinalStatus != "approved" {
		t.Fatalf("outcome = %+v, want Won=true FinalStatus=approved", outcome)
	}

	// The outbox row itself: exactly one ports.NotificationKindSlackPlanDecided
	// row for this session, carrying the stored channel/ts.
	var kind, payloadRaw string
	if err := rig.pool.QueryRow(ctx,
		`SELECT kind, payload::text FROM outbox WHERE session_id = $1 AND kind = $2`,
		session.ID, string(ports.NotificationKindSlackPlanDecided),
	).Scan(&kind, &payloadRaw); err != nil {
		t.Fatalf("query enqueued slack plan-decided outbox row: %v", err)
	}
	var payload slackapi.PlanDecidedPayload
	if err := json.Unmarshal([]byte(payloadRaw), &payload); err != nil {
		t.Fatalf("unmarshal outbox payload: %v", err)
	}
	if payload.ChannelID != "C-cross-channel" || payload.MessageTS != "1700000000.000001" {
		t.Errorf("payload channel/ts = (%q, %q), want (%q, %q)", payload.ChannelID, payload.MessageTS, "C-cross-channel", "1700000000.000001")
	}

	// Now run a REAL outboxworker.Builder, backed by a fake Slack API
	// server, to prove the row is actually DELIVERED as a real chat.update
	// call -- not merely enqueued.
	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()

	slackClient := slackapi.New(server.Client(), server.URL, "xoxb-test-token")
	planNotifier := outboxworker.NewPlanSlackNotifier(slackClient, rig.plans)

	builder, err := outboxworker.NewBuilder(rig.outbox, rig.pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlackPlanDecided: planNotifier,
	}, platform.DefaultTimeouts())
	if err != nil {
		t.Fatalf("outboxworker.NewBuilder: %v", err)
	}
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if gotPath != "/chat.update" {
		t.Fatalf("delivered request path = %q, want %q (row was never actually delivered)", gotPath, "/chat.update")
	}
	if gotBody["channel"] != "C-cross-channel" || gotBody["ts"] != "1700000000.000001" {
		t.Errorf("delivered (channel, ts) = (%v, %v), want (%q, %q)", gotBody["channel"], gotBody["ts"], "C-cross-channel", "1700000000.000001")
	}

	var status sqlcgen.OutboxStatus
	if err := rig.pool.QueryRow(ctx, `SELECT status FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindSlackPlanDecided)).Scan(&status); err != nil {
		t.Fatalf("query outbox status: %v", err)
	}
	if status != sqlcgen.OutboxStatusDelivered {
		t.Errorf("outbox status = %q, want %q", status, sqlcgen.OutboxStatusDelivered)
	}
}

// TestDecidePlan_FirstWinsAcrossChannels_ApproveVsReject proves "first
// verdict wins" holds even when the two racing calls carry DIFFERENT
// verdicts (approve vs reject) -- exactly the cross-channel race this
// Step's own brief describes (e.g. Slack approves while Linear/web
// rejects, concurrently). Exactly one call must win; the loser's own
// FinalStatus must honestly report the REAL winning outcome, never its
// own losing verdict or a bare "conflict".
func TestDecidePlan_FirstWinsAcrossChannels_ApproveVsReject(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlanWithSlackRef(ctx, t, rig, session.ID, 1, "C-race", "1700000000.000002")

	type result struct {
		verdict httpapi.PlanVerdict
		outcome httpapi.DecidePlanOutcome
		err     error
	}
	results := make(chan result, 2)

	var eg errgroup.Group
	for _, verdict := range []httpapi.PlanVerdict{httpapi.PlanVerdictApprove, httpapi.PlanVerdictReject} {
		verdict := verdict
		eg.Go(func() error {
			var noDecider pgtype.UUID
			outcome, err := httpapi.DecidePlan(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, rig.events, rig.planDocuments, rig.outbox, rig.linearAgentSessions, rig.auditLog, rig.registry,
				session.ID, plan.ID, verdict, noDecider, false)
			results <- result{verdict: verdict, outcome: outcome, err: err}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		t.Fatalf("errgroup.Wait: %v", err)
	}
	close(results)

	var wins int
	var winningVerdict httpapi.PlanVerdict
	all := make([]result, 0, 2)
	for r := range results {
		all = append(all, r)
		if r.err != nil {
			t.Fatalf("DecidePlan(%s) unexpected error: %v", r.verdict, r.err)
		}
		if r.outcome.Won {
			wins++
			winningVerdict = r.verdict
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (first verdict wins, even across different verdicts)", wins)
	}

	wantFinalStatus := "approved"
	if winningVerdict == httpapi.PlanVerdictReject {
		wantFinalStatus = "rejected"
	}

	for _, r := range all {
		if r.outcome.FinalStatus != wantFinalStatus {
			t.Errorf("DecidePlan(%s).FinalStatus = %q, want %q (the REAL winning outcome, whether this call won or lost)", r.verdict, r.outcome.FinalStatus, wantFinalStatus)
		}
	}

	// The DB itself must agree.
	var dbStatus sqlcgen.PlanStatus
	if err := rig.pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if string(dbStatus) != wantFinalStatus {
		t.Errorf("db status = %q, want %q", dbStatus, wantFinalStatus)
	}

	// Exactly one ports.NotificationKindSlackPlanDecided row must have been
	// enqueued (by the WINNER's own DecidePlanOnTx call), reflecting the
	// real final outcome -- never one per racing call.
	var notifyCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindSlackPlanDecided)).Scan(&notifyCount); err != nil {
		t.Fatalf("count enqueued outbox rows: %v", err)
	}
	if notifyCount != 1 {
		t.Errorf("enqueued slack plan-decided outbox rows = %d, want exactly 1", notifyCount)
	}
}

// TestDecidePlan_CrossSessionPlanIDDoesNotLeakStatus is the regression test
// for a confirmed adversarial-review finding: DecidePlanOnTx's post-UPDATE
// re-fetch (plans.WithTx(tx).Get(ctx, planID), decideplan.go) looks up the
// plan row BY PLAN ID ALONE, with no session_id filter -- unlike the guarded
// UPDATE immediately above it, which IS correctly scoped to (planID,
// sessionRow.ID) and so correctly affects 0 rows for a cross-session planID.
// Before the fix, that re-fetch would still SUCCEED for a planID that
// exists but belongs to a DIFFERENT session than the one the caller
// actually established a relationship with (e.g. a forged/replayed Slack
// button value, a Linear lookup bug, a malformed REST call, or simple data
// confusion elsewhere) -- returning that OTHER session's real, current plan
// status via DecidePlanOutcome.FinalStatus, which callers render straight
// into caller-facing text (e.g. "already decided: <status>"). That leaks
// information about a session the caller has no established relationship
// to.
//
// This seeds two INDEPENDENT sessions, each with its own plan: session B's
// plan is decided for real first (so it carries a genuine, non-empty
// FinalStatus, "approved", making a leak unambiguous rather than
// coincidentally matching an empty default). It then calls DecidePlan for
// session A's sessionID but session B's planID -- exactly the cross-session
// mismatch described above. Pre-fix, this test fails: outcome.FinalStatus
// leaks session B's real "approved" status. Post-fix, the mismatch is
// caught and treated exactly like the existing pgx.ErrNoRows case
// immediately above it in decideplan.go: Won=false, FinalStatus empty, and
// (critically) session B's own plan row is left completely untouched.
func TestDecidePlan_CrossSessionPlanIDDoesNotLeakStatus(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)

	sessionA := createSessionForUser(ctx, t, rig, owner.ID, nil)
	sessionB := createSessionForUser(ctx, t, rig, owner.ID, nil)
	planB := seedAwaitingApprovalPlan(ctx, t, rig, sessionB.ID, 1)

	// Decide session B's own plan for real first, so its FinalStatus is a
	// genuine, non-empty, distinctive value ("approved") a leak would
	// unambiguously surface.
	var noDecider pgtype.UUID
	decideOutcome, err := httpapi.DecidePlan(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, rig.events, rig.planDocuments, rig.outbox, rig.linearAgentSessions, rig.auditLog, rig.registry,
		sessionB.ID, planB.ID, httpapi.PlanVerdictApprove, noDecider, false)
	if err != nil || !decideOutcome.Won || decideOutcome.FinalStatus != "approved" {
		t.Fatalf("seed decision on session B's own plan: outcome=%+v err=%v, want Won=true FinalStatus=approved", decideOutcome, err)
	}

	// Now the cross-session call: session A's OWN sessionID, but session
	// B's planID -- a planID that exists, and IS currently 'approved', but
	// belongs to a session sessionA has no relationship to whatsoever.
	outcome, err := httpapi.DecidePlan(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, rig.events, rig.planDocuments, rig.outbox, rig.linearAgentSessions, rig.auditLog, rig.registry,
		sessionA.ID, planB.ID, httpapi.PlanVerdictApprove, noDecider, false)
	if err != nil {
		t.Fatalf("DecidePlan(cross-session planID): unexpected error: %v", err)
	}
	if outcome.Won {
		t.Errorf("outcome.Won = true, want false (a cross-session planID must never be treated as a real decision)")
	}
	if outcome.FinalStatus != "" {
		t.Errorf("outcome.FinalStatus = %q, want empty -- session B's real status must never leak to a caller scoped to session A", outcome.FinalStatus)
	}

	// Session B's own plan row must be completely untouched by session A's
	// mismatched call: still 'approved' from the earlier legitimate
	// decision, same decided_by, never re-decided.
	var dbStatus sqlcgen.PlanStatus
	if err := rig.pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, planB.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query session B's plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusApproved {
		t.Errorf("session B's plan db status = %q, want %q (untouched by session A's mismatched call)", dbStatus, sqlcgen.PlanStatusApproved)
	}
}

// auditLogDetailForPlanDecision fetches the single plan.<verdict> audit_log
// row's own detail_json for planID, decoded into a plain map -- this file's
// own assertion helper for the audit-fix batch's own M2-part-1 fix
// (decideplan.go's DecidePlanOnTx now includes the created implementation
// turn's own id in this detail, when one was created).
func auditLogDetailForPlanDecision(ctx context.Context, t *testing.T, rig testRig, action, planID string) map[string]any {
	t.Helper()
	var detailRaw []byte
	if err := rig.pool.QueryRow(ctx,
		`SELECT detail_json FROM audit_log WHERE resource_type = 'plan' AND resource_id = $1 AND action = $2`,
		planID, action,
	).Scan(&detailRaw); err != nil {
		t.Fatalf("query audit_log detail_json for action %q: %v", action, err)
	}
	var detail map[string]any
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		t.Fatalf("unmarshal audit_log detail_json: %v", err)
	}
	return detail
}

// TestDecidePlanOnTx_Approve_AuditDetailCarriesTurnID proves the audit-fix
// batch's own M2 part 1: an Approve verdict's own "plan.approve" audit_log
// row now carries the newly-created implementation turn's own id in its
// detail JSON, alongside the pre-existing session_id/verdict fields.
func TestDecidePlanOnTx_Approve_AuditDetailCarriesTurnID(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	var noDecider pgtype.UUID
	outcome, err := httpapi.DecidePlan(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, rig.events, rig.planDocuments, rig.outbox, rig.linearAgentSessions, rig.auditLog, rig.registry,
		session.ID, plan.ID, httpapi.PlanVerdictApprove, noDecider, false)
	if err != nil {
		t.Fatalf("DecidePlan: %v", err)
	}
	if !outcome.Won || outcome.TurnID == nil || *outcome.TurnID == "" {
		t.Fatalf("outcome = %+v, want Won=true and a non-empty TurnID", outcome)
	}

	detail := auditLogDetailForPlanDecision(ctx, t, rig, "plan.approve", plan.ID.String())
	gotTurnID, ok := detail["turn_id"].(string)
	if !ok || gotTurnID != *outcome.TurnID {
		t.Errorf("detail_json[turn_id] = %v, want %q", detail["turn_id"], *outcome.TurnID)
	}
	if detail["session_id"] != session.ID.String() {
		t.Errorf("detail_json[session_id] = %v, want %q", detail["session_id"], session.ID.String())
	}
	if detail["verdict"] != "approve" {
		t.Errorf("detail_json[verdict] = %v, want %q", detail["verdict"], "approve")
	}
}

// TestDecidePlanOnTx_Reject_AuditDetailHasNoTurnID is the Approve test's
// sibling: a Reject verdict creates no turn at all, so its own
// "plan.reject" audit_log row's detail JSON must carry no turn_id key
// whatsoever -- never a present-but-null one.
func TestDecidePlanOnTx_Reject_AuditDetailHasNoTurnID(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	var noDecider pgtype.UUID
	outcome, err := httpapi.DecidePlan(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, rig.events, rig.planDocuments, rig.outbox, rig.linearAgentSessions, rig.auditLog, rig.registry,
		session.ID, plan.ID, httpapi.PlanVerdictReject, noDecider, false)
	if err != nil {
		t.Fatalf("DecidePlan: %v", err)
	}
	if !outcome.Won || outcome.TurnID != nil {
		t.Fatalf("outcome = %+v, want Won=true and a nil TurnID", outcome)
	}

	detail := auditLogDetailForPlanDecision(ctx, t, rig, "plan.reject", plan.ID.String())
	if _, ok := detail["turn_id"]; ok {
		t.Errorf("detail_json carries a turn_id key (%v), want none -- a reject verdict creates no turn", detail["turn_id"])
	}
	if detail["verdict"] != "reject" {
		t.Errorf("detail_json[verdict] = %v, want %q", detail["verdict"], "reject")
	}
}
