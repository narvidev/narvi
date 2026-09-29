// sourceLabel.ts -- the words a session's spawnSource is shown with:
// SourceIcon.tsx's title tooltip and SessionHeader.tsx's `.srctag` text
// (decision 31, "the source stays attached to the session"). Split out so
// both are unit-tested directly and the component files export components
// only (oxlint's react-refresh rule).
//
// Session.spawnSource is an OPEN enum (contracts/manifest.json's
// openEnums): a server newer than this bundle can send a source the
// generated union does not name yet. Every function here therefore takes
// any string, and a value it does not know gets neutral wording: never
// another source's label, and never an empty tag. The map stays a full
// Record over the generated union, so a source added to the contracts
// fails the typecheck here until it is given its own words.
import type { Session } from '@narvi/contracts/rest-dtos'

type SourceWording = { title: string; tag: string }

const SOURCE_WORDING: Record<Session['spawnSource'], SourceWording> = {
  web: { title: 'Started from the web app', tag: 'web' },
  slack: { title: 'Started from Slack', tag: 'Slack' },
  linear: { title: 'Started from Linear', tag: 'Linear' },
  github: { title: 'Started from GitHub', tag: 'GitHub' },
  mcp: { title: 'Started from an MCP client', tag: 'MCP' },
}

const UNKNOWN_SOURCE_WORDING: SourceWording = { title: 'Started from another source', tag: 'other' }

/**
 * isKnownSpawnSource says whether this bundle has words (and a glyph) for
 * source. An own-property check, not `in`: a value such as "constructor"
 * must not resolve to something on the object's prototype.
 */
export function isKnownSpawnSource(source: string): source is Session['spawnSource'] {
  return Object.hasOwn(SOURCE_WORDING, source)
}

function wordingFor(source: Session['spawnSource'] | string): SourceWording {
  return isKnownSpawnSource(source) ? SOURCE_WORDING[source] : UNKNOWN_SOURCE_WORDING
}

/** sourceTitle is the tooltip sentence for a session's source glyph. */
export function sourceTitle(source: Session['spawnSource'] | string): string {
  return wordingFor(source).title
}

/** sourceTagLabel is the short text of the session header's source tag. */
export function sourceTagLabel(source: Session['spawnSource'] | string): string {
  return wordingFor(source).tag
}
