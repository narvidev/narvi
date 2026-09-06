// This file (plans.go) implements the audit-fix batch's own completeness
// fix (M3): GET /api/sessions/:id/plans -- the endpoint that closes the gap
// §8.1 ("plan mode, web", §8.1/§12.2 item 3) left open by shipping plan
// mode write-only (approve/reject, planapprove.go) with no way for a web
// client to ever discover a planId to approve in the first place.
//
// Mirrors ListArtifacts/ListEvents's own exact shape (artifacts.go,
// events.go) rather than inventing a new one: parseSessionID, session-exists
// 404 check, then the list query, then writeJSON. Deliberately NO extra RBAC
// beyond "session exists" -- the whole /api/sessions route group already
// sits behind auth.Middleware, and a plain read of the plan list changes no
// state, unlike ApprovePlan/RejectPlan's own canActOnPlan gate (planauthz.go)
// which exists specifically because those two calls DO change state.
//
// Deliberately minimal per this batch's own explicit scope note: no new
// WS/event notification on plan creation, no pagination (a session's own
// plan history is expected to stay small, matching ArtifactsResponse's own
// identical "unbounded" precedent) -- this endpoint's only job was closing
// the "no way to ever get a planId" gap.
//
// # Plan-mode UI addition: content
//
// The plan-mode UI (§12.2 item 3) needs more than a planId -- it needs the
// plan's own rendered text to show the human deciding on it. planContentMap
// below computes restdtos.Plan.content for every version returned, reusing
// internal/domain/plan.ExtractContent (the same bounded scan the Slack/
// Linear cross-channel notifiers already use, internal/app/sessionactor/
// planapprovalcontent.go) -- see that function's own doc comment for why a
// per-version UPPER bound (the next turn dispatched in the session, if any)
// is required here and was not needed by the single-turn notifier caller:
// an older, already-superseded/decided plan version is never the session's
// own most-recently-dispatched turn by the time anyone lists it.

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/platform"
)

// planContentEventFetchLimit mirrors internal/app/sessionactor/
// planapprovalcontent.go's own identically-named, identically-justified
// constant (a fixed, safe upper bound on how much of the session's own
// event-log TAIL this reads back, generous for any one turn's own streamed
// output) -- kept as this package's own separate copy rather than an
// exported cross-package constant, since the two callers' own tuning
// concerns are independent (this one scans once per REQUEST, across
// potentially several plan versions, not once per turn completion).
const planContentEventFetchLimit = 2000

// ListPlans backs GET /api/sessions/{sessionID}/plans (audit finding M3,
// completeness). Session existence is checked first -- 404 if it doesn't
// exist; otherwise every plan VERSION for the session (PlanStore.
// ListForSession, ordered by version), mapped to restdtos.Plan (including
// its own best-effort-extracted content, see this file's own top doc
// comment) and returned as restdtos.ListPlansResponse.
func ListPlans(sessions *postgres.SessionStore, plans *postgres.PlanStore, turns *postgres.TurnStore, events *postgres.EventStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		logger := platform.Logger(ctx)

		if _, err := sessions.Get(ctx, sessionID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: get session failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		rows, err := plans.ListForSession(ctx, sessionID)
		if err != nil {
			logger.Error("httpapi: list plans failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		contentByPlanID, err := planContentMap(ctx, turns, events, sessionID, rows)
		if err != nil {
			logger.Error("httpapi: compute plan content failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		wire := make([]restdtos.Plan, len(rows))
		for i, p := range rows {
			wire[i] = planWireMap(p, contentByPlanID[p.ID.String()])
		}

		writeJSON(w, http.StatusOK, restdtos.ListPlansResponse{Plans: wire})
	}
}

// planContentMap computes plandomain.ExtractContent's own result for every
// row in planRows, keyed by plan id (string form) -- ONE events fetch (the
// session's own most recent planContentEventFetchLimit events, newest
// first, exactly like planapprovalcontent.go's own single-turn fetch)
// shared across every plan version, since ExtractContent's own bounds do
// all the per-version scoping work; re-fetching per plan would be pure
// waste against the SAME underlying rows.
//
// Bounds are derived from EVERY turn dispatched in the session (turns.
// ListForSession), not only the plan-producing ones: a plan version's own
// upper bound is the NEXT turn dispatched afterward REGARDLESS of what kind
// of turn it was (an approved plan's own approval-dispatched IMPLEMENTATION
// turn is exactly such a turn, and is what makes an unbounded-above scan
// wrong for a decided plan -- see plandomain.ExtractContent's own doc
// comment). Turns with no DispatchedEventID (never dispatched -- e.g. still
// pending) carry no scan boundary of their own and are excluded from the
// ordered boundary list; a plan's own producing turn always HAS one by the
// time a plan row exists for it (a plan is only ever created once its
// producing turn completes, which requires having been dispatched first).
func planContentMap(ctx context.Context, turns *postgres.TurnStore, events *postgres.EventStore, sessionID pgtype.UUID, planRows []sqlcgen.Plan) (map[string]string, error) {
	if len(planRows) == 0 {
		return nil, nil
	}

	allTurns, err := turns.ListForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	recentEvents, err := events.ListRecentForSession(ctx, sessionID, planContentEventFetchLimit)
	if err != nil {
		return nil, err
	}
	contentEvents := sessionactor.ToContentEvents(recentEvents)

	out := make(map[string]string, len(planRows))
	for _, p := range planRows {
		lower, upper, ok := turnContentBounds(allTurns, p.TurnID)
		if !ok {
			// Defensive: plans.turn_id is a NOT NULL FK to turns and a plan
			// row is only ever created once its producing turn has already
			// been dispatched (see this function's own top doc comment) --
			// this branch should be unreachable in practice, but degrades to
			// the SAME honest fallback ExtractContent itself would return
			// for an empty window, never a panic on a missing map key.
			out[p.ID.String()] = plandomain.ContentFallbackText
			continue
		}
		out[p.ID.String()] = plandomain.ExtractContent(contentEvents, lower, upper)
	}
	return out, nil
}

// turnContentBounds returns plandomain.ExtractContent's own (lower, upper)
// bounds for turnID, given every turn dispatched in the session so far (any
// order, any kind -- an approval-dispatched IMPLEMENTATION turn counts
// exactly like a plan-producing one, see this file's own top doc comment):
// lower is turnID's own DispatchedEventID; upper is the DispatchedEventID
// of whichever DISPATCHED turn ran next in the session, if any (nil when
// turnID's own turn is the most recently dispatched one so far). ok is
// false when turnID names no turn in sessionTurns with a DispatchedEventID
// set at all -- should be unreachable for any real plan's producing turn
// (a plan row is only ever created once its producing turn has already
// been dispatched), but is surfaced as a plain bool rather than a panic or
// a silent unbounded scan, so every caller degrades the SAME honest way
// (plandomain.ContentFallbackText) instead of assuming it can't happen.
//
// Factored out of planContentMap above so httpapi's OTHER caller of this
// exact bounds calculation (decideplan.go's own approved-plan snapshot,
// §31.3) shares ONE implementation of an algorithm this package has
// already been bitten by an off-by-one in once (see plandomain.
// ExtractContent's own doc comment) -- never a second, independently
// re-derived copy that can silently drift from this one.
func turnContentBounds(sessionTurns []sqlcgen.Turn, turnID pgtype.UUID) (lower, upper *int64, ok bool) {
	dispatched := make([]sqlcgen.Turn, 0, len(sessionTurns))
	for _, t := range sessionTurns {
		if t.DispatchedEventID != nil {
			dispatched = append(dispatched, t)
		}
	}
	sort.Slice(dispatched, func(i, j int) bool {
		return *dispatched[i].DispatchedEventID < *dispatched[j].DispatchedEventID
	})

	for i, t := range dispatched {
		if t.ID != turnID {
			continue
		}
		lower = dispatched[i].DispatchedEventID
		if i+1 < len(dispatched) {
			upper = dispatched[i+1].DispatchedEventID
		}
		return lower, upper, true
	}
	return nil, nil, false
}

// planWireMap maps one sqlcgen.Plan row (plus its own separately-computed
// content, planContentMap above) onto restdtos.Plan -- deliberately
// dropping TurnID/SlackChannelID/SlackMessageTs, present on the underlying
// row but not on the wire DTO (see Plan's own schema doc comment,
// contracts/rest/v1/dtos.schema.json, for why).
func planWireMap(p sqlcgen.Plan, content string) restdtos.Plan {
	var decidedAt *time.Time
	if p.DecidedAt.Valid {
		t := p.DecidedAt.Time
		decidedAt = &t
	}
	var decidedBy restdtos.PlanDecidedBy
	if p.DecidedBy.Valid {
		s := p.DecidedBy.String()
		decidedBy = &s
	}
	return restdtos.Plan{
		Id:          p.ID.String(),
		SessionId:   p.SessionID.String(),
		Version:     int(p.Version),
		Status:      restdtos.PlanStatus(p.Status),
		PlanModelId: restdtos.PlanPlanModelId(p.PlanModelID),
		CreatedAt:   p.CreatedAt.Time,
		DecidedAt:   decidedAt,
		DecidedBy:   decidedBy,
		Content:     content,
	}
}
