package sandboxsecret

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/narvidev/narvi/internal/domain/cloudidentity"
	"github.com/narvidev/narvi/internal/domain/clusterbinding"
	"github.com/narvidev/narvi/internal/domain/providercredential"
)

// maxNameLength bounds a sandbox_secrets name. Not specified by §27.1;
// chosen generously (an ordinary env-var name is a handful of words at
// most) purely to reject pathological input before it reaches Postgres --
// mirrors this codebase's own "generous, not tuned" precedent for bounds
// with no specified figure (e.g. maxRequestBodyBytes,
// maxProviderCredentialsResponseSize).
const maxNameLength = 256

// narviReservedPrefix is the reserved env-var namespace §19.8 already
// established (NARVI_BOOT_MODE, NARVI_SESSION_CONFIG, and 6 siblings --
// internal/sandboxagent/boot/config.go's own env-var constants, all 8 of
// which start with this exact prefix) and §27.1 requires ValidateName to
// reject outright. A prefix check (rather than an enumerated list of the
// 8 CURRENT names) is deliberate and fail-closed: it also rejects any
// FUTURE NARVI_* var this codebase adds later, without ValidateName ever
// needing to change again -- exactly the reservation §19.8's own closing
// sentence asks for ("must be reserved and excluded before any
// user-settable env surface ships").
const narviReservedPrefix = "NARVI_"

// OpenCodeReservedPrefix reserves the ENTIRE "OPENCODE_" env-var namespace
// -- adversarial-review CRITICAL fix: §27.2's own injection owns
// OPENCODE_CONFIG (OpenCode's documented "custom config" slot env var,
// cmd/sandbox-agent/opencodeconfig.go), and a future engine
// version could grow OPENCODE_CONFIG_CONTENT (OpenCode's documented
// "inline config" slot, ABOVE even the project slot in OpenCode's own
// precedence -- see opencodeconfig.go's own top doc comment). Before this
// fix, NEITHER name was rejected here, so a maintainer holding
// ActionManageEnvSecrets could save a sandbox_secrets row literally named
// "OPENCODE_CONFIG_CONTENT", which applySandboxSecretEnv/opencodeproc.
// Spawn would then thread into `opencode serve`'s own env, letting a
// customer-authored value at OpenCode's HIGHEST-precedence slot override
// §8.2's sentinel-fix capability-restriction write (which targets the
// LOWER-precedence project slot) -- exactly the "a customer-authored
// config can never override the security-relevant agent restriction"
// guarantee §27.2 claims, defeated by this Step's OWN sibling mechanism.
//
// A prefix (not an enumerated {OPENCODE_CONFIG, OPENCODE_CONFIG_CONTENT}
// pair) is the fail-closed choice, exactly mirroring narviReservedPrefix's
// own reasoning immediately above: it also rejects any OpenCode env var
// this codebase does not yet inject (or does not yet know exists) without
// ValidateName ever needing another edit. Exported (unlike
// narviReservedPrefix) specifically so cmd/sandbox-agent/opencodeconfig.go
// can build its own openCodeConfigEnvVar constant FROM this exact value
// (openCodeConfigEnvVar = OpenCodeReservedPrefix + "CONFIG") rather than
// repeating the literal "OPENCODE_" independently -- one source, so the
// reservation and the injection can never drift apart again the way they
// did before this fix (see that file's own doc comment).
const OpenCodeReservedPrefix = "OPENCODE_"

// posixEnvVarNamePattern is the classic POSIX "Environment Variable Name"
// shape (IEEE Std 1003.1: "words consisting solely of uppercase letters,
// digits, and the '_'... and do not begin with a digit") -- the same
// shape every name this codebase already treats as an env var uses
// (NARVI_BOOT_MODE, ANTHROPIC_API_KEY, GOOGLE_GENERATIVE_AI_API_KEY, ...).
// Uppercase-only is a deliberate reading of "POSIX env-var shape" (§27.1),
// not an arbitrary tightening: every existing reserved name in this
// codebase (the 8 NARVI_* vars, all 5 providercredential.EnvVarNames
// entries) is uppercase, and a lowercase env var, while technically
// readable by a shell, is not the portable, conventional shape §27.1's
// own wording invokes.
var posixEnvVarNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// Sentinel errors ValidateName returns, wrapped via fmt.Errorf(w"%w: ...")
// so a caller can branch on the REASON (errors.Is) while a human-facing
// message still names the offending value -- mirrors this codebase's own
// established named-sentinel-error precedent (e.g. platform.
// ModeMismatchError, though those are structs; these are plain sentinels
// since no structured field beyond the name itself is ever needed by a
// caller).
var (
	// ErrNameEmpty means name is the empty string.
	ErrNameEmpty = errors.New("sandboxsecret: name must not be empty")
	// ErrNameTooLong means name exceeds maxNameLength.
	ErrNameTooLong = errors.New("sandboxsecret: name too long")
	// ErrNameShape means name does not match posixEnvVarNamePattern --
	// not POSIX env-var shaped (uppercase letters/digits/underscore,
	// must not start with a digit).
	ErrNameShape = errors.New("sandboxsecret: name is not a valid POSIX environment variable name (uppercase letters, digits, underscore; must not start with a digit)")
	// ErrNameReservedNarviNamespace means name starts with
	// narviReservedPrefix ("NARVI_") -- §19.8's own reservation.
	ErrNameReservedNarviNamespace = errors.New("sandboxsecret: name is in the reserved NARVI_ namespace")
	// ErrNameReservedOpenCodeNamespace means name starts with
	// OpenCodeReservedPrefix ("OPENCODE_") -- adversarial-review CRITICAL
	// fix, see that constant's own doc comment for the full "why".
	ErrNameReservedOpenCodeNamespace = errors.New("sandboxsecret: name is in the reserved OPENCODE_ namespace")
	// ErrNameReservedProviderCredential means name is exactly one of
	// providercredential.AllEnvVarNames -- already owned by §25.1's
	// provider-credential injection mechanism. §27.1's own "one owning
	// mechanism per env-var name" rule: this collision is refused, never
	// silently shadowed by whichever mechanism happens to write its env
	// entry last.
	ErrNameReservedProviderCredential = errors.New("sandboxsecret: name is already owned by provider credential injection")
	// ErrNameReservedCloudIdentity means name is exactly one of
	// cloudidentity.ReservedEnvVarNames -- already owned by §27.4's own
	// §27.3 cloud-identity-consumption mechanism (AWS_WEB_IDENTITY_TOKEN_
	// FILE, AWS_ROLE_ARN, AWS_ROLE_SESSION_NAME, GOOGLE_APPLICATION_
	// CREDENTIALS, AZURE_FEDERATED_TOKEN_FILE, AZURE_CLIENT_ID,
	// AZURE_TENANT_ID). Without this, a maintainer holding
	// ActionManageEnvSecrets could define e.g. AWS_ROLE_ARN as a sandbox
	// secret and redirect the whole federation mechanism to a role of
	// their own choosing -- exactly the class of hijack
	// ErrNameReservedProviderCredential/ErrNameReservedOpenCodeNamespace
	// already close for their own mechanisms.
	ErrNameReservedCloudIdentity = errors.New("sandboxsecret: name is already owned by cloud identity injection")
	// ErrNameReservedClusterBinding means name is exactly
	// clusterbinding.EnvVarKubeconfig ("KUBECONFIG") -- already owned by
	// §27.4's own §27.4 kubeconfig-injection mechanism, for the
	// identical "one owning mechanism per env-var name" reason
	// ErrNameReservedCloudIdentity's own doc comment gives.
	ErrNameReservedClusterBinding = errors.New("sandboxsecret: name is already owned by kubeconfig injection")
	// ErrNameReservedProcessHijack means name is one of
	// processHijackReservedNames or starts with gitConfigReservedPrefix --
	// see that slice's own doc comment for the full per-name "why". A
	// different HAZARD CLASS than every reservation above: those protect
	// one specific injection MECHANISM from being shadowed; this protects
	// every ROOT-UID CONSUMER of a resolved sandbox_secrets/automation
	// env var row (a repo's own setup.sh/start.sh, dockerd) from having
	// its own binary/library/startup-file resolution redirected by a
	// value that was never meant to be anything more than an ordinary
	// application-level env var.
	ErrNameReservedProcessHijack = errors.New("sandboxsecret: name is reserved -- it can redirect which binary, library, or startup file a spawned root-uid process runs, not merely configure that process's own behavior")
)

// processHijackReservedNames / gitConfigReservedPrefix ("process-hijack
// surface", adversarial-review LOW-severity fix, §8 item 4 review round):
// exact names (plus one prefix) that let a value threaded through
// cmd.Env redirect which BINARY, LIBRARY, or STARTUP FILE a spawned
// process actually runs, rather than merely configuring that process's
// own behavior -- a fundamentally different hazard class than every
// OTHER name this file reserves (those protect one specific injection
// MECHANISM from being shadowed by a same-named row; these protect every
// CONSUMER of a resolved row from having its own process substrate
// hijacked).
//
// Reserved HERE, at the one shared "one owning mechanism per env-var
// name" choke point both cmd/sandbox-agent/sandboxsecrets.go's own
// fetchSandboxSecrets (general sandbox_secrets) and internal/domain/
// automation.ValidateEnvVarShapeAndReservation (automation env vars,
// reused by both CreateAutomation's own write-time check AND cmd/
// sandbox-agent/automationenvvars.go's own injection-boundary
// re-validation) already call -- rather than in only one of the two --
// because BOTH mechanisms fold into the EXACT SAME sandboxSecretEnv
// slice (cmd/sandbox-agent/main.go's own automationAndSandboxSecretEnv)
// that reaches a repo's own setup.sh/start.sh hooks (internal/
// sandboxagent/boot/hooks.go's own runHook) and dockerd (internal/
// sandboxagent/boot/docker.go's own RunDocker) at SANDBOX-AGENT'S OWN
// uid -- root in production, no credential drop at all, unlike
// opencodeproc.Spawn's own single call site, which both env-filters
// (supervisor.EnvWithout) and uid-drops (runtimeCredential) -- see
// internal/domain/automation/envvar.go's own top doc comment for the
// full "where these values actually go" audit this reservation responds
// to. Reserving here closes the hazard for BOTH mechanisms at once, for
// every FUTURE fetch, with one edit:
//
//   - PATH: a hook/dockerd process's own bare-name exec.Command
//     lookups (`npm`, `git`, `curl`, ...) resolve against ITS OWN
//     cmd.Env PATH at the point IT execs something, not sandbox-agent's
//     (contrast sandbox-agent's OWN exec.Command call that spawns the
//     hook in the first place, which resolves against the CALLING
//     process's env and is therefore untouched by this -- see
//     opencodeproc.Spawn's own doc comment for that exact distinction).
//     A poisoned PATH here silently substitutes a trojan binary for the
//     real one, running as root.
//   - HOME: redirects every dotfile a root-uid hook/dockerd process
//     reads at startup (~/.bashrc, ~/.gitconfig, ~/.npmrc, ~/.docker/
//     config.json, an SSH client config, ...) to an attacker-chosen
//     location.
//   - LD_PRELOAD / LD_LIBRARY_PATH: injects an arbitrary shared object
//     into every dynamically-linked binary a hook/dockerd process (or
//     anything it spawns) loads.
//   - BASH_ENV / ENV: bash and POSIX sh both source the file this
//     names before running ANY non-interactive script -- exactly
//     runHook's own invocation shape (a `#!/bin/sh`/`#!/bin/bash`
//     setup.sh) -- arbitrary code execution as root, before the
//     script's own first line ever runs.
//   - NODE_OPTIONS: Node.js reads this at every invocation and honors
//     "--require <path>" inside it -- arbitrary code execution at
//     process start, the identical hazard class as BASH_ENV for a
//     Node-based hook/service.
//   - PYTHONSTARTUP: CPython's own identical hazard for a Python-based
//     hook/service.
//   - GIT_SSH_COMMAND / GIT_SSH: git substitutes this for its own ssh
//     invocation outright -- arbitrary command execution for any hook/
//     dockerd step that touches a git-over-ssh remote.
//   - GIT_ALLOW_PROTOCOL: a NAMED cross-PR hazard, not a general
//     process-hijack one -- a separate, concurrent Step hardens git in
//     this same sandbox by setting this var to restrict which
//     transports git honors; an automation env var or sandbox secret
//     that happened to share the name could silently override or
//     shadow that restriction depending on append order. Reserved here
//     so the two mechanisms can never collide, regardless of which
//     lands first.
//
// gitConfigReservedPrefix ("GIT_CONFIG_") is a PREFIX, not an
// enumerated {GIT_CONFIG_COUNT, GIT_CONFIG_KEY_0, GIT_CONFIG_VALUE_0,
// ...} list, mirroring narviReservedPrefix's/OpenCodeReservedPrefix's
// own "reject the whole mechanism, not today's specific variable names"
// reasoning: git's own env-based config-override mechanism
// (GIT_CONFIG_COUNT declares how many GIT_CONFIG_KEY_<n>/GIT_CONFIG_
// VALUE_<n> pairs follow) can set core.sshCommand, core.fsmonitor, or
// any other git config key to the same arbitrary-execution effect as
// GIT_SSH_COMMAND, one level of indirection removed -- an enumerated
// pair would miss GIT_CONFIG_KEY_1/GIT_CONFIG_VALUE_1 the moment a
// caller declared a second override.
var processHijackReservedNames = []string{
	"PATH", "HOME",
	"LD_PRELOAD", "LD_LIBRARY_PATH",
	"BASH_ENV", "ENV",
	"NODE_OPTIONS",
	"PYTHONSTARTUP",
	"GIT_SSH_COMMAND", "GIT_SSH",
	"GIT_ALLOW_PROTOCOL",
}

const gitConfigReservedPrefix = "GIT_CONFIG_"

// ValidateName reports whether name is an acceptable sandbox_secrets env-
// var name, per §27.1's own fail-closed rule: POSIX env-var shape, plus
// ValidateNotReserved's own "one owning mechanism per env-var name" rule
// (below). Returns nil when name is acceptable. Pure -- no I/O, no
// time.Now(), no randomness (CLAUDE.md §11) -- this only inspects name
// itself; it says nothing about whether name already has a row at some
// OTHER (scope, scopeTargetID) pair (a Postgres UNIQUE-index concern, not
// this function's).
func ValidateName(name string) error {
	if name == "" {
		return ErrNameEmpty
	}
	if len(name) > maxNameLength {
		return fmt.Errorf("%w: %d bytes (max %d)", ErrNameTooLong, len(name), maxNameLength)
	}
	if !posixEnvVarNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrNameShape, name)
	}
	return ValidateNotReserved(name)
}

// ValidateNotReserved reports whether name collides with a namespace or
// exact name another injection mechanism already owns: the reserved
// NARVI_* namespace, the reserved OPENCODE_* namespace (adversarial-review
// CRITICAL fix -- see OpenCodeReservedPrefix's own doc comment), one of
// the names providercredential.EnvVarNames already owns, one of §27.4's
// own §27.3 cloud-identity names (cloudidentity.ReservedEnvVarNames), or
// §27.4's own KUBECONFIG (clusterbinding.ReservedEnvVarNames) -- the "one
// owning mechanism per env-var name" rule §27.1 established for this
// package's own sandbox_secrets rows.
//
// Factored out of ValidateName (which still runs this AND its own POSIX-
// shape check together, unchanged) specifically so automation.
// ValidateEnvVars (§8 item 4's own "automation env vars reach the
// process, not just the prompt") can reuse this EXACT reservation logic
// without also inheriting ValidateName's own stricter uppercase-only shape
// rule -- automations.env_vars has never required that shape (isValidEnvVarName,
// internal/domain/automation/envvar.go, accepts lowercase, matching every
// automation env var saved before this reuse existed), and reservation
// collision-safety does not depend on it: every reserved name/prefix this
// function checks is itself always uppercase, so a lowercase candidate can
// never collide with one regardless (env var names are compared
// case-sensitively, both here and in the process environment they end up
// in). One shared definition of "already owned by another mechanism" for
// both callers, never two independently maintained copies that could
// drift apart.
func ValidateNotReserved(name string) error {
	if strings.HasPrefix(name, narviReservedPrefix) {
		return fmt.Errorf("%w: %q", ErrNameReservedNarviNamespace, name)
	}
	if strings.HasPrefix(name, OpenCodeReservedPrefix) {
		return fmt.Errorf("%w: %q", ErrNameReservedOpenCodeNamespace, name)
	}
	for _, reserved := range providercredential.AllEnvVarNames() {
		if name == reserved {
			return fmt.Errorf("%w: %q", ErrNameReservedProviderCredential, name)
		}
	}
	for _, reserved := range cloudidentity.ReservedEnvVarNames() {
		if name == reserved {
			return fmt.Errorf("%w: %q", ErrNameReservedCloudIdentity, name)
		}
	}
	for _, reserved := range clusterbinding.ReservedEnvVarNames() {
		if name == reserved {
			return fmt.Errorf("%w: %q", ErrNameReservedClusterBinding, name)
		}
	}
	if strings.HasPrefix(name, gitConfigReservedPrefix) {
		return fmt.Errorf("%w: %q", ErrNameReservedProcessHijack, name)
	}
	for _, reserved := range processHijackReservedNames {
		if name == reserved {
			return fmt.Errorf("%w: %q", ErrNameReservedProcessHijack, name)
		}
	}
	return nil
}
