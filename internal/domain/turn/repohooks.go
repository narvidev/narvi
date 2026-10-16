package turn

// RepoHooksGateAction is what DecideRepoHooksGate tells the dispatch
// evaluation to do with a turn about to be sent to a sandbox gen.
type RepoHooksGateAction string

const (
	// RepoHooksProceed means: send the turn, as far as the repository's hooks
	// are concerned.
	RepoHooksProceed RepoHooksGateAction = "proceed"
	// RepoHooksWait means: send nothing yet; the gen has sent no ready, so what
	// it honours is not known. Its ready evaluates the turn again.
	RepoHooksWait RepoHooksGateAction = "wait"
	// RepoHooksRetireGen means: retire the gen and clear the sandbox's snapshot,
	// so the next gen boots fresh rather than restoring an agent that runs
	// the repository's hooks whatever it is told.
	RepoHooksRetireGen RepoHooksGateAction = "retire_gen"
	// RepoHooksRetireNotTold means: retire the gen and clear the sandbox's
	// snapshot, so the next gen boots fresh: its SESSION_CONFIG did not
	// tell it to skip the repository's hooks, so it ran them, whatever its
	// agent can do. The image is not the problem.
	RepoHooksRetireNotTold RepoHooksGateAction = "retire_not_told"
	// RepoHooksRefuse means: refuse the turn; the gen's agent runs the
	// repository's hooks whatever it is told, and no snapshot is left to
	// discard, so the sandbox image needs rebuilding with a current agent.
	RepoHooksRefuse RepoHooksGateAction = "refuse"
)

// RepoHooksGateFacts are what DecideRepoHooksGate reads.
type RepoHooksGateFacts struct {
	// SkipRepoHooks is true when the session boots without its
	// repository's hooks (reposource.ReviewSessionSkipsRepoHooks).
	SkipRepoHooks bool
	// Told is true when the live gen's SESSION_CONFIG told it to skip the
	// repository's hooks (sandboxes.skip_repo_hooks_told_gen = gen).
	Told bool
	// AwaitingReady is true when the live gen has sent no ready yet.
	AwaitingReady bool
	// GenCapable is true when the live gen's latest ready advertised
	// capabilities.skipRepoHooks (sandboxes.skip_repo_hooks_gen = gen).
	GenCapable bool
	// HasSnapshot is true when the sandbox has a snapshot.
	HasSnapshot bool
}

// DecideRepoHooksGate decides whether a turn of a session may be sent to
// its sandbox's live gen, as far as the repository's hooks go (technical
// plan §6.4, §27.1, §30.4). A session that boots with them is never held.
// One that boots without them -- a pull request's review session whose
// head is not in its base repository, so code the pull request brings
// never runs at boot -- is sent only to a gen that booted without them: it
// was told to, and its agent honours SESSION_CONFIG's skipRepoHooks. The
// capability alone proves nothing of the boot: a current agent advertises
// it whatever it was told.
//
// A gen that was not told -- one the previous binary spawned -- ran the
// hooks, so it is retired and the sandbox's snapshot cleared, at once,
// whether or not it has sent a ready. A told gen whose agent predates the
// key ran them too: it is retired once while a snapshot could bring it
// back, so the next gen boots fresh; a fresh gen that lacks it too takes
// no snapshot, since no turn runs on it, and its turn is refused naming
// the image. A told gen that has sent no ready yet is waited for, never
// read as lacking the capability.
func DecideRepoHooksGate(f RepoHooksGateFacts) RepoHooksGateAction {
	switch {
	case !f.SkipRepoHooks:
		return RepoHooksProceed
	case !f.Told:
		return RepoHooksRetireNotTold
	case f.AwaitingReady:
		return RepoHooksWait
	case f.GenCapable:
		return RepoHooksProceed
	case f.HasSnapshot:
		return RepoHooksRetireGen
	default:
		return RepoHooksRefuse
	}
}
