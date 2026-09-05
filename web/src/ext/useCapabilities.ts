// useCapabilities.ts (technical plan §34, docs/design/boundaries-design.md
// section 4.3) is the ONE query definition every ext/ slot/affordance
// shares -- mirrors auth/session.ts's own meQueryOptions "share one
// definition, share one cache entry" reasoning exactly: a slot fallback
// and a future private screen both consulting GET /api/capabilities must
// never race each other into two independent requests, and must never be
// able to disagree about state (docs/design/boundaries-design.md, section
// 4.5, item 3: "the wire read model is the one both the affordance and
// the private screens consult, so they cannot disagree about state").
import { useQuery } from '@tanstack/react-query'

import { getCapabilities } from '../api/endpoints'
import { capabilityQueryKeys } from '../api/queryKeys'

// eightHoursMs is useCapabilities' own staleTime. Deliberately long,
// unlike meQueryOptions' own 60s: the value here changes only at this
// deployment's own restart (a different binary composed, or a different
// NARVI_LICENSE_KEY) or when the configured licence's own expiry passes
// -- neither event a client-side poll could usefully anticipate, and
// unlike a signed-in session (which can be revoked mid-use, meQueryOptions'
// own reason for a short staleTime), there is no "notice it promptly"
// requirement here: a stale read still resolves correctly the next time
// this query actually refetches (a fresh page load, an explicit
// invalidation), so this is chosen to be comfortably longer than any
// single browser tab is likely to stay open on one uninterrupted render.
const eightHoursMs = 8 * 60 * 60 * 1000

/**
 * useCapabilities reads GET /api/capabilities -- readable by every role
 * including viewer server-side (authz.ActionViewCapabilities), so this
 * hook needs no role check of its own before calling it.
 */
export function useCapabilities() {
  return useQuery({
    queryKey: capabilityQueryKeys.list(),
    queryFn: ({ signal }) => getCapabilities(signal),
    staleTime: eightHoursMs,
  })
}
