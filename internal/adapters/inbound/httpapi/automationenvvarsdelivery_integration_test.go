//go:build integration

// Integration tests for §8 item 4's own ("automation env vars reach the
// process, not just the prompt") CP-side sandbox-facing delivery endpoint
// (automationenvvarsdelivery.go), against a real Postgres instance --
// sharing this package's own testRig (httpapi_integration_test.go) and
// scmcredentials_integration_test.go's own createSandboxWithToken/
// moveSandboxStatus/bumpSandboxGen helpers, mirroring
// sandboxsecretsdelivery_integration_test.go's own test shapes exactly
// (this file's own top doc comment: "mirrors sandboxsecretsdelivery.go's
// own handshake VERBATIM").
package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

type automationEnvVarsResp struct {
	EnvVars map[string]string `json:"envVars"`
}

func postAutomationEnvVars(t *testing.T, r testRig, sessionID, bearer, gen string) (int, automationEnvVarsResp) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.server.URL+"/sessions/"+sessionID+"/automation-env-vars", strings.NewReader(``))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if gen != "" {
		req.Header.Set("X-Sandbox-Gen", gen)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got automationEnvVarsResp
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return resp.StatusCode, got
}

// createAutomationRunForSession seeds a real automation + invocation + run
// row chain, with the run's own session_id pointing at sessionID -- the
// exact chain GetAutomationEnvVarsForSession's own join walks. envVarsJSON
// is automations.env_vars' own raw wire shape (`[{"name":...,"value":...}]`).
func createAutomationRunForSession(ctx context.Context, t *testing.T, r testRig, ownerID pgtype.UUID, sessionID pgtype.UUID, envVarsJSON string) sqlcgen.Automation {
	t.Helper()
	a, err := r.automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name:          "env var automation",
		Repos:         []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}]`),
		CreatedBy:     ownerID,
		TriggerType:   sqlcgen.AutomationTriggerTypeManual,
		TriggerConfig: []byte(`{}`),
		EnvVars:       []byte(envVarsJSON),
	})
	if err != nil {
		t.Fatalf("create automation: %v", err)
	}

	inv, err := r.automationInvocations.Create(ctx, sqlcgen.CreateAutomationInvocationParams{
		AutomationID: a.ID,
		Targets:      []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}]`),
		TotalRuns:    1,
	})
	if err != nil {
		t.Fatalf("create invocation: %v", err)
	}

	target, err := json.Marshal(map[string]any{"name": "widgets", "url": "https://github.com/acme/widgets", "branch": nil})
	if err != nil {
		t.Fatalf("marshal target: %v", err)
	}
	if _, err := r.automationRuns.Create(ctx, sqlcgen.CreateAutomationRunParams{
		InvocationID: inv.ID,
		AutomationID: a.ID,
		Target:       target,
		Status:       sqlcgen.AutomationRunStatusStarting,
		SessionID:    sessionID,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	return a
}

// --- Auth / dead-sandbox / gen-fencing (mirrors sandboxsecretsdelivery_integration_test.go) ---

func TestAutomationEnvVarsDelivery_MissingBearer(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postAutomationEnvVars(t, rig, session.ID.String(), "", "1")
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestAutomationEnvVarsDelivery_InvalidBearer(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postAutomationEnvVars(t, rig, session.ID.String(), "totally-wrong-token", "1")
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestAutomationEnvVarsDelivery_UnknownSession(t *testing.T) {
	rig := newTestRig(t)

	status, _ := postAutomationEnvVars(t, rig, "11111111-1111-1111-1111-111111111111", "any-token", "1")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

func TestAutomationEnvVarsDelivery_MalformedSessionID(t *testing.T) {
	rig := newTestRig(t)

	status, _ := postAutomationEnvVars(t, rig, "not-a-uuid", "any-token", "1")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

func TestAutomationEnvVarsDelivery_DeadSandboxStatus(t *testing.T) {
	tests := []struct {
		name   string
		status sqlcgen.SandboxStatus
	}{
		{"stopped", sqlcgen.SandboxStatusStopped},
		{"failed", sqlcgen.SandboxStatusFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			session := rig.createSession(ctx, t)
			createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")
			moveSandboxStatus(ctx, t, rig, session.ID, tc.status)

			status, _ := postAutomationEnvVars(t, rig, session.ID.String(), "sandbox-bearer-token", "1")
			if status != http.StatusGone {
				t.Errorf("status = %d, want %d (dead sandbox status %s)", status, http.StatusGone, tc.status)
			}
		})
	}
}

func TestAutomationEnvVarsDelivery_GenMismatch_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token") // gen 1

	status, _ := postAutomationEnvVars(t, rig, session.ID.String(), "sandbox-bearer-token", "999")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (gen mismatch)", status, http.StatusForbidden)
	}
}

func TestAutomationEnvVarsDelivery_MissingGen_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postAutomationEnvVars(t, rig, session.ID.String(), "sandbox-bearer-token", "" /* no X-Sandbox-Gen header at all */)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (missing X-Sandbox-Gen header)", status, http.StatusForbidden)
	}
}

func TestAutomationEnvVarsDelivery_CorrectCurrentGen_Succeeds(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token") // gen 1
	bumped := bumpSandboxGen(ctx, t, rig, session.ID, "respawned-bearer-token")

	if bumped.Gen != 2 {
		t.Fatalf("bumped.Gen = %d, want 2", bumped.Gen)
	}

	status, _ := postAutomationEnvVars(t, rig, session.ID.String(), "respawned-bearer-token", "2")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (correct current gen)", status, http.StatusOK)
	}
}

// --- Resolution correctness ---

// TestAutomationEnvVarsDelivery_NonAutomationSession_EmptyMap is the
// exit criterion's own "non-automation sessions must be unaffected"
// assertion at this endpoint's own boundary: an ordinary session (no
// automation_runs row references it at all -- the overwhelming common
// case) gets exactly {}, never an error.
func TestAutomationEnvVarsDelivery_NonAutomationSession_EmptyMap(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postAutomationEnvVars(t, rig, session.ID.String(), "sandbox-bearer-token", "1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(got.EnvVars) != 0 {
		t.Errorf("EnvVars = %v, want empty", got.EnvVars)
	}
}

// TestAutomationEnvVarsDelivery_AutomationSessionNoEnvVars_EmptyMap is the
// exit criterion's own "an automation with no env vars" zero-value case:
// a session DOES belong to an automation run, but that automation's own
// env_vars column is `[]`.
func TestAutomationEnvVarsDelivery_AutomationSessionNoEnvVars_EmptyMap(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")
	createAutomationRunForSession(ctx, t, rig, owner.ID, session.ID, `[]`)

	status, got := postAutomationEnvVars(t, rig, session.ID.String(), "sandbox-bearer-token", "1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(got.EnvVars) != 0 {
		t.Errorf("EnvVars = %v, want empty", got.EnvVars)
	}
}

// TestAutomationEnvVarsDelivery_AutomationSession_Resolves is the direct,
// real-DB proof of the exit criterion's own "a value set on an automation
// is visible... inside that automation's session": a session created for
// an automation run resolves that automation's OWN configured env vars,
// including a deliberately empty value (§8 item 4's own "an empty string
// is a legitimate value" zero-value case).
func TestAutomationEnvVarsDelivery_AutomationSession_Resolves(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")
	createAutomationRunForSession(ctx, t, rig, owner.ID, session.ID,
		`[{"name":"TARGET_ENV","value":"staging"},{"name":"EMPTY_FLAG","value":""}]`)

	status, got := postAutomationEnvVars(t, rig, session.ID.String(), "sandbox-bearer-token", "1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.EnvVars["TARGET_ENV"] != "staging" {
		t.Errorf("EnvVars[TARGET_ENV] = %q, want %q", got.EnvVars["TARGET_ENV"], "staging")
	}
	if v, present := got.EnvVars["EMPTY_FLAG"]; !present || v != "" {
		t.Errorf("EnvVars[EMPTY_FLAG] = (present=%v, value=%q), want (true, \"\")", present, v)
	}
}

// TestAutomationEnvVarsDelivery_OtherSession_NotMatched proves this
// resolves PER SESSION, not per automation: a second session belonging to
// the SAME automation's run history never leaks the first session's own
// run linkage -- an unrelated, plain session sees nothing.
func TestAutomationEnvVarsDelivery_OtherSession_NotMatched(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	automationSession := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, automationSession.ID, "automation-bearer-token")
	createAutomationRunForSession(ctx, t, rig, owner.ID, automationSession.ID, `[{"name":"TARGET_ENV","value":"staging"}]`)

	otherSession := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, otherSession.ID, "other-bearer-token")

	status, got := postAutomationEnvVars(t, rig, otherSession.ID.String(), "other-bearer-token", "1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(got.EnvVars) != 0 {
		t.Errorf("EnvVars = %v, want empty (this session's own run history is unrelated)", got.EnvVars)
	}
}
