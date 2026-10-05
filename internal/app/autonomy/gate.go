// Package autonomy is how an automatic-action site consults the autonomy
// freeze (technical plan §40.2): Gate reads platform_settings, per action
// and with no cache, and records each action it holds.
//
// The freeze is a call-site check, deliberately not a query exclusion. A
// frozen candidate must survive the freeze; shadow-era verdicts must never
// become candidates (§30.8, which excludes them inside the candidate
// query, ListLatestAutoApprovedInRepo). A query exclusion here would also
// make a skip unrecordable: a candidate a query never returns is one no
// site can count as held.
package autonomy

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
	"github.com/narvidev/narvi/internal/platform"
)

// meterName is this package's OTel meter name, on the "narvi/<package>"
// convention.
const meterName = "narvi/autonomy"

// Gate reads the autonomy freeze for the automatic-action sites
// (domainautonomy.Site) and records what they skip.
//
// Every component that holds a site builds its own Gate from its own pool
// (NewGate), so no constructor gains an optional gate and a site cannot be
// wired without one. They share one counter: the meter provider returns
// the same instrument for the same name and description.
//
// A site reads the freeze inside its own transaction when it has one
// (FrozenTx), and on the pool (Check) right before an effect that leaves
// the database -- a merge, a delivery. Under READ COMMITTED each read sees
// every freeze committed before it starts, on every replica. An action
// whose last read came before the freeze committed finishes: that bounded
// tail, as a running turn's (§32.8), is cheaper than a row lock on every
// action to serialize against a write made a few times a year.
type Gate struct {
	store *postgres.PlatformSettingsStore
	skips metric.Int64Counter
}

// NewGate builds a Gate that reads the freeze on pool. It fails only if
// the skip counter cannot be built.
func NewGate(pool *pgxpool.Pool) (*Gate, error) {
	skips, err := otel.Meter(meterName).Int64Counter(
		"autonomy_freeze_skip_total",
		metric.WithDescription("Automatic actions an automatic-action site skipped because autonomy is frozen (reason frozen) or the freeze could not be read (reason freeze_unreadable) -- technical plan §40.2. A skip consumes nothing: the action is still a candidate once the freeze lifts."),
		metric.WithUnit("{action}"),
	)
	if err != nil {
		return nil, fmt.Errorf("autonomy: construct autonomy_freeze_skip_total counter: %w", err)
	}
	return &Gate{store: postgres.NewPlatformSettingsStore(pool), skips: skips}, nil
}

// Check reads the freeze on the pool for site, and reports whether site
// skips its action, and why. A skip is recorded here (RecordSkip), with
// attrs -- slog key-value pairs naming the action -- on its log line. A
// read that fails is a skip, SkipFreezeUnreadable, never a pass.
func (g *Gate) Check(ctx context.Context, site domainautonomy.Site, attrs ...any) (skip bool, reason domainautonomy.SkipReason) {
	frozen, err := g.store.AutonomyFrozen(ctx)
	switch {
	case err != nil:
		platform.Logger(ctx).With(attrs...).Warn("autonomy: the freeze could not be read; the automatic action is skipped",
			"site", string(site), "error", err)
		reason = domainautonomy.SkipFreezeUnreadable
	case frozen:
		reason = domainautonomy.SkipFrozen
	default:
		return false, ""
	}
	g.RecordSkip(ctx, site, reason, attrs...)
	return true, reason
}

// Frozen reads the freeze on the pool and records nothing: for a reader
// that is not itself about to skip an action -- the outbox's lag gauge,
// which leaves the rows the freeze holds out of its reading. A site reads
// through Check or FrozenTx instead.
func (g *Gate) Frozen(ctx context.Context) (bool, error) {
	frozen, err := g.store.AutonomyFrozen(ctx)
	if err != nil {
		return false, fmt.Errorf("autonomy: read the freeze: %w", err)
	}
	return frozen, nil
}

// FrozenTx reads the freeze inside tx, a site's own transaction. A read
// that fails returns its error, since the failed statement has already
// aborted tx: the caller records the skip as SkipFreezeUnreadable and
// rolls back. A frozen read records nothing either: the caller records the
// skip once its own durable trace is written.
func (g *Gate) FrozenTx(ctx context.Context, tx pgx.Tx) (bool, error) {
	frozen, err := g.store.WithTx(tx).AutonomyFrozen(ctx)
	if err != nil {
		return false, fmt.Errorf("autonomy: read the freeze: %w", err)
	}
	return frozen, nil
}

// RecordSkip records that site skipped one action for reason: one on
// autonomy_freeze_skip_total{site, reason}, and a Debug line with outcome
// skipped and attrs. A skip is never a failure, so it is never logged as
// one; each pump also logs one Info line per tick with how many actions it
// held.
func (g *Gate) RecordSkip(ctx context.Context, site domainautonomy.Site, reason domainautonomy.SkipReason, attrs ...any) {
	g.skips.Add(ctx, 1, metric.WithAttributes(
		attribute.String("site", string(site)),
		attribute.String("reason", string(reason)),
	))
	platform.Logger(ctx).With(attrs...).Debug("autonomy: automatic action skipped",
		"outcome", domainautonomy.OutcomeSkipped, "reason", string(reason), "site", string(site))
}
