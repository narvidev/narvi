package httpapi

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// This file tests sessionActivityToDTO -- the status route's decode of one
// GetSessionActivityFacts row into restdtos.SessionActivity -- on synthetic
// rows, for what real Postgres cannot produce or never produces alone:
// a turn state turn_status does not have yet, and every gate open at once.
// The route itself, on real Postgres, is sessionstatus_integration_test.go.

var statusObservedAt = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func statusFactsRow(turnCounts string) sqlcgen.GetSessionActivityFactsRow {
	return sqlcgen.GetSessionActivityFactsRow{
		SessionID:     pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		SessionStatus: sqlcgen.SessionStatusCompleted,
		ObservedAt:    pgtype.Timestamptz{Time: statusObservedAt, Valid: true},
		TurnCounts:    []byte(turnCounts),
	}
}

func statusDTO(t *testing.T, facts sqlcgen.GetSessionActivityFactsRow) restdtos.SessionActivity {
	t.Helper()
	timeouts := platform.DefaultTimeouts()
	got, err := sessionActivityToDTO(facts, statusDelayTable(timeouts), timeouts.MCPStatusDeliveryWindow)
	if err != nil {
		t.Fatalf("sessionActivityToDTO: %v", err)
	}
	return got
}

// TestSessionActivityToDTO_UnknownTurnStateReachesTheDerivation pins the
// promise of technical plan §43.20 and GetSessionActivityFacts' own doc
// comment where it could break -- the decode, not the domain rule: a turn
// state added to turn_status later is carried from the JSON histogram into
// the derivation, which counts it as in flight, never dropped. Were the
// decode narrowed to the states known today, a session whose only live
// turn is in a new state would read finished and settled.
func TestSessionActivityToDTO_UnknownTurnStateReachesTheDerivation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		turnCounts string
	}{
		{"a new state beside terminal turns", `{"completed":2,"warming_up":1}`},
		{"a new state alone", `{"warming_up":1}`},
		{"a new state beside every terminal state", `{"completed":1,"failed":1,"cancelled":1,"warming_up":3}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := statusDTO(t, statusFactsRow(tc.turnCounts))
			if got.Activity != restdtos.SessionActivityActivityRunning || got.Settled {
				t.Fatalf("turn counts %s: activity %q settled %v, want running and unsettled -- the unknown state was dropped", tc.turnCounts, got.Activity, got.Settled)
			}
			if got.SuggestedDelaySeconds != 10 {
				t.Fatalf("turn counts %s: delay %d, want the running 10", tc.turnCounts, got.SuggestedDelaySeconds)
			}
		})
	}

	t.Run("only the known terminal states -> finished (the control)", func(t *testing.T) {
		t.Parallel()
		if got := statusDTO(t, statusFactsRow(`{"completed":2,"failed":1}`)); got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled {
			t.Fatalf("activity %q settled %v, want finished and settled", got.Activity, got.Settled)
		}
	})
}

// TestSessionActivityToDTO_AwaitingPrecedence pins the order the schema,
// §43.20 and awaitingDTO's own comment give when more than one gate is
// open: a plan first, then a workflow step, then an escalated run. Real
// Postgres can hold a plan and a step at once (a custom plan-lane workflow
// with a human gate after its step: the turn's completion records the plan
// and marks the step awaiting a decision in one transaction), and the
// route's integration test pins that one; this sweeps every combination.
func TestSessionActivityToDTO_AwaitingPrecedence(t *testing.T) {
	t.Parallel()

	planID := pgtype.UUID{Bytes: [16]byte{0xa}, Valid: true}
	stepID := pgtype.UUID{Bytes: [16]byte{0xb}, Valid: true}
	runID := pgtype.UUID{Bytes: [16]byte{0xc}, Valid: true}
	planSince := statusObservedAt.Add(-3 * time.Minute)
	stepSince := statusObservedAt.Add(-2 * time.Minute)
	runSince := statusObservedAt.Add(-time.Minute)

	for _, tc := range []struct {
		name            string
		plan, step, run bool
		wantKind        restdtos.SessionActivityAwaitingKind
		wantID          pgtype.UUID
		wantSince       time.Time
	}{
		{"plan, step and escalation -> the plan", true, true, true, restdtos.SessionActivityAwaitingKindPlan, planID, planSince},
		{"plan and step -> the plan", true, true, false, restdtos.SessionActivityAwaitingKindPlan, planID, planSince},
		{"plan and escalation -> the plan", true, false, true, restdtos.SessionActivityAwaitingKindPlan, planID, planSince},
		{"step and escalation -> the step", false, true, true, restdtos.SessionActivityAwaitingKindWorkflowStep, stepID, stepSince},
		{"the escalation alone", false, false, true, restdtos.SessionActivityAwaitingKindWorkflowEscalation, runID, runSince},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			facts := statusFactsRow(`{"completed":1}`)
			if tc.plan {
				facts.AwaitingPlanID, facts.AwaitingPlanSince = planID, pgtype.Timestamptz{Time: planSince, Valid: true}
			}
			if tc.step {
				facts.AwaitingStepID, facts.AwaitingStepSince = stepID, pgtype.Timestamptz{Time: stepSince, Valid: true}
			}
			if tc.run {
				facts.EscalatedRunID, facts.EscalatedRunSince = runID, pgtype.Timestamptz{Time: runSince, Valid: true}
			}
			got := statusDTO(t, facts)
			if got.Activity != restdtos.SessionActivityActivityAwaitingApproval || !got.Settled {
				t.Fatalf("activity %q settled %v, want awaiting_approval and settled", got.Activity, got.Settled)
			}
			if got.Awaiting == nil || got.Awaiting.Kind != tc.wantKind || got.Awaiting.Id != tc.wantID.String() || !got.Awaiting.Since.Equal(tc.wantSince) {
				t.Fatalf("awaiting = %+v, want %s %v since %v", got.Awaiting, tc.wantKind, tc.wantID, tc.wantSince)
			}
		})
	}
}

// TestSessionActivityToDTO_DeliveryWindow pins the handler's half of the
// push/PR delivery (technical plan §43.20): a stamp inside
// MCPStatusDeliveryWindow of the snapshot reads delivering -- unsettled,
// with the short delivering delay -- whatever gate is also open, and one
// the window has passed reads as if there were none, so a push that never
// reported back cannot hold the session unsettled for good.
func TestSessionActivityToDTO_DeliveryWindow(t *testing.T) {
	t.Parallel()

	window := platform.DefaultTimeouts().MCPStatusDeliveryWindow
	planID := pgtype.UUID{Bytes: [16]byte{0xa}, Valid: true}
	for _, tc := range []struct {
		name         string
		stamp        *time.Time
		plan         bool
		wantActivity restdtos.SessionActivityActivity
		wantSettled  bool
		wantDelay    int
	}{
		{"no stamp -> finished", nil, false, restdtos.SessionActivityActivityFinished, true, 300},
		{"stamped seconds ago -> delivering", stampAt(statusObservedAt.Add(-4 * time.Second)), false, restdtos.SessionActivityActivityDelivering, false, 5},
		{"stamped just inside the window -> delivering", stampAt(statusObservedAt.Add(-window + time.Second)), false, restdtos.SessionActivityActivityDelivering, false, 5},
		{"stamped a window ago -> finished", stampAt(statusObservedAt.Add(-window)), false, restdtos.SessionActivityActivityFinished, true, 300},
		{"stamped a day ago -> finished", stampAt(statusObservedAt.Add(-24 * time.Hour)), false, restdtos.SessionActivityActivityFinished, true, 300},
		{"stamped seconds ago beside an open plan -> delivering, the plan still reported", stampAt(statusObservedAt.Add(-4 * time.Second)), true, restdtos.SessionActivityActivityDelivering, false, 5},
		{"a stale stamp beside an open plan -> awaiting_approval", stampAt(statusObservedAt.Add(-window - time.Minute)), true, restdtos.SessionActivityActivityAwaitingApproval, true, 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			facts := statusFactsRow(`{"completed":1}`)
			if tc.stamp != nil {
				facts.PrDeliveryStartedAt = pgtype.Timestamptz{Time: *tc.stamp, Valid: true}
			}
			if tc.plan {
				facts.AwaitingPlanID, facts.AwaitingPlanSince = planID, pgtype.Timestamptz{Time: statusObservedAt.Add(-time.Minute), Valid: true}
			}
			got := statusDTO(t, facts)
			if got.Activity != tc.wantActivity || got.Settled != tc.wantSettled || got.SuggestedDelaySeconds != tc.wantDelay {
				t.Fatalf("activity %q settled %v delay %d, want %q, %v, %d", got.Activity, got.Settled, got.SuggestedDelaySeconds, tc.wantActivity, tc.wantSettled, tc.wantDelay)
			}
			if tc.plan && (got.Awaiting == nil || got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindPlan) {
				t.Fatalf("awaiting = %+v, want the plan reported whatever the activity", got.Awaiting)
			}
		})
	}
}

func stampAt(at time.Time) *time.Time { return &at }
