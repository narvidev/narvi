package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
)

// cronTriggerConfigJSON is the on-wire shape of a TriggerTypeCron
// automation's own trigger_config column -- {"schedule": "<5-field cron
// expr>"}. Deliberately this package's OWN small, private, decode-only
// copy of the shape internal/adapters/inbound/httpapi's own automations.go
// marshals at automation-creation time, rather than a shared exported
// helper: app/automation already imports httpapi (fanout.go, for
// CreateSessionOnTx), so httpapi importing back would be a cycle. This
// package only ever needs to DECODE a cron schedule (the trigger pump
// below); it has no reason to also carry the github/linear trigger_config
// shapes httpapi's own Get/List responses decode, since those two trigger
// types are validated+stored but not yet live-evaluated by this Step (see
// internal/domain/automation/doc.go's own writeup).
type cronTriggerConfigJSON struct {
	Schedule string `json:"schedule"`
}

func unmarshalCronTriggerConfig(raw []byte) (domainautomation.CronTriggerConfig, error) {
	var wire cronTriggerConfigJSON
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &wire); err != nil {
			return domainautomation.CronTriggerConfig{}, fmt.Errorf("automation: unmarshal cron trigger config: %w", err)
		}
	}
	return domainautomation.CronTriggerConfig{Schedule: wire.Schedule}, nil
}

// EvaluateCronTriggersOnce runs exactly one cron-trigger evaluation tick
// (§8.4's own "cron path"): lists every active, TriggerTypeCron automation
// (ListActiveCronAutomations, small -- expected to stay a handful of
// rows), and for each whose own schedule matched ANY whole-minute bucket
// since that automation's own last fire (domainautomation.
// CronMatchesWithin, capped at AutomationCronCatchUpWindow -- see this
// function's own "review fix" paragraph below), attempts to claim the
// CURRENT minute bucket (ClaimCronFire's CAS) and, on winning it, calls
// CreateInvocation with a fresh snapshot of the automation's own current
// repos as targets -- ListActiveCronAutomations' own "AND status = 'active'"
// filter is the SAME "check this automation is still active before ever
// calling CreateInvocation" convention every trigger evaluator in this
// package now shares (DispatchGitHubWebhookEvent's own
// ListActiveGitHubAutomations, DispatchLinearWebhookEvent's own
// ListActiveLinearAutomations), evaluated fresh every tick (a paused
// automation simply stops appearing in this list, with no separate gate
// needed).
//
// # Review fix: catch-up window, not a point-in-time check
//
// Before this fix, this function compared ONLY the exact current instant
// against each schedule (domainautomation.CronMatches) -- so a gap between
// two consecutive evaluations wider than one AutomationEnginePumpInterval
// tick (a routine control-plane restart, a slow tick, a GC pause) silently
// and permanently lost that minute's own fire: e.g. a '30 2 * * *' schedule,
// a restart at 02:30:10, first post-restart evaluation at 02:31:10 --
// CronMatches('30 2 * * *', 02:31:10) is false, and nothing ever re-examined
// 02:30 afterward. Every OTHER time-driven path in this codebase
// (session_timers' fires_at <= now(), outbox's next_attempt_at <= now(),
// image_builds' next_retry_at <= now()) is inherently catch-up-safe by
// virtue of being a range/threshold query -- this function now is too: each
// row's own window runs from max(row.LastCronFiredAt,
// now-AutomationCronCatchUpWindow) through the current minute bucket
// (domainautomation.CronMatchesWithin), firing AT MOST ONCE per tick even
// when multiple buckets inside that window matched (CAS discipline is
// unchanged -- ClaimCronFire still only ever records the CURRENT bucket as
// this automation's own last fire, exactly as it always has).
//
// Exported (rather than only reachable through Run's own loop) so tests
// can drive exactly one tick deterministically, matching PumpOnce/
// ReconcileOnce/SweepOnce's own precedent. now is read ONCE per tick
// (never per-row), mirroring SweepOnce's own identical "one cutoff/instant
// computed once" precedent.
//
// A failure in the batch-level list step aborts the tick and returns the
// error (Run logs it) -- but once listed, one automation's own evaluation
// failure (a malformed trigger_config, a claim error) is isolated: logged,
// and does NOT abort the rest of the batch, exactly like every other pump
// in this package.
func (e *Engine) EvaluateCronTriggersOnce(ctx context.Context) error {
	now := time.Now()
	toBucket := now.Truncate(e.timeouts.AutomationCronGranularity)

	rows, err := e.automations.ListActiveCronAutomations(ctx)
	if err != nil {
		return fmt.Errorf("automation: list active cron automations: %w", err)
	}

	logger := platform.Logger(ctx)
	for _, row := range rows {
		e.evaluateCronAutomation(ctx, logger, row, now, toBucket)
	}
	return nil
}

// evaluateCronAutomation evaluates ONE automation's own catch-up window,
// (from, toBucket] -- from is the later of row.LastCronFiredAt (this
// automation's own last recorded fire, if any) and
// now-AutomationCronCatchUpWindow (the catch-up ceiling, so a genuinely
// long-down engine backfills a BOUNDED amount, never an ever-growing one);
// an automation that has NEVER fired (row.LastCronFiredAt invalid) starts
// from exactly one granularity bucket back -- the SAME single-bucket window
// CronMatches alone always evaluated, so a brand-new automation's very
// first tick behaves identically to before this fix.
func (e *Engine) evaluateCronAutomation(ctx context.Context, logger *slog.Logger, row sqlcgen.Automation, now time.Time, toBucket time.Time) {
	logger = logger.With("automation_id", row.ID.String())

	cfg, err := unmarshalCronTriggerConfig(row.TriggerConfig)
	if err != nil {
		logger.Error("automation: decode cron trigger config failed", "error", err)
		return
	}

	from := toBucket.Add(-e.timeouts.AutomationCronGranularity)
	if row.LastCronFiredAt.Valid {
		catchUpFloor := now.Add(-e.timeouts.AutomationCronCatchUpWindow)
		if row.LastCronFiredAt.Time.After(catchUpFloor) {
			from = row.LastCronFiredAt.Time
		} else {
			from = catchUpFloor
			logger.Warn("automation: cron catch-up window capped a longer gap",
				"last_cron_fired_at", row.LastCronFiredAt.Time, "catch_up_floor", catchUpFloor)
		}
	}

	matched, err := domainautomation.CronMatchesWithin(cfg.Schedule, from, toBucket, e.timeouts.AutomationCronGranularity)
	if err != nil {
		logger.Error("automation: evaluate cron schedule failed", "error", err, "schedule", cfg.Schedule)
		return
	}
	if !matched {
		return
	}

	minuteBucket := pgtype.Timestamptz{Time: toBucket, Valid: true}
	claimed, err := e.automations.ClaimCronFire(ctx, row.ID, minuteBucket)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Already fired for this exact minute bucket -- a concurrent
			// tick (this pod's or another pod's own Engine) won the race
			// first. Harmless no-op, the SAME "lost the race" outcome every
			// other CAS-guarded write in this package treats identically.
			return
		}
		logger.Error("automation: claim cron fire failed", "error", err)
		return
	}

	targets, err := UnmarshalTargets(claimed.Repos)
	if err != nil {
		logger.Error("automation: decode automation repos for cron fire failed", "error", err)
		return
	}

	if _, err := CreateInvocation(ctx, e.invocations, claimed.ID, targets); err != nil {
		logger.Error("automation: create invocation for cron fire failed", "error", err)
	}
}
