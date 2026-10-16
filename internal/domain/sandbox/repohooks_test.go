package sandbox

import "testing"

// TestDecideRepoHooksRestore pins which snapshots a restore may bring back
// as far as the repository's hooks go: a session that boots without them
// only a snapshot of an agent that honours that, minted by a gen that
// skipped them; a session that runs them never one whose gen skipped them.
func TestDecideRepoHooksRestore(t *testing.T) {
	recorded := func(protocol string) SnapshotProvenance {
		return SnapshotProvenance{Recorded: true, AgentProtocol: protocol}
	}
	for _, tc := range []struct {
		name               string
		sessionSkips       bool
		p                  SnapshotProvenance
		mintedWithoutHooks bool
		want               RepoHooksRestoreReason
	}{
		{name: "skips: a current agent's gen that skipped", sessionSkips: true, p: recorded("1.28.0"), mintedWithoutHooks: true, want: RepoHooksRestoreAllowed},
		{name: "skips: a later agent's gen that skipped", sessionSkips: true, p: recorded("1.31.2"), mintedWithoutHooks: true, want: RepoHooksRestoreAllowed},
		{name: "skips: a current agent's gen that ran them", sessionSkips: true, p: recorded("1.28.0"), want: RepoHooksRestoreMintedWithHooks},
		{name: "skips: protocol 1.27.0", sessionSkips: true, p: recorded("1.27.0"), mintedWithoutHooks: true, want: RepoHooksRestoreAgentTooOld},
		{name: "skips: protocol 1.25.0", sessionSkips: true, p: recorded("1.25.0"), want: RepoHooksRestoreAgentTooOld},
		{name: "skips: no protocol reported", sessionSkips: true, p: recorded(""), mintedWithoutHooks: true, want: RepoHooksRestoreAgentUnknown},
		{name: "skips: an unreadable protocol", sessionSkips: true, p: recorded("1.28"), mintedWithoutHooks: true, want: RepoHooksRestoreAgentUnknown},
		{name: "skips: nothing recorded", sessionSkips: true, mintedWithoutHooks: true, want: RepoHooksRestoreAgentUnknown},
		{name: "runs them: a gen that ran them", p: recorded("1.28.0"), want: RepoHooksRestoreAllowed},
		{name: "runs them: an older agent", p: recorded("1.25.0"), want: RepoHooksRestoreAllowed},
		{name: "runs them: nothing recorded", want: RepoHooksRestoreAllowed},
		{name: "runs them: a gen that skipped them", p: recorded("1.28.0"), mintedWithoutHooks: true, want: RepoHooksRestoreMintedWithoutHooks},
		{name: "runs them: a skip stamp on another snapshot's provenance", mintedWithoutHooks: true, want: RepoHooksRestoreAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideRepoHooksRestore(tc.sessionSkips, tc.p, tc.mintedWithoutHooks); got != tc.want {
				t.Errorf("DecideRepoHooksRestore(%v, %+v, %v) = %q, want %q", tc.sessionSkips, tc.p, tc.mintedWithoutHooks, got, tc.want)
			}
		})
	}
	if _, ok := ParseVersion(MinSkipRepoHooksAgentProtocol); !ok {
		t.Fatalf("MinSkipRepoHooksAgentProtocol %q does not parse", MinSkipRepoHooksAgentProtocol)
	}
}
