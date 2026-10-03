// tokenCutVectors.ts -- reads the shared vector file
// (fixtures/tokenCutFrames.json) every reader of a cut frame is held to:
// internal/domain/framecut, plan.FinalText and the session actor's storage
// guard in Go, tokenCut.ts and the timeline model here. Each case is one
// text part's frames in id order, the frame every reader must read the
// part as (null when it has no text), and the ids of the frames the
// storage guard keeps when they arrive in that order.
import { readFileSync } from 'node:fs'

import type { FrameCut } from '../tokenCut'

export interface TokenCutVector {
  name: string
  frames: { id: number; text: string; cut?: unknown }[]
  want: { id: number; cut: FrameCut | null } | null
  stored: number[]
}

export function readTokenCutVectors(): TokenCutVector[] {
  const fixture = new URL('./fixtures/tokenCutFrames.json', import.meta.url)
  const vectors = JSON.parse(readFileSync(fixture, 'utf8')) as TokenCutVector[]
  if (vectors.length === 0) throw new Error('fixtures/tokenCutFrames.json holds no case')
  return vectors
}
