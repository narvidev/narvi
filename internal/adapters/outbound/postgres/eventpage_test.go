package postgres

import (
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestEventPageFit pins which walked events a page holds: every one whose
// running size is within the budget, and the first whatever its size; the
// walk's last event, when it takes the sum past the budget, is not held and
// marks the page as stopped at its budget.
func TestEventPageFit(t *testing.T) {
	t.Parallel()

	walk := func(running ...int64) []sqlcgen.ListEventPageExtentForSessionRow {
		rows := make([]sqlcgen.ListEventPageExtentForSessionRow, len(running))
		for i, r := range running {
			rows[i] = sqlcgen.ListEventPageExtentForSessionRow{ID: int64(10 + i), N: int64(i + 1), Running: r}
		}
		return rows
	}
	for _, tc := range []struct {
		name        string
		extent      []sqlcgen.ListEventPageExtentForSessionRow
		budget      int64
		wantFit     int
		wantStopped bool
	}{
		{name: "nothing walked", extent: nil, budget: 100, wantFit: 0},
		{name: "every event fits", extent: walk(30, 60, 90), budget: 100, wantFit: 3},
		{name: "exactly the budget fits", extent: walk(50, 100), budget: 100, wantFit: 2},
		{name: "the walk stopped on an event past the budget", extent: walk(50, 90, 140), budget: 100, wantFit: 2, wantStopped: true},
		{name: "a first event alone over the budget is still held", extent: walk(500, 520), budget: 100, wantFit: 1, wantStopped: true},
		{name: "a first event alone over the budget, and nothing after", extent: walk(500), budget: 100, wantFit: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fit, stopped := eventPageFit(tc.extent, tc.budget)
			if fit != tc.wantFit || stopped != tc.wantStopped {
				t.Fatalf("eventPageFit = %d, %v; want %d, %v", fit, stopped, tc.wantFit, tc.wantStopped)
			}
		})
	}
}
