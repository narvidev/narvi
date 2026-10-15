// autonomyFreezeFormat.ts -- the autonomy freeze's wording (technical plan
// §40.2), shared by the Settings card (AutonomyFreezePanel.tsx) and the
// decision inbox (DecisionInboxView.tsx), so both say the same thing.
import type { AutonomyFreeze } from '@narvi/contracts/rest-dtos'

import { formatRelativeTime } from './relativeTime'

/**
 * FREEZE_RESUME_BOUNDS is what a lifted freeze does not bring back, in
 * the words a person reads: §40.2's three bounds, one clause each. A
 * scheduled run is caught up only within its ten-minute catch-up window
 * (AutomationCronCatchUpWindow); the sentinel-fix merge gate runs once per
 * close event of the origin, so a merge it skipped while frozen is never
 * retried; and the auto-merge worker lists only verdicts newer than
 * AutoMergeCandidateLookback (seven days), so one that aged past it while
 * frozen is left to a person's Merge click. Every promise that held
 * actions start again carries this sentence, and the web guide, §40.2,
 * the runbook, the AutonomyFreeze contract and postUnfreezeAutonomy's doc
 * name the same three.
 */
export const FREEZE_RESUME_BOUNDS =
  "Three do not: a scheduled automation run held more than ten minutes waits for its next occurrence; a sentinel fix held from merging stays open for a person; and a pull request auto-approved more than seven days ago waits for a person's Merge click."

/**
 * frozenByText names who set the freeze: the administrator's display name,
 * or, when the server has no one on record -- that user no longer exists,
 * or the freeze was written by hand with no user, as an earlier release's
 * runbook did -- says exactly that, never that a user was deleted.
 */
export function frozenByText(freeze: Pick<AutonomyFreeze, 'frozenByDisplayName'>): string {
  return freeze.frozenByDisplayName ?? 'someone not on record'
}

/** heldAgoText is a held row's age: "held 3 min ago", or "held just now" for a hold under a minute old -- never "held just now ago". */
export function heldAgoText(heldAt: string, now: Date = new Date()): string {
  const relative = formatRelativeTime(heldAt, now)
  return relative === 'just now' ? 'held just now' : `held ${relative} ago`
}

/**
 * heldAdvancesCountText is the held-advances section's count: the list is
 * bounded at 100, oldest first, and when the bound cut it the count says
 * how many are held and that only the oldest are shown. total is undefined
 * from a server that predates it.
 */
export function heldAdvancesCountText(shown: number, total: number | undefined): string {
  if (total !== undefined && total > shown) {
    return `${total} held · the oldest ${shown} shown · workflow runs whose next step starts once autonomy is unfrozen`
  }
  return `${shown} · workflow runs whose next step starts once autonomy is unfrozen`
}
