// This file (content.go) implements the plan-mode UI's own generalization of
// internal/app/sessionactor/planapprovalcontent.go's pre-existing plan-
// content-extraction algorithm (built for §8.1's Slack/Linear cross-channel
// notifications) so a SECOND caller -- the web UI's GET /api/sessions/:id/
// plans, internal/adapters/inbound/httpapi/plans.go -- can reuse the exact
// same scan, never a drifted re-implementation.
//
// The original algorithm (what ExtractContent below does when called with
// upperBoundEventID == nil, except that it now orders text parts by their
// first frame rather than by their newest, see its doc comment) was built
// to recover exactly ONE turn's own content: the plan-mode turn that JUST
// completed, called synchronously from inside that turn's own completion
// transaction, when it is -- by construction -- the most recently
// dispatched turn in the session, so scanning newest-first down to its own
// DispatchedEventID can
// never run into a LATER turn's own token events (none exist yet).
//
// A plan-mode UI rendering a session's own FULL plan history (v1, v2, ...)
// breaks that assumption: an older, already-superseded/decided plan
// version's own producing turn is NOT the most recently dispatched turn in
// the session by the time anyone asks for it (at minimum, the turn that
// produced the NEXT plan version already ran after it; for an APPROVED
// plan, the approval-dispatched IMPLEMENTATION turn also ran after it,
// §8.1's "on approval, the implementation turn is dispatched server-side").
// Scanning unbounded-above for such a turn would silently return a LATER
// turn's own streamed text instead -- wrong, not merely incomplete, so
// upperBoundEventID exists specifically to prevent it: the caller supplies
// the NEXT turn dispatched in the same session (by any turn, not only a
// plan-mode one), if one exists yet, and this function excludes everything
// at or above it.

package plan

import (
	"sort"

	"github.com/narvidev/narvi/internal/domain/framecut"
)

// ContentFallbackText is the fixed, honest placeholder ExtractContent
// returns when no token event could be recovered inside the requested
// window -- shared by every caller (the Slack/Linear cross-channel
// notifiers, internal/app/sessionactor/planapprovalcontent.go, and the web
// UI's own plans.go) so neither can drift to a DIFFERENT wording for the
// SAME "we tried and found nothing" case. Verbatim copy of
// planapprovalcontent.go's own pre-existing planContentFallbackText value
// -- that file now delegates to this constant instead of keeping its own,
// see this Step's own change to that file.
const ContentFallbackText = "(plan content unavailable -- see the session's own event log)"

// ContentEvent is the minimal, adapter-independent shape ExtractContent
// needs from one of a session's own persisted events -- callers convert
// their own adapter-layer event rows (sqlcgen.Event) at the boundary,
// mirroring this package's own established "plain types, callers convert"
// convention (plan.go's own top doc comment: "domain packages never import
// adapter types"). Text is the event's own ALREADY-DECODED "token" payload
// text (contracts/sandbox-ws/v1/events.schema.json's own Token.text,
// cumulative per messageId per §6.1) for a "token"-typed event, or the
// empty string for every other event type -- this package never parses raw
// JSON payload bytes itself, exactly like every other domain package's "no
// I/O" boundary (§11).
//
// MessageID is the same payload's own Token.messageId: the id of the text
// part the frame belongs to, which every frame of that part carries. It is
// the PAYLOAD's id, not the events.message_id storage key, which carries a
// per-frame suffix on every stored frame of a part but its first
// (internal/app/sessionactor/tokenframe.go). Empty for every other event
// type.
//
// Cut is the frame's `cut` property (framecut.DecodeCut), nil for a whole
// frame and for every other event type: a frame the sandbox-agent cut on
// its way to the control plane records it there, and this package learns a
// cut only from it, never from the text (technical plan §6.1).
type ContentEvent struct {
	ID        int64
	Type      string
	MessageID string
	Text      string
	Cut       *framecut.Cut
}

// Final is a turn's final text as FinalText reads it: the text, and the
// cut its frame records when the part's text is a cut frame (nil when it
// is whole). A surface states the cut from Cut, never from the text, which
// it may shorten for display.
type Final struct {
	Text string
	Cut  *framecut.Cut
}

// ExtractContent recovers the final, rendered assistant text of ONE
// plan-mode turn's own streamed output: FinalText's text, or
// ContentFallbackText when FinalText finds none. See FinalText for the
// window, the bounds, and which text is the turn's final one.
//
// Never fails: finding nothing in-window returns ContentFallbackText, with
// no cut, exactly like the original best-effort extraction this
// generalizes.
func ExtractContent(events []ContentEvent, lowerBoundEventID, upperBoundEventID *int64) Final {
	if final, ok := FinalText(events, lowerBoundEventID, upperBoundEventID); ok {
		return final
	}
	return Final{Text: ContentFallbackText}
}

// FinalText recovers the final, rendered assistant text of ONE turn's
// own streamed output from events (a session's own event
// log, scanned NEWEST-EVENT-FIRST -- callers MUST supply it already sorted
// that way, mirroring EventStore.ListRecentForSession's own descending-id
// query; this function never sorts).
//
// lowerBoundEventID is the producing turn's own DispatchedEventID
// (exclusive: an event AT OR BELOW it belongs to an EARLIER turn and ends
// the scan, via break, since everything after it in this descending walk
// is even older). nil is accepted defensively as "no lower bound" (scan
// all the way to the oldest supplied event) but is not a real case in
// practice -- every dispatched turn has one.
//
// upperBoundEventID is the NEXT turn dispatched in this SAME session after
// the producing turn, if any -- its own DispatchedEventID, the SAME kind of
// value lowerBoundEventID is, just one turn later. Since a turn's own
// DispatchedEventID is the events-log watermark that existed BEFORE any of
// its events were produced (see this package's own callers, e.g.
// planapprovalcontent.go's ToContentEvents doc comment), an event AT
// EXACTLY upperBoundEventID still belongs to THIS turn (it is <= the next
// turn's own lower bound, which is the same "at or below belongs to the
// earlier turn" rule lowerBoundEventID applies one turn down) -- only an
// event STRICTLY GREATER than it belongs to that later turn and is skipped,
// via continue. Getting this boundary inclusive-at-the-edge wrong here
// silently drops the producing turn's own LAST event whenever the very
// next turn happened to dispatch immediately after it with no intervening
// activity -- caught live by this package's own real-Postgres integration
// test (httpapi/plans_integration_test.go), not by a unit test alone,
// because ContentEvent IDs are hand-picked in-package but the actual
// events.id sequence (and thus this off-by-one) only appears once turns
// dispatch back-to-back for real. nil means "no later turn exists yet" --
// this is the session's own most-recently-dispatched turn, exactly the
// case planapprovalcontent.go's own pre-existing algorithm was built for.
//
// Within the window, the text is the turn's LAST text part: the part
// whose first in-window frame is newest, among the parts with at least one
// non-empty frame there, read as framecut.PartText reads its non-empty
// frames: the newest that yields to no other (§6.1: token text is
// cumulative per messageId, so with no cut that is the newest non-empty
// frame, the part's full text so far). A cut frame yields to the whole text
// it was taken from, and to a cut of that text keeping more, whatever order
// they were stored in, so a part stored whole and then cut reads whole; a
// part whose text is a cut frame reads as that frame, its marker included,
// and Final.Cut reports the cut. A part is ordered by its FIRST frame, not
// its newest, because a
// part's later frame is not always stored right behind it: the turn
// running when the control plane is deployed onto per-frame storage gets
// its parts' later frames back from the sandbox's replay, stored at the
// tail of its window, and a sandbox-agent built before that deploy can
// send a later part's frame live AHEAD of an earlier part's replayed one
// (internal/app/sessionactor/tokenframe.go, "The turn running at deploy").
// The newest row alone would then name the earlier part. This is the order
// the web timeline shows (web/src/session/timelineModel.ts places a part
// where its first frame is) and the one the session actor keys a part's
// window on (GetLatestTokenFrameForPart's first_id). A part whose frames in
// the window are all empty -- one that opened and whose later frames never
// reached the log -- has no text to offer and is passed over, exactly as
// an empty row always was.
//
// It is the ONE reader of a turn's final text: plan content
// (ExtractContent, for the plan views, the approval snapshot and the
// cross-channel notifiers) and a session's result summary (row 182,
// technical plan §43.20) both read through it, so the two can never
// disagree about which text a turn ended on. ok is false when no text part
// in the window has any text; the Final is then empty, and the caller says
// so its own way (ExtractContent with ContentFallbackText, the result with
// a null summary). Never fails.
func FinalText(events []ContentEvent, lowerBoundEventID, upperBoundEventID *int64) (Final, bool) {
	type frame struct {
		id    int64
		frame framecut.Frame
	}
	type textPart struct {
		firstID int64   // its oldest in-window frame, empty or not
		frames  []frame // its non-empty in-window frames, in scan order
	}
	var parts []*textPart // in scan order, so the pick below is deterministic
	byMessageID := make(map[string]*textPart)
	for _, e := range events {
		if upperBoundEventID != nil && e.ID > *upperBoundEventID {
			continue
		}
		if lowerBoundEventID != nil && e.ID <= *lowerBoundEventID {
			break
		}
		if e.Type != "token" {
			continue
		}
		p, ok := byMessageID[e.MessageID]
		if !ok {
			p = &textPart{firstID: e.ID}
			byMessageID[e.MessageID] = p
			parts = append(parts, p)
		}
		p.firstID = min(p.firstID, e.ID)
		if e.Text != "" {
			p.frames = append(p.frames, frame{id: e.ID, frame: framecut.Frame{Text: e.Text, Cut: e.Cut}})
		}
	}

	var last *textPart
	for _, p := range parts {
		if len(p.frames) > 0 && (last == nil || p.firstID > last.firstID) {
			last = p
		}
	}
	if last == nil {
		return Final{}, false
	}
	// framecut.PartText takes the frames in id order; callers supply the
	// events newest first, but need not be strict about it.
	sort.SliceStable(last.frames, func(i, j int) bool { return last.frames[i].id < last.frames[j].id })
	inOrder := make([]framecut.Frame, len(last.frames))
	for i, f := range last.frames {
		inOrder[i] = f.frame
	}
	picked, ok := framecut.PartText(inOrder)
	if !ok {
		return Final{}, false
	}
	return Final(picked), true
}
