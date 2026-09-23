package automation

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
)

// unmarshalPathScope decodes automations.sandbox_path_scope -- a plain
// JSONB array of glob-pattern strings, the SAME shape environments.
// path_scope already uses (migrations/000021_environments.up.sql). Only a
// decoder, never an encoder: this package (fanout.go's own
// applySandboxSettings, below) only ever READS this column -- WRITING it
// happens once, at automation-creation time, in internal/adapters/inbound/
// httpapi's own automations.go, which builds its own path-scope JSON
// directly (it cannot import this package's own encoder due to the import-
// cycle constraint this package's own triggerpump.go doc comment already
// explains for cronTriggerConfigJSON).
func unmarshalPathScope(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var patterns []string
	if err := json.Unmarshal(raw, &patterns); err != nil {
		return nil, fmt.Errorf("automation: unmarshal sandbox path scope: %w", err)
	}
	return patterns, nil
}

// envVarJSON is the on-wire shape automations.env_vars is persisted as --
// mirrors targetJSON's own "small, unexported wire struct, never the
// domain type itself" precedent (target.go).
type envVarJSON struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// unmarshalEnvVars decodes an automations.env_vars JSONB column back into
// []domainautomation.EnvVar. Only a decoder, never an encoder -- see
// unmarshalPathScope's own identical doc comment immediately above for
// why (WRITING env_vars happens once, at automation-creation time, in
// httpapi's own automations.go).
func unmarshalEnvVars(raw []byte) ([]domainautomation.EnvVar, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var wire []envVarJSON
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("automation: unmarshal env vars: %w", err)
	}
	vars := make([]domainautomation.EnvVar, len(wire))
	for i, w := range wire {
		vars[i] = domainautomation.EnvVar{Name: w.Name, Value: w.Value}
	}
	return vars, nil
}

// applySandboxSettings threads automationRow's own persisted sandbox_path_
// scope/sandbox_mock_configured/sandbox_contracts_path columns onto req's
// PathScope/MockConfig fields -- the SAME two request fields an ordinary
// scoped web session already sets (httpapi.CreateSession), so this run's
// own CreateSessionOnTx call (fanout.go) creates an environments row and
// applies sparse-checkout/mock-config exactly like it would for any other
// caller. A decode failure on the automation's own persisted columns is
// logged and otherwise ignored (req is left unscoped) -- malformed
// settings on the AUTOMATION row must never block this run's own session
// from being created.
func applySandboxSettings(logger *slog.Logger, req *restdtos.CreateSessionRequest, automationRow sqlcgen.Automation) {
	pathScope, err := unmarshalPathScope(automationRow.SandboxPathScope)
	if err != nil {
		logger.Error("automation: decode automation sandbox path scope failed; run will be unscoped", "error", err, "automation_id", automationRow.ID.String())
		pathScope = nil
	}

	settings := domainautomation.SandboxSettings{
		PathScope:      pathScope,
		MockConfigured: automationRow.SandboxMockConfigured,
	}
	if automationRow.SandboxContractsPath != nil {
		settings.ContractsPath = *automationRow.SandboxContractsPath
	}

	if domainautomation.IsUnscoped(settings) {
		return
	}

	if len(settings.PathScope) > 0 {
		ps := restdtos.CreateSessionRequestPathScope(settings.PathScope)
		req.PathScope = &ps
	}
	if settings.MockConfigured {
		mc := restdtos.CreateSessionRequestMockConfig{}
		if settings.ContractsPath != "" {
			cp := settings.ContractsPath
			ccp := restdtos.CreateSessionRequestMockConfigContractsPath(&cp)
			mc.ContractsPath = ccp
		}
		req.MockConfig = &mc
	}
}

// envVarPreamblePrefix opens the plain-text block buildRunPrompt prepends
// to an automation's own configured prompt when it carries env_vars (§8.4:
// "per-automation env vars"). Exported as a named constant (not inlined at
// its own single call site) so a test can assert against it directly
// without duplicating the literal.
const envVarPreamblePrefix = "Environment variables for this automation run:"

// buildRunPrompt renders the turn prompt actually dispatched for one run:
// automationRow's own configured Prompt, prefixed with a short, clearly
// labeled "Environment variables for this automation run" block when the
// automation carries any env_vars.
//
// This is DELIBERATELY NOT the only place env_vars reach this run:
// automations.env_vars is ALSO threaded into the sandboxed agent
// PROCESS's own OS environment (cmd.Env) now, via
// cmd/sandbox-agent's own fetchAutomationEnvVars/automationEnvVarSpawnEnv
// (automationenvvars.go), folded into the SAME opencodeproc.Spawn
// sandboxSecretEnv parameter provider credentials (§25.1/§25.3) and
// sandbox secrets (§27.1) already use -- see that parameter's own doc
// comment (spawn.go) for the full three-way resolution order and why.
// The two are deliberately NOT exclusive: an agent that must KNOW a
// flag's name (this function's own preamble text) and a shell that must
// RESOLVE it ($VAR/os.Getenv, the cmd.Env path) are different needs, and
// this function is kept exactly as-is for the former.
func buildRunPrompt(logger *slog.Logger, automationRow sqlcgen.Automation) *string {
	prompt := ""
	if automationRow.Prompt != nil {
		prompt = *automationRow.Prompt
	}

	vars, err := unmarshalEnvVars(automationRow.EnvVars)
	if err != nil {
		logger.Error("automation: decode automation env vars failed; run prompt will not carry them", "error", err, "automation_id", automationRow.ID.String())
		vars = nil
	}
	if len(vars) == 0 {
		return &prompt
	}

	var b strings.Builder
	b.WriteString(envVarPreamblePrefix)
	b.WriteString("\n")
	for _, v := range vars {
		fmt.Fprintf(&b, "%s=%s\n", v.Name, v.Value)
	}
	b.WriteString("\n")
	b.WriteString(prompt)
	rendered := b.String()
	return &rendered
}
