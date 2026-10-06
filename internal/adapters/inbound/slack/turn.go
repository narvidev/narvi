// This file (turn.go) adds a turn to an EXISTING session -- the "reply
// within an already-mapped thread" half of doc.go's own thread<->session
// mapping design, and also the final step of the "brand-new thread"
// path once its own session id is resolved (see doc.go's own numbered
// design writeup).
//
// Audit-fix batch update (findings H7/L12/L2/M6): addTurn used to be a
// copy-pasted reimplementation of internal/adapters/inbound/httpapi/
// turn.go's own CreateTurn precondition/locking -- its own doc comment
// used to say so explicitly, since no shared, HTTP-independent core
// existed at the time it was written. httpapi.CreateTurnCore's own
// hasOpenTurn 409 gate is now parameterized by a CreateTurnPolicy
// (turn.go), so addTurn below is a thin wrapper over that ONE shared core
// instead: this closes L12 (hasOpenTurn was copy-pasted here, not shared)
// and H7 (this call now writes the SAME turn.create audit_log row every
// other CreateTurnCore caller gets).

package slack

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/intentclassifier"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/turnguard"
)

// addTurn enqueues a new Pending turn carrying prompt on the EXISTING
// session sessionID, via httpapi.CreateTurnCore's own DropIfOpen policy --
// the SAME shared core REST's own CreateTurn, Linear's handlePrompted, and
// Slack's own "Request changes" modal submission (interactive.go) all now
// go through too (see CreateTurnPolicy's own doc comment, httpapi/turn.go).
// created reports whether a turn was actually inserted: false (err == nil)
// means a turn was already open for this session (Pending/Dispatched/
// Processing) and this call was a deliberate no-op, exactly as before this
// batch -- the caller's own in-thread ack wording (handler.go) reflects
// that case distinctly ("still working on the previous message") rather
// than silently queuing a second turn behind it.
//
// planMode (a follow-up fix, §8.1) lets handleEvent (handler.go)
// route a revise:-prefixed reply through as a real plan_mode=true
// "request changes" turn instead of always hardcoding false -- see that
// function's own doc comment. err is a plain *httpapi.CreateTurnError
// (never unwrapped/converted) specifically so a caller can recognize
// httpapi.ErrPlanAwaitingApproval via errors.Is -- handleEvent does exactly
// that to post an honest reply instead of treating this as a hard failure
// -- and a refusal of the session guard (guard, technical plan §40.1) via
// sessionguard.AsRefusal, which it answers with the refusal's own text.
//
// actorUserID (audit-fix batch addition) is attributed onto the resulting
// turn.create audit_log row only -- turns itself carries no per-row actor
// column (migrations/000005_turns.up.sql), so this mirrors handleEvent's
// own already-resolved actorUserID, which previously had nowhere at all to
// flow into for a reply on an existing thread.
//
// intentSvc (§23.1/§23.2) is threaded straight through to
// httpapi.CreateTurnCore's own plan_followup block, exactly like plans
// immediately before it -- handleEvent's own caller passes the SAME
// deps.IntentClassifier every other classification use in this package
// already does (handler.go).
func addTurn(ctx context.Context, pool *pgxpool.Pool, sessions *postgres.SessionStore, turns *postgres.TurnStore, plans *postgres.PlanStore, intentSvc *intentclassifier.Service, auditLog *postgres.AuditLogStore, registry *sessionactor.Registry, guard *turnguard.Guard, sessionID pgtype.UUID, prompt string, planMode bool, epistemicCheckDefault bool, actorUserID pgtype.UUID) (turn sqlcgen.Turn, created bool, err error) {
	turnRow, wasCreated, cerr := httpapi.CreateTurnCore(ctx, pool, sessions, turns, plans, intentSvc, auditLog, registry, guard, sessionID, prompt, nil, planMode, epistemicCheckDefault, actorUserID, httpapi.DropIfOpen)
	if cerr != nil {
		return sqlcgen.Turn{}, false, cerr
	}
	return turnRow, wasCreated, nil
}

// findAwaitingApprovalPlanID reports sessionID's own current
// awaiting_approval plan id, if any -- mirrors internal/adapters/inbound/
// linear's own identical findAwaitingApprovalPlanID (webhook.go): a
// package-private copy rather than a shared helper, matching this
// codebase's own established per-package convention for small,
// cheap-to-duplicate webhook-adjacent helpers (see this package's own
// maxRequestBodyBytes doc comment, handler.go, for the identical
// precedent). Used by handleEvent (handler.go) to decide whether an
// incoming reply's own text should be checked against
// plandomain.MatchVerdict/plandomain.MatchRevise at all -- a lookup
// failure is logged and treated as "no awaiting plan" (false, zero
// pgtype.UUID), since a keyword-parsing convenience must never turn into a
// hard failure of the underlying event: createTurnLocked's own
// awaiting-plan gate (httpapi/turn.go) is what actually enforces the
// invariant for the ordinary-reply fallthrough regardless, and
// handlePlanVerdict's own httpapi.DecidePlan call re-validates the plan's
// real, current status itself before ever deciding it.
//
// Renamed from hasAwaitingApprovalPlan (this batch's own addition, "honour
// a typed plan verdict"): the ONE caller (handleEvent) now needs the
// plan's own id too, to pass to handlePlanVerdict/httpapi.DecidePlan on a
// plandomain.MatchVerdict match -- a bare bool was no longer enough.
func findAwaitingApprovalPlanID(ctx context.Context, logger *slog.Logger, plans *postgres.PlanStore, sessionID pgtype.UUID) (pgtype.UUID, bool) {
	summaries, err := plans.ListSummariesForSession(ctx, sessionID)
	if err != nil {
		logger.Warn("slack: list plan summaries for verdict/revise check failed", "error", err, "session_id", sessionID.String())
		return pgtype.UUID{}, false
	}
	for _, s := range summaries {
		if s.Status == sqlcgen.PlanStatusAwaitingApproval {
			return s.ID, true
		}
	}
	return pgtype.UUID{}, false
}
