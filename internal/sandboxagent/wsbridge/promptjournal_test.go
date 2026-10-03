package wsbridge_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// Technical plan §3.3's prompt receipts, the agent's half (doc.go, "Prompt
// receipts and the dedup journal"): a real Bridge against a scripted fake
// control plane, with a handler counting how often each prompt runs.

// promptCounter is a wsbridge.CommandHandler counting HandlePrompt calls
// per messageId.
type promptCounter struct {
	noopHandler
	mu   sync.Mutex
	runs map[string]int
}

func newPromptCounter() *promptCounter { return &promptCounter{runs: make(map[string]int)} }

func (c *promptCounter) HandlePrompt(_ context.Context, cmd sandboxws.Prompt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runs[cmd.MessageId]++
}

func (c *promptCounter) count(messageID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[messageID]
}

// promptFrame is a prompt command as the control plane writes it.
func promptFrame(messageID string, gen int, receiptRequested bool) string {
	frame := fmt.Sprintf(`{"type":"prompt","messageId":%q,"sessionId":%q,"gen":%d,"text":"do it","model":null,"effort":null,"scmName":"n","scmEmail":"n@example.com"`,
		messageID, testSessionID, gen)
	if receiptRequested {
		frame += `,"receiptRequested":true`
	}
	return frame + `}`
}

// scriptedControlPlane accepts one connection per step: it forwards every
// frame the bridge sends -- "ready" first -- to frames, and writes the
// step's commands once the ready has arrived.
func scriptedControlPlane(t *testing.T, frames chan<- []byte, steps ...[]string) *httptest.Server {
	t.Helper()
	var fnSteps []func(*websocket.Conn)
	for _, commands := range steps {
		fnSteps = append(fnSteps, func(conn *websocket.Conn) {
			ready, err := serverRead(conn, testWait)
			if err != nil {
				return
			}
			frames <- ready
			for _, cmd := range commands {
				if err := conn.Write(context.Background(), websocket.MessageText, []byte(cmd)); err != nil {
					return
				}
			}
			for {
				_, data, err := conn.Read(context.Background())
				if err != nil {
					return
				}
				frames <- data
			}
		})
	}
	server := httptest.NewServer(&stepServer{steps: fnSteps, fallback: absorbForever})
	t.Cleanup(server.Close)
	return server
}

// receiptFrame is a prompt_received event as the bridge sends it.
type receiptFrame struct {
	Type            string `json:"type"`
	MessageID       string `json:"messageId"`
	SessionID       string `json:"sessionId"`
	Gen             int    `json:"gen"`
	PromptMessageID string `json:"promptMessageId"`
	Duplicate       bool   `json:"duplicate"`
}

// nextReceipt returns the next prompt_received frame on frames, skipping
// every other kind, and fails the test when none arrives in time.
func nextReceipt(t *testing.T, frames <-chan []byte) receiptFrame {
	t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case data := <-frames:
			var got receiptFrame
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("malformed frame from the bridge: %v (%s)", err, data)
			}
			if got.Type != "prompt_received" {
				continue
			}
			// The generated decoder validates the deterministic key.
			var typed sandboxws.PromptReceived
			if err := json.Unmarshal(data, &typed); err != nil {
				t.Fatalf("prompt_received fails its contract: %v (%s)", err, data)
			}
			return got
		case <-deadline:
			t.Fatalf("no prompt_received within %s", testWait)
			return receiptFrame{}
		}
	}
}

// startBridge builds a Bridge on server for testGen, with prompt receipts
// enabled in stateDir unless stateDir is empty, and runs it until the test
// ends.
func startBridge(t *testing.T, server *httptest.Server, handler wsbridge.CommandHandler, stateDir string) *wsbridge.Bridge {
	t.Helper()
	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", handler,
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
	if stateDir != "" {
		if err := bridge.EnablePromptReceipts(stateDir); err != nil {
			t.Fatalf("EnablePromptReceipts(%s) error = %v, want nil", stateDir, err)
		}
	}
	stopBridge(t, bridge)
	return bridge
}

// stopBridge runs bridge in the background, and stops it and waits for it
// once the test ends -- or earlier, through the returned func.
func stopBridge(t *testing.T, bridge *wsbridge.Bridge) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := wait(); err != nil {
				t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// waitRuns waits until counter has run messageID want times.
func waitRuns(t *testing.T, counter *promptCounter, messageID string, want int) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for counter.count(messageID) < want {
		if time.Now().After(deadline) {
			t.Fatalf("prompt %q ran %d times, want %d", messageID, counter.count(messageID), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReady_StatesMaxFrameBytes_AdvertisesPromptReceiptOnlyWhenEnabled:
// every ready states the agent's read limit, platform.MaxPromptFrameBytes,
// as capabilities.maxFrameBytes, whether or not the journal is open
// (technical plan §3.3, §6.1); promptReceipt is present only when it is.
func TestReady_StatesMaxFrameBytes_AdvertisesPromptReceiptOnlyWhenEnabled(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{name: "journal open", enabled: true},
		{name: "journal never opened", enabled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			frames := make(chan []byte, 16)
			server := scriptedControlPlane(t, frames, nil)
			stateDir := ""
			if tc.enabled {
				stateDir = t.TempDir()
			}
			startBridge(t, server, noopHandler{}, stateDir)

			ready := waitChan(t, frames, testWait)
			var keys struct {
				Capabilities map[string]json.RawMessage `json:"capabilities"`
			}
			if err := json.Unmarshal(ready, &keys); err != nil {
				t.Fatalf("malformed ready: %v", err)
			}
			if got, want := string(keys.Capabilities["maxFrameBytes"]), strconv.Itoa(platform.MaxPromptFrameBytes); got != want {
				t.Fatalf("ready states maxFrameBytes %q, want %s (ready: %s)", got, want, ready)
			}
			if _, has := keys.Capabilities["promptReceipt"]; has != tc.enabled {
				t.Fatalf("ready carries promptReceipt = %v, want %v (ready: %s)", has, tc.enabled, ready)
			}
			var typed sandboxws.Ready
			if err := json.Unmarshal(ready, &typed); err != nil {
				t.Fatalf("ready fails its contract: %v", err)
			}
			if tc.enabled && (typed.Capabilities == nil || typed.Capabilities.PromptReceipt == nil || !*typed.Capabilities.PromptReceipt) {
				t.Fatalf("ready = %s, want promptReceipt true", ready)
			}
		})
	}
}

func TestPrompt_ReceiptRequested_SendsReceiptAndRunsOnce(t *testing.T) {
	t.Parallel()

	frames := make(chan []byte, 16)
	server := scriptedControlPlane(t, frames, []string{promptFrame("p1", testGen, true)})
	counter := newPromptCounter()
	startBridge(t, server, counter, t.TempDir())

	got := nextReceipt(t, frames)
	want := receiptFrame{Type: "prompt_received", MessageID: "prompt_received:p1", SessionID: testSessionID, Gen: testGen, PromptMessageID: "p1", Duplicate: false}
	if got != want {
		t.Fatalf("receipt = %+v, want %+v", got, want)
	}
	waitRuns(t, counter, "p1", 1)
}

func TestPrompt_Duplicate_ReceiptAgainNeverRunsTwice(t *testing.T) {
	t.Parallel()

	frames := make(chan []byte, 16)
	server := scriptedControlPlane(t, frames, []string{
		promptFrame("p1", testGen, true),
		promptFrame("p1", testGen, true),
		promptFrame("p2", testGen, true),
	})
	counter := newPromptCounter()
	startBridge(t, server, counter, t.TempDir())

	first, second, third := nextReceipt(t, frames), nextReceipt(t, frames), nextReceipt(t, frames)
	if first.PromptMessageID != "p1" || first.Duplicate {
		t.Fatalf("first receipt = %+v, want p1, not a duplicate", first)
	}
	if second.PromptMessageID != "p1" || !second.Duplicate || second.MessageID != "prompt_received:p1" {
		t.Fatalf("second receipt = %+v, want p1 again, a duplicate, under the same key", second)
	}
	if third.PromptMessageID != "p2" {
		t.Fatalf("third receipt = %+v, want p2's", third)
	}
	// p2's receipt is written before p2 runs, and after the duplicate has
	// been handled: the duplicate's run, had it happened, is counted by now.
	waitRuns(t, counter, "p2", 1)
	if got := counter.count("p1"); got != 1 {
		t.Fatalf("p1 ran %d times, want exactly 1", got)
	}
}

func TestPrompt_NoReceiptRequested_NoReceipt_Deduped(t *testing.T) {
	t.Parallel()

	frames := make(chan []byte, 16)
	server := scriptedControlPlane(t, frames, []string{
		promptFrame("p1", testGen, false),
		promptFrame("p1", testGen, false),
		promptFrame("sentinel", testGen, true),
	})
	counter := newPromptCounter()
	startBridge(t, server, counter, t.TempDir())

	// The sentinel's receipt is the first one: neither copy of p1 drew one.
	if got := nextReceipt(t, frames); got.PromptMessageID != "sentinel" {
		t.Fatalf("first receipt = %+v, want the sentinel's (a prompt that asked for none gets none)", got)
	}
	waitRuns(t, counter, "sentinel", 1)
	if got := counter.count("p1"); got != 1 {
		t.Fatalf("p1 ran %d times, want exactly 1 (deduped though it asked for no receipt)", got)
	}
}

func TestPrompt_StaleGen_NoReceiptNoRun(t *testing.T) {
	t.Parallel()

	frames := make(chan []byte, 16)
	server := scriptedControlPlane(t, frames, []string{
		promptFrame("stale", testGen+1, true),
		promptFrame("sentinel", testGen, true),
	})
	counter := newPromptCounter()
	startBridge(t, server, counter, t.TempDir())

	if got := nextReceipt(t, frames); got.PromptMessageID != "sentinel" {
		t.Fatalf("first receipt = %+v, want the sentinel's (a stale gen's prompt is never receipted)", got)
	}
	waitRuns(t, counter, "sentinel", 1)
	if got := counter.count("stale"); got != 0 {
		t.Fatalf("the stale gen's prompt ran %d times, want 0", got)
	}
}

func TestPrompt_DedupSurvivesBridgeRestartSameGen(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	counter := newPromptCounter()

	frames1 := make(chan []byte, 16)
	server1 := scriptedControlPlane(t, frames1, []string{promptFrame("p1", testGen, true)})
	bridge1 := wsbridge.New(testSessionConfig(server1.URL), "sbx-1", "test-agent-version", "test-image-digest", counter,
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
	if err := bridge1.EnablePromptReceipts(stateDir); err != nil {
		t.Fatalf("EnablePromptReceipts: %v", err)
	}
	stop1 := stopBridge(t, bridge1)
	if got := nextReceipt(t, frames1); got.Duplicate {
		t.Fatalf("first process's receipt = %+v, want not a duplicate", got)
	}
	waitRuns(t, counter, "p1", 1)
	stop1()

	// The agent process restarts within the gen: a new Bridge, the same
	// state directory, the same prompt re-sent after its reconnect.
	frames2 := make(chan []byte, 16)
	server2 := scriptedControlPlane(t, frames2, []string{
		promptFrame("p1", testGen, true),
		promptFrame("sentinel", testGen, true),
	})
	startBridge(t, server2, counter, stateDir)
	if got := nextReceipt(t, frames2); got.PromptMessageID != "p1" || !got.Duplicate {
		t.Fatalf("restarted process's receipt = %+v, want p1, a duplicate", got)
	}
	nextReceipt(t, frames2)
	waitRuns(t, counter, "sentinel", 1)
	if got := counter.count("p1"); got != 1 {
		t.Fatalf("p1 ran %d times across the restart, want exactly 1", got)
	}
}

func TestPrompt_JournalOfOtherSessionOrGenIgnored(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	others := []string{
		wsbridge.PromptJournalFileNameForTest(testSessionID, testGen-1),
		wsbridge.PromptJournalFileNameForTest("another-session", testGen),
	}
	for _, name := range others {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte("\"p1\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	frames := make(chan []byte, 16)
	server := scriptedControlPlane(t, frames, []string{promptFrame("p1", testGen, true)})
	counter := newPromptCounter()
	startBridge(t, server, counter, stateDir)

	if got := nextReceipt(t, frames); got.Duplicate {
		t.Fatalf("receipt = %+v, want not a duplicate: another session's or gen's journal says nothing of this gen", got)
	}
	waitRuns(t, counter, "p1", 1)
	for _, name := range others {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !os.IsNotExist(err) {
			t.Errorf("another gen's journal %s still present (stat error %v), want it removed at open", name, err)
		}
	}
}

// TestPromptJournal_TornLastLineIsNotRecorded: a journal whose last append
// a crash cut short reads that line as not recorded, and the next append
// starts a line of its own, so it is read back after a later restart.
func TestPromptJournal_TornLastLineIsNotRecorded(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	journal := filepath.Join(stateDir, wsbridge.PromptJournalFileNameForTest(testSessionID, testGen))
	if err := os.WriteFile(journal, []byte("\"p1\"\n\"p2"), 0o600); err != nil {
		t.Fatal(err)
	}
	counter := newPromptCounter()

	frames1 := make(chan []byte, 16)
	server1 := scriptedControlPlane(t, frames1, []string{
		promptFrame("p1", testGen, true),
		promptFrame("p2", testGen, true),
	})
	bridge1 := wsbridge.New(testSessionConfig(server1.URL), "sbx-1", "test-agent-version", "test-image-digest", counter,
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
	if err := bridge1.EnablePromptReceipts(stateDir); err != nil {
		t.Fatalf("EnablePromptReceipts: %v", err)
	}
	stop1 := stopBridge(t, bridge1)
	if got := nextReceipt(t, frames1); got.PromptMessageID != "p1" || !got.Duplicate {
		t.Fatalf("p1's receipt = %+v, want a duplicate: its line is complete", got)
	}
	if got := nextReceipt(t, frames1); got.PromptMessageID != "p2" || got.Duplicate {
		t.Fatalf("p2's receipt = %+v, want not a duplicate: its line was cut short", got)
	}
	waitRuns(t, counter, "p2", 1)
	stop1()

	frames2 := make(chan []byte, 16)
	server2 := scriptedControlPlane(t, frames2, []string{promptFrame("p2", testGen, true)})
	startBridge(t, server2, counter, stateDir)
	if got := nextReceipt(t, frames2); !got.Duplicate {
		t.Fatalf("p2's receipt after a restart = %+v, want a duplicate: its append was a line of its own", got)
	}
	if got, want := counter.count("p1")+counter.count("p2"), 1; got != want {
		t.Fatalf("p1 and p2 ran %d times in all, want %d", got, want)
	}
}

// TestPrompt_JournalAppendFails_RequestedPromptNeitherRunNorReceipted: a
// prompt that asked for a receipt and cannot be journaled is neither run
// nor receipted (fail closed), and the agent says so and stops promising:
// a non-fatal error event names the prompt, the connection is ended, and
// the next connection's ready advertises no capability. A prompt that
// asked for none still runs, once.
func TestPrompt_JournalAppendFails_RequestedPromptNeitherRunNorReceipted(t *testing.T) {
	t.Parallel()

	frames1 := make(chan []byte, 16)
	frames2 := make(chan []byte, 16)
	closed1 := make(chan struct{})
	conn1 := func(conn *websocket.Conn) {
		defer close(closed1)
		ready, err := serverRead(conn, testWait)
		if err != nil {
			return
		}
		frames1 <- ready
		if err := conn.Write(context.Background(), websocket.MessageText, []byte(promptFrame("asked", testGen, true))); err != nil {
			return
		}
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return // the agent ended the connection
			}
			frames1 <- data
		}
	}
	conn2 := func(conn *websocket.Conn) {
		ready, err := serverRead(conn, testWait)
		if err != nil {
			return
		}
		frames2 <- ready
		for _, cmd := range []string{promptFrame("unasked", testGen, false), promptFrame("unasked", testGen, false), promptFrame("last", testGen, false)} {
			if err := conn.Write(context.Background(), websocket.MessageText, []byte(cmd)); err != nil {
				return
			}
		}
		absorbForever(conn)
	}
	server := httptest.NewServer(&stepServer{steps: []func(*websocket.Conn){conn1, conn2}, fallback: absorbForever})
	t.Cleanup(server.Close)

	counter := newPromptCounter()
	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", counter,
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
	if err := bridge.EnablePromptReceipts(t.TempDir()); err != nil {
		t.Fatalf("EnablePromptReceipts: %v", err)
	}
	wsbridge.BreakPromptJournalForTest(bridge)
	stopBridge(t, bridge)

	if ready := waitChan(t, frames1, testWait); !strings.Contains(string(ready), `"promptReceipt":true`) {
		t.Fatalf("first ready = %s, want the capability: no append has failed yet", ready)
	}
	// The agent ends the connection the prompt arrived on.
	select {
	case <-closed1:
	case <-time.After(testWait):
		t.Fatal("the agent kept the connection after failing to journal a prompt that asked for a receipt")
	}
	var signal *sandboxws.SandboxErrorEvent
	for done := false; !done; {
		select {
		case data := <-frames1:
			var head receiptFrame
			_ = json.Unmarshal(data, &head)
			switch head.Type {
			case "prompt_received":
				t.Fatalf("a receipt was sent for a prompt that was not journaled: %s", data)
			case "error":
				var e sandboxws.SandboxErrorEvent
				if err := json.Unmarshal(data, &e); err != nil {
					t.Fatalf("error event fails its contract: %v (%s)", err, data)
				}
				signal = &e
			}
		default:
			done = true
		}
	}
	if signal == nil || signal.Fatal || !strings.Contains(signal.Message, "asked") || signal.AckId != "error:prompt-not-journaled:asked" {
		t.Fatalf("signal = %+v, want a non-fatal, critical error event naming the prompt", signal)
	}

	// The next connection: no promptReceipt -- the ready still states the
	// agent's read limit -- and prompts that ask for none run.
	if ready := waitChan(t, frames2, testWait); strings.Contains(string(ready), "promptReceipt") {
		t.Fatalf("the ready after a failed append advertises promptReceipt: %s", ready)
	}
	waitRuns(t, counter, "last", 1)
	if got := counter.count("asked"); got != 0 {
		t.Fatalf("a prompt that asked for a receipt ran %d times with its append failed, want 0 (fail closed)", got)
	}
	if got := counter.count("unasked"); got != 1 {
		t.Fatalf("a prompt that asked for no receipt ran %d times, want exactly 1 (run, deduped in memory)", got)
	}
}

// TestPromptJournal_TornCompleteIdIsNotReadAsRecordedLater: a journal whose
// last line is a whole id without its newline -- an append cut exactly
// before it -- reads that id as not recorded, and so does every later
// process of the gen: a prompt no process ran is run when it comes, never
// answered as a duplicate.
func TestPromptJournal_TornCompleteIdIsNotReadAsRecordedLater(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	journal := filepath.Join(stateDir, wsbridge.PromptJournalFileNameForTest(testSessionID, testGen))
	if err := os.WriteFile(journal, []byte("\"p1\"\n\"p2\""), 0o600); err != nil {
		t.Fatal(err)
	}
	counter := newPromptCounter()

	// Process A opens the journal, runs only a sentinel, and stops.
	framesA := make(chan []byte, 16)
	serverA := scriptedControlPlane(t, framesA, []string{promptFrame("sentinel", testGen, true)})
	bridgeA := wsbridge.New(testSessionConfig(serverA.URL), "sbx-1", "test-agent-version", "test-image-digest", counter,
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
	if err := bridgeA.EnablePromptReceipts(stateDir); err != nil {
		t.Fatalf("EnablePromptReceipts: %v", err)
	}
	stopA := stopBridge(t, bridgeA)
	nextReceipt(t, framesA)
	waitRuns(t, counter, "sentinel", 1)
	stopA()

	// Process B is sent p2.
	framesB := make(chan []byte, 16)
	serverB := scriptedControlPlane(t, framesB, []string{promptFrame("p2", testGen, true)})
	startBridge(t, serverB, counter, stateDir)
	if got := nextReceipt(t, framesB); got.PromptMessageID != "p2" || got.Duplicate {
		t.Fatalf("p2's receipt = %+v, want not a duplicate: no process ran it", got)
	}
	waitRuns(t, counter, "p2", 1)
}

// TestRun_ReadsAPromptFrameOver32KiB: a prompt frame longer than the
// WebSocket library's default read limit (32 KiB) -- a review's, with its
// diff inlined -- is read whole and run once, on the connection it came
// on: the agent reads up to platform.MaxPromptFrameBytes.
func TestRun_ReadsAPromptFrameOver32KiB(t *testing.T) {
	t.Parallel()

	big := fmt.Sprintf(`{"type":"prompt","messageId":"big","sessionId":%q,"gen":%d,"text":%q,"model":null,"effort":null,"scmName":"n","scmEmail":"n@example.com","receiptRequested":true}`,
		testSessionID, testGen, strings.Repeat("<diff>", 40*1024/6+1))
	if len(big) <= 40*1024 {
		t.Fatalf("frame is %d bytes, want over 40 KiB", len(big))
	}
	frames := make(chan []byte, 16)
	server := scriptedControlPlane(t, frames, []string{big})
	counter := newPromptCounter()
	startBridge(t, server, counter, t.TempDir())

	if got := nextReceipt(t, frames); got.PromptMessageID != "big" || got.Duplicate {
		t.Fatalf("receipt = %+v, want big's, not a duplicate", got)
	}
	waitRuns(t, counter, "big", 1)
}

func TestEnablePromptReceipts_RejectsDirNotOursOrWritableByOthers(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	file := filepath.Join(base, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	groupWritable := filepath.Join(base, "group-writable")
	if err := os.Mkdir(groupWritable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(groupWritable, 0o770); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, dir string
	}{
		{name: "a file, not a directory", dir: file},
		{name: "a symlink to a directory", dir: link},
		{name: "writable by others", dir: open},
		{name: "writable by its group", dir: groupWritable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			frames := make(chan []byte, 16)
			server := scriptedControlPlane(t, frames, nil)
			bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
				testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
			if err := bridge.EnablePromptReceipts(tc.dir); err == nil {
				t.Fatalf("EnablePromptReceipts(%s) error = nil, want a refusal", tc.dir)
			}
			// The Bridge stays exactly as it was: no promptReceipt advertised
			// (its ready still states its read limit).
			stopBridge(t, bridge)
			ready := waitChan(t, frames, testWait)
			if strings.Contains(string(ready), "promptReceipt") {
				t.Fatalf("ready advertises promptReceipt after a refused journal: %s", ready)
			}
		})
	}

	t.Run("owned by another uid", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := wsbridge.AssertStateDirIsOursForTest(dir, os.Getuid()); err != nil {
			t.Fatalf("own 0700 directory refused: %v", err)
		}
		if err := wsbridge.AssertStateDirIsOursForTest(dir, os.Getuid()+1); err == nil {
			t.Fatal("a directory owned by another uid was accepted, want a refusal")
		}
	})
}
