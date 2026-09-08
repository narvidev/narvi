package wshub

import (
	"context"
	"encoding/json"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// envelope peeks just the fields this package needs to construct a
// sessionactor.SandboxEvent command, common across every sandbox-ws
// event/command shape (contracts/gen/go/sandboxws has no generated
// discriminated-union wrapper -- go-jsonschema does not synthesize one, see
// internal/sandboxagent/wsbridge/doc.go's own identical reasoning for its
// client-side mirror of this same pattern). Go's json.Unmarshal silently
// ignores JSON keys absent from a struct and leaves fields absent from the
// JSON at their zero value, so this one shared struct safely peeks every
// field needed across every event type: AckId is "" for the ~15
// non-critical event types, LastBootPhase is nil/absent for every type
// except heartbeat -- both exactly as intended.
type envelope struct {
	Type           string  `json:"type"`
	MessageID      string  `json:"messageId"`
	Gen            int     `json:"gen"`
	AckID          string  `json:"ackId"`
	LastBootPhase  *string `json:"lastBootPhase"`
	ConversationID *string `json:"conversationId"`
	// AgentVersion/ImageDigest (§12.2 item 1's own runtime-fingerprint
	// gap) are non-empty only on a "ready" event (sandbox-ws's own Ready
	// def, the only event type that carries them) -- absent from JSON on
	// every other event type, decoding to nil here exactly like
	// LastBootPhase/ConversationID already do for their own single
	// carrying type.
	AgentVersion *string `json:"agentVersion"`
	ImageDigest  *string `json:"imageDigest"`
}

// readLoop reads and dispatches inbound sandbox-WS frames on conn until
// conn.Read errors (disconnect) or ctx is done, delivering each recognized
// frame to actor as a sessionactor.SandboxEvent and writing back an ack
// when the actor's own reply says one is due (§6.1 ack protocol). Mirrors
// internal/sandboxagent/wsbridge/dispatch.go's own "one malformed frame
// must not kill the whole connection" precedent.
//
// This function's own job is strictly inbound: reading frames off conn and
// acking them. Actually SENDING prompt/stop/push/snapshot/shutdown/
// git_sync_complete to a live sandbox connection from the control-plane
// side is commander.go's own SandboxRegistry.SendCommand (§9.3, "e2e
// happy path") -- registered against this SAME conn by NewSandboxHandler
// right before entering this loop (sandbox.go), not built in this
// function.
func readLoop(ctx context.Context, conn *websocket.Conn, actor *sessionactor.Actor, sessionIDStr string, gen int, timeouts platform.Timeouts) {
	logger := platform.Logger(ctx)

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			logger.Warn("wshub: dropping malformed inbound sandbox event", "error", err)
			continue
		}

		// Buffered (size 1) so handleSandboxEvent's own non-blocking send
		// into it (sessionactor/sandboxevent.go) always succeeds
		// immediately, even if this loop already gave up waiting below.
		reply := make(chan sessionactor.SandboxEventOutcome, 1)
		cmd := sessionactor.SandboxEvent{
			Type:           env.Type,
			Gen:            env.Gen,
			MessageID:      env.MessageID,
			Raw:            json.RawMessage(data),
			LastBootPhase:  env.LastBootPhase,
			ConversationID: env.ConversationID,
			AgentVersion:   env.AgentVersion,
			ImageDigest:    env.ImageDigest,
			Reply:          reply,
		}

		if err := actor.Send(ctx, cmd); err != nil {
			// The actor being gone (ErrActorStopped) or ctx canceled means
			// no further useful work can happen here in THIS process --
			// close the connection; the sandbox-agent's own reconnect
			// (§6.1) will trigger a fresh GetOrSpawn.
			logger.Warn("wshub: actor.Send failed; closing connection", "error", err)
			return
		}

		select {
		case outcome := <-reply:
			if outcome.AckID == "" {
				continue
			}
			if err := writeAck(ctx, conn, sessionIDStr, gen, outcome.AckID); err != nil {
				logger.Warn("wshub: write ack failed; closing connection", "error", err)
				return
			}

		case <-ctx.Done():
			return

		case <-time.After(timeouts.SandboxEventAckTimeout):
			// One slow/lost ack must never close the connection: the
			// sandbox-agent's own ack-buffer-and-resend-on-reconnect
			// machinery (§6.1) already tolerates a missed ack by
			// construction -- losing one here is a documented, safe,
			// non-catastrophic outcome, not something that must be
			// prevented at all costs.
			logger.Warn("wshub: actor did not reply within ack timeout; continuing",
				"event_type", env.Type, "ack_id", env.AckID)
		}
	}
}

// writeAck marshals and writes a fresh sandboxws.Ack{AckId: ackID, ...} on
// conn -- messageId is freshly minted (github.com/google/uuid), matching
// internal/sandboxagent/wsbridge/bridge.go's own newMessageID precedent
// exactly; ackId is the ORIGINAL critical event's own deterministic ackId
// being acknowledged (commands.schema.json's own Ack def).
func writeAck(ctx context.Context, conn *websocket.Conn, sessionIDStr string, gen int, ackID string) error {
	ack := sandboxws.Ack{
		Type:      "ack",
		MessageId: uuid.NewString(),
		SessionId: sessionIDStr,
		Gen:       gen,
		AckId:     ackID,
	}
	payload, err := json.Marshal(ack)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, payload)
}
