//go:build integration

// Integration tests for §12.2 item 9's own two composition-finding
// actions ("Block release / Acknowledge & ship", releasecompositiondecision.go),
// against a real Postgres instance -- sharing this package's own testRig
// (httpapi_integration_test.go) and createUserWithRole (planapprove_
// integration_test.go).
package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// blockReleaseComposition/acknowledgeReleaseComposition mirror doJSON's
// own "v may be nil" convention exactly (rig.doJSON's own doc comment):
// v MUST be passed as the untyped literal nil (never a nil-valued
// *restdtos.PostReleaseCompositionDecisionResponse -- a typed nil boxed
// into doJSON's own "v any" parameter is a non-nil interface value, which
// would make doJSON attempt to decode into a nil pointer and panic) for a
// test that only cares about the status code (an error-shaped body --
// {"error": "..."} -- would otherwise fail restdtos.
// PostReleaseCompositionDecisionResponse's own generated, required-
// fields-enforcing UnmarshalJSON), or &resp for a test that also asserts
// on the decoded 200 response body -- exactly the same two call shapes
// every other doJSON call site in this package already uses.
func blockReleaseComposition(t *testing.T, r testRig, sessionID, token string, v any) int {
	t.Helper()
	return r.doJSON(t, http.MethodPost, "/api/sessions/"+sessionID+"/release-manifest/block", nil, v, token)
}

func acknowledgeReleaseComposition(t *testing.T, r testRig, sessionID, token string, v any) int {
	t.Helper()
	return r.doJSON(t, http.MethodPost, "/api/sessions/"+sessionID+"/release-manifest/acknowledge", nil, v, token)
}

// createReviewedReleaseManifestCheck inserts a release manifest check row
// and immediately marks its composition review complete (a real finding
// posted) -- the fixture every test below that needs to actually decide
// on something starts from, mirroring createReleaseManifestCheck
// (releasecompositionfindings_integration_test.go) plus one further
// guarded UPDATE, the SAME write PostReleaseCompositionFindings itself
// performs.
func createReviewedReleaseManifestCheck(ctx context.Context, t *testing.T, r testRig, sessionID sqlcgen.Session) sqlcgen.ReleaseManifestCheck {
	t.Helper()
	check := createReleaseManifestCheck(ctx, t, r, sessionID.ID)
	updated, err := r.releaseManifestChecks.UpdateCompositionFindings(ctx, check.ID, []byte(`[{"kind":"conflict","detail":"PR #1 and #2 both add migration 42"}]`))
	if err != nil {
		t.Fatalf("UpdateCompositionFindings: %v", err)
	}
	return updated
}

func TestBlockReleaseComposition_NotFound(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	status := blockReleaseComposition(t, rig, "00000000-0000-0000-0000-000000000000", adminToken, nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

func TestBlockReleaseComposition_NoAuth_Unauthorized(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)

	status := blockReleaseComposition(t, rig, session.ID.String(), "", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

// TestBlockReleaseComposition_MemberDenied proves a plain member cannot
// block a release -- authz.ActionEditReviewVerdict is admin/maintainer
// only.
func TestBlockReleaseComposition_MemberDenied(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createReviewedReleaseManifestCheck(ctx, t, rig, session)

	_, memberToken := rig.createAuthenticatedUser(ctx, t)
	status := blockReleaseComposition(t, rig, session.ID.String(), memberToken, nil)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestBlockReleaseComposition_MaintainerAllowed proves a maintainer CAN
// block -- authz.ActionEditReviewVerdict's own row (§13.3 row 5), unlike
// AcknowledgeReleaseComposition's stricter admin-only row.
func TestBlockReleaseComposition_MaintainerAllowed(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createReviewedReleaseManifestCheck(ctx, t, rig, session)

	_, maintainerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
	var resp restdtos.PostReleaseCompositionDecisionResponse
	status := blockReleaseComposition(t, rig, session.ID.String(), maintainerToken, &resp)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if resp.CompositionDecision != restdtos.PostReleaseCompositionDecisionResponseCompositionDecisionBlocked {
		t.Errorf("CompositionDecision = %q, want %q", resp.CompositionDecision, restdtos.PostReleaseCompositionDecisionResponseCompositionDecisionBlocked)
	}
	if resp.CompositionDecisionAt.IsZero() {
		t.Error("CompositionDecisionAt is zero, want a real timestamp")
	}
}

// TestBlockReleaseComposition_CompositionNotYetReviewed_BadRequest proves
// the §15.3 "not yet available" sentinel is enforced here too: a release
// whose composition review has not completed (composition_reviewed_at
// still NULL) has nothing to decide on yet.
func TestBlockReleaseComposition_CompositionNotYetReviewed_BadRequest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createReleaseManifestCheck(ctx, t, rig, session.ID) // composition_reviewed_at left NULL

	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	status := blockReleaseComposition(t, rig, session.ID.String(), adminToken, nil)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestBlockReleaseComposition_NoReleaseManifestCheck_BadRequest proves a
// session with no release manifest check at all also 400s.
func TestBlockReleaseComposition_NoReleaseManifestCheck_BadRequest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)

	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	status := blockReleaseComposition(t, rig, session.ID.String(), adminToken, nil)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestBlockReleaseComposition_AlreadyDecided_Conflict proves the
// transition table's own terminal-state enforcement: a SECOND decision
// attempt against an already-blocked release is rejected, never silently
// re-applied or overwritten with a different one.
func TestBlockReleaseComposition_AlreadyDecided_Conflict(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createReviewedReleaseManifestCheck(ctx, t, rig, session)

	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	status := blockReleaseComposition(t, rig, session.ID.String(), adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("first block status = %d, want %d", status, http.StatusOK)
	}

	status = blockReleaseComposition(t, rig, session.ID.String(), adminToken, nil)
	if status != http.StatusConflict {
		t.Errorf("second block status = %d, want %d", status, http.StatusConflict)
	}

	// The OTHER action, against the same already-decided release, must
	// also be rejected -- Blocked is terminal against BOTH actions, never
	// just the one that produced it.
	status = acknowledgeReleaseComposition(t, rig, session.ID.String(), adminToken, nil)
	if status != http.StatusConflict {
		t.Errorf("acknowledge-after-block status = %d, want %d", status, http.StatusConflict)
	}
}

// TestAcknowledgeReleaseComposition_MaintainerDenied proves the stricter,
// admin-only row: a maintainer (who CAN block) cannot acknowledge -- the
// override direction is reserved for admin per authz.
// ActionAcknowledgeReleaseComposition's own doc comment.
func TestAcknowledgeReleaseComposition_MaintainerDenied(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createReviewedReleaseManifestCheck(ctx, t, rig, session)

	_, maintainerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
	status := acknowledgeReleaseComposition(t, rig, session.ID.String(), maintainerToken, nil)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestAcknowledgeReleaseComposition_AdminAllowed proves the admin-only
// row's own positive case: an admin CAN acknowledge and ship despite a
// composition finding.
func TestAcknowledgeReleaseComposition_AdminAllowed(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createReviewedReleaseManifestCheck(ctx, t, rig, session)

	admin, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	var resp restdtos.PostReleaseCompositionDecisionResponse
	status := acknowledgeReleaseComposition(t, rig, session.ID.String(), adminToken, &resp)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if resp.CompositionDecision != restdtos.PostReleaseCompositionDecisionResponseCompositionDecisionAcknowledged {
		t.Errorf("CompositionDecision = %q, want %q", resp.CompositionDecision, restdtos.PostReleaseCompositionDecisionResponseCompositionDecisionAcknowledged)
	}
	if resp.CompositionDecisionBy != admin.ID.String() {
		t.Errorf("CompositionDecisionBy = %q, want %q", resp.CompositionDecisionBy, admin.ID.String())
	}

	// The decision is durably persisted, not just reflected in the
	// response.
	updated, err := rig.releaseManifestChecks.GetBySessionID(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetBySessionID: %v", err)
	}
	if updated.CompositionDecision != "acknowledged" {
		t.Errorf("persisted composition_decision = %q, want %q", updated.CompositionDecision, "acknowledged")
	}
}
