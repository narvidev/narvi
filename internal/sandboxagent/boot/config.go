package boot

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/sandboxboot"
)

// The environment variables Load reads. bootModeEnvVar is the only one
// §6.4 explicitly pins a delivery mechanism for today ("Delivered to the
// sandbox as the NARVI_BOOT_MODE env var" -- see
// contracts/gen/go/sessionconfig's own doc comment on SessionConfig.
// BootMode); the rest are this Step's own invented plumbing, since no
// other SESSION_CONFIG delivery mechanism is pinned yet.
//
// SessionConfigEnvVar (NARVI_SESSION_CONFIG) is §6.4's own answer to
// that gap: an OPTIONAL env var carrying the full SESSION_CONFIG document
// as JSON. Its absence remains a fully valid, correct state (see Config.
// SessionConfig's own doc comment) -- dev/CI environments have no live
// session and are not required to set it. Exported (this Step's own env-
// leak remediation) so it is the single source of truth for the literal
// "NARVI_SESSION_CONFIG" string everywhere a caller outside this package
// needs to name it (e.g. as an argument to
// internal/sandboxagent/supervisor.EnvWithout, to keep a spawned child
// from inheriting the sandbox's own plaintext bearer token) instead of
// duplicating the literal.
const (
	bootModeEnvVar           = "NARVI_BOOT_MODE"
	agentVersionEnvVar       = "NARVI_AGENT_VERSION"
	imageDigestEnvVar        = "NARVI_IMAGE_DIGEST"
	workspaceDirEnvVar       = "NARVI_WORKSPACE_DIR"
	logLevelEnvVar           = "NARVI_LOG_LEVEL"
	SessionConfigEnvVar      = "NARVI_SESSION_CONFIG"
	credentialCacheDirEnvVar = "NARVI_CREDENTIAL_CACHE_DIR"
	sandboxIDEnvVar          = "NARVI_SANDBOX_ID"

	// runtimeUIDEnvVar/runtimeGIDEnvVar (TECHNICAL_PLAN.md §30.5, "OS-level
	// isolation between sandbox-agent and the agent runtime") name the
	// uid/gid cmd/sandbox-agent/main.go drops the agent runtime
	// (opencodeproc.Spawn's `opencode serve`) to, via a
	// *syscall.Credential built from Config.RuntimeUID/RuntimeGID below.
	// This package deliberately stops at plain uint32s -- no `syscall`
	// import here -- leaving the *syscall.Credential construction to the
	// one call site that actually needs it (see Config.RuntimeUID's own
	// doc comment).
	runtimeUIDEnvVar = "NARVI_RUNTIME_UID"
	runtimeGIDEnvVar = "NARVI_RUNTIME_GID"

	// gitDirRootEnvVar (§30.5) names the sandbox-wide root
	// internal/sandboxagent/gitdir seeds every repo's own AGENT-OWNED
	// git-dir under (gitdir.Layout.Root) -- deliberately OUTSIDE
	// WorkspaceDir (the agent-visible /workspace tree the isolated runtime
	// owns after §30.5's own chown): a git-dir living inside the runtime's
	// own tree would defeat the entire point of splitting it out in the
	// first place. See gitdir's own package doc comment for the full
	// shape.
	gitDirRootEnvVar = "NARVI_GIT_DIR_ROOT"
)

// Defaults for every optional env var above.
const (
	// defaultAgentVersion is used when NARVI_AGENT_VERSION is unset: no
	// build-time version-stamping pipeline exists yet. A later ops/release
	// Step is expected to inject a real value via -ldflags or this env
	// var.
	defaultAgentVersion = "dev"

	// defaultImageDigest is used when NARVI_IMAGE_DIGEST is unset. HONEST
	// GAP: ports.CreateSpec/the Modal adapter have no
	// mechanism to inject an arbitrary env var like this one into a
	// spawned sandbox, so in practice this will always default to
	// "unknown" until some later change closes that gap.
	defaultImageDigest = "unknown"

	// defaultWorkspaceDir is used when NARVI_WORKSPACE_DIR is unset (§6.4:
	// "Multi-repo ordered clones under /workspace/{name}").
	defaultWorkspaceDir = "/workspace"

	// defaultLogLevelValue is used when NARVI_LOG_LEVEL is unset.
	defaultLogLevelValue = "info"

	// defaultCredentialCacheDir is used when NARVI_CREDENTIAL_CACHE_DIR is
	// unset. Deliberately OUTSIDE defaultWorkspaceDir (the agent-visible
	// /workspace tree, §6.4) so a coding agent operating there never sees
	// the raw credential cache file (§5.2).
	defaultCredentialCacheDir = "/tmp/narvi-credentials"

	// defaultSandboxID is used when NARVI_SANDBOX_ID is unset AND no
	// SessionConfig is present at all (the dev/CI-with-no-live-session
	// case -- see Config.SandboxID's own doc comment for the real,
	// production path). This remains a correct, valid state on that path:
	// nothing production-relevant is running with no live session to begin
	// with.
	defaultSandboxID = ""

	// defaultRuntimeUID/defaultRuntimeGID are used when
	// NARVI_RUNTIME_UID/NARVI_RUNTIME_GID are unset: the traditional
	// "nobody"/"nogroup" numeric ids (present as raw kernel ids on
	// virtually every Linux system -- no /etc/passwd or /etc/group entry
	// required, since a Credential's Uid/Gid are plain kernel-level
	// numbers, not names). This is a deliberately image-agnostic default:
	// the base sandbox image's real build definition does not live in
	// this repo (TECHNICAL_PLAN.md §27.7 -- "its real build definition is
	// where these land", not here), so this package cannot assume any
	// SPECIFIC provisioned account exists in it. An operator whose image
	// already provisions a dedicated low-privilege account can override
	// either var to that account's own uid/gid instead.
	defaultRuntimeUID uint32 = 65534
	defaultRuntimeGID uint32 = 65534

	// defaultGitDirRoot is used when NARVI_GIT_DIR_ROOT is unset --
	// deliberately root-owned and NOT world-writable, matching gitdir.
	// EnsureRoot's own default and doc comment exactly (this literal is
	// this Step's own single source of truth for it; gitdir itself takes
	// the root as a plain parameter, never re-deriving a default of its
	// own).
	defaultGitDirRoot = "/var/lib/narvi/gitdirs"
)

// Config is sandbox-agent's own typed, boot-time-validated configuration,
// following the same fail-fast named-error convention as
// platform/config.go.
type Config struct {
	BootMode     sandboxboot.BootMode
	AgentVersion string
	ImageDigest  string
	WorkspaceDir string
	LogLevel     slog.Level

	// CredentialCacheDir is where the git credential helper
	// (internal/sandboxagent/credentials) caches minted credentials on
	// disk (§5.2: "caches to disk with flock"). Deliberately OUTSIDE
	// WorkspaceDir -- see defaultCredentialCacheDir's own doc comment.
	CredentialCacheDir string

	// RuntimeUID/RuntimeGID (TECHNICAL_PLAN.md §30.5) are the uid/gid the
	// agent runtime (opencodeproc.Spawn's `opencode serve`, and every
	// process it forks) runs as -- a KERNEL-ENFORCED boundary from
	// sandbox-agent's own identity, distinct from and strictly stronger
	// than CredentialCacheDir's own placement-based discipline above (see
	// that field's own doc comment: "outside WorkspaceDir" is a
	// convention a same-uid shell can trivially defeat; a different uid
	// cannot read the cache at all, regardless of where it sits).
	// Plain uint32s, never a *syscall.Credential, deliberately: this
	// package has no other reason to import "syscall", and the one call
	// site that builds a Credential from these two values
	// (cmd/sandbox-agent/main.go) is also the one place that decides
	// NoSetGroups/Groups, which are a spawn-time concern, not a boot-time
	// config concern.
	//
	// Resolved by Load from NARVI_RUNTIME_UID/NARVI_RUNTIME_GID, each
	// independently: unset (or empty) uses defaultRuntimeUID/
	// defaultRuntimeGID; set to a non-numeric value is a fail-fast
	// *InvalidRuntimeUIDError/*InvalidRuntimeGIDError; set to exactly "0"
	// (root) is ALSO a fail-fast *RuntimeUIDIsRootError/
	// *RuntimeGIDIsRootError -- a Credential naming uid/gid 0 would not
	// drop any privilege at all, silently defeating this Step's entire
	// purpose, exactly the "operator mistake becomes a loud fail-closed,
	// never a quiet regression" posture §30.4's own scope-introspection
	// requirement already establishes for a different credential.
	RuntimeUID uint32
	RuntimeGID uint32

	// GitDirRoot (§30.5) is the sandbox-wide root
	// internal/sandboxagent/gitdir seeds every repo's own agent-owned
	// git-dir under -- gitdir.Layout.Root, gitdir.EnsureRoot's own
	// argument. Resolved by Load from NARVI_GIT_DIR_ROOT: unset uses
	// defaultGitDirRoot; set to anything empty, non-absolute, or nested
	// UNDER WorkspaceDir is a fail-fast *InvalidGitDirRootError -- an
	// agent-owned git-dir living inside the runtime-owned workspace tree
	// would defeat the entire structural guarantee this Step exists to
	// provide (the runtime could then reach it via an ordinary path
	// inside its own tree).
	GitDirRoot string

	// SandboxID is the value internal/sandboxagent/wsbridge.New sends as
	// the sandbox WS connection's X-Sandbox-ID header (§6.1). Resolved by
	// Load in this priority order:
	//
	//  1. NARVI_SANDBOX_ID, if explicitly non-empty -- a deliberate dev/
	//     test override, always wins.
	//  2. Otherwise, when SessionConfig is present, SessionConfig.
	//     SandboxId -- the real, control-plane-assigned identity
	//     (sandboxes.id, populated by internal/app/sessionactor.
	//     assembleSessionConfig from the sandbox row's own already-known
	//     id, BEFORE CreateSandbox is ever called). This is the real
	//     production path: NARVI_SESSION_CONFIG is the one existing
	//     channel into the sandbox's own environment, so this is how
	//     sandbox-agent learns its own identity for the handshake without
	//     any new provider-level plumbing.
	//  3. Otherwise (no env var override, no SessionConfig at all --
	//     the dev/CI-with-no-live-session case), defaultSandboxID ("") --
	//     a correct, valid state on that path.
	//
	// If NARVI_SANDBOX_ID is explicitly set AND SessionConfig is present
	// AND SessionConfig.SandboxId is also non-empty, but the two disagree,
	// Load returns a *SandboxIDMismatchError instead of silently picking
	// one -- the same fail-fast reconciliation ModeMismatchError already
	// applies to a diverging NARVI_BOOT_MODE/SessionConfig.BootMode pair.
	SandboxID string

	// SessionConfig is the full SESSION_CONFIG document (§6.4), parsed
	// from NARVI_SESSION_CONFIG when present -- nil when that env var is
	// unset. This is intentionally NOT a forced requirement: dev/CI
	// environments have no live session, and nil here (with a nil Repos
	// slice, exactly today's boot behavior) remains a fully valid,
	// correct state. When present, its own BootMode field is
	// cross-checked against the separately-read NARVI_BOOT_MODE (see
	// ModeMismatchError) -- a real, fail-fast reconciliation, the
	// same shape as ports.CreateSpec.Validate's Gen/SessionConfig.Gen
	// check.
	SessionConfig *sessionconfig.SessionConfig
}

// ModeMismatchError is returned by Load when NARVI_SESSION_CONFIG is
// present but its own bootMode field disagrees with the separately-read
// NARVI_BOOT_MODE env var. The two are a deliberate duplicate (NARVI_
// BOOT_MODE is §6.4's own pinned delivery mechanism; SessionConfig.
// BootMode travels inside the larger, optional SESSION_CONFIG document)
// with nothing structurally keeping them in sync -- a caller-side bug
// that sets one and forgets the other must be caught before either value
// is trusted, exactly like ports.CreateSpec.Validate's GenMismatchError
// catches a diverging Gen/SessionConfig.Gen pair.
type ModeMismatchError struct {
	EnvValue           string
	SessionConfigValue string
}

func (e *ModeMismatchError) Error() string {
	return fmt.Sprintf(
		"boot: %s=%q does not match NARVI_SESSION_CONFIG's bootMode=%q",
		bootModeEnvVar, e.EnvValue, e.SessionConfigValue,
	)
}

// SandboxIDMismatchError is returned by Load when NARVI_SANDBOX_ID is
// explicitly set to a non-empty value AND NARVI_SESSION_CONFIG is present
// with its own non-empty sandboxId field, but the two disagree -- the same
// shape and reasoning as ModeMismatchError: a caller-side bug (something
// set both, inconsistently) must be caught before either value is
// trusted, rather than silently preferring one over the other.
type SandboxIDMismatchError struct {
	EnvValue           string
	SessionConfigValue string
}

func (e *SandboxIDMismatchError) Error() string {
	return fmt.Sprintf(
		"boot: %s=%q does not match NARVI_SESSION_CONFIG's sandboxId=%q",
		sandboxIDEnvVar, e.EnvValue, e.SessionConfigValue,
	)
}

// InvalidLogLevelError is returned by Load when NARVI_LOG_LEVEL is set to
// a value that is not (case-insensitively) one of debug, info, warn, or
// error. Same shape and reasoning as platform/config.go's own
// InvalidLogLevelError -- deliberately re-implemented rather than shared,
// since sandbox-agent's config is otherwise entirely disjoint from
// control-plane's.
type InvalidLogLevelError struct {
	Value string
}

func (e *InvalidLogLevelError) Error() string {
	return fmt.Sprintf("boot: invalid %s=%q: must be one of %q, %q, %q, %q (case-insensitive)",
		logLevelEnvVar, e.Value, "debug", "info", "warn", "error")
}

// parseLogLevel validates raw (already defaulted to defaultLogLevelValue
// if the env var was unset) against the four accepted spellings,
// case-insensitively -- an explicit switch, not slog.Level.UnmarshalText,
// for the same reason platform/config.go's own parseLogLevel uses one:
// UnmarshalText also accepts numeric offset suffixes like "error-8", more
// than this env var is specified to take.
func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, &InvalidLogLevelError{Value: raw}
	}
}

// InvalidRuntimeUIDError is returned by Load when NARVI_RUNTIME_UID is set
// to a non-empty value that does not parse as a uint32. Same fail-fast
// shape as InvalidLogLevelError above; see InvalidRuntimeGIDError just
// below for the gid counterpart.
type InvalidRuntimeUIDError struct {
	Value string
}

func (e *InvalidRuntimeUIDError) Error() string {
	return fmt.Sprintf("boot: invalid %s=%q: must be a non-negative integer", runtimeUIDEnvVar, e.Value)
}

// InvalidRuntimeGIDError is InvalidRuntimeUIDError's own gid counterpart,
// returned by Load when NARVI_RUNTIME_GID is set to a non-empty value
// that does not parse as a uint32.
type InvalidRuntimeGIDError struct {
	Value string
}

func (e *InvalidRuntimeGIDError) Error() string {
	return fmt.Sprintf("boot: invalid %s=%q: must be a non-negative integer", runtimeGIDEnvVar, e.Value)
}

// RuntimeUIDIsRootError is returned by Load when NARVI_RUNTIME_UID is
// explicitly set to "0" -- a Credential naming uid 0 drops no privilege
// at all, silently defeating §30.5's entire purpose. See
// Config.RuntimeUID's own doc comment for why this is a loud fail-closed,
// never a quiet regression, mirroring §30.4's own scope-introspection
// posture for a different credential; see RuntimeGIDIsRootError just
// below for the gid counterpart.
type RuntimeUIDIsRootError struct{}

func (e *RuntimeUIDIsRootError) Error() string {
	return fmt.Sprintf("boot: %s=0 (root) would not drop any privilege; refusing to boot", runtimeUIDEnvVar)
}

// RuntimeGIDIsRootError is RuntimeUIDIsRootError's own gid counterpart,
// returned by Load when NARVI_RUNTIME_GID is explicitly set to "0".
type RuntimeGIDIsRootError struct{}

func (e *RuntimeGIDIsRootError) Error() string {
	return fmt.Sprintf("boot: %s=0 (root) would not drop any privilege; refusing to boot", runtimeGIDEnvVar)
}

// InvalidGitDirRootError is returned by Load when NARVI_GIT_DIR_ROOT is
// set to an empty value, a non-absolute path, or a path nested under
// WorkspaceDir. See Config.GitDirRoot's own doc comment for why the
// under-WorkspaceDir case specifically is refused, not merely
// discouraged.
type InvalidGitDirRootError struct {
	Value  string
	Reason string
}

func (e *InvalidGitDirRootError) Error() string {
	return fmt.Sprintf("boot: invalid %s=%q: %s", gitDirRootEnvVar, e.Value, e.Reason)
}

// validateGitDirRoot enforces Config.GitDirRoot's own four requirements:
// non-empty, absolute, not nested under (or equal to) workspaceDir, and
// not itself containing (or equal to) workspaceDir.
//
// (Correction, review): the original version of this function only ever
// checked the first direction (root under-or-equal workspaceDir). It
// never checked the REVERSE -- workspaceDir under-or-equal root -- which
// is exactly as dangerous: gitdir.Layout.Repo builds GitDir as
// filepath.Join(Root, name), and gitdir.Seed's very first destructive
// step is os.RemoveAll(repo.GitDir). An operator who sets GitDirRoot to
// WorkspaceDir's own parent directory (e.g. WorkspaceDir=/srv/narvi/
// workspace, GitDirRoot=/srv/narvi) makes a session repo NAMED
// "workspace" resolve GitDir to WorkspaceDir itself
// (filepath.Join("/srv/narvi", "workspace") == "/srv/narvi/workspace") --
// reposource.ValidateRepoName accepts "workspace" as an ordinary
// identifier -- so Seed's own RemoveAll deletes the entire workspace,
// every repo in it, not just the one being (re-)seeded. Both directions
// are checked here, symmetrically, against CLEANED paths (filepath.Clean
// -- so a trailing slash or a "GitDirRoot/.." style value cannot dodge
// either check).
func validateGitDirRoot(root, workspaceDir string) error {
	if root == "" {
		return &InvalidGitDirRootError{Value: root, Reason: "must not be empty"}
	}
	if !filepath.IsAbs(root) {
		return &InvalidGitDirRootError{Value: root, Reason: "must be an absolute path"}
	}
	cleanRoot := filepath.Clean(root)
	cleanWorkspace := filepath.Clean(workspaceDir)
	if isPathUnderOrEqual(cleanRoot, cleanWorkspace) {
		return &InvalidGitDirRootError{Value: root, Reason: fmt.Sprintf("must not be nested under WorkspaceDir (%s) -- an agent-owned git-dir inside the runtime-owned workspace tree defeats the structural guarantee §30.5 provides", workspaceDir)}
	}
	if isPathUnderOrEqual(cleanWorkspace, cleanRoot) {
		return &InvalidGitDirRootError{Value: root, Reason: fmt.Sprintf("must not contain WorkspaceDir (%s) -- a session repo name could then resolve GitDirRoot/<name> to WorkspaceDir itself, letting gitdir.Seed's own RemoveAll delete the whole workspace", workspaceDir)}
	}
	return nil
}

// isPathUnderOrEqual reports whether path is base itself, or nested
// anywhere under it -- checked via filepath.Rel: a relative result of "."
// (equal) or one with no leading ".." segment (a strict descendant) both
// count. Both arguments are expected already-cleaned (filepath.Clean);
// this function does not clean them itself.
func isPathUnderOrEqual(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// parseRuntimeID parses raw (the env var's own raw string value, "" when
// unset) as a uint32, applying fallback when raw is empty and rejecting 0
// unconditionally (see RuntimeUIDIsRootError/RuntimeGIDIsRootError's own
// doc comment) -- shared by both NARVI_RUNTIME_UID and NARVI_RUNTIME_GID,
// which validate identically apart from which named error each returns.
func parseRuntimeID(raw string, fallback uint32) (uint32, bool, error) {
	if raw == "" {
		return fallback, false, nil
	}
	parsed, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, false, err
	}
	return uint32(parsed), parsed == 0, nil
}

// Load reads and validates sandbox-agent's process configuration,
// fail-fast, from environment variables. NARVI_BOOT_MODE is required (§6.4,
// explicit); everything else is optional with a documented default.
func Load() (Config, error) {
	mode, err := sandboxboot.ParseBootMode(os.Getenv(bootModeEnvVar))
	if err != nil {
		return Config{}, fmt.Errorf("boot: %s: %w", bootModeEnvVar, err)
	}

	agentVersion := os.Getenv(agentVersionEnvVar)
	if agentVersion == "" {
		agentVersion = defaultAgentVersion
	}

	imageDigest := os.Getenv(imageDigestEnvVar)
	if imageDigest == "" {
		imageDigest = defaultImageDigest
	}

	workspaceDir := os.Getenv(workspaceDirEnvVar)
	if workspaceDir == "" {
		workspaceDir = defaultWorkspaceDir
	}

	rawLogLevel := os.Getenv(logLevelEnvVar)
	if rawLogLevel == "" {
		rawLogLevel = defaultLogLevelValue
	}
	logLevel, err := parseLogLevel(rawLogLevel)
	if err != nil {
		return Config{}, err
	}

	credentialCacheDir := os.Getenv(credentialCacheDirEnvVar)
	if credentialCacheDir == "" {
		credentialCacheDir = defaultCredentialCacheDir
	}

	runtimeUID, uidIsZero, err := parseRuntimeID(os.Getenv(runtimeUIDEnvVar), defaultRuntimeUID)
	if err != nil {
		return Config{}, &InvalidRuntimeUIDError{Value: os.Getenv(runtimeUIDEnvVar)}
	}
	if uidIsZero {
		return Config{}, &RuntimeUIDIsRootError{}
	}

	runtimeGID, gidIsZero, err := parseRuntimeID(os.Getenv(runtimeGIDEnvVar), defaultRuntimeGID)
	if err != nil {
		return Config{}, &InvalidRuntimeGIDError{Value: os.Getenv(runtimeGIDEnvVar)}
	}
	if gidIsZero {
		return Config{}, &RuntimeGIDIsRootError{}
	}

	gitDirRoot := os.Getenv(gitDirRootEnvVar)
	if gitDirRoot == "" {
		gitDirRoot = defaultGitDirRoot
	}
	if err := validateGitDirRoot(gitDirRoot, workspaceDir); err != nil {
		return Config{}, err
	}

	// sessionConfig is resolved BEFORE sandboxID below -- sandboxID's own
	// resolution needs to know whether a SessionConfig is present (and,
	// if so, its own SandboxId) to pick the right value/detect a mismatch.
	sessionConfig, err := loadSessionConfig(mode)
	if err != nil {
		return Config{}, err
	}

	sandboxID, err := resolveSandboxID(os.Getenv(sandboxIDEnvVar), sessionConfig)
	if err != nil {
		return Config{}, err
	}

	return Config{
		BootMode:           mode,
		AgentVersion:       agentVersion,
		ImageDigest:        imageDigest,
		WorkspaceDir:       workspaceDir,
		LogLevel:           logLevel,
		CredentialCacheDir: credentialCacheDir,
		RuntimeUID:         runtimeUID,
		RuntimeGID:         runtimeGID,
		GitDirRoot:         gitDirRoot,
		SandboxID:          sandboxID,
		SessionConfig:      sessionConfig,
	}, nil
}

// resolveSandboxID implements Config.SandboxID's own documented priority
// order: envValue (NARVI_SANDBOX_ID), if explicitly non-empty, always
// wins over sessionConfig -- but a non-empty, DISAGREEING pair of the two
// is a fail-fast *SandboxIDMismatchError, never a silent preference.
// Otherwise, sessionConfig's own SandboxId is used when sessionConfig is
// present; otherwise defaultSandboxID.
func resolveSandboxID(envValue string, sessionConfig *sessionconfig.SessionConfig) (string, error) {
	if envValue != "" && sessionConfig != nil && sessionConfig.SandboxId != "" && sessionConfig.SandboxId != envValue {
		return "", &SandboxIDMismatchError{
			EnvValue:           envValue,
			SessionConfigValue: sessionConfig.SandboxId,
		}
	}

	if envValue != "" {
		return envValue, nil
	}

	if sessionConfig != nil {
		return sessionConfig.SandboxId, nil
	}

	return defaultSandboxID, nil
}

// loadSessionConfig reads and parses NARVI_SESSION_CONFIG when present,
// cross-checking its bootMode field against mode (the already-resolved
// NARVI_BOOT_MODE value). Returns (nil, nil) when the env var is unset --
// a fully valid, correct state (see Config.SessionConfig's own doc
// comment). A malformed JSON document is a wrapped, propagated error
// (fail-fast, matching every other Load() failure mode); a document
// missing a required SessionConfig field surfaces the generated
// UnmarshalJSON's own error UNWRAPPED and un-rehashed, per this Step's own
// scope (SessionConfig's generated json.Unmarshal already validates every
// required field is present).
func loadSessionConfig(mode sandboxboot.BootMode) (*sessionconfig.SessionConfig, error) {
	raw := os.Getenv(SessionConfigEnvVar)
	if raw == "" {
		return nil, nil
	}

	var sc sessionconfig.SessionConfig
	if err := json.Unmarshal([]byte(raw), &sc); err != nil {
		return nil, fmt.Errorf("boot: %s: %w", SessionConfigEnvVar, err)
	}

	if string(sc.BootMode) != string(mode) {
		return nil, &ModeMismatchError{
			EnvValue:           string(mode),
			SessionConfigValue: string(sc.BootMode),
		}
	}

	return &sc, nil
}
