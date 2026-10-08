// AutonomyFreezePanel.tsx -- Settings → General's autonomy freeze card
// (technical plan §40.2): the platform-wide control an operator reaches for
// during an incident, instead of turning off auto-merge repository by
// repository and pausing automations one by one.
//
// Every role sees the freeze in force (GET /api/autonomy, authz.
// ActionViewSessions -- the decision inbox shows every role the same
// banner); an administrator also sees Freeze, with a required reason, and
// Unfreeze (authz.ActionManageAutonomyFreeze, admin only). The server is the
// authority: a non-admin's controls are hidden here and refused there.
//
// # Rendering safety
//
// freeze.reason (an administrator's free text) and freeze.
// frozenByDisplayName (a user-chosen name) render through the plain-text T
// component (truncateForDisplay), never dangerouslySetInnerHTML -- the
// reason textarea binds to a controlled input's value, which React never
// interprets as markup.
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { AutonomyFreeze } from '@narvi/contracts/rest-dtos'

import { getAutonomyFreeze, postFreezeAutonomy, postUnfreezeAutonomy } from '../api/endpoints'
import { ApiError } from '../api/http'
import { autonomyFreezeQueryKeys, decisionInboxQueryKeys } from '../api/queryKeys'
import { truncateForDisplay } from './textSafety'

const MAX_FIELD_CHARS = 500

/** MAX_FREEZE_REASON_CHARS is the longest reason a freeze keeps, after trimming -- the server refuses a longer one with a 400 that says so; this card stops the operator first. */
const MAX_FREEZE_REASON_CHARS = 500

function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_FIELD_CHARS)}</>
}

/**
 * AutonomyFreezeStatus renders the freeze read-only: running, or frozen
 * since when, by whom and why. Exported for direct render-safety testing
 * (__tests__/autonomyFreezePanel.test.tsx).
 */
export function AutonomyFreezeStatus({ freeze }: { freeze: AutonomyFreeze }) {
  if (!freeze.frozen) {
    return (
      <p className="ph">
        <span className="chip ok">
          <span className="dot" />
          not frozen
        </span>{' '}
        Automatic actions start as each repository and automation is configured.
      </p>
    )
  }
  const row = { display: 'flex', justifyContent: 'space-between', gap: 12, padding: '4px 0', fontSize: 'var(--text-sm)', borderBottom: '1px solid var(--line)' } as const
  return (
    <>
      <p className="sidebar-notice">
        <span className="chip warn">
          <span className="dot" />
          frozen
        </span>{' '}
        Nothing automatic starts: no auto-merge, auto-fix, automatic re-review, automation run or workflow advance. Each waits, and starts once the freeze is lifted. A person&rsquo;s own actions still work, and a turn already running
        finishes.
      </p>
      <div style={row}>
        <span style={{ color: 'var(--faint)' }}>Frozen since</span>
        <span>{freeze.frozenAt ? new Date(freeze.frozenAt).toLocaleString() : '—'}</span>
      </div>
      <div style={row}>
        <span style={{ color: 'var(--faint)' }}>By</span>
        <span>{freeze.frozenByDisplayName ? <T text={freeze.frozenByDisplayName} /> : 'a user who no longer exists'}</span>
      </div>
      <div style={row}>
        <span style={{ color: 'var(--faint)' }}>Reason</span>
        <span>
          <T text={freeze.reason ?? ''} />
        </span>
      </div>
    </>
  )
}

function MutationError({ error }: { error: unknown }) {
  return (
    <p className="sidebar-notice" role="alert">
      {error instanceof ApiError ? <T text={error.message} /> : 'Save failed. Try again.'}
    </p>
  )
}

/**
 * AutonomyFreezePanel is the card itself. role is the signed-in caller's
 * (GET /api/me); only an administrator is offered Freeze and Unfreeze. Both
 * answer the freeze in force, written straight into the cache, and the
 * decision inbox is refetched, since its banner and held rows change with
 * it. A 409 means another administrator froze or unfroze first, so the
 * cached freeze is stale: it is refetched, and the card shows the state the
 * server holds beside the server's own message.
 */
export function AutonomyFreezePanel({ role }: { role: string | undefined }) {
  const queryClient = useQueryClient()
  const [reason, setReason] = useState('')
  const isAdmin = role === 'admin'

  const query = useQuery({
    queryKey: autonomyFreezeQueryKeys.detail(),
    queryFn: ({ signal }) => getAutonomyFreeze(signal),
  })

  const settle = (updated: AutonomyFreeze) => {
    queryClient.setQueryData(autonomyFreezeQueryKeys.detail(), updated)
    void queryClient.invalidateQueries({ queryKey: decisionInboxQueryKeys.list() })
  }
  const refetchIfStale = (error: unknown) => {
    if (error instanceof ApiError && error.status === 409) {
      void queryClient.invalidateQueries({ queryKey: autonomyFreezeQueryKeys.detail() })
      void queryClient.invalidateQueries({ queryKey: decisionInboxQueryKeys.list() })
    }
  }

  const freeze = useMutation({
    mutationFn: () => postFreezeAutonomy({ reason: reason.trim() }),
    onSuccess: (updated) => {
      settle(updated)
      setReason('')
    },
    onError: refetchIfStale,
  })

  const unfreeze = useMutation({
    mutationFn: () => postUnfreezeAutonomy(),
    onSuccess: settle,
    onError: refetchIfStale,
  })

  const trimmed = reason.trim()

  return (
    <div className="panel">
      <h4>Autonomy freeze</h4>
      <p className="ph">platform-wide · audited · a person&rsquo;s own actions are never held</p>
      <p className="ph">
        Freezing stops every automatic action at once, on every replica: auto-merge, sentinel auto-fix, description rewrites, automatic re-reviews, automation runs and workflow advances. Nothing is lost: each held action starts once
        the freeze is lifted, within about a minute.
      </p>
      {query.isPending && <p className="rail-empty">Loading…</p>}
      {query.isError && <p className="rail-empty">Couldn&rsquo;t read whether autonomy is frozen. Every automatic action reads the freeze itself before it starts.</p>}
      {query.isSuccess && (
        <>
          <AutonomyFreezeStatus freeze={query.data} />
          {!isAdmin && (
            <p className="ph" style={{ marginTop: 4 }}>
              Admin only. Your role can see the freeze but not set or lift it -- enforced by the server, not merely hidden on this screen.
            </p>
          )}
          {isAdmin && query.data.frozen && (
            <div className="formrow" style={{ marginTop: 8 }}>
              <button type="button" className="btn primary" disabled={unfreeze.isPending} onClick={() => unfreeze.mutate()}>
                {unfreeze.isPending ? 'Unfreezing…' : 'Unfreeze'}
              </button>
            </div>
          )}
          {isAdmin && !query.data.frozen && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8, marginTop: 8 }}>
              <label className="ph" htmlFor="autonomy-freeze-reason">
                Reason (required, at most {MAX_FREEZE_REASON_CHARS} characters)
              </label>
              <textarea id="autonomy-freeze-reason" value={reason} maxLength={MAX_FREEZE_REASON_CHARS} rows={2} onChange={(e) => setReason(e.target.value)} />
              <div className="formrow">
                <button type="button" className="btn" disabled={freeze.isPending || trimmed.length === 0} onClick={() => freeze.mutate()}>
                  {freeze.isPending ? 'Freezing…' : 'Freeze autonomy'}
                </button>
              </div>
            </div>
          )}
          {freeze.isError && <MutationError error={freeze.error} />}
          {unfreeze.isError && <MutationError error={unfreeze.error} />}
        </>
      )}
    </div>
  )
}
