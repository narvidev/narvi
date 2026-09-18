//go:build integration

// Integration tests for "human acceptance of a verdict the engine
// refuses" (§21.1b) against a REAL Postgres instance -- gated behind the
// "integration" build tag, same package (decisioninbox_test) and
// newTestPool/newRevalidateStores/fakeDecisionInboxSourceControl
// precedent as revalidate_integration_test.go, which this file reuses
// directly rather than duplicating.
package decisioninbox_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

// seedNotShippableAutoVerdict is seedAutoApprovedVerdict's own sibling
// (aggregate_integration_test.go): the SAME "otherwise fully eligible"
// shape (adequate coverage, ok premise, counter-review done, matching
// base/policy context) except RiskLevel is HIGH, which baselineFromRisk
// (review/shippable.go) maps to ShippableNeedsHuman regardless of every
// floor -- the ONE criterion this fixture means to fail, so a PR seeded
// with it is refused by ComputeEligible on ReasonNotShippableAuto alone,
// never on anything else (§21.1b's own first waivable criterion).
func seedNotShippableAutoVerdict(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, prNumber int32, headSHA string) {
	t.Helper()
	store := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelHigh,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableNeedsHuman,
		FilesChanged:      3,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	if verdict.Shippable == review.ShippableAuto {
		t.Fatalf("seedNotShippableAutoVerdict: fixture bug -- RiskLevelHigh computed Shippable=auto, want anything else")
	}
	verdictContext := reviewverdict.Context{
		BaseRef:       testEligibleBaseRef,
		BaseSHA:       testEligibleBaseSHA,
		PolicyVersion: autoapproval.CurrentPolicyVersion,
	}
	if _, err := appreviewverdict.Insert(ctx, store, repoSettings, false, repoFullName, prNumber, headSHA, pgtype.UUID{}, verdict, reviewpost.Digest{Summary: "Test-seeded high-risk verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false, verdictContext, pgtype.UUID{}); err != nil {
		t.Fatalf("seed not-shippable-auto review_verdicts row for %s#%d: %v", repoFullName, prNumber, err)
	}
}

// TestRevalidateForMerge_AcceptedVerdict is this Step's own exit
// criterion, proven end to end against real Postgres: "an accepted
// verdict merges while a moved base makes the same acceptance
// inapplicable without a human touching it, and the analytics still show
// the original risk level." Every phase below is the SAME PR, the SAME
// never-reviewed-again verdict, and the SAME never-revoked acceptance --
// only the PR's own live base changes between phases 3 and 4.
func TestRevalidateForMerge_AcceptedVerdict(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	rs := newRevalidateStores(pool)
	const actorGitHubID = "revalidate-accept-actor"
	const repoFullName = "acme/revalidate-accepted-verdict"

	users := narvipg.NewUserStore(pool)
	maintainer, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "accept-maintainer@example.com", DisplayName: "Maintainer", Role: sqlcgen.UserRoleMaintainer})
	if err != nil {
		t.Fatalf("create maintainer: %v", err)
	}

	// Phase 0: seed a PR that is fully eligible on EVERY criterion except
	// Shippable -- eligiblePR's own baseline, then a SECOND, later
	// review_verdicts row (append-only, §21.1) that overrides it with a
	// high-risk verdict at the SAME head sha, so GetLatest returns THIS
	// one.
	pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 1)
	seedNotShippableAutoVerdict(ctx, t, pool, repoFullName, int32(pr.Number), pr.HeadSHA)

	// Phase 1: the engine refuses -- ReasonNotShippableAuto, and ONLY
	// that reason (proven by asserting the reason text names it, never a
	// blanket "not eligible").
	ok, _, reason, _, _, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
	if err != nil {
		t.Fatalf("RevalidateForMerge() (phase 1, before acceptance) error = %v, want nil", err)
	}
	if ok {
		t.Fatal("RevalidateForMerge() (phase 1) ok = true, want false: this verdict is high-risk and has not been accepted yet")
	}
	if !strings.Contains(reason, string(autoapproval.ReasonNotShippableAuto)) {
		t.Fatalf("RevalidateForMerge() (phase 1) reason = %q, want it to name %q", reason, autoapproval.ReasonNotShippableAuto)
	}

	// Record the verdict's own risk level BEFORE acceptance, to compare
	// against AFTER acceptance and AFTER the base moves -- this is the
	// exit criterion's own "the analytics still show the original risk
	// level" half, checked against internal/app/reviewverdict.GetLatest,
	// the SAME read the decision inbox's own analytics/rollups
	// ultimately reduce over (§21.1).
	recordBefore, hasVerdict, err := appreviewverdict.GetLatest(ctx, rs.deps.ReviewVerdict, repoFullName, int32(pr.Number))
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest before acceptance: hasVerdict=%v err=%v", hasVerdict, err)
	}
	if recordBefore.Verdict.RiskLevel != review.RiskLevelHigh || recordBefore.Verdict.Shippable == review.ShippableAuto {
		t.Fatalf("fixture bug: seeded verdict is RiskLevel=%q Shippable=%q, want RiskLevelHigh/not-auto", recordBefore.Verdict.RiskLevel, recordBefore.Verdict.Shippable)
	}

	// Phase 2: a maintainer+ accepts it.
	var verdictID pgtype.UUID
	if err := verdictID.Scan(recordBefore.ID); err != nil {
		t.Fatalf("scan verdict id: %v", err)
	}
	acceptance, err := appreviewverdict.Accept(ctx, rs.deps.ReviewVerdict.Acceptances, appreviewverdict.AcceptInput{
		RepoFullName:  repoFullName,
		PRNumber:      int32(pr.Number),
		VerdictID:     verdictID,
		HeadSHA:       recordBefore.HeadSHA,
		Context:       recordBefore.Context,
		Reason:        string(autoapproval.ReasonNotShippableAuto),
		Justification: "Reviewed offline with the team; the risk is understood and accepted for this one PR.",
		AcceptedBy:    maintainer.ID,
	})
	if err != nil {
		t.Fatalf("Accept() error = %v, want nil", err)
	}
	if acceptance.Revoked() {
		t.Fatal("a freshly-created acceptance reports Revoked() = true, want false")
	}

	// Phase 3: the SAME verdict, SAME context, now merges -- the
	// acceptance authorised proceeding past ReasonNotShippableAuto, and
	// every OTHER criterion (CI green, blast radius known and clean,
	// head/base/ancestor-chain freshness) still holds because nothing
	// about the PR's own live facts changed.
	ok, headSHA, reason, _, _, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
	if err != nil {
		t.Fatalf("RevalidateForMerge() (phase 3, after acceptance) error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("RevalidateForMerge() (phase 3) ok = false, reason = %q, want true: an applicable acceptance should waive ReasonNotShippableAuto", reason)
	}
	if headSHA != pr.HeadSHA {
		t.Errorf("headSHA = %q, want %q", headSHA, pr.HeadSHA)
	}

	// The exit criterion's own analytics half, checked again right after
	// the merge would have happened: GetLatest still reports the
	// ORIGINAL risk level -- the acceptance never rewrote review_verdicts.
	recordAfterAccept, hasVerdict, err := appreviewverdict.GetLatest(ctx, rs.deps.ReviewVerdict, repoFullName, int32(pr.Number))
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest after acceptance: hasVerdict=%v err=%v", hasVerdict, err)
	}
	if recordAfterAccept.Verdict.RiskLevel != review.RiskLevelHigh {
		t.Errorf("RiskLevel after acceptance = %q, want unchanged RiskLevelHigh (§21.1b: acceptance never rewrites the assessment)", recordAfterAccept.Verdict.RiskLevel)
	}
	if recordAfterAccept.Verdict.Shippable != recordBefore.Verdict.Shippable {
		t.Errorf("Shippable after acceptance = %q, want unchanged %q", recordAfterAccept.Verdict.Shippable, recordBefore.Verdict.Shippable)
	}

	// Phase 4: the base moves (a retarget -- the ref itself changes,
	// which ComputeEligible refuses UNCONDITIONALLY, no fast-forward
	// tolerance applies) -- WITHOUT a human touching the acceptance:
	// nobody calls RevokeAcceptance, the row's own revoked_at stays NULL
	// throughout this phase.
	retargeted := pr
	retargeted.BaseRef = "retargeted-branch"
	retargeted.BaseSHA = "sha-retargeted-base"
	rs.replaceTargetPR(actorGitHubID, retargeted)

	ok, _, reason, _, _, err = decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
	if err != nil {
		t.Fatalf("RevalidateForMerge() (phase 4, after base moved) error = %v, want nil", err)
	}
	if ok {
		t.Fatal("RevalidateForMerge() (phase 4) ok = true, want false: a moved base must make the same acceptance inapplicable, without a human revoking anything")
	}
	if !strings.Contains(reason, string(autoapproval.ReasonBaseMoved)) {
		t.Fatalf("RevalidateForMerge() (phase 4) reason = %q, want it to name %q (a mandatory freshness check, never waived by acceptance)", reason, autoapproval.ReasonBaseMoved)
	}

	// Confirm, directly against the store, that this refusal was NOT the
	// result of a human revoking the acceptance -- the row this test
	// created in phase 2 is still active.
	stillActive, ok, err := appreviewverdict.GetActiveAcceptance(ctx, rs.deps.ReviewVerdict.Acceptances, repoFullName, int32(pr.Number))
	if err != nil {
		t.Fatalf("GetActiveAcceptance after base moved: error = %v, want nil", err)
	}
	if !ok {
		t.Fatal("GetActiveAcceptance after base moved: ok = false, want true -- nobody revoked this acceptance, it must still exist and be active")
	}
	if stillActive.ID != acceptance.ID {
		t.Fatalf("GetActiveAcceptance after base moved returned a DIFFERENT acceptance id (%s), want the SAME one this test created (%s)", stillActive.ID, acceptance.ID)
	}
	if stillActive.Revoked() {
		t.Error("the acceptance's own Revoked() = true after phase 4, want false -- the exit criterion is that the base move alone (never a human action) is what refuses the merge again")
	}

	// The analytics half, a third time, after the base move refused the
	// merge again: still the original risk level.
	recordAfterBaseMove, hasVerdict, err := appreviewverdict.GetLatest(ctx, rs.deps.ReviewVerdict, repoFullName, int32(pr.Number))
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest after base moved: hasVerdict=%v err=%v", hasVerdict, err)
	}
	if recordAfterBaseMove.Verdict.RiskLevel != review.RiskLevelHigh {
		t.Errorf("RiskLevel after base moved = %q, want unchanged RiskLevelHigh", recordAfterBaseMove.Verdict.RiskLevel)
	}
}

// TestRevalidateForMerge_AcceptedVerdict_MandatoryConditionsStillRefuse
// proves the OTHER half of §21.1b's own contract against real Postgres,
// wiring-level (not just the pure ComputeEligibleWithAcceptance unit
// tests in internal/domain/autoapproval): an applicable acceptance for
// ReasonNotShippableAuto does NOT also waive CI green -- "the conditions
// that are not about human judgment... stay mandatory."
func TestRevalidateForMerge_AcceptedVerdict_MandatoryConditionsStillRefuse(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	rs := newRevalidateStores(pool)
	const actorGitHubID = "revalidate-accept-mandatory-actor"
	const repoFullName = "acme/revalidate-accepted-verdict-mandatory"

	users := narvipg.NewUserStore(pool)
	maintainer, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "accept-mandatory@example.com", DisplayName: "Maintainer", Role: sqlcgen.UserRoleMaintainer})
	if err != nil {
		t.Fatalf("create maintainer: %v", err)
	}

	pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 1)
	seedNotShippableAutoVerdict(ctx, t, pool, repoFullName, int32(pr.Number), pr.HeadSHA)

	record, hasVerdict, err := appreviewverdict.GetLatest(ctx, rs.deps.ReviewVerdict, repoFullName, int32(pr.Number))
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest: hasVerdict=%v err=%v", hasVerdict, err)
	}
	var verdictID pgtype.UUID
	if err := verdictID.Scan(record.ID); err != nil {
		t.Fatalf("scan verdict id: %v", err)
	}
	if _, err := appreviewverdict.Accept(ctx, rs.deps.ReviewVerdict.Acceptances, appreviewverdict.AcceptInput{
		RepoFullName:  repoFullName,
		PRNumber:      int32(pr.Number),
		VerdictID:     verdictID,
		HeadSHA:       record.HeadSHA,
		Context:       record.Context,
		Reason:        string(autoapproval.ReasonNotShippableAuto),
		Justification: "Accepted despite the risk level -- but CI is red, so this must not merge.",
		AcceptedBy:    maintainer.ID,
	}); err != nil {
		t.Fatalf("Accept() error = %v, want nil", err)
	}

	// Now turn CI red -- a MANDATORY condition an acceptance never waives.
	ciRed := pr
	ciRed.CIConclusion = "failure"
	rs.replaceTargetPR(actorGitHubID, ciRed)

	ok, _, reason, _, _, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
	if err != nil {
		t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
	}
	if ok {
		t.Fatal("RevalidateForMerge() ok = true, want false: CI is red, which an acceptance must never waive, even with an otherwise-applicable acceptance for ReasonNotShippableAuto")
	}
	if !strings.Contains(reason, string(autoapproval.ReasonCINotGreen)) {
		t.Fatalf("reason = %q, want it to name %q", reason, autoapproval.ReasonCINotGreen)
	}
}

// TestRevalidateForMerge_AcceptedVerdict_NewAttemptInvalidatesAcceptance
// pins the OTHER half of §21.1b's own contract that
// TestRevalidateForMerge_AcceptedVerdict above never actually exercises
// (finding F7, adversarial review: that test's own verdict id never
// changes across all four of its phases, so deleting
// reviewverdict.Acceptance.Applicable's own verdict-id check from
// revalidateCore -- e.g. replacing `accepted := acceptanceOK &&
// acceptance.Applicable(record.ID)` with `accepted := acceptanceOK` --
// leaves that test green). This test posts a SECOND verdict (a fresh
// attempt, at the SAME head sha -- a re-triggered review with nothing
// new to say) for the SAME pull request AFTER the first one was
// accepted, and proves the acceptance no longer applies to the new
// attempt -- "acceptance binds to ONE verdict, one attempt and one
// context... a new attempt... makes it inapplicable" (§21.1b) -- even
// though nobody ever revoked it.
func TestRevalidateForMerge_AcceptedVerdict_NewAttemptInvalidatesAcceptance(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	rs := newRevalidateStores(pool)
	const actorGitHubID = "revalidate-accept-new-attempt-actor"
	const repoFullName = "acme/revalidate-accepted-verdict-new-attempt"

	users := narvipg.NewUserStore(pool)
	maintainer, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "accept-new-attempt@example.com", DisplayName: "Maintainer", Role: sqlcgen.UserRoleMaintainer})
	if err != nil {
		t.Fatalf("create maintainer: %v", err)
	}

	pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 1)
	seedNotShippableAutoVerdict(ctx, t, pool, repoFullName, int32(pr.Number), pr.HeadSHA)

	firstVerdict, hasVerdict, err := appreviewverdict.GetLatest(ctx, rs.deps.ReviewVerdict, repoFullName, int32(pr.Number))
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest (first verdict): hasVerdict=%v err=%v", hasVerdict, err)
	}

	// A maintainer+ accepts the FIRST verdict.
	var firstVerdictID pgtype.UUID
	if err := firstVerdictID.Scan(firstVerdict.ID); err != nil {
		t.Fatalf("scan first verdict id: %v", err)
	}
	acceptance, err := appreviewverdict.Accept(ctx, rs.deps.ReviewVerdict.Acceptances, appreviewverdict.AcceptInput{
		RepoFullName:  repoFullName,
		PRNumber:      int32(pr.Number),
		VerdictID:     firstVerdictID,
		HeadSHA:       firstVerdict.HeadSHA,
		Context:       firstVerdict.Context,
		Reason:        string(autoapproval.ReasonNotShippableAuto),
		Justification: "Accepted the FIRST attempt -- a fresh re-review must not inherit this.",
		AcceptedBy:    maintainer.ID,
	})
	if err != nil {
		t.Fatalf("Accept() error = %v, want nil", err)
	}

	// Confirm the acceptance genuinely authorises a merge of the first
	// attempt, BEFORE the new attempt exists -- otherwise this test would
	// prove nothing about which mechanism later refuses.
	ok, _, reason, _, _, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
	if err != nil {
		t.Fatalf("RevalidateForMerge() (before new attempt) error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("RevalidateForMerge() (before new attempt) ok = false, reason = %q, want true", reason)
	}

	// A re-triggered review posts a SECOND, high-risk verdict for the SAME
	// pull request at the SAME head sha (nothing new to say, still not
	// Shippable=auto) -- an ordinary re-review, never a code change.
	seedNotShippableAutoVerdict(ctx, t, pool, repoFullName, int32(pr.Number), pr.HeadSHA)

	secondVerdict, hasVerdict, err := appreviewverdict.GetLatest(ctx, rs.deps.ReviewVerdict, repoFullName, int32(pr.Number))
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest (second verdict): hasVerdict=%v err=%v", hasVerdict, err)
	}
	if secondVerdict.ID == firstVerdict.ID {
		t.Fatalf("fixture bug: second seedNotShippableAutoVerdict call did not produce a new verdict row (id unchanged: %s)", secondVerdict.ID)
	}

	// The acceptance itself is untouched -- nobody revoked it, and it is
	// still the LATEST non-revoked row for this pull request.
	stillActive, activeOK, err := appreviewverdict.GetActiveAcceptance(ctx, rs.deps.ReviewVerdict.Acceptances, repoFullName, int32(pr.Number))
	if err != nil {
		t.Fatalf("GetActiveAcceptance after new attempt: error = %v, want nil", err)
	}
	if !activeOK {
		t.Fatal("GetActiveAcceptance after new attempt: ok = false, want true -- nobody revoked this acceptance")
	}
	if stillActive.ID != acceptance.ID {
		t.Fatalf("GetActiveAcceptance after new attempt returned a DIFFERENT acceptance id (%s), want the SAME one this test created (%s)", stillActive.ID, acceptance.ID)
	}
	if stillActive.Revoked() {
		t.Error("the acceptance's own Revoked() = true after the new attempt, want false -- this test's own point is that Applicable, not revocation, is what refuses here")
	}

	// The decisive assertion: RevalidateForMerge must now refuse, on
	// ReasonNotShippableAuto again, because the OLD acceptance's own
	// VerdictID no longer matches the NEW latest verdict -- if
	// Acceptance.Applicable's own verdict-id check were deleted from
	// revalidateCore (accepted := acceptanceOK, with no Applicable call
	// at all), this would incorrectly stay eligible=true, since
	// GetActiveAcceptance still reports the old row as active.
	ok, _, reason, _, _, err = decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
	if err != nil {
		t.Fatalf("RevalidateForMerge() (after new attempt) error = %v, want nil", err)
	}
	if ok {
		t.Fatal("RevalidateForMerge() (after new attempt) ok = true, want false: an acceptance granted against a SUPERSEDED verdict must not authorise a merge of a fresh, never-accepted attempt")
	}
	if !strings.Contains(reason, string(autoapproval.ReasonNotShippableAuto)) {
		t.Fatalf("RevalidateForMerge() (after new attempt) reason = %q, want it to name %q -- the new attempt was never accepted", reason, autoapproval.ReasonNotShippableAuto)
	}
}

// TestAccept_SupersedesPriorActiveAcceptance pins finding F2 (adversarial
// review) directly against real Postgres: nothing stops two live
// acceptances for one pull request BEFORE this fix, so revoking the row
// GetActiveAcceptance reports as active could silently re-activate an
// earlier one that was never touched -- "a revocation that does not
// revoke is worse than none." A second Accept for the SAME verdict now
// atomically supersedes (revokes) the first, so at most one row is ever
// active, and revoking "the" active acceptance afterwards finds nothing
// left to silently re-activate.
func TestAccept_SupersedesPriorActiveAcceptance(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	rs := newRevalidateStores(pool)
	const actorGitHubID = "revalidate-supersede-actor"
	const repoFullName = "acme/revalidate-accept-supersede"

	users := narvipg.NewUserStore(pool)
	maintainer, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "supersede-maintainer@example.com", DisplayName: "Maintainer", Role: sqlcgen.UserRoleMaintainer})
	if err != nil {
		t.Fatalf("create maintainer: %v", err)
	}

	pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 1)
	seedNotShippableAutoVerdict(ctx, t, pool, repoFullName, int32(pr.Number), pr.HeadSHA)

	record, hasVerdict, err := appreviewverdict.GetLatest(ctx, rs.deps.ReviewVerdict, repoFullName, int32(pr.Number))
	if err != nil || !hasVerdict {
		t.Fatalf("GetLatest: hasVerdict=%v err=%v", hasVerdict, err)
	}
	var verdictID pgtype.UUID
	if err := verdictID.Scan(record.ID); err != nil {
		t.Fatalf("scan verdict id: %v", err)
	}

	acceptInput := appreviewverdict.AcceptInput{
		RepoFullName:  repoFullName,
		PRNumber:      int32(pr.Number),
		VerdictID:     verdictID,
		HeadSHA:       record.HeadSHA,
		Context:       record.Context,
		Reason:        string(autoapproval.ReasonNotShippableAuto),
		Justification: "First acceptance -- about to be superseded by a second accept.",
		AcceptedBy:    maintainer.ID,
	}
	first, err := appreviewverdict.Accept(ctx, rs.deps.ReviewVerdict.Acceptances, acceptInput)
	if err != nil {
		t.Fatalf("Accept() (first) error = %v, want nil", err)
	}

	// A second accept for the SAME verdict (a double-click, a retried
	// request) must SUPERSEDE the first, never coexist with it.
	acceptInput.Justification = "Second acceptance -- supersedes the first."
	second, err := appreviewverdict.Accept(ctx, rs.deps.ReviewVerdict.Acceptances, acceptInput)
	if err != nil {
		t.Fatalf("Accept() (second) error = %v, want nil", err)
	}
	if second.ID == first.ID {
		t.Fatalf("second Accept() returned the SAME id as the first (%s) -- Accept must always insert a NEW row (append-only)", first.ID)
	}

	// Exactly one row reports active: the second.
	active, ok, err := appreviewverdict.GetActiveAcceptance(ctx, rs.deps.ReviewVerdict.Acceptances, repoFullName, int32(pr.Number))
	if err != nil {
		t.Fatalf("GetActiveAcceptance: error = %v, want nil", err)
	}
	if !ok {
		t.Fatal("GetActiveAcceptance: ok = false, want true")
	}
	if active.ID != second.ID {
		t.Fatalf("GetActiveAcceptance returned %s, want the SECOND acceptance %s", active.ID, second.ID)
	}

	// The FIRST row must now read back as revoked -- superseded, not left
	// dangling as a second live row.
	firstAfter, ok, err := appreviewverdict.GetAcceptance(ctx, rs.deps.ReviewVerdict.Acceptances, verdictIDFromString(t, first.ID), repoFullName)
	if err != nil {
		t.Fatalf("GetAcceptance (first, after supersede): error = %v, want nil", err)
	}
	if !ok {
		t.Fatal("GetAcceptance (first, after supersede): ok = false, want true (the row must still exist, only revoked)")
	}
	if !firstAfter.Revoked() {
		t.Error("the FIRST acceptance's own Revoked() = false after a second accept superseded it, want true")
	}

	// THE DECISIVE ASSERTION: revoke "the" active acceptance (the
	// second) -- this must NOT silently re-activate the first.
	if _, ok, err := appreviewverdict.RevokeAcceptance(ctx, rs.deps.ReviewVerdict.Acceptances, verdictIDFromString(t, second.ID), maintainer.ID, repoFullName); err != nil || !ok {
		t.Fatalf("RevokeAcceptance (second): ok=%v err=%v, want ok=true err=nil", ok, err)
	}
	_, ok, err = appreviewverdict.GetActiveAcceptance(ctx, rs.deps.ReviewVerdict.Acceptances, repoFullName, int32(pr.Number))
	if err != nil {
		t.Fatalf("GetActiveAcceptance after revoking the second: error = %v, want nil", err)
	}
	if ok {
		t.Error("GetActiveAcceptance after revoking the second: ok = true, want false -- revoking the only active acceptance must never silently re-activate an earlier, already-superseded one (finding F2)")
	}
}

// verdictIDFromString is a small local helper -- every call site above
// already has a pgtype.UUID scan boilerplate; this collects it once for
// this test's own repeated GetAcceptance/RevokeAcceptance calls.
func verdictIDFromString(t *testing.T, id string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(id); err != nil {
		t.Fatalf("scan id %q: %v", id, err)
	}
	return u
}
