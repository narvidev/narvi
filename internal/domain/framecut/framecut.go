// Package framecut is the one rule every reader of a streamed text part
// applies to a frame the sandbox-agent cut on its way to the control plane
// (technical plan §6.1).
//
// A connection that reads less than a frame holds is written a cut form of
// it: one string of the frame shortened at a UTF-8 boundary, ended with a
// line for people, "[text cut at <kept> of <total> bytes on its way from the
// sandbox]", and recorded for machines in the frame's `cut` property,
// {kept, total}: the bytes of that string kept and its whole length. A
// reader learns a cut only from that property, never from the text: a
// marker-shaped line the model wrote itself is text.
//
// The whole form of the same frame may be stored too -- written whole on an
// earlier connection that read it, and then cut on a later one that did not
// -- so a part can hold a cut frame and the whole text it was taken from, in
// either order. YieldsTo says when a cut frame gives way to another frame of
// its part, and PartText picks a part's text by it. The session actor's
// storage guard (tokenFrameAddsNoRow), the plan reader (plan.FinalText) and
// the web timeline (web/src/session/tokenCut.ts, the TypeScript twin) all
// apply this one rule; a shared vector file
// (web/src/session/__tests__/fixtures/tokenCutFrames.json) holds both twins
// to it. With no `cut` anywhere every reader behaves as it did before the
// rule existed: no frame yields, and a part reads as its newest non-empty
// frame.
//
// Nothing here does I/O: a frame's `cut` arrives as the raw JSON its payload
// carried, and DecodeCut reads it.
package framecut

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Cut is what a cut frame records of the one string it shortened: Kept,
// the bytes of the string kept (UTF-8, unescaped), and Total, the string's
// whole length. A cut whose recorded value could not be read is the
// fail-closed sentinel Malformed.
type Cut struct {
	Kept  int `json:"kept"`
	Total int `json:"total"`
}

// Malformed is the cut a frame is read as when its `cut` property is
// present but not a well-formed {kept, total} with 0 <= kept < total. It
// fails closed: it never yields and nothing yields to it (YieldsTo), it is
// reported as a cut, and a plan carrying it cannot be approved.
var Malformed = Cut{Kept: -1, Total: -1}

// Frame is one non-empty frame of a text part, as a reader holds it: its
// text and, for a cut frame, its cut.
type Frame struct {
	Text string
	Cut  *Cut
}

// valid reports whether c is a cut of text as the cutter writes one: it
// kept some prefix of the string, shorter than the string's whole length,
// and text holds at least that prefix. A nil cut (a whole frame) and the
// Malformed sentinel are never valid.
func (c *Cut) valid(text string) bool {
	return c != nil && c.Kept >= 0 && c.Kept < c.Total && c.Kept <= len(text)
}

// YieldsTo reports whether frame a, a cut, gives way to frame b of the same
// part: b is the whole text a was cut from (no cut, exactly a.Cut.Total
// bytes, and starting with the bytes a kept), or b is another cut of that
// same text (the same Total) that kept strictly more, starting with what a
// kept.
//
// A whole frame never yields, and neither does a malformed cut, nor any
// frame to a malformed cut. An earlier, shorter frame that shares the kept
// prefix but is not Total bytes long -- an earlier cumulative frame of a
// runtime that sends more than one non-empty frame per part -- never takes
// a cut's place: it is not the text the cut was taken from.
//
// Yielding is a strict order: a whole frame yields to nothing, and between
// cuts Kept strictly increases, so among any frames of a part at least one
// yields to no other.
func YieldsTo(a, b Frame) bool {
	if !a.Cut.valid(a.Text) {
		return false
	}
	kept := a.Text[:a.Cut.Kept]
	if b.Cut == nil {
		return len(b.Text) == a.Cut.Total && strings.HasPrefix(b.Text, kept)
	}
	return b.Cut.valid(b.Text) && b.Cut.Total == a.Cut.Total &&
		b.Cut.Kept > a.Cut.Kept && strings.HasPrefix(b.Text[:b.Cut.Kept], kept)
}

// PartText returns a part's text from its frames, given in id order (the
// order they were stored in): among the non-empty frames, the newest that
// yields to no other. ok is false when every frame is empty.
//
// The answer does not depend on the order a cut and the whole text it was
// taken from were stored in: the cut yields either way, and the whole text
// is read. With no cut among the frames none yields, and the answer is the
// newest non-empty frame -- the rule every reader applied before cuts.
func PartText(frames []Frame) (Frame, bool) {
	for i := len(frames) - 1; i >= 0; i-- {
		f := frames[i]
		if f.Text == "" {
			continue
		}
		if !yieldsToAny(f, frames) {
			return f, true
		}
	}
	return Frame{}, false
}

// yieldsToAny reports whether f yields to any non-empty frame of frames.
func yieldsToAny(f Frame, frames []Frame) bool {
	for _, other := range frames {
		if other.Text != "" && YieldsTo(f, other) {
			return true
		}
	}
	return false
}

// DecodeCut reads a frame's `cut` property from the raw JSON its payload
// carried. Absent (nil or empty raw) or JSON null is a whole frame: nil. An
// object of exactly the two keys kept and total, each an integer a browser
// reads exactly (|n| <= 2^53-1, as JavaScript's Number.isSafeInteger, so
// the web twin reads the same cut), with 0 <= kept < total, is that cut.
// Anything else present is Malformed: the reader fails closed, reporting a
// cut it cannot read rather than reading a cut frame as whole.
func DecodeCut(raw []byte) *Cut {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil || len(obj) != 2 {
		return malformed()
	}
	kept, keptOK := safeInteger(obj["kept"])
	total, totalOK := safeInteger(obj["total"])
	if !keptOK || !totalOK || kept < 0 || kept >= total {
		return malformed()
	}
	return &Cut{Kept: kept, Total: total}
}

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, 2^53-1.
const maxSafeInteger = 1<<53 - 1

// safeInteger reads raw as a JSON number holding an integer within
// maxSafeInteger: 5, 5.0 and 5e0 alike, as JSON.parse reads them, and
// nothing that is not a number.
func safeInteger(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || f != math.Trunc(f) || math.Abs(f) > maxSafeInteger {
		return 0, false
	}
	return int(f), true
}

func malformed() *Cut {
	c := Malformed
	return &c
}

// Reason is the one sentence every surface gives for a cut plan: why it
// cannot be approved, and what to do instead. The web says the same
// (cutReason, web/src/session/tokenCut.ts), checked against a shared
// fixture (web/src/session/__tests__/fixtures/cutReasons.json). It names
// the sizes when the cut was readable, and only the cut when it was not.
// It holds none of plan.ApproveKeywords as a word: Linear's notice for a cut
// plan carries it where it would otherwise list them. Empty for nil: a
// whole plan has no reason.
func Reason(c *Cut) string {
	if c == nil {
		return ""
	}
	if c.Kept >= 0 && c.Kept < c.Total {
		return fmt.Sprintf("No approval for this plan: its text was cut on its way from the sandbox, keeping %d of %d bytes. Request changes for a shorter plan, or reject it.", c.Kept, c.Total)
	}
	return "No approval for this plan: its text was cut on its way from the sandbox. Request changes for a shorter plan, or reject it."
}
