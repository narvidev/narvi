// slots.tsx is the SPA's own runtime extension-slot registry (technical
// plan §34, docs/design/boundaries-design.md, section 4.1/4.3). A
// private module's own bootstrap registers a component into a named
// slot at runtime; this repository's own bootstrap ({}, main.tsx)
// registers nothing, so registry stays empty forever in the public
// bundle and every SlotOutlet renders its own fallback -- see that
// file's own doc comment.
//
// Named ".tsx", not the design note's own literal ".ts": SlotOutlet
// returns JSX, and a plain .ts file cannot contain JSX syntax at all
// (TypeScript reserves that to .tsx). The exported surface is otherwise
// exactly what that section names -- SlotId, registerSlot, SlotOutlet.
//
// Runtime, not build-time conditional imports (docs/design/
// boundaries-design.md, section 4.5, item 3): slot ids are the ONLY
// coupling between this repository and a private bundle, so a future
// private screen and this file's own fallback can never disagree about
// which slot they mean.
import type { ComponentType, ReactNode } from 'react'

/**
 * SlotId is the closed set of extension points this repository's own UI
 * defines -- a TypeScript union, never an open string, so a private
 * module and this repository's own SlotOutlet calls are checked against
 * the SAME fixed, reviewable vocabulary (adding a slot id is a
 * deliberate PR here, mirroring internal/domain/license.Capability's own
 * "adding one is a reviewable PR" convention on the Go side of this same
 * boundary).
 *
 * "repo-settings.governance" is the one slot this Step wires up:
 * RepoSettingsView.tsx's own card column, right after ShadowLedgerCard
 * (see that view's own doc comment for why there, and why not a ninth
 * Settings tab).
 */
export type SlotId = 'repo-settings.governance'

// SlotComponent takes no props: every slot this repository defines today
// renders standalone (it fetches its own data, e.g. ext/useCapabilities.ts),
// so there is nothing a caller needs to pass in.
type SlotComponent = ComponentType<Record<string, never>>

// registry is module-level, private state -- there is deliberately no
// way to read it from outside this file except through SlotOutlet below,
// mirroring internal/app/capability.Registry's own "no method that can
// express anything but Enabled/State" discipline one layer down.
const registry = new Map<SlotId, SlotComponent>()

/**
 * registerSlot fills slot id with component -- called by a private
 * module's own bootstrap (docs/design/boundaries-design.md, section 4.5,
 * item 2's own bootstrap({ slots })), never by this repository itself.
 *
 * Registering the SAME id twice is a programming error, not a
 * legitimate "last one wins" override: two different bundles both
 * trying to own the same extension point is exactly the kind of silent
 * conflict a closed, reviewable slot vocabulary exists to make loud
 * instead of quietly non-deterministic.
 */
export function registerSlot(id: SlotId, component: SlotComponent): void {
  if (registry.has(id)) {
    throw new Error(`ext/slots: slot "${id}" is already registered`)
  }
  registry.set(id, component)
}

/**
 * SlotOutlet renders the component registered for id, or fallback when
 * nothing has registered one -- the public bundle's own permanent state
 * for every slot today, since registerSlot above is never called from
 * anywhere in this repository.
 */
export function SlotOutlet({ id, fallback }: { id: SlotId; fallback: ReactNode }): ReactNode {
  const Component = registry.get(id)
  return Component ? <Component /> : fallback
}
