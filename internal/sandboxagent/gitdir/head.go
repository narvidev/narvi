package gitdir

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// maxHeadBytes bounds how much of a worktree's own .git/HEAD SyncHeadIn
// ever reads -- a real HEAD is a few dozen bytes at most ("ref:
// refs/heads/<branch>\n" or a 41-byte detached sha); anything larger is
// already not a shape this function accepts, and this bound keeps a
// runtime-planted oversized file from ever being read in full before that
// rejection.
const maxHeadBytes = 4096

// detachedHeadPattern matches a real detached-HEAD SyncHeadIn accepts: a
// lowercase, 40-character hex object id, alone on its own line.
var detachedHeadPattern = regexp.MustCompile(`^[0-9a-f]{40}\n$`)

// SyncHeadIn brings the agent-owned git-dir's own HEAD up to date with
// whatever repo.WorkTree's own .git/HEAD currently says (runtime -> agent
// direction) -- git refuses a symlinked HEAD outright ("fatal: not a git
// repository"), so this is a REAL, agent-owned file, kept in sync by copy
// rather than shared by symlink like every other piece of this shape (see
// this package's own doc comment).
//
// The source is runtime-authored, untrusted content, root-read from a
// directory the runtime owns -- so this is read defensively, never
// echoed unvalidated, and never trusted to be well-formed:
//
//   - opened O_RDONLY|O_NOFOLLOW|O_NONBLOCK: NOFOLLOW refuses a symlink
//     planted at the HEAD path itself (this function's own caller already
//     guards the .git directory itself is not a symlink -- Seed's own
//     step 0 -- but HEAD is opened fresh on every Run, not only at Seed
//     time); NONBLOCK keeps a FIFO planted at that path from hanging this
//     read rather than failing it outright.
//   - fstat'd after open (not before -- avoids a TOCTOU swap between stat
//     and open) and REJECTED unless it is a regular file no larger than
//     maxHeadBytes.
//   - the content is accepted ONLY if it is EXACTLY one of two shapes:
//     "ref: refs/heads/<name>\n" with reposource.ValidateBranch(name)
//     passing, or a bare 40-character lowercase hex sha followed by "\n"
//     (a detached HEAD). Anything else is a hard error -- the error text
//     never includes the offending bytes: a root process reading an
//     attacker-chosen file and echoing it back is its own exfiltration
//     primitive, distinct from (and not excused by) the read itself being
//     bounded and non-following.
//
// Only once every check above has passed is the agent-owned HEAD actually
// replaced, via write-temp-then-rename (never an in-place truncate+write,
// which would leave a half-written HEAD visible to a concurrent reader).
func SyncHeadIn(repo githarden.Repo) error {
	src := filepath.Join(repo.WorkTree, ".git", "HEAD")

	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("gitdir: sync head in: open %s: %w", src, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("gitdir: sync head in: stat %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("gitdir: sync head in: %s is not a regular file", src)
	}
	if info.Size() > maxHeadBytes {
		return fmt.Errorf("gitdir: sync head in: %s exceeds %d bytes", src, maxHeadBytes)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxHeadBytes+1))
	if err != nil {
		return fmt.Errorf("gitdir: sync head in: read %s: %w", src, err)
	}
	if !validHeadContents(data) {
		// Deliberately never includes data itself -- see this function's
		// own doc comment.
		return fmt.Errorf("gitdir: sync head in: %s has unexpected contents (neither a valid symbolic ref nor a detached sha)", src)
	}

	dst := filepath.Join(repo.GitDir, "HEAD")
	if err := writeFileAtomic(dst, data, 0o600); err != nil {
		return fmt.Errorf("gitdir: sync head in: write %s: %w", dst, err)
	}
	return nil
}

// validHeadContents reports whether data is exactly one of the two shapes
// SyncHeadIn accepts. See its own doc comment.
func validHeadContents(data []byte) bool {
	s := string(data)
	if rest, ok := strings.CutPrefix(s, "ref: refs/heads/"); ok {
		name, ok := strings.CutSuffix(rest, "\n")
		if !ok || strings.Contains(name, "\n") {
			return false
		}
		return reposource.ValidateBranch(name) == nil
	}
	return detachedHeadPattern.MatchString(s)
}

// writeFileAtomic writes data to a fresh temp file beside path, then
// renames it into place -- so a concurrent reader of path never observes
// a partial write, and a failure partway through never corrupts the
// existing file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gitdir-head-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Removing an already-renamed-away temp path is a documented no-op
	// error (ENOENT) from os.Remove -- discarded here on the success path
	// exactly like it would be, deliberately, on the fixed-name precedent
	// this mirrors (internal/sandboxagent/credentials/cache.go's own
	// withFileLock, which also always defers a close/cleanup regardless of
	// outcome).
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file to %s: %w", path, err)
	}
	return nil
}

// SyncHeadOut writes the agent-owned git-dir's own current HEAD back onto
// repo.WorkTree (agent -> runtime direction), as the RUNTIME's own
// identity (cred) -- the reverse of SyncHeadIn, called by Run whenever an
// agent git command actually moved the agent's own HEAD (a checkout, a
// branch creation, ...).
//
// The agent's own HEAD must be a symbolic ref (a branch) -- "ref:
// refs/heads/<branch>\n" -- never a detached sha: the agent side of this
// split never legitimately detaches (every checkoutBranch/checkoutBase
// call in gitclone either switches to or creates a named branch), so a
// detached agent HEAD reaching this function is treated as an error
// rather than silently detaching the runtime's own worktree to match.
//
// The write itself runs `git -C <worktree> symbolic-ref HEAD
// refs/heads/<branch>` AS THE RUNTIME's OWN IDENTITY (RuntimeGit) --
// this is the runtime's own identity acting on its own .git, granted by
// §30.5, and avoids a root-privileged write landing inside a
// runtime-owned directory (the same posture boot/workspaceowner.go's own
// doc comment already establishes for why the chown itself, not its
// absence, is the thing that "fails loudly").
func SyncHeadOut(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, cred *syscall.Credential, timeout, stopGrace time.Duration) error {
	data, err := os.ReadFile(filepath.Join(repo.GitDir, "HEAD"))
	if err != nil {
		return fmt.Errorf("gitdir: sync head out: read agent HEAD: %w", err)
	}

	s := string(data)
	rest, ok := strings.CutPrefix(s, "ref: refs/heads/")
	if !ok {
		return fmt.Errorf("gitdir: sync head out: agent HEAD is detached, never synced out: %q", s)
	}
	branch, ok := strings.CutSuffix(rest, "\n")
	if !ok || strings.Contains(branch, "\n") {
		return fmt.Errorf("gitdir: sync head out: agent HEAD is not a well-formed symbolic ref: %q", s)
	}
	if err := reposource.ValidateBranch(branch); err != nil {
		return fmt.Errorf("gitdir: sync head out: invalid branch %q: %w", branch, err)
	}

	result, err := RuntimeGit(ctx, sup, cred, repo.WorkTree, nil, nil, timeout, stopGrace, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("gitdir: sync head out: %w", err)
	}
	if result.Err != nil {
		return fmt.Errorf("gitdir: sync head out: %w", result.Err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("gitdir: sync head out: git symbolic-ref exited %d", result.ExitCode)
	}
	return nil
}
