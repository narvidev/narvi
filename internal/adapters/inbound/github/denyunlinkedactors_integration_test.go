//go:build integration

// Integration tests for batch fix/deny-unlinked-github-actors: the
// repo-owner-decided hardening that aligns GitHub with Slack/Linear by
// denying an UNLINKED commenter's mention outright (coalesce.go's WINNER
// and REUSE gates both now call actorauthz.AuthorizeLinkedActor, never
// AuthorizeResolvedActor's own `!Valid -> allow` short-circuit), together
// with the actionable "please sign in" reply and its anti-spam dedupe
// this batch adds to compensate for GitHub having no magic-link/pending-
// link mechanism of its own (see actornotauthorizedreply.go's own doc
// comment). Reuses newTestPool/testWebhookSecret/testBotHandleIntegration/
// sign/issueCommentBody/issueCommentBodyWithCommenter/postWebhook/testRig/
// createLinkedGitHubUser/fakeCommentPoster from handler_integration_test.go
// (same package, same build tag).
package github_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

const testPublicBaseURL = "https://narvi.example.test"

// newDenyUnlinkedTestRig builds a rig with a fakeCommentPoster and a real
// PublicBaseURL wired -- every test in this file needs both to assert on
// the actionable reply this batch posts.
func newDenyUnlinkedTestRig(t *testing.T) (testRig, *fakeCommentPoster) {
	t.Helper()
	poster := &fakeCommentPoster{}
	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.Comments = poster
		cfg.Outbound = platform.MustNewGitHubOutboundConfig("test-bot-token")
		cfg.PublicBaseURL = testPublicBaseURL
		cfg.Timeouts = platform.DefaultTimeouts()
	})
	return rig, poster
}

// TestGitHubIntegration_UnlinkedCommenter_DeniedOnUntrackedPR_ReplyPosted
// proves the WINNER-path half of this batch's own hardening: an unlinked
// commenter's mention on a PR with no existing review session creates
// NEITHER a session NOR a claim row (see the claim-row-not-orphaned proof
// below for that half specifically), is acknowledged 200 (never released
// for a pointless GitHub redelivery retry -- retrying would reproduce the
// SAME denial), and gets the honest "please sign in" reply posted back to
// the PR thread -- the one thing GitHub's own ingress can offer in place
// of Slack/Linear's magic-link prompt.
func TestGitHubIntegration_UnlinkedCommenter_DeniedOnUntrackedPR_ReplyPosted(t *testing.T) {
	ctx := context.Background()
	rig, poster := newDenyUnlinkedTestRig(t)

	const repoFullName = "acme/unlinked-untracked-repo"
	const prNumber = 901

	// issueCommentBody never sets comment.user -- CommenterID resolves to
	// 0, resolveCommenterActor returns an invalid (unlinked) actor.
	body := issueCommentBody(repoFullName, "unlinked-untracked-repo", "https://github.com/acme/unlinked-untracked-repo.git", prNumber, "unlinked-mention")
	const deliveryID = "delivery-unlinked-untracked-1"

	status := postWebhook(t, rig, body, deliveryID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (denied, but still acknowledged)", status, http.StatusOK)
	}

	var sessionCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE spawn_source = 'github'`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (an unlinked commenter's mention must never create a session)", sessionCount)
	}

	var deliveryRowCount int
	if err := rig.pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'github' AND delivery_id = $1`, deliveryID,
	).Scan(&deliveryRowCount); err != nil {
		t.Fatalf("count webhook_deliveries rows: %v", err)
	}
	if deliveryRowCount != 1 {
		t.Errorf("webhook_deliveries row count = %d, want exactly 1 (claim must NOT be released -- a redelivery would only reproduce this same denial)", deliveryRowCount)
	}

	if len(poster.calls) != 1 {
		t.Fatalf("len(poster.calls) = %d, want exactly 1", len(poster.calls))
	}
	got := poster.calls[0]
	if got.token != "test-bot-token" {
		t.Errorf("posted comment token = %q, want %q", got.token, "test-bot-token")
	}
	if !strings.Contains(got.body, testPublicBaseURL+"/auth/github/login") {
		t.Errorf("posted comment body = %q, want it to contain the sign-in URL %q", got.body, testPublicBaseURL+"/auth/github/login")
	}
	if !strings.Contains(got.body, "won't be replayed automatically") {
		t.Errorf("posted comment body = %q, want it to be honest that the original mention will NOT be replayed", got.body)
	}
}

// TestGitHubIntegration_UnlinkedCommenter_DeniedOnExistingSession_ReplyPosted
// proves the REUSE-path half: an unlinked commenter mentioning the bot on
// a PR that ALREADY has a review session (created by a different, linked,
// authorized commenter) gets no new turn enqueued, and the SAME honest
// reply posted.
func TestGitHubIntegration_UnlinkedCommenter_DeniedOnExistingSession_ReplyPosted(t *testing.T) {
	ctx := context.Background()
	rig, poster := newDenyUnlinkedTestRig(t)

	const repoFullName = "acme/unlinked-reuse-repo"
	const cloneURL = "https://github.com/acme/unlinked-reuse-repo.git"
	const prNumber = 902

	const creatorCommenterID = 90090001
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, creatorCommenterID, sqlcgen.UserRoleMaintainer)

	first := issueCommentBodyWithCommenter(repoFullName, "unlinked-reuse-repo", cloneURL, prNumber, "first-mention", creatorCommenterID, "session-creator")
	firstStatus := postWebhook(t, rig, first, "delivery-unlinked-reuse-first")
	if firstStatus != http.StatusOK {
		t.Fatalf("first mention status = %d, want %d", firstStatus, http.StatusOK)
	}

	var sessionID string
	if err := rig.pool.QueryRow(ctx, `SELECT id::text FROM sessions WHERE spawn_source = 'github' AND repos::text LIKE '%unlinked-reuse-repo%'`).Scan(&sessionID); err != nil {
		t.Fatalf("query created session: %v", err)
	}

	// Second mention: a genuinely UNLINKED commenter (a real, non-zero
	// CommenterID with no identities row behind it) on the SAME PR.
	const unlinkedCommenterID = 90090002
	second := issueCommentBodyWithCommenter(repoFullName, "unlinked-reuse-repo", cloneURL, prNumber, "second-mention", unlinkedCommenterID, "unlinked-user")
	secondStatus := postWebhook(t, rig, second, "delivery-unlinked-reuse-second")
	if secondStatus != http.StatusOK {
		t.Fatalf("second mention status = %d, want %d (denied, but still acknowledged)", secondStatus, http.StatusOK)
	}

	var turnCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turnCount != 1 {
		t.Errorf("turn count = %d, want exactly 1 (the first mention's own turn only -- the unlinked second mention must not add one)", turnCount)
	}

	if len(poster.calls) != 1 {
		t.Fatalf("len(poster.calls) = %d, want exactly 1", len(poster.calls))
	}
	if !strings.Contains(poster.calls[0].body, testPublicBaseURL+"/auth/github/login") {
		t.Errorf("posted comment body = %q, want it to contain the sign-in URL", poster.calls[0].body)
	}
}

// TestGitHubIntegration_UnlinkedCommenter_RepeatMention_NoDuplicateReply is
// this batch's own anti-spam proof: a SECOND mention from the SAME
// still-unlinked commenter, on the SAME PR, within GitHubActorNoticeTTL of
// the first, must NOT post a second reply -- github_actor_link_notices'
// own dedupe row (upserted after the first reply) suppresses it.
func TestGitHubIntegration_UnlinkedCommenter_RepeatMention_NoDuplicateReply(t *testing.T) {
	rig, poster := newDenyUnlinkedTestRig(t)

	const repoFullName = "acme/unlinked-repeat-repo"
	const prNumber = 903
	const commenterID = 90090003

	first := issueCommentBodyWithCommenter(repoFullName, "unlinked-repeat-repo", "https://github.com/acme/unlinked-repeat-repo.git", prNumber, "first-mention", commenterID, "repeat-user")
	firstStatus := postWebhook(t, rig, first, "delivery-unlinked-repeat-first")
	if firstStatus != http.StatusOK {
		t.Fatalf("first mention status = %d, want %d", firstStatus, http.StatusOK)
	}
	if len(poster.calls) != 1 {
		t.Fatalf("after first mention: len(poster.calls) = %d, want exactly 1", len(poster.calls))
	}

	second := issueCommentBodyWithCommenter(repoFullName, "unlinked-repeat-repo", "https://github.com/acme/unlinked-repeat-repo.git", prNumber, "second-mention", commenterID, "repeat-user")
	secondStatus := postWebhook(t, rig, second, "delivery-unlinked-repeat-second")
	if secondStatus != http.StatusOK {
		t.Fatalf("second mention status = %d, want %d", secondStatus, http.StatusOK)
	}

	if len(poster.calls) != 1 {
		t.Errorf("after second (repeat, still within TTL) mention: len(poster.calls) = %d, want STILL exactly 1 (no duplicate reply)", len(poster.calls))
	}
}

// TestGitHubIntegration_UnlinkedCommenter_DifferentPR_GetsItsOwnReply
// proves the dedupe key is scoped per (repo, PR, commenter), not globally
// per commenter: the SAME still-unlinked commenter mentioning the bot on a
// DIFFERENT PR gets their own, independent reply -- not suppressed by the
// notice already recorded for the first PR.
func TestGitHubIntegration_UnlinkedCommenter_DifferentPR_GetsItsOwnReply(t *testing.T) {
	rig, poster := newDenyUnlinkedTestRig(t)

	const repoFullName = "acme/unlinked-multi-pr-repo"
	const commenterID = 90090004

	first := issueCommentBodyWithCommenter(repoFullName, "unlinked-multi-pr-repo", "https://github.com/acme/unlinked-multi-pr-repo.git", 904, "first-pr-mention", commenterID, "multi-pr-user")
	if status := postWebhook(t, rig, first, "delivery-unlinked-multi-pr-1"); status != http.StatusOK {
		t.Fatalf("first PR mention status = %d, want %d", status, http.StatusOK)
	}

	second := issueCommentBodyWithCommenter(repoFullName, "unlinked-multi-pr-repo", "https://github.com/acme/unlinked-multi-pr-repo.git", 905, "second-pr-mention", commenterID, "multi-pr-user")
	if status := postWebhook(t, rig, second, "delivery-unlinked-multi-pr-2"); status != http.StatusOK {
		t.Fatalf("second PR mention status = %d, want %d", status, http.StatusOK)
	}

	if len(poster.calls) != 2 {
		t.Errorf("len(poster.calls) = %d, want exactly 2 (a different PR gets its own, independent reply)", len(poster.calls))
	}
}

// TestGitHubIntegration_LinkedButDeniedCommenter_NoSignInReply is
// handler.go's own !actor.Valid branch guard, proven directly: a linked
// `viewer` (denied ActionCreateSession by the PRE-EXISTING role gate,
// nothing to do with this batch) must NOT get the "please sign in" reply
// -- that commenter is already signed in, so the reply would be actively
// wrong. Distinguishes this from every "unlinked" test above by wiring the
// SAME poster/PublicBaseURL and asserting NO call was made at all, not
// merely a different body.
func TestGitHubIntegration_LinkedButDeniedCommenter_NoSignInReply(t *testing.T) {
	ctx := context.Background()
	rig, poster := newDenyUnlinkedTestRig(t)

	const repoFullName = "acme/linked-denied-repo"
	const prNumber = 907
	const commenterID = 90090007

	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleViewer)

	body := issueCommentBodyWithCommenter(repoFullName, "linked-denied-repo", "https://github.com/acme/linked-denied-repo.git", prNumber, "linked-viewer-mention", commenterID, "linked-viewer-user")
	status := postWebhook(t, rig, body, "delivery-linked-denied-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (denied, but still acknowledged)", status, http.StatusOK)
	}

	var sessionCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE spawn_source = 'github' AND repos::text LIKE '%linked-denied-repo%'`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (a viewer must still be denied session creation, unchanged)", sessionCount)
	}

	if len(poster.calls) != 0 {
		t.Errorf("len(poster.calls) = %d, want 0 (a LINKED-but-denied commenter must never get the sign-in reply -- they are already signed in)", len(poster.calls))
	}
}

// TestGitHubIntegration_DeniedWinnerAttempt_ClaimRowNotOrphaned is the
// design decision's own explicitly-flagged claim/coalescing safety proof
// (not assumed, verified): a denied WINNER attempt (the very first mention
// ever received for a (repo, PR)) must roll back its own EnsureRow insert
// along with everything else in that transaction, leaving NO claim row
// behind -- so a SUBSEQUENT, legitimate mention on the SAME PR becomes a
// fresh, independent winner of its own, never blocked or confused by the
// earlier denial.
func TestGitHubIntegration_DeniedWinnerAttempt_ClaimRowNotOrphaned(t *testing.T) {
	ctx := context.Background()
	rig, _ := newDenyUnlinkedTestRig(t)

	const repoFullName = "acme/unlinked-claim-row-repo"
	const cloneURL = "https://github.com/acme/unlinked-claim-row-repo.git"
	const prNumber = 906

	denied := issueCommentBody(repoFullName, "unlinked-claim-row-repo", cloneURL, prNumber, "denied-first-attempt")
	if status := postWebhook(t, rig, denied, "delivery-unlinked-claim-row-1"); status != http.StatusOK {
		t.Fatalf("denied attempt status = %d, want %d", status, http.StatusOK)
	}

	var claimRowCount int
	if err := rig.pool.QueryRow(ctx,
		`SELECT count(*) FROM github_pr_sessions WHERE repo_full_name = $1 AND pr_number = $2`, repoFullName, prNumber,
	).Scan(&claimRowCount); err != nil {
		t.Fatalf("count claim rows after denied attempt: %v", err)
	}
	if claimRowCount != 0 {
		t.Fatalf("claim row count after denied WINNER attempt = %d, want 0 (EnsureRow's own insert must be rolled back along with the denial, not orphaned)", claimRowCount)
	}

	const legitimateCommenterID = 90090006
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, legitimateCommenterID, sqlcgen.UserRoleMaintainer)

	legitimate := issueCommentBodyWithCommenter(repoFullName, "unlinked-claim-row-repo", cloneURL, prNumber, "legitimate-second-attempt", legitimateCommenterID, "legitimate-user")
	if status := postWebhook(t, rig, legitimate, "delivery-unlinked-claim-row-2"); status != http.StatusOK {
		t.Fatalf("legitimate attempt status = %d, want %d", status, http.StatusOK)
	}

	var sessionCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE spawn_source = 'github' AND repos::text LIKE '%unlinked-claim-row-repo%'`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Errorf("session count = %d, want exactly 1 (the legitimate mention becomes a fresh, independent winner, unaffected by the earlier denied attempt)", sessionCount)
	}
}
