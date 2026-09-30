//go:build integration

// Integration tests for technical plan §5.1's shutdown rule, against real
// Postgres: a delivery this process's own shutdown cuts short keeps its
// attempt -- recognised from the process's shutdown state, recorded on a
// context that outlives the shutdown, logged as an interruption -- within
// the bounds that keep a row on its way to dead-letter.
//
// Each interruption here is a real shutdown as the control plane runs one:
// the worker runs on platform.ShutdownState.Bind's context, and the
// notifier, mid-delivery, ends that context's parent -- the signal -- so
// the state is set and the worker's context ends after it, exactly as
// controlplane.App.Run's drain does.
package outboxworker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// repeatableKind is a kind a second delivery does not add to
// (outboxworker's repeatability table), and a PASS-THROUGH one, so no
// egress-mode read runs before its delivery.
const repeatableKind = ports.NotificationKindBlobDelete

// deliveryFunc is a ports.Notifier whose Deliver is the test's own
// function, counting its calls.
type deliveryFunc struct {
	mu    sync.Mutex
	calls int
	fn    func(ctx context.Context) error
}

func (d *deliveryFunc) Deliver(ctx context.Context, _ ports.Notification) error {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	return d.fn(ctx)
}

func (d *deliveryFunc) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// process is one run of this process: its shutdown state and the context
// its outbox worker runs on, bound to that state as the control plane
// binds it. signal ends the parent context -- SIGTERM, as the worker sees
// it.
type process struct {
	state  *platform.ShutdownState
	ctx    context.Context
	signal context.CancelFunc
}

func newProcess(t *testing.T) process {
	t.Helper()
	state := &platform.ShutdownState{}
	parent, signal := context.WithCancel(context.Background())
	ctx, stop := state.Bind(parent)
	t.Cleanup(func() {
		stop()
		signal()
	})
	return process{state: state, ctx: ctx, signal: signal}
}

// cutShort is a delivery in flight when the process receives its signal:
// it sends the signal, then waits for its own context to end, as an HTTP
// call would, and returns that context's error.
func (p process) cutShort(ctx context.Context) error {
	p.signal()
	<-ctx.Done()
	return ctx.Err()
}

// shutDown sends the signal and returns once the worker's context has
// ended -- by which time the shutdown state is set.
func (p process) shutDown() {
	p.signal()
	<-p.ctx.Done()
}

// logCapture records every log line written through slog's default logger
// for the rest of the test.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	capture := &logCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(capture, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return capture
}

func (c *logCapture) entries(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	raw := append([]byte(nil), c.buf.Bytes()...)
	c.mu.Unlock()

	var out []map[string]any
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		out = append(out, entry)
	}
	return out
}

// requireNoErrorLogs fails on any log line at error level.
func (c *logCapture) requireNoErrorLogs(t *testing.T) {
	t.Helper()
	for _, entry := range c.entries(t) {
		if entry["level"] == "ERROR" {
			t.Fatalf("logged at error level: %v\nfull log:\n%s", entry, c.buf.String())
		}
	}
}

// requireShutdownWarning fails unless a warning names this process's
// shutdown and the rule applied to the row.
func (c *logCapture) requireShutdownWarning(t *testing.T, rowID pgtype.UUID, rule string) {
	t.Helper()
	for _, entry := range c.entries(t) {
		msg, _ := entry["msg"].(string)
		if entry["level"] == "WARN" && strings.Contains(msg, "this process's shutdown") && entry["rule"] == rule && entry["outbox_id"] == rowID.String() {
			return
		}
	}
	t.Fatalf("no warning naming the shutdown with rule %q for row %s; full log:\n%s", rule, rowID.String(), c.buf.String())
}

func newShutdownTestBuilder(t *testing.T, pool *pgxpool.Pool, store *narvipg.OutboxStore, notifiers map[ports.NotificationKind]ports.Notifier, timeouts platform.Timeouts, state *platform.ShutdownState) *outboxworker.Builder {
	t.Helper()
	builder, err := outboxworker.NewBuilder(store, pool, notifiers, timeouts, state)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	return builder
}

// makeDue makes a handed-back row due now on the database's own clock,
// standing in for its settle delay, or its backoff, having passed.
func makeDue(t *testing.T, pool *pgxpool.Pool, id pgtype.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE outbox SET next_attempt_at = LEAST(next_attempt_at, now()) WHERE id = $1`, id); err != nil {
		t.Fatalf("make due: %v", err)
	}
}

// absorbClockSkew makes a row due on the database's own clock only if the
// worker already made it due on the host's: the host's clock may lead the
// database's by a few milliseconds, which production's next tick absorbs.
// A row due later -- settling, backing off -- stays as it is.
func absorbClockSkew(t *testing.T, pool *pgxpool.Pool, id pgtype.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE outbox SET next_attempt_at = now() WHERE id = $1 AND next_attempt_at <= now() + interval '1 second'`, id); err != nil {
		t.Fatalf("absorb clock skew: %v", err)
	}
}

func getRow(t *testing.T, store *narvipg.OutboxStore, id pgtype.UUID) sqlcgen.Outbox {
	t.Helper()
	row, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get outbox row: %v", err)
	}
	return row
}

// want is the state a row is expected in after one pump tick.
type want struct {
	attempts     int32
	interrupted  int32
	dueNow       bool // next_attempt_at already passed: handed back, never started
	settling     bool // next_attempt_at OutboxInterruptedSettleDelay away: handed back after an interruption
	backedOff    bool // next_attempt_at at least most of OutboxBackoffBase away: counted
	lastErrorHas string
}

func requireRow(t *testing.T, got sqlcgen.Outbox, w want) {
	t.Helper()
	if got.Status != sqlcgen.OutboxStatusPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}
	if got.Attempts != w.attempts {
		t.Fatalf("attempts = %d, want %d", got.Attempts, w.attempts)
	}
	if got.ConsecutiveInterruptions != w.interrupted {
		t.Fatalf("consecutive_interruptions = %d, want %d", got.ConsecutiveInterruptions, w.interrupted)
	}
	if w.dueNow && got.NextAttemptAt.Time.After(time.Now()) {
		t.Fatalf("next_attempt_at = %v, want it already due", got.NextAttemptAt.Time)
	}
	if settle := platform.DefaultTimeouts().OutboxInterruptedSettleDelay; w.settling &&
		(got.NextAttemptAt.Time.Before(time.Now().Add(settle-5*time.Second)) || got.NextAttemptAt.Time.After(time.Now().Add(settle+time.Second))) {
		t.Fatalf("next_attempt_at = %v, want it OutboxInterruptedSettleDelay (%v) away", got.NextAttemptAt.Time, settle)
	}
	if w.backedOff && got.NextAttemptAt.Time.Before(time.Now().Add(20*time.Second)) {
		t.Fatalf("next_attempt_at = %v, want it backed off by OutboxBackoffBase", got.NextAttemptAt.Time)
	}
	if w.lastErrorHas != "" && (got.LastError == nil || !strings.Contains(*got.LastError, w.lastErrorHas)) {
		t.Fatalf("last_error = %v, want it to contain %q", got.LastError, w.lastErrorHas)
	}
}

// TestShutdown_InterruptedDeliveryKeepsItsAttempt is the rule's exit: a
// delivery this process's shutdown cuts short is handed back with its
// attempt count unchanged, logs no error and a warning naming the
// shutdown, and is delivered by the next process -- its attempt then the
// first.
func TestShutdown_InterruptedDeliveryKeepsItsAttempt(t *testing.T) {
	ctx := context.Background()
	pool := IntegrationTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)
	logs := captureLogs(t)

	row := seedOutboxEntry(ctx, t, store, string(repeatableKind), map[string]any{"key": "blob-1"})

	first := newProcess(t)
	interrupted := &deliveryFunc{fn: first.cutShort}
	builder := newShutdownTestBuilder(t, pool, store, map[ports.NotificationKind]ports.Notifier{repeatableKind: interrupted}, platform.DefaultTimeouts(), first.state)
	if err := builder.PumpOnce(first.ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	if interrupted.callCount() != 1 {
		t.Fatalf("deliveries = %d, want 1", interrupted.callCount())
	}
	requireRow(t, getRow(t, store, row.ID), want{attempts: 0, interrupted: 1, settling: true, lastErrorHas: "interrupted by this process's shutdown"})
	logs.requireNoErrorLogs(t)
	logs.requireShutdownWarning(t, row.ID, "shutdown_interrupted")

	// The next process delivers it, as its first attempt, and the run of
	// interruptions ends with a completed attempt.
	makeDue(t, pool, row.ID)
	next := newProcess(t)
	delivered := &deliveryFunc{fn: func(context.Context) error { return nil }}
	builder = newShutdownTestBuilder(t, pool, store, map[ports.NotificationKind]ports.Notifier{repeatableKind: delivered}, platform.DefaultTimeouts(), next.state)
	if err := builder.PumpOnce(next.ctx); err != nil {
		t.Fatalf("PumpOnce (next process): %v", err)
	}
	got := getRow(t, store, row.ID)
	if delivered.callCount() != 1 || got.Status != sqlcgen.OutboxStatusDelivered || got.Attempts != 1 || got.ConsecutiveInterruptions != 0 {
		t.Fatalf("after the next process: deliveries %d, status %q, attempts %d, consecutive_interruptions %d; want 1, delivered, 1, 0",
			delivered.callCount(), got.Status, got.Attempts, got.ConsecutiveInterruptions)
	}
	logs.requireNoErrorLogs(t)
}

// TestShutdown_WhatCounts pins, one outcome at a time, what the rule
// leaves counted: a failure on its own merits; a cancellation that is not
// this process's shutdown; a delivery that outlived its own delivery
// timeout, even with shutdown set; and an interruption of every kind a
// second delivery would add to. And the one that does not count although
// its error is no cancellation: the state decides, never the error.
func TestShutdown_WhatCounts(t *testing.T) {
	ownFailure := errors.New("remote answered 500")

	for _, tc := range []struct {
		name string
		kind ports.NotificationKind
		// deliver runs as the notifier, given the process it runs in.
		deliver func(p process, ctx context.Context) error
		// runCtx is the context PumpOnce runs on; the process's own when nil.
		runCtx   func(p process) (context.Context, context.CancelFunc)
		timeouts func(*platform.Timeouts)
		want     want
		wantRule string // a warning naming the shutdown with this rule; "" for none
	}{
		{
			name:    "a failure on its own merits",
			kind:    repeatableKind,
			deliver: func(process, context.Context) error { return ownFailure },
			want:    want{attempts: 1, interrupted: 0, backedOff: true, lastErrorHas: "remote answered 500"},
		},
		{
			name: "a cancellation that is not this process's shutdown",
			kind: repeatableKind,
			runCtx: func(process) (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			// Filled in below: cancels the run context, not the process.
			// Counted like any failure, and recorded all the same: the
			// write goes on the tick's outcome context, not the one that
			// ended.
			want: want{attempts: 1, interrupted: 0, backedOff: true, lastErrorHas: "context canceled"},
		},
		{
			name: "shutdown read from the state, whatever the error says",
			kind: repeatableKind,
			deliver: func(p process, _ context.Context) error {
				// The shutdown begins while this delivery fails with an
				// error of its own, the worker's context still live.
				p.state.Begin()
				return ownFailure
			},
			want:     want{attempts: 0, interrupted: 1, settling: true, lastErrorHas: "interrupted by this process's shutdown"},
			wantRule: "shutdown_interrupted",
		},
		{
			name: "a delivery that outlived its own delivery timeout, shutdown set",
			kind: repeatableKind,
			deliver: func(p process, ctx context.Context) error {
				<-ctx.Done() // its own delivery timeout
				p.shutDown()
				return ctx.Err()
			},
			timeouts: func(to *platform.Timeouts) { to.OutboxDeliveryTimeout = 300 * time.Millisecond },
			want:     want{attempts: 1, interrupted: 1, backedOff: true, lastErrorHas: "shutdown_interrupted_outlived_delivery_timeout"},
			wantRule: "shutdown_interrupted_outlived_delivery_timeout",
		},
		{
			name:     "an interrupted comment (BotNotifier)",
			kind:     ports.NotificationKindGitHub,
			want:     want{attempts: 1, interrupted: 1, backedOff: true, lastErrorHas: "shutdown_interrupted_not_repeatable"},
			wantRule: "shutdown_interrupted_not_repeatable",
		},
		{
			name:     "an interrupted formal review",
			kind:     ports.NotificationKindGitHubVerdict,
			want:     want{attempts: 1, interrupted: 1, backedOff: true},
			wantRule: "shutdown_interrupted_not_repeatable",
		},
		{
			name:     "an interrupted Slack post",
			kind:     ports.NotificationKindSlack,
			want:     want{attempts: 1, interrupted: 1, backedOff: true},
			wantRule: "shutdown_interrupted_not_repeatable",
		},
		{
			name:     "an interrupted Linear activity",
			kind:     ports.NotificationKindLinearProgress,
			want:     want{attempts: 1, interrupted: 1, backedOff: true},
			wantRule: "shutdown_interrupted_not_repeatable",
		},
		{
			name:     "an interrupted preview dispatch",
			kind:     ports.NotificationKindRWXPreviewDispatch,
			want:     want{attempts: 1, interrupted: 1, backedOff: true},
			wantRule: "shutdown_interrupted_not_repeatable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := IntegrationTestPool(t)
			store := narvipg.NewOutboxStore(pool, false)
			logs := captureLogs(t)
			row := seedOutboxEntry(ctx, t, store, string(tc.kind), map[string]any{"key": "blob-1"})

			p := newProcess(t)
			runCtx := p.ctx
			deliver := tc.deliver
			if deliver == nil {
				deliver = func(p process, ctx context.Context) error { return p.cutShort(ctx) }
			}
			if tc.runCtx != nil {
				var cancelRun context.CancelFunc
				runCtx, cancelRun = tc.runCtx(p)
				t.Cleanup(cancelRun)
				deliver = func(_ process, ctx context.Context) error {
					cancelRun()
					<-ctx.Done()
					return ctx.Err()
				}
			}
			timeouts := platform.DefaultTimeouts()
			if tc.timeouts != nil {
				tc.timeouts(&timeouts)
			}
			notifier := &deliveryFunc{fn: func(ctx context.Context) error { return deliver(p, ctx) }}
			builder := newShutdownTestBuilder(t, pool, store, map[ports.NotificationKind]ports.Notifier{tc.kind: notifier}, timeouts, p.state)

			if err := builder.PumpOnce(runCtx); err != nil {
				t.Fatalf("PumpOnce: %v", err)
			}
			if notifier.callCount() != 1 {
				t.Fatalf("deliveries = %d, want 1", notifier.callCount())
			}
			requireRow(t, getRow(t, store, row.ID), tc.want)
			if tc.wantRule != "" {
				logs.requireShutdownWarning(t, row.ID, tc.wantRule)
			}
			logs.requireNoErrorLogs(t)
		})
	}
}

// TestShutdown_BoundMakesARepeatedInterruptionCount pins the bound: with
// OutboxMaxConsecutiveInterruptions at 2, the first two interruptions in a
// row keep their attempt, and the third counts.
func TestShutdown_BoundMakesARepeatedInterruptionCount(t *testing.T) {
	ctx := context.Background()
	pool := IntegrationTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)
	logs := captureLogs(t)
	timeouts := platform.DefaultTimeouts()
	timeouts.OutboxMaxConsecutiveInterruptions = 2

	row := seedOutboxEntry(ctx, t, store, string(repeatableKind), map[string]any{"key": "blob-1"})

	for i, w := range []struct {
		want want
		rule string
	}{
		{want{attempts: 0, interrupted: 1, settling: true}, "shutdown_interrupted"},
		{want{attempts: 0, interrupted: 2, settling: true}, "shutdown_interrupted"},
		{want{attempts: 1, interrupted: 3, backedOff: true, lastErrorHas: "shutdown_interrupted_past_bound"}, "shutdown_interrupted_past_bound"},
	} {
		makeDue(t, pool, row.ID)
		p := newProcess(t)
		notifier := &deliveryFunc{fn: p.cutShort}
		builder := newShutdownTestBuilder(t, pool, store, map[ports.NotificationKind]ports.Notifier{repeatableKind: notifier}, timeouts, p.state)
		if err := builder.PumpOnce(p.ctx); err != nil {
			t.Fatalf("interruption %d: PumpOnce: %v", i+1, err)
		}
		if notifier.callCount() != 1 {
			t.Fatalf("interruption %d: deliveries = %d, want 1 -- the row must be due again", i+1, notifier.callCount())
		}
		requireRow(t, getRow(t, store, row.ID), w.want)
		logs.requireShutdownWarning(t, row.ID, w.rule)
	}
	logs.requireNoErrorLogs(t)
}

// TestShutdown_CompletedAttemptResetsTheRun pins the reset: with the bound
// at 1, an interruption keeps its attempt; an attempt that then completes
// -- failed on its own merits -- resets the run, so the next interruption
// keeps its attempt again; and a delivery resets it too.
func TestShutdown_CompletedAttemptResetsTheRun(t *testing.T) {
	ctx := context.Background()
	pool := IntegrationTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)
	logs := captureLogs(t)
	timeouts := platform.DefaultTimeouts()
	timeouts.OutboxMaxConsecutiveInterruptions = 1

	row := seedOutboxEntry(ctx, t, store, string(repeatableKind), map[string]any{"key": "blob-1"})
	pump := func(step string, fn func(p process, ctx context.Context) error) {
		t.Helper()
		// Due now, whatever the previous outcome scheduled.
		if _, err := pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() WHERE id = $1`, row.ID); err != nil {
			t.Fatalf("%s: make due: %v", step, err)
		}
		p := newProcess(t)
		notifier := &deliveryFunc{fn: func(ctx context.Context) error { return fn(p, ctx) }}
		builder := newShutdownTestBuilder(t, pool, store, map[ports.NotificationKind]ports.Notifier{repeatableKind: notifier}, timeouts, p.state)
		if err := builder.PumpOnce(p.ctx); err != nil {
			t.Fatalf("%s: PumpOnce: %v", step, err)
		}
		if notifier.callCount() != 1 {
			t.Fatalf("%s: deliveries = %d, want 1", step, notifier.callCount())
		}
	}
	interrupt := func(p process, ctx context.Context) error { return p.cutShort(ctx) }

	pump("first interruption", interrupt)
	requireRow(t, getRow(t, store, row.ID), want{attempts: 0, interrupted: 1, settling: true})

	pump("a completed, failed attempt", func(process, context.Context) error { return errors.New("remote answered 500") })
	requireRow(t, getRow(t, store, row.ID), want{attempts: 1, interrupted: 0, backedOff: true})

	pump("second interruption", interrupt)
	requireRow(t, getRow(t, store, row.ID), want{attempts: 1, interrupted: 1, settling: true})
	logs.requireShutdownWarning(t, row.ID, "shutdown_interrupted")

	pump("delivered", func(process, context.Context) error { return nil })
	got := getRow(t, store, row.ID)
	if got.Status != sqlcgen.OutboxStatusDelivered || got.ConsecutiveInterruptions != 0 {
		t.Fatalf("after delivery: status %q, consecutive_interruptions %d; want delivered, 0", got.Status, got.ConsecutiveInterruptions)
	}
	logs.requireNoErrorLogs(t)
}

// TestShutdown_RestOfTheBatchKeepsItsAttempts pins what the shutdown does
// to the rows claimed with the interrupted one: none of them is delivered,
// each is handed back with its attempt and its run as they were -- a kind a
// second delivery would add to included, and a kind with no notifier,
// since nothing was sent -- and nothing is logged at error level. And a
// delivery that succeeds as the shutdown begins is recorded as delivered,
// not left to be delivered again once its claim lapses.
func TestShutdown_RestOfTheBatchKeepsItsAttempts(t *testing.T) {
	for _, tc := range []struct {
		name string
		// begin is how the shutdown reaches the worker while the first
		// row's delivery is in flight.
		begin func(p process)
	}{
		// The signal: the state is set, then the worker's context ends.
		{name: "signalled", begin: process.shutDown},
		// The state set, the worker's context not yet ended: the state
		// alone decides, never the context.
		{name: "state set, context still live", begin: func(p process) { p.state.Begin() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := IntegrationTestPool(t)
			store := narvipg.NewOutboxStore(pool, false)
			logs := captureLogs(t)

			first := seedOutboxEntry(ctx, t, store, string(repeatableKind), map[string]any{"key": "blob-1"})
			var rest []sqlcgen.Outbox
			for i := range 3 {
				rest = append(rest, seedOutboxEntry(ctx, t, store, string(ports.NotificationKindGitHub), map[string]any{"text": fmt.Sprintf("comment %d", i)}))
			}
			rest = append(rest, seedOutboxEntry(ctx, t, store, "kind_with_no_notifier", map[string]any{}))
			// Claimed in this order: the first row's delivery is the one in flight.
			if _, err := pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() - interval '1 minute' WHERE id = $1`, first.ID); err != nil {
				t.Fatalf("order the batch: %v", err)
			}
			if _, err := pool.Exec(ctx, `UPDATE outbox SET consecutive_interruptions = 2, last_error = 'earlier' WHERE id = $1`, rest[0].ID); err != nil {
				t.Fatalf("give a row a run: %v", err)
			}

			p := newProcess(t)
			// The delivery in flight succeeds as the shutdown begins.
			succeeds := &deliveryFunc{fn: func(context.Context) error {
				tc.begin(p)
				return nil
			}}
			never := &deliveryFunc{fn: func(context.Context) error { return errors.New("must not be delivered") }}
			builder := newShutdownTestBuilder(t, pool, store, map[ports.NotificationKind]ports.Notifier{
				repeatableKind:               succeeds,
				ports.NotificationKindGitHub: never,
			}, platform.DefaultTimeouts(), p.state)
			if err := builder.PumpOnce(p.ctx); err != nil {
				t.Fatalf("PumpOnce: %v", err)
			}

			if got := getRow(t, store, first.ID); got.Status != sqlcgen.OutboxStatusDelivered {
				t.Fatalf("the delivery that succeeded: status %q, want delivered", got.Status)
			}
			if never.callCount() != 0 {
				t.Fatalf("rows delivered after the shutdown began = %d, want 0", never.callCount())
			}
			requireRow(t, getRow(t, store, rest[0].ID), want{attempts: 0, interrupted: 2, dueNow: true, lastErrorHas: "earlier"})
			for _, r := range rest[1:] {
				got := getRow(t, store, r.ID)
				requireRow(t, got, want{attempts: 0, interrupted: 0, dueNow: true})
				if got.LastError != nil {
					t.Fatalf("last_error = %q, want none: nothing was attempted", *got.LastError)
				}
				logs.requireShutdownWarning(t, r.ID, "shutdown_before_start")
			}
			logs.requireNoErrorLogs(t)
		})
	}
}

// TestShutdown_LedgerDeliveryResetsTheRun pins the reset on the other
// terminal write an attempt can complete with: a row delivered to the
// shadow ledger instead of the world ends its run of interruptions too.
func TestShutdown_LedgerDeliveryResetsTheRun(t *testing.T) {
	ctx := context.Background()
	pool := IntegrationTestPool(t)
	// Shadow deployment-wide: a row with no session resolves suppressed.
	store := narvipg.NewOutboxStore(pool, true)
	logs := captureLogs(t)

	row := seedOutboxEntry(ctx, t, store, string(ports.NotificationKindSlack), map[string]any{"text": "hi"})
	if _, err := pool.Exec(ctx, `UPDATE outbox SET consecutive_interruptions = 2 WHERE id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}

	p := newProcess(t)
	never := &deliveryFunc{fn: func(context.Context) error { return errors.New("must not be delivered") }}
	builder := newShutdownTestBuilder(t, pool, store, map[ports.NotificationKind]ports.Notifier{ports.NotificationKindSlack: never}, platform.DefaultTimeouts(), p.state)
	if err := builder.PumpOnce(p.ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	got := getRow(t, store, row.ID)
	if never.callCount() != 0 || got.Status != sqlcgen.OutboxStatusDelivered || !got.DeliveredToLedger || got.ConsecutiveInterruptions != 0 {
		t.Fatalf("deliveries %d, status %q, to ledger %v, consecutive_interruptions %d; want 0, delivered, true, 0",
			never.callCount(), got.Status, got.DeliveredToLedger, got.ConsecutiveInterruptions)
	}
	logs.requireNoErrorLogs(t)
}

// TestOutboxStoreDefer pins the write the deferred class records through:
// it gives the claim's attempt back only while the row's next_attempt_at is
// still the one the caller observed -- never once another builder has
// claimed it since, whose own attempt it would take back -- never below
// zero, and keeps the row's last error when given none.
func TestOutboxStoreDefer(t *testing.T) {
	ctx := context.Background()
	pool := IntegrationTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)
	due := pgtype.Timestamptz{Time: time.Now(), Valid: true}

	for _, tc := range []struct {
		name         string
		claims       int
		stale        bool // pass a next_attempt_at another claim has since replaced
		lastError    *string
		wantErr      bool
		wantAttempts int32
		wantLastErr  string
	}{
		{name: "gives the claim's attempt back", claims: 1, lastError: strPtr("interrupted"), wantAttempts: 0, wantLastErr: "interrupted"},
		{name: "refused once another claim replaced the observed time", claims: 2, stale: true, lastError: strPtr("interrupted"), wantErr: true, wantAttempts: 2, wantLastErr: "earlier"},
		{name: "never below zero", claims: 0, lastError: strPtr("interrupted"), wantAttempts: 0, wantLastErr: "interrupted"},
		{name: "keeps the last error when given none", claims: 1, wantAttempts: 0, wantLastErr: "earlier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := seedOutboxEntry(ctx, t, store, string(repeatableKind), map[string]any{"key": "k"})
			if _, err := pool.Exec(ctx, `UPDATE outbox SET last_error = 'earlier' WHERE id = $1`, row.ID); err != nil {
				t.Fatal(err)
			}
			observed := getRow(t, store, row.ID).NextAttemptAt
			var claimedAt pgtype.Timestamptz
			for i := range tc.claims {
				claimedAt = pgtype.Timestamptz{Time: time.Now().Add(time.Duration(i+1) * time.Minute), Valid: true}
				claimed, err := store.Claim(ctx, row.ID, claimedAt)
				if err != nil {
					t.Fatalf("claim: %v", err)
				}
				if i == 0 {
					observed = claimed.NextAttemptAt
				}
			}
			if !tc.stale && tc.claims > 0 {
				observed = getRow(t, store, row.ID).NextAttemptAt
			}

			_, err := store.Defer(ctx, row.ID, due, observed, tc.lastError, 1)
			if tc.wantErr != (err != nil) {
				t.Fatalf("Defer error = %v, want an error: %v", err, tc.wantErr)
			}
			got := getRow(t, store, row.ID)
			if got.Attempts != tc.wantAttempts || got.LastError == nil || *got.LastError != tc.wantLastErr {
				t.Fatalf("attempts %d, last_error %v; want %d, %q", got.Attempts, got.LastError, tc.wantAttempts, tc.wantLastErr)
			}
		})
	}
}
