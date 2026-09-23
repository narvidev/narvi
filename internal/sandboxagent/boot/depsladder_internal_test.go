package boot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// seedTestRepo builds a fresh agent-owned git-dir (a temp root, EnsureRoot
// + Seed) for the already-existing, non-bare git repo at wt, and returns
// the resulting githarden.Repo plus a Supervisor callers can drive
// setupUnchangedSinceBuild's own gitdir.Run choke point through. cred is
// always nil here -- these tests run as the same uid throughout (no real
// runtime/agent identity split), matching every other gitdir-adjacent
// test's own "cred == nil (self) in tests" convention.
func seedTestRepo(t *testing.T, wt string) (githarden.Repo, *supervisor.Supervisor) {
	t.Helper()
	root := t.TempDir()
	if err := gitdir.EnsureRoot(root); err != nil {
		t.Fatalf("gitdir.EnsureRoot: %v", err)
	}
	layout := gitdir.Layout{Root: root, WorkspaceDir: filepath.Dir(wt)}
	repo := layout.Repo(filepath.Base(wt))
	sup := supervisor.New()
	if err := gitdir.Seed(context.Background(), sup, repo, "https://example.invalid/repo.git", nil, 5*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed: %v", err)
	}
	return repo, sup
}

// mkdirAllInternal/initGitRepoInternal/runGitInternal duplicate
// fingerprint_test.go's own boot_test-package helpers of almost the same
// name -- a DIFFERENT Go package (boot vs boot_test) even though it lives
// in the same directory, so nothing there is visible here; duplicated
// rather than shared, exactly like runboot_test.go's own freePort
// precedent already establishes for this identical situation.
func mkdirAllInternal(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
}

func runGitInternal(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initGitRepoInternal(t *testing.T, dir string) {
	t.Helper()
	runGitInternal(t, dir, "init")
	runGitInternal(t, dir, "config", "user.email", "test@example.com")
	runGitInternal(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitInternal(t, dir, "add", "file.txt")
	runGitInternal(t, dir, "commit", "-m", "initial commit")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestComputeDependencyManifestDigest_EmptyRepoReportsNotFound proves the
// B1 adversarial-review fix directly: a repo with ZERO recognized
// manifests anywhere reports found: false and digest: "" (never a
// well-defined "digest of nothing" a caller could mistake for real
// evidence), deterministically across repeated calls, and never an error.
func TestComputeDependencyManifestDigest_EmptyRepoReportsNotFound(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	digest1, found1, err := ComputeDependencyManifestDigest(dir)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest() error = %v, want nil", err)
	}
	if found1 {
		t.Error("found = true for a repo with zero recognized manifests, want false")
	}
	if digest1 != "" {
		t.Errorf("digest = %q for found=false, want empty string", digest1)
	}

	digest2, found2, err := ComputeDependencyManifestDigest(dir)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest() error = %v, want nil", err)
	}
	if found2 || digest2 != "" {
		t.Errorf("second call not deterministic with first: (digest=%q,found=%v) != (digest=%q,found=%v)", digest2, found2, digest1, found1)
	}
}

// TestComputeDependencyManifestDigest_ContentChangeChangesDigest is the
// MUTATION-detecting core of §19.6's first bullet: two repos differing
// ONLY in one lockfile's own byte content must produce different digests --
// otherwise the skip tier could wrongly treat a genuine dependency bump as
// unchanged.
func TestComputeDependencyManifestDigest_ContentChangeChangesDigest(t *testing.T) {
	t.Parallel()

	dirA := t.TempDir()
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "package-lock.json"), []byte(`{"lockfileVersion":1}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "package-lock.json"), []byte(`{"lockfileVersion":2}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	digestA, foundA, err := ComputeDependencyManifestDigest(dirA)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest(dirA) error = %v", err)
	}
	digestB, foundB, err := ComputeDependencyManifestDigest(dirB)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest(dirB) error = %v", err)
	}
	if !foundA || !foundB {
		t.Fatalf("found = (%v, %v), want (true, true)", foundA, foundB)
	}
	if digestA == digestB {
		t.Errorf("digests equal for different lockfile content: %q", digestA)
	}
}

// TestComputeDependencyManifestDigest_PresenceMattersNotJustContent proves
// dropping a lockfile entirely changes the digest even though every
// REMAINING file's own content is byte-identical -- otherwise deleting a
// package manager's lockfile (a genuine dependency-surface change) could be
// silently invisible to the skip tier.
func TestComputeDependencyManifestDigest_PresenceMattersNotJustContent(t *testing.T) {
	t.Parallel()

	dirWith := t.TempDir()
	dirWithout := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirWith, "go.sum"), []byte("same-content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// dirWithout has no go.sum at all.

	digestWith, foundWith, err := ComputeDependencyManifestDigest(dirWith)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest(dirWith) error = %v", err)
	}
	if !foundWith {
		t.Fatal("found = false for a repo with a real go.sum, want true")
	}
	digestWithout, foundWithout, err := ComputeDependencyManifestDigest(dirWithout)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest(dirWithout) error = %v", err)
	}
	if foundWithout {
		t.Fatal("found = true for a repo with no go.sum at all, want false")
	}
	if digestWith == digestWithout {
		t.Errorf("digests equal despite go.sum being present in one and absent in the other: %q", digestWith)
	}
}

// TestComputeDependencyManifestDigest_UnreadableFilePropagatesError proves
// a genuine I/O failure (not "does not exist") on a lockfile that DOES
// exist is returned as a real error -- §19.6's own "unreadable... digest...
// means ineligible" case depends on this error actually surfacing.
func TestComputeDependencyManifestDigest_UnreadableFilePropagatesError(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("running as root -- file permissions are not enforced")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "go.sum")
	if err := os.WriteFile(path, []byte("content"), 0o000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_, found, err := ComputeDependencyManifestDigest(dir)
	if err == nil {
		t.Fatal("ComputeDependencyManifestDigest() error = nil, want a real error for an unreadable lockfile")
	}
	if found {
		t.Error("found = true alongside a non-nil error, want false")
	}
}

// TestComputeDependencyManifestDigest_DiscoversNestedManifests proves the
// B2 adversarial-review fix: a manifest nested under a subdirectory (a
// monorepo's own per-component lockfile, e.g. web/package-lock.json beside
// a root go.sum) is discovered by the recursive walk, not just a repo's own
// root -- the pre-fix root-only scan would have reported found: false here.
func TestComputeDependencyManifestDigest_DiscoversNestedManifests(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdirAllInternal(t, filepath.Join(dir, "web"))
	if err := os.WriteFile(filepath.Join(dir, "web", "package-lock.json"), []byte(`{"lockfileVersion":1}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	digest, found, err := ComputeDependencyManifestDigest(dir)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest() error = %v", err)
	}
	if !found {
		t.Fatal("found = false for a repo with a nested web/package-lock.json, want true")
	}
	if digest == "" {
		t.Error("digest is empty despite found = true")
	}
}

// TestComputeDependencyManifestDigest_RelocationChangesDigest proves the
// B2 fix's other half: the digest folds in each manifest's own PATH, not
// just its basename and content -- moving a lockfile from the repo root to
// a subdirectory (or vice versa) must change the digest even when its own
// bytes stay identical, so a monorepo restructuring is never invisible to
// this tier.
func TestComputeDependencyManifestDigest_RelocationChangesDigest(t *testing.T) {
	t.Parallel()

	const content = `{"lockfileVersion":1}`

	atRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(atRoot, "package-lock.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	nested := t.TempDir()
	mkdirAllInternal(t, filepath.Join(nested, "sub"))
	if err := os.WriteFile(filepath.Join(nested, "sub", "package-lock.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	digestRoot, foundRoot, err := ComputeDependencyManifestDigest(atRoot)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest(atRoot) error = %v", err)
	}
	digestNested, foundNested, err := ComputeDependencyManifestDigest(nested)
	if err != nil {
		t.Fatalf("ComputeDependencyManifestDigest(nested) error = %v", err)
	}
	if !foundRoot || !foundNested {
		t.Fatalf("found = (%v, %v), want (true, true)", foundRoot, foundNested)
	}
	if digestRoot == digestNested {
		t.Errorf("digests equal despite package-lock.json living at different relative paths: %q", digestRoot)
	}
}

// TestComputeDependencyManifestDigest_SkipsWellKnownVendorDirectories
// proves the walk's own bound: a manifest-shaped file nested inside
// node_modules/ or .git/ (dependencyManifestSkipDirs) is never discovered
// -- both because these trees can be enormous (the walk must stay cheap)
// and because their own contents are never a repo's own real dependency
// surface.
func TestComputeDependencyManifestDigest_SkipsWellKnownVendorDirectories(t *testing.T) {
	t.Parallel()

	for _, skipDir := range []string{"node_modules", "vendor", ".git", "dist", "build", "target", "venv", ".venv", "__pycache__", "bower_components"} {
		skipDir := skipDir
		t.Run(skipDir, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			nestedDir := filepath.Join(dir, skipDir, "nested")
			mkdirAllInternal(t, nestedDir)
			if err := os.WriteFile(filepath.Join(nestedDir, "go.sum"), []byte("content"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			_, found, err := ComputeDependencyManifestDigest(dir)
			if err != nil {
				t.Fatalf("ComputeDependencyManifestDigest() error = %v", err)
			}
			if found {
				t.Errorf("found = true for a go.sum nested only under %s/, want false (skip-dir not honored)", skipDir)
			}
		})
	}
}

func TestIsRecognizedDigestHex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"valid sha256 hex", sha256Hex([]byte("anything")), true},
		{"empty string", "", false},
		{"too short", "abcd", false},
		{"right length but uppercase", strings.ToUpper(sha256Hex([]byte("anything"))), false},
		{"right length but non-hex char", "g" + sha256Hex([]byte("x"))[1:], false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isRecognizedDigestHex(tc.in); got != tc.want {
				t.Errorf("isRecognizedDigestHex(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestEvaluateDependencySkip is the pure decision table behind
// DependencySkipOutcome -- every branch of §19.6's own "unreadable, absent,
// or unrecognized... means ineligible" rule, plus the match/mismatch split,
// plus the B1 (currentFound: false) and B5 (scoped: true) adversarial-review
// additions -- both of which must win over an otherwise-valid match.
func TestEvaluateDependencySkip(t *testing.T) {
	t.Parallel()

	validDigest := sha256Hex([]byte("baked"))
	otherDigest := sha256Hex([]byte("current"))

	tests := []struct {
		name          string
		manifestFound bool
		bakedDigest   string
		bakedOK       bool
		currentDigest string
		currentFound  bool
		currentErr    error
		scoped        bool
		want          DependencySkipOutcome
	}{
		{"manifest not found", false, validDigest, true, validDigest, true, nil, false, DependencySkipIneligible},
		{"repo has no baked entry", true, "", false, validDigest, true, nil, false, DependencySkipIneligible},
		{"baked digest not recognized hex", true, "not-a-digest", true, validDigest, true, nil, false, DependencySkipIneligible},
		{"current digest compute error", true, validDigest, true, "", true, errComputeFailed, false, DependencySkipIneligible},
		{"digests match", true, validDigest, true, validDigest, true, nil, false, DependencySkipMatch},
		{"digests mismatch", true, validDigest, true, otherDigest, true, nil, false, DependencySkipMismatch},
		// B1: a boot-side scan that found ZERO recognized manifests must
		// never be compared, even when every other input looks like it
		// would otherwise produce a match (bakedDigest == currentDigest,
		// both being the pre-fix "empty scan" constant) -- currentFound:
		// false wins unconditionally.
		{"B1: current scan found nothing", true, validDigest, true, "", false, nil, false, DependencySkipIneligible},
		{"B1: current scan found nothing, digests coincidentally equal", true, validDigest, true, validDigest, false, nil, false, DependencySkipIneligible},
		// B5: a scoped session must never resolve to Match OR Mismatch,
		// even when the (necessarily scope-truncated) recompute otherwise
		// looks like a clean match or a clean, "provable" mismatch --
		// scoped: true wins unconditionally, over every other input.
		{"B5: scoped session, digests would otherwise match", true, validDigest, true, validDigest, true, nil, true, DependencySkipIneligible},
		{"B5: scoped session, digests would otherwise mismatch", true, validDigest, true, otherDigest, true, nil, true, DependencySkipIneligible},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := evaluateDependencySkip(tc.manifestFound, tc.bakedDigest, tc.bakedOK, tc.currentDigest, tc.currentFound, tc.currentErr, tc.scoped)
			if got != tc.want {
				t.Errorf("evaluateDependencySkip(%+v) = %q, want %q", tc, got, tc.want)
			}
		})
	}
}

var errComputeFailed = &staticError{"boom"}

type staticError struct{ msg string }

func (e *staticError) Error() string { return e.msg }

// TestSetupUnchangedSinceBuild_Unchanged proves the happy path: setup.sh
// byte-identical between the built SHA and HEAD reports (true, nil).
func TestSetupUnchangedSinceBuild_Unchanged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdirAllInternal(t, dir)
	initGitRepoInternal(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "setup.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitInternal(t, dir, "add", "setup.sh")
	runGitInternal(t, dir, "commit", "-m", "add setup.sh")
	builtSHA := gitHeadInternal(t, dir)

	// A later, unrelated commit that does NOT touch setup.sh.
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("other"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitInternal(t, dir, "add", "other.txt")
	runGitInternal(t, dir, "commit", "-m", "unrelated change")

	repo, sup := seedTestRepo(t, dir)
	unchanged, err := setupUnchangedSinceBuild(context.Background(), sup, repo, nil, builtSHA, 5*time.Second, 5*time.Second)
	if err != nil {
		t.Fatalf("setupUnchangedSinceBuild() error = %v, want nil", err)
	}
	if !unchanged {
		t.Error("setupUnchangedSinceBuild() = false, want true (setup.sh was never touched since builtSHA)")
	}
}

// TestSetupUnchangedSinceBuild_Changed proves a REAL setup.sh change since
// builtSHA reports (false, nil) -- a clean, definitive "no", not an error.
func TestSetupUnchangedSinceBuild_Changed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdirAllInternal(t, dir)
	initGitRepoInternal(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "setup.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitInternal(t, dir, "add", "setup.sh")
	runGitInternal(t, dir, "commit", "-m", "add setup.sh")
	builtSHA := gitHeadInternal(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "setup.sh"), []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitInternal(t, dir, "add", "setup.sh")
	runGitInternal(t, dir, "commit", "-m", "change setup.sh")

	repo, sup := seedTestRepo(t, dir)
	unchanged, err := setupUnchangedSinceBuild(context.Background(), sup, repo, nil, builtSHA, 5*time.Second, 5*time.Second)
	if err != nil {
		t.Fatalf("setupUnchangedSinceBuild() error = %v, want nil (a real diff is not an error)", err)
	}
	if unchanged {
		t.Error("setupUnchangedSinceBuild() = true, want false (setup.sh WAS changed since builtSHA)")
	}
}

// TestSetupUnchangedSinceBuild_UnresolvableSHAIsAnError proves §19.6's own
// "any git error on this check is conservative: ineligible" rule: a
// builtSHA git cannot resolve at all (not a real diff, a genuine failure)
// must surface as a non-nil error, never silently treated as "unchanged".
func TestSetupUnchangedSinceBuild_UnresolvableSHAIsAnError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdirAllInternal(t, dir)
	initGitRepoInternal(t, dir)

	repo, sup := seedTestRepo(t, dir)
	_, err := setupUnchangedSinceBuild(context.Background(), sup, repo, nil, "0000000000000000000000000000000000000000", 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatal("setupUnchangedSinceBuild() error = nil, want a real error for an unresolvable builtSHA")
	}
}

// TestSetupUnchangedSinceBuild_RejectsMalformedBuiltSHA is C1's own
// mutation-detecting core: builtSHA sits BEFORE setupUnchangedSinceBuild's
// own "--" separator in the `git diff --quiet <builtSHA> HEAD -- setup.sh`
// invocation, i.e. in git's own option/revision zone, so a malformed value
// (most dangerously one beginning with "-", which git's own argument parser
// would otherwise consume as an OPTION rather than a revision) must never
// reach exec.CommandContext at all -- builtSHAPattern's own doc comment has
// the full "why '--' does not protect this argument" reasoning. Every
// invalid case here asserts on the VALIDATION error's own distinctive
// message (rather than merely "err != nil"), so this test would fail if the
// gate were ever removed and a downstream git-level error masqueraded as
// the same "ineligible" outcome for a different reason.
func TestSetupUnchangedSinceBuild_RejectsMalformedBuiltSHA(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdirAllInternal(t, dir)
	initGitRepoInternal(t, dir)

	tests := []struct {
		name     string
		builtSHA string
	}{
		{"leading dash (option-shaped)", "-upload-pack=touch /tmp/pwned-0000000"},
		{"empty string", ""},
		{"non-hex character", "g23456789012345678901234567890123456789"},
		{"over-long value", strings.Repeat("a", 41)},
		{"under-long value", strings.Repeat("a", 39)},
	}
	repo, sup := seedTestRepo(t, dir)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := setupUnchangedSinceBuild(context.Background(), sup, repo, nil, tc.builtSHA, 5*time.Second, 5*time.Second)
			if err == nil {
				t.Fatalf("setupUnchangedSinceBuild(builtSHA=%q) error = nil, want a validation error", tc.builtSHA)
			}
			if !strings.Contains(err.Error(), "not a well-formed git object id") {
				t.Errorf("setupUnchangedSinceBuild(builtSHA=%q) error = %v, want the builtSHAPattern validation error specifically (not a downstream git error)", tc.builtSHA, err)
			}
		})
	}
}

// TestSetupUnchangedSinceBuild_AcceptsValidFullSHA is the companion "zero
// regression" half: a well-formed, resolvable 40-character hex sha must
// still pass validation and reach the real git diff -- proving
// builtSHAPattern's own gate never rejects the one shape a real build
// service actually produces.
func TestSetupUnchangedSinceBuild_AcceptsValidFullSHA(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdirAllInternal(t, dir)
	initGitRepoInternal(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "setup.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitInternal(t, dir, "add", "setup.sh")
	runGitInternal(t, dir, "commit", "-m", "add setup.sh")
	builtSHA := gitHeadInternal(t, dir)

	if len(builtSHA) != 40 {
		t.Fatalf("precondition failed: git rev-parse HEAD returned %q (%d chars), want a 40-character sha", builtSHA, len(builtSHA))
	}

	repo, sup := seedTestRepo(t, dir)
	unchanged, err := setupUnchangedSinceBuild(context.Background(), sup, repo, nil, builtSHA, 5*time.Second, 5*time.Second)
	if err != nil {
		t.Fatalf("setupUnchangedSinceBuild() error = %v, want nil (a well-formed, resolvable sha must pass validation)", err)
	}
	if !unchanged {
		t.Error("setupUnchangedSinceBuild() = false, want true (setup.sh was never touched since builtSHA)")
	}
}

func gitHeadInternal(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func writeScriptInternal(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// noopHookRerunTimingInternal is this file's own package-boot copy of
// runboot_test.go's own noopReporter precedent (a DIFFERENT Go package,
// boot_test, invisible here -- see this file's own top comment) -- a §33.3
// OnHookRerunTiming this file's one white-box runSetupRerunLadder call
// below does not care to observe.
func noopHookRerunTimingInternal(_, _, _ string, _, _ bool, _ float64) {
}

// TestRunSetupRerunLadder_ConsultsHookDeltaPolicy proves the B4
// adversarial-review fix directly, white-box: runSetupRerunLadder's own
// delta-tier gate consults sandboxboot.EvaluateHook's HookDelta policy row,
// not just ladder.DeltaEligible, before ever spawning sync.sh.
//
// Calling this unexported function directly with moved=false is the only
// way to observe that consultation in isolation: at the one real call site
// (runRepoHooks), moved is always true by construction (it only enters
// this function inside its own `mode == BootModeRepoImage && moved`
// branch), so EvaluateHook's HookDelta row is always ShouldRun: true there
// too -- moved=false is the one input that can flip EvaluateHook's own
// verdict without touching ladder.DeltaEligible at all. If sync.sh still
// ran despite that, it would prove the policy table's own HookDelta row is
// dead code again -- the exact B4 finding this test exists to catch.
func TestRunSetupRerunLadder_ConsultsHookDeltaPolicy(t *testing.T) {
	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	mkdirAllInternal(t, repoDir)
	setupMarker := filepath.Join(workspaceDir, "setup-ran")
	syncMarker := filepath.Join(workspaceDir, "sync-ran")

	writeScriptInternal(t, filepath.Join(repoDir, "setup.sh"), "touch "+setupMarker)
	writeScriptInternal(t, filepath.Join(repoDir, "sync.sh"), "touch "+syncMarker)

	// DeltaEligible: true in isolation would normally send this straight
	// into the delta tier -- proving that moved=false (via EvaluateHook)
	// overrides that and forces the full-setup.sh floor instead.
	ladder := SetupRerunLadder{DependencySkip: DependencySkipIneligible, DeltaEligible: true}
	sup := supervisor.New()
	repo := RepoInfo{Name: "repo1", Primary: true}

	runSetupRerunLadder(context.Background(), sup, workspaceDir, repo, ladder, false, nil, noopHookRerunTimingInternal, 5*time.Second, time.Second, time.Millisecond)

	if _, err := os.Stat(syncMarker); err == nil {
		t.Error("sync.sh ran despite EvaluateHook(BootModeRepoImage, HookDelta, primary, moved=false).ShouldRun = false -- HookDelta policy row not actually consulted (B4 regression)")
	}
	if _, err := os.Stat(setupMarker); err != nil {
		t.Errorf("full setup.sh (the ladder's own floor) did not run: %v", err)
	}
}
