//go:build integration

package wshub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/contracts/gen/go/clientws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// allocatedDuring returns how many bytes this process allocated while fn
// ran -- the server's reads, its reply, and the test's own reads alike.
func allocatedDuring(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestClientHandler_HistoryReadsOnlyTheEventsItSends: the subscribe replay
// and a fetch_history page read only the events they send, not their whole
// count -- the budget bounds the read, not only the reply (technical plan
// §6.2) -- and paging through large events reads each once. Measured by
// what the process allocates: 36 events of about 1 MiB each, which a
// sandbox can now write. The replay sends one of them, and so does each
// page of 500. Measured under -race: reading only the event sent takes
// about 13 to 15 MiB for the replay or a page, and about 500 MiB for the
// other 35 pages; reading the replay's 201 rows, or a page's whole count,
// as the hub did before the store measured events, took about 67 MiB for
// the replay or the first page and about 1.5 GiB for the others. The
// ceilings sit near the middle of each gap, about twice from either side.
func TestClientHandler_HistoryReadsOnlyTheEventsItSends(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	timeouts.ClientFetchHistoryMinInterval = 0
	rig, sessionRow := newClientTestRig(t, timeouts)
	ctx := context.Background()
	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))

	const count = 36
	message := strings.Repeat("w", 1_048_500)
	for i := 0; i < count; i++ {
		if _, err := rig.events.Create(ctx, sqlcgen.CreateEventParams{
			SessionID: sessionRow.ID,
			Type:      "warning",
			MessageID: fmt.Sprintf("large-%d", i),
			Payload:   []byte(fmt.Sprintf(`{"type":"warning","message":%q}`, message)),
		}); err != nil {
			t.Fatalf("store event %d: %v", i, err)
		}
	}

	// A browser's WebSocket sets no per-message limit; this client reads
	// as much, since every reply here carries a 1 MiB event.
	var conn *websocket.Conn
	var replayed clientws.SubscribedPayload
	subscribe := allocatedDuring(func() {
		var err error
		conn, _, err = websocket.Dial(ctx, rig.wsURL+"/sessions/"+sessionRow.ID.String()+"/ws?type=client", nil)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		conn.SetReadLimit(8 << 20)
		raw, err := json.Marshal(clientws.SubscribeRequest{Token: token, ClientId: "test-client"})
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatalf("Write subscribe: %v", err)
		}
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, data, err := conn.Read(readCtx)
		if err != nil {
			t.Fatalf("Read subscribed reply: %v", err)
		}
		if err := json.Unmarshal(data, &replayed); err != nil {
			t.Fatalf("decode subscribed reply: %v", err)
		}
	})
	defer func() { _ = conn.CloseNow() }()
	if len(replayed.Events) != 1 || !replayed.EventsTruncated {
		t.Fatalf("the replay sent %d events, truncated %v; want one event, truncated", len(replayed.Events), replayed.EventsTruncated)
	}

	fetch := func(cursor *string) clientws.FetchHistoryResponse {
		t.Helper()
		cursorJSON := "null"
		if cursor != nil {
			cursorJSON = fmt.Sprintf("%q", *cursor)
		}
		msg := fmt.Sprintf(`{"type":"fetch_history","sessionId":%q,"cursor":%s,"limit":500}`, sessionRow.ID.String(), cursorJSON)
		if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatalf("Write fetch_history: %v", err)
		}
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, data, err := conn.Read(readCtx)
		if err != nil {
			t.Fatalf("Read fetch_history response: %v", err)
		}
		var resp clientws.FetchHistoryResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			t.Fatalf("decode fetch_history response: %v", err)
		}
		return resp
	}

	var resp clientws.FetchHistoryResponse
	first := allocatedDuring(func() { resp = fetch(nil) })
	if len(resp.Events) != 1 || resp.NextCursor == nil {
		t.Fatalf("the first page sent %d events, nextCursor %v; want one event and a cursor", len(resp.Events), resp.NextCursor)
	}

	pages, total, next := 1, len(resp.Events), resp.NextCursor
	all := allocatedDuring(func() {
		for next != nil && pages <= count {
			page := fetch(next)
			total += len(page.Events)
			next = page.NextCursor
			pages++
		}
	})
	if total != count || pages != count || next != nil {
		t.Fatalf("paging sent %d events in %d pages, ending on cursor %v; want %d events, one to a page, the last with no cursor", total, pages, next, count)
	}
	t.Logf("allocated %d MiB for the replay, %d MiB for the first page, %d MiB for the other %d", subscribe>>20, first>>20, all>>20, count-1)

	const oneEventCeiling = 32 << 20
	if subscribe > oneEventCeiling {
		t.Fatalf("the replay allocated %d MiB to send one 1 MiB event, want under %d MiB: it read more events than it sent", subscribe>>20, oneEventCeiling>>20)
	}
	if first > oneEventCeiling {
		t.Fatalf("the first page allocated %d MiB to send one 1 MiB event, want under %d MiB: it read more events than it sent", first>>20, oneEventCeiling>>20)
	}
	const allPagesCeiling = 850 << 20
	if all > allPagesCeiling {
		t.Fatalf("paging the other %d events allocated %d MiB, want under %d MiB: pages read events they did not send", count-1, all>>20, allPagesCeiling>>20)
	}
}
