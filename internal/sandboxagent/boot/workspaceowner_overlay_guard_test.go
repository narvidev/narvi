package boot

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The overlay tests (workspaceowner_overlay_linux_test.go) re-own, and
// remove, what an environment variable names, and the walk they run
// crosses mount points. What follows decides, before they touch anything,
// whether they may: requireDisposableTargets applies it.

// kernelTrees are where an entry belongs to the kernel or to the container
// engine rather than to the image: re-owning a /proc entry, for one,
// changes the kernel's own record of it, which every mount of proc shares
// and which outlives the container.
var kernelTrees = []string{"/proc", "/sys", "/dev", "/run"}

// checkDisposableTargets returns nil if every path may be walked by a
// destructive overlay test, and otherwise why the first that may not is
// refused. It looks at the strings alone: each must be absolute and
// clean, with no .. component; it must be neither / nor an entry of /,
// which would put a whole top-level tree, or / itself when a test walks
// the parent, in reach; and it must not be in one of kernelTrees.
func checkDisposableTargets(paths ...string) error {
	if len(paths) == 0 {
		return errors.New("no path given")
	}
	for _, path := range paths {
		if err := checkDisposableTarget(path); err != nil {
			return fmt.Errorf("%q: %w", path, err)
		}
	}
	return nil
}

func checkDisposableTarget(path string) error {
	switch {
	case path == "":
		return errors.New("empty")
	case !filepath.IsAbs(path):
		return errors.New("not absolute")
	case slices.Contains(strings.Split(path, "/"), ".."):
		return errors.New("has a .. component")
	case filepath.Clean(path) != path:
		return fmt.Errorf("not clean: it cleans to %q", filepath.Clean(path))
	case path == "/":
		return errors.New("is the root directory")
	case filepath.Dir(path) == "/":
		return errors.New("is an entry of /")
	}
	for _, tree := range kernelTrees {
		if path == tree || strings.HasPrefix(path, tree+"/") {
			return fmt.Errorf("is under %s", tree)
		}
	}
	return nil
}

// containerMarkers are the files a container engine creates at the root of
// every container it starts: Docker's /.dockerenv (the engine the
// documented runs use) and Podman's /run/.containerenv. A host has
// neither unless someone made one by hand, and an engine that makes
// neither only makes the overlay tests refuse to run.
var containerMarkers = []string{"/.dockerenv", "/run/.containerenv"}

// containerMarker returns the first of containerMarkers that exists as a
// regular file, per isRegular, or "" if none does.
func containerMarker(isRegular func(string) bool) string {
	for _, marker := range containerMarkers {
		if isRegular(marker) {
			return marker
		}
	}
	return ""
}

// regularFileExists reports whether path names a regular file, without
// following a symlink there.
func regularFileExists(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

// mountPointsAtOrUnder returns the mount points that mountinfo, in the
// format of /proc/self/mountinfo, lists at root or under it.
func mountPointsAtOrUnder(mountinfo []byte, root string) ([]string, error) {
	var found []string
	lines := bufio.NewScanner(bytes.NewReader(mountinfo))
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) < 5 {
			return nil, fmt.Errorf("mountinfo line %q has %d fields, want at least 5", lines.Text(), len(fields))
		}
		mountPoint, err := unescapeMountinfo(fields[4])
		if err != nil {
			return nil, err
		}
		if mountPoint == root || strings.HasPrefix(mountPoint, root+"/") {
			found = append(found, mountPoint)
		}
	}
	return found, lines.Err()
}

// unescapeMountinfo undoes the kernel's escaping of a mountinfo path, in
// which a space, tab, newline or backslash is a backslash and three octal
// digits.
func unescapeMountinfo(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+4 > len(s) {
			return "", fmt.Errorf("mountinfo path %q: a backslash without three octal digits", s)
		}
		c, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			return "", fmt.Errorf("mountinfo path %q: %w", s, err)
		}
		b.WriteByte(byte(c))
		i += 3
	}
	return b.String(), nil
}

func TestCheckDisposableTargets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []string
		// refused names the reason expected, or is empty for accepted.
		refused string
	}{
		{name: "a symlink-heavy tree", paths: []string{"/usr/share/zoneinfo"}},
		{name: "several trees", paths: []string{"/usr/share/doc", "/usr/local/go/src"}},
		{name: "an empty directory and its parent", paths: []string{"/etc/apt/auth.conf.d", "/etc/apt"}},
		{name: "a second-level directory", paths: []string{"/usr/share"}},
		{name: "a name that only starts like a kernel tree", paths: []string{"/procedures/x", "/runner/x", "/devices/x", "/system/x"}},
		{name: "a kernel tree's name further down", paths: []string{"/usr/proc", "/usr/local/sys"}},

		{name: "nothing", paths: nil, refused: "no path given"},
		{name: "an empty path", paths: []string{""}, refused: "empty"},
		{name: "an empty element of a list", paths: []string{"/usr/share/zoneinfo", ""}, refused: "empty"},
		{name: "a relative path", paths: []string{"usr/share/zoneinfo"}, refused: "not absolute"},
		{name: "a dot-relative path", paths: []string{"./zoneinfo"}, refused: "not absolute"},
		{name: "a .. component", paths: []string{"/usr/share/../../proc/sys"}, refused: "has a .. component"},
		{name: "a trailing ..", paths: []string{"/usr/share/zoneinfo/.."}, refused: "has a .. component"},
		{name: "a trailing slash", paths: []string{"/usr/share/zoneinfo/"}, refused: "not clean"},
		{name: "a doubled slash", paths: []string{"//usr/share/zoneinfo"}, refused: "not clean"},
		{name: "a . component", paths: []string{"/usr/./share"}, refused: "not clean"},
		{name: "the root", paths: []string{"/"}, refused: "is the root directory"},
		{name: "a top-level directory", paths: []string{"/srv"}, refused: "is an entry of /"},
		{name: "an empty top-level directory's parent", paths: []string{"/srv", "/"}, refused: "is an entry of /"},
		{name: "a second-level empty directory's parent", paths: []string{"/var/opt", "/var"}, refused: "is an entry of /"},
		{name: "a top-level tree after a valid one", paths: []string{"/usr/share/zoneinfo", "/usr"}, refused: "is an entry of /"},
		{name: "proc itself", paths: []string{"/proc"}, refused: "is an entry of /"},
		{name: "under proc", paths: []string{"/proc/sys/fs"}, refused: "is under /proc"},
		{name: "under sys", paths: []string{"/sys/fs/cgroup"}, refused: "is under /sys"},
		{name: "under dev", paths: []string{"/dev/shm"}, refused: "is under /dev"},
		{name: "under run", paths: []string{"/run/lock"}, refused: "is under /run"},
		{name: "a kernel tree after a valid one", paths: []string{"/usr/share/zoneinfo", "/proc/self"}, refused: "is under /proc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDisposableTargets(tc.paths...)
			switch {
			case tc.refused == "" && err != nil:
				t.Errorf("checkDisposableTargets(%q) = %v, want accepted", tc.paths, err)
			case tc.refused != "" && err == nil:
				t.Errorf("checkDisposableTargets(%q) accepted them, want refused: %s", tc.paths, tc.refused)
			case tc.refused != "" && !strings.Contains(err.Error(), tc.refused):
				t.Errorf("checkDisposableTargets(%q) = %v, want it refused because it %s", tc.paths, err, tc.refused)
			}
		})
	}
}

func TestContainerMarker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present []string
		want    string
	}{
		{name: "a host", present: nil, want: ""},
		{name: "Docker", present: []string{"/.dockerenv"}, want: "/.dockerenv"},
		{name: "Podman", present: []string{"/run/.containerenv"}, want: "/run/.containerenv"},
		{name: "both", present: []string{"/run/.containerenv", "/.dockerenv"}, want: "/.dockerenv"},
		{name: "an unrelated file", present: []string{"/.containerenv", "/run/.dockerenv"}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := containerMarker(func(p string) bool { return slices.Contains(tc.present, p) }); got != tc.want {
				t.Errorf("containerMarker with %q present = %q, want %q", tc.present, got, tc.want)
			}
		})
	}

	// A marker counts only as a regular file: a directory or a symlink
	// by that name is not what an engine makes.
	dir := t.TempDir()
	file, sub, link := filepath.Join(dir, "file"), filepath.Join(dir, "dir"), filepath.Join(dir, "link")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{file: true, sub: false, link: false, filepath.Join(dir, "missing"): false} {
		if got := regularFileExists(path); got != want {
			t.Errorf("regularFileExists(%s) = %t, want %t", path, got, want)
		}
	}
}

func TestMountPointsAtOrUnder(t *testing.T) {
	// The shape of a container's own /proc/self/mountinfo, with a mount
	// point whose name the kernel escaped, and one that only starts like
	// the tree asked about.
	mountinfo := []byte(`741 616 0:109 / / rw,relatime master:273 - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w
742 741 0:112 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
750 741 254:17 /containers/c/hosts /etc/hosts rw,relatime - btrfs /dev/vdb1 rw
751 741 254:17 /containers/c/hostname /etc/hostname rw,relatime - btrfs /dev/vdb1 rw
760 741 0:120 / /usr/share/with\040space rw - tmpfs tmpfs rw
761 741 0:121 / /usr/share/zoneinfo2 rw - tmpfs tmpfs rw
762 741 0:122 / /srv/data rw - tmpfs tmpfs rw
`)
	for _, tc := range []struct {
		root string
		want []string
	}{
		{root: "/usr/share/zoneinfo", want: nil},
		{root: "/etc/apt", want: nil},
		{root: "/etc", want: []string{"/etc/hosts", "/etc/hostname"}},
		{root: "/usr/share", want: []string{"/usr/share/with space", "/usr/share/zoneinfo2"}},
		{root: "/usr/share/with space", want: []string{"/usr/share/with space"}},
		{root: "/srv/data", want: []string{"/srv/data"}},
	} {
		got, err := mountPointsAtOrUnder(mountinfo, tc.root)
		if err != nil {
			t.Fatalf("mountPointsAtOrUnder(%s): %v", tc.root, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("mountPointsAtOrUnder(%s) = %q, want %q", tc.root, got, tc.want)
		}
	}

	for name, bad := range map[string]string{
		"a short line":           "741 616 0:109 /\n",
		"a truncated escape":     "760 741 0:120 / /usr/share/with\\04 rw - tmpfs tmpfs rw\n",
		"a non-octal escape":     "760 741 0:120 / /usr/share/with\\089 rw - tmpfs tmpfs rw\n",
		"an escape out of range": "760 741 0:120 / /usr/share/with\\777 rw - tmpfs tmpfs rw\n",
	} {
		if got, err := mountPointsAtOrUnder([]byte(bad), "/usr/share"); err == nil {
			t.Errorf("%s: mountPointsAtOrUnder = %q, nil; want an error, so the test refuses rather than guess", name, got)
		}
	}
}
