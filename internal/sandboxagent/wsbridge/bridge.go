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
	"github.com/narvidev/narvi/internal/sandboxagent/services"
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
	// disconnected, and also while a fresh connection is still replaying
	// the buffer), and replayDone -- read by SendCritical/SendBestEffort,
	// which may be called concurrently with Run's own reconnect loop
	// swapping them. It also makes "buffer an entry and read conn and
	// replayDone" (enqueue) atomic with flushBuffer's "nothing left to
	// replay, publish conn, end the replay" step, which is what keeps a
	// live send from overtaking an older buffered entry after a reconnect,
	// and an entry held behind the replay from being skipped by it: see
	// flushBuffer (run.go).
	connMu sync.Mutex
	conn   *websocket.Conn
	// replayDone is non-nil while a fresh connection replays the buffer,
	// and closed when that replay ends, whether it caught up or failed. A
	// send made meanwhile buffers its entry, then waits on it (enqueue).
	replayDone chan struct{}

	// flushWriteHook, when non-nil, runs after flushBuffer writes each
	// entry; enqueueHeldHook runs when a send has buffered its entry and is
	// about to wait for a running replay; replayCaughtUpHook runs inside
	// flushBuffer's final critical section, connMu held, right after its
	// last snapshot came back empty and before it publishes conn. All
	// three are always nil in production; set only through export_test.go
	// so a test can place a send at a deterministic point of a replay.
	flushWriteHook     func()
	enqueueHeldHook    func()
	replayCaughtUpHook func()

	// bootMu guards lastBootPhase, read by the heartbeat loop and written
	// by SendBootProgress/MarkBootComplete. lastBootPhase is never nil
	// before MarkBootComplete: New starts it at InitialBootPhase, because a
	// heartbeat's null lastBootPhase is the wire's "boot has completed"
	// (§6.1) and the control plane moves a Booting sandbox to Ready on it.
	//
	// It also guards bootStartPhase: the phase ReportBootStarted found,
	// which every connection carries ahead of its first null heartbeat --
	// see ReportBootStarted.
	bootMu         sync.Mutex
	lastBootPhase  *string
	bootStartPhase *string

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
	// ReportBootStarted and MarkBootComplete send on it too, for the same
	// reason: the control plane decides Booting -> Ready from the boot
	// phase heartbeats carry (§3.2).
	forceHeartbeat chan struct{}
}

// InitialBootPhase is what every heartbeat reports as lastBootPhase from
// the moment a Bridge exists until SendBootProgress first names a phase:
// boot has not completed, and nothing has reported a phase yet. The Bridge
// dials before the boot sequence starts (cmd/sandbox-agent runs bridge.Run
// alongside runBootSequence, so boot_progress reaches the control plane
// during boot), and the clone, the git-dir sync and the repo hooks report
// no phase at all -- only services and dockerd do -- so without this
// value every heartbeat of that window carried null, which §6.1 and the
// control plane read as "boot has completed" (technical plan §3.2).
//
// It is services.PhaseStarting on its own, with no "<service>:" prefix:
// every phase SendBootProgress reports carries one, so this value can
// never be mistaken for a service's phase. It rides only on heartbeats,
// never on a boot_progress event, so the boot phases a session shows are
// unchanged.
const InitialBootPhase = string(services.PhaseStarting)

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
	initialPhase := InitialBootPhase
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

		lastBootPhase: &initialPhase,

		forceHeartbeat: make(chan struct{}, 1),
	}
}

func (b *Bridge) setConn(c *websocket.Conn) {
	b.connMu.Lock()
	defer b.connMu.Unlock()
	b.conn = c
}

// enqueue buffers entry and returns the connection to write it on right
// now, or nil when there is none.
//
// While a fresh connection is still replaying the buffer, the replay
// itself writes the entry, after every older one, and enqueue HOLDS the
// caller until that replay has ended (or ctx is done), then returns nil:
// there is nothing left for the caller to write. Holding is what bounds a
// replay -- each sending goroutine adds at most one entry per replay, so a
// sender faster than the connection cannot keep it replaying, and delay
// the reading of commands, indefinitely -- and what keeps the replay from
// evicting entries it has not written yet (doc.go, "The replay after a
// (re)connect").
//
// Buffering, reading conn and reading replayDone happen in one connMu
// critical section, and flushBuffer checks that nothing is left and ends
// the replay in one too, so an entry is always either written by the
// replay or written live after it -- never live in the middle of it, and
// never buffered behind a replay that has already made its last check.
func (b *Bridge) enqueue(ctx context.Context, entry outboundEntry) *websocket.Conn {
	b.connMu.Lock()
	b.buffer.add(entry)
	conn, replay := b.conn, b.replayDone
	b.connMu.Unlock()
	if replay == nil {
		return conn
	}
	if b.enqueueHeldHook != nil {
		b.enqueueHeldHook()
	}
	select {
	case <-replay:
	case <-ctx.Done():
	}
	return nil
}

// endReplay closes and clears replayDone, releasing every send held behind
// the replay that just ended. connMu must be held.
func (b *Bridge) endReplay() {
	if b.replayDone != nil {
		close(b.replayDone)
		b.replayDone = nil
	}
}

func (b *Bridge) setLastBootPhase(phase *string) {
	b.bootMu.Lock()
	defer b.bootMu.Unlock()
	b.lastBootPhase = phase
}

// ReportBootStarted sends a heartbeat right away, through forceHeartbeat,
// reporting the boot phase -- InitialBootPhase until a service reports
// one. Call it as the boot sequence starts. That non-null heartbeat is
// boot evidence (technical plan §3.2): the control plane reads a later
// null phase as boot completion only once the sandbox's generation has
// shown some. Without it, a boot that completes within the first
// heartbeatInterval and starts no service would show none but
// boot_timing, a best-effort telemetry event whose loss must never fail a
// boot (§33.3). Called while disconnected, the heartbeat is sent as soon
// as the next connection's heartbeat loop starts.
//
// The phase it finds is the boot's start phase, and every connection
// carries it ahead of its first null heartbeat -- every connection, not
// only the first. Nothing on the wire acknowledges a heartbeat, and a
// successful write only means the frame left this process: a connection
// that drops can lose frames it wrote, the start phase included. What the
// agent can rely on is order within one connection: the control plane
// reads a connection's frames in the order they were written, so a null
// that reaches it has been preceded by whatever that same connection
// wrote before it. So a connection that comes up once the boot has
// completed writes the start phase right after "ready", ahead of the
// buffer replay; one whose first heartbeat would report null writes the
// start phase just before it (sendHeartbeatNow, run.go); and one whose
// first heartbeat carries a non-null phase has shown the evidence already.
// Within one connection the start phase is therefore written at most once,
// and never after a null.
//
// The evidence thus never depends on when heartbeats go out relative to
// the boot, or on which connection's frames get through: a boot that
// completes before the first connection is up -- the dial backing off
// while the control plane is briefly unreachable -- still reaches the
// control plane as the start phase, then null, rather than as the single
// null heartbeat forceHeartbeat's one pending signal would otherwise
// coalesce the two calls into; and a connection lost with its start phase
// in flight is followed by one that carries it again. The control plane
// takes the repeat in its stride: it records evidence once per generation
// and moves a sandbox on no heartbeat whose phase is non-null.
func (b *Bridge) ReportBootStarted() {
	b.bootMu.Lock()
	if b.lastBootPhase != nil {
		phase := *b.lastBootPhase
		b.bootStartPhase = &phase
	}
	b.bootMu.Unlock()
	b.signalHeartbeat()
}

// heartbeatBootPhases returns, read together, the phase the next heartbeat
// reports and the boot's start phase (nil until ReportBootStarted has
// recorded one).
func (b *Bridge) heartbeatBootPhases() (phase, start *string) {
	b.bootMu.Lock()
	defer b.bootMu.Unlock()
	return b.lastBootPhase, b.bootStartPhase
}

// MarkBootComplete clears the tracked lastBootPhase to nil --
// events.schema.json's own Heartbeat.LastBootPhase doc comment: "Null once
// boot has completed (no more boot phases to report)." -- and sends a
// heartbeat right away, through forceHeartbeat, rather than leaving the
// control plane to learn of it up to one heartbeatInterval later: that
// null heartbeat is what moves the sandbox Booting -> Ready (technical
// plan §3.2). Called while disconnected, the heartbeat is sent as soon as
// the next connection's heartbeat loop starts. Call it once the whole boot
// sequence has succeeded, and never before: it is the only place
// lastBootPhase becomes nil.
func (b *Bridge) MarkBootComplete() {
	b.setLastBootPhase(nil)
	b.signalHeartbeat()
}

// signalHeartbeat asks heartbeatLoop for an immediate heartbeat, without
// blocking: forceHeartbeat's capacity-1 buffer coalesces signals raised
// before the loop gets to them, and the one heartbeat it then sends reads
// the state current at that moment -- preceded by the boot's start phase
// when it is its connection's first heartbeat and would report null
// (ReportBootStarted), which is what keeps the start and completion
// signals from coalescing into a lone null.
func (b *Bridge) signalHeartbeat() {
	select {
	case b.forceHeartbeat <- struct{}{}:
	default:
	}
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
		b.signalHeartbeat()
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
