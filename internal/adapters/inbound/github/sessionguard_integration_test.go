//go:build integration

package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/platform"
)

// This file is technical plan §40.1's spend cap as the code host's mention
// lane meets it: a mention on a review session that has spent its cap is
// acknowledged without releasing the delivery claim, answered on the pull
// request, and the crossing is told there exactly once -- by the reply when
// it lands, by the crossing's held notice when it does not.

const spendCapRepo = "acme/spend-cap-repo"

// reviewSessionAtCap lets a first mention create a review session on
// spendCapRepo's pull request prNumber, takes it past the repository's
// $1.00 cap ($1.25 spent on that mention's turn), and returns it with the
// refusal the guard then makes.
func reviewSessionAtCap(ctx context.Context, t *testing.T, rig testRig, prNumber int, commenterID int64) (pgtype.UUID, sessionguard.Refusal) {
	t.Helper()
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)
	if status := postWebhook(t, rig, issueCommentBodyWithCommenter(spendCapRepo, "spend-cap-repo", "https://github.com/"+spendCapRepo+".git", prNumber, "first-mention", commenterID, "spend-cap-user"), "delivery-spend-cap-first"); status != http.StatusOK {
		t.Fatalf("first delivery status = %d, want %d", status, http.StatusOK)
	}
	var sessionID pgtype.UUID
	if err := rig.pool.QueryRow(ctx, `SELECT session_id FROM github_pr_sessions WHERE repo_full_name = $1 AND pr_number = $2`, spendCapRepo, prNumber).Scan(&sessionID); err != nil {
		t.Fatalf("query claim row session id: %v", err)
	}
	// Under the session's row lock, as every writer of a session's turns
	// is: the session actor may be evaluating the first mention's turn for
	// dispatch, and a write it does not wait for could land between its
	// read of the turn and its read of the cap.
	tx, err := rig.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := narvipg.NewSessionStore(rig.pool).WithTx(tx).GetActorEpochForUpdate(ctx, sessionID); err != nil {
		t.Fatalf("lock the session: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE turns SET status = 'completed', dispatched_at = now(), completed_at = now(), cost_usd = 1.25 WHERE session_id = $1`, sessionID); err != nil {
		t.Fatalf("record the first turn's spend: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, 1.00)
		ON CONFLICT (repo_full_name) DO UPDATE SET session_spend_cap_usd = EXCLUDED.session_spend_cap_usd`, spendCapRepo); err != nil {
		t.Fatalf("cap the repository: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return sessionID, sessionguard.Refusal{
		Reason: sessionguard.ReasonSpendCap, SessionID: sessionID.Bytes, Cap: 1_000_000, Spent: 1_250_000,
		Source: sessionguard.CapSource{Kind: sessionguard.CapSourceRepo, Name: spendCapRepo, ID: spendCapRepo},
		Turns:  1,
	}
}

// guardNotices is the state of a session's code-host guard notices: held
// (pending, never attempted, due later), withdrawn (delivered in place,
// never attempted: the reply told the channel), and any other row.
type guardNotices struct {
	held, withdrawn, other int
}

func readGuardNotices(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) guardNotices {
	t.Helper()
	var n guardNotices
	if err := pool.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE status = 'pending' AND attempts = 0 AND next_attempt_at > now()),
			count(*) FILTER (WHERE status = 'delivered' AND attempts = 0),
			count(*) FILTER (WHERE NOT ((status = 'pending' AND attempts = 0 AND next_attempt_at > now()) OR (status = 'delivered' AND attempts = 0)))
		FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, string(ports.NotificationKindGitHubSessionGuard)).Scan(&n.held, &n.withdrawn, &n.other); err != nil {
		t.Fatalf("read the guard notices: %v", err)
	}
	return n
}

// recordAutomaticRefusal is an automatic producer meeting the same crossing
// (a push's re-review, an owed request, a queued turn at dispatch): in a
// transaction holding the session's row lock it asks the guard and records
// the refusal with its notice, as the session actor does. It returns what
// was recorded.
func recordAutomaticRefusal(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) turnguard.Recorded {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := narvipg.NewSessionStore(pool).WithTx(tx).GetActorEpochForUpdate(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	bound := turnguard.New(pool, nil, false).WithTx(tx, nil)
	_, refusal, err := bound.Admit(ctx, sessionID, sessionguard.OriginAutomatic, turnguard.StageAutoRetrigger)
	if err != nil || refusal == nil {
		t.Fatalf("want an automatic refusal, got %v (error %v)", refusal, err)
	}
	sessionRow, err := narvipg.NewSessionStore(pool).WithTx(tx).Get(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := bound.Record(ctx, sessionRow, refusal, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return recorded
}

// TestGitHubMention_AtSpendCap_AcknowledgedClaimKeptHonestReply: a mention
// on a pull request whose review session has spent its cap (technical plan
// §40.1) is acknowledged 200 without releasing the delivery's claim -- a
// redelivery would only meet the same refusal until an administrator raises
// the cap -- creates no turn, leaves mention_count alone, and is answered on
// the pull request with the refusal's own text. The crossing's warning is
// recorded once. The reply landed, so it is the crossing's one telling: the
// notice held while it was tried is withdrawn, never delivered, and neither
// a second refused mention nor a later automatic refusal in the same
// crossing tells the pull request again.
func TestGitHubMention_AtSpendCap_AcknowledgedClaimKeptHonestReply(t *testing.T) {
	ctx := context.Background()

	poster := &fakeCommentPoster{}
	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.Comments = poster
		cfg.Outbound = platform.MustNewGitHubOutboundConfig("test-bot-token")
	})
	const prNumber = 708
	const commenterID = 80000708
	sessionID, refusal := reviewSessionAtCap(ctx, t, rig, prNumber, commenterID)

	for i, deliveryID := range []string{"delivery-spend-cap-2", "delivery-spend-cap-3"} {
		if status := postWebhook(t, rig, issueCommentBodyWithCommenter(spendCapRepo, "spend-cap-repo", "https://github.com/"+spendCapRepo+".git", prNumber, deliveryID, commenterID, "spend-cap-user"), deliveryID); status != http.StatusOK {
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

	var turns, mentionCount, warnings int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if turns != 1 {
		t.Errorf("turns = %d, want the first mention's alone", turns)
	}
	if err := rig.pool.QueryRow(ctx, `SELECT mention_count FROM github_pr_sessions WHERE repo_full_name = $1 AND pr_number = $2`, spendCapRepo, prNumber).Scan(&mentionCount); err != nil {
		t.Fatal(err)
	}
	if mentionCount != 1 {
		t.Errorf("mention_count = %d, want 1: a refused mention created no turn", mentionCount)
	}
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND message_id = $2`, sessionID, turnguard.WarningMessageID(refusal)).Scan(&warnings); err != nil {
		t.Fatal(err)
	}
	if warnings != 1 {
		t.Errorf("warnings = %d, want the crossing's one", warnings)
	}
	if n := readGuardNotices(ctx, t, rig.pool, sessionID); n != (guardNotices{withdrawn: 1}) {
		t.Errorf("guard notices %+v, want the one held notice withdrawn: the reply on the pull request told the crossing", n)
	}

	if recorded := recordAutomaticRefusal(ctx, t, rig.pool, sessionID); recorded.WarningInserted || recorded.NoticeEnqueued {
		t.Fatalf("a later automatic refusal recorded %+v, want nothing new: the crossing was told", recorded)
	}
	if n := readGuardNotices(ctx, t, rig.pool, sessionID); n != (guardNotices{withdrawn: 1}) {
		t.Errorf("guard notices after a later automatic refusal %+v, want the withdrawn one alone", n)
	}
}

// recordingGitHubNotifier records the text of every notice the outbox
// delivers.
type recordingGitHubNotifier struct {
	mu    sync.Mutex
	texts []string
}

func (n *recordingGitHubNotifier) Deliver(_ context.Context, notification ports.Notification) error {
	var payload githubapi.Payload
	if err := json.Unmarshal(notification.Payload, &payload); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.texts = append(n.texts, payload.Text)
	return nil
}

// TestGitHubMention_AtSpendCap_FailedReplyLeavesOneDurableNotice: when the
// reply on the pull request fails (a code host 502), the crossing is still
// told exactly once: the notice held while the reply was tried stays, due
// once its hold ends; a later automatic refusal in the same crossing adds
// nothing, the held notice already telling it; and the outbox delivers
// that one notice, the refusal's own text, to the pull request.
func TestGitHubMention_AtSpendCap_FailedReplyLeavesOneDurableNotice(t *testing.T) {
	ctx := context.Background()

	poster := &fakeCommentPoster{}
	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.Comments = poster
		cfg.Outbound = platform.MustNewGitHubOutboundConfig("test-bot-token")
	})
	const prNumber = 709
	const commenterID = 80000709
	sessionID, refusal := reviewSessionAtCap(ctx, t, rig, prNumber, commenterID)
	// A repository whose egress is live: its notices reach the pull request.
	if _, err := narvipg.NewRepoSettingsStore(rig.pool).UpsertLiveEgressEnabled(ctx, spendCapRepo, true); err != nil {
		t.Fatalf("promote the repository to live egress: %v", err)
	}

	poster.err = errors.New("github: 502 Bad Gateway")
	var mentionedAt time.Time
	if err := rig.pool.QueryRow(ctx, `SELECT now()`).Scan(&mentionedAt); err != nil {
		t.Fatal(err)
	}
	if status := postWebhook(t, rig, issueCommentBodyWithCommenter(spendCapRepo, "spend-cap-repo", "https://github.com/"+spendCapRepo+".git", prNumber, "refused-mention", commenterID, "spend-cap-user"), "delivery-spend-cap-failed"); status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(poster.calls) != 1 {
		t.Fatalf("reply attempts = %d, want 1", len(poster.calls))
	}
	if n := readGuardNotices(ctx, t, rig.pool, sessionID); n != (guardNotices{held: 1}) {
		t.Fatalf("guard notices %+v, want one held notice: the reply failed, so the crossing is still to be told", n)
	}
	var due time.Time
	if err := rig.pool.QueryRow(ctx, `SELECT next_attempt_at FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, string(ports.NotificationKindGitHubSessionGuard)).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if hold, dueIn := platform.DefaultTimeouts().SessionGuardNoticeHold, due.Sub(mentionedAt); dueIn < hold || dueIn > hold+time.Minute {
		t.Fatalf("the notice is due %v after the mention, want the notice hold, %v, past it", dueIn, hold)
	}

	if recorded := recordAutomaticRefusal(ctx, t, rig.pool, sessionID); recorded.WarningInserted || recorded.NoticeEnqueued {
		t.Fatalf("a later automatic refusal recorded %+v, want nothing new: the held notice tells the crossing", recorded)
	}

	// The hold ends; the outbox delivers the one notice.
	if _, err := rig.pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() WHERE session_id = $1 AND kind = $2`, sessionID, string(ports.NotificationKindGitHubSessionGuard)); err != nil {
		t.Fatal(err)
	}
	notifier := &recordingGitHubNotifier{}
	builder, err := outboxworker.NewBuilder(narvipg.NewOutboxStore(rig.pool, false), rig.pool,
		map[ports.NotificationKind]ports.Notifier{ports.NotificationKindGitHubSessionGuard: notifier}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatal(err)
	}
	notifier.mu.Lock()
	texts := append([]string(nil), notifier.texts...)
	notifier.mu.Unlock()
	if len(texts) != 1 || texts[0] != sessionguard.Text(refusal) {
		t.Fatalf("notices delivered = %q, want the refusal's text once", texts)
	}
}
