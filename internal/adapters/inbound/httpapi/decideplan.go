// This file (decideplan.go) implements §8.1's ("plan mode,
// cross-channel", §8.1/§13.3) own central deliverable: the shared,
// transport-agnostic "decide a plan" function every entry point (the
// existing REST approve/reject endpoints, planapprove.go; the new Slack
// interactivity block_actions handler, internal/adapters/inbound/slack/
// interactive.go; the new Linear text-verdict parsing,
// internal/adapters/inbound/linear/webhook.go's handlePrompted) calls
// identically -- so "first verdict wins" (the plans_one_awaiting_approval_
// per_session partial unique index, migrations/000034_plan_mode.up.sql)
// and "notify the other channels" are each implemented exactly ONCE, not
// once per caller.
//
// Mirrors create.go's own CreateSessionCore/CreateSessionOnTx split
// exactly, for the exact same reason: DecidePlanOnTx takes an
// ALREADY-OPEN transaction (so a caller that has already locked the
// session row for its own authorization check -- the REST handlers,
// planapprove.go -- can decide inline, on that SAME connection, rather
// than opening a second, simultaneous one out of the pool) and never
// finalizes it; DecidePlan is the pool-based wrapper every OTHER caller
// (Slack, Linear -- neither has an open transaction of its own yet) uses.
//
// DecidePlanOnTx ALWAYS (re-)acquires the session row's own lock itself
// (GetActorEpochForUpdate), regardless of whether the caller already holds
// it -- harmless and idempotent within the SAME transaction (mirrors
// CreateSessionOnTx's own "validate again... deliberate, harmless"
// tolerance for the identical kind of redundancy), and is what lets every
// caller -- one that already locked for its own authorization check, and
// one that never had a reason to lock at all -- call this function
// identically.
//
// # Authorization is NOT this function's job -- its CALLERS' is
//
// DecidePlanOnTx/DecidePlan themselves never call canActOnPlan/authz.
// Authorize -- every caller is expected to have already rendered that
// verdict BEFORE ever reaching this function, exactly like
// planapprove.go's own REST handlers (authorizePlanAction -> canActOnPlan,
// planauthz.go) always have.
//
// # §13.2 ("identities + full RBAC") correction: Slack/Linear verdicts
// NOW go through the same matrix too
//
// An EARLIER version of this doc comment claimed Slack/Linear verdicts
// would stay unauthenticated-per-user "until identity auto-linking exists,
// which is out of this Step's own scope" -- that was wrong: the work that
// supplies identity auto-linking (§13.2) is exactly this one, so
// it is also the one that closes this gap, not a future one. A confirmed
// security review caught the contradiction: Slack's interactive.go
// (decideAndUpdateMessage) and Linear's webhook.go (handlePlanVerdict)
// were both already resolving a REAL, auto-linked decidedBy by this point
// in §13.2's own work, yet neither called Authorize on it before
// reaching this function -- letting a linked `viewer` (or an unowned
// `member`) decide a plan via Slack/Linear when the identical REST call
// would have been rejected, directly contradicting §13.3's own
// "channel-agnostic" requirement this file's own doc comment already
// promised above.
//
// Both callers now run a domain/authz.Authorize(ActionApprovePlan, ...)
// check of their own (mirroring canActOnPlan's own "resolve own/joined,
// then Authorize" shape) BEFORE ever calling DecidePlan/DecidePlanOnTx --
// see internal/adapters/inbound/slack's own InteractiveDeps.
// authorizeSessionAction (interactive.go) and internal/adapters/inbound/
// linear's own identical Deps.authorizeSessionAction (identity.go). A
// still-unlinked (bot-attributed, Valid == false) decidedBy is UNCHANGED:
// it still proceeds with no per-user gate at all, exactly matching §13.2's
// own explicit "unlinked actors get bot attribution ... the action
// proceeds" precedent for that case -- this fix closes the gap for a
// RESOLVED, linked actor's role specifically, not the not-yet-linked case.
//
// DecidePlanOnTx itself also writes an audit_log row (§13.3) on every
// winning decision, on the SAME tx, for EVERY caller alike -- REST (a real
// decidedBy) and Slack/Linear (a real decidedBy once linked, otherwise an
// invalid, bot-attributed one) all get one, actor_user_id NULL only for
// the still-unlinked case, mirroring decidedBy's own existing
// NULL-for-bot convention. The audit write is unconditional on winning the
// decision, regardless of who decided or how they were authorized -- it
// is a record of WHAT changed, not a second authorization gate, and it
// never runs at all for a call this function never reaches (i.e. one its
// own caller already denied upstream).

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// PlanVerdict is the decision DecidePlan/DecidePlanOnTx renders.
type PlanVerdict string

// The two verdicts a plan can be decided with -- PlanVerdictApprove flips
// the plan to 'approved' and dispatches the new implementation turn;
// PlanVerdictReject flips it to 'rejected' with no new turn.
const (
	PlanVerdictApprove PlanVerdict = "approve"
	PlanVerdictReject  PlanVerdict = "reject"
)

// ErrPlanOpenTurnInFlight is returned by DecidePlanOnTx/DecidePlan when an
// Approve verdict is refused because another turn for this session hasn't
// yet reached a terminal state -- the exact same gate ApprovePlan's own top
// doc comment (planapprove.go) already documents in full; callers map this
// to whatever their own transport's "not right now, try again shortly"
// shape is (409 for REST; a plain honest reply for Slack/Linear -- neither
// currently implements a retry surface for this narrow race, matching
// today's existing REST-only behavior).
var ErrPlanOpenTurnInFlight = errors.New("httpapi: a turn is already pending, dispatched, or processing for this session")

// DecidePlanOutcome is DecidePlan/DecidePlanOnTx's uniform,
// transport-agnostic result.
type DecidePlanOutcome struct {
	// Won reports whether THIS call performed the transition. false means
	// the plan was already approved/rejected/superseded by the time this
	// call's own guarded UPDATE ran (by any entry point, possibly
	// including a concurrent call from this SAME entry point) -- see
	// FinalStatus for the plan's real, current status either way.
	Won bool

	// FinalStatus is the plan's ACTUAL status string (mirrors
	// plandomain.Status's own values: "approved"/"rejected"/"superseded")
	// after this call: the verdict this call itself just rendered, if Won;
	// otherwise whatever it already was. Empty only in the defensive,
	// should-be-unreachable case where planID does not name any row of
	// sessionID's own -- callers treat that identically to "already
	// decided" (a plain, honest "no such awaiting plan", never a 404 that
	// implies the SESSION itself is missing).
	FinalStatus string

	// TurnID is the new implementation turn's id, set iff Won && the
	// rendered verdict was Approve.
	TurnID *string
}

// planDecisionOutcomeText builds the short, human-readable line every
// cross-channel notification (this Step's own point 6) carries -- shared
// so the Slack chat.update text and the Linear follow-up AgentActivity
// text are byte-for-byte the same wording for the same outcome.
func planDecisionOutcomeText(verdict PlanVerdict) string {
	switch verdict {
	case PlanVerdictApprove:
		return "Plan approved — implementation started."
	case PlanVerdictReject:
		return "Plan rejected."
	default:
		return "Plan decided."
	}
}

// DecidePlanOnTx renders verdict on planID (which must belong to
// sessionRow.ID) inside the caller's own already-open transaction tx --
// see this file's own top doc comment for the full CreateSessionOnTx-
// mirroring contract (tx is never committed/rolled back here; the caller
// owns that entirely) and for why authorization is deliberately NOT this
// function's job.
//
// decidedBy is the acting user id, or an explicitly INVALID pgtype.UUID
// (Valid == false) for a bot/channel-attributed decision (Slack button
// click, Linear text reply) -- mirrors sessions.created_by's own existing
// NULL-for-bot convention exactly (this Step's own brief).
//
// Sequencing: lock the session row (see top doc comment on why this is
// unconditional) -> for Approve only, the SAME hasOpenTurn 409 gate
// ApprovePlan's own top doc comment describes (ErrPlanOpenTurnInFlight) ->
// the guarded conditional UPDATE (plans.ApproveIfAwaitingApproval/
// RejectIfAwaitingApproval) -> re-fetch the plan row (to learn its REAL
// current state either way, and -- on a win -- its own stored Slack message
// ref) -> on a win: for Approve, insert the new implementation turn exactly
// as ApprovePlan always has; either way, enqueue this Step's own
// cross-channel-notify outbox rows (enqueuePlanDecisionNotifications
// below), inside this SAME transaction, so they are visible if and only if
// the whole decision itself commits.
// epistemicCheckDefault (F6, adversarial review) is a REQUIRED
// parameter, exactly mirroring createTurnLocked's own identical parameter
// (turn.go's own doc comment on why) -- closes F6's own verified gap: the
// Approve verdict's own implementation-turn insert below used to bypass
// turn.MaybeInjectEpistemicPreamble entirely (dispatched directly via
// turns.Create, never createTurnLocked/CreateTurnCore, mirroring
// workflowengine's own dispatchNextAttempt precedent, this function's own
// doc comment), so the post-plan-approval turn that actually edits files
// never got the devil's-advocate preamble regardless of platform/session
// config. F6's own decision overrules the argument that plan mode's HITL
// approval already covers this turn: a human approves a PLAN, not each
// premise the implementation rests on while carrying it out -- exactly
// what this check exists to catch. Every caller passes its own real,
// operator-configured platform.Config.EpistemicCheckDefault: DecidePlanOnTx
// is reached only for an ordinary (never review-session) plan-mode
// session, so no F7-style hardcoded-false carve-out applies here.
func DecidePlanOnTx(
	ctx context.Context,
	tx pgx.Tx,
	sessions *postgres.SessionStore,
	turns *postgres.TurnStore,
	plans *postgres.PlanStore,
	events *postgres.EventStore,
	planDocuments *postgres.PlanDocumentStore,
	outbox *postgres.OutboxStore,
	linearAgentSessions *postgres.LinearAgentSessionStore,
	auditLog *postgres.AuditLogStore,
	sessionRow sqlcgen.Session,
	planID pgtype.UUID,
	verdict PlanVerdict,
	decidedBy pgtype.UUID,
	epistemicCheckDefault bool,
) (DecidePlanOutcome, error) {
	logger := platform.Logger(ctx)

	if _, err := sessions.WithTx(tx).GetActorEpochForUpdate(ctx, sessionRow.ID); err != nil {
		return DecidePlanOutcome{}, fmt.Errorf("httpapi: lock session row for plan decision: %w", err)
	}

	if verdict == PlanVerdictApprove {
		existingTurns, err := turns.WithTx(tx).ListForSession(ctx, sessionRow.ID)
		if err != nil {
			return DecidePlanOutcome{}, fmt.Errorf("httpapi: list turns for plan-decision open-turn check: %w", err)
		}
		if hasOpenTurn(existingTurns) {
			return DecidePlanOutcome{}, ErrPlanOpenTurnInFlight
		}
	}

	var rowsAffected int64
	var err error
	var trig plandomain.Trigger
	switch verdict {
	case PlanVerdictApprove:
		trig = plandomain.TriggerApprove
		rowsAffected, err = plans.WithTx(tx).ApproveIfAwaitingApproval(ctx, planID, sessionRow.ID, decidedBy)
	case PlanVerdictReject:
		trig = plandomain.TriggerReject
		rowsAffected, err = plans.WithTx(tx).RejectIfAwaitingApproval(ctx, planID, sessionRow.ID, decidedBy)
	default:
		return DecidePlanOutcome{}, fmt.Errorf("httpapi: unrecognized plan verdict %q", verdict)
	}
	if err != nil {
		return DecidePlanOutcome{}, fmt.Errorf("httpapi: guarded plan decision update: %w", err)
	}

	planRow, err := plans.WithTx(tx).Get(ctx, planID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Defensive: a stale/wrong plan id. Reported identically to
			// "already decided" (Won=false, FinalStatus empty) -- never a
			// hard error, matching planapprove.go's own pre-existing
			// "already decided, or a stale id" 409 message for this exact
			// case.
			return DecidePlanOutcome{Won: false}, nil
		}
		return DecidePlanOutcome{}, fmt.Errorf("httpapi: re-fetch plan after decision: %w", err)
	}
	if planRow.SessionID != sessionRow.ID {
		// Defensive, security-relevant: this re-fetch is BY PLAN ID ALONE
		// (plans.Get has no session_id filter), unlike the guarded UPDATE
		// above which IS correctly scoped to (planID, sessionRow.ID) and so
		// would already have affected 0 rows for a cross-session planID
		// (rowsAffected == 0, won == false below regardless). Without this
		// check, a caller supplying a planID that exists but belongs to a
		// DIFFERENT session (a forged/replayed Slack button value, a Linear
		// lookup bug, a malformed REST call, simple data confusion
		// elsewhere) would still have this re-fetch succeed and leak that
		// OTHER session's real, current plan status into FinalStatus --
		// which callers render straight into caller-facing text (e.g.
		// "already decided: <status>"). Treated EXACTLY like the
		// pgx.ErrNoRows case immediately above: never leak the mismatched
		// row's real status, just report "no such awaiting plan for THIS
		// session" (Won=false, FinalStatus empty).
		logger.Warn("httpapi: decide plan: planID belongs to a different session than the caller's own; refusing to leak its status", "plan_id", planID.String(), "requested_session_id", sessionRow.ID.String(), "actual_session_id", planRow.SessionID.String())
		return DecidePlanOutcome{Won: false}, nil
	}

	won := rowsAffected > 0
	outcome := DecidePlanOutcome{Won: won, FinalStatus: string(planRow.Status)}

	// Defense-in-depth (audit-fix batch, L10): sanity-check the guarded
	// SQL UPDATE's own real outcome (won, planRow.Status -- the plan's
	// CURRENT status, re-fetched above and already confirmed to belong to
	// THIS session by the mismatch check above) against internal/domain/
	// plan's own Transition table. The guarded UPDATE remains the sole
	// authority for whether the decision itself took effect -- this check
	// can only ever log a loud, should-be-unreachable mismatch; it never
	// changes won/outcome itself.
	//
	// won == true means the UPDATE's own "AND status = 'awaiting_approval'"
	// clause matched, so the plan's status immediately before this call was
	// necessarily StatusAwaitingApproval -- no extra read is needed to know
	// the "from" status for this branch. won == false means that guard did
	// NOT match, so planRow.Status (unaffected by our own UPDATE, and
	// unable to have changed from under us: sessionRow's own actor-epoch
	// row lock taken above is held for this transaction's whole duration,
	// and every other writer of this session's own plan rows -- planrecord.
	// go's own Supersede call -- takes that SAME lock before ever touching
	// a plan row) must already be a status with no legal outgoing edge for
	// trig.
	if won {
		wantStatus, transErr := plandomain.Transition(plandomain.StatusAwaitingApproval, trig)
		if transErr != nil || plandomain.Status(planRow.Status) != wantStatus {
			logger.Error("httpapi: decide plan: domain transition sanity check mismatch: guarded SQL update won but domain model disagrees with the resulting status",
				"plan_id", planID.String(), "session_id", sessionRow.ID.String(), "trigger", trig.String(), "want_status", string(wantStatus), "actual_status", string(planRow.Status), "domain_error", transErr)
		}
	} else if _, transErr := plandomain.Transition(plandomain.Status(planRow.Status), trig); transErr == nil {
		logger.Error("httpapi: decide plan: domain transition sanity check mismatch: guarded SQL update affected no rows but domain model deems the transition legal from the plan's current status",
			"plan_id", planID.String(), "session_id", sessionRow.ID.String(), "trigger", trig.String(), "current_status", string(planRow.Status))
	}

	if !won {
		return outcome, nil
	}

	if verdict == PlanVerdictApprove {
		// §31.3's durability fix: snapshot the approved version's own prose
		// into plan_documents FIRST, in this SAME transaction as the
		// guarded UPDATE above that just flipped this row to 'approved' --
		// before anything else this branch does, including dispatching the
		// implementation turn below. See snapshotApprovedPlanContent's own
		// doc comment for why a failure here is propagated (never logged
		// and continued, unlike enqueuePlanDecisionNotifications' own
		// best-effort notify below): this call's only job is making "an
		// approved plan with no durable snapshot" unrepresentable, so it
		// must abort this whole decision -- and, via the caller's own
		// tx.Rollback, the guarded UPDATE with it -- rather than let either
		// one commit without the other.
		if err := snapshotApprovedPlanContent(ctx, tx, turns, events, planDocuments, sessionRow.ID, planRow); err != nil {
			return DecidePlanOutcome{}, fmt.Errorf("httpapi: snapshot approved plan content: %w", err)
		}

		// F6 (adversarial review): the SAME shared gate
		// createTurnLocked/CreateSessionOnTx/dispatchNextAttempt also route
		// through (internal/domain/turn.MaybeInjectEpistemicPreamble).
		// planMode is passed literally false, matching CreateTurnParams.
		// PlanMode below -- this is deliberately the POST-approval
		// implementation turn, never itself a plan-mode turn.
		prompt := turn.MaybeInjectEpistemicPreamble(epistemicCheckDefault, sessionRow.EpistemicCheckEnabled, false, implementPlanPrompt)
		createdTurn, err := turns.WithTx(tx).Create(ctx, sqlcgen.CreateTurnParams{
			SessionID: sessionRow.ID,
			Status:    sqlcgen.TurnStatusPending,
			Prompt:    &prompt,
			ModelID:   sessionRow.BuildModelID,
			// H2 (adversarial review, §29.8): sessions.build_effort must be
			// copied onto this approval-dispatched implementation turn
			// exactly as build_model_id -> model_id already is on the line
			// above (migration 000063's own doc comment) -- mirroring
			// workflowengine/advance.go's own dispatchNextAttempt, which
			// already does this (Effort: res.Effort, itself resolved from
			// sessionRow.BuildEffort). Before this fix, this was the ONE
			// dispatch path that dropped it, making build_effort write-only
			// on a plain (non-workflow) plan-mode session -- §29.8 forbids a
			// dispatch-time session-level fallback for a NULL turn effort,
			// so there was no rescue.
			Effort:   sessionRow.BuildEffort,
			PlanMode: false,
		})
		if err != nil {
			return DecidePlanOutcome{}, fmt.Errorf("httpapi: create implementation turn: %w", err)
		}
		turnIDStr := createdTurn.ID.String()
		outcome.TurnID = &turnIDStr
	}

	// Audit-fix batch (completeness/observability, M2 part 1): the created
	// implementation turn's own id (outcome.TurnID, just computed above for
	// an Approve verdict) is included in this audit row's own detail JSON
	// too -- previously omitted despite being available at this exact point
	// in this same function. The key is present only when a turn was
	// actually created (Approve); a Reject verdict never creates one, so
	// its own detail JSON carries no turn_id key at all, rather than an
	// always-present-but-sometimes-null one.
	detail := map[string]any{
		"session_id": sessionRow.ID.String(),
		"verdict":    string(verdict),
	}
	if outcome.TurnID != nil {
		detail["turn_id"] = *outcome.TurnID
	}
	if err := recordAuditLog(ctx, auditLog.WithTx(tx), decidedBy, "plan."+string(verdict), "plan", planID.String(), detail); err != nil {
		return DecidePlanOutcome{}, fmt.Errorf("httpapi: record plan decision audit log: %w", err)
	}

	if err := enqueuePlanDecisionNotifications(ctx, tx, outbox, linearAgentSessions, sessionRow, planRow, planDecisionOutcomeText(verdict)); err != nil {
		// Logged, not propagated: the decision itself (the guarded UPDATE
		// above, already durable once this transaction commits) must never
		// be rolled back merely because notifying an already-DECIDED
		// outcome to another channel failed to enqueue -- that would
		// re-litigate an already-final verdict over a notification
		// side-effect, exactly backwards from §5.1's own outbox philosophy
		// ("written in the same tx as the state change" describes
		// durability of the ENQUEUE, not a reason to fail the change
		// itself). A failed enqueue here is a real gap (the other
		// channel's message never gets its outcome update) -- logged
		// loudly so it is visible in practice; not built as a retried
		// background reconciliation, out of this Step's own scope.
		logger.Error("httpapi: enqueue plan decision cross-channel notifications failed", "error", err, "plan_id", planID.String())
	}

	return outcome, nil
}

// snapshotApprovedPlanContent recovers planRow's own approved prose (the
// SAME bounded events-log scan the plan-mode UI and the Slack/Linear
// notifiers already use, plandomain.ExtractContent via turnContentBounds,
// plans.go) and durably persists it into plan_documents, on tx -- §31.3's
// durability fix: an approved plan's prose used to live ONLY in the events
// log, which cascades away with its session exactly like plans' own
// session_id does (see migrations/000112_plan_documents.up.sql's own
// comment for the full "why").
//
// Called from DecidePlanOnTx's own PlanVerdictApprove branch, ONLY once
// the guarded UPDATE has already won, BEFORE that branch does anything
// else (including dispatching the implementation turn) -- see that call
// site's own comment for why a returned error here is propagated, never
// logged-and-continued: it must abort the whole decision, undoing the
// guarded UPDATE via the caller's own tx.Rollback, so "approved" and "has
// a durable snapshot" can never come apart.
//
// Content EXTRACTION itself stays exactly as best-effort as every existing
// caller of plandomain.ExtractContent (sessionactor.planContentText,
// plans.go's own planContentMap): an events-log read hiccup, or a
// producing turn turnContentBounds cannot find (should be unreachable --
// plans.turn_id is a NOT NULL FK to an already-dispatched turn by the time
// a plan row exists at all), degrades to plandomain.ContentFallbackText
// rather than failing the approval. Only the WRITE into plan_documents
// itself is strict: a plan approved through this path always gets a row
// here, and that row's content is never LESS honest than what every other
// reader of this same log has always shown a human.
//
// turns is read via tx (turns.WithTx(tx).ListForSession): sessionRow's own
// row lock (GetActorEpochForUpdate, taken earlier in DecidePlanOnTx) is
// what makes "every turn dispatched in this session so far" a stable
// snapshot for the rest of this transaction's duration, mirroring
// DecidePlanOnTx's own pre-existing hasOpenTurn fetch immediately above.
// events is read via the POOL-based EventStore, never WithTx --
// EventStore's own doc comment ("no such transactional requirement...
// never WithTx") and sessionactor.planContentText's own identical
// precedent: every event this scan can possibly find was already
// committed by the producing turn's own terminal-state write, long before
// this approval could even begin.
func snapshotApprovedPlanContent(ctx context.Context, tx pgx.Tx, turns *postgres.TurnStore, events *postgres.EventStore, planDocuments *postgres.PlanDocumentStore, sessionID pgtype.UUID, planRow sqlcgen.Plan) error {
	sessionTurns, err := turns.WithTx(tx).ListForSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("httpapi: list turns for plan snapshot: %w", err)
	}

	content := plandomain.ContentFallbackText
	if lower, upper, ok := turnContentBounds(sessionTurns, planRow.TurnID); ok {
		recentEvents, err := events.ListRecentForSession(ctx, sessionID, planContentEventFetchLimit)
		if err != nil {
			return fmt.Errorf("httpapi: list events for plan snapshot: %w", err)
		}
		content = plandomain.ExtractContent(sessionactor.ToContentEvents(recentEvents), lower, upper)
	}

	if _, err := planDocuments.WithTx(tx).Create(ctx, planRow.ID, content); err != nil {
		return fmt.Errorf("httpapi: create plan document snapshot: %w", err)
	}
	return nil
}

// enqueuePlanDecisionNotifications implements this Step's own point 6
// ("notify the other channels"): if planRow carries a stored Slack message
// ref (slack_channel_id/slack_message_ts -- only ever set when the outbox's
// own Slack plan-approval notifier successfully posted THIS plan version's
// approval-request message), ALWAYS enqueue a
// ports.NotificationKindSlackPlanDecided row reflecting outcomeText,
// regardless of which channel actually rendered this decision (update-to-
// self is a harmless, no-op-shaped confirmation; update-to-a-different-
// channel is the real "notify" case -- this function does not need to know
// which). Likewise, if sessionRow is Linear-origin, ALWAYS enqueue a plain
// ports.NotificationKindLinear row (reusing the EXISTING kind/payload
// §5.1 already built -- no new Linear-specific kind needed, see this Step's
// own design note) describing the same outcome as a follow-up
// AgentActivity. A session can only ever be Slack-origin XOR Linear-origin
// (or web/GitHub) -- sessions.spawn_source is a single value -- so in
// practice at most one of these two branches ever fires for any given
// session; both are still checked unconditionally, since the ONLY signal
// this function needs is "does a delivery target exist for this plan/
// session", not "which one specific channel did the deciding".
func enqueuePlanDecisionNotifications(
	ctx context.Context,
	tx pgx.Tx,
	outbox *postgres.OutboxStore,
	linearAgentSessions *postgres.LinearAgentSessionStore,
	sessionRow sqlcgen.Session,
	planRow sqlcgen.Plan,
	outcomeText string,
) error {
	logger := platform.Logger(ctx)

	// Correlation ID propagation (Batch 11 audit-fix scope, extended here
	// for consistency): this function is DecidePlanOnTx's own second
	// outbox-enqueue call site (alongside sessionactor/outboxenqueue.go's
	// enqueueOutboxNotification), reachable from the REST approve/reject
	// endpoints, Slack's block_actions handler, and Linear's text-verdict
	// handler -- every one of which runs inside a real request/webhook ctx.
	// Mirrors internal/app/auditlog.Record's own "read from ctx if present,
	// else NULL" convention exactly, so a plan-decision notification's own
	// correlation_id is no less complete than a turn-completion
	// notification's.
	var correlationID *string
	if id, ok := platform.CorrelationIDFromContext(ctx); ok && id != "" {
		correlationID = &id
	}

	if planRow.SlackChannelID != nil && planRow.SlackMessageTs != nil && *planRow.SlackChannelID != "" && *planRow.SlackMessageTs != "" {
		payload, err := json.Marshal(slackapi.PlanDecidedPayload{
			ChannelID: *planRow.SlackChannelID,
			MessageTS: *planRow.SlackMessageTs,
			Text:      outcomeText,
		})
		if err != nil {
			return fmt.Errorf("marshal slack plan-decided payload: %w", err)
		}
		if _, err := outbox.WithTx(tx).Create(ctx, sqlcgen.CreateOutboxEntryParams{
			SessionID:     sessionRow.ID,
			Kind:          string(ports.NotificationKindSlackPlanDecided),
			Payload:       payload,
			CorrelationID: correlationID,
		}); err != nil {
			return fmt.Errorf("enqueue slack plan-decided outbox entry: %w", err)
		}
	}

	if sessionRow.SpawnSource == sqlcgen.SessionSpawnSourceLinear {
		row, err := linearAgentSessions.WithTx(tx).GetBySessionID(ctx, sessionRow.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Defensive -- see outboxenqueue.go's own identical
				// "should be unreachable in practice" note for the generic
				// turn-completion notification; never fatal to the
				// decision itself.
				logger.Warn("httpapi: linear-origin session has no linear_agent_sessions row; skipping plan-decided notify", "session_id", sessionRow.ID.String())
			} else {
				return fmt.Errorf("get linear agent session for plan-decided notify: %w", err)
			}
		} else {
			payload, err := json.Marshal(linearapi.Payload{
				AgentSessionID: row.AgentSessionID,
				OrganizationID: row.OrganizationID,
				Text:           outcomeText,
				Success:        true, // a rendered plan decision (approve or reject) is a normal outcome, never an "error" activity
			})
			if err != nil {
				return fmt.Errorf("marshal linear plan-decided payload: %w", err)
			}
			if _, err := outbox.WithTx(tx).Create(ctx, sqlcgen.CreateOutboxEntryParams{
				SessionID:     sessionRow.ID,
				Kind:          string(ports.NotificationKindLinear),
				Payload:       payload,
				CorrelationID: correlationID,
			}); err != nil {
				return fmt.Errorf("enqueue linear plan-decided outbox entry: %w", err)
			}
		}
	}

	return nil
}

// DecidePlan is the pool-based wrapper every caller with NO already-open
// transaction of its own uses (Slack's block_actions handler; Linear's
// handlePrompted keyword match) -- mirrors CreateSessionCore's own
// identical "own a single transaction start-to-finish, then trigger
// post-commit dispatch" shape exactly.
//
// A caller that is ALREADY holding an open transaction (e.g. one that took
// the session row's lock for its own authorization check first -- the REST
// approve/reject handlers, planapprove.go) must NOT call DecidePlan: doing
// so would open a SECOND, simultaneous connection out of the same pool
// while the first transaction's own connection is still held. That caller
// calls DecidePlanOnTx directly, inline on its own already-open tx, and
// calls TriggerDispatch itself once its own outer transaction has committed
// and outcome.Won && verdict == PlanVerdictApprove.
func DecidePlan(
	ctx context.Context,
	pool *pgxpool.Pool,
	sessions *postgres.SessionStore,
	turns *postgres.TurnStore,
	plans *postgres.PlanStore,
	events *postgres.EventStore,
	planDocuments *postgres.PlanDocumentStore,
	outbox *postgres.OutboxStore,
	linearAgentSessions *postgres.LinearAgentSessionStore,
	auditLog *postgres.AuditLogStore,
	registry *sessionactor.Registry,
	sessionID, planID pgtype.UUID,
	verdict PlanVerdict,
	decidedBy pgtype.UUID,
	epistemicCheckDefault bool,
) (DecidePlanOutcome, error) {
	logger := platform.Logger(ctx)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return DecidePlanOutcome{}, fmt.Errorf("httpapi: begin decide-plan tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	sessionRow, err := sessions.WithTx(tx).Get(ctx, sessionID)
	if err != nil {
		return DecidePlanOutcome{}, fmt.Errorf("httpapi: get session for plan decision: %w", err)
	}

	outcome, err := DecidePlanOnTx(ctx, tx, sessions, turns, plans, events, planDocuments, outbox, linearAgentSessions, auditLog, sessionRow, planID, verdict, decidedBy, epistemicCheckDefault)
	if err != nil {
		return DecidePlanOutcome{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return DecidePlanOutcome{}, fmt.Errorf("httpapi: commit decide-plan tx: %w", err)
	}

	if outcome.Won && verdict == PlanVerdictApprove {
		TriggerDispatch(ctx, registry, sessionID)
	}

	logger.Info("httpapi: decided plan", "plan_id", planID.String(), "session_id", sessionID.String(), "verdict", string(verdict), "won", outcome.Won, "final_status", outcome.FinalStatus)
	return outcome, nil
}
