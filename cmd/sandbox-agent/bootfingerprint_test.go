// This file pins §5.3's "boot fingerprint is the very first logged line"
// invariant directly against bootFingerprintAndSeed, at unit-test
// granularity, without booting run() itself (which blocks on OS signals /
// a live WS bridge / a real opencode spawn -- see main_test.go's own doc
// comment for the identical reasoning). The round-2 adversarial review of
// this PR found that a primary-repo Seed failure, or a gitdir.EnsureRoot
// failure, made run() exit before ever logging the fingerprint -- and that
// a secondary-repo Seed warning was logged BEFORE the fingerprint even on
// a successful boot. bootFingerprintAndSeed was factored out of run()
// specifically so this ordering is testable in isolation (see its own doc
// comment, main.go).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// installDefaultTestLogger builds a JSON logger writing to a buffer and
// installs it as slog.Default() for the duration of the test (restored via
// t.Cleanup) -- exactly mirroring production's own run() (main.go), which
// calls slog.SetDefault(logger) right after building the identical logger,
// so any global slog.* call anywhere in the seed path -- not just the
// explicit *slog.Logger bootFingerprintAndSeed itself is handed -- lands in
// the SAME stream the fingerprint line does, in the same relative order.
//
// (Round-3 review, Q5): the tests below used to build a private
// platform.NewLogger(&buf, ...) and pass it ONLY as bootFingerprintAndSeed's
// explicit logger argument, never installing it as slog.Default(). A
// global slog call reached through the seed path (e.g. the exact
// round-2 regression: an inline slog.Warn in seedWarmBootRepos, logged
// before the fingerprint) went to Go's own default stderr handler and
// never reached the captured buffer, so these tests could not have caught
// that regression coming back. Callers of this helper must NOT use
// t.Parallel(): slog.SetDefault mutates process-global state, so two such
// tests running concurrently would race each other's captured output.
func installDefaultTestLogger(t *testing.T, level slog.Level) (*bytes.Buffer, *slog.Logger) {
	t.Helper()
	var buf bytes.Buffer
	logger := platform.NewLogger(&buf, level)
	orig := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf, logger
}

// loggedRepoSHAs parses buf the same way loggedMessages does and returns the
// "repo_shas" field of the FIRST logged line (the §5.3 fingerprint, per this
// file's own bootFingerprintMsg invariant).
func loggedRepoSHAs(t *testing.T, buf *bytes.Buffer) map[string]string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("loggedRepoSHAs: buf has no logged lines")
	}
	var entry struct {
		Msg      string            `json:"msg"`
		RepoSHAs map[string]string `json:"repo_shas"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal first log line %q: %v", lines[0], err)
	}
	if entry.Msg != bootFingerprintMsg {
		t.Fatalf("first logged line msg = %q, want %q", entry.Msg, bootFingerprintMsg)
	}
	return entry.RepoSHAs
}

// loggedMessages parses buf as a stream of JSON log lines (platform.
// NewLogger's own slog.NewJSONHandler shape) and returns each line's "msg"
// field, in emission order.
func loggedMessages(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var msgs []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		msgs = append(msgs, entry.Msg)
	}
	return msgs
}

const bootFingerprintMsg = "sandbox-agent: boot fingerprint"

// TestBootFingerprintAndSeed_PrimaryFailure_FingerprintLoggedFirst proves
// the §5.3 fingerprint line is still logged, and is still the FIRST logged
// line, even when the primary repo's Seed call fails fatally -- the exact
// scenario (a `.git/shallow` marker from a real `git fetch --depth=1`,
// exactly as a repo_image/snapshot_restore boot can produce) the round-2
// review reproduced.
func TestBootFingerprintAndSeed_PrimaryFailure_FingerprintLoggedFirst(t *testing.T) {
	// Deliberately NOT t.Parallel() -- installDefaultTestLogger installs a
	// process-global slog.Default(), see its own doc comment.

	workspaceDir := t.TempDir()
	seedWarmBootTestBadRepo(t, workspaceDir, "bad-primary")

	gitDirRoot := t.TempDir()
	cfg := boot.Config{
		GitDirRoot:   gitDirRoot,
		WorkspaceDir: workspaceDir,
		SessionConfig: &sessionconfig.SessionConfig{
			Repos: []sessionconfig.SessionConfigReposElem{
				{Name: "bad-primary", Url: "https://example.invalid/bad-primary.git"},
			},
		},
	}
	layout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}

	buf, logger := installDefaultTestLogger(t, cfg.LogLevel)

	err := bootFingerprintAndSeed(context.Background(), supervisor.New(), cfg, layout, nil, platform.DefaultTimeouts(), logger)
	if err == nil {
		t.Fatal("bootFingerprintAndSeed() error = nil, want a fatal error for the failed primary repo's Seed call")
	}

	msgs := loggedMessages(t, buf)
	if len(msgs) == 0 {
		t.Fatal("bootFingerprintAndSeed() logged nothing, want the boot fingerprint line even on a primary Seed failure")
	}
	if msgs[0] != bootFingerprintMsg {
		t.Errorf("first logged line = %q, want %q (§5.3: fingerprint must be first, even on a fatal primary Seed failure)", msgs[0], bootFingerprintMsg)
	}
}

// TestBootFingerprintAndSeed_EnsureRootFailure_FingerprintLoggedFirst
// proves the same invariant when gitdir.EnsureRoot itself fails (e.g. a
// GitDirRoot whose parent path component is not a directory) -- the other
// fatal path identified by the round-2 review that used to return from
// run() before ever logging the fingerprint.
func TestBootFingerprintAndSeed_EnsureRootFailure_FingerprintLoggedFirst(t *testing.T) {
	// Deliberately NOT t.Parallel() -- installDefaultTestLogger installs a
	// process-global slog.Default(), see its own doc comment.

	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	// gitdir.EnsureRoot's own os.MkdirAll must fail: "blocker" is a
	// regular file, not a directory, so nothing can be created under it.
	gitDirRoot := filepath.Join(blocker, "root")

	workspaceDir := t.TempDir()
	cfg := boot.Config{
		GitDirRoot:   gitDirRoot,
		WorkspaceDir: workspaceDir,
		SessionConfig: &sessionconfig.SessionConfig{
			Repos: []sessionconfig.SessionConfigReposElem{
				{Name: "primary", Url: "https://example.invalid/primary.git"},
			},
		},
	}
	layout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}

	buf, logger := installDefaultTestLogger(t, cfg.LogLevel)

	err := bootFingerprintAndSeed(context.Background(), supervisor.New(), cfg, layout, nil, platform.DefaultTimeouts(), logger)
	if err == nil {
		t.Fatal("bootFingerprintAndSeed() error = nil, want a fatal error for the failed gitdir.EnsureRoot call")
	}

	msgs := loggedMessages(t, buf)
	if len(msgs) == 0 {
		t.Fatal("bootFingerprintAndSeed() logged nothing, want the boot fingerprint line even on an EnsureRoot failure")
	}
	if msgs[0] != bootFingerprintMsg {
		t.Errorf("first logged line = %q, want %q (§5.3: fingerprint must be first, even on a fatal EnsureRoot failure)", msgs[0], bootFingerprintMsg)
	}
}

// TestBootFingerprintAndSeed_SecondaryFailure_FingerprintPrecedesWarning
// proves that on a successful boot with a non-fatal secondary-repo Seed
// failure, the fingerprint line still precedes the warning -- the ordering
// the round-2 review found broken even on the success path (the inline
// warning used to be logged before the fingerprint).
func TestBootFingerprintAndSeed_SecondaryFailure_FingerprintPrecedesWarning(t *testing.T) {
	// Deliberately NOT t.Parallel() -- installDefaultTestLogger installs a
	// process-global slog.Default(), see its own doc comment.

	workspaceDir := t.TempDir()
	seedWarmBootTestGoodRepo(t, workspaceDir, "primary")
	seedWarmBootTestBadRepo(t, workspaceDir, "bad-secondary")

	gitDirRoot := t.TempDir()
	cfg := boot.Config{
		GitDirRoot:   gitDirRoot,
		WorkspaceDir: workspaceDir,
		SessionConfig: &sessionconfig.SessionConfig{
			Repos: []sessionconfig.SessionConfigReposElem{
				{Name: "primary", Url: "https://example.invalid/primary.git"},
				{Name: "bad-secondary", Url: "https://example.invalid/bad-secondary.git"},
			},
		},
	}
	layout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}

	buf, logger := installDefaultTestLogger(t, cfg.LogLevel)

	err := bootFingerprintAndSeed(context.Background(), supervisor.New(), cfg, layout, nil, platform.DefaultTimeouts(), logger)
	if err != nil {
		t.Fatalf("bootFingerprintAndSeed() error = %v, want nil (a secondary repo's Seed failure is a warning, not fatal)", err)
	}

	msgs := loggedMessages(t, buf)
	if len(msgs) < 2 {
		t.Fatalf("bootFingerprintAndSeed() logged %d lines, want at least 2 (fingerprint, then the secondary warning): %v", len(msgs), msgs)
	}
	if msgs[0] != bootFingerprintMsg {
		t.Errorf("first logged line = %q, want %q (§5.3: fingerprint must precede the secondary-repo warning)", msgs[0], bootFingerprintMsg)
	}
	foundWarning := false
	for _, m := range msgs[1:] {
		if strings.Contains(m, "secondary repo failed") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("logged messages = %v, want a secondary-repo-failed warning after the fingerprint", msgs)
	}
}

// TestBootFingerprintAndSeed_EnsureRootRejected_NoDiscoveryNoGitSpawned
// reproduces the R3 finding directly: gitdir.EnsureRoot refuses cfg.
// GitDirRoot (here, mode 0777 -- group/other-writable), and
// <GitDirRoot>/primary is planted as a symlink to a "victim" directory
// outside the workspace entirely. Before the fix, boot.CollectFingerprint
// ran anyway with the same (rejected) layout: DiscoverRepoSHAs would stat
// through the symlink, and repoHeadSHA (via gitdir.Run/SyncHeadIn) would
// write victim/HEAD and spawn `git rev-parse HEAD` against victim as
// sandbox-agent, reporting whatever SHA the attacker-controlled victim
// directory chose. This proves neither happens: victim/HEAD is never
// created, and the fingerprint's own repo_shas is empty.
func TestBootFingerprintAndSeed_EnsureRootRejected_NoDiscoveryNoGitSpawned(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	seedWarmBootTestGoodRepo(t, workspaceDir, "primary")

	gitDirRoot := t.TempDir()
	if err := os.Chmod(gitDirRoot, 0o777); err != nil {
		t.Fatalf("chmod gitDirRoot 0777: %v", err)
	}

	// victim is deliberately OUTSIDE gitDirRoot and workspaceDir -- an
	// attacker-controlled directory the planted symlink points at. It has
	// no HEAD file yet, so a write reaching it is directly observable.
	victim := t.TempDir()
	if err := os.Symlink(victim, filepath.Join(gitDirRoot, "primary")); err != nil {
		t.Fatalf("symlink %s/primary -> victim: %v", gitDirRoot, err)
	}

	cfg := boot.Config{
		GitDirRoot:   gitDirRoot,
		WorkspaceDir: workspaceDir,
		SessionConfig: &sessionconfig.SessionConfig{
			Repos: []sessionconfig.SessionConfigReposElem{
				{Name: "primary", Url: "https://example.invalid/primary.git"},
			},
		},
	}
	layout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}

	var buf bytes.Buffer
	logger := platform.NewLogger(&buf, cfg.LogLevel)

	err := bootFingerprintAndSeed(context.Background(), supervisor.New(), cfg, layout, nil, platform.DefaultTimeouts(), logger)
	if err == nil {
		t.Fatal("bootFingerprintAndSeed() error = nil, want a fatal error for the rejected (0777) git-dir root")
	}

	if _, statErr := os.Stat(filepath.Join(victim, "HEAD")); statErr == nil {
		t.Error("victim/HEAD exists -- SyncHeadIn wrote through the symlinked, rejected root; the fix must skip repo discovery entirely when EnsureRoot fails")
	}

	shas := loggedRepoSHAs(t, &buf)
	if len(shas) != 0 {
		t.Errorf("fingerprint repo_shas = %v, want empty -- a rejected git-dir root must not be discovered against at all", shas)
	}
}

// TestBootFingerprintAndSeed_SecondaryFailure_ExcludedFromRepoSHAs proves
// the second half of the R3 finding: a secondary repo whose Seed call
// fails THIS boot must be excluded from the fingerprint's own repo_shas,
// even if its agent git-dir already exists on disk (seeded successfully on
// an earlier boot) -- DiscoverRepoSHAs' own os.Stat gate has no notion of
// "seeded this boot", only "a git-dir is present", so without the fix a
// stale SHA from the earlier, successful seed would still be reported as
// if it were current.
func TestBootFingerprintAndSeed_SecondaryFailure_ExcludedFromRepoSHAs(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	seedWarmBootTestGoodRepo(t, workspaceDir, "primary")
	seedWarmBootTestGoodRepo(t, workspaceDir, "secondary")

	gitDirRoot := t.TempDir()
	layout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}

	// Simulate an earlier, successful boot that already seeded "secondary"'s
	// agent git-dir -- BEFORE this boot's own shallow marker (below) makes
	// gitdir.Seed refuse it. This leaves a real, stale git-dir with a real
	// SHA on disk at layout.Repo("secondary").GitDir.
	timeouts := platform.DefaultTimeouts()
	if err := gitdir.Seed(context.Background(), supervisor.New(), layout.Repo("secondary"), "https://example.invalid/secondary.git", nil, timeouts.GitSyncStepTimeout, timeouts.ProcessStopGracePeriod); err != nil {
		t.Fatalf("precondition: gitdir.Seed(secondary) error = %v, want nil (must succeed once, to leave a stale git-dir behind)", err)
	}

	// NOW plant the .git/shallow marker that makes THIS boot's Seed call for
	// "secondary" fail -- exactly seedWarmBootTestBadRepo's own shape,
	// applied after the fact so the earlier Seed call above ran clean.
	if err := os.WriteFile(filepath.Join(workspaceDir, "secondary", ".git", "shallow"), []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatalf("write .git/shallow: %v", err)
	}

	cfg := boot.Config{
		GitDirRoot:   gitDirRoot,
		WorkspaceDir: workspaceDir,
		SessionConfig: &sessionconfig.SessionConfig{
			Repos: []sessionconfig.SessionConfigReposElem{
				{Name: "primary", Url: "https://example.invalid/primary.git"},
				{Name: "secondary", Url: "https://example.invalid/secondary.git"},
			},
		},
	}

	var buf bytes.Buffer
	logger := platform.NewLogger(&buf, cfg.LogLevel)

	err := bootFingerprintAndSeed(context.Background(), supervisor.New(), cfg, layout, nil, timeouts, logger)
	if err != nil {
		t.Fatalf("bootFingerprintAndSeed() error = %v, want nil (a secondary repo's Seed failure is a warning, not fatal)", err)
	}

	shas := loggedRepoSHAs(t, &buf)
	if _, present := shas["primary"]; !present {
		t.Errorf("fingerprint repo_shas = %v, want a \"primary\" entry (its Seed call succeeded this boot)", shas)
	}
	if sha, present := shas["secondary"]; present {
		t.Errorf("fingerprint repo_shas = %v, want NO \"secondary\" entry (its Seed call failed THIS boot; got stale sha %q from an earlier boot)", shas, sha)
	}
}
