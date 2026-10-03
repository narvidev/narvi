package wsbridge_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

// TestRun_ReadLimitIsTheMaxFrameBytesItStates ties the read limit a real
// Bridge's connection enforces to the one its ready states as
// capabilities.maxFrameBytes, which the control plane holds every prompt
// to this gen to (technical plan §3.3, §6.1). On the connection the ready
// came on, a frame of exactly the stated size is read -- the prompt written
// behind it runs -- and a frame one byte longer closes the connection, so
// the agent reconnects. A read limit lowered below what the ready states
// would lose every prompt between the two, and one raised above it would
// read the longer frame: either fails here.
func TestRun_ReadLimitIsTheMaxFrameBytesItStates(t *testing.T) {
	t.Parallel()
	// A frame of the stated size is 32 MiB, which takes a while to read and
	// decode under -race.
	const wait = 60 * time.Second

	// probe is a command of a type the agent does not know, exactly n bytes
	// long: read, logged and skipped.
	probe := func(n int) []byte {
		const head, tail = `{"type":"read_limit_probe","pad":"`, `"}`
		return []byte(head + strings.Repeat("a", n-len(head)-len(tail)) + tail)
	}

	stated := make(chan int, 1)
	reconnected := make(chan []byte, 1)
	first := func(conn *websocket.Conn) {
		ready, err := serverRead(conn, wait)
		if err != nil {
			stated <- 0
			return
		}
		var r struct {
			Capabilities struct {
				MaxFrameBytes int `json:"maxFrameBytes"`
			} `json:"capabilities"`
		}
		if err := json.Unmarshal(ready, &r); err != nil || r.Capabilities.MaxFrameBytes <= 0 {
			stated <- 0
			return
		}
		n := r.Capabilities.MaxFrameBytes
		stated <- n
		for _, frame := range [][]byte{probe(n), []byte(promptFrame("after-the-stated-size", testGen, false)), probe(n + 1)} {
			if err := conn.Write(context.Background(), websocket.MessageText, frame); err != nil {
				return
			}
		}
		absorbForever(conn)
	}
	second := func(conn *websocket.Conn) {
		if ready, err := serverRead(conn, wait); err == nil {
			reconnected <- ready
		}
		absorbForever(conn)
	}
	server := httptest.NewServer(&stepServer{steps: []func(*websocket.Conn){first, second}, fallback: absorbForever})
	t.Cleanup(server.Close)

	counter := newPromptCounter()
	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", counter,
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
	stopBridge(t, bridge)

	n := waitChan(t, stated, wait)
	if n <= platform.DefaultFrameReadLimitBytes {
		t.Fatalf("the ready states maxFrameBytes %d, want a limit over the library's default", n)
	}
	// The frame of exactly the stated size was read on the ready's own
	// connection: the prompt written behind it ran.
	deadline := time.Now().Add(wait)
	for counter.count("after-the-stated-size") < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the prompt written behind a %d-byte frame never ran: the agent did not read a frame of the size its ready states", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// One byte more closed that connection: the agent reconnected.
	waitChan(t, reconnected, wait)
	if got := counter.count("after-the-stated-size"); got != 1 {
		t.Fatalf("the prompt ran %d times, want 1", got)
	}
}
