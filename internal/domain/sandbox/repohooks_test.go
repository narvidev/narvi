package sandbox

import "testing"

// TestDecideRepoHooksRestore pins which snapshots a restore may bring back
// as far as the repository's hooks go: a session that boots without them
// only a snapshot of an agent that honours that, minted by a gen that
// skipped them -- the record naming that snapshot; a session that runs
// them none while the record names a snapshot, this one or another.
func TestDecideRepoHooksRestore(t *testing.T) {
	recorded := func(protocol string) SnapshotProvenance {
		return SnapshotProvenance{Recorded: true, AgentProtocol: protocol}
	}
	thisOne := RepoHooksSnapshotRecord{MintedWithoutHooks: true, Set: true}
	another := RepoHooksSnapshotRecord{Set: true}
	for _, tc := range []struct {
		name         string
		sessionSkips bool
		p            SnapshotProvenance
		r            RepoHooksSnapshotRecord
		want         RepoHooksRestoreReason
	}{
		{name: "skips: a current agent's gen that skipped", sessionSkips: true, p: recorded("1.28.0"), r: thisOne, want: RepoHooksRestoreAllowed},
		{name: "skips: a later agent's gen that skipped", sessionSkips: true, p: recorded("1.31.2"), r: thisOne, want: RepoHooksRestoreAllowed},
		{name: "skips: a current agent's gen that ran them", sessionSkips: true, p: recorded("1.28.0"), want: RepoHooksRestoreMintedWithHooks},
		{name: "skips: a current agent's snapshot, the record naming another", sessionSkips: true, p: recorded("1.28.0"), r: another, want: RepoHooksRestoreMintedWithHooks},
		{name: "skips: protocol 1.27.0", sessionSkips: true, p: recorded("1.27.0"), r: thisOne, want: RepoHooksRestoreAgentTooOld},
		{name: "skips: protocol 1.25.0", sessionSkips: true, p: recorded("1.25.0"), want: RepoHooksRestoreAgentTooOld},
		{name: "skips: no protocol reported", sessionSkips: true, p: recorded(""), r: thisOne, want: RepoHooksRestoreAgentUnknown},
		{name: "skips: an unreadable protocol", sessionSkips: true, p: recorded("1.28"), r: thisOne, want: RepoHooksRestoreAgentUnknown},
		{name: "skips: nothing recorded", sessionSkips: true, r: thisOne, want: RepoHooksRestoreAgentUnknown},
		{name: "runs them: a gen that ran them", p: recorded("1.28.0"), want: RepoHooksRestoreAllowed},
		{name: "runs them: an older agent", p: recorded("1.25.0"), want: RepoHooksRestoreAllowed},
		{name: "runs them: nothing recorded", want: RepoHooksRestoreAllowed},
		{name: "runs them: a gen that skipped them", p: recorded("1.28.0"), r: thisOne, want: RepoHooksRestoreMintedWithoutHooks},
		{name: "runs them: the record naming another snapshot", p: recorded("1.28.0"), r: another, want: RepoHooksRestoreHooklessLineage},
		{name: "runs them: the record naming another, nothing recorded of this one", r: another, want: RepoHooksRestoreHooklessLineage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideRepoHooksRestore(tc.sessionSkips, tc.p, tc.r); got != tc.want {
				t.Errorf("DecideRepoHooksRestore(%v, %+v, %+v) = %q, want %q", tc.sessionSkips, tc.p, tc.r, got, tc.want)
			}
		})
	}
	if _, ok := ParseVersion(MinSkipRepoHooksAgentProtocol); !ok {
		t.Fatalf("MinSkipRepoHooksAgentProtocol %q does not parse", MinSkipRepoHooksAgentProtocol)
	}
}
