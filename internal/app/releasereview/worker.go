// This file (worker.go) implements blocking-finding fix #1's own SECOND
// phase: Worker is the process-wide background loop that claims
// release_manifest_pending rows (Enqueue's own durable hand-off,
// enqueue.go) and runs the actual manifest check (Run, run.go) for each
// -- entirely decoupled from any webhook request's own context/lifetime.
// See this package's own top doc comment and migrations/
// 000050_release_manifest_pending.up.sql's own doc comment for the full
// "why".
//
// Mirrors internal/app/outboxworker.Builder/app/imagebuild.Builder/
// app/reconciler.Reconciler's own identical "ticker -> claim batch ->
// process each claimed row outside any transaction" shape -- its OWN
// small package/loop (never folded into outboxworker itself), because
// Run's own worst-case per-item processing time (minutes) is utterly
// incompatible with outboxworker's own tuned-for-real-time-notifications
// OutboxDeliveryTimeout (15s) and its own strictly sequential per-batch
// processing -- see the migration's own doc comment for the full
// reasoning this fix rests on.

package releasereview

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// pendingBatchSize bounds how many release_manifest_pending rows a
// single Worker tick processes -- deliberately much smaller than
// outboxworker.pumpBatchSize/imagebuild.pumpBatchSize (20): each row here
// can take up to platform.Timeouts.ReleaseManifestCheckTimeout (minutes)
// to process, sequentially, so a small batch keeps one tick's own
// worst-case wall-clock time bounded to a handful of minutes rather than
// tens of them.
const pendingBatchSize = 5

// PendingLister is the narrow slice of *postgres.ReleaseManifestPendingStore
// Worker needs -- mirrors this package's own PendingEnqueuer/
// MergedPRLister precedent: a small, locally-defined interface so a unit
// test can inject a fake with no real DB round trip.
type PendingLister interface {
	// ClaimDue stamps claimed_at on up to limit unclaimed rows, oldest
	// first, and returns them; a claimed row is never returned again.
	ClaimDue(ctx context.Context, limit int32) ([]sqlcgen.ReleaseManifestPending, error)
	// Finish deletes one claimed row once its check has returned.
	Finish(ctx context.Context, id pgtype.UUID) error
	// PurgeStaleClaimed deletes rows claimed longer ago than maxAge.
	PurgeStaleClaimed(ctx context.Context, maxAge time.Duration) (int64, error)
}

// Worker is constructed once per process (NewWorker), then run via its
// own Run method -- exactly like app/outboxworker.Builder/app/imagebuild.
// Builder/app/reconciler.Reconciler.
type Worker struct {
	store    PendingLister
	deps     Deps
	botToken string
	timeouts platform.Timeouts
}

// NewWorker builds a Worker backed by store (the durable
// release_manifest_pending queue), deps (SourceControl/Outbox/Timeouts --
// the SAME shape Run itself takes), botToken (this deployment's own
// statically-configured GitHub bot credential -- the SAME one the
// pre-fix inline call already authenticated every ListMergedBetween call
// with; never persisted onto a release_manifest_pending row itself, see
// Enqueue's own doc comment), and timeouts (for
// ReleaseManifestCheckPumpInterval/ReleaseManifestCheckTimeout).
func NewWorker(store PendingLister, deps Deps, botToken string, timeouts platform.Timeouts) *Worker {
	return &Worker{store: store, deps: deps, botToken: botToken, timeouts: timeouts}
}

// Run runs the process-wide release-manifest-check loop until ctx is
// done -- mirrors app/outboxworker.Builder.Run/app/imagebuild.Builder.
// Run/app/reconciler.Reconciler.Run exactly: a ticker on platform.
// Timeouts.ReleaseManifestCheckPumpInterval, calling PumpOnce each tick,
// logging (never propagating) any per-tick error so one bad tick never
// kills the whole loop. The caller starts this via its own errgroup.Go
// exactly once per process, against the SAME process-lifetime context
// every other background loop already uses (cmd/control-plane/main.go's
// own groupCtx) -- NEVER any individual webhook request's own context,
// which is the entire point of this fix.
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.timeouts.ReleaseManifestCheckPumpInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.PumpOnce(ctx); err != nil {
				platform.Logger(ctx).Error("releasereview: worker tick failed", "error", err)
			}
		}
	}
}

// PumpOnce runs exactly one pump tick: up to pendingBatchSize times, it
// claims the oldest unclaimed release_manifest_pending row, runs the
// actual manifest check for it, then deletes it. Exported (rather than
// only reachable through Run's own loop) so tests can drive exactly one
// tick deterministically, matching outboxworker.Builder.PumpOnce/
// imagebuild.Builder.PumpOnce's own precedent.
//
// One row at a time, so a row's claimed_at is the instant its own check
// starts: the session's status (technical plan §43.20) reads a claimed row
// as work that can still add a composition review turn to the session,
// but only within ReleaseManifestCheckTimeout plus MCPStatusScheduledMargin
// of its claim -- a row claimed in a batch and left waiting behind the
// others would outlive that bound before its check even began. Finished
// only after Run returns, so any composition turn Run inserted has already
// committed when the row goes: no snapshot sees neither. A tick first
// purges rows claimed longer ago than that same bound (their worker died
// mid-check), which the status has already stopped counting.
//
// A failure in the claim step aborts the tick and returns the error (Run
// logs it) -- but once a row is claimed, its Run call is unconditionally
// attempted: Run itself has no error return at all (it is already fully
// best-effort/void, per its own doc comment), so there is nothing here to
// isolate a per-row failure FROM -- every claimed row simply gets its one
// attempt. A purge or finish failure is logged and never aborts the tick:
// a row left claimed is bounded by the status and purged on a later tick.
func (w *Worker) PumpOnce(ctx context.Context) error {
	logger := platform.Logger(ctx)
	if purged, err := w.store.PurgeStaleClaimed(ctx, w.timeouts.ReleaseManifestCheckTimeout+w.timeouts.MCPStatusScheduledMargin); err != nil {
		logger.Warn("releasereview: purge stale claimed release manifest checks failed", "error", err)
	} else if purged > 0 {
		logger.Warn("releasereview: purged release manifest checks whose worker never finished them", "count", purged)
	}

	for i := 0; i < pendingBatchSize; i++ {
		claimed, err := w.store.ClaimDue(ctx, 1)
		if err != nil {
			return fmt.Errorf("releasereview: claim due release manifest pending check: %w", err)
		}
		if len(claimed) == 0 {
			return nil
		}
		for _, row := range claimed {
			w.process(ctx, row)
			if err := w.store.Finish(ctx, row.ID); err != nil {
				logger.Warn("releasereview: delete finished release manifest check failed; it stays claimed until purged",
					"error", err, "release_manifest_pending_id", row.ID.String())
			}
		}
	}
	return nil
}

// process runs Run for one already-claimed row, bounded by
// platform.Timeouts.ReleaseManifestCheckTimeout -- derived from ctx
// (Worker.Run's own process-lifetime context), never from any webhook
// request's own context, which no longer exists by the time this runs
// (the webhook handler that enqueued this row acked and returned long
// ago).
func (w *Worker) process(ctx context.Context, row sqlcgen.ReleaseManifestPending) {
	logger := platform.Logger(ctx).With(
		"release_manifest_pending_id", row.ID.String(),
		"owner", row.Owner,
		"repo", row.Repo,
		"pr_number", row.PrNumber,
	)

	runCtx, cancel := context.WithTimeout(ctx, w.timeouts.ReleaseManifestCheckTimeout)
	defer cancel()

	Run(runCtx, logger, w.deps, Input{
		SessionID:     row.SessionID,
		Owner:         row.Owner,
		Repo:          row.Repo,
		PRNumber:      row.PrNumber,
		BaseRef:       row.BaseRef,
		HeadRef:       row.HeadRef,
		Token:         w.botToken,
		CorrelationID: row.CorrelationID,
	})
}
