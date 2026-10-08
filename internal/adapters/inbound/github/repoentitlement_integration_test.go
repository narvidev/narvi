//go:build integration

// This file proves technical plan §31.4's "Un-entitlement" on GitHub: a
// pull-request mention on a repository an administrator revoked is refused
// before either of CreateOrJoin's branches runs -- a new mention, a
// follow-up on an existing review session, a fork pull request's mention --
// with the permanent-denial idiom: 200, the webhook-delivery claim kept,
// nothing posted on the pull request, no session and no turn. The refusal
// is keyed by the pull request's base repository, which a fork's clone URL
// does not name.
package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// forkReviewCommentBody is a "pull_request_review_comment" mention on
// baseFullName's pull request prNumber whose head lives in a fork,
// contributor/<baseName>, cloned from forkCloneURL -- head.repo.full_name
// names the fork as the head's repository, and the top-level repository
// the base as the pull request's own, the one a review session clones
// (technical plan §30.4).
func forkReviewCommentBody(baseFullName, baseName, forkCloneURL string, prNumber int, label string, commenterID int64, commenterLogin string) []byte {
	body, err := json.Marshal(map[string]any{
		"action": "created",
		"comment": map[string]any{
			"id":   int64(prNumber)*1000 + 1,
			"body": fmt.Sprintf("@%s please review (%s)", testBotHandleIntegration, label),
			"user": map[string]any{"id": commenterID, "login": commenterLogin},
		},
		"pull_request": map[string]any{
			"number": prNumber,
			"head": map[string]any{
				"ref": "contributor-patch",
				"sha": "sha-fork-head",
				// full_name is what the ingress reads to tell a fork's
				// head from a branch of the base (reviewSessionRepo).
				"repo": map[string]any{"name": baseName, "full_name": "contributor/" + baseName, "clone_url": forkCloneURL},
			},
		},
		"repository": map[string]any{
			"full_name": baseFullName,
			"name":      baseName,
			"clone_url": "https://github.com/" + baseFullName + ".git",
		},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// revokeRepo records an administrator's revocation of repoFullName.
func revokeRepo(ctx context.Context, t *testing.T, rig testRig, repoFullName string) {
	t.Helper()
	if _, err := narvipg.NewRepoEntitlementRevocationStore(rig.pool).Revoke(ctx, repoFullName, pgtype.UUID{}, "frozen by an administrator"); err != nil {
		t.Fatalf("revoke %s: %v", repoFullName, err)
	}
}

// countRows runs a count query on the rig's pool.
func countRows(ctx context.Context, t *testing.T, rig testRig, query string, args ...any) int {
	t.Helper()
	var n int
	if err := rig.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// assertSilentRefusal checks the permanent-denial idiom for deliveryID on
// repoFullName's pull request: the claim kept, nothing posted, one more
// denial audited with reason "revoked".
func assertSilentRefusal(ctx context.Context, t *testing.T, rig testRig, poster *fakeCommentPoster, deliveryID, repoFullName string, wantDenials int) {
	t.Helper()
	if n := countRows(ctx, t, rig, `SELECT count(*) FROM webhook_deliveries WHERE provider = 'github' AND delivery_id = $1`, deliveryID); n != 1 {
		t.Errorf("webhook_deliveries rows for %s = %d, want 1 (the claim kept: a redelivery would meet the same revocation)", deliveryID, n)
	}
	if len(poster.calls) != 0 {
		t.Errorf("comments posted = %v, want none: a revoked repository's pull request gets no reply", poster.calls)
	}
	if n := countRows(ctx, t, rig, `SELECT count(*) FROM audit_log WHERE action = 'session.repo_entitlement_denied' AND resource_id = $1 AND detail_json->>'reason' = 'revoked' AND detail_json->>'spawn_source' = 'github'`, repoFullName); n != wantDenials {
		t.Errorf("revoked denial audit rows for %s = %d, want %d", repoFullName, n, wantDenials)
	}
}

// TestGitHubIntegration_RevokedRepo_NewMentionSilentClaimKept: the first
// mention on a revoked repository's pull request creates nothing -- no
// session, no claim row -- answers 200, keeps the delivery claim, posts
// nothing, and audits the refusal.
func TestGitHubIntegration_RevokedRepo_NewMentionSilentClaimKept(t *testing.T) {
	ctx := context.Background()
	rig, _, poster := newRolloutTestRig(t, platform.RolloutModeOpen)

	const repoFullName = "acme/revoked-new-mention"
	const prNumber = 1510
	const commenterID = 90152001
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)
	revokeRepo(ctx, t, rig, repoFullName)

	body := issueCommentBodyWithCommenter(repoFullName, "revoked-new-mention", "https://github.com/"+repoFullName+".git", prNumber, "revoked-new", commenterID, "revoked-new-user")
	const deliveryID = "delivery-revoked-new-mention-1"
	if status := postWebhook(t, rig, body, deliveryID); status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (acknowledged, never retried)", status)
	}

	if n := countRows(ctx, t, rig, `SELECT count(*) FROM sessions WHERE spawn_source = 'github'`); n != 0 {
		t.Errorf("sessions = %d, want none for a revoked repository", n)
	}
	if n := countRows(ctx, t, rig, `SELECT count(*) FROM github_pr_sessions WHERE repo_full_name = $1`, repoFullName); n != 0 {
		t.Errorf("claim rows = %d, want none", n)
	}
	assertSilentRefusal(ctx, t, rig, poster, deliveryID, repoFullName, 1)
}

// TestGitHubIntegration_RevokedRepo_FollowUpMentionCreatesNoTurn: a review
// session that existed before the revocation gets no new turn from a later
// mention -- the REUSE branch is refused as well as the WINNER one -- and
// its existing turns are left as they were.
func TestGitHubIntegration_RevokedRepo_FollowUpMentionCreatesNoTurn(t *testing.T) {
	ctx := context.Background()
	rig, _, poster := newRolloutTestRig(t, platform.RolloutModeOpen)

	const repoFullName = "acme/revoked-follow-up"
	const cloneURL = "https://github.com/acme/revoked-follow-up.git"
	const prNumber = 1511
	const commenterID = 90152002
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	first := issueCommentBodyWithCommenter(repoFullName, "revoked-follow-up", cloneURL, prNumber, "before-revocation", commenterID, "follow-up-user")
	if status := postWebhook(t, rig, first, "delivery-revoked-follow-up-1"); status != http.StatusOK {
		t.Fatalf("first mention status = %d, want 200", status)
	}
	if n := countRows(ctx, t, rig, `SELECT count(*) FROM github_pr_sessions WHERE repo_full_name = $1 AND session_id IS NOT NULL`, repoFullName); n != 1 {
		t.Fatalf("claim rows after the first mention = %d, want the review session's", n)
	}
	turnsBefore := countRows(ctx, t, rig, `SELECT count(*) FROM turns t JOIN github_pr_sessions g ON g.session_id = t.session_id WHERE g.repo_full_name = $1`, repoFullName)
	if turnsBefore != 1 {
		t.Fatalf("turns after the first mention = %d, want 1", turnsBefore)
	}

	revokeRepo(ctx, t, rig, repoFullName)
	poster.calls = nil

	followUp := issueCommentBodyWithCommenter(repoFullName, "revoked-follow-up", cloneURL, prNumber, "after-revocation", commenterID, "follow-up-user")
	const deliveryID = "delivery-revoked-follow-up-2"
	if status := postWebhook(t, rig, followUp, deliveryID); status != http.StatusOK {
		t.Fatalf("follow-up status = %d, want 200", status)
	}
	if n := countRows(ctx, t, rig, `SELECT count(*) FROM turns t JOIN github_pr_sessions g ON g.session_id = t.session_id WHERE g.repo_full_name = $1`, repoFullName); n != turnsBefore {
		t.Errorf("turns after the follow-up = %d, want still %d: a revoked repository's review session gets no new turn", n, turnsBefore)
	}
	if n := countRows(ctx, t, rig, `SELECT mention_count FROM github_pr_sessions WHERE repo_full_name = $1`, repoFullName); n != 1 {
		t.Errorf("mention_count = %d, want 1: a refused mention is not coalesced", n)
	}
	assertSilentRefusal(ctx, t, rig, poster, deliveryID, repoFullName, 1)
}

// TestGitHubIntegration_RevokedRepo_ForkPRMentionRefused: a fork pull
// request's mention names the fork as its head's repository; the
// revocation of its base repository refuses it.
func TestGitHubIntegration_RevokedRepo_ForkPRMentionRefused(t *testing.T) {
	ctx := context.Background()
	rig, _, poster := newRolloutTestRig(t, platform.RolloutModeOpen)

	const baseFullName = "acme/revoked-fork-base"
	const prNumber = 1512
	const commenterID = 90152003
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)
	revokeRepo(ctx, t, rig, baseFullName)

	body := forkReviewCommentBody(baseFullName, "revoked-fork-base", "https://github.com/contributor/revoked-fork-base.git", prNumber, "fork", commenterID, "fork-reviewer")
	const deliveryID = "delivery-revoked-fork-1"
	if status := postWebhookEventType(t, rig, body, deliveryID, "pull_request_review_comment"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if n := countRows(ctx, t, rig, `SELECT count(*) FROM sessions WHERE spawn_source = 'github'`); n != 0 {
		t.Errorf("sessions = %d, want none: the fork's clone URL must not carry the mention past its base repository's revocation", n)
	}
	assertSilentRefusal(ctx, t, rig, poster, deliveryID, baseFullName, 1)

	// The positive control: the same fork pull request on a base repository
	// nobody revoked creates its review session, cloning the base, never
	// the fork (technical plan §30.4).
	const openBase = "acme/open-fork-base"
	open := forkReviewCommentBody(openBase, "open-fork-base", "https://github.com/contributor/open-fork-base.git", prNumber+1, "fork-open", commenterID, "fork-reviewer")
	if status := postWebhookEventType(t, rig, open, "delivery-open-fork-1", "pull_request_review_comment"); status != http.StatusOK {
		t.Fatalf("open fork status = %d, want 200", status)
	}
	if n := countRows(ctx, t, rig, `SELECT count(*) FROM sessions WHERE spawn_source = 'github' AND repos->0->>'url' = 'https://github.com/acme/open-fork-base.git' AND repos->0->'branch' = 'null'::jsonb`); n != 1 {
		t.Errorf("sessions cloning the base, with no branch = %d, want 1 for a base repository nobody revoked", n)
	}
	if n := countRows(ctx, t, rig, `SELECT count(*) FROM sessions WHERE repos::text LIKE '%contributor/%'`); n != 0 {
		t.Errorf("sessions naming the fork = %d, want none", n)
	}
}
