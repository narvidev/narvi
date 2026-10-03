//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// pageWireSize is an event's exact size in a page as the control plane
// writes one -- the client WS hub's and the REST route's eventWireMap,
// marshaled, and the comma after it.
func pageWireSize(t *testing.T, e sqlcgen.Event) int {
	t.Helper()
	b, err := json.Marshal(map[string]any{"id": e.ID, "type": e.Type, "payload": json.RawMessage(e.Payload), "createdAt": e.CreatedAt})
	if err != nil {
		t.Fatalf("marshal event %d: %v", e.ID, err)
	}
	return len(b) + 1
}

// storeWarnings stores count warning events of type eventType, `warning`
// when empty, whose message is text repeated to about size bytes, and
// returns their ids in order.
func storeWarnings(ctx context.Context, t *testing.T, events *narvipg.EventStore, sessionID pgtype.UUID, prefix, eventType string, count, size int, text string) []int64 {
	t.Helper()
	if eventType == "" {
		eventType = "warning"
	}
	ids := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		message := strings.Repeat(text, size/len(text))
		created, err := events.Create(ctx, sqlcgen.CreateEventParams{
			SessionID: sessionID,
			Type:      eventType,
			MessageID: fmt.Sprintf("%s-%d", prefix, i),
			Payload:   []byte(fmt.Sprintf(`{"type":"warning","message":%q}`, message)),
		})
		if err != nil {
			t.Fatalf("store event %d: %v", i, err)
		}
		ids = append(ids, created.ID)
	}
	return ids
}

// TestEventStore_ListPageForSession_ReadsOnlyWhatFits pins the read a page
// makes (technical plan §6.2, §6.3): the events the store returns for one
// page fit its byte budget as the page carries them -- or are one event,
// when the first alone does not -- so a page never reads an event it then
// drops, and paging through a log reads each event exactly once, however
// large. Before the store measured events, a page read its whole count,
// up to 500 events of up to 1 MiB each, and the next page read every
// dropped one again.
func TestEventStore_ListPageForSession_ReadsOnlyWhatFits(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	events := narvipg.NewEventStore(pool)

	for _, tc := range []struct {
		name      string
		eventType string
		count     int
		size      int
		text      string
		maxRows   int32
		budget    int64
		wantPages []int
	}{
		{name: "events of 600 KiB: three to a 2 MiB page", count: 8, size: 600 * 1024, text: "w", maxRows: 500,
			budget: platform.FetchHistoryMaxReplyBytes, wantPages: []int{3, 3, 2}},
		{name: "events of nearly 1 MiB: one to a page", count: 4, size: 1_048_500, text: "w", maxRows: 500,
			budget: platform.FetchHistoryMaxReplyBytes, wantPages: []int{1, 1, 1, 1}},
		{name: "a first event alone over the budget is still read, and alone", count: 3, size: 5000, text: "w", maxRows: 500,
			budget: 1000, wantPages: []int{1, 1, 1}},
		{name: "the count bounds a page of small events", count: 10, size: 100, text: "w", maxRows: 4,
			budget: platform.FetchHistoryMaxReplyBytes, wantPages: []int{4, 4, 2}},
		{name: "escape-heavy events are measured as written, six bytes a character", count: 12, size: 60 * 1024, text: "<&>", maxRows: 500,
			budget: platform.FetchHistoryMaxReplyBytes},
		// A type is measured as written too: each of these 100 bytes is
		// written as \u003c or \u0001, so the type takes 600 bytes of a
		// page, not 100, and an event about 700 -- a budget of 2000 holds
		// two, where a type counted by its length would let seven in.
		{name: "an escape-heavy type is measured as written, six bytes a byte", eventType: strings.Repeat("<\u0001", 50), count: 6, size: 10, text: "w", maxRows: 500,
			budget: 2000, wantPages: []int{2, 2, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			ids := storeWarnings(ctx, t, events, sessionID, "page", tc.eventType, tc.count, tc.size, tc.text)

			var read []int64
			var pages []int
			var after int64
			for len(pages) <= tc.count {
				page, err := events.ListPageForSession(ctx, sessionID, after, tc.maxRows, tc.budget)
				if err != nil {
					t.Fatalf("ListPageForSession: %v", err)
				}
				if len(page.Events) == 0 {
					if page.StoppedAtBudget {
						t.Fatal("an empty page reports a stop at its budget")
					}
					break
				}
				size := 0
				for _, e := range page.Events {
					size += pageWireSize(t, e)
					read = append(read, e.ID)
				}
				if len(page.Events) > 1 && int64(size) > tc.budget {
					t.Fatalf("page %d read %d events of %d bytes as written, over its %d-byte budget", len(pages)+1, len(page.Events), size, tc.budget)
				}
				if len(page.Events) > int(tc.maxRows) {
					t.Fatalf("page %d read %d events, over its count of %d", len(pages)+1, len(page.Events), tc.maxRows)
				}
				last := len(read) == tc.count
				if page.StoppedAtBudget == last && len(page.Events) < int(tc.maxRows) {
					t.Fatalf("page %d stopped at its budget = %v, with %d of %d events read", len(pages)+1, page.StoppedAtBudget, len(read), tc.count)
				}
				pages = append(pages, len(page.Events))
				after = page.Events[len(page.Events)-1].ID
			}
			if fmt.Sprint(read) != fmt.Sprint(ids) {
				t.Fatalf("paging read events %v, want each of %v exactly once, in order", read, ids)
			}
			if tc.wantPages != nil && fmt.Sprint(pages) != fmt.Sprint(tc.wantPages) {
				t.Fatalf("pages held %v events, want %v", pages, tc.wantPages)
			}

			// The walk measures the page's own events and at most one more,
			// never the rest of its count.
			extent, err := sqlcgen.New(pool).ListEventPageExtentForSession(ctx, sqlcgen.ListEventPageExtentForSessionParams{
				SessionID: sessionID, AfterID: 0, MaxRows: int64(tc.maxRows), MaxBytes: tc.budget,
			})
			if err != nil {
				t.Fatalf("ListEventPageExtentForSession: %v", err)
			}
			if len(extent) > pages[0]+1 {
				t.Fatalf("the first page's walk measured %d events for a page of %d, want at most one more", len(extent), pages[0])
			}
		})
	}
}
