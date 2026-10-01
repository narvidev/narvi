package wsbridge

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// envelope peeks just the "type" discriminator field common to every
// command shape in commands.schema.json's own `oneOf` -- see doc.go for
// why this package must dispatch by hand rather than unmarshal into a
// single generated union type (none exists).
type envelope struct {
	Type string `json:"type"`
}

// readLoop reads and dispatches inbound commands until conn.Read errors
// (disconnect, or ctx canceled) or a valid "shutdown" command is seen
// (returns ErrShutdownRequested, which propagates through the errgroup in
// runConnection to cancel the sibling heartbeat loop too).
func (b *Bridge) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}

		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			slog.Warn("wsbridge: dropping malformed inbound message", "error", err)
			continue
		}

		if err := b.dispatch(ctx, env.Type, data); err != nil {
			return err
		}
	}
}

// dispatch unmarshals data into the concrete command type env's own type
// value names, then routes it: "ack" is handled internally (never gen-
// checked -- it isn't a business command, and rejecting a stale-gen ack
// would only leak a buffered critical entry that will just get resent
// again on the next reconnect regardless); "shutdown" is gen-checked and,
// if current, reported back to Run via ErrShutdownRequested; every other
// recognized type is gen-checked and, if current, dispatched to
// b.handler; anything unrecognized is logged and skipped (forward-
// compatible with a future command type this package doesn't know about).
func (b *Bridge) dispatch(ctx context.Context, msgType string, data []byte) error {
	switch msgType {
	case "ack":
		var cmd sandboxws.Ack
		if err := json.Unmarshal(data, &cmd); err != nil {
			slog.Warn("wsbridge: dropping malformed ack", "error", err)
			return nil
		}
		b.buffer.ack(cmd.AckId)
		return nil

	case "shutdown":
		var cmd sandboxws.Shutdown
		if err := json.Unmarshal(data, &cmd); err != nil {
			slog.Warn("wsbridge: dropping malformed shutdown command", "error", err)
			return nil
		}
		if !b.checkGen("shutdown", cmd.Gen) {
			return nil
		}
		return ErrShutdownRequested

	case "prompt":
		var cmd sandboxws.Prompt
		if err := json.Unmarshal(data, &cmd); err != nil {
			slog.Warn("wsbridge: dropping malformed prompt command", "error", err)
			return nil
		}
		// Gen-checked before anything else: a receipt for a stale gen's
		// prompt would tell the control plane this gen holds it.
		if !b.checkGen("prompt", cmd.Gen) {
			return nil
		}
		// An error here ends the connection (dispatchPrompt's doc comment).
		return b.dispatchPrompt(ctx, cmd)

	case "stop":
		var cmd sandboxws.Stop
		if err := json.Unmarshal(data, &cmd); err != nil {
			slog.Warn("wsbridge: dropping malformed stop command", "error", err)
			return nil
		}
		if !b.checkGen("stop", cmd.Gen) {
			return nil
		}
		b.handler.HandleStop(ctx, cmd)
		return nil

	case "push":
		var cmd sandboxws.Push
		if err := json.Unmarshal(data, &cmd); err != nil {
			slog.Warn("wsbridge: dropping malformed push command", "error", err)
			return nil
		}
		if !b.checkGen("push", cmd.Gen) {
			return nil
		}
		b.handler.HandlePush(ctx, cmd)
		return nil

	case "snapshot":
		var cmd sandboxws.Snapshot
		if err := json.Unmarshal(data, &cmd); err != nil {
			slog.Warn("wsbridge: dropping malformed snapshot command", "error", err)
			return nil
		}
		if !b.checkGen("snapshot", cmd.Gen) {
			return nil
		}
		b.handler.HandleSnapshot(ctx, cmd)
		return nil

	case "git_sync_complete":
		var cmd sandboxws.GitSyncComplete
		if err := json.Unmarshal(data, &cmd); err != nil {
			slog.Warn("wsbridge: dropping malformed git_sync_complete command", "error", err)
			return nil
		}
		if !b.checkGen("git_sync_complete", cmd.Gen) {
			return nil
		}
		b.handler.HandleGitSyncComplete(ctx, cmd)
		return nil

	default:
		slog.Warn("wsbridge: unrecognized inbound command type, skipping", "type", msgType)
		return nil
	}
}

// checkGen implements commands.schema.json's own documented invariant:
// "per-message gen-fencing ... does not rely solely on the connection-level
// X-Sandbox-Gen header". Returns true when gen matches this Bridge's own
// session gen (dispatch should proceed); logs a warning and returns false
// on a mismatch (the command must be ignored, never dispatched).
func (b *Bridge) checkGen(cmdType string, gen int) bool {
	if gen != b.sessionGen {
		slog.Warn("wsbridge: ignoring stale-gen command", "type", cmdType, "gen", gen, "want", b.sessionGen)
		return false
	}
	return true
}
