//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// storeToolSteps stores steps tool steps of a turn as the session actor
// stores a real one's (technical plan §6.1): per step, a step_start under
// its message's id, and a tool_call, tool_result and step_finish under the
// keys their correlators derive, each tool output of 200 bytes.
func storeToolSteps(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, steps int) {
	t.Helper()
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO events (session_id, type, message_id, payload)
		SELECT $1, kind.type,
		       CASE kind.type WHEN 'step_start' THEN 'msg_' || s
		                      WHEN 'step_finish' THEN 'msg_' || s || '#step_finish:prt_finish_' || s
		                      ELSE 'msg_' || s || '#' || kind.type || ':call_' || s END,
		       jsonb_build_object('type', kind.type, 'messageId', 'msg_' || s, 'callId', 'call_' || s, 'stepId', 'prt_' || s,
		                          'output', jsonb_build_object('output', repeat('o', 200)))
		FROM generate_series(1, $2::int) s
		CROSS JOIN LATERAL (VALUES (1, 'step_start'), (2, 'tool_call'), (3, 'tool_result'), (4, 'step_finish')) AS kind(ord, type)
		ORDER BY s, kind.ord`, sessionID, steps); err != nil {
		t.Fatalf("store %d tool steps: %v", steps, err)
	}
}

// TestTurnText_LaterTurnsToolRowsPastTheTail_SummaryAndPlanStillRead: a
// turn streams its text and ends, and a later turn in flight logs 600 tool
// steps, about four rows a step since a turn's tool calls, results and step
// ends are stored. The earlier turn's text now lies more than 2000 rows
// below the log's end, past the tail the session result and the plan views
// read it from before, where both lost it: the result's summary went null
// and an awaiting plan's content the placeholder. Each reads the turn's own
// window now, and finds it.
func TestTurnText_LaterTurnsToolRowsPastTheTail_SummaryAndPlanStillRead(t *testing.T) {
	ctx := context.Background()
	rig, user, cookie := newResultRig(t, nil)
	sess := createSessionForUser(ctx, t, rig, user.ID, nil)

	run := dispatchedRun(ctx, t, rig, sess.ID)
	tokenFrame(ctx, t, rig, sess.ID, "prt_plan", "prt_plan", "")
	tokenFrame(ctx, t, rig, sess.ID, "prt_plan#2", "prt_plan", "THE PLAN v1")
	transitionTurn(ctx, t, rig.turns, run.ID, turn.StateDispatched, turn.TriggerStartProcessing)
	transitionTurn(ctx, t, rig.turns, run.ID, turn.StateProcessing, turn.TriggerComplete)
	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: sess.ID, TurnID: run.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create awaiting plan: %v", err)
	}

	later := dispatchedRun(ctx, t, rig, sess.ID)
	storeToolSteps(ctx, t, rig, sess.ID, 600)
	var after int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND id > (SELECT MAX(id) FROM events WHERE session_id = $1 AND type = 'token')`, sess.ID).Scan(&after); err != nil {
		t.Fatalf("count the rows after the text: %v", err)
	}
	if after < 2000 {
		t.Fatalf("%d rows after the earlier turn's text, want over 2000: the test would not push it past the tail", after)
	}

	got, raw := getResult(t, rig, sess.ID, cookie)
	if lastRun, _ := raw["lastRun"].(map[string]any); lastRun["turnId"] != run.ID.String() || lastRun["turnId"] == later.ID.String() {
		t.Fatalf("lastRun = %v, want the ended run", lastRun)
	}
	if s := got.LastRun.Summary; s.Text == nil || *s.Text != "THE PLAN v1" || s.Cut != nil {
		t.Errorf("the last run's summary = %v (cut %v), want its text, read from its own window", s.Text, s.Cut)
	}

	var plans restdtos.ListPlansResponse
	if status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+sess.ID.String()+"/plans", nil, &plans, cookie); status != http.StatusOK {
		t.Fatalf("GET plans = %d, want 200", status)
	}
	if len(plans.Plans) != 1 || plans.Plans[0].Id != plan.ID.String() || plans.Plans[0].Content != "THE PLAN v1" {
		t.Errorf("plans = %+v, want plan v1 with its text, read from its producing turn's window", plans.Plans)
	}
}
