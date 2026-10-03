package wsbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

// boundStep scripts one connection of boundServer: the
// platform.MaxFrameBytesHeader its handshake states ("" for none), and how
// many frames after the agent's ready it reads before closing the
// connection (0: until the agent goes).
type boundStep struct {
	header string
	frames int
}

// receivedFrame is one frame boundServer read, after the connection's
// ready, and the 1-based number of the connection that carried it.
type receivedFrame struct {
	conn int
	data []byte
}

// boundServer is a fake control plane that states, or not, the largest
// message it reads, reads far more than that so a frame over the stated
// bound arrives as the agent wrote it, and records every frame.
type boundServer struct {
	mu       sync.Mutex
	steps    []boundStep
	fallback boundStep
	conns    int
	got      []receivedFrame
}

func (s *boundServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	step := s.fallback
	if len(s.steps) > 0 {
		step, s.steps = s.steps[0], s.steps[1:]
	}
	s.conns++
	n := s.conns
	s.mu.Unlock()

	if step.header != "" {
		w.Header().Set(platform.MaxFrameBytesHeader, step.header)
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(8 << 20)

	read := 0
	for step.frames == 0 || read < step.frames {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var env testEnvelope
		_ = json.Unmarshal(data, &env)
		if env.Type == "ready" || env.Type == "heartbeat" {
			continue
		}
		s.mu.Lock()
		s.got = append(s.got, receivedFrame{conn: n, data: data})
		s.mu.Unlock()
		read++
	}
}

// frames returns the frames read so far on connection n (0: every
// connection).
func (s *boundServer) frames(n int) []receivedFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []receivedFrame
	for _, f := range s.got {
		if n == 0 || f.conn == n {
			out = append(out, f)
		}
	}
	return out
}

// waitFrames waits until connection n has carried at least want frames.
func (s *boundServer) waitFrames(t *testing.T, n, want int) []receivedFrame {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		if got := s.frames(n); len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("connection %d carried %d frames, want %d", n, len(s.frames(n)), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startBoundBridge(t *testing.T, srv *boundServer) (*wsbridge.Bridge, context.Context) {
	t.Helper()
	server := httptest.NewServer(srv)
	t.Cleanup(server.Close)
	bridge := wsbridge.New(testSessionConfig("ws"+strings.TrimPrefix(server.URL, "http")), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
	ctx, cancel := context.WithCancel(context.Background())
	return bridge, startBridgeRun(ctx, t, cancel, bridge)
}

// startBridgeRun runs bridge until the test ends.
func startBridgeRun(ctx context.Context, t *testing.T, cancel context.CancelFunc, bridge *wsbridge.Bridge) context.Context {
	t.Helper()
	wait := runInBackground(ctx, bridge)
	t.Cleanup(func() {
		cancel()
		if err := wait(); err != nil {
			t.Errorf("Run() = %v, want nil after cancel", err)
		}
	})
	return ctx
}

// frameView is the part of a frame these tests read.
type frameView struct {
	Type      string `json:"type"`
	MessageID string `json:"messageId"`
	Text      string `json:"text"`
	Message   string `json:"message"`
	Cut       *struct {
		Kept  int `json:"kept"`
		Total int `json:"total"`
	} `json:"cut"`
}

func viewOf(t *testing.T, data []byte) frameView {
	t.Helper()
	var v frameView
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return v
}

func bigToken(messageID string, n int) sandboxws.Token {
	return sandboxws.Token{Type: "token", MessageId: messageID, SessionId: testSessionID, Gen: testGen, Text: strings.Repeat("plan line\n", n/10)}
}

// TestRunConnection_WriteBoundFromHeader: the agent holds every write on a
// connection to the largest message its control plane states it reads,
// and to the library's 32 KiB default when the handshake states none or
// states something that is not a positive integer. A 40 KiB token is
// written whole to a control plane that reads 1 MiB, and cut to fit every
// other.
func TestRunConnection_WriteBoundFromHeader(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		header    string
		wantWhole bool
		wantBound int
	}{
		{name: "present", header: strconv.Itoa(platform.MaxEventFrameBytes), wantWhole: true},
		{name: "present and below the frame", header: "36000", wantBound: 36000},
		{name: "absent", header: "", wantBound: platform.DefaultFrameReadLimitBytes},
		{name: "malformed", header: "1 MiB", wantBound: platform.DefaultFrameReadLimitBytes},
		{name: "zero", header: "0", wantBound: platform.DefaultFrameReadLimitBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := &boundServer{fallback: boundStep{header: tc.header}}
			bridge, ctx := startBoundBridge(t, srv)

			token := bigToken("prt_1", 40*1024)
			if err := bridge.SendBestEffort(ctx, token); err != nil {
				t.Fatalf("SendBestEffort: %v", err)
			}
			got := srv.waitFrames(t, 1, 1)[0]
			view := viewOf(t, got.data)
			if tc.wantWhole {
				if view.Cut != nil || view.Text != token.Text {
					t.Fatalf("the token was written cut (%d bytes), want it whole", len(got.data))
				}
				return
			}
			if len(got.data) > tc.wantBound || view.Cut == nil || view.Cut.Total != len(token.Text) {
				t.Fatalf("the token was written as %d bytes, cut %+v; want it cut to at most %d bytes", len(got.data), view.Cut, tc.wantBound)
			}
		})
	}
}

// TestSendCritical_OverDefaultReadLimit_RefusedNotBufferedWarned: a
// critical event is never cut, so one over the 32 KiB every control plane
// reads is refused: SendCritical returns ErrFrameTooLarge, nothing is
// buffered -- no reconnect ever replays it -- and a warning naming it goes
// out in its place. One of exactly 32 KiB is sent, and replayed until
// acked, as every critical event is.
func TestSendCritical_OverDefaultReadLimit_RefusedNotBufferedWarned(t *testing.T) {
	t.Parallel()

	srv := &boundServer{
		steps:    []boundStep{{header: strconv.Itoa(platform.MaxEventFrameBytes), frames: 2}},
		fallback: boundStep{header: strconv.Itoa(platform.MaxEventFrameBytes)},
	}
	bridge, ctx := startBoundBridge(t, srv)

	critical := func(messageID string, reasonBytes int) (sandboxws.ExecutionComplete, int) {
		reason := strings.Repeat("r", reasonBytes)
		msg := sandboxws.ExecutionComplete{Type: "execution_complete", MessageId: messageID, SessionId: testSessionID, Gen: testGen,
			AckId: "execution_complete:" + messageID, Outcome: sandboxws.ExecutionCompleteOutcomeFailed, Reason: &reason}
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return msg, len(raw)
	}
	over, overSize := critical("msg-over", 40*1024)
	err := bridge.SendCritical(ctx, over, over.AckId)
	if !errors.Is(err, wsbridge.ErrFrameTooLarge) {
		t.Fatalf("SendCritical(%d bytes) = %v, want ErrFrameTooLarge", overSize, err)
	}
	_, base := critical("msg-limit", 0)
	atLimit, atLimitSize := critical("msg-limit", platform.DefaultFrameReadLimitBytes-base)
	if atLimitSize != platform.DefaultFrameReadLimitBytes {
		t.Fatalf("built a %d-byte critical event, want exactly %d", atLimitSize, platform.DefaultFrameReadLimitBytes)
	}
	if err := bridge.SendCritical(ctx, atLimit, atLimit.AckId); err != nil {
		t.Fatalf("SendCritical(exactly %d bytes) = %v, want nil", atLimitSize, err)
	}

	first := srv.waitFrames(t, 1, 2)
	warning := viewOf(t, first[0].data)
	if warning.Type != "warning" || !strings.Contains(warning.Message, "execution_complete") || !strings.Contains(warning.Message, `"msg-over"`) ||
		!strings.Contains(warning.Message, strconv.Itoa(overSize)) || !strings.Contains(warning.Message, strconv.Itoa(platform.DefaultFrameReadLimitBytes)) {
		t.Fatalf("first frame = %+v, want a warning naming the type, messageId, size and bound", warning)
	}
	if v := viewOf(t, first[1].data); v.MessageID != "msg-limit" || len(first[1].data) != platform.DefaultFrameReadLimitBytes {
		t.Fatalf("second frame = %s (%d bytes), want the 32 KiB critical event", v.MessageID, len(first[1].data))
	}

	// The reconnect replays the buffer: the unacked critical event of
	// exactly the limit, and the warning; never the refused one.
	second := srv.waitFrames(t, 2, 2)
	time.Sleep(200 * time.Millisecond)
	for _, f := range srv.frames(2) {
		if v := viewOf(t, f.data); v.MessageID == "msg-over" {
			t.Fatal("the refused critical event was replayed: it was buffered")
		}
	}
	ids := map[string]bool{}
	for _, f := range second {
		ids[viewOf(t, f.data).MessageID] = true
	}
	if !ids["msg-limit"] || !ids[warning.MessageID] {
		t.Fatalf("the reconnect replayed %v, want the 32 KiB critical event and the warning", ids)
	}
}

// TestSendBestEffort_OverMaxEventFrameBytes_BufferedCut: a best-effort
// event over platform.MaxEventFrameBytes is cut to it before it is
// buffered, so no buffered entry is larger -- a connection that reads more
// is still written the cut -- and a connection that reads less is written
// a cut of that same string, which keeps the total the first cut recorded.
// One that cannot be cut is refused with ErrFrameTooLarge, warned about,
// and never buffered.
func TestSendBestEffort_OverMaxEventFrameBytes_BufferedCut(t *testing.T) {
	t.Parallel()

	srv := &boundServer{
		steps:    []boundStep{{header: strconv.Itoa(4 << 20), frames: 2}},
		fallback: boundStep{},
	}
	bridge, ctx := startBoundBridge(t, srv)

	token := bigToken("prt_big", 1_200_000)
	if err := bridge.SendBestEffort(ctx, token); err != nil {
		t.Fatalf("SendBestEffort(token) = %v, want nil: it is cut, not refused", err)
	}
	artifact := map[string]any{"type": "artifact", "messageId": "art-big", "sessionId": testSessionID, "gen": testGen, "data": strings.Repeat("a", 1_200_000)}
	if err := bridge.SendBestEffort(ctx, artifact); !errors.Is(err, wsbridge.ErrFrameTooLarge) {
		t.Fatalf("SendBestEffort(artifact) = %v, want ErrFrameTooLarge", err)
	}

	first := srv.waitFrames(t, 1, 2)
	cut := viewOf(t, first[0].data)
	if len(first[0].data) > platform.MaxEventFrameBytes || cut.Cut == nil || cut.Cut.Total != len(token.Text) || !strings.HasPrefix(token.Text, cut.Text[:cut.Cut.Kept]) {
		t.Fatalf("a connection that reads 4 MiB was written %d bytes, cut %+v; want the entry cut at enqueue to %d", len(first[0].data), cut.Cut, platform.MaxEventFrameBytes)
	}
	if w := viewOf(t, first[1].data); w.Type != "warning" || !strings.Contains(w.Message, `"art-big"`) {
		t.Fatalf("second frame = %+v, want the warning for the refused artifact", w)
	}

	second := srv.waitFrames(t, 2, 2)
	recut := viewOf(t, second[0].data)
	if len(second[0].data) > platform.DefaultFrameReadLimitBytes || recut.Cut == nil || recut.Cut.Total != len(token.Text) || recut.Cut.Kept >= cut.Cut.Kept ||
		!strings.HasPrefix(token.Text, recut.Text[:recut.Cut.Kept]) {
		t.Fatalf("a connection that states nothing was written %d bytes, cut %+v; want a re-cut of at most %d keeping total %d", len(second[0].data), recut.Cut, platform.DefaultFrameReadLimitBytes, len(token.Text))
	}
	for _, f := range srv.frames(0) {
		if viewOf(t, f.data).MessageID == "art-big" {
			t.Fatal("the refused artifact was written: it was buffered")
		}
	}
}

// TestFlushBuffer_OversizeNonCuttable_SkippedWarnedOnce_RestReplayed: an
// entry a connection cannot be written, and that no cut fits, is skipped
// by the replay, which goes on to every entry behind it; it stays
// buffered, and a later connection that reads more writes it whole. One
// warning goes out for it, however many connections skip it.
func TestFlushBuffer_OversizeNonCuttable_SkippedWarnedOnce_RestReplayed(t *testing.T) {
	t.Parallel()

	srv := &boundServer{
		steps:    []boundStep{{frames: 3}, {frames: 3}},
		fallback: boundStep{header: strconv.Itoa(platform.MaxEventFrameBytes)},
	}
	server := httptest.NewServer(srv)
	t.Cleanup(server.Close)
	bridge := wsbridge.New(testSessionConfig("ws"+strings.TrimPrefix(server.URL, "http")), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
	ctx, cancel := context.WithCancel(context.Background())

	// Buffered before any connection: the first replay writes them.
	small := func(id string) sandboxws.Token {
		return sandboxws.Token{Type: "token", MessageId: id, SessionId: testSessionID, Gen: testGen, Text: "short"}
	}
	oversize := map[string]any{"type": "artifact", "messageId": "art-40k", "sessionId": testSessionID, "gen": testGen, "data": strings.Repeat("a", 40*1024)}
	for _, msg := range []any{small("prt_a"), oversize, small("prt_b")} {
		if err := bridge.SendBestEffort(ctx, msg); err != nil {
			t.Fatalf("SendBestEffort: %v", err)
		}
	}
	startBridgeRun(ctx, t, cancel, bridge)

	ids := func(frames []receivedFrame) []string {
		var out []string
		for _, f := range frames {
			v := viewOf(t, f.data)
			if v.Type == "warning" {
				out = append(out, "warning:"+v.MessageID)
				continue
			}
			out = append(out, v.MessageID)
		}
		return out
	}
	first := ids(srv.waitFrames(t, 1, 3))
	if len(first) != 3 || first[0] != "prt_a" || first[1] != "prt_b" || !strings.HasPrefix(first[2], "warning:") {
		t.Fatalf("the first replay wrote %v, want prt_a, prt_b, then one warning: the oversize entry skipped, the rest replayed", first)
	}
	if w := viewOf(t, srv.frames(1)[2].data); !strings.Contains(w.Message, "artifact") || !strings.Contains(w.Message, `"art-40k"`) ||
		!strings.Contains(w.Message, strconv.Itoa(platform.DefaultFrameReadLimitBytes)) {
		t.Fatalf("warning = %q, want it to name the type, messageId and the bound", w.Message)
	}
	if second := ids(srv.waitFrames(t, 2, 3)); strings.Join(second, ",") != strings.Join(first, ",") {
		t.Fatalf("the second replay wrote %v, want %v: the same entries, and no second warning", second, first)
	}
	third := ids(srv.waitFrames(t, 3, 4))
	if strings.Join(third, ",") != strings.Join([]string{"prt_a", "art-40k", "prt_b", first[2]}, ",") {
		t.Fatalf("a connection that reads 1 MiB was replayed %v, want the oversize entry whole, in its place", third)
	}
	time.Sleep(200 * time.Millisecond)
	warnings := map[string]bool{}
	for _, id := range ids(srv.frames(0)) {
		if strings.HasPrefix(id, "warning:") {
			warnings[id] = true
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("%d distinct warnings went out, want one for the one skipped entry", len(warnings))
	}
}
