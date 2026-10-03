// Timeline.tsx -- decision 4 ("A timeline of typed events") + decision 3
// ("Every failure is actionable"): renders a TimelineModel
// (timelineModel.ts) as the mockup's own turn/step card sequence,
// including §7.1's sub-task lane nesting and the failure card + Resume.
//
// # This Step's own defining risk, concretely, in this file
//
// Every string rendered below that originates from an event payload --
// toolName, a tool's freeform input/output, an execution_complete's own
// reason, a warning/error message -- is rendered as plain React text
// content (JSX text/attribute interpolation, which escapes by
// construction) or through safeJsonPreview (textSafety.ts, which caps
// length before the DOM ever sees it). Nothing in this file ever calls
// dangerouslySetInnerHTML, and no event-sourced string is ever built into
// a URL (artifact links live in the rail -- this file renders no
// href from event content at all).
import { useState } from 'react'
import { formatUsd } from './money'
import { useMutation, useQueryClient } from '@tanstack/react-query'

import { createTurn } from '../api/endpoints'
import { sessionListQueryKeys, sessionQueryKeys } from '../api/queryKeys'
import { safeJsonPreview } from './textSafety'
import type { StepNode, SubTaskNode, ToolCallNode, TurnNode } from './timelineModel'

// The composer is what lets a person type their own follow-up; the
// "one-click Resume" here (decision 3) has no text box to draw
// a prompt from, so it sends this fixed, honestly-generic continuation
// instead -- CreateTurnRequest.prompt is required/non-null (unlike
// CreateSessionRequest.prompt), and the OpenCode conversation itself is
// what actually carries context forward (sessions.opencode_conversation_id,
// threaded automatically server-side, CreateTurnRequest's own doc
// comment) -- this string only needs to nudge the model to continue, not
// restate anything. Exported so DecisionInboxView.tsx's own needs_attention
// "Resume" action (a failed-session row, decision inbox §16.1) sends the
// exact same createTurn call this file's own FailureCard does, rather than
// a second, independently-drifting copy of this string.
export const RESUME_PROMPT = 'Resume — continue exactly where the previous turn left off.'

// MAX_RENDERED_TURNS -- the "very long timeline must stay readable and
// must not hang the tab" requirement: rather than rendering every turn a
// long-lived session ever ran, only the most recent MAX_RENDERED_TURNS
// are mounted, with an honest count of what's hidden above them. A full
// virtualized scroller is a reasonable future enhancement; this fixed cap
// is the simplest thing that keeps a pathologically long history from
// ever reaching the DOM at all.
const MAX_RENDERED_TURNS = 30

function formatDuration(startIso: string, endIso: string): string {
  const ms = new Date(endIso).getTime() - new Date(startIso).getTime()
  if (!Number.isFinite(ms) || ms < 0) return ''
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(1)}s`
}

// A tool call's state comes from its own result and its turn, never from its
// position: parallel calls of one message run side by side, so any call of a
// live turn without a result is running, not only the last one drawn. A call
// of an ended turn without a result never got one -- its turn was cancelled,
// timed out or lost its sandbox, and a result the agent sends after that is
// not stored (technical plan §6.1) -- so it reads "no result", never ✓.
type ToolCallState = 'running' | 'done' | 'err' | 'none'

const TOOL_CALL_GLYPH: Record<ToolCallState, { className: string; glyph: string }> = {
  running: { className: 'live', glyph: '●' },
  done: { className: 'done', glyph: '✓' },
  err: { className: 'err', glyph: '✕' },
  none: { className: 'none', glyph: '–' },
}

function toolCallState(tc: ToolCallNode, turnLive: boolean): ToolCallState {
  if (tc.result === null) return turnLive ? 'running' : 'none'
  return tc.result.isError ? 'err' : 'done'
}

function ToolCallRow({ tc, turnLive }: { tc: ToolCallNode; turnLive: boolean }) {
  const [open, setOpen] = useState(false)
  const state = toolCallState(tc, turnLive)
  const { className, glyph } = TOOL_CALL_GLYPH[state]
  return (
    <div className="tool-block">
      <button type="button" className="tool tool-toggle" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
        <span className={`st ${className}`}>{glyph}</span>
        {tc.toolName}
        <span className="dur">
          {state === 'running' ? 'running…' : state === 'none' ? 'no result' : tc.result ? formatDuration(tc.startedAt, tc.result.finishedAt) : ''}
        </span>
      </button>
      {open && (
        <pre className="evt-json">
          {safeJsonPreview(tc.input)}
          {tc.result ? `\n\n→ ${safeJsonPreview(tc.result.output)}` : ''}
        </pre>
      )}
      {tc.subTasks.length > 0 && <SubTaskLanes subTasks={tc.subTasks} />}
    </div>
  )
}

function SubTaskLanes({ subTasks }: { subTasks: SubTaskNode[] }) {
  const running = subTasks.filter((s) => s.status === 'running').length
  return (
    <div className="sublanes">
      {subTasks.length > 1 && (
        <div className="sublane-summary">
          {running > 0 ? `${running} running` : `${subTasks.length} sub-tasks`}
        </div>
      )}
      {subTasks.map((s) => (
        <div key={s.subTaskId} className={`sublane sublane-${s.status}`}>
          <span className="st">{s.status === 'running' ? '●' : s.status === 'completed' ? '✓' : s.status === 'failed' ? '✕' : '⦸'}</span>
          {s.label}
          {s.subAgentType ? <span className="sublane-agent"> · {s.subAgentType}</span> : null}
        </div>
      ))}
    </div>
  )
}

function ToolCallList({ toolCalls, turnLive }: { toolCalls: ToolCallNode[]; turnLive: boolean }) {
  const [expanded, setExpanded] = useState(false)
  const HEAD = 2
  const TAIL = 1
  if (toolCalls.length <= HEAD + TAIL || expanded) {
    return (
      <>
        {toolCalls.map((tc) => (
          <ToolCallRow key={tc.callId || tc.messageId} tc={tc} turnLive={turnLive} />
        ))}
      </>
    )
  }
  const head = toolCalls.slice(0, HEAD)
  const tail = toolCalls.slice(toolCalls.length - TAIL)
  const hidden = toolCalls.length - HEAD - TAIL
  return (
    <>
      {head.map((tc) => (
        <ToolCallRow key={tc.callId || tc.messageId} tc={tc} turnLive={turnLive} />
      ))}
      <button type="button" className="fold" onClick={() => setExpanded(true)}>
        ▸ {hidden} more tool call{hidden === 1 ? '' : 's'}
      </button>
      {tail.map((tc) => (
        <ToolCallRow key={tc.callId || tc.messageId} tc={tc} turnLive={turnLive} />
      ))}
    </>
  )
}

// live marks the card the turn is streaming into (its last step); turnLive
// is whether its turn is still running at all, which every tool call of
// every step of the turn reads its state from.
function StepCard({ step, live, turnLive }: { step: StepNode; live: boolean; turnLive: boolean }) {
  const tokenText = step.tokens.map((t) => t.text).join('\n\n')
  return (
    <div className={`card${live ? ' stream' : ''}`}>
      <div className="who">
        <span className="avatar b">A</span>
        <b>Agent</b>
        {step.startedAt && <time>{new Date(step.startedAt).toLocaleTimeString()}</time>}
      </div>
      <div className="steps">
        <ToolCallList toolCalls={step.toolCalls} turnLive={turnLive} />
        {step.cost && (
          <div className="step-sum">
            {step.toolCalls.length} call{step.toolCalls.length === 1 ? '' : 's'} · {step.cost.inputTokens + step.cost.outputTokens} tokens
            {step.cost.usd !== null ? ` · ${formatUsd(step.cost.usd)}` : ''}
          </div>
        )}
      </div>
      {tokenText && (
        <p>
          {tokenText}
          {live && <span className="caret" aria-hidden="true" />}
        </p>
      )}
    </div>
  )
}

const OUTCOME_HEADLINE: Record<string, string> = {
  heartbeat_silence: 'Sandbox lost mid-turn',
  timeout: 'This turn ran out of time',
  never_started: 'This turn never started',
}

function FailureCard({ sessionId, turn }: { sessionId: string; turn: TurnNode }) {
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => createTurn(sessionId, { prompt: RESUME_PROMPT, modelId: null, effort: null, planMode: false }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: sessionQueryKeys.detail(sessionId) })
      void queryClient.invalidateQueries({ queryKey: sessionListQueryKeys.list('mine') })
      void queryClient.invalidateQueries({ queryKey: sessionListQueryKeys.list('all') })
    },
  })
  const outcome = turn.outcome
  if (!outcome || outcome.outcome === 'completed') return null
  const headline = (outcome.reason && OUTCOME_HEADLINE[outcome.reason]) || 'This turn did not complete'
  return (
    <div className="card failcard">
      <div className="fh">
        <span className="chip crit">
          <span className="dot" />
          turn {outcome.outcome}
        </span>
        <b>{headline}</b>
      </div>
      <p>Your conversation and branch are intact -- resuming replays the same conversation on a fresh sandbox.</p>
      <div className="btnrow">
        <button type="button" className="btn primary" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
          {mutation.isPending ? 'Resuming…' : 'Resume turn'}
        </button>
        <span className="reason">reason: {outcome.reason ?? 'not provided'}</span>
      </div>
      {mutation.isError && (
        <p className="sidebar-notice" role="alert">
          Resume failed. Try again.
        </p>
      )}
    </div>
  )
}

export function Timeline({ sessionId, turns }: { sessionId: string; turns: TurnNode[] }) {
  const shown = turns.slice(-MAX_RENDERED_TURNS)
  const hiddenCount = turns.length - shown.length
  return (
    <div className="timeline">
      {hiddenCount > 0 && <div className="resumed-note">{hiddenCount} earlier turns not shown in this view</div>}
      {shown.map((turn) => (
        <div key={turn.firstEventId} className="turn-block">
          {turn.steps.map((step, i) => (
            <StepCard key={step.stepId} step={step} live={turn.live && i === turn.steps.length - 1} turnLive={turn.live} />
          ))}
          {/* Sub-tasks that found no spawning tool call: lanes of the turn itself, never dropped (timelineModel.ts). */}
          {turn.subTasks.length > 0 && <SubTaskLanes subTasks={turn.subTasks} />}
          <FailureCard sessionId={sessionId} turn={turn} />
        </div>
      ))}
    </div>
  )
}
