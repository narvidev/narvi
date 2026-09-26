// connectedAppsWiring.test.tsx -- what the connected-apps screens SEND,
// what each of their buttons does when clicked, and what each offers while
// its request is in flight and once it has finished:
// ConnectedAppsSection.tsx's rows and sections,
// and the Members & access row (MembersPanel.tsx's MemberRow) whose button
// opens a member's Connected apps drawer.
//
// # Captured options
//
// This suite runs in plain Node (vitest.config.ts: no DOM), and a static
// render never runs a queryFn or fires a mutation. A test that fills the
// query cache itself, or calls the endpoint helpers itself, proves the
// helpers and never the component -- the drawer's revoke with its two ids
// swapped, or pointed at the admin's own list, passed such a test. So
// useQuery and useMutation are wrapped, never replaced: the real hooks
// still run, and each call's options are recorded on the way in and run
// against a stubbed fetch. That proves how an id or a flag becomes a
// route, and nothing about which id or flag a button hands it.
//
// # Clicks
//
// drive() renders a wrapper with renderToStaticMarkup that calls the ui and
// every component in it inline, so each component's own state -- a row's
// confirmation, the drawer's open flag, a form field -- is a hook of that
// wrapper. Each step clicks the named button's real onClick, or types into
// a field through its real onChange; React then renders the wrapper again,
// and the next step acts in that render. A render in which a component
// updates its own state as it renders -- a row closing a confirmation its
// props say is done -- React throws away and renders again at once, and
// so does the wrapper: no step acts in it, and it is not the last render.
// The test never re-implements a confirmation. A bare label clicks a section's own button, outside its
// rows; { row, click } clicks in the one row showing that text.
//
// The render after a click shows the mutation it started pending, for
// real: the hook's observer turns pending as mutate() is called. The
// mutation runs the mutationFn of the render the click happened in -- or,
// if the handler updated state before it called mutate(), of the render
// those updates produce. A browser runs the same one: React renders a
// click's updates in a microtask the first of them queues, the effect of
// that render hands a pending mutation its new options, and the mutation
// calls its mutationFn a microtask after mutate().
//
// # Rows: every state, from the props
//
// ROW_MODELS describe each row component: every flag, enum and nullable
// field it is handed that can change what it renders, with every value it
// can take (the axes) -- a client's kind, whether it is disabled, whether
// its delete or its disable is in flight; an authorization's client kind,
// whether it was ever used, whether its revocation is in flight; whether
// the viewer may manage a member, the member's role and disabled flag, and
// its linked identity: none, or one through each provider, linked each way
// -- its own states -- the confirmation it has open, its drawer; and what
// it must offer in each combination of the two -- its buttons, in order,
// and what a click on each calls and leaves open. Each enum's values are
// checked against the contract types: a value missing from an axis fails
// the typecheck. The harness holds the props in its own state, so a step
// can change them under the row, as the section does when a request starts
// or ends or its list refetches. A prop change leaves the row's own state
// where it was, unless the model says the new props close it (settles): a
// disable confirmation is done once the list shows the client disabled.
// Starting from every combination of the axes, a walk reaches every own
// state through the row's own clicks and every combination the row can be
// in by changing one prop at a time. It tells two states apart by the
// props, the model's own state AND the row's own React state -- every
// useState value read while the row renders -- so a state that renders
// like another but holds something else is a state of its own, followed on
// by every click and every prop change: a disable confirmation kept, not
// closed, under Enable would show again once the list showed the client
// enabled, where the model offers Disable. Then, for each row:
//   - every combination the row can be in is reached, and in every state
//     the walk reaches, the row offers exactly the model's buttons, and a
//     click on each calls exactly the model's callbacks and leaves the row
//     where the model says -- checked up to CLICK_DEPTH clicks on from
//     every state it reaches;
//   - while a request is in flight, the button that sends it shows its
//     in-flight label ("Deleting…", "Disabling…", "Enabling…",
//     "Revoking…") and is disabled, and no other button in the row sends
//     that request again; with nothing in flight, no in-flight label shows.
//     Which buttons send a request is found by clicking each with its
//     in-flight prop false, not listed.
//
// # Sections
//
// SECTION_FLOWS click through each section down to the stubbed fetch: each
// callback a section hands a row, and the section's own Register client,
// sends exactly the request asserted. Each list holds two rows, and every
// call a flow makes in one of them -- Confirm delete, Confirm disable,
// Enable, Confirm revoke -- a flow also makes in the other: a handler that
// always sends the first row's id, or the last's, fails a flow. The two
// rows are lookalikes, one name and different ids, so a handler that finds
// its row by name fails one too. Disable shows only in a row whose client
// is enabled, and Enable only in one whose client is disabled, so their
// second flows run over the two clients the other way round, and a third
// over two clients both enabled, or both disabled: a handler that finds
// its row by whether it is disabled fails that one. What else tells the
// rows apart -- an authorization's host, a client's client ID -- is what
// a flow finds its row by. A flow also asserts every row's buttons in the
// render right after its last click -- with its request in flight, only
// the row that sent it shows the in-flight label and is locked, the other
// row's own confirmation open where the flow opened it -- and, once the
// request is done, exactly which of the screen's cached lists and audit
// pages it invalidated.
//
// A request ends after the static render has returned, so no render here
// reaches its end for real. Each flow that sends one therefore runs again,
// the render after its last click reporting that request -- with the
// variables the click really sent -- pending, failed and succeeded, in
// place of the hook's own result. Pending, the rows offer what the flow
// asserts; failed or succeeded, the same, with the button that sent it
// back to its own label and enabled: no row stays locked. And the
// section's notice for that request shows exactly while it has failed:
// not while it is pending, as it is once sent again, nor once it has
// succeeded.
//
// The "no section control without a flow" describe block makes a section
// control without a flow fail the suite by name:
//   - every callback a section hands a row is called by one of that
//     section's flows, and every call one of them makes in a row -- the
//     callback and its arguments -- one of them makes in each row of the
//     list;
//   - in every combination of its list's state (loading, failed, refused,
//     empty, listed), each of its mutations' status (idle, pending, failed,
//     succeeded) and its form (each field empty or filled), the buttons a
//     section renders outside its rows -- its table's header included --
//     are exactly those SECTION_CONTROLS names. There, the list's failures
//     and each mutation's pending, failed and succeeded status are results
//     the hooks report in place of their own: no synchronous render reaches
//     a failure or a finished request (the flows above reach pending for
//     real);
//   - in each state of its form, over its listed rows with nothing pending,
//     every enabled button outside the rows is clicked, in that very state,
//     by a SECTION_FLOW step that names it outside the rows -- a click in a
//     row on a button of the same name does not count.
//
// # A member's drawer's list
//
// In each state of its list, the drawer says exactly what DRAWER_LIST
// does -- a list it could not load is never a member with no apps -- and
// its list's options, mounted on a query observer, fetch that member's
// list: a query that never ran would leave it loading for good.
//
// Not covered here:
//   - a row's text fields and lists -- names, ids, scopes, redirect URIs,
//     dates -- take one value each, except a member's identities, none or
//     one: the axes are its flags, enums and nullable fields;
//   - a row's state held anywhere but in useState -- a ref, a reducer, a
//     query's or mutation's own -- or held there as what JSON cannot write
//     out (a function, a Map, a Set), that shows only more than
//     CLICK_DEPTH clicks after a state the walk reaches, or only after a
//     prop change;
//   - with one of a client row's requests in flight, whether it can send
//     its OTHER one: the check above is per request, and today a client
//     whose disable is in flight can still be deleted, and one whose
//     delete is in flight disabled or enabled;
//   - a section handler that finds its row by what a flow finds it by: an
//     authorization's client ID, whose host its row shows, or a client's;
//   - a section once its list has refetched after a request: a finished
//     request is shown over the list the flow started with, and what a
//     row does as its props change is the row walk's;
//   - the section buttons that only a finished request's state renders are
//     checked for what is offered, never clicked;
//   - controls that are not buttons, such as the member's role select;
//   - anything MembersPanel does with the onShowAudit it hands MemberRow:
//     this file proves only that Audit log calls it with that member's id.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { isValidElement, useState, type ReactElement, type ReactNode, type SetStateAction } from 'react'
import type * as ReactModule from 'react'
import { QueryClient, QueryClientProvider, QueryObserver, type QueryKey, type QueryObserverOptions } from '@tanstack/react-query'
import type * as ReactQuery from '@tanstack/react-query'

import type { Identity, MCPAuthorization, MCPClient, Member } from '@narvi/contracts/rest-dtos'

import { ApiError } from '../../api/http'
import { auditLogQueryKeys, mcpAuthorizationQueryKeys, mcpClientQueryKeys, memberQueryKeys } from '../../api/queryKeys'
import { ConnectedAppRow, ConnectedAppsSection, MCPClientRow, MCPClientsSection, MemberConnectedApps } from '../ConnectedAppsSection'
import { MemberRow } from '../MembersPanel'

// The options of every useQuery and useMutation call, in call order.
const captured = vi.hoisted(() => ({ queries: [] as unknown[], mutations: [] as unknown[] }))

// Every state update a component has made, and whether the click being
// handled has made one yet.
const updates = vi.hoisted(() => {
  let count = 0
  let atClick: number | undefined
  return {
    made: () => void count++,
    count: () => count,
    clickStarts: () => void (atClick = count),
    clickEnds: () => void (atClick = undefined),
    madeDuringClick: () => atClick !== undefined && count > atClick,
  }
})

// Hook results the render after a drive's last step reports instead of the
// real ones, by the order of the calls within a render.
const forcing = vi.hoisted(() => {
  const next = { queries: 0, mutations: 0 }
  const state = {
    queries: [] as (object | undefined)[],
    mutations: [] as (object | undefined)[],
    active: false,
    // A render begins: whether it is the one after the drive's last step.
    renders: (last: boolean) => {
      state.active = last
      next.queries = 0
      next.mutations = 0
    },
    // The result the next call of that hook reports instead of its own, if any.
    next: (hook: 'queries' | 'mutations'): object | undefined => (state.active ? state[hook][next[hook]++] : void next[hook]++),
  }
  return state
})

// The own state of the row a harness drives: while it is set, every
// useState value read is added to it, in call order (see ResolveOptions.own).
const ownState = vi.hoisted(() => ({ into: undefined as unknown[] | undefined }))

// React's own useState, with every update counted, and every value read
// while ownState is set recorded.
vi.mock('react', async (importOriginal) => {
  const actual = await importOriginal<typeof ReactModule>()
  function useState<S>(initial?: S | (() => S)) {
    const [state, set] = actual.useState(initial)
    ownState.into?.push(state)
    const counted = actual.useCallback(
      (action: SetStateAction<S | undefined>) => {
        updates.made()
        set(action)
      },
      [set],
    )
    return [state, counted] as const
  }
  return { ...actual, useState: useState as typeof actual.useState }
})

vi.mock('@tanstack/react-query', async (importOriginal) => {
  const actual = await importOriginal<typeof ReactQuery>()
  const { useState } = await import('react')
  const useQuery = (...args: Parameters<typeof actual.useQuery>) => {
    captured.queries.push(args[0])
    const result = actual.useQuery(...args)
    const forced = forcing.next('queries')
    return forced === undefined ? result : { ...result, ...forced }
  }
  type MutationOptions = Parameters<typeof actual.useMutation>[0]
  type MutationFn = NonNullable<MutationOptions['mutationFn']>
  // A server render runs no effects, so the observer keeps the options it
  // was built with. It gets a mutationFn that runs, for each mutate() in
  // turn, the mutationFn pinned when mutate() was called: that render's,
  // or, when the click's handler had updated state first, the next
  // render's (see # Clicks above).
  const useMutation = (...args: Parameters<typeof actual.useMutation>) => {
    const [options, queryClient] = args
    captured.mutations.push(options)
    const [synced] = useState(() => {
      const pins: { mutationFn?: MutationFn }[] = []
      const observed: MutationOptions = {
        ...options,
        mutationFn: (...call: Parameters<MutationFn>) => {
          const pin = pins.shift()
          if (pin?.mutationFn === undefined) throw new Error('useMutation: a mutation ran with no render to take its mutationFn from')
          return pin.mutationFn(...call)
        },
      }
      return { pins, observed }
    })
    for (const pin of synced.pins) pin.mutationFn ??= options.mutationFn
    const result = actual.useMutation(synced.observed, queryClient)
    const pin = () => synced.pins.push(updates.madeDuringClick() ? {} : { mutationFn: options.mutationFn })
    const wrapped: typeof result = {
      ...result,
      mutate: (...call) => {
        pin()
        result.mutate(...call)
      },
      mutateAsync: (...call) => {
        pin()
        return result.mutateAsync(...call)
      },
    }
    const forced = forcing.next('mutations')
    return forced === undefined ? wrapped : { ...wrapped, ...forced }
  }
  return { ...actual, useQuery: useQuery as typeof actual.useQuery, useMutation: useMutation as typeof actual.useMutation }
})

type Call = { url: string; method: string; body?: unknown }

// stubFetch records every request the code under test sends -- its path,
// its method and any JSON body -- and answers each with an empty success.
function stubFetch(): Call[] {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url: String(url), method: init?.method ?? 'GET', ...(typeof init?.body === 'string' ? { body: JSON.parse(init.body) as unknown } : {}) })
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

// allOf returns values, and fails the typecheck unless they are every
// value of T -- a new client kind, role, identity provider or way of
// linking an identity must join the axes below.
function allOf<T>() {
  return <const L extends readonly T[]>(values: L & ([Exclude<T, L[number]>] extends [never] ? unknown : { missing: Exclude<T, L[number]> })): L => values
}

const CLIENT_KINDS = allOf<MCPClient['kind']>()(['preregistered', 'dynamic', 'metadata_document'])
const AUTHORIZATION_KINDS = allOf<MCPAuthorization['clientKind']>()(['preregistered', 'dynamic', 'metadata_document'])
const MEMBER_ROLES = allOf<Member['role']>()(['admin', 'maintainer', 'member', 'viewer'])
const IDENTITY_PROVIDERS = allOf<Identity['provider']>()(['github', 'slack', 'linear', 'google', 'oidc'])
const IDENTITY_LINKS = allOf<Identity['linkedVia']>()(['auto_email', 'prompt', 'admin'])

// A member's linked identity: none, or one through each provider, linked
// each way.
type LinkedIdentity = Pick<Identity, 'provider' | 'linkedVia'>
const LINKED_IDENTITIES: readonly (LinkedIdentity | null)[] = [null, ...IDENTITY_PROVIDERS.flatMap((provider) => IDENTITY_LINKS.map((linkedVia) => ({ provider, linkedVia })))]

const DISABLED_AT = '2026-09-22T00:00:00Z'

// A client ID of the form each kind has.
const CLIENT_ID_OF_KIND = {
  preregistered: 'narvi_mcp_c_one',
  dynamic: 'narvi_mcp_d_one',
  metadata_document: 'https://tools.example/mcp/client.json',
} as const satisfies Record<MCPClient['kind'], string>

const IDENTITY: Identity = { id: 'identity/1', provider: 'github', externalId: 'sarah', linkedVia: 'auto_email', createdAt: '2026-08-20T02:00:00Z' }

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

// -- Rendering without a DOM --

// A rendered tree with every component called away: host elements, with
// their real props, and text.
type HostProps = {
  onClick?: () => void
  onChange?: (event: { target: { value: string } }) => void
  disabled?: boolean
  placeholder?: string
  className?: string
  'aria-label'?: string
  'aria-expanded'?: boolean
  member?: Member
}
type Host = { tag: string; props: HostProps; children: Resolved[] }
type Resolved = string | Host

// The row components. Rendering a section, resolve marks each row as a
// host named for its component, records each callback prop the section
// hands it, and each call of one.
const ROWS = new Map<unknown, string>([
  [MCPClientRow, 'MCPClientRow'],
  [ConnectedAppRow, 'ConnectedAppRow'],
])
const ROW_TAGS = new Set(ROWS.values())

// What resolving a section records: each callback it hands a row; each
// call of one, in order; and each call with its arguments and the position
// of the row it was made in, first row 1 -- counting, in each render, the
// rows resolved so far.
type Wiring = { handed: Set<string>; called: string[]; calledIn: Set<string>; rows: number }

function newWiring(): Wiring {
  return { handed: new Set(), called: [], calledIn: new Set(), rows: 0 }
}

// A row harness's handle on its props (see RowHarness): take hands it the
// harness's setter, and set is that setter.
type Become = { set?: (props: RowProps) => void; take: (set: (props: RowProps) => void) => void }

function newBecome(): Become {
  const become: Become = { take: (set) => void (become.set = set) }
  return become
}

type ResolveOptions = {
  // Components never called: each renders as a host element named for it,
  // with its props, and no children. Needed for a component that appears
  // only after a click, because its hooks would change the wrapper's hook
  // count between renders.
  opaque?: ReadonlyMap<unknown, string>
  // Given when resolving a section.
  wiring?: Wiring
  // Given when driving a row harness.
  become?: Become
  // Given when driving a row harness: the row component, and its own
  // state in the render being resolved -- every useState value it, and
  // each component it renders, reads, in call order.
  own?: { type: unknown; values: unknown[] }
}

// resolve calls every function component in node inline, down to host
// elements. Called during a component's render, each hook those components
// use belongs to that component.
function resolve(node: ReactNode, options: ResolveOptions): Resolved[] {
  if (node === null || node === undefined || typeof node === 'boolean') return []
  if (typeof node === 'string' || typeof node === 'number' || typeof node === 'bigint') return [String(node)]
  if (Array.isArray(node)) return node.flatMap((child: ReactNode) => resolve(child, options))
  if (!isValidElement(node)) throw new Error('resolve: a node that is neither text, a list nor an element')
  const { type, props } = node as ReactElement<Record<string, unknown> & { children?: ReactNode }>
  if (typeof type === 'function') {
    const opaque = options.opaque?.get(type)
    if (opaque !== undefined) return [{ tag: opaque, props: props as HostProps, children: [] }]
    const row = ROWS.get(type)
    const position = row !== undefined && options.wiring !== undefined ? ++options.wiring.rows : undefined
    const outer = ownState.into
    if (options.own?.type === type) ownState.into = options.own.values
    try {
      const rendered = resolve((type as (p: typeof props) => ReactNode)(spied(type, props, options.wiring, position)), options)
      return row !== undefined && position !== undefined ? [{ tag: row, props: {}, children: rendered }] : rendered
    } finally {
      ownState.into = outer
    }
  }
  if (typeof type === 'string') return [{ tag: type, props: props as HostProps, children: resolve(props.children, options) }]
  return resolve(props.children, options) // a fragment
}

// spied returns a row's props with each callback recorded as handed, and
// wrapped to record each call, with its arguments and the row's position;
// any other component's props unchanged.
function spied<P extends Record<string, unknown>>(type: unknown, props: P, wiring: Wiring | undefined, position: number | undefined): P {
  const row = ROWS.get(type)
  if (row === undefined || wiring === undefined) return props
  const out: Record<string, unknown> = { ...props }
  for (const [name, value] of Object.entries(props)) {
    if (typeof value !== 'function') continue
    const callback = `${row}.${name}`
    wiring.handed.add(callback)
    out[name] = (...args: unknown[]) => {
      wiring.called.push(callback)
      wiring.calledIn.add(`${callback}(${args.map((a) => JSON.stringify(a)).join(', ')}) in row ${String(position)}`)
      return (value as (...a: unknown[]) => unknown)(...args)
    }
  }
  return out as P
}

function textOf(node: Resolved): string {
  return typeof node === 'string' ? node : node.children.map(textOf).join('')
}

function hostsIn(nodes: Resolved[], tag: string): Host[] {
  return nodes.flatMap((n) => (typeof n === 'string' ? [] : [...(n.tag === tag ? [n] : []), ...hostsIn(n.children, tag)]))
}

// fieldsIn is every field a person types into, in the order they render.
function fieldsIn(nodes: Resolved[]): Host[] {
  return nodes.flatMap((n) => (typeof n === 'string' ? [] : [...(n.tag === 'input' || n.tag === 'textarea' ? [n] : []), ...fieldsIn(n.children)]))
}

// rowsIn is every row a section renders, each marked by resolve.
function rowsIn(nodes: Resolved[]): Host[] {
  return nodes.flatMap((n) => (typeof n === 'string' ? [] : ROW_TAGS.has(n.tag) ? [n] : rowsIn(n.children)))
}

// buttonsOutsideRows is every button in nodes that no row contains: a
// section's own, wherever it renders -- a table's header included.
function buttonsOutsideRows(nodes: Resolved[]): Host[] {
  return nodes.flatMap((n) => (typeof n === 'string' || ROW_TAGS.has(n.tag) ? [] : [...(n.tag === 'button' ? [n] : []), ...buttonsOutsideRows(n.children)]))
}

// labelOf is a button's accessible name: its aria-label, else its text.
function labelOf(button: Host): string {
  return button.props['aria-label'] ?? textOf(button)
}

// offered is a button as a flow or a model writes it: its accessible name,
// then whether it is disabled and, for a toggle, expanded.
function offered(button: Host): string {
  const expanded = button.props['aria-expanded']
  return `${labelOf(button)}${button.props.disabled === true ? ' (disabled)' : ''}${expanded === undefined ? '' : expanded ? ' (expanded)' : ' (collapsed)'}`
}

// rowShowing is the one row whose text contains text.
function rowShowing(tree: Resolved[], text: string): Host {
  const rows = rowsIn(tree).filter((r) => textOf(r).includes(text))
  const [row] = rows
  if (row === undefined || rows.length !== 1) throw new Error(`expected one row showing "${text}", found ${rows.length}`)
  return row
}

// -- Driving clicks --

// The props a row harness hands its row (see ROW_MODELS).
type RowProps = Readonly<Record<string, unknown>>

// A step: a click on the only button so labelled outside every row, a click
// on the button so labelled in the one row showing `row`, typing `type`
// into the field whose placeholder is `field`, or -- driving a row harness
// -- the props it hands the row becoming `become`.
type Step = string | { row: string; click: string } | { field: string; type: string } | { become: RowProps }

function stepName(step: Step): string {
  if (typeof step === 'string') return step
  if ('click' in step) return `${step.click} in the row showing ${step.row}`
  if ('field' in step) return `type into ${step.field}`
  return 'the props change'
}

function act(tree: Resolved[], step: Step, options: ResolveOptions): void {
  if (typeof step !== 'string' && 'become' in step) {
    if (options.become?.set === undefined) throw new Error('a props change outside a row harness')
    options.become.set(step.become)
    return
  }
  if (typeof step !== 'string' && 'field' in step) {
    const fields = fieldsIn(tree).filter((f) => f.props.placeholder === step.field)
    const [field] = fields
    if (field === undefined || fields.length !== 1 || field.props.onChange === undefined) throw new Error(`no single field "${step.field}" to type into`)
    field.props.onChange({ target: { value: step.type } })
    return
  }
  const label = typeof step === 'string' ? step : step.click
  const candidates = typeof step === 'string' ? buttonsOutsideRows(tree) : hostsIn([rowShowing(tree, step.row)], 'button')
  const where = typeof step === 'string' ? 'the render, outside its rows' : `the row showing "${step.row}"`
  const buttons = candidates.filter((b) => labelOf(b) === label)
  const [button] = buttons
  if (button === undefined || buttons.length !== 1) throw new Error(`no single "${label}" button in ${where}; it offers ${JSON.stringify(candidates.map(labelOf))}`)
  if (button.props.disabled === true || button.props.onClick === undefined) throw new Error(`"${label}" in ${where} cannot be clicked`)
  button.props.onClick()
}

// What drive did: the steps it took, in order, the last render, and --
// driving a row harness -- the row's own state in it (ResolveOptions.own).
type Run = { applied: string[]; last: Resolved[]; own: string }

// rendered records a render in run, and returns the step to take in it.
function rendered(run: Run, tree: Resolved[], steps: readonly Step[]): Step | undefined {
  run.last = tree
  const step = steps[run.applied.length]
  if (step !== undefined) run.applied.push(stepName(step))
  return step
}

// resolveRender resolves ui for one render: the rows it counts start again
// from none, and run records the row's own state in it.
function resolveRender(ui: ReactNode, options: ResolveOptions, run: Run): Resolved[] {
  if (options.wiring !== undefined) options.wiring.rows = 0
  if (options.own !== undefined) options.own.values = []
  const tree = resolve(ui, options)
  run.own = JSON.stringify(options.own?.values ?? [])
  return tree
}

// click takes step in tree, as a click handler: the state updates it makes
// before a mutate() decide which render's mutationFn that mutation runs.
function click(tree: Resolved[], step: Step, options: ResolveOptions): void {
  updates.clickStarts()
  try {
    act(tree, step, options)
  } finally {
    updates.clickEnds()
  }
}

function Driver({ ui, steps, run, options }: { ui: ReactNode; steps: readonly Step[]; run: Run; options: ResolveOptions }) {
  const [, rerender] = useState(0)
  forcing.renders(run.applied.length === steps.length)
  const before = updates.count()
  const tree = resolveRender(ui, options, run)
  // A component updated its own state as it rendered: React throws this
  // render away and renders again at once (see # Clicks above).
  if (updates.count() !== before) return null
  const step = rendered(run, tree, steps)
  if (step !== undefined) {
    click(tree, step, options)
    rerender((n) => n + 1)
  }
  return null
}

// drive renders ui and takes each step in turn, one per render, and then
// renders once more: run.last is the render after the last step.
function drive(ui: ReactNode, steps: readonly Step[], queryClient: QueryClient, options: ResolveOptions = {}): Run {
  const run: Run = { applied: [], last: [], own: '[]' }
  try {
    renderToStaticMarkup(
      <QueryClientProvider client={queryClient}>
        <Driver ui={ui} steps={steps} run={run} options={options} />
      </QueryClientProvider>,
    )
  } finally {
    forcing.renders(false)
  }
  return run
}

// settled waits for every mutation a click started to finish.
async function settled(queryClient: QueryClient): Promise<void> {
  await vi.waitFor(() => expect(queryClient.isMutating()).toBe(0), { interval: 1 })
}

beforeEach(() => {
  captured.queries.length = 0
  captured.mutations.length = 0
  forcing.queries = []
  forcing.mutations = []
})

afterEach(() => {
  vi.unstubAllGlobals()
})

// -- Captured options --

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

// -- Rows: every state, from the props --

// A button a row offers in a state, and what a click on it does.
type Offer = {
  label: string
  disabled?: boolean
  // A toggle's aria-expanded.
  expanded?: boolean
  // The callbacks a click calls, in order; none if omitted.
  calls?: readonly string[]
  // The row's own state after the click.
  then: string
}

type RowModel = {
  component: string
  // The row component itself, whose own state the walk records.
  type: unknown
  // Every prop that changes what the row renders, with every value it can take.
  axes: Readonly<Record<string, readonly unknown[]>>
  // The row's own states, and the one it starts in.
  states: readonly string[]
  initial: string
  // The own state the row is in, handed props, when it was in state: a
  // state the props close gives way to another with no click. Omitted,
  // every state stays under every props.
  settles?: (props: RowProps, state: string) => string
  // Each prop that says a request is in flight, and the callback that sends it.
  inFlight: Readonly<Record<string, string>>
  row: (props: RowProps, log: string[]) => ReactNode
  opaque?: ReadonlyMap<unknown, string>
  offers: (props: RowProps, state: string) => readonly Offer[]
  // Anything else a state must render, as a problem, or undefined.
  shows?: (tree: Resolved[], props: RowProps, state: string) => string | undefined
}

// MemberRow's drawer is MemberConnectedApps, whose hooks appear only once
// the drawer opens: it stays uncalled, visible as a host of its own name.
const DRAWER = new Map<unknown, string>([[MemberConnectedApps, 'MemberConnectedApps']])

const ROW_MODELS: readonly RowModel[] = [
  {
    component: 'MCPClientRow',
    type: MCPClientRow,
    axes: { kind: CLIENT_KINDS, disabled: [false, true], deleting: [false, true], settingDisabled: [false, true] },
    states: ['no confirmation', 'confirming disable', 'confirming delete'],
    initial: 'no confirmation',
    // The disable confirmation is done once the list shows the client
    // disabled: it closes, and stays closed if the list later shows the
    // client enabled -- by anyone, with no click here.
    settles: (p, state) => (state === 'confirming disable' && p.disabled === true ? 'no confirmation' : state),
    inFlight: { deleting: 'onDelete', settingDisabled: 'onSetDisabled' },
    row: (p, log) => (
      <MCPClientRow
        client={client({ kind: p.kind as MCPClient['kind'], clientId: CLIENT_ID_OF_KIND[p.kind as MCPClient['kind']], disabledAt: p.disabled === true ? DISABLED_AT : null })}
        deleting={p.deleting === true}
        settingDisabled={p.settingDisabled === true}
        onDelete={() => log.push('onDelete')}
        onSetDisabled={(disabled) => log.push(`onSetDisabled(${String(disabled)})`)}
      />
    ),
    offers: (p, state) => {
      if (state === 'confirming delete') {
        return [
          { label: p.deleting === true ? 'Deleting…' : 'Confirm delete', disabled: p.deleting === true, calls: ['onDelete'], then: 'confirming delete' },
          { label: 'Cancel', then: 'no confirmation' },
        ]
      }
      if (state === 'confirming disable') {
        return [
          { label: p.settingDisabled === true ? 'Disabling…' : 'Confirm disable', disabled: p.settingDisabled === true, calls: ['onSetDisabled(true)'], then: 'confirming disable' },
          { label: 'Cancel', then: 'no confirmation' },
        ]
      }
      // No confirmation open.
      if (p.disabled === true) {
        return [
          { label: p.settingDisabled === true ? 'Enabling…' : 'Enable', disabled: p.settingDisabled === true, calls: ['onSetDisabled(false)'], then: 'no confirmation' },
          { label: 'Delete', then: 'confirming delete' },
        ]
      }
      return [
        { label: 'Disable', then: 'confirming disable' },
        { label: 'Delete', then: 'confirming delete' },
      ]
    },
  },
  {
    component: 'ConnectedAppRow',
    type: ConnectedAppRow,
    axes: { kind: AUTHORIZATION_KINDS, used: [false, true], revoking: [false, true] },
    states: ['no confirmation', 'confirming revoke'],
    initial: 'no confirmation',
    inFlight: { revoking: 'onRevoke' },
    row: (p, log) => (
      <ConnectedAppRow
        authorization={authorization({
          clientKind: p.kind as MCPAuthorization['clientKind'],
          clientId: CLIENT_ID_OF_KIND[p.kind as MCPAuthorization['clientKind']],
          lastUsedAt: p.used === true ? '2026-09-21T08:30:00Z' : null,
        })}
        revoking={p.revoking === true}
        onRevoke={() => log.push('onRevoke')}
      />
    ),
    offers: (p, state) =>
      state === 'confirming revoke'
        ? [
            { label: p.revoking === true ? 'Revoking…' : 'Confirm revoke', disabled: p.revoking === true, calls: ['onRevoke'], then: 'confirming revoke' },
            { label: 'Cancel', then: 'no confirmation' },
          ]
        : [{ label: 'Revoke', then: 'confirming revoke' }],
  },
  {
    component: 'MemberRow',
    type: MemberRow,
    axes: { canManage: [true, false], role: MEMBER_ROLES, disabled: [false, true], identity: LINKED_IDENTITIES },
    states: ['drawer closed', 'drawer open'],
    initial: 'drawer closed',
    inFlight: {},
    row: (p, log) => (
      <MemberRow
        member={member({ role: p.role as Member['role'], disabled: p.disabled === true, identities: p.identity === null ? [] : [{ ...IDENTITY, ...(p.identity as LinkedIdentity) }] })}
        canManage={p.canManage === true}
        onShowAudit={(id) => log.push(`onShowAudit(${id})`)}
      />
    ),
    opaque: DRAWER,
    offers: (p, state) => [
      ...(p.canManage === true ? [{ label: 'Connected apps', expanded: state === 'drawer open', then: state === 'drawer open' ? 'drawer closed' : 'drawer open' }] : []),
      { label: 'Audit log', calls: [`onShowAudit(${member().id})`], then: state },
    ],
    shows: (tree, p, state) => {
      const drawers = hostsIn(tree, 'MemberConnectedApps').map((d) => d.props.member?.id)
      const want = p.canManage === true && state === 'drawer open' ? [member().id] : []
      return JSON.stringify(drawers) === JSON.stringify(want) ? undefined : `shows the drawer of [${drawers.join(', ')}], want [${want.join(', ')}]`
    },
  },
]

// IN_FLIGHT_LABELS is each button that sends a request, and what it says
// while that request is in flight.
const IN_FLIGHT_LABELS: Readonly<Record<string, string>> = {
  'Confirm delete': 'Deleting…',
  'Confirm disable': 'Disabling…',
  Enable: 'Enabling…',
  'Confirm revoke': 'Revoking…',
}

// How many clicks on the walk checks from every state it reaches.
const CLICK_DEPTH = 3

// RowHarness stands in for the section around a row: it logs each
// callback the row calls, and holds the props it hands the row in its own
// state, which a { become } step sets.
function RowHarness({ model, start, log, become }: { model: RowModel; start: RowProps; log: string[]; become: Become }) {
  const [props, setProps] = useState(start)
  become.take(setProps)
  return <>{model.row(props, log)}</>
}

// A row harness driven: the run, and every callback the row called.
type RowRun = { run: Run; log: string[] }

function runRow(model: RowModel, start: RowProps, steps: readonly Step[]): RowRun {
  const log: string[] = []
  const become = newBecome()
  const run = drive(<RowHarness model={model} start={start} log={log} become={become} />, steps, new QueryClient(), { opaque: model.opaque, become, own: { type: model.type, values: [] } })
  return { run, log }
}

// product is every combination of the axes' values.
function product(axes: RowModel['axes']): RowProps[] {
  return Object.entries(axes).reduce<RowProps[]>((all, [axis, values]) => all.flatMap((p) => values.map((v) => ({ ...p, [axis]: v }))), [{}])
}

// settle is the own state a row is in once a step has left it in state
// with props (RowModel.settles).
function settle(model: RowModel, props: RowProps, state: string): string {
  return model.settles?.(props, state) ?? state
}

// restingCombinations is every combination of the axes and own states
// the row can be in: those no props close.
function restingCombinations(model: RowModel): string[] {
  return product(model.axes).flatMap((p) => model.states.filter((s) => settle(model, p, s) === s).map((s) => JSON.stringify([p, s])))
}

// A state the walk reached: the props it started the harness with, the
// steps from there, and the props and the model's own state they lead to.
type RowNode = { start: RowProps; steps: readonly Step[]; props: RowProps; state: string }

function describeValue(axis: string, v: unknown): string {
  if (v === true) return axis
  if (v === false) return `not ${axis}`
  if (v === null) return `no ${axis}`
  if (typeof v === 'object') return `${axis} {${Object.entries(v).map(([field, x]) => `${field} ${String(x)}`).join(', ')}}`
  return `${axis} ${String(v)}`
}

function describeProps(props: RowProps): string {
  return Object.entries(props)
    .map(([axis, v]) => describeValue(axis, v))
    .join(', ')
}

function describeNode(model: RowModel, node: { start: RowProps; steps: readonly Step[] }): string {
  let current = node.start
  const steps = node.steps.map((s) => {
    if (typeof s !== 'string' && 'become' in s) {
      const changed = Object.fromEntries(Object.entries(s.become).filter(([axis, v]) => current[axis] !== v))
      current = s.become
      return `(${describeProps(changed)})`
    }
    return typeof s === 'string' ? s : stepName(s)
  })
  return `${model.component} {${describeProps(node.start)}}${steps.length === 0 ? '' : `: ${steps.join(', then ')}`}`
}

// The callbacks a step must call: the row's log from before onward.
type Calls = { before: number; want: readonly string[] }

// conform checks node, rendered, and, up to depth clicks on, every state
// its clicks reach, against the model: what the step that reached it
// called, the buttons offered, and anything else the state shows. It says
// whether node offers the model's buttons, so that its clicks can be
// followed.
function conform(model: RowModel, node: RowNode, rendered: RowRun, calls: Calls, depth: number, problems: string[]): boolean {
  const { run, log } = rendered
  const where = describeNode(model, node)
  const called = log.slice(calls.before)
  if (JSON.stringify(called) !== JSON.stringify(calls.want)) problems.push(`${where}: the last step called [${called.join(', ')}], want [${calls.want.join(', ')}]`)
  const offers = model.offers(node.props, node.state)
  const got = hostsIn(run.last, 'button').map(offered)
  const want = offers.map((o) => `${o.label}${o.disabled === true ? ' (disabled)' : ''}${o.expanded === undefined ? '' : o.expanded ? ' (expanded)' : ' (collapsed)'}`)
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    problems.push(`${where}: offers [${got.join(', ')}], want [${want.join(', ')}]`)
    return false
  }
  const shown = model.shows?.(run.last, node.props, node.state)
  if (shown !== undefined) problems.push(`${where}: ${shown}`)
  if (depth === 0) return true
  for (const offer of offers) {
    if (offer.disabled === true) continue
    const steps = [...node.steps, offer.label]
    conform(model, { ...node, steps, state: settle(model, node.props, offer.then) }, runRow(model, node.start, steps), { before: log.length, want: offer.calls ?? [] }, depth - 1, problems)
  }
  return true
}

// walkRow reaches every combination of the model's axes and own states
// the row can be in: from every combination of the axes, each own state
// through the row's own clicks, and each combination by changing one prop
// at a time -- each step leaving the model's own state where the model
// says it settles (RowModel.settles). Two
// states are the same only if the props, the model's own state AND the
// row's own React state (ResolveOptions.own) are: a state that renders
// like another, but holds something else, is followed on by every click
// and every prop change as a state of its own. It conforms every state it
// reaches, with what the step that reached it must call: a click, the
// model's callbacks; a first render or a prop change, none.
function walkRow(model: RowModel): { reached: RowNode[]; problems: string[] } {
  const problems: string[] = []
  const reached: RowNode[] = []
  const seen = new Set<string>()
  const combinations = product(model.axes).length * model.states.length
  const queue: { node: RowNode; calls: Calls }[] = product(model.axes).map((p) => ({ node: { start: p, steps: [], props: p, state: model.initial }, calls: { before: 0, want: [] } }))
  for (const { node, calls } of queue) {
    const rendered = runRow(model, node.start, node.steps)
    const key = JSON.stringify([node.props, node.state, rendered.run.own])
    if (seen.has(key)) continue
    seen.add(key)
    reached.push(node)
    // A row whose own state kept taking new values would never let the
    // walk end.
    if (reached.length > 4 * combinations) throw new Error(`${model.component}: the walk reached more than ${4 * combinations} states, four for each combination of the axes and own states: does the row's own state grow without bound?`)
    const before = rendered.log.length
    if (conform(model, node, rendered, calls, CLICK_DEPTH, problems)) {
      for (const offer of model.offers(node.props, node.state)) {
        if (offer.disabled !== true) queue.push({ node: { ...node, steps: [...node.steps, offer.label], state: settle(model, node.props, offer.then) }, calls: { before, want: offer.calls ?? [] } })
      }
    }
    for (const [axis, values] of Object.entries(model.axes)) {
      for (const v of values) {
        if (v === node.props[axis]) continue
        const props = { ...node.props, [axis]: v }
        queue.push({ node: { ...node, steps: [...node.steps, { become: props }], props, state: settle(model, props, node.state) }, calls: { before, want: [] } })
      }
    }
  }
  return { reached, problems }
}

const walks = new Map<RowModel, ReturnType<typeof walkRow>>()
function walked(model: RowModel): ReturnType<typeof walkRow> {
  const cached = walks.get(model)
  if (cached !== undefined) return cached
  const walk = walkRow(model)
  walks.set(model, walk)
  return walk
}

// sends says whether calls include callback, with or without arguments.
function sends(calls: readonly string[], callback: string): boolean {
  return calls.some((c) => c === callback || c.startsWith(`${callback}(`))
}

// inFlightProblems checks, in every combination the walk reached, what
// each in-flight prop does to the row's buttons.
function inFlightProblems(model: RowModel, reached: readonly RowNode[]): string[] {
  const problems: string[] = []
  const guarded = new Set<string>()
  const inFlightLabels = new Set(Object.values(IN_FLIGHT_LABELS))
  for (const node of reached) {
    const where = describeNode(model, node)
    const { run, log } = runRow(model, node.start, node.steps)
    const buttons = hostsIn(run.last, 'button')
    const flags = Object.entries(model.inFlight).filter(([flag]) => node.props[flag] === true)
    if (flags.length === 0) {
      for (const b of buttons) if (inFlightLabels.has(labelOf(b))) problems.push(`${where}: nothing is in flight, yet it offers ${offered(b)}`)
      continue
    }
    for (const [flag, callback] of flags) {
      // The same state with this request not in flight: its buttons that
      // send it are the ones that must now say so and be locked.
      const twinSteps = [...node.steps, { become: { ...node.props, [flag]: false } }]
      const twin = runRow(model, node.start, twinSteps)
      const twinButtons = hostsIn(twin.run.last, 'button')
      if (twinButtons.length !== buttons.length) {
        problems.push(`${where}: offers [${buttons.map(offered).join(', ')}], and [${twinButtons.map(offered).join(', ')}] once not ${flag}`)
        continue
      }
      twinButtons.forEach((sender, i) => {
        if (sender.props.disabled === true) return
        const click = runRow(model, node.start, [...twinSteps, labelOf(sender)])
        if (!sends(click.log.slice(twin.log.length), callback)) return
        guarded.add(flag)
        const shown = buttons[i]
        const label = IN_FLIGHT_LABELS[labelOf(sender)]
        if (label === undefined) problems.push(`${where}: ${labelOf(sender)} calls ${callback}, and has no in-flight label`)
        else if (shown === undefined || offered(shown) !== `${label} (disabled)`) problems.push(`${where}: the button that calls ${callback} offers ${shown === undefined ? 'nothing' : offered(shown)} while ${flag}, want ${label} (disabled)`)
      })
      for (const b of buttons) {
        if (b.props.disabled === true) continue
        const click = runRow(model, node.start, [...node.steps, labelOf(b)])
        if (sends(click.log.slice(log.length), callback)) problems.push(`${where}: ${labelOf(b)} calls ${callback} again while ${flag}`)
      }
    }
  }
  for (const [flag, callback] of Object.entries(model.inFlight)) {
    if (!guarded.has(flag)) problems.push(`${model.component}: no state offers a button that calls ${callback}, so ${flag} guards nothing`)
  }
  return problems
}

// firstProblems keeps a failure readable when one defect shows in many states.
function firstProblems(problems: readonly string[]): string[] {
  const unique = [...new Set(problems)]
  return unique.length <= 25 ? unique : [...unique.slice(0, 25), `... and ${unique.length - 25} more`]
}

describe('each row, in every combination of its props and its own state, offers what its model says, and each click does what it says', () => {
  for (const model of ROW_MODELS) {
    it(`${model.component}: every combination it can be in is reached, and every button in it, clicked, calls and leaves open what the model says`, () => {
      const { reached, problems } = walked(model)
      expect(firstProblems(problems)).toEqual([])
      const states = new Set(reached.map((n) => n.state))
      expect([...states].sort()).toEqual([...model.states].sort())
      const combinations = new Set(reached.map((n) => JSON.stringify([n.props, n.state])))
      expect([...combinations].sort()).toEqual(restingCombinations(model).sort())
    })

    it(`${model.component}: while a request is in flight, the button that sends it says so and is disabled, and no other button sends it again`, () => {
      const { reached } = walked(model)
      expect(firstProblems(inFlightProblems(model, reached))).toEqual([])
    })
  }
})

describe('MCPClientRow -- a disable that has gone through closes its confirmation for good', () => {
  const enabled: RowProps = { kind: 'preregistered', disabled: false, deleting: false, settingDisabled: false }
  // Disable, Confirm disable, the request in flight, then the list showing
  // the client disabled, the request done.
  const disabled: readonly Step[] = ['Disable', 'Confirm disable', { become: { ...enabled, settingDisabled: true } }, { become: { ...enabled, disabled: true } }]

  function clientRow(steps: readonly Step[]): RowRun & { offers: string[]; text: string } {
    const model = ROW_MODELS.find((m) => m.component === 'MCPClientRow')
    if (model === undefined) throw new Error('no MCPClientRow model')
    const ran = runRow(model, enabled, steps)
    expect(ran.run.applied).toEqual(steps.map(stepName))
    return { ...ran, offers: hostsIn(ran.run.last, 'button').map(offered), text: ran.run.last.map(textOf).join('') }
  }

  it('once the list shows the client disabled, the row offers Enable and Delete, and Enable, clicked in that very render, sends the enable', () => {
    const { offers, log } = clientRow(disabled)
    expect(offers).toEqual(['Enable', 'Delete'])
    expect(log).toEqual(['onSetDisabled(true)'])
    expect(clientRow([...disabled, 'Enable']).log).toEqual(['onSetDisabled(true)', 'onSetDisabled(false)'])
  })

  it('when the list then shows the client enabled -- by anyone else, with no click here -- the row offers Disable and Delete, no confirmation, and sends nothing more', () => {
    const { offers, text, log } = clientRow([...disabled, { become: enabled }])
    expect(offers).toEqual(['Disable', 'Delete'])
    expect(text).not.toContain('Disabling this client')
    expect(log).toEqual(['onSetDisabled(true)'])
  })
})

// -- Section flows: each callback a section hands a row, down to fetch --

// Every list's two rows are lookalikes: one name, 'Editor Plugin' -- a
// name the app chooses itself, so never an identity -- and different ids.
// A handler that finds its row by name finds the first in both.

// Two apps identified by their description's address: the real one, and a
// lookalike on another host. Their rows differ only in that host.
const REAL_HOST = 'tools.example'
const LOOKALIKE_HOST = 'lookalike.example'
const TWO_AUTHORIZATIONS = [
  authorization({ clientKind: 'metadata_document', clientId: `https://${REAL_HOST}/mcp/client.json` }),
  authorization({ id: 'auth/2', clientKind: 'metadata_document', clientId: `https://${LOOKALIKE_HOST}/mcp/client.json` }),
]

// Two clients, each row showing its own client ID: the first disabled, the
// second enabled.
const TWO_CLIENTS = [client({ disabledAt: DISABLED_AT }), client({ id: 'client/2', clientId: 'narvi_mcp_c_two' })]

// The same two clients, the first enabled and the second disabled: so that
// Disable and Enable are each clicked in both rows.
const TWO_CLIENTS_SWAPPED = [client(), client({ id: 'client/2', clientId: 'narvi_mcp_c_two', disabledAt: DISABLED_AT })]

// The same two clients, both enabled, then both disabled: so that Disable
// and Enable are each clicked in the second of two rows alike in that too.
const TWO_CLIENTS_ENABLED = [client(), client({ id: 'client/2', clientId: 'narvi_mcp_c_two' })]
const TWO_CLIENTS_DISABLED = [client({ disabledAt: DISABLED_AT }), client({ id: 'client/2', clientId: 'narvi_mcp_c_two', disabledAt: DISABLED_AT })]

const OTHER_MEMBER_ID = 'user/2'

// Every cached result on the screens these sections render on, by name: a
// flow asserts which it invalidated.
const SEEDED = {
  'the MCP clients list': mcpClientQueryKeys.list(),
  'your own connected apps': mcpAuthorizationQueryKeys.mine(),
  "this member's connected apps": mcpAuthorizationQueryKeys.member(member().id),
  "another member's connected apps": mcpAuthorizationQueryKeys.member(OTHER_MEMBER_ID),
  'an audit log page': auditLogQueryKeys.page(50, 0),
  'the members list': memberQueryKeys.list(),
} as const satisfies Record<string, QueryKey>

type SeededName = keyof typeof SEEDED

// A mutation of a section, in the order it declares them, with what the
// section's own code hands it and gets back, and the notice the section
// shows while it has failed.
type SectionMutation = { name: string; variables: unknown; data: unknown; notice: string }

type SectionStart = {
  component: string
  ui: () => ReactNode
  // The section's own list: its key, and its data with the two rows or none.
  list: QueryKey
  data: (rows: boolean) => unknown
  mutations: readonly SectionMutation[]
}

const SECTION_STARTS = {
  'the MCP clients section': {
    component: 'MCPClientsSection',
    ui: () => <MCPClientsSection />,
    list: mcpClientQueryKeys.list(),
    data: (rows) => ({ clients: rows ? TWO_CLIENTS : [] }),
    mutations: [
      { name: 'register', variables: undefined, data: client({ id: 'client/3', clientId: 'narvi_mcp_c_new', clientName: 'New App' }), notice: "Couldn't register the client." },
      { name: 'delete', variables: 'client/2', data: undefined, notice: "Couldn't delete the client." },
      { name: 'disable/enable', variables: { clientId: 'client/2', disabled: true }, data: client({ id: 'client/2', disabledAt: DISABLED_AT }), notice: "Couldn't change whether the client is disabled." },
    ],
  },
  "a member's drawer": {
    component: 'MemberConnectedApps',
    ui: () => <MemberConnectedApps member={member()} />,
    list: mcpAuthorizationQueryKeys.member(member().id),
    data: (rows) => ({ authorizations: rows ? TWO_AUTHORIZATIONS : [] }),
    mutations: [{ name: 'revoke', variables: 'auth/2', data: undefined, notice: "Couldn't revoke. Try again." }],
  },
  'your own connected apps': {
    component: 'ConnectedAppsSection',
    ui: () => <ConnectedAppsSection />,
    list: mcpAuthorizationQueryKeys.mine(),
    data: (rows) => ({ authorizations: rows ? TWO_AUTHORIZATIONS : [] }),
    mutations: [{ name: 'revoke', variables: 'auth/2', data: undefined, notice: "Couldn't revoke. Try again." }],
  },
} satisfies Record<string, SectionStart>

type SectionStartName = keyof typeof SECTION_STARTS

// The register form, filled: surrounding blanks and a repeated redirect URI
// are the section's to drop.
const FILL: Readonly<Record<string, string>> = {
  'App name': ' Editor Plugin ',
  'Redirect URIs, one per line': 'http://127.0.0.1/callback\n https://client.example/cb \nhttp://127.0.0.1/callback',
  'Homepage (optional, https)': ' https://client.example ',
}

function typeInto(field: string): Step {
  const value = FILL[field]
  if (value === undefined) throw new Error(`no value to type into "${field}": add one to FILL`)
  return { field, type: value }
}

const REGISTER_FORM: readonly Step[] = ['App name', 'Redirect URIs, one per line', 'Homepage (optional, https)'].map(typeInto)

type SectionFlow = {
  start: SectionStartName
  // The section's list, when not its start's two rows: what the test's
  // name calls it, and its data -- the same rows, in the same order.
  list?: { name: string; data: unknown }
  steps: readonly Step[]
  // The section mutation the last step starts, by name, if it sends.
  mutation?: string
  // The render right after the last step: each row's buttons, by a text
  // that finds it, and the section's own, outside its rows.
  after: { rows: Readonly<Record<string, readonly string[]>>; outside: readonly string[] }
  // Every request sent, in order.
  sends: readonly Call[]
  // Every row callback the clicks called, in order.
  calls: readonly string[]
  // Every cached result the requests' completion invalidated.
  invalidates: readonly SeededName[]
}

const CLIENTS_SWAPPED = { name: 'its first client enabled and its second disabled', data: { clients: TWO_CLIENTS_SWAPPED } }
const CLIENTS_ENABLED = { name: 'both its clients enabled', data: { clients: TWO_CLIENTS_ENABLED } }
const CLIENTS_DISABLED = { name: 'both its clients disabled', data: { clients: TWO_CLIENTS_DISABLED } }

// Every call a section's rows make is made in each of its two rows (the
// last test below says so by name), over rows of one name: Confirm delete,
// Confirm disable, Enable and both Confirm revokes each send the id of the
// row clicked in, whether it is the first or the last, and whether or not
// another row has its name -- or, for Confirm disable and Enable, its
// state.
const SECTION_FLOWS: readonly SectionFlow[] = [
  {
    start: 'the MCP clients section',
    steps: [
      { row: 'narvi_mcp_c_two', click: 'Disable' },
      { row: 'narvi_mcp_c_two', click: 'Confirm disable' },
    ],
    mutation: 'disable/enable',
    after: { rows: { narvi_mcp_c_one: ['Enable', 'Delete'], narvi_mcp_c_two: ['Disabling… (disabled)', 'Cancel'] }, outside: ['Register client (disabled)'] },
    sends: [{ url: '/api/mcp-clients/client%2F2/disable', method: 'POST' }],
    calls: ['MCPClientRow.onSetDisabled'],
    invalidates: ['the MCP clients list'],
  },
  {
    start: 'the MCP clients section',
    list: CLIENTS_SWAPPED,
    steps: [
      { row: 'narvi_mcp_c_one', click: 'Disable' },
      { row: 'narvi_mcp_c_one', click: 'Confirm disable' },
    ],
    mutation: 'disable/enable',
    after: { rows: { narvi_mcp_c_one: ['Disabling… (disabled)', 'Cancel'], narvi_mcp_c_two: ['Enable', 'Delete'] }, outside: ['Register client (disabled)'] },
    sends: [{ url: '/api/mcp-clients/client%2F1/disable', method: 'POST' }],
    calls: ['MCPClientRow.onSetDisabled'],
    invalidates: ['the MCP clients list'],
  },
  {
    start: 'the MCP clients section',
    steps: [{ row: 'narvi_mcp_c_two', click: 'Disable' }],
    after: { rows: { narvi_mcp_c_one: ['Enable', 'Delete'], narvi_mcp_c_two: ['Confirm disable', 'Cancel'] }, outside: ['Register client (disabled)'] },
    sends: [],
    calls: [],
    invalidates: [],
  },
  {
    start: 'the MCP clients section',
    steps: [{ row: 'narvi_mcp_c_one', click: 'Enable' }],
    mutation: 'disable/enable',
    after: { rows: { narvi_mcp_c_one: ['Enabling… (disabled)', 'Delete'], narvi_mcp_c_two: ['Disable', 'Delete'] }, outside: ['Register client (disabled)'] },
    sends: [{ url: '/api/mcp-clients/client%2F1/enable', method: 'POST' }],
    calls: ['MCPClientRow.onSetDisabled'],
    invalidates: ['the MCP clients list'],
  },
  {
    start: 'the MCP clients section',
    list: CLIENTS_SWAPPED,
    steps: [{ row: 'narvi_mcp_c_two', click: 'Enable' }],
    mutation: 'disable/enable',
    after: { rows: { narvi_mcp_c_one: ['Disable', 'Delete'], narvi_mcp_c_two: ['Enabling… (disabled)', 'Delete'] }, outside: ['Register client (disabled)'] },
    sends: [{ url: '/api/mcp-clients/client%2F2/enable', method: 'POST' }],
    calls: ['MCPClientRow.onSetDisabled'],
    invalidates: ['the MCP clients list'],
  },
  {
    start: 'the MCP clients section',
    list: CLIENTS_ENABLED,
    steps: [
      { row: 'narvi_mcp_c_two', click: 'Disable' },
      { row: 'narvi_mcp_c_two', click: 'Confirm disable' },
    ],
    mutation: 'disable/enable',
    after: { rows: { narvi_mcp_c_one: ['Disable', 'Delete'], narvi_mcp_c_two: ['Disabling… (disabled)', 'Cancel'] }, outside: ['Register client (disabled)'] },
    sends: [{ url: '/api/mcp-clients/client%2F2/disable', method: 'POST' }],
    calls: ['MCPClientRow.onSetDisabled'],
    invalidates: ['the MCP clients list'],
  },
  {
    start: 'the MCP clients section',
    list: CLIENTS_DISABLED,
    steps: [{ row: 'narvi_mcp_c_two', click: 'Enable' }],
    mutation: 'disable/enable',
    after: { rows: { narvi_mcp_c_one: ['Enable', 'Delete'], narvi_mcp_c_two: ['Enabling… (disabled)', 'Delete'] }, outside: ['Register client (disabled)'] },
    sends: [{ url: '/api/mcp-clients/client%2F2/enable', method: 'POST' }],
    calls: ['MCPClientRow.onSetDisabled'],
    invalidates: ['the MCP clients list'],
  },
  {
    start: 'the MCP clients section',
    steps: [
      { row: 'narvi_mcp_c_one', click: 'Delete' },
      { row: 'narvi_mcp_c_two', click: 'Delete' },
      { row: 'narvi_mcp_c_two', click: 'Confirm delete' },
    ],
    mutation: 'delete',
    after: { rows: { narvi_mcp_c_one: ['Confirm delete', 'Cancel'], narvi_mcp_c_two: ['Deleting… (disabled)', 'Cancel'] }, outside: ['Register client (disabled)'] },
    sends: [{ url: '/api/mcp-clients/client%2F2', method: 'DELETE' }],
    calls: ['MCPClientRow.onDelete'],
    // Deleting a client revokes every user's authorization of it.
    invalidates: ['the MCP clients list', 'your own connected apps', "this member's connected apps", "another member's connected apps"],
  },
  {
    start: 'the MCP clients section',
    steps: [
      { row: 'narvi_mcp_c_two', click: 'Delete' },
      { row: 'narvi_mcp_c_one', click: 'Delete' },
      { row: 'narvi_mcp_c_one', click: 'Confirm delete' },
    ],
    mutation: 'delete',
    after: { rows: { narvi_mcp_c_one: ['Deleting… (disabled)', 'Cancel'], narvi_mcp_c_two: ['Confirm delete', 'Cancel'] }, outside: ['Register client (disabled)'] },
    sends: [{ url: '/api/mcp-clients/client%2F1', method: 'DELETE' }],
    calls: ['MCPClientRow.onDelete'],
    invalidates: ['the MCP clients list', 'your own connected apps', "this member's connected apps", "another member's connected apps"],
  },
  {
    start: 'the MCP clients section',
    steps: [...REGISTER_FORM, 'Register client'],
    mutation: 'register',
    after: { rows: { narvi_mcp_c_one: ['Enable', 'Delete'], narvi_mcp_c_two: ['Disable', 'Delete'] }, outside: ['Registering… (disabled)'] },
    sends: [
      {
        url: '/api/mcp-clients',
        method: 'POST',
        body: { clientName: 'Editor Plugin', redirectUris: ['http://127.0.0.1/callback', 'https://client.example/cb'], clientUri: 'https://client.example' },
      },
    ],
    calls: [],
    invalidates: ['the MCP clients list'],
  },
  {
    start: 'the MCP clients section',
    steps: [...REGISTER_FORM.slice(0, 2), 'Register client'],
    mutation: 'register',
    after: { rows: { narvi_mcp_c_one: ['Enable', 'Delete'], narvi_mcp_c_two: ['Disable', 'Delete'] }, outside: ['Registering… (disabled)'] },
    sends: [{ url: '/api/mcp-clients', method: 'POST', body: { clientName: 'Editor Plugin', redirectUris: ['http://127.0.0.1/callback', 'https://client.example/cb'] } }],
    calls: [],
    invalidates: ['the MCP clients list'],
  },
  {
    start: "a member's drawer",
    steps: [
      { row: REAL_HOST, click: 'Revoke' },
      { row: LOOKALIKE_HOST, click: 'Revoke' },
      { row: LOOKALIKE_HOST, click: 'Confirm revoke' },
    ],
    mutation: 'revoke',
    after: { rows: { [REAL_HOST]: ['Confirm revoke', 'Cancel'], [LOOKALIKE_HOST]: ['Revoking… (disabled)', 'Cancel'] }, outside: [] },
    sends: [{ url: '/api/members/user%2F1%3Fx/mcp-authorizations/auth%2F2', method: 'DELETE' }],
    calls: ['ConnectedAppRow.onRevoke'],
    // Every user's list, the admin's own included, and the audit log below.
    invalidates: ['your own connected apps', "this member's connected apps", "another member's connected apps", 'an audit log page'],
  },
  {
    start: "a member's drawer",
    steps: [
      { row: LOOKALIKE_HOST, click: 'Revoke' },
      { row: REAL_HOST, click: 'Revoke' },
      { row: REAL_HOST, click: 'Confirm revoke' },
    ],
    mutation: 'revoke',
    after: { rows: { [REAL_HOST]: ['Revoking… (disabled)', 'Cancel'], [LOOKALIKE_HOST]: ['Confirm revoke', 'Cancel'] }, outside: [] },
    sends: [{ url: '/api/members/user%2F1%3Fx/mcp-authorizations/auth%2F1', method: 'DELETE' }],
    calls: ['ConnectedAppRow.onRevoke'],
    invalidates: ['your own connected apps', "this member's connected apps", "another member's connected apps", 'an audit log page'],
  },
  {
    start: 'your own connected apps',
    steps: [
      { row: REAL_HOST, click: 'Revoke' },
      { row: LOOKALIKE_HOST, click: 'Revoke' },
      { row: LOOKALIKE_HOST, click: 'Confirm revoke' },
    ],
    mutation: 'revoke',
    after: { rows: { [REAL_HOST]: ['Confirm revoke', 'Cancel'], [LOOKALIKE_HOST]: ['Revoking… (disabled)', 'Cancel'] }, outside: [] },
    sends: [{ url: '/api/me/mcp-authorizations/auth%2F2', method: 'DELETE' }],
    calls: ['ConnectedAppRow.onRevoke'],
    invalidates: ['your own connected apps'],
  },
  {
    start: 'your own connected apps',
    steps: [
      { row: LOOKALIKE_HOST, click: 'Revoke' },
      { row: REAL_HOST, click: 'Revoke' },
      { row: REAL_HOST, click: 'Confirm revoke' },
    ],
    mutation: 'revoke',
    after: { rows: { [REAL_HOST]: ['Revoking… (disabled)', 'Cancel'], [LOOKALIKE_HOST]: ['Confirm revoke', 'Cancel'] }, outside: [] },
    sends: [{ url: '/api/me/mcp-authorizations/auth%2F1', method: 'DELETE' }],
    calls: ['ConnectedAppRow.onRevoke'],
    invalidates: ['your own connected apps'],
  },
]

// seeded is a query client holding every SEEDED result, and the section's
// own list with its two rows -- as list has them, if given -- or none, or
// -- rows undefined -- nothing.
function seeded(start: SectionStart, rows: boolean | undefined, list: SectionFlow['list']): QueryClient {
  const queryClient = new QueryClient()
  for (const key of Object.values(SEEDED)) queryClient.setQueryData(key, { seeded: true })
  if (rows === undefined) queryClient.removeQueries({ queryKey: start.list, exact: true })
  else queryClient.setQueryData(start.list, rows && list !== undefined ? list.data : start.data(rows))
  return queryClient
}

// runSection drives start's section through steps, over a query client
// seeded as seeded() says: rows is never defaulted, since an undefined one
// -- no list cached, a list still loading -- must stay undefined.
function runSection(start: SectionStartName, steps: readonly Step[], wiring: Wiring, rows: boolean | undefined, list?: SectionFlow['list']): { run: Run; queryClient: QueryClient } {
  const section = SECTION_STARTS[start] as SectionStart
  const queryClient = seeded(section, rows, list)
  return { run: drive(section.ui(), steps, queryClient, { wiring }), queryClient }
}

// invalidated is every SEEDED result queryClient holds invalidated.
function invalidated(queryClient: QueryClient): SeededName[] {
  return (Object.entries(SEEDED) as [SeededName, QueryKey][]).filter(([, key]) => queryClient.getQueryState(key)?.isInvalidated === true).map(([name]) => name)
}

// offeredIn is what a render offers: each row's buttons, the row found by
// the one of names its text shows, and the buttons outside the rows.
function offeredIn(tree: Resolved[], names: readonly string[]): SectionFlow['after'] {
  const rows: Record<string, string[]> = {}
  for (const row of rowsIn(tree)) {
    const shown = names.filter((n) => textOf(row).includes(n))
    const name = shown.length === 1 ? shown[0] : `a row showing ${shown.length === 0 ? 'none' : shown.join(' and ')} of ${names.join(', ')}`
    rows[name ?? ''] = hostsIn([row], 'button').map(offered)
  }
  return { rows, outside: buttonsOutsideRows(tree).map(offered) }
}

// noticesIn is the text of each notice a render shows.
function noticesIn(tree: Resolved[]): string[] {
  const notices = (nodes: Resolved[]): Host[] => nodes.flatMap((n) => (typeof n === 'string' ? [] : [...(n.props.className === 'sidebar-notice' ? [n] : []), ...notices(n.children)]))
  return notices(tree).map(textOf)
}

// flowSteps names a flow: its section, its list, and its steps.
function flowSteps(flow: SectionFlow): string {
  return `${SECTION_STARTS[flow.start].component}, ${flow.start}${flow.list === undefined ? '' : `, over ${flow.list.name}`}: ${flow.steps.map(stepName).join(', then ')}`
}

function sectionFlowName(flow: SectionFlow): string {
  const sends = flow.sends.length === 0 ? 'sends nothing' : `sends ${flow.sends.map((c) => `${c.method} ${c.url}`).join(', ')}`
  const invalidates = flow.invalidates.length === 0 ? '' : `, locked while it is in flight, then invalidates ${flow.invalidates.join(', ')}`
  return `${flowSteps(flow)} ${sends}${invalidates}`
}

describe('each section, clicked through, sends exactly its own request, locks only the row that sent it, and invalidates exactly what it changed', () => {
  for (const flow of SECTION_FLOWS) {
    it(sectionFlowName(flow), async () => {
      const calls = stubFetch()
      const wiring = newWiring()
      const { run, queryClient } = runSection(flow.start, flow.steps, wiring, true, flow.list)
      expect(run.applied).toEqual(flow.steps.map(stepName))
      expect(offeredIn(run.last, Object.keys(flow.after.rows))).toEqual(flow.after)

      await settled(queryClient)
      expect(wiring.called).toEqual(flow.calls)
      expect(calls).toEqual(flow.sends)
      expect(invalidated(queryClient)).toEqual(flow.invalidates)
    })
  }
})

// -- Section flows: once the request has finished --

// SENT_BY is each in-flight button, locked, by the button that sent its
// request.
const SENT_BY = new Map(Object.entries(IN_FLIGHT_LABELS).map(([button, inFlight]) => [`${inFlight} (disabled)`, button]))

// finishedRows is what a flow's rows offer once its request has finished,
// however it ended: what they offered while it was in flight, with the
// button that sent it back to its own label, and enabled.
function finishedRows(rows: SectionFlow['after']['rows']): SectionFlow['after']['rows'] {
  return Object.fromEntries(Object.entries(rows).map(([name, buttons]) => [name, buttons.map((b) => SENT_BY.get(b) ?? b)]))
}

const REQUEST_OUTCOMES = ['pending', 'failed', 'succeeded'] as const

describe('each section, its request pending, failed or succeeded, locks the row that sent it only while it is pending, and shows its failure notice only while it has failed', () => {
  for (const flow of SECTION_FLOWS) {
    if (flow.sends.length === 0) continue
    for (const outcome of REQUEST_OUTCOMES) {
      it(`${flowSteps(flow)}, the request ${outcome}`, async () => {
        const section = SECTION_STARTS[flow.start] as SectionStart
        const index = section.mutations.findIndex((m) => m.name === flow.mutation)
        const mutation = section.mutations[index]
        if (mutation === undefined) throw new Error(`the flow sends a request, and names no mutation of ${flow.start}: ${String(flow.mutation)}`)

        // The request the flow's last click sends, as it sent it.
        stubFetch()
        const sent = runSection(flow.start, flow.steps, newWiring(), true, flow.list)
        await settled(sent.queryClient)
        const variables = sent.queryClient
          .getMutationCache()
          .getAll()
          .map((m) => m.state.variables)
        expect(variables).toHaveLength(1)

        // The same flow, the render after its last click reporting that
        // request as outcome: the one render a section shows it in (see
        // # Sections above).
        forcing.mutations = section.mutations.map((m, i) => (i === index ? forcedMutation({ ...m, variables: variables[0] }, outcome) : undefined))
        const { run, queryClient } = runSection(flow.start, flow.steps, newWiring(), true, flow.list)
        forcing.mutations = []
        expect(offeredIn(run.last, Object.keys(flow.after.rows)).rows).toEqual(outcome === 'pending' ? flow.after.rows : finishedRows(flow.after.rows))
        expect(noticesIn(run.last)).toEqual(outcome === 'failed' ? [mutation.notice] : [])
        await settled(queryClient)
      })
    }
  }
})

// -- No section control without a flow --

// A state of a section's list, and of each of its mutations.
const LIST_STATES = ['loading', 'failed', 'refused', 'empty', 'listed'] as const
const MUTATION_STATES = ['idle', 'pending', 'failed', 'succeeded'] as const
type ListState = (typeof LIST_STATES)[number]
type MutationState = (typeof MUTATION_STATES)[number]

type SectionState = { list: ListState; mutations: Readonly<Record<string, MutationState>>; filled: readonly string[] }

// SECTION_CONTROLS is every button a section offers outside its rows, in
// a state of its list, its mutations and its form.
const SECTION_CONTROLS: Readonly<Record<SectionStartName, (state: SectionState) => readonly string[]>> = {
  'the MCP clients section': (s) => {
    if (s.list === 'refused') return []
    const registering = s.mutations.register === 'pending'
    const complete = s.filled.includes('App name') && s.filled.includes('Redirect URIs, one per line')
    return [`${registering ? 'Registering…' : 'Register client'}${registering || !complete ? ' (disabled)' : ''}`]
  },
  "a member's drawer": () => [],
  'your own connected apps': () => [],
}

// The list's query result a state reports: loading and the two successes
// are the real ones, from the cache; a failure is reported.
function forcedList(list: ListState): object | undefined {
  if (list !== 'failed' && list !== 'refused') return undefined
  const error = list === 'refused' ? new ApiError(403, 'forbidden', null) : new ApiError(500, 'internal error', null)
  return { status: 'error', fetchStatus: 'idle', isPending: false, isLoading: false, isError: true, isLoadingError: true, isSuccess: false, data: undefined, error }
}

// The mutation result a state reports: idle is the real one; pending,
// failed and succeeded are reported.
function forcedMutation(mutation: SectionMutation, state: MutationState): object | undefined {
  const flags = { isIdle: false, isPending: state === 'pending', isError: state === 'failed', isSuccess: state === 'succeeded' }
  if (state === 'pending') return { ...flags, status: 'pending', variables: mutation.variables }
  if (state === 'failed') return { ...flags, status: 'error', variables: mutation.variables, error: new ApiError(409, 'refused', null) }
  if (state === 'succeeded') return { ...flags, status: 'success', variables: mutation.variables, data: mutation.data }
  return undefined
}

// subsets is every subset of values, each in values' order.
function subsets<T>(values: readonly T[]): T[][] {
  return values.reduce<T[][]>((all, v) => [...all, ...all.map((s) => [...s, v])], [[]])
}

// formStates is every state of a section's form: each field it renders,
// over its listed rows, empty or filled.
function formStates(start: SectionStartName): string[][] {
  stubFetch()
  const { run } = runSection(start, [], newWiring(), true)
  const fields = fieldsIn(run.last).map((f) => f.props.placeholder ?? '(a field with no placeholder)')
  return subsets(fields)
}

function describeSectionState(start: SectionStartName, state: SectionState): string {
  const mutations = Object.entries(state.mutations)
    .map(([name, s]) => `${name} ${s}`)
    .join(', ')
  return `${SECTION_STARTS[start].component}, list ${state.list}${mutations === '' ? '' : `, ${mutations}`}, ${state.filled.length === 0 ? 'form empty' : `filled ${state.filled.join(' + ')}`}`
}

describe('no section control without a flow', () => {
  it('every callback a section hands a row is called by one of that section\'s SECTION_FLOWS, and every call one of them makes in a row -- the callback and its arguments -- one of them makes in each row of the list', async () => {
    const missing: string[] = []
    for (const start of Object.keys(SECTION_STARTS) as SectionStartName[]) {
      stubFetch()
      const listed = newWiring()
      const { run } = runSection(start, [], listed, true)
      expect([...listed.handed], `${start} hands its rows no callback`).not.toEqual([])
      const rows = rowsIn(run.last).length
      const called = new Set<string>()
      const calledIn = new Set<string>()
      for (const flow of SECTION_FLOWS.filter((f) => f.start === start)) {
        const wiring = newWiring()
        const { queryClient } = runSection(start, flow.steps, wiring, true, flow.list)
        await settled(queryClient)
        for (const c of wiring.called) called.add(c)
        for (const c of wiring.calledIn) calledIn.add(c)
      }
      for (const callback of listed.handed) if (!called.has(callback)) missing.push(`${start}: no flow calls ${callback}`)
      for (const call of new Set([...calledIn].map((c) => c.replace(/ in row \d+$/, '')))) {
        for (let row = 1; row <= rows; row++) if (!calledIn.has(`${call} in row ${row}`)) missing.push(`${start}: no flow calls ${call} in row ${row}`)
      }
    }
    expect(missing).toEqual([])
  })

  it('in every state of its list, its mutations and its form, the buttons a section renders outside its rows are exactly SECTION_CONTROLS\'', () => {
    stubFetch()
    const problems: string[] = []
    for (const start of Object.keys(SECTION_STARTS) as SectionStartName[]) {
      const section = SECTION_STARTS[start] as SectionStart
      const mutationStates = section.mutations.reduce<Record<string, MutationState>[]>((all, m) => all.flatMap((s) => MUTATION_STATES.map((state) => ({ ...s, [m.name]: state }))), [{}])
      for (const filled of formStates(start)) {
        for (const list of LIST_STATES) {
          for (const mutations of mutationStates) {
            const state: SectionState = { list, mutations, filled }
            forcing.queries = [forcedList(list)]
            forcing.mutations = section.mutations.map((m) => forcedMutation(m, mutations[m.name] ?? 'idle'))
            const rows = list === 'listed' ? true : list === 'empty' ? false : undefined
            const { run } = runSection(start, filled.map(typeInto), newWiring(), rows)
            const got = buttonsOutsideRows(run.last).map(offered)
            const want = SECTION_CONTROLS[start](state)
            if (JSON.stringify(got) !== JSON.stringify(want)) problems.push(`${describeSectionState(start, state)}: offers [${got.join(', ')}] outside its rows, want [${want.join(', ')}]`)
          }
        }
      }
    }
    forcing.queries = []
    forcing.mutations = []
    expect(firstProblems(problems)).toEqual([])
  })

  it('in each state of its form, every enabled button a section renders outside its rows is clicked, in that very state, by a SECTION_FLOW step naming it outside the rows', () => {
    stubFetch()
    const missing: string[] = []
    for (const start of Object.keys(SECTION_STARTS) as SectionStartName[]) {
      for (const filled of formStates(start)) {
        const setup = filled.map(typeInto)
        const { run } = runSection(start, setup, newWiring(), true)
        for (const button of buttonsOutsideRows(run.last)) {
          if (button.props.disabled === true) continue
          const label = labelOf(button)
          const covered = SECTION_FLOWS.some(
            (f) =>
              f.start === start &&
              f.steps.length > setup.length &&
              setup.every((s, i) => JSON.stringify(s) === JSON.stringify(f.steps[i])) &&
              // A bare label: a click outside the rows, never one in a row.
              f.steps[setup.length] === label,
          )
          if (!covered) missing.push(`${SECTION_STARTS[start].component}, ${filled.length === 0 ? 'form empty' : `filled ${filled.join(' + ')}`}: ${label}`)
        }
      }
    }
    expect(missing).toEqual([])
  })
})

// -- A member's drawer: what it says of its own list --

// DRAWER_LIST is what a member's drawer says of its list in each state of
// it -- each list status it shows -- and whether it shows the table: a
// list it could not load never reads as a member with no connected apps.
const DRAWER_LIST: Readonly<Record<ListState, { says: readonly string[]; table: boolean }>> = {
  loading: { says: ['Loading connected apps…'], table: false },
  failed: { says: ["Couldn't load this member's connected apps."], table: false },
  refused: { says: ['Your role cannot do this.'], table: false },
  empty: { says: ['No connected apps.'], table: false },
  listed: { says: [], table: true },
}

// listStatusIn is the text of each list status a render shows.
function listStatusIn(tree: Resolved[]): string[] {
  const statuses = (nodes: Resolved[]): Host[] => nodes.flatMap((n) => (typeof n === 'string' ? [] : [...(n.props.className === 'rail-empty' ? [n] : []), ...statuses(n.children)]))
  return statuses(tree).map(textOf)
}

describe("a member's drawer -- what it says of its list in each state, and that it asks for the list at all", () => {
  for (const list of LIST_STATES) {
    it(`list ${list}: it says ${DRAWER_LIST[list].says.length === 0 ? 'nothing of it' : DRAWER_LIST[list].says.join(', ')}, ${DRAWER_LIST[list].table ? 'and shows' : 'and shows no'} table`, () => {
      stubFetch()
      forcing.queries = [forcedList(list)]
      const { run } = runSection("a member's drawer", [], newWiring(), list === 'listed' ? true : list === 'empty' ? false : undefined)
      forcing.queries = []
      expect(listStatusIn(run.last)).toEqual(DRAWER_LIST[list].says)
      expect(hostsIn(run.last, 'table')).toHaveLength(DRAWER_LIST[list].table ? 1 : 0)
    })
  }

  // No render here runs a query, so the drawer's own list options, as it
  // passed them, are mounted on an observer of their own: one that never
  // fetched would leave the drawer loading for good.
  it("its list, mounted, sends GET for that member's list and loads it", async () => {
    render(<MemberConnectedApps member={member()} />)
    expect(captured.queries).toHaveLength(1)
    const calls = stubFetch()
    const queryClient = new QueryClient()
    const observer = new QueryObserver(queryClient, captured.queries[0] as QueryObserverOptions)
    const unsubscribe = observer.subscribe(() => {})
    try {
      await vi.waitFor(() => expect(observer.getCurrentResult().status).toBe('success'), { interval: 1 })
    } finally {
      unsubscribe()
      queryClient.clear()
    }
    expect(calls).toEqual([{ url: '/api/members/user%2F1%3Fx/mcp-authorizations', method: 'GET' }])
  })
})
