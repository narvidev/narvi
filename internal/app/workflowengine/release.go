// This file (release.go) is the workflow engine's half of the autonomy
// freeze (technical plan §40.2): the automatic advance OnTurnCompleted holds
// while autonomy is frozen (completion.go, holdAdvance below), its release
// once the freeze lifts (HeldAdvanceReleaser), and its drop when a person
// stops the session (CancelHeldAdvancesForStopRequest, and
// CancelHeldAdvancesForStop for the stop timer).
//
// A held advance is one workflow_advance_holds row, written in the
// transaction that ended the attempt's turn. The attempt is finished as it
// always is -- its status, outcome and summary stored -- and the run stays
// running with no live attempt: nothing is dispatched, no step run is
// created, the session guard (§40.1) is not asked, and no notice is sent.
// The row is the skip's durable trace, beside autonomy_freeze_skip_total.
//
// While a run holds its advance, a person's own turn on the session is
// never held: ResolveStepForNewTurn (dispatch.go) passes it through
// untracked, as it does for any running run with no live attempt. A
// person's decision on a step awaiting one never reaches here. The
// session's status reads the hold as scheduled work, never settled
// (GetSessionActivityFacts, technical plan §43.20): it creates a turn with
// no new input once the freeze lifts.
//
// Once the freeze lifts, HeldAdvanceReleaser applies each held advance
// exactly once -- every one in the tick that first finds the freeze lifted,
// oldest first, a page at a time -- in one transaction under the session's
// actor-epoch lock --
// the lock the decide endpoint and every session actor transaction take --
// it reads the freeze again, deletes the row (a compare-and-swap: of two
// replicas releasing it, one deletes it and the other finds nothing), and
// applies the stored outcome through ApplyStepOutcome, the same authority
// OnTurnCompleted would have called: the circuit breaker, the session
// guard and the next attempt's turn, whose dispatch timer is armed in the
// same transaction, so the timer pump dispatches it with no further
// trigger. When a person's stop stands on the session, the run is
// cancelled instead, as OnTurnCompleted's stopped branch ends one.
//
// A person's stop drops every advance the session holds in the stop
// request's own transaction (CancelHeldAdvancesForStopRequest, from
// httpapi's stop route), so a resume that commits right after it cannot
// revive one. The stop timer's handler (CancelHeldAdvancesForStop, by the
// stop's held_at rule) and the releaser's own check of a standing stop
// catch a stop written without that -- by a replica of the previous
// release during a rolling deploy.

package workflowengine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/autonomy"
	"github.com/narvidev/narvi/internal/app/turnguard"
	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/domain/workflow"
	"github.com/narvidev/narvi/internal/platform"
)

// releaseBatchSize bounds how many held advances one page of a release tick
// reads, oldest first. A page size, not a duration, so not a
// platform.Timeouts field -- the automation engine's reconcileBatchSize
// precedent. A tick reads every page, each after the last row of the one
// before (postgres.AdvanceHoldCursor), so however many advances a long
// freeze held, every one is released within the tick that first finds the
// freeze lifted, and each is tried at most once in that tick; each release
// is one short transaction of its own.
const releaseBatchSize = 50

// holdAdvance writes the advance past finished -- runRow's attempt, just
// finished -- as held, in deps' transaction. A run that already holds one
// keeps the first, logged: a run with no live attempt cannot reach it.
func holdAdvance(ctx context.Context, deps Deps, runRow sqlcgen.WorkflowRun, finished sqlcgen.WorkflowStepRun, sessionRow sqlcgen.Session) error {
	held, err := deps.Workflows.HoldAdvance(ctx, runRow.ID, finished.ID, sessionRow.ID)
	if err != nil {
		return fmt.Errorf("workflowengine: hold the advance: %w", err)
	}
	if !held {
		platform.Logger(ctx).Warn("workflowengine: the run already holds an advance; keeping the first",
			"run_id", runRow.ID.String(), "step_run_id", finished.ID.String())
	}
	return nil
}

// CancelHeldAdvancesForStopRequest is a person's stop request (technical
// plan §3.3) reaching the advances the autonomy freeze holds on sessionID,
// in the transaction that records the request (httpapi's stop route),
// under the session's actor-epoch lock: every one the session holds is
// dropped, whatever its held_at, and its run, still running, ends
// cancelled, as OnTurnCompleted's stopped branch ends one. Its finished
// attempt keeps its status and outcome, workflow.NextStep is not consulted
// again and no notice is sent: nothing follows a stop.
//
// Every hold is written under the same lock, so every one this reads
// committed before the stop. That includes a hold whose transaction began
// after the stop request's: its held_at, that transaction's start, is then
// later than the request's instant, and a held_at rule
// (CancelHeldAdvancesForStop) would keep it. So a resume that commits
// after the stop finds nothing to revive, and the stop holds even once a
// person resumes the session. workflows is bound to the caller's
// transaction; an error is a store failure, which it rolls back.
func CancelHeldAdvancesForStopRequest(ctx context.Context, workflows *postgres.WorkflowStore, sessionID pgtype.UUID) error {
	runIDs, err := workflows.DeleteAdvanceHoldsForStopRequest(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("workflowengine: drop the advances a stop request reaches: %w", err)
	}
	return cancelHeldRuns(ctx, workflows, runIDs, "a stop")
}

// CancelHeldAdvancesForStop is the session actor's stop timer reaching the
// advances the autonomy freeze holds on sessionID: every one held at or
// before stopRequestedAt -- the rule the stop timer deletes the session's
// work-creating timers by -- is dropped and its run ended cancelled, as
// CancelHeldAdvancesForStopRequest does. It catches a stop that a replica
// of the previous release recorded without dropping the holds itself.
// workflows is bound to the caller's transaction; a NULL stopRequestedAt
// -- no stop standing -- drops nothing. An error is a store failure, which
// the caller's transaction rolls back.
func CancelHeldAdvancesForStop(ctx context.Context, workflows *postgres.WorkflowStore, sessionID pgtype.UUID, stopRequestedAt pgtype.Timestamptz) error {
	if !stopRequestedAt.Valid {
		return nil
	}
	runIDs, err := workflows.DeleteAdvanceHoldsForStop(ctx, sessionID, stopRequestedAt)
	if err != nil {
		return fmt.Errorf("workflowengine: drop the advances a stop predates: %w", err)
	}
	return cancelHeldRuns(ctx, workflows, runIDs, "a stop")
}

// cancelHeldRuns ends each of runIDs cancelled (cancelHeldRun), their holds
// already dropped.
func cancelHeldRuns(ctx context.Context, workflows *postgres.WorkflowStore, runIDs []pgtype.UUID, cause string) error {
	for _, runID := range runIDs {
		if err := cancelHeldRun(ctx, workflows, runID, cause); err != nil {
			return err
		}
	}
	return nil
}

// cancelHeldRun ends runID cancelled when it is still running, its hold
// already dropped. cause names what ended it, for the log.
func cancelHeldRun(ctx context.Context, workflows *postgres.WorkflowStore, runID pgtype.UUID, cause string) error {
	runRow, err := workflows.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("workflowengine: read the run whose held advance %s dropped: %w", cause, err)
	}
	if runRow.Status != sqlcgen.WorkflowRunStatusRunning {
		return nil
	}
	if _, err := workflows.CancelRun(ctx, runID); err != nil {
		return fmt.Errorf("workflowengine: cancel the run whose held advance %s dropped: %w", cause, err)
	}
	platform.Logger(ctx).Info("workflowengine: workflow run cancelled with its advance held by the autonomy freeze; next step not consulted",
		"cause", cause, "run_id", runID.String())
	return nil
}

// HeldAdvanceReleaser applies the workflow advances the autonomy freeze
// held, once it lifts -- see this file's top comment. Built once, on the
// pool, in the control plane's composition root, and run in its errgroup.
type HeldAdvanceReleaser struct {
	pool                  *pgxpool.Pool
	gate                  *autonomy.Gate
	guard                 *turnguard.Guard
	sessions              *postgres.SessionStore
	workflows             *postgres.WorkflowStore
	turns                 *postgres.TurnStore
	slackThreadSessions   *postgres.SlackThreadSessionStore
	linearAgentSessions   *postgres.LinearAgentSessionStore
	githubPRSessions      *postgres.GitHubPRSessionStore
	outbox                *postgres.OutboxStore
	interval              time.Duration
	epistemicCheckDefault bool
	// pageSize is releaseBatchSize; a test pages smaller.
	pageSize int32
}

// NewHeldAdvanceReleaser builds the releaser on pool. guard is the session
// guard (technical plan §40.1) every released advance is admitted by, as
// any advance is. timeouts.AutonomyFreezeRecheckInterval paces it.
// platformShadow is the deployment's shadow flag, which the outbox store a
// released advance's notices are written through needs (§30.8);
// epistemicCheckDefault is the deployment's default the next attempt's
// turn is built with (Deps.EpistemicCheckDefault). It refuses a nil guard:
// a released advance with no guard would admit nothing, and its run would
// never move.
func NewHeldAdvanceReleaser(pool *pgxpool.Pool, guard *turnguard.Guard, timeouts platform.Timeouts, platformShadow, epistemicCheckDefault bool) (*HeldAdvanceReleaser, error) {
	if guard == nil {
		return nil, errors.New("workflowengine: the held-advance releaser needs the session guard")
	}
	gate, err := autonomy.NewGate(pool)
	if err != nil {
		return nil, fmt.Errorf("workflowengine: %w", err)
	}
	return &HeldAdvanceReleaser{
		pool:                  pool,
		gate:                  gate,
		guard:                 guard,
		sessions:              postgres.NewSessionStore(pool),
		workflows:             postgres.NewWorkflowStore(pool),
		turns:                 postgres.NewTurnStore(pool),
		slackThreadSessions:   postgres.NewSlackThreadSessionStore(pool),
		linearAgentSessions:   postgres.NewLinearAgentSessionStore(pool),
		githubPRSessions:      postgres.NewGitHubPRSessionStore(pool),
		outbox:                postgres.NewOutboxStore(pool, platformShadow),
		interval:              timeouts.AutonomyFreezeRecheckInterval,
		epistemicCheckDefault: epistemicCheckDefault,
		pageSize:              releaseBatchSize,
	}, nil
}

// Run releases held advances once when it starts -- a freeze lifted while
// no replica ran the releaser costs no wait -- and then every
// AutonomyFreezeRecheckInterval until ctx is done: the unfreeze latency of
// every held advance. A tick that fails is logged and the next one tries
// again.
func (r *HeldAdvanceReleaser) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	tick := func() {
		if _, err := r.ReleaseOnce(ctx); err != nil && ctx.Err() == nil {
			platform.Logger(ctx).Error("workflowengine: held-advance release tick failed", "error", err)
		}
	}
	tick()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			tick()
		}
	}
}

// releaseResult is what one hold's release did.
type releaseResult int

const (
	// releaseHeld: autonomy is frozen again, or the freeze could not be
	// read; the hold stays, and the tick stops.
	releaseHeld releaseResult = iota
	// releaseGone: the hold was released or dropped already, or its run no
	// longer runs; nothing applied.
	releaseGone
	// releaseApplied: the advance was applied.
	releaseApplied
	// releaseCancelled: a person's stop stands; the run was cancelled.
	releaseCancelled
)

// ReleaseOnce runs one tick: unless autonomy is frozen, it releases every
// held advance, oldest first -- page after page of pageSize, each read
// after the last row of the one before -- each in its own transaction, and
// reports how many it applied or cancelled. It reads the freeze first on
// the pool, recording nothing -- each advance it holds was counted as
// skipped when it was held -- and again inside each release's transaction,
// which is the read that decides: a freeze that lands during the tick
// stops it there. A read that fails releases nothing. One hold whose
// release fails is logged, stays held for the next tick, and leaves the
// others to be released: the next page starts after it.
func (r *HeldAdvanceReleaser) ReleaseOnce(ctx context.Context) (int, error) {
	logger := platform.Logger(ctx)
	frozen, err := r.gate.Frozen(ctx)
	if err != nil {
		return 0, fmt.Errorf("workflowengine: read the autonomy freeze before releasing held advances: %w", err)
	}
	if frozen {
		return 0, nil
	}

	released := 0
	report := func() {
		if released > 0 {
			logger.Info("workflowengine: released workflow advances the autonomy freeze held", "released", released)
		}
	}
	defer report()
	cursor := postgres.FirstAdvanceHolds
	for {
		holds, err := r.workflows.ListAdvanceHolds(ctx, cursor, r.pageSize)
		if err != nil {
			return released, fmt.Errorf("workflowengine: list held advances: %w", err)
		}
		for _, h := range holds {
			result, err := r.releaseHold(ctx, h)
			if err != nil {
				logger.Error("workflowengine: release a held workflow advance failed; it stays held for the next tick",
					"run_id", h.WorkflowRunID.String(), "error", err)
			}
			if result == releaseHeld {
				return released, nil
			}
			if result == releaseApplied || result == releaseCancelled {
				released++
			}
		}
		if len(holds) < int(r.pageSize) {
			return released, nil
		}
		if err := ctx.Err(); err != nil {
			return released, err
		}
		cursor = postgres.AdvanceHoldCursorAfter(holds[len(holds)-1])
	}
}

// releaseHold releases h in a transaction of its own -- see this file's top
// comment. Its error is the transaction's, rolled back, with h kept.
func (r *HeldAdvanceReleaser) releaseHold(ctx context.Context, h sqlcgen.WorkflowAdvanceHold) (releaseResult, error) {
	logger := platform.Logger(ctx)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return releaseHeld, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The session's actor-epoch lock, first, as the decide endpoint and
	// every session actor transaction take it: the release is serialized
	// with the session's own writes, a person's stop among them.
	if _, err := r.sessions.WithTx(tx).GetActorEpochForUpdate(ctx, h.SessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The session is gone, and its holds with it.
			return releaseGone, nil
		}
		return releaseGone, fmt.Errorf("lock the session: %w", err)
	}

	// §40.2: read again here, inside the release's own transaction -- a
	// freeze committed since the tick's read holds the advance still. A
	// read that fails is a skip, never a pass.
	frozen, err := r.gate.FrozenTx(ctx, tx)
	if err != nil {
		r.gate.RecordSkip(ctx, domainautonomy.SiteWorkflowAdvance, domainautonomy.SkipFreezeUnreadable, "run_id", h.WorkflowRunID.String())
		return releaseHeld, err
	}
	if frozen {
		r.gate.RecordSkip(ctx, domainautonomy.SiteWorkflowAdvance, domainautonomy.SkipFrozen, "run_id", h.WorkflowRunID.String())
		return releaseHeld, nil
	}

	workflows := r.workflows.WithTx(tx)
	hold, err := workflows.ReleaseAdvanceHold(ctx, h.WorkflowRunID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Another replica released it, or a person's stop dropped it.
			return releaseGone, nil
		}
		return releaseGone, fmt.Errorf("release the hold: %w", err)
	}
	runRow, err := workflows.GetRun(ctx, hold.WorkflowRunID)
	if err != nil {
		return releaseGone, fmt.Errorf("read the run: %w", err)
	}
	if runRow.Status != sqlcgen.WorkflowRunStatusRunning {
		// Nothing left to advance: the stale hold goes.
		logger.Warn("workflowengine: a held advance's run no longer runs; dropping the hold",
			"run_id", runRow.ID.String(), "run_status", string(runRow.Status))
		if err := tx.Commit(ctx); err != nil {
			return releaseGone, fmt.Errorf("commit: %w", err)
		}
		return releaseGone, nil
	}
	sessionRow, err := r.sessions.WithTx(tx).Get(ctx, hold.SessionID)
	if err != nil {
		return releaseGone, fmt.Errorf("read the session: %w", err)
	}

	// Technical plan §3.3: a stop standing on the session ends the run, as
	// OnTurnCompleted's stopped branch ends one, before NextStep is
	// consulted again.
	if sessionRow.StopRequestedAt.Valid {
		if err := cancelHeldRun(ctx, workflows, runRow.ID, "a stop"); err != nil {
			return releaseGone, err
		}
		if err := tx.Commit(ctx); err != nil {
			return releaseGone, fmt.Errorf("commit: %w", err)
		}
		return releaseCancelled, nil
	}

	stepRun, err := workflows.GetStepRun(ctx, hold.StepRunID)
	if err != nil {
		return releaseGone, fmt.Errorf("read the held attempt: %w", err)
	}
	def, err := LoadDefinition(ctx, workflows, runRow.WorkflowDefinitionID)
	if err != nil {
		return releaseGone, fmt.Errorf("load the run's definition: %w", err)
	}
	var outcome workflow.StepOutcomeStatus
	if stepRun.OutcomeStatus != nil {
		outcome = workflow.StepOutcomeStatus(*stepRun.OutcomeStatus)
	}
	if !workflow.IsValidStepOutcomeStatus(outcome) {
		return releaseGone, fmt.Errorf("the held attempt %s has no valid outcome (%q)", stepRun.ID.String(), outcome)
	}
	stepID := workflow.ID(stepRun.StepDefinitionID.String())

	deps := Deps{
		Workflows:           workflows,
		Turns:               r.turns.WithTx(tx),
		SlackThreadSessions: r.slackThreadSessions.WithTx(tx),
		LinearAgentSessions: r.linearAgentSessions.WithTx(tx),
		GitHubPRSessions:    r.githubPRSessions.WithTx(tx),
		Outbox:              r.outbox.WithTx(tx),
		// Technical plan §40.1: the released advance is admitted by the
		// session guard as any automatic advance is, and a refusal
		// escalates the run with the guard's text as its one notice. Its
		// warning is stored in this transaction, broadcast by nobody: a
		// live subscriber reads it on its next load.
		Guard:                 r.guard.WithTx(tx, nil),
		Origin:                sessionguard.OriginAutomatic,
		EpistemicCheckDefault: r.epistemicCheckDefault,
	}
	applied, err := ApplyStepOutcome(ctx, deps, runRow, def, sessionRow, stepID, outcome, stepRun.OutcomeSummary)
	if err != nil {
		return releaseGone, fmt.Errorf("apply the held outcome: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return releaseGone, fmt.Errorf("commit: %w", err)
	}
	dispatched := ""
	if applied.DispatchedTurnID != nil {
		dispatched = applied.DispatchedTurnID.String()
	}
	logger.Info("workflowengine: workflow advance released after the autonomy freeze",
		"run_id", runRow.ID.String(), "step_run_id", stepRun.ID.String(), "step_outcome", string(outcome),
		"run_status", applied.RunStatus, "dispatched_turn_id", dispatched)
	return releaseApplied, nil
}
