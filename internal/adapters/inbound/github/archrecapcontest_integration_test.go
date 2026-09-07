//go:build integration

// Integration tests for §26.4's own §26.5 capture command against a
// real Postgres instance -- mirrors falsepositivecapture_integration_test.go's
// own established conventions (newTestRig, postWebhook, sign,
// createLinkedGitHubUser), gated behind the "integration" build tag. Run
// via `make test-integration`.
package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
)

// archRecapCommentBody mirrors falsePositiveCommentBody exactly (same
// file, falsepositivecapture_integration_test.go) -- an "issue_comment"
// webhook payload carrying commentID/commenterID/commenterLogin and an
// arbitrary body string.
func archRecapCommentBody(repoFullName, repoName, cloneURL string, prNumber int, commentID, commenterID int64, commenterLogin, commentBody string) []byte {
	body, err := json.Marshal(map[string]any{
		"action": "created",
		"issue": map[string]any{
			"number":       prNumber,
			"pull_request": map[string]any{"url": fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d", repoFullName, prNumber)},
		},
		"comment": map[string]any{
			"id":   commentID,
			"body": commentBody,
			"user": map[string]any{"id": commenterID, "login": commenterLogin},
		},
		"repository": map[string]any{
			"full_name": repoFullName,
			"name":      repoName,
			"clone_url": cloneURL,
		},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// newArchRecapTestRig builds the shared rig (handler_integration_test.go's
// own newTestRig) with ArchRecapContestCapture/ArchRecapVerdicts wired --
// mirrors newFalsePositiveTestRig's own identical shape
// (falsepositivecapture_integration_test.go).
func newArchRecapTestRig(t *testing.T) (testRig, *narvipg.ReviewDigestSectionFeedbackStore, appreviewverdict.Deps) {
	t.Helper()
	pool := newTestPool(t)
	feedback := narvipg.NewReviewDigestSectionFeedbackStore(pool)
	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	deps := appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts}
	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.ArchRecapContestCapture = feedback
		cfg.ArchRecapVerdicts = deps
	})
	return rig, feedback, deps
}

// seedDeepVerdictWithArchDecisions inserts a review_verdicts row carrying
// real Digest.ArchDecisions content, the SAME shape appreviewverdict.
// GetLatest (tryCaptureArchRecapContest's own read) reconstructs from --
// this is what a maintainer's contest is reconciled against.
func seedDeepVerdictWithArchDecisions(ctx context.Context, t *testing.T, reviewVerdicts *narvipg.ReviewVerdictStore, repoSettings *narvipg.RepoSettingsStore, repoFullName string, prNumber int32, headSHA string, decisions []reviewpost.ArchDecision) {
	t.Helper()
	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	digest := reviewpost.Digest{
		Summary:             "Test-seeded deep-path verdict.",
		DescriptionAdequacy: review.DescriptionAdequacyOK,
		AdequacyExplanation: "matches the diff",
		ArchDecisions:       decisions,
	}
	if _, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettings, false, repoFullName, prNumber, headSHA, pgtype.UUID{}, verdict, digest, "deep", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false); err != nil {
		t.Fatalf("seed deep review_verdicts row: %v", err)
	}
}

func countDigestSectionFeedback(ctx context.Context, t *testing.T, rig testRig, repoFullName string) int {
	t.Helper()
	var count int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM review_digest_section_feedback WHERE repo_full_name = $1`, repoFullName).Scan(&count); err != nil {
		t.Fatalf("count review_digest_section_feedback: %v", err)
	}
	return count
}

// TestGitHubIntegration_ArchRecapContest_MaintainerContests_FeedbackCreated
// is this Step's own core happy-path proof: a maintainer's `arch recap
// wrong: <reason>` comment on a PR whose latest verdict carries a real
// arch recap creates exactly one review_digest_section_feedback row, with
// a content_hash matching reviewpost.ComputeDigestSectionIdentity's own
// computation over that SAME recap -- and creates NO session/turn at all
// (dispatch-before-router, never reaching the ordinary mention pipeline).
func TestGitHubIntegration_ArchRecapContest_MaintainerContests_FeedbackCreated(t *testing.T) {
	ctx := context.Background()
	rig, feedback, _ := newArchRecapTestRig(t)

	const repoFullName = "acme/arch-recap-maintainer-repo"
	const prNumber = 9
	const commenterID = 80090001
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	decisions := []reviewpost.ArchDecision{
		{Decision: "Introduced a retry queue table", RejectedAlternative: "Extending the outbox", ConventionConformance: "Matches the one-table-per-concern pattern"},
	}
	seedDeepVerdictWithArchDecisions(ctx, t, narvipg.NewReviewVerdictStore(rig.pool), narvipg.NewRepoSettingsStore(rig.pool), repoFullName, prNumber, "sha-arch-recap-1", decisions)

	wantHash := reviewpost.ComputeDigestSectionIdentity(reviewpost.DigestSectionArchRecap, reviewpost.ArchRecapText(decisions))

	body := archRecapCommentBody(repoFullName, "arch-recap-maintainer-repo", "https://github.com/acme/arch-recap-maintainer-repo.git", prNumber, 800001, commenterID, "maintainer-user", "arch recap wrong: the outbox alternative wasn't actually considered")
	status := postWebhook(t, rig, body, "delivery-arch-recap-maintainer-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if got := countDigestSectionFeedback(ctx, t, rig, repoFullName); got != 1 {
		t.Fatalf("feedback count = %d, want 1", got)
	}

	rows, err := feedback.List(ctx, repoFullName, nil, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(rows))
	}
	if rows[0].Reason != "the outbox alternative wasn't actually considered" {
		t.Errorf("Reason = %q, want %q", rows[0].Reason, "the outbox alternative wasn't actually considered")
	}
	if rows[0].ContentHash != wantHash {
		t.Errorf("ContentHash = %q, want %q (must match the SAME content hash reviewpost.ComputeDigestSectionIdentity computes over the seeded arch recap)", rows[0].ContentHash, wantHash)
	}
	if rows[0].Section != string(reviewpost.DigestSectionArchRecap) {
		t.Errorf("Section = %q, want %q", rows[0].Section, reviewpost.DigestSectionArchRecap)
	}
	if !rows[0].CreatedBy.Valid {
		t.Error("CreatedBy.Valid = false, want true (attributed to the real maintainer)")
	}

	if got := countSessionsForRepo(ctx, t, rig, "arch-recap-maintainer-repo"); got != 0 {
		t.Errorf("session count = %d, want 0 (dispatch-before-router: a contest command must never spawn a review session)", got)
	}
}

// TestGitHubIntegration_ArchRecapContest_MemberDenied proves §13.3's own
// maintainer+ gate (authz.ActionContestArchRecap): a linked `member` is
// denied -- no feedback row created, still acknowledged 200.
func TestGitHubIntegration_ArchRecapContest_MemberDenied(t *testing.T) {
	ctx := context.Background()
	rig, _, _ := newArchRecapTestRig(t)

	const repoFullName = "acme/arch-recap-member-repo"
	const prNumber = 3
	const commenterID = 80090002
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMember)

	seedDeepVerdictWithArchDecisions(ctx, t, narvipg.NewReviewVerdictStore(rig.pool), narvipg.NewRepoSettingsStore(rig.pool), repoFullName, prNumber, "sha-arch-recap-2", []reviewpost.ArchDecision{{Decision: "x"}})

	body := archRecapCommentBody(repoFullName, "arch-recap-member-repo", "https://github.com/acme/arch-recap-member-repo.git", prNumber, 800002, commenterID, "member-user", "arch recap wrong: nope")
	status := postWebhook(t, rig, body, "delivery-arch-recap-member-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if got := countDigestSectionFeedback(ctx, t, rig, repoFullName); got != 0 {
		t.Errorf("feedback count = %d, want 0 (a member must be denied)", got)
	}
}

// TestGitHubIntegration_ArchRecapContest_NoVerdictOnRecord_NoOp proves the
// "nothing to contest" business-rule no-op: a maintainer's contest on a PR
// with NO review_verdicts row at all is acknowledged 200, but creates no
// feedback row -- never a 500, since this is a legitimate (if unusual)
// state, not a genuine backend failure.
func TestGitHubIntegration_ArchRecapContest_NoVerdictOnRecord_NoOp(t *testing.T) {
	ctx := context.Background()
	rig, _, _ := newArchRecapTestRig(t)

	const repoFullName = "acme/arch-recap-no-verdict-repo"
	const prNumber = 5
	const commenterID = 80090003
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	body := archRecapCommentBody(repoFullName, "arch-recap-no-verdict-repo", "https://github.com/acme/arch-recap-no-verdict-repo.git", prNumber, 800003, commenterID, "maintainer-user", "arch recap wrong: nope, no verdict exists yet")
	status := postWebhook(t, rig, body, "delivery-arch-recap-no-verdict-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if got := countDigestSectionFeedback(ctx, t, rig, repoFullName); got != 0 {
		t.Errorf("feedback count = %d, want 0 (no verdict on record -- nothing to contest)", got)
	}
}

// TestGitHubIntegration_ArchRecapContest_RedeliveredComment_IdempotentNoDuplicate
// is this Step's own explicit idempotency pin: the SAME (comment_id,
// comment_type) delivered TWICE (a GitHub webhook redelivery, or a
// retried command) must produce exactly ONE feedback row, never two --
// mirrors TestGitHubIntegration_FalsePositiveCapture_RedeliveredComment_IdempotentNoDuplicate's
// own identical precedent (falsepositivecapture_integration_test.go).
func TestGitHubIntegration_ArchRecapContest_RedeliveredComment_IdempotentNoDuplicate(t *testing.T) {
	ctx := context.Background()
	rig, feedback, _ := newArchRecapTestRig(t)

	const repoFullName = "acme/arch-recap-redelivery-repo"
	const prNumber = 11
	const commenterID = 80090004
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	seedDeepVerdictWithArchDecisions(ctx, t, narvipg.NewReviewVerdictStore(rig.pool), narvipg.NewRepoSettingsStore(rig.pool), repoFullName, prNumber, "sha-arch-recap-3", []reviewpost.ArchDecision{{Decision: "Introduced a retry queue"}})

	body := archRecapCommentBody(repoFullName, "arch-recap-redelivery-repo", "https://github.com/acme/arch-recap-redelivery-repo.git", prNumber, 800004, commenterID, "maintainer-user", "arch recap wrong: still wrong")

	first := postWebhook(t, rig, body, "delivery-arch-recap-redelivery-1")
	if first != http.StatusOK {
		t.Fatalf("first delivery status = %d, want %d", first, http.StatusOK)
	}
	// A DIFFERENT delivery id (GitHub's own X-GitHub-Delivery is per-
	// delivery, not per-comment) carrying the IDENTICAL comment.id -- the
	// real shape a genuine GitHub redelivery takes.
	second := postWebhook(t, rig, body, "delivery-arch-recap-redelivery-2")
	if second != http.StatusOK {
		t.Fatalf("second (redelivered) status = %d, want %d", second, http.StatusOK)
	}

	if got := countDigestSectionFeedback(ctx, t, rig, repoFullName); got != 1 {
		t.Fatalf("feedback count after redelivery = %d, want 1 (idempotent on the triggering comment id)", got)
	}
	rows, err := feedback.List(ctx, repoFullName, nil, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(rows))
	}
}
