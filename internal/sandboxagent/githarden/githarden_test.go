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

// soloRepo wraps a plain, non-split repository directory as a Repo whose
// GitDir is that SAME directory's own real ".git" -- i.e. WorkTree and the
// resolved .git are the ordinary, single-location shape most of this
// file's transport-class tests only ever needed (they exercise
// hardeningFlags/Env, which do not care whether GitDir is split from
// WorkTree or not). Distinct from newSplitRepo (below), which builds the
// REAL, measured Step 171 shape (an agent-owned git-dir seeded via
// internal/sandboxagent/gitdir.Seed) for the tests that specifically need
// to prove something about THAT split.
func soloRepo(dir string) Repo {
	return Repo{WorkTree: dir, GitDir: filepath.Join(dir, ".git")}
}

// TestArgs_CarriesTheHardenedShape pins the pair that must never be
// separated -- the Step 171 (§30.5) successor to this test's own
// pre-Step-171 shape (which pinned safe.directory+hooksPath together;
// safe.directory is gone entirely now, see Args' own doc comment for why
// the explicit --git-dir/--work-tree pair replaces it structurally rather
// than merely widening a config allowlist).
func TestArgs_CarriesTheHardenedShape(t *testing.T) {
	repo := soloRepo("/workspace/repo")
	got := Args(repo, "push", "--", "origin", "main")

	if got[0] != "-C" || got[1] != "/workspace/repo" {
		t.Errorf("args do not start with -C <worktree>: %v", got[:2])
	}
	if !slices.Contains(got, "--git-dir="+repo.GitDir) {
		t.Errorf("args missing --git-dir=%s: %v", repo.GitDir, got)
	}
	if !slices.Contains(got, "--work-tree=/workspace/repo") {
		t.Errorf("args missing --work-tree=/workspace/repo: %v", got)
	}
	assertFlag(t, got, "core.hooksPath=/dev/null",
		"without it a hook the runtime planted in .git/hooks runs as this process")
	assertFlag(t, got, "core.fsmonitor=",
		"core.fsmonitor names a command git runs, and it is settable from the repository config the runtime owns")
	for _, a := range got {
		if strings.HasPrefix(a, "safe.directory=") || a == "safe.directory" {
			t.Errorf("safe.directory must never be set any more (Step 171/§30.5 -- --git-dir/--work-tree makes it moot): %v", got)
		}
	}

	if !slices.Contains(got, "push") {
		t.Errorf("the caller's own arguments were dropped: %v", got)
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
// Step 171 (§30.5): only Args is asserted now -- Harden (the "insert
// around an existing -C" entry point) is deleted, since every hardened
// invocation now names an agent-owned Repo explicitly rather than
// rewriting an already-built argv.
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

	got := Args(soloRepo("/workspace/repo"), "status")
	set := map[string]string{}
	for i := 0; i+1 < len(got); i++ {
		if got[i] != "-c" {
			continue
		}
		k, v, found := strings.Cut(got[i+1], "=")
		if !found {
			t.Errorf("-c %q has no '='", got[i+1])
			continue
		}
		set[k] = v
	}
	for key, wantValue := range want {
		gotValue, ok := set[key]
		if !ok {
			t.Errorf("%s is NOT neutralised; a repository-authored value for it runs a command as this process", key)
			continue
		}
		if gotValue != wantValue {
			t.Errorf("%s = %q, want %q", key, gotValue, wantValue)
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
	args := Args(soloRepo("/workspace/repo"), "-c", "credential.helper=!narvi credential-helper", "fetch")

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

// -- transport class: protocol.allow, core.gitProxy, http.proxy ------------
//
// §30.5 also hands the runtime remote.origin.url and every protocol.*/
// core.gitProxy/http.proxy key -- but unlike filter.<driver>/merge.<driver>
// above, EVERY key in this class has a FIXED, enumerable name, which is
// exactly what makes a -c override able to close it. Each test below
// follows the same three-beat shape: CONTROL (zero hardening, against the
// plain runtime repo -- the attack must succeed, or the test proves
// nothing), FIXED (the real Args() output, via soloRepo -- the attack must
// fail), MUTATION (drop exactly the one flag under test -- the attack must
// succeed again, or some OTHER flag is silently doing its job and the test
// is not actually pinning what it claims to). soloRepo wraps a plain,
// non-split repo directory as a Repo whose GitDir is its own real ".git"
// -- these tests are about hardeningFlags/GIT_ALLOW_PROTOCOL's own
// contents, not about the agent/runtime split itself (which
// TestClosedClass_*/TestSharedObjects_VisibleBothWays, above, and
// TestTransportClass_AgentConfigArbitraryHelperBlockedByAllowProtocol,
// below, already cover).

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

// argsWithout builds Args(repo, rest...)'s exact output EXCEPT every
// "-c <k>" pair whose value is named in drop is removed -- used to
// mutation-verify one flag's own necessity in isolation.
func argsWithout(repo Repo, drop []string, rest ...string) []string {
	full := hardeningFlags()
	kept := make([]string, 0, len(full))
	for i := 0; i < len(full); i++ {
		if full[i] == "-c" && i+1 < len(full) && slices.Contains(drop, full[i+1]) {
			i++
			continue
		}
		kept = append(kept, full[i])
	}
	args := []string{"-C", repo.WorkTree, "--git-dir=" + repo.GitDir, "--work-tree=" + repo.WorkTree}
	args = append(args, kept...)
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

	repo := soloRepo(repoDir)
	gitIgnoringExitCode(t, repoDir, Args(repo, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ext:: transport ran even with hardeningFlags in place")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repo, []string{"protocol.ext.allow=never"}, "fetch", "origin")...)
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

	repo := soloRepo(repoDir)
	gitIgnoringExitCode(t, repoDir, Args(repo, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("insteadOf rewrite to ext:: ran even with hardeningFlags in place -- protocol restriction was not applied to the rewritten URL")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repo, []string{"protocol.ext.allow=never"}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mutation: removing protocol.ext.allow=never alone did not re-open the attack (marker absent)")
	}
}

// TestTransportClass_GitProxyBlocked: the runtime points core.gitProxy at
// a planted script and rewrites remote.origin.url to a "git://" URL --
// core.gitProxy is consulted ONLY for that protocol (git-config(1)).
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

	repo := soloRepo(repoDir)
	gitIgnoringExitCode(t, repoDir, Args(repo, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("core.gitProxy ran even with hardeningFlags in place")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repo, []string{"protocol.git.allow=never"}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mutation: removing protocol.git.allow=never alone (keeping core.gitProxy=none) did not re-open the attack -- core.gitProxy=none was NOT expected to be independently sufficient")
	}
}

// TestTransportClass_SSHBlocked: the runtime rewrites remote.origin.url to
// an ssh:// URL, plants core.sshCommand, AND arms protocol.ssh.allow=always
// explicitly.
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

	repo := soloRepo(repoDir)
	gitIgnoringExitCode(t, repoDir, Args(repo, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ssh transport ran even with hardeningFlags in place")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repo, []string{"protocol.ssh.allow=never"}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("removing protocol.ssh.allow=never alone re-opened the attack -- core.sshCommand= (a pre-existing reset, still present) was expected to still cover this by itself")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repo, []string{"core.sshCommand="}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("removing core.sshCommand= alone re-opened the attack -- protocol.ssh.allow=never was expected to still cover this by itself")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repo, []string{"protocol.ssh.allow=never", "core.sshCommand="}, "fetch", "origin")...)
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
// actually does.
func TestTransportClass_ArbitraryRemoteHelperBlockedByAllowProtocol(t *testing.T) {
	repoDir, marker := newAttackRepo(t)
	gitInRepo(t, repoDir, "config", "protocol.foo.allow", "always")
	// The leading "!" is load-bearing: without it, git treats an alias
	// value as a literal git subcommand plus word-split arguments, never
	// as a shell command.
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

	repo := soloRepo(repoDir)

	// ARGS-ONLY: hardeningFlags' own -c enumeration, with NO
	// GIT_ALLOW_PROTOCOL in the environment -- "foo" has no
	// protocol.foo.allow=never entry (and could not: the name is the
	// attacker's own choice), so this must STILL execute.
	argsOnly := exec.Command("git", Args(repo, "fetch", "origin")...)
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
	fixed := exec.Command("git", Args(repo, "fetch", "origin")...)
	fixed.Dir = repoDir
	fixed.Env = Env(gitEnv())
	_ = fixed.Run()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the foo:: alias ran even with Args()+Env() (GIT_ALLOW_PROTOCOL=https) in place")
	}

	// MUTATION: removing GIT_ALLOW_PROTOCOL from the environment (keeping
	// every -c flag) re-opens the attack.
	mutated := exec.Command("git", Args(repo, "fetch", "origin")...)
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
// have their own protocol.ftp(s).allow=never entry -- and now do -- but
// the point this test pins is narrower: even with NO protocol.ftp.allow
// entry at all, GIT_ALLOW_PROTOCOL is what actually stops git from even
// ATTEMPTING the transport.
func TestTransportClass_FTPBlockedByAllowProtocol(t *testing.T) {
	repoDir := t.TempDir()
	gitInRepo(t, repoDir, "init", "-q", ".")
	gitInRepo(t, repoDir, "remote", "add", "origin", "ftp://nonexistent-host-xyz-abc.invalid/repo.git")
	gitInRepo(t, repoDir, "config", "protocol.ftp.allow", "always")

	repo := soloRepo(repoDir)
	// argsNoFTPPolicy simulates the ORIGINAL PR's own -c enumeration --
	// hardeningFlags MINUS this Step's own protocol.ftp.allow=never/
	// protocol.ftps.allow=never additions.
	argsNoFTPPolicy := argsWithout(repo, []string{"protocol.ftp.allow=never", "protocol.ftps.allow=never"}, "fetch", "origin")

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
// TestOpenClass_UploadPackExecutes: remote.<name>.uploadpack has no
// .gitattributes half at all, but is consulted ONLY for a local ("file")
// or ssh transport, never for smart HTTP.
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

	gitIgnoringExitCode(t, repoDir, "fetch", "origin")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: armed uploadpack did NOT run with no hardening at all (marker absent: %v) -- this test proves nothing", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker between control and fixed runs: %v", err)
	}

	repo := soloRepo(repoDir)
	gitIgnoringExitCode(t, repoDir, Args(repo, "fetch", "origin")...)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("armed uploadpack ran even with hardeningFlags in place -- if this now passes, it means file-transport denial stopped closing this class; investigate before touching this test")
	}

	gitIgnoringExitCode(t, repoDir, argsWithout(repo, []string{"protocol.file.allow=never"}, "fetch", "origin")...)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mutation: removing protocol.file.allow=never alone (repo's own protocol.file.allow=always still armed) did not re-open the attack (marker absent) -- protocol.allow=never's general fallback was NOT expected to be independently sufficient once a specific per-protocol policy is set")
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
	gitInRepo(t, repoDir, "config", "http.proxy", "http://127.0.0.1:1")

	controlCmd := exec.Command("git", "-c", "http.sslVerify=false", "fetch", "origin")
	controlCmd.Dir = repoDir
	controlCmd.Env = gitEnv()
	if out, err := controlCmd.CombinedOutput(); err == nil {
		t.Fatalf("control: fetch through the broken proxy SUCCEEDED (want failure) -- this test proves nothing\n%s", out)
	}

	repo := soloRepo(repoDir)
	fixedCmd := exec.Command("git", Args(repo, "fetch", "origin")...)
	fixedCmd.Dir = repoDir
	fixedCmd.Env = gitEnv()
	if out, err := fixedCmd.CombinedOutput(); err != nil {
		t.Fatalf("fetch with hardeningFlags in place still went through the repository's own proxy: %v\n%s", err, out)
	}

	mutated1Cmd := exec.Command("git", argsWithout(repo, []string{"http.proxy="}, "fetch", "origin")...)
	mutated1Cmd.Dir = repoDir
	mutated1Cmd.Env = gitEnv()
	if out, err := mutated1Cmd.CombinedOutput(); err != nil {
		t.Fatalf("removing http.proxy= alone (remote.origin.proxy= still present) re-opened the attack (fetch failed) -- remote.origin.proxy= was expected to still cover this by itself: %v\n%s", err, out)
	}

	mutated2Cmd := exec.Command("git", argsWithout(repo, []string{"remote.origin.proxy="}, "fetch", "origin")...)
	mutated2Cmd.Dir = repoDir
	mutated2Cmd.Env = gitEnv()
	if out, err := mutated2Cmd.CombinedOutput(); err != nil {
		t.Fatalf("removing remote.origin.proxy= alone (http.proxy= still present) re-opened the attack (fetch failed) -- http.proxy= was expected to still cover this by itself: %v\n%s", err, out)
	}

	mutated3Cmd := exec.Command("git", argsWithout(repo, []string{"http.proxy=", "remote.origin.proxy="}, "fetch", "origin")...)
	mutated3Cmd.Dir = repoDir
	mutated3Cmd.Env = gitEnv()
	if out, err := mutated3Cmd.CombinedOutput(); err == nil {
		t.Fatalf("mutation: removing BOTH http.proxy= and remote.origin.proxy= did not re-open the attack (fetch still succeeded) -- some OTHER flag is silently doing this job\n%s", out)
	}
}

// startCaptureProxy starts a real TCP listener that stands in for an
// attacker-controlled HTTP proxy, and reports on connected whether the
// FIRST connection it ever accepts opens with an HTTP CONNECT line.
func startCaptureProxy(t *testing.T) (proxyURL string, connected <-chan bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ch := make(chan bool, 1)
	var group errgroup.Group
	group.Go(func() error {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
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
// guarantee behind gitclone's fetch/ls-remote helpers: hardeningFlags' own
// unconditional, remote-NAME-keyed "-c remote.origin.proxy=".
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
	remoteURL := server.URL + "/src"

	repoDir := t.TempDir()
	gitInRepo(t, repoDir, "init", "-q", ".")
	gitInRepo(t, repoDir, "config", "remote.origin.url", remoteURL)
	gitInRepo(t, repoDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")

	armProxy := func() <-chan bool {
		t.Helper()
		proxyURL, connected := startCaptureProxy(t)
		gitInRepo(t, repoDir, "config", "http."+remoteURL+".proxy", proxyURL)
		return connected
	}

	controlConnected := armProxy()
	controlCmd := exec.Command("git", "-c", "http.sslVerify=false", "fetch", "origin")
	controlCmd.Dir = repoDir
	controlCmd.Env = gitEnv()
	_ = controlCmd.Run()
	select {
	case gotConnect := <-controlConnected:
		if !gotConnect {
			t.Fatal("control: the listener accepted a connection that did not open with CONNECT -- fixture is wrong")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control: fetch never dialed the planted proxy at all (want a CONNECT) -- this test proves nothing")
	}

	repo := soloRepo(repoDir)
	fixedConnected := armProxy()
	fixedCmd := exec.Command("git", Args(repo, "fetch", "origin")...)
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
	}

	mutatedConnected := armProxy()
	mutatedCmd := exec.Command("git", argsWithout(repo, []string{"remote.origin.proxy="}, "fetch", "origin")...)
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
// operation" half of this Step: a normal https clone (via ArgsForClone)
// and fetch (via Args, soloRepo) of a real repository must still succeed
// -- proving the transport allowlist permits exactly the one transport
// this codebase actually uses. It ALSO proves remote.<name>.uploadpack is
// genuinely inert for a smart-HTTP transport.
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
	cloneCmd := exec.Command("git", append([]string{"-C", cloneParent}, ArgsForClone(cloneDir)...)...)
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

	fetchCmd := exec.Command("git", Args(soloRepo(cloneDir), "fetch", "origin")...)
	fetchCmd.Env = gitEnv()
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		t.Fatalf("real https fetch failed through hardeningFlags: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the armed uploadpack command ran over a smart-HTTP transport -- it is supposed to be inert there")
	}
}
