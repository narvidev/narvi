// This file (automations.go) implements §8.4's own ("automations:
// triggers & extras", §8.4) REST CRUD surface over automations
// (migrations/000051_automations.up.sql, extended by migrations/
// 000055_automations_triggers_and_extras.up.sql) -- §3.5 ("automations:
// engine") shipped the fan-out/reconcile/sweep engine ENGINE-ONLY, with no
// HTTP surface at all (verified directly: no automations.go existed in
// this package before this Step) -- invocationenqueue.go's own doc comment
// already anticipated this: "§8.4's own future trigger evaluator is
// expected to call [CreateInvocation] unchanged once it exists."
//
// Seven routes, all mounted behind auth.Middleware (cmd/control-plane/
// main.go) like every other browser-facing REST route in this package:
//
//   - POST   /api/automations                 CreateAutomation
//   - GET    /api/automations                 ListAutomations (§8.4's own
//     "creator/status filters", ?createdBy=me|<uuid>&status=active|paused)
//   - GET    /api/automations/{automationID}   GetAutomation
//   - POST   /api/automations/{automationID}/pause   PauseAutomation
//   - POST   /api/automations/{automationID}/resume  ResumeAutomation
//   - POST   /api/automations/{automationID}/webhook-token    RotateAutomationWebhookToken
//   - DELETE /api/automations/{automationID}/webhook-token    RevokeAutomationWebhookToken
//
// Create/Pause/Resume/RotateAutomationWebhookToken/
// RevokeAutomationWebhookToken are gated by authz.ActionManageAutomations
// (admin/maintainer only, internal/domain/authz/authorize.go's own
// already-reserved row) -- Get/List are NOT further gated beyond "must be
// logged in": mockups.html's own Automations view ("My automations ▾ /
// All statuses ▾" toolbar) shows every signed-in user browsing automations
// read-only, mirroring ListPlans' own "no extra RBAC for a plain read"
// precedent rather than ListMembers' own admin-only gate (members are a
// workspace-wide identity/role listing, a materially more sensitive
// surface than an automation's own name/repos/trigger).
//
// # Webhook token rotate/revoke (review fix)
//
// The webhook bearer token minted at CreateAutomation time (below) used to
// be permanent and non-rotatable -- the first such token in this codebase
// (every other bearer-token precedent, ws_tokens.token_hash/
// sandboxes.token_hash, is expiring/rotatable). RotateAutomationWebhookToken
// mints a brand-new token exactly the same way CreateAutomation does
// (platform.GenerateToken()/HashToken()) and overwrites webhook_token_hash
// outright, invalidating the old token immediately, no grace period.
// RevokeAutomationWebhookToken clears webhook_token_hash to NULL --
// automationwebhook's own handler.go (internal/adapters/inbound/
// automationwebhook) already 401s on ANY hash miss, so revoking needs no
// handler-side change at all for the revoke to take effect; it is a pure
// data change.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/authz"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/platform"
)

// parseAutomationID parses chi's own "automationID" URL path param as a
// UUID -- mirrors parseSessionID's own identical shape (helpers.go).
func parseAutomationID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	raw := chi.URLParam(r, "automationID")
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		writeError(w, http.StatusBadRequest, "malformed automation id")
		return pgtype.UUID{}, false
	}
	return id, true
}

// automationToDTO converts a stored sqlcgen.Automation row into its REST
// wire shape (restdtos.Automation) -- mirrors sessionToDTO's own identical
// "explicit conversions for enum/nullability-representation mismatches"
// shape (session.go). webhook_token_hash is deliberately NEVER surfaced
// here (a bearer credential, returned in plaintext exactly once, at
// creation, mirroring MintWSToken's own identical convention) -- this
// function has no field for it at all.
//
// runHealth is resolved by the caller (ListAutomations/GetAutomation, a
// single batched ListRunHealthForAutomations query rather than one query
// per row) and passed through verbatim -- nil means this automation has
// never had a terminal run (§12.2 item 4's own gap, an honest "no runs
// yet" rather than a fabricated 0/0).
func automationToDTO(a sqlcgen.Automation, runHealth *restdtos.AutomationRunHealth) restdtos.Automation {
	var prompt *string
	if a.Prompt != nil {
		prompt = a.Prompt
	}

	var createdBy *string
	if a.CreatedBy.Valid {
		str := a.CreatedBy.String()
		createdBy = &str
	}

	var repos []restdtos.AutomationReposElem
	_ = json.Unmarshal(a.Repos, &repos)

	var pathScope *restdtos.AutomationSandboxPathScope
	if len(a.SandboxPathScope) > 0 {
		var patterns []string
		if err := json.Unmarshal(a.SandboxPathScope, &patterns); err == nil && len(patterns) > 0 {
			ps := restdtos.AutomationSandboxPathScope(patterns)
			pathScope = &ps
		}
	}

	var envVars []restdtos.AutomationEnvVarElem
	if len(a.EnvVars) > 0 {
		_ = json.Unmarshal(a.EnvVars, &envVars)
	}
	if envVars == nil {
		envVars = []restdtos.AutomationEnvVarElem{}
	}

	// AutomationLastRunAt is ITSELF a named *time.Time (go-jsonschema's own
	// generated shape for a nullable, non-enum date-time field, distinct
	// from the wrapped-struct shape nullable ENUMS like LastRunStatus get
	// below) -- so its own zero value (nil) already means "never run yet",
	// with no extra pointer-to-pointer indirection needed.
	var lastRunAt restdtos.AutomationLastRunAt
	if a.LastRunAt.Valid {
		t := a.LastRunAt.Time
		lastRunAt = restdtos.AutomationLastRunAt(&t)
	}

	var lastRunStatus *restdtos.AutomationLastRunStatus
	if a.LastRunStatus != nil {
		lastRunStatus = &restdtos.AutomationLastRunStatus{Value: string(*a.LastRunStatus)}
	}

	return restdtos.Automation{
		Id:                    a.ID.String(),
		Name:                  a.Name,
		Prompt:                prompt,
		Repos:                 repos,
		Status:                restdtos.AutomationStatus(a.Status),
		ConsecutiveFailures:   int(a.ConsecutiveFailures),
		CreatedBy:             createdBy,
		CreatedAt:             a.CreatedAt.Time,
		UpdatedAt:             a.UpdatedAt.Time,
		TriggerType:           restdtos.AutomationTriggerType(a.TriggerType),
		TriggerConfig:         json.RawMessage(a.TriggerConfig),
		SandboxPathScope:      pathScope,
		SandboxMockConfigured: a.SandboxMockConfigured,
		SandboxContractsPath:  a.SandboxContractsPath,
		EnvVars:               envVars,
		LastRunAt:             lastRunAt,
		LastRunStatus:         lastRunStatus,
		ArtifactSummary:       a.ArtifactSummary,
		RunHealth:             runHealth,
	}
}

// runHealthDTO converts one sqlcgen.ListRunHealthForAutomationsRow into
// its wire shape -- the one place both lookupRunHealth (single row) and
// lookupRunHealthBatch (many rows, below) build a restdtos.
// AutomationRunHealth, so the two never drift.
func runHealthDTO(row sqlcgen.ListRunHealthForAutomationsRow) *restdtos.AutomationRunHealth {
	return &restdtos.AutomationRunHealth{
		SucceededRuns: int(row.SucceededRuns),
		TerminalRuns:  int(row.TerminalRuns),
	}
}

// lookupRunHealth resolves ONE automation's own §12.2 item 4 health ratio
// -- nil (never an error) when this id has no terminal runs at all yet,
// exactly like ListRunHealthForAutomations' own generated doc comment
// describes. Used by every single-automation response (Get/Pause/Resume/
// Rotate/Revoke) -- ListAutomations below uses the batched form instead,
// one query for the whole page rather than one per row.
func lookupRunHealth(ctx context.Context, runs *postgres.AutomationRunStore, automationID pgtype.UUID) (*restdtos.AutomationRunHealth, error) {
	rows, err := runs.ListRunHealth(ctx, []pgtype.UUID{automationID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return runHealthDTO(rows[0]), nil
}

// lookupRunHealthBatch resolves §12.2 item 4's own health ratio for EVERY
// id in automationIDs in one query -- the ListAutomations page-load path,
// where issuing lookupRunHealth per row would mean N extra round trips
// for a page of N automations. Returns a map keyed by the row's own
// String() id (sqlcgen.Automation.ID.String(), the same key
// automationToDTO's own caller already has in hand) -- an id absent from
// the map has no terminal runs, mirrors lookupRunHealth's own nil.
func lookupRunHealthBatch(ctx context.Context, runs *postgres.AutomationRunStore, automationIDs []pgtype.UUID) (map[string]*restdtos.AutomationRunHealth, error) {
	rows, err := runs.ListRunHealth(ctx, automationIDs)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*restdtos.AutomationRunHealth, len(rows))
	for _, row := range rows {
		byID[row.AutomationID.String()] = runHealthDTO(row)
	}
	return byID, nil
}

// CreateAutomation backs POST /api/automations (§8.4). 403 if the
// caller fails authz.ActionManageAutomations; 400 for a malformed request
// body or any validation failure (repos, trigger config, sandbox
// settings, env vars); 201 with restdtos.CreateAutomationResponse
// otherwise -- carrying the plaintext webhook token exactly once, iff
// triggerType is "webhook" (mirrors MintWSToken's own identical
// "hashed at rest, plaintext returned exactly once" convention).
func CreateAutomation(automations *postgres.AutomationStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionManageAutomations, authz.Resource{}) {
			return
		}
		userID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.CreateAutomationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}

		if req.Name == "" {
			writeError(w, http.StatusBadRequest, "name must not be empty")
			return
		}

		reposJSON, targets, verr := validateAutomationRepos(req.Repos)
		if verr != "" {
			writeError(w, http.StatusBadRequest, verr)
			return
		}
		if err := domainautomation.ValidateTargets(targets); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("repos: %s", err))
			return
		}

		triggerType := domainautomation.TriggerType(req.TriggerType)
		if err := domainautomation.ValidateTriggerType(triggerType); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		triggerConfigJSON, err := buildTriggerConfig(triggerType, req.TriggerConfig)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("triggerConfig: %s", err))
			return
		}

		settings := domainautomation.SandboxSettings{}
		if req.SandboxPathScope != nil {
			settings.PathScope = []string(*req.SandboxPathScope)
		}
		if req.SandboxMockConfig != nil {
			settings.MockConfigured = true
			if req.SandboxMockConfig.ContractsPath != nil {
				settings.ContractsPath = *req.SandboxMockConfig.ContractsPath
			}
		}
		if err := domainautomation.ValidateSandboxSettings(settings); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("sandbox settings: %s", err))
			return
		}

		envVars := make([]domainautomation.EnvVar, len(req.EnvVars))
		for i, v := range req.EnvVars {
			envVars[i] = domainautomation.EnvVar{Name: v.Name, Value: v.Value}
		}
		if err := domainautomation.ValidateEnvVars(envVars); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("envVars: %s", err))
			return
		}
		envVarsWire := req.EnvVars
		if envVarsWire == nil {
			envVarsWire = []restdtos.AutomationEnvVarElem{}
		}
		envVarsJSON, err := json.Marshal(envVarsWire)
		if err != nil {
			logger.Error("httpapi: marshal automation env vars failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		var sandboxPathScopeJSON []byte
		if len(settings.PathScope) > 0 {
			sandboxPathScopeJSON, err = json.Marshal(settings.PathScope)
			if err != nil {
				logger.Error("httpapi: marshal automation sandbox path scope failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
		var sandboxContractsPath *string
		if settings.MockConfigured {
			cp := settings.ContractsPath
			sandboxContractsPath = &cp
		}

		var webhookToken *string
		var webhookTokenHash *string
		if triggerType == domainautomation.TriggerTypeWebhook {
			token, terr := platform.GenerateToken()
			if terr != nil {
				logger.Error("httpapi: generate automation webhook token failed", "error", terr)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			hash := platform.HashToken(token)
			webhookToken = &token
			webhookTokenHash = &hash
		}

		var promptCol *string
		if req.Prompt != nil {
			promptCol = req.Prompt
		}

		created, err := automations.Create(ctx, sqlcgen.CreateAutomationParams{
			Name:                  req.Name,
			Prompt:                promptCol,
			Repos:                 reposJSON,
			CreatedBy:             userID,
			TriggerType:           sqlcgen.AutomationTriggerType(triggerType),
			TriggerConfig:         triggerConfigJSON,
			WebhookTokenHash:      webhookTokenHash,
			SandboxPathScope:      sandboxPathScopeJSON,
			SandboxMockConfigured: settings.MockConfigured,
			SandboxContractsPath:  sandboxContractsPath,
			EnvVars:               envVarsJSON,
		})
		if err != nil {
			logger.Error("httpapi: create automation failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusCreated, restdtos.CreateAutomationResponse{
			// runHealth is nil directly, no query: a row this handler just
			// inserted moments ago cannot have any automation_runs yet.
			Automation:   automationToDTO(created, nil),
			WebhookToken: webhookToken,
		})
	}
}

// validateAutomationRepos validates every repo's Name/Url/Branch (the SAME
// reposource.ValidateRepoName/ValidateRepoURL/ValidateBranch checks
// validateCreateSessionRequest already applies to CreateSessionRequest.
// repos, create.go) and marshals them for the automations.repos JSONB
// column, returning the domainautomation.Target slice too (so the caller
// can additionally run domainautomation.ValidateTargets' own fan-out-cap
// check without a second conversion pass). Returns a non-empty message
// (never wrapped as an error type, matching this file's own inline
// writeError-at-call-site style) on the first problem found.
func validateAutomationRepos(repos []restdtos.AutomationReposElem) ([]byte, []domainautomation.Target, string) {
	if len(repos) < 1 {
		return nil, nil, "repos must be non-empty"
	}

	targets := make([]domainautomation.Target, len(repos))
	for i, repo := range repos {
		if err := reposource.ValidateRepoName(repo.Name); err != nil {
			return nil, nil, fmt.Sprintf("repos[%d].name: %s", i, err)
		}
		if err := reposource.ValidateRepoURL(repo.Url); err != nil {
			return nil, nil, fmt.Sprintf("repos[%d].url: %s", i, err)
		}
		branch := ""
		if repo.Branch != nil {
			if err := reposource.ValidateBranch(*repo.Branch); err != nil {
				return nil, nil, fmt.Sprintf("repos[%d].branch: %s", i, err)
			}
			branch = *repo.Branch
		}
		targets[i] = domainautomation.Target{Name: repo.Name, URL: repo.Url, Branch: branch}
	}

	reposJSON, err := json.Marshal(repos)
	if err != nil {
		return nil, nil, "internal error marshaling repos"
	}
	return reposJSON, targets, ""
}

// cronTriggerConfigWire/githubTriggerConfigWire/linearTriggerConfigWire are
// this handler's OWN small, private wire structs for decoding+
// re-marshaling a request's own triggerConfig into a CANONICAL shape
// (only the fields this codebase recognizes, never arbitrary
// caller-supplied extra keys passed through verbatim) -- deliberately NOT
// shared with internal/app/automation's own (separate, decode-only)
// cronTriggerConfigJSON: app/automation already imports this package
// (fanout.go, for CreateSessionOnTx), so this package importing IT back
// would be an import cycle. Each copy is small (~3 fields) and one-way
// (this package only ever WRITES trigger_config; app/automation's trigger
// pump only ever READS the one shape it cares about, cron) -- see
// internal/app/automation/triggerpump.go's own identical doc comment on
// this same constraint.
type cronTriggerConfigWire struct {
	Schedule string `json:"schedule"`
}
type githubTriggerConfigWire struct {
	Event  string `json:"event"`
	Action string `json:"action,omitempty"`
	Label  string `json:"label,omitempty"`
}
type linearTriggerConfigWire struct {
	EventType string `json:"eventType"`
	Action    string `json:"action,omitempty"`
	TeamKey   string `json:"teamKey,omitempty"`
}

// buildTriggerConfig decodes raw (the request's own, possibly-absent,
// opaque triggerConfig object) against whichever shape triggerType
// implies, validates it via internal/domain/automation's own
// ValidateCronTriggerConfig/ValidateGitHubTriggerConfig/
// ValidateLinearTriggerConfig, and re-marshals a CANONICAL JSON object
// (only the recognized fields) for storage -- never the caller's own raw
// bytes verbatim, so an extra/unexpected key in the request body is
// silently dropped rather than persisted. manual/webhook always store
// "{}", ignoring raw entirely (neither trigger type has any config of its
// own -- webhook's own "condition" is authentication, handled by a
// dedicated column, never trigger_config).
func buildTriggerConfig(triggerType domainautomation.TriggerType, raw *json.RawMessage) ([]byte, error) {
	switch triggerType {
	case domainautomation.TriggerTypeManual, domainautomation.TriggerTypeWebhook:
		return []byte("{}"), nil

	case domainautomation.TriggerTypeCron:
		var wire cronTriggerConfigWire
		if raw != nil {
			if err := json.Unmarshal(*raw, &wire); err != nil {
				return nil, errors.New("malformed cron trigger config")
			}
		}
		cfg := domainautomation.CronTriggerConfig{Schedule: wire.Schedule}
		if err := domainautomation.ValidateCronTriggerConfig(cfg); err != nil {
			return nil, err
		}
		return json.Marshal(cronTriggerConfigWire{Schedule: cfg.Schedule})

	case domainautomation.TriggerTypeGitHub:
		var wire githubTriggerConfigWire
		if raw != nil {
			if err := json.Unmarshal(*raw, &wire); err != nil {
				return nil, errors.New("malformed github trigger config")
			}
		}
		cfg := domainautomation.GitHubTriggerConfig{Event: wire.Event, Action: wire.Action, Label: wire.Label}
		if err := domainautomation.ValidateGitHubTriggerConfig(cfg); err != nil {
			return nil, err
		}
		return json.Marshal(githubTriggerConfigWire{Event: cfg.Event, Action: cfg.Action, Label: cfg.Label})

	case domainautomation.TriggerTypeLinear:
		var wire linearTriggerConfigWire
		if raw != nil {
			if err := json.Unmarshal(*raw, &wire); err != nil {
				return nil, errors.New("malformed linear trigger config")
			}
		}
		cfg := domainautomation.LinearTriggerConfig{EventType: wire.EventType, Action: wire.Action, TeamKey: wire.TeamKey}
		if err := domainautomation.ValidateLinearTriggerConfig(cfg); err != nil {
			return nil, err
		}
		return json.Marshal(linearTriggerConfigWire{EventType: cfg.EventType, Action: cfg.Action, TeamKey: cfg.TeamKey})

	default:
		// Unreachable: the caller already ran ValidateTriggerType before
		// this function is ever called.
		return []byte("{}"), nil
	}
}

// GetAutomation backs GET /api/automations/{automationID} -- 404 if the
// automation doesn't exist; 200 with restdtos.Automation otherwise. No
// extra RBAC beyond "must be logged in" -- see this file's own top doc
// comment for why.
func GetAutomation(automations *postgres.AutomationStore, runs *postgres.AutomationRunStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		id, ok := parseAutomationID(w, r)
		if !ok {
			return
		}

		a, err := automations.Get(ctx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "automation not found")
				return
			}
			logger.Error("httpapi: get automation failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		health, err := lookupRunHealth(ctx, runs, id)
		if err != nil {
			logger.Error("httpapi: lookup automation run health failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, automationToDTO(a, health))
	}
}

// ListAutomations backs GET /api/automations (§8.4's own
// "creator/status filters"): two independent, optional query params --
// ?createdBy=me|<uuid> and ?status=active|paused. "me" resolves to the
// authenticated caller's own id (mockups.html's own "My automations ▾"
// toolbar filter); an explicit UUID filters to that creator instead
// (visible to anyone -- this list itself carries no per-row RBAC, see this
// file's own top doc comment); either query param absent means "no
// filter" for that dimension. An unrecognized status value is a 400, not
// a silently-ignored filter.
func ListAutomations(automations *postgres.AutomationStore, runs *postgres.AutomationRunStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		var createdBy pgtype.UUID
		if raw := r.URL.Query().Get("createdBy"); raw != "" {
			if raw == "me" {
				userID, ok := authenticatedUserID(w, r)
				if !ok {
					return
				}
				createdBy = userID
			} else if err := createdBy.Scan(raw); err != nil {
				writeError(w, http.StatusBadRequest, "malformed createdBy filter")
				return
			}
		}

		var status *sqlcgen.AutomationStatus
		if raw := r.URL.Query().Get("status"); raw != "" {
			switch raw {
			case string(sqlcgen.AutomationStatusActive), string(sqlcgen.AutomationStatusPaused):
				s := sqlcgen.AutomationStatus(raw)
				status = &s
			default:
				writeError(w, http.StatusBadRequest, "unrecognized status filter")
				return
			}
		}

		rows, err := automations.List(ctx, createdBy, status)
		if err != nil {
			logger.Error("httpapi: list automations failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		ids := make([]pgtype.UUID, len(rows))
		for i, row := range rows {
			ids[i] = row.ID
		}
		health, err := lookupRunHealthBatch(ctx, runs, ids)
		if err != nil {
			logger.Error("httpapi: batch lookup automation run health failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		wire := make([]restdtos.Automation, len(rows))
		for i, row := range rows {
			wire[i] = automationToDTO(row, health[row.ID.String()])
		}
		writeJSON(w, http.StatusOK, restdtos.ListAutomationsResponse{Automations: wire})
	}
}

// PauseAutomation backs POST /api/automations/{automationID}/pause -- the
// direct-admin-action twin of ResumeAutomation below (internal/domain/
// automation.TriggerAutoPause applied via PauseAutomation, queries/
// automations.sql, rather than the auto-pause path). 403 if the caller
// fails authz.ActionManageAutomations; 404 if the automation doesn't exist
// OR is already paused (PauseAutomation's own "AND status = 'active'"
// guard makes both cases indistinguishable from a plain row lookup, so
// this handler does a separate existence check first to give a genuine
// "not found" a distinct message from "already paused").
func PauseAutomation(automations *postgres.AutomationStore, runs *postgres.AutomationRunStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionManageAutomations, authz.Resource{}) {
			return
		}
		id, ok := parseAutomationID(w, r)
		if !ok {
			return
		}

		if _, err := automations.Get(ctx, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "automation not found")
				return
			}
			logger.Error("httpapi: get automation failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		a, err := automations.Pause(ctx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusConflict, "automation is not active")
				return
			}
			logger.Error("httpapi: pause automation failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		health, err := lookupRunHealth(ctx, runs, id)
		if err != nil {
			logger.Error("httpapi: lookup automation run health failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, automationToDTO(a, health))
	}
}

// ResumeAutomation backs POST /api/automations/{automationID}/resume --
// applies internal/domain/automation.TriggerResume. Same existence-check-
// then-CAS shape as PauseAutomation immediately above.
func ResumeAutomation(automations *postgres.AutomationStore, runs *postgres.AutomationRunStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionManageAutomations, authz.Resource{}) {
			return
		}
		id, ok := parseAutomationID(w, r)
		if !ok {
			return
		}

		if _, err := automations.Get(ctx, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "automation not found")
				return
			}
			logger.Error("httpapi: get automation failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		a, err := automations.Resume(ctx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusConflict, "automation is not paused")
				return
			}
			logger.Error("httpapi: resume automation failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		health, err := lookupRunHealth(ctx, runs, id)
		if err != nil {
			logger.Error("httpapi: lookup automation run health failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, automationToDTO(a, health))
	}
}

// RotateAutomationWebhookToken backs POST /api/automations/{automationID}/
// webhook-token (review fix: "webhook token has no rotation/revocation/
// expiry") -- mints a brand-new plaintext webhook bearer token exactly the
// same way CreateAutomation does (platform.GenerateToken()/HashToken()) and
// overwrites the automation's own webhook_token_hash outright, so the OLD
// token stops authenticating immediately, with no grace period (mirrors
// MintWSToken's own "hashed at rest, plaintext returned exactly once"
// convention). 403 if the caller fails authz.ActionManageAutomations
// (SAME gate CreateAutomation applies); 404 if the automation doesn't
// exist (a separate existence check first, mirroring PauseAutomation's
// own identical "distinct 404 vs 409" shape); 409 if the automation's own
// triggerType is not "webhook" (this mechanism only ever exists for a
// webhook-triggered automation -- rotating a token that could never have
// existed in the first place is a conflict, not a silent success); 200
// with restdtos.RotateAutomationWebhookTokenResponse otherwise.
func RotateAutomationWebhookToken(automations *postgres.AutomationStore, runs *postgres.AutomationRunStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionManageAutomations, authz.Resource{}) {
			return
		}
		id, ok := parseAutomationID(w, r)
		if !ok {
			return
		}

		existing, err := automations.Get(ctx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "automation not found")
				return
			}
			logger.Error("httpapi: get automation failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if existing.TriggerType != sqlcgen.AutomationTriggerTypeWebhook {
			writeError(w, http.StatusConflict, "automation is not webhook-triggered")
			return
		}

		token, terr := platform.GenerateToken()
		if terr != nil {
			logger.Error("httpapi: generate automation webhook token failed", "error", terr)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		hash := platform.HashToken(token)

		a, err := automations.RotateWebhookToken(ctx, id, hash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Lost a race against a concurrent change to this
				// automation's own trigger_type between the Get above and
				// this guarded UPDATE (trigger_type is otherwise immutable
				// once created -- no code path in this codebase changes it
				// today, so this is defensive, not a live concern) --
				// treated identically to the pre-check above.
				writeError(w, http.StatusConflict, "automation is not webhook-triggered")
				return
			}
			logger.Error("httpapi: rotate automation webhook token failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		health, err := lookupRunHealth(ctx, runs, id)
		if err != nil {
			logger.Error("httpapi: lookup automation run health failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, restdtos.RotateAutomationWebhookTokenResponse{
			Automation:   automationToDTO(a, health),
			WebhookToken: token,
		})
	}
}

// RevokeAutomationWebhookToken backs DELETE /api/automations/
// {automationID}/webhook-token (review fix, same finding as
// RotateAutomationWebhookToken above) -- clears the automation's own
// webhook_token_hash to NULL. After this commits, automationwebhook's own
// handler.go (internal/adapters/inbound/automationwebhook) already 401s on
// ANY hash miss, permanently, until a subsequent RotateAutomationWebhookToken
// mints a new one -- no handler-side change needed for the revoke to take
// effect. 403 if the caller fails authz.ActionManageAutomations; 404 if the
// automation doesn't exist; 200 with the updated restdtos.Automation
// otherwise (no plaintext token to ever return here, unlike rotate/create).
func RevokeAutomationWebhookToken(automations *postgres.AutomationStore, runs *postgres.AutomationRunStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionManageAutomations, authz.Resource{}) {
			return
		}
		id, ok := parseAutomationID(w, r)
		if !ok {
			return
		}

		if _, err := automations.Get(ctx, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "automation not found")
				return
			}
			logger.Error("httpapi: get automation failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		a, err := automations.RevokeWebhookToken(ctx, id)
		if err != nil {
			logger.Error("httpapi: revoke automation webhook token failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		health, err := lookupRunHealth(ctx, runs, id)
		if err != nil {
			logger.Error("httpapi: lookup automation run health failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, automationToDTO(a, health))
	}
}
