package turn

import "testing"

// TestDecideRepoHooksGate pins the gate's whole truth table: a session
// that boots with the repository's hooks is never held, and one that boots
// without them is sent only to a gen that was told to skip them and
// advertised it honours that; a gen that was not told is retired.
func TestDecideRepoHooksGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    RepoHooksGateFacts
		want RepoHooksGateAction
	}{
		{name: "hooks run: an old gen with a snapshot", f: RepoHooksGateFacts{HasSnapshot: true}, want: RepoHooksProceed},
		{name: "hooks run: a gen awaiting its ready", f: RepoHooksGateFacts{AwaitingReady: true}, want: RepoHooksProceed},
		{name: "hooks run: an old fresh gen", f: RepoHooksGateFacts{}, want: RepoHooksProceed},
		{name: "no hooks: told, a capable gen", f: RepoHooksGateFacts{SkipRepoHooks: true, Told: true, GenCapable: true}, want: RepoHooksProceed},
		{name: "no hooks: told, a capable gen with a snapshot", f: RepoHooksGateFacts{SkipRepoHooks: true, Told: true, GenCapable: true, HasSnapshot: true}, want: RepoHooksProceed},
		{name: "no hooks: told, a gen awaiting its ready", f: RepoHooksGateFacts{SkipRepoHooks: true, Told: true, AwaitingReady: true, HasSnapshot: true}, want: RepoHooksWait},
		{name: "no hooks: told, an old gen with a snapshot", f: RepoHooksGateFacts{SkipRepoHooks: true, Told: true, HasSnapshot: true}, want: RepoHooksRetireGen},
		{name: "no hooks: told, an old fresh gen", f: RepoHooksGateFacts{SkipRepoHooks: true, Told: true}, want: RepoHooksRefuse},
		{name: "no hooks: not told, a capable gen", f: RepoHooksGateFacts{SkipRepoHooks: true, GenCapable: true}, want: RepoHooksRetireNotTold},
		{name: "no hooks: not told, a capable gen with a snapshot", f: RepoHooksGateFacts{SkipRepoHooks: true, GenCapable: true, HasSnapshot: true}, want: RepoHooksRetireNotTold},
		{name: "no hooks: not told, a gen awaiting its ready", f: RepoHooksGateFacts{SkipRepoHooks: true, AwaitingReady: true}, want: RepoHooksRetireNotTold},
		{name: "no hooks: not told, an old gen", f: RepoHooksGateFacts{SkipRepoHooks: true}, want: RepoHooksRetireNotTold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideRepoHooksGate(tc.f); got != tc.want {
				t.Errorf("DecideRepoHooksGate(%+v) = %s, want %s", tc.f, got, tc.want)
			}
		})
	}
}
