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

/**
 * scopesSummary renders what the user allowed an app in their MOST RECENT
 * approval -- and says so, because it is not what the app can do now: each
 * token the app already holds keeps the scopes approved when it was issued,
 * and approving again never takes those back (technical plan §43.16). So an
 * empty list reads "no tools" for that approval, never "No access": an
 * earlier approval's token may still be live, and revoking is what withdraws
 * it.
 */
export function scopesSummary(scopes: string[]): string {
  if (scopes.length === 0) return 'Last approved: no tools'
  return `Last approved: ${scopes.map(scopeLabel).join(', ')}`
}

/**
 * clientIdentityLabel says who vouched for the client -- the same identity
 * the consent page shows (technical plan §43.15). An admin-registered client
 * says so. A client identified by a client ID metadata document is known by
 * the host its document was fetched from -- its clientId is that URL, and
 * the host is the one thing about it the app could not choose (the name
 * beside it, it could). A client that registered itself says only that.
 * An unknown kind never borrows either claim.
 */
export function clientIdentityLabel(kind: MCPAuthorization['clientKind'] | string, clientId: string): string {
  if (kind === 'preregistered') return 'Registered by an administrator of this deployment'
  if (kind === 'metadata_document') {
    const host = metadataDocumentHost(clientId)
    if (host) return `Identified by ${host}`
  }
  return 'Registered by the app itself'
}

/** metadataDocumentHost is the host (with any port) of a metadata-document client's https clientId, or '' when it is not one. */
function metadataDocumentHost(clientId: string): string {
  if (!clientId.startsWith('https://')) return ''
  try {
    return new URL(clientId).host
  } catch {
    return ''
  }
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
