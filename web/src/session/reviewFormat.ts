// reviewFormat.ts -- small, pure formatting/mapping helpers shared by
// CodeReviewView.tsx/ReleaseReviewView.tsx. No I/O, no rendering -- just
// the closed-enum -> chip-tone/label mappings every review-adjacent view
// needs, kept in one place so the two views can't independently drift on
// what "medium risk" or "needs_human" looks like.

export type ChipTone = 'ok' | 'warn' | 'crit' | 'neutral'

export function riskTone(riskLevel: string): ChipTone {
  if (riskLevel === 'high') return 'crit'
  if (riskLevel === 'medium') return 'warn'
  if (riskLevel === 'low') return 'ok'
  return 'neutral'
}

export function shippableTone(shippable: string): ChipTone {
  if (shippable === 'auto') return 'ok'
  if (shippable === 'needs_human') return 'warn'
  if (shippable === 'block') return 'crit'
  return 'neutral'
}

export function shippableLabel(shippable: string): string {
  if (shippable === 'auto') return 'ready to merge'
  if (shippable === 'needs_human') return 'needs human review'
  if (shippable === 'block') return 'blocked'
  return shippable
}

export function findingStatusTone(status: string): ChipTone {
  if (status === 'open') return 'crit'
  if (status === 'rebutted') return 'neutral'
  if (status === 'fix_merged' || status === 'fix_applied') return 'ok'
  if (status === 'fix_pending' || status === 'fix_open') return 'warn'
  return 'neutral'
}

export function findingStatusLabel(status: string): string {
  switch (status) {
    case 'open':
      return 'open'
    case 'rebutted':
      return 'rebutted'
    case 'fix_pending':
      return 'fix pending'
    case 'fix_open':
      return 'fix open'
    case 'fix_merged':
      return 'fix merged'
    case 'fix_applied':
      return 'fix applied'
    default:
      return status
  }
}

export function ciConclusionTone(conclusion: string): ChipTone {
  if (conclusion === 'success') return 'ok'
  if (conclusion === 'failure') return 'crit'
  return 'neutral'
}

export function descriptionAdequacyTone(adequacy: string): ChipTone {
  if (adequacy === 'ok') return 'ok'
  if (adequacy === 'drift') return 'warn'
  if (adequacy === 'misleading') return 'crit'
  return 'neutral'
}

// §12.2 item 2's own "the review readout's four missing fields" -- see
// CodeReviewView.tsx's own top comment for where each of these renders.

/** visualQaTone maps ReviewReadout.visualQa's own raw label suffix (human-authored external text, never Narvi-validated) to a chip tone -- "pass"/"skip"/"skipped" are recognized case-sensitively (the exact vocabulary §8/§12.2 name); anything else (a typo, a future value) renders neutral rather than guessing. */
export function visualQaTone(visualQa: string): ChipTone {
  if (visualQa === 'pass') return 'ok'
  if (visualQa === 'fail') return 'crit'
  if (visualQa === 'skip' || visualQa === 'skipped') return 'neutral'
  return 'neutral'
}

/** sentinelFixTone maps ReviewReadoutSentinelFix.status (sentinel_fixes.status, an unconstrained TEXT column, §17) to a chip tone. */
export function sentinelFixTone(status: string): ChipTone {
  if (status === 'fix_merged') return 'ok'
  if (status === 'abandoned') return 'neutral'
  return 'warn' // pending/spawned/fix_open -- still in flight.
}

/** sentinelFixLabel renders ReviewReadoutSentinelFix.status as the short phrase the rail shows next to the fix-PR link. */
export function sentinelFixLabel(status: string): string {
  switch (status) {
    case 'pending':
      return 'fix pending'
    case 'spawned':
      return 'fix session started'
    case 'fix_open':
      return 'fix open, merges automatically once this PR lands'
    case 'fix_merged':
      return 'fix merged'
    case 'abandoned':
      return 'fix abandoned (this PR closed unmerged)'
    default:
      return status
  }
}
