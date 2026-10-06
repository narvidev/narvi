package outboxworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/autonomy"
	"github.com/narvidev/narvi/internal/app/ports"
	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
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

	// shutdown is this process's own shutdown state (platform.
	// ShutdownState), set by the run path when its drain begins and read
	// here to tell a delivery that shutdown cut short from one that failed
	// (§5.1) -- never inferred from a cancellation error. The process's
	// one instance, shared with every other reader.
	shutdown *platform.ShutdownState

	// gate is the autonomy freeze (technical plan §40.2), built here from
	// pool: attempt holds the kinds freeze.go marks FreezeHolds while
	// autonomy is frozen, and PumpOnce leaves them out of the lag gauge.
	gate *autonomy.Gate

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
//
// shutdown is this process's shutdown state (§5.1), required: a Builder
// with none could never tell an interruption from a failure, and would
// quietly count every delivery a deploy cuts short.
func NewBuilder(store *postgres.OutboxStore, pool *pgxpool.Pool, notifiers map[ports.NotificationKind]ports.Notifier, timeouts platform.Timeouts, shutdown *platform.ShutdownState) (*Builder, error) {
	if shutdown == nil {
		return nil, errors.New("outboxworker: refusing to start: no shutdown state (§5.1)")
	}

	// §30.2's own outbox seam: refuse to start rather than let a
	// registered-but-unclassified kind reach attempt() with no way to
	// decide whether §30.8's suppress-wins check applies to it -- see
	// classification.go's own top comment for why this check belongs
	// HERE, on the finished map NewBuilder receives, rather than at
	// main.go's own wiring line.
	if err := classifyNotifiers(notifiers); err != nil {
		return nil, err
	}
	// §5.1's shutdown rule needs the same of every registered kind: whether
	// a second delivery adds to the first (repeatability.go).
	if err := checkRepeatability(notifiers); err != nil {
		return nil, err
	}
	// §40.2's autonomy freeze needs the same of every registered kind:
	// whether its delivery is an automatic action the freeze holds
	// (freeze.go).
	if err := classifyFreeze(notifiers); err != nil {
		return nil, err
	}
	gate, err := autonomy.NewGate(pool)
	if err != nil {
		return nil, fmt.Errorf("outboxworker: %w", err)
	}

	meter := otel.Meter(meterName)

	outboxLag, err := meter.Int64Gauge(
		"outbox_lag_seconds",
		metric.WithDescription("Age, in seconds, of the oldest still-due outbox row claimed at the start of the most recent pump tick (§5.3: outbox lag) -- zero when nothing was due. While autonomy is frozen (§40.2), the rows the freeze holds are left out."),
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
		shutdown:         shutdown,
		gate:             gate,
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

// RegisteredKinds returns every kind with a notifier registered, sorted --
// HasNotifier's whole-map sibling, for the same composition-root tests: a
// relational assertion (the kinds one configuration registers minus
// another's) needs the full set, where HasNotifier can only confirm a list
// the test already wrote down.
func (b *Builder) RegisteredKinds() []ports.NotificationKind {
	kinds := make([]ports.NotificationKind, 0, len(b.notifiers))
	for kind := range b.notifiers {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

// ShutdownBegun reports whether the shutdown state this Builder reads has
// begun -- for the same composition-root tests as HasNotifier: the control
// plane's run path sets one state and this Builder must read that same
// one, or every delivery a deploy cuts short would be counted again
// (§5.1).
func (b *Builder) ShutdownBegun() bool {
	return b.shutdown.Begun()
}

// Run runs the process-wide outbox-delivery loop until ctx is done --
// mirrors app/imagebuild.Builder.Run/app/reconciler.Reconciler.Run
// exactly: a ticker on platform.Timeouts.OutboxPumpInterval, calling
// PumpOnce each tick, logging (never propagating) any per-tick error so
// one bad tick never kills the whole loop. The caller starts this via its
// own errgroup.Go exactly once per process.
//
// ctx should end only once this process's shutdown has begun -- the
// control plane runs this on platform.ShutdownState.Bind's context -- so a
// delivery it cuts short is always read as an interruption (§5.1).
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
//
// Every outcome the tick records is written on one context that outlives
// the worker's (newOutcomeContext), so a shutdown that begins while a write
// is in flight does not cancel it (§5.1). Once the shutdown has begun,
// every row of the batch not yet attempted is handed back without spending
// its attempt (attempt's own first check).
//
// The tick reads the autonomy freeze (§40.2) once, before its claim, and
// while it holds -- frozen, or a read that failed, since attempt then holds
// those kinds as well -- two things change. The kinds the freeze holds
// (freeze.go) are claimed in a lane of their own (claimBatch), so the rows
// it holds, due again every AutonomyFreezeRecheckInterval, never take the
// slots of the notifications behind them; and they are left out of
// outbox_lag_seconds, since a held row's age grows with the freeze and
// would read as a stuck outbox (OutboxLagHigh) though nothing is stuck. A
// delivering row that is due is always claimed in its own lane, so the
// gauge reads its age whenever there is one. Whether each held row is
// held is still decided, and its skip recorded, at its own attempt.
func (b *Builder) PumpOnce(ctx context.Context) error {
	frozen, freezeErr := b.gate.Frozen(ctx)
	if freezeErr != nil {
		platform.Logger(ctx).Warn("outboxworker: the autonomy freeze could not be read; the kinds it holds are claimed in their own lane this tick", "error", freezeErr)
	}
	holding := frozen || freezeErr != nil

	claimed, err := b.claimBatch(ctx, holding)
	if err != nil {
		return fmt.Errorf("outboxworker: claim batch: %w", err)
	}

	b.outboxLag.Record(ctx, lagSeconds(claimed, holding, time.Now()))

	if backlog, err := b.store.CountPending(ctx); err != nil {
		platform.Logger(ctx).Error("outboxworker: count pending outbox backlog failed", "error", err)
	} else {
		b.outboxDueBacklog.Record(ctx, backlog)
	}

	outcomeCtx, release := newOutcomeContext(ctx, b.timeouts.OutboxShutdownRecordTimeout)
	defer release()
	held := 0
	for _, row := range claimed {
		if b.attempt(ctx, outcomeCtx, row) {
			held++
		}
	}
	if held > 0 {
		platform.Logger(ctx).Info("outboxworker: deliveries held by the autonomy freeze this tick; each is due again after the recheck interval, its attempt not counted",
			"outcome", domainautonomy.OutcomeSkipped, "held", held, "recheck_interval", b.timeouts.AutonomyFreezeRecheckInterval)
	}
	return nil
}

// lagSeconds is outbox_lag_seconds for one tick: the age at now of the
// oldest row claimed, zero when none was -- leaving out, when holding, the
// rows of the kinds the autonomy freeze holds.
func lagSeconds(claimed []sqlcgen.Outbox, holding bool, now time.Time) int64 {
	var oldest time.Time
	for _, row := range claimed {
		if holding && freezeOf(ports.NotificationKind(row.Kind)).class == FreezeHolds {
			continue
		}
		if row.CreatedAt.Valid && (oldest.IsZero() || row.CreatedAt.Time.Before(oldest)) {
			oldest = row.CreatedAt.Time
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return max(int64(now.Sub(oldest).Seconds()), 0)
}

// newOutcomeContext returns the context a pump tick records outcomes on
// (§5.1), and the function that releases it once the tick is done.
//
// An outcome is known before its write starts -- a delivery succeeded, it
// failed, a claimed row goes back -- and is correct to record whatever the
// shutdown state reads by then. On the worker's own context, a shutdown
// that begins while the write is in flight cancels it, and the row, its
// attempt spent, waits for its claim to lapse: a delivery that succeeded
// is then delivered again. So this context carries the worker context's
// values and is not cancelled with it.
//
// It is bounded from the moment the worker context ends, not from its own
// creation: while the worker runs it has no deadline, as the worker
// context has none, so a write late in a long tick is never cut by a
// bound meant for the shutdown; once the worker context ends it is
// cancelled OutboxShutdownRecordTimeout later, so everything the tick
// still records after its shutdown began finishes within that one bound,
// however many rows it covers, and Validate keeps that bound below
// ShutdownGracePeriod.
//
// The bound is kept by context.AfterFunc's own goroutine, which ends at
// the bound or at release, whichever comes first.
func newOutcomeContext(worker context.Context, bound time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(worker))
	stop := context.AfterFunc(worker, func() {
		timer := time.NewTimer(bound)
		defer timer.Stop()
		select {
		case <-timer.C:
			cancel()
		case <-ctx.Done():
		}
	})
	return ctx, func() {
		stop()
		cancel()
	}
}

// claimBatch runs the ENTIRE claim step inside one transaction:
// ListDuePending (FOR UPDATE SKIP LOCKED -- so a concurrent tick, this
// pod's or another pod's own Builder, claims a DISJOINT batch rather than
// double-claiming the same row), then Claim for each due row (bumps
// next_attempt_at forward by OutboxClaimDuration, increments attempts),
// then commits -- exactly mirroring imagebuild.Builder.claimBatch's own
// shape. PumpOnce records the outbox_lag_seconds gauge for this tick from
// the claimed rows' CreatedAt.
//
// While holding (PumpOnce's read of the autonomy freeze, §40.2) the batch
// is two lanes, each up to pumpBatchSize oldest-due-first: the kinds the
// freeze does not hold, then the kinds it holds. Scheduling only: every due
// row of a held kind is still claimed in its lane and decided at its own
// attempt, so no row is passed over unrecorded, and a notification never
// waits behind rows the freeze is holding. Each lane names its kinds
// (deliveringKinds, heldKinds), so it reads only its own due rows through
// the outbox's (kind, next_attempt_at) index. A row of a kind this binary
// does not declare -- one a newer binary enqueued -- is in neither lane, so
// it waits while the freeze holds; it is one this binary could not deliver
// anyway, and the single batch claims it again once the freeze lifts.
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
func (b *Builder) claimBatch(ctx context.Context, holding bool) ([]sqlcgen.Outbox, error) {
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	txStore := b.store.WithTx(tx)

	var due []sqlcgen.Outbox
	if !holding {
		due, err = txStore.ListDuePending(ctx, pumpBatchSize)
		if err != nil {
			return nil, fmt.Errorf("list due outbox entries: %w", err)
		}
	} else {
		delivering, err := txStore.ListDuePendingOfKinds(ctx, pumpBatchSize, deliveringKinds())
		if err != nil {
			return nil, fmt.Errorf("list due outbox entries of the delivering kinds: %w", err)
		}
		held, err := txStore.ListDuePendingOfKinds(ctx, pumpBatchSize, heldKinds())
		if err != nil {
			return nil, fmt.Errorf("list due outbox entries of the held kinds: %w", err)
		}
		due = make([]sqlcgen.Outbox, 0, len(delivering)+len(held))
		due = append(due, delivering...)
		due = append(due, held...)
	}

	now := time.Now()
	claimed := make([]sqlcgen.Outbox, 0, len(due))
	for _, row := range due {
		c, err := txStore.Claim(ctx, row.ID, pgtype.Timestamptz{Time: now.Add(b.timeouts.OutboxClaimDuration), Valid: true})
		if err != nil {
			return nil, fmt.Errorf("claim outbox entry %s: %w", row.ID.String(), err)
		}
		claimed = append(claimed, c)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return claimed, nil
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
//
// This process's own shutdown (§5.1) is read from its shutdown state,
// never inferred from an error, at two points. First, before anything
// else: once shutdown has begun, no delivery starts, and the row -- the
// rest of a batch the shutdown reached -- is handed back with its
// attempt, whatever its kind, since nothing was sent. Then once Deliver
// has returned: domain/outbox.EvaluateFailure decides from that one
// reading whether the shutdown cut the delivery short in a way that keeps
// the attempt. Every outcome -- a failure, a success, a row handed back --
// is written on outcomeCtx (newOutcomeContext), never on ctx, so a
// shutdown that begins after either reading does not cancel the write.
//
// The autonomy freeze (§40.2) is read once the claim is renewed, for the
// kinds freeze.go marks FreezeHolds, before the shadow check: a held row
// reaches neither the world nor the suppression ledger until the freeze
// lifts (holdFrozen). A row stamped suppressed_in_shadow at enqueue is
// never held: by §30.8 it can only end in the ledger, so its delivery
// starts nothing. attempt returns true for a row it held.
func (b *Builder) attempt(ctx, outcomeCtx context.Context, row sqlcgen.Outbox) (held bool) {
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

	// The first reading: once shutdown has begun, the row is handed back
	// untouched but for its attempt -- nothing was sent, whatever the kind.
	if b.shutdown.Begun() {
		b.recordFailure(outcomeCtx, logger, row, domainoutbox.Failure{
			AttemptCount:             int(row.Attempts),
			ConsecutiveInterruptions: int(row.ConsecutiveInterruptions),
			ShutdownBegun:            true,
			NotStarted:               true,
		}, "")
		return false
	}

	notifier, ok := b.notifiers[ports.NotificationKind(row.Kind)]
	if !ok {
		logger.Error("outboxworker: no notifier registered for kind; recording as a failed attempt")
		b.recordFailure(outcomeCtx, logger, row, domainoutbox.Failure{
			AttemptCount:             int(row.Attempts),
			ConsecutiveInterruptions: int(row.ConsecutiveInterruptions),
		}, fmt.Sprintf("no notifier registered for kind %q", row.Kind))
		return false
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
			return false
		}
		logger.Error("outboxworker: renew claim failed", "error", err)
		return false
	}
	row = renewed

	// §40.2: a kind whose delivery is itself an automatic action is held
	// while autonomy is frozen, before the shadow check below -- a held row
	// is not delivered to the ledger either. A freeze read that fails holds
	// it too.
	//
	// Except a row born in shadow: §30.8 makes it terminally shadow, so its
	// delivery can only record what would have happened -- the shadow block
	// below sends a SUPPRESS kind to the ledger, and the sentinel auto-fix's
	// own Deliver short-circuits to the ledger on the stamp before it claims
	// or spawns anything. It starts no action, so the freeze has nothing to
	// hold; holding it would only keep it pending, and a person's
	// shadow-to-live Activate waits for every such row to settle
	// (shadowoperator.ErrUnhandledShadowEraRows). Only the stamp is trusted
	// here, never the current mode: a born-live row whose repository has
	// been demoted since may still find it live again before it delivers.
	if entry := freezeOf(ports.NotificationKind(row.Kind)); entry.class == FreezeHolds && !row.SuppressedInShadow {
		if skip, reason := b.gate.Check(ctx, entry.site, "outbox_id", row.ID.String(), "kind", row.Kind); skip {
			b.holdFrozen(outcomeCtx, logger, row, reason)
			return true
		}
	}

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
			return false
		}
	}

	deliverCtx, cancel := context.WithTimeout(ctx, b.timeouts.OutboxDeliveryTimeout)
	defer cancel()

	started := time.Now()
	err = notifier.Deliver(deliverCtx, ports.Notification{
		Kind:    ports.NotificationKind(row.Kind),
		Payload: row.Payload,
		// Carried for the PASS-THROUGH kinds, which reach here without
		// the stamp check above ever having run for them.
		SuppressedInShadow: row.SuppressedInShadow,
	})
	elapsed := time.Since(started)
	if err != nil {
		// The second reading, the one this failure is decided by.
		begun := b.shutdown.Begun()
		if !begun {
			logger.Warn("outboxworker: Deliver failed", "error", err)
		}
		b.recordFailure(outcomeCtx, logger, row, domainoutbox.Failure{
			AttemptCount:             int(row.Attempts),
			ConsecutiveInterruptions: int(row.ConsecutiveInterruptions),
			ShutdownBegun:            begun,
			OutlivedDeliveryTimeout:  elapsed >= b.timeouts.OutboxDeliveryTimeout,
			Repeatable:               repeatabilityOf(ports.NotificationKind(row.Kind)) == Repeatable,
		}, err.Error())
		return false
	}

	// A delivery that succeeded is recorded as delivered whenever the
	// shutdown begins, before this write or during it: left pending, it
	// would be delivered again once its claim lapsed.
	if _, err := b.store.MarkDelivered(outcomeCtx, row.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The row is no longer 'pending' -- a should-be-rare, benign
			// race (e.g. a bug elsewhere ever double-claims a row); logged,
			// not fatal to this tick.
			logger.Warn("outboxworker: mark delivered no-op: row no longer pending")
			return false
		}
		logger.Error("outboxworker: mark delivered failed", "error", err)
	}
	return false
}

// holdFrozen records a delivery the autonomy freeze held (§40.2): the row
// stays pending, its attempt -- which the claim counted -- is given back
// (DeferOutboxEntry, a compare-and-swap on the renewed claim, so it never
// takes back an attempt another builder's claim counted), its run of
// shutdown interruptions is left as it was, and it is due again
// AutonomyFreezeRecheckInterval from now. last_error says it was skipped,
// and why; attempts not moving is what tells it from a failure. Nothing
// else is written: no branch, no session, no rewrite, no dead letter.
// Written on the tick's outcome context, like every outcome.
func (b *Builder) holdFrozen(ctx context.Context, logger *slog.Logger, row sqlcgen.Outbox, reason domainautonomy.SkipReason) {
	lastError := fmt.Sprintf("skipped (%s): the autonomy freeze holds this delivery; attempt not counted", reason)
	if _, err := b.store.Defer(ctx, row.ID,
		pgtype.Timestamptz{Time: time.Now().Add(b.timeouts.AutonomyFreezeRecheckInterval), Valid: true},
		row.NextAttemptAt, // CAS: the claim this attempt renewed.
		&lastError, row.ConsecutiveInterruptions,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("outboxworker: hold for the autonomy freeze no-op: row no longer pending or claimed by another builder since")
			return
		}
		logger.Error("outboxworker: hold for the autonomy freeze failed", "error", err)
		return
	}
	logger.Debug("outboxworker: delivery held by the autonomy freeze", "outcome", domainautonomy.OutcomeSkipped, "reason", string(reason))
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
//
// Written on the worker's own context, never the outcome one: a mode read
// the shutdown cut short resolves shadow, fail-closed, and a mark written
// past the shutdown would record that as a real suppression.
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

// failurePolicy is domain/outbox.EvaluateFailure's configuration, from
// this Builder's timeouts.
func (b *Builder) failurePolicy() domainoutbox.Policy {
	return domainoutbox.Policy{
		Backoff: domainoutbox.BackoffConfig{
			BaseDelay: b.timeouts.OutboxBackoffBase,
			MaxDelay:  b.timeouts.OutboxBackoffMax,
		},
		MaxConsecutiveInterruptions: b.timeouts.OutboxMaxConsecutiveInterruptions,
		InterruptedSettleDelay:      b.timeouts.OutboxInterruptedSettleDelay,
	}
}

// recordFailure records one failed attempt as domain/outbox.EvaluateFailure
// decides it -- the one path every failure takes (no notifier registered, a
// Deliver error, a row the shutdown reached before its delivery started):
// ClassDeferred gives the attempt back (Defer), ClassCounted keeps it and
// reschedules (RecordFailure) or dead-letters (MarkDeadLetter). Every write
// carries the run of shutdown interruptions the decision names. ctx is the
// context to write on: the tick's outcome context, which outlives the
// worker's.
//
// A failure the shutdown caused is logged at warning level, naming the
// shutdown and the rule applied, never at error level: a deploy is not an
// outage. The caller logs an ordinary failure's own line itself.
func (b *Builder) recordFailure(ctx context.Context, logger *slog.Logger, row sqlcgen.Outbox, f domainoutbox.Failure, lastError string) {
	decision := domainoutbox.EvaluateFailure(f, b.failurePolicy(), time.Now())
	logger = logger.With("rule", string(decision.Rule), "consecutive_interruptions", decision.ConsecutiveInterruptions)

	if decision.Class == domainoutbox.ClassDeferred {
		var recorded *string
		switch decision.Rule {
		case domainoutbox.RuleShutdownBeforeStart:
			logger.Warn("outboxworker: this process's shutdown began before this row's delivery started; returned to the queue without spending an attempt")
		default:
			logger.Warn("outboxworker: delivery interrupted by this process's shutdown; returned to the queue without spending an attempt",
				"max_consecutive_interruptions", b.timeouts.OutboxMaxConsecutiveInterruptions,
				"error", redactURLCredentials(lastError),
			)
			interrupted := "interrupted by this process's shutdown, attempt not counted: " + lastError
			recorded = &interrupted
		}
		if _, err := b.store.Defer(ctx, row.ID,
			pgtype.Timestamptz{Time: decision.NextRetryAt, Valid: true},
			row.NextAttemptAt, // CAS: never take back an attempt another builder's claim counted.
			recorded, int32(decision.ConsecutiveInterruptions),
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				logger.Warn("outboxworker: return to queue no-op: row no longer pending or claimed by another builder since")
				return
			}
			logger.Error("outboxworker: return to queue failed", "error", err)
		}
		return
	}

	if decision.Rule != domainoutbox.RuleFailed {
		logger.Warn("outboxworker: delivery interrupted by this process's shutdown; counted as a failed attempt",
			"max_consecutive_interruptions", b.timeouts.OutboxMaxConsecutiveInterruptions,
			"error", redactURLCredentials(lastError),
		)
		lastError = "interrupted by this process's shutdown, counted (" + string(decision.Rule) + "): " + lastError
	}

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
		if _, err := b.store.MarkDeadLetter(ctx, row.ID, lastError, int32(decision.ConsecutiveInterruptions)); err != nil {
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

	if _, err := b.store.RecordFailure(ctx, row.ID, pgtype.Timestamptz{Time: decision.NextRetryAt, Valid: true}, lastError, int32(decision.ConsecutiveInterruptions)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("outboxworker: record failure no-op: row no longer pending")
			return
		}
		logger.Error("outboxworker: record failure failed", "error", err)
	}
}
