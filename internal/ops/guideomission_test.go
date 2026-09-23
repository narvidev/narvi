package ops

import "testing"

// TestRouteGuideExemptions_RealReasonsPassVacuousCheck is G4's own
// zero-false-positive proof: reasonClausesAllVacuous (the clause-level
// denylist check added alongside the pre-existing whole-string exact
// match) must never flag one of the real, hand-written
// RouteGuideExemptions reasons -- every one of them makes a substantive
// claim (a file, a route, a caller), so at least one clause always
// survives clause-splitting un-denylisted. A failure here would mean the
// real register itself no longer passes TestNoGuideDrift.
func TestRouteGuideExemptions_RealReasonsPassVacuousCheck(t *testing.T) {
	if len(RouteGuideExemptions) == 0 {
		t.Fatal("RouteGuideExemptions is empty -- this test would be vacuous")
	}
	for _, ex := range RouteGuideExemptions {
		if vacuousExemptionReasons[normalizeReasonForCheck(ex.Reason)] {
			t.Errorf("route %q: whole-string exact-match denylist flagged a real reason: %q", ex.Route, ex.Reason)
		}
		if reasonClausesAllVacuous(ex.Reason) {
			t.Errorf("route %q: clause-level denylist flagged a real reason: %q", ex.Route, ex.Reason)
		}
	}
}

// TestReasonClausesAllVacuous is G4's table-driven proof of
// reasonClausesAllVacuous itself: which shapes it catches (punctuation-
// joined concatenation, hyphen-padding) and which it -- honestly,
// per its own doc comment -- still does not (a concatenation with no
// punctuation at all between the stock phrases).
func TestReasonClausesAllVacuous(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   bool
	}{
		{"empty string", "", false},
		{"single real clause", "webhook receiver an external system posts to", false},
		{"single denylisted word, no punctuation", "internal", true},
		{"comma-joined concatenation of denylisted phrases", "internal only, not applicable, admin only", true},
		{"hyphen-padded repetition of one denylisted phrase", "internal-internal-internal-internal", true},
		{"semicolon-joined concatenation", "n/a; todo; tbd", true},
		{"one real clause among denylisted ones stays not-vacuous", "internal only, posts to httpapi/foo.go, admin only", false},
		{"space-only concatenation is the known remaining gap", "internal only not applicable admin only", false},
		{"denylisted phrase with its own internal hyphen fragments unevenly", "not user-facing, not applicable, internal use only", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reasonClausesAllVacuous(tt.reason); got != tt.want {
				t.Errorf("reasonClausesAllVacuous(%q) = %v, want %v", tt.reason, got, tt.want)
			}
		})
	}
}
