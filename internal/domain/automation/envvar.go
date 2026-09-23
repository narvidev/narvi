package automation

import (
	"errors"
	"fmt"

	"github.com/narvidev/narvi/internal/domain/sandboxsecret"
)

// EnvVar is one entry of §8.4's own "per-automation env vars" -- PLAIN
// configuration threaded into every run this automation fans out (app/
// automation's own fanout.go), NEVER a secret. See this package's own
// doc.go for the explicit, documented decision to defer per-automation
// SECRETS to §25.1 -- EnvVar is that decision's own opposite case: data
// this package DOES implement, precisely because it carries no
// confidentiality requirement at all (a feature-flag name, a target
// environment label, a non-sensitive tuning parameter).
type EnvVar struct {
	// Name is the environment variable's own name -- validated by
	// ValidateEnvVars against envVarNamePattern below.
	Name string
	// Value is the environment variable's own value -- unvalidated beyond
	// "present" (an empty string is a legitimate value for some env vars,
	// e.g. an intentionally-blank feature flag).
	Value string
}

// MaxEnvVars is this package's own explicit cap on how many EnvVar entries
// a single automation may carry -- mirrors MaxFanOutTargets' own identical
// "an explicit, small, application-enforced cap on an otherwise-unbounded
// JSONB array" precedent (target.go).
const MaxEnvVars = 50

// Sentinel errors ValidateEnvVars can return -- mirrors this codebase's own
// established sentinel-error house style (internal/domain/environment's
// ErrEmptyPattern et al.) rather than a bare fmt.Errorf string.
var (
	// ErrTooManyEnvVars means the candidate list exceeds MaxEnvVars.
	ErrTooManyEnvVars = errors.New("automation: too many env vars")
	// ErrEmptyEnvVarName means an EnvVar's own Name was the empty string.
	ErrEmptyEnvVarName = errors.New("automation: env var name must not be empty")
	// ErrInvalidEnvVarName means an EnvVar's own Name failed
	// envVarNamePattern -- not a syntactically legal POSIX shell/env
	// variable name.
	ErrInvalidEnvVarName = errors.New("automation: env var name is not a valid identifier")
	// ErrDuplicateEnvVarName means the same Name appeared more than once in
	// one candidate list -- ambiguous (which value would actually apply to
	// the dispatched turn's own environment), so rejected outright rather
	// than silently letting the last one win.
	ErrDuplicateEnvVarName = errors.New("automation: duplicate env var name")
	// ErrReservedEnvVarName means an EnvVar's own Name collides with a
	// namespace or exact name another injection mechanism already owns --
	// checked because this Step threads automation env vars into cmd.Env
	// alongside provider credentials (§25.1/§25.3) and sandbox secrets
	// (§27.1), the SAME reserved-name/prefix set that already governs
	// those two paths (sandboxsecret.ValidateNotReserved, reused here
	// rather than a second, independently maintained list that could
	// drift from it). Wraps the specific sandboxsecret sentinel
	// (ErrNameReservedNarviNamespace/ErrNameReservedOpenCodeNamespace/
	// ErrNameReservedProviderCredential/ErrNameReservedCloudIdentity/
	// ErrNameReservedClusterBinding) so a caller can branch on either the
	// generic automation-level reason or the specific underlying one.
	ErrReservedEnvVarName = errors.New("automation: env var name is reserved by another injection mechanism")
)

// InvalidEnvVarError reports a single candidate EnvVar ValidateEnvVars
// rejected, and why -- mirrors environment.InvalidGlobError's own shape
// exactly.
type InvalidEnvVarError struct {
	// Name is the offending EnvVar's own Name, verbatim.
	Name string
	// Reason is one of ErrEmptyEnvVarName, ErrInvalidEnvVarName,
	// ErrReservedEnvVarName, or ErrDuplicateEnvVarName -- the base
	// sentinel this error unwraps to.
	Reason error
}

func (e *InvalidEnvVarError) Error() string {
	return fmt.Sprintf("automation: invalid env var %q: %s", e.Name, e.Reason)
}

func (e *InvalidEnvVarError) Unwrap() error { return e.Reason }

// isValidEnvVarName reports whether name is a syntactically legal POSIX
// shell/environment variable name: one or more ASCII letters/digits/
// underscore, not starting with a digit -- the same restriction every
// POSIX shell itself enforces on `export NAME=value`, so a name this
// function accepts is guaranteed to survive being written into the
// dispatched turn's own process environment without special-character
// escaping concerns.
func isValidEnvVarName(name string) bool {
	for i, r := range name {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			continue
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
			continue
		default:
			return false
		}
	}
	return true
}

// ValidateEnvVars validates a candidate []EnvVar list before it is accepted
// onto an automation, at creation/update time: at most MaxEnvVars entries,
// each with a non-empty, syntactically valid Name not reserved by another
// injection mechanism, and no two entries sharing the same Name. Returns
// the first problem found (and stops) -- same "first error wins, no
// accumulation" convention as environment.ValidatePathScope.
//
// The reservation check (sandboxsecret.ValidateNotReserved) is this
// Step's own addition: once an automation's env_vars are threaded into
// cmd.Env alongside provider credentials/sandbox secrets, a name this
// package previously accepted (e.g. "ANTHROPIC_API_KEY" or "KUBECONFIG")
// would silently shadow -- or be shadowed by -- a mechanism that name
// already belongs to, rather than merely sitting unused in a prompt
// preamble. This is a fail-CLOSED check at the one write path automation
// env vars have (CreateAutomation, internal/adapters/inbound/httpapi/
// automations.go) -- unlike sandbox_secrets, automations have no separate
// UPDATE route yet and no second write path to re-validate at (contrast
// fetchSandboxSecrets' own defense-in-depth re-validation, cmd/sandbox-
// agent/sandboxsecrets.go), so this single check is the only fence.
//
// Deliberately does NOT also reject PATH/HOME the way this package's
// caller (cmd/sandbox-agent) could still be handed an automation env var
// named either: mirrors sandboxsecret.ValidateName's own identical,
// already-shipped position (name.go: neither is in the reserved set
// there either) -- for the SAME reason. A value threaded via
// opencodeproc.Spawn's own sandboxSecretEnv parameter (which this Step's
// automation env vars are folded into, cmd/sandbox-agent/main.go) only
// ever reaches the spawned opencode process's own cmd.Env, NEVER
// sandbox-agent's own os.Setenv'd process environment (see that
// parameter's own doc comment for the incident this architecture
// structurally prevents) -- so a PATH/HOME collision here is contained to
// a maintainer's own automation misconfiguring its own session's agent
// process, never a hazard to sandbox-agent itself.
func ValidateEnvVars(vars []EnvVar) error {
	if len(vars) > MaxEnvVars {
		return ErrTooManyEnvVars
	}
	seen := make(map[string]struct{}, len(vars))
	for _, v := range vars {
		if v.Name == "" {
			return &InvalidEnvVarError{Name: v.Name, Reason: ErrEmptyEnvVarName}
		}
		if !isValidEnvVarName(v.Name) {
			return &InvalidEnvVarError{Name: v.Name, Reason: ErrInvalidEnvVarName}
		}
		if err := sandboxsecret.ValidateNotReserved(v.Name); err != nil {
			return &InvalidEnvVarError{Name: v.Name, Reason: fmt.Errorf("%w: %w", ErrReservedEnvVarName, err)}
		}
		if _, dup := seen[v.Name]; dup {
			return &InvalidEnvVarError{Name: v.Name, Reason: ErrDuplicateEnvVarName}
		}
		seen[v.Name] = struct{}{}
	}
	return nil
}
