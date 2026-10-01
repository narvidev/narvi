// workflowRunsViewWiring.test.tsx -- what WorkflowRunsView reads, polls
// and re-reads, rendered whole: the session's status (GET
// /api/sessions/:id/status) decides which run is featured and polled, and
// what its banner says (technical plan §43.20), and every verdict re-reads
// that status. workflowRunFormat.test.ts proves the rules; this proves the
// view hands them the status it reads.
//
// The suite runs in plain Node (vitest.config.ts: no DOM). useQuery and
// useMutation are wrapped, never replaced -- the real hooks still run
// against a QueryClient whose cache holds the server's answers, so the
// static render shows that data -- and each call's options are recorded on
// the way in: the options say what is polled, and a verdict's onSuccess
// says what it re-reads.
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactNode } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider, type QueryKey } from '@tanstack/react-query'
import type * as ReactQuery from '@tanstack/react-query'
import type * as ReactRouter from '@tanstack/react-router'

import type { Session, SessionActivity, WorkflowRun, WorkflowRunDetail, WorkflowStepRun } from '@narvi/contracts/rest-dtos'

import { sessionListQueryKeys, sessionQueryKeys, workflowRunQueryKeys } from '../../api/queryKeys'
import { ESCALATION_CLOSED_NOTE, ESCALATION_OPEN_NOTE, WORKFLOW_RUN_POLL_MS } from '../workflowRunFormat'
import { ReviseBox, WorkflowRunsView } from '../WorkflowRunsView'

// The options of every useQuery and useMutation call, in call order.
const captured = vi.hoisted(() => ({ queries: [] as unknown[], mutations: [] as unknown[] }))

vi.mock('@tanstack/react-query', async (importOriginal) => {
  const actual = await importOriginal<typeof ReactQuery>()
  const useQuery = ((...args: Parameters<typeof actual.useQuery>) => {
    captured.queries.push(args[0])
    return actual.useQuery(...args)
  }) as typeof actual.useQuery
  const useMutation = ((...args: Parameters<typeof actual.useMutation>) => {
    captured.mutations.push(args[0])
    return actual.useMutation(...args)
  }) as typeof actual.useMutation
  return { ...actual, useQuery, useMutation }
})

// The view links back to the session; a static render has no router.
vi.mock('@tanstack/react-router', async (importOriginal) => {
  const actual = await importOriginal<typeof ReactRouter>()
  const { createElement } = await import('react')
  return { ...actual, Link: ({ children }: { children?: ReactNode }) => createElement('a', null, children) }
})

afterEach(() => {
  captured.queries.length = 0
  captured.mutations.length = 0
})

type CapturedQuery = { queryKey: QueryKey; refetchInterval?: unknown }
type CapturedMutation = { onSuccess?: (...args: unknown[]) => unknown }

const SESSION = 'sess-1'

function session(): Session {
  return {
    id: SESSION,
    title: 'A session',
    status: 'active',
    failureReason: null,
    archived: false,
    spawnSource: 'web',
    createdBy: null,
    createdAt: '2026-10-01T08:00:00Z',
    updatedAt: '2026-10-01T08:00:00Z',
    repos: [],
    sandboxStatus: null,
    buildModelId: null,
    buildEffort: null,
  }
}

function run(id: string, status: WorkflowRun['status'], createdAt: string): WorkflowRun {
  return {
    id,
    sessionId: SESSION,
    lane: 'request',
    workflowDefinitionId: 'def-1',
    definitionVersion: 1,
    status,
    createdAt,
    updatedAt: createdAt,
    finishedAt: null,
  }
}

function stepRun(runId: string, status: WorkflowStepRun['status']): WorkflowStepRun {
  return {
    id: `${runId}-sr-1`,
    workflowRunId: runId,
    stepDefinitionId: 'step-a',
    turnId: 'turn-1',
    status,
    outcomeStatus: status === 'awaiting_decision' ? 'ok' : 'needs_fix',
    outcomeSummary: null,
    decision: null,
    decidedAt: null,
    decidedBy: null,
    createdAt: '2026-10-01T08:30:00Z',
    finishedAt: null,
    modelId: null,
    costUsd: null,
  }
}

function activity(escalationRunId: string | null): SessionActivity {
  return {
    sessionId: SESSION,
    activity: 'awaiting_approval',
    settled: true,
    pendingTurns: 0,
    inFlightTurn: null,
    awaiting: null,
    escalation: escalationRunId === null ? null : { id: escalationRunId, since: '2026-10-01T09:00:00Z' },
    lastRun: null,
    sandboxStatus: 'ready',
    archived: false,
    suggestedDelaySeconds: 60,
    observedAt: '2026-10-01T09:01:00Z',
  }
}

// renderView renders the view over a cache holding the server's answers:
// the session, its status, its runs and each run's attempts.
function renderView(runs: WorkflowRun[], escalationRunId: string | null, stepRuns: Record<string, WorkflowStepRun[]>): { html: string; queryClient: QueryClient } {
  const queryClient = new QueryClient()
  queryClient.setQueryData(sessionQueryKeys.detail(SESSION), session())
  queryClient.setQueryData(sessionQueryKeys.activity(SESSION), activity(escalationRunId))
  queryClient.setQueryData(workflowRunQueryKeys.listForSession(SESSION), { runs })
  for (const r of runs) {
    const detail: WorkflowRunDetail = { run: r, stepRuns: stepRuns[r.id] ?? [] }
    queryClient.setQueryData(workflowRunQueryKeys.detail(r.id), detail)
  }
  const html = renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <WorkflowRunsView sessionId={SESSION} />
    </QueryClientProvider>,
  )
  return { html, queryClient }
}

function queryFor(key: QueryKey): CapturedQuery {
  const found = (captured.queries as CapturedQuery[]).find((q) => JSON.stringify(q.queryKey) === JSON.stringify(key))
  if (found === undefined) throw new Error(`no useQuery call with key ${JSON.stringify(key)}`)
  return found
}

// The run the view features is the one whose attempts it reads.
function featuredRunId(): string | null {
  const detail = (captured.queries as CapturedQuery[]).find((q) => q.queryKey[0] === 'workflow-run')
  if (detail === undefined) throw new Error('no run detail query')
  const id = detail.queryKey[1]
  return id === '' ? null : String(id)
}

describe('WorkflowRunsView: the escalation the session status reports', () => {
  // R escalated last (the newest run); Z finished before it.
  const escalated = run('R', 'needs_review', '2026-10-01T09:00:00Z')
  const finished = run('Z', 'completed', '2026-10-01T08:00:00Z')
  const parkedAttempts = { R: [stepRun('R', 'completed')], Z: [stepRun('Z', 'completed')] }

  const cases: {
    name: string
    runs: WorkflowRun[]
    escalation: string | null
    featured: string | null
    poll: number | false
    note: string | null
  }[] = [
    { name: 'the live escalation is featured, re-read on a timer, and says it waits on a person', runs: [escalated, finished], escalation: 'R', featured: 'R', poll: WORKFLOW_RUN_POLL_MS, note: ESCALATION_OPEN_NOTE },
    { name: 'the only run, the live escalation, likewise', runs: [escalated], escalation: 'R', featured: 'R', poll: WORKFLOW_RUN_POLL_MS, note: ESCALATION_OPEN_NOTE },
    { name: 'an escalation the status no longer reports is passed over for the run before it', runs: [escalated, finished], escalation: null, featured: 'Z', poll: false, note: null },
    { name: 'the only run, a parked escalation: nothing is featured', runs: [escalated], escalation: null, featured: null, poll: false, note: null },
  ]
  for (const c of cases) {
    it(c.name, () => {
      const { html } = renderView(c.runs, c.escalation, parkedAttempts)
      expect(featuredRunId()).toBe(c.featured)
      if (c.featured !== null) {
        expect(queryFor(workflowRunQueryKeys.detail(c.featured)).refetchInterval).toBe(c.poll)
      } else {
        expect(html).toContain('No run is in progress or waiting on a person.')
      }
      if (c.note !== null) expect(html).toContain(c.note)
      expect(html).not.toContain(c.note === ESCALATION_OPEN_NOTE ? ESCALATION_CLOSED_NOTE : ESCALATION_OPEN_NOTE)
    })
  }

  it('reads the session status on the run list cadence', () => {
    renderView([escalated, finished], 'R', parkedAttempts)
    expect(queryFor(sessionQueryKeys.activity(SESSION)).refetchInterval).toBe(WORKFLOW_RUN_POLL_MS)
    expect(queryFor(workflowRunQueryKeys.listForSession(SESSION)).refetchInterval).toBe(WORKFLOW_RUN_POLL_MS)
  })
})

// Every verdict re-reads the session status as well as the run: an approve
// can escalate the run inside the request, and the view features and
// polls by the status's `escalation`.
describe('WorkflowRunsView: what a verdict re-reads', () => {
  const wantKeys: QueryKey[] = [
    workflowRunQueryKeys.detail('G'),
    workflowRunQueryKeys.listForSession(SESSION),
    sessionQueryKeys.detail(SESSION),
    sessionQueryKeys.activity(SESSION),
    sessionListQueryKeys.list('mine'),
    sessionListQueryKeys.list('all'),
  ]

  function invalidatedBy(mutation: CapturedMutation, queryClient: QueryClient): string[] {
    const invalidate = vi.spyOn(queryClient, 'invalidateQueries').mockResolvedValue(undefined)
    void mutation.onSuccess?.(undefined, undefined, undefined, undefined)
    const keys = invalidate.mock.calls.map(([filters]) => JSON.stringify((filters as { queryKey?: QueryKey } | undefined)?.queryKey))
    invalidate.mockRestore()
    return keys.sort()
  }

  it('approve and reject, at the decision gate', () => {
    const gated = run('G', 'running', '2026-10-01T09:00:00Z')
    const { html, queryClient } = renderView([gated], null, { G: [stepRun('G', 'awaiting_decision')] })
    expect(html).toContain('Approve')
    const mutations = captured.mutations as CapturedMutation[]
    expect(mutations).toHaveLength(2)
    for (const mutation of mutations) {
      expect(invalidatedBy(mutation, queryClient)).toEqual(wantKeys.map((k) => JSON.stringify(k)).sort())
    }
  })

  it('revise', () => {
    const queryClient = new QueryClient()
    let done = 0
    renderToStaticMarkup(
      <QueryClientProvider client={queryClient}>
        <ReviseBox runId="G" stepRunId="G-sr-1" sessionId={SESSION} onDone={() => done++} />
      </QueryClientProvider>,
    )
    const mutations = captured.mutations as CapturedMutation[]
    expect(mutations).toHaveLength(1)
    expect(invalidatedBy(mutations[0]!, queryClient)).toEqual(wantKeys.map((k) => JSON.stringify(k)).sort())
    expect(done).toBe(1)
  })
})
