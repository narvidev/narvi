package boot_test

// (§19.6): end-to-end tests of the graduated setup-rerun ladder,
// exercised through the SAME real orchestration points as
// resilience_repoimage_test.go's own §19.2 precedent (boot.RunBoot,
// boot.ComputeSetupRerunLadder) -- a real git repo, a real /narvi/image-
// manifest.json-shaped boot.ImageManifest, and real spawned shell scripts,
// never a bare EvaluateHook table-test case for the ladder's own
// orchestration (hooks.go's runSetupRerunLadder is unexported and has no
// meaningful surface to unit-test in isolation from RunBoot/RunHooks).

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
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// buildRepoAtBuiltSHA sets up a real git repo at repoDir with a setup.sh
// (touching setupMarker) and, if lockfileContent is non-empty, a
// package-lock.json carrying it -- commits everything as one "built"
// commit, and returns that commit's own SHA (the value a real image build
// would have baked into built_repo_shas).
func buildRepoAtBuiltSHA(t *testing.T, repoDir, setupMarker, lockfileContent string) string {
	t.Helper()

	mkdirAll(t, repoDir)
	initGitRepo(t, repoDir)

	writeScript(t, filepath.Join(repoDir, "setup.sh"), "touch "+setupMarker)
	if lockfileContent != "" {
		writeFileHelper(t, filepath.Join(repoDir, "package-lock.json"), lockfileContent)
	}
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-m", "built commit")

	return gitRevParseHEAD(t, repoDir)
}

func writeFileHelper(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestSetupRerunLadder_DigestMatch_SkipsSetupEntirely proves §19.6's first
// bullet end to end: an unrelated later commit makes workspaceMoved true,
// but the dependency-manifest digest (baked to match the CURRENT, still
// byte-identical package-lock.json) proves dependencies never moved --
// setup.sh must never run at all.
func TestSetupRerunLadder_DigestMatch_SkipsSetupEntirely(t *testing.T) {
	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	setupMarker := filepath.Join(workspaceDir, "setup-should-not-run")

	builtSHA := buildRepoAtBuiltSHA(t, repoDir, setupMarker, `{"lockfileVersion":1}`)

	// An unrelated later commit -- moves workspaceMoved, never touches
	// package-lock.json.
	writeFileHelper(t, filepath.Join(repoDir, "unrelated.txt"), "unrelated")
	runGit(t, repoDir, "add", "unrelated.txt")
	runGit(t, repoDir, "commit", "-m", "unrelated change")

	bakedDigest, found, err := boot.ComputeDependencyManifestDigest(repoDir)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest() error = %v", err)
	}
	if !found {
		t.Fatal("precondition failed: found = false, want true (repoDir has a real package-lock.json)")
	}

	currentSHA := gitRevParseHEAD(t, repoDir)
	manifest := boot.ImageManifest{
		BuiltRepoShas:             map[string]string{"repo1": builtSHA},
		DependencyManifestDigests: map[string]string{"repo1": bakedDigest},
	}
	currentSHAs := map[string]string{"repo1": currentSHA}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, currentSHAs)
	if !workspaceMoved["repo1"] {
		t.Fatalf("precondition failed: workspaceMoved[repo1] = false, want true")
	}
	gitDirRoot := t.TempDir()
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}
	ladderLayout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	ladderSup := supervisor.New()
	if err := gitdir.Seed(context.Background(), ladderSup, ladderLayout.Repo("repo1"), "https://example.invalid/repo1.git", nil, 5*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed() error = %v", err)
	}
	ladder := boot.ComputeSetupRerunLadder(context.Background(), ladderSup, ladderLayout, nil, manifest, true, false, workspaceDir, currentSHAs, 5*time.Second, 5*time.Second)

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	err = boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved, ladder, nil,
		noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil", err)
	}

	assertFileAbsent(t, setupMarker)
}

// TestSetupRerunLadder_DeltaEligible_RunsSyncInsteadOfSetup proves §19.6's
// third bullet: setup.sh is byte-identical since the built SHA (a real,
// unrelated later commit made workspaceMoved true, and there is no baked
// dependency digest, so the digest tier is ineligible) -- sync.sh runs
// INSTEAD of setup.sh, which must never run at all.
func TestSetupRerunLadder_DeltaEligible_RunsSyncInsteadOfSetup(t *testing.T) {
	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	setupMarker := filepath.Join(workspaceDir, "setup-should-not-run")
	syncMarker := filepath.Join(workspaceDir, "sync-should-run")

	builtSHA := buildRepoAtBuiltSHA(t, repoDir, setupMarker, "")
	writeScript(t, filepath.Join(repoDir, "sync.sh"), "touch "+syncMarker)
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-m", "add sync.sh")

	// Another later commit that does NOT touch setup.sh -- moves
	// workspaceMoved, keeps delta eligibility.
	writeFileHelper(t, filepath.Join(repoDir, "unrelated.txt"), "unrelated")
	runGit(t, repoDir, "add", "unrelated.txt")
	runGit(t, repoDir, "commit", "-m", "unrelated change")

	currentSHA := gitRevParseHEAD(t, repoDir)
	manifest := boot.ImageManifest{
		BuiltRepoShas: map[string]string{"repo1": builtSHA},
		// No DependencyManifestDigests entry at all -- digest tier must be
		// ineligible, falling through to the delta tier.
	}
	currentSHAs := map[string]string{"repo1": currentSHA}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, currentSHAs)
	if !workspaceMoved["repo1"] {
		t.Fatalf("precondition failed: workspaceMoved[repo1] = false, want true")
	}
	gitDirRoot := t.TempDir()
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}
	ladderLayout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	ladderSup := supervisor.New()
	if err := gitdir.Seed(context.Background(), ladderSup, ladderLayout.Repo("repo1"), "https://example.invalid/repo1.git", nil, 5*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed() error = %v", err)
	}
	ladder := boot.ComputeSetupRerunLadder(context.Background(), ladderSup, ladderLayout, nil, manifest, true, false, workspaceDir, currentSHAs, 5*time.Second, 5*time.Second)
	if !ladder["repo1"].DeltaEligible {
		t.Fatalf("precondition failed: ladder[repo1].DeltaEligible = false, want true (setup.sh was never touched since builtSHA)")
	}

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved, ladder, nil,
		noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil", err)
	}

	assertFileExists(t, syncMarker)
	assertFileAbsent(t, setupMarker)
}

// TestSetupRerunLadder_DeltaFails_FallsBackToFullSetup proves the failure
// ladder's own explicit step: a delta script that fails must not fail the
// boot, and must fall back to a full, real setup.sh rerun -- both markers
// end up present (sync.sh ran and failed; setup.sh then ran and
// succeeded), and RunBoot itself still reports success.
func TestSetupRerunLadder_DeltaFails_FallsBackToFullSetup(t *testing.T) {
	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	setupMarker := filepath.Join(workspaceDir, "setup-should-run")

	builtSHA := buildRepoAtBuiltSHA(t, repoDir, setupMarker, "")
	writeScript(t, filepath.Join(repoDir, "sync.sh"), "exit 1")
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-m", "add failing sync.sh")

	writeFileHelper(t, filepath.Join(repoDir, "unrelated.txt"), "unrelated")
	runGit(t, repoDir, "add", "unrelated.txt")
	runGit(t, repoDir, "commit", "-m", "unrelated change")

	currentSHA := gitRevParseHEAD(t, repoDir)
	manifest := boot.ImageManifest{BuiltRepoShas: map[string]string{"repo1": builtSHA}}
	currentSHAs := map[string]string{"repo1": currentSHA}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, currentSHAs)
	gitDirRoot := t.TempDir()
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}
	ladderLayout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	ladderSup := supervisor.New()
	if err := gitdir.Seed(context.Background(), ladderSup, ladderLayout.Repo("repo1"), "https://example.invalid/repo1.git", nil, 5*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed() error = %v", err)
	}
	ladder := boot.ComputeSetupRerunLadder(context.Background(), ladderSup, ladderLayout, nil, manifest, true, false, workspaceDir, currentSHAs, 5*time.Second, 5*time.Second)
	if !ladder["repo1"].DeltaEligible {
		t.Fatalf("precondition failed: ladder[repo1].DeltaEligible = false, want true")
	}

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved, ladder, nil,
		noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil (a delta-script failure must be non-fatal and fall back to full setup.sh)", err)
	}

	assertFileExists(t, setupMarker)
}

// TestSetupRerunLadder_DeltaIneligible_SetupChanged_RunsFullSetup proves
// the delta tier's own conservative "no": setup.sh itself changed since
// the built SHA, so the delta script (even though present) must never run
// -- only full setup.sh does.
func TestSetupRerunLadder_DeltaIneligible_SetupChanged_RunsFullSetup(t *testing.T) {
	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	setupMarker := filepath.Join(workspaceDir, "setup-should-run")
	syncMarker := filepath.Join(workspaceDir, "sync-should-not-run")

	builtSHA := buildRepoAtBuiltSHA(t, repoDir, setupMarker, "")
	writeScript(t, filepath.Join(repoDir, "sync.sh"), "touch "+syncMarker)
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-m", "add sync.sh")

	// This commit CHANGES setup.sh itself -- delta must become ineligible.
	writeScript(t, filepath.Join(repoDir, "setup.sh"), "touch "+setupMarker+" # changed")
	runGit(t, repoDir, "add", "setup.sh")
	runGit(t, repoDir, "commit", "-m", "change setup.sh")

	currentSHA := gitRevParseHEAD(t, repoDir)
	manifest := boot.ImageManifest{BuiltRepoShas: map[string]string{"repo1": builtSHA}}
	currentSHAs := map[string]string{"repo1": currentSHA}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, currentSHAs)
	gitDirRoot := t.TempDir()
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}
	ladderLayout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	ladderSup := supervisor.New()
	if err := gitdir.Seed(context.Background(), ladderSup, ladderLayout.Repo("repo1"), "https://example.invalid/repo1.git", nil, 5*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed() error = %v", err)
	}
	ladder := boot.ComputeSetupRerunLadder(context.Background(), ladderSup, ladderLayout, nil, manifest, true, false, workspaceDir, currentSHAs, 5*time.Second, 5*time.Second)
	if ladder["repo1"].DeltaEligible {
		t.Fatalf("precondition failed: ladder[repo1].DeltaEligible = true, want false (setup.sh WAS changed since builtSHA)")
	}

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved, ladder, nil,
		noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil", err)
	}

	assertFileExists(t, setupMarker)
	assertFileAbsent(t, syncMarker)
}

// TestSetupRerunLadder_DigestMismatch_FallsThroughToFullSetup proves
// DependencySkipMismatch (a REAL baked digest that disagrees with the
// recomputed one, distinct from DependencySkipIneligible's "nothing to
// compare at all") still falls through the ladder exactly like an
// ineligible digest does: with no sync.sh present, full setup.sh runs, and
// -- per §19.6's own explicit "the reason changes but the rule still
// holds" instruction -- this is STILL non-fatal even though the mismatch
// is positive proof dependencies moved.
func TestSetupRerunLadder_DigestMismatch_FallsThroughToFullSetup(t *testing.T) {
	originalLogger := slog.Default()
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(originalLogger)

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	setupMarker := filepath.Join(workspaceDir, "setup-should-run")

	builtSHA := buildRepoAtBuiltSHA(t, repoDir, setupMarker, `{"lockfileVersion":1}`)

	// A later commit that changes package-lock.json's own content -- a
	// GENUINE dependency change, moving both workspaceMoved and the
	// recomputed digest away from whatever was baked.
	writeFileHelper(t, filepath.Join(repoDir, "package-lock.json"), `{"lockfileVersion":2}`)
	runGit(t, repoDir, "add", "package-lock.json")
	runGit(t, repoDir, "commit", "-m", "bump dependency")

	currentSHA := gitRevParseHEAD(t, repoDir)
	manifest := boot.ImageManifest{
		BuiltRepoShas: map[string]string{"repo1": builtSHA},
		// A real, well-formed digest -- but for the OLD lockfile content,
		// deliberately different from what ComputeDependencyManifestDigest
		// will recompute against the tree above.
		DependencyManifestDigests: map[string]string{"repo1": mustDigestForContent(t, `{"lockfileVersion":1}`)},
	}
	currentSHAs := map[string]string{"repo1": currentSHA}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, currentSHAs)
	gitDirRoot := t.TempDir()
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}
	ladderLayout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	ladderSup := supervisor.New()
	if err := gitdir.Seed(context.Background(), ladderSup, ladderLayout.Repo("repo1"), "https://example.invalid/repo1.git", nil, 5*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed() error = %v", err)
	}
	ladder := boot.ComputeSetupRerunLadder(context.Background(), ladderSup, ladderLayout, nil, manifest, true, false, workspaceDir, currentSHAs, 5*time.Second, 5*time.Second)
	if ladder["repo1"].DependencySkip != boot.DependencySkipMismatch {
		t.Fatalf("precondition failed: ladder[repo1].DependencySkip = %q, want %q", ladder["repo1"].DependencySkip, boot.DependencySkipMismatch)
	}

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved, ladder, nil,
		noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil (a proven digest mismatch must still be non-fatal)", err)
	}

	assertFileExists(t, setupMarker)

	logged := logBuf.String()
	if !strings.Contains(logged, `"digest_outcome":"mismatch"`) {
		t.Errorf("log output missing digest_outcome=mismatch; full log:\n%s", logged)
	}
}

// TestSetupRerunLadder_DigestMatchButSetupChanged_RunsFullSetup proves the
// B3 adversarial-review fix directly, end to end: package-lock.json stays
// byte-identical since the built SHA (the digest tier's own evidence says
// "unchanged"), but setup.sh ITSELF was edited in the same later commit --
// a digest match must NEVER skip setup.sh's rerun in that case, because the
// digest can only speak for the dependency-manifest surface, never for
// setup.sh's own non-package-manager work (§19.4: "may provision local
// service stacks, run codegen, seed local state"). The pre-fix ladder would
// have returned on DependencySkipMatch alone and silently skipped the
// CHANGED setup.sh entirely.
func TestSetupRerunLadder_DigestMatchButSetupChanged_RunsFullSetup(t *testing.T) {
	originalLogger := slog.Default()
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(originalLogger)

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	setupMarkerV1 := filepath.Join(workspaceDir, "setup-v1-should-not-run")
	setupMarkerV2 := filepath.Join(workspaceDir, "setup-v2-should-run")

	builtSHA := buildRepoAtBuiltSHA(t, repoDir, setupMarkerV1, `{"lockfileVersion":1}`)

	bakedDigest, found, err := boot.ComputeDependencyManifestDigest(repoDir)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest() error = %v", err)
	}
	if !found {
		t.Fatal("precondition failed: found = false, want true (repoDir has a real package-lock.json)")
	}

	// A later commit that changes setup.sh ITSELF (a new version, touching
	// a DIFFERENT marker so the test can tell which version actually ran)
	// but leaves package-lock.json byte-identical -- the digest tier's own
	// evidence still matches, but setup.sh's own logic moved.
	writeScript(t, filepath.Join(repoDir, "setup.sh"), "touch "+setupMarkerV2)
	runGit(t, repoDir, "add", "setup.sh")
	runGit(t, repoDir, "commit", "-m", "change setup.sh, keep package-lock.json")

	currentSHA := gitRevParseHEAD(t, repoDir)
	manifest := boot.ImageManifest{
		BuiltRepoShas:             map[string]string{"repo1": builtSHA},
		DependencyManifestDigests: map[string]string{"repo1": bakedDigest},
	}
	currentSHAs := map[string]string{"repo1": currentSHA}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, currentSHAs)
	if !workspaceMoved["repo1"] {
		t.Fatalf("precondition failed: workspaceMoved[repo1] = false, want true")
	}
	gitDirRoot := t.TempDir()
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}
	ladderLayout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	ladderSup := supervisor.New()
	if err := gitdir.Seed(context.Background(), ladderSup, ladderLayout.Repo("repo1"), "https://example.invalid/repo1.git", nil, 5*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed() error = %v", err)
	}
	ladder := boot.ComputeSetupRerunLadder(context.Background(), ladderSup, ladderLayout, nil, manifest, true, false, workspaceDir, currentSHAs, 5*time.Second, 5*time.Second)
	if ladder["repo1"].DependencySkip != boot.DependencySkipMatch {
		t.Fatalf("precondition failed: ladder[repo1].DependencySkip = %q, want %q (package-lock.json is unchanged)",
			ladder["repo1"].DependencySkip, boot.DependencySkipMatch)
	}
	if ladder["repo1"].DeltaEligible {
		t.Fatalf("precondition failed: ladder[repo1].DeltaEligible = true, want false (setup.sh WAS changed since builtSHA)")
	}

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	err = boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved, ladder, nil,
		noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil", err)
	}

	// The CHANGED setup.sh (v2) must have actually run -- a digest match
	// alone must never have skipped it.
	assertFileExists(t, setupMarkerV2)
	assertFileAbsent(t, setupMarkerV1)

	logged := logBuf.String()
	if !strings.Contains(logged, `"digest_outcome":"match"`) {
		t.Errorf("log output missing digest_outcome=match; full log:\n%s", logged)
	}
	if !strings.Contains(logged, `"setup_sh_unchanged":false`) {
		t.Errorf("log output missing setup_sh_unchanged=false; full log:\n%s", logged)
	}
	if !strings.Contains(logged, `"tier":"full"`) {
		t.Errorf("log output missing a full-setup.sh tier decision; full log:\n%s", logged)
	}
}

// TestSetupRerunLadder_ScopedSession_DigestTierAlwaysIneligible proves the
// B5 adversarial-review fix (§19.7): under a scoped Environment, the
// digest tier must resolve to Ineligible regardless of what the
// (necessarily scope-truncated) recompute would find -- even when, as
// here, the baked digest and an UNSCOPED recompute would have matched
// perfectly (this is otherwise the identical scenario to
// TestSetupRerunLadder_DigestMatch_SkipsSetupEntirely above). Shared
// images are always built unscoped (§19.1: "No sparse-checkout at build
// time, ever"), so a scoped boot's own on-disk tree can never be trusted
// as complete evidence for this tier, and the ladder must fall through to
// the tiers below exactly as if no baked digest existed at all -- never a
// false "match", and (§19.7's own explicit requirement) never a false
// "mismatch" either.
func TestSetupRerunLadder_ScopedSession_DigestTierAlwaysIneligible(t *testing.T) {
	originalLogger := slog.Default()
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(originalLogger)

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	setupMarker := filepath.Join(workspaceDir, "setup-should-run")

	builtSHA := buildRepoAtBuiltSHA(t, repoDir, setupMarker, `{"lockfileVersion":1}`)

	bakedDigest, found, err := boot.ComputeDependencyManifestDigest(repoDir)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest() error = %v", err)
	}
	if !found {
		t.Fatal("precondition failed: found = false, want true (repoDir has a real package-lock.json)")
	}

	// An unrelated later commit -- moves workspaceMoved, never touches
	// package-lock.json: an UNSCOPED recompute at this point would match
	// bakedDigest exactly.
	writeFileHelper(t, filepath.Join(repoDir, "unrelated.txt"), "unrelated")
	runGit(t, repoDir, "add", "unrelated.txt")
	runGit(t, repoDir, "commit", "-m", "unrelated change")

	currentSHA := gitRevParseHEAD(t, repoDir)
	manifest := boot.ImageManifest{
		BuiltRepoShas:             map[string]string{"repo1": builtSHA},
		DependencyManifestDigests: map[string]string{"repo1": bakedDigest},
	}
	currentSHAs := map[string]string{"repo1": currentSHA}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, currentSHAs)
	if !workspaceMoved["repo1"] {
		t.Fatalf("precondition failed: workspaceMoved[repo1] = false, want true")
	}

	// scoped=true is the ONLY difference from
	// TestSetupRerunLadder_DigestMatch_SkipsSetupEntirely's own identical
	// setup, which resolves to DependencySkipMatch and skips setup.sh
	// entirely.
	gitDirRoot := t.TempDir()
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}
	ladderLayout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	ladderSup := supervisor.New()
	if err := gitdir.Seed(context.Background(), ladderSup, ladderLayout.Repo("repo1"), "https://example.invalid/repo1.git", nil, 5*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed() error = %v", err)
	}
	ladder := boot.ComputeSetupRerunLadder(context.Background(), ladderSup, ladderLayout, nil, manifest, true, true, workspaceDir, currentSHAs, 5*time.Second, 5*time.Second)
	if ladder["repo1"].DependencySkip != boot.DependencySkipIneligible {
		t.Fatalf("ladder[repo1].DependencySkip = %q, want %q (a scoped session must never trust the digest tier, even on what looks like an exact match)",
			ladder["repo1"].DependencySkip, boot.DependencySkipIneligible)
	}

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	err = boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved, ladder, nil,
		noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil", err)
	}

	// The ladder must fall all the way through (digest ineligible, no
	// sync.sh present) to the full setup.sh floor.
	assertFileExists(t, setupMarker)

	logged := logBuf.String()
	if !strings.Contains(logged, `"tier":"digest"`) || !strings.Contains(logged, `"outcome":"ineligible-fallback"`) {
		t.Errorf("log output missing a digest-tier ineligible-fallback decision; full log:\n%s", logged)
	}
	if strings.Contains(logged, `"digest_outcome":"match"`) {
		t.Errorf("log output claims digest_outcome=match for a scoped session -- must never be logged as a trustworthy match; full log:\n%s", logged)
	}
	if strings.Contains(logged, `"digest_outcome":"mismatch"`) {
		t.Errorf("log output claims digest_outcome=mismatch for a scoped session -- §19.7 requires never a false mismatch; full log:\n%s", logged)
	}
}

// mustDigestForContent computes the digest a repo whose ONLY lockfile is
// package-lock.json with exactly this content would produce -- by writing
// it to a throwaway directory and calling the real, exported
// ComputeDependencyManifestDigest, never a hand-rolled reimplementation of
// its own hashing scheme.
func mustDigestForContent(t *testing.T, packageLockContent string) string {
	t.Helper()
	dir := t.TempDir()
	writeFileHelper(t, filepath.Join(dir, "package-lock.json"), packageLockContent)
	digest, found, err := boot.ComputeDependencyManifestDigest(dir)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest() error = %v", err)
	}
	if !found {
		t.Fatal("precondition failed: found = false, want true")
	}
	return digest
}

// TestSetupRerunLadder_LogsStructuredDecisionsForEachTier proves §19.6's
// own explicit instruction: every ladder decision (skip / delta / full /
// ineligible-fallback) logs a structured reason, individually auditable.
// Uses the delta-eligible scenario (two tiers actually consulted: digest
// falls through ineligible-fallback since no baked digest exists, delta
// then runs).
func TestSetupRerunLadder_LogsStructuredDecisionsForEachTier(t *testing.T) {
	originalLogger := slog.Default()
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(originalLogger)

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	setupMarker := filepath.Join(workspaceDir, "setup-marker")
	syncMarker := filepath.Join(workspaceDir, "sync-marker")

	builtSHA := buildRepoAtBuiltSHA(t, repoDir, setupMarker, "")
	writeScript(t, filepath.Join(repoDir, "sync.sh"), "touch "+syncMarker)
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-m", "add sync.sh")

	writeFileHelper(t, filepath.Join(repoDir, "unrelated.txt"), "unrelated")
	runGit(t, repoDir, "add", "unrelated.txt")
	runGit(t, repoDir, "commit", "-m", "unrelated change")

	currentSHA := gitRevParseHEAD(t, repoDir)
	manifest := boot.ImageManifest{BuiltRepoShas: map[string]string{"repo1": builtSHA}}
	currentSHAs := map[string]string{"repo1": currentSHA}
	workspaceMoved := boot.ComputeWorkspaceMoved(manifest, true, currentSHAs)
	gitDirRoot := t.TempDir()
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}
	ladderLayout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	ladderSup := supervisor.New()
	if err := gitdir.Seed(context.Background(), ladderSup, ladderLayout.Repo("repo1"), "https://example.invalid/repo1.git", nil, 5*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed() error = %v", err)
	}
	ladder := boot.ComputeSetupRerunLadder(context.Background(), ladderSup, ladderLayout, nil, manifest, true, false, workspaceDir, currentSHAs, 5*time.Second, 5*time.Second)

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo1", Primary: true}}

	if err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeRepoImage, workspaceMoved, ladder, nil,
		noopReporter, noopHookRerunTiming, 10*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil); err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil", err)
	}

	logged := logBuf.String()
	for _, want := range []string{
		`"boot: setup-rerun ladder decision"`,
		`"tier":"digest"`,
		`"outcome":"ineligible-fallback"`,
		`"tier":"delta"`,
		`"outcome":"delta"`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log output missing %q; full log:\n%s", want, logged)
		}
	}
}
