package reviewverdict_test

import (
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

func TestAcceptance_Applicable(t *testing.T) {
	t.Parallel()

	revokedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name             string
		acceptance       reviewverdict.Acceptance
		currentVerdictID string
		hasNewerAttempt  bool
		want             bool
	}{
		{
			name:             "same verdict id, not revoked, no newer attempt: applicable",
			acceptance:       reviewverdict.Acceptance{VerdictID: "verdict-1"},
			currentVerdictID: "verdict-1",
			hasNewerAttempt:  false,
			want:             true,
		},
		{
			name:             "a NEW attempt posted a new verdict: different id, inapplicable",
			acceptance:       reviewverdict.Acceptance{VerdictID: "verdict-1"},
			currentVerdictID: "verdict-2",
			hasNewerAttempt:  false,
			want:             false,
		},
		{
			// Finding F1 (adversarial review), reproduced end to end
			// against real Postgres: a second attempt at the SAME head
			// ending terminal_not_assessed posts NO review_verdicts row
			// at all, so GetLatest still returns the OLD, accepted
			// verdict -- currentVerdictID stays UNCHANGED. Before this
			// fix, Applicable took only currentVerdictID and had no way
			// to see this at all; this is the DECISIVE case that proves
			// hasNewerAttempt, not verdict-id equality, is what must
			// refuse here.
			name:             "same verdict id still latest, but a newer attempt ran and posted NOTHING (not_assessed): inapplicable",
			acceptance:       reviewverdict.Acceptance{VerdictID: "verdict-1", AttemptID: "attempt-1"},
			currentVerdictID: "verdict-1",
			hasNewerAttempt:  true,
			want:             false,
		},
		{
			name:             "revoked, even though the verdict id still matches and no newer attempt: inapplicable",
			acceptance:       reviewverdict.Acceptance{VerdictID: "verdict-1", RevokedAt: &revokedAt},
			currentVerdictID: "verdict-1",
			hasNewerAttempt:  false,
			want:             false,
		},
		{
			name:             "acceptance's own VerdictID unset (should never happen for a real row): inapplicable",
			acceptance:       reviewverdict.Acceptance{VerdictID: ""},
			currentVerdictID: "",
			hasNewerAttempt:  false,
			want:             false,
		},
		{
			name:             "no verdict at all right now (currentVerdictID empty): inapplicable",
			acceptance:       reviewverdict.Acceptance{VerdictID: "verdict-1"},
			currentVerdictID: "",
			hasNewerAttempt:  false,
			want:             false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.acceptance.Applicable(tc.currentVerdictID, tc.hasNewerAttempt)
			if got != tc.want {
				t.Errorf("Applicable(%q, hasNewerAttempt=%v) = %v, want %v", tc.currentVerdictID, tc.hasNewerAttempt, got, tc.want)
			}
		})
	}
}

func TestAcceptance_Revoked(t *testing.T) {
	t.Parallel()

	revokedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if (reviewverdict.Acceptance{}).Revoked() {
		t.Error("zero-value Acceptance (RevokedAt nil) reports Revoked() = true, want false")
	}
	if !(reviewverdict.Acceptance{RevokedAt: &revokedAt}).Revoked() {
		t.Error("Acceptance with a non-nil RevokedAt reports Revoked() = false, want true")
	}
}
