package boot_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// seedFingerprintRepo builds a fresh agent-owned git-dir for the
// already-existing, non-bare repo at workspaceDir/name -- §30.5:
// DiscoverRepoSHAs now only reads a repo whose agent git-dir has
// already been seeded (see that function's own doc comment), so every
// test repo in this file needs one before DiscoverRepoSHAs/
// CollectFingerprint can see it at all.
func seedFingerprintRepo(t *testing.T, workspaceDir, name string) (gitdir.Layout, *supervisor.Supervisor) {
	t.Helper()
	root := t.TempDir()
	if err := gitdir.EnsureRoot(root); err != nil {
		t.Fatalf("gitdir.EnsureRoot: %v", err)
	}
	layout := gitdir.Layout{Root: root, WorkspaceDir: workspaceDir}
	sup := supervisor.New()
	if err := gitdir.Seed(context.Background(), sup, layout.Repo(name), "https://example.invalid/"+name+".git", nil, 5*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed: %v", err)
	}
	return layout, sup
}

func TestDiscoverRepoSHAs(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()

	gitRepoDir := filepath.Join(workspaceDir, "repo-a")
	mkdirAll(t, gitRepoDir)
	initGitRepo(t, gitRepoDir)

	plainDir := filepath.Join(workspaceDir, "not-a-repo")
	mkdirAll(t, plainDir)

	layout, sup := seedFingerprintRepo(t, workspaceDir, "repo-a")
	shas := boot.DiscoverRepoSHAs(context.Background(), sup, layout, nil, 5*time.Second, 5*time.Second)

	sha, ok := shas["repo-a"]
	if !ok {
		t.Fatalf("DiscoverRepoSHAs()[%q] missing, want a real SHA; got map %v", "repo-a", shas)
	}
	if len(sha) != 40 {
		t.Errorf("repo-a SHA = %q (len %d), want a 40-char hex SHA", sha, len(sha))
	}

	if _, ok := shas["not-a-repo"]; ok {
		t.Errorf("DiscoverRepoSHAs()[%q] present, want absent (not a git repo)", "not-a-repo")
	}
}

func TestDiscoverRepoSHAs_NonexistentWorkspace(t *testing.T) {
	t.Parallel()

	workspaceDir := filepath.Join(t.TempDir(), "does-not-exist")
	layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}
	sup := supervisor.New()
	shas := boot.DiscoverRepoSHAs(context.Background(), sup, layout, nil, 5*time.Second, 5*time.Second)
	if len(shas) != 0 {
		t.Errorf("DiscoverRepoSHAs() = %v, want empty map for a nonexistent workspaceDir", shas)
	}
}

func TestCollectFingerprint(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo-a")
	mkdirAll(t, repoDir)
	initGitRepo(t, repoDir)

	cfg := boot.Config{
		BootMode:     sandboxboot.BootModeFresh,
		AgentVersion: "1.2.3",
		ImageDigest:  "sha256:deadbeef",
		WorkspaceDir: workspaceDir,
	}

	layout, sup := seedFingerprintRepo(t, workspaceDir, "repo-a")
	fp := boot.CollectFingerprint(context.Background(), sup, cfg, layout, nil, 5*time.Second, 5*time.Second, "")

	if fp.AgentVersion != cfg.AgentVersion {
		t.Errorf("AgentVersion = %q, want %q", fp.AgentVersion, cfg.AgentVersion)
	}
	if fp.ImageDigest != cfg.ImageDigest {
		t.Errorf("ImageDigest = %q, want %q", fp.ImageDigest, cfg.ImageDigest)
	}
	if fp.BootMode != cfg.BootMode {
		t.Errorf("BootMode = %q, want %q", fp.BootMode, cfg.BootMode)
	}
	if _, ok := fp.RepoSHAs["repo-a"]; !ok {
		t.Errorf("RepoSHAs missing repo-a entry: %v", fp.RepoSHAs)
	}
	if fp.OpenCodeVersion != "" {
		t.Errorf("OpenCodeVersion = %q, want empty when not yet discovered", fp.OpenCodeVersion)
	}
}

func TestCollectFingerprint_OpenCodeVersion(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	cfg := boot.Config{
		BootMode:     sandboxboot.BootModeFresh,
		AgentVersion: "1.2.3",
		WorkspaceDir: workspaceDir,
	}

	layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}
	sup := supervisor.New()
	fp := boot.CollectFingerprint(context.Background(), sup, cfg, layout, nil, 5*time.Second, 5*time.Second, "1.17.15")
	if fp.OpenCodeVersion != "1.17.15" {
		t.Errorf("OpenCodeVersion = %q, want %q", fp.OpenCodeVersion, "1.17.15")
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
}

// initGitRepo initializes a real, tiny git repo at dir with one commit, so
// `git rev-parse HEAD` has a real SHA to return.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()

	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	runGit(t, dir, "add", "file.txt")
	runGit(t, dir, "commit", "-m", "initial commit")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
