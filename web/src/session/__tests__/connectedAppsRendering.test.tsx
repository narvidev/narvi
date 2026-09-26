// connectedAppsRendering.test.tsx -- ConnectedAppsSection.tsx's own
// defining risks: clientName and redirect URIs are admin-supplied -- or,
// for a self-registered client, app-supplied -- strings (text, never
// markup, never an href), the identity line never borrows an
// administrator's claim, a scope-less authorization must say
// plainly that it sees nothing, and both destructive actions (revoke,
// delete) must never be a single bare click. Mirrors
// integrationsRendering.test.tsx's own pattern: assert on specific visible
// text, never the whole rendered HTML.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

import type { MCPAuthorization, MCPClient, Member } from '@narvi/contracts/rest-dtos'

import { listMemberMCPAuthorizations, revokeMemberMCPAuthorization } from '../../api/endpoints'
import { mcpAuthorizationQueryKeys } from '../../api/queryKeys'
import { ConnectedAppRow, ConnectedAppsTable, MCPClientRow, MemberConnectedApps } from '../ConnectedAppsSection'

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

  it('a scope-less last approval says so, never that the app has no access', () => {
    const html = renderAuthorization(baseAuthorization({ scopes: [] }))
    expect(html).toContain('Last approved: no tools')
    expect(html).not.toMatch(/no access/i)
  })

  it('a hostile client name renders as text', () => {
    const html = renderAuthorization(baseAuthorization({ clientName: XSS }))
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a metadata-document client is identified by its host, its self-chosen name only beside it', () => {
    const html = renderAuthorization(
      baseAuthorization({ clientKind: 'metadata_document', clientId: 'https://tools.example/mcp/client.json', clientName: `Registered by an administrator ${XSS}` }),
    )
    expect(html).toContain('Identified by tools.example')
    expect(html).not.toContain('<script>')
    expect(html).not.toContain('Registered by an administrator of this deployment')
  })

  it('a self-registered client never claims an administrator vouched for it', () => {
    const html = renderAuthorization(baseAuthorization({ clientKind: 'dynamic', clientId: 'narvi_mcp_d_abc' }))
    expect(html).toContain('Registered by the app itself')
    expect(html).not.toContain('administrator')
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

describe('ConnectedAppsTable', () => {
  it('renders one row per authorization, each saying exactly what ConnectedAppRow says', () => {
    const own = baseAuthorization()
    const document = baseAuthorization({ id: '3f2504e0-4f89-11d3-9a0c-0305e82c3303', clientKind: 'metadata_document', clientId: 'https://tools.example/mcp/client.json', clientName: 'Tools' })
    const html = renderToStaticMarkup(<ConnectedAppsTable authorizations={[own, document]} revokingId={undefined} onRevoke={noop} />)
    expect(html.match(/<tr>/g)?.length).toBe(3)
    expect(html).toContain('Registered by an administrator of this deployment')
    expect(html).toContain('Identified by tools.example')
    expect(html).not.toContain('Revoking…')
  })

  it('a revocation in flight never skips another row\'s confirmation', () => {
    const other = baseAuthorization({ id: '3f2504e0-4f89-11d3-9a0c-0305e82c3304' })
    const html = renderToStaticMarkup(<ConnectedAppsTable authorizations={[baseAuthorization(), other]} revokingId={other.id} onRevoke={noop} />)
    // A row shows "Revoking…" only once its confirmation is open, which a
    // static render never is: both rows still offer the bare first step.
    expect(html.match(/>Revoke</g)?.length).toBe(2)
  })
})

function baseMember(overrides: Partial<Member> = {}): Member {
  return {
    id: 'user/1?x',
    email: 'sarah@example.invalid',
    displayName: 'Sarah K.',
    role: 'member',
    disabled: false,
    createdAt: '2026-08-20T02:00:00Z',
    identities: [],
    ...overrides,
  }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } })
}

describe('MemberConnectedApps -- an admin\'s view of a member\'s connected apps', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('lists the member\'s authorizations from the admin route, and revokes through it, the ids escaped into the path', async () => {
    const member = baseMember()
    const fetchSpy = vi.fn(async (_url: string, init?: RequestInit) => {
      if (init?.method === 'DELETE') return new Response(null, { status: 204 })
      return jsonResponse({ authorizations: [baseAuthorization()] })
    })
    vi.stubGlobal('fetch', fetchSpy)

    const queryClient = new QueryClient()
    await queryClient.fetchQuery({ queryKey: mcpAuthorizationQueryKeys.member(member.id), queryFn: () => listMemberMCPAuthorizations(member.id) })
    await revokeMemberMCPAuthorization(member.id, 'auth/2')
    const [listCall, revokeCall] = fetchSpy.mock.calls
    expect(String(listCall[0])).toContain('/api/members/user%2F1%3Fx/mcp-authorizations')
    expect(String(revokeCall[0])).toContain('/api/members/user%2F1%3Fx/mcp-authorizations/auth%2F2')
    expect((revokeCall[1] as RequestInit).method).toBe('DELETE')

    const html = renderToStaticMarkup(
      <QueryClientProvider client={queryClient}>
        <MemberConnectedApps member={member} />
      </QueryClientProvider>,
    )
    expect(html).toContain('Connected apps · Sarah K.')
    expect(html).toContain('Editor Plugin')
    expect(html).toContain('Registered by an administrator of this deployment')
    expect(html).toContain('no token is ever shown')
    // Revoking starts behind the same confirmation as the member's own.
    expect(html).toContain('>Revoke<')
    expect(html).not.toContain('Confirm revoke')
  })

  it('a member with no connected apps says so', () => {
    const member = baseMember()
    const queryClient = new QueryClient()
    queryClient.setQueryData(mcpAuthorizationQueryKeys.member(member.id), { authorizations: [] })
    const html = renderToStaticMarkup(
      <QueryClientProvider client={queryClient}>
        <MemberConnectedApps member={member} />
      </QueryClientProvider>,
    )
    expect(html).toContain('No connected apps.')
    expect(html).not.toContain('<table')
  })

  it('a hostile display name or client name renders as text', () => {
    const member = baseMember({ displayName: `Sarah ${XSS}` })
    const queryClient = new QueryClient()
    queryClient.setQueryData(mcpAuthorizationQueryKeys.member(member.id), { authorizations: [baseAuthorization({ clientName: XSS })] })
    const html = renderToStaticMarkup(
      <QueryClientProvider client={queryClient}>
        <MemberConnectedApps member={member} />
      </QueryClientProvider>,
    )
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('every list key shares one prefix, so a revocation or a client deletion invalidates them all', () => {
    const all = mcpAuthorizationQueryKeys.all()
    for (const key of [mcpAuthorizationQueryKeys.mine(), mcpAuthorizationQueryKeys.member('u1')]) {
      expect(key.slice(0, all.length)).toEqual([...all])
    }
    expect(mcpAuthorizationQueryKeys.member('u1')).not.toEqual(mcpAuthorizationQueryKeys.member('u2'))
  })
})
