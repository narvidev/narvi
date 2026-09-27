package boot

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestChownTree_EntriesThatVanishMidWalk drives chownTree through an
// injected lchown that reproduces, at an exact point, what a concurrent
// writer does to the tree while the real walk runs: a file or directory
// that WalkDir has already listed disappears (or disappears and comes back)
// before it is re-owned. That is how the push integration test's own
// `git commit` failed boot in CI -- "lchown .../.git/HEAD.lock: no such
// file or directory" -- and how any services.yml process writing in its
// checkout can fail cmd/sandbox-agent's post-boot pass.
func TestChownTree_EntriesThatVanishMidWalk(t *testing.T) {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	enoent := &fs.PathError{Op: "lchown", Err: syscall.ENOENT}

	tests := []struct {
		name string
		// rel is the entry the wrapper acts on, relative to the root.
		rel string
		// act runs instead of the real Lchown for rel. call counts the
		// calls made for rel so far, starting at 1.
		act     func(t *testing.T, path string, call int) error
		wantErr error // nil means chownTree must succeed
	}{
		{
			name: "file removed before its own chown",
			rel:  "repo/.git/HEAD.lock",
			act: func(t *testing.T, path string, _ int) error {
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove %s: %v", path, err)
				}
				return os.Lchown(path, int(uid), int(gid))
			},
		},
		{
			name: "directory removed before its own chown",
			rel:  "repo/node_modules/.cache",
			act: func(t *testing.T, path string, _ int) error {
				if err := os.RemoveAll(path); err != nil {
					t.Fatalf("remove %s: %v", path, err)
				}
				return os.Lchown(path, int(uid), int(gid))
			},
		},
		{
			// Re-owned, then gone before WalkDir can read it: the ReadDir
			// error reaches the walk function as walkErr.
			name: "directory removed after its chown, before it is read",
			rel:  "repo/node_modules/.cache",
			act: func(t *testing.T, path string, _ int) error {
				if err := os.Lchown(path, int(uid), int(gid)); err != nil {
					return err
				}
				if err := os.RemoveAll(path); err != nil {
					t.Fatalf("remove %s: %v", path, err)
				}
				return nil
			},
		},
		{
			// ENOENT, yet the name exists again by the time it is checked
			// (git recreates index.lock on its next operation): re-owned on
			// the next attempt, not skipped.
			name: "name recreated after an ENOENT is re-owned",
			rel:  "repo/.git/index.lock",
			act: func(_ *testing.T, path string, call int) error {
				if call == 1 {
					return enoent
				}
				return os.Lchown(path, int(uid), int(gid))
			},
		},
		{
			// What a symlink-following chown reports for a dangling link:
			// ENOENT for an entry that plainly exists. Must stay an error.
			name:    "ENOENT for an entry that still exists is an error",
			rel:     "repo/escape-link",
			act:     func(*testing.T, string, int) error { return enoent },
			wantErr: syscall.ENOENT,
		},
		{
			name:    "any other error still aborts the walk",
			rel:     "repo/main.go",
			act:     func(*testing.T, string, int) error { return &fs.PathError{Op: "lchown", Err: syscall.EPERM} },
			wantErr: syscall.EPERM,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"repo/.git", "repo/node_modules/.cache/nested"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
			}
			for _, file := range []string{"repo/main.go", "repo/.git/HEAD", "repo/.git/HEAD.lock", "repo/.git/index.lock", "repo/node_modules/.cache/nested/blob"} {
				if err := os.WriteFile(filepath.Join(root, file), []byte("x\n"), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "never-created"), filepath.Join(root, "repo/escape-link")); err != nil {
				t.Fatalf("Symlink: %v", err)
			}

			target := filepath.Join(root, tc.rel)
			calls := 0
			var visited []string
			lchown := func(path string, uid, gid int) error {
				visited = append(visited, path)
				if path == target {
					calls++
					return tc.act(t, path, calls)
				}
				return os.Lchown(path, uid, gid)
			}

			err := chownTree(root, uid, gid, lchown)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("chownTree() error = %v, want one wrapping %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("chownTree() error = %v, want nil -- an entry that vanished mid-walk has nothing left to re-own", err)
			}
			if calls == 0 {
				t.Fatalf("the walk never reached %s, so this case proved nothing", target)
			}
			// The walk must have carried on past the vanished entry, not
			// stopped at it: the last entry in lexical order is still visited.
			if last := filepath.Join(root, "repo/node_modules/.cache/nested/blob"); tc.rel != "repo/node_modules/.cache" && visited[len(visited)-1] != last {
				t.Errorf("last visited entry = %s, want %s (the walk must continue after a vanished entry)", visited[len(visited)-1], last)
			}
			if tc.rel == "repo/.git/index.lock" && calls != 2 {
				t.Errorf("calls for the recreated name = %d, want 2 (one ENOENT, then re-owned)", calls)
			}
		})
	}
}

// TestChownTree_MissingRootIsStillAnError pins that tolerating vanished
// entries never extends to workspaceDir itself.
func TestChownTree_MissingRootIsStillAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err := chownTree(missing, uint32(os.Getuid()), uint32(os.Getgid()), os.Lchown)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("chownTree(missing root) error = %v, want one wrapping fs.ErrNotExist", err)
	}
}
