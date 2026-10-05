// Package gitclone implements §6.4's "Multi-repo ordered clones under
// /workspace/{name} + generated AGENTS.md manifest": CloneAll clones every
// repo named in a SESSION_CONFIG document's Repos list, IN ORDER, into
// workspaceDir/<name>, each spawned as a real `git clone` subprocess via
// the shared internal/sandboxagent/supervisor.Supervisor (the same
// process-group/reap/drain machinery §6.4/§14.2 already use for hooks and
// services.yml -- never a bare exec.Command); WriteAgentsManifest then
// renders the successfully-cloned subset as a plain markdown manifest at
// workspaceDir/AGENTS.md.
//
// Criticality semantics mirror internal/sandboxagent/boot.RunHooks
// (§6.4) and internal/sandboxagent/services.Run (§14.2) exactly:
// repos[0] (§3.4: "position 0 = primary") failing to clone is fatal and
// stops the whole sequence immediately -- no repo after it is even
// attempted; a secondary repo's clone failure is logged as a warning
// (platform.Logger(ctx)) and the loop continues to the next repo.
//
// Every clone is configured with a PER-INVOCATION (never global) git
// credential helper: `-c credential.helper=!'<this binary's own absolute
// path>' credential-helper`, so git shells out to THIS SAME sandbox-agent
// binary's own "credential-helper" subcommand (see
// internal/sandboxagent/credentials) whenever it needs a username/password
// for an https remote -- never a long-lived credential on disk outside
// that package's own flock-protected, expiry-bounded cache (§5.2).
//
// A nil Repo.Branch (§3.4's own documented "nil means the repo's own
// default branch") means CloneAll passes NO --branch flag at all -- it
// never invents or creates a branch; that is explicitly §3.4's
// (internal/domain/gitstate's) job, not this package's.
//
// WriteAgentsManifest's exact markdown shape is this Step's own invented,
// documented convention -- no contracts/ schema governs it, exactly like
// §14.2 documented its own invented Readiness.Health shape.
//
// §3.4 ("gitstate in-sandbox", §3.4) adds SyncAll (sync.go) -- the
// counterpart to CloneAll for a boot whose workspace ALREADY has a real
// git repo on disk (a BootModeRepoImage/BootModeSnapshotRestore boot,
// baked into the image or restored from a snapshot), never invoked
// alongside CloneAll for the same repo list. SyncAll runs the real
// "stash-if-dirty -> checkout session branch (create from base if absent)
// -> stash pop" sequence §3.4 describes, driving each real git outcome
// through internal/domain/gitstate's own pure Transition table (via that
// package's TriggerFor* helpers) rather than deciding anything about
// sequencing itself -- see gitstate's own doc.go for why that split exists.
// It shares this package's own hardening conventions unchanged: every new
// git invocation goes through the SAME validateRepoSpec/reposource
// checks, the SAME supervisor.Supervisor (never a bare exec.Command), and
// the SAME "-- ends option parsing" defense in depth, adapted to
// checkout's own real semantics (see sync.go's own checkoutBranch doc
// comment for why that placement differs from cloneOne's).
//
// A repo whose Ref is set -- a pull request's review session, technical
// plan §21.1, §30.4 -- reads that pull request's head from the base
// repository, which keeps it as refs/pull/<number>/head: CheckoutPullRef
// (checkout.go) fetches the ref; forgets an operation a turn left in
// progress, resets the shared index to the commit's tree, and resets the
// sparse state and skip-worktree bits the runtime may have left in it;
// and checks out the commit detached and forced and removes untracked
// files. CloneAll clones
// such a repo without its Branch and checks out the ref's tip; SyncAll
// does the same in place of its stash, checkout and pop, since a review
// session never pushes and its worktree holds no edit to keep; and
// cmd/sandbox-agent's checkout command runs it for the commit a turn
// records. A review never boots on a tree other than its head: when the
// ref cannot be fetched at boot, the spec's head branch is checked out
// instead, and with none the repo fails.
package gitclone
