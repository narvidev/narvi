package sandbox

// A fresh lineage is a sandbox that holds nothing of the session's earlier
// work: a fresh spawn that replaces an earlier gen of a session that had a
// turn dispatched to one (technical plan §35.5). The repository is cloned
// again and the agent runtime opens an empty conversation, so the agent has
// neither its context nor any work that was not pushed. A restore keeps the
// snapshot's filesystem and the runtime's session storage, and a resume
// keeps the provider's own object, so neither starts one. A restore of a
// snapshot a fresh lineage minted before its recap reached it brings back
// that fresh lineage's runtime, which never received the recap: it carries
// the pending mark of the gen it replaces.
//
// DecideFreshLineage is the one place that tells them apart. Its caller is
// the claim that creates the gen, in the claim's own transaction: every
// sandbox the control plane creates is claimed there, so a fresh lineage is
// decided once, by this function, whichever path spawned it.

// LineageClaim is what the claim that creates the new gen does. The zero
// value is no claim at all, and is never marked.
type LineageClaim int

// The three claims.
const (
	// LineageClaimSpawn spawns a new sandbox from the image.
	LineageClaimSpawn LineageClaim = iota + 1
	// LineageClaimRestore creates the sandbox from a recorded snapshot.
	LineageClaimRestore
	// LineageClaimResume resumes the provider's own stopped object.
	LineageClaimResume
)

// LineageRefusal is a restore the same claim refused before it spawned
// fresh instead.
type LineageRefusal int

// The refusals.
const (
	// LineageRefusalNone means the claim refused no restore.
	LineageRefusalNone LineageRefusal = iota
	// LineageRefusalCleared means a restore was refused and its snapshot
	// cleared: on the snapshot's recorded provenance (§35.5b), or across
	// the repository-hooks boundary.
	LineageRefusalCleared
	// LineageRefusalKept means a restore was downgraded to a fresh spawn
	// and its snapshot kept: a session that needs a container daemon
	// (§27.8), or a shadow session (§30.4(3)).
	LineageRefusalKept
)

// LineageCause says why a marked claim started a fresh lineage. Empty when
// the claim is not marked. The values are the ones the metrics and logs
// carry.
type LineageCause string

// The causes, in the order DecideFreshLineage tries them.
const (
	// LineageCauseNone is the cause of a claim that is not marked.
	LineageCauseNone LineageCause = ""
	// LineageCauseUnrecappedSnapshot: a restore of a snapshot minted by a
	// gen whose recap had not reached it, replacing a gen whose mark is
	// still pending. Always carried.
	LineageCauseUnrecappedSnapshot LineageCause = "unrecapped_snapshot"
	// LineageCauseRestoreRefused: a restore refused, its snapshot cleared.
	LineageCauseRestoreRefused LineageCause = "restore_refused"
	// LineageCauseRestoreDowngraded: a restore downgraded, its snapshot kept.
	LineageCauseRestoreDowngraded LineageCause = "restore_downgraded"
	// LineageCauseRotationNoSnapshot: a rotation with no snapshot to
	// restore (§35.3).
	LineageCauseRotationNoSnapshot LineageCause = "rotation_no_snapshot"
	// LineageCauseStuckRespawn: a gen still spawning, connecting, booting
	// or ready, given up on as stuck and spawned fresh, a slow restore
	// among them.
	LineageCauseStuckRespawn LineageCause = "stuck_respawn"
	// LineageCauseNoSnapshot: a dead gen with no snapshot to restore and
	// nothing to resume.
	LineageCauseNoSnapshot LineageCause = "no_snapshot"
)

// LineageFacts is what DecideFreshLineage decides on, read in the claim's
// transaction, from the replaced row as it was before the claim's upsert.
type LineageFacts struct {
	// Claim is what the claim does.
	Claim LineageClaim
	// FirstGen is true when the session had no sandbox row before the
	// claim, or one still pending: the session's first spawn.
	FirstGen bool
	// PriorTurnDispatched is true when a turn was dispatched to a gen below
	// the one the claim creates.
	PriorTurnDispatched bool
	// ReplacedMarkPending is true when the replaced row's mark is still
	// pending: its fresh_lineage_gen equals its gen, so the loss of its
	// context was announced and no recap has reached it yet.
	ReplacedMarkPending bool
	// Refusal is the restore this claim refused, if any.
	Refusal LineageRefusal
	// Rotating is true when the replaced gen was rotating (§35.3).
	Rotating bool
	// ReplacedLive is true when the replaced gen was spawning, connecting,
	// booting or ready.
	ReplacedLive bool
	// RestoredSnapshotUnrecapped is true, for a restore, when the snapshot
	// it restores was minted by a gen that was a fresh lineage whose recap
	// had not reached it (sandboxes.snapshot_recap_owed_id names it): the
	// restored runtime holds neither the earlier conversation nor the
	// recap.
	RestoredSnapshotUnrecapped bool
}

// LineageDecision is DecideFreshLineage's answer.
type LineageDecision struct {
	// Mark is true when the new gen starts a fresh lineage: it is marked,
	// and the first turn dispatched to it carries a recap.
	Mark bool
	// Announce is true when the loss is announced by this claim: a marked
	// claim whose replaced gen's loss was not already announced.
	Announce bool
	// Carried is true when the mark is carried over from a replaced gen
	// whose own mark is still pending: its loss was announced once, and no
	// recap reached it.
	Carried bool
	// Cause is why the claim is marked; empty when it is not.
	Cause LineageCause
}

// DecideFreshLineage decides whether the claim f describes starts a fresh
// lineage (technical plan §35.5). The first row that matches decides:
//
//  0. a restore of a snapshot minted before its fresh lineage's recap
//     reached it, replacing a gen whose mark is pending: marked and
//     carried, unrecapped_snapshot;
//  1. a restore: not marked;
//  2. a resume: not marked;
//  3. the session's first gen: not marked, there is nothing to lose;
//  4. no turn was dispatched to an earlier gen: not marked, likewise;
//  5. the replaced gen's mark is pending: marked and carried, not
//     announced again, with the cause rows 6 to 10 give;
//  6. a restore refused, its snapshot cleared: restore_refused;
//  7. a restore downgraded, its snapshot kept: restore_downgraded;
//  8. the replaced gen was rotating: rotation_no_snapshot;
//  9. the replaced gen was live: stuck_respawn;
//  10. otherwise: no_snapshot.
//
// Rows 6 to 10 are marked and announced. A claim that is none of spawn,
// restore and resume is never marked. A restore is marked only by row 0,
// and only ever carries a mark: the loss was announced when the fresh
// lineage began.
func DecideFreshLineage(f LineageFacts) LineageDecision {
	if f.Claim == LineageClaimRestore && f.ReplacedMarkPending && f.RestoredSnapshotUnrecapped {
		return LineageDecision{Mark: true, Carried: true, Cause: LineageCauseUnrecappedSnapshot}
	}
	if f.Claim != LineageClaimSpawn || f.FirstGen || !f.PriorTurnDispatched {
		return LineageDecision{}
	}
	cause := freshLineageCause(f)
	if f.ReplacedMarkPending {
		return LineageDecision{Mark: true, Carried: true, Cause: cause}
	}
	return LineageDecision{Mark: true, Announce: true, Cause: cause}
}

// freshLineageCause is rows 6 to 10 of DecideFreshLineage.
func freshLineageCause(f LineageFacts) LineageCause {
	switch {
	case f.Refusal == LineageRefusalCleared:
		return LineageCauseRestoreRefused
	case f.Refusal == LineageRefusalKept:
		return LineageCauseRestoreDowngraded
	case f.Rotating:
		return LineageCauseRotationNoSnapshot
	case f.ReplacedLive:
		return LineageCauseStuckRespawn
	default:
		return LineageCauseNoSnapshot
	}
}
