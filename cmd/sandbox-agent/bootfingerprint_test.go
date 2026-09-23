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
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// bootFingerprintTestBackdate is a fixed, arbitrary timestamp well before
// any test in this file runs -- used to backdate an agent git-dir's own
// HEAD file so a later, unwanted write (SyncHeadIn, reached only via a
// `git rev-parse` spawn through gitdir.Run) is directly observable as an
// mtime that moved to "now", rather than staying pinned at this value.
var bootFingerprintTestBackdate = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// backdateAgentHead sets repoGitDir/HEAD's own mtime (and atime) to
// bootFingerprintTestBackdate -- repoGitDir must already have a real HEAD
// file (i.e. the repo was already Seed'd once) -- so a later assertion
// can prove that file was never rewritten.
func backdateAgentHead(t *testing.T, repoGitDir string) {
	t.Helper()
	head := filepath.Join(repoGitDir, "HEAD")
	if err := os.Chtimes(head, bootFingerprintTestBackdate, bootFingerprintTestBackdate); err != nil {
		t.Fatalf("Chtimes(%s): %v", head, err)
	}
}

// assertAgentHeadUntouched fails t unless repoGitDir/HEAD's mtime is still
// exactly bootFingerprintTestBackdate -- i.e. nothing wrote through it
// since backdateAgentHead was called, which (since SyncHeadIn always runs
// immediately before any git spawn through gitdir.Run, see head.go's own
// doc comment) is direct proof no git command was ever spawned against
// this agent git-dir.
func assertAgentHeadUntouched(t *testing.T, label, repoGitDir string) {
	t.Helper()
	head := filepath.Join(repoGitDir, "HEAD")
	info, err := os.Stat(head)
	if err != nil {
		t.Fatalf("stat %s HEAD (%s): %v", label, head, err)
	}
	if !info.ModTime().Equal(bootFingerprintTestBackdate) {
		t.Errorf("%s agent git-dir HEAD mtime = %v, want unchanged %v -- a write here means git was spawned against a repo not seeded this boot", label, info.ModTime(), bootFingerprintTestBackdate)
	}
}

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

// assertAgentGitDirAbsent fails t unless repoGitDir does not exist at all --
// the round-5 fix's own guarantee (internal/sandboxagent/gitdir/seed.go:
// Seed's os.RemoveAll(repo.GitDir) is now its very first action, before any
// guard that could refuse the call), and a strictly stronger proof than
// assertAgentHeadUntouched above: not merely "nothing wrote through the
// stale git-dir", but "the stale git-dir itself is gone", which is what
// makes DiscoverRepoSHAs' plain os.Stat(repo.GitDir) gate (boot/
// fingerprint.go) safe for a LATER, allowed=nil call to rely on by itself.
func assertAgentGitDirAbsent(t *testing.T, label, repoGitDir string) {
	t.Helper()
	if _, err := os.Stat(repoGitDir); !os.IsNotExist(err) {
		t.Errorf("%s agent git-dir (%s) stat error = %v, want os.IsNotExist -- a refused Seed call must remove the agent git-dir entirely", label, repoGitDir, err)
	}
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

	// Not merely absent from repo_shas -- the refused Seed call above must
	// have removed secondary's stale agent git-dir entirely (round-5 fix),
	// which is what proves no git could have been spawned against it, here
	// or by any later call this boot.
	assertAgentGitDirAbsent(t, "secondary", layout.Repo("secondary").GitDir)
}

// TestBootFingerprintAndSeed_PrimaryFailureWarmBoot_NoDiscoveryNoGitSpawned
// reproduces the round-4 finding directly: a WARM boot (repo_image/
// snapshot_restore) where BOTH "primary" and "secondary" were already
// seeded successfully on an EARLIER boot, so real, stale agent git-dirs
// for both sit on disk before this boot's own seedWarmBootRepos loop ever
// runs. This boot then plants primary/.git/shallow, so gitdir.Seed refuses
// the PRIMARY (its own unsupported-layout guard) -- but not before removing
// primary's stale agent git-dir entirely first (round-5 fix: Seed's
// os.RemoveAll(repo.GitDir) is now its very first action, ahead of every
// guard). seedWarmBootRepos returns immediately, with an empty "seeded this
// boot" set: no warning for the primary, and "secondary" never even
// attempted.
//
// Before the round-4 fix, the exclusion in bootFingerprintAndSeed only
// matched warnings by message, and a failed PRIMARY produces no warning at
// all (nor does a secondary that was never reached) -- so
// boot.CollectFingerprint ran anyway, unrestricted, and found both stale
// git-dirs still on disk: DiscoverRepoSHAs' os.Stat gate has no notion of
// "seeded this boot", so it happily ran SyncHeadIn (a write) and spawned
// `git rev-parse HEAD` against BOTH, including the very primary whose
// layout gitdir.Seed had just refused to vouch for. This proves neither
// happens now: the fatal error still propagates, repo_shas is completely
// empty, primary's agent git-dir is gone entirely (removed by its own
// refused Seed call, round-5), and secondary's agent git-dir's own HEAD
// file was never written to (it was never even attempted this boot) --
// i.e. no git was spawned against either.
func TestBootFingerprintAndSeed_PrimaryFailureWarmBoot_NoDiscoveryNoGitSpawned(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	seedWarmBootTestGoodRepo(t, workspaceDir, "primary")
	seedWarmBootTestGoodRepo(t, workspaceDir, "secondary")

	gitDirRoot := t.TempDir()
	layout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}

	// Simulate an EARLIER, successful boot that already seeded BOTH
	// repos' agent git-dirs -- BEFORE this boot's own shallow marker
	// (below) makes gitdir.Seed refuse the primary. This leaves real,
	// stale git-dirs with real SHAs on disk for both.
	timeouts := platform.DefaultTimeouts()
	if err := gitdir.Seed(context.Background(), supervisor.New(), layout.Repo("primary"), "https://example.invalid/primary.git", nil, timeouts.GitSyncStepTimeout, timeouts.ProcessStopGracePeriod); err != nil {
		t.Fatalf("precondition: gitdir.Seed(primary) error = %v, want nil (must succeed once, to leave a stale git-dir behind)", err)
	}
	if err := gitdir.Seed(context.Background(), supervisor.New(), layout.Repo("secondary"), "https://example.invalid/secondary.git", nil, timeouts.GitSyncStepTimeout, timeouts.ProcessStopGracePeriod); err != nil {
		t.Fatalf("precondition: gitdir.Seed(secondary) error = %v, want nil (must succeed once, to leave a stale git-dir behind)", err)
	}

	// Backdate secondary's agent HEAD -- secondary is never even attempted
	// this boot (the primary's failure is fatal, so the loop returns before
	// reaching it), so its HEAD's mtime staying pinned here is still direct
	// proof no git ever ran against it. Primary's own agent git-dir is
	// checked differently below (assertAgentGitDirAbsent): the round-5 fix
	// removes it ENTIRELY as part of refusing primary's own Seed call, so
	// backdating its HEAD first would only be overwritten by that removal.
	backdateAgentHead(t, layout.Repo("secondary").GitDir)

	// NOW plant the .git/shallow marker that makes THIS boot's Seed call
	// for "primary" fail -- exactly seedWarmBootTestBadRepo's own shape,
	// applied after the fact so the earlier Seed calls above ran clean.
	if err := os.WriteFile(filepath.Join(workspaceDir, "primary", ".git", "shallow"), []byte("deadbeef\n"), 0o644); err != nil {
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
	if err == nil {
		t.Fatal("bootFingerprintAndSeed() error = nil, want a fatal error for the failed primary repo's Seed call (warm boot)")
	}

	shas := loggedRepoSHAs(t, &buf)
	if len(shas) != 0 {
		t.Errorf("fingerprint repo_shas = %v, want EMPTY -- neither repo was (re-)seeded THIS boot (primary's Seed call failed; secondary was never attempted)", shas)
	}

	assertAgentGitDirAbsent(t, "primary", layout.Repo("primary").GitDir)
	assertAgentHeadUntouched(t, "secondary", layout.Repo("secondary").GitDir)
}

// TestBootFingerprintAndSeed_SecondaryFailure_LaterNilAllowedCallAlsoExcludesIt
// is round 5's own finding, reproduced directly. bootFingerprintAndSeed's
// own call to boot.CollectFingerprint restricts discovery to seededRepos,
// the positive allowlist seedWarmBootRepos returns -- but that allowlist
// only covers THIS one call. Two LATER calls in the same boot (run()'s own
// post-opencode-spawn call, main.go ~1677, and runBootSequence's own
// post-clone call, main.go ~2617) both pass allowed=nil, relying entirely
// on DiscoverRepoSHAs' plain os.Stat(repo.GitDir) gate.
//
// Before the round-5 fix, a refused Seed call left the PREVIOUS boot's
// stale agent git-dir sitting on disk untouched -- gitdir.Seed only ever
// removed and rebuilt repo.GitDir as part of ITS OWN successful run, AFTER
// every guard, so a refusal (e.g. this boot's own `.git/shallow` marker)
// short-circuited before ever reaching that step. A later, allowed=nil
// call's os.Stat gate then found that stale git-dir still present and let
// DiscoverRepoSHAs spawn `git rev-parse HEAD` against a SHA THIS boot never
// validated. The fix (seed.go) makes os.RemoveAll(repo.GitDir) Seed's very
// first action, before any guard -- so a refused Seed always leaves the
// agent git-dir absent, and this test proves a follow-up call using the
// EXACT allowed=nil shape of those two other call sites still excludes the
// failed secondary, with no allowlist needed at that call site at all.
func TestBootFingerprintAndSeed_SecondaryFailure_LaterNilAllowedCallAlsoExcludesIt(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	seedWarmBootTestGoodRepo(t, workspaceDir, "primary")
	seedWarmBootTestGoodRepo(t, workspaceDir, "secondary")

	gitDirRoot := t.TempDir()
	layout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}

	timeouts := platform.DefaultTimeouts()

	// Simulate an earlier, successful boot that already seeded "secondary"'s
	// agent git-dir -- BEFORE this boot's own shallow marker (below) makes
	// gitdir.Seed refuse it. This leaves a real, stale git-dir with a real
	// SHA on disk at layout.Repo("secondary").GitDir.
	if err := gitdir.Seed(context.Background(), supervisor.New(), layout.Repo("secondary"), "https://example.invalid/secondary.git", nil, timeouts.GitSyncStepTimeout, timeouts.ProcessStopGracePeriod); err != nil {
		t.Fatalf("precondition: gitdir.Seed(secondary) error = %v, want nil (must succeed once, to leave a stale git-dir behind)", err)
	}
	if _, statErr := os.Stat(layout.Repo("secondary").GitDir); statErr != nil {
		t.Fatalf("precondition: secondary agent git-dir missing after seeding: %v", statErr)
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

	sup := supervisor.New()
	if err := bootFingerprintAndSeed(context.Background(), sup, cfg, layout, nil, timeouts, logger); err != nil {
		t.Fatalf("bootFingerprintAndSeed() error = %v, want nil (a secondary repo's Seed failure is a warning, not fatal)", err)
	}

	// The refused Seed call above must have removed secondary's agent
	// git-dir entirely -- the round-5 fix's own guarantee.
	assertAgentGitDirAbsent(t, "secondary", layout.Repo("secondary").GitDir)

	// The follow-up call: the EXACT allowed=nil shape of run()'s own
	// post-opencode-spawn boot.CollectFingerprint call and
	// runBootSequence's own post-clone call -- neither threads seededRepos
	// (or any allowlist) through.
	followUp := boot.CollectFingerprint(context.Background(), sup, cfg, layout, nil, nil, timeouts.RepoSHADiscoveryTimeout, timeouts.ProcessStopGracePeriod, "")
	if sha, present := followUp.RepoSHAs["secondary"]; present {
		t.Errorf("follow-up CollectFingerprint(allowed=nil) repo_shas secondary = %q, want absent -- its agent git-dir should have been removed by the refused Seed call, so DiscoverRepoSHAs' own os.Stat gate should have skipped it with no allowlist at all", sha)
	}
	if _, present := followUp.RepoSHAs["primary"]; !present {
		t.Errorf("follow-up CollectFingerprint(allowed=nil) repo_shas = %v, want a \"primary\" entry (its Seed call succeeded this boot, so its agent git-dir is real and present)", followUp.RepoSHAs)
	}
}
