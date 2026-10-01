// workflowRunFormat.ts -- pure, testable formatting/derivation helpers for
// WorkflowRunsView.tsx (the run view + human decision gate), mirroring this
// codebase's own established split (planFormat.ts/reviewFormat.ts/
// workflowFormat.ts): render logic that needs no React lives here.
//
// # The edge actually taken is derived here, never stored
//
// §25.15 is explicit that "the edge actually taken" is not a wire field: it
// is derivable, exactly, from the ordered step runs and their outcome
// statuses -- the next step run's own stepDefinitionId IS the edge target
// that was taken. edgeToNext/buildStepRunSequence below are that
// derivation, kept pure and unit-testable so the logic has one home instead
// of being inlined into the view.
import type { SessionActivity, WorkflowRun, WorkflowStepRun } from '@narvi/contracts/rest-dtos'
import { formatUsd } from './money'

import { canActOnPlan } from './planFormat'
import type { ChipTone } from './reviewFormat'
import type { StatusTone } from './sessionStatus'
import { edgeStatusTone } from './workflowFormat'

// -- run-level status --------------------------------------------------

/** Reuses sessionStatus.ts's own StatusTone (adds 'run' -- a pulsing chip for an actively-in-progress state -- to the plain ChipTone vocabulary) rather than inventing a second "tone plus running" union: a workflow run's own 'running' status is the exact same kind of live-activity fact a session's own status chip already renders this way. */
export function runStatusTone(status: WorkflowRun['status']): StatusTone {
  switch (status) {
    case 'running':
      return 'run'
    case 'completed':
      return 'ok'
    case 'needs_review':
      return 'warn'
    case 'failed':
      return 'crit'
    case 'cancelled':
      return 'neutral'
    default:
      return 'neutral'
  }
}

export function runStatusLabel(status: WorkflowRun['status']): string {
  switch (status) {
    case 'running':
      return 'running'
    case 'completed':
      return 'completed'
    case 'needs_review':
      return 'needs review'
    case 'failed':
      return 'failed'
    case 'cancelled':
      return 'cancelled'
    default:
      return status
  }
}

/**
 * liveEscalationRunId reads, from the session's status (GET
 * /api/sessions/{id}/status, SessionActivity), the run whose escalation
 * is still open -- the server's rule, technical plan §43.20: a custom
 * workflow's run escalated to needs_review, while it is the session's
 * newest run, ran an attempt of its own, and no turn other than its own
 * attempts has been created since it escalated. needs_review is never
 * left (the next turn starts a fresh run beside the parked one), so the
 * run's own status cannot tell a live escalation from a parked one; this
 * client never re-derives the rule, it reads `escalation`, which the
 * server reports whatever gate `awaiting` names first. A server older
 * than that field leaves it out; its `awaiting` then names the escalation
 * when no plan or workflow step is open before it. Null without a status
 * read yet, or with no escalation open.
 */
export function liveEscalationRunId(activity: SessionActivity | null | undefined): string | null {
  if (!activity) return null
  if (activity.escalation !== undefined) return activity.escalation?.id ?? null
  return activity.awaiting?.kind === 'workflow_escalation' ? activity.awaiting.id : null
}

/**
 * isLiveRun reports whether a run can still change, so WorkflowRunsView.tsx
 * features it and re-reads it: a run still running (its step's turn, or a
 * step awaiting a person's decision), or the live escalation above. A
 * needs_review run that is not the live escalation is parked for good:
 * nothing moves it again, and re-reading it would fetch an immutable
 * document every few seconds.
 */
export function isLiveRun(run: WorkflowRun, liveEscalationId: string | null): boolean {
  return run.status === 'running' || run.id === liveEscalationId
}

/**
 * WORKFLOW_RUN_POLL_MS is how often a live run is re-read. Matches the
 * cadence the other live surfaces in this app already use, so an operator
 * watching two screens does not see them disagree about how current they
 * are. Deliberately a plain client-side interval and not a websocket
 * subscription: the run read model changes on the order of a turn, not a
 * token, and a second realtime channel is a mechanism this screen does not
 * need to justify.
 */
export const WORKFLOW_RUN_POLL_MS = 15_000

/**
 * runRefetchInterval is how often WorkflowRunsView.tsx re-reads the run it
 * shows: every WORKFLOW_RUN_POLL_MS while the run can still move
 * (isLiveRun), never otherwise. A completed, failed or cancelled run is
 * finished changing, and so is a parked escalation -- needs_review is
 * never left -- so polling either would be a request every few seconds
 * for a document that is now immutable, the operator's own pick from the
 * run history included.
 */
export function runRefetchInterval(run: WorkflowRun | null, liveEscalationId: string | null): number | false {
  return run !== null && isLiveRun(run, liveEscalationId) ? WORKFLOW_RUN_POLL_MS : false
}

/**
 * featuredRun picks the run WorkflowRunsView.tsx features: the newest live
 * one (isLiveRun) if there is one, otherwise the newest run that is not a
 * parked escalation. A parked escalation -- needs_review, and not the live
 * escalation the status surface reports -- stays listed in the rail and is
 * never featured: a later run, finished or not, is what the session did
 * next. Null when every run is a parked escalation, or there is none.
 * `runs` is already server-sorted newest-first (ListWorkflowRunsForSession's
 * own ORDER BY created_at DESC), so nothing here re-sorts.
 */
export function featuredRun(runs: readonly WorkflowRun[], liveEscalationId: string | null): WorkflowRun | null {
  const live = runs.find((r) => isLiveRun(r, liveEscalationId))
  if (live) return live
  return runs.find((r) => r.status !== 'needs_review') ?? null
}

/**
 * NEEDS_REVIEW_EXPLANATION is the operator-facing text for a run parked at
 * 'needs_review' with no attempt awaiting a decision (i.e. the circuit
 * breaker escalation path, not the ordinary HITL gate -- see
 * decidableStepRun below for how the view tells the two apart). Mirrors the
 * server's own escalationNoticeText (advance.go) honestly: that function
 * cannot tell a spent retry loop apart from an outcome with no configured
 * next step either, so this does not pretend to. Deliberately offers no
 * retry affordance: a spent circuit breaker is a stopping point, not
 * something a manual re-trigger works around (the manual-re-trigger
 * exemption is a budget-check carve-out for automatic re-review, a
 * different mechanism entirely, and does not apply to a breaker that has
 * already tripped here).
 */
export const NEEDS_REVIEW_EXPLANATION =
  "Automatic progress on this run has stopped: either a bounded retry reached its limit, or a step's outcome had no automatic next step configured. Nothing further happens on this run automatically, and there is no retry action here."

/** ESCALATION_OPEN_NOTE: the live escalation's own gate -- what answers it. */
export const ESCALATION_OPEN_NOTE = 'It is waiting on a person: new work sent to the session -- a message, a mention, a plan approval -- answers it and starts a fresh run.'

/** ESCALATION_CLOSED_NOTE: a parked escalation nothing waits on any more. */
export const ESCALATION_CLOSED_NOTE = 'Nothing waits on it any more: the session has moved on since it escalated, or it belongs to a built-in workflow, which no person acts on.'

/**
 * escalationNotice is the banner a run parked at needs_review with no
 * attempt awaiting a decision shows: NEEDS_REVIEW_EXPLANATION, then whether
 * the escalation still waits on a person -- the live escalation the
 * status surface reports (liveEscalationRunId), which new work sent to the
 * session answers (technical plan §43.20) -- or no longer does. Null for
 * any other run.
 */
export function escalationNotice(run: WorkflowRun, liveEscalationId: string | null): string | null {
  if (run.status !== 'needs_review') return null
  const note = run.id === liveEscalationId ? ESCALATION_OPEN_NOTE : ESCALATION_CLOSED_NOTE
  return `${NEEDS_REVIEW_EXPLANATION} ${note}`
}

// -- step-attempt-level status ------------------------------------------

export function stepRunStatusTone(status: WorkflowStepRun['status']): StatusTone {
  switch (status) {
    case 'awaiting_decision':
      return 'warn'
    case 'running':
      return 'run'
    case 'completed':
      return 'ok'
    case 'failed':
      return 'crit'
    case 'cancelled':
      return 'neutral'
    default:
      return 'neutral'
  }
}

export function stepRunStatusLabel(status: WorkflowStepRun['status']): string {
  switch (status) {
    case 'awaiting_decision':
      return 'awaiting decision'
    case 'running':
      return 'running'
    case 'completed':
      return 'completed'
    case 'failed':
      return 'failed'
    case 'cancelled':
      return 'cancelled'
    default:
      return status
  }
}

/** outcomeStatusTone reuses workflowFormat.ts's own edgeStatusTone: WorkflowStepRun.outcomeStatus and WorkflowEdge.onStatus are the exact same closed 3-value Postgres enum, so this is one rule with two call sites, not two independent copies. */
export function outcomeStatusTone(status: NonNullable<WorkflowStepRun['outcomeStatus']>): ChipTone {
  return edgeStatusTone(status)
}

export function outcomeStatusLabel(status: NonNullable<WorkflowStepRun['outcomeStatus']>): string {
  switch (status) {
    case 'ok':
      return 'ok'
    case 'needs_fix':
      return 'needs fix'
    case 'blocked':
      return 'blocked'
    default:
      return status
  }
}

export function decisionLabel(decision: NonNullable<WorkflowStepRun['decision']>): string {
  switch (decision) {
    case 'approve':
      return 'approved'
    case 'reject':
      return 'rejected'
    case 'revise':
      return 'revision requested'
    default:
      return decision
  }
}

/**
 * formatStepCost renders WorkflowStepRun.costUsd honestly, which here means
 * defending TWO distinctions, not one.
 *
 * null is "no cost figure has arrived yet for this attempt" and must never
 * collapse into "$0.00" -- an already-observed $0.00 is a real value and a
 * different one from "unknown yet" (§25.15).
 *
 * And a sub-cent figure must not collapse into "$0.00" either. Two decimals
 * is the wrong precision for a PER-STEP figure: a single agent step
 * routinely costs a fraction of a cent, so "$0.00" would be what the screen
 * showed for most steps, indistinguishable from free. The column behind this
 * carries six decimals for exactly that reason -- rounding the honesty back
 * out here would undo it one layer up, which is the same mistake in a
 * different place. Anything that would round to zero but is not zero gets
 * four decimals instead.
 *
 * SessionRail.tsx's formatUsd is two-decimal for a per-TURN total, where the
 * figure is larger by construction; this is deliberately not that function.
 */
export function formatStepCost(usd: number | null): string {
  return formatUsd(usd)
}

/**
 * totalKnownCost sums every non-null costUsd across a run's own step
 * attempts -- treating a still-null attempt as "not yet known" rather than
 * zero, the same discipline formatStepCost enforces one level up. Returns
 * null (never a fabricated 0) when NOT ONE attempt has reported a cost yet,
 * so a freshly-started run's header never claims "$0.00 so far".
 */
export function totalKnownCost(stepRuns: readonly WorkflowStepRun[]): number | null {
  let total = 0
  let sawAny = false
  for (const sr of stepRuns) {
    if (sr.costUsd !== null) {
      total += sr.costUsd
      sawAny = true
    }
  }
  return sawAny ? total : null
}

// -- step sequence + the edge actually taken -----------------------------

export interface StepRunAttempt {
  stepRun: WorkflowStepRun
  /** 1-based position among DISTINCT step definitions encountered so far, in execution order -- the closest REAL substitute for a step "name" this wire carries (WorkflowStepRun has no order/name field of its own, mirroring WorkflowCanvas.tsx's own identical "no fabricated label" reasoning for WorkflowStepDefinition). */
  stepIndex: number
  /** 1-based count of attempts of this exact step within the run so far, including this one -- 1 on a step's first attempt, 2+ on a needs_fix retry or a human revise re-run. */
  attemptNumber: number
}

/** buildStepRunSequence walks a run's own ordered step runs (oldest-first, per ListWorkflowStepRunsForRun's own doc comment) and assigns each attempt its step position and attempt count. Pure; the view renders one StepRunAttempt per card. */
export function buildStepRunSequence(stepRuns: readonly WorkflowStepRun[]): StepRunAttempt[] {
  const stepIndexOf = new Map<string, number>()
  const attemptCounts = new Map<string, number>()
  let nextIndex = 1
  return stepRuns.map((stepRun) => {
    let stepIndex = stepIndexOf.get(stepRun.stepDefinitionId)
    if (stepIndex === undefined) {
      stepIndex = nextIndex
      nextIndex += 1
      stepIndexOf.set(stepRun.stepDefinitionId, stepIndex)
    }
    const attemptNumber = (attemptCounts.get(stepRun.stepDefinitionId) ?? 0) + 1
    attemptCounts.set(stepRun.stepDefinitionId, attemptNumber)
    return { stepRun, stepIndex, attemptNumber }
  })
}

export type EdgeKind = 'advance' | 'retry' | 'revise'

export interface EdgeTaken {
  kind: EdgeKind
  onStatus: WorkflowStepRun['outcomeStatus']
}

/**
 * edgeToNext derives the edge actually taken from one attempt to the next
 * one in the same run's own sequence (§25.15: "the next step run's own
 * stepDefinitionId IS the edge target that was taken" -- never a stored
 * field). Three cases, checked in this order because a decision='revise'
 * attempt is defined to ALWAYS target the same step regardless of its own
 * outcomeStatus (§25.9: revise never consults workflow.NextStep at all), so
 * that check must win. Not because the same-step-id check below would
 * otherwise reach the right answer by luck -- it would reach the WRONG one:
 * a revise always re-runs the same step, so that check matches too, and
 * whichever runs first decides the label. Ordered the other way, every
 * human revise would be presented to the operator as an automatic retry,
 * which is a different event with a different cause:
 *   - decision === 'revise' on `from` -- a human's revise verdict; `to` is
 *     guaranteed the SAME stepDefinitionId (DispatchSameStepRevision never
 *     resolves via an edge), rendered distinctly from an automatic retry.
 *   - `to` shares `from`'s own stepDefinitionId -- an automatic needs_fix
 *     re-fire looping back to the same step (loopguard-permitted).
 *   - otherwise -- an ordinary advance to a different step.
 */
export function edgeToNext(from: WorkflowStepRun, to: WorkflowStepRun): EdgeTaken {
  if (from.decision === 'revise') {
    return { kind: 'revise', onStatus: from.outcomeStatus }
  }
  if (to.stepDefinitionId === from.stepDefinitionId) {
    return { kind: 'retry', onStatus: from.outcomeStatus }
  }
  return { kind: 'advance', onStatus: from.outcomeStatus }
}

/** edgeLabel renders one EdgeTaken as the connector caption between two step cards -- operator-facing, so no § citation and no internal identifier, only the real outcome status and the real destination step's own position. */
export function edgeLabel(edge: EdgeTaken, toStepIndex: number): string {
  const statusWord = edge.onStatus ? outcomeStatusLabel(edge.onStatus) : 'no outcome reported'
  switch (edge.kind) {
    case 'revise':
      return `revision requested → re-running step ${toStepIndex}`
    case 'retry':
      return `${statusWord} → retrying step ${toStepIndex}`
    case 'advance':
      return `${statusWord} → step ${toStepIndex}`
    default:
      return statusWord
  }
}

/**
 * decidableStepRun finds the ONE attempt (if any) actually parked
 * awaiting_decision -- the real, server-enforced gate on whether the decide
 * endpoint's guarded UPDATE can succeed at all (its own "AND status =
 * 'awaiting_decision'" clause). This is how the view tells §25.9's ordinary
 * HITL gate (a real decidable row exists; the RUN's own status usually
 * stays 'running') apart from the circuit-breaker/unrouted-outcome
 * escalation path (WorkflowRun.status flips to 'needs_review' directly,
 * with NO step run ever entering awaiting_decision) -- the two share
 * nothing on the wire except both being "stuck", and only one of them has
 * anything to decide.
 */
export function decidableStepRun(stepRuns: readonly WorkflowStepRun[]): WorkflowStepRun | null {
  return stepRuns.find((sr) => sr.status === 'awaiting_decision') ?? null
}

/**
 * canActOnWorkflowStep is this view's own CLIENT-SIDE approximation of
 * authz.ActionDecideWorkflowStep -- §25.11 states this is "the SAME row as
 * ActionApprovePlan" in so many words, and internal/adapters/inbound/httpapi/
 * workflowstepauthz.go's own canActOnWorkflowStep confirms it is a direct,
 * unmodified pass-through to that same matrix row (own/joined-aware:
 * admin/maintainer decide any run, a member only one they created or
 * joined, a viewer never). Rather than re-deriving that matrix a second
 * time client-side, this reuses planFormat.ts's own canActOnPlan under a
 * name that matches THIS screen's call site -- same function, same
 * conservative under-approximation caveat (documented in full on
 * canActOnPlan itself: "joined" has no client-visible signal anywhere in
 * this codebase, so this can wrongly HIDE the decide affordance from a
 * genuinely-joined member, never wrongly show it to someone the server
 * would refuse). The server re-checks the real matrix, including "joined",
 * independently on every decide call regardless of what this renders.
 */
export const canActOnWorkflowStep = canActOnPlan
