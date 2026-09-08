package outboxworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	domainoutbox "github.com/narvidev/narvi/internal/domain/outbox"
	"github.com/narvidev/narvi/internal/platform"
)

// meterName is this package's own OTel meter name -- mirrors app/
// imagebuild's/app/reconciler's own "narvi/<package>" convention exactly.
const meterName = "narvi/outboxworker"

// pumpBatchSize bounds how many due outbox rows a single tick claims -- a
// plain count, not a duration, so (mirroring imagebuild.pumpBatchSize's own
// identical precedent) it is a Go constant rather than a platform.Timeouts
// field. Matches imagebuild.pumpBatchSize's own value: a real outbound
// notifier call is a genuine network operation, not free, so a bounded
// batch keeps one tick's own wall-clock time bounded.
const pumpBatchSize = 20

// Builder is the process-wide background outbox-delivery loop (see doc.go
// for the full writeup). Constructed once per process (NewBuilder), then
// run via its own Run method -- exactly like app/imagebuild.Builder and
// app/reconciler.Reconciler.
type Builder struct {
	store     *postgres.OutboxStore
	pool      *pgxpool.Pool
	notifiers map[ports.NotificationKind]ports.Notifier
	timeouts  platform.Timeouts

	outboxLag        metric.Int64Gauge
	outboxDueBacklog metric.Int64Gauge
	deadLetterCount  metric.Int64Counter
}

// NewBuilder builds a Builder backed by store/pool (pool is needed
// directly, alongside store, for the claim step's own transaction --
// mirrors app/imagebuild.NewBuilder's own identical reasoning), notifiers
// (the kind->Notifier routing map this Builder's own attempt step
// consults -- see this package's own doc.go), and timeouts (for
// OutboxPumpInterval/OutboxClaimDuration/OutboxDeliveryTimeout/backoff
// config, consulted by Run/PumpOnce).
//
// The outbox_lag gauge, the outbox_due_backlog gauge, and the
// outbox_dead_letter counter are constructed exactly once, here, at
// construction time -- not per-tick, not per-row -- mirroring
// app/imagebuild.NewBuilder's own image_build_failure_streak precedent
// exactly.
func NewBuilder(store *postgres.OutboxStore, pool *pgxpool.Pool, notifiers map[ports.NotificationKind]ports.Notifier, timeouts platform.Timeouts) (*Builder, error) {
	// §30.2's own outbox seam: refuse to start rather than let a
	// registered-but-unclassified kind reach attempt() with no way to
	// decide whether §30.8's suppress-wins check applies to it -- see
	// classification.go's own top comment for why this check belongs
	// HERE, on the finished map NewBuilder receives, rather than at
	// main.go's own wiring line.
	if err := classifyNotifiers(notifiers); err != nil {
		return nil, err
	}

	meter := otel.Meter(meterName)

	outboxLag, err := meter.Int64Gauge(
		"outbox_lag_seconds",
		metric.WithDescription("Age, in seconds, of the oldest still-due outbox row claimed at the start of the most recent pump tick (§5.3: outbox lag) -- zero when nothing was due."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("outboxworker: construct outbox_lag_seconds gauge: %w", err)
	}

	// Audit fix (M15/M17, the lag-metric blind spot): outbox_lag_seconds
	// above is computed ONLY from rows THIS tick's own claimBatch actually
	// claimed -- during a sustained notifier outage, every currently-
	// pending row can be mid-backoff (not yet due) at once, silently
	// reading that gauge as zero even with a large, genuinely stuck
	// backlog. This SECOND, independent gauge counts EVERY 'pending' row
	// each tick (CountPendingOutboxEntries), deliberately not restricted
	// to rows due now, so a real backlog is always visible regardless of
	// backoff timing.
	outboxDueBacklog, err := meter.Int64Gauge(
		"outbox_due_backlog_count",
		metric.WithDescription("Total count of 'pending' outbox rows, INCLUDING rows still mid-backoff (next_attempt_at in the future) -- unlike outbox_lag_seconds, which only reflects rows actually claimed this tick, this gauge stays visible during a sustained outage where every pending row is currently cooling down."),
		metric.WithUnit("{entry}"),
	)
	if err != nil {
		return nil, fmt.Errorf("outboxworker: construct outbox_due_backlog_count gauge: %w", err)
	}

	deadLetterCount, err := meter.Int64Counter(
		"outbox_dead_letter_total",
		metric.WithDescription("Number of outbox entries this Builder has dead-lettered after exhausting domain/outbox.MaxAttempts delivery attempts."),
		metric.WithUnit("{entry}"),
	)
	if err != nil {
		return nil, fmt.Errorf("outboxworker: construct outbox_dead_letter_total counter: %w", err)
	}

	return &Builder{
		store:            store,
		pool:             pool,
		notifiers:        notifiers,
		timeouts:         timeouts,
		outboxLag:        outboxLag,
		outboxDueBacklog: outboxDueBacklog,
		deadLetterCount:  deadLetterCount,
	}, nil
}

// HasNotifier reports whether kind has a notifier registered in this
// Builder's own notifiers map -- the SAME map attempt() consults to
// decide between actually delivering a row and dead-lettering it with "no
// notifier registered for kind" (classification.go). Exists for
// composition-root tests (controlplane's own Build, which wires this map
// from cfg.IngressEnabled/cfg.RWXAccessToken/etc.) to assert directly on
// what got registered, rather than on a proxy like the route table: a
// notifier can be missing or wrongly present with the route table
// unaffected either way, since routes and outbox notifiers are two
// separate registrations gated by (usually, but not provably always) the
// same condition.
func (b *Builder) HasNotifier(kind ports.NotificationKind) bool {
	_, ok := b.notifiers[kind]
	return ok
}

// Run runs the process-wide outbox-delivery loop until ctx is done --
// mirrors app/imagebuild.Builder.Run/app/reconciler.Reconciler.Run
// exactly: a ticker on platform.Timeouts.OutboxPumpInterval, calling
// PumpOnce each tick, logging (never propagating) any per-tick error so
// one bad tick never kills the whole loop. The caller starts this via its
// own errgroup.Go exactly once per process.
func (b *Builder) Run(ctx context.Context) error {
	ticker := time.NewTicker(b.timeouts.OutboxPumpInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := b.PumpOnce(ctx); err != nil {
				platform.Logger(ctx).Error("outboxworker: tick failed", "error", err)
			}
		}
	}
}

// PumpOnce runs exactly one pump tick: claims a batch of due outbox rows,
// records the outbox_lag_seconds gauge for this tick, records the
// outbox_due_backlog_count gauge (a genuine, real-time count of every
// 'pending' row, independent of what this tick's own claim step actually
// claimed -- audit fix M15/M17), then attempts delivery for each claimed
// row, OUTSIDE any transaction. Exported (rather than only reachable
// through Run's own loop) so tests can drive exactly one tick
// deterministically, matching imagebuild.Builder.PumpOnce's own
// precedent.
//
// A failure in the batch-level claim step aborts the tick and returns the
// error (Run logs it) -- but once a batch is successfully claimed, one
// row's own delivery failure (or a failure recording its outcome) is
// isolated: logged, and does NOT abort the rest of the batch, exactly like
// imagebuild.Builder.PumpOnce's own per-row isolation. The backlog-count
// query below is likewise isolated: a failure there is logged, not
// propagated -- it is a cheap, standalone observability read, never
// allowed to abort a tick's own real claim/deliver work.
func (b *Builder) PumpOnce(ctx context.Context) error {
	claimed, oldestCreatedAt, err := b.claimBatch(ctx)
	if err != nil {
		return fmt.Errorf("outboxworker: claim batch: %w", err)
	}

	lag := int64(0)
	if !oldestCreatedAt.IsZero() {
		lag = int64(time.Since(oldestCreatedAt).Seconds())
		if lag < 0 {
			lag = 0
		}
	}
	b.outboxLag.Record(ctx, lag)

	if backlog, err := b.store.CountPending(ctx); err != nil {
		platform.Logger(ctx).Error("outboxworker: count pending outbox backlog failed", "error", err)
	} else {
		b.outboxDueBacklog.Record(ctx, backlog)
	}

	for _, row := range claimed {
		b.attempt(ctx, row)
	}
	return nil
}

// claimBatch runs the ENTIRE claim step inside one transaction:
// ListDuePending (FOR UPDATE SKIP LOCKED -- so a concurrent tick, this
// pod's or another pod's own Builder, claims a DISJOINT batch rather than
// double-claiming the same row), then Claim for each due row (bumps
// next_attempt_at forward by OutboxClaimDuration, increments attempts),
// then commits -- exactly mirroring imagebuild.Builder.claimBatch's own
// shape. Also returns the oldest CreatedAt among the due rows (the zero
// time.Time if none were due), so PumpOnce can record the outbox_lag_seconds
// gauge for this tick.
//
// The single now := time.Now() below is shared across every row in this
// batch (up to pumpBatchSize), so every claimed row's own next_attempt_at
// is stamped with the SAME provisional expiry, well before any of them
// are actually delivered -- PumpOnce attempts each claimed row
// SEQUENTIALLY, one at a time, so a row late in the batch would otherwise
// have its own claim-lease elapse before its own attempt() call even
// starts (audit fix H6). attempt() itself closes that gap with its own
// per-row RenewOutboxClaim heartbeat, taken from a FRESH time.Now()
// immediately before the real delivery call -- this batch-level claim
// below only ever needs to survive up to the moment attempt() runs for
// that row, not the whole batch's own worst-case sequential duration.
func (b *Builder) claimBatch(ctx context.Context) ([]sqlcgen.Outbox, time.Time, error) {
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	txStore := b.store.WithTx(tx)

	due, err := txStore.ListDuePending(ctx, pumpBatchSize)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("list due outbox entries: %w", err)
	}

	var oldestCreatedAt time.Time
	now := time.Now()
	claimed := make([]sqlcgen.Outbox, 0, len(due))
	for _, row := range due {
		if row.CreatedAt.Valid && (oldestCreatedAt.IsZero() || row.CreatedAt.Time.Before(oldestCreatedAt)) {
			oldestCreatedAt = row.CreatedAt.Time
		}

		c, err := txStore.Claim(ctx, row.ID, pgtype.Timestamptz{Time: now.Add(b.timeouts.OutboxClaimDuration), Valid: true})
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("claim outbox entry %s: %w", row.ID.String(), err)
		}
		claimed = append(claimed, c)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, time.Time{}, fmt.Errorf("commit: %w", err)
	}
	return claimed, oldestCreatedAt, nil
}

// attempt performs the real (possibly slow, network-bound) delivery for
// one already-claimed row OUTSIDE any transaction, bounded by
// OutboxDeliveryTimeout, then records the outcome via a single, pool-
// scoped statement. Every failure here (no notifier registered for this
// kind, a lost claim, Deliver error, a failed/no-op outcome-record call)
// is logged and returns -- it never propagates, so one row's problem can
// never abort the rest of PumpOnce's own batch, mirroring
// imagebuild.Builder.attempt's own identical isolation.
//
// Immediately before the real Deliver call, this renews THIS row's own
// claim-protection window (RenewOutboxClaim, from a FRESH time.Now() taken
// right here -- not claimBatch's own shared, batch-level claim-time now)
// -- audit fix H6: without this, a row late in a sequentially-processed
// batch could have its shared batch-level claim already expired by the
// time its own attempt() call even starts, letting a concurrent tick
// re-claim it.
//
// This renewal is a genuine optimistic-concurrency compare-and-swap, not
// just a status check: it passes row.NextAttemptAt (the value THIS row
// carried when claimBatch's own ClaimOutboxEntry call -- or a prior
// attempt() call's own RenewOutboxClaim -- last returned it) as the
// expected prior value, and the guarded UPDATE only succeeds if the row's
// CURRENT next_attempt_at still matches it. A "status = 'pending'" guard
// alone cannot tell "untouched since I last observed it" apart from "a
// DIFFERENT builder already re-claimed/renewed this row and is now
// mid-delivery on it" -- both leave status at 'pending' (the outbox table
// has no third, in-flight status) -- so a status-only renewal would
// spuriously succeed for BOTH builders in that scenario, and both would
// call notifier.Deliver on the same row concurrently: a genuine duplicate
// side effect (see RenewOutboxClaim's own generated doc comment,
// queries/outbox.sql, for the full mechanism and how this was
// empirically reproduced). With the CAS, whichever builder wins re-claims
// the row first changes next_attempt_at away from the value this caller
// observed, so this caller's own renewal correctly returns pgx.ErrNoRows
// instead -- handled below by skipping delivery entirely. This makes the
// renewal a real single-writer lease: at most one builder proceeds to
// notifier.Deliver for this row at a time. The renewal never increments
// attempts -- claimBatch already counted this attempt.
func (b *Builder) attempt(ctx context.Context, row sqlcgen.Outbox) {
	var correlationID string
	if row.CorrelationID != nil {
		correlationID = *row.CorrelationID
	}
	logger := platform.Logger(ctx).With(
		"outbox_id", row.ID.String(),
		"kind", row.Kind,
		"attempts", row.Attempts,
		"session_id", row.SessionID.String(),
		"correlation_id", correlationID,
	)

	notifier, ok := b.notifiers[ports.NotificationKind(row.Kind)]
	if !ok {
		logger.Error("outboxworker: no notifier registered for kind; recording as a failed attempt")
		b.recordFailure(ctx, logger, row, fmt.Sprintf("no notifier registered for kind %q", row.Kind))
		return
	}

	renewed, err := b.store.RenewClaim(ctx, row.ID,
		pgtype.Timestamptz{Time: time.Now().Add(b.timeouts.OutboxClaimDuration), Valid: true},
		row.NextAttemptAt, // CAS: only renew if next_attempt_at still matches what THIS caller last observed.
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The row is no longer 'pending', OR (the CAS's own real job)
			// its next_attempt_at no longer matches what this caller last
			// observed -- meaning a DIFFERENT builder already re-claimed
			// or renewed this row in between and may be mid-delivery on
			// it right now. Either way: skip delivery entirely rather
			// than risk a duplicate notifier.Deliver call for a row
			// whose claim has already been superseded.
			logger.Warn("outboxworker: renew claim no-op: row no longer pending or claim superseded by another builder")
			return
		}
		logger.Error("outboxworker: renew claim failed", "error", err)
		return
	}
	row = renewed

	// §30.8's own epoch discipline: "suppress if the stamp OR the current
	// flag says shadow -- monotone toward suppression, in both
	// directions." row.SuppressedInShadow is the enqueue-time half (a
	// born-shadow row is terminally shadow, and NEVER re-checked here --
	// this notifier is never even called for it, whatever repo_settings
	// says by now). A born-LIVE row gets ONE further check, right here,
	// at the true moment of delivery: has this row's own repo been
	// demoted since it was enqueued? That is the delivery-time half
	// suppress-wins needs, and it applies ONLY to §30.2's classified
	// SUPPRESS kinds -- a PASS-THROUGH kind (blob_delete, sentinel_auto_fix,
	// linear_digest) always reaches notifier.Deliver unconditionally, and
	// therefore carries its own enqueue stamp on the Notification
	// (ports.Notification.SuppressedInShadow): a pass-through notifier
	// with a shadow-suppressible effect must still honour the
	// born-shadow half of this rule, which this block never applies for
	// it.
	// classifyNotifiers (NewBuilder) already guarantees row.Kind, having
	// resolved a real notifier above, also has an entry here.
	if notificationKindClassification[ports.NotificationKind(row.Kind)] == ClassSuppress {
		suppressed := row.SuppressedInShadow
		if !suppressed {
			suppressed = b.store.ResolveEffectiveMode(ctx, row.SessionID)
		}
		if suppressed {
			b.deliverToLedger(ctx, logger, row)
			return
		}
	}

	deliverCtx, cancel := context.WithTimeout(ctx, b.timeouts.OutboxDeliveryTimeout)
	defer cancel()

	if err := notifier.Deliver(deliverCtx, ports.Notification{
		Kind:    ports.NotificationKind(row.Kind),
		Payload: row.Payload,
		// Carried for the PASS-THROUGH kinds, which reach here without
		// the stamp check above ever having run for them.
		SuppressedInShadow: row.SuppressedInShadow,
	}); err != nil {
		logger.Warn("outboxworker: Deliver failed", "error", err)
		b.recordFailure(ctx, logger, row, err.Error())
		return
	}

	if _, err := b.store.MarkDelivered(ctx, row.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The row is no longer 'pending' -- a should-be-rare, benign
			// race (e.g. a bug elsewhere ever double-claims a row); logged,
			// not fatal to this tick.
			logger.Warn("outboxworker: mark delivered no-op: row no longer pending")
			return
		}
		logger.Error("outboxworker: mark delivered failed", "error", err)
	}
}

// deliverToLedger records §30.6/§30.8's own terminal mark: row's own
// effective egress mode was shadow at this exact moment, so this
// attempt delivers it into the suppression ledger instead of the world
// -- notifier.Deliver is never called. The outbox row ITSELF is the
// record (§30.6: "the row already carries the full payload... it IS the
// record"), so unlike a suppressed direct SCM write (shadowledger.
// Record, shadow_scm_writes), nothing further needs writing beyond this
// one terminal mark -- see MarkOutboxEntryDeliveredToLedger's own
// generated doc comment for the exact column-level effect.
func (b *Builder) deliverToLedger(ctx context.Context, logger *slog.Logger, row sqlcgen.Outbox) {
	if _, err := b.store.MarkDeliveredToLedger(ctx, row.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("outboxworker: mark delivered-to-ledger no-op: row no longer pending")
			return
		}
		logger.Error("outboxworker: mark delivered-to-ledger failed", "error", err)
		return
	}
	logger.Info("outboxworker: row suppressed in shadow -- delivered to the ledger instead of the world")
}

// recordFailure computes the next retry time (or dead-letter decision) via
// domain/outbox.EvaluateBackoff and records the failure -- shared by
// every attempt failure path above (no notifier registered, Deliver
// error).
func (b *Builder) recordFailure(ctx context.Context, logger *slog.Logger, row sqlcgen.Outbox, lastError string) {
	now := time.Now()
	decision := domainoutbox.EvaluateBackoff(int(row.Attempts), domainoutbox.BackoffConfig{
		BaseDelay: b.timeouts.OutboxBackoffBase,
		MaxDelay:  b.timeouts.OutboxBackoffMax,
	}, now)

	if decision.DeadLetter {
		// Confirmed audit finding (LOW): docs/runbooks/outbox-delivery.md's
		// own Confirm section already claims this log line "carries
		// max_attempts and the last delivery error" -- it used to carry
		// only the former. last_error (redacted -- see redact.go's own doc
		// comment) closes that gap for real, sparing an operator the extra
		// hop through ListDeadLetter/a direct DB read just to see WHY a
		// dead-lettered row gave up, for the common case of triaging from
		// logs alone.
		logger.Warn("outboxworker: outbox entry has exhausted max delivery attempts; dead-lettering",
			"max_attempts", domainoutbox.MaxAttempts,
			"last_error", redactURLCredentials(lastError),
		)
		if _, err := b.store.MarkDeadLetter(ctx, row.ID, lastError); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				logger.Warn("outboxworker: mark dead letter no-op: row no longer pending")
				return
			}
			logger.Error("outboxworker: mark dead letter failed", "error", err)
			return
		}
		b.deadLetterCount.Add(ctx, 1)
		return
	}

	if _, err := b.store.RecordFailure(ctx, row.ID, pgtype.Timestamptz{Time: decision.NextRetryAt, Valid: true}, lastError); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("outboxworker: record failure no-op: row no longer pending")
			return
		}
		logger.Error("outboxworker: record failure failed", "error", err)
	}
}
