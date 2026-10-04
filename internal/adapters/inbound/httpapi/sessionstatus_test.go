package httpapi

import (
	"encoding/json"
	"strings"
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
	got, err := sessionActivityToDTO(facts, statusDelayTable(timeouts), statusBoundsFrom(timeouts))
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
// escalation reports the open escalation in every one of them, whatever
// awaiting names first, and is null without one.
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
		{"the step alone", false, true, false, restdtos.SessionActivityAwaitingKindWorkflowStep, stepID, stepSince},
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
			switch {
			case !tc.run && got.Escalation != nil:
				t.Errorf("escalation = %+v, want null: no escalation is open", got.Escalation)
			case tc.run && (got.Escalation == nil || got.Escalation.Id != runID.String() || !got.Escalation.Since.Equal(runSince)):
				t.Errorf("escalation = %+v, want run %v since %v whatever awaiting names", got.Escalation, runID, runSince)
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

// TestSessionActivityToDTO_ScheduledWork pins the handler's half of review
// round 3's P1 (technical plan §43.20's inventory): an armed timer whose
// kind can create a turn -- or a kind sessionactor does not know -- and a
// release manifest check still to come or still running read scheduled,
// never settled, whatever gate is open; timers that only watch a sandbox
// or an in-flight turn do not; the re-review debounce counts only while
// review_retrigger_can_fire says its fire can insert a turn (review round
// 4's P3: the repository opted in, the budget unspent); a claimed check
// whose worker died stops counting once ReleaseManifestCheckTimeout plus
// MCPStatusScheduledMargin has passed. The suggestion never reaches past
// the work's due instant plus the margin, and is otherwise the scheduled
// 15 s.
func TestSessionActivityToDTO_ScheduledWork(t *testing.T) {
	t.Parallel()

	timeouts := platform.DefaultTimeouts()
	at := func(d time.Duration) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: statusObservedAt.Add(d), Valid: true}
	}
	planID := pgtype.UUID{Bytes: [16]byte{0xa}, Valid: true}
	type timer struct {
		name string
		in   time.Duration
	}
	for _, tc := range []struct {
		name         string
		turnCounts   string
		timers       []timer
		pendingSince *time.Duration
		claimedAgo   *time.Duration
		stampAgo     *time.Duration
		plan         bool
		// cannotFire is review_retrigger_can_fire false: the repository has
		// not opted in, or the budget is spent.
		cannotFire   bool
		wantActivity restdtos.SessionActivityActivity
		wantDelay    int
	}{
		{"nothing armed -> finished", `{"completed":1}`, nil, nil, nil, nil, false, false, restdtos.SessionActivityActivityFinished, 300},
		{"the reviewers' case: a re-review debounce armed 2 min out -> scheduled", `{"completed":1}`, []timer{{"review_retrigger_debounce", 2 * time.Minute}}, nil, nil, nil, false, false, restdtos.SessionActivityActivityScheduled, 15},
		{"a debounce due in 3 s -> due plus the margin", `{"completed":1}`, []timer{{"review_retrigger_debounce", 3 * time.Second}}, nil, nil, nil, false, false, restdtos.SessionActivityActivityScheduled, 8},
		{"a debounce overdue (the pump has not claimed it yet) -> the margin", `{"completed":1}`, []timer{{"review_retrigger_debounce", -2 * time.Second}}, nil, nil, nil, false, false, restdtos.SessionActivityActivityScheduled, 5},
		{"every sandbox-only and in-flight-only kind armed -> finished", `{"completed":1}`, []timer{{"connecting_deadline", time.Second}, {"liveness_check", time.Second}, {"inactivity", time.Minute}, {"terminal_grace", time.Second}, {"turn_deadline", time.Hour}}, nil, nil, nil, false, false, restdtos.SessionActivityActivityFinished, 300},
		{"a kind this binary does not know -> scheduled, never settled", `{"completed":1}`, []timer{{"a_kind_from_a_newer_binary", time.Minute}}, nil, nil, nil, false, false, restdtos.SessionActivityActivityScheduled, 15},
		{"the earliest work-creating timer decides the delay", `{"completed":1}`, []timer{{"liveness_check", time.Second}, {"a_kind_from_a_newer_binary", 4 * time.Second}, {"review_retrigger_debounce", time.Minute}}, nil, nil, nil, false, false, restdtos.SessionActivityActivityScheduled, 9},
		{"a debounce beside an open plan -> scheduled, the plan still reported", `{"completed":1}`, []timer{{"review_retrigger_debounce", time.Minute}}, nil, nil, nil, true, false, restdtos.SessionActivityActivityScheduled, 15},
		{"a debounce while a delivery is under way -> delivering", `{"completed":1}`, []timer{{"review_retrigger_debounce", time.Minute}}, nil, nil, durationPtr(-3 * time.Second), false, false, restdtos.SessionActivityActivityDelivering, 5},
		{"a debounce behind a queued turn -> queued", `{"completed":1,"pending":1}`, []timer{{"review_retrigger_debounce", time.Minute}}, nil, nil, nil, false, false, restdtos.SessionActivityActivityQueued, 15},
		{"a release check enqueued beside the review turn still queued -> queued", `{"pending":1}`, nil, durationPtr(-time.Second), nil, nil, false, false, restdtos.SessionActivityActivityQueued, 15},
		{"a release check enqueued, the review turn done -> scheduled", `{"completed":1}`, nil, durationPtr(-time.Second), nil, nil, false, false, restdtos.SessionActivityActivityScheduled, 14},
		{"a release check waiting long past a tick (the worker is busy) -> scheduled, the margin", `{"completed":1}`, nil, durationPtr(-time.Hour), nil, nil, false, false, restdtos.SessionActivityActivityScheduled, 5},
		{"a release check claimed a minute ago -> scheduled", `{"completed":1}`, nil, nil, durationPtr(time.Minute), nil, false, false, restdtos.SessionActivityActivityScheduled, 15},
		{"a release check claimed just inside its bound -> scheduled", `{"completed":1}`, nil, nil, durationPtr(timeouts.ReleaseManifestCheckTimeout + timeouts.MCPStatusScheduledMargin - time.Second), nil, false, false, restdtos.SessionActivityActivityScheduled, 5},
		{"a release check claimed past its bound (its worker died) -> finished", `{"completed":1}`, nil, nil, durationPtr(timeouts.ReleaseManifestCheckTimeout + timeouts.MCPStatusScheduledMargin), nil, false, false, restdtos.SessionActivityActivityFinished, 300},
		{"a dead claim beside an open plan -> awaiting_approval", `{"completed":1}`, nil, nil, durationPtr(time.Hour), nil, true, false, restdtos.SessionActivityActivityAwaitingApproval, 60},
		{"a debounce that cannot fire (not opted in, or the budget spent) -> finished at once", `{"completed":1}`, []timer{{"review_retrigger_debounce", 2 * time.Minute}}, nil, nil, nil, false, true, restdtos.SessionActivityActivityFinished, 300},
		{"a debounce that cannot fire beside an open plan -> awaiting_approval", `{"completed":1}`, []timer{{"review_retrigger_debounce", time.Minute}}, nil, nil, nil, true, true, restdtos.SessionActivityActivityAwaitingApproval, 60},
		{"a debounce that cannot fire beside an unknown kind -> scheduled on the unknown kind", `{"completed":1}`, []timer{{"review_retrigger_debounce", 3 * time.Second}, {"a_kind_from_a_newer_binary", time.Minute}}, nil, nil, nil, false, true, restdtos.SessionActivityActivityScheduled, 15},
		{"a debounce that cannot fire beside a release check waiting -> scheduled on the check", `{"completed":1}`, []timer{{"review_retrigger_debounce", 3 * time.Second}}, durationPtr(-time.Second), nil, nil, false, true, restdtos.SessionActivityActivityScheduled, 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			facts := statusFactsRow(tc.turnCounts)
			facts.ReviewRetriggerCanFire = !tc.cannotFire
			facts.ArmedTimerNames = []string{}
			facts.ArmedTimerFiresAt = []pgtype.Timestamptz{}
			for _, tm := range tc.timers {
				facts.ArmedTimerNames = append(facts.ArmedTimerNames, tm.name)
				facts.ArmedTimerFiresAt = append(facts.ArmedTimerFiresAt, at(tm.in))
			}
			if tc.pendingSince != nil {
				facts.ReleaseCheckPendingSince = at(*tc.pendingSince)
			}
			if tc.claimedAgo != nil {
				facts.ReleaseCheckClaimedAt = at(-*tc.claimedAgo)
			}
			if tc.stampAgo != nil {
				facts.PrDeliveryStartedAt = at(*tc.stampAgo)
			}
			if tc.plan {
				facts.AwaitingPlanID, facts.AwaitingPlanSince = planID, at(-time.Minute)
			}
			got := statusDTO(t, facts)
			wantSettled := tc.wantActivity == restdtos.SessionActivityActivityFinished || tc.wantActivity == restdtos.SessionActivityActivityAwaitingApproval
			if got.Activity != tc.wantActivity || got.Settled != wantSettled || got.SuggestedDelaySeconds != tc.wantDelay {
				t.Fatalf("activity %q settled %v delay %d, want %q, %v, %d", got.Activity, got.Settled, got.SuggestedDelaySeconds, tc.wantActivity, wantSettled, tc.wantDelay)
			}
			if tc.plan && (got.Awaiting == nil || got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindPlan) {
				t.Fatalf("awaiting = %+v, want the plan reported whatever the activity", got.Awaiting)
			}
		})
	}

	t.Run("names and instants out of step are refused, never guessed", func(t *testing.T) {
		t.Parallel()
		facts := statusFactsRow(`{"completed":1}`)
		facts.ArmedTimerNames = []string{"review_retrigger_debounce"}
		if _, err := sessionActivityToDTO(facts, statusDelayTable(timeouts), statusBoundsFrom(timeouts)); err == nil {
			t.Fatal("sessionActivityToDTO accepted one timer name with no instant")
		}
	})
}

func durationPtr(d time.Duration) *time.Duration { return &d }

// TestSessionActivityToDTO_ReviewRetriggerDropped pins the decode of the
// automatic re-review's drop (technical plan §24.9): a facts row carrying
// one renders SessionActivity.reviewRetriggerDropped with its head, its
// instant and the one reason a drop is recorded for, beside the settled
// reading the drop leaves -- no debounce, so nothing scheduled -- and a row
// with none renders no property at all, so a plain read is unchanged.
func TestSessionActivityToDTO_ReviewRetriggerDropped(t *testing.T) {
	t.Parallel()

	droppedAt := statusObservedAt.Add(-time.Minute)
	for _, tc := range []struct {
		name string
		drop bool
	}{
		{name: "a drop", drop: true},
		{name: "none", drop: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			facts := statusFactsRow(`{"completed":1,"failed":1}`)
			if tc.drop {
				facts.ReviewRetriggerDroppedAt = pgtype.Timestamptz{Time: droppedAt, Valid: true}
				facts.ReviewRetriggerDroppedHeadSha = "sha-given-up-on"
			}
			got := statusDTO(t, facts)
			if got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled {
				t.Fatalf("activity %q settled %v, want finished and settled", got.Activity, got.Settled)
			}
			if !tc.drop {
				if got.ReviewRetriggerDropped != nil {
					t.Fatalf("reviewRetriggerDropped = %+v, want absent", got.ReviewRetriggerDropped)
				}
				body, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(body), "reviewRetriggerDropped") {
					t.Fatalf("the wire carries reviewRetriggerDropped with no drop: %s", body)
				}
				return
			}
			want := restdtos.SessionActivityReviewRetriggerDropped{
				HeadSha:   "sha-given-up-on",
				DroppedAt: droppedAt,
				Reason:    restdtos.SessionActivityReviewRetriggerDroppedReasonContextMovedBound,
			}
			if got.ReviewRetriggerDropped == nil || *got.ReviewRetriggerDropped != want {
				t.Fatalf("reviewRetriggerDropped = %+v, want %+v", got.ReviewRetriggerDropped, want)
			}
		})
	}
}
