// This file (deliberately NOT behind the "integration" build tag, unlike
// push_integration_test.go) proves commandHandler.pushOneRepo's own new
// validation gating directly, in-process, calling pushOneRepo itself
// rather than driving a real, separately-compiled sandbox-agent binary --
// fast enough to run under the default `go test ./...`/`go test -race`
// suite, not just `make test-integration`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
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
// which the spawn count in the tests below can actually catch.
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
// It asserts on the spawn itself. h.sup.SpawnCount must not move across
// the call, and every git process pushOneRepo can start goes through
// h.sup, so an unmoved count means no git ran at all, however briefly.
// The error must also carry the validator's own typed error, which tells
// a rejection apart from a git failure. It also tells a rejection apart
// from a structural guard that would have refused the call before any
// spawn anyway. The spawn count alone would pass in that case, even with
// the validator gone.
//
// This replaces a timing baseline. The test used to time one real `git
// --version` and fail any rejection that took half of it or more. One
// fast baseline sample, or one scheduler pause under -race, was enough
// to fail it with the code correct: 3.59ms against a 1.815ms baseline,
// on main, with the marker files still absent.
func TestPushOneRepo_MaliciousInputsRejectedBeforeSpawn(t *testing.T) {
	workspaceDir := t.TempDir()
	markerDir := t.TempDir()

	h := newSeededPushTestHandler(t, workspaceDir, "widgets")

	branchMarker := filepath.Join(markerDir, "branch-marker-should-never-exist")
	remoteMarker := filepath.Join(markerDir, "remote-marker-should-never-exist")

	var (
		badName   *reposource.InvalidRepoNameError
		badBranch *reposource.InvalidRefError
		badRemote *reposource.InvalidRemoteNameError
	)

	tests := []struct {
		name string
		spec sandboxws.PushReposElem
		// rejectedAs is the validator's typed error pushOneRepo must
		// return (errors.As target).
		rejectedAs any
	}{
		{
			name:       "malicious name (path traversal)",
			spec:       sandboxws.PushReposElem{Name: "../escaped-outside-workspace", Branch: "main"},
			rejectedAs: &badName,
		},
		{
			name:       "malicious branch (argument injection)",
			spec:       sandboxws.PushReposElem{Name: "widgets", Branch: "--receive-pack=touch " + branchMarker},
			rejectedAs: &badBranch,
		},
		{
			name: "malicious remote (argument injection)",
			spec: sandboxws.PushReposElem{
				Name:   "widgets",
				Branch: "main",
				Remote: pushTestStrPtr("--receive-pack=touch " + remoteMarker),
			},
			rejectedAs: &badRemote,
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
			rejectedAs: &badRemote,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := h.sup.SpawnCount()
			_, err := h.pushOneRepo(tc.spec)
			spawned := h.sup.SpawnCount() - before

			if spawned != 0 {
				t.Errorf("pushOneRepo(%+v) spawned %d process(es) before returning, want 0 -- "+
					"a real git process ran before the malicious input was rejected", tc.spec, spawned)
			}
			if err == nil {
				t.Fatalf("pushOneRepo(%+v) error = nil, want a validation error", tc.spec)
			}
			if !errors.As(err, tc.rejectedAs) {
				t.Errorf("pushOneRepo(%+v) error = %v, want it to wrap the validator's %T -- "+
					"something other than the validator refused this input", tc.spec, err, tc.rejectedAs)
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

// TestSendPushError_CapsGitPushStderr pins that sendPushError sends git
// push's stderr capped (wsbridge.CapCriticalText): push_error is a critical
// event, never cut, and SendCritical refuses one over the 32 KiB every
// control plane reads, so an uncapped 40 KiB stderr would never be
// reported at all -- the control plane would get a warning in its place
// and no push failure (technical plan §6.1).
func TestSendPushError_CapsGitPushStderr(t *testing.T) {
	t.Parallel()

	frames := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(platform.MaxFrameBytesHeader, strconv.Itoa(platform.MaxEventFrameBytes))
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		conn.SetReadLimit(platform.MaxEventFrameBytes)
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var env struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(data, &env)
			if env.Type != "ready" && env.Type != "heartbeat" {
				frames <- data
			}
		}
	}))
	t.Cleanup(server.Close)

	bridge := wsbridge.New(sessionconfig.SessionConfig{
		BootMode: sessionconfig.SessionConfigBootModeFresh, ControlPlaneWsUrl: server.URL, Gen: 1, SandboxToken: "t", SessionId: "s",
	}, "sbx", "v", "d", nil, time.Second, time.Hour, 10*time.Millisecond, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	var group errgroup.Group
	group.Go(func() error { return bridge.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		_ = group.Wait()
	})

	h := &commandHandler{runCtx: ctx, bridge: bridge}
	stderr := strings.Repeat("remote: error: refusing to update checked out branch\n", 800) // ~42 KiB
	h.sendPushError(sandboxws.Push{Type: "push", MessageId: "push-1", SessionId: "s", Gen: 1}, errors.New("git push widgets: exited 1: "+stderr))

	var data []byte
	select {
	case data = <-frames:
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was sent")
	}
	var got sandboxws.PushError
	if err := json.Unmarshal(data, &got); err != nil || got.Type != "push_error" {
		t.Fatalf("sent %.200s (%v), want the push_error itself", data, err)
	}
	if len(got.Error) > 4096 || !strings.HasSuffix(got.Error, "...[truncated]") || !strings.HasPrefix(got.Error, "git push widgets: exited 1: remote: error") {
		t.Fatalf("push_error carries %d bytes ending %q; want the head of git's stderr capped at 4096, marked", len(got.Error), got.Error[len(got.Error)-20:])
	}
}

// TestPushOneRepo_RefspecShapedBranchRejectedBeforeSpawn: a branch reaches
// pushOneRepo's `git push -- <remote> <branch>` as a refspec, so a value
// git would read as something other than that one branch is refused by
// reposource.ValidateBranch before any git process starts -- the spawn
// count does not move, and the error is the validator's own. "+main" is
// the forced push technical plan §35.3 rules out; the others delete,
// redirect, glob or name HEAD (TestGitPushRefspecBranch_RealPremise shows
// the first two against real git).
func TestPushOneRepo_RefspecShapedBranchRejectedBeforeSpawn(t *testing.T) {
	h := newSeededPushTestHandler(t, t.TempDir(), "widgets")

	tests := []struct {
		branch string
		reason error
	}{
		{branch: "+main", reason: reposource.ErrRefPlusPrefix},
		{branch: "+refs/heads/main", reason: reposource.ErrRefPlusPrefix},
		{branch: ":main", reason: reposource.ErrRefNotBranchName},
		{branch: "work:main", reason: reposource.ErrRefNotBranchName},
		{branch: "refs/heads/*", reason: reposource.ErrRefNotBranchName},
		{branch: "@", reason: reposource.ErrRefNotBranchName},
	}
	for _, tc := range tests {
		t.Run(tc.branch, func(t *testing.T) {
			spec := sandboxws.PushReposElem{Name: "widgets", Branch: tc.branch}
			before := h.sup.SpawnCount()
			_, err := h.pushOneRepo(spec)
			if spawned := h.sup.SpawnCount() - before; spawned != 0 {
				t.Errorf("pushOneRepo(%+v) spawned %d process(es), want 0 -- git ran before the branch was refused", spec, spawned)
			}
			var refErr *reposource.InvalidRefError
			if !errors.As(err, &refErr) || !errors.Is(err, tc.reason) {
				t.Fatalf("pushOneRepo(%+v) error = %v, want the validator's *InvalidRefError wrapping %v", spec, err, tc.reason)
			}
		})
	}
}

// TestGitPushRefspecBranch_RealPremise pins, against the real git binary,
// why reposource.ValidateBranch refuses a leading "+" and a ":": pushed
// the way pushOneRepo pushes (`git push -- <remote> <branch>`), "+main"
// overwrites a remote main the local one does not contain, and ":feature"
// deletes the remote's feature. Plain "main" is refused as
// non-fast-forward, which is what a push from a sandbox must always get.
// ValidateBranch refuses both spellings and accepts "main".
func TestGitPushRefspecBranch_RealPremise(t *testing.T) {
	tests := []struct {
		branch string
		// effect checks the remote after the push, given the remote's
		// main and feature before it and the local main's commit.
		effect string
		reason error
	}{
		{branch: "main", effect: "unchanged"},
		{branch: "+main", effect: "main overwritten", reason: reposource.ErrRefPlusPrefix},
		{branch: ":feature", effect: "feature deleted", reason: reposource.ErrRefNotBranchName},
	}
	for _, tc := range tests {
		t.Run(tc.branch, func(t *testing.T) {
			tmp := t.TempDir()
			git := func(dir string, args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
				}
				return strings.TrimSpace(string(out))
			}

			// The remote: main at C1 then C2, and a feature branch.
			bareDir := filepath.Join(tmp, "remote.git")
			git(tmp, "init", "-q", "--bare", "-b", "main", bareDir)
			other := filepath.Join(tmp, "other")
			initRealGitRepoForPushTest(t, other)
			git(other, "remote", "add", "origin", bareDir)
			git(other, "commit", "-q", "--allow-empty", "-m", "C2")
			git(other, "push", "-q", "origin", "main", "main:feature")
			remoteMain := git(bareDir, "rev-parse", "main")

			// The sandbox: a clone at C1 with a commit of its own, C3.
			local := filepath.Join(tmp, "local")
			git(tmp, "clone", "-q", bareDir, local)
			git(local, "reset", "-q", "--hard", "HEAD~1")
			git(local, "commit", "-q", "--allow-empty", "-m", "C3")
			localMain := git(local, "rev-parse", "main")

			pushErr := exec.Command("git", "-C", local, "push", "--", "origin", tc.branch).Run()

			refs := git(bareDir, "for-each-ref", "--format=%(refname) %(objectname)")
			switch tc.effect {
			case "unchanged":
				if pushErr == nil || !strings.Contains(refs, "refs/heads/main "+remoteMain) || !strings.Contains(refs, "refs/heads/feature ") {
					t.Fatalf("git push -- origin %s: err %v, remote refs %q; want it refused and the remote unchanged", tc.branch, pushErr, refs)
				}
			case "main overwritten":
				if !strings.Contains(refs, "refs/heads/main "+localMain) {
					t.Fatalf("git push -- origin %s: remote refs %q; the premise no longer holds: it did not force main to %s", tc.branch, refs, localMain)
				}
			case "feature deleted":
				if strings.Contains(refs, "refs/heads/feature ") {
					t.Fatalf("git push -- origin %s: remote refs %q; the premise no longer holds: feature was not deleted", tc.branch, refs)
				}
			}

			err := reposource.ValidateBranch(tc.branch)
			if tc.reason == nil {
				if err != nil {
					t.Errorf("ValidateBranch(%q) = %v, want nil", tc.branch, err)
				}
			} else if !errors.Is(err, tc.reason) {
				t.Errorf("ValidateBranch(%q) = %v, want an error wrapping %v", tc.branch, err, tc.reason)
			}
		})
	}
}

// TestValidateBranch_AgreesWithGitCheckRefFormat holds reposource.
// ValidateBranch, which cannot run git itself (no I/O in /internal/domain),
// to the real git binary: it accepts a name exactly when `git check-ref-
// format --branch` does, run outside any repository so nothing like
// "@{-1}" is expanded, except for the names git accepts but would read as
// something other than that branch on `git push` -- a leading "+" (a
// forced push) and "@" (HEAD) -- which it refuses.
func TestValidateBranch_AgreesWithGitCheckRefFormat(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	gitAccepts := func(name string) bool {
		cmd := exec.Command("git", "check-ref-format", "--branch", name)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir))
		return cmd.Run() == nil
	}

	stricterThanGit := map[string]bool{"+main": true, "+": true, "++main": true, "+feature/x": true, "@": true}
	names := []string{
		// accepted by both
		"main", "feature/foo", "release/1.2.3", "narvi/0b9c6f0e-1d2a-4c3b-9e8f-7a6b5c4d3e2f",
		"a./b", "a.lock.b", "a.b/c", "café", "a{b}", "@a", "a@", "user@host", "a+b", "feature/+x",
		"a]b", "a!b", "a#b", "a'b", `a"b`, "a;b", "a|b", "a$b", "a%b", "a&b", "a=b", "a<b>", "a,b", "a`b",
		"refs/heads/main", "origin/main",
		// refused by both
		"", "-", "-x", "--force", "HEAD", "a b", "a\tb", "a\nb", "a\x7f", "a..b", "..", ".", ".a", "a.", "a/.b", "a/b.",
		"/", "/a", "a/", "a//b", "*", "refs/heads/*", "a*", ":", ":main", "work:main", "^main", "a^b", "main~1", "a~b",
		"a?b", "a[b", `a\b`, "main@{1}", "@{-1}", "a@{b", "main.lock", "a.lock/b", "a/b.lock/c",
	}
	for name := range stricterThanGit {
		names = append(names, name)
	}

	for _, name := range names {
		git := gitAccepts(name)
		ours := reposource.ValidateBranch(name) == nil
		switch {
		case stricterThanGit[name]:
			if !git || ours {
				t.Errorf("%q: git accepts=%v, ValidateBranch accepts=%v; want git to accept it and ValidateBranch to refuse it", name, git, ours)
			}
		case git != ours:
			t.Errorf("%q: git accepts=%v, ValidateBranch accepts=%v; want them to agree", name, git, ours)
		}
	}
}
