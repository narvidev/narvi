package wsbridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
)

// CommandHandler is the pluggable dispatch target for the 5 business
// commands this package does NOT implement the actual behavior of --
// "ack" and "shutdown" are both handled internally by Run (see Run's own
// doc comment), never exposed here. Every method is handed the already
// gen-checked, concretely-typed command.
type CommandHandler interface {
	HandlePrompt(ctx context.Context, cmd sandboxws.Prompt)
	HandleStop(ctx context.Context, cmd sandboxws.Stop)
	HandlePush(ctx context.Context, cmd sandboxws.Push)
	HandleSnapshot(ctx context.Context, cmd sandboxws.Snapshot)
	HandleGitSyncComplete(ctx context.Context, cmd sandboxws.GitSyncComplete)
}

// FatalConnectError is returned by Run when the WS handshake itself
// returns 401/403/404/410 (§6.1: "Agent treats 401/403/404/410 as fatal (no
// retry)"). Any OTHER connect failure (network error, 5xx, timeout) is
// retried with exponential backoff instead of ever surfacing as an error
// from Run.
type FatalConnectError struct {
	Status int
}

func (e *FatalConnectError) Error() string {
	return fmt.Sprintf("wsbridge: fatal connect status %d (no retry, §6.1)", e.Status)
}

// ErrShutdownRequested is returned by Run when a "shutdown" command was
// received over the bridge -- distinct from ctx being canceled (an OS
// signal) so the caller (main.go) can tell the two apart if it ever needs
// to, even though both currently converge on the same graceful-shutdown
// sequence.
var ErrShutdownRequested = errors.New("wsbridge: shutdown requested by control plane")

// Bridge is one session's own sandbox-WS client: connection lifecycle
// (dial, fatal-status classification, exponential-backoff reconnect),
// the ack-protocol outbound buffer, heartbeat/boot_progress reporting, and
// inbound command dispatch. Build one with New; drive it with Run.
type Bridge struct {
	dialURL      string
	sandboxToken string
	sandboxID    string
	sessionID    string
	sessionGen   int
	handler      CommandHandler

	// agentVersion/imageDigest (§12.2 item 1's own runtime-fingerprint
	// gap) are this gen's own sandboxboot.BootFingerprint.AgentVersion/
	// ImageDigest, set once at New and reported on sendReady's own "ready"
	// event (the only event that carries them) -- immutable for this
	// Bridge's whole lifetime, unlike lastBootPhase/conversationID above,
	// so neither needs its own mutex.
	agentVersion string
	imageDigest  string

	dialTimeout         time.Duration
	heartbeatInterval   time.Duration
	reconnectMinBackoff time.Duration
	reconnectMaxBackoff time.Duration

	buffer *outboundBuffer

	// connMu guards conn, the CURRENT live connection (nil when
	// disconnected) -- read by SendCritical/SendBestEffort, which may be
	// called concurrently with Run's own reconnect loop swapping it out.
	connMu sync.RWMutex
	conn   *websocket.Conn

	// bootMu guards lastBootPhase, read by the heartbeat loop and written
	// by SendBootProgress/MarkBootComplete.
	bootMu        sync.Mutex
	lastBootPhase *string

	// convMu guards conversationID, read by the heartbeat loop and written
	// by SetConversationID -- the OpenCode-adapter analogue of bootMu/
	// lastBootPhase above.
	convMu         sync.Mutex
	conversationID *string

	// forceHeartbeat is §3.3's ("turn recovery") own out-of-band
	// signal: SetConversationID sends on it (non-blocking) the first time
	// it is called with a genuinely NEW, non-nil conversation id (§3.3:
	// "at turn start... never lazily") -- heartbeatLoop's own select
	// (run.go) also listens on this, sending an immediate heartbeat
	// rather than waiting for its own next b.heartbeatInterval tick.
	// Capacity 1, not 0: a burst of SetConversationID calls between two
	// regular ticks must wake the loop at most once (the SAME already-
	// current value is all a second signal would report anyway), never
	// block the calling goroutine (cmd/sandbox-agent's own
	// commandHandler.HandlePrompt, which must never be delayed by this).
	forceHeartbeat chan struct{}
}

// New builds a Bridge for one session, from its full SessionConfig (dial
// URL = sc.ControlPlaneWsUrl VERBATIM -- unlike §6.4's scm-credentials
// derivation, this field IS already the real WS connect target, no URL
// surgery needed) and a CommandHandler for the 5 business commands.
// sandboxID is this Step's own invented, HONEST-GAP value for the
// X-Sandbox-ID header -- see doc.go. agentVersion/imageDigest (§12.2 item
// 1's own runtime-fingerprint gap) are this gen's own already-resolved
// boot.Config.AgentVersion/ImageDigest (cmd/sandbox-agent/main.go's own
// caller already computed these for the boot-fingerprint log line, §5.3
// -- New reuses that same value rather than re-reading the env itself).
func New(
	sc sessionconfig.SessionConfig,
	sandboxID string,
	agentVersion, imageDigest string,
	handler CommandHandler,
	dialTimeout, heartbeatInterval, reconnectMinBackoff, reconnectMaxBackoff time.Duration,
) *Bridge {
	return &Bridge{
		dialURL:      sc.ControlPlaneWsUrl,
		sandboxToken: sc.SandboxToken,
		sandboxID:    sandboxID,
		sessionID:    sc.SessionId,
		sessionGen:   sc.Gen,
		agentVersion: agentVersion,
		imageDigest:  imageDigest,
		handler:      handler,

		dialTimeout:         dialTimeout,
		heartbeatInterval:   heartbeatInterval,
		reconnectMinBackoff: reconnectMinBackoff,
		reconnectMaxBackoff: reconnectMaxBackoff,

		buffer: newOutboundBuffer(),

		forceHeartbeat: make(chan struct{}, 1),
	}
}

func (b *Bridge) getConn() *websocket.Conn {
	b.connMu.RLock()
	defer b.connMu.RUnlock()
	return b.conn
}

func (b *Bridge) setConn(c *websocket.Conn) {
	b.connMu.Lock()
	defer b.connMu.Unlock()
	b.conn = c
}

func (b *Bridge) getLastBootPhase() *string {
	b.bootMu.Lock()
	defer b.bootMu.Unlock()
	return b.lastBootPhase
}

func (b *Bridge) setLastBootPhase(phase *string) {
	b.bootMu.Lock()
	defer b.bootMu.Unlock()
	b.lastBootPhase = phase
}

// MarkBootComplete clears the tracked lastBootPhase back to nil --
// events.schema.json's own Heartbeat.LastBootPhase doc comment: "Null once
// boot has completed (no more boot phases to report)." Call this once
// main.go's own boot.RunBoot call returns successfully.
func (b *Bridge) MarkBootComplete() {
	b.setLastBootPhase(nil)
}

// SetConversationID updates what the NEXT heartbeat reports as
// Heartbeat.ConversationId (§6.1: "heartbeat (30s, carries conversation id
// + last_boot_phase)"). §6.1 hardcoded ConversationId: nil with an
// explicit "no OpenCode adapter exists yet" comment (see run.go's
// heartbeatLoop) -- this Step's OpenCode adapter
// (internal/adapters/outbound/opencode) is that adapter, and
// cmd/sandbox-agent's own commandHandler calls this once StartTurn returns
// a real conversation id. Pass nil to clear it back to "no conversation
// yet" (there is no scenario that currently does this, but the method
// accepts it for the same reason SendBootProgress/heartbeat's own
// LastBootPhase is nilable).
//
// §3.3 ("turn recovery") extends this: when id is a genuinely NEW,
// non-nil value (different from whatever was recorded before -- a nil id,
// or a different string), this ALSO triggers an immediate, out-of-band
// heartbeat send via forceHeartbeat, rather than leaving the new
// conversation id to wait for the next regular b.heartbeatInterval tick
// (§3.3: "at turn start... never lazily" -- cmd/sandbox-agent's own
// commandHandler now calls this the INSTANT StartTurn resolves a real id,
// long before a turn's own, possibly-minutes-long execution completes, so
// this method must propagate that urgency onward to the wire, not just to
// this in-memory field). Called with the SAME id again (e.g. a later turn
// resuming the same conversation) does NOT re-trigger one -- only a real
// change is worth an early heartbeat. The non-blocking select-with-default
// is safe regardless of whether heartbeatLoop is currently between ticks
// or not: forceHeartbeat's own capacity-1 buffer already coalesces a burst
// of calls into a single wakeup (see that field's own doc comment).
func (b *Bridge) SetConversationID(id *string) {
	b.convMu.Lock()
	prev := b.conversationID
	b.conversationID = id
	b.convMu.Unlock()

	if id != nil && (prev == nil || *prev != *id) {
		select {
		case b.forceHeartbeat <- struct{}{}:
		default:
		}
	}
}

func (b *Bridge) getConversationID() *string {
	b.convMu.Lock()
	defer b.convMu.Unlock()
	return b.conversationID
}

// newMessageID mints a fresh messageId for a Bridge-originated event
// (ready/heartbeat/boot_progress) -- SendCritical/SendBestEffort's own
// caller-supplied msg already carries its own messageId, so this is never
// used for those.
func (b *Bridge) newMessageID() string {
	return uuid.NewString()
}
