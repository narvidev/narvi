// WorkflowRunsView.tsx -- the run view + human decision gate a lane
// workflow's execution needs (the previous Step, WorkflowEditorView.tsx,
// only authors the graph; this Step watches it run and unblocks it).
//
// # What this screen renders, and from where
//
// GET /api/sessions/:id/workflow-runs (§25.10) lists this session's own
// runs, newest first; GET /api/workflow-runs/:runId returns ONE run WITH
// its ordered step runs (oldest-first, one row per ATTEMPT -- a retry or a
// human revise re-run is a NEW row, never an update-in-place, §25.5/§25.10).
// featuredRun (workflowRunFormat.ts) picks the run to show in the main
// panel: the newest live one -- running, or the escalation the session's
// status still reports open (GET /api/sessions/:id/status, `escalation`:
// the server's rule, technical plan §43.20, never re-derived here) -- else
// the newest run that is not a parked escalation. A run parked at
// needs_review that the server no longer reports open is listed in the
// rail, never featured, and never re-read on a timer: needs_review is
// never left, and the session's next turn starts a fresh run beside it.
// Every run is listed in the rail, and selectable there.
//
// # The edge actually taken is rendered, never fetched
//
// §25.15 is explicit that there is no such field on the wire: the
// connector caption between two step cards (edgeToNext/edgeLabel,
// workflowRunFormat.ts) is derived purely from the ordered step runs this
// screen already has in hand.
//
// # The human decision gate mirrors plan mode, on purpose
//
// §25.9 reuses plan mode's own cross-channel delivery mechanism and its own
// three-verdict vocabulary; decisionLabel/DecisionGate below mirror
// PlanModeView.tsx's own ApprovalBar/ReviseBox shape closely enough to feel
// like the same screen, not a new invention: approve/reject are one-click,
// revise is a mandatory non-empty text box, and a verdict is sent to the
// real endpoint unconditionally regardless of what canActOnWorkflowStep
// (workflowRunFormat.ts, a CLIENT-SIDE approximation only -- see that
// function's own doc comment) chose to render enabled. The one behavior
// this gate must never offer, per this Step's own spec: a run parked at
// 'needs_review' with NO attempt awaiting a decision (the circuit-breaker/
// unrouted-outcome escalation path, §25.9) gets an explanatory banner
// (escalationNotice: NEEDS_REVIEW_EXPLANATION, then whether the escalation
// still waits on a person, who answers it by sending the session new work)
// and no button of any kind -- see decidableStepRun's own doc comment for
// how this screen tells that path apart from the ordinary HITL gate.
//
// # Rendering safety
//
// stepRun.outcomeSummary (an agent's own advisory free text, §25.6),
// stepRun.modelId (an opaque, Narvi-unvalidated provider/model passthrough
// string, §25.7) and decidedBy/error messages are all attacker-reachable in
// the same sense PlanModeView.tsx's own top comment describes for
// plan.content: rendered through the SAME plain-text T/truncateForDisplay
// path MembersPanel.tsx established, never dangerouslySetInnerHTML. Both
// StepRunCard and EdgeConnector are exported for direct render-safety
// testing, mirroring PlanCard/DefinitionRow/WorkflowStepNode's own
// established precedent.
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'

import type { WorkflowRun, WorkflowStepRun } from '@narvi/contracts/rest-dtos'

import { decideWorkflowStep, getSession, getSessionActivity, getWorkflowRun, listSessionWorkflowRuns } from '../api/endpoints'
import { ApiError } from '../api/http'
import { sessionListQueryKeys, sessionQueryKeys, workflowRunQueryKeys } from '../api/queryKeys'
import { meQueryOptions } from '../auth/session'
import { modelLabel } from './planFormat'
import { truncateForDisplay } from './textSafety'
import { laneLabel } from './workflowFormat'
import type { EdgeTaken, StepRunAttempt } from './workflowRunFormat'
import {
  buildStepRunSequence,
  canActOnWorkflowStep,
  decidableStepRun,
  decisionLabel,
  edgeLabel,
  edgeToNext,
  escalationNotice,
  featuredRun,
  formatStepCost,
  liveEscalationRunId,
  outcomeStatusLabel,
  outcomeStatusTone,
  runRefetchInterval,
  runStatusLabel,
  runStatusTone,
  stepRunStatusLabel,
  stepRunStatusTone,
  totalKnownCost,
  WORKFLOW_RUN_POLL_MS,
} from './workflowRunFormat'

const MAX_FIELD_CHARS = 2000

function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_FIELD_CHARS)}</>
}

/** StepRunCard renders one attempt's own card. Exported for direct render-safety testing: outcomeSummary/modelId/decidedBy are all free text that must render as plain text only, never markup (this file's own top doc comment). */
export function StepRunCard({ attempt }: { attempt: StepRunAttempt }) {
  const { stepRun, stepIndex, attemptNumber } = attempt
  return (
    <div className="card">
      <div className="who">
        <span className="avatar b">{stepIndex}</span>
        <b>
          Step {stepIndex}
          {attemptNumber > 1 ? ` · attempt ${attemptNumber}` : ''}
        </b>
        <time>{new Date(stepRun.createdAt).toLocaleString()}</time>
        <span className={`chip ${stepRunStatusTone(stepRun.status)}`} style={{ marginLeft: 'auto' }}>
          <span className="dot" />
          {stepRunStatusLabel(stepRun.status)}
        </span>
      </div>
      <dl className="kv">
        <dt>model</dt>
        <dd>
          <T text={modelLabel(stepRun.modelId)} />
        </dd>
        <dt>cost</dt>
        <dd>{formatStepCost(stepRun.costUsd)}</dd>
        {stepRun.outcomeStatus && (
          <>
            <dt>outcome</dt>
            <dd>
              <span className={`chip ${outcomeStatusTone(stepRun.outcomeStatus)}`}>
                <span className="dot" />
                {outcomeStatusLabel(stepRun.outcomeStatus)}
              </span>
            </dd>
          </>
        )}
      </dl>
      {stepRun.outcomeSummary && (
        <p className="plan-content">
          <T text={stepRun.outcomeSummary} />
        </p>
      )}
      {stepRun.decision && (
        <div className="verdict-foot">
          <span>
            {decisionLabel(stepRun.decision)}
            {stepRun.decidedBy ? (
              <>
                {' by '}
                <T text={stepRun.decidedBy} />
              </>
            ) : (
              ''
            )}
            {stepRun.decidedAt ? ` · ${new Date(stepRun.decidedAt).toLocaleString()}` : ''}
          </span>
        </div>
      )}
    </div>
  )
}

/** EdgeConnector renders the edge actually taken between two consecutive attempts -- exported for direct render-safety testing alongside StepRunCard, though edgeLabel's own inputs are all closed-enum/numeric, never free text. */
export function EdgeConnector({ edge, toStepIndex }: { edge: EdgeTaken; toStepIndex: number }) {
  return <div className="resumed-note">{edgeLabel(edge, toStepIndex)}</div>
}

/**
 * invalidateAfterDecision re-reads everything a verdict on a workflow step
 * can change, for all three verdicts alike: the run and the session's run
 * list, the session and the session lists, and the session's status --
 * whose `escalation` decides which run this view features and re-reads, and
 * what its banner says (featuredRun, runRefetchInterval, escalationNotice).
 * An approve can escalate the run inside the request (an outcome with no
 * next step, or a retry loop's guard tripping, §25.9), making it the live
 * escalation (technical plan §43.20); without the status, the view would
 * read the escalation as it stood before the verdict until the next poll,
 * and say that nothing waits on a person who is being waited on.
 */
function invalidateAfterDecision(queryClient: QueryClient, sessionId: string, runId: string): void {
  for (const queryKey of [
    workflowRunQueryKeys.detail(runId),
    workflowRunQueryKeys.listForSession(sessionId),
    sessionQueryKeys.detail(sessionId),
    sessionQueryKeys.activity(sessionId),
    sessionListQueryKeys.list('mine'),
    sessionListQueryKeys.list('all'),
  ]) {
    void queryClient.invalidateQueries({ queryKey })
  }
}

/** ReviseBox is the revise verdict's own text box. Exported for direct wiring testing: what its verdict re-reads once it lands (invalidateAfterDecision). */
export function ReviseBox({ runId, stepRunId, sessionId, onDone }: { runId: string; stepRunId: string; sessionId: string; onDone: () => void }) {
  const queryClient = useQueryClient()
  const [text, setText] = useState('')
  const reviseMutation = useMutation({
    mutationFn: () => decideWorkflowStep(runId, stepRunId, { verdict: 'revise', text }),
    onSuccess: () => {
      setText('')
      onDone()
      invalidateAfterDecision(queryClient, sessionId, runId)
    },
  })

  return (
    <div className="btnrow" style={{ flexDirection: 'column', alignItems: 'stretch', width: '100%' }}>
      <textarea
        className="btn"
        style={{ resize: 'vertical', minHeight: 70, textAlign: 'left' }}
        placeholder="What should this step do differently?"
        value={text}
        onChange={(e) => setText(e.target.value)}
      />
      <div className="btnrow">
        <button type="button" className="btn primary" disabled={reviseMutation.isPending || text.trim().length === 0} onClick={() => reviseMutation.mutate()}>
          {reviseMutation.isPending ? 'Sending…' : 'Send revision'}
        </button>
        <button type="button" className="btn" onClick={onDone}>
          Cancel
        </button>
      </div>
      {reviseMutation.isError && (
        <p className="sidebar-notice" role="alert">
          {reviseMutation.error instanceof ApiError && reviseMutation.error.status === 409
            ? 'This step was already decided (or this is a stale attempt) -- someone else acted on it first.'
            : reviseMutation.error instanceof ApiError && reviseMutation.error.status === 403
              ? "You're not authorized to decide this workflow step."
              : 'Sending the revision failed. Try again.'}
        </p>
      )}
    </div>
  )
}

/** DecisionGate renders approve/reject/revise for ONE attempt currently awaiting_decision -- §25.9's HITL gate, the same three verdicts as PlanModeView.tsx's own ApprovalBar. revise ALWAYS re-runs this same step with the human's text as an extra instruction (never a structured substitution, decideWorkflowStep's own doc comment) -- there is no "approve with edits" affordance because there is no such verdict on the wire. */
function DecisionGate({ sessionId, runId, stepRun, canAct }: { sessionId: string; runId: string; stepRun: WorkflowStepRun; canAct: boolean }) {
  const queryClient = useQueryClient()
  const [revising, setRevising] = useState(false)

  const approveMutation = useMutation({
    mutationFn: () => decideWorkflowStep(runId, stepRun.id, { verdict: 'approve', text: null }),
    onSuccess: () => invalidateAfterDecision(queryClient, sessionId, runId),
  })
  const rejectMutation = useMutation({
    mutationFn: () => decideWorkflowStep(runId, stepRun.id, { verdict: 'reject', text: null }),
    onSuccess: () => invalidateAfterDecision(queryClient, sessionId, runId),
  })

  const pending = approveMutation.isPending || rejectMutation.isPending

  return (
    <div className="approvalbar">
      {revising ? (
        <ReviseBox runId={runId} stepRunId={stepRun.id} sessionId={sessionId} onDone={() => setRevising(false)} />
      ) : (
        <>
          {/* Disabled, not unmounted, when !canAct -- mirrors PlanModeView.tsx's
              own ApprovalBar exactly: canActOnWorkflowStep is a conservative
              client-side under-approximation (it cannot see a "joined"
              participants row), so a disabled-but-visible control with an
              honest tooltip is more truthful than hiding it, and the server
              re-checks the real matrix independently either way. */}
          <button
            type="button"
            className="btn primary"
            disabled={!canAct || pending}
            title={!canAct ? "You're not authorized to decide this step (create the session, join it, or ask an admin/maintainer)" : undefined}
            onClick={() => approveMutation.mutate()}
          >
            {approveMutation.isPending ? 'Approving…' : 'Approve'}
          </button>
          <button type="button" className="btn" disabled={!canAct || pending} onClick={() => setRevising(true)}>
            Revise
          </button>
          <button type="button" className="btn danger" disabled={!canAct || pending} onClick={() => rejectMutation.mutate()}>
            {rejectMutation.isPending ? 'Rejecting…' : 'Reject'}
          </button>
          <span className="channels">approve continues the run · reject ends it · revise re-runs this step with your instructions</span>
        </>
      )}
      {(approveMutation.isError || rejectMutation.isError) && (
        <p className="sidebar-notice" role="alert" style={{ width: '100%' }}>
          {(approveMutation.error instanceof ApiError && approveMutation.error.status === 403) || (rejectMutation.error instanceof ApiError && rejectMutation.error.status === 403)
            ? "The server refused this action: you're not authorized to decide this workflow step."
            : (approveMutation.error instanceof ApiError && approveMutation.error.status === 409) || (rejectMutation.error instanceof ApiError && rejectMutation.error.status === 409)
              ? 'This step was already decided by someone else (or this is a stale attempt).'
              : 'That action failed. Try again.'}
        </p>
      )}
    </div>
  )
}


function RunHistoryPanel({ runs, featuredId, onSelect }: { runs: WorkflowRun[]; featuredId: string | null; onSelect: (runId: string) => void }) {
  if (runs.length === 0) return null
  return (
    <div>
      <h3>Run history</h3>
      {/*
        Selectable, not a read-only list. Which run the main panel opens on
        is a rule -- the newest live run, else the newest one not parked --
        and an operator may still want any other: a parked escalation, an
        older finished run. Selecting one shows it; it is re-read on a timer
        only while it can still change (isLiveRun).
      */}
      <ul className="transitions">
        {runs.map((r) => (
          <li key={r.id} className={r.id === featuredId ? 'now' : undefined}>
            <button type="button" className="linklike" onClick={() => onSelect(r.id)} aria-current={r.id === featuredId ? 'true' : undefined}>
              <b>
                {laneLabel(r.lane)} · {runStatusLabel(r.status)}
              </b>{' '}
              · {new Date(r.createdAt).toLocaleString()}
            </button>
          </li>
        ))}
      </ul>
    </div>
  )
}

export function WorkflowRunsView({ sessionId }: { sessionId: string }) {
  const meQuery = useQuery(meQueryOptions)
  const sessionQuery = useQuery({ queryKey: sessionQueryKeys.detail(sessionId), queryFn: ({ signal }) => getSession(sessionId, signal) })
  // The session's status says which escalation is still open -- the one
  // run parked at needs_review that is still live (technical plan
  // §43.20). Re-read on the same cadence as the run list, since a turn
  // created on the session closes it.
  const activityQuery = useQuery({
    queryKey: sessionQueryKeys.activity(sessionId),
    queryFn: ({ signal }) => getSessionActivity(sessionId, signal),
    refetchInterval: WORKFLOW_RUN_POLL_MS,
  })
  const runsQuery = useQuery({
    queryKey: workflowRunQueryKeys.listForSession(sessionId),
    queryFn: ({ signal }) => listSessionWorkflowRuns(sessionId, signal),
    // This screen's whole job is watching a run advance and answering it
    // when it stops. Without a refresh it shows whatever was true when the
    // tab was opened: a step that finished never finishes, and a decision
    // gate that opens after load never appears -- the operator waits at a
    // screen that will not tell them it is their turn. Same cadence as the
    // other live surfaces in this app.
    refetchInterval: WORKFLOW_RUN_POLL_MS,
  })

  // A run the operator picked out of the history list, if any. Null means
  // "follow featuredRun", which is what an operator who has not chosen
  // wants: land on whatever is live, else on what the session did last.
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null)

  const runs = runsQuery.isSuccess ? runsQuery.data.runs : []
  // A status that could not be read reports no escalation open: no parked
  // run is featured or re-read on a guess.
  const liveEscalationId = liveEscalationRunId(activityQuery.data)
  const selected = selectedRunId === null ? undefined : runs.find((r) => r.id === selectedRunId)
  const featured = runsQuery.isSuccess ? (selected ?? featuredRun(runs, liveEscalationId)) : null

  const runDetailQuery = useQuery({
    queryKey: workflowRunQueryKeys.detail(featured?.id ?? ''),
    queryFn: ({ signal }) => getWorkflowRun(featured?.id ?? '', signal),
    enabled: featured !== null,
    // Only while the run can still move (runRefetchInterval).
    refetchInterval: runRefetchInterval(featured, liveEscalationId),
  })

  if (sessionQuery.isPending || runsQuery.isPending || activityQuery.isPending) {
    return (
      <div className="session-state" aria-live="polite">
        <p>Loading workflow runs…</p>
      </div>
    )
  }
  if (sessionQuery.isError || runsQuery.isError) {
    return (
      <div className="session-state" role="alert">
        <p>Couldn't load this session's workflow runs.</p>
      </div>
    )
  }

  const session = sessionQuery.data
  const canAct = canActOnWorkflowStep(meQuery.data?.role, meQuery.data?.id, session.createdBy)

  const detail = runDetailQuery.isSuccess ? runDetailQuery.data : null
  const sequence = detail ? buildStepRunSequence(detail.stepRuns) : []
  const decidable = detail ? decidableStepRun(detail.stepRuns) : null
  const totalCost = detail ? totalKnownCost(detail.stepRuns) : null
  const notice = featured && detail && !decidable ? escalationNotice(featured, liveEscalationId) : null

  return (
    <div className="app two">
      <section className="main">
        <header className="sess-head">
          <Link to="/session/$sessionId" params={{ sessionId }} className="repo" style={{ textDecoration: 'none' }}>
            ← Session
          </Link>
          <span className="title">{session.title ?? '(untitled session)'}</span>
          {session.repos[0] && <span className="repo">{session.repos[0].name}</span>}
          {featured && (
            <span className="chip neutral">
              {laneLabel(featured.lane)} lane
            </span>
          )}
          {featured && (
            <span className={`chip ${runStatusTone(featured.status)}`}>
              <span className="dot" />
              {runStatusLabel(featured.status)}
            </span>
          )}
          <span className="spacer" />
          {totalCost !== null && <span className="cost">{formatStepCost(totalCost)} so far</span>}
        </header>

        <div className="timeline">
          {!featured && (
            <div className="card">
              <p>
                {runs.length === 0
                  ? 'No workflow runs have started for this session yet.'
                  : 'No run is in progress or waiting on a person. Every run of this session is in the run history.'}
              </p>
            </div>
          )}

          {featured && runDetailQuery.isPending && (
            <div className="session-state" aria-live="polite">
              <p>Loading run…</p>
            </div>
          )}

          {featured && runDetailQuery.isError && (
            <div className="session-state" role="alert">
              <p>Couldn't load this run's steps.</p>
            </div>
          )}

          {featured &&
            detail &&
            sequence.map((attempt, i) => {
              const next = sequence[i + 1]
              return (
                <div key={attempt.stepRun.id}>
                  <StepRunCard attempt={attempt} />
                  {next && <EdgeConnector edge={edgeToNext(attempt.stepRun, next.stepRun)} toStepIndex={next.stepIndex} />}
                </div>
              )
            })}

          {notice && <div className="banner banner-warn">{notice}</div>}
        </div>

        {/*
          Keyed on the attempt id, which is load-bearing rather than tidy.
          decidableStepRun returns WHICHEVER attempt is currently parked, and
          a run advances within itself, so a background refetch can swap the
          gate's target from one attempt to another without this element ever
          unmounting. Unkeyed, an open revise draft written about attempt A
          would ride that swap and be submitted against attempt B -- which is
          genuinely awaiting a decision, so the server accepts it and re-runs
          the wrong step with instructions meant for another one. The key
          forces a remount, discarding a draft that no longer has a target.
        */}
        {featured && detail && decidable && (
          <DecisionGate key={decidable.id} sessionId={sessionId} runId={featured.id} stepRun={decidable} canAct={canAct} />
        )}
      </section>

      <aside className="rail" aria-label="Workflow run details">
        {featured && (
          <div>
            <h3>Run</h3>
            <dl className="kv">
              <dt>lane</dt>
              <dd>{laneLabel(featured.lane)}</dd>
              <dt>definition v</dt>
              <dd>{featured.definitionVersion}</dd>
              <dt>started</dt>
              <dd>{new Date(featured.createdAt).toLocaleString()}</dd>
              {featured.finishedAt && (
                <>
                  <dt>finished</dt>
                  <dd>{new Date(featured.finishedAt).toLocaleString()}</dd>
                </>
              )}
            </dl>
          </div>
        )}
        {decidable && (
          <div>
            <h3>Decision</h3>
            <dl className="kv">
              <dt>status</dt>
              <dd>
                <span className={`chip ${stepRunStatusTone(decidable.status)}`}>
                  <span className="dot" />
                  {stepRunStatusLabel(decidable.status)}
                </span>
              </dd>
              <dt>who may decide</dt>
              <dd>admin/maintainer (any) · member (own or joined sessions)</dd>
            </dl>
          </div>
        )}
        {/*
          No empty-state line here. The main panel already says there are no
          runs; the rail repeating it in different words made the screen tell
          the operator the same thing twice, in two places, as though they
          were two facts.
        */}
        <RunHistoryPanel runs={runs} featuredId={featured?.id ?? null} onSelect={setSelectedRunId} />
      </aside>
    </div>
  )
}
