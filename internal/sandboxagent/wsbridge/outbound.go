package wsbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
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
// (re)connect regardless. Only a msg marshal failure (a genuine caller
// bug) is returned as an error.
func (b *Bridge) SendCritical(ctx context.Context, msg any, ackID string) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("wsbridge: marshal critical event %q: %w", ackID, err)
	}

	b.bestEffortSend(ctx, b.enqueue(outboundEntry{ackID: ackID, critical: true, payload: payload}), payload)
	return nil
}

// SendBestEffort marshals and sends msg, buffering it too (so it's resent
// on reconnect, relying on the receiver's own upsert-by-messageId
// idempotency per §6.1), but IS subject to eviction once the buffer is
// over cap and no non-critical entry is left to prefer evicting instead.
func (b *Bridge) SendBestEffort(ctx context.Context, msg any) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("wsbridge: marshal best-effort event: %w", err)
	}

	b.bestEffortSend(ctx, b.enqueue(outboundEntry{critical: false, payload: payload}), payload)
	return nil
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

// bestEffortSend attempts to write payload on conn, the connection
// enqueue returned when it buffered payload, silently doing nothing if
// there was none or the write fails -- the caller (SendCritical/
// SendBestEffort) has already buffered payload, so eventual delivery is
// guaranteed via the next (re)connect's flushBuffer regardless of whether
// THIS immediate attempt succeeds. conn is nil while a fresh connection is
// still replaying the buffer: the replay then writes payload itself, after
// every older entry (see enqueue).
func (b *Bridge) bestEffortSend(ctx context.Context, conn *websocket.Conn, payload []byte) {
	if conn == nil {
		return
	}
	_ = conn.Write(ctx, websocket.MessageText, payload)
}
