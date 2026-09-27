package boot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

// The walk runs as root while services.yml processes run as the runtime
// uid and own the tree it walks. The tests in this file stand in for a
// hostile one: at the exact moment a directory has been listed, or an
// entry in it is about to be re-owned, a directory is replaced by a
// symlink to a directory OUTSIDE the workspace. The property under test is
// that nothing outside is ever re-owned. A path-based walk fails every one
// of these: Lchown resolves each parent component again, and os.ReadDir
// follows a directory swapped for a symlink after it was listed.

// ObservableOwner is observableOwner, for the boot_test package's tests.
var ObservableOwner = observableOwner

// observableOwner returns a uid/gid the calling process can re-own entries
// it creates under dir to, and that those entries do not already have --
// so whether an entry was re-owned can be read off its owner. As root that
// is 65534:65534. Otherwise it is the caller's own uid with a supplementary
// group other than the one new entries get: an owner may chown a file to
// any group it belongs to, no privilege needed. Skips when neither exists.
func observableOwner(t *testing.T, dir string) (uid, gid uint32) {
	t.Helper()
	if os.Geteuid() == 0 {
		return 65534, 65534
	}
	probe := filepath.Join(dir, ".owner-probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", probe, err)
	}
	_, newGID := ownerOf(t, probe)
	if err := os.Remove(probe); err != nil {
		t.Fatalf("Remove(%s): %v", probe, err)
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatalf("Getgroups: %v", err)
	}
	for _, g := range groups {
		if uint32(g) != newGID {
			return uint32(os.Getuid()), uint32(g)
		}
	}
	t.Skip("needs root, or membership in a second group, to observe a re-own")
	return 0, 0
}

// ownerOf reports path's own owner, never following a symlink.
func ownerOf(t *testing.T, path string) (uid, gid uint32) {
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

// owners records the owner of every entry under dir, dir included.
func owners(t *testing.T, dir string) map[string][2]uint32 {
	t.Helper()
	out := map[string][2]uint32{}
	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		uid, gid := ownerOf(t, path)
		out[path] = [2]uint32{uid, gid}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
	return out
}

// requireUntouched fails for every entry under dir whose owner is no longer
// the one recorded in before.
func requireUntouched(t *testing.T, dir string, before map[string][2]uint32) {
	t.Helper()
	after := owners(t, dir)
	for path, was := range before {
		if now := after[path]; now != was {
			t.Errorf("OUTSIDE the workspace, %s was re-owned: %d:%d -> %d:%d", path, was[0], was[1], now[0], now[1])
		}
	}
}

// swapForSymlink moves the directory at path aside (to path+".moved",
// still inside the tree) and plants, under its name, a symlink to target.
func swapForSymlink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatalf("Rename(%s): %v", path, err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Symlink(%s -> %s): %v", path, target, err)
	}
}

// redirectTree is the workspace the redirect cases walk, and the outside
// directory mirrors it: whatever name a path-based walk resolves through a
// planted symlink exists outside too, so a redirect re-owns something.
var (
	redirectTreeDirs  = []string{"repo/d/sub"}
	redirectTreeFiles = []string{"repo/a.txt", "repo/d/victim", "repo/d/sub/deep"}
)

func TestChownTree_SwappedDirectoryNeverRedirectsOutside(t *testing.T) {
	tests := []struct {
		name  string
		point walkPoint
		rel   string // the entry at whose hook point the swap happens
		// swap is the directory replaced by a symlink, relative to the
		// root; linkTo is where the symlink points, relative to outside.
		swap, linkTo string
		// reowned must carry the new owner afterwards -- the entries the
		// walk had already reached, under wherever the swap moved them.
		reowned []string
	}{
		{
			// Listed as a directory, a symlink by the time the walk
			// descends: the path-based walk's os.ReadDir followed it and
			// listed, then re-owned, the outside directory.
			name: "directory swapped between its listing and the descent into it", point: beforeDescend, rel: "repo/d",
			swap: "repo/d", linkTo: "repo/d",
			reowned: []string{"repo", "repo/a.txt"},
		},
		{
			// The entry itself is still a directory; its PARENT is not.
			// Even an open that refuses a symlink as its last component
			// resolves repo by name and lands outside.
			name: "parent swapped between the listing and the descent into its child", point: beforeDescend, rel: "repo/d",
			swap: "repo", linkTo: "repo",
			reowned: []string{"repo.moved", "repo.moved/d", "repo.moved/d/victim", "repo.moved/d/sub", "repo.moved/d/sub/deep"},
		},
		{
			// Lchown refuses to follow only the last component: with d
			// swapped, Lchown(".../repo/d/victim") re-owned outside's
			// victim.
			name: "parent swapped during an entry's chown", point: beforeChown, rel: "repo/d/victim",
			swap: "repo/d", linkTo: "repo/d",
			reowned: []string{"repo/d.moved", "repo/d.moved/victim", "repo/d.moved/sub", "repo/d.moved/sub/deep"},
		},
		{
			name: "grandparent swapped during an entry's chown", point: beforeChown, rel: "repo/d/victim",
			swap: "repo", linkTo: "repo",
			reowned: []string{"repo.moved/d", "repo.moved/d/victim", "repo.moved/d/sub", "repo.moved/d/sub/deep"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			buildTree(t, root, redirectTreeDirs, redirectTreeFiles)
			buildTree(t, outside, redirectTreeDirs, redirectTreeFiles)
			uid, gid := observableOwner(t, root)
			outsideBefore := owners(t, outside)
			target := filepath.Join(root, tc.rel)

			acted := false
			hook := func(point walkPoint, path string) {
				if point == tc.point && path == target && !acted {
					acted = true
					swapForSymlink(t, filepath.Join(root, tc.swap), filepath.Join(outside, tc.linkTo))
				}
			}
			if err := chownTree(root, uid, gid, maxChownDepth, hook); err != nil {
				t.Fatalf("chownTree() error = %v, want nil", err)
			}
			if !acted {
				t.Fatalf("the walk never reached %s at the hooked point, so this case proved nothing", tc.rel)
			}

			requireUntouched(t, outside, outsideBefore)
			for _, rel := range tc.reowned {
				if u, g := ownerOf(t, filepath.Join(root, rel)); u != uid || g != gid {
					t.Errorf("%s = %d:%d, want %d:%d -- the walk must carry on with the directory it opened, wherever it now is", rel, u, g, uid, gid)
				}
			}
		})
	}
}

// TestChownTree_RepoSymlinksAreReownedNeverFollowed pins the symlinks a
// repository can carry from the start, with no race at all: to a file
// outside, to a directory outside, and dangling. Each link's own inode is
// re-owned; nothing it points at is, and a linked directory is never
// descended into.
func TestChownTree_RepoSymlinksAreReownedNeverFollowed(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	buildTree(t, root, redirectTreeDirs, redirectTreeFiles)
	buildTree(t, outside, redirectTreeDirs, redirectTreeFiles)
	links := map[string]string{
		"repo/to-file":   filepath.Join(outside, "repo", "a.txt"),
		"repo/to-dir":    filepath.Join(outside, "repo", "d"),
		"repo/d/dangles": filepath.Join(outside, "never-created"),
	}
	for link, target := range links {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
	}
	uid, gid := observableOwner(t, root)
	outsideBefore := owners(t, outside)

	if err := chownTree(root, uid, gid, maxChownDepth, nil); err != nil {
		t.Fatalf("chownTree() error = %v, want nil", err)
	}

	requireUntouched(t, outside, outsideBefore)
	for link := range links {
		if u, g := ownerOf(t, filepath.Join(root, link)); u != uid || g != gid {
			t.Errorf("symlink %s = %d:%d, want %d:%d -- the link's own inode must be re-owned", link, u, g, uid, gid)
		}
	}
}

// TestChownTree_SymlinkedRootIsRefused pins that workspaceDir itself is
// opened without following a final symlink: a repo directory replaced by a
// link (possible for a per-repo call once the workspace directory belongs
// to the runtime) fails the walk instead of re-owning the link's target.
func TestChownTree_SymlinkedRootIsRefused(t *testing.T) {
	parent, outside := t.TempDir(), t.TempDir()
	buildTree(t, outside, redirectTreeDirs, redirectTreeFiles)
	root := filepath.Join(parent, "repo")
	if err := os.Symlink(filepath.Join(outside, "repo"), root); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	uid, gid := observableOwner(t, parent)
	outsideBefore := owners(t, outside)

	err := chownTree(root, uid, gid, maxChownDepth, nil)
	if !errors.Is(err, syscall.ELOOP) && !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("chownTree(symlinked root) error = %v, want one wrapping ELOOP or ENOTDIR", err)
	}
	requireUntouched(t, outside, outsideBefore)
}

// TestChownTree_DepthBound pins maxChownDepth's contract at its edge: a
// tree exactly as deep as the bound is walked whole, one level deeper
// fails the walk instead of being re-owned in part.
func TestChownTree_DepthBound(t *testing.T) {
	const bound = 3
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	for _, tc := range []struct {
		levels  int
		wantErr bool
	}{{bound, false}, {bound + 1, true}} {
		t.Run(fmt.Sprintf("%d levels", tc.levels), func(t *testing.T) {
			root := t.TempDir()
			deepest := root
			for range tc.levels {
				deepest = filepath.Join(deepest, "d")
			}
			if err := os.MkdirAll(deepest, 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			err := chownTree(root, uid, gid, bound, nil)
			if gotErr := errors.Is(err, errTreeTooDeep); gotErr != tc.wantErr {
				t.Fatalf("chownTree(%d levels, bound %d) error = %v, want errTreeTooDeep: %t", tc.levels, bound, err, tc.wantErr)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("chownTree() error = %v, want nil", err)
			}
		})
	}
}

// TestChownTree_ConcurrentAtomicSwapNeverRedirects is the review's own
// reproduction, kept as a test: a real concurrent writer atomically
// exchanges a directory full of files with a symlink to an outside
// directory holding the same names, over and over, while the real walk
// runs again and again. The path-based walk re-owned the outside victim in
// about half of 300 walks this way. The swapper pauses briefly between
// exchanges: Linux may list a directory whose entries are being renamed
// without them (POSIX leaves it unspecified), and an unpaced swapper then
// keeps the walk from ever reaching the directory at all -- which this
// test counts, and refuses to pass on. The pause is a busy wait: a
// time.Sleep that short lasts a timer tick or more in a container, which
// leaves too few swaps for this test to catch a path-based descent there.
func TestChownTree_ConcurrentAtomicSwapNeverRedirects(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	const files = 200
	names := make([]string, 0, files+1)
	for i := range files {
		names = append(names, fmt.Sprintf("f%04d", i))
	}
	names = append(names, "zz-victim")

	root, outside := t.TempDir(), t.TempDir()
	dir, link := filepath.Join(root, "repo", "d"), filepath.Join(root, "repo", "d-link")
	var dirFiles, outsideFiles []string
	for _, n := range names {
		dirFiles = append(dirFiles, "repo/d/"+n)
		outsideFiles = append(outsideFiles, "target/"+n)
	}
	buildTree(t, root, []string{"repo/d"}, dirFiles)
	buildTree(t, outside, []string{"target"}, outsideFiles)
	if err := os.Symlink(filepath.Join(outside, "target"), link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := atomicExchange(dir, link); err != nil {
		t.Skipf("no atomic exchange here: %v", err)
	}
	uid, gid := observableOwner(t, root)
	outsideBefore := owners(t, outside)
	victim := filepath.Join(outside, "target", "zz-victim")
	victimBefore := outsideBefore[victim]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var swapper errgroup.Group
	swaps := 0
	swapper.Go(func() error {
		for ctx.Err() == nil {
			if err := atomicExchange(dir, link); err != nil {
				return err
			}
			swaps++
			spinFor(20 * time.Microsecond)
		}
		return nil
	})

	// reached counts the walks that opened the real directory, under
	// either name, and began listing it: the walks in which a swap could
	// have redirected them.
	reached := 0
	hook := func(point walkPoint, path string) {
		if point == beforeList && (path == dir || path == link) {
			reached++
		}
	}
	const walks = 300
	var failures []error
	redirected := 0
	for range walks {
		if err := chownTree(root, uid, gid, maxChownDepth, hook); err != nil {
			failures = append(failures, err)
		}
		if u, g := ownerOf(t, victim); [2]uint32{u, g} != victimBefore {
			redirected++
			break
		}
	}
	cancel()
	if err := swapper.Wait(); err != nil {
		t.Fatalf("swapper: %v", err)
	}
	if swaps == 0 || reached == 0 {
		t.Fatalf("swaps = %d, walks that reached the swapped directory = %d: this proved nothing", swaps, reached)
	}
	t.Logf("%d walks, %d swaps, %d walks reached the swapped directory", walks, swaps, reached)
	if redirected > 0 {
		t.Errorf("a walk re-owned the OUTSIDE victim %s (%d swaps so far)", victim, swaps)
	}
	requireUntouched(t, outside, outsideBefore)
	if len(failures) > 0 {
		t.Errorf("%d of %d walks failed beside an atomic swap (%d swaps); first: %v -- an exchange never removes a name, so nothing here should fail", len(failures), walks, swaps, failures[0])
	}
}

// spinFor busy-waits for d, yielding to other goroutines as it does. See
// TestChownTree_ConcurrentAtomicSwapNeverRedirects for why not time.Sleep.
func spinFor(d time.Duration) {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); {
		runtime.Gosched()
	}
}
