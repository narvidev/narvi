// tokenCut.test.ts -- the TypeScript twin of internal/domain/framecut, held
// to the same shared vector file the Go side reads
// (fixtures/tokenCutFrames.json: framecut, plan.FinalText and the session
// actor's storage guard) and to the same reason text (fixtures/
// cutReasons.json: framecut.Reason), so the two readers of a cut frame
// cannot drift apart.
import { readFileSync } from 'node:fs'

import { describe, expect, it } from 'vitest'

import { type CutFrame, cutReason, cutYieldsTo, decodeCut, type FrameCut, MALFORMED_CUT, partText } from '../tokenCut'
import { readTokenCutVectors } from './tokenCutVectors'

describe('partText -- the shared vectors', () => {
  for (const vector of readTokenCutVectors()) {
    it(vector.name, () => {
      const frames = vector.frames.map((f) => ({ id: f.id, text: f.text, cut: decodeCut(f.cut) }))
      const picked = partText(frames)
      if (vector.want === null) {
        expect(picked).toBeNull()
        return
      }
      const want = frames.find((f) => f.id === vector.want!.id)!
      expect(picked).not.toBeNull()
      expect([picked!.text, picked!.cut]).toEqual([want.text, vector.want.cut])
    })
  }
})

describe('cutReason -- the one sentence every surface gives for a cut plan', () => {
  it('matches framecut.Reason on every shared case', () => {
    const fixture = new URL('./fixtures/cutReasons.json', import.meta.url)
    const cases = JSON.parse(readFileSync(fixture, 'utf8')) as { cut: FrameCut; reason: string }[]
    expect(cases.length).toBeGreaterThan(0)
    for (const c of cases) expect(cutReason(c.cut)).toBe(c.reason)
  })
})

describe('cutYieldsTo', () => {
  const cut = (text: string, kept: number, total: number): CutFrame => ({ text, cut: { kept, total } })
  const whole = (text: string): CutFrame => ({ text, cut: null })
  const cases: [string, CutFrame, CutFrame, boolean][] = [
    ['a whole frame never yields', whole('abc'), whole('abcdef'), false],
    ['a cut yields to the whole text of exactly total bytes', cut('ab+', 2, 4), whole('abcd'), true],
    ['not to a whole frame one byte short', cut('ab+', 2, 4), whole('abc'), false],
    ['not to a whole frame of total bytes not starting with what was kept', cut('ab+', 2, 4), whole('xbcd'), false],
    ['to a cut of the same text keeping more', cut('ab+', 2, 4), cut('abc+', 3, 4), true],
    ['not to a cut keeping as much', cut('ab+', 2, 4), cut('ab+', 2, 4), false],
    ['not to a cut of another total', cut('ab+', 2, 4), cut('abc+', 3, 5), false],
    ['a malformed cut never yields', { text: 'ab+', cut: { ...MALFORMED_CUT } }, whole('abcd'), false],
    ['nothing yields to a malformed cut', cut('ab+', 2, 4), { text: 'abc+', cut: { ...MALFORMED_CUT } }, false],
    // UTF-8 bytes, never UTF-16 units: '€' is three bytes and one unit.
    ['sizes are UTF-8 bytes', cut('a+', 1, 4), whole('a€'), true],
    ['not UTF-16 units', cut('a+', 1, 2), whole('a€'), false],
  ]
  for (const [name, a, b, want] of cases) {
    it(name, () => expect(cutYieldsTo(a, b)).toBe(want))
  }
})

describe('decodeCut -- fail closed, as framecut.DecodeCut', () => {
  const cases: [string, unknown, FrameCut | null][] = [
    ['absent', undefined, null],
    ['null', null, null],
    ['well formed', { kept: 20, total: 64 }, { kept: 20, total: 64 }],
    ['a string kept', { kept: '20', total: 64 }, MALFORMED_CUT],
    ['a fractional total', { kept: 20, total: 64.5 }, MALFORMED_CUT],
    ['a missing total', { kept: 20 }, MALFORMED_CUT],
    ['an extra key', { kept: 20, total: 64, extra: 1 }, MALFORMED_CUT],
    ['a negative kept', { kept: -1, total: 64 }, MALFORMED_CUT],
    ['kept equal to total', { kept: 64, total: 64 }, MALFORMED_CUT],
    ['a number past what a browser reads exactly', { kept: 1, total: 2 ** 53 }, MALFORMED_CUT],
    ['an array', [20, 64], MALFORMED_CUT],
    ['a string', 'cut', MALFORMED_CUT],
  ]
  for (const [name, raw, want] of cases) {
    it(name, () => expect(decodeCut(raw)).toEqual(want))
  }
})
