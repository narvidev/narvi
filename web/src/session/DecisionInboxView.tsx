// DecisionInboxView.tsx -- §16's own decision-inbox home view (decisions
// 32-34): "the home screen routes attention -- it doesn't monitor
// activity." A queue of pending decisions addressed to the signed-in
// user, sectioned by kind (ready_to_merge/needs_review/awaiting_approval/
// needs_attention), each row carrying its action inline.
//
// # The read model's own honest-state distinctions, and why this view
// # never collapses them
//
// GET /api/decision-inbox draws several real distinctions this view
// preserves rather than flattening into one generic "loading"/"empty":
//   - items is ALREADY ranked server-side (§16.1: decision cost then age)
//     -- this view renders that order as-is inside each section, never
//     re-sorting.
//   - scmAsOf==null && !scmFetchFailed means no GitHub identity is linked
//     -- a legitimate, non-degraded state -- distinct from scmFetchFailed
//     (a real but incomplete/degraded PR read), which is itself distinct
//     from carrying BOTH (a partial-but-real fetch: ListDecisionInboxResponse.
//     scmFetchFailed's own doc comment is explicit these two are NOT
//     mutually exclusive). ScmStatusBanner below renders all three
//     differently, and never presents scmAsOf as live truth -- only ever
//     "as of N ago".
//   - decisionLatencyComputed==false means "no data in the window", never
//     rendered identically to a real, computed zero-second median.
//   - needs_attention is admin-only, enforced inside decisioninbox.Build
//     itself -- this view does not re-filter it (there is nothing to
//     filter: a non-admin's own response simply never contains one), and
//     deliberately never renders an empty "Needs attention · 0" section
//     for a non-admin, which would misleadingly imply "nothing needs
//     attention" for a category that viewer cannot see at all.
//
// # Rendering safety
//
// item.title (a PR/plan/session/automation title), repoFullName,
// provenanceRepoFullName/provenancePattern (a CODEOWNERS pattern),
// failureReason, artifactSummary, and lastError are ALL third-party or
// model-influenced free text (a PR title is GitHub-author-controlled; a
// CODEOWNERS pattern is a repo-author-controlled string; failureReason/
// lastError can echo arbitrary upstream error text) -- every one of them
// renders through the plain-text T component below (truncateForDisplay,
// mirroring AutomationsView.tsx's own AutomationRow / MembersPanel.tsx's
// own MemberRow precedent exactly), never dangerouslySetInnerHTML.
// htmlUrl is the ONLY field that ever becomes an href, and only after
// isSafeHref (urlSafety.ts) accepts it -- mirrors SessionRail.tsx's own
// ArtifactRow precedent, including its identical "link unavailable"
// fallback text for a rejected URL.
import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'

import type { DecisionInboxItem } from '@narvi/contracts/rest-dtos'

import { approvePlan, createTurn, listDecisionInbox, mergePullRequest, resumeAutomation } from '../api/endpoints'
import { ApiError } from '../api/http'
import { automationQueryKeys, decisionInboxQueryKeys, planQueryKeys, repoAnalyticsQueryKeys, sessionListQueryKeys, sessionQueryKeys } from '../api/queryKeys'
import { meQueryOptions } from '../auth/session'
import { AUTO_PAUSE_THRESHOLD } from './automationFormat'
import {
  canMergeDecisionInboxItem,
  formatAgeSeconds,
  formatDecisionLatencySeconds,
  prChipData,
  provenanceText,
  releaseChipData,
  rowKeyFor,
  rowKind,
  SECTION_ORDER,
  sectionBlurb,
  sectionTitle,
} from './decisionInboxFormat'
import { formatRelativeTime } from './relativeTime'
import { RESUME_PROMPT } from './Timeline'
import { truncateForDisplay } from './textSafety'
import { isSafeHref } from './urlSafety'

const MAX_FIELD_CHARS = 500

// A stable, empty, reused array -- see DecisionInboxView's own "items"
// derivation below for why this must be a single shared reference rather
// than a fresh `[]` literal on every render.
const EMPTY_ITEMS: DecisionInboxItem[] = []

function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_FIELD_CHARS)}</>
}

/** OpenOnGitHubLink is the ONE href this view builds from PR-adjacent content -- htmlUrl comes straight from GitHub (never Narvi-authored) and is validated by isSafeHref before ever becoming a real anchor, mirroring SessionRail.tsx's own ArtifactRow precedent, including its "link unavailable" fallback. */
function OpenOnGitHubLink({ htmlUrl }: { htmlUrl: string | null }) {
  if (htmlUrl === null || !isSafeHref(htmlUrl)) {
    return <span className="qwhy">link unavailable</span>
  }
  return (
    <a className="btn" href={htmlUrl} target="_blank" rel="noreferrer noopener" style={{ textDecoration: 'none' }}>
      Open on GitHub ↗
    </a>
  )
}

/**
 * OpenReviewLink navigates into Narvi's own code-review screen for the
 * review session sessionId names -- ONLY ever rendered when the server
 * has already resolved a real session id for THIS exact PR
 * (DecisionInboxItem.sessionId's own doc comment), so this never links to
 * a screen that 400s at click time. A PR Narvi has never reviewed still
 * falls back to OpenOnGitHubLink instead (see DecisionInboxRow below) --
 * the honest "stays external" outcome for a row this client cannot offer
 * a working in-app action for.
 */
function OpenReviewLink({ sessionId }: { sessionId: string }) {
  return (
    <Link to="/session/$sessionId/review" params={{ sessionId }} className="btn" style={{ textDecoration: 'none' }}>
      Open review →
    </Link>
  )
}

/** OpenReleaseReviewLink is OpenReviewLink's own release-cut sibling, linking into the release-review screen instead -- same "only ever rendered with a real, server-resolved session id" guarantee. */
function OpenReleaseReviewLink({ sessionId }: { sessionId: string }) {
  return (
    <Link to="/session/$sessionId/release-review" params={{ sessionId }} className="btn" style={{ textDecoration: 'none' }}>
      Open release review →
    </Link>
  )
}

function PrChips({ item }: { item: DecisionInboxItem }) {
  return (
    <>
      {prChipData(item).map((chip) => (
        <span key={chip.text} className={`chip ${chip.tone}`}>
          <span className="dot" />
          {chip.text}
        </span>
      ))}
    </>
  )
}

function ReleaseChips({ item }: { item: DecisionInboxItem }) {
  return (
    <>
      {releaseChipData(item).map((chip) => (
        <span key={chip.text} className={`chip ${chip.tone}`}>
          <span className="dot" />
          {chip.text}
        </span>
      ))}
    </>
  )
}

/**
 * MergeButton -- decision 33: "auto-approved still means human-merged".
 * One click reveals an inline confirm step (never a bare, immediately-
 * destructive click); the actual POST fires only from "Confirm merge".
 * canMerge is this view's own CLIENT-SIDE affordance gate (viewer role
 * sees the queue read-only, §16.2) -- the real gate is server-side
 * (RevalidateForMerge, decisioninbox.go), re-checked unconditionally at
 * click time regardless of what this component decided to render.
 * hasChangesRequested pre-disables the button with an explanation (never
 * hasApprovingReview, which is display-only -- DecisionInboxItem.
 * hasChangesRequested's own doc comment is explicit this is the field a
 * client gates on).
 */
function MergeButton({ item, canMerge }: { item: DecisionInboxItem; canMerge: boolean }) {
  const [confirming, setConfirming] = useState(false)
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => mergePullRequest({ repoFullName: item.repoFullName ?? '', prNumber: item.prNumber ?? 0 }),
    onSuccess: () => {
      setConfirming(false)
      void queryClient.invalidateQueries({ queryKey: decisionInboxQueryKeys.list() })
      // A successful merge also records a confirmed/overridden §21.2 stage
      // 2 outcome server-side (appreviewverdict.RecordConfirmed,
      // decisioninbox.go's own MergePullRequest) -- the repo's own
      // review-risk analytics (contradiction-rate) read model just
      // changed too.
      if (item.repoFullName !== null) {
        void queryClient.invalidateQueries({ queryKey: repoAnalyticsQueryKeys.reviewAnalytics(item.repoFullName) })
      }
    },
  })

  if (!canMerge) {
    return (
      <span className="qwhy" title="Viewers see the decision queue read-only and cannot merge.">
        read-only
      </span>
    )
  }

  const blockedByChangesRequested = item.hasChangesRequested === true

  if (confirming) {
    return (
      <span className="btnrow" style={{ gap: 6, flex: 'none' }}>
        <button type="button" className="btn primary" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
          {mutation.isPending ? 'Merging…' : 'Confirm merge'}
        </button>
        <button type="button" className="btn" disabled={mutation.isPending} onClick={() => setConfirming(false)}>
          Cancel
        </button>
        {mutation.isError && (
          <span className="qwhy" role="alert" style={{ flexBasis: '100%', paddingLeft: 0 }}>
            {mutation.error instanceof ApiError ? <T text={mutation.error.message} /> : 'Merge failed. Try again.'}
          </span>
        )}
      </span>
    )
  }

  return (
    <button
      type="button"
      className="btn primary"
      disabled={blockedByChangesRequested}
      title={blockedByChangesRequested ? 'A reviewer requested changes on this pull request -- merge is blocked until that is resolved.' : undefined}
      onClick={() => setConfirming(true)}
    >
      Merge
    </button>
  )
}

function ApprovePlanButton({ sessionId, planId }: { sessionId: string; planId: string }) {
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => approvePlan(sessionId, planId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: planQueryKeys.list(sessionId) })
      void queryClient.invalidateQueries({ queryKey: decisionInboxQueryKeys.list() })
    },
  })
  return (
    <span className="btnrow" style={{ gap: 6, flex: 'none' }}>
      <button type="button" className="btn primary" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
        {mutation.isPending ? 'Approving…' : 'Approve & build'}
      </button>
      <Link to="/session/$sessionId/plan" params={{ sessionId }} className="btn" style={{ textDecoration: 'none' }}>
        Open
      </Link>
      {mutation.isError && (
        <span className="qwhy" role="alert" style={{ flexBasis: '100%', paddingLeft: 0 }}>
          {mutation.error instanceof ApiError ? <T text={mutation.error.message} /> : 'Approval failed. Try again.'}
        </span>
      )}
    </span>
  )
}

/** ResumeSessionButton sends the exact same createTurn call Timeline.tsx's own FailureCard "Resume turn" action does (RESUME_PROMPT, imported from there rather than a second, drifting copy). */
function ResumeSessionButton({ sessionId }: { sessionId: string }) {
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => createTurn(sessionId, { prompt: RESUME_PROMPT, modelId: null, effort: null, planMode: false }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: sessionQueryKeys.detail(sessionId) })
      void queryClient.invalidateQueries({ queryKey: sessionListQueryKeys.list('mine') })
      void queryClient.invalidateQueries({ queryKey: sessionListQueryKeys.list('all') })
      void queryClient.invalidateQueries({ queryKey: decisionInboxQueryKeys.list() })
    },
  })
  return (
    <span className="btnrow" style={{ gap: 6, flex: 'none' }}>
      <button type="button" className="btn primary" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
        {mutation.isPending ? 'Resuming…' : 'Resume'}
      </button>
      {mutation.isError && (
        <span className="qwhy" role="alert" style={{ flexBasis: '100%', paddingLeft: 0 }}>
          {mutation.error instanceof ApiError ? <T text={mutation.error.message} /> : 'Resume failed. Try again.'}
        </span>
      )}
    </span>
  )
}

function ResumeAutomationButton({ automationId }: { automationId: string }) {
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => resumeAutomation(automationId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: automationQueryKeys.list({}) })
      void queryClient.invalidateQueries({ queryKey: decisionInboxQueryKeys.list() })
    },
  })
  return (
    <span className="btnrow" style={{ gap: 6, flex: 'none' }}>
      <button type="button" className="btn primary" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
        {mutation.isPending ? 'Resuming…' : 'Resume'}
      </button>
      {mutation.isError && (
        <span className="qwhy" role="alert" style={{ flexBasis: '100%', paddingLeft: 0 }}>
          {mutation.error instanceof ApiError ? <T text={mutation.error.message} /> : 'Resume failed. Try again.'}
        </span>
      )}
    </span>
  )
}

/**
 * DecisionInboxRow renders one row -- exported for direct render-safety
 * testing (mirrors AutomationsView.tsx's own AutomationRow / MembersPanel.
 * tsx's own MemberRow precedent exactly). rowKind (decisionInboxFormat.ts)
 * decides which action/chip set applies -- see this file's own top doc
 * comment for the full field-provenance/rendering-safety accounting.
 */
export function DecisionInboxRow({ item, canMerge }: { item: DecisionInboxItem; canMerge: boolean }) {
  const kind = rowKind(item)
  const why = provenanceText(item)

  return (
    <div className="qrow">
      <span className="qkind">{kind}</span>
      <span className="qt">
        <T text={item.title} />
      </span>

      {(kind === 'pr' || kind === 'handoff') && <PrChips item={item} />}
      {kind === 'release' && <ReleaseChips item={item} />}
      {kind === 'plan' && (
        <span className="chip warn">
          <span className="dot" />
          awaiting approval
        </span>
      )}
      {kind === 'session' && (
        <span className="chip crit">
          <span className="dot" />
          {item.failureReason ? <T text={`failed · ${item.failureReason}`} /> : 'failed'}
        </span>
      )}
      {kind === 'automation' && (
        <span className="chip neutral">
          <span className="dot" />
          auto-paused
        </span>
      )}
      {kind === 'outbox' && (
        <span className="chip crit">
          <span className="dot" />
          {item.outboxKind ? <T text={`dead-lettered · ${item.outboxKind}`} /> : 'dead-lettered'}
        </span>
      )}

      {/*
        `stale` is the server's own flag and it is past tense: DecisionInboxItem.stale
        is "true once age exceeds the configured staleness threshold" (§16.1's >48h,
        configurable). This read "going stale", which says the opposite -- that the
        item is approaching a threshold it has in fact already crossed. On a triage
        queue that is the difference between acting now and leaving it another day.
        The threshold itself is never recomputed here; the flag is taken as given.
      */}
      <span className={item.stale ? 'qage stale' : 'qage'} title={item.stale ? 'Older than the staleness threshold' : undefined}>
        {formatAgeSeconds(item.ageSeconds)}
        {item.stale ? ' — stale' : ''}
      </span>

      {item.kind === 'ready_to_merge' && <MergeButton item={item} canMerge={canMerge} />}
      {/*
        Open review links into Narvi's own code-review screen, but ONLY
        when the server has already resolved a real review session for
        THIS exact PR (item.sessionId's own doc comment) -- a PR Narvi has
        never been mentioned on falls back to the external GitHub link
        instead, exactly like before this row could ever carry a session
        id at all. This never offers an action that fails: the id, when
        present, always names a session GetReviewReadout can actually
        serve.
      */}
      {kind === 'pr' &&
        item.kind === 'needs_review' &&
        (item.sessionId !== null ? <OpenReviewLink sessionId={item.sessionId} /> : <OpenOnGitHubLink htmlUrl={item.htmlUrl} />)}
      {kind === 'release' &&
        (item.sessionId !== null ? <OpenReleaseReviewLink sessionId={item.sessionId} /> : <OpenOnGitHubLink htmlUrl={item.htmlUrl} />)}
      {kind === 'handoff' && <OpenOnGitHubLink htmlUrl={item.htmlUrl} />}
      {kind === 'plan' && item.sessionId !== null && item.planId !== null && <ApprovePlanButton sessionId={item.sessionId} planId={item.planId} />}
      {kind === 'session' && item.sessionId !== null && <ResumeSessionButton sessionId={item.sessionId} />}
      {kind === 'automation' && item.automationId !== null && <ResumeAutomationButton automationId={item.automationId} />}

      {/*
        Through T like every other untrusted string on this row, not a bare
        interpolation: `why` is built from provenanceRepoFullName and
        provenancePattern, and a CODEOWNERS pattern is repository content, not
        something this platform validates. React escapes it either way, so the
        injection half was never at risk -- but the bound was, and the bound is
        what textSafety.ts exists for. This line was the one field named in
        this file's own rendering-safety accounting that did not actually go
        through it.
      */}
      {why !== null && (
        <span className="qwhy">
          <T text={why} />
        </span>
      )}
      {kind === 'session' && (
        <span className="qwhy">
          {/*
            Not "your conversation". This section is admin-only and the rows
            behind it are system-wide: buildAttentionItems calls
            SessionStore.ListFailed, whose query carries no per-user filter at
            all ("an admin's own ops-triage view is system-wide, not narrowed
            to sessions they personally created"). The wire row has no owner
            field, so the client cannot know whose session this is -- and the
            usual reader of this string is an admin looking at someone else's.
            The resume semantics below are true regardless of owner, which is
            the part worth saying.
          */}
          Conversation and branch are intact -- resuming replays the same conversation on a fresh sandbox.
        </span>
      )}
      {kind === 'automation' && (
        <span className="qwhy">
          Auto-paused after {AUTO_PAUSE_THRESHOLD} consecutive failed invocations.{item.artifactSummary && ' '}
          {item.artifactSummary && <T text={item.artifactSummary} />}
        </span>
      )}
      {kind === 'outbox' && <span className="qwhy">{item.lastError ? <T text={item.lastError} /> : 'No error detail recorded.'}</span>}
    </div>
  )
}

function Section({ kind, items, canMerge }: { kind: DecisionInboxItem['kind']; items: DecisionInboxItem[]; canMerge: boolean }) {
  return (
    <div>
      <div className="qhead">
        <h4>{sectionTitle(kind)}</h4>
        <span className="qcount">
          {items.length} · {sectionBlurb(kind)}
        </span>
      </div>
      {items.length > 0 ? (
        <div className="qrows">
          {items.map((item) => (
            <DecisionInboxRow key={rowKeyFor(item)} item={item} canMerge={canMerge} />
          ))}
        </div>
      ) : (
        <p className="rail-empty">Nothing here right now.</p>
      )}
    </div>
  )
}

/**
 * ScmStatusBanner -- §16.2's own three-way SCM state, rendered as three
 * genuinely different messages (never collapsed): no GitHub linked (a
 * legitimate empty state), a real but degraded/incomplete PR read (a
 * distinct, explicit warning, never silently presented as a complete
 * queue), and the two carried TOGETHER (a partial-but-real fetch --
 * ListDecisionInboxResponse.scmFetchFailed's own doc comment: "NOT
 * mutually exclusive"). scmAsOf is only ever shown as a staleness
 * marker ("as of N ago"), never as live truth.
 */
export function ScmStatusBanner({ scmAsOf, scmFetchFailed }: { scmAsOf: string | null; scmFetchFailed: boolean }) {
  if (scmFetchFailed) {
    return (
      <div className="sync-banner sync-banner-warn" role="status">
        Temporarily unable to load your pull requests — try again shortly.
        {scmAsOf !== null && ` Pull-request rows shown are as of ${formatRelativeTime(scmAsOf)} ago and may be incomplete.`}
      </div>
    )
  }
  if (scmAsOf !== null) {
    return (
      <div className="sync-banner" role="status">
        Pull requests as of {formatRelativeTime(scmAsOf)} ago.
      </div>
    )
  }
  return (
    <div className="sync-banner" role="status">
      No GitHub account linked — pull-request items won't appear here.
    </div>
  )
}

export function DecisionInboxView() {
  const meQuery = useQuery(meQueryOptions)
  const [repoFilter, setRepoFilter] = useState('all')

  const inboxQuery = useQuery({
    queryKey: decisionInboxQueryKeys.list(),
    queryFn: ({ signal }) => listDecisionInbox(signal),
    // The home screen is the one view a signed-in user is expected to
    // leave open longest (mockups.html's own decision 32: "the unit of
    // work is a decision, not a session") -- unlike a single session's own
    // WS-pushed live stream, this read model has no push channel at all,
    // so a moderate background refresh (mirrors SessionSidebar.tsx's own
    // 15s precedent for the identical "no live-update mechanism, but stale
    // data actively misleads" reasoning) is the honest middle ground
    // between "never updates" and hammering the endpoint.
    refetchInterval: 30_000,
  })

  // A stable module-level fallback -- `?? []` inline at the call site
  // would allocate a NEW array reference on every render while
  // inboxQuery.data is still undefined (pending/error), which defeats
  // repoOptions' own useMemo below (a changed dependency every render,
  // recomputing for no reason).
  const items = inboxQuery.data?.items ?? EMPTY_ITEMS

  // Repo-only filter (the inbox is inherently user-scoped -- no user
  // filter). Options are derived from the ALREADY-loaded items' own
  // repoFullName set -- there is no repo-enumeration endpoint for a
  // dropdown to populate from (mirrors AnalyticsView.tsx's own free-text
  // repo entry precedent for the identical reason), and every option this
  // produces is one that could plausibly narrow the CURRENT queue instead
  // of an empty guess.
  const repoOptions = useMemo(() => {
    const set = new Set<string>()
    for (const it of items) {
      if (it.repoFullName !== null) set.add(it.repoFullName)
    }
    return Array.from(set).sort()
  }, [items])

  const visibleItems = repoFilter === 'all' ? items : items.filter((it) => it.repoFullName === repoFilter)

  const role = meQuery.data?.role
  const isAdmin = role === 'admin'
  const canMerge = canMergeDecisionInboxItem(role)

  return (
    <div className="app one">
      <section className="main">
        <div className="anav">
          <span style={{ fontWeight: 600, fontSize: 13 }}>Everything waiting on you</span>
          <select className="sel" aria-label="Filter by repo" value={repoFilter} onChange={(e) => setRepoFilter(e.target.value)}>
            <option value="all">All repos</option>
            {repoOptions.map((repo) => (
              <option key={repo} value={repo}>
                <T text={repo} />
              </option>
            ))}
          </select>
          <span style={{ flex: 1 }} />
          {inboxQuery.isSuccess && (
            <span className="cost">
              median time-to-decision ·{' '}
              {inboxQuery.data.decisionLatencyComputed && inboxQuery.data.decisionLatencyMedianSeconds !== null
                ? formatDecisionLatencySeconds(inboxQuery.data.decisionLatencyMedianSeconds)
                : 'not yet computed'}
            </span>
          )}
        </div>

        {inboxQuery.isSuccess && <ScmStatusBanner scmAsOf={inboxQuery.data.scmAsOf} scmFetchFailed={inboxQuery.data.scmFetchFailed} />}

        {inboxQuery.isPending && (
          <div className="session-state" aria-live="polite">
            <p>Loading your queue…</p>
          </div>
        )}
        {inboxQuery.isError && (
          <div className="session-state" role="alert">
            <p>Couldn't load your decision queue.</p>
          </div>
        )}

        {inboxQuery.isSuccess && (
          <div className="inbox">
            {SECTION_ORDER.filter((kind) => kind !== 'needs_attention' || isAdmin).map((kind) => (
              <Section key={kind} kind={kind} items={visibleItems.filter((it) => it.kind === kind)} canMerge={canMerge} />
            ))}
          </div>
        )}
      </section>
    </div>
  )
}
