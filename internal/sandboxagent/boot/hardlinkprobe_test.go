package boot

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// maskedEnv, when set, says /proc/sys/fs is masked in this environment --
// an empty read-only tmpfs mounted over it, which is how gVisor presents
// it -- and gives the fs.protected_hardlinks value the kernel behind the
// mask really has. TestChownWorkspaceForRuntime_ReownsHardLinksWhereTheSettingIsMasked
// runs only then.
const maskedEnv = "NARVI_TEST_MASKED_PROTECTED_HARDLINKS"

// maskedProtectedHardlinks returns maskedEnv's value.
func maskedProtectedHardlinks() string { return os.Getenv(maskedEnv) }

// requireLinuxRoot skips unless the test runs on Linux as root, which a
// real probe needs: only root creates a file the runtime does not own and
// runs a process as the runtime.
func requireLinuxRoot(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("requires linux; running on %s", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root (euid 0)")
	}
}

// kernelBoundsHardLinks returns what the kernel says fs.protected_hardlinks
// is, from the setting or, where it is masked, from maskedEnv, and skips
// when neither says.
func kernelBoundsHardLinks(t *testing.T) bool {
	t.Helper()
	value, err := readProtectedHardlinks()
	if err != nil {
		value = maskedProtectedHardlinks()
	}
	switch value {
	case "1":
		return true
	case "0":
		return false
	}
	t.Skipf("fs.protected_hardlinks is unreadable (%v) and %s is unset: nothing says what this kernel does", err, maskedEnv)
	return false
}

// TestProbeExitStatus_RoundTrip pins the child's only channel: link(2)'s
// result as an exit status, decoded back to the same result, and every
// status the child does not produce for a link(2) decoded as no answer --
// 0 included, which any binary that never ran the probe may exit with.
func TestProbeExitStatus_RoundTrip(t *testing.T) {
	for _, err := range []error{nil, unix.EPERM, unix.EEXIST, unix.EACCES, unix.EXDEV, unix.Errno(probeMaxErrno)} {
		status := probeExitStatus(err)
		ok, got := probeLinkResult(status)
		if !ok || got != err {
			t.Errorf("link(2) result %v -> status %d -> %v, %t, want %v, true", err, status, got, ok, err)
		}
	}
	for _, err := range []error{unix.Errno(probeMaxErrno + 1), errors.New("not an errno")} {
		if status := probeExitStatus(err); status != probeExitUnreported {
			t.Errorf("probeExitStatus(%v) = %d, want probeExitUnreported (%d)", err, status, probeExitUnreported)
		}
	}
	for _, status := range []int{0, 1, 2, probeExitUsage, probeExitPrivileged, probeExitUnreported, probeExitLinked + probeMaxErrno + 1, 255, -1} {
		if ok, got := probeLinkResult(status); ok {
			t.Errorf("probeLinkResult(%d) = %v, true, want no answer", status, got)
		}
	}
}

// TestAnswerHardLinkProbe_AnswersOnlyWhatTheProbeAsks pins the child's
// refusals: arguments the parent did not write, and, as root, any answer
// at all, since root may link anything.
func TestAnswerHardLinkProbe_AnswersOnlyWhatTheProbeAsks(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"a/b"}, {"a", "b"}} {
		if got := answerHardLinkProbe(args); got != probeExitUsage {
			t.Errorf("answerHardLinkProbe(%q) = %d, want probeExitUsage (%d)", args, got, probeExitUsage)
		}
	}
	if os.Geteuid() == 0 {
		if got := answerHardLinkProbe([]string{"name"}); got != probeExitPrivileged {
			t.Errorf("answerHardLinkProbe as root = %d, want probeExitPrivileged (%d)", got, probeExitPrivileged)
		}
	}
}

// TestHardLinkProbe_InconclusiveWhereItCannotAsk pins the conditions under
// which the probe does not run a child at all: off Linux, without root,
// for a runtime that is root, and in a binary whose entry point cannot
// answer.
func TestHardLinkProbe_InconclusiveWhereItCannotAsk(t *testing.T) {
	probe := productionHardLinkProbe
	probe.tmpDir = t.TempDir()
	type check struct {
		name     string
		applies  bool
		uid, gid int
		setup    func(t *testing.T)
		want     string
	}
	linuxRoot := runtime.GOOS == "linux" && os.Geteuid() == 0
	checks := []check{
		{name: "off Linux", applies: runtime.GOOS != "linux", uid: 65534, gid: 65534, want: "only on Linux"},
		{name: "without root", applies: runtime.GOOS == "linux" && os.Geteuid() != 0, uid: 65534, gid: 65534, want: "needs root"},
		{name: "a root runtime", applies: linuxRoot, uid: 0, gid: 0, want: "runtime is root"},
		{name: "a binary that cannot answer", applies: linuxRoot, uid: 65534, gid: 65534, want: "cannot answer the probe", setup: func(t *testing.T) {
			hardLinkProbeAnswered.Store(false)
			t.Cleanup(func() { hardLinkProbeAnswered.Store(true) })
		}},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if !c.applies {
				t.Skip("not this environment")
			}
			if c.setup != nil {
				c.setup(t)
			}
			res := probe.run(c.uid, c.gid)
			if res.err == nil || !strings.Contains(res.err.Error(), c.want) {
				t.Errorf("run() = %+v, want inconclusive with an error containing %q", res, c.want)
			}
			requireEmptyDir(t, probe.tmpDir)
		})
	}
}

// TestHardLinkProbe_RealKernel runs the real probe against the kernel this
// test runs on, as uid 65534: refused where the kernel bounds hard links,
// a link made where it does not. It must never be inconclusive here, and
// must leave nothing behind.
func TestHardLinkProbe_RealKernel(t *testing.T) {
	requireLinuxRoot(t)
	bounded := kernelBoundsHardLinks(t)
	probe := productionHardLinkProbe
	probe.tmpDir = t.TempDir()

	res := probe.run(65534, 65534)
	outcome, why := res.outcome()
	want := map[bool]string{true: "refused", false: "linked"}[bounded]
	if outcome != want {
		t.Errorf("probe outcome = %q (%v), want %q: the kernel's fs.protected_hardlinks is %t", outcome, why, want, bounded)
	}
	if res.cleanupErr != nil {
		t.Errorf("cleanupErr = %v, want nil", res.cleanupErr)
	}
	requireEmptyDir(t, probe.tmpDir)
}

// TestHardLinkProbe_NoAnswerIsInconclusive pins that a child that fails
// to start, exits without answering, or answers nothing in time makes the
// probe inconclusive, never a verdict, and that the probe's directory is
// removed on each of those paths.
func TestHardLinkProbe_NoAnswerIsInconclusive(t *testing.T) {
	requireLinuxRoot(t)
	yes, err := exec.LookPath("yes")
	if err != nil {
		t.Skipf("no yes(1) to stand in for a child that never answers: %v", err)
	}
	cases := []struct {
		name    string
		exe     string
		timeout time.Duration
		want    string
	}{
		{name: "the child cannot start", exe: "/nonexistent/narvi-probe", timeout: time.Minute, want: "run the probe as the runtime"},
		{name: "the child exits without answering", exe: "/bin/true", timeout: time.Minute, want: "status 0"},
		{name: "the child never answers", exe: yes, timeout: 300 * time.Millisecond, want: "no answer within"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			probe := hardLinkProbe{exe: c.exe, tmpDir: t.TempDir(), timeout: c.timeout}
			res := probe.run(65534, 65534)
			if outcome, why := res.outcome(); outcome != "inconclusive" || why == nil || !strings.Contains(why.Error(), c.want) {
				t.Errorf("probe = %q (%v), want inconclusive with an error containing %q", outcome, why, c.want)
			}
			requireEmptyDir(t, probe.tmpDir)
		})
	}
}

// TestChownWorkspaceForRuntime_ReownsHardLinksWhereTheSettingIsMasked is
// the provider in use today, reproduced: /proc/sys/fs masked, as gVisor
// presents it, on a kernel that bounds hard links. The production entry
// point, with nothing injected, must still find the bound -- the setting
// cannot say, only the probe can -- and re-own every hard-linked entry: a
// package store's file hard-linked into node_modules from outside the
// tree, with its outside name, and a pair of build outputs inside it.
//
// Runs only where maskedEnv is set, in a container run as root with
// --privileged and an empty read-only tmpfs mounted over /proc/sys/fs.
func TestChownWorkspaceForRuntime_ReownsHardLinksWhereTheSettingIsMasked(t *testing.T) {
	requireLinuxRoot(t)
	if maskedProtectedHardlinks() == "" {
		t.Skipf("set %s to the kernel's real fs.protected_hardlinks, with /proc/sys/fs masked by an empty read-only tmpfs, to run this", maskedEnv)
	}
	if _, err := readProtectedHardlinks(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("readProtectedHardlinks() error = %v, want ErrNotExist: %s is set but /proc/sys/fs is not masked", err, maskedEnv)
	}
	if !kernelBoundsHardLinks(t) {
		t.Skip("the kernel behind the mask does not bound hard links, so the linked entries must stay")
	}
	resetHardLinkVerdicts(t)

	root, store := t.TempDir(), t.TempDir()
	pkg := "repo/node_modules/.pnpm/left-pad@1.3.0/node_modules/left-pad"
	buildTree(t, store, []string{"v3/files/ab"}, []string{"v3/files/ab/cdef"})
	buildTree(t, root, []string{pkg, "repo/target/debug/deps", "repo/src"}, []string{"repo/src/app.js", "repo/target/debug/deps/app-0123abcd"})
	storeFile := filepath.Join(store, "v3", "files", "ab", "cdef")
	hardLink(t, storeFile, filepath.Join(root, pkg, "index.js"))
	hardLink(t, filepath.Join(root, "repo", "target", "debug", "deps", "app-0123abcd"), filepath.Join(root, "repo", "target", "debug", "app"))

	if err := ChownWorkspaceForRuntime(root, 65534, 65534); err != nil {
		t.Fatalf("ChownWorkspaceForRuntime() error = %v, want nil", err)
	}
	requireAllOwnedBy(t, root, 65534, 65534)
	if u, g := ownerOf(t, storeFile); u != 65534 || g != 65534 {
		t.Errorf("the store's own name for the linked file is %d:%d, want 65534:65534: it is the same inode", u, g)
	}
}

// requireEmptyDir fails unless dir holds no entry.
func requireEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	for _, e := range entries {
		t.Errorf("%s was left behind in %s", e.Name(), dir)
	}
}

// requireAllOwnedBy fails for every entry under dir, dir included, not
// owned by uid:gid.
func requireAllOwnedBy(t *testing.T, dir string, uid, gid uint32) {
	t.Helper()
	for path, owner := range owners(t, dir) {
		if owner != [2]uint32{uid, gid} {
			t.Errorf("%s is %d:%d, want %d:%d", path, owner[0], owner[1], uid, gid)
		}
	}
}
