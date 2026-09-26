// ConnectedAppsSection.tsx -- Settings -> Integrations -> Connected apps
// (technical plan §43.15/§43.18): the MCP clients -- editor plugins,
// assistants -- a user allowed to use Narvi as them, and, for admins, the
// registered clients that may ask at all. Same connect/verify/liveness/
// disconnect shape as the ChatGPT-account card beside it.
//
// Three views, three independently-gated backend surfaces, one table:
//
//   1. ConnectedAppsSection: every role (authz.ActionViewOwnProfile to
//      list, authz.ActionRevokeOwnMCPAuthorization to revoke) -- the
//      caller's OWN authorizations only. Revoking deletes the
//      authorization server-side; the app's very next /mcp call is
//      refused. Behind an explicit confirm, never a bare button.
//   2. MemberConnectedApps: admin only (authz.ActionManageMembers) -- the
//      Members & access screen's per-member drawer (MembersPanel.tsx): the
//      very list that member sees in (1), through the same table and row,
//      and revocation on the member's behalf, audited server-side. Never a
//      token -- none exists in plaintext anywhere to show.
//   3. MCPClientsSection: admin only (authz.ActionManageIntegrations) --
//      register a client (the server generates its public client ID and
//      refuses any redirect URI the consent flow could not use), delete
//      one, which disconnects every user of it, or disable one -- every
//      user of it refused from its next call, nothing deleted -- and enable
//      it again. Disabling, not deleting, keeps out an app identified by its
//      description's address, which a deletion would let register again.
//
// # Adversarial rendering
//
// clientName and every redirect URI are admin-supplied strings. They
// render through T (plain text, truncated), never as markup and never as
// an href -- a redirect URI is where an app receives codes, not a link a
// person should follow from here.
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { MCPAuthorization, MCPClient, Member } from '@narvi/contracts/rest-dtos'

import {
  createMCPClient,
  deleteMCPClient,
  disableMCPClient,
  enableMCPClient,
  listMCPClients,
  listMemberMCPAuthorizations,
  listMyMCPAuthorizations,
  revokeMemberMCPAuthorization,
  revokeMyMCPAuthorization,
} from '../api/endpoints'
import { ApiError } from '../api/http'
import { auditLogQueryKeys, mcpAuthorizationQueryKeys, mcpClientQueryKeys } from '../api/queryKeys'
import { apiErrorMessage, clientIdentityLabel, parseRedirectUris, scopesSummary } from './connectedAppsFormat'
import { formatDateTime } from './settingsFormat'
import { truncateForDisplay } from './textSafety'

const MAX_FIELD_CHARS = 500

function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_FIELD_CHARS)}</>
}

/** ConnectedAppRow renders one of the caller's own MCP authorizations -- exported for direct render-safety testing. Revoke asks for confirmation first. */
export function ConnectedAppRow({ authorization, onRevoke, revoking }: { authorization: MCPAuthorization; onRevoke: () => void; revoking: boolean }) {
  const [confirming, setConfirming] = useState(false)
  return (
    <tr>
      <td>
        <b>
          <T text={authorization.clientName} />
        </b>
        <div className="ph">
          <T text={clientIdentityLabel(authorization.clientKind, authorization.clientId)} />
        </div>
      </td>
      <td>
        <T text={scopesSummary(authorization.scopes)} />
      </td>
      <td>{formatDateTime(authorization.createdAt)}</td>
      <td>{authorization.lastUsedAt ? formatDateTime(authorization.lastUsedAt) : 'never'}</td>
      <td>{formatDateTime(authorization.expiresAt)}</td>
      <td>
        {confirming ? (
          <span className="btnrow">
            <button type="button" className="btn danger" disabled={revoking} onClick={onRevoke}>
              {revoking ? 'Revoking…' : 'Confirm revoke'}
            </button>
            <button type="button" className="btn" onClick={() => setConfirming(false)}>
              Cancel
            </button>
          </span>
        ) : (
          <button type="button" className="btn danger" onClick={() => setConfirming(true)}>
            Revoke
          </button>
        )}
      </td>
    </tr>
  )
}

/** ConnectedAppsSection -- the caller's own connected MCP apps (every role). */
export function ConnectedAppsSection() {
  const queryClient = useQueryClient()
  const query = useQuery({
    queryKey: mcpAuthorizationQueryKeys.mine(),
    queryFn: ({ signal }) => listMyMCPAuthorizations(signal),
    retry: false,
  })
  const revokeMutation = useMutation({
    mutationFn: (authorizationId: string) => revokeMyMCPAuthorization(authorizationId),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: mcpAuthorizationQueryKeys.mine() }),
  })

  return (
    <div className="panel">
      <h4>Connected apps</h4>
      <p className="ph">
        MCP clients -- editor plugins and assistants -- you allowed to use Narvi as you. An app can never do more than your own role allows. Access shows your most recent approval; approving an app again never takes away access you allowed it before, so revoke to withdraw it. Revoking takes effect on the app's very next call.
      </p>
      {query.isPending && <p className="rail-empty">Loading connected apps…</p>}
      {query.isError && <p className="rail-empty">Couldn't load your connected apps.</p>}
      {query.isSuccess && query.data.authorizations.length === 0 && (
        <p className="rail-empty">No connected apps. An app appears here after you approve it on its consent page.</p>
      )}
      {query.isSuccess && query.data.authorizations.length > 0 && (
        <ConnectedAppsTable authorizations={query.data.authorizations} revokingId={revokeMutation.isPending ? revokeMutation.variables : undefined} onRevoke={(id) => revokeMutation.mutate(id)} />
      )}
      {revokeMutation.isError && <p className="sidebar-notice">{apiErrorMessage(revokeMutation.error, "Couldn't revoke. Try again.")}</p>}
    </div>
  )
}

/** ConnectedAppsTable renders a user's MCP authorizations, one ConnectedAppRow each -- the caller's own (ConnectedAppsSection) and, for an admin, a member's (MemberConnectedApps) go through this one table, so both say exactly the same thing about an app. Exported for direct render testing. */
export function ConnectedAppsTable({ authorizations, revokingId, onRevoke }: { authorizations: MCPAuthorization[]; revokingId: string | undefined; onRevoke: (authorizationId: string) => void }) {
  return (
    <table className="sectable">
      <thead>
        <tr>
          <th>App</th>
          <th>Access</th>
          <th>Connected</th>
          <th>Last used</th>
          <th>Expires</th>
          <th />
        </tr>
      </thead>
      <tbody>
        {authorizations.map((a) => (
          <ConnectedAppRow key={a.id} authorization={a} revoking={revokingId === a.id} onRevoke={() => onRevoke(a.id)} />
        ))}
      </tbody>
    </table>
  )
}

/**
 * MemberConnectedApps is the Members & access screen's per-member "Connected
 * apps" drawer (technical plan §43.18): an admin's view of one member's MCP
 * authorizations -- the list that member sees, never a token -- and a
 * Revoke behind the same confirmation, which disconnects the app on the
 * member's behalf from its very next call. The member can approve the app
 * again; keeping it out is disabling its client (MCPClientsSection), which
 * holds for every kind but an app that registered itself -- that one can
 * register again under a new client ID while self-registration is on.
 */
export function MemberConnectedApps({ member }: { member: Member }) {
  const queryClient = useQueryClient()
  const query = useQuery({
    queryKey: mcpAuthorizationQueryKeys.member(member.id),
    queryFn: ({ signal }) => listMemberMCPAuthorizations(member.id, signal),
    retry: false,
  })
  const revokeMutation = useMutation({
    mutationFn: (authorizationId: string) => revokeMemberMCPAuthorization(member.id, authorizationId),
    onSuccess: () => {
      // Every cached list: the member's, and the admin's own when an admin
      // revokes one of their own apps from here.
      void queryClient.invalidateQueries({ queryKey: mcpAuthorizationQueryKeys.all() })
      // The revocation is an audit row (mcp_authorization.revoked, reason
      // admin), and the audit log renders on this same screen.
      void queryClient.invalidateQueries({ queryKey: auditLogQueryKeys.list() })
    },
  })

  return (
    <div className="memberdrawer">
      <b>
        Connected apps · <T text={member.displayName} />
      </b>
      <p className="ph">
        The MCP apps this member allowed to use Narvi as them -- the same list they see under Integrations; no token is ever shown. Revoking disconnects the app on their behalf from its very next call, and is audited. They can approve it again. To keep the app out for everyone, disable its client under Integrations → MCP clients -- except an app that registered itself, which can register again under a new client ID while the deployment allows self-registration.
      </p>
      {query.isPending && <p className="rail-empty">Loading connected apps…</p>}
      {query.isError && <p className="rail-empty">{apiErrorMessage(query.error, "Couldn't load this member's connected apps.")}</p>}
      {query.isSuccess && query.data.authorizations.length === 0 && <p className="rail-empty">No connected apps.</p>}
      {query.isSuccess && query.data.authorizations.length > 0 && (
        <ConnectedAppsTable authorizations={query.data.authorizations} revokingId={revokeMutation.isPending ? revokeMutation.variables : undefined} onRevoke={(id) => revokeMutation.mutate(id)} />
      )}
      {revokeMutation.isError && <p className="sidebar-notice">{apiErrorMessage(revokeMutation.error, "Couldn't revoke. Try again.")}</p>}
    </div>
  )
}

/** MCPClientRow renders one registered MCP client (admin view) -- exported for direct render-safety testing. Delete disconnects every user of the client, and Disable refuses every one of them from its next call, so each asks for confirmation first and says so; Enable, which only lets a disabled client carry on, does not. */
export function MCPClientRow({
  client,
  onDelete,
  deleting,
  onSetDisabled,
  settingDisabled,
}: {
  client: MCPClient
  onDelete: () => void
  deleting: boolean
  onSetDisabled: (disabled: boolean) => void
  settingDisabled: boolean
}) {
  const [confirming, setConfirming] = useState<'delete' | 'disable' | null>(null)
  // The disable confirmation stays open while the request runs, and gives
  // way to the Enable button once the list shows the client disabled.
  const confirmingDisable = confirming === 'disable' && !client.disabledAt
  const idle = confirming === null || (confirming === 'disable' && client.disabledAt !== null)
  return (
    <tr>
      <td>
        <b>
          <T text={client.clientName} />
        </b>
        {client.disabledAt && (
          <div>
            <span className="chip neutral">
              <span className="dot" />
              disabled {formatDateTime(client.disabledAt)}
            </span>
          </div>
        )}
      </td>
      <td>
        <code>
          <T text={client.clientId} />
        </code>
      </td>
      <td>
        {client.redirectUris.map((uri) => (
          <div key={uri}>
            <code>
              <T text={uri} />
            </code>
          </div>
        ))}
      </td>
      <td>{formatDateTime(client.createdAt)}</td>
      <td>
        {confirming === 'delete' && (
          <div className="confirmbox">
            <p>Deleting this client disconnects every user of it.</p>
            <span className="btnrow">
              <button type="button" className="btn danger" disabled={deleting} onClick={onDelete}>
                {deleting ? 'Deleting…' : 'Confirm delete'}
              </button>
              <button type="button" className="btn" onClick={() => setConfirming(null)}>
                Cancel
              </button>
            </span>
          </div>
        )}
        {confirmingDisable && (
          <div className="confirmbox">
            <p>Disabling this client refuses every user of it from its next call. Nothing is deleted, and Enable undoes it.</p>
            <span className="btnrow">
              <button type="button" className="btn danger" disabled={settingDisabled} onClick={() => onSetDisabled(true)}>
                {settingDisabled ? 'Disabling…' : 'Confirm disable'}
              </button>
              <button type="button" className="btn" onClick={() => setConfirming(null)}>
                Cancel
              </button>
            </span>
          </div>
        )}
        {idle && (
          <span className="btnrow">
            {client.disabledAt ? (
              <button
                type="button"
                className="btn"
                disabled={settingDisabled}
                onClick={() => {
                  setConfirming(null)
                  onSetDisabled(false)
                }}
              >
                {settingDisabled ? 'Enabling…' : 'Enable'}
              </button>
            ) : (
              <button type="button" className="btn danger" onClick={() => setConfirming('disable')}>
                Disable
              </button>
            )}
            <button type="button" className="btn danger" onClick={() => setConfirming('delete')}>
              Delete
            </button>
          </span>
        )}
      </td>
    </tr>
  )
}

/** MCPClientsSection -- admin-only registration of the MCP clients that may ask this deployment's users for access. */
export function MCPClientsSection() {
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [redirects, setRedirects] = useState('')
  const [homepage, setHomepage] = useState('')
  const query = useQuery({
    queryKey: mcpClientQueryKeys.list(),
    queryFn: ({ signal }) => listMCPClients(signal),
    retry: false,
  })
  const createMutation = useMutation({
    mutationFn: () => {
      // The wire type is a non-empty list (minItems 1); canSubmit already
      // guarantees one, and this guard keeps the type honest about it.
      const [first, ...rest] = parseRedirectUris(redirects)
      if (first === undefined) throw new Error('at least one redirect URI is required')
      return createMCPClient({
        clientName: name.trim(),
        redirectUris: [first, ...rest],
        ...(homepage.trim().length > 0 ? { clientUri: homepage.trim() } : {}),
      })
    },
    onSuccess: () => {
      setName('')
      setRedirects('')
      setHomepage('')
      void queryClient.invalidateQueries({ queryKey: mcpClientQueryKeys.list() })
    },
  })
  const deleteMutation = useMutation({
    mutationFn: (clientId: string) => deleteMCPClient(clientId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: mcpClientQueryKeys.list() })
      // Deleting a client revokes every user's authorization of it: every
      // cached list, the caller's own and any member's, is stale.
      void queryClient.invalidateQueries({ queryKey: mcpAuthorizationQueryKeys.all() })
    },
  })
  // Disabling deletes no authorization, so only the clients list changes.
  const setDisabledMutation = useMutation({
    mutationFn: ({ clientId, disabled }: { clientId: string; disabled: boolean }) => (disabled ? disableMCPClient(clientId) : enableMCPClient(clientId)),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: mcpClientQueryKeys.list() }),
  })

  const forbidden = query.isError && query.error instanceof ApiError && query.error.status === 403
  const canSubmit = name.trim().length > 0 && parseRedirectUris(redirects).length > 0 && !createMutation.isPending

  return (
    <div className="panel">
      <h4>MCP clients</h4>
      <p className="ph">
        The apps that may ask this deployment's users for access. Configure the client ID below in the app; each user still approves it themselves. Redirect URIs must be https, or http on 127.0.0.1, [::1] or localhost -- the port of a 127.0.0.1 or [::1] URI may vary. Disabling a client refuses every user of it from its next call and deletes nothing; it is how to keep out an app identified by its description's address (a client ID that is an https URL), which a deletion would let register again.
      </p>
      {forbidden && <p className="notavailable">MCP client registration is admin-only -- enforced server-side, not merely hidden here.</p>}
      {!forbidden && (
        <div className="formrow">
          <input placeholder="App name" value={name} onChange={(e) => setName(e.target.value)} />
          <textarea placeholder="Redirect URIs, one per line" rows={3} value={redirects} onChange={(e) => setRedirects(e.target.value)} />
          <input placeholder="Homepage (optional, https)" value={homepage} onChange={(e) => setHomepage(e.target.value)} />
          <button type="button" className="btn primary" disabled={!canSubmit} onClick={() => createMutation.mutate()}>
            {createMutation.isPending ? 'Registering…' : 'Register client'}
          </button>
        </div>
      )}
      {createMutation.isError && <p className="sidebar-notice">{apiErrorMessage(createMutation.error, "Couldn't register the client.")}</p>}
      {createMutation.isSuccess && (
        <p className="overlapwindow">
          Registered. Client ID for the app:{' '}
          <code>
            <T text={createMutation.data.clientId} />
          </code>
        </p>
      )}
      {!forbidden && query.isPending && <p className="rail-empty">Loading clients…</p>}
      {!forbidden && query.isError && <p className="rail-empty">Couldn't load MCP clients.</p>}
      {!forbidden && query.isSuccess && query.data.clients.length === 0 && <p className="rail-empty">No MCP clients registered.</p>}
      {!forbidden && query.isSuccess && query.data.clients.length > 0 && (
        <table className="sectable">
          <thead>
            <tr>
              <th>Name</th>
              <th>Client ID</th>
              <th>Redirect URIs</th>
              <th>Registered</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {query.data.clients.map((c) => (
              <MCPClientRow
                key={c.id}
                client={c}
                deleting={deleteMutation.isPending && deleteMutation.variables === c.id}
                onDelete={() => deleteMutation.mutate(c.id)}
                settingDisabled={setDisabledMutation.isPending && setDisabledMutation.variables?.clientId === c.id}
                onSetDisabled={(disabled) => setDisabledMutation.mutate({ clientId: c.id, disabled })}
              />
            ))}
          </tbody>
        </table>
      )}
      {deleteMutation.isError && <p className="sidebar-notice">{apiErrorMessage(deleteMutation.error, "Couldn't delete the client.")}</p>}
      {setDisabledMutation.isError && <p className="sidebar-notice">{apiErrorMessage(setDisabledMutation.error, "Couldn't change whether the client is disabled.")}</p>}
    </div>
  )
}
