package boot_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/services"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// eventCollector records every boot_progress event RunDocker reports,
// mirroring internal/sandboxagent/services' own run_test.go precedent
// exactly (same shape, same reasoning: RunDocker may report from a
// context the test needs to observe safely).
type eventCollector struct {
	mu     sync.Mutex
	events []services.BootProgressEvent
}

func (c *eventCollector) report(e services.BootProgressEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *eventCollector) all() []services.BootProgressEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]services.BootProgressEvent, len(c.events))
	copy(out, c.events)
	return out
}

// fakeDockerdScript writes a small, real, executable shell script standing
// in for dockerd -- no real Docker runtime is reachable in this test
// environment, mirroring internal/adapters/outbound/modal's own "fake
// httptest.Server, not real Modal API docs" precedent, applied here to a
// fake local process instead of a fake HTTP server. The script sleeps
// delaySeconds, then (if createSocket) touches socketPath -- simulating
// dockerd's own real readiness signal (RunDocker's own doc comment: "its
// socket file appearing") -- then sleeps long enough for the test to
// observe it, unless exitCode is non-zero, in which case it exits
// immediately instead (never creating the socket, simulating a crash
// before readiness). It is written through boot.WriteExecutableForTest,
// so a concurrent test's fork cannot leave it ETXTBSY for RunDocker.
func fakeDockerdScript(t *testing.T, delaySeconds float64, socketPath string, createSocket bool, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-dockerd.sh")

	var body string
	switch {
	case exitCode != 0:
		body = fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)
	case createSocket:
		body = fmt.Sprintf("#!/bin/sh\nsleep %v\ntouch '%s'\nsleep 30\n", delaySeconds, socketPath)
	default:
		body = fmt.Sprintf("#!/bin/sh\nsleep %v\nsleep 30\n", delaySeconds)
	}

	boot.WriteExecutableForTest(t, scriptPath, []byte(body))
	return scriptPath
}

func stopAllOnCleanup(t *testing.T, sup *supervisor.Supervisor) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sup.StopAll(ctx, time.Second)
	})
}

// TestRunDocker_ReadySucceeds proves the happy path: dockerd (the fake
// script) creates its own socket file, RunDocker returns nil, and the
// reporter observes exactly [PhaseStarting, PhaseReady] for
// "dockerd" -- the SAME boot_progress event vocabulary
// internal/sandboxagent/services already reports through for every
// OTHER supervised process/service (this file's own top doc comment).
func TestRunDocker_ReadySucceeds(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "docker.sock")
	dockerd := fakeDockerdScript(t, 0.05, socketPath, true, 0)

	sup := supervisor.New()
	stopAllOnCleanup(t, sup)
	collector := &eventCollector{}

	err := boot.RunDocker(context.Background(), sup, dockerd, socketPath, nil, collector.report,
		5*time.Second, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("RunDocker() error = %v, want nil", err)
	}

	events := collector.all()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (Starting, Ready): %+v", len(events), events)
	}
	if events[0].ServiceName != "dockerd" || events[0].Phase != services.PhaseStarting {
		t.Errorf("events[0] = %+v, want ServiceName=dockerd Phase=Starting", events[0])
	}
	if events[1].ServiceName != "dockerd" || events[1].Phase != services.PhaseReady {
		t.Errorf("events[1] = %+v, want ServiceName=dockerd Phase=Ready", events[1])
	}
}

// TestRunDocker_TimeoutIfSocketNeverAppears proves a dockerd that never
// creates its own socket is a FATAL, real error (§27.5 brief: "there is
// no lesser criticality... a session that asked for Docker and silently
// got none is worse than a session that fails to boot loudly") -- never
// a silent, logged-only warning the way a secondary services.yml entry's
// own timeout is.
func TestRunDocker_TimeoutIfSocketNeverAppears(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "docker.sock")
	// createSocket=false: the fake dockerd simply never creates it.
	dockerd := fakeDockerdScript(t, 0, socketPath, false, 0)

	sup := supervisor.New()
	stopAllOnCleanup(t, sup)
	collector := &eventCollector{}

	err := boot.RunDocker(context.Background(), sup, dockerd, socketPath, nil, collector.report,
		200*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("RunDocker() error = nil, want a timeout error")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("RunDocker() error = %v, want it to mention dockerd not becoming ready", err)
	}

	events := collector.all()
	if len(events) != 2 || events[1].Phase != services.PhaseTimeout {
		t.Fatalf("events = %+v, want [Starting, Timeout]", events)
	}
}

// TestRunDocker_CrashBeforeReadyIsFatal proves a dockerd that exits
// (crashes) before ever creating its socket is likewise a real, fatal
// error -- not merely absorbed as a timeout.
func TestRunDocker_CrashBeforeReadyIsFatal(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "docker.sock")
	dockerd := fakeDockerdScript(t, 0, socketPath, false, 7) // exits 7 immediately

	sup := supervisor.New()
	stopAllOnCleanup(t, sup)
	collector := &eventCollector{}

	// 5s, not services_test.go's own more typical 2s: this specific test
	// proved flaky under a full, heavily-parallel `go test -race ./...`
	// run (the reap goroutine that populates proc.Exited() can be
	// scheduling-delayed under enough concurrent OS-process contention
	// across the whole suite) even though it passed reliably every time
	// in isolation -- a larger budget costs nothing on the fast, common
	// path (the fake dockerd here exits near-instantly) and removes the
	// flake under load.
	err := boot.RunDocker(context.Background(), sup, dockerd, socketPath, nil, collector.report,
		5*time.Second, 20*time.Millisecond)
	if err == nil {
		t.Fatal("RunDocker() error = nil, want an error for a dockerd that exited before becoming ready")
	}
	if !strings.Contains(err.Error(), "exited with code 7") {
		t.Errorf("RunDocker() error = %v, want it to mention exit code 7", err)
	}

	events := collector.all()
	if len(events) != 2 || events[1].Phase != services.PhaseFailed {
		t.Fatalf("events = %+v, want [Starting, Failed]", events)
	}
	if events[1].Err == nil {
		t.Error("PhaseFailed event has nil Err, want non-nil")
	}
}

// TestRunDocker_SpawnFailureReportsFailedAndReturnsError proves a
// dockerd binary that cannot even be spawned (path does not exist) is
// reported as PhaseFailed and returned as a real error -- never a
// silent PhaseStarting-only sequence.
func TestRunDocker_SpawnFailureReportsFailedAndReturnsError(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "docker.sock")
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist-dockerd")

	sup := supervisor.New()
	stopAllOnCleanup(t, sup)
	collector := &eventCollector{}

	err := boot.RunDocker(context.Background(), sup, nonexistent, socketPath, nil, collector.report,
		2*time.Second, 20*time.Millisecond)
	if err == nil {
		t.Fatal("RunDocker() error = nil, want an error for a nonexistent dockerd binary")
	}

	events := collector.all()
	if len(events) != 2 || events[1].Phase != services.PhaseFailed {
		t.Fatalf("events = %+v, want [Starting, Failed]", events)
	}
}
