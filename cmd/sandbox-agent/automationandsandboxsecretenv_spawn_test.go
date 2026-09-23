// This file (automationandsandboxsecretenv_spawn_test.go) is W1's own
// strongest regression test: it drives BOTH real production functions run()
// itself calls -- automationAndSandboxSecretEnv (automationenvvars.go) AND
// opencodeproc.Spawn (spawn.go) -- end to end, against a REAL spawned
// process, rather than a test-reconstructed []string literal handed
// straight to Spawn the way the pre-existing opencodeproc tests
// (TestSpawn_AutomationEnvVarAppendedViaSandboxSecretEnv,
// TestSpawn_SandboxSecretEnvWinsOverAutomationEnvVar) do. Mirrors those
// tests' own fake-script-probe technique exactly, but sources the env
// slice Spawn receives from automationAndSandboxSecretEnv's own real
// output, the exact call run() itself makes (main.go).
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/sandboxagent/opencodeproc"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// TestAutomationAndSandboxSecretEnv_RealSpawn_AutomationVarReachesSpawnedProcess
// proves an automation env var, threaded through the REAL
// automationAndSandboxSecretEnv assembly and then the REAL
// opencodeproc.Spawn, genuinely reaches a spawned process's own
// environment -- the full, real production path run() itself drives, not
// a replica.
func TestAutomationAndSandboxSecretEnv_RealSpawn_AutomationVarReachesSpawnedProcess(t *testing.T) {
	// Not t.Parallel(): t.Setenv forbids combining the two.
	binDir := t.TempDir()
	probeFile := filepath.Join(t.TempDir(), "probe")

	script := "#!/bin/sh\n" +
		`printf '%s\n' "${TARGET_ENV:-ABSENT}" > "$PROBE_FILE"` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("PATH", binDir)
	t.Setenv("PROBE_FILE", probeFile)
	// TARGET_ENV is deliberately left unset on the test process itself --
	// proves this reaches the child via the threaded env, never ambient
	// inheritance.

	sup := supervisor.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sandboxSecretEnv := automationAndSandboxSecretEnv(map[string]string{"TARGET_ENV": "staging"}, nil)
	_, err := opencodeproc.Spawn(ctx, sup, t.TempDir(), nil, sandboxSecretEnv, nil, 5*time.Second, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Spawn() error = nil, want an error (the fake opencode script exits 1 before ever becoming healthy)")
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	if got := strings.TrimSpace(string(got)); got != "staging" {
		t.Errorf("TARGET_ENV as seen by the spawned process = %q, want %q (via automationAndSandboxSecretEnv -> Spawn, run()'s own real path)", got, "staging")
	}
}

// TestAutomationAndSandboxSecretEnv_RealSpawn_CollisionOrderMatchesRecordedThreeWayOrder
// is W1's own full-chain collision proof: a name present in ALL THREE
// sources (automation env var, sandbox secret, provider credential)
// resolves exactly the way spawn.go's own "recorded three-way order"
// doc comment states -- provider credential wins over both, sandbox
// secret wins over automation env var -- driven through the REAL
// automationAndSandboxSecretEnv assembly AND the REAL opencodeproc.Spawn,
// never a hand-built already-in-final-order slice. Deleting main.go's
// own automation-env-var injection line, inverting
// automationAndSandboxSecretEnv's own append order, or inverting
// Spawn's own providerCredentialEnv/sandboxSecretEnv append order would
// each independently fail this test.
func TestAutomationAndSandboxSecretEnv_RealSpawn_CollisionOrderMatchesRecordedThreeWayOrder(t *testing.T) {
	// Not t.Parallel(): t.Setenv forbids combining the two.
	binDir := t.TempDir()
	probeFile := filepath.Join(t.TempDir(), "probe")

	script := "#!/bin/sh\n" +
		`printf '%s\n' "${SHARED_NAME:-ABSENT}" > "$PROBE_FILE"` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("PATH", binDir)
	t.Setenv("PROBE_FILE", probeFile)

	sup := supervisor.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sandboxSecretEnv := automationAndSandboxSecretEnv(
		map[string]string{"SHARED_NAME": "from-automation-env-var"},
		map[string]string{"SHARED_NAME": "from-sandbox-secret"},
	)
	providerCredentialEnv := []string{"SHARED_NAME=from-provider-credential"}

	_, err := opencodeproc.Spawn(ctx, sup, t.TempDir(), providerCredentialEnv, sandboxSecretEnv, nil, 5*time.Second, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Spawn() error = nil, want an error (the fake opencode script exits 1 before ever becoming healthy)")
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	if got := strings.TrimSpace(string(got)); got != "from-provider-credential" {
		t.Errorf("SHARED_NAME as seen by the spawned process = %q, want %q (provider credential outranks both sandbox secret and automation env var)", got, "from-provider-credential")
	}
}
