package automation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
)

// PumpOnce runs exactly one fan-out tick: claims a batch of
// automation_invocations rows not yet fanned out, then fans each one out
// into ≤10 automation_runs (one per target), OUTSIDE any transaction.
// Exported (rather than only reachable through Run's own loop) so tests
// can drive exactly one tick deterministically, matching
// imagebuild.Builder.PumpOnce's own precedent.
//
// A failure in the batch-level claim step aborts the tick and returns the
// error (Run logs it) -- but once a batch is successfully claimed, one
// invocation's own fan-out failure is isolated: logged, and does NOT abort
// the rest of the batch, exactly like app/imagebuild.Builder.PumpOnce's
// own per-row isolation.
func (e *Engine) PumpOnce(ctx context.Context) error {
	claimed, err := e.claimBatch(ctx)
	if err != nil {
		return fmt.Errorf("automation: claim batch: %w", err)
	}

	for _, inv := range claimed {
		e.fanOut(ctx, inv)
	}
	return nil
}

// claimBatch runs the ENTIRE claim step inside one transaction:
// ListDueForFanOut (FOR UPDATE SKIP LOCKED -- so a concurrent tick, this
// pod's or another pod's own Engine, claims a DISJOINT batch rather than
// double-claiming the same invocation), then ClaimForFanOut for each due
// row (flips its own fanned_out_at CAS guard), then commits -- exactly
// mirroring imagebuild.Builder.claimBatch's own shape.
func (e *Engine) claimBatch(ctx context.Context) ([]sqlcgen.AutomationInvocation, error) {
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	txInvocations := e.invocations.WithTx(tx)

	due, err := txInvocations.ListDueForFanOut(ctx, fanOutBatchSize)
	if err != nil {
		return nil, fmt.Errorf("list due invocations: %w", err)
	}

	claimed := make([]sqlcgen.AutomationInvocation, 0, len(due))
	for _, row := range due {
		c, err := txInvocations.ClaimForFanOut(ctx, row.ID)
		if err != nil {
			return nil, fmt.Errorf("claim invocation %s: %w", row.ID.String(), err)
		}
		claimed = append(claimed, c)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return claimed, nil
}

// fanOut fans out one already-claimed invocation into one automation_runs
// row per target (§3.5: "one run per target, fan-out ≤10"). One target's
// own failure is isolated -- logged, recorded as a RunStatusFailed run,
// and does NOT abort fanning out the rest of this invocation's own
// targets.
func (e *Engine) fanOut(ctx context.Context, inv sqlcgen.AutomationInvocation) {
	logger := platform.Logger(ctx).With("automation_invocation_id", inv.ID.String(), "automation_id", inv.AutomationID.String())

	targets, err := UnmarshalTargets(inv.Targets)
	if err != nil {
		logger.Error("automation: decode invocation targets failed; closing this invocation as failed with no runs", "error", err)
		// Every target this invocation was supposed to fan out is now
		// unrecoverable -- but total_runs still promises that many
		// automation_runs rows will eventually exist and reach a terminal
		// state (closeout.go's own "every run terminal" check depends on
		// it). Recording zero runs here would strand this invocation in
		// 'pending' forever. There is no target to name a failed run
		// against, so this invocation is closed directly, bypassing the
		// normal per-run accounting -- a defensive path that should be
		// unreachable in practice (targets is only ever written by this
		// package's own MarshalTargets).
		e.closeInvocation(ctx, logger, inv.ID, inv.AutomationID, true)
		return
	}

	automationRow, err := e.automations.Get(ctx, inv.AutomationID)
	if err != nil {
		logger.Error("automation: get automation for fan-out failed; closing this invocation as failed with no runs", "error", err)
		e.closeInvocation(ctx, logger, inv.ID, inv.AutomationID, true)
		return
	}

	for _, target := range targets {
		e.createRunAndSession(ctx, logger, inv, automationRow, target)
	}
}

// createRunAndSession creates ONE automation_runs row for target, and --
// unless session/turn creation itself fails -- the session+turn it
// dispatches against, together on ONE freshly opened transaction, via the
// SAME httpapi.CreateSessionOnTx core §5.1 established (see this
// package's own doc.go for the full "why together, on one tx" reasoning).
//
// spawnSource is deliberately restdtos.CreateSessionRequestSpawnSourceWeb,
// not a new dedicated enum value -- a judgment call, see this function's
// own inline comment at the assignment below for the full reasoning.
func (e *Engine) createRunAndSession(ctx context.Context, logger *slog.Logger, inv sqlcgen.AutomationInvocation, automationRow sqlcgen.Automation, target domainautomation.Target) {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		logger.Error("automation: begin fan-out tx failed", "error", err, "target", target.Name)
		e.createFailedRun(ctx, logger, inv, target)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	req := restdtos.CreateSessionRequest{
		// Judgment call: session_spawn_source (migrations/000004_sessions.
		// up.sql) has exactly four values (web/slack/linear/github), each
		// naming a real inbound ingress CHANNEL -- an automation run has
		// none (it is triggered internally by this engine, never by a
		// live webhook/REST caller). Rather than adding a fifth,
		// irreversible enum value (this codebase's own established
		// preference, see migrations/000042_image_builds_permanent_failure.
		// up.sql's own "deliberately a boolean column, not a new enum
		// value" precedent, and migrations/000022_sandbox_snapshot_id.
		// up.sql's own explicit refusal to add an unused 'stale' value) for
		// a distinction mockups.html's own Automations/session-list views
		// do not currently surface either (§12.2 item 1 names exactly four
		// session-source icons, no fifth), this reuses 'web' -- the bucket
		// for "no external ingress channel, created directly" -- as the
		// closest existing fit. This does NOT lose the "which automation
		// created this session" fact: automation_runs.session_id is the
		// real, permanent link (a future read model joins through it,
		// never spawn_source). Revisit if §8.4/§10 want a dedicated
		// value once/if the UI needs to distinguish an automation session
		// from an ordinary web one at a glance.
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceWeb,
		Repos: []restdtos.CreateSessionRequestReposElem{
			targetToReposElem(target),
		},
	}

	// §8.4 ("automations: triggers & extras"): sandboxSettings honored on
	// automation sessions -- before this Step, this request never set
	// PathScope/MockConfig at all, so an automation-created session always
	// ignored whatever sandbox scoping a maintainer configured on the
	// automation itself, even though an ordinary web session honors it via
	// this SAME CreateSessionOnTx call. applySandboxSettings logs (but does
	// not fail this run over) a decode error on the automation's own
	// persisted columns -- malformed sandbox settings must not block an
	// otherwise-healthy run from being created at all.
	applySandboxSettings(logger, &req, automationRow)

	if automationRow.Prompt != nil {
		req.Prompt = buildRunPrompt(logger, automationRow)
	}
	title := fmt.Sprintf("Automation: %s", automationRow.Name)
	req.Title = &title

	// createdBy is deliberately invalid/NULL -- an automation run has no
	// direct human user, exactly like httpapi.CreateSessionForBot's own
	// identical choice for every other unattended session-creation path
	// this codebase already has (sessions.created_by's own nullability,
	// migrations/000004_sessions.up.sql: "bot/automation-created sessions
	// may have no direct human user").
	session, hasPrompt, cerr := httpapi.CreateSessionOnTx(ctx, tx, e.sessions, e.turns, e.environments, e.auditLog, req, pgtype.UUID{}, e.epistemicCheckDefault, e.rolloutMode, e.repoSettings, e.prSessions)
	if cerr != nil {
		logger.Warn("automation: create session for target failed", "error", cerr, "target", target.Name)
		e.createFailedRun(ctx, logger, inv, target)
		return
	}

	if _, err := e.runs.WithTx(tx).Create(ctx, sqlcgen.CreateAutomationRunParams{
		InvocationID: inv.ID,
		AutomationID: inv.AutomationID,
		Target:       mustMarshalOneTarget(target),
		SessionID:    session.ID,
		Status:       sqlcgen.AutomationRunStatusStarting,
	}); err != nil {
		logger.Error("automation: create run row failed", "error", err, "target", target.Name)
		// The deferred rollback above is guaranteed to fire (committed is
		// still false) -- this whole transaction, session/turn included, is
		// definitely undone, so this target has NO run row at all yet.
		// Unlike the tx.Commit branch below, there is nothing ambiguous
		// here: falling through to createFailedRun's own pool-backed insert
		// (outside this now-dead tx) is correct and complete, exactly like
		// the pool.Begin/CreateSessionOnTx failure branches above.
		e.createFailedRun(ctx, logger, inv, target)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		logger.Error("automation: commit fan-out tx failed", "error", err, "target", target.Name)
		// UNLIKE the runs.Create failure branch immediately above, a
		// tx.Commit error is an AMBIGUOUS Postgres outcome: the server may
		// have committed this transaction (run row, session, and turn all
		// durably written) despite this client observing an error -- e.g. a
		// connection drop after the server processed the COMMIT but before
		// its acknowledgement made it back. Falling through to
		// createFailedRun here is still required (recording zero runs for
		// this target would strand this invocation in 'pending' forever,
		// exactly like this file's own fanOut doc comment reasons about),
		// but it must not risk a SECOND row for a target that may already
		// have one. createFailedRun's own CreateIfAbsent call ("ON CONFLICT
		// (invocation_id, (target::text)) DO NOTHING", backed by
		// automation_runs_invocation_target_uniq, migrations/000054) makes
		// this safe either way: if the earlier commit actually succeeded,
		// this insert is a silent no-op (the real 'starting' row already
		// exists); if it didn't, this insert lands for real as the target's
		// only row, a RunStatusFailed one.
		e.createFailedRun(ctx, logger, inv, target)
		return
	}
	committed = true

	// Fire-and-forget, OUTSIDE the transaction above, and ONLY if a
	// prompt/turn was actually created -- mirrors every other
	// CreateSessionOnTx caller's own post-commit TriggerDispatch
	// sequencing (internal/adapters/inbound/github's own coalescer,
	// create.go's own CreateSessionCore).
	if hasPrompt {
		httpapi.TriggerDispatch(ctx, e.registry, session.ID)
	}
}

// createFailedRun records a target this invocation could not even create a
// session for (or could not durably confirm a run row for -- see
// createRunAndSession's own tx.Commit failure branch) as a RunStatusFailed
// run with no linked session (session_id NULL) -- RunTriggerCreateFailed,
// applied directly at creation (Starting is never observed for this row).
// Runs OUTSIDE any transaction, via CreateIfAbsent's own idempotent insert
// ("ON CONFLICT (invocation_id, (target::text)) DO NOTHING",
// automation_runs_invocation_target_uniq, migrations/000054) on the pool
// directly -- there is nothing left to make atomic with.
//
// Idempotent, deliberately: three of createRunAndSession's own five
// failure branches reach here, and for TWO of them (pool.Begin,
// CreateSessionOnTx) no row for this target could possibly exist yet, so
// the ON CONFLICT clause is simply never exercised -- but for the THIRD
// (tx.Commit, an ambiguous Postgres outcome -- the earlier transaction may
// have actually committed server-side despite the client observing an
// error) a row MAY already exist. pgx.ErrNoRows here means exactly that:
// this target's own run was already durably recorded by the commit this
// call is recovering from, and this call is a harmless no-op -- the SAME
// "lost the race" outcome every other CAS-guarded write in this package
// already treats identically. It deliberately does NOT cascade to
// maybeCloseInvocation in that case: the pre-existing row this lost race
// against is virtually always a 'starting' row (the tx.Commit branch's own
// normal, successful outcome), still non-terminal, and will reach
// maybeCloseInvocation on its own via the normal reconcile/sweep path --
// calling it here too would be a redundant, harmless-but-pointless extra
// query, not a correctness issue, but there is no run to report a NEW
// terminalization for either way.
//
// When a row is genuinely and newly created here (the common case), it
// cascades to closeout.go's own maybeCloseInvocation exactly like a normal
// terminalization does.
func (e *Engine) createFailedRun(ctx context.Context, logger *slog.Logger, inv sqlcgen.AutomationInvocation, target domainautomation.Target) {
	run, err := e.runs.CreateIfAbsent(ctx, sqlcgen.CreateAutomationRunParams{
		InvocationID: inv.ID,
		AutomationID: inv.AutomationID,
		Target:       mustMarshalOneTarget(target),
		SessionID:    pgtype.UUID{},
		Status:       sqlcgen.AutomationRunStatusFailed,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Already recorded (this target's own run already exists,
			// almost certainly a 'starting' row from an ambiguous
			// tx.Commit that actually succeeded server-side) -- harmless
			// no-op, see this function's own doc comment.
			logger.Debug("automation: record failed run: target already has a run row, no-op", "target", target.Name)
			return
		}
		logger.Error("automation: record failed run failed", "error", err, "target", target.Name)
		return
	}
	e.maybeCloseInvocation(ctx, logger, run.InvocationID, run.AutomationID)
}

// mustMarshalOneTarget marshals a single target for the automation_runs.
// target column -- "must" because marshaling a plain targetJSON value can
// only fail for a reason (an unsupported type) that cannot occur for this
// package's own fixed struct shape; mirrors app/imagebuild.attempt's own
// identical "cannot happen for a plain map[string]string" reasoning for
// its own builtRepoSHAsJSON marshal.
func mustMarshalOneTarget(target domainautomation.Target) []byte {
	raw, err := MarshalTargets([]domainautomation.Target{target})
	if err != nil {
		// Unreachable in practice (see doc comment) -- but never panics: a
		// malformed target is recorded as an empty JSON array rather than
		// crashing this whole tick.
		return []byte("[]")
	}
	return raw
}

// targetToReposElem converts a domainautomation.Target into the
// restdtos.CreateSessionRequestReposElem shape CreateSessionOnTx requires
// -- the boundary conversion this package's own doc.go describes (mirrors
// internal/app/releasereview's own toDomainMergedPR precedent, just in the
// opposite direction: domain -> wire, not wire -> domain).
func targetToReposElem(target domainautomation.Target) restdtos.CreateSessionRequestReposElem {
	elem := restdtos.CreateSessionRequestReposElem{Name: target.Name, Url: target.URL}
	if target.Branch != "" {
		branch := target.Branch
		elem.Branch = &branch
	}
	return elem
}
