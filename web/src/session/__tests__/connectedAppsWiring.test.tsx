// connectedAppsWiring.test.tsx -- what ConnectedAppsSection.tsx's admin
// views SEND, from their own wiring: the options MemberConnectedApps (the
// Members & access drawer) and MCPClientsSection hand to TanStack Query,
// captured as the component passes them and run against a stubbed fetch.
//
// Why capture: this suite runs in plain Node (vitest.config.ts: no DOM), and
// a static render never runs a queryFn or fires a mutation. A test that
// fills the query cache itself, or calls the endpoint helpers itself,
// proves the helpers and never the component -- the drawer's revoke with
// its two ids swapped, or pointed at the admin's own list, passed such a
// test. useQuery and useMutation are wrapped, never replaced: the real
// hooks still run, and each call's options are recorded on the way in.
//
// Why click: a captured mutationFn proves how a flag or an id becomes a
// route, and nothing about which flag or id a BUTTON hands it -- Confirm
// disable passing false, Enable passing true, the section negating the
// row's flag, the drawer revoking the member's id, or Disable firing with
// no confirmation all passed that test. So each control is also driven
// from its own onClick down to fetch: drive() renders the section, calls
// every component in it inline -- one wrapper's render, so each row's own
// confirmation state is a hook of that wrapper -- and clicks the named
// button's real onClick. A click that opens a confirmation sets that
// state during the render, React renders the wrapper again, and the next
// step clicks in that render: the confirmation is the component's own,
// never one the test re-implements.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { isValidElement, type ReactElement, type ReactNode } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type * as ReactQuery from '@tanstack/react-query'

import type { MCPAuthorization, MCPClient, Member } from '@narvi/contracts/rest-dtos'

import { mcpAuthorizationQueryKeys, mcpClientQueryKeys } from '../../api/queryKeys'
import { MCPClientsSection, MemberConnectedApps } from '../ConnectedAppsSection'

// The options of every useQuery and useMutation call, in call order.
const captured = vi.hoisted(() => ({ queries: [] as unknown[], mutations: [] as unknown[] }))

vi.mock('@tanstack/react-query', async (importOriginal) => {
  const actual = await importOriginal<typeof ReactQuery>()
  const useQuery = (...args: Parameters<typeof actual.useQuery>) => {
    captured.queries.push(args[0])
    return actual.useQuery(...args)
  }
  const useMutation = (...args: Parameters<typeof actual.useMutation>) => {
    captured.mutations.push(args[0])
    return actual.useMutation(...args)
  }
  return { ...actual, useQuery: useQuery as typeof actual.useQuery, useMutation: useMutation as typeof actual.useMutation }
})

type Call = { url: string; method: string }

// stubFetch records every request the code under test sends -- its path
// and method -- and answers each with an empty success.
function stubFetch(): Call[] {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url: String(url), method: init?.method ?? 'GET' })
      if (init?.method === 'DELETE') return new Response(null, { status: 204 })
      return new Response(JSON.stringify({ authorizations: [] }), { status: 200, headers: { 'content-type': 'application/json' } })
    }),
  )
  return calls
}

function render(node: ReactNode): void {
  renderToStaticMarkup(<QueryClientProvider client={new QueryClient()}>{node}</QueryClientProvider>)
}

// The query function and the mutation function exactly as the component
// passed them. Each is called with only what the component's own function
// reads: the signal, and the mutation's variables.
type CapturedQuery = { queryKey: unknown; queryFn: (context: { signal: AbortSignal }) => Promise<unknown> }
type CapturedMutation<V> = { mutationFn: (variables: V) => Promise<unknown> }

function member(overrides: Partial<Member> = {}): Member {
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

function client(overrides: Partial<MCPClient> = {}): MCPClient {
  return {
    id: 'client/1',
    clientId: 'narvi_mcp_c_one',
    clientName: 'Editor Plugin',
    kind: 'preregistered',
    redirectUris: ['http://127.0.0.1/callback'],
    clientUri: null,
    createdAt: '2026-09-20T10:00:00Z',
    disabledAt: null,
    ...overrides,
  }
}

function authorization(overrides: Partial<MCPAuthorization> = {}): MCPAuthorization {
  return {
    id: 'auth/1',
    clientId: 'narvi_mcp_c_one',
    clientName: 'Editor Plugin',
    clientKind: 'preregistered',
    scopes: ['mcp:read'],
    createdAt: '2026-09-20T10:00:00Z',
    expiresAt: '2026-12-19T10:00:00Z',
    lastUsedAt: null,
    ...overrides,
  }
}

// A rendered tree with every component called away: host elements, with
// their real props, and text.
type Host = { tag: string; props: { onClick?: () => void; disabled?: boolean }; children: Resolved[] }
type Resolved = string | Host

// resolve calls every function component in node inline, down to host
// elements. Called during a component's render, each hook those components
// use belongs to that component.
function resolve(node: ReactNode): Resolved[] {
  if (node === null || node === undefined || typeof node === 'boolean') return []
  if (typeof node === 'string' || typeof node === 'number' || typeof node === 'bigint') return [String(node)]
  if (Array.isArray(node)) return node.flatMap((child: ReactNode) => resolve(child))
  if (!isValidElement(node)) throw new Error('resolve: a node that is neither text, a list nor an element')
  const { type, props } = node as ReactElement<{ children?: ReactNode }>
  if (typeof type === 'function') return resolve((type as (p: typeof props) => ReactNode)(props))
  if (typeof type === 'string') return [{ tag: type, props: props as Host['props'], children: resolve(props.children) }]
  return resolve(props.children) // a fragment
}

function textOf(node: Resolved): string {
  return typeof node === 'string' ? node : node.children.map(textOf).join('')
}

function hostsIn(nodes: Resolved[], tag: string): Host[] {
  return nodes.flatMap((n) => (typeof n === 'string' ? [] : [...(n.tag === tag ? [n] : []), ...hostsIn(n.children, tag)]))
}

// rowShowing is the one table row whose text contains text.
function rowShowing(tree: Resolved[], text: string): Host {
  const rows = hostsIn(tree, 'tr').filter((r) => textOf(r).includes(text))
  const [row] = rows
  if (row === undefined || rows.length !== 1) throw new Error(`expected one row showing "${text}", found ${rows.length}`)
  return row
}

function buttonLabels(row: Host): string[] {
  return hostsIn([row], 'button').map(textOf)
}

// A click on the button labelled `click` in the row showing `row`.
type Step = { row: string; click: string }
// What drive did: the labels it clicked, in order, and the last render.
type Run = { clicked: string[]; last: Resolved[] }

function Driver({ ui, steps, run }: { ui: ReactNode; steps: readonly Step[]; run: Run }) {
  const tree = resolve(ui)
  run.last = tree
  const step = steps[run.clicked.length]
  if (step !== undefined) {
    const row = rowShowing(tree, step.row)
    const buttons = hostsIn([row], 'button').filter((b) => textOf(b) === step.click)
    const [button] = buttons
    if (button === undefined || buttons.length !== 1) throw new Error(`no single "${step.click}" button in the row showing "${step.row}"; it offers ${JSON.stringify(buttonLabels(row))}`)
    if (button.props.disabled === true || button.props.onClick === undefined) throw new Error(`"${step.click}" in the row showing "${step.row}" cannot be clicked`)
    run.clicked.push(step.click)
    button.props.onClick()
  }
  return null
}

// drive renders ui and clicks each step's button in turn, one per render.
// A click that changes no state ends the run there: run.clicked says how
// far it got.
function drive(ui: ReactNode, steps: readonly Step[], queryClient: QueryClient): Run {
  const run: Run = { clicked: [], last: [] }
  renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <Driver ui={ui} steps={steps} run={run} />
    </QueryClientProvider>,
  )
  return run
}

// settled waits for every mutation a click started to finish.
async function settled(queryClient: QueryClient): Promise<void> {
  await vi.waitFor(() => expect(queryClient.isMutating()).toBe(0))
}

beforeEach(() => {
  captured.queries.length = 0
  captured.mutations.length = 0
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('MemberConnectedApps -- the drawer sends its own list and revoke to the admin routes, for that member', () => {
  it('lists with GET on the member\'s own admin route, the member id escaped into the path, under the member\'s own key', async () => {
    const m = member()
    render(<MemberConnectedApps member={m} />)
    expect(captured.queries).toHaveLength(1)
    const list = captured.queries[0] as CapturedQuery
    expect(list.queryKey).toEqual(mcpAuthorizationQueryKeys.member(m.id))

    const calls = stubFetch()
    await list.queryFn({ signal: new AbortController().signal })
    expect(calls).toEqual([{ url: '/api/members/user%2F1%3Fx/mcp-authorizations', method: 'GET' }])
  })

  it('revokes with DELETE on that member\'s authorization, member id first and authorization id second, both escaped', async () => {
    const m = member()
    render(<MemberConnectedApps member={m} />)
    expect(captured.mutations).toHaveLength(1)
    const revoke = captured.mutations[0] as CapturedMutation<string>

    const calls = stubFetch()
    await revoke.mutationFn('auth/2')
    expect(calls).toEqual([{ url: '/api/members/user%2F1%3Fx/mcp-authorizations/auth%2F2', method: 'DELETE' }])
  })

  it('clicking Revoke, then Confirm revoke, in a row sends DELETE /api/members/{memberId}/mcp-authorizations/{that row\'s authorizationId}', async () => {
    const m = member()
    const queryClient = new QueryClient()
    queryClient.setQueryData(mcpAuthorizationQueryKeys.member(m.id), {
      authorizations: [authorization(), authorization({ id: 'auth/2', clientId: 'narvi_mcp_c_two', clientName: 'Other Tool' })],
    })
    const calls = stubFetch()

    const run = drive(<MemberConnectedApps member={m} />, [
      { row: 'Other Tool', click: 'Revoke' },
      { row: 'Other Tool', click: 'Confirm revoke' },
    ], queryClient)
    await settled(queryClient)

    expect(run.clicked).toEqual(['Revoke', 'Confirm revoke'])
    expect(calls).toEqual([{ url: '/api/members/user%2F1%3Fx/mcp-authorizations/auth%2F2', method: 'DELETE' }])
  })
})

describe('MCPClientsSection -- each row\'s Disable and Enable buttons, clicked, send that client\'s own admin route', () => {
  // The first client is disabled, the second enabled: each test clicks in
  // one row of two, so the id sent is that row's own.
  function clientsCached(): QueryClient {
    const queryClient = new QueryClient()
    queryClient.setQueryData(mcpClientQueryKeys.list(), {
      clients: [client({ disabledAt: '2026-09-22T00:00:00Z' }), client({ id: 'client/2', clientId: 'narvi_mcp_c_two', clientName: 'Other Tool' })],
    })
    return queryClient
  }

  it('clicking Disable, then Confirm disable, sends POST /api/mcp-clients/{id}/disable for that row\'s client', async () => {
    const queryClient = clientsCached()
    const calls = stubFetch()

    const run = drive(<MCPClientsSection />, [
      { row: 'narvi_mcp_c_two', click: 'Disable' },
      { row: 'narvi_mcp_c_two', click: 'Confirm disable' },
    ], queryClient)
    await settled(queryClient)

    expect(run.clicked).toEqual(['Disable', 'Confirm disable'])
    expect(calls).toEqual([{ url: '/api/mcp-clients/client%2F2/disable', method: 'POST' }])
  })

  it('clicking Disable alone sends nothing: it opens the confirmation, whose Confirm disable is the one that sends', async () => {
    const queryClient = clientsCached()
    const calls = stubFetch()

    const run = drive(<MCPClientsSection />, [{ row: 'narvi_mcp_c_two', click: 'Disable' }], queryClient)
    await settled(queryClient)

    expect(run.clicked).toEqual(['Disable'])
    expect(buttonLabels(rowShowing(run.last, 'narvi_mcp_c_two'))).toEqual(['Confirm disable', 'Cancel'])
    expect(calls).toEqual([])
  })

  it('clicking Enable on a disabled client sends POST /api/mcp-clients/{id}/enable for that row\'s client, with no confirmation', async () => {
    const queryClient = clientsCached()
    const calls = stubFetch()

    const run = drive(<MCPClientsSection />, [{ row: 'narvi_mcp_c_one', click: 'Enable' }], queryClient)
    await settled(queryClient)

    expect(run.clicked).toEqual(['Enable'])
    expect(calls).toEqual([{ url: '/api/mcp-clients/client%2F1/enable', method: 'POST' }])
  })
})

describe('MCPClientsSection -- the disable/enable mutation maps its flag to the client\'s own admin route', () => {
  it('disabled: true sends POST .../disable and disabled: false sends POST .../enable, the client id escaped', async () => {
    render(<MCPClientsSection />)
    // The section's mutations, in the order it declares them: register,
    // delete, then disable/enable.
    expect(captured.mutations).toHaveLength(3)
    const setDisabled = captured.mutations[2] as CapturedMutation<{ clientId: string; disabled: boolean }>

    const calls = stubFetch()
    await setDisabled.mutationFn({ clientId: 'client/1', disabled: true })
    await setDisabled.mutationFn({ clientId: 'client/1', disabled: false })
    expect(calls).toEqual([
      { url: '/api/mcp-clients/client%2F1/disable', method: 'POST' },
      { url: '/api/mcp-clients/client%2F1/enable', method: 'POST' },
    ])
  })
})
