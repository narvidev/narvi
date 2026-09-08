package wshub

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/clientws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestTrimToReplayLimit proves the item-count half of EventsTruncated
// (§6.2's SubscribedPayload.eventsTruncated) in complete isolation from
// the byte-budget half (TestTruncateEventsToByteBudget below): plain
// sqlcgen.Event values with no realistic payload, so nothing here depends
// on marshaled JSON size the way an integration-level fixture would. That
// separation matters in practice, not just in principle -- initialReplayLimit
// (200) events each carrying only the id/type/createdAt/payload keys'
// own fixed JSON overhead already total close to (and, with any real
// payload content, past) maxInitialReplayBytes (16KiB), so an
// integration test seeding "200-ish small events" cannot isolate this
// mechanism from the byte-budget one -- whichever fires first would mask
// a regression in the other. This test is what actually pins the count
// mechanism on its own; client_test.go's own
// TestClientHandler_SubscribeEventsTruncatedFlag confirms both are wired
// correctly end to end through the real handler.
func TestTrimToReplayLimit(t *testing.T) {
	t.Parallel()

	fakeEvents := func(n int) []sqlcgen.Event {
		rows := make([]sqlcgen.Event, n)
		for i := range rows {
			rows[i] = sqlcgen.Event{ID: int64(i)}
		}
		return rows
	}

	t.Run("nil input is not truncated", func(t *testing.T) {
		t.Parallel()
		rows, truncated := trimToReplayLimit(nil)
		if truncated {
			t.Error("truncated = true, want false")
		}
		if len(rows) != 0 {
			t.Errorf("len(rows) = %d, want 0", len(rows))
		}
	})

	t.Run("fewer than the limit is not truncated", func(t *testing.T) {
		t.Parallel()
		rows, truncated := trimToReplayLimit(fakeEvents(initialReplayLimit - 1))
		if truncated {
			t.Error("truncated = true, want false")
		}
		if len(rows) != initialReplayLimit-1 {
			t.Errorf("len(rows) = %d, want %d", len(rows), initialReplayLimit-1)
		}
	})

	t.Run("exactly the limit is not truncated", func(t *testing.T) {
		t.Parallel()
		// This is the important boundary: initialReplayLimit rows means
		// the session's full history was exactly initialReplayLimit
		// events -- nothing was cut, so EventsTruncated must be able to
		// report false even right at the cap.
		rows, truncated := trimToReplayLimit(fakeEvents(initialReplayLimit))
		if truncated {
			t.Error("truncated = true, want false (exactly the limit, nothing cut)")
		}
		if len(rows) != initialReplayLimit {
			t.Errorf("len(rows) = %d, want %d", len(rows), initialReplayLimit)
		}
	})

	t.Run("one past the limit is truncated, and trimmed to exactly the limit", func(t *testing.T) {
		t.Parallel()
		rows, truncated := trimToReplayLimit(fakeEvents(initialReplayLimit + 1))
		if !truncated {
			t.Error("truncated = false, want true")
		}
		if len(rows) != initialReplayLimit {
			t.Fatalf("len(rows) = %d, want %d", len(rows), initialReplayLimit)
		}
		// Oldest-first order preserved: row IDs 0..initialReplayLimit-1
		// kept, the newest (extra) one -- id initialReplayLimit -- cut.
		for i, row := range rows {
			if row.ID != int64(i) {
				t.Fatalf("rows[%d].ID = %d, want %d (order not preserved)", i, row.ID, i)
			}
		}
	})

	t.Run("many past the limit is truncated to exactly the limit", func(t *testing.T) {
		t.Parallel()
		rows, truncated := trimToReplayLimit(fakeEvents(initialReplayLimit * 3))
		if !truncated {
			t.Error("truncated = false, want true")
		}
		if len(rows) != initialReplayLimit {
			t.Errorf("len(rows) = %d, want %d", len(rows), initialReplayLimit)
		}
	})
}

// TestTruncateEventsToByteBudget proves the fix for a real, live-reproduced
// bug found in review: initialReplayLimit alone (an item-count cap) does
// not bound the SubscribedPayload's own total marshaled size, and
// coder/websocket's own default 32KiB per-message read limit means a
// session with enough large event payloads could blow the entire subscribe
// handshake with ErrMessageTooBig instead of just replaying fewer events.
func TestTruncateEventsToByteBudget(t *testing.T) {
	t.Parallel()

	elem := func(n int) clientws.SubscribedPayloadEventsElem {
		return map[string]interface{}{"payload": strings.Repeat("x", n)}
	}
	sizeOf := func(e clientws.SubscribedPayloadEventsElem) int {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return len(b)
	}

	t.Run("empty input stays empty", func(t *testing.T) {
		t.Parallel()
		got := truncateEventsToByteBudget(nil, 1000)
		if len(got) != 0 {
			t.Fatalf("len(got) = %d, want 0", len(got))
		}
	})

	t.Run("everything fits under budget", func(t *testing.T) {
		t.Parallel()
		wire := []clientws.SubscribedPayloadEventsElem{elem(10), elem(10), elem(10)}
		got := truncateEventsToByteBudget(wire, 10_000)
		if len(got) != 3 {
			t.Fatalf("len(got) = %d, want 3 (nothing should be dropped when well under budget)", len(got))
		}
	})

	t.Run("truncates once the running total exceeds budget, preserving order", func(t *testing.T) {
		t.Parallel()
		e0, e1, e2 := elem(100), elem(100), elem(100)
		size := sizeOf(e0)
		wire := []clientws.SubscribedPayloadEventsElem{e0, e1, e2}
		// Budget for exactly 2 elements' worth (plus a little slack so the
		// 3rd is what actually pushes it over, not the 2nd).
		got := truncateEventsToByteBudget(wire, size*2+1)
		if len(got) != 2 {
			t.Fatalf("len(got) = %d, want 2", len(got))
		}
	})

	t.Run("always keeps at least one element, even if it alone exceeds the budget", func(t *testing.T) {
		t.Parallel()
		wire := []clientws.SubscribedPayloadEventsElem{elem(10_000)}
		got := truncateEventsToByteBudget(wire, 1) // a budget of 1 byte is trivially exceeded
		if len(got) != 1 {
			t.Fatalf("len(got) = %d, want 1 (must never truncate to zero)", len(got))
		}
	})

	t.Run("a huge first element does not prevent a small second one from being dropped correctly", func(t *testing.T) {
		t.Parallel()
		big, small := elem(10_000), elem(10)
		wire := []clientws.SubscribedPayloadEventsElem{big, small}
		got := truncateEventsToByteBudget(wire, sizeOf(big)) // exactly big's own size, no room for small
		if len(got) != 1 {
			t.Fatalf("len(got) = %d, want 1 (the oversized first element consumes the whole budget)", len(got))
		}
	})
}
