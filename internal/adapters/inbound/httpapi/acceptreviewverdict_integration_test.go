//go:build integration

// Integration tests for AcceptReviewVerdict/RevokeReviewVerdictAcceptance
// ("human acceptance of a verdict the engine refuses", §21.1b) against a
// REAL Postgres instance -- reuses decisionInboxTestRig/
// newDecisionInboxTestRig (decisioninbox_integration_test.go) rather than
// building a second fixture, exactly like that file reuses testRig's own
// conventions without extending it.
package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
)

// TestAcceptReviewVerdict_HappyPath proves a maintainer+ can accept a
// refused verdict end to end over HTTP, and that the resulting row binds
// to the PR's CURRENT verdict, never one the client supplied.
func TestAcceptReviewVerdict_HappyPath(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	maintainer, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMaintainer)
	const repoFullName = "acme/accept-verdict-happy"
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, 501, "headsha501")

	record, hasVerdict, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: rig.reviewVerdicts}, repoFullName, 501)
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest: hasVerdict=%v err=%v", hasVerdict, err)
	}

	body, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName:  repoFullName,
		PrNumber:      501,
		Justification: "Accepted for the purposes of this test.",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var got restdtos.ReviewVerdictAcceptance
	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if got.VerdictId != record.ID {
		t.Errorf("VerdictId = %q, want the PR's own current verdict id %q -- the server must resolve this itself, never trust a client-supplied value (the request body carries no verdict id at all)", got.VerdictId, record.ID)
	}
	if got.RepoFullName != repoFullName || got.PrNumber != 501 {
		t.Errorf("repoFullName/prNumber = %s/%d, want %s/501", got.RepoFullName, got.PrNumber, repoFullName)
	}
	if got.Justification != "Accepted for the purposes of this test." {
		t.Errorf("Justification = %q, want the submitted text", got.Justification)
	}
	if got.AcceptedBy != maintainer.ID.String() {
		t.Errorf("AcceptedBy = %q, want the authenticated maintainer's own id %q", got.AcceptedBy, maintainer.ID.String())
	}
	if got.RevokedAt != nil {
		t.Errorf("RevokedAt = %v, want nil (a freshly-created acceptance)", got.RevokedAt)
	}

	// Auditable: an audit_log row exists for this action.
	entries, err := narvipg.NewAuditLogStore(rig.pool).List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "review_verdict.accept" && e.ResourceID == got.Id {
			found = true
		}
	}
	if !found {
		t.Error("no audit_log entry found for review_verdict.accept -- §21.1b requires an auditable acceptance")
	}
}

// TestAcceptReviewVerdict_Member_Returns403 pins §13.3 row 5's own
// admin/maintainer-only gate -- a member (or viewer) may never accept a
// refused verdict.
func TestAcceptReviewVerdict_Member_Returns403(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)
	const repoFullName = "acme/accept-verdict-member"
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, 502, "headsha502")

	body, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: repoFullName, PrNumber: 502, Justification: "Should never be accepted by a member.",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", body, nil, token)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (member role must be denied)", status, http.StatusForbidden)
	}
}

// TestAcceptReviewVerdict_NoVerdict_Returns409 proves a PR with no posted
// review verdict at all cannot be "accepted" -- there is nothing to
// accept.
func TestAcceptReviewVerdict_NoVerdict_Returns409(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMaintainer)

	body, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: "acme/accept-verdict-none", PrNumber: 999, Justification: "No verdict exists for this PR.",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", body, nil, token)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d", status, http.StatusConflict)
	}
}

// TestRevokeReviewVerdictAcceptance_HappyPathThenAlreadyRevoked proves
// revocation end to end, and its own guarded-UPDATE idempotency: a
// second revoke of the SAME acceptance is a 409, never a silent
// no-op that could overwrite who revoked it first.
func TestRevokeReviewVerdictAcceptance_HappyPathThenAlreadyRevoked(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, acceptToken := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMaintainer)
	revoker, revokeToken := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleAdmin)
	const repoFullName = "acme/revoke-verdict-happy"
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, 503, "headsha503")

	acceptBody, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: repoFullName, PrNumber: 503, Justification: "Accepted, then revoked by a DIFFERENT maintainer+ (admin), by design -- §21.1b: revocation is never restricted to the original acceptor.",
	})
	if err != nil {
		t.Fatalf("marshal accept request: %v", err)
	}
	var accepted restdtos.ReviewVerdictAcceptance
	if status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", acceptBody, &accepted, acceptToken); status != http.StatusCreated {
		t.Fatalf("accept status = %d, want %d", status, http.StatusCreated)
	}

	revokeBody, err := json.Marshal(restdtos.RevokeReviewVerdictAcceptanceRequest{
		RepoFullName: repoFullName, Id: accepted.Id,
	})
	if err != nil {
		t.Fatalf("marshal revoke request: %v", err)
	}

	var revoked restdtos.ReviewVerdictAcceptance
	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/revoke-verdict-acceptance", revokeBody, &revoked, revokeToken)
	if status != http.StatusOK {
		t.Fatalf("revoke status = %d, want %d", status, http.StatusOK)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("RevokedAt = nil after revoke, want non-nil")
	}
	if revoked.RevokedBy == nil || *revoked.RevokedBy != revoker.ID.String() {
		t.Errorf("RevokedBy = %v, want the revoking admin's own id %q", revoked.RevokedBy, revoker.ID.String())
	}

	// GetActiveAcceptance must no longer report this row as active.
	_, ok, err := appreviewverdict.GetActiveAcceptance(ctx, narvipg.NewReviewVerdictAcceptanceStore(rig.pool), repoFullName, 503)
	if err != nil {
		t.Fatalf("GetActiveAcceptance after revoke: error = %v, want nil", err)
	}
	if ok {
		t.Error("GetActiveAcceptance after revoke: ok = true, want false -- a revoked acceptance must no longer read as active")
	}

	// Re-revoke: 409, never a silent no-op.
	status = rig.doJSON(t, http.MethodPost, "/api/decision-inbox/revoke-verdict-acceptance", revokeBody, nil, revokeToken)
	if status != http.StatusConflict {
		t.Fatalf("second revoke status = %d, want %d (already revoked)", status, http.StatusConflict)
	}
}

// TestRevokeReviewVerdictAcceptance_UnknownID_Returns404 proves a
// nonexistent acceptance id 404s.
func TestRevokeReviewVerdictAcceptance_UnknownID_Returns404(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleAdmin)

	body, err := json.Marshal(restdtos.RevokeReviewVerdictAcceptanceRequest{
		RepoFullName: "acme/revoke-verdict-unknown", Id: "00000000-0000-0000-0000-000000000000",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/revoke-verdict-acceptance", body, nil, token)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
	}
}
