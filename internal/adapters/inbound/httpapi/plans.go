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
// below resolves restdtos.Plan.content for every version returned.
//
// The durable plan_documents snapshot (migrations/000112_plan_documents.
// up.sql) is the FIRST choice wherever a usable row exists; the bounded
// live event-log recompute -- internal/domain/plan.ExtractContent, the same
// scan the Slack/Linear cross-channel notifiers already use,
// internal/app/sessionactor/planapprovalcontent.go -- is the FALLBACK, used
// only where no usable snapshot exists (see resolvePlanRenderedContent's
// own doc comment for the exact three-way rule). See ExtractContent's own
// doc comment for why a per-version UPPER bound (the next turn dispatched
// in the session, if any) is required for that fallback and was not needed
// by the single-turn notifier caller: an older, already-superseded/decided
// plan version is never the session's own most-recently-dispatched turn by
// the time anyone lists it.
//
// # Plan-mode UI addition: structured (§12.2 item 3's own missing schema)
//
// planContentMap additionally resolves restdtos.Plan.structured alongside
// content, following the SAME snapshot-first rule: a snapshot's own
// persisted structured_steps (decoded) when non-NULL, or
// internal/domain/plan.ExtractStructured re-derived from whichever content
// was resolved (snapshot or live recompute) when it is NULL -- see that
// function's and resolvePlanRenderedContent's own doc comments for the
// extraction rule and why nil (never a partial value) is the only "no
// structure" representation.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/framecut"
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
// its own rendered content/structured, see this file's own top doc comment
// and planContentMap's own "snapshot first, live recompute as fallback"
// doc comment) and returned as restdtos.ListPlansResponse.
func ListPlans(sessions *postgres.SessionStore, plans *postgres.PlanStore, turns *postgres.TurnStore, events *postgres.EventStore, planDocuments *postgres.PlanDocumentStore) http.HandlerFunc {
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

		renderedByPlanID, err := planContentMap(ctx, turns, events, planDocuments, sessionID, rows)
		if err != nil {
			logger.Error("httpapi: compute plan content failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		wire := make([]restdtos.Plan, len(rows))
		for i, p := range rows {
			wire[i] = planWireMap(p, renderedByPlanID[p.ID.String()])
		}

		writeJSON(w, http.StatusOK, restdtos.ListPlansResponse{Plans: wire})
	}
}

// planRenderedContent is one plan version's own rendered content/structured
// pair, as planContentMap resolves it -- see that function's own doc
// comment for the "snapshot first, live recompute as fallback" rule that
// produces it -- with the cut its text carries (technical plan §6.1), nil
// for a whole text and for a snapshot, which records none.
type planRenderedContent struct {
	Content    string
	Structured *plandomain.Structured
	Cut        *framecut.Cut
}

// planContentMap resolves restdtos.Plan.content/structured for every row in
// planRows, keyed by plan id (string form).
//
// The durable plan_documents snapshot (migrations/000112_plan_documents.
// up.sql, written once at approval time by decideplan.go's own
// snapshotApprovedPlanContent) is now the FIRST choice, not merely a
// coverage-measurement side table nothing in production ever read: past
// this package's own planContentEventFetchLimit, the live recompute below
// can only return plandomain.ContentFallbackText even though the exact
// approved prose sits durably in that snapshot. The bounded live recompute
// (exactly what this function did before the snapshot existed) is now the
// FALLBACK, used only where no USABLE snapshot exists -- see
// planDocumentSnapshotMap and resolvePlanRenderedContent below for the
// precise three-way rule (no row / usable row / row with NULL content).
//
// ONE snapshot batch fetch (planDocumentSnapshotMap) always runs, and is
// shared across every plan version in the session -- re-fetching it per plan
// would be pure waste against the SAME underlying rows. The live-recompute
// inputs -- ONE turns query (turns.ListForSession) and ONE events fetch (the
// session's own most recent planContentEventFetchLimit events, newest
// first, exactly like planapprovalcontent.go's own single-turn fetch) -- are
// fetched AFTER the snapshot batch, and only when at least one plan row
// lacks a usable snapshot (usablePlanSnapshot): when every row is usable,
// resolvePlanRenderedContent takes the snapshot branch for all of them and
// never consults either fetch, so skipping both is safe and is the whole
// point of making the snapshot the first choice rather than merely trying
// it first while still paying for the fallback unconditionally.
//
// Bounds for the live recompute are derived from EVERY turn dispatched in
// the session (turns.ListForSession), not only the plan-producing ones: a
// plan version's own upper bound is the NEXT turn dispatched afterward
// REGARDLESS of what kind of turn it was (an approved plan's own
// approval-dispatched IMPLEMENTATION turn is exactly such a turn, and is
// what makes an unbounded-above scan wrong for a decided plan -- see
// plandomain.ExtractContent's own doc comment). Turns with no
// DispatchedEventID (never dispatched -- e.g. still pending) carry no scan
// boundary of their own and are excluded from the ordered boundary list; a
// plan's own producing turn always HAS one by the time a plan row exists
// for it (a plan is only ever created once its producing turn completes,
// which requires having been dispatched first).
func planContentMap(ctx context.Context, turns *postgres.TurnStore, events *postgres.EventStore, planDocuments *postgres.PlanDocumentStore, sessionID pgtype.UUID, planRows []sqlcgen.Plan) (map[string]planRenderedContent, error) {
	if len(planRows) == 0 {
		return nil, nil
	}

	snapshotByPlanID, err := planDocumentSnapshotMap(ctx, planDocuments, planRows)
	if err != nil {
		return nil, err
	}

	// The live-recompute inputs (allTurns/contentEvents) are only fetched
	// when at least one plan row lacks a usable snapshot -- see
	// usablePlanSnapshot's own doc comment for why this predicate MUST be
	// identical to the one resolvePlanRenderedContent below branches on.
	// When every row is usable, both fetches (a turns query and a bounded
	// 2000-event scan) are pure waste: resolvePlanRenderedContent will take
	// the snapshot branch for every row and never consult allTurns/
	// contentEvents, which is exactly the case the snapshot-first design
	// was meant to make cheap.
	needsFallback := false
	for _, p := range planRows {
		snapshot, snapshotOK := snapshotByPlanID[p.ID.String()]
		if !usablePlanSnapshot(snapshot, snapshotOK) {
			needsFallback = true
			break
		}
	}

	var allTurns []sqlcgen.Turn
	var contentEvents []plandomain.ContentEvent
	if needsFallback {
		var err error
		allTurns, err = turns.ListForSession(ctx, sessionID)
		if err != nil {
			return nil, err
		}

		recentEvents, err := events.ListRecentForSession(ctx, sessionID, planContentEventFetchLimit)
		if err != nil {
			return nil, err
		}
		contentEvents = sessionactor.ToContentEvents(recentEvents)
	}

	out := make(map[string]planRenderedContent, len(planRows))
	for _, p := range planRows {
		snapshot, snapshotOK := snapshotByPlanID[p.ID.String()]
		rendered, err := resolvePlanRenderedContent(p, snapshot, snapshotOK, allTurns, contentEvents)
		if err != nil {
			return nil, err
		}
		out[p.ID.String()] = rendered
	}
	return out, nil
}

// planDocumentSnapshotMap fetches every plan_documents row for planRows'
// own plan ids, in ONE batch call (PlanDocumentStore.ListByPlanIDs) --
// mirrors planContentMap's own "one events fetch shared across every
// version" discipline, never one snapshot lookup per plan. A plan with no
// snapshot row simply has no entry in the returned map -- ok(false) from a
// plain map lookup is how resolvePlanRenderedContent below distinguishes
// "no snapshot" from "a snapshot exists".
func planDocumentSnapshotMap(ctx context.Context, planDocuments *postgres.PlanDocumentStore, planRows []sqlcgen.Plan) (map[string]sqlcgen.PlanDocument, error) {
	ids := make([]pgtype.UUID, len(planRows))
	for i, p := range planRows {
		ids[i] = p.ID
	}
	docs, err := planDocuments.ListByPlanIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]sqlcgen.PlanDocument, len(docs))
	for _, d := range docs {
		out[d.PlanID.String()] = d
	}
	return out, nil
}

// usablePlanSnapshot reports whether snapshot is a snapshot
// resolvePlanRenderedContent will actually render from (its content
// branch), as opposed to one it will fall back past (no row, or a
// retention-nulled content). planContentMap's "is it safe to skip the live-
// recompute fetch" decision and resolvePlanRenderedContent's own branch
// condition MUST use this exact same predicate, or a plan that actually
// needs the fallback could be handed an empty event set by a skipped fetch
// and silently render plandomain.ContentFallbackText instead of its real
// prose -- a correctness regression dressed as an optimization.
func usablePlanSnapshot(snapshot sqlcgen.PlanDocument, snapshotOK bool) bool {
	return snapshotOK && snapshot.Content != nil
}

// resolvePlanRenderedContent implements the three-era rule a plan approved
// before, at, or after plan_documents existed must render under -- "prefer
// the snapshot only where a usable snapshot exists":
//
//   - No snapshot row at all (planRow predates migration 000112, or was
//     never approved): there is nothing to prefer, so this falls back to
//     EXACTLY the live event-log recompute this package always did --
//     placeholder included when the window has nothing left to find. This
//     is the deliberate settlement, not a gap: snapshot-first applies only
//     where a row exists.
//   - A snapshot row whose content is non-NULL (approved on/after 000112):
//     that content is used VERBATIM -- even when it happens to equal
//     plandomain.ContentFallbackText itself, which is the honest durable
//     record of what approval-time recovery actually found; a live
//     recompute now could only match it or do worse (the window has moved
//     on). structured comes from the snapshot's own structured_steps when
//     non-NULL (decoded, never re-derived -- the persisted document IS the
//     immutable record of what was approved, §5.1), or from
//     plandomain.ExtractStructured(snapshotContent) when structured_steps
//     itself is NULL.
//   - A snapshot row whose content IS NULL (a future retention policy
//     nulled it, migrations/000112_plan_documents.up.sql's own reason that
//     column is nullable at all): there is no usable snapshot, so this
//     falls back to the live recompute exactly like the "no row" case --
//     structured_steps on such a row is never consulted, since a snapshot
//     with no prose to show has nothing authoritative to pair it with
//     either.
//
// The cut (technical plan §6.1) comes only from the live recompute: its
// Final reports whether the plan's text is a frame the sandbox-agent cut,
// and a cut text has no structured steps (plandomain.ExtractStructured). A
// snapshot reports none: it records what was approved, and the approval
// refuses a cut plan (ErrPlanCut). A snapshot taken of a cut text by a
// binary built before cuts existed -- the accepted rollback-window residue
// -- keeps its marker in its content and reports no cut, by design: it is
// the record of what a person approved.
func resolvePlanRenderedContent(planRow sqlcgen.Plan, snapshot sqlcgen.PlanDocument, snapshotOK bool, allTurns []sqlcgen.Turn, contentEvents []plandomain.ContentEvent) (planRenderedContent, error) {
	if usablePlanSnapshot(snapshot, snapshotOK) {
		content := *snapshot.Content
		if snapshot.StructuredSteps != nil {
			var structured plandomain.Structured
			if err := json.Unmarshal(snapshot.StructuredSteps, &structured); err != nil {
				return planRenderedContent{}, fmt.Errorf("httpapi: decode plan_documents.structured_steps for plan %s: %w", planRow.ID.String(), err)
			}
			return planRenderedContent{Content: content, Structured: &structured}, nil
		}
		return planRenderedContent{Content: content, Structured: plandomain.ExtractStructured(content, nil)}, nil
	}

	final := plandomain.Final{Text: plandomain.ContentFallbackText}
	if lower, upper, ok := sessionactor.TurnContentBounds(allTurns, planRow.TurnID); ok {
		final = plandomain.ExtractContent(contentEvents, lower, upper)
	}
	// Defensive: the !ok branch above (plans.turn_id names no dispatched
	// turn) should be unreachable in practice -- plans.turn_id is a NOT
	// NULL FK to turns and a plan row is only ever created once its
	// producing turn has already been dispatched (see planContentMap's own
	// top doc comment) -- but degrades to the SAME honest fallback
	// ExtractContent itself would return for an empty window, never a
	// panic on a missing map key.
	return planRenderedContent{Content: final.Text, Structured: plandomain.ExtractStructured(final.Text, final.Cut), Cut: final.Cut}, nil
}

// planWireMap maps one sqlcgen.Plan row (plus its own separately-resolved
// rendered content/structured pair, planContentMap/resolvePlanRenderedContent
// above) onto restdtos.Plan -- deliberately dropping TurnID/
// SlackChannelID/SlackMessageTs, present on the underlying row but not on
// the wire DTO (see Plan's own schema doc comment,
// contracts/rest/v1/dtos.schema.json, for why).
//
// rendered.Structured already reflects resolvePlanRenderedContent's own
// snapshot-first rule -- this function never calls plandomain.
// ExtractStructured itself, so it never has an opinion of its own on
// whether a re-derivation from rendered.Content would disagree with a
// persisted structured_steps value; it renders exactly what was resolved.
func planWireMap(p sqlcgen.Plan, rendered planRenderedContent) restdtos.Plan {
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
		Content:     rendered.Content,
		Structured:  planStructuredWireMap(rendered.Structured),
		Cut:         planCutWireMap(rendered.Cut),
	}
}

// planCutWireMap maps a plan's cut report onto restdtos.PlanCut -- nil in,
// nil out: a whole plan reports null, never an empty object.
func planCutWireMap(c *framecut.Cut) *restdtos.PlanCut {
	if c == nil {
		return nil
	}
	return &restdtos.PlanCut{Kept: c.Kept, Total: c.Total}
}

// planStructuredWireMap maps plandomain.ExtractStructured's own output onto
// restdtos.PlanStructured -- nil in, nil out, so a caller that got "no
// structure" from the domain function reports the identical "no structure"
// on the wire, never an empty-but-present object.
func planStructuredWireMap(s *plandomain.Structured) *restdtos.PlanStructured {
	if s == nil {
		return nil
	}
	steps := make([]restdtos.PlanStep, len(s.Steps))
	for i, step := range s.Steps {
		steps[i] = restdtos.PlanStep{
			Title:       step.Title,
			Description: step.Description,
			FileRefs:    step.FileRefs,
		}
	}
	return &restdtos.PlanStructured{
		Steps:         steps,
		ScopeEstimate: s.ScopeEstimate,
	}
}
