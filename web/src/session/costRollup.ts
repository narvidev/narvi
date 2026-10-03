// costRollup.ts -- §12.2's own "cost incl. sub-task roll-up" (§7.1: the
// OpenCode adapter's own turnState.spentUSD sums every step_finish this
// turn observes, "main lane and every sub-task alike", but that running
// total is scoped to the adapter's own in-memory turn state and NEVER
// persisted or put on the wire -- §7.1's own explicit words: "never itself
// transmitted over the sandbox WS, and never visible outside the one
// sandbox-agent process running that turn"). This module reconstructs the
// SAME roll-up client-side, from the SAME per-step_finish cost figures
// that DO reach the client (§7.1: "the individual per-step cost figures
// still flow to the control plane exactly as before... on each
// step_finish event", unchanged).
//
// Deliberately NOT built from timelineModel.ts's own per-step cost:
// TimelineModel skips every subTaskId-tagged step_finish on purpose
// (timelineModel.ts's own top comment: "COLLAPSED sub-lanes carry no
// tool-call content of their own... every event that carries a non-null
// subTaskId is routed away from the main lane"), which is correct for
// rendering the TIMELINE and wrong for a cost total that must include
// sub-task spend. This module reads the RAW event log instead, counting
// every step_finish regardless of subTaskId -- the same event, read for a
// different question.
//
// It reads nothing but step_finish rows, and never their messageId, so it
// is only as good as the rows the server stores. A real step_finish carries
// its assistant message's id, which that message's step_start stores first;
// the server stores it under a key of its own, derived from its stepId
// (technical plan §6.1). Before it did, no real session had a step_finish
// row, and this rollup read "—" for every turn and session.
import type { EventEnvelope } from '../ws/types'
import { asStepFinish, asTurnEnd } from './eventPayloads'

export interface CostRollup {
  /** Null while no step_finish carrying a usd figure has been seen for the CURRENT (still-open, or most recently closed) turn -- distinct from 0, which would claim "measured, and free". */
  turnUsd: number | null
  turnInputTokens: number
  turnOutputTokens: number
  /** Same null-vs-0 distinction as turnUsd, for the whole session. */
  sessionUsd: number | null
  sessionInputTokens: number
  sessionOutputTokens: number
}

export function buildCostRollup(events: readonly EventEnvelope[]): CostRollup {
  let sessionUsd = 0
  let sessionHasCost = false
  let sessionInputTokens = 0
  let sessionOutputTokens = 0
  let turnUsd = 0
  let turnHasCost = false
  let turnInputTokens = 0
  let turnOutputTokens = 0

  for (const event of events) {
    const stepFinish = asStepFinish(event)
    if (stepFinish !== null) {
      sessionInputTokens += stepFinish.cost.tokens.input
      sessionOutputTokens += stepFinish.cost.tokens.output
      turnInputTokens += stepFinish.cost.tokens.input
      turnOutputTokens += stepFinish.cost.tokens.output
      if (stepFinish.cost.usd != null) {
        sessionUsd += stepFinish.cost.usd
        sessionHasCost = true
        turnUsd += stepFinish.cost.usd
        turnHasCost = true
      }
      continue
    }

    // A turn boundary (§3.3) resets only the CURRENT-turn accumulator --
    // the session-wide total keeps growing across every turn this session
    // has ever run, mirroring SessionHeader.tsx's own existing totalCost
    // loop over every turn in timelineModel. A sub-task-tagged
    // execution_complete (subTaskId non-null) never closes the MAIN
    // turn -- excluded here for the identical reason timelineModel.ts's
    // own turn-end branch excludes it from turn-outcome handling. A turn
    // the control plane ended itself, with its synthetic
    // execution_complete, ends here too (asTurnEnd).
    const turnEnd = asTurnEnd(event)
    if (turnEnd !== null && !turnEnd.subTaskId) {
      turnUsd = 0
      turnHasCost = false
      turnInputTokens = 0
      turnOutputTokens = 0
      continue
    }
  }

  return {
    turnUsd: turnHasCost ? turnUsd : null,
    turnInputTokens,
    turnOutputTokens,
    sessionUsd: sessionHasCost ? sessionUsd : null,
    sessionInputTokens,
    sessionOutputTokens,
  }
}
