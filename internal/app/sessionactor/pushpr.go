// This file (pushpr.go) implements the two remaining halves of §9.3's
// ("e2e happy path") own end-to-end wiring that design decisions 3/6/7/8/9
// alone do not connect: (1) completing whichever turn is currently
// Processing when a REAL execution_complete event arrives from the
// sandbox (§3.3: "On terminal event: complete turn... re-derive session
// status, dispatch next pending" -- the turn-lifecycle half of §3.3 that
// domain/turn.TriggerComplete/TriggerFail already exist for, per their own
// doc comments, but that nothing outside this file's own tests ever
// called before this Step, since no turn could ever reach Processing in
// production before this Step's own dispatch.go existed either); (2)
// actually sending the `push` command once a turn completes successfully,
// and actually calling ports.SourceControl.CreatePR once the resulting
// push_complete event arrives -- the wiring gap an independent verifier's
// review of this Step found: ports.SourceControl/githubapi.Adapter were
// built and unit-tested in isolation but never constructed or invoked
// anywhere in the running system.
//
// Both halves are invoked from handleSandboxEvent (sandboxevent.go):
// completeProcessingTurn runs INSIDE that function's own transact (a
// turn's terminal transition is a real state write, subject to the exact
// same epoch-fencing/commit-or-rollback discipline every other write in
// this package already gets); sendPushBestEffort and createPRBestEffort
// both run AFTER that transact has already committed, and never inside
// any Postgres transaction of their own for their OWN network calls
// (SandboxCommander.SendCommand is a bounded WS write; SourceControl.
// CreatePR is a real outbound HTTP call to GitHub) -- exactly the same
// "a real network call must never hold a Postgres transaction open"
// discipline dispatch.go's own executeSpawn already established for
// SandboxProvider.CreateSandbox. createPRBestEffort's own artifact-record
// write (recordPRArtifact) is a SEPARATE, small, fresh transact, mirroring
// executeSpawn's own "network call, then a second small transact records
// the outcome" shape precisely.
//
// # Explicitly out of scope (do not expand this file's job)
//
// A "cancelled" real execution_complete outcome is handled (turn.
// TriggerCancel, StateCancelled) since it is one of ExecutionComplete.
// Outcome's own three enum values and leaving it unhandled would silently
// drop a real terminal event -- but no push is attempted for it (only a
// genuine "completed" outcome has anything to push). Building out a
// fuller stop/cancel-driven UX beyond "the turn correctly reaches
// Cancelled" is not this Step's own job.
//
// This file never touches internal/domain/gitstate (§3.4's own job:
// stash/checkout/pop, dirty-tree reconciliation) -- sendPushBestEffort's
// own push is a plain push of whatever branch the session's own repos
// config already named at spawn time, matching design decision 7's own
// "no pre-existing dirty state to reconcile in the happy path" scoping
// exactly. A repo whose own repos[].branch was left null ("use the repo's
// default base branch") is honestly skipped here -- see
// sendPushBestEffort's own doc comment for why this Step cannot resolve
// that case.

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/shadowledger"
	"github.com/narvidev/narvi/internal/app/shadowscm"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/provenance"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// resolvePRBaseBranch resolves owner/repoName's REAL current default branch
// via a real GitHub API call -- ports.SourceControl.ResolveBranchSHA, called
// with Branch: "" (the same "repo's own default branch" resolution
// imagebuild/builder.go's own refreshOne and sessionactor's own
// checkContractDrift already rely on for their own empty-branch case; see
// that method's own doc comment). Fix (independent of, and narrower than,
// any drift-detection concern this same investigation also looked at):
// every PR this file used to open targeted a hardcoded "main" regardless of
// a repo's actual configured default branch (e.g. "master", "develop") --
// this resolves it for real, per repo, instead of guessing.
//
// Bounded by a.timeouts.RepoSHAResolutionTimeout, mirroring
// checkContractDrift's (contractdrift.go) and refreshOne's
// (imagebuild/builder.go) own identical per-call bound for this exact port
// method. The resolved SHA itself is discarded -- this call only ever
// needs ResolveBranchSHA's SECOND return value (resolvedBranch), never the
// first.
func (a *Actor) resolvePRBaseBranch(ctx context.Context, owner, repoName, token string) (string, error) {
	baseCtx, cancel := context.WithTimeout(ctx, a.timeouts.RepoSHAResolutionTimeout)
	defer cancel()
	_, resolvedBranch, err := a.sourceControl.ResolveBranchSHA(baseCtx, ports.ResolveBranchSHASpec{
		Owner:  owner,
		Repo:   repoName,
		Branch: "",
		Token:  token,
	})
	if err != nil {
		return "", fmt.Errorf("sessionactor: resolve repo default branch: %w", err)
	}
	return resolvedBranch, nil
}

// executionOutcomeTrigger maps a real, wire-level sandboxws.
// ExecutionComplete.Outcome to the domain/turn.Trigger that reports it --
// the ONLY three values ExecutionCompleteOutcome's own generated
// UnmarshalJSON accepts. ok is false for anything else (a value that
// somehow bypassed that validation) so callers never attempt a
// turn.Transition call with a fabricated trigger.
func executionOutcomeTrigger(outcome sandboxws.ExecutionCompleteOutcome) (turn.Trigger, bool) {
	switch outcome {
	case sandboxws.ExecutionCompleteOutcomeCompleted:
		return turn.TriggerComplete, true
	case sandboxws.ExecutionCompleteOutcomeFailed:
		return turn.TriggerFail, true
	case sandboxws.ExecutionCompleteOutcomeCancelled:
		return turn.TriggerCancel, true
	default:
		return 0, false
	}
}

// stampedSuppressor is the narrow, consumer-side view of the shadow SCM
// decorator this file needs: suppress-and-record a PR creation on a
// decision frozen earlier, ignoring the repo's current egress mode.
// Declared here rather than in ports because it exists for §30.8's
// stamp discipline alone and has exactly one implementation.
type stampedSuppressor interface {
	SuppressCreatePR(ctx context.Context, spec ports.CreatePRSpec) (ports.PRRef, error)
}

// errNoStampedSuppressor is returned when a cycle stamped shadow reaches
// a source control that cannot suppress-and-record. Failing is the safe
// direction: the alternative is a real pull request in a customer repo.
var errNoStampedSuppressor = errors.New("sessionactor: push cycle stamped shadow but source control cannot suppress-and-record; refusing to create a PR")

// pushSignal is what completeProcessingTurn hands back to
// handleSandboxEvent when (and only when) a turn just completed
// successfully -- the actual SandboxCommander.SendCommand call happens
// AFTER the caller's own transact has committed (sendPushBestEffort),
// never inside it, since composing/sending that command needs nothing
// further from the database.
type pushSignal struct {
	gen   int
	repos []sessionconfig.SessionConfigReposElem
	// suppressedInShadow is the SAME §30.8 push/PR egress decision
	// completeProcessingTurn resolves and persists onto the sandbox row
	// (below) -- carried on the signal too, since sendPushBestEffort
	// consumes it in the SAME handleSandboxEvent invocation that produced
	// it, before any wire redelivery gap the persisted column exists to
	// survive (createPRBestEffort, a genuinely separate later invocation,
	// still reads the persisted column, never this field). §30.9
	// (resolved: no git mirror; short-circuit the push, done properly):
	// true means sendPushBestEffort must never send the sandbox a push
	// command at all for this cycle.
	suppressedInShadow bool
}

// completeProcessingTurn implements this file's own first half: given a
// real, already-schema-validated execution_complete event's raw wire
// bytes (already persisted verbatim by handleSandboxEvent's own
// appendRawEvent call, immediately before this is ever invoked), find
// whichever turn is currently Processing and drive it to its real
// terminal state, exactly mirroring handleTurnDeadlineTimer's own
// (timeout-triggered) shape -- re-deriving session status and deleting
// turn_deadline identically -- but WITHOUT ever calling
// turn.RequiresSyntheticExecutionComplete/appending a synthetic
// execution_complete: a REAL terminal event already arrived on the wire
// (this SandboxEvent itself), so no synthesis is ever needed here,
// regardless of which Trigger constant was used to compute the
// transition (RequiresSyntheticExecutionComplete's own doc comment
// reserves synthesis for CONTROL-PLANE-internal triggers with no real
// wire event of their own -- timeout/abandon/cancel-by-the-control-plane
// -- which this real-event-driven path never is).
//
// Returns (nil, nil) -- not an error -- for every case where there is
// genuinely nothing to complete: no turn currently Processing (a
// redelivery of an already-handled execution_complete, or one that
// arrived with nothing ever dispatched), or an unrecognized Outcome.
// Returns a non-nil *pushSignal only when the completed trigger was
// genuinely TriggerComplete (a failed/cancelled turn has nothing to
// push) AND the session names at least one repo.
//
// inserted is handleSandboxEvent's own appendRawEvent result for THIS
// event -- true only the FIRST time this exact (session_id, messageID)
// pair is ever seen (events.sql's own upsert-on-(session_id, message_id)
// doc comment), false on every wire-level redelivery of an already-
// persisted execution_complete. Threaded through here for exactly one
// reason: recordFalseFailureIfApplicable, below, must run at most ONCE
// per genuinely-late completion, not once per redelivery of that SAME
// late event -- see that function's own doc comment for why, and
// sandboxevent.go's own cmd.Type == "tool_call" case for the identical
// gating idiom this mirrors (maybeEnqueueLinearProgress's own inserted
// parameter).
func (a *Actor) completeProcessingTurn(ctx context.Context, tx pgx.Tx, sandboxRow sqlcgen.Sandbox, raw json.RawMessage, inserted bool) (*pushSignal, error) {
	var evt sandboxws.ExecutionComplete
	if err := json.Unmarshal(raw, &evt); err != nil {
		// Defensive, not fatal -- mirrors peekAckID's own doc comment
		// exactly ("a decode failure here is defensive, not expected"):
		// wshub's own read loop (internal/adapters/inbound/wshub/
		// dispatch.go) only peeks a small, permissive envelope struct
		// before ever constructing this SandboxEvent, it never validates
		// against sandboxws.ExecutionComplete's own stricter generated
		// UnmarshalJSON (required keys, enum/pattern checks) -- so a
		// genuinely malformed-per-schema execution_complete CAN reach this
		// point in production. Returning an error here would roll back the
		// caller's WHOLE transact, including the raw event this same
		// transact already persisted moments ago via appendRawEvent --
		// this file's own top comment (sandboxevent.go) promises "persist
		// ALWAYS, for every recognized event type" unconditionally, so a
		// decode failure here must never retroactively break that promise.
		a.logger.Warn("sessionactor: execution_complete failed schema decode; persisted verbatim, but no turn completion attempted",
			"error", err)
		return nil, nil
	}

	trig, ok := executionOutcomeTrigger(evt.Outcome)
	if !ok {
		a.logger.Warn("sessionactor: execution_complete carries an unrecognized outcome; ignoring",
			"outcome", string(evt.Outcome))
		return nil, nil
	}

	turns, err := a.stores.turn.WithTx(tx).ListForSession(ctx, a.sessionID)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: list turns: %w", err)
	}
	processing, ok := findProcessingTurn(turns)
	if !ok {
		// Confirmed audit finding (MEDIUM): gated on inserted, not just
		// trig == TriggerComplete -- without this, EVERY wire-level
		// redelivery of the SAME already-counted late execution_complete
		// re-enters this branch (turn.TriggerComplete never changes turn/
		// session state here, so nothing about redelivery #2+ differs from
		// #1 from this function's own point of view) and would re-increment
		// turn_false_failure_total again, unbounded, once per resend --
		// §6.1's ack protocol buffers and resends exactly this class of
		// critical event until acked, and this branch is BY CONSTRUCTION
		// reached only when the connection was unhealthy enough to let
		// turn_deadline fire first, so a redelivery here is the likely
		// case, not the rare one. inserted (appendRawEvent's own upsert
		// result, threaded in from handleSandboxEvent) is true only the
		// FIRST time this exact messageID is ever seen -- exactly the
		// discriminator recordFalseFailure's own "count the false failure
		// once" contract needs.
		if trig == turn.TriggerComplete && inserted {
			if err := a.recordFalseFailureIfApplicable(ctx, tx); err != nil {
				// Observability-only: never fail (or roll back) an
				// otherwise-legitimate "nothing to do" outcome over a
				// failure to READ the session row for this metric.
				a.logger.Warn("sessionactor: false-failure check failed", "error", err)
			}
		}
		a.logger.Warn("sessionactor: execution_complete arrived with no turn in processing; ignoring")
		return nil, nil
	}

	to, err := turn.Transition(turn.StateProcessing, trig)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: turn transition via real execution_complete (trigger %s): %w", trig, err)
	}

	now := time.Now()
	if _, err := a.stores.turn.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:          processing.ID,
		Status:      sqlcgen.TurnStatus(to),
		CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("sessionactor: update turn status: %w", err)
	}

	failureReason, _ := turn.DeriveFailureReason(turn.StateProcessing, trig)
	if err := a.persistDerivedSessionStatus(ctx, tx, summariesWithOverride(turns, processing.ID, to, failureReason)); err != nil {
		return nil, err
	}
	if err := a.deleteTimer(ctx, tx, TimerTurnDeadline); err != nil {
		return nil, err
	}

	sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: get session: %w", err)
	}

	// §8.1 ("plan mode, web", §8.1/§12.2 item 3): a plan_mode=true turn
	// that just genuinely completed records exactly one new plans row, in
	// this SAME transaction -- see planrecord.go's own doc comment.
	// recordPlanIfNeeded itself is a no-op (nil, nil) for every other case
	// (plan_mode false, or trig != TriggerComplete). Deliberately called
	// BEFORE enqueueOutboxNotification below (§8.1, "plan mode,
	// cross-channel", reordering this Step's own predecessor): that call
	// needs to know whether a plan was just recorded (and its own id/
	// version) to route this turn's completion to the plan-approval-
	// request notification instead of the generic one.
	plan, err := a.recordPlanIfNeeded(ctx, tx, processing, trig)
	if err != nil {
		return nil, err
	}

	// §25.6/§25.9 ("workflow execution engine" / "workflow HITL gate +
	// circuit breaker", §25.6/§25.9): if processing's own turn is a live,
	// engine-tracked workflow step attempt, finalize it (and, unless
	// HITLAfter-gated, consult workflow.NextStep -- via ApplyStepOutcome,
	// wiring loopguard.Evaluate and dispatching the next attempt's turn as
	// needed -- to advance/complete/escalate the owning run) -- a no-op,
	// logged only, for a turn this package never tracked (see
	// OnTurnCompleted's own doc comment). Never returns an error: this is
	// bookkeeping, never allowed to roll back a turn's own already-persisted
	// completion. sessionRow is the SAME row already fetched above.
	workflowengine.OnTurnCompleted(ctx, workflowengine.Deps{
		Workflows:             a.stores.workflow.WithTx(tx),
		Turns:                 a.stores.turn.WithTx(tx),
		SlackThreadSessions:   a.stores.slackThreadSession.WithTx(tx),
		LinearAgentSessions:   a.stores.linearAgentSession.WithTx(tx),
		GitHubPRSessions:      a.stores.githubPRSession.WithTx(tx),
		Outbox:                a.stores.outbox.WithTx(tx),
		EpistemicCheckDefault: a.epistemicCheckDefault,
	}, sessionRow, processing.ID, trig)

	// §5.1 ("outbox delivery", §5.1): enqueue exactly one outbox
	// notification for THIS turn's completion, in the SAME transaction as
	// the state change above -- runs for every outcome (complete/fail/
	// cancel alike), unlike the push/PR path below which is success-only.
	// A no-op (never an error propagated to the caller) for a 'web'-origin
	// session, or a non-'web'-origin session whose own reverse-lookup row
	// is missing -- see enqueueOutboxNotification's own doc comment
	// (outboxenqueue.go).
	if err := a.enqueueOutboxNotification(ctx, tx, sessionRow, trig, failureReason, processing, plan); err != nil {
		return nil, err
	}

	if trig != turn.TriggerComplete {
		// A failed/cancelled turn has nothing to push -- §9.3's resilience
		// scenarios for THOSE outcomes are later Steps' own job (see this
		// file's own top comment), not this one's.
		return nil, nil
	}

	repos, err := reposFromJSON(sessionRow.Repos)
	if err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return nil, nil
	}

	// §30.8's own epoch discipline, applied to this turn's own push/PR
	// pair: "the push/PR pair resolves its mode ONCE per turn... resolved
	// at push send, persisted with the signal, and createPRBestEffort
	// honors the persisted decision -- never a re-read." Resolved EXACTLY
	// ONCE, right here (the SAME session-repos-aggregated formula
	// postgres.OutboxStore.ResolveEffectiveMode already centralizes for
	// the outbox's own identical epoch-stamp need), and persisted onto the
	// sandbox row in this SAME transact. The column, not the signal, is
	// what carries the decision to createPRBestEffort: that runs from a
	// SEPARATE wire-event handler invocation with nothing in memory
	// surviving in between. It rides on pushSignal too, for
	// sendPushBestEffort, which this same handler invocation calls after
	// the commit.
	suppressedInShadow := a.stores.outbox.WithTx(tx).ResolveEffectiveMode(ctx, a.sessionID)
	if _, err := a.stores.sandbox.WithTx(tx).SetPendingPush(ctx, sqlcgen.SetSandboxPendingPushParams{
		SessionID:                     a.sessionID,
		PendingPushSuppressedInShadow: &suppressedInShadow,
	}); err != nil {
		return nil, fmt.Errorf("sessionactor: persist push/PR egress-mode decision: %w", err)
	}

	// §30.6's record-or-fail, for the push this turn is about to NOT
	// send. It belongs here, inside the transact, and not beside the
	// suppression itself.
	//
	// sendPushBestEffort runs after this transaction commits AND after
	// the wire ack has gone out, so a ledger failure there has nothing
	// left to hand the error to: the execution_complete is never
	// redelivered, the turn is already Completed, and the row is lost
	// permanently. The ledger row IS the product in shadow -- there is no
	// other trace of a push that was never sent -- so losing it silently
	// is exactly the "suppressed but unrecorded" contract violation §30.6
	// names. Recorded here, it commits with the turn or not at all, and a
	// failure leaves the event unacked for redelivery.
	if suppressedInShadow {
		if err := a.recordSuppressedPush(ctx, tx, repos); err != nil {
			return nil, err
		}
	}

	return &pushSignal{gen: int(sandboxRow.Gen), repos: repos, suppressedInShadow: suppressedInShadow}, nil
}

// recordFalseFailureIfApplicable implements the "false failures"
// instrument (§5.3's metric list itself names
// no precise definition -- this is the derived semantics, reasoned from
// this codebase's own already-modelled §3.2/§9.3-scenario-#4 "late-
// success reconciliation" concept).
//
// Called ONLY from completeProcessingTurn's own no-turn-currently-
// Processing branch, and ONLY when the just-arrived real wire event's own
// outcome was genuinely "completed" (trig == turn.TriggerComplete) AND
// this is genuinely the first time this exact execution_complete messageID
// has ever been seen (inserted == true, that branch's own doc comment) --
// i.e. the sandbox is, right now, telling the control plane a turn
// succeeded, for the first time, but by the time this event arrived no
// turn was Processing anymore for this session to attach that success to.
// A wire-level redelivery of the SAME late execution_complete must never
// reach this function a second time: nothing about this function's own
// gate (session status/failure_reason) changes between redeliveries, so
// without the caller's own inserted gate this would re-count the identical
// false failure once per resend.
//
// domain/turn.State's own transitions table (state.go) has NO Failed ->
// Completed edge, by deliberate design (see that file's own top comment):
// a turn this genuinely applies to is normally STILL Processing when a
// real completion arrives, because the one case where a live sandbox can
// go quiet long enough to legitimately lose track of it --
// terminal_grace's Suspect-grace window, a couple of minutes -- is
// dwarfed by turn_deadline's own independent budget (tens of minutes),
// and Suspect-recovery-during-grace (sandboxevent.go) already reconciles
// that case for real, completing the STILL-Processing turn in the same
// event.
//
// The one way a real, late "completed" CAN still arrive with NOTHING
// Processing is the other branch of that timing: turn_deadline itself
// expired FIRST (handleTurnDeadlineTimer, timerfired.go) -- the control
// plane's own inference that the turn was stuck, with no wire signal yet
// -- terminalizing the turn Failed with failure_reason=timeout, and only
// AFTER that does the sandbox's real execution_complete finally show up
// reporting success. That is a genuine false failure: the control plane
// killed a turn its own deadline said was stuck, and the agent was
// actually still working and got there.
//
// This is deliberately NOT re-derived by scanning turns for the most
// recently terminal one and asking why -- turns carry no failure_reason
// column at all (turn.FailureReason's own doc comment: only the (from,
// trigger) pair that PRODUCED a terminal transition knows the reason, and
// only the caller making that transition can derive it). Instead this
// reads the SAME already-persisted proxy rederiveSessionStatusUnchanged
// (timerfired.go) already established for the identical problem: since
// domain/session.DeriveStatus derives the session's own failure_reason
// from the LAST turn's outcome, and at most one turn can ever be
// Processing at a time (turns_one_processing_per_session), the session's
// CURRENT failure_reason, read back right now, already IS "why the most
// recently terminalized turn failed" -- no re-derivation needed. A
// timeout-derived Failed session is the narrow, precise gate: a genuine
// agent-reported failure (failure_reason=failed) or an explicit cancel
// are not "the control plane was wrong", they are the agent's or a
// user's own decision, and a later contradicting "completed" for either
// would be a wire anomaly (duplicate/corrupted redelivery), not a false
// failure -- deliberately not counted here.
//
// §12.2 item 6's own platform-wide analytics rollup adds a
// durable sibling to the OTel-only signal this function always recorded:
// a false_failures row (migrations/000124_false_failures.up.sql),
// inserted in the SAME transaction as everything else this call
// participates in. turn_false_failure_total (the OTel counter,
// recordFalseFailure below) is UNCHANGED -- a live dashboard still reads
// it exactly as before -- this is a SECOND consumer of the identical
// fact, queryable by time window and surviving a process restart, which
// the counter alone never could.
func (a *Actor) recordFalseFailureIfApplicable(ctx context.Context, tx pgx.Tx) error {
	sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: get session: %w", err)
	}
	if sessionRow.Status != sqlcgen.SessionStatusFailed {
		return nil
	}
	if sessionRow.FailureReason == nil || *sessionRow.FailureReason != sqlcgen.SessionFailureReasonTimeout {
		return nil
	}
	if _, err := a.stores.falseFailure.WithTx(tx).Insert(ctx, a.sessionID); err != nil {
		return fmt.Errorf("sessionactor: insert false_failures row: %w", err)
	}
	a.recordFalseFailure(ctx)
	return nil
}

// sendPushBestEffort implements this file's own second half's first step:
// once a turn has genuinely completed, best-effort send a real
// sandboxws.Push command for every repo the session names whose own
// repos[].branch is a known, explicit branch name. Never rolls anything
// back on failure (this runs AFTER handleSandboxEvent's own transact has
// already committed the turn's completion -- that fact is real regardless
// of whether a push command can be delivered) -- a log line is this Step's
// own honest, complete recourse, matching design decision 3b's own
// "SendCommand failing is logged, not retried by any new machinery"
// precedent for the symmetric dispatch-a-prompt case.
//
// A repo whose repos[].branch was left null ("use the repo's default base
// branch", per restdtos.CreateSessionRequestReposElem's own doc comment)
// is skipped, not guessed at: resolving which branch gitclone actually
// ended up checking out for such a repo would require either gitclone
// itself reporting back the real checked-out branch name (it does not) or
// a real `git symbolic-ref` round trip through the sandbox this Step's own
// wire contract has no command for -- an honest, documented gap, not a
// silent shortcut.
//
// # §30.9 (resolved): the push itself is gated, not merely accepted as a failure
//
// sig.suppressedInShadow is the SAME §30.8 push/PR egress decision
// completeProcessingTurn resolves and persists onto the sandbox row. An
// earlier version of this function deliberately did NOT consult it here,
// reasoning that the sandbox-side read-only credential (§30.4) already
// makes a shadow repo's push harmless and that §30.8's fix was the push/PR
// pair's CONSISTENCY alone. That reasoning was correct about SECURITY --
// §30.4 remains explicit that the read-only token and the UID boundary
// are the whole security boundary here, this gate is not a second one --
// but wrong about the CONSEQUENCE of leaving the send ungated: a real push
// attempt against a read-only credential does not silently no-op, it
// fails, producing a push_error on every single shadow turn that pushes.
// A failed push never yields a push_complete, so createPRBestEffort (the
// gated PR path §30.2's decorator already covers) never even runs -- the
// ledger's single most important entry, "would have opened PR ...", was
// never recorded at all, and every shadow turn looked broken to whoever
// was evaluating it.
//
// §30.9 resolves this: gate the send itself. When sig.suppressedInShadow
// is true, the push command is never sent -- recordSuppressedPush, called
// from completeProcessingTurn INSIDE its own transaction, has already
// recorded the branch that would have been pushed AND that
// createPRBestEffort would have opened a pull request against it next
// (combined into ONE ledger row, since no push_complete will ever arrive
// to let createPRBestEffort record the second half itself). This is
// consistency, still never security: a shadow repo's push would already
// fail against the read-only credential exactly as before, this gate only
// changes whether that failure is attempted, surfaced as an error, and
// left unrecorded, or skipped, recorded, and read by the evaluator as the
// product working as designed.
func (a *Actor) sendPushBestEffort(sessionID string, sig *pushSignal) {
	if sig == nil {
		return
	}

	if sig.suppressedInShadow {
		// The ledger row was already written, inside the transaction that
		// resolved this decision (completeProcessingTurn). Nothing to do
		// here but not send the command.
		return
	}

	if a.commander == nil {
		a.logger.Warn("sessionactor: turn completed but no SandboxCommander is configured; skipping push")
		return
	}

	repos := make([]sandboxws.PushReposElem, 0, len(sig.repos))
	for _, r := range sig.repos {
		if r.Branch == nil || *r.Branch == "" {
			a.logger.Warn("sessionactor: session repo has no explicit branch; skipping auto-push for it", "repo", r.Name)
			continue
		}
		repos = append(repos, sandboxws.PushReposElem{Name: r.Name, Branch: *r.Branch})
	}
	if len(repos) == 0 {
		return
	}

	push := sandboxws.Push{
		Type:      "push",
		MessageId: uuid.NewString(),
		SessionId: sessionID,
		Gen:       sig.gen,
		Repos:     repos,
	}
	payload, err := json.Marshal(push)
	if err != nil {
		a.logger.Error("sessionactor: marshal push command failed", "error", err)
		return
	}
	if err := a.commander.SendCommand(sessionID, payload); err != nil {
		a.logger.Warn("sessionactor: send push command failed", "error", err)
	}
}

// recordSuppressedPush is the shadow half of this turn's push decision
// (§30.7/§30.9, resolved: no git mirror -- short-circuit the push, done
// properly): one shadowledger.Push row per repo this turn would have
// pushed, naming the branch and recording that a pull request would have
// followed -- see that type's own doc comment (shadowledger/spec.go) for
// why one combined row rather than two.
//
// It runs INSIDE completeProcessingTurn's transaction, and that placement
// is the point. §30.6's record-or-fail says a suppression that cannot be
// evidenced is a contract violation, and in shadow this row is the ONLY
// trace a push ever existed: no command is sent, so no push_complete or
// push_error ever comes back, and createPRBestEffort never runs to record
// the PR half itself. Recorded after the commit it would be best-effort
// against nothing -- the ack has already gone out, the turn is Completed,
// and a failed insert is lost for good. Recorded here, it commits with
// the turn or not at all.
//
// Every repo with a branch gets a row, matching sendPushBestEffort's own
// loop exactly. A repo whose clone URL is not owner/repo on a supported
// host is still recorded, under a host+path identity: the live loop has
// no host check and really does push such a repo, so skipping it here
// would mean the ONE case where suppression leaves no evidence is also a
// case where the live path acts. Any userinfo in the URL is dropped
// before it is used -- a credential embedded in a clone URL must not
// reach the ledger, and a sealed spec type cannot stop what arrives
// inside a string.
func (a *Actor) recordSuppressedPush(ctx context.Context, tx pgx.Tx, repos []sessionconfig.SessionConfigReposElem) error {
	for _, r := range repos {
		if r.Branch == nil || *r.Branch == "" {
			// Skipped by sendPushBestEffort too, for the same reason:
			// there is no branch to push, so nothing was suppressed.
			continue
		}
		branch := *r.Branch

		repoFullName, spec := suppressedPushIdentity(r.Url, branch)
		if err := shadowledger.Record(ctx, a.stores.shadowLedgerStore.WithTx(tx), shadowledger.Entry{
			Operation:    "push",
			RepoFullName: repoFullName,
			Target:       branch,
			Spec:         spec,
			SessionID:    a.sessionID,
		}); err != nil {
			return fmt.Errorf("sessionactor: record suppressed push for %s: %w", r.Name, err)
		}
	}
	return nil
}

// suppressedPushIdentity derives the ledger identity for a repo whose
// push is being suppressed.
//
// The ordinary case is owner/repo on a supported host. The fallback
// exists because session creation validates a clone URL's scheme and host
// only, so a URL that is neither is perfectly reachable -- and the live
// push loop pushes it regardless. Recording it under host+path keeps the
// evidence, and never under a bare name that could collide.
//
// User info is stripped unconditionally. A clone URL may legitimately
// carry credentials, and this string is about to be written to a
// permanent, append-only table.
func suppressedPushIdentity(rawURL, branch string) (string, shadowledger.Push) {
	if owner, repoName, err := reposource.ParseOwnerRepo(rawURL); err == nil {
		if hostErr := reposource.CheckRepoHost(rawURL, ports.SupportedSourceControlHosts()...); hostErr == nil {
			return owner + "/" + repoName, shadowledger.Push{Owner: owner, Repo: repoName, Branch: branch, WouldOpenPR: true}
		}
	}
	identity := redactedURLIdentity(rawURL)
	return identity, shadowledger.Push{Owner: "", Repo: identity, Branch: branch, WouldOpenPR: true}
}

// redactedURLIdentity renders a clone URL as host+path with any userinfo
// removed, or a fixed placeholder if it cannot be parsed at all. Never
// empty: shadowledger.Entry requires a non-empty RepoFullName, and an
// unrecordable suppression is worse than a coarsely-named one.
func redactedURLIdentity(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return "unparseable-repo-url"
	}
	return strings.TrimSuffix(parsed.Host+parsed.Path, ".git")
}

// createPRBestEffort implements this file's own second half's second
// step: once a push_complete event has already been persisted (generically,
// by handleSandboxEvent's own appendRawEvent, before this is ever
// invoked), open a pull request for each pushed repo using the session's
// own creator's decrypted GitHub OAuth token, and record each success as
// an artifact row (type "pr") -- the ONLY durable place a created PR's
// URL/number is ever written. This is deliberately what makes it visible
// to a client: GET /api/sessions/{id}/artifacts (§6.2) and the client
// WS hub's own SubscribedPayload.artifacts (§6.2) already exist and
// already read this exact table -- no new wire contract is invented here.
//
// Never rolls anything back and never retries: per ports.SourceControl's
// own doc comment, "no caller of this port retries or trips a
// circuit-breaker on a PR-creation failure yet" -- a failure here (no
// usable OAuth token, a GitHub API error, ...) is logged, and the happy
// path simply has no PR artifact to show for that repo, exactly like a
// push failure (push_error) already has no further automated recourse in
// this Step's own scope.
//
// The decrypted OAuth token is NEVER logged, here or anywhere it might
// propagate to.
func (a *Actor) createPRBestEffort(ctx context.Context, raw json.RawMessage) {
	if a.sourceControl == nil {
		a.logger.Warn("sessionactor: push_complete arrived but no SourceControl is configured; skipping PR creation")
		return
	}

	var evt sandboxws.PushComplete
	if err := json.Unmarshal(raw, &evt); err != nil {
		a.logger.Error("sessionactor: decode push_complete for PR creation failed", "error", err)
		return
	}
	if len(evt.Repos) == 0 {
		return
	}

	// A plain, pool-scoped read (not WithTx) -- gathering information for
	// an outbound API call is not a state mutation, so it needs none of
	// transact's own epoch-fencing; the same already-established "read
	// straight off the store" precedent internal/adapters/inbound/httpapi
	// handlers already use throughout.
	sessionRow, err := a.stores.session.Get(ctx, a.sessionID)
	if err != nil {
		a.logger.Error("sessionactor: get session for PR creation failed", "error", err)
		return
	}

	// (§17.2 amendment): a sentinel-auto-fix child session has NO
	// human creator to attribute a PR to (sessionRow.CreatedBy is
	// invalid/NULL, SpawnChildSession's own doc comment) -- routing it
	// through creatorMayGetPRAttribution below would ALWAYS reject it
	// (that guard's own doc comment: "if !createdBy.Valid { return
	// false }"), silently dropping every sentinel fix PR. This is a
	// SEPARATE, dedicated code path -- never resolvePRBaseBranch, never
	// the per-repo loop below -- see createSentinelFixPRBestEffort's own
	// doc comment.
	// §30.8's own "the push/PR pair resolves its mode ONCE per turn":
	// honor the decision completeProcessingTurn already resolved and
	// persisted at push-send time (pushSignal.suppressedInShadow's own
	// doc comment) -- NEVER re-derive it here via egressmode.Resolve or
	// postgres.OutboxStore.ResolveEffectiveMode. That re-read is exactly
	// the bug this part of the Step exists to prevent: a repo demoted (or
	// promoted) between this turn's own push send and this push_complete
	// arriving must not flip what this ONE turn's PR creation does.
	sandboxRow, err := a.stores.sandbox.Get(ctx, a.sessionID)
	if err != nil {
		a.logger.Error("sessionactor: get sandbox for push/PR egress-mode decision failed; skipping PR creation (fail-closed)", "error", err)
		return
	}
	suppressedInShadow, pushCancelled := sandboxRow.PendingPushSuppressedInShadow, sandboxRow.PendingPushCancelled
	if _, clearErr := a.stores.sandbox.ClearPendingPush(ctx, a.sessionID); clearErr != nil {
		a.logger.Warn("sessionactor: clear consumed push/PR egress-mode decision failed", "error", clearErr)
	}
	// stampSaysShadow carries the frozen decision INTO the paths below
	// rather than returning here.
	//
	// Returning early looks equivalent and is not: it skips the shadow
	// decorator, and the decorator is what writes the ledger row and mints
	// the synthetic PR ref. A suppressed effect that leaves no record is
	// §30.6's named contract violation, and before this bookkeeping
	// existed a shadow repo's CreatePR reached the decorator and WAS
	// recorded -- so an early return here would have quietly removed an
	// entry the ledger used to get.
	//
	// A demotion-cancelled cycle takes the SAME path, for the same reason.
	// §30.4's demotion fix cancels an in-flight push signal because the
	// repo has just become shadow (the ScmCredentialTTL window), so the PR
	// that would have followed is a suppressed effect like any other. It
	// is not a reason to record LESS: an operator reading the ledger to
	// answer "what did this demotion stop" learns nothing from a cycle
	// that vanished.
	stampSaysShadow := (suppressedInShadow != nil && *suppressedInShadow) || pushCancelled
	switch {
	case pushCancelled:
		a.logger.Warn("sessionactor: push_complete arrived for a push cycle cancelled by a repo-demotion sweep; suppressing and recording PR creation (§30.4)")
	case stampSaysShadow:
		a.logger.Info("sessionactor: push_complete arrived for a push cycle whose own persisted decision is shadow; suppressing and recording PR creation (§30.4/§30.8)")
	}

	// The sentinel-auto-fix path is dispatched only AFTER the frozen
	// decision is read, and carries it. It has no human creator to
	// attribute a PR to (sessionRow.CreatedBy is NULL), so it never runs
	// the creator guard or the per-repo loop below -- but that is a reason
	// for a separate PR-BUILDING path, never a reason to be outside the
	// egress gate. Dispatching above the read, as this first did, meant a
	// sentinel fix ignored both the frozen stamp and the demotion
	// cancellation and opened a real pull request with the bot token.
	if provenance.IsSentinelAutoFix(sessionRow.ProvenanceTag) {
		a.createSentinelFixPRBestEffort(ctx, evt, stampSaysShadow)
		return
	}
	// suppressedInShadow == nil (no push cycle was ever recorded for this
	// sandbox -- an honest gap: a push_complete this Step's own new
	// bookkeeping never saw resolved/persisted, e.g. one already in flight
	// across a rollout of this exact change) falls through to the
	// pre-existing, unconditional behavior below, exactly as it always
	// was before this Step.

	if !a.creatorMayGetPRAttribution(ctx, sessionRow.CreatedBy) {
		return // already logged by creatorMayGetPRAttribution
	}

	token, ok := a.decryptCreatorGitHubToken(ctx, sessionRow.CreatedBy)
	if !ok {
		return // already logged by decryptCreatorGitHubToken
	}

	repos, err := reposFromJSON(sessionRow.Repos)
	if err != nil {
		a.logger.Error("sessionactor: parse session repos for PR creation failed", "error", err)
		return
	}
	reposByName := make(map[string]sessionconfig.SessionConfigReposElem, len(repos))
	for _, r := range repos {
		reposByName[r.Name] = r
	}

	title := prTitle(sessionRow)

	for _, pushed := range evt.Repos {
		repoCfg, ok := reposByName[pushed.Name]
		if !ok {
			a.logger.Warn("sessionactor: push_complete named a repo not in this session's own repos", "repo", pushed.Name)
			continue
		}

		// Audit fix (HIGH, cross-batch parity with imageresolve.go's
		// repoAccessAllowedForSpawn and contractdrift.go's
		// checkContractDriftForRepo): checked BEFORE ParseOwnerRepo/CreatePR
		// -- a.sourceControl.CreatePR is a WRITE operation, so silently
		// deriving owner/repo from a non-GitHub repo URL (e.g. a GitLab
		// host, which reposource.ValidateRepoURL accepts -- any https host)
		// and calling the real GitHub-only adapter (production's
		// a.sourceControl, ports.GitHubSourceControlHost's own doc comment)
		// regardless would risk opening a REAL pull request against a
		// coincidentally-matching, completely unrelated GitHub repo using
		// the session creator's real OAuth token. See
		// ports.GitHubSourceControlHost's own doc comment for the full
		// rationale this gate closes everywhere it's applied.
		if err := reposource.CheckRepoHost(repoCfg.Url, ports.SupportedSourceControlHosts()...); err != nil {
			a.logger.Error("sessionactor: create PR: repo url does not name a supported source-control host; skipping this repo",
				"repo", pushed.Name, "url", repoCfg.Url, "error", err)
			continue
		}

		owner, repoName, err := reposource.ParseOwnerRepo(repoCfg.Url)
		if err != nil {
			a.logger.Error("sessionactor: parse owner/repo from clone url for PR creation failed",
				"repo", pushed.Name, "error", err)
			continue
		}

		base, err := a.resolvePRBaseBranch(ctx, owner, repoName, token)
		if err != nil {
			a.logger.Error("sessionactor: resolve PR base branch failed; skipping this repo",
				"repo", pushed.Name, "error", err)
			continue
		}

		prSpec := ports.CreatePRSpec{
			Owner: owner,
			Repo:  repoName,
			Head:  pushed.Branch,
			Base:  base,
			Title: title,
			Body:  prBody(pushed),
			Token: token,
		}
		prCtx, cancel := context.WithTimeout(ctx, a.timeouts.PRCreateTimeout)
		var (
			ref       ports.PRRef
			createErr error
		)
		switch {
		case !stampSaysShadow:
			// The frozen stamp says live. CreatePR still consults the
			// CURRENT mode itself, so a repo demoted live->shadow between
			// push and here is suppressed and recorded by the decorator --
			// the other direction of §30.8's monotone rule, already
			// covered without anything here.
			ref, createErr = a.sourceControl.CreatePR(prCtx, prSpec)
		default:
			suppressor, ok := a.sourceControl.(stampedSuppressor)
			if !ok {
				// Never reachable in production (cmd/control-plane wires
				// the decorator), and loud rather than silent if it ever
				// becomes so: proceeding would open a real pull request
				// for a cycle already decided to be shadow.
				createErr = errNoStampedSuppressor
				break
			}
			ref, createErr = suppressor.SuppressCreatePR(prCtx, prSpec)
		}
		cancel()
		if createErr != nil {
			a.logger.Error("sessionactor: create PR failed", "repo", pushed.Name, "error", createErr)
			continue
		}

		if err := a.recordPRArtifact(ctx, repoName, ref); err != nil {
			a.logger.Error("sessionactor: record PR artifact failed", "repo", pushed.Name, "error", err)
		}

		// §4.1 ("RWX provider + previews", §4.1.2 point 1): best-effort,
		// never blocks -- a repo with no (or only partially) configured RWX
		// preview setting returns immediately with no further work (see
		// that function's own doc comment, previewpr.go). This is the ONE
		// enqueue point for both new outbox kinds (rwx_preview_dispatch,
		// github_preview_link), per that section's own design.
		// Stop at the single hop when the PR was suppressed rather than
		// opened (§30.9's resolved decision, and the claim
		// internal/app/shadowledger's own package doc makes about what a
		// ledger row means).
		//
		// A suppressed CreatePR returns a nil error and a usable-looking
		// ref, so carrying on is the default and it does not announce
		// itself. Everything below would then run against a pull request
		// that does not exist: preview rows enqueued for it, handoff
		// readiness read for it, and whatever state those write. None of
		// it is a leak -- the preview kinds are suppressed by the outbox
		// and the handoff reads are GETs §30.1 already excludes -- but
		// all of it is second-order exercise on a suppressed write, which
		// is exactly what single-hop rules out. The artifact row above IS
		// recorded: it is the direct trace of this one hop.
		if shadowscm.IsSyntheticPRRef(ref) {
			continue
		}

		a.enqueuePreviewBestEffort(ctx, owner, repoName, pushed, ref)

		// §14.4 ("handoff-readiness sentinel", §14.4): best-effort, never
		// blocks -- an ordinary (non-scoped) session's PR returns
		// immediately with no further work (handoffsentinel.go's own top
		// check). See that file's own top comment for why this runs HERE
		// rather than via a GitHub pull_request webhook lane.
		a.runHandoffSentinelBestEffort(ctx, sessionRow, token, owner, repoName, repoCfg.Branch, ref.Number)
	}
}

// createSentinelFixPRBestEffort implements §8.2's own ("sentinels +
// suggestions", §17.2 amendment) fix-PR-creation path: called ONLY for a
// session whose provenance_tag is provenance.SentinelAutoFix
// (createPRBestEffort's own caller check, above) -- a DEDICATED path that
// NEVER calls resolvePRBaseBranch (the amendment's own central
// requirement: "the fix PR's base is never resolved, only assigned"). The
// fix PR's Base is fix.OriginHeadBranch, a LITERAL value captured once, at
// claim time, from the origin session's own repos config
// (reviewverdict.go) -- a stacked PR's base is its parent's head branch,
// never the repository's default.
//
// Also deliberately does NOT call creatorMayGetPRAttribution/
// decryptCreatorGitHubToken: this session has no human creator to check
// or decrypt a token for (sessionRow.CreatedBy is invalid/NULL,
// SpawnChildSession's own doc comment) -- the fix PR is a SYSTEM-INITIATED
// action, bot-attributed via a.githubBotToken (the SAME static credential
// internal/adapters/outbound/githubapi's own BotNotifier/VerdictNotifier
// already authenticate with), mirroring §17.4's own "system-initiated,
// not a delegated human one" framing for the eventual merge -- a
// deliberate design choice this Step's own report names explicitly (no
// other credential source exists for a session with no creator).
//
// Once the fix PR opens successfully, this ALSO (§17.2's own amendment,
// "a second call after creation, never a substitute for it") calls
// RegisterPRStack with the origin+fix PR numbers, bottom to top -- logged
// and otherwise ignored on any failure (§17.2: "never fails the fix-
// session flow"), recording only whether it stuck (sentinel_fixes.
// stack_registered) as an observability signal, never the authority on
// whether registration actually took (§17.6: that authority is always a
// FRESH GetPullRequest.Stack field, checked later, at merge-gating time).
// stampSaysShadow is this turn's own frozen push/PR decision, resolved at
// push send and read by the caller. It is passed in rather than resolved
// here for the same reason the main path does not re-derive it: a repo
// promoted between push send and this event must not turn a shadow-era
// branch into a real pull request.
func (a *Actor) createSentinelFixPRBestEffort(ctx context.Context, evt sandboxws.PushComplete, stampSaysShadow bool) {
	fix, err := a.stores.sentinelFix.GetByFixSession(ctx, a.sessionID)
	if err != nil {
		a.logger.Error("sessionactor: get sentinel_fixes row by fix session failed; skipping fix PR creation", "error", err)
		return
	}

	if a.githubBotToken == "" {
		a.logger.Warn("sessionactor: push_complete arrived for a sentinel-auto-fix session but no bot token is configured; skipping PR creation")
		return
	}

	sessionRow, err := a.stores.session.Get(ctx, a.sessionID)
	if err != nil {
		a.logger.Error("sessionactor: get session for sentinel-fix PR creation failed", "error", err)
		return
	}
	repos, err := reposFromJSON(sessionRow.Repos)
	if err != nil {
		a.logger.Error("sessionactor: parse session repos for sentinel-fix PR creation failed", "error", err)
		return
	}
	reposByName := make(map[string]sessionconfig.SessionConfigReposElem, len(repos))
	for _, r := range repos {
		reposByName[r.Name] = r
	}

	for _, pushed := range evt.Repos {
		repoCfg, ok := reposByName[pushed.Name]
		if !ok {
			a.logger.Warn("sessionactor: sentinel-fix push_complete named a repo not in this session's own repos", "repo", pushed.Name)
			continue
		}

		if err := reposource.CheckRepoHost(repoCfg.Url, ports.SupportedSourceControlHosts()...); err != nil {
			a.logger.Error("sessionactor: create sentinel-fix PR: repo url does not name a supported source-control host; skipping this repo",
				"repo", pushed.Name, "url", repoCfg.Url, "error", err)
			continue
		}

		owner, repoName, err := reposource.ParseOwnerRepo(repoCfg.Url)
		if err != nil {
			a.logger.Error("sessionactor: parse owner/repo from clone url for sentinel-fix PR creation failed",
				"repo", pushed.Name, "error", err)
			continue
		}

		prSpec := ports.CreatePRSpec{
			Owner: owner,
			Repo:  repoName,
			Head:  pushed.Branch,
			// NEVER resolvePRBaseBranch -- this literal, already-captured
			// value IS the point of this dedicated code path (this
			// function's own doc comment).
			Base:  fix.OriginHeadBranch,
			Title: "Sentinel auto-fix: " + prTitle(sessionRow),
			Body:  fmt.Sprintf("Automated sentinel-auto-fix remediation (Narvi, §17) for pull request #%d, branch %s.", fix.OriginPrNumber, pushed.Branch),
			Token: a.githubBotToken,
		}
		prCtx, cancel := context.WithTimeout(ctx, a.timeouts.PRCreateTimeout)
		var (
			ref       ports.PRRef
			createErr error
		)
		switch {
		case !stampSaysShadow:
			// CreatePR still consults the CURRENT mode itself, so a repo
			// demoted between push and here is suppressed and recorded by
			// the decorator -- the other direction of §30.8's monotone
			// rule, needing nothing here.
			ref, createErr = a.sourceControl.CreatePR(prCtx, prSpec)
		default:
			suppressor, ok := a.sourceControl.(stampedSuppressor)
			if !ok {
				createErr = errNoStampedSuppressor
				break
			}
			ref, createErr = suppressor.SuppressCreatePR(prCtx, prSpec)
		}
		cancel()
		if createErr != nil {
			a.logger.Error("sessionactor: create sentinel-fix PR failed", "repo", pushed.Name, "error", createErr)
			continue
		}

		if err := a.recordPRArtifact(ctx, repoName, ref); err != nil {
			a.logger.Error("sessionactor: record sentinel-fix PR artifact failed", "repo", pushed.Name, "error", err)
		}

		if _, err := a.stores.sentinelFix.UpdateOpened(ctx, fix.ID, int32(ref.Number)); err != nil {
			a.logger.Error("sessionactor: update sentinel_fixes with opened fix PR failed", "error", err)
		}
		if _, err := a.stores.reviewFinding.MarkFixOpen(ctx, a.sessionID, int32(ref.Number)); err != nil {
			a.logger.Warn("sessionactor: mark review findings fix_open failed", "error", err)
		}

		// Registering the stack is a SECOND call, made only now that both
		// PRs exist (§17.2) -- best-effort, log-and-ignore.
		stackCtx, stackCancel := context.WithTimeout(ctx, a.timeouts.PRCreateTimeout)
		registerErr := a.sourceControl.RegisterPRStack(stackCtx, ports.RegisterPRStackSpec{
			Owner:     owner,
			Repo:      repoName,
			PRNumbers: []int{int(fix.OriginPrNumber), ref.Number},
			Token:     a.githubBotToken,
		})
		stackCancel()
		if registerErr != nil {
			a.logger.Warn("sessionactor: register pr stack failed (logged and ignored, per §17.2)", "error", registerErr)
		}
		if _, err := a.stores.sentinelFix.UpdateStackRegistered(ctx, fix.ID, registerErr == nil); err != nil {
			a.logger.Warn("sessionactor: record stack-registered observability flag failed", "error", err)
		}
	}
}

// recordPRArtifact persists a successfully created PR as a "pr"-typed
// artifact row, inside its OWN small, fresh transact -- mirroring
// dispatch.go's own executeSpawn shape exactly ("a real network call
// already happened outside any transaction; a separate, small transact
// now records its outcome"). Deliberately still epoch-fenced by transact
// (the same mechanism every other write in this package gets) even though
// an artifact row is not session/turn/sandbox state -- transact's own
// fencing check does not care what a caller's fn writes, only that the
// actor invoking it is still the legitimate owner of the session.
// recordPRArtifact inserts a "pr"-typed artifact row for ref, unless one
// already exists for this session (a confirmed-finding fix,
// companion to CreatePR's own new idempotency, githubapi/adapter.go):
// making CreatePR idempotent means createPRBestEffort's per-repo loop now
// "succeeds" (recovering the SAME PR) on turn 2+ instead of erroring, so
// without this guard the same PR would gain one duplicate artifact row per
// subsequent completed turn. No new migration/unique constraint --an
// application-level guard, the same idempotency idiom the outbox/claim
// paths elsewhere in this codebase already use, and cheap: this list is
// expected to stay small (ArtifactStore.ListForSession's own doc comment).
func (a *Actor) recordPRArtifact(ctx context.Context, repoName string, ref ports.PRRef) error {
	metadata, err := json.Marshal(map[string]any{"repo": repoName, "number": ref.Number})
	if err != nil {
		return fmt.Errorf("sessionactor: marshal pr artifact metadata: %w", err)
	}

	return a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		existing, err := a.stores.artifact.WithTx(tx).ListForSession(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: list existing artifacts: %w", err)
		}
		for _, art := range existing {
			if art.Type == sqlcgen.ArtifactTypePr && art.Url == ref.URL {
				return nil
			}
		}

		if _, err := a.stores.artifact.WithTx(tx).Create(ctx, sqlcgen.CreateArtifactParams{
			SessionID: a.sessionID,
			Type:      sqlcgen.ArtifactTypePr,
			Url:       ref.URL,
			Metadata:  metadata,
		}); err != nil {
			return fmt.Errorf("sessionactor: insert pr artifact: %w", err)
		}
		return nil
	})
}

// prTitle uses the session's own title if it has one (set at creation,
// restdtos.CreateSessionRequest.title), falling back to a generic,
// clearly-labeled title otherwise -- sessions created with no title are
// common (title is nullable) and must still produce a valid, non-empty PR
// title.
func prTitle(sessionRow sqlcgen.Session) string {
	if sessionRow.Title != nil && *sessionRow.Title != "" {
		return *sessionRow.Title
	}
	return "Narvi session " + sessionRow.ID.String()
}

// prBody builds a minimal, honest PR description -- this Step invents no
// richer changelog/summary mechanism than "which branch, which commit".
func prBody(pushed sandboxws.PushCompleteReposElem) string {
	return fmt.Sprintf("Automated changes from a Narvi session (branch %q, commit %s).", pushed.Branch, pushed.Sha)
}

// parseOwnerRepo used to live here as a byte-for-byte fork of
// internal/app/imagebuild/builder.go's own identical helper -- audit-
// remediation batch B3 moved both into
// internal/domain/reposource.ParseOwnerRepo (which already owns
// ValidateRepoURL/ValidateBranch, i.e. this codebase's one home for
// repo-URL parsing knowledge) and deleted both forks. See that function's
// own doc comment for the full rationale; this call site now uses it
// directly, above.
