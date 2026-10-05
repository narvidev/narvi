package gitclone

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClearInterruptedOperation: every entry an interrupted operation
// leaves under a worktree's .git is removed, and nothing outside the
// workspace ever is, even when the worktree's .git has been swapped for a
// symlink out of it after gitdir.Run's own check.
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
