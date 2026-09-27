package boot_test

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/narvidev/narvi/internal/sandboxagent/boot"
)

// lchownUID reports the on-disk owner of path via os.Lstat's own
// FileInfo.Sys() -- NEVER following a symlink, mirroring
// ChownWorkspaceForRuntime's own AT_SYMLINK_NOFOLLOW exactly, so this test
// observes the same thing that function changes. *syscall.Stat_t's own
// Uid field is uint32 on both linux and darwin (verified: `go doc
// syscall.Stat_t` on both GOOS) -- no build tag needed.
func lchownUID(t *testing.T, path string) uint32 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Lstat(%s).Sys() = %T, want *syscall.Stat_t", path, info.Sys())
	}
	return st.Uid
}

// TestChownWorkspaceForRuntime_SelfUID_DoesNotError proves
// ChownWorkspaceForRuntime walks a real, nested tree and returns no error
// using the CALLING process's own current uid/gid (a self-referential
// chown, POSIX-permitted with no privilege at all). Deliberately does
// NOT assert anything about the resulting owner value: chowning a file
// to the uid/gid it ALREADY has is observably identical to never
// chowning it at all -- confirmed live (an earlier version of this test
// asserted owner-equality here and passed even after mutating this
// package's own Lchown call into a pure no-op, i.e. it was vacuous). See
// TestChownWorkspaceForRuntime_ActuallyChangesOwnership below for this
// function's own real, executed proof that ownership actually changes,
// which needs a genuinely different uid and therefore real privilege.
func TestChownWorkspaceForRuntime_SelfUID_DoesNotError(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "repo1", "src", "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	filePath := filepath.Join(nested, "main.go")
	if err := os.WriteFile(filePath, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	selfUID := uint32(os.Getuid())
	selfGID := uint32(os.Getgid())

	if err := boot.ChownWorkspaceForRuntime(root, selfUID, selfGID); err != nil {
		t.Fatalf("ChownWorkspaceForRuntime() error = %v, want nil", err)
	}
}

// TestChownWorkspaceForRuntime_ActuallyChangesOwnership is this
// function's own real, EXECUTED proof that every entry in a nested tree
// is genuinely re-owned: run as root (uid 0), a fresh tree's every entry
// starts owned by uid 0 -- after ChownWorkspaceForRuntime(root, 65534,
// 65534), every single one (root dir, nested dirs, the file) must report
// owner 65534, a REAL, observable change unavailable to the self-uid
// test above.
//
// Needs: Linux, running as root -- an unprivileged caller cannot chown
// ANY path to a uid other than its own current one at all (POSIX:
// CAP_CHOWN required), so this property is undemonstrable without that
// privilege. Same gate/reasoning as
// internal/sandboxagent/supervisor's own requireLinuxRoot.
func TestChownWorkspaceForRuntime_ActuallyChangesOwnership(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("requires linux; running on %s", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		t.Skip("requires running as root (euid 0): chowning to a DIFFERENT uid/gid requires CAP_CHOWN, which a non-root test process does not have")
	}

	root := t.TempDir()
	nested := filepath.Join(root, "repo1", "src", "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	filePath := filepath.Join(nested, "main.go")
	if err := os.WriteFile(filePath, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	const targetUID, targetGID = 65534, 65534
	for _, p := range []string{root, filepath.Join(root, "repo1"), nested, filePath} {
		if got := lchownUID(t, p); got != 0 {
			t.Fatalf("precondition failed: owner of %s = %d, want 0 (root) before ChownWorkspaceForRuntime runs", p, got)
		}
	}

	if err := boot.ChownWorkspaceForRuntime(root, targetUID, targetGID); err != nil {
		t.Fatalf("ChownWorkspaceForRuntime() error = %v, want nil", err)
	}

	for _, p := range []string{root, filepath.Join(root, "repo1"), nested, filePath} {
		if got := lchownUID(t, p); got != targetUID {
			t.Errorf("owner of %s = %d, want %d", p, got, uint32(targetUID))
		}
	}
}

// TestChownWorkspaceForRuntime_DoesNotFollowSymlinks is this Step's own
// real, executed proof that a repo-authored symlink pointing OUTSIDE
// workspaceDir cannot cause this function to re-own (or even touch)
// whatever it points at: the walk re-owns the symlink's own inode
// (fchownat with AT_SYMLINK_NOFOLLOW), never its target.
//
// Re-owning to the caller's own uid/gid proves nothing here: chowning a
// file to the owner it already has looks exactly like never touching it
// (an earlier version compared owner values around a real outside file
// that way and PASSED with the walk mutated to a symlink-following chown).
// The next version relied on a DANGLING link instead, on which a
// following chown fails with ENOENT -- which stopped being a failure once
// the walk had to skip entries that vanish mid-walk, since that ENOENT is
// indistinguishable from one. So this re-owns to an owner the entries do
// not already have (boot.ObservableOwner), and reads the result off both
// sides: each link's own inode must carry it, and neither target may.
func TestChownWorkspaceForRuntime_DoesNotFollowSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	realTarget := filepath.Join(outside, "outside-file")
	if err := os.WriteFile(realTarget, []byte("not the workspace's\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	links := map[string]string{
		"escape-link":   filepath.Join(outside, "this-path-is-never-created"),
		"outside-alias": realTarget,
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
	}
	uid, gid := boot.ObservableOwner(t, root)
	targetUID, targetGID := lchownOwner(t, realTarget)

	if err := boot.ChownWorkspaceForRuntime(root, uid, gid); err != nil {
		t.Fatalf("ChownWorkspaceForRuntime() error = %v, want nil", err)
	}

	for name := range links {
		if u, g := lchownOwner(t, filepath.Join(root, name)); u != uid || g != gid {
			t.Errorf("owner of the symlink %s itself = %d:%d, want %d:%d", name, u, g, uid, gid)
		}
	}
	if u, g := lchownOwner(t, realTarget); u != targetUID || g != targetGID {
		t.Errorf("the symlink's OUTSIDE target was re-owned: %d:%d -> %d:%d", targetUID, targetGID, u, g)
	}
}

// lchownOwner is lchownUID with the gid as well.
func lchownOwner(t *testing.T, path string) (uid, gid uint32) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Lstat(%s).Sys() = %T, want *syscall.Stat_t", path, info.Sys())
	}
	return st.Uid, st.Gid
}

// TestChownWorkspaceForRuntime_NonexistentDir proves a nonexistent
// workspaceDir is a real, propagated error, never a silent success --
// this function's own caller treats any error here as fatal to boot (see
// its own doc comment), so a silently-ignored nonexistent-dir case would
// be a much harder failure to diagnose later.
func TestChownWorkspaceForRuntime_NonexistentDir(t *testing.T) {
	if err := boot.ChownWorkspaceForRuntime(filepath.Join(t.TempDir(), "does-not-exist"), 65534, 65534); err == nil {
		t.Fatal("ChownWorkspaceForRuntime() error = nil, want an error for a nonexistent workspaceDir")
	}
}
