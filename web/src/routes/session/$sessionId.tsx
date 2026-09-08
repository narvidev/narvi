// The session workspace (§12.2 item 1): sidebar (status chips, session
// source, My sessions/All filter, boot n/m) + the typed-event timeline
// (sub-task lane nesting, failure cards + Resume). Driven by §12.1's own
// WS pipeline (useSessionStream) rather than fetching independently --
// this route is that pipeline's first real consumer.
//
// Singular "/session/$sessionId", not the plural "/sessions/...": the
// PLURAL prefix is server-reserved (internal/adapters/inbound/webui/
// mount.go's own protectedPrefixes -- GET /sessions/{sessionID}/ws, §6.2's
// sandbox/client WS upgrade, lives there) and Mount's own r.NotFound
// handler 404s ANY path under it rather than ever falling back to the SPA
// shell, on purpose (doc.go's own "never let a typo'd/removed API route
// silently start serving the SPA instead" guarantee) -- confirmed live
// during this Step's own browser verification pass (the plural form
// 404'd for exactly this reason before this route was renamed).
import { useMemo } from 'react'
import { createFileRoute } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import type { Session } from '@narvi/contracts/rest-dtos'

import { requireAuth } from '../../auth/requireAuth'
import { getSession } from '../../api/endpoints'
import { ApiError } from '../../api/http'
import { sessionQueryKeys } from '../../api/queryKeys'
import { buildCostRollup } from '../../session/costRollup'
import { Composer } from '../../session/Composer'
import { parseParticipants } from '../../session/participants'
import { parseSandboxSnapshot } from '../../session/sandboxSnapshot'
import { buildSandboxRailModel } from '../../session/sandboxRail'
import { SessionHeader } from '../../session/SessionHeader'
import { SessionRail } from '../../session/SessionRail'
import { SessionSidebar } from '../../session/SessionSidebar'
import { isStillBooting } from '../../session/sessionStatus'
import { Timeline } from '../../session/Timeline'
import { buildTimelineModel } from '../../session/timelineModel'
import { useSessionStream } from '../../session/useSessionStream'
import '../../styles/session.css'

export const Route = createFileRoute('/session/$sessionId')({
  beforeLoad: requireAuth,
  component: SessionWorkspace,
})

function SessionWorkspace() {
  const { sessionId } = Route.useParams()
  const queryClient = useQueryClient()

  const sessionQuery = useQuery({
    queryKey: sessionQueryKeys.detail(sessionId),
    queryFn: ({ signal }) => getSession(sessionId, signal),
    // A 404 here is a definitive "this session does not exist" answer,
    // not a transient failure -- TanStack Query's own default (retry 3x
    // with backoff) would otherwise leave the "Loading session…" state
    // showing for several extra seconds on every genuinely-missing
    // session before the error state ever appears (confirmed live during
    // this Step's own browser verification pass: a bogus session id sat
    // on "Loading session…" for ~7s). Mirrors auth/session.ts's own
    // meQueryOptions precedent for the identical reasoning on a 401.
    retry: (failureCount, error) => {
      if (error instanceof ApiError && error.status === 404) return false
      return failureCount < 3
    },
  })
  const stream = useSessionStream(sessionId)
  const model = useMemo(() => buildTimelineModel(stream.events), [stream.events])
  const sandboxModel = useMemo(() => buildSandboxRailModel(stream.events, parseSandboxSnapshot(stream.sandboxState)), [stream.events, stream.sandboxState])
  const costModel = useMemo(() => buildCostRollup(stream.events), [stream.events])
  const participants = useMemo(() => parseParticipants(stream.participantsState), [stream.participantsState])
  const hasOpenTurn = model.turns.length > 0 && (model.turns[model.turns.length - 1]?.live ?? false)

  return (
    <div className={sessionQuery.isSuccess ? 'app' : 'app no-rail'}>
      <SessionSidebar />
      <section className="main">
        {sessionQuery.isPending && (
          <div className="session-state" aria-live="polite">
            <p>Loading session…</p>
          </div>
        )}

        {sessionQuery.isError && (
          <div className="session-state" role="alert">
            {sessionQuery.error instanceof ApiError && sessionQuery.error.status === 404 ? (
              <p>This session doesn't exist, or you don't have access to it.</p>
            ) : (
              <p>Couldn't load this session. This is a connection problem, not a missing session.</p>
            )}
            <button
              type="button"
              className="btn"
              onClick={() => void queryClient.invalidateQueries({ queryKey: sessionQueryKeys.detail(sessionId) })}
            >
              Try again
            </button>
          </div>
        )}

        {sessionQuery.isSuccess && (
          <>
            <SessionHeader session={sessionQuery.data} model={model} cost={costModel} participants={participants} />
            {stream.syncState === 'syncing' && (
              <div className="sync-banner" role="status">
                Syncing session history…
              </div>
            )}
            {(stream.connectionStatus === 'reconnecting' || stream.connectionStatus === 'closed') && (
              <div className="sync-banner sync-banner-warn" role="status">
                {stream.connectionStatus === 'reconnecting' ? 'Reconnecting…' : 'Disconnected — reconnecting shortly.'}
              </div>
            )}

            <SessionTimelineBody
              sessionId={sessionId}
              model={model}
              bootPhase={model.latestBootPhase}
              sawReady={model.sawReady}
              sessionStatus={sessionQuery.data.status}
            />
            <Composer sessionId={sessionId} sandboxStatus={sandboxModel.status} hasOpenTurn={hasOpenTurn} />
          </>
        )}
      </section>
      {sessionQuery.isSuccess && <SessionRail sessionId={sessionId} sandbox={sandboxModel} cost={costModel} />}
    </div>
  )
}

function SessionTimelineBody({
  sessionId,
  model,
  bootPhase,
  sawReady,
  sessionStatus,
}: {
  sessionId: string
  model: ReturnType<typeof buildTimelineModel>
  bootPhase: string | null
  sawReady: boolean
  sessionStatus: Session['status']
}) {
  const hasContent = model.turns.length > 0 || model.warnings.length > 0 || model.errors.length > 0

  if (!hasContent) {
    if (isStillBooting(sessionStatus, sawReady)) {
      return (
        <div className="session-state" aria-live="polite">
          <p>{bootPhase ? `Sandbox is booting — ${bootPhase}…` : 'Sandbox is booting…'}</p>
        </div>
      )
    }
    return (
      <div className="session-state">
        <p>No events yet.</p>
      </div>
    )
  }

  return (
    <>
      {model.errors.map((e) => (
        <div key={e.id} className="banner banner-crit">
          {e.message}
        </div>
      ))}
      {model.warnings.map((w) => (
        <div key={w.id} className="banner banner-warn">
          {w.message}
        </div>
      ))}
      <Timeline sessionId={sessionId} turns={model.turns} />
    </>
  )
}
