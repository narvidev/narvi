// GatekeeperAffordance.tsx is the public bundle's own permanent content
// for the "repo-settings.governance" slot (RepoSettingsView.tsx, in the
// card column, right after ShadowLedgerCard) -- technical plan §34,
// docs/design/boundaries-design.md, section 4.1: "in the public bundle
// nothing registers, so every slot renders either nothing or an honest
// affordance stating that the capability is part of Narvi Gatekeeper and
// why it is unavailable here."
//
// The idiom is sign-in.tsx's own SSO notice ("SSO becomes available once
// your organization configures it.") -- state a fact about configuration,
// never a hidden feature, and never with a section number or a Step
// number in view (internal/ops/operatorcopy.go scans JSX text nodes and
// string literals for exactly that leak).
//
// # What this component does not, and must not, claim
//
// Whether, or how, Narvi Gatekeeper is ever distributed is undecided
// (technical plan §34: "Neither exists yet and how either is distributed
// is undecided; nothing here depends on that answer"). Nothing below
// says sold, paid, priced, commercial or proprietary -- only that a
// capability is part of Narvi Gatekeeper, and whether this build/licence
// currently makes it usable.
import type { CapabilityState, CapabilityStatus } from '@narvi/contracts/rest-dtos'

import { formatDateTime } from '../session/settingsFormat'
import { useCapabilities } from './useCapabilities'

// governanceCapability is the one capability this slot describes -- the
// slot id's own name ("repo-settings.governance") names it, and
// internal/domain/license.CapabilityOrganizationGovernance's own wire
// value ("organization_governance") is the row this component looks up.
const governanceCapability = 'organization_governance' as const

/**
 * gatekeeperSentence renders exactly one of three honest sentences from a
 * capability's own state (docs/design/boundaries-design.md, section 4.3)
 * -- never a fourth. not_licensed/license_not_yet_valid/license_invalid
 * collapse to the SAME "no valid license configured" sentence: from an
 * operator's own point of view reading this screen, none of the three is
 * actionably different from the others (all three mean "nothing usable
 * is configured right now"), mirroring this codebase's own §34.5 "every
 * failure is a bare false" reasoning applied to what a human reads on
 * screen rather than to a boolean. "enabled" renders nothing: a
 * genuinely enabled capability means a private screen has already taken
 * this slot (ext/slots.tsx's own SlotOutlet), so this fallback rendering
 * a sentence at all would itself be the contradiction worth
 * investigating, never something to paper over with copy.
 */
export function gatekeeperSentence(state: CapabilityState, licenseExpiresAt: string | null): string | null {
  switch (state) {
    case 'not_installed':
      return 'Part of Narvi Gatekeeper. This build does not include it.'
    case 'not_licensed':
    case 'license_not_yet_valid':
    case 'license_invalid':
      return 'Part of Narvi Gatekeeper. Included in this build, but no valid license is configured.'
    case 'license_expired':
      return `Part of Narvi Gatekeeper. Included in this build, but its license expired on ${licenseExpiresAt ? formatDateTime(licenseExpiresAt) : 'an unknown date'}.`
    case 'enabled':
      return null
    default:
      return null
  }
}

/**
 * GatekeeperAffordanceForStatus renders gatekeeperSentence's own text for
 * one already-resolved CapabilityStatus -- exported for direct render-
 * safety/content testing, mirroring RepoSettingsSummary's own precedent
 * (RepoSettingsView.tsx) of separating a pure, prop-driven render from
 * the query-fetching wrapper below.
 */
export function GatekeeperAffordanceForStatus({ status, licenseExpiresAt }: { status: CapabilityStatus; licenseExpiresAt: string | null }) {
  const sentence = gatekeeperSentence(status.state, licenseExpiresAt)
  if (!sentence) return null
  return <p className="notavailable">{sentence}</p>
}

/**
 * GatekeeperAffordance fetches its own data (ext/useCapabilities.ts)
 * rather than taking it as a prop, since a SlotOutlet fallback is
 * composed with no way for its caller to already have this response on
 * hand. Loading/error states match this screen's own siblings (e.g.
 * ShadowLedgerCard's identical "rail-empty" loading/error copy).
 */
export function GatekeeperAffordance() {
  const query = useCapabilities()

  return (
    <div className="panel">
      <h4>Organization governance</h4>
      {query.isPending && <p className="rail-empty">Loading…</p>}
      {query.isError && <p className="rail-empty">Couldn&rsquo;t load capability status.</p>}
      {query.isSuccess && <GatekeeperAffordanceBody capabilities={query.data.capabilities} licenseExpiresAt={query.data.licenseExpiresAt} />}
    </div>
  )
}

function GatekeeperAffordanceBody({ capabilities, licenseExpiresAt }: { capabilities: CapabilityStatus[]; licenseExpiresAt: string | null }) {
  const status = capabilities.find((c) => c.name === governanceCapability)
  if (!status) return <p className="rail-empty">Couldn&rsquo;t load capability status.</p>
  return <GatekeeperAffordanceForStatus status={status} licenseExpiresAt={licenseExpiresAt} />
}
