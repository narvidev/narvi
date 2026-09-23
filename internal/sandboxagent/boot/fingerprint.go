package boot

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// CollectFingerprint assembles the boot fingerprint §5.3 requires
// sandbox-agent log first -- before any other line -- directly from cfg,
// plus a best-effort repo-SHA discovery pass over layout.WorkspaceDir.
// repoSHATimeout bounds each individual repo's `git rev-parse` call (see
// DiscoverRepoSHAs); callers pass platform.Timeouts.RepoSHADiscoveryTimeout
// (never a literal -- this package must not import time.Duration unit
// literals per §5.4/§11, enforced by tools/lint/narvichecks/notimeliteral).
// stopGrace is platform.Timeouts.ProcessStopGracePeriod, threaded through
// to gitdir.Run's own bounded Stop call on a hang.
//
// Step 171 (§30.5): every repo-SHA read this function makes now goes
// through the SAME agent-owned git-dir every other sandbox-agent git
// invocation uses (internal/sandboxagent/gitdir) rather than a bare,
// unhardened `git -C <dir> rev-parse HEAD` against the runtime-owned
// worktree .git -- closed for free on a WARM boot (repo_image/
// snapshot_restore), where real repos already exist on disk before this
// is ever called: see cmd/sandbox-agent/main.go's own warm-mode Seed loop,
// which seeds every such repo's agent git-dir BEFORE this function's own
// first call, specifically so this read is already hardened even on the
// very first "boot fingerprint" log line. sup/layout/cred are threaded
// through from that same call site; cred is the runtime's own
// *syscall.Credential (nil/self in tests), needed by gitdir.Run's own
// SyncHeadOut bracket even though a plain rev-parse never actually moves
// HEAD (so that bracket is always a no-op here in practice).
func CollectFingerprint(ctx context.Context, sup *supervisor.Supervisor, cfg Config, layout gitdir.Layout, cred *syscall.Credential, repoSHATimeout, stopGrace time.Duration, openCodeVersion string) sandboxboot.BootFingerprint {
	return sandboxboot.BootFingerprint{
		AgentVersion:    cfg.AgentVersion,
		ImageDigest:     cfg.ImageDigest,
		BootMode:        cfg.BootMode,
		RepoSHAs:        DiscoverRepoSHAs(ctx, sup, layout, cred, repoSHATimeout, stopGrace),
		OpenCodeVersion: openCodeVersion,
	}
}

// DiscoverRepoSHAs globs the immediate subdirectories of layout.
// WorkspaceDir; for each that contains a .git entry AND an already-seeded
// agent git-dir (layout.Repo(name).GitDir -- a repo whose git-dir was
// never seeded has nothing this function could safely spawn hardened git
// against), reads its current HEAD via repoHeadSHA, bounded by timeout.
// Never returns an error: any single repo's SHA that can't be determined
// (workspaceDir itself missing, not a git repo, git-dir not yet seeded,
// git binary missing, command failed, timed out) is simply omitted from
// the returned map. This function does no logging of its own -- it stays
// a pure-ish, easily-testable function that returns data; the CALLER
// decides whether/how to log an omission (at most debug-level, per this
// Step's own instructions).
func DiscoverRepoSHAs(ctx context.Context, sup *supervisor.Supervisor, layout gitdir.Layout, cred *syscall.Credential, timeout, stopGrace time.Duration) map[string]string {
	shas := make(map[string]string)

	entries, err := os.ReadDir(layout.WorkspaceDir)
	if err != nil {
		return shas
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		repoDir := filepath.Join(layout.WorkspaceDir, entry.Name())
		if _, statErr := os.Stat(filepath.Join(repoDir, ".git")); statErr != nil {
			continue
		}

		repo := layout.Repo(entry.Name())
		if _, statErr := os.Stat(repo.GitDir); statErr != nil {
			// No agent git-dir seeded for this entry yet -- nothing safe to
			// spawn hardened git against (see this function's own doc
			// comment); omitted exactly like every other "can't determine"
			// case below.
			continue
		}

		sha, ok := repoHeadSHA(ctx, sup, repo, cred, timeout, stopGrace)
		if !ok {
			continue
		}
		shas[entry.Name()] = sha
	}

	return shas
}

// repoHeadSHA runs `git rev-parse HEAD` against repo -- through
// gitdir.Run, the same choke point (SyncHeadIn/SyncHeadOut bracket around
// a githarden.Args-hardened spawn) every other sandbox-agent git
// invocation uses -- bounded by timeout, returning (sha, true) on success
// or ("", false) on any failure whatsoever (non-git directory, git
// missing, non-zero exit, timeout, the repo/worktree symlink-swap guard
// Run itself enforces).
func repoHeadSHA(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, cred *syscall.Credential, timeout, stopGrace time.Duration) (string, bool) {
	var stdout bytes.Buffer
	spec := supervisor.Spec{
		Path:   "git",
		Args:   githarden.Args(repo, "rev-parse", "HEAD"),
		Env:    githarden.Env(nil),
		Stdout: &stdout,
	}
	result, err := gitdir.Run(ctx, sup, repo, cred, spec, timeout, stopGrace)
	if err != nil || result.Err != nil || result.ExitCode != 0 {
		return "", false
	}

	sha := strings.TrimSpace(stdout.String())
	if sha == "" {
		return "", false
	}
	return sha, true
}
