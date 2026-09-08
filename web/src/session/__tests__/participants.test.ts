import { describe, expect, it } from 'vitest'

import { initials, parseParticipants } from '../participants'

describe('parseParticipants', () => {
  it('parses a well-formed participants array (client.go\'s own resolveParticipants shape)', () => {
    const raw = [
      { userId: 'u1', displayName: 'Alice Anderson' },
      { userId: 'u2', displayName: 'Bob Baker' },
    ]
    expect(parseParticipants(raw)).toEqual([
      { userId: 'u1', displayName: 'Alice Anderson' },
      { userId: 'u2', displayName: 'Bob Baker' },
    ])
  })

  it('returns an empty array for an empty array (nobody else watching -- the common case)', () => {
    expect(parseParticipants([])).toEqual([])
  })

  it('drops an element missing userId or displayName, never crashes or fabricates one', () => {
    const raw = [{ userId: 'u1' }, { displayName: 'No Id' }, { userId: 'u2', displayName: 'Real Person' }]
    expect(parseParticipants(raw)).toEqual([{ userId: 'u2', displayName: 'Real Person' }])
  })

  it('drops an element whose userId/displayName is the wrong type -- never coerces', () => {
    const raw = [{ userId: 42, displayName: 'Numeric Id' }, { userId: 'u1', displayName: null }]
    expect(parseParticipants(raw)).toEqual([])
  })

  it('drops an element with an empty-string userId or displayName', () => {
    const raw = [
      { userId: '', displayName: 'Empty Id' },
      { userId: 'u1', displayName: '' },
    ]
    expect(parseParticipants(raw)).toEqual([])
  })

  it('drops a non-object element', () => {
    expect(parseParticipants([null, 'nope', 42] as unknown as { [k: string]: unknown }[])).toEqual([])
  })
})

describe('initials', () => {
  it('takes the first letter of the first two words, uppercased', () => {
    expect(initials('Alice Anderson')).toBe('AA')
    expect(initials('benoît leleve')).toBe('BL')
  })

  it('takes just the first letter for a single-word name', () => {
    expect(initials('Cher')).toBe('C')
  })

  it('collapses extra whitespace', () => {
    expect(initials('  Alice   Anderson  ')).toBe('AA')
  })

  it('returns "?" for a blank name rather than an empty avatar', () => {
    expect(initials('')).toBe('?')
    expect(initials('   ')).toBe('?')
  })
})
