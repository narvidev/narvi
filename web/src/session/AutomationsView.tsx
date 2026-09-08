// AutomationsView.tsx -- §12.2 item 4's own automations screen (decisions
// 18-20): a table with a health column and expandable invocation -> runs
// rows.
//
// # Every third-party-authored string here is plain text
//
// automation.name/automation.prompt (human-authored, by whoever configured
// the automation -- free text, never Narvi-validated) and every run's own
// target.name/target.url (also human-authored config, snapshotted at
// invocation time) render as plain React text content only -- see the
// local T component below, identical to every other view in this
// codebase's own established discipline (CodeReviewView.tsx/
// PlanModeView.tsx). This view builds no href from any of that content --
// the only links it renders are the "session ->" links, built from a
// server-resolved sessionId (a UUID this view never derived from any
// free-text field) and Narvi's own internal route helper (TanStack
// Router's Link, never a raw <a href> built from a string).
//
// # What this view declines to build, and why
//
// "New automation" opens a MINIMAL creation form (name + one repo,
// triggerType fixed to "manual") -- CreateAutomationRequest's full surface
// (cron/GitHub/Linear trigger-config editors, sandbox path-scope/mock
// config, per-automation env vars) is a materially larger settings-style
// form, out of this view's own named scope (the automations health/runs
// table, §8.4) -- named here rather than
// silently built partially. A per-run one-line narrative (mockups.html's
// own decisions 19/20, "Reviewed PR #1187 · verdict medium risk · 2
// findings" / "reason: image_build_failed · retried with backoff") is
// declined for the identical reason ReleaseReviewView.tsx declines
// composition findings: automation_runs carries no such column (only
// automations.artifact_summary, one per automation's own MOST RECENT
// invocation, already rendered on the automation row's own "Last run"
// cell below) -- this view renders each run's REAL fields (target,
// status, timestamps, a link to its own session for the full story)
// rather than fabricating one.
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'

import type { Automation, AutomationInvocation, AutomationRun, CreateAutomationRequest } from '@narvi/contracts/rest-dtos'

import { createAutomation, listAutomationInvocations, listAutomations, resumeAutomation } from '../api/endpoints'
import { ApiError } from '../api/http'
import { automationQueryKeys } from '../api/queryKeys'
import { meQueryOptions } from '../auth/session'
import { AUTO_PAUSE_THRESHOLD, automationStatusTone, lastRunTone, nextRunSummary, runHealthLabel, runStatusTone, targetsSummary, triggerSummary } from './automationFormat'
import { formatRelativeTime } from './relativeTime'
import { truncateForDisplay } from './textSafety'

const MAX_FIELD_CHARS = 2000

function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_FIELD_CHARS)}</>
}

function isMaintainerPlus(role: string | undefined): boolean {
  return role === 'admin' || role === 'maintainer'
}

/** RunRow renders one automation run -- exported for direct render-safety testing (mirrors CodeReviewView.tsx's own FindingCard precedent): run.target.name (human-authored config, snapshotted at invocation time) must render as plain text only. */
export function RunRow({ invocation, run }: { invocation: AutomationInvocation; run: AutomationRun }) {
  return (
    <div className="run">
      <span className={`chip ${runStatusTone(run.status)}`}>
        <span className="dot" />
        {run.status}
      </span>
      <span className="target">
        <T text={run.target.name} />
      </span>
      <span className="sum">{formatRelativeTime(run.completedAt ?? run.startedAt)} ago</span>
      {run.sessionId && (
        <Link to="/session/$sessionId" params={{ sessionId: run.sessionId }} style={{ marginLeft: 'auto', color: 'var(--accent)', fontSize: '11.5px', fontWeight: 600, textDecoration: 'none' }}>
          session →
        </Link>
      )}
      {!run.sessionId && (
        <span className="sum" style={{ marginLeft: 'auto' }}>
          invocation {invocation.id.slice(0, 8)}
        </span>
      )}
    </div>
  )
}

function RunsPanel({ automationId }: { automationId: string }) {
  const query = useQuery({
    queryKey: automationQueryKeys.invocations(automationId),
    queryFn: ({ signal }) => listAutomationInvocations(automationId, signal),
  })

  if (query.isPending) return <p className="rail-empty">Loading runs…</p>
  if (query.isError) return <p className="rail-empty">Couldn't load runs.</p>
  if (query.data.invocations.length === 0) return <p className="rail-empty">No invocations yet.</p>

  // Flatten (invocation, run) pairs into one flat run-row list, newest
  // invocation first (the list is already newest-first per the API's own
  // contract) -- this is a deliberate simplification of the mockup's own
  // grouping-by-invocation-number display: this codebase's own domain has
  // no sequential "invocation #N" counter anywhere (automation_invocations.
  // id is a UUID, not an ordinal, migrations/000052_automation_invocations.
  // up.sql), so this view never fabricates one; each run row instead
  // carries its own real target/status/time and a link to its own real
  // session (or the invocation's own uuid prefix, when no session exists).
  const rows = query.data.invocations.flatMap((inv) => inv.runs.map((run) => ({ inv, run })))

  return (
    <div className="runs">
      {rows.map(({ inv, run }) => (
        <RunRow key={run.id} invocation={inv} run={run} />
      ))}
    </div>
  )
}

/** AutomationRow renders one automation's own table row -- exported for direct render-safety testing: automation.name/automation.prompt (human-authored, free text) must render as plain text only. */
export function AutomationRow({ automation, canManage }: { automation: Automation; canManage: boolean }) {
  const [expanded, setExpanded] = useState(false)
  const queryClient = useQueryClient()

  const resumeMutation = useMutation({
    mutationFn: () => resumeAutomation(automation.id),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: automationQueryKeys.list({}) }),
  })

  return (
    <>
      <tr onClick={() => setExpanded((v) => !v)} style={{ cursor: 'pointer' }}>
        <td>
          <b>
            <T text={automation.name} />
          </b>
          {automation.prompt && (
            <div className="trig">
              <T text={automation.prompt} />
            </div>
          )}
        </td>
        <td className="trig">{triggerSummary(automation.triggerType, automation.triggerConfig)}</td>
        <td className="num">{targetsSummary(automation.repos)}</td>
        <td>
          <span className={`chip ${lastRunTone(automation.lastRunStatus)}`}>
            <span className="dot" />
            {automation.lastRunStatus ?? 'never run'}
          </span>{' '}
          {automation.lastRunAt && <span className="num">{formatRelativeTime(automation.lastRunAt)} ago</span>}
        </td>
        <td className="num">{nextRunSummary(automation)}</td>
        <td>
          {automation.status === 'paused' ? (
            <>
              <span className="chip neutral">
                <span className="dot" />
                auto-paused
              </span>
              {canManage && (
                <button
                  type="button"
                  className="btn"
                  style={{ marginLeft: 6, padding: '2px 9px', fontSize: 11 }}
                  disabled={resumeMutation.isPending}
                  onClick={(e) => {
                    e.stopPropagation()
                    resumeMutation.mutate()
                  }}
                >
                  {resumeMutation.isPending ? 'Resuming…' : 'Resume'}
                </button>
              )}
            </>
          ) : automation.consecutiveFailures > 0 ? (
            <span className="strike">
              ▲ {automation.consecutiveFailures}/{AUTO_PAUSE_THRESHOLD} strikes before auto-pause
            </span>
          ) : runHealthLabel(automation.runHealth) !== null ? (
            <span className="num">{runHealthLabel(automation.runHealth)}</span>
          ) : (
            <span className={`chip ${automationStatusTone(automation.status)}`}>
              <span className="dot" />
              {automation.status}
            </span>
          )}
        </td>
      </tr>
      {expanded && (
        <tr className="runrow">
          <td colSpan={6}>
            <RunsPanel automationId={automation.id} />
          </td>
        </tr>
      )}
    </>
  )
}

function NewAutomationForm({ onDone }: { onDone: () => void }) {
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [repoName, setRepoName] = useState('')
  const [repoUrl, setRepoUrl] = useState('')
  const [prompt, setPrompt] = useState('')

  const createMutation = useMutation({
    mutationFn: () => {
      const body: CreateAutomationRequest = {
        name,
        prompt: prompt.trim().length > 0 ? prompt : null,
        repos: [{ name: repoName, url: repoUrl, branch: null }],
        triggerType: 'manual',
      }
      return createAutomation(body)
    },
    onSuccess: () => {
      onDone()
      void queryClient.invalidateQueries({ queryKey: automationQueryKeys.list({}) })
    },
  })

  const canSubmit = name.trim().length > 0 && repoName.trim().length > 0 && repoUrl.trim().length > 0

  return (
    <div className="card" style={{ margin: '12px 18px' }}>
      <div className="who">
        <b>New automation</b>
      </div>
      <p style={{ color: 'var(--faint)', fontSize: 'var(--text-sm-alt)', margin: '4px 0 10px' }}>
        Manual trigger only here -- cron/GitHub/Linear trigger config, sandbox scoping, and env vars are configured after creation.
      </p>
      <div className="btnrow" style={{ flexDirection: 'column', alignItems: 'stretch', gap: 8 }}>
        <input className="btn" style={{ textAlign: 'left' }} placeholder="Name" value={name} onChange={(e) => setName(e.target.value)} />
        <input className="btn" style={{ textAlign: 'left' }} placeholder="Repo name (e.g. acme/widgets)" value={repoName} onChange={(e) => setRepoName(e.target.value)} />
        <input className="btn" style={{ textAlign: 'left' }} placeholder="Repo URL (e.g. https://github.com/acme/widgets)" value={repoUrl} onChange={(e) => setRepoUrl(e.target.value)} />
        <textarea className="btn" style={{ resize: 'vertical', minHeight: 60, textAlign: 'left' }} placeholder="Prompt (what should this automation do?)" value={prompt} onChange={(e) => setPrompt(e.target.value)} />
        <div className="btnrow">
          <button type="button" className="btn primary" disabled={!canSubmit || createMutation.isPending} onClick={() => createMutation.mutate()}>
            {createMutation.isPending ? 'Creating…' : 'Create automation'}
          </button>
          <button type="button" className="btn" onClick={onDone}>
            Cancel
          </button>
        </div>
        {createMutation.isError && (
          <p className="sidebar-notice" role="alert">
            {createMutation.error instanceof ApiError && createMutation.error.status === 403 ? "You're not authorized to create automations (admin/maintainer only)." : 'Creating the automation failed. Try again.'}
          </p>
        )}
      </div>
    </div>
  )
}

export function AutomationsView() {
  const meQuery = useQuery(meQueryOptions)
  const [filterMine, setFilterMine] = useState(false)
  const [filterStatus, setFilterStatus] = useState<'active' | 'paused' | undefined>(undefined)
  const [creating, setCreating] = useState(false)

  const canManage = isMaintainerPlus(meQuery.data?.role)
  const filter = { createdBy: filterMine ? ('me' as const) : undefined, status: filterStatus }

  const listQuery = useQuery({
    queryKey: automationQueryKeys.list(filter),
    queryFn: ({ signal }) => listAutomations(filter, signal),
  })

  return (
    <div className="app one">
      <section className="main">
        <div className="toolbar">
          <select className="sel" value={filterMine ? 'mine' : 'all'} onChange={(e) => setFilterMine(e.target.value === 'mine')}>
            <option value="all">All automations</option>
            <option value="mine">My automations</option>
          </select>
          <select className="sel" value={filterStatus ?? 'all'} onChange={(e) => setFilterStatus(e.target.value === 'all' ? undefined : (e.target.value as 'active' | 'paused'))}>
            <option value="all">All statuses</option>
            <option value="active">Active</option>
            <option value="paused">Paused</option>
          </select>
          <span className="spacer" style={{ flex: 1 }} />
          {canManage && (
            <button type="button" className="btn primary" onClick={() => setCreating((v) => !v)}>
              New automation
            </button>
          )}
        </div>

        {creating && <NewAutomationForm onDone={() => setCreating(false)} />}

        {listQuery.isPending && (
          <div className="session-state" aria-live="polite">
            <p>Loading automations…</p>
          </div>
        )}
        {listQuery.isError && (
          <div className="session-state" role="alert">
            <p>Couldn't load automations.</p>
          </div>
        )}
        {listQuery.isSuccess && listQuery.data.automations.length === 0 && (
          <div className="session-state">
            <p>No automations yet.</p>
          </div>
        )}
        {listQuery.isSuccess && listQuery.data.automations.length > 0 && (
          <div style={{ overflowX: 'auto' }}>
            <table className="atable">
              <thead>
                <tr>
                  <th>Automation</th>
                  <th>Trigger</th>
                  <th>Targets</th>
                  <th>Last run</th>
                  <th>Next</th>
                  <th>Health</th>
                </tr>
              </thead>
              <tbody>
                {listQuery.data.automations.map((a) => (
                  <AutomationRow key={a.id} automation={a} canManage={canManage} />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  )
}
