// decisionInboxRendering.test.tsx -- DecisionInboxView.tsx's own defining
// risk, proven at the RENDER boundary: item.title (a PR/plan/session/
// automation title), repoFullName, provenanceRepoFullName/
// provenancePattern (a CODEOWNERS pattern), failureReason, artifactSummary,
// and lastError are ALL third-party or model-influenced free text --
// authored by a GitHub PR's own author, a repo's own CODEOWNERS file, or
// upstream error text this codebase does not control. Mirrors
// reviewRendering.test.tsx/membersRendering.test.tsx's own established
// pattern exactly: renderToStaticMarkup, no jsdom needed, proving React's
// default escaping is actually in effect.
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

import type { DecisionInboxItem } from '@narvi/contracts/rest-dtos'

import { DecisionInboxRow, ScmStatusBanner } from '../DecisionInboxView'
import { isSafeHref } from '../urlSafety'

const XSS_IMG = '<img src=x onerror=alert(1)>'
const XSS_SCRIPT = '<script>alert(document.cookie)</script>'
const JS_URL = 'javascript:alert(document.cookie)'

function withQueryClient(node: React.ReactNode) {
  const client = new QueryClient()
  return renderToStaticMarkup(<QueryClientProvider client={client}>{node}</QueryClientProvider>)
}

function baseItem(overrides: Partial<DecisionInboxItem> = {}): DecisionInboxItem {
  return {
    kind: 'ready_to_merge',
    title: 'A normal title',
    enteredQueueAt: '2026-08-20T00:00:00Z',
    ageSeconds: 3600,
    stale: false,
    repoFullName: null,
    prNumber: null,
    htmlUrl: null,
    headSha: null,
    provenanceKind: null,
    provenanceRepoFullName: null,
    provenancePattern: null,
    riskLabel: null,
    ciGreen: null,
    findings: null,
    isHandoff: null,
    hasApprovingReview: null,
    hasChangesRequested: null,
    verdictId: null,
    acceptanceId: null,
    acceptanceJustification: null,
    acceptanceMergeable: null,
    acceptanceMergeBlockedReason: null,
    acceptedAt: null,
    acceptedBy: null,
    isRelease: null,
    manifestFindingsCount: null,
    manifestCoveragePartial: null,
    aggregateReviewTriggered: null,
    planId: null,
    sessionId: null,
    failureReason: null,
    automationId: null,
    artifactSummary: null,
    outboxId: null,
    outboxKind: null,
    lastError: null,
    ...overrides,
  }
}

function prItem(overrides: Partial<DecisionInboxItem> = {}): DecisionInboxItem {
  return baseItem({
    repoFullName: 'acme/widgets',
    prNumber: 100,
    htmlUrl: 'https://github.com/acme/widgets/pull/100',
    isHandoff: false,
    ciGreen: true,
    hasChangesRequested: false,
    hasApprovingReview: true,
    findings: 0,
    ...overrides,
  })
}

describe('DecisionInboxRow -- adversarial PR title stays text, never markup', () => {
  it('a hostile title renders as text, in every row shape that can be rendered outside a real router', () => {
    // The plan shape (kind=awaiting_approval, planId set) is deliberately
    // excluded from this sweep -- ApprovePlanButton always renders a
    // TanStack Router <Link>, which requires a real RouterProvider/router
    // context (this codebase has no precedent for unit-rendering a
    // <Link>-bearing component outside the real app -- see
    // automationRendering.test.tsx's own identical note on RunRow's
    // "session ->" link). The plan shape's own title goes through the
    // exact SAME shared <T text={item.title} /> call at the top of
    // DecisionInboxRow every shape below already proves safe -- there is
    // no shape-specific title-rendering logic left unproven by excluding
    // it here.
    const shapes: DecisionInboxItem[] = [
      prItem({ kind: 'ready_to_merge', title: `fix: bug ${XSS_IMG}` }),
      prItem({ kind: 'needs_review', title: `fix: bug ${XSS_SCRIPT}` }),
      baseItem({ kind: 'needs_attention', sessionId: 's1', title: `session: ${XSS_SCRIPT}` }),
      baseItem({ kind: 'needs_attention', automationId: 'a1', title: `automation: ${XSS_IMG}` }),
    ]
    for (const item of shapes) {
      const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
      expect(html).not.toContain('<img')
      expect(html).not.toContain('<script>')
    }
  })
})

describe('DecisionInboxRow -- adversarial provenance/repo content stays text', () => {
  it('a hostile provenanceRepoFullName (requested_reviewer) renders as text', () => {
    const item = prItem({ kind: 'needs_review', provenanceKind: 'requested_reviewer', provenanceRepoFullName: `evil/${XSS_IMG}` })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })

  it('a hostile CODEOWNERS provenancePattern (codeowners) renders as text', () => {
    const item = prItem({ kind: 'ready_to_merge', provenanceKind: 'codeowners', provenancePattern: `internal/**/${XSS_SCRIPT}` })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })
})

describe('DecisionInboxRow -- adversarial failureReason/artifactSummary/lastError stay text', () => {
  it('a hostile failureReason renders as text on a needs_attention session row', () => {
    const item = baseItem({ kind: 'needs_attention', sessionId: 's1', failureReason: `timeout ${XSS_SCRIPT}` })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a hostile artifactSummary renders as text on a needs_attention automation row', () => {
    const item = baseItem({ kind: 'needs_attention', automationId: 'a1', artifactSummary: `paused: ${XSS_IMG}` })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })

  it('a hostile lastError renders as text on a needs_attention outbox row', () => {
    const item = baseItem({ kind: 'needs_attention', outboxId: 'o1', outboxKind: 'slack_delivery', lastError: `500: ${XSS_SCRIPT}` })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('does not hang or break layout on a 200KB title', () => {
    const item = prItem({ kind: 'needs_review', title: 'x'.repeat(200_000) })
    const start = Date.now()
    let html = ''
    expect(() => {
      html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    }).not.toThrow()
    expect(Date.now() - start).toBeLessThan(2000)
    expect(html.length).toBeLessThan(200_000)
    expect(html).toContain('more characters truncated')
  })
})

describe('DecisionInboxRow -- htmlUrl is the ONLY field that becomes an href, and only when isSafeHref accepts it', () => {
  it('a well-formed GitHub htmlUrl on a needs_review row renders a real, safe anchor', () => {
    const item = prItem({ kind: 'needs_review', htmlUrl: 'https://github.com/acme/widgets/pull/100' })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).toContain('href="https://github.com/acme/widgets/pull/100"')
  })

  it('a javascript:-scheme htmlUrl never renders as a live href -- "link unavailable" instead', () => {
    const item = prItem({ kind: 'needs_review', htmlUrl: JS_URL })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain(`href="${JS_URL}"`)
    expect(html).toContain('link unavailable')
  })

  it('a handoff PR row (awaiting_approval) uses the same htmlUrl guard', () => {
    const item = prItem({ kind: 'awaiting_approval', isHandoff: true, htmlUrl: JS_URL })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain(`href="${JS_URL}"`)
    expect(html).toContain('link unavailable')
  })

  it('a null htmlUrl renders "link unavailable", never a broken empty href', () => {
    const item = prItem({ kind: 'needs_review', htmlUrl: null })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain('href=""')
    expect(html).toContain('link unavailable')
  })
})

// OpenReviewLink/OpenReleaseReviewLink both render a TanStack Router
// <Link>, which -- like ApprovePlanButton's own identical <Link> above --
// needs a real RouterProvider/router context this file's own
// renderToStaticMarkup harness does not provide (this file's own top
// comment on the adversarial-title sweep explains why the plan shape is
// excluded for the identical reason). Every case below therefore holds
// sessionId at its default null, which is exactly the branch that must
// NEVER attempt to render either Link: a null sessionId means the server
// found no review session for this PR, and DecisionInboxRow falls back to
// OpenOnGitHubLink instead -- proving that fallback fires (and no Link
// is attempted) is exactly what CAN be proven at this render layer
// without a router; that a REAL sessionId instead renders a working
// review link is covered by decisionInboxFormat.test.ts's own rowKind
// coverage and by the backend's own wire-level tests (aggregate_
// integration_test.go, decisioninbox_integration_test.go).
describe('DecisionInboxRow -- a PR-shaped row with no resolvable review session falls back to the external GitHub link', () => {
  it('a needs_review PR row with sessionId=null renders "Open on GitHub", never attempts the review-screen Link', () => {
    const item = prItem({ kind: 'needs_review', sessionId: null, htmlUrl: 'https://github.com/acme/widgets/pull/100' })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).toContain('Open on GitHub')
    expect(html).not.toContain('Open review')
  })

  it('a release-cut row with sessionId=null ALSO falls back to the external GitHub link', () => {
    const item = prItem({ kind: 'needs_review', isRelease: true, sessionId: null, htmlUrl: 'https://github.com/acme/widgets/pull/200' })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).toContain('Open on GitHub')
    expect(html).not.toContain('Open release review')
  })

  it('a release-cut row renders its own manifest chip, never the ordinary PR risk/CI chips', () => {
    const item = prItem({ kind: 'needs_review', isRelease: true, manifestFindingsCount: 2, riskLabel: 'review:high-risk', ciGreen: true })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).toContain('manifest: 2 flags')
    expect(html).not.toContain('review: high risk')
  })
})

describe('DecisionInboxRow -- hasChangesRequested, not hasApprovingReview, gates the Merge button', () => {
  it('pre-disables Merge and explains why when hasChangesRequested is true', () => {
    const item = prItem({ kind: 'ready_to_merge', hasChangesRequested: true, hasApprovingReview: false })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).toContain('<button')
    expect(html).toMatch(/<button[^>]*disabled/)
  })

  // hasApprovingReview is display-only: DecisionInboxItem's own schema says it
  // is "NEVER what kind=ready_to_merge's own 'approved' means", while
  // hasChangesRequested "DOES gate an action". These two cases pin exactly
  // that asymmetry by holding hasChangesRequested fixed at false and flipping
  // hasApprovingReview across both values -- the earlier single case set it to
  // false and so never touched the field it was named for.
  //
  // The assertion must also be able to SEE a disabled button. A /<button[^>]*>/
  // pattern cannot: [^>]* swallows the whole attribute list, `disabled` included,
  // so it matches either way. Asserting on the absence of the disabled attribute
  // is what makes gating on the wrong field fail this test.
  it('a false hasApprovingReview does not disable Merge -- display only', () => {
    const item = prItem({ kind: 'ready_to_merge', hasChangesRequested: false, hasApprovingReview: false })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).toContain('Merge')
    expect(html).not.toMatch(/<button[^>]*disabled/)
  })

  it('a true hasApprovingReview does not enable Merge on its own either -- hasChangesRequested is the gate', () => {
    const displayOnly = prItem({ kind: 'ready_to_merge', hasChangesRequested: false, hasApprovingReview: true })
    const html = withQueryClient(<DecisionInboxRow item={displayOnly} canMerge={true} />)
    expect(html).toContain('Merge')
    expect(html).not.toMatch(/<button[^>]*disabled/)

    // The gate is hasChangesRequested, and it wins regardless of an approving
    // review being present.
    const blocked = prItem({ kind: 'ready_to_merge', hasChangesRequested: true, hasApprovingReview: true })
    const blockedHtml = withQueryClient(<DecisionInboxRow item={blocked} canMerge={true} />)
    expect(blockedHtml).toMatch(/<button[^>]*disabled/)
  })
})

// finding F5 (adversarial review): the aggregate stays DELIBERATELY
// acceptance-blind (an accepted PR still classifies needs_review, never
// ready_to_merge -- decisioninbox.Item.AcceptanceID's own doc comment,
// server-side), which used to mean this row rendered NO way to act on it
// beyond "Open review" -- even though the acceptance's own justification
// was already sitting in the API response. These cases pin that the
// Merge button and the justification text now BOTH render for exactly
// that row shape, and NEITHER renders for an ordinary, never-accepted
// needs_review row.
//
// Round 3, finding R1 (adversarial review, corrected): the Merge button
// itself must ALSO gate on acceptanceMergeable, the server's own answer
// to "does this acceptance actually unblock a merge right now" -- never
// on acceptanceId's own presence alone, which is true the instant a
// maintainer+ accepts a verdict regardless of whether some OTHER,
// mandatory criterion (an open finding, changes requested...) still
// blocks it. Both directions are pinned below: a mergeable acceptance
// still shows the button (unchanged from before this fix), and a
// non-mergeable one shows neither the button nor a false "still stuck"
// silence -- an honest "Still blocked: <reason>" line instead.
describe('DecisionInboxRow -- an accepted-but-refused PR (§21.1b) gets a real surface, not just "Open review"', () => {
  it('a needs_review row with a MERGEABLE active acceptance renders the Merge button', () => {
    const item = prItem({ kind: 'needs_review', acceptanceId: 'acceptance-1', acceptanceJustification: 'Reviewed offline; risk accepted.', acceptanceMergeable: true })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).toContain('>Merge<')
  })

  it("a needs_review row with an active acceptance renders the maintainer's own justification as text", () => {
    const item = prItem({ kind: 'needs_review', acceptanceId: 'acceptance-1', acceptanceJustification: 'Reviewed offline; risk accepted.', acceptanceMergeable: true })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).toContain('Reviewed offline; risk accepted.')
    expect(html).toContain('accepted override')
  })

  // Round 3, finding R7 (adversarial review): round 2 added acceptedBy/
  // acceptedAt to the wire (Item.AcceptedByUserID's own doc comment: "the
  // missing 'by whom'") specifically so a maintainer scanning needs_review
  // could see who authorised a refusal and when -- but nothing here ever
  // rendered either field, so the row still read as anonymous and
  // undated. Both must now actually appear.
  it("a needs_review row with an active acceptance renders WHO accepted it and WHEN", () => {
    const item = prItem({
      kind: 'needs_review',
      acceptanceId: 'acceptance-1',
      acceptanceJustification: 'Reviewed offline; risk accepted.',
      acceptanceMergeable: true,
      acceptedBy: '11111111-1111-1111-1111-111111111111',
      acceptedAt: '2026-08-20T10:00:00Z',
    })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).toContain('11111111-1111-1111-1111-111111111111')
    expect(html).toContain(new Date('2026-08-20T10:00:00Z').toLocaleString())
  })

  it('a hostile acceptedBy renders as text, never markup (defense in depth -- a raw user id, never actually attacker-authored)', () => {
    const item = prItem({
      kind: 'needs_review',
      acceptanceId: 'acceptance-1',
      acceptanceJustification: 'Reviewed offline; risk accepted.',
      acceptanceMergeable: true,
      acceptedBy: XSS_SCRIPT,
      acceptedAt: null,
    })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a null acceptedBy (the accepting user was since deleted) renders no "by" clause, never a broken/empty one', () => {
    const item = prItem({
      kind: 'needs_review',
      acceptanceId: 'acceptance-1',
      acceptanceJustification: 'Reviewed offline; risk accepted.',
      acceptanceMergeable: true,
      acceptedBy: null,
      acceptedAt: '2026-08-20T10:00:00Z',
    })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain(' by ')
    expect(html).toContain(new Date('2026-08-20T10:00:00Z').toLocaleString())
  })

  it('a hostile justification renders as text, never markup', () => {
    const item = prItem({ kind: 'needs_review', acceptanceId: 'acceptance-1', acceptanceJustification: XSS_IMG, acceptanceMergeable: true })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain('<img')
  })

  it('an ordinary needs_review row (never accepted) renders NEITHER the Merge button NOR any acceptance text', () => {
    const item = prItem({ kind: 'needs_review', acceptanceId: null, acceptanceJustification: null, acceptanceMergeable: null })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain('>Merge<')
    expect(html).not.toContain('accepted override')
  })

  // finding R1's own decisive case: an acceptance exists (the chip and
  // justification still render -- a maintainer must still be able to SEE
  // it was accepted), but the server reports it does NOT currently
  // unblock a merge (an open finding, changes requested, or any other
  // mandatory criterion). Before this fix, hasAcceptedOverride alone
  // gated the button, so this exact row shape rendered an ENABLED Merge
  // button that RevalidateForMerge would unconditionally 409 on click.
  it('a needs_review row with an acceptance that is NOT currently mergeable renders NO Merge button, and explains why', () => {
    const item = prItem({
      kind: 'needs_review',
      acceptanceId: 'acceptance-1',
      acceptanceJustification: 'Reviewed offline; risk accepted.',
      acceptanceMergeable: false,
      acceptanceMergeBlockedReason: 'this pull request has an open, unresolved review finding',
    })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain('>Merge<')
    // The acceptance itself must still be visible -- never silently
    // indistinguishable from "never accepted at all".
    expect(html).toContain('accepted override')
    expect(html).toContain('Reviewed offline; risk accepted.')
    expect(html).toContain('Still blocked')
    expect(html).toContain('this pull request has an open, unresolved review finding')
  })

  it('a hostile acceptanceMergeBlockedReason renders as text, never markup', () => {
    const item = prItem({
      kind: 'needs_review',
      acceptanceId: 'acceptance-1',
      acceptanceJustification: 'Reviewed offline; risk accepted.',
      acceptanceMergeable: false,
      acceptanceMergeBlockedReason: XSS_SCRIPT,
    })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  // The OTHER direction, named explicitly by finding R1 itself: a client
  // that hid the button whenever the server WOULD permit the merge is
  // just as much a defect as one that shows a button that 409s. Ready_to_
  // merge's own gate is unaffected by acceptanceMergeable (that field is
  // only ever meaningful for the needs_review+acceptance combination),
  // so a ready_to_merge row with acceptanceMergeable left null/false must
  // still show the button, exactly as it always has.
  it('a ready_to_merge row shows the Merge button regardless of acceptanceMergeable (a different row shape entirely)', () => {
    const item = prItem({ kind: 'ready_to_merge', acceptanceId: null, acceptanceMergeable: null })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={true} />)
    expect(html).toContain('>Merge<')
  })
})

describe('DecisionInboxRow -- viewer role sees a read-only queue (§16.2)', () => {
  it('canMerge=false on a ready_to_merge row renders no Merge button at all', () => {
    const item = prItem({ kind: 'ready_to_merge' })
    const html = withQueryClient(<DecisionInboxRow item={item} canMerge={false} />)
    expect(html).not.toContain('>Merge<')
    expect(html).toContain('read-only')
  })
})

describe('mutation guard: isSafeHref actually called on decision-inbox htmlUrl', () => {
  it('isSafeHref(javascript:...) is false -- if this ever flips true, DecisionInboxRow\'s own guard silently stops working', () => {
    expect(isSafeHref(JS_URL)).toBe(false)
  })
})

// ScmStatusBanner's own three-way SCM state (§16.2, this file's own top
// comment): the two wire fields, scmAsOf and scmFetchFailed, are NOT
// mutually exclusive (ListDecisionInboxResponse.scmFetchFailed's own doc
// comment) -- a partial-but-real fetch carries a real as-of instant AND
// the "may be incomplete" flag together. Before this suite, none of the
// three states had a render test at all.
describe('ScmStatusBanner -- the three-way SCM state, never collapsed', () => {
  it('no GitHub linked (scmAsOf null, scmFetchFailed false) renders the honest empty-link state, not a warning', () => {
    const html = renderToStaticMarkup(<ScmStatusBanner scmAsOf={null} scmFetchFailed={false} />)
    expect(html).toContain('No GitHub account linked')
    expect(html).not.toContain('sync-banner-warn')
  })

  it('a real, complete fetch (scmAsOf set, scmFetchFailed false) renders only the as-of staleness marker, no warning', () => {
    const html = renderToStaticMarkup(<ScmStatusBanner scmAsOf="2026-08-20T10:00:00Z" scmFetchFailed={false} />)
    expect(html).toContain('Pull requests as of')
    expect(html).not.toContain('sync-banner-warn')
    expect(html).not.toContain('Temporarily unable')
  })

  it('a fetch that failed outright (scmAsOf null, scmFetchFailed true) renders the warning with no staleness marker -- no fetch was even attempted', () => {
    const html = renderToStaticMarkup(<ScmStatusBanner scmAsOf={null} scmFetchFailed={true} />)
    expect(html).toContain('sync-banner-warn')
    expect(html).toContain('Temporarily unable to load your pull requests')
    expect(html).not.toContain('Pull-request rows shown are as of')
  })

  // The genuine partial-fetch state this Step exists to cover: BOTH
  // fields carried together, proving the two are read as independent
  // signals rather than as an if/else pair that could only ever show
  // one or the other.
  it('a genuine partial fetch (scmAsOf set AND scmFetchFailed true) renders the warning WITH the as-of marker, distinctly from either field alone', () => {
    const html = renderToStaticMarkup(<ScmStatusBanner scmAsOf="2026-08-20T10:00:00Z" scmFetchFailed={true} />)
    expect(html).toContain('sync-banner-warn')
    expect(html).toContain('Temporarily unable to load your pull requests')
    expect(html).toContain('Pull-request rows shown are as of')
    expect(html).toContain('may be incomplete')
    // Distinct from the "failed outright" case above: this specific
    // combination adds the staleness clause the outright-failure case
    // must never show (no fetch to be stale from, in that case).
    expect(html).not.toBe(renderToStaticMarkup(<ScmStatusBanner scmAsOf={null} scmFetchFailed={true} />))
  })
})
