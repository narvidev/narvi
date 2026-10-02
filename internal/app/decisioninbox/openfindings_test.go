package decisioninbox

import (
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestTallyOpenFindings pins the inbox's open-findings split (§26.6's
// amendment): the merge gate's count (Blocking) takes every finding whose
// status still blocks merge, unverified additions included, and the count
// the inbox shows leaves those additions out, counting them apart -- the
// same rule as the posted comment, the readout and the Code review view.
func TestTallyOpenFindings(t *testing.T) {
	str := func(s string) *string { return &s }
	finding := func(status string, source, check *string) sqlcgen.ReviewFinding {
		return sqlcgen.ReviewFinding{Status: status, ReportedSource: source, AdditionCheck: check}
	}
	tests := []struct {
		name           string
		rows           []sqlcgen.ReviewFinding
		wantBlocking   int
		wantUnverified int
		wantDisplayed  int
	}{
		{"no findings", nil, 0, 0, 0},
		{
			name: "primary, no source recorded and a checked addition are plain findings",
			rows: []sqlcgen.ReviewFinding{
				finding("open", str("primary"), nil),
				finding("open", nil, nil),
				finding("open", str("counter_review"), str("checked")),
			},
			wantBlocking: 3, wantUnverified: 0, wantDisplayed: 3,
		},
		{
			name: "unverified additions block merge but are counted apart",
			rows: []sqlcgen.ReviewFinding{
				finding("open", str("primary"), nil),
				finding("open", str("counter_review"), str("not_found")),
				finding("fix_recorded", str("counter_review"), str("unconfirmed")),
				finding("open", str("counter_review"), nil),
			},
			wantBlocking: 4, wantUnverified: 3, wantDisplayed: 1,
		},
		{
			name: "only unverified additions: nothing shown as a finding, still blocking",
			rows: []sqlcgen.ReviewFinding{
				finding("open", str("counter_review"), str("not_run")),
			},
			wantBlocking: 1, wantUnverified: 1, wantDisplayed: 0,
		},
		{
			name: "a resolved finding counts nowhere, an unverified addition included",
			rows: []sqlcgen.ReviewFinding{
				finding("rebutted", str("counter_review"), str("not_found")),
				finding("rebutted", str("primary"), nil),
			},
			wantBlocking: 0, wantUnverified: 0, wantDisplayed: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tallyOpenFindings(tt.rows)
			if got.Blocking != tt.wantBlocking || got.UnverifiedAdditions != tt.wantUnverified || got.Displayed() != tt.wantDisplayed {
				t.Errorf("tallyOpenFindings() = blocking %d, unverified %d, displayed %d; want %d, %d, %d",
					got.Blocking, got.UnverifiedAdditions, got.Displayed(), tt.wantBlocking, tt.wantUnverified, tt.wantDisplayed)
			}
		})
	}
}
