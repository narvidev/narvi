// This file implements this Step's own scoping-discipline requirement
// for TECHNICAL_PLAN.md §30.5's own "inventory what the runtime
// legitimately uses and preserve exactly that" rule: the workspace tree
// is named there, verbatim, as something the isolated agent runtime must
// keep working access to.

package boot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// On the asymmetry between this and the credential itself, which is why
// only one of them needed a privilege-free guard added:
//
// Removing the Credential from the spawn fails OPEN and silently -- the
// runtime keeps working, at sandbox-agent's own uid, with the boundary
// gone and nothing to notice. That is why there is now a test asserting
// the credential reaches the kernel's attributes, runnable anywhere.
//
// I wrote here that removing this chown "fails closed and loudly", and
// used that as the reason it needed no guard. The review showed the
// sentence was aimed at the wrong thing. What fails loudly is the chown
// being PRESENT: it hands each repository to the runtime, and every git
// command sandbox-agent then runs in that repository is refused for
// dubious ownership -- the end-of-turn push included. That is what
// internal/sandboxagent/githarden exists for.
//
// The asymmetry the sentence was reaching for is real, and it belongs to
// the credential rather than to this: removing the credential from the
// spawn fails OPEN and silently, because the runtime keeps working at
// sandbox-agent's own uid with the boundary simply gone. That is why the
// credential has a privilege-free guard and this does not -- but the
// reason had to be corrected before it could be relied on again.

// ChownWorkspaceForRuntime recursively changes the owner of every entry
// under workspaceDir to uid/gid -- the SAME uid/gid
// cmd/sandbox-agent/main.go builds the agent runtime's own *syscall.
// Credential from (boot.Config.RuntimeUID/RuntimeGID).
//
// Why this exists at all: everything under workspaceDir up to this point
// -- the clone itself (internal/sandboxagent/gitclone.CloneAll), every
// repo-configured setup hook, every services.yml command, the generated
// AGENTS.md manifest and opencode.json config -- is written by
// sandbox-agent's OWN trusted process (git clone/hooks/services all run
// via supervisor.Spec's own nil-Credential default, sandbox-agent's own
// identity; see supervisor.Spec.Credential's own doc comment for why
// that is correct, not an oversight). Left untouched, those files would
// stay owned by sandbox-agent's own uid with an ordinary umask -- world
// or group READABLE in the common case, but not group/other WRITABLE --
// which would leave the isolated runtime able to read the workspace but
// not edit files, `git commit`, or run a build, silently breaking the one
// piece of legitimate agent behavior §30.5 explicitly requires be
// preserved. This call closes that gap by re-owning the entire tree to
// the runtime's own uid/gid, once, after every boot-time writer
// (gitclone, hooks, services, the manifest writer) has already finished
// and before the WS bridge would ever let a "prompt" command reach the
// runtime -- see this function's own call site in
// cmd/sandbox-agent/main.go for the exact ordering.
//
// Lchown (never Chown/os.Chown, which follows a symlink to its target)
// is deliberate: workspaceDir's own content is CUSTOMER-CONTROLLED git
// repo content by the time this walks it, and a repo-authored symlink
// pointing outside workspaceDir (e.g. at an arbitrary absolute path) must
// never cause this call to re-own a file outside the tree it was asked
// to re-own -- Lchown changes the ownership of the symlink INODE ITSELF,
// never whatever it points at.
//
// A partial failure (one entry's Lchown erroring, e.g. a filesystem
// quirk) aborts the whole walk immediately and returns that error,
// wrapped -- this function's own caller treats any error here as fatal
// to boot (see that call site's own comment for why: a partially
// re-owned workspace is worse than a clearly-failed boot).
//
// An entry that disappears between being listed and being re-owned is
// not such a failure: there is nothing left to re-own. The walk runs
// while other processes may be writing in the tree -- cmd/sandbox-agent's
// post-boot pass covers every repository after RunBoot has already started
// its services.yml processes, and opencode serve is already running -- and
// a lock or temp file created and removed under it (git's own index.lock
// and HEAD.lock among them) used to fail the whole boot with ENOENT. See
// lchownEntry for how a vanished entry is told apart from any other
// ENOENT. workspaceDir itself must still exist.
func ChownWorkspaceForRuntime(workspaceDir string, uid, gid uint32) error {
	return chownTree(workspaceDir, uid, gid, os.Lchown)
}

// chownTree is ChownWorkspaceForRuntime with its Lchown call injectable, so
// a test can make an entry vanish at exactly the point a concurrent writer
// would.
func chownTree(workspaceDir string, uid, gid uint32, lchown func(path string, uid, gid int) error) error {
	err := filepath.WalkDir(workspaceDir, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A directory listed by its parent but removed before it could
			// be read. Never the root: a missing workspaceDir is an error.
			if path != workspaceDir && errors.Is(walkErr, fs.ErrNotExist) && isGone(path) {
				return nil
			}
			return walkErr
		}
		return lchownEntry(path, int(uid), int(gid), lchown)
	})
	if err != nil {
		return fmt.Errorf("boot: chown workspace %s for runtime uid=%d gid=%d: %w", workspaceDir, uid, gid, err)
	}
	return nil
}

// lchownVanishAttempts bounds how many times lchownEntry retries a path that
// reported ENOENT yet still exists -- a name recreated by a concurrent writer
// between the failed call and the check (git recreates index.lock on every
// operation). Not a timeout: nothing here waits.
const lchownVanishAttempts = 3

// lchownEntry re-owns one walked entry, treating "it no longer exists" as
// done rather than as an error.
//
// Lchown never resolves a symlink, so an ENOENT from it can only mean the
// entry itself was gone at the moment of the call. That is confirmed with a
// fresh Lstat before anything is skipped. An entry that is still there was
// recreated in between and is re-owned again; one that keeps reporting
// ENOENT while existing -- which only a symlink-FOLLOWING chown can do, on a
// dangling link -- is returned as the error it is, so replacing Lchown with
// Chown still fails loudly (TestChownWorkspaceForRuntime_DoesNotFollowSymlinks).
func lchownEntry(path string, uid, gid int, lchown func(path string, uid, gid int) error) error {
	var err error
	for range lchownVanishAttempts {
		err = lchown(path, uid, gid)
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if isGone(path) {
			return nil
		}
	}
	return err
}

// isGone reports whether path does not exist right now (never following a
// symlink).
func isGone(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// RuntimeHomeDir is where the dropped agent runtime's own home lives, and
// why it is not sandbox-agent's.
//
// Several things the runtime must READ are written by sandbox-agent into
// a home directory: the global OpenCode configuration document (§27.2),
// and the cloud-identity material rendered for it (§27.4). Those files are
// world-readable by mode, which made them look fine -- but in production
// sandbox-agent is root, its home is /root, and /root is not traversable
// by another uid. A 0644 file inside a 0700 directory is unreadable, and
// the failure surfaces as the runtime simply not finding its
// configuration.
//
// So the runtime gets its own home, owned by it. Under the workspace's
// parent rather than inside the workspace, because the workspace is the
// repository tree the agent edits and commits: a home directory appearing
// inside a checkout would show up in git status, and something would
// eventually commit it.
const runtimeHomeDirName = ".narvi-runtime-home"

// RuntimeHomePath returns the runtime's home directory path, given the
// workspace directory it sits beside.
func RuntimeHomePath(workspaceDir string) string {
	return filepath.Join(filepath.Dir(workspaceDir), runtimeHomeDirName)
}

// EnsureRuntimeHome creates the runtime's home directory and gives it to
// the runtime, so the process dropped to that identity can read and write
// its own configuration and caches.
//
// 0700 after the chown: the home belongs to the runtime alone. Nothing
// else needs to read it, and sandbox-agent -- which is root in production
// -- is not blocked by a mode it can always override.
func EnsureRuntimeHome(workspaceDir string, uid, gid uint32) (string, error) {
	home := RuntimeHomePath(workspaceDir)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("boot: create runtime home %s: %w", home, err)
	}
	if err := os.Lchown(home, int(uid), int(gid)); err != nil {
		return "", fmt.Errorf("boot: give runtime home %s to %d:%d: %w", home, uid, gid, err)
	}
	return home, nil
}
