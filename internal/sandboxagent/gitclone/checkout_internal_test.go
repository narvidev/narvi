package gitclone

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClearInterruptedOperation: every entry an interrupted operation
// leaves under a worktree's .git is removed, and nothing else ever is, even
// when the worktree's .git has been swapped for a symlink after gitdir.Run's
// own check: out of the workspace, or to another repo's .git inside it.
func TestClearInterruptedOperation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// gitDir builds widgets/.git under workspace, and returns the
		// directory the entries are planted in.
		gitDir   func(t *testing.T, workspace string) string
		wantErr  bool
		wantGone bool
	}{
		{
			name: "a real .git",
			gitDir: func(t *testing.T, workspace string) string {
				t.Helper()
				dir := filepath.Join(workspace, "widgets", ".git")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return dir
			},
			wantGone: true,
		},
		{
			name: "a .git that is a symlink out of the workspace",
			gitDir: func(t *testing.T, workspace string) string {
				t.Helper()
				outside := t.TempDir()
				if err := os.MkdirAll(filepath.Join(workspace, "widgets"), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.Symlink(outside, filepath.Join(workspace, "widgets", ".git")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return outside
			},
			wantErr: true,
		},
		{
			name: "a .git that is a symlink to another repo's .git in the workspace",
			gitDir: func(t *testing.T, workspace string) string {
				t.Helper()
				other := filepath.Join(workspace, "other", ".git")
				if err := os.MkdirAll(other, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.MkdirAll(filepath.Join(workspace, "widgets"), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.Symlink(filepath.Join("..", "other", ".git"), filepath.Join(workspace, "widgets", ".git")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return other
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			workspace := t.TempDir()
			dir := tc.gitDir(t, workspace)
			planted := []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "rebase-merge/done", "BISECT_START"}
			for _, rel := range planted {
				path := filepath.Join(dir, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
					t.Fatalf("write %s: %v", rel, err)
				}
			}

			err := clearInterruptedOperation(workspace, "widgets")
			if (err != nil) != tc.wantErr {
				t.Fatalf("clearInterruptedOperation() = %v, want an error %v", err, tc.wantErr)
			}
			for _, rel := range planted {
				_, statErr := os.Lstat(filepath.Join(dir, rel))
				if gone := os.IsNotExist(statErr); gone != tc.wantGone {
					t.Errorf("%s gone = %v, want %v", rel, gone, tc.wantGone)
				}
			}
		})
	}
}

// TestDroppedIndexInfo: the entries diff-index lists as the index's and not
// the target's are written back with their own mode and object; an
// unmerged path the target lacks with the empty blob's; an unmerged path
// the target has, and a path git would never write, not at all; and
// output of any other shape is an error.
func TestDroppedIndexInfo(t *testing.T) {
	t.Parallel()
	const (
		zero = "0000000000000000000000000000000000000000"
		blob = "e5cdc04390f717a14e22f121e4d3ce271a8eebb0"
		link = "0c6117d9fb83be5d944c757c10508e44b4cf2b30"
	)
	record := func(targetMode, mode, targetObject, object, status, path string) string {
		return ":" + targetMode + " " + mode + " " + targetObject + " " + object + " " + status + "\x00" + path + "\x00"
	}
	tests := []struct {
		name    string
		listed  string
		want    string
		wantErr bool
	}{
		{name: "nothing listed", listed: "", want: ""},
		{
			name: "entries the target lacks keep their mode and object",
			listed: record("000000", "100644", zero, blob, "A", "dist/app.js") +
				record("000000", "120000", zero, link, "A", "lib with space "),
			want: "100644 " + blob + "\tdist/app.js\x00120000 " + link + "\tlib with space \x00",
		},
		{
			name:   "an unmerged path the target lacks gets the empty blob",
			listed: record("000000", "000000", zero, zero, "U", "dist/app.js"),
			want:   "100644 " + emptyBlobSHA1 + "\tdist/app.js\x00",
		},
		{
			name:   "an unmerged path the target has is left to the target's entry",
			listed: record("100644", "000000", blob, zero, "U", "pr.txt"),
			want:   "",
		},
		{
			name: "a path git would never write is left out",
			listed: record("000000", "100644", zero, blob, "A", "../escape") +
				record("000000", "100644", zero, blob, "A", "sub/.GIT/config") +
				record("000000", "100644", zero, blob, "A", "/abs"),
			want: "",
		},
		{name: "a record without its path", listed: ":000000 100644 " + zero + " " + blob + " A\x00", wantErr: true},
		{name: "a record of another shape", listed: "000000 100644 A\x00dist/app.js\x00", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := droppedIndexInfo(tc.listed)
			if (err != nil) != tc.wantErr {
				t.Fatalf("droppedIndexInfo() error = %v, want an error %v", err, tc.wantErr)
			}
			if string(got) != tc.want {
				t.Errorf("droppedIndexInfo() = %q, want %q", got, tc.want)
			}
		})
	}
}
