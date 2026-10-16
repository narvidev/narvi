package sandbox

// MinSkipRepoHooksAgentProtocol is the first sandbox-agent protocol, the
// contracts VERSION an agent was compiled with, that honours
// SESSION_CONFIG's skipRepoHooks (technical plan §6.4, §27.1). A restore
// brings back the agent a snapshot holds, so a snapshot an older agent
// minted is never restored into a session that boots without the
// repository's hooks: that agent would run them.
const MinSkipRepoHooksAgentProtocol = "1.28.0"

// RepoHooksRestoreReason says why DecideRepoHooksRestore refuses a
// restore; empty when it does not.
type RepoHooksRestoreReason string

const (
	// RepoHooksRestoreAllowed means nothing about the repository's hooks
	// stands in the restore's way.
	RepoHooksRestoreAllowed RepoHooksRestoreReason = ""
	// RepoHooksRestoreAgentUnknown means the session boots without the
	// repository's hooks and nothing readable is recorded of the agent the
	// snapshot holds.
	RepoHooksRestoreAgentUnknown RepoHooksRestoreReason = "agent_unknown"
	// RepoHooksRestoreAgentTooOld means the session boots without the
	// repository's hooks and the snapshot holds an agent that predates
	// skipRepoHooks.
	RepoHooksRestoreAgentTooOld RepoHooksRestoreReason = "agent_too_old"
	// RepoHooksRestoreMintedWithHooks means the session boots without the
	// repository's hooks and the snapshot's gen booted with them.
	RepoHooksRestoreMintedWithHooks RepoHooksRestoreReason = "minted_with_hooks"
	// RepoHooksRestoreMintedWithoutHooks means the session runs the
	// repository's hooks and the snapshot's gen booted without them: its
	// lineage never ran setup.sh, which a restore does not run.
	RepoHooksRestoreMintedWithoutHooks RepoHooksRestoreReason = "minted_without_hooks"
	// RepoHooksRestoreHooklessLineage means the session runs the
	// repository's hooks and the sandbox records another snapshot as
	// minted without them: the snapshot may descend from it, minted from a
	// restore of it by a binary that does not write the record, so its
	// lineage may never have run setup.sh.
	RepoHooksRestoreHooklessLineage RepoHooksRestoreReason = "hookless_lineage"
)

// RepoHooksSnapshotRecord is what a sandbox records of its snapshots and
// the repository's hooks (sandboxes.snapshot_skipped_repo_hooks_id: the id
// of the snapshot whose gen booted without them, written at its mint and
// cleared with the snapshot).
type RepoHooksSnapshotRecord struct {
	// MintedWithoutHooks means the record names the snapshot to restore:
	// the gen that minted it booted without the hooks.
	MintedWithoutHooks bool
	// Set means the record names a snapshot, this one or another. This
	// release's mint writes it for every snapshot, NULL for one whose gen
	// ran the hooks, so a record naming another snapshot is left by a
	// binary that mints without writing it -- the previous release, which
	// may have minted this snapshot from a restore of the one recorded.
	Set bool
}

// DecideRepoHooksRestore decides whether a snapshot may be restored as far
// as the repository's hooks go (technical plan §6.4, §27.1). sessionSkips
// is whether the session boots without them now; p is the snapshot's
// recorded provenance (SnapshotProvenance), and r what the sandbox records
// of its snapshots and the hooks.
//
// A restore crosses no boundary of the hooks, and the record is read in
// the direction that fails toward that:
//   - a session that skips them restores only a snapshot whose agent is of
//     MinSkipRepoHooksAgentProtocol or later and whose gen skipped them --
//     the record names this snapshot, with its provenance recorded -- since
//     an unknown or older agent would run them at the restored boot, and a
//     gen that ran them left what they wrote;
//   - a session that runs them restores no snapshot while the record names
//     any: one whose gen skipped them never ran setup.sh, which a restore
//     does not run, and one the record does not name may descend from it.
//     This release's mint and clear keep the record on the live snapshot or
//     NULL, so only a snapshot the previous release minted pays that fresh
//     spawn.
//
// The caller spawns fresh instead and clears the snapshot.
func DecideRepoHooksRestore(sessionSkips bool, p SnapshotProvenance, r RepoHooksSnapshotRecord) RepoHooksRestoreReason {
	if !sessionSkips {
		switch {
		case r.MintedWithoutHooks:
			return RepoHooksRestoreMintedWithoutHooks
		case r.Set:
			return RepoHooksRestoreHooklessLineage
		}
		return RepoHooksRestoreAllowed
	}
	skipped := p.Recorded && r.MintedWithoutHooks
	if !p.Recorded || p.AgentProtocol == "" {
		return RepoHooksRestoreAgentUnknown
	}
	protocol, ok := ParseVersion(p.AgentProtocol)
	if !ok {
		return RepoHooksRestoreAgentUnknown
	}
	if floor, _ := ParseVersion(MinSkipRepoHooksAgentProtocol); protocol.Less(floor) {
		return RepoHooksRestoreAgentTooOld
	}
	if !skipped {
		return RepoHooksRestoreMintedWithHooks
	}
	return RepoHooksRestoreAllowed
}
