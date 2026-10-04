package gitclone_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/gitstate"
	"github.com/narvidev/narvi/internal/sandboxagent/gitclone"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// These tests check out a pull request's head the way a review session
// does (technical plan §21.1, §30.4): from the base repository, served
// over a real git-http-backend, which keeps the head as refs/pull/7/head.

const testPullRef = "refs/pull/7/head"

// testHeadBranch is the pull request's head branch in the base repository
// itself -- a same-repository pull request -- at S1.
const testHeadBranch = "pr-branch"

// pullRefOrigin is a base repository whose main holds a file of its own
// (main-only.txt), and whose refs/pull/7/head holds S1, a contributor's
// commit off an older main, as does its branch pr-branch. S2, S1's child,
// exists only in the contributor's clone until advancePullRef pushes it.
type pullRefOrigin struct {
	url  string
	work string
	main string
	s1   string
	s2   string
}

func newPullRefOrigin(t *testing.T) *pullRefOrigin {
	t.Helper()
	parent := t.TempDir()
	bare := filepath.Join(parent, "widgets.git")
	runGit(t, parent, "init", "--bare", "-q", "-b", "main", bare)
	work := filepath.Join(t.TempDir(), "work")
	runGit(t, parent, "clone", "-q", bare, work)
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "Test")

	commit := func(file, content, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		runGit(t, work, "add", ".")
		runGit(t, work, "commit", "-q", "-m", message)
		return strings.TrimSpace(gitOutput(t, work, "rev-parse", "HEAD"))
	}

	o := &pullRefOrigin{work: work}
	commit("README.md", "base\n", "base")
	runGit(t, work, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	o.s1 = commit("pr.txt", "first\n", "the pull request's first head")
	o.s2 = commit("pr.txt", "second\n", "the pull request's second head")
	runGit(t, work, "checkout", "-q", "main")
	o.main = commit("main-only.txt", "main\n", "main moves on")
	runGit(t, work, "push", "-q", "origin", "main")
	runGit(t, work, "push", "-q", "origin", o.s1+":"+testPullRef)
	runGit(t, work, "push", "-q", "origin", o.s1+":refs/heads/"+testHeadBranch)

	o.url = startGitHTTPSServer(t, parent).URL + "/widgets.git"
	return o
}

// advancePullRef points the base repository's pull request ref at sha, as
// the code host does when the contributor pushes.
func (o *pullRefOrigin) advancePullRef(t *testing.T, sha string) {
	t.Helper()
	runGit(t, o.work, "push", "-q", "--force", "origin", sha+":"+testPullRef)
}

// clonedBase clones o's base repository at its default branch, without a
// ref, and seeds its agent git-dir -- a review sandbox's workspace before
// any pull request checkout.
func clonedBase(t *testing.T, o *pullRefOrigin) (gitdir.Layout, sessionconfig.SessionConfigReposElem, string) {
	t.Helper()
	layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: t.TempDir()}
	repo := sessionconfig.SessionConfigReposElem{Name: "widgets", Url: o.url}
	results, err := gitclone.CloneAll(context.Background(), supervisor.New(), layout, nil, nil,
		[]sessionconfig.SessionConfigReposElem{repo}, nil, testCloneTimeout, testStopGrace)
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("CloneAll() = %+v, %v, want one cloned repo", results, err)
	}
	return layout, repo, filepath.Join(layout.WorkspaceDir, "widgets")
}

func checkoutPullRef(layout gitdir.Layout, repo sessionconfig.SessionConfigReposElem, ref, want string, pathScope []string) gitclone.PullCheckoutResult {
	return gitclone.CheckoutPullRef(context.Background(), supervisor.New(), layout, nil, nil, repo, ref, want, pathScope,
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace)
}

func headOf(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
}

func porcelain(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(gitOutput(t, dir, "status", "--porcelain"))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func strPtr(s string) *string { return &s }

// TestCheckoutPullRef_ChecksOutTheRecordedHead: the recorded head is
// fetched from the base repository's pull ref and checked out, detached --
// in the runtime's own worktree too -- and the base's main is left
// behind.
func TestCheckoutPullRef_ChecksOutTheRecordedHead(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout, repo, dir := clonedBase(t, o)

	got := checkoutPullRef(layout, repo, testPullRef, o.s1, nil)
	if got.Outcome != gitclone.PullCheckoutCheckedOut || got.Err != nil {
		t.Fatalf("CheckoutPullRef() = %+v, want checked_out", got)
	}
	if got.HeadSHA != o.s1 || got.RefSHA != o.s1 {
		t.Errorf("HeadSHA, RefSHA = %s, %s, want both %s", got.HeadSHA, got.RefSHA, o.s1)
	}
	if head := headOf(t, dir); head != o.s1 {
		t.Errorf("worktree HEAD = %s, want %s", head, o.s1)
	}
	runtimeHead, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
	if err != nil {
		t.Fatalf("read the runtime's HEAD: %v", err)
	}
	if string(runtimeHead) != o.s1+"\n" {
		t.Errorf("runtime HEAD = %q, want detached at %s", runtimeHead, o.s1)
	}
	if exists(filepath.Join(dir, "main-only.txt")) {
		t.Error("main-only.txt is in the worktree: the base's main was left checked out")
	}
	if status := porcelain(t, dir); status != "" {
		t.Errorf("status = %q, want clean", status)
	}
}

// TestCheckoutPullRef_AbsentHeadLeavesTheTreeUntouched: a recorded head the
// ref has not reached yet is sha_absent, the ref's tip reported, and the
// worktree left exactly as it was, edits included.
func TestCheckoutPullRef_AbsentHeadLeavesTheTreeUntouched(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout, repo, dir := clonedBase(t, o)
	before := headOf(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("an edit\n"), 0o644); err != nil {
		t.Fatalf("edit README.md: %v", err)
	}

	got := checkoutPullRef(layout, repo, testPullRef, o.s2, nil)
	if got.Outcome != gitclone.PullCheckoutSHAAbsent || got.Err == nil {
		t.Fatalf("CheckoutPullRef() = %+v, want sha_absent with its reason", got)
	}
	if got.RefSHA != o.s1 || got.HeadSHA != "" {
		t.Errorf("RefSHA, HeadSHA = %q, %q, want %s and none", got.RefSHA, got.HeadSHA, o.s1)
	}
	if head := headOf(t, dir); head != before {
		t.Errorf("worktree HEAD = %s, want it untouched at %s", head, before)
	}
	if status := porcelain(t, dir); status != "M README.md" {
		t.Errorf("status = %q, want the edit untouched", status)
	}
}

// TestCheckoutPullRef_AdvancedRef: once the ref has moved on, its new tip
// is checked out when asked for; and a head it moved past, already
// fetched, is still checked out exactly, the ref's newer tip reported
// beside it so the caller can tell the two apart.
func TestCheckoutPullRef_AdvancedRef(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout, repo, dir := clonedBase(t, o)
	if got := checkoutPullRef(layout, repo, testPullRef, o.s1, nil); got.Outcome != gitclone.PullCheckoutCheckedOut {
		t.Fatalf("first CheckoutPullRef() = %+v, want checked_out", got)
	}

	o.advancePullRef(t, o.s2)
	got := checkoutPullRef(layout, repo, testPullRef, o.s2, nil)
	if got.Outcome != gitclone.PullCheckoutCheckedOut || got.HeadSHA != o.s2 || got.RefSHA != o.s2 {
		t.Fatalf("CheckoutPullRef(S2) = %+v, want checked_out at %s", got, o.s2)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "pr.txt")); string(body) != "second\n" {
		t.Errorf("pr.txt = %q, want S2's", body)
	}

	got = checkoutPullRef(layout, repo, testPullRef, o.s1, nil)
	if got.Outcome != gitclone.PullCheckoutCheckedOut || got.HeadSHA != o.s1 || got.RefSHA != o.s2 {
		t.Fatalf("CheckoutPullRef(S1) after the ref moved = %+v, want checked_out at %s with the ref at %s", got, o.s1, o.s2)
	}
	if head := headOf(t, dir); head != o.s1 {
		t.Errorf("worktree HEAD = %s, want %s", head, o.s1)
	}
}

// TestCheckoutPullRef_WithoutAHeadChecksOutTheRefsTip is the boot's call:
// no recorded head, so the ref's tip.
func TestCheckoutPullRef_WithoutAHeadChecksOutTheRefsTip(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout, repo, dir := clonedBase(t, o)

	got := checkoutPullRef(layout, repo, testPullRef, "", nil)
	if got.Outcome != gitclone.PullCheckoutCheckedOut || got.HeadSHA != o.s1 || got.RefSHA != o.s1 {
		t.Fatalf("CheckoutPullRef() = %+v, want checked_out at the ref's tip %s", got, o.s1)
	}
	if head := headOf(t, dir); head != o.s1 {
		t.Errorf("worktree HEAD = %s, want %s", head, o.s1)
	}
}

// TestCheckoutPullRef_FetchFailureLeavesTheTreeUntouched: a ref the base
// repository does not have, and a base repository that cannot be reached,
// are each fetch_failed, git's reason carried, the worktree untouched.
func TestCheckoutPullRef_FetchFailureLeavesTheTreeUntouched(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ref  string
		url  func(o *pullRefOrigin) string
		want string
	}{
		{name: "a ref the base repository does not have", ref: "refs/pull/99/head", url: func(o *pullRefOrigin) string { return o.url }, want: "refs/pull/99/head"},
		{name: "a base repository that cannot be reached", ref: testPullRef, url: func(o *pullRefOrigin) string { return o.url + "-gone" }, want: "fetch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newPullRefOrigin(t)
			layout, repo, dir := clonedBase(t, o)
			before := headOf(t, dir)
			repo.Url = tc.url(o)
			runGit(t, layout.Repo("widgets").GitDir, "config", "remote.origin.url", repo.Url)

			got := checkoutPullRef(layout, repo, tc.ref, o.s1, nil)
			if got.Outcome != gitclone.PullCheckoutFetchFailed || got.Err == nil {
				t.Fatalf("CheckoutPullRef() = %+v, want fetch_failed with its reason", got)
			}
			if !strings.Contains(got.Err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", got.Err, tc.want)
			}
			if got.RefSHA != "" || got.HeadSHA != "" {
				t.Errorf("RefSHA, HeadSHA = %q, %q, want none", got.RefSHA, got.HeadSHA)
			}
			if head := headOf(t, dir); head != before {
				t.Errorf("worktree HEAD = %s, want it untouched at %s", head, before)
			}
		})
	}
}

// TestCheckoutPullRef_DiscardsEveryEditAndKeepsIgnoredFiles is a warm
// re-review's worktree: the previous turn edited a tracked file, staged
// another change, deleted a file and left untracked files and directories
// behind, plus an ignored one. The checkout leaves exactly the recorded
// head, clean, with the ignored file -- what setup installed -- kept.
func TestCheckoutPullRef_DiscardsEveryEditAndKeepsIgnoredFiles(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout, repo, dir := clonedBase(t, o)
	if got := checkoutPullRef(layout, repo, testPullRef, o.s1, nil); got.Outcome != gitclone.PullCheckoutCheckedOut {
		t.Fatalf("first CheckoutPullRef() = %+v, want checked_out", got)
	}

	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("pr.txt", "the previous turn's edit\n")
	write("README.md", "a staged edit\n")
	runGit(t, dir, "add", "README.md")
	if err := os.Remove(filepath.Join(dir, ".gitignore")); err != nil {
		t.Fatalf("delete .gitignore: %v", err)
	}
	write("untracked.txt", "left behind\n")
	write("scratch/nested.txt", "left behind\n")
	if err := os.WriteFile(filepath.Join(dir, "deps.log"), []byte("installed by setup\n"), 0o644); err != nil {
		t.Fatalf("write deps.log: %v", err)
	}
	o.advancePullRef(t, o.s2)

	got := checkoutPullRef(layout, repo, testPullRef, o.s2, nil)
	if got.Outcome != gitclone.PullCheckoutCheckedOut || got.HeadSHA != o.s2 {
		t.Fatalf("CheckoutPullRef() = %+v, want checked_out at %s", got, o.s2)
	}
	if status := porcelain(t, dir); status != "" {
		t.Errorf("status = %q, want clean: an edit survived the checkout", status)
	}
	for _, gone := range []string{"untracked.txt", "scratch"} {
		if exists(filepath.Join(dir, gone)) {
			t.Errorf("%s survived the checkout", gone)
		}
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "pr.txt")); string(body) != "second\n" {
		t.Errorf("pr.txt = %q, want S2's", body)
	}
	if !exists(filepath.Join(dir, "deps.log")) {
		t.Error("deps.log, an ignored file, was removed")
	}
}

// TestCheckoutPullRef_PathScope: a scoped session's checkout leaves only
// its paths on disk, and an unscoped session's turns sparse-checkout off
// on a worktree that held it.
func TestCheckoutPullRef_PathScope(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout, repo, dir := clonedBase(t, o)

	got := checkoutPullRef(layout, repo, testPullRef, o.s1, []string{"/pr.txt"})
	if got.Outcome != gitclone.PullCheckoutCheckedOut {
		t.Fatalf("scoped CheckoutPullRef() = %+v, want checked_out", got)
	}
	if !exists(filepath.Join(dir, "pr.txt")) || exists(filepath.Join(dir, "README.md")) {
		t.Errorf("scoped worktree: pr.txt present %v, README.md present %v, want only pr.txt", exists(filepath.Join(dir, "pr.txt")), exists(filepath.Join(dir, "README.md")))
	}

	got = checkoutPullRef(layout, repo, testPullRef, o.s1, nil)
	if got.Outcome != gitclone.PullCheckoutCheckedOut {
		t.Fatalf("unscoped CheckoutPullRef() = %+v, want checked_out", got)
	}
	if !exists(filepath.Join(dir, "README.md")) {
		t.Error("README.md is still out of the worktree: an unscoped checkout kept the earlier scope")
	}
	if enabled := gitOutputAllowFailure(t, dir, "config", "--type=bool", "core.sparseCheckout"); enabled == "true" {
		t.Error("the runtime's core.sparseCheckout is still true after an unscoped checkout")
	}
}

// TestCheckoutPullRef_InvalidInputSpawnsNothing: a repo name, ref, commit
// or path scope out of shape is failed before any git process is spawned,
// and the worktree is untouched.
func TestCheckoutPullRef_InvalidInputSpawnsNothing(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout, repo, dir := clonedBase(t, o)
	before := headOf(t, dir)

	tests := []struct {
		name      string
		repoName  string
		ref       string
		sha       string
		pathScope []string
	}{
		{name: "a path escape for a name", repoName: "../widgets", ref: testPullRef, sha: o.s1},
		{name: "a branch for a ref", repoName: "widgets", ref: "refs/heads/main", sha: o.s1},
		{name: "an option for a ref", repoName: "widgets", ref: "--upload-pack=touch /tmp/pwned", sha: o.s1},
		{name: "an abbreviated commit", repoName: "widgets", ref: testPullRef, sha: o.s1[:12]},
		{name: "a revision expression", repoName: "widgets", ref: testPullRef, sha: "HEAD~1"},
		{name: "an invalid path scope", repoName: "widgets", ref: testPullRef, sha: o.s1, pathScope: []string{"[unclosed"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bad := repo
			bad.Name = tc.repoName
			got := checkoutPullRef(layout, bad, tc.ref, tc.sha, tc.pathScope)
			if got.Outcome != gitclone.PullCheckoutFailed || got.Err == nil || got.RefSHA != "" {
				t.Fatalf("CheckoutPullRef() = %+v, want failed before any fetch", got)
			}
		})
	}
	if head := headOf(t, dir); head != before {
		t.Errorf("worktree HEAD = %s, want it untouched at %s", head, before)
	}
	if out := gitOutputAllowFailure(t, dir, "rev-parse", "--verify", "--quiet", gitclone.PullHeadLocalRef); out != "" {
		t.Errorf("%s = %s, want it never fetched", gitclone.PullHeadLocalRef, out)
	}
}

// TestCheckoutPullRef_SpawnCount pins the git processes one checkout
// spawns -- one fetch, and at most PullCheckoutMaxLocalGitSpawns local
// ones, each with its own bound -- along its three paths, the longest
// reaching that maximum exactly. The bound on a whole checkout is built
// from these counts, so a step added without counting it fails here.
func TestCheckoutPullRef_SpawnCount(t *testing.T) {
	o := newPullRefOrigin(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("exec.LookPath(git): %v", err)
	}

	tests := []struct {
		name         string
		sparse       bool // the worktree is left sparse before the checkout
		skipWorktree bool // README.md carries the skip-worktree bit, its file present
		pathScope    []string
		wantLocal    int
	}{
		{name: "unscoped, never sparse", wantLocal: 12},
		{name: "unscoped, left sparse", sparse: true, wantLocal: 12},
		{name: "scoped", pathScope: []string{"/pr.txt"}, wantLocal: 13},
		{name: "scoped, a skip-worktree file present", skipWorktree: true, pathScope: []string{"/pr.txt"}, wantLocal: gitclone.PullCheckoutMaxLocalGitSpawns},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			layout, repo, dir := clonedBase(t, o)
			if tc.sparse {
				if got := checkoutPullRef(layout, repo, testPullRef, o.s1, []string{"/pr.txt"}); got.Outcome != gitclone.PullCheckoutCheckedOut {
					t.Fatalf("scoping CheckoutPullRef() = %+v", got)
				}
				// Back to main, so the counted checkout moves HEAD and
				// writes the runtime's, as every path counted here does.
				runGit(t, dir, "checkout", "-q", "--detach", o.main)
			}
			if tc.skipWorktree {
				runGit(t, dir, "update-index", "--skip-worktree", "README.md")
			}

			fakeDir := t.TempDir()
			capture := filepath.Join(t.TempDir(), "spawns.txt")
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + capture + "\"\nexec \"" + realGit + "\" \"$@\"\n"
			if err := os.WriteFile(filepath.Join(fakeDir, "git"), []byte(script), 0o755); err != nil {
				t.Fatalf("write the counting git: %v", err)
			}
			t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			got := checkoutPullRef(layout, repo, testPullRef, o.s1, tc.pathScope)
			if got.Outcome != gitclone.PullCheckoutCheckedOut {
				t.Fatalf("CheckoutPullRef() = %+v, want checked_out", got)
			}
			raw, err := os.ReadFile(capture)
			if err != nil {
				t.Fatalf("read the spawn capture: %v", err)
			}
			var fetches, local int
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				if strings.Contains(" "+line+" ", " fetch ") {
					fetches++
				} else {
					local++
				}
			}
			if fetches != gitclone.PullCheckoutNetworkGitSpawns || local != tc.wantLocal {
				t.Errorf("spawned %d fetches and %d local git processes, want %d and %d:\n%s",
					fetches, local, gitclone.PullCheckoutNetworkGitSpawns, tc.wantLocal, raw)
			}
			if local > gitclone.PullCheckoutMaxLocalGitSpawns {
				t.Errorf("%d local git processes, over PullCheckoutMaxLocalGitSpawns (%d)", local, gitclone.PullCheckoutMaxLocalGitSpawns)
			}
		})
	}
}

// TestCloneAll_PullRef_ForkHeadBranchIsNeverCloned is a fork's pull
// request: the branch names the contributor's head branch, which the base
// repository does not have. The clone never asks for it -- a clone of a
// branch the base lacks fails the boot -- and the worktree holds the pull
// ref's tip, detached, not the base's main.
func TestCloneAll_PullRef_ForkHeadBranchIsNeverCloned(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: t.TempDir()}
	branch, ref := "feature", testPullRef
	repos := []sessionconfig.SessionConfigReposElem{{Name: "widgets", Url: o.url, Branch: &branch, Ref: &ref}}

	results, err := gitclone.CloneAll(context.Background(), supervisor.New(), layout, nil, nil, repos, nil, testCloneTimeout, testStopGrace)
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("CloneAll() = %+v, %v, want one cloned repo", results, err)
	}
	dir := filepath.Join(layout.WorkspaceDir, "widgets")
	if head := headOf(t, dir); head != o.s1 {
		t.Errorf("worktree HEAD = %s, want the pull ref's tip %s (main is %s)", head, o.s1, o.main)
	}
	if exists(filepath.Join(dir, "main-only.txt")) {
		t.Error("main-only.txt is in the worktree: the base's main was left checked out")
	}
}

// TestCloneAll_PullRef_UnfetchableRefChecksOutTheHeadBranch: a fresh boot
// whose pull ref cannot be fetched right after the clone never stays on the
// clone's default branch. The spec names the head branch, which a
// same-repository pull request has in the base repository, so the clone's
// own origin/<branch> is checked out -- what `clone --branch` checked out
// before refs existed -- and then scoped.
func TestCloneAll_PullRef_UnfetchableRefChecksOutTheHeadBranch(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: t.TempDir()}
	branch, ref := testHeadBranch, "refs/pull/99/head"
	repos := []sessionconfig.SessionConfigReposElem{{Name: "widgets", Url: o.url, Branch: &branch, Ref: &ref}}

	results, err := gitclone.CloneAll(context.Background(), supervisor.New(), layout, nil, nil, repos, []string{"/pr.txt"}, testCloneTimeout, testStopGrace)
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("CloneAll() = %+v, %v, want the head branch checked out", results, err)
	}
	dir := filepath.Join(layout.WorkspaceDir, "widgets")
	if head := headOf(t, dir); head != o.s1 {
		t.Errorf("worktree HEAD = %s, want the head branch's %s (the default branch is %s)", head, o.s1, o.main)
	}
	if got := currentBranch(t, dir); got != testHeadBranch {
		t.Errorf("checked-out branch = %q, want %q", got, testHeadBranch)
	}
	if exists(filepath.Join(dir, "README.md")) || !exists(filepath.Join(dir, "pr.txt")) {
		t.Error("the path scope was not applied to the head branch")
	}
}

// TestCloneAll_PullRef_UnfetchableRefFailsClosed: with no head branch to
// fall back to -- none in the spec, or one the base repository does not
// have -- a fresh boot whose pull ref cannot be fetched fails the primary
// repo, never booting the review on the clone's default branch.
func TestCloneAll_PullRef_UnfetchableRefFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		branch *string
	}{
		{name: "no head branch in the spec"},
		{name: "a head branch the base repository does not have", branch: strPtr("feature")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newPullRefOrigin(t)
			layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: t.TempDir()}
			ref := "refs/pull/99/head"
			repos := []sessionconfig.SessionConfigReposElem{{Name: "widgets", Url: o.url, Branch: tc.branch, Ref: &ref}}

			results, err := gitclone.CloneAll(context.Background(), supervisor.New(), layout, nil, nil, repos, nil, testCloneTimeout, testStopGrace)
			if err == nil || len(results) != 1 || results[0].Err == nil {
				t.Fatalf("CloneAll() = %+v, %v, want the primary repo failed", results, err)
			}
			if !strings.Contains(results[0].Err.Error(), "never boots on another tree") {
				t.Errorf("error = %v, want it to say the review never boots on another tree", results[0].Err)
			}
		})
	}
}

// TestCloneAll_PullRef_InvalidRefRejectedBeforeAnySpawn: a ref out of
// shape fails the repo before anything is cloned.
func TestCloneAll_PullRef_InvalidRefRejectedBeforeAnySpawn(t *testing.T) {
	t.Parallel()
	layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: t.TempDir()}
	ref := "--upload-pack=touch /tmp/pwned"
	repos := []sessionconfig.SessionConfigReposElem{{Name: "widgets", Url: "https://example.invalid/widgets.git", Ref: &ref}}

	results, err := gitclone.CloneAll(context.Background(), supervisor.New(), layout, nil, nil, repos, nil, testCloneTimeout, testStopGrace)
	if err == nil || len(results) != 1 || results[0].Err == nil || results[0].Dir != "" {
		t.Fatalf("CloneAll() = %+v, %v, want the primary repo refused before any directory or spawn", results, err)
	}
}

// TestSyncAll_PullRef_WarmBootDiscardsThePreviousTurnsEdits is a review
// sandbox restored from its snapshot: the previous turn's edits are in the
// worktree and the ref has moved on. The boot checks out the ref's new
// tip, clean, without stashing anything, and never enters the branch
// machine.
func TestSyncAll_PullRef_WarmBootDiscardsThePreviousTurnsEdits(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: t.TempDir()}
	ref := testPullRef
	repos := []sessionconfig.SessionConfigReposElem{{Name: "widgets", Url: o.url, Ref: &ref}}
	if results, err := gitclone.CloneAll(context.Background(), supervisor.New(), layout, nil, nil, repos, nil, testCloneTimeout, testStopGrace); err != nil || results[0].Err != nil {
		t.Fatalf("CloneAll() = %+v, %v", results, err)
	}
	dir := filepath.Join(layout.WorkspaceDir, "widgets")
	if err := os.WriteFile(filepath.Join(dir, "pr.txt"), []byte("the previous turn's edit\n"), 0o644); err != nil {
		t.Fatalf("edit pr.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("left behind\n"), 0o644); err != nil {
		t.Fatalf("write untracked.txt: %v", err)
	}
	o.advancePullRef(t, o.s2)

	var events []gitSyncEvent
	results, err := gitclone.SyncAll(context.Background(), supervisor.New(), layout, nil, repos, nil, "22222222-2222-2222-2222-222222222222",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, recordingOnGitSync(&events), noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("SyncAll() = %+v, %v, want one synced repo", results, err)
	}
	if results[0].State != "" || results[0].Branch != "" {
		t.Errorf("State, Branch = %q, %q, want none: a pull ref never enters the branch machine", results[0].State, results[0].Branch)
	}
	if head := headOf(t, dir); head != o.s2 {
		t.Errorf("worktree HEAD = %s, want the ref's new tip %s", head, o.s2)
	}
	if status := porcelain(t, dir); status != "" {
		t.Errorf("status = %q, want clean", status)
	}
	if stashes := strings.TrimSpace(gitOutput(t, dir, "stash", "list")); stashes != "" {
		t.Errorf("stash list = %q, want nothing stashed", stashes)
	}
	if len(events) != 1 || events[0].status != "checkout" || events[0].branch != testPullRef {
		t.Errorf("events = %#v, want one checkout of %s", events, testPullRef)
	}
}

// TestSyncAll_PullRef_UnfetchableRefSyncsTheHeadBranch: a warm boot (a
// repo image, here built from the default branch) whose pull ref cannot be
// fetched takes the §19.3 branch sequence for the spec's head branch, as it
// did before refs existed: the branch is fetched and checked out, and the
// boot never stays on the image's default branch.
func TestSyncAll_PullRef_UnfetchableRefSyncsTheHeadBranch(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout, _, dir := clonedBase(t, o)
	branch, ref := testHeadBranch, "refs/pull/99/head"
	repos := []sessionconfig.SessionConfigReposElem{{Name: "widgets", Url: o.url, Branch: &branch, Ref: &ref}}

	results, err := gitclone.SyncAll(context.Background(), supervisor.New(), layout, nil, repos, nil, "33333333-3333-3333-3333-333333333333",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("SyncAll() = %+v, %v, want the head branch synced", results, err)
	}
	if results[0].State != gitstate.StateReady || results[0].Branch != testHeadBranch {
		t.Errorf("State, Branch = %q, %q, want ready on %q", results[0].State, results[0].Branch, testHeadBranch)
	}
	if head := headOf(t, dir); head != o.s1 {
		t.Errorf("worktree HEAD = %s, want the head branch's %s (the image's default branch is %s)", head, o.s1, o.main)
	}
}

// TestSyncAll_PullRef_UnfetchableRefFailsClosed: a warm boot whose pull ref
// cannot be fetched fails the primary repo when the head branch is neither
// local nor fetchable either (§19.3's rule for an explicit branch), or when
// the spec names none -- the worktree left on the image's tree, its path
// scope enforced, and never booted as the review's.
func TestSyncAll_PullRef_UnfetchableRefFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		branch    *string
		ref       string
		goneURL   bool
		wantState gitstate.State
	}{
		{name: "head branch neither local nor fetchable", branch: strPtr(testHeadBranch), ref: testPullRef, goneURL: true, wantState: gitstate.StateFetchFailed},
		{name: "no head branch in the spec", ref: "refs/pull/99/head"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newPullRefOrigin(t)
			layout, _, dir := clonedBase(t, o)
			url := o.url
			if tc.goneURL {
				url += "-gone"
			}
			ref := tc.ref
			repos := []sessionconfig.SessionConfigReposElem{{Name: "widgets", Url: url, Branch: tc.branch, Ref: &ref}}

			results, err := gitclone.SyncAll(context.Background(), supervisor.New(), layout, nil, repos, []string{"/README.md"}, "44444444-4444-4444-4444-444444444444",
				testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
			if err == nil || len(results) != 1 || results[0].Err == nil {
				t.Fatalf("SyncAll() = %+v, %v, want the primary repo failed", results, err)
			}
			if results[0].State != tc.wantState {
				t.Errorf("State = %q, want %q", results[0].State, tc.wantState)
			}
			if head := headOf(t, dir); head != o.main {
				t.Errorf("worktree HEAD = %s, want it left on the image's %s", head, o.main)
			}
			if exists(filepath.Join(dir, "main-only.txt")) {
				t.Error("main-only.txt is in the worktree: the path scope was not enforced on the failed repo")
			}
		})
	}
}

// skipWorktreeEntries is what `git ls-files -v` marks skip-worktree in dir:
// the paths whose tag is S, or s with assume-unchanged too.
func skipWorktreeEntries(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(gitOutput(t, dir, "ls-files", "-v")), "\n") {
		if strings.HasPrefix(line, "S ") || strings.HasPrefix(line, "s ") {
			paths = append(paths, line[2:])
		}
	}
	return paths
}

// skipWorktreeCase is a previous turn that left skip-worktree state in the
// shared index, and what a checkout of S2 must leave instead.
type skipWorktreeCase struct {
	name      string
	pathScope []string
	// plant runs in the runtime's worktree, as the previous turn's agent.
	plant func(t *testing.T, dir string)
	// check runs after the checkout of S2.
	check func(t *testing.T, dir string)
}

func plantSkipWorktree(file, content string) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		runGit(t, dir, "update-index", "--skip-worktree", file)
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
}

func wantContent(file, content string) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		if body, err := os.ReadFile(filepath.Join(dir, file)); err != nil || string(body) != content {
			t.Errorf("%s = %q (%v), want %q", file, body, err, content)
		}
	}
}

func skipWorktreeCases() []skipWorktreeCase {
	return []skipWorktreeCase{
		{
			name:  "update-index on a file the next head leaves alone",
			plant: plantSkipWorktree("README.md", "PLANTED readme\n"),
			check: wantContent("README.md", "base\n"),
		},
		{
			name: "the runtime's own sparse-checkout",
			plant: func(t *testing.T, dir string) {
				t.Helper()
				runGit(t, dir, "sparse-checkout", "set", "--no-cone", "/README.md")
			},
			check: func(t *testing.T, dir string) {
				t.Helper()
				if !exists(filepath.Join(dir, ".gitignore")) || !exists(filepath.Join(dir, "pr.txt")) {
					t.Error(".gitignore or pr.txt is missing: the runtime's sparse patterns survived")
				}
				if got := gitOutputAllowFailure(t, dir, "config", "--type=bool", "core.sparseCheckout"); got == "true" {
					t.Error("the runtime's core.sparseCheckout is still true")
				}
			},
		},
		{
			name:  "update-index on a file the next head changes",
			plant: plantSkipWorktree("pr.txt", "PLANTED pr\n"),
			check: wantContent("pr.txt", "second\n"),
		},
		{
			// git clears the bit of a present file itself when it reads the
			// index with sparse-checkout on, which every scoped checkout
			// leaves on in the agent's config. With the runtime's own
			// sparse-checkout turned off first, a boot's Seed imports it
			// off, and only the explicit clear keeps the scope's set from
			// reporting the planted file as left on disk.
			name:      "the runtime turned its scope off and marked an in-scope file",
			pathScope: []string{"/pr.txt"},
			plant: func(t *testing.T, dir string) {
				t.Helper()
				runGit(t, dir, "sparse-checkout", "disable")
				plantSkipWorktree("pr.txt", "PLANTED pr\n")(t, dir)
			},
			check: func(t *testing.T, dir string) {
				t.Helper()
				wantContent("pr.txt", "second\n")(t, dir)
				if exists(filepath.Join(dir, "README.md")) {
					t.Error("README.md, out of scope, is in the worktree")
				}
			},
		},
		{
			name:      "update-index on an in-scope file of a scoped session",
			pathScope: []string{"/pr.txt"},
			plant:     plantSkipWorktree("pr.txt", "PLANTED pr\n"),
			check: func(t *testing.T, dir string) {
				t.Helper()
				wantContent("pr.txt", "second\n")(t, dir)
				if exists(filepath.Join(dir, "README.md")) {
					t.Error("README.md, out of scope, is in the worktree")
				}
			},
		},
	}
}

// assertCleanAtS2 is what every skipWorktreeCase leaves after a checkout of
// S2: HEAD at S2, a clean status, no skip-worktree bit but the scope's own,
// and the case's own check.
func assertCleanAtS2(t *testing.T, o *pullRefOrigin, dir string, tc skipWorktreeCase) {
	t.Helper()
	if head := headOf(t, dir); head != o.s2 {
		t.Errorf("worktree HEAD = %s, want %s", head, o.s2)
	}
	if status := porcelain(t, dir); status != "" {
		t.Errorf("status = %q, want clean", status)
	}
	for _, path := range skipWorktreeEntries(t, dir) {
		if len(tc.pathScope) == 0 || path == "pr.txt" {
			t.Errorf("%s still carries the skip-worktree bit", path)
		}
	}
	tc.check(t, dir)
}

// TestCheckoutPullRef_ClearsAPreviousTurnsSkipWorktreeState: the index is
// shared with the runtime, and a forced checkout never rewrites an entry
// carrying the skip-worktree bit, nor does clean remove its file. A
// previous turn's `update-index --skip-worktree` or its own sparse-checkout
// would otherwise survive a checkout reported checked_out: a planted file
// kept, tracked files missing, or every later checkout failing on a file
// the heads change. The checkout command's path.
func TestCheckoutPullRef_ClearsAPreviousTurnsSkipWorktreeState(t *testing.T) {
	t.Parallel()
	for _, tc := range skipWorktreeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newPullRefOrigin(t)
			layout, repo, dir := clonedBase(t, o)
			if got := checkoutPullRef(layout, repo, testPullRef, o.s1, tc.pathScope); got.Outcome != gitclone.PullCheckoutCheckedOut {
				t.Fatalf("first CheckoutPullRef() = %+v, want checked_out", got)
			}
			tc.plant(t, dir)
			o.advancePullRef(t, o.s2)

			got := checkoutPullRef(layout, repo, testPullRef, o.s2, tc.pathScope)
			if got.Outcome != gitclone.PullCheckoutCheckedOut || got.HeadSHA != o.s2 {
				t.Fatalf("CheckoutPullRef(S2) = %+v, want checked_out at %s", got, o.s2)
			}
			assertCleanAtS2(t, o, dir, tc)
		})
	}
}

// TestSyncAll_PullRef_ClearsAPreviousTurnsSkipWorktreeState is the same
// for a warm boot: a review sandbox restored with the previous turn's
// skip-worktree state boots at the ref's new tip, exactly.
func TestSyncAll_PullRef_ClearsAPreviousTurnsSkipWorktreeState(t *testing.T) {
	t.Parallel()
	for _, tc := range skipWorktreeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newPullRefOrigin(t)
			layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: t.TempDir()}
			ref := testPullRef
			repos := []sessionconfig.SessionConfigReposElem{{Name: "widgets", Url: o.url, Ref: &ref}}
			if results, err := gitclone.CloneAll(context.Background(), supervisor.New(), layout, nil, nil, repos, tc.pathScope, testCloneTimeout, testStopGrace); err != nil || results[0].Err != nil {
				t.Fatalf("CloneAll() = %+v, %v", results, err)
			}
			dir := filepath.Join(layout.WorkspaceDir, "widgets")
			tc.plant(t, dir)
			o.advancePullRef(t, o.s2)

			results, err := gitclone.SyncAll(context.Background(), supervisor.New(), layout, nil, repos, tc.pathScope, "55555555-5555-5555-5555-555555555555",
				testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
			if err != nil || len(results) != 1 || results[0].Err != nil {
				t.Fatalf("SyncAll() = %+v, %v, want one synced repo", results, err)
			}
			assertCleanAtS2(t, o, dir, tc)
		})
	}
}

// TestCheckoutPullRef_ReownsTheWorktreeOnEveryOutcomeAfterTheFetch: the
// fetch and everything after it run as sandbox-agent's own identity and
// can write into the runtime's tree -- new object directories on a
// sha_absent fetch alone -- so the worktree is re-owned on every outcome
// from the fetch on, and only an input refused before any git ran skips
// it.
func TestCheckoutPullRef_ReownsTheWorktreeOnEveryOutcomeAfterTheFetch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		ref         string
		want        func(o *pullRefOrigin) string
		pathScope   []string
		prepare     func(t *testing.T, o *pullRefOrigin, layout gitdir.Layout, repo sessionconfig.SessionConfigReposElem, dir string)
		wantOutcome gitclone.PullCheckoutOutcome
		wantReowns  int
	}{
		{name: "checked_out", ref: testPullRef, want: func(o *pullRefOrigin) string { return o.s1 },
			wantOutcome: gitclone.PullCheckoutCheckedOut, wantReowns: 1},
		{name: "sha_absent", ref: testPullRef, want: func(o *pullRefOrigin) string { return o.s2 },
			wantOutcome: gitclone.PullCheckoutSHAAbsent, wantReowns: 1},
		{name: "fetch_failed", ref: "refs/pull/99/head", want: func(o *pullRefOrigin) string { return o.s1 },
			wantOutcome: gitclone.PullCheckoutFetchFailed, wantReowns: 1},
		{
			name: "failed after the fetch", ref: testPullRef, want: func(o *pullRefOrigin) string { return o.s1 },
			pathScope: []string{"/pr.txt"},
			prepare: func(t *testing.T, o *pullRefOrigin, layout gitdir.Layout, repo sessionconfig.SessionConfigReposElem, dir string) {
				t.Helper()
				// An edit to a file the scope leaves out: the scope's set
				// will not remove a changed file, and says so.
				if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("an out-of-scope edit\n"), 0o644); err != nil {
					t.Fatalf("edit README.md: %v", err)
				}
			},
			wantOutcome: gitclone.PullCheckoutFailed, wantReowns: 1,
		},
		{name: "failed before any git", ref: testPullRef, want: func(*pullRefOrigin) string { return "HEAD~1" },
			wantOutcome: gitclone.PullCheckoutFailed, wantReowns: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newPullRefOrigin(t)
			layout, repo, dir := clonedBase(t, o)
			if tc.prepare != nil {
				tc.prepare(t, o, layout, repo, dir)
			}
			var reowned []string
			chown := func(path string) error {
				reowned = append(reowned, path)
				return nil
			}
			got := gitclone.CheckoutPullRef(context.Background(), supervisor.New(), layout, nil, chown, repo, tc.ref, tc.want(o), tc.pathScope,
				testFetchStepTimeout, testSyncStepTimeout, testStopGrace)
			if got.Outcome != tc.wantOutcome {
				t.Fatalf("CheckoutPullRef() = %+v, want %s", got, tc.wantOutcome)
			}
			if len(reowned) != tc.wantReowns {
				t.Fatalf("re-owned %d times, want %d", len(reowned), tc.wantReowns)
			}
			for _, path := range reowned {
				if path != dir {
					t.Errorf("re-owned %s, want the worktree %s", path, dir)
				}
			}
		})
	}
}
