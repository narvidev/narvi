// This file pins Seed's own round-5 fix directly: os.RemoveAll(repo.GitDir)
// is now the FIRST action Seed takes, before any guard that could refuse
// the call (symlinked worktree/.git, an unsupported on-disk layout). Before
// this fix, a refused Seed call left whatever agent git-dir a PRIOR,
// successful Seed call had built still on disk -- and every
// boot.CollectFingerprint call this package's own callers make relies on
// DiscoverRepoSHAs' plain os.Stat(repo.GitDir) gate (boot/fingerprint.go)
// to skip a repo that was never (re-)seeded. A stale git-dir left behind by
// a refused Seed defeated that gate: it looked exactly like a legitimately
// seeded one. See cmd/sandbox-agent/bootfingerprint_test.go's own
// TestBootFingerprintAndSeed_SecondaryFailure_LaterNilAllowedCallAlsoExcludesIt
// for the end-to-end reproduction against a real warm-boot sequence.
package gitdir_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// TestSeed_RefusalRemovesPreexistingAgentGitDir seeds a repo successfully
// once (leaving a real agent git-dir on disk), plants each of Seed's own
// refusal conditions in turn, and proves that a second, refused Seed call
// both fails AND removes the agent git-dir that was already there --
// exactly the structural guarantee every later, unrestricted discovery
// call needs.
func TestSeed_RefusalRemovesPreexistingAgentGitDir(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, wt, otherRepo string)
	}{
		{
			name: "symlinked worktree .git",
			plant: func(t *testing.T, wt, otherRepo string) {
				t.Helper()
				wtGit := filepath.Join(wt, ".git")
				if err := os.RemoveAll(wtGit); err != nil {
					t.Fatalf("remove wt/.git: %v", err)
				}
				if err := os.Symlink(filepath.Join(otherRepo, ".git"), wtGit); err != nil {
					t.Fatalf("symlink wt/.git -> other repo: %v", err)
				}
			},
		},
		{
			name: "symlinked worktree itself",
			plant: func(t *testing.T, wt, otherRepo string) {
				t.Helper()
				if err := os.RemoveAll(wt); err != nil {
					t.Fatalf("remove wt: %v", err)
				}
				if err := os.Symlink(otherRepo, wt); err != nil {
					t.Fatalf("symlink wt -> other repo: %v", err)
				}
			},
		},
		{
			name: "shallow",
			plant: func(t *testing.T, wt, _ string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(wt, ".git", "shallow"), []byte("deadbeef\n"), 0o644); err != nil {
					t.Fatalf("write .git/shallow: %v", err)
				}
			},
		},
		{
			name: "reftable",
			plant: func(t *testing.T, wt, _ string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(wt, ".git", "reftable"), []byte(""), 0o644); err != nil {
					t.Fatalf("write .git/reftable: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			workspaceDir := filepath.Join(base, "workspace")
			wt := filepath.Join(workspaceDir, "repo1")
			initRunTestRepo(t, wt)

			// A second, entirely unrelated repository -- only consumed by
			// the two symlink-swap cases above, harmless to create for the
			// others.
			otherRepo := filepath.Join(base, "other-repo")
			initRunTestRepo(t, otherRepo)

			gitDirRoot := filepath.Join(base, "gitdirs")
			if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
				t.Fatalf("gitdir.EnsureRoot: %v", err)
			}
			layout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
			repo := layout.Repo("repo1")
			sup := supervisor.New()
			ctx := context.Background()

			// Seed once, successfully -- leaves a real agent git-dir on
			// disk, exactly like a repo seeded on a PRIOR boot.
			if err := gitdir.Seed(ctx, sup, repo, "https://example.invalid/repo1.git", nil, 10*time.Second, 5*time.Second); err != nil {
				t.Fatalf("precondition: gitdir.Seed() error = %v, want nil (must succeed once, to leave an agent git-dir behind)", err)
			}
			if _, statErr := os.Stat(repo.GitDir); statErr != nil {
				t.Fatalf("precondition: agent git-dir missing after first Seed: %v", statErr)
			}

			tc.plant(t, wt, otherRepo)

			err := gitdir.Seed(ctx, sup, repo, "https://example.invalid/repo1.git", nil, 10*time.Second, 5*time.Second)
			if err == nil {
				t.Fatalf("gitdir.Seed() after planting %q: error = nil, want a refusal", tc.name)
			}

			if _, statErr := os.Stat(repo.GitDir); !os.IsNotExist(statErr) {
				t.Errorf("gitdir.Seed() after planting %q: agent git-dir stat error = %v, want os.IsNotExist -- a refused Seed call must remove the agent git-dir entirely, so a later, unrestricted DiscoverRepoSHAs call can never find it", tc.name, statErr)
			}
		})
	}
}
