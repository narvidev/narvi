//go:build integration

// Integration tests for §8.4's ("automations: triggers & extras", §8.4)
// own REST CRUD surface over automations (automations.go), against a real
// Postgres instance -- gated behind the "integration" build tag, sharing
// this package's own testRig (httpapi_integration_test.go).
package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

const createManualAutomationBody = `{"name":"nightly audit","prompt":"do the thing","repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}],"triggerType":"manual"}`

// TestCreateAutomation_MemberDenied proves an ordinary member is denied
// (403) -- authz.ActionManageAutomations is admin/maintainer only.
func TestCreateAutomation_MemberDenied(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t) // default role: member.

	status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(createManualAutomationBody), nil, token)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestCreateAutomation_MaintainerAllowed_ManualRoundTrips proves a
// maintainer can create a manual-trigger automation, and that GET reflects
// it -- including the "never run yet" defaults (lastRunAt/lastRunStatus/
// artifactSummary all null).
func TestCreateAutomation_MaintainerAllowed_ManualRoundTrips(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	var created restdtos.CreateAutomationResponse
	status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(createManualAutomationBody), &created, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if created.Automation.Name != "nightly audit" {
		t.Errorf("Name = %q, want %q", created.Automation.Name, "nightly audit")
	}
	if created.Automation.Prompt == nil || *created.Automation.Prompt != "do the thing" {
		t.Errorf("Prompt = %v, want \"do the thing\"", created.Automation.Prompt)
	}
	if created.Automation.TriggerType != restdtos.AutomationTriggerTypeManual {
		t.Errorf("TriggerType = %q, want %q", created.Automation.TriggerType, restdtos.AutomationTriggerTypeManual)
	}
	if created.Automation.Status != restdtos.AutomationStatusActive {
		t.Errorf("Status = %q, want %q (every automation starts active)", created.Automation.Status, restdtos.AutomationStatusActive)
	}
	if created.Automation.CreatedBy == nil || *created.Automation.CreatedBy != user.ID.String() {
		t.Errorf("CreatedBy = %v, want %s", created.Automation.CreatedBy, user.ID.String())
	}
	if created.Automation.LastRunAt != nil {
		t.Errorf("LastRunAt = %v, want nil (never run yet)", created.Automation.LastRunAt)
	}
	if created.Automation.LastRunStatus != nil {
		t.Errorf("LastRunStatus = %v, want nil (never run yet)", created.Automation.LastRunStatus)
	}
	if created.Automation.ArtifactSummary != nil {
		t.Errorf("ArtifactSummary = %v, want nil (never run yet)", created.Automation.ArtifactSummary)
	}
	if created.WebhookToken != nil {
		t.Errorf("WebhookToken = %v, want nil for a non-webhook trigger", created.WebhookToken)
	}

	var got restdtos.Automation
	status = rig.doJSON(t, http.MethodGet, "/api/automations/"+created.Automation.Id, nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", status, http.StatusOK)
	}
	if got.Id != created.Automation.Id {
		t.Errorf("GET id = %q, want %q", got.Id, created.Automation.Id)
	}
}

// TestCreateAutomation_RejectsEmptyRepos proves the SAME repos-must-be-
// non-empty validation CreateSessionRequest.repos already gets also
// applies here.
func TestCreateAutomation_RejectsEmptyRepos(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	body := `{"name":"empty repos","prompt":null,"repos":[],"triggerType":"manual"}`
	status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(body), nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestCreateAutomation_RejectsInvalidCronSchedule proves
// internal/domain/automation.ValidateCronTriggerConfig's own verdict
// actually gates this endpoint.
func TestCreateAutomation_RejectsInvalidCronSchedule(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	body := `{"name":"bad cron","prompt":null,"repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}],"triggerType":"cron","triggerConfig":{"schedule":"not a cron expr"}}`
	status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(body), nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestCreateAutomation_CronRoundTripsSchedule proves a VALID cron
// schedule round-trips through create->get unchanged.
func TestCreateAutomation_CronRoundTripsSchedule(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	body := `{"name":"nightly cron","prompt":null,"repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}],"triggerType":"cron","triggerConfig":{"schedule":"0 2 * * *"}}`
	var created restdtos.CreateAutomationResponse
	status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(body), &created, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if created.Automation.TriggerType != restdtos.AutomationTriggerTypeCron {
		t.Errorf("TriggerType = %q, want %q", created.Automation.TriggerType, restdtos.AutomationTriggerTypeCron)
	}

	var cfg struct {
		Schedule string `json:"schedule"`
	}
	if err := json.Unmarshal(created.Automation.TriggerConfig, &cfg); err != nil {
		t.Fatalf("unmarshal trigger config: %v", err)
	}
	if cfg.Schedule != "0 2 * * *" {
		t.Errorf("triggerConfig.schedule = %q, want %q", cfg.Schedule, "0 2 * * *")
	}
}

// TestCreateAutomation_WebhookTriggerReturnsTokenExactlyOnce proves
// §8.4's own webhook-facing surface mints a real bearer token, returned
// only in the create response -- mirroring MintWSToken's own identical
// "hashed at rest, plaintext returned exactly once" convention -- and that
// a subsequent GET never re-exposes it (restdtos.Automation carries no
// such field at all).
func TestCreateAutomation_WebhookTriggerReturnsTokenExactlyOnce(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	body := `{"name":"webhook automation","prompt":null,"repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}],"triggerType":"webhook"}`
	var created restdtos.CreateAutomationResponse
	status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(body), &created, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if created.WebhookToken == nil || *created.WebhookToken == "" {
		t.Fatalf("WebhookToken = %v, want a real, non-empty plaintext token for a webhook trigger", created.WebhookToken)
	}

	if got, err := rig.automations.GetByWebhookTokenHash(ctx, platform.HashToken(*created.WebhookToken)); err != nil {
		t.Fatalf("GetByWebhookTokenHash: %v", err)
	} else if got.ID.String() != created.Automation.Id {
		t.Fatalf("GetByWebhookTokenHash returned id %s, want %s", got.ID.String(), created.Automation.Id)
	}
}

// TestListAutomations_FiltersByCreatedByMe proves the "creator" filter
// (§8.4's own "creator/status filters") -- two different maintainers each
// create one automation; each one's own ?createdBy=me lists ONLY the one
// they created.
func TestListAutomations_FiltersByCreatedByMe(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	userA, tokenA := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
	_, tokenB := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	bodyA := `{"name":"automation A","prompt":null,"repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}],"triggerType":"manual"}`
	bodyB := `{"name":"automation B","prompt":null,"repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}],"triggerType":"manual"}`
	var createdA, createdB restdtos.CreateAutomationResponse
	if status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(bodyA), &createdA, tokenA); status != http.StatusCreated {
		t.Fatalf("create A status = %d, want %d", status, http.StatusCreated)
	}
	if status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(bodyB), &createdB, tokenB); status != http.StatusCreated {
		t.Fatalf("create B status = %d, want %d", status, http.StatusCreated)
	}

	var list restdtos.ListAutomationsResponse
	status := rig.doJSON(t, http.MethodGet, "/api/automations?createdBy=me", nil, &list, tokenA)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	for _, a := range list.Automations {
		if a.CreatedBy == nil || *a.CreatedBy != userA.ID.String() {
			t.Errorf("list for ?createdBy=me (user A) returned an automation not created by A: %+v", a)
		}
	}
	foundA := false
	for _, a := range list.Automations {
		if a.Id == createdA.Automation.Id {
			foundA = true
		}
		if a.Id == createdB.Automation.Id {
			t.Errorf("list for ?createdBy=me (user A) unexpectedly included user B's automation")
		}
	}
	if !foundA {
		t.Errorf("list for ?createdBy=me (user A) did not include user A's own automation")
	}
}

// TestListAutomations_FiltersByStatus proves the "status" filter -- an
// active and a paused automation, ?status=paused returns only the paused
// one.
func TestListAutomations_FiltersByStatus(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var active, paused restdtos.CreateAutomationResponse
	activeBody := `{"name":"stays active","prompt":null,"repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}],"triggerType":"manual"}`
	pausedBody := `{"name":"gets paused","prompt":null,"repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}],"triggerType":"manual"}`
	if status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(activeBody), &active, token); status != http.StatusCreated {
		t.Fatalf("create active status = %d, want %d", status, http.StatusCreated)
	}
	if status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(pausedBody), &paused, token); status != http.StatusCreated {
		t.Fatalf("create paused status = %d, want %d", status, http.StatusCreated)
	}
	if status := rig.doJSON(t, http.MethodPost, "/api/automations/"+paused.Automation.Id+"/pause", nil, nil, token); status != http.StatusOK {
		t.Fatalf("pause status = %d, want %d", status, http.StatusOK)
	}

	var list restdtos.ListAutomationsResponse
	status := rig.doJSON(t, http.MethodGet, "/api/automations?status=paused", nil, &list, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	for _, a := range list.Automations {
		if a.Status != restdtos.AutomationStatusPaused {
			t.Errorf("?status=paused returned a non-paused automation: %+v", a)
		}
		if a.Id == active.Automation.Id {
			t.Errorf("?status=paused unexpectedly included the still-active automation")
		}
	}
}

// TestListAutomations_UnrecognizedStatusIsBadRequest proves an
// unrecognized status value is a 400, never a silently-ignored filter.
func TestListAutomations_UnrecognizedStatusIsBadRequest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodGet, "/api/automations?status=bogus", nil, nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestGetAutomation_NotFound proves a well-formed but nonexistent id is a
// 404.
func TestGetAutomation_NotFound(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodGet, "/api/automations/00000000-0000-0000-0000-000000000000", nil, nil, token)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestPauseAutomation_MemberDenied proves the member/maintainer split
// applies to pause/resume too, not just create.
func TestPauseAutomation_MemberDenied(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	_, memberToken := rig.createAuthenticatedUser(ctx, t)

	var created restdtos.CreateAutomationResponse
	if status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(createManualAutomationBody), &created, adminToken); status != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", status, http.StatusCreated)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/automations/"+created.Automation.Id+"/pause", nil, nil, memberToken)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestPauseThenResumeAutomation_FullLifecycle drives Active -> Paused ->
// Active end to end via the REST surface, including the conflict cases
// (pausing an already-paused automation, resuming an already-active one).
func TestPauseThenResumeAutomation_FullLifecycle(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	var created restdtos.CreateAutomationResponse
	if status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(createManualAutomationBody), &created, token); status != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", status, http.StatusCreated)
	}
	id := created.Automation.Id

	var paused restdtos.Automation
	if status := rig.doJSON(t, http.MethodPost, "/api/automations/"+id+"/pause", nil, &paused, token); status != http.StatusOK {
		t.Fatalf("pause status = %d, want %d", status, http.StatusOK)
	}
	if paused.Status != restdtos.AutomationStatusPaused {
		t.Fatalf("status after pause = %q, want %q", paused.Status, restdtos.AutomationStatusPaused)
	}

	if status := rig.doJSON(t, http.MethodPost, "/api/automations/"+id+"/pause", nil, nil, token); status != http.StatusConflict {
		t.Fatalf("second pause status = %d, want %d", status, http.StatusConflict)
	}

	var resumed restdtos.Automation
	if status := rig.doJSON(t, http.MethodPost, "/api/automations/"+id+"/resume", nil, &resumed, token); status != http.StatusOK {
		t.Fatalf("resume status = %d, want %d", status, http.StatusOK)
	}
	if resumed.Status != restdtos.AutomationStatusActive {
		t.Fatalf("status after resume = %q, want %q", resumed.Status, restdtos.AutomationStatusActive)
	}

	if status := rig.doJSON(t, http.MethodPost, "/api/automations/"+id+"/resume", nil, nil, token); status != http.StatusConflict {
		t.Fatalf("second resume status = %d, want %d", status, http.StatusConflict)
	}
}

// TestCreateAutomation_SandboxSettingsAndEnvVarsRoundTrip proves §8.4's
// own sandboxSettings/envVars fields round-trip through create->get.
func TestCreateAutomation_SandboxSettingsAndEnvVarsRoundTrip(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	body := `{
		"name":"scoped automation",
		"prompt":null,
		"repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}],
		"triggerType":"manual",
		"sandboxPathScope":["apps/web/**"],
		"sandboxMockConfig":{"contractsPath":"contracts/custom"},
		"envVars":[{"name":"TARGET_ENV","value":"staging"}]
	}`
	var created restdtos.CreateAutomationResponse
	status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(body), &created, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if created.Automation.SandboxPathScope == nil || len(*created.Automation.SandboxPathScope) != 1 || (*created.Automation.SandboxPathScope)[0] != "apps/web/**" {
		t.Errorf("SandboxPathScope = %v, want [apps/web/**]", created.Automation.SandboxPathScope)
	}
	if !created.Automation.SandboxMockConfigured {
		t.Errorf("SandboxMockConfigured = false, want true")
	}
	if created.Automation.SandboxContractsPath == nil || *created.Automation.SandboxContractsPath != "contracts/custom" {
		t.Errorf("SandboxContractsPath = %v, want contracts/custom", created.Automation.SandboxContractsPath)
	}
	if len(created.Automation.EnvVars) != 1 || created.Automation.EnvVars[0].Name != "TARGET_ENV" || created.Automation.EnvVars[0].Value != "staging" {
		t.Errorf("EnvVars = %+v, want [{TARGET_ENV staging}]", created.Automation.EnvVars)
	}

	var got restdtos.Automation
	status = rig.doJSON(t, http.MethodGet, "/api/automations/"+created.Automation.Id, nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", status, http.StatusOK)
	}
	if len(got.EnvVars) != 1 || got.EnvVars[0].Value != "staging" {
		t.Errorf("GET EnvVars = %+v, want the same value to round-trip", got.EnvVars)
	}
}

// TestCreateAutomation_InvalidSandboxPathScopeRejected proves
// environment.ValidatePathScope's own traversal check really does gate
// this endpoint (via internal/domain/automation.ValidateSandboxSettings).
func TestCreateAutomation_InvalidSandboxPathScopeRejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	body := `{"name":"bad scope","prompt":null,"repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}],"triggerType":"manual","sandboxPathScope":["../etc"]}`
	status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(body), nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestCreateAutomation_DuplicateEnvVarNameRejected proves
// internal/domain/automation.ValidateEnvVars' own duplicate-name check
// gates this endpoint.
func TestCreateAutomation_DuplicateEnvVarNameRejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	body := `{"name":"dup env","prompt":null,"repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}],"triggerType":"manual","envVars":[{"name":"FOO","value":"1"},{"name":"FOO","value":"2"}]}`
	status := rig.doJSON(t, http.MethodPost, "/api/automations", []byte(body), nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestGetAutomation_RunHealthReflectsRealTerminalRuns is §12.2 item 4's
// own (§8.4) end-to-end proof: RunHealth is a real COUNT over
// automation_runs, produced by seeding genuine invocation/run rows via the
// SAME stores the engine itself writes through (mirrors
// TestListAutomationInvocations_ReturnsNestedRunsNewestFirst's own
// precedent in the sibling file), never asserted against a hand-built
// response. One invocation fans out to three runs -- two succeeded, one
// failed, one still 'running' (non-terminal, must not count in either
// side of the ratio) -- so a mutation that dropped the status filter
// entirely (counting every row) or counted 'succeeded' as the
// denominator instead of "either terminal status" would both be caught.
func TestGetAutomation_RunHealthReflectsRealTerminalRuns(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	automation := createManualAutomation(ctx, t, rig, owner.ID)

	// Each run gets its OWN invocation (automation_runs_invocation_target_uniq,
	// migrations/000054, is at-most-one row per (invocation_id, target) --
	// a separate invocation per run is the simplest way to seed several
	// runs without needing several distinct target repos, and is at least
	// as representative of "all-time" as one invocation would be: real
	// automations accumulate runs across MANY invocations over their
	// lifetime, never all inside one).
	target, err := json.Marshal(map[string]any{"name": "widgets", "url": "https://github.com/acme/widgets", "branch": nil})
	if err != nil {
		t.Fatalf("marshal target: %v", err)
	}
	for _, status := range []sqlcgen.AutomationRunStatus{sqlcgen.AutomationRunStatusSucceeded, sqlcgen.AutomationRunStatusSucceeded, sqlcgen.AutomationRunStatusFailed, sqlcgen.AutomationRunStatusRunning} {
		inv, err := rig.automationInvocations.Create(ctx, sqlcgen.CreateAutomationInvocationParams{
			AutomationID: automation.ID,
			Targets:      []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}]`),
			TotalRuns:    1,
		})
		if err != nil {
			t.Fatalf("create invocation (status %s): %v", status, err)
		}
		if _, err := rig.automationRuns.Create(ctx, sqlcgen.CreateAutomationRunParams{
			InvocationID: inv.ID,
			AutomationID: automation.ID,
			Target:       target,
			Status:       status,
		}); err != nil {
			t.Fatalf("create run (status %s): %v", status, err)
		}
	}

	var got restdtos.Automation
	status := rig.doJSON(t, http.MethodGet, "/api/automations/"+automation.ID.String(), nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.RunHealth == nil {
		t.Fatal("RunHealth = nil, want a real ratio (this automation has terminal runs)")
	}
	if got.RunHealth.SucceededRuns != 2 {
		t.Errorf("RunHealth.SucceededRuns = %d, want 2", got.RunHealth.SucceededRuns)
	}
	if got.RunHealth.TerminalRuns != 3 {
		t.Errorf("RunHealth.TerminalRuns = %d, want 3 (2 succeeded + 1 failed; the still-'running' 4th run must NOT count)", got.RunHealth.TerminalRuns)
	}

	// The list endpoint must report the identical ratio for the same
	// automation -- ListAutomations' own BATCHED lookup must never drift
	// from GetAutomation's own single-id lookup.
	var list restdtos.ListAutomationsResponse
	status = rig.doJSON(t, http.MethodGet, "/api/automations", nil, &list, token)
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200", status)
	}
	found := false
	for _, a := range list.Automations {
		if a.Id != automation.ID.String() {
			continue
		}
		found = true
		if a.RunHealth == nil || a.RunHealth.SucceededRuns != 2 || a.RunHealth.TerminalRuns != 3 {
			t.Errorf("list RunHealth = %+v, want {SucceededRuns:2 TerminalRuns:3}", a.RunHealth)
		}
	}
	if !found {
		t.Fatal("automation not present in ListAutomations response")
	}
}

// TestGetAutomation_RunHealthNilWhenNoTerminalRunsYet proves a freshly
// created automation with zero runs -- and, separately, one whose only
// run is still non-terminal -- renders RunHealth as null, never a
// fabricated 0/0: §12.2 item 4's own "an honest ratio... never a
// fabricated 0/0" requirement.
func TestGetAutomation_RunHealthNilWhenNoTerminalRunsYet(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	automation := createManualAutomation(ctx, t, rig, owner.ID)

	var got restdtos.Automation
	status := rig.doJSON(t, http.MethodGet, "/api/automations/"+automation.ID.String(), nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.RunHealth != nil {
		t.Errorf("RunHealth = %+v, want nil (no runs at all yet)", got.RunHealth)
	}

	target, err := json.Marshal(map[string]any{"name": "widgets", "url": "https://github.com/acme/widgets", "branch": nil})
	if err != nil {
		t.Fatalf("marshal target: %v", err)
	}
	inv, err := rig.automationInvocations.Create(ctx, sqlcgen.CreateAutomationInvocationParams{
		AutomationID: automation.ID,
		Targets:      []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}]`),
		TotalRuns:    1,
	})
	if err != nil {
		t.Fatalf("create invocation: %v", err)
	}
	if _, err := rig.automationRuns.Create(ctx, sqlcgen.CreateAutomationRunParams{
		InvocationID: inv.ID,
		AutomationID: automation.ID,
		Target:       target,
		Status:       sqlcgen.AutomationRunStatusRunning,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	status = rig.doJSON(t, http.MethodGet, "/api/automations/"+automation.ID.String(), nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.RunHealth != nil {
		t.Errorf("RunHealth = %+v, want nil (its only run is still 'running', not terminal)", got.RunHealth)
	}
}
