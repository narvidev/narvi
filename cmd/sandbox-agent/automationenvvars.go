// This file (automationenvvars.go) implements §8 item 4's own
// ("automation env vars reach the process, not just the prompt")
// sandbox-agent-side FETCH + injection-env-building of this session's own
// automation env vars, mirroring sandboxsecrets.go's own
// fetchSandboxSecrets/sandboxSecretSpawnEnv split exactly in spirit --
// bounded retry (§27.1's own adversarial-review MEDIUM fix, reused
// unchanged), defense-in-depth re-validation at the injection boundary
// (reused unchanged, sandboxsecret.ValidateNotReserved -- see below for
// why NotReserved rather than the full ValidateName), best-effort/
// never-fatal-to-boot (a fetch failure degrades to "this session's env
// carries no automation env vars", never a boot failure).
//
// # Threaded into the SAME base slice sandbox secrets already build,
// NOT a new opencodeproc.Spawn parameter
//
// Unlike sandboxSecretEnv (its own dedicated opencodeproc.Spawn parameter,
// §27.1's own adversarial-review HIGH fix), automation env vars are
// folded into that EXACT SAME parameter by this file's own caller
// (main.go, run()) -- prepended to the FRONT of sandboxSecretEnv, before
// sandbox_secrets/OPENCODE_CONFIG/cloud-identity/kubeconfig are appended
// on top of them. This is the recorded ordering decision (see
// opencodeproc.Spawn's own doc comment, spawn.go, for the full three-way
// chain and the "why" behind it): automation env vars are the LEAST
// trusted of the three sources threaded into cmd.Env -- "plain config a
// maintainer typed", never RBAC-gated the elevated way
// ActionManageRepoSecrets/ActionManageEnvSecrets/ActionManageGlobalSecrets
// gate sandbox_secrets/provider_credentials -- so a later, more-trusted
// entry for the same name always overrides an earlier, less-trusted one
// (exec.Cmd's own documented Env semantics). No new opencodeproc.Spawn
// parameter is introduced: this Step is a third source on the EXISTING
// path, not a new mechanism (this file's own top doc comment mirrors that
// framing deliberately).
//
// # Not a secret, but re-validated at the injection boundary anyway
//
// automationenvvarsdelivery.go's own doc comment already covers why this
// value is never treated as a secret (no decrypt step, no encryption at
// rest). The re-validation below is NOT about confidentiality -- it is
// about the exact same "one owning mechanism per env-var name" hazard
// sandbox_secrets already re-validates against (fetchSandboxSecrets' own
// doc comment, sandboxsecrets.go): a name reserved by provider-credential
// injection, cloud-identity injection, kubeconfig injection, or the
// process-hijack surface (PATH/HOME/LD_PRELOAD and siblings, internal/
// domain/sandboxsecret/name.go's own processHijackReservedNames) must
// never reach cmd.Env from THIS source either, however the row reached
// the table.
//
// Calls automation.ValidateEnvVarShapeAndReservation, NOT
// sandboxsecret.ValidateName directly -- adversarial-review MEDIUM fix
// (W3): an earlier version of this loop called sandboxsecret.
// ValidateNotReserved alone, re-checking reservation but never shape, so
// a delivered name containing "=", an empty name, or a leading-digit
// name -- none legal in a POSIX environment -- would still have reached
// cmd.Env. ValidateEnvVarShapeAndReservation runs the exact same shape
// check CreateAutomation's own write-time ValidateEnvVars already
// enforces (isValidEnvVarName, lowercase permitted -- automation env var
// names have never been required to be uppercase-only, unlike
// sandboxsecret.ValidateName's own stricter POSIX-uppercase shape rule),
// so re-validating here cannot silently start dropping a lowercase name
// that was valid the moment it was saved, while still closing the shape
// hole. See internal/domain/automation/envvar.go's own top doc comment
// for why this injection-boundary re-check -- not CreateAutomation's own
// write-time one -- is the guarantee that actually holds end to end,
// regardless of write-path drift (a second write path, internal/app/
// seed's own seedAutomation, does not call ValidateEnvVars at all).

package main

import (
	"context"
	"log/slog"
	"sort"

	"github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/credentials"
)

// fetchAutomationEnvVars fetches this session's own automation env vars
// from CP (POST /sessions/{id}/automation-env-vars), retrying a transport
// error or 5xx up to timeouts.AutomationEnvVarFetchMaxAttempts times
// (mirrors fetchSandboxSecrets' own identical bounded-retry posture).
// Every attempt failing (or a terminal 401/403/404/410 on the very first
// one) is logged (Warn, never Error) and reported via fetchOK=false -- the
// caller then simply boots with no automation env vars injected, exactly
// as if this feature did not exist for this session, and MAY choose to
// record that degradation somewhere an agent can see it (run(), main.go,
// folds fetchOK into the AGENTS.md degrade-notice list, mirroring §27.1's
// own "recorded in the boot log and AGENTS.md" requirement).
//
// fetchOK=true with an EMPTY resolved map is the overwhelming common
// case (this session was never created by app/automation's own
// fanout.go at all, or the automation it belongs to simply carries no
// env_vars) and is NOT a degraded outcome -- callers must not conflate
// the two; that is exactly why this returns a separate bool rather than
// overloading a nil map to mean "failed".
//
// Never logs any resolved value, at any point -- only names, for
// observability, matching fetchSandboxSecrets'/fetchProviderCredentials'
// own identical discipline (this file's own top doc comment: not because
// these particular values are confidential, but as the same blanket habit
// every value threaded into a customer's own process environment gets
// regardless).
func fetchAutomationEnvVars(ctx context.Context, cfg boot.Config, timeouts platform.Timeouts) (resolved map[string]string, fetchOK bool) {
	client, err := credentials.NewCPClient(cfg.SessionConfig.ControlPlaneWsUrl, timeouts.AutomationEnvVarFetchTimeout)
	if err != nil {
		slog.Warn("sandbox-agent: build automation-env-vars CP client failed, booting with no automation env vars", "error", err)
		return nil, false
	}

	retryErr := platform.Retry(ctx, timeouts.AutomationEnvVarFetchMaxAttempts, timeouts.AutomationEnvVarFetchRetryBaseDelay, timeouts.AutomationEnvVarFetchRetryMaxDelay, func() error {
		fetchCtx, cancel := context.WithTimeout(ctx, timeouts.AutomationEnvVarFetchTimeout)
		defer cancel()

		r, fetchErr := client.FetchAutomationEnvVars(fetchCtx, cfg.SessionConfig.SessionId, cfg.SessionConfig.SandboxToken, cfg.SessionConfig.Gen)
		if fetchErr != nil {
			return classifyDeliveryFetchError(fetchErr)
		}
		resolved = r
		return nil
	})
	if retryErr != nil {
		slog.Warn("sandbox-agent: fetch automation env vars exhausted every retry attempt, booting with no automation env vars", "error", retryErr)
		return nil, false
	}

	// Defense in depth -- see this file's own top doc comment for the
	// full "why ValidateEnvVarShapeAndReservation, not sandboxsecret.
	// ValidateName" reasoning (W3 fix: shape AND reservation, not
	// reservation alone).
	for name := range resolved {
		if err := automation.ValidateEnvVarShapeAndReservation(name); err != nil {
			slog.Warn("sandbox-agent: dropping delivered automation env var whose name is not injectable", "name", name, "error", err)
			delete(resolved, name)
		}
	}

	if len(resolved) > 0 {
		names := make([]string, 0, len(resolved))
		for name := range resolved {
			names = append(names, name)
		}
		sort.Strings(names)
		slog.Info("sandbox-agent: resolved automation env vars", "names", names)
	}
	return resolved, true
}

// automationEnvVarSpawnEnv maps every entry in envVars onto its own
// already-built "NAME=VALUE" string -- mirrors sandboxSecretSpawnEnv's own
// identical "resolved map -> []string of spawn-ready entries" shape.
// Sorted by name purely for deterministic output (a test/log convenience
// -- exec.Cmd's own Env semantics never depend on slice order for a set of
// UNIQUE keys, which a Go map's own keys always are). A nil/empty map is a
// correct, nil return -- the overwhelming common case: this session
// carries no automation env vars.
func automationEnvVarSpawnEnv(envVars map[string]string) []string {
	if len(envVars) == 0 {
		return nil
	}
	names := make([]string, 0, len(envVars))
	for name := range envVars {
		names = append(names, name)
	}
	sort.Strings(names)

	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+envVars[name])
	}
	return env
}

// automationAndSandboxSecretEnv builds sandboxSecretEnv's own first two
// layers, in the EXACT order run() (main.go) itself assembles them: the
// recorded three-way order (opencodeproc.Spawn's own doc comment,
// spawn.go's "The recorded three-way order, and why") puts automation
// env vars (least-trusted -- "plain config a maintainer typed") first,
// then general sandbox secrets (more-trusted -- "a secret the operator
// configured") layered on top, so a later, more-trusted entry wins on a
// name collision (exec.Cmd's own documented Env semantics). run() calls
// this function DIRECTLY as the one and only place it builds this part
// of sandboxSecretEnv -- this is not a test-facing replica of that
// assembly, it IS the assembly, extracted so a test can drive it without
// also driving every OTHER thing run() does (HTTP servers, supervised
// process spawns, signal handling, ...).
//
// Adversarial-review MEDIUM fix (W1): before this extraction, run()'s
// own two-line assembly (automationEnvVarSpawnEnv, then a separate
// append of sandboxSecretSpawnEnv onto the front) was never exercised by
// any test -- every existing collision-order test
// (opencodeproc.TestSpawn_SandboxSecretEnvWinsOverAutomationEnvVar)
// instead hand-built an already-in-final-order []string literal and
// handed it straight to opencodeproc.Spawn, which proved Spawn's OWN
// append order but said nothing about whether run() actually PRODUCES
// that order. Deleting the automation-env-var injection line in main.go,
// or swapping this function's own two statements (so sandbox secrets
// would end up FIRST, automation env vars LAST -- inverting who wins),
// now fails automationenvvars_test.go's own
// TestAutomationAndSandboxSecretEnv_SandboxSecretWinsOverAutomationEnvVar
// directly, because that test calls this exact function.
func automationAndSandboxSecretEnv(resolvedAutomationEnvVars, resolvedSandboxSecrets map[string]string) []string {
	env := automationEnvVarSpawnEnv(resolvedAutomationEnvVars)
	return append(env, sandboxSecretSpawnEnv(resolvedSandboxSecrets)...)
}

// automationEnvVarDegradeNotes reports the AGENTS.md boot-degrade note
// (if any) to record for this session's own automation-env-var fetch
// outcome -- always nil, deliberately, unlike sandboxSecretEnv's/
// openCodeConfigEnv's own sibling degrade notes (main.go's "sandbox
// secrets"/"opencode config" blocks, which DO warn on a failed fetch).
//
// Adversarial-review LOW fix (W6): a prior version of run() appended a
// warning UNCONDITIONALLY whenever automationEnvVarsFetchOK was false --
// which fires on EVERY session during a control-plane outage, including
// the overwhelming common case of an ordinary, non-automation session,
// for which the warning is actively wrong (it never had any automation
// env vars to lose in the first place). Fixing that by trying to detect
// "is this session actually an automation session" was considered and
// rejected: CP's own delivery response (automationenvvarsdelivery.go's
// own doc comment, outcome 7) is `{}` for BOTH "not an automation
// session" and "automation session with zero env_vars" on SUCCESS, and
// carries no signal AT ALL on FAILURE -- SessionConfig (contracts/
// session-config/v1/session-config.schema.json) has no automation-
// association field to consult either, so any such detection would be a
// heuristic guess, not a fact.
//
// The real fix is simpler and does not need that signal at all: §8.4's
// own "keep the preamble" decision -- the prompt-preamble delivery and
// this boot-time env-var injection are deliberately NOT exclusive
// alternatives, because an agent that must KNOW a feature-flag name and
// a shell that must RESOLVE it are different needs -- means
// buildRunPrompt (internal/app/automation/settings.go)
// ALREADY writes every one of this automation's own configured
// NAME=value pairs into the dispatched turn's own prompt text,
// UNCONDITIONALLY, at run-dispatch time -- entirely independent of
// whether THIS boot-time fetch (which only controls $VAR/os.Getenv
// visibility) ever succeeds. An agent that reads its own prompt already
// knows every value this fetch would have injected, so a boot-time fetch
// failure here degrades silently but never SILENTLY -- unlike a missing
// sandbox secret (which CAN break a repo's own setup.sh/services.yml/
// opencode auth with no other channel telling the agent anything went
// wrong), there is no second, boot-time-only piece of information the
// agent would otherwise be missing. A bool parameter (currently unused,
// so named _ to satisfy this codebase's own revive/unused-parameter
// lint rule) is kept in the signature -- rather than dropping it down to
// an argument-less func() []string -- so a future, genuinely-informed
// policy can be reinstated here without changing run()'s own call site
// again.
func automationEnvVarDegradeNotes(_ bool) []string {
	return nil
}
