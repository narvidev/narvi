//go:build integration

package sessionactor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// This file pins technical plan §40.2 in the session actor, on real
// Postgres: while autonomy is frozen the automatic re-review inserts
// nothing and spends nothing, keeping its pushed head and its debounce;
// once the freeze lifts it reviews the head pushed last. And the freeze
// never reaches what a person asked for -- a prompt, the owed re-run of a
// person's request -- nor a turn already running.

// freezeAutonomyForActorTest freezes autonomy on pool, and lifts it when
// the test ends if it is still frozen.
func freezeAutonomyForActorTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	settings := narvipg.NewPlatformSettingsStore(pool)
	if _, err := settings.Freeze(ctx, pgtype.UUID{}, "an incident: hold every automatic action"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}
	t.Cleanup(func() { _, _ = settings.Unfreeze(context.Background()) })
}

func unfreezeAutonomyForActorTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := narvipg.NewPlatformSettingsStore(pool).Unfreeze(ctx); err != nil {
		t.Fatalf("unfreeze autonomy: %v", err)
	}
}

// reReviewSkips reads autonomy_freeze_skip_total{site=auto_re_review,
// reason=frozen} from this binary's one meter provider.
func reReviewSkips(t *testing.T) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	want := attribute.NewSet(attribute.String("site", string(domainautonomy.SiteAutoReReview)), attribute.String("reason", string(domainautonomy.SkipFrozen)))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "autonomy_freeze_skip_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("autonomy_freeze_skip_total is %T, want a Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if dp.Attributes.Equals(&want) {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// frozenDebounce reads the session's debounce and reports whether a frozen
// firing re-armed it after armedBefore: armed later, and due exactly the
// recheck interval past that arm, both stamped from one now() on the
// database's clock (TimerStore.HoldReviewRetriggerDebounce).
func (f *holdFixture) frozenDebounce(ctx context.Context, t *testing.T, armedBefore time.Time) (row sqlcgen.SessionTimer, rearmed, ok bool) {
	t.Helper()
	recheck := platform.DefaultTimeouts().AutonomyFreezeRecheckInterval.Seconds()
	err := f.pool.QueryRow(ctx, `
		SELECT id, session_id, name, fires_at, created_at, armed_at,
		       armed_at > $3 AND fires_at - armed_at = make_interval(secs => $4)
		FROM session_timers WHERE session_id = $1 AND name = $2`,
		f.sessionID, TimerReviewRetriggerDebounce, armedBefore, recheck,
	).Scan(&row.ID, &row.SessionID, &row.Name, &row.FiresAt, &row.CreatedAt, &row.ArmedAt, &rearmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.SessionTimer{}, false, false
	}
	if err != nil {
		t.Fatalf("read the debounce: %v", err)
	}
	return row, rearmed, true
}

// waitFrozenRearm waits for a frozen firing to re-arm the debounce armed at
// armedBefore, and returns the re-armed row.
func waitFrozenRearm(ctx context.Context, t *testing.T, f *holdFixture, armedBefore time.Time) sqlcgen.SessionTimer {
	t.Helper()
	var row sqlcgen.SessionTimer
	waitUntil(t, 5*time.Second, func() bool {
		r, rearmed, ok := f.frozenDebounce(ctx, t, armedBefore)
		row = r
		return ok && rearmed
	})
	return row
}

// reviewAttemptsOf counts the review attempts of head the debounce
// inserted.
func (f *holdFixture) reviewAttemptsOf(ctx context.Context, t *testing.T, head string) int {
	t.Helper()
	return countRows(ctx, t, f.pool, `SELECT count(*) FROM turns WHERE session_id = $1 AND is_review_attempt AND review_head_sha = $2`, f.sessionID, head)
}

// assertNothingSpent checks a frozen pull request: no turn, the pushed
// head kept as the target, no budget spent and no budget notice.
func assertNothingSpent(ctx context.Context, t *testing.T, f *holdFixture, wantHead string) {
	t.Helper()
	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 0 {
		t.Fatalf("turns = %d, want none while frozen", len(turns))
	}
	pr := f.prSession(ctx, t)
	if pr.PendingRetriggerHeadSha == nil || *pr.PendingRetriggerHeadSha != wantHead || pr.AutoRetriggerCount != 0 || pr.AutoRetriggerBudgetNoticeSentAt.Valid {
		t.Fatalf("pull request: pending %v, count %d, notice sent %v; want %s kept, nothing spent, no notice",
			pr.PendingRetriggerHeadSha, pr.AutoRetriggerCount, pr.AutoRetriggerBudgetNoticeSentAt.Valid, wantHead)
	}
	if n := countRows(ctx, t, f.pool, `SELECT count(*) FROM outbox WHERE session_id = $1`, f.sessionID); n != 0 {
		t.Fatalf("%d outbox rows while frozen, want none", n)
	}
}

// TestAutoReReview_Frozen_NoTurnNoBudgetNoFetch: a debounce firing while
// autonomy is frozen reads nothing from GitHub, inserts no turn, spends no
// budget and posts nothing; the pushed head stays the target, and the
// debounce is re-armed the recheck interval out -- not at the hold's
// backstop -- with its created_at kept.
func TestAutoReReview_Frozen_NoTurnNoBudgetNoFetch(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newHoldFixture(ctx, t, pool, "acme/frozen-rereview", 901, pgtype.UUID{})
	f.armDebounce(ctx, t, time.Now())
	before, _, _, _ := f.debounce(ctx, t)
	freezeAutonomyForActorTest(ctx, t, pool)
	rig := newHoldRig(ctx, t, pool, f.sessionID, nil)

	skipsBefore := reReviewSkips(t)
	if err := rig.actor.Send(ctx, TimerFired{Name: TimerReviewRetriggerDebounce}); err != nil {
		t.Fatalf("Send TimerFired: %v", err)
	}
	row := waitFrozenRearm(ctx, t, f, before.ArmedAt.Time)
	if got := reReviewSkips(t) - skipsBefore; got != 1 {
		t.Errorf("autonomy_freeze_skip_total{site=auto_re_review, reason=frozen} rose by %d, want 1", got)
	}
	if !row.CreatedAt.Time.Equal(before.CreatedAt.Time) {
		t.Errorf("created_at moved from %v to %v: a stop compares with it", before.CreatedAt.Time, row.CreatedAt.Time)
	}
	if _, _, held, _ := f.debounce(ctx, t); held {
		t.Error("the frozen debounce sits at the hold's backstop; want the recheck interval")
	}
	if got := rig.fetcher.prCalls(); got != 0 {
		t.Errorf("GetPullRequest calls = %d, want 0", got)
	}
	assertNothingSpent(ctx, t, f, holdPushedHead)
}

// freezingFetcher commits the autonomy freeze as the fire reads the pull
// request -- after its phase 1 found autonomy free, before its insert.
type freezingFetcher struct {
	*fakeReviewDiffFetcher
	pool *pgxpool.Pool

	mu     sync.Mutex
	frozen bool
	err    error
}

func (f *freezingFetcher) GetPullRequest(ctx context.Context, owner, repo string, prNumber int32, token string) (githubapi.PullRequest, error) {
	f.mu.Lock()
	if !f.frozen {
		f.frozen = true
		_, f.err = narvipg.NewPlatformSettingsStore(f.pool).Freeze(ctx, pgtype.UUID{}, "an incident: frozen during the fetch")
	}
	f.mu.Unlock()
	return f.fakeReviewDiffFetcher.GetPullRequest(ctx, owner, repo, prNumber, token)
}

func (f *freezingFetcher) freezeErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// TestAutoReReview_FreezeDuringFetch_NoInsert: the freeze is read again at
// the insert. A freeze committed while the fire read the pull request --
// after it decided to review -- inserts nothing and spends nothing; the
// pushed head stays the target and the debounce is re-armed the recheck
// interval out.
func TestAutoReReview_FreezeDuringFetch_NoInsert(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newHoldFixture(ctx, t, pool, "acme/frozen-mid-fetch", 902, pgtype.UUID{})
	f.armDebounce(ctx, t, time.Now())
	before, _, _, _ := f.debounce(ctx, t)
	t.Cleanup(func() { _, _ = narvipg.NewPlatformSettingsStore(pool).Unfreeze(context.Background()) })
	fetcher := &freezingFetcher{
		fakeReviewDiffFetcher: &fakeReviewDiffFetcher{nextHeadSHA: holdLiveHead, nextBaseRef: "main", nextDiff: oneLineReadableDiff},
		pool:                  pool,
	}
	rig := newHoldRig(ctx, t, pool, f.sessionID, fetcher)

	skipsBefore := reReviewSkips(t)
	if err := rig.actor.Send(ctx, TimerFired{Name: TimerReviewRetriggerDebounce}); err != nil {
		t.Fatalf("Send TimerFired: %v", err)
	}
	waitFrozenRearm(ctx, t, f, before.ArmedAt.Time)
	if got := reReviewSkips(t) - skipsBefore; got != 1 {
		t.Errorf("autonomy_freeze_skip_total{site=auto_re_review, reason=frozen} rose by %d, want 1", got)
	}
	if err := fetcher.freezeErr(); err != nil {
		t.Fatalf("freeze during the fetch: %v", err)
	}
	if got := fetcher.prCalls(); got != 1 {
		t.Fatalf("GetPullRequest calls = %d, want 1: the fire decided to review before the freeze", got)
	}
	assertNothingSpent(ctx, t, f, holdPushedHead)
}

// pumpFrozenFiring makes the debounce due, runs one real timer-pump tick,
// and waits for its frozen firing to re-arm it.
func pumpFrozenFiring(ctx context.Context, t *testing.T, rig *holdRig, f *holdFixture) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `UPDATE session_timers SET fires_at = now() WHERE session_id = $1 AND name = $2`, f.sessionID, TimerReviewRetriggerDebounce); err != nil {
		t.Fatalf("make the debounce due: %v", err)
	}
	before, _, _, ok := f.debounce(ctx, t)
	if !ok {
		t.Fatal("no debounce to fire: the frozen firing must re-arm it, never drop it")
	}
	if err := rig.registry.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	waitFrozenRearm(ctx, t, f, before.ArmedAt.Time)
}

// TestAutoReReview_Unfreeze_ReviewsTheLastPushedHead: a re-review held
// through the freeze, and a second push while frozen, review once after
// the freeze lifts, for the head pushed last: one automatic review, one
// slot of the budget, the pending head cleared. Every firing here comes
// from the real timer pump, so a frozen firing that dropped its debounce
// would leave nothing to fire.
func TestAutoReReview_Unfreeze_ReviewsTheLastPushedHead(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	const secondPush = "sha-pushed-while-frozen"
	f := newHoldFixture(ctx, t, pool, "acme/frozen-then-free", 903, pgtype.UUID{})
	f.armDebounce(ctx, t, time.Now())
	freezeAutonomyForActorTest(ctx, t, pool)
	fetcher := &fakeReviewDiffFetcher{nextHeadSHA: secondPush, nextBaseRef: "main", nextDiff: oneLineReadableDiff}
	rig := newHoldRig(ctx, t, pool, f.sessionID, fetcher)

	pumpFrozenFiring(ctx, t, rig, f)
	assertNothingSpent(ctx, t, f, holdPushedHead)

	// A second push while frozen moves the target, as the synchronize
	// webhook does: the pending head, and the debounce armed.
	if _, err := f.prSessions.UpsertPendingRetriggerHeadSHA(ctx, f.repoFullName, f.prNumber, secondPush); err != nil {
		t.Fatalf("push again while frozen: %v", err)
	}
	f.armDebounce(ctx, t, time.Now().Add(platform.DefaultTimeouts().ReviewRetriggerDebounce))
	pumpFrozenFiring(ctx, t, rig, f)
	assertNothingSpent(ctx, t, f, secondPush)

	unfreezeAutonomyForActorTest(ctx, t, pool)
	if _, err := pool.Exec(ctx, `UPDATE session_timers SET fires_at = now() WHERE session_id = $1 AND name = $2`, f.sessionID, TimerReviewRetriggerDebounce); err != nil {
		t.Fatalf("bring the recheck due: %v", err)
	}
	pumpUntilDebounceHandled(ctx, t, rig, f)

	if got := f.reviewAttemptsOf(ctx, t, secondPush); got != 1 {
		t.Fatalf("automatic reviews of %s after the unfreeze = %d, want 1", secondPush, got)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM turns WHERE session_id = $1`, f.sessionID); n != 1 {
		t.Fatalf("turns = %d, want the one review", n)
	}
	if row := f.prSession(ctx, t); row.PendingRetriggerHeadSha != nil || row.AutoRetriggerCount != 1 {
		t.Fatalf("after the review was queued: pending %v, count %d; want cleared and 1", row.PendingRetriggerHeadSha, row.AutoRetriggerCount)
	}
	if got := fetcher.prCalls(); got != 1 {
		t.Fatalf("GetPullRequest calls = %d, want 1: the frozen firings read nothing", got)
	}
}

// TestFreeze_ProcessingTurnCompletes: a freeze severs no running turn
// (§32.8, inherited). With a review processing on a ready sandbox when
// autonomy freezes, and the debounce's firing reading the freeze, nothing
// is sent to the sandbox, the turn's deadline timer is untouched and the
// sandbox's gen is not retired; the turn's own execution_complete then
// completes it -- and that end does not wake the frozen debounce.
func TestFreeze_ProcessingTurnCompletes(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newHoldFixture(ctx, t, pool, "acme/frozen-mid-turn", 904, pgtype.UUID{})
	seedReadySandbox(ctx, t, pool, f.sessionID)
	open := holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusProcessing, "review this pull request", nil, time.Now())
	deadline, err := f.timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: f.sessionID, Name: TimerTurnDeadline,
		FiresAt: pgtype.Timestamptz{Time: time.Now().Add(platform.DefaultTimeouts().TurnDeadline), Valid: true},
	})
	if err != nil {
		t.Fatalf("arm the turn's deadline: %v", err)
	}
	f.armDebounce(ctx, t, time.Now())
	before, _, _, _ := f.debounce(ctx, t)
	rig := newHoldRig(ctx, t, pool, f.sessionID, nil)
	freezeAutonomyForActorTest(ctx, t, pool)

	if err := rig.actor.Send(ctx, TimerFired{Name: TimerReviewRetriggerDebounce}); err != nil {
		t.Fatalf("Send TimerFired: %v", err)
	}
	frozen := waitFrozenRearm(ctx, t, f, before.ArmedAt.Time)

	got, err := f.turns.Get(ctx, open.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("the running turn is %s after the freeze, want still processing", got.Status)
	}
	if n := rig.commander.callCount(); n != 0 {
		t.Fatalf("%d commands sent to the sandbox after the freeze, want none: no stop", n)
	}
	after, err := f.timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: f.sessionID, Name: TimerTurnDeadline})
	if err != nil {
		t.Fatalf("read the turn's deadline: %v", err)
	}
	if !after.FiresAt.Time.Equal(deadline.FiresAt.Time) || !after.ArmedAt.Time.Equal(deadline.ArmedAt.Time) {
		t.Fatalf("turn_deadline moved from %v to %v after the freeze, want untouched", deadline.FiresAt.Time, after.FiresAt.Time)
	}
	var status string
	var gen int32
	var retired *int32
	if err := pool.QueryRow(ctx, `SELECT status::text, gen, stop_retire_gen FROM sandboxes WHERE session_id = $1`, f.sessionID).Scan(&status, &gen, &retired); err != nil {
		t.Fatalf("read the sandbox: %v", err)
	}
	if status != string(sqlcgen.SandboxStatusReady) || gen != 1 || retired != nil {
		t.Fatalf("sandbox %s gen %d retired %v after the freeze, want ready gen 1 not retired", status, gen, retired)
	}

	executionCompleteEnding(sandboxws.ExecutionCompleteOutcomeCompleted)(ctx, t, f, rig)
	waitUntil(t, 5*time.Second, func() bool {
		ended, err := f.turns.Get(ctx, open.ID)
		return err == nil && turn.IsTerminal(turn.State(ended.Status))
	})
	if ended, _ := f.turns.Get(ctx, open.ID); ended.Status != sqlcgen.TurnStatusCompleted {
		t.Fatalf("the turn ended %s, want completed", ended.Status)
	}
	row, rearmed, ok := f.frozenDebounce(ctx, t, before.ArmedAt.Time)
	if !ok || !rearmed || !row.ArmedAt.Time.Equal(frozen.ArmedAt.Time) {
		t.Fatalf("the turn's end touched the frozen debounce (present %v, frozen lead %v, armed %v -> %v); want it left at the recheck",
			ok, rearmed, frozen.ArmedAt.Time, row.ArmedAt.Time)
	}
}

// TestFreeze_PromptDispatches: a person's prompt is never held. Queued
// while autonomy is frozen, it is dispatched to the ready sandbox at once.
func TestFreeze_PromptDispatches(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	seedReadySandbox(ctx, t, pool, sessionID)
	freezeAutonomyForActorTest(ctx, t, pool)
	turnStore := narvipg.NewTurnStore(pool)
	created := createPendingTurn(ctx, t, turnStore, sessionID, "a person's prompt, while frozen")

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return commander.callCount() == 1 })

	got, err := turnStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if got.Status != sqlcgen.TurnStatusProcessing || !got.DispatchedAt.Valid {
		t.Fatalf("turn status %s, dispatched %v; want processing, dispatched while frozen", got.Status, got.DispatchedAt.Valid)
	}
}

// TestFreeze_OwedRequestReRuns: the owed re-run of a person's review
// request is the person's request, never the automatic lane's, and is
// never held: frozen, the button request whose context moved is re-run for
// the head the pull request has now.
func TestFreeze_OwedRequestReRuns(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newContextFixture(ctx, t, pool, "acme/frozen-owed-rerun", 905)
	requester := createRequester(ctx, t, pool, "frozen-rerun")
	seedRunningTurn(ctx, t, f)
	attempt := seedPersonsAttempt(ctx, t, f, turn.RequestTriggerButton, requester, 0)
	rig := newOwedRig(ctx, t, pool, f.sessionID, movedReader(f), &fakeReviewRequestAuthorizer{allowed: true})
	freezeAutonomyForActorTest(ctx, t, pool)

	endRunningTurn(ctx, t, f, rig)
	assertContextMoved(ctx, t, f, rig, attempt)
	assertOwed(ctx, t, f, attempt, 1)

	pumpUntilOwedServed(ctx, t, rig, f)
	runs := reRuns(ctx, t, f)
	if len(runs) != 1 {
		t.Fatalf("re-runs while frozen = %d, want 1", len(runs))
	}
	if runs[0].RequestTrigger == nil || *runs[0].RequestTrigger != turn.RequestTriggerButton || runs[0].RequestedBy != requester {
		t.Fatalf("re-run = %+v, want the person's button request", runs[0])
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, f.sessionID, string(ports.NotificationKindGitHubVerdict)); n != 0 {
		t.Fatalf("%d drop notices for a request that was re-run", n)
	}
}
