package gitclone

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/environment"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// CloneResult is one repo's outcome from CloneAll.
type CloneResult struct {
	Repo sessionconfig.SessionConfigReposElem
	// Primary is true for exactly the repo at position 0 in the manifest
	// (§3.4: "position 0 = primary").
	Primary bool
	// Dir is workspaceDir/<Repo.Name> -- set even when Err is non-nil, so
	// callers (WriteAgentsManifest, boot logging) know where this repo
	// was supposed to land, with exactly one exception: when Repo.Name/
	// Url/Branch itself fails reposource validation (see validateRepoSpec
	// below), Dir is left as the empty string rather than computed via
	// filepath.Join -- an invalid Name is precisely a potential path-
	// traversal payload, so no directory join is ever performed against
	// it at all, not even a "harmless" string-only one.
	Dir string
	// Err is non-nil if this repo's clone failed.
	Err error
}

// CloneAll clones every repo in repos, IN ORDER, into workspaceDir/<name>,
// via a real `git clone` subprocess spawned through sup (never a bare
// exec.Command) -- see doc.go for the full criticality/credential-helper/
// branch semantics. workspaceDir is created (MkdirAll) if it does not
// already exist, since `git clone` itself requires its target's PARENT
// directory to already exist.
//
// pathScope is the session's own Environment.PathScope (§14.1), when one
// is attached -- nil or empty means unscoped, the overwhelming common
// case, and produces ZERO behavior change: no sparse-checkout invocation
// at all. When non-empty, EVERY repo's clone (§3.4/§14.1: a session's
// Environment applies uniformly across its always-a-list repos, there is
// no per-repo scoping concept anywhere in this codebase) is immediately
// followed by `git sparse-checkout set --no-cone -- <patterns...>` --
// applySparseCheckout's own doc comment explains why --no-cone is
// required. pathScope is validated (internal/domain/environment.
// ValidatePathScope) ONCE, before any repo is even attempted: an invalid
// session-wide setting is a fatal configuration error for the whole spawn,
// not a single repo's problem.
//
// On a primary (repos[0]) clone (or, when scoped, sparse-checkout)
// failure, CloneAll returns immediately with a fatal error -- no repo
// after it is attempted, matching RunHooks' own "any fatal failure stops
// immediately" semantics exactly. A secondary repo's failure is logged as
// a warning and does not stop the loop; subsequent repos still get
// cloned. results always reflects every repo actually attempted (in
// order), regardless of outcome.
func CloneAll(
	ctx context.Context,
	sup *supervisor.Supervisor,
	workspaceDir string,
	repos []sessionconfig.SessionConfigReposElem,
	pathScope []string,
	cloneTimeout, stopGrace time.Duration,
) ([]CloneResult, error) {
	if len(repos) == 0 {
		return nil, nil
	}

	if err := environment.ValidatePathScope(pathScope); err != nil {
		return nil, fmt.Errorf("gitclone: invalid path scope: %w", err)
	}

	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return nil, fmt.Errorf("gitclone: create workspace dir %s: %w", workspaceDir, err)
	}

	credHelperArg, err := CredHelperGitArg()
	if err != nil {
		return nil, fmt.Errorf("gitclone: determine credential helper: %w", err)
	}

	scoped := len(pathScope) > 0

	results := make([]CloneResult, 0, len(repos))
	for i, repo := range repos {
		primary := i == 0

		// Validate BEFORE any filepath.Join or sup.Spawn happens for this
		// repo -- repo.Name/Url/Branch are all session-controlled and
		// reach this loop with no upstream validation of their own (see
		// reposource's own package doc comment for the full argument-
		// injection/path-traversal reasoning this closes).
		var dir string
		cloneErr := validateRepoSpec(repo)
		if cloneErr == nil {
			dir = filepath.Join(workspaceDir, repo.Name)
			cloneErr = cloneOne(ctx, sup, credHelperArg, repo, dir, cloneTimeout, stopGrace)
		}
		if cloneErr == nil && scoped {
			cloneErr = applySparseCheckout(ctx, sup, dir, pathScope, cloneTimeout, stopGrace)
		}
		results = append(results, CloneResult{Repo: repo, Primary: primary, Dir: dir, Err: cloneErr})

		if cloneErr == nil {
			continue
		}

		if primary {
			return results, fmt.Errorf("gitclone: primary repo %q failed to clone (fatal): %w", repo.Name, cloneErr)
		}
		platform.Logger(ctx).Warn("gitclone: secondary repo failed to clone, continuing",
			"repo", repo.Name, "error", cloneErr)
	}

	return results, nil
}

// applySparseCheckout runs `git -C <dir> sparse-checkout set --no-cone --
// <patterns...>` for one already-cloned OR already-existing repo (§14.1:
// "domain/gitstate's clone step runs `git sparse-checkout set <globs>` per
// repo when path_scope is present"). Excluded paths never materialize on
// the sandbox filesystem afterward -- §14.1's own enforcement guarantee --
// with exactly one caveat, verified directly against real git behavior
// (sync_test.go), that this function itself now closes: a real `git
// sparse-checkout set` exits 0 (success) yet LEAVES an out-of-scope path on
// disk, untouched, whenever that path carries uncommitted local changes at
// the moment the patterns are applied -- git's own documented reluctance to
// discard dirty content, not a bug in this codebase. That path is reachable
// today ONLY via SyncAll's own stash -> checkout -> pop sequence
// (sync.go), never via CloneAll's fresh, guaranteed-clean checkout -- but
// letting it pass silently here would be exactly the §14.1 bypass this
// whole function exists to prevent, so this is guarded here, once, for
// every caller: stderr is captured, and this call is forced to the "C"
// locale (LC_ALL=C, appended so it wins over any ambient value -- see
// exec.Cmd's own "last duplicate wins" documented Env behavior) so that
// git's own warning text is captured deterministically rather than
// depending on the sandbox's ambient locale. Any stderr output at all,
// alongside a successful (0) exit code, is treated as this exact failure
// mode and returned as a real error -- git's own sparse-checkout set
// produces NO stderr output at all on an unqualified success (verified
// directly, sync_test.go), so this never false-positives on the ordinary
// path.
//
// --no-cone is required -- verified directly against real git behavior,
// not assumed (sync_test.go's own house style, applied here too): git's
// default cone mode REJECTS exactly the gitignore-style glob syntax
// internal/domain/environment.ValidatePathScope already validates (e.g. a
// leading "/" combined with a "*" wildcard, such as "/apps/web/*"),
// demanding directory-only patterns instead ("specify directories rather
// than patterns"); non-cone mode is the one that actually supports
// arbitrary path.Match-syntax globs. "--" ends option parsing for
// everything after it, mirroring cloneOne's own exact defense-in-depth
// convention: even an already-validated pattern should never be
// positionally ambiguous to git's own argument parser -- a pattern
// beginning with "-" passes ValidatePathScope's own glob-SYNTAX check
// (path.Match's own grammar does not forbid a leading "-"), so this is a
// real, not merely theoretical, defense-in-depth gap were "--" omitted.
func applySparseCheckout(ctx context.Context, sup *supervisor.Supervisor, dir string, patterns []string, timeout, stopGrace time.Duration) error {
	// See internal/sandboxagent/gitclone's runGit (sync.go) and
	// githarden.NeutralizeFiltersBestEffort's own doc comments for the full
	// reasoning: `sparse-checkout set` materializes newly-in-scope paths
	// into the working tree, which is exactly the class of operation that
	// can run a content filter -- and unlike cloneOne's own fresh-clone
	// call just above in this file, this function is ALSO reached from
	// syncOne (sync.go) against an ALREADY-EXISTING repo (SyncAll's own
	// deferred pathScope re-narrowing, this function's own doc comment
	// above), where the agent runtime has already had a full turn to
	// write .git/config. Required here, not just at CloneAll's own call
	// site, for exactly the reason githarden.Harden's own doc comment
	// gives for covering a shared runner instead of each caller: a path
	// this function reaches unprotected is a hole regardless of how safe
	// its OTHER caller is.
	if err := githarden.NeutralizeFiltersBestEffort(dir); err != nil {
		return fmt.Errorf("neutralize content filters for %s: %w", dir, err)
	}

	args := append([]string{"-C", dir, "sparse-checkout", "set", "--no-cone", "--"}, patterns...)

	var stderr bytes.Buffer
	proc, err := sup.Spawn(supervisor.Spec{
		Path: "git",
		Args: githarden.Harden(args),
		// Inherit the ambient environment (matching every other call site
		// in this package -- see cloneOne's own doc comment for the full
		// "why nil/inherit is deliberate" reasoning) EXCEPT locale: LC_ALL=C
		// is appended last so it always wins (exec.Cmd's own documented
		// "last duplicate key wins" Env behavior), forcing git's own
		// warning text below to a known, stable, English string regardless
		// of the sandbox's ambient locale.
		Env:    append(os.Environ(), "LC_ALL=C"),
		Stderr: &stderr,
	})
	if err != nil {
		return fmt.Errorf("spawn git sparse-checkout set: %w", err)
	}

	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, waitErr := proc.Wait(stepCtx)
	if waitErr != nil {
		_ = proc.Stop(ctx, stopGrace)
		return fmt.Errorf("git sparse-checkout set: did not complete within %s: %w", timeout, waitErr)
	}
	if result.Err != nil {
		return fmt.Errorf("git sparse-checkout set: %w", result.Err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("git sparse-checkout set: exited %d", result.ExitCode)
	}
	// A successful (0) exit code is NOT sufficient on its own -- see this
	// function's own doc comment above: git leaves a dirty out-of-scope
	// path on disk, untouched, and merely warns on stderr, rather than
	// failing outright. Any stderr output at all here means at least one
	// path did not actually leave the sandbox filesystem despite being out
	// of pathScope -- exactly the §14.1 bypass this whole function exists
	// to prevent -- so this is reported as a real, fatal error rather than
	// accepted silently.
	if stderr.Len() > 0 {
		return fmt.Errorf(
			"git sparse-checkout set: succeeded but left at least one out-of-scope path on disk (uncommitted local changes prevented removal): %s",
			strings.TrimSpace(stderr.String()),
		)
	}
	return nil
}

// isSparseCheckoutEnabled runs `git -C <dir> config --type=bool
// core.sparseCheckout` and reports whether sparse-checkout is CURRENTLY
// active for dir -- verified directly against real git behavior (see
// TestDisableSparseCheckoutIfEnabled_* in sync_test.go), not assumed:
// core.sparseCheckout is unset entirely (a non-zero, exit-1 `git config`
// failure -- "the key does not exist") on a working tree that has never
// had sparse-checkout enabled at all, exits 0 printing "true" once `git
// sparse-checkout set` has ever run, and exits 0 printing "false" once
// `git sparse-checkout disable` has explicitly run afterward (git sets the
// key to false rather than removing it again). A genuinely unexpected
// command failure (any exit code other than the well-understood "unset"
// case) is returned as a real error rather than silently treated as
// "not enabled" -- callers must not blindly run `sparse-checkout disable`
// without first confirming it is actually needed (§19.7: "never blindly
// running disable when it was never enabled, to avoid a wasted subprocess
// on the overwhelming common case"), but an ambiguous/broken check is not
// safe to treat as "definitely not enabled" either.
func isSparseCheckoutEnabled(ctx context.Context, sup *supervisor.Supervisor, dir string, timeout, stopGrace time.Duration) (bool, error) {
	var stdout bytes.Buffer
	proc, err := sup.Spawn(supervisor.Spec{
		Path:   "git",
		Args:   githarden.Args(dir, "config", "--type=bool", "core.sparseCheckout"),
		Stdout: &stdout,
	})
	if err != nil {
		return false, fmt.Errorf("spawn git config core.sparseCheckout: %w", err)
	}

	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, waitErr := proc.Wait(stepCtx)
	if waitErr != nil {
		_ = proc.Stop(ctx, stopGrace)
		return false, fmt.Errorf("git config core.sparseCheckout: did not complete within %s: %w", timeout, waitErr)
	}
	if result.Err != nil {
		return false, fmt.Errorf("git config core.sparseCheckout: %w", result.Err)
	}
	switch result.ExitCode {
	case 0:
		return strings.TrimSpace(stdout.String()) == "true", nil
	case 1:
		// The key is simply unset -- sparse-checkout has never been enabled
		// for this working tree. Not an error.
		return false, nil
	default:
		return false, fmt.Errorf("git config core.sparseCheckout: exited %d", result.ExitCode)
	}
}

// disableSparseCheckoutIfEnabled implements §19.7's own sparse-checkout-
// disable hardening: when a session's own pathScope is EMPTY (the
// unscoped case -- SyncAll's own caller, syncOne, only invokes this
// branch when len(pathScope) == 0) but the on-disk workspace at dir is
// CURRENTLY sparse-checked-out (from whatever baked/restored state it
// came from -- §19.1: shared images are always built unscoped, but a
// snapshot_restore boot can restore a SCOPED session's own snapshot into
// an unscoped session's config, and a repo_image boot's own workspace may
// simply have been left sparse by an earlier scoped session that shared
// the same on-disk state), this disables sparse-checkout so the full tree
// actually materializes -- the reverse direction of applySparseCheckout
// above, which this codebase had no branch for at all before this Step.
//
// Cheap and unconditional to CALL for every unscoped session (§19.7: "it
// must run for EVERY unscoped session, unconditionally checking whether
// disabling is even needed") -- but never blindly runs `sparse-checkout
// disable` itself: isSparseCheckoutEnabled is checked FIRST, so the
// overwhelming common case (a workspace that was never sparse to begin
// with) costs exactly one cheap `git config` subprocess, never a second,
// wasted `sparse-checkout disable` invocation.
func disableSparseCheckoutIfEnabled(ctx context.Context, sup *supervisor.Supervisor, dir string, timeout, stopGrace time.Duration) error {
	enabled, err := isSparseCheckoutEnabled(ctx, sup, dir, timeout, stopGrace)
	if err != nil {
		return fmt.Errorf("determine whether sparse-checkout is enabled: %w", err)
	}
	if !enabled {
		return nil
	}

	// `sparse-checkout disable` re-materializes every previously-excluded
	// path into the working tree -- the same class of operation as
	// applySparseCheckout's own identical call just above in this file,
	// and reached the same way, from syncOne against an already-existing
	// repo. See that call's own comment and
	// githarden.NeutralizeFiltersBestEffort's own doc comment for the full
	// reasoning.
	if err := githarden.NeutralizeFiltersBestEffort(dir); err != nil {
		return fmt.Errorf("neutralize content filters for %s: %w", dir, err)
	}

	proc, err := sup.Spawn(supervisor.Spec{
		Path: "git",
		Args: githarden.Args(dir, "sparse-checkout", "disable"),
	})
	if err != nil {
		return fmt.Errorf("spawn git sparse-checkout disable: %w", err)
	}

	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, waitErr := proc.Wait(stepCtx)
	if waitErr != nil {
		_ = proc.Stop(ctx, stopGrace)
		return fmt.Errorf("git sparse-checkout disable: did not complete within %s: %w", timeout, waitErr)
	}
	if result.Err != nil {
		return fmt.Errorf("git sparse-checkout disable: %w", result.Err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("git sparse-checkout disable: exited %d", result.ExitCode)
	}
	return nil
}

// validateRepoSpec runs every internal/domain/reposource validator this
// repo's fields need, in order, stopping at the first failure -- Branch
// is validated only when non-nil (§3.4: nil means "the repo's own
// default branch", reposource.ValidateBranch is never invoked for that
// case). Called BEFORE any filepath.Join or sup.Spawn happens for this
// repo (see CloneAll's own loop and reposource's own package doc comment
// for the argument-injection/path-traversal reasoning this closes).
func validateRepoSpec(repo sessionconfig.SessionConfigReposElem) error {
	if err := reposource.ValidateRepoName(repo.Name); err != nil {
		return fmt.Errorf("gitclone: invalid repo name: %w", err)
	}
	if err := reposource.ValidateRepoURL(repo.Url); err != nil {
		return fmt.Errorf("gitclone: invalid repo url: %w", err)
	}
	if repo.Branch != nil {
		if err := reposource.ValidateBranch(*repo.Branch); err != nil {
			return fmt.Errorf("gitclone: invalid repo branch: %w", err)
		}
	}
	return nil
}

// cloneOne spawns `git clone` for exactly one repo and waits for it,
// bounded by cloneTimeout -- mirroring internal/sandboxagent/boot's own
// runHook precisely: a hang is stopped (bounded by stopGrace, using the
// OUTER ctx for the Stop call, not the already-expired clone-scoped
// context) and reported as a timeout failure; a non-zero exit or a wait
// failure is likewise a real, returned error.
//
// Deliberately NOT preceded by githarden.NeutralizeFiltersBestEffort, unlike
// applySparseCheckout/disableSparseCheckoutIfEnabled below and runGit
// (sync.go): dir does not exist yet when this runs (CloneAll's own doc
// comment: workspaceDir's PARENT is created via MkdirAll, but `git
// clone` itself requires its OWN target to be empty or absent), so
// .git/config is created by THIS clone, empty, with nothing having had
// any chance to write a filter driver into it yet. The internal checkout
// `git clone` performs as its own last step is safe for the same reason
// a missing filter driver is a no-op passthrough (gitattributes(5)):
// there is no config yet for .gitattributes' filter= attribute to
// resolve against.
func cloneOne(
	ctx context.Context,
	sup *supervisor.Supervisor,
	credHelperArg string,
	repo sessionconfig.SessionConfigReposElem,
	dir string,
	cloneTimeout, stopGrace time.Duration,
) error {
	args := []string{"clone", "-c", "credential.helper=" + credHelperArg}
	if repo.Branch != nil {
		args = append(args, "--branch", *repo.Branch)
	}
	// "--" ends option parsing for everything after it (verified directly
	// against real `git clone` behavior, not assumed) -- defense in depth
	// alongside validateRepoSpec's own rejection above: even an already-
	// validated repo.Url/dir should never be positionally ambiguous to
	// git's own argument parser.
	args = append(args, "--", repo.Url, dir)

	proc, err := sup.Spawn(supervisor.Spec{
		Path: "git",
		Args: githarden.Harden(args),
		// Env is DELIBERATELY left at its zero value (nil, "inherit this
		// process's own environment") -- a reviewed choice, not an
		// oversight. git's own credential.helper mechanism (credHelperArg,
		// configured above via CredHelperGitArg) re-execs THIS SAME
		// sandbox-agent binary as `<binary> credential-helper get`, as
		// git's OWN child process, inheriting whatever env git itself
		// received here -- i.e. exactly what this Spec.Env carries,
		// nothing more. cmd/sandbox-agent's own runCredentialHelper calls
		// boot.Load(), which reads NARVI_SESSION_CONFIG via os.Getenv and
		// fails outright ("nothing to fetch credentials for") if it is
		// absent, so stripping it here would BREAK git authentication for
		// every private repo clone -- a real functional regression, not a
		// hardening win. A hand-built allowlist would also risk silently
		// omitting something the real `git` binary or its transport
		// (http/ssh) legitimately needs (PATH, HOME, an ssh-agent socket,
		// ...) that isn't yet enumerated anywhere in this codebase.
	})
	if err != nil {
		return fmt.Errorf("spawn git clone for %s: %w", repo.Name, err)
	}

	cloneCtx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()

	result, waitErr := proc.Wait(cloneCtx)
	if waitErr != nil {
		_ = proc.Stop(ctx, stopGrace)
		return fmt.Errorf("git clone %s: did not complete within %s: %w", repo.Name, cloneTimeout, waitErr)
	}

	if result.Err != nil {
		return fmt.Errorf("git clone %s: %w", repo.Name, result.Err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("git clone %s: exited %d", repo.Name, result.ExitCode)
	}
	return nil
}

// CredHelperGitArg builds the exact `-c credential.helper=...` value: the
// CURRENTLY RUNNING binary's own absolute path (os.Executable()) plus the
// "credential-helper" subcommand, prefixed with `!` so git runs this exact
// shell command rather than treating it as a suffix appended to
// "git-credential-" (git itself appends the final "get"/"store"/"erase"
// argument when it invokes the helper). Exported (§9.3, "e2e happy
// path") so cmd/sandbox-agent's own HandlePush can configure the SAME
// per-invocation credential helper for `git push` that CloneAll already
// configures for `git clone` -- one shared implementation, two callers,
// never a second copy of this exact string-building logic.
func CredHelperGitArg() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("os.Executable: %w", err)
	}
	return fmt.Sprintf("!'%s' credential-helper", exePath), nil
}
