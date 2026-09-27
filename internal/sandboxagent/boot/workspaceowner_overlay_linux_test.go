package boot

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

// The tests in this file remove entries from the image's own lower layer,
// so they run only in a throwaway container, as root, when pointed at
// trees there by these variables.
const (
	// overlayEmptyDirEnv names a directory, empty in the image's lower
	// layer, for the walk over its parent to have removed just before the
	// walk re-owns it.
	overlayEmptyDirEnv = "NARVI_TEST_OVERLAY_EMPTY_LOWER_DIR"
	// overlayTreesEnv lists, filepath.SplitList-style, lower-layer trees
	// to walk while a concurrent remover takes every other subdirectory of
	// each. None may hold what exec needs (the dynamic loader, libc): the
	// walk's first run of the hard-link probe execs.
	overlayTreesEnv = "NARVI_TEST_OVERLAY_LOWER_TREES"
)

// TestChownTree_OverlayLowerLayerRemovals pins the two ways overlayfs
// reports an entry removed mid-walk other than ENOENT, when the entry is
// still only in a lower layer and re-owning it must copy it up first. A
// directory removed after the walk opened it fails its fchown with ENOTDIR,
// deterministically where it was empty in that layer. An entry removed
// while fchownat runs -- its name looked up before the removal, the
// copy-up after -- fails with EEXIST, which no hook point can time, so a
// concurrent remover makes it happen. Either is an entry that has gone,
// not a failure, and every entry that survives must be re-owned.
func TestChownTree_OverlayLowerLayerRemovals(t *testing.T) {
	requireLinuxRoot(t)

	t.Run("an empty lower directory removed before its re-own", func(t *testing.T) {
		dir := os.Getenv(overlayEmptyDirEnv)
		if dir == "" {
			t.Skipf("set %s to an empty directory in an overlay lower layer, in a throwaway container, to run this", overlayEmptyDirEnv)
		}
		requireOverlay(t, dir)
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("%s: %d entries, error %v, want an empty directory", dir, len(entries), err)
		}
		parent := filepath.Dir(dir)
		var fired bool
		var removeErr error
		hook := func(point walkPoint, path string) {
			if point == beforeReown && path == dir && !fired {
				fired, removeErr = true, unix.Rmdir(dir)
			}
		}
		if err := chownTree(parent, 65534, 65534, quietly(hook)); err != nil {
			t.Errorf("chownTree(%s) = %v, want nil: the directory was removed after the walk opened it", parent, err)
		}
		if !fired || removeErr != nil {
			t.Fatalf("removing %s before its re-own: hook fired = %t, error = %v", dir, fired, removeErr)
		}
		requireAllOwnedBy(t, parent, 65534, 65534)
	})

	t.Run("entries removed while the walk re-owns them", func(t *testing.T) {
		trees := filepath.SplitList(os.Getenv(overlayTreesEnv))
		if len(trees) == 0 {
			t.Skipf("set %s to overlay lower-layer trees, in a throwaway container, to run this", overlayTreesEnv)
		}
		for _, tree := range trees {
			requireOverlay(t, tree)
			entries, err := os.ReadDir(tree)
			if err != nil {
				t.Fatalf("ReadDir(%s): %v", tree, err)
			}
			var doomed []string
			for i, e := range entries {
				if e.IsDir() && i%2 == 1 {
					doomed = append(doomed, filepath.Join(tree, e.Name()))
				}
			}
			start := make(chan struct{})
			var remover errgroup.Group
			remover.Go(func() error {
				<-start
				for _, d := range doomed {
					if err := os.RemoveAll(d); err != nil {
						return err
					}
				}
				return nil
			})
			close(start)
			walkErr := chownTree(tree, 65534, 65534, quietly(nil))
			if err := remover.Wait(); err != nil {
				t.Fatalf("the concurrent remover: %v", err)
			}
			if walkErr != nil {
				t.Errorf("chownTree(%s) beside a remover = %v, want nil: every entry the remover took is gone, not a failure", tree, walkErr)
			}
			requireAllOwnedBy(t, tree, 65534, 65534)
		}
	})
}

// requireOverlay fails unless path is on overlayfs.
func requireOverlay(t *testing.T, path string) {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		t.Fatalf("Statfs(%s): %v", path, err)
	}
	if st.Type != unix.OVERLAYFS_SUPER_MAGIC {
		t.Fatalf("%s is on filesystem type %#x, want overlayfs", path, st.Type)
	}
}
