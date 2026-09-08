// participants.ts -- §8.11's own "multiplayer presence": parses the WS
// subscribe reply's own top-level `participants` array (ws/sessionStream.ts's
// own participantsState, client.go's own resolveParticipants:
// `{userId, displayName}` per currently-live, distinct user) into a typed,
// render-ready shape, mirroring sandboxSnapshot.ts's own "narrow untrusted
// wire data, never throw" discipline: a malformed element is dropped, never
// crashes this parser or the view built on top of it.
//
// This is LIVE presence, not the durable `participants` Postgres table
// (§13.3's separate "member on own/joined" authorization concept,
// postgres.ParticipantStore) -- see wshub/doc.go's own "two different
// questions with two different lifetimes" paragraph for the full
// reasoning. A session with nobody else watching renders zero
// participants, honestly, never a fabricated "just you" placeholder.
import { isPlainObject } from '../ws/util'

export interface Participant {
  userId: string
  displayName: string
}

/** parseParticipants narrows raw (SessionStreamSnapshot.participantsState) to Participant[], silently dropping any element missing a non-empty string userId/displayName -- a malformed element from the server must not crash this view, it must just not be counted as a live participant. */
export function parseParticipants(raw: readonly { [k: string]: unknown }[]): Participant[] {
  const out: Participant[] = []
  for (const item of raw) {
    if (!isPlainObject(item)) continue
    const { userId, displayName } = item
    if (typeof userId !== 'string' || userId === '') continue
    if (typeof displayName !== 'string' || displayName === '') continue
    out.push({ userId, displayName })
  }
  return out
}

/**
 * initials derives a short (1-2 character) avatar label from a display
 * name -- the first letter of the first two whitespace-separated words,
 * uppercased ("Alice Anderson" -> "AA", "Cher" -> "C"), mirroring the
 * mockup's own "BL" convention (docs/design/mockups.html) for a
 * two-word name. Never throws on an empty/blank name -- returns "?"
 * rather than an empty avatar, so a malformed-but-present displayName
 * still renders something visible instead of a blank chip.
 */
export function initials(displayName: string): string {
  const words = displayName.trim().split(/\s+/).filter(Boolean)
  if (words.length === 0) return '?'
  const first = words[0]!.charAt(0)
  const second = words.length > 1 ? words[1]!.charAt(0) : ''
  const result = (first + second).toUpperCase()
  return result === '' ? '?' : result
}
