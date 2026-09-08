// AnalyticsView.tsx -- §12.2 items 5-6's own Analytics screen.
//
// # What renders as real data now, and the definitions that make it honest
//
// mockups.html's own Analytics view draws 5 platform-wide KPI tiles
// (Sessions, Success rate, False failures, Cost, Boot p95) and 4 charts
// (Sessions per day, Cost by model, Review finding outcomes, Top failure
// reasons). All nine now render real, computed data: the review-risk
// section (KPI tiles, trend chart, top-risk table, finding outcomes) fed
// by the existing repo-scoped GetReviewAnalytics read model, and the
// five platform-wide tiles plus the remaining three charts fed by a NEW,
// platform-wide (never cross-repository, never cross-deployment) read
// model, GET /api/analytics (internal/app/platformanalytics). Every
// number below is computable from THIS deployment's own sessions/turns/
// events/false_failures rows alone.
//
// A few definitions were genuinely ambiguous and had to be picked rather
// than found -- stated here so a hostile reading has somewhere to check:
//
//   - "Sessions" counts EVERY session row created in the window,
//     including one nobody ever dispatched a turn for. "False failures"
//     (watchdog kills later proven alive, target 0) is likewise a plain
//     count. Both are meaningful even at a real, computed zero -- but a
//     failed backend fetch is not a computed zero, so both still carry
//     their own sessionsTotalComputed/falseFailureCountComputed sentinel
//     and render "not available" rather than a false 0 when that fetch
//     failed (the very defect this Step's own fix comment names).
//   - "Success rate" is completed / (completed + failed) -- a cancelled
//     session is a human's own decision, not a system judgment, and an
//     active/just-created one has no outcome yet, so both are excluded
//     from the rate entirely (matching the "Review finding outcomes"
//     KPI's own "computed only over definitively-resolved findings" rule
//     one section up).
//   - "Boot p95" is the 95th percentile of every SUCCESSFUL boot's own
//     measured duration -- a failed boot's elapsed time does not
//     represent normal latency, so it is excluded. Below a minimum
//     sample count a percentile is not a trustworthy estimate of
//     anything, so the tile stays "not available" even when a handful of
//     real samples exist -- distinguishable on screen from genuinely
//     zero samples via the sample count itself.
//   - "Top failure reasons" uses the four typed reasons this deployment's
//     schema actually persists (cancelled/failed/timeout/never started),
//     not the more granular free-text examples the mockup draws --
//     inventing labels this system cannot actually attribute a session to
//     would be exactly the kind of untrustworthy number this Step exists
//     to eliminate.
//
// Both repo-scoped sections (review-risk analytics, digest scope) are
// unchanged from before this Step and stay repo-scoped (the underlying
// REST routes are GET /api/repos/:owner/:repo/...), so this view still
// takes a plain owner/repo text input rather than a dropdown -- mirroring
// AutomationsView.tsx's own free-text repo entry precedent (no
// repo-enumeration endpoint exists for a client to populate a dropdown
// from; a caller who does not already know a repo name types it,
// exactly like reposettings.go's own resolveKnownRepo -- an unknown repo
// 404s honestly rather than ever being silently offered as a choice).
import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'

import type { PlatformAnalytics } from '@narvi/contracts/rest-dtos'

import { getPlatformAnalytics, getRepoDigestScope, getReviewAnalytics } from '../api/endpoints'
import { ApiError } from '../api/http'
import { platformAnalyticsQueryKeys, repoAnalyticsQueryKeys, repoDigestScopeQueryKeys } from '../api/queryKeys'
import { lookbackDaysLabel } from './settingsFormat'
import { truncateForDisplay } from './textSafety'

const TAG_LABEL: Record<string, string> = {
  auth: 'auth',
  migrations: 'migrations',
  contracts: 'contracts',
  secrets: 'secrets',
  infra: 'infra',
  public_api: 'public API',
  data_layer: 'data layer',
  dependencies: 'dependencies',
}

const STATUS_LABEL: Record<string, string> = {
  open: 'open',
  rebutted: 'rebutted',
  fix_pending: 'fix pending',
  fix_open: 'fix open',
  fix_merged: 'fix merged',
  fix_applied: 'fix applied',
}

// FAILURE_REASON_LABEL/FAILURE_REASON_DETAIL name the four REAL typed
// reasons session_failure_reason persists (internal/domain/session's own
// FailureReason consts) -- see this file's own top comment for why these
// four, and not the mockup's more granular free-text examples.
const FAILURE_REASON_LABEL: Record<string, string> = {
  cancelled: 'cancelled',
  failed: 'failed',
  timeout: 'timeout',
  never_started: 'never started',
}
const FAILURE_REASON_DETAIL: Record<string, string> = {
  cancelled: 'stopped by a user',
  failed: 'the agent itself reported failure',
  timeout: 'the turn deadline expired before completion',
  never_started: 'abandoned before a turn ever dispatched',
}

const MAX_FIELD_CHARS = 500
function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_FIELD_CHARS)}</>
}

function NotAvailableTile({ label, detail }: { label: string; detail?: string }) {
  return (
    <div className="tile">
      <span className="tl">{label}</span>
      <span className="big" style={{ color: 'var(--faint)', fontSize: 15 }}>
        {detail ?? 'not available yet'}
      </span>
    </div>
  )
}

function ReviewRiskSection({ owner, repo }: { owner: string; repo: string }) {
  const enabled = owner.trim().length > 0 && repo.trim().length > 0
  const repoFullName = `${owner}/${repo}`
  const query = useQuery({
    queryKey: repoAnalyticsQueryKeys.reviewAnalytics(repoFullName),
    queryFn: ({ signal }) => getReviewAnalytics(owner, repo, signal),
    enabled,
    retry: false,
  })

  if (!enabled) return <p style={{ color: 'var(--faint)', fontSize: 'var(--text-sm)' }}>Enter a repo above to load its review-risk analytics.</p>
  if (query.isPending) return <p className="rail-empty">Loading…</p>
  if (query.isError && query.error instanceof ApiError && query.error.status === 404) return <p className="notavailable">Repo not known to this deployment yet.</p>
  if (query.isError) return <p className="rail-empty">Couldn't load review analytics.</p>

  const data = query.data
  const maxDriverCount = data.topRiskDriversComputed && data.topRiskDrivers ? Math.max(1, ...data.topRiskDrivers.map((d) => d.count)) : 1
  const totalOutcomes = data.findingOutcomesComputed && data.findingOutcomes ? data.findingOutcomes.reduce((sum, o) => sum + o.count, 0) : 0

  return (
    <div className="charts2">
      <div className="chart">
        <h4>Shippable classification, per day</h4>
        <p className="ch">the timeseries rollup</p>
        {!data.timeseriesComputed && <p className="notavailable">Not available yet -- no review verdicts posted for this repo in the analytics window.</p>}
        {data.timeseriesComputed && data.timeseries && (
          <>
            <div className="legend">
              <span>
                <i style={{ background: 'var(--ok)' }} /> auto
              </span>
              <span>
                <i style={{ background: 'var(--warn)' }} /> needs human
              </span>
              <span>
                <i style={{ background: 'var(--crit)' }} /> block
              </span>
            </div>
            <div className="cols" role="img" aria-label="Daily verdict classification">
              {data.timeseries.map((b) => {
                const total = Math.max(1, b.autoCount + b.needsHumanCount + b.blockCount)
                const scale = 90 / total
                return (
                  <div className="col" key={b.day} title={`${b.day}: ${b.autoCount} auto, ${b.needsHumanCount} needs human, ${b.blockCount} block`}>
                    <div className="seg s-ok" style={{ height: `${b.autoCount * scale}px` }} />
                    <div className="seg s-crit" style={{ height: `${b.blockCount * scale}px` }} />
                    <div className="seg s-neu" style={{ height: `${b.needsHumanCount * scale}px`, background: 'var(--warn)' }} />
                  </div>
                )
              })}
            </div>
          </>
        )}
      </div>

      <div className="chart">
        <h4>Top risk drivers</h4>
        <p className="ch">the top-risk-driver breakdown</p>
        {!data.topRiskDriversComputed && <p className="notavailable">Not available yet -- no review verdicts posted for this repo in the analytics window.</p>}
        {data.topRiskDriversComputed && data.topRiskDrivers && data.topRiskDrivers.length === 0 && <p style={{ color: 'var(--faint)', fontSize: 'var(--text-sm)' }}>Verdicts exist, but none tagged a risk driver.</p>}
        {data.topRiskDriversComputed && data.topRiskDrivers && data.topRiskDrivers.length > 0 && (
          <div className="hbars">
            {data.topRiskDrivers.map((d) => (
              <div className="hbar" key={d.tag}>
                <span className="hl">{TAG_LABEL[d.tag] ?? d.tag}</span>
                <span className="track">
                  <span className="fill" style={{ width: `${(d.count / maxDriverCount) * 100}%` }} />
                </span>
                <span className="hv">{d.count}</span>
              </div>
            ))}
          </div>
        )}
      </div>

      <div className="chart">
        <h4>Review · finding outcomes</h4>
        <p className="ch">the "Review finding outcomes" KPI</p>
        {!data.findingOutcomesComputed && <p className="notavailable">Not available yet -- no review findings reported for this repo in the analytics window.</p>}
        {data.findingOutcomesComputed && data.findingOutcomes && data.findingOutcomes.length > 0 && (
          <>
            <div className="outcomebar" role="img" aria-label="Finding outcome distribution">
              {data.findingOutcomes.map((o) => (
                <i key={o.status} style={{ width: `${(o.count / Math.max(1, totalOutcomes)) * 100}%`, background: 'var(--accent)' }} title={`${STATUS_LABEL[o.status] ?? o.status} · ${o.count}`} />
              ))}
            </div>
            <div className="olabels">
              {data.findingOutcomes.map((o) => (
                <span key={o.status}>
                  <i style={{ background: 'var(--accent)' }} />
                  {STATUS_LABEL[o.status] ?? o.status} {o.count}
                </span>
              ))}
            </div>
          </>
        )}
      </div>

      <div className="chart">
        <h4>Digest contestation rate</h4>
        <p className="ch">the "digest precision" KPI -- deep-path arch recaps only</p>
        {!data.digestContestationRateComputed && <p className="notavailable">Not available yet -- no deep-path verdicts posted for this repo in the analytics window.</p>}
        {data.digestContestationRateComputed && data.digestContestationRatePercent !== null && <div className="tile" style={{ border: 'none', padding: 0 }}>
          <span className="big">{data.digestContestationRatePercent.toFixed(1)}%</span>
        </div>}
      </div>
    </div>
  )
}

function DigestScopeSection({ owner, repo }: { owner: string; repo: string }) {
  const enabled = owner.trim().length > 0 && repo.trim().length > 0
  const repoFullName = `${owner}/${repo}`
  const query = useQuery({
    queryKey: repoDigestScopeQueryKeys.detail(repoFullName),
    queryFn: ({ signal }) => getRepoDigestScope(owner, repo, signal),
    enabled,
    retry: false,
  })

  if (!enabled) return null
  if (query.isPending) return <p className="rail-empty">Loading digest scope…</p>
  if (query.isError && query.error instanceof ApiError && query.error.status === 404) return null
  if (query.isError) return <p className="rail-empty">Couldn't load digest scope.</p>

  const data = query.data
  return (
    <div className="chart">
      <h4>Digest scope</h4>
      <p className="ch">
        Sent daily on a fixed schedule. The recipients below are derived from which channels threaded a review session for this repository in the last {lookbackDaysLabel(data.lookbackDays)}; they are not configurable
        (§21.3).
      </p>
      {data.slackChannelIds.length === 0 && data.linearOrganizationIds.length === 0 && <p style={{ color: 'var(--faint)', fontSize: 'var(--text-sm)' }}>No Slack channel or Linear organization has threaded a review session for this repository recently, so nothing is in scope to receive its digest.</p>}
      {data.slackChannelIds.length > 0 && (
        <p>
          <b>Slack:</b> {data.slackChannelIds.map((id, i) => (
            <span key={id}>
              {i > 0 && ', '}
              <T text={id} />
            </span>
          ))}
        </p>
      )}
      {data.linearOrganizationIds.length > 0 && (
        <p>
          <b>Linear:</b> {data.linearOrganizationIds.map((id, i) => (
            <span key={id}>
              {i > 0 && ', '}
              <T text={id} />
            </span>
          ))}
        </p>
      )}
    </div>
  )
}

// SESSION_STATUS_COLOR maps the five real session_status values onto this
// app's own fixed 5-color semantic palette (tokens.css) -- completed=ok,
// failed=crit, cancelled=neutral (a human decision, not a system
// judgment -- the SAME reasoning successRate below excludes it for),
// active=warn (work still in flight), created=faint (barely started,
// the least prominent value on purpose). No new colors invented.
const SESSION_STATUS_COLOR: Record<string, string> = {
  completed: 'var(--ok)',
  failed: 'var(--crit)',
  cancelled: 'var(--neutral)',
  active: 'var(--warn)',
  created: 'var(--faint)',
}
const SESSION_STATUS_LABEL: Record<string, string> = {
  completed: 'completed',
  failed: 'failed',
  cancelled: 'cancelled',
  active: 'active',
  created: 'created',
}

function PlatformAnalyticsSection() {
  const query = useQuery({
    queryKey: platformAnalyticsQueryKeys.detail(),
    queryFn: ({ signal }) => getPlatformAnalytics(signal),
    retry: false,
  })

  // A loading/error KPI row still renders all five tile SLOTS (never
  // collapses the row to nothing), so the layout never jumps once real
  // data lands -- each slot just reads "not available yet" until then,
  // the same honest state a genuine sentinel produces. This is the
  // WHOLE-REQUEST failure path (network error, 401/403, 5xx before any
  // rollup ever ran) -- distinct from a PER-ROLLUP fetch failure inside
  // an otherwise-200 response, which PlatformAnalyticsBody's own
  // per-field Computed checks below handle instead.
  if (query.isPending || query.isError) {
    return (
      <>
        <div className="anav">
          <span className="ph">{query.isError ? "Couldn't load platform analytics" : 'Loading platform analytics…'}</span>
        </div>
        <div className="kpis">
          <NotAvailableTile label="Sessions" />
          <NotAvailableTile label="Success rate" />
          <NotAvailableTile label="False failures" />
          <NotAvailableTile label="Cost" />
          <NotAvailableTile label="Boot p95" />
        </div>
      </>
    )
  }

  return <PlatformAnalyticsBody data={query.data} />
}

// PlatformAnalyticsBody is the pure, presentational half of
// PlatformAnalyticsSection -- every tile/chart below is a function of
// `data` alone, no query involved, so a test can drive every rollup's
// own computed-or-not/error state directly (mutation-verify: force ONE
// rollup's own *Computed field to false, as a real fetch failure would
// leave it, and assert only THAT tile renders "not available", every
// other tile still rendering its real value). Exported for exactly that
// -- see analyticsRendering.test.tsx.
export function PlatformAnalyticsBody({ data }: { data: PlatformAnalytics }) {
  const maxModelCost = data.costByModelComputed && data.costByModel && data.costByModel.length > 0 ? Math.max(...data.costByModel.map((m) => m.totalUsd), 0.01) : 1
  const maxDayTotal =
    data.sessionsPerDayComputed && data.sessionsPerDay
      ? Math.max(1, ...data.sessionsPerDay.map((b) => b.completedCount + b.failedCount + b.cancelledCount + b.activeCount + b.createdCount))
      : 1

  return (
    <>
      <div className="anav">
        <span className="ph">{lookbackDaysLabel(data.windowDays).replace(/^./, (c) => c.toUpperCase())} · all sessions, this deployment</span>
        <span style={{ flex: 1 }} />
        <span className="cost">Platform-wide, computed live -- never cross-repository, never cross-deployment.</span>
      </div>

      <div className="kpis">
        {data.sessionsTotalComputed ? (
          <div className="tile">
            <span className="tl">Sessions</span>
            <span className="big">{data.sessionsTotal}</span>
            <span className="delta">every session created in the window</span>
          </div>
        ) : (
          <NotAvailableTile label="Sessions" detail="couldn't load this count" />
        )}

        {data.successRateComputed && data.successRatePercent !== null ? (
          <div className="tile">
            <span className="tl">Success rate</span>
            <span className="big">{data.successRatePercent.toFixed(1)}%</span>
            <span className="delta">of {data.successRateSampleSize} resolved sessions</span>
          </div>
        ) : (
          <NotAvailableTile label="Success rate" detail="no resolved sessions yet" />
        )}

        {data.falseFailureCountComputed ? (
          <div className="tile">
            <span className="tl">False failures</span>
            <span className="big">{data.falseFailureCount}</span>
            <span className="delta">target 0 · watchdog kills later proven alive</span>
          </div>
        ) : (
          <NotAvailableTile label="False failures" detail="couldn't load this count" />
        )}

        {data.costComputed && data.costTotalUsd !== null ? (
          <div className="tile">
            <span className="tl">Cost</span>
            <span className="big">${data.costTotalUsd.toFixed(2)}</span>
            <span className="delta">${(data.costMedianPerSessionUsd ?? 0).toFixed(2)} / session median (n={data.costSampleSize})</span>
          </div>
        ) : (
          <NotAvailableTile label="Cost" detail="no costed turns yet" />
        )}

        {data.bootP95Computed && data.bootP95Seconds !== null ? (
          <div className="tile">
            <span className="tl">Boot p95</span>
            <span className="big">{data.bootP95Seconds.toFixed(0)} s</span>
            <span className="delta">n={data.bootP95SampleSize} successful boots</span>
          </div>
        ) : (
          <NotAvailableTile label="Boot p95" detail={data.bootP95SampleSize > 0 ? `too few samples yet (n=${data.bootP95SampleSize})` : 'not available yet'} />
        )}
      </div>

      <div className="charts2">
        <div className="chart">
          <h4>Sessions per day</h4>
          <p className="ch">by outcome · {lookbackDaysLabel(data.windowDays)}</p>
          {!data.sessionsPerDayComputed && <p className="notavailable">Not available yet -- no sessions created in this window.</p>}
          {data.sessionsPerDayComputed && data.sessionsPerDay && (
            <>
              <div className="legend">
                {(['completed', 'failed', 'cancelled', 'active', 'created'] as const).map((s) => (
                  <span key={s}>
                    <i style={{ background: SESSION_STATUS_COLOR[s] }} /> {SESSION_STATUS_LABEL[s]}
                  </span>
                ))}
              </div>
              <div className="cols" role="img" aria-label="Stacked daily sessions by outcome">
                {data.sessionsPerDay.map((b) => {
                  // Scaled against the WINDOW's own tallest day (maxDayTotal),
                  // deliberately NOT each day's own total: this is a volume
                  // comparison across days ("which days were busy"), so a
                  // quiet day must draw visibly shorter than a busy one --
                  // unlike ReviewRiskSection's own per-day-normalized
                  // timeseries chart one section down, which draws each day's
                  // own COMPOSITION (proportion of auto/needs-human/block)
                  // and is correct to stretch every day to the same height.
                  const scale = 90 / maxDayTotal
                  return (
                    <div
                      className="col"
                      key={b.day}
                      title={`${b.day}: ${b.completedCount} completed, ${b.failedCount} failed, ${b.cancelledCount} cancelled, ${b.activeCount} active, ${b.createdCount} created`}
                    >
                      <div style={{ height: `${b.completedCount * scale}px`, background: SESSION_STATUS_COLOR.completed }} />
                      <div style={{ height: `${b.failedCount * scale}px`, background: SESSION_STATUS_COLOR.failed }} />
                      <div style={{ height: `${b.cancelledCount * scale}px`, background: SESSION_STATUS_COLOR.cancelled }} />
                      <div style={{ height: `${b.activeCount * scale}px`, background: SESSION_STATUS_COLOR.active }} />
                      <div style={{ height: `${b.createdCount * scale}px`, background: SESSION_STATUS_COLOR.created }} />
                    </div>
                  )
                })}
              </div>
            </>
          )}
        </div>

        <div className="chart">
          <h4>Cost by model</h4>
          <p className="ch">{lookbackDaysLabel(data.windowDays)}</p>
          {!data.costByModelComputed && <p className="notavailable">Not available yet -- no costed turns in this window.</p>}
          {data.costByModelComputed && data.costByModel && data.costByModel.length > 0 && (
            <div className="hbars">
              {data.costByModel.map((m) => (
                <div className="hbar" key={m.modelId}>
                  <span className="hl">{m.modelId}</span>
                  <span className="track">
                    <span className="fill" style={{ width: `${(m.totalUsd / maxModelCost) * 100}%` }} />
                  </span>
                  <span className="hv">${m.totalUsd.toFixed(2)}</span>
                </div>
              ))}
            </div>
          )}
        </div>

        <div className="chart">
          <h4>Top failure reasons</h4>
          <p className="ch">{lookbackDaysLabel(data.windowDays)} · typed reasons</p>
          {!data.topFailureReasonsComputed && <p className="notavailable">Not available yet -- no sessions in this window.</p>}
          {data.topFailureReasonsComputed && data.topFailureReasons && data.topFailureReasons.length === 0 && <p style={{ color: 'var(--faint)', fontSize: 'var(--text-sm)' }}>Sessions exist, none failed or were cancelled.</p>}
          {data.topFailureReasonsComputed && data.topFailureReasons && data.topFailureReasons.length > 0 && (
            <table className="ftable">
              <tbody>
                {data.topFailureReasons.map((fr) => (
                  <tr key={fr.reason}>
                    <td>{FAILURE_REASON_LABEL[fr.reason] ?? fr.reason}</td>
                    <td>{FAILURE_REASON_DETAIL[fr.reason] ?? ''}</td>
                    <td>{fr.count}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </>
  )
}

export function AnalyticsView() {
  const [owner, setOwner] = useState('')
  const [repo, setRepo] = useState('')

  return (
    <div className="app one">
      <section className="main">
        <div className="abody">
          <PlatformAnalyticsSection />

          <div className="panel">
            <h4>Review-risk analytics &amp; digest scope</h4>
            <p className="ph">Live per-repository figures.</p>
            <div className="formrow">
              <input placeholder="owner" value={owner} onChange={(e) => setOwner(e.target.value)} />
              <input placeholder="repo" value={repo} onChange={(e) => setRepo(e.target.value)} />
            </div>
            <ReviewRiskSection owner={owner} repo={repo} />
            <DigestScopeSection owner={owner} repo={repo} />
          </div>
        </div>
      </section>
    </div>
  )
}
