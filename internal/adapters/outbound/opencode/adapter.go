package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/app/ports"
)

// ssePollDivisor derives Adapter.pollInterval from the caller-supplied
// sseInactivityTimeout by plain integer division, rather than introducing
// either a dedicated new platform.Timeouts field or a forbidden
// time.Duration unit literal (§5.4/§11's notimeliteral lint forbids
// time.Second/time.Millisecond/etc. selectors anywhere outside
// platform/timeouts.go and _test.go files) — this Step's own instructions
// scope new Timeouts fields narrowly to exactly the two opencodeproc needs.
// 24 keeps the poll granularity comfortably fine relative to the default
// 120s SSEInactivityTimeout (5s) without hammering anything, and scales
// automatically if that default is ever reconfigured.
const ssePollDivisor = 24

// fallbackReconnectGraceMultiplier bounds how many multiples of
// sseInactivityTimeout waitForTurn's own poll loop waits for reconnection to
// recover a dropped connection (Finding 1/2's own liveness disambiguation)
// before finalizing via the fallback anyway -- a plain named int constant,
// not a forbidden time.Duration literal (§5.4/§11's notimeliteral lint
// forbids time.Second/time.Millisecond/etc. selectors outside
// platform/timeouts.go and _test.go files; this multiplies an existing
// duration FIELD, mirroring ssePollDivisor's own precedent above, not a
// fresh unit literal). 2x gives reconnection (now happening every
// platform.Timeouts.OpenCodeSSEReconnectInterval, deliberately much shorter
// than sseInactivityTimeout) a full extra window beyond the normal
// per-turn-idle threshold before concluding the turn's own real outcome is
// unrecoverable -- long enough to be a genuine grace period for a
// same-machine, sandbox-agent-managed process's connection to recover, short
// enough that a turn can never wait unboundedly on a connection that never
// comes back.
const fallbackReconnectGraceMultiplier = 2

// Adapter is the OpenCode ports.AgentRuntime implementation (§7): a pure
// HTTP+SSE client against an ALREADY-RUNNING `opencode serve` process —
// mirroring internal/adapters/outbound/modal's own shape exactly. This
// package does NOT spawn or supervise the OpenCode process itself; that is
// internal/sandboxagent/opencodeproc's job, using
// internal/sandboxagent/supervisor — the same separation Modal's adapter
// already establishes relative to §6.4's supervisor. Build one with New
// from a baseURL an opencodeproc.Spawn call already confirmed healthy.
//
// # Verified vs. schema-derived vs. best-effort
//
// See types.go's own per-type comments for the detailed breakdown; the
// short version: session/prompt_async/abort/message-list shapes and the
// text/step-start/step-finish/tool part progressions (including a genuine
// abort producing session.error{MessageAbortedError}) were all directly
// observed against the real, installed OpenCode 1.17.15 binary during this
// Step's own research pass. The "subtask" part type was schema-derived
// only at the time — a later Step's own investigation DID go on to
// exercise OpenCode's own "task" tool live (see "Sub-task handling" below)
// and found that this part type still never fires; that section is the
// up-to-date, empirically-verified account of this adapter's own §7.1
// sub-task handling.
//
// # Sub-task handling (§7.1): the real correlator, found by live investigation
//
// A later Step's own investigation set out to find OpenCode's real,
// live wire correlator for §7.1's "subTaskId" fan-out (which event tells
// this adapter that a batch of subsequent events belongs to a spawned
// sub-agent, not the turn's own main lane) — starting a real `opencode
// serve` process, scripting a prompt that explicitly delegates via
// OpenCode's own "task" tool, and inspecting the real resulting SSE trace.
// Two things came out of that investigation:
//
//  1. The "subtask" part type (subtaskPart, types.go) — the ONLY signal
//     this adapter originally watched for — never appeared on the wire at
//     all for a real, live-triggered task-tool invocation. It remains
//     schema-present (confirmed in /doc) but is now further confirmed
//     unverified-live even after a genuine attempt to trigger it; kept
//     only as an extra fallback path (dispatchSubtaskStart, sse.go) in
//     case some other OpenCode-internal path emits it.
//  2. The REAL signal observed: an ordinary "tool" part (tool=="task") —
//     going through the SAME dispatchTool machinery every other tool call
//     already does — whose own toolPartState carries a "metadata" object
//     (taskToolMetadata, types.go: {"parentSessionId","sessionId"}) once
//     status reaches "running" (confirmed still present at "completed").
//     sessionId is the newly-spawned sub-agent's own DISTINCT OpenCode
//     session id — every one of ITS OWN inner text/tool/step-start/
//     step-finish parts subsequently arrives on the SAME global /event
//     stream tagged with THAT session id as its own top-level
//     "sessionID", never the enclosing turn's.
//
// This is genuinely more information than §7.1 assumed was available when
// it was written ("OpenCode's own nested-task id" was documented as the
// correlator in the abstract, without having independently confirmed a
// concrete field for it): maybeStartTaskSubtask/maybeFinishTaskSubtask
// (sse.go) use meta.SessionID directly as this sub-task's own subTaskId,
// and Adapter.registerSubtaskSession/resolveEvent route that CHILD
// session's own subsequent events back to the SAME turnState, tagged with
// that subTaskId, via translateToken/translateStepStart/
// translateStepFinish/translateToolCall/translateToolResult (translate.go)
// — each of the six event types Finding 1's own schema change added
// subTaskId to.
//
// This ALSO gives this adapter a genuinely more precise sub_task_finish
// than before: the task tool call's own "completed"/"error" transition
// (maybeFinishTaskSubtask) is a real, per-sub-task completion signal, not
// just the enclosing turn's own eventual outcome. finalize's own
// drainOpenSubtasks (below) is kept as the fallback for the one case that
// signal can't cover: a sub-task whose own task tool call never reaches a
// terminal status before the turn itself ends (e.g. the turn is
// cancelled while the task tool call is still "running") — that sub-task
// is still closed out using the ENCLOSING turn's own outcome, exactly as
// documented before this investigation.
//
// What remains genuinely unverified/best-effort, honestly, after this
// investigation: a sub-agent's own INTERNAL failure (e.g. an assistant
// error inside its own conversation, as opposed to the task tool call
// itself reporting "error") is not separately modeled — only the task
// tool's own reported status governs a sub-task's own outcome, mirroring
// how an ordinary tool_result's own isError already works. The model
// stays flat (§7.1: a sub-task cannot itself spawn a further-nested
// sub-task) — nothing here defends against or specially handles a nested
// "task" tool call occurring INSIDE a sub-agent's own conversation, since
// OpenCode's own real task-tool contract does not appear to allow one.
type Adapter struct {
	baseURL    string
	httpClient *http.Client

	// capabilityRestricted ("sentinels + suggestions", §17.2) is
	// set ONCE, at construction, from SessionConfig.CapabilityRestricted
	// -- true exactly for a sentinel-auto-fix child session. postPromptAsync
	// (session.go) is this field's own one reader: every build-mode turn
	// dispatched while it is true selects OpenCode's own glob-restricted
	// "sentinel-fix" custom agent instead of the ordinary "build" one.
	capabilityRestricted bool

	sseInactivityTimeout time.Duration
	pollInterval         time.Duration

	// reconnectInterval is runEventLoop's own reconnect delay after a
	// dropped GET /event connection (platform.Timeouts.
	// OpenCodeSSEReconnectInterval) -- deliberately a SEPARATE field from
	// sseInactivityTimeout (Finding 2: reusing the latter as the
	// reconnect delay made reconnection structurally unable to ever beat
	// the per-turn fallback race).
	reconnectInterval time.Duration

	// requestTimeout bounds every doJSON-routed HTTP call (client.go) via
	// a per-request context.WithTimeout wrap (Finding 3) --
	// platform.Timeouts.OpenCodeRequestTimeout. Deliberately NOT applied
	// as a client-wide http.Client.Timeout: connectAndConsume's own GET
	// /event call uses this SAME httpClient for the intentionally
	// long-lived persistent SSE stream, which a client-wide Timeout would
	// incorrectly kill.
	requestTimeout time.Duration

	// summarizeTimeout bounds forceCompaction's own POST
	// /session/{id}/summarize call specifically (compact.go), via
	// doJSONTimeout directly rather than the shared requestTimeout above
	// (§7.2 Finding 3) -- platform.Timeouts.OpenCodeSummarizeTimeout, 120s
	// in production, deliberately more generous than requestTimeout since
	// a real compaction over a large, genuinely-overflowed context can
	// plausibly take far longer than an ordinary session/catalog/
	// message-list call.
	summarizeTimeout time.Duration

	// transientRetryBackoff bounds how long Adapter.attemptTransientRetry
	// (this Step: "typed transient-error retry for the OpenCode adapter")
	// waits before re-dispatching the SAME prompt after a first-time
	// transient APIError (isTransientAPIError, outcome.go) --
	// platform.Timeouts.OpenCodeTransientRetryBackoff. Deliberately a
	// SEPARATE field from summarizeTimeout above (§7.2's own compaction
	// retry needs no backoff at all -- forceCompaction itself is the
	// recovery action there, not a wait) -- see that field's own doc
	// comment (platform/timeouts.go) for why an immediate retry of a
	// provider blip (429/529) is generally still likely to fail again
	// instantly without a short pause first.
	transientRetryBackoff time.Duration

	// runtimeVersion/sandboxID are §7.3's own ("a retry decision is
	// not a diagnosis") two adapter-side facts a provider-failure
	// diagnostic (diagnostic.go) needs and OpenCode's own error payload
	// never carries: runtimeVersion is the pinned OpenCode binary version
	// this sandbox actually ran (opencodeproc.Result.Version, best-effort
	// — "" when undiscoverable — sourced identically to §7's own boot
	// fingerprint, cmd/sandbox-agent/main.go's own postSpawnFingerprint);
	// sandboxID is the sandbox this turn ran on
	// (SessionConfig.SandboxId). Both set once, at construction, and
	// never mutated — read-only from every goroutine thereafter, exactly
	// like baseURL/capabilityRestricted above.
	runtimeVersion string
	sandboxID      string

	mu               sync.Mutex
	currentSessionID string

	// disconnectMu guards lastDisconnectAt -- the adapter-level (not
	// per-turn) record of the last time runEventLoop's own connectAndConsume
	// call observed a GENUINE connection error (a real outage), set BEFORE
	// the reconnect-delay wait so it reflects when the outage STARTED, not
	// when reconnection later succeeds (see recordDisconnect below). Zero
	// value means no disconnect has ever been observed. Deliberately never
	// reset on a successful reconnect: shouldFinalizeByFallback's own
	// disambiguation needs to ask "did a disconnect happen at any point
	// during THIS turn's own current idle episode", which only a value that
	// stays put (compared against a per-turn timestamp) can answer --
	// reconnecting successfully doesn't retroactively prove any given turn
	// is fine, only that reconnection is now technically possible. A
	// separate mutex from mu above (which guards the unrelated
	// currentSessionID) keeps each concern's own lock scoped to exactly
	// what it protects, matching this file's own existing per-concern-
	// locking style.
	disconnectMu     sync.Mutex
	lastDisconnectAt time.Time

	turnsMu sync.Mutex
	turns   map[string]*turnState

	// subtasksMu/subtaskSessions extend the turns registry above to ALSO
	// route a sub-task's own distinct child OpenCode session id back to
	// the SAME enclosing turnState, tagged with that sub-task's own
	// subTaskId — see maybeStartTaskSubtask (sse.go) and this type's own
	// doc comment for the real, empirically-verified §7.1 correlator this
	// implements. A separate mutex/map (rather than folding this into
	// turns/turnsMu itself) keeps "which turn does this MAIN session
	// belong to" and "which turn/sub-task does this CHILD session belong
	// to" independently scoped, matching this file's own existing
	// per-concern-locking style (see disconnectMu above).
	subtasksMu      sync.Mutex
	subtaskSessions map[string]subtaskSession

	connectOnce sync.Once
	connectedCh chan struct{}

	// bgCtx/bgCancel are this Adapter's own lifetime context: bgCtx is
	// runEventLoop's own ctx (New below), canceled by Close via bgCancel.
	// bgCtx is ALSO reused (§7.2) as the base context for
	// attemptCompactionRetry's own background forceCompaction/postPromptAsync
	// retry call — see finalizeOrRecoverFromOverflow's own doc comment for
	// why this, rather than an independent context.Background(), is the
	// right choice: it ties a recovery attempt's own lifetime to this
	// Adapter's own lifecycle, so Close (bgCancel then group.Wait) can
	// never block indefinitely on one that outlives the adapter's own
	// intended shutdown window.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	group    errgroup.Group
}

var _ ports.AgentRuntime = (*Adapter)(nil)

// New constructs an Adapter against baseURL (an already-running OpenCode
// server — see internal/sandboxagent/opencodeproc.Spawn) and immediately
// starts ONE persistent connection to OpenCode's own global GET /event
// stream, opened here once for this Adapter's whole lifetime rather than
// per-turn (§7: "filter the global event stream by session id" — a single
// shared stream is what makes that filtering necessary and meaningful in
// the first place; opening a fresh stream per turn would also risk a
// subscribe-before-first-event race this design avoids entirely), via
// this Adapter's own zero-value errgroup.Group.Go call — the same
// "background work started from within a constructor/Spawn-style call,
// via the type's own errgroup field, never a bare `go` statement"
// pattern internal/sandboxagent/supervisor.Supervisor.Spawn already
// establishes for its own reap goroutine.
//
// sseInactivityTimeout, reconnectInterval, requestTimeout, summarizeTimeout,
// and transientRetryBackoff are, in production, platform.Timeouts.
// SSEInactivityTimeout, OpenCodeSSEReconnectInterval, OpenCodeRequestTimeout,
// OpenCodeSummarizeTimeout, and OpenCodeTransientRetryBackoff respectively:
// sseInactivityTimeout is the per-turn silence threshold both waitForTurn's
// own fallback and its own disconnect-during-this-idle-episode
// disambiguation (disconnectedSince) share; reconnectInterval is how long
// runEventLoop waits before retrying a dropped GET /event connection
// (deliberately much shorter than sseInactivityTimeout — Finding 2 — so
// reconnection has a real chance to win against the fallback); requestTimeout
// bounds every doJSON-routed HTTP call (Finding 3), applied per-request in
// client.go, never as a client-wide http.Client.Timeout (which would also
// incorrectly bound the persistent SSE connection below); summarizeTimeout
// (§7.2) bounds forceCompaction's own POST /session/{id}/summarize call
// specifically, deliberately separate from and more generous than
// requestTimeout; transientRetryBackoff (this Step: "typed transient-error
// retry for the OpenCode adapter") bounds Adapter.attemptTransientRetry's own
// wait before re-dispatching after a first-time transient APIError — see
// that field's own doc comment above for why this, unlike the compaction
// retry, needs a backoff at all.
// runtimeVersion/sandboxID (§7.3) are threaded straight onto the
// Adapter fields of the same name — see those fields' own doc comment for
// what each is and where a real caller sources it
// (cmd/sandbox-agent/main.go: opencodeproc.Result.Version and
// SessionConfig.SandboxId respectively). Neither is validated non-empty
// here: an empty value is a legitimate, honest zero value (e.g. a
// best-effort version discovery that failed) and simply renders as an
// absent field on ProviderFailureDiagnostic (omitempty), never a
// placeholder string.
//
// capabilityRestricted (§17.2) is a trailing, variadic bool
// parameter -- so every EXISTING caller (cmd/sandbox-agent/main.go's own
// production wiring, this package's own newAdapter(t) test helper) keeps
// compiling and behaving identically (capabilityRestricted false) with no
// change of its own; only the ONE real caller that needs true (cmd/
// sandbox-agent/main.go, threading SessionConfig.CapabilityRestricted)
// supplies it explicitly.
func New(baseURL string, sseInactivityTimeout, reconnectInterval, requestTimeout, summarizeTimeout, transientRetryBackoff time.Duration, runtimeVersion, sandboxID string, capabilityRestricted ...bool) *Adapter {
	bgCtx, cancel := context.WithCancel(context.Background())

	restricted := false
	if len(capabilityRestricted) > 0 {
		restricted = capabilityRestricted[0]
	}

	a := &Adapter{
		baseURL:               strings.TrimSuffix(baseURL, "/"),
		capabilityRestricted:  restricted,
		httpClient:            &http.Client{},
		sseInactivityTimeout:  sseInactivityTimeout,
		reconnectInterval:     reconnectInterval,
		requestTimeout:        requestTimeout,
		summarizeTimeout:      summarizeTimeout,
		transientRetryBackoff: transientRetryBackoff,
		runtimeVersion:        runtimeVersion,
		sandboxID:             sandboxID,
		pollInterval:          sseInactivityTimeout / ssePollDivisor,
		turns:                 make(map[string]*turnState),
		subtaskSessions:       make(map[string]subtaskSession),
		connectedCh:           make(chan struct{}),
		bgCtx:                 bgCtx,
		bgCancel:              cancel,
	}

	a.group.Go(func() error {
		return a.runEventLoop(bgCtx)
	})

	return a
}

// Close stops the persistent SSE connection and waits for it to actually
// exit. Not part of ports.AgentRuntime (the port declares no lifecycle-
// teardown method — see agentruntime.go's own doc comment on why nothing
// engine-specific belongs there); an extra capability on the concrete
// type, called by cmd/sandbox-agent/main.go's own shutdown sequence and by
// this package's own tests.
func (a *Adapter) Close() {
	a.bgCancel()
	_ = a.group.Wait()
}

// Connected blocks until the persistent SSE stream has observed at least
// one server.connected handshake, or ctx is done — used by this package's
// own tests to prove the stream came up; not required for StartTurn's own
// correctness (a turn's prompt_async POST and its own SSE-inactivity
// fallback are safe even if called before the very first server.connected
// arrives).
func (a *Adapter) Connected(ctx context.Context) error {
	select {
	case <-a.connectedCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Adapter) getCurrentSession() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentSessionID
}

func (a *Adapter) setCurrentSession(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.currentSessionID = id
}

func (a *Adapter) registerTurn(sessionID string, ts *turnState) {
	a.turnsMu.Lock()
	defer a.turnsMu.Unlock()
	a.turns[sessionID] = ts
}

func (a *Adapter) unregisterTurn(sessionID string) {
	a.turnsMu.Lock()
	defer a.turnsMu.Unlock()
	delete(a.turns, sessionID)
}

func (a *Adapter) lookupTurn(sessionID string) *turnState {
	a.turnsMu.Lock()
	defer a.turnsMu.Unlock()
	return a.turns[sessionID]
}

// subtaskSession is one registered sub-task's own routing entry:
// register/unregisterSubtaskSession/lookupSubtaskSession below.
type subtaskSession struct {
	ts        *turnState
	subTaskID string
}

// registerSubtaskSession/unregisterSubtaskSession/lookupSubtaskSession
// implement the child-session routing this type's own subtaskSessions
// field documents — called by maybeStartTaskSubtask/maybeFinishTaskSubtask
// (sse.go) and by finalize's own drainOpenSubtasks loop below.
func (a *Adapter) registerSubtaskSession(childSessionID string, ts *turnState, subTaskID string) {
	a.subtasksMu.Lock()
	defer a.subtasksMu.Unlock()
	a.subtaskSessions[childSessionID] = subtaskSession{ts: ts, subTaskID: subTaskID}
}

func (a *Adapter) unregisterSubtaskSession(childSessionID string) {
	a.subtasksMu.Lock()
	defer a.subtasksMu.Unlock()
	delete(a.subtaskSessions, childSessionID)
}

func (a *Adapter) lookupSubtaskSession(childSessionID string) (*turnState, string, bool) {
	a.subtasksMu.Lock()
	defer a.subtasksMu.Unlock()
	sub, ok := a.subtaskSessions[childSessionID]
	return sub.ts, sub.subTaskID, ok
}

// resolveEvent resolves sessionID to whichever turnState it belongs to —
// the turn's own main OpenCode session (subTaskID == "", the overwhelming
// common case) or a sub-task's own distinct child session (subTaskID !=
// "", see registerSubtaskSession above) — for dispatchEvent (sse.go) to
// route by uniformly, regardless of which lane an incoming SSE event's own
// top-level sessionID belongs to. ok is false when sessionID matches
// neither — the existing "no registered turn for this session, silently
// drop" case dispatchEvent's own doc comment already documents.
func (a *Adapter) resolveEvent(sessionID string) (ts *turnState, subTaskID string, ok bool) {
	if ts := a.lookupTurn(sessionID); ts != nil {
		return ts, "", true
	}
	return a.lookupSubtaskSession(sessionID)
}

// recordDisconnect records that runEventLoop's own connectAndConsume call
// just observed a GENUINE connection error (a real outage), called BEFORE
// the reconnect-delay wait so this reflects the moment the outage STARTED,
// not when reconnection later succeeds — Finding 1/2's own disambiguation
// (see shouldFinalizeByFallback below) depends on this being the outage's
// own start time, not a point-in-time "is the stream fresh right now"
// snapshot.
func (a *Adapter) recordDisconnect() {
	a.disconnectMu.Lock()
	defer a.disconnectMu.Unlock()
	a.lastDisconnectAt = time.Now()
}

// disconnectedSince reports whether a genuine disconnect (recordDisconnect
// above) was observed strictly after t — shouldFinalizeByFallback's own
// disambiguation passes a turn's own lastActivityTime as t, to ask "did a
// real outage occur at any point during THIS turn's current idle episode",
// as opposed to merely asking whether the stream looks fresh RIGHT NOW (a
// point-in-time snapshot that reconnecting alone flips back to "fresh"
// without proving anything about what happened during the turn's own
// silence). The zero value of lastDisconnectAt (no disconnect ever
// observed) is never After any real t, so this correctly reports false
// until the first genuine disconnect.
func (a *Adapter) disconnectedSince(t time.Time) bool {
	a.disconnectMu.Lock()
	defer a.disconnectMu.Unlock()
	return a.lastDisconnectAt.After(t)
}

// StartTurn implements ports.AgentRuntime — see that interface's own doc
// comment for the full contract. Dispatches cmd as a turn, streams every
// translated event to sink live, and always attempts exactly one
// execution_complete-shaped terminal event before returning — including on
// EVERY early-return path below, not just the common case: a ctx
// cancellation before resolveSession/postPromptAsync ever completes, or
// while waitForTurn is still blocked, now correctly finalizes via
// finalizeCanceled instead of returning silently (Finding 5 — the three
// gaps this promise used to have).
//
// §3.3 ("turn recovery") adds onConversationID: invoked immediately
// after resolveSession succeeds below — BEFORE registerTurn/
// postPromptAsync/waitForTurn, i.e. well before this call's own long
// block for the rest of the turn — with the real, resolved sessionID.
// Guarded against nil (some callers/tests have no need for early
// reporting) and never called on the resolveSession failure path (no real
// id was ever resolved there — see ConversationIDReporter's own doc
// comment: it fires "at most once... with a real, non-empty, resolved
// conversation id").
func (a *Adapter) StartTurn(ctx context.Context, cmd sandboxws.Prompt, sink ports.EventSink, onConversationID ports.ConversationIDReporter) (string, error) {
	// Created up front, before resolveSession is ever called, so EVERY
	// subsequent return path below — including the very first one — has
	// a valid turnState to finalize through. This does not register it in
	// a.turns any earlier than today: nothing dispatches SSE events for a
	// session that doesn't exist yet, so only its own local existence
	// needs to move up (registerTurn below is unchanged).
	ts := newTurnState(cmd, sink)

	sessionID, err := a.resolveSession(ctx, cmd)
	if err != nil {
		if ctx.Err() != nil {
			a.finalizeCanceled(ts)
			return "", ctx.Err()
		}
		reason := "opencode: could not start a conversation"
		a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeFailed, Reason: &reason})
		return "", nil
	}

	// §3.3 "at turn start... never lazily": report the resolved
	// conversation id THE MOMENT it's known, not once this whole
	// (possibly minutes-long) call eventually returns it — see this
	// method's own doc comment above.
	if onConversationID != nil {
		onConversationID(sessionID)
	}

	a.setCurrentSession(sessionID)

	a.registerTurn(sessionID, ts)
	defer a.unregisterTurn(sessionID)

	// resolveModel (session.go), NOT resolveModelForced: cmd.Model is
	// genuinely optional on prompt_async's own wire shape
	// (promptAsyncRequest.Model, types.go), and a nil cmd.Model (the
	// default configuration every one of Composer/PlanModeView/Timeline
	// resume/DecisionInbox dispatches through, web/src/session) must
	// reach OpenCode with the "model" field OMITTED, letting the engine
	// apply its own configured default — never a Narvi-side substitute of
	// its own (§7.3; internal/app/workflowengine/advance.go states this
	// same rule for modelID/effort's own session-row fallback). Which
	// model actually ran is recovered a different way, from the engine's
	// own report on the resulting assistant message, not by forcing one
	// onto the request here — see ProviderFailureDiagnostic.Model's own
	// doc comment (diagnostic.go).
	model := a.resolveModel(ctx, (*string)(cmd.Model))
	if err := a.postPromptAsync(ctx, sessionID, cmd, model); err != nil {
		if ctx.Err() != nil {
			a.finalizeCanceled(ts)
			return sessionID, ctx.Err()
		}
		reason := "opencode: could not dispatch prompt"
		a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeFailed, Reason: &reason})
		return sessionID, nil
	}

	a.waitForTurn(ctx, sessionID, ts)
	return sessionID, nil
}

// finalizeCanceled finalizes ts with a Cancelled outcome and an honest,
// fixed reason string — used by every one of StartTurn/waitForTurn's own
// ctx-cancellation early-return paths (Finding 5). Cancelled (rather than
// Failed) is the correct outcome for an externally-imposed interruption
// like this, not a genuine execution failure — the same precedent
// deriveOutcome (outcome.go) already follows for a real
// MessageAbortedError. Safe to call even if ts is already finalized
// (finalize's own tryFinalize guard makes every finalize call idempotent),
// so no caller here needs to reason about whether some OTHER goroutine
// (e.g. the SSE loop's own session.idle/session.error dispatch) might have
// already finalized ts first.
func (a *Adapter) finalizeCanceled(ts *turnState) {
	reason := "opencode: turn context canceled before completion"
	a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeCancelled, Reason: &reason})
}

// waitForTurn blocks until ts.done is closed (the SSE loop's own
// dispatchEvent already finalized and emitted the turn's terminal event
// via session.idle/session.error), ctx is canceled, or this turn's own
// SSE-inactivity fallback fires — polling ts.idleFor rather than a
// Timer.Reset-based scheme (see turnState.idleFor's own doc comment for
// why). A ctx cancellation now finalizes ts (Cancelled) before returning
// (Finding 5) rather than leaving the turn's own execution_complete
// promise unfulfilled — safe even if some OTHER goroutine (the SSE loop,
// or this turn's own fallback path racing on a near-simultaneous tick)
// already finalized ts first, since finalizeCanceled's underlying
// a.finalize call is idempotent.
func (a *Adapter) waitForTurn(ctx context.Context, sessionID string, ts *turnState) {
	ticker := time.NewTicker(a.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.finalizeCanceled(ts)
			return
		case <-ts.done:
			return
		case <-ticker.C:
			if a.shouldFinalizeByFallback(ts) {
				a.finalizeByFallback(ctx, sessionID, ts)
				select {
				case <-ts.done:
					return
				default:
					// finalizeByFallback did NOT itself finalize ts on this
					// call. Every way it can reach this point without
					// closing ts.done (see its own doc comment for the
					// full breakdown) still leaves this turn's own eventual
					// completion in the hands of a goroutine that WILL
					// close it, or leaves the turn genuinely still active
					// (in which case returning early would be wrong
					// regardless):
					//
					//   - It launched a brand-new recovery retry
					//     (finalizeOrRecoverFromOverflow's own
					//     overflowActionBeginRetry branch, discovered via
					//     THIS fallback path rather than the live SSE one):
					//     attemptCompactionRetry or attemptTransientRetry
					//     (adapter.go, whichever this turn's own error kind
					//     selected) always eventually calls a.finalize on
					//     every one of its own failure paths, or lets a
					//     successful re-dispatch's own eventual live
					//     session.idle/session.error do so instead.
					//   - It abandoned (turnState.resolveOverflowAction's own
					//     overflowActionStale, turn.go, reached via
					//     finalizeOrRecoverFromOverflow) because a recovery
					//     retry is live right now on some OTHER goroutine
					//     (which owes this turn a finalize the exact same
					//     way), or because this turn saw genuine activity
					//     since finalizeByFallback's own preFetchActivity
					//     snapshot — it is, by construction, NOT silent right
					//     now, so this ticker's own NEXT iteration
					//     re-evaluates shouldFinalizeByFallback from scratch.
					//     That activity necessarily called ts.touch()
					//     (turn.go), resetting the idle clock, so
					//     shouldFinalizeByFallback will correctly return
					//     false until this turn has gone idle for a full
					//     fresh a.sseInactivityTimeout window again — at
					//     which point either the turn has genuinely
					//     completed by then (ts.done already closed, the
					//     next ctx.Done()/ts.done/ticker.C select picks that
					//     up directly) or this fallback fires again with a
					//     brand-new snapshot. No goroutine needs to be "the
					//     authority" for this case specifically: the poll
					//     loop itself, unchanged, remains sufficient,
					//     exactly as it already is for every ordinary tick
					//     where shouldFinalizeByFallback simply returns
					//     false.
					//
					// Returning here regardless (the OLD, unconditional
					// behavior) would let StartTurn return and its own
					// deferred unregisterTurn fire IMMEDIATELY, orphaning
					// whichever of the above actually applies: every
					// subsequent SSE event for this session (including a
					// retry's own eventual real completion) would then be
					// silently dropped by dispatchEvent's own resolveEvent
					// (sse.go, "no registered turn for this session"), and
					// this turn's own execution_complete would never reach
					// the sink at all. Keep polling instead — bounded, as
					// always, by ctx.Done() above if every other path
					// somehow never resolves.
				}
			}
		}
	}
}

// shouldFinalizeByFallback implements Finding 1/2's own liveness
// disambiguation: ts.idleFor(a.sseInactivityTimeout) alone (the ORIGINAL
// fallback trigger) cannot tell "this turn is genuinely stuck" apart from
// "the whole SSE connection dropped, and this turn just went silent as a
// side effect" — the latter might still resolve normally once runEventLoop
// reconnects (now happening every a.reconnectInterval, deliberately much
// shorter than a.sseInactivityTimeout so it has a real chance to win this
// race), letting the turn's own real session.idle/session.error arrive as
// usual.
//
// This asks "did a genuine disconnect happen at any point DURING this
// turn's own current idle episode" (a.disconnectedSince(ts.
// lastActivityTime())), NOT "does the stream look fresh RIGHT NOW" — a
// point-in-time snapshot a bare reconnect handshake flips back to "fresh"
// on the very next poll tick, without the turn itself having had any
// chance yet to receive its own completion event. Reconnecting only proves
// reconnection is now technically possible, never that THIS turn's own
// outcome is ready.
//
//   - Turn idle, no disconnect ever observed during this turn's own
//     silence (!a.disconnectedSince(...)): this turn alone stalled on a
//     continuously healthy connection — the fallback's own original reason
//     for existing, unchanged. Finalize now.
//   - Turn idle AND a genuine disconnect occurred at some point during
//     this turn's own current silence (a.disconnectedSince(...)): do NOT
//     finalize yet — keep polling, giving the turn's own real outcome a
//     real chance to arrive normally (whether the stream has already
//     reconnected or not). Bounded: once the turn has been silent for
//     fallbackReconnectGraceMultiplier times the normal threshold (a full
//     extra window beyond the point reconnection should have already
//     recovered, were it going to), finalize via the fallback anyway —
//     this wait must never be unbounded.
func (a *Adapter) shouldFinalizeByFallback(ts *turnState) bool {
	if ts.isCompacting() {
		// §7.2 Finding 1's own fix: mirrors dispatchEvent's own four
		// isCompacting-guarded cases (sse.go) — a genuine recovery retry
		// (Adapter.attemptCompactionRetry for a ContextOverflowError, or
		// Adapter.attemptTransientRetry for a transient APIError — this
		// Step's own retry, launched by finalizeOrRecoverFromOverflow below,
		// sharing this SAME guard rather than a parallel one) may be in
		// flight for this exact turn RIGHT NOW, bounded independently by
		// a.summarizeTimeout/a.requestTimeout/a.transientRetryBackoff via
		// forceCompaction/postPromptAsync's own doJSONTimeout wraps
		// (compact.go, session.go) — NOT by this fallback. If the retry's
		// own sub-turn goes silent on the SSE stream for longer than
		// a.sseInactivityTimeout before producing its own first event (e.g.
		// the underlying provider is slow to emit a first token — very
		// plausible for exactly the large-context/overloaded-provider cases
		// these two retries exist for), finalizeByFallback must NOT step in:
		// it would fetch the final-state snapshot, still see the ORIGINAL
		// error-shaped message, and — since ts.compactionAlreadyAttempted()
		// is already true the moment the attempt began — take the "already
		// attempted" branch and finalize this turn immediately, closing
		// ts.done and unregistering sessionID from a.turns WHILE the real
		// background retry goroutine is still running. That orphaned
		// goroutine's own eventual forceCompaction/postPromptAsync calls
		// would then re-dispatch the stale original prompt against a
		// session id a LATER, unrelated turn may already have re-registered
		// by then (registerTurn unconditionally overwrites the map entry),
		// corrupting that turn's own turnState with orphaned SSE traffic.
		// The retry goroutine itself (attemptCompactionRetry or
		// attemptTransientRetry, whichever this turn's own recovery kind
		// launched) is the sole authority for finalizing this turn while
		// compacting is true: it always eventually calls a.finalize on
		// every one of its own failure paths, and only clears compacting to
		// false once it is genuinely done (success: after re-dispatching
		// via postPromptAsync; see that method's own doc comment for why
		// even that clear is deliberately not any earlier) — at which point
		// normal fallback evaluation below resumes as usual.
		return false
	}
	if !ts.idleFor(a.sseInactivityTimeout) {
		return false
	}
	if a.disconnectedSince(ts.lastActivityTime()) {
		return ts.idleFor(fallbackReconnectGraceMultiplier * a.sseInactivityTimeout)
	}
	return true
}

// finalizeByFallback implements §7's own "final-state fetch fallback"
// quirk: the SSE stream went quiet for longer than sseInactivityTimeout
// without session.idle/session.error ever arriving, so this determines the
// final state directly via GET /session/{id}/message instead of hanging
// forever waiting for an event that may never come.
//
// Routes through finalizeOrRecoverFromOverflow (§7.2), not a direct
// a.finalize call, exactly like dispatchEvent's own "session.idle" case
// (sse.go) — a transient SSE hiccup right when the model overflows must
// not silently skip the whole compaction-retry recovery mechanism just
// because this fallback path, rather than the live event, happened to be
// what noticed the turn was done.
//
// preFetchActivity is snapshotted BELOW, BEFORE fetchFinalMessages' own
// real HTTP round trip (bounded by a.requestTimeout, client.go, with no
// lock held across it) — during which the live SSE session.idle dispatch
// (sse.go) can perfectly well observe this exact turn's first
// ContextOverflowError, win tryBeginCompactionRetry, and run a genuine
// compaction retry (Adapter.attemptCompactionRetry) partway, fully, or not
// at all, by the time the fetch above returns. finalizeOrRecoverFromOverflow
// is handed this snapshot and re-checks it for staleness (ts.isCompacting()
// now, or ANY activity recorded since preFetchActivity) INSIDE the exact
// SAME ts.mu critical section that decides whether to finalize directly or
// begin a retry (turnState.resolveOverflowAction, turn.go) — see that
// method's own doc comment for the TOCTOU this closes that a version of
// this staleness check evaluated here, in a separate/earlier critical
// section (as a prior round of this fix did), could not: a live SSE
// session.idle for the SAME first-time overflow winning tryBeginCompactionRetry
// strictly BETWEEN this caller's own staleness checks and its own eventual
// arrival at the check-and-set. There is no such gap any more — the
// staleness read and the retry claim are now the same atomic operation, so
// nothing can happen "in between" them at all, however long the CPU-bound
// work computing this call's own outcome/isOverflow arguments below (e.g.
// partsHaveOutput, deriveOutcome) takes.
//
// ts.compactionAlreadyAttempted() is deliberately never consulted on its
// own here — a LATER audit's own diagnosis: that flag (turnState, turn.go)
// is a ONE-WAY LATCH, set true the first time a retry is ever attempted and
// NEVER cleared again, including long after that retry has fully succeeded
// and the turn has moved on to processing the RETRIED prompt. Gating on it
// alone (a prior fix's own approach) is correct only for as long as that
// commitment is still live — it does not hold once the retry has long
// since finished, at which point the flag is permanently true for the rest
// of this turn's life while genuinely NOTHING is in flight and NOTHING owes
// this turn a finalize any more. resolveOverflowAction instead asks the
// right pair of questions together, atomically: "is a retry live right
// now, or did my own snapshot go stale since I took it" (in which case
// abandon) and, only once that reads clean, "was a retry already fully
// attempted" (in which case enrich-and-finalize) or "is this a genuine
// first-time overflow" (in which case claim the retry) — so a retry that
// is merely part of this turn's PAST, with nothing currently live and
// nothing having happened since this call's own snapshot, correctly still
// lets the fallback finalize.
//
//   - A retry begins during the fetch and is still in flight when
//     resolveOverflowAction runs: overflowActionStale. Abandon — the live
//     retry goroutine owns the outcome.
//   - A retry begins AND fully completes its dispatch during the fetch:
//     ts.compacting has flipped back to false, but the retry's own SSE
//     traffic necessarily touched ts during that window (ts.touch(),
//     turn.go, is called by dispatchEvent for every dispatched event, even
//     compaction-internal ones the isCompacting guards otherwise suppress
//     translating — §7.2's VERIFIED LIVE finding, compact.go) — so
//     ts.lastActivity is now after preFetchActivity: overflowActionStale.
//     Abandon — the entries are stale relative to the now-redispatched
//     prompt.
//   - No retry ever happened, or one happened long ago and has since fully
//     quiesced: neither guard fires — overflowActionFinalizeDirect,
//     overflowActionAlreadyAttempted, or overflowActionBeginRetry, exactly
//     as finalizeOrRecoverFromOverflow's own doc comment describes.
//
// waitForTurn (the sole caller) simply returns without having finalized ts
// itself here, exactly as it already does on the "tryBeginCompactionRetry
// succeeds" branch elsewhere — see its own doc comment for why every path
// that reaches here is still safe for waitForTurn's own poll loop to keep
// ticking on.
func (a *Adapter) finalizeByFallback(ctx context.Context, sessionID string, ts *turnState) {
	preFetchActivity := ts.lastActivityTime()

	entries, err := a.fetchFinalMessages(ctx, sessionID)

	if err != nil || len(entries) == 0 {
		reason := "opencode: SSE stream went inactive and the final-state fallback fetch also failed"
		a.finalizeOrRecoverFromOverflow(sessionID, ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeFailed, Reason: &reason}, nil, preFetchActivity)
		return
	}

	last := entries[len(entries)-1]
	hasText, hasToolCall := partsHaveOutput(last.Parts)
	outcome := deriveOutcome(last.Info.Error, hasText, hasToolCall)
	// §7.3: built here, not inside deriveOutcome itself, since only this
	// Adapter-receiver call site has runtimeVersion/sandboxID in scope —
	// see turnOutcome.Diagnostic's own doc comment (outcome.go) for why
	// deriveOutcome stays pure. Model comes from THIS SAME last message's
	// own engine-reported ModelID/ProviderID (modelDisplayFromInfo,
	// diagnostic.go) -- last.Info.Error and last.Info.ModelID/ProviderID
	// are read off the identical messageListEntry, so the model reported
	// here can never belong to some OTHER message than the one whose
	// error it is being attached to. Gated on outcome.Outcome == Failed --
	// mirrors dispatchEvent's own identical gate (sse.go) exactly, for the
	// identical reason: last.Info.Error can be a MessageAbortedError too
	// (the fallback's own final-message fetch observing a Stop that
	// landed before session.idle ever arrived), and deriveOutcome maps
	// that to Outcome: Cancelled -- a cancellation is not a provider
	// failure, and the wire schema's own diagnostic description says
	// "absent for every other outcome" (contracts/sandbox-ws/v1/
	// events.schema.json).
	if outcome.Outcome == sandboxws.ExecutionCompleteOutcomeFailed {
		outcome.Diagnostic = a.buildProviderFailureDiagnostic(last.Info.Error, modelDisplayFromInfo(last.Info))
	}
	a.finalizeOrRecoverFromOverflow(sessionID, ts, outcome, last.Info.Error, preFetchActivity)
}

// finalizeOrRecoverFromOverflow implements §7.2's own "one retry, inside
// the same StartTurn call" design point — the single decision point BOTH
// dispatchEvent's own "session.idle" case (sse.go, the live path) and
// finalizeByFallback above (the SSE-inactivity-fallback path) now route
// through instead of calling a.finalize directly, so a transient SSE
// hiccup right when the model overflows gets the exact same recovery
// treatment as the live session.idle path.
//
// WIDENED by this Step ("typed transient-error retry for the OpenCode
// adapter") to also cover a first-time transient APIError
// (isTransientAPIError, outcome.go) — deliberately reusing this SAME
// decision point and this SAME turnState guard (ts.compacting/
// ts.compactionAttempted, turn.go) rather than building a second, parallel
// one: both failure classes need the exact same three things — suppress
// in-flight-recovery SSE noise, claim an atomic at-most-once retry, and
// never leave a turn without its one execution_complete — and the name
// "isOverflow" passed to resolveOverflowAction below now really means "is
// this outcome eligible for EITHER of this turn's two recovery kinds",
// still gating the SAME one-way latch either kind shares (§7.2's own
// "at most one retry per turn" is now, unchanged in mechanism, "at most one
// RECOVERY ATTEMPT of either kind per turn").
//
// err is the SAME typed *openCodeTaggedError each caller already had in
// hand at the point it derived outcome (ts.errorForOutcome() for the live
// path, last.Info.Error for the fallback path, possibly nil for
// finalizeByFallback's own "fetch itself failed" case) — threaded through
// explicitly rather than re-derived here, and checked via its own typed
// Name/Data fields (isContextOverflowError/isTransientAPIError, outcome.go),
// never string-matched out of outcome.Reason (matching this package's own
// "typed discriminator, not string-matching" discipline, e.g. deriveOutcome
// itself).
//
// snapshotTime is forwarded, unexamined, straight into
// turnState.resolveOverflowAction (turn.go) — see that method's own doc
// comment for exactly what it means for each caller and, most importantly,
// for the TOCTOU a PRIOR version of this fix left open by evaluating
// staleness in a critical section separate from the one that actually
// claims the retry: dispatchEvent's own session.idle/session.error cases
// pass essentially "now" (their own outcome was derived synchronously, no
// gap); finalizeByFallback passes preFetchActivity, snapshotted before its
// own unlocked fetchFinalMessages HTTP round trip. The single call below
// to ts.resolveOverflowAction is now the ONLY place that reads
// isCompacting/lastActivity and claims tryBeginCompactionRetry's own
// check-and-set — one atomic ts.mu-guarded operation, not several — so
// nothing can ever change ts's state in between "checked fresh" and
// "claimed the retry" any more, regardless of how much work (an HTTP fetch,
// JSON parsing, deriving an outcome) either caller did beforehand.
//
//   - overflowActionStale (turn.go): either a compaction retry is live
//     right now, or activity was recorded for this turn after
//     snapshotTime — some OTHER goroutine already owns this turn's
//     outcome (or is in the middle of claiming it). Abandon entirely: no
//     finalize, no retry launch. The live SSE dispatch path's own prior
//     isCompacting() guard (checked just before this call, sse.go) makes
//     this branch effectively unreachable for that caller in practice;
//     finalizeByFallback is the caller this exists for.
//   - overflowActionFinalizeDirect: err is neither a ContextOverflowError
//     nor a transient APIError — finalize exactly as derived (unchanged
//     behavior).
//   - overflowActionAlreadyAttempted: err IS again a recoverable error (of
//     either kind), but a recovery retry was already fully attempted for
//     this turn (and resolveOverflowAction has just confirmed, atomically,
//     that nothing is live right now and nothing happened since
//     snapshotTime) — finalize, but with outcome.Reason enriched
//     (enrichReasonForRepeatedOverflow for a ContextOverflowError,
//     enrichReasonForRepeatedTransientFailure for a transient APIError —
//     outcome.go) so an operator reading the final reason string can tell a
//     recovery WAS attempted, never a silent double failure
//     indistinguishable from a first-time, never-retried failure (§7.2
//     point 3, extended to this Step's own failure class).
//   - overflowActionBeginRetry: a genuine first-time recoverable error, not
//     yet attempted, and resolveOverflowAction has ATOMICALLY marked
//     ts.compactionAttempted/ts.compacting true as part of the SAME check
//     (§7.2 Finding 2's own original "fold the check and the two writes
//     into one ts.mu-guarded step" fix, now additionally folded together
//     with the staleness check above) — this call launches the actual
//     recovery attempt on ITS OWN tracked background goroutine via
//     a.group.Go — this Adapter's own existing errgroup field, the same
//     "background work via the type's own errgroup field, never a bare go
//     statement" convention New's own persistent SSE loop already
//     establishes — rather than blocking dispatchEvent's own calling
//     goroutine (the persistent SSE reader) for however long the recovery
//     action takes. WHICH recovery action depends on err's own typed kind:
//     a ContextOverflowError launches attemptCompactionRetry (forces a
//     compaction first, §7.2); a transient APIError launches
//     attemptTransientRetry below instead (a bounded backoff wait, no
//     compaction at all — see that function's own doc comment for why).
//
// Uses a.bgCtx as the recovery attempt's own base context, NOT
// context.Background() and NOT either caller's own per-call ctx (dispatchEvent
// has none at all; finalizeByFallback's own ctx is StartTurn's — already
// gone, or about to be, by the time a human-scale compaction actually
// finishes, on the exact fallback path that fires specifically when that
// ctx's own stream looked stuck): a.bgCtx is this Adapter's own lifetime
// context, canceled by Close (bgCancel) — reusing it here means Close can
// never block indefinitely on an in-flight recovery attempt that outlives
// the adapter's own intended shutdown window, since canceling bgCtx
// immediately unblocks whatever doJSONTimeout call attemptCompactionRetry
// is currently waiting on, bounding Close's own group.Wait() by however
// long that one HTTP call takes to notice ctx.Done(). Absent a Close call,
// the recovery attempt is still bounded on its own: doJSONTimeout wraps
// a.bgCtx (which carries no deadline of its own) in its own
// context.WithTimeout(summarizeTimeout) / context.WithTimeout(requestTimeout)
// per call, so the whole attempt is bounded by summarizeTimeout+requestTimeout
// even if Close is never called at all.
func (a *Adapter) finalizeOrRecoverFromOverflow(sessionID string, ts *turnState, outcome turnOutcome, err *openCodeTaggedError, snapshotTime time.Time) {
	kind := recoveryKindNone
	switch {
	case isTransientAPIError(err):
		kind = recoveryKindTransientAPI
	case isContextOverflowError(err):
		kind = recoveryKindCompaction
	}

	switch ts.resolveOverflowAction(snapshotTime, kind) {
	case overflowActionStale:
		slog.Warn("opencode: this turn's own state changed since this outcome was derived (a recovery retry "+
			"is live right now, or activity was recorded since this call's own snapshot) -- abandoning, "+
			"another goroutine already owns this turn's outcome",
			"sessionID", sessionID)
		return

	case overflowActionFinalizeDirect:
		a.finalize(ts, outcome)

	case overflowActionAlreadyAttempted:
		// A recovery retry was already fully attempted for this turn, and
		// resolveOverflowAction has just confirmed atomically that nothing
		// is live right now and nothing happened since snapshotTime. This
		// call must NOT launch a second attempt: enrich and finalize
		// instead. Deliberately branches on ts.attemptedRecoveryKind() --
		// WHICH kind this turn's one shot was ACTUALLY spent on -- never on
		// this CURRENT failure's own kind (`kind` above): a turn whose
		// first-time ContextOverflowError already consumed the shared latch,
		// whose retried prompt then hits a transient APIError instead
		// (`kind` would read recoveryKindTransientAPI here), must still be
		// described as a compaction retry having already been attempted --
		// that IS what actually happened this turn.
		var reason string
		if ts.attemptedRecoveryKind() == recoveryKindTransientAPI {
			reason = enrichReasonForRepeatedTransientFailure(outcome.Reason)
		} else {
			reason = enrichReasonForRepeatedOverflow(outcome.Reason)
		}
		slog.Warn("opencode: retried prompt also failed, finalizing as failed",
			"sessionID", sessionID, "reason", reason)
		// Diagnostic carried forward from outcome (§7.3) --
		// outcome is THIS session.idle's own freshly-derived outcome (the
		// retried prompt's own failure, not the original one), so its own
		// Diagnostic already describes whichever error just fired. Only
		// Reason is rebuilt here (to note a retry already happened);
		// Diagnostic must survive unchanged, per turnOutcome.Diagnostic's
		// own doc comment (outcome.go).
		a.finalize(ts, turnOutcome{Outcome: outcome.Outcome, Reason: &reason, Diagnostic: outcome.Diagnostic})

	case overflowActionBeginRetry:
		if kind == recoveryKindTransientAPI {
			slog.Warn("opencode: transient provider error detected, attempting retry", "sessionID", sessionID)
			a.group.Go(func() error {
				a.attemptTransientRetry(a.bgCtx, sessionID, ts, outcome)
				return nil
			})
			return
		}
		slog.Warn("opencode: context overflow detected, attempting compaction retry", "sessionID", sessionID)
		a.group.Go(func() error {
			a.attemptCompactionRetry(a.bgCtx, sessionID, ts, outcome)
			return nil
		})
	}
}

// attemptCompactionRetry implements the actual recovery work
// finalizeOrRecoverFromOverflow above launches on its own tracked
// background goroutine: force a compaction (forceCompaction, compact.go)
// using the SAME resolveModelForced-resolved model for both that call and
// the retried prompt, then re-dispatch ts.cmd exactly once.
//
// ts.stillLive() is consulted TWICE, not once — a LATER audit's own
// round-2 Finding 1: the original fix only ever read it right after
// forceCompaction returned, which proves the turn was live at THAT
// instant but is never re-checked between then and the actual
// a.postPromptAsync re-dispatch call below — including across that
// call's own full HTTP round trip. Since stopRequested/finalized
// (turn.go) are both one-way latches that, once set, never reset, a
// SECOND stillLive() read taken right after postPromptAsync returns is
// guaranteed to observe any Stop (or independent finalize) that landed
// at any point up to and including during that call, fully closing the
// window a single point-in-time check left open. See the second check's
// own inline comment below for what happens when it fires.
//
//   - forceCompaction returns (success OR failure), but the FIRST
//     ts.stillLive() check is now false: a Stop landed for this exact turn
//     (turnState.stopRequested, set by Adapter.Stop), or the turn was
//     ALREADY finalized by some other path entirely (ts.finalized — e.g.
//     StartTurn's own ctx being canceled independently of a.bgCtx, which
//     this call runs on), at some point during the compaction window.
//     Neither signal can ever reach here any other way: every
//     dispatchEvent case that would otherwise observe an OpenCode-side
//     abort/error for this session is guarded by isCompacting and silently
//     drops it for as long as ts.compacting stays true (sse.go) — see
//     stillLive's own doc comment (turn.go) for the full write-up. Never
//     re-dispatch (nor even bother enriching a ContextOverflowError reason
//     with a forceCompaction failure detail nobody asked about anymore):
//     finalize as Cancelled instead, using a.finalize's own tryFinalize
//     idempotency to make this a safe no-op on the "already finalized"
//     branch (ts.finalized was already true) and a genuine, otherwise-
//     never-emitted finalize on the "Stop only" branch (nothing else was
//     ever going to close this turn out, since the abort's own SSE
//     signal was swallowed).
//   - forceCompaction fails (and the turn is still live): the compaction
//     attempt itself did not complete — finalize using the ORIGINAL
//     overflow outcome, with the reason enriched
//     (enrichReasonForFailedRecovery, outcome.go) to note the compaction
//     attempt failed. ts.compacting is reset to false first so a stray
//     late-arriving event for this session (there shouldn't be one — the
//     /summarize call itself failed — but nothing here assumes that) is
//     not silently swallowed by a guard that no longer serves a purpose.
//   - forceCompaction succeeds (and the turn is still live per the FIRST
//     check): clear ts's stored errors (ts.clearErrorsForRetry —
//     CRITICAL, see that method's own doc comment for why: without it,
//     the retry's own eventual session.idle would still see the STALE
//     original ContextOverflowError and incorrectly finalize as failed
//     even though the retry actually succeeded), re-dispatch the SAME
//     prompt via postPromptAsync, THEN re-check ts.stillLive() a SECOND
//     time (the round-2 Finding 1 fix):
//   - stillLive() is now false: a Stop landed anywhere during
//     postPromptAsync's own round trip. If that call itself succeeded
//     (err == nil), the retried prompt WAS accepted by OpenCode despite
//     the Stop — explicitly a.postAbort it rather than silently let the
//     cancelled prompt run to completion; a failed postPromptAsync needs
//     no such abort (OpenCode never accepted it). Either way, finalize as
//     Cancelled — a.finalize's own tryFinalize (turn.go) atomically clears
//     ts.compacting in the SAME critical section that commits
//     ts.finalized, so this goroutine's own tryFinalize claim cannot lose
//     to a stray compaction-tail (or retry-tail) event isCompacting would
//     otherwise still be suppressing, mirroring the FIRST stillLive
//     branch's own reasoning above exactly.
//   - stillLive() is still true: if postPromptAsync itself failed, finalize
//     directly with an enriched reason WITHOUT first clearing compacting —
//     a.finalize's own tryFinalize (turn.go) atomically clears it in the
//     SAME critical section that commits ts.finalized, exactly mirroring
//     both stillLive()-false branches above (see the err != nil branch's
//     own inline comment below for the concrete race this avoids
//     reopening). Only on the OTHER path — postPromptAsync succeeded — is
//     compacting cleared explicitly here, since nothing else will: no
//     finalize call happens on this goroutine at all in that case, and the
//     retry's own real SSE traffic must be let through the guard from this
//     point on.
//   - That re-dispatch succeeds and no Stop landed: nothing further to do
//     here — the retry's own eventual session.idle (or session.error, if
//     it also overflows) arrives on the SSE goroutine exactly like any
//     normal turn's completion and routes back through
//     finalizeOrRecoverFromOverflow, this time with
//     ts.compactionAlreadyAttempted() true, so it correctly falls through
//     to the enrich-and-finalize branch there rather than looping.
//
// §7.2 Finding 3: ts.setCompacting(false) is deliberately called AFTER
// postPromptAsync returns on the success path below, NOT before dispatching
// it (the original ordering). forceCompaction's own doc comment (compact.go)
// documents that a real POST /summarize call's HTTP response only returns
// once OpenCode has ALREADY streamed the full wave of compaction-internal
// SSE traffic — but that is an empirically-observed timing property across
// TWO INDEPENDENT connections/goroutines (the POST /summarize response read
// here vs. the persistent GET /event stream read by the separate SSE-reader
// goroutine, sse.go), which Go's own runtime gives no happens-before
// guarantee for. Clearing compacting BEFORE dispatching the retry left a
// window — however small in practice — in which a still-in-flight
// compaction-tail session.idle/message.updated event, processed late by a
// scheduled-behind SSE-reader goroutine (a GC pause, a buffered partial read
// still pending, ordinary scheduler unfairness), could sail straight through
// the now-false guard and get evaluated using this turn's STALE
// pre-overflow ts.sawText/ts.sawToolCall (never reset by clearErrorsForRetry
// — deliberately, see its own doc comment: a genuine retry's own new output
// should ADD to, not replace, whatever real output the turn already
// produced before overflowing) with no tracked error (just cleared) —
// deriveOutcome would then return "completed" and finalizeOrRecoverFromOverflow
// would finalize the WHOLE turn immediately, BEFORE the retried prompt was
// even dispatched, silently discarding the actual retry and reporting a
// spurious success. Keeping compacting true through postPromptAsync's own
// full round trip means every event that could possibly still belong to the
// compaction's own internal wave remains suppressed for that entire
// additional window, closing the specific race this finding describes:
// legitimate SSE traffic for the RETRIED prompt cannot arrive before
// postPromptAsync's own POST is even accepted by OpenCode, so there is no
// legitimate event this ordering could ever wrongly suppress. The FIRST
// stillLive check (right after forceCompaction returns, above) is placed
// BEFORE this ordering even comes into play (it can only ever short-circuit
// before clearErrorsForRetry/postPromptAsync are reached at all), so it
// cannot reopen or narrow the window Finding 3 already closed. The SECOND
// stillLive check (round-2 Finding 1, right after postPromptAsync returns,
// below) is placed strictly AFTER that call, for the same reason in
// reverse: ts.compacting is still true at that point too (setCompacting(false)
// has not yet run), so a Stop discovered there still enjoys the exact same
// "every dispatchEvent case stays suppressed" protection Finding 3 relies
// on, right up until this goroutine's own finalize call claims tryFinalize.
func (a *Adapter) attemptCompactionRetry(ctx context.Context, sessionID string, ts *turnState, originalOutcome turnOutcome) {
	model := a.resolveModelForced(ctx, (*string)(ts.cmd.Model))

	compactionErr := a.forceCompaction(ctx, sessionID, model)

	if !ts.stillLive() {
		// No separate ts.setCompacting(false) call here (a CI-only
		// regression fix: see tryFinalize's own doc comment, turn.go, for
		// the race this used to leave open when that call was a distinct,
		// later statement on this goroutine) — a.finalize's own tryFinalize
		// atomically clears ts.compacting in the SAME critical section that
		// commits ts.finalized, guarding against the hazard this exact
		// branch introduces: forceCompaction SUCCEEDING (compactionErr ==
		// nil, the common case a Stop lands during) means the fake/real
		// compaction wave's own tail traffic — crucially including its own
		// session.idle, on the SAME two independent connections/goroutines
		// Finding 3's own doc comment describes — may not have been fully
		// processed by the SSE-reader goroutine yet. Were ts.compacting to
		// read false to some OTHER goroutine before THIS goroutine's own
		// finalize call has actually claimed tryFinalize, a stray
		// session.idle could sail through dispatchEvent's own isCompacting
		// guard (sse.go), observe the STILL-uncleared original
		// ContextOverflowError (clearErrorsForRetry is deliberately never
		// reached on this abort path), and race this goroutine's own
		// finalize call via finalizeOrRecoverFromOverflow's "already
		// attempted" branch — a race a.finalize's own tryFinalize
		// idempotency resolves, but not necessarily in THIS goroutine's
		// favor, wrongly reporting Failed instead of Cancelled if the stray
		// event won. tryFinalize's own atomic clear closes that window to
		// zero width.
		reason := "opencode: turn was stopped or already finalized during compaction, retry aborted"
		slog.Warn("opencode: turn no longer live after forceCompaction, aborting compaction retry without re-dispatching",
			"sessionID", sessionID, "forceCompactionErr", compactionErr)
		a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeCancelled, Reason: &reason})
		return
	}

	if compactionErr != nil {
		ts.setCompacting(false)
		reason := enrichReasonForFailedRecovery(originalOutcome.Reason, fmt.Sprintf("forceCompaction: %v", compactionErr))
		slog.Error("opencode: compaction attempt failed, finalizing with the original overflow error",
			"sessionID", sessionID, "error", compactionErr)
		// Diagnostic carried forward from originalOutcome (§7.3): clearErrorsForRetry was never reached on this branch
		// (forceCompaction itself failed, before the retry prompt was
		// ever dispatched), so ts's own current error state still holds
		// the ORIGINAL overflow -- but only originalOutcome, captured
		// BEFORE this attempt began, carries that error's own diagnostic.
		// Rebuilding turnOutcome{} here without it would silently drop
		// the one record §7.3 requires at exactly the moment ("retries
		// exhausted") it matters most.
		a.finalize(ts, turnOutcome{Outcome: originalOutcome.Outcome, Reason: &reason, Diagnostic: originalOutcome.Diagnostic})
		return
	}

	slog.Warn("opencode: compaction succeeded, retrying the original prompt", "sessionID", sessionID)
	ts.clearErrorsForRetry()

	// setCompacting(false) intentionally happens AFTER postPromptAsync
	// returns, not before it is called — see this method's own doc comment
	// for why (§7.2 Finding 3).
	err := a.postPromptAsync(ctx, sessionID, ts.cmd, model)

	if !ts.stillLive() {
		// Round-2 Finding 1: re-check stillLive() HERE, after postPromptAsync
		// has actually returned, not only before it was called (the FIRST
		// check above, right after forceCompaction). stopRequested/finalized
		// (turn.go) are both one-way latches, so this is guaranteed to
		// observe any Stop (or independent finalize) that landed at any
		// point up to and including during postPromptAsync's own HTTP round
		// trip -- exactly the window the single, earlier check alone left
		// completely unobserved.
		//
		// If postPromptAsync itself succeeded (err == nil), the retried
		// prompt WAS accepted by OpenCode despite the Stop -- explicitly
		// abort it via a.postAbort rather than silently let the cancelled
		// prompt run to completion having overridden the user's own Stop. A
		// failed postPromptAsync needs no such abort: OpenCode never
		// accepted that call in the first place. Either way, no separate
		// ts.setCompacting(false) call here -- a.finalize's own tryFinalize
		// (turn.go) atomically clears ts.compacting in the SAME critical
		// section that commits ts.finalized, mirroring the FIRST stillLive
		// branch's own reasoning above exactly: this goroutine's own
		// Cancelled outcome claims tryFinalize before any stray
		// compaction-tail (or now, retry-tail) event that isCompacting
		// would otherwise still be suppressing can possibly observe
		// compacting read false.
		reason := "opencode: turn was stopped while the retry prompt was being (re-)dispatched, aborting"
		slog.Warn("opencode: turn no longer live after the retry's own postPromptAsync call returned, aborting",
			"sessionID", sessionID, "postPromptAsyncErr", err)
		if err == nil {
			if abortErr := a.postAbort(ctx, sessionID); abortErr != nil {
				slog.Warn("opencode: failed to abort the re-dispatched retry prompt after a late stop",
					"sessionID", sessionID, "error", abortErr)
			}
		}
		a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeCancelled, Reason: &reason})
		return
	}

	if err != nil {
		// No separate ts.setCompacting(false) call here — mirrors BOTH
		// stillLive()-false branches above (and this exact pattern in
		// attemptTransientRetry below): a.finalize's own tryFinalize
		// (turn.go) atomically clears ts.compacting in the SAME critical
		// section that commits ts.finalized, so a stray compaction-tail
		// event dispatched on the SSE-reader goroutine (isCompacting still
		// suppressing it right up until tryFinalize's own atomic commit)
		// can never win a race this goroutine's own finalize call should
		// win instead.
		//
		// A CI-only regression this branch used to reintroduce: an EARLIER
		// version of this function called ts.setCompacting(false)
		// UNCONDITIONALLY, before this err check, on the (mistaken)
		// assumption that "after postPromptAsync returns" was the only
		// ordering constraint §7.2 Finding 3 cared about. That reopened
		// exactly the window tryFinalize's own doc comment (turn.go)
		// describes closing: between that unconditional clear and this
		// a.finalize call below, ts.compacting read false to every OTHER
		// goroutine while ts.finalized was still false — wide enough for
		// broadcastCompactionSuccessWave's own tail session.idle
		// (fake_server_test.go, queued onto a buffered channel by the
		// /summarize handler and drained independently by the SSE-reader
		// goroutine, with no happens-before relationship to forceCompaction's
		// own HTTP response returning — see that comment's own "an in-process
		// fake server's forceCompaction+postPromptAsync round trip can
		// complete... before the SSE-reader goroutine has drained every
		// event this handler just queued" disclaimer) to sail through
		// dispatchEvent's session.idle case's now-false isCompacting guard
		// (sse.go), observe ts.errorForOutcome()==nil (clearErrorsForRetry
		// already ran) and hasText/hasToolCall==false,false (nothing this
		// turn ever produced), derive "opencode: turn produced no output",
		// and win tryFinalize against THIS goroutine's own, correctly
		// enriched, finalize call below — reproduced live as
		// TestCompactionRetry_RetryPostPromptAsyncFails. This exact hazard
		// is not a fake-server-only artifact either: forceCompaction's own
		// doc comment (compact.go) already documents that a REAL OpenCode
		// server's "the /summarize response only returns after the full
		// compaction-internal SSE wave has already streamed" ordering is
		// merely empirically observed, across two independent connections/
		// goroutines Go's memory model gives no happens-before guarantee
		// for — so the identical race is genuinely reachable in production,
		// not just under a test harness's own buffered-channel scheduling.
		reason := enrichReasonForFailedRecovery(originalOutcome.Reason, fmt.Sprintf("retry postPromptAsync: %v", err))
		slog.Error("opencode: retry prompt dispatch failed, finalizing with the original overflow error",
			"sessionID", sessionID, "error", err)
		// Diagnostic carried forward from originalOutcome (§7.3) -- see attemptCompactionRetry's own identical
		// compactionErr-branch comment above for why originalOutcome,
		// not ts's own current (cleared) error state, is the right
		// source here too.
		a.finalize(ts, turnOutcome{Outcome: originalOutcome.Outcome, Reason: &reason, Diagnostic: originalOutcome.Diagnostic})
		return
	}

	ts.setCompacting(false)
}

// attemptTransientRetry implements the actual recovery work
// finalizeOrRecoverFromOverflow above launches on its own tracked
// background goroutine for this Step's own transient-APIError retry ("typed
// transient-error retry for the OpenCode adapter"): wait a bounded backoff
// (a.transientRetryBackoff), then re-dispatch ts.cmd exactly once —
// deliberately mirroring attemptCompactionRetry's own shape (the same
// stillLive()-guarded, ts.compacting-suppressed structure, reusing the SAME
// shared latch — see finalizeOrRecoverFromOverflow's own doc comment for
// why this is the SAME machinery, not a parallel one) but WITHOUT a
// forceCompaction step: a transient provider blip (429/529-shaped, per
// OpenCode's own isRetryable verdict, isTransientAPIError outcome.go) needs
// no compaction at all, only a short pause before trying the exact same
// prompt again.
//
// ts.stillLive() is consulted TWICE, exactly mirroring
// attemptCompactionRetry's own round-2 Finding 1 fix (see that function's
// own doc comment for the full race this closes) — substituting "the
// backoff wait" for "forceCompaction" as the first bounded operation this
// goroutine performs before re-dispatching:
//
//   - The backoff wait itself is interrupted by ctx.Done() (a.bgCtx,
//     canceled only by Adapter.Close): treated exactly like
//     forceCompaction/postPromptAsync failing below — finalize with the
//     ORIGINAL outcome, reason enriched via
//     enrichReasonForFailedTransientRetry, rather than silently leaving
//     this turn without its one execution_complete.
//   - The backoff elapses, but the FIRST ts.stillLive() check is now
//     false: a Stop landed for this exact turn, or the turn was ALREADY
//     finalized by some other path — finalize as Cancelled; no separate
//     ts.setCompacting(false) call, mirroring attemptCompactionRetry's own
//     identical reasoning exactly (a.finalize's own tryFinalize, turn.go,
//     atomically clears ts.compacting in the SAME critical section that
//     commits ts.finalized, so every dispatchEvent case that would
//     otherwise observe a stray event for this session stays suppressed by
//     isCompacting right up until this goroutine's own tryFinalize claim —
//     closing the exact same race attemptCompactionRetry's own first
//     stillLive branch closes).
//   - The backoff elapses and the turn is still live: clear ts's stored
//     errors (ts.clearErrorsForRetry — CRITICAL, identical reasoning to
//     attemptCompactionRetry's own use of it: without it, the retry's own
//     eventual session.idle would still see the STALE original APIError
//     and incorrectly finalize as failed even though the retry actually
//     succeeded), re-dispatch the SAME prompt via postPromptAsync using
//     resolveModel — NOT resolveModelForced — unlike forceCompaction's own
//     /summarize call (which has no "omit and let OpenCode pick" option at
//     all, session.go), postPromptAsync's own "model" field is genuinely
//     optional, exactly as the ORIGINAL dispatch in StartTurn already
//     treats it; forcing a resolved model here would silently change this
//     retry's own request shape relative to the attempt it is retrying.
//     THEN re-check ts.stillLive() a SECOND time — identical to
//     attemptCompactionRetry's own round-2 check, and ts.compacting is
//     likewise deliberately still true up to and through that second check
//     (only cleared afterward, on the still-live success path below, or
//     atomically by a.finalize's own tryFinalize on the abort path), for
//     the exact same §7.2 Finding 3 reason: closing the window between the
//     retried dispatch being accepted and this goroutine's own tryFinalize
//     claim, during which a stray original-attempt SSE event could
//     otherwise slip through.
func (a *Adapter) attemptTransientRetry(ctx context.Context, sessionID string, ts *turnState, originalOutcome turnOutcome) {
	if waitErr := waitTransientRetryBackoff(ctx, a.transientRetryBackoff); waitErr != nil {
		// No separate ts.setCompacting(false) call -- a.finalize's own
		// tryFinalize (turn.go) atomically clears it. See tryFinalize's own
		// doc comment for why a distinct, later ts.setCompacting(false) call
		// on this goroutine is not safe to rely on.
		reason := enrichReasonForFailedTransientRetry(originalOutcome.Reason, fmt.Sprintf("backoff wait: %v", waitErr))
		slog.Error("opencode: transient-error retry backoff wait was interrupted, finalizing with the original error",
			"sessionID", sessionID, "error", waitErr)
		// Diagnostic carried forward from originalOutcome (§7.3) -- see attemptCompactionRetry's own identical
		// compactionErr-branch comment (above, this file) for why the
		// PRE-attempt outcome, not ts's own current error state, is the
		// right source: no retry was ever dispatched on this branch
		// either (the backoff wait itself was interrupted).
		a.finalize(ts, turnOutcome{Outcome: originalOutcome.Outcome, Reason: &reason, Diagnostic: originalOutcome.Diagnostic})
		return
	}

	if !ts.stillLive() {
		// Mirrors attemptCompactionRetry's own first stillLive branch
		// exactly (adapter.go) — no separate ts.setCompacting(false) call;
		// a.finalize's own tryFinalize (turn.go) atomically clears it, for
		// the identical reason given there.
		reason := "opencode: turn was stopped or already finalized during the transient-error retry backoff, retry aborted"
		slog.Warn("opencode: turn no longer live after the transient-error backoff wait, aborting retry without re-dispatching",
			"sessionID", sessionID)
		a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeCancelled, Reason: &reason})
		return
	}

	slog.Warn("opencode: transient-error backoff elapsed, retrying the original prompt", "sessionID", sessionID)
	ts.clearErrorsForRetry()

	model := a.resolveModel(ctx, (*string)(ts.cmd.Model))

	// setCompacting(false) intentionally happens AFTER postPromptAsync
	// returns, not before it is called — mirrors attemptCompactionRetry's
	// own §7.2 Finding 3 ordering exactly, for the identical reason (see
	// that function's own doc comment).
	err := a.postPromptAsync(ctx, sessionID, ts.cmd, model)

	if !ts.stillLive() {
		// Round-2 Finding 1, mirrored exactly: re-check stillLive() HERE,
		// after postPromptAsync has actually returned — see
		// attemptCompactionRetry's own identical branch for the full race
		// this closes. No separate ts.setCompacting(false) call here either
		// -- a.finalize's own tryFinalize (turn.go) atomically clears it.
		reason := "opencode: turn was stopped while the transient-error retry prompt was being (re-)dispatched, aborting"
		slog.Warn("opencode: turn no longer live after the retry's own postPromptAsync call returned, aborting",
			"sessionID", sessionID, "postPromptAsyncErr", err)
		if err == nil {
			if abortErr := a.postAbort(ctx, sessionID); abortErr != nil {
				slog.Warn("opencode: failed to abort the re-dispatched retry prompt after a late stop",
					"sessionID", sessionID, "error", abortErr)
			}
		}
		a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeCancelled, Reason: &reason})
		return
	}

	if err != nil {
		// No separate ts.setCompacting(false) call here — mirrors
		// attemptCompactionRetry's own identical fix to its own identical
		// bug, exactly (see that function's own doc comment on its matching
		// branch for the full race this closes): a.finalize's own
		// tryFinalize (turn.go) atomically clears ts.compacting in the SAME
		// critical section that commits ts.finalized, so a stray event for
		// this session dispatched on the SSE-reader goroutine while this
		// goroutine is still between "backoff elapsed, dispatch attempted"
		// and "finalize claimed" cannot win a race this goroutine's own
		// finalize call should win instead. This branch used to call
		// ts.setCompacting(false) unconditionally before this check, which
		// — exactly like attemptCompactionRetry's own now-fixed sibling
		// bug — reopened that window for however long it took this
		// goroutine to build the enriched reason string and reach
		// a.finalize below.
		reason := enrichReasonForFailedTransientRetry(originalOutcome.Reason, fmt.Sprintf("retry postPromptAsync: %v", err))
		slog.Error("opencode: retry prompt dispatch failed, finalizing with the original transient error",
			"sessionID", sessionID, "error", err)
		// Diagnostic carried forward from originalOutcome (§7.3) -- see attemptCompactionRetry's own identical
		// compactionErr-branch comment (above, this file) for why the
		// PRE-attempt outcome, not ts's own current (cleared) error
		// state, is the right source here too.
		a.finalize(ts, turnOutcome{Outcome: originalOutcome.Outcome, Reason: &reason, Diagnostic: originalOutcome.Diagnostic})
		return
	}

	ts.setCompacting(false)
}

// waitTransientRetryBackoff waits d, honoring ctx cancellation (a.bgCtx,
// canceled only by Adapter.Close) — kept as its own tiny helper so
// attemptTransientRetry's own doc comment can describe the backoff wait as
// a single bounded operation, exactly like forceCompaction/postPromptAsync
// already are from attemptCompactionRetry's own perspective.
func waitTransientRetryBackoff(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finalize is the single path that ever emits a turn's own terminal
// events: guarded by ts.tryFinalize so a stray duplicate session.idle (or
// one arriving after the inactivity fallback already ran) can never
// double-emit. Closes out every still-open sub-task (§7.1, see this
// package's own doc comment on Adapter for the full sub-task-fidelity
// writeup) before the turn's own execution_complete, unregistering each
// one's own child-session routing entry too (a subtask closed via the
// real per-sub-task completion signal, maybeFinishTaskSubtask, already
// unregistered and removed itself before ever reaching here — see
// markSubtaskFinished's own doc comment, turn.go — so this loop only ever
// runs for a sub-task whose own completion was never observed, e.g. the
// turn was cancelled while its task tool call was still running), then
// closes ts.done so waitForTurn's own poll loop returns.
//
// Uses ts.emitFinal, NOT ts.emit, for both kinds of terminal emission
// below (Finding 4): tryFinalize above has already set ts.finalized=true
// under ts.mu, and emit's own finalized check (turn.go) — reading that SAME
// flag under that SAME lock — would unconditionally drop anything finalize
// itself tries to emit here. emitFinal is the one path explicitly entitled
// to emit despite ts.finalized already being true, since finalize is the
// only caller of it.
func (a *Adapter) finalize(ts *turnState, outcome turnOutcome) {
	if !ts.tryFinalize() {
		return
	}

	for _, subTaskID := range ts.drainOpenSubtasks() {
		a.unregisterSubtaskSession(subTaskID)
		ts.emitFinal(translateSubTaskFinish(ts.cmd, subTaskID, outcome.Outcome))
	}

	ts.emitFinal(translateExecutionComplete(ts.cmd, outcome, ""))

	close(ts.done)
}

// Stop implements ports.AgentRuntime — aborts whatever OpenCode session is
// currently "current" for this adapter (there is exactly one live
// conversation per sandbox-agent process at a time, §3.3: "Exactly one
// processing per session"); cmd itself carries no OpenCode session
// reference of its own (commands.schema.json's own Stop has no
// conversationId field). A Stop before any session has ever been created
// is a safe local no-op (no HTTP call at all, so it can never fail).
//
// Marks the currently-registered turnState (if any) via markStopRequested
// BEFORE posting the abort — a LATER audit's own Finding 1: this is the
// ONLY place a Stop is ever recorded on turnState at all. Without it, a
// Stop landing while a compaction retry is in flight (ts.isCompacting())
// would be entirely invisible to attemptCompactionRetry: the abort's own
// resulting OpenCode-side session.idle/session.error is exactly what
// dispatchEvent's own isCompacting guards (sse.go) silently swallow during
// that window, so nothing else would ever learn the turn was cancelled.
// Safe to call even if lookupTurn finds nothing (no turn currently
// registered for this session — a plain no-op, same as before this fix)
// or if a compaction retry never happens at all (stopRequested is only
// ever consulted by attemptCompactionRetry's own stillLive check,
// adapter.go/turn.go; every other Stop-handling path is unchanged).
func (a *Adapter) Stop(ctx context.Context, _ sandboxws.Stop) error {
	sessionID := a.getCurrentSession()
	if sessionID == "" {
		return nil
	}
	if ts := a.lookupTurn(sessionID); ts != nil {
		ts.markStopRequested()
	}
	return a.postAbort(ctx, sessionID)
}

// CurrentTurnSpentUSD returns the running cost total (turnState.spentUSD,
// turn.go's own addCost/spentUSDTotal) for whichever turn is currently
// live on this adapter -- §26.7/§7.1's own accumulator, finally given a
// real reader (§26.5). Mirrors Stop's own getCurrentSession/lookupTurn
// precedent immediately above exactly, for the identical reason: a
// review sub-agent's own budget-check call (cmd/sandbox-agent's loopback
// review-cost-budget HTTP server) has no OpenCode session id of its own
// to key on -- it is asking on behalf of the ONE live turn this whole
// sandbox-agent process is running (§3.3: "Exactly one processing per
// session"), never a specific session it names itself.
//
// ok is false when no turn is currently registered at all (no live
// session yet, or the turn already finalized between the HTTP request's
// own arrival and this call) -- the caller treats that identically to
// "nothing spent yet" (spentUSD 0), never as an error: a budget check
// racing the very end of a turn has nothing left to gate anyway.
func (a *Adapter) CurrentTurnSpentUSD() (spentUSD float64, ok bool) {
	sessionID := a.getCurrentSession()
	if sessionID == "" {
		return 0, false
	}
	ts := a.lookupTurn(sessionID)
	if ts == nil {
		return 0, false
	}
	return ts.spentUSDTotal(), true
}

// partsHaveOutput scans a message's own parts (as returned by
// GET /session/{id}/message) for a non-empty text part or any tool part —
// the same "genuinely non-empty output" signal outcomeInputs derives
// incrementally from the live SSE stream, computed instead from the
// final-state fallback's own already-complete snapshot.
func partsHaveOutput(parts []json.RawMessage) (hasText, hasToolCall bool) {
	for _, raw := range parts {
		var env partEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		switch env.Type {
		case "text":
			var p textPart
			if err := json.Unmarshal(raw, &p); err == nil && p.Text != "" {
				hasText = true
			}
		case "tool":
			hasToolCall = true
		}
	}
	return hasText, hasToolCall
}
