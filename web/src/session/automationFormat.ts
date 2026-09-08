// automationFormat.ts -- small, pure formatting/mapping helpers for
// AutomationsView.tsx, mirroring reviewFormat.ts/planFormat.ts's own
// "closed-enum -> chip-tone/label mappings, no I/O, no rendering" shape.
import type { Automation, AutomationRun } from '@narvi/contracts/rest-dtos'

import type { ChipTone } from './reviewFormat'

/** AUTO_PAUSE_THRESHOLD mirrors internal/domain/automation.AutoPauseThreshold (strike.go) exactly -- "3" is a structural fact of the domain's own auto-pause rule (§3.5: "auto-pause after 3 consecutive failed invocations"), not fetched from any endpoint (no GET route exposes it, and it is a compile-time Go constant, not configuration) -- named here, verbatim, rather than silently duplicated as a bare literal. */
export const AUTO_PAUSE_THRESHOLD = 3

export function automationStatusTone(status: Automation['status']): ChipTone {
  return status === 'active' ? 'ok' : 'neutral'
}

export function lastRunTone(status: Automation['lastRunStatus']): ChipTone {
  if (status === 'succeeded') return 'ok'
  if (status === 'failed') return 'crit'
  return 'neutral'
}

export function runStatusTone(status: AutomationRun['status']): ChipTone {
  if (status === 'succeeded') return 'ok'
  if (status === 'failed') return 'crit'
  return 'neutral' // starting/running
}

/** targetsSummary mirrors the mockup's own "3 repos" / "payroll-api" convention: the single repo's own name when there is exactly one, otherwise a count. */
export function targetsSummary(repos: Automation['repos']): string {
  if (repos.length === 1) return repos[0]!.name
  return `${repos.length} repos`
}

/** triggerSummary renders triggerType plus its own type-specific config, read directly from the already-typed automation.domain.*TriggerConfig shapes (internal/domain/automation/trigger.go) -- triggerConfig itself is opaque JSON on the wire (Automation.triggerConfig's own schema doc comment: "validated at the application layer, not in the schema"), so this function defensively checks each field's own runtime type rather than assuming a shape. */
export function triggerSummary(triggerType: Automation['triggerType'], cfg: Record<string, unknown>): string {
  switch (triggerType) {
    case 'cron':
      return typeof cfg.schedule === 'string' ? `cron · ${cfg.schedule}` : 'cron'
    case 'github': {
      const parts = [typeof cfg.event === 'string' ? cfg.event : null, typeof cfg.action === 'string' ? cfg.action : null].filter((v): v is string => v !== null)
      return `github · ${parts.length > 0 ? parts.join('.') : 'event'}`
    }
    case 'linear': {
      const parts = [typeof cfg.eventType === 'string' ? cfg.eventType : null, typeof cfg.action === 'string' ? cfg.action : null].filter((v): v is string => v !== null)
      return `linear · ${parts.length > 0 ? parts.join('.') : 'event'}`
    }
    case 'webhook':
      return 'webhook'
    case 'manual':
    default:
      return 'manual'
  }
}

/**
 * nextRunSummary renders the mockup's own "Next" column honestly: there is
 * no "next scheduled fire" computation anywhere in this codebase (only
 * automations.last_cron_fired_at, a CAS guard, not a prediction -- grepped
 * directly under internal/app/automation and internal/domain/automation).
 * A cron automation shows its own real schedule expression (real data,
 * already shown once in the Trigger column too, but repeated here since
 * "next" genuinely has no other honest content); an event-triggered
 * automation (github/linear) shows "on event" (structurally true: it has
 * no schedule at all); manual/webhook, or a PAUSED automation of any
 * trigger type, show "—" (a paused automation will not fire next,
 * regardless of trigger type -- never a countdown mockups.html's own
 * static "in 11 h" example would otherwise mislead this view into
 * fabricating).
 */
/**
 * runHealthLabel renders §12.2 item 4's own health-ratio text
 * (mockups.html's own "12/12 ok" / "47/48 ok") from Automation.runHealth
 * -- null when this automation has never had a terminal run (the field
 * itself is null/undefined on the wire), rendered by the caller as
 * whatever it already shows for "no data yet" rather than a fabricated
 * ratio (AutomationsView.tsx's own existing plain status chip).
 */
export function runHealthLabel(runHealth: Automation['runHealth']): string | null {
  if (runHealth == null) return null
  return `${runHealth.succeededRuns}/${runHealth.terminalRuns} ok`
}

export function nextRunSummary(automation: Automation): string {
  if (automation.status === 'paused') return '—'
  if (automation.triggerType === 'cron') {
    return typeof automation.triggerConfig.schedule === 'string' ? automation.triggerConfig.schedule : 'scheduled'
  }
  if (automation.triggerType === 'github' || automation.triggerType === 'linear') return 'on event'
  return '—'
}
