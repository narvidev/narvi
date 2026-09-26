// MembersPanel.tsx -- Settings -> Members & access (§13.2/§13.3): role
// management, linked-identity chips, the audit log, and, per member, a
// "Connected apps" drawer (technical plan §43.18).
//
// # The Connected apps drawer
//
// mockups.html's Members view has no per-member drawer; its rows end in a
// small button (the "Audit log" one). The drawer follows that: a second
// button of the same kind beside "Audit log", opening an inset row under
// the member's own that renders ConnectedAppsSection.tsx's own
// MemberConnectedApps -- the same table, row and formatting the member's
// Connected apps section uses, never a second copy -- with revocation on
// the member's behalf behind the same confirmation. Admin only, like the
// rest of this panel; no token is ever shown (none exists in plaintext).
//
// # A real shape difference from the mockup, stated here rather than
// # silently reproduced
//
// mockups.html draws a "linear · pending" chip on the SAME row as a
// member's other linked identities. The real DTO shape does not support
// that: ListMembersResponse.pendingLinkPrompts is a FLAT, top-level list
// (PendingLinkPrompt{provider, externalId, expiresAt, createdAt}) with no
// userID field at all -- §13.2's own auto-linking design means a pending
// prompt has not yet been matched to any member (that happens once the
// verified email resolves it), so there is no real "this row's own
// pending identity" to draw. This panel renders pending prompts as their
// own separate list instead of fabricating a per-member association the
// data cannot support. The mockup's own "Resend link" button is likewise
// declined -- members.go exposes no resend endpoint (only list/
// role-change/manual link/unlink); a button with no real action behind
// it would be exactly the "plausible fiction" this Step's own
// instructions rule out.
//
// # Adversarial rendering
//
// Member.displayName/email and every AuditLogEntry field (action,
// resourceType, resourceId, detail) are rendered as plain text only (the
// T component below) or, for `detail` (an opaque per-action JSON blob,
// AuditLogEntry.detail's own doc comment: "Arbitrary per-action JSON
// detail"), through safeJsonPreview -- never markdown-parsed, never
// interpolated into an href.
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { AuditLogEntry, Member } from '@narvi/contracts/rest-dtos'

import { listAuditLog, listMembers, updateMemberRole } from '../api/endpoints'
import { ApiError } from '../api/http'
import { auditLogQueryKeys, memberQueryKeys } from '../api/queryKeys'
import { meQueryOptions } from '../auth/session'
import { MemberConnectedApps } from './ConnectedAppsSection'
import { formatDateTime, identityLinkProof, identityProviderLabel, roleTone } from './settingsFormat'
import { safeJsonPreview, truncateForDisplay } from './textSafety'

const MAX_FIELD_CHARS = 500
const ROLES = ['admin', 'maintainer', 'member', 'viewer'] as const

function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_FIELD_CHARS)}</>
}

/** MemberRow renders one member's own table row, and, once an admin opens it, the member's Connected apps drawer beneath -- exported for direct render-safety testing (mirrors AutomationsView.tsx's own AutomationRow precedent): member.displayName is admin-editable free text (github display name, §13.1), never Narvi-validated, and must render as plain text only. */
export function MemberRow({ member, canManage, onShowAudit }: { member: Member; canManage: boolean; onShowAudit: (userId: string) => void }) {
  const queryClient = useQueryClient()
  const [showApps, setShowApps] = useState(false)
  const roleMutation = useMutation({
    mutationFn: (role: string) => updateMemberRole(member.id, { role }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: memberQueryKeys.list() })
      // The audit log renders on THIS screen, directly below the members
      // table, and a role change writes a row to it (member.role_changed).
      // Invalidating only the member list left the panel showing "No entries"
      // straight after the change it had just recorded -- the one screen whose
      // subtitle promises every state change is audited was the screen that
      // failed to show it until a full reload.
      void queryClient.invalidateQueries({ queryKey: auditLogQueryKeys.list() })
    },
  })

  return (
    <>
      <tr>
        <td>
          <T text={member.displayName} /> {member.disabled && <span className="chip warn">disabled</span>}
        </td>
        <td>
          {canManage ? (
            <>
              <select className="sel-select" value={member.role} disabled={roleMutation.isPending} onChange={(e) => roleMutation.mutate(e.target.value)}>
                {ROLES.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
              {/*
                A rejected role change used to be completely silent: the select
                is bound to the server's own member.role, so it just snapped
                back with nothing rendered anywhere. UpdateMemberRole refuses
                deliberately and usefully -- 409 "cannot demote the last
                remaining admin" is the case an admin is most likely to hit,
                and the one where a silent revert looks like a broken dropdown.
                The server's message is a server-authored constant, never model
                or user text, but it still goes through the same plain-text
                path as every other string on this screen.
              */}
              {roleMutation.isError && <span className="rolefail">{roleMutation.error instanceof ApiError ? <T text={roleMutation.error.message} /> : 'Role change failed.'}</span>}
            </>
          ) : (
            <span className={`chip ${roleTone(member.role)}`}>
              <span className="dot" />
              {member.role}
            </span>
          )}
        </td>
        <td>
          {member.identities.length === 0 && <span style={{ color: 'var(--faint)' }}>none linked</span>}
          {member.identities.map((id) => {
            // The mark and tone come from linkedVia, never from a constant: see
            // identityLinkProof's own doc comment on why an admin force-link must
            // not wear the same check mark as a verified one.
            const proof = identityLinkProof(id.linkedVia)
            return (
              <span key={id.id} className={`idchip ${proof.tone}`} style={{ marginRight: 4 }} title={proof.title}>
                {identityProviderLabel(id.provider)} {proof.mark}
              </span>
            )
          })}
        </td>
        <td style={{ textAlign: 'right' }}>
          {canManage && (
            <>
              <button type="button" className="btn" style={{ padding: '2px 9px', fontSize: 11 }} aria-expanded={showApps} onClick={() => setShowApps(!showApps)}>
                Connected apps
              </button>{' '}
            </>
          )}
          <button type="button" className="btn" style={{ padding: '2px 9px', fontSize: 11 }} onClick={() => onShowAudit(member.id)}>
            Audit log
          </button>
        </td>
      </tr>
      {canManage && showApps && (
        <tr>
          <td colSpan={4} className="memberdrawercell">
            <MemberConnectedApps member={member} />
          </td>
        </tr>
      )}
    </>
  )
}

/** AuditLogRow renders one audit_log row -- exported for direct render-safety testing. action/resourceType/resourceId are server-controlled constants (never model/user free text) but resourceId can carry a caller-typed identifier (e.g. a repo full name) and detail is an OPAQUE per-action JSON blob (AuditLogEntry.detail's own doc comment: "Arbitrary per-action JSON detail") that can legitimately embed anything a request body once carried -- both render through the T/safeJsonPreview plain-text path, never markdown-parsed or interpolated into an href. */
export function AuditLogRow({ entry }: { entry: AuditLogEntry }) {
  return (
    <tr>
      <td>
        <T text={entry.action} />
      </td>
      <td>
        <T text={`${entry.resourceType} · ${entry.resourceId}`} />
      </td>
      <td style={{ fontFamily: 'var(--mono)', fontSize: 'var(--text-xs)', color: 'var(--muted)' }}>
        <T text={safeJsonPreview(entry.detail, 200)} />
      </td>
      <td style={{ fontFamily: 'var(--mono)', fontSize: 'var(--text-xs)', color: 'var(--faint)' }}>{entry.correlationId ? <T text={entry.correlationId} /> : '—'}</td>
      <td style={{ fontFamily: 'var(--mono)', fontSize: 'var(--text-xs)', color: 'var(--faint)' }}>{formatDateTime(entry.createdAt)}</td>
    </tr>
  )
}

// AUDIT_LOG_PAGE_SIZE is maxAuditLogPageSize (members.go), the largest page
// the server will serve. The endpoint has no actor filter, so the per-member
// view can only filter what has been fetched; asking for the biggest page
// keeps that window as wide as the server allows, and the pager below covers
// the rest. Anything smaller would narrow the member filter for no gain.
const AUDIT_LOG_PAGE_SIZE = 200

function AuditLogSection({ actorUserId, onClear }: { actorUserId: string | null; onClear: () => void }) {
  const [offset, setOffset] = useState(0)
  const query = useQuery({
    queryKey: auditLogQueryKeys.page(AUDIT_LOG_PAGE_SIZE, offset),
    queryFn: ({ signal }) => listAuditLog({ limit: AUDIT_LOG_PAGE_SIZE, offset }, signal),
    retry: false,
  })

  if (query.isPending) return <p className="rail-empty">Loading audit log…</p>
  if (query.isError && query.error instanceof ApiError && query.error.status === 403) {
    return <p className="notavailable">Audit log is admin-only.</p>
  }
  if (query.isError) return <p className="rail-empty">Couldn't load audit log.</p>

  const page = query.data.entries
  const entries: AuditLogEntry[] = actorUserId ? page.filter((e) => e.actorUserId === actorUserId) : page
  // A full page means there is at least potentially another one behind it.
  // The endpoint returns no total, so this is the only honest signal there is.
  const mayHaveOlder = page.length === AUDIT_LOG_PAGE_SIZE

  return (
    <div>
      {actorUserId && (
        <p className="ph">
          this member's actions within the events loaded below ·{' '}
          <button type="button" className="btn" style={{ padding: '2px 9px', fontSize: 11 }} onClick={onClear}>
            show all
          </button>
        </p>
      )}
      {/*
        The empty state must not read as "this member has never acted". The
        server has no actor filter, so what is filtered here is one page of
        platform-wide events; a member whose last action is older than that
        page produces exactly the same empty result as a member who has done
        nothing. Saying which one this is would be a claim the data cannot
        support, so the copy says what was actually searched and points at
        the pager instead.
      */}
      {entries.length === 0 &&
        (actorUserId ? (
          <p style={{ color: 'var(--faint)', fontSize: 'var(--text-sm)' }}>
            No actions by this member among the {page.length} event{page.length === 1 ? '' : 's'} loaded
            {offset > 0 ? ` (starting ${offset} back)` : ''}
            {mayHaveOlder ? ' — older events have not been loaded.' : '.'}
          </p>
        ) : (
          <p style={{ color: 'var(--faint)', fontSize: 'var(--text-sm)' }}>No entries.</p>
        ))}
      <table className="sectable">
        <thead>
          <tr>
            <th>Action</th>
            <th>Resource</th>
            <th>Detail</th>
            <th>Correlation</th>
            <th>When</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <AuditLogRow key={e.id} entry={e} />
          ))}
        </tbody>
      </table>
      {(offset > 0 || mayHaveOlder) && (
        <p className="ph">
          <button type="button" className="btn" style={{ padding: '2px 9px', fontSize: 11 }} disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - AUDIT_LOG_PAGE_SIZE))}>
            ← Newer
          </button>{' '}
          <button type="button" className="btn" style={{ padding: '2px 9px', fontSize: 11 }} disabled={!mayHaveOlder} onClick={() => setOffset(offset + AUDIT_LOG_PAGE_SIZE)}>
            Older →
          </button>
        </p>
      )}
    </div>
  )
}

export function MembersPanel() {
  const meQuery = useQuery(meQueryOptions)
  const isAdmin = meQuery.data?.role === 'admin'
  const [auditFilter, setAuditFilter] = useState<string | null>(null)

  const listQuery = useQuery({
    queryKey: memberQueryKeys.list(),
    queryFn: ({ signal }) => listMembers(signal),
    retry: false,
  })

  const forbidden = listQuery.isError && listQuery.error instanceof ApiError && listQuery.error.status === 403

  return (
    <>
      <div className="panel">
        <h4>Members &amp; access</h4>
        <p className="ph">roles enforced server-side · every state change audited with correlation id</p>
        {forbidden && <p className="notavailable">Members &amp; access is admin-only. Your role cannot view this panel -- enforced server-side (authz.ActionManageMembers), not merely hidden here.</p>}
        {!forbidden && listQuery.isPending && <p className="rail-empty">Loading members…</p>}
        {!forbidden && listQuery.isError && <p className="rail-empty">Couldn't load members.</p>}
        {!forbidden && listQuery.isSuccess && (
          <>
            <table className="sectable">
              <thead>
                <tr>
                  <th>Member</th>
                  <th>Role</th>
                  <th>Linked identities</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {listQuery.data.members.map((m) => (
                  <MemberRow key={m.id} member={m} canManage={isAdmin} onShowAudit={setAuditFilter} />
                ))}
              </tbody>
            </table>
            {listQuery.data.pendingLinkPrompts.length > 0 && (
              <div style={{ marginTop: 12 }}>
                <b style={{ fontSize: 'var(--text-sm)' }}>Pending identity links</b>
                <p className="ph">not yet matched to a member -- resolves automatically once the verified email matches</p>
                {listQuery.data.pendingLinkPrompts.map((p, i) => (
                  <span key={i} className="idchip pend" style={{ marginRight: 6 }}>
                    {identityProviderLabel(p.provider)} · pending (expires {formatDateTime(p.expiresAt)})
                  </span>
                ))}
              </div>
            )}
          </>
        )}
      </div>
      {!forbidden && listQuery.isSuccess && (
        <div className="panel">
          <h4>Audit log</h4>
          <AuditLogSection actorUserId={auditFilter} onClear={() => setAuditFilter(null)} />
        </div>
      )}
    </>
  )
}
