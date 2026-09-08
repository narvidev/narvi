import { describe, expect, it } from 'vitest'

import { parseSessionCorrelationId } from '../sessionCorrelationId'

describe('parseSessionCorrelationId', () => {
  it('returns the real string when present (client.go\'s own latestTurnCorrelationID)', () => {
    expect(parseSessionCorrelationId('cor_8f3ka91')).toBe('cor_8f3ka91')
  })

  it('returns null for null (no turns yet, or the newest turn recorded none)', () => {
    expect(parseSessionCorrelationId(null)).toBeNull()
  })

  it('returns null for undefined', () => {
    expect(parseSessionCorrelationId(undefined)).toBeNull()
  })

  it('returns null for an empty string -- never a visibly-blank "trace" value', () => {
    expect(parseSessionCorrelationId('')).toBeNull()
  })

  it('returns null for a non-string value -- never coerces', () => {
    expect(parseSessionCorrelationId(42)).toBeNull()
    expect(parseSessionCorrelationId({ id: 'cor_8f3ka91' })).toBeNull()
  })
})
