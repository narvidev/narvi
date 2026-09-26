import { describe, expect, it } from 'vitest'

import { ApiError } from '../../api/http'
import { apiErrorMessage, clientIdentityLabel, parseRedirectUris, scopeLabel, scopesSummary } from '../connectedAppsFormat'

describe('connectedAppsFormat', () => {
  it('scopeLabel names known scopes and passes unknown ones through verbatim', () => {
    expect(scopeLabel('mcp:read')).toBe('Read models and sessions')
    expect(scopeLabel('mcp:write')).toBe('Act on sessions')
    expect(scopeLabel('mcp:future')).toBe('mcp:future')
  })

  it('scopesSummary describes the most recent approval, never the access an app holds now', () => {
    expect(scopesSummary(['mcp:read', 'mcp:write'])).toBe('Last approved: Read models and sessions, Act on sessions')
    // An earlier approval's token keeps its own scopes, so an empty last
    // approval must not read as "no access".
    expect(scopesSummary([])).toBe('Last approved: no tools')
    expect(scopesSummary([])).not.toMatch(/no access/i)
  })

  it('clientIdentityLabel claims admin registration only for preregistered clients', () => {
    expect(clientIdentityLabel('preregistered', 'narvi_mcp_c_abc')).toBe('Registered by an administrator of this deployment')
    expect(clientIdentityLabel('dynamic', 'narvi_mcp_d_abc')).toBe('Registered by the app itself')
    expect(clientIdentityLabel('metadata_document', 'https://tools.example/mcp/client.json')).not.toContain('administrator')
    expect(clientIdentityLabel('some_future_kind', 'https://tools.example/mcp/client.json')).toBe('Registered by the app itself')
  })

  it('clientIdentityLabel names a metadata-document client by its host, never by a name it chose', () => {
    expect(clientIdentityLabel('metadata_document', 'https://tools.example/mcp/client.json')).toBe('Identified by tools.example')
    expect(clientIdentityLabel('metadata_document', 'https://tools.example:8443/client.json')).toBe('Identified by tools.example:8443')
    // A clientId that is not an https URL (never stored, but never trusted): no host is claimed.
    expect(clientIdentityLabel('metadata_document', 'narvi_mcp_c_abc')).toBe('Registered by the app itself')
    expect(clientIdentityLabel('metadata_document', 'https://')).toBe('Registered by the app itself')
  })

  it('parseRedirectUris trims, drops blank lines and keeps each URI once', () => {
    expect(parseRedirectUris('  http://127.0.0.1/cb \n\nhttps://x.example/cb\nhttp://127.0.0.1/cb\n')).toEqual(['http://127.0.0.1/cb', 'https://x.example/cb'])
    expect(parseRedirectUris('   \n  ')).toEqual([])
  })

  it('apiErrorMessage surfaces the server message, a role refusal, or the fallback', () => {
    expect(apiErrorMessage(new ApiError(400, 'bad', { error: 'invalid redirect URI: must not contain a fragment' }), 'x')).toBe('invalid redirect URI: must not contain a fragment')
    expect(apiErrorMessage(new ApiError(403, 'forbidden', { error: 'not authorized' }), 'x')).toBe('Your role cannot do this.')
    expect(apiErrorMessage(new ApiError(500, 'boom', undefined), 'fallback')).toBe('fallback')
    expect(apiErrorMessage(new Error('network'), 'fallback')).toBe('fallback')
  })
})
