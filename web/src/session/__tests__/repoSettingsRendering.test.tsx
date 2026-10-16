// repoSettingsRendering.test.tsx -- RepoSettingsView.tsx's own defining
// risk, proven at the RENDER boundary: repoFullName (echoes what the
// caller typed, but is also GitHub's own repo naming) and
// reviewDepthDeepPaths (operator-entered glob patterns) are free text, and
// sensitiveBlastRadiusTags is a closed wire enum rendered defensively
// through the same plain-text path anyway. Mirrors decisionInboxRendering.
// test.tsx/membersRendering.test.tsx's own established pattern exactly:
// renderToStaticMarkup, no jsdom needed, proving React's default escaping
// is actually in effect on RepoSettingsSummary -- the one component this
// file exports specifically for this test (this file's own top doc
// comment).
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'

import type { RepoSettings } from '@narvi/contracts/rest-dtos'

import { RepoSettingsSummary } from '../RepoSettingsView'

const XSS_IMG = '<img src=x onerror=alert(1)>'
const XSS_SCRIPT = '<script>alert(document.cookie)</script>'

function baseSettings(overrides: Partial<RepoSettings> = {}): RepoSettings {
  return {
    repoFullName: 'acme/widgets',
    blockOnHighRisk: false,
    sentinelAutofixEnabled: false,
    autoMergeEnabled: false,
    maxAutoApproveFilesChanged: null,
    sensitiveBlastRadiusTags: null,
    contradictionRateComputed: false,
    contradictionRatePercent: null,
    contradictionSampleSize: 0,
    autoRetriggerReviewEnabled: false,
    descriptionAutofixEnabled: false,
    reviewDepthMode: null,
    reviewDepthDeepPaths: null,
    reviewCostBudgetLightUsd: null,
    reviewCostBudgetDeepUsd: null,
    sessionSpendCapUsd: null,
    ...overrides,
  }
}

describe('RepoSettingsSummary -- adversarial repoFullName stays text, never markup', () => {
  it('a hostile repoFullName renders as text', () => {
    const html = renderToStaticMarkup(<RepoSettingsSummary settings={baseSettings({ repoFullName: `acme/${XSS_IMG}` })} />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })
})

describe('RepoSettingsSummary -- adversarial reviewDepthDeepPaths stays text', () => {
  it('a hostile deep-path glob renders as text', () => {
    const html = renderToStaticMarkup(<RepoSettingsSummary settings={baseSettings({ reviewDepthDeepPaths: [`internal/**`, `payments/${XSS_SCRIPT}`] })} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
    // The well-formed sibling entry still renders, unaffected by its neighbor.
    expect(html).toContain('internal/**')
  })
})

describe('RepoSettingsSummary -- an unrecognized sensitiveBlastRadiusTags value stays text', () => {
  it('a future/unknown tag value (bypassing the closed wire enum) renders as text, not thrown', () => {
    // sensitiveBlastRadiusTags is a closed enum on the wire today, but this
    // proves the defensive T path still holds if a future server version
    // ever sends a value this client's own type does not yet know about --
    // cast past the literal union deliberately, mirroring how a real,
    // version-skewed server response would arrive untyped over the wire.
    const settings = baseSettings({ sensitiveBlastRadiusTags: [`weird-tag-${XSS_IMG}`] as unknown as RepoSettings['sensitiveBlastRadiusTags'] })
    const html = renderToStaticMarkup(<RepoSettingsSummary settings={settings} />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })
})

describe('RepoSettingsSummary -- an unrecognized reviewDepthMode value stays text', () => {
  it('an unrecognized mode string renders as text via its own fallback branch', () => {
    const html = renderToStaticMarkup(<RepoSettingsSummary settings={baseSettings({ reviewDepthMode: `custom-${XSS_SCRIPT}` })} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })
})

describe('RepoSettingsSummary -- does not hang or break layout on a pathologically long value', () => {
  it('caps a 200KB deep-path entry rather than rendering it in full', () => {
    const settings = baseSettings({ reviewDepthDeepPaths: ['x'.repeat(200_000)] })
    const start = Date.now()
    let html = ''
    expect(() => {
      html = renderToStaticMarkup(<RepoSettingsSummary settings={settings} />)
    }).not.toThrow()
    expect(Date.now() - start).toBeLessThan(2000)
    expect(html.length).toBeLessThan(200_000)
    expect(html).toContain('more characters truncated')
  })
})

describe('RepoSettingsSummary -- null/absent config renders an honest default, never a fabricated value', () => {
  // The point of these is that a null is never rendered as a blank, a zero, or
  // the literal "null" -- an operator must be able to tell "the engine decides
  // this" apart from "this is set to nothing". The earlier single case
  // asserted only that the string "engine default" appeared SOMEWHERE in the
  // markup, so four of the five fields could have regressed with it still
  // green; and its name claimed all five render "engine default" when the
  // deep-paths row deliberately renders "none configured" instead (a list with
  // no entries is not the same statement as a value the engine supplies).
  // Each field is now asserted on its own rendered row.
  const summaryRow = (html: string, label: string): string => {
    const at = html.indexOf(label)
    expect(at, `row ${label} is missing from the summary`).toBeGreaterThan(-1)
    return html.slice(at, at + 220)
  }

  it.each([
    ['Max auto-approve files changed', 'engine default'],
    ['Sensitive blast-radius tags', 'engine default'],
    ['Review depth mode', 'engine default'],
    ['Review cost budget · light', 'engine default'],
    ['Review cost budget · deep', 'engine default'],
    ['Review depth · additional deep paths', 'none configured'],
  ])('a null %s renders as %p on its own row', (label, expected) => {
    const html = renderToStaticMarkup(<RepoSettingsSummary settings={baseSettings()} />)
    expect(summaryRow(html, label)).toContain(expected)
  })

  it('never renders a null as a literal, a blank or a zero', () => {
    const html = renderToStaticMarkup(<RepoSettingsSummary settings={baseSettings()} />)
    expect(html).not.toContain('null')
    expect(html).not.toContain('undefined')
    expect(summaryRow(html, 'Max auto-approve files changed')).not.toMatch(/>\s*0\s*</)
  })
})
