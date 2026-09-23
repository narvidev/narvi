package gitdir

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// unsupportedGitDirEntries names on-disk layouts Seed refuses to run
// against at all, rather than silently mis-sharing them: "reftable" is
// git's newer ref-storage format -- a wholesale replacement for the
// packed-refs-plus-loose-refs shape sharedEntries below assumes -- and
// "shallow" marks a shallow clone, whose object graph carries its own,
// separate completeness invariant this package has not measured the
// symlink shape against. Both are refused loudly here rather than
// producing a git-dir whose sharing this package cannot actually vouch
// for.
var unsupportedGitDirEntries = []string{"reftable", "shallow"}

// sharedEntries is every path Seed shares between the agent-owned git-dir
// and the runtime worktree's own .git via a symlink -- data that carries
// no command (see this package's own top doc comment). isDir distinguishes
// a directory symlink (objects/refs/logs/info) from a file symlink
// (packed-refs/index); every symlink Seed creates is ABSOLUTE, never
// relative, so it resolves correctly regardless of which uid or working
// directory later reads through it. HEAD is deliberately absent from this
// list -- git refuses a symlinked HEAD outright, so it is kept as a real,
// agent-owned file instead, synced by SyncHeadIn/SyncHeadOut (head.go).
var sharedEntries = []struct {
	name  string
	isDir bool
}{
	{"objects", true},
	{"refs", true},
	{"logs", true},
	{"info", true},
	{"packed-refs", false},
	{"index", false},
}

// Seed builds repo.GitDir from scratch, idempotent -- safe to call again
// for an already-seeded repo (e.g. a warm boot reusing the same
// workspace): the whole function is destructive-then-rebuild
// (os.RemoveAll(repo.GitDir) at step 0), never an incremental patch, so
// there is no stale state a second call could leave half-applied. See this
// package's own top doc comment for the shape this produces; the sequence
// below is numbered to match that comment's own step list, measured
// directly against the real git binary rather than assumed.
//
// repoURL is this repo's OWN clone url, from already-validated
// (reposource.ValidateRepoURL, re-checked here too) TRUSTED session
// config -- never read back from the runtime's own worktree, which this
// function exists specifically because the runtime cannot be trusted to
// have left alone.
func Seed(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, repoURL string, cred *syscall.Credential, timeout, stopGrace time.Duration) error {
	if err := reposource.ValidateRepoURL(repoURL); err != nil {
		return fmt.Errorf("gitdir: seed: invalid repo url: %w", err)
	}

	// (0) Remove any agent git-dir already on repo.GitDir BEFORE any guard
	// below runs, so that a REFUSED Seed (unsupported layout, symlinked
	// worktree/.git) never leaves a previous boot's agent git-dir behind.
	// This is the structural fix for the finding that DiscoverRepoSHAs'
	// (boot/fingerprint.go) os.Stat(repo.GitDir) gate -- the thing every
	// caller of boot.CollectFingerprint relies on to skip a repo this exact
	// call did not vouch for -- cannot distinguish "never seeded" from
	// "seeded on an earlier boot, refused on this one": on a warm boot
	// where a repo's Seed is refused, its stale agent git-dir was still on
	// disk from the PRIOR boot, so the gate found it and let git run
	// against a SHA this boot never validated. repo.GitDir lives under a
	// root gitdir.EnsureRoot has already validated (root-owned, 0700, not
	// a symlink) and is fully derived state this package owns end to end,
	// so removing it unconditionally, before even looking at repo.WorkTree,
	// is always safe -- there is nothing under it a later guard could need
	// to inspect first. If this fails, Seed returns the error and the repo
	// is treated as failed, exactly as before.
	if err := os.RemoveAll(repo.GitDir); err != nil {
		return fmt.Errorf("gitdir: seed: remove existing agent git-dir %s: %w", repo.GitDir, err)
	}

	// (0b) wt and wt/.git must both be real directories, never symlinks --
	// the SAME Lstat guard Run itself re-checks on every later spawn (see
	// run.go's own correction), checked here too so a Seed call is never
	// the one place this guard is skipped. Also refuse an unsupported
	// on-disk .git layout before touching anything else -- the agent
	// git-dir this repo may have had is already gone (above), so a refusal
	// past this point leaves nothing for a later CollectFingerprint call to
	// find.
	if err := assertRealDir(repo.WorkTree); err != nil {
		return fmt.Errorf("gitdir: seed: %w", err)
	}
	runtimeGitDir := filepath.Join(repo.WorkTree, ".git")
	if err := assertRealDir(runtimeGitDir); err != nil {
		return fmt.Errorf("gitdir: seed: %w", err)
	}
	for _, entry := range unsupportedGitDirEntries {
		if _, statErr := os.Lstat(filepath.Join(runtimeGitDir, entry)); statErr == nil {
			return fmt.Errorf("gitdir: seed: %s uses an unsupported layout (%s present) -- this package's own symlink shape has not been measured against it", runtimeGitDir, entry)
		}
	}

	// (1) git init -q --bare --template= <agent>. --template= (empty)
	// suppresses whatever hooks/templates the SYSTEM git-init template
	// directory would otherwise seed hooks/ with -- a real, if unusual,
	// exposure were this omitted: a template directory another uid could
	// plant into.
	if _, err := runAgentGit(ctx, sup, timeout, stopGrace, "init", "-q", "--bare", "--template=", repo.GitDir); err != nil {
		return fmt.Errorf("gitdir: seed: %w", err)
	}

	// (2) remove agent/objects, agent/refs -- git init creates both as
	// real, empty directories; both are about to become directory symlinks
	// in step (3) below, and removing them first (rather than a
	// conditional remove-if-exists per shared entry) keeps that step a
	// single, uniform loop.
	if err := os.RemoveAll(filepath.Join(repo.GitDir, "objects")); err != nil {
		return fmt.Errorf("gitdir: seed: remove agent objects: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(repo.GitDir, "refs")); err != nil {
		return fmt.Errorf("gitdir: seed: remove agent refs: %w", err)
	}

	// (3) six symlinks, ABSOLUTE targets.
	for _, entry := range sharedEntries {
		target := filepath.Join(runtimeGitDir, entry.name)
		link := filepath.Join(repo.GitDir, entry.name)
		if err := os.RemoveAll(link); err != nil {
			return fmt.Errorf("gitdir: seed: remove %s before symlinking: %w", link, err)
		}
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("gitdir: seed: symlink %s -> %s: %w", link, target, err)
		}
	}

	// (4) mkdir agent/hooks -- agent-owned, empty. core.hooksPath below
	// also points at /dev/null as defense in depth (githarden's own
	// hardeningFlags carries the identical -c override per invocation),
	// but this directory existing at all, empty and agent-owned, is the
	// structural guarantee: nothing the runtime ever wrote can be a member
	// of it.
	if err := os.MkdirAll(filepath.Join(repo.GitDir, "hooks"), 0o700); err != nil {
		return fmt.Errorf("gitdir: seed: create agent hooks dir: %w", err)
	}

	// (5) agent config.
	agentConfig := [][2]string{
		{"core.bare", "false"},
		{"core.logAllRefUpdates", "true"},
		{"core.hooksPath", "/dev/null"},
		{"gc.auto", "0"},
		{"remote.origin.url", repoURL},
		{"remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
	}
	for _, kv := range agentConfig {
		if _, err := runAgentGit(ctx, sup, timeout, stopGrace, "--git-dir", repo.GitDir, "config", kv[0], kv[1]); err != nil {
			return fmt.Errorf("gitdir: seed: %w", err)
		}
	}

	if err := importSparseCheckoutConfig(ctx, sup, repo, cred, timeout, stopGrace); err != nil {
		return fmt.Errorf("gitdir: seed: %w", err)
	}

	// (6) SyncHeadIn -- brings the agent-owned HEAD up to date with
	// whatever the runtime's own worktree currently has checked out.
	if err := SyncHeadIn(repo); err != nil {
		return fmt.Errorf("gitdir: seed: %w", err)
	}

	// (7) agent dir 0700, files 0600.
	if err := tightenPermissions(repo.GitDir); err != nil {
		return fmt.Errorf("gitdir: seed: %w", err)
	}

	return nil
}

// importSparseCheckoutConfig reads core.sparseCheckout/
// core.sparseCheckoutCone from the RUNTIME's own worktree config (AS THE
// RUNTIME UID, via RuntimeGit -- these are read-only `git config --get`
// calls against a directory the runtime owns, so reading them as the
// runtime's own identity is both correct and the minimum privilege
// needed) and, when set, writes the SAME value into the agent-owned
// config -- the reverse direction of MirrorSparseCheckout (run.go), which
// runs after every later agent-side `sparse-checkout set/disable`. A key
// that is unset on the runtime side (exit 1) is left unset on the agent
// side too -- nothing to import.
func importSparseCheckoutConfig(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, cred *syscall.Credential, timeout, stopGrace time.Duration) error {
	for _, key := range []string{"core.sparseCheckout", "core.sparseCheckoutCone"} {
		var stdout bytes.Buffer
		result, err := RuntimeGit(ctx, sup, cred, repo.WorkTree, &stdout, nil, timeout, stopGrace, "config", "--type=bool", "--get", key)
		if err != nil {
			return fmt.Errorf("read runtime %s: %w", key, err)
		}
		switch result.ExitCode {
		case 0:
			val := strings.TrimSpace(stdout.String())
			if val != "true" && val != "false" {
				return fmt.Errorf("unexpected runtime value for %s: %q", key, val)
			}
			if _, err := runAgentGit(ctx, sup, timeout, stopGrace, "--git-dir", repo.GitDir, "config", key, val); err != nil {
				return fmt.Errorf("set agent %s: %w", key, err)
			}
		case 1:
			// Unset on the runtime side -- nothing to import.
		default:
			return fmt.Errorf("git config --get %s exited %d", key, result.ExitCode)
		}
	}
	return nil
}

// tightenPermissions restricts repo.GitDir itself and the two real,
// agent-written files directly inside it (config, HEAD) to owner-only --
// deliberately NOT recursive into the six shared symlinks: a symlink's own
// mode is not a meaningful security boundary on Linux, and chmod-ing
// through one would follow it onto the runtime-owned target, which this
// function must never touch the permissions of.
func tightenPermissions(gitDir string) error {
	if err := os.Chmod(gitDir, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", gitDir, err)
	}
	for _, name := range []string{"config", "HEAD"} {
		path := filepath.Join(gitDir, name)
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
	}
	return nil
}
