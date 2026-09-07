package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	"github.com/narvidev/narvi/internal/platform"
)

// mailboxBufferSize is the mailbox channel's queue depth -- a plain
// count, not a duration/interval, so it does not belong in
// platform.Timeouts (notimeliteral only forbids time.Duration unit
// literals; this is a plain int). Chosen generously: an actor processes
// one command at a time (that serialization is what makes §2's
// single-writer rule true), so a deep buffer just means a burst of
// timer/command deliveries queues up instead of blocking whoever is
// sending them.
const mailboxBufferSize = 32

// Actor is the single in-process owner of one session's state (§2: "All
// mutations of a session's state go through its actor -- no other code
// path writes session/sandbox/turn rows"). Built exclusively by
// Registry.hydrateAndAcquire (hydrate.go), which is the only code that
// may construct one -- every field is unexported.
type Actor struct {
	sessionID pgtype.UUID
	epoch     int64

	pool     *pgxpool.Pool
	timeouts platform.Timeouts
	stores   storeBundle

	// broadcaster is how a successfully committed event reaches every
	// currently-subscribed browser client for this session, live (§6.2's
	// "→ broadcast stream" made real -- design decision: broadcasting is a
	// generic, automatic property of transact's own commit path, not
	// something each command handler must remember to call; see
	// pendingBroadcast below and transact's own doc comment). May be nil
	// (some tests construct an Actor without one, e.g. via a fake
	// storeBundle-only setup) -- appendEvent/appendRawEvent still append to
	// pendingBroadcast unconditionally, but transact skips the delivery
	// loop entirely when broadcaster is nil.
	broadcaster ports.EventBroadcaster

	// commander is how a successfully-dispatched turn's prompt actually
	// reaches a live sandbox connection (§9.3, "e2e happy path", design
	// decision 4) -- implemented by internal/adapters/inbound/wshub's own
	// SandboxRegistry, app depends on the port
	// (internal/app/ports.SandboxCommander), never the adapter. May be nil
	// (some tests construct an Actor without one, e.g. the resilience
	// test, which never exercises the dispatch path at all).
	commander ports.SandboxCommander

	// provider is this Actor's own SandboxProvider (§9.3) -- the SAME
	// port every Actor of this Registry shares, used only by
	// handleEnsureDispatched's own spawn branch (dispatch.go) to actually
	// call CreateSandbox. May be nil (tests that never exercise the spawn
	// path, e.g. the resilience test).
	provider ports.SandboxProvider

	// publicBaseURL is this control plane's own externally-reachable
	// http(s):// base URL (platform.Config.PublicBaseURL, e.g.
	// "http://localhost:8080" in dev, a real "https://..." URL in
	// production) -- the SAME value §13.1's OAuth wiring already uses
	// for its own redirect URL. sessionconfig.go's own assembleSessionConfig
	// derives SessionConfig.ControlPlaneWsUrl from it by swapping the
	// scheme (http->ws, https->wss) rather than requiring a second,
	// separately-configured ws(s):// base URL field -- see that file's own
	// doc comment for the full reasoning.
	publicBaseURL string

	// sourceControl is this Actor's own ports.SourceControl (§9.3) --
	// used only by pushpr.go's createPRBestEffort, once a push_complete
	// event arrives for a turn that completed successfully, to open a pull
	// request. May be nil (tests that never exercise the push/PR path,
	// e.g. the resilience test).
	sourceControl ports.SourceControl

	// githubBotToken is §8.2's ("sentinels + suggestions", §17.2) own
	// addition -- see Registry's own identical field doc comment
	// (registry.go) for the full rationale; createSentinelFixPRBestEffort
	// (pushpr.go) is this Actor's own one use of it. May be empty (tests
	// that never exercise the sentinel-fix PR path).
	githubBotToken string

	// reviewModelDeep is §26.3's own addition (§26.3) -- see
	// RegistryOptions.ReviewModelDeep's own doc comment.
	reviewModelDeep string

	// diffFetcher is §14.4's ("handoff-readiness sentinel", §14.4) own
	// addition -- see Registry's own identical field doc comment
	// (registry.go) for the full rationale; handoffsentinel.go's own
	// runHandoffSentinelBestEffort is this Actor's own one use of it. May
	// be nil (tests that never exercise the handoff-sentinel path).
	diffFetcher PRDiffFetcher

	// reviewDiffFetcher/githubBotHandle are §24's ("review: automatic
	// re-review on new commits", §24) own additions -- see Registry's own
	// identical field doc comments (registry.go) for the full rationale;
	// reviewretrigger.go's own handleReviewRetriggerDebounceTimer is this
	// Actor's own one use of either. May be nil/empty (tests that never
	// exercise the automatic-re-review path).
	reviewDiffFetcher reviewcontext.Fetcher
	githubBotHandle   string

	// knowledgeRanker (§31.6/§34.7) is Registry.knowledgeRanker's own
	// doc comment -- composeAutoRetriggerPrompt (reviewretrigger.go) is
	// this Actor's own one use of it. May be nil (tests that never
	// exercise the automatic-re-review path): reviewcontext.
	// FetchPriorArchDecisions degrades a nil ranker to
	// knowledge.RecencyRanker{}, never a panic.
	knowledgeRanker ports.KnowledgeRanker

	// tokenEncryptionKey decrypts identities.access_token_encrypted (§13.1)
	// to obtain the session creator's own plaintext GitHub OAuth access
	// token -- the SAME key platform.Config.TokenEncryptionKey already
	// supplies §13.1's OAuth callback and the scm-credentials endpoint
	// (design decision 8); createPRBestEffort (pushpr.go) is this
	// package's own use of it. Never logged. May be nil/empty (tests that
	// never exercise the push/PR path).
	tokenEncryptionKey []byte

	// openCodeRuntimeVersion is §8.5's ("image builds") own addition:
	// the pinned OpenCode runtime version (platform.Config.
	// OpenCodeRuntimeVersion) fed into domain/imagebuild.Fingerprint
	// alongside a spawn's own resolved repo SHAs (dispatch.go/
	// imageresolve.go's resolveAndSetImage). May be empty (tests that
	// never exercise the image-resolution path).
	openCodeRuntimeVersion string

	// contractDriftDetected is §14.3's ("mocking + contract drift",
	// §14.3) own OTel counter -- the SAME instance every Actor this
	// Registry hydrates shares, constructed exactly once by NewRegistry
	// and threaded through at hydration time (hydrate.go), never
	// reconstructed per-Actor. Incremented by dispatch.go/contractdrift.go's
	// own checkContractDrift.
	contractDriftDetected metric.Int64Counter

	// opsMetrics is §5.3's ("ops: dashboards, alerts, runbooks", §5.3)
	// own bundle of five OTel instruments -- the SAME instance every Actor
	// this Registry hydrates shares, constructed exactly once by
	// NewRegistry and threaded through at hydration time (hydrate.go),
	// never reconstructed per-Actor. See opsmetrics.go's own top comment
	// for the full gap this closes and each instrument's own call site.
	opsMetrics opsMetrics

	// repoAccessCache is the audit fix's ("warm-boot image access control",
	// HIGH) own addition: the SAME *repoAccessCache instance every Actor
	// this Registry hydrates shares (registry.go's own field doc comment)
	// -- used only by imageresolve.go's own repo-access gate, immediately
	// before resolveAndSetImage will otherwise mint a pending image_builds
	// row or warm-hit an already-ready one. May be nil in a test that
	// never exercises the image-resolution path; the gate treats a nil
	// cache as "always miss, always check live" (see imageresolve.go).
	repoAccessCache *repoAccessCache

	// epistemicCheckDefault (F6, adversarial review) is the SAME
	// platform.Config.EpistemicCheckDefault value every Actor this
	// Registry hydrates shares (registry.go's own field doc comment) --
	// threaded into workflowengine.Deps.EpistemicCheckDefault at each of
	// this Actor's own three OnTurnCompleted call sites (pushpr.go,
	// dispatch.go, timerfired.go).
	epistemicCheckDefault bool

	// rolloutMode (§10 Phase 6, §32) is the SAME
	// Registry.rolloutMode value every Actor this Registry hydrates
	// shares (registry.go's own field doc comment, and RegistryOptions.
	// RolloutMode's own doc comment for why this is an options field, not
	// a required NewRegistry parameter) -- consulted by dispatch.go's own
	// refuseIfRolloutUnenrolled, the dispatch-time half of §32's "fail-
	// closed, twice" pair.
	rolloutMode platform.RolloutMode

	// pendingBroadcast queues each event appended (via appendEvent/
	// appendRawEvent) during the CURRENT transact attempt, in order. Safe
	// unsynchronized: Actor.handle processes exactly one command at a time
	// (the same single-goroutine invariant every other Actor field already
	// relies on, see doc.go's Concurrency section), so no two goroutines
	// ever touch this slice concurrently. Reset to nil on every transact
	// entry path (see transact's own doc comment) -- never accumulates
	// across separate commands or separate attempts.
	pendingBroadcast []json.RawMessage

	registry *Registry

	// lockConn holds the session-scoped Postgres advisory lock for this
	// Actor's entire life (§2: "held for the actor's lifetime"). It is
	// NEVER used for the Actor's own transactional writes -- transact
	// (below) always acquires a FRESH connection from the pool -- so it
	// never becomes a concurrency bottleneck.
	lockConn *pgxpool.Conn

	mailbox chan Command
	// done is closed exactly once, the instant run's loop returns for any
	// reason (idle-TTL eviction, ErrStaleEpoch, or the lifecycle context
	// being cancelled) -- via run's own deferred close, BEFORE shutdown's
	// slower eviction/unlock steps, so Send starts rejecting as early as
	// possible. Send checks it to report ErrActorStopped instead of
	// blocking forever, or sending on a channel nobody will ever read
	// from again.
	done chan struct{}

	logger *slog.Logger
}

// Send delivers cmd to the actor's mailbox, or reports ErrActorStopped if
// the actor's run loop has already exited.
//
// The done check runs FIRST, on its own, so that once the actor is known
// dead every Send deterministically fails -- without this priority stage,
// a single select with a buffered mailbox case and a closed done case
// would pick between the two pseudo-randomly, sometimes "accepting" a
// command into a mailbox nobody will ever read again.
//
// One narrow race is inherent to any channel-based mailbox and documented
// rather than hidden: a Send that passes the done check just as the run
// loop is exiting can still enqueue into the buffer and return nil. Those
// commands are drained and logged by shutdown on a best-effort basis (an
// enqueue racing the drain itself can still slip through unlogged) -- so
// delivery through Send is at-most-once by construction, and a command
// that must not be lost needs its own redelivery mechanism, exactly as
// TimerFired has (the timer pump's claim window redelivers an unhandled
// timer once TimerClaimDuration elapses).
func (a *Actor) Send(ctx context.Context, cmd Command) error {
	select {
	case <-a.done:
		return ErrActorStopped
	default:
	}
	select {
	case a.mailbox <- cmd:
		return nil
	case <-a.done:
		return ErrActorStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is the actor's own mailbox-processing loop, started exactly once by
// Registry.GetOrSpawn via errgroup.Group.Go. It processes exactly one
// command at a time against the mailbox channel and a shutdown signal
// (ctx.Done, or its own idle timer) -- this serialization is what makes
// §2's "single writer" true even though Postgres itself would happily
// accept concurrent connections from this same process.
//
// Deferred-cleanup ordering matters here (LIFO): close(a.done) is
// registered LAST so it runs FIRST the instant the loop exits -- marking
// the actor dead for Send before shutdown starts its slower map-eviction
// and network-round-trip unlock steps. Without that ordering, the whole
// unlock round-trip is a window during which GetOrSpawn still hands this
// dead actor out and Send still accepts commands nobody will ever read.
func (a *Actor) run(ctx context.Context) error {
	defer a.shutdown()
	defer close(a.done)

	idleTimer := time.NewTimer(a.timeouts.ActorIdleTTL)
	defer idleTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			a.logger.Info("sessionactor: run loop stopping: context done", "error", ctx.Err())
			return ctx.Err()

		case <-idleTimer.C:
			// §2: "evicts after idle TTL (default 30 min without
			// commands or connected clients)". This Step has no
			// mechanism to observe "connected clients" (that's the
			// client WS hub) -- only "no commands" is
			// checked here; see doc.go for the same documented gap.
			a.logger.Info("sessionactor: evicting self: idle TTL elapsed with no commands")
			return nil

		case cmd, ok := <-a.mailbox:
			if !ok {
				return nil
			}

			if err := a.handle(ctx, cmd); err != nil {
				if errors.Is(err, ErrStaleEpoch) {
					a.logger.Error("sessionactor: stale epoch detected -- a newer actor has taken over; evicting self", "error", err)
					return err
				}
				// Any other error is logged but non-fatal to the actor
				// itself: a transient failure handling one command must
				// not stop it from processing the next one, or make it
				// permanently unreachable (that would require an
				// external eviction + a fresh GetOrSpawn to recover
				// from, for no correctness benefit -- this actor is
				// still the legitimate, non-fenced owner of the
				// session).
				a.logger.Error("sessionactor: command handling failed", "error", err)
			}

			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(a.timeouts.ActorIdleTTL)
		}
	}
}

// shutdown finishes tearing the actor down after run's own deferred
// close(a.done) has already marked it dead. Deferred by run, so it
// executes exactly once regardless of which branch run returned from.
// Step order is deliberate:
//
//  1. evict from the Registry's map FIRST, so GetOrSpawn stops handing
//     this dead actor out (a caller arriving during the remaining steps
//     misses the map, attempts hydration, and gets a clean
//     ErrSessionActorElsewhere until the lock below is released --
//     fail-fast, retryable, never a silent black hole);
//  2. drain the mailbox, logging any command that slipped in through
//     Send's inherent enqueue-vs-death race (see Send's own comment) --
//     dropped commands are observable in logs, never silently retained;
//  3. release the advisory lock LAST -- the slow, network-bound step,
//     safe to do last precisely because steps 1-2 already made this
//     actor unreachable.
//
// Uses context.Background() rather than run's own ctx: by the time this
// runs, ctx may already be Done (process shutdown, or the idle-TTL/
// ErrStaleEpoch paths that return before any external cancellation) and
// the unlock statement must still be attempted with a live, un-cancelled
// context.
func (a *Actor) shutdown() {
	a.registry.evict(a.sessionID, a)
	a.drainMailbox()
	unlockAndRelease(context.Background(), a.lockConn, a.sessionID)
}

// drainMailbox empties whatever commands were still buffered (or raced
// in) when the run loop exited, logging each one: they were accepted by
// Send but will never be processed by THIS actor. TimerFired commands are
// re-delivered automatically by the timer pump once their claim window
// elapses; any future command type without redelivery semantics will at
// least be visibly accounted for here instead of vanishing.
func (a *Actor) drainMailbox() {
	for {
		select {
		case cmd := <-a.mailbox:
			a.logger.Error(
				"sessionactor: dropping command accepted during shutdown; it will not be processed by this actor",
				"command", fmt.Sprintf("%T", cmd),
			)
		default:
			return
		}
	}
}

// appendRawEvent inserts a session event row inside tx, exactly like
// timerfired.go's own appendEvent, but for a caller that already holds raw
// wire bytes to persist verbatim (§3.2's sandboxevent.go) rather than a
// map[string]any this package would otherwise have to marshal itself --
// skipping that round-trip means the append-only event log holds precisely
// what the sandbox sent, not a lossy re-encoding through an intermediate Go
// value. messageID is the wire event's own top-level "messageId" (cmd.
// MessageID, command.go) -- CreateEvent upserts on (session_id, messageID)
// (§6.1), so a genuine resend of an already-persisted event is deduped
// (Inserted false) rather than appended as an indistinguishable duplicate
// row; see this function's own broadcast-queueing guard below for why a
// deduped resend must never reach a.pendingBroadcast twice.
//
// Returns row.Inserted (an audit-fix batch's own addition, finding M16,
// "completeness"): handleSandboxEvent's own cmd.Type == "tool_call" case
// (sandboxevent.go) reuses this SAME already-computed signal to gate its
// mid-turn Linear progress notification against a wire-level redelivery
// of an already-processed tool_call, rather than inventing a second,
// separate dedupe check for the same underlying fact -- see
// progressnotify.go's own doc comment for the full reasoning.
func (a *Actor) appendRawEvent(ctx context.Context, tx pgx.Tx, eventType string, messageID string, raw json.RawMessage) (bool, error) {
	row, err := a.stores.event.WithTx(tx).Create(ctx, sqlcgen.CreateEventParams{
		SessionID: a.sessionID,
		Type:      eventType,
		MessageID: messageID,
		Payload:   raw,
	})
	if err != nil {
		return false, fmt.Errorf("sessionactor: append raw %s event: %w", eventType, err)
	}
	// Queue for broadcast AFTER commit (§6.2's "→ broadcast stream", made
	// generic rather than per-handler -- see transact's own doc comment
	// for the commit-then-broadcast, discard-on-rollback ordering) -- only
	// when this call genuinely inserted a fresh row: a deduped resend
	// (Inserted false) must never cause the SAME event to be broadcast
	// twice to live subscribers.
	if row.Inserted {
		a.pendingBroadcast = append(a.pendingBroadcast, raw)
	}
	return row.Inserted, nil
}

// transact is the ONLY way this package writes session/turn/sandbox state
// (§2's transactional-write rule): it acquires a FRESH connection from
// the pool (never lockConn),
// begins a transaction, re-reads the session's actor_epoch INSIDE that
// transaction and fences it against the epoch this Actor was hydrated
// with -- returning ErrStaleEpoch (never running fn, never committing) if
// they no longer match, since that proves a newer actor has since taken
// ownership -- and otherwise runs the caller's fn (the actual
// state-transition writes) and commits. §2: "state transition + appended
// event + outbox entries commit in ONE Postgres transaction. There is no
// such thing as a fire-and-forget state write."
//
// Broadcasting (§6.2's "→ broadcast stream") is wired here, generically,
// rather than in any individual command handler: appendEvent/
// appendRawEvent both queue their own already-marshaled payload onto
// a.pendingBroadcast as they run inside fn (see each of their own doc
// comments). On any early return -- fn's own error, or tx.Commit's own
// error -- that queue is discarded (a.pendingBroadcast reset to nil)
// without ever calling a.broadcaster: an event that might still roll back,
// or whose surrounding transaction never actually committed, must never
// be announced to a live client. Only on a SUCCESSFUL commit is the queue
// taken, the field reset to nil, and THEN (deliberately after the reset,
// so a panic or re-entrancy inside Broadcast can never corrupt the next
// call's own queue) each payload handed to a.broadcaster.Broadcast, once
// per item, in order. A nil a.broadcaster (some tests construct an Actor
// without one) is guarded against by skipping the loop entirely.
func (a *Actor) transact(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	conn, err := a.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("sessionactor: transact: acquire connection: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sessionactor: transact: begin: %w", err)
	}
	// Rollback is a safety net for every return path other than a
	// successful Commit below; pgx reports (and this discards) an
	// already-closed-transaction error on the no-op case where Commit
	// already ran.
	defer func() { _ = tx.Rollback(ctx) }()

	currentEpoch, err := a.stores.session.WithTx(tx).GetActorEpochForUpdate(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: transact: get actor epoch for update: %w", err)
	}
	if currentEpoch != a.epoch {
		return ErrStaleEpoch
	}

	if err := fn(ctx, tx); err != nil {
		a.pendingBroadcast = nil
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		a.pendingBroadcast = nil
		return fmt.Errorf("sessionactor: transact: commit: %w", err)
	}

	a.broadcastPending()
	return nil
}

// broadcastPending delivers every queued event payload (in order) to
// a.broadcaster, then resets the queue -- called only after transact's own
// successful commit above. Takes and resets a.pendingBroadcast BEFORE
// calling Broadcast for any item, so a panic or re-entrant call partway
// through the loop can never leave a stale/duplicated queue for the next
// transact attempt to inherit.
func (a *Actor) broadcastPending() {
	pending := a.pendingBroadcast
	a.pendingBroadcast = nil

	if a.broadcaster == nil {
		return
	}
	for _, payload := range pending {
		a.broadcaster.Broadcast(a.sessionID.String(), payload)
	}
}
