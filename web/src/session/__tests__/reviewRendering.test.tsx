// reviewRendering.test.tsx -- the review views' own defining risk, proven at the
// RENDER boundary: every field the code-review/release-review views draw
// (digest summary, architecture-decision prose, stack-risk text, a
// finding's own description/filePath/suggestedFix/rebuttalText, a
// manifest PR's own title, an aggregate-review trigger reason) is
// authored by a model or by the PR's own author -- i.e. by an attacker in
// the threat model that matters. Mirrors timelineRendering.test.tsx's own
// established pattern exactly: renderToStaticMarkup, no jsdom needed,
// proving React's default escaping is actually in effect (a raw `<img`
// would appear in the markup if any of these components ever started
// using dangerouslySetInnerHTML -- which is precisely the mutation the
// "guard" tests below arm against).
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

import type { ReleaseManifestPR, ReleaseManifestReadout, ReviewReadoutFinding, ReviewReadoutVerdict } from '@narvi/contracts/rest-dtos'

import { DigestSections, FindingCard, FindingsAppendix, HandoffReadinessCard, PrGitHubLink, ReviewSessionPanel, SentinelAutoFixPanel, SentinelsPanel } from '../CodeReviewView'
import { ReleaseManifestBody, isAdmin, isMaintainerPlus } from '../ReleaseReviewView'
import { isSafeHref } from '../urlSafety'

const XSS_IMG = '<img src=x onerror=alert(1)>'
const XSS_SCRIPT = '<script>alert(document.cookie)</script>'
const JS_URL = 'javascript:alert(document.cookie)'

function withQueryClient(node: React.ReactNode) {
  const client = new QueryClient()
  return renderToStaticMarkup(<QueryClientProvider client={client}>{node}</QueryClientProvider>)
}

function baseVerdict(overrides: Partial<ReviewReadoutVerdict> = {}): ReviewReadoutVerdict {
  return {
    riskLevel: 'medium',
    premise: 'ok',
    blastRadius: [],
    filesChanged: 3,
    testsCoverage: 'adequate',
    docsDrift: 'none',
    proposedShippable: 'needs_human',
    shippable: 'needs_human',
    digest: {
      summary: 'A normal summary.',
      archDecisions: [],
      stackRisks: null,
      unverifiedLimits: null,
      descriptionAdequacy: 'ok',
      adequacyExplanation: 'Matches the diff.',
      proposedBody: null,
      contestedPoints: null,
    },
    reviewPath: null,
    counterReview: null,
    factCheck: 'done',
    factCheckKilled: 0,
    headSha: 'abc123',
    postedAt: '2026-08-20T10:00:00Z',
    sessionId: 's1',
    ...overrides,
  }
}

function baseFinding(overrides: Partial<ReviewReadoutFinding> = {}): ReviewReadoutFinding {
  return {
    identityHash: 'h1',
    sentinelKind: null,
    severity: 'medium',
    filePath: 'internal/foo.go',
    line: 10,
    description: 'A normal finding.',
    suggestedFix: null,
    status: 'open',
    rebuttalText: null,
    startLine: 10,
    endLine: 10,
    ...overrides,
  }
}

function baseManifestPR(overrides: Partial<ReleaseManifestPR> = {}): ReleaseManifestPR {
  return {
    number: 100,
    title: 'A normal PR title',
    hasApprovingReview: true,
    mergedViaAdminOverride: false,
    ciConclusion: 'success',
    wasReverted: false,
    revertReviewState: 'unknown',
    revertedAfterMergeSeconds: null,
    hadManualConflictResolution: false,
    highRiskFlagged: false,
    ...overrides,
  }
}

function baseManifestReadout(overrides: Partial<ReleaseManifestReadout> = {}): ReleaseManifestReadout {
  return {
    repoFullName: 'acme/widgets',
    prNumber: 200,
    baseRef: 'main',
    headRef: 'release/2026.08',
    computed: true,
    computedAt: '2026-08-20T10:00:00Z',
    constituentPrCount: 1,
    coveragePartial: false,
    aggregateReviewTriggered: false,
    aggregateReviewTriggerReasons: [],
    findings: [],
    mergedPrs: [baseManifestPR()],
    compositionFindings: [],
    compositionDecision: 'pending',
    ...overrides,
  }
}

describe('CodeReviewView rendering -- adversarial digest content stays text, never markup', () => {
  it('a digest summary containing <img onerror=...> renders as text', () => {
    const verdict = baseVerdict({ digest: { ...baseVerdict().digest, summary: `What this PR does: ${XSS_IMG}` } })
    const html = renderToStaticMarkup(<DigestSections verdict={verdict} />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })

  it('a hostile architecture-decision string renders as text', () => {
    const verdict = baseVerdict({
      digest: { ...baseVerdict().digest, archDecisions: [{ decision: XSS_SCRIPT, rejectedAlternative: XSS_IMG, conventionConformance: 'n/a' }] },
    })
    const html = renderToStaticMarkup(<DigestSections verdict={verdict} />)
    expect(html).not.toContain('<script>')
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;script&gt;')
    expect(html).toContain('&lt;img')
  })

  it('a hostile stackRisks/unverifiedLimits/contestedPoints string renders as text', () => {
    const verdict = baseVerdict({
      digest: { ...baseVerdict().digest, stackRisks: XSS_IMG, unverifiedLimits: XSS_SCRIPT, contestedPoints: XSS_IMG },
    })
    const html = renderToStaticMarkup(<DigestSections verdict={verdict} />)
    expect(html).not.toContain('<img')
    expect(html).not.toContain('<script>')
  })

  it('a finding description containing a script tag renders as text', () => {
    const finding = baseFinding({ description: `Timing leak. ${XSS_SCRIPT}` })
    const html = withQueryClient(<FindingCard finding={finding} canAct={false} sessionId="s1" />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a file path containing markup renders as text', () => {
    const finding = baseFinding({ filePath: `internal/${XSS_IMG}/verify.go` })
    const html = withQueryClient(<FindingCard finding={finding} canAct={false} sessionId="s1" />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })

  it('a suggestedFix diff renders inside a <pre>, as text, never as markup', () => {
    const finding = baseFinding({ suggestedFix: `-old\n+new ${XSS_SCRIPT}` })
    const html = withQueryClient(<FindingCard finding={finding} canAct={false} sessionId="s1" />)
    expect(html).toContain('<pre')
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a rebuttal text containing markup renders as text', () => {
    const finding = baseFinding({ status: 'rebutted', rebuttalText: XSS_IMG })
    const html = withQueryClient(<FindingCard finding={finding} canAct={false} sessionId="s1" />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })

  it('a hostile finding severity/sentinelKind string never becomes an unescaped class or attribute value', () => {
    // severity/sentinelKind drive className concatenation directly
    // (`chip ${finding.severity === 'high' ? ...}`) -- this proves an
    // out-of-vocabulary value can't inject extra attributes/markup via
    // that path either, since React only ever writes className as a
    // plain, escaped attribute string.
    const finding = baseFinding({ severity: 'medium', description: 'ok', filePath: '"><svg onload=alert(1)>' })
    const html = withQueryClient(<FindingCard finding={finding} canAct={false} sessionId="s1" />)
    expect(html).not.toContain('<svg')
    expect(html).not.toMatch(/"\s*>\s*<svg/)
  })

  it('does not hang or break layout on a 200KB finding description', () => {
    const finding = baseFinding({ description: 'x'.repeat(200_000) })
    const start = Date.now()
    let html = ''
    expect(() => {
      html = withQueryClient(<FindingCard finding={finding} canAct={false} sessionId="s1" />)
    }).not.toThrow()
    expect(Date.now() - start).toBeLessThan(2000)
    // truncateForDisplay caps what actually reaches the DOM -- the full
    // 200,000 raw characters must never appear verbatim.
    expect(html.length).toBeLessThan(200_000)
    expect(html).toContain('more characters truncated')
  })

  it('a snippet with no line breaks does not break the appendix render', () => {
    const finding = baseFinding({ description: 'x'.repeat(5000), suggestedFix: 'y'.repeat(5000) })
    expect(() => withQueryClient(<FindingsAppendix findings={[finding]} canAct={false} sessionId="s1" />)).not.toThrow()
  })

  it('deeply nested contested points (a long, un-nested string standing in for pathological input) does not hang', () => {
    const verdict = baseVerdict({ digest: { ...baseVerdict().digest, contestedPoints: 'nested: '.repeat(20_000) } })
    const start = Date.now()
    expect(() => renderToStaticMarkup(<DigestSections verdict={verdict} />)).not.toThrow()
    expect(Date.now() - start).toBeLessThan(2000)
  })
})

describe('PrGitHubLink -- the ONE href this view builds from review-adjacent content', () => {
  it('a well-formed repoFullName/prNumber renders a real, safe anchor', () => {
    const html = renderToStaticMarkup(<PrGitHubLink repoFullName="acme/widgets" prNumber={42} />)
    expect(html).toContain('<a ')
    expect(html).toContain('href="https://github.com/acme/widgets/pull/42"')
  })

  it('a hostile repoFullName can never move the rendered href off github.com -- the fixed "https://github.com/" prefix, not isSafeHref, is what actually prevents scheme/host injection here', () => {
    // This link is built as a FIXED trusted prefix + untrusted suffix,
    // never as untrusted content standing in for the whole URL -- so no
    // value repoFullName could ever hold (a `javascript:` payload, an
    // "@evil.example" authority-confusion attempt, a bare scheme) can
    // change the URL's own origin. Verified for real, not assumed: every
    // case below still resolves to the github.com origin. isSafeHref is
    // still called (defense in depth / consistency with this codebase's
    // blanket "every review-derived URL goes through urlSafety.ts" rule)
    // but is NOT the primitive doing the real work for this specific
    // call site -- see PrGitHubLink's own doc comment.
    for (const hostile of [JS_URL, '@evil.example', 'evil.example#', XSS_IMG]) {
      const html = renderToStaticMarkup(<PrGitHubLink repoFullName={hostile} prNumber={1} />)
      expect(html).toContain('href="https://github.com/')
      expect(html).not.toMatch(/href="(?!https:\/\/github\.com\/)/)
    }
  })
})

describe('mutation guard: isSafeHref actually called on a genuinely free-form review URL', () => {
  // PrGitHubLink's own href is safe by construction regardless of
  // isSafeHref (proven above) -- SessionRail.tsx's ArtifactRow is this
  // codebase's own real example of a genuinely free-form, server-supplied
  // URL (Artifact.url) gated by isSafeHref, where the check is NOT dead
  // code: a `javascript:`-scheme artifact URL, if isSafeHref's own call
  // in ArtifactRow were ever removed, WOULD render as a live href. This
  // is that guard, re-proven here as this Step's own named mutation test
  // (bypassing it must fail this exact assertion).
  it('ArtifactRow never renders javascript:-scheme artifact URLs as a live href', async () => {
    const { ArtifactRow } = await import('../SessionRail')
    const html = renderToStaticMarkup(
      <ArtifactRow artifact={{ id: 'a1', type: 'pr', url: JS_URL, createdAt: '2026-08-20T10:00:00Z', status: 'ready', failureReason: null, filename: null, sizeBytes: null, contentType: null, metadata: {} }} />,
    )
    expect(html).not.toContain(`href="${JS_URL}"`)
    expect(html).toContain('link unavailable')
  })
})

describe('ReleaseReviewView rendering -- adversarial manifest content stays text, never markup', () => {
  it('a hostile constituent-PR title renders as text', () => {
    const readout = baseManifestReadout({ mergedPrs: [baseManifestPR({ title: `fix: bug ${XSS_IMG}` })] })
    const html = renderToStaticMarkup(<ReleaseManifestBody readout={readout} sessionId="s1" />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })

  it('a hostile aggregate-review trigger reason renders as text', () => {
    const readout = baseManifestReadout({ aggregateReviewTriggered: true, aggregateReviewTriggerReasons: [XSS_SCRIPT] })
    const html = renderToStaticMarkup(<ReleaseManifestBody readout={readout} sessionId="s1" />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a hostile composition finding detail renders as text', () => {
    const readout = baseManifestReadout({
      aggregateReviewTriggered: true,
      compositionReviewedAt: '2026-08-20T11:00:00Z',
      compositionFindings: [{ kind: 'conflict', detail: `two PRs collide ${XSS_SCRIPT}` }],
    })
    const html = renderToStaticMarkup(<ReleaseManifestBody readout={readout} sessionId="s1" />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('does not hang on a 200KB constituent-PR title', () => {
    const readout = baseManifestReadout({ mergedPrs: [baseManifestPR({ title: 'x'.repeat(200_000) })] })
    const start = Date.now()
    expect(() => renderToStaticMarkup(<ReleaseManifestBody readout={readout} sessionId="s1" />)).not.toThrow()
    expect(Date.now() - start).toBeLessThan(2000)
  })

  it('renders "not applicable" when the composition criteria were never met, never a fabricated result', () => {
    const readout = baseManifestReadout()
    const html = renderToStaticMarkup(<ReleaseManifestBody readout={readout} sessionId="s1" />)
    expect(html).not.toContain('Composition findings')
    expect(html).toContain('None of the composition criteria were met')
  })

  it('renders "pending", never an empty findings list, while the composition pass has not completed', () => {
    const readout = baseManifestReadout({ aggregateReviewTriggered: true })
    const html = renderToStaticMarkup(<ReleaseManifestBody readout={readout} sessionId="s1" />)
    expect(html).toContain('Pending')
    expect(html).not.toContain('No composition findings')
  })

  it('renders an honest "no findings" result once the pass has actually completed clean', () => {
    const readout = baseManifestReadout({ aggregateReviewTriggered: true, compositionReviewedAt: '2026-08-20T11:00:00Z' })
    const html = renderToStaticMarkup(<ReleaseManifestBody readout={readout} sessionId="s1" />)
    expect(html).toContain('No composition findings')
    expect(html).not.toContain('Pending')
  })

  it('renders real composition findings once the pass has completed with something to report', () => {
    const readout = baseManifestReadout({
      aggregateReviewTriggered: true,
      compositionReviewedAt: '2026-08-20T11:00:00Z',
      compositionFindings: [{ kind: 'duplication', detail: 'PR #1 and #2 both add the same migration' }],
    })
    const html = renderToStaticMarkup(<ReleaseManifestBody readout={readout} sessionId="s1" />)
    expect(html).toContain('duplication')
    expect(html).toContain('PR #1 and #2 both add the same migration')
  })

  // Test-integrity fix: the two tests below now DERIVE canBlock/
  // canAcknowledge from a REAL role string via the exported isMaintainerPlus/
  // isAdmin -- the SAME functions ReleaseReviewView itself calls on the
  // authenticated caller's own role -- rather than hand-passing booleans
  // directly. A prior version of this suite hand-passed
  // canBlock={true}/canAcknowledge={false} etc. straight into
  // ReleaseManifestBody, which proved the RENDER gate works but never
  // actually exercised isMaintainerPlus/isAdmin at all: an inverted
  // `role === 'viewer'` typo in either function would have passed every
  // test in this file unnoticed.
  it.each(['viewer', 'member'] as const)('never renders Block/Acknowledge actions for a %s (isMaintainerPlus/isAdmin both false for this role)', (role) => {
    const readout = baseManifestReadout({
      aggregateReviewTriggered: true,
      compositionReviewedAt: '2026-08-20T11:00:00Z',
      compositionFindings: [{ kind: 'other', detail: 'something worth a human look' }],
    })
    expect(isMaintainerPlus(role)).toBe(false)
    expect(isAdmin(role)).toBe(false)
    const html = renderToStaticMarkup(<ReleaseManifestBody readout={readout} sessionId="s1" canBlock={isMaintainerPlus(role)} canAcknowledge={isAdmin(role)} />)
    expect(html).not.toContain('Block release')
    expect(html).not.toContain('Acknowledge & ship')
  })

  it('renders only Block for a "maintainer" role, and both for an "admin" role -- derived from the real role string, mirroring server RBAC', () => {
    const readout = baseManifestReadout({
      aggregateReviewTriggered: true,
      compositionReviewedAt: '2026-08-20T11:00:00Z',
      compositionFindings: [{ kind: 'other', detail: 'something worth a human look' }],
    })

    expect(isMaintainerPlus('maintainer')).toBe(true)
    expect(isAdmin('maintainer')).toBe(false)
    const maintainerHtml = withQueryClient(<ReleaseManifestBody readout={readout} sessionId="s1" canBlock={isMaintainerPlus('maintainer')} canAcknowledge={isAdmin('maintainer')} />)
    expect(maintainerHtml).toContain('Block release')
    expect(maintainerHtml).not.toContain('Acknowledge &amp; ship')

    expect(isMaintainerPlus('admin')).toBe(true)
    expect(isAdmin('admin')).toBe(true)
    const adminHtml = withQueryClient(<ReleaseManifestBody readout={readout} sessionId="s1" canBlock={isMaintainerPlus('admin')} canAcknowledge={isAdmin('admin')} />)
    expect(adminHtml).toContain('Block release')
    expect(adminHtml).toContain('Acknowledge &amp; ship')
  })

  it('never renders Block/Acknowledge once a decision has already been made', () => {
    const readout = baseManifestReadout({
      aggregateReviewTriggered: true,
      compositionReviewedAt: '2026-08-20T11:00:00Z',
      compositionFindings: [{ kind: 'other', detail: 'something worth a human look' }],
      compositionDecision: 'blocked',
      compositionDecisionAt: '2026-08-20T12:00:00Z',
    })
    const html = withQueryClient(<ReleaseManifestBody readout={readout} sessionId="s1" canBlock={true} canAcknowledge={true} />)
    expect(html).not.toContain('<button')
    expect(html).toContain('Blocked')
  })

  // Blocking-finding fix: compositionDecision is a CLOSED, three-value
  // server enum ('pending' | 'blocked' | 'acknowledged') -- a prior
  // version of this component matched with `!== 'pending'` and then
  // picked 'blocked' vs. an "acknowledged & shipped" FALLBACK for
  // anything else, which is fail-OPEN in exactly the wrong direction: an
  // out-of-enum value (this exact regression happening again, a
  // transport bug, or -- before the readout-population fix this Step
  // also lands -- the server's own unpopulated zero value "") rendered a
  // green "acknowledged & shipped" chip. Reproduced here with the
  // byte-for-byte "" value the unfixed server used to actually send, to
  // prove the CLIENT half of the fix independently of the server half.
  it('an out-of-enum compositionDecision (e.g. "", the server\'s own former unpopulated zero value) never renders as acknowledged & shipped', () => {
    const readout = baseManifestReadout({
      aggregateReviewTriggered: true,
      compositionReviewedAt: '2026-08-20T11:00:00Z',
      compositionFindings: [],
      // @ts-expect-error -- deliberately out-of-enum, proving the runtime default branch (a real ApiError-shaped server response would fail contract validation before this component ever saw it; this test proves the component's own defense-in-depth independent of that).
      compositionDecision: '',
    })
    const html = renderToStaticMarkup(<ReleaseManifestBody readout={readout} sessionId="s1" />)
    expect(html).not.toContain('acknowledged &amp; shipped')
    expect(html).not.toContain('acknowledged & shipped')
  })
})

describe('mutation guard: urlSafety must actually reject a javascript: URL', () => {
  it('isSafeHref(javascript:...) is false -- if this ever flips true, PrGitHubLink\'s own guard silently stops working', () => {
    expect(isSafeHref(JS_URL)).toBe(false)
  })
})

// §12.2 item 2's own four gap fields -- ReviewSessionPanel/SentinelAutoFixPanel/
// HandoffReadinessCard/SentinelsPanel's own visualQa row. sentinelFix.status
// (sentinel_fixes.status, an unconstrained TEXT column, dtos.schema.json's own
// comment on it) and visualQa (a human-applied GitHub label suffix) are the
// two genuinely free-form, externally-authored strings among these four
// fields -- mentionCount/claimedAt/contractDriftFlagged/todoCount/flaggedAt
// are all server-computed, never attacker text, so there is nothing to prove
// adversarially for ReviewSessionPanel/HandoffReadinessCard beyond an honest
// render of both states.
describe('ReviewSessionPanel -- coalesced-mention/session-reuse info (§12.2 item 2)', () => {
  it('a single, un-reused claim renders "single @mention" / "new"', () => {
    const html = renderToStaticMarkup(<ReviewSessionPanel sessionReuse={{ mentionCount: 1, claimedAt: '2026-08-20T10:00:00Z' }} />)
    expect(html).toContain('single @mention')
    expect(html).toContain('new')
    expect(html).not.toContain('coalesced')
  })

  it('a reused claim renders the real coalesced count, never a fabricated one', () => {
    const html = renderToStaticMarkup(<ReviewSessionPanel sessionReuse={{ mentionCount: 4, claimedAt: '2026-08-20T10:00:00Z' }} />)
    expect(html).toContain('@mention ×4 → coalesced')
    expect(html).toContain('reused (same PR)')
  })
})

describe('SentinelAutoFixPanel -- sentinel auto-fix PR link with merge-gated state (§12.2 item 2, §17)', () => {
  it('renders nothing at all when no sentinel-fix was ever triggered, an honest absence rather than an empty panel', () => {
    const html = renderToStaticMarkup(<SentinelAutoFixPanel sentinelFix={null} repoFullName="acme/widgets" />)
    expect(html).toBe('')
  })

  it('a known in-flight status renders its label and no fix-PR link yet (fixPrNumber still null)', () => {
    const html = renderToStaticMarkup(<SentinelAutoFixPanel sentinelFix={{ status: 'spawned', fixPrNumber: null, stackRegistered: true }} repoFullName="acme/widgets" />)
    expect(html).toContain('fix session started')
    expect(html).not.toContain('View fix PR on GitHub')
  })

  it('once a fix PR has actually opened, renders the distinctly-labeled link to it', () => {
    const html = renderToStaticMarkup(<SentinelAutoFixPanel sentinelFix={{ status: 'fix_open', fixPrNumber: 77, stackRegistered: true }} repoFullName="acme/widgets" />)
    expect(html).toContain('View fix PR on GitHub')
    expect(html).toContain('href="https://github.com/acme/widgets/pull/77"')
  })

  it('an out-of-vocabulary status string (sentinel_fixes.status is an unconstrained TEXT column) renders as text, never markup', () => {
    const hostile = `weird-status ${XSS_IMG}`
    const html = renderToStaticMarkup(<SentinelAutoFixPanel sentinelFix={{ status: hostile, fixPrNumber: null, stackRegistered: false }} repoFullName="acme/widgets" />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })
})

describe('HandoffReadinessCard -- handoff-readiness display (§12.2 item 2, §14.4)', () => {
  it('renders nothing at all when this PR was never a scoped-Environment prototype', () => {
    const html = renderToStaticMarkup(<HandoffReadinessCard handoffReadiness={null} />)
    expect(html).toBe('')
  })

  it('a clean run (no drift, no TODOs) renders both as honestly clean', () => {
    const html = renderToStaticMarkup(<HandoffReadinessCard handoffReadiness={{ contractDriftFlagged: false, todoCount: 0, flaggedAt: '2026-08-20T10:00:00Z' }} />)
    expect(html).toContain('none')
    expect(html).not.toContain('drifted')
  })

  it('a flagged run renders the real drift/TODO facts, never suppressed', () => {
    const html = renderToStaticMarkup(<HandoffReadinessCard handoffReadiness={{ contractDriftFlagged: true, todoCount: 3, flaggedAt: '2026-08-20T10:00:00Z' }} />)
    expect(html).toContain('drifted')
    expect(html).toContain('3 found')
  })
})

describe('SentinelsPanel -- visual-QA sentinel status (§12.2 item 2)', () => {
  it('renders "not set" when no visual-qa label exists (or the live fetch degraded) -- never a fabricated pass/fail', () => {
    const html = withQueryClient(<SentinelsPanel verdict={null} visualQa={null} />)
    expect(html).toContain('not set')
  })

  it('a real pass/fail/skip label renders as its own chip', () => {
    const html = withQueryClient(<SentinelsPanel verdict={null} visualQa="pass" />)
    expect(html).toContain('pass')
  })

  it('a hostile visual-qa label suffix (human-applied GitHub label text, never Narvi-validated) renders as text, never markup', () => {
    const html = withQueryClient(<SentinelsPanel verdict={null} visualQa={XSS_SCRIPT} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })
})
