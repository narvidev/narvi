// decisionInboxFormat.ts -- small, pure formatting/mapping helpers for
// DecisionInboxView.tsx, mirroring reviewFormat.ts/automationFormat.ts's
// own "closed-enum -> chip-tone/label mappings, no I/O, no rendering"
// shape. Every function here is a pure function over already-typed
// restdtos.DecisionInboxItem fields -- nothing here calls the network or
// touches the DOM.
import type { DecisionInboxItem } from '@narvi/contracts/rest-dtos'

import type { ChipTone } from './reviewFormat'

// -- row "kind" tag (mockups.html's own `.qkind` column: pr/handoff/plan/
// session/automation/outbox/release) --
//
// This is DELIBERATELY a different axis from DecisionInboxItem.kind
// (ready_to_merge/needs_review/awaiting_approval/needs_attention, the
// SECTION a row lives in): mockups.html's own decision-inbox demo draws a
// "release" tag inside the needs_review section, alongside ordinary PR
// rows within that SAME kind=needs_review bucket -- distinguishing them
// needs `isRelease`, exactly the way distinguishing a handoff PR from an
// ordinary plan within kind=awaiting_approval needs `isHandoff`.
export type DecisionInboxRowKind = 'pr' | 'handoff' | 'release' | 'plan' | 'session' | 'automation' | 'outbox'

/**
 * rowKind derives the row's own type tag from which ID field the schema's
 * own per-kind field-population contract says is set (DecisionInboxItem's
 * own doc comment: "Only the fields relevant to `kind` are populated") --
 * never from `kind` itself, which cannot distinguish a handoff PR from an
 * ordinary plan within the SAME kind=awaiting_approval bucket (that is
 * exactly what `isHandoff` exists to answer, DecisionInboxItem.isHandoff's
 * own doc comment), nor a release cut from an ordinary PR within the SAME
 * kind=needs_review bucket (`isRelease`'s own identical role).
 */
export function rowKind(item: DecisionInboxItem): DecisionInboxRowKind {
  if (item.isHandoff) return 'handoff'
  if (item.isRelease) return 'release'
  if (item.repoFullName !== null) return 'pr'
  if (item.planId !== null) return 'plan'
  if (item.automationId !== null) return 'automation'
  if (item.outboxId !== null) return 'outbox'
  return 'session'
}

/**
 * provenanceText renders §16.1's own "a first-class field, not a UI
 * nicety" assignment provenance (decision 34: "every row says why it's
 * yours") -- null for any row provenanceKind itself is null on (every
 * plan/session/automation/outbox row, DecisionInboxItem.provenanceKind's
 * own doc comment), rendered as NOTHING by the caller, never a fabricated
 * "unknown".
 */
export function provenanceText(item: Pick<DecisionInboxItem, 'provenanceKind' | 'provenanceRepoFullName' | 'provenancePattern'>): string | null {
  switch (item.provenanceKind) {
    case 'assigned_directly':
      return 'assigned to you directly'
    case 'requested_reviewer':
      return item.provenanceRepoFullName ? `requested reviewer · ${item.provenanceRepoFullName}` : 'requested reviewer'
    case 'codeowners':
      return item.provenancePattern ? `yours via CODEOWNERS · ${item.provenancePattern}` : 'yours via CODEOWNERS'
    default:
      return null
  }
}

// -- risk-label chip (reviewpost.LabelLowRisk/MediumRisk/HighRisk's own
// wire values, "review:low-risk" etc -- verified directly against
// internal/domain/reviewpost/label.go, NOT the bare "low"/"medium"/"high"
// reviewFormat.ts's own riskTone expects for a ReviewReadoutVerdict's
// riskLevel, a structurally different field on a different DTO). --

export function riskLabelTone(riskLabel: string): ChipTone {
  if (riskLabel === 'review:high-risk') return 'crit'
  if (riskLabel === 'review:medium-risk') return 'warn'
  if (riskLabel === 'review:low-risk') return 'ok'
  return 'neutral'
}

/** riskLabelText renders the mockup's own "review: low risk" copy from the wire's "review:low-risk" label -- an unrecognized label (a future GitHub label this client predates) renders verbatim rather than throwing or silently disappearing. */
export function riskLabelText(riskLabel: string): string {
  if (riskLabel === 'review:high-risk') return 'review: high risk'
  if (riskLabel === 'review:medium-risk') return 'review: medium risk'
  if (riskLabel === 'review:low-risk') return 'review: low risk'
  return riskLabel
}

// -- age / decision-latency duration formatting --
//
// Both operate on a raw SECOND COUNT (DecisionInboxItem.ageSeconds,
// ListDecisionInboxResponse.decisionLatencyMedianSeconds), never an ISO
// timestamp -- relativeTime.ts's own formatRelativeTime takes an instant
// and diffs it against `now`, which is the wrong shape for a value the
// server already reduced to a duration.

/** formatAgeSeconds mirrors relativeTime.ts's own coarse "2 min"/"1 h"/"3 d" rounding (mockups.html's own `.qage` column), applied to a duration already in seconds rather than an instant to diff. */
export function formatAgeSeconds(totalSeconds: number): string {
  const seconds = Math.max(0, totalSeconds)
  const minutes = Math.floor(seconds / 60)
  if (minutes < 1) return 'just now'
  if (minutes < 60) return `${minutes} min`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} h`
  const days = Math.floor(hours / 24)
  return `${days} d`
}

/** formatDecisionLatencySeconds renders the header's own median time-to-decision (mockups.html: "median time-to-decision · 3.2 h") -- one decimal place once the unit is hours/days (a coarser "2 min"-style integer would lose exactly the precision this metric exists to show, §16.2: "the human bottleneck, made visible"). Caller-gated on decisionLatencyComputed -- see DecisionInboxView.tsx's own usage. */
export function formatDecisionLatencySeconds(totalSeconds: number): string {
  const seconds = Math.max(0, totalSeconds)
  const minutes = seconds / 60
  if (minutes < 60) return `${Math.round(minutes)} min`
  const hours = minutes / 60
  if (hours < 24) return `${hours.toFixed(1)} h`
  const days = hours / 24
  return `${days.toFixed(1)} d`
}

// -- section labels/blurbs (mockups.html's own `.qhead`/`.qcount`) --

export const SECTION_ORDER: readonly DecisionInboxItem['kind'][] = ['ready_to_merge', 'needs_review', 'awaiting_approval', 'needs_attention']

export function sectionTitle(kind: DecisionInboxItem['kind']): string {
  switch (kind) {
    case 'ready_to_merge':
      return 'Ready to merge'
    case 'needs_review':
      return 'Needs your review'
    case 'awaiting_approval':
      return 'Awaiting your approval'
    case 'needs_attention':
      return 'Needs attention'
  }
}

export function sectionBlurb(kind: DecisionInboxItem['kind']): string {
  switch (kind) {
    case 'ready_to_merge':
      return 'auto-approved by code review, CI green, assigned to you'
    case 'needs_review':
      return 'human judgment required'
    case 'awaiting_approval':
      return 'plans and handoffs you can act on'
    case 'needs_attention':
      return 'recoverable'
  }
}

/** canMergeDecisionInboxItem is this view's own CLIENT-SIDE approximation of the server's real gate (authz.ActionMergePR, decisioninbox.go: "Viewer role sees the queue read-only... cannot merge"). Every ready_to_merge row already means the underlying PR IS assigned to this actor (buildPRItems' own discovery mechanism), so unlike canActOnPlan (planFormat.ts) this needs no ownership check of its own -- role alone decides. The server re-validates CI/approval-state/Authorize independently on every real merge call regardless of what this returns (MergePullRequest's own doc comment) -- this only ever decides whether the button renders. */
export function canMergeDecisionInboxItem(role: string | undefined): boolean {
  return role !== undefined && role !== 'viewer'
}

// -- PR-shaped row chips (ready_to_merge/needs_review, and the handoff
// sub-case of awaiting_approval) --

export interface DecisionInboxChip {
  tone: ChipTone
  text: string
}

/**
 * prChipData builds the mockup's own `.chip` sequence for a PR-shaped row
 * ("review: low risk", "CI green", a findings count, "changes requested")
 * from ONLY the fields DecisionInboxItem actually carries for such a row
 * -- ciGreen/findings/hasChangesRequested are each independently nullable
 * on the wire (set for any PR-shaped row, per each field's own doc
 * comment) and are skipped here rather than rendered as a false "0"/
 * "not green" when genuinely unknown.
 */
export function prChipData(item: Pick<DecisionInboxItem, 'riskLabel' | 'findings' | 'ciGreen' | 'hasChangesRequested'>): DecisionInboxChip[] {
  const chips: DecisionInboxChip[] = []

  if (item.riskLabel) {
    const findingsSuffix = item.findings !== null && item.findings > 0 ? ` · ${item.findings} finding${item.findings === 1 ? '' : 's'}` : ''
    chips.push({ tone: riskLabelTone(item.riskLabel), text: `${riskLabelText(item.riskLabel)}${findingsSuffix}` })
  } else if (item.findings !== null && item.findings > 0) {
    chips.push({ tone: 'warn', text: `${item.findings} finding${item.findings === 1 ? '' : 's'}` })
  }

  if (item.ciGreen !== null) {
    chips.push(item.ciGreen ? { tone: 'ok', text: 'CI green' } : { tone: 'crit', text: 'CI not green' })
  }

  if (item.hasChangesRequested === true) {
    chips.push({ tone: 'crit', text: 'changes requested' })
  }

  return chips
}

/**
 * releaseChipData builds the mockup's own `.chip` sequence for a release-
 * cut row ("manifest: 3 flags") -- deliberately NOT prChipData's own
 * riskLabel/findings chips, which describe an ordinary code-review
 * verdict a release cut never carries. manifestFindingsCount is §15.2's
 * own mechanical findings only; this NEVER fabricates an "aggregate: N
 * findings" chip the mockup also shows, because the aggregate diff
 * review's own composition findings are not computed anywhere in this
 * system (DecisionInboxItem.aggregateReviewTriggered's own doc comment)
 * -- aggregateReviewTriggered renders as its own, honestly-worded chip
 * instead, saying only that the trigger criteria were met, never
 * implying a finding count that does not exist.
 */
export function releaseChipData(item: Pick<DecisionInboxItem, 'manifestFindingsCount' | 'aggregateReviewTriggered'>): DecisionInboxChip[] {
  const chips: DecisionInboxChip[] = []

  if (item.manifestFindingsCount !== null) {
    const count = item.manifestFindingsCount
    chips.push({ tone: count > 0 ? 'crit' : 'ok', text: `manifest: ${count} flag${count === 1 ? '' : 's'}` })
  }

  if (item.aggregateReviewTriggered === true) {
    chips.push({ tone: 'warn', text: 'aggregate review needed' })
  }

  return chips
}

// -- stable React key per row --
//
// DecisionInboxItem carries no single, universal "id" field across every
// kind (a PR-shaped row has none at all -- repoFullName+prNumber together
// ARE its identity; a plan/session/automation/outbox row each carry
// exactly one of planId/sessionId/automationId/outboxId). rowKeyFor picks
// whichever field the schema's own per-kind contract guarantees is set,
// mirroring rowKind's own dispatch order above.

export function rowKeyFor(item: DecisionInboxItem): string {
  if (item.repoFullName !== null && item.prNumber !== null) return `pr:${item.repoFullName}#${item.prNumber}`
  if (item.planId !== null) return `plan:${item.planId}`
  if (item.automationId !== null) return `automation:${item.automationId}`
  if (item.outboxId !== null) return `outbox:${item.outboxId}`
  if (item.sessionId !== null) return `session:${item.sessionId}`
  // Unreachable given DecisionInboxItem's own per-kind field-population
  // contract (every real row matches one of the branches above) -- a
  // stable-enough fallback rather than a thrown error, so one malformed
  // row can never crash the whole queue render.
  return `row:${item.title}:${item.enteredQueueAt}`
}
