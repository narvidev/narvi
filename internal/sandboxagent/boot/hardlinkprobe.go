package boot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/narvidev/narvi/internal/platform"
)

// The re-own walk re-owns a non-directory entry with more than one link
// only if the kernel refuses the runtime a hard link to a file it neither
// owns nor can read and write: see ChownWorkspaceForRuntime's doc. This
// file answers that question by asking the kernel, not a setting that
// stands for it. fs.protected_hardlinks is such a setting, and it is
// missing exactly where it matters most: gVisor, the runtime Modal
// sandboxes use by default, publishes no /proc/sys/fs/protected_hardlinks
// and enforces the rule unconditionally.
//
// The probe makes the kernel answer for the runtime's own uid and gid. In
// a fresh private directory T (root's, mode 0711) it creates T/src (root's,
// 0600) and T/dst (the runtime's, 0700), links T/src to T/control itself as
// a control, then runs a short-lived child process as the runtime -- this
// binary again, through /proc/self/exe, with hardLinkProbeArg -- that tries
// link(T/src, T/dst/<random name>) and exits with what link(2) returned.
// Only EPERM means the kernel bounds hard links; a link it made means it
// does not. Anything else -- another errno (EEXIST, since a live process of
// the runtime's uid can create names in T/dst; EACCES; EXDEV), a child that
// could not start, that answered nothing this file wrote, or that did not
// finish within platform.HardLinkProbeTimeout -- is inconclusive, and the
// walk then reads fs.protected_hardlinks: "1" means bounded, anything
// else, or no readable value, means not. T is removed afterwards. The
// answer is computed once per process and runtime identity, and logged
// once, at INFO.
//
// The child is run with SysProcAttr.Credential, the same drop the runtime
// itself gets, and never probes with setfsuid on a thread of this process
// instead: setfsuid reports no failure, so where it does not take effect
// the probe would silently link as root and read as unbounded. A drop
// that fails stops the child from starting, which is inconclusive, and
// the child refuses to answer if it is still root.

// hardLinkProbeArg, as a binary's first argument, makes
// RunHardlinkProbeIfRequested answer the probe instead of returning.
const hardLinkProbeArg = "__narvi-hardlink-probe"

// The probe's directory, as the child finds it: open on probeDirFD, the
// first fd after stdin, stdout and stderr, holding probeSrcName and the
// directory probeDstDirName. Handing the child the directory, rather than
// a path to it, spares it resolving T's parents, which the runtime may not
// be allowed to search.
const (
	probeDirFD      = 3
	probeSrcName    = "src"
	probeCtlName    = "control"
	probeDstDirName = "dst"
)

// The child's exit status is its whole answer; it writes nothing. Another
// process of the runtime's uid could write into the child's output through
// /proc, while changing how it exits takes tracing it. And the probe runs
// at the first walk, which precedes every services.yml process (the order
// of the walk's callers is in ChownWorkspaceForRuntime's doc).
// probeExitLinked means link(2) made the link,
// and probeExitLinked+errno that it failed with errno. Every other status
// is no answer: probeExitUsage for arguments this file did not write,
// probeExitPrivileged for a child still running as root, probeExitUnreported
// for an error that is not an errno at most probeMaxErrno -- and any status
// the Go runtime or a binary that never ran the probe exits with.
const (
	probeExitLinked     = 100
	probeMaxErrno       = 150
	probeExitUsage      = 97
	probeExitPrivileged = 98
	probeExitUnreported = 99
)

// hardLinkProbeAnswered records that this binary's entry point called
// RunHardlinkProbeIfRequested, so that re-executing /proc/self/exe with
// hardLinkProbeArg reaches the probe. A binary that never did -- a test
// binary other than this package's -- would run its own main, or its
// whole test suite, as the runtime instead; the probe is then
// inconclusive without running anything.
var hardLinkProbeAnswered atomic.Bool

// RunHardlinkProbeIfRequested must be the first thing done by every binary
// whose ChownWorkspaceForRuntime should run the hard-link probe:
// cmd/sandbox-agent's main, and this package's TestMain. When args is a
// probe's command line it answers the probe and exits the process;
// otherwise it returns, having recorded that this binary can answer one.
func RunHardlinkProbeIfRequested(args []string) {
	hardLinkProbeAnswered.Store(true)
	if len(args) < 2 || args[1] != hardLinkProbeArg {
		return
	}
	os.Exit(answerHardLinkProbe(args[2:]))
}

// answerHardLinkProbe is the child's side of the probe: one link(2) from
// the source to a new name in the runtime's own directory, both relative
// to probeDirFD, and its result as an exit status.
func answerHardLinkProbe(args []string) int {
	if len(args) != 1 || args[0] == "" || strings.ContainsRune(args[0], '/') {
		return probeExitUsage
	}
	if os.Geteuid() == 0 {
		// The drop did not happen, and root may link anything: whatever
		// link(2) returned would say nothing about the runtime.
		return probeExitPrivileged
	}
	return probeExitStatus(unix.Linkat(probeDirFD, probeSrcName, probeDirFD, probeDstDirName+"/"+args[0], 0))
}

// probeExitStatus encodes what link(2) returned as the child's exit status.
func probeExitStatus(err error) int {
	if err == nil {
		return probeExitLinked
	}
	var errno unix.Errno
	if errors.As(err, &errno) && errno > 0 && errno <= probeMaxErrno {
		return probeExitLinked + int(errno)
	}
	return probeExitUnreported
}

// probeLinkResult decodes a child's exit status: answered false for a
// status that answers nothing, and otherwise what its link(2) returned,
// nil for a link it made.
func probeLinkResult(status int) (answered bool, linkErr error) {
	switch {
	case status == probeExitLinked:
		return true, nil
	case status > probeExitLinked && status <= probeExitLinked+probeMaxErrno:
		return true, unix.Errno(status - probeExitLinked)
	default:
		return false, nil
	}
}

// probeResult is what one run of the probe observed.
type probeResult struct {
	// err is why the probe observed no link(2) made as the runtime: the
	// probe did not run, or the child gave no answer. Inconclusive.
	err error
	// linkErr is what link(2), made as the runtime, returned: nil when it
	// made the link. Meaningful only when err is nil.
	linkErr error
	// cleanupErr is why the probe's directory could not be removed; it
	// changes nothing about the answer, and is only logged.
	cleanupErr error
}

// outcome names what the probe observed, for the log: "refused" (EPERM:
// the kernel bounds hard links), "linked" (it does not), or
// "inconclusive", with why.
func (p probeResult) outcome() (outcome string, why error) {
	switch {
	case p.err != nil:
		return "inconclusive", p.err
	case p.linkErr == nil:
		return "linked", nil
	case errors.Is(p.linkErr, unix.EPERM):
		return "refused", nil
	default:
		return "inconclusive", fmt.Errorf("link(2) as the runtime: %w", p.linkErr)
	}
}

// hardLinkProbe is where the probe runs, and how long it waits.
type hardLinkProbe struct {
	// exe is the binary run as the runtime: /proc/self/exe.
	exe string
	// tmpDir is where the probe's private directory is made; empty means
	// os.TempDir().
	tmpDir string
	// timeout bounds the child: platform.HardLinkProbeTimeout.
	timeout time.Duration
}

// productionHardLinkProbe is the probe ChownWorkspaceForRuntime runs.
var productionHardLinkProbe = hardLinkProbe{exe: "/proc/self/exe", timeout: platform.HardLinkProbeTimeout}

// run runs the probe for the runtime identity uid/gid.
func (p hardLinkProbe) run(uid, gid int) probeResult {
	switch {
	case runtime.GOOS != "linux":
		return probeResult{err: fmt.Errorf("the probe runs only on Linux, not %s", runtime.GOOS)}
	case !hardLinkProbeAnswered.Load():
		return probeResult{err: errors.New("this binary cannot answer the probe: its entry point never called RunHardlinkProbeIfRequested")}
	case os.Geteuid() != 0:
		return probeResult{err: errors.New("the probe needs root, to create a file the runtime does not own and to run a process as the runtime")}
	case uid == 0:
		return probeResult{err: errors.New("the runtime is root, which the kernel lets link anything")}
	}
	dir, err := os.MkdirTemp(p.tmpDir, "narvi-hardlink-probe-")
	if err != nil {
		return probeResult{err: fmt.Errorf("create the probe's directory: %w", err)}
	}
	res := p.runIn(dir, uid, gid)
	res.cleanupErr = os.RemoveAll(dir)
	return res
}

// runIn runs the probe in dir, which this process just created.
func (p hardLinkProbe) runIn(dir string, uid, gid int) probeResult {
	inconclusive := func(format string, args ...any) probeResult {
		return probeResult{err: fmt.Errorf(format, args...)}
	}
	// 0711: the runtime may pass through dir, and neither list it nor
	// create or remove anything in it.
	if err := os.Chmod(dir, 0o711); err != nil {
		return inconclusive("set up the probe: %w", err)
	}
	src := filepath.Join(dir, probeSrcName)
	f, err := os.OpenFile(src, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return inconclusive("set up the probe: %w", err)
	}
	err = f.Chown(0, 0)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return inconclusive("set up the probe: %w", err)
	}
	// Root's own link must work, or an EPERM from the runtime's could be
	// the filesystem refusing every hard link, or a seccomp filter the
	// child inherits, rather than the kernel's rule.
	if err := os.Link(src, filepath.Join(dir, probeCtlName)); err != nil {
		return inconclusive("root's own control link failed, so a refusal would prove nothing: %w", err)
	}
	dst := filepath.Join(dir, probeDstDirName)
	if err := os.Mkdir(dst, 0o700); err != nil {
		return inconclusive("set up the probe: %w", err)
	}
	if err := os.Lchown(dst, uid, gid); err != nil {
		return inconclusive("set up the probe: %w", err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return inconclusive("set up the probe: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return inconclusive("set up the probe: %w", err)
	}
	defer func() { _ = d.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.exe, hardLinkProbeArg, hex.EncodeToString(random[:]))
	// No environment at all: this process's carries the sandbox's own
	// bearer token, and the child runs as the runtime.
	cmd.Env = []string{}
	cmd.ExtraFiles = []*os.File{d} // probeDirFD
	// The same drop the runtime gets (runtimeCredentialFor in
	// cmd/sandbox-agent): supplementary groups cleared.
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return inconclusive("no answer within %s", p.timeout)
	}
	var exitErr *exec.ExitError
	var status int
	switch {
	case runErr == nil:
		status = 0
	case errors.As(runErr, &exitErr):
		status = exitErr.ExitCode()
	default:
		return inconclusive("run the probe as the runtime: %w", runErr)
	}
	answered, linkErr := probeLinkResult(status)
	if !answered {
		return inconclusive("the probe exited with status %d, which answers nothing", status)
	}
	return probeResult{linkErr: linkErr}
}

// hardLinkVerdict is whether the kernel bounds what the runtime can
// hard-link, and how the walk found out.
type hardLinkVerdict struct {
	// bounded: the runtime can link only a file it owns or can already
	// read and write, so the walk re-owns entries with more than one link.
	bounded bool
	probe   probeResult
	// settingRead: the probe was inconclusive, and fs.protected_hardlinks
	// decided; setting and settingErr are what reading it gave.
	settingRead bool
	setting     string
	settingErr  error
}

// decideHardLinks turns what the probe observed into a verdict, reading
// fs.protected_hardlinks through readSetting only if the probe was
// inconclusive.
func decideHardLinks(probe probeResult, readSetting func() (string, error)) hardLinkVerdict {
	v := hardLinkVerdict{probe: probe}
	switch outcome, _ := probe.outcome(); outcome {
	case "refused":
		v.bounded = true
		return v
	case "linked":
		return v
	}
	v.settingRead = true
	v.setting, v.settingErr = readSetting()
	v.bounded = v.settingErr == nil && v.setting == "1"
	return v
}

// log records the verdict, once, at INFO.
func (v hardLinkVerdict) log(logger *slog.Logger, uid, gid int) {
	outcome, why := v.probe.outcome()
	attrs := []any{"runtime_uid", uid, "runtime_gid", gid, "reown_hard_linked", v.bounded, "probe", outcome}
	if why != nil {
		attrs = append(attrs, "probe_error", why.Error())
	}
	if v.probe.cleanupErr != nil {
		attrs = append(attrs, "probe_cleanup_error", v.probe.cleanupErr.Error())
	}
	if v.settingRead {
		if v.settingErr != nil {
			attrs = append(attrs, "protected_hardlinks_error", v.settingErr.Error())
		} else {
			attrs = append(attrs, "protected_hardlinks", v.setting)
		}
	}
	logger.Info("boot: re-own walk: whether the kernel bounds what the runtime can hard-link", attrs...)
}

// hardLinkVerdicts holds the production verdict per runtime identity: the
// probe runs, and its verdict is logged, once per process and identity.
var hardLinkVerdicts struct {
	sync.Mutex
	byRuntime map[[2]int]hardLinkVerdict
}

// productionHardLinkVerdict is the verdict ChownWorkspaceForRuntime acts
// on: productionHardLinkProbe, then readProtectedHardlinks if the probe is
// inconclusive, computed and logged to logger on the first call for
// uid/gid, and the same verdict on every later one.
func productionHardLinkVerdict(uid, gid int, logger *slog.Logger) hardLinkVerdict {
	hardLinkVerdicts.Lock()
	defer hardLinkVerdicts.Unlock()
	key := [2]int{uid, gid}
	if v, ok := hardLinkVerdicts.byRuntime[key]; ok {
		return v
	}
	v := decideHardLinks(productionHardLinkProbe.run(uid, gid), readProtectedHardlinks)
	v.log(logger, uid, gid)
	if hardLinkVerdicts.byRuntime == nil {
		hardLinkVerdicts.byRuntime = map[[2]int]hardLinkVerdict{}
	}
	hardLinkVerdicts.byRuntime[key] = v
	return v
}

// protectedHardlinksPath is where Linux publishes fs.protected_hardlinks.
const protectedHardlinksPath = "/proc/sys/fs/protected_hardlinks"

// readProtectedHardlinks returns the kernel's fs.protected_hardlinks
// setting, trimmed. Only Linux has one; anywhere else this is an error,
// which the walk treats like any setting it cannot read.
func readProtectedHardlinks() (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("%s: no such setting on %s", protectedHardlinksPath, runtime.GOOS)
	}
	b, err := os.ReadFile(protectedHardlinksPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
