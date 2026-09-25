// connectedAppsRendering.test.tsx -- ConnectedAppsSection.tsx's own
// defining risks: clientName and redirect URIs are admin-supplied strings
// (text, never markup, never an href), a scope-less authorization must say
// plainly that it sees nothing, and both destructive actions (revoke,
// delete) must never be a single bare click. Mirrors
// integrationsRendering.test.tsx's own pattern: assert on specific visible
// text, never the whole rendered HTML.
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'

import type { MCPAuthorization, MCPClient } from '@narvi/contracts/rest-dtos'

import { ConnectedAppRow, MCPClientRow } from '../ConnectedAppsSection'

const XSS = '<script>alert(document.cookie)</script>'
const noop = () => {}

function baseAuthorization(overrides: Partial<MCPAuthorization> = {}): MCPAuthorization {
  return {
    id: '3f2504e0-4f89-11d3-9a0c-0305e82c3301',
    clientId: 'narvi_mcp_c_abc',
    clientName: 'Editor Plugin',
    clientKind: 'preregistered',
    scopes: ['mcp:read'],
    createdAt: '2026-09-20T10:00:00Z',
    expiresAt: '2026-12-19T10:00:00Z',
    lastUsedAt: null,
    ...overrides,
  }
}

function baseClient(overrides: Partial<MCPClient> = {}): MCPClient {
  return {
    id: '3f2504e0-4f89-11d3-9a0c-0305e82c3302',
    clientId: 'narvi_mcp_c_abc',
    clientName: 'Editor Plugin',
    kind: 'preregistered',
    redirectUris: ['http://127.0.0.1/callback'],
    clientUri: null,
    createdAt: '2026-09-20T10:00:00Z',
    disabledAt: null,
    ...overrides,
  }
}

function renderAuthorization(a: MCPAuthorization): string {
  return renderToStaticMarkup(
    <table>
      <tbody>
        <ConnectedAppRow authorization={a} onRevoke={noop} revoking={false} />
      </tbody>
    </table>,
  )
}

function renderClient(c: MCPClient): string {
  return renderToStaticMarkup(
    <table>
      <tbody>
        <MCPClientRow client={c} onDelete={noop} deleting={false} />
      </tbody>
    </table>,
  )
}

describe('ConnectedAppRow', () => {
  it('renders the app, who vouched for it, its access and its dates', () => {
    const html = renderAuthorization(baseAuthorization({ lastUsedAt: '2026-09-21T08:30:00Z' }))
    expect(html).toContain('Editor Plugin')
    expect(html).toContain('Registered by an administrator of this deployment')
    expect(html).toContain('Read models and sessions')
    expect(html).toContain('2026-09-21 08:30:00Z')
    expect(html).toContain('2026-12-19 10:00:00Z')
  })

  it('a never-used authorization says never, not a fabricated date', () => {
    expect(renderAuthorization(baseAuthorization())).toContain('>never<')
  })

  it('a scope-less authorization says it sees no tools', () => {
    expect(renderAuthorization(baseAuthorization({ scopes: [] }))).toContain('No access (sees no tools)')
  })

  it('a hostile client name renders as text', () => {
    const html = renderAuthorization(baseAuthorization({ clientName: XSS }))
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('revoking starts behind a confirmation, never a single click', () => {
    const html = renderAuthorization(baseAuthorization())
    expect(html).toContain('>Revoke<')
    expect(html).not.toContain('Confirm revoke')
  })
})

describe('MCPClientRow', () => {
  it('renders the public client ID and every redirect URI as text, never as a link', () => {
    const html = renderClient(baseClient({ redirectUris: ['http://127.0.0.1/callback', 'https://client.example/cb'] }))
    expect(html).toContain('narvi_mcp_c_abc')
    expect(html).toContain('http://127.0.0.1/callback')
    expect(html).toContain('https://client.example/cb')
    expect(html).not.toContain('<a ')
    expect(html).not.toContain('href=')
  })

  it('hostile admin-supplied strings render as text', () => {
    const html = renderClient(baseClient({ clientName: XSS, redirectUris: [`https://x.example/${XSS}`] }))
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a disabled client says so', () => {
    expect(renderClient(baseClient({ disabledAt: '2026-09-22T00:00:00Z' }))).toContain('disabled 2026-09-22 00:00:00Z')
    expect(renderClient(baseClient())).not.toContain('disabled')
  })

  it('deleting starts behind a confirmation', () => {
    const html = renderClient(baseClient())
    expect(html).toContain('>Delete<')
    expect(html).not.toContain('Confirm delete')
  })
})
