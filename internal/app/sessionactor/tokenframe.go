package sessionactor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// This file decides how one streamed `token` frame reaches the append-only
// event log (§6.1).
//
// A `token` event carries the CUMULATIVE text of one assistant text part,
// keyed by the part's own id as its wire messageId (the opencode adapter's
// translateToken), and the runtime sends several frames per part. The
// pinned runtime, measured against the real binary
// (opencode/tokencadence_realbinary_test.go), sends exactly two: an empty
// frame when the part opens and the full text when it closes, however many
// deltas the model streamed in between.
//
// Every other event type is stored under its wire messageId, first wins
// (CreateEvent's ON CONFLICT, migrations/000019). Applied to `token`, that
// rule kept the FIRST frame of every part and silently swallowed the rest:
// the rows read as blank or as a prefix, the later frames were never
// broadcast either (appendRawEvent broadcasts only an inserted row), and a
// plan-approval notification or approved plan snapshot built from them
// froze that text. That was the case from 2026-07-20 until this file; what
// becomes of the history stored in that window is under "The turn running
// at deploy" below.
//
// Replacing the stored payload in place was rejected: the row would keep
// its id, so every `id > cursor` reader (the web client's backfill,
// fetch_history, REST ?cursor=) would never see the newer text, and a
// frame replayed out of order would overwrite a newer one. Each DISTINCT
// frame is instead stored as its own row, so the log stays append-only and
// every frame is broadcast once. Readers are unchanged: they fold `token`
// rows by the PAYLOAD's messageId, newest id wins (web timelineModel,
// plan.ExtractContent); the storage key never leaves the store.
//
// # Storage keys, and the binary before this one
//
// A part's FIRST stored frame is stored under its bare wire messageId, the
// key every other event is stored under; each later distinct frame under
// messageId + "#" + a hash of its text (tokenFrameStorageKey). A
// byte-identical resend adds no row: a later frame's key dedupes on
// 000019's unique index, and the first frame again -- keyed like a later
// frame once the part is stored, so its bare key no longer matches -- is
// refused by tokenFrameAddsNoRow.
//
// The bare key on the first frame is a compatibility contract. A control
// plane built before this file stores EVERY frame of a part under that
// bare key, first wins, with no turn check, and it can still receive a
// sandbox's replay after this binary has stored frames: after a rollback
// (migrations/000144 ships its down migration for exactly that), or in a
// mixed fleet during a rolling deploy (deploy/control-plane runs two
// replicas) when a new pod gives a session up while an old one still
// serves. Since every part this binary stores holds its first frame under
// the bare key, that binary's ON CONFLICT swallows every replayed frame of
// it, as it does its own. Hashing the first frame too would leave that
// binary no row to match: it would append each replayed frame at the tail
// of the log -- the phantom live turn "Only while its turn is live"
// describes, through a binary that runs none of the rules below.
// TestHandleSandboxEvent_TokenFrames_PreviousBinaryReplayAddsNoRow replays
// every frame through that binary's exact storage call. A part stored
// before this file has the same layout -- its one row, the first frame,
// under the bare key -- so there is one layout whichever binary wrote it.
//
// Rows per part are bounded by the runtime's frame cadence -- two for the
// pinned runtime, one of them empty, so a part's stored bytes stay linear
// in its final length. A runtime bump that sends a frame per delta would
// make both rows and bytes grow with every delta, cumulative bytes
// quadratically; the real-binary test above fails first, and adapter-side
// coalescing is the answer then, not a change here.
//
// # Only while its turn is live
//
// A frame adds a row only while the turn that produced it is the one
// Processing. The log has no other way to say which turn a row belongs
// to: readers place a row by its id. The web timeline
// (web/src/session/timelineModel.ts) closes a turn at its
// execution_complete and opens a new one at the next turn-scoped event,
// and plan.ExtractContent reads a turn's text from the rows above its
// dispatched_event_id. So a frame stored after its turn ended does not
// repair that turn: it lands at the tail of the log, where the timeline
// shows it as a new turn that is still running (which also disables the
// composer, since a turn looks open) and a later turn's window reads it
// as that turn's text. The log is append-only, so the damage would be
// permanent.
//
// Late frames are not hypothetical. The sandbox-agent keeps best-effort
// entries in its outbound buffer until they are evicted (up to 1000 of
// them, internal/sandboxagent/wsbridge) and replays the whole buffer on
// every reconnect, so a control-plane restart gets back every frame the
// sandbox still holds -- including, on the first reconnect after this
// file shipped, the full-text frames the first-wins rule had swallowed. A
// frame whose transaction failed comes back the same way.
//
// The rule, read inside the actor's transaction, under the session row
// lock every event insert takes before drawing its id (CreateEvent,
// queries/events.sql), so ids are in commit order within the session:
//   - no turn Processing: no row. Every frame of a turn arrives while it
//     Processes -- dispatch moves it Pending -> Dispatched -> Processing in
//     one transaction, before the prompt is sent, and the runtime sends a
//     part's frames before the turn's execution_complete -- so a frame
//     with no live turn is a late one.
//   - the part's FIRST stored frame at or below the Processing turn's
//     dispatched_event_id (the log's high-water mark when it was
//     dispatched, migrations/000089): the part entered the log before this
//     turn existed, so it belongs to an earlier one. No row. The first
//     frame decides, not the newest: a part cannot begin in one turn and
//     continue in the next.
//
// A late frame of a part NONE of whose frames was ever stored cannot be
// placed: a `token` names no turn on the wire (§6.1's contract is fixed),
// so if a later turn is Processing when it arrives, it is stored in that
// turn's window. That takes a lost first frame AND a replay during a
// later turn, and it is the same for every event type whose first
// delivery failed; it is not something this file introduced.
//
// A NULL dispatched_event_id places no window, exactly as
// plan.ExtractContent reads a NULL lower bound: every dispatch stamps it
// in the same write that makes the turn Processing (dispatch.go,
// tryPlanDispatch and tryPlanReenqueue), so a Processing turn without one
// does not occur outside tests that seed a turn row directly.
//
// # The turn running at deploy
//
// History stored before this file keeps one row per part, its first frame,
// and the sandbox replays the rest on its first reconnect to this binary.
// For a turn that had ended by then, the rules above refuse every such
// frame: that history stays truncated. The turn still Processing when the
// control plane restarts onto this binary is different: nothing ends it
// (a rolling deploy fails no session, §9.3), a reconnect at the same gen
// does not move its dispatched_event_id, and its parts' first frames lie
// above that watermark -- so the rules admit the replayed frames, and each
// part's later frames are stored as new rows at the tail of the turn's own
// window, after the steps that followed the part. That is what the rules
// are for: the frames belong to the live turn, and plan.ExtractContent
// reads the right text, since the replay keeps the sandbox's order. The
// web timeline places a part where its FIRST row is, not where a later row
// lands (timelineModel.ts), so the recovered text shows in the step the
// part belongs to. TestHandleSandboxEvent_TokenFrames_
// ReplayDuringTheTurnRunningAtDeploy pins the stored rows, and the web
// test reads the same rows from its checked-in fixture.

// tokenFrameKeyHashBytes is how many bytes of the text's SHA-256 the
// storage key keeps: 128 bits, so two distinct frames of one part never
// collide in practice, at a key length (the part id, "#", 32 hex
// characters) far below anything the index minds.
const tokenFrameKeyHashBytes = 16

// tokenFrameStorageKey is the events.message_id a `token` frame is stored
// under. The part's first frame (partStored false: no frame of it is
// stored yet) keeps the bare part id, the wire messageId, which a binary
// predating per-frame keys dedupes every frame of the part on (see
// "Storage keys, and the binary before this one" above). Every later frame
// is keyed by the part id, "#", then a hash of its text: two later frames
// with different text get different keys, so each is appended, and the
// same later frame sent twice gets the same key, so the second is deduped
// exactly as a resend of any other event type is.
func tokenFrameStorageKey(partID, text string, partStored bool) string {
	if !partStored {
		return partID
	}
	sum := sha256.Sum256([]byte(text))
	return partID + "#" + hex.EncodeToString(sum[:tokenFrameKeyHashBytes])
}

// tokenPartFromEarlierTurn reports whether a stored part whose first frame
// has id firstFrameID entered the log before the Processing turn whose
// dispatched_event_id is dispatchedEventID -- i.e. belongs to an earlier
// turn, so that a late frame of it must add no row (see "Only while its
// turn is live" above). Every event the Processing turn produces has an id
// above its dispatched_event_id; nil places no window.
func tokenPartFromEarlierTurn(firstFrameID int64, dispatchedEventID *int64) bool {
	return dispatchedEventID != nil && firstFrameID <= *dispatchedEventID
}

// tokenFrameAddsNoRow reports whether a `token` frame whose text is
// incoming must add no row, given first and latest, the texts of the first
// and the newest frames already stored for the same part.
//
// It adds no row when it is either of those frames again -- the first one
// is stored under the bare part id, which a resend of it no longer matches
// by key (tokenFrameStorageKey), and every other stored frame dedupes on
// its key anyway -- or when it is an OLDER frame of the part replayed
// late: a strict prefix of latest that is missing some non-whitespace
// text. That happens when an earlier frame failed to persist while a
// later one succeeded, and the sandbox then replays its buffer after a
// reconnect. Storing it would make the older text the newest row, which
// is exactly what every reader shows.
//
// A strict prefix that is missing only whitespace is admitted: a runtime
// may trim a part's trailing whitespace when the part closes, making its
// final frame shorter than the one before. The pinned runtime was measured
// not to (its final frame keeps the trailing newlines), and a stale frame
// of that shape shows exactly the same text, so admitting it costs
// nothing either way.
//
// A frame that is not a prefix of latest at all is a rewrite, not a
// replay, and adds its row: the newest arrival wins, as it does for every
// other reader of the log.
func tokenFrameAddsNoRow(incoming, first, latest string) bool {
	if incoming == latest || incoming == first {
		return true
	}
	if len(incoming) > len(latest) || !strings.HasPrefix(latest, incoming) {
		return false
	}
	return strings.TrimSpace(latest[len(incoming):]) != ""
}

// appendTokenFrame stores one inbound `token` frame inside tx: the
// token-only branch of handleSandboxEvent's "persist ALWAYS" step, which
// calls appendRawEvent directly for every other type. Returns whether a
// row was inserted (and so queued for broadcast), exactly like
// appendRawEvent: false for a deduped resend, for a frame that arrives
// with no turn Processing or belongs to an earlier turn's part (see "Only
// while its turn is live" above), and for a frame tokenFrameAddsNoRow
// rejects.
//
// The part id is read from the payload itself -- the same bytes wshub
// took cmd.MessageID from, and the value events_token_part_idx indexes --
// so the storage key and the stored-part lookup always name the same
// part. A payload whose fields cannot be decoded (never produced by a real
// sandbox: the wire schema requires both as strings) is stored under
// cmd.MessageID, first wins, as every frame was before per-frame keys
// existed, so it is still persisted rather than dropped, provided a turn
// is Processing.
func (a *Actor) appendTokenFrame(ctx context.Context, tx pgx.Tx, cmd SandboxEvent) (bool, error) {
	processing, err := a.stores.turn.WithTx(tx).GetProcessingTurnForSession(ctx, a.sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		a.logger.Debug("sessionactor: token frame with no turn processing; adding no row",
			"message_id", cmd.MessageID)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sessionactor: read processing turn for token frame: %w", err)
	}

	var frame struct {
		MessageID string `json:"messageId"`
		Text      string `json:"text"`
	}
	if err := json.Unmarshal(cmd.Raw, &frame); err != nil {
		a.logger.Warn("sessionactor: token frame not decodable; storing it under its wire messageId",
			"message_id", cmd.MessageID, "error", err)
		return a.appendRawEvent(ctx, tx, cmd.Type, cmd.MessageID, cmd.Raw)
	}

	part, found, err := a.stores.event.WithTx(tx).StoredTokenPart(ctx, a.sessionID, frame.MessageID)
	if err != nil {
		return false, fmt.Errorf("sessionactor: read stored token part: %w", err)
	}
	if found {
		if tokenPartFromEarlierTurn(part.FirstFrameID, processing.DispatchedEventID) {
			a.logger.Debug("sessionactor: token frame of a part from an earlier turn; adding no row",
				"message_id", frame.MessageID, "first_frame_id", part.FirstFrameID,
				"turn_id", processing.ID.String())
			return false, nil
		}
		if tokenFrameAddsNoRow(frame.Text, part.FirstText, part.LatestText) {
			return false, nil
		}
	}
	return a.appendRawEvent(ctx, tx, cmd.Type, tokenFrameStorageKey(frame.MessageID, frame.Text, found), cmd.Raw)
}
