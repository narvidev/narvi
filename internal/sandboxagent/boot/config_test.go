package boot_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
)

// validSessionConfigJSON is a well-formed SESSION_CONFIG document (every
// required field per contracts/session-config/v1/session-config.schema.json
// present) whose bootMode is "fresh" -- used by every NARVI_SESSION_CONFIG
// test below that needs a valid document, mutating just bootMode where a
// mismatch is wanted.
const validSessionConfigJSON = `{
	"bootMode": "fresh",
	"controlPlaneWsUrl": "wss://cp.example.com/sessions/sess-1/ws?type=sandbox",
	"correlationId": null,
	"gen": 1,
	"repos": [{"name": "repo1", "url": "https://example.com/repo1.git", "branch": null}],
	"sandboxId": "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a",
	"sandboxToken": "tok-123",
	"sessionId": "sess-1"
}`

// These tests use t.Setenv, which the testing package forbids combining
// with t.Parallel() (env vars are process-global) -- so none of them call
// t.Parallel.

func TestLoad_MissingBootMode(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "")

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error for missing NARVI_BOOT_MODE")
	}

	var invErr *sandboxboot.InvalidBootModeError
	if !errors.As(err, &invErr) {
		t.Fatalf("Load() error = %v (%T), want it to wrap *sandboxboot.InvalidBootModeError", err, err)
	}
}

func TestLoad_InvalidBootMode(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "garbage")

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error for invalid NARVI_BOOT_MODE")
	}

	var invErr *sandboxboot.InvalidBootModeError
	if !errors.As(err, &invErr) {
		t.Fatalf("Load() error = %v (%T), want it to wrap *sandboxboot.InvalidBootModeError", err, err)
	}
}

func TestLoad_DefaultsWhenOptionalVarsAbsent(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_AGENT_VERSION", "")
	t.Setenv("NARVI_IMAGE_DIGEST", "")
	t.Setenv("NARVI_WORKSPACE_DIR", "")
	t.Setenv("NARVI_LOG_LEVEL", "")

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.BootMode != sandboxboot.BootModeFresh {
		t.Errorf("BootMode = %q, want %q", cfg.BootMode, sandboxboot.BootModeFresh)
	}
	if cfg.AgentVersion != "dev" {
		t.Errorf("AgentVersion = %q, want %q", cfg.AgentVersion, "dev")
	}
	if cfg.ImageDigest != "unknown" {
		t.Errorf("ImageDigest = %q, want %q", cfg.ImageDigest, "unknown")
	}
	if cfg.WorkspaceDir != "/workspace" {
		t.Errorf("WorkspaceDir = %q, want %q", cfg.WorkspaceDir, "/workspace")
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
}

func TestLoad_AllVarsPresent(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "repo_image")
	t.Setenv("NARVI_AGENT_VERSION", "1.2.3")
	t.Setenv("NARVI_IMAGE_DIGEST", "sha256:deadbeef")
	t.Setenv("NARVI_WORKSPACE_DIR", "/custom/workspace")
	t.Setenv("NARVI_LOG_LEVEL", "debug")

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.BootMode != sandboxboot.BootModeRepoImage {
		t.Errorf("BootMode = %q, want %q", cfg.BootMode, sandboxboot.BootModeRepoImage)
	}
	if cfg.AgentVersion != "1.2.3" {
		t.Errorf("AgentVersion = %q, want %q", cfg.AgentVersion, "1.2.3")
	}
	if cfg.ImageDigest != "sha256:deadbeef" {
		t.Errorf("ImageDigest = %q, want %q", cfg.ImageDigest, "sha256:deadbeef")
	}
	if cfg.WorkspaceDir != "/custom/workspace" {
		t.Errorf("WorkspaceDir = %q, want %q", cfg.WorkspaceDir, "/custom/workspace")
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelDebug)
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_LOG_LEVEL", "trace")

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error for invalid NARVI_LOG_LEVEL")
	}

	var invErr *boot.InvalidLogLevelError
	if !errors.As(err, &invErr) {
		t.Fatalf("Load() error = %v (%T), want *boot.InvalidLogLevelError", err, err)
	}
	if invErr.Error() == "" {
		t.Errorf("InvalidLogLevelError.Error() = %q, want a non-empty message", invErr.Error())
	}
}

func TestLoad_CredentialCacheDirDefault(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_CREDENTIAL_CACHE_DIR", "")

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.CredentialCacheDir != "/tmp/narvi-credentials" {
		t.Errorf("CredentialCacheDir = %q, want %q", cfg.CredentialCacheDir, "/tmp/narvi-credentials")
	}
}

func TestLoad_CredentialCacheDirOverride(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_CREDENTIAL_CACHE_DIR", "/custom/creds")

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.CredentialCacheDir != "/custom/creds" {
		t.Errorf("CredentialCacheDir = %q, want %q", cfg.CredentialCacheDir, "/custom/creds")
	}
}

func TestLoad_SandboxIDDefault(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_SANDBOX_ID", "")
	// No live SessionConfig either -- the dev/CI-with-no-live-session case,
	// where defaultSandboxID ("") remains a correct, valid state.
	t.Setenv("NARVI_SESSION_CONFIG", "")

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.SandboxID != "" {
		t.Errorf("SandboxID = %q, want empty string (dev/CI-with-no-live-session default)", cfg.SandboxID)
	}
}

func TestLoad_SandboxIDOverride(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_SANDBOX_ID", "sbx-abc123")
	t.Setenv("NARVI_SESSION_CONFIG", "")

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.SandboxID != "sbx-abc123" {
		t.Errorf("SandboxID = %q, want %q", cfg.SandboxID, "sbx-abc123")
	}
}

// TestLoad_SandboxIDFromSessionConfig proves the actual production bug fix:
// with NARVI_SANDBOX_ID unset and a valid NARVI_SESSION_CONFIG carrying a
// real sandboxId, Config.SandboxID now equals that value -- it is no
// longer always "" (§6.1's X-Sandbox-ID handshake header, previously
// always empty, would now carry the real, control-plane-assigned sandbox
// identity).
func TestLoad_SandboxIDFromSessionConfig(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_SANDBOX_ID", "")
	t.Setenv("NARVI_SESSION_CONFIG", validSessionConfigJSON) // sandboxId: "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	want := "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"
	if cfg.SandboxID != want {
		t.Errorf("SandboxID = %q, want %q (from SessionConfig.SandboxId)", cfg.SandboxID, want)
	}
}

// TestLoad_SandboxIDEnvOverrideWinsOverSessionConfig proves NARVI_SANDBOX_ID,
// when explicitly set, still wins over whatever a present SessionConfig
// carries (a deliberate dev/test override -- see Config.SandboxID's own
// doc comment) when the two do not genuinely disagree (SessionConfig's own
// sandboxId here matches the override exactly) -- no error.
func TestLoad_SandboxIDEnvOverrideWinsOverSessionConfig(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_SANDBOX_ID", "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a")
	t.Setenv("NARVI_SESSION_CONFIG", validSessionConfigJSON) // sandboxId: "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.SandboxID != "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a" {
		t.Errorf("SandboxID = %q, want %q", cfg.SandboxID, "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a")
	}
}

// TestLoad_SandboxIDMismatch proves a genuine disagreement between an
// explicitly set NARVI_SANDBOX_ID and a present SessionConfig's own
// non-empty sandboxId is a fail-fast *boot.SandboxIDMismatchError -- the
// same reconciliation shape as TestLoad_SessionConfigBootModeMismatch's own
// *boot.ModeMismatchError, never a silent preference of one value.
func TestLoad_SandboxIDMismatch(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_SANDBOX_ID", "sbx-env-value")
	t.Setenv("NARVI_SESSION_CONFIG", validSessionConfigJSON) // sandboxId: "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want *boot.SandboxIDMismatchError")
	}

	var mismatchErr *boot.SandboxIDMismatchError
	if !errors.As(err, &mismatchErr) {
		t.Fatalf("Load() error = %v (%T), want *boot.SandboxIDMismatchError", err, err)
	}
	if mismatchErr.EnvValue != "sbx-env-value" {
		t.Errorf("EnvValue = %q, want %q", mismatchErr.EnvValue, "sbx-env-value")
	}
	if mismatchErr.SessionConfigValue != "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a" {
		t.Errorf("SessionConfigValue = %q, want %q", mismatchErr.SessionConfigValue, "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a")
	}
	if mismatchErr.Error() == "" {
		t.Errorf("SandboxIDMismatchError.Error() = %q, want a non-empty message", mismatchErr.Error())
	}
}

// TestLoad_SessionConfigAbsent proves NARVI_SESSION_CONFIG's absence
// remains a fully valid, correct state: SessionConfig is nil and every
// other field behaves exactly as it did before this Step (§6.4/§14.2's
// own tests, unmodified, already cover that).
func TestLoad_SessionConfigAbsent(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_SESSION_CONFIG", "")

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.SessionConfig != nil {
		t.Errorf("SessionConfig = %+v, want nil", cfg.SessionConfig)
	}
}

// TestLoad_SessionConfigPresentAndValid proves a well-formed
// NARVI_SESSION_CONFIG document whose bootMode agrees with NARVI_BOOT_MODE
// parses into a populated Config.SessionConfig.
func TestLoad_SessionConfigPresentAndValid(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_SESSION_CONFIG", validSessionConfigJSON)

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.SessionConfig == nil {
		t.Fatal("SessionConfig = nil, want a populated *SessionConfig")
	}
	if cfg.SessionConfig.SessionId != "sess-1" {
		t.Errorf("SessionConfig.SessionId = %q, want %q", cfg.SessionConfig.SessionId, "sess-1")
	}
	if len(cfg.SessionConfig.Repos) != 1 || cfg.SessionConfig.Repos[0].Name != "repo1" {
		t.Errorf("SessionConfig.Repos = %+v, want one repo named %q", cfg.SessionConfig.Repos, "repo1")
	}
}

// TestLoad_SessionConfigMalformedJSON proves a malformed NARVI_SESSION_CONFIG
// document is a real, propagated error (fail-fast, matching every other
// Load() failure mode) -- not silently ignored.
func TestLoad_SessionConfigMalformedJSON(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_SESSION_CONFIG", "{not valid json")

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error for malformed NARVI_SESSION_CONFIG")
	}
}

// TestLoad_SessionConfigMissingRequiredField proves a SESSION_CONFIG
// document missing a required field surfaces the generated UnmarshalJSON's
// own error, unwrapped and un-rehashed -- Load() must not swallow or
// re-wrap it into something less informative.
func TestLoad_SessionConfigMissingRequiredField(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	// sessionId is omitted entirely.
	t.Setenv("NARVI_SESSION_CONFIG", `{
		"bootMode": "fresh",
		"controlPlaneWsUrl": "wss://cp.example.com/sessions/sess-1/ws?type=sandbox",
		"correlationId": null,
		"gen": 1,
		"repos": [{"name": "repo1", "url": "https://example.com/repo1.git", "branch": null}],
		"sandboxId": "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a",
		"sandboxToken": "tok-123"
	}`)

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error for a SESSION_CONFIG document missing sessionId")
	}
	if !strings.Contains(err.Error(), "sessionId") {
		t.Errorf("Load() error = %v, want it to name the missing field %q", err, "sessionId")
	}
}

// TestLoad_SessionConfigBootModeMismatch proves a valid SESSION_CONFIG
// document whose bootMode disagrees with the separately-read
// NARVI_BOOT_MODE is a fail-fast *ModeMismatchError.
func TestLoad_SessionConfigBootModeMismatch(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "repo_image")
	t.Setenv("NARVI_SESSION_CONFIG", validSessionConfigJSON) // bootMode: "fresh"

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want *boot.ModeMismatchError")
	}

	var mismatchErr *boot.ModeMismatchError
	if !errors.As(err, &mismatchErr) {
		t.Fatalf("Load() error = %v (%T), want *boot.ModeMismatchError", err, err)
	}
	if mismatchErr.EnvValue != "repo_image" {
		t.Errorf("EnvValue = %q, want %q", mismatchErr.EnvValue, "repo_image")
	}
	if mismatchErr.SessionConfigValue != "fresh" {
		t.Errorf("SessionConfigValue = %q, want %q", mismatchErr.SessionConfigValue, "fresh")
	}
	if mismatchErr.Error() == "" {
		t.Errorf("ModeMismatchError.Error() = %q, want a non-empty message", mismatchErr.Error())
	}
}

func TestLoad_RuntimeUIDGIDDefaults(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_RUNTIME_UID", "")
	t.Setenv("NARVI_RUNTIME_GID", "")

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.RuntimeUID != 65534 {
		t.Errorf("RuntimeUID = %d, want %d", cfg.RuntimeUID, 65534)
	}
	if cfg.RuntimeGID != 65534 {
		t.Errorf("RuntimeGID = %d, want %d", cfg.RuntimeGID, 65534)
	}
}

func TestLoad_RuntimeUIDGIDOverride(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_RUNTIME_UID", "10001")
	t.Setenv("NARVI_RUNTIME_GID", "10002")

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.RuntimeUID != 10001 {
		t.Errorf("RuntimeUID = %d, want %d", cfg.RuntimeUID, 10001)
	}
	if cfg.RuntimeGID != 10002 {
		t.Errorf("RuntimeGID = %d, want %d", cfg.RuntimeGID, 10002)
	}
}

func TestLoad_InvalidRuntimeUID(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_RUNTIME_UID", "not-a-number")

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error for invalid NARVI_RUNTIME_UID")
	}
	var invErr *boot.InvalidRuntimeUIDError
	if !errors.As(err, &invErr) {
		t.Fatalf("Load() error = %v (%T), want *boot.InvalidRuntimeUIDError", err, err)
	}
	if invErr.Error() == "" {
		t.Errorf("InvalidRuntimeUIDError.Error() = %q, want a non-empty message", invErr.Error())
	}
}

func TestLoad_InvalidRuntimeGID(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_RUNTIME_GID", "not-a-number")

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error for invalid NARVI_RUNTIME_GID")
	}
	var invErr *boot.InvalidRuntimeGIDError
	if !errors.As(err, &invErr) {
		t.Fatalf("Load() error = %v (%T), want *boot.InvalidRuntimeGIDError", err, err)
	}
	if invErr.Error() == "" {
		t.Errorf("InvalidRuntimeGIDError.Error() = %q, want a non-empty message", invErr.Error())
	}
}

// TestLoad_RuntimeUIDZeroRefused proves NARVI_RUNTIME_UID=0 (root) is a
// fail-fast boot error, not a silently-accepted value that would make
// cmd/sandbox-agent/main.go build a Credential naming root -- dropping no
// privilege at all and silently defeating §30.5's entire purpose.
func TestLoad_RuntimeUIDZeroRefused(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_RUNTIME_UID", "0")

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want *boot.RuntimeUIDIsRootError for NARVI_RUNTIME_UID=0")
	}
	var rootErr *boot.RuntimeUIDIsRootError
	if !errors.As(err, &rootErr) {
		t.Fatalf("Load() error = %v (%T), want *boot.RuntimeUIDIsRootError", err, err)
	}
}

// TestLoad_RuntimeGIDZeroRefused is TestLoad_RuntimeUIDZeroRefused's own
// gid counterpart.
func TestLoad_RuntimeGIDZeroRefused(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_RUNTIME_GID", "0")

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want *boot.RuntimeGIDIsRootError for NARVI_RUNTIME_GID=0")
	}
	var rootErr *boot.RuntimeGIDIsRootError
	if !errors.As(err, &rootErr) {
		t.Fatalf("Load() error = %v (%T), want *boot.RuntimeGIDIsRootError", err, err)
	}
}

// TestLoad_GitDirRootDefault proves NARVI_GIT_DIR_ROOT unset resolves to
// the documented default -- deliberately outside the default
// NARVI_WORKSPACE_DIR ("/workspace"), matching Config.GitDirRoot's own
// doc comment.
func TestLoad_GitDirRootDefault(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_GIT_DIR_ROOT", "")

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.GitDirRoot != "/var/lib/narvi/gitdirs" {
		t.Errorf("GitDirRoot = %q, want %q", cfg.GitDirRoot, "/var/lib/narvi/gitdirs")
	}
}

// TestLoad_GitDirRootOverride proves a valid, explicit NARVI_GIT_DIR_ROOT
// (absolute, outside WorkspaceDir) is accepted verbatim.
func TestLoad_GitDirRootOverride(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_WORKSPACE_DIR", "/workspace")
	t.Setenv("NARVI_GIT_DIR_ROOT", "/custom/gitdirs")

	cfg, err := boot.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.GitDirRoot != "/custom/gitdirs" {
		t.Errorf("GitDirRoot = %q, want %q", cfg.GitDirRoot, "/custom/gitdirs")
	}
}

// TestLoad_GitDirRootRejectsRelativePath proves a non-absolute
// NARVI_GIT_DIR_ROOT is refused fail-fast, never silently resolved
// relative to some working directory.
func TestLoad_GitDirRootRejectsRelativePath(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_GIT_DIR_ROOT", "relative/gitdirs")

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want *boot.InvalidGitDirRootError for a relative NARVI_GIT_DIR_ROOT")
	}
	var invalidErr *boot.InvalidGitDirRootError
	if !errors.As(err, &invalidErr) {
		t.Fatalf("Load() error = %v (%T), want *boot.InvalidGitDirRootError", err, err)
	}
}

// TestLoad_GitDirRootRejectsNestedUnderWorkspace proves the load-bearing
// guard: an agent-owned git-dir root that is WorkspaceDir itself, or
// nested under it, would put it inside the runtime-owned tree the whole
// split-git-dir design exists to keep it out of.
func TestLoad_GitDirRootRejectsNestedUnderWorkspace(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_WORKSPACE_DIR", "/workspace")
	t.Setenv("NARVI_GIT_DIR_ROOT", "/workspace/gitdirs")

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want *boot.InvalidGitDirRootError for a NARVI_GIT_DIR_ROOT nested under WorkspaceDir")
	}
	var invalidErr *boot.InvalidGitDirRootError
	if !errors.As(err, &invalidErr) {
		t.Fatalf("Load() error = %v (%T), want *boot.InvalidGitDirRootError", err, err)
	}
}

// TestLoad_GitDirRootRejectsEqualToWorkspace proves the degenerate case
// (GitDirRoot == WorkspaceDir exactly) is refused too, not just a proper
// descendant.
func TestLoad_GitDirRootRejectsEqualToWorkspace(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_WORKSPACE_DIR", "/workspace")
	t.Setenv("NARVI_GIT_DIR_ROOT", "/workspace")

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want *boot.InvalidGitDirRootError for NARVI_GIT_DIR_ROOT == WorkspaceDir")
	}
	var invalidErr *boot.InvalidGitDirRootError
	if !errors.As(err, &invalidErr) {
		t.Fatalf("Load() error = %v (%T), want *boot.InvalidGitDirRootError", err, err)
	}
}

// TestLoad_GitDirRootValidation_TableCases is validateGitDirRoot's own
// table-driven proof, covering both directions of nesting symmetrically
// (Correction, review): the original guard only ever rejected GitDirRoot
// nested under (or equal to) WorkspaceDir. It never checked the REVERSE
// -- WorkspaceDir nested under (or equal to) GitDirRoot -- which is just
// as dangerous: gitdir.Layout.Repo builds GitDir as
// filepath.Join(Root, name), and gitdir.Seed's own first destructive step
// is os.RemoveAll(repo.GitDir). An operator setting GitDirRoot to
// WorkspaceDir's own parent lets an ordinary session repo NAME (accepted
// by reposource.ValidateRepoName as a plain identifier) resolve GitDir to
// WorkspaceDir itself, so Seed's own RemoveAll deletes the entire
// workspace, not just the repo being seeded.
func TestLoad_GitDirRootValidation_TableCases(t *testing.T) {
	tests := []struct {
		name         string
		workspaceDir string
		gitDirRoot   string
		wantErr      bool
	}{
		{
			name:         "equal",
			workspaceDir: "/workspace",
			gitDirRoot:   "/workspace",
			wantErr:      true,
		},
		{
			name:         "root nested under workspace",
			workspaceDir: "/workspace",
			gitDirRoot:   "/workspace/gitdirs",
			wantErr:      true,
		},
		{
			// The finding's own exact reproduction: GitDirRoot is
			// WorkspaceDir's parent, so a session repo named "workspace"
			// would resolve GitDir to WorkspaceDir itself.
			name:         "workspace nested under root",
			workspaceDir: "/srv/narvi/workspace",
			gitDirRoot:   "/srv/narvi",
			wantErr:      true,
		},
		{
			// A trailing slash on WorkspaceDir must not dodge the "equal"
			// check -- both sides are filepath.Clean'd before comparison.
			name:         "equal, trailing slash on workspace",
			workspaceDir: "/workspace/",
			gitDirRoot:   "/workspace",
			wantErr:      true,
		},
		{
			// A "GitDirRoot/foo/.." style value must not dodge the
			// "equal" check either -- it Cleans down to exactly
			// WorkspaceDir.
			name:         "equal after .. normalization",
			workspaceDir: "/workspace",
			gitDirRoot:   "/workspace/foo/..",
			wantErr:      true,
		},
		{
			name:         "disjoint, valid",
			workspaceDir: "/workspace",
			gitDirRoot:   "/var/lib/narvi/gitdirs",
			wantErr:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NARVI_BOOT_MODE", "fresh")
			t.Setenv("NARVI_WORKSPACE_DIR", tc.workspaceDir)
			t.Setenv("NARVI_GIT_DIR_ROOT", tc.gitDirRoot)

			_, err := boot.Load()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Load() error = %v, want nil (workspaceDir=%q, gitDirRoot=%q)", err, tc.workspaceDir, tc.gitDirRoot)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() error = nil, want *boot.InvalidGitDirRootError (workspaceDir=%q, gitDirRoot=%q)", tc.workspaceDir, tc.gitDirRoot)
			}
			var invalidErr *boot.InvalidGitDirRootError
			if !errors.As(err, &invalidErr) {
				t.Fatalf("Load() error = %v (%T), want *boot.InvalidGitDirRootError", err, err)
			}
		})
	}
}

// TestLoad_WorkspaceDirRejectsRelativePath proves a non-absolute
// NARVI_WORKSPACE_DIR is refused fail-fast, mirroring
// TestLoad_GitDirRootRejectsRelativePath's own identical proof for
// NARVI_GIT_DIR_ROOT. Round-2 review (R5): before this check existed,
// GitDirRoot's own filepath.IsAbs check had no WorkspaceDir counterpart,
// so isPathUnderOrEqual's underlying filepath.Rel calls could receive one
// absolute and one relative operand -- see InvalidWorkspaceDirError's own
// doc comment for the fail-open consequence that had.
func TestLoad_WorkspaceDirRejectsRelativePath(t *testing.T) {
	t.Setenv("NARVI_BOOT_MODE", "fresh")
	t.Setenv("NARVI_WORKSPACE_DIR", "relative/workspace")

	_, err := boot.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want *boot.InvalidWorkspaceDirError for a relative NARVI_WORKSPACE_DIR")
	}
	var invalidErr *boot.InvalidWorkspaceDirError
	if !errors.As(err, &invalidErr) {
		t.Fatalf("Load() error = %v (%T), want *boot.InvalidWorkspaceDirError", err, err)
	}
}

// TestLoad_GitDirRootValidation_RelativeWorkspaceDirTableCases reproduces
// R2's own concrete bypass cases directly: before the fix, a relative
// WorkspaceDir made filepath.Rel error in BOTH directions at once, so
// isPathUnderOrEqual (fail-open on that error) returned false for both
// validateGitDirRoot checks, silently accepting a configuration where a
// session repo could resolve GitDir onto WorkspaceDir itself.
func TestLoad_GitDirRootValidation_RelativeWorkspaceDirTableCases(t *testing.T) {
	tests := []struct {
		name         string
		workspaceDir string
		gitDirRoot   string
	}{
		{
			// The finding's own first repro: GitDirRoot is WorkspaceDir's
			// (relative) parent -- a session repo named "workspace" would
			// resolve GitDir onto WorkspaceDir itself.
			name:         "relative workspace, root is its parent",
			workspaceDir: "srv/narvi/workspace",
			gitDirRoot:   "/srv/narvi",
		},
		{
			// The finding's own second repro: GitDirRoot nested under a
			// relative WorkspaceDir.
			name:         "relative workspace, root nested under it",
			workspaceDir: "srv/narvi/workspace",
			gitDirRoot:   "/srv/narvi/workspace/x",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NARVI_BOOT_MODE", "fresh")
			t.Setenv("NARVI_WORKSPACE_DIR", tc.workspaceDir)
			t.Setenv("NARVI_GIT_DIR_ROOT", tc.gitDirRoot)

			_, err := boot.Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want *boot.InvalidWorkspaceDirError (workspaceDir=%q, gitDirRoot=%q) -- a relative WorkspaceDir must be rejected before it can make the root/workspace overlap check fail open", tc.workspaceDir, tc.gitDirRoot)
			}
			var invalidErr *boot.InvalidWorkspaceDirError
			if !errors.As(err, &invalidErr) {
				t.Fatalf("Load() error = %v (%T), want *boot.InvalidWorkspaceDirError", err, err)
			}
		})
	}
}
