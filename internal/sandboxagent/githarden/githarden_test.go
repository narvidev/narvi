package githarden

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestArgs_CarriesBothHalves pins the pair that must never be separated.
//
// safe.directory alone un-breaks git against a runtime-owned repository
// AND hands a prompt-injected agent execution as sandbox-agent, because
// git push runs .git/hooks/pre-push and the runtime owns .git. Verified in
// a container: with only safe.directory, a planted hook printed "HOOK RAN
// as uid=0" under a root push; with hooksPath pointed away, the same push
// succeeded and the hook did not run.
//
// So a change that keeps one and drops the other is either a broken push
// or a root execution primitive, and neither announces itself in a way a
// reviewer would notice. This test is what notices.
func TestArgs_CarriesBothHalves(t *testing.T) {
	got := Args("/workspace/repo", "push", "--", "origin", "main")

	assertFlag(t, got, "safe.directory=/workspace/repo",
		"git refuses a repository owned by another user without it, so every push and head-sha read fails")
	assertFlag(t, got, "core.hooksPath=/dev/null",
		"without it a hook the runtime planted in .git/hooks runs as this process")
	assertFlag(t, got, "core.fsmonitor=",
		"core.fsmonitor names a command git runs, and it is settable from the repository config the runtime owns")

	if got[0] != "-C" || got[1] != "/workspace/repo" {
		t.Errorf("args do not start with -C <dir>: %v", got[:2])
	}
	if !slices.Contains(got, "push") {
		t.Errorf("the caller's own arguments were dropped: %v", got)
	}
}

// TestHarden_InsertsAroundAnExistingDashC covers the call sites that
// assemble their arguments elsewhere and pass them through one runner.
func TestHarden_InsertsAroundAnExistingDashC(t *testing.T) {
	got := Harden([]string{"-C", "/workspace/repo", "status", "--porcelain"})

	assertFlag(t, got, "safe.directory=/workspace/repo", "the repository path must be scoped from the caller's own -C")
	assertFlag(t, got, "core.hooksPath=/dev/null", "hardening must not depend on which call site assembled the arguments")
	if got[len(got)-1] != "--porcelain" || !slices.Contains(got, "status") {
		t.Errorf("the caller's own subcommand was lost or reordered: %v", got)
	}
}

// TestHarden_NoRepositoryScopeIsLeftAlone: an invocation naming no
// repository has no path to declare safe, and inventing a wildcard would
// widen exactly what this narrows.
func TestHarden_NoRepositoryScopeIsLeftAlone(t *testing.T) {
	in := []string{"--version"}
	got := Harden(in)
	if !slices.Equal(got, in) {
		t.Errorf("Harden(%v) = %v, want it unchanged -- no -C means no repository to scope to", in, got)
	}
	for _, a := range got {
		if strings.HasPrefix(a, "safe.directory=") {
			t.Errorf("a wildcard-ish safe.directory was added with no repository in sight: %v", got)
		}
	}
}

func assertFlag(t *testing.T, args []string, want, why string) {
	t.Helper()
	for i, a := range args {
		if a == "-c" && i+1 < len(args) && args[i+1] == want {
			return
		}
	}
	t.Errorf("missing -c %s\n    %s\n    got: %v", want, why, args)
}

// TestHardeningFlags_NeutralisesEveryRepoSettableCommandKey asserts the
// SET, not a sample.
//
// The phase audit found credential.helper missing while hooksPath and
// fsmonitor were present — a check that names some of the dangerous keys
// reads like a perimeter and is not one. credential.helper is the worst
// of them: git runs the named command AND hands it the credential over
// its own protocol, so one planted value is both execution as this
// process and theft of the SCM token.
//
// Both entry points are asserted, because they were two separate lists
// until this audit and a key added to one is exactly what gets missed.
func TestHardeningFlags_NeutralisesEveryRepoSettableCommandKey(t *testing.T) {
	// Every key here makes git RUN something and is settable from the
	// repository's own .git/config, which the agent runtime owns.
	want := map[string]string{
		"credential.helper": "",
		"core.hooksPath":    "/dev/null",
		"core.sshCommand":   "",
		"diff.external":     "",
		"core.pager":        "cat",
		"core.fsmonitor":    "",
	}

	for _, tc := range []struct {
		name string
		got  []string
	}{
		{"Args", Args("/workspace/repo", "status")},
		{"Harden", Harden([]string{"-C", "/workspace/repo", "status"})},
	} {
		set := map[string]string{}
		for i := 0; i+1 < len(tc.got); i++ {
			if tc.got[i] != "-c" {
				continue
			}
			k, v, found := strings.Cut(tc.got[i+1], "=")
			if !found {
				t.Errorf("%s: -c %q has no '='", tc.name, tc.got[i+1])
				continue
			}
			set[k] = v
		}
		for key, wantValue := range want {
			gotValue, ok := set[key]
			if !ok {
				t.Errorf("%s: %s is NOT neutralised; a repository-authored value for it runs a command as this process", tc.name, key)
				continue
			}
			if gotValue != wantValue {
				t.Errorf("%s: %s = %q, want %q", tc.name, key, gotValue, wantValue)
			}
		}
	}
}

// TestArgs_CredentialHelperResetPrecedesTheCallersOwn pins the ORDER,
// which is the whole reason the empty value works.
//
// credential.helper is multi-valued: git accumulates helpers rather than
// replacing them. An empty value discards every helper configured so far
// — including one planted in the repository's config — and the caller's
// own helper, added after, is then the only survivor. Reversed, the
// reset would wipe Narvi's own helper and leave the repository's.
func TestArgs_CredentialHelperResetPrecedesTheCallersOwn(t *testing.T) {
	args := Args("/workspace/repo", "-c", "credential.helper=!narvi credential-helper", "fetch")

	resetAt, oursAt := -1, -1
	for i, a := range args {
		if a != "-c" || i+1 >= len(args) {
			continue
		}
		switch args[i+1] {
		case "credential.helper=":
			resetAt = i
		case "credential.helper=!narvi credential-helper":
			oursAt = i
		}
	}
	if resetAt == -1 {
		t.Fatal("no credential.helper reset in the hardening flags")
	}
	if oursAt == -1 {
		t.Fatal("the caller's own credential.helper did not survive")
	}
	if resetAt > oursAt {
		t.Errorf("the reset is at %d and the caller's helper at %d: the reset must come FIRST, or it discards Narvi's own helper and leaves the repository's", resetAt, oursAt)
	}
}

// -- the three command classes this package does NOT close ----------------
//
// The tests below are the unusual kind: they assert that an attack SUCCEEDS.
// That is deliberate, and it is this file's whole remaining contribution on
// the subject.
//
// A mitigation was tried here and withdrawn -- see githarden.go's own doc
// comment for the measurement that condemned it. What replaces it is not a
// weaker guard but an executable record: three tests that run REAL git and
// observe a repository-selected command actually execute, exactly the way
// sandbox-agent's own later invocations would let it. A comment claiming
// "this class is open" rots the moment someone reads it and assumes it was
// fixed since. A test that goes red when the class finally closes cannot.
//
// So when the structural remedy lands -- sandbox-agent no longer running git
// against a .git the runtime owns -- these tests SHOULD start failing. That
// failure is the signal to delete them, not to repair them.
//
// Each one plants only what the sandbox runtime can plant with its own
// ordinary, unprivileged powers: the config half in .git/config, which
// §30.5 hands it, plus (for the first two) a committed .gitattributes,
// which any repository ships.

// gitInRepo runs git in dir with a fixed identity, failing the test on error.
func gitInRepo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// newAttackRepo builds a repository whose .git/config the "runtime" owns,
// and returns its path plus the marker path a planted command touches.
func newAttackRepo(t *testing.T) (repoDir, marker string) {
	t.Helper()
	repoDir = t.TempDir()
	marker = filepath.Join(t.TempDir(), "EXECUTED")
	gitInRepo(t, repoDir, "init", "-q", ".")
	return repoDir, marker
}

// payload returns a shell command that touches marker and then behaves.
func payload(marker, passthrough string) string {
	return "touch " + marker + "; " + passthrough
}

// TestOpenClass_ContentFilterExecutes is class (1): filter.<driver>.smudge.
//
// The driver NAME is chosen by the repository's own .gitattributes, so there
// is no fixed config key a -c flag could reset -- which is why hardeningFlags
// cannot cover it the way it covers core.hooksPath.
func TestOpenClass_ContentFilterExecutes(t *testing.T) {
	repoDir, marker := newAttackRepo(t)

	if err := os.WriteFile(filepath.Join(repoDir, ".gitattributes"), []byte("victim.txt filter=evil\n"), 0o644); err != nil {
		t.Fatalf("write .gitattributes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "victim.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	gitInRepo(t, repoDir, "add", "-A")
	gitInRepo(t, repoDir, "commit", "-qm", "seed")

	// The config half: an ordinary unprivileged write for the runtime.
	gitInRepo(t, repoDir, "config", "filter.evil.smudge", payload(marker, "cat"))

	if err := os.Remove(filepath.Join(repoDir, "victim.txt")); err != nil {
		t.Fatalf("remove victim: %v", err)
	}
	// A checkout is what SyncAll and CleanForImageBuild both perform.
	gitInRepo(t, repoDir, append(Args(repoDir), "checkout", "--", "victim.txt")...)

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the armed smudge filter did NOT run (marker %s absent: %v) -- if this now passes because the class was closed, delete this test rather than repairing it", marker, err)
	}
}

// TestOpenClass_MergeDriverExecutes is class (2): merge.<driver>.driver.
//
// Same shape as (1) with a different attribute, and notably NOT covered by an
// attributes override that unsets only `filter`. It fires during the
// `stash pop --index` gitclone's own syncOne performs.
func TestOpenClass_MergeDriverExecutes(t *testing.T) {
	repoDir, marker := newAttackRepo(t)

	if err := os.WriteFile(filepath.Join(repoDir, ".gitattributes"), []byte("f.txt merge=evil\n"), 0o644); err != nil {
		t.Fatalf("write .gitattributes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write f: %v", err)
	}
	gitInRepo(t, repoDir, "add", "-A")
	gitInRepo(t, repoDir, "commit", "-qm", "seed")

	gitInRepo(t, repoDir, "config", "merge.evil.driver", payload(marker, "true"))

	// Diverge the same path on two branches so a real three-way merge runs.
	gitInRepo(t, repoDir, "checkout", "-q", "-b", "theirs")
	if err := os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("theirs\n"), 0o644); err != nil {
		t.Fatalf("write theirs: %v", err)
	}
	gitInRepo(t, repoDir, "commit", "-qam", "theirs")
	gitInRepo(t, repoDir, "checkout", "-q", "-")
	if err := os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("ours\n"), 0o644); err != nil {
		t.Fatalf("write ours: %v", err)
	}
	gitInRepo(t, repoDir, "commit", "-qam", "ours")

	// The merge itself is allowed to report a conflict; only the driver
	// having RUN is what this test observes.
	merge := exec.Command("git", append(Args(repoDir), "merge", "theirs")...)
	merge.Dir = repoDir
	_ = merge.Run()

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the armed merge driver did NOT run (marker %s absent: %v) -- if this now passes because the class was closed, delete this test rather than repairing it", marker, err)
	}
}

// TestOpenClass_UploadPackExecutes is class (3), and the decisive one:
// remote.<name>.uploadpack has NO .gitattributes half at all.
//
// It is pure config, consulted on a plain fetch, so no attributes file --
// however privileged, however early -- could ever have reached it. This is
// what makes "write an override into .git/info/attributes" unsound as a
// strategy rather than merely as an implementation.
func TestOpenClass_UploadPackExecutes(t *testing.T) {
	srcDir := t.TempDir()
	gitInRepo(t, srcDir, "init", "-q", ".")
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	gitInRepo(t, srcDir, "add", "-A")
	gitInRepo(t, srcDir, "commit", "-qm", "seed")

	cloneParent := t.TempDir()
	gitInRepo(t, cloneParent, "clone", "-q", srcDir, "clone")
	repoDir := filepath.Join(cloneParent, "clone")
	marker := filepath.Join(t.TempDir(), "EXECUTED")

	// No .gitattributes anywhere -- the config half is the whole attack.
	gitInRepo(t, repoDir, "config", "remote.origin.uploadpack", payload(marker, "git-upload-pack"))

	fetch := exec.Command("git", append(Args(repoDir), "fetch", "origin")...)
	fetch.Dir = repoDir
	_ = fetch.Run()

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the armed uploadpack command did NOT run (marker %s absent: %v) -- if this now passes because the class was closed, delete this test rather than repairing it", marker, err)
	}
}
