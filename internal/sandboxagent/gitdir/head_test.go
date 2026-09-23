package gitdir_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
)

// newHeadTestRepo builds the minimal shape SyncHeadIn needs: a
// repo.WorkTree/.git directory (real, not seeded -- SyncHeadIn only ever
// reads repo.WorkTree/.git/HEAD, never anything under repo.GitDir) and a
// repo.GitDir that already exists (SyncHeadIn writes repo.GitDir/HEAD via
// write-temp-then-rename, which needs its own parent directory present).
func newHeadTestRepo(t *testing.T) githarden.Repo {
	t.Helper()
	base := t.TempDir()
	wt := filepath.Join(base, "wt")
	gitDir := filepath.Join(base, "agent-gitdir")
	if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return githarden.Repo{WorkTree: wt, GitDir: gitDir}
}

func writeRuntimeHead(t *testing.T, repo githarden.Repo, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo.WorkTree, ".git", "HEAD"), content, 0o644); err != nil {
		t.Fatalf("write runtime HEAD: %v", err)
	}
}

func readAgentHead(t *testing.T, repo githarden.Repo) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo.GitDir, "HEAD"))
	if err != nil {
		t.Fatalf("read agent HEAD: %v", err)
	}
	return string(data)
}

// TestSyncHeadIn_AcceptsSymbolicRef pins the ordinary, overwhelmingly
// common case: a well-formed "ref: refs/heads/<branch>\n" is copied
// verbatim into the agent-owned HEAD.
func TestSyncHeadIn_AcceptsSymbolicRef(t *testing.T) {
	repo := newHeadTestRepo(t)
	writeRuntimeHead(t, repo, []byte("ref: refs/heads/main\n"))

	if err := gitdir.SyncHeadIn(repo); err != nil {
		t.Fatalf("SyncHeadIn() error = %v, want nil", err)
	}
	if got := readAgentHead(t, repo); got != "ref: refs/heads/main\n" {
		t.Errorf("agent HEAD = %q, want %q", got, "ref: refs/heads/main\n")
	}
}

// TestSyncHeadIn_AcceptsDetachedSHA pins the other accepted shape: a bare,
// lowercase, 40-character hex object id.
func TestSyncHeadIn_AcceptsDetachedSHA(t *testing.T) {
	repo := newHeadTestRepo(t)
	sha := strings.Repeat("a", 40) + "\n"
	writeRuntimeHead(t, repo, []byte(sha))

	if err := gitdir.SyncHeadIn(repo); err != nil {
		t.Fatalf("SyncHeadIn() error = %v, want nil", err)
	}
	if got := readAgentHead(t, repo); got != sha {
		t.Errorf("agent HEAD = %q, want %q", got, sha)
	}
}

// TestSyncHeadIn_RejectsSymlinkedHead is the defensive-read half of
// correction 1's own reasoning (run.go's doc comment on Run): SyncHeadIn's
// own O_NOFOLLOW open must refuse a HEAD path that is itself a symlink
// (e.g. planted by the runtime to point at an arbitrary file elsewhere),
// never silently follow it.
func TestSyncHeadIn_RejectsSymlinkedHead(t *testing.T) {
	repo := newHeadTestRepo(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	headPath := filepath.Join(repo.WorkTree, ".git", "HEAD")
	if err := os.Remove(headPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(target, headPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	err := gitdir.SyncHeadIn(repo)
	if err == nil {
		t.Fatal("SyncHeadIn() error = nil, want an error for a symlinked HEAD")
	}
	if strings.Contains(err.Error(), "ref: refs/heads/main") {
		t.Errorf("error text leaked the symlink target's own contents: %v", err)
	}
}

// TestSyncHeadIn_RejectsFIFO proves the O_NONBLOCK half of the same
// defensive open: a FIFO planted at the HEAD path must be refused
// promptly (never hang this read waiting for a writer that will never
// come).
func TestSyncHeadIn_RejectsFIFO(t *testing.T) {
	repo := newHeadTestRepo(t)
	headPath := filepath.Join(repo.WorkTree, ".git", "HEAD")
	if err := os.Remove(headPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove: %v", err)
	}
	if err := syscall.Mkfifo(headPath, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	done := make(chan error, 1)
	// errgroup.Group.Go, never a bare `go` statement -- §11/nakedgoroutine
	// grants no test exemption (mirrors githarden_test.go's own
	// startCaptureProxy precedent for the identical situation).
	var group errgroup.Group
	group.Go(func() error {
		done <- gitdir.SyncHeadIn(repo)
		return nil
	})
	t.Cleanup(func() { _ = group.Wait() })

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("SyncHeadIn() error = nil, want an error for a FIFO planted at HEAD")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SyncHeadIn() did not return promptly against a FIFO -- O_NONBLOCK did not do its job")
	}
}

// TestSyncHeadIn_RejectsOversizedFile proves the maxHeadBytes bound: a
// file larger than a real HEAD could ever legitimately be is refused
// before its full contents are ever read.
func TestSyncHeadIn_RejectsOversizedFile(t *testing.T) {
	repo := newHeadTestRepo(t)
	oversized := make([]byte, 8192)
	for i := range oversized {
		oversized[i] = 'a'
	}
	writeRuntimeHead(t, repo, oversized)

	err := gitdir.SyncHeadIn(repo)
	if err == nil {
		t.Fatal("SyncHeadIn() error = nil, want an error for an oversized HEAD")
	}
}

// TestSyncHeadIn_RejectsPathTraversalRef proves reposource.ValidateBranch
// is genuinely consulted, not merely a shape check: "ref: refs/heads/../x"
// parses as a syntactically well-formed symbolic ref line, but names an
// invalid branch (a path-traversal-shaped segment) that must never reach
// the agent-owned HEAD.
func TestSyncHeadIn_RejectsPathTraversalRef(t *testing.T) {
	repo := newHeadTestRepo(t)
	writeRuntimeHead(t, repo, []byte("ref: refs/heads/../x\n"))

	err := gitdir.SyncHeadIn(repo)
	if err == nil {
		t.Fatal("SyncHeadIn() error = nil, want an error for a path-traversal-shaped branch name")
	}
}

// TestSyncHeadIn_ErrorNeverIncludesBadBytes pins the doc comment's own
// explicit promise: a root process reading an attacker-chosen file and
// echoing it back into an error/log message is its own exfiltration
// primitive, distinct from the read itself being bounded and
// non-following. A garbage HEAD containing an obviously-distinctive
// marker must never have that marker surface in the returned error.
func TestSyncHeadIn_ErrorNeverIncludesBadBytes(t *testing.T) {
	repo := newHeadTestRepo(t)
	marker := "SUPER-SECRET-MARKER-VALUE-0xdeadbeef"
	writeRuntimeHead(t, repo, []byte(marker+"\n"))

	err := gitdir.SyncHeadIn(repo)
	if err == nil {
		t.Fatal("SyncHeadIn() error = nil, want an error for a garbage HEAD")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("error text leaked the offending HEAD bytes verbatim: %v", err)
	}
}
