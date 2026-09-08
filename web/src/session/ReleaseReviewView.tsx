// ReleaseReviewView.tsx -- §15's dedicated release-review screen (§12.2
// item 9): manifest table + aggregate-diff trigger banner + composition
// findings with Block release / Acknowledge & ship actions.
//
// Step 125 closes this screen's own named gap: the composition-focused
// aggregate diff review pass (§15.3) is now actually dispatched, and this
// view renders its real result -- but the result has THREE genuinely
// distinct states, never collapsed into one:
//
//   1. aggregateReviewTriggered is false: none of §15.3's composition
//      criteria were met for this release, so no aggregate-diff pass
//      ever runs for it -- "not applicable", not a gap.
//   2. aggregateReviewTriggered is true but compositionReviewedAt is
//      null: the pass was dispatched but has not completed (or its
//      dispatch itself failed) -- "pending", never rendered as zero
//      findings. This is the sentinel §21's own "not yet computed" rule
//      exists to protect: a real zero (state 3, empty findings) and "we
//      do not know yet" (this state) must never render identically.
//   3. compositionReviewedAt is set: a real result, findings or none,
//      plus the Block release / Acknowledge & ship actions when
//      compositionDecision is still "pending".
//
// Every third-party-authored string here (a constituent PR's own title, a
// manifest finding's own detail text, a composition finding's own detail
// text) is plain React text content only -- same discipline as
// CodeReviewView.tsx's own top comment.
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'

import type { ReleaseManifestReadout } from '@narvi/contracts/rest-dtos'

import { acknowledgeReleaseComposition, blockReleaseComposition, getReleaseManifestReadout } from '../api/endpoints'
import { reviewQueryKeys } from '../api/queryKeys'
import { meQueryOptions } from '../auth/session'
import { formatRelativeTime } from './relativeTime'
import { ciConclusionTone } from './reviewFormat'
import { truncateForDisplay } from './textSafety'

const MAX_FIELD_CHARS = 4000

function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_FIELD_CHARS)}</>
}

function isMaintainerPlus(role: string | undefined): boolean {
  return role === 'admin' || role === 'maintainer'
}

function isAdmin(role: string | undefined): boolean {
  return role === 'admin'
}

function reviewStateChip(pr: { hasApprovingReview: boolean; mergedViaAdminOverride: boolean }) {
  if (pr.hasApprovingReview) {
    return (
      <span className="chip ok">
        <span className="dot" />
        approved
      </span>
    )
  }
  if (pr.mergedViaAdminOverride) {
    return (
      <span className="chip warn">
        <span className="dot" />
        admin override
      </span>
    )
  }
  return (
    <span className="chip crit">
      <span className="dot" />
      unreviewed
    </span>
  )
}

function manifestNote(pr: { hasApprovingReview: boolean; mergedViaAdminOverride: boolean; ciConclusion: string; wasReverted: boolean; revertReviewState: string }): string | null {
  if (!pr.hasApprovingReview && pr.mergedViaAdminOverride) return 'merged without an approving review'
  if (pr.ciConclusion === 'failure') return 'red at merge'
  if (pr.wasReverted && pr.revertReviewState === 'not_reviewed') return 'revert itself was not reviewed'
  return null
}

function compositionFindingKindLabel(kind: string): string {
  switch (kind) {
    case 'conflict':
      return 'conflict'
    case 'duplication':
      return 'duplication'
    case 'invalidated_assumption':
      return 'invalidated assumption'
    default:
      return 'other'
  }
}

/**
 * CompositionDecisionActions holds the Block release / Acknowledge & ship
 * mutations (§12.2 item 9) -- split out of ReleaseManifestBody so that
 * ONLY this leaf, only ever reached once a composition review has
 * actually completed with a still-"pending" decision, needs a
 * QueryClientProvider ancestor (mirrors CodeReviewView.tsx's own
 * FindingCard precedent exactly). canBlock/canAcknowledge are rendered
 * role-aware, but the server independently re-validates authz.
 * ActionEditReviewVerdict/authz.ActionAcknowledgeReleaseComposition on
 * every call regardless of what this view chooses to show. The caller
 * (ReleaseManifestBody, below) only ever mounts this component at all
 * when canBlock || canAcknowledge -- so a viewer/member render path never
 * reaches this component's own useQueryClient/useMutation hooks, and
 * never needs a QueryClientProvider ancestor either.
 */
function CompositionDecisionActions({ sessionId, canBlock, canAcknowledge }: { sessionId: string; canBlock: boolean; canAcknowledge: boolean }) {
  const queryClient = useQueryClient()
  const invalidate = () => void queryClient.invalidateQueries({ queryKey: reviewQueryKeys.releaseManifest(sessionId) })

  const blockMutation = useMutation({
    mutationFn: () => blockReleaseComposition(sessionId),
    onSuccess: invalidate,
  })
  const acknowledgeMutation = useMutation({
    mutationFn: () => acknowledgeReleaseComposition(sessionId),
    onSuccess: invalidate,
  })

  return (
    <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
      {canBlock && (
        <button type="button" className="btn danger" disabled={blockMutation.isPending} onClick={() => blockMutation.mutate()}>
          {blockMutation.isPending ? 'Blocking…' : 'Block release'}
        </button>
      )}
      {canAcknowledge && (
        <button type="button" className="btn" disabled={acknowledgeMutation.isPending} onClick={() => acknowledgeMutation.mutate()}>
          {acknowledgeMutation.isPending ? 'Acknowledging…' : 'Acknowledge & ship'}
        </button>
      )}
      {(blockMutation.isError || acknowledgeMutation.isError) && (
        <span role="alert" style={{ color: 'var(--crit)', fontSize: 'var(--text-base)' }}>
          Couldn't record that decision. Try again.
        </span>
      )}
    </div>
  )
}

/** ReleaseManifestBody: the readout-to-markup half, taking already-fetched data as a plain prop -- exported for direct render-safety testing (mirrors SessionRail.tsx's own ArtifactRow precedent). No hook of its own besides plain prop threading -- CompositionDecisionActions is the one leaf that needs a QueryClientProvider ancestor, and it is only ever reached once a composition review has actually completed with a still-pending decision. */
export function ReleaseManifestBody({ readout, sessionId, canBlock = false, canAcknowledge = false }: { readout: ReleaseManifestReadout; sessionId: string; canBlock?: boolean; canAcknowledge?: boolean }) {
  const flaggedCount = readout.mergedPrs.filter((pr) => manifestNote(pr) !== null).length

  return (
    <div className="timeline">
      {!readout.computed && (
        <div className="card">
          <p>No release manifest check has run for this PR yet.</p>
        </div>
      )}

      {readout.computed && (
        <div className="card">
          <div className="who">
            <span className="avatar b">R</span>
            <b>Manifest</b>
            <time>always runs</time>
          </div>
          <div style={{ overflowX: 'auto' }}>
            <table className="atable">
              <thead>
                <tr>
                  <th>PR</th>
                  <th>Review</th>
                  <th>CI @ merge</th>
                  <th>Notes</th>
                </tr>
              </thead>
              <tbody>
                {readout.mergedPrs.map((pr) => {
                  const note = manifestNote(pr)
                  return (
                    <tr key={pr.number} style={note ? { background: pr.ciConclusion === 'failure' ? 'var(--warn-soft)' : !pr.hasApprovingReview ? 'var(--crit-soft)' : 'var(--warn-soft)' } : undefined}>
                      <td>
                        <span className="num">#{pr.number}</span> <span className="trig"><T text={pr.title} /></span>
                      </td>
                      <td>{reviewStateChip(pr)}</td>
                      <td>
                        <span className={`chip ${ciConclusionTone(pr.ciConclusion)}`}>
                          <span className="dot" />
                          {pr.ciConclusion === 'success' ? 'green' : pr.ciConclusion === 'failure' ? 'red at merge' : 'unknown'}
                        </span>
                      </td>
                      <td>{note ?? <span className="num">—</span>}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
          <div className="verdict-foot">
            <span className="lock">⛨ posted via server-side verdict tool</span>
            <span>
              · compliance check, not a risk verdict · {flaggedCount} of {readout.mergedPrs.length} PRs flagged
            </span>
            {readout.coveragePartial && <span>· coverage was partial for this check</span>}
          </div>
        </div>
      )}

      {readout.computed && (
        <div className="card">
          <div className="who">
            <b>Aggregate diff review</b>
            <span className={`chip ${readout.aggregateReviewTriggered ? 'warn' : 'neutral'}`} style={{ marginLeft: 8 }}>
              <span className="dot" />
              {readout.aggregateReviewTriggered ? 'triggered' : 'not triggered'}
            </span>
          </div>
          {readout.aggregateReviewTriggered && readout.aggregateReviewTriggerReasons.length > 0 && (
            <p style={{ margin: '6px 0 0', color: 'var(--muted)', fontSize: 'var(--text-base)' }}>
              Trigger: {readout.aggregateReviewTriggerReasons.map((r, i) => (i === 0 ? <T key={i} text={r} /> : <span key={i}> · <T text={r} /></span>))}
            </p>
          )}
          {!readout.aggregateReviewTriggered && <p style={{ margin: '6px 0 0', color: 'var(--faint)', fontSize: 'var(--text-base)' }}>None of the composition criteria were met for this release.</p>}
        </div>
      )}

      {readout.computed && readout.aggregateReviewTriggered && (
        <div className="card">
          <div className="who">
            <b>Composition findings</b>
            {readout.compositionReviewedAt && (
              <span className={`chip ${readout.compositionFindings.length > 0 ? 'warn' : 'ok'}`} style={{ marginLeft: 8 }}>
                <span className="dot" />
                {readout.compositionFindings.length > 0 ? `${readout.compositionFindings.length} finding${readout.compositionFindings.length === 1 ? '' : 's'}` : 'clean'}
              </span>
            )}
            {readout.compositionDecision !== 'pending' && (
              <span className={`chip ${readout.compositionDecision === 'blocked' ? 'crit' : 'ok'}`} style={{ marginLeft: 8 }}>
                <span className="dot" />
                {readout.compositionDecision === 'blocked' ? 'blocked' : 'acknowledged & shipped'}
              </span>
            )}
          </div>

          {!readout.compositionReviewedAt && (
            <p style={{ color: 'var(--faint)', fontSize: 'var(--text-base)' }}>
              Pending: the composition-focused aggregate diff review pass has been dispatched but has not completed yet. This is not a result -- check back shortly.
            </p>
          )}

          {readout.compositionReviewedAt && readout.compositionFindings.length === 0 && (
            <p style={{ color: 'var(--faint)', fontSize: 'var(--text-base)' }}>
              No composition findings — this release's already-individually-reviewed changes compose cleanly (reviewed {formatRelativeTime(readout.compositionReviewedAt)}).
            </p>
          )}

          {readout.compositionReviewedAt && readout.compositionFindings.length > 0 && (
            <div>
              {readout.compositionFindings.map((f, i) => (
                <div key={i} className="card" style={{ marginTop: i === 0 ? 8 : 6 }}>
                  <div className="who">
                    <span className="chip warn">
                      <span className="dot" />
                      {compositionFindingKindLabel(f.kind)}
                    </span>
                  </div>
                  <p style={{ margin: '6px 0 0' }}>
                    <T text={f.detail} />
                  </p>
                </div>
              ))}
            </div>
          )}

          {readout.compositionReviewedAt && readout.compositionDecision === 'pending' && (canBlock || canAcknowledge) && (
            <CompositionDecisionActions sessionId={sessionId} canBlock={canBlock} canAcknowledge={canAcknowledge} />
          )}

          {readout.compositionDecision !== 'pending' && readout.compositionDecisionAt && (
            <p style={{ color: 'var(--faint)', fontSize: 'var(--text-base)', marginTop: 8 }}>
              {readout.compositionDecision === 'blocked' ? 'Blocked' : 'Acknowledged & shipped'} {formatRelativeTime(readout.compositionDecisionAt)}.
            </p>
          )}
        </div>
      )}
    </div>
  )
}

export function ReleaseReviewView({ sessionId }: { sessionId: string }) {
  const readoutQuery = useQuery({
    queryKey: reviewQueryKeys.releaseManifest(sessionId),
    queryFn: ({ signal }) => getReleaseManifestReadout(sessionId, signal),
  })
  const meQuery = useQuery(meQueryOptions)

  if (readoutQuery.isPending) {
    return (
      <div className="session-state" aria-live="polite">
        <p>Loading release manifest…</p>
      </div>
    )
  }
  if (readoutQuery.isError) {
    return (
      <div className="session-state" role="alert">
        <p>Couldn't load this release manifest. This session may not be a release-review session.</p>
      </div>
    )
  }

  const readout = readoutQuery.data
  const canBlock = isMaintainerPlus(meQuery.data?.role)
  const canAcknowledge = isAdmin(meQuery.data?.role)

  return (
    <div className="app one">
      <section className="main">
        <header className="sess-head">
          <Link to="/session/$sessionId" params={{ sessionId }} className="repo" style={{ textDecoration: 'none' }}>
            ← Session
          </Link>
          <span className="title">Release review · {readout.repoFullName}</span>
          <span className="repo">
            PR #{readout.prNumber}
            {readout.baseRef && readout.headRef ? ` · ${readout.baseRef} → ${readout.headRef}` : ''} · {readout.constituentPrCount} PRs
          </span>
          {readout.aggregateReviewTriggered && (
            <span className="chip warn">
              <span className="dot" />
              aggregate review triggered
            </span>
          )}
          <span className="spacer" />
        </header>

        <ReleaseManifestBody readout={readout} sessionId={sessionId} canBlock={canBlock} canAcknowledge={canAcknowledge} />
      </section>
    </div>
  )
}
