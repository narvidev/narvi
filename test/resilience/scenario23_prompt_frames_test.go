//go:build integration

// Resilience scenario #23, the prompt direction (technical plan §3.3,
// §6.1; docs/IMPLEMENTATION_PLAN.md row 228): "a prompt over the bound its
// gen's agent states -> refused at dispatch with both sizes named".
//
// A prompt is one WebSocket message, and an agent closes its connection on
// a message longer than its read limit, losing the prompt. An agent built
// before the read limit was raised to platform.MaxPromptFrameBytes -- in an
// older snapshot or repo image -- reads at the library's default, 32 KiB,
// and advertises nothing in its ready; the control plane holds every
// prompt to its gen to that default and refuses a longer one at dispatch,
// with both sizes named, where it used to write it to be lost silently
// and leave the turn to turn_deadline. An agent built with the larger read
// limit and prompt receipts, before it stated its limit, advertises
// promptReceipt: its gen is held to MaxPromptFrameBytes, and a prompt over
// 32 KiB reaches it whole. Both agents are written by hand here, as they
// write and read the wire, on scenario #22's rig: the real wshub sandbox
// handler and session actor on Postgres, behind scenario #7's relay.
package resilience_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// promptFrameAgent is one connection of a hand-written agent: a ready
// carrying capabilities (raw members, "" for none), a read limit (0 for
// the library's default), and, for each prompt it reads, a receipt when
// the prompt asks for one and then an execution_complete.
type promptFrameAgent struct {
	conn      *websocket.Conn
	sessionID string
	runs      bool
	group     errgroup.Group

	mu      sync.Mutex
	prompts map[string]int // messageId -> frame bytes read
	readErr error
	closing bool
}

func dialPromptFrameAgent(ctx context.Context, t *testing.T, url, sessionID, capabilities string, readLimit int64, runs bool) *promptFrameAgent {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer scenario23-test-token")
	header.Set("X-Sandbox-ID", "sbx-scenario23")
	header.Set("X-Sandbox-Gen", "1")
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if readLimit > 0 {
		conn.SetReadLimit(readLimit)
	}
	a := &promptFrameAgent{conn: conn, sessionID: sessionID, runs: runs, prompts: map[string]int{}}
	a.group.Go(a.readLoop)
	caps := ""
	if capabilities != "" {
		caps = `,"capabilities":{` + capabilities + `}`
	}
	a.write(ctx, t, fmt.Sprintf(`{"type":"ready","messageId":%q,"sessionId":%q,"gen":1,"timestamp":%q,"agentVersion":"old","imageDigest":"old"%s}`,
		"r-"+time.Now().Format(time.RFC3339Nano), sessionID, time.Now().UTC().Format(time.RFC3339Nano), caps))
	t.Cleanup(a.close)
	return a
}

func (a *promptFrameAgent) readLoop() error {
	ctx := context.Background()
	for {
		_, data, err := a.conn.Read(ctx)
		if err != nil {
			a.mu.Lock()
			if !a.closing {
				a.readErr = err
			}
			a.mu.Unlock()
			return nil
		}
		if relayType(data) != "prompt" {
			continue
		}
		var p struct {
			MessageID        string `json:"messageId"`
			ReceiptRequested bool   `json:"receiptRequested"`
		}
		_ = json.Unmarshal(data, &p)
		a.mu.Lock()
		a.prompts[p.MessageID] = len(data)
		a.mu.Unlock()
		if !a.runs {
			continue
		}
		if p.ReceiptRequested {
			if err := a.conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(
				`{"type":"prompt_received","messageId":"prompt_received:%s","sessionId":%q,"gen":1,"promptMessageId":%q,"duplicate":false}`,
				p.MessageID, a.sessionID, p.MessageID))); err != nil {
				return nil
			}
		}
		id := "ec-" + p.MessageID
		if err := a.conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(
			`{"type":"execution_complete","messageId":%q,"sessionId":%q,"gen":1,"ackId":"execution_complete:%s","outcome":"completed","reason":null}`,
			id, a.sessionID, id))); err != nil {
			return nil
		}
	}
}

func (a *promptFrameAgent) write(ctx context.Context, t *testing.T, frame string) {
	t.Helper()
	if err := a.conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// read returns the prompts read so far (messageId -> frame bytes) and the
// error that ended the connection's reads, nil while it is open.
func (a *promptFrameAgent) read() (map[string]int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.prompts))
	for k, v := range a.prompts {
		out[k] = v
	}
	return out, a.readErr
}

func (a *promptFrameAgent) close() {
	a.mu.Lock()
	a.closing = true
	a.mu.Unlock()
	_ = a.conn.CloseNow()
	_ = a.group.Wait()
}

// promptOver32KiB is a turn's text whose prompt frame is over the library's
// default read limit: 40 KiB of plain bytes, which encode as themselves.
var promptOver32KiB = strings.Repeat("a", 40*1024)

// TestResilience_Scenario23_Step226Agent_PromptOver32KiB_Delivered: an agent
// built with the larger read limit and prompt receipts, before it stated
// its limit -- its ready advertises promptReceipt and no maxFrameBytes --
// is held to platform.MaxPromptFrameBytes: a 40 KiB prompt reaches it
// whole, on the connection it was sent on, is receipted, and its turn
// completes.
func TestResilience_Scenario23_Step226Agent_PromptOver32KiB_Delivered(t *testing.T) {
	ctx := context.Background()
	rig := newLostPromptRigWithPrompt(ctx, t, platform.DefaultTimeouts(), wsProxyStep{}, promptOver32KiB)
	agent := dialPromptFrameAgent(ctx, t, rig.proxyURL, rig.sessionID.String(), `"promptReceipt":true`, platform.MaxPromptFrameBytes, true)

	waitUntil(t, scenario22Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })

	messageID := *rig.turn(ctx, t).DispatchedMessageID
	prompts, readErr := agent.read()
	if len(prompts) != 1 || prompts[messageID] <= 40*1024 {
		t.Fatalf("prompts read = %v, want %q once, its frame over 40 KiB", prompts, messageID)
	}
	if readErr != nil {
		t.Fatalf("the agent's connection ended: %v; want it read on the connection it came on", readErr)
	}
	if n := countEventsByMessageID(ctx, t, rig.h, rig.sessionID, "prompt_received:"+messageID); n != 1 {
		t.Fatalf("%d stored receipts for %q, want 1", n, messageID)
	}
	if got := rig.readyCount(); got != 1 {
		t.Fatalf("%d readies relayed, want 1", got)
	}
	if all, synthetic := rig.executionCompleteRows(ctx, t); all != 1 || synthetic != 0 {
		t.Fatalf("execution_complete events = %d (%d synthetic), want the agent's one", all, synthetic)
	}
}

// TestResilience_Scenario23_PromptOver32KiB_PreReceiptAgent_RefusedAtDispatch:
// an agent built before the larger read limit -- its ready carries no
// capabilities, and it reads at the library's default -- is held to 32
// KiB: a 40 KiB prompt is never written to it, and its turn fails at
// dispatch, with both sizes named in the session's warning and its
// synthetic execution_complete marked "delivered": false. The agent's
// connection stays up: no frame over its limit reached it.
func TestResilience_Scenario23_PromptOver32KiB_PreReceiptAgent_RefusedAtDispatch(t *testing.T) {
	ctx := context.Background()
	rig := newLostPromptRigWithPrompt(ctx, t, platform.DefaultTimeouts(), wsProxyStep{}, promptOver32KiB)
	agent := dialPromptFrameAgent(ctx, t, rig.proxyURL, rig.sessionID.String(), "", 0, true)

	waitUntil(t, scenario22Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusFailed })

	prompts, readErr := agent.read()
	if len(prompts) != 0 {
		t.Fatalf("the agent read %d prompts, want 0: nothing over its limit is written", len(prompts))
	}
	if readErr != nil {
		t.Fatalf("the agent's connection ended: %v; want it up, no frame over its limit having reached it", readErr)
	}
	if got := rig.readyCount(); got != 1 {
		t.Fatalf("%d readies relayed, want 1", got)
	}

	var raw []byte
	var warning string
	if err := rig.h.Pool.QueryRow(ctx,
		`SELECT (SELECT payload FROM events WHERE session_id = $1 AND type = 'execution_complete'),
		        (SELECT payload->>'message' FROM events WHERE session_id = $1 AND type = 'warning')`,
		rig.sessionID).Scan(&raw, &warning); err != nil {
		t.Fatalf("read the turn's end: %v", err)
	}
	var complete map[string]any
	if err := json.Unmarshal(raw, &complete); err != nil {
		t.Fatalf("decode execution_complete: %v", err)
	}
	if _, synthetic := complete["synthetic"]; !synthetic {
		t.Fatalf("execution_complete = %s, want the control plane's synthetic one", raw)
	}
	if delivered, marked := complete["delivered"]; !marked || delivered != false {
		t.Fatalf("execution_complete = %s, want it marked \"delivered\": false", raw)
	}
	if !regexp.MustCompile(`\b40\.\d KiB\b`).MatchString(warning) || !strings.Contains(warning, "32.0 KiB") {
		t.Fatalf("session warning = %q, want the frame's size (40.x KiB) and the agent's limit (32.0 KiB) named", warning)
	}

	// The connection is still the one the ready came on.
	agent.write(ctx, t, fmt.Sprintf(`{"type":"heartbeat","messageId":"h-after","sessionId":%q,"gen":1,"conversationId":null,"lastBootPhase":null,"timestamp":%q}`,
		rig.sessionID.String(), time.Now().UTC().Format(time.RFC3339Nano)))
	waitUntil(t, scenario22Wait, func() bool {
		return countEventsByMessageID(ctx, t, rig.h, rig.sessionID, "h-after") == 1
	})
	if _, readErr := agent.read(); readErr != nil {
		t.Fatalf("the agent's connection ended: %v", readErr)
	}
}
