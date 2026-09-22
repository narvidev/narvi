// Package opencode implements the OpenCode ports.AgentRuntime adapter
// (§7): the anti-corruption layer between Narvi's own wire contract
// (contracts/gen/go/sandboxws) and OpenCode's real HTTP+SSE API. It is a
// pure HTTP+SSE client against an ALREADY-RUNNING `opencode serve` process
// — mirroring internal/adapters/outbound/modal's own shape exactly — and
// does NOT spawn or supervise that process itself; that is
// internal/sandboxagent/opencodeproc's own job (a Spawn call that reuses
// internal/sandboxagent/supervisor, never a bare exec.Command), the same
// separation Modal's adapter already has from §6.4's own supervisor.
//
// See adapter.go's own doc comment on Adapter for the verified-vs-schema-
// derived-vs-best-effort breakdown of the real OpenCode wire shapes this
// package translates, and for the documented reasoning behind this
// adapter's own best-effort sub-task handling (§7.1).
//
// File layout:
//   - adapter.go: the Adapter type, New/Close/Connected, StartTurn/Stop
//     (the ports.AgentRuntime implementation), the per-turn wait/finalize
//     orchestration, and (§7.2) finalizeOrRecoverFromOverflow/
//     attemptCompactionRetry, the context-overflow compaction-retry
//     decision point both dispatchEvent's own "session.idle" case (sse.go)
//     and finalizeByFallback route through instead of calling finalize
//     directly.
//   - compact.go (§7.2): forceCompaction, the POST /session/{id}/summarize
//     call finalizeOrRecoverFromOverflow/attemptCompactionRetry (adapter.go)
//     use to force a compaction before retrying an overflowed turn.
//   - session.go: OpenCode session resolution (create-vs-resume), the §7
//     model-catalog-fallback quirk (resolveModelForced/resolveProviderModel
//     -- resolveModelForced is this adapter's only model-resolution
//     entry point, "forced" because it never returns nil, used
//     identically by StartTurn's own initial dispatch and every retry's
//     own re-dispatch, §7.3 audit fix), and prompt_async/abort/final-
//     message-fetch HTTP calls.
//   - sse.go: the persistent global GET /event connection and per-event
//     dispatch, including the tool-call/tool-result dedup logic (§7's own
//     "dedupe tool states by sid:callID:status" quirk) and (§7.2) the
//     ts.compacting guard on message.updated/message.part.updated/
//     session.idle/session.error that prevents a synchronous /summarize
//     call's own compaction-internal SSE traffic (a full extra wave of
//     events for the SAME sessionID, empirically confirmed by capturing
//     real GET /event traffic during a live /summarize call) from
//     corrupting the turn's own tracked state or triggering a premature
//     finalize.
//   - turn.go: turnState, the per-in-flight-turn demux/dedup/outcome-
//     tracking state the SSE loop and StartTurn's own wait loop share,
//     including (§7.2) the compacting/compactionAttempted guard fields and
//     clearErrorsForRetry.
//   - translate.go: pure OpenCode-part-or-message -> wire-event
//     translation functions.
//   - outcome.go: deriveOutcome, the pure, directly unit-testable
//     execution_complete outcome derivation (§7's "treat 'no output' as
//     failure" quirk and the tagged-error-to-outcome mapping), and (§7.2)
//     the equally pure isContextOverflowError/enrichReasonForFailedRecovery/
//     enrichReasonForRepeatedOverflow helpers finalizeOrRecoverFromOverflow/
//     attemptCompactionRetry (adapter.go) use.
//   - types.go: this adapter's own understanding of OpenCode's real wire
//     shapes, including (§7.2) summarizeRequest.
//   - client.go: the low-level JSON HTTP helper every other file's own
//     OpenCode calls go through -- doJSON (the a.requestTimeout-bounded
//     default every EXISTING caller keeps using unchanged) and (§7.2
//     Finding 3) doJSONTimeout, its extracted body taking the per-request
//     timeout as an explicit parameter so forceCompaction (compact.go) can
//     use the separate, more generous a.summarizeTimeout instead.
package opencode
