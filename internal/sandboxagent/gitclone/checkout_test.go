package gitclone_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/sandboxagent/gitclone"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// These tests check out a pull request's head the way a review session
// does (technical plan §21.1, §30.4): from the base repository, served
// over a real git-http-backend, which keeps the head as refs/pull/7/head.

const testPullRef = "refs/pull/7/head"

// pullRefOrigin is a base repository whose main holds a file of its own
// (main-only.txt), and whose refs/pull/7/head holds S1, a contributor's
// commit off an older main. S2, S1's child, exists only in the
// contributor's clone until advancePullRef pushes it.
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
		name      string
		sparse    bool // the worktree is left sparse before the checkout
		pathScope []string
		wantLocal int
	}{
		{name: "unscoped, never sparse", wantLocal: 7},
		{name: "scoped", pathScope: []string{"/pr.txt"}, wantLocal: 12},
		{name: "unscoped, left sparse", sparse: true, wantLocal: gitclone.PullCheckoutMaxLocalGitSpawns},
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

// TestCloneAll_PullRef_RefThatCannotBeFetchedIsAWarning: the boot goes on,
// on the clone's default branch, scoped like any clone.
func TestCloneAll_PullRef_RefThatCannotBeFetchedIsAWarning(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: t.TempDir()}
	ref := "refs/pull/99/head"
	repos := []sessionconfig.SessionConfigReposElem{{Name: "widgets", Url: o.url, Ref: &ref}}

	results, err := gitclone.CloneAll(context.Background(), supervisor.New(), layout, nil, nil, repos, []string{"/README.md"}, testCloneTimeout, testStopGrace)
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("CloneAll() = %+v, %v, want the boot to go on", results, err)
	}
	dir := filepath.Join(layout.WorkspaceDir, "widgets")
	if head := headOf(t, dir); head != o.main {
		t.Errorf("worktree HEAD = %s, want the default branch's %s", head, o.main)
	}
	if exists(filepath.Join(dir, "main-only.txt")) {
		t.Error("main-only.txt is in the worktree: the path scope was not applied")
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

// TestSyncAll_PullRef_RefThatCannotBeFetchedIsAWarning: the warm boot goes
// on, the worktree as it was, its scope still enforced.
func TestSyncAll_PullRef_RefThatCannotBeFetchedIsAWarning(t *testing.T) {
	t.Parallel()
	o := newPullRefOrigin(t)
	layout, _, dir := clonedBase(t, o)
	ref := "refs/pull/99/head"
	repos := []sessionconfig.SessionConfigReposElem{{Name: "widgets", Url: o.url, Ref: &ref}}

	results, err := gitclone.SyncAll(context.Background(), supervisor.New(), layout, nil, repos, []string{"/README.md"}, "33333333-3333-3333-3333-333333333333",
		testFetchStepTimeout, testSyncStepTimeout, testStopGrace, func(string, string, string) {}, noopGitFetchTiming, noopGitCheckoutTiming)
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("SyncAll() = %+v, %v, want the boot to go on", results, err)
	}
	if head := headOf(t, dir); head != o.main {
		t.Errorf("worktree HEAD = %s, want it as it was at %s", head, o.main)
	}
	if exists(filepath.Join(dir, "main-only.txt")) {
		t.Error("main-only.txt is in the worktree: the path scope was not enforced")
	}
}
