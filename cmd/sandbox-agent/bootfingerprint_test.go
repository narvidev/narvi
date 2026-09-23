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
	t.Parallel()

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

	var buf bytes.Buffer
	logger := platform.NewLogger(&buf, cfg.LogLevel)

	err := bootFingerprintAndSeed(context.Background(), supervisor.New(), cfg, layout, nil, platform.DefaultTimeouts(), logger)
	if err == nil {
		t.Fatal("bootFingerprintAndSeed() error = nil, want a fatal error for the failed primary repo's Seed call")
	}

	msgs := loggedMessages(t, &buf)
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
	t.Parallel()

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

	var buf bytes.Buffer
	logger := platform.NewLogger(&buf, cfg.LogLevel)

	err := bootFingerprintAndSeed(context.Background(), supervisor.New(), cfg, layout, nil, platform.DefaultTimeouts(), logger)
	if err == nil {
		t.Fatal("bootFingerprintAndSeed() error = nil, want a fatal error for the failed gitdir.EnsureRoot call")
	}

	msgs := loggedMessages(t, &buf)
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
	t.Parallel()

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

	var buf bytes.Buffer
	logger := platform.NewLogger(&buf, cfg.LogLevel)

	err := bootFingerprintAndSeed(context.Background(), supervisor.New(), cfg, layout, nil, platform.DefaultTimeouts(), logger)
	if err != nil {
		t.Fatalf("bootFingerprintAndSeed() error = %v, want nil (a secondary repo's Seed failure is a warning, not fatal)", err)
	}

	msgs := loggedMessages(t, &buf)
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
