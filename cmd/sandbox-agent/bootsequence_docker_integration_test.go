//go:build integration

// This file proves §27.5's own §27.5 in-sandbox wiring end to end,
// through the REAL runBootSequence (not internal/sandboxagent/boot.
// RunDocker called in isolation, already covered directly by
// internal/sandboxagent/boot/docker_test.go): a session whose
// SessionConfig.Docker is true reaches a real dockerd spawn attempt
// before RunBoot's own per-repo loop; a session whose Docker is false
// (or has no SessionConfig at all) never spawns anything Docker-shaped,
// regardless of what happens to be (or not be) on PATH.
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/services"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// dockerMarkerEnvVar is the ONE env var this test's own fake dockerd
// script reads to learn where to leave proof it genuinely ran --
// deliberately NOT boot.DefaultDockerSocketPath itself: that real
// default path ("/var/run/docker.sock") may already exist on a
// developer/CI machine that happens to run a real Docker daemon (e.g.
// OrbStack/Docker Desktop), which would make RunDocker's own readiness
// poll succeed instantly regardless of whether THIS test's fake script
// ever ran at all -- an environment-dependent false pass this marker
// avoids entirely by using a path only this test's own TempDir owns.
const dockerMarkerEnvVar = "NARVI_TEST_FAKE_DOCKERD_MARKER"

// writeFakeDockerdOnPath prepends a directory containing an executable
// named exactly "dockerd" (boot.DefaultDockerdBinary) to the test
// process's own PATH, mirroring internal/sandboxagent/boot/docker_test.
// go's own fakeDockerdScript precedent but reachable by bare-name PATH
// lookup (supervisor.Spec.Path is "dockerd", not an absolute path, for
// every REAL caller -- boot.DefaultDockerdBinary's own doc comment).
// The script touches whatever path it finds in dockerMarkerEnvVar (set
// per-call via runBootSequence's own secretEnv parameter, which
// RunDocker's real call site threads straight into the spawned
// process's environment) and then sleeps, so it never exits on its own
// during the test.
func writeFakeDockerdOnPath(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "dockerd")
	body := "#!/bin/sh\n" +
		"if [ -n \"$" + dockerMarkerEnvVar + "\" ]; then touch \"$" + dockerMarkerEnvVar + "\"; fi\n" +
		"sleep 30\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dockerd: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// dockerTestSessionConfig builds a minimal, repo-free SessionConfig (Go
// literal, never JSON-decoded, so the wire schema's own "repos minItems:
// 1" constraint -- enforced only by SessionConfig.UnmarshalJSON, never by
// the Go type itself -- does not apply here) so this test can reach
// runBootSequence's own Docker-gate call site without needing a real git
// server: gitclone.CloneAll's own documented behavior on an empty repo
// list is a correct no-op (RunBoot's own doc comment: "boot.RunBoot's own
// documented, correct no-op on an empty repo list").
func dockerTestSessionConfig(docker bool) *sessionconfig.SessionConfig {
	return &sessionconfig.SessionConfig{
		BootMode:          sessionconfig.SessionConfigBootModeFresh,
		ControlPlaneWsUrl: "wss://unused.invalid/ws",
		Gen:               1,
		SandboxId:         "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a",
		SandboxToken:      "test-sandbox-token",
		SessionId:         "docker-boot-sequence-test-session",
		Docker:            docker,
	}
}

// TestRunBootSequence_DockerRequired_SpawnsDockerdBeforeRunBoot proves a
// docker-required session reaches a real dockerd spawn attempt: the fake
// "dockerd" on PATH touches the marker file runBootSequence's own
// secretEnv parameter told it about, proving RunDocker's own sup.Spawn
// call genuinely fired -- independent of runBootSequence's own overall
// outcome (which itself still depends on the REAL, hardcoded default
// docker socket path this test does not control, and so is not asserted
// on here either way).
func TestRunBootSequence_DockerRequired_SpawnsDockerdBeforeRunBoot(t *testing.T) {
	writeFakeDockerdOnPath(t)
	markerPath := filepath.Join(t.TempDir(), "dockerd-ran")

	timeouts := platform.DefaultTimeouts()
	timeouts.DockerReadinessTimeout = 500 * time.Millisecond

	sup := supervisor.New()
	// The fake dockerd script this test spawns via writeFakeDockerdOnPath
	// ends in `sleep 30` (it never exits on its own, standing in for a
	// real long-lived daemon) -- without this cleanup it survives past
	// the test, exactly the stranded-process class this repo has been
	// bitten by before. Mirrors internal/sandboxagent/boot/runboot_test.
	// go's own identical inline t.Cleanup shape (same package family,
	// same sup.StopAll(ctx, time.Second) call).
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sup.StopAll(ctx, time.Second)
	})
	cfg := boot.Config{
		BootMode:           sandboxboot.BootModeFresh,
		WorkspaceDir:       t.TempDir(),
		CredentialCacheDir: t.TempDir(),
		SessionConfig:      dockerTestSessionConfig(true),
		// Step 171 (§30.5): see bootsequence_cleanbuild_integration_test.go's
		// identical comment -- gitclone.CloneAll's own real lchown needs a
		// self-uid/gid Credential to run unprivileged.
		RuntimeUID: uint32(os.Getuid()),
		RuntimeGID: uint32(os.Getgid()),
	}

	ctx, cancel := context.WithTimeout(context.Background(), bootSequenceTestTimeout)
	defer cancel()

	noopProgress := func(services.BootProgressEvent) {}
	noopGitSync := func(string, string, string) {}
	noopFetchTiming := func(string, float64, bool) {}
	noopCheckoutTiming := func(string, float64, bool) {}
	noopHookTiming := func(string, string, string, bool, bool, float64) {}
	secretEnv := []string{dockerMarkerEnvVar + "=" + markerPath}

	bootErr := runBootSequence(ctx, sup, cfg, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: cfg.WorkspaceDir}, nil, timeouts, secretEnv, nil, noopProgress, noopGitSync, noopFetchTiming, noopCheckoutTiming, noopHookTiming)
	t.Logf("runBootSequence() returned: %v (outcome depends on this machine's own real docker socket state; not asserted on)", bootErr)

	// 10s, not a tighter budget: this test proved flaky under a full,
	// heavily-parallel `go test -tags=integration -race ./...` run (many
	// concurrent testcontainers-backed packages competing for process-
	// scheduling and disk I/O can delay even a near-instant fake script
	// from being scheduled/writing its marker) despite passing reliably
	// in isolation -- mirrors internal/sandboxagent/boot/docker_test.go's
	// own identical fix for the same class of flake.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(markerPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake dockerd's own marker file was never created -- RunDocker was not genuinely invoked for a Docker-required session")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunBootSequence_DockerFalse_NeverSpawnsDockerd proves the mirror
// image: a Docker-false (the ordinary, default) session never invokes
// RunDocker at all -- proven by the fake "dockerd" on PATH never
// touching the marker file runBootSequence's own secretEnv still told it
// about (if the process had ever been spawned, it would have).
func TestRunBootSequence_DockerFalse_NeverSpawnsDockerd(t *testing.T) {
	writeFakeDockerdOnPath(t)
	markerPath := filepath.Join(t.TempDir(), "dockerd-ran")

	sup := supervisor.New()
	cfg := boot.Config{
		BootMode:           sandboxboot.BootModeFresh,
		WorkspaceDir:       t.TempDir(),
		CredentialCacheDir: t.TempDir(),
		SessionConfig:      dockerTestSessionConfig(false),
		// Step 171 (§30.5): see bootsequence_cleanbuild_integration_test.go's
		// identical comment -- gitclone.CloneAll's own real lchown needs a
		// self-uid/gid Credential to run unprivileged.
		RuntimeUID: uint32(os.Getuid()),
		RuntimeGID: uint32(os.Getgid()),
	}

	ctx, cancel := context.WithTimeout(context.Background(), bootSequenceTestTimeout)
	defer cancel()

	noopProgress := func(services.BootProgressEvent) {}
	noopGitSync := func(string, string, string) {}
	noopFetchTiming := func(string, float64, bool) {}
	noopCheckoutTiming := func(string, float64, bool) {}
	noopHookTiming := func(string, string, string, bool, bool, float64) {}
	secretEnv := []string{dockerMarkerEnvVar + "=" + markerPath}

	if err := runBootSequence(ctx, sup, cfg, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: cfg.WorkspaceDir}, nil, platform.DefaultTimeouts(), secretEnv, nil, noopProgress, noopGitSync, noopFetchTiming, noopCheckoutTiming, noopHookTiming); err != nil {
		t.Fatalf("runBootSequence() error = %v, want nil (Docker=false must never even attempt to spawn dockerd)", err)
	}

	if _, err := os.Stat(markerPath); err == nil {
		t.Error("fake dockerd's own marker file exists -- it must never have been spawned for a Docker=false session")
	}
}
