package sessionactor

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/framecut"
)

// TestTokenFrameStorageKey pins the storage keys. A part's first frame is
// stored under the bare part id, the key a binary predating per-frame keys
// dedupes every frame of the part on; each later frame under the part id
// (so the key still names the part it belongs to), one key per distinct
// text, the same key for the same text.
func TestTokenFrameStorageKey(t *testing.T) {
	t.Parallel()

	for _, text := range []string{"", "1. Add the", "1. Add the migration\n2. Wire the store"} {
		if got := tokenFrameStorageKey("prt_plan", text, false); got != "prt_plan" {
			t.Errorf("first frame %q keyed %q, want the bare part id %q", text, got, "prt_plan")
		}
	}

	empty := tokenFrameStorageKey("prt_plan", "", true)
	prefix := tokenFrameStorageKey("prt_plan", "1. Add the", true)
	full := tokenFrameStorageKey("prt_plan", "1. Add the migration\n2. Wire the store", true)

	for _, k := range []string{empty, prefix, full} {
		if !strings.HasPrefix(k, "prt_plan#") {
			t.Errorf("key %q does not start with the part id and '#'", k)
		}
		if got, want := len(k), len("prt_plan#")+2*tokenFrameKeyHashBytes; got != want {
			t.Errorf("key %q has length %d, want %d", k, got, want)
		}
	}
	if empty == prefix || prefix == full || empty == full {
		t.Errorf("distinct frames share a key: empty=%q prefix=%q full=%q", empty, prefix, full)
	}
	if again := tokenFrameStorageKey("prt_plan", "1. Add the", true); again != prefix {
		t.Errorf("same frame keyed twice: %q then %q, want equal (a resend must dedupe)", prefix, again)
	}
	if other := tokenFrameStorageKey("prt_note", "1. Add the", true); other == prefix {
		t.Errorf("same text in two parts shares key %q, want distinct", other)
	}
}

// TestTokenFrameAddsNoRow covers every shape an incoming cumulative frame
// can have against the newest stored frame of its part.
func TestTokenFrameAddsNoRow(t *testing.T) {
	t.Parallel()

	const full = "1. Add the migration\n2. Wire the store\n3. Tests"

	tests := []struct {
		name     string
		incoming string
		first    string
		latest   string
		want     bool
	}{
		{name: "the stored frame again", incoming: full, first: "", latest: full, want: true},
		{name: "empty frame again", incoming: "", first: "", latest: "", want: true},
		{name: "newer frame extends the stored empty one", incoming: full, first: "", latest: "", want: false},
		{name: "newer frame extends the stored prefix", incoming: full, first: "1. Add the", latest: "1. Add the", want: false},
		{name: "empty frame replayed after the full one", incoming: "", first: "", latest: full, want: true},
		{name: "prefix frame replayed after the full one", incoming: "1. Add the", first: "", latest: full, want: true},
		{name: "final frame with trailing whitespace trimmed", incoming: "1. Add the", first: "", latest: "1. Add the \n\n", want: false},
		{name: "first frame replayed after a trimmed final frame", incoming: "1. Add the \n\n", first: "1. Add the \n\n", latest: "1. Add the", want: true},
		{name: "first frame replayed after a rewrite", incoming: "1. Add the", first: "1. Add the", latest: "Plan withdrawn.", want: true},
		{name: "rewrite that is not a prefix", incoming: "Plan withdrawn.", first: "", latest: full, want: false},
		{name: "same length, different text", incoming: "1. Add thX", first: "", latest: "1. Add the", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tokenFrameAddsNoRow(framecut.Frame{Text: tt.incoming}, framecut.Frame{Text: tt.first}, framecut.Frame{Text: tt.latest})
			if got != tt.want {
				t.Errorf("tokenFrameAddsNoRow(%q, %q, %q) = %v, want %v", tt.incoming, tt.first, tt.latest, got, tt.want)
			}
		})
	}
}

// TestTokenFrameAddsNoRow_CutFrames covers a cut frame (§6.1) against the
// part's stored frames: it adds no row when it yields to the first or the
// newest stored frame -- the whole text it was taken from, or a cut of that
// text keeping more -- and its row otherwise.
func TestTokenFrameAddsNoRow_CutFrames(t *testing.T) {
	t.Parallel()

	const whole = "1. Add the migration\n2. Wire the store\n3. Tests"
	cutOf := func(kept int) framecut.Frame {
		return framecut.Frame{
			Text: whole[:kept] + "\n[text cut at " + strconv.Itoa(kept) + " of " + strconv.Itoa(len(whole)) + " bytes on its way from the sandbox]",
			Cut:  &framecut.Cut{Kept: kept, Total: len(whole)},
		}
	}
	empty := framecut.Frame{}
	wholeFrame := framecut.Frame{Text: whole}
	malformed := cutOf(20)
	malformed.Cut = &framecut.Malformed

	tests := []struct {
		name                    string
		incoming, first, latest framecut.Frame
		want                    bool
	}{
		{name: "a cut of the stored newest whole text", incoming: cutOf(20), first: empty, latest: wholeFrame, want: true},
		{name: "a cut of the whole text stored first, a later frame newest", incoming: cutOf(20), first: wholeFrame, latest: framecut.Frame{Text: "Plan withdrawn."}, want: true},
		{name: "a cut keeping less than the stored cut", incoming: cutOf(10), first: empty, latest: cutOf(20), want: true},
		{name: "a cut keeping more than the stored cut", incoming: cutOf(30), first: empty, latest: cutOf(20), want: false},
		{name: "a cut of a text never stored whole", incoming: cutOf(20), first: empty, latest: empty, want: false},
		{name: "a cut whose total the stored whole text is not", incoming: cutOf(20), first: empty, latest: framecut.Frame{Text: whole[:35]}, want: false},
		{name: "a malformed cut", incoming: malformed, first: empty, latest: wholeFrame, want: false},
		{name: "the whole text after its cut", incoming: wholeFrame, first: empty, latest: cutOf(20), want: false},
		{name: "the stored cut again", incoming: cutOf(20), first: empty, latest: cutOf(20), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tokenFrameAddsNoRow(tt.incoming, tt.first, tt.latest); got != tt.want {
				t.Errorf("tokenFrameAddsNoRow(%+v, %+v, %+v) = %v, want %v", tt.incoming, tt.first, tt.latest, got, tt.want)
			}
		})
	}
}

// tokenCutFramesFixture is the vector file framecut, plan.FinalText and the
// web timeline read too, so the storage guard and every reader are held to
// one rule.
const tokenCutFramesFixture = "../../../web/src/session/__tests__/fixtures/tokenCutFrames.json"

// TestTokenFrameAddsNoRow_SharedVectors replays each vector's frames, in
// id order, through the guard as appendTokenFrame applies it to one part --
// the first frame always stored, each later one against the first and
// newest stored -- and checks the rows it keeps, and that those rows read
// as the frame every reader reads the whole vector as.
func TestTokenFrameAddsNoRow_SharedVectors(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(tokenCutFramesFixture)
	if err != nil {
		t.Fatalf("read %s: %v", tokenCutFramesFixture, err)
	}
	var cases []struct {
		Name   string `json:"name"`
		Frames []struct {
			ID   int64           `json:"id"`
			Text string          `json:"text"`
			Cut  json.RawMessage `json:"cut"`
		} `json:"frames"`
		Want *struct {
			ID  int64         `json:"id"`
			Cut *framecut.Cut `json:"cut"`
		} `json:"want"`
		Stored []int64 `json:"stored"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decode %s: %v", tokenCutFramesFixture, err)
	}
	if len(cases) == 0 {
		t.Fatalf("%s holds no case", tokenCutFramesFixture)
	}
	for _, tt := range cases {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()
			var storedIDs []int64
			var stored []framecut.Frame
			byID := map[int64]framecut.Frame{}
			for _, f := range tt.Frames {
				frame := framecut.Frame{Text: f.Text, Cut: framecut.DecodeCut(f.Cut)}
				byID[f.ID] = frame
				if len(stored) > 0 && tokenFrameAddsNoRow(frame, stored[0], stored[len(stored)-1]) {
					continue
				}
				stored = append(stored, frame)
				storedIDs = append(storedIDs, f.ID)
			}
			if !reflect.DeepEqual(storedIDs, tt.Stored) {
				t.Errorf("rows kept for frames %v, want %v", storedIDs, tt.Stored)
			}
			got, ok := framecut.PartText(stored)
			if tt.Want == nil {
				if ok {
					t.Errorf("stored rows read as %+v, want no text", got)
				}
				return
			}
			if want := byID[tt.Want.ID]; !ok || got.Text != want.Text || !reflect.DeepEqual(got.Cut, tt.Want.Cut) {
				t.Errorf("stored rows read as (%+v, %v), want frame %d (%q, %+v)", got, ok, tt.Want.ID, want.Text, tt.Want.Cut)
			}
		})
	}
}

// TestTokenPartFromEarlierTurn pins where a stored part sits against the
// Processing turn's window: every event that turn produces has an id above
// its dispatched_event_id, so a part first stored at or below it entered
// the log before the turn existed.
func TestTokenPartFromEarlierTurn(t *testing.T) {
	t.Parallel()

	watermark := func(id int64) *int64 { return &id }
	tests := []struct {
		name              string
		firstFrameID      int64
		dispatchedEventID *int64
		want              bool
	}{
		{name: "part first stored inside the turn's window", firstFrameID: 11, dispatchedEventID: watermark(10), want: false},
		{name: "part first stored at the watermark itself", firstFrameID: 10, dispatchedEventID: watermark(10), want: true},
		{name: "part first stored before the turn was dispatched", firstFrameID: 3, dispatchedEventID: watermark(10), want: true},
		{name: "turn dispatched into an empty log", firstFrameID: 1, dispatchedEventID: watermark(0), want: false},
		{name: "no watermark places no window", firstFrameID: 1, dispatchedEventID: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tokenPartFromEarlierTurn(tt.firstFrameID, tt.dispatchedEventID); got != tt.want {
				t.Errorf("tokenPartFromEarlierTurn(%d, %v) = %v, want %v", tt.firstFrameID, tt.dispatchedEventID, got, tt.want)
			}
		})
	}
}
