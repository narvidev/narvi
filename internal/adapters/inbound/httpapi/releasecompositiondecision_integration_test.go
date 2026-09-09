//go:build integration

// Integration tests for §12.2 item 9's own two composition-finding
// actions ("Block release / Acknowledge & ship", releasecompositiondecision.go),
// against a real Postgres instance -- sharing this package's own testRig
// (httpapi_integration_test.go) and createUserWithRole (planapprove_
// integration_test.go).
package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

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

// TestUpdateCompositionDecision_GuardedCAS_ConcurrentDecisionsRace proves
// the guarded compare-and-swap on release_manifest_checks.
// composition_decision is a REAL race guard, not merely redundant with
// the domain-layer TransitionCompositionDecision check that already runs
// (once, sequentially) before every HTTP call reaches it. Test-integrity
// fix: this exact scenario was previously entirely untested -- removing
// the CAS predicate ("AND composition_decision = sqlc.arg(expected_
// decision)") from UpdateReleaseManifestCompositionDecision (queries/
// releasemanifestchecks.sql) left all of this package's other 19
// composition-decision integration tests green, because every one of
// them only ever calls Block/Acknowledge SEQUENTIALLY (the second call
// always observes the FIRST call's own already-updated row via the
// domain-layer check, well before the SQL UPDATE is ever reached a
// second time).
//
// This test instead calls ReleaseManifestCheckStore.UpdateCompositionDecision
// directly, TWICE, both times with the SAME expectedCurrent="pending" --
// simulating two truly concurrent callers who both read "pending" before
// EITHER one's own write landed (exactly the race window between two
// simultaneous Block/Acknowledge HTTP requests that the application-layer
// check alone cannot close: each request's own in-memory "current" read
// cannot see the other request's concurrent write).
func TestUpdateCompositionDecision_GuardedCAS_ConcurrentDecisionsRace(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	check := createReviewedReleaseManifestCheck(ctx, t, rig, session)

	adminA, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	adminB, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	first, err := rig.releaseManifestChecks.UpdateCompositionDecision(ctx, check.ID, "pending", "blocked", adminA.ID)
	if err != nil {
		t.Fatalf("first UpdateCompositionDecision: %v", err)
	}
	if first.CompositionDecision != "blocked" {
		t.Fatalf("first winner's own decision = %q, want %q", first.CompositionDecision, "blocked")
	}

	// The SAME expectedCurrent ("pending") the first call also used --
	// this is the race: without the CAS predicate this second call would
	// ALSO match (id alone) and silently overwrite adminA's own decision.
	if _, err := rig.releaseManifestChecks.UpdateCompositionDecision(ctx, check.ID, "pending", "acknowledged", adminB.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second (losing) UpdateCompositionDecision error = %v, want pgx.ErrNoRows (the CAS predicate should have matched zero rows)", err)
	}

	// The row's own real, current state must be the FIRST call's own
	// decision -- never silently overwritten by the second, losing call.
	final, err := rig.releaseManifestChecks.GetBySessionID(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetBySessionID: %v", err)
	}
	if final.CompositionDecision != "blocked" {
		t.Errorf("final persisted composition_decision = %q, want %q (the second call must never have won)", final.CompositionDecision, "blocked")
	}
	if final.CompositionDecisionBy != adminA.ID {
		t.Errorf("final persisted composition_decision_by = %v, want %v (adminA, the actual winner)", final.CompositionDecisionBy, adminA.ID)
	}
}

func unblockReleaseComposition(t *testing.T, r testRig, sessionID, token string, v any) int {
	t.Helper()
	return r.doJSON(t, http.MethodPost, "/api/sessions/"+sessionID+"/release-manifest/unblock", nil, v, token)
}

// TestUnblockReleaseComposition_MaintainerDenied proves the confirmed-major
// fix's own stricter row: a maintainer CAN block (authz.
// ActionEditReviewVerdict) but cannot unblock -- Unblock is gated on the
// SAME admin-only row as Acknowledge & ship (authz.
// ActionUnblockReleaseComposition).
func TestUnblockReleaseComposition_MaintainerDenied(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createReviewedReleaseManifestCheck(ctx, t, rig, session)

	_, maintainerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
	status := blockReleaseComposition(t, rig, session.ID.String(), maintainerToken, nil)
	if status != http.StatusOK {
		t.Fatalf("block status = %d, want %d", status, http.StatusOK)
	}

	status = unblockReleaseComposition(t, rig, session.ID.String(), maintainerToken, nil)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestUnblockReleaseComposition_NotBlocked_Conflict proves the transition
// table's own guard: Unblock has no legal transition out of Pending (or
// Acknowledged) -- there is nothing to unblock.
func TestUnblockReleaseComposition_NotBlocked_Conflict(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createReviewedReleaseManifestCheck(ctx, t, rig, session)

	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	status := unblockReleaseComposition(t, rig, session.ID.String(), adminToken, nil)
	if status != http.StatusConflict {
		t.Errorf("status = %d, want %d (nothing blocked yet)", status, http.StatusConflict)
	}
}

// TestUnblockReleaseComposition_AdminAllowed_ReopensToPendingThenAcknowledgeSucceeds
// is the confirmed-major fix's own end-to-end proof: a maintainer's Block
// used to be terminal, permanently voiding the admin-only Acknowledge &
// ship for that release. This proves the full repaired sequence -- block,
// unblock (admin), THEN acknowledge (admin) -- succeeds all the way
// through, where before this fix the final Acknowledge call would have
// 409'd forever.
func TestUnblockReleaseComposition_AdminAllowed_ReopensToPendingThenAcknowledgeSucceeds(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	createReviewedReleaseManifestCheck(ctx, t, rig, session)

	_, maintainerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
	admin, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	if status := blockReleaseComposition(t, rig, session.ID.String(), maintainerToken, nil); status != http.StatusOK {
		t.Fatalf("block status = %d, want %d", status, http.StatusOK)
	}

	var unblockResp restdtos.PostReleaseCompositionDecisionResponse
	status := unblockReleaseComposition(t, rig, session.ID.String(), adminToken, &unblockResp)
	if status != http.StatusOK {
		t.Fatalf("unblock status = %d, want %d", status, http.StatusOK)
	}
	if unblockResp.CompositionDecision != restdtos.PostReleaseCompositionDecisionResponseCompositionDecisionPending {
		t.Errorf("unblock CompositionDecision = %q, want %q", unblockResp.CompositionDecision, restdtos.PostReleaseCompositionDecisionResponseCompositionDecisionPending)
	}
	if unblockResp.CompositionDecisionBy != admin.ID.String() {
		t.Errorf("unblock CompositionDecisionBy = %q, want %q", unblockResp.CompositionDecisionBy, admin.ID.String())
	}

	// The previously-permanently-voided admin override is reachable again.
	var ackResp restdtos.PostReleaseCompositionDecisionResponse
	status = acknowledgeReleaseComposition(t, rig, session.ID.String(), adminToken, &ackResp)
	if status != http.StatusOK {
		t.Fatalf("acknowledge-after-unblock status = %d, want %d (this is the confirmed-major fix's own end-to-end proof)", status, http.StatusOK)
	}
	if ackResp.CompositionDecision != restdtos.PostReleaseCompositionDecisionResponseCompositionDecisionAcknowledged {
		t.Errorf("post-unblock acknowledge CompositionDecision = %q, want %q", ackResp.CompositionDecision, restdtos.PostReleaseCompositionDecisionResponseCompositionDecisionAcknowledged)
	}
}

// TestBlockReleaseComposition_RecordsAuditLog proves the decision write
// and its own audit_log row (M5, confirmed-major transactional-audit fix)
// both land -- mirrors TestCreateCloudIdentityBinding_RecordsAuditLog's
// own established "list audit_log, find the expected row" precedent
// (cloudidentitybindings_integration_test.go). This test-integrity gap
// (this exact endpoint previously asserted NOTHING about audit_log at
// all) is closed here, not merely the transaction wiring itself.
func TestBlockReleaseComposition_RecordsAuditLog(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	check := createReviewedReleaseManifestCheck(ctx, t, rig, session)

	admin, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	status := blockReleaseComposition(t, rig, session.ID.String(), adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	entries, err := rig.auditLog.List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Action == "release_manifest_check.block" && e.ResourceType == "release_manifest_check" && e.ResourceID == check.ID.String() {
			found = true
			if e.ActorUserID != admin.ID {
				t.Errorf("audit log actor_user_id = %v, want %v", e.ActorUserID, admin.ID)
			}
			break
		}
	}
	if !found {
		t.Errorf("no release_manifest_check.block audit log entry found for resource %s", check.ID.String())
	}
}

