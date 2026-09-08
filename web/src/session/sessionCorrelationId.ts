// sessionCorrelationId.ts -- §12.2 item 1's own session-rail gap: parses
// the WS subscribe reply's own `state.correlationId` (ws/sessionStream.ts's
// own correlationIdState, client.go's own latestTurnCorrelationID) into a
// typed, render-ready value.
//
// Deliberately its OWN, separate module from sandboxRail.ts/
// sandboxSnapshot.ts, even though SessionRail.tsx renders the result
// right next to the runtime fingerprint those modules build: a
// correlation id names a REQUEST (this session's own most-recently-
// created turn), never a property of the sandbox itself. A sandbox's
// fingerprint is one stable fact for its whole gen; a session's
// correlation id changes every time a new turn dispatches. Folding the
// two into one shape because a mockup happens to draw them in adjacent
// <dt>/<dd> pairs would conflate two facts with different lifetimes and
// different owners (sandboxes vs. turns) -- see migrations/
// 000120_sandboxes_boot_fingerprint.up.sql's own doc comment for the
// full "why these are two columns on two tables, never one".

/** parseSessionCorrelationId narrows raw (SessionStreamSnapshot.correlationIdState) to a non-empty string, or null when this session has no turns yet, its newest turn recorded no correlation id, or raw is malformed -- never throws. */
export function parseSessionCorrelationId(raw: unknown): string | null {
  if (typeof raw === 'string' && raw !== '') return raw
  return null
}
