package boot

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

// The tests in this file re-own, and remove, entries of the image's own
// lower layer, so they run only as root, inside a throwaway container,
// when pointed at trees there by these variables. requireDisposableTargets
// decides whether they may, before they touch anything.
const (
	// overlayEmptyDirEnv names a directory, empty in the image's lower
	// layer, for the walk over its parent to have removed just before the
	// walk re-owns it. The parent is walked, so both must pass
	// requireDisposableTargets: /etc/apt/auth.conf.d, say, rather than a
	// top-level /srv, whose parent is /.
	overlayEmptyDirEnv = "NARVI_TEST_OVERLAY_EMPTY_LOWER_DIR"
	// overlayTreesEnv lists, filepath.SplitList-style, lower-layer trees
	// to walk while a concurrent remover takes every other subdirectory of
	// each. Together they must hold minDoomedSymlinks symlinks in what the
	// remover takes, so include one such as /usr/share/zoneinfo. None may
	// hold what exec needs (the dynamic loader, libc): the walk's first run
	// of the hard-link probe execs.
	overlayTreesEnv = "NARVI_TEST_OVERLAY_LOWER_TREES"
)

// minDoomedSymlinks is how many symlinks the subdirectories the remover
// takes must hold, across the trees overlayTreesEnv names. A symlink's
// removal is undone only when it lands inside the walk's fchownat, so the
// race needs many of them to show at all. Of debian:bookworm-slim's
// /usr/share/doc the remover takes no symlink, so it cannot show it there;
// of its /usr/share/zoneinfo the remover takes 283, and some were back
// after 20 of 22 runs measured, up to 16 in one. A run where none comes
// back still passes: the race decides that, not the walk.
const minDoomedSymlinks = 100

// TestChownTree_OverlayLowerLayerRemovals pins what
// ChownWorkspaceForRuntime's doc comment says of the three outcomes kernel
// overlayfs gives a removal that overlaps re-owning a lower-layer entry.
// A directory removed after the walk opened it, empty in that layer, fails
// its fchown with ENOTDIR, which the beforeReown seam makes happen
// deterministically. A regular file removed while fchownat runs fails it
// with EEXIST, and a symlink removed then is copied back up and stays. No
// hook point sits inside a system call, so a concurrent remover makes
// those two happen, by chance. Throughout, the walk must return nil, and
// every entry left afterwards must be the runtime's, including each one
// brought back. The remover's ENOTEMPTY, for a directory holding an entry
// brought back, is the documented outcome: it is recorded, not failed.
func TestChownTree_OverlayLowerLayerRemovals(t *testing.T) {
	requireLinuxRoot(t)

	t.Run("an empty lower directory removed before its re-own", func(t *testing.T) {
		dir := os.Getenv(overlayEmptyDirEnv)
		if dir == "" {
			t.Skipf("set %s to an empty directory in an overlay lower layer, in a throwaway container, to run this", overlayEmptyDirEnv)
		}
		parent := filepath.Dir(dir)
		requireDisposableTargets(t, overlayEmptyDirEnv, dir, parent)
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("%s: %d entries, error %v, want an empty directory", dir, len(entries), err)
		}
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
		requireDisposableTargets(t, overlayTreesEnv, trees...)
		doomed := make([][]string, len(trees))
		symlinks := 0
		for i, tree := range trees {
			doomed[i] = everyOtherSubdir(t, tree)
			for _, d := range doomed[i] {
				symlinks += countSymlinks(t, d)
			}
		}
		if symlinks < minDoomedSymlinks {
			t.Fatalf("what the remover would take of %q holds %d symlinks, want at least %d: include a symlink-heavy lower-layer tree such as /usr/share/zoneinfo in %s, or a removal the walk undoes is never raced",
				trees, symlinks, minDoomedSymlinks, overlayTreesEnv)
		}
		for i, tree := range trees {
			raceRemoverAgainstWalk(t, tree, doomed[i])
		}
	})
}

// raceRemoverAgainstWalk removes every directory in doomed, all under
// tree, while the walk re-owns tree, and holds the outcome to what
// ChownWorkspaceForRuntime's doc comment says of it.
func raceRemoverAgainstWalk(t *testing.T, tree string, doomed []string) {
	t.Helper()
	// Written by the remover alone, and read only once Wait has returned.
	var removeErrs []error
	start := make(chan struct{})
	var remover errgroup.Group
	remover.Go(func() error {
		<-start
		for _, d := range doomed {
			if err := os.RemoveAll(d); err != nil {
				removeErrs = append(removeErrs, err)
			}
		}
		return nil
	})
	close(start)
	walkErr := chownTree(tree, 65534, 65534, quietly(nil))
	_ = remover.Wait() // its function records each removal's error in removeErrs, and returns nil

	if walkErr != nil {
		t.Errorf("chownTree(%s) beside a remover = %v, want nil: every entry the remover took is gone or back, not a failure", tree, walkErr)
	}
	notEmpty := 0
	for _, err := range removeErrs {
		if !errors.Is(err, unix.ENOTEMPTY) {
			t.Errorf("the concurrent remover: %v", err)
			continue
		}
		notEmpty++
		t.Logf("the concurrent remover: %v: it holds an entry the walk brought back", err)
	}
	back := leftUnder(t, doomed)
	var sample []string
	for _, e := range back {
		if e.Type().IsRegular() {
			t.Errorf("%s: a regular file is back after its removal; its copy-up links it into place, and the link fails with EEXIST where the upper layer's filesystem supports O_TMPFILE", e.path)
		}
		if len(sample) < leftSampleSize {
			sample = append(sample, e.path)
		}
	}
	if notEmpty > 0 && len(back) == 0 {
		t.Errorf("the remover failed %d times with ENOTEMPTY, yet nothing it took is left", notEmpty)
	}
	t.Logf("%s: %d entries the remover took are back (%q); removing %d of its %d directories failed with ENOTEMPTY",
		tree, len(back), sample, notEmpty, len(doomed))
	// Every entry left, those brought back included, is the runtime's.
	requireAllOwnedBy(t, tree, 65534, 65534)
}

// requireDisposableTargets fails the test unless every path is one the
// tests in this file may walk, looking but touching nothing: the strings
// pass checkDisposableTargets; each path is what it names, with no
// component a symlink; the test runs inside a container, by the marker
// containerMarker looks for; each path is on overlayfs; and no mount point
// is at or under one, since the walk crosses mount points and must stay on
// the container's own overlay.
func requireDisposableTargets(t *testing.T, env string, paths ...string) {
	t.Helper()
	if err := checkDisposableTargets(paths...); err != nil {
		t.Fatalf("%s: %v", env, err)
	}
	marker := containerMarker(regularFileExists)
	if marker == "" {
		t.Fatalf("%s is set, but none of %q exists: these tests re-own and remove image content, and run only inside a throwaway container", env, containerMarkers)
	}
	t.Logf("inside a container: %s exists", marker)
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("reading the mount table: %v", err)
	}
	for _, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("%s: resolving %s: %v", env, path, err)
		}
		if resolved != path {
			t.Fatalf("%s: %s resolves to %s: a component of it is a symlink", env, path, resolved)
		}
		requireOverlay(t, path)
		mounts, err := mountPointsAtOrUnder(mountinfo, path)
		if err != nil {
			t.Fatalf("reading the mount table: %v", err)
		}
		if len(mounts) > 0 {
			t.Fatalf("%s: mount points at or under %s, which the walk would cross into: %q", env, path, mounts)
		}
	}
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

// everyOtherSubdir returns the subdirectories of tree at odd positions in
// its sorted listing: what the remover takes.
func everyOtherSubdir(t *testing.T, tree string) []string {
	t.Helper()
	entries, err := os.ReadDir(tree)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", tree, err)
	}
	var subdirs []string
	for i, e := range entries {
		if e.IsDir() && i%2 == 1 {
			subdirs = append(subdirs, filepath.Join(tree, e.Name()))
		}
	}
	return subdirs
}

// countSymlinks returns how many symlinks there are under dir.
func countSymlinks(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink != 0 {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
	return n
}

// leftEntry is an entry that is still there after its removal.
type leftEntry struct {
	fs.DirEntry
	path string
}

// leftUnder returns every entry other than a directory still under the
// directories in removed, which were all removed: each is one whose
// removal was undone.
func leftUnder(t *testing.T, removed []string) []leftEntry {
	t.Helper()
	var left []leftEntry
	for _, dir := range removed {
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				left = append(left, leftEntry{DirEntry: d, path: path})
			}
			return err
		})
		if err != nil {
			t.Fatalf("WalkDir(%s): %v", dir, err)
		}
	}
	return left
}
