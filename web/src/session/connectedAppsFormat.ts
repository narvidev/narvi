// connectedAppsFormat.ts -- ConnectedAppsSection.tsx's own pure logic,
// split out so each piece has a direct unit test and the component file
// exports components only (oxlint's react-refresh rule).
import type { MCPAuthorization } from '@narvi/contracts/rest-dtos'

import { ApiError } from '../api/http'

/**
 * scopeLabel renders one granted MCP scope (technical plan §43.17) the way
 * the consent page describes it. An unknown value renders as itself --
 * never dropped, never guessed at: the wire list is the truth, and the
 * reader should see exactly what was granted.
 */
export function scopeLabel(scope: string): string {
  switch (scope) {
    case 'mcp:read':
      return 'Read models and sessions'
    case 'mcp:write':
      return 'Act on sessions'
    default:
      return scope
  }
}

/** scopesSummary renders an authorization's scopes; an empty list is stated plainly, since a scope-less app sees no tools at all. */
export function scopesSummary(scopes: string[]): string {
  if (scopes.length === 0) return 'No access (sees no tools)'
  return scopes.map(scopeLabel).join(', ')
}

/**
 * clientIdentityLabel says who vouched for the client -- the same line the
 * consent page shows. Only admin-registered clients exist today; any other
 * kind (reserved for later registration mechanisms) says so rather than
 * borrowing that claim.
 */
export function clientIdentityLabel(kind: MCPAuthorization['clientKind'] | string): string {
  return kind === 'preregistered' ? 'Registered by an administrator of this deployment' : 'Registered by the app itself'
}

/** parseRedirectUris turns the register form's one-per-line textarea into the request's list: trimmed, blank lines dropped, duplicates kept once. The server validates every entry; this only shapes the list. */
export function parseRedirectUris(text: string): string[] {
  const out: string[] = []
  for (const line of text.split('\n')) {
    const uri = line.trim()
    if (uri.length > 0 && !out.includes(uri)) out.push(uri)
  }
  return out
}

/** apiErrorMessage extracts the server's own {"error": "..."} text from a failed request, or falls back -- rendered as plain text by the caller, never markup. */
export function apiErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof ApiError) {
    if (error.status === 403) return 'Your role cannot do this.'
    const body = error.body as { error?: unknown } | undefined
    if (body && typeof body.error === 'string' && body.error.length > 0) return body.error
  }
  return fallback
}
