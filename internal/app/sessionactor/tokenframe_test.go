package sessionactor

import (
	"strings"
	"testing"
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
			if got := tokenFrameAddsNoRow(tt.incoming, tt.first, tt.latest); got != tt.want {
				t.Errorf("tokenFrameAddsNoRow(%q, %q, %q) = %v, want %v", tt.incoming, tt.first, tt.latest, got, tt.want)
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
