//go:build integration

package wshub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/contracts/gen/go/clientws"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/platform"
)

// wireEvent is one element of a `subscribed` or `fetch_history` reply
// (eventWireMap, client.go), decoded as far as this file needs.
type wireEvent struct {
	ID      int64           `json:"id"`
	Type    string          `json:"type"`
	Payload sandboxws.Token `json:"payload"`
}

// decodeWireEvents re-decodes a reply's schema-generic event elements.
func decodeWireEvents(t *testing.T, elems any) []wireEvent {
	t.Helper()
	raw, err := json.Marshal(elems)
	if err != nil {
		t.Fatalf("marshal reply events: %v", err)
	}
	var out []wireEvent
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode reply events: %v (%s)", err, raw)
	}
	return out
}

// foldTokenText folds `token` events the way every reader of the log does
// (web timelineModel, plan.ExtractContent): by the payload's messageId,
// in id order, the newest frame replacing the older ones.
func foldTokenText(events []wireEvent) map[string]string {
	sorted := append([]wireEvent(nil), events...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	out := map[string]string{}
	for _, e := range sorted {
		if e.Type == "token" {
			out[e.Payload.MessageId] = e.Payload.Text
		}
	}
	return out
}

// TestTokenFrames_SandboxToClients_EveryReaderGetsTheFinalText drives
// streamed text from a real sandbox socket to real web clients through
// the real actor and store. Every frame of a part shares the part's
// messageId; before per-frame storage only the first frame of each part
// was stored and broadcast, so every reader below saw a blank or a prefix.
// It proves the three ways a web client reads the log:
//   - live: N frames in, N broadcasts out, in order;
//   - backfill: a client whose cursor was taken after the first frame
//     fetches the rest with fetch_history -- the append-only property a
//     payload replaced in place (same row id) would break;
//   - reload: a fresh `subscribed` replay folds to the final text.
func TestTokenFrames_SandboxToClients_EveryReaderGetsTheFinalText(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	timeouts.ClientFetchHistoryMinInterval = 0
	rig, sessionRow := newClientTestRigWithBroadcast(t, timeouts)
	ctx := context.Background()
	sessionID := sessionRow.ID.String()
	if _, err := rig.sandboxes.Create(ctx, sessionRow.ID); err != nil {
		t.Fatalf("create test sandbox: %v", err)
	}

	const (
		planFull = "1. Add the migration\n2. Wire the store\n3. Tests"
		noteFull = "Summary: 3 files touched, all tests green."
	)
	frames := []struct{ part, text string }{
		{"prt_plan", ""},
		{"prt_plan", "1. Add the"},
		{"prt_plan", planFull},
		{"prt_note", "Summary: 3 files"},
		{"prt_note", noteFull},
	}
	wantFinal := map[string]string{"prt_plan": planFull, "prt_note": noteFull}

	token := func() string {
		return createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))
	}
	live := subscribeClient(ctx, t, rig.wsURL, sessionID, token())
	defer func() { _ = live.CloseNow() }()

	header := http.Header{}
	for k, v := range baseHeaders() {
		header.Set(k, v)
	}
	sandbox, _, err := websocket.Dial(ctx, rig.wsURL+"/sessions/"+sessionID+"/ws?type=sandbox",
		&websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial sandbox socket: %v", err)
	}
	defer func() { _ = sandbox.CloseNow() }()

	send := func(i int) {
		t.Helper()
		raw, err := json.Marshal(sandboxws.Token{
			Type: "token", MessageId: frames[i].part, SessionId: sessionID, Gen: 1, Text: frames[i].text,
		})
		if err != nil {
			t.Fatalf("marshal frame %d: %v", i, err)
		}
		if err := sandbox.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}
	// readTokenBroadcast reads conn until the next `token` broadcast.
	readTokenBroadcast := func(conn *websocket.Conn, who string) sandboxws.Token {
		t.Helper()
		for {
			rc, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, data, err := conn.Read(rc)
			cancel()
			if err != nil {
				t.Fatalf("%s: read broadcast: %v", who, err)
			}
			var tok sandboxws.Token
			if json.Unmarshal(data, &tok) == nil && tok.Type == "token" {
				return tok
			}
		}
	}

	// Frame 1 is stored and broadcast; a second client then subscribes, so
	// its replay -- and the cursor it backfills from -- stop at frame 1.
	send(0)
	liveTexts := []string{readTokenBroadcast(live, "live client").Text}

	late, lateSubscribed := dialAndSubscribe(ctx, t, rig.wsURL, sessionID, token())
	defer func() { _ = late.CloseNow() }()
	lateReplay := decodeWireEvents(t, lateSubscribed.Events)
	var cursor int64
	for _, e := range lateReplay {
		cursor = max(cursor, e.ID)
	}
	if got := foldTokenText(lateReplay); got["prt_plan"] != "" || len(got) != 1 {
		t.Fatalf("late client's replay = %q, want only prt_plan's first (empty) frame", got)
	}

	for i := 1; i < len(frames); i++ {
		send(i)
	}

	// Live: every frame broadcast, in order.
	for len(liveTexts) < len(frames) {
		liveTexts = append(liveTexts, readTokenBroadcast(live, fmt.Sprintf("live client after %d broadcasts %q", len(liveTexts), liveTexts)).Text)
	}
	for i, f := range frames {
		if liveTexts[i] != f.text {
			t.Errorf("live broadcast %d text = %q, want %q", i, liveTexts[i], f.text)
		}
	}

	// Backfill: the late client drains its live signals, then fetches
	// everything after its cursor, as web/src/ws/sessionStream.ts does.
	for i := 1; i < len(frames); i++ {
		readTokenBroadcast(late, "late client")
	}
	msg := fmt.Sprintf(`{"type":"fetch_history","sessionId":%q,"cursor":%s,"limit":100}`, sessionID, strconv.Quote(strconv.FormatInt(cursor, 10)))
	if err := late.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatalf("write fetch_history: %v", err)
	}
	rc, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, data, err := late.Read(rc)
	cancel()
	if err != nil {
		t.Fatalf("read fetch_history response: %v", err)
	}
	var history clientws.FetchHistoryResponse
	if err := json.Unmarshal(data, &history); err != nil {
		t.Fatalf("unmarshal fetch_history response: %v (%s)", err, data)
	}
	fetched := decodeWireEvents(t, history.Events)
	if len(fetched) != len(frames)-1 {
		t.Errorf("fetch_history after frame 1 returned %d events, want %d (one per later frame)", len(fetched), len(frames)-1)
	}
	if got := foldTokenText(append(lateReplay, fetched...)); fmt.Sprint(got) != fmt.Sprint(wantFinal) {
		t.Errorf("late client's folded text = %q, want %q", got, wantFinal)
	}

	// Reload: a fresh subscribe replays every stored frame; folded, the
	// finals.
	fresh, freshSubscribed := dialAndSubscribe(ctx, t, rig.wsURL, sessionID, token())
	defer func() { _ = fresh.CloseNow() }()
	if got := foldTokenText(decodeWireEvents(t, freshSubscribed.Events)); fmt.Sprint(got) != fmt.Sprint(wantFinal) {
		t.Errorf("subscribed replay folds to %q, want %q", got, wantFinal)
	}
	if got := countEvents(ctx, t, rig.pool, sessionRow.ID, "token"); got != len(frames) {
		t.Errorf("stored token rows = %d, want %d (one per distinct frame)", got, len(frames))
	}
}
