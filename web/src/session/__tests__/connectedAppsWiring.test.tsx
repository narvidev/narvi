// connectedAppsWiring.test.tsx -- what the connected-apps screens SEND,
// what each of their buttons does when clicked, and what each offers while
// its request is in flight: ConnectedAppsSection.tsx's rows and sections,
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
// and the next step acts in that render. The test never re-implements a
// confirmation. A bare label clicks a section's own button, outside its
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
// the viewer may manage a member, and the member's role, disabled flag and
// linked identities -- its own states -- the confirmation it has open, its
// drawer; and what it must offer in each combination of the two -- its
// buttons, in order, and what a click on each calls and leaves open. The harness holds the props in its own
// state, so a step can change them under the row, as the section does when
// a request starts or ends or its list refetches. Starting from every
// combination of the axes, a walk reaches every own state through the
// row's own clicks and every combination by changing one prop at a time.
// Then, for each row:
//   - every combination is reached, and in each, the row offers exactly the
//     model's buttons, and a click on each calls exactly the model's
//     callbacks and leaves the row where the model says -- checked up to
//     CLICK_DEPTH clicks on from every combination, so a state that looks
//     like another but acts differently is caught;
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
// sends exactly the request asserted. Each list holds two rows and each
// flow clicks in one of them, so the id sent is that row's own. A flow also
// asserts every row's buttons in the render right after its last click --
// with its request in flight, only the row that sent it shows the in-flight
// label and is locked, the other row's own confirmation open where the
// flow opened it -- and, once the request is done, exactly which of the
// screen's cached lists and audit pages it invalidated.
//
// The last describe block makes a section control without a flow fail the
// suite by name:
//   - every callback a section hands a row is called by one of that
//     section's flows;
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
// Not covered here:
//   - a row's text fields -- names, ids, redirect URIs, dates -- take one
//     value each: the axes are the fields that choose what it renders;
//   - a row's hidden state that shows only more than CLICK_DEPTH clicks
//     after a combination the walk reaches;
//   - with one of a client row's requests in flight, whether it can send
//     its OTHER one: the check above is per request, and today a client
//     whose disable is in flight can still be deleted, and one whose
//     delete is in flight disabled or enabled;
//   - the section buttons that only a finished request's state renders are
//     checked for what is offered, never clicked;
//   - controls that are not buttons, such as the member's role select;
//   - anything MembersPanel does with the onShowAudit it hands MemberRow:
//     this file proves only that Audit log calls it with that member's id.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { isValidElement, useState, type ReactElement, type ReactNode, type SetStateAction } from 'react'
import type * as ReactModule from 'react'
import { QueryClient, QueryClientProvider, type QueryKey } from '@tanstack/react-query'
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

// React's own useState, with every update counted.
vi.mock('react', async (importOriginal) => {
  const actual = await importOriginal<typeof ReactModule>()
  function useState<S>(initial?: S | (() => S)) {
    const [state, set] = actual.useState(initial)
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
// value of T -- a new client kind or role must join the axes below.
function allOf<T>() {
  return <const L extends readonly T[]>(values: L & ([Exclude<T, L[number]>] extends [never] ? unknown : { missing: Exclude<T, L[number]> })): L => values
}

const CLIENT_KINDS = allOf<MCPClient['kind']>()(['preregistered', 'dynamic', 'metadata_document'])
const AUTHORIZATION_KINDS = allOf<MCPAuthorization['clientKind']>()(['preregistered', 'dynamic', 'metadata_document'])
const MEMBER_ROLES = allOf<Member['role']>()(['admin', 'maintainer', 'member', 'viewer'])

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

type Wiring = { handed: Set<string>; called: string[] }

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
    const rendered = resolve((type as (p: typeof props) => ReactNode)(spied(type, props, options.wiring)), options)
    const row = ROWS.get(type)
    return row !== undefined && options.wiring !== undefined ? [{ tag: row, props: {}, children: rendered }] : rendered
  }
  if (typeof type === 'string') return [{ tag: type, props: props as HostProps, children: resolve(props.children, options) }]
  return resolve(props.children, options) // a fragment
}

// spied returns a row's props with each callback recorded as handed, and
// wrapped to record each call; any other component's props unchanged.
function spied<P extends Record<string, unknown>>(type: unknown, props: P, wiring: Wiring | undefined): P {
  const row = ROWS.get(type)
  if (row === undefined || wiring === undefined) return props
  const out: Record<string, unknown> = { ...props }
  for (const [name, value] of Object.entries(props)) {
    if (typeof value !== 'function') continue
    const callback = `${row}.${name}`
    wiring.handed.add(callback)
    out[name] = (...args: unknown[]) => {
      wiring.called.push(callback)
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

// What drive did: the steps it took, in order, and the last render.
type Run = { applied: string[]; last: Resolved[] }

// rendered records a render in run, and returns the step to take in it.
function rendered(run: Run, tree: Resolved[], steps: readonly Step[]): Step | undefined {
  run.last = tree
  const step = steps[run.applied.length]
  if (step !== undefined) run.applied.push(stepName(step))
  return step
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
  const tree = resolve(ui, options)
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
  const run: Run = { applied: [], last: [] }
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
  await vi.waitFor(() => expect(queryClient.isMutating()).toBe(0))
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
  // Every prop that changes what the row renders, with every value it can take.
  axes: Readonly<Record<string, readonly unknown[]>>
  // The row's own states, and the one it starts in.
  states: readonly string[]
  initial: string
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
    axes: { kind: CLIENT_KINDS, disabled: [false, true], deleting: [false, true], settingDisabled: [false, true] },
    states: ['no confirmation', 'confirming disable', 'confirming delete'],
    initial: 'no confirmation',
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
      if (state === 'confirming disable' && p.disabled !== true) {
        return [
          { label: p.settingDisabled === true ? 'Disabling…' : 'Confirm disable', disabled: p.settingDisabled === true, calls: ['onSetDisabled(true)'], then: 'confirming disable' },
          { label: 'Cancel', then: 'no confirmation' },
        ]
      }
      // No confirmation open -- or the disable confirmation once the list
      // shows the client disabled: it gives way to Enable and Delete, and
      // opens again if the list shows the client enabled.
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
    axes: { canManage: [true, false], role: MEMBER_ROLES, disabled: [false, true], linked: [false, true] },
    states: ['drawer closed', 'drawer open'],
    initial: 'drawer closed',
    inFlight: {},
    row: (p, log) => (
      <MemberRow
        member={member({ role: p.role as Member['role'], disabled: p.disabled === true, identities: p.linked === true ? [IDENTITY] : [] })}
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

// How many clicks on the walk checks from every combination it reaches.
const CLICK_DEPTH = 3

// RowHarness stands in for the section around a row: it logs each
// callback the row calls, and holds the props it hands the row in its own
// state, which a { become } step sets.
function RowHarness({ model, start, log, become }: { model: RowModel; start: RowProps; log: string[]; become: Become }) {
  const [props, setProps] = useState(start)
  become.take(setProps)
  return <>{model.row(props, log)}</>
}

function runRow(model: RowModel, start: RowProps, steps: readonly Step[]): { run: Run; log: string[] } {
  const log: string[] = []
  const become = newBecome()
  const run = drive(<RowHarness model={model} start={start} log={log} become={become} />, steps, new QueryClient(), { opaque: model.opaque, become })
  return { run, log }
}

// product is every combination of the axes' values.
function product(axes: RowModel['axes']): RowProps[] {
  return Object.entries(axes).reduce<RowProps[]>((all, [axis, values]) => all.flatMap((p) => values.map((v) => ({ ...p, [axis]: v }))), [{}])
}

// A combination the walk reached: the props it started the harness with,
// the steps from there, and the props and own state they lead to.
type RowNode = { start: RowProps; steps: readonly Step[]; props: RowProps; state: string }

function describeProps(props: RowProps): string {
  return Object.entries(props)
    .map(([axis, v]) => (v === true ? axis : v === false ? `not ${axis}` : `${axis} ${String(v)}`))
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

// conform checks node and, up to depth clicks on, every state its clicks
// reach, against the model: what the last step called, the buttons
// offered, and anything else the state shows. It says whether node offers
// the model's buttons, so that its clicks can be followed.
function conform(model: RowModel, node: RowNode, calls: { before: number; want: readonly string[] }, depth: number, problems: string[]): boolean {
  const { run, log } = runRow(model, node.start, node.steps)
  const where = describeNode(model, node)
  const called = log.slice(calls.before)
  if (JSON.stringify(called) !== JSON.stringify(calls.want)) problems.push(`${where}: the last click called [${called.join(', ')}], want [${calls.want.join(', ')}]`)
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
    conform(model, { ...node, steps: [...node.steps, offer.label], state: offer.then }, { before: log.length, want: offer.calls ?? [] }, depth - 1, problems)
  }
  return true
}

// walkRow reaches every combination of the model's axes and own states:
// from every combination of the axes, each own state through the row's
// own clicks, and each combination by changing one prop at a time. It
// conforms every combination it reaches.
function walkRow(model: RowModel): { reached: RowNode[]; problems: string[] } {
  const problems: string[] = []
  const reached: RowNode[] = []
  const seen = new Set<string>()
  const queue: RowNode[] = product(model.axes).map((p) => ({ start: p, steps: [], props: p, state: model.initial }))
  for (const node of queue) {
    const key = JSON.stringify([node.props, node.state])
    if (seen.has(key)) continue
    seen.add(key)
    reached.push(node)
    if (conform(model, node, { before: 0, want: [] }, CLICK_DEPTH, problems)) {
      for (const offer of model.offers(node.props, node.state)) {
        if (offer.disabled !== true) queue.push({ ...node, steps: [...node.steps, offer.label], state: offer.then })
      }
    }
    for (const [axis, values] of Object.entries(model.axes)) {
      for (const v of values) {
        if (v === node.props[axis]) continue
        const props = { ...node.props, [axis]: v }
        queue.push({ ...node, steps: [...node.steps, { become: props }], props })
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
    it(`${model.component}: every combination is reached, and every button in it, clicked, calls and leaves open what the model says`, () => {
      const { reached, problems } = walked(model)
      expect(firstProblems(problems)).toEqual([])
      const states = new Set(reached.map((n) => n.state))
      expect([...states].sort()).toEqual([...model.states].sort())
      expect(reached).toHaveLength(product(model.axes).length * model.states.length)
    })

    it(`${model.component}: while a request is in flight, the button that sends it says so and is disabled, and no other button sends it again`, () => {
      const { reached } = walked(model)
      expect(firstProblems(inFlightProblems(model, reached))).toEqual([])
    })
  }
})

// -- Section flows: each callback a section hands a row, down to fetch --

const TWO_AUTHORIZATIONS = [authorization(), authorization({ id: 'auth/2', clientId: 'narvi_mcp_c_two', clientName: 'Other Tool' })]

// The first client is disabled, the second enabled.
const TWO_CLIENTS = [client({ disabledAt: DISABLED_AT }), client({ id: 'client/2', clientId: 'narvi_mcp_c_two', clientName: 'Other Tool' })]

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
// section's own code hands it and gets back.
type SectionMutation = { name: string; variables: unknown; data: unknown }

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
      { name: 'register', variables: undefined, data: client({ id: 'client/3', clientId: 'narvi_mcp_c_new', clientName: 'New App' }) },
      { name: 'delete', variables: 'client/2', data: undefined },
      { name: 'disable/enable', variables: { clientId: 'client/2', disabled: true }, data: client({ id: 'client/2', disabledAt: DISABLED_AT }) },
    ],
  },
  "a member's drawer": {
    component: 'MemberConnectedApps',
    ui: () => <MemberConnectedApps member={member()} />,
    list: mcpAuthorizationQueryKeys.member(member().id),
    data: (rows) => ({ authorizations: rows ? TWO_AUTHORIZATIONS : [] }),
    mutations: [{ name: 'revoke', variables: 'auth/2', data: undefined }],
  },
  'your own connected apps': {
    component: 'ConnectedAppsSection',
    ui: () => <ConnectedAppsSection />,
    list: mcpAuthorizationQueryKeys.mine(),
    data: (rows) => ({ authorizations: rows ? TWO_AUTHORIZATIONS : [] }),
    mutations: [{ name: 'revoke', variables: 'auth/2', data: undefined }],
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
  steps: readonly Step[]
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

const SECTION_FLOWS: readonly SectionFlow[] = [
  {
    start: 'the MCP clients section',
    steps: [
      { row: 'narvi_mcp_c_two', click: 'Disable' },
      { row: 'narvi_mcp_c_two', click: 'Confirm disable' },
    ],
    after: { rows: { narvi_mcp_c_one: ['Enable', 'Delete'], narvi_mcp_c_two: ['Disabling… (disabled)', 'Cancel'] }, outside: ['Register client (disabled)'] },
    sends: [{ url: '/api/mcp-clients/client%2F2/disable', method: 'POST' }],
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
    after: { rows: { narvi_mcp_c_one: ['Enabling… (disabled)', 'Delete'], narvi_mcp_c_two: ['Disable', 'Delete'] }, outside: ['Register client (disabled)'] },
    sends: [{ url: '/api/mcp-clients/client%2F1/enable', method: 'POST' }],
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
    after: { rows: { narvi_mcp_c_one: ['Confirm delete', 'Cancel'], narvi_mcp_c_two: ['Deleting… (disabled)', 'Cancel'] }, outside: ['Register client (disabled)'] },
    sends: [{ url: '/api/mcp-clients/client%2F2', method: 'DELETE' }],
    calls: ['MCPClientRow.onDelete'],
    // Deleting a client revokes every user's authorization of it.
    invalidates: ['the MCP clients list', 'your own connected apps', "this member's connected apps", "another member's connected apps"],
  },
  {
    start: 'the MCP clients section',
    steps: [...REGISTER_FORM, 'Register client'],
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
    after: { rows: { narvi_mcp_c_one: ['Enable', 'Delete'], narvi_mcp_c_two: ['Disable', 'Delete'] }, outside: ['Registering… (disabled)'] },
    sends: [{ url: '/api/mcp-clients', method: 'POST', body: { clientName: 'Editor Plugin', redirectUris: ['http://127.0.0.1/callback', 'https://client.example/cb'] } }],
    calls: [],
    invalidates: ['the MCP clients list'],
  },
  {
    start: "a member's drawer",
    steps: [
      { row: 'Editor Plugin', click: 'Revoke' },
      { row: 'Other Tool', click: 'Revoke' },
      { row: 'Other Tool', click: 'Confirm revoke' },
    ],
    after: { rows: { 'Editor Plugin': ['Confirm revoke', 'Cancel'], 'Other Tool': ['Revoking… (disabled)', 'Cancel'] }, outside: [] },
    sends: [{ url: '/api/members/user%2F1%3Fx/mcp-authorizations/auth%2F2', method: 'DELETE' }],
    calls: ['ConnectedAppRow.onRevoke'],
    // Every user's list, the admin's own included, and the audit log below.
    invalidates: ['your own connected apps', "this member's connected apps", "another member's connected apps", 'an audit log page'],
  },
  {
    start: 'your own connected apps',
    steps: [
      { row: 'Editor Plugin', click: 'Revoke' },
      { row: 'Other Tool', click: 'Revoke' },
      { row: 'Other Tool', click: 'Confirm revoke' },
    ],
    after: { rows: { 'Editor Plugin': ['Confirm revoke', 'Cancel'], 'Other Tool': ['Revoking… (disabled)', 'Cancel'] }, outside: [] },
    sends: [{ url: '/api/me/mcp-authorizations/auth%2F2', method: 'DELETE' }],
    calls: ['ConnectedAppRow.onRevoke'],
    invalidates: ['your own connected apps'],
  },
]

// seeded is a query client holding every SEEDED result, and the section's
// own list with its two rows, or none, or -- rows undefined -- nothing.
function seeded(start: SectionStart, rows: boolean | undefined): QueryClient {
  const queryClient = new QueryClient()
  for (const key of Object.values(SEEDED)) queryClient.setQueryData(key, { seeded: true })
  if (rows === undefined) queryClient.removeQueries({ queryKey: start.list, exact: true })
  else queryClient.setQueryData(start.list, start.data(rows))
  return queryClient
}

function runSection(start: SectionStartName, steps: readonly Step[], wiring: Wiring, rows: boolean | undefined = true): { run: Run; queryClient: QueryClient } {
  const section = SECTION_STARTS[start] as SectionStart
  const queryClient = seeded(section, rows)
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

function sectionFlowName(flow: SectionFlow): string {
  const sends = flow.sends.length === 0 ? 'sends nothing' : `sends ${flow.sends.map((c) => `${c.method} ${c.url}`).join(', ')}`
  const invalidates = flow.invalidates.length === 0 ? '' : `, locked while it is in flight, then invalidates ${flow.invalidates.join(', ')}`
  return `${SECTION_STARTS[flow.start].component}, ${flow.start}: ${flow.steps.map(stepName).join(', then ')} ${sends}${invalidates}`
}

describe('each section, clicked through, sends exactly its own request, locks only the row that sent it, and invalidates exactly what it changed', () => {
  for (const flow of SECTION_FLOWS) {
    it(sectionFlowName(flow), async () => {
      const calls = stubFetch()
      const wiring: Wiring = { handed: new Set(), called: [] }
      const { run, queryClient } = runSection(flow.start, flow.steps, wiring)
      expect(run.applied).toEqual(flow.steps.map(stepName))
      expect(offeredIn(run.last, Object.keys(flow.after.rows))).toEqual(flow.after)

      await settled(queryClient)
      expect(wiring.called).toEqual(flow.calls)
      expect(calls).toEqual(flow.sends)
      expect(invalidated(queryClient)).toEqual(flow.invalidates)
    })
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
  const { run } = runSection(start, [], { handed: new Set(), called: [] })
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
  it('every callback a section hands a row is called by one of that section\'s SECTION_FLOWS', () => {
    stubFetch()
    const missing: string[] = []
    for (const start of Object.keys(SECTION_STARTS) as SectionStartName[]) {
      const wiring: Wiring = { handed: new Set(), called: [] }
      runSection(start, [], wiring)
      expect([...wiring.handed], `${start} hands its rows no callback`).not.toEqual([])
      for (const callback of wiring.handed) {
        if (!SECTION_FLOWS.some((f) => f.start === start && f.calls.includes(callback))) missing.push(`${start}: ${callback}`)
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
            const { run } = runSection(start, filled.map(typeInto), { handed: new Set(), called: [] }, rows)
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
        const { run } = runSection(start, setup, { handed: new Set(), called: [] })
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
