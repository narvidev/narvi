package session_test

import (
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/session"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// counts builds an ActivityInput.TurnCounts from state/count pairs.
func counts(pairs ...any) map[turn.State]int {
	m := map[turn.State]int{}
	for i := 0; i < len(pairs); i += 2 {
		m[pairs[i].(turn.State)] = pairs[i+1].(int)
	}
	return m
}

// TestDeriveActivity_Table is table-driven over every combination
// technical plan §43.20 names: the precedence (in flight, then pending,
// then a human gate, then no turn at all, then finished), an unknown turn
// state read as running, and -- the failure this exists to prevent -- a
// non-empty queue never reading as idle or finished, whatever else the
// snapshot holds. The input carries no session Status at all: the
// derivation cannot consult it.
func TestDeriveActivity_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   session.ActivityInput
		want session.Activity
	}{
		{"zero turns -> idle", session.ActivityInput{}, session.ActivityIdle},
		{"zero turns, empty map -> idle", session.ActivityInput{TurnCounts: counts()}, session.ActivityIdle},
		{"lone pending -> queued", session.ActivityInput{TurnCounts: counts(turn.StatePending, 1)}, session.ActivityQueued},
		{"lone dispatched -> running", session.ActivityInput{TurnCounts: counts(turn.StateDispatched, 1)}, session.ActivityRunning},
		{"lone processing -> running", session.ActivityInput{TurnCounts: counts(turn.StateProcessing, 1)}, session.ActivityRunning},
		{"processing with a pending turn behind it -> running", session.ActivityInput{TurnCounts: counts(turn.StateProcessing, 1, turn.StatePending, 1)}, session.ActivityRunning},
		{"dispatched with a pending turn behind it -> running", session.ActivityInput{TurnCounts: counts(turn.StateDispatched, 1, turn.StatePending, 2)}, session.ActivityRunning},
		{"completed then a queued follow-up -> queued, never finished", session.ActivityInput{TurnCounts: counts(turn.StateCompleted, 1, turn.StatePending, 1)}, session.ActivityQueued},
		{"failed then a queued follow-up -> queued", session.ActivityInput{TurnCounts: counts(turn.StateFailed, 1, turn.StatePending, 1)}, session.ActivityQueued},
		{"completed then a running follow-up -> running", session.ActivityInput{TurnCounts: counts(turn.StateCompleted, 3, turn.StateProcessing, 1)}, session.ActivityRunning},
		{"all terminal -> finished", session.ActivityInput{TurnCounts: counts(turn.StateCompleted, 2, turn.StateFailed, 1, turn.StateCancelled, 1)}, session.ActivityFinished},
		{"lone cancelled -> finished", session.ActivityInput{TurnCounts: counts(turn.StateCancelled, 1)}, session.ActivityFinished},
		{"completed + plan awaiting -> awaiting_approval", session.ActivityInput{TurnCounts: counts(turn.StateCompleted, 1), PlanAwaitingApproval: true}, session.ActivityAwaitingApproval},
		{"plan awaiting + pending follow-up -> queued", session.ActivityInput{TurnCounts: counts(turn.StateCompleted, 1, turn.StatePending, 1), PlanAwaitingApproval: true}, session.ActivityQueued},
		{"plan awaiting + processing turn -> running", session.ActivityInput{TurnCounts: counts(turn.StateProcessing, 1), PlanAwaitingApproval: true}, session.ActivityRunning},
		{"workflow step awaiting_decision -> awaiting_approval", session.ActivityInput{TurnCounts: counts(turn.StateCompleted, 1), WorkflowStepAwaitingDecision: true}, session.ActivityAwaitingApproval},
		{"workflow step awaiting + pending next step -> queued", session.ActivityInput{TurnCounts: counts(turn.StateCompleted, 1, turn.StatePending, 1), WorkflowStepAwaitingDecision: true}, session.ActivityQueued},
		{"open workflow escalation -> awaiting_approval", session.ActivityInput{TurnCounts: counts(turn.StateFailed, 2), WorkflowEscalationOpen: true}, session.ActivityAwaitingApproval},
		{"a gate with no turn at all -> awaiting_approval, not idle", session.ActivityInput{WorkflowEscalationOpen: true}, session.ActivityAwaitingApproval},
		{"unknown turn state -> running", session.ActivityInput{TurnCounts: counts(turn.State("warming_up"), 1)}, session.ActivityRunning},
		{"unknown turn state among terminal ones -> running, never finished", session.ActivityInput{TurnCounts: counts(turn.StateCompleted, 4, turn.State("warming_up"), 1)}, session.ActivityRunning},
		{"unknown turn state with a plan awaiting -> running", session.ActivityInput{TurnCounts: counts(turn.State(""), 1), PlanAwaitingApproval: true}, session.ActivityRunning},
		{"zero and negative counts are ignored", session.ActivityInput{TurnCounts: counts(turn.StatePending, 0, turn.StateProcessing, -1, turn.StateCompleted, 1)}, session.ActivityFinished},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := session.DeriveActivity(tc.in); got != tc.want {
				t.Fatalf("DeriveActivity(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDeriveActivity_NonEmptyQueueNeverIdleOrFinished sweeps every gate
// combination under every non-empty queue shape: a pending, dispatched,
// processing or unknown-state turn, with or without terminal turns beside
// it, must never read idle, finished or awaiting approval -- the row's own
// "a non-empty queue must never render as idle".
func TestDeriveActivity_NonEmptyQueueNeverIdleOrFinished(t *testing.T) {
	t.Parallel()

	live := []turn.State{turn.StatePending, turn.StateDispatched, turn.StateProcessing, turn.State("unknown")}
	for _, state := range live {
		for _, terminal := range []int{0, 1, 5} {
			for gates := 0; gates < 8; gates++ {
				in := session.ActivityInput{
					TurnCounts:                   counts(state, 1, turn.StateCompleted, terminal),
					PlanAwaitingApproval:         gates&1 != 0,
					WorkflowStepAwaitingDecision: gates&2 != 0,
					WorkflowEscalationOpen:       gates&4 != 0,
				}
				got := session.DeriveActivity(in)
				if got != session.ActivityQueued && got != session.ActivityRunning {
					t.Errorf("DeriveActivity(%+v) = %q, want queued or running", in, got)
				}
				if got.Settled() {
					t.Errorf("DeriveActivity(%+v) = %q reads settled", in, got)
				}
			}
		}
	}
}

func TestActivity_Settled(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		a    session.Activity
		want bool
	}{
		{session.ActivityFinished, true},
		{session.ActivityIdle, true},
		{session.ActivityAwaitingApproval, true},
		{session.ActivityQueued, false},
		{session.ActivityRunning, false},
		{session.Activity("something_new"), false},
		{session.Activity(""), false},
	} {
		if got := tc.a.Settled(); got != tc.want {
			t.Errorf("Activity(%q).Settled() = %v, want %v", tc.a, got, tc.want)
		}
	}
}

// shippedTable mirrors the shipped defaults (platform.DefaultTimeouts's
// MCPStatusDelay* fields) without importing platform.
var shippedTable = session.DelayTable{
	Starting:      15 * time.Second,
	Queued:        5 * time.Second,
	Running:       10 * time.Second,
	AwaitingHuman: 60 * time.Second,
	Settled:       300 * time.Second,
	Floor:         2 * time.Second,
	Ceiling:       300 * time.Second,
}

func sandboxState(s sandbox.State) *sandbox.State { return &s }

func TestSuggestedReadDelay_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		a       session.Activity
		sandbox *sandbox.State
		want    time.Duration
	}{
		{"queued, no sandbox yet -> starting", session.ActivityQueued, nil, 15 * time.Second},
		{"queued, sandbox pending -> starting", session.ActivityQueued, sandboxState(sandbox.StatePending), 15 * time.Second},
		{"queued, sandbox spawning -> starting", session.ActivityQueued, sandboxState(sandbox.StateSpawning), 15 * time.Second},
		{"queued, sandbox connecting -> starting", session.ActivityQueued, sandboxState(sandbox.StateConnecting), 15 * time.Second},
		{"queued, sandbox booting -> starting", session.ActivityQueued, sandboxState(sandbox.StateBooting), 15 * time.Second},
		{"queued, sandbox stopped (respawn ahead) -> starting", session.ActivityQueued, sandboxState(sandbox.StateStopped), 15 * time.Second},
		{"queued, sandbox failed (respawn ahead) -> starting", session.ActivityQueued, sandboxState(sandbox.StateFailed), 15 * time.Second},
		{"queued, sandbox suspect -> starting", session.ActivityQueued, sandboxState(sandbox.StateSuspect), 15 * time.Second},
		{"queued, unknown sandbox state -> starting", session.ActivityQueued, sandboxState(sandbox.State("new_state")), 15 * time.Second},
		{"queued, sandbox ready -> queued", session.ActivityQueued, sandboxState(sandbox.StateReady), 5 * time.Second},
		{"queued, sandbox snapshotting -> queued", session.ActivityQueued, sandboxState(sandbox.StateSnapshotting), 5 * time.Second},
		{"running -> running", session.ActivityRunning, sandboxState(sandbox.StateReady), 10 * time.Second},
		{"running, sandbox ignored -> running", session.ActivityRunning, nil, 10 * time.Second},
		{"awaiting approval -> human latency", session.ActivityAwaitingApproval, nil, 60 * time.Second},
		{"finished -> settled", session.ActivityFinished, sandboxState(sandbox.StateStopped), 300 * time.Second},
		{"idle -> settled", session.ActivityIdle, nil, 300 * time.Second},
		{"unknown activity -> running, never settled", session.Activity("mystery"), nil, 10 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := session.SuggestedReadDelay(tc.a, tc.sandbox, shippedTable); got != tc.want {
				t.Fatalf("SuggestedReadDelay(%q, %v) = %v, want %v", tc.a, tc.sandbox, got, tc.want)
			}
		})
	}
}

// TestSuggestedReadDelay_AlwaysWithinBounds proves the clamp: with a table
// whose per-activity values fall on both sides of [Floor, Ceiling], every
// activity crossed with every sandbox status (and none, and an unknown
// one) still suggests a delay inside the bounds -- and the out-of-bounds
// values land exactly on the bound they crossed.
func TestSuggestedReadDelay_AlwaysWithinBounds(t *testing.T) {
	t.Parallel()

	wild := session.DelayTable{
		Starting:      time.Hour,        // above the ceiling
		Queued:        time.Millisecond, // below the floor
		Running:       0,                // below the floor
		AwaitingHuman: 45 * time.Second, // inside
		Settled:       24 * time.Hour,   // above the ceiling
		Floor:         3 * time.Second,
		Ceiling:       120 * time.Second,
	}
	activities := []session.Activity{
		session.ActivityIdle, session.ActivityQueued, session.ActivityRunning,
		session.ActivityAwaitingApproval, session.ActivityFinished, session.Activity("unknown"),
	}
	sandboxes := []*sandbox.State{nil}
	for _, s := range []sandbox.State{
		sandbox.StatePending, sandbox.StateSpawning, sandbox.StateConnecting, sandbox.StateBooting,
		sandbox.StateReady, sandbox.StateSnapshotting, sandbox.StateSuspect, sandbox.StateStopped,
		sandbox.StateFailed, sandbox.StateStale, sandbox.State("unknown"),
	} {
		sandboxes = append(sandboxes, sandboxState(s))
	}
	for _, a := range activities {
		for _, sb := range sandboxes {
			got := session.SuggestedReadDelay(a, sb, wild)
			if got < wild.Floor || got > wild.Ceiling {
				t.Errorf("SuggestedReadDelay(%q, %v) = %v, outside [%v, %v]", a, sb, got, wild.Floor, wild.Ceiling)
			}
		}
	}
	for _, tc := range []struct {
		a    session.Activity
		sb   *sandbox.State
		want time.Duration
	}{
		{session.ActivityQueued, nil, wild.Ceiling},
		{session.ActivityQueued, sandboxState(sandbox.StateReady), wild.Floor},
		{session.ActivityRunning, nil, wild.Floor},
		{session.ActivityAwaitingApproval, nil, 45 * time.Second},
		{session.ActivityFinished, nil, wild.Ceiling},
	} {
		if got := session.SuggestedReadDelay(tc.a, tc.sb, wild); got != tc.want {
			t.Errorf("SuggestedReadDelay(%q, %v) = %v, want %v", tc.a, tc.sb, got, tc.want)
		}
	}
}
