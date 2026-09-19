//go:build integration

// This file is Y5 audit fix's own required coverage: Build's own
// githubingress.Config.Automations/AutomationInvocations wiring (serve.go,
// around the GitHub webhook route) and linear.Deps.Automations/
// AutomationInvocations wiring (serve.go, around the Linear webhook route)
// are both correct and reached in production, but deleting either leaves
// the entire build AND integration suite green -- because in this
// codebase a nil Automations/AutomationInvocations store fails closed and
// silently: dispatchAutomationsBestEffort (both adapters) checks for nil
// and returns before doing anything, logging at most an Error line nothing
// here was watching. internal/adapters/inbound/github's and .../linear's
// own automation-dispatch integration tests do NOT catch this class of
// regression -- they construct githubingress.Config/linear.Deps directly,
// with Automations/AutomationInvocations wired by the TEST itself, so they
// prove those packages are correct GIVEN correct wiring, never that
// controlplane.Build's own composition-root code actually supplies it.
//
// So, per this finding's own instruction, this proof goes through the
// REAL production path: Build (the actual composition root) constructs
// app.Router, a correctly-signed webhook POST goes to it exactly like the
// route-table tests elsewhere in this file do, and the assertion is a real
// automation_invocations row -- unreachable if either wiring line is
// deleted, since dispatchAutomationsBestEffort would then return at its
// own nil check before ever calling automation.DispatchGitHubWebhookEvent/
// DispatchLinearWebhookEvent.
package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
)

// signGitHubWebhook mirrors GitHub's own "X-Hub-Signature-256: sha256=<hex>"
// scheme -- an exact copy of internal/adapters/inbound/github's own sign
// helper (handler_integration_test.go), duplicated here for the same
// reason every other package in this codebase keeps its own copy of this
// shape rather than importing an unexported test helper across a package
// boundary.
func signGitHubWebhook(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// signLinearWebhook mirrors Linear's own real signature scheme (hex
// HMAC-SHA256 over the raw body, no prefix) -- an exact copy of
// internal/adapters/inbound/linear's own signBody helper (webhook_
// integration_test.go).
func signLinearWebhook(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// countAutomationInvocationsForTest counts automation_invocations rows for
// automationID -- mirrors countAutomationInvocations, duplicated in every
// other package with an automation-dispatch integration test (internal/
// app/automation, internal/adapters/inbound/github, .../linear) for the
// same cross-package-boundary reason.
func countAutomationInvocationsForTest(t *testing.T, pool *pgxpool.Pool, automationID pgtype.UUID) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM automation_invocations WHERE automation_id = $1", automationID).Scan(&count); err != nil {
		t.Fatalf("count automation invocations: %v", err)
	}
	return count
}

// pullRequestLabeledBodyForTest builds a synthetic, real-shaped
// "pull_request" webhook payload with action="labeled" -- an exact copy of
// internal/adapters/inbound/github's own pullRequestLabeledBody
// (handler_integration_test.go), narrowed to the fields this test's own
// fixed fixture repo/branch/label actually need.
func pullRequestLabeledBodyForTest(repoFullName, cloneRepoName, cloneURL string, prNumber int, labelName string, senderID int64, senderLogin string) []byte {
	body, err := json.Marshal(map[string]any{
		"action": "labeled",
		"label":  map[string]any{"name": labelName},
		"sender": map[string]any{"id": senderID, "login": senderLogin},
		"pull_request": map[string]any{
			"number": prNumber,
			"head": map[string]any{
				"ref":  "feature-x",
				"sha":  "sha-wiring-proof-head",
				"repo": map[string]any{"name": cloneRepoName, "clone_url": cloneURL, "full_name": repoFullName},
			},
			"base": map[string]any{"ref": "main"},
		},
		"repository": map[string]any{"full_name": repoFullName, "default_branch": "main"},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// TestBuild_GitHubAutomationWiringReachesRealDispatch is Y5's own required
// proof for githubingress.Config.Automations/AutomationInvocations
// (serve.go, GitHub webhook route). See this file's own top doc comment
// for why this must go through Build's real app.Router rather than a
// directly-constructed githubingress.Config.
func TestBuild_GitHubAutomationWiringReachesRealDispatch(t *testing.T) {
	setRequiredEnv(t)

	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}

	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	const repoFullName = "acme/controlplane-wiring-proof"
	const cloneURL = "https://github.com/acme/controlplane-wiring-proof"

	automations := narvipg.NewAutomationStore(pool)
	reposJSON, err := json.Marshal([]domainautomation.Target{{Name: "repo", URL: cloneURL}})
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}
	triggerConfigJSON, err := json.Marshal(map[string]string{"event": "pull_request", "action": "labeled", "label": "automation:run"})
	if err != nil {
		t.Fatalf("marshal trigger config: %v", err)
	}
	auto, err := automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: "wiring proof: github", Repos: reposJSON, CreatedBy: pgtype.UUID{},
		TriggerType: sqlcgen.AutomationTriggerTypeGithub, TriggerConfig: triggerConfigJSON, EnvVars: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create github automation: %v", err)
	}

	// An authorized, linked sender -- see the D2/D-06 fail-closed gate this
	// path also runs (githubingress's own doc.go); an unlinked sender would
	// deny before ever reaching the wiring this test exists to prove.
	const senderID = int64(90000901)
	users := narvipg.NewUserStore(pool)
	identities := narvipg.NewIdentityStore(pool)
	senderUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: fmt.Sprintf("controlplane-wiring-sender-%d@example.com", senderID),
		DisplayName:  "Controlplane Wiring Test Sender",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	email := senderUser.PrimaryEmail
	if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: senderUser.ID, Provider: sqlcgen.IdentityProviderGithub,
		ExternalID: strconv.FormatInt(senderID, 10), Email: &email, EmailVerified: true,
		LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail,
	}); err != nil {
		t.Fatalf("create fixture github identity: %v", err)
	}

	body := pullRequestLabeledBodyForTest(repoFullName, "controlplane-wiring-proof", cloneURL, 42, "automation:run", senderID, "wiring-proof-sender")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signGitHubWebhook([]byte(cfg.GitHubWebhookSecret), body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "delivery-controlplane-wiring-proof-github-1")

	rec := httptest.NewRecorder()
	app.Router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := countAutomationInvocationsForTest(t, pool, auto.ID); got != 1 {
		t.Fatalf("automation_invocations for automation = %d, want 1 -- if this is 0, Build's own githubingress.Config.Automations/AutomationInvocations "+
			"wiring (serve.go) is missing: dispatchAutomationsBestEffort fails closed and silently on a nil store, so a dropped wiring line shows up "+
			"as exactly this, never a build/vet/lint failure", got)
	}
}

// TestBuild_LinearAutomationWiringReachesRealDispatch is Y5's own required
// proof for linear.Deps.Automations/AutomationInvocations (serve.go,
// Linear webhook route). See this file's own top doc comment for why this
// must go through Build's real app.Router rather than a directly-
// constructed linear.Deps.
func TestBuild_LinearAutomationWiringReachesRealDispatch(t *testing.T) {
	setRequiredEnv(t)

	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}

	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	const organizationID = "org-controlplane-wiring-proof"
	const cloneURL = "https://github.com/acme/controlplane-wiring-proof-linear"

	automations := narvipg.NewAutomationStore(pool)
	reposJSON, err := json.Marshal([]domainautomation.Target{{Name: "repo", URL: cloneURL}})
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}
	triggerConfigJSON, err := json.Marshal(map[string]string{"eventType": "Issue", "action": "create", "teamKey": "ENG", "organizationId": organizationID})
	if err != nil {
		t.Fatalf("marshal trigger config: %v", err)
	}
	auto, err := automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: "wiring proof: linear", Repos: reposJSON, CreatedBy: pgtype.UUID{},
		TriggerType: sqlcgen.AutomationTriggerTypeLinear, TriggerConfig: triggerConfigJSON, EnvVars: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create linear automation: %v", err)
	}

	// D9's own installation gate: the sending workspace must have a real
	// installation row, or dispatch denies before ever reaching the wiring
	// this test exists to prove.
	encrypted, err := platform.EncryptToken(cfg.TokenEncryptionKey, []byte("fake-access-token"))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	installations := narvipg.NewLinearInstallationStore(pool)
	if _, err := installations.Upsert(ctx, sqlcgen.UpsertLinearInstallationParams{
		OrganizationID: organizationID, AppUserID: "app-user-wiring-proof",
		AccessTokenEncrypted: encrypted, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("upsert linear installation: %v", err)
	}

	// D-06/D-07's own fail-closed actor gate: an authorized, linked
	// "user"-origin actor, or dispatch denies before ever reaching the
	// wiring this test exists to prove.
	const actorExternalID = "linear-controlplane-wiring-actor-1"
	users := narvipg.NewUserStore(pool)
	identities := narvipg.NewIdentityStore(pool)
	actorUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: actorExternalID + "@narvi-test.example.com",
		DisplayName:  "Controlplane Wiring Test Actor",
		Role:         sqlcgen.UserRoleMaintainer,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: actorUser.ID, Provider: sqlcgen.IdentityProviderLinear,
		ExternalID: actorExternalID, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("link fixture linear identity: %v", err)
	}

	body := []byte(fmt.Sprintf(`{
		"action": "create",
		"type": "Issue",
		"organizationId": %q,
		"actor": {"id": %q, "type": "user", "name": "Controlplane Wiring Test Actor"},
		"webhookTimestamp": %d,
		"data": {"team": {"key": "ENG"}}
	}`, organizationID, actorExternalID, time.Now().UnixMilli()))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/linear", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Linear-Event", "Issue")
	req.Header.Set("Linear-Delivery", "delivery-controlplane-wiring-proof-linear-1")
	req.Header.Set("Linear-Signature", signLinearWebhook([]byte(cfg.LinearWebhookSecret), body))

	rec := httptest.NewRecorder()
	app.Router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := countAutomationInvocationsForTest(t, pool, auto.ID); got != 1 {
		t.Fatalf("automation_invocations for automation = %d, want 1 -- if this is 0, Build's own linear.Deps.Automations/AutomationInvocations "+
			"wiring (serve.go) is missing: dispatchAutomationsBestEffort fails closed and silently on a nil store, so a dropped wiring line shows up "+
			"as exactly this, never a build/vet/lint failure", got)
	}
}
