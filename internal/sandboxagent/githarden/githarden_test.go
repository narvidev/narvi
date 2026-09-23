package githarden

import (
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestMain sets GIT_SSL_NO_VERIFY=true ONCE, before any test runs (never
// racing any test's own t.Parallel() goroutines) -- the transport-class
// tests below clone/fetch over a real, but self-signed-TLS, local
// git-http-backend server (startLocalHTTPSGitServer) to exercise the one
// transport hardeningFlags still permits (https). Trusting the self-signed
// cert is acceptable ONLY because these are throwaway test servers --
// mirrors internal/sandboxagent/gitclone's own clone_test.go TestMain,
// same reason.
func TestMain(m *testing.M) {
	_ = os.Setenv("GIT_SSL_NO_VERIFY", "true")
	os.Exit(m.Run())
}

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
		"credential.helper":    "",
		"core.hooksPath":       "/dev/null",
		"core.sshCommand":      "",
		"diff.external":        "",
		"core.pager":           "cat",
		"core.fsmonitor":       "",
		"protocol.allow":       "never",
		"protocol.https.allow": "always",
		"protocol.http.allow":  "never",
		"protocol.file.allow":  "never",
		"protocol.git.allow":   "never",
		"protocol.ssh.allow":   "never",
		"protocol.ext.allow":   "never",
		"core.gitProxy":        "none",
		"http.proxy":           "",
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

// -- the two command classes this package does NOT close -------------------
//
// The tests below are the unusual kind: they assert that an attack SUCCEEDS.
// That is deliberate, and it is this file's whole remaining contribution on
// the subject.
//
// A mitigation was tried here and withdrawn -- see githarden.go's own doc
// comment for the measurement that condemned it. What replaces it is not a
// weaker guard but an executable record: tests that run REAL git and
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
// §30.5 hands it, plus a committed .gitattributes, which any repository
// ships.
//
// A THIRD class used to be recorded here: remote.<name>.uploadpack /
// remote.<name>.receivepack, the transport-class hardening below now
// closes it -- see TestTransportClass_FileUploadPackNoLongerExecutes for
// the executable proof, and this package's own doc comment for why:
// uploadpack/receivepack is consulted only for a local ("file") or ssh
// transport, never for smart HTTP, and sandbox-agent never configures
// anything but https in the first place.

// gitEnv is os.Environ plus a fixed identity. EVERY git command this file
// spawns needs it, not only the committing ones: a merge writes a commit
// too, and a developer machine's own global identity would silently supply
// it while a CI container has none -- so a command that omits this passes
// locally and fails remotely for a reason that has nothing to do with what
// the test is about.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
	)
}

// gitInRepo runs git in dir with a fixed identity, failing the test on error.
func gitInRepo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
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

	// The merge is allowed to report a conflict -- only the driver having
	// RUN is what this test observes. But its output is captured and
	// reported on failure: a merge that never happened (a git that refused
	// the repository, a branch name this fixture guessed wrong) would
	// otherwise look identical to a driver that did not fire, and this test
	// would blame the wrong thing.
	merge := exec.Command("git", append(Args(repoDir), "merge", "theirs")...)
	merge.Dir = repoDir
	merge.Env = gitEnv()
	mergeOut, mergeErr := merge.CombinedOutput()

	// Precondition: git must actually have attempted a three-way merge of
	// the armed path. If it fast-forwarded or refused outright, the driver
	// was never reachable and the run proves nothing either way.
	if !strings.Contains(string(mergeOut), "f.txt") {
		t.Fatalf("precondition failed: git never attempted a three-way merge of the armed path.\ngit merge said: %v\n%s", mergeErr, mergeOut)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the armed merge driver did NOT run (marker %s absent: %v)\ngit merge said: %v\n%s\n-- if this now passes because the class was closed, delete this test rather than repairing it", marker, err, mergeErr, mergeOut)
	}
}

// -- transport class: protocol.allow, core.gitProxy, http.proxy ------------
//
// §30.5 also hands the runtime remote.origin.url and every protocol.*/
// core.gitProxy/http.proxy key -- but unlike filter.<driver>/merge.<driver>
// above, EVERY key in this class has a FIXED, enumerable name, which is
// exactly what makes a -c override able to close it. Each test below
// follows the same three-beat shape: CONTROL (zero hardening -- the attack
// must succeed, or the test proves nothing), FIXED (the real Args()/
// Harden() output -- the attack must fail), MUTATION (drop exactly the one
// flag under test -- the attack must succeed again, or some OTHER flag is
// silently doing its job and the test is not actually pinning what it
// claims to).

// gitIgnoringExitCode runs git with args in dir, under a fixed identity,
// and does NOT fail the test on a non-zero exit: every attack below makes
// the OVERALL git command fail (the planted transport/proxy/uploadpack
// value is not a real git-remote-helper, and most of these commands are
// refused before they'd ever succeed for real) -- the marker file touched
// on the way there is the only signal any of these tests cares about.
func gitIgnoringExitCode(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	_ = cmd.Run()
}

// argsWithout builds Args(repoDir, rest...)'s exact output EXCEPT every
// "-c <k>" pair whose value is named in drop is removed -- used to
// mutation-verify one flag's own necessity in isolation.
func argsWithout(repoDir string, drop []string, rest ...string) []string {
	full := hardeningFlags(repoDir)
	kept := make([]string, 0, len(full))
	for i := 0; i < len(full); i++ {
		if full[i] == "-c" && i+1 < len(full) && slices.Contains(drop, full[i+1]) {
			i++
			continue
		}
		kept = append(kept, full[i])
	}
	args := append([]string{"-C", repoDir}, kept...)
	return append(args, rest...)
}

// TestTransportClass_ExtRewriteBlocked: the runtime rewrites
// remote.origin.url directly to an "ext::" command. protocol.ext.allow
// itself defaults to "never" in real git (verified: git refuses this with
// ZERO configuration at all), so the attacker also arms the allow policy
// -- an ordinary, unprivileged .git/config write, same as the URL rewrite
// itself.
func TestTransportClass_ExtRewriteBlocked(t *testing.T) {
	repoDir, marker := newAttackRepo(t)
	gitInRepo(t, repoDir, "config", "remote.origin.url", "ext::touch "+marker)
	gitInRepo(t, repoDir, "config", "protocol.ext.allow", "always")

	gitIgnoringExitCode(t, repoDir, "fetch", "origin")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: armed ext:: remote did NOT run with NO hardening at all (marker absent: %v) -- this test proves nothing", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker between control and fixed runs: %v", err)
	}

	gitIgnoringExitCode(t, repoDir, Args(repoDir, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ext:: transport ran even with hardeningFlags in place")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repoDir, []string{"protocol.ext.allow=never"}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mutation: removing protocol.ext.allow=never alone did not re-open the attack (marker absent) -- some OTHER flag is silently doing this one's job")
	}
}

// TestTransportClass_InsteadOfRewriteToExtBlocked proves git's own
// documented behaviour actually holds (git-config(1), url.<base>.
// insteadOf: "Note that any protocol restrictions will be applied to the
// rewritten URL"): origin's URL is exactly what reposource.ValidateRepoURL
// would have accepted at session-config time (an absolute https:// URL),
// but a runtime-planted insteadOf rewrite turns it into an ext:: command
// at fetch time. protocol.ext.allow=never must still refuse it -- checked
// AFTER the rewrite, never against the pre-rewrite scheme.
func TestTransportClass_InsteadOfRewriteToExtBlocked(t *testing.T) {
	repoDir, marker := newAttackRepo(t)
	gitInRepo(t, repoDir, "config", "remote.origin.url", "https://good.example.invalid/repo.git")
	gitInRepo(t, repoDir, "config", "protocol.ext.allow", "always")
	gitInRepo(t, repoDir, "config", "url.ext::touch "+marker+" x.insteadOf", "https://good.example.invalid/repo.git")

	gitIgnoringExitCode(t, repoDir, "fetch", "origin")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: insteadOf rewrite to ext:: did NOT run with no hardening (marker absent: %v) -- this test proves nothing", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker between control and fixed runs: %v", err)
	}

	gitIgnoringExitCode(t, repoDir, Args(repoDir, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("insteadOf rewrite to ext:: ran even with hardeningFlags in place -- protocol restriction was not applied to the rewritten URL")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repoDir, []string{"protocol.ext.allow=never"}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mutation: removing protocol.ext.allow=never alone did not re-open the attack (marker absent)")
	}
}

// TestTransportClass_GitProxyBlocked: the runtime points core.gitProxy at
// a planted script and rewrites remote.origin.url to a "git://" URL --
// core.gitProxy is consulted ONLY for that protocol (git-config(1)).
//
// The decisive finding this test pins: core.gitProxy=none, ON ITS OWN, is
// NOT what closes this. core.gitProxy is documented as multi-valued
// ("this variable may be set multiple times ... the FIRST match wins"),
// and -- unlike credential.helper, whose empty value is documented to
// discard every earlier entry -- git gives an empty/"none" core.gitProxy
// no such reset semantics: a repository-file-sourced entry is consulted
// BEFORE anything the command line adds, so it wins regardless of what a
// later "-c core.gitProxy=..." says (verified directly: a command-line
// "-c core.gitProxy=none" does NOT stop a repo-configured proxy script
// from running). What actually closes this is protocol.git.allow=never,
// which denies the only transport that ever consults core.gitProxy at
// all -- the mutation below drops exactly that key, keeping
// core.gitProxy=none, and the attack succeeds again.
func TestTransportClass_GitProxyBlocked(t *testing.T) {
	repoDir, marker := newAttackRepo(t)
	proxyScript := filepath.Join(t.TempDir(), "proxy.sh")
	if err := os.WriteFile(proxyScript, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write proxy script: %v", err)
	}
	gitInRepo(t, repoDir, "config", "remote.origin.url", "git://example.invalid/repo.git")
	gitInRepo(t, repoDir, "config", "core.gitProxy", proxyScript)
	gitInRepo(t, repoDir, "config", "protocol.git.allow", "always")

	gitIgnoringExitCode(t, repoDir, "fetch", "origin")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: armed core.gitProxy did NOT run with no hardening (marker absent: %v) -- this test proves nothing", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker between control and fixed runs: %v", err)
	}

	gitIgnoringExitCode(t, repoDir, Args(repoDir, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("core.gitProxy ran even with hardeningFlags in place")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repoDir, []string{"protocol.git.allow=never"}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mutation: removing protocol.git.allow=never alone (keeping core.gitProxy=none) did not re-open the attack -- core.gitProxy=none was NOT expected to be independently sufficient")
	}
}

// TestTransportClass_SSHBlocked: the runtime rewrites remote.origin.url to
// an ssh:// URL, plants core.sshCommand, AND arms protocol.ssh.allow=always
// explicitly (ssh is one of git's own "known-safe" defaults already, but
// arming the specific policy is what proves protocol.ssh.allow=never's OWN
// necessity below, rather than the general protocol.allow=never fallback
// silently doing the work: a specific per-protocol policy set anywhere
// always outranks the general fallback -- verified earlier for file/ext).
//
// This one has TWO independent guards, and the test pins both. The
// pre-existing core.sshCommand= reset (a single-valued key -- unlike core.gitProxy,
// a command-line override DOES win here) already closes the CONFIG-BASED
// version of this attack by itself; dropping protocol.ssh.allow=never
// alone must NOT reopen it, and dropping core.sshCommand= alone must not
// either. Only dropping BOTH reopens it -- proving protocol.ssh.allow=never
// closes the ssh transport outright (defense in depth: any ssh-transport
// path that does NOT go through core.sshCommand stays closed too), not the
// config-based vector alone.
func TestTransportClass_SSHBlocked(t *testing.T) {
	repoDir, marker := newAttackRepo(t)
	fakeSSH := filepath.Join(t.TempDir(), "fakessh.sh")
	if err := os.WriteFile(fakeSSH, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	gitInRepo(t, repoDir, "config", "remote.origin.url", "ssh://nonexistent-host-xyz.invalid/repo.git")
	gitInRepo(t, repoDir, "config", "core.sshCommand", fakeSSH)
	gitInRepo(t, repoDir, "config", "protocol.ssh.allow", "always")

	gitIgnoringExitCode(t, repoDir, "fetch", "origin")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: armed core.sshCommand did NOT run with no hardening (marker absent: %v) -- this test proves nothing", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker between control and fixed runs: %v", err)
	}

	gitIgnoringExitCode(t, repoDir, Args(repoDir, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ssh transport ran even with hardeningFlags in place")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repoDir, []string{"protocol.ssh.allow=never"}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("removing protocol.ssh.allow=never alone re-opened the attack -- core.sshCommand= (a pre-existing reset, still present) was expected to still cover this by itself")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repoDir, []string{"core.sshCommand="}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("removing core.sshCommand= alone re-opened the attack -- protocol.ssh.allow=never was expected to still cover this by itself")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repoDir, []string{"protocol.ssh.allow=never", "core.sshCommand="}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mutation: removing BOTH protocol.ssh.allow=never and core.sshCommand= did not re-open the attack (marker absent) -- some OTHER flag is silently doing this job")
	}
}

// TestTransportClass_FileUploadPackNoLongerExecutes supersedes the former
// TestOpenClass_UploadPackExecutes: remote.<name>.uploadpack has
// no .gitattributes half at all -- it lives purely in .git/config -- but it
// is consulted ONLY for a local ("file") or ssh transport, never for smart
// HTTP (verified: TestArgs_RealHTTPSCloneAndFetchStillWork below exercises
// the exact same key over https and it is never even invoked). Since
// sandbox-agent only ever configures an https remote in production, and
// protocol.file.allow=never/protocol.ssh.allow=never now deny the only two
// transports that ever consult this key, this class closes as a
// consequence of the transport allowlist -- not by a key of its own.
func TestTransportClass_FileUploadPackNoLongerExecutes(t *testing.T) {
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

	gitInRepo(t, repoDir, "config", "remote.origin.uploadpack", payload(marker, "git-upload-pack"))

	// CONTROL: this is the former TestOpenClass_UploadPackExecutes, unchanged -- the
	// historical record that the attack is real against a local-transport
	// origin with no hardening at all.
	gitIgnoringExitCode(t, repoDir, "fetch", "origin")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: armed uploadpack did NOT run with no hardening at all (marker absent: %v) -- this test proves nothing", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker between control and fixed runs: %v", err)
	}

	gitIgnoringExitCode(t, repoDir, Args(repoDir, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("armed uploadpack ran even with hardeningFlags in place -- if this now passes, it means file-transport denial stopped closing this class; investigate before touching this test")
	}

	// MUTATION: the origin here has no explicit protocol.file.allow of its
	// own (unlike the ext:: tests above, which arm the specific policy),
	// so the GENERAL protocol.allow=never fallback alone already covers
	// it -- both must be dropped to prove neither is silently redundant
	// with something else in the list.
	gitIgnoringExitCode(t, repoDir, argsWithout(repoDir, []string{"protocol.allow=never", "protocol.file.allow=never"}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mutation: removing protocol.allow=never and protocol.file.allow=never did not re-open the attack (marker absent) -- some OTHER flag is silently doing this job")
	}
}

// TestTransportClass_HTTPProxyNeutralised proves http.proxy is reset, via
// an observable side effect rather than a marker file: a repository-
// configured proxy pointed at a port nothing listens on makes a fetch over
// the one surviving transport (https) fail outright if honoured, and
// succeed if the reset works.
func TestTransportClass_HTTPProxyNeutralised(t *testing.T) {
	reposParent := t.TempDir()
	srcDir := filepath.Join(reposParent, "src")
	gitInRepo(t, "", "init", "-q", srcDir)
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	gitInRepo(t, srcDir, "add", "-A")
	gitInRepo(t, srcDir, "commit", "-qm", "seed")
	server := startLocalHTTPSGitServer(t, reposParent)

	repoDir := t.TempDir()
	gitInRepo(t, repoDir, "init", "-q", ".")
	gitInRepo(t, repoDir, "config", "remote.origin.url", server.URL+"/src")
	// Port 1 is never a listener in any real test/CI environment (same
	// reasoning gitclone's own clone_test.go uses for its unreachable-host
	// fixtures) -- a repository-configured proxy that, if honoured, makes
	// every fetch through it fail fast with connection-refused.
	gitInRepo(t, repoDir, "config", "http.proxy", "http://127.0.0.1:1")

	// CONTROL: the repo's proxy, with no override, must actually break the
	// fetch -- otherwise this test does not exercise what it claims to.
	controlCmd := exec.Command("git", "-c", "http.sslVerify=false", "fetch", "origin")
	controlCmd.Dir = repoDir
	controlCmd.Env = gitEnv()
	if out, err := controlCmd.CombinedOutput(); err == nil {
		t.Fatalf("control: fetch through the broken proxy SUCCEEDED (want failure) -- this test proves nothing\n%s", out)
	}

	// FIXED: Args()' own http.proxy= reset must override it and reach the
	// real server directly.
	fixedCmd := exec.Command("git", Args(repoDir, "fetch", "origin")...)
	fixedCmd.Dir = repoDir
	fixedCmd.Env = gitEnv()
	if out, err := fixedCmd.CombinedOutput(); err != nil {
		t.Fatalf("fetch with hardeningFlags in place still went through the repository's own proxy: %v\n%s", err, out)
	}

	// MUTATION: drop http.proxy= alone -- the fetch must fail again.
	mutatedCmd := exec.Command("git", argsWithout(repoDir, []string{"http.proxy="}, "fetch", "origin")...)
	mutatedCmd.Dir = repoDir
	mutatedCmd.Env = gitEnv()
	if out, err := mutatedCmd.CombinedOutput(); err == nil {
		t.Fatalf("mutation: removing http.proxy= alone did not re-open the attack (fetch still succeeded)\n%s", out)
	}
}

// startLocalHTTPSGitServer serves reposParent via git's own smart-HTTP
// backend (git-http-backend, via net/http/cgi -- a real git server, not a
// mock) over TLS -- the same technique internal/sandboxagent/gitclone's own
// clone_test.go uses (startGitHTTPSServer), reproduced here because it is
// the only thing that exercises the one transport hardeningFlags still
// permits: sandbox-agent never legitimately uses anything else
// (reposource.ValidateRepoURL accepts only "https://"). Skips (not fails)
// if git-http-backend is unavailable in this environment.
func startLocalHTTPSGitServer(t *testing.T, reposParent string) *httptest.Server {
	t.Helper()
	execPathOut, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backendPath := filepath.Join(strings.TrimSpace(string(execPathOut)), "git-http-backend")
	if _, statErr := os.Stat(backendPath); statErr != nil {
		t.Skipf("git-http-backend not available at %s, skipping: %v", backendPath, statErr)
	}

	cgiHandler := &cgi.Handler{
		Path: backendPath,
		Root: "/",
		Env: []string{
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_PROJECT_ROOT=" + reposParent,
		},
	}

	server := httptest.NewUnstartedServer(cgiHandler)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// TestArgs_RealHTTPSCloneAndFetchStillWork is the "do not break legitimate
// operation" half of this Step: a normal https clone and fetch of a real
// repository, entirely through Args()/Harden(), must still succeed --
// proving the transport allowlist permits exactly the one transport this
// codebase actually uses. It ALSO proves remote.<name>.uploadpack is
// genuinely inert for a smart-HTTP transport (see
// TestTransportClass_FileUploadPackNoLongerExecutes's own doc comment): the
// same armed uploadpack key is planted here too, the fetch must still
// succeed, and the marker must NOT exist -- an https fetch never consults
// this key at all, so denying file/ssh transport is not merely denying
// this class' one remaining home, it is denying its ONLY home.
func TestArgs_RealHTTPSCloneAndFetchStillWork(t *testing.T) {
	reposParent := t.TempDir()
	srcDir := filepath.Join(reposParent, "src")
	gitInRepo(t, "", "init", "-q", srcDir)
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	gitInRepo(t, srcDir, "add", "-A")
	gitInRepo(t, srcDir, "commit", "-qm", "seed")
	server := startLocalHTTPSGitServer(t, reposParent)

	cloneParent := t.TempDir()
	cloneDir := filepath.Join(cloneParent, "clone")
	cloneCmd := exec.Command("git", append([]string{"-C", cloneParent}, hardeningFlags(cloneDir)...)...)
	cloneCmd.Args = append(cloneCmd.Args, "clone", "--", server.URL+"/src", cloneDir)
	cloneCmd.Env = gitEnv()
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("real https clone failed through hardeningFlags: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(cloneDir, "a.txt")); err != nil {
		t.Fatalf("cloned repo missing its own committed file: %v", err)
	}

	marker := filepath.Join(t.TempDir(), "EXECUTED")
	gitInRepo(t, cloneDir, "config", "remote.origin.uploadpack", payload(marker, "git-upload-pack"))

	if err := os.WriteFile(filepath.Join(srcDir, "b.txt"), []byte("second commit\n"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}
	gitInRepo(t, srcDir, "add", "-A")
	gitInRepo(t, srcDir, "commit", "-qm", "second")

	fetchCmd := exec.Command("git", Args(cloneDir, "fetch", "origin")...)
	fetchCmd.Env = gitEnv()
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		t.Fatalf("real https fetch failed through hardeningFlags: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the armed uploadpack command ran over a smart-HTTP transport -- it is supposed to be inert there")
	}
}
