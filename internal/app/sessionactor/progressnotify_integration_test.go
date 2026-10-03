//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves audit finding M16's ("completeness", internal/adapters/
// outbound/linearapi/doc.go) own enqueue-side wiring: a Linear-origin
// session's turn processing its FIRST tool_call wire event enqueues
// exactly one 'linear_progress'-kind outbox row, a resent/duplicate
// underlying wire event never enqueues a second one, a second genuinely
// DISTINCT tool_call in the SAME turn never enqueues a second one either,
// and a Slack/GitHub-origin session's equivalent tool_call event enqueues
// nothing at all -- see progressnotify.go's own doc comment for the full
// design this exercises.

// toolCallRaw marshals a real, schema-valid sandboxws.ToolCall wire
// payload -- mirrors executionCompleteRaw's own shape exactly
// (pushpr_integration_test.go).
func toolCallRaw(t *testing.T, sessionID string, gen int, messageID, callID string) json.RawMessage {
	t.Helper()
	evt := sandboxws.ToolCall{
		Type:      "tool_call",
		MessageId: messageID,
		SessionId: sessionID,
		Gen:       gen,
		CallId:    callID,
		ToolName:  "bash",
		Input:     sandboxws.ToolCallInput{"command": "echo hi"},
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal tool_call: %v", err)
	}
	return raw
}

// claimLinearAgentSessionForTest claims and attaches agentSessionID/
// organizationID to sessionID -- mirrors
// TestCompleteProcessingTurn_LinearOrigin_EnqueuesExactlyOneLinearOutboxRow's
// own inline setup (outboxenqueue_integration_test.go), factored out since
// this file's own tests need it repeatedly.
func claimLinearAgentSessionForTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, agentSessionID, organizationID string) {
	t.Helper()
	agentSessions := narvipg.NewLinearAgentSessionStore(pool)
	if _, err := agentSessions.Claim(ctx, agentSessionID, organizationID); err != nil {
		t.Fatalf("claim linear agent session: %v", err)
	}
	if err := agentSessions.SetSessionID(ctx, agentSessionID, sessionID); err != nil {
		t.Fatalf("set linear agent session id: %v", err)
	}
}

// TestMaybeEnqueueLinearProgress_LinearOrigin_FirstToolCall_EnqueuesExactlyOneProgressOutboxRow
// proves the milestone's own happy path: a Linear-origin session's turn
// processing its FIRST tool_call wire event enqueues exactly one
// 'linear_progress'-kind outbox row, shaped as linearapi.ProgressPayload,
// with the session's own reverse-looked-up agent_session_id/
// organization_id.
func TestMaybeEnqueueLinearProgress_LinearOrigin_FirstToolCall_EnqueuesExactlyOneProgressOutboxRow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceLinear)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)
	claimLinearAgentSessionForTest(ctx, t, pool, sessionID, "agent-session-1", "org-1")

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type:      "tool_call",
		Gen:       1,
		MessageID: "toolcall-1",
		Raw:       toolCallRaw(t, sessionID.String(), 1, "toolcall-1", "call-1"),
	})

	row := getSoleOutboxRowForSession(ctx, t, pool, sessionID)
	if row.Kind != string(ports.NotificationKindLinearProgress) {
		t.Errorf("Kind = %q, want %q", row.Kind, ports.NotificationKindLinearProgress)
	}
	if row.Status != sqlcgen.OutboxStatusPending {
		t.Errorf("Status = %q, want %q", row.Status, sqlcgen.OutboxStatusPending)
	}

	var payload linearapi.ProgressPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload as linearapi.ProgressPayload: %v", err)
	}
	if payload.AgentSessionID != "agent-session-1" {
		t.Errorf("AgentSessionID = %q, want %q", payload.AgentSessionID, "agent-session-1")
	}
	if payload.OrganizationID != "org-1" {
		t.Errorf("OrganizationID = %q, want %q", payload.OrganizationID, "org-1")
	}
	if payload.Text == "" {
		t.Error("Text is empty, want a human-readable progress message")
	}
}

// TestMaybeEnqueueLinearProgress_ResentDuplicateToolCall_DoesNotEnqueueSecondRow
// proves guard (1) from progressnotify.go's own doc comment: a wire-level
// redelivery of the SAME already-processed tool_call (identical
// MessageID) is deduped by appendRawEvent's own upsert-on-messageId
// (Inserted false), so it must never enqueue a second progress
// notification.
func TestMaybeEnqueueLinearProgress_ResentDuplicateToolCall_DoesNotEnqueueSecondRow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceLinear)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)
	claimLinearAgentSessionForTest(ctx, t, pool, sessionID, "agent-session-1", "org-1")

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	raw := toolCallRaw(t, sessionID.String(), 1, "toolcall-1", "call-1")
	// First delivery: enqueues exactly one row.
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "tool_call", Gen: 1, MessageID: "toolcall-1", Raw: raw})
	// Second delivery: the EXACT same MessageID/Raw, simulating the
	// sandbox's own buffer/resend-on-reconnect protocol (§6.1) redelivering
	// an event this session actor already processed.
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "tool_call", Gen: 1, MessageID: "toolcall-1", Raw: raw})

	if n := countOutboxRowsForSession(ctx, t, pool, sessionID); n != 1 {
		t.Errorf("outbox row count after a resent duplicate tool_call = %d, want exactly 1", n)
	}
}

// TestMaybeEnqueueLinearProgress_SecondDistinctToolCallSameTurn_DoesNotEnqueueSecondRow
// proves guard (2): a SECOND, genuinely DIFFERENT tool_call event (its own
// distinct MessageID/CallId -- not a wire-level resend) later in the SAME
// turn must not enqueue a second progress notification either -- the
// common, expected case of an agent calling more than one tool per turn.
func TestMaybeEnqueueLinearProgress_SecondDistinctToolCallSameTurn_DoesNotEnqueueSecondRow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceLinear)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)
	claimLinearAgentSessionForTest(ctx, t, pool, sessionID, "agent-session-1", "org-1")

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "tool_call", Gen: 1, MessageID: "toolcall-1",
		Raw: toolCallRaw(t, sessionID.String(), 1, "toolcall-1", "call-1"),
	})
	// A second, distinct tool_call -- different MessageID/CallId entirely,
	// a genuinely new insert into the events table, NOT a resend.
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "tool_call", Gen: 1, MessageID: "toolcall-2",
		Raw: toolCallRaw(t, sessionID.String(), 1, "toolcall-2", "call-2"),
	})

	if n := countOutboxRowsForSession(ctx, t, pool, sessionID); n != 1 {
		t.Errorf("outbox row count after a second, distinct tool_call in the same turn = %d, want exactly 1", n)
	}
}

// TestMaybeEnqueueLinearProgress_SlackOrigin_ToolCall_EnqueuesNothing proves
// scope containment: this finding (M16) is explicitly Linear-scoped -- a
// Slack-origin session's own tool_call event must never enqueue a
// 'linear_progress' (or any other) outbox row.
func TestMaybeEnqueueLinearProgress_SlackOrigin_ToolCall_EnqueuesNothing(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceSlack)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)
	if _, ok, err := narvipg.NewSlackThreadSessionStore(pool).Claim(ctx, "C123", "1700000000.000100", sessionID); err != nil || !ok {
		t.Fatalf("claim slack thread session: ok=%v err=%v", ok, err)
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "tool_call", Gen: 1, MessageID: "toolcall-1",
		Raw: toolCallRaw(t, sessionID.String(), 1, "toolcall-1", "call-1"),
	})

	if n := countOutboxRowsForSession(ctx, t, pool, sessionID); n != 0 {
		t.Errorf("outbox row count for slack-origin session's tool_call = %d, want 0", n)
	}
}

// TestMaybeEnqueueLinearProgress_GitHubOrigin_ToolCall_EnqueuesNothing is
// TestMaybeEnqueueLinearProgress_SlackOrigin_ToolCall_EnqueuesNothing's
// mirror for a GitHub-origin session.
func TestMaybeEnqueueLinearProgress_GitHubOrigin_ToolCall_EnqueuesNothing(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceGithub)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)

	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, "acme/widgets", 42); err != nil {
		t.Fatalf("ensure github pr session row: %v", err)
	}
	if err := prSessions.SetSessionID(ctx, "acme/widgets", 42, sessionID); err != nil {
		t.Fatalf("set github pr session id: %v", err)
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "tool_call", Gen: 1, MessageID: "toolcall-1",
		Raw: toolCallRaw(t, sessionID.String(), 1, "toolcall-1", "call-1"),
	})

	if n := countOutboxRowsForSession(ctx, t, pool, sessionID); n != 0 {
		t.Errorf("outbox row count for github-origin session's tool_call = %d, want 0", n)
	}
}

// TestLinearProgress_FirstToolCallOfATurn_ProductionShape_NotifiesOnce is
// the shape a real turn sends, which every other test in this file missed:
// a message's step_start first, then its tool calls, all under the
// message's id (translate.go). Under the wire messageId alone each
// tool_call deduped onto the step_start's row, insertedFresh was false,
// and no Linear-origin session ever got this notice. Each tool_call now
// has its own key, so the first of the turn notifies, once, and the
// second does not.
func TestLinearProgress_FirstToolCallOfATurn_ProductionShape_NotifiesOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceLinear)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	createDispatchedProcessingTurn(ctx, t, pool, sessionID)
	claimLinearAgentSessionForTest(ctx, t, pool, sessionID, "agent-session-1", "org-1")

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sid := sessionID.String()
	sendSandboxEventForTest(ctx, t, a, messageEventForTest(t, sid, "step_start", "msg_1", "prt_start"))
	sendSandboxEventForTest(ctx, t, a, messageEventForTest(t, sid, "tool_call", "msg_1", "call_a"))
	sendSandboxEventForTest(ctx, t, a, messageEventForTest(t, sid, "tool_call", "msg_1", "call_b"))

	if n := countOutboxRowsForSessionKind(ctx, t, pool, sessionID, string(ports.NotificationKindLinearProgress)); n != 1 {
		t.Fatalf("linear progress notices = %d, want exactly 1: the turn's first tool_call notifies, the second does not", n)
	}
	if got := storedKeys(ctx, t, pool, sessionID, "tool_call"); len(got) != 2 {
		t.Fatalf("stored tool_calls %q, want both, each under its own key", got)
	}
}
