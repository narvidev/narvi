package releasereview_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/releasereview"
	"github.com/narvidev/narvi/internal/platform"
)

// fakePendingLister is a test-only releasereview.PendingLister -- no real
// DB round trip, mirroring fakeMergedPRLister/fakeOutboxEnqueuer's own
// precedent (run_test.go) exactly. Simulates a queue: ClaimDue takes up to
// limit rows off the front -- a claimed row is never returned again,
// exactly like the real ClaimDueReleaseManifestPending query, which
// deletes it (queries/releasemanifestpending.sql) -- and Finish records
// which claimed checks' running rows were deleted, and after how many
// ListMergedBetween calls (the check's own first step), so a test can tell
// whether a row went before or after its check ran.
type fakePendingLister struct {
	rows      []sqlcgen.ReleaseManifestPending
	err       error
	calls     int
	lastLimit int32

	lister       *fakeMergedPRLister
	finished     []pgtype.UUID
	finishedAt   []int
	finishErr    error
	purgeCalls   int
	purgeMaxAges []time.Duration
}

func (f *fakePendingLister) ClaimDue(_ context.Context, limit int32) ([]sqlcgen.ReleaseManifestPending, error) {
	f.calls++
	f.lastLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	n := int(limit)
	if n > len(f.rows) {
		n = len(f.rows)
	}
	claimed := f.rows[:n]
	f.rows = f.rows[n:]
	return claimed, nil
}

func (f *fakePendingLister) Finish(_ context.Context, id pgtype.UUID) error {
	f.finished = append(f.finished, id)
	if f.lister != nil {
		f.finishedAt = append(f.finishedAt, f.lister.calls)
	}
	return f.finishErr
}

func (f *fakePendingLister) PurgeStaleClaimed(_ context.Context, maxAge time.Duration) (int64, error) {
	f.purgeCalls++
	f.purgeMaxAges = append(f.purgeMaxAges, maxAge)
	return 0, nil
}

func testWorkerSessionID(t *testing.T, raw string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		t.Fatalf("scan test session id: %v", err)
	}
	return id
}

// TestWorker_PumpOnce_ClaimsAndRunsEachClaimedRow proves the core
// blocking-finding fix #1 mechanism: PumpOnce claims whatever is due and
// calls Run for EACH claimed row, decoupled from any per-request
// context -- here proven by the fact that Run's own downstream
// SourceControl.ListMergedBetween call happens once per claimed row,
// each carrying that row's own owner/repo/PR identity.
func TestWorker_PumpOnce_ClaimsAndRunsEachClaimedRow(t *testing.T) {
	t.Parallel()

	store := &fakePendingLister{rows: []sqlcgen.ReleaseManifestPending{
		{ID: testWorkerSessionID(t, "00000000-0000-0000-0000-000000000001"), SessionID: testWorkerSessionID(t, "11111111-1111-1111-1111-111111111111"), Owner: "acme", Repo: "widgets", PrNumber: 10, BaseRef: "main", HeadRef: "release/1.0"},
		{ID: testWorkerSessionID(t, "00000000-0000-0000-0000-000000000002"), SessionID: testWorkerSessionID(t, "22222222-2222-2222-2222-222222222222"), Owner: "acme", Repo: "gadgets", PrNumber: 20, BaseRef: "main", HeadRef: "release/2.0"},
	}}
	lister := &fakeMergedPRLister{}
	outbox := &fakeOutboxEnqueuer{}

	worker := releasereview.NewWorker(store, releasereview.Deps{
		SourceControl: lister,
		Outbox:        outbox,
		Timeouts:      platform.DefaultTimeouts(),
	}, "gho_bottoken", platform.DefaultTimeouts())

	if err := worker.PumpOnce(context.Background()); err != nil {
		t.Fatalf("PumpOnce() error = %v", err)
	}

	if lister.calls != 2 {
		t.Fatalf("ListMergedBetween calls = %d, want 2 (one per claimed row)", lister.calls)
	}
	if outbox.calls != 2 {
		t.Fatalf("Outbox.Create calls = %d, want 2 (one rendered comment per claimed row)", outbox.calls)
	}
	if lister.lastReq.Owner != "acme" || lister.lastReq.Repo != "gadgets" || lister.lastReq.BaseRef != "main" || lister.lastReq.HeadRef != "release/2.0" {
		t.Errorf("last ListMergedBetween spec = %+v, want the SECOND claimed row's own owner/repo/base/head", lister.lastReq)
	}
	// The bot token authenticating every call is Worker's OWN
	// statically-configured credential -- never anything read off the
	// claimed row itself (ReleaseManifestPending carries no token column
	// at all; see Enqueue's own doc comment for why).
	if lister.lastReq.Token != "gho_bottoken" {
		t.Errorf("last ListMergedBetween spec.Token = %q, want %q (Worker's own configured bot token)", lister.lastReq.Token, "gho_bottoken")
	}
}

// TestWorker_PumpOnce_NoRowsDue_NoOp proves an empty claim batch is a
// harmless no-op: no error, no Run/ListMergedBetween/Outbox calls at all.
func TestWorker_PumpOnce_NoRowsDue_NoOp(t *testing.T) {
	t.Parallel()

	store := &fakePendingLister{}
	lister := &fakeMergedPRLister{}
	outbox := &fakeOutboxEnqueuer{}

	worker := releasereview.NewWorker(store, releasereview.Deps{
		SourceControl: lister,
		Outbox:        outbox,
		Timeouts:      platform.DefaultTimeouts(),
	}, "gho_bottoken", platform.DefaultTimeouts())

	if err := worker.PumpOnce(context.Background()); err != nil {
		t.Fatalf("PumpOnce() error = %v", err)
	}
	if lister.calls != 0 || outbox.calls != 0 {
		t.Errorf("ListMergedBetween/Outbox.Create calls = %d/%d, want 0/0 (nothing was due)", lister.calls, outbox.calls)
	}
}

// TestWorker_PumpOnce_ClaimBatchFails_PropagatesErrorNeverRunsAnything
// proves a failed claim-batch call aborts the tick (Run.go's own doc
// comment: Worker.Run logs this, never lets one bad tick kill the whole
// loop) and never calls Run for any row.
func TestWorker_PumpOnce_ClaimBatchFails_PropagatesErrorNeverRunsAnything(t *testing.T) {
	t.Parallel()

	store := &fakePendingLister{err: errors.New("db exploded")}
	lister := &fakeMergedPRLister{}
	outbox := &fakeOutboxEnqueuer{}

	worker := releasereview.NewWorker(store, releasereview.Deps{
		SourceControl: lister,
		Outbox:        outbox,
		Timeouts:      platform.DefaultTimeouts(),
	}, "gho_bottoken", platform.DefaultTimeouts())

	if err := worker.PumpOnce(context.Background()); err == nil {
		t.Fatal("PumpOnce() error = nil, want a propagated claim-batch error")
	}
	if lister.calls != 0 || outbox.calls != 0 {
		t.Errorf("ListMergedBetween/Outbox.Create calls = %d/%d, want 0/0 (claim batch itself failed)", lister.calls, outbox.calls)
	}
}

// TestWorker_PumpOnce_ClaimsOneRowAtATimeAndFinishesItAfterItsCheck pins
// the order the session's status relies on (technical plan §43.20,
// migrations/000146): each row is claimed alone -- so its running row's
// claimed_at is when its own check starts, and the status's bound on a
// running check measures that check -- and finished only after its check
// has run (any composition turn the check inserts is committed by then, so
// a snapshot that no longer counts the check already counts the turn). A
// tick first purges running rows claimed longer ago than
// ReleaseManifestCheckTimeout plus MCPStatusScheduledMargin -- the same
// bound the status applies -- and stops after pendingBatchSize rows.
func TestWorker_PumpOnce_ClaimsOneRowAtATimeAndFinishesItAfterItsCheck(t *testing.T) {
	t.Parallel()

	var rows []sqlcgen.ReleaseManifestPending
	for i := 1; i <= 7; i++ {
		id := testWorkerSessionID(t, fmt.Sprintf("00000000-0000-0000-0000-%012d", i))
		rows = append(rows, sqlcgen.ReleaseManifestPending{ID: id, SessionID: id, Owner: "acme", Repo: "widgets", PrNumber: int32(i), BaseRef: "main", HeadRef: "release/x"})
	}
	lister := &fakeMergedPRLister{}
	store := &fakePendingLister{rows: rows, lister: lister}
	timeouts := platform.DefaultTimeouts()
	worker := releasereview.NewWorker(store, releasereview.Deps{
		SourceControl: lister,
		Outbox:        &fakeOutboxEnqueuer{},
		Timeouts:      timeouts,
	}, "gho_bottoken", timeouts)

	if err := worker.PumpOnce(context.Background()); err != nil {
		t.Fatalf("PumpOnce() error = %v", err)
	}
	if store.lastLimit != 1 {
		t.Errorf("ClaimDue limit = %d, want 1: a row claimed in a batch waits behind the others with its claimed_at already running", store.lastLimit)
	}
	if len(store.finished) != 5 || lister.calls != 5 || len(store.rows) != 2 {
		t.Fatalf("finished %d rows after %d checks with %d left, want 5, 5 and 2: one tick handles pendingBatchSize rows", len(store.finished), lister.calls, len(store.rows))
	}
	for i, id := range store.finished {
		if id != rows[i].ID {
			t.Errorf("finished[%d] = %v, want %v (oldest first)", i, id, rows[i].ID)
		}
		if store.finishedAt[i] != i+1 {
			t.Errorf("row %d finished after %d checks, want %d: a row must go only after its own check has run", i, store.finishedAt[i], i+1)
		}
	}
	if store.purgeCalls != 1 || store.purgeMaxAges[0] != timeouts.ReleaseManifestCheckTimeout+timeouts.MCPStatusScheduledMargin {
		t.Errorf("purge calls %d with %v, want one, with ReleaseManifestCheckTimeout + MCPStatusScheduledMargin", store.purgeCalls, store.purgeMaxAges)
	}
}

// TestWorker_PumpOnce_FinishFailureNeverAbortsTheTick: a row whose delete
// fails stays claimed -- never claimed again, bounded by the status, and
// purged on a later tick -- and the tick goes on to the next row.
func TestWorker_PumpOnce_FinishFailureNeverAbortsTheTick(t *testing.T) {
	t.Parallel()

	lister := &fakeMergedPRLister{}
	store := &fakePendingLister{lister: lister, finishErr: errors.New("db hiccup"), rows: []sqlcgen.ReleaseManifestPending{
		{ID: testWorkerSessionID(t, "00000000-0000-0000-0000-000000000011"), Owner: "acme", Repo: "widgets", PrNumber: 1, BaseRef: "main", HeadRef: "release/1"},
		{ID: testWorkerSessionID(t, "00000000-0000-0000-0000-000000000012"), Owner: "acme", Repo: "widgets", PrNumber: 2, BaseRef: "main", HeadRef: "release/2"},
	}}
	worker := releasereview.NewWorker(store, releasereview.Deps{
		SourceControl: lister,
		Outbox:        &fakeOutboxEnqueuer{},
		Timeouts:      platform.DefaultTimeouts(),
	}, "gho_bottoken", platform.DefaultTimeouts())
	if err := worker.PumpOnce(context.Background()); err != nil {
		t.Fatalf("PumpOnce() error = %v, want a finish failure logged, never returned", err)
	}
	if lister.calls != 2 || len(store.finished) != 2 {
		t.Fatalf("%d checks, %d finish attempts, want both rows checked and a finish tried for each", lister.calls, len(store.finished))
	}
}

// TestWorker_PumpOnce_RequestsPendingBatchSizeNotOutboxBatchSize proves
// Worker asks its own store for a SMALL batch -- deliberately much
// smaller than outboxworker's own pumpBatchSize(20) -- since each row
// here can take minutes, not milliseconds, to process (worker.go's own
// doc comment on pendingBatchSize).
func TestWorker_PumpOnce_RequestsPendingBatchSizeNotOutboxBatchSize(t *testing.T) {
	t.Parallel()

	store := &fakePendingLister{}
	worker := releasereview.NewWorker(store, releasereview.Deps{
		SourceControl: &fakeMergedPRLister{},
		Outbox:        &fakeOutboxEnqueuer{},
		Timeouts:      platform.DefaultTimeouts(),
	}, "gho_bottoken", platform.DefaultTimeouts())

	if err := worker.PumpOnce(context.Background()); err != nil {
		t.Fatalf("PumpOnce() error = %v", err)
	}
	if store.lastLimit <= 0 || store.lastLimit >= 20 {
		t.Errorf("ClaimDue limit = %d, want a small batch strictly less than outboxworker's own 20-row batch size", store.lastLimit)
	}
}

// deadlineCapturingOutboxEnqueuer records the deadline (if any) the ctx
// it was called with actually carried -- used by
// TestWorker_Process_BoundsRunByReleaseManifestCheckTimeout below.
// Observed on Outbox.Create rather than SourceControl.ListMergedBetween
// deliberately: Run's own listCtx, context.WithTimeout(ctx, deps.
// Timeouts.GitHubListMergedBetweenTimeout), always re-wraps whatever ctx
// Worker.process handed it with a TIGHTER, closer deadline (2 minutes)
// before ListMergedBetween ever sees it (context.WithTimeout takes the
// EARLIER of the two) -- Outbox.Create, in contrast, is called with
// Run's own ctx parameter UNCHANGED, so it is the one call site that can
// actually observe Worker.process's own outer deadline directly.
type deadlineCapturingOutboxEnqueuer struct {
	sawDeadline   time.Time
	sawDeadlineOK bool
}

func (f *deadlineCapturingOutboxEnqueuer) Create(ctx context.Context, _ sqlcgen.CreateOutboxEntryParams) (sqlcgen.Outbox, error) {
	f.sawDeadline, f.sawDeadlineOK = ctx.Deadline()
	return sqlcgen.Outbox{}, nil
}

// TestWorker_Process_BoundsRunByReleaseManifestCheckTimeout proves
// Worker.process derives Run's own ctx from
// platform.Timeouts.ReleaseManifestCheckTimeout -- a genuinely bounded
// deadline, never the caller's own (here, context.Background()'s own
// deadline-free) ctx passed through unbounded. A regression here would
// mean the manifest check could hang forever on a stuck GitHub API call,
// exactly the class of bug blocking-finding fix #1 as a whole exists to
// close.
func TestWorker_Process_BoundsRunByReleaseManifestCheckTimeout(t *testing.T) {
	t.Parallel()

	outbox := &deadlineCapturingOutboxEnqueuer{}
	store := &fakePendingLister{rows: []sqlcgen.ReleaseManifestPending{
		{ID: testWorkerSessionID(t, "00000000-0000-0000-0000-000000000003"), SessionID: testWorkerSessionID(t, "33333333-3333-3333-3333-333333333333"), Owner: "acme", Repo: "widgets", PrNumber: 30, BaseRef: "main", HeadRef: "release/3.0"},
	}}
	timeouts := platform.DefaultTimeouts()

	worker := releasereview.NewWorker(store, releasereview.Deps{
		SourceControl: &fakeMergedPRLister{},
		Outbox:        outbox,
		Timeouts:      timeouts,
	}, "gho_bottoken", timeouts)

	before := time.Now()
	if err := worker.PumpOnce(context.Background()); err != nil {
		t.Fatalf("PumpOnce() error = %v", err)
	}
	after := time.Now()

	if !outbox.sawDeadlineOK {
		t.Fatal("Outbox.Create's own ctx carried no deadline at all, want one bounded by ReleaseManifestCheckTimeout")
	}
	wantMin := before.Add(timeouts.ReleaseManifestCheckTimeout)
	wantMax := after.Add(timeouts.ReleaseManifestCheckTimeout)
	if outbox.sawDeadline.Before(wantMin) || outbox.sawDeadline.After(wantMax) {
		t.Errorf("Outbox.Create's own ctx deadline = %v, want it within [%v, %v] (bounded by ReleaseManifestCheckTimeout = %v)",
			outbox.sawDeadline, wantMin, wantMax, timeouts.ReleaseManifestCheckTimeout)
	}
}
