//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/review"
	domainreviewtriage "github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// This file pins technical plan §24.9's second rule for an automatic
// review attempt on real Postgres, through the real actor: a review
// attempt the automatic re-review asked for that waited behind another
// turn checks, as it is dispatched, the head, base and ancestor chain it
// recorded against its pull request's live ones; one whose context moved
// ends context_moved without running -- notifying nobody, failing nothing,
// counted as no attempt -- and the re-review asks again, a bounded number
// of times in a row.

const (
	// ctxRecordedHead and ctxRecordedBase are the head and base commit
	// the queued attempt recorded, on base ref main.
	ctxRecordedHead = "sha-recorded-at-insert"
	ctxRecordedBase = "base-recorded-at-insert"
	// ctxMovedHead is the head the pull request moved to while the
	// attempt waited.
	ctxMovedHead = "sha-pushed-while-queued"
)

// fakeReviewLiveReader is a scripted ReviewLiveReader: the pull request as
// GetOpenPR answers it, each branch's live tip, and which commit is an
// ancestor of which.
type fakeReviewLiveReader struct {
	mu        sync.Mutex
	pr        ports.OpenPR
	notFound  bool
	prErr     error
	branches  map[string]string
	branchErr error
	ancestors map[string]bool // "ancestor..descendant"
	calls     int
}

func (f *fakeReviewLiveReader) GetOpenPR(_ context.Context, _, _ string, _ int, _ string) (ports.OpenPR, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.prErr != nil {
		return ports.OpenPR{}, false, f.prErr
	}
	return f.pr, !f.notFound, nil
}

func (f *fakeReviewLiveReader) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.branchErr != nil {
		return "", "", f.branchErr
	}
	return f.branches[spec.Branch], spec.Branch, nil
}

func (f *fakeReviewLiveReader) IsAncestor(_ context.Context, spec ports.IsAncestorSpec) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.ancestors[spec.Ancestor+".."+spec.Descendant], nil
}

func (f *fakeReviewLiveReader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// unmovedLiveReader answers the pull request exactly as the queued
// attempt recorded it: head, base ref and base commit unchanged, no
// ancestor link.
func unmovedLiveReader(repoFullName string, prNumber int32) *fakeReviewLiveReader {
	owner, repo, _ := strings.Cut(repoFullName, "/")
	return &fakeReviewLiveReader{
		pr:       ports.OpenPR{Owner: owner, Repo: repo, Number: int(prNumber), HeadSHA: ctxRecordedHead, BaseRef: "main"},
		branches: map[string]string{"main": ctxRecordedBase},
	}
}

// newContextRig hosts sessionID's actor on a registry of its own, wired
// like production: a commander, a spawn provider, the review diff fetcher
// the re-requested review reads its live head with (ctxMovedHead), the
// bot's credential, and reader as the context check's live reader (none
// when nil).
func newContextRig(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, reader *fakeReviewLiveReader, timeouts ...platform.Timeouts) *holdRig {
	t.Helper()
	rig := &holdRig{
		commander: &fakeSendCommander{},
		provider:  &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-context"}},
		fetcher:   &fakeReviewDiffFetcher{nextHeadSHA: ctxMovedHead, nextBaseRef: "main", nextDiff: oneLineReadableDiff},
	}
	opts := RegistryOptions{
		ReviewDiffFetcher: rig.fetcher, GitHubBotHandle: "narvi-bot",
		GitHubOutbound:       platform.MustNewGitHubOutboundConfig("test-token"),
		ReviewSizeExclusions: domainreviewtriage.DefaultSizeExclusions(),
	}
	if reader != nil {
		opts.ReviewLiveReader = reader
	}
	to := platform.DefaultTimeouts()
	if len(timeouts) > 0 {
		to = timeouts[0]
	}
	r, err := NewRegistry(ctx, pool, to, nil, rig.commander, rig.provider, "http://localhost:8080", nil, nil, "", nil, false, opts)
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

// newContextFixture is a review session (opted in, live) whose automatic
// re-review's last insert took its pending head, with a ready sandbox at
// gen 1.
func newContextFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, prNumber int32) *holdFixture {
	t.Helper()
	f := newHoldFixture(ctx, t, pool, repoFullName, prNumber, pgtype.UUID{})
	if _, err := f.prSessions.ClearPendingRetriggerHeadSHA(ctx, repoFullName, prNumber, holdPushedHead); err != nil {
		t.Fatalf("take the pending head: %v", err)
	}
	seedReadySandbox(ctx, t, pool, f.sessionID)
	return f
}

// recordedContext is the context the queued attempt recorded at insert.
func recordedContext(t *testing.T, ctxValue reviewverdict.Context) []byte {
	t.Helper()
	raw, err := json.Marshal(ctxValue)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// defaultRecordedContext is base ref main at ctxRecordedBase, no ancestor
// link, the current policy.
func defaultRecordedContext() reviewverdict.Context {
	return reviewverdict.Context{BaseRef: "main", BaseSHA: ctxRecordedBase, PolicyVersion: 1}
}

// backdate moves a turn's creation secs seconds into the past, on the
// database's clock. The check reads an attempt as queued when a turn
// created before it ended after it was created, comparing the ending
// replica's clock with the database's: seeded turns are spread seconds
// apart so that skew between the two never decides a test.
func backdate(ctx context.Context, t *testing.T, f *holdFixture, id pgtype.UUID, secs int) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `UPDATE turns SET created_at = now() - make_interval(secs => $2::int) WHERE id = $1`, id, secs); err != nil {
		t.Fatalf("backdate turn %v: %v", id, err)
	}
}

// seedRunningTurn creates the turn the attempt waits behind: a person's
// follow-up, processing on gen 1, created twenty seconds ago.
func seedRunningTurn(ctx context.Context, t *testing.T, f *holdFixture) sqlcgen.Turn {
	t.Helper()
	prompt := "@narvi-bot why is this flagged?"
	created, err := f.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: f.sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt})
	if err != nil {
		t.Fatalf("create the running turn: %v", err)
	}
	backdate(ctx, t, f, created.ID, 20)
	gen := int32(1)
	watermark, err := narvipg.NewEventStore(f.pool).MaxEventIDForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	messageID := fmt.Sprintf("msg-%s", created.ID.String())
	running, err := f.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: created.ID, Status: sqlcgen.TurnStatusProcessing,
		DispatchedAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
		DispatchedSandboxGen: &gen, DispatchedEventID: &watermark, DispatchedMessageID: &messageID,
	})
	if err != nil {
		t.Fatalf("move the running turn to processing: %v", err)
	}
	return running
}

// seedAttempt creates a pending review attempt of ctxRecordedHead with
// recorded as its context, asked for by trigger (nil: no trigger
// recorded), created ten seconds ago -- inserted the way a replica without
// the hold inserts one behind an open turn.
func seedAttempt(ctx context.Context, t *testing.T, f *holdFixture, trigger *string, recorded []byte) sqlcgen.Turn {
	t.Helper()
	// The prompt names the head it was built for, so a send of it shows.
	prompt := autoRetriggerPromptText + " Head: " + ctxRecordedHead
	head := ctxRecordedHead
	created, err := f.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: f.sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt,
		ReviewHeadSha: &head, ReviewVerdictContext: recorded,
		IsReviewAttempt: true, RequestTrigger: trigger,
	})
	if err != nil {
		t.Fatalf("create the review attempt: %v", err)
	}
	backdate(ctx, t, f, created.ID, 10)
	return created
}

func autoTrigger() *string {
	v := turn.RequestTriggerAuto
	return &v
}

// endRunningTurn ends the running turn with a real execution_complete
// through the actor. The turn's end starts a snapshot of the sandbox, so
// the evaluation its handler runs next dispatches nothing; once the
// snapshot cycle has returned the sandbox to ready, the next evaluation
// picks the queued attempt up -- as in production, where that cycle's own
// ready triggers it.
//
// beforeReady, when given, runs once the running turn's end has committed
// -- its wake-up of a held debounce with it -- and before the evaluation
// that reads the queued attempt.
func endRunningTurn(ctx context.Context, t *testing.T, f *holdFixture, rig *holdRig, beforeReady ...func()) {
	t.Helper()
	var running pgtype.UUID
	if err := f.pool.QueryRow(ctx, `SELECT id FROM turns WHERE session_id = $1 AND status = 'processing'`, f.sessionID).Scan(&running); err != nil {
		t.Fatalf("find the running turn: %v", err)
	}
	executionCompleteEnding(sandboxws.ExecutionCompleteOutcomeCompleted)(ctx, t, f, rig)
	waitForTurnStatus(ctx, t, f.turns, running, sqlcgen.TurnStatusCompleted)
	waitUntil(t, 5*time.Second, func() bool {
		var status string
		err := f.pool.QueryRow(ctx, `SELECT status::text FROM sandboxes WHERE session_id = $1`, f.sessionID).Scan(&status)
		return err == nil && status == "snapshotting"
	})
	for _, fn := range beforeReady {
		fn()
	}
	if _, err := f.pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready' WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatalf("return the sandbox to ready: %v", err)
	}
	sendEnsureDispatched(ctx, t, rig.actor)
}

// waitForAttempt waits until the attempt has left pending: ended, or
// dispatched.
func waitForAttempt(ctx context.Context, t *testing.T, f *holdFixture, id pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	var got sqlcgen.Turn
	waitUntil(t, 5*time.Second, func() bool {
		row, err := f.turns.Get(ctx, id)
		got = row
		return err == nil && row.Status != sqlcgen.TurnStatusPending
	})
	return got
}

// assertContextMoved checks that the attempt ended context_moved: failed
// through the abandon edge with the end reason, never stamped unconfirmed,
// never sent, with one synthetic execution_complete marked undelivered
// from pending, and no outbox row naming it.
func assertContextMoved(ctx context.Context, t *testing.T, f *holdFixture, rig *holdRig, attempt sqlcgen.Turn) {
	t.Helper()
	got := waitForAttempt(ctx, t, f, attempt.ID)
	if got.Status != sqlcgen.TurnStatusFailed || got.EndReason == nil || *got.EndReason != turn.EndReasonContextMoved {
		t.Fatalf("attempt: status %s end reason %v, want failed context_moved", got.Status, got.EndReason)
	}
	if got.ContextUnconfirmedAt.Valid || got.DispatchedAt.Valid || !got.CompletedAt.Valid {
		t.Fatalf("attempt: unconfirmed %v dispatched %v completed %v, want neither stamped and completed_at set", got.ContextUnconfirmedAt.Valid, got.DispatchedAt.Valid, got.CompletedAt.Valid)
	}
	var synthetic, delivered, dispatched, reason string
	err := f.pool.QueryRow(ctx, `SELECT COALESCE(payload->>'synthetic', ''), COALESCE(payload->>'delivered', ''), COALESCE(payload->>'dispatched', ''), COALESCE(payload->>'reason', '')
		FROM events WHERE session_id = $1 AND type = 'execution_complete' AND payload->>'turn_id' = $2`, f.sessionID, attempt.ID.String()).
		Scan(&synthetic, &delivered, &dispatched, &reason)
	if err != nil {
		t.Fatalf("read the attempt's terminal event: %v", err)
	}
	if synthetic != "true" || delivered != "false" || dispatched != "" || !strings.HasPrefix(reason, contextMovedReasonPrefix) {
		t.Fatalf("attempt's terminal event: synthetic %q delivered %q dispatched %q reason %q; want a synthetic end from pending, marked undelivered, naming the move", synthetic, delivered, dispatched, reason)
	}
	if n := countRows(ctx, t, f.pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND payload::text LIKE '%' || $2 || '%'`, f.sessionID, attempt.ID.String()); n != 0 {
		t.Fatalf("%d outbox rows name the attempt: a context_moved end notifies nobody and publishes no check", n)
	}
	for _, payload := range sentPayloads(rig.commander) {
		if strings.Contains(string(payload), ctxRecordedHead) {
			t.Fatalf("the attempt's prompt was sent: %s", payload)
		}
	}
}

func sentPayloads(c *fakeSendCommander) []json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]json.RawMessage(nil), c.payloads...)
}

// sentPrompts counts the prompts c sent, leaving aside every other
// command (a turn's end starts a snapshot, sent the same way).
func sentPrompts(t *testing.T, c *fakeSendCommander) int {
	t.Helper()
	n := 0
	for _, payload := range sentPayloads(c) {
		var frame struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &frame); err != nil {
			t.Fatalf("decode a sent command: %v", err)
		}
		if frame.Type == "prompt" {
			n++
		}
	}
	return n
}

// TestReviewContext_AQueuedAutomaticReviewWhoseHeadMovedDoesNotStart is
// the exit's fourth sentence for an automatic attempt (technical plan
// §24.9): an attempt queued behind a running turn, whose pull request's
// head moved while it waited, does not start when that turn ends. It ends
// context_moved, notifying nobody; in the same transaction the automatic
// request goes back to the lane -- the live head pending, the debounce due
// at once, one move counted, no budget given back -- and the next pump
// tick reviews the head the pull request has now, at once, unchecked.
func TestReviewContext_AQueuedAutomaticReviewWhoseHeadMovedDoesNotStart(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newContextFixture(ctx, t, pool, "acme/context-head", 600)
	if _, err := f.prSessions.IncrementAutoRetriggerCount(ctx, f.repoFullName, f.prNumber); err != nil {
		t.Fatal(err)
	}
	seedRunningTurn(ctx, t, f)
	attempt := seedAttempt(ctx, t, f, autoTrigger(), recordedContext(t, defaultRecordedContext()))
	reader := unmovedLiveReader(f.repoFullName, f.prNumber)
	reader.pr.HeadSHA = ctxMovedHead
	rig := newContextRig(ctx, t, pool, f.sessionID, reader)

	endRunningTurn(ctx, t, f, rig)
	assertContextMoved(ctx, t, f, rig, attempt)

	// The re-request, in the transaction that ended the attempt.
	var sameTx, due bool
	if err := pool.QueryRow(ctx, `
		SELECT t.xmin::text = st.xmin::text, st.fires_at <= now()
		FROM turns t JOIN session_timers st ON st.session_id = t.session_id AND st.name = $2
		WHERE t.id = $1`, attempt.ID, TimerReviewRetriggerDebounce).Scan(&sameTx, &due); err != nil {
		t.Fatalf("read the ended attempt and the debounce: %v", err)
	}
	if !sameTx || !due {
		t.Fatalf("debounce: written with the attempt's end %v, due %v; want both", sameTx, due)
	}
	row := f.prSession(ctx, t)
	if row.PendingRetriggerHeadSha == nil || *row.PendingRetriggerHeadSha != ctxMovedHead {
		t.Fatalf("pending head = %v, want the live head %s", row.PendingRetriggerHeadSha, ctxMovedHead)
	}
	if row.AutoRetriggerContextMoves != 1 || row.AutoRetriggerCount != 1 || row.AutoRetriggerDroppedAt.Valid {
		t.Fatalf("pull request: moves %d count %d dropped %v; want one move, the budget slot kept spent, no drop", row.AutoRetriggerContextMoves, row.AutoRetriggerCount, row.AutoRetriggerDroppedAt.Valid)
	}
	if got := sentPrompts(t, rig.commander); got != 0 {
		t.Fatalf("prompts sent = %d, want 0", got)
	}

	// The next tick reviews the head the pull request has now. The turns
	// that ended did so on this process's clock, which the new attempt's
	// creation, on the database's, must follow whatever the skew between
	// the two: their ends move a few seconds back.
	if _, err := pool.Exec(ctx, `UPDATE turns SET completed_at = completed_at - interval '5 seconds' WHERE session_id = $1 AND completed_at IS NOT NULL`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	pumpUntilDebounceHandled(ctx, t, rig, f)
	var newID pgtype.UUID
	var trigger *string
	if err := pool.QueryRow(ctx, `SELECT id, request_trigger FROM turns WHERE session_id = $1 AND is_review_attempt AND review_head_sha = $2`, f.sessionID, ctxMovedHead).Scan(&newID, &trigger); err != nil {
		t.Fatalf("the re-requested review of %s: %v", ctxMovedHead, err)
	}
	if trigger == nil || *trigger != turn.RequestTriggerAuto {
		t.Fatalf("the re-requested review's trigger = %v, want auto", trigger)
	}
	started := waitForAttempt(ctx, t, f, newID)
	if started.Status != sqlcgen.TurnStatusProcessing || started.ContextUnconfirmedAt.Valid {
		t.Fatalf("the re-requested review: status %s unconfirmed %v; want it started at once, unchecked", started.Status, started.ContextUnconfirmedAt.Valid)
	}
	row = f.prSession(ctx, t)
	if row.AutoRetriggerContextMoves != 0 || row.AutoRetriggerCount != 2 || row.PendingRetriggerHeadSha != nil {
		t.Fatalf("after the re-requested review started: moves %d count %d pending %v; want the moves reset, one more slot spent, nothing pending", row.AutoRetriggerContextMoves, row.AutoRetriggerCount, row.PendingRetriggerHeadSha)
	}
}

// TestReviewContext_AContextMovedTurnLeavesTheSessionNotFailedAndCountsAsNoAttempt
// is the exit's fourth sentence's second half (technical plan §24.9): a
// turn ended context_moved is no outcome of the session and no attempt. A
// review attempt ran to completion; an automatic attempt queued behind it
// ends context_moved. The session reads completed, not failed; the newest
// review attempt is the one that ran; no attempt is newer than it; and
// the status's last run is the one that ran, with the session's recorded
// failure reason still read against it.
func TestReviewContext_AContextMovedTurnLeavesTheSessionNotFailedAndCountsAsNoAttempt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newContextFixture(ctx, t, pool, "acme/context-no-attempt", 601)
	ran := holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusProcessing, "review this pull request", nil, time.Now())
	backdate(ctx, t, f, ran.ID, 20)
	attempt := seedAttempt(ctx, t, f, autoTrigger(), recordedContext(t, defaultRecordedContext()))
	reader := unmovedLiveReader(f.repoFullName, f.prNumber)
	reader.pr.HeadSHA = ctxMovedHead
	rig := newContextRig(ctx, t, pool, f.sessionID, reader)

	endRunningTurn(ctx, t, f, rig)
	assertContextMoved(ctx, t, f, rig, attempt)

	session, err := narvipg.NewSessionStore(pool).Get(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != sqlcgen.SessionStatusCompleted || session.FailureReason != nil {
		t.Fatalf("session: %s (reason %v), want completed: the attempt that never ran is no outcome", session.Status, session.FailureReason)
	}
	newest, err := f.turns.NewestReviewAttempt(ctx, f.sessionID)
	if err != nil || newest.ID != ran.ID {
		t.Fatalf("newest review attempt = %v (err %v), want the one that ran, %v", newest.ID, err, ran.ID)
	}
	ranRow, err := f.turns.Get(ctx, ran.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newer, err := f.turns.ExistsNewerReviewAttempt(ctx, f.sessionID, ranRow.CreatedAt); err != nil || newer {
		t.Fatalf("a newer review attempt than the one that ran: %v (err %v), want none", newer, err)
	}
	facts, err := narvipg.NewSessionStore(pool).ActivityFacts(ctx, f.sessionID, ReviewAutoRetriggerBudget)
	if err != nil {
		t.Fatal(err)
	}
	if facts.LastRunTurnID != ran.ID || facts.NewestTurnID != ran.ID || facts.LastRunStatus != string(sqlcgen.TurnStatusCompleted) {
		t.Fatalf("status facts: last run %v (%s), newest %v; want the one that ran, completed, both", facts.LastRunTurnID, facts.LastRunStatus, facts.NewestTurnID)
	}
}

// TestReviewContext_OnlyTheHeadBaseAndAncestorsMove pins what the check
// reads as a move (technical plan §24.9): the head, a retargeted base, a
// base commit that moved other than forward, an ancestor link that moved
// other than forward. A base or ancestor that only moved forward -- the
// code host confirms the recorded commit is an ancestor of the live one
// -- and a recorded policy older than the current one are no move: the
// attempt starts, unchecked by anything but its verdict's own freshness
// later, and the pull request's count of moves starts again.
func TestReviewContext_OnlyTheHeadBaseAndAncestorsMove(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	withLink := defaultRecordedContext()
	withLink.AncestorChain = []review.AncestorLink{{Ref: "feature-base", SHA: "link-recorded"}}

	for i, tc := range []struct {
		name     string
		recorded reviewverdict.Context
		live     func(r *fakeReviewLiveReader)
		moved    bool
	}{
		{name: "nothing moved", recorded: defaultRecordedContext(), live: func(*fakeReviewLiveReader) {}},
		{name: "the head moved", recorded: defaultRecordedContext(), live: func(r *fakeReviewLiveReader) { r.pr.HeadSHA = ctxMovedHead }, moved: true},
		{name: "the base was retargeted", recorded: defaultRecordedContext(), live: func(r *fakeReviewLiveReader) {
			r.pr.BaseRef = "release"
			r.branches["release"] = ctxRecordedBase
		}, moved: true},
		{name: "the base moved, not forward", recorded: defaultRecordedContext(), live: func(r *fakeReviewLiveReader) { r.branches["main"] = "base-rewritten" }, moved: true},
		{name: "the base moved forward", recorded: defaultRecordedContext(), live: func(r *fakeReviewLiveReader) {
			r.branches["main"] = "base-advanced"
			r.ancestors = map[string]bool{ctxRecordedBase + "..base-advanced": true}
		}},
		{name: "the ancestor link moved, not forward", recorded: withLink, live: func(r *fakeReviewLiveReader) {
			r.pr.AncestorChain = []ports.PRAncestorLink{{Ref: "feature-base"}}
			r.branches["feature-base"] = "link-rewritten"
		}, moved: true},
		{name: "the ancestor link moved forward", recorded: withLink, live: func(r *fakeReviewLiveReader) {
			r.pr.AncestorChain = []ports.PRAncestorLink{{Ref: "feature-base"}}
			r.branches["feature-base"] = "link-advanced"
			r.ancestors = map[string]bool{"link-recorded..link-advanced": true}
		}},
		{name: "an older policy recorded", recorded: reviewverdict.Context{BaseRef: "main", BaseSHA: ctxRecordedBase, PolicyVersion: 0}, live: func(*fakeReviewLiveReader) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContextFixture(ctx, t, pool, fmt.Sprintf("acme/context-moves-%d", i), int32(610+i))
			if _, err := pool.Exec(ctx, `UPDATE github_pr_sessions SET auto_retrigger_context_moves = 2 WHERE session_id = $1`, f.sessionID); err != nil {
				t.Fatal(err)
			}
			seedRunningTurn(ctx, t, f)
			attempt := seedAttempt(ctx, t, f, autoTrigger(), recordedContext(t, tc.recorded))
			reader := unmovedLiveReader(f.repoFullName, f.prNumber)
			tc.live(reader)
			rig := newContextRig(ctx, t, pool, f.sessionID, reader)

			endRunningTurn(ctx, t, f, rig)
			row := f.prSession(ctx, t)
			if tc.moved {
				assertContextMoved(ctx, t, f, rig, attempt)
				if row = f.prSession(ctx, t); row.AutoRetriggerContextMoves != 3 {
					t.Fatalf("moves = %d, want 3", row.AutoRetriggerContextMoves)
				}
				return
			}
			got := waitForAttempt(ctx, t, f, attempt.ID)
			if got.Status != sqlcgen.TurnStatusProcessing || got.EndReason != nil || got.ContextUnconfirmedAt.Valid {
				t.Fatalf("attempt: status %s end reason %v unconfirmed %v; want it started, confirmed", got.Status, got.EndReason, got.ContextUnconfirmedAt.Valid)
			}
			if got := sentPrompts(t, rig.commander); got != 1 {
				t.Fatalf("prompts sent = %d, want the attempt's", got)
			}
			if row = f.prSession(ctx, t); row.AutoRetriggerContextMoves != 0 {
				t.Fatalf("moves = %d, want them reset by the attempt that started", row.AutoRetriggerContextMoves)
			}
			if reader.callCount() == 0 {
				t.Fatal("the code host was never read: a queued attempt is checked")
			}
		})
	}
}

// TestReviewContext_AnUnreadableContextStartsTheTurnUnconfirmed: a
// comparison that cannot be made lets the attempt start, as every attempt
// did before the check, and records that its context was unconfirmed at
// start (technical plan §24.9) -- the pull request's read fails, the base
// branch's does, no live reader is configured, or the attempt recorded no
// usable context. An unknown is never read as fresh, nor as moved.
func TestReviewContext_AnUnreadableContextStartsTheTurnUnconfirmed(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	for i, tc := range []struct {
		name     string
		recorded []byte
		live     func(r *fakeReviewLiveReader)
		noReader bool
	}{
		{name: "the pull request's read fails", live: func(r *fakeReviewLiveReader) { r.prErr = errors.New("502 bad gateway") }},
		{name: "the base branch's read fails", live: func(r *fakeReviewLiveReader) { r.branchErr = errors.New("502 bad gateway") }},
		{name: "the pull request is no longer open", live: func(r *fakeReviewLiveReader) { r.notFound = true }},
		{name: "no live reader is configured", noReader: true},
		{name: "the attempt recorded no context", recorded: []byte{}},
		{name: "the attempt recorded no base", recorded: []byte(`{"baseRef":"","baseSha":"","policyVersion":1}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContextFixture(ctx, t, pool, fmt.Sprintf("acme/context-unread-%d", i), int32(630+i))
			if _, err := pool.Exec(ctx, `UPDATE github_pr_sessions SET auto_retrigger_context_moves = 2 WHERE session_id = $1`, f.sessionID); err != nil {
				t.Fatal(err)
			}
			seedRunningTurn(ctx, t, f)
			recorded := tc.recorded
			if recorded == nil {
				recorded = recordedContext(t, defaultRecordedContext())
			} else if len(recorded) == 0 {
				recorded = nil
			}
			attempt := seedAttempt(ctx, t, f, autoTrigger(), recorded)
			var reader *fakeReviewLiveReader
			if !tc.noReader {
				reader = unmovedLiveReader(f.repoFullName, f.prNumber)
				if tc.live != nil {
					tc.live(reader)
				}
			}
			rig := newContextRig(ctx, t, pool, f.sessionID, reader)

			endRunningTurn(ctx, t, f, rig)
			got := waitForAttempt(ctx, t, f, attempt.ID)
			if got.Status != sqlcgen.TurnStatusProcessing || got.EndReason != nil || !got.ContextUnconfirmedAt.Valid {
				t.Fatalf("attempt: status %s end reason %v unconfirmed %v; want it started, stamped unconfirmed", got.Status, got.EndReason, got.ContextUnconfirmedAt.Valid)
			}
			if got := sentPrompts(t, rig.commander); got != 1 {
				t.Fatalf("prompts sent = %d, want the attempt's", got)
			}
			if row := f.prSession(ctx, t); row.AutoRetriggerContextMoves != 0 || row.PendingRetriggerHeadSha != nil {
				t.Fatalf("pull request: moves %d pending %v; want the moves reset by the attempt that started, nothing requeued", row.AutoRetriggerContextMoves, row.PendingRetriggerHeadSha)
			}
		})
	}
}

// TestReviewContext_AnAutomaticRequestStopsAtTheBoundAndSaysSo is the
// exit's fifth sentence for an automatic request (technical plan §24.9):
// an automatic request whose attempts keep meeting a moved context asks
// again ReviewContextMoveMaxConsecutive times in a row; the move after
// that drops it -- nothing pending, no debounce, the drop on the status
// surface, said once and nowhere else -- and the next push clears the
// drop and re-arms it.
func TestReviewContext_AnAutomaticRequestStopsAtTheBoundAndSaysSo(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newContextFixture(ctx, t, pool, "acme/context-bound", 650)
	reader := unmovedLiveReader(f.repoFullName, f.prNumber)
	reader.pr.HeadSHA = ctxMovedHead
	bound := platform.DefaultTimeouts().ReviewContextMoveMaxConsecutive
	rig := newContextRig(ctx, t, pool, f.sessionID, reader)
	sessions := narvipg.NewSessionStore(pool)

	for move := 1; move <= bound+1; move++ {
		// Each round: an attempt queued behind a running turn whose
		// pull request moved while it waited.
		seedRunningTurn(ctx, t, f)
		attempt := seedAttempt(ctx, t, f, autoTrigger(), recordedContext(t, defaultRecordedContext()))
		endRunningTurn(ctx, t, f, rig)
		assertContextMoved(ctx, t, f, rig, attempt)

		row := f.prSession(ctx, t)
		_, due, _, armed := f.debounce(ctx, t)
		facts, err := sessions.ActivityFacts(ctx, f.sessionID, ReviewAutoRetriggerBudget)
		if err != nil {
			t.Fatal(err)
		}
		if int(row.AutoRetriggerContextMoves) != move {
			t.Fatalf("move %d: count %d", move, row.AutoRetriggerContextMoves)
		}
		if move <= bound {
			if row.PendingRetriggerHeadSha == nil || !armed || !due || row.AutoRetriggerDroppedAt.Valid || facts.ReviewRetriggerDroppedAt.Valid {
				t.Fatalf("move %d of %d: pending %v, debounce armed %v due %v, dropped %v; want it asked again", move, bound, row.PendingRetriggerHeadSha, armed, due, row.AutoRetriggerDroppedAt.Valid)
			}
			// The re-request is not fired here: the next round's attempt
			// stands for it, queued behind a turn again.
			if err := f.timers.Delete(ctx, sqlcgen.DeleteSessionTimerParams{SessionID: f.sessionID, Name: TimerReviewRetriggerDebounce}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.prSessions.ClearPendingRetriggerHeadSHA(ctx, f.repoFullName, f.prNumber, *row.PendingRetriggerHeadSha); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if row.PendingRetriggerHeadSha != nil || armed {
			t.Fatalf("past the bound: pending %v, debounce armed %v; want the request dropped", row.PendingRetriggerHeadSha, armed)
		}
		if !row.AutoRetriggerDroppedAt.Valid || row.AutoRetriggerDroppedHeadSha == nil || *row.AutoRetriggerDroppedHeadSha != ctxMovedHead {
			t.Fatalf("past the bound: dropped %v on %v; want the drop recorded on %s", row.AutoRetriggerDroppedAt.Valid, row.AutoRetriggerDroppedHeadSha, ctxMovedHead)
		}
		if !facts.ReviewRetriggerDroppedAt.Valid || facts.ReviewRetriggerDroppedHeadSha != ctxMovedHead {
			t.Fatalf("status facts: dropped %v on %q; want the drop shown", facts.ReviewRetriggerDroppedAt.Valid, facts.ReviewRetriggerDroppedHeadSha)
		}
		if n := countRows(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1`, f.sessionID); n != 0 {
			t.Fatalf("%d outbox rows: an automatic drop is shown on the status, never posted", n)
		}
		// Each running turn's end warns that a review session is
		// read-only; nothing else may warn.
		if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning'
			AND payload->>'message' NOT LIKE 'This session is a pull request review, which is read-only%'`, f.sessionID); n != 0 {
			t.Fatalf("%d session warnings: an automatic drop is shown on the status alone", n)
		}
	}

	// The next push clears the drop and re-arms the re-review.
	if _, err := f.prSessions.UpsertPendingRetriggerHeadSHA(ctx, f.repoFullName, f.prNumber, "sha-next-push"); err != nil {
		t.Fatal(err)
	}
	facts, err := sessions.ActivityFacts(ctx, f.sessionID, ReviewAutoRetriggerBudget)
	if err != nil {
		t.Fatal(err)
	}
	if facts.ReviewRetriggerDroppedAt.Valid || facts.ReviewRetriggerDroppedHeadSha != "" {
		t.Fatalf("after the next push: dropped %v on %q; want the drop cleared", facts.ReviewRetriggerDroppedAt.Valid, facts.ReviewRetriggerDroppedHeadSha)
	}
}

// TestReviewContext_AnAttemptDispatchedAtOnceIsNotChecked: the check
// costs a read of the code host only for an attempt that waited behind
// another turn (technical plan §24.9). One that finds the session free --
// the turn before it ended before it was inserted -- starts unchecked, its
// pull request's count of moves reset; a person's review request, queued
// or not, starts unchecked too.
func TestReviewContext_AnAttemptDispatchedAtOnceIsNotChecked(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	label := "label"

	for i, tc := range []struct {
		name    string
		trigger *string
		queued  bool
		reset   bool
	}{
		{name: "an automatic attempt that waited behind no turn", trigger: autoTrigger(), reset: true},
		{name: "a person's review request queued behind a turn", trigger: &label, queued: true},
		{name: "a review attempt with no trigger recorded, queued behind a turn", queued: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContextFixture(ctx, t, pool, fmt.Sprintf("acme/context-at-once-%d", i), int32(660+i))
			if _, err := pool.Exec(ctx, `UPDATE github_pr_sessions SET auto_retrigger_context_moves = 2 WHERE session_id = $1`, f.sessionID); err != nil {
				t.Fatal(err)
			}
			if !tc.queued {
				// A turn that ended an hour before the attempt.
				before := seedRunningTurn(ctx, t, f)
				if _, err := pool.Exec(ctx, `UPDATE turns SET status = 'completed', completed_at = now() - interval '1 hour', created_at = now() - interval '2 hours' WHERE id = $1`, before.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				seedRunningTurn(ctx, t, f)
			}
			attempt := seedAttempt(ctx, t, f, tc.trigger, recordedContext(t, defaultRecordedContext()))
			reader := unmovedLiveReader(f.repoFullName, f.prNumber)
			reader.pr.HeadSHA = ctxMovedHead
			rig := newContextRig(ctx, t, pool, f.sessionID, reader)

			if tc.queued {
				endRunningTurn(ctx, t, f, rig)
			} else {
				sendEnsureDispatched(ctx, t, rig.actor)
			}
			got := waitForAttempt(ctx, t, f, attempt.ID)
			if got.Status != sqlcgen.TurnStatusProcessing || got.EndReason != nil || got.ContextUnconfirmedAt.Valid {
				t.Fatalf("attempt: status %s end reason %v unconfirmed %v; want it started unchecked", got.Status, got.EndReason, got.ContextUnconfirmedAt.Valid)
			}
			if n := reader.callCount(); n != 0 {
				t.Fatalf("the code host was read %d times: an attempt the check skips costs no read", n)
			}
			wantMoves := int32(2)
			if tc.reset {
				wantMoves = 0
			}
			if row := f.prSession(ctx, t); row.AutoRetriggerContextMoves != wantMoves {
				t.Fatalf("moves = %d, want %d", row.AutoRetriggerContextMoves, wantMoves)
			}
		})
	}
}

// TestReviewContext_NoReadWhileTheSandboxCannotTakeTheTurn: an evaluation
// that would spawn or wait for the sandbox never reads the code host for
// the queued attempt -- the check runs where the turn is sent -- and the
// evaluation once the sandbox is ready reads it and applies it (technical
// plan §24.9).
func TestReviewContext_NoReadWhileTheSandboxCannotTakeTheTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newContextFixture(ctx, t, pool, "acme/context-booting", 670)
	before := seedRunningTurn(ctx, t, f)
	attempt := seedAttempt(ctx, t, f, autoTrigger(), recordedContext(t, defaultRecordedContext()))
	// The running turn ended, and the sandbox is booting again.
	if _, err := pool.Exec(ctx, `UPDATE turns SET status = 'completed', completed_at = now() WHERE id = $1`, before.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'booting', last_seen_at = now(), updated_at = now() WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	reader := unmovedLiveReader(f.repoFullName, f.prNumber)
	reader.pr.HeadSHA = ctxMovedHead
	rig := newContextRig(ctx, t, pool, f.sessionID, reader)

	sendEnsureDispatched(ctx, t, rig.actor)
	time.Sleep(200 * time.Millisecond)
	if got, err := f.turns.Get(ctx, attempt.ID); err != nil || got.Status != sqlcgen.TurnStatusPending {
		t.Fatalf("attempt while the sandbox boots: %s (err %v), want pending", got.Status, err)
	}
	if n := reader.callCount(); n != 0 {
		t.Fatalf("the code host was read %d times while the sandbox could not take the turn", n)
	}

	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready' WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, rig.actor)
	assertContextMoved(ctx, t, f, rig, attempt)
}

// TestReviewContext_TheDispatchHoldsAnAttemptItsReadDidNotCover: the
// dispatch applies the check only to the turn the read covered (technical
// plan §24.9). With no read, a read for another turn, or a read made while
// the sandbox could not take the turn, the attempt is neither sent nor
// ended: the evaluation dispatches nothing and re-arms the dispatch timer
// due at once, so the next one reads the attempt.
func TestReviewContext_TheDispatchHoldsAnAttemptItsReadDidNotCover(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newContextFixture(ctx, t, pool, "acme/context-hold", 680)
	attempt := seedAttempt(ctx, t, f, autoTrigger(), recordedContext(t, defaultRecordedContext()))
	rig := newContextRig(ctx, t, pool, f.sessionID, unmovedLiveReader(f.repoFullName, f.prNumber))
	other := pgtype.UUID{Bytes: [16]byte{9}, Valid: true}

	for _, tc := range []struct {
		name  string
		check *reviewContextCheck
	}{
		{name: "no read", check: nil},
		{name: "a read for another turn", check: &reviewContextCheck{turnID: other, outcome: reviewContextFresh}},
		{name: "a read made while the sandbox could not take the turn", check: &reviewContextCheck{turnID: attempt.ID, outcome: reviewContextUnread}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Driven directly, like dispatch_integration_test.go's
			// stale-epoch test: the actor has no command in flight.
			spawn, dispatch, _, repick, err := rig.actor.planDispatch(ctx, tc.check)
			if err != nil {
				t.Fatalf("planDispatch: %v", err)
			}
			if spawn != nil || dispatch != nil || repick {
				t.Fatalf("planDispatch = spawn %v dispatch %v repick %v, want nothing", spawn != nil, dispatch != nil, repick)
			}
			got, err := f.turns.Get(ctx, attempt.ID)
			if err != nil || got.Status != sqlcgen.TurnStatusPending || got.EndReason != nil {
				t.Fatalf("attempt: %s %v (err %v), want still pending", got.Status, got.EndReason, err)
			}
			row, ok := dispatchTimer(ctx, t, pool, f.sessionID)
			if !ok {
				t.Fatal("no dispatch timer: the held attempt has no trigger left")
			}
			var due bool
			if err := pool.QueryRow(ctx, `SELECT $1::timestamptz <= now()`, row.FiresAt).Scan(&due); err != nil || !due {
				t.Fatalf("dispatch timer due %v (err %v), want due at once", due, err)
			}
		})
	}
	if got := sentPrompts(t, rig.commander); got != 0 {
		t.Fatalf("prompts sent = %d, want 0", got)
	}
}

// TestReviewContext_TheReRequestKeepsAPushsWindowAndWakesAHeldDebounce: the
// re-request re-arms the lane under the hold's own rules (technical plan
// §24.9). With no debounce, it inserts one due at once; a push's quiet
// window still running keeps its trailing edge and its head; a debounce
// the hold re-armed is woken to now by the wake-up the attempt's end
// runs, in the same transaction, its created_at kept.
func TestReviewContext_TheReRequestKeepsAPushsWindowAndWakesAHeldDebounce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	window := platform.DefaultTimeouts().ReviewRetriggerDebounce

	for i, tc := range []struct {
		name string
		// arm leaves the debounce and pending head before the attempt's
		// end; nil leaves none.
		arm         func(t *testing.T, f *holdFixture)
		wantPending string
		// wantWindow: the debounce still fires about a window out.
		wantWindow bool
	}{
		{name: "no debounce", wantPending: ctxMovedHead},
		{name: "a push's window still running", arm: func(t *testing.T, f *holdFixture) {
			if _, err := f.prSessions.UpsertPendingRetriggerHeadSHA(ctx, f.repoFullName, f.prNumber, "sha-push-in-window"); err != nil {
				t.Fatal(err)
			}
			f.armDebounce(ctx, t, time.Now().Add(window))
		}, wantPending: "sha-push-in-window", wantWindow: true},
		{name: "a debounce the hold re-armed", arm: func(t *testing.T, f *holdFixture) {
			if _, err := f.prSessions.UpsertPendingRetriggerHeadSHA(ctx, f.repoFullName, f.prNumber, "sha-push-held"); err != nil {
				t.Fatal(err)
			}
			f.armHeldDebounce(ctx, t)
		}, wantPending: "sha-push-held"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContextFixture(ctx, t, pool, fmt.Sprintf("acme/context-rearm-%d", i), int32(690+i))
			seedRunningTurn(ctx, t, f)
			attempt := seedAttempt(ctx, t, f, autoTrigger(), recordedContext(t, defaultRecordedContext()))
			reader := unmovedLiveReader(f.repoFullName, f.prNumber)
			reader.pr.HeadSHA = ctxMovedHead
			rig := newContextRig(ctx, t, pool, f.sessionID, reader)

			// Armed once the running turn's end has committed -- and
			// woken whatever was held then -- as the debounce stands
			// when the queued attempt is read.
			var armedBefore sqlcgen.SessionTimer
			endRunningTurn(ctx, t, f, rig, func() {
				if tc.arm != nil {
					tc.arm(t, f)
					armedBefore, _, _, _ = f.debounce(ctx, t)
				}
			})
			assertContextMoved(ctx, t, f, rig, attempt)

			row := f.prSession(ctx, t)
			if row.PendingRetriggerHeadSha == nil || *row.PendingRetriggerHeadSha != tc.wantPending {
				t.Fatalf("pending head = %v, want %s", row.PendingRetriggerHeadSha, tc.wantPending)
			}
			got, due, _, ok := f.debounce(ctx, t)
			if !ok {
				t.Fatal("no debounce after the re-request")
			}
			if tc.wantWindow {
				if due || !got.FiresAt.Time.Equal(armedBefore.FiresAt.Time) {
					t.Fatalf("the push's window: fires %v (due %v), want it left at %v", got.FiresAt.Time, due, armedBefore.FiresAt.Time)
				}
				return
			}
			if !due {
				t.Fatalf("debounce fires %v, want due at once", got.FiresAt.Time)
			}
			if tc.arm != nil && !got.CreatedAt.Time.Equal(armedBefore.CreatedAt.Time) {
				t.Fatalf("the woken debounce's created_at moved from %v to %v: a stop compares with it", armedBefore.CreatedAt.Time, got.CreatedAt.Time)
			}
			var sameTx bool
			if err := pool.QueryRow(ctx, `SELECT t.xmin::text = st.xmin::text FROM turns t JOIN session_timers st ON st.session_id = t.session_id AND st.name = $2 WHERE t.id = $1`,
				attempt.ID, TimerReviewRetriggerDebounce).Scan(&sameTx); err != nil || !sameTx {
				t.Fatalf("the debounce and the attempt's end in one transaction: %v (err %v)", sameTx, err)
			}
		})
	}
}

// TestReviewContext_AStopFlaggedAttemptIsCancelledNotChecked: a person's
// stop flagged the queued attempt; the dispatch's stop gate cancels it
// before the check would read anything, and nothing is asked again.
func TestReviewContext_AStopFlaggedAttemptIsCancelledNotChecked(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newContextFixture(ctx, t, pool, "acme/context-stopped", 700)
	attempt := seedAttempt(ctx, t, f, autoTrigger(), recordedContext(t, defaultRecordedContext()))
	requestStop(ctx, t, pool, f.sessionID)
	reader := unmovedLiveReader(f.repoFullName, f.prNumber)
	reader.pr.HeadSHA = ctxMovedHead
	rig := newContextRig(ctx, t, pool, f.sessionID, reader)

	sendEnsureDispatched(ctx, t, rig.actor)
	waitForTurnStatus(ctx, t, f.turns, attempt.ID, sqlcgen.TurnStatusCancelled)
	if got, err := f.turns.Get(ctx, attempt.ID); err != nil || got.EndReason != nil {
		t.Fatalf("attempt end reason %v (err %v), want none: a stop cancelled it", got.EndReason, err)
	}
	if n := reader.callCount(); n != 0 {
		t.Fatalf("the code host was read %d times for a stopped attempt", n)
	}
	if row := f.prSession(ctx, t); row.PendingRetriggerHeadSha != nil || row.AutoRetriggerContextMoves != 0 {
		t.Fatalf("pull request: pending %v moves %d, want nothing asked again", row.PendingRetriggerHeadSha, row.AutoRetriggerContextMoves)
	}
	if _, err := f.timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: f.sessionID, Name: TimerReviewRetriggerDebounce}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("debounce after the stop: err %v, want none", err)
	}
}
