package gitclone_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/gitstate"
	"github.com/narvidev/narvi/internal/sandboxagent/credentials"
	"github.com/narvidev/narvi/internal/sandboxagent/gitclone"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// These tests exercise SyncAll against a REAL, already-existing git
// workspace (never cloned by SyncAll itself -- see its own doc comment: it
// reconciles a workspace that already exists on disk, exactly like a
// BootModeRepoImage/BootModeSnapshotRestore boot would find one baked into
// an image or restored from a snapshot). initRepo/runGit/currentBranch/
// exitCode are clone_test.go's own helpers, reused here unchanged (same
// package, same test binary).

const testSyncStepTimeout = 10 * time.Second

// testFetchStepTimeout bounds the new boot-time fetch step's own git
// subprocesses (ls-remote/fetch) in these tests -- a real, but ALWAYS
// LOCAL-transport (never actually crossing a network), git operation
// against either a genuine local "origin" fixture (see initRepoWithOrigin,
// below) or -- for every pre-existing test in this file that predates this
// Step and configures no "origin" remote at all -- a fast, real git
// failure ("origin" does not exist), never a hang. 10s (matching
// testSyncStepTimeout) is generous for either real outcome.
const testFetchStepTimeout = 10 * time.Second

// gitSyncEvent records one onGitSync callback invocation for assertions.
type gitSyncEvent struct {
	repoName, status, branch string
}

func recordingOnGitSync(events *[]gitSyncEvent) gitclone.OnGitSync {
	return func(repoName, status, branch string) {
		*events = append(*events, gitSyncEvent{repoName, status, branch})
	}
}

// noopGitFetchTiming/noopGitCheckoutTiming are §33.3's OnGitFetchTiming/
// OnGitCheckoutTiming callbacks for every test below that does not care to
// observe them -- mirroring internal/sandboxagent/boot/runboot_test.go's
// own noopReporter precedent exactly.
func noopGitFetchTiming(_ string, _ float64, _ bool)    {}
func noopGitCheckoutTiming(_ string, _ float64, _ bool) {}

// gitFetchTimingEvent/gitCheckoutTimingEvent record one OnGitFetchTiming/
// OnGitCheckoutTiming callback invocation for assertions -- mirroring
// gitSyncEvent/recordingOnGitSync above exactly.
type gitFetchTimingEvent struct {
	repo     string
	seconds  float64
	degraded bool
}

func recordingOnGitFetchTiming(events *[]gitFetchTimingEvent) gitclone.OnGitFetchTiming {
	return func(repo string, seconds float64, degraded bool) {
		*events = append(*events, gitFetchTimingEvent{repo, seconds, degraded})
	}
}

type gitCheckoutTimingEvent struct {
	repo    string
	seconds float64
	failed  bool
}

func recordingOnGitCheckoutTiming(events *[]gitCheckoutTimingEvent) gitclone.OnGitCheckoutTiming {
	return func(repo string, seconds float64, failed bool) {
		*events = append(*events, gitCheckoutTimingEvent{repo, seconds, failed})
	}
}

// TestSyncAll_CleanTree_CreatesSessionBranchFromHead covers §3.4's "checkout
// session branch (create from base if absent)" for a repo with NO explicit
// branch (repos[].branch == nil): the generated "narvi/<sessionID>" branch
// does not exist yet, so SyncAll creates it from HEAD -- no stash/pop
// phase at all for a clean tree.
func TestSyncAll_CleanTree_CreatesSessionBranchFromHead(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir) // branch "main", one commit

	sessionID := "11111111-1111-1111-1111-111111111111"
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"},
	}

	var events []gitSyncEvent
	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, sessionID,
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, recordingOnGitSync(&events), noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}

	got := results[0]
	if got.Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", got.Err)
	}
	if got.State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready", got.State)
	}
	if !got.Primary {
		t.Error("results[0].Primary = false, want true (position 0)")
	}

	wantBranch := "narvi/" + sessionID
	if got.Branch != wantBranch {
		t.Errorf("results[0].Branch = %q, want %q", got.Branch, wantBranch)
	}
	if head := currentBranch(t, repoDir); head != wantBranch {
		t.Errorf("checked-out branch = %q, want %q", head, wantBranch)
	}

	if len(events) != 1 || events[0].status != "checkout" || events[0].repoName != "repo1" || events[0].branch != wantBranch {
		t.Errorf("events = %#v, want exactly one checkout event for repo1/%s", events, wantBranch)
	}
}

// TestSyncAll_DirtyTree_StashCheckoutPop_PreservesEditsByteForByte is §9.3
// scenario #11's own core assertion at the smallest possible scope: a
// dirty working tree survives a full stash -> checkout -> pop cycle with
// zero lost user edits, byte-for-byte, and fires all three onGitSync
// phases in order.
func TestSyncAll_DirtyTree_StashCheckoutPop_PreservesEditsByteForByte(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir)

	dirtyContent := "uncommitted user edit, must survive\n"
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte(dirtyContent), 0o644); err != nil {
		t.Fatalf("write dirty edit: %v", err)
	}

	sessionID := "22222222-2222-2222-2222-222222222222"
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"},
	}

	var events []gitSyncEvent
	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, sessionID,
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, recordingOnGitSync(&events), noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil", err)
	}

	got := results[0]
	if got.Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", got.Err)
	}
	if got.State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready", got.State)
	}
	if gitstate.RequiresStashRecovery(got.State) {
		t.Error("RequiresStashRecovery(results[0].State) = true, want false on the happy path")
	}

	data, err := os.ReadFile(filepath.Join(repoDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md after sync: %v", err)
	}
	if string(data) != dirtyContent {
		t.Errorf("README.md content = %q, want %q (dirty edit must survive byte-for-byte)", data, dirtyContent)
	}

	wantStatuses := []string{"stash", "checkout", "pop"}
	if len(events) != len(wantStatuses) {
		t.Fatalf("events = %#v, want exactly %v (in order)", events, wantStatuses)
	}
	for i, wantStatus := range wantStatuses {
		if events[i].status != wantStatus {
			t.Errorf("events[%d].status = %q, want %q", i, events[i].status, wantStatus)
		}
		if events[i].repoName != "repo1" {
			t.Errorf("events[%d].repoName = %q, want repo1", i, events[i].repoName)
		}
	}
}

// TestResilienceScenario11_DirtyWorkingTree_RelaunchWithDifferentBranch_ZeroLostEdits
// proves §9.3 resilience scenario #11 end to end, at the sandbox-agent
// level, in the exact shape that scenario names: "Dirty working tree at
// relaunch -> stash -> checkout session branch -> pop; zero lost user
// edits." This is the scenario §3.4 exists to make real -- a repo with
// REAL uncommitted changes reconciles against a session branch that
// ALREADY EXISTS but is DIFFERENT from whatever is currently checked out
// (simulating a BootModeRepoImage/BootModeSnapshotRestore relaunch whose
// session names a different branch than the one baked into/restored in
// this already-existing workspace). Confirms all three of this scenario's
// own guarantees: the dirty edits survive stashed-then-popped, byte-
// identical; the correct (different, existing) branch ends up checked
// out; and a GitSync event fires for each of the three phases with the
// correct repo/status/branch fields.
func TestResilienceScenario11_DirtyWorkingTree_RelaunchWithDifferentBranch_ZeroLostEdits(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir) // checked out on "main"

	// The session's own target branch already exists (as if created by an
	// earlier boot of this same session) -- distinct from "main", the
	// branch currently checked out in this already-existing workspace.
	targetBranch := "session-target-branch"
	runGit(t, repoDir, "branch", targetBranch)

	dirtyContent := "user's real uncommitted edit -- must survive relaunch\n"
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte(dirtyContent), 0o644); err != nil {
		t.Fatalf("write dirty edit: %v", err)
	}

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git", Branch: &targetBranch},
	}

	var events []gitSyncEvent
	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "resilience-session-11",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, recordingOnGitSync(&events), noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil", err)
	}

	got := results[0]
	if got.Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", got.Err)
	}
	if got.State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready", got.State)
	}
	if gitstate.RequiresStashRecovery(got.State) {
		t.Error("RequiresStashRecovery(results[0].State) = true, want false -- zero lost edits means a clean recovery, not a P0")
	}
	if got.Branch != targetBranch {
		t.Errorf("results[0].Branch = %q, want %q", got.Branch, targetBranch)
	}

	if head := currentBranch(t, repoDir); head != targetBranch {
		t.Errorf("checked-out branch = %q, want %q (the DIFFERENT session target branch)", head, targetBranch)
	}

	data, err := os.ReadFile(filepath.Join(repoDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md after relaunch: %v", err)
	}
	if string(data) != dirtyContent {
		t.Errorf("README.md content = %q, want %q (zero lost user edits, §9.3 scenario #11)", data, dirtyContent)
	}

	wantEvents := []gitSyncEvent{
		{repoName: "repo1", status: "stash", branch: targetBranch},
		{repoName: "repo1", status: "checkout", branch: targetBranch},
		{repoName: "repo1", status: "pop", branch: targetBranch},
	}
	if len(events) != len(wantEvents) {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	for i, want := range wantEvents {
		if events[i] != want {
			t.Errorf("events[%d] = %#v, want %#v", i, events[i], want)
		}
	}
}

// TestSyncAll_UntrackedFileOnly_StashCheckoutPop_PreservesEditsByteForByte
// covers a dirty tree whose ONLY change is a brand-new UNTRACKED file --
// one of the most realistic forms of session dirtiness (e.g. an agent's
// own uncommitted new file from a prior turn) -- and is the regression
// test for a genuine gap: gitStatusDirty (`git status --porcelain`)
// reports an untracked file as dirty, but a plain `git stash push` (no
// --include-untracked) has nothing to stash in that case and exits 0
// without creating a stash entry, so the later unconditional `git stash
// pop` then failed with "no stash entries found" -- a spurious fatal sync
// failure and a P0 "stash outstanding" log even though `git stash list`
// was genuinely empty and the file was never at risk. Proves: SyncAll
// succeeds, State is Ready (never PopFailed), RequiresStashRecovery is
// false, `git stash list` is empty afterward, and the untracked file's
// content survives byte-for-byte.
func TestSyncAll_UntrackedFileOnly_StashCheckoutPop_PreservesEditsByteForByte(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir)

	untrackedContent := "brand-new untracked file, never git-add'ed, must survive\n"
	if err := os.WriteFile(filepath.Join(repoDir, "untracked.txt"), []byte(untrackedContent), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"},
	}

	var events []gitSyncEvent
	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-untracked-only",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, recordingOnGitSync(&events), noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil (an untracked-only tree must not be a fatal failure)", err)
	}

	got := results[0]
	if got.Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", got.Err)
	}
	if got.State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready (must not land in pop_failed for an untracked-only tree)", got.State)
	}
	if gitstate.RequiresStashRecovery(got.State) {
		t.Error("RequiresStashRecovery(results[0].State) = true, want false -- nothing was ever at risk")
	}

	if stashList := gitOutput(t, repoDir, "stash", "list"); stashList != "" {
		t.Errorf("git stash list = %q, want empty after a clean pop", stashList)
	}

	data, err := os.ReadFile(filepath.Join(repoDir, "untracked.txt"))
	if err != nil {
		t.Fatalf("read untracked.txt after sync: %v", err)
	}
	if string(data) != untrackedContent {
		t.Errorf("untracked.txt content = %q, want %q (untracked edit must survive byte-for-byte)", data, untrackedContent)
	}

	wantStatuses := []string{"stash", "checkout", "pop"}
	if len(events) != len(wantStatuses) {
		t.Fatalf("events = %#v, want exactly %v (in order)", events, wantStatuses)
	}
}

// TestSyncAll_StagedChange_StashCheckoutPop_PreservesStagedStatus covers a
// dirty tree whose only change is `git add`-ed (staged) but not committed
// -- proving the stash/checkout/pop cycle preserves the STAGED bit, not
// just the content. Regression test for gitStashPop previously running
// plain `git stash pop` (no --index): git's own documented default leaves
// popped content unstaged even if it was staged at stash time, which is a
// real divergence from "edits survive byte-for-byte" once staging state
// counts as part of the edit.
func TestSyncAll_StagedChange_StashCheckoutPop_PreservesStagedStatus(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir) // README.md = "hello\n", committed

	stagedContent := "staged uncommitted edit, must survive AND remain staged\n"
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte(stagedContent), 0o644); err != nil {
		t.Fatalf("write staged edit: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"},
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-staged",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil", err)
	}

	got := results[0]
	if got.Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", got.Err)
	}
	if got.State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready", got.State)
	}

	data, err := os.ReadFile(filepath.Join(repoDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md after sync: %v", err)
	}
	if string(data) != stagedContent {
		t.Errorf("README.md content = %q, want %q (staged edit must survive byte-for-byte)", data, stagedContent)
	}

	// `git status --porcelain`'s first column is the INDEX (staged) status;
	// "M " (staged modified, clean working tree) proves the edit is still
	// staged, not just present -- "M " and " M" are meaningfully different
	// here, so this checks the exact two-character code, not just non-empty.
	statusOut := gitOutput(t, repoDir, "status", "--porcelain")
	if !strings.HasPrefix(statusOut, "M ") {
		t.Errorf("git status --porcelain = %q, want to start with \"M \" (staged), not unstaged", statusOut)
	}
}

// TestSyncAll_BranchAlreadyExists_PlainCheckout covers the "branch already
// exists" half of §3.4's "checkout session branch (create from base if
// absent)": an explicit repos[].branch that already exists locally is
// checked out directly, never re-created.
func TestSyncAll_BranchAlreadyExists_PlainCheckout(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir)

	explicitBranch := "existing-feature"
	runGit(t, repoDir, "branch", explicitBranch) // exists, not checked out yet

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git", Branch: &explicitBranch},
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-x",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil", err)
	}
	if results[0].Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", results[0].Err)
	}
	if head := currentBranch(t, repoDir); head != explicitBranch {
		t.Errorf("checked-out branch = %q, want %q", head, explicitBranch)
	}
}

// TestSyncAll_PopFailureDetectedNotFatal reproduces a REAL stash-pop
// conflict (verified directly against real git behavior, not simulated)
// and proves: the failure is detected (State == StatePopFailed,
// RequiresStashRecovery == true), it is reported via results[0].Err (not a
// panic/crash), and -- for a PRIMARY repo -- SyncAll still returns a
// non-nil, fatal error (matching CloneAll's own primary-fatal semantics)
// while never crashing the process.
func TestSyncAll_PopFailureDetectedNotFatal(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir) // main, README.md = "hello\n"

	conflictBranch := "conflict-branch"
	runGit(t, repoDir, "checkout", "-b", conflictBranch)
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello from conflict branch\n"), 0o644); err != nil {
		t.Fatalf("write conflict branch content: %v", err)
	}
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "conflicting change on the target branch")
	runGit(t, repoDir, "checkout", "main")

	// Dirty, uncommitted edit on main to the SAME line -- verified directly
	// (see this Step's own exploratory testing) that stashing this, then
	// checking out conflictBranch, then popping produces a real merge
	// conflict, not a clean auto-merge.
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello from main's dirty edit\n"), 0o644); err != nil {
		t.Fatalf("write dirty edit: %v", err)
	}

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git", Branch: &conflictBranch},
	}

	var events []gitSyncEvent
	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-y",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, recordingOnGitSync(&events), noopGitFetchTiming, noopGitCheckoutTiming)
	if err == nil {
		t.Fatal("SyncAll() error = nil, want a fatal error (primary repo's pop failed)")
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}

	got := results[0]
	if got.Err == nil {
		t.Fatal("results[0].Err = nil, want a pop error")
	}
	if got.State != gitstate.StatePopFailed {
		t.Errorf("results[0].State = %s, want pop_failed", got.State)
	}
	if !gitstate.RequiresStashRecovery(got.State) {
		t.Error("RequiresStashRecovery(results[0].State) = false, want true (P0: stash left outstanding)")
	}

	// The stash itself must NOT have been dropped -- git's own real
	// behavior on a pop conflict (verified directly): the entry stays in
	// the stash list for manual recovery, never silently lost.
	stashList := gitOutput(t, repoDir, "stash", "list")
	if stashList == "" {
		t.Error("git stash list is empty after a failed pop -- the stash must survive for manual recovery")
	}

	wantStatuses := []string{"stash", "checkout", "pop"}
	if len(events) != len(wantStatuses) {
		t.Fatalf("events = %#v, want exactly %v", events, wantStatuses)
	}
}

// TestSyncAll_PrimaryFailureStopsImmediately and
// TestSyncAll_SecondaryFailureContinues mirror CloneAll's own identical
// criticality tests (clone_test.go) exactly, proving SyncAll shares the
// SAME primary-fatal/secondary-warn semantics (§3.4: "position 0 =
// primary").
func TestSyncAll_PrimaryFailureStopsImmediately(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	// "bad-primary" is never actually created as a real repo directory --
	// `git -C <nonexistent dir> status --porcelain` fails immediately,
	// exactly the real-world shape of a workspace that failed to bake/
	// restore correctly.
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "bad-primary", Url: "https://example.invalid/never.git"},
		{Name: "never-attempted", Url: "https://example.invalid/never.git"},
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-z",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err == nil {
		t.Fatal("SyncAll() error = nil, want a fatal error for the failed primary repo")
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (only the primary should have been attempted)", len(results))
	}
	if results[0].Err == nil {
		t.Error("results[0].Err = nil, want a sync error")
	}
}

func TestSyncAll_SecondaryFailureContinues(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	initRepo(t, filepath.Join(workspaceDir, "primary"))
	initRepo(t, filepath.Join(workspaceDir, "later"))

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "primary", Url: "https://example.invalid/primary.git"},
		{Name: "bad-secondary", Url: "https://example.invalid/never.git"},
		{Name: "later", Url: "https://example.invalid/later.git"},
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-w",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil (a secondary failure is a warning, not fatal)", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3 (every repo attempted)", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("results[0] (primary) Err = %v, want nil", results[0].Err)
	}
	if results[1].Err == nil {
		t.Error("results[1] (bad secondary) Err = nil, want a sync error")
	}
	if results[2].Err != nil {
		t.Errorf("results[2] (later) Err = %v, want nil -- loop must continue past the secondary failure", results[2].Err)
	}
}

// TestSyncAll_MaliciousRepoNameRejectedBeforeAnySpawn mirrors
// TestCloneAll_MaliciousRepoNameRejectedBeforeAnySpawn (clone_test.go)
// exactly: a repo.Name attempting path traversal is rejected by
// reposource.ValidateRepoName BEFORE SyncAll's own filepath.Join, let alone
// any git subprocess, ever runs for it.
func TestSyncAll_MaliciousRepoNameRejectedBeforeAnySpawn(t *testing.T) {
	t.Parallel()

	parentDir := t.TempDir()
	workspaceDir := filepath.Join(parentDir, "workspace")
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "../escaped-outside-workspace", Url: "https://example.invalid/never.git"},
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-v",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err == nil {
		t.Fatal("SyncAll() error = nil, want a fatal validation error for the malicious repo name")
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Error("results[0].Err = nil, want a validation error")
	}
	if results[0].Dir != "" {
		t.Errorf("results[0].Dir = %q, want empty -- an invalid Name must never even reach filepath.Join", results[0].Dir)
	}

	escapeTarget := filepath.Join(parentDir, "escaped-outside-workspace")
	if _, statErr := os.Stat(escapeTarget); !os.IsNotExist(statErr) {
		t.Errorf("escape target stat = %v, want IsNotExist (a path-traversal target must never be created)", statErr)
	}
}

// TestSyncAll_MaliciousBranchRejectedBeforeAnySpawn proves a "-"-prefixed
// branch value -- whether supplied explicitly or (defensively) via the
// resolved narvi/<sessionID> fallback -- is rejected by
// reposource.ValidateBranch before ever reaching a git subprocess's
// argument list.
func TestSyncAll_MaliciousBranchRejectedBeforeAnySpawn(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir)

	maliciousBranch := "--upload-pack=touch /tmp/should-never-run"
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git", Branch: &maliciousBranch},
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-u",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err == nil {
		t.Fatal("SyncAll() error = nil, want a fatal validation error for the malicious branch")
	}
	if results[0].Err == nil {
		t.Error("results[0].Err = nil, want a validation error")
	}
}

// TestGitCheckoutDashDashPlacement_RealDefenseInDepth proves, against the
// REAL git binary (verified directly, not assumed from documentation), the
// exact reasoning checkoutBranch's own doc comment (sync.go) relies on: a
// TRAILING "--" (after the branch, with nothing following it) switches
// branches exactly like no "--" at all, while a LEADING "--" (immediately
// before the branch) instead makes git treat the branch name as a
// PATHSPEC to restore -- the opposite of what a checkout step needs. This
// isolates the mechanical placement question from reposource.ValidateBranch's
// own separate rejection of a "-"-prefixed value (which, in production,
// never lets such a value reach a real checkout at all).
func TestGitCheckoutDashDashPlacement_RealDefenseInDepth(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	initRepo(t, dir)
	runGit(t, dir, "checkout", "-b", "feature-x")
	runGit(t, dir, "checkout", "main")

	t.Run("trailing dashdash switches branch as expected", func(t *testing.T) {
		cmd := exec.Command("git", "-C", dir, "checkout", "feature-x", "--")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git checkout feature-x -- failed: %v\n%s", err, out)
		}
		if head := currentBranch(t, dir); head != "feature-x" {
			t.Errorf("checked-out branch = %q, want feature-x", head)
		}
	})

	t.Run("leading dashdash treats branch as a pathspec instead", func(t *testing.T) {
		runGit(t, dir, "checkout", "main")
		cmd := exec.Command("git", "-C", dir, "checkout", "--", "feature-x")
		err := cmd.Run()
		if err == nil {
			t.Fatal("git checkout -- feature-x unexpectedly succeeded -- expected it to fail treating feature-x as a pathspec")
		}
		if head := currentBranch(t, dir); head != "main" {
			t.Errorf("checked-out branch = %q, want main (a leading -- must not switch branches)", head)
		}
	})
}

// TestCleanForImageBuild_DiscardsUntrackedAndTrackedResidue proves Part E's
// own §3.4 "Image builds must snapshot a clean tree" step end to end
// against real git: setup.sh-style residue (an untracked file, an
// untracked+gitignored directory, and an uncommitted modification to a
// tracked file) is fully discarded, leaving the tree byte-for-byte
// identical to its last commit.
func TestCleanForImageBuild_DiscardsUntrackedAndTrackedResidue(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir) // README.md = "hello\n", committed

	if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	runGit(t, repoDir, "add", ".gitignore")
	runGit(t, repoDir, "commit", "-m", "add gitignore")

	// setup.sh-style residue: an untracked file, an untracked+ignored
	// directory, and an uncommitted modification to a tracked file.
	if err := os.WriteFile(filepath.Join(repoDir, "untracked.txt"), []byte("residue\n"), 0o644); err != nil {
		t.Fatalf("write untracked.txt: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "build"), 0o755); err != nil {
		t.Fatalf("mkdir build: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "build", "out.bin"), []byte("artifact\n"), 0o644); err != nil {
		t.Fatalf("write build/out.bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("dirty modification\n"), 0o644); err != nil {
		t.Fatalf("write dirty README.md: %v", err)
	}

	sup := supervisor.New()
	credentialCacheDir := filepath.Join(t.TempDir(), "credentials")
	if err := gitclone.CleanForImageBuild(context.Background(), sup, workspaceDir, []string{"repo1"}, credentialCacheDir,
		testSyncStepTimeout, testStopGrace); err != nil {
		t.Fatalf("CleanForImageBuild() error = %v, want nil", err)
	}

	if status := gitOutput(t, repoDir, "status", "--porcelain"); status != "" {
		t.Errorf("git status --porcelain = %q, want empty (fully clean tree)", status)
	}
	data, err := os.ReadFile(filepath.Join(repoDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	if string(data) != "hello\n" {
		t.Errorf("README.md = %q, want the committed content restored", data)
	}
	if _, statErr := os.Stat(filepath.Join(repoDir, "untracked.txt")); !os.IsNotExist(statErr) {
		t.Errorf("untracked.txt stat = %v, want IsNotExist", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(repoDir, "build")); !os.IsNotExist(statErr) {
		t.Errorf("build/ stat = %v, want IsNotExist", statErr)
	}
}

// TestCleanForImageBuild_FailureIsFatal proves a repo that fails to clean
// (a nonexistent/broken workspace, mirroring a real setup-gone-wrong image
// build) is a real, propagated error -- never silently swallowed.
func TestCleanForImageBuild_FailureIsFatal(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()

	sup := supervisor.New()
	credentialCacheDir := filepath.Join(t.TempDir(), "credentials")
	err := gitclone.CleanForImageBuild(context.Background(), sup, workspaceDir, []string{"nonexistent-repo"}, credentialCacheDir,
		testSyncStepTimeout, testStopGrace)
	if err == nil {
		t.Fatal("CleanForImageBuild() error = nil, want a fatal error for a nonexistent repo directory")
	}
}

// TestCleanForImageBuild_MaliciousRepoNameRejectedBeforeAnySpawn mirrors
// TestSyncAll_MaliciousRepoNameRejectedBeforeAnySpawn exactly, for
// CleanForImageBuild: this function is EXPORTED and takes bare repo names
// with no upstream validation guarantee of its own (unlike SyncAll/
// CloneAll's sessionconfig.SessionConfigReposElem, which validateRepoSpec
// already runs before either of those ever reach this package's own
// filepath.Join). A ".."-containing name must be rejected by
// reposource.ValidateRepoName before `git clean -fdx` / `git checkout --
// .` ever run against a directory outside workspaceDir -- proven here by
// seeding a real, untracked file in a sibling directory OUTSIDE
// workspaceDir and confirming CleanForImageBuild never touches it.
func TestCleanForImageBuild_MaliciousRepoNameRejectedBeforeAnySpawn(t *testing.T) {
	t.Parallel()

	parentDir := t.TempDir()
	workspaceDir := filepath.Join(parentDir, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("mkdir workspaceDir: %v", err)
	}

	outsideDir := filepath.Join(parentDir, "escaped-outside-workspace")
	initRepo(t, outsideDir)
	outsideUntracked := filepath.Join(outsideDir, "must-survive.txt")
	if err := os.WriteFile(outsideUntracked, []byte("must not be deleted\n"), 0o644); err != nil {
		t.Fatalf("write outside untracked file: %v", err)
	}

	sup := supervisor.New()
	credentialCacheDir := filepath.Join(t.TempDir(), "credentials")
	err := gitclone.CleanForImageBuild(context.Background(), sup, workspaceDir, []string{"../escaped-outside-workspace"}, credentialCacheDir,
		testSyncStepTimeout, testStopGrace)
	if err == nil {
		t.Fatal("CleanForImageBuild() error = nil, want a fatal validation error for the malicious repo name")
	}

	if _, statErr := os.Stat(outsideUntracked); statErr != nil {
		t.Errorf("outside untracked file stat = %v, want nil -- a path-traversal name must never reach `git clean`", statErr)
	}
}

// TestCleanForImageBuild_PurgesCredentialCache is §30.4(2)'s own
// defense-in-depth test: a credential minted during the clone that
// preceded this call has already been written to disk (RunGet/Get's own
// Fetch+Store path, internal/sandboxagent/credentials/get.go) -- this
// proves CleanForImageBuild removes it, so a provider image capturing the
// filesystem right after this call finds no token at all, independent of
// whether the read-only-mint primary fix (cmd/sandbox-agent's own
// runCredentialHelper) also worked correctly.
func TestCleanForImageBuild_PurgesCredentialCache(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir)

	credentialCacheDir := filepath.Join(t.TempDir(), "credentials")
	cache := &credentials.Cache{Dir: credentialCacheDir}
	if err := cache.Store("github.com", credentials.Credential{
		Username: "x-access-token", Password: "a-token-that-must-not-survive-into-any-image", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed credential cache: %v", err)
	}

	sup := supervisor.New()
	if err := gitclone.CleanForImageBuild(context.Background(), sup, workspaceDir, []string{"repo1"}, credentialCacheDir,
		testSyncStepTimeout, testStopGrace); err != nil {
		t.Fatalf("CleanForImageBuild() error = %v, want nil", err)
	}

	// The guarantee is that no credential survives into the baked image,
	// not that the directory is gone. PurgeAll deliberately empties the
	// directory and leaves it in place: removing it would unclaim its
	// name under a world-writable parent, letting another uid create it
	// and redirect a later root write with a symlink. An empty,
	// root-owned 0700 directory in the image is harmless, and carries
	// that protection forward into it.
	entries, readErr := os.ReadDir(credentialCacheDir)
	if readErr != nil {
		t.Fatalf("credential cache dir %s does not survive CleanForImageBuild: %v", credentialCacheDir, readErr)
	}
	if len(entries) != 0 {
		t.Errorf("credential cache dir holds %d entries after CleanForImageBuild, want 0 -- nothing may be baked into the image", len(entries))
	}
	if _, found, err := cache.Load("github.com"); err != nil || found {
		t.Errorf("cache.Load(\"github.com\") after CleanForImageBuild = (found=%v, err=%v), want (false, nil)", found, err)
	}
}

// TestSyncAll_FullUnscopedCheckoutOnDisk_ReAppliesSparseCheckout is the
// core regression test for audit finding F1 (§14.1, "Scoped-environment
// image-boot bypass"): a workspace that is a FULL, unscoped checkout on
// disk -- simulating a shared repo_image baked WITHOUT any path_scope,
// exactly the scenario F1 names (an image's own fingerprint/ImageSpec is
// (base, repoSHAs, runtimeVersion) only -- never path_scope -- so the SAME
// prebuilt image can be reused by a scoped session) -- must have every
// out-of-scope path actually REMOVED from disk once SyncAll runs with a
// non-empty pathScope. Before this fix, SyncAll never called
// applySparseCheckout at all, so a repo_image/snapshot_restore boot of a
// scoped session left the full, unscoped tree materialized on the sandbox
// filesystem -- exactly the bypass F1 reports.
func TestSyncAll_FullUnscopedCheckoutOnDisk_ReAppliesSparseCheckout(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	// initRepo alone already produces exactly what this test needs to
	// simulate: a real, FULL, never-sparse-configured checkout -- precisely
	// what a shared, unscoped repo_image would have baked.
	initRepo(t, repoDir)

	for _, dir := range []string{"apps/web", "apps/api"} {
		if err := os.MkdirAll(filepath.Join(repoDir, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "apps/web/index.js"), []byte("web\n"), 0o644); err != nil {
		t.Fatalf("write apps/web/index.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "apps/api/index.js"), []byte("api\n"), 0o644); err != nil {
		t.Fatalf("write apps/api/index.js: %v", err)
	}
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "add apps/web and apps/api")

	// Precondition sanity check: BEFORE SyncAll runs, this really is a full,
	// unscoped checkout -- the out-of-scope file genuinely materializes.
	if _, statErr := os.Stat(filepath.Join(repoDir, "apps/api/index.js")); statErr != nil {
		t.Fatalf("precondition failed: apps/api/index.js missing before SyncAll: %v", statErr)
	}

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"},
	}
	pathScope := []string{"/apps/web/*"}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, pathScope, "session-scoped-image",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil", err)
	}
	if results[0].Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", results[0].Err)
	}

	if _, statErr := os.Stat(filepath.Join(repoDir, "apps/web/index.js")); statErr != nil {
		t.Errorf("expected apps/web/index.js to remain on disk (in scope): %v", statErr)
	}
	for _, wantAbsent := range []string{"apps/api/index.js", "README.md"} {
		if _, statErr := os.Stat(filepath.Join(repoDir, wantAbsent)); !os.IsNotExist(statErr) {
			t.Errorf("expected %s to be ABSENT from disk after SyncAll re-applies sparse-checkout (out of scope) -- stat = %v -- this is F1's own core bypass", wantAbsent, statErr)
		}
	}
}

// TestSyncAll_InvalidPathScopeRejectedBeforeAnySync mirrors
// TestCloneAll_InvalidPathScopeRejectedBeforeAnyClone (clone_test.go)
// exactly: SyncAll validates pathScope (environment.ValidatePathScope)
// ONCE, before any repo is even attempted -- same guarantee, same
// validator, as CloneAll.
func TestSyncAll_InvalidPathScopeRejectedBeforeAnySync(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	initRepo(t, filepath.Join(workspaceDir, "repo1"))

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"},
	}
	pathScope := []string{"../escape"}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, pathScope, "session-invalid-scope",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err == nil {
		t.Fatal("SyncAll() error = nil, want a fatal validation error for the invalid path scope")
	}
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0 (no repo should have been attempted)", len(results))
	}
}

// initRepoWithDirtyOutOfScopeFile seeds an apps/web (in scope) + apps/api
// (out of scope, left DIRTY) layout -- the exact shape
// TestSyncAll_DirtyOutOfScopeFile_SparseCheckoutDetectsAndFailsLoudly,
// below, documents empirically as a real, deterministic sparse-checkout
// failure trigger. Reused by the primary/secondary criticality tests below
// it as a reliable way to force a failure specifically AT the
// sparse-checkout step, as opposed to some earlier, unrelated failure.
func initRepoWithDirtyOutOfScopeFile(t *testing.T, dir string) {
	t.Helper()

	initRepo(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "apps/web"), 0o755); err != nil {
		t.Fatalf("mkdir apps/web: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "apps/api"), 0o755); err != nil {
		t.Fatalf("mkdir apps/api: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apps/web/index.js"), []byte("web\n"), 0o644); err != nil {
		t.Fatalf("write apps/web/index.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apps/api/index.js"), []byte("api\n"), 0o644); err != nil {
		t.Fatalf("write apps/api/index.js: %v", err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "add apps/web and apps/api")

	// A real, uncommitted, OUT-OF-SCOPE edit -- dirty at sync start, so it
	// takes the stash-push -> checkout -> stash-pop path before
	// sparse-checkout ever runs.
	if err := os.WriteFile(filepath.Join(dir, "apps/api/index.js"), []byte("dirty out-of-scope edit\n"), 0o644); err != nil {
		t.Fatalf("write dirty out-of-scope edit: %v", err)
	}
}

// TestSyncAll_DirtyOutOfScopeFile_SparseCheckoutDetectsAndFailsLoudly
// covers F1's own dirty-content edge case: a path OUTSIDE pathScope that is
// DIRTY (uncommitted local changes) at sync start, so it goes through the
// real stash-push -> checkout -> stash-pop sequence before sparse-checkout
// is ever applied.
//
// EMPIRICAL FINDING (verified directly against real git, not assumed --
// git 2.42, both the system's own ambient locale and LC_ALL=C): `git
// sparse-checkout set --no-cone` on a path carrying uncommitted local
// changes EXITS 0 (success) but LEAVES that path on disk, UNTOUCHED,
// printing only "warning: The following paths are not up to date and were
// left despite sparse patterns" to stderr -- git's own documented
// reluctance to discard dirty content, not a bug in this codebase. Left
// unguarded, that is a residual instance of the exact F1 bypass this Step
// exists to close: an apparently-successful SyncAll silently leaving an
// out-of-scope path materialized on the sandbox filesystem.
//
// applySparseCheckout (clone.go) now closes this gap for every caller: any
// stderr output from `sparse-checkout set`, even alongside a 0 exit code,
// is treated as this exact failure mode and returned as a real, reported
// error -- proven here end to end via SyncAll (never silently accepted).
func TestSyncAll_DirtyOutOfScopeFile_SparseCheckoutDetectsAndFailsLoudly(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepoWithDirtyOutOfScopeFile(t, repoDir)

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"},
	}
	pathScope := []string{"/apps/web/*"}

	var events []gitSyncEvent
	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, pathScope, "session-dirty-out-of-scope",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, recordingOnGitSync(&events), noopGitFetchTiming, noopGitCheckoutTiming)
	if err == nil {
		t.Fatal("SyncAll() error = nil, want a fatal error -- a dirty out-of-scope file must be detected, never silently accepted")
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("results[0].Err = nil, want a sparse-checkout error")
	}
	// The stash/checkout/pop sequence itself succeeds (State reaches
	// Ready) -- the failure is specifically the LATER sparse-checkout
	// re-narrowing step, deliberately orthogonal to gitstate's own
	// transition table (see syncOne's own comment, sync.go, and
	// applySparseCheckout's, clone.go).
	if results[0].State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready (a sparse-checkout failure must not perturb gitstate's own terminal state)", results[0].State)
	}

	// Document the empirical finding precisely: the dirty, out-of-scope
	// edit DOES survive on disk after this failure (git's own real
	// behavior) -- exactly why this must be treated as a fatal, reported
	// error rather than silently accepted as a successful sync.
	data, readErr := os.ReadFile(filepath.Join(repoDir, "apps/api/index.js"))
	if readErr != nil {
		t.Fatalf("read apps/api/index.js: %v", readErr)
	}
	if string(data) != "dirty out-of-scope edit\n" {
		t.Errorf("apps/api/index.js content = %q, want the dirty edit preserved (documenting git's real, empirically-verified behavior)", data)
	}

	wantStatuses := []string{"stash", "checkout", "pop"}
	if len(events) != len(wantStatuses) {
		t.Fatalf("events = %#v, want exactly %v (the git-sync sequence itself completes before sparse-checkout ever runs)", events, wantStatuses)
	}
}

// TestSyncAll_SparseCheckoutFailure_PrimaryStopsImmediately and
// TestSyncAll_SparseCheckoutFailure_SecondaryContinues prove SyncAll's
// existing primary-fatal/secondary-warn criticality split (see
// TestSyncAll_PrimaryFailureStopsImmediately/
// TestSyncAll_SecondaryFailureContinues, above) holds for a failure at the
// NEW sparse-checkout step too. SyncAll's outer loop treats result.Err
// uniformly regardless of which step inside syncOne produced it, so this
// confirms EXISTING, correct behavior extends through the new code path --
// it is not new branching logic.
func TestSyncAll_SparseCheckoutFailure_PrimaryStopsImmediately(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	initRepoWithDirtyOutOfScopeFile(t, filepath.Join(workspaceDir, "bad-primary"))
	initRepo(t, filepath.Join(workspaceDir, "never-attempted"))

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "bad-primary", Url: "https://example.invalid/never.git"},
		{Name: "never-attempted", Url: "https://example.invalid/never.git"},
	}
	pathScope := []string{"/apps/web/*"}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, pathScope, "session-sparse-primary",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err == nil {
		t.Fatal("SyncAll() error = nil, want a fatal error for the primary repo's sparse-checkout failure")
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (only the primary should have been attempted)", len(results))
	}
	if results[0].Err == nil {
		t.Error("results[0].Err = nil, want a sparse-checkout error")
	}
}

func TestSyncAll_SparseCheckoutFailure_SecondaryContinues(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	initRepo(t, filepath.Join(workspaceDir, "primary"))
	initRepoWithDirtyOutOfScopeFile(t, filepath.Join(workspaceDir, "bad-secondary"))
	initRepo(t, filepath.Join(workspaceDir, "later"))

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "primary", Url: "https://example.invalid/primary.git"},
		{Name: "bad-secondary", Url: "https://example.invalid/never.git"},
		{Name: "later", Url: "https://example.invalid/later.git"},
	}
	pathScope := []string{"/apps/web/*"}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, pathScope, "session-sparse-secondary",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil (a secondary sparse-checkout failure is a warning, not fatal)", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3 (every repo attempted)", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("results[0] (primary) Err = %v, want nil", results[0].Err)
	}
	if results[1].Err == nil {
		t.Error("results[1] (bad secondary) Err = nil, want a sparse-checkout error")
	}
	if results[2].Err != nil {
		t.Errorf("results[2] (later) Err = %v, want nil -- loop must continue past the secondary's sparse-checkout failure", results[2].Err)
	}
}

// TestSyncAll_StashPopFailure_StillReAppliesSparseCheckout closes the
// residual F1 gap a security review identified against this fix: before
// this test (and the fix it verifies), syncOne returned as soon as the
// stash/checkout/pop sequence itself failed (here, a real stash-pop merge
// conflict -- StatePopFailed, the same reachable outcome
// TestSyncAll_PopFailureDetectedNotFatal already demonstrates), WITHOUT
// ever reaching the sparse-checkout re-narrowing step below it. Since a
// repo_image/snapshot_restore boot's workspace may already contain a full,
// unscoped checkout baked at image time (see SyncAll's own doc comment),
// and a failure like this one on a SECONDARY repo is only ever logged as a
// warning (SyncAll's own primary-fatal/secondary-warn split) rather than
// stopping the boot, the out-of-scope directory would have been left on
// disk, readable by an agent in the now-booted sandbox -- exactly the F1
// bypass this whole fix exists to close, just reached via a different
// (failing) path through syncOne. This test proves pathScope is now
// re-applied regardless: apps/api (out of scope) must be gone from disk
// even though the pop itself fails and the repo's own result correctly
// still reports that failure (State/Err untouched in what they report
// about the git-sync sequence itself).
func TestSyncAll_StashPopFailure_StillReAppliesSparseCheckout(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir) // main, README.md = "hello\n"

	for _, dir := range []string{"apps/web", "apps/api"} {
		if err := os.MkdirAll(filepath.Join(repoDir, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "apps/web/index.js"), []byte("web\n"), 0o644); err != nil {
		t.Fatalf("write apps/web/index.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "apps/api/index.js"), []byte("api\n"), 0o644); err != nil {
		t.Fatalf("write apps/api/index.js: %v", err)
	}
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "add apps/web and apps/api")

	// Same real stash-pop-conflict setup as TestSyncAll_PopFailureDetectedNotFatal:
	// a conflicting committed change to README.md on the target branch, plus
	// a dirty, uncommitted edit to README.md on main -- verified there
	// (and re-verified here) to produce a real merge conflict on `git stash
	// pop`, not a clean auto-merge. apps/api is untouched by any of this --
	// it is only ever removed by sparse-checkout re-narrowing, never by the
	// stash/checkout/pop sequence itself.
	conflictBranch := "conflict-branch"
	runGit(t, repoDir, "checkout", "-b", conflictBranch)
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello from conflict branch\n"), 0o644); err != nil {
		t.Fatalf("write conflict branch content: %v", err)
	}
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "conflicting change on the target branch")
	runGit(t, repoDir, "checkout", "main")

	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello from main's dirty edit\n"), 0o644); err != nil {
		t.Fatalf("write dirty edit: %v", err)
	}

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git", Branch: &conflictBranch},
	}
	pathScope := []string{"/apps/web/*"}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, pathScope, "session-pop-fail-scope",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err == nil {
		t.Fatal("SyncAll() error = nil, want a fatal error (primary repo's pop failed)")
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}

	got := results[0]
	if got.Err == nil {
		t.Fatal("results[0].Err = nil, want a pop error")
	}
	if got.State != gitstate.StatePopFailed {
		t.Errorf("results[0].State = %s, want pop_failed", got.State)
	}
	if !gitstate.RequiresStashRecovery(got.State) {
		t.Error("RequiresStashRecovery(results[0].State) = false, want true (P0: stash left outstanding)")
	}

	// The stash itself must still survive the failed pop, exactly as
	// TestSyncAll_PopFailureDetectedNotFatal already verifies -- this fix
	// must not change that P0 guarantee.
	stashList := gitOutput(t, repoDir, "stash", "list")
	if stashList == "" {
		t.Error("git stash list is empty after a failed pop -- the stash must survive for manual recovery")
	}

	// The actual regression check: pathScope re-narrowing must have run
	// despite the pop failure above -- apps/api (out of scope) gone,
	// apps/web (in scope) still present.
	if _, statErr := os.Stat(filepath.Join(repoDir, "apps/api/index.js")); !os.IsNotExist(statErr) {
		t.Errorf("expected apps/api/index.js to be ABSENT from disk after SyncAll re-applies sparse-checkout despite the pop failure -- stat = %v -- this is the residual F1 bypass a stash/checkout/pop failure previously left open", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(repoDir, "apps/web/index.js")); statErr != nil {
		t.Errorf("expected apps/web/index.js to remain on disk (in scope): %v", statErr)
	}
}

// --- §19.3 (§19.3, "warm boot: fetch-aware git sync") tests below ---
//
// newLocalOrigin/addOriginBranch create a real, local, non-bare git
// repository to act as this Step's new boot-time fetch step's own remote
// target -- matching this package's own established "verify directly
// against the real git binary, fully offline, deterministic" testing
// philosophy (see checkoutBranch's/gitStashPop's own doc comments)
// extended to SyncAll's new fetch step: a plain local filesystem path
// works perfectly well as a git remote for `git fetch`/`git ls-remote`
// (verified directly, not assumed), with no bare-repo ceremony needed and
// no real network dependency anywhere in these tests.

// newLocalOrigin creates a real git repository at a fresh directory with
// default branch "main" and one commit (README.md = "hello\n", via
// initRepo) -- the repo_image's own "shared, tip-tracking remote" for these
// tests' own purposes. Returns its directory, ready to be wired as
// `git remote add origin <this path>` in a separate workspace repo.
func newLocalOrigin(t *testing.T) string {
	t.Helper()
	originDir := filepath.Join(t.TempDir(), "origin")
	initRepo(t, originDir)
	return originDir
}

// addOriginBranch creates a new branch on an already-initialized origin
// repo (newLocalOrigin's own return value), with a MARKER.txt file unique
// to that branch (content) -- so a later test can prove, by reading
// MARKER.txt back out of the WORKSPACE repo after SyncAll runs, that a
// checkout genuinely pulled this exact origin branch's own content, not
// some other ref. Leaves origin checked out on its own default branch
// afterward (never leaves it on the newly created branch), matching a real
// remote repo's own steady state.
func addOriginBranch(t *testing.T, originDir, branch, markerContent string) {
	t.Helper()
	runGit(t, originDir, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(originDir, "MARKER.txt"), []byte(markerContent), 0o644); err != nil {
		t.Fatalf("write MARKER.txt on origin branch %s: %v", branch, err)
	}
	runGit(t, originDir, "add", ".")
	runGit(t, originDir, "commit", "-m", "content for "+branch)
	runGit(t, originDir, "checkout", "main")
}

// updateOriginDefaultBranch commits a new README.md content on origin's own
// default branch ("main") -- so a later test can prove a checkout used
// origin's fetched TIP content, not the workspace repo's own pre-existing,
// stale local content (both start from initRepo's identical "hello\n").
func updateOriginDefaultBranch(t *testing.T, originDir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(originDir, "README.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("update origin default-branch README.md: %v", err)
	}
	runGit(t, originDir, "add", ".")
	runGit(t, originDir, "commit", "-m", "origin default-branch tip update")
}

// branchExistsForTest reports whether branch exists as a local branch in
// dir -- a read-only, test-side sibling of sync.go's own unexported
// branchExistsLocally (unreachable from this external gitclone_test
// package), used here specifically to prove the negative: that
// TestSyncAll_FetchFails_ExplicitBranchNotLocalNotFetchable_PrimaryFatal's
// own explicit target branch was NEVER created, not even a same-named
// branch forked at a stale base (§19.3's own non-negotiable rule).
func branchExistsForTest(t *testing.T, dir, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	err := cmd.Run()
	if err == nil {
		return true
	}
	if exitCode(t, err) == 1 {
		return false
	}
	t.Fatalf("git rev-parse --verify --quiet refs/heads/%s: unexpected error: %v", branch, err)
	return false
}

// TestSyncAll_FetchSucceeds_BranchExistsOnOrigin_PrefersOriginTrackingBranch
// covers §19.3 point 2's own primary preference: an explicit target branch
// that does NOT exist locally, but DOES exist on origin -- the fetch
// actually succeeds for it, so checkoutBranch must check it out FROM
// origin/<branch> (`git checkout -b <branch> origin/<branch> --`), never
// falling back to creating it fresh from the workspace's own (here,
// deliberately stale) local HEAD.
func TestSyncAll_FetchSucceeds_BranchExistsOnOrigin_PrefersOriginTrackingBranch(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir) // local "main" only, README.md = "hello\n" -- simulates a repo_image's own baked, stale tree; the target branch does not exist here at all

	originDir := newLocalOrigin(t)
	targetBranch := "feature-on-origin"
	addOriginBranch(t, originDir, targetBranch, "origin's real tip content for feature-on-origin\n")

	runGit(t, repoDir, "remote", "add", "origin", originDir)

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git", Branch: &targetBranch},
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-fetch-branch-on-origin",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil", err)
	}

	got := results[0]
	if got.Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", got.Err)
	}
	if got.State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready", got.State)
	}
	if head := currentBranch(t, repoDir); head != targetBranch {
		t.Errorf("checked-out branch = %q, want %q", head, targetBranch)
	}

	data, err := os.ReadFile(filepath.Join(repoDir, "MARKER.txt"))
	if err != nil {
		t.Fatalf("read MARKER.txt after sync (must exist -- it only exists on origin's own feature-on-origin branch): %v", err)
	}
	if string(data) != "origin's real tip content for feature-on-origin\n" {
		t.Errorf("MARKER.txt content = %q, want origin's own tip content -- proves checkout preferred origin/%s over creating a fresh branch from local HEAD", data, targetBranch)
	}
}

// TestSyncAll_FetchSucceeds_InventedBranchNotOnOrigin_FallsBackToOriginDefaultBranch
// covers §19.3 point 2's own second preference: the common invented
// "narvi/<sessionID>" branch case (repo.Branch nil) -- the fetch of the
// repo's DEFAULT branch succeeds (the remote is perfectly reachable), but
// this exact invented branch obviously does not exist upstream. checkoutBranch
// must still prefer origin/<default-branch> over the workspace's own local,
// stale HEAD -- proven here by giving origin's own default branch DIFFERENT
// content than the local repo's stale one.
func TestSyncAll_FetchSucceeds_InventedBranchNotOnOrigin_FallsBackToOriginDefaultBranch(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir) // local "main", README.md = "hello\n" -- stale relative to origin's own tip below

	originDir := newLocalOrigin(t)
	updateOriginDefaultBranch(t, originDir, "origin's real default-branch tip\n")

	runGit(t, repoDir, "remote", "add", "origin", originDir)

	sessionID := "session-fetch-invented-branch"
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"}, // Branch nil -> invented narvi/<sessionID>
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, sessionID,
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil", err)
	}

	got := results[0]
	if got.Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", got.Err)
	}
	if got.State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready", got.State)
	}
	wantBranch := "narvi/" + sessionID
	if head := currentBranch(t, repoDir); head != wantBranch {
		t.Errorf("checked-out branch = %q, want %q", head, wantBranch)
	}

	data, err := os.ReadFile(filepath.Join(repoDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md after sync: %v", err)
	}
	if string(data) != "origin's real default-branch tip\n" {
		t.Errorf("README.md content = %q, want origin's own default-branch tip content -- proves the invented branch was created from the fetched origin/main, not the local stale HEAD", data)
	}
}

// TestSyncAll_FetchFails_InventedBranchNil_DegradesAndFallsBackToHead covers
// §19.3's degrade policy for the invented-branch case (repo.Branch nil,
// "acceptable from HEAD"): the fetch fails ENTIRELY (origin points at a
// real but nonexistent path -- deterministic, offline), so checkoutBranch
// has no remote-tracking ref of any kind available and falls all the way
// back to today's original, unchanged HEAD-based creation.
//
// This IS a genuine degrade (Finding 2/3's own distinction): unlike the
// "ordinary warm boot" case (TestSyncAll_FetchSucceeds_
// InventedBranchNotOnOrigin_NoDegradeWarningLogged, below), the default
// branch here was never even resolved (the remote is entirely unreachable),
// so the top-level "boot-time fetch failed" WARN must still fire -- along
// with checkoutBase's own new HEAD-fallback WARN (Finding 5). Deliberately
// NOT t.Parallel(): swaps the process-wide slog default logger (platform.
// Logger(ctx) always reads slog.Default()) to capture both -- see
// TestSyncAll_FetchFails_BranchResolvableLocally_DegradesAndLogsWarning's
// own doc comment for why this is safe against every t.Parallel() test in
// this file.
func TestSyncAll_FetchFails_InventedBranchNil_DegradesAndFallsBackToHead(t *testing.T) {
	originalLogger := slog.Default()
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(originalLogger)

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir)

	runGit(t, repoDir, "remote", "add", "origin", filepath.Join(t.TempDir(), "does-not-exist"))

	sessionID := "session-fetch-degrade-invented"
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"}, // Branch nil
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, sessionID,
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil (degrade-and-proceed, not fatal)", err)
	}

	got := results[0]
	if got.Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", got.Err)
	}
	if got.State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready (degraded fetch failure reaches the SAME terminal state as success)", got.State)
	}
	wantBranch := "narvi/" + sessionID
	if head := currentBranch(t, repoDir); head != wantBranch {
		t.Errorf("checked-out branch = %q, want %q", head, wantBranch)
	}

	data, err := os.ReadFile(filepath.Join(repoDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	if string(data) != "hello\n" {
		t.Errorf("README.md = %q, want %q -- the invented branch must be created from local HEAD, today's unchanged fallback, since the fetch failed entirely", data, "hello\n")
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "boot-time fetch failed") {
		t.Errorf("log output = %q, want the genuine-degrade WARN (the remote was entirely unreachable, not just this invented branch missing)", logged)
	}
	if !strings.Contains(logged, "checkout base falls back to local HEAD") {
		t.Errorf("log output = %q, want checkoutBase's own new WARN naming HEAD as the (stale) selected base", logged)
	}
}

// TestSyncAll_FetchSucceeds_InventedBranchNotOnOrigin_NoDegradeWarningLogged
// proves the fix for Finding 2/3: an ORDINARY warm boot (repo.Branch nil,
// origin perfectly reachable, its own default branch fetches cleanly) must
// NOT log the "boot-time fetch failed" WARN, even though the invented
// "narvi/<sessionID>" branch's own fetch does fail (by construction -- that
// exact name never exists upstream). Before this fix, this exact scenario
// logged the misleading WARN on virtually every ordinary boot, drowning the
// genuine-degrade signal it exists to raise. The routine origin/<default>
// fallback IS still logged, though -- at Info, not Warn, per Finding 3's own
// "log the resolved base and the reason whenever the preferred branch is not
// used".
//
// Deliberately NOT t.Parallel() -- same logger-swap justification as this
// file's other slog-capturing tests.
func TestSyncAll_FetchSucceeds_InventedBranchNotOnOrigin_NoDegradeWarningLogged(t *testing.T) {
	originalLogger := slog.Default()
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(originalLogger)

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir) // local "main", README.md = "hello\n" -- stale relative to origin's own tip below

	originDir := newLocalOrigin(t)
	updateOriginDefaultBranch(t, originDir, "origin's real default-branch tip\n")

	runGit(t, repoDir, "remote", "add", "origin", originDir)

	sessionID := "session-fetch-invented-branch-no-warn"
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"}, // Branch nil -> invented narvi/<sessionID>
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, sessionID,
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil", err)
	}

	got := results[0]
	if got.Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", got.Err)
	}
	if got.State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready", got.State)
	}

	logged := logBuf.String()
	if strings.Contains(logged, "boot-time fetch failed") {
		t.Errorf("log output = %q, want NO degrade WARN -- this is an ordinary warm boot (origin reachable, default branch fetched cleanly), not a genuine degrade", logged)
	}
	if !strings.Contains(logged, "checkout base falls back to origin's default branch") {
		t.Errorf("log output = %q, want the routine Info-level base-selection log naming origin/<default> as the reason the preferred (invented) branch wasn't used", logged)
	}
	if strings.Contains(logged, `"level":"WARN"`) {
		t.Errorf("log output = %q, want no WARN-level line at all for this ordinary boot", logged)
	}

	// Finding 2 (audit-remediation batch B7): the Info-level fallback log
	// line previously asserted a single invented reason ("target branch does
	// not exist as a fetched remote-tracking ref") without ever including
	// the actual gitFetchStep targetFetchErr for the invented branch's own
	// fetch attempt -- discarding it entirely, even at Info. It must now be
	// present and non-empty: a real, non-obvious cause (a transient auth/
	// network blip specific to this branch's own fetch) would otherwise
	// never surface anywhere in the log, indistinguishable from this
	// routine, expected case.
	if !strings.Contains(logged, `"fetch_error"`) {
		t.Fatalf("log output = %q, want the checkout-base fallback Info line to carry a fetch_error field (the actual, real underlying git error), not silently discard it", logged)
	}
	if strings.Contains(logged, `"fetch_error":null`) || strings.Contains(logged, `"fetch_error":""`) {
		t.Errorf("log output = %q, want fetch_error to carry the real underlying git fetch failure, not a null/empty placeholder", logged)
	}
}

// TestSyncAll_DefaultBranchFetchFailsIndependently_LogsWarningEvenWhenTargetFetchSucceeds
// proves the fix for Finding 5's second silently-swallowed error: gitFetchStep
// used to discard defaultFetchErr ENTIRELY whenever branch != defaultBranch,
// even on a boot where the actual target-branch fetch succeeds fine (no
// "boot-time fetch failed" WARN would ever fire on this path at all -- the
// only place that error could otherwise have surfaced).
//
// Origin is built with two branches with COMPLETELY DISJOINT history:
// "feature" (this test's own explicit target, a REAL commit) and "main"
// (HEAD's own default) -- but "main" is never given a real commit at all:
// its own refs/heads/main file is written directly (bypassing `git
// update-ref`, which would refuse a nonexistent object) with a well-formed
// but entirely fictitious 40-hex-digit SHA that has never existed anywhere.
// `git ls-remote --symref` still resolves defaultBranch to "main" correctly
// (it reports the ref NAME/target from the ref store directly, never
// validating that the SHA it names actually resolves to a real object --
// verified directly against the real git binary, this package's own
// established philosophy); `git fetch -- main` genuinely and
// deterministically fails ("not our ref"), regardless of any gc/pack timing
// -- unlike deleting a real loose object after the fact (this test's own
// earlier, flakier approach under heavy parallel load: gc/pack timing is
// never a factor when the object never existed in the first place).
// "feature"'s own disjoint object graph is entirely untouched and fetches
// fine.
func TestSyncAll_DefaultBranchFetchFailsIndependently_LogsWarningEvenWhenTargetFetchSucceeds(t *testing.T) {
	originalLogger := slog.Default()
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(originalLogger)

	originDir := filepath.Join(t.TempDir(), "origin")
	if err := os.MkdirAll(originDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", originDir, err)
	}
	runGit(t, originDir, "init", "-b", "main")
	runGit(t, originDir, "config", "user.email", "test@example.com")
	runGit(t, originDir, "config", "user.name", "Test")

	// "feature" is the only branch that ever gets a real commit -- created
	// via --orphan (works fine against main's still-unborn HEAD), so its own
	// object graph shares nothing with "main" at all.
	runGit(t, originDir, "checkout", "--orphan", "feature")
	if err := os.WriteFile(filepath.Join(originDir, "feature-file.txt"), []byte("feature content\n"), 0o644); err != nil {
		t.Fatalf("write feature-file.txt: %v", err)
	}
	runGit(t, originDir, "add", ".")
	runGit(t, originDir, "commit", "-m", "feature (disjoint history)")

	// Point HEAD's own symref back at "main" (a lightweight ref-file rewrite
	// -- no working-tree checkout, so it doesn't care that main has no real
	// commit to check out), then write refs/heads/main directly with a
	// fictitious SHA -- git's own plain-text loose-ref format, never once
	// touching `git update-ref`/`git commit`, either of which would refuse
	// (or be unable) to point a real ref at a nonexistent object.
	runGit(t, originDir, "symbolic-ref", "HEAD", "refs/heads/main")
	fakeSHA := strings.Repeat("ab", 20)
	if err := os.WriteFile(filepath.Join(originDir, ".git", "refs", "heads", "main"), []byte(fakeSHA+"\n"), 0o644); err != nil {
		t.Fatalf("write fictitious refs/heads/main: %v", err)
	}

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir)
	runGit(t, repoDir, "remote", "add", "origin", originDir)

	targetBranch := "feature"
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git", Branch: &targetBranch},
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-default-branch-fetch-fails",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil (the EXPLICIT target branch's own fetch succeeded)", err)
	}

	got := results[0]
	if got.Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", got.Err)
	}
	if got.State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready", got.State)
	}
	if head := currentBranch(t, repoDir); head != targetBranch {
		t.Errorf("checked-out branch = %q, want %q", head, targetBranch)
	}

	logged := logBuf.String()
	if strings.Contains(logged, "boot-time fetch failed") {
		t.Errorf("log output = %q, want NO top-level degrade WARN -- the target branch's own fetch succeeded", logged)
	}
	if !strings.Contains(logged, "fetch of resolved default branch failed") {
		t.Errorf("log output = %q, want a WARN naming the independently-failed default-branch fetch (previously silently discarded)", logged)
	}
	if !strings.Contains(logged, `"default_branch":"main"`) {
		t.Errorf("log output = %q, want it to name the affected default branch %q", logged, "main")
	}
}

// TestSyncAll_FetchFails_BranchResolvableLocally_DegradesAndLogsWarning
// covers §19.3's degrade policy for an explicit branch that already exists
// locally: the fetch fails entirely, but nothing upstream was ever needed
// to check this branch out, so SyncAll must degrade-and-proceed via
// today's existing local checkout path AND log the boot-time warning §19.3
// requires ("recorded in the boot log") -- AND the separate
// resolveDefaultBranch failure this same scenario triggers (origin is
// entirely unreachable), previously silently discarded (Finding 5).
//
// Deliberately NOT t.Parallel(): this test (and its siblings added
// alongside it in this Step) temporarily swaps the process-wide slog
// default logger (platform.Logger(ctx) always reads slog.Default()) to
// capture these exact lines. Go's own test scheduler never interleaves a
// NON-parallel test's body with any OTHER test's body in the same package
// -- every t.Parallel() test above pauses immediately at that call and only
// resumes once every sequential (non-parallel) test in this file has
// already finished running -- so this global mutation cannot race with any
// other test here, specifically because none of these tests calls
// t.Parallel() themselves.
func TestSyncAll_FetchFails_BranchResolvableLocally_DegradesAndLogsWarning(t *testing.T) {
	originalLogger := slog.Default()
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(originalLogger)

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir)

	targetBranch := "already-local-branch"
	runGit(t, repoDir, "branch", targetBranch) // exists locally -- degrade policy allows proceeding regardless of the fetch outcome

	// origin points at a real but nonexistent path -- a real, deterministic,
	// offline fetch failure every time, simulating "the remote is
	// unreachable" with no actual network dependency.
	runGit(t, repoDir, "remote", "add", "origin", filepath.Join(t.TempDir(), "does-not-exist"))

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git", Branch: &targetBranch},
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-fetch-degrade-warn",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil (degrade-and-proceed, not fatal)", err)
	}

	got := results[0]
	if got.Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", got.Err)
	}
	if got.State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready", got.State)
	}
	if head := currentBranch(t, repoDir); head != targetBranch {
		t.Errorf("checked-out branch = %q, want %q (existing local checkout path, unaffected by the fetch failure)", head, targetBranch)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "boot-time fetch failed") {
		t.Errorf("log output = %q, want it to contain the boot-time fetch failure warning (§19.3: \"recorded in the boot log\")", logged)
	}
	if !strings.Contains(logged, `"level":"WARN"`) {
		t.Errorf("log output = %q, want a WARN-level log entry", logged)
	}
	if !strings.Contains(logged, targetBranch) {
		t.Errorf("log output = %q, want it to name the affected branch %q", logged, targetBranch)
	}
	// Finding 5: resolveDefaultBranch's own error (origin is entirely
	// unreachable here, so ls-remote --symref fails too) must no longer be
	// silently discarded.
	if !strings.Contains(logged, "resolve default branch failed") {
		t.Errorf("log output = %q, want it to also contain the resolve-default-branch failure warning (previously silently discarded)", logged)
	}
}

// TestSyncAll_FetchFails_ExplicitBranchNotLocalNotFetchable_PrimaryFatal is
// the single most important test in this Step (§19.3's own non-negotiable
// rule): a session explicitly named a branch that exists NEITHER locally
// NOR on a reachable remote (the fetch fails entirely). This must be a hard
// failure -- gitstate.StateFetchFailed -- NEVER a silent degrade into
// checkoutBranch's own HEAD fallback, which would fork a new, same-named
// branch at a stale base and hide a real divergence from the branch this
// session actually asked for. Proven here both by the returned error/state
// AND by the negative assertion that matters most: the branch was NEVER
// created on disk at all.
func TestSyncAll_FetchFails_ExplicitBranchNotLocalNotFetchable_PrimaryFatal(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir) // local "main" only -- targetBranch does not exist locally

	// origin points at a real but nonexistent path -- deterministic,
	// offline fetch failure every time.
	runGit(t, repoDir, "remote", "add", "origin", filepath.Join(t.TempDir(), "does-not-exist"))

	targetBranch := "explicit-branch-nowhere"
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git", Branch: &targetBranch},
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-fetch-hard-fail-primary",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err == nil {
		t.Fatal("SyncAll() error = nil, want a fatal error (primary repo's fetch failed with no degrade allowed)")
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}

	got := results[0]
	if got.Err == nil {
		t.Fatal("results[0].Err = nil, want a fetch error")
	}
	if got.State != gitstate.StateFetchFailed {
		t.Errorf("results[0].State = %s, want fetch_failed", got.State)
	}
	if gitstate.RequiresStashRecovery(got.State) {
		t.Error("RequiresStashRecovery(fetch_failed) = true, want false -- no stash was ever taken (the fetch step precedes the dirty-check entirely)")
	}

	// The critical negative assertion this whole Step exists to guarantee:
	// no same-named branch was silently forked at a stale base.
	if branchExistsForTest(t, repoDir, targetBranch) {
		t.Error("target branch was created locally despite a fatal fetch failure -- this is EXACTLY the silent-fork-at-stale-base bug §19.3 exists to prevent")
	}
	if head := currentBranch(t, repoDir); head != "main" {
		t.Errorf("checked-out branch = %q, want main (unchanged -- checkout must never even have been attempted)", head)
	}
}

// TestSyncAll_FetchFails_ExplicitBranchNotLocalNotFetchable_SecondaryWarnContinues
// mirrors TestSyncAll_SecondaryFailureContinues's own existing criticality
// convention exactly (§3.4: "position 0 = primary"), proving the SAME
// primary-fatal/secondary-warn split holds for this Step's new
// StateFetchFailed failure: a secondary repo's fatal fetch failure is
// logged as a warning and the loop continues past it, never stopping
// SyncAll as a whole.
func TestSyncAll_FetchFails_ExplicitBranchNotLocalNotFetchable_SecondaryWarnContinues(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	initRepo(t, filepath.Join(workspaceDir, "primary"))

	badSecondaryDir := filepath.Join(workspaceDir, "bad-secondary")
	initRepo(t, badSecondaryDir)
	runGit(t, badSecondaryDir, "remote", "add", "origin", filepath.Join(t.TempDir(), "does-not-exist"))

	initRepo(t, filepath.Join(workspaceDir, "later"))

	targetBranch := "explicit-branch-nowhere"
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "primary", Url: "https://example.invalid/primary.git"},
		{Name: "bad-secondary", Url: "https://example.invalid/never.git", Branch: &targetBranch},
		{Name: "later", Url: "https://example.invalid/later.git"},
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-fetch-hard-fail-secondary",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil (a secondary repo's fatal fetch failure is a warning, not fatal for the whole loop)", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3 (every repo attempted)", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("results[0] (primary) Err = %v, want nil", results[0].Err)
	}
	if results[1].Err == nil {
		t.Error("results[1] (bad secondary) Err = nil, want a fetch error")
	}
	if results[1].State != gitstate.StateFetchFailed {
		t.Errorf("results[1].State = %s, want fetch_failed", results[1].State)
	}
	if branchExistsForTest(t, badSecondaryDir, targetBranch) {
		t.Error("target branch was created locally on the secondary repo despite a fatal fetch failure -- this is EXACTLY the silent-fork-at-stale-base bug §19.3 exists to prevent")
	}
	if results[2].Err != nil {
		t.Errorf("results[2] (later) Err = %v, want nil -- loop must continue past the secondary's fetch failure", results[2].Err)
	}
}

// gitOutput runs git with args in dir and returns its trimmed stdout,
// failing the test on any error -- a read-only sibling of clone_test.go's
// own runGit (which discards output and only checks for failure).
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v (dir=%s) failed: %v", args, dir, err)
	}
	return string(out)
}

// TestSyncAll_ScopedBakedThenUnscopedSession_DisablesSparseCheckout proves
// §19.7's own new hardening: the on-disk workspace was left sparse-checked-
// out by whatever produced it (simulating a snapshot_restore boot
// restoring a SCOPED session's own snapshot into an UNSCOPED session's
// config, or a repo_image workspace shared from a scoped session) -- a
// SyncAll call with an EMPTY pathScope must disable sparse-checkout so the
// full tree actually materializes, the reverse direction
// applySparseCheckout's own forward direction does not cover.
func TestSyncAll_ScopedBakedThenUnscopedSession_DisablesSparseCheckout(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir)

	for _, dir := range []string{"apps/web", "apps/api"} {
		if err := os.MkdirAll(filepath.Join(repoDir, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "apps/web/index.js"), []byte("web\n"), 0o644); err != nil {
		t.Fatalf("write apps/web/index.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "apps/api/index.js"), []byte("api\n"), 0o644); err != nil {
		t.Fatalf("write apps/api/index.js: %v", err)
	}
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "add apps/web and apps/api")

	// Simulate a PRIOR scoped bake/restore: sparse-checkout is enabled
	// directly against the real on-disk repo, narrowed to apps/web only --
	// exactly the state a snapshot_restore boot would find on disk before
	// this session's own (unscoped) SyncAll call ever runs.
	runGit(t, repoDir, "sparse-checkout", "set", "--no-cone", "--", "/apps/web/*")

	// Precondition sanity check: the out-of-scope file is genuinely absent
	// BEFORE this session's own SyncAll call.
	if _, statErr := os.Stat(filepath.Join(repoDir, "apps/api/index.js")); !os.IsNotExist(statErr) {
		t.Fatalf("precondition failed: apps/api/index.js unexpectedly present before SyncAll, stat = %v", statErr)
	}

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"},
	}

	sup := supervisor.New()
	// pathScope is nil: THIS session is unscoped.
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-unscoped-after-scoped-bake",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil", err)
	}
	if results[0].Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", results[0].Err)
	}

	for _, wantPresent := range []string{"apps/web/index.js", "apps/api/index.js", "README.md"} {
		if _, statErr := os.Stat(filepath.Join(repoDir, wantPresent)); statErr != nil {
			t.Errorf("expected %s to materialize (sparse-checkout must be disabled for an unscoped session): %v", wantPresent, statErr)
		}
	}

	if enabled := strings.TrimSpace(gitOutputAllowFailure(t, repoDir, "config", "--type=bool", "core.sparseCheckout")); enabled != "false" {
		t.Errorf("core.sparseCheckout = %q after SyncAll, want %q (disabled)", enabled, "false")
	}
}

// TestSyncAll_NeverSparse_UnscopedSession_NoDisableAttempted proves the
// overwhelming common case (a workspace that was never sparse-checked-out
// at all) costs nothing extra: SyncAll succeeds unchanged, and (since
// there is nothing to disable) `git sparse-checkout disable` is never even
// attempted -- confirmed indirectly by the fact that core.sparseCheckout
// stays entirely UNSET (not merely "false") after SyncAll, exactly as it
// was before.
func TestSyncAll_NeverSparse_UnscopedSession_NoDisableAttempted(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initRepo(t, repoDir)

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"},
	}

	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, "session-never-sparse",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil", err)
	}
	if results[0].Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", results[0].Err)
	}

	if _, statErr := os.Stat(filepath.Join(repoDir, "README.md")); statErr != nil {
		t.Errorf("README.md missing, want present (never scoped): %v", statErr)
	}

	// core.sparseCheckout was never set at all -- `git config` itself
	// exits 1 (key unset), which gitOutputAllowFailure surfaces as "".
	if got := gitOutputAllowFailure(t, repoDir, "config", "--type=bool", "core.sparseCheckout"); got != "" {
		t.Errorf("core.sparseCheckout = %q, want unset entirely (disable was never even attempted)", got)
	}
}

// gitOutputAllowFailure runs git with args in dir and returns its trimmed
// stdout, tolerating a non-zero exit (returning "" in that case) -- unlike
// gitOutput above, which fails the test on any error. Used for `git config
// --type=bool core.sparseCheckout`, whose own "key unset" case is a
// normal, expected exit 1.
func gitOutputAllowFailure(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestSyncAll_RelaysGitFetchAndCheckoutTiming proves §33.3's sandbox-side
// relay: this Step deleted telemetry.go's own local sandbox_agent_git_
// fetch_duration_seconds/sandbox_agent_git_checkout_duration_seconds
// histogram recording (recordGitFetchDuration/recordGitCheckoutDuration)
// and replaced it with the OnGitFetchTiming/OnGitCheckoutTiming callbacks
// syncOne now calls instead -- this test drives a REAL SyncAll call (same
// scenario the deleted telemetry_test.go's own
// TestSyncAll_RecordsGitFetchAndCheckoutDurationMetrics used: no "origin"
// remote configured, so the fetch step is expected to degrade rather than
// fail this repo outright, and the subsequent checkout onto HEAD is
// expected to succeed) and asserts each callback receives exactly the
// tags its own deleted call site used to record locally.
func TestSyncAll_RelaysGitFetchAndCheckoutTiming(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoName := "repo-fetch-checkout-timing-relay"
	repoDir := filepath.Join(workspaceDir, repoName)
	initRepo(t, repoDir) // branch "main", one commit, no "origin" configured

	sessionID := "session-fetch-checkout-timing-relay"
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: repoName, Url: "https://example.invalid/repo1.git"},
	}

	var fetchCalls []gitFetchTimingEvent
	var checkoutCalls []gitCheckoutTimingEvent
	sup := supervisor.New()
	results, err := gitclone.SyncAll(context.Background(), sup, workspaceDir, repos, nil, sessionID,
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {},
		recordingOnGitFetchTiming(&fetchCalls), recordingOnGitCheckoutTiming(&checkoutCalls))
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil", err)
	}
	if len(results) != 1 || results[0].Err != nil || results[0].State != gitstate.StateReady {
		t.Fatalf("results = %+v, want one ready result", results)
	}

	if len(fetchCalls) != 1 {
		t.Fatalf("len(fetchCalls) = %d, want exactly 1", len(fetchCalls))
	}
	if fetchCalls[0].repo != repoName {
		t.Errorf("fetchCalls[0].repo = %q, want %q", fetchCalls[0].repo, repoName)
	}
	if !fetchCalls[0].degraded {
		t.Errorf("fetchCalls[0].degraded = false, want true -- no real 'origin' remote is configured, so this repo's own fetch is expected to degrade")
	}
	if fetchCalls[0].seconds < 0 {
		t.Errorf("fetchCalls[0].seconds = %v, want >= 0", fetchCalls[0].seconds)
	}

	if len(checkoutCalls) != 1 {
		t.Fatalf("len(checkoutCalls) = %d, want exactly 1", len(checkoutCalls))
	}
	if checkoutCalls[0].repo != repoName {
		t.Errorf("checkoutCalls[0].repo = %q, want %q", checkoutCalls[0].repo, repoName)
	}
	if checkoutCalls[0].failed {
		t.Errorf("checkoutCalls[0].failed = true, want false -- this repo's own checkout succeeded")
	}
	if checkoutCalls[0].seconds < 0 {
		t.Errorf("checkoutCalls[0].seconds = %v, want >= 0", checkoutCalls[0].seconds)
	}
}
