// IdentityStatusPanel (§12.2 item 7's "identity auto-link status
// panel", mockups.html's own `.linknote`/`.idrow`/`.idchip`) renders the
// SIGNED-IN caller's own identity graph, straight off GET /api/me's
// restdtos.Member (Member.identities, contracts/gen/ts/rest-dtos.ts) --
// never a hand-written shape (check-no-dto-redeclaration.mjs would flag
// one anyway). The actual per-provider derivation (deriveIdentityStatuses)
// and caption text (linkedViaCaption) live in auth/identityStatus.ts, not
// here -- see that module's own doc comment for why (a plain-function unit
// test, and keeping this file component-exports-only).
//
// # Third-party-controlled strings rendered here
//
// member.displayName (sourced from the signing-in user's own GitHub
// profile `name` field, callback.go's own githubUser.Name -- entirely
// user-controlled on GitHub's side) and member.email are both rendered
// as plain JSX text interpolation (`{member.displayName}`), which React
// escapes by construction; this file uses no dangerouslySetInnerHTML
// anywhere, and no other library renders markup on this panel's behalf.
import type { Member } from '@narvi/contracts/rest-dtos'

import { deriveIdentityStatuses, linkedViaCaption } from '../../auth/identityStatus'
import '../../styles/identity.css'

export interface IdentityStatusPanelProps {
  member: Member
  /**
   * oidcConfigured (review round 1, finding O12) is the SAME
   * GET /auth/capabilities signal the sign-in view's own SSO button
   * already gates on -- passed down explicitly rather than re-fetched
   * here, so this component stays a plain, render-independent function of
   * its own props (deriveIdentityStatuses's own doc comment: no affordance
   * for a capability this deployment doesn't offer).
   */
  oidcConfigured: boolean
}

export function IdentityStatusPanel({ member, oidcConfigured }: IdentityStatusPanelProps) {
  const statuses = deriveIdentityStatuses(member.identities, oidcConfigured)

  return (
    <div className="linknote">
      <span className="lt">Identities link themselves</span>
      {statuses.map((status) => (
        <span className="idrow" key={status.provider}>
          <span className={status.connected ? 'idchip ok' : 'idchip'}>
            {status.provider} {status.connected ? '✓' : '–'}
          </span>
          {status.connected && status.linkedVia ? linkedViaCaption(status.linkedVia) : 'not connected'}
        </span>
      ))}
    </div>
  )
}
