// This file (deliberately NOT behind the "integration" build tag, unlike
// push_integration_test.go) proves commandHandler.pushOneRepo's own new
// validation gating directly, in-process, calling pushOneRepo itself
// rather than driving a real, separately-compiled sandbox-agent binary --
// fast enough to run under the default `go test ./...`/`go test -race`
// suite, not just `make test-integration`.
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// initRealGitRepoForPushTest creates a fresh, real git repo at dir on
// branch "main" with one commit -- so a hypothetically-unvalidated
// pushOneRepo call would have a real, valid `-C dir` target to actually
// operate against (rather than failing early for an unrelated "no such
// directory" reason, which would make the "rejected before any real git
// process runs" proof below meaningless).
func initRealGitRepoForPushTest(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "initial commit")
}

func pushTestStrPtr(s string) *string { return &s }

// newSeededPushTestHandler builds a *commandHandler wired the way run()
// actually wires one (§30.5: layout/cred threaded through, matching
// runtimeCredentialFor(cfg) and gitdir.Layout{Root, WorkspaceDir}) rather
// than the bare `&commandHandler{cfg: boot.Config{WorkspaceDir: ...}}`
// literal this file used before.
//
// (Correction, round-2 review): this doc comment used to claim "cred
// threaded through, matching runtimeCredentialFor(cfg)" while the
// function actually left h.cred nil -- no test using this helper could
// then observe pushOneRepo/readRuntimeRemoteURL using the wrong identity
// at all. Fixed by actually calling runtimeCredentialFor with the
// RuntimeUID/RuntimeGID set to THIS TEST PROCESS's own uid/gid: exactly
// like production's own runtimeCredentialFor(cfg), this returns a real,
// non-nil *syscall.Credential, but with NoSetGroups true because the
// configured identity equals the calling process's own -- the identical
// unprivileged-safe combination internal/sandboxagent/supervisor's own
// credential_test.go already proves works without CAP_SETUID (see
// Spec.Credential's own doc comment there). A literal runtime uid (e.g.
// 65534) would make every real git spawn below (the push itself, and
// readRuntimeRemoteURL's config read) fail outright with "operation not
// permitted" under an unprivileged test run. h.cred being non-nil (rather
// than nil) is what makes it possible for a test to tell "this call used
// SOME identity" apart from "this call used none" at all; push_nonorigin_
// test.go's own TestReadRuntimeRemoteURL_PassesHandlerCredential pins the
// exact pointer identity through a dedicated seam (runtimeGitFunc, main.go)
// -- distinguishing THIS credential from nil or any other.
//
// (Correction, review): pushOneRepo now resolves its target via
// h.layout.Repo(repoSpec.Name), not filepath.Join(h.cfg.WorkspaceDir, ...).
// With a zero-value gitdir.Layout, Layout.Repo returns a RELATIVE,
// nonexistent path ({WorkTree:"widgets", GitDir:"widgets"}), so
// gitdir.Run's own assertRealDir guard rejects every call immediately with
// "no such directory" -- an unrelated, structural reason that has nothing
// to do with the validators this file exists to prove. That made every
// "rejected before any real git process runs" proof below vacuous: a
// regression that dropped a validator entirely would still "pass" the
// timing/marker checks, rejected instead by the missing-directory check.
// Wiring a real gitdir.Layout AND actually seeding the repo's agent-owned
// git-dir (gitdir.Seed, exactly like gitclone.SyncAll/CloneAll do before
// any push is ever attempted in production) restores a real target: a
// validator regression now really does reach gitdir.Run and spawn git,
// which the timing baseline in the tests below can actually catch.
func newSeededPushTestHandler(t *testing.T, workspaceDir string, repoNames ...string) *commandHandler {
	t.Helper()

	layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}
	if err := gitdir.EnsureRoot(layout.Root); err != nil {
		t.Fatalf("gitdir.EnsureRoot(%s): %v", layout.Root, err)
	}

	sup := supervisor.New()
	for _, name := range repoNames {
		dir := filepath.Join(workspaceDir, name)
		initRealGitRepoForPushTest(t, dir)
		if err := gitdir.Seed(context.Background(), sup, layout.Repo(name), "https://example.invalid/"+name+".git", nil,
			platform.DefaultTimeouts().GitSyncStepTimeout, platform.DefaultTimeouts().ProcessStopGracePeriod); err != nil {
			t.Fatalf("gitdir.Seed(%s): %v", name, err)
		}
	}

	cfg := boot.Config{
		WorkspaceDir: workspaceDir,
		RuntimeUID:   uint32(os.Getuid()),
		RuntimeGID:   uint32(os.Getgid()),
	}

	return &commandHandler{
		runCtx:   context.Background(),
		cfg:      cfg,
		timeouts: platform.DefaultTimeouts(),
		sup:      sup,
		layout:   layout,
		cred:     runtimeCredentialFor(cfg),
	}
}

// TestPushOneRepo_MaliciousInputsRejectedBeforeSpawn proves a malicious
// repoSpec.Name/Branch/Remote is rejected by pushOneRepo's own
// reposource validators BEFORE h.sup.Spawn is ever called for it.
//
// supervisor.Supervisor exposes no process count to assert against
// directly, so "no real git process ran" is proven via wall-clock
// timing instead, self-calibrated against a REAL measured baseline on
// this exact machine/CI runner (rather than a fixed constant, which
// would be flaky across faster/slower hosts): `git --version` -- the
// cheapest possible real git subprocess invocation, paying only process-
// creation cost and none of git's own repository/network work -- is
// used as that baseline. A pure in-process regex/string rejection
// (reposource's own validators) is faster by orders of magnitude, not
// merely somewhat faster, so asserting the rejection completes in well
// under half that baseline is not expected to be flaky.
func TestPushOneRepo_MaliciousInputsRejectedBeforeSpawn(t *testing.T) {
	workspaceDir := t.TempDir()
	markerDir := t.TempDir()

	baselineStart := time.Now()
	if err := exec.Command("git", "--version").Run(); err != nil {
		t.Fatalf("git --version: %v", err)
	}
	baseline := time.Since(baselineStart)

	h := newSeededPushTestHandler(t, workspaceDir, "widgets")

	branchMarker := filepath.Join(markerDir, "branch-marker-should-never-exist")
	remoteMarker := filepath.Join(markerDir, "remote-marker-should-never-exist")

	tests := []struct {
		name string
		spec sandboxws.PushReposElem
	}{
		{
			name: "malicious name (path traversal)",
			spec: sandboxws.PushReposElem{Name: "../escaped-outside-workspace", Branch: "main"},
		},
		{
			name: "malicious branch (argument injection)",
			spec: sandboxws.PushReposElem{Name: "widgets", Branch: "--receive-pack=touch " + branchMarker},
		},
		{
			name: "malicious remote (argument injection)",
			spec: sandboxws.PushReposElem{
				Name:   "widgets",
				Branch: "main",
				Remote: pushTestStrPtr("--receive-pack=touch " + remoteMarker),
			},
		},
		{
			// The exact attack shape an adversarial review confirmed
			// live: a path-like Remote has no leading dash and no
			// control characters, so it used to pass the old, shared
			// validateRef rule cleanly -- ValidateRemoteName's own
			// charset allowlist (see TestPushOneRepo_
			// PathLikeRemoteRejectedBeforeSpawn_RealTwoRepoProof below
			// for the fuller, real-two-repo version of this same proof)
			// now rejects it before pushOneRepo computes anything else.
			name: "malicious remote (path-like destination, no leading dash)",
			spec: sandboxws.PushReposElem{
				Name:   "widgets",
				Branch: "main",
				Remote: pushTestStrPtr("/tmp/attacker-controlled-rogue-bare-repo.git"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			_, err := h.pushOneRepo(tc.spec)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatalf("pushOneRepo(%+v) error = nil, want a validation error", tc.spec)
			}
			if elapsed >= baseline/2 {
				t.Errorf("pushOneRepo(%+v) took %s (real git-subprocess baseline = %s) -- suspiciously slow "+
					"for pure validation, suggesting a real git process may have been spawned before rejection",
					tc.spec, elapsed, baseline)
			}
		})
	}

	if _, statErr := os.Stat(branchMarker); !os.IsNotExist(statErr) {
		t.Errorf("marker file for malicious branch exists (stat error = %v) -- a real git push actually ran", statErr)
	}
	if _, statErr := os.Stat(remoteMarker); !os.IsNotExist(statErr) {
		t.Errorf("marker file for malicious remote exists (stat error = %v) -- a real git push actually ran", statErr)
	}
}

// TestPushOneRepo_PathLikeRemoteRejectedBeforeSpawn_RealTwoRepoProof proves,
// via a REAL two-repo setup, the exact attack an adversarial review
// confirmed live against this codebase: before this fix,
// reposource.ValidateRemoteName shared ValidateBranch's own permissive
// rule (reject empty/leading-dash/control-chars only), which a plain
// filesystem path passes cleanly -- no leading dash, no control
// characters. A real `git push` then genuinely sent the sandbox's real
// commit to that attacker-chosen destination instead of the real "origin"
// (proven directly by the reviewer with a real two-repo test: a
// legitimate origin repo left untouched, and an attacker's rogue repo
// that received the actual commit).
//
// This test reproduces that same two-repo shape and proves the fix: the
// rogue destination is a real, otherwise-empty bare git repo, and after
// pushOneRepo rejects the malicious repoSpec.Remote, the rogue repo's HEAD
// still fails to resolve at all (nothing was ever pushed to it) and it
// never contains the widgets repo's real commit object either -- zero
// side effects, not merely a harmlessly-failed push for some unrelated
// reason.
func TestPushOneRepo_PathLikeRemoteRejectedBeforeSpawn_RealTwoRepoProof(t *testing.T) {
	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "widgets")
	h := newSeededPushTestHandler(t, workspaceDir, "widgets")

	shaOut, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s rev-parse HEAD: %v\n%s", repoDir, err, shaOut)
	}
	wantSHA := strings.TrimSpace(string(shaOut))

	// The attacker's rogue destination: a real, otherwise-empty bare git
	// repo living entirely outside workspaceDir -- exactly the shape the
	// adversarial review's own live proof used (a filesystem path, no
	// leading dash, no control characters).
	rogueDir := filepath.Join(t.TempDir(), "rogue-bare-repo.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", rogueDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare (rogue): %v\n%s", err, out)
	}

	spec := sandboxws.PushReposElem{
		Name:   "widgets",
		Branch: "main",
		Remote: pushTestStrPtr(rogueDir),
	}

	if _, err := h.pushOneRepo(spec); err == nil {
		t.Fatalf("pushOneRepo(%+v) error = nil, want a validation error rejecting the path-like remote", spec)
	}

	// Zero side effects, proof 1: a bare repo that never received a push
	// has no commit HEAD can resolve to at all.
	if out, err := exec.Command("git", "-C", rogueDir, "rev-parse", "--verify", "HEAD").CombinedOutput(); err == nil {
		t.Fatalf("rogue repo HEAD resolved to %q -- a real push reached the rogue destination", strings.TrimSpace(string(out)))
	}

	// Zero side effects, proof 2: even independent of HEAD, the rogue repo
	// must never contain the widgets repo's actual commit object.
	if out, err := exec.Command("git", "-C", rogueDir, "cat-file", "-e", wantSHA).CombinedOutput(); err == nil {
		t.Fatalf("rogue repo contains the real commit %s (output: %s) -- a real push reached the rogue destination", wantSHA, out)
	}
}

// TestGitPushDashDash_RealDefenseInDepth proves, against the REAL git
// binary, that "--" placed in pushOneRepo's own exact position
// ("push -- <remote> <branch>") genuinely stops git's own option parser
// from treating a leading-"-" remote as a FLAG -- the push-side analog
// of internal/sandboxagent/gitclone's own
// TestGitCloneDashDash_RealDefenseInDepth.
//
// Isolated from reposource.ValidateRemoteName's own separate rejection
// (which, in production, never lets such a value reach pushOneRepo's
// Args at all): this test invokes git directly against a real local
// bare repo, bypassing commandHandler/reposource entirely.
//
// Unlike clone's own "--upload-pack" analog (verified NOT to trigger
// real command execution in cloneOne's own exact positional shape,
// since the leftover positional there is always a not-yet-existing
// clone destination), push's local transport DOES invoke a
// "--receive-pack=<cmd>" override immediately, without first checking
// whether the remaining positional resolves to a real, reachable
// remote -- verified directly below via a real marker file <cmd>
// creates, not merely a parsed, locale-dependent error message.
func TestGitPushDashDash_RealDefenseInDepth(t *testing.T) {
	tmp := t.TempDir()
	bareDir := filepath.Join(tmp, "bare.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}

	srcDir := filepath.Join(tmp, "src")
	initRealGitRepoForPushTest(t, srcDir)
	runInSrc := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = srcDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v (dir=%s): %v\n%s", args, srcDir, err, out)
		}
	}
	runInSrc("remote", "add", "origin", bareDir)
	runInSrc("push", "-q", "origin", "main")

	markerWithoutSeparator := filepath.Join(tmp, "marker-without-separator")
	markerWithSeparator := filepath.Join(tmp, "marker-with-separator")

	// Exactly pushOneRepo's own two trailing positionals (remote, branch)
	// -- here, remote is the malicious value and branch is "main".
	maliciousWithoutSeparator := "--receive-pack=touch " + markerWithoutSeparator
	_ = exec.Command("git", "-C", srcDir, "push", maliciousWithoutSeparator, "main").Run()
	if _, statErr := os.Stat(markerWithoutSeparator); statErr != nil {
		t.Fatalf("sanity check failed: without --, git did not execute --receive-pack's command at all "+
			"(marker missing: %v) -- this test's own premise no longer holds against this git version", statErr)
	}

	maliciousWithSeparator := "--receive-pack=touch " + markerWithSeparator
	cmdWith := exec.Command("git", "-C", srcDir, "push", "--", maliciousWithSeparator, "main")
	if err := cmdWith.Run(); err == nil {
		t.Fatal("git push -- <leading-dash remote> unexpectedly succeeded, want a real failure")
	}
	if _, statErr := os.Stat(markerWithSeparator); !os.IsNotExist(statErr) {
		t.Errorf(`marker file exists (stat error = %v) -- "--" did NOT stop option parsing; `+
			"the malicious --receive-pack value was still executed", statErr)
	}
}
