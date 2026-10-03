package framecut

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// tokenCutFramesFixture is the vector file the web twin
// (web/src/session/tokenCut.ts) is checked against too, so the two
// readers cannot drift.
const tokenCutFramesFixture = "../../../web/src/session/__tests__/fixtures/tokenCutFrames.json"

// cutReasonsFixture pins Reason's text for the web twin's cutReason.
const cutReasonsFixture = "../../../web/src/session/__tests__/fixtures/cutReasons.json"

// vectorCase is one case of tokenCutFrames.json: the frames of one text
// part in id order, the frame every reader must read the part as (nil when
// it has no text), and the ids of the frames the session actor's storage
// guard keeps when they arrive in that order.
type vectorCase struct {
	Name   string        `json:"name"`
	Frames []vectorFrame `json:"frames"`
	Want   *struct {
		ID  int64 `json:"id"`
		Cut *Cut  `json:"cut"`
	} `json:"want"`
	Stored []int64 `json:"stored"`
}

type vectorFrame struct {
	ID   int64           `json:"id"`
	Text string          `json:"text"`
	Cut  json.RawMessage `json:"cut"`
}

func readVectors(t *testing.T) []vectorCase {
	t.Helper()
	raw, err := os.ReadFile(tokenCutFramesFixture)
	if err != nil {
		t.Fatalf("read %s: %v", tokenCutFramesFixture, err)
	}
	var cases []vectorCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decode %s: %v", tokenCutFramesFixture, err)
	}
	if len(cases) == 0 {
		t.Fatalf("%s holds no case", tokenCutFramesFixture)
	}
	return cases
}

func (v vectorCase) frames() []Frame {
	out := make([]Frame, len(v.Frames))
	for i, f := range v.Frames {
		out[i] = Frame{Text: f.Text, Cut: DecodeCut(f.Cut)}
	}
	return out
}

func (v vectorCase) frameByID(id int64) Frame {
	for i, f := range v.Frames {
		if f.ID == id {
			return v.frames()[i]
		}
	}
	return Frame{}
}

// TestPartText_SharedVectors reads every part of the shared vector file as
// the web timeline reads it: the frame the case names, with its cut.
func TestPartText_SharedVectors(t *testing.T) {
	for _, tt := range readVectors(t) {
		t.Run(tt.Name, func(t *testing.T) {
			got, ok := PartText(tt.frames())
			if tt.Want == nil {
				if ok {
					t.Fatalf("PartText() = %+v, want no text", got)
				}
				return
			}
			if !ok {
				t.Fatalf("PartText() found no text, want frame %d", tt.Want.ID)
			}
			want := tt.frameByID(tt.Want.ID)
			if got.Text != want.Text || !reflect.DeepEqual(got.Cut, tt.Want.Cut) {
				t.Errorf("PartText() = (%q, %+v), want frame %d (%q, %+v)", got.Text, got.Cut, tt.Want.ID, want.Text, tt.Want.Cut)
			}
		})
	}
}

// TestPartText_OrderOfACutAndItsWholeText pins that a part holding a cut
// and the whole text it was taken from reads as the whole text whichever
// was stored first, and that among frames none of which yields the newest
// is read.
func TestPartText_OrderOfACutAndItsWholeText(t *testing.T) {
	whole := Frame{Text: "abcdefghij"}
	cut := Frame{Text: "abcd\n[text cut at 4 of 10 bytes on its way from the sandbox]", Cut: &Cut{Kept: 4, Total: 10}}
	longerCut := Frame{Text: "abcdefg\n[text cut at 7 of 10 bytes on its way from the sandbox]", Cut: &Cut{Kept: 7, Total: 10}}
	tests := []struct {
		name   string
		frames []Frame
		want   Frame
	}{
		{name: "whole, then cut", frames: []Frame{whole, cut}, want: whole},
		{name: "cut, then whole", frames: []Frame{cut, whole}, want: whole},
		{name: "two cuts and the whole, any order", frames: []Frame{longerCut, whole, cut}, want: whole},
		{name: "two cuts, the longer first", frames: []Frame{longerCut, cut}, want: longerCut},
		{name: "two cuts, the longer last", frames: []Frame{cut, longerCut}, want: longerCut},
		{name: "empty frames are passed over", frames: []Frame{{}, cut, {}}, want: cut},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := PartText(tt.frames)
			if !ok || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PartText() = (%+v, %v), want %+v", got, ok, tt.want)
			}
		})
	}
}

func TestYieldsTo(t *testing.T) {
	cut := func(text string, kept, total int) Frame {
		return Frame{Text: text, Cut: &Cut{Kept: kept, Total: total}}
	}
	tests := []struct {
		name string
		a, b Frame
		want bool
	}{
		{name: "a whole frame never yields", a: Frame{Text: "abc"}, b: Frame{Text: "abcdef"}, want: false},
		{name: "a cut yields to the whole text of exactly total bytes", a: cut("ab+", 2, 4), b: Frame{Text: "abcd"}, want: true},
		{name: "not to a whole frame one byte short", a: cut("ab+", 2, 4), b: Frame{Text: "abc"}, want: false},
		{name: "not to a whole frame one byte long", a: cut("ab+", 2, 4), b: Frame{Text: "abcde"}, want: false},
		{name: "not to a whole frame of total bytes not starting with what was kept", a: cut("ab+", 2, 4), b: Frame{Text: "xbcd"}, want: false},
		{name: "to a cut of the same text keeping more", a: cut("ab+", 2, 4), b: cut("abc+", 3, 4), want: true},
		{name: "not to a cut of the same text keeping as much", a: cut("ab+", 2, 4), b: cut("ab+", 2, 4), want: false},
		{name: "not to a cut of the same text keeping less", a: cut("abc+", 3, 4), b: cut("ab+", 2, 4), want: false},
		{name: "not to a cut of another total", a: cut("ab+", 2, 4), b: cut("abc+", 3, 5), want: false},
		{name: "not to a longer cut whose kept bytes differ", a: cut("ab+", 2, 4), b: cut("xbc+", 3, 4), want: false},
		{name: "a malformed cut never yields", a: Frame{Text: "ab+", Cut: &Malformed}, b: Frame{Text: "abcd"}, want: false},
		{name: "nothing yields to a malformed cut", a: cut("ab+", 2, 4), b: Frame{Text: "abc+", Cut: &Malformed}, want: false},
		{name: "a cut keeping more than its text holds never yields", a: cut("a", 2, 4), b: Frame{Text: "abcd"}, want: false},
		{name: "a cut keeping its whole total never yields", a: cut("abcd", 4, 4), b: Frame{Text: "abcd"}, want: false},
		{name: "a cut keeping nothing yields to any whole text of its total", a: cut("+", 0, 4), b: Frame{Text: "wxyz"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := YieldsTo(tt.a, tt.b); got != tt.want {
				t.Errorf("YieldsTo(%+v, %+v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestDecodeCut(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want *Cut
	}{
		{name: "absent", raw: "", want: nil},
		{name: "null", raw: "null", want: nil},
		{name: "well formed", raw: `{"kept":20,"total":64}`, want: &Cut{Kept: 20, Total: 64}},
		{name: "keys in any order", raw: `{"total":64,"kept":0}`, want: &Cut{Kept: 0, Total: 64}},
		{name: "an integral number in another notation", raw: `{"kept":2e1,"total":64.0}`, want: &Cut{Kept: 20, Total: 64}},
		{name: "a string kept", raw: `{"kept":"20","total":64}`, want: &Malformed},
		{name: "a fractional total", raw: `{"kept":20,"total":64.5}`, want: &Malformed},
		{name: "a missing total", raw: `{"kept":20}`, want: &Malformed},
		{name: "an extra key", raw: `{"kept":20,"total":64,"extra":1}`, want: &Malformed},
		{name: "a negative kept", raw: `{"kept":-1,"total":64}`, want: &Malformed},
		{name: "kept equal to total", raw: `{"kept":64,"total":64}`, want: &Malformed},
		{name: "kept above total", raw: `{"kept":65,"total":64}`, want: &Malformed},
		{name: "a number past what a browser reads exactly", raw: `{"kept":1,"total":9007199254740993}`, want: &Malformed},
		{name: "not an object", raw: `[20,64]`, want: &Malformed},
		{name: "a string", raw: `"cut"`, want: &Malformed},
		{name: "not JSON", raw: `{kept:1}`, want: &Malformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecodeCut([]byte(tt.raw)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DecodeCut(%s) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestReason_SharedFixture pins the sentence every surface gives for a cut
// plan, and that the web says the same (cutReasons.json).
func TestReason_SharedFixture(t *testing.T) {
	raw, err := os.ReadFile(cutReasonsFixture)
	if err != nil {
		t.Fatalf("read %s: %v", cutReasonsFixture, err)
	}
	var cases []struct {
		Cut    Cut    `json:"cut"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decode %s: %v", cutReasonsFixture, err)
	}
	if len(cases) == 0 {
		t.Fatalf("%s holds no case", cutReasonsFixture)
	}
	for _, tt := range cases {
		if got := Reason(&tt.Cut); got != tt.Reason {
			t.Errorf("Reason(%+v) = %q, want %q", tt.Cut, got, tt.Reason)
		}
	}
	if got := Reason(nil); got != "" {
		t.Errorf("Reason(nil) = %q, want empty", got)
	}
}
