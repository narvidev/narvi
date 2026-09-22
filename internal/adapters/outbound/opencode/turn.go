package opencode

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/app/ports"
)

// turnState is the demux state for one in-flight StartTurn call, keyed by
// its OpenCode sessionID in Adapter.turns (turn.go's own registry, see
// adapter.go). Exactly one turnState is ever registered per OpenCode
// session at a time (Narvi's own turn state machine, §3.3, guarantees
// "exactly one processing per session"), but nothing here assumes that
// beyond what the registry itself enforces (one map entry per key).
type turnState struct {
	cmd  sandboxws.Prompt // for stamping SessionId/Gen on every translated event (see translate.go)
	sink ports.EventSink

	// model is this turn's own "providerID/modelID" display string
	// (modelDisplay, below) — audit fix (§7.3, "the model is empty in the
	// default configuration"): NEVER "" for a real turn now that StartTurn
	// resolves cmd.Model via resolveModelForced (session.go, adapter.go),
	// which always returns a concrete *promptModelRef -- falling back to
	// fallbackModelRef() rather than omitting the field when cmd.Model was
	// nil (Composer/PlanModeView/Timeline resume/DecisionInbox all
	// dispatch this way by default, web/src/session). Set exactly once,
	// by newTurnState, from a value already resolved BEFORE this
	// turnState is ever constructed (StartTurn, adapter.go) — so, exactly
	// like cmd above, it is safe to read from any goroutine (the SSE
	// dispatch goroutine, via buildProviderFailureDiagnostic, diagnostic.go)
	// with no lock: it is immutable for this turnState's entire life, and
	// registerTurn (the first point another goroutine can even observe
	// this turnState at all) only ever runs after both cmd and model are
	// already set.
	model string

	mu sync.Mutex

	// toolCallSent/toolResultSent implement §7's own "dedupe tool states
	// by sid:callID:status" quirk — sid is implicit, since a turnState is
	// already scoped to one OpenCode session. Emit wire tool_call the
	// FIRST time a callID's own input is seen non-empty; emit wire
	// tool_result exactly ONCE, on the first "completed" or "error"
	// status for that callID.
	toolCallSent   map[string]bool
	toolResultSent map[string]bool

	// subtasksOpen tracks every subTaskId this turn has emitted
	// sub_task_start for but not yet sub_task_finish — keyed by whichever
	// id space started it (the legacy, unverified "subtask" part's own
	// "prt_..." id, or — the real, empirically-verified path, see
	// maybeStartTaskSubtask in sse.go — a task tool call's own spawned
	// child session's "ses_..." id; the two never collide). A sub-task
	// closed via the real per-sub-task completion signal
	// (maybeFinishTaskSubtask) is removed here immediately
	// (markSubtaskFinished); anything still left here when the ENCLOSING
	// turn itself finalizes is drained by finalize's own doc comment
	// (adapter.go) as the documented best-effort fallback.
	subtasksOpen map[string]bool

	// assistantMessageIDs tracks which OpenCode message ids belong to an
	// ASSISTANT message, learned from message.updated's own info.role the
	// moment that message is created. VERIFIED LIVE BUG this field fixes:
	// message.part.updated fires for the USER's own message too (its own
	// prompt text, echoed back as a "text" part on the user messageID) --
	// with no role check, that user-authored text was being emitted as a
	// wire token event (indistinguishable from real assistant output to a
	// control-plane consumer, since sandboxws.Token has no role field by
	// design) AND was marking sawText true before the assistant had said
	// anything at all, silently defeating §7's own "treat 'no output' as
	// failure" quirk on the live SSE path (a genuinely empty assistant
	// reply would still read as "completed"). dispatchPart's "text" case
	// now only translates/marks a text part whose MessageID is a KNOWN
	// assistant message id -- an id not yet seen at all is treated the
	// SAME as "not assistant" (fail-closed: skip), which is safe because
	// OpenCode's own real event ordering (verified live) always delivers
	// message.updated for a message before any of that message's own
	// parts.
	assistantMessageIDs map[string]bool

	// lastAssistantError/sawText/sawToolCall feed deriveOutcome
	// (outcome.go) once the turn's own terminal signal (session.idle) or
	// the SSE-inactivity fallback resolves it.
	lastAssistantError *openCodeTaggedError
	sessionError       *openCodeTaggedError
	sawText            bool
	sawToolCall        bool

	// spentUSD is this turn's own running cost total (§7.1's own corrected
	// text, §26.7) -- the accumulator §26.7's cost-budget
	// mechanism assumed already existed (it did not, see
	// internal/domain/reviewtriage.ShouldSkipOptionalPass's own "NOT YET
	// CALLED BY ANY PRODUCTION PATH" doc comment, costbudget.go) and this
	// Step finally builds. Summed by addCost, called from dispatchPart's
	// own "step-finish" case (sse.go) for EVERY step-finish this turn
	// observes outside a compaction window -- main lane AND every
	// sub-task alike, since §7.1's own fan-out routes a sub-task's events
	// back to this SAME turnState pointer (resolveEvent, adapter.go),
	// tagged with its own subTaskId -- dispatchPart itself is
	// subTaskId-agnostic (it has no idea, and does not need to know,
	// whether the step-finish it is looking at belongs to the main lane
	// or a sub-task), so summing unconditionally here is exactly "main
	// lane and every sub-task alike" by construction, not an extra case
	// to wire in. ALSO called directly (bypassing dispatchPart) from
	// dispatchEvent's own "message.part.updated" isCompacting branch
	// (sse.go, D1 fix) -- a compaction's own forceCompaction call
	// (compact.go) is a real, billed call that emits its own genuine
	// step-finish, and that cost must still be counted even though every
	// OTHER effect of that part (hasText, wire-event translation) is
	// deliberately suppressed for the whole compaction window. Read back
	// by Adapter.CurrentTurnSpentUSD (adapter.go), the one method
	// cmd/sandbox-agent's own loopback review-cost-budget HTTP server
	// (reviewcostbudgetserver.go) calls to answer a review agent's own
	// GET /review-cost-budget check.
	spentUSD float64

	// compacting/compactionAttempted implement §7.2's own compaction-retry
	// guard. compacting is true for the whole window between
	// Adapter.finalizeOrRecoverFromOverflow deciding to attempt a recovery
	// (adapter.go) and either giving up or successfully re-dispatching the
	// prompt — every dispatchEvent case that would otherwise corrupt this
	// turnState's own tracked fields (markAssistantMessageID/
	// setLastAssistantError/dispatchPart/finalize/setSessionError) with
	// compaction-INTERNAL SSE traffic checks isCompacting first and no-ops
	// instead (see dispatchEvent's own doc comment, sse.go, and this
	// Step's own VERIFIED LIVE finding: a synchronous POST /summarize call
	// still produces a full extra wave of message.updated/message.part.
	// updated/session.idle/session.error events for the SAME sessionID on
	// the SAME global SSE stream while it's in flight — see forceCompaction's
	// own doc comment, compact.go, for the full captured event sequence).
	// A single shared boolean guards all four dispatchEvent cases
	// (message.updated, message.part.updated, session.idle, session.error)
	// rather than one per case: simplicity wins here, since every one of
	// them needs to be suppressed for the exact SAME reason and the exact
	// SAME window.
	//
	// compactionAttempted is a SEPARATE, one-way flag (set once, never
	// reset) guarding against a SECOND compaction attempt if the retried
	// prompt ALSO overflows — at most one retry per turn (§7.2 point 3's
	// own infinite-loop guard).
	//
	// WIDENED by this Step ("typed transient-error retry for the OpenCode
	// adapter") to also guard a transient-APIError retry — deliberately
	// the SAME shared latch, not a second parallel one: a turn gets AT MOST
	// ONE recovery attempt per turn, of EITHER kind (see
	// finalizeOrRecoverFromOverflow's own doc comment, adapter.go, for why
	// sharing is correct here). attemptedKind below records WHICH kind that
	// one shot was actually spent on.
	compacting          bool
	compactionAttempted bool

	// attemptedKind records WHICH kind of recovery (recoveryKindCompaction
	// or recoveryKindTransientAPI) this turn's own at-most-one recovery
	// attempt was spent on — set ONLY by resolveOverflowAction below, in the
	// EXACT SAME ts.mu-guarded critical section that sets
	// compactionAttempted/compacting to true (never as a separate,
	// after-the-fact write: a version of this that set it via a second,
	// independently-locked call site AFTER resolveOverflowAction already
	// returned would reopen exactly the "two separately-locked critical
	// sections" TOCTOU class §7.2 Finding 2/this method's own doc comment
	// already closed once — a concurrent SECOND caller landing on
	// overflowActionAlreadyAttempted could then read attemptedKind's own
	// zero value before the WINNING caller ever got scheduled to set it).
	// Exists so the "already attempted" branch (finalizeOrRecoverFromOverflow,
	// adapter.go) can describe, honestly, which kind of retry this turn
	// already spent its one shot on, even when the CURRENT (second) failure
	// is of the OTHER kind — e.g. a turn whose first-time ContextOverflowError
	// already consumed the shared latch, whose retried prompt then hits a
	// transient APIError, must never be described as "a transient-error
	// retry was already attempted" when the retry this turn actually got was
	// a compaction one.
	attemptedKind recoveryKind

	// stopRequested is set by Adapter.Stop (adapter.go) the moment a user
	// Stop is observed for this exact turn's own OpenCode session — a
	// LATER audit's own Finding 1: Stop itself only posts an OpenCode-side
	// abort (Adapter.postAbort) and otherwise touches no turnState at all,
	// but that abort's own resulting session.idle/session.error is exactly
	// the kind of event dispatchEvent's own isCompacting guards (sse.go)
	// silently swallow while a compaction retry is in flight — so without
	// a DEDICATED signal recorded here, attemptCompactionRetry below would
	// have no way to ever learn a Stop happened during that window at all,
	// and would re-dispatch the very prompt the user just cancelled. A
	// one-way latch (never reset to false): once a turn has been asked to
	// stop, it stays asked-to-stop for its own remaining lifetime, mirroring
	// compactionAttempted's own one-way-latch precedent just above. Read via
	// stillLive below, never on its own -- see that method's own doc
	// comment for why both flags are checked together under one lock.
	stopRequested bool

	lastActivity time.Time

	// finalized is set exactly once, under mu, by tryFinalize below — and
	// read under that SAME mu by emit (Finding 4), so a racing SSE-
	// dispatched emit call and a concurrent Adapter.finalize call can
	// never straddle the moment finalize commits: whichever acquires mu
	// first atomically determines whether the event is delivered or
	// dropped.
	finalized bool

	// done is closed exactly once, by Adapter.finalize, once this turn's
	// own execution_complete (and any still-open sub_task_finish events)
	// have been emitted — the signal StartTurn's own wait loop is blocked
	// on.
	done chan struct{}
}

func newTurnState(cmd sandboxws.Prompt, sink ports.EventSink, model string) *turnState {
	return &turnState{
		cmd:                 cmd,
		sink:                sink,
		model:               model,
		toolCallSent:        make(map[string]bool),
		toolResultSent:      make(map[string]bool),
		subtasksOpen:        make(map[string]bool),
		assistantMessageIDs: make(map[string]bool),
		lastActivity:        time.Now(),
		done:                make(chan struct{}),
	}
}

// modelDisplay renders a resolved *promptModelRef (session.go's own
// resolveModelForced) as the "providerID/modelID" string turnState.model
// and ProviderFailureDiagnostic.Model both carry. The nil-ref guard below
// is defensive only, not a real production case: resolveModelForced never
// returns nil (session.go's own doc comment), so every caller today
// passes a non-nil ref — kept so this function stays total rather than
// panicking if a future caller ever passes nil.
func modelDisplay(m *promptModelRef) string {
	if m == nil {
		return ""
	}
	return m.ProviderID + "/" + m.ModelID
}

// emit populates AgentEvent.Critical/AckID via ports.ClassifyAgentEvent
// (the single shared "which wire types are critical" classification) and
// forwards to the sink — UNLESS this turn has already finalized (Finding
// 4): a late-arriving SSE-dispatched event (e.g. a fresh sub_task_start,
// dispatchSubtaskStart below racing Adapter.finalize on a different
// goroutine) must never slip out after execution_complete was already
// sent. The ts.finalized check happens under the SAME ts.mu that
// tryFinalize sets it under, and sink is called WHILE STILL HOLDING that
// lock — never released between the check and the sink call, or the race
// reopens — so whichever of {this emit call} or {finalize's own
// tryFinalize call} acquires ts.mu first correctly and atomically
// determines the outcome; there is no window for a check-then-act race in
// either direction. Every call site here goes through the SSE dispatch
// path (dispatchEvent, dispatchPart, dispatchTool, dispatchSubtaskStart,
// ...) — Adapter.finalize uses the separate emitFinal below instead, which
// is deliberately NOT subject to this same check.
//
// The returned bool reports whether the event was actually sent (true) or
// dropped because this turn had already finalized (false) — a caller with
// its own side effect to perform ONLY when the turn is still genuinely
// live (maybeStartTaskSubtask's own registerSubtaskSession call, sse.go)
// must gate that side effect on this same return value rather than
// performing it unconditionally before/after calling emit: doing it
// unconditionally would reopen exactly the check-then-act race this
// function's own ts.mu-guarded check exists to close, just one level up
// (a task's own "running" event processed after a concurrent finalize has
// already drained this turn's open sub-tasks would otherwise register a
// routing entry nothing will ever remove).
func (ts *turnState) emit(payload any) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.finalized {
		slog.Warn("opencode: dropping event emitted after finalize", "type", fmt.Sprintf("%T", payload))
		return false
	}

	critical, ackID := ports.ClassifyAgentEvent(payload)
	ts.sink(ports.AgentEvent{Payload: payload, Critical: critical, AckID: ackID})
	return true
}

// emitFinal is used ONLY by Adapter.finalize (adapter.go), for its own two
// kinds of terminal emission: the drained sub_task_finish events and the
// final execution_complete itself. Unlike emit above, this does NOT check
// ts.finalized — finalize is the one call path explicitly entitled to emit
// despite having just set that flag via tryFinalize — and needs no locking
// of its own beyond reading ts.sink, which is set once at construction
// (newTurnState) and never mutated by any other method on this type
// afterward: an unlocked read of a value that is written exactly once,
// before this turnState is ever shared across goroutines (registerTurn
// happens after newTurnState returns), and never written again, is safe
// per Go's own memory model.
func (ts *turnState) emitFinal(payload any) {
	critical, ackID := ports.ClassifyAgentEvent(payload)
	ts.sink(ports.AgentEvent{Payload: payload, Critical: critical, AckID: ackID})
}

// touch records that SOME event for this turn's own OpenCode session was
// just dispatched — resetting the SSE-inactivity clock that backs §7's
// own "SSE inactivity timeout (default 120s)" and "final-state fetch
// fallback" quirks (see adapter.go's finalizeByFallback). Called for
// EVERY dispatched event, even ones this adapter otherwise skips
// translating, since any event at all proves the stream is still alive for
// this session.
func (ts *turnState) touch() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.lastActivity = time.Now()
}

// idleFor reports whether this turn has gone at least d since its last
// touch — the poll-based check Adapter.waitForTurn uses instead of a
// Timer.Reset-based scheme (avoids that API's own well-known Stop/Reset
// race entirely, at the cost of poll granularity, acceptable here since
// this bounds only a last-resort fallback path, not the common case).
func (ts *turnState) idleFor(d time.Duration) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return time.Since(ts.lastActivity) >= d
}

// lastActivityTime returns the raw timestamp idleFor above compares against
// — used by Adapter.shouldFinalizeByFallback (adapter.go) to ask
// Adapter.disconnectedSince whether a genuine connection disconnect
// occurred at any point during THIS turn's own current idle episode, not
// just whether the turn has been idle for some duration in the abstract.
func (ts *turnState) lastActivityTime() time.Time {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.lastActivity
}

func (ts *turnState) setLastAssistantError(err *openCodeTaggedError) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.lastAssistantError = err
}

// markAssistantMessageID records messageID as belonging to an assistant
// message — see assistantMessageIDs' own field comment for why this
// exists.
func (ts *turnState) markAssistantMessageID(messageID string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.assistantMessageIDs[messageID] = true
}

// isAssistantMessage reports whether messageID is a KNOWN assistant
// message id — false for a user message id, and false (fail-closed) for
// an id this turnState has never seen a message.updated for at all.
func (ts *turnState) isAssistantMessage(messageID string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.assistantMessageIDs[messageID]
}

func (ts *turnState) setSessionError(err openCodeTaggedError) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.sessionError = &err
}

// isCompacting/setCompacting implement the compacting guard described on
// this type's own field comment above — read by dispatchEvent's four
// guarded cases (sse.go), set by Adapter.finalizeOrRecoverFromOverflow/
// attemptCompactionRetry (adapter.go).
func (ts *turnState) isCompacting() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.compacting
}

func (ts *turnState) setCompacting(v bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.compacting = v
}

// compactionAlreadyAttempted reports the one-shot retry guard described on
// this type's own field comment above — read-only; see tryBeginCompactionRetry
// below for the ONLY path allowed to actually set it.
func (ts *turnState) compactionAlreadyAttempted() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.compactionAttempted
}

// tryBeginCompactionRetry atomically implements finalizeOrRecoverFromOverflow's
// own "first-time ContextOverflowError, not yet attempted" branch (adapter.go)
// as a single locked critical section: check compactionAttempted, and if
// false, mark it true and set compacting true, all under the SAME ts.mu
// acquisition. Returns whether THIS call is the one that gets to launch the
// retry.
//
// §7.2 Finding 2's own fix: the OLD call site did this as two SEPARATE
// method calls (ts.compactionAlreadyAttempted() then, later, ts.
// markCompactionAttempted()+ts.setCompacting(true)) — a classic
// check-then-act race with no lock spanning both steps. The live SSE
// session.idle dispatch (sse.go, on the persistent SSE-reader goroutine) and
// the independent SSE-inactivity fallback (finalizeByFallback, adapter.go,
// on StartTurn's own calling goroutine) can both observe the very first
// ContextOverflowError for the same turn at nearly the same wall-clock
// moment (a plausible near-tie: both are wall-clock-driven, independent
// goroutines racing on the exact same a.sseInactivityTimeout threshold) —
// under the old two-call sequence, both could read compactionAttempted==false
// before either had set it, and both would go on to launch their own
// attemptCompactionRetry goroutine: two concurrent POST /summarize calls and,
// if both succeed, two concurrent retried prompt_async re-dispatches against
// the SAME OpenCode session, violating §3.3's "exactly one processing per
// session" invariant. Folding the check and the two writes into one
// ts.mu-guarded method closes that race exactly the same way tryFinalize
// above already closes the equivalent race for finalization.
func (ts *turnState) tryBeginCompactionRetry() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.compactionAttempted {
		return false
	}
	ts.compactionAttempted = true
	ts.compacting = true
	return true
}

// recoveryKind distinguishes WHICH of this turn's own at-most-one recovery
// attempts (compacting/compactionAttempted/attemptedKind above) is being
// claimed or was already spent — shared guard state across both kinds (this
// Step: "typed transient-error retry for the OpenCode adapter" widens §7.2's
// original compaction-only guard to also cover a transient APIError), but
// the concrete recovery action taken (Adapter.attemptCompactionRetry vs.
// Adapter.attemptTransientRetry, adapter.go) differs by kind: a
// ContextOverflowError forces a compaction before re-dispatching; a
// transient APIError re-dispatches directly, after a bounded backoff, with
// no compaction step at all.
type recoveryKind int

const (
	// recoveryKindNone means no recovery is applicable at all (the derived
	// outcome is neither a ContextOverflowError nor a transient APIError),
	// or (attemptedRecoveryKind's own zero value) no recovery has ever been
	// claimed for this turn yet.
	recoveryKindNone recoveryKind = iota
	// recoveryKindCompaction is §7.2's original context-overflow recovery:
	// forceCompaction, then re-dispatch.
	recoveryKindCompaction
	// recoveryKindTransientAPI is this Step's own transient-APIError
	// recovery: a bounded backoff wait, then re-dispatch — no compaction.
	recoveryKindTransientAPI
)

// overflowAction is the result of resolveOverflowAction below — the single
// decision Adapter.finalizeOrRecoverFromOverflow (adapter.go) acts on,
// whichever of its two callers (the live SSE dispatch path, or
// finalizeByFallback) produced it.
type overflowAction int

const (
	// overflowActionStale means the caller's own already-derived outcome
	// can no longer be trusted: EITHER a compaction retry is live right
	// now, or SOME activity was recorded for this turn after snapshotTime
	// — in both cases some OTHER goroutine already owns (or is about to
	// own) this turn's outcome, and the caller must abandon: no finalize,
	// no retry launch, nothing.
	overflowActionStale overflowAction = iota
	// overflowActionFinalizeDirect means the caller's own outcome is not a
	// first-time ContextOverflowError eligible for a retry — finalize with
	// it exactly as derived, unchanged behavior.
	overflowActionFinalizeDirect
	// overflowActionAlreadyAttempted means the derived outcome IS again
	// eligible for recovery (either kind), but a recovery retry was already
	// fully attempted for this turn (and, per the checks above, is not live
	// right now and nothing happened since snapshotTime) — finalize with an
	// enriched reason instead of launching a second attempt. The caller
	// reads ts.attemptedRecoveryKind() to know WHICH kind that was, so the
	// enriched reason describes the retry this turn actually got, not
	// necessarily the kind of the CURRENT (second) failure.
	overflowActionAlreadyAttempted
	// overflowActionBeginRetry means THIS call atomically won the retry:
	// ts.compactionAttempted/ts.compacting are now both true, ts.attemptedKind
	// now records which kind (recoveryKindCompaction/recoveryKindTransientAPI)
	// was claimed, and the caller must launch the matching recovery
	// function (attemptCompactionRetry or attemptTransientRetry) on its own
	// tracked goroutine.
	overflowActionBeginRetry
)

// resolveOverflowAction is the SINGLE ts.mu-guarded critical section
// Adapter.finalizeOrRecoverFromOverflow (adapter.go) routes both of its own
// callers through, replacing what used to be TWO SEPARATE critical
// sections: finalizeByFallback's own isCompacting()/lastActivityTime()
// staleness re-checks (evaluated, unlocked relative to each other, via two
// independent ts.mu acquisitions) followed — after more unlocked work
// (partsHaveOutput, deriveOutcome) — by a THIRD, independent ts.mu
// acquisition inside tryBeginCompactionRetry above.
//
// A LATER audit's own finding: that gap between the staleness re-checks and
// the eventual tryBeginCompactionRetry call was itself a TOCTOU window — a
// live SSE session.idle for the exact SAME first-time overflow could win
// tryBeginCompactionRetry strictly AFTER finalizeByFallback's own checks
// read clean but BEFORE finalizeByFallback's own call reached
// tryBeginCompactionRetry, causing finalizeByFallback to fall into the
// "already attempted" branch and finalize with a stale pre-retry snapshot
// while the just-launched real retry was still working — exactly the "two
// goroutines both believe they own the outcome" hazard this whole
// isCompacting/lastActivityTime/tryBeginCompactionRetry machinery exists to
// close, reopened by composing three separately-locked critical sections
// where the whole decision needed to be one. Narrowing the window further
// (e.g. re-checking staleness YET AGAIN immediately before this call)
// would only shrink it, not close it — the fix here instead folds
// "is my snapshot still fresh" and "atomically claim the retry" into the
// exact same lock acquisition, so nothing can ever observe a state change
// between the two.
//
// snapshotTime is the point in time as of which the caller's own outcome
// was derived: the live SSE dispatch path (dispatchEvent's session.idle/
// session.error cases, sse.go) passes essentially "now" — nothing
// intervenes between its own ts.touch()/errorForOutcome reads and this
// call, so the freshness check below is trivially satisfied there, exactly
// preserving that path's own prior behavior while now also closing the
// symmetric (vanishingly small, but real) gap it used to leave between its
// own isCompacting() guard and this call. finalizeByFallback (adapter.go)
// passes preFetchActivity, snapshotted BEFORE its own unlocked
// fetchFinalMessages HTTP round trip — the caller whose own snapshot can
// genuinely go stale for however long that fetch takes, and however long
// the CPU-bound partsHaveOutput/deriveOutcome work after it takes, since
// NEITHER matters any more: only the state AT THE INSTANT of this single
// locked check governs the outcome now.
//
// kind is recoveryKindNone when the caller's own already-derived outcome is
// eligible for neither recovery kind, or the specific kind
// (recoveryKindCompaction/recoveryKindTransientAPI) it computed via
// isContextOverflowError/isTransientAPIError (outcome.go) — a pure
// computation over already-fetched data, safe to perform before this call
// since it never touches ts.
//
// On overflowActionBeginRetry, kind is ALSO recorded into ts.attemptedKind
// in this SAME critical section (never a separate, after-the-fact write —
// see that field's own doc comment above for the TOCTOU a second,
// independently-locked write here would reopen).
func (ts *turnState) resolveOverflowAction(snapshotTime time.Time, kind recoveryKind) overflowAction {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.compacting || ts.lastActivity.After(snapshotTime) {
		return overflowActionStale
	}
	if kind == recoveryKindNone {
		return overflowActionFinalizeDirect
	}
	if ts.compactionAttempted {
		return overflowActionAlreadyAttempted
	}
	ts.compactionAttempted = true
	ts.compacting = true
	ts.attemptedKind = kind
	return overflowActionBeginRetry
}

// attemptedRecoveryKind reports WHICH recovery kind this turn's own
// at-most-one attempt was spent on (recoveryKindNone if none has been
// claimed yet) — read-only; see resolveOverflowAction above for the ONLY
// path allowed to actually set it.
func (ts *turnState) attemptedRecoveryKind() recoveryKind {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.attemptedKind
}

// clearErrorsForRetry resets this turn's own tracked assistant/session
// errors — called ONLY by Adapter.attemptCompactionRetry (adapter.go),
// immediately after a successful forceCompaction and immediately before
// re-dispatching the same prompt. CRITICAL: without this, a SUCCESSFUL
// retry's own eventual session.idle would still see the STALE original
// ContextOverflowError via errorForOutcome below and incorrectly finalize
// the turn as failed even though the retry itself actually succeeded.
func (ts *turnState) clearErrorsForRetry() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.lastAssistantError = nil
	ts.sessionError = nil
}

func (ts *turnState) markSawText() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.sawText = true
}

func (ts *turnState) markSawToolCall() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.sawToolCall = true
}

// addCost adds cost (a single step-finish's own p.Cost, USD) to this
// turn's own running spentUSD total (§26.7/§7.1) -- called from
// dispatchPart's own "step-finish" case (sse.go) for every step-finish
// this turn observes, main lane and every sub-task alike (see spentUSD's
// own field doc comment above for why no separate sub-task case is
// needed). cost is never negative in practice (OpenCode's own step-finish
// schema, types.go), but this adds it verbatim rather than guarding
// against a hypothetical negative value here -- ShouldSkipOptionalPass
// (reviewtriage/costbudget.go) already clamps a negative spentUSD to 0 at
// the one place that actually matters (its own decision), so a defensive
// guard here would only duplicate that, never add real safety.
//
// cost is *float64 (§25.15, types.go's own stepFinishPart.Cost doc
// comment): nil means OpenCode's own "cost" key was genuinely absent from
// this step-finish, and is a no-op here -- adding a fabricated 0 would
// silently understate this SAME sandbox-local total exactly as it would
// the control-plane's own turns.cost_usd (internal/app/sessionactor's own
// stepcost.go, which skips an absent cost.usd the identical way).
func (ts *turnState) addCost(cost *float64) {
	if cost == nil {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.spentUSD += *cost
}

// spentUSDTotal returns this turn's own running spentUSD total (addCost's
// own accumulator, above) -- read by Adapter.CurrentTurnSpentUSD
// (adapter.go). Named with a "Total" suffix, not just "spentUSD", to keep
// it visually distinct from the field of the identical name it reads
// (Go permits a method and a field to share a name, but this package's
// existing style elsewhere avoids that pattern, e.g. compactionAlreadyAttempted
// vs. compactionAttempted).
func (ts *turnState) spentUSDTotal() float64 {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.spentUSD
}

// errorForOutcome returns whichever tagged error was observed for this
// turn, preferring the final assistant message's own "error" field over
// session.error's (ground truth: "Derive... from whichever of these you
// observe" — both were verified to carry the identical shape live on the
// same aborted turn, so preferring one over the other is a tie-break, not
// a correctness concern).
func (ts *turnState) errorForOutcome() *openCodeTaggedError {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.lastAssistantError != nil {
		return ts.lastAssistantError
	}
	return ts.sessionError
}

func (ts *turnState) outcomeInputs() (hasText, hasToolCall bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.sawText, ts.sawToolCall
}

// alreadySentCall/markCallSent/alreadySentResult/markResultSent implement
// the per-callID dedup state described on toolCallSent/toolResultSent
// above.
func (ts *turnState) alreadySentCall(callID string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.toolCallSent[callID]
}

func (ts *turnState) markCallSent(callID string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.toolCallSent[callID] = true
}

func (ts *turnState) alreadySentResult(callID string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.toolResultSent[callID]
}

func (ts *turnState) markResultSent(callID string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.toolResultSent[callID] = true
}

// subtaskAlreadyStarted/markSubtaskStarted guard sub_task_start against
// OpenCode ever repeating the same subtask id across multiple updates for
// its own underlying signal (a "subtask" part's own fields are static once
// created, per its schema; a "task" tool call's own "running" state can
// likewise repeat — this guard keeps sub_task_start genuinely "first sight
// only" regardless of which signal or how many times it repeats).
func (ts *turnState) subtaskAlreadyStarted(subTaskID string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.subtasksOpen[subTaskID]
}

func (ts *turnState) markSubtaskStarted(subTaskID string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.subtasksOpen[subTaskID] = true
}

// markSubtaskFinished removes subTaskID from the open set — called by
// maybeFinishTaskSubtask (sse.go) once it has ALREADY emitted that
// sub-task's own sub_task_finish via the real, per-sub-task completion
// signal (the task tool call's own "completed"/"error" transition), so
// Adapter.finalize's own drainOpenSubtasks below (still the fallback for a
// sub-task whose own completion signal never arrived before the turn
// itself ended, e.g. cancellation) never double-closes it.
func (ts *turnState) markSubtaskFinished(subTaskID string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.subtasksOpen, subTaskID)
}

// drainOpenSubtasks returns every subTaskId still open (sub_task_start
// emitted, sub_task_finish not yet) and clears the set — called exactly
// once, by Adapter.finalize, so every subtask this turn ever started is
// guaranteed a matching sub_task_finish (§6.1's own critical-delivery
// rationale for that event type). Only ever contains a subtask the real
// per-sub-task completion signal (markSubtaskFinished above) has not
// already closed out.
func (ts *turnState) drainOpenSubtasks() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ids := make([]string, 0, len(ts.subtasksOpen))
	for id := range ts.subtasksOpen {
		ids = append(ids, id)
	}
	ts.subtasksOpen = make(map[string]bool)
	return ids
}

// markStopRequested records that Adapter.Stop (adapter.go) observed a Stop
// for this exact turn's own OpenCode session — see stopRequested's own
// field comment for why this dedicated flag exists at all.
func (ts *turnState) markStopRequested() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.stopRequested = true
}

// stillLive reports whether attemptCompactionRetry (adapter.go) is still
// entitled to re-dispatch ts.cmd after a successful forceCompaction — a
// LATER audit's own Finding 1's fix. false means either:
//
//   - stopRequested: a user Stop landed for this exact turn at some point
//     during the compaction window (markStopRequested above) — re-dispatching
//     now would silently override that cancellation, exactly the "Stop
//     ignored" hazard this method exists to close.
//   - finalized: this turn was ALREADY finalized by some other path while
//     compaction was in flight — concretely, StartTurn's own ctx being
//     canceled (e.g. a process shutdown) races waitForTurn's own
//     ctx.Done() branch straight to finalizeCanceled, entirely independent
//     of a.bgCtx (attemptCompactionRetry's own base context, which that ctx
//     cancellation never touches) — re-dispatching here would restart work
//     for a turn the control plane has already been told is Cancelled.
//
// Both flags are read together under ONE ts.mu acquisition — not because
// either individually needs cross-field atomicity (each is its own
// one-way latch, never reset), but to mirror this package's existing
// "single ts.mu-guarded read/check" idiom (tryBeginCompactionRetry,
// tryFinalize) rather than introducing a bespoke two-call convention just
// for this one caller.
func (ts *turnState) stillLive() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return !ts.stopRequested && !ts.finalized
}

// tryFinalize marks this turn finalized exactly once, reporting whether
// THIS call was the one that did so (false means some earlier call already
// finalized it — the caller must not emit anything further).
//
// Also atomically clears compacting -- a CI-only regression fix: attemptCompactionRetry/
// attemptTransientRetry's own "abort during retry" branches (adapter.go) used
// to call a.finalize (which closes ts.done, the signal StartTurn's own
// waitForTurn blocks on) FIRST, and only THEN call ts.setCompacting(false) as
// a separate, later statement on the same goroutine -- deliberately, to make
// sure THIS goroutine always wins tryFinalize against a stray compaction-tail
// SSE event before compacting ever reads false to anyone else (see this
// field's own comment above, and attemptCompactionRetry's now-simplified doc
// comment, for that original race). But nothing ever synchronized the OTHER
// goroutine unblocked by close(ts.done) (StartTurn's own caller, e.g. a
// test's group.Wait()) against that later ts.setCompacting(false) call --
// close() and the receive it unblocks are a synchronization point in Go's
// memory model, but ts.compacting is a SEPARATE field guarded by a SEPARATE
// lock acquisition after finalize already returned, so an observer that woke
// up because ts.done closed had no guarantee of also observing compacting's
// new value yet. Under scheduling pressure (confirmed reproducible locally,
// several-per-thousand-iterations under `go test -race -count=N`, on a
// CLEAN checkout with none of this branch's own broadcast-wait changes
// present) that gap let TestCompactionRetry_StopDuringCompactionAbortsRetry
// observe ts.isCompacting() still true immediately after group.Wait()
// returned. Folding the compacting clear into the SAME ts.mu critical
// section that commits ts.finalized closes the gap to zero width: nothing
// can ever observe ts.finalized (or, transitively, ts.done closed, which
// this method's only caller -- Adapter.finalize -- never does until AFTER
// this call returns) without also observing compacting already false. This
// does not reopen the original stray-event race: dispatchEvent's own
// isCompacting guards (sse.go) and resolveOverflowAction's own re-check
// (turn.go) never consult ts.finalized directly, only ts.compacting -- and
// compacting cannot read false to ANY goroutine until this exact call has
// already committed ts.finalized true under this same lock, so a stray event
// that observes compacting==false is guaranteed to find tryFinalize already
// claimed (a no-op finalize call) exactly as before.
func (ts *turnState) tryFinalize() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.finalized {
		return false
	}
	ts.finalized = true
	ts.compacting = false
	return true
}
