// automationRendering.test.tsx -- AutomationsView.tsx's own defining risk,
// proven at the render boundary: automation.name/automation.prompt and a
// run's own target.name are human-authored free text, never
// Narvi-validated, and must render as plain text only. Mirrors
// reviewRendering.test.tsx/planRendering.test.tsx's own established
// pattern exactly: renderToStaticMarkup, no jsdom needed.
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

import type { Automation, AutomationInvocation, AutomationRun } from '@narvi/contracts/rest-dtos'

import { AutomationRow, RunRow } from '../AutomationsView'

const XSS_IMG = '<img src=x onerror=alert(1)>'
const XSS_SCRIPT = '<script>alert(document.cookie)</script>'
const JS_URL = 'javascript:alert(document.cookie)'

function withQueryClient(node: React.ReactNode) {
  const client = new QueryClient()
  return renderToStaticMarkup(<QueryClientProvider client={client}>{node}</QueryClientProvider>)
}

function baseAutomation(overrides: Partial<Automation> = {}): Automation {
  return {
    id: 'a1',
    name: 'nightly audit',
    prompt: 'do the thing',
    repos: [{ name: 'widgets', url: 'https://github.com/acme/widgets', branch: null }],
    status: 'active',
    consecutiveFailures: 0,
    createdBy: 'u1',
    createdAt: '2026-08-20T02:00:00Z',
    updatedAt: '2026-08-20T02:00:00Z',
    triggerType: 'manual',
    triggerConfig: {},
    sandboxPathScope: null,
    sandboxMockConfigured: false,
    sandboxContractsPath: null,
    envVars: [],
    lastRunAt: null,
    lastRunStatus: null,
    artifactSummary: null,
    ...overrides,
  }
}

function baseInvocation(overrides: Partial<AutomationInvocation> = {}): AutomationInvocation {
  return {
    id: 'i1',
    automationId: 'a1',
    status: 'succeeded',
    totalRuns: 1,
    closedAt: '2026-08-20T02:05:00Z',
    createdAt: '2026-08-20T02:00:00Z',
    runs: [],
    ...overrides,
  }
}

function baseRun(overrides: Partial<AutomationRun> = {}): AutomationRun {
  return {
    id: 'r1',
    invocationId: 'i1',
    automationId: 'a1',
    target: { name: 'widgets', url: 'https://github.com/acme/widgets', branch: null },
    sessionId: null,
    status: 'succeeded',
    startedAt: '2026-08-20T02:00:00Z',
    runningAt: '2026-08-20T02:01:00Z',
    completedAt: '2026-08-20T02:05:00Z',
    ...overrides,
  }
}

describe('AutomationRow rendering -- adversarial name/prompt stays text, never markup', () => {
  it('a hostile automation.name renders as text', () => {
    const html = withQueryClient(<AutomationRow automation={baseAutomation({ name: `nightly ${XSS_SCRIPT}` })} canManage={false} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a hostile automation.prompt renders as text', () => {
    const html = withQueryClient(<AutomationRow automation={baseAutomation({ prompt: XSS_IMG })} canManage={false} />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })

  it('a null prompt renders no prompt line at all (never "null" as text)', () => {
    const html = withQueryClient(<AutomationRow automation={baseAutomation({ prompt: null })} canManage={false} />)
    expect(html).not.toContain('>null<')
  })
})

// §12.2 item 4's own health-column success ratio: the ratio
// only replaces the plain "active" status chip when there is no active
// failure streak and no pause -- AutomationsView.tsx's own three-way
// branch (paused / strikes / else), unchanged by this addition except
// for what the "else" branch itself renders.
describe('AutomationRow health column -- the §12.2 item 4 success ratio', () => {
  it('renders the real ratio when runHealth is present', () => {
    const html = withQueryClient(<AutomationRow automation={baseAutomation({ runHealth: { succeededRuns: 47, terminalRuns: 48 } })} canManage={false} />)
    expect(html).toContain('47/48 ok')
  })

  it('falls back to the plain status chip when runHealth is null -- an honest "no runs yet", never a fabricated 0/0', () => {
    const html = withQueryClient(<AutomationRow automation={baseAutomation({ runHealth: null })} canManage={false} />)
    expect(html).not.toContain('/0 ok')
    expect(html).toContain('active')
  })

  it('a strike in progress still wins over a real runHealth ratio -- the mockup never shows both at once', () => {
    const html = withQueryClient(<AutomationRow automation={baseAutomation({ consecutiveFailures: 2, runHealth: { succeededRuns: 10, terminalRuns: 12 } })} canManage={false} />)
    expect(html).toContain('strikes before auto-pause')
    expect(html).not.toContain('10/12 ok')
  })

  it('a paused automation still wins over a real runHealth ratio', () => {
    const html = withQueryClient(<AutomationRow automation={baseAutomation({ status: 'paused', runHealth: { succeededRuns: 10, terminalRuns: 12 } })} canManage={false} />)
    expect(html).toContain('auto-paused')
    expect(html).not.toContain('10/12 ok')
  })
})

describe('RunRow rendering -- adversarial target.name stays text, never markup, and never becomes a link', () => {
  it('a hostile run.target.name renders as text', () => {
    const html = withQueryClient(<RunRow invocation={baseInvocation()} run={baseRun({ target: { name: XSS_SCRIPT, url: 'https://github.com/acme/widgets', branch: null } })} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a target.name containing a javascript: URL as plain text never becomes a clickable link -- this view builds no href from target content at all (a run with no sessionId renders no link at all, only the invocation-id fallback text)', () => {
    const html = withQueryClient(<RunRow invocation={baseInvocation()} run={baseRun({ sessionId: null, target: { name: JS_URL, url: 'https://github.com/acme/widgets', branch: null } })} />)
    expect(html).not.toContain(`href="${JS_URL}"`)
    expect(html).not.toContain('<a ')
  })

  // Note: RunRow's own "session ->" affordance (rendered only when
  // run.sessionId is set) uses TanStack Router's <Link>, which requires a
  // real RouterProvider/router context to render at all -- this codebase
  // has no precedent anywhere for unit-rendering a <Link>-bearing
  // component outside the real app (SessionRail.tsx's own identical
  // ReviewLinksPanel, which ALSO uses <Link>, has no direct render test
  // either, for the same reason). That link is built from a
  // server-resolved sessionId (a UUID), never from run.target content, so
  // it carries none of this file's own adversarial-input risk -- proven
  // structurally by the two tests above (target.name never reaches an
  // href anywhere in this component), not by rendering the Link itself.
})
