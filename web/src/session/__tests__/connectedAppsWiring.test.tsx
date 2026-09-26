// connectedAppsWiring.test.tsx -- what the connected-apps screens SEND,
// and what each of their buttons does when clicked: ConnectedAppsSection.tsx's
// rows and sections, and the Members & access row (MembersPanel.tsx's
// MemberRow) whose button opens a member's Connected apps drawer.
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
// a field through its real onChange. That sets state during the render,
// React renders the wrapper again, and the next step acts in that render.
// The test never re-implements a confirmation. There are two tables:
//
//   - ROW_FLOWS click each row's buttons through a harness standing in for
//     the section around the row. It logs every callback the row calls,
//     and for an MCP client it holds the client in its own state, so
//     onSetDisabled sets or clears disabledAt at once, as the section's
//     list does once it refetches. That is the only way the row's hand-off
//     from the disable confirmation back to Enable and Delete is reached.
//   - SECTION_FLOWS click through each section down to the stubbed fetch:
//     each callback a section hands a row, and the section's own Register
//     client, sends exactly the request asserted.
//
// Together they chain each button to its request: a row flow proves which
// callback a button calls in a given state, and a section flow proves
// which request that callback sends.
//
// # No button without a flow
//
// The last describe block makes an unclicked button fail the suite by
// name. It checks four things:
//   - every state a row reaches by clicks is listed in ROW_STATES, found
//     by clicking every enabled button from each start until no new
//     render appears;
//   - every button a listed row state offers is clicked, in that very
//     state, by a ROW_FLOW (a flow whose steps are the state's own steps
//     followed by that button);
//   - every button a SECTION_STATE offers outside its table rows is
//     clicked, in that state, by a SECTION_FLOW;
//   - every callback a section hands a row is called by one of that
//     section's flows.
// A button that is disabled in a state is exempt there only if a flow
// clicks it from another state of the same start.
//
// Not covered here: states that only a request in flight reaches. A row's
// "Disabling…", "Deleting…", "Enabling…" and "Revoking…" are the same
// buttons, disabled; connectedAppsRendering.test.tsx renders them.
// Controls that are not buttons, such as the member's role select, are
// not covered either. Nor is anything MembersPanel does with the
// onShowAudit it hands MemberRow: this file proves only that Audit log
// calls it with that member's id.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { isValidElement, useState, type ReactElement, type ReactNode } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type * as ReactQuery from '@tanstack/react-query'

import type { MCPAuthorization, MCPClient, Member } from '@narvi/contracts/rest-dtos'

import { mcpAuthorizationQueryKeys, mcpClientQueryKeys } from '../../api/queryKeys'
import { ConnectedAppRow, ConnectedAppsSection, MCPClientRow, MCPClientsSection, MemberConnectedApps } from '../ConnectedAppsSection'
import { MemberRow } from '../MembersPanel'

// The options of every useQuery and useMutation call, in call order.
const captured = vi.hoisted(() => ({ queries: [] as unknown[], mutations: [] as unknown[] }))

vi.mock('@tanstack/react-query', async (importOriginal) => {
  const actual = await importOriginal<typeof ReactQuery>()
  const { useState } = await import('react')
  const useQuery = (...args: Parameters<typeof actual.useQuery>) => {
    captured.queries.push(args[0])
    return actual.useQuery(...args)
  }
  type MutationOptions = Parameters<typeof actual.useMutation>[0]
  // useMutation hands its observer each render's options from an effect,
  // and a server render never runs effects: a mutationFn that reads the
  // component's state, such as the register form's, would keep the first
  // render's. The observer gets a mutationFn that calls the latest render's
  // instead, which is what the browser's effect gives it before any click.
  const latestOptions = new WeakMap<object, MutationOptions>()
  const useMutation = (...args: Parameters<typeof actual.useMutation>) => {
    const [options, queryClient] = args
    captured.mutations.push(options)
    const [synced] = useState(() => {
      const self: MutationOptions = {
        ...options,
        mutationFn: (...call: Parameters<NonNullable<MutationOptions['mutationFn']>>) => {
          const mutationFn = latestOptions.get(self)?.mutationFn
          if (mutationFn === undefined) throw new Error('useMutation: no mutationFn')
          return mutationFn(...call)
        },
      }
      return self
    })
    latestOptions.set(synced, options)
    return actual.useMutation(synced, queryClient)
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

const DISABLED_AT = '2026-09-22T00:00:00Z'

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

// The row components. When resolve is given a Wiring, it records each
// callback prop a section hands one of them, and each call of one.
const ROWS = new Map<unknown, string>([
  [MCPClientRow, 'MCPClientRow'],
  [ConnectedAppRow, 'ConnectedAppRow'],
])

type Wiring = { handed: Set<string>; called: string[] }

type ResolveOptions = {
  // Components never called: each renders as a host element named for it,
  // with its props, and no children. Needed for a component that appears
  // only after a click, because its hooks would change the wrapper's hook
  // count between renders.
  opaque?: ReadonlyMap<unknown, string>
  wiring?: Wiring
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
    return resolve((type as (p: typeof props) => ReactNode)(spied(type, props, options.wiring)), options)
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

// buttonsOutsideRows is every button in nodes that no table row contains.
function buttonsOutsideRows(nodes: Resolved[]): Host[] {
  return nodes.flatMap((n) => (typeof n === 'string' || n.tag === 'tr' ? [] : [...(n.tag === 'button' ? [n] : []), ...buttonsOutsideRows(n.children)]))
}

// labelOf is a button's accessible name: its aria-label, else its text.
function labelOf(button: Host): string {
  return button.props['aria-label'] ?? textOf(button)
}

function buttonLabels(nodes: Resolved[]): string[] {
  return hostsIn(nodes, 'button').map(labelOf)
}

// rowShowing is the one table row whose text contains text.
function rowShowing(tree: Resolved[], text: string): Host {
  const rows = hostsIn(tree, 'tr').filter((r) => textOf(r).includes(text))
  const [row] = rows
  if (row === undefined || rows.length !== 1) throw new Error(`expected one row showing "${text}", found ${rows.length}`)
  return row
}

// shape is a render as markup-like text: every tag, text, disabled flag and
// aria-expanded value. Two renders with the same shape look the same.
function shape(nodes: Resolved[]): string {
  return nodes
    .map((n) => {
      if (typeof n === 'string') return JSON.stringify(n)
      const flags = `${n.props.disabled === true ? ' disabled' : ''}${n.props['aria-expanded'] === undefined ? '' : ` aria-expanded=${String(n.props['aria-expanded'])}`}`
      return `<${n.tag}${flags}>${shape(n.children)}</${n.tag}>`
    })
    .join('')
}

// -- Driving clicks --

// A step: a click on the only button so labelled in the whole render, a
// click on the button so labelled in the one table row showing `row`, or
// typing `type` into the field whose placeholder is `field`.
type Step = string | { row: string; click: string } | { field: string; type: string }

function stepName(step: Step): string {
  if (typeof step === 'string') return step
  if ('click' in step) return `${step.click} in the row showing ${step.row}`
  return `type into ${step.field}`
}

// clickedLabel is the label of the button step clicks, if it clicks one.
function clickedLabel(step: Step | undefined): string | undefined {
  if (step === undefined) return undefined
  if (typeof step === 'string') return step
  return 'click' in step ? step.click : undefined
}

function act(tree: Resolved[], step: Step): void {
  if (typeof step !== 'string' && 'field' in step) {
    const fields = [...hostsIn(tree, 'input'), ...hostsIn(tree, 'textarea')].filter((f) => f.props.placeholder === step.field)
    const [field] = fields
    if (field === undefined || fields.length !== 1 || field.props.onChange === undefined) throw new Error(`no single field "${step.field}" to type into`)
    field.props.onChange({ target: { value: step.type } })
    return
  }
  const scope = typeof step === 'string' ? tree : [rowShowing(tree, step.row)]
  const label = typeof step === 'string' ? step : step.click
  const where = typeof step === 'string' ? 'the render' : `the row showing "${step.row}"`
  const buttons = hostsIn(scope, 'button').filter((b) => labelOf(b) === label)
  const [button] = buttons
  if (button === undefined || buttons.length !== 1) throw new Error(`no single "${label}" button in ${where}; it offers ${JSON.stringify(buttonLabels(scope))}`)
  if (button.props.disabled === true || button.props.onClick === undefined) throw new Error(`"${label}" in ${where} cannot be clicked`)
  button.props.onClick()
}

// What drive did: the steps it took, in order, and the last render.
type Run = { applied: string[]; last: Resolved[] }

function Driver({ ui, steps, run, options }: { ui: ReactNode; steps: readonly Step[]; run: Run; options: ResolveOptions }) {
  const tree = resolve(ui, options)
  run.last = tree
  const step = steps[run.applied.length]
  if (step !== undefined) {
    run.applied.push(stepName(step))
    act(tree, step)
  }
  return null
}

// drive renders ui and takes each step in turn, one per render. A step
// that changes no state ends the run there: the render it acted on is the
// last, and run.applied says how far it got.
function drive(ui: ReactNode, steps: readonly Step[], queryClient: QueryClient, options: ResolveOptions = {}): Run {
  const run: Run = { applied: [], last: [] }
  renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <Driver ui={ui} steps={steps} run={run} options={options} />
    </QueryClientProvider>,
  )
  return run
}

// settled waits for every mutation a click started to finish.
async function settled(queryClient: QueryClient): Promise<void> {
  await vi.waitFor(() => expect(queryClient.isMutating()).toBe(0))
}

// coveredBy says whether some flow from start takes exactly the steps
// `setup` and then clicks the button labelled `label`.
function coveredBy(flows: readonly { start: string; steps: readonly Step[] }[], start: string, setup: readonly Step[], label: string): boolean {
  return flows.some(
    (f) => f.start === start && f.steps.length > setup.length && setup.every((s, i) => JSON.stringify(s) === JSON.stringify(f.steps[i])) && clickedLabel(f.steps[setup.length]) === label,
  )
}

// clickedFrom says whether some flow from start clicks `label` at any step.
function clickedFrom(flows: readonly { start: string; steps: readonly Step[] }[], start: string, label: string): boolean {
  return flows.some((f) => f.start === start && f.steps.some((s) => clickedLabel(s) === label))
}

beforeEach(() => {
  captured.queries.length = 0
  captured.mutations.length = 0
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

// -- Row flows: which callback each button calls, in each state --

// ClientRowHarness stands in for MCPClientsSection around one row. It logs
// each callback the row calls, and keeps the client in its own state, so
// onSetDisabled sets or clears disabledAt at once, as the section's list
// does once it refetches.
function ClientRowHarness({ start, log }: { start: MCPClient; log: string[] }) {
  const [shown, setShown] = useState(start)
  return (
    <MCPClientRow
      client={shown}
      deleting={false}
      settingDisabled={false}
      onDelete={() => log.push('onDelete')}
      onSetDisabled={(disabled) => {
        log.push(`onSetDisabled(${String(disabled)})`)
        setShown({ ...shown, disabledAt: disabled ? DISABLED_AT : null })
      }}
    />
  )
}

// MemberRow's drawer is MemberConnectedApps, whose hooks appear only once
// the drawer opens: it stays uncalled, visible as a host of its own name.
const DRAWER = new Map<unknown, string>([[MemberConnectedApps, 'MemberConnectedApps']])

type RowStart = { component: string; ui: (log: string[]) => ReactNode; opaque?: ReadonlyMap<unknown, string> }

const ROW_STARTS = {
  'an enabled client': { component: 'MCPClientRow', ui: (log) => <ClientRowHarness start={client()} log={log} /> },
  'a disabled client': { component: 'MCPClientRow', ui: (log) => <ClientRowHarness start={client({ disabledAt: DISABLED_AT })} log={log} /> },
  'an authorization': {
    component: 'ConnectedAppRow',
    ui: (log) => <ConnectedAppRow authorization={authorization()} revoking={false} onRevoke={() => log.push('onRevoke')} />,
  },
  'a member, shown to an admin': {
    component: 'MemberRow',
    ui: (log) => <MemberRow member={member()} canManage onShowAudit={(id) => log.push(`onShowAudit(${id})`)} />,
    opaque: DRAWER,
  },
  'a member, shown to anyone else': {
    component: 'MemberRow',
    ui: (log) => <MemberRow member={member()} canManage={false} onShowAudit={(id) => log.push(`onShowAudit(${id})`)} />,
    opaque: DRAWER,
  },
} satisfies Record<string, RowStart>

type RowStartName = keyof typeof ROW_STARTS

type RowFlow = {
  start: RowStartName
  steps: readonly string[]
  // Every callback the clicks called, in order.
  calls: readonly string[]
  // The buttons the last render offers. Omitted where the last click only
  // calls a callback, which changes nothing the row shows.
  offers?: readonly string[]
  // MemberRow only: whether that member's drawer is open after the steps.
  drawer?: boolean
}

// ROW_FLOWS is the click-through table: the meta-tests below fail for any
// button, in any state, that no flow here clicks.
const ROW_FLOWS: readonly RowFlow[] = [
  // MCPClientRow, an enabled client.
  { start: 'an enabled client', steps: ['Disable'], calls: [], offers: ['Confirm disable', 'Cancel'] },
  { start: 'an enabled client', steps: ['Disable', 'Confirm disable'], calls: ['onSetDisabled(true)'], offers: ['Enable', 'Delete'] },
  { start: 'an enabled client', steps: ['Disable', 'Cancel'], calls: [], offers: ['Disable', 'Delete'] },
  { start: 'an enabled client', steps: ['Delete'], calls: [], offers: ['Confirm delete', 'Cancel'] },
  { start: 'an enabled client', steps: ['Delete', 'Cancel'], calls: [], offers: ['Disable', 'Delete'] },
  { start: 'an enabled client', steps: ['Delete', 'Confirm delete'], calls: ['onDelete'] },
  // The hand-off: the list shows the confirmed disable while the row's
  // disable confirmation is still its state.
  { start: 'an enabled client', steps: ['Disable', 'Confirm disable', 'Enable'], calls: ['onSetDisabled(true)', 'onSetDisabled(false)'], offers: ['Disable', 'Delete'] },
  { start: 'an enabled client', steps: ['Disable', 'Confirm disable', 'Delete'], calls: ['onSetDisabled(true)'], offers: ['Confirm delete', 'Cancel'] },
  // MCPClientRow, a disabled client.
  { start: 'a disabled client', steps: ['Enable'], calls: ['onSetDisabled(false)'], offers: ['Disable', 'Delete'] },
  { start: 'a disabled client', steps: ['Delete'], calls: [], offers: ['Confirm delete', 'Cancel'] },
  { start: 'a disabled client', steps: ['Delete', 'Cancel'], calls: [], offers: ['Enable', 'Delete'] },
  { start: 'a disabled client', steps: ['Delete', 'Confirm delete'], calls: ['onDelete'] },
  // ConnectedAppRow: the caller's own list and the admin's drawer.
  { start: 'an authorization', steps: ['Revoke'], calls: [], offers: ['Confirm revoke', 'Cancel'] },
  { start: 'an authorization', steps: ['Revoke', 'Cancel'], calls: [], offers: ['Revoke'] },
  { start: 'an authorization', steps: ['Revoke', 'Confirm revoke'], calls: ['onRevoke'] },
  // MemberRow: the drawer's own button, and Audit log beside it.
  { start: 'a member, shown to an admin', steps: ['Connected apps'], calls: [], offers: ['Connected apps', 'Audit log'], drawer: true },
  { start: 'a member, shown to an admin', steps: ['Connected apps', 'Connected apps'], calls: [], offers: ['Connected apps', 'Audit log'], drawer: false },
  { start: 'a member, shown to an admin', steps: ['Audit log'], calls: ['onShowAudit(user/1?x)'] },
  { start: 'a member, shown to an admin', steps: ['Connected apps', 'Audit log'], calls: ['onShowAudit(user/1?x)'] },
  { start: 'a member, shown to anyone else', steps: ['Audit log'], calls: ['onShowAudit(user/1?x)'] },
]

function runRow(start: RowStartName, steps: readonly string[]): { run: Run; log: string[] } {
  const { ui, opaque } = ROW_STARTS[start] as RowStart
  const log: string[] = []
  const run = drive(ui(log), steps, new QueryClient(), { opaque })
  return { run, log }
}

function rowFlowName(flow: RowFlow): string {
  const calls = flow.calls.length === 0 ? 'calls nothing' : `calls ${flow.calls.join(', ')}`
  const offers = flow.offers === undefined ? '' : `, then offers ${flow.offers.join(', ')}`
  const drawer = flow.drawer === undefined ? '' : `, drawer ${flow.drawer ? 'open' : 'closed'}`
  return `${ROW_STARTS[flow.start].component}, ${flow.start}: ${flow.steps.join(', then ')} ${calls}${offers}${drawer}`
}

describe('each row button, clicked, calls what it should and leaves the row offering what it should', () => {
  for (const flow of ROW_FLOWS) {
    it(rowFlowName(flow), () => {
      const calls = stubFetch()
      const { run, log } = runRow(flow.start, flow.steps)

      expect(run.applied).toEqual(flow.steps)
      expect(log).toEqual(flow.calls)
      if (flow.offers !== undefined) expect(buttonLabels(run.last)).toEqual(flow.offers)
      if (flow.drawer !== undefined) {
        const drawers = hostsIn(run.last, 'MemberConnectedApps')
        expect(drawers.map((d) => d.props.member?.id)).toEqual(flow.drawer ? [member().id] : [])
        const [toggle] = hostsIn(run.last, 'button').filter((b) => labelOf(b) === 'Connected apps')
        expect(toggle?.props['aria-expanded']).toBe(flow.drawer)
      }
      // A row sends nothing itself: every request is its section's.
      expect(calls).toEqual([])
    })
  }
})

// -- Section flows: each callback a section hands a row, down to fetch --

const TWO_AUTHORIZATIONS = [authorization(), authorization({ id: 'auth/2', clientId: 'narvi_mcp_c_two', clientName: 'Other Tool' })]

type SectionStart = { component: string; ui: () => ReactNode; cache: (queryClient: QueryClient) => void }

// Each list holds two rows, and each flow clicks in one of them, so the id
// sent is that row's own. The first client is disabled, the second enabled.
const SECTION_STARTS = {
  'the MCP clients section': {
    component: 'MCPClientsSection',
    ui: () => <MCPClientsSection />,
    cache: (q) =>
      q.setQueryData(mcpClientQueryKeys.list(), {
        clients: [client({ disabledAt: DISABLED_AT }), client({ id: 'client/2', clientId: 'narvi_mcp_c_two', clientName: 'Other Tool' })],
      }),
  },
  "a member's drawer": {
    component: 'MemberConnectedApps',
    ui: () => <MemberConnectedApps member={member()} />,
    cache: (q) => q.setQueryData(mcpAuthorizationQueryKeys.member(member().id), { authorizations: TWO_AUTHORIZATIONS }),
  },
  'your own connected apps': {
    component: 'ConnectedAppsSection',
    ui: () => <ConnectedAppsSection />,
    cache: (q) => q.setQueryData(mcpAuthorizationQueryKeys.mine(), { authorizations: TWO_AUTHORIZATIONS }),
  },
} satisfies Record<string, SectionStart>

type SectionStartName = keyof typeof SECTION_STARTS

// The register form, filled: surrounding blanks and a repeated redirect URI
// are the section's to drop.
const REGISTER_FORM: readonly Step[] = [
  { field: 'App name', type: ' Editor Plugin ' },
  { field: 'Redirect URIs, one per line', type: 'http://127.0.0.1/callback\n https://client.example/cb \nhttp://127.0.0.1/callback' },
  { field: 'Homepage (optional, https)', type: ' https://client.example ' },
]

type SectionFlow = {
  start: SectionStartName
  steps: readonly Step[]
  // Every request sent, in order.
  sends: readonly Call[]
  // Every row callback the clicks called, in order.
  calls: readonly string[]
}

const SECTION_FLOWS: readonly SectionFlow[] = [
  {
    start: 'the MCP clients section',
    steps: [
      { row: 'narvi_mcp_c_two', click: 'Disable' },
      { row: 'narvi_mcp_c_two', click: 'Confirm disable' },
    ],
    sends: [{ url: '/api/mcp-clients/client%2F2/disable', method: 'POST' }],
    calls: ['MCPClientRow.onSetDisabled'],
  },
  { start: 'the MCP clients section', steps: [{ row: 'narvi_mcp_c_two', click: 'Disable' }], sends: [], calls: [] },
  {
    start: 'the MCP clients section',
    steps: [{ row: 'narvi_mcp_c_one', click: 'Enable' }],
    sends: [{ url: '/api/mcp-clients/client%2F1/enable', method: 'POST' }],
    calls: ['MCPClientRow.onSetDisabled'],
  },
  {
    start: 'the MCP clients section',
    steps: [
      { row: 'narvi_mcp_c_two', click: 'Delete' },
      { row: 'narvi_mcp_c_two', click: 'Confirm delete' },
    ],
    sends: [{ url: '/api/mcp-clients/client%2F2', method: 'DELETE' }],
    calls: ['MCPClientRow.onDelete'],
  },
  {
    start: 'the MCP clients section',
    steps: [...REGISTER_FORM, 'Register client'],
    sends: [
      {
        url: '/api/mcp-clients',
        method: 'POST',
        body: { clientName: 'Editor Plugin', redirectUris: ['http://127.0.0.1/callback', 'https://client.example/cb'], clientUri: 'https://client.example' },
      },
    ],
    calls: [],
  },
  {
    start: "a member's drawer",
    steps: [
      { row: 'Other Tool', click: 'Revoke' },
      { row: 'Other Tool', click: 'Confirm revoke' },
    ],
    sends: [{ url: '/api/members/user%2F1%3Fx/mcp-authorizations/auth%2F2', method: 'DELETE' }],
    calls: ['ConnectedAppRow.onRevoke'],
  },
  {
    start: 'your own connected apps',
    steps: [
      { row: 'Other Tool', click: 'Revoke' },
      { row: 'Other Tool', click: 'Confirm revoke' },
    ],
    sends: [{ url: '/api/me/mcp-authorizations/auth%2F2', method: 'DELETE' }],
    calls: ['ConnectedAppRow.onRevoke'],
  },
]

function runSection(start: SectionStartName, steps: readonly Step[], wiring: Wiring): { run: Run; queryClient: QueryClient } {
  const { ui, cache } = SECTION_STARTS[start] as SectionStart
  const queryClient = new QueryClient()
  cache(queryClient)
  return { run: drive(ui(), steps, queryClient, { wiring }), queryClient }
}

function sectionFlowName(flow: SectionFlow): string {
  const sends = flow.sends.length === 0 ? 'sends nothing' : `sends ${flow.sends.map((c) => `${c.method} ${c.url}`).join(', ')}`
  return `${SECTION_STARTS[flow.start].component}, ${flow.start}: ${flow.steps.map(stepName).join(', then ')} ${sends}`
}

describe('each section, clicked through, sends exactly its own request', () => {
  for (const flow of SECTION_FLOWS) {
    it(sectionFlowName(flow), async () => {
      const calls = stubFetch()
      const wiring: Wiring = { handed: new Set(), called: [] }
      const { run, queryClient } = runSection(flow.start, flow.steps, wiring)
      await settled(queryClient)

      expect(run.applied).toEqual(flow.steps.map(stepName))
      expect(wiring.called).toEqual(flow.calls)
      expect(calls).toEqual(flow.sends)
    })
  }
})

// -- No button without a flow --

type RowState = { name: string; start: RowStartName; steps: readonly string[] }

// ROW_STATES is every state a row can be in, each reached from its start
// through the row's own clicks.
const ROW_STATES: readonly RowState[] = [
  { name: 'MCPClientRow, enabled, no confirmation open', start: 'an enabled client', steps: [] },
  { name: 'MCPClientRow, enabled, confirming disable', start: 'an enabled client', steps: ['Disable'] },
  { name: 'MCPClientRow, enabled, confirming delete', start: 'an enabled client', steps: ['Delete'] },
  { name: 'MCPClientRow, disabled, no confirmation open', start: 'a disabled client', steps: [] },
  { name: 'MCPClientRow, disabled, confirming disable (the list shows the confirmed disable)', start: 'an enabled client', steps: ['Disable', 'Confirm disable'] },
  { name: 'MCPClientRow, disabled, confirming delete', start: 'a disabled client', steps: ['Delete'] },
  { name: 'ConnectedAppRow, idle', start: 'an authorization', steps: [] },
  { name: 'ConnectedAppRow, confirming revoke', start: 'an authorization', steps: ['Revoke'] },
  { name: 'MemberRow, shown to an admin, drawer closed', start: 'a member, shown to an admin', steps: [] },
  { name: 'MemberRow, shown to an admin, drawer open', start: 'a member, shown to an admin', steps: ['Connected apps'] },
  { name: 'MemberRow, shown to anyone else', start: 'a member, shown to anyone else', steps: [] },
]

type SectionState = { name: string; start: SectionStartName; steps: readonly Step[] }

// SECTION_STATES is every state of a section's own controls -- the
// buttons outside its table rows, which are the rows' own.
const SECTION_STATES: readonly SectionState[] = [
  { name: 'MCPClientsSection, register form empty', start: 'the MCP clients section', steps: [] },
  { name: 'MCPClientsSection, register form filled', start: 'the MCP clients section', steps: REGISTER_FORM },
  { name: "MemberConnectedApps, a member's list", start: "a member's drawer", steps: [] },
  { name: 'ConnectedAppsSection, your own list', start: 'your own connected apps', steps: [] },
]

// reachableShapes clicks every enabled button from start, then from each
// new render, until no click shows anything new, and returns each render
// reached with the first steps that reached it.
function reachableShapes(start: RowStartName): { shape: string; steps: readonly string[] }[] {
  const seen = new Map<string, readonly string[]>()
  const queue: (readonly string[])[] = [[]]
  for (let i = 0; i < queue.length; i++) {
    if (i > 1000) throw new Error(`reachableShapes: no end to the renders reached from ${start}`)
    const steps = queue[i]
    if (steps === undefined) break
    const { run } = runRow(start, steps)
    const reached = shape(run.last)
    if (seen.has(reached)) continue
    seen.set(reached, steps)
    for (const button of hostsIn(run.last, 'button')) if (button.props.disabled !== true) queue.push([...steps, labelOf(button)])
  }
  return [...seen].map(([s, steps]) => ({ shape: s, steps }))
}

describe('no button without a click-through flow', () => {
  it('ROW_STATES lists every state a row reaches through its own clicks', () => {
    const listed = new Set(ROW_STATES.map((s) => shape(runRow(s.start, s.steps).run.last)))
    const unlisted = (Object.keys(ROW_STARTS) as RowStartName[]).flatMap((start) =>
      reachableShapes(start)
        .filter((r) => !listed.has(r.shape))
        .map((r) => `${start}: ${r.steps.join(', then ') || 'no click'}`),
    )
    expect(unlisted).toEqual([])
  })

  it('every button each ROW_STATE offers is clicked, in that very state, by a ROW_FLOW', () => {
    const missing: string[] = []
    for (const state of ROW_STATES) {
      const { run } = runRow(state.start, state.steps)
      expect(run.applied, state.name).toEqual(state.steps)
      const buttons = hostsIn(run.last, 'button')
      expect(buttons, `${state.name} offers no button at all`).not.toEqual([])
      for (const button of buttons) {
        const label = labelOf(button)
        const covered = button.props.disabled === true ? clickedFrom(ROW_FLOWS, state.start, label) : coveredBy(ROW_FLOWS, state.start, state.steps, label)
        if (!covered) missing.push(`${state.name}: ${label}`)
      }
    }
    expect(missing).toEqual([])
  })

  it('every button a SECTION_STATE offers outside its table rows is clicked, in that very state, by a SECTION_FLOW', () => {
    stubFetch()
    const missing: string[] = []
    for (const state of SECTION_STATES) {
      const { run } = runSection(state.start, state.steps, { handed: new Set(), called: [] })
      expect(run.applied, state.name).toEqual(state.steps.map(stepName))
      for (const button of buttonsOutsideRows(run.last)) {
        const label = labelOf(button)
        const covered = button.props.disabled === true ? clickedFrom(SECTION_FLOWS, state.start, label) : coveredBy(SECTION_FLOWS, state.start, state.steps, label)
        if (!covered) missing.push(`${state.name}: ${label}`)
      }
    }
    expect(missing).toEqual([])
  })

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
})
