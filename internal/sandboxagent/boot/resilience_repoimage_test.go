package boot_test

// Two new §9.3-class resilience scenarios (§19.4/§19.9's own
// "stale-image boot" and "non-idempotent-setup boot" additions), exercised
// through boot.RunBoot itself -- the same real orchestration point
// cmd/sandbox-agent's own runBootSequence calls (hook policy +
// workspaceMoved + output-tail capture + telemetry, all wired together
// against a REAL git repo and a REAL /narvi/image-manifest.json-shaped
// manifest), not a bare EvaluateHook table-test case. This is where the
// actual logic under test lives (internal/sandboxagent/boot), and
// cmd/sandbox-agent's own thin main.go wiring around LoadImageManifest/
// ComputeWorkspaceMoved is already covered by that package's own existing
// bootsequence_cleanbuild_integration_test.go precedent -- these two
// scenarios do not need a second, duplicate proof through main.go's own
// os.Executable()-sensitive subprocess machinery.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// TestResilienceScenario_StaleImageBoot_WorkspaceMovedFiresSetupReruns
// proves §19.4's own headline contract end to end: a repo_image boot whose
// baked manifest names a DIFFERENT built SHA than the repo's own real,
// checked-out HEAD -- workspaceMoved fires, and setup.sh reruns,
// non-fatally, through the real hook-policy/output-capture machinery
// RunBoot orchestrates.
func TestResilienceScenario_StaleImageBoot_WorkspaceMovedFiresSetupReruns(t *testing.T) {
	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	mkdirAll(t, repoDir)
	initGitRepo(t, repoDir) // fingerprint_test.go's own helper: one real commit

	rerunMarker := filepath.Join(workspaceDir, "setup-rerun-marker")
	writeScript(t, filepath.Join(repoDir, "setup.sh"), "touch "+rerunMarker)

	currentSHA := gitRevParseHEAD(t, repoDir)

	manifest := boot.ImageManifest{
		Fingerprint:   "fp-stale-image",
		BuiltRepoShas: map[string]string{"repo1": "sha-different-from-current-" + currentSHA[:8]},
	}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, map[string]string{"repo1": currentSHA})
	if !workspaceMoved["repo1"] {
		t.Fatalf("precondition failed: ComputeWorkspaceMoved()[repo1] = false, want true (manifest SHA deliberately differs from current)")
	}

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved,
		nil, nil, noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil (a moved workspace's setup.sh rerun must be non-fatal)", err)
	}

	assertFileExists(t, rerunMarker)
}

// TestResilienceScenario_StaleImageBoot_WorkspaceUnmoved_SetupSkipped proves
// the companion "zero regression" half: an UNMOVED workspace (manifest SHA
// matches current HEAD exactly) still skips setup.sh entirely under
// repo_image, exactly as before §19.4's amendment.
func TestResilienceScenario_StaleImageBoot_WorkspaceUnmoved_SetupSkipped(t *testing.T) {
	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	mkdirAll(t, repoDir)
	initGitRepo(t, repoDir)

	rerunMarker := filepath.Join(workspaceDir, "setup-should-not-run-marker")
	writeScript(t, filepath.Join(repoDir, "setup.sh"), "touch "+rerunMarker)

	currentSHA := gitRevParseHEAD(t, repoDir)

	manifest := boot.ImageManifest{
		Fingerprint:   "fp-unmoved-image",
		BuiltRepoShas: map[string]string{"repo1": currentSHA}, // matches exactly
	}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, map[string]string{"repo1": currentSHA})
	if workspaceMoved["repo1"] {
		t.Fatalf("precondition failed: ComputeWorkspaceMoved()[repo1] = true, want false (manifest SHA deliberately matches current)")
	}

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved,
		nil, nil, noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil", err)
	}

	assertFileAbsent(t, rerunMarker)
}

// TestResilienceScenario_NonIdempotentSetupBoot_NonFatalFailure_VisibleInOutputTail
// proves §19.4's own non-fatal-severity guarantee for the OTHER real-world
// failure mode: a setup.sh that is NOT safely re-runnable (fails on a
// rerun against an already-warm workspace) must still let the boot
// succeed overall -- with the failure's own diagnostic output actually
// captured and surfaced (§19.5(a)), not silently swallowed the way a
// non-fatal hook failure used to be before this Step ("undiagnosable by
// construction").
func TestResilienceScenario_NonIdempotentSetupBoot_NonFatalFailure_VisibleInOutputTail(t *testing.T) {
	// Not t.Parallel(): swaps slog's global default logger, mirroring
	// hooks_output_test.go's own identical precedent.
	handler := &recordingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	mkdirAll(t, repoDir)
	initGitRepo(t, repoDir)

	writeScript(t, filepath.Join(repoDir, "setup.sh"),
		"echo 'installing dependency that is broken on rerun' >&2\nexit 1")

	currentSHA := gitRevParseHEAD(t, repoDir)
	manifest := boot.ImageManifest{
		Fingerprint:   "fp-non-idempotent-setup",
		BuiltRepoShas: map[string]string{"repo1": "sha-different-from-current-" + currentSHA[:8]},
	}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, map[string]string{"repo1": currentSHA})

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved,
		nil, nil, noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil (a non-idempotent setup.sh's rerun failure must be non-fatal -- boot still succeeds)", err)
	}

	rawTail, ok := handler.findAttr("output_tail")
	if !ok {
		t.Fatal("no Warn log line carried an output_tail attribute -- the failure is undiagnosable, exactly the gap §19.5(a) exists to close")
	}
	lines, ok := rawTail.Any().([]string)
	if !ok {
		t.Fatalf("output_tail attribute = %T, want []string", rawTail.Any())
	}
	found := false
	for _, line := range lines {
		if line == "installing dependency that is broken on rerun" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("output_tail = %v, want it to contain the setup.sh failure's own diagnostic output", lines)
	}
}

// TestResilienceScenario_RepoAbsentFromWorkspaceMoved_SetupStillReruns proves
// workspaceMovedFor's own load-bearing "assume moved" default (hooks.go) for
// a repo genuinely ABSENT from the workspaceMoved map -- not a hand-built map
// with a key deleted, but the real production mechanism the doc comment
// names: ComputeWorkspaceMoved only ever emits an entry for a repo present
// in currentSHAs (boot.DiscoverRepoSHAs's own return shape), and
// DiscoverRepoSHAs omits a repo entirely when `git rev-parse HEAD` fails for
// it (the OTHER of the doc comment's two named causes is the repo's
// directory never existing at all under workspaceDir -- not usable here,
// since with no directory there would be no setup.sh on disk to prove
// reran).
//
// repo-no-sha's directory here is a real `git init` with zero commits: .git
// exists (so DiscoverRepoSHAs's own os.Stat(".git") gate passes and it
// actually shells out to git), but `git rev-parse HEAD` on a repo with no
// commits yet fails ("ambiguous argument 'HEAD'") -- a realistic
// production shape (e.g. a session repo cloned/initialized but not yet
// committed to) that DiscoverRepoSHAs documents itself as silently omitting.
// The resulting currentSHAs therefore has no "repo-no-sha" key at all, so
// ComputeWorkspaceMoved (which only ranges over currentSHAs) never emits a
// workspaceMoved["repo-no-sha"] entry either -- proving the omission
// genuinely propagates end to end, not merely asserted by construction.
//
// Under BootModeRepoImage, EvaluateHook's HookSetup ShouldRun IS
// workspaceMoved for that repo (hook.go) -- so if workspaceMovedFor's
// missing-key default were ever flipped from true to false, this repo's
// setup.sh would be silently skipped instead of rerun. This test is the one
// place in the suite that actually takes that missing-key branch under
// repo_image; every other repo_image test (both above in this file, and
// TestRunHooks_LogsSetupRerunDecision in hooks_test.go) passes a map that
// already contains an explicit entry for the repo under test.
func TestResilienceScenario_RepoAbsentFromWorkspaceMoved_SetupStillReruns(t *testing.T) {
	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo-no-sha")
	mkdirAll(t, repoDir)
	runGit(t, repoDir, "init") // .git exists, but zero commits: rev-parse HEAD fails

	rerunMarker := filepath.Join(workspaceDir, "setup-rerun-marker-no-sha")
	writeScript(t, filepath.Join(repoDir, "setup.sh"), "touch "+rerunMarker)

	// Step 171 (§30.5): DiscoverRepoSHAs now only reads a repo whose agent
	// git-dir has already been seeded -- seed repo-no-sha's here too, so
	// the omission this test proves is still genuinely `git rev-parse
	// HEAD` failing (a zero-commit repo), not merely "never seeded".
	// gitdir.Seed itself succeeds fine against a zero-commit repo: `git
	// init` still writes a well-formed symbolic-ref HEAD
	// ("ref: refs/heads/<branch>\n") even with no commits yet, which is
	// exactly the shape SyncHeadIn accepts (see that function's own doc
	// comment) -- the FAILURE this test cares about is `rev-parse HEAD`
	// itself finding no commit to resolve the ref to, which still happens
	// identically through the agent-owned git-dir.
	gitDirRoot := t.TempDir()
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}
	layout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	seedSup := supervisor.New()
	if err := gitdir.Seed(context.Background(), seedSup, layout.Repo("repo-no-sha"), "https://example.invalid/repo-no-sha.git", nil, 5*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed() error = %v", err)
	}

	// The real production path (cmd/sandbox-agent/main.go's own
	// runBootSequence): discover current SHAs over the whole workspace, then
	// compute workspaceMoved from that set -- never a hand-constructed map.
	currentSHAs := boot.DiscoverRepoSHAs(context.Background(), seedSup, layout, nil, 5*time.Second, 5*time.Second)
	if _, ok := currentSHAs["repo-no-sha"]; ok {
		t.Fatalf("precondition failed: DiscoverRepoSHAs()[repo-no-sha] present, want absent (rev-parse HEAD should fail on a zero-commit repo); got %v", currentSHAs)
	}

	manifest := boot.ImageManifest{
		Fingerprint:   "fp-repo-absent-from-sha-set",
		BuiltRepoShas: map[string]string{"repo-no-sha": "some-built-sha"},
	}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, currentSHAs)
	if _, ok := workspaceMoved["repo-no-sha"]; ok {
		t.Fatalf("precondition failed: ComputeWorkspaceMoved()[repo-no-sha] present, want absent (no current SHA to compare against); got %v", workspaceMoved)
	}

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo-no-sha", Primary: true}}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved,
		nil, nil, noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil (a repo absent from workspaceMoved must still rerun setup.sh non-fatally, per the safe default)", err)
	}

	assertFileExists(t, rerunMarker)
}

// gitRevParseHEAD returns dir's own checked-out HEAD SHA via a real,
// direct `git -C dir rev-parse HEAD` call -- deliberately NOT routed
// through boot.DiscoverRepoSHAs (Step 171/§30.5 made that function require
// an already-seeded agent git-dir, which this helper's own callers should
// not need to set up just to read back a SHA they themselves just
// committed).
func gitRevParseHEAD(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git -C %s rev-parse HEAD: %v", dir, err)
	}
	return strings.TrimSpace(string(out))
}

// countingFailThenSucceedScript returns a setup.sh body that counts its own
// invocations via an on-disk counter file (the only way a plain shell
// script can distinguish "this is my first run" from "this is my retry"
// without a test-only hook into runSetupRerunLadder itself): it fails on
// invocation 1 and succeeds (touching successMarker) on invocation 2
// onward.
func countingFailThenSucceedScript(counterPath, successMarker string) string {
	return fmt.Sprintf(`
count=0
if [ -f %[1]q ]; then count=$(cat %[1]q); fi
count=$((count+1))
echo "$count" > %[1]q
if [ "$count" -eq 1 ]; then
	echo "attempt $count: simulated transient failure" >&2
	exit 1
fi
touch %[2]q
`, counterPath, successMarker)
}

// countingAlwaysFailScript is countingFailThenSucceedScript's own
// companion: counts its own invocations identically, but ALWAYS fails,
// echoing the attempt number to stderr so a test can tell which attempt's
// own output_tail it is looking at.
func countingAlwaysFailScript(counterPath string) string {
	return fmt.Sprintf(`
count=0
if [ -f %[1]q ]; then count=$(cat %[1]q); fi
count=$((count+1))
echo "$count" > %[1]q
echo "attempt $count: still broken" >&2
exit 1
`, counterPath)
}

// TestResilienceScenario_FullSetupRetry_FirstFailsSecondSucceeds proves
// §19.6's own required retry (the manifest-digest bullet: "retry the
// install on transient failure, then warn -- never fail the boot on it"):
// a full-setup.sh rerun whose FIRST attempt fails is retried EXACTLY ONCE,
// and when that retry succeeds the boot ends up with the dependency
// actually installed -- not the pre-fix single-attempt warn-and-continue
// outcome that would have left successMarker never created. The attempts
// counter proves the retry fired ONCE, never zero times (no retry at all,
// the pre-fix bug this test guards against) and never more than once (an
// unbounded loop, which §19.6 explicitly rules out: "not an unbounded
// backoff loop").
func TestResilienceScenario_FullSetupRetry_FirstFailsSecondSucceeds(t *testing.T) {
	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	mkdirAll(t, repoDir)
	initGitRepo(t, repoDir)

	attemptsCounter := filepath.Join(workspaceDir, "attempts")
	successMarker := filepath.Join(workspaceDir, "success-marker")
	writeScript(t, filepath.Join(repoDir, "setup.sh"), countingFailThenSucceedScript(attemptsCounter, successMarker))

	currentSHA := gitRevParseHEAD(t, repoDir)
	manifest := boot.ImageManifest{
		Fingerprint:   "fp-retry-then-succeed",
		BuiltRepoShas: map[string]string{"repo1": "sha-different-from-current-" + currentSHA[:8]},
	}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, map[string]string{"repo1": currentSHA})

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	// ladder: nil -- ladderFor's own missing-key default (DependencySkip:
	// Ineligible, DeltaEligible: false) sends this straight to the
	// full-setup.sh floor, the ONE tier this test exercises.
	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved,
		nil, nil, noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, 10*time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil", err)
	}

	assertFileExists(t, successMarker)

	raw, readErr := os.ReadFile(attemptsCounter)
	if readErr != nil {
		t.Fatalf("read attempts counter: %v", readErr)
	}
	if got := strings.TrimSpace(string(raw)); got != "2" {
		t.Errorf("setup.sh ran %s time(s), want exactly 2 (one failed attempt + one successful retry)", got)
	}
}

// TestResilienceScenario_FullSetupRetry_BothAttemptsFail proves the other
// half of §19.6's own retry rule: a full-setup.sh rerun whose retry ALSO
// fails must still warn and continue -- the boot itself never fails on it
// -- and the retry stops at exactly one extra attempt, never an unbounded
// loop.
func TestResilienceScenario_FullSetupRetry_BothAttemptsFail(t *testing.T) {
	// Not t.Parallel(): swaps slog's global default logger, mirroring
	// TestResilienceScenario_NonIdempotentSetupBoot_NonFatalFailure_VisibleInOutputTail's
	// own identical precedent.
	handler := &recordingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	mkdirAll(t, repoDir)
	initGitRepo(t, repoDir)

	attemptsCounter := filepath.Join(workspaceDir, "attempts")
	writeScript(t, filepath.Join(repoDir, "setup.sh"), countingAlwaysFailScript(attemptsCounter))

	currentSHA := gitRevParseHEAD(t, repoDir)
	manifest := boot.ImageManifest{
		Fingerprint:   "fp-retry-still-fails",
		BuiltRepoShas: map[string]string{"repo1": "sha-different-from-current-" + currentSHA[:8]},
	}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, map[string]string{"repo1": currentSHA})

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved,
		nil, nil, noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, 10*time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil (a retried failure must still be non-fatal -- boot still succeeds)", err)
	}

	raw, readErr := os.ReadFile(attemptsCounter)
	if readErr != nil {
		t.Fatalf("read attempts counter: %v", readErr)
	}
	if got := strings.TrimSpace(string(raw)); got != "2" {
		t.Errorf("setup.sh ran %s time(s), want exactly 2 (one attempt + exactly one retry, never an unbounded loop)", got)
	}

	rawTail, ok := handler.findAttr("output_tail")
	if !ok {
		t.Fatal("no Warn log line carried an output_tail attribute for the retried failure")
	}
	lines, ok := rawTail.Any().([]string)
	if !ok {
		t.Fatalf("output_tail attribute = %T, want []string", rawTail.Any())
	}
	found := false
	for _, line := range lines {
		if line == "attempt 2: still broken" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("output_tail = %v, want it to contain the SECOND (retried) attempt's own diagnostic output -- proves findAttr's own most-recent-first scan actually surfaced the retry's outcome, not just the first attempt's", lines)
	}
}
