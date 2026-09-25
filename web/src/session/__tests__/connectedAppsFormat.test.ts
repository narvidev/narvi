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
    expect(clientIdentityLabel('preregistered')).toBe('Registered by an administrator of this deployment')
    expect(clientIdentityLabel('dynamic')).not.toContain('administrator')
    expect(clientIdentityLabel('metadata_document')).not.toContain('administrator')
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
