// IntegrationsPanel.tsx -- Settings -> Integrations (§12.5, §29.3/§29.9,
// §27.3/§27.8): every surface connecting this platform to something
// outside it, on one screen because they share one shape -- connect,
// verify, show liveness, disconnect -- and nothing else claims them.
//
// Three sections, three independently-gated backend surfaces, deliberately
// NOT unified into one query or one table:
//
//   1. Ingress integrations (Slack/Linear/GitHub, §12.5): admin-only
//      (authz.ActionManageIntegrations), read-only -- there is no
//      connect/disconnect WRITE here at all. A surface connects by
//      deploying its own configuration (the same posture §27.3's
//      cloud-identity capability already takes), so this section is a
//      table, never a form.
//   2. ChatGPT-account (Codex) linking (§29.3/§29.9): self-service,
//      own-user only (authz.ActionLinkChatGPTAccount, member+). The
//      link's own device-flow attempt expiry AND the refresh pump's
//      health are both surfaced explicitly -- a silently-expired link
//      degrades model availability with no other signal, and
//      ChatGPTLinkStatus.status folding needs_relink in as its own
//      distinct value (rather than a boolean "healthy" flag) IS how the
//      pump's own terminal failure state reaches this screen; see
//      restdtos.ChatGPTLinkStatus's own doc comment.
//   3. Connected apps and MCP clients (technical plan §43.15/§43.18):
//      ConnectedAppsSection.tsx -- every user's own connected MCP clients
//      (list, revoke), and the admin-only registration of the clients
//      that may ask at all. Its own file; see that file's doc comment.
//   4. Cloud-identity signing-key rotation (§27.3/§27.8, admin): fails
//      closed exactly like every other §27.3 surface -- relocated here
//      verbatim from EnvironmentsPanel.tsx (this Step's own "one screen
//      for every outside-connecting surface" mandate), which used to own
//      it alongside cloud-identity BINDINGS. Bindings (which cloud role an
//      Environment federates to) stay on EnvironmentsPanel -- they are
//      per-Environment/global CONFIG keyed by environments.id, the same
//      family as that panel's OpenCode config editor, not a "this
//      platform connects to an outside thing" surface in the sense the
//      other two sections above are. Signing-key rotation is the one
//      §27.3 sub-resource that is genuinely platform-wide and
//      connection-shaped (mint a fresh trust anchor, show the overlap
//      window while the old one is still trusted), so it moves here.
//      No GET for "the currently active signing key" exists on the wire
//      (only POST .../rotate, which returns what it just did) -- this
//      section therefore shows the OUTCOME of a rotation the operator
//      just triggered, never a page-load snapshot of "what key is active
//      right now" it has no way to ask for. That is a real, load-bearing
//      absence, not an oversight papered over client-side; see this
//      Step's own landing commit/report for why it is not invented here.
//
// # Adversarial rendering
//
// Integration.lastOutboundError is genuinely free text -- an upstream
// or internal error message that can carry anything a request/response
// body once carried -- and Integration.lastOutboundStatus is a plain
// string on the wire, not a schema enum (that DTO's own doc comment).
// ChatGPTLinkStatus.userCode/verificationUrl are server-supplied strings
// this client did not originate. All four render through the T
// (truncateForDisplay) plain-text path below, never markdown-parsed or
// interpolated into an href without isSafeHref's own scheme check first
// -- mirrors MembersPanel.tsx/EnvironmentsPanel.tsx's identical
// discipline for their own third-party-authored fields.
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { ChatGPTLinkStatus, Integration } from '@narvi/contracts/rest-dtos'

import { getChatGPTLinkStatus, getIntegrations, listCloudIdentityBindings, rotateCloudIdentitySigningKey, startChatGPTLink, unlinkChatGPTAccount } from '../api/endpoints'
import { ConnectedAppsSection, MCPClientsSection } from './ConnectedAppsSection'
import { ApiError } from '../api/http'
import { chatgptLinkQueryKeys, cloudIdentityBindingQueryKeys, integrationQueryKeys } from '../api/queryKeys'
import { meQueryOptions } from '../auth/session'
import { chatgptCardPresentation, chatgptLinkStatusPresentation, formatDateTime, integrationOutboundTone, integrationSurfaceLabel } from './settingsFormat'
import { truncateForDisplay } from './textSafety'
import { isSafeHref } from './urlSafety'

const MAX_FIELD_CHARS = 2000

function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_FIELD_CHARS)}</>
}

function isAdmin(role: string | undefined): boolean {
  return role === 'admin'
}

/** canLinkChatGPT mirrors authz.ActionLinkChatGPTAccount's own matrix row: admin/maintainer/member, never viewer (§13.3's own read-only row). */
function canLinkChatGPT(role: string | undefined): boolean {
  return role === 'admin' || role === 'maintainer' || role === 'member'
}

/** IntegrationRow renders one GET /api/integrations row -- exported for direct render-safety testing. lastOutboundError/lastOutboundStatus are server-supplied free text/plain-string fields (this file's own top doc comment) and render through T only. */
export function IntegrationRow({ integration }: { integration: Integration }) {
  return (
    <tr>
      <td>{integrationSurfaceLabel(integration.surface)}</td>
      <td>
        <span className={`chip ${integration.configured ? 'ok' : 'neutral'}`}>
          <span className="dot" />
          {integration.configured ? 'configured' : 'not configured'}
        </span>
      </td>
      <td>{integration.lastInboundAt ? formatDateTime(integration.lastInboundAt) : 'never received'}</td>
      <td>
        {integration.lastOutboundAt ? (
          <>
            <span className={`chip ${integrationOutboundTone(integration.lastOutboundStatus)}`}>
              <span className="dot" />
              <T text={integration.lastOutboundStatus ?? 'unknown'} />
            </span>{' '}
            {formatDateTime(integration.lastOutboundAt)}
          </>
        ) : (
          'no delivery attempt recorded'
        )}
      </td>
      <td>{integration.lastOutboundError ? <T text={integration.lastOutboundError} /> : '—'}</td>
    </tr>
  )
}

/** IngressIntegrationsSection -- GET /api/integrations (§12.5), admin-only, read-only: no connect/disconnect button anywhere in this section, since there is no write route behind one (this file's own top doc comment). */
function IngressIntegrationsSection() {
  const query = useQuery({
    queryKey: integrationQueryKeys.list(),
    queryFn: ({ signal }) => getIntegrations(signal),
    retry: false,
  })

  const forbidden = query.isError && query.error instanceof ApiError && query.error.status === 403

  return (
    <div className="panel">
      <h4>Ingress integrations</h4>
      <p className="ph">
        Slack, Linear and GitHub connect by deployment configuration, not a button here. Each row is a fact with a timestamp, never a health verdict -- a quiet surface and a broken one can look identical from here.
      </p>
      {forbidden && <p className="notavailable">Integrations are admin-only. Your role cannot view this panel -- enforced server-side, not merely hidden here.</p>}
      {!forbidden && query.isPending && <p className="rail-empty">Loading integrations…</p>}
      {!forbidden && query.isError && <p className="rail-empty">Couldn't load integrations.</p>}
      {!forbidden && query.isSuccess && (
        <table className="sectable">
          <thead>
            <tr>
              <th>Surface</th>
              <th>Status</th>
              <th>Last inbound</th>
              <th>Last outbound</th>
              <th>Last outbound error</th>
            </tr>
          </thead>
          <tbody>
            {query.data.integrations.map((integration) => (
              <IntegrationRow key={integration.surface} integration={integration} />
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}

// CHATGPT_LINK_POLL_INTERVAL_MS is this screen's own UI choice, not a value
// the wire returns: the server throttles the actual upstream attempt via
// chatgpt_link_attempts.last_polled_at against OpenAI's own device-flow
// interval (§29.3), so a client poll faster than that server-side throttle
// simply no-ops rather than double-spending attempts, and a slower one only
// delays how quickly this page notices a grant. 3s keeps the code-entry
// wait feeling responsive without hammering the endpoint.
const CHATGPT_LINK_POLL_INTERVAL_MS = 3000

/** ChatGPTLinkCard renders one ChatGPTLinkStatus -- exported for direct render-safety testing. userCode/verificationUrl are server-supplied strings (this file's own top doc comment); verificationUrl only ever becomes an href after isSafeHref's own scheme check. */
export function ChatGPTLinkCard({
  status,
  onStart,
  onUnlink,
  starting,
  unlinking,
}: {
  status: ChatGPTLinkStatus
  onStart: () => void
  onUnlink: () => void
  starting: boolean
  unlinking: boolean
}) {
  const presentation = chatgptLinkStatusPresentation(status.status)
  const href = status.verificationUrl ?? ''
  const hrefSafe = href.length > 0 && isSafeHref(href)

  return (
    <div>
      <span className={`chip ${presentation.tone}`}>
        <span className="dot" />
        {presentation.label}
      </span>

      {status.status === 'unlinked' && (
        <div className="btnrow">
          <button type="button" className="btn primary" disabled={starting} onClick={onStart}>
            {starting ? 'Starting…' : 'Connect ChatGPT account'}
          </button>
        </div>
      )}

      {status.status === 'pending' && (
        <div className="overlapwindow">
          <span>
            Open {hrefSafe ? (
              <a href={href} target="_blank" rel="noopener noreferrer">
                <T text={href} />
              </a>
            ) : (
              <T text={href} />
            )}{' '}
            and enter code{' '}
            <b>
              <T text={status.userCode ?? ''} />
            </b>
          </span>
          {status.expiresAt && <span>This code expires {formatDateTime(status.expiresAt)} -- start again after that if it lapses.</span>}
          <span>This page polls automatically while it stays open -- no separate action needed once the code is entered.</span>
        </div>
      )}

      {status.status === 'needs_relink' && (
        <div className="btnrow" style={{ flexDirection: 'column', alignItems: 'flex-start', gap: 8 }}>
          <p style={{ margin: 0 }}>The refresh pump could not renew this link and stopped serving it to any sandbox -- Codex models are unavailable under your seat until you reconnect.</p>
          <button type="button" className="btn primary" disabled={starting} onClick={onStart}>
            {starting ? 'Starting…' : 'Reconnect ChatGPT account'}
          </button>
        </div>
      )}

      {status.status === 'linked' && (
        <div className="btnrow">
          <button type="button" className="btn danger" disabled={unlinking} onClick={onUnlink}>
            {unlinking ? 'Disconnecting…' : 'Disconnect'}
          </button>
        </div>
      )}
    </div>
  )
}

/** ChatGPTLinkSection -- self-service device-flow link (§29.3/§29.9): start/poll/confirm, the device-flow attempt's own expiry, and the refresh pump's health (surfaced as the needs_relink status value, never a separate boolean). */
function ChatGPTLinkSection({ canLink }: { canLink: boolean }) {
  const queryClient = useQueryClient()
  const query = useQuery({
    queryKey: chatgptLinkQueryKeys.status(),
    queryFn: ({ signal }) => getChatGPTLinkStatus(signal),
    retry: false,
    enabled: canLink,
    // The human sitting on the page IS the polling loop (§29.3 point 2) --
    // only while an attempt is actually pending; unlinked/linked/needs_relink
    // are stable states this screen does not poll for on its own.
    refetchInterval: (q) => (q.state.data?.status === 'pending' ? CHATGPT_LINK_POLL_INTERVAL_MS : false),
  })
  const startMutation = useMutation({
    mutationFn: () => startChatGPTLink(),
    onSuccess: (data) => queryClient.setQueryData(chatgptLinkQueryKeys.status(), data),
  })
  const unlinkMutation = useMutation({
    mutationFn: () => unlinkChatGPTAccount(),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: chatgptLinkQueryKeys.status() }),
  })

  const forbidden = query.isError && query.error instanceof ApiError && query.error.status === 403
  const show = chatgptCardPresentation({ isPending: query.isPending, isError: query.isError, hasData: query.data !== undefined })

  return (
    <div className="panel">
      <h4>ChatGPT account (Codex)</h4>
      <p className="ph">Personal and subscription-tied -- links your own ChatGPT Plus/Pro account over the device flow so Codex models run under your seat. The control plane is the only refresher; a sandbox never sees the rotating token.</p>
      {!canLink && <p className="notavailable">Viewers cannot link a ChatGPT account.</p>}
      {canLink && forbidden && <p className="notavailable">Couldn't load your link status. Try again.</p>}
      {canLink && !forbidden && show.loading && <p className="rail-empty">Loading link status…</p>}
      {/*
        Render from the last status we have, NOT from query.isSuccess. While a
        device-flow attempt is pending this query polls every few seconds, and
        keying the card on isSuccess meant a single failed poll unmounted it --
        taking away the verification URL and the user code at the exact moment
        someone is typing them into another window, with no way to get them
        back but starting a new attempt. A failed refetch is a stale card, not
        a lost one: keep showing what we last knew and say so above it.
      */}
      {canLink && !forbidden && show.staleNotice && (
        <p className="sidebar-notice">Couldn't refresh your link status just now -- showing the last known state.</p>
      )}
      {canLink && !forbidden && show.error && (
        <p className="rail-empty">Couldn't load your ChatGPT link status.</p>
      )}
      {canLink && !forbidden && show.card && query.data !== undefined && (
        <ChatGPTLinkCard status={query.data} onStart={() => startMutation.mutate()} onUnlink={() => unlinkMutation.mutate()} starting={startMutation.isPending} unlinking={unlinkMutation.isPending} />
      )}
      {startMutation.isError && <p className="sidebar-notice">Couldn't start the link. Try again.</p>}
      {unlinkMutation.isError && <p className="sidebar-notice">Couldn't disconnect. Try again.</p>}
    </div>
  )
}

/**
 * SigningKeyRotationSection -- cloud-identity signing-key rotation
 * (§27.3/§27.8). Admin-only, destructive-adjacent: rendered behind an
 * explicit two-step confirm, never a bare button, and shows the JWKS
 * overlap window the rotation response itself returns. Renders NOTHING
 * at all (no affordance) when a 503 proves the capability is unconfigured
 * -- the SAME fail-closed posture RequireCloudIdentityCapability enforces
 * server-side (internal/adapters/inbound/httpapi/
 * cloudidentitycapability.go), discovered here by attempting the global
 * cloud-identity-bindings read (a route mounted behind that exact same
 * middleware, §27.3) rather than a second, dedicated status probe -- there
 * is no GET for "is this capability on" on its own, and there does not
 * need to be one: any route under that middleware answers the same
 * question with the same 503.
 *
 * Relocated verbatim from EnvironmentsPanel.tsx as part of this Step's own
 * "one screen for every outside-connecting surface" consolidation -- see
 * this file's own top doc comment for why signing-key rotation moves here
 * while cloud-identity BINDINGS stay on EnvironmentsPanel.
 */
function SigningKeyRotationSection({ canRotate }: { canRotate: boolean }) {
  const [confirming, setConfirming] = useState(false)
  const capabilityQuery = useQuery({
    queryKey: cloudIdentityBindingQueryKeys.list({ kind: 'global' }),
    queryFn: ({ signal }) => listCloudIdentityBindings({ kind: 'global' }, signal),
    retry: false,
  })
  const rotateMutation = useMutation({
    mutationFn: () => rotateCloudIdentitySigningKey(),
    onSuccess: () => setConfirming(false),
  })

  // Fail closed means the affordance appears only once the probe has
  // AFFIRMATIVELY answered, not merely when it has failed to say no. Keyed
  // on "is this an explicit 503", the button rendered while the probe was
  // still in flight and on every non-503 failure -- a network drop, a 500,
  // a timeout -- which is the state where least is known and the button is
  // least defensible. The server would still refuse the rotation, so this
  // was never an escalation; it is the affordance itself that §27.3 says
  // must not be there, because offering an operator a destructive-adjacent
  // action for a capability that may not exist is its own defect.
  const capabilityOn = capabilityQuery.isSuccess
  const capabilityOff = capabilityQuery.isError && capabilityQuery.error instanceof ApiError && capabilityQuery.error.status === 503
  if (capabilityOff) {
    return (
      <div className="panel">
        <h4>Cloud-identity signing-key rotation</h4>
        <p className="notavailable">
          Cloud identity federation is not configured on this deployment, so there is nothing to rotate.
        </p>
      </div>
    )
  }
  if (!canRotate) return null
  if (!capabilityOn) {
    // Still asking, or asked and could not find out. Either way this
    // deployment has not been shown to have the capability, so it gets no
    // rotation control -- only a line saying which of the two it is.
    return (
      <div className="panel">
        <h4>Cloud-identity signing-key rotation</h4>
        <p className="rail-empty">
          {capabilityQuery.isPending
            ? 'Checking whether cloud identity federation is configured…'
            : "Couldn't check whether cloud identity federation is configured, so no rotation is offered."}
        </p>
      </div>
    )
  }

  return (
    <div className="panel">
      <h4>Cloud-identity signing-key rotation</h4>
      <p className="ph">Rotates the trust anchor customer clouds federate to. The retired key keeps verifying tokens minted before rotation until the JWKS overlap window ends, and no longer after.</p>
      {!confirming && !rotateMutation.isSuccess && (
        <button type="button" className="btn danger" onClick={() => setConfirming(true)}>
          Rotate signing key
        </button>
      )}
      {confirming && (
        <div className="confirmbox">
          <p>
            This mints a fresh OIDC signing key and retires the current one after the JWKS overlap window. Every customer cloud federated to this issuer keeps trusting the retired key
            until then, but no LONGER than that -- this is a platform-wide, admin-only, destructive-adjacent action.
          </p>
          <div className="btnrow">
            <button type="button" className="btn danger" disabled={rotateMutation.isPending} onClick={() => rotateMutation.mutate()}>
              {rotateMutation.isPending ? 'Rotating…' : 'Confirm rotation'}
            </button>
            <button type="button" className="btn" onClick={() => setConfirming(false)}>
              Cancel
            </button>
          </div>
        </div>
      )}
      {rotateMutation.isError && <p className="sidebar-notice">Rotation failed. Try again.</p>}
      {rotateMutation.isSuccess && (
        <div className="overlapwindow">
          <span>
            active kid: <b>{rotateMutation.data.activeKid}</b> (since {formatDateTime(rotateMutation.data.activeCreatedAt)})
          </span>
          {rotateMutation.data.retiredKid && (
            <>
              <span>
                retired kid: <b>{rotateMutation.data.retiredKid}</b> (at {formatDateTime(rotateMutation.data.retiredAt ?? '')})
              </span>
              <span>JWKS overlap window ends: {formatDateTime(rotateMutation.data.publishableUntil ?? '')} -- the retired key verifies tokens minted before then, and no longer after.</span>
            </>
          )}
          {!rotateMutation.data.retiredKid && <span>First-ever rotation -- nothing to retire, no overlap window.</span>}
        </div>
      )}
    </div>
  )
}

export function IntegrationsPanel() {
  const meQuery = useQuery(meQueryOptions)

  return (
    <>
      <IngressIntegrationsSection />
      <ChatGPTLinkSection canLink={canLinkChatGPT(meQuery.data?.role)} />
      <ConnectedAppsSection />
      {isAdmin(meQuery.data?.role) && <MCPClientsSection />}
      <SigningKeyRotationSection canRotate={isAdmin(meQuery.data?.role)} />
    </>
  )
}
