//go:build integration

// Integration tests proving Builder.PumpOnce against a REAL Postgres
// instance (§9.1) -- gated behind the "integration" build tag, matching
// internal/app/imagebuild/builder_integration_test.go's own conventions
// exactly (testcontainers Postgres, embedded migrations via golang-migrate's
// iofs source driver, a real *pgxpool.Pool, a single global OTel
// MeterProvider wired once in TestMain). Run via `make test-integration`.
// newTestPool (below) no longer starts its OWN container per call -- see
// sharedpool_integration_test.go's own top doc comment for the
// container/pool this now shares with every other test in this package,
// including this file's own TestMain, which now ALSO owns starting that
// shared container (merged with the OTel MeterProvider wiring this file
// used to own alone -- Go allows exactly one TestMain per test binary).
package outboxworker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// newTestPool returns this package's own single, shared Postgres pool --
// started ONCE for the whole test binary by TestMain (below), not freshly
// per test/container as this function used to do itself. Kept as a thin
// wrapper under its own original name/signature so every existing call
// site in this package's own *_integration_test.go files keeps compiling
// unchanged. See sharedpool_integration_test.go's own top doc comment for
// the full container-reuse story.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return IntegrationTestPool(t)
}

// fakeNotifier is a test-only ports.Notifier recording every Deliver call
// and returning a caller-configured error -- mirrors internal/app/
// imagebuild's own fakeBuildProvider precedent (configurable behavior + a
// recorded-calls slice, mutex-guarded). delay (audit fix H6's own lease
// test, see TestPumpOnce_SlowSequentialDelivery_ConcurrentTickNeverStealsRowMidDelivery
// below), when non-zero, makes Deliver actually block for that long
// (real wall-clock time, honoring ctx cancellation) before returning --
// simulating a genuinely slow, sequential outbound notifier call rather
// than an instant fake, since H6's own race only manifests when one
// row's real delivery duration is comparable to (or exceeds) the claim
// window.
type fakeNotifier struct {
	mu sync.Mutex

	delivered []ports.Notification
	nextErr   error
	delay     time.Duration
}

var _ ports.Notifier = (*fakeNotifier)(nil)

func (f *fakeNotifier) Deliver(ctx context.Context, n ports.Notification) error {
	f.mu.Lock()
	delay := f.delay
	err := f.nextErr
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, n)
	return err
}

func (f *fakeNotifier) deliverCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.delivered)
}

func (f *fakeNotifier) setNextErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextErr = err
}

// otelReader is the SINGLE ManualReader backing the SINGLE, GLOBAL SDK
// MeterProvider TestMain (sharedpool_integration_test.go) registers for
// this whole test binary -- mirrors internal/app/imagebuild's own
// TestMain/otelReader precedent exactly, adapted to this package's own
// "narvi/outboxworker" meter. This file used to own TestMain itself
// (wiring otelReader was its only job); it now lives in sharedpool_
// integration_test.go, merged with that file's own shared-Postgres-
// container startup -- Go allows exactly one TestMain per test binary,
// so once this package needed a shared container too, the two had to
// combine into one function.
var otelReader *sdkmetric.ManualReader

// readDeadLetterCount sums every data point of the narvi/outboxworker
// meter's own outbox_dead_letter_total counter -- CUMULATIVE across every
// test in this binary (see TestMain's own doc comment), so callers must
// diff a "before" and "after" reading around their own PumpOnce call(s).
func readDeadLetterCount(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "narvi/outboxworker" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "outbox_dead_letter_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("outbox_dead_letter_total metric data = %T, want metricdata.Sum[int64]", m.Data)
			}
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	return 0
}

// readInt64Gauge returns the latest recorded value of the narvi/outboxworker
// meter's own int64 gauge named metricName (outbox_lag_seconds or, audit fix
// M15/M17's own addition, outbox_due_backlog_count), and whether any data
// point was found at all. Unlike readDeadLetterCount's own counter (which
// sums CUMULATIVE data points), a gauge reading reflects the CURRENT value
// as of the most recent Record call -- exactly what PumpOnce sets fresh
// every tick, so no before/after diffing is needed here.
func readInt64Gauge(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader, metricName string) (int64, bool) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "narvi/outboxworker" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != metricName {
				continue
			}
			gauge, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("%s metric data = %T, want metricdata.Gauge[int64]", metricName, m.Data)
			}
			if len(gauge.DataPoints) == 0 {
				return 0, false
			}
			return gauge.DataPoints[len(gauge.DataPoints)-1].Value, true
		}
	}
	return 0, false
}

// seedOutboxEntry inserts a fresh 'pending' outbox row directly (bypassing
// app/sessionactor entirely -- this package's own tests exercise Builder in
// isolation, matching imagebuild's own scope precedent) and returns it.
func seedOutboxEntry(ctx context.Context, t *testing.T, store *narvipg.OutboxStore, kind string, payload map[string]any) sqlcgen.Outbox {
	t.Helper()

	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	row, err := store.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		Kind:    kind,
		Payload: rawPayload,
	})
	if err != nil {
		t.Fatalf("seed outbox entry: %v", err)
	}
	return row
}

// TestPumpOnce_SuccessfulDelivery_MarksDelivered proves a claimed row whose
// notifier succeeds is marked 'delivered', with delivered_at set.
func TestPumpOnce_SuccessfulDelivery_MarksDelivered(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)

	row := seedOutboxEntry(ctx, t, store, "slack", map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "hi"})

	notifier := &fakeNotifier{}
	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack: notifier,
	}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if notifier.deliverCount() != 1 {
		t.Fatalf("deliverCount = %d, want 1", notifier.deliverCount())
	}

	got, err := store.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get outbox entry: %v", err)
	}
	if got.Status != sqlcgen.OutboxStatusDelivered {
		t.Fatalf("Status = %q, want %q", got.Status, sqlcgen.OutboxStatusDelivered)
	}
	if !got.DeliveredAt.Valid {
		t.Fatal("DeliveredAt.Valid = false, want true")
	}
}

// TestPumpOnce_FailedDelivery_BacksOffAndNotRetriedBeforeNextAttemptAt
// proves a failed delivery attempt is NOT retried before its own
// next_attempt_at, confirmed by direct Postgres inspection -- a second
// PumpOnce call immediately after the first must not re-claim the still-
// not-due row (deliverCount stays at 1), but once next_attempt_at has
// genuinely elapsed, a later PumpOnce DOES pick it up again (deliverCount
// advances to 2).
func TestPumpOnce_FailedDelivery_BacksOffAndNotRetriedBeforeNextAttemptAt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)

	seedOutboxEntry(ctx, t, store, "slack", map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "hi"})

	notifier := &fakeNotifier{nextErr: errors.New("notifier: delivery failed")}
	timeouts := platform.DefaultTimeouts()
	timeouts.OutboxBackoffBase = 200 * time.Millisecond
	timeouts.OutboxBackoffMax = 500 * time.Millisecond
	timeouts.OutboxClaimDuration = 1 * time.Millisecond

	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack: notifier,
	}, timeouts, &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce (1st): %v", err)
	}
	if notifier.deliverCount() != 1 {
		t.Fatalf("deliverCount after 1st PumpOnce = %d, want 1", notifier.deliverCount())
	}

	// Immediately re-pumping must NOT re-claim the row: its next_attempt_at
	// was just pushed forward by the backoff decision (>= 200ms away).
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce (2nd, immediate): %v", err)
	}
	if notifier.deliverCount() != 1 {
		t.Fatalf("deliverCount after immediate 2nd PumpOnce = %d, want still 1 (not yet due)", notifier.deliverCount())
	}

	time.Sleep(250 * time.Millisecond)

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce (3rd, after backoff elapsed): %v", err)
	}
	if notifier.deliverCount() != 2 {
		t.Fatalf("deliverCount after 3rd PumpOnce = %d, want 2 (backoff elapsed)", notifier.deliverCount())
	}
}

// TestPumpOnce_DeadLettersAfterMaxAttempts proves a row that keeps failing
// is eventually dead-lettered (never retried indefinitely), and the
// outbox_dead_letter_total counter increments accordingly.
func TestPumpOnce_DeadLettersAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)

	row := seedOutboxEntry(ctx, t, store, "slack", map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "hi"})

	notifier := &fakeNotifier{nextErr: errors.New("notifier: permanently broken")}
	timeouts := platform.DefaultTimeouts()
	// Backoff/claim windows are intentionally short (this test wants MANY
	// attempts to run fast, not a real-scale schedule) but NOT
	// single-digit-millisecond: a real, reproduced flake (second
	// re-verification pass, §13.2) traced back to exactly this test
	// pairing a 1ms/2ms backoff window with a per-iteration sleep of only
	// 5ms and zero margin beyond it. NextRetryAt is computed from THIS
	// process's own time.Now() (recordFailure, builder.go) but
	// ListDuePendingOutboxEntries' own "next_attempt_at <= now()" check
	// runs against the testcontainers Postgres server's OWN clock -- under
	// full-suite -race load (this whole module's test binaries running in
	// parallel), ordinary scheduling jitter plus any nonzero skew between
	// the two clocks can exceed a 1-5ms margin, silently skipping a tick's
	// own claim (the row simply isn't "due" yet from Postgres's own point
	// of view). A skipped tick does NOT error and does NOT change the
	// row's status, so it passes unnoticed here -- it only ever surfaces as
	// this test needing more than the available iterations to reach
	// MaxAttempts, i.e. exactly the observed "Status = pending, want
	// dead_letter" failure. 10ms/20ms leaves a real order-of-magnitude
	// margin over realistic same-host clock skew while staying fast.
	timeouts.OutboxBackoffBase = 10 * time.Millisecond
	timeouts.OutboxBackoffMax = 20 * time.Millisecond
	timeouts.OutboxClaimDuration = 10 * time.Millisecond

	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack: notifier,
	}, timeouts, &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	before := readDeadLetterCount(ctx, t, otelReader)

	// domain/outbox.MaxAttempts is 10 -- pump enough times, with a sleep
	// between ticks that comfortably clears the backoff window's own worst
	// case (OutboxBackoffMax, 20ms) PLUS a generous buffer against the
	// scheduling-jitter/clock-skew margin explained above, that the row is
	// guaranteed to have been attempted at least that many times. 15
	// iterations (a 5-iteration buffer over the 10 actually needed) at 60ms
	// apart keeps this well under a second even with the wider windows.
	for i := 0; i < 15; i++ {
		if err := builder.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce (%d): %v", i, err)
		}
		time.Sleep(60 * time.Millisecond)
	}

	got, err := store.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get outbox entry: %v", err)
	}
	if got.Status != sqlcgen.OutboxStatusDeadLetter {
		t.Fatalf("Status = %q, want %q", got.Status, sqlcgen.OutboxStatusDeadLetter)
	}

	after := readDeadLetterCount(ctx, t, otelReader)
	if after-before != 1 {
		t.Fatalf("outbox_dead_letter_total delta = %d, want 1", after-before)
	}

	// A dead-lettered row must never be re-claimed by a later tick.
	deliveredBefore := notifier.deliverCount()
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce after dead-letter: %v", err)
	}
	if notifier.deliverCount() != deliveredBefore {
		t.Fatalf("deliverCount changed after dead-letter (%d -> %d), want unchanged", deliveredBefore, notifier.deliverCount())
	}
}

// TestPumpOnce_PerRowIsolation proves one row's delivery failure never
// aborts the rest of the batch: two rows are claimed in the same tick, one
// notifier fails and the other succeeds, and the successful one is still
// marked delivered.
func TestPumpOnce_PerRowIsolation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)

	failingRow := seedOutboxEntry(ctx, t, store, "slack", map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "hi"})
	okRow := seedOutboxEntry(ctx, t, store, "github", map[string]any{"owner": "acme", "repo": "widgets", "pr_number": 1, "text": "hi"})

	slackNotifier := &fakeNotifier{nextErr: errors.New("notifier: slack down")}
	githubNotifier := &fakeNotifier{}

	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack:  slackNotifier,
		ports.NotificationKindGitHub: githubNotifier,
	}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	gotFailing, err := store.Get(ctx, failingRow.ID)
	if err != nil {
		t.Fatalf("get failing row: %v", err)
	}
	if gotFailing.Status != sqlcgen.OutboxStatusPending {
		t.Fatalf("failing row Status = %q, want %q (retry scheduled)", gotFailing.Status, sqlcgen.OutboxStatusPending)
	}

	gotOK, err := store.Get(ctx, okRow.ID)
	if err != nil {
		t.Fatalf("get ok row: %v", err)
	}
	if gotOK.Status != sqlcgen.OutboxStatusDelivered {
		t.Fatalf("ok row Status = %q, want %q", gotOK.Status, sqlcgen.OutboxStatusDelivered)
	}
}

// TestPumpOnce_ConcurrentTicksNeverDoubleClaim proves FOR UPDATE SKIP
// LOCKED genuinely prevents two concurrent PumpOnce calls (simulating two
// pods' own independent Builder instances against the SAME underlying
// Postgres) from both claiming (and therefore both delivering) the same
// row.
func TestPumpOnce_ConcurrentTicksNeverDoubleClaim(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)

	seedOutboxEntry(ctx, t, store, "slack", map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "hi"})

	notifierA := &fakeNotifier{}
	notifierB := &fakeNotifier{}

	timeouts := platform.DefaultTimeouts()
	builderA, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack: notifierA,
	}, timeouts, &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder A: %v", err)
	}
	builderB, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack: notifierB,
	}, timeouts, &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder B: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = builderA.PumpOnce(ctx) }()
	go func() { defer wg.Done(); _ = builderB.PumpOnce(ctx) }()
	wg.Wait()

	total := notifierA.deliverCount() + notifierB.deliverCount()
	if total != 1 {
		t.Fatalf("total delivers across both concurrent ticks = %d, want exactly 1", total)
	}
}

// TestPumpOnce_NoNotifierRegistered_TreatedAsFailure proves a row whose
// kind has no registered notifier is treated as a failed attempt (retried
// with backoff, eventually dead-lettered), never silently dropped or
// panicking.
func TestPumpOnce_NoNotifierRegistered_TreatedAsFailure(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)

	row := seedOutboxEntry(ctx, t, store, "linear", map[string]any{"agent_session_id": "as1", "organization_id": "org1", "text": "hi", "success": true})

	timeouts := platform.DefaultTimeouts()
	timeouts.OutboxBackoffBase = 1 * time.Millisecond
	timeouts.OutboxBackoffMax = 2 * time.Millisecond
	timeouts.OutboxClaimDuration = 1 * time.Millisecond

	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		// Deliberately no NotificationKindLinear entry.
	}, timeouts, &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	got, err := store.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get outbox entry: %v", err)
	}
	if got.Status != sqlcgen.OutboxStatusPending {
		t.Fatalf("Status = %q, want %q (scheduled for retry)", got.Status, sqlcgen.OutboxStatusPending)
	}
	if got.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", got.Attempts)
	}
}

// TestPumpOnce_SlowSequentialDelivery_ConcurrentTickNeverStealsRowMidDelivery
// is audit fix H6's own lease test: it drives a REAL, wall-clock-slow,
// SEQUENTIAL delivery of a 3-row batch (fakeNotifier's own delay field,
// not an instant fake) and proves a concurrent second PumpOnce call --
// simulating another pod's own independent Builder, or this pod's own
// next tick -- can never cause the SAME row to be delivered twice, even
// though it deliberately lands in exactly the window the pre-fix bug
// exploited: AFTER the batch's own shared claim-time protection for a
// late row has already elapsed, but BEFORE that row's own turn in
// builderA's sequential processing.
//
// Critically, builderB's OWN notifier is ALSO slow here (delay =
// rowDeliveryDelay, matching builderA's own per-row delay) -- NOT an
// instant fake. An earlier version of this test used a zero-delay
// notifier for builderB, which passed even against the FIRST, incomplete
// fix (RenewOutboxClaim guarded only by "AND status = 'pending'", no
// compare-and-swap): with an instant concurrent claimant, builderB's own
// delivery (claim -> Deliver -> MarkDelivered) always completed and
// flipped the row to 'delivered' well before builderA's own renewal
// attempt ever ran, so builderA's status-only renewal correctly found the
// row no longer 'pending' and skipped it -- by TIMING COINCIDENCE, not
// because the guard actually had CAS teeth. A genuinely slow concurrent
// claimant (this version) reproduces the real-world case where BOTH
// builders' own deliveries are still in flight at the moment the first
// builder's renewal runs: row3's own status is still 'pending' (builderB
// has re-claimed it but not yet delivered), so a status-only guard would
// ALSO succeed for builderA -- letting both builders call Deliver on the
// same row concurrently. Only a genuine optimistic-concurrency
// compare-and-swap against the row's own next_attempt_at (RenewOutboxClaim's
// current guard, queries/outbox.sql) can tell "untouched" apart from
// "a different builder already re-claimed it and is mid-delivery", which
// is what this test now actually exercises and asserts.
//
// Timing (see the constants below): builderA claims all 3 rows in one
// transaction at t0, each stamped with the SAME provisional
// next_attempt_at = t0+claimDuration (400ms) -- the pre-fix bug's own
// root cause. builderA then delivers them ONE AT A TIME, each taking
// rowDeliveryDelay (300ms) of REAL wall-clock time: row1 [t0, t0+300ms],
// row2 [t0+300ms, t0+600ms], row3 [t0+600ms, t0+900ms]. Row3's own
// batch-level claim (t0+400ms) has therefore already expired a full
// 200ms before row3's own turn even starts (t0+600ms) -- exactly the H6
// race window (§ builder.go's own claimBatch doc comment). builderB fires
// its OWN concurrent PumpOnce at t0+500ms, squarely inside that
// [400ms, 600ms] window -- mirroring a second pod's tick winning the SKIP
// LOCKED race for a row this pod's own builderA has not yet reached --
// and its own Deliver call for row3 then runs [500ms, 800ms], still very
// much in flight when builderA reaches row3's own turn at 600ms.
//
// Without the CAS fix, builderA reaching row3's own turn at t0+600ms
// would find it STILL 'pending' (builderB's own slow delivery does not
// complete until t0+800ms) and its renewal would succeed on status alone,
// so builderA would ALSO call Deliver on row3 WHILE builderB's own
// Deliver call for the SAME row is still in flight -- a genuine
// concurrent double notifier.Deliver call for the same row. With the CAS
// fix, builderA's own attempt() for row3 calls RenewOutboxClaim expecting
// row3's ORIGINAL next_attempt_at (the value from builderA's own
// claimBatch, t0+400ms) -- but builderB's own claim at t0+500ms already
// changed it (to t0+900ms), so builderA's renewal matches zero rows
// (pgx.ErrNoRows) regardless of row3's status, and builderA skips Deliver
// entirely. So row3 is delivered EXACTLY once, by builderB, and rows 1
// and 2 (never stolen -- see the per-row analysis in this function's own
// body) are delivered exactly once each, by builderA -- asserted below via
// a per-row delivery-count map, not merely "no error".
func TestPumpOnce_SlowSequentialDelivery_ConcurrentTickNeverStealsRowMidDelivery(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)

	row1 := seedOutboxEntry(ctx, t, store, "slack", map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "row1"})
	row2 := seedOutboxEntry(ctx, t, store, "slack", map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "row2"})
	row3 := seedOutboxEntry(ctx, t, store, "slack", map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "row3"})
	rows := []sqlcgen.Outbox{row1, row2, row3}

	const (
		// rowDeliveryDelay is EVERY builder's own per-row REAL wall-clock
		// Deliver duration -- big enough, relative to claimDuration below,
		// that the 3rd row's own turn genuinely starts after the FIRST
		// row's own claim-duration window has elapsed twice over, AND
		// that builderB's own concurrent delivery for row3 is still in
		// flight at the moment builderA reaches row3's own turn (the
		// exact adversarial timing the reviewers reproduced -- see this
		// function's own doc comment above).
		rowDeliveryDelay = 300 * time.Millisecond
		// claimDuration deliberately sits BETWEEN one row's own delivery
		// duration (so a freshly-renewed claim always comfortably
		// outlives the single delivery attempt it protects -- the H6 fix's
		// own real invariant, platform.Timeouts.Validate()'s new
		// "OutboxClaimDuration > OutboxDeliveryTimeout" check) and TWO
		// rows' cumulative delivery duration (so the ORIGINAL, unrenewed
		// batch-level claim for a row queued behind two others is
		// genuinely stale by the time that row's own turn arrives).
		claimDuration = 400 * time.Millisecond
		// concurrentTickDelay lands builderB's own PumpOnce call squarely
		// inside [claimDuration, 2*rowDeliveryDelay] = [400ms, 600ms]: the
		// window where row3's own original claim has expired but its own
		// renewal (due at row3's own turn, 2*rowDeliveryDelay = 600ms)
		// has not yet happened.
		concurrentTickDelay = 500 * time.Millisecond
	)

	notifierA := &fakeNotifier{delay: rowDeliveryDelay}
	notifierB := &fakeNotifier{delay: rowDeliveryDelay}

	timeouts := platform.DefaultTimeouts()
	timeouts.OutboxClaimDuration = claimDuration
	timeouts.OutboxDeliveryTimeout = 5 * time.Second // generous; never the limiting factor here

	builderA, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack: notifierA,
	}, timeouts, &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder A: %v", err)
	}
	builderB, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack: notifierB,
	}, timeouts, &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder B: %v", err)
	}

	// errgroup (not a naked goroutine + sync.WaitGroup, CLAUDE.md §11)
	// runs builderA's own PumpOnce concurrently with builderB's own,
	// fired concurrentTickDelay later from this goroutine -- its error,
	// if any, surfaces from eg.Wait() below, never via t.Errorf/t.Fatalf
	// called from inside the goroutine itself.
	var eg errgroup.Group
	eg.Go(func() error {
		return builderA.PumpOnce(ctx)
	})

	time.Sleep(concurrentTickDelay)
	if err := builderB.PumpOnce(ctx); err != nil {
		t.Fatalf("builderB PumpOnce: %v", err)
	}

	if err := eg.Wait(); err != nil {
		t.Fatalf("builderA PumpOnce: %v", err)
	}

	notifierA.mu.Lock()
	notifierB.mu.Lock()
	all := make([]ports.Notification, 0, len(notifierA.delivered)+len(notifierB.delivered))
	all = append(all, notifierA.delivered...)
	all = append(all, notifierB.delivered...)
	notifierA.mu.Unlock()
	notifierB.mu.Unlock()

	if len(all) != len(rows) {
		t.Fatalf("total Deliver() calls across both builders = %d, want exactly %d (one per row, no double-delivery)", len(all), len(rows))
	}

	// Per-row delivery-count map -- the real assertion this test exists
	// for: not merely that no error occurred, but that EVERY row was
	// delivered EXACTLY once even under the adversarial slow-concurrent-
	// claimant timing above.
	deliveredCount := map[string]int{}
	for _, n := range all {
		deliveredCount[string(n.Payload)]++
	}
	for _, row := range rows {
		if got := deliveredCount[string(row.Payload)]; got != 1 {
			t.Errorf("outbox row %s (payload %s) delivered %d times, want exactly 1", row.ID.String(), row.Payload, got)
		}
	}

	for _, row := range rows {
		got, err := store.Get(ctx, row.ID)
		if err != nil {
			t.Fatalf("get outbox entry %s: %v", row.ID.String(), err)
		}
		if got.Status != sqlcgen.OutboxStatusDelivered {
			t.Errorf("outbox row %s Status = %q, want %q", row.ID.String(), got.Status, sqlcgen.OutboxStatusDelivered)
		}
	}
}

// TestPumpOnce_OutboxDueBacklogCount_ReflectsBacklogIndependentOfCurrentTickClaim
// is audit fix M15/M17's own test: several rows are seeded then pushed
// into the future (mid-backoff, next_attempt_at well past "now"), so
// claimBatch's own ListDuePendingOutboxEntries claims NONE of them this
// tick -- proving the pre-existing outbox_lag_seconds gauge would read
// exactly zero here (nothing was due), while the NEW, independent
// outbox_due_backlog_count gauge still reports the real backlog, because
// its own CountPendingOutboxEntries query is deliberately NOT restricted
// to next_attempt_at <= now().
func TestPumpOnce_OutboxDueBacklogCount_ReflectsBacklogIndependentOfCurrentTickClaim(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)

	const backlogSize = 3
	for i := 0; i < backlogSize; i++ {
		row := seedOutboxEntry(ctx, t, store, "slack", map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "hi"})
		// Push this row's own next_attempt_at an hour into the future --
		// simulating a row genuinely mid-backoff during a sustained outage,
		// not merely "not yet processed this millisecond". Direct SQL
		// (rather than a store method) mirrors this file's own
		// "bypassing app/sessionactor entirely" precedent for seedOutboxEntry
		// above -- no existing OutboxStore method sets next_attempt_at
		// without also touching status/attempts.
		if _, err := pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() + interval '1 hour' WHERE id = $1`, row.ID); err != nil {
			t.Fatalf("push row %s next_attempt_at into the future: %v", row.ID.String(), err)
		}
	}

	notifier := &fakeNotifier{}
	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack: notifier,
	}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	// Nothing was due -- claimBatch's own ListDuePendingOutboxEntries
	// claimed none of the backlogSize rows, so no delivery was even
	// attempted.
	if notifier.deliverCount() != 0 {
		t.Fatalf("deliverCount = %d, want 0 (every row is mid-backoff, none due this tick)", notifier.deliverCount())
	}

	lag, lagFound := readInt64Gauge(ctx, t, otelReader, "outbox_lag_seconds")
	if !lagFound {
		t.Fatal("outbox_lag_seconds: no data point recorded")
	}
	if lag != 0 {
		t.Errorf("outbox_lag_seconds = %d, want 0 (this is the pre-existing gauge's own blind spot: nothing was due, so it reads zero even with a real backlog)", lag)
	}

	backlog, backlogFound := readInt64Gauge(ctx, t, otelReader, "outbox_due_backlog_count")
	if !backlogFound {
		t.Fatal("outbox_due_backlog_count: no data point recorded")
	}
	if backlog != backlogSize {
		t.Errorf("outbox_due_backlog_count = %d, want %d (every 'pending' row, regardless of next_attempt_at)", backlog, backlogSize)
	}
}

// TestPumpOnce_AttemptLogsCorrelationIDAndSessionID proves attempt()'s own
// log line carries both correlation_id (Batch 11 audit-fix scope, read
// from the outbox row's own correlation_id column) and session_id (already
// logged pre-fix) -- forcing the "no notifier registered" error log line
// (deterministic, single call, exactly like
// TestPumpOnce_NoNotifierRegistered_TreatedAsFailure above) rather than the
// happy path, which emits no log line of its own to inspect.
func TestPumpOnce_AttemptLogsCorrelationIDAndSessionID(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)

	var sessionID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (spawn_source) VALUES ('slack') RETURNING id`).Scan(&sessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	const wantCorrelationID = "corr-h6-audit-fix-test"
	row, err := store.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID:     sessionID,
		Kind:          "linear",
		Payload:       []byte(`{"agent_session_id":"as1","organization_id":"org1","text":"hi","success":true}`),
		CorrelationID: strPtr(wantCorrelationID),
	})
	if err != nil {
		t.Fatalf("seed outbox entry: %v", err)
	}

	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	timeouts := platform.DefaultTimeouts()
	timeouts.OutboxBackoffBase = 1 * time.Millisecond
	timeouts.OutboxBackoffMax = 2 * time.Millisecond
	timeouts.OutboxClaimDuration = 45 * time.Millisecond

	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		// Deliberately no NotificationKindLinear entry -- forces the "no
		// notifier registered" error log line.
	}, timeouts, &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	var found bool
	for _, line := range bytes.Split(logBuf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if entry["msg"] != "outboxworker: no notifier registered for kind; recording as a failed attempt" {
			continue
		}
		found = true
		if got, _ := entry["correlation_id"].(string); got != wantCorrelationID {
			t.Errorf("log line correlation_id = %q, want %q (full entry: %v)", got, wantCorrelationID, entry)
		}
		if got, _ := entry["session_id"].(string); got != sessionID.String() {
			t.Errorf("log line session_id = %q, want %q (full entry: %v)", got, sessionID.String(), entry)
		}
		if got, _ := entry["outbox_id"].(string); got != row.ID.String() {
			t.Errorf("log line outbox_id = %q, want %q", got, row.ID.String())
		}
	}
	if !found {
		t.Fatalf("no log line with the expected msg found; full log output:\n%s", logBuf.String())
	}
}

// strPtr returns a pointer to s -- a small helper so
// sqlcgen.CreateOutboxEntryParams.CorrelationID (a *string) can be
// populated from a string literal inline above without a throwaway local
// variable at each call site.
func strPtr(s string) *string { return &s }
