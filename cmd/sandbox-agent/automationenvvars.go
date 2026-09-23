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
// injection, cloud-identity injection, or kubeconfig injection must never
// reach cmd.Env from THIS source either, however the row reached the
// table (internal/domain/automation.ValidateEnvVars already enforces this
// at CreateAutomation's own write path, but re-checking here makes the
// shadowing unrepresentable regardless of write-path drift, exactly like
// fetchSandboxSecrets' own identical reasoning). Deliberately calls
// sandboxsecret.ValidateNotReserved, NOT the stricter ValidateName --
// automation env var names have never been required to be uppercase-only
// (internal/domain/automation.ValidateEnvVars' own isValidEnvVarName
// accepts lowercase), and re-validating here must not silently start
// dropping a lowercase name that was valid the moment it was saved.

package main

import (
	"context"
	"log/slog"
	"sort"

	"github.com/narvidev/narvi/internal/domain/sandboxsecret"
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
	// full "why ValidateNotReserved, not ValidateName" reasoning.
	for name := range resolved {
		if err := sandboxsecret.ValidateNotReserved(name); err != nil {
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
