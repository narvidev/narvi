// This file (planapprove.go) implements §8.1's ("plan mode, web",
// §8.1/§12.2 item 3) two new REST endpoints: POST
// /api/sessions/:id/plans/:planId/approve and its reject twin. Both
// follow CreateTurn's own exact pattern (turn.go) -- open a tx, lock the
// session row, run canActOnPlan (planauthz.go), then hand off to
// DecidePlanOnTx (decideplan.go, §8.1's own "plan mode, cross-channel"
// extraction) for the shared guarded-UPDATE-then-(approve-only)-turn-
// insert-then-cross-channel-notify sequencing every entry point (this
// REST pair, Slack's block_actions handler, Linear's text-verdict parsing)
// now shares identically -- see decideplan.go's own top doc comment for
// the full CreateSessionCore/CreateSessionOnTx-mirroring design this
// extraction follows.
//
// Neither endpoint re-explains the plan in the new turn's own prompt text
// -- sessions.opencode_conversation_id already persists across turns and
// is already threaded into every dispatched Prompt automatically
// (dispatch.go's buildPromptPayload), so the SAME OpenCode conversation
// the plan turn ran in continues with full context, no re-statement
// needed (see this Step's own brief for the precedent this reuses).
//
// ApprovePlan additionally applies CreateTurn's own hasOpenTurn 409 gate
// (turn.go), via DecidePlanOnTx, before ever touching the plans row: a
// plan stays 'awaiting_approval' in the DB for as long as no LATER
// plan-mode turn has yet completed, even while a "request changes" turn
// for it is already Pending/Dispatched/Processing -- approving it during
// that window would flip it to 'approved' and enqueue an implementation
// turn that auto-dispatches (handleEnsureDispatched, sandboxevent.go) the
// instant that in-flight revision completes, building the STALE plan
// before any human ever reviews its successor.

package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// implementPlanPrompt is the short, fixed prompt text the approval-
// dispatched implementation turn carries -- sufficient because the
// SAME OpenCode conversation the plan turn itself ran in already has the
// full plan context (see this file's own top doc comment).
const implementPlanPrompt = "Implement the plan you just proposed."

// parsePlanID parses chi's own "planId" URL path param as a UUID --
// mirrors parseSessionID's own exact shape (helpers.go).
func parsePlanID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	raw := chi.URLParam(r, "planId")
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		writeError(w, http.StatusBadRequest, "malformed plan id")
		return pgtype.UUID{}, false
	}
	return id, true
}

// lockSessionForPlanAction fetches sessionID (404 if missing) and then
// locks its row for the rest of the caller's own transaction (404 again
// if it vanished between the two, vanishingly rare) -- mirrors
// CreateTurn's own identical two-step shape (turn.go) exactly. Returns
// the UNLOCKED row fetched by the first call (sufficient for this
// caller's own read-only use of CreatedBy/BuildModelID; the lock itself,
// not a fresher read, is what matters here).
func lockSessionForPlanAction(w http.ResponseWriter, r *http.Request, tx pgx.Tx, sessions *postgres.SessionStore, sessionID pgtype.UUID) (sqlcgen.Session, bool) {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	sessionRow, err := sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "session not found")
			return sqlcgen.Session{}, false
		}
		logger.Error("httpapi: get session failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return sqlcgen.Session{}, false
	}

	if _, err := sessions.WithTx(tx).GetActorEpochForUpdate(ctx, sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "session not found")
			return sqlcgen.Session{}, false
		}
		logger.Error("httpapi: lock session row failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return sqlcgen.Session{}, false
	}

	return sessionRow, true
}

// authorizePlanAction runs canActOnPlan (planauthz.go) against the
// currently authenticated request user, writing 500/403 and returning
// false on any failure -- shared by both ApprovePlan and RejectPlan so
// neither duplicates this sequencing.
func authorizePlanAction(w http.ResponseWriter, r *http.Request, participants *postgres.ParticipantStore, sessionRow sqlcgen.Session) (pgtype.UUID, bool) {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	actorUserID, ok := authenticatedUserID(w, r)
	if !ok {
		return pgtype.UUID{}, false
	}

	authUser, ok := platform.UserFromContext(ctx)
	if !ok {
		// Unreachable behind auth.Middleware -- see authenticatedUserID's
		// own identical defensive precedent (helpers.go).
		logger.Error("httpapi: no authenticated user in context (route not mounted behind auth.Middleware?)")
		writeError(w, http.StatusInternalServerError, "internal error")
		return pgtype.UUID{}, false
	}

	allowed, err := canActOnPlan(ctx, participants, sessionRow, actorUserID, authUser.Role)
	if err != nil {
		logger.Error("httpapi: canActOnPlan failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return pgtype.UUID{}, false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "not authorized to act on this session's plans")
		return pgtype.UUID{}, false
	}

	return actorUserID, true
}

// ApprovePlan backs POST /api/sessions/:id/plans/:planId/approve (§12.2
// item 3's own "Approve & build" action). See this file's own top doc
// comment for the full sequencing; the actual decision now runs through
// DecidePlanOnTx (decideplan.go), shared with every other entry point.
// events/planDocuments (§31.3) are DecidePlanOnTx's own approved-plan
// snapshot dependencies -- threaded through here rather than constructed
// inline, exactly like every other store this handler already receives.
func ApprovePlan(pool *pgxpool.Pool, sessions *postgres.SessionStore, turns *postgres.TurnStore, plans *postgres.PlanStore, events *postgres.EventStore, planDocuments *postgres.PlanDocumentStore, participants *postgres.ParticipantStore, outbox *postgres.OutboxStore, linearAgentSessions *postgres.LinearAgentSessionStore, auditLog *postgres.AuditLogStore, registry *sessionactor.Registry, epistemicCheckDefault bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		planID, ok := parsePlanID(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		r = r.WithContext(ctx)
		logger := platform.Logger(ctx)

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: begin approve-plan tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		sessionRow, ok := lockSessionForPlanAction(w, r, tx, sessions, sessionID)
		if !ok {
			return
		}

		actorUserID, ok := authorizePlanAction(w, r, participants.WithTx(tx), sessionRow)
		if !ok {
			return
		}

		outcome, err := DecidePlanOnTx(ctx, tx, sessions, turns, plans, events, planDocuments, outbox, linearAgentSessions, auditLog, sessionRow, planID, PlanVerdictApprove, actorUserID, epistemicCheckDefault)
		if err != nil {
			if errors.Is(err, ErrPlanOpenTurnInFlight) {
				// Mirrors CreateTurn's own hasOpenTurn 409 gate (turn.go)
				// exactly, including its message -- see this file's own
				// top doc comment and DecidePlanOnTx's own doc comment for
				// the full "why" this closes.
				writeError(w, http.StatusConflict, "a turn is already pending, dispatched, or processing for this session")
				return
			}
			logger.Error("httpapi: decide plan (approve) failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !outcome.Won {
			writeError(w, http.StatusConflict, "plan is not awaiting approval (already decided, or a stale id)")
			return
		}

		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: commit approve-plan tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Audit-fix batch (completeness/observability, M2 part 2): this REST
		// entry point calls DecidePlanOnTx directly (it already holds its own
		// open transaction from authorizePlanAction's lock above), so it
		// never went through DecidePlan's own pool-based wrapper -- the ONLY
		// place that previously logged a "decided plan" success line. Same
		// message/field shape as that wrapper's own log line
		// (decideplan.go), logged at the same point in the flow (right after
		// commit), so a REST-originated decision is exactly as observable as
		// a Slack/Linear-originated one.
		logger.Info("httpapi: decided plan", "plan_id", planID.String(), "session_id", sessionID.String(), "verdict", string(PlanVerdictApprove), "won", outcome.Won, "final_status", outcome.FinalStatus)

		// Fire-and-forget, OUTSIDE the transaction above, mirroring
		// CreateTurn's own identical post-commit sequencing exactly
		// (turn.go). Audit-fix batch (C1): now via the shared TriggerDispatch
		// helper (create.go) -- the identical GetOrSpawn + Send
		// (EnsureDispatched{}) sequence this used to hand-inline, already
		// reused by DecidePlan's own pool-wrapper (decideplan.go) and
		// github/coalesce.go. The only observable difference is the log
		// message wording on either step's failure (now TriggerDispatch's
		// own generic text rather than this call site's former "after plan
		// approval" text) -- an accepted side effect of this consolidation,
		// not a behavior change to session/turn state; DecidePlan's own
		// existing TriggerDispatch call site already uses this same generic
		// wording for an identical plan-decision-triggered dispatch.
		TriggerDispatch(ctx, registry, sessionID)

		writeJSON(w, http.StatusOK, restdtos.PlanActionResponse{
			PlanId: planID.String(),
			Status: restdtos.PlanActionResponseStatusApproved,
			TurnId: restdtos.PlanActionResponseTurnId(outcome.TurnID),
		})
	}
}

// RejectPlan backs POST /api/sessions/:id/plans/:planId/reject (§12.2
// item 3's own "Reject" action). Same guarded-UPDATE shape as ApprovePlan,
// with no new turn and no dispatch, via DecidePlanOnTx (decideplan.go).
//
// epistemicCheckDefault is threaded through purely for parameter-shape
// consistency with ApprovePlan/DecidePlanOnTx's own now-required parameter
// (F6) -- DecidePlanOnTx only ever consults it on a PlanVerdictApprove
// verdict (the branch that creates the post-approval implementation turn),
// which this handler never passes, so this value never actually changes
// behavior on this specific path today. Still required, never defaulted,
// mirroring epistemicCheckDefault's own established convention throughout
// this codebase (turn.go's own doc comment on CreateTurnCore: "every call
// site must compile-time-decide what to pass, exactly like planMode
// itself").
func RejectPlan(pool *pgxpool.Pool, sessions *postgres.SessionStore, turns *postgres.TurnStore, plans *postgres.PlanStore, events *postgres.EventStore, planDocuments *postgres.PlanDocumentStore, participants *postgres.ParticipantStore, outbox *postgres.OutboxStore, linearAgentSessions *postgres.LinearAgentSessionStore, auditLog *postgres.AuditLogStore, epistemicCheckDefault bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		planID, ok := parsePlanID(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		r = r.WithContext(ctx)
		logger := platform.Logger(ctx)

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: begin reject-plan tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		sessionRow, ok := lockSessionForPlanAction(w, r, tx, sessions, sessionID)
		if !ok {
			return
		}

		actorUserID, ok := authorizePlanAction(w, r, participants.WithTx(tx), sessionRow)
		if !ok {
			return
		}

		outcome, err := DecidePlanOnTx(ctx, tx, sessions, turns, plans, events, planDocuments, outbox, linearAgentSessions, auditLog, sessionRow, planID, PlanVerdictReject, actorUserID, epistemicCheckDefault)
		if err != nil {
			logger.Error("httpapi: decide plan (reject) failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !outcome.Won {
			writeError(w, http.StatusConflict, "plan is not awaiting approval (already decided, or a stale id)")
			return
		}

		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: commit reject-plan tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Audit-fix batch (completeness/observability, M2 part 2): see
		// ApprovePlan's own identical addition above for the full "why" --
		// same message/field shape as DecidePlan's own wrapper log line.
		logger.Info("httpapi: decided plan", "plan_id", planID.String(), "session_id", sessionID.String(), "verdict", string(PlanVerdictReject), "won", outcome.Won, "final_status", outcome.FinalStatus)

		writeJSON(w, http.StatusOK, restdtos.PlanActionResponse{
			PlanId: planID.String(),
			Status: restdtos.PlanActionResponseStatusRejected,
			TurnId: nil,
		})
	}
}
