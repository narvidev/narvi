// PlanModeView.tsx -- §12.2 item 3's own plan-mode screen (decisions
// 15-20): a versioned plan document + multi-channel approval bar.
//
// # Approving a plan is an authorization act, not a display
//
// canActOnPlan (planFormat.ts) is this view's own CLIENT-SIDE gate on
// rendering the approve/reject/request-changes affordances -- it mirrors
// §13.3's real matrix (admin/maintainer any plan; a member only one they
// created or joined; a viewer never) as closely as a browser CAN check it,
// but it is explicitly a CONSERVATIVE UNDER-approximation (see that
// function's own doc comment: "joined" has no client-visible signal
// anywhere in this codebase yet). The real authority is server-side,
// unconditionally: every mutation below (approveMutation/rejectMutation/
// reviseMutation) is sent to the real REST endpoint regardless of what
// this view chose to render, and internal/adapters/inbound/httpapi/
// planauthz.go's canActOnPlan re-derives the full matrix -- including
// "joined" -- independently, from the database, on every single call. A
// 403 from any of the three mutations below is surfaced as an honest
// error state (mutation.isError), never silently swallowed or
// reinterpreted as success -- proving the client cannot be the real gate
// even when its own affordance happened to be shown.
//
// # Revise semantics (read from internal/domain/plan/verdict.go,
// internal/app/sessionactor/outboxenqueue.go's planApprovalLinearText, and
// internal/adapters/inbound/httpapi/planapprove.go's own top doc comment)
//
// "Request changes" is NEVER a structured edit of the existing plan and
// NEVER a REST call of its own (there is no POST .../plans/:id/revise
// endpoint anywhere in this codebase -- grepped directly). It is always a
// BRAND NEW plan_mode=true turn, dispatched with the human's own feedback
// text as its prompt, on the SAME session/OpenCode conversation (the
// producing turn's own conversation id already threads forward
// automatically, dispatch.go's buildPromptPayload) -- an ordinary
// createTurn call, identical in shape to Timeline.tsx's own Resume action,
// just with planMode: true and the feedback as the prompt instead of a
// fixed string. The server then supersedes the current awaiting_approval
// plan row the MOMENT that new turn completes and produces v(N+1)
// (internal/app/sessionactor/planrecord.go, atomic with the turn's own
// terminal-state write) -- this view never marks the OLD plan superseded
// itself; it only sends the new turn and lets the existing pipeline (the
// SAME WS/query-invalidation path every other turn already uses) surface
// the new version once it lands. Mirrors Slack's "revise:" prefix /
// Linear's identical convention (plandomain.RevisePrefix) exactly, just
// reached through the composer's own structured request instead of a
// parsed chat reply.
//
// # Structured plans, and the honest fallback for one that isn't (§12.2
// item 3's own missing piece, closed here)
//
// plan.content is ALWAYS present -- the producing turn's own rendered
// prose, exactly as before this Step. plan.structured is the SAME content
// string's own machine-recovered structure (internal/domain/plan.
// ExtractStructured), present only when the model emitted a valid
// ```plan-steps block; null otherwise. Null covers three cases
// identically and deliberately (Plan's own schema doc comment,
// contracts/rest/v1/dtos.schema.json): every plan that predates this
// field, a plan-mode turn whose model never attempted the block, and one
// that attempted and failed validation -- none of these is a "confident
// empty plan", so PlanCard below never renders an empty numbered list for
// any of them. It renders EXACTLY plan.content, in prose, precisely as
// this view always has -- structured rendering is additive, never a
// replacement path that could silently swallow a plan that has real
// content but no recoverable structure.
//
// # Every third-party-authored string here is plain text
//
// plan.content, plan.structured's own step title/description/fileRefs
// entries and scopeEstimate (all model-authored, verbatim -- see Plan's
// own schema doc comment), and the revise feedback textarea's own value
// (human-authored, echoed back nowhere in THIS view but sent to the server
// as a turn prompt, the exact same trust level as anything else typed into
// the composer) are the attacker-reachable field families. All render as
// plain React text content only -- see the local T component below
// (identical truncateForDisplay + JSX-text-interpolation shape
// CodeReviewView.tsx/ReleaseReviewView.tsx already establish) -- never
// dangerouslySetInnerHTML, never markdown/ANSI-parsed. A step's own
// fileRefs are rendered as inert `<code>` text only, NEVER as a clickable
// or navigable link: this view constructs no href from any plan-authored
// string at all (content, a step's title/description, or a file path), so
// urlSafety.ts has nothing to guard here (unlike SessionRail.tsx's
// artifact links or CodeReviewView's PR link) and a hostile fileRefs entry
// (e.g. a path crafted to look like it escapes the repository, or one
// containing a `javascript:`-scheme string) can never become navigation.
import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'

import type { Plan } from '@narvi/contracts/rest-dtos'

import { approvePlan, createTurn, getSession, listPlans, rejectPlan } from '../api/endpoints'
import { ApiError } from '../api/http'
import { planQueryKeys, sessionListQueryKeys, sessionQueryKeys } from '../api/queryKeys'
import { meQueryOptions } from '../auth/session'
import { canActOnPlan, latestPlan, modelLabel, planStatusLabel, planStatusTone, stripStructureBlock } from './planFormat'
import { truncateForDisplay } from './textSafety'

const MAX_CONTENT_CHARS = 8000

function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_CONTENT_CHARS)}</>
}

/** StructuredPlanSteps renders plan.structured's own numbered steps (docs/design/mockups.html's own `.planlist` shape, decision 15) -- exported alongside PlanCard for the identical render-safety testing this file's own top comment describes: every string here (title, description, each fileRefs entry) is model-authored and renders as plain text only, and no fileRefs entry is ever turned into an href. */
export function StructuredPlanSteps({ structured }: { structured: NonNullable<Plan['structured']> }) {
  return (
    <ol className="planlist">
      {structured.steps.map((step, i) => (
        <li key={i}>
          <div>
            <b>
              <T text={step.title} />
            </b>
            <p>
              <T text={step.description} />
            </p>
            {step.fileRefs.length > 0 && (
              <p>
                {step.fileRefs.map((ref, j) => (
                  <code key={j}>
                    <T text={ref} />
                  </code>
                ))}
              </p>
            )}
          </div>
        </li>
      ))}
    </ol>
  )
}

/** PlanCard renders one plan version -- as a structured numbered list when plan.structured is present, or plan.content's own prose otherwise (see this file's own top comment for why those are the ONLY two cases and why null is never mistaken for "zero steps"). Exported for direct render-safety testing (mirrors CodeReviewView.tsx's own DigestSections/FindingCard precedent): a hostile plan.content or a hostile structured field (markup, a `javascript:` URL as plain text, an XSS payload) must render as plain text only, never as markup or a link -- see this file's own top doc comment. */
export function PlanCard({ plan }: { plan: Plan }) {
  return (
    <div className="card">
      <div className="who">
        <span className="avatar b">A</span>
        <b>Plan</b>
        <time>{new Date(plan.createdAt).toLocaleString()}</time>
      </div>
      {plan.structured ? <StructuredPlanSteps structured={plan.structured} /> : <p className="plan-content"><T text={stripStructureBlock(plan.content)} /></p>}
      <div className="verdict-foot">
        {plan.structured && (
          <span>
            estimated scope: <T text={plan.structured.scopeEstimate} />
          </span>
        )}
        <span style={plan.structured ? { marginLeft: 'auto' } : undefined}>
          plan persisted · v{plan.version}
          {plan.decidedAt ? ` · decided ${new Date(plan.decidedAt).toLocaleString()}` : ''}
        </span>
      </div>
    </div>
  )
}

function PlanHistoryPanel({ plans }: { plans: Plan[] }) {
  if (plans.length === 0) return null
  const sorted = plans.slice().sort((a, b) => a.version - b.version)
  return (
    <div>
      <h3>Plan history</h3>
      <ul className="transitions">
        {sorted.map((p, i) => (
          <li key={p.id} className={i === sorted.length - 1 ? 'now' : undefined}>
            <b>
              v{p.version} {planStatusLabel(p.status)}
            </b>{' '}
            · {new Date(p.createdAt).toLocaleString()}
          </li>
        ))}
      </ul>
    </div>
  )
}

function ReviseBox({ sessionId, onDone }: { sessionId: string; onDone: () => void }) {
  const queryClient = useQueryClient()
  const [feedback, setFeedback] = useState('')
  const reviseMutation = useMutation({
    mutationFn: () => createTurn(sessionId, { prompt: feedback, modelId: null, effort: null, planMode: true }),
    onSuccess: () => {
      setFeedback('')
      onDone()
      void queryClient.invalidateQueries({ queryKey: sessionQueryKeys.detail(sessionId) })
      void queryClient.invalidateQueries({ queryKey: planQueryKeys.list(sessionId) })
    },
  })

  return (
    <div className="btnrow" style={{ flexDirection: 'column', alignItems: 'stretch', width: '100%' }}>
      <textarea
        className="btn"
        style={{ resize: 'vertical', minHeight: 70, textAlign: 'left' }}
        placeholder="What should change about this plan?"
        value={feedback}
        onChange={(e) => setFeedback(e.target.value)}
      />
      <div className="btnrow">
        <button type="button" className="btn primary" disabled={reviseMutation.isPending || feedback.trim().length === 0} onClick={() => reviseMutation.mutate()}>
          {reviseMutation.isPending ? 'Sending…' : 'Send revision'}
        </button>
        <button type="button" className="btn" onClick={onDone}>
          Cancel
        </button>
      </div>
      {reviseMutation.isError && (
        <p className="sidebar-notice" role="alert">
          {reviseMutation.error instanceof ApiError && reviseMutation.error.status === 409
            ? 'A turn is already in flight for this session -- try again once it finishes.'
            : reviseMutation.error instanceof ApiError && reviseMutation.error.status === 403
              ? "You're not authorized to act on this session's plan."
              : 'Sending the revision failed. Try again.'}
        </p>
      )}
    </div>
  )
}

function ApprovalBar({ sessionId, plan, canAct }: { sessionId: string; plan: Plan; canAct: boolean }) {
  const queryClient = useQueryClient()
  const [revising, setRevising] = useState(false)

  const approveMutation = useMutation({
    mutationFn: () => approvePlan(sessionId, plan.id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: planQueryKeys.list(sessionId) })
      void queryClient.invalidateQueries({ queryKey: sessionQueryKeys.detail(sessionId) })
      void queryClient.invalidateQueries({ queryKey: sessionListQueryKeys.list('mine') })
      void queryClient.invalidateQueries({ queryKey: sessionListQueryKeys.list('all') })
    },
  })
  const rejectMutation = useMutation({
    mutationFn: () => rejectPlan(sessionId, plan.id),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: planQueryKeys.list(sessionId) }),
  })

  const pending = approveMutation.isPending || rejectMutation.isPending

  return (
    <div className="approvalbar">
      {revising ? (
        <ReviseBox sessionId={sessionId} onDone={() => setRevising(false)} />
      ) : (
        <>
          {/* Both mutation buttons are disabled (not unmounted) when !canAct: this view's own client-side approximation
              can under-shoot a real "joined" member (see canActOnPlan's own doc comment), so a disabled-but-visible
              button, with the honest reason below, is more truthful than hiding it outright -- and either way, the
              server independently re-checks every call (this file's own top doc comment). */}
          <button type="button" className="btn primary" disabled={!canAct || pending} onClick={() => approveMutation.mutate()} title={!canAct ? "You're not authorized to approve this plan (create it, join the session, or ask an admin/maintainer)" : undefined}>
            {approveMutation.isPending ? 'Approving…' : 'Approve & build'}
          </button>
          <button type="button" className="btn" disabled={!canAct || pending} onClick={() => setRevising(true)}>
            Request changes
          </button>
          <button type="button" className="btn danger" disabled={!canAct || pending} onClick={() => rejectMutation.mutate()}>
            {rejectMutation.isPending ? 'Rejecting…' : 'Reject'}
          </button>
          <span className="channels">approvals from Slack/Linear are accepted too, if this session originated there -- first verdict wins</span>
        </>
      )}
      {(approveMutation.isError || rejectMutation.isError) && (
        <p className="sidebar-notice" role="alert" style={{ width: '100%' }}>
          {(approveMutation.error instanceof ApiError && approveMutation.error.status === 403) || (rejectMutation.error instanceof ApiError && rejectMutation.error.status === 403)
            ? "The server refused this action: you're not authorized to decide this plan."
            : (approveMutation.error instanceof ApiError && approveMutation.error.status === 409) || (rejectMutation.error instanceof ApiError && rejectMutation.error.status === 409)
              ? 'This plan was already decided (or superseded) by someone else.'
              : 'That action failed. Try again.'}
        </p>
      )}
    </div>
  )
}

export function PlanModeView({ sessionId }: { sessionId: string }) {
  const meQuery = useQuery(meQueryOptions)
  const sessionQuery = useQuery({ queryKey: sessionQueryKeys.detail(sessionId), queryFn: ({ signal }) => getSession(sessionId, signal) })
  const plansQuery = useQuery({ queryKey: planQueryKeys.list(sessionId), queryFn: ({ signal }) => listPlans(sessionId, signal) })

  const featured = useMemo(() => (plansQuery.isSuccess ? latestPlan(plansQuery.data.plans) : null), [plansQuery.isSuccess, plansQuery.data])

  if (sessionQuery.isPending || plansQuery.isPending) {
    return (
      <div className="session-state" aria-live="polite">
        <p>Loading plan…</p>
      </div>
    )
  }
  if (sessionQuery.isError || plansQuery.isError) {
    return (
      <div className="session-state" role="alert">
        <p>Couldn't load this session's plan.</p>
      </div>
    )
  }

  const session = sessionQuery.data
  const canAct = canActOnPlan(meQuery.data?.role, meQuery.data?.id, session.createdBy)

  return (
    <div className="app two">
      <section className="main">
        <header className="sess-head">
          <Link to="/session/$sessionId" params={{ sessionId }} className="repo" style={{ textDecoration: 'none' }}>
            ← Session
          </Link>
          <span className="title">{session.title ?? '(untitled session)'}</span>
          {session.repos[0] && <span className="repo">{session.repos[0].name} · plan</span>}
          {featured && (
            <span className={`chip ${planStatusTone(featured.status)}`}>
              <span className="dot" />
              {planStatusLabel(featured.status)}
            </span>
          )}
          <span className="spacer" />
          {featured && (
            <span className="cost">
              planned with {modelLabel(featured.planModelId)} · builds with {modelLabel(session.buildModelId)}
            </span>
          )}
        </header>

        <div className="timeline">
          {!featured && (
            <div className="card">
              <p>No plan has been proposed for this session yet.</p>
            </div>
          )}
          {featured && (
            <>
              <PlanCard plan={featured} />
              <div className="resumed-note">on approval, the implementation turn is dispatched server-side -- no client re-prompt</div>
            </>
          )}
        </div>

        {featured && featured.status === 'awaiting_approval' && <ApprovalBar sessionId={sessionId} plan={featured} canAct={canAct} />}
      </section>

      <aside className="rail" aria-label="Plan details">
        {featured && (
          <div>
            <h3>Approval</h3>
            <dl className="kv">
              <dt>status</dt>
              <dd>
                <span className={`chip ${planStatusTone(featured.status)}`}>
                  <span className="dot" />
                  {planStatusLabel(featured.status)}
                </span>
              </dd>
              <dt>requested</dt>
              <dd>{new Date(featured.createdAt).toLocaleString()}</dd>
              <dt>who may decide</dt>
              <dd>admin/maintainer (any) · member (own or joined sessions)</dd>
              {featured.decidedBy && (
                <>
                  <dt>decided by</dt>
                  <dd>{featured.decidedBy}</dd>
                </>
              )}
            </dl>
          </div>
        )}
        <div>
          <h3>Models</h3>
          <dl className="kv">
            <dt>plan</dt>
            <dd>{featured ? modelLabel(featured.planModelId) : '—'}</dd>
            <dt>build</dt>
            <dd>{modelLabel(session.buildModelId)}</dd>
            <dt>build effort</dt>
            <dd>{modelLabel(session.buildEffort)}</dd>
          </dl>
        </div>
        <PlanHistoryPanel plans={plansQuery.data.plans} />
      </aside>
    </div>
  )
}
