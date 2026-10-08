package sandbox

import (
	"fmt"
	"math"
	"time"
)

// SpawnState is the sandbox snapshot EvaluateSpawnDecision reasons over.
type SpawnState struct {
	// Status is the sandbox's current state.
	Status State
	// CreatedAt is when the sandbox row was created/last spawned.
	CreatedAt time.Time
	// ProviderObjectID is the provider's own object id, if the sandbox
	// still exists remotely in a persistently-resumable form. "" means
	// none.
	ProviderObjectID string
	// SnapshotImageID is the snapshot to restore from, if any. "" means
	// none.
	SnapshotImageID string
	// HasActiveWebSocket reports whether a live sandbox WebSocket
	// connection currently exists.
	HasActiveWebSocket bool
	// LastSeenAt is the last sign of life from the sandbox (heartbeat or
	// boot-progress ping) -- §3.2's "last_seen_at ... updated by:
	// heartbeat, boot-progress report, any agent event ... any WS frame".
	// Zero (time.Time{}) means none yet.
	//
	// The "still booting" guard below measures from max(CreatedAt,
	// LastSeenAt) so it uses the SAME clock as the connecting-timeout
	// watchdog (EvaluateConnectingTimeout) -- the two can never disagree,
	// so a healthy slow boot that keeps pinging is not respawned (which
	// would rotate its identity and orphan it) before the watchdog gives
	// up.
	LastSeenAt time.Time
}

// SpawnConfig configures EvaluateSpawnDecision. Populated by the caller
// from platform.Timeouts (SpawnCooldown, SpawnReadyWait, SpawnStuckTimeout
// -- not given explicit values in Narvi's plan; chosen and documented in
// platform/timeouts.go, defaults of 30s/60s/120s).
type SpawnConfig struct {
	// Cooldown is the minimum interval between spawn attempts (bypassed
	// for Failed/Stopped sandboxes).
	Cooldown time.Duration
	// ReadyWait is how long a "ready but no WebSocket" sandbox is given
	// to reconnect before treating it as needing a fresh spawn.
	ReadyWait time.Duration
	// SpawningTimeout is the max time a sandbox may remain in a
	// spawning/connecting-style status (measured from its last sign of
	// life) before it is treated as an interrupted spawn and a fresh
	// spawn is allowed.
	SpawningTimeout time.Duration
}

// SpawnActionKind discriminates the SpawnAction union.
type SpawnActionKind int

// The five possible spawn-decision actions.
const (
	SpawnActionSpawn SpawnActionKind = iota
	SpawnActionResume
	SpawnActionRestore
	SpawnActionSkip
	SpawnActionWait
)

func (k SpawnActionKind) String() string {
	switch k {
	case SpawnActionSpawn:
		return "spawn"
	case SpawnActionResume:
		return "resume"
	case SpawnActionRestore:
		return "restore"
	case SpawnActionSkip:
		return "skip"
	case SpawnActionWait:
		return "wait"
	default:
		return fmt.Sprintf("SpawnActionKind(%d)", int(k))
	}
}

// SpawnAction is the discriminated-union result of EvaluateSpawnDecision.
// Only the field(s) documented for Kind are meaningful; the others are
// zero-valued.
type SpawnAction struct {
	Kind SpawnActionKind
	// ProviderObjectID is set when Kind == SpawnActionResume.
	ProviderObjectID string
	// SnapshotImageID is set when Kind == SpawnActionRestore.
	SnapshotImageID string
	// Reason is set when Kind == SpawnActionSkip or SpawnActionWait.
	Reason string
}

// EvaluateSpawnDecision decides what action to take for a sandbox that a
// caller wants live: resume (persistent-resume priority) > restore (from
// snapshot) > skip/wait for an in-progress or cooling-down spawn > spawn
// fresh. See the comments on each branch below for the reasoning behind it.
//
// supportsPersistentResume defaults to false when the provider doesn't
// support it -- Go has no default parameters, so callers must pass it
// explicitly.
func EvaluateSpawnDecision(state SpawnState, cfg SpawnConfig, now time.Time, isSpawningInMemory bool, supportsPersistentResume bool) SpawnAction {
	timeSinceLastSpawn := now.Sub(state.CreatedAt)

	// Resume takes priority over restore: reuses the SAME provider
	// sandbox, so it's cheaper and preserves more state than a
	// snapshot restore.
	if supportsPersistentResume && state.ProviderObjectID != "" &&
		(state.Status == StateStopped || state.Status == StateStale) {
		return SpawnAction{Kind: SpawnActionResume, ProviderObjectID: state.ProviderObjectID}
	}

	// Restore from snapshot if the sandbox has exited and a follow-up
	// arrived: restore on stopped/stale/failed + snapshot (§3.2).
	if state.SnapshotImageID != "" &&
		(state.Status == StateStopped || state.Status == StateStale || state.Status == StateFailed) {
		return SpawnAction{Kind: SpawnActionRestore, SnapshotImageID: state.SnapshotImageID}
	}

	// Don't spawn if a spawn/connect is genuinely in progress (persisted
	// status). But a spawn interrupted before the sandbox connects
	// (provider crash, redeploy, cancelled provider call) can pin the
	// status at a boot-in-progress status forever -- the connecting-
	// timeout alarm may never have been scheduled. Treat a stale
	// spawn/connect as dead so a fresh spawn can recover the session,
	// instead of skipping indefinitely.
	//
	// Measure from the last sign of life (max of CreatedAt and
	// LastSeenAt), identical to the connecting-timeout watchdog: a
	// healthy slow boot keeps emitting boot-progress pings, so it stays
	// "booting" (skip) past the raw CreatedAt window and is NOT respawned
	// -- a respawn would rotate this box's identity and orphan it. A
	// genuinely stuck boot goes silent, so both this guard and the
	// watchdog release after the same window (recovery preserved).
	//
	// Booting is included alongside Spawning/Connecting (Narvi's own
	// three-way boot phase, §3.2) for the same reason: a boot in progress
	// must not be duplicated by a fresh spawn.
	sinceLastSignOfLife := now.Sub(maxTime(state.CreatedAt, state.LastSeenAt))
	if (state.Status == StateSpawning || state.Status == StateConnecting || state.Status == StateBooting) &&
		sinceLastSignOfLife < cfg.SpawningTimeout {
		return SpawnAction{Kind: SpawnActionSkip, Reason: fmt.Sprintf("already %s", state.Status)}
	}

	// Snapshotting is a live sandbox mid-snapshot, and the machine has no
	// spawn edge out of it: no force-respawn, unlike the boot states above
	// (state.go). Its own snapshot report returns it to Ready, a watchdog
	// to Suspect, and the runway gate's deadline row (EvaluateRunway)
	// retires it once its deadline has passed; a spawn decision must not
	// race them, so there is no staleness carve-out here either. Without
	// this row, a pending turn evaluated during a post-turn snapshot fell
	// through to Spawn once the cooldown had passed -- a decision the
	// caller could only refuse as an error, every evaluation until the
	// snapshot ended.
	if state.Status == StateSnapshotting {
		return SpawnAction{Kind: SpawnActionSkip, Reason: "snapshot in progress"}
	}

	// Suspect is mid-grace-period (§3.2: a watchdog silence/timeout parks a
	// live sandbox in Suspect before deciding Stopped/Failed/Stale; "any
	// liveness signal during grace returns to previous state"). Spawning
	// fresh over it would duplicate/orphan a box that may still recover --
	// exactly the failure mode the Spawning/Connecting/Booting guard above
	// exists to prevent. Unlike that guard, there is no staleness carve-out
	// here: grace has its own bounded timer (Timeouts.TerminalGracePeriod)
	// that resolves Suspect to a terminal state on its own; this decision
	// function must not race that resolution by spawning early.
	if state.Status == StateSuspect {
		return SpawnAction{Kind: SpawnActionSkip, Reason: "sandbox suspect, awaiting grace-period resolution"}
	}

	// Don't spawn if status is Ready and we have an active WebSocket.
	if state.Status == StateReady {
		if state.HasActiveWebSocket {
			return SpawnAction{Kind: SpawnActionSkip, Reason: "sandbox ready with active WebSocket"}
		}
		// No WebSocket, but recently spawned: wait for reconnect.
		if timeSinceLastSpawn < cfg.ReadyWait {
			return SpawnAction{
				Kind: SpawnActionWait,
				Reason: fmt.Sprintf(
					"status ready but no WebSocket, last spawn was %ds ago",
					roundSeconds(timeSinceLastSpawn)),
			}
		}
	}

	// Cooldown: don't spawn if the last spawn was within the cooldown
	// period. Exception: Failed or Stopped bypasses cooldown.
	if timeSinceLastSpawn < cfg.Cooldown && state.Status != StateFailed && state.Status != StateStopped {
		return SpawnAction{
			Kind: SpawnActionWait,
			Reason: fmt.Sprintf(
				"last spawn was %ds ago, waiting",
				roundSeconds(timeSinceLastSpawn)),
		}
	}

	// In-memory flag for same-request protection.
	if isSpawningInMemory {
		return SpawnAction{Kind: SpawnActionSkip, Reason: "spawn already in progress (in-memory flag)"}
	}

	// All checks passed: spawn a new sandbox.
	return SpawnAction{Kind: SpawnActionSpawn}
}

// maxTime returns whichever of a, b is later. The zero value time.Time{}
// (Narvi's "never" convention, used for LastSeenAt when no sign of life has
// been recorded yet) sorts before any real timestamp, so
// maxTime(createdAt, time.Time{}) == createdAt.
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// roundSeconds renders d as whole seconds for a human-readable Reason
// string, without spelling out a time.Duration unit literal (forbidden
// outside platform/timeouts.go by the notimeliteral lint rule).
func roundSeconds(d time.Duration) int {
	return int(math.Round(d.Seconds()))
}
