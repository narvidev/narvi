package opencodeproc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// maxHealthResponseBodySize bounds how much of /global/health's response
// body discoverVersion ever reads — a tiny, fixed-shape JSON document, so
// this is a generous ceiling, not a tuned value.
const maxHealthResponseBodySize = 1 << 16 // 64 KiB

// Result is what Spawn returns once `opencode serve` is confirmed healthy.
type Result struct {
	// BaseURL is "http://127.0.0.1:<port>" for the ephemeral port the
	// kernel assigned — pass this directly to opencode.New
	// (internal/adapters/outbound/opencode).
	BaseURL string

	// Version is OpenCode's own reported version, best-effort ("" when it
	// could not be determined — see discoverVersion), for the boot
	// fingerprint (§7).
	Version string

	// Process is the supervised *supervisor.Process. Spawn does not stop
	// it itself — the caller folds it into whatever else it is already
	// tracking (e.g. sandbox-agent's own Supervisor.StopAll already stops
	// every process it spawned, this one included, at shutdown).
	Process *supervisor.Process
}

// Spawn starts `opencode serve --port <ephemeral> --hostname 127.0.0.1`
// via sup.Spawn (never a bare exec.Command — see
// internal/sandboxagent/supervisor/doc.go) with workDir as the process's
// own OS working directory, waits for it to report healthy, and returns
// its base URL plus a best-effort version string. See doc.go for the full
// rationale of each step.
//
// providerCredentialEnv ("provider credential injection",
// §25.1/§25.3) is zero or more already-built "NAME=VALUE" entries --
// mapped from a resolved provider credential onto its own OpenCode env-var
// name(s), via internal/domain/providercredential.EnvVarNames -- appended
// to the base env AFTER supervisor.EnvWithout, so an explicit, resolved
// credential always wins over anything already ambient in sandbox-agent's
// own OS environment (a later entry for the SAME key overrides an earlier
// one in exec.Cmd's own documented Env semantics). Nil/empty is the
// overwhelming common case (no provider credential configured at any scope
// for this session) and changes NOTHING about this function's own
// pre-existing behavior -- every existing call site keeps compiling and
// behaving identically by simply passing nil.
//
// sandboxSecretEnv ("sandbox secrets & opencode config", §27.1/
// §27.2, adversarial-review HIGH fix) is zero or more already-built
// "NAME=VALUE" entries covering EVERYTHING this Step injects: a session's
// own resolved general sandbox_secrets rows, plus (when an environment
// OpenCode config document exists) a single OPENCODE_CONFIG entry pointing
// at the file cmd/sandbox-agent's own applyOpenCodeConfig already wrote to
// disk, plus (§8 item 4, "automation env vars reach the process, not
// just the prompt") this session's own resolved automation env vars,
// PREPENDED to the front of this SAME slice by the caller (main.go, run())
// before the sandbox_secrets/OPENCODE_CONFIG/cloud-identity/kubeconfig
// entries above are appended on top of them -- a third source folded into
// this EXISTING parameter, not a new one. Appended BEFORE
// providerCredentialEnv (§27.1's own explicit ordering: "appended before
// providerCredentialEnv, so the ordering question is moot anyway given the
// disjoint-name rule" -- internal/domain/sandboxsecret.ValidateName
// rejects every name providercredential.AllEnvVarNames or this package's
// own OPENCODE_* reservation already owns, so the two slices can never
// actually collide; the ordering is honored anyway, matching the spec
// exactly).
//
// # The recorded three-way order, and why
//
// Least-trusted first, most-trusted last (a later entry for the same name
// always wins, exec.Cmd's own documented Env semantics):
//
//  1. automation env vars (§8 item 4) -- "plain config a maintainer
//     typed" (internal/domain/automation's own doc.go), never RBAC-gated
//     the elevated way #2/#3 below are; automations.env_vars is explicitly
//     documented non-secret.
//  2. sandbox_secrets/OPENCODE_CONFIG/cloud-identity/kubeconfig (this
//     parameter's own pre-existing contents, §27.1) -- "a secret the
//     operator configured" (ActionManageRepoSecrets/ActionManageEnv
//     Secrets/ActionManageGlobalSecrets), encrypted at rest.
//  3. providerCredentialEnv (§25.1/§25.3, below) -- the single credential
//     `opencode serve` itself needs to authenticate at all; the most
//     operationally load-bearing of the three, so it wins over either of
//     the other two on a name collision.
//
// A collision between #1 and #3 is NOT structurally impossible -- an
// earlier version of this comment claimed it was, on the strength of
// internal/domain/automation.ValidateEnvVars already rejecting, "at
// CreateAutomation's own write path", any name providercredential.
// AllEnvVarNames/cloudidentity.ReservedEnvVarNames/clusterbinding.
// ReservedEnvVarNames/the NARVI_*/OPENCODE_* namespaces already own.
// Adversarial review (W7) found CreateAutomation is not the only write
// path automations.env_vars has: internal/app/seed's own seedAutomation
// (internal/app/seed/automations.go) writes that column directly, from a
// `control-plane seed -manifest <path>` run, without ever calling
// ValidateEnvVars (internal/domain/seedmanifest.ValidateManifest does
// call it, but nothing on the real seed-apply path ever calls
// ValidateManifest) -- so a write-time-only guarantee does not actually
// hold end to end. What DOES hold, regardless of write-path drift, is
// cmd/sandbox-agent/automationenvvars.go's own injection-boundary
// re-validation (fetchAutomationEnvVars, via internal/domain/automation.
// ValidateEnvVarShapeAndReservation, which reuses this exact
// sandboxsecret.ValidateNotReserved check): it drops any reserved name
// from the resolved map BEFORE this parameter is ever built, for every
// automation env var regardless of which write path produced it -- see
// internal/domain/automation/envvar.go's own top doc comment for the
// full "not the only fence" writeup this corrects. A collision between
// #1 and #2 IS reachable (sandbox_secrets has
// no equivalent reservation against an ARBITRARY automation-chosen name,
// only against the names #3 and the cloud-identity/kubeconfig mechanisms
// already own) -- #2 wins, matching "an operator-managed secret outranks
// plain config a maintainer typed".
//
// This parameter is this Step's OWN fix for a HIGH-severity finding: the
// original implementation instead os.Setenv'd every resolved secret onto
// sandbox-agent's OWN process environment, ahead of every EnvWithout call
// in this binary -- which meant a secret literally named e.g. "PATH" or
// "HOME" corrupted the SUPERVISOR's own process (PATH poisons every
// bare-name exec.Command LookPath -- including THIS function's own "opencode"
// lookup below -- since Go's exec.LookPath resolves against the CALLING
// process's os.Getenv("PATH") at exec.Command() call time, never from
// Spec.Env; HOME poisons os.UserHomeDir(), silently redirecting where the
// global OpenCode config document gets written), turning what §10-P2 and
// this feature's own "warn and continue, never a boot failure" posture
// require to be a harmless per-secret misconfiguration into a hard SPAWN
// failure for `opencode serve` (and, via the ambient PATH `git clone` also
// inherits, boot itself). Threading the resolved env explicitly here --
// exactly like providerCredentialEnv already does -- makes that entire
// class of failure structurally unrepresentable: sandbox-agent's own
// process environment (and therefore its own PATH-based binary lookups,
// its own os.UserHomeDir(), and every OTHER already-running piece of this
// binary) is never touched by ANY resolved secret, no matter what name a
// customer chose for it.
// runtimeCredential (TECHNICAL_PLAN.md §30.5, "OS-level isolation between
// sandbox-agent and the agent runtime") is threaded straight into
// supervisor.Spec's own Credential field for the ONE process this
// function spawns -- opencode serve, and every shell/tool it forks on the
// agent's behalf, are the exact "agent runtime" §30.5 names as running
// same-UID as sandbox-agent today, which makes the env-strip above purely
// cosmetic (sandbox-agent's own /proc/<pid>/environ, and the on-disk
// credential cache, are one read away from a same-UID process) and makes
// the on-disk credential cache's own 0600 a placement convention, not a
// kernel-enforced boundary. nil (every existing call site/test's own
// value, unchanged) preserves this function's exact pre-existing
// behavior -- see supervisor.Spec.Credential's own doc comment for
// why nil is safe everywhere else this package's sibling
// gitclone/docker/hooks/push spawns run (they legitimately need
// sandbox-agent's own identity) and why only THIS spawn ever receives a
// non-nil value in production (cmd/sandbox-agent/main.go).
func Spawn(
	ctx context.Context,
	sup *supervisor.Supervisor,
	workDir string,
	providerCredentialEnv []string,
	sandboxSecretEnv []string,
	runtimeCredential *syscall.Credential,
	readinessTimeout, readinessPollInterval time.Duration,
) (Result, error) {
	port, err := freePort()
	if err != nil {
		return Result{}, fmt.Errorf("opencodeproc: allocate ephemeral port: %w", err)
	}

	env := supervisor.EnvWithout(boot.SessionConfigEnvVar)
	env = append(env, sandboxSecretEnv...)
	env = append(env, providerCredentialEnv...)

	proc, err := sup.Spawn(supervisor.Spec{
		Path: "opencode",
		Args: []string{"serve", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1"},
		Dir:  workDir,
		// opencode serve is a separate coding-agent HTTP server with no
		// legitimate use for NARVI_SESSION_CONFIG (the sandbox's own
		// plaintext bearer token, among other things) -- excluding it here
		// closes a real env leak while leaving everything else opencode
		// might legitimately need (PATH, HOME, ...) untouched. Any
		// resolved provider credential env vars (providerCredentialEnv,
		// this func's own doc comment) are layered on top of that same
		// filtered base.
		Env: env,
		// Credential (§30.5, this func's own doc comment above): the
		// kernel-enforced half of the isolation, layered on top of the
		// env-strip above rather than replacing it -- both close
		// different halves of the same leak (this process's own
		// environment vs. sandbox-agent's).
		Credential: runtimeCredential,
	})
	if err != nil {
		return Result{}, fmt.Errorf("opencodeproc: spawn opencode serve: %w", err)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	if err := waitHealthy(ctx, proc, baseURL, readinessTimeout, readinessPollInterval); err != nil {
		return Result{}, err
	}

	return Result{
		BaseURL: baseURL,
		Version: discoverVersion(ctx, baseURL, readinessTimeout),
		Process: proc,
	}, nil
}

// freePort binds to 127.0.0.1:0, reads back the ephemeral port the kernel
// assigned, then immediately closes the listener — the same technique
// internal/sandboxagent/services' own tests already use (freePort there),
// promoted here to real, non-test code since sandbox-agent genuinely needs
// an unused port at boot time. There is an inherent, accepted TOCTOU
// window between this Close and opencode serve rebinding the same port;
// standard practice for this kind of ephemeral allocation.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	port, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("opencodeproc: unexpected listener address type %T", l.Addr())
	}
	return port.Port, nil
}

// waitHealthy polls GET baseURL+"/api/health" until it succeeds, proc
// exits first (a crash before ever becoming healthy — reported as a fail-
// fast, bounded error, never a hang), or timeout expires. Mirrors
// internal/sandboxagent/services.waitReady's own poll-vs-Process.Exited()
// shape exactly, minus that package's multi-service fan-out (there is
// exactly one process here).
func waitHealthy(
	ctx context.Context,
	proc *supervisor.Process,
	baseURL string,
	timeout, pollInterval time.Duration,
) error {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Each individual health-check attempt is bounded well below the
	// OVERALL readiness deadline (readyCtx), never by it directly. A real,
	// observed failure mode this fixes: opencode serve's own listener can
	// start accepting connections promptly while still taking several
	// seconds to actually service the first few requests (its own
	// startup work isn't done yet) -- healthCheck previously ran with
	// readyCtx itself as its request context, so ONE such slow-to-respond
	// attempt could silently consume most or all of the remaining budget
	// by itself, leaving only a handful of attempts ever made before the
	// overall timeout fired, even though the server would have become
	// healthy well within that same window if polling had continued.
	// Deriving from readyCtx (not ctx) keeps it correctly bounded by
	// whatever of the overall deadline remains, never longer.
	attemptTimeout := timeout / 10

	for {
		if result, exited := proc.Exited(); exited {
			return fmt.Errorf("opencodeproc: opencode serve exited before becoming healthy: %s", describeExit(result))
		}

		attemptCtx, cancelAttempt := context.WithTimeout(readyCtx, attemptTimeout)
		healthy := healthCheck(attemptCtx, baseURL)
		cancelAttempt()
		if healthy {
			return nil
		}

		select {
		case <-readyCtx.Done():
			return fmt.Errorf("opencodeproc: opencode serve did not become healthy within %s", timeout)
		case <-ticker.C:
		}
	}
}

// describeExit turns a supervisor.ExitResult (necessarily an unexpected
// exit, since waitHealthy only calls this when the process ended before
// ever becoming healthy) into a short, descriptive string.
func describeExit(result supervisor.ExitResult) string {
	if result.Err != nil {
		return result.Err.Error()
	}
	return fmt.Sprintf("exit code %d", result.ExitCode)
}

// healthCheck reports whether a GET to baseURL+"/api/health" returned a
// 2xx status -- this Step's own live research against the real OpenCode
// 1.17.15 binary found this endpoint suitable for readiness polling once
// spawned. It deliberately does NOT set an http.Client-level
// timeout: ctx (already bounded by waitHealthy's own overall timeout) is
// sufficient bounding for each individual attempt, matching
// internal/sandboxagent/services' own healthReady precedent exactly.
func healthCheck(ctx context.Context, baseURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
}

// healthResponse is /global/health's own response shape — verified live
// against the real, installed OpenCode 1.17.15 binary during this Step's
// own research pass: {"healthy":true,"version":"1.17.15"}. Distinct from
// /api/health (used by healthCheck above), which reports only
// {"healthy":true} with no version field.
type healthResponse struct {
	Version string `json:"version"`
}

// discoverVersion makes ONE best-effort GET /global/health call against
// the already-running server Spawn just started, rather than shelling out
// to `opencode --version` a second time — the server it just started
// already reports its own version, so a second process invocation would
// be pure overhead for information already at hand. Returns "" on any
// failure whatsoever — version discovery is
// best-effort, exactly like internal/sandboxagent/boot.DiscoverRepoSHAs'
// own "omission is a valid outcome, never an error" contract; sandbox-
// agent's own boot must never block on it. timeout bounds this single call
// (reusing the same readiness timeout Spawn was already given, rather than
// inventing a dedicated new platform.Timeouts field for one best-effort
// HTTP call).
func discoverVersion(ctx context.Context, baseURL string, timeout time.Duration) string {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+"/global/health", nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return ""
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthResponseBodySize))
	if err != nil {
		return ""
	}
	var parsed healthResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	return parsed.Version
}
