package sandbox_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/sandbox"
)

// allStates is every state of the machine (state.go), for the sweeps.
var allStates = []sandbox.State{
	sandbox.StatePending, sandbox.StateSpawning, sandbox.StateConnecting, sandbox.StateBooting,
	sandbox.StateReady, sandbox.StateSnapshotting, sandbox.StateSuspect,
	sandbox.StateStopped, sandbox.StateFailed, sandbox.StateStale,
}

// runwayConfig is the gate at the shipped values (platform.DefaultTimeouts:
// RotationRunwayFloor 13m, RotationSnapshotWait 2m, SnapshotReportWait 2m,
// SteadyHeartbeatBudget 90s, TerminalGracePeriod 60s), restated here
// because a domain test does not import platform.
var runwayConfig = sandbox.RunwayConfig{
	Floor: 13 * time.Minute, SnapshotWait: 2 * time.Minute,
	ReportWait: 2 * time.Minute, HeartbeatBudget: 90 * time.Second, Grace: time.Minute,
}

// distinctConfig gives every duration of the gate its own value, so a
// field read in another's place shows: the rotation's wait 2m, the report
// wait 3m, the heartbeat budget 100s, the grace 70s.
var distinctConfig = sandbox.RunwayConfig{
	Floor: 13 * time.Minute, SnapshotWait: 2 * time.Minute,
	ReportWait: 3 * time.Minute, HeartbeatBudget: 100 * time.Second, Grace: 70 * time.Second,
}

// healthyRunway is a ready sandbox at the shipped 2h lifetime, its
// deadline known and an hour away, a provider that snapshots, nothing in
// flight, nothing rotating and no delivery open: a dispatch passes.
func healthyRunway() sandbox.RunwayFacts {
	return sandbox.RunwayFacts{
		Status:        sandbox.StateReady,
		DeadlineKnown: true,
		Runway:        time.Hour,
		Lifetime:      2 * time.Hour,
		CanSnapshot:   true,
	}
}

// TestRotationThreshold pins the threshold the gate measures a runway
// against (technical plan §35.3): the floor or a sixth of the lifetime,
// whichever is less -- never the greater -- and the floor alone when no
// lifetime is known.
func TestRotationThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		floor, lifetime time.Duration
		want            time.Duration
	}{
		{"the floor binds at the shipped lifetime", 13 * time.Minute, 2 * time.Hour, 13 * time.Minute},
		{"a sixth of a short lifetime binds", 13 * time.Minute, time.Hour, 10 * time.Minute},
		{"min, not max: a floor above a sixth of the lifetime is capped", 25 * time.Minute, 2 * time.Hour, 20 * time.Minute},
		{"the floor exactly a sixth of the lifetime", 20 * time.Minute, 2 * time.Hour, 20 * time.Minute},
		{"no lifetime: the floor", 13 * time.Minute, 0, 13 * time.Minute},
		{"a negative lifetime: the floor", 13 * time.Minute, -time.Hour, 13 * time.Minute},
		{"a zero floor", 0, 2 * time.Hour, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sandbox.RotationThreshold(tc.floor, tc.lifetime); got != tc.want {
				t.Errorf("RotationThreshold(%v, %v) = %v, want %v", tc.floor, tc.lifetime, got, tc.want)
			}
		})
	}
}

// TestEvaluateRunway covers every row of the gate's table (technical plan
// §35.3), in its order, each with the neighbours that pin where it starts
// and ends: the boundary runway equal to the threshold passes, a deadline
// that passes mid-rotation retires the gen, and a delivery holds only a
// rotation that would otherwise start.
func TestEvaluateRunway(t *testing.T) {
	t.Parallel()

	const threshold = 13 * time.Minute
	tests := []struct {
		name          string
		facts         func(*sandbox.RunwayFacts)
		cfg           *sandbox.RunwayConfig // nil: runwayConfig
		wantAction    sandbox.RunwayAction
		wantReason    sandbox.RunwayReason
		wantNextLook  time.Duration
		wantThreshold time.Duration // 0: threshold
	}{
		// Row 1: a sandbox that is not live and steady is not the gate's.
		{name: "row 1: pending", facts: func(f *sandbox.RunwayFacts) { f.Status = sandbox.StatePending },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonNotSteady},
		{name: "row 1: booting, past its deadline and rotating, still not the gate's", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating = sandbox.StateBooting, -time.Minute, true
		}, wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonNotSteady},
		{name: "row 1: stopped", facts: func(f *sandbox.RunwayFacts) { f.Status, f.Runway = sandbox.StateStopped, -time.Hour },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonNotSteady},

		// Row 2: the deadline has passed and the agent is silent: the
		// sandbox died at its deadline.
		{name: "row 2: ready, runway exactly zero, silent exactly the budget", facts: func(f *sandbox.RunwayFacts) { f.Runway, f.SinceLastSeen = 0, 90*time.Second },
			wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2: ready, a nanosecond past", facts: func(f *sandbox.RunwayFacts) { f.Runway, f.SinceLastSeen = -1, time.Hour },
			wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2: suspect", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.SinceLastSeen = sandbox.StateSuspect, -time.Minute, 2*time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2: snapshotting, not rotating, before the report wait", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.SinceLastSeen = sandbox.StateSnapshotting, -time.Minute, 2*time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2 precedes row 3: the deadline passes while the rotation waits, its agent silent", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.RotationSnapshotPending, f.SinceRotationStart, f.SinceLastSeen = sandbox.StateSnapshotting, 0, true, true, 30*time.Second, 2*time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2 precedes row 5: the rotation's snapshot taken, the deadline passed, silent", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.Rotating, f.RotationSnapshotTaken, f.SinceLastSeen = -time.Second, true, true, 2*time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2: with a turn in flight", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.TurnInFlight, f.SinceLastSeen = -time.Minute, true, 2*time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2: with no snapshot capability", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.CanSnapshot, f.SinceLastSeen = -time.Minute, false, 2*time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2 needs the deadline known: a stale runway is row 8", facts: func(f *sandbox.RunwayFacts) {
			f.DeadlineKnown, f.Runway, f.SinceLastSeen = false, -time.Minute, 2*time.Minute
		}, wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonDeadlineUnknown},
		{name: "row 2 reads the heartbeat budget, not another wait", facts: func(f *sandbox.RunwayFacts) { f.Runway, f.SinceLastSeen = -time.Minute, 95*time.Second },
			cfg: &distinctConfig, wantAction: sandbox.RunwayRotate, wantReason: sandbox.RunwayReasonBelowThreshold, wantNextLook: 2 * time.Minute},

		// Past the deadline, the agent heard from: an estimate the gen
		// outlives. Nothing in progress is cut short; it is rotated
		// through its own snapshot (row 14), its holds bounded by their
		// own waits.
		{name: "past the deadline, heard from: rotated, not retired", facts: func(f *sandbox.RunwayFacts) { f.Runway, f.SinceLastSeen = -30*time.Second, 0 },
			wantAction: sandbox.RunwayRotate, wantReason: sandbox.RunwayReasonBelowThreshold, wantNextLook: 2 * time.Minute},
		{name: "past the deadline, silent a nanosecond short of the budget: rotated", facts: func(f *sandbox.RunwayFacts) { f.Runway, f.SinceLastSeen = -time.Minute, 90*time.Second-1 },
			wantAction: sandbox.RunwayRotate, wantReason: sandbox.RunwayReasonBelowThreshold, wantNextLook: 2 * time.Minute},
		{name: "past the deadline, heard from, a delivery open: held for the delivery", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.DeliveryOpen, f.DeliveryRemaining = -30*time.Second, true, 9*time.Minute
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonDeliveryOpen, wantNextLook: 9 * time.Minute},
		{name: "past the deadline, heard from, a post-turn snapshot in progress: held for its report", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.SinceSnapshotStart = sandbox.StateSnapshotting, -30*time.Second, 10*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSnapshotInProgress, wantNextLook: 110 * time.Second},
		{name: "past the deadline, heard from, the rotation's snapshot awaited", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.RotationSnapshotPending, f.SinceRotationStart = sandbox.StateSnapshotting, -30*time.Second, true, true, 30*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingRotationSnapshot, wantNextLook: 90 * time.Second},
		{name: "past the deadline, heard from, no snapshot capability: declined", facts: func(f *sandbox.RunwayFacts) { f.Runway, f.CanSnapshot = -time.Minute, false },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonNoSnapshot},
		{name: "past the deadline, heard from, a turn in flight: declined", facts: func(f *sandbox.RunwayFacts) { f.Runway, f.TurnInFlight = -time.Minute, true },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonTurnInFlight},

		// Row 3: the rotation's own snapshot is awaited.
		{name: "row 3: within the wait", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.RotationSnapshotPending, f.SinceRotationStart = sandbox.StateSnapshotting, 10*time.Minute, true, true, 30*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingRotationSnapshot, wantNextLook: 90 * time.Second},
		{name: "row 3: just started", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.RotationSnapshotPending, f.SinceRotationStart = sandbox.StateSnapshotting, 10*time.Minute, true, true, 0
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingRotationSnapshot, wantNextLook: 2 * time.Minute},
		{name: "row 3: the deadline comes before the wait ends", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.RotationSnapshotPending, f.SinceRotationStart = sandbox.StateSnapshotting, time.Minute, true, true, 30*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingRotationSnapshot, wantNextLook: time.Minute},
		{name: "row 3: no deadline known, the wait alone", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.DeadlineKnown, f.Runway, f.Rotating, f.RotationSnapshotPending, f.SinceRotationStart = sandbox.StateSnapshotting, false, 10*time.Second, true, true, 30*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingRotationSnapshot, wantNextLook: 90 * time.Second},
		{name: "row 3 uses the rotation's wait, not the report wait", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Rotating, f.RotationSnapshotPending, f.SinceRotationStart = sandbox.StateSnapshotting, true, true, 30*time.Second
		}, cfg: &distinctConfig, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingRotationSnapshot, wantNextLook: 90 * time.Second},

		// Row 4: the rotation's own snapshot was never reported.
		{name: "row 4: exactly at the wait", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.RotationSnapshotPending, f.SinceRotationStart = sandbox.StateSnapshotting, 10*time.Minute, true, true, 2*time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonRotationSnapshotNotReported},
		{name: "row 4: long past the wait", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.RotationSnapshotPending, f.SinceRotationStart = sandbox.StateSnapshotting, 5*time.Minute, true, true, 10*time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonRotationSnapshotNotReported},
		{name: "row 4 needs its own snapshot: another snapshot of a rotating gen is waited for", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.RotationSnapshotTaken, f.SinceRotationStart, f.SinceSnapshotStart = sandbox.StateSnapshotting, 8*time.Minute, true, true, 4*time.Minute, 0
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSnapshotInProgress, wantNextLook: 2 * time.Minute},

		// Row 5: the rotation's snapshot was recorded.
		{name: "row 5: ready", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.Rotating, f.SinceRotationStart, f.RotationSnapshotTaken = 10*time.Minute, true, time.Minute, true
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonRotationSnapshotTaken},
		{name: "row 5: suspect since", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.RotationSnapshotTaken = sandbox.StateSuspect, 10*time.Minute, true, true
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonRotationSnapshotTaken},

		// Row 6: rotating, back to ready or suspect, its snapshot not recorded.
		{name: "row 6: ready (the snapshot command could not be sent)", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.Rotating = 10*time.Minute, true
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonRotationSnapshotFailed},
		{name: "row 6: suspect", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating = sandbox.StateSuspect, 10*time.Minute, true
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonRotationSnapshotFailed},
		{name: "row 6 precedes the runway: rotating with an hour left", facts: func(f *sandbox.RunwayFacts) { f.Rotating = true },
			wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonRotationSnapshotFailed},

		// Row 7: a snapshot that is not a rotation's own is in progress,
		// within its report wait.
		{name: "row 7: just started, an hour left", facts: func(f *sandbox.RunwayFacts) { f.Status = sandbox.StateSnapshotting },
			wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSnapshotInProgress, wantNextLook: 2 * time.Minute},
		{name: "row 7: within the wait", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SinceSnapshotStart = sandbox.StateSnapshotting, 30*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSnapshotInProgress, wantNextLook: 90 * time.Second},
		{name: "row 7: below the threshold, never rotated, the deadline comes first", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.SinceSnapshotStart = sandbox.StateSnapshotting, time.Minute, 30*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSnapshotInProgress, wantNextLook: time.Minute},
		{name: "row 7: no deadline known, the wait alone", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.DeadlineKnown, f.Runway, f.SinceSnapshotStart = sandbox.StateSnapshotting, false, 10*time.Second, 30*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSnapshotInProgress, wantNextLook: 90 * time.Second},
		{name: "row 7 within the wait, its agent silent: the wait comes first", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, time.Minute, time.Hour
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSnapshotInProgress, wantNextLook: time.Minute},
		{name: "row 7 uses the report wait, not the rotation's", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SinceSnapshotStart = sandbox.StateSnapshotting, 2*time.Minute
		}, cfg: &distinctConfig, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSnapshotInProgress, wantNextLook: time.Minute},

		// Row 7a: the wait has run and the agent is silent past the
		// heartbeat budget: suspected, through the liveness path.
		{name: "row 7a: exactly at the wait, silent exactly the budget", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, 2*time.Minute, 90*time.Second
		}, wantAction: sandbox.RunwaySuspect, wantReason: sandbox.RunwayReasonSnapshotAgentSilent},
		{name: "row 7a: no deadline known", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.DeadlineKnown, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, false, time.Hour, 3*time.Hour
		}, wantAction: sandbox.RunwaySuspect, wantReason: sandbox.RunwayReasonSnapshotAgentSilent},
		{name: "row 7a reads the heartbeat budget: silent 95s, under a 100s budget, is not silent", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, 200*time.Second, 95*time.Second
		}, cfg: &distinctConfig, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingSignOfLife, wantNextLook: 5 * time.Second},
		{name: "row 7a reads the heartbeat budget: silent 100s is silent", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, 200*time.Second, 100*time.Second
		}, cfg: &distinctConfig, wantAction: sandbox.RunwaySuspect, wantReason: sandbox.RunwayReasonSnapshotAgentSilent},
		{name: "row 7a follows row 4: a rotation's own snapshot is the rotation's", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Rotating, f.RotationSnapshotPending, f.SinceRotationStart, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, true, true, 3*time.Minute, 3*time.Minute, time.Hour
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonRotationSnapshotNotReported},

		// Row 7b: the wait has run and the agent was heard from after it
		// ended: the snapshot alone failed, and the sandbox goes back to
		// ready.
		{name: "row 7b: heard from just now, the wait a minute past", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SinceSnapshotStart = sandbox.StateSnapshotting, 3*time.Minute
		}, wantAction: sandbox.RunwayRevert, wantReason: sandbox.RunwayReasonSnapshotNotReported},
		{name: "row 7b: heard from a nanosecond after the wait ended", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, 3*time.Minute, time.Minute-1
		}, wantAction: sandbox.RunwayRevert, wantReason: sandbox.RunwayReasonSnapshotNotReported},
		{name: "row 7b needs a sign of life after the wait: one exactly at it is 7c", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, 3*time.Minute, time.Minute
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingSignOfLife, wantNextLook: 30 * time.Second},
		{name: "row 7b: below the threshold, past the wait, heard from since", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, 5*time.Minute, 10*time.Minute, 10*time.Second
		}, wantAction: sandbox.RunwayRevert, wantReason: sandbox.RunwayReasonSnapshotNotReported},
		{name: "row 7b: past the deadline, heard from since the wait", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, -time.Second, 10*time.Minute, 5*time.Second
		}, wantAction: sandbox.RunwayRevert, wantReason: sandbox.RunwayReasonSnapshotNotReported},

		// Row 7c: the wait has run and the agent has not been heard from
		// since: one that died partway through the wait is not read as
		// alive. Held until it is heard from or its silence reaches the
		// budget.
		{name: "row 7c: at the bound, last heard exactly at it", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SinceSnapshotStart = sandbox.StateSnapshotting, 2*time.Minute
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingSignOfLife, wantNextLook: 90 * time.Second},
		{name: "row 7c: the agent died 60s into the wait", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, 121*time.Second, 61*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingSignOfLife, wantNextLook: 29 * time.Second},
		{name: "row 7c: the agent died 100s into the wait", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, 121*time.Second, 21*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingSignOfLife, wantNextLook: 69 * time.Second},
		{name: "row 7c: bounded by a deadline ahead", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.SinceSnapshotStart, f.SinceLastSeen = sandbox.StateSnapshotting, 10*time.Second, 3*time.Minute, 70*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingSignOfLife, wantNextLook: 10 * time.Second},

		// Row 7d: suspected while snapshotting: a recovery takes it back
		// to snapshotting, so it takes no prompt; held until the grace
		// decides.
		{name: "row 7d: suspected mid-snapshot", facts: func(f *sandbox.RunwayFacts) { f.Status, f.SuspectedMidSnapshot = sandbox.StateSuspect, true },
			wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSuspectedMidSnapshot, wantNextLook: time.Minute},
		{name: "row 7d reads the grace", facts: func(f *sandbox.RunwayFacts) { f.Status, f.SuspectedMidSnapshot = sandbox.StateSuspect, true },
			cfg: &distinctConfig, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSuspectedMidSnapshot, wantNextLook: 70 * time.Second},
		{name: "row 2 precedes row 7d: past the deadline, silent", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SuspectedMidSnapshot, f.Runway, f.SinceLastSeen = sandbox.StateSuspect, true, -time.Minute, 3*time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 7d before rows 5 and 6: a rotating gen suspected mid-snapshot keeps its grace", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.SuspectedMidSnapshot, f.Rotating, f.RotationSnapshotTaken, f.SinceRotationStart = sandbox.StateSuspect, true, true, true, 5*time.Minute
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSuspectedMidSnapshot, wantNextLook: time.Minute},
		{name: "row 6: a rotating gen suspected while ready is retired", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Rotating, f.SinceRotationStart = sandbox.StateSuspect, true, 5*time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonRotationSnapshotFailed},
		{name: "row 7d needs suspect: a ready sandbox's flag is stale", facts: func(f *sandbox.RunwayFacts) { f.SuspectedMidSnapshot = true },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonOK},

		// Row 8: no deadline known, declined.
		{name: "row 8: ready", facts: func(f *sandbox.RunwayFacts) { f.DeadlineKnown, f.Runway = false, time.Second },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonDeadlineUnknown},
		{name: "row 8: suspect", facts: func(f *sandbox.RunwayFacts) { f.Status, f.DeadlineKnown = sandbox.StateSuspect, false },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonDeadlineUnknown},

		// Row 9: enough runway.
		{name: "row 9: the runway exactly the threshold passes", facts: func(f *sandbox.RunwayFacts) { f.Runway = threshold },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonOK},
		{name: "row 9: a nanosecond above", facts: func(f *sandbox.RunwayFacts) { f.Runway = threshold + 1 },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonOK},
		{name: "row 9: suspect with an hour left", facts: func(f *sandbox.RunwayFacts) { f.Status = sandbox.StateSuspect },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonOK},
		{name: "row 9: a short lifetime's sixth binds", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.Lifetime = 11*time.Minute, time.Hour
		}, wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonOK, wantThreshold: 10 * time.Minute},
		{name: "row 9 needs a lifetime to cap the floor: none, so the floor", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.Lifetime = 11*time.Minute, 0
		}, wantAction: sandbox.RunwayRotate, wantReason: sandbox.RunwayReasonBelowThreshold, wantNextLook: 2 * time.Minute},

		// Row 10: a turn in flight, declined.
		{name: "row 10: a re-send below the threshold", facts: func(f *sandbox.RunwayFacts) { f.Runway, f.TurnInFlight = 5*time.Minute, true },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonTurnInFlight},
		{name: "row 10 precedes row 11", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.TurnInFlight = sandbox.StateSuspect, 5*time.Minute, true
		}, wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonTurnInFlight},
		{name: "row 10 precedes row 13", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.TurnInFlight, f.DeliveryOpen, f.DeliveryRemaining = 5*time.Minute, true, true, 3*time.Minute
		}, wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonTurnInFlight},

		// Row 11: suspect, declined: no suspect -> snapshotting edge.
		{name: "row 11: suspect below the threshold", facts: func(f *sandbox.RunwayFacts) { f.Status, f.Runway = sandbox.StateSuspect, 5*time.Minute },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonSuspect},
		{name: "row 11 precedes row 12: suspect, below the threshold, no snapshot capability", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.CanSnapshot = sandbox.StateSuspect, 5*time.Minute, false
		}, wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonSuspect},

		// Row 12: no snapshot capability, declined.
		{name: "row 12: below the threshold", facts: func(f *sandbox.RunwayFacts) { f.Runway, f.CanSnapshot = 5*time.Minute, false },
			wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonNoSnapshot},
		{name: "row 12 precedes row 13", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.CanSnapshot, f.DeliveryOpen, f.DeliveryRemaining = 5*time.Minute, false, true, 3*time.Minute
		}, wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonNoSnapshot},

		// Row 13: a delivery is open; the rotation waits for its end.
		{name: "row 13: the delivery ends first", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.DeliveryOpen, f.DeliveryRemaining = 10*time.Minute, true, 3*time.Minute
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonDeliveryOpen, wantNextLook: 3 * time.Minute},
		{name: "row 13: the deadline comes first", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.DeliveryOpen, f.DeliveryRemaining = 2*time.Minute, true, 3*time.Minute
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonDeliveryOpen, wantNextLook: 2 * time.Minute},
		{name: "row 13 follows row 9: a delivery never holds a dispatch with runway", facts: func(f *sandbox.RunwayFacts) {
			f.DeliveryOpen, f.DeliveryRemaining = true, 3*time.Minute
		}, wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonOK},

		// Row 14: ready, below the threshold: rotate.
		{name: "row 14: a nanosecond below the threshold", facts: func(f *sandbox.RunwayFacts) { f.Runway = threshold - 1 },
			wantAction: sandbox.RunwayRotate, wantReason: sandbox.RunwayReasonBelowThreshold, wantNextLook: 2 * time.Minute},
		{name: "row 14: a nanosecond left", facts: func(f *sandbox.RunwayFacts) { f.Runway = 1 },
			wantAction: sandbox.RunwayRotate, wantReason: sandbox.RunwayReasonBelowThreshold, wantNextLook: 2 * time.Minute},
		{name: "row 14: the wait is the configured one", facts: func(f *sandbox.RunwayFacts) { f.Runway = 5 * time.Minute },
			cfg:        &sandbox.RunwayConfig{Floor: threshold, SnapshotWait: 3 * time.Minute, ReportWait: 2 * time.Minute, HeartbeatBudget: 90 * time.Second},
			wantAction: sandbox.RunwayRotate, wantReason: sandbox.RunwayReasonBelowThreshold, wantNextLook: 3 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			facts := healthyRunway()
			tc.facts(&facts)
			cfg := runwayConfig
			if tc.cfg != nil {
				cfg = *tc.cfg
			}
			wantThreshold := tc.wantThreshold
			if wantThreshold == 0 {
				wantThreshold = threshold
			}
			want := sandbox.RunwayDecision{Action: tc.wantAction, Reason: tc.wantReason, Threshold: wantThreshold, NextLook: tc.wantNextLook}
			if got := sandbox.EvaluateRunway(facts, cfg); got != want {
				t.Errorf("EvaluateRunway(%+v) = %+v, want %+v", facts, got, want)
			}
		})
	}
}

// runwaySweep calls visit with every combination of the gate's facts over
// a grid that straddles each boundary of its table: every state, the
// deadline known or not, runways on either side of zero and the
// threshold, rotation and snapshot ages on either side of the waits, an
// agent heard from just now, between heartbeats, or silent past the
// budget, and every boolean.
func runwaySweep(visit func(sandbox.RunwayFacts)) {
	runways := []time.Duration{-time.Hour, -1, 0, 1, time.Minute, 13*time.Minute - 1, 13 * time.Minute, time.Hour}
	sinces := []time.Duration{0, time.Minute, 2*time.Minute - 1, 2 * time.Minute, time.Hour}
	silences := []time.Duration{0, 30 * time.Second, 90 * time.Second}
	lifetimes := []time.Duration{0, 2 * time.Hour}
	bools := []bool{false, true}
	for _, status := range allStates {
		for _, known := range bools {
			for _, runway := range runways {
				for _, lifetime := range lifetimes {
					for _, inFlight := range bools {
						for _, canSnapshot := range bools {
							for _, rotating := range bools {
								for _, own := range bools {
									for _, since := range sinces {
										for _, taken := range bools {
											for _, delivery := range bools {
												for _, silence := range silences {
													for _, midSnapshot := range bools {
														visit(sandbox.RunwayFacts{
															Status: status, DeadlineKnown: known, Runway: runway, Lifetime: lifetime,
															TurnInFlight: inFlight, CanSnapshot: canSnapshot,
															Rotating: rotating, RotationSnapshotPending: own, SinceRotationStart: since, RotationSnapshotTaken: taken,
															SinceSnapshotStart: since, SinceLastSeen: silence, SuspectedMidSnapshot: midSnapshot,
															DeliveryOpen: delivery, DeliveryRemaining: 3 * time.Minute,
														})
													}
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

// TestEvaluateRunway_Properties sweeps every combination of facts and pins
// the table's properties (technical plan §35.3) and that each action has
// its edges in the machine's transition table (state.go), which the
// caller then takes:
//
//   - a snapshotting sandbox, or one suspected mid-snapshot, is never
//     passed, so no prompt goes to a sandbox mid-snapshot, and never
//     rotated, as it is snapshotting already;
//   - a known deadline that has passed retires a live sandbox at once when
//     its agent is silent, and never by the deadline row when it is heard
//     from: a gen that outlives its estimate keeps its snapshot in
//     progress and its delivery;
//   - every retirement is of a rotating gen or of one past its deadline,
//     the two a resume is ruled out for;
//   - nothing is rotated with a turn in flight;
//   - a rotation starts only from ready, over ready -> snapshotting;
//   - a retirement starts only from a state the machine can take to
//     stopped, through suspect;
//   - a suspicion starts only from snapshotting, over snapshotting ->
//     suspect, once the report wait has run, with the agent silent, and
//     never of a rotation's own snapshot;
//   - a revert starts only from snapshotting, over snapshotting -> ready,
//     once the report wait has run and the agent was heard from after it;
//   - a passed dispatch has at least the threshold of runway, unless the
//     gate declined;
//   - every hold or rotation names a positive next look: a hold always
//     ends, a snapshot that never reports included.
func TestEvaluateRunway_Properties(t *testing.T) {
	t.Parallel()

	declined := map[sandbox.RunwayReason]bool{
		sandbox.RunwayReasonNotSteady: true, sandbox.RunwayReasonDeadlineUnknown: true,
		sandbox.RunwayReasonTurnInFlight: true, sandbox.RunwayReasonSuspect: true, sandbox.RunwayReasonNoSnapshot: true,
	}
	live := map[sandbox.State]bool{sandbox.StateReady: true, sandbox.StateSuspect: true, sandbox.StateSnapshotting: true}
	visited := 0
	runwaySweep(func(f sandbox.RunwayFacts) {
		visited++
		d := sandbox.EvaluateRunway(f, runwayConfig)
		fail := func(format string, args ...any) {
			t.Helper()
			t.Errorf("EvaluateRunway(%+v) = %+v: %s", f, d, fmt.Sprintf(format, args...))
		}
		silent := f.SinceLastSeen >= runwayConfig.HeartbeatBudget
		pastDeadline := f.DeadlineKnown && f.Runway <= 0
		midSnapshot := f.Status == sandbox.StateSnapshotting || (f.Status == sandbox.StateSuspect && f.SuspectedMidSnapshot)
		if d.Threshold != sandbox.RotationThreshold(runwayConfig.Floor, f.Lifetime) {
			fail("threshold is not RotationThreshold(floor, lifetime)")
		}
		if midSnapshot && (d.Action == sandbox.RunwayPass || d.Action == sandbox.RunwayRotate) {
			fail("a sandbox mid-snapshot is only held, retired, suspected or reverted")
		}
		if f.Status == sandbox.StateSuspect && f.SuspectedMidSnapshot && d.Action == sandbox.RunwayRetire && d.Reason != sandbox.RunwayReasonDeadlinePassed {
			fail("a suspicion's grace cut short by a rotation row")
		}
		if live[f.Status] && pastDeadline && silent && (d.Action != sandbox.RunwayRetire || d.Reason != sandbox.RunwayReasonDeadlinePassed) {
			fail("a live sandbox past its deadline with its agent silent is retired as such")
		}
		if d.Reason == sandbox.RunwayReasonDeadlinePassed && !silent {
			fail("retired at its deadline with its agent heard from")
		}
		if d.Action == sandbox.RunwayRetire && !f.Rotating && !pastDeadline {
			fail("retired a gen neither rotating nor past its deadline: a resume is not ruled out for it")
		}
		if f.TurnInFlight && d.Action == sandbox.RunwayRotate {
			fail("rotated with a turn in flight")
		}
		switch d.Action {
		case sandbox.RunwayRotate:
			if _, err := sandbox.Transition(f.Status, 1, sandbox.SnapshotStartTrigger()); err != nil || f.Status != sandbox.StateReady {
				fail("a rotation starts from %s: %v", f.Status, err)
			}
			if d.NextLook != runwayConfig.SnapshotWait {
				fail("a rotation looks again after the snapshot wait")
			}
		case sandbox.RunwayRetire:
			from := f.Status
			if from != sandbox.StateSuspect {
				var err error
				if from, err = sandbox.Transition(from, 1, sandbox.SuspectTrigger()); err != nil {
					fail("no edge to suspect: %v", err)
					return
				}
			}
			if _, err := sandbox.Transition(from, 1, sandbox.GraceExpiredTrigger(sandbox.StateStopped)); err != nil {
				fail("no edge to stopped: %v", err)
			}
		case sandbox.RunwaySuspect:
			if _, err := sandbox.Transition(f.Status, 1, sandbox.SuspectTrigger()); err != nil || f.Status != sandbox.StateSnapshotting {
				fail("a suspicion starts from %s: %v", f.Status, err)
			}
			if !silent || f.SinceSnapshotStart < runwayConfig.ReportWait || (f.Rotating && f.RotationSnapshotPending) {
				fail("suspected within the report wait, with its agent heard from, or a rotation's own snapshot")
			}
		case sandbox.RunwayRevert:
			if _, err := sandbox.Transition(f.Status, 1, sandbox.SnapshotCompleteTrigger()); err != nil || f.Status != sandbox.StateSnapshotting {
				fail("a revert starts from %s: %v", f.Status, err)
			}
			if (f.Rotating && f.RotationSnapshotPending) || f.SinceSnapshotStart < runwayConfig.ReportWait || silent ||
				f.SinceLastSeen >= f.SinceSnapshotStart-runwayConfig.ReportWait {
				fail("reverted a rotation's own snapshot, within the report wait, or with its agent not heard from since the wait")
			}
			if d.NextLook != 0 {
				fail("a revert names no next look")
			}
		case sandbox.RunwayPass:
			if !declined[d.Reason] && (!f.DeadlineKnown || f.Runway < d.Threshold) {
				fail("passed with less than the threshold, not declined")
			}
			if d.NextLook != 0 {
				fail("a pass names no next look")
			}
		case sandbox.RunwayHold:
			if d.NextLook <= 0 {
				fail("a hold looks again after a positive delay")
			}
		default:
			fail("unknown action")
		}
	})
	if visited < 500_000 {
		t.Fatalf("swept %d combinations: the sweep is broken", visited)
	}
}

// TestEvaluateRunway_RaisingTheFloorBringsTheRotationForward is the
// amended monotonicity test (row 137): a floor the gate read but never
// compared would pass every other test of its value, so raising it must
// actually bring the rotation forward. For floors f1 < f2 below a sixth of
// the lifetime, a sandbox rotated at f1 is rotated at f2 whatever else is
// true of it, and every runway in [f1, f2) passes at f1 and rotates at f2.
func TestEvaluateRunway_RaisingTheFloorBringsTheRotationForward(t *testing.T) {
	t.Parallel()

	const lifetime = 2 * time.Hour
	floors := []time.Duration{time.Minute, 5 * time.Minute, 10 * time.Minute, 13 * time.Minute, 15 * time.Minute, 19 * time.Minute, lifetime/6 - 1}
	runways := []time.Duration{1}
	for r := 30 * time.Second; r <= 25*time.Minute; r += 30 * time.Second {
		runways = append(runways, r)
	}
	for _, f := range floors {
		runways = append(runways, f-1, f, f+1)
	}
	bools := []bool{false, true}

	for i, f1 := range floors {
		for _, f2 := range floors[i+1:] {
			t.Run(fmt.Sprintf("%v to %v", f1, f2), func(t *testing.T) {
				t.Parallel()
				if f1 >= f2 || f2 >= lifetime/6 {
					t.Fatalf("floors %v, %v: want f1 < f2 < lifetime/6 (%v)", f1, f2, lifetime/6)
				}
				low, high := runwayConfig, runwayConfig
				low.Floor, high.Floor = f1, f2
				inGap := 0
				for _, runway := range runways {
					// The implication, over every fact a rotation can depend on.
					for _, status := range []sandbox.State{sandbox.StateReady, sandbox.StateSuspect, sandbox.StateSnapshotting} {
						for _, known := range bools {
							for _, inFlight := range bools {
								for _, canSnapshot := range bools {
									for _, delivery := range bools {
										facts := sandbox.RunwayFacts{
											Status: status, DeadlineKnown: known, Runway: runway, Lifetime: lifetime,
											TurnInFlight: inFlight, CanSnapshot: canSnapshot,
											DeliveryOpen: delivery, DeliveryRemaining: 3 * time.Minute,
										}
										atLow, atHigh := sandbox.EvaluateRunway(facts, low), sandbox.EvaluateRunway(facts, high)
										if atLow.Action == sandbox.RunwayRotate && atHigh.Action != sandbox.RunwayRotate {
											t.Errorf("%+v: rotated at floor %v (%+v), not at the higher %v (%+v)", facts, f1, atLow, f2, atHigh)
										}
									}
								}
							}
						}
					}
					if runway < f1 || runway >= f2 {
						continue
					}
					// Brought forward: a runway in [f1, f2) passes at f1 and rotates at f2.
					inGap++
					facts := healthyRunway()
					facts.Lifetime, facts.Runway = lifetime, runway
					if got := sandbox.EvaluateRunway(facts, low); got.Action != sandbox.RunwayPass || got.Reason != sandbox.RunwayReasonOK {
						t.Errorf("runway %v at floor %v = %+v, want pass (runway_ok)", runway, f1, got)
					}
					if got := sandbox.EvaluateRunway(facts, high); got.Action != sandbox.RunwayRotate {
						t.Errorf("runway %v at floor %v = %+v, want rotate: raising the floor did not bring the rotation forward", runway, f2, got)
					}
				}
				if inGap == 0 {
					t.Fatalf("no runway sampled in [%v, %v): the sweep is broken", f1, f2)
				}
			})
		}
	}
}

// TestEvaluateRunway_APushPastItsBoundFallsBackBeforeTheDeadline pins the
// residual platform.Timeouts.RotationRunwayFloor's doc states. The
// delivery hold ends with the delivery's bound, not with the push, so a
// push that outlasts the bound (several slow repositories) delays the
// rotation's snapshot until it misses the wait. Following each decision's
// NextLook from the first evaluation below the threshold, with any part of
// the bound still ahead, the gen is retired as
// rotation_snapshot_not_reported -- the fallback to the previous snapshot
// -- with runway left, never by the deadline, within the bound and the
// wait. Values are the shipped ones: floor 13m, wait 2m, bound 10m.
func TestEvaluateRunway_APushPastItsBoundFallsBackBeforeTheDeadline(t *testing.T) {
	t.Parallel()

	const deliveryBound = 10 * time.Minute
	for _, deliveryLeft := range []time.Duration{0, time.Minute, 5 * time.Minute, deliveryBound} {
		t.Run(fmt.Sprintf("%v of the bound left", deliveryLeft), func(t *testing.T) {
			t.Parallel()
			facts := healthyRunway()
			facts.Runway = runwayConfig.Floor - 1 // the first evaluation below the threshold
			var elapsed, rotationStart time.Duration
			for range 10 {
				facts.DeliveryOpen = deliveryLeft > elapsed
				facts.DeliveryRemaining = deliveryLeft - elapsed
				if facts.Rotating {
					facts.SinceRotationStart = elapsed - rotationStart
				}
				d := sandbox.EvaluateRunway(facts, runwayConfig)
				switch d.Action {
				case sandbox.RunwayRetire:
					if d.Reason != sandbox.RunwayReasonRotationSnapshotNotReported || facts.Runway <= 0 {
						t.Fatalf("after %v: %+v with %v of runway, want the rotation's fallback with runway left", elapsed, d, facts.Runway)
					}
					if elapsed > deliveryLeft+runwayConfig.SnapshotWait {
						t.Fatalf("fell back after %v, want within the bound left and the wait (%v)", elapsed, deliveryLeft+runwayConfig.SnapshotWait)
					}
					return
				case sandbox.RunwayRotate:
					facts.Status, facts.Rotating, facts.RotationSnapshotPending, rotationStart = sandbox.StateSnapshotting, true, true, elapsed
				case sandbox.RunwayHold:
				default:
					t.Fatalf("after %v: %+v, want hold, rotate or retire", elapsed, d)
				}
				if d.NextLook <= 0 {
					t.Fatalf("after %v: %+v names no next look: nothing would end the hold", elapsed, d)
				}
				elapsed += d.NextLook
				facts.Runway -= d.NextLook
			}
			t.Fatal("no retirement after 10 evaluations")
		})
	}
}
