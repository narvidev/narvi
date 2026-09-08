// analyticsRendering.test.tsx -- proves the defect this fix exists for is
// gone, at the SCREEN, not just the wire: sessionsTotal/falseFailureCount
// used to carry no computed sentinel at all, so a query failure left them
// at their Go zero value and the handler still returned 200 -- the
// Analytics view then rendered "Sessions: 0" and "False failures: 0,
// target 0" as settled fact on a query that never ran. Fixed by giving
// both fields their own sessionsTotalComputed/falseFailureCountComputed
// sentinel (contracts/rest/v1/dtos.schema.json), identical in kind to
// every other rollup on this DTO.
//
// This suite drives PlatformAnalyticsBody -- AnalyticsView.tsx's own
// pure, presentational half -- directly with hand-built
// PlatformAnalytics fixtures shaped exactly like a real degraded
// response (the *Computed field false, the value at its Go zero/null),
// mirroring workflowRunRendering.test.tsx's own "renderToStaticMarkup,
// no jsdom" precedent. Each case forces exactly ONE rollup's own fetch to
// have failed and asserts BOTH halves of the property: that rollup's own
// tile/chart renders "not available" (never a fabricated value), AND
// every other tile still renders its real, computed value (proving the
// partial-render/resilience posture this handler deliberately keeps,
// rather than failing the whole request).
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'

import type { PlatformAnalytics } from '@narvi/contracts/rest-dtos'

import { PlatformAnalyticsBody } from '../AnalyticsView'

// healthyAnalytics is a fully-computed baseline -- every rollup succeeded,
// every field carries a real, non-sentinel value. Each test below starts
// from this and degrades exactly ONE rollup's own field(s), the same
// shape internal/adapters/inbound/httpapi.GetPlatformAnalytics actually
// produces when exactly one of its Postgres reads returns an error.
function healthyAnalytics(overrides: Partial<PlatformAnalytics> = {}): PlatformAnalytics {
  return {
    windowDays: 30,
    sessionsTotalComputed: true,
    sessionsTotal: 42,
    sessionsPerDayComputed: true,
    sessionsPerDay: [{ day: '2026-09-07T00:00:00Z', createdCount: 1, activeCount: 1, completedCount: 3, failedCount: 1, cancelledCount: 0 }],
    successRateComputed: true,
    successRatePercent: 75,
    successRateSampleSize: 4,
    falseFailureCountComputed: true,
    falseFailureCount: 0,
    costComputed: true,
    costTotalUsd: 16,
    costMedianPerSessionUsd: 4,
    costSampleSize: 3,
    costByModelComputed: true,
    costByModel: [{ modelId: 'sonnet-5', totalUsd: 16 }],
    bootP95Computed: true,
    bootP95Seconds: 19.05,
    bootP95SampleSize: 20,
    topFailureReasonsComputed: true,
    topFailureReasons: [{ reason: 'timeout', count: 1 }],
    ...overrides,
  }
}

function render(data: PlatformAnalytics): string {
  return renderToStaticMarkup(<PlatformAnalyticsBody data={data} />)
}

describe('PlatformAnalyticsBody -- the healthy baseline renders every real value, no sentinel text anywhere', () => {
  it('renders sessions, success rate, false failures, cost, and boot p95 as real numbers', () => {
    const html = render(healthyAnalytics())
    expect(html).toContain('<span class="big">42</span>')
    expect(html).toContain('75.0%')
    expect(html).toContain('<span class="big">0</span>')
    expect(html).toContain('$16.00')
    expect(html).toContain('19 s')
    expect(html).not.toContain('not available')
  })
})

describe('PlatformAnalyticsBody -- the session-outcome-counts query fails: BEFORE this fix this rendered "Sessions: 0" as fact; now it renders "not available"', () => {
  // internal/app/platformanalytics.SessionOutcomeCountsInWindow feeds FOUR
  // wire fields at once (sessionsTotal, sessionsPerDay, successRate,
  // topFailureReasons) -- a fetch failure degrades all four together,
  // exactly as httpapi.GetPlatformAnalytics' own doc comment states.
  const degraded = healthyAnalytics({
    sessionsTotalComputed: false,
    sessionsTotal: 0,
    sessionsPerDayComputed: false,
    sessionsPerDay: null,
    successRateComputed: false,
    successRatePercent: null,
    successRateSampleSize: 0,
    topFailureReasonsComputed: false,
    topFailureReasons: null,
  })
  const html = render(degraded)

  it('the Sessions tile renders "not available", never a fabricated 0', () => {
    expect(html).toContain('Sessions')
    expect(html).toContain('couldn&#x27;t load this count')
    // The specific defect: this string must NEVER appear paired with the
    // Sessions tile's own zero value once sessionsTotalComputed is false.
    expect(html).not.toMatch(/Sessions<\/span>\s*<span class="big">0<\/span>/)
  })

  it('the "Sessions per day" chart renders "not available", not an empty/silent chart', () => {
    expect(html).toContain('Not available yet -- no sessions created in this window.')
  })

  it('the "Top failure reasons" chart renders "not available"', () => {
    expect(html).toContain('Not available yet -- no sessions in this window.')
  })

  it('the Success rate tile still renders its own honest "not available" (unaffected in KIND, since it always had a sentinel)', () => {
    expect(html).toContain('Success rate')
    expect(html).toContain('no resolved sessions yet')
  })

  it('False failures, Cost, and Boot p95 -- rollups that did NOT fail -- still render their real computed values', () => {
    expect(html).toContain('False failures')
    expect(html).toContain('target 0 · watchdog kills later proven alive')
    expect(html).toContain('$16.00')
    expect(html).toContain('19 s')
  })
})

describe('PlatformAnalyticsBody -- the false-failures COUNT query fails: BEFORE this fix this rendered "target 0, met" as fact; now it renders "not available"', () => {
  const degraded = healthyAnalytics({ falseFailureCountComputed: false, falseFailureCount: 0 })
  const html = render(degraded)

  it('the False failures tile renders "not available", never a fabricated 0/"target met"', () => {
    expect(html).toContain('False failures')
    expect(html).toContain('couldn&#x27;t load this count')
    expect(html).not.toContain('target 0 · watchdog kills later proven alive')
    expect(html).not.toMatch(/False failures<\/span>\s*<span class="big">0<\/span>/)
  })

  it('every other tile -- Sessions, success rate, cost, boot p95 -- still renders its real computed value', () => {
    expect(html).toContain('<span class="big">42</span>')
    expect(html).toContain('75.0%')
    expect(html).toContain('$16.00')
    expect(html).toContain('19 s')
  })
})

describe('PlatformAnalyticsBody -- the cost-summary query fails', () => {
  // CostByModelInWindow's own doc comment: costByModelComputed mirrors
  // costComputed, "the same underlying 'has any cost data arrived' fact"
  // -- so both degrade together on this one query's failure.
  const degraded = healthyAnalytics({
    costComputed: false,
    costTotalUsd: null,
    costMedianPerSessionUsd: null,
    costSampleSize: 0,
    costByModelComputed: false,
    costByModel: null,
  })
  const html = render(degraded)

  it('the Cost tile and "Cost by model" chart render "not available", never a fabricated $0.00', () => {
    expect(html).toContain('Cost')
    expect(html).toContain('no costed turns yet')
    expect(html).toContain('Not available yet -- no costed turns in this window.')
    expect(html).not.toContain('$0.00')
  })

  it('Sessions and False failures -- unaffected rollups -- still render their real values', () => {
    expect(html).toContain('<span class="big">42</span>')
    expect(html).toContain('<span class="big">0</span>')
  })
})

describe('PlatformAnalyticsBody -- the boot-p95 query fails', () => {
  const degraded = healthyAnalytics({ bootP95Computed: false, bootP95Seconds: null, bootP95SampleSize: 0 })
  const html = render(degraded)

  it('the Boot p95 tile renders "not available", never a fabricated 0 s', () => {
    expect(html).toContain('Boot p95')
    expect(html).toContain('not available yet')
    expect(html).not.toContain('0 s')
  })

  it('Sessions and Cost -- unaffected rollups -- still render their real values', () => {
    expect(html).toContain('<span class="big">42</span>')
    expect(html).toContain('$16.00')
  })
})
