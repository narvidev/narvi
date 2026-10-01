import { describe, expect, it } from 'vitest'

import type { SessionActivity, WorkflowRun, WorkflowStepRun } from '@narvi/contracts/rest-dtos'

import { canActOnPlan } from '../planFormat'
import {
  buildStepRunSequence,
  canActOnWorkflowStep,
  decidableStepRun,
  decisionLabel,
  edgeLabel,
  edgeToNext,
  ESCALATION_CLOSED_NOTE,
  ESCALATION_OPEN_NOTE,
  escalationNotice,
  featuredRun,
  formatStepCost,
  isLiveRun,
  liveEscalationRunId,
  NEEDS_REVIEW_EXPLANATION,
  outcomeStatusLabel,
  outcomeStatusTone,
  runRefetchInterval,
  runStatusLabel,
  runStatusTone,
  stepRunStatusLabel,
  stepRunStatusTone,
  totalKnownCost,
  WORKFLOW_RUN_POLL_MS,
} from '../workflowRunFormat'

// Fixtures deliberately shaped exactly like what the server actually sends
// (workflowStepRunToDTO/workflowRunToDTO, internal/adapters/inbound/httpapi/
// workflowruns.go) -- including a NULL modelId/costUsd/outcomeStatus/
// decision by default, never an invented shape the wire never produces
// (this Step's own explicit fixture-fidelity requirement).
function baseRun(overrides: Partial<WorkflowRun> = {}): WorkflowRun {
  return {
    id: 'run-1',
    sessionId: 'sess-1',
    lane: 'request',
    workflowDefinitionId: 'def-1',
    definitionVersion: 1,
    status: 'running',
    createdAt: '2026-08-20T00:00:00Z',
    updatedAt: '2026-08-20T00:00:00Z',
    finishedAt: null,
    ...overrides,
  }
}

function baseStepRun(overrides: Partial<WorkflowStepRun> = {}): WorkflowStepRun {
  return {
    id: 'sr-1',
    workflowRunId: 'run-1',
    stepDefinitionId: 'step-a',
    turnId: null,
    status: 'running',
    outcomeStatus: null,
    outcomeSummary: null,
    decision: null,
    decidedAt: null,
    decidedBy: null,
    createdAt: '2026-08-20T00:00:00Z',
    finishedAt: null,
    modelId: null,
    costUsd: null,
    ...overrides,
  }
}

describe('runStatusTone / runStatusLabel', () => {
  it('maps every workflow_run_status value to a tone and a label', () => {
    expect(runStatusTone('running')).toBe('run')
    expect(runStatusTone('completed')).toBe('ok')
    expect(runStatusTone('needs_review')).toBe('warn')
    expect(runStatusTone('failed')).toBe('crit')
    expect(runStatusTone('cancelled')).toBe('neutral')

    expect(runStatusLabel('running')).toBe('running')
    expect(runStatusLabel('completed')).toBe('completed')
    expect(runStatusLabel('needs_review')).toBe('needs review')
    expect(runStatusLabel('failed')).toBe('failed')
    expect(runStatusLabel('cancelled')).toBe('cancelled')
  })
})

describe('stepRunStatusTone / stepRunStatusLabel', () => {
  it('maps every workflow_step_run_status value to a tone and a label', () => {
    expect(stepRunStatusTone('awaiting_decision')).toBe('warn')
    expect(stepRunStatusTone('running')).toBe('run')
    expect(stepRunStatusTone('completed')).toBe('ok')
    expect(stepRunStatusTone('failed')).toBe('crit')
    expect(stepRunStatusTone('cancelled')).toBe('neutral')

    expect(stepRunStatusLabel('awaiting_decision')).toBe('awaiting decision')
  })
})

describe('outcomeStatusTone / outcomeStatusLabel', () => {
  it('maps the 3-value outcome enum, reusing workflowFormat.ts own edge tone vocabulary', () => {
    expect(outcomeStatusTone('ok')).toBe('ok')
    expect(outcomeStatusTone('needs_fix')).toBe('warn')
    expect(outcomeStatusTone('blocked')).toBe('crit')

    expect(outcomeStatusLabel('ok')).toBe('ok')
    expect(outcomeStatusLabel('needs_fix')).toBe('needs fix')
    expect(outcomeStatusLabel('blocked')).toBe('blocked')
  })
})

describe('decisionLabel', () => {
  it('maps the 3-value decision enum to operator language', () => {
    expect(decisionLabel('approve')).toBe('approved')
    expect(decisionLabel('reject')).toBe('rejected')
    expect(decisionLabel('revise')).toBe('revision requested')
  })
})

describe('formatStepCost -- null must never render as a fabricated $0.00', () => {
  it('renders null as an em dash, never $0.00', () => {
    expect(formatStepCost(null)).toBe('—')
  })
  it('renders a genuine 0 as $0.00 -- a real, distinct value from "unknown yet"', () => {
    expect(formatStepCost(0)).toBe('$0.00')
  })
  it('renders a normal value to 2 decimal places', () => {
    expect(formatStepCost(1.5)).toBe('$1.50')
    expect(formatStepCost(12)).toBe('$12.00')
  })

  // The second collapse, and the one two decimals causes on its own: a
  // single agent step routinely costs a fraction of a cent, so at 2dp most
  // steps would read "$0.00" -- indistinguishable from free, which is the
  // exact failure the column behind this carries six decimals to avoid.
  it('keeps a sub-cent figure visible instead of rounding it into $0.00', () => {
    expect(formatStepCost(0.004)).toBe('$0.0040')
    expect(formatStepCost(0.0004)).toBe('$0.0004')
  })

  it('still separates a sub-cent cost from a genuine zero', () => {
    expect(formatStepCost(0.004)).not.toBe(formatStepCost(0))
  })
})

describe('totalKnownCost', () => {
  it('returns null (never a fabricated 0) when no attempt has reported a cost yet', () => {
    expect(totalKnownCost([])).toBeNull()
    expect(totalKnownCost([baseStepRun({ costUsd: null }), baseStepRun({ costUsd: null })])).toBeNull()
  })
  it('sums only the non-null attempts, treating a still-null one as "not yet known" rather than 0', () => {
    const total = totalKnownCost([baseStepRun({ costUsd: 1.5 }), baseStepRun({ costUsd: null }), baseStepRun({ costUsd: 2.25 })])
    expect(total).toBe(3.75)
  })
  it('a genuine 0 attempt still counts as "known" (sawAny), distinguishing it from an all-null run', () => {
    expect(totalKnownCost([baseStepRun({ costUsd: 0 })])).toBe(0)
  })
})

// Technical plan §43.20: needs_review is never left, the next turn starts
// a fresh run beside the parked one, and an escalation is live only while
// the status surface reports it (SessionActivity.escalation). The view
// features and re-reads a run by that rule, never by needs_review alone.
// Featuring "the newest running or needs_review" again makes the parked
// rows below fail; re-reading a parked run makes the isLiveRun rows fail.
describe('featuredRun', () => {
  const cases: { name: string; runs: WorkflowRun[]; live: string | null; want: string | null }[] = [
    { name: 'no runs', runs: [], live: null, want: null },
    {
      name: 'a parked needs_review followed by a completed run features the completed one',
      runs: [baseRun({ id: 'done', status: 'completed', createdAt: '2026-08-20T02:00:00Z' }), baseRun({ id: 'parked', status: 'needs_review', createdAt: '2026-08-20T01:00:00Z' })],
      live: null,
      want: 'done',
    },
    {
      name: 'a parked needs_review followed by a failed run features the failed one',
      runs: [baseRun({ id: 'later', status: 'failed' }), baseRun({ id: 'parked', status: 'needs_review' })],
      live: null,
      want: 'later',
    },
    {
      name: 'the live escalation of a custom workflow is featured',
      runs: [baseRun({ id: 'escalated', status: 'needs_review' }), baseRun({ id: 'older', status: 'completed' })],
      live: 'escalated',
      want: 'escalated',
    },
    {
      name: 'a newest needs_review the server no longer reports open (a turn since, or a built-in\'s) is passed over for the run before it',
      runs: [baseRun({ id: 'answered', status: 'needs_review' }), baseRun({ id: 'older', status: 'completed' })],
      live: null,
      want: 'older',
    },
    { name: 'every run parked and none live: nothing is featured', runs: [baseRun({ id: 'p1', status: 'needs_review' }), baseRun({ id: 'p2', status: 'needs_review' })], live: null, want: null },
    { name: 'a running run is featured over a newer finished one', runs: [baseRun({ id: 'r1', status: 'completed' }), baseRun({ id: 'r2', status: 'running' })], live: null, want: 'r2' },
    {
      name: 'nothing live: the FIRST run not parked (server-sorted newest-first), without re-sorting',
      runs: [baseRun({ id: 'r1', status: 'completed', createdAt: '2026-08-20T02:00:00Z' }), baseRun({ id: 'r2', status: 'failed', createdAt: '2026-08-20T03:00:00Z' })],
      live: null,
      want: 'r1',
    },
    { name: 'a live id that names no listed run changes nothing', runs: [baseRun({ id: 'r1', status: 'completed' })], live: 'gone', want: 'r1' },
  ]
  for (const c of cases) {
    it(c.name, () => {
      expect(featuredRun(c.runs, c.live)?.id ?? null).toBe(c.want)
    })
  }
})

describe('isLiveRun: whether the view re-reads a run on a timer', () => {
  const cases: { name: string; run: WorkflowRun; live: string | null; want: boolean }[] = [
    { name: 'a running run', run: baseRun({ id: 'r', status: 'running' }), live: null, want: true },
    { name: 'the live escalation', run: baseRun({ id: 'e', status: 'needs_review' }), live: 'e', want: true },
    { name: 'a parked escalation is never re-read', run: baseRun({ id: 'p', status: 'needs_review' }), live: null, want: false },
    { name: 'a parked escalation beside another live one', run: baseRun({ id: 'p', status: 'needs_review' }), live: 'e', want: false },
    { name: 'a completed run', run: baseRun({ id: 'c', status: 'completed' }), live: null, want: false },
    { name: 'a failed run', run: baseRun({ id: 'f', status: 'failed' }), live: null, want: false },
    { name: 'a cancelled run', run: baseRun({ id: 'x', status: 'cancelled' }), live: null, want: false },
  ]
  for (const c of cases) {
    it(c.name, () => {
      expect(isLiveRun(c.run, c.live)).toBe(c.want)
    })
  }
})

describe('runRefetchInterval: the run view re-reads the run it shows only while it can change', () => {
  const cases: { name: string; run: WorkflowRun | null; live: string | null; want: number | false }[] = [
    { name: 'no run shown', run: null, live: null, want: false },
    { name: 'a running run', run: baseRun({ id: 'r', status: 'running' }), live: null, want: WORKFLOW_RUN_POLL_MS },
    { name: 'the live escalation', run: baseRun({ id: 'e', status: 'needs_review' }), live: 'e', want: WORKFLOW_RUN_POLL_MS },
    { name: 'a parked escalation, featured or picked from the history, is not polled', run: baseRun({ id: 'p', status: 'needs_review' }), live: null, want: false },
    { name: 'a completed run', run: baseRun({ id: 'c', status: 'completed' }), live: null, want: false },
  ]
  for (const c of cases) {
    it(c.name, () => {
      expect(runRefetchInterval(c.run, c.live)).toBe(c.want)
    })
  }
})

describe('liveEscalationRunId', () => {
  const runID = '3f2a1c9e-8d4b-4e6f-9a1b-2c3d4e5f6a7b'
  const activity = (overrides: Partial<SessionActivity>): SessionActivity => ({
    sessionId: 'sess-1',
    activity: 'awaiting_approval',
    settled: true,
    pendingTurns: 0,
    inFlightTurn: null,
    awaiting: null,
    escalation: null,
    lastRun: null,
    sandboxStatus: 'ready',
    archived: false,
    suggestedDelaySeconds: 60,
    observedAt: '2026-10-01T09:00:00Z',
    ...overrides,
  })
  const since = '2026-10-01T08:58:00Z'
  const cases: { name: string; activity: SessionActivity | null | undefined; want: string | null }[] = [
    { name: 'no status read yet', activity: undefined, want: null },
    { name: 'none open', activity: activity({}), want: null },
    { name: 'the escalation the server reports', activity: activity({ escalation: { id: runID, since } }), want: runID },
    {
      name: 'an escalation open beside a plan awaiting approval: awaiting names the plan, escalation the run',
      activity: activity({ awaiting: { kind: 'plan', id: 'plan-1', since }, escalation: { id: runID, since } }),
      want: runID,
    },
    {
      name: 'a server that reports no escalation field: its awaiting, when it names the escalation',
      activity: (() => {
        const a: Partial<SessionActivity> = activity({ awaiting: { kind: 'workflow_escalation', id: runID, since } })
        delete a.escalation
        return a as SessionActivity
      })(),
      want: runID,
    },
    {
      name: 'escalation null wins over awaiting: the field is the answer',
      activity: activity({ awaiting: { kind: 'workflow_escalation', id: runID, since }, escalation: null }),
      want: null,
    },
  ]
  for (const c of cases) {
    it(c.name, () => {
      expect(liveEscalationRunId(c.activity)).toBe(c.want)
    })
  }
})

describe('escalationNotice', () => {
  it('a live escalation says it waits on a person, and what answers it', () => {
    const notice = escalationNotice(baseRun({ id: 'e', status: 'needs_review' }), 'e')
    expect(notice).toBe(`${NEEDS_REVIEW_EXPLANATION} ${ESCALATION_OPEN_NOTE}`)
  })
  it('a parked escalation says nothing waits on it any more', () => {
    const notice = escalationNotice(baseRun({ id: 'p', status: 'needs_review' }), null)
    expect(notice).toBe(`${NEEDS_REVIEW_EXPLANATION} ${ESCALATION_CLOSED_NOTE}`)
  })
  it('any other run has none', () => {
    expect(escalationNotice(baseRun({ id: 'r', status: 'running' }), 'r')).toBeNull()
    expect(escalationNotice(baseRun({ id: 'c', status: 'completed' }), null)).toBeNull()
  })
})

describe('buildStepRunSequence', () => {
  it('assigns stepIndex 1/attemptNumber 1 to a run\'s single attempt', () => {
    const stepRun = baseStepRun({ id: 'sr1', stepDefinitionId: 'a' })
    const seq = buildStepRunSequence([stepRun])
    expect(seq).toEqual([{ stepRun, stepIndex: 1, attemptNumber: 1 }])
  })

  it('assigns increasing stepIndex to distinct steps encountered in order, each at attempt 1', () => {
    const seq = buildStepRunSequence([baseStepRun({ id: 'sr1', stepDefinitionId: 'a' }), baseStepRun({ id: 'sr2', stepDefinitionId: 'b' })])
    expect(seq.map((s) => [s.stepIndex, s.attemptNumber])).toEqual([
      [1, 1],
      [2, 1],
    ])
  })

  it('an immediate same-step retry increments attemptNumber, keeping the SAME stepIndex', () => {
    const seq = buildStepRunSequence([baseStepRun({ id: 'sr1', stepDefinitionId: 'a' }), baseStepRun({ id: 'sr2', stepDefinitionId: 'a' })])
    expect(seq.map((s) => [s.stepIndex, s.attemptNumber])).toEqual([
      [1, 1],
      [1, 2],
    ])
  })

  it('a loop-back retry (a -> b -> a again) keeps step a\'s ORIGINAL stepIndex rather than assigning it a new one, and counts its second attempt correctly', () => {
    const seq = buildStepRunSequence([baseStepRun({ id: 'sr1', stepDefinitionId: 'a' }), baseStepRun({ id: 'sr2', stepDefinitionId: 'b' }), baseStepRun({ id: 'sr3', stepDefinitionId: 'a' })])
    expect(seq.map((s) => [s.stepIndex, s.attemptNumber])).toEqual([
      [1, 1],
      [2, 1],
      [1, 2],
    ])
  })
})

describe('edgeToNext', () => {
  it('is "retry" when the next attempt targets the SAME step and there was no revise decision', () => {
    const from = baseStepRun({ stepDefinitionId: 'a', outcomeStatus: 'needs_fix' })
    const to = baseStepRun({ stepDefinitionId: 'a' })
    expect(edgeToNext(from, to)).toEqual({ kind: 'retry', onStatus: 'needs_fix' })
  })

  it('is "advance" when the next attempt targets a DIFFERENT step', () => {
    const from = baseStepRun({ stepDefinitionId: 'a', outcomeStatus: 'ok' })
    const to = baseStepRun({ stepDefinitionId: 'b' })
    expect(edgeToNext(from, to)).toEqual({ kind: 'advance', onStatus: 'ok' })
  })

  it('is "revise" whenever `from.decision` is revise, EVEN THOUGH the target is the same step -- decision must win over the same-step-id check, not just happen to agree with it', () => {
    const from = baseStepRun({ stepDefinitionId: 'a', outcomeStatus: 'ok', decision: 'revise' })
    const to = baseStepRun({ stepDefinitionId: 'a' })
    expect(edgeToNext(from, to)).toEqual({ kind: 'revise', onStatus: 'ok' })
  })

  it('preserves a null onStatus rather than fabricating one', () => {
    const from = baseStepRun({ stepDefinitionId: 'a', outcomeStatus: null })
    const to = baseStepRun({ stepDefinitionId: 'b' })
    expect(edgeToNext(from, to).onStatus).toBeNull()
  })
})

describe('edgeLabel', () => {
  it('renders each edge kind distinctly, naming the real destination step position', () => {
    expect(edgeLabel({ kind: 'advance', onStatus: 'ok' }, 2)).toBe('ok → step 2')
    expect(edgeLabel({ kind: 'retry', onStatus: 'needs_fix' }, 1)).toBe('needs fix → retrying step 1')
    expect(edgeLabel({ kind: 'revise', onStatus: 'ok' }, 1)).toBe('revision requested → re-running step 1')
  })
  it('never fabricates an outcome word when onStatus is null', () => {
    expect(edgeLabel({ kind: 'advance', onStatus: null }, 2)).toBe('no outcome reported → step 2')
  })
})

describe('decidableStepRun', () => {
  it('returns null when no attempt is awaiting_decision', () => {
    expect(decidableStepRun([baseStepRun({ status: 'completed' }), baseStepRun({ status: 'running' })])).toBeNull()
  })
  it('finds the one attempt actually parked awaiting_decision', () => {
    const target = baseStepRun({ id: 'sr2', status: 'awaiting_decision' })
    expect(decidableStepRun([baseStepRun({ id: 'sr1', status: 'completed' }), target])).toBe(target)
  })
})

describe('NEEDS_REVIEW_EXPLANATION', () => {
  it('is operator-facing copy: no § section citation, since an operator has no access to the technical plan', () => {
    expect(NEEDS_REVIEW_EXPLANATION).not.toMatch(/§/)
  })
  it('does not imply a retry action exists here', () => {
    expect(NEEDS_REVIEW_EXPLANATION.toLowerCase()).toContain('no retry')
  })
})

describe('canActOnWorkflowStep', () => {
  it('is the SAME function as canActOnPlan -- §25.11 states ActionDecideWorkflowStep is the identical matrix row as ActionApprovePlan, so this must be a reuse, never an independently-maintained copy that could silently drift', () => {
    expect(canActOnWorkflowStep).toBe(canActOnPlan)
  })
})
