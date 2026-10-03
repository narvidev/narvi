//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// allocatedDuring returns how many bytes this process allocated while fn
// ran -- the server's reads, its reply, and the test's own decode alike.
func allocatedDuring(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// storeLargeWarnings stores count `warning` events of about 1 MiB each,
// as a sandbox can now write them (platform.MaxEventFrameBytes): each
// within the sandbox socket's read limit, and two over a page's budget.
func storeLargeWarnings(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID, count int) {
	t.Helper()
	message := strings.Repeat("w", 1_048_500)
	for i := 0; i < count; i++ {
		if _, err := rig.events.Create(ctx, sqlcgen.CreateEventParams{
			SessionID: sessionID,
			Type:      "warning",
			MessageID: fmt.Sprintf("large-%d", i),
			Payload:   []byte(fmt.Sprintf(`{"type":"warning","message":%q}`, message)),
		}); err != nil {
			t.Fatalf("store event %d: %v", i, err)
		}
	}
}

// TestListEvents_PageReadsOnlyTheEventsItSends: a page of the REST events
// route reads only the events it sends, not its whole count -- the budget
// bounds the read, not only the reply (technical plan §6.3) -- and paging
// through large events reads each once. Measured by what the process
// allocates: 36 events of about 1 MiB, asked for 500 at a time, are sent
// one to a page. Measured under -race: reading only the event sent takes
// about 11 MiB a page, its row, its reply and this test's decode, and about
// 400 MiB for the other 35 pages; reading each page's whole count, as the
// route did before the store measured events, took about 65 MiB for the
// first page and about 1.4 GiB for the others. The ceilings sit near the
// middle of each gap, about twice from either side.
func TestListEvents_PageReadsOnlyTheEventsItSends(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	_, token := rig.createAuthenticatedUser(ctx, t)
	const count = 36
	storeLargeWarnings(ctx, t, rig, session.ID, count)

	page := func(cursor string) (int, *string) {
		t.Helper()
		url := rig.server.URL + "/api/sessions/" + session.ID.String() + "/events?limit=500"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET events: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var got struct {
			Events     []json.RawMessage `json:"events"`
			NextCursor *string           `json:"nextCursor"`
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode page: %v", err)
		}
		return len(got.Events), got.NextCursor
	}

	var sent int
	var next *string
	first := allocatedDuring(func() { sent, next = page("") })
	if sent != 1 || next == nil {
		t.Fatalf("the first page sent %d events, nextCursor %v; want one event and a cursor", sent, next)
	}
	pages, total := 1, sent
	all := allocatedDuring(func() {
		for next != nil && pages <= count {
			var n int
			n, next = page(*next)
			total += n
			pages++
		}
	})
	if total != count || pages != count || next != nil {
		t.Fatalf("paging sent %d events in %d pages, ending on cursor %v; want %d events, one to a page, the last with no cursor", total, pages, next, count)
	}
	t.Logf("allocated %d MiB for the first page, %d MiB for the other %d", first>>20, all>>20, count-1)

	const firstPageCeiling = 32 << 20
	if first > firstPageCeiling {
		t.Fatalf("the first page allocated %d MiB to send one 1 MiB event, want under %d MiB: it read more events than it sent", first>>20, firstPageCeiling>>20)
	}
	const allPagesCeiling = 850 << 20
	if all > allPagesCeiling {
		t.Fatalf("paging the other %d events allocated %d MiB, want under %d MiB: pages read events they did not send", count-1, all>>20, allPagesCeiling>>20)
	}
}
