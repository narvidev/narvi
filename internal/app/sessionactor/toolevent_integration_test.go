//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves toolevent.go against a real Postgres instance, every
// event driven through Actor.Send as wshub's read loop delivers it: each
// event of one assistant message carries that message's id, as the runtime
// adapter gives it (translate.go), with its `step_start` first -- the
// production shape, never a fresh id per event.

// messageEventForTest builds one event of assistant message messageID as
// wshub delivers it: the wire payload the runtime adapter emits, and the
// correlator its envelope peeks -- a callId for a tool_call or
// tool_result, a stepId for a step_start or step_finish.
func messageEventForTest(t *testing.T, sessionID, eventType, messageID, correlator string) SandboxEvent {
	t.Helper()
	var payload any
	cmd := SandboxEvent{Type: eventType, Gen: 1, MessageID: messageID}
	switch eventType {
	case "step_start":
		payload = sandboxws.StepStart{Type: eventType, MessageId: messageID, SessionId: sessionID, Gen: 1, StepId: correlator}
		cmd.StepID = correlator
	case "step_finish":
		usd := 0.25
		payload = sandboxws.StepFinish{Type: eventType, MessageId: messageID, SessionId: sessionID, Gen: 1, StepId: correlator,
			Cost: sandboxws.StepFinishCost{Tokens: sandboxws.StepFinishCostTokens{Input: 100, Output: 20}, Usd: &usd}}
		cmd.StepID = correlator
	case "tool_call":
		payload = sandboxws.ToolCall{Type: eventType, MessageId: messageID, SessionId: sessionID, Gen: 1, CallId: correlator,
			ToolName: "read", Input: sandboxws.ToolCallInput{"filePath": "main.go"}}
		cmd.CallID = correlator
	case "tool_result":
		payload = sandboxws.ToolResult{Type: eventType, MessageId: messageID, SessionId: sessionID, Gen: 1, CallId: correlator,
			Output: sandboxws.ToolResultOutput{"output": "package main"}}
		cmd.CallID = correlator
	default:
		t.Fatalf("messageEventForTest: no payload for %q", eventType)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", eventType, err)
	}
	cmd.Raw = raw
	return cmd
}

// oneMessageForTest is the events of one assistant message: its
// step_start, a tool call and its result, and its step_finish, in the order
// the runtime adapter emits them.
func oneMessageForTest(t *testing.T, sessionID, messageID string) []SandboxEvent {
	t.Helper()
	return []SandboxEvent{
		messageEventForTest(t, sessionID, "step_start", messageID, "prt_start_"+messageID),
		messageEventForTest(t, sessionID, "tool_call", messageID, "call_"+messageID),
		messageEventForTest(t, sessionID, "tool_result", messageID, "call_"+messageID),
		messageEventForTest(t, sessionID, "step_finish", messageID, "prt_finish_"+messageID),
	}
}

// storedKeys returns sessionID's rows of eventTypes, oldest first, each as
// "type key".
func storedKeys(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, eventTypes ...string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT type, message_id FROM events WHERE session_id = $1 AND type = ANY($2) ORDER BY id`, sessionID, eventTypes)
	if err != nil {
		t.Fatalf("query event rows: %v", err)
	}
	var out []string
	for rows.Next() {
		var eventType, key string
		if err := rows.Scan(&eventType, &key); err != nil {
			rows.Close()
			t.Fatalf("scan event row: %v", err)
		}
		out = append(out, eventType+" "+key)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate event rows: %v", err)
	}
	return out
}

var toolEventTypes = []string{"step_start", "tool_call", "tool_result", "step_finish"}

// TestToolEvent_ProductionShape_EachStoredOnceUnderItsKey: a message's
// step_start, tool_call, tool_result and step_finish, all under the
// message's id, are each stored and broadcast once, the step_start under
// the bare id and the other three under the key their correlator derives;
// a resend of each adds no row and broadcasts nothing. Under the wire
// messageId alone, the three after the step_start added no row at all.
func TestToolEvent_ProductionShape_EachStoredOnceUnderItsKey(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	createDispatchedProcessingTurn(ctx, t, pool, sessionID)
	fb := &fakeBroadcaster{}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), fb, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	message := oneMessageForTest(t, sessionID.String(), "msg_1")
	for _, cmd := range message {
		if outcome := sendSandboxEventForTest(ctx, t, a, cmd); !outcome.Persisted {
			t.Fatalf("%s: outcome %+v, want Persisted", cmd.Type, outcome)
		}
	}
	want := []string{
		"step_start msg_1",
		"tool_call msg_1#tool_call:call_msg_1",
		"tool_result msg_1#tool_result:call_msg_1",
		"step_finish msg_1#step_finish:prt_finish_msg_1",
	}
	if got := storedKeys(ctx, t, pool, sessionID, toolEventTypes...); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("stored %q, want %q", got, want)
	}
	var broadcast []string
	for _, call := range fb.calls {
		var p struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(call.payload, &p); err != nil {
			t.Fatalf("decode broadcast: %v", err)
		}
		broadcast = append(broadcast, p.Type)
	}
	if fmt.Sprint(broadcast) != fmt.Sprint(toolEventTypes) {
		t.Fatalf("broadcast %v, want each event once: %v", broadcast, toolEventTypes)
	}

	// A resend of each -- the sandbox replays its buffer on a reconnect --
	// adds no row and broadcasts nothing.
	for _, cmd := range message {
		sendSandboxEventForTest(ctx, t, a, cmd)
	}
	if got := storedKeys(ctx, t, pool, sessionID, toolEventTypes...); len(got) != len(want) {
		t.Fatalf("after the resend, stored %q, want %q", got, want)
	}
	if len(fb.calls) != len(toolEventTypes) {
		t.Fatalf("the resend broadcast %d more events, want none", len(fb.calls)-len(toolEventTypes))
	}
}

// TestToolEvent_ReplayedAfterItsTurn_AddsNoRow: the sandbox-agent replays
// its whole buffer on every reconnect, best-effort events included, long
// after their turn's execution_complete -- and on its first reconnect to
// this binary, tool events the first-wins rule swallowed. Stored there, a
// row would land at the tail of the log, where the web timeline opens a
// new, running turn. A tool_call, tool_result or step_finish of a message
// from an ended turn adds no row and broadcasts nothing, whether no turn
// is Processing or a later one is; the later turn's own message is stored;
// and a tool_call of a message whose step_start was never stored adds no
// row either: nothing places it in the Processing turn.
func TestToolEvent_ReplayedAfterItsTurn_AddsNoRow(t *testing.T) {
	for _, nextTurnProcessing := range []bool{false, true} {
		name := "no turn Processing during the replay"
		if nextTurnProcessing {
			name = "the next turn Processing during the replay"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			sessionID := createTestSession(ctx, t, pool)
			sid := sessionID.String()
			if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			fb := &fakeBroadcaster{}
			r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), fb, nil, nil, "", nil, nil, "", nil, false)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}

			// Turn one: its message stored while it ran. A second message's
			// step_start is stored as history before this file left it --
			// its tool call swallowed then, and replayed below.
			createDispatchedProcessingTurn(ctx, t, pool, sessionID)
			for _, cmd := range oneMessageForTest(t, sid, "msg_1") {
				sendSandboxEventForTest(ctx, t, a, cmd)
			}
			sendSandboxEventForTest(ctx, t, a, messageEventForTest(t, sid, "step_start", "msg_2", "prt_start_msg_2"))
			sendExecutionComplete(ctx, t, a, sessionID)
			if _, err := narvipg.NewTurnStore(pool).GetProcessingTurnForSession(ctx, sessionID); err == nil {
				t.Fatal("turn one still Processing after its execution_complete")
			}

			var nextTurn sqlcgen.Turn
			if nextTurnProcessing {
				nextTurn = createDispatchedProcessingTurn(ctx, t, pool, sessionID)
			}
			before := listEventRows(ctx, t, pool, sessionID)
			broadcastsBefore := len(fb.calls)

			// The replay: every event of turn one's messages, msg_2's
			// swallowed tool call and step end among them.
			replay := oneMessageForTest(t, sid, "msg_1")
			replay = append(replay, oneMessageForTest(t, sid, "msg_2")...)
			for _, cmd := range replay {
				if outcome := sendSandboxEventForTest(ctx, t, a, cmd); !outcome.Persisted {
					t.Fatalf("replayed %s: outcome %+v, want Persisted (handled, gen current)", cmd.Type, outcome)
				}
			}
			after := listEventRows(ctx, t, pool, sessionID)
			if len(after) != len(before) {
				t.Fatalf("the replay added %d rows %+v, want none", len(after)-len(before), after[len(before):])
			}
			if got := len(fb.calls) - broadcastsBefore; got != 0 {
				t.Fatalf("the replay broadcast %d events, want none", got)
			}

			if !nextTurnProcessing {
				return
			}
			// The next turn's own message is stored and broadcast, in its
			// window, its step_start above its watermark; a tool call of a
			// message none of whose events was stored before it is not.
			for _, cmd := range oneMessageForTest(t, sid, "msg_3") {
				sendSandboxEventForTest(ctx, t, a, cmd)
			}
			sendSandboxEventForTest(ctx, t, a, messageEventForTest(t, sid, "tool_call", "msg_4", "call_msg_4"))
			var inWindow []string
			for _, row := range listEventRows(ctx, t, pool, sessionID) {
				if row.id > *nextTurn.DispatchedEventID {
					inWindow = append(inWindow, row.eventType+" "+row.storageKey)
				}
			}
			want := []string{
				"step_start msg_3",
				"tool_call msg_3#tool_call:call_msg_3",
				"tool_result msg_3#tool_result:call_msg_3",
				"step_finish msg_3#step_finish:prt_finish_msg_3",
			}
			if strings.Join(inWindow, "|") != strings.Join(want, "|") {
				t.Errorf("rows in the next turn's window = %q, want %q", inWindow, want)
			}
			if got := len(fb.calls) - broadcastsBefore; got != len(want) {
				t.Errorf("broadcasts after the replay = %d, want %d (the next turn's own events)", got, len(want))
			}
		})
	}
}

// TestToolEvent_MessageWithNoStoredStepStart_AddsNoRow: a tool_call,
// tool_result or step_finish whose message has no stored step_start adds
// no row and broadcasts nothing while a turn is Processing, whether that
// turn places a window or not (a NULL dispatched_event_id); a message whose
// step_start is stored has its tool call stored.
func TestToolEvent_MessageWithNoStoredStepStart_AddsNoRow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stamped bool
	}{
		{name: "the turn places a window", stamped: true},
		{name: "the turn places no window", stamped: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			sessionID := createTestSession(ctx, t, pool)
			sid := sessionID.String()
			if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			if tc.stamped {
				createDispatchedProcessingTurn(ctx, t, pool, sessionID)
			} else {
				createProcessingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID)
			}
			fb := &fakeBroadcaster{}
			r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), fb, nil, nil, "", nil, nil, "", nil, false)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}

			for _, cmd := range oneMessageForTest(t, sid, "msg_x")[1:] {
				if outcome := sendSandboxEventForTest(ctx, t, a, cmd); !outcome.Persisted {
					t.Fatalf("%s: outcome %+v, want Persisted (handled, gen current)", cmd.Type, outcome)
				}
			}
			if got := storedKeys(ctx, t, pool, sessionID, toolEventTypes...); len(got) != 0 {
				t.Fatalf("with no step_start of its message stored, stored %q, want nothing", got)
			}
			if len(fb.calls) != 0 {
				t.Fatalf("broadcast %d events, want none", len(fb.calls))
			}

			message := oneMessageForTest(t, sid, "msg_y")
			sendSandboxEventForTest(ctx, t, a, message[0])
			sendSandboxEventForTest(ctx, t, a, message[1])
			want := []string{"step_start msg_y", "tool_call msg_y#tool_call:call_msg_y"}
			if got := storedKeys(ctx, t, pool, sessionID, toolEventTypes...); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("stored %q, want %q", got, want)
			}
		})
	}
}

// TestLateStepStart_NoTurnProcessing_AddsNoRow: turn_deadline ends a turn
// without stopping its agent, which can go on sending a step_start or a
// sub_task_start. A step_start stored once no turn is Processing opened a
// turn on the page that nothing ended: it adds no row then, and is stored
// while a turn is Processing. A sub_task_start is stored either way, as a
// late sub_task_finish, critical, is: §26.4's corroboration of a timed-out
// turn's late verdict reads both.
func TestLateStepStart_NoTurnProcessing_AddsNoRow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	sid := sessionID.String()
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	fb := &fakeBroadcaster{}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), fb, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	raw := func(v any) json.RawMessage {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	subStart := func(id string) SandboxEvent {
		return SandboxEvent{Type: "sub_task_start", Gen: 1, MessageID: id,
			Raw: raw(sandboxws.SubTaskStart{Type: "sub_task_start", MessageId: id, SessionId: sid, Gen: 1, SubTaskId: "ses_" + id, Label: "lane", ParentMessageId: "msg_1"})}
	}
	subFinish := func(id string) SandboxEvent {
		return SandboxEvent{Type: "sub_task_finish", Gen: 1, MessageID: id,
			Raw: raw(sandboxws.SubTaskFinish{Type: "sub_task_finish", MessageId: id, SessionId: sid, Gen: 1, AckId: "sub_task_finish:" + id, SubTaskId: "ses_x", Outcome: sandboxws.SubTaskFinishOutcomeCancelled})}
	}
	types := []string{"step_start", "sub_task_start", "sub_task_finish"}

	// No turn Processing: a late step_start adds no row; a late
	// sub_task_start and sub_task_finish are stored, the finish acked.
	sendSandboxEventForTest(ctx, t, a, messageEventForTest(t, sid, "step_start", "msg_late", "prt_late"))
	if outcome := sendSandboxEventForTest(ctx, t, a, subStart("sst_late")); !outcome.Persisted {
		t.Fatalf("late sub_task_start outcome %+v, want Persisted", outcome)
	}
	if outcome := sendSandboxEventForTest(ctx, t, a, subFinish("ssf_late")); outcome.AckID != "sub_task_finish:ssf_late" {
		t.Fatalf("late sub_task_finish outcome %+v, want it acked", outcome)
	}
	late := []string{"sub_task_start sst_late", "sub_task_finish ssf_late"}
	if got := storedKeys(ctx, t, pool, sessionID, types...); strings.Join(got, "|") != strings.Join(late, "|") {
		t.Fatalf("with no turn processing, stored %q, want %q", got, late)
	}

	// A turn Processing: both starts are stored.
	createDispatchedProcessingTurn(ctx, t, pool, sessionID)
	sendSandboxEventForTest(ctx, t, a, messageEventForTest(t, sid, "step_start", "msg_1", "prt_1"))
	sendSandboxEventForTest(ctx, t, a, subStart("sst_1"))
	want := []string{"sub_task_start sst_late", "sub_task_finish ssf_late", "step_start msg_1", "sub_task_start sst_1"}
	if got := storedKeys(ctx, t, pool, sessionID, types...); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("with a turn processing, stored %q, want %q", got, want)
	}
}

// TestToolEvent_MessageBegunWhileNoTurnProcessing_AddsNoRowInTheNextTurn:
// turn_deadline fails a turn without stopping its agent, which can begin a
// message before the next turn is dispatched. Its step_start adds no row,
// no turn being Processing, and its tool call, result and step end, sent
// once the next turn is Processing, add none either: no stored step_start
// places the message in that turn's window. Stored there, they were drawn
// in the next turn and counted in its cost. The next turn's own message is
// stored.
func TestToolEvent_MessageBegunWhileNoTurnProcessing_AddsNoRowInTheNextTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	sid := sessionID.String()
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	timeouts := platform.DefaultTimeouts()
	timeouts.TurnDeadline = 50 * time.Millisecond // tiny, injected -- not the real 60m default
	fb := &fakeBroadcaster{}
	r, err := NewRegistry(ctx, pool, timeouts, fb, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	turns := narvipg.NewTurnStore(pool)

	// Turn A runs a message, then its deadline passes.
	turnA := createDispatchedProcessingTurn(ctx, t, pool, sessionID)
	for _, cmd := range oneMessageForTest(t, sid, "msg_a") {
		sendSandboxEventForTest(ctx, t, a, cmd)
	}
	if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:           turnA.ID,
		Status:       sqlcgen.TurnStatusProcessing,
		DispatchedAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("age turn A's dispatched_at: %v", err)
	}
	if err := a.Send(ctx, TimerFired{Name: TimerTurnDeadline}); err != nil {
		t.Fatalf("Send TimerFired turn_deadline: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		got, err := turns.Get(ctx, turnA.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})

	// A's agent begins a message in the gap; turn B is dispatched; the
	// message goes on, and B's own message follows.
	gap := oneMessageForTest(t, sid, "msg_m")
	sendSandboxEventForTest(ctx, t, a, gap[0])
	turnB := createDispatchedProcessingTurn(ctx, t, pool, sessionID)
	broadcastsBefore := len(fb.calls)
	for _, cmd := range gap[1:] {
		if outcome := sendSandboxEventForTest(ctx, t, a, cmd); !outcome.Persisted {
			t.Fatalf("%s of the gap message: outcome %+v, want Persisted (handled, gen current)", cmd.Type, outcome)
		}
	}
	for _, cmd := range oneMessageForTest(t, sid, "msg_b") {
		sendSandboxEventForTest(ctx, t, a, cmd)
	}

	var inWindow []string
	for _, row := range listEventRows(ctx, t, pool, sessionID) {
		if row.id > *turnB.DispatchedEventID {
			inWindow = append(inWindow, row.eventType+" "+row.storageKey)
		}
	}
	want := []string{
		"step_start msg_b",
		"tool_call msg_b#tool_call:call_msg_b",
		"tool_result msg_b#tool_result:call_msg_b",
		"step_finish msg_b#step_finish:prt_finish_msg_b",
	}
	if strings.Join(inWindow, "|") != strings.Join(want, "|") {
		t.Errorf("rows in turn B's window = %q, want B's own message alone %q", inWindow, want)
	}
	if got := len(fb.calls) - broadcastsBefore; got != len(want) {
		t.Errorf("broadcasts once B was Processing = %d, want %d (B's own events)", got, len(want))
	}
}
