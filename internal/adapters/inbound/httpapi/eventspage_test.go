package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestEventsPageLen pins the REST events page's byte budget, the twin of
// wshub's historyPageLen (technical plan §6.3): the longest run from the
// first event whose encoded events, each counted with the comma after it,
// stay within the budget, and never fewer than one event.
func TestEventsPageLen(t *testing.T) {
	t.Parallel()

	event := func(id int64, payloadBytes int) sqlcgen.Event {
		return sqlcgen.Event{ID: id, Type: "token", Payload: []byte(`{"text":"` + strings.Repeat("x", payloadBytes) + `"}`)}
	}
	rows := []sqlcgen.Event{event(1, 100), event(2, 100), event(3, 100), event(4, 100)}
	b, err := json.Marshal(eventWireMap(rows[0]))
	if err != nil {
		t.Fatal(err)
	}
	one := len(b) + 1

	for _, tc := range []struct {
		name   string
		rows   []sqlcgen.Event
		budget int
		want   int
	}{
		{name: "no rows", rows: nil, budget: 10, want: 0},
		{name: "every row fits", rows: rows, budget: 4 * one, want: 4},
		{name: "a budget of exactly three rows", rows: rows, budget: 3 * one, want: 3},
		{name: "one byte short of three rows", rows: rows, budget: 3*one - 1, want: 2},
		{name: "a first row over the budget is still sent", rows: []sqlcgen.Event{event(1, 5000), event(2, 1)}, budget: 100, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := eventsPageLen(tc.rows, tc.budget); got != tc.want {
				t.Fatalf("eventsPageLen(%d rows, %d) = %d, want %d", len(tc.rows), tc.budget, got, tc.want)
			}
		})
	}
}
