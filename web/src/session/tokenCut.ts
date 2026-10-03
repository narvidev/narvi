// tokenCut.ts -- the TypeScript twin of internal/domain/framecut: the one
// rule every reader of a streamed text part applies to a frame the
// sandbox-agent cut on its way to the control plane (technical plan §6.1).
//
// A connection that reads less than a frame holds is written a cut form of
// it: one string shortened at a UTF-8 boundary, ended with a line for
// people, "[text cut at <kept> of <total> bytes on its way from the
// sandbox]", and recorded for machines in the frame's `cut` property,
// {kept, total} -- the bytes of that string kept and its whole length. A
// reader learns a cut only from that property, never from the text: a
// marker-shaped line the model wrote itself is text.
//
// The whole form of the same frame may be stored too, before or after the
// cut, so a part can hold both. cutYieldsTo says when a cut frame gives way
// to another frame of its part, and partText picks a part's text by it --
// the same rule the session actor's storage guard and plan.FinalText apply
// in Go, held to one shared vector file
// (__tests__/fixtures/tokenCutFrames.json). With no `cut` anywhere no frame
// yields, and a part reads as its newest non-empty frame, as it always did.
//
// Sizes are UTF-8 bytes of the unescaped text, as the Go side counts them:
// every comparison here is on `new TextEncoder().encode(text)`, never on a
// JavaScript string's UTF-16 length. The kept bytes are compared raw, never
// decoded back, so a `kept` that falls inside a character is compared byte
// for byte like any other: the cut's own text still holds that whole
// character, and it yields to a whole text of `total` bytes that starts
// with the same bytes, exactly as framecut.YieldsTo does.

/** A frame's cut, as its `cut` property records it. MALFORMED_CUT when that property was present but unreadable. */
export interface FrameCut {
  kept: number
  total: number
}

/** One frame of a text part, as a reader holds it: its text and, for a cut frame, its cut. */
export interface CutFrame {
  text: string
  cut: FrameCut | null
}

/**
 * The cut a frame is read as when its `cut` property is present but not a
 * well-formed {kept, total} with 0 <= kept < total. It fails closed: it
 * never yields and nothing yields to it, it is reported as a cut, and a
 * plan carrying it cannot be approved -- framecut.Malformed's twin.
 */
export const MALFORMED_CUT: Readonly<FrameCut> = Object.freeze({ kept: -1, total: -1 })

const encoder = new TextEncoder()

function utf8(text: string): Uint8Array {
  return encoder.encode(text)
}

function startsWith(bytes: Uint8Array, prefix: Uint8Array): boolean {
  if (prefix.length > bytes.length) return false
  for (let i = 0; i < prefix.length; i++) {
    if (bytes[i] !== prefix[i]) return false
  }
  return true
}

function valid(cut: FrameCut | null, bytes: Uint8Array): cut is FrameCut {
  return cut !== null && cut.kept >= 0 && cut.kept < cut.total && cut.kept <= bytes.length
}

/**
 * decodeCut reads a frame's raw `cut` property, as framecut.DecodeCut does:
 * absent or null is a whole frame (null); an object of exactly the two keys
 * kept and total, each a safe integer, with 0 <= kept < total, is that cut;
 * anything else present is MALFORMED_CUT.
 */
export function decodeCut(raw: unknown): FrameCut | null {
  if (raw === undefined || raw === null) return null
  if (typeof raw !== 'object' || Array.isArray(raw)) return { ...MALFORMED_CUT }
  const keys = Object.keys(raw)
  const { kept, total } = raw as Record<string, unknown>
  if (keys.length !== 2 || !Number.isSafeInteger(kept) || !Number.isSafeInteger(total)) return { ...MALFORMED_CUT }
  const k = kept as number
  const t = total as number
  if (k < 0 || k >= t) return { ...MALFORMED_CUT }
  return { kept: k === 0 ? 0 : k, total: t }
}

/**
 * cutYieldsTo reports whether frame a, a cut, gives way to frame b of the
 * same part: b is the whole text a was cut from (no cut, exactly
 * a.cut.total bytes, starting with the bytes a kept), or another cut of
 * that text (the same total) keeping strictly more, starting with what a
 * kept. A whole frame never yields, nor a malformed cut, nor anything to a
 * malformed cut; an earlier, shorter frame sharing the kept prefix is not
 * the text a cut was taken from.
 */
export function cutYieldsTo(a: CutFrame, b: CutFrame): boolean {
  const aBytes = utf8(a.text)
  if (!valid(a.cut, aBytes)) return false
  const kept = aBytes.subarray(0, a.cut.kept)
  const bBytes = utf8(b.text)
  if (b.cut === null) {
    return bBytes.length === a.cut.total && startsWith(bBytes, kept)
  }
  return valid(b.cut, bBytes) && b.cut.total === a.cut.total && b.cut.kept > a.cut.kept && startsWith(bBytes.subarray(0, b.cut.kept), kept)
}

/**
 * partText returns a part's text from its frames, given in id order: among
 * the non-empty frames, the newest that yields to no other; null when every
 * frame is empty. A cut and the whole text it was taken from read as the
 * whole text whichever was stored first.
 */
export function partText<T extends CutFrame>(frames: readonly T[]): T | null {
  for (let i = frames.length - 1; i >= 0; i--) {
    const frame = frames[i]!
    if (frame.text === '') continue
    if (!frames.some((other) => other.text !== '' && cutYieldsTo(frame, other))) return frame
  }
  return null
}

/**
 * cutReason is the one sentence every surface gives for a cut plan --
 * framecut.Reason's twin, held to it by __tests__/fixtures/cutReasons.json:
 * why it cannot be approved, and what to do instead. It names the sizes
 * when the cut was readable, and only the cut when it was not.
 */
export function cutReason(cut: FrameCut): string {
  if (cut.kept >= 0 && cut.kept < cut.total) {
    return `No approval for this plan: its text was cut on its way from the sandbox, keeping ${cut.kept} of ${cut.total} bytes. Request changes for a shorter plan, or reject it.`
  }
  return 'No approval for this plan: its text was cut on its way from the sandbox. Request changes for a shorter plan, or reject it.'
}
