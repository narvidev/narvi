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
		want             bool
	}{
		{
			name:             "same verdict id, not revoked: applicable",
			acceptance:       reviewverdict.Acceptance{VerdictID: "verdict-1"},
			currentVerdictID: "verdict-1",
			want:             true,
		},
		{
			name:             "a NEW attempt posted a new verdict: different id, inapplicable",
			acceptance:       reviewverdict.Acceptance{VerdictID: "verdict-1"},
			currentVerdictID: "verdict-2",
			want:             false,
		},
		{
			name:             "revoked, even though the verdict id still matches: inapplicable",
			acceptance:       reviewverdict.Acceptance{VerdictID: "verdict-1", RevokedAt: &revokedAt},
			currentVerdictID: "verdict-1",
			want:             false,
		},
		{
			name:             "acceptance's own VerdictID unset (should never happen for a real row): inapplicable",
			acceptance:       reviewverdict.Acceptance{VerdictID: ""},
			currentVerdictID: "",
			want:             false,
		},
		{
			name:             "no verdict at all right now (currentVerdictID empty): inapplicable",
			acceptance:       reviewverdict.Acceptance{VerdictID: "verdict-1"},
			currentVerdictID: "",
			want:             false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.acceptance.Applicable(tc.currentVerdictID)
			if got != tc.want {
				t.Errorf("Applicable(%q) = %v, want %v", tc.currentVerdictID, got, tc.want)
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
