// slots.test.tsx -- ext/slots.tsx's own three named behaviors (docs/
// design/boundaries-design.md, section 4.4): unregistered renders
// fallback; registered renders component; registering twice is an error.
//
// registry (slots.tsx) is deliberately module-level, private state -- so
// each `it` below resets the module graph first (vi.resetModules) and
// re-imports fresh, giving every test its own pristine, empty registry
// rather than depending on write order across tests sharing one instance
// (SlotId has exactly one member today, so a real second id to isolate
// against does not exist yet either).
import { describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'

describe('ext/slots', () => {
  it('an unregistered slot renders the fallback', async () => {
    vi.resetModules()
    const { SlotOutlet } = await import('../slots')

    const html = renderToStaticMarkup(<SlotOutlet id="repo-settings.governance" fallback={<span>fallback text</span>} />)

    expect(html).toContain('fallback text')
  })

  it('a registered slot renders the registered component, never the fallback', async () => {
    vi.resetModules()
    const { SlotOutlet, registerSlot } = await import('../slots')

    function Fake() {
      return <span>registered text</span>
    }
    registerSlot('repo-settings.governance', Fake)

    const html = renderToStaticMarkup(<SlotOutlet id="repo-settings.governance" fallback={<span>fallback text</span>} />)

    expect(html).toContain('registered text')
    expect(html).not.toContain('fallback text')
  })

  it('registering the same slot id twice throws, rather than silently overriding', async () => {
    vi.resetModules()
    const { registerSlot } = await import('../slots')

    function First() {
      return <span>first</span>
    }
    function Second() {
      return <span>second</span>
    }
    registerSlot('repo-settings.governance', First)

    expect(() => registerSlot('repo-settings.governance', Second)).toThrow(/already registered/)
  })
})
