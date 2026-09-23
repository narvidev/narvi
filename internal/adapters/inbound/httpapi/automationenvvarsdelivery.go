// This file (automationenvvarsdelivery.go) implements §8 item 4's own
// "automation env vars reach the process, not just the prompt" injection
// mechanism's CP-side DELIVERY endpoint for sandbox-agent: POST
// /sessions/{sessionID}/automation-env-vars (note: no /api prefix, exactly
// like provider-credentials/sandbox-secrets -- a sandbox-to-CP endpoint,
// not a browser-facing REST route, §5.2).
//
// Mirrors sandboxsecretsdelivery.go's own handshake VERBATIM (itself
// mirroring providercredentialsdelivery.go/scmcredentials.go): sandbox-
// bearer-token-authenticated (mounted OUTSIDE auth.Middleware entirely,
// alongside the other sandbox-facing routes, controlplane/serve.go), the
// SAME dead-sandbox check (sandbox.IsDeadSandboxStatus, 410 -- checked
// immediately after the sandbox row lookup, before the gen/token
// comparisons) and X-Sandbox-Gen fencing (403 on a missing/mismatched
// header) at the SAME points in the handshake, and the SAME bearer-token
// verification (verifySandboxBearerToken, constant-time hash compare, no
// nil-token_hash bypass). Outcome table below mirrors
// sandboxsecretsdelivery.go's own numbered list exactly (no request body/
// host to check here either):
//
//  1. sessionID does not parse as a UUID -> 404.
//  2. Authorization: Bearer <token> missing/malformed -> 401 (checked
//     before the sandbox row lookup).
//  3. No sandbox row exists for sessionID -> 404.
//  4. sandbox.IsDeadSandboxStatus(sandboxRow.Status) -> 410.
//  5. X-Sandbox-Gen missing/malformed/mismatched -> 403.
//  6. The presented token fails verifySandboxBearerToken -> 401.
//  7. Otherwise -> 200 with a plain name->value map -- {} when sessionID
//     is not referenced by any automation_runs row at all (the
//     overwhelming common case: an ordinary web/Slack/Linear/GitHub-
//     created session, GetAutomationEnvVarsForSession's own pgx.ErrNoRows),
//     or when the automation that row belongs to currently carries zero
//     env_vars.
//
// # Not a secret-carrying response
//
// Unlike sandboxsecretsdelivery.go, there is no decrypt step and no
// per-row decrypt-failure path here: automations.env_vars is explicitly
// NOT a secret (migrations/000055_automations_triggers_and_extras.up.sql's
// own trailing comment, internal/domain/automation/envvar.go's own top
// doc comment) -- it is already plaintext at rest, the same plaintext
// buildRunPrompt (internal/app/automation/settings.go) already puts in
// this run's own dispatched turn prompt. Threading it into cmd.Env (this
// Step's own addition) does not change that classification, so it is
// still never routed anywhere sandbox_secrets/provider credentials get
// special handling for (encryption at rest, redaction from logs at the
// storage layer) -- it inherits only the ONE discipline every value
// threaded into a customer's own process environment gets regardless of
// confidentiality: never logged here, matching every sibling delivery
// handler's blanket habit.
//
// # Resolution: current value, not a frozen snapshot
//
// GetAutomationEnvVarsForSession (queries/automationruns.sql) reads the
// automation's CURRENT env_vars column at THIS spawn/respawn moment, not
// a copy frozen at the run's own creation time the way buildRunPrompt's
// own preamble text is -- this mirrors provider credentials/sandbox
// secrets, which are ALSO re-resolved fresh at every spawn, never frozen
// onto the session row. An automation edited between a session's original
// spawn and a later resume/respawn can therefore genuinely diverge
// between the turn's own already-dispatched prompt text and a later
// resume's own process environment for that session -- an accepted,
// narrow inconsistency shared with every other CP-resolved-at-spawn value
// this binary already injects, not a new one this Step introduces.

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/platform"
)

// automationEnvVarsResponse is this Step's own invented, documented
// response shape -- mirrors sandboxSecretsResponse's own "small, explicit
// map, since a value here is always a bare string" precedent
// (sandboxsecretsdelivery.go).
type automationEnvVarsResponse struct {
	EnvVars map[string]string `json:"envVars"`
}

// AutomationEnvVarsDelivery backs POST
// /sessions/{sessionID}/automation-env-vars -- see this file's own top
// doc comment for the full outcome table and resolution reasoning.
func AutomationEnvVarsDelivery(
	sandboxes *postgres.SandboxStore,
	automationRuns *postgres.AutomationRunStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		var sessionID pgtype.UUID
		if err := sessionID.Scan(chi.URLParam(r, "sessionID")); err != nil {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		ctx = platform.WithSessionID(ctx, sessionID.String())
		logger := platform.Logger(ctx)

		token, ok := bearerTokenFromHeader(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or malformed authorization header")
			return
		}

		sandboxRow, err := sandboxes.Get(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: automation-env-vars: get sandbox failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Dead-sandbox check FIRST, before the gen/token comparisons below
		// -- same ordering every sibling delivery handler uses.
		if sandbox.IsDeadSandboxStatus(sandbox.State(sandboxRow.Status)) {
			writeError(w, http.StatusGone, "session stopped")
			return
		}

		presentedGen, genErr := strconv.Atoi(r.Header.Get("X-Sandbox-Gen"))
		if genErr != nil || presentedGen != int(sandboxRow.Gen) {
			logger.Warn("httpapi: automation-env-vars: rejecting: gen mismatch",
				"presented_gen_header", r.Header.Get("X-Sandbox-Gen"), "sandbox_gen", sandboxRow.Gen)
			writeError(w, http.StatusForbidden, "no usable automation env vars for this session")
			return
		}

		if !verifySandboxBearerToken(token, sandboxRow.TokenHash) {
			writeError(w, http.StatusUnauthorized, "invalid sandbox token")
			return
		}

		rawEnvVars, err := automationRuns.EnvVarsForSession(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The overwhelming common case: this session was never
				// created by app/automation's own fanout.go at all.
				writeJSON(w, http.StatusOK, automationEnvVarsResponse{EnvVars: map[string]string{}})
				return
			}
			logger.Error("httpapi: automation-env-vars: get automation env vars failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		var wire []restdtos.AutomationEnvVarElem
		if len(rawEnvVars) > 0 {
			// Reuses restdtos.AutomationEnvVarElem -- the SAME wire shape
			// httpapi/automations.go's own automationToDTO already decodes
			// this exact column into, never a second, independently
			// maintained {name,value} struct that could drift from it.
			if err := json.Unmarshal(rawEnvVars, &wire); err != nil {
				logger.Error("httpapi: automation-env-vars: decode automation env vars failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}

		envVars := make(map[string]string, len(wire))
		for _, v := range wire {
			envVars[v.Name] = v.Value
		}

		writeJSON(w, http.StatusOK, automationEnvVarsResponse{EnvVars: envVars})
	}
}
