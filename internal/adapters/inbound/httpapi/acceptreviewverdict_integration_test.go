//go:build integration

// Integration tests for AcceptReviewVerdict/RevokeReviewVerdictAcceptance
// ("human acceptance of a verdict the engine refuses", §21.1b) against a
// REAL Postgres instance -- reuses decisionInboxTestRig/
// newDecisionInboxTestRig (decisioninbox_integration_test.go) rather than
// building a second fixture, exactly like that file reuses testRig's own
// conventions without extending it.
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/platform"
)

// TestAcceptReviewVerdict_HappyPath proves a maintainer+ can accept a
// refused verdict end to end over HTTP, naming the exact verdict it read
// (finding F3, adversarial review), and that the resulting row binds to
// that SAME verdict -- the server confirms it is still the PR's current
// one, never silently substitutes a different one the client never named.
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
		VerdictId:     record.ID,
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
		t.Errorf("VerdictId = %q, want the verdict id the request named %q", got.VerdictId, record.ID)
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

// TestAcceptReviewVerdict_Supersede_RecordsDistinctAuditRow pins finding
// F4 (adversarial review) end to end over HTTP: a SECOND accept for the
// SAME pull request supersedes the first (Accept's own doc comment), and
// that supersession must leave its OWN audited fact --
// review_verdict.accept_supersedes_prior, naming the superseded
// acceptance's own id -- never just a second review_verdict.accept entry
// that makes the first acceptance look like it simply vanished. Before
// this fix, the ONLY audit row was review_verdict.accept by the second
// accepter, whose detail never named the first acceptance at all.
func TestAcceptReviewVerdict_Supersede_RecordsDistinctAuditRow(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMaintainer)
	const repoFullName = "acme/accept-verdict-supersede-audit"
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, 509, "headsha509")

	record, hasVerdict, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: rig.reviewVerdicts}, repoFullName, 509)
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest: hasVerdict=%v err=%v", hasVerdict, err)
	}

	firstBody, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: repoFullName, PrNumber: 509, VerdictId: record.ID, Justification: "First acceptance -- about to be superseded.",
	})
	if err != nil {
		t.Fatalf("marshal first request: %v", err)
	}
	var first restdtos.ReviewVerdictAcceptance
	if status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", firstBody, &first, token); status != http.StatusCreated {
		t.Fatalf("first accept status = %d, want %d", status, http.StatusCreated)
	}

	secondBody, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: repoFullName, PrNumber: 509, VerdictId: record.ID, Justification: "Second acceptance -- supersedes the first.",
	})
	if err != nil {
		t.Fatalf("marshal second request: %v", err)
	}
	var second restdtos.ReviewVerdictAcceptance
	if status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", secondBody, &second, token); status != http.StatusCreated {
		t.Fatalf("second accept status = %d, want %d", status, http.StatusCreated)
	}
	if second.Id == first.Id {
		t.Fatalf("second accept returned the SAME id as the first (%s) -- Accept must always insert a NEW row", first.Id)
	}

	entries, err := narvipg.NewAuditLogStore(rig.pool).List(ctx, 20, 0)
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}

	acceptCount := 0
	foundSupersede := false
	for _, e := range entries {
		switch e.Action {
		case "review_verdict.accept":
			if e.ResourceID == first.Id || e.ResourceID == second.Id {
				acceptCount++
			}
		case "review_verdict.accept_supersedes_prior":
			// round 3, finding R6 (adversarial review): this row is now
			// filed under the SUPERSEDED acceptance's own id (first.Id),
			// never the new one -- symmetric with an explicit revoke,
			// which is filed under the id of the acceptance IT revoked.
			// An audit lookup keyed on first.Id must find this row;
			// detail.SupersedingAcceptanceID is what still lets a reader
			// starting from the NEW acceptance's own id (second.Id) find
			// what it superseded.
			var detail struct {
				SupersedingAcceptanceID string `json:"superseding_acceptance_id"`
			}
			if err := json.Unmarshal(e.DetailJson, &detail); err != nil {
				t.Fatalf("unmarshal accept_supersedes_prior detail: %v", err)
			}
			if e.ResourceID == first.Id && detail.SupersedingAcceptanceID == second.Id {
				foundSupersede = true
			}
		}
	}
	if acceptCount != 2 {
		t.Errorf("review_verdict.accept audit rows for this test's own two acceptances = %d, want 2", acceptCount)
	}
	if !foundSupersede {
		t.Error("no review_verdict.accept_supersedes_prior audit row found filed under the superseded (first) acceptance's own id and naming the superseding (second) one -- finding F4/R6, adversarial review: a supersession must not vanish from the audit trail, and must be findable from the row it revoked, exactly like an explicit revoke already is")
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
// accept. VerdictId is a syntactically-valid but unrelated UUID: this
// test's own intent is "no verdict on record", never "verdictId missing",
// and it must reach GetLatest's own !hasVerdict branch rather than being
// masked by the earlier required-field check.
func TestAcceptReviewVerdict_NoVerdict_Returns409(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMaintainer)

	body, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: "acme/accept-verdict-none", PrNumber: 999, VerdictId: "00000000-0000-0000-0000-000000000000", Justification: "No verdict exists for this PR.",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", body, nil, token)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d", status, http.StatusConflict)
	}
}

// TestAcceptReviewVerdict_VerdictIDMismatch_Returns409 pins finding F3
// (adversarial review) directly: a caller naming a DIFFERENT verdict id
// than (repoFullName, prNumber)'s own current latest verdict must be
// refused, never silently bound to whatever is latest now -- the whole
// point of requiring the client to name which verdict it read. Deleting
// the mismatch check in httpapi.AcceptReviewVerdict (comparing
// record.ID against req.VerdictId) must make this test fail.
func TestAcceptReviewVerdict_VerdictIDMismatch_Returns409(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMaintainer)
	const repoFullName = "acme/accept-verdict-mismatch"
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, 504, "headsha504")

	record, hasVerdict, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: rig.reviewVerdicts}, repoFullName, 504)
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest: hasVerdict=%v err=%v", hasVerdict, err)
	}

	// A re-triggered review posts a SECOND verdict at the same PR -- the
	// caller above read the FIRST one (record.ID) and is about to accept
	// it, unaware a fresh attempt has already superseded it.
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, 504, "headsha504-v2")

	body, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName:  repoFullName,
		PrNumber:      504,
		VerdictId:     record.ID,
		Justification: "Accepting what I read -- but it is no longer current.",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", body, nil, token)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d -- a stale verdictId must be refused, never silently bound to whatever verdict is latest now", status, http.StatusConflict)
	}
}

// TestAcceptReviewVerdict_MissingVerdictID_Returns400 proves verdictId is
// a required field -- an omitted verdict id must never silently resolve
// to "whatever is current now" (finding F3's own contract: only the
// client that read a specific verdict may name it).
func TestAcceptReviewVerdict_MissingVerdictID_Returns400(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMaintainer)
	const repoFullName = "acme/accept-verdict-missing-id"
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, 505, "headsha505")

	body, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: repoFullName, PrNumber: 505, Justification: "No verdictId supplied.",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", body, nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestAcceptReviewVerdict_MissingJustification_Returns400 pins finding
// F3b (adversarial review): justification is a required field -- the
// `if justification == "" { 400 }` check (decisioninbox.go) had NO test
// of its own before this fix, and could be deleted with the whole suite
// still green. An omitted justification must never silently record an
// acceptance with an empty explanation (§21.1b: "carries author,
// justification").
func TestAcceptReviewVerdict_MissingJustification_Returns400(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMaintainer)
	const repoFullName = "acme/accept-verdict-missing-justification"
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, 506, "headsha506")

	record, hasVerdict, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: rig.reviewVerdicts}, repoFullName, 506)
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest: hasVerdict=%v err=%v", hasVerdict, err)
	}

	body, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: repoFullName, PrNumber: 506, VerdictId: record.ID,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", body, nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}

	// No acceptance must have been recorded.
	_, ok, err := appreviewverdict.GetActiveAcceptance(ctx, narvipg.NewReviewVerdictAcceptanceStore(rig.pool), repoFullName, 506)
	if err != nil {
		t.Fatalf("GetActiveAcceptance: error = %v, want nil", err)
	}
	if ok {
		t.Error("GetActiveAcceptance: ok = true, want false -- a request with no justification must never record an acceptance")
	}
}

// TestAcceptReviewVerdict_JustificationTooLong_Returns400 pins T8 (round
// 4, adversarial review): before this fix, the ONLY limit on
// Justification's own length was maxRequestBodyBytes' own 1 MiB
// whole-request cap (helpers.go) -- this test proves a justification well
// under that cap, but over maxAcceptReviewVerdictJustificationChars, is
// STILL refused with a typed 400, and that no acceptance is recorded.
//
// Mutation-test target: deleting the
// `utf8.RuneCountInString(justification) >
// maxAcceptReviewVerdictJustificationChars` check (decisioninbox.go) must
// turn this test's own 400 assertion into a failure (the request would
// succeed, status 201).
func TestAcceptReviewVerdict_JustificationTooLong_Returns400(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMaintainer)
	const repoFullName = "acme/accept-verdict-justification-too-long"
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, 507, "headsha507")

	record, hasVerdict, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: rig.reviewVerdicts}, repoFullName, 507)
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest: hasVerdict=%v err=%v", hasVerdict, err)
	}

	// One rune over the bound -- the exact boundary this check must
	// refuse on, not merely some arbitrarily larger value.
	tooLong := strings.Repeat("a", 4001)
	body, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: repoFullName, PrNumber: 507, VerdictId: record.ID, Justification: tooLong,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", body, nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}

	// No acceptance must have been recorded.
	_, ok, err := appreviewverdict.GetActiveAcceptance(ctx, narvipg.NewReviewVerdictAcceptanceStore(rig.pool), repoFullName, 507)
	if err != nil {
		t.Fatalf("GetActiveAcceptance: error = %v, want nil", err)
	}
	if ok {
		t.Error("GetActiveAcceptance: ok = true, want false -- an over-long justification must never record an acceptance")
	}
}

// TestRevokeReviewVerdictAcceptance_Member_Returns403 pins finding F3a
// (adversarial review): AcceptReviewVerdict's own sibling authz gate
// (TestAcceptReviewVerdict_Member_Returns403, above) had no equivalent
// test for RevokeReviewVerdictAcceptance -- the authorize(...,
// authz.ActionAcceptReviewVerdict, ...) block in that handler
// (decisioninbox.go) could be deleted with the whole suite still green.
// A member (or viewer) may never revoke an acceptance, mirroring
// action.go's own "revocation is deliberately never a stricter role than
// acceptance" doc comment -- same role floor, same denial for anything
// below it.
func TestRevokeReviewVerdictAcceptance_Member_Returns403(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, maintainerToken := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMaintainer)
	_, memberToken := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)
	const repoFullName = "acme/revoke-verdict-member"
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, 507, "headsha507")

	record, hasVerdict, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: rig.reviewVerdicts}, repoFullName, 507)
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest: hasVerdict=%v err=%v", hasVerdict, err)
	}

	acceptBody, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: repoFullName, PrNumber: 507, VerdictId: record.ID, Justification: "Accepted by a maintainer, then a member tries (and must fail) to revoke it.",
	})
	if err != nil {
		t.Fatalf("marshal accept request: %v", err)
	}
	var accepted restdtos.ReviewVerdictAcceptance
	if status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", acceptBody, &accepted, maintainerToken); status != http.StatusCreated {
		t.Fatalf("accept status = %d, want %d", status, http.StatusCreated)
	}

	revokeBody, err := json.Marshal(restdtos.RevokeReviewVerdictAcceptanceRequest{
		RepoFullName: repoFullName, Id: accepted.Id,
	})
	if err != nil {
		t.Fatalf("marshal revoke request: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/revoke-verdict-acceptance", revokeBody, nil, memberToken)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (member role must be denied)", status, http.StatusForbidden)
	}

	// The acceptance must still be active -- a denied request must never
	// have any side effect.
	_, ok, err := appreviewverdict.GetActiveAcceptance(ctx, narvipg.NewReviewVerdictAcceptanceStore(rig.pool), repoFullName, 507)
	if err != nil {
		t.Fatalf("GetActiveAcceptance: error = %v, want nil", err)
	}
	if !ok {
		t.Error("GetActiveAcceptance: ok = false, want true -- a forbidden revoke attempt must never actually revoke anything")
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

	record, hasVerdict, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: rig.reviewVerdicts}, repoFullName, 503)
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest: hasVerdict=%v err=%v", hasVerdict, err)
	}

	acceptBody, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: repoFullName, PrNumber: 503, VerdictId: record.ID, Justification: "Accepted, then revoked by a DIFFERENT maintainer+ (admin), by design -- §21.1b: revocation is never restricted to the original acceptor.",
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
	// finding F4 (adversarial review): a genuine, explicit revoke click
	// must read back as "explicit", never "superseded" -- the fact that
	// tells the two apart on the wire.
	if revoked.RevocationReason == nil || *revoked.RevocationReason != "explicit" {
		t.Errorf("RevocationReason = %v, want %q", revoked.RevocationReason, "explicit")
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

// TestRevokeReviewVerdictAcceptance_AuditFailureRollsBackRevoke pins
// finding R3 (round 3, adversarial review) directly against real
// Postgres: an audit-log write failure must abort the WHOLE revocation,
// never leave the acceptance revoked with no record of it. Forces the
// audit INSERT specifically (never review_verdict_acceptances) to fail
// with a throwaway CHECK constraint naming this test's own action --
// review_verdict_acceptances carries no such constraint, so a version of
// this handler that still writes the revoke UPDATE on the bare pool,
// then the audit row best-effort afterward (round 2's own shape, before
// this fix), would commit the revoke regardless of this constraint and
// only fail (silently, logged-and-swallowed, 200 OK) writing the audit
// row -- this test's own decisive assertion is that the acceptance
// itself is STILL ACTIVE afterward, which only holds once revoke and its
// audit row run in the SAME transaction.
func TestRevokeReviewVerdictAcceptance_AuditFailureRollsBackRevoke(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMaintainer)
	const repoFullName = "acme/revoke-verdict-audit-failure"
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, 510, "headsha510")

	record, hasVerdict, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: rig.reviewVerdicts}, repoFullName, 510)
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest: hasVerdict=%v err=%v", hasVerdict, err)
	}

	acceptBody, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: repoFullName, PrNumber: 510, VerdictId: record.ID, Justification: "About to be revoked -- the revocation's own audit write will be forced to fail.",
	})
	if err != nil {
		t.Fatalf("marshal accept request: %v", err)
	}
	var accepted restdtos.ReviewVerdictAcceptance
	if status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/accept-verdict", acceptBody, &accepted, token); status != http.StatusCreated {
		t.Fatalf("accept status = %d, want %d", status, http.StatusCreated)
	}

	// THE FAULT: a throwaway CHECK constraint that only ever rejects an
	// audit_log row for THIS test's own action -- review_verdict.
	// revoke_acceptance -- never anything on review_verdict_acceptances
	// itself, so the revoke UPDATE this handler issues is, on its own,
	// completely unaffected by this constraint.
	const constraintName = "test_r3_block_revoke_audit"
	if _, err := rig.pool.Exec(ctx, "ALTER TABLE audit_log ADD CONSTRAINT "+constraintName+" CHECK (action <> 'review_verdict.revoke_acceptance')"); err != nil {
		t.Fatalf("add throwaway audit_log constraint: %v", err)
	}
	t.Cleanup(func() {
		if _, err := rig.pool.Exec(context.Background(), "ALTER TABLE audit_log DROP CONSTRAINT IF EXISTS "+constraintName); err != nil {
			t.Errorf("drop throwaway audit_log constraint: %v", err)
		}
	})

	revokeBody, err := json.Marshal(restdtos.RevokeReviewVerdictAcceptanceRequest{
		RepoFullName: repoFullName, Id: accepted.Id,
	})
	if err != nil {
		t.Fatalf("marshal revoke request: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/revoke-verdict-acceptance", revokeBody, nil, token)
	if status != http.StatusInternalServerError {
		t.Fatalf("revoke status = %d, want %d (the audit write must fail, and the handler must surface that, never a silent 200)", status, http.StatusInternalServerError)
	}

	// THE DECISIVE ASSERTION: the acceptance must still be ACTIVE -- the
	// revoke UPDATE rolled back together with its own failed audit write,
	// never committed independently.
	_, ok, err := appreviewverdict.GetActiveAcceptance(ctx, narvipg.NewReviewVerdictAcceptanceStore(rig.pool), repoFullName, 510)
	if err != nil {
		t.Fatalf("GetActiveAcceptance after failed revoke: error = %v, want nil", err)
	}
	if !ok {
		t.Fatal("GetActiveAcceptance after failed revoke: ok = false, want true -- a revocation whose own audit write failed must roll back entirely, never leave the acceptance revoked with no record of it")
	}

	// No audit row for this action must exist either -- the failed
	// INSERT itself never committed (it could not have, given the
	// constraint), but this also rules out some OTHER, unrelated
	// audit row accidentally satisfying the test above.
	entries, err := rig.auditLog.List(ctx, 100, 0)
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	for _, e := range entries {
		if e.Action == "review_verdict.revoke_acceptance" && e.ResourceID == accepted.Id {
			t.Errorf("found a review_verdict.revoke_acceptance audit row for %s despite the forced failure -- want none", accepted.Id)
		}
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

// TestAcceptReviewVerdict_ConcurrentAccept_ExactlyOneActive exercises
// AcceptReviewVerdict itself (not merely the store) under real HTTP
// concurrency: N requests race to accept the SAME verdict, and the row
// count must settle at EXACTLY one active acceptance regardless of how
// many individual requests succeeded (201) versus lost to the
// unique-constraint violation (500, an accepted, documented residual --
// ReviewVerdictAcceptanceStore.Insert's own doc comment: "an ordinary,
// retryable 500, never a silent violation of 'at most one active row'").
//
// HONEST LIMIT, stated rather than implied (execution-checked): this test
// does NOT reliably distinguish AcceptReviewVerdict calling Acceptances.
// WithTx(tx).Insert from a reverted version calling the bare,
// pool-scoped Acceptances.Insert directly -- both configurations were
// run against this exact test and both left activeCount == 1. The reason
// is structural, not a weak assertion: the ONLY way Insert can fail
// through this real endpoint is the unique-constraint race itself (the
// handler's own req.VerdictId mismatch check refuses a bogus verdict id
// with a 409 before Accept is ever called, so the foreign-key-violation
// failure mode the store-level tests use to force a deterministic
// partial failure is unreachable here) -- and an ordinary "loser
// collides with an already-committed winner" race tends to self-heal via
// later racers' own chained supersessions even without per-request
// atomicity, in exactly the interleavings this test's own goroutines
// produced when checked against the reverted handler. The DETERMINISTIC
// proof that WithTx is what makes a genuine partial failure roll back
// together -- rather than leaving zero active rows, per finding F2,
// adversarial review -- is
// reviewverdictacceptance_store_integration_test.go's own
// TestReviewVerdictAcceptanceStore_Insert_PartialFailureRollsBackTogether/
// _WithoutTxLeavesNoActiveAcceptanceOnPartialFailure pair, which forces
// the second half to fail via a real foreign-key violation rather than a
// race. This test still earns its place alongside them: it is the one
// place the handler's OWN transaction/commit/rollback wiring runs
// end-to-end under concurrency without throwing an unhandled panic or
// leaving a stuck connection, which the store-level tests, calling the
// store directly, cannot exercise.
func TestAcceptReviewVerdict_ConcurrentAccept_ExactlyOneActive(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()

	_, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMaintainer)
	const repoFullName = "acme/accept-verdict-concurrent"
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, 508, "headsha508")

	record, hasVerdict, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: rig.reviewVerdicts}, repoFullName, 508)
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest: hasVerdict=%v err=%v", hasVerdict, err)
	}

	body, err := json.Marshal(restdtos.AcceptReviewVerdictRequest{
		RepoFullName: repoFullName, PrNumber: 508, VerdictId: record.ID, Justification: "Concurrent accept race.",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	const n = 10
	var g errgroup.Group
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		idx := i
		g.Go(func() error {
			req, err := http.NewRequest(http.MethodPost, rig.server.URL+"/api/decision-inbox/accept-verdict", bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			statuses[idx] = resp.StatusCode
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("errgroup: %v", err)
	}

	successCount := 0
	for _, s := range statuses {
		switch s {
		case http.StatusCreated:
			successCount++
		case http.StatusInternalServerError:
			// A genuine, accepted concurrent-accept race residual (this
			// test's own doc comment) -- never a hard failure.
		default:
			t.Errorf("unexpected status %d, want %d or %d", s, http.StatusCreated, http.StatusInternalServerError)
		}
	}
	if successCount < 1 {
		t.Fatalf("successCount = 0 of %d, want at least 1 -- %d concurrent accepts racing the SAME verdict must yield at least one committed acceptance", n, n)
	}

	// THE DECISIVE ASSERTION: regardless of how many individual requests
	// happened to succeed, exactly one row is active once every request
	// has finished.
	var activeCount int
	if err := rig.pool.QueryRow(ctx,
		`SELECT count(*) FROM review_verdict_acceptances WHERE repo_full_name = $1 AND pr_number = $2 AND revoked_at IS NULL`,
		repoFullName, 508,
	).Scan(&activeCount); err != nil {
		t.Fatalf("count active rows: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("active row count = %d, want exactly 1 -- a losing request's own Supersede half must never commit alone (finding F2, adversarial review)", activeCount)
	}
}
