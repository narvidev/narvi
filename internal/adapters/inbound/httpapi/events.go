package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// eventsDefaultLimit / eventsMaxLimit bound ?limit= on ListEvents below --
// a plain count, not a duration. This is the first Step to expose a
// client-controlled page-size query param at all, so these figures have
// no existing precedent to match; chosen to match
// internal/adapters/inbound/wshub's own fetch_history default/max
// exactly, since REST and WS read the identical underlying event log via
// the identical EventStore.ListForSession and should not diverge on page
// sizing either.
const (
	eventsDefaultLimit = 100
	eventsMaxLimit     = 500
)

// ListEvents backs GET /api/sessions/{sessionID}/events?cursor=&limit=
// (§6.3). Session existence is checked FIRST (matching the client WS
// hub's own session-existence-first convention) -- 404 if it doesn't
// exist. cursor defaults to 0 ("from the beginning"); limit defaults to
// eventsDefaultLimit and is capped at eventsMaxLimit regardless of what
// the caller requests. Responds 200 with restdtos.EventsResponse: the
// events and nextCursor of clientws.FetchHistoryResponse, backed by the
// SAME EventStore.ListPageForSession the client WS hub's own fetch_history
// handler uses, one implementation, two callers. It deliberately leaves
// out FetchHistoryResponse's sandbox: that row is a WS concern, what keeps
// an open page's sandbox status current after a sandbox_status broadcast
// (§6.2); a REST caller reads the status from GET
// /api/sessions/{id}/status (SessionActivity.sandboxStatus).
//
// A page holds at most limit events and at most
// platform.FetchHistoryMaxReplyBytes of them, always at least one, exactly
// as a fetch_history page does, and the budget bounds what the page reads,
// not only what it sends: the store reads only the events that fit
// (EventStore.ListPageForSession), and eventsPageLen checks each one's
// exact encoded size against the budget again. nextCursor is the id of the
// last event sent whenever the page stopped at its byte budget or read its
// full count, so a page shorter than limit can still have one after it.
func ListEvents(sessions *postgres.SessionStore, events *postgres.EventStore) http.HandlerFunc {
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

		var cursor int64
		if raw := r.URL.Query().Get("cursor"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, "malformed cursor")
				return
			}
			cursor = parsed
		}

		limit := eventsDefaultLimit
		if raw := r.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 {
				writeError(w, http.StatusBadRequest, "malformed limit")
				return
			}
			limit = parsed
			if limit > eventsMaxLimit {
				limit = eventsMaxLimit
			}
		}

		page, err := events.ListPageForSession(ctx, sessionID, cursor, int32(limit), platform.FetchHistoryMaxReplyBytes)
		if err != nil {
			logger.Error("httpapi: list events failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		rows := page.Events

		sent := eventsPageLen(rows, platform.FetchHistoryMaxReplyBytes)
		wire := make([]restdtos.EventsResponseEventsElem, sent)
		for i, e := range rows[:sent] {
			wire[i] = eventWireMap(e)
		}

		var nextCursor *string
		if sent > 0 && (page.StoppedAtBudget || sent < len(rows) || len(rows) == limit) {
			s := strconv.FormatInt(rows[sent-1].ID, 10)
			nextCursor = &s
		}

		writeJSON(w, http.StatusOK, restdtos.EventsResponse{
			Events:     wire,
			NextCursor: nextCursor,
		})
	}
}

// eventWireMap mirrors internal/adapters/inbound/wshub's own identical
// helper in client.go (same field choices, same json.RawMessage
// treatment to avoid base64-encoding sqlcgen.Event.Payload's plain
// []byte) -- duplicated, not shared, since the two packages return
// distinctly-named generated element types (restdtos.
// EventsResponseEventsElem here vs clientws.SubscribedPayloadEventsElem/
// FetchHistoryResponseEventsElem there) even though all are structurally
// map[string]interface{}; each must be assembled directly as its own
// named type, so a shared "returns []map[string]interface{}" helper would
// still need a per-package conversion loop regardless. This mirrors the
// codebase's own established tolerance for small, justified per-package
// duplication (e.g. sessionactor's and wshub's own near-identical
// integration_helpers_test.go files).
func eventWireMap(e sqlcgen.Event) map[string]interface{} {
	return map[string]interface{}{
		"id":        e.ID,
		"type":      e.Type,
		"payload":   json.RawMessage(e.Payload),
		"createdAt": e.CreatedAt,
	}
}

// eventsPageLen mirrors internal/adapters/inbound/wshub's own
// historyPageLen (client.go), duplicated for the reason eventWireMap is: it
// returns how many of rows, oldest first, one page sends -- the longest run
// from the first whose events, each encoded as eventWireMap gives it and
// counted with the comma that follows it, sum to at most budget, and never
// fewer than one for a non-empty rows, so a page always gives its reader a
// cursor to go on from. The store already read no more than fits
// (EventStore.ListPageForSession measures each event by an upper bound on
// this size), so this is the exact check behind it, and it keeps every row
// the store returns. A marshal failure counts as zero bytes: writeJSON's
// own marshal reports it.
func eventsPageLen(rows []sqlcgen.Event, budget int) int {
	total := 0
	for i, e := range rows {
		if b, err := json.Marshal(eventWireMap(e)); err == nil {
			total += len(b) + 1
		}
		if total > budget && i > 0 {
			return i
		}
	}
	return len(rows)
}
