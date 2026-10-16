package sandbox

import "time"

// The pre-dispatch runway gate (technical plan §35.3): before a turn is
// dispatched into a live sandbox with less than the rotation threshold of
// its lifetime left, the sandbox rotates -- snapshot, stopped, shutdown,
// restore, over the machine's existing edges -- and the turn stays pending
// for the replacement. That is no guarantee: a rotation the gate cannot
// make (no deadline known, a turn in flight, a suspect sandbox, no snapshot
// capability) is declined, and the turn is dispatched as before, below the
// threshold, so the queue never stalls on the gate. Nor does a snapshot
// that never reports: the gate gives one up once it has waited for it, the
// sandbox kept when its agent is heard from after the wait, and suspected
// -- the liveness path every silence takes -- when it falls silent.
//
// EvaluateRunway decides one dispatch evaluation from the sandbox row's
// facts. A rotation is not a state of its own: it is data on the row
// (which gen is rotating, since when, whether its snapshot was recorded)
// that each evaluation reads, so the decision is the whole of it. So is a
// snapshot in progress: the row records when it started.
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

// RotationThreshold is the runway below which the gate rotates a sandbox
// before dispatching to it, when it can: min(floor, lifetime/6), or floor
// when lifetime is not positive.
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
	// RotationSnapshotPending is true when the snapshot in progress is the
	// rotation's own: its command is the one the rotation sent. A rotating
	// gen can be taking another, a post-turn snapshot a previous binary
	// started after it dispatched into the gen during a rolling deploy.
	RotationSnapshotPending bool
	// SinceRotationStart is the database's now less the rotation's start.
	// Meaningless unless Rotating.
	SinceRotationStart time.Duration
	// RotationSnapshotTaken is true when the row's snapshot was recorded,
	// with its provenance, at or after the rotation started.
	RotationSnapshotTaken bool
	// SinceSnapshotStart is the database's now less the start of the
	// snapshot in progress, a rotation's or any other. Meaningless unless
	// the sandbox is snapshotting.
	SinceSnapshotStart time.Duration
	// SinceLastSeen is the database's now less the sandbox's last sign of
	// life (last_seen_at): its agent heartbeats on a loop of its own, so
	// a long silence says the agent is gone, not that it is busy.
	SinceLastSeen time.Duration
	// SuspectedMidSnapshot is true when the sandbox is suspect and was
	// snapshotting when it was suspected: a recovery takes it back to
	// snapshotting, so it is no more sent a prompt than a snapshotting one.
	SuspectedMidSnapshot bool
	// DeliveryOpen is true while a completed turn's push and pull request
	// are still being delivered, within the delivery's bound from its
	// start, and DeliveryRemaining is how long until that bound. A push
	// that outlasts the bound is no longer open here (see EvaluateRunway).
	DeliveryOpen      bool
	DeliveryRemaining time.Duration
}

// RunwayConfig is the gate's configuration, populated by the caller from
// platform.Timeouts (RotationRunwayFloor, RotationSnapshotWait,
// SnapshotReportWait, SteadyHeartbeatBudget, TerminalGracePeriod).
type RunwayConfig struct {
	// Floor is the most runway the threshold ever asks for.
	Floor time.Duration
	// SnapshotWait is how long a rotation waits for its snapshot to be
	// reported before it falls back to the previous one.
	SnapshotWait time.Duration
	// ReportWait is how long a snapshot that is not a rotation's -- the
	// post-turn one -- holds a turn before the gate gives it up.
	ReportWait time.Duration
	// HeartbeatBudget is the silence past which the sandbox's agent is
	// taken to be gone, as the steady-state liveness watchdog takes it.
	HeartbeatBudget time.Duration
	// Grace is the terminal grace a suspected sandbox is given before it is
	// failed, when it recovers on any sign of life (§3.2).
	Grace time.Duration
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
	// RunwayRevert gives up a snapshot that was never reported: the
	// sandbox goes back to ready over snapshot_complete, the snapshot's
	// command forgotten so a late report of it is ignored, and the next
	// evaluation dispatches to it.
	RunwayRevert RunwayAction = "revert"
	// RunwaySuspect suspects a snapshotting sandbox whose agent has fallen
	// silent, through the liveness path (§3.2's two-phase rule): suspect,
	// terminal grace armed, the snapshot's command kept. A sign of life
	// during the grace recovers it to snapshotting -- a replayed report
	// then records the snapshot -- and a grace that runs out fails it, and
	// the next evaluation restores or spawns its replacement.
	RunwaySuspect RunwayAction = "suspect"
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
	RunwayReasonSnapshotAgentSilent         RunwayReason = "snapshot_agent_silent"
	RunwayReasonSnapshotNotReported         RunwayReason = "snapshot_not_reported"
	RunwayReasonAwaitingSignOfLife          RunwayReason = "awaiting_sign_of_life"
	RunwayReasonSuspectedMidSnapshot        RunwayReason = "suspected_mid_snapshot"
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
	// most; for a rotation, the snapshot wait. Zero for every other
	// decision.
	NextLook time.Duration
}

// EvaluateRunway decides what a dispatch evaluation does with the sandbox
// it would dispatch to. "Silent" is SinceLastSeen at or past
// HeartbeatBudget. The first row that matches decides:
//
//	 1  not ready, suspect or snapshotting                   pass     not_steady
//	 2  deadline known and passed, the agent silent          retire   deadline_passed
//	 3  rotating, its own snapshot pending, within the wait  hold     awaiting_rotation_snapshot
//	 4  rotating, its own snapshot pending                   retire   rotation_snapshot_not_reported
//	 5  rotating, not mid-snapshot, its snapshot taken       retire   rotation_snapshot_taken
//	 6  rotating, not mid-snapshot                           retire   rotation_snapshot_failed
//	 7  snapshotting, within the report wait                 hold     snapshot_in_progress
//	7a  snapshotting, the agent silent                       suspect  snapshot_agent_silent
//	7b  snapshotting, the agent heard from since the wait    revert   snapshot_not_reported
//	7c  snapshotting                                         hold     awaiting_sign_of_life
//	7d  suspected while snapshotting                         hold     suspected_mid_snapshot
//	 8  deadline unknown                                     pass     deadline_unknown (declined)
//	 9  runway at or above the threshold                     pass     runway_ok
//	10  a turn in flight                                     pass     turn_in_flight (declined)
//	11  suspect                                              pass     suspect (declined)
//	12  no snapshot capability                               pass     no_snapshot (declined)
//	13  a delivery open                                      hold     delivery_open
//	14  ready, below the threshold or past the deadline      rotate   below_threshold
//
// "Mid-snapshot" is snapshotting, or suspected while snapshotting. A
// snapshotting sandbox is never passed (rows 2 to 7c take every one), and
// neither is one suspected mid-snapshot (row 2 or 7d), so no prompt goes to
// a sandbox in the middle of a snapshot, and no rotation row cuts a
// suspicion's grace short. Equality at the threshold passes. The declines
// precede the delivery hold, so a hold never delays a dispatch that would
// be declined anyway; a declined dispatch has less than the threshold of
// runway, by design, so the queue never stalls on the gate. The delivery
// hold exists because the agent runs push and snapshot one after the other
// on its read loop, so a rotation's snapshot sent during a push would wait
// behind it and outrun the snapshot wait.
//
// The deadline is the claim's conservative estimate, which a provider
// that gives a sandbox longer outlives. So a passed deadline retires a gen
// at once only when its agent is silent too -- the sandbox died at its
// deadline. A gen past its deadline whose agent is still heard from is
// treated as one below the threshold: a snapshot in progress and an open
// delivery are waited for, never cut short, and then it is rotated through
// its own snapshot, which keeps its work. Its holds are then bounded by
// their own waits alone, since the deadline is behind them.
//
// Rows 7 to 7d bound a snapshot that is not a rotation's. The agent
// reports a failed mint with no frame at all, and one that dies mid-mint
// reports nothing either, so silence is the only signal, and it is read
// once the report wait has run from the snapshot's start. An agent silent
// past the heartbeat budget is suspected (row 7a) through the liveness
// path, so a sign of life during its grace recovers it. Otherwise the
// snapshot is given up (row 7b) only for an agent heard from after the
// wait ended -- its heartbeats run apart from the read loop the snapshot
// blocks, so a live one is heard within a heartbeat interval -- and an
// agent not heard from since is held (row 7c) until it is, or until its
// silence reaches the budget, so one that died partway through the wait
// is suspected, never sent the turn. A rotation's own snapshot is the
// rotation's (rows 3 and 4); another snapshot a rotating gen takes is
// waited for like any other, and its report then retires the gen (row 5)
// with that snapshot.
//
// Every retirement is of a rotating gen or of one past its deadline, the
// two a resume is ruled out for (a resume carries the deadline forward,
// technical plan §35.2). Every hold names a positive next look, so a hold
// always ends. The delivery hold ends with the delivery's bound, not with
// the push: a session whose repositories push past that bound still delays
// the rotation's snapshot behind them, and when the snapshot then misses
// the wait, row 4 retires the gen and the rotation falls back to the
// previous snapshot (platform.Timeouts.RotationRunwayFloor's doc).
func EvaluateRunway(f RunwayFacts, cfg RunwayConfig) RunwayDecision {
	threshold := RotationThreshold(cfg.Floor, f.Lifetime)
	decide := func(action RunwayAction, reason RunwayReason, nextLook time.Duration) RunwayDecision {
		return RunwayDecision{Action: action, Reason: reason, Threshold: threshold, NextLook: nextLook}
	}
	// runwayOr bounds a hold by the deadline while it is still ahead; one
	// behind a passed deadline is bounded by its own wait.
	runwayOr := func(d time.Duration) time.Duration {
		if f.DeadlineKnown && f.Runway > 0 {
			return min(d, f.Runway)
		}
		return d
	}
	snapshotting := f.Status == StateSnapshotting
	midSnapshot := snapshotting || (f.Status == StateSuspect && f.SuspectedMidSnapshot)
	silent := f.SinceLastSeen >= cfg.HeartbeatBudget
	ownSnapshot := f.Rotating && f.RotationSnapshotPending && snapshotting

	switch {
	case f.Status != StateReady && f.Status != StateSuspect && !snapshotting:
		return decide(RunwayPass, RunwayReasonNotSteady, 0)
	case f.DeadlineKnown && f.Runway <= 0 && silent:
		return decide(RunwayRetire, RunwayReasonDeadlinePassed, 0)
	case ownSnapshot && f.SinceRotationStart < cfg.SnapshotWait:
		return decide(RunwayHold, RunwayReasonAwaitingRotationSnapshot, runwayOr(cfg.SnapshotWait-f.SinceRotationStart))
	case ownSnapshot:
		return decide(RunwayRetire, RunwayReasonRotationSnapshotNotReported, 0)
	case f.Rotating && !midSnapshot && f.RotationSnapshotTaken:
		return decide(RunwayRetire, RunwayReasonRotationSnapshotTaken, 0)
	case f.Rotating && !midSnapshot:
		return decide(RunwayRetire, RunwayReasonRotationSnapshotFailed, 0)
	case snapshotting && f.SinceSnapshotStart < cfg.ReportWait:
		return decide(RunwayHold, RunwayReasonSnapshotInProgress, runwayOr(cfg.ReportWait-f.SinceSnapshotStart))
	case snapshotting && silent:
		return decide(RunwaySuspect, RunwayReasonSnapshotAgentSilent, 0)
	case snapshotting && f.SinceLastSeen < f.SinceSnapshotStart-cfg.ReportWait:
		return decide(RunwayRevert, RunwayReasonSnapshotNotReported, 0)
	case snapshotting:
		return decide(RunwayHold, RunwayReasonAwaitingSignOfLife, runwayOr(cfg.HeartbeatBudget-f.SinceLastSeen))
	case f.Status == StateSuspect && f.SuspectedMidSnapshot:
		return decide(RunwayHold, RunwayReasonSuspectedMidSnapshot, runwayOr(cfg.Grace))
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
		return decide(RunwayHold, RunwayReasonDeliveryOpen, runwayOr(f.DeliveryRemaining))
	default:
		return decide(RunwayRotate, RunwayReasonBelowThreshold, cfg.SnapshotWait)
	}
}
