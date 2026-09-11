package gitclone

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// This file tests unexported call sites directly (applySparseCheckout,
// disableSparseCheckoutIfEnabled, runGit) -- the three places, besides
// cloneOne, where this package spawns a git command that can populate a
// working tree, and therefore the three places githarden.
// NeutralizeFiltersBestEffort must run before every spawn. sync_test.go/
// clone_test.go (package gitclone_test) cover the same protection
// end-to-end through SyncAll/CloneAll/CleanForImageBuild; this file
// isolates each call site instead, which is the only way to prove
// applySparseCheckout/disableSparseCheckoutIfEnabled specifically are
// covered -- through SyncAll, runGit's OWN check (inside checkoutBranch)
// always fires first and would mask a regression in either sparse-
// checkout function.

const itTimeout = 10 * time.Second
const itStopGrace = 2 * time.Second

func itRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir=%s) failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func itWriteScript(t *testing.T, path, body string) {
	t.Helper()
	content := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write script %s: %v", path, err)
	}
}

// itArmSparseRepo builds a repo with an unfiltered "always.txt" (always in
// scope) and a filtered "included.bin" (filter=evil), commits both, then
// narrows sparse-checkout to EXCLUDE included.bin -- so a later widen (or
// a disable) has to materialize included.bin FRESH, forcing a real smudge
// invocation rather than a same-content no-op.
func itArmSparseRepo(t *testing.T) (dir, marker string) {
	t.Helper()
	dir = t.TempDir()
	marker = filepath.Join(t.TempDir(), "smudge-ran")

	itRunGit(t, dir, "init", "-b", "main")
	itRunGit(t, dir, "config", "user.email", "t@t.com")
	itRunGit(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("included.bin filter=evil\n"), 0o644); err != nil {
		t.Fatalf("write .gitattributes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "always.txt"), []byte("always\n"), 0o644); err != nil {
		t.Fatalf("write always.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "included.bin"), []byte("payload\n"), 0o644); err != nil {
		t.Fatalf("write included.bin: %v", err)
	}
	itRunGit(t, dir, "add", ".")
	itRunGit(t, dir, "commit", "-m", "initial")

	smudge := filepath.Join(t.TempDir(), "smudge.sh")
	itWriteScript(t, smudge, "touch '"+marker+"'\ncat")
	itRunGit(t, dir, "config", "filter.evil.smudge", smudge)

	// Narrow to exclude included.bin -- removes it from the working tree,
	// so it is genuinely absent before the widen/disable under test.
	sup := supervisor.New()
	if err := applySparseCheckout(context.Background(), sup, dir, []string{"/always.txt"}, itTimeout, itStopGrace); err != nil {
		t.Fatalf("initial narrowing applySparseCheckout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "included.bin")); !os.IsNotExist(err) {
		t.Fatalf("included.bin stat = %v, want IsNotExist after narrowing", err)
	}

	return dir, marker
}

// TestApplySparseCheckout_ArmedFilterDoesNotRun gives applySparseCheckout
// its first real coverage against an armed content filter: widening scope
// to bring included.bin back materializes it fresh from its blob, which
// is exactly the operation a content filter hooks -- and confirms the
// armed filter.evil.smudge does not run.
func TestApplySparseCheckout_ArmedFilterDoesNotRun(t *testing.T) {
	dir, marker := itArmSparseRepo(t)

	sup := supervisor.New()
	if err := applySparseCheckout(context.Background(), sup, dir, []string{"/always.txt", "/included.bin"}, itTimeout, itStopGrace); err != nil {
		t.Fatalf("applySparseCheckout (widen) = %v, want nil", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "included.bin"))
	if err != nil {
		t.Fatalf("read included.bin: %v", err)
	}
	if string(data) != "payload\n" {
		t.Errorf("included.bin content = %q, want %q", data, "payload\n")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("armed filter.evil.smudge RAN during applySparseCheckout's own widen -- NeutralizeFiltersBestEffort was not applied where it needed to be")
	}
}

// TestApplySparseCheckout_AttributesSymlinked_FailsClosed is the
// fail-closed regression test at THIS call site specifically: a symlink
// pre-planted at .git/info/attributes (exactly what the agent runtime,
// owning this repository under §30.5, can do with no elevated access of
// its own) must make applySparseCheckout return a real, propagated error
// -- never silently proceed to spawn `git sparse-checkout set` with a
// write it could not vouch for.
func TestApplySparseCheckout_AttributesSymlinked_FailsClosed(t *testing.T) {
	dir, marker := itArmSparseRepo(t)

	infoDir := filepath.Join(dir, ".git", "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", infoDir, err)
	}
	// itArmSparseRepo's own initial narrowing call already ran
	// NeutralizeFiltersBestEffort once, so a real attributes file exists
	// here already -- os.Symlink (unlike `ln -sfn`) refuses to overwrite
	// an existing path, so it must be removed first.
	attrPath := filepath.Join(infoDir, "attributes")
	if err := os.Remove(attrPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove existing %s: %v", attrPath, err)
	}
	if err := os.Symlink("/dev/null", attrPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	sup := supervisor.New()
	err := applySparseCheckout(context.Background(), sup, dir, []string{"/always.txt", "/included.bin"}, itTimeout, itStopGrace)
	if err == nil {
		t.Fatal("applySparseCheckout returned nil with .git/info/attributes symlinked -- fail-closed is not wired up at this call site")
	}
	t.Logf("got the expected fail-closed error: %v", err)

	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("armed filter.evil.smudge RAN even though applySparseCheckout was supposed to fail closed before ever spawning git")
	}
}

// TestDisableSparseCheckoutIfEnabled_ArmedFilterDoesNotRun is
// TestApplySparseCheckout_ArmedFilterDoesNotRun's counterpart for the
// reverse direction: `sparse-checkout disable` re-materializes EVERY
// previously-excluded path, included.bin among them.
func TestDisableSparseCheckoutIfEnabled_ArmedFilterDoesNotRun(t *testing.T) {
	dir, marker := itArmSparseRepo(t)

	sup := supervisor.New()
	if err := disableSparseCheckoutIfEnabled(context.Background(), sup, dir, itTimeout, itStopGrace); err != nil {
		t.Fatalf("disableSparseCheckoutIfEnabled = %v, want nil", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "included.bin"))
	if err != nil {
		t.Fatalf("read included.bin: %v", err)
	}
	if string(data) != "payload\n" {
		t.Errorf("included.bin content = %q, want %q", data, "payload\n")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("armed filter.evil.smudge RAN during disableSparseCheckoutIfEnabled -- NeutralizeFiltersBestEffort was not applied where it needed to be")
	}
}

// TestDisableSparseCheckoutIfEnabled_AttributesSymlinked_FailsClosed is
// TestApplySparseCheckout_AttributesSymlinked_FailsClosed's counterpart
// for this call site.
func TestDisableSparseCheckoutIfEnabled_AttributesSymlinked_FailsClosed(t *testing.T) {
	dir, marker := itArmSparseRepo(t)

	infoDir := filepath.Join(dir, ".git", "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", infoDir, err)
	}
	// itArmSparseRepo's own initial narrowing call already ran
	// NeutralizeFiltersBestEffort once, so a real attributes file exists
	// here already -- os.Symlink (unlike `ln -sfn`) refuses to overwrite
	// an existing path, so it must be removed first.
	attrPath := filepath.Join(infoDir, "attributes")
	if err := os.Remove(attrPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove existing %s: %v", attrPath, err)
	}
	if err := os.Symlink("/dev/null", attrPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	sup := supervisor.New()
	err := disableSparseCheckoutIfEnabled(context.Background(), sup, dir, itTimeout, itStopGrace)
	if err == nil {
		t.Fatal("disableSparseCheckoutIfEnabled returned nil with .git/info/attributes symlinked -- fail-closed is not wired up at this call site")
	}
	t.Logf("got the expected fail-closed error: %v", err)

	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("armed filter.evil.smudge RAN even though disableSparseCheckoutIfEnabled was supposed to fail closed before ever spawning git")
	}
}

// TestRunGit_AttributesSymlinked_FailsClosed isolates runGit's own
// fail-closed behavior (sync.go) from the full SyncAll/CleanForImageBuild
// orchestration sync_test.go already exercises end to end -- the third
// and most heavily used of the three call sites.
func TestRunGit_AttributesSymlinked_FailsClosed(t *testing.T) {
	dir := t.TempDir()
	itRunGit(t, dir, "init", "-b", "main")
	itRunGit(t, dir, "config", "user.email", "t@t.com")
	itRunGit(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	itRunGit(t, dir, "add", ".")
	itRunGit(t, dir, "commit", "-m", "initial")

	infoDir := filepath.Join(dir, ".git", "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", infoDir, err)
	}
	// itArmSparseRepo's own initial narrowing call already ran
	// NeutralizeFiltersBestEffort once, so a real attributes file exists
	// here already -- os.Symlink (unlike `ln -sfn`) refuses to overwrite
	// an existing path, so it must be removed first.
	attrPath := filepath.Join(infoDir, "attributes")
	if err := os.Remove(attrPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove existing %s: %v", attrPath, err)
	}
	if err := os.Symlink("/dev/null", attrPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	sup := supervisor.New()
	_, err := runGit(context.Background(), sup, []string{"-C", dir, "status", "--porcelain"}, itTimeout, itStopGrace)
	if err == nil {
		t.Fatal("runGit returned nil with .git/info/attributes symlinked -- fail-closed is not wired up at this call site")
	}
	t.Logf("got the expected fail-closed error: %v", err)
}
