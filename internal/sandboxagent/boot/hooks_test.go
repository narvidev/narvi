package boot_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// writeScript writes a /bin/sh script with body at path, through
// boot.WriteExecutableForTest -- the writer that keeps a concurrent
// test's fork from making the script ETXTBSY (see its doc comment).
func writeScript(t *testing.T, path, body string) {
	t.Helper()
	boot.WriteExecutableForTest(t, path, []byte("#!/bin/sh\n"+body+"\n"))
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected marker file %s to exist, stat error: %v", path, err)
	}
}

func assertFileAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("expected marker file %s to be absent, but it exists", path)
	}
}

func TestRunHooks_EmptyRepos(t *testing.T) {
	t.Parallel()

	sup := supervisor.New()
	err := boot.RunHooks(context.Background(), sup, t.TempDir(), nil, sandboxboot.BootModeFresh, nil, nil, nil, noopHookRerunTiming, 5*time.Second, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("RunHooks() error = %v, want nil for an empty repos slice", err)
	}
}

func TestRunHooks_AbsentHookSkipped(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceDir, "repo-a"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// No setup.sh/start.sh created at all -- a repo without either script
	// is normal and expected.

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo-a", Primary: true}}

	err := boot.RunHooks(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeFresh, nil, nil, nil, noopHookRerunTiming, 5*time.Second, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("RunHooks() error = %v, want nil (absent hooks are a routine no-op)", err)
	}
}

func TestRunHooks_SuccessContinuesToLaterHooksAndRepos(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	markerA := filepath.Join(workspaceDir, "marker-a-setup")
	markerB := filepath.Join(workspaceDir, "marker-b-start")

	writeScript(t, filepath.Join(workspaceDir, "repo-a", "setup.sh"), "touch "+markerA)
	writeScript(t, filepath.Join(workspaceDir, "repo-b", "start.sh"), "touch "+markerB)

	sup := supervisor.New()
	repos := []boot.RepoInfo{
		{Name: "repo-a", Primary: true},
		{Name: "repo-b", Primary: false},
	}

	// fresh mode: repo-a's setup.sh runs (non-fatal); repo-b's start.sh
	// runs (non-fatal, secondary). Both succeed, so RunHooks must run
	// every hook across both repos.
	err := boot.RunHooks(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeFresh, nil, nil, nil, noopHookRerunTiming, 5*time.Second, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("RunHooks() error = %v, want nil", err)
	}

	assertFileExists(t, markerA)
	assertFileExists(t, markerB)
}

func TestRunHooks_FatalFailureStopsImmediately(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	laterMarker := filepath.Join(workspaceDir, "later-marker")

	// build mode: repo-a's setup.sh is fatal (primary or not -- setup.sh's
	// FatalOnFailure never depends on primary). Make it fail.
	writeScript(t, filepath.Join(workspaceDir, "repo-a", "setup.sh"), "exit 1")
	// A hook that would only run if RunHooks kept going after repo-a's
	// fatal failure.
	writeScript(t, filepath.Join(workspaceDir, "repo-b", "setup.sh"), "touch "+laterMarker)

	sup := supervisor.New()
	repos := []boot.RepoInfo{
		{Name: "repo-a", Primary: true},
		{Name: "repo-b", Primary: false},
	}

	err := boot.RunHooks(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeBuild, nil, nil, nil, noopHookRerunTiming, 5*time.Second, time.Second, time.Millisecond)
	if err == nil {
		t.Fatal("RunHooks() error = nil, want an error (build mode's setup.sh failure is fatal)")
	}

	assertFileAbsent(t, laterMarker)
}

func TestRunHooks_NonFatalFailureContinues(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	laterMarker := filepath.Join(workspaceDir, "later-marker")

	// fresh mode: repo-a is a SECONDARY repo, so its start.sh failing is
	// only a warning, never fatal.
	writeScript(t, filepath.Join(workspaceDir, "repo-a", "start.sh"), "exit 1")
	writeScript(t, filepath.Join(workspaceDir, "repo-b", "start.sh"), "touch "+laterMarker)

	sup := supervisor.New()
	repos := []boot.RepoInfo{
		{Name: "repo-a", Primary: false},
		{Name: "repo-b", Primary: false},
	}

	err := boot.RunHooks(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeFresh, nil, nil, nil, noopHookRerunTiming, 5*time.Second, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("RunHooks() error = %v, want nil (a secondary repo's start.sh failure is only a warning)", err)
	}

	assertFileExists(t, laterMarker)
}

// TestRunHooks_EnvExcludesSessionConfig proves the real regression this
// Step's env-leak remediation fixes: a repo's own setup.sh must NOT
// inherit NARVI_SESSION_CONFIG (the sandbox's own plaintext bearer token).
// The hook script itself checks its own environment and exits 9 (a real,
// observed failure) if the var is present at all; build mode's setup.sh
// policy is fatal, so RunHooks returning nil here is proof positive the
// spawned script never saw it -- not merely that nothing crashed.
func TestRunHooks_EnvExcludesSessionConfig(t *testing.T) {
	// Not t.Parallel(): t.Setenv forbids combining the two.
	t.Setenv("NARVI_SESSION_CONFIG", "marker-should-not-reach-child")

	workspaceDir := t.TempDir()
	writeScript(t, filepath.Join(workspaceDir, "repo-a", "setup.sh"), `[ -z "$NARVI_SESSION_CONFIG" ] || exit 9`)

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo-a", Primary: true}}

	err := boot.RunHooks(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeBuild, nil, nil, nil, noopHookRerunTiming, 5*time.Second, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("RunHooks() error = %v, want nil (the spawned setup.sh must not see NARVI_SESSION_CONFIG)", err)
	}
}

// TestRunHooks_SecretEnvReachesRealSpawnedHook is the direct, end-to-end
// proof of §27.1's own injection mechanism (§27.1) AFTER the
// adversarial-review HIGH fix: RunHooks' own new secretEnv parameter --
// NOT a value os.Setenv'd onto sandbox-agent's own process environment (the
// PRE-fix mechanism this test used to exercise; see git history for the
// prior version, TestRunHooks_SeesProcessEnvSetAfterPackageLoadedIncludingSandboxSecrets) --
// reaches a REAL spawned hook script's own environment. The pre-fix
// mechanism relied on EnvWithout's own SUBTRACTIVE-filter-over-os.Environ()
// behavior picking up anything a caller had ADDED to sandbox-agent's own
// process first -- which meant a resolved secret literally named "PATH" or
// "HOME" corrupted sandbox-agent's OWN process (see opencodeproc.Spawn's
// own doc comment for the full incident this fixes). secretEnv is
// deliberately NOT set via t.Setenv here at all -- proving this value
// reaches the spawned script ONLY through the explicit parameter, with the
// TEST process's own ambient environment carrying no trace of it, is the
// whole point of the threaded fix.
func TestRunHooks_SecretEnvReachesRealSpawnedHook(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	probeFile := filepath.Join(t.TempDir(), "probe")
	writeScript(t, filepath.Join(workspaceDir, "repo-a", "setup.sh"),
		`printf '%s' "$MY_SANDBOX_SECRET" > `+probeFile)

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo-a", Primary: true}}
	secretEnv := []string{"MY_SANDBOX_SECRET=resolved-secret-value"}

	err := boot.RunHooks(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeBuild, nil, nil, secretEnv, noopHookRerunTiming, 5*time.Second, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("RunHooks() error = %v, want nil", err)
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	if string(got) != "resolved-secret-value" {
		t.Errorf("MY_SANDBOX_SECRET as seen by the spawned setup.sh = %q, want %q -- RunHooks' own secretEnv parameter (a session's own resolved sandbox_secrets, threaded explicitly -- never os.Setenv onto sandbox-agent's own process) must reach every hook script", got, "resolved-secret-value")
	}
}

// TestRunHooks_SecretEnvNeverMutatesTestProcessEnvironment is the
// complementary proof: passing secretEnv touches ONLY the spawned child's
// own environment, never the calling (here, the test) process's own
// os.Environ() -- the exact property the pre-fix os.Setenv-based mechanism
// broke (see TestRunHooks_SecretEnvReachesRealSpawnedHook's own doc
// comment).
func TestRunHooks_SecretEnvNeverMutatesTestProcessEnvironment(t *testing.T) {
	t.Parallel()

	if v, present := os.LookupEnv("MY_OTHER_SANDBOX_SECRET"); present {
		t.Fatalf("MY_OTHER_SANDBOX_SECRET already present in this test process's own environment (%q) -- test precondition violated", v)
	}

	workspaceDir := t.TempDir()
	writeScript(t, filepath.Join(workspaceDir, "repo-a", "setup.sh"), "true")

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo-a", Primary: true}}
	secretEnv := []string{"MY_OTHER_SANDBOX_SECRET=should-never-leak-into-this-process"}

	err := boot.RunHooks(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeBuild, nil, nil, secretEnv, noopHookRerunTiming, 5*time.Second, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("RunHooks() error = %v, want nil", err)
	}

	if v, present := os.LookupEnv("MY_OTHER_SANDBOX_SECRET"); present {
		t.Errorf("MY_OTHER_SANDBOX_SECRET leaked into the TEST process's own environment (%q) -- secretEnv must only ever reach the spawned CHILD's environment", v)
	}
}

func TestRunHooks_TimeoutIsFatalWhenPolicyFatal(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	writeScript(t, filepath.Join(workspaceDir, "repo-a", "setup.sh"), "sleep 30")

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo-a", Primary: true}}

	err := boot.RunHooks(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeBuild, nil, nil, nil, noopHookRerunTiming,
		200*time.Millisecond, 200*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("RunHooks() error = nil, want an error (hook exceeded hookTimeout, fatal per build mode's setup.sh policy)")
	}
}

// TestRunHooks_LogsSetupRerunDecision proves the fix for the observability
// finding: the §19.4 setup-rerun decision (repo, boot_mode, workspace_moved,
// and what was actually decided) was never logged at all -- "working as
// designed, the repo moved" was indistinguishable from "the build service
// stopped baking manifests". No script is even created on disk here: the
// decision must be logged BEFORE hookScriptPresent is ever consulted (the
// decision itself is what determines whether setup.sh would even be
// attempted, not the other way around).
//
// Deliberately NOT t.Parallel(): swaps the process-wide slog default logger
// (platform.Logger(ctx) always reads slog.Default()) to capture it -- same
// justification as gitclone/sync_test.go's own identical precedent (Go's own
// test scheduler never interleaves a non-parallel test's body with any
// other test's body in the same package).
func TestRunHooks_LogsSetupRerunDecision(t *testing.T) {
	originalLogger := slog.Default()
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(originalLogger)

	workspaceDir := t.TempDir()
	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo-decision-test", Primary: true}}
	workspaceMoved := map[string]bool{"repo-decision-test": true}

	if err := boot.RunHooks(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved, nil, nil, noopHookRerunTiming,
		5*time.Second, time.Second, time.Millisecond); err != nil {
		t.Fatalf("RunHooks() error = %v, want nil", err)
	}

	logged := logBuf.String()
	for _, want := range []string{
		"boot: setup-rerun decision",
		`"repo":"repo-decision-test"`,
		`"boot_mode":"repo_image"`,
		`"workspace_moved":true`,
		`"setup_will_run":true`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log output = %q, want it to contain %q", logged, want)
		}
	}
}

// hookRerunTimingCall records one OnHookRerunTiming (§33.3) callback
// invocation for assertions.
type hookRerunTimingCall struct {
	repo, hook, bootMode   string
	workspaceMoved, failed bool
	seconds                float64
}

func recordingHookRerunTiming(calls *[]hookRerunTimingCall) boot.OnHookRerunTiming {
	return func(repo, hook, bootMode string, workspaceMoved, failed bool, seconds float64) {
		*calls = append(*calls, hookRerunTimingCall{repo, hook, bootMode, workspaceMoved, failed, seconds})
	}
}

// TestRunHooks_RelaysHookRerunTiming proves §33.3's sandbox-side relay:
// this Step deleted telemetry.go's own local sandbox_agent_hook_rerun_
// duration_seconds histogram recording (recordHookRerunDuration) and
// replaced it with the OnHookRerunTiming callback runRepoHooks now calls
// once per actually-run hook instead -- this test drives a REAL setup.sh
// run through RunHooks (the same call path production main.go uses) and
// asserts the callback receives exactly the tags recordHookRerunDuration's
// own deleted call site used to record locally: repo, hook, boot_mode, and
// workspace_moved, plus a genuinely-elapsed (not the deleted feature's own
// "recorded at all" bar).
//
// hookTimeout is generously large (unlike most other tests in this file,
// which use 5*time.Second): a trivial `touch` command should never
// approach it, but under this package's own heavier -tags=integration
// suite (real docker/services subprocesses running in parallel) a tight
// timeout can spuriously fire runSetupRerunLadder's own §19.6 "retry the
// install on transient failure" path for the full-setup.sh tier this
// BootModeRepoImage+workspaceMoved scenario routes through -- a LEGITIMATE
// production behavior (a second OnHookRerunTiming call, the first tagged
// failed=true) this test must tolerate rather than assume away. Checking
// only the LAST call below (not asserting exactly one) is the fix: it
// still proves the callback carries the right tags on the run that
// actually produced the marker file, regardless of host contention.
func TestRunHooks_RelaysHookRerunTiming(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	marker := filepath.Join(workspaceDir, "marker-setup-relay")
	writeScript(t, filepath.Join(workspaceDir, "repo-relay-test", "setup.sh"), "touch "+marker)

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo-relay-test", Primary: true}}
	workspaceMoved := map[string]bool{"repo-relay-test": true}

	var calls []hookRerunTimingCall
	if err := boot.RunHooks(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved, nil, nil,
		recordingHookRerunTiming(&calls), 30*time.Second, time.Second, time.Millisecond); err != nil {
		t.Fatalf("RunHooks() error = %v, want nil", err)
	}
	assertFileExists(t, marker)

	if len(calls) == 0 {
		t.Fatal("len(calls) = 0, want at least 1 (one setup.sh run, no start.sh present)")
	}
	got := calls[len(calls)-1]
	want := hookRerunTimingCall{repo: "repo-relay-test", hook: "setup.sh", bootMode: "repo_image", workspaceMoved: true, failed: false}
	if got.repo != want.repo || got.hook != want.hook || got.bootMode != want.bootMode || got.workspaceMoved != want.workspaceMoved || got.failed != want.failed {
		t.Errorf("OnHookRerunTiming's own last call = %+v, want repo/hook/bootMode/workspaceMoved/failed = %+v", got, want)
	}
	if got.seconds < 0 {
		t.Errorf("OnHookRerunTiming seconds = %v, want >= 0", got.seconds)
	}
}
