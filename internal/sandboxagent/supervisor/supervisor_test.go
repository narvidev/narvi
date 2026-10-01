package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// spawnShell spawns `/bin/sh -c script` under sup, failing the test
// immediately on a Spawn error. /bin/sh is always present on both macOS
// and Linux CI runners.
func spawnShell(t *testing.T, sup *Supervisor, script string) *Process {
	t.Helper()

	proc, err := sup.Spawn(Spec{Path: "/bin/sh", Args: []string{"-c", script}})
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	return proc
}

// processAlive reports whether pid currently identifies a live process,
// using the POSIX convention of signal 0 (no actual signal sent, only
// existence/permission checked).
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestSpawn_NaturalExit(t *testing.T) {
	t.Parallel()

	sup := New()
	proc := spawnShell(t, sup, "exit 7")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := proc.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Err != nil {
		t.Errorf("result.Err = %v, want nil", result.Err)
	}
	if result.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", result.ExitCode)
	}
}

func TestSpawn_NonexistentPath(t *testing.T) {
	t.Parallel()

	sup := New()
	_, err := sup.Spawn(Spec{Path: "/nonexistent/narvi-test-binary-xyz"})
	if err == nil {
		t.Fatal("Spawn() error = nil, want an error for a nonexistent/non-executable path")
	}
}

// TestSupervisor_SpawnCount proves SpawnCount counts every Spawn call, one
// whose process never started included, and nothing else -- stopping the
// processes leaves it as it was. A test asserting that a code path spawned
// nothing (cmd/sandbox-agent's push rejection test) relies on both halves:
// a count that missed a call would let a spawn through unseen.
func TestSupervisor_SpawnCount(t *testing.T) {
	t.Parallel()

	const (
		starts = "/bin/sh"
		fails  = "/nonexistent/narvi-test-binary-xyz"
	)

	tests := []struct {
		name  string
		paths []string
		want  int
	}{
		{name: "nothing spawned", paths: nil, want: 0},
		{name: "one process that starts", paths: []string{starts}, want: 1},
		{name: "one spawn that fails to start", paths: []string{fails}, want: 1},
		{name: "starts and failures together", paths: []string{starts, fails, starts}, want: 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sup := New()
			for _, path := range tc.paths {
				_, _ = sup.Spawn(Spec{Path: path, Args: []string{"-c", "exit 0"}})
			}
			if got := sup.SpawnCount(); got != tc.want {
				t.Errorf("SpawnCount() after spawning %v = %d, want %d", tc.paths, got, tc.want)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := sup.StopAll(ctx, time.Second); err != nil {
				t.Fatalf("StopAll() error = %v", err)
			}
			if got := sup.SpawnCount(); got != tc.want {
				t.Errorf("SpawnCount() after StopAll = %d, want %d unchanged", got, tc.want)
			}
		})
	}
}

func TestWait_ContextDeadlineExceeded(t *testing.T) {
	t.Parallel()

	sup := New()
	proc := spawnShell(t, sup, `trap "exit 0" TERM; while true; do :; done`)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = proc.Stop(ctx, time.Second)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := proc.Wait(ctx)
	if err == nil {
		t.Fatal("Wait() error = nil, want context.DeadlineExceeded for a still-running process")
	}
}

func TestStop_Graceful(t *testing.T) {
	t.Parallel()

	sup := New()
	proc := spawnShell(t, sup, `trap "exit 0" TERM; while true; do :; done`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const grace = 5 * time.Second // generous; the process should exit long before this elapses

	start := time.Now()
	if err := proc.Stop(ctx, grace); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	elapsed := time.Since(start)

	// Well under the grace period proves the SIGTERM was handled directly
	// and Stop() never needed to escalate to SIGKILL.
	if elapsed >= grace/2 {
		t.Errorf("Stop() took %v, want well under grace period %v (no SIGKILL escalation expected)", elapsed, grace)
	}
}

func TestStop_ForcefulEscalation(t *testing.T) {
	t.Parallel()

	sup := New()
	proc := spawnShell(t, sup, `trap '' TERM; while true; do :; done`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const shortGrace = 200 * time.Millisecond // _test.go is exempt from notimeliteral

	start := time.Now()
	if err := proc.Stop(ctx, shortGrace); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	elapsed := time.Since(start)

	// Stop() returning at all (rather than the test timing out) already
	// proves the SIGKILL escalation fired and unblocked it; the bound
	// below is a generous sanity check, well above the actual OS signal
	// delivery latency.
	if elapsed >= 5*time.Second {
		t.Errorf("Stop() took %v, want well under 5s", elapsed)
	}
}

// TestStop_ProcessGroupKill is the single most important test in this
// Step: it proves killpg-style group signaling actually reaches a
// grandchild process the supervised script itself backgrounds, not just
// the direct child -- the concrete bug class group-based signaling exists
// to prevent (an orphaned process left behind after Stop()).
func TestStop_ProcessGroupKill(t *testing.T) {
	t.Parallel()

	sup := New()
	childPidFile := filepath.Join(t.TempDir(), "childpid")

	// The parent shell backgrounds a `sleep`, records its pid, then
	// ignores SIGTERM itself and busy-loops -- so it survives the initial
	// SIGTERM and requires SIGKILL escalation, while its backgrounded
	// child does NOT ignore SIGTERM (default disposition) and dies
	// straight from the group signal, proving the signal reaches the
	// grandchild independently of whatever the direct child does with
	// its own signal handling.
	script := fmt.Sprintf(`sleep 30 & echo $! > '%s'; trap '' TERM; while true; do :; done`, childPidFile)
	proc := spawnShell(t, sup, script)

	childPID := waitForChildPID(t, childPidFile)
	if !processAlive(childPID) {
		t.Fatalf("child pid %d not alive before Stop()", childPID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := proc.Stop(ctx, 2*time.Second); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if processAlive(childPID) {
		t.Errorf("child pid %d still alive after Stop() -- process-group signal did not reach it, orphan left behind", childPID)
	}
}

// TestStop_ProcessGroupKill_AfterLeaderAlreadyExited proves Stop() still
// signals the process group even when the tracked leader has already
// exited (and been reaped) before Stop is ever called -- the exact bug an
// adversarial review caught and reproduced: a leader that backgrounds a
// descendant and then exits immediately on its own used to leave that
// descendant permanently orphaned, because Stop() returned early ("already
// exited, nothing to do") without ever signaling the group at all. POSIX
// guarantees a process group's ID is not reused for an unrelated process
// while any member remains alive, so signaling -pgid here is always safe
// even though the original leader pid it equals is long gone.
func TestStop_ProcessGroupKill_AfterLeaderAlreadyExited(t *testing.T) {
	t.Parallel()

	sup := New()
	childPidFile := filepath.Join(t.TempDir(), "childpid")

	// The parent backgrounds a `sleep`, records its pid, then exits
	// immediately itself -- unlike TestStop_ProcessGroupKill, the LEADER
	// is gone (and reaped by the background goroutine) long before Stop()
	// is ever called below.
	script := fmt.Sprintf(`sleep 30 & echo $! > '%s'; exit 0`, childPidFile)
	proc := spawnShell(t, sup, script)

	childPID := waitForChildPID(t, childPidFile)
	if !processAlive(childPID) {
		t.Fatalf("child pid %d not alive before leader exit", childPID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Wait for the LEADER itself to be reaped, so doneCh is confirmed
	// already closed before Stop() is called at all -- reproducing the
	// exact ordering the bug depended on.
	if _, err := proc.Wait(ctx); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	if err := proc.Stop(ctx, 2*time.Second); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if processAlive(childPID) {
		t.Errorf("child pid %d still alive after Stop() on an already-exited leader -- orphan left behind", childPID)
	}
}

// TestStop_ProcessGroupKill_LeaderExitsDuringGrace is the regression test
// for the identical hole TestStop_ProcessGroupKill_AfterLeaderAlreadyExited
// covers for the OTHER ordering: this one exercises a COOPERATIVE leader
// (exits promptly on the initial SIGTERM) that has backgrounded a STUBBORN
// descendant (ignores SIGTERM) into the same process group before doing
// so. The leader's own doneCh therefore closes well before grace elapses,
// taking Stop's `case <-p.doneCh:` branch. An earlier version of that
// branch returned immediately there, without ever sweeping the group with
// SIGKILL -- silently orphaning the stubborn descendant forever, since
// nothing else in Stop would ever signal it again. Re-introducing that
// early `return nil` must make this test fail.
//
// The descendant writes its OWN pid file, from inside itself, AFTER its
// own `trap` statement has already run (via its own $$) -- not the leader
// capturing `$!` right after backgrounding it -- so that the file's mere
// existence is proof the TERM-ignoring trap is already installed, exactly
// TestStopProcessGroup_KillsCooperativeAndStubbornDescendants's own
// precedent in cmd/sandbox-agent/processgroup_test.go.
func TestStop_ProcessGroupKill_LeaderExitsDuringGrace(t *testing.T) {
	t.Parallel()

	sup := New()
	stubbornPIDFile := filepath.Join(t.TempDir(), "stubbornpid")

	// sleep 0.05 instead of a tight `while true; do :; done` busy-loop:
	// keeps this test's own CPU footprint low without weakening what it
	// proves -- SIGKILL is unblockable and kills the descendant regardless
	// of what its loop body does between iterations, and the shell's TERM
	// trap (or lack thereof) is checked between commands either way.
	script := fmt.Sprintf(
		`sh -c 'trap "" TERM; echo $$ > %s; while true; do sleep 0.05; done' &
trap "exit 0" TERM
while true; do sleep 0.05; done`,
		stubbornPIDFile,
	)
	proc := spawnShell(t, sup, script)

	stubbornPID := waitForChildPID(t, stubbornPIDFile)
	if !processAlive(stubbornPID) {
		t.Fatalf("stubborn descendant pid %d not alive before Stop()", stubbornPID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Grace is generous and NOT raced against: the point isn't proving
	// Stop() is fast, it's proving the descendant is dead once Stop()
	// returns, regardless of which select branch got there.
	start := time.Now()
	if err := proc.Stop(ctx, 2*time.Second); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	t.Logf("Stop() took %v", time.Since(start))

	// Poll rather than assert once: unlike TestStop_ProcessGroupKill (whose
	// descendant has the DEFAULT SIGTERM disposition and so dies from the
	// very first group signal, minutes before Stop()'s own full grace
	// period elapses) or TestStop_ProcessGroupKill_AfterLeaderAlreadyExited
	// (whose branch always blocks out the full grace period before ever
	// sending SIGKILL), this test's fast path has Stop() return the instant
	// the LEADER's own (already-closed) doneCh is observed -- it does not
	// wait on the descendant at all. The final SIGKILL is issued only
	// microseconds before Stop() returns, so a single immediate
	// processAlive check races the kernel's own signal-delivery/zombie-reap
	// latency: it can observe a zombie (kill(pid,0) still succeeds) rather
	// than an actually-reaped pid. This does not weaken the assertion --
	// without the fix the descendant is never signaled at all and stays
	// alive indefinitely, so it is still alive long past this deadline.
	deadline := time.Now().Add(5 * time.Second) // _test.go is exempt from notimeliteral
	for processAlive(stubbornPID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(stubbornPID) {
		t.Errorf("stubborn descendant pid %d still alive after Stop() -- leader's own exit short-circuited the SIGKILL sweep, orphan left behind", stubbornPID)
	}
}

// waitForChildPID polls childPidFile until it contains a non-empty pid,
// failing the test if it never appears.
func waitForChildPID(t *testing.T, childPidFile string) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(childPidFile)
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

	t.Fatalf("child pid file %s never populated", childPidFile)
	return 0
}

// TestProcess_Exited covers both of Exited's outcomes: false (with a zero
// ExitResult) while the process is still running, and true (with the
// correct ExitResult) once it has exited and been reaped -- proving Exited
// itself never blocks either way.
func TestProcess_Exited(t *testing.T) {
	t.Parallel()

	sup := New()
	proc := spawnShell(t, sup, `trap "exit 0" TERM; while true; do :; done`)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = proc.Stop(ctx, time.Second)
	})

	result, exited := proc.Exited()
	if exited {
		t.Fatalf("Exited() = (%v, true), want (_, false) for a still-running process", result)
	}
	if result != (ExitResult{}) {
		t.Errorf("Exited() result = %+v, want zero ExitResult while not yet exited", result)
	}

	exitedProc := spawnShell(t, sup, "exit 9")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := exitedProc.Wait(ctx); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	result, exited = exitedProc.Exited()
	if !exited {
		t.Fatal("Exited() = (_, false), want (_, true) for an already-exited process")
	}
	if result.ExitCode != 9 {
		t.Errorf("Exited() ExitCode = %d, want 9", result.ExitCode)
	}
}

func TestSupervisor_StopAll(t *testing.T) {
	t.Parallel()

	sup := New()

	cooperativeA := spawnShell(t, sup, `trap "exit 0" TERM; while true; do :; done`)
	cooperativeB := spawnShell(t, sup, `trap "exit 0" TERM; while true; do :; done`)
	stubborn := spawnShell(t, sup, `trap '' TERM; while true; do :; done`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	if err := sup.StopAll(ctx, 300*time.Millisecond); err != nil {
		t.Fatalf("StopAll() error = %v", err)
	}
	elapsed := time.Since(start)

	if elapsed >= 5*time.Second {
		t.Errorf("StopAll() took %v, want bounded well under 5s", elapsed)
	}

	procs := map[string]*Process{
		"cooperativeA": cooperativeA,
		"cooperativeB": cooperativeB,
		"stubborn":     stubborn,
	}
	for name, proc := range procs {
		if processAlive(proc.pgid) {
			t.Errorf("%s (pgid %d) still alive after StopAll()", name, proc.pgid)
		}
	}
}

// TestUnconditionalReaping proves a process that exits naturally is
// reaped by the background goroutine launched at Spawn time, WITHOUT
// anyone ever calling Wait or Stop on it in the meantime: sleeping well
// past its natural exit before the first Wait call means Wait can only
// return near-instantly if the reap goroutine already collected the exit
// on its own.
func TestUnconditionalReaping(t *testing.T) {
	t.Parallel()

	sup := New()
	proc := spawnShell(t, sup, "exit 3")

	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, err := proc.Wait(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Wait() error = %v, want the already-reaped result", err)
	}
	if result.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", result.ExitCode)
	}
	if elapsed >= 50*time.Millisecond {
		t.Errorf("Wait() took %v, want near-instant (doneCh should already be closed by the background reap goroutine)", elapsed)
	}
}
