package sessionactor

import (
	"strings"
	"testing"
)

// TestTokenFrameStorageKey pins the per-frame storage key: the part id
// first (so the key still names the part it belongs to), one key per
// distinct text, the same key for the same text.
func TestTokenFrameStorageKey(t *testing.T) {
	t.Parallel()

	empty := tokenFrameStorageKey("prt_plan", "")
	prefix := tokenFrameStorageKey("prt_plan", "1. Add the")
	full := tokenFrameStorageKey("prt_plan", "1. Add the migration\n2. Wire the store")

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
	if again := tokenFrameStorageKey("prt_plan", "1. Add the"); again != prefix {
		t.Errorf("same frame keyed twice: %q then %q, want equal (a resend must dedupe)", prefix, again)
	}
	if other := tokenFrameStorageKey("prt_note", "1. Add the"); other == prefix {
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
		latest   string
		want     bool
	}{
		{name: "the stored frame again", incoming: full, latest: full, want: true},
		{name: "empty frame again", incoming: "", latest: "", want: true},
		{name: "newer frame extends the stored empty one", incoming: full, latest: "", want: false},
		{name: "newer frame extends the stored prefix", incoming: full, latest: "1. Add the", want: false},
		{name: "empty frame replayed after the full one", incoming: "", latest: full, want: true},
		{name: "prefix frame replayed after the full one", incoming: "1. Add the", latest: full, want: true},
		{name: "final frame with trailing whitespace trimmed", incoming: "1. Add the", latest: "1. Add the \n\n", want: false},
		{name: "rewrite that is not a prefix", incoming: "Plan withdrawn.", latest: full, want: false},
		{name: "same length, different text", incoming: "1. Add thX", latest: "1. Add the", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tokenFrameAddsNoRow(tt.incoming, tt.latest); got != tt.want {
				t.Errorf("tokenFrameAddsNoRow(%q, %q) = %v, want %v", tt.incoming, tt.latest, got, tt.want)
			}
		})
	}
}
