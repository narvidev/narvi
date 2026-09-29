// This file (stop.go) implements POST /api/sessions/{sessionID}/stop
// (technical plan §3.3): a person's request to stop a session and every
// session it started. The route writes the request as data and changes no
// state itself -- every transition stays with each session's actor (§2),
// through §3.3's existing cancel edge (internal/app/sessionactor's
// stop.go). In one transaction per session, under the session's
// actor-epoch lock (the lock REST already takes to insert a turn, and the
// review webhook to write a timer):
//
//   - turns.stop_requested_at on every turn open at that instant;
//   - sessions.stop_requested_at;
//   - the session's `stop` timer upserted to now -- what makes the request
//     survive the loss of a replica;
//   - one session.stop audit row.
//
// After the named session's commit, its descendants are walked through
// parent_session_id, breadth first, each reached only after its parent's
// own request has committed, and each given the same request in its own
// transaction, audited with detail.via_parent_session_id and authorized by
// the check on the session named, since they are its work. A child whose
// creation races the walk either committed before its parent's request,
// and is found, or reads the parent's request FOR SHARE and is refused
// (CreateSessionOnTx, ParentStopped).
//
// Once the named session's request has committed, the rest -- waking its
// actor and the walk -- no longer depends on the caller staying: it runs on
// the request's context with its cancellation removed (so every value on it,
// the MCP grant the audit stamp reads included, still reaches each row),
// bounded by platform.Timeouts.StopDescendantWalkTimeout; the wakes follow
// the walk, so a slow one delays a stop but costs the walk nothing. A client that
// times out or disconnects after the commit therefore still stops the whole
// tree; cut short, the walk would leave the parent stopped and the sessions
// it started running, and a client that never got an answer has nothing
// telling it to repeat the request.

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/platform"
)

// parentStoppedMessage is CreateSessionOnTx's refusal of a child session
// whose parent a person stopped (CreateSessionError.ParentStopped).
const parentStoppedMessage = "the parent session was stopped: no new child session until a person resumes it"

// errStopSessionNotFound reports that the session a stop was being written
// to no longer exists.
var errStopSessionNotFound = errors.New("httpapi: stop: session not found")

// StopSessionDeps bundles what StopSession needs.
type StopSessionDeps struct {
	Pool         *pgxpool.Pool
	Sessions     *postgres.SessionStore
	Turns        *postgres.TurnStore
	Timers       *postgres.TimerStore
	Participants *postgres.ParticipantStore
	AuditLog     *postgres.AuditLogStore
	// GitHubPRSessions tells a pull request's review session -- one a
	// github_pr_sessions row points at -- from any other: the member rule
	// does not reach it (see StopSession).
	GitHubPRSessions *postgres.GitHubPRSessionStore
	// Registry, when set, is woken after each commit so the session's actor
	// handles its stop at once instead of at the timer pump's next tick.
	// The timer is the request's durable half: without the wake -- this
	// replica cannot host the actor, or is lost -- the pump of whichever
	// replica claims the timer does the same.
	Registry *sessionactor.Registry
	// Timeouts supplies StopDescendantWalkTimeout, the bound on what runs
	// after the named session's commit, detached from the caller (this
	// file's top comment). A zero bound -- Timeouts never validated -- ends
	// that work at once: no descendant is reached and the route answers
	// 500, so the gap shows.
	Timeouts platform.Timeouts
}

// StopSession backs POST /api/sessions/{sessionID}/stop (technical plan
// §3.3). No body. 400 on a malformed id and 404 on an unknown session, as
// GET /api/sessions/{sessionID} answers; 403 unless authz.ActionStopSession
// admits the caller -- admin and maintainer on any session, a member on
// their own or joined sessions except a pull request's review session
// (owner decision O1 and its review-session exception, technical plan
// §13.3), a viewer never. 202 with restdtos.StopSessionResponse once the
// request is written to the session and to every session it started. 500
// when a descendant could not be reached: what was written stands, and
// repeating the request reaches the rest. Once the named session's request
// has committed, a caller that goes away does not cut the rest short: the
// wake and the walk run to their end, or to StopDescendantWalkTimeout, and a
// walk that fails then is logged at WARN, the log being its only trace.
//
// A repeat is not a no-op. It flags every turn open at the moment it is
// made -- including one a person created since the first request, which
// then stops too -- writes its own session.stop audit row, and re-arms the
// stop timer. A turn keeps the instant it was first flagged, so its grace
// runs from the first request that reached it (RequestStopOpenTurns'
// COALESCE). The session keeps its latest request's instant, which
// requestedAt answers (RequestSessionStop's GREATEST): a repeat moves it
// forward, so the stop timer's handler also disarms the work-creating
// timers armed since an earlier request -- a re-review debounce a push
// armed between the two.
func StopSession(deps StopSessionDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		r = r.WithContext(ctx)
		logger := platform.Logger(ctx)

		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		// 404 before 403, as CreateTurn answers: a caller never learns
		// "you can't stop this" about a session that does not exist.
		sessionRow, err := deps.Sessions.Get(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: get session for authorization failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		ownedOrJoined := sessionRow.CreatedBy.Valid && sessionRow.CreatedBy == actorUserID
		if !ownedOrJoined {
			exists, err := deps.Participants.Exists(ctx, sessionRow.ID, actorUserID)
			if err != nil {
				logger.Error("httpapi: check participant for authorization failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			ownedOrJoined = exists
		}
		// A pull request's review session is shared: every review attempt
		// on that PR runs in it, a maintainer's label re-trigger and the
		// automatic re-review included, and the sentinel auto-fix sessions
		// it starts are its descendants. Its created_by is only whoever
		// first mentioned the bot there. So the member rule, whose reason is
		// that a member can already start and prompt the work a stop ends,
		// does not reach it: stopping one takes admin or maintainer, as
		// before O1 (technical plan §13.3).
		if ownedOrJoined {
			review, err := isPRReviewSession(ctx, deps.GitHubPRSessions, sessionRow.ID)
			if err != nil {
				logger.Error("httpapi: check review session for authorization failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			ownedOrJoined = !review
		}
		if !authorize(w, r, authz.ActionStopSession, authz.Resource{OwnedOrJoined: ownedOrJoined}) {
			return
		}

		requestedAt, openTurns, err := requestSessionStop(ctx, deps, sessionID, actorUserID, map[string]any{})
		if err != nil {
			if errors.Is(err, errStopSessionNotFound) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: request session stop failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// The request is written. What is left is owed to the session and
		// every session it started, whether or not the caller is still
		// there: detached from its cancellation, never from its values.
		walkCtx, cancelWalk := context.WithTimeout(context.WithoutCancel(ctx), deps.Timeouts.StopDescendantWalkTimeout)
		defer cancelWalk()
		reached, descendantTurns, walkErr := stopDescendants(walkCtx, deps, sessionID, actorUserID)
		// The wakes come after the walk, so a slow hydration can only
		// delay a stop, never cost the walk a session: every request woken
		// here is already written, with its timer, which the pump delivers
		// if the wake fails or the bound has run out.
		wakeStop(walkCtx, deps.Registry, sessionID)
		reachedIDs := make([]string, 0, len(reached)+1)
		reachedIDs = append(reachedIDs, sessionID.String())
		for _, id := range reached {
			wakeStop(walkCtx, deps.Registry, id)
			reachedIDs = append(reachedIDs, id.String())
		}
		if walkErr != nil {
			logger.Warn("httpapi: stop did not reach every session it started",
				"session_id", sessionID.String(), "reached", len(reached), "error", walkErr,
				"bound_expired", walkCtx.Err() != nil, "caller_gone", ctx.Err() != nil)
			writeError(w, http.StatusInternalServerError, "the session was stopped, but not every session it started could be reached: repeat the request")
			return
		}

		writeJSON(w, http.StatusAccepted, restdtos.StopSessionResponse{
			SessionId:         sessionID.String(),
			RequestedAt:       requestedAt.Time,
			ReachedSessionIds: reachedIDs,
			OpenTurns:         openTurns + descendantTurns,
		})
	}
}

// isPRReviewSession reports whether a github_pr_sessions row points at
// sessionID: whether it is a pull request's review session.
func isPRReviewSession(ctx context.Context, prSessions *postgres.GitHubPRSessionStore, sessionID pgtype.UUID) (bool, error) {
	if _, err := prSessions.GetBySessionID(ctx, sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// stopDescendants applies the stop request to every session rootID started,
// recursively, breadth first. Each session's children are listed only
// after that session's own request has committed, so a child whose
// creation raced it is either listed or refused (CreateSessionOnTx). A
// descendant that vanished meanwhile is skipped; any other failure is
// joined into err, and the walk goes on with the others -- below a session
// it could not stop, it cannot see what that session started. It wakes no
// actor: the caller wakes each session reached once the walk is over.
func stopDescendants(ctx context.Context, deps StopSessionDeps, rootID, actorUserID pgtype.UUID) (reached []pgtype.UUID, openTurns int, err error) {
	var errs []error
	seen := map[pgtype.UUID]bool{rootID: true}
	queue := []pgtype.UUID{rootID}
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]

		children, lerr := deps.Sessions.ListChildIDs(ctx, parentID)
		if lerr != nil {
			errs = append(errs, lerr)
			continue
		}
		for _, childID := range children {
			if seen[childID] {
				continue
			}
			seen[childID] = true

			_, n, serr := requestSessionStop(ctx, deps, childID, actorUserID, map[string]any{
				"via_parent_session_id": parentID.String(),
				"requested_session_id":  rootID.String(),
			})
			if serr != nil {
				if !errors.Is(serr, errStopSessionNotFound) {
					errs = append(errs, serr)
				}
				continue
			}
			reached = append(reached, childID)
			openTurns += n
			queue = append(queue, childID)
		}
	}
	return reached, openTurns, errors.Join(errs...)
}

// requestSessionStop writes the stop request to one session in its own
// transaction -- see this file's top comment. detail is the audit row's
// detail, gaining open_turns. Returns the session's request instant, now
// this request's, and how many turns it flagged. The caller wakes the
// session's actor once it has committed (wakeStop), on the detached
// context the rest of the request runs on.
func requestSessionStop(ctx context.Context, deps StopSessionDeps, sessionID, actorUserID pgtype.UUID, detail map[string]any) (pgtype.Timestamptz, int, error) {
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return pgtype.Timestamptz{}, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := deps.Sessions.WithTx(tx).GetActorEpochForUpdate(ctx, sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.Timestamptz{}, 0, errStopSessionNotFound
		}
		return pgtype.Timestamptz{}, 0, err
	}
	flagged, err := deps.Turns.WithTx(tx).RequestStopOpen(ctx, sessionID)
	if err != nil {
		return pgtype.Timestamptz{}, 0, err
	}
	requestedAt, err := deps.Sessions.WithTx(tx).RequestStop(ctx, sessionID)
	if err != nil {
		return pgtype.Timestamptz{}, 0, err
	}
	// Due at once. requestedAt is on the database's clock -- the clock the
	// pump compares fires_at with -- and never later than its current
	// time: this transaction's own instant, or an earlier request's when
	// that one began later (RequestSessionStop's GREATEST).
	if _, err := deps.Timers.WithTx(tx).Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID,
		Name:      sessionactor.TimerStop,
		FiresAt:   requestedAt,
	}); err != nil {
		return pgtype.Timestamptz{}, 0, err
	}

	auditDetail := make(map[string]any, len(detail)+1)
	for k, v := range detail {
		auditDetail[k] = v
	}
	auditDetail["open_turns"] = len(flagged)
	if err := recordAuditLog(ctx, deps.AuditLog.WithTx(tx), actorUserID, "session.stop", "session", sessionID.String(), auditDetail); err != nil {
		return pgtype.Timestamptz{}, 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return pgtype.Timestamptz{}, 0, err
	}
	return requestedAt, len(flagged), nil
}

// wakeStop hands the session's actor the stop timer's firing at once,
// best effort, like TriggerDispatch after a turn is created. The handler is
// idempotent -- the pump may deliver the same timer too -- and a failure
// here costs only latency: the timer is due, and the pump delivers it.
func wakeStop(ctx context.Context, registry *sessionactor.Registry, sessionID pgtype.UUID) {
	if registry == nil {
		return
	}
	logger := platform.Logger(ctx)
	actor, err := registry.GetOrSpawn(ctx, sessionID)
	if err != nil {
		logger.Info("httpapi: stop written; the timer pump will deliver it",
			"session_id", sessionID.String(), "reason", sessionactor.SpawnFailureReason(err), "error", err)
		return
	}
	if err := actor.Send(ctx, sessionactor.TimerFired{Name: sessionactor.TimerStop}); err != nil {
		logger.Info("httpapi: stop written; the timer pump will deliver it",
			"session_id", sessionID.String(), "error", err)
	}
}
