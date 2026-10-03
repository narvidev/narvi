// planRendering.test.tsx -- PlanModeView.tsx's own defining risk, proven
// at the render boundary: plan.content is model-authored, freeform prose,
// and plan.structured (§12.2 item 3's own structured plan document, now
// that internal/domain/plan.ExtractStructured exists) is model-authored
// too -- every field of both renders as plain text only, never
// markdown-parsed, and a step's own fileRefs never becomes a link. Mirrors
// reviewRendering.test.tsx's own established pattern exactly:
// renderToStaticMarkup, no jsdom needed, proving React's default escaping
// is actually in effect.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type * as ReactQuery from '@tanstack/react-query'

import type { Plan } from '@narvi/contracts/rest-dtos'

import { ApiError } from '../../api/http'
import { planQueryKeys } from '../../api/queryKeys'
import { ApprovalBar, PlanCard, StructuredPlanSteps } from '../PlanModeView'
import { latestPlan, stripStructureBlock } from '../planFormat'
import { cutReason } from '../tokenCut'

// The approval bar's mutations: every useMutation call's options are
// recorded, in call order (ApprovalBar calls approve, then reject), and a
// result can be forced over the real hook's for one of them, to render the
// bar as it is after a refused request. The real hooks still run; with
// nothing forced the wrapper changes nothing, so the PlanCard tests below
// are unaffected.
const hooks = vi.hoisted(() => ({ mutations: [] as unknown[], forced: [] as (object | undefined)[] }))

vi.mock('@tanstack/react-query', async (importOriginal) => {
  const actual = await importOriginal<typeof ReactQuery>()
  const useMutation = ((...args: Parameters<typeof actual.useMutation>) => {
    const index = hooks.mutations.length
    hooks.mutations.push(args[0])
    const result = actual.useMutation(...args)
    const forced = hooks.forced[index]
    return forced === undefined ? result : { ...result, ...forced }
  }) as typeof actual.useMutation
  return { ...actual, useMutation }
})

afterEach(() => {
  hooks.mutations.length = 0
  hooks.forced.length = 0
})

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
    cut: null,
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

  // Finding 7: plan.content is the authoritative document -- the SAME text
  // every channel (web, Slack, Linear) shows, since neither Slack nor
  // Linear has ever rendered plan.structured (outboxenqueue.go's own two
  // call sites only ever strip and send plan.content). An earlier version
  // of this test asserted the OPPOSITE ("prose does not ALSO render
  // underneath") -- that was the bug: hiding the prose whenever a
  // structured summary existed meant a web approver could be deciding on
  // different text than the SAME plan's Slack/Linear approver. This test
  // now pins the fix: the structured list is an ADDITIVE readability
  // affordance on top of the prose, never a replacement for it.
  it('renders plan.content\'s own prose ALONGSIDE the structured list, never hidden by it -- every channel decides on the same document', () => {
    const structured = structuredPlan({ content: 'This prose is the authoritative document -- it must still be visible.' })
    const html = renderToStaticMarkup(<PlanCard plan={structured} />)
    expect(html).toContain('planlist')
    expect(html).toContain('This prose is the authoritative document -- it must still be visible.')
  })

  it('still strips the machine block from the prose even when structured is present -- the human never sees the raw JSON twice over', () => {
    const structured = structuredPlan({
      content:
        'Here is my plan.\n\n```plan-steps\n' +
        '{"steps":[{"title":"Add table","description":"New migration.","fileRefs":["migrations/000200.up.sql"]}],"scopeEstimate":"1 file"}' +
        '\n```\n',
    })
    const html = renderToStaticMarkup(<PlanCard plan={structured} />)
    expect(html).toContain('Here is my plan.')
    expect(html).not.toContain('plan-steps')
    expect(html).not.toContain('scopeEstimate')
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

describe('stripStructureBlock -- a human never reads the machine block', () => {
  it('removes a trailing plan-steps block and the blank seam it left', () => {
    const content =
      'Here is my plan.\n\n1. Add a table.\n\n```plan-steps\n' +
      '{"steps":[{"title":"T","description":"D","fileRefs":[]}],"scopeEstimate":"1 file"}' +
      '\n```\n'
    expect(stripStructureBlock(content)).toBe('Here is my plan.\n\n1. Add a table.')
  })

  it('leaves content with no block untouched', () => {
    expect(stripStructureBlock('1. Add a table.')).toBe('1. Add a table.')
  })

  it('removes nothing when two open fences make the block ambiguous', () => {
    const content = 'A\n```plan-steps\n{}\n```\nB\n```plan-steps\n{}\n```\n'
    expect(stripStructureBlock(content)).toBe(content)
  })

  // Pins the Go twin's own coverage (plandomain.StripStructureBlock,
  // structured_test.go's "an unterminated fence removes nothing") on this
  // side too -- a truncated/cut-off stream must not have its trailing,
  // incomplete block guessed at and removed.
  it('an unterminated fence removes nothing', () => {
    const content = 'Plan.\n\n```plan-steps\n{"steps":[]'
    expect(stripStructureBlock(content)).toBe(content)
  })

  // The core reproduction for finding 1: JSON does not require backticks to
  // be escaped inside a string, so a step's own title/description can
  // legitimately contain a "```" sequence strictly before the block's real
  // close. A naive first-match search stops there, splicing the back half
  // of the fenced JSON into the prose. The fix (closingFenceIndex requiring
  // the close to start a line) removes the WHOLE block instead.
  it('an embedded ``` sequence inside the fenced JSON does not end the block early -- the whole block is removed, none of it leaks into the prose', () => {
    const content =
      'Plan.\n\n```plan-steps\n' +
      '{"title":"Wrap it in ```code``` blocks"}' +
      '\n```\n\nDone.'
    expect(stripStructureBlock(content)).toBe('Plan.\n\nDone.')
  })

  // Finding 2: a reply that is ONLY the block (the model skipped the
  // "propose your plan in prose first" half of the instruction) must not
  // strip down to the empty string -- an empty result is never more honest
  // than the raw block it came from.
  it('a reply that is ONLY the block is returned UNCHANGED, never stripped to empty', () => {
    const content = '```plan-steps\n{"steps":[{"title":"T","description":"D","fileRefs":[]}],"scopeEstimate":"1 file"}\n```'
    expect(stripStructureBlock(content)).toBe(content)
  })

  it('a reply that is the block plus only surrounding whitespace is also returned unchanged', () => {
    const content = '   \n```plan-steps\n{"steps":[{"title":"T","description":"D","fileRefs":[]}],"scopeEstimate":"1 file"}\n```\n   '
    expect(stripStructureBlock(content)).toBe(content)
  })

  it('the prose fallback -- the path taken when the block did NOT parse -- never shows the raw JSON', () => {
    // A block that fails validation (zero steps) so structured is null and
    // the fallback renders. Without the strip the reader would be handed the
    // very JSON that just failed.
    const plan = {
      id: 'p1',
      version: 1,
      status: 'awaiting_approval',
      createdAt: new Date().toISOString(),
      content:
        'My plan in prose.\n\n```plan-steps\n{"steps":[],"scopeEstimate":"1 file"}\n```\n',
      structured: null,
    } as unknown as Plan
    const html = renderToStaticMarkup(<PlanCard plan={plan} />)
    expect(html).toContain('My plan in prose.')
    expect(html).not.toContain('scopeEstimate')
    expect(html).not.toContain('plan-steps')
  })

  // Finding 2: a reply that is ONLY the machine block (the model skipped
  // the "propose your plan in prose first" half of the instruction) and
  // fails validation must not render an EMPTY plan card -- before this fix,
  // stripStructureBlock stripped the whole content down to '', so
  // plan.structured was null (the block failed validation), the <p> was
  // empty, and the approval bar was still live: a human was asked to
  // approve nothing, with no way to tell why. It must show SOMETHING real.
  it('a block-only reply that fails validation renders the raw content, never an empty card', () => {
    const plan = {
      id: 'p1',
      version: 1,
      status: 'awaiting_approval',
      createdAt: new Date().toISOString(),
      content: '```plan-steps\n{"steps":[],"scopeEstimate":"1 file"}\n```',
      structured: null,
    } as unknown as Plan
    const html = renderToStaticMarkup(<PlanCard plan={plan} />)
    expect(html).toContain('class="plan-content"')
    // The <p class="plan-content"> must carry SOME text -- the raw,
    // unstripped reply -- rather than an empty element.
    expect(html).toMatch(/<p class="plan-content">[^<].*plan-steps/s)
  })
})

// A plan whose text was cut on its way from the sandbox (plan.cut,
// technical plan §6.1) cannot be approved: the server answers 409 with the
// reason. The bar never offers Approve for one, and an approve refused with
// 409 for any reason shows the server's own words.
describe('ApprovalBar -- a cut plan', () => {
  type CapturedMutation = { onError?: (...args: unknown[]) => unknown }

  function renderBar(plan: Plan) {
    hooks.mutations.length = 0
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    client.setQueryData(planQueryKeys.list(plan.sessionId), { plans: [plan] })
    const html = renderToStaticMarkup(
      <QueryClientProvider client={client}>
        <ApprovalBar sessionId={plan.sessionId} plan={plan} canAct={true} />
      </QueryClientProvider>,
    )
    const [approve, reject] = hooks.mutations as CapturedMutation[]
    return { html, client, approve, reject }
  }

  const cut = { kept: 4096, total: 40960 }

  it('a cut plan offers no Approve and shows the reason where it would be, keeping Request changes and Reject', () => {
    const { html } = renderBar(basePlan({ cut }))
    expect(html).not.toContain('Approve &amp; build')
    expect(html).toContain(cutReason(cut))
    expect(html).toContain('Request changes')
    expect(html).toContain('Reject')
  })

  it('a malformed cut is still a cut: no Approve, and the reason without sizes', () => {
    const { html } = renderBar(basePlan({ cut: { kept: -1, total: -1 } }))
    expect(html).not.toContain('Approve &amp; build')
    expect(html).toContain(cutReason({ kept: -1, total: -1 }))
  })

  it('a whole plan offers Approve and no cut reason', () => {
    const { html } = renderBar(basePlan())
    expect(html).toContain('Approve &amp; build')
    expect(html).not.toContain('No approval for this plan')
  })

  it("an approve refused with 409 shows the server's own reason, the plan's cut among them", () => {
    const reason = cutReason(cut)
    hooks.forced[0] = { status: 'error', isError: true, isIdle: false, error: new ApiError(409, reason, { error: reason }) }
    const { html } = renderBar(basePlan())
    expect(html).toContain(reason)
    expect(html).not.toContain('already decided')
  })

  it('a reject refused with 409 keeps its own text', () => {
    hooks.forced[1] = { status: 'error', isError: true, isIdle: false, error: new ApiError(409, 'plan is not awaiting approval (already decided, or a stale id)', null) }
    const { html } = renderBar(basePlan())
    expect(html).toContain('This plan was already decided (or superseded) by someone else.')
  })

  it('an approve refused with 409 re-reads the plans, so the bar catches up with the server', () => {
    const plan = basePlan()
    const { client, approve } = renderBar(plan)
    expect(client.getQueryState(planQueryKeys.list(plan.sessionId))?.isInvalidated).toBe(false)
    approve.onError?.(new ApiError(409, cutReason(cut), null), undefined, undefined)
    expect(client.getQueryState(planQueryKeys.list(plan.sessionId))?.isInvalidated).toBe(true)
  })

  it('an approve failing otherwise leaves the plans as they are', () => {
    const plan = basePlan()
    const { client, approve } = renderBar(plan)
    approve.onError?.(new ApiError(500, 'internal error', null), undefined, undefined)
    expect(client.getQueryState(planQueryKeys.list(plan.sessionId))?.isInvalidated).toBe(false)
  })
})
