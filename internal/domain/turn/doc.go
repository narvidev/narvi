// Package turn implements the turn (prompt) state machine, its queue, and
// two derived-fact helpers (§3.3, "Turn (prompt)"):
//
//   - The explicit Transition(from, trigger) (to, error) table (state.go),
//     matching internal/domain/sandbox's house style: typed sentinel
//     errors for illegal transitions, never a bare zero-value State
//     silently accepted.
//   - DeriveFailureReason (failurereason.go): given the same (from,
//     trigger) pair Transition just validated, which of the four
//     session_failure_reason values (cancelled/failed/timeout/
//     never_started) a Failed/Cancelled transition implies. Turns
//     themselves carry no failure_reason column (migrations/
//     000005_turns.up.sql) — only sessions do (migrations/
//     000004_sessions.up.sql) — so this mapping exists purely so a caller
//     that DOES need one (internal/domain/session below, later a session
//     actor) can derive the SESSION's failure_reason without turns
//     redundantly storing anything.
//   - RequiresSyntheticExecutionComplete (synthetic.go): given a trigger
//     that produced a terminal transition, whether the caller must
//     synthesize an execution_complete event (§3.3: "Stop/failure paths
//     emit a synthetic execution_complete event so clients always see one
//     terminal event per turn").
//   - HasInFlightTurn / NextToDispatch (queue.go): the "exactly one
//     processing per session, dispatch oldest pending next" queue policy
//     (§3.3; DB-enforced too, via the turns_one_processing_per_session
//     partial unique index).
//   - EvaluateTurnDeadline (deadline.go): the turn_deadline named
//     persistent timer (§2, §5.4 Chain A), using
//     platform.Timeouts.TurnDeadline (already defined at §5.4).
//     ReenqueueBound beside it: a turn re-sent to a newer sandbox gen
//     keeps its first dispatch's bound, and one with no more than the
//     re-send floor of it left is not re-sent (§3.3, §35.4).
//   - Summary (summary.go): the minimal per-turn view internal/domain/
//     session's status derivation needs.
//   - EpistemicOutcome (epistemicoutcome.go) and the devil's-advocate
//     preamble rendering/gating (epistemicpreamble.go): §20's own
//     ("domain/turn: builder epistemic pre-action check", §20) closed
//     3-value outcome vocabulary (none/minor/strong), the fixed preamble
//     text a non-plan-mode build turn's prompt is preceded by when the
//     check is enabled, and the two small pure functions deciding
//     enabled/injected (session-override-wins-over-platform-default,
//     never for a plan-mode turn).
//   - The interruption readers and texts (interruption.go; §35.4): a turn
//     its sandbox's stop interrupted has not failed. ReadAgentEnd reads an
//     agent's execution_complete -- its typed interruption key first, an
//     older agent's cancellation text compared whole as the permanent
//     fallback -- and never reads a reason's digits or a diagnostic;
//     AttributeInterruption names the cause from the gen's persisted
//     deadline; DecideSilentInterruption decides a gen lost in silence
//     under its turn; and RenderInterruptedTurnContext,
//     PromptWithInterruptionContext and InterruptionWarning write the
//     agent-facing context and the neutral warning, with no failure word
//     and no duration.
//
// Every function here is pure per §11: no I/O, no time.Now(), no
// randomness — anything needing "the current time" takes it as an
// explicit `now time.Time` parameter. This package does not import
// internal/platform (domain has zero external dependencies, §1); timeout
// VALUES are threaded in by whoever wires this package up, same as
// internal/domain/sandbox.
//
// Two design calls the plan leaves genuinely ambiguous are resolved here:
//
//  1. WHICH transition arms turn_deadline. §3.3 says "Dispatch arms
//     turn_deadline", right after stating "Enqueue → if no live sandbox,
//     trigger spawn and return (dispatch happens when sandbox connects)".
//     Read together, these pin the Pending→Dispatched edge down precisely:
//     "dispatch" is not "the turn is queued, waiting for a sandbox" — it
//     is the act of handing the turn to a sandbox that is ALREADY live,
//     which by definition only happens once first_connect_budget's window
//     has already been satisfied. So arming turn_deadline at
//     Pending→Dispatched (TriggerDispatch) does not double-count spawn/
//     connect latency against it — that latency is already over by the
//     time Dispatched is ever reached. The alternative (arming later, at
//     Dispatched→Processing) would leave the hand-off gap between "sandbox
//     connected" and "agent confirmed it started" covered by no watchdog
//     at all — first_connect_budget has already succeeded, and the timer
//     hasn't started yet — silently defeating §5.4's point that every
//     window in a turn's lifecycle is bounded by something. The schema
//     backs this reading too: turns has a single `dispatched_at` column
//     (migrations/000005_turns.up.sql) and no separate "processing
//     started" column, i.e. dispatch time is the one moment meant to be
//     recorded and measured from. So turn_deadline is armed at
//     Pending→Dispatched, measured via EvaluateTurnDeadline from
//     dispatchedAt; TriggerStartProcessing does not re-arm it. The caller
//     (a later Step's session actor) is expected to arm the
//     `turn_deadline` named timer (§2) at the Pending→Dispatched
//     transition, using the same `dispatched_at` timestamp it persists.
//
//  2. HOW never_started is modeled. Rather than inventing a distinct
//     trigger per abandoning FROM-state (e.g. a TriggerAbandonPending
//     alongside a TriggerAbandonDispatched), this package uses a SINGLE
//     TriggerAbandon, legal from both Pending and Dispatched — consistent
//     with how TriggerCancel already applies uniformly across
//     Pending/Dispatched/Processing (and with internal/domain/sandbox's
//     own TriggerSuspect, which fires uniformly from five different live
//     states). The FROM state is not part of the trigger's identity; what
//     matters is the semantic event itself ("give up on this turn before
//     any sandbox ever started processing it") which is one
//     control-plane decision regardless of which pre-Processing state the
//     turn happened to be sitting in when the decision was made (spawn
//     attempts exhausted, or the session itself is being torn down).
//     DeriveFailureReason still reports NeverStarted correctly for both
//     edges because the trigger alone determines the mapping:
//     TriggerAbandon is the only trigger that ever reaches Failed from
//     Pending or Dispatched, and no other trigger implies NeverStarted.
package turn
