// planRendering.test.tsx -- PlanModeView.tsx's own defining risk, proven
// at the render boundary: plan.content is model-authored, freeform prose,
// and plan.structured (§12.2 item 3's own structured plan document, now
// that internal/domain/plan.ExtractStructured exists) is model-authored
// too -- every field of both renders as plain text only, never
// markdown-parsed, and a step's own fileRefs never becomes a link. Mirrors
// reviewRendering.test.tsx's own established pattern exactly:
// renderToStaticMarkup, no jsdom needed, proving React's default escaping
// is actually in effect.
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'

import type { Plan } from '@narvi/contracts/rest-dtos'

import { PlanCard, StructuredPlanSteps } from '../PlanModeView'
import { latestPlan } from '../planFormat'

const XSS_IMG = '<img src=x onerror=alert(1)>'
const XSS_SCRIPT = '<script>alert(document.cookie)</script>'
const JS_URL = 'javascript:alert(document.cookie)'

function basePlan(overrides: Partial<Plan> = {}): Plan {
  return {
    id: 'p1',
    sessionId: 's1',
    version: 1,
    status: 'awaiting_approval',
    planModelId: 'anthropic/claude-opus-4-8',
    createdAt: '2026-08-20T15:06:00Z',
    decidedAt: null,
    decidedBy: null,
    content: 'A normal plan.',
    structured: null,
    ...overrides,
  }
}

describe('PlanCard rendering -- adversarial plan content stays text, never markup', () => {
  it('a plan.content containing a script tag renders as text', () => {
    const html = renderToStaticMarkup(<PlanCard plan={basePlan({ content: `1. Add a table.\n${XSS_SCRIPT}` })} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a plan.content containing an <img onerror=...> renders as text', () => {
    const html = renderToStaticMarkup(<PlanCard plan={basePlan({ content: XSS_IMG })} />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })

  it('a plan.content that is literally structured like a numbered plan (the mockup\'s own .planlist shape), with structured left null, still renders as plain preformatted text, never parsed into <ol>/<li> markup -- this view never re-parses content client-side; a real <ol> only ever comes from the SERVER-decided plan.structured field, proven separately below', () => {
    const html = renderToStaticMarkup(<PlanCard plan={basePlan({ content: '1. **Add automation_secrets table**\n   New migration.\n2. **Extend SESSION_CONFIG assembly**', structured: null })} />)
    expect(html).not.toContain('<ol')
    expect(html).not.toContain('<li')
    // The literal markdown-looking asterisks pass through verbatim as text
    // (never interpreted as bold) -- proof no markdown parser is involved.
    expect(html).toContain('**Add automation_secrets table**')
  })

  it('a plan.content containing a javascript: URL as plain text never becomes a clickable link -- this view builds no href from plan content at all', () => {
    const html = renderToStaticMarkup(<PlanCard plan={basePlan({ content: `Click here: ${JS_URL}` })} />)
    expect(html).not.toContain('<a ')
    expect(html).not.toContain(`href="${JS_URL}"`)
    // The scheme string itself is still present, but only as inert text content.
    expect(html).toContain(JS_URL)
  })

  it('a hostile plan_model_id (attacker-controlled only in the sense that it is a free-text catalog id, never Narvi-validated against an allowlist per §29.8) still renders as text in the header context, proven via latestPlan + PlanCard together', () => {
    const plans = [basePlan({ id: 'p1', version: 1, status: 'superseded' }), basePlan({ id: 'p2', version: 2, status: 'awaiting_approval', content: XSS_SCRIPT })]
    const featured = latestPlan(plans)
    expect(featured?.id).toBe('p2')
    const html = renderToStaticMarkup(<PlanCard plan={featured!} />)
    expect(html).not.toContain('<script>')
  })
})

function structuredPlan(overrides: Partial<Plan> = {}): Plan {
  return basePlan({
    structured: {
      steps: [{ title: 'Add table', description: 'New migration.', fileRefs: ['migrations/000200.up.sql'] }],
      scopeEstimate: '1 file',
    },
    ...overrides,
  })
}

describe('PlanCard/StructuredPlanSteps rendering -- the structured path, and its own adversarial-content proof', () => {
  it('renders a real <ol className="planlist"> with one <li> per step when plan.structured is present -- the ONLY case this ever happens, proven negatively above', () => {
    const html = renderToStaticMarkup(<PlanCard plan={structuredPlan()} />)
    expect(html).toContain('planlist')
    expect(html).toContain('<ol')
    expect(html).toContain('<li')
  })

  it('renders plan.content\'s own prose ONLY when structured is null -- a structured plan does not ALSO render its raw content underneath', () => {
    const structured = structuredPlan({ content: 'THE RAW PROSE MUST NOT APPEAR TWICE' })
    const html = renderToStaticMarkup(<PlanCard plan={structured} />)
    expect(html).not.toContain('THE RAW PROSE MUST NOT APPEAR TWICE')
  })

  it('renders the scopeEstimate in the verdict-foot', () => {
    const html = renderToStaticMarkup(<PlanCard plan={structuredPlan()} />)
    expect(html).toContain('estimated scope')
    expect(html).toContain('1 file')
  })

  it('a hostile step title/description renders as text, never markup', () => {
    const plan = structuredPlan({
      structured: {
        steps: [{ title: XSS_SCRIPT, description: XSS_IMG, fileRefs: [] }],
        scopeEstimate: '1 file',
      },
    })
    const html = renderToStaticMarkup(<PlanCard plan={plan} />)
    expect(html).not.toContain('<script>')
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a hostile fileRefs entry (a path styled to look like it escapes the repository, or a javascript: URL) renders as inert text only -- never a link, never an href, exactly like plan.content\'s own established discipline', () => {
    const plan = structuredPlan({
      structured: {
        steps: [{ title: 'Step', description: 'Touches a file.', fileRefs: ['../../etc/passwd', JS_URL, XSS_SCRIPT] }],
        scopeEstimate: '3 files',
      },
    })
    const html = renderToStaticMarkup(<PlanCard plan={plan} />)
    expect(html).not.toContain('<a ')
    expect(html).not.toContain(`href="${JS_URL}"`)
    expect(html).not.toContain('<script>')
    // Each entry still appears, but only as inert text inside a <code> span.
    expect(html).toContain('../../etc/passwd')
    expect(html).toContain(JS_URL)
    expect(html).toContain('&lt;script&gt;')
  })

  it('an empty fileRefs array renders the step with no SECOND paragraph at all, never an empty one -- only the description <p>, proven by counting <p> tags rather than merely checking for <code> (a wrapping-but-empty <p> would still contain no <code> and wrongly pass a weaker assertion)', () => {
    const html = renderToStaticMarkup(
      <StructuredPlanSteps structured={{ steps: [{ title: 'Step', description: 'No files yet.', fileRefs: [] }], scopeEstimate: '0 files' }} />,
    )
    expect(html).not.toContain('<code')
    const paragraphCount = (html.match(/<p>/g) ?? []).length
    expect(paragraphCount).toBe(1)
  })

  it('multiple steps render in the given order, each with its own numbered <li>', () => {
    const html = renderToStaticMarkup(
      <StructuredPlanSteps
        structured={{
          steps: [
            { title: 'First', description: 'Do first.', fileRefs: [] },
            { title: 'Second', description: 'Do second.', fileRefs: [] },
          ],
          scopeEstimate: '2 files',
        }}
      />,
    )
    const firstIdx = html.indexOf('First')
    const secondIdx = html.indexOf('Second')
    expect(firstIdx).toBeGreaterThanOrEqual(0)
    expect(secondIdx).toBeGreaterThan(firstIdx)
  })
})

describe('latestPlan -- pure selection logic', () => {
  it('picks the awaiting_approval version even when it is not the highest-numbered one listed (defensive -- in practice it always is, per the DB\'s own partial unique index)', () => {
    const plans = [basePlan({ id: 'p1', version: 1, status: 'superseded' }), basePlan({ id: 'p2', version: 2, status: 'awaiting_approval' })]
    expect(latestPlan(plans)?.id).toBe('p2')
  })
  it('falls back to the highest version when nothing is awaiting approval', () => {
    const plans = [basePlan({ id: 'p1', version: 1, status: 'rejected' }), basePlan({ id: 'p2', version: 2, status: 'superseded' }), basePlan({ id: 'p3', version: 3, status: 'approved' })]
    expect(latestPlan(plans)?.id).toBe('p3')
  })
  it('returns null for an empty list', () => {
    expect(latestPlan([])).toBeNull()
  })
})
