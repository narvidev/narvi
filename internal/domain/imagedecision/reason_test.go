package imagedecision

import "testing"

// TestAll_NoDuplicatesAndExcludesNone guards the two properties every
// other consumer of All() depends on: every persisted value is listed
// exactly once (a duplicate would silently under-count in the Postgres
// enum's own value list, migrations/000139_sandboxes_image_decision.up.sql),
// and ReasonNone -- the one non-persisted sentinel -- never appears (see
// its own doc comment for why it must not).
func TestAll_NoDuplicatesAndExcludesNone(t *testing.T) {
	seen := make(map[Reason]bool)
	for _, r := range All() {
		if r == ReasonNone {
			t.Fatalf("All() includes ReasonNone, which must never be persisted")
		}
		if r == "" {
			t.Fatalf("All() includes the empty Reason")
		}
		if seen[r] {
			t.Fatalf("All() lists %q more than once", r)
		}
		seen[r] = true
	}
	// 22 persisted values, per this package's own const block: ReasonSelected
	// plus 21 fallback reasons (including ReasonUnrecognized, the
	// designated fallback persistImageDecisionBestEffort substitutes for
	// a Reason outside this vocabulary -- see that constant's own doc
	// comment). Pinned as a literal count (not just len(All()) > 0) so an
	// accidental removal is caught here, in a pure unit test, rather than
	// only surfacing later as a Postgres enum/Go vocabulary drift.
	const wantCount = 22
	if got := len(All()); got != wantCount {
		t.Fatalf("len(All()) = %d, want %d", got, wantCount)
	}
}
