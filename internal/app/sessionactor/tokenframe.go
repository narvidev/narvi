package sessionactor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// froze that text. That was the case from 2026-07-20 until this file, and
// the history stored in that window cannot be repaired: the later frames
// were never written anywhere.
//
// Replacing the stored payload in place was rejected: the row would keep
// its id, so every `id > cursor` reader (the web client's backfill,
// fetch_history, REST ?cursor=) would never see the newer text, and a
// frame replayed out of order would overwrite a newer one. Each DISTINCT
// frame is instead stored as its own row, under the storage key
// tokenFrameStorageKey builds, so the log stays append-only, every frame
// is broadcast once, and a byte-identical resend still dedupes on
// 000019's unique index. Readers are unchanged: they fold `token` rows by
// the PAYLOAD's messageId, newest id wins (web timelineModel,
// plan.ExtractContent); the storage key never leaves the store.
//
// Rows per part are bounded by the runtime's frame cadence -- two for the
// pinned runtime, one of them empty, so a part's stored bytes stay linear
// in its final length. A runtime bump that sends a frame per delta would
// make both rows and bytes grow with every delta, cumulative bytes
// quadratically; the real-binary test above fails first, and adapter-side
// coalescing is the answer then, not a change here.

// tokenFrameKeyHashBytes is how many bytes of the text's SHA-256 the
// storage key keeps: 128 bits, so two distinct frames of one part never
// collide in practice, at a key length (the part id, "#", 32 hex
// characters) far below anything the index minds.
const tokenFrameKeyHashBytes = 16

// tokenFrameStorageKey is the events.message_id a `token` frame is stored
// under: its part id (the wire messageId), "#", then a hash of its text.
// Two frames of the same part with different text get different keys, so
// each is appended; the same frame sent twice gets the same key, so the
// second is deduped exactly as a resend of any other event type is.
func tokenFrameStorageKey(partID, text string) string {
	sum := sha256.Sum256([]byte(text))
	return partID + "#" + hex.EncodeToString(sum[:tokenFrameKeyHashBytes])
}

// tokenFrameAddsNoRow reports whether a `token` frame whose text is
// incoming must add no row, given latest, the text of the newest frame
// already stored for the same part.
//
// It adds no row when it is that same frame again (including a frame
// stored before per-frame keys existed, which a resend no longer matches
// by key), or when it is an OLDER frame of the part replayed late: a
// strict prefix of latest that is missing some non-whitespace text. That
// happens when an earlier frame failed to persist while a later one
// succeeded, and the sandbox then replays its buffer after a reconnect.
// Storing it would make the older text the newest row, which is exactly
// what every reader shows.
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
func tokenFrameAddsNoRow(incoming, latest string) bool {
	if incoming == latest {
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
// appendRawEvent: false for a deduped resend and for a frame
// tokenFrameAddsNoRow rejects.
//
// The part id is read from the payload itself -- the same bytes wshub
// took cmd.MessageID from, and the value events_token_part_idx indexes --
// so the storage key and the latest-frame lookup always name the same
// part. A payload whose fields cannot be decoded (never produced by a real
// sandbox: the wire schema requires both as strings) is stored under
// cmd.MessageID as it was before per-frame keys existed, so it is still
// persisted rather than dropped.
func (a *Actor) appendTokenFrame(ctx context.Context, tx pgx.Tx, cmd SandboxEvent) (bool, error) {
	var frame struct {
		MessageID string `json:"messageId"`
		Text      string `json:"text"`
	}
	if err := json.Unmarshal(cmd.Raw, &frame); err != nil {
		a.logger.Warn("sessionactor: token frame not decodable; storing it under its wire messageId",
			"message_id", cmd.MessageID, "error", err)
		return a.appendRawEvent(ctx, tx, cmd.Type, cmd.MessageID, cmd.Raw)
	}

	latest, found, err := a.stores.event.WithTx(tx).LatestTokenFrameText(ctx, a.sessionID, frame.MessageID)
	if err != nil {
		return false, fmt.Errorf("sessionactor: read latest token frame: %w", err)
	}
	if found && tokenFrameAddsNoRow(frame.Text, latest) {
		return false, nil
	}
	return a.appendRawEvent(ctx, tx, cmd.Type, tokenFrameStorageKey(frame.MessageID, frame.Text), cmd.Raw)
}
