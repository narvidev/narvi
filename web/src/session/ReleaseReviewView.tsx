// ReleaseReviewView.tsx -- §15's dedicated release-review screen (§12.2
// item 9): manifest table + aggregate-diff trigger banner + composition
// findings with Block release / Acknowledge & ship actions.
//
// This closes the screen's own named gap: the composition-focused
// aggregate diff review pass (§15.3) is now actually dispatched, and this
// view renders its real result -- but the result has FOUR genuinely
// distinct states, never collapsed into one (see compositionPassState's
// own doc comment for the full "declined vs. pending" reasoning a prior
// version of this file got wrong):
//
//   1. aggregateReviewTriggered is false: none of §15.3's composition
//      criteria were met for this release, so no aggregate-diff pass
//      ever runs for it -- "not applicable", not a gap.
//   2. aggregateReviewTriggered is true, compositionHeadSha is null:
//      the pass was NEVER actually dispatched (its own template/diff/
//      head-sha fetch failed, or the turn insert itself failed) --
//      "declined", a HONEST TERMINAL state, never rendered as "pending"/
//      "check back shortly" (this system has no retry for a declined
//      composition dispatch, releasemanifestpending.go's own "no
//      revisit, no retry, no backoff" precedent).
//   3. aggregateReviewTriggered is true, compositionHeadSha is set,
//      compositionReviewedAt is null: the pass WAS genuinely dispatched
//      and is awaiting completion -- "pending", never rendered as zero
//      findings. This is the sentinel §21's own "not yet computed" rule
//      exists to protect: a real zero (state 4, empty findings) and "we
//      do not know yet" (this state) must never render identically.
//   4. compositionReviewedAt is set: a real result, findings or none,
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

import { acknowledgeReleaseComposition, blockReleaseComposition, getReleaseManifestReadout, unblockReleaseComposition } from '../api/endpoints'
import { reviewQueryKeys } from '../api/queryKeys'
import { meQueryOptions } from '../auth/session'
import { formatRelativeTime } from './relativeTime'
import { ciConclusionTone } from './reviewFormat'
import { truncateForDisplay } from './textSafety'

const MAX_FIELD_CHARS = 4000

function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_FIELD_CHARS)}</>
}

// isMaintainerPlus/isAdmin are exported (test-integrity fix): the ONLY
// caller inside this file passes their own boolean RESULT into
// ReleaseManifestBody as canBlock/canAcknowledge/canUnblock, so a
// rendering test that hand-passes those booleans directly (as this
// file's own render-safety tests used to) never actually exercises
// either function -- an inverted `role === 'viewer'` typo here, or a
// role string drifting from what GET /api/me actually returns, would
// pass every existing test unnoticed. Exporting lets
// __tests__/reviewRendering.test.tsx derive its own canBlock/
// canAcknowledge/canUnblock from a REAL role string, the same way the
// one production call site (ReleaseReviewView, below) does.
export function isMaintainerPlus(role: string | undefined): boolean {
  return role === 'admin' || role === 'maintainer'
}

export function isAdmin(role: string | undefined): boolean {
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
 * compositionDecisionChip/compositionDecisionSummaryText both render
 * readout.compositionDecision -- a CLOSED, three-value server enum
 * ('pending' | 'blocked' | 'acknowledged', contracts/rest/v1/dtos.schema.
 * json). A prior version of this file matched with `!== 'pending'` and
 * then picked between 'blocked' and an "acknowledged & shipped" FALLBACK
 * for anything else -- which is fail-OPEN in exactly the wrong direction:
 * the server's own generated restdtos.
 * ReleaseManifestReadoutCompositionDecision zero value is the empty
 * string (never a valid enum member, doc.go's own "an unset field is
 * never confused with a real value" discipline), so a readout this view
 * had not yet populated -- BEFORE the composition-fields-population fix
 * this Step also lands -- rendered a green "acknowledged & shipped" chip
 * immediately above the prose saying the pass had not even completed. An
 * out-of-enum value (an unrecognized future member, a transport bug, this
 * exact regression happening again) must fall to the SAME neutral "no
 * decision rendered" branch 'pending' itself takes, never the
 * risk-accepting affirmative one -- these two functions therefore each
 * switch on the two REAL terminal values only, with every other input
 * (including 'pending') falling through to a shared, neutral default.
 */
function compositionDecisionChip(decision: string) {
  switch (decision) {
    case 'blocked':
      return (
        <span className="chip crit" style={{ marginLeft: 8 }}>
          <span className="dot" />
          blocked
        </span>
      )
    case 'acknowledged':
      return (
        <span className="chip ok" style={{ marginLeft: 8 }}>
          <span className="dot" />
          acknowledged & shipped
        </span>
      )
    default:
      return null
  }
}

/**
 * compositionPassState distinguishes "genuinely dispatched, awaiting
 * completion" from "never actually dispatched at all" -- both of which
 * render as compositionReviewedAt == null, the ONE signal a prior
 * version of this file used alone (`!readout.compositionReviewedAt`) to
 * mean "Pending... has been dispatched but has not completed yet...
 * check back shortly". That claim is false whenever
 * dispatchCompositionReview itself declined (a failed template fetch, a
 * failed diff fetch -- exactly the failure mode a LARGE, truncated
 * release is most likely to hit, since run.go now also treats
 * coveragePartial as its own trigger) -- and this system has NO retry
 * for a declined dispatch (release_manifest_pending's own claim-and-
 * delete-in-one-statement design, "no revisit, no retry, no backoff"),
 * so "check back shortly" was not just momentarily wrong, it was a
 * promise this system would never keep.
 *
 * compositionHeadSha is the fact that tells the two apart: it is
 * recorded at DISPATCH time (internal/app/releasereview.
 * dispatchCompositionReview's own UpdateCompositionAnchor call),
 * immediately after the composition review turn is successfully
 * created -- strictly BEFORE that turn ever completes and posts
 * findings (compositionReviewedAt). A row can therefore be in
 * exactly one of three states once aggregateReviewTriggered is true:
 * headSha null (dispatch never happened), headSha set + reviewedAt null
 * (dispatched, awaiting completion), or reviewedAt set (a real result).
 * The server already shipped compositionHeadSha; this function is what
 * was missing to actually read it.
 */
export type CompositionPassState = 'declined' | 'pending' | 'reviewed'

export function compositionPassState(readout: Pick<ReleaseManifestReadout, 'compositionReviewedAt' | 'compositionHeadSha'>): CompositionPassState {
  if (readout.compositionReviewedAt) return 'reviewed'
  if (readout.compositionHeadSha) return 'pending'
  return 'declined'
}

function compositionDecisionSummaryText(decision: string): string | null {
  switch (decision) {
    case 'blocked':
      return 'Blocked'
    case 'acknowledged':
      return 'Acknowledged & shipped'
    default:
      return null
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

/**
 * UnblockAction is the confirmed-major "unblock path" fix's own leaf:
 * mirrors CompositionDecisionActions' own "QueryClientProvider only
 * needed here" isolation exactly, mounted ONLY when compositionDecision
 * reads 'blocked' AND the caller passes authz.ActionUnblockReleaseComposition
 * (admin only, the SAME row as Acknowledge & ship -- see that action's own
 * doc comment). Reopens back to 'pending', never straight to
 * 'acknowledged' -- the SAME admin-only Acknowledge & ship button above
 * still has to be clicked separately afterward.
 */
function UnblockAction({ sessionId }: { sessionId: string }) {
  const queryClient = useQueryClient()
  const unblockMutation = useMutation({
    mutationFn: () => unblockReleaseComposition(sessionId),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: reviewQueryKeys.releaseManifest(sessionId) }),
  })

  return (
    <div style={{ display: 'flex', gap: 8, marginTop: 10, alignItems: 'center' }}>
      <button type="button" className="btn" disabled={unblockMutation.isPending} onClick={() => unblockMutation.mutate()}>
        {unblockMutation.isPending ? 'Unblocking…' : 'Unblock'}
      </button>
      {unblockMutation.isError && (
        <span role="alert" style={{ color: 'var(--crit)', fontSize: 'var(--text-base)' }}>
          Couldn't unblock. Try again.
        </span>
      )}
    </div>
  )
}

/** ReleaseManifestBody: the readout-to-markup half, taking already-fetched data as a plain prop -- exported for direct render-safety testing (mirrors SessionRail.tsx's own ArtifactRow precedent). No hook of its own besides plain prop threading -- CompositionDecisionActions is the one leaf that needs a QueryClientProvider ancestor, and it is only ever reached once a composition review has actually completed with a still-pending decision. */
export function ReleaseManifestBody({ readout, sessionId, canBlock = false, canAcknowledge = false, canUnblock = false }: { readout: ReleaseManifestReadout; sessionId: string; canBlock?: boolean; canAcknowledge?: boolean; canUnblock?: boolean }) {
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
            {compositionDecisionChip(readout.compositionDecision)}
          </div>

          {readout.coveragePartial && (
            <p style={{ margin: '6px 0 0', color: 'var(--warn)', fontSize: 'var(--text-base)' }}>
              This release's own constituent PR listing (§15.2, the manifest table above) was incomplete when this check ran -- whether this composition pass was triggered at all may not reflect every constituent PR in this release. This describes the constituent-PR listing, never the composition diff itself below.
            </p>
          )}

          {compositionPassState(readout) === 'declined' && (
            <p style={{ color: 'var(--crit)', fontSize: 'var(--text-base)' }}>
              This release's own composition pass could not be dispatched (its prompt template or its diff could not be fetched) and will not be retried automatically for this release -- there is no result, and none is coming. A maintainer may need to re-trigger the review manually.
            </p>
          )}

          {compositionPassState(readout) === 'pending' && (
            <p style={{ color: 'var(--faint)', fontSize: 'var(--text-base)' }}>
              Pending: the composition-focused aggregate diff review pass has been dispatched but has not completed yet. This is not a result -- check back shortly.
            </p>
          )}

          {readout.compositionReviewedAt && readout.compositionFindings.length === 0 && (
            <p style={{ color: 'var(--faint)', fontSize: 'var(--text-base)' }}>
              No composition findings — this release's already-individually-reviewed changes compose cleanly (reviewed {formatRelativeTime(readout.compositionReviewedAt)}).
            </p>
          )}

          {readout.compositionReviewedAt && readout.compositionHeadSha && (
            <p style={{ margin: '2px 0 0', color: 'var(--faint)', fontSize: 'var(--text-base)' }}>
              Reviewed against <span className="num">{readout.compositionHeadSha.slice(0, 12)}</span>
              {readout.compositionDiffTruncated ? ' — this diff was truncated at its own fetch size cap, so this result may not reflect the release in full.' : '.'}
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

          {readout.compositionDecision === 'blocked' && canUnblock && <UnblockAction sessionId={sessionId} />}

          {/*
            Minor fix: a Block that was later Unblocked reopens
            compositionDecision back to 'pending' (internal/domain/review.
            CompositionDecisionActionUnblock's own doc comment) -- with NO
            trace of that on this screen, a reopened-but-not-yet-redecided
            release rendered byte-for-byte identically to a release nobody
            had ever looked at. compositionDecisionAt IS still populated
            by an Unblock (UpdateCompositionDecision sets it on every
            transition, including this one) even though compositionDecision
            itself reads 'pending' again -- that combination (pending AND
            a real decisionAt) can ONLY mean "reopened", never "never
            decided" (a truly-never-decided row has decisionAt null), so
            it is what this note keys on. No user name shown, matching
            the summary line immediately below for blocked/acknowledged --
            compositionDecisionBy is a raw id with no display-name
            resolution wired into this readout.
          */}
          {readout.compositionDecision === 'pending' && readout.compositionDecisionAt && (
            <p style={{ color: 'var(--faint)', fontSize: 'var(--text-base)', marginTop: 8 }}>
              Reopened {formatRelativeTime(readout.compositionDecisionAt)} -- a previous Block was undone; nothing is currently blocking this release.
            </p>
          )}

          {compositionDecisionSummaryText(readout.compositionDecision) && readout.compositionDecisionAt && (
            <p style={{ color: 'var(--faint)', fontSize: 'var(--text-base)', marginTop: 8 }}>
              {compositionDecisionSummaryText(readout.compositionDecision)} {formatRelativeTime(readout.compositionDecisionAt)}.
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
  // canUnblock: admin only, the SAME row as canAcknowledge -- see
  // authz.ActionUnblockReleaseComposition's own doc comment. Computed as
  // its own named value (never just `canAcknowledge` reused under a
  // second name) so a render-safety test can exercise it independently
  // of Acknowledge & ship's own gate, and so this call site reads as an
  // explicit RBAC decision rather than an accidental alias.
  const canUnblock = isAdmin(meQuery.data?.role)

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

        <ReleaseManifestBody readout={readout} sessionId={sessionId} canBlock={canBlock} canAcknowledge={canAcknowledge} canUnblock={canUnblock} />
      </section>
    </div>
  )
}
