//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	domainreviewtriage "github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// This file pins technical plan §21.1's review checkout
// (reviewcheckout.go) on real Postgres, through the real actor: a turn of
// a pull request's review session that records a head stays pending until
// its sandbox reports holding that head, and only then is its prompt sent.
// The sandbox is a fake commander that records every command, and its
// replies are sandbox events the test sends, as the agent would.

const (
	// coHead is the head a review turn recorded; coMoved the head the pull
	// request's ref moved to while the turn waited.
	coHead  = "c0ffee0000000000000000000000000000000001"
	coMoved = "c0ffee0000000000000000000000000000000002"
)

// checkoutFixture is a review session claiming repoFullName#prNumber, its
// primary repo the claim's repository, with a ready sandbox at gen 1 whose
// capability nothing has recorded yet.
func checkoutFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, prNumber int32) *holdFixture {
	t.Helper()
	return newContextFixture(ctx, t, pool, repoFullName, prNumber)
}

// seedReviewTurn creates a pending turn of f recording head: a review
// attempt asked for by trigger (nil: none, as a mention-opened first
// review), or, with attempt false, a follow-up mention.
func seedReviewTurn(ctx context.Context, t *testing.T, f *holdFixture, head string, trigger *string, attempt bool) sqlcgen.Turn {
	t.Helper()
	prompt := "review the pull request at " + head
	created, err := f.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: f.sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt,
		ReviewHeadSha: &head, ReviewVerdictContext: recordedContext(t, defaultRecordedContext()),
		IsReviewAttempt: attempt, RequestTrigger: trigger,
	})
	if err != nil {
		t.Fatalf("create the review turn: %v", err)
	}
	return created
}

func triggerOf(v string) *string { return &v }

// i32 reads a count the turn stores NULL until its first checkout as 0.
func i32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// checkoutReady is a ready of gen, advertising reviewCheckout when
// capable.
func checkoutReady(gen int, capable bool) SandboxEvent {
	id := "ready-" + uuid.NewString()
	caps := ""
	if capable {
		caps = `,"capabilities":{"reviewCheckout":true}`
	}
	agentVersion, imageDigest := "dev", "unknown"
	return SandboxEvent{Type: "ready", Gen: gen, MessageID: id, AgentVersion: &agentVersion, ImageDigest: &imageDigest,
		Raw: json.RawMessage(fmt.Sprintf(`{"type":"ready","messageId":%q,"sessionId":"s","gen":%d,"timestamp":"2026-10-01T12:00:00Z","agentVersion":"dev","imageDigest":"unknown"%s}`, id, gen, caps))}
}

// checkoutResultRaw is the agent's checkout_result answering cmd, for its
// one repo, at gen.
func checkoutResultRaw(t *testing.T, cmd sandboxws.Checkout, gen int, outcome string, head, ref, errText *string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "checkout_result", "messageId": "checkout_result:" + cmd.MessageId, "sessionId": cmd.SessionId,
		"gen": gen, "commandMessageId": cmd.MessageId,
		"repos": []map[string]any{{"name": cmd.Repos[0].Name, "outcome": outcome, "headSha": head, "refSha": ref, "error": errText}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// checkoutReply is checkoutResultRaw as the sandbox event the agent sends.
func checkoutReply(t *testing.T, cmd sandboxws.Checkout, gen int, outcome string, head, ref, errText *string) SandboxEvent {
	t.Helper()
	return SandboxEvent{Type: "checkout_result", Gen: gen, MessageID: "checkout_result:" + cmd.MessageId,
		Raw: checkoutResultRaw(t, cmd, gen, outcome, head, ref, errText)}
}

func strp(v string) *string { return &v }

// sentCommandTypes returns the type of every command c sent, in order.
func sentCommandTypes(t *testing.T, c *fakeSendCommander) []string {
	t.Helper()
	var out []string
	for _, payload := range sentPayloads(c) {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &head); err != nil {
			t.Fatalf("decode a sent command: %v", err)
		}
		out = append(out, head.Type)
	}
	return out
}

// sentCheckouts returns every checkout command c sent, each passing the
// contract's own decoder.
func sentCheckouts(t *testing.T, c *fakeSendCommander) []sandboxws.Checkout {
	t.Helper()
	var out []sandboxws.Checkout
	for _, payload := range sentPayloads(c) {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &head); err != nil {
			t.Fatalf("decode a sent command: %v", err)
		}
		if head.Type != "checkout" {
			continue
		}
		var cmd sandboxws.Checkout
		if err := json.Unmarshal(payload, &cmd); err != nil {
			t.Fatalf("a checkout fails its contract: %v (%s)", err, payload)
		}
		out = append(out, cmd)
	}
	return out
}

// sentPromptsOf returns every prompt c sent.
func sentPromptsOf(t *testing.T, c *fakeSendCommander) []sandboxws.Prompt {
	t.Helper()
	var out []sandboxws.Prompt
	for _, payload := range sentPayloads(c) {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &head); err != nil {
			t.Fatalf("decode a sent command: %v", err)
		}
		if head.Type != "prompt" {
			continue
		}
		var p sandboxws.Prompt
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("a prompt fails its contract: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// lastCheckout returns the last checkout c sent, failing unless exactly n
// were sent.
func lastCheckout(t *testing.T, c *fakeSendCommander, n int) sandboxws.Checkout {
	t.Helper()
	got := sentCheckouts(t, c)
	if len(got) != n {
		t.Fatalf("%d checkouts sent, want %d (commands %v)", len(got), n, sentCommandTypes(t, c))
	}
	return got[n-1]
}

func getTurn(ctx context.Context, t *testing.T, f *holdFixture, id pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	row, err := f.turns.Get(ctx, id)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	return row
}

// dispatchTimerLead is how far ahead of the database's now the session's
// dispatch timer fires; ok is false when it has none.
func dispatchTimerLead(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) (time.Duration, bool) {
	t.Helper()
	var nanos *int64
	err := pool.QueryRow(ctx, `SELECT (EXTRACT(EPOCH FROM (fires_at - now())) * 1000000000)::bigint FROM session_timers WHERE session_id = $1 AND name = $2`,
		sessionID, TimerDispatch).Scan(&nanos)
	if err != nil || nanos == nil {
		return 0, false
	}
	return time.Duration(*nanos), true
}

// backdateCheckout moves the turn's checkout bound and latest send into the
// past, on the database's clock, as if that much time had passed.
func backdateCheckout(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id pgtype.UUID, sinceRequest, sinceSend time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE turns SET checkout_requested_at = now() - make_interval(secs => $2::double precision),
		checkout_sent_at = now() - make_interval(secs => $3::double precision) WHERE id = $1`, id, sinceRequest.Seconds(), sinceSend.Seconds()); err != nil {
		t.Fatalf("backdate the checkout: %v", err)
	}
}

// shiftCheckout moves the turn's checkout bound and latest send d into the
// past together, as if d had passed since each: a send made since the bound
// started shows in the gap between them.
func shiftCheckout(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id pgtype.UUID, d time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE turns SET checkout_requested_at = checkout_requested_at - make_interval(secs => $2::double precision),
		checkout_sent_at = checkout_sent_at - make_interval(secs => $2::double precision) WHERE id = $1`, id, d.Seconds()); err != nil {
		t.Fatalf("shift the checkout: %v", err)
	}
}

func reviewCheckoutCount(ctx context.Context, t *testing.T, outcome string) int64 {
	t.Helper()
	return readCounterSumByAttr(ctx, t, otelReader, "review_checkout_total", "outcome", outcome)
}

// barrier returns once a has finished every command sent before it, the
// post-commit dispatch evaluation of each included: a frame with no type
// is dropped with a reply and runs nothing after it, and the mailbox is
// handled in order. A heartbeat cannot stand in for it, since its own
// evaluation runs after its reply.
func barrier(ctx context.Context, t *testing.T, a *Actor) {
	t.Helper()
	sendSandboxEvent(ctx, t, a, SandboxEvent{MessageID: "barrier-" + uuid.NewString(), Raw: json.RawMessage(`{}`)})
}

// deliver hands the actor cmd, as the sandbox's socket does, and returns
// once it has been handled, its dispatch evaluation included.
func deliver(ctx context.Context, t *testing.T, a *Actor, cmd SandboxEvent) {
	t.Helper()
	sendSandboxEvent(ctx, t, a, cmd)
	barrier(ctx, t, a)
}

// settle hands the actor an EnsureDispatched -- a dispatch timer's firing,
// a turn's creation -- and returns once it has been handled.
func settle(ctx context.Context, t *testing.T, a *Actor) {
	t.Helper()
	sendEnsureDispatched(ctx, t, a)
	barrier(ctx, t, a)
}

// TestReviewCheckout_TheTurnIsSentOnlyAfterItsSandboxReportsTheRecordedHead
// is exit 3's order (technical plan §21.1): the review turn's checkout is
// sent first, naming the pull request's ref and the head the turn
// recorded, and recorded in the evaluation's own commit; the turn stays
// pending -- and the actor goes on handling its mailbox -- until the
// sandbox's reply says it holds that head; then the prompt is sent, and the
// commit is recorded on the turn.
func TestReviewCheckout_TheTurnIsSentOnlyAfterItsSandboxReportsTheRecordedHead(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-order", 701)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)
	sentBefore, checkedOutBefore := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeSent), reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeCheckedOut)

	deliver(ctx, t, rig.actor, checkoutReady(1, true))

	if got := sentCommandTypes(t, rig.commander); len(got) != 1 || got[0] != "checkout" {
		t.Fatalf("commands sent before any reply = %v, want one checkout and no prompt", got)
	}
	cmd := lastCheckout(t, rig.commander, 1)
	if cmd.Gen != 1 || cmd.SessionId != f.sessionID.String() || len(cmd.Repos) != 1 ||
		cmd.Repos[0] != (sandboxws.CheckoutReposElem{Name: "widgets", Ref: "refs/pull/701/head", Sha: coHead}) {
		t.Fatalf("checkout = %+v, want gen 1 asking widgets for refs/pull/701/head at %s", cmd, coHead)
	}
	waiting := getTurn(ctx, t, f, reviewTurn.ID)
	if waiting.Status != sqlcgen.TurnStatusPending || waiting.DispatchedAt.Valid {
		t.Fatalf("turn while its checkout is outstanding: %s, dispatched %v; want pending, not dispatched", waiting.Status, waiting.DispatchedAt.Valid)
	}
	if waiting.CheckoutMessageID == nil || *waiting.CheckoutMessageID != cmd.MessageId || waiting.CheckoutGen == nil || *waiting.CheckoutGen != 1 ||
		i32(waiting.CheckoutSends) != 1 || !waiting.CheckoutRequestedAt.Valid || !waiting.CheckoutSentAt.Valid || waiting.CheckedOutSha != nil {
		t.Fatalf("recorded request = message %v gen %v sends %d requested %v sent %v checked out %v; want the command's, gen 1, 1, both stamped, none",
			waiting.CheckoutMessageID, waiting.CheckoutGen, i32(waiting.CheckoutSends), waiting.CheckoutRequestedAt.Valid, waiting.CheckoutSentAt.Valid, waiting.CheckedOutSha)
	}
	// The handler returned without the reply: the heartbeat after the ready
	// was handled, and more are, with the turn still pending.
	for i := 0; i < 3; i++ {
		sendSandboxEvent(ctx, t, rig.actor, receiptHeartbeat(1))
	}
	barrier(ctx, t, rig.actor)
	if got := sentCommandTypes(t, rig.commander); len(got) != 1 {
		t.Fatalf("commands after heartbeats with no reply = %v, want the one checkout", got)
	}

	deliver(ctx, t, rig.actor, checkoutReply(t, cmd, 1, "checked_out", strp(coHead), strp(coHead), nil))

	if got := sentCommandTypes(t, rig.commander); len(got) != 2 || got[0] != "checkout" || got[1] != "prompt" {
		t.Fatalf("commands = %v, want the checkout, then the prompt", got)
	}
	prompt := sentPromptsOf(t, rig.commander)[0]
	if !strings.Contains(prompt.Text, coHead) {
		t.Fatalf("the prompt sent is not the turn's: %q", prompt.Text)
	}
	dispatched := getTurn(ctx, t, f, reviewTurn.ID)
	if dispatched.Status != sqlcgen.TurnStatusProcessing || dispatched.CheckedOutSha == nil || *dispatched.CheckedOutSha != coHead {
		t.Fatalf("turn after the reply: %s, checked out %v; want processing at %s", dispatched.Status, dispatched.CheckedOutSha, coHead)
	}
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND message_id = $2 AND type = 'checkout_result'`,
		f.sessionID, "checkout_result:"+cmd.MessageId).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored replies under the command's key = %d (%v), want 1", stored, err)
	}
	if got := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeSent) - sentBefore; got != 1 {
		t.Fatalf("review_checkout_total{sent} moved by %d, want 1", got)
	}
	if got := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeCheckedOut) - checkedOutBefore; got != 1 {
		t.Fatalf("review_checkout_total{checked_out} moved by %d, want 1", got)
	}
}

// TestReviewCheckout_TheWaitReArmsTheDispatchTimerAhead: every wait arms
// the session's dispatch timer ahead, on the database's clock -- at the
// bound after a send, at the re-fetch for a ref that lags -- never due at
// once, so a waiting turn never spins the timer pump.
func TestReviewCheckout_TheWaitReArmsTheDispatchTimerAhead(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-timer", 702)
	seedReviewTurn(ctx, t, f, coHead, nil, true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)
	timeouts := platform.DefaultTimeouts()

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	lead, ok := dispatchTimerLead(ctx, t, pool, f.sessionID)
	if !ok || lead < timeouts.ReviewCheckoutTimeout-time.Minute || lead > timeouts.ReviewCheckoutTimeout {
		t.Fatalf("dispatch timer after the send fires in %s (armed %v), want about ReviewCheckoutTimeout (%s) ahead", lead, ok, timeouts.ReviewCheckoutTimeout)
	}

	cmd := lastCheckout(t, rig.commander, 1)
	deliver(ctx, t, rig.actor, checkoutReply(t, cmd, 1, "sha_absent", nil, strp(coMoved), nil))
	lead, ok = dispatchTimerLead(ctx, t, pool, f.sessionID)
	if !ok || lead <= 0 || lead > timeouts.ReviewCheckoutRefetchInterval {
		t.Fatalf("dispatch timer after a lagging ref fires in %s (armed %v), want ahead, within ReviewCheckoutRefetchInterval (%s)", lead, ok, timeouts.ReviewCheckoutRefetchInterval)
	}
}

// TestReviewCheckout_AnAttemptWhoseHeadMovedEndsContextMoved is exit 4 for
// every lane that asks for a review attempt again: the sandbox holds the
// recorded head, but the pull request's ref is elsewhere, and still is past
// the lag window, so the attempt ends context_moved without running --
// technical plan §24.9's end -- and its request is asked again for the head
// the pull request has now: the automatic lane's pending head, or a
// person's request owed.
func TestReviewCheckout_AnAttemptWhoseHeadMovedEndsContextMoved(t *testing.T) {
	for i, trigger := range []string{turn.RequestTriggerAuto, turn.RequestTriggerLabel, turn.RequestTriggerButton} {
		t.Run(trigger, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			f := checkoutFixture(ctx, t, pool, "acme/co-moved-"+trigger, int32(710+i))
			attempt := seedReviewTurn(ctx, t, f, coHead, triggerOf(trigger), true)
			rig := newContextRig(ctx, t, pool, f.sessionID, nil)
			movedBefore := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeContextMoved)

			deliver(ctx, t, rig.actor, checkoutReady(1, true))
			cmd := lastCheckout(t, rig.commander, 1)
			deliver(ctx, t, rig.actor, checkoutReply(t, cmd, 1, "checked_out", strp(coHead), strp(coMoved), nil))
			// Inside the lag window the ref may still lag a head the clone
			// already holds: the attempt waits for a re-fetch.
			if got := getTurn(ctx, t, f, attempt.ID); got.Status != sqlcgen.TurnStatusPending || got.EndReason != nil {
				t.Fatalf("attempt inside the lag window: %s, end reason %v; want pending, waiting for a re-fetch", got.Status, got.EndReason)
			}
			backdateCheckout(ctx, t, pool, attempt.ID, platform.DefaultTimeouts().ReviewCheckoutRefLagWindow+time.Second, time.Second)
			settle(ctx, t, rig.actor)

			assertContextMoved(ctx, t, f, rig, attempt)
			if n := len(sentPromptsOf(t, rig.commander)); n != 0 {
				t.Fatalf("%d prompts sent: an attempt whose head moved never runs", n)
			}
			var reason string
			if err := pool.QueryRow(ctx, `SELECT payload->>'reason' FROM events WHERE session_id = $1 AND type = 'execution_complete' AND payload->>'turn_id' = $2`,
				f.sessionID, attempt.ID.String()).Scan(&reason); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(reason, coMoved) || !strings.Contains(reason, coHead) {
				t.Fatalf("end reason %q, want it to name the head the ref holds and the one recorded", reason)
			}
			if trigger == turn.RequestTriggerAuto {
				if row := f.prSession(ctx, t); row.PendingRetriggerHeadSha == nil || *row.PendingRetriggerHeadSha != coMoved || row.AutoRetriggerContextMoves != 1 {
					t.Fatalf("pending head %v, moves %d; want the live head %s asked again, one move", row.PendingRetriggerHeadSha, row.AutoRetriggerContextMoves, coMoved)
				}
			} else if n := countRows(ctx, t, pool, `SELECT count(*) FROM owed_review_requests WHERE session_id = $1 AND moved_turn_id = $2 AND trigger = $3`,
				f.sessionID, attempt.ID, trigger); n != 1 {
				t.Fatalf("%d owed requests for the moved attempt, want 1", n)
			}
			if got := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeContextMoved) - movedBefore; got != 1 {
				t.Fatalf("review_checkout_total{context_moved} moved by %d, want 1", got)
			}
		})
	}
}

// TestReviewCheckout_AHeadStillAbsentPastTheLagWindowIsMoved: a ref that
// lacks the recorded head is fetched again within the lag window; past it,
// the head is gone, and an attempt a lane asks for again ends
// context_moved.
func TestReviewCheckout_AHeadStillAbsentPastTheLagWindowIsMoved(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-absent", 720)
	attempt := seedReviewTurn(ctx, t, f, coHead, triggerOf(turn.RequestTriggerAuto), true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	cmd := lastCheckout(t, rig.commander, 1)
	deliver(ctx, t, rig.actor, checkoutReply(t, cmd, 1, "sha_absent", nil, strp(coMoved), nil))
	if got := getTurn(ctx, t, f, attempt.ID); got.Status != sqlcgen.TurnStatusPending {
		t.Fatalf("attempt after a lagging ref inside the window: %s, want pending", got.Status)
	}

	backdateCheckout(ctx, t, pool, attempt.ID, platform.DefaultTimeouts().ReviewCheckoutRefLagWindow+time.Second, time.Second)
	settle(ctx, t, rig.actor)
	assertContextMoved(ctx, t, f, rig, attempt)
	if row := f.prSession(ctx, t); row.PendingRetriggerHeadSha == nil || *row.PendingRetriggerHeadSha != coMoved {
		t.Fatalf("pending head %v, want the ref's tip %s asked again", row.PendingRetriggerHeadSha, coMoved)
	}
}

// TestReviewCheckout_AMentionsTurnRunsOnTheHeadItRecorded is exit 4's
// other half: a turn no lane asks for again -- a mention-opened first
// review, a follow-up mention -- whose pull request's ref moved runs
// anyway, on the head it recorded, checked out exactly: the command names
// that head, and the turn records it as the commit it ran on, never the
// ref's tip. Its verdict is anchored to that head (turns.review_head_sha),
// so every freshness reader shows it stale.
func TestReviewCheckout_AMentionsTurnRunsOnTheHeadItRecorded(t *testing.T) {
	for i, tc := range []struct {
		name    string
		attempt bool
	}{
		{name: "a mention-opened first review", attempt: true},
		{name: "a follow-up mention", attempt: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			f := checkoutFixture(ctx, t, pool, fmt.Sprintf("acme/co-mention-%d", i), int32(730+i))
			mention := seedReviewTurn(ctx, t, f, coHead, nil, tc.attempt)
			rig := newContextRig(ctx, t, pool, f.sessionID, nil)

			deliver(ctx, t, rig.actor, checkoutReady(1, true))
			cmd := lastCheckout(t, rig.commander, 1)
			if cmd.Repos[0].Sha != coHead {
				t.Fatalf("checkout asks for %s, want the recorded head %s", cmd.Repos[0].Sha, coHead)
			}
			deliver(ctx, t, rig.actor, checkoutReply(t, cmd, 1, "checked_out", strp(coHead), strp(coMoved), nil))

			got := getTurn(ctx, t, f, mention.ID)
			if got.Status != sqlcgen.TurnStatusProcessing || got.EndReason != nil {
				t.Fatalf("turn: %s, end reason %v; want processing: a turn no lane asks for again runs on its head", got.Status, got.EndReason)
			}
			if got.CheckedOutSha == nil || *got.CheckedOutSha != coHead || got.ReviewHeadSha == nil || *got.ReviewHeadSha != coHead {
				t.Fatalf("checked out %v, review head %v; want both the recorded head %s, never the ref's tip", got.CheckedOutSha, got.ReviewHeadSha, coHead)
			}
			if n := len(sentPromptsOf(t, rig.commander)); n != 1 {
				t.Fatalf("%d prompts sent, want 1", n)
			}
		})
	}
}

// TestReviewCheckout_ARefThatLagsIsFetchedAgain: a reply saying the head is
// not in the ref yet is fetched again once ReviewCheckoutRefetchInterval has
// passed since the send, with a new command, and the turn runs once the
// ref holds the head -- a lag is never read as a move.
func TestReviewCheckout_ARefThatLagsIsFetchedAgain(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-lag", 740)
	attempt := seedReviewTurn(ctx, t, f, coHead, triggerOf(turn.RequestTriggerAuto), true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	first := lastCheckout(t, rig.commander, 1)
	deliver(ctx, t, rig.actor, checkoutReply(t, first, 1, "sha_absent", nil, strp(coMoved), nil))
	if got := getTurn(ctx, t, f, attempt.ID); got.Status != sqlcgen.TurnStatusPending || len(sentCheckouts(t, rig.commander)) != 1 {
		t.Fatalf("right after a lagging ref: %s with %d checkouts; want pending, nothing sent again before the interval", got.Status, len(sentCheckouts(t, rig.commander)))
	}

	backdateCheckout(ctx, t, pool, attempt.ID, 11*time.Second, platform.DefaultTimeouts().ReviewCheckoutRefetchInterval+time.Second)
	settle(ctx, t, rig.actor)
	second := lastCheckout(t, rig.commander, 2)
	if second.MessageId == first.MessageId || second.Repos[0].Sha != coHead {
		t.Fatalf("second checkout %+v, want a new command for the same head", second)
	}
	if got := getTurn(ctx, t, f, attempt.ID); i32(got.CheckoutSends) != 2 || i32(got.CheckoutFailures) != 0 {
		t.Fatalf("sends %d, failures %d; want 2 and 0: a lag is no failure", i32(got.CheckoutSends), i32(got.CheckoutFailures))
	}

	deliver(ctx, t, rig.actor, checkoutReply(t, second, 1, "checked_out", strp(coHead), strp(coHead), nil))
	if got := getTurn(ctx, t, f, attempt.ID); got.Status != sqlcgen.TurnStatusProcessing || got.CheckedOutSha == nil || *got.CheckedOutSha != coHead {
		t.Fatalf("turn once the ref holds the head: %s, checked out %v; want processing at %s", got.Status, got.CheckedOutSha, coHead)
	}
}

// TestReviewCheckout_AReconnectSendsItAgain: a command lost with its socket
// -- the sandbox reconnects, and no reply is stored -- is sent again with a
// new messageId on the reconnect's own evaluation, inside the bound.
func TestReviewCheckout_AReconnectSendsItAgain(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-reconnect", 750)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	first := lastCheckout(t, rig.commander, 1)
	// Heartbeats are no reconnect: nothing is sent again.
	settle(ctx, t, rig.actor)
	lastCheckout(t, rig.commander, 1)

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	second := lastCheckout(t, rig.commander, 2)
	if second.MessageId == first.MessageId {
		t.Fatal("the checkout sent again after a reconnect reuses the lost command's messageId")
	}
	got := getTurn(ctx, t, f, reviewTurn.ID)
	if got.Status != sqlcgen.TurnStatusPending || got.CheckoutMessageID == nil || *got.CheckoutMessageID != second.MessageId || i32(got.CheckoutSends) != 2 {
		t.Fatalf("turn: %s, message %v, sends %d; want pending, the second command's, 2", got.Status, got.CheckoutMessageID, i32(got.CheckoutSends))
	}

	deliver(ctx, t, rig.actor, checkoutReply(t, second, 1, "checked_out", strp(coHead), strp(coHead), nil))
	if n := len(sentPromptsOf(t, rig.commander)); n != 1 {
		t.Fatalf("%d prompts sent, want 1", n)
	}
}

// TestReviewCheckout_AGenChangeAsksTheNewGen: a new gen holds a new tree,
// so a reply from the old one -- stored before the gen changed -- confirms
// nothing: the new gen is asked, with a bound of its own, and a late reply
// of the old gen is dropped by the gen fence, never stored.
func TestReviewCheckout_AGenChangeAsksTheNewGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-gen", 760)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	old := lastCheckout(t, rig.commander, 1)
	// Gen 1's reply, stored as the old gen sent it, before the gen moved on.
	if _, err := narvipg.NewEventStore(pool).Create(ctx, sqlcgen.CreateEventParams{
		SessionID: f.sessionID, Type: "checkout_result", MessageID: "checkout_result:" + old.MessageId,
		Payload: checkoutResultRaw(t, old, 1, "checked_out", strp(coHead), strp(coHead), nil),
	}); err != nil {
		t.Fatalf("store the old gen's reply: %v", err)
	}
	backdateCheckout(ctx, t, pool, reviewTurn.ID, 5*time.Minute, 5*time.Minute)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET gen = 2, review_checkout_gen = 2 WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	settle(ctx, t, rig.actor)

	if n := len(sentPromptsOf(t, rig.commander)); n != 0 {
		t.Fatalf("%d prompts sent to the new gen on the old gen's reply, want none", n)
	}
	fresh := lastCheckout(t, rig.commander, 2)
	if fresh.Gen != 2 || fresh.MessageId == old.MessageId {
		t.Fatalf("checkout after the gen change = %+v, want a new command to gen 2", fresh)
	}
	got := getTurn(ctx, t, f, reviewTurn.ID)
	if got.CheckoutGen == nil || *got.CheckoutGen != 2 || i32(got.CheckoutSends) != 1 {
		t.Fatalf("checkout gen %v, sends %d; want gen 2, its first", got.CheckoutGen, i32(got.CheckoutSends))
	}
	if lead, ok := dispatchTimerLead(ctx, t, pool, f.sessionID); !ok || lead < platform.DefaultTimeouts().ReviewCheckoutTimeout-time.Minute {
		t.Fatalf("dispatch timer in %s (%v): the new gen's bound starts again", lead, ok)
	}

	// The old gen answers its command again, late: fenced off, not stored.
	sendSandboxEvent(ctx, t, rig.actor, SandboxEvent{Type: "checkout_result", Gen: 1, MessageID: "checkout_result:late-" + old.MessageId,
		Raw: checkoutResultRaw(t, old, 1, "checked_out", strp(coHead), strp(coHead), nil)})
	barrier(ctx, t, rig.actor)
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND message_id = $2`, f.sessionID, "checkout_result:late-"+old.MessageId); n != 0 {
		t.Fatalf("a stale gen's reply was stored (%d rows)", n)
	}

	deliver(ctx, t, rig.actor, checkoutReply(t, fresh, 2, "checked_out", strp(coHead), strp(coHead), nil))
	if prompts := sentPromptsOf(t, rig.commander); len(prompts) != 1 || prompts[0].Gen != 2 {
		t.Fatalf("prompts %+v, want one, to gen 2", prompts)
	}
}

// TestReviewCheckout_AReplyStoredByAReplicaThatDoesNotKnowTheTypeIsFound:
// the reply is the stored row under its deterministic key, so one a
// binary that does not know the type stored -- through the generic event
// insert -- is the reply the next evaluation reads.
func TestReviewCheckout_AReplyStoredByAReplicaThatDoesNotKnowTheTypeIsFound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-generic", 770)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	cmd := lastCheckout(t, rig.commander, 1)
	if _, err := narvipg.NewEventStore(pool).Create(ctx, sqlcgen.CreateEventParams{
		SessionID: f.sessionID, Type: "checkout_result", MessageID: "checkout_result:" + cmd.MessageId,
		Payload: checkoutResultRaw(t, cmd, 1, "checked_out", strp(coHead), strp(coHead), nil),
	}); err != nil {
		t.Fatalf("store the reply as another binary would: %v", err)
	}
	settle(ctx, t, rig.actor)

	if got := getTurn(ctx, t, f, reviewTurn.ID); got.Status != sqlcgen.TurnStatusProcessing || got.CheckedOutSha == nil || *got.CheckedOutSha != coHead {
		t.Fatalf("turn: %s, checked out %v; want processing at %s", got.Status, got.CheckedOutSha, coHead)
	}
}

// TestReviewCheckout_AnIncapableFreshGenIsRefusedWithItsRemedy: a gen whose
// ready did not advertise reviewCheckout, with no snapshot to clear, is
// never sent the command: the turn is refused at once, naming the agent
// and the remedy -- rebuild the image -- its check closed not assessed, and
// the next turn of the queue is evaluated at once.
func TestReviewCheckout_AnIncapableFreshGenIsRefusedWithItsRemedy(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-old-agent", 780)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	backdate(ctx, t, f, reviewTurn.ID, 10)
	next := createPendingTurn(ctx, t, f.turns, f.sessionID, "a question with no head")
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)
	before := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeUnsupported)

	deliver(ctx, t, rig.actor, checkoutReady(1, false))

	if n := len(sentCheckouts(t, rig.commander)); n != 0 {
		t.Fatalf("%d checkouts sent to a gen that cannot check out, want none", n)
	}
	got := getTurn(ctx, t, f, reviewTurn.ID)
	if got.Status != sqlcgen.TurnStatusFailed || got.DispatchedAt.Valid || got.EndReason != nil {
		t.Fatalf("turn: %s, dispatched %v, end reason %v; want failed before it started", got.Status, got.DispatchedAt.Valid, got.EndReason)
	}
	var reason, delivered string
	if err := pool.QueryRow(ctx, `SELECT payload->>'reason', COALESCE(payload->>'delivered', '') FROM events WHERE session_id = $1 AND type = 'execution_complete' AND payload->>'turn_id' = $2`,
		f.sessionID, reviewTurn.ID.String()).Scan(&reason, &delivered); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "agent cannot check out") || delivered != "false" {
		t.Fatalf("synthetic end reason %q delivered %q; want the agent named and marked undelivered", reason, delivered)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%Rebuild the sandbox image%'`, f.sessionID); n != 1 {
		t.Fatalf("%d session warnings naming the remedy, want 1", n)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND payload::text LIKE '%review_checkout_unsupported%'`, f.sessionID); n != 1 {
		t.Fatalf("%d not-assessed checks naming the reason, want 1", n)
	}
	if prompts := sentPromptsOf(t, rig.commander); len(prompts) != 1 || prompts[0].Text != "a question with no head" {
		t.Fatalf("prompts %+v, want the next turn's, dispatched in the same round", prompts)
	}
	if got := getTurn(ctx, t, f, next.ID); got.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("next turn: %s, want processing", got.Status)
	}
	if got := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeUnsupported) - before; got != 1 {
		t.Fatalf("review_checkout_total{unsupported} moved by %d, want 1", got)
	}
}

// TestReviewCheckout_ASnapshotRestoredOldAgentIsRespawnedFresh: a gen that
// cannot check out, with a snapshot -- whose restore may have brought that
// agent back -- is retired and the snapshot cleared in one transaction;
// the next gen is spawned fresh, never restored, and the turn waits for it.
func TestReviewCheckout_ASnapshotRestoredOldAgentIsRespawnedFresh(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-snapshot", 790)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_id = 'snap-old-agent', snapshot_suppressed_in_shadow = true WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)
	before := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeRetiredOldAgent)

	deliver(ctx, t, rig.actor, checkoutReady(1, false))

	if n := len(sentCheckouts(t, rig.commander)); n != 0 {
		t.Fatalf("%d checkouts sent to a gen that cannot check out, want none", n)
	}
	if rig.provider.restoreCallCount() != 0 || rig.provider.callCount() != 1 {
		t.Fatalf("restores %d, fresh spawns %d; want the next gen spawned fresh, never restored", rig.provider.restoreCallCount(), rig.provider.callCount())
	}
	sb, err := narvipg.NewSandboxStore(pool).Get(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.SnapshotID != nil || sb.SnapshotSuppressedInShadow || sb.Gen != 2 {
		t.Fatalf("sandbox: snapshot %v (shadow bit %v), gen %d; want the snapshot cleared and gen 2", sb.SnapshotID, sb.SnapshotSuppressedInShadow, sb.Gen)
	}
	if got := getTurn(ctx, t, f, reviewTurn.ID); got.Status != sqlcgen.TurnStatusPending {
		t.Fatalf("turn: %s, want pending for the fresh gen", got.Status)
	}
	if got := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeRetiredOldAgent) - before; got != 1 {
		t.Fatalf("review_checkout_total{retired_old_agent} moved by %d, want 1", got)
	}

	// The fresh gen is old too: it has no snapshot, so the turn is refused.
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready' WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	deliver(ctx, t, rig.actor, checkoutReady(2, false))
	if got := getTurn(ctx, t, f, reviewTurn.ID); got.Status != sqlcgen.TurnStatusFailed {
		t.Fatalf("turn on a fresh gen that cannot check out: %s, want refused", got.Status)
	}
	if rig.provider.callCount() != 1 {
		t.Fatalf("fresh spawns %d, want 1: an old agent costs at most one respawn", rig.provider.callCount())
	}
}

// TestReviewCheckout_ASnapshotRestoredOldAgentIsRespawnedFresh_Visibly:
// the retirement that clears a snapshot whose agent cannot check out a
// commit makes the loss visible as a refused restore does (technical plan
// §35.5b): one persisted warning at the retired gen, through the same
// helper and key, naming the snapshot and what it records of its agent;
// the snapshot's provenance goes with it.
func TestReviewCheckout_ASnapshotRestoredOldAgentIsRespawnedFresh_Visibly(t *testing.T) {
	tests := []struct {
		name     string
		protocol *string
		names    string
	}{
		{"nothing recorded of the snapshot's agent", nil, "nothing was recorded of the agent the snapshot holds"},
		{"the snapshot's agent recorded", strPtr("1.25.0"), "an agent of protocol 1.25.0"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			f := checkoutFixture(ctx, t, pool, "acme/co-snapshot-visible", int32(791+i))
			seedReviewTurn(ctx, t, f, coHead, nil, true)
			const snapshotID = "snap-old-agent-visible"
			if _, err := narvipg.NewSandboxStore(pool).UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{
				SessionID: f.sessionID, SnapshotID: strPtr(snapshotID), SnapshotSuppressedInShadow: true, AgentProtocol: tc.protocol,
			}); err != nil {
				t.Fatal(err)
			}
			if tc.protocol == nil {
				// Recorded by a control plane that predates the record.
				if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_provenance_id = NULL WHERE session_id = $1`, f.sessionID); err != nil {
					t.Fatal(err)
				}
			}
			rig := newContextRig(ctx, t, pool, f.sessionID, nil)

			deliver(ctx, t, rig.actor, checkoutReady(1, false))

			if rig.provider.restoreCallCount() != 0 || rig.provider.callCount() != 1 {
				t.Fatalf("restores %d, fresh spawns %d; want the next gen spawned fresh", rig.provider.restoreCallCount(), rig.provider.callCount())
			}
			warnings := storedWarnings(ctx, t, pool, f.sessionID)
			if len(warnings) != 1 || warnings[0].messageID != snapshotRefusalMessageIDPrefix+snapshotID || warnings[0].gen != 1 {
				t.Fatalf("warnings %+v, want exactly one, %s%s, at the retired gen 1", warnings, snapshotRefusalMessageIDPrefix, snapshotID)
			}
			for _, want := range []string{snapshotID, "cannot check out the commit", tc.names, "starting from the repository"} {
				if !strings.Contains(warnings[0].message, want) {
					t.Errorf("warning %q does not name %q", warnings[0].message, want)
				}
			}
			row := readProvenanceRow(ctx, t, pool, f.sessionID)
			if row.snapshotID != nil || row.provenanceID != nil || row.agentProtocol != nil || row.mintedAt.Valid || row.suppressedInShadow {
				t.Fatalf("sandbox after the retirement: %v; want the snapshot cleared with its provenance and shadow bit", row)
			}
		})
	}
}

// TestReviewCheckout_ASilentSandboxIsRefusedAtTheBound: a capable sandbox
// that never answers keeps the turn waiting -- nothing sent again without
// a reconnect -- until ReviewCheckoutTimeout, when the turn is refused,
// naming the bound.
func TestReviewCheckout_ASilentSandboxIsRefusedAtTheBound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-silent", 800)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)
	before := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeNoReport)

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	backdateCheckout(ctx, t, pool, reviewTurn.ID, 14*time.Minute, 14*time.Minute)
	settle(ctx, t, rig.actor)
	if got := getTurn(ctx, t, f, reviewTurn.ID); got.Status != sqlcgen.TurnStatusPending || len(sentCheckouts(t, rig.commander)) != 1 {
		t.Fatalf("inside the bound: %s with %d checkouts; want pending, one", got.Status, len(sentCheckouts(t, rig.commander)))
	}

	backdateCheckout(ctx, t, pool, reviewTurn.ID, platform.DefaultTimeouts().ReviewCheckoutTimeout+time.Second, 15*time.Minute)
	settle(ctx, t, rig.actor)
	got := getTurn(ctx, t, f, reviewTurn.ID)
	if got.Status != sqlcgen.TurnStatusFailed {
		t.Fatalf("at the bound: %s, want refused", got.Status)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'execution_complete' AND payload->>'reason' LIKE '%did not report its checkout%within 15m0s%'`, f.sessionID); n != 1 {
		t.Fatalf("%d ends naming the silence and the bound, want 1", n)
	}
	if n := len(sentPromptsOf(t, rig.commander)); n != 0 {
		t.Fatalf("%d prompts sent, want none", n)
	}
	if got := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeNoReport) - before; got != 1 {
		t.Fatalf("review_checkout_total{no_report} moved by %d, want 1", got)
	}
}

// TestReviewCheckout_AFetchFailureIsRetriedThenRefused: a ref that cannot
// be fetched is tried again, the spacing doubling with each send, and at
// the bound the turn is refused naming the agent's error and, since the
// error says the code host refused the read, the remedy: the App, with
// read access, on the base repository, named. A fetch failure never
// retires the gen.
func TestReviewCheckout_AFetchFailureIsRetriedThenRefused(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-fetch", 810)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)
	before := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeError)
	authErr := "fatal: Authentication failed for 'https://github.com/acme/co-fetch.git/'"

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	for i := 1; i <= 4; i++ {
		cmd := lastCheckout(t, rig.commander, i)
		deliver(ctx, t, rig.actor, checkoutReply(t, cmd, 1, "fetch_failed", nil, nil, strp(authErr)))
		if n := len(sentCheckouts(t, rig.commander)); n != i {
			t.Fatalf("send %d: %d checkouts right after its failure, want %d: the next waits its interval", i, n, i)
		}
		// The next send is due RefetchInterval doubled once per send made.
		spacing := platform.DefaultTimeouts().ReviewCheckoutRefetchInterval << (i - 1)
		backdateCheckout(ctx, t, pool, reviewTurn.ID, time.Minute, spacing-time.Second)
		settle(ctx, t, rig.actor)
		if n := len(sentCheckouts(t, rig.commander)); n != i {
			t.Fatalf("send %d: sent again %s after it, before its interval %s", i, spacing-time.Second, spacing)
		}
		backdateCheckout(ctx, t, pool, reviewTurn.ID, time.Minute, spacing)
		settle(ctx, t, rig.actor)
	}
	got := getTurn(ctx, t, f, reviewTurn.ID)
	if got.Status != sqlcgen.TurnStatusPending || i32(got.CheckoutFailures) != 0 || got.CheckoutRetiredGen != nil {
		t.Fatalf("after four fetch failures: %s, failures %d, retired %v; want pending, none counted, none retired", got.Status, i32(got.CheckoutFailures), got.CheckoutRetiredGen)
	}

	cmd := lastCheckout(t, rig.commander, 5)
	deliver(ctx, t, rig.actor, checkoutReply(t, cmd, 1, "fetch_failed", nil, nil, strp(authErr)))
	backdateCheckout(ctx, t, pool, reviewTurn.ID, platform.DefaultTimeouts().ReviewCheckoutTimeout, time.Second)
	settle(ctx, t, rig.actor)
	if got := getTurn(ctx, t, f, reviewTurn.ID); got.Status != sqlcgen.TurnStatusFailed {
		t.Fatalf("at the bound: %s, want refused", got.Status)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%Authentication failed%' AND payload::text LIKE '%install the App on acme/co-fetch, with read access%'`, f.sessionID); n != 1 {
		t.Fatalf("%d warnings naming the error and the remedy, want 1", n)
	}
	if rig.provider.callCount() != 0 {
		t.Fatalf("%d spawns: a fetch failure never retires the gen", rig.provider.callCount())
	}
	if got := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeError) - before; got != 1 {
		t.Fatalf("review_checkout_total{error} moved by %d, want 1", got)
	}
}

// TestReviewCheckout_AStaleIndexLockRetiresTheGenOnce is how the gate
// recovers from a worktree a killed git left broken -- a stale
// .git/index.lock or a corrupted index fails every checkout on that gen,
// and the snapshot taken after the turn holds it too: the
// ReviewCheckoutFailuresBeforeRetire'th failed reply retires the gen and
// clears the snapshot, so the next boots fresh, and the turn waits for it.
// A turn retires at most one gen: on the fresh gen its failures are only
// retried, and refused at the bound.
func TestReviewCheckout_AStaleIndexLockRetiresTheGenOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-index-lock", 820)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_id = 'snap-with-the-lock' WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)
	before := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeRetiredFailing)
	lockErr := "fatal: Unable to create '/workspace/widgets/.git/index.lock': File exists."
	timeouts := platform.DefaultTimeouts()

	failAll := func(gen, from, to int) {
		t.Helper()
		for i := from; i <= to; i++ {
			cmd := lastCheckout(t, rig.commander, i)
			if cmd.Gen != gen {
				t.Fatalf("checkout %d to gen %d, want gen %d", i, cmd.Gen, gen)
			}
			deliver(ctx, t, rig.actor, checkoutReply(t, cmd, gen, "failed", nil, strp(coHead), strp(lockErr)))
			if i < to {
				backdateCheckout(ctx, t, pool, reviewTurn.ID, time.Minute, timeouts.ReviewCheckoutRefetchInterval<<(i-from))
				settle(ctx, t, rig.actor)
			}
		}
	}

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	failAll(1, 1, timeouts.ReviewCheckoutFailuresBeforeRetire)

	got := getTurn(ctx, t, f, reviewTurn.ID)
	if got.Status != sqlcgen.TurnStatusPending || got.CheckoutRetiredGen == nil || *got.CheckoutRetiredGen != 1 {
		t.Fatalf("after %d failed checkouts: %s, retired gen %v; want pending, gen 1 retired", timeouts.ReviewCheckoutFailuresBeforeRetire, got.Status, got.CheckoutRetiredGen)
	}
	if rig.provider.restoreCallCount() != 0 || rig.provider.callCount() != 1 {
		t.Fatalf("restores %d, fresh spawns %d; want one fresh spawn", rig.provider.restoreCallCount(), rig.provider.callCount())
	}
	sb, err := narvipg.NewSandboxStore(pool).Get(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.SnapshotID != nil || sb.Gen != 2 {
		t.Fatalf("sandbox: snapshot %v, gen %d; want the snapshot cleared and gen 2", sb.SnapshotID, sb.Gen)
	}
	// The cleared snapshot is named in one persisted warning (technical
	// plan §35.5b), at the retired gen.
	if w := storedWarnings(ctx, t, pool, f.sessionID); len(w) != 1 || w[0].messageID != snapshotRefusalMessageIDPrefix+"snap-with-the-lock" ||
		w[0].gen != 1 || !strings.Contains(w[0].message, "snap-with-the-lock") || !strings.Contains(w[0].message, "kept failing") {
		t.Fatalf("warnings %+v, want one at gen 1 naming snap-with-the-lock and the failing checkouts", w)
	}
	if got := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeRetiredFailing) - before; got != 1 {
		t.Fatalf("review_checkout_total{retired_failing} moved by %d, want 1", got)
	}

	// The fresh gen boots and is asked anew, its own bound; it fails as
	// well, and is not retired a second time.
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready' WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	n := timeouts.ReviewCheckoutFailuresBeforeRetire
	deliver(ctx, t, rig.actor, checkoutReady(2, true))
	if got := getTurn(ctx, t, f, reviewTurn.ID); i32(got.CheckoutSends) != 1 || i32(got.CheckoutFailures) != 0 {
		t.Fatalf("on the fresh gen: sends %d, failures %d; want its first send and no failure", i32(got.CheckoutSends), i32(got.CheckoutFailures))
	}
	failAll(2, n+1, 2*n)
	if got := getTurn(ctx, t, f, reviewTurn.ID); got.Status != sqlcgen.TurnStatusPending || *got.CheckoutRetiredGen != 1 || rig.provider.callCount() != 1 {
		t.Fatalf("after the fresh gen's failures: %s, retired gen %v, spawns %d; want pending, still gen 1, one spawn", got.Status, *got.CheckoutRetiredGen, rig.provider.callCount())
	}
	backdateCheckout(ctx, t, pool, reviewTurn.ID, timeouts.ReviewCheckoutTimeout, time.Second)
	settle(ctx, t, rig.actor)
	if got := getTurn(ctx, t, f, reviewTurn.ID); got.Status != sqlcgen.TurnStatusFailed {
		t.Fatalf("at the fresh gen's bound: %s, want refused", got.Status)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'execution_complete' AND payload->>'reason' LIKE '%index.lock%'`, f.sessionID); n != 1 {
		t.Fatalf("%d ends naming the agent's error, want 1", n)
	}
}

// TestReviewCheckout_ASlowFailureIsRetiredAtTheBound: a worktree whose
// checkouts fail only after minutes of work -- the first reply, failed, 9
// minutes into the bound -- leaves its re-send still running when the
// bound comes. A gen that answered failed is retired then, its snapshot
// cleared so the next boots fresh, as it is when the latest reply is in:
// never kept in place while its turn is refused for want of a report.
func TestReviewCheckout_ASlowFailureIsRetiredAtTheBound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-slow-failure", 830)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_id = 'snap-slow-failure' WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)
	retiredBefore, noReportBefore := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeRetiredFailing), reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeNoReport)
	timeouts := platform.DefaultTimeouts()

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	first := lastCheckout(t, rig.commander, 1)
	// The first checkout fails after 5 minutes of work, 9 minutes into the
	// bound; the re-send goes at once.
	backdateCheckout(ctx, t, pool, reviewTurn.ID, 9*time.Minute, 5*time.Minute)
	deliver(ctx, t, rig.actor, checkoutReply(t, first, 1, "failed", nil, strp(coHead), strp("fatal: index file corrupt")))
	resend := lastCheckout(t, rig.commander, 2)
	if got := getTurn(ctx, t, f, reviewTurn.ID); resend.Gen != 1 || got.Status != sqlcgen.TurnStatusPending || i32(got.CheckoutFailures) != 1 || i32(got.CheckoutSends) != 2 {
		t.Fatalf("after the first failure: re-sent to gen %d, %s, failures %d, sends %d; want gen 1, pending, 1, 2", resend.Gen, got.Status, i32(got.CheckoutFailures), i32(got.CheckoutSends))
	}

	// The re-send works on past the bound, unanswered.
	backdateCheckout(ctx, t, pool, reviewTurn.ID, timeouts.ReviewCheckoutTimeout+time.Second, 6*time.Minute+time.Second)
	settle(ctx, t, rig.actor)

	got := getTurn(ctx, t, f, reviewTurn.ID)
	if got.Status != sqlcgen.TurnStatusPending || got.CheckoutRetiredGen == nil || *got.CheckoutRetiredGen != 1 {
		t.Fatalf("at the bound with the re-send unanswered: %s, retired gen %v; want pending, gen 1 retired", got.Status, got.CheckoutRetiredGen)
	}
	sb, err := narvipg.NewSandboxStore(pool).Get(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.SnapshotID != nil || sb.Gen != 2 || rig.provider.callCount() != 1 || rig.provider.restoreCallCount() != 0 {
		t.Fatalf("sandbox: snapshot %v, gen %d, spawns %d, restores %d; want the snapshot cleared and one fresh spawn of gen 2",
			sb.SnapshotID, sb.Gen, rig.provider.callCount(), rig.provider.restoreCallCount())
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%did not report its checkout%'`, f.sessionID); n != 0 {
		t.Fatalf("%d warnings that the sandbox did not report, want none", n)
	}
	if got := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeRetiredFailing) - retiredBefore; got != 1 {
		t.Fatalf("review_checkout_total{retired_failing} moved by %d, want 1", got)
	}
	if got := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeNoReport) - noReportBefore; got != 0 {
		t.Fatalf("review_checkout_total{no_report} moved by %d, want 0", got)
	}
}

// TestReviewCheckout_AReEnqueuedTurnChecksOutOnTheNewGen is exit 3 for a
// re-enqueue: a turn processing on a gen that is gone is re-sent to the
// new one only once the new gen holds the turn's head -- the new tree is a
// fresh boot's -- and a moved head does not end it, since its run had
// started on that head.
func TestReviewCheckout_AReEnqueuedTurnChecksOutOnTheNewGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-reenqueue", 830)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, triggerOf(turn.RequestTriggerAuto), true)
	gen1, firstMessage := int32(1), "msg-gen-1"
	if _, err := f.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: reviewTurn.ID, Status: sqlcgen.TurnStatusProcessing,
		DispatchedAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
		DispatchedSandboxGen: &gen1, DispatchedMessageID: &firstMessage,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET gen = 2 WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)

	deliver(ctx, t, rig.actor, checkoutReady(2, true))
	if got := sentCommandTypes(t, rig.commander); len(got) != 1 || got[0] != "checkout" {
		t.Fatalf("commands to the new gen = %v, want the checkout before any prompt", got)
	}
	cmd := lastCheckout(t, rig.commander, 1)
	if cmd.Gen != 2 || cmd.Repos[0].Sha != coHead {
		t.Fatalf("checkout = %+v, want gen 2 at %s", cmd, coHead)
	}
	if got := getTurn(ctx, t, f, reviewTurn.ID); got.Status != sqlcgen.TurnStatusProcessing || *got.DispatchedSandboxGen != 1 {
		t.Fatalf("turn while the new gen checks out: %s on gen %d; want processing, still gen 1's", got.Status, *got.DispatchedSandboxGen)
	}

	deliver(ctx, t, rig.actor, checkoutReply(t, cmd, 2, "checked_out", strp(coHead), strp(coMoved), nil))
	prompts := sentPromptsOf(t, rig.commander)
	if len(prompts) != 1 || prompts[0].Gen != 2 || prompts[0].MessageId == firstMessage {
		t.Fatalf("prompts %+v, want the turn re-sent once to gen 2 under a new messageId", prompts)
	}
	got := getTurn(ctx, t, f, reviewTurn.ID)
	if got.Status != sqlcgen.TurnStatusProcessing || got.EndReason != nil || *got.DispatchedSandboxGen != 2 ||
		got.CheckedOutSha == nil || *got.CheckedOutSha != coHead {
		t.Fatalf("turn: %s, end reason %v, gen %d, checked out %v; want processing on gen 2 at %s, never ended moved",
			got.Status, got.EndReason, *got.DispatchedSandboxGen, got.CheckedOutSha, coHead)
	}
}

// TestReviewCheckout_ARefusedReEnqueueIsFailedForward: a turn re-sent to a
// new gen whose agent cannot check out is failed forward, refused -- not
// marked undelivered, since an earlier gen did receive its prompt.
func TestReviewCheckout_ARefusedReEnqueueIsFailedForward(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-reenqueue-refused", 835)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	gen1, firstMessage := int32(1), "msg-gen-1"
	if _, err := f.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: reviewTurn.ID, Status: sqlcgen.TurnStatusProcessing,
		DispatchedAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
		DispatchedSandboxGen: &gen1, DispatchedMessageID: &firstMessage,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET gen = 2 WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)

	deliver(ctx, t, rig.actor, checkoutReady(2, false))
	waitForTurnStatus(ctx, t, f.turns, reviewTurn.ID, sqlcgen.TurnStatusFailed)
	if n := len(sentPayloads(rig.commander)); n != 0 {
		t.Fatalf("%d commands sent, want none", n)
	}
	var delivered string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(payload->>'delivered', '') FROM events WHERE session_id = $1 AND type = 'execution_complete' AND payload->>'turn_id' = $2`,
		f.sessionID, reviewTurn.ID.String()).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != "" {
		t.Fatalf("synthetic end marked delivered %q, want no mark: gen 1 received the prompt", delivered)
	}
}

// TestReviewCheckout_ATurnWithNoRecordedHeadIsNotCheckedOut: a review
// session's turn that records no head -- a web prompt, a workflow step --
// or one whose recorded head is no commit id no code host returns,
// dispatches as before, with no checkout.
func TestReviewCheckout_ATurnWithNoRecordedHeadIsNotCheckedOut(t *testing.T) {
	for i, head := range []string{"", "sha-that-is-no-commit-id"} {
		t.Run(fmt.Sprintf("head %q", head), func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			f := checkoutFixture(ctx, t, pool, fmt.Sprintf("acme/co-nohead-%d", i), int32(840+i))
			var created sqlcgen.Turn
			if head == "" {
				created = createPendingTurn(ctx, t, f.turns, f.sessionID, "a question")
			} else {
				created = seedReviewTurn(ctx, t, f, head, nil, true)
			}
			rig := newContextRig(ctx, t, pool, f.sessionID, nil)

			deliver(ctx, t, rig.actor, checkoutReady(1, true))
			if got := sentCommandTypes(t, rig.commander); len(got) != 1 || got[0] != "prompt" {
				t.Fatalf("commands %v, want the prompt alone", got)
			}
			if got := getTurn(ctx, t, f, created.ID); got.CheckoutMessageID != nil || got.CheckedOutSha != nil {
				t.Fatalf("turn records a checkout (%v, %v), want none", got.CheckoutMessageID, got.CheckedOutSha)
			}
		})
	}
}

// TestReviewCheckout_NonReviewSessionsDispatchAsBefore: only a session
// that claims a pull request, whose primary repo is the claim's
// repository, checks a turn out. A session with no claim -- a web
// session, a sentinel-fix child -- and a review session whose spec still
// names the fork it was opened on dispatch as before, whatever the turn
// recorded.
func TestReviewCheckout_NonReviewSessionsDispatchAsBefore(t *testing.T) {
	for i, tc := range []struct {
		name  string
		setup func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID
	}{
		{name: "a web session", setup: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
			return createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", "https://github.com/acme/web.git", "main")
		}},
		{name: "a github session that claims no pull request", setup: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
			session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
				SpawnSource: sqlcgen.SessionSpawnSourceGithub, Repos: reposJSONForTest(t, "widgets", "https://github.com/acme/fix.git", "narvi/fix"),
			})
			if err != nil {
				t.Fatal(err)
			}
			return session.ID
		}},
		{name: "a review session whose spec names the fork", setup: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
			session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
				SpawnSource: sqlcgen.SessionSpawnSourceGithub, Repos: reposJSONForTest(t, "widgets", "https://github.com/contributor/widgets.git", "main"),
			})
			if err != nil {
				t.Fatal(err)
			}
			claimPullRequest(ctx, t, pool, "acme/widgets-fork-legacy", 850, session.ID)
			return session.ID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			sessionID := tc.setup(ctx, t, pool)
			seedReadySandbox(ctx, t, pool, sessionID)
			turns := narvipg.NewTurnStore(pool)
			head := coHead
			prompt := fmt.Sprintf("review %d", i)
			created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt, ReviewHeadSha: &head, IsReviewAttempt: true})
			if err != nil {
				t.Fatal(err)
			}
			rig := newContextRig(ctx, t, pool, sessionID, nil)

			deliver(ctx, t, rig.actor, checkoutReady(1, true))
			if got := sentCommandTypes(t, rig.commander); len(got) != 1 || got[0] != "prompt" {
				t.Fatalf("commands %v, want the prompt alone", got)
			}
			if got, err := turns.Get(ctx, created.ID); err != nil || got.Status != sqlcgen.TurnStatusProcessing || got.CheckoutMessageID != nil {
				t.Fatalf("turn %+v (%v), want processing with no checkout", got.Status, err)
			}
		})
	}
}

// TestReviewCheckout_AStopCancelsATurnWaitingOnItsCheckout: a person's stop
// cancels a turn waiting on its checkout, and the reply that lands after it
// starts nothing.
func TestReviewCheckout_AStopCancelsATurnWaitingOnItsCheckout(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-stop", 860)
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	cmd := lastCheckout(t, rig.commander, 1)
	requestStop(ctx, t, pool, f.sessionID)
	settle(ctx, t, rig.actor)
	waitForTurnStatus(ctx, t, f.turns, reviewTurn.ID, sqlcgen.TurnStatusCancelled)

	deliver(ctx, t, rig.actor, checkoutReply(t, cmd, 1, "checked_out", strp(coHead), strp(coHead), nil))
	if n := len(sentPromptsOf(t, rig.commander)); n != 0 {
		t.Fatalf("%d prompts sent after the stop, want none", n)
	}
	if got := getTurn(ctx, t, f, reviewTurn.ID); got.Status != sqlcgen.TurnStatusCancelled || got.CheckedOutSha != nil {
		t.Fatalf("turn: %s, checked out %v; want cancelled, never checked out", got.Status, got.CheckedOutSha)
	}
}

// getOpenPRCounter counts GetOpenPR calls apart from the reads the
// freshness check makes after it.
type getOpenPRCounter struct {
	*fakeReviewLiveReader
	mu    sync.Mutex
	reads int
}

func (c *getOpenPRCounter) GetOpenPR(ctx context.Context, owner, repo string, number int, token string) (ports.OpenPR, bool, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.fakeReviewLiveReader.GetOpenPR(ctx, owner, repo, number, token)
}

func (c *getOpenPRCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// newCountingContextRig is newContextRig with reader, any ReviewLiveReader,
// as the context check's live reader, handed to the registry before the
// actor is hosted.
func newCountingContextRig(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, reader ReviewLiveReader) *holdRig {
	t.Helper()
	rig := &holdRig{
		commander: &fakeSendCommander{},
		provider:  &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-counting"}},
		fetcher:   &fakeReviewDiffFetcher{nextHeadSHA: coHead, nextBaseRef: "main", nextDiff: oneLineReadableDiff},
	}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, rig.commander, rig.provider, "http://localhost:8080", nil, nil, "", nil, false, RegistryOptions{
		ReviewDiffFetcher: rig.fetcher, GitHubBotHandle: "narvi-bot",
		GitHubOutbound:       platform.MustNewGitHubOutboundConfig("test-token"),
		ReviewSizeExclusions: domainreviewtriage.DefaultSizeExclusions(),
		ReviewLiveReader:     reader,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	rig.registry = r
	if rig.actor, err = r.GetOrSpawn(ctx, sessionID); err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	return rig
}

// TestReviewCheckout_OneCodeHostReadPerDispatch: a queued automatic attempt
// passes its checkout first, and technical plan §24.9's context check
// reads the code host once, in the evaluation that sends the prompt -- the
// pre-read skips it while the checkout is outstanding.
func TestReviewCheckout_OneCodeHostReadPerDispatch(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-one-read", 870)
	seedRunningTurn(ctx, t, f)
	attempt := seedAttemptOf(ctx, t, f, autoTrigger(), recordedContext(t, defaultRecordedContext()), coHead, 10)
	reader := unmovedLiveReader(f.repoFullName, f.prNumber)
	reader.pr.HeadSHA = coHead
	counter := &getOpenPRCounter{fakeReviewLiveReader: reader}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET review_checkout_gen = gen WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	rig := newCountingContextRig(ctx, t, pool, f.sessionID, counter)

	endRunningTurn(ctx, t, f, rig)
	barrier(ctx, t, rig.actor)
	cmd := lastCheckout(t, rig.commander, 1)
	if counter.count() != 0 {
		t.Fatalf("%d code-host reads before the checkout was confirmed, want none", counter.count())
	}
	deliver(ctx, t, rig.actor, checkoutReply(t, cmd, 1, "checked_out", strp(coHead), strp(coHead), nil))

	if got := getTurn(ctx, t, f, attempt.ID); got.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("attempt: %s, want processing", got.Status)
	}
	if counter.count() != 1 {
		t.Fatalf("GetOpenPR read %d times for one dispatch, want 1", counter.count())
	}
}

// TestReviewCheckout_TheBoundRunsFromTheFirstRequestOnTheGen: the lag window
// and the bound run from the turn's first checkout on the gen, whatever is
// sent after it. A head that never appears in the ref is fetched again
// every interval, each re-fetch a new command, and as time passes -- the
// bound and the latest send moved back together, so only the sends since
// the first can restart the bound -- the attempt ends once the window has
// run, rather than re-fetching for ever.
func TestReviewCheckout_TheBoundRunsFromTheFirstRequestOnTheGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-bound", 880)
	attempt := seedReviewTurn(ctx, t, f, coHead, triggerOf(turn.RequestTriggerAuto), true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)
	timeouts := platform.DefaultTimeouts()

	// Each step: the latest command's reply, then one interval and a
	// second pass, and the evaluation that follows.
	step := timeouts.ReviewCheckoutRefetchInterval + time.Second
	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	for sends := 1; ; sends++ {
		cmd := lastCheckout(t, rig.commander, sends)
		deliver(ctx, t, rig.actor, checkoutReply(t, cmd, 1, "sha_absent", nil, strp(coMoved), nil))
		if got := getTurn(ctx, t, f, attempt.ID); got.Status != sqlcgen.TurnStatusPending {
			t.Fatalf("attempt ended on the reply to send %d, %s after the first request, inside the lag window", sends, time.Duration(sends-1)*step)
		}
		shiftCheckout(ctx, t, pool, attempt.ID, step)
		settle(ctx, t, rig.actor)
		elapsed := time.Duration(sends) * step
		if got := getTurn(ctx, t, f, attempt.ID); got.Status != sqlcgen.TurnStatusPending {
			if elapsed < timeouts.ReviewCheckoutRefLagWindow {
				t.Fatalf("attempt ended %s after the first request, before the lag window ran", elapsed)
			}
			if sends < 2 {
				t.Fatalf("attempt ended after %d send, want re-fetches first", sends)
			}
			assertContextMoved(ctx, t, f, rig, attempt)
			return
		}
		if elapsed > 2*timeouts.ReviewCheckoutRefLagWindow {
			t.Fatalf("still pending after %d sends, %s after the first request: the bound restarted on a send", sends, elapsed)
		}
	}
}

// TestReviewCheckout_ARefLaggingBehindAHeadTheCloneHoldsIsFetchedAgain: a
// fresh boot clones every branch, so a same-repository pull request's new
// head is in the clone before the pull request's ref shows it, and the
// checkout answers checked_out at the head with the ref still at the old
// tip. Inside the lag window that is a lag, not a move: the attempt is
// fetched again, never ended context_moved nor its lane pointed at the
// older tip, and runs once the ref shows its head.
func TestReviewCheckout_ARefLaggingBehindAHeadTheCloneHoldsIsFetchedAgain(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-lag-behind", 890)
	attempt := seedReviewTurn(ctx, t, f, coHead, triggerOf(turn.RequestTriggerAuto), true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)
	const olderTip = "c0ffee0000000000000000000000000000000000"

	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	first := lastCheckout(t, rig.commander, 1)
	deliver(ctx, t, rig.actor, checkoutReply(t, first, 1, "checked_out", strp(coHead), strp(olderTip), nil))
	if got := getTurn(ctx, t, f, attempt.ID); got.Status != sqlcgen.TurnStatusPending || got.EndReason != nil {
		t.Fatalf("attempt after a ref behind its head: %s, end reason %v; want pending", got.Status, got.EndReason)
	}

	backdateCheckout(ctx, t, pool, attempt.ID, 11*time.Second, platform.DefaultTimeouts().ReviewCheckoutRefetchInterval)
	settle(ctx, t, rig.actor)
	second := lastCheckout(t, rig.commander, 2)
	deliver(ctx, t, rig.actor, checkoutReply(t, second, 1, "checked_out", strp(coHead), strp(coHead), nil))

	got := getTurn(ctx, t, f, attempt.ID)
	if got.Status != sqlcgen.TurnStatusProcessing || got.EndReason != nil || got.CheckedOutSha == nil || *got.CheckedOutSha != coHead {
		t.Fatalf("attempt once the ref shows its head: %s, end reason %v, checked out %v; want processing at %s", got.Status, got.EndReason, got.CheckedOutSha, coHead)
	}
	if row := f.prSession(ctx, t); row.PendingRetriggerHeadSha != nil || row.AutoRetriggerContextMoves != 0 {
		t.Fatalf("pending head %v, moves %d; want none: a lag is no move, and the lane is not pointed at the older tip", row.PendingRetriggerHeadSha, row.AutoRetriggerContextMoves)
	}
}

// TestReviewCheckout_AMovedHeadSpendsNoCodeHostRead: a queued attempt whose
// checkout says the ref is elsewhere is waited on, and then ended by the
// checkout, never by the context check: §24.9's pre-read, deciding with the
// checkout's own rules, reads nothing of the code host for it.
func TestReviewCheckout_AMovedHeadSpendsNoCodeHostRead(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/co-moved-no-read", 895)
	seedRunningTurn(ctx, t, f)
	attempt := seedAttemptOf(ctx, t, f, autoTrigger(), recordedContext(t, defaultRecordedContext()), coHead, 10)
	reader := unmovedLiveReader(f.repoFullName, f.prNumber)
	reader.pr.HeadSHA = coMoved
	counter := &getOpenPRCounter{fakeReviewLiveReader: reader}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET review_checkout_gen = gen WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	rig := newCountingContextRig(ctx, t, pool, f.sessionID, counter)

	endRunningTurn(ctx, t, f, rig)
	barrier(ctx, t, rig.actor)
	cmd := lastCheckout(t, rig.commander, 1)
	deliver(ctx, t, rig.actor, checkoutReply(t, cmd, 1, "checked_out", strp(coHead), strp(coMoved), nil))
	backdateCheckout(ctx, t, pool, attempt.ID, platform.DefaultTimeouts().ReviewCheckoutRefLagWindow+time.Second, time.Second)
	settle(ctx, t, rig.actor)

	assertContextMoved(ctx, t, f, rig, attempt)
	if n := counter.count(); n != 0 {
		t.Fatalf("GetOpenPR read %d times for an attempt the checkout ended, want none", n)
	}
}

// slowRestoreFixture is a review session whose gen 1 advertised the
// checkout, then stopped -- with a snapshot when snapshot is set -- and a
// review turn whose dispatch has started gen 2, a restore or a respawn,
// still connecting when its connecting deadline found it silent: suspect,
// pre_suspect connecting, its capability not known. processing makes the
// turn one in flight on gen 1, re-sent to the new gen.
func slowRestoreFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo string, pr int32, snapshot, processing bool) (*holdFixture, *holdRig, sqlcgen.Turn) {
	t.Helper()
	f := checkoutFixture(ctx, t, pool, repo, pr)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)
	deliver(ctx, t, rig.actor, checkoutReady(1, true))
	snapshotID := any(nil)
	if snapshot {
		snapshotID = "snap-good"
	}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped', snapshot_id = $2 WHERE session_id = $1`, f.sessionID, snapshotID); err != nil {
		t.Fatal(err)
	}
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	if processing {
		gen1, message := int32(1), "msg-gen-1"
		if _, err := f.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
			ID: reviewTurn.ID, Status: sqlcgen.TurnStatusProcessing,
			DispatchedAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
			DispatchedSandboxGen: &gen1, DispatchedMessageID: &message,
		}); err != nil {
			t.Fatal(err)
		}
	}
	settle(ctx, t, rig.actor)
	sb, err := narvipg.NewSandboxStore(pool).Get(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Gen != 2 || sb.Status != sqlcgen.SandboxStatusConnecting {
		t.Fatalf("after the dispatch: gen %d %s, want gen 2 connecting", sb.Gen, sb.Status)
	}
	// Its connecting deadline finds it silent: a respawn's claim stamps
	// last_seen_at, so the steady budget applies.
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET last_seen_at = now() - interval '241 seconds', created_at = now() - interval '2 hours' WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	if err := rig.actor.Send(ctx, TimerFired{Name: TimerConnectingDeadline}); err != nil {
		t.Fatal(err)
	}
	barrier(ctx, t, rig.actor)
	if sb, err = narvipg.NewSandboxStore(pool).Get(ctx, f.sessionID); err != nil {
		t.Fatal(err)
	}
	if sb.Status != sqlcgen.SandboxStatusSuspect || sb.PreSuspectStatus == nil || *sb.PreSuspectStatus != sqlcgen.SandboxStatusConnecting ||
		sb.ReviewCheckoutGen == nil || *sb.ReviewCheckoutGen != 1 {
		t.Fatalf("after the connecting deadline: %s pre %v, review_checkout_gen %v; want suspect while connecting, still gen 1's", sb.Status, sb.PreSuspectStatus, sb.ReviewCheckoutGen)
	}
	return f, rig, reviewTurn
}

// TestReviewCheckout_AGenThatHasNotConnectedIsWaitedFor: a gen that has
// sent no ready yet -- a restore or a respawn slow to connect, gone suspect
// while connecting -- is no old agent: a dispatch evaluation in its grace
// waits for it, never retiring it, clearing the session's snapshot or
// refusing the turn with a request to rebuild the image. Once it connects
// and advertises the checkout, its turn is checked out on it. Covered for a
// slow restore, a slow respawn with no snapshot, and a turn re-sent to a
// restoring gen.
func TestReviewCheckout_AGenThatHasNotConnectedIsWaitedFor(t *testing.T) {
	for i, tc := range []struct {
		name                 string
		snapshot, processing bool
	}{
		{name: "a slow restore, with a snapshot", snapshot: true},
		{name: "a slow respawn, no snapshot"},
		{name: "a turn re-sent to a restoring gen", snapshot: true, processing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			f, rig, reviewTurn := slowRestoreFixture(ctx, t, pool, fmt.Sprintf("acme/co-slow-%d", i), int32(900+i), tc.snapshot, tc.processing)
			spawnsBefore, restoresBefore := rig.provider.callCount(), rig.provider.restoreCallCount()
			retiredBefore, unsupportedBefore := reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeRetiredOldAgent), reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeUnsupported)
			// A person's follow-up, inserted in the grace, asks for an
			// evaluation.
			createTurnArmingDispatch(ctx, t, pool, f.sessionID, "a follow-up question")
			settle(ctx, t, rig.actor)

			sb, err := narvipg.NewSandboxStore(pool).Get(ctx, f.sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if sb.Gen != 2 || sb.Status != sqlcgen.SandboxStatusSuspect {
				t.Fatalf("sandbox: gen %d %s, want gen 2 still suspect: never retired", sb.Gen, sb.Status)
			}
			if tc.snapshot && (sb.SnapshotID == nil || *sb.SnapshotID != "snap-good") {
				t.Fatalf("snapshot %v, want snap-good kept", sb.SnapshotID)
			}
			if rig.provider.callCount() != spawnsBefore || rig.provider.restoreCallCount() != restoresBefore {
				t.Fatalf("spawns %d, restores %d after the evaluation; want no new one", rig.provider.callCount()-spawnsBefore, rig.provider.restoreCallCount()-restoresBefore)
			}
			got := getTurn(ctx, t, f, reviewTurn.ID)
			wantStatus := sqlcgen.TurnStatusPending
			if tc.processing {
				wantStatus = sqlcgen.TurnStatusProcessing
			}
			if got.Status != wantStatus {
				t.Fatalf("review turn: %s, want %s, waiting", got.Status, wantStatus)
			}
			if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%Rebuild the sandbox image%'`, f.sessionID); n != 0 {
				t.Fatalf("%d warnings asking to rebuild the image, want none", n)
			}
			if n := len(sentPayloads(rig.commander)); n != 0 {
				t.Fatalf("%d commands sent to a gen that has not connected, want none", n)
			}
			if reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeRetiredOldAgent) != retiredBefore || reviewCheckoutCount(ctx, t, reviewCheckoutOutcomeUnsupported) != unsupportedBefore {
				t.Fatal("review_checkout_total counted a retirement or a refusal for a gen that has not connected")
			}
			if lead, ok := dispatchTimerLead(ctx, t, pool, f.sessionID); !ok || lead <= 0 {
				t.Fatalf("dispatch timer in %s (%v), want it ahead: the wait looks again", lead, ok)
			}

			// It connects, advertising the checkout: its turn is checked out
			// on it.
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready', pre_suspect_status = NULL WHERE session_id = $1`, f.sessionID); err != nil {
				t.Fatal(err)
			}
			deliver(ctx, t, rig.actor, checkoutReady(2, true))
			if cmd := lastCheckout(t, rig.commander, 1); cmd.Gen != 2 || cmd.Repos[0].Sha != coHead {
				t.Fatalf("checkout = %+v, want gen 2 at %s", cmd, coHead)
			}
		})
	}
}
