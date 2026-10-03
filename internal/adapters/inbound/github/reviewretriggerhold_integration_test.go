//go:build integration

// Technical plan §24.9's first rule end to end, on real Postgres: pushes
// that land while a review of the pull request runs reach the real signed
// pull_request/synchronize delivery through NewHandler, the debounce's
// firings reach the real session actor through the real timer pump
// (Registry.PumpOnce), the running review ends with a real
// execution_complete sent to that actor, and the session's status is read
// through the real httpapi.GetSessionStatus route the whole time.
package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// holdHeadFetcher is a review diff fetcher whose live head the test moves:
// the head the pull request has when the held review finally runs.
type holdHeadFetcher struct {
	mu   sync.Mutex
	head string
}

func (f *holdHeadFetcher) setHead(head string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head = head
}

func (f *holdHeadFetcher) GetPullRequest(context.Context, string, string, int32, string) (githubapi.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return githubapi.PullRequest{HeadSHA: f.head, BaseRef: "main"}, nil
}

func (f *holdHeadFetcher) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	return "", spec.Branch, nil
}

func (f *holdHeadFetcher) GetCompareDiff(context.Context, string, string, string, string, string) (string, bool, error) {
	return "+ a line a push changed", false, nil
}

// holdStatusFixture is retriggerStatusFixture with a review running: the
// session's sandbox ready at gen 1 and a review attempt processing on it,
// whose end the test sends to the real actor.
type holdStatusFixture struct {
	retriggerStatusFixture
	fetcher  *holdHeadFetcher
	reviewID pgtype.UUID
}

func newHoldStatusFixture(ctx context.Context, t *testing.T) holdStatusFixture {
	t.Helper()
	fetcher := &holdHeadFetcher{head: "sha-under-review"}
	f := holdStatusFixture{retriggerStatusFixture: newRetriggerStatusFixtureWith(ctx, t, true, fetcher), fetcher: fetcher}

	sandboxes := narvipg.NewSandboxStore(f.rig.pool)
	if _, err := sandboxes.Create(ctx, f.sessionID); err != nil {
		t.Fatalf("create the sandbox: %v", err)
	}
	if _, err := f.rig.pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready', gen = 1 WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatalf("move the sandbox to ready: %v", err)
	}
	head := "sha-under-review"
	prompt := "review this pull request"
	review, err := f.rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: f.sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt, ReviewHeadSha: &head, IsReviewAttempt: true})
	if err != nil {
		t.Fatalf("create the running review: %v", err)
	}
	gen := int32(1)
	messageID := uuid.NewString()
	if _, err := f.rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: review.ID, Status: sqlcgen.TurnStatusProcessing,
		DispatchedAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
		DispatchedSandboxGen: &gen, DispatchedMessageID: &messageID,
	}); err != nil {
		t.Fatalf("start the running review: %v", err)
	}
	f.reviewID = review.ID
	return f
}

// pumpUntilHeld runs real timer-pump ticks until the debounce's firing has
// held: the row re-armed more than half ReviewRetriggerHoldBackstop out.
func (f holdStatusFixture) pumpUntilHeld(ctx context.Context, t *testing.T) {
	t.Helper()
	half := platform.DefaultTimeouts().ReviewRetriggerHoldBackstop.Seconds() / 2
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := f.registry.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
		var held bool
		if err := f.rig.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM session_timers WHERE session_id = $1 AND name = $2 AND fires_at > now() + make_interval(secs => $3))`,
			f.sessionID, sessionactor.TimerReviewRetriggerDebounce, half).Scan(&held); err != nil {
			t.Fatalf("read the debounce: %v", err)
		}
		if held {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the debounce's firing never held")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pumpUntilHandled runs real timer-pump ticks until the debounce row is
// gone: its firing inserted the review or declined.
func (f holdStatusFixture) pumpUntilHandled(ctx context.Context, t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := f.registry.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
		if _, armed := f.debounceArmed(ctx, t); !armed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the debounce was never handled")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// endReview ends the running review with a real execution_complete,
// delivered to the session's actor the way the sandbox socket delivers it.
func (f holdStatusFixture) endReview(ctx context.Context, t *testing.T) {
	t.Helper()
	messageID := uuid.NewString()
	raw, err := json.Marshal(sandboxws.ExecutionComplete{
		Type: "execution_complete", MessageId: messageID, SessionId: f.sessionID.String(), Gen: 1,
		AckId: "execution_complete:" + messageID, Outcome: sandboxws.ExecutionCompleteOutcomeCompleted,
	})
	if err != nil {
		t.Fatalf("marshal execution_complete: %v", err)
	}
	a, err := f.registry.GetOrSpawn(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	if err := a.Send(ctx, sessionactor.SandboxEvent{Type: "execution_complete", Gen: 1, MessageID: messageID, Raw: raw, Reply: reply}); err != nil {
		t.Fatalf("Send execution_complete: %v", err)
	}
	select {
	case outcome := <-reply:
		if !outcome.Persisted {
			t.Fatal("execution_complete not persisted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the execution_complete outcome")
	}
}

// assertEndWokeTheDebounce checks, in one statement, that the review ended
// completed, that the debounce is due, and that both were last written by
// one transaction (equal xmin).
func (f holdStatusFixture) assertEndWokeTheDebounce(ctx context.Context, t *testing.T) {
	t.Helper()
	var status string
	var due, sameTx bool
	if err := f.rig.pool.QueryRow(ctx, `
		SELECT t.status::text, st.fires_at <= now(), t.xmin::text = st.xmin::text
		FROM turns t
		JOIN session_timers st ON st.session_id = t.session_id AND st.name = $2
		WHERE t.id = $1`, f.reviewID, sessionactor.TimerReviewRetriggerDebounce).Scan(&status, &due, &sameTx); err != nil {
		t.Fatalf("read the ended review and the debounce: %v", err)
	}
	if status != string(sqlcgen.TurnStatusCompleted) || !due || !sameTx {
		t.Fatalf("after the review ended: status %s, debounce due %v, same transaction %v; want completed, due, one transaction", status, due, sameTx)
	}
}

func (f holdStatusFixture) prSession(ctx context.Context, t *testing.T) sqlcgen.GithubPrSession {
	t.Helper()
	row, err := narvipg.NewGitHubPRSessionStore(f.rig.pool).GetBySessionID(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("read the pull request's session row: %v", err)
	}
	return row
}

// watchWhile runs act while a reader polls the status route the whole
// time, and returns every activity the reader saw, in order -- at least
// one after act returned.
func (f holdStatusFixture) watchWhile(t *testing.T, act func()) []restdtos.SessionActivity {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []restdtos.SessionActivity
		rerr error
	)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := f.read()
			mu.Lock()
			if err != nil && rerr == nil {
				rerr = err
			}
			seen = append(seen, got)
			mu.Unlock()
		}
	}()
	act()
	close(stop)
	<-done
	if rerr != nil {
		t.Fatalf("status read: %v", rerr)
	}
	return append(seen, f.mustRead(t))
}

// TestReviewRetriggerHold_FivePushesDuringOneReviewQueueOneReviewOfTheLastHead
// is the exit's first sentence (technical plan §24.9): five pushes landing
// more than the debounce window apart during one running review produce
// exactly one further review, of the last head, spending one budget slot.
// Each push's debounce comes due and fires through the real pump while the
// review runs, and holds: no turn, the push's head kept as the target, no
// budget spent, the debounce re-armed the backstop out, the session still
// read as running. The review's real end moves the debounce to now in its
// own transaction, the session reads scheduled, and the next pump tick
// queues the one review of the fifth head.
func TestReviewRetriggerHold_FivePushesDuringOneReviewQueueOneReviewOfTheLastHead(t *testing.T) {
	ctx := context.Background()
	f := newHoldStatusFixture(ctx, t)
	backstop := platform.DefaultTimeouts().ReviewRetriggerHoldBackstop

	var last string
	for push := 1; push <= 5; push++ {
		last = fmt.Sprintf("sha-pushed-%d", push)
		f.push(t, last)
		f.comeDue(ctx, t)
		f.pumpUntilHeld(ctx, t)

		turns, err := f.rig.turns.ListForSession(ctx, f.sessionID)
		if err != nil || len(turns) != 2 {
			t.Fatalf("push %d: %d turns (err %v), want the old review and the running one only", push, len(turns), err)
		}
		row := f.prSession(ctx, t)
		if row.PendingRetriggerHeadSha == nil || *row.PendingRetriggerHeadSha != last || row.AutoRetriggerCount != 0 {
			t.Fatalf("push %d: pending %v, count %d; want %s pending and nothing spent", push, row.PendingRetriggerHeadSha, row.AutoRetriggerCount, last)
		}
		// Re-armed by the firing, on the database's clock: exactly the
		// backstop past an arm made within the last few seconds.
		var rearmed bool
		if err := f.rig.pool.QueryRow(ctx, `SELECT fires_at - armed_at = make_interval(secs => $3) AND armed_at > now() - interval '10 seconds'
			FROM session_timers WHERE session_id = $1 AND name = $2`, f.sessionID, sessionactor.TimerReviewRetriggerDebounce, backstop.Seconds()).Scan(&rearmed); err != nil || !rearmed {
			t.Fatalf("push %d: debounce re-armed the backstop (%v) past a fresh arm: %v (err %v), want true", push, backstop, rearmed, err)
		}
		if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityRunning || got.Settled {
			t.Fatalf("push %d held: activity %q settled %v, want running", push, got.Activity, got.Settled)
		}
	}

	f.fetcher.setHead(last)
	f.endReview(ctx, t)
	f.assertEndWokeTheDebounce(ctx, t)
	if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityScheduled || got.Settled {
		t.Fatalf("the review ended: activity %q settled %v, want scheduled", got.Activity, got.Settled)
	}

	f.pumpUntilHandled(ctx, t)
	turns, err := f.rig.turns.ListForSession(ctx, f.sessionID)
	if err != nil || len(turns) != 3 {
		t.Fatalf("after the held review ran: %d turns (err %v), want the old review, the ended one and exactly one more", len(turns), err)
	}
	review := turns[len(turns)-1]
	if !review.IsReviewAttempt || review.Status != sqlcgen.TurnStatusPending || review.ReviewHeadSha == nil || *review.ReviewHeadSha != last {
		t.Fatalf("the held review = %+v, want a pending review attempt of %s", review, last)
	}
	row := f.prSession(ctx, t)
	if row.PendingRetriggerHeadSha != nil || row.AutoRetriggerCount != 1 {
		t.Fatalf("after the held review was queued: pending %v, count %d; want cleared and one slot spent", row.PendingRetriggerHeadSha, row.AutoRetriggerCount)
	}
	if _, armed := f.debounceArmed(ctx, t); armed {
		t.Fatal("the debounce is still armed after the held review was queued")
	}
	if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityQueued || got.Settled || got.PendingTurns != 1 {
		t.Fatalf("the held review queued: activity %q settled %v pending %d, want queued with one turn", got.Activity, got.Settled, got.PendingTurns)
	}
}

// TestReviewRetriggerHold_ABurstStraddlingTheReviewsEndReviewsOnce: a push
// lands while a review runs, the review ends inside that push's quiet
// window, and a second push follows seconds later (technical plan §24.2,
// §24.9). The review's end wakes only a debounce the hold re-armed, so it
// leaves this push's window running: no pump tick before the window ends
// queues anything, the second push re-arms the window, and when it fires
// the burst gets exactly one review, of its last head, spending one budget
// slot -- never a review of the first head now and one of the second
// after it.
func TestReviewRetriggerHold_ABurstStraddlingTheReviewsEndReviewsOnce(t *testing.T) {
	ctx := context.Background()
	f := newHoldStatusFixture(ctx, t)

	f.push(t, "sha-burst-1")
	f.endReview(ctx, t)
	var due bool
	if err := f.rig.pool.QueryRow(ctx, `SELECT fires_at <= now() FROM session_timers WHERE session_id = $1 AND name = $2`,
		f.sessionID, sessionactor.TimerReviewRetriggerDebounce).Scan(&due); err != nil {
		t.Fatalf("read the debounce after the review ended: %v", err)
	}
	if due {
		t.Fatal("the review's end woke a push's quiet window: the burst's first head would be reviewed at once")
	}
	for range 3 {
		if err := f.registry.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
	}
	if turns, err := f.rig.turns.ListForSession(ctx, f.sessionID); err != nil || len(turns) != 2 {
		t.Fatalf("pump ticks inside the window: %d turns (err %v), want the old review and the ended one only", len(turns), err)
	}

	const last = "sha-burst-2"
	f.push(t, last)
	f.fetcher.setHead(last)
	f.comeDue(ctx, t)
	f.pumpUntilHandled(ctx, t)

	turns, err := f.rig.turns.ListForSession(ctx, f.sessionID)
	if err != nil || len(turns) != 3 {
		t.Fatalf("after the burst's window ran out: %d turns (err %v), want exactly one more review", len(turns), err)
	}
	if review := turns[len(turns)-1]; !review.IsReviewAttempt || review.ReviewHeadSha == nil || *review.ReviewHeadSha != last {
		t.Fatalf("the burst's review = %+v, want a review attempt of %s", review, last)
	}
	if row := f.prSession(ctx, t); row.PendingRetriggerHeadSha != nil || row.AutoRetriggerCount != 1 {
		t.Fatalf("after the burst's review was queued: pending %v, count %d; want cleared and one slot spent", row.PendingRetriggerHeadSha, row.AutoRetriggerCount)
	}
}

// TestSessionStatus_AHeldReReviewNeverReadsSettled reads the status route
// concurrently through a held re-review's whole life (technical plan
// §43.20, §24.9): running while the firing holds behind the running
// review; running until the review's end commits and scheduled from that
// commit on, never finished in between, since the end and the debounce's
// wake-up are one transaction; then scheduled until the held review's turn
// exists, and queued after. No read is ever settled.
func TestSessionStatus_AHeldReReviewNeverReadsSettled(t *testing.T) {
	ctx := context.Background()
	f := newHoldStatusFixture(ctx, t)
	const pushed = "sha-pushed-while-reviewing"

	f.push(t, pushed)
	f.comeDue(ctx, t)
	for i, got := range f.watchWhile(t, func() { f.pumpUntilHeld(ctx, t) }) {
		if got.Settled || got.Activity != restdtos.SessionActivityActivityRunning {
			t.Fatalf("read %d while the firing held: activity %q settled %v, want running", i, got.Activity, got.Settled)
		}
	}

	f.fetcher.setHead(pushed)
	seen := f.watchWhile(t, func() { f.endReview(ctx, t) })
	scheduledFrom := -1
	for i, got := range seen {
		if got.Settled {
			t.Fatalf("read %d while the review ended: activity %q, settled", i, got.Activity)
		}
		switch got.Activity {
		case restdtos.SessionActivityActivityRunning:
			if scheduledFrom >= 0 {
				t.Fatalf("read %d while the review ended: running again after scheduled at read %d", i, scheduledFrom)
			}
		case restdtos.SessionActivityActivityScheduled:
			if scheduledFrom < 0 {
				scheduledFrom = i
			}
		default:
			t.Fatalf("read %d while the review ended: activity %q, want running, then scheduled", i, got.Activity)
		}
	}
	if scheduledFrom < 0 {
		t.Fatal("the session never read scheduled once the review ended")
	}

	seen = f.watchWhile(t, func() { f.pumpUntilHandled(ctx, t) })
	queuedFrom := -1
	for i, got := range seen {
		if got.Settled {
			t.Fatalf("read %d while the held review was queued: activity %q, settled", i, got.Activity)
		}
		switch got.Activity {
		case restdtos.SessionActivityActivityScheduled:
			if queuedFrom >= 0 {
				t.Fatalf("read %d: scheduled again after queued at read %d", i, queuedFrom)
			}
		case restdtos.SessionActivityActivityQueued:
			if queuedFrom < 0 {
				queuedFrom = i
			}
		default:
			t.Fatalf("read %d while the held review was queued: activity %q, want scheduled, then queued", i, got.Activity)
		}
	}
	if queuedFrom < 0 {
		t.Fatal("the session never read queued once the held review's turn existed")
	}
}
