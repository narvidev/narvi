package wsbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/services"
)

// SendCritical marshals msg (already a fully-populated concrete
// contracts/gen/go/sandboxws.* struct, e.g. sandboxws.ExecutionComplete{...}
// -- this package does not know or care which specific type, only that
// it's one of the 6 real critical types per events.schema.json, see
// doc.go) and buffers+sends it under ackID, resent on every future
// reconnect until the matching "ack{ackId}" command arrives (dispatch.go).
// A critical (unacked) buffered entry is NEVER evicted -- see buffer.go's
// evictionDecision and doc.go for the full reasoning.
//
// The immediate send is best-effort: if it fails (e.g. no live connection
// right now), that is NOT an error from this method -- the entry is
// already buffered, so Run's own flushBuffer will deliver it on the next
// (re)connect regardless. While a fresh connection replays the buffer,
// the call waits for that replay to end, which writes the entry itself
// (enqueue, bridge.go).
//
// A critical event is never cut, so none may be larger than every control
// plane reads: one over platform.DefaultFrameReadLimitBytes is refused --
// nothing buffered, a warning naming it sent in its place -- and
// SendCritical returns ErrFrameTooLarge. Its free text, where it carries
// text from outside this process, is capped first (CapCriticalText), so a
// real one never is. Otherwise only a marshal failure (a genuine caller
// bug) is returned as an error.
func (b *Bridge) SendCritical(ctx context.Context, msg any, ackID string) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("wsbridge: marshal critical event %q: %w", ackID, err)
	}
	if len(payload) > platform.DefaultFrameReadLimitBytes {
		b.warnNotSent(ctx, payload, platform.DefaultFrameReadLimitBytes)
		return fmt.Errorf("%w: critical event %q is %d bytes, over the %d every control plane reads",
			ErrFrameTooLarge, ackID, len(payload), platform.DefaultFrameReadLimitBytes)
	}

	entry, conn, bound := b.enqueue(ctx, outboundEntry{ackID: ackID, critical: true, payload: payload})
	b.bestEffortSend(ctx, entry, conn, bound)
	return nil
}

// SendBestEffort marshals and sends msg, buffering it too (so it's resent
// on reconnect, relying on the receiver's own upsert-by-messageId
// idempotency per §6.1), but IS subject to eviction once the buffer is
// at cap and it is the oldest non-critical entry left (doc.go, "The replay
// after a (re)connect", lists exactly when that can happen). Like
// SendCritical, it waits while a fresh connection replays the buffer.
//
// An event over platform.MaxEventFrameBytes is cut to it before it is
// buffered (Fit), the cut's path recorded on the entry, so no buffered
// entry is larger -- the bound on the buffer's memory (doc.go, "What a
// connection writes"). One that cannot be cut to fit is not buffered: a
// warning naming it is sent in its place, and SendBestEffort returns
// ErrFrameTooLarge.
func (b *Bridge) SendBestEffort(ctx context.Context, msg any) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("wsbridge: marshal best-effort event: %w", err)
	}

	entry := outboundEntry{critical: false, payload: payload}
	if len(payload) > platform.MaxEventFrameBytes {
		fitted, path, ok := Fit(payload, platform.MaxEventFrameBytes, nil)
		if !ok {
			b.warnNotSent(ctx, payload, platform.MaxEventFrameBytes)
			return fmt.Errorf("%w: best-effort event is %d bytes, over the %d buffered, and cannot be cut to fit",
				ErrFrameTooLarge, len(payload), platform.MaxEventFrameBytes)
		}
		entry.payload, entry.cutPath = fitted, path
	}

	entry, conn, bound := b.enqueue(ctx, entry)
	b.bestEffortSend(ctx, entry, conn, bound)
	return nil
}

// warnNotSent sends a best-effort warning that payload was refused, never
// buffered: it is over bound and cannot be cut to fit.
func (b *Bridge) warnNotSent(ctx context.Context, payload []byte, bound int) {
	slog.Warn("wsbridge: event refused, not buffered: over the bound it must fit, and no cut fits it",
		"bytes", len(payload), "bound_bytes", bound)
	warning, err := b.warningPayload(frameNotWrittenMessage(payload, bound))
	if err != nil {
		return
	}
	entry, conn, connBound := b.enqueue(ctx, outboundEntry{payload: warning, notice: true})
	b.bestEffortSend(ctx, entry, conn, connBound)
}

// SendBootProgress translates one internal/sandboxagent/services.
// BootProgressEvent into a wire boot_progress event and sends it
// (best-effort, not critical -- boot_progress is not one of the 6 critical
// types). events.schema.json's own "phase" field is a free-form string
// with no enum -- this Step's own invented, documented convention for
// reporting §14.2's PER-SERVICE phase over a wire event that only
// carries ONE session-wide phase string is "<serviceName>:<phase>" (e.g.
// "web:starting", "mock-api:ready"). Also updates the internally-tracked
// lastBootPhase the next heartbeat carries.
//
// event.ServiceName is url.QueryEscape'd before joining: servicemanifest's
// own Service.Name validation (§14.2) only requires non-empty/unique, no
// charset restriction, so a service literally named e.g. "web:ready" would
// otherwise produce a phase string indistinguishable from service "web" in
// phase "ready" -- percent-encoding the name specifically closes that
// ambiguity (a normal name with no special characters round-trips
// unchanged) without needing to touch §14.2's own validation rules.
func (b *Bridge) SendBootProgress(ctx context.Context, event services.BootProgressEvent) error {
	phase := url.QueryEscape(event.ServiceName) + ":" + string(event.Phase)
	b.setLastBootPhase(&phase)

	msg := sandboxws.BootProgress{
		Type:      "boot_progress",
		MessageId: b.newMessageID(),
		SessionId: b.sessionID,
		Gen:       b.sessionGen,
		Phase:     phase,
		Timestamp: time.Now(),
	}
	return b.SendBestEffort(ctx, msg)
}

// bestEffortSend attempts to write entry on conn, the connection enqueue
// returned when it buffered entry, fitted to bound, the largest message
// that connection's peer reads (Fit), silently doing nothing if there was
// no connection or the write fails -- the caller (SendCritical/
// SendBestEffort) has already buffered entry, so eventual delivery is
// guaranteed via the next (re)connect's flushBuffer regardless of whether
// THIS immediate attempt succeeds. conn is nil when the entry was buffered
// while a fresh connection was replaying the buffer: that replay wrote
// entry itself, after every older one (see enqueue).
//
// An entry that cannot be fitted is not written on conn, and stays
// buffered for a later connection that reads more; a warning naming it
// goes out once (warnOnce), written here on conn.
func (b *Bridge) bestEffortSend(ctx context.Context, entry outboundEntry, conn *websocket.Conn, bound int) {
	if conn == nil {
		return
	}
	payload, _, ok := Fit(entry.payload, bound, entry.cutPath)
	if !ok {
		warning, warned := b.warnOnce(entry, bound)
		if !warned {
			return
		}
		if payload, _, ok := Fit(warning.payload, bound, nil); ok {
			_ = conn.Write(ctx, websocket.MessageText, payload)
		}
		return
	}
	_ = conn.Write(ctx, websocket.MessageText, payload)
}
