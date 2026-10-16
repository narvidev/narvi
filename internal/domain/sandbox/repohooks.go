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
)

// DecideRepoHooksRestore decides whether a snapshot may be restored as far
// as the repository's hooks go (technical plan §6.4, §27.1). sessionSkips
// is whether the session boots without them now; p is the snapshot's
// recorded provenance (SnapshotProvenance), and mintedWithoutHooks whether
// the gen that minted it booted without them -- read only when p is
// recorded, since an unrecorded provenance says nothing of that gen.
//
// A restore crosses no boundary of the hooks: a session that skips them
// restores only a snapshot whose agent is of MinSkipRepoHooksAgentProtocol
// or later and whose gen skipped them -- an unknown or older agent would
// run them at the restored boot, and a gen that ran them left what they
// wrote -- and a session that runs them never restores one whose gen
// skipped them, whose lineage never ran setup.sh. The caller spawns fresh
// instead and clears the snapshot.
func DecideRepoHooksRestore(sessionSkips bool, p SnapshotProvenance, mintedWithoutHooks bool) RepoHooksRestoreReason {
	skipped := p.Recorded && mintedWithoutHooks
	if !sessionSkips {
		if skipped {
			return RepoHooksRestoreMintedWithoutHooks
		}
		return RepoHooksRestoreAllowed
	}
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
