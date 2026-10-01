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

// §26.6's amendment: what the counter-reviewer adds is fact-checked, or
// published marked unverified and counted apart. The server resolves it
// (ReviewReadoutFinding.additionCheck); these helpers only read that
// resolution, and never upgrade an unknown value to "checked".

/** isUnverifiedAddition reports whether a readout finding is a counter-review addition the server could not count as checked. Only 'checked' counts -- null or any value this client does not know is unverified, the server's own fail-conservative rule. A finding with any other source, or none recorded (last published before sources were recorded, or posted without one by a turn rendered before they were), is never one. */
export function isUnverifiedAddition(finding: { source?: string | null; additionCheck?: string | null }): boolean {
  return finding.source === 'counter_review' && finding.additionCheck !== 'checked'
}

/** additionCheckReason says why an addition is unverified, in the posted comment's own words (internal/domain/reviewpost's unverifiedAdditionReason). "Could not be confirmed" never claims what the trace holds. */
export function additionCheckReason(check: string | null | undefined): string {
  switch (check) {
    case 'not_run':
      return 'no fact-check run over it after the counter-review was reported'
    case 'not_found':
      return "a fact-check run over it was reported, but none that started after the counter-review and completed was found in this turn's trace when the verdict was posted"
    case 'unconfirmed':
      return "whether a fact-check ran over it could not be confirmed: this turn's trace could not be read in full when the verdict was posted"
    default:
      return 'no fact-check run over it was confirmed'
  }
}

/** additionsCheckLabel renders ReviewReadoutVerdict.additionsCheck -- the server's resolution of the counter-review additions the verdict published -- for the Sentinels panel; '—' when there was nothing to resolve. */
export function additionsCheckLabel(check: string | null | undefined): string {
  if (check === null || check === undefined || check === '') return '—'
  if (check === 'checked') return 'additions checked'
  if (check === 'unconfirmed') return 'additions unverified (could not be confirmed)'
  return 'additions unverified'
}

/** additionsRunSummary renders the Sentinels panel's second fact-check row: the reviewer's report and kill count, then the server's resolution of the additions the verdict published. A verdict that published none -- its second run removed every addition -- reads 'no addition published', never 'additions unverified': additionsCheck is null exactly then. '—' when there was neither a second run nor an addition. */
export function additionsRunSummary(
  verdict: { additionsFactCheck?: string | null; additionsFactCheckKilled?: number | null; additionsCheck?: string | null } | null | undefined,
): string {
  const reported = verdict?.additionsFactCheck ?? null
  const check = verdict?.additionsCheck ?? null
  if (!reported && !check) return '—'
  const killed = typeof verdict?.additionsFactCheckKilled === 'number' ? ` (${verdict.additionsFactCheckKilled} killed)` : ''
  return `${reported ?? 'not reported'}${killed} · ${check ? additionsCheckLabel(check) : 'no addition published'}`
}

/** findingSourceBucketLabel renders ReviewAnalyticsFindingSourceCount.source, the bucket the per-source finding breakdown counts under. */
export function findingSourceBucketLabel(source: string): string {
  switch (source) {
    case 'primary':
      return 'primary reviewer'
    case 'counter_review':
      return 'counter-review, checked'
    case 'counter_review_unverified':
      return 'counter-review, unverified'
    case 'not_recorded':
      return 'source not recorded'
    default:
      return source
  }
}
