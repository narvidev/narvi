package wsbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/platform"
)

// Run connects (retrying with exponential backoff, bounded
// [reconnectMinBackoff, reconnectMaxBackoff], on any non-fatal failure),
// sends "ready" as the first event on every fresh connection (including
// after a reconnect -- it is genuinely a new connection each time) --
// followed, on a connection that comes up once the boot has completed, by
// a heartbeat carrying the boot's start phase (ReportBootStarted) -- starts
// the heartbeat loop, runs the inbound read loop (dispatching commands,
// handling "ack" internally -- never exposed to CommandHandler), and
// resends every still-unacked/un-evicted buffered event after each
// (re)connect, in original order, BEFORE resuming normal send traffic.
//
// Returns nil when ctx is canceled (an OS signal, from main.go's own
// signal.NotifyContext) -- deliberately nil, not ctx.Err(), so this reads
// as an ordinary, successful shutdown trigger exactly like the pre-existing
// `<-ctx.Done()` it replaces. Returns ErrShutdownRequested when a
// (non-stale-gen) "shutdown" command is received. Returns
// *FatalConnectError when the handshake itself returns 401/403/404/410 --
// no retry is attempted in that case.
//
// All concurrency (the heartbeat loop, the read loop) goes through an
// errgroup.Group -- no naked "go" statement anywhere in this package.
func (b *Bridge) Run(ctx context.Context) error {
	if b.journal != nil {
		defer b.journal.close()
	}
	backoff := b.reconnectMinBackoff

	for {
		if ctx.Err() != nil {
			return nil
		}

		conn, resp, err := b.dial(ctx)
		if err != nil {
			if resp != nil && isFatalStatus(resp.StatusCode) {
				return &FatalConnectError{Status: resp.StatusCode}
			}

			slog.Warn("wsbridge: connect failed, retrying with backoff", "error", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = nextBackoff(backoff, b.reconnectMaxBackoff)
			continue
		}

		backoff = b.reconnectMinBackoff

		runErr := b.runConnection(ctx, conn)
		if errors.Is(runErr, ErrShutdownRequested) {
			return ErrShutdownRequested
		}
		if ctx.Err() != nil {
			return nil
		}
		if runErr != nil {
			slog.Warn("wsbridge: connection lost, reconnecting", "error", runErr)
		}
		// A lost ESTABLISHED connection is retried immediately (no
		// backoff) -- backoff only governs repeated FAILED connect
		// attempts (the branch above), a deliberately different phase.
	}
}

// dial performs one WS handshake attempt, setting the 3 headers §6.1
// names: Authorization (bearer sandbox token), X-Sandbox-ID (this Step's
// own honest-gap value, see doc.go), and X-Sandbox-Gen (the session's own
// spawn generation, for the connection-level half of gen-fencing --
// commands.schema.json's own per-message half is enforced separately, see
// dispatch.go).
func (b *Bridge) dial(ctx context.Context) (*websocket.Conn, *http.Response, error) {
	dialCtx, cancel := context.WithTimeout(ctx, b.dialTimeout)
	defer cancel()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+b.sandboxToken)
	header.Set("X-Sandbox-ID", b.sandboxID)
	header.Set("X-Sandbox-Gen", strconv.Itoa(b.sessionGen))

	return websocket.Dial(dialCtx, b.dialURL, &websocket.DialOptions{HTTPHeader: header})
}

// isFatalStatus reports whether status is one of §6.1's 4 fatal handshake
// statuses ("Agent treats 401/403/404/410 as fatal (no retry)").
func isFatalStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return true
	default:
		return false
	}
}

// nextBackoff doubles current, capped at max -- a plain deterministic
// helper, not given its own dedicated test file since Run's own
// backoff-bounded-retry test already exercises it end to end.
func nextBackoff(current, maxBackoff time.Duration) time.Duration {
	next := current * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}

// runConnection drives exactly one live connection: send "ready" -- then,
// once the boot has completed, the boot's start phase
// (sendBootStartAfterReady) -- flush the outbound buffer, then run the
// heartbeat loop and inbound read loop concurrently via a single
// errgroup.WithContext(ctx) -- deliberately
// cancel-on-first-error semantics (unlike internal/sandboxagent/
// supervisor's own zero-value groups): the heartbeat loop and read loop
// share ONE underlying connection, so either one failing (a write error,
// a read error, a received shutdown command) means the whole connection is
// done and the other loop must stop too. Returns the first error either
// loop returned; the sentinel ErrShutdownRequested specifically means the
// read loop saw a valid shutdown command, not a disconnect.
//
// conn is published for live sends (b.conn) only by flushBuffer, once the
// replay has caught up -- never before it starts, or a live send would be
// written ahead of older entries still waiting to be replayed. Commands
// are read only after the replay, which the hold in enqueue keeps to one
// pass over what was buffered plus at most one entry per sender.
func (b *Bridge) runConnection(ctx context.Context, conn *websocket.Conn) error {
	defer b.setConn(nil)
	defer func() { _ = conn.CloseNow() }()

	// The largest message this connection reads is the largest prompt
	// frame the control plane may send (technical plan §6.1). The
	// library's default, 32 KiB, closed the connection on any longer prompt
	// -- a review's, with its pull request's diff inlined -- which was then
	// lost. sendReady states this limit to the control plane.
	conn.SetReadLimit(platform.MaxPromptFrameBytes)

	if err := b.sendReady(ctx, conn); err != nil {
		return fmt.Errorf("wsbridge: send ready: %w", err)
	}
	var heartbeats connHeartbeats
	if err := b.sendBootStartAfterReady(ctx, conn, &heartbeats); err != nil {
		return fmt.Errorf("wsbridge: send boot-start heartbeat: %w", err)
	}
	if err := b.flushBuffer(ctx, conn); err != nil {
		return fmt.Errorf("wsbridge: flush buffered events: %w", err)
	}

	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return b.heartbeatLoop(groupCtx, conn, &heartbeats)
	})
	group.Go(func() error {
		return b.readLoop(groupCtx, conn)
	})

	return group.Wait()
}

// sendReady sends "ready" directly on conn -- deliberately NOT buffered:
// it is genuinely fresh on every connection -- events.schema.json's own
// Ready doc comment: "First event on a fresh WS connection, once the
// agent is ready to receive commands" -- never something to replay
// verbatim from a PRIOR connection's buffer. Every ready is therefore one
// reconnect, which is what the control plane answers a lost prompt on
// (technical plan §3.3's prompt receipts).
//
// Every ready states capabilities.maxFrameBytes, the read limit
// runConnection set on conn: the control plane holds every prompt frame to
// this gen to it (technical plan §3.3, §6.1). It advertises
// capabilities.promptReceipt when, and only when, the prompt journal is
// open (EnablePromptReceipts) and no append to it has failed; otherwise
// that key is absent, as from an agent that predates it.
func (b *Bridge) sendReady(ctx context.Context, conn *websocket.Conn) error {
	maxFrameBytes := platform.MaxPromptFrameBytes
	msg := sandboxws.Ready{
		Type:         "ready",
		MessageId:    b.newMessageID(),
		SessionId:    b.sessionID,
		Gen:          b.sessionGen,
		Timestamp:    time.Now(),
		AgentVersion: b.agentVersion,
		ImageDigest:  b.imageDigest,
		Capabilities: &sandboxws.ReadyCapabilities{MaxFrameBytes: &maxFrameBytes},
	}
	if b.promptReceiptsOn() {
		promptReceipt := true
		msg.Capabilities.PromptReceipt = &promptReceipt
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, payload)
}

// flushBuffer replays every still-buffered entry, in original order, BEFORE
// any other traffic resumes on this connection, then publishes conn for
// live sends.
//
// Live sends are held for the whole replay (enqueue, bridge.go): a send
// made meanwhile buffers its entry and waits until the replay ends, and
// this loop writes that entry after everything older. The last check --
// nothing left to write -- happens in the same connMu critical section
// that publishes conn and releases the held sends, so no entry can be
// buffered behind a replay that has already made its last check, and a
// released send never finds conn unpublished. Publishing before the
// replay (as this package once did) let a live `token` frame reach the
// control plane ahead of an older, never-delivered buffered frame of the
// same text part; replayed after it, the older frame became the part's
// newest stored row, which is the text every reader shows.
//
// Because a held sender adds nothing more until the replay ends, the loop
// runs one pass over what was buffered when the connection came up, then
// at most one more over the entries its senders added meanwhile -- however
// fast they are -- so commands, read only once it returns, wait for that
// and no more. A failed write ends the replay too, releasing the held
// sends; their entries stay buffered for the next connection.
func (b *Bridge) flushBuffer(ctx context.Context, conn *websocket.Conn) (err error) {
	b.connMu.Lock()
	b.replayDone = make(chan struct{})
	b.connMu.Unlock()
	defer func() {
		if err != nil {
			b.connMu.Lock()
			b.endReplay()
			b.connMu.Unlock()
		}
	}()

	var nextSeq uint64
	for {
		b.connMu.Lock()
		pending := b.buffer.snapshotFrom(nextSeq)
		if len(pending) == 0 {
			if b.replayCaughtUpHook != nil {
				b.replayCaughtUpHook()
			}
			b.conn = conn
			b.endReplay()
			b.connMu.Unlock()
			return nil
		}
		b.connMu.Unlock()

		for _, entry := range pending {
			if err := conn.Write(ctx, websocket.MessageText, entry.payload); err != nil {
				return err
			}
			nextSeq = entry.seq + 1
			if b.flushWriteHook != nil {
				b.flushWriteHook()
			}
		}
	}
}

// connHeartbeats is one connection's share of the boot-start phase every
// connection carries ahead of its first null heartbeat (ReportBootStarted,
// bridge.go): whether any heartbeat has been written on that connection
// yet. A connection's first heartbeat either carries a non-null phase --
// boot evidence already, ahead of any null the connection writes later --
// or reports null and goes out behind the start phase; so only a
// connection's first heartbeat is ever preceded by it, and it never
// follows a null. Held by one connection: runConnection sets it before the
// heartbeat loop starts, and only that loop touches it after.
type connHeartbeats struct {
	sent bool
}

// sendBootStartAfterReady writes, on a connection that comes up once the
// boot has completed, a heartbeat carrying the boot's start phase
// (ReportBootStarted) right after "ready" -- ahead of the buffer replay,
// so the control plane has this connection's boot evidence before
// anything else it carries, a replayed boot_timing included. While the
// boot still runs it writes nothing: the heartbeat loop's heartbeats carry
// the boot's phase themselves.
func (b *Bridge) sendBootStartAfterReady(ctx context.Context, conn *websocket.Conn, heartbeats *connHeartbeats) error {
	phase, start := b.heartbeatBootPhases()
	if phase != nil || start == nil {
		return nil
	}
	if err := b.writeHeartbeat(ctx, conn, start); err != nil {
		return err
	}
	heartbeats.sent = true
	return nil
}

// sendHeartbeatNow builds and sends one heartbeat frame directly on conn
// -- pulled out of heartbeatLoop's own `case <-ticker.C:` arm (§3.3, "turn
// recovery") so the new `case <-b.forceHeartbeat:` arm below can send the
// SAME shape out-of-band, without duplicating the build-and-send logic.
//
// When it is the connection's first heartbeat and would report a null
// boot phase once ReportBootStarted has recorded a start phase, a
// heartbeat carrying the start phase goes first, so the control plane
// always sees this boot's evidence on a connection before that
// connection's "boot has completed" (technical plan §3.2).
func (b *Bridge) sendHeartbeatNow(ctx context.Context, conn *websocket.Conn, heartbeats *connHeartbeats) error {
	phase, start := b.heartbeatBootPhases()
	if phase == nil && start != nil && !heartbeats.sent {
		if err := b.writeHeartbeat(ctx, conn, start); err != nil {
			return err
		}
	}
	if err := b.writeHeartbeat(ctx, conn, phase); err != nil {
		return err
	}
	heartbeats.sent = true
	return nil
}

// writeHeartbeat writes one heartbeat frame reporting lastBootPhase.
func (b *Bridge) writeHeartbeat(ctx context.Context, conn *websocket.Conn, lastBootPhase *string) error {
	msg := sandboxws.Heartbeat{
		Type:      "heartbeat",
		MessageId: b.newMessageID(),
		SessionId: b.sessionID,
		Gen:       b.sessionGen,
		// ConversationId reflects whatever SetConversationID last
		// recorded -- nil until the first turn's own StartTurn call
		// (internal/adapters/outbound/opencode.Adapter, §7)
		// resolves a real OpenCode conversation id.
		ConversationId: b.getConversationID(),
		LastBootPhase:  lastBootPhase,
		Timestamp:      time.Now(),
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("wsbridge: marshal heartbeat: %w", err)
	}
	return conn.Write(ctx, websocket.MessageText, payload)
}

// heartbeatLoop sends a heartbeat every b.heartbeatInterval (§6.1: 30s)
// directly on conn -- deliberately NOT routed through the outbound buffer:
// a heartbeat is a point-in-time liveness signal, and a stale one replayed
// after a reconnect would carry a stale timestamp with no informational
// value a FRESH heartbeat (already due within one more interval) doesn't
// already supersede. §3.3 ("turn recovery") adds a SECOND trigger
// alongside the regular ticker: b.forceHeartbeat, which SetConversationID
// sends on (non-blocking) the first time it observes a genuinely new,
// non-nil conversation id (§3.3: "at turn start... never lazily"), and
// ReportBootStarted/MarkBootComplete send on as boot starts and completes
// (§3.2) -- both
// arms call the SAME sendHeartbeatNow helper, so the wire shape is
// identical regardless of which one fired. The regular ticker is NOT
// reset when the forceHeartbeat arm fires -- its own cadence keeps running
// independently; an extra, slightly-early heartbeat is harmless (§6.1's
// heartbeat carries no ack/dedup concern at all, unlike the 6 critical
// event types the outbound buffer exists for).
func (b *Bridge) heartbeatLoop(ctx context.Context, conn *websocket.Conn, heartbeats *connHeartbeats) error {
	ticker := time.NewTicker(b.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := b.sendHeartbeatNow(ctx, conn, heartbeats); err != nil {
				return err
			}
		case <-b.forceHeartbeat:
			if err := b.sendHeartbeatNow(ctx, conn, heartbeats); err != nil {
				return err
			}
		}
	}
}
