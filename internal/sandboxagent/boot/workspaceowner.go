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

	"golang.org/x/sys/unix"
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

// ChownWorkspaceForRuntime recursively changes the owner of workspaceDir
// and of every entry under it to uid/gid -- the SAME uid/gid
// cmd/sandbox-agent/main.go builds the agent runtime's own *syscall.
// Credential from (boot.Config.RuntimeUID/RuntimeGID).
//
// Why this exists at all: the clone (internal/sandboxagent/gitclone.
// CloneAll), every repo-configured setup hook, and the generated AGENTS.md
// manifest and opencode.json config are written by sandbox-agent's OWN
// process, under its own identity (supervisor.Spec's nil-Credential
// default; see supervisor.Spec.Credential's own doc comment for why that
// is correct, not an oversight). services.yml commands are the exception:
// RunBoot drops them to the runtime's credential. Left untouched, what
// sandbox-agent wrote would stay owned by sandbox-agent's own uid with an
// ordinary umask -- world or group READABLE in the common case, but not
// group/other WRITABLE -- which would leave the isolated runtime able to
// read the workspace but not edit files, `git commit`, or run a build,
// silently breaking the one piece of legitimate agent behavior §30.5
// explicitly requires be preserved.
//
// It can run several times per boot, and never on a tree it can assume is
// quiet. Its callers are gitclone.CloneAll's chownRepo (per freshly cloned
// repo), RunBoot's chownWorkspace (per repo that has services.yml commands,
// just before they start) and run()'s post-boot pass over the whole
// workspace in cmd/sandbox-agent, which precedes the WS bridge ever letting
// a "prompt" reach the runtime. By RunBoot's pass for a repo, the
// services.yml processes of the repos before it are already running as
// the runtime uid; by the post-boot pass all of them are, alongside
// opencode serve. Those processes own what the earlier passes handed
// them, and write in it while this walks it: lock and temp files created
// and removed, cache directories reset -- and, if a repo's own services.yml
// is hostile, a directory swapped for a symlink.
//
// So the walk never resolves a path through a name such a writer could
// have changed (chownTree). workspaceDir is opened O_DIRECTORY|O_NOFOLLOW;
// every directory is listed from its own open fd; the walk descends only
// through openat(dirfd, name, O_DIRECTORY|O_NOFOLLOW) and re-owns each
// directory through the fd that opened it; and every other entry is
// re-owned with fchownat(dirfd, name, AT_SYMLINK_NOFOLLOW). A symlink --
// repo-authored, or planted where a directory was listed a moment before --
// is never followed, listed or descended through (if it is there when its
// name is re-owned, the link's own inode is what gets re-owned), and a
// parent renamed away or swapped mid-walk changes nothing for the entries
// under it, because the walk holds the directory it actually opened rather
// than the name it opened it by. What the walk re-owns is exactly the set
// of inodes reachable as entries of directories it opened by descent from
// workspaceDir. The path-based walk this replaced (filepath.WalkDir with
// os.Lchown) resolved every parent component again at each call: Lchown
// declines to follow only the LAST component, and os.ReadDir follows a
// directory swapped for a symlink after it was listed, so a runtime-uid
// writer could make root list and re-own a directory outside the
// workspace, of which the credential cache is a plausible choice.
//
// Two limits, named so they are not mistaken for closed. The components of
// the path this is given, other than the last, are resolved by name once,
// at the first open, and trusted: every directory holding one of them must
// be writable by root alone. That holds for the default /workspace, where
// the only such name in a per-repo call's /workspace/<repo> is workspace
// itself, an entry of /. And an inode hard-linked into the tree is
// re-owned under every name it has, inside the tree or not -- what that
// allows is bounded by what the kernel lets the runtime hard-link
// (fs.protected_hardlinks), not by this walk.
//
// An entry that disappears before the walk reaches it is not a failure:
// there is nothing left to re-own. ENOENT for a name in a directory the
// walk holds open -- from fstatat, fchownat or openat -- skips that entry,
// and ENOENT on a directory's own fd, once the directory has been removed
// after the walk opened it, skips that directory; no path is resolved
// again to double-check either. An entry created after the walk listed its
// directory is not re-owned by this pass, which re-owns a
// snapshot: the writers it runs beside are the runtime's own processes, so
// what they create is already the runtime's. workspaceDir itself must
// still exist, and must be a directory rather than a symlink to one.
//
// Any other failure (an entry that cannot be re-owned, a directory that
// cannot be opened or listed, a tree deeper than maxChownDepth) aborts the
// whole walk immediately and returns that error, wrapped -- this
// function's own callers treat any error here as fatal to boot (see the
// post-boot call site's own comment for why: a partially re-owned
// workspace is worse than a clearly-failed boot).
func ChownWorkspaceForRuntime(workspaceDir string, uid, gid uint32) error {
	return chownTree(workspaceDir, uid, gid, maxChownDepth, nil)
}

// maxChownDepth bounds how many directory levels below workspaceDir the
// walk descends. The walk holds one open directory fd per level between
// workspaceDir and the directory it is in -- that is what makes it immune
// to a parent being renamed -- so this is also the bound on how many fds
// it holds at once. A deeper tree fails the walk, and boot, rather than
// being re-owned in part. Not a timeout: nothing here waits.
const maxChownDepth = 1024

// errTreeTooDeep is what the walk reports for a tree deeper than its bound.
var errTreeTooDeep = errors.New("directory tree is deeper than the re-own walk descends")

// walkPoint names a moment in the walk at which a concurrent writer's
// change to the tree matters. A test hooks each one to make that change
// happen exactly there; production passes no hook.
type walkPoint int

const (
	// beforeEntry: an entry has been listed, and the walk is about to look
	// at what it is.
	beforeEntry walkPoint = iota
	// beforeChown: an entry that was not a directory when the walk looked
	// is about to be re-owned.
	beforeChown
	// beforeDescend: an entry that was a directory is about to be opened,
	// re-owned through that fd, and listed.
	beforeDescend
	// beforeList: a directory has been opened and re-owned and is about to
	// be listed.
	beforeList
)

// chownTree is ChownWorkspaceForRuntime with its depth bound and a hook
// injectable. hook, when non-nil, is called with the entry's path at each
// walkPoint; the path is for the hook and for error messages only, and is
// never passed to a system call.
func chownTree(workspaceDir string, uid, gid uint32, maxDepth int, hook func(walkPoint, string)) error {
	w := treeWalker{uid: int(uid), gid: int(gid), maxDepth: maxDepth, hook: hook}
	if err := w.walk(workspaceDir); err != nil {
		return fmt.Errorf("boot: chown workspace %s for runtime uid=%d gid=%d: %w", workspaceDir, uid, gid, err)
	}
	return nil
}

type treeWalker struct {
	uid, gid int
	maxDepth int
	hook     func(walkPoint, string)
}

// dirFrame is one directory the walk holds open.
type dirFrame struct {
	dir  *os.File // owns fd
	fd   int
	path string // for errors and hooks only
	// subdirs are the entries that were directories when the walk looked,
	// still to be opened, re-owned and descended into.
	subdirs []string
}

func (w *treeWalker) at(point walkPoint, path string) {
	if w.hook != nil {
		w.hook(point, path)
	}
}

// walk re-owns root and everything under it, depth first, with an explicit
// stack rather than recursion: the stack holds one frame per open level.
func (w *treeWalker) walk(root string) error {
	rootDir, err := openDirNoFollow(unix.AT_FDCWD, root, root)
	if err != nil {
		return &fs.PathError{Op: "open", Path: root, Err: err}
	}
	stack := []dirFrame{{dir: rootDir, fd: int(rootDir.Fd()), path: root}}
	defer func() {
		for i := range stack {
			_ = stack[i].dir.Close()
		}
	}()

	// Unlike any directory under it, the root vanishing is an error.
	if _, err := w.enter(&stack[0]); err != nil {
		return err
	}

	for len(stack) > 0 {
		top := len(stack) - 1
		if len(stack[top].subdirs) == 0 {
			_ = stack[top].dir.Close()
			stack = stack[:top]
			continue
		}
		parentFD, parentPath := stack[top].fd, stack[top].path
		name := stack[top].subdirs[0]
		stack[top].subdirs = stack[top].subdirs[1:]
		path := filepath.Join(parentPath, name)

		w.at(beforeDescend, path)
		child, err := openDirNoFollow(parentFD, name, path)
		switch {
		case err == nil:
		case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ELOOP):
			// Gone, or no longer a directory, since the walk looked at it: a
			// symlink or anything else put in its place is never descended
			// through, and was put there after the walk had passed.
			continue
		default:
			return &fs.PathError{Op: "openat", Path: path, Err: err}
		}
		stack = append(stack, dirFrame{dir: child, fd: int(child.Fd()), path: path})
		if depth := len(stack) - 1; depth > w.maxDepth {
			return fmt.Errorf("%s: %w (more than %d levels)", path, errTreeTooDeep, w.maxDepth)
		}
		if gone, err := w.enter(&stack[len(stack)-1]); gone {
			// Removed after it was opened: every entry it had went with it,
			// since a directory is only removed once it is empty.
			continue
		} else if err != nil {
			return err
		}
	}
	return nil
}

// enter re-owns the directory open in f, lists it from that same fd, and
// re-owns every entry in it that is not a directory, recording the ones
// that are: each of those is opened, and re-owned through that fd, when
// the walk descends into it. A directory is re-owned through its fd rather
// than by name so that the directory re-owned is the one whose entries the
// walk then re-owns, whatever its name names by now -- and because APFS
// answers a chown by name of a directory being removed during the call
// with EINVAL instead of ENOENT, where a chown through an fd it already
// holds succeeds.
//
// gone reports that the directory itself was removed after it was opened,
// and err is then the error that said so. Only an operation on the
// directory's own fd can say that -- fchown, which APFS fails with ENOENT
// when the directory is removed while it runs, or listing, which Linux
// fails with ENOENT on a removed directory (darwin lists it as empty). An
// ENOENT for an entry in it is skipped here, never returned, so it is never
// mistaken for the whole directory being gone.
func (w *treeWalker) enter(f *dirFrame) (gone bool, err error) {
	if err := fchown(f.fd, w.uid, w.gid); err != nil {
		return errors.Is(err, unix.ENOENT), &fs.PathError{Op: "fchown", Path: f.path, Err: err}
	}
	w.at(beforeList, f.path)
	names, err := f.dir.Readdirnames(-1)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist), err // an *fs.PathError naming f.path
	}
	for _, name := range names {
		path := filepath.Join(f.path, name)
		w.at(beforeEntry, path)
		isDir, err := isDirNoFollow(f.fd, name)
		switch {
		case err == nil:
		case errors.Is(err, unix.ENOENT):
			continue // gone since it was listed
		default:
			return false, &fs.PathError{Op: "fstatat", Path: path, Err: err}
		}
		if isDir {
			f.subdirs = append(f.subdirs, name)
			continue
		}
		w.at(beforeChown, path)
		switch err := fchownatNoFollow(f.fd, name, w.uid, w.gid); {
		case err == nil:
		case errors.Is(err, unix.ENOENT):
			continue // gone since it was looked at
		default:
			return false, &fs.PathError{Op: "fchownat", Path: path, Err: err}
		}
	}
	return false, nil
}

// openDirNoFollow opens name, relative to dirfd, as a directory -- failing
// with ELOOP or ENOTDIR, never following, if name is a symlink. path is
// only the name the returned *os.File reports in its errors.
func openDirNoFollow(dirfd int, name, path string) (*os.File, error) {
	for {
		fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	}
}

// fchownatNoFollow re-owns the entry name in the directory open as dirfd --
// the entry itself, never what it points at if it is a symlink.
func fchownatNoFollow(dirfd int, name string, uid, gid int) error {
	for {
		if err := unix.Fchownat(dirfd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != unix.EINTR {
			return err
		}
	}
}

func fchown(fd, uid, gid int) error {
	for {
		if err := unix.Fchown(fd, uid, gid); err != unix.EINTR {
			return err
		}
	}
}

// isDirNoFollow reports whether the entry name in the directory open as
// dirfd is a directory -- a symlink to one is not.
func isDirNoFollow(dirfd int, name string) (bool, error) {
	var st unix.Stat_t
	for {
		err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		return st.Mode&unix.S_IFMT == unix.S_IFDIR, nil
	}
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
