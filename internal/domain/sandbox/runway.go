package sandbox

import "time"

// The pre-dispatch runway gate (technical plan §35.3): a turn is never
// dispatched into a live sandbox with less than the rotation threshold of
// its lifetime left. Below it the sandbox rotates first -- snapshot,
// stopped, shutdown, restore, over the machine's existing edges -- and the
// turn stays pending for the replacement. A rotation the gate cannot make
// (no deadline known, a turn in flight, a suspect sandbox, no snapshot
// capability) is declined, and the turn is dispatched as before, so the
// queue never stalls on the gate.
//
// EvaluateRunway decides one dispatch evaluation from the sandbox row's
// facts. A rotation is not a state of its own: it is data on the row
// (which gen is rotating, since when, whether its snapshot was recorded)
// that each evaluation reads, so the decision is the whole of it.
//
// It takes no now. Every duration in RunwayFacts is computed by the
// caller on the database's clock, in the statement that reads the row, so
// the gate and the deadline it is measured against can never disagree on
// what time it is.

// rotationLifetimeDivisor caps the rotation threshold at a sixth of the
// sandbox's lifetime (technical plan §35.3, "min(RotationRunwayFloor,
// lifetime/6)"): a replacement starts with its whole lifetime, far above
// its own threshold, so a rotation can never lead straight into another.
const rotationLifetimeDivisor = 6

// RotationThreshold is the least runway a turn is dispatched with:
// min(floor, lifetime/6), or floor when lifetime is not positive.
// Lifetime is the gen's own when its row records one, otherwise the
// shortest any kind is given (the caller's choice, the conservative one).
func RotationThreshold(floor, lifetime time.Duration) time.Duration {
	if lifetime <= 0 {
		return floor
	}
	return min(floor, lifetime/rotationLifetimeDivisor)
}

// RunwayFacts is what one dispatch evaluation knows of the sandbox it
// would dispatch to. Every duration was computed on the database's clock.
type RunwayFacts struct {
	// Status is the sandbox's state.
	Status State
	// DeadlineKnown is true when the row's lifetime deadline was stamped
	// for its current gen.
	DeadlineKnown bool
	// Runway is the deadline less the database's now; zero or negative
	// once it has passed. Meaningless unless DeadlineKnown.
	Runway time.Duration
	// Lifetime is the lifetime the gen's deadline was stamped from, or the
	// shortest configured kind's when the row records none.
	Lifetime time.Duration
	// TurnInFlight is true when the turn to send is already processing:
	// its prompt is being re-sent to a newer gen.
	TurnInFlight bool
	// CanSnapshot is true when the provider takes snapshots, a commander
	// can send the snapshot command, and the session needs no container
	// daemon (technical plan §27.8). The caller may leave it false when an
	// earlier row decides, since only the below-threshold rows read it.
	CanSnapshot bool
	// Rotating is true when the row records a rotation of its current gen.
	Rotating bool
	// SinceRotationStart is the database's now less the rotation's start.
	// Meaningless unless Rotating.
	SinceRotationStart time.Duration
	// RotationSnapshotTaken is true when the row's snapshot was recorded,
	// with its provenance, at or after the rotation started.
	RotationSnapshotTaken bool
	// DeliveryOpen is true while a completed turn's push and pull request
	// are still being delivered, and DeliveryRemaining is how long until
	// the delivery's bound from its start.
	DeliveryOpen      bool
	DeliveryRemaining time.Duration
}

// RunwayConfig is the gate's configuration, populated by the caller from
// platform.Timeouts (RotationRunwayFloor, RotationSnapshotWait).
type RunwayConfig struct {
	// Floor is the most runway the threshold ever asks for.
	Floor time.Duration
	// SnapshotWait is how long a rotation waits for its snapshot to be
	// reported before it falls back to the previous one.
	SnapshotWait time.Duration
}

// RunwayAction is what the evaluation does with the sandbox.
type RunwayAction string

const (
	// RunwayPass lets the evaluation go on as before: dispatch, or
	// whatever else it would have done.
	RunwayPass RunwayAction = "pass"
	// RunwayHold sends nothing this round; the evaluation looks again
	// after NextLook, when it is positive, or at the next event.
	RunwayHold RunwayAction = "hold"
	// RunwayRotate starts a rotation: ready to snapshotting, the rotation
	// recorded on the row, the snapshot command sent.
	RunwayRotate RunwayAction = "rotate"
	// RunwayRetire retires the gen to stopped before anything more is sent
	// to it; the next evaluation restores or spawns its replacement.
	RunwayRetire RunwayAction = "retire"
)

// RunwayReason says which row of the gate decided.
type RunwayReason string

// The reasons, one per row of EvaluateRunway's table, in its order.
const (
	RunwayReasonNotSteady                   RunwayReason = "not_steady"
	RunwayReasonDeadlinePassed              RunwayReason = "deadline_passed"
	RunwayReasonAwaitingRotationSnapshot    RunwayReason = "awaiting_rotation_snapshot"
	RunwayReasonRotationSnapshotNotReported RunwayReason = "rotation_snapshot_not_reported"
	RunwayReasonRotationSnapshotTaken       RunwayReason = "rotation_snapshot_taken"
	RunwayReasonRotationSnapshotFailed      RunwayReason = "rotation_snapshot_failed"
	RunwayReasonSnapshotInProgress          RunwayReason = "snapshot_in_progress"
	RunwayReasonDeadlineUnknown             RunwayReason = "deadline_unknown"
	RunwayReasonOK                          RunwayReason = "runway_ok"
	RunwayReasonTurnInFlight                RunwayReason = "turn_in_flight"
	RunwayReasonSuspect                     RunwayReason = "suspect"
	RunwayReasonNoSnapshot                  RunwayReason = "no_snapshot"
	RunwayReasonDeliveryOpen                RunwayReason = "delivery_open"
	RunwayReasonBelowThreshold              RunwayReason = "below_threshold"
)

// RunwayDecision is EvaluateRunway's result.
type RunwayDecision struct {
	Action RunwayAction
	Reason RunwayReason
	// Threshold is the rotation threshold the facts were measured against,
	// set on every decision.
	Threshold time.Duration
	// NextLook is when to evaluate again: for a hold, how long it lasts at
	// most, zero when only an event ends it; for a rotation, the snapshot
	// wait. Zero for every other decision.
	NextLook time.Duration
}

// EvaluateRunway decides what a dispatch evaluation does with the sandbox
// it would dispatch to. The first row that matches decides:
//
//	 1  not ready, suspect or snapshotting          pass    not_steady
//	 2  deadline known and passed                   retire  deadline_passed
//	 3  rotating, snapshotting, within the wait     hold    awaiting_rotation_snapshot
//	 4  rotating, snapshotting                      retire  rotation_snapshot_not_reported
//	 5  rotating, its snapshot taken                retire  rotation_snapshot_taken
//	 6  rotating                                    retire  rotation_snapshot_failed
//	 7  snapshotting                                hold    snapshot_in_progress
//	 8  deadline unknown                            pass    deadline_unknown (declined)
//	 9  runway at or above the threshold            pass    runway_ok
//	10  a turn in flight                            pass    turn_in_flight (declined)
//	11  suspect                                     pass    suspect (declined)
//	12  no snapshot capability                      pass    no_snapshot (declined)
//	13  a delivery open                             hold    delivery_open
//	14  ready, below the threshold                  rotate  below_threshold
//
// A snapshotting sandbox is never passed (rows 2 to 7 take every one), so
// no prompt goes to a sandbox in the middle of a snapshot. The deadline
// row precedes every rotation row, so a deadline that passes mid-rotation
// retires the gen. Equality at the threshold passes: a turn is never
// dispatched with less than it. The declines precede the delivery hold, so
// a hold never delays a dispatch that would be declined anyway; the hold
// itself exists because the agent runs push and snapshot one after the
// other on its read loop, so a rotation's snapshot sent during a push
// would wait behind it and outrun the snapshot wait.
func EvaluateRunway(f RunwayFacts, cfg RunwayConfig) RunwayDecision {
	threshold := RotationThreshold(cfg.Floor, f.Lifetime)
	decide := func(action RunwayAction, reason RunwayReason, nextLook time.Duration) RunwayDecision {
		return RunwayDecision{Action: action, Reason: reason, Threshold: threshold, NextLook: nextLook}
	}
	// runwayOr bounds a hold by the deadline, when one is known.
	runwayOr := func(d time.Duration) time.Duration {
		if f.DeadlineKnown {
			return min(d, f.Runway)
		}
		return d
	}
	snapshotting := f.Status == StateSnapshotting

	switch {
	case f.Status != StateReady && f.Status != StateSuspect && !snapshotting:
		return decide(RunwayPass, RunwayReasonNotSteady, 0)
	case f.DeadlineKnown && f.Runway <= 0:
		return decide(RunwayRetire, RunwayReasonDeadlinePassed, 0)
	case f.Rotating && snapshotting && f.SinceRotationStart < cfg.SnapshotWait:
		return decide(RunwayHold, RunwayReasonAwaitingRotationSnapshot, runwayOr(cfg.SnapshotWait-f.SinceRotationStart))
	case f.Rotating && snapshotting:
		return decide(RunwayRetire, RunwayReasonRotationSnapshotNotReported, 0)
	case f.Rotating && f.RotationSnapshotTaken:
		return decide(RunwayRetire, RunwayReasonRotationSnapshotTaken, 0)
	case f.Rotating:
		return decide(RunwayRetire, RunwayReasonRotationSnapshotFailed, 0)
	case snapshotting:
		if f.DeadlineKnown {
			return decide(RunwayHold, RunwayReasonSnapshotInProgress, f.Runway)
		}
		return decide(RunwayHold, RunwayReasonSnapshotInProgress, 0)
	case !f.DeadlineKnown:
		return decide(RunwayPass, RunwayReasonDeadlineUnknown, 0)
	case f.Runway >= threshold:
		return decide(RunwayPass, RunwayReasonOK, 0)
	case f.TurnInFlight:
		return decide(RunwayPass, RunwayReasonTurnInFlight, 0)
	case f.Status == StateSuspect:
		return decide(RunwayPass, RunwayReasonSuspect, 0)
	case !f.CanSnapshot:
		return decide(RunwayPass, RunwayReasonNoSnapshot, 0)
	case f.DeliveryOpen:
		return decide(RunwayHold, RunwayReasonDeliveryOpen, min(f.DeliveryRemaining, f.Runway))
	default:
		return decide(RunwayRotate, RunwayReasonBelowThreshold, cfg.SnapshotWait)
	}
}
