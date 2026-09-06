package automation

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// fanOutBatchSize/reconcileBatchSize/sweepBatchSize bound how many rows a
// single tick claims/processes -- plain counts, not durations, so
// (mirroring imagebuild.pumpBatchSize/outboxworker.pumpBatchSize's own
// identical precedent) each is a Go constant rather than a
// platform.Timeouts field. fanOutBatchSize is deliberately smaller than
// the other two: fanning out ONE invocation can itself create up to
// domainautomation.MaxFanOutTargets (10) real sessions, each a genuine
// Postgres transaction plus a fire-and-forget sandbox-spawn dispatch --
// far more per-item work than reconciling or sweeping a single already-
// existing row, so a smaller batch keeps one tick's own worst-case
// wall-clock time bounded, mirroring releasereview.pendingBatchSize's own
// identical "this row's own per-item cost is unusually high" reasoning.
const (
	fanOutBatchSize    = 5
	reconcileBatchSize = 50
	sweepBatchSize     = 50
)

// Engine is the process-wide background automation engine (see doc.go for
// the full writeup). Constructed once per process (NewEngine), then run
// via its own Run method -- exactly like app/imagebuild.Builder and
// app/reconciler.Reconciler.
type Engine struct {
	automations  *postgres.AutomationStore
	invocations  *postgres.AutomationInvocationStore
	runs         *postgres.AutomationRunStore
	sessions     *postgres.SessionStore
	turns        *postgres.TurnStore
	environments *postgres.EnvironmentStore
	auditLog     *postgres.AuditLogStore
	pool         *pgxpool.Pool
	registry     *sessionactor.Registry
	timeouts     platform.Timeouts
	// epistemicCheckDefault (F6, adversarial review) is the SAME
	// platform.Config.EpistemicCheckDefault value every other
	// CreateSessionOnTx-reaching caller in this codebase now threads
	// through -- createRunAndSession (fanout.go) is this Engine's own ONE
	// caller, an ordinary (never review-session) build turn, so no F7-style
	// hardcoded-false carve-out applies here.
	epistemicCheckDefault bool
	// rolloutMode/repoSettings (§10 Phase 6, §32) are the SAME
	// two REQUIRED httpapi.CreateSessionOnTx parameters every other
	// caller now threads through -- createRunAndSession (fanout.go) is
	// this Engine's own ONE caller. An automation-created session is
	// never itself refused today in practice (automation targets are
	// admin-configured, not arbitrary user input), but this Engine passes
	// the SAME real, operator-configured values every other caller does,
	// never a hardcoded rollout.ModeOpen bypass -- createFailedRun (below)
	// already handles ANY CreateSessionOnTx refusal (rollout or
	// otherwise) as a terminal RunStatusFailed row, unmodified (§32: "the
	// existing right thing, unmodified").
	rolloutMode  platform.RolloutMode
	repoSettings *postgres.RepoSettingsStore
	// prSessions (§31.4) is the SAME further REQUIRED
	// httpapi.CreateSessionOnTx parameter every other caller now threads
	// through -- see rolloutMode/repoSettings' own doc comment immediately
	// above for why an automation-created session is never itself refused
	// TODAY in practice; the identical "no hardcoded bypass" reasoning
	// applies here too, one gate later: this Engine passes the SAME real
	// githubPRSessionStore every other caller does, never a carve-out.
	prSessions *postgres.GitHubPRSessionStore
}

// NewEngine builds an Engine backed by the given stores/pool (pool is
// needed directly, alongside the stores, for the claim step's own
// transaction and for opening a fresh per-run transaction during fan-out --
// mirrors app/imagebuild.NewBuilder's own identical reasoning), registry
// (the *sessionactor.Registry whose GetOrSpawn/EnsureDispatched
// httpapi.TriggerDispatch drives, once a run's own session is committed),
// and timeouts (for AutomationEnginePumpInterval/AutomationSweepInterval/
// the two orphan thresholds, consulted by Run/PumpOnce/ReconcileOnce/
// SweepOnce).
func NewEngine(
	automations *postgres.AutomationStore,
	invocations *postgres.AutomationInvocationStore,
	runs *postgres.AutomationRunStore,
	sessions *postgres.SessionStore,
	turns *postgres.TurnStore,
	environments *postgres.EnvironmentStore,
	auditLog *postgres.AuditLogStore,
	pool *pgxpool.Pool,
	registry *sessionactor.Registry,
	timeouts platform.Timeouts,
	epistemicCheckDefault bool,
	rolloutMode platform.RolloutMode,
	repoSettings *postgres.RepoSettingsStore,
	prSessions *postgres.GitHubPRSessionStore,
) *Engine {
	return &Engine{
		automations:           automations,
		invocations:           invocations,
		runs:                  runs,
		sessions:              sessions,
		turns:                 turns,
		environments:          environments,
		auditLog:              auditLog,
		pool:                  pool,
		registry:              registry,
		timeouts:              timeouts,
		epistemicCheckDefault: epistemicCheckDefault,
		rolloutMode:           rolloutMode,
		repoSettings:          repoSettings,
		prSessions:            prSessions,
	}
}

// Run runs the process-wide automation engine until ctx is done: FOUR
// independent ticker loops, fanned out via a zero-value errgroup.Group
// (never a bare `go` statement, §11; a zero-value Group, NOT
// errgroup.WithContext, so one loop's own ctx.Err() return can never
// cancel-race the others -- mirrors app/imagebuild.Builder.Run's own
// identical two-loop fan-out, scaled to four here: §3.5's original
// three, plus §8.4's own cron trigger pump). Each tick's own error is
// logged, never propagated, so one bad tick never kills the other loops.
// The caller starts this via its own errgroup.Go exactly once per process
// (cmd/control-plane/main.go).
func (e *Engine) Run(ctx context.Context) error {
	var g errgroup.Group
	g.Go(func() error { return e.runFanOutPump(ctx) })
	g.Go(func() error { return e.runReconcilePump(ctx) })
	g.Go(func() error { return e.runSweepPump(ctx) })
	g.Go(func() error { return e.runTriggerPump(ctx) })
	return g.Wait()
}

func (e *Engine) runFanOutPump(ctx context.Context) error {
	ticker := time.NewTicker(e.timeouts.AutomationEnginePumpInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := e.PumpOnce(ctx); err != nil {
				platform.Logger(ctx).Error("automation: fan-out pump tick failed", "error", err)
			}
		}
	}
}

func (e *Engine) runReconcilePump(ctx context.Context) error {
	ticker := time.NewTicker(e.timeouts.AutomationEnginePumpInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := e.ReconcileOnce(ctx); err != nil {
				platform.Logger(ctx).Error("automation: reconcile pump tick failed", "error", err)
			}
		}
	}
}

func (e *Engine) runSweepPump(ctx context.Context) error {
	ticker := time.NewTicker(e.timeouts.AutomationSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := e.SweepOnce(ctx); err != nil {
				platform.Logger(ctx).Error("automation: sweep pump tick failed", "error", err)
			}
		}
	}
}

// runTriggerPump is §8.4's own cron-trigger evaluation loop (§8.4) --
// reuses AutomationEnginePumpInterval as its own TICKER cadence (the SAME
// 60s the fan-out/reconcile pumps already tick at) rather than a dedicated
// new interval field: a standard cron schedule's own finest resolution is
// one whole minute (platform.Timeouts.AutomationCronGranularity), so
// ticking once every 60s is already the correct cadence to evaluate it at,
// with no separate poll-interval to reason about. AutomationCronGranularity
// itself is a DIFFERENT kind of field -- not this loop's own cadence, but
// the CAS-guard bucket size EvaluateCronTriggersOnce truncates `now` by
// (triggerpump.go) -- kept apart from AutomationEnginePumpInterval since
// the two answer different questions (how often this loop wakes up, vs.
// how coarse a "have I already fired this instant" bucket is), even
// though they happen to share the same 60s/1min order of magnitude today.
//
// Review fix (missed cron evaluations): runs EvaluateCronTriggersOnce
// exactly ONCE, synchronously, immediately, BEFORE ever entering the
// ticker loop below -- time.NewTicker's own first tick only fires after a
// full AutomationEnginePumpInterval elapses, so without this a freshly
// started (or restarted) process would begin with a guaranteed blind
// window of up to 60s during which nothing evaluates cron triggers at all,
// on top of whatever gap caused the restart in the first place. This is
// pure latency-reduction, layered on top of (not a substitute for)
// EvaluateCronTriggersOnce's own catch-up-window fix (triggerpump.go,
// AutomationCronCatchUpWindow) -- that fix already makes a late first
// evaluation CORRECT (it backfills), this makes it PROMPT too.
//
// Also logs a warning whenever the observed gap between the START of two
// consecutive evaluations exceeds AutomationCronGranularity -- so an
// operator can actually SEE in logs when a tick was skipped/delayed and
// catch-up kicked in, rather than that only being inferable indirectly
// from automations.last_cron_fired_at jumping by more than one bucket.
func (e *Engine) runTriggerPump(ctx context.Context) error {
	logger := platform.Logger(ctx)

	lastEvalStartedAt := time.Now()
	if err := e.EvaluateCronTriggersOnce(ctx); err != nil {
		logger.Error("automation: trigger pump tick failed", "error", err)
	}

	ticker := time.NewTicker(e.timeouts.AutomationEnginePumpInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			now := time.Now()
			if gap := now.Sub(lastEvalStartedAt); gap > e.timeouts.AutomationCronGranularity {
				logger.Warn("automation: trigger pump observed a gap wider than one cron granularity bucket; catch-up window will backfill any missed fire",
					"gap", gap.String())
			}
			lastEvalStartedAt = now

			if err := e.EvaluateCronTriggersOnce(ctx); err != nil {
				logger.Error("automation: trigger pump tick failed", "error", err)
			}
		}
	}
}
