// This file (automationenvvars.go) implements §8 item 4's own
// ("automation env vars reach the process, not just the prompt") sandbox-
// agent-side client half of CP's POST /sessions/{id}/automation-env-vars
// delivery endpoint (internal/adapters/inbound/httpapi/
// automationenvvarsdelivery.go) -- mirrors FetchSandboxSecrets' own shape
// exactly (sandboxsecrets.go), simplified identically to a bare
// name->value map (an automation env var, like a sandbox secret, is
// always a plain string).

package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// maxAutomationEnvVarsResponseSize bounds how much of a CP automation-
// env-vars response body is ever read -- mirrors
// maxSandboxSecretsResponseSize's own identical reasoning: a generous,
// not-tuned ceiling, since http.Client.Timeout already bounds wall-clock
// time.
const maxAutomationEnvVarsResponseSize = 1 << 20 // 1 MiB

// AutomationEnvVarsFetcher is the minimal interface spawn-time code needs
// from a CP client -- CPClient satisfies it, and a test can supply a small
// fake with no real HTTP round trip. Mirrors SandboxSecretsFetcher's own
// identical "narrow interface alongside the concrete struct" shape.
type AutomationEnvVarsFetcher interface {
	FetchAutomationEnvVars(ctx context.Context, sessionID, sandboxToken string, gen int) (map[string]string, error)
}

// automationEnvVarsResponse mirrors internal/adapters/inbound/httpapi's
// own (unexported) automationEnvVarsResponse -- a plain map from env var
// name to its resolved value.
type automationEnvVarsResponse struct {
	EnvVars map[string]string `json:"envVars"`
}

// FetchAutomationEnvVars POSTs (no body) to
// <baseURL>/sessions/<sessionID>/automation-env-vars with
// "Authorization: Bearer <sandboxToken>" and an "X-Sandbox-Gen: <gen>"
// header -- the SAME two headers Fetch/FetchProviderCredentials/
// FetchSandboxSecrets already send -- and expects back
// {"envVars": {"<name>": "<value>", ...}} on a 2xx response. This session
// belonging to no automation run at all (the overwhelming common case) is
// simply an EMPTY map, never an error of its own.
//
// This METHOD itself makes exactly ONE HTTP attempt and applies no retry
// of its own -- any non-2xx response is returned as a *DeliveryStatusError
// (deliverystatus.go), any transport/decode failure as a plain wrapped
// error -- mirrors FetchSandboxSecrets' own identical split between "this
// method makes one attempt" and "the caller wraps repeated calls in
// platform.Retry" (cmd/sandbox-agent's own fetchAutomationEnvVars).
//
// The raw response body is deliberately never embedded in the returned
// error -- mirrors Fetch/FetchProviderCredentials/FetchSandboxSecrets' own
// identical rationale.
func (c CPClient) FetchAutomationEnvVars(ctx context.Context, sessionID, sandboxToken string, gen int) (map[string]string, error) {
	path := fmt.Sprintf("%s/sessions/%s/automation-env-vars", c.baseURL, url.PathEscape(sessionID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: build automation-env-vars request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+sandboxToken)
	req.Header.Set("X-Sandbox-Gen", strconv.Itoa(gen))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("credentials: automation-env-vars request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAutomationEnvVarsResponseSize))
	if err != nil {
		return nil, fmt.Errorf("credentials: read automation-env-vars response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// Deliberately does NOT include body -- see this func's own doc
		// comment above. A typed *DeliveryStatusError (not a plain
		// fmt.Errorf) so the caller's own retry wrapper can classify this
		// by StatusCode alone -- see this func's own doc comment.
		return nil, &DeliveryStatusError{Endpoint: "automation-env-vars", StatusCode: resp.StatusCode}
	}

	var parsed automationEnvVarsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("credentials: decode automation-env-vars response: %w", err)
	}
	return parsed.EnvVars, nil
}
