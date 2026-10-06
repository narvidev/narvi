//go:build integration

package github_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/platform"
)

// TestGitHubMention_AtSpendCap_AcknowledgedClaimKeptHonestReply: a mention
// on a pull request whose review session has spent its cap (technical plan
// §40.1) is acknowledged 200 without releasing the delivery's claim -- a
// redelivery would only meet the same refusal until an administrator raises
// the cap -- creates no turn, leaves mention_count alone, and is answered on
// the pull request with the refusal's own text. The crossing's warning is
// recorded once, and the reply on the pull request, the session's own
// channel, is the crossing's one telling: no outbox notice posts the same
// text there again. A second refused mention replies again and records
// nothing new.
func TestGitHubMention_AtSpendCap_AcknowledgedClaimKeptHonestReply(t *testing.T) {
	ctx := context.Background()

	poster := &fakeCommentPoster{}
	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.Comments = poster
		cfg.Outbound = platform.MustNewGitHubOutboundConfig("test-bot-token")
	})

	const repoFullName = "acme/spend-cap-repo"
	const cloneURL = "https://github.com/acme/spend-cap-repo.git"
	const prNumber = 708
	const commenterID = 80000708
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	if status := postWebhook(t, rig, issueCommentBodyWithCommenter(repoFullName, "spend-cap-repo", cloneURL, prNumber, "first-mention", commenterID, "spend-cap-user"), "delivery-spend-cap-1"); status != http.StatusOK {
		t.Fatalf("first delivery status = %d, want %d", status, http.StatusOK)
	}
	var sessionID pgtype.UUID
	if err := rig.pool.QueryRow(ctx, `SELECT session_id FROM github_pr_sessions WHERE repo_full_name = $1 AND pr_number = $2`, repoFullName, prNumber).Scan(&sessionID); err != nil {
		t.Fatalf("query claim row session id: %v", err)
	}

	// The first mention's turn ran and cost $1.25; the repository is capped
	// at $1.00.
	if _, err := rig.pool.Exec(ctx, `UPDATE turns SET status = 'completed', dispatched_at = now(), completed_at = now(), cost_usd = 1.25 WHERE session_id = $1`, sessionID); err != nil {
		t.Fatalf("record the first turn's spend: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, 1.00)
		ON CONFLICT (repo_full_name) DO UPDATE SET session_spend_cap_usd = EXCLUDED.session_spend_cap_usd`, repoFullName); err != nil {
		t.Fatalf("cap the repository: %v", err)
	}
	refusal := sessionguard.Refusal{
		Reason: sessionguard.ReasonSpendCap, SessionID: sessionID.Bytes, Cap: 1_000_000, Spent: 1_250_000,
		Source: sessionguard.CapSource{Kind: sessionguard.CapSourceRepo, Name: repoFullName, ID: repoFullName},
		Turns:  1,
	}

	for i, deliveryID := range []string{"delivery-spend-cap-2", "delivery-spend-cap-3"} {
		if status := postWebhook(t, rig, issueCommentBodyWithCommenter(repoFullName, "spend-cap-repo", cloneURL, prNumber, deliveryID, commenterID, "spend-cap-user"), deliveryID); status != http.StatusOK {
			t.Fatalf("%s status = %d, want %d: a session at its cap is a deterministic state, never a 500", deliveryID, status, http.StatusOK)
		}
		var claims int
		if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE provider = 'github' AND delivery_id = $1`, deliveryID).Scan(&claims); err != nil {
			t.Fatalf("count webhook_deliveries rows: %v", err)
		}
		if claims != 1 {
			t.Errorf("%s: webhook_deliveries rows = %d, want 1: the claim is kept", deliveryID, claims)
		}
		if len(poster.calls) != i+1 {
			t.Fatalf("%s: comments posted = %d, want %d", deliveryID, len(poster.calls), i+1)
		}
		got := poster.calls[i]
		if got.owner != "acme" || got.repo != "spend-cap-repo" || got.prNumber != prNumber || got.token != "test-bot-token" {
			t.Errorf("%s: reply posted to %s/%s#%d with %q, want acme/spend-cap-repo#%d with the bot token", deliveryID, got.owner, got.repo, got.prNumber, got.token, prNumber)
		}
		if got.body != sessionguard.Text(refusal) {
			t.Errorf("%s: reply = %q, want the refusal's text", deliveryID, got.body)
		}
	}

	var turns, mentionCount, warnings, notices int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if turns != 1 {
		t.Errorf("turns = %d, want the first mention's alone", turns)
	}
	if err := rig.pool.QueryRow(ctx, `SELECT mention_count FROM github_pr_sessions WHERE repo_full_name = $1 AND pr_number = $2`, repoFullName, prNumber).Scan(&mentionCount); err != nil {
		t.Fatal(err)
	}
	if mentionCount != 1 {
		t.Errorf("mention_count = %d, want 1: a refused mention created no turn", mentionCount)
	}
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND message_id = $2`, sessionID, turnguard.WarningMessageID(refusal)).Scan(&warnings); err != nil {
		t.Fatal(err)
	}
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, string(ports.NotificationKindGitHubSessionGuard)).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if warnings != 1 || notices != 0 {
		t.Errorf("warnings %d, notices %d; want the crossing's warning, and no notice beside the replies on the pull request", warnings, notices)
	}
}
