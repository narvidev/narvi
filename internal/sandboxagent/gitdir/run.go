package gitdir

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// sessionConfigEnvVar mirrors boot.SessionConfigEnvVar's own literal
// ("NARVI_SESSION_CONFIG") -- duplicated here, deliberately, rather than
// imported: internal/sandboxagent/boot itself imports this package (for
// the boot-time Seed loop and the fingerprint/deps-ladder git calls this
// Step routes through gitdir), so importing boot back would cycle. If
// boot.SessionConfigEnvVar's own literal value ever changes, this must
// change with it.
const sessionConfigEnvVar = "NARVI_SESSION_CONFIG"

// SessionConfigEnvVarForTest exposes sessionConfigEnvVar's own literal
// value for exactly one purpose: an EXTERNAL test (package gitdir_test,
// which CAN import internal/sandboxagent/boot without cycling back into
// this package the way this package itself cannot) asserting it still
// equals boot.SessionConfigEnvVar. A silent drift here would leak the
// sandbox's own plaintext bearer token into the runtime's git via
// RuntimeGit's own env (RuntimeGit itself never strips it -- only the
// hardened, sandbox-agent-identity path, supervisor.EnvWithout, does).
// Not meant for any other use.
func SessionConfigEnvVarForTest() string { return sessionConfigEnvVar }

// RuntimeGit spawns `git -C wt <args...>` as the RUNTIME's own identity
// (cred -- built by cmd/sandbox-agent/main.go's own runtimeCredentialFor,
// ~line 2448, from boot.Config.RuntimeUID/RuntimeGID; nil/self in tests,
// e.g. wherever a test still runs with the default RuntimeUID=0) against
// wt, the runtime-owned worktree -- NEVER against an agent-owned git-dir.
// This is the runtime's own identity acting on its own .git, granted
// explicitly by §30.5 (SyncHeadOut's own `symbolic-ref HEAD` write, and
// the sparse-checkout mirror/import below, are both this codebase's own
// only reasons to ever spawn git as the runtime at all) -- so, unlike
// every githarden.Args invocation, this carries NO hardening flags: the
// runtime already has the run of its own worktree, and hardening flags
// exist to keep sandbox-agent's OWN git from reading runtime-authored
// config, which is not what this call does.
//
// Env is built from supervisor.EnvWithout(sessionConfigEnvVar) (the
// runtime never legitimately needs the sandbox's own plaintext bearer
// token) plus a forced LC_ALL=C (deterministic, English git output for
// anything this package ever parses back), mirroring gitclone's own
// applySparseCheckout precedent for the identical reasoning.
func RuntimeGit(ctx context.Context, sup *supervisor.Supervisor, cred *syscall.Credential, wt string, stdout, stderr io.Writer, timeout, stopGrace time.Duration, args ...string) (supervisor.ExitResult, error) {
	full := append([]string{"-C", wt}, args...)
	proc, err := sup.Spawn(supervisor.Spec{
		Path:       "git",
		Args:       full,
		Stdout:     stdout,
		Stderr:     stderr,
		Credential: cred,
		Env:        append(supervisor.EnvWithout(sessionConfigEnvVar), "LC_ALL=C"),
	})
	if err != nil {
		return supervisor.ExitResult{}, fmt.Errorf("gitdir: spawn runtime git %s: %w", strings.Join(args, " "), err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, waitErr := proc.Wait(runCtx)
	if waitErr != nil {
		_ = proc.Stop(ctx, stopGrace)
		return supervisor.ExitResult{}, fmt.Errorf("gitdir: runtime git %s did not complete within %s: %w", strings.Join(args, " "), timeout, waitErr)
	}
	return result, nil
}

// runAgentGit spawns `git <args...>` as sandbox-agent's OWN identity, with
// no Credential and full env inheritance -- used only for the local,
// no-network plumbing Seed performs directly against an agent-owned
// git-dir (git init --bare, git config), never against a runtime-owned
// path. Distinct from RuntimeGit (a different identity, a different
// target) and from githarden.Args-hardened calls (this is never run
// against repo.WorkTree with repo.GitDir's own runtime-adjacent risk --
// it targets the agent git-dir directly via --git-dir, before that
// git-dir has anything shared into it yet).
func runAgentGit(ctx context.Context, sup *supervisor.Supervisor, timeout, stopGrace time.Duration, args ...string) (supervisor.ExitResult, error) {
	proc, err := sup.Spawn(supervisor.Spec{Path: "git", Args: args})
	if err != nil {
		return supervisor.ExitResult{}, fmt.Errorf("gitdir: spawn git %s: %w", strings.Join(args, " "), err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, waitErr := proc.Wait(runCtx)
	if waitErr != nil {
		_ = proc.Stop(ctx, stopGrace)
		return supervisor.ExitResult{}, fmt.Errorf("gitdir: git %s did not complete within %s: %w", strings.Join(args, " "), timeout, waitErr)
	}
	if result.Err != nil {
		return result, fmt.Errorf("gitdir: git %s: %w", strings.Join(args, " "), result.Err)
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("gitdir: git %s: exited %d", strings.Join(args, " "), result.ExitCode)
	}
	return result, nil
}

// Run is the CHOKE POINT every agent git invocation in this codebase is
// expected to go through: SyncHeadIn (bring the agent-owned HEAD up to
// date with whatever the runtime's own worktree currently has checked
// out) -> spawn spec (already built via githarden.Spec(repo, ...) or
// githarden.Args(repo, ...) wrapped in a Spec by the caller) -> if the
// command actually moved the agent's own HEAD (a checkout, a branch
// creation, ...), SyncHeadOut (write that new branch back onto the
// runtime's own worktree, as the runtime's own identity).
//
// No call site should ever assemble a hardened Spec and call sup.Spawn on
// it directly -- doing so silently skips both brackets, leaving the
// agent-owned HEAD stale (a later read, e.g. `rev-parse HEAD`, reports the
// PREVIOUS branch) or the runtime-owned worktree's own HEAD never
// following a checkout the agent just performed. Routing every call
// through Run, once, here, is the only way that guarantee holds as call
// sites are added -- the same "one choke point, not eight copies"
// reasoning githarden.hardeningFlags' own doc comment gives for -c flags.
func Run(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, cred *syscall.Credential, spec supervisor.Spec, timeout, stopGrace time.Duration) (supervisor.ExitResult, error) {
	// Correction (review): SyncHeadIn's own O_NOFOLLOW only protects the
	// FINAL path component (HEAD) -- if the runtime replaces repo.WorkTree
	// or repo.WorkTree/.git itself with a symlink AFTER Seed ran (Seed's
	// own step 0 only checked this ONCE, at seed time), SyncHeadIn -- and
	// every git spawn this function makes -- would silently follow it,
	// operating against whatever directory the symlink now points at
	// instead of this repo's own worktree. Checked here, on every call,
	// because Run is the one choke point every agent git invocation goes
	// through (see this function's own doc comment below): a swap that
	// happens between two Run calls is caught on the very next one, before
	// SyncHeadIn or the spawn itself ever runs.
	if err := assertRealDir(repo.WorkTree); err != nil {
		return supervisor.ExitResult{}, fmt.Errorf("gitdir: run: worktree: %w", err)
	}
	if err := assertRealDir(filepath.Join(repo.WorkTree, ".git")); err != nil {
		return supervisor.ExitResult{}, fmt.Errorf("gitdir: run: worktree .git: %w", err)
	}

	if err := SyncHeadIn(repo); err != nil {
		return supervisor.ExitResult{}, fmt.Errorf("gitdir: run: sync head in: %w", err)
	}

	headPath := filepath.Join(repo.GitDir, "HEAD")
	before, _ := os.ReadFile(headPath)

	proc, err := sup.Spawn(spec)
	if err != nil {
		return supervisor.ExitResult{}, fmt.Errorf("gitdir: run: spawn: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, waitErr := proc.Wait(runCtx)
	if waitErr != nil {
		_ = proc.Stop(ctx, stopGrace)
		return supervisor.ExitResult{}, fmt.Errorf("gitdir: run: did not complete within %s: %w", timeout, waitErr)
	}

	after, readErr := os.ReadFile(headPath)
	if readErr != nil {
		return result, fmt.Errorf("gitdir: run: read agent HEAD after spawn: %w", readErr)
	}
	if !bytes.Equal(before, after) {
		if err := SyncHeadOut(ctx, sup, repo, cred, timeout, stopGrace); err != nil {
			return result, fmt.Errorf("gitdir: run: sync head out: %w", err)
		}
	}

	return result, nil
}

// MirrorSparseCheckout copies the agent-owned git-dir's own
// core.sparseCheckout/core.sparseCheckoutCone values into the runtime's
// own .git/config, as the runtime's own identity (RuntimeGit) -- the
// reverse direction of Seed's own import at boot time. Callers run this
// immediately after every agent-side `sparse-checkout set`/`sparse-
// checkout disable` (gitclone's own applySparseCheckout/
// disableSparseCheckoutIfEnabled) -- §14.1's own path-scope enforcement
// must be visible to BOTH sides, since either side's later git invocation
// (an agent `git status`, a runtime `git diff`) consults its own copy of
// this bool, never the other side's.
//
// A key that is unset on the agent side (exit 1 from `git config --get`)
// is left untouched on the runtime side -- there is nothing to mirror,
// and forcing it to "false" would be a real behavior change for a
// runtime-owned repo whose own sparse-checkout state this package never
// asked to change.
//
// The write is scoped "--worktree", not a bare "config key value" -- measured
// directly against real git, not assumed: `git sparse-checkout set/disable`
// itself, from Git 2.25 onward, silently turns on extensions.worktreeConfig
// the FIRST time it ever runs against a repo and stores core.sparseCheckout/
// core.sparseCheckoutCone in $GIT_DIR/config.worktree -- a config source that
// resolves with HIGHER precedence than the plain, unscoped local config a
// bare "git config key value" write lands in. Once that extension is on (the
// overwhelmingly common case for any repo this package's own sparse-checkout
// path has ever touched at all), a same-key unscoped write is silently
// SHADOWED by the pre-existing worktree-scoped entry: `git config --get`
// keeps reporting the OLD value even though the write itself reports exit 0
// -- verified directly (the exact failure mode a bare write here produced:
// disabling sparse-checkout on the agent side succeeded and even
// re-materialized the runtime's own worktree files correctly, yet the
// runtime's own core.sparseCheckout still read back "true" afterward).
// "--worktree" writes to the SAME config.worktree file git's own
// sparse-checkout machinery already uses, so it always wins the same way a
// direct `git sparse-checkout` invocation's own writes would.
//
// (Correction, review): the claim this doc comment used to make here --
// that "--worktree" degrades harmlessly to the ordinary local config when
// extensions.worktreeConfig was never enabled -- is only true while the
// runtime repo has exactly ONE worktree. Measured directly against real
// git: the runtime is free to run an ordinary, unprivileged
// `git worktree add` at any point during a session (its own metadata
// lives under repo.WorkTree/.git/worktrees, which Seed never shares or
// cleans up), and once a repo has MORE than one worktree entry --even a
// prunable one whose directory no longer exists-- git refuses
// "--worktree" outright ("fatal: --worktree cannot be used with multiple
// working trees unless the config extension worktreeConfig is enabled"),
// exit 128. That turned an ordinary session action into a fatal boot
// error for the primary repo (gitclone.SyncAll's own criticality split).
// The agent-side `sparse-checkout set/disable` enables the extension in
// the AGENT's own config, which Seed deletes and rebuilds on every boot
// -- it never reaches the runtime side, so the runtime repo's own
// extensions.worktreeConfig stays off across that boundary no matter how
// many times sparse-checkout runs on the agent side.
//
// Fixed by turning the extension on, on the RUNTIME side, explicitly and
// unconditionally, before the very first "--worktree" write below --
// idempotent (a plain `git config` write, always exit 0 whether the key
// was already true or not) and exactly what `git sparse-checkout
// set/disable`, run directly against a single-worktree repo, would end up
// doing to that repo's own config anyway, so this does not change the
// mirror's own resulting config shape versus the pre-PR direct-git path.
func MirrorSparseCheckout(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, cred *syscall.Credential, timeout, stopGrace time.Duration) error {
	enableResult, err := RuntimeGit(ctx, sup, cred, repo.WorkTree, nil, nil, timeout, stopGrace, "config", "extensions.worktreeConfig", "true")
	if err != nil {
		return fmt.Errorf("gitdir: mirror sparse-checkout: enable runtime extensions.worktreeConfig: %w", err)
	}
	if enableResult.ExitCode != 0 {
		return fmt.Errorf("gitdir: mirror sparse-checkout: git config extensions.worktreeConfig exited %d", enableResult.ExitCode)
	}

	for _, key := range []string{"core.sparseCheckout", "core.sparseCheckoutCone"} {
		val, ok, err := readAgentBoolConfig(ctx, sup, repo, key, timeout, stopGrace)
		if err != nil {
			return fmt.Errorf("gitdir: mirror sparse-checkout: read agent %s: %w", key, err)
		}
		if !ok {
			continue
		}
		result, err := RuntimeGit(ctx, sup, cred, repo.WorkTree, nil, nil, timeout, stopGrace, "config", "--worktree", key, val)
		if err != nil {
			return fmt.Errorf("gitdir: mirror sparse-checkout: set runtime %s: %w", key, err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("gitdir: mirror sparse-checkout: git config %s exited %d", key, result.ExitCode)
		}
	}
	return nil
}

// readAgentBoolConfig reads key directly from repo.GitDir's own config
// (sandbox-agent's own identity -- this is an agent-owned file, no
// elevated read needed), returning (value, true, nil) when set to a
// recognized "true"/"false", (_, false, nil) when the key is entirely
// unset, and an error for anything else (an unexpected value, or a git
// failure other than "key not found").
func readAgentBoolConfig(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, key string, timeout, stopGrace time.Duration) (string, bool, error) {
	var stdout bytes.Buffer
	proc, err := sup.Spawn(supervisor.Spec{
		Path:   "git",
		Args:   []string{"--git-dir", repo.GitDir, "config", "--type=bool", "--get", key},
		Stdout: &stdout,
	})
	if err != nil {
		return "", false, fmt.Errorf("spawn git config --get %s: %w", key, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, waitErr := proc.Wait(runCtx)
	if waitErr != nil {
		_ = proc.Stop(ctx, stopGrace)
		return "", false, fmt.Errorf("git config --get %s did not complete within %s: %w", key, timeout, waitErr)
	}
	switch result.ExitCode {
	case 0:
		val := strings.TrimSpace(stdout.String())
		if val != "true" && val != "false" {
			return "", false, fmt.Errorf("unexpected value for %s: %q", key, val)
		}
		return val, true, nil
	case 1:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("git config --get %s exited %d", key, result.ExitCode)
	}
}

// MirrorBranchUpstream copies branch.<branch>.remote/branch.<branch>.merge
// out of the agent-owned git-dir's own config and into the runtime's own
// .git/config, as the runtime's own identity (RuntimeGit) -- the exact
// same "agent writes, runtime never sees it" gap MirrorSparseCheckout
// already closes for core.sparseCheckout, applied here to git's own
// upstream-tracking config.
//
// (Correction, review): a `checkout -b <branch> origin/<x> --` run
// through the agent-owned git-dir (syncOne's own checkoutBranch, when the
// branch does not yet exist locally) makes git's default
// branch.autoSetupMerge write branch.<branch>.remote/.merge into
// $GIT_DIR/config -- which, since §30.5, is the AGENT's own config, never
// shared with the runtime and deleted outright by Seed on every boot
// (seed.go's own os.RemoveAll(repo.GitDir) at step 0). Before §30.5 the
// same checkout ran directly against the runtime's own .git, so the
// runtime kept the upstream it set for itself; after it, that tracking
// silently evaporates the moment this sandbox next reboots warm, and
// `git pull`/a bare `git push` in that branch then fail outright ("no
// tracking information" / "has no upstream branch") for a branch that
// worked identically before this package existed. Called once, right
// after syncOne's own agent-side `checkout -b` succeeds -- mirroring
// MirrorSparseCheckout's own "run immediately after the agent-side
// mutation that could have changed it" placement exactly.
//
// A key that is unset on the agent side (exit 1 -- e.g. checkoutBase fell
// through to "HEAD", which autoSetupMerge never sets tracking for) is
// left untouched on the runtime side: nothing to mirror, and there is no
// stale runtime-side value this call needs to clear (a freshly created
// branch has no pre-existing branch.<branch>.* entry on either side).
func MirrorBranchUpstream(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, cred *syscall.Credential, branch string, timeout, stopGrace time.Duration) error {
	if err := reposource.ValidateBranch(branch); err != nil {
		return fmt.Errorf("gitdir: mirror branch upstream: invalid branch %q: %w", branch, err)
	}

	for _, suffix := range []string{"remote", "merge"} {
		key := "branch." + branch + "." + suffix
		val, ok, err := readAgentStringConfig(ctx, sup, repo, key, timeout, stopGrace)
		if err != nil {
			return fmt.Errorf("gitdir: mirror branch upstream: read agent %s: %w", key, err)
		}
		if !ok {
			continue
		}
		result, err := RuntimeGit(ctx, sup, cred, repo.WorkTree, nil, nil, timeout, stopGrace, "config", key, val)
		if err != nil {
			return fmt.Errorf("gitdir: mirror branch upstream: set runtime %s: %w", key, err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("gitdir: mirror branch upstream: git config %s exited %d", key, result.ExitCode)
		}
	}
	return nil
}

// readAgentStringConfig reads key directly from repo.GitDir's own config
// (sandbox-agent's own identity -- an agent-owned file, no elevated read
// needed), returning (value, true, nil) when set, (_, false, nil) when
// the key is entirely unset (exit 1), and an error for any other git
// failure. Unlike readAgentBoolConfig, this accepts any string value
// verbatim (branch.<b>.remote/.merge are a remote name and a full refname
// respectively, neither a bool) -- trimmed of the single trailing
// newline `git config --get` always emits, nothing else validated here;
// callers that need this value not to be an argument-injection risk
// validate it themselves (MirrorBranchUpstream validates branch itself,
// via reposource.ValidateBranch, before ever building this key).
func readAgentStringConfig(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, key string, timeout, stopGrace time.Duration) (string, bool, error) {
	var stdout bytes.Buffer
	proc, err := sup.Spawn(supervisor.Spec{
		Path:   "git",
		Args:   []string{"--git-dir", repo.GitDir, "config", "--get", key},
		Stdout: &stdout,
	})
	if err != nil {
		return "", false, fmt.Errorf("spawn git config --get %s: %w", key, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, waitErr := proc.Wait(runCtx)
	if waitErr != nil {
		_ = proc.Stop(ctx, stopGrace)
		return "", false, fmt.Errorf("git config --get %s did not complete within %s: %w", key, timeout, waitErr)
	}
	switch result.ExitCode {
	case 0:
		return strings.TrimSuffix(stdout.String(), "\n"), true, nil
	case 1:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("git config --get %s exited %d", key, result.ExitCode)
	}
}
