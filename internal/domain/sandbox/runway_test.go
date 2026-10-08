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
// RotationRunwayFloor 13m, RotationSnapshotWait 2m), restated here because
// a domain test does not import platform.
var runwayConfig = sandbox.RunwayConfig{Floor: 13 * time.Minute, SnapshotWait: 2 * time.Minute}

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

		// Row 2: the deadline has passed.
		{name: "row 2: ready, runway exactly zero", facts: func(f *sandbox.RunwayFacts) { f.Runway = 0 },
			wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2: ready, a nanosecond past", facts: func(f *sandbox.RunwayFacts) { f.Runway = -1 },
			wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2: suspect", facts: func(f *sandbox.RunwayFacts) { f.Status, f.Runway = sandbox.StateSuspect, -time.Minute },
			wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2: snapshotting, not rotating", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway = sandbox.StateSnapshotting, -time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2 precedes row 3: the deadline passes while the rotation waits", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.SinceRotationStart = sandbox.StateSnapshotting, 0, true, 30*time.Second
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2 precedes row 5: the rotation's snapshot taken, the deadline passed", facts: func(f *sandbox.RunwayFacts) {
			f.Runway, f.Rotating, f.RotationSnapshotTaken = -time.Second, true, true
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2: with a turn in flight", facts: func(f *sandbox.RunwayFacts) { f.Runway, f.TurnInFlight = -time.Minute, true },
			wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2: with no snapshot capability", facts: func(f *sandbox.RunwayFacts) { f.Runway, f.CanSnapshot = -time.Minute, false },
			wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonDeadlinePassed},
		{name: "row 2 needs the deadline known: a stale runway is row 8", facts: func(f *sandbox.RunwayFacts) {
			f.DeadlineKnown, f.Runway = false, -time.Minute
		}, wantAction: sandbox.RunwayPass, wantReason: sandbox.RunwayReasonDeadlineUnknown},

		// Row 3: the rotation's snapshot is awaited.
		{name: "row 3: within the wait", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.SinceRotationStart = sandbox.StateSnapshotting, 10*time.Minute, true, 30*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingRotationSnapshot, wantNextLook: 90 * time.Second},
		{name: "row 3: just started", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.SinceRotationStart = sandbox.StateSnapshotting, 10*time.Minute, true, 0
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingRotationSnapshot, wantNextLook: 2 * time.Minute},
		{name: "row 3: the deadline comes before the wait ends", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.SinceRotationStart = sandbox.StateSnapshotting, time.Minute, true, 30*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingRotationSnapshot, wantNextLook: time.Minute},
		{name: "row 3: no deadline known, the wait alone", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.DeadlineKnown, f.Runway, f.Rotating, f.SinceRotationStart = sandbox.StateSnapshotting, false, 10*time.Second, true, 30*time.Second
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonAwaitingRotationSnapshot, wantNextLook: 90 * time.Second},

		// Row 4: the rotation's snapshot was never reported.
		{name: "row 4: exactly at the wait", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.SinceRotationStart = sandbox.StateSnapshotting, 10*time.Minute, true, 2*time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonRotationSnapshotNotReported},
		{name: "row 4: long past the wait", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway, f.Rotating, f.SinceRotationStart = sandbox.StateSnapshotting, 5*time.Minute, true, 10*time.Minute
		}, wantAction: sandbox.RunwayRetire, wantReason: sandbox.RunwayReasonRotationSnapshotNotReported},

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

		// Row 7: a snapshot that is not a rotation's is in progress.
		{name: "row 7: an hour left", facts: func(f *sandbox.RunwayFacts) { f.Status = sandbox.StateSnapshotting },
			wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSnapshotInProgress, wantNextLook: time.Hour},
		{name: "row 7: below the threshold, never rotated", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.Runway = sandbox.StateSnapshotting, 5*time.Minute
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSnapshotInProgress, wantNextLook: 5 * time.Minute},
		{name: "row 7: no deadline known, only the snapshot's report ends it", facts: func(f *sandbox.RunwayFacts) {
			f.Status, f.DeadlineKnown, f.Runway = sandbox.StateSnapshotting, false, 5*time.Minute
		}, wantAction: sandbox.RunwayHold, wantReason: sandbox.RunwayReasonSnapshotInProgress},

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
			cfg:        &sandbox.RunwayConfig{Floor: threshold, SnapshotWait: 3 * time.Minute},
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
// threshold, rotation ages on either side of the wait, and every boolean.
func runwaySweep(visit func(sandbox.RunwayFacts)) {
	runways := []time.Duration{-time.Hour, -1, 0, 1, time.Minute, 13*time.Minute - 1, 13 * time.Minute, time.Hour}
	sinces := []time.Duration{0, time.Minute, 2*time.Minute - 1, 2 * time.Minute, time.Hour}
	lifetimes := []time.Duration{0, time.Hour, 2 * time.Hour}
	bools := []bool{false, true}
	for _, status := range allStates {
		for _, known := range bools {
			for _, runway := range runways {
				for _, lifetime := range lifetimes {
					for _, inFlight := range bools {
						for _, canSnapshot := range bools {
							for _, rotating := range bools {
								for _, since := range sinces {
									for _, taken := range bools {
										for _, delivery := range bools {
											visit(sandbox.RunwayFacts{
												Status: status, DeadlineKnown: known, Runway: runway, Lifetime: lifetime,
												TurnInFlight: inFlight, CanSnapshot: canSnapshot,
												Rotating: rotating, SinceRotationStart: since, RotationSnapshotTaken: taken,
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

// TestEvaluateRunway_Properties sweeps every combination of facts and pins
// the table's properties (technical plan §35.3) and that each action has
// its edges in the machine's transition table (state.go), which the
// caller then takes:
//
//   - a snapshotting sandbox is never passed, so no prompt goes to a
//     sandbox mid-snapshot, and never rotated, as it is snapshotting
//     already;
//   - a known deadline that has passed always retires a live sandbox,
//     whatever else is true of it, rotating included;
//   - nothing is rotated with a turn in flight;
//   - a rotation starts only from ready, over ready -> snapshotting;
//   - a retirement starts only from a state the machine can take to
//     stopped, through suspect;
//   - a passed dispatch has at least the threshold of runway, unless the
//     gate declined;
//   - every hold or rotation names when to look again, unless only an
//     event can end it: a snapshot with no deadline known.
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
		if d.Threshold != sandbox.RotationThreshold(runwayConfig.Floor, f.Lifetime) {
			fail("threshold is not RotationThreshold(floor, lifetime)")
		}
		if f.Status == sandbox.StateSnapshotting && (d.Action == sandbox.RunwayPass || d.Action == sandbox.RunwayRotate) {
			fail("a snapshotting sandbox is only held or retired")
		}
		if live[f.Status] && f.DeadlineKnown && f.Runway <= 0 && (d.Action != sandbox.RunwayRetire || d.Reason != sandbox.RunwayReasonDeadlinePassed) {
			fail("a live sandbox past its deadline is retired as such")
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
		case sandbox.RunwayPass:
			if !declined[d.Reason] && (!f.DeadlineKnown || f.Runway < d.Threshold) {
				fail("passed with less than the threshold, not declined")
			}
			if d.NextLook != 0 {
				fail("a pass names no next look")
			}
		case sandbox.RunwayHold:
			eventOnly := d.Reason == sandbox.RunwayReasonSnapshotInProgress && !f.DeadlineKnown
			if (d.NextLook <= 0) != eventOnly {
				fail("a hold looks again after a positive delay unless only an event ends it")
			}
		default:
			fail("unknown action")
		}
	})
	if visited < 50_000 {
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
				low := sandbox.RunwayConfig{Floor: f1, SnapshotWait: runwayConfig.SnapshotWait}
				high := sandbox.RunwayConfig{Floor: f2, SnapshotWait: runwayConfig.SnapshotWait}
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
