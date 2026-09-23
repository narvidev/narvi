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
//
// # Where a resolved value actually goes (corrected, W4)
//
// A prior version of this comment claimed a value threaded through
// opencodeproc.Spawn's own sandboxSecretEnv parameter "only ever reaches
// the spawned opencode process's own cmd.Env" -- false, disproved during
// adversarial review with a real setup.sh. The same slice this package's
// own resolved env vars are folded into (cmd/sandbox-agent/main.go's own
// automationAndSandboxSecretEnv, automationenvvars.go) also reaches, at
// SANDBOX-AGENT'S OWN uid -- root in production, no credential drop --
// a repo's own setup.sh/start.sh hooks (internal/sandboxagent/boot/
// hooks.go's runHook) and dockerd (internal/sandboxagent/boot/docker.go's
// RunDocker), and reaches a repo's own services.yml-declared services
// (internal/sandboxagent/services.Run, dropped to the runtime's OWN uid
// via runtimeCredential, internal/sandboxagent/boot/runboot.go) -- all
// BEFORE opencode is ever spawned.
//
// Accepted anyway, on two grounds that actually hold:
//
//  1. Parity: this is the EXACT SAME slice sandbox_secrets already
//     threads through the same call sites (§27.1) -- an automation env
//     var carries no reach a sandbox secret did not already have.
//  2. The same role gate: creating/editing an automation
//     (authz.ActionManageAutomations) and managing a repo/environment
//     secret (authz.ActionManageRepoSecrets/ActionManageEnvSecrets) are
//     ALL roles(RoleAdmin, RoleMaintainer)-only (internal/domain/authz/
//     authorize.go) -- this reach is available only to the same
//     privileged callers who could already reach it via sandbox_secrets.
//
// Given that reach, this package no longer excludes PATH/HOME/LD_PRELOAD
// and siblings from the reserved set either (a change from this
// comment's own prior, incorrect position that reach was contained to
// opencode's own env) -- ValidateEnvVarShapeAndReservation's own
// sandboxsecret.ValidateNotReserved call now refuses them, for BOTH
// sandbox_secrets and automation env vars at once (internal/domain/
// sandboxsecret/name.go's own processHijackReservedNames doc comment has
// the full per-name "why", including the cross-PR GIT_ALLOW_PROTOCOL
// collision this closes).
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

// ValidateEnvVarShapeAndReservation validates ONE candidate name: non-
// empty, POSIX-identifier shaped (isValidEnvVarName, above -- lowercase
// permitted, see that function's own doc comment for why), and not
// reserved by another injection mechanism (sandboxsecret.
// ValidateNotReserved -- the "one owning mechanism per env-var name" rule
// §27.1 established, which now also covers the process-hijack surface,
// PATH/HOME/LD_PRELOAD and siblings -- see name.go's own
// processHijackReservedNames doc comment for the full per-name "why").
// Factored out of ValidateEnvVars (below), which still runs this AND its
// own list-level MaxEnvVars/duplicate checks together, unchanged --
// specifically so a SECOND caller can reuse the exact same per-name rule
// without also inheriting those list-level checks, which make no sense
// against a single already-delivered name: cmd/sandbox-agent/
// automationenvvars.go's own injection-boundary re-validation
// (adversarial-review MEDIUM fix -- an earlier version there called
// sandboxsecret.ValidateNotReserved alone, re-checking reservation but
// never shape, so a name containing "=", an empty name, or a
// leading-digit name -- none legal in a POSIX environment, any of them
// capable of confusing an exec.Cmd.Env consumer -- could still reach
// cmd.Env if it arrived at that boundary by any path other than through
// ValidateEnvVars). One shared definition, not two independently
// maintained copies that could drift apart -- see ValidateEnvVars' own
// doc comment for why this is still not the ONLY fence in practice.
func ValidateEnvVarShapeAndReservation(name string) error {
	if name == "" {
		return &InvalidEnvVarError{Name: name, Reason: ErrEmptyEnvVarName}
	}
	if !isValidEnvVarName(name) {
		return &InvalidEnvVarError{Name: name, Reason: ErrInvalidEnvVarName}
	}
	if err := sandboxsecret.ValidateNotReserved(name); err != nil {
		return &InvalidEnvVarError{Name: name, Reason: fmt.Errorf("%w: %w", ErrReservedEnvVarName, err)}
	}
	return nil
}

// ValidateEnvVars validates a candidate []EnvVar list before it is accepted
// onto an automation, at creation/update time: at most MaxEnvVars entries,
// each valid per ValidateEnvVarShapeAndReservation (above), and no two
// entries sharing the same Name. Returns the first problem found (and
// stops) -- same "first error wins, no accumulation" convention as
// environment.ValidatePathScope.
//
// # This is not the only fence, and CreateAutomation is not the only write path
//
// A prior version of this comment claimed CreateAutomation (internal/
// adapters/inbound/httpapi/automations.go) was "the one write path
// automation env vars have" and this function "the only fence" -- an
// adversarial review round (W7) found both false. internal/app/seed's own
// seedAutomation (internal/app/seed/automations.go) writes
// automations.env_vars directly, via sqlcgen.CreateAutomationParams, from
// a `control-plane seed -manifest <path>` run -- WITHOUT itself calling
// this function. That does not leave the seed path unvalidated: a later
// "correction" to this comment claimed it did, and named a
// seedmanifest.ValidateManifest that does not exist -- both false. The
// real seed-apply path is controlplane/seed.go's own runSeedCommand ->
// seed.LoadManifest (internal/app/seed/manifest_io.go) ->
// seedmanifest.Validate (validate.go), which runSeedCommand calls and
// aborts the command on error BEFORE seed.Run -> seedAutomation ever
// writes -- and seedmanifest.Validate's own validateAutomations calls
// this exact function on every manifest automation's env vars. So this
// write path has TWO independent fences, not the single wrongly-named
// one an earlier revision of this comment asserted: seedmanifest.Validate
// at load time, and cmd/sandbox-agent/automationenvvars.go's own
// fetchAutomationEnvVars -- ALREADY a second, independent re-validation
// of every DELIVERED name, regardless of which write path produced it --
// at the injection boundary. THAT re-validation is still the guarantee
// that actually holds end to end even if a future write path forgets to
// call ValidateEnvVars at all: it sits at the one place every automation
// env var must pass through before it ever reaches cmd.Env. spawn.go's
// own former "a collision ... is structurally impossible ... ValidateEnvVars
// already rejects [it], at CreateAutomation's own write path" leaned on
// this same wrong guard -- corrected there too, to point at the same
// injection-boundary re-validation instead.
func ValidateEnvVars(vars []EnvVar) error {
	if len(vars) > MaxEnvVars {
		return ErrTooManyEnvVars
	}
	seen := make(map[string]struct{}, len(vars))
	for _, v := range vars {
		if err := ValidateEnvVarShapeAndReservation(v.Name); err != nil {
			return err
		}
		if _, dup := seen[v.Name]; dup {
			return &InvalidEnvVarError{Name: v.Name, Reason: ErrDuplicateEnvVarName}
		}
		seen[v.Name] = struct{}{}
	}
	return nil
}
