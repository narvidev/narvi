// This file (planapprovalcontent.go) implements §8.1's ("plan mode,
// cross-channel", §8.1/§13.3) own best-effort extraction of a plan's
// rendered content (the numbered steps/scope the producing turn's own
// assistant text laid out) for use in the Slack/Linear plan-approval-
// request notifications (outboxenqueue.go).
//
// This function itself does not attempt to parse steps out of the model's
// own prose into any structured shape -- it extracts the producing turn's
// own final streamed assistant text VERBATIM (the plan document's own
// actual content, whatever shape the model rendered it in, almost always
// already a numbered list given how plan-mode turns are prompted) and lets
// the caller (Slack Block Kit / Linear activity text) truncate/render it
// as plain text. §12.2 item 3's own "numbered steps with file refs, scope
// estimate" structured shape now DOES exist (internal/domain/plan.
// ExtractStructured, plandomain/structured.go) -- recovered from this SAME
// verbatim text, by a strict, model-asked-for-it-explicitly machine-
// readable block, never by inferring structure from freeform prose layout.
// This function's own two callers (outboxenqueue.go's Slack/Linear
// notifications) render the verbatim text either way and have no present
// need for the structured form; the web UI's own GET .../plans
// (internal/adapters/inbound/httpapi/plans.go) is the one caller that
// additionally runs ExtractStructured over the SAME content this function
// (or, for its own multi-version needs, plandomain.ExtractContent
// directly) recovers.
//
// The plan-mode UI (§12.2 item 3) generalized the actual scan below
// into plandomain.ExtractContent, reusable by a SECOND caller that needs it
// for MORE than just the single most-recently-dispatched turn (a session's
// full plan v1->v2 history) -- see that function's own doc comment for why
// an upper bound was needed for that case and not this one. This method
// keeps its own exact prior behavior (upperBoundEventID always nil: this
// caller only ever runs synchronously right when the plan-mode turn itself
// completes, when it IS -- by construction -- the most recently dispatched
// turn in the session) by degenerating to that same single-bound call.

package sessionactor

import (
	"context"
	"encoding/json"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
)

// planContentFallbackText is a local alias for plandomain.ContentFallbackText
// -- kept as its own name here (rather than every call site in this
// package spelling out the qualified name) purely for this file's own
// pre-existing readability; the two are byte-identical by construction
// (plandomain.go's own doc comment: "shared by every caller ... so neither
// can drift to a DIFFERENT wording").
const planContentFallbackText = plandomain.ContentFallbackText

// planContentEventFetchLimit bounds how many of the session's own MOST
// RECENT events this reads back (a.stores.event.ListRecentForSession,
// newest-first) -- generous for a single plan-mode turn's own streamed
// output (a turn's own text/tool-call event count is overwhelmingly never
// anywhere near this many), while still being a fixed, safe upper bound
// rather than an unbounded full-table scan. Deliberately anchored to the
// TAIL of the session's event log, not the beginning: a long-lived session
// (many prior turns) can easily have accumulated more than this many
// events in total by the time a LATER plan-mode turn completes, and this
// turn's own token events -- the most recent activity in the session --
// must still be found within the fetched window regardless of how much
// earlier history precedes them.
const planContentEventFetchLimit = 2000

// tokenEventPayload is the minimal shape this function reads out of a
// "token" event's own raw payload (contracts/sandbox-ws/v1/events.schema.
// json's own Token shape) -- Text is CUMULATIVE per messageId (§6.1: "text
// is CUMULATIVE, not a delta"), so the LAST token event (by event id, i.e.
// arrival order) for the producing turn's own span is already the full,
// final rendered text of whichever assistant message it belongs to.
type tokenEventPayload struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// planContentText best-effort recovers processing's own final streamed
// assistant text -- the plan document's own actual content -- by fetching
// the session's own event log, NEWEST FIRST (a.stores.event.
// ListRecentForSession, pool-based per EventStore's own doc comment, so
// this is always a plain read of already-committed rows, safe to call from
// inside completeProcessingTurn's own transact), converting it to
// plandomain.ContentEvent (decoding each "token" event's own raw payload;
// every other event type carries no Text this scan needs), and delegating
// the actual scan to plandomain.ExtractContent -- bounded below by
// processing's own DispatchedEventID (the monotonic events.id watermark
// stamped at dispatch, NOT a created_at/dispatched_at timestamp comparison:
// the latter straddles the Postgres server clock and the application
// clock, so a few ms of ordinary skew between them could truncate the scan
// early or run it past this turn's own boundary into an earlier turn's
// output -- see migrations/000089_turns_dispatched_event_id.up.sql), and
// UNBOUNDED above (upperBoundEventID nil): this method only ever runs
// synchronously right when processing itself completes, when it IS -- by
// construction -- the most recently dispatched turn in the session, so no
// LATER turn's own token events exist yet to contaminate an unbounded-above
// scan (see plandomain.ExtractContent's own doc comment for the case where
// that assumption does NOT hold, and why that caller supplies a real upper
// bound instead). Never fails the caller: any read error, or finding
// nothing at all, returns planContentFallbackText rather than propagating
// an error -- a best-effort notification enrichment must never block or
// fail the turn-completion transaction it runs inside.
func (a *Actor) planContentText(ctx context.Context, processing sqlcgen.Turn) string {
	events, err := a.stores.event.ListRecentForSession(ctx, a.sessionID, planContentEventFetchLimit)
	if err != nil {
		a.logger.Warn("sessionactor: list events for plan content extraction failed", "error", err)
		return planContentFallbackText
	}

	return plandomain.ExtractContent(ToContentEvents(events), processing.DispatchedEventID, nil)
}

// ToContentEvents converts a []sqlcgen.Event (newest-first, as
// ListRecentForSession returns) into plandomain.ExtractContent's own
// adapter-independent input shape -- the ONE conversion point the plan-mode
// UI's own second caller (internal/adapters/inbound/httpapi/plans.go) also reuses,
// so the sqlcgen.Event -> plandomain.ContentEvent boundary conversion
// itself never drifts between the two call sites either. A "token" event
// whose payload fails to decode degrades to an empty Text (silently
// skipped by ExtractContent, exactly like this function's own prior
// inline `continue` on a decode error), never propagated as an error --
// matching this file's own "never fails the caller" discipline.
func ToContentEvents(events []sqlcgen.Event) []plandomain.ContentEvent {
	out := make([]plandomain.ContentEvent, len(events))
	for i, e := range events {
		ce := plandomain.ContentEvent{ID: e.ID, Type: e.Type}
		if e.Type == "token" {
			var tok tokenEventPayload
			if err := json.Unmarshal(e.Payload, &tok); err == nil {
				ce.Text = tok.Text
			}
		}
		out[i] = ce
	}
	return out
}
