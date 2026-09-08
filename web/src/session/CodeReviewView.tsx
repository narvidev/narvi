// CodeReviewView.tsx -- §26.1's merge-readout layout (§12.2 item 2): digest
// sections FIRST ("What this PR does", architecture choices, stack risks,
// contested points), findings in a COLLAPSED appendix. The whole point is
// that a reviewer reads the merge decision before the finding-by-finding
// detail -- this file's own component order matches that: DigestSections
// renders before FindingsAppendix in JSX source order, and the appendix
// starts closed (useState(false)).
//
// # This view's own defining risk
//
// Every string rendered below -- digest.summary, archDecisions[].*,
// stackRisks, unverifiedLimits, contestedPoints, every finding's own
// description/filePath/suggestedFix/rebuttalText, the PR's own title --
// is authored by a model or by the PR's own author, i.e. by an attacker in
// the threat model that matters (this file's own defining risk). None of
// it is ever markdown-parsed or passed through dangerouslySetInnerHTML --
// every one of those fields renders as plain React text/attribute content
// (escaped by construction) or, for suggestedFix (a unified diff), inside
// a plain <pre> (this repo's own established "code snippets/diffs render
// as plain text in a <pre>" convention, mirroring Timeline.tsx's own
// .evt-json block). The ONE href this view ever constructs from review
// content -- "Open PR on GitHub" -- is built from server-resolved
// repoFullName/prNumber (never raw attacker text) and still goes through
// urlSafety.ts's isSafeHref before it becomes an anchor, exactly like
// every other REST-content-derived link in this codebase (SessionRail.tsx
// artifact links).
//
// # FilesChanged/BlastRadius gate nothing (§21.2)
//
// filesChanged and blastRadius below are rendered as plain informational
// text/chips ONLY -- neither is ever read by this file to enable/disable
// a button, sort with implied authority, or drive an auto-approval
// affordance. The ONLY thing that gates the maintainer-only actions
// (re-run, rebut, apply-suggestion, retire a false-positive pattern) is
// the signed-in caller's own role (meQuery.data.role) -- and even that is
// a UX nicety, never the real enforcement point: the server independently
// re-checks authz.Authorize on every one of those endpoints regardless of
// what this view chooses to render.
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'

import type { ArchDecision, ReviewReadout, ReviewReadoutFinding, ReviewReadoutSessionReuse, ReviewReadoutVerdict, ReviewVerdictHistoryEntry } from '@narvi/contracts/rest-dtos'

// sentinelFix/handoffReadiness (unlike sessionReuse) have no standalone
// named export -- their JSON Schema defs use an inline `type:
// ["object","null"]` rather than a $ref (dtos.schema.json's own comment on
// each: the $ref form collided with go-jsonschema's auto-derived wrapper
// name on the Go side, e.g. Automation.runHealth hit the exact same
// collision first). Indexed-access off ReviewReadout itself, rather than a
// hand-duplicated shape, so these two can never silently drift from the
// wire type they name.
type SentinelFix = NonNullable<ReviewReadout['sentinelFix']>
type HandoffReadiness = NonNullable<ReviewReadout['handoffReadiness']>

import { applySuggestion, getReviewReadout, listFalsePositivePatterns, rebutReviewFinding, retireFalsePositivePattern, retriggerReview } from '../api/endpoints'
import { falsePositivePatternQueryKeys, reviewQueryKeys } from '../api/queryKeys'
import { meQueryOptions } from '../auth/session'
import { formatRelativeTime } from './relativeTime'
import { descriptionAdequacyTone, findingStatusLabel, findingStatusTone, riskTone, sentinelFixLabel, sentinelFixTone, shippableLabel, shippableTone, visualQaTone } from './reviewFormat'
import { truncateForDisplay } from './textSafety'
import { isSafeHref } from './urlSafety'

const MAX_FIELD_CHARS = 4000

function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_FIELD_CHARS)}</>
}

function isMaintainerPlus(role: string | undefined): boolean {
  return role === 'admin' || role === 'maintainer'
}

export function DigestSections({ verdict }: { verdict: ReviewReadoutVerdict }) {
  const d = verdict.digest
  const hasArch = (d.archDecisions?.length ?? 0) > 0
  return (
    <div className="card">
      <div className="who">
        <span className="avatar b">R</span>
        <b>Review verdict</b>
        <time>{new Date(verdict.postedAt).toLocaleString()}</time>
      </div>

      <div className="digest-section">
        <h4>What this PR does</h4>
        <p>
          <T text={d.summary} />
        </p>
      </div>

      {hasArch && (
        <div className="digest-section">
          <h4>Architecture choices</h4>
          {(d.archDecisions ?? []).map((ad: ArchDecision, i: number) => (
            <div className="arch-decision" key={i}>
              <p>
                <T text={ad.decision ?? ''} />
              </p>
              <dl>
                {ad.rejectedAlternative && (
                  <>
                    <dt>not chosen</dt>
                    <dd>
                      <T text={ad.rejectedAlternative} />
                    </dd>
                  </>
                )}
                {ad.conventionConformance && (
                  <>
                    <dt>conformance</dt>
                    <dd>
                      <T text={ad.conventionConformance} />
                    </dd>
                  </>
                )}
              </dl>
            </div>
          ))}
        </div>
      )}

      {(d.stackRisks || verdict.blastRadius.length > 0 || d.unverifiedLimits) && (
        <div className="digest-section">
          <h4>Risks to the stack</h4>
          {verdict.blastRadius.length > 0 && (
            <div className="btnrow" style={{ marginBottom: 6 }}>
              {verdict.blastRadius.map((tag) => (
                <span className="chip neutral" key={tag}>
                  <span className="dot" />
                  {tag.replace('_', ' ')}
                </span>
              ))}
            </div>
          )}
          {d.stackRisks && (
            <p>
              <T text={d.stackRisks} />
            </p>
          )}
          {d.unverifiedLimits && (
            <p style={{ marginTop: 6, color: 'var(--faint)' }}>
              Not verified: <T text={d.unverifiedLimits} />
            </p>
          )}
        </div>
      )}

      <div className="digest-section">
        <h4>Description adequacy</h4>
        <div className="btnrow">
          <span className={`chip ${descriptionAdequacyTone(d.descriptionAdequacy)}`}>
            <span className="dot" />
            {d.descriptionAdequacy}
          </span>
          <span style={{ color: 'var(--muted)', fontSize: 'var(--text-base)' }}>
            <T text={d.adequacyExplanation} />
          </span>
        </div>
      </div>

      {d.contestedPoints && (
        <div className="digest-section">
          <h4>Contested points</h4>
          <p>
            <T text={d.contestedPoints} />
          </p>
        </div>
      )}

      <div className="verdict-foot">
        <span className="lock">⛨ posted via server-side verdict tool</span>
        <span>· files changed: {verdict.filesChanged} (self-reported, display only)</span>
        {verdict.reviewPath && <span>· {verdict.reviewPath} path</span>}
      </div>
    </div>
  )
}

export function FindingCard({
  finding,
  canAct,
  sessionId,
}: {
  finding: ReviewReadoutFinding
  canAct: boolean
  sessionId: string
}) {
  const queryClient = useQueryClient()
  const [rebutting, setRebutting] = useState(false)
  const [rebuttalText, setRebuttalText] = useState('')

  const rebutMutation = useMutation({
    mutationFn: () => rebutReviewFinding(sessionId, finding.identityHash, { rebuttalText }),
    onSuccess: () => {
      setRebutting(false)
      setRebuttalText('')
      void queryClient.invalidateQueries({ queryKey: reviewQueryKeys.readout(sessionId) })
    },
  })
  const applyMutation = useMutation({
    mutationFn: () => applySuggestion(sessionId, finding.identityHash),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: reviewQueryKeys.readout(sessionId) }),
  })

  const actionable = finding.status === 'open'
  const locLine = finding.startLine > 0 ? `${finding.startLine}${finding.endLine > finding.startLine ? `-${finding.endLine}` : ''}` : finding.line !== null && finding.line !== undefined ? String(finding.line) : null

  return (
    <div className={`card finding${finding.severity === 'high' ? ' high' : ''}`}>
      <div className="who">
        <b>
          <T text={finding.description.length > 80 ? `${finding.description.slice(0, 80)}…` : finding.description} />
        </b>
        <span className={`chip ${finding.severity === 'high' ? 'crit' : finding.severity === 'medium' ? 'warn' : 'ok'}`} style={{ marginLeft: 'auto' }}>
          <span className="dot" />
          {finding.severity}
        </span>
        <span className={`chip ${findingStatusTone(finding.status)}`} style={{ marginLeft: 6 }}>
          <span className="dot" />
          {findingStatusLabel(finding.status)}
        </span>
      </div>
      <span className="loc">
        <T text={finding.filePath} />
        {locLine ? `:${locLine}` : ''}
        {finding.startLine === 0 && ' · position unresolved'}
      </span>
      <p>
        <T text={finding.description} />
      </p>
      {finding.suggestedFix && <pre className="snippet">{truncateForDisplay(finding.suggestedFix, 8000)}</pre>}
      {finding.status === 'rebutted' && finding.rebuttalText && (
        <p style={{ color: 'var(--faint)', fontSize: 'var(--text-sm-alt)' }}>
          rebuttal: <T text={finding.rebuttalText} />
        </p>
      )}

      {canAct && actionable && !rebutting && (
        <div className="btnrow">
          <button type="button" className="btn primary" disabled={applyMutation.isPending || !finding.suggestedFix} onClick={() => applyMutation.mutate()}>
            {applyMutation.isPending ? 'Applying…' : 'Apply suggestion'}
          </button>
          <button type="button" className="btn" onClick={() => setRebutting(true)}>
            Dismiss with rebuttal
          </button>
        </div>
      )}
      {canAct && rebutting && (
        <div className="btnrow" style={{ flexDirection: 'column', alignItems: 'stretch' }}>
          <textarea
            className="btn"
            style={{ resize: 'vertical', minHeight: 60, textAlign: 'left' }}
            placeholder="Why is this not a real issue?"
            value={rebuttalText}
            onChange={(e) => setRebuttalText(e.target.value)}
          />
          <div className="btnrow">
            <button type="button" className="btn primary" disabled={rebutMutation.isPending || rebuttalText.trim().length === 0} onClick={() => rebutMutation.mutate()}>
              {rebutMutation.isPending ? 'Submitting…' : 'Submit rebuttal'}
            </button>
            <button type="button" className="btn" onClick={() => setRebutting(false)}>
              Cancel
            </button>
          </div>
        </div>
      )}
      {(rebutMutation.isError || applyMutation.isError) && (
        <p className="sidebar-notice" role="alert">
          That action failed. Try again.
        </p>
      )}
    </div>
  )
}

export function FindingsAppendix({ findings, canAct, sessionId }: { findings: ReviewReadoutFinding[]; canAct: boolean; sessionId: string }) {
  const [open, setOpen] = useState(false)
  if (findings.length === 0) {
    return <p className="rail-empty">No findings have ever been posted for this PR.</p>
  }
  const openCount = findings.filter((f) => f.status === 'open').length
  return (
    <div className="turn-block">
      <button type="button" className="appendix-toggle" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
        {open ? '▾' : '▸'} Findings ({findings.length} total{openCount > 0 ? `, ${openCount} open` : ''})
      </button>
      {open && (
        <div className="timeline" style={{ padding: 0, marginTop: 10 }}>
          {findings.map((f) => (
            <FindingCard key={f.identityHash} finding={f} canAct={canAct} sessionId={sessionId} />
          ))}
        </div>
      )}
    </div>
  )
}

/**
 * PrGitHubLink builds an "Open PR on GitHub"-style link from repoFullName/
 * prNumber -- server-resolved fields, never raw review-content text, but
 * still passed through urlSafety.ts's isSafeHref before ever becoming an
 * href (this view's own top comment: "the ONE href this view ever
 * constructs from review content"). Exported for direct render-safety
 * testing (mirrors DigestSections/FindingCard's own precedent): a hostile
 * repoFullName (e.g. one somehow containing a `javascript:`-shaped
 * segment) must degrade to the plain-text fallback below, never an
 * anchor. label defaults to "Open PR on GitHub" (every pre-existing call
 * site's own unchanged behavior); SentinelAutoFixPanel below overrides it
 * to name the FIX pr distinctly from the origin PR this whole view is
 * already about.
 */
export function PrGitHubLink({ repoFullName, prNumber, label = 'Open PR on GitHub' }: { repoFullName: string; prNumber: number; label?: string }) {
  const href = `https://github.com/${repoFullName}/pull/${prNumber}`
  if (!isSafeHref(href)) {
    return <span className="sub">PR link unavailable</span>
  }
  return (
    <a className="btn" href={href} target="_blank" rel="noreferrer noopener" style={{ textAlign: 'center', textDecoration: 'none' }}>
      {label}
    </a>
  )
}

/**
 * ReviewSessionPanel is §12.2 item 2's own "coalesced-mention/session-
 * reuse info" gap (mockups.html's own "Review session" rail panel,
 * decision 10: "trigger: @mention x2 -> coalesced" / "session: reused
 * (same PR)"). Only trigger/session are rendered -- the mockup's own
 * adjacent "model"/"diff" rows name facts this readout has no field for
 * at all (no per-turn model id or diff-prefetch flag on ReviewReadout),
 * so this panel renders exactly what is real rather than a fabricated
 * pair of rows alongside them.
 */
export function ReviewSessionPanel({ sessionReuse }: { sessionReuse: ReviewReadoutSessionReuse }) {
  const reused = sessionReuse.mentionCount > 1
  return (
    <div>
      <h3>Review session</h3>
      <dl className="kv">
        <dt>trigger</dt>
        <dd>{reused ? `@mention ×${sessionReuse.mentionCount} → coalesced` : 'single @mention'}</dd>
        <dt>session</dt>
        <dd>{reused ? 'reused (same PR)' : 'new'}</dd>
      </dl>
    </div>
  )
}

/**
 * SentinelAutoFixPanel is §12.2 item 2's own "sentinel auto-fix PR link
 * with its merge-gated state" gap (§17). Renders nothing at all when no
 * sentinel-fix was ever triggered for this PR (sentinelFix null) -- an
 * honest absence, never an empty panel. mockups.html names this gap in
 * §12.2's own inventory text but draws no screenshot state for it; this
 * panel derives its layout from the SAME kv/chip/link component patterns
 * every other rail panel on this screen already establishes (§12's own
 * "derive an undrawn state from the established design system" rule)
 * rather than inventing a new visual idiom.
 */
export function SentinelAutoFixPanel({ sentinelFix, repoFullName }: { sentinelFix: SentinelFix | null; repoFullName: string }) {
  if (sentinelFix === null) return null
  return (
    <div>
      <h3>Sentinel auto-fix</h3>
      <dl className="kv">
        <dt>status</dt>
        <dd>
          <span className={`chip ${sentinelFixTone(sentinelFix.status)}`}>
            <span className="dot" />
            {sentinelFixLabel(sentinelFix.status)}
          </span>
        </dd>
      </dl>
      {sentinelFix.fixPrNumber !== null && (
        <div className="btnrow" style={{ marginTop: 6 }}>
          <PrGitHubLink repoFullName={repoFullName} prNumber={sentinelFix.fixPrNumber} label="View fix PR on GitHub" />
        </div>
      )}
    </div>
  )
}

/**
 * HandoffReadinessCard is §12.2 item 2's own "handoff-readiness display"
 * gap (§14.4) -- mirrors mockups.html's own "Handoff-readiness sentinel"
 * timeline card (avatar + area/status table + verdict-foot), scoped to
 * THIS review's own PR (the mockup's own example instead narrates a
 * DIFFERENT, related PR as a cross-PR notification -- this readout has no
 * data about any PR other than the one it is already about, so this card
 * reports only what is real for THIS one). Renders nothing when this PR's
 * session was never a scoped-Environment prototype (handoffReadiness
 * null).
 */
export function HandoffReadinessCard({ handoffReadiness }: { handoffReadiness: HandoffReadiness | null }) {
  if (handoffReadiness === null) return null
  return (
    <div className="card">
      <div className="who">
        <span className="avatar b">R</span>
        <b>Handoff-readiness sentinel</b>
        <span className="chip neutral" style={{ marginLeft: 8 }}>
          <span className="dot" />
          handoff
        </span>
        <time>{formatRelativeTime(handoffReadiness.flaggedAt)} ago</time>
      </div>
      <dl className="kv">
        <dt>contract drift</dt>
        <dd>
          <span className={`chip ${handoffReadiness.contractDriftFlagged ? 'warn' : 'ok'}`}>
            <span className="dot" />
            {handoffReadiness.contractDriftFlagged ? 'drifted' : 'none'}
          </span>
        </dd>
        <dt>backend TODOs</dt>
        <dd>
          <span className={`chip ${handoffReadiness.todoCount > 0 ? 'warn' : 'ok'}`}>
            <span className="dot" />
            {handoffReadiness.todoCount > 0 ? `${handoffReadiness.todoCount} found` : 'none'}
          </span>
        </dd>
      </dl>
      <div className="verdict-foot">
        <span className="lock">⛨ posted via the handoff-sentinel notifier</span>
      </div>
    </div>
  )
}

export function SentinelsPanel({ verdict, visualQa }: { verdict: ReviewReadoutVerdict | null; visualQa: string | null }) {
  return (
    <div>
      <h3>Sentinels</h3>
      <dl className="kv">
        <dt>coverage</dt>
        <dd>
          <span className={`chip ${verdict?.testsCoverage === 'adequate' ? 'ok' : verdict?.testsCoverage === 'insufficient' ? 'crit' : 'neutral'}`}>
            <span className="dot" />
            {verdict?.testsCoverage ?? 'not reviewed yet'}
          </span>
        </dd>
        <dt>doc drift</dt>
        <dd>
          <span className={`chip ${verdict?.docsDrift === 'found' ? 'warn' : verdict?.docsDrift === 'none' ? 'ok' : 'neutral'}`}>
            <span className="dot" />
            {verdict?.docsDrift ?? 'not reviewed yet'}
          </span>
        </dd>
        {/* visual QA (§12.2 item 2's own gap): read live from this PR's
            own "visual-qa:" GitHub label -- a human applies it, Narvi
            never computes it. Null means either no such label exists yet,
            or the live label fetch itself degraded (reviewreadout.go's
            own doc comment) -- the two are indistinguishable here, same
            as prTitle's own identical degrade. */}
        <dt>visual QA</dt>
        <dd>
          {visualQa !== null ? (
            <span className={`chip ${visualQaTone(visualQa)}`}>
              <span className="dot" />
              <T text={visualQa} />
            </span>
          ) : (
            <span className="chip neutral">
              <span className="dot" />
              not set
            </span>
          )}
        </dd>
        <dt>fact check</dt>
        <dd>{verdict?.factCheck ?? '—'}</dd>
        <dt>counter-review</dt>
        <dd>{verdict?.counterReview ?? '—'}</dd>
      </dl>
    </div>
  )
}

function HistoryPanel({ history }: { history: ReviewVerdictHistoryEntry[] }) {
  if (history.length === 0) return null
  return (
    <div>
      <h3>History</h3>
      <ul className="transitions">
        {history
          .slice()
          .reverse()
          .map((h, i) => (
            <li key={`${h.headSha}-${h.postedAt}`} className={i === history.length - 1 ? `now tone-${riskTone(h.riskLevel) === 'crit' ? 'crit' : riskTone(h.riskLevel) === 'warn' ? 'warn' : ''}` : ''}>
              <b>{h.riskLevel} risk</b> · {shippableLabel(h.shippable)} · {new Date(h.postedAt).toLocaleString()}
            </li>
          ))}
      </ul>
    </div>
  )
}

function FalsePositivePatternsPanel({ repoFullName }: { repoFullName: string }) {
  const [expanded, setExpanded] = useState(false)
  const queryClient = useQueryClient()
  const [owner, repo] = repoFullName.split('/')
  const patternsQuery = useQuery({
    queryKey: falsePositivePatternQueryKeys.list(repoFullName),
    queryFn: ({ signal }) => listFalsePositivePatterns(owner ?? '', repo ?? '', signal),
    enabled: expanded && Boolean(owner) && Boolean(repo),
  })
  const retireMutation = useMutation({
    mutationFn: (patternId: string) => retireFalsePositivePattern(owner ?? '', repo ?? '', patternId),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: falsePositivePatternQueryKeys.list(repoFullName) }),
  })

  return (
    <div>
      <h3>False-positive patterns</h3>
      <button type="button" className="btn" onClick={() => setExpanded((v) => !v)}>
        {expanded ? 'Hide' : 'View patterns for this repo'}
      </button>
      {expanded && patternsQuery.isPending && <p className="rail-empty">Loading…</p>}
      {expanded && patternsQuery.isSuccess && patternsQuery.data.patterns.length === 0 && <p className="rail-empty">No taught patterns yet.</p>}
      {expanded &&
        patternsQuery.isSuccess &&
        patternsQuery.data.patterns.map((p) => (
          <div key={p.id} className={`fp-pattern-row${p.retiredAt ? ' retired' : ''}`}>
            <span className="reason">
              <T text={p.reason} /> · hit {p.hitCount}×{p.retiredAt ? ' · retired' : ''}
            </span>
            {!p.retiredAt && (
              <button type="button" className="btn" disabled={retireMutation.isPending} onClick={() => retireMutation.mutate(p.id)}>
                Retire
              </button>
            )}
          </div>
        ))}
    </div>
  )
}

export function CodeReviewView({ sessionId }: { sessionId: string }) {
  const meQuery = useQuery(meQueryOptions)
  const canAct = isMaintainerPlus(meQuery.data?.role)

  const readoutQuery = useQuery({
    queryKey: reviewQueryKeys.readout(sessionId),
    queryFn: ({ signal }) => getReviewReadout(sessionId, signal),
  })

  const retriggerMutation = useMutation({
    mutationFn: () => retriggerReview(sessionId),
  })

  if (readoutQuery.isPending) {
    return (
      <div className="session-state" aria-live="polite">
        <p>Loading review…</p>
      </div>
    )
  }
  if (readoutQuery.isError) {
    return (
      <div className="session-state" role="alert">
        <p>Couldn't load this review. This session may not have an associated pull request.</p>
      </div>
    )
  }

  const readout = readoutQuery.data
  const verdict = readout.latestVerdict ?? null
  // The four gap fields (§12.2 item 2) are all optional on the wire --
  // absent (undefined) whenever the underlying row/label/fetch genuinely
  // has nothing to report, per each field's own doc comment in
  // dtos.schema.json. Normalized to null once, here, mirroring verdict's
  // own line above, so every render site below can use a single "is this
  // present" check instead of juggling undefined and null separately.
  const visualQa = readout.visualQa ?? null
  const sentinelFix = readout.sentinelFix ?? null
  const handoffReadiness = readout.handoffReadiness ?? null

  return (
    <div className="app two">
      <section className="main">
        <header className="sess-head">
          <Link to="/session/$sessionId" params={{ sessionId }} className="repo" style={{ textDecoration: 'none' }}>
            ← Session
          </Link>
          <span className="title">
            Review · PR #{readout.prNumber}
            {readout.prTitle ? ` — ${readout.prTitle}` : ''}
          </span>
          <span className="repo">{readout.repoFullName}</span>
          {verdict && (
            <span className={`chip ${shippableTone(verdict.shippable)}`}>
              <span className="dot" />
              {shippableLabel(verdict.shippable)}
            </span>
          )}
          {readout.epistemicOutcome && (
            <span className="heads-up" title="The authoring session's own epistemic self-check flagged something worth a second look">
              ⚑ Heads-up ({readout.epistemicOutcome})
            </span>
          )}
          <span className="spacer" />
          {/* session reuse (§12.2 item 2's own gap) rides the SAME "cost"
              slot the mockup's own header uses ("review session · reused
              ×2") -- shown only when this PR's own claim was genuinely
              reused (a solo @mention says nothing new here, mirroring
              PresenceIndicator's own "only show when there is something
              notable" precedent, SessionHeader.tsx). */}
          {readout.sessionReuse.mentionCount > 1 && <span className="cost">review session · reused ×{readout.sessionReuse.mentionCount}</span>}
          {verdict?.riskLevel && (
            <span className="cost">
              <span className={`chip ${riskTone(verdict.riskLevel)}`}>
                <span className="dot" />
                {verdict.riskLevel} risk
              </span>
            </span>
          )}
        </header>

        <div className="timeline">
          {!verdict && (
            <div className="card">
              <p>No verdict has been posted for this PR yet.</p>
            </div>
          )}
          {verdict && <DigestSections verdict={verdict} />}
          <HandoffReadinessCard handoffReadiness={handoffReadiness} />
          <FindingsAppendix findings={readout.findings} canAct={canAct} sessionId={sessionId} />
        </div>
      </section>

      <aside className="rail" aria-label="Review details">
        <ReviewSessionPanel sessionReuse={readout.sessionReuse} />
        <SentinelAutoFixPanel sentinelFix={sentinelFix} repoFullName={readout.repoFullName} />
        <SentinelsPanel verdict={verdict} visualQa={visualQa} />
        <div>
          <h3>Actions</h3>
          <div className="btnrow" style={{ flexDirection: 'column', alignItems: 'stretch' }}>
            {canAct && (
              <button type="button" className="btn" disabled={retriggerMutation.isPending} onClick={() => retriggerMutation.mutate()}>
                {retriggerMutation.isPending ? 'Re-running…' : 'Re-run review'}
              </button>
            )}
            <PrGitHubLink repoFullName={readout.repoFullName} prNumber={readout.prNumber} />
          </div>
          {retriggerMutation.isError && (
            <p className="sidebar-notice" role="alert">
              Re-run failed. Try again.
            </p>
          )}
        </div>
        <HistoryPanel history={readout.history} />
        {canAct && <FalsePositivePatternsPanel repoFullName={readout.repoFullName} />}
      </aside>
    </div>
  )
}
