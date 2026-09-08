import { describe, expect, it } from 'vitest'

import type { DecisionInboxItem } from '@narvi/contracts/rest-dtos'

import {
  canMergeDecisionInboxItem,
  formatAgeSeconds,
  formatDecisionLatencySeconds,
  prChipData,
  provenanceText,
  releaseChipData,
  rowKeyFor,
  rowKind,
  sectionBlurb,
  sectionTitle,
} from '../decisionInboxFormat'

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

describe('rowKind -- derives the row-type tag from field presence, never from `kind`', () => {
  it('a PR row (repoFullName set, not a handoff) is "pr"', () => {
    expect(rowKind(baseItem({ repoFullName: 'acme/widgets', prNumber: 1, isHandoff: false }))).toBe('pr')
  })

  it('a handoff PR row (isHandoff=true) inside kind=awaiting_approval is "handoff", not "pr"', () => {
    expect(rowKind(baseItem({ kind: 'awaiting_approval', repoFullName: 'acme/widgets', prNumber: 1, isHandoff: true }))).toBe('handoff')
  })

  it('a release-cut PR row (isRelease=true) inside kind=needs_review is "release", not "pr"', () => {
    expect(rowKind(baseItem({ kind: 'needs_review', repoFullName: 'acme/rockets', prNumber: 5, isHandoff: false, isRelease: true }))).toBe('release')
  })

  it('isRelease takes precedence over isHandoff being false, but never over isHandoff being true', () => {
    // A row cannot legitimately carry both flags true in practice (a
    // scoped-session handoff PR is not also a release cut), but the
    // dispatch ORDER itself is worth pinning: isHandoff is checked FIRST,
    // so a (hypothetically) mis-tagged row never silently renders as a
    // release cut instead of a handoff.
    expect(rowKind(baseItem({ kind: 'awaiting_approval', repoFullName: 'acme/rockets', prNumber: 5, isHandoff: true, isRelease: true }))).toBe('handoff')
  })

  it('a plan row is "plan"', () => {
    expect(rowKind(baseItem({ kind: 'awaiting_approval', planId: 'p1', sessionId: 's1' }))).toBe('plan')
  })

  it('an automation row is "automation"', () => {
    expect(rowKind(baseItem({ kind: 'needs_attention', automationId: 'a1' }))).toBe('automation')
  })

  it('an outbox row is "outbox"', () => {
    expect(rowKind(baseItem({ kind: 'needs_attention', outboxId: 'o1' }))).toBe('outbox')
  })

  it('a failed-session row is "session"', () => {
    expect(rowKind(baseItem({ kind: 'needs_attention', sessionId: 's1' }))).toBe('session')
  })
})

describe('provenanceText -- decision 34: every row says why it is yours', () => {
  it('renders "assigned to you directly"', () => {
    expect(provenanceText({ provenanceKind: 'assigned_directly', provenanceRepoFullName: null, provenancePattern: null })).toBe('assigned to you directly')
  })

  it('renders the requesting repo for requested_reviewer', () => {
    expect(provenanceText({ provenanceKind: 'requested_reviewer', provenanceRepoFullName: 'acme/payroll-api', provenancePattern: null })).toBe('requested reviewer · acme/payroll-api')
  })

  it('renders the winning CODEOWNERS pattern for codeowners', () => {
    expect(provenanceText({ provenanceKind: 'codeowners', provenanceRepoFullName: null, provenancePattern: 'internal/app/scheduler/**' })).toBe('yours via CODEOWNERS · internal/app/scheduler/**')
  })

  it('renders null (nothing) when provenanceKind is null -- never "unknown"', () => {
    expect(provenanceText({ provenanceKind: null, provenanceRepoFullName: null, provenancePattern: null })).toBeNull()
  })
})

describe('riskLabel chips via prChipData -- the wire value is "review:high-risk" etc, not "high"', () => {
  it('a high-risk label with open findings combines into one chip', () => {
    const chips = prChipData({ riskLabel: 'review:high-risk', findings: 2, ciGreen: true, hasChangesRequested: false })
    expect(chips[0]).toEqual({ tone: 'crit', text: 'review: high risk · 2 findings' })
    expect(chips).toContainEqual({ tone: 'ok', text: 'CI green' })
  })

  it('a low-risk label with zero findings omits the findings suffix', () => {
    const chips = prChipData({ riskLabel: 'review:low-risk', findings: 0, ciGreen: true, hasChangesRequested: false })
    expect(chips[0]).toEqual({ tone: 'ok', text: 'review: low risk' })
  })

  it('null findings (could not be determined) never renders as a fabricated "0 findings"', () => {
    const chips = prChipData({ riskLabel: 'review:medium-risk', findings: null, ciGreen: true, hasChangesRequested: false })
    expect(chips[0]).toEqual({ tone: 'warn', text: 'review: medium risk' })
  })

  it('null ciGreen (not a PR-shaped fact yet known) renders no CI chip at all', () => {
    const chips = prChipData({ riskLabel: null, findings: null, ciGreen: null, hasChangesRequested: false })
    expect(chips.some((c) => c.text.includes('CI'))).toBe(false)
  })

  it('hasChangesRequested=true adds its own explicit chip', () => {
    const chips = prChipData({ riskLabel: null, findings: null, ciGreen: true, hasChangesRequested: true })
    expect(chips).toContainEqual({ tone: 'crit', text: 'changes requested' })
  })
})

describe('releaseChipData -- a release-cut row never fabricates an aggregate-findings count', () => {
  it('zero manifest findings renders an "ok"-toned chip, not "crit"', () => {
    const chips = releaseChipData({ manifestFindingsCount: 0, manifestCoveragePartial: false, aggregateReviewTriggered: false })
    expect(chips).toEqual([{ tone: 'ok', text: 'manifest: 0 flags' }])
  })

  it('one manifest finding uses the singular "flag", not "flags"', () => {
    const chips = releaseChipData({ manifestFindingsCount: 1, manifestCoveragePartial: false, aggregateReviewTriggered: false })
    expect(chips[0]).toEqual({ tone: 'crit', text: 'manifest: 1 flag' })
  })

  it('multiple manifest findings render a "crit"-toned chip', () => {
    const chips = releaseChipData({ manifestFindingsCount: 3, manifestCoveragePartial: false, aggregateReviewTriggered: false })
    expect(chips[0]).toEqual({ tone: 'crit', text: 'manifest: 3 flags' })
  })

  it('a null manifestFindingsCount (not actually a release cut) renders no manifest chip at all', () => {
    const chips = releaseChipData({ manifestFindingsCount: null, manifestCoveragePartial: null, aggregateReviewTriggered: null })
    expect(chips.some((c) => c.text.includes('manifest'))).toBe(false)
  })

  it('aggregateReviewTriggered=true adds an honest "needed" chip, never a fabricated finding count -- the aggregate diff review pass itself is not computed anywhere in this system', () => {
    const chips = releaseChipData({ manifestFindingsCount: 0, manifestCoveragePartial: false, aggregateReviewTriggered: true })
    expect(chips).toContainEqual({ tone: 'warn', text: 'aggregate review needed' })
    expect(chips.some((c) => /aggregate:\s*\d/.test(c.text))).toBe(false)
  })

  it('a TRUNCATED scan finding nothing is never an "ok" chip -- 0 over an incomplete set is not a clean release', () => {
    const chips = releaseChipData({ manifestFindingsCount: 0, manifestCoveragePartial: true, aggregateReviewTriggered: false })
    expect(chips[0].tone).not.toBe('ok')
    expect(chips[0]).toEqual({ tone: 'warn', text: 'manifest: 0 flags (partial scan)' })
  })

  it('a truncated scan and a complete scan with the same count never render identically', () => {
    const complete = releaseChipData({ manifestFindingsCount: 0, manifestCoveragePartial: false, aggregateReviewTriggered: false })
    const partial = releaseChipData({ manifestFindingsCount: 0, manifestCoveragePartial: true, aggregateReviewTriggered: false })
    expect(partial).not.toEqual(complete)
  })

  it('a truncated scan that DID find flags stays "crit" and still says the scan was partial', () => {
    const chips = releaseChipData({ manifestFindingsCount: 2, manifestCoveragePartial: true, aggregateReviewTriggered: false })
    expect(chips[0]).toEqual({ tone: 'crit', text: 'manifest: 2 flags (partial scan)' })
  })

  it('a null manifestCoveragePartial (a server that did not report coverage) is not treated as a complete scan claim', () => {
    // null rides isRelease's own gate, so it only occurs on a non-release
    // row -- where there is no manifest chip to over-claim with at all.
    const chips = releaseChipData({ manifestFindingsCount: null, manifestCoveragePartial: null, aggregateReviewTriggered: null })
    expect(chips.some((c) => c.text.includes('manifest'))).toBe(false)
  })

  it('aggregateReviewTriggered=false renders no aggregate chip at all', () => {
    const chips = releaseChipData({ manifestFindingsCount: 0, manifestCoveragePartial: false, aggregateReviewTriggered: false })
    expect(chips.some((c) => c.text.includes('aggregate'))).toBe(false)
  })
})

describe('formatAgeSeconds', () => {
  it('renders minutes under an hour', () => {
    expect(formatAgeSeconds(60 * 20)).toBe('20 min')
  })
  it('renders hours under a day', () => {
    expect(formatAgeSeconds(60 * 60 * 2)).toBe('2 h')
  })
  it('renders days at or beyond 24h', () => {
    expect(formatAgeSeconds(60 * 60 * 24 * 3)).toBe('3 d')
  })
  it('renders "just now" for sub-minute ages', () => {
    expect(formatAgeSeconds(10)).toBe('just now')
  })
})

describe('formatDecisionLatencySeconds -- one decimal once the unit is hours/days', () => {
  it('renders whole minutes under an hour', () => {
    expect(formatDecisionLatencySeconds(60 * 45)).toBe('45 min')
  })
  it('renders one decimal of hours (mockups.html: "3.2 h")', () => {
    expect(formatDecisionLatencySeconds(60 * 60 * 3.2)).toBe('3.2 h')
  })
  it('renders one decimal of days at or beyond 24h', () => {
    expect(formatDecisionLatencySeconds(60 * 60 * 24 * 1.5)).toBe('1.5 d')
  })
})

describe('sectionTitle/sectionBlurb -- exhaustive over every real Kind value', () => {
  it('covers all four kinds without throwing', () => {
    const kinds: DecisionInboxItem['kind'][] = ['ready_to_merge', 'needs_review', 'awaiting_approval', 'needs_attention']
    for (const kind of kinds) {
      expect(typeof sectionTitle(kind)).toBe('string')
      expect(typeof sectionBlurb(kind)).toBe('string')
    }
  })
})

describe('canMergeDecisionInboxItem -- viewer role sees the queue read-only (§16.2)', () => {
  it('denies a viewer', () => {
    expect(canMergeDecisionInboxItem('viewer')).toBe(false)
  })
  it('denies an unknown/undefined role (still loading, or unrecognized)', () => {
    expect(canMergeDecisionInboxItem(undefined)).toBe(false)
  })
  it('allows member/maintainer/admin', () => {
    expect(canMergeDecisionInboxItem('member')).toBe(true)
    expect(canMergeDecisionInboxItem('maintainer')).toBe(true)
    expect(canMergeDecisionInboxItem('admin')).toBe(true)
  })
})

describe('rowKeyFor -- a stable, collision-free React key per row shape', () => {
  it('keys a PR row by repo+number', () => {
    expect(rowKeyFor(baseItem({ repoFullName: 'acme/widgets', prNumber: 42 }))).toBe('pr:acme/widgets#42')
  })
  it('keys a plan row by planId', () => {
    expect(rowKeyFor(baseItem({ planId: 'p1' }))).toBe('plan:p1')
  })
  it('keys an automation row by automationId', () => {
    expect(rowKeyFor(baseItem({ automationId: 'a1' }))).toBe('automation:a1')
  })
  it('keys an outbox row by outboxId', () => {
    expect(rowKeyFor(baseItem({ outboxId: 'o1' }))).toBe('outbox:o1')
  })
  it('keys a session row by sessionId', () => {
    expect(rowKeyFor(baseItem({ sessionId: 's1' }))).toBe('session:s1')
  })
})
