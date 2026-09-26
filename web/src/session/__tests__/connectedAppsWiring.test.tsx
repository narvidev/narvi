// connectedAppsWiring.test.tsx -- what ConnectedAppsSection.tsx's admin
// drawer SENDS, from its own wiring: the options MemberConnectedApps (the
// Members & access drawer) hands to TanStack Query, captured as the
// component passes them and run against a stubbed fetch.
//
// Why capture: this suite runs in plain Node (vitest.config.ts: no DOM), and
// a static render never runs a queryFn or fires a mutation. A test that
// fills the query cache itself, or calls the endpoint helpers itself,
// proves the helpers and never the component -- the drawer's revoke with
// its two ids swapped, or pointed at the admin's own list, passed such a
// test. useQuery and useMutation are wrapped, never replaced: the real
// hooks still run, and each call's options are recorded on the way in.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import type { ReactNode } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type * as ReactQuery from '@tanstack/react-query'

import type { Member } from '@narvi/contracts/rest-dtos'

import { mcpAuthorizationQueryKeys } from '../../api/queryKeys'
import { MemberConnectedApps } from '../ConnectedAppsSection'

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
})
