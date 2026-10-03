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
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/framecut"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
)

// planContentFallbackText is a local alias for plandomain.ContentFallbackText
// -- kept as its own name here (rather than every call site in this
// package spelling out the qualified name) purely for this file's own
// pre-existing readability; the two are byte-identical by construction
// (plandomain.go's own doc comment: "shared by every caller ... so neither
// can drift to a DIFFERENT wording").
const planContentFallbackText = plandomain.ContentFallbackText

// planContentEventFetchLimit bounds how many `token` frames of one turn's
// window of the log a reader of its final text reads back
// (EventStore.ListTokenFramesInWindow, newest first, ReadWindowFinal):
// generous for a single turn's own text parts -- two frames a part on the
// pinned runtime -- while still a fixed, safe upper bound rather than an
// unbounded scan. It bounds the turn's own frames, never the session's
// tail: every reader of a turn's text reads that turn's window, of `token`
// frames only, so neither the history before the turn nor what the session
// logged after it -- the tool calls, results and step ends a turn stores
// since technical plan §6.1's "Stored tool events", about four rows a step
// -- can push the turn's text out of what is read, and no tool payload is
// read along with it.
const planContentEventFetchLimit = 2000

// tokenEventPayload is the minimal shape this function reads out of a
// "token" event's own raw payload (contracts/sandbox-ws/v1/events.schema.
// json's own Token shape) -- Text is CUMULATIVE per messageId (§6.1: "text
// is CUMULATIVE, not a delta"), so a text part's newest frame is already
// its full text so far. MessageID is the part's id, which every frame of
// the part carries in its payload, unlike the events.message_id storage
// key (sessionactor/tokenframe.go): plandomain.ExtractContent groups the
// frames by it to tell which part opened last, since the newest row
// is not always the last part's (see that function's doc comment).
//
// Cut is the frame's `cut` property, raw (§6.1): a frame the sandbox-agent
// cut on its way to the control plane records what it kept there, and
// ToContentEvents decodes it fail-closed (framecut.DecodeCut), so a reader
// learns a cut only from it, never from the text.
type tokenEventPayload struct {
	Type      string          `json:"type"`
	MessageID string          `json:"messageId"`
	Text      string          `json:"text"`
	Cut       json.RawMessage `json:"cut"`
}

// planContentText best-effort recovers processing's own final streamed
// assistant text -- the plan document's own actual content -- by reading
// the `token` frames of processing's own window of the event log, NEWEST
// FIRST (ReadWindowFinal over a.stores.event, pool-based per EventStore's
// own doc comment, so this is always a plain read of already-committed
// rows, safe to call from inside completeProcessingTurn's own transact),
// and scanning them as plandomain.ExtractContent does -- bounded below by
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
// nothing at all, returns planContentFallbackText, with no cut, rather than
// propagating an error -- a best-effort notification enrichment must never
// block or fail the turn-completion transaction it runs inside.
//
// The Final's Cut reports a plan whose text is a frame the sandbox-agent
// cut on its way here (§6.1): its notices offer no Approve
// (outboxenqueue.go), since the approval would be refused (httpapi.
// ErrPlanCut). A cut is settled by the time this runs: the notice goes out
// as the turn completes, and no `token` frame adds a row once its turn has
// ended (tokenframe.go).
//
// It reads the turn's own window rather than the session's newest 2000
// events: a turn that streams its plan and then calls tools stores about
// four rows a step after the text, and enough of them pushed the text out
// of the tail, where the window keeps it.
func (a *Actor) planContentText(ctx context.Context, processing sqlcgen.Turn) plandomain.Final {
	final, found, err := ReadWindowFinal(ctx, a.stores.event, a.sessionID, processing.DispatchedEventID, nil)
	if err != nil {
		a.logger.Warn("sessionactor: list token frames for plan content extraction failed", "error", err)
		return plandomain.Final{Text: planContentFallbackText}
	}
	if !found {
		return plandomain.Final{Text: planContentFallbackText}
	}
	return final
}

// ToContentEvents converts a []sqlcgen.Event (newest-first, as
// ListTokenFramesInWindow and ListRecentForSession return them) into
// plandomain.ExtractContent's own adapter-independent input shape -- the ONE
// conversion point every reader of a turn's text goes through
// (ReadWindowFinal, and the tests that read a session's tail), so the
// sqlcgen.Event -> plandomain.ContentEvent boundary conversion itself never
// drifts between call sites. A "token" event
// carries its payload's messageId (the text part's id) and text; every
// other event type carries neither. A "token" event whose payload fails to
// decode degrades to an empty Text (silently skipped by ExtractContent,
// exactly like this function's own prior inline `continue` on a decode
// error), never propagated as an error -- matching this file's own "never
// fails the caller" discipline. A "token" event's `cut` is decoded
// fail-closed (framecut.DecodeCut): one present but unreadable is
// framecut.Malformed, read as a cut, never as a whole frame.
func ToContentEvents(events []sqlcgen.Event) []plandomain.ContentEvent {
	out := make([]plandomain.ContentEvent, len(events))
	for i, e := range events {
		ce := plandomain.ContentEvent{ID: e.ID, Type: e.Type}
		if e.Type == "token" {
			var tok tokenEventPayload
			if err := json.Unmarshal(e.Payload, &tok); err == nil {
				ce.MessageID = tok.MessageID
				ce.Text = tok.Text
				ce.Cut = framecut.DecodeCut(tok.Cut)
			}
		}
		out[i] = ce
	}
	return out
}

// TurnContentBounds returns plandomain.FinalText's own (lower, upper)
// bounds for turnID, given every turn dispatched in the session so far (any
// order, any kind -- an approval-dispatched IMPLEMENTATION turn counts
// exactly like a plan-producing one): lower is turnID's own
// DispatchedEventID; upper is the DispatchedEventID of whichever
// DISPATCHED turn ran next in the session, if any (nil when turnID's own
// turn is the most recently dispatched one so far). ok is false when
// turnID names no turn in sessionTurns with a DispatchedEventID set at all
// -- should be unreachable for any real plan's producing turn (a plan row
// is only ever created once its producing turn has already been
// dispatched), but is surfaced as a plain bool rather than a panic or a
// silent unbounded scan, so every caller degrades the SAME honest way
// (plandomain.ContentFallbackText) instead of assuming it can't happen.
//
// The one implementation of these bounds: the plan views, the approved
// snapshot and the approval's cut check (internal/adapters/inbound/
// httpapi), a session's result summary, and the decision inbox's cut report
// (internal/app/decisioninbox) all window a turn's text by it, an algorithm
// this codebase has already been bitten by an off-by-one in once (see
// plandomain.FinalText's own doc comment) -- never a second, independently
// re-derived copy that can silently drift from this one.
func TurnContentBounds(sessionTurns []sqlcgen.Turn, turnID pgtype.UUID) (lower, upper *int64, ok bool) {
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

// ReadPlanFinal reads the final text of turnID, a plan's producing turn in
// sessionID, as plandomain.FinalText reads it, with the cut it reports: the
// turn's window of the log (TurnContentBounds, over every turn turns lists
// for the session) and that window's `token` frames
// (EventStore.ListTokenFramesInWindow, up to planContentEventFetchLimit,
// newest first). found is false when the turn has no window or the window
// no text; the Final is then empty, and the caller says so its own way
// (plandomain.ContentFallbackText for a snapshot).
//
// The approval's cut check and the snapshot it writes read the plan
// through it, in one read (httpapi.DecidePlanOnTx, which passes its own
// transaction's TurnStore), and so does the decision inbox's cut report, so
// the inbox never offers Approve for a plan the approval would refuse.
// Reading the window (ReadWindowFinal) rather than the session's newest
// events finds a plan's text however much the session has logged since.
func ReadPlanFinal(ctx context.Context, turns *postgres.TurnStore, events *postgres.EventStore, sessionID, turnID pgtype.UUID) (final plandomain.Final, found bool, err error) {
	sessionTurns, err := turns.ListForSession(ctx, sessionID)
	if err != nil {
		return plandomain.Final{}, false, fmt.Errorf("sessionactor: list turns for plan final text: %w", err)
	}
	lower, upper, ok := TurnContentBounds(sessionTurns, turnID)
	if !ok {
		return plandomain.Final{}, false, nil
	}
	return ReadWindowFinal(ctx, events, sessionID, lower, upper)
}

// ReadWindowFinal reads the final text of the turn whose window of
// sessionID's log is (lower, upper] -- TurnContentBounds' bounds, upper nil
// for the session's most recently dispatched turn -- as plandomain.FinalText
// reads it, with the cut it reports: the window's `token` frames, newest
// first, up to planContentEventFetchLimit (EventStore.ListTokenFramesInWindow,
// a range of events_session_id_id_idx bounded by the turn, of which only
// the token rows are read). A nil lower reads from the log's start, as
// FinalText reads a nil lower bound. found is false when the window holds
// no text.
//
// Every reader of a turn's text reads it here -- the approval and its
// snapshot, the decision inbox, the plan views' fallback, the session
// result's summary and the plan notices (planContentText) -- so none reads
// the session's newest 2000 events of every type, whose window a later
// turn's tool activity pushed an earlier turn's text out of, and whose read
// carried every tool payload in it.
func ReadWindowFinal(ctx context.Context, events *postgres.EventStore, sessionID pgtype.UUID, lower, upper *int64) (final plandomain.Final, found bool, err error) {
	var lowerID int64
	if lower != nil {
		lowerID = *lower
	}
	frames, err := events.ListTokenFramesInWindow(ctx, sessionID, lowerID, upper, planContentEventFetchLimit)
	if err != nil {
		return plandomain.Final{}, false, fmt.Errorf("sessionactor: list token frames for a turn's final text: %w", err)
	}
	final, found = plandomain.FinalText(ToContentEvents(frames), lower, upper)
	return final, found, nil
}

// ReadPlanCut reads the cut report of the plan planID (ReadPlanFinal, over
// the plan's own producing turn): nil for a plan whose text is whole. The
// Slack and Linear replies to a message that neither decides nor revises a
// plan awaiting approval read it, so they offer no approve keyword or
// button for a plan the approval would refuse (httpapi.ErrPlanCut).
func ReadPlanCut(ctx context.Context, plans *postgres.PlanStore, turns *postgres.TurnStore, events *postgres.EventStore, planID pgtype.UUID) (*framecut.Cut, error) {
	plan, err := plans.Get(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: read plan for its cut report: %w", err)
	}
	final, _, err := ReadPlanFinal(ctx, turns, events, plan.SessionID, plan.TurnID)
	if err != nil {
		return nil, err
	}
	return final.Cut, nil
}
