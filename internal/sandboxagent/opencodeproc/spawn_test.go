package opencodeproc_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/sandboxagent/opencodeproc"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// These tests need only the OpenCode SERVER itself (spawning it, hitting
// /api/health) -- no AI provider call is ever made, so they run
// unconditionally, no skip needed (starting the server costs nothing and
// needs no credentials).

func TestSpawn_RealBinary(t *testing.T) {
	t.Parallel()

	sup := supervisor.New()
	// A generous 150s test-local readiness bound (well above
	// platform.Timeouts.OpenCodeReadinessTimeout's own 30s production
	// default) -- a real dev machine (or a shared, smaller-CPU-count CI
	// runner) running `go test ./...` under -race has many OTHER packages'
	// own test binaries compiling/running concurrently, which was observed
	// to occasionally starve a fresh opencode serve process past even a
	// 60s bound specifically on GitHub Actions' own hosted runners
	// (confirmed via a from-scratch Docker repro matching the runner's
	// exact node/npm versions and architecture: opencode serve became
	// healthy in under 5s with no other load competing for CPU) --
	// production spawns exactly one opencode server per sandbox VM with no
	// such competition.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	// Registered BEFORE the fallible Spawn call below, deliberately: even
	// when Spawn's own readiness wait times out, its own internal
	// sup.Spawn call already registered the OS process with sup the
	// moment it started, well before any readiness check -- so sup still
	// knows about it and can still reap it. t.Fatalf calls
	// runtime.Goexit() immediately; a t.Cleanup registered AFTER an
	// `if err != nil { t.Fatalf(...) }` check would never run at all on
	// that path, leaking a real orphaned OS process across test runs.
	//
	// Known, honest gap -- see internal/adapters/outbound/opencode/
	// helpers_test.go's startServer, the canonical fuller explanation:
	// this t.Cleanup (like every real-binary spawn helper following this
	// same sup.StopAll shape) never runs at all if the TEST BINARY itself
	// is killed abruptly (SIGKILL, Ctrl-C's default SIGINT, `go test
	// -timeout` firing) rather than exiting normally -- not something this
	// function itself can fix.
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = sup.StopAll(stopCtx, 5*time.Second)
	})

	result, err := opencodeproc.Spawn(ctx, sup, t.TempDir(), nil, nil, nil, 150*time.Second, 250*time.Millisecond)
	if err != nil {
		t.Fatalf("Spawn() error = %v, want nil (real opencode binary should be on PATH)", err)
	}

	if !strings.HasPrefix(result.BaseURL, "http://127.0.0.1:") {
		t.Errorf("BaseURL = %q, want an http://127.0.0.1:<port> URL", result.BaseURL)
	}
	if result.Version == "" {
		t.Errorf("Version = %q, want a non-empty version string from the real installed binary", result.Version)
	}
	if result.Process == nil {
		t.Fatal("Process = nil, want a live *supervisor.Process")
	}
	if _, exited := result.Process.Exited(); exited {
		t.Error("Process.Exited() = true immediately after a successful Spawn, want still running")
	}

	resp, err := http.Get(result.BaseURL + "/api/health")
	if err != nil {
		t.Fatalf("GET %s/api/health: %v", result.BaseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/health status = %d, want 200", resp.StatusCode)
	}
}

// TestSpawn_BadBinaryPath verifies a nonexistent binary fails cleanly and
// promptly (bounded, not hanging) -- PATH is overridden to a directory
// containing no "opencode" executable at all, so exec.Command's own
// LookPath resolution fails immediately inside supervisor.Spawn, well
// before Spawn's own readiness-wait loop would ever start.
func TestSpawn_BadBinaryPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	sup := supervisor.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := opencodeproc.Spawn(ctx, sup, t.TempDir(), nil, nil, nil, 30*time.Second, 250*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Spawn() error = nil, want a fail-fast error for a nonexistent opencode binary")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Spawn() took %s to fail, want a fast, bounded failure (LookPath fails immediately)", elapsed)
	}
}

// TestSpawn_EnvExcludesSessionConfig proves the real regression this
// Step's env-leak remediation fixes: the spawned `opencode serve` process
// must NOT inherit NARVI_SESSION_CONFIG (the sandbox's own plaintext
// bearer token), while ordinary process environment (PATH, HOME) it
// genuinely needs to run is left untouched. PATH is overridden (same
// technique as TestSpawn_BadBinaryPath) to a tempdir containing a real,
// executable fake "opencode" script that probes its own environment and
// writes what it finds to PROBE_FILE, then exits 1 immediately -- Spawn's
// own waitHealthy treats a prompt pre-healthy exit as a fast, bounded
// failure (TestSpawn_BadBinaryPath's own precedent), so asserting Spawn
// returns an error here is consistent, not a new pattern.
func TestSpawn_EnvExcludesSessionConfig(t *testing.T) {
	// Not t.Parallel(): t.Setenv forbids combining the two.
	binDir := t.TempDir()
	probeFile := filepath.Join(t.TempDir(), "probe")

	script := "#!/bin/sh\n" +
		`printf '%s\n' "${NARVI_SESSION_CONFIG:-ABSENT}" > "$PROBE_FILE"` + "\n" +
		`if [ -n "$PATH" ]; then printf 'PATH_PRESENT\n' >> "$PROBE_FILE"; else printf 'PATH_ABSENT\n' >> "$PROBE_FILE"; fi` + "\n" +
		`if [ -n "$HOME" ]; then printf 'HOME_PRESENT\n' >> "$PROBE_FILE"; else printf 'HOME_ABSENT\n' >> "$PROBE_FILE"; fi` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("PATH", binDir)
	t.Setenv("PROBE_FILE", probeFile)
	t.Setenv("NARVI_SESSION_CONFIG", "marker-should-not-reach-child")

	sup := supervisor.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := opencodeproc.Spawn(ctx, sup, t.TempDir(), nil, nil, nil, 5*time.Second, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Spawn() error = nil, want an error (the fake opencode script exits 1 before ever becoming healthy)")
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 3 {
		t.Fatalf("probe file = %q, want 3 lines", got)
	}
	if lines[0] != "ABSENT" {
		t.Errorf("NARVI_SESSION_CONFIG as seen by the spawned process = %q, want %q (must not be inherited)", lines[0], "ABSENT")
	}
	if lines[1] != "PATH_PRESENT" {
		t.Errorf("PATH as seen by the spawned process = %q, want it present", lines[1])
	}
	if lines[2] != "HOME_PRESENT" {
		t.Errorf("HOME as seen by the spawned process = %q, want it present", lines[2])
	}
}

// TestSpawn_ProviderCredentialEnvAppended proves §25.1's own
// ("provider credential injection", §25.1/§25.3) providerCredentialEnv
// parameter actually reaches the spawned opencode process's own
// environment -- the ACTUAL injection point this Step exists to build.
// Mirrors TestSpawn_EnvExcludesSessionConfig's own fake-script-probe
// technique exactly, extended to also probe an env var this test supplies
// via providerCredentialEnv.
func TestSpawn_ProviderCredentialEnvAppended(t *testing.T) {
	// Not t.Parallel(): t.Setenv forbids combining the two.
	binDir := t.TempDir()
	probeFile := filepath.Join(t.TempDir(), "probe")

	script := "#!/bin/sh\n" +
		`printf '%s\n' "${ANTHROPIC_API_KEY:-ABSENT}" > "$PROBE_FILE"` + "\n" +
		`printf '%s\n' "${GOOGLE_API_KEY:-ABSENT}" >> "$PROBE_FILE"` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("PATH", binDir)
	t.Setenv("PROBE_FILE", probeFile)

	sup := supervisor.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	providerCredentialEnv := []string{"ANTHROPIC_API_KEY=sk-resolved-anthropic-key"}
	_, err := opencodeproc.Spawn(ctx, sup, t.TempDir(), providerCredentialEnv, nil, nil, 5*time.Second, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Spawn() error = nil, want an error (the fake opencode script exits 1 before ever becoming healthy)")
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 2 {
		t.Fatalf("probe file = %q, want 2 lines", got)
	}
	if lines[0] != "sk-resolved-anthropic-key" {
		t.Errorf("ANTHROPIC_API_KEY as seen by the spawned process = %q, want %q", lines[0], "sk-resolved-anthropic-key")
	}
	if lines[1] != "ABSENT" {
		t.Errorf("GOOGLE_API_KEY as seen by the spawned process = %q, want %q (never set when not passed in providerCredentialEnv)", lines[1], "ABSENT")
	}
}

// TestSpawn_NilProviderCredentialEnv_UnchangedBehavior proves nil (the
// overwhelming common case -- no provider credential configured for this
// session at any scope) behaves EXACTLY like this function did before
// §25.1: PATH/HOME still present, NARVI_SESSION_CONFIG still absent --
// this parameter's own absence changes nothing.
func TestSpawn_NilProviderCredentialEnv_UnchangedBehavior(t *testing.T) {
	// Not t.Parallel(): t.Setenv forbids combining the two.
	binDir := t.TempDir()
	probeFile := filepath.Join(t.TempDir(), "probe")

	script := "#!/bin/sh\n" +
		`printf '%s\n' "${NARVI_SESSION_CONFIG:-ABSENT}" > "$PROBE_FILE"` + "\n" +
		`if [ -n "$PATH" ]; then printf 'PATH_PRESENT\n' >> "$PROBE_FILE"; else printf 'PATH_ABSENT\n' >> "$PROBE_FILE"; fi` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("PATH", binDir)
	t.Setenv("PROBE_FILE", probeFile)
	t.Setenv("NARVI_SESSION_CONFIG", "marker-should-not-reach-child")

	sup := supervisor.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := opencodeproc.Spawn(ctx, sup, t.TempDir(), nil, nil, nil, 5*time.Second, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Spawn() error = nil, want an error (the fake opencode script exits 1 before ever becoming healthy)")
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 2 {
		t.Fatalf("probe file = %q, want 2 lines", got)
	}
	if lines[0] != "ABSENT" {
		t.Errorf("NARVI_SESSION_CONFIG as seen by the spawned process = %q, want %q", lines[0], "ABSENT")
	}
	if lines[1] != "PATH_PRESENT" {
		t.Errorf("PATH as seen by the spawned process = %q, want it present", lines[1])
	}
}

// TestSpawn_SandboxSecretEnvAppended is the direct, real-spawn proof of
// an adversarial-review HIGH fix (threading, §27.1): the
// sandboxSecretEnv parameter -- NOT sandbox-agent's own os.Setenv'd
// process environment (the pre-fix mechanism) -- actually reaches the
// spawned opencode process's own environment. Mirrors
// TestSpawn_ProviderCredentialEnvAppended's own fake-script-probe
// technique exactly.
func TestSpawn_SandboxSecretEnvAppended(t *testing.T) {
	// Not t.Parallel(): t.Setenv forbids combining the two.
	binDir := t.TempDir()
	probeFile := filepath.Join(t.TempDir(), "probe")

	script := "#!/bin/sh\n" +
		`printf '%s\n' "${MY_SANDBOX_SECRET:-ABSENT}" > "$PROBE_FILE"` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("PATH", binDir)
	t.Setenv("PROBE_FILE", probeFile)
	// Proves this parameter is genuinely threaded, not read from ambient
	// process env: MY_SANDBOX_SECRET is deliberately left UNSET on the
	// test process itself -- only sandboxSecretEnv (below) carries it.

	sup := supervisor.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sandboxSecretEnv := []string{"MY_SANDBOX_SECRET=resolved-secret-value"}
	_, err := opencodeproc.Spawn(ctx, sup, t.TempDir(), nil, sandboxSecretEnv, nil, 5*time.Second, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Spawn() error = nil, want an error (the fake opencode script exits 1 before ever becoming healthy)")
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	if got := strings.TrimSpace(string(got)); got != "resolved-secret-value" {
		t.Errorf("MY_SANDBOX_SECRET as seen by the spawned process = %q, want %q (threaded via sandboxSecretEnv, never os.Setenv on sandbox-agent's own process)", got, "resolved-secret-value")
	}
}

// TestSpawn_ProviderCredentialEnvWinsOverSandboxSecretEnv proves §27.1's
// own explicit ordering ("appended before providerCredentialEnv") is
// honored: when the SAME name appears in both slices (never possible in
// production -- internal/domain/sandboxsecret.ValidateName rejects every
// name providercredential.AllEnvVarNames already owns -- but Env's own
// documented "last duplicate key wins" semantics make this the correct,
// pinned observable behavior regardless), providerCredentialEnv's own
// entry is the one the spawned process actually sees.
func TestSpawn_ProviderCredentialEnvWinsOverSandboxSecretEnv(t *testing.T) {
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

	providerCredentialEnv := []string{"SHARED_NAME=from-provider-credential"}
	sandboxSecretEnv := []string{"SHARED_NAME=from-sandbox-secret"}
	_, err := opencodeproc.Spawn(ctx, sup, t.TempDir(), providerCredentialEnv, sandboxSecretEnv, nil, 5*time.Second, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Spawn() error = nil, want an error (the fake opencode script exits 1 before ever becoming healthy)")
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	if got := strings.TrimSpace(string(got)); got != "from-provider-credential" {
		t.Errorf("SHARED_NAME as seen by the spawned process = %q, want %q (providerCredentialEnv appended last, per §27.1)", got, "from-provider-credential")
	}
}

// TestSpawn_AutomationEnvVarAppendedViaSandboxSecretEnv proves §8 item
// 4's own ("automation env vars reach the process, not just the prompt")
// threading mechanism: an automation env var, folded by the caller
// (cmd/sandbox-agent's own run()) into the SAME sandboxSecretEnv
// parameter sandbox_secrets already use (no new opencodeproc.Spawn
// parameter -- a third source on the existing path, not a new mechanism),
// actually reaches the spawned opencode process's own environment.
// Mirrors TestSpawn_SandboxSecretEnvAppended's own fake-script-probe
// technique exactly -- from Spawn's own point of view this parameter's
// entries are indistinguishable by source, which is exactly the point.
func TestSpawn_AutomationEnvVarAppendedViaSandboxSecretEnv(t *testing.T) {
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
	// Proves this parameter is genuinely threaded, not read from ambient
	// process env: TARGET_ENV is deliberately left UNSET on the test
	// process itself.

	sup := supervisor.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Simulates exactly what main.go's own run() produces: automation env
	// vars prepended to the FRONT of sandboxSecretEnv (cmd/sandbox-agent's
	// own automationEnvVarSpawnEnv output, folded in ahead of
	// sandboxSecretSpawnEnv's own).
	sandboxSecretEnv := []string{"TARGET_ENV=staging"}
	_, err := opencodeproc.Spawn(ctx, sup, t.TempDir(), nil, sandboxSecretEnv, nil, 5*time.Second, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Spawn() error = nil, want an error (the fake opencode script exits 1 before ever becoming healthy)")
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	if got := strings.TrimSpace(string(got)); got != "staging" {
		t.Errorf("TARGET_ENV as seen by the spawned process = %q, want %q (threaded via sandboxSecretEnv, exactly like a sandbox secret)", got, "staging")
	}
}

// TestSpawn_SandboxSecretEnvWinsOverAutomationEnvVar proves the FIRST
// recorded collision pairing (spawn.go's own doc comment, "The recorded
// three-way order, and why"): when an automation env var and a sandbox
// secret share a name, main.go's own append order (automation env vars
// prepended to the FRONT of sandboxSecretEnv, sandbox secrets appended
// AFTER them onto the SAME slice) means the sandbox secret -- "a secret
// the operator configured" -- is what the spawned process actually sees,
// never the automation's own "plain config a maintainer typed" value.
func TestSpawn_SandboxSecretEnvWinsOverAutomationEnvVar(t *testing.T) {
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

	// Exactly main.go's own construction order: automation env var first
	// (least-trusted), sandbox secret appended second (more-trusted).
	sandboxSecretEnv := []string{"SHARED_NAME=from-automation-env-var", "SHARED_NAME=from-sandbox-secret"}
	_, err := opencodeproc.Spawn(ctx, sup, t.TempDir(), nil, sandboxSecretEnv, nil, 5*time.Second, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Spawn() error = nil, want an error (the fake opencode script exits 1 before ever becoming healthy)")
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	if got := strings.TrimSpace(string(got)); got != "from-sandbox-secret" {
		t.Errorf("SHARED_NAME as seen by the spawned process = %q, want %q (sandbox secret appended after automation env var, so it wins)", got, "from-sandbox-secret")
	}
}

// TestSpawn_ProviderCredentialEnvWinsOverAutomationEnvVar proves the
// SECOND recorded collision pairing: an automation env var loses to a
// provider credential too, even though this specific collision is
// structurally unreachable in production (internal/domain/automation.
// ValidateEnvVars already rejects, at automation-creation time, any name
// providercredential.AllEnvVarNames owns -- reusing
// sandboxsecret.ValidateNotReserved, the SAME disjoint-name rule
// TestSpawn_ProviderCredentialEnvWinsOverSandboxSecretEnv's own doc
// comment already notes for sandbox secrets). Pinned as the correct
// observable behavior regardless, mirroring that test's own reasoning
// exactly.
func TestSpawn_ProviderCredentialEnvWinsOverAutomationEnvVar(t *testing.T) {
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

	providerCredentialEnv := []string{"SHARED_NAME=from-provider-credential"}
	sandboxSecretEnv := []string{"SHARED_NAME=from-automation-env-var"}
	_, err := opencodeproc.Spawn(ctx, sup, t.TempDir(), providerCredentialEnv, sandboxSecretEnv, nil, 5*time.Second, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Spawn() error = nil, want an error (the fake opencode script exits 1 before ever becoming healthy)")
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	if got := strings.TrimSpace(string(got)); got != "from-provider-credential" {
		t.Errorf("SHARED_NAME as seen by the spawned process = %q, want %q (providerCredentialEnv appended last, wins over an automation env var too)", got, "from-provider-credential")
	}
}

// A self-uid Credential's harmlessness is proven one layer down, by
// supervisor's own TestSpawn_CredentialSelfUID_Succeeds, and it is proven
// better there: that test spawns /bin/sh rather than the real opencode
// binary, so it runs on every platform and in CI, needs no port, and
// cannot be destabilised by anything else running beside it.
//
// A test here that spawned `opencode serve` for the same property was
// removed rather than repaired. Its own doc comment conceded it did not
// prove the parameter reaches supervisor.Spec.Credential -- the cross-uid
// test below is what proves that -- so it was paying a real binary spawn
// to re-establish something already established. And it was not free: it
// made a SEVENTH concurrent `opencode serve` in this package, and serve
// picks its port through a free-port helper that closes its listener
// before the child binds. That window is a known, filed flake, and this
// test lost the race in CI while passing locally. Adding contention to a
// known race in order to re-prove a covered property is a bad trade in
// both directions.

func TestSpawn_RuntimeCredentialDropsCannotReadAnotherUIDsFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("requires linux (this test asserts a real kernel-enforced file-permission denial across a uid boundary); running on %s", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		t.Skip("requires running as root (euid 0): dropping a spawned child to an UNPRIVILEGED uid/gid requires CAP_SETUID/CAP_SETGID, which a non-root test process does not have")
	}

	// binDir (holding the fake "opencode" script), workDir (the spawned
	// process's own cwd), and probeDir (where it writes its findings) are
	// all this TEST's OWN scaffolding, not the secret being protected --
	// they must be reachable by the unprivileged runtimeCredential uid.
	// Deliberately os.MkdirTemp (creates DIRECTLY under os.TempDir(),
	// e.g. /tmp -- conventionally mode 1777, world-traversable) rather
	// than t.TempDir() (which nests every per-test directory one level
	// deeper, under an ADDITIONAL shared, mode-0700, root-owned directory
	// unique to this test -- confirmed live: using t.TempDir() here first
	// made this test fail with "fork/exec ...: permission denied" even
	// though binDir ITSELF was correctly chmod'd 0755, because the
	// unprivileged child could not even traverse INTO that shared 0700
	// parent to reach it). Every directory permission check below is
	// real kernel enforcement, not simulated -- this test's own
	// scaffolding bugs are exactly the same class of denial it exists to
	// prove for the real secret. secretPath's own directory is
	// deliberately left at t.TempDir()'s default (root-only, nested
	// under that same 0700 shared parent) -- matching
	// internal/sandboxagent/credentials/cache.go's own real
	// Dir-0700/file-0600 shape (if anything, a STRICTER, doubly-enforced
	// version of it) -- so the file's own 0600 mode (and/or its parents)
	// is what blocks the read, exactly like the real credential cache.
	binDir, err := os.MkdirTemp("", "narvi-runtimecred-bin")
	if err != nil {
		t.Fatalf("MkdirTemp(binDir): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(binDir) })
	if err := os.Chmod(binDir, 0o755); err != nil {
		t.Fatalf("Chmod(binDir): %v", err)
	}
	workDir, err := os.MkdirTemp("", "narvi-runtimecred-work")
	if err != nil {
		t.Fatalf("MkdirTemp(workDir): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })
	if err := os.Chmod(workDir, 0o755); err != nil {
		t.Fatalf("Chmod(workDir): %v", err)
	}
	probeDir, err := os.MkdirTemp("", "narvi-runtimecred-probe")
	if err != nil {
		t.Fatalf("MkdirTemp(probeDir): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(probeDir) })
	if err := os.Chmod(probeDir, 0o777); err != nil {
		t.Fatalf("Chmod(probeDir): %v", err)
	}
	probeFile := filepath.Join(probeDir, "probe")

	secretPath := filepath.Join(t.TempDir(), "narvi-credentials-like-secret.json")
	if err := os.WriteFile(secretPath, []byte(`{"password":"should-be-unreadable"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// /bin/cat (absolute path, never bare "cat"): PATH is overridden below
	// to binDir ONLY, so a bare "cat" would fail with "not found" for a
	// reason having nothing to do with runtimeCredential -- confirmed
	// live (this test originally used a bare "cat" and passed
	// VACUOUSLY, for exactly that wrong reason, both with and without
	// the Credential threaded through -- see this test's own mutation
	// note in the Step's report, since a "test that passes either way"
	// is worse than no test at all).
	script := "#!/bin/sh\n" +
		`/bin/cat ` + secretPath + ` > "$PROBE_FILE" 2>>"$PROBE_FILE"` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("PATH", binDir)
	t.Setenv("PROBE_FILE", probeFile)

	sup := supervisor.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cred := &syscall.Credential{Uid: 65534, Gid: 65534}
	_, err = opencodeproc.Spawn(ctx, sup, workDir, nil, nil, cred, 5*time.Second, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Spawn() error = nil, want an error (the fake opencode script exits 1 before ever becoming healthy)")
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	if strings.Contains(string(got), "should-be-unreadable") {
		t.Fatalf("the spawned process (runtimeCredential uid 65534) read a 0600 file owned by this root test process successfully (probe=%q); want a kernel permission error, proving runtimeCredential reached the spawned process", got)
	}
}
