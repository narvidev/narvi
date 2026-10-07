// Package workflowengine is §25.6's own ("workflow execution engine",
// §25.6/§25.7/§25.8) impure engine atop §25.4's dark domain model
// (internal/domain/workflow, internal/domain/loopguard) and schema
// (migrations/000057_workflows.up.sql, internal/adapters/outbound/postgres.
// WorkflowStore). This package MAY do I/O (CLAUDE.md, §11's own carve-out
// for /internal/app) but every actual DECISION -- what a step's default
// vocabulary means, what happens after a posted outcome -- goes through
// internal/domain/workflow's pure functions (LaneFor, ValidateDefinition,
// NextStep); this package's own job is reading fresh state, calling the
// right decision function, and writing the result back, exactly like
// internal/app/sessionactor's own dispatch.go/timerfired.go already do for
// their own domain packages.
//
// # The two wiring points (§25.6)
//
// ResolveStepForNewTurn (dispatch.go) is called from
// internal/adapters/inbound/httpapi's createTurnLocked, BEFORE the turn
// row itself is inserted: it resolves the session's Lane (workflow.LaneFor,
// from sessions.intent_decision), the (lane, repo) WorkflowBinding, the
// bound WorkflowDefinition, and -- via the session's current
// WorkflowRun/live WorkflowStepRun, if any -- which StepDefinition governs
// THIS turn. It returns the turn's actual prompt/modelID to use: the
// step's PromptTemplate rendered against the caller's own text
// (internal/domain/intent.AssembleTemplate, "{{prompt}}" being pure
// passthrough for the built-in review/request/plan steps, §25.8's
// zero-config proof) and the step's ModelID when non-nil, otherwise the
// caller's own modelID UNCHANGED (§25.7: no override). AttachTurn backfills
// the newly created step-run's turn_id once the real turn row exists
// (ResolveStepForNewTurn cannot know it yet -- it runs before that insert).
//
// OnTurnCompleted (completion.go) is called from sessionactor at each of
// the three places a turn reaches a real terminal state (pushpr.go's
// completeProcessingTurn, timerfired.go's handleTurnDeadlineTimer,
// dispatch.go's failDispatchedTurn -- see completion.go's own doc comment
// for why all three, not just the first). It looks up whether the
// finishing turn is a live, engine-tracked attempt; if so, it derives (or
// honors an already-posted) StepOutcomeStatus, consults workflow.NextStep,
// and applies the verdict: advance (create the next attempt -- see
// completion.go's own doc comment on why this Step never auto-dispatches
// one), complete the run, or escalate it to needs_review. A HITLAfter-gated
// step never reaches NextStep at all here -- its attempt lands in
// awaiting_decision and the run stays running, exactly where §25.9's own
// decide endpoint picks it up. As of migration
// 000088_plan_builtin_passthrough (§25.9's own corrective follow-up,
// §25.8/§25.9), no BUILT-IN workflow carries hitl_after any longer -- the
// built-in plan workflow's original 2-step, hitl_after-on-step-1 shape
// double-parked a workflow-level HITL gate against classic plan mode's own
// pre-existing, unconditional persisted-state awaiting-plan gate on every
// plan-mode session, so it was corrected to a single-step passthrough,
// matching review/request; this mechanism now only fires for a CUSTOM
// (non-built-in) workflow's own hitl_after step.
//
// # The autonomy freeze (technical plan §40.2)
//
// OnTurnCompleted's advance is an automatic action: while autonomy is
// frozen it is held in a row instead of applied, and HeldAdvanceReleaser
// (release.go) applies it exactly once after the freeze lifts. Completing
// or escalating a run -- the circuit breaker's escalation included -- and a
// person's decision on a step are never held.
// One exception to the fail-open rule below: a freeze that cannot be read
// is a skip, never a pass, so the advance is not applied -- the failed read
// has aborted the caller's transaction, whose turn end is then retried.
//
// # Fail-open is the load-bearing safety property of this whole package
//
// This engine runs in 100% of production turn dispatch from day
// one -- so neither wiring point may ever
// let an internal bug, a malformed custom definition, or a missing row
// block an ordinary turn from being created or completed. ResolveStepForNewTurn
// and OnTurnCompleted therefore never return an error at all: any failure
// internal to this package is logged (platform.Logger(ctx)) and degrades to
// the safest available fallback (pass the caller's own prompt/modelID
// through unchanged, or leave a turn's completion bookkeeping untouched)
// rather than propagating up into createTurnLocked/handleSandboxEvent's own
// transaction and rolling back real user-facing work over what is
// fundamentally observability/bookkeeping for now. This mirrors
// internal/domain/workflow.LaneFor's own documented fail-open discipline
// (§25.13) one level up, at the engine's own I/O boundary.
//
// # What this package deliberately does NOT do (§25.9's job)
//
// No HITL decision endpoint, no notifier wiring, no loopguard consultation
// (verified directly: none of the three built-in workflows ever re-fires a
// needs_fix edge through a path this Step actually evaluates -- see
// completion.go's own doc comment), no auto-dispatch of an advanced step's
// turn. plan mode's own existing approve/reject/revise dispatch
// (internal/adapters/inbound/httpapi/decideplan.go) is completely
// untouched by this package -- see dispatch.go's own doc comment for the
// documented, deliberate gap that leaves in this Step's own engine
// coverage.
package workflowengine
