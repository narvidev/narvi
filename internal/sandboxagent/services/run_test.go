package services_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/servicemanifest"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/services"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// --- small test helpers -----------------------------------------------

func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }

// freePort binds to 127.0.0.1:0, reads back the actual ephemeral port the
// kernel assigned, then immediately closes the listener -- so the caller
// gets a real, currently-unused port number without ever hardcoding one.
// There is an inherent, accepted TOCTOU window between this Close and
// whatever the caller spawns to rebind the same port; standard practice
// for this kind of test.
func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// neverBoundPort is a fixed, low TCP port for the two tests
// (TestRun_PrimaryTimeoutIsFatal, TestRun_SecondaryTimeoutLeavesProcessRunning)
// that need a port GUARANTEED to stay closed for the readiness timeout's
// entire duration, not merely at one instant -- freePort's own "bind,
// read the port, close" pattern only proves the port was free the moment
// it was checked. Once closed, the kernel is free to hand that exact
// ephemeral port to any other process on the machine for the rest of the
// test; when it does, portReady's TCP dial legitimately succeeds against
// that unrelated listener, and Run wrongly observes PhaseReady instead of
// PhaseTimeout -- not a bug in the code under test, but the test's own
// port choice (observed once on a loaded CI runner).
//
// 1 (TCPMUX, RFC 1078) is below every mainstream OS's own ephemeral/
// dynamic port range floor (Linux and macOS/BSD both start at 32768 or
// higher), so the kernel's automatic port allocator -- the actual
// mechanism that stole freePort's own released port above -- can never
// hand it to an unrelated process, on a loaded machine or otherwise. That
// holds independent of the test process's own privilege level: the
// restriction is on what the kernel auto-assigns, not on who may
// explicitly bind low ports. Nothing on an ordinary CI runner or dev
// machine binds it on purpose either -- unlike e.g. 22 (ssh) or 631
// (cups on macOS), TCPMUX has no real-world users to collide with.
const neverBoundPort = 1

// tcpListenerCmd is a real, separate process (python3, reliably present on
// both macOS and Linux CI) that opens a TCP listener on port after
// sleeping delaySeconds, then stays up long enough for the test to observe
// it. listen() alone (no accept() call) is sufficient for a TCP dial to
// this port to succeed, since the kernel completes the handshake into the
// backlog.
func tcpListenerCmd(port int, delaySeconds float64) string {
	return fmt.Sprintf(
		`python3 -c "import socket,time;time.sleep(%v);s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1', %d));s.listen(1);time.sleep(30)"`,
		delaySeconds, port,
	)
}

// probeEnvAndListenCmd is a real, separate process (python3) that writes
// NARVI_SESSION_CONFIG's own value as seen by ITS OWN os.environ (or the
// literal "ABSENT" if unset/empty) to probeFile, then opens a real TCP
// listener on port -- so the existing Port-readiness path
// (TestRun_PortReadiness's own technique) can observe it becoming ready
// without any time.Sleep-based synchronization on the test's own side.
func probeEnvAndListenCmd(probeFile string, port int) string {
	return fmt.Sprintf(
		`python3 -c "import os,socket,time; open('%s','w').write(os.environ.get('NARVI_SESSION_CONFIG') or 'ABSENT'); s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(('127.0.0.1', %d)); s.listen(1); time.sleep(30)"`,
		probeFile, port,
	)
}

// httpServerCmd is a real, separate process serving a plain 200 (Python's
// standard-library http.server -- GET / on a fresh directory listing
// returns 200) after a short delay, so the readiness poll loop is
// exercised rather than succeeding on the very first attempt.
func httpServerCmd(port int, delaySeconds float64) string {
	return fmt.Sprintf("sleep %v && exec python3 -m http.server %d --bind 127.0.0.1", delaySeconds, port)
}

// sleepWithPIDFileCmd records its own pid to pidFile immediately, then
// sleeps -- used to independently prove (via syscall.Kill(pid, 0), the
// same POSIX signal-0 liveness check internal/sandboxagent/supervisor's
// own tests use) that a service Run() left running is genuinely still
// alive, not proactively stopped.
func sleepWithPIDFileCmd(pidFile string, sleepSeconds int) string {
	return fmt.Sprintf(`echo $$ > '%s'; sleep %d`, pidFile, sleepSeconds)
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			trimmed := strings.TrimSpace(string(raw))
			if trimmed != "" {
				pid, convErr := strconv.Atoi(trimmed)
				if convErr == nil {
					return pid
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("pid file %s never populated", path)
	return 0
}

// eventCollector is a test ProgressReporter that records every event it
// receives, safe for concurrent use since Run reports from multiple
// per-service goroutines.
type eventCollector struct {
	mu     sync.Mutex
	events []services.BootProgressEvent
}

func (c *eventCollector) report(e services.BootProgressEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *eventCollector) forService(name string) []services.BootProgressEvent {
	c.mu.Lock()
	defer c.mu.Unlock()

	var out []services.BootProgressEvent
	for _, e := range c.events {
		if e.ServiceName == name {
			out = append(out, e)
		}
	}
	return out
}

// assertSequence checks collector recorded exactly [PhaseStarting, want]
// for serviceName, in that order.
func assertSequence(t *testing.T, collector *eventCollector, serviceName string, want services.Phase) {
	t.Helper()

	got := collector.forService(serviceName)
	if len(got) != 2 {
		t.Fatalf("service %q: got %d events, want 2 (PhaseStarting then %s): %+v", serviceName, len(got), want, got)
	}
	if got[0].Phase != services.PhaseStarting {
		t.Errorf("service %q: first event phase = %s, want %s", serviceName, got[0].Phase, services.PhaseStarting)
	}
	if got[1].Phase != want {
		t.Errorf("service %q: second event phase = %s, want %s", serviceName, got[1].Phase, want)
	}
	if want == services.PhaseFailed && got[1].Err == nil {
		t.Errorf("service %q: PhaseFailed event has nil Err, want non-nil", serviceName)
	}
	if want != services.PhaseFailed && got[1].Err != nil {
		t.Errorf("service %q: phase %s event has non-nil Err = %v, want nil", serviceName, want, got[1].Err)
	}
}

func stopAllOnCleanup(t *testing.T, sup *supervisor.Supervisor) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sup.StopAll(ctx, time.Second)
	})
}

// --- tests --------------------------------------------------------------

func TestRun_PortReadiness(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	sup := supervisor.New()
	stopAllOnCleanup(t, sup)

	manifest := servicemanifest.Manifest{Services: []servicemanifest.Service{
		{
			Name:        "web",
			Cmd:         tcpListenerCmd(port, 0),
			Readiness:   servicemanifest.Readiness{Port: intPtr(port)},
			Criticality: servicemanifest.CriticalityPrimary,
		},
	}}

	collector := &eventCollector{}
	err := services.Run(context.Background(), sup, t.TempDir(), manifest, nil, collector.report,
		5*time.Second, 50*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	assertSequence(t, collector, "web", services.PhaseReady)
}

// TestRun_EnvExcludesSessionConfig proves the real regression this Step's
// env-leak remediation fixes: a services.yml command spawned via Run must
// NOT inherit NARVI_SESSION_CONFIG (the sandbox's own plaintext bearer
// token) when the caller passes supervisor.EnvWithout(boot.
// SessionConfigEnvVar) as Run's own env parameter -- exactly what
// internal/sandboxagent/boot.RunBoot does in production (runboot.go).
// t.Setenv sets the marker on the TEST process itself; EnvWithout reads it
// straight from os.Environ() at call time, so this is a real, observed
// process-level exclusion, not a mock.
func TestRun_EnvExcludesSessionConfig(t *testing.T) {
	// Not t.Parallel(): t.Setenv forbids combining the two.
	t.Setenv("NARVI_SESSION_CONFIG", "marker-should-not-reach-child")

	port := freePort(t)
	sup := supervisor.New()
	stopAllOnCleanup(t, sup)

	probeFile := filepath.Join(t.TempDir(), "probe")
	manifest := servicemanifest.Manifest{Services: []servicemanifest.Service{
		{
			Name:        "probe",
			Cmd:         probeEnvAndListenCmd(probeFile, port),
			Readiness:   servicemanifest.Readiness{Port: intPtr(port)},
			Criticality: servicemanifest.CriticalityPrimary,
		},
	}}

	env := supervisor.EnvWithout(boot.SessionConfigEnvVar)

	collector := &eventCollector{}
	err := services.Run(context.Background(), sup, t.TempDir(), manifest, env, collector.report,
		5*time.Second, 50*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	assertSequence(t, collector, "probe", services.PhaseReady)

	got, err := os.ReadFile(probeFile)
	if err != nil {
		t.Fatalf("read probe file: %v", err)
	}
	if string(got) != "ABSENT" {
		t.Errorf("probe file = %q, want %q (NARVI_SESSION_CONFIG must not reach the spawned service)", got, "ABSENT")
	}
}

func TestRun_HealthReadiness(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	sup := supervisor.New()
	stopAllOnCleanup(t, sup)

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	manifest := servicemanifest.Manifest{Services: []servicemanifest.Service{
		{
			Name:        "mock-api",
			Cmd:         httpServerCmd(port, 0.3),
			Readiness:   servicemanifest.Readiness{Health: strPtr(url)},
			Criticality: servicemanifest.CriticalityPrimary,
		},
	}}

	collector := &eventCollector{}
	err := services.Run(context.Background(), sup, t.TempDir(), manifest, nil, collector.report,
		5*time.Second, 50*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	assertSequence(t, collector, "mock-api", services.PhaseReady)
}

func TestRun_PrimaryCrashIsFatal(t *testing.T) {
	t.Parallel()

	sup := supervisor.New()
	stopAllOnCleanup(t, sup)

	manifest := servicemanifest.Manifest{Services: []servicemanifest.Service{
		{
			Name:        "crashes",
			Cmd:         "exit 1",
			Readiness:   servicemanifest.Readiness{Port: intPtr(freePort(t))},
			Criticality: servicemanifest.CriticalityPrimary,
		},
	}}

	collector := &eventCollector{}
	err := services.Run(context.Background(), sup, t.TempDir(), manifest, nil, collector.report,
		5*time.Second, 50*time.Millisecond, nil)
	if err == nil {
		t.Fatal("Run() error = nil, want a fatal error (primary service crashed before ready)")
	}

	assertSequence(t, collector, "crashes", services.PhaseFailed)
}

// TestRun_PrimaryCleanExitIsAlsoFatal proves the foreground-only assumption
// documented in doc.go/exitErr: a CLEAN exit (code 0) before readiness is
// just as much a failure as a nonzero one -- a service is expected to keep
// running once started, so any exit at all before it ever became ready is
// unexpected, regardless of exit code. This exercises exitErr's final
// branch (Err == nil, ExitCode == 0), which TestRun_PrimaryCrashIsFatal's
// "exit 1" case does not reach.
func TestRun_PrimaryCleanExitIsAlsoFatal(t *testing.T) {
	t.Parallel()

	sup := supervisor.New()
	stopAllOnCleanup(t, sup)

	manifest := servicemanifest.Manifest{Services: []servicemanifest.Service{
		{
			Name:        "exits-cleanly",
			Cmd:         "exit 0",
			Readiness:   servicemanifest.Readiness{Port: intPtr(freePort(t))}, // never opened by "exit 0"
			Criticality: servicemanifest.CriticalityPrimary,
		},
	}}

	collector := &eventCollector{}
	err := services.Run(context.Background(), sup, t.TempDir(), manifest, nil, collector.report,
		5*time.Second, 50*time.Millisecond, nil)
	if err == nil {
		t.Fatal("Run() error = nil, want a fatal error (a clean exit(0) before readiness is still a crash here)")
	}

	// assertSequence itself asserts a non-nil Err for PhaseFailed -- this is
	// what proves exitErr's clean-exit branch produces a real, non-nil
	// error rather than being mistaken for success.
	assertSequence(t, collector, "exits-cleanly", services.PhaseFailed)
}

// TestRun_MixedOutcomes_OneCrashesOneSucceeds proves there is no cross-talk
// between concurrently-running services: one primary service crashing must
// not affect a SIBLING primary service's own independent readiness outcome,
// and Run's fatal path must not proactively stop the sibling that DID
// succeed (matching internal/sandboxagent/boot.RunHooks' own precedent of
// never calling StopAll on a fatal failure).
func TestRun_MixedOutcomes_OneCrashesOneSucceeds(t *testing.T) {
	t.Parallel()

	sup := supervisor.New()
	stopAllOnCleanup(t, sup)

	survivorPort := freePort(t)
	pidFile := filepath.Join(t.TempDir(), "survivor.pid")

	manifest := servicemanifest.Manifest{Services: []servicemanifest.Service{
		{
			Name:        "crashes",
			Cmd:         "exit 1",
			Readiness:   servicemanifest.Readiness{Port: intPtr(freePort(t))},
			Criticality: servicemanifest.CriticalityPrimary,
		},
		{
			Name: "survives",
			Cmd: fmt.Sprintf("%s; %s",
				fmt.Sprintf(`echo $$ > '%s'`, pidFile),
				tcpListenerCmd(survivorPort, 0)),
			Readiness:   servicemanifest.Readiness{Port: intPtr(survivorPort)},
			Criticality: servicemanifest.CriticalityPrimary,
		},
	}}

	collector := &eventCollector{}
	err := services.Run(context.Background(), sup, t.TempDir(), manifest, nil, collector.report,
		5*time.Second, 50*time.Millisecond, nil)
	if err == nil {
		t.Fatal("Run() error = nil, want a fatal error naming the crashed service")
	}
	if !strings.Contains(err.Error(), "crashes") {
		t.Errorf("Run() error = %v, want it to name the crashed service %q", err, "crashes")
	}

	assertSequence(t, collector, "crashes", services.PhaseFailed)
	assertSequence(t, collector, "survives", services.PhaseReady)

	survivorPID := waitForPIDFile(t, pidFile)
	if !processAlive(survivorPID) {
		t.Errorf("survivor pid %d not alive after Run() returned -- a sibling's crash must not stop it "+
			"(Run never calls StopAll on its own fatal path)", survivorPID)
	}
}

func TestRun_SecondaryTimeoutLeavesProcessRunning(t *testing.T) {
	t.Parallel()

	sup := supervisor.New()
	stopAllOnCleanup(t, sup)

	pidFile := filepath.Join(t.TempDir(), "svc.pid")
	manifest := servicemanifest.Manifest{Services: []servicemanifest.Service{
		{
			Name:        "slow-secondary",
			Cmd:         sleepWithPIDFileCmd(pidFile, 30),
			Readiness:   servicemanifest.Readiness{Port: intPtr(neverBoundPort)}, // never opened by the script above
			Criticality: servicemanifest.CriticalitySecondary,
		},
	}}

	collector := &eventCollector{}
	err := services.Run(context.Background(), sup, t.TempDir(), manifest, nil, collector.report,
		300*time.Millisecond, 30*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (a secondary service's timeout is only a warning)", err)
	}

	assertSequence(t, collector, "slow-secondary", services.PhaseTimeout)

	pid := waitForPIDFile(t, pidFile)
	if !processAlive(pid) {
		t.Errorf("pid %d not alive after Run() timed out on a secondary service -- it must be left running, not killed", pid)
	}
}

func TestRun_PrimaryTimeoutIsFatal(t *testing.T) {
	t.Parallel()

	sup := supervisor.New()
	stopAllOnCleanup(t, sup)

	pidFile := filepath.Join(t.TempDir(), "svc.pid")
	manifest := servicemanifest.Manifest{Services: []servicemanifest.Service{
		{
			Name:        "slow-primary",
			Cmd:         sleepWithPIDFileCmd(pidFile, 30),
			Readiness:   servicemanifest.Readiness{Port: intPtr(neverBoundPort)}, // never opened by the script above
			Criticality: servicemanifest.CriticalityPrimary,
		},
	}}

	collector := &eventCollector{}
	err := services.Run(context.Background(), sup, t.TempDir(), manifest, nil, collector.report,
		300*time.Millisecond, 30*time.Millisecond, nil)
	if err == nil {
		t.Fatal("Run() error = nil, want a fatal error (primary service never became ready)")
	}

	assertSequence(t, collector, "slow-primary", services.PhaseTimeout)
}

// TestRun_ServicesRunConcurrently proves the three services' readiness
// waits overlap rather than run one at a time: total elapsed must stay
// close to the SLOWEST individual delay, nowhere near the SUM of all
// three.
func TestRun_ServicesRunConcurrently(t *testing.T) {
	t.Parallel()

	// fastDelay/mediumDelay are deliberately larger than the minimum
	// needed to prove ordering -- this widens the gap between sumOfDelays
	// (the sequential-would-take bound below) and slowestDelay (the actual
	// concurrent wall-clock bound, since real elapsed time is governed by
	// the slowest service, not by fast/medium) without changing the test's
	// own real-world runtime at all. A previously tighter margin (0.6s
	// medium against a 2.0s sum) produced a real, observed CI flake
	// ("Run() took 2.206698958s, want well under the sequential sum 2s")
	// on a resource-contended runner, confirmed non-reproducing in 5 clean
	// local reruns immediately after -- process-spawn/scheduling jitter
	// under contention, not a genuine concurrency regression. Widening
	// this margin (2.7s sum vs. the same 1.4s slowest/actual-runtime
	// bound) is the fix.
	const (
		fastDelay   = 0.4
		mediumDelay = 0.9
		slowDelay   = 1.4
	)

	portFast := freePort(t)
	portMedium := freePort(t)
	portSlow := freePort(t)

	sup := supervisor.New()
	stopAllOnCleanup(t, sup)

	manifest := servicemanifest.Manifest{Services: []servicemanifest.Service{
		{
			Name:        "fast",
			Cmd:         tcpListenerCmd(portFast, fastDelay),
			Readiness:   servicemanifest.Readiness{Port: intPtr(portFast)},
			Criticality: servicemanifest.CriticalityPrimary,
		},
		{
			Name:        "medium",
			Cmd:         tcpListenerCmd(portMedium, mediumDelay),
			Readiness:   servicemanifest.Readiness{Port: intPtr(portMedium)},
			Criticality: servicemanifest.CriticalityPrimary,
		},
		{
			Name:        "slow",
			Cmd:         tcpListenerCmd(portSlow, slowDelay),
			Readiness:   servicemanifest.Readiness{Port: intPtr(portSlow)},
			Criticality: servicemanifest.CriticalityPrimary,
		},
	}}

	collector := &eventCollector{}

	start := time.Now()
	err := services.Run(context.Background(), sup, t.TempDir(), manifest, nil, collector.report,
		5*time.Second, 50*time.Millisecond, nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	assertSequence(t, collector, "fast", services.PhaseReady)
	assertSequence(t, collector, "medium", services.PhaseReady)
	assertSequence(t, collector, "slow", services.PhaseReady)

	sumOfDelays := time.Duration((fastDelay + mediumDelay + slowDelay) * float64(time.Second))
	if elapsed >= sumOfDelays {
		t.Errorf("Run() took %v, want well under the sequential sum %v -- services did not run concurrently",
			elapsed, sumOfDelays)
	}

	slowestDelay := time.Duration(slowDelay * float64(time.Second))
	if elapsed < slowestDelay {
		t.Errorf("Run() took %v, want at least the slowest individual delay %v", elapsed, slowestDelay)
	}
}
