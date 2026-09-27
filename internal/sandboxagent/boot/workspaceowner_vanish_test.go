package boot

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sync/errgroup"
)

// vanishTreeDirs and vanishTreeFiles are the tree the vanish cases walk,
// relative to its root.
var (
	vanishTreeDirs  = []string{"repo/.git", "repo/node_modules/.cache/nested"}
	vanishTreeFiles = []string{"repo/main.go", "repo/.git/HEAD", "repo/.git/HEAD.lock", "repo/.git/index.lock", "repo/.git/ORIG_HEAD", "repo/node_modules/.cache/nested/blob"}
)

// TestChownTree_EntriesThatVanishMidWalk drives chownTree through a hook
// that makes, at an exact point, the change a concurrent writer makes to
// the tree while the real walk runs: a file or directory the walk has
// already listed disappears, or disappears and comes back under the same
// name, before the walk looks at it, opens it or lists it. That is how the
// push integration test's own `git commit` failed boot in CI -- "lchown
// .../.git/HEAD.lock: no such file or directory" -- and how a services.yml
// process resetting a cache directory (`rm -rf d && mkdir d`) could still
// fail cmd/sandbox-agent's post-boot pass after the first fix, because a
// directory recreated under its old name was not the one that had gone.
// Every system call in the walk is the real one; only the timing is
// arranged. The walk re-owns to an owner the entries do not already have,
// so what it re-owned is read off the tree afterwards.
func TestChownTree_EntriesThatVanishMidWalk(t *testing.T) {
	const cache = "repo/node_modules/.cache"

	// writer is the owner entries had before the walk, which is also what
	// anything the hook creates is given, as a concurrent writer's own
	// entries would be. Explicitly: on darwin a new entry takes its
	// parent directory's group, which the walk may already have changed,
	// and would then look re-owned without the walk having touched it.
	var writer [2]uint32
	written := func(t *testing.T, paths ...string) {
		t.Helper()
		for _, p := range paths {
			if err := os.Lchown(p, int(writer[0]), int(writer[1])); err != nil {
				t.Fatalf("Lchown(%s): %v", p, err)
			}
		}
	}
	remove := func(t *testing.T, path string) {
		t.Helper()
		if err := os.RemoveAll(path); err != nil {
			t.Fatalf("remove %s: %v", path, err)
		}
	}
	// recreate removes the directory at path and puts a new one, with new
	// content, under the same name -- a cache or build-output reset.
	recreate := func(t *testing.T, path string) {
		t.Helper()
		remove(t, path)
		if err := os.MkdirAll(filepath.Join(path, "fresh"), 0o755); err != nil {
			t.Fatalf("recreate %s: %v", path, err)
		}
		if err := os.WriteFile(filepath.Join(path, "fresh", "file"), []byte("y\n"), 0o644); err != nil {
			t.Fatalf("write under recreated %s: %v", path, err)
		}
		written(t, path, filepath.Join(path, "fresh"), filepath.Join(path, "fresh", "file"))
	}

	tests := []struct {
		name  string
		point walkPoint
		// rel is the entry the hook acts on, relative to the root. A rel
		// ending in "/*" is whichever entry of that directory the walk
		// reaches first at point, so that entries after it remain to prove
		// the walk carried on, whatever order the filesystem lists them in.
		rel string
		act func(t *testing.T, path string)
		// stale are entries left in the tree that the walk must NOT have
		// re-owned, because they were created after it had passed them.
		// Every other entry left in the tree must have been re-owned: the
		// walk carries on past a vanished entry, and re-owns whatever a
		// name holds when it gets to it.
		stale []string
	}{
		{
			name: "file removed after it was listed", point: beforeEntry, rel: "repo/.git/*",
			act: remove,
		},
		{
			name: "file removed before its own chown", point: beforeChown, rel: "repo/.git/*",
			act: remove,
		},
		{
			name: "file recreated under the same name before its own chown", point: beforeChown, rel: "repo/.git/*",
			act: func(t *testing.T, path string) {
				remove(t, path)
				if err := os.WriteFile(path, []byte("new\n"), 0o644); err != nil {
					t.Fatalf("recreate %s: %v", path, err)
				}
				written(t, path)
			},
		},
		{
			name: "directory removed after it was listed", point: beforeEntry, rel: cache,
			act: remove,
		},
		{
			name: "directory removed before it is opened", point: beforeDescend, rel: cache,
			act: remove,
		},
		{
			name: "directory removed after it was opened, before it is listed", point: beforeList, rel: cache,
			act: remove,
		},
		{
			// The case that still failed boot: the name exists again, so a
			// check of the path finds it, yet the directory the walk was
			// about to read is gone. The walk opens whatever the name holds
			// when it descends, and re-owns that.
			name: "directory recreated under the same name after it was listed", point: beforeEntry, rel: cache,
			act: recreate,
		},
		{
			name: "directory recreated under the same name before it is opened", point: beforeDescend, rel: cache,
			act: recreate,
		},
		{
			// The walk holds the old directory open; listing it finds it
			// removed (ENOENT on Linux, empty on darwin). The new one was
			// created after the walk listed its parent, so this pass does
			// not reach it.
			name: "directory recreated under the same name after it was opened, before it is listed", point: beforeList, rel: cache,
			act: recreate, stale: []string{cache, cache + "/fresh", cache + "/fresh/file"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			buildTree(t, root, vanishTreeDirs, vanishTreeFiles)
			uid, gid := observableOwner(t, root)
			writer[0], writer[1] = ownerOf(t, filepath.Join(root, "repo", "main.go"))
			target := filepath.Join(root, tc.rel)
			matches := func(path string) bool { return path == target }
			if dir, ok := strings.CutSuffix(tc.rel, "/*"); ok {
				matches = func(path string) bool { return filepath.Dir(path) == filepath.Join(root, dir) }
			}

			acted := false
			hook := func(point walkPoint, path string) {
				if point == tc.point && matches(path) && !acted {
					acted = true
					tc.act(t, path)
				}
			}
			if err := chownTree(root, uid, gid, maxChownDepth, hook); err != nil {
				t.Fatalf("chownTree() error = %v, want nil -- an entry that vanished or was replaced mid-walk must not fail boot", err)
			}
			if !acted {
				t.Fatalf("the walk never reached %s at the hooked point, so this case proved nothing", tc.rel)
			}

			for path, owner := range owners(t, root) {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					t.Fatalf("Rel(%s): %v", path, err)
				}
				wantReowned := !slices.Contains(tc.stale, rel)
				if reowned := owner == [2]uint32{uid, gid}; reowned != wantReowned {
					t.Errorf("%s: re-owned = %t, want %t", rel, reowned, wantReowned)
				}
			}
		})
	}
}

// TestChownTree_ConcurrentSameNameResetNeverFailsBoot runs the real walk,
// repeatedly, while a real concurrent writer resets a cache directory under
// the same name as fast as it can -- the race that the path-based walk
// turned into a failed boot a few percent of the time on Linux.
func TestChownTree_ConcurrentSameNameResetNeverFailsBoot(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	root := t.TempDir()
	buildTree(t, root, vanishTreeDirs, vanishTreeFiles)
	cache := filepath.Join(root, "repo", "node_modules", ".cache")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var writer errgroup.Group
	resets := 0
	writer.Go(func() error {
		for ctx.Err() == nil {
			if err := os.RemoveAll(cache); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(cache, "sub"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(cache, "sub", "f"), []byte("z\n"), 0o644); err != nil {
				return err
			}
			resets++
		}
		return nil
	})

	const walks = 2000
	var failures []error
	for range walks {
		if err := chownTree(root, uid, gid, maxChownDepth, nil); err != nil {
			failures = append(failures, err)
		}
	}
	cancel()
	if err := writer.Wait(); err != nil {
		t.Fatalf("writer: %v", err)
	}
	if resets == 0 {
		t.Fatal("the writer never reset the directory, so this proved nothing")
	}
	if len(failures) > 0 {
		t.Fatalf("%d of %d walks failed beside a same-name directory reset (%d resets); first: %v", len(failures), walks, resets, failures[0])
	}
}

// TestChownTree_MissingRootIsStillAnError pins that tolerating vanished
// entries never extends to workspaceDir itself.
func TestChownTree_MissingRootIsStillAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err := chownTree(missing, uint32(os.Getuid()), uint32(os.Getgid()), maxChownDepth, nil)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("chownTree(missing root) error = %v, want one wrapping fs.ErrNotExist", err)
	}
}

// TestChownTree_OtherErrorsStillAbortTheWalk pins that only a vanished
// entry is tolerated: any other failure to re-own, open or list is
// returned, and fails boot. Both cases need a caller WITHOUT privilege --
// root is refused neither the chown nor the open -- so both skip as root.
func TestChownTree_OtherErrorsStillAbortTheWalk(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs an unprivileged caller: root is refused neither a chown nor an open")
	}
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	t.Run("a directory that cannot be opened", func(t *testing.T) {
		root := t.TempDir()
		buildTree(t, root, vanishTreeDirs, vanishTreeFiles)
		locked := filepath.Join(root, "repo", "node_modules")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

		err := chownTree(root, uid, gid, maxChownDepth, nil)
		if !errors.Is(err, syscall.EACCES) {
			t.Fatalf("chownTree() error = %v, want one wrapping EACCES", err)
		}
	})

	t.Run("a chown the caller is not allowed", func(t *testing.T) {
		root := t.TempDir()
		buildTree(t, root, vanishTreeDirs, vanishTreeFiles)
		err := chownTree(root, uid, groupNotHeld(t), maxChownDepth, nil)
		if !errors.Is(err, syscall.EPERM) {
			t.Fatalf("chownTree() error = %v, want one wrapping EPERM", err)
		}
	})
}

// groupNotHeld returns a gid the calling process is not a member of, and so
// may not chown a file to.
func groupNotHeld(t *testing.T) uint32 {
	t.Helper()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatalf("Getgroups: %v", err)
	}
	for g := 4000; g < 65000; g++ {
		if g != os.Getegid() && !slices.Contains(groups, g) {
			return uint32(g)
		}
	}
	t.Fatalf("no gid free of this process's %d groups", len(groups))
	return 0
}

// buildTree creates dirs and then files (each holding one line) under root.
func buildTree(t *testing.T, root string, dirs, files []string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(root, file), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
}
