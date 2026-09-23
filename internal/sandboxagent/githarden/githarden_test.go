package githarden

import (
	"bufio"
	"net"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
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
// SET, not a sample -- of the -c overrides hardeningFlags carries, which
// is deliberately NOT the same claim as "the set of protocols this class
// permits". An earlier version of this test omitted ftp/ftps while making
// that "not a sample" claim, which was itself the exact class of
// near-miss this test exists to catch: ftp/ftps are enumerable, fixed
// names, exactly like every other protocol.<name>.allow key here, and
// were simply missing.
//
// What this test does NOT claim, and the -c list itself no longer is: the
// FULL guarantee behind the transport class. GIT_ALLOW_PROTOCOL (see
// githarden.go's own "transport class" doc comment, and TestEnv_* below)
// is what actually closes the class this enumeration cannot reach at
// all -- an arbitrary "<name>::" remote helper, whose name an attacker
// chooses and this list can therefore never enumerate in advance. Every
// key asserted below is still real, still defense-in-depth, and still
// worth pinning by name -- it is just not, on its own, the reason the
// class is closed any more.
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
	// Every key here makes git RUN something (or, for the proxy pair,
	// redirect the one surviving transport) and is settable from the
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
		"protocol.ftp.allow":   "never",
		"protocol.ftps.allow":  "never",
		"core.gitProxy":        "none",
		"http.proxy":           "",
		"remote.origin.proxy":  "",
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

// TestTransportClass_ArbitraryRemoteHelperBlockedByAllowProtocol is F1's
// own executable proof: an ARBITRARY "<name>::" remote helper, whose name
// the attacker chooses, has no fixed protocol.<name>.allow key for
// hardeningFlags' own -c enumeration to reset in advance -- so the -c
// flags alone (Args, with no GIT_ALLOW_PROTOCOL in the child's own
// environment) do NOT stop it, and GIT_ALLOW_PROTOCOL=https (Env) is what
// actually does. The planted "helper" here is a git ALIAS that runs an
// arbitrary shell command (a documented git feature -- "alias.<name>" can
// begin with "!" to run a shell command), invoked by git's own resolution
// of a remote helper program named "git-remote-foo": this codebase does
// not ship one, so the alias stands in for it, but the invocation path
// (git resolves "foo::..." to a program named "git-remote-foo" on PATH,
// which an attacker able to write BOTH .git/config's own alias section
// AND control what PATH resolves for this process could plant for real --
// the alias is the reliable, portable stand-in that does not depend on
// PATH layout in a test environment) is git's own documented behavior,
// not a test artifact.
func TestTransportClass_ArbitraryRemoteHelperBlockedByAllowProtocol(t *testing.T) {
	repoDir, marker := newAttackRepo(t)
	gitInRepo(t, repoDir, "config", "protocol.foo.allow", "always")
	// The leading "!" is load-bearing: without it, git treats an alias
	// value as a literal git subcommand plus word-split arguments, never
	// as a shell command -- verified directly (an earlier version of this
	// test omitted it and the marker never appeared, even with no
	// hardening at all).
	gitInRepo(t, repoDir, "config", "alias.remote-foo", "!"+payload(marker, "true"))
	gitInRepo(t, repoDir, "remote", "add", "origin", "foo::whatever")

	// CONTROL: no hardening at all.
	gitIgnoringExitCode(t, repoDir, "fetch", "origin")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: the foo:: alias did NOT run with no hardening at all (marker absent: %v) -- this test proves nothing", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker between control and args-only runs: %v", err)
	}

	// ARGS-ONLY: hardeningFlags' own -c enumeration, with NO
	// GIT_ALLOW_PROTOCOL in the environment -- the exact shape this PR
	// shipped before this Step. "foo" has no protocol.foo.allow=never
	// entry (and could not: the name is the attacker's own choice), so
	// this must STILL execute.
	argsOnly := exec.Command("git", Args(repoDir, "fetch", "origin")...)
	argsOnly.Dir = repoDir
	argsOnly.Env = gitEnv()
	_ = argsOnly.Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("args-only (hardeningFlags, no GIT_ALLOW_PROTOCOL): the foo:: alias did NOT run (marker absent) -- if this now passes, the -c enumeration alone started closing arbitrary remote helpers; investigate before touching this test")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker between args-only and fixed runs: %v", err)
	}

	// FIXED: Args() plus Env(gitEnv()) -- GIT_ALLOW_PROTOCOL=https now in
	// the child's own environment. Must block it.
	fixed := exec.Command("git", Args(repoDir, "fetch", "origin")...)
	fixed.Dir = repoDir
	fixed.Env = Env(gitEnv())
	_ = fixed.Run()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the foo:: alias ran even with Args()+Env() (GIT_ALLOW_PROTOCOL=https) in place")
	}

	// MUTATION: same as ARGS-ONLY above, restated as the mutation this
	// class's own fix depends on -- removing GIT_ALLOW_PROTOCOL from the
	// environment (keeping every -c flag) re-opens the attack.
	mutated := exec.Command("git", Args(repoDir, "fetch", "origin")...)
	mutated.Dir = repoDir
	mutated.Env = gitEnv()
	_ = mutated.Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mutation: removing GIT_ALLOW_PROTOCOL from the environment (keeping every -c flag) did not re-open the attack (marker absent) -- some OTHER mechanism is silently doing GIT_ALLOW_PROTOCOL's job")
	}
}

// TestTransportClass_FTPBlockedByAllowProtocol is F1's own second
// executable proof: ftp/ftps are FIXED names (git ships git-remote-ftp(s)
// by default), so unlike the arbitrary "<name>::" helper above they COULD
// have their own protocol.ftp(s).allow=never entry -- and now do (this
// Step's own hardeningFlags addition) -- but the point this test pins is
// narrower: even with NO protocol.ftp.allow entry at all (this repo never
// arms "protocol.ftp.allow=always" -- the general protocol.allow=never
// fallback is what this test exercises), GIT_ALLOW_PROTOCOL is what
// actually stops git from even ATTEMPTING the transport (observed via
// git's own error text: "Could not resolve host" -- a real connection
// attempt -- with no GIT_ALLOW_PROTOCOL, versus "transport 'ftp' not
// allowed" -- refused before any connection attempt -- with it).
func TestTransportClass_FTPBlockedByAllowProtocol(t *testing.T) {
	repoDir := t.TempDir()
	gitInRepo(t, repoDir, "init", "-q", ".")
	gitInRepo(t, repoDir, "remote", "add", "origin", "ftp://nonexistent-host-xyz-abc.invalid/repo.git")
	gitInRepo(t, repoDir, "config", "protocol.ftp.allow", "always")

	// argsNoFTPPolicy simulates the ORIGINAL PR's own -c enumeration --
	// hardeningFlags MINUS this Step's own protocol.ftp.allow=never/
	// protocol.ftps.allow=never additions (argsWithout) -- so this test
	// isolates GIT_ALLOW_PROTOCOL's OWN independent contribution, not the
	// -c flags this Step ALSO added for defense-in-depth. Without that
	// exclusion, Args() already carries an explicit protocol.ftp.allow=never
	// on the command line, which (like every other per-protocol key in
	// this file) already outranks the repo's own "always" on its own,
	// leaving nothing left for GIT_ALLOW_PROTOCOL to independently prove.
	argsNoFTPPolicy := argsWithout(repoDir, []string{"protocol.ftp.allow=never", "protocol.ftps.allow=never"}, "fetch", "origin")

	// ARGS-ONLY: no GIT_ALLOW_PROTOCOL. Must reach an actual connection
	// attempt (DNS resolution), not a transport-level refusal -- proving
	// the enumeration gap this Step's own doc comment describes.
	argsOnly := exec.Command("git", argsNoFTPPolicy...)
	argsOnly.Dir = repoDir
	argsOnly.Env = gitEnv()
	out, err := argsOnly.CombinedOutput()
	if err == nil {
		t.Fatal("fetch of an ftp:// remote from a nonexistent host unexpectedly SUCCEEDED -- this test proves nothing")
	}
	if !strings.Contains(string(out), "Could not resolve host") {
		t.Fatalf("args-only (no protocol.ftp.allow=never, no GIT_ALLOW_PROTOCOL): expected a real connection attempt (DNS resolution failure), got: %s", out)
	}

	// FIXED: Env(gitEnv()) added -- must refuse the transport OUTRIGHT,
	// before ever attempting DNS resolution, via GIT_ALLOW_PROTOCOL alone.
	fixed := exec.Command("git", argsNoFTPPolicy...)
	fixed.Dir = repoDir
	fixed.Env = Env(gitEnv())
	out, err = fixed.CombinedOutput()
	if err == nil {
		t.Fatal("fetch of an ftp:// remote unexpectedly SUCCEEDED with GIT_ALLOW_PROTOCOL=https in place")
	}
	if !strings.Contains(string(out), "ftp") {
		t.Fatalf("with GIT_ALLOW_PROTOCOL=https in place, expected a transport-level refusal naming ftp, got: %s", out)
	}
	if strings.Contains(string(out), "Could not resolve host") {
		t.Fatalf("with GIT_ALLOW_PROTOCOL=https in place, git still attempted a real connection instead of refusing the transport outright: %s", out)
	}

	// MUTATION: remove GIT_ALLOW_PROTOCOL again (same as ARGS-ONLY) --
	// restated as the mutation this fix depends on.
	mutated := exec.Command("git", argsNoFTPPolicy...)
	mutated.Dir = repoDir
	mutated.Env = gitEnv()
	out, err = mutated.CombinedOutput()
	if err == nil {
		t.Fatal("fetch of an ftp:// remote unexpectedly SUCCEEDED")
	}
	if !strings.Contains(string(out), "Could not resolve host") {
		t.Fatalf("mutation: removing GIT_ALLOW_PROTOCOL did not re-open a real connection attempt, got: %s", out)
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
//
// The origin here explicitly arms "protocol.file.allow=always" -- the real
// attacker capability (an ordinary, unprivileged .git/config write, same
// as every other transport test in this file) -- rather than relying on
// the origin having no policy of its own, the way an earlier version of
// this test did. That earlier shape's fixed arm was vacuous: with no
// repo-level protocol.file.allow at all, the GENERAL protocol.allow=never
// fallback alone already blocks the attack, so the run "proved" nothing
// about protocol.file.allow=never specifically -- it would have passed
// identically had that key been silently dropped from hardeningFlags
// entirely. Arming the specific policy, mirroring
// TestTransportClass_ExtRewriteBlocked/_GitProxyBlocked/_SSHBlocked
// above, makes the mutation below actually exercise protocol.file.allow's
// own necessity: once the repo has its own specific policy, that policy
// outranks the general fallback (verified earlier in this file), so
// dropping protocol.file.allow=never ALONE must reopen the attack.
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
	gitInRepo(t, repoDir, "config", "protocol.file.allow", "always")

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

	// MUTATION: drop protocol.file.allow=never ALONE -- the origin's own
	// explicit "protocol.file.allow=always" is now a SPECIFIC policy,
	// which outranks the general protocol.allow=never fallback this run
	// still carries, so the attack must succeed again.
	gitIgnoringExitCode(t, repoDir, argsWithout(repoDir, []string{"protocol.file.allow=never"}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mutation: removing protocol.file.allow=never alone (repo's own protocol.file.allow=always still armed) did not re-open the attack (marker absent) -- protocol.allow=never's general fallback was NOT expected to be independently sufficient once a specific per-protocol policy is set")
	}
}

// TestTransportClass_HTTPProxyNeutralised proves http.proxy is reset, via
// an observable side effect rather than a marker file: a repository-
// configured proxy pointed at a port nothing listens on makes a fetch over
// the one surviving transport (https) fail outright if honoured, and
// succeed if the reset works.
//
// This has TWO independent guards, and the test pins both, mirroring
// TestTransportClass_SSHBlocked's own shape: hardeningFlags' own
// "remote.origin.proxy=" (added for the F3 audit finding below) turns out
// to ALSO override a plain, non-url-scoped http.proxy on its own --
// verified directly against real git, not assumed -- because git treats
// an explicitly-set remote.<name>.proxy (even an empty one) as more
// specific than the general http.proxy family regardless of source.
// Dropping http.proxy= alone must NOT reopen the attack (remote.origin.
// proxy= still covers this exact scenario), and dropping remote.origin.
// proxy= alone must not either (http.proxy= still covers it, as it always
// did) -- only dropping BOTH reopens it.
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

	// FIXED: Args()' own http.proxy=/remote.origin.proxy= resets must
	// override it and reach the real server directly.
	fixedCmd := exec.Command("git", Args(repoDir, "fetch", "origin")...)
	fixedCmd.Dir = repoDir
	fixedCmd.Env = gitEnv()
	if out, err := fixedCmd.CombinedOutput(); err != nil {
		t.Fatalf("fetch with hardeningFlags in place still went through the repository's own proxy: %v\n%s", err, out)
	}

	// MUTATION 1: drop http.proxy= alone -- remote.origin.proxy= (still
	// present) was expected to still cover this by itself, so the fetch
	// must still SUCCEED (a failure here means removing http.proxy= alone
	// already re-opened the attack, i.e. remote.origin.proxy= was NOT
	// independently sufficient).
	mutated1Cmd := exec.Command("git", argsWithout(repoDir, []string{"http.proxy="}, "fetch", "origin")...)
	mutated1Cmd.Dir = repoDir
	mutated1Cmd.Env = gitEnv()
	if out, err := mutated1Cmd.CombinedOutput(); err != nil {
		t.Fatalf("removing http.proxy= alone (remote.origin.proxy= still present) re-opened the attack (fetch failed) -- remote.origin.proxy= was expected to still cover this by itself: %v\n%s", err, out)
	}

	// MUTATION 2: drop remote.origin.proxy= alone -- http.proxy= (still
	// present) was expected to still cover this by itself, exactly as it
	// did before remote.origin.proxy= existed, so the fetch must still
	// SUCCEED.
	mutated2Cmd := exec.Command("git", argsWithout(repoDir, []string{"remote.origin.proxy="}, "fetch", "origin")...)
	mutated2Cmd.Dir = repoDir
	mutated2Cmd.Env = gitEnv()
	if out, err := mutated2Cmd.CombinedOutput(); err != nil {
		t.Fatalf("removing remote.origin.proxy= alone (http.proxy= still present) re-opened the attack (fetch failed) -- http.proxy= was expected to still cover this by itself: %v\n%s", err, out)
	}

	// MUTATION 3: drop BOTH -- the fetch must fail again.
	mutated3Cmd := exec.Command("git", argsWithout(repoDir, []string{"http.proxy=", "remote.origin.proxy="}, "fetch", "origin")...)
	mutated3Cmd.Dir = repoDir
	mutated3Cmd.Env = gitEnv()
	if out, err := mutated3Cmd.CombinedOutput(); err == nil {
		t.Fatalf("mutation: removing BOTH http.proxy= and remote.origin.proxy= did not re-open the attack (fetch still succeeded) -- some OTHER flag is silently doing this job\n%s", out)
	}
}

// startCaptureProxy starts a real TCP listener that stands in for an
// attacker-controlled HTTP proxy, and reports on connected whether the
// FIRST connection it ever accepts opens with an HTTP CONNECT line -- the
// tunnel handshake a proxied https fetch issues. This observes the same
// thing a real attacker-run proxy would observe (a connection actually
// arriving), rather than inferring "was the proxy used" indirectly from
// whether the fetch failed: an unreachable-port fixture (as
// TestTransportClass_HTTPProxyNeutralised, above, uses) can only prove the
// fetch failed, never WHY -- "routed through the proxy, which then refused
// it" and "went direct and the destination refused it" look identical from
// the git process's own exit code. No real proxying happens behind this
// listener: it deliberately closes the connection the instant it has read
// enough to answer the one question this test asks, so a fetch that does
// dial it fails fast rather than hanging until a timeout.
func startCaptureProxy(t *testing.T) (proxyURL string, connected <-chan bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ch := make(chan bool, 1)
	// errgroup.Group.Go, never a bare `go` statement -- §11/nakedgoroutine
	// grants no test exemption (mirrors credentials/cache_test.go's own
	// TestCache_FlockSerializesConcurrentAccess).
	var group errgroup.Group
	group.Go(func() error {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			// t.Cleanup below closed the listener -- this phase's fetch
			// never dialed it at all, which is the expected shape of the
			// FIXED case below and not itself a test failure.
			return nil
		}
		defer func() { _ = conn.Close() }()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		ch <- strings.HasPrefix(line, "CONNECT ")
		return nil
	})
	t.Cleanup(func() {
		_ = ln.Close()
		_ = group.Wait()
	})
	return "http://" + ln.Addr().String(), ch
}

// TestTransportClass_RemoteOriginProxyClosesRewrittenURL pins the actual
// guarantee behind gitclone's fetch/ls-remote helpers (resolveDefaultBranch,
// gitFetchRef, sync.go): hardeningFlags' own unconditional, remote-NAME-keyed
// "-c remote.origin.proxy=" -- not a url-keyed override, which a previous
// version of this codebase added at those two call sites
// (githarden.RepoURLProxyArg, keyed to the validated SESSION url) and which
// docs/DECISIONS.md and both call sites' own comments credited with closing
// this vector. That credit was never earned there: `fetch origin`/`ls-remote
// origin` contact whatever remote.origin.url currently resolves to, read
// from THIS repository's own runtime-owned .git/config, which §30.5 hands
// the agent runtime -- a url the validated session config never re-checks
// against. This test arms the WORST case for that gap: an attacker who has
// rewritten remote.origin.url and planted an http.<url>.proxy for the EXACT
// resulting url (the single most specific match http.<url>.proxy's own
// urlmatch scoring could ever be given), and proves remote.origin.proxy=
// alone -- present regardless of what remote.origin.url says -- still closes
// it, observed directly via a real listening proxy (startCaptureProxy,
// above) rather than inferred from a failure.
func TestTransportClass_RemoteOriginProxyClosesRewrittenURL(t *testing.T) {
	reposParent := t.TempDir()
	srcDir := filepath.Join(reposParent, "src")
	gitInRepo(t, "", "init", "-q", srcDir)
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	gitInRepo(t, srcDir, "add", "-A")
	gitInRepo(t, srcDir, "commit", "-qm", "seed")
	server := startLocalHTTPSGitServer(t, reposParent)
	// Stands in for the url the runtime rewrote remote.origin.url to --
	// this test does not need it to differ from any "original" session url
	// at all (githarden itself never sees a session url in the first
	// place); what matters is that http.<url>.proxy is armed for the EXACT
	// string remote.origin.url resolves to, the case a url-keyed override
	// would, at best, only ever match.
	remoteURL := server.URL + "/src"

	repoDir := t.TempDir()
	gitInRepo(t, repoDir, "init", "-q", ".")
	gitInRepo(t, repoDir, "config", "remote.origin.url", remoteURL)
	gitInRepo(t, repoDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")

	// armProxy (re)plants http.<remoteURL>.proxy pointed at a FRESH
	// listener for one phase -- a fresh listener per phase, rather than
	// one long-lived listener shared across all three, so each phase's own
	// "did a CONNECT arrive" question has an unambiguous, race-free answer.
	armProxy := func() <-chan bool {
		t.Helper()
		proxyURL, connected := startCaptureProxy(t)
		gitInRepo(t, repoDir, "config", "http."+remoteURL+".proxy", proxyURL)
		return connected
	}

	// CONTROL: with no hardening at all, the fetch must actually dial the
	// planted proxy -- otherwise this fixture proves nothing.
	controlConnected := armProxy()
	controlCmd := exec.Command("git", "-c", "http.sslVerify=false", "fetch", "origin")
	controlCmd.Dir = repoDir
	controlCmd.Env = gitEnv()
	_ = controlCmd.Run() // the fake proxy always refuses the tunnel; only the CONNECT itself is observed
	select {
	case gotConnect := <-controlConnected:
		if !gotConnect {
			t.Fatal("control: the listener accepted a connection that did not open with CONNECT -- fixture is wrong")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control: fetch never dialed the planted proxy at all (want a CONNECT) -- this test proves nothing")
	}

	// FIXED: the real Args()/hardeningFlags output -- remote.origin.proxy=
	// must keep the fetch off the planted proxy entirely, reaching the real
	// server directly instead.
	fixedConnected := armProxy()
	fixedCmd := exec.Command("git", Args(repoDir, "fetch", "origin")...)
	fixedCmd.Dir = repoDir
	fixedCmd.Env = gitEnv()
	if out, err := fixedCmd.CombinedOutput(); err != nil {
		t.Fatalf("fetch with hardeningFlags in place failed (want it to reach the real server directly): %v\n%s", err, out)
	}
	select {
	case gotConnect := <-fixedConnected:
		if gotConnect {
			t.Fatal("fixed: fetch with remote.origin.proxy= in place still dialed the planted proxy (CONNECT observed) -- the vector is NOT closed")
		}
	case <-time.After(300 * time.Millisecond):
		// No connection at all -- the expected, correct outcome: the fetch
		// already completed above, so nothing is still in flight to wait for.
	}

	// MUTATION: drop remote.origin.proxy= alone -- the attack must reopen.
	// This is the in-test mirror of "delete the flag from hardeningFlags
	// itself and confirm this test fails", verified by hand (see PR body)
	// against the real, unedited production list.
	mutatedConnected := armProxy()
	mutatedCmd := exec.Command("git", argsWithout(repoDir, []string{"remote.origin.proxy="}, "fetch", "origin")...)
	mutatedCmd.Dir = repoDir
	mutatedCmd.Env = gitEnv()
	_ = mutatedCmd.Run()
	select {
	case gotConnect := <-mutatedConnected:
		if !gotConnect {
			t.Fatal("mutation: removing remote.origin.proxy= did not re-open the attack (no CONNECT observed) -- some OTHER flag is silently doing this job")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mutation: removing remote.origin.proxy= did not re-open the attack (fetch never dialed the proxy)")
	}
}

// TestEnv_AppendsGitAllowProtocolLast pins Env's own documented contract:
// GIT_ALLOW_PROTOCOL=https is appended LAST (so it wins any earlier
// duplicate, exec.Cmd's own "last key wins" Env behavior), and a nil base
// becomes this process's own os.Environ() first rather than a bare
// one-entry slice that would silently strip everything else a real git
// child needs (PATH, HOME, ...).
func TestEnv_AppendsGitAllowProtocolLast(t *testing.T) {
	got := Env(nil)
	if len(got) < 2 {
		t.Fatalf("Env(nil) = %v, want this process's own environ plus GIT_ALLOW_PROTOCOL, not a bare one-entry slice", got)
	}
	if last := got[len(got)-1]; last != "GIT_ALLOW_PROTOCOL=https" {
		t.Errorf("Env(nil)'s last entry = %q, want \"GIT_ALLOW_PROTOCOL=https\" (must be LAST to win over any earlier duplicate)", last)
	}

	got = Env([]string{"FOO=bar", "GIT_ALLOW_PROTOCOL=ssh"})
	want := []string{"FOO=bar", "GIT_ALLOW_PROTOCOL=ssh", "GIT_ALLOW_PROTOCOL=https"}
	if !slices.Equal(got, want) {
		t.Errorf("Env(%v) = %v, want %v -- the caller-supplied base must be preserved verbatim, with GIT_ALLOW_PROTOCOL=https appended last", []string{"FOO=bar", "GIT_ALLOW_PROTOCOL=ssh"}, got, want)
	}
}

// TestRepoURLProxyArg_RejectsAnEqualsSign pins the defensive guard: git's
// own "-c key=value" parsing splits on the FIRST "=" in the whole
// argument, so a repoURL containing one would corrupt the intended
// "http.<url>.proxy=" key rather than merely fail to match anything.
// reposource.ValidateRepoURL does not forbid "=" (legal in a URL query
// string), so this is enforced here rather than assumed impossible
// upstream.
func TestRepoURLProxyArg_RejectsAnEqualsSign(t *testing.T) {
	if got := RepoURLProxyArg("https://example.invalid/repo.git?a=b"); got != nil {
		t.Errorf("RepoURLProxyArg with an '=' in the url = %v, want nil (skip, not a corrupted override)", got)
	}
	if got := RepoURLProxyArg(""); got != nil {
		t.Errorf("RepoURLProxyArg(\"\") = %v, want nil", got)
	}
	want := []string{"-c", "http.https://example.invalid/repo.git.proxy="}
	if got := RepoURLProxyArg("https://example.invalid/repo.git"); !slices.Equal(got, want) {
		t.Errorf("RepoURLProxyArg(...) = %v, want %v", got, want)
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
