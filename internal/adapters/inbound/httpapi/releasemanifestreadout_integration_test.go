//go:build integration

// Integration tests for §15.2/§15.3/§12.2 item 9's own release-review
// read model (releasemanifestreadout.go): GET
// /api/sessions/{sessionID}/release-manifest, against a real Postgres
// instance -- gated behind the "integration" build tag, sharing this
// package's own testRig (httpapi_integration_test.go) and
// createOwnedGitHubReviewSession (reviewretrigger_integration_test.go).
//
// Before this file, this route had ZERO integration coverage of its own
// -- confirmed by grep before writing this file, mirroring
// reviewreadout_integration_test.go's own identical "this file's own
// first two tests pin this handler's own pre-existing baseline behavior
// for the first time" precedent. This is exactly the blocking finding an
// adversarial review of this branch caught: the composition-review
// fields (compositionReviewedAt/compositionFindings/compositionDecision/
// compositionDecisionBy/compositionDecisionAt) were computed and
// persisted by the rest of this branch's own work but never read back
// out by this handler -- and no test anywhere would have caught that,
// because no test exercised this handler at all.
package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

func TestGetReleaseManifestReadout_NotFound(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodGet, "/api/sessions/00000000-0000-0000-0000-000000000000/release-manifest", nil, nil, token)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestGetReleaseManifestReadout_NoGitHubPRMapping_BadRequest proves an
// ordinary, non-GitHub session (no github_pr_sessions row at all) is
// rejected 400 -- there is no PR to read a release manifest for.
func TestGetReleaseManifestReadout_NoGitHubPRMapping_BadRequest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)

	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}

	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/release-manifest", nil, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (no github_pr_sessions row for this session)", status, http.StatusBadRequest)
	}
}

// TestGetReleaseManifestReadout_NotComputed_HonestDefaults proves the
// "never computed for this PR" state (this handler's own top doc
// comment): computed stays false, and -- the closed-enum discipline this
// Step's own blocking-finding fix depends on -- compositionDecision is
// still a LEGAL enum member ("pending", never the Go zero value "") and
// compositionFindings is a present, empty array (never JSON null), even
// though no release_manifest_checks row exists at all yet.
func TestGetReleaseManifestReadout_NotComputed_HonestDefaults(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/readout-not-computed", 1)

	raw := map[string]any{}
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/release-manifest", nil, &raw, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if raw["computed"] != false {
		t.Errorf("computed = %v, want false", raw["computed"])
	}
	if raw["compositionDecision"] != "pending" {
		t.Errorf("compositionDecision = %v, want %q (never the empty-string zero value)", raw["compositionDecision"], "pending")
	}
	findings, ok := raw["compositionFindings"].([]any)
	if !ok {
		t.Fatalf("compositionFindings = %v (%T), want a present JSON array, never null", raw["compositionFindings"], raw["compositionFindings"])
	}
	if len(findings) != 0 {
		t.Errorf("compositionFindings = %v, want empty", findings)
	}
	if _, present := raw["compositionReviewedAt"]; present && raw["compositionReviewedAt"] != nil {
		t.Errorf("compositionReviewedAt = %v, want null/absent (never computed)", raw["compositionReviewedAt"])
	}

	// Also decode into the generated, contract-enforcing DTO -- its own
	// UnmarshalJSON rejects a missing required field or an out-of-enum
	// compositionDecision outright, so a successful decode here is itself
	// part of the proof.
	var typed restdtos.ReleaseManifestReadout
	status = rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/release-manifest", nil, &typed, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if typed.CompositionDecision != restdtos.ReleaseManifestReadoutCompositionDecisionPending {
		t.Errorf("typed CompositionDecision = %q, want %q", typed.CompositionDecision, restdtos.ReleaseManifestReadoutCompositionDecisionPending)
	}
	if typed.CompositionFindings == nil {
		t.Error("typed CompositionFindings is nil, want a non-nil (possibly empty) slice")
	}
}

// TestGetReleaseManifestReadout_CompositionReviewed_PopulatesAllFields is
// THE blocking-finding regression test: with composition_reviewed_at set,
// one real finding stored, and composition_decision still 'pending' (the
// exact real-Postgres state the adversarial review reproduced this
// finding against), the readout must actually surface all five
// composition-review fields -- not silently stop at
// aggregateReviewTriggered the way this handler used to.
func TestGetReleaseManifestReadout_CompositionReviewed_PopulatesAllFields(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	const repoFullName = "acme/readout-composition-reviewed"
	const prNumber = 42
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, repoFullName, prNumber)

	check, err := rig.releaseManifestChecks.Insert(ctx, sqlcgen.InsertReleaseManifestCheckParams{
		SessionID:                     session.ID,
		RepoFullName:                  repoFullName,
		PrNumber:                      prNumber,
		BaseRef:                       "main",
		HeadRef:                       "release/2026.09",
		ConstituentPrCount:            3,
		AggregateReviewTriggered:      true,
		AggregateReviewTriggerReasons: []byte(`["a high-risk pull request is included in this release"]`),
		Findings:                      []byte(`[]`),
		MergedPrs:                     []byte(`[]`),
	})
	if err != nil {
		t.Fatalf("insert release manifest check: %v", err)
	}
	if _, err := rig.releaseManifestChecks.UpdateCompositionFindings(ctx, check.ID,
		[]byte(`[{"kind":"conflict","detail":"PR #1 and #2 both add migration 42"}]`)); err != nil {
		t.Fatalf("UpdateCompositionFindings: %v", err)
	}
	if _, err := rig.releaseManifestChecks.UpdateCompositionAnchor(ctx, check.ID, "deadbeefcafe", true); err != nil {
		t.Fatalf("UpdateCompositionAnchor: %v", err)
	}

	// Raw JSON assertion first -- reproduces the adversarial review's own
	// exact repro: a null/absent compositionReviewedAt or a null
	// compositionFindings here is the bug this Step exists to fix.
	raw := map[string]any{}
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/release-manifest", nil, &raw, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if raw["compositionReviewedAt"] == nil {
		t.Error("compositionReviewedAt is null -- the composition review's own completion is invisible to the readout")
	}
	findings, ok := raw["compositionFindings"].([]any)
	if !ok || len(findings) != 1 {
		t.Fatalf("compositionFindings = %v (%T), want a one-element array", raw["compositionFindings"], raw["compositionFindings"])
	}
	if raw["compositionDecision"] != "pending" {
		t.Errorf("compositionDecision = %v, want %q", raw["compositionDecision"], "pending")
	}
	if raw["compositionHeadSha"] != "deadbeefcafe" {
		t.Errorf("compositionHeadSha = %v, want %q", raw["compositionHeadSha"], "deadbeefcafe")
	}
	if raw["compositionDiffTruncated"] != true {
		t.Errorf("compositionDiffTruncated = %v, want true", raw["compositionDiffTruncated"])
	}

	// Typed decode, asserting the actual finding content survived the
	// round trip.
	var typed restdtos.ReleaseManifestReadout
	status = rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/release-manifest", nil, &typed, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if typed.CompositionReviewedAt == nil {
		t.Fatal("typed CompositionReviewedAt is nil")
	}
	if len(typed.CompositionFindings) != 1 {
		t.Fatalf("typed CompositionFindings has %d elements, want 1", len(typed.CompositionFindings))
	}
	if typed.CompositionFindings[0].Kind != restdtos.ReleaseCompositionFindingKindConflict {
		t.Errorf("finding kind = %q, want %q", typed.CompositionFindings[0].Kind, restdtos.ReleaseCompositionFindingKindConflict)
	}
	if typed.CompositionFindings[0].Detail != "PR #1 and #2 both add migration 42" {
		t.Errorf("finding detail = %q, want the real persisted text", typed.CompositionFindings[0].Detail)
	}
	if typed.CompositionDecision != restdtos.ReleaseManifestReadoutCompositionDecisionPending {
		t.Errorf("typed CompositionDecision = %q, want %q", typed.CompositionDecision, restdtos.ReleaseManifestReadoutCompositionDecisionPending)
	}
}

// TestGetReleaseManifestReadout_CompositionDecided_PopulatesDecisionFields
// proves the readout also surfaces a REAL human decision (Block/
// Acknowledge/Unblock, §12.2 item 9) once one has been rendered -- not
// just the pass's own findings.
func TestGetReleaseManifestReadout_CompositionDecided_PopulatesDecisionFields(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	const repoFullName = "acme/readout-composition-decided"
	const prNumber = 7
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, repoFullName, prNumber)

	check, err := rig.releaseManifestChecks.Insert(ctx, sqlcgen.InsertReleaseManifestCheckParams{
		SessionID: session.ID, RepoFullName: repoFullName, PrNumber: prNumber,
		BaseRef: "main", HeadRef: "release/2026.09", ConstituentPrCount: 1,
		AggregateReviewTriggered: true, AggregateReviewTriggerReasons: []byte(`[]`),
		Findings: []byte(`[]`), MergedPrs: []byte(`[]`),
	})
	if err != nil {
		t.Fatalf("insert release manifest check: %v", err)
	}
	if _, err := rig.releaseManifestChecks.UpdateCompositionFindings(ctx, check.ID, []byte(`[]`)); err != nil {
		t.Fatalf("UpdateCompositionFindings: %v", err)
	}
	admin, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	if status := acknowledgeReleaseComposition(t, rig, session.ID.String(), adminToken, nil); status != http.StatusOK {
		t.Fatalf("acknowledge status = %d, want %d", status, http.StatusOK)
	}

	var typed restdtos.ReleaseManifestReadout
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/release-manifest", nil, &typed, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if typed.CompositionDecision != restdtos.ReleaseManifestReadoutCompositionDecisionAcknowledged {
		t.Errorf("CompositionDecision = %q, want %q", typed.CompositionDecision, restdtos.ReleaseManifestReadoutCompositionDecisionAcknowledged)
	}
	if typed.CompositionDecisionBy == nil || *typed.CompositionDecisionBy != admin.ID.String() {
		t.Errorf("CompositionDecisionBy = %v, want %q", typed.CompositionDecisionBy, admin.ID.String())
	}
	if typed.CompositionDecisionAt == nil {
		t.Error("CompositionDecisionAt is nil, want a real timestamp")
	}
}
