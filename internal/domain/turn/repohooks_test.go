package turn

import "testing"

// TestDecideRepoHooksGate pins the gate's whole truth table: a session
// that boots with the repository's hooks is never held, and one that boots
// without them is sent only to a gen that advertised it honours that.
func TestDecideRepoHooksGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    RepoHooksGateFacts
		want RepoHooksGateAction
	}{
		{name: "hooks run: an old gen with a snapshot", f: RepoHooksGateFacts{HasSnapshot: true}, want: RepoHooksProceed},
		{name: "hooks run: a gen awaiting its ready", f: RepoHooksGateFacts{AwaitingReady: true}, want: RepoHooksProceed},
		{name: "hooks run: an old fresh gen", f: RepoHooksGateFacts{}, want: RepoHooksProceed},
		{name: "no hooks: a capable gen", f: RepoHooksGateFacts{SkipRepoHooks: true, GenCapable: true}, want: RepoHooksProceed},
		{name: "no hooks: a capable gen with a snapshot", f: RepoHooksGateFacts{SkipRepoHooks: true, GenCapable: true, HasSnapshot: true}, want: RepoHooksProceed},
		{name: "no hooks: a gen awaiting its ready", f: RepoHooksGateFacts{SkipRepoHooks: true, AwaitingReady: true, HasSnapshot: true}, want: RepoHooksWait},
		{name: "no hooks: an old gen with a snapshot", f: RepoHooksGateFacts{SkipRepoHooks: true, HasSnapshot: true}, want: RepoHooksRetireGen},
		{name: "no hooks: an old fresh gen", f: RepoHooksGateFacts{SkipRepoHooks: true}, want: RepoHooksRefuse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideRepoHooksGate(tc.f); got != tc.want {
				t.Errorf("DecideRepoHooksGate(%+v) = %s, want %s", tc.f, got, tc.want)
			}
		})
	}
}
