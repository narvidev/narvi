//go:build integration

package sessionactor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/rollout"
	"github.com/narvidev/narvi/internal/platform"
)

// This file is the exit of technical plan §3.3's prompt receipts
// (promptreceipt.go) on the real actor and real Postgres: a prompt lost
// between its dispatch and the sandbox is sent again, once per same-gen
// reconnect, with its own messageId, to a gen that advertised the
// capability and whose dispatch asked -- and to nothing else.

// receiptCommander is a ports.SandboxCommander recording every command it
// is asked to send. failErr, when set, fails every send (after recording
// it). block, when set, holds the next send until it is closed, closing
// blocked once that send is held.
type receiptCommander struct {
	mu       sync.Mutex
	payloads []json.RawMessage
	failErr  error
	block    chan struct{}
	blocked  chan struct{}
}

var _ ports.SandboxCommander = (*receiptCommander)(nil)

func (c *receiptCommander) SendCommand(_ string, payload json.RawMessage) error {
	c.mu.Lock()
	c.payloads = append(c.payloads, append(json.RawMessage(nil), payload...))
	err := c.failErr
	block, blocked := c.block, c.blocked
	c.block = nil
	c.mu.Unlock()
	if block != nil {
		close(blocked)
		<-block
	}
	return err
}

// holdNextSend makes the next send block; it returns a channel closed once
// that send is held, and a release func.
func (c *receiptCommander) holdNextSend() (held <-chan struct{}, release func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.block, c.blocked = make(chan struct{}), make(chan struct{})
	block := c.block
	var once sync.Once
	return c.blocked, func() { once.Do(func() { close(block) }) }
}

func (c *receiptCommander) setFail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failErr = err
}

// prompts returns every prompt command sent, in order, each with its raw
// payload.
func (c *receiptCommander) prompts(t *testing.T) []sentPrompt {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []sentPrompt
	for _, payload := range c.payloads {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &head); err != nil {
			t.Fatalf("malformed command payload %s: %v", payload, err)
		}
		if head.Type != "prompt" {
			continue
		}
		var p sandboxws.Prompt
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("prompt fails its contract: %v (%s)", err, payload)
		}
		out = append(out, sentPrompt{Prompt: p, raw: payload})
	}
	return out
}

// commandTypes returns the type of every command sent, in order.
func (c *receiptCommander) commandTypes(t *testing.T) []string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, payload := range c.payloads {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &head); err != nil {
			t.Fatalf("malformed command payload %s: %v", payload, err)
		}
		out = append(out, head.Type)
	}
	return out
}

type sentPrompt struct {
	sandboxws.Prompt
	raw json.RawMessage
}

// asksReceipt reports whether the prompt's payload carries
// receiptRequested, failing the test when the key carries anything but
// true: a prompt that asks for none omits it.
func (p sentPrompt) asksReceipt(t *testing.T) bool {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(p.raw, &keys); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	value, ok := keys["receiptRequested"]
	if ok && string(value) != "true" {
		t.Fatalf("prompt carries receiptRequested = %s, want the key absent or true", value)
	}
	return ok
}

// Wire frames for gen, as a sandbox-agent sends them.

func receiptReady(gen int, capable bool) SandboxEvent {
	id := "ready-" + uuid.NewString()
	caps := ""
	if capable {
		caps = `,"capabilities":{"promptReceipt":true}`
	}
	agentVersion, imageDigest := "dev", "unknown"
	return SandboxEvent{Type: "ready", Gen: gen, MessageID: id, AgentVersion: &agentVersion, ImageDigest: &imageDigest,
		Raw: json.RawMessage(fmt.Sprintf(`{"type":"ready","messageId":%q,"sessionId":"s","gen":%d,"timestamp":"2026-10-01T12:00:00Z","agentVersion":"dev","imageDigest":"unknown"%s}`, id, gen, caps))}
}

func receiptHeartbeat(gen int) SandboxEvent {
	return nullPhaseHeartbeat("hb-"+uuid.NewString(), gen)
}

func promptReceivedEvent(promptMessageID string, gen int, duplicate bool) SandboxEvent {
	id := "prompt_received:" + promptMessageID
	return SandboxEvent{Type: "prompt_received", Gen: gen, MessageID: id,
		Raw: json.RawMessage(fmt.Sprintf(`{"type":"prompt_received","messageId":%q,"sessionId":"s","gen":%d,"promptMessageId":%q,"duplicate":%t}`, id, gen, promptMessageID, duplicate))}
}

func completedEvent(gen int) SandboxEvent {
	id := "ec-" + uuid.NewString()
	return SandboxEvent{Type: "execution_complete", Gen: gen, MessageID: id,
		Raw: json.RawMessage(fmt.Sprintf(`{"type":"execution_complete","messageId":%q,"sessionId":"s","gen":%d,"ackId":"execution_complete:%s","outcome":"completed","reason":null}`, id, gen, id))}
}

// sendAndSettle hands cmd to a and returns once a has finished with it,
// its post-commit dispatch evaluation included: the reply comes before
// that evaluation, so a heartbeat of gen follows it -- the mailbox is
// handled in order, and the heartbeat's reply comes only once cmd's
// handling has returned.
func sendAndSettle(ctx context.Context, t *testing.T, a *Actor, cmd SandboxEvent, gen int) {
	t.Helper()
	sendSandboxEvent(ctx, t, a, cmd)
	sendSandboxEvent(ctx, t, a, receiptHeartbeat(gen))
}

// receiptRig is one session on the real actor: a sandbox at gen 1, Ready,
// and -- unless noTurn -- one pending turn.
type receiptRig struct {
	pool      *pgxpool.Pool
	sessionID pgtype.UUID
	turnID    pgtype.UUID
	turns     *narvipg.TurnStore
	sandboxes *narvipg.SandboxStore
	commander *receiptCommander
	registry  *Registry
	actor     *Actor
}

type receiptRigOptions struct {
	noTurn   bool
	provider ports.SandboxProvider
	timeouts *platform.Timeouts
	// prompt is the pending turn's text; "do the thing" when empty.
	prompt string
	// repoFullName, when set, gives the session that one repo, enrolled in
	// the cohort rollout, and the registry runs in cohort mode.
	repoFullName string
}

func newReceiptRig(ctx context.Context, t *testing.T, opts receiptRigOptions) *receiptRig {
	t.Helper()
	pool := newTestPool(t)
	rig := &receiptRig{
		pool:      pool,
		turns:     narvipg.NewTurnStore(pool),
		sandboxes: narvipg.NewSandboxStore(pool),
		commander: &receiptCommander{},
	}
	var options []RegistryOptions
	if opts.repoFullName != "" {
		rig.sessionID = createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", "https://github.com/"+opts.repoFullName+".git", "")
		if _, err := narvipg.NewRepoSettingsStore(pool).UpsertSessionsEnabled(ctx, opts.repoFullName, true); err != nil {
			t.Fatalf("enroll the repo: %v", err)
		}
		options = append(options, RegistryOptions{RolloutMode: rollout.ModeCohort})
	} else {
		rig.sessionID = createTestSession(ctx, t, pool)
	}
	if _, err := rig.sandboxes.Create(ctx, rig.sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := rig.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: rig.sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}
	if !opts.noTurn {
		prompt := opts.prompt
		if prompt == "" {
			prompt = "do the thing"
		}
		rig.turnID = createPendingTurn(ctx, t, rig.turns, rig.sessionID, prompt).ID
	}
	timeouts := platform.DefaultTimeouts()
	if opts.timeouts != nil {
		timeouts = *opts.timeouts
	}
	r, err := NewRegistry(ctx, pool, timeouts, nil, rig.commander, opts.provider, "http://localhost:8080", nil, nil, "", nil, false, options...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	rig.registry = r
	if rig.actor, err = r.GetOrSpawn(ctx, rig.sessionID); err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	return rig
}

func (r *receiptRig) turn(ctx context.Context, t *testing.T) sqlcgen.Turn {
	t.Helper()
	row, err := r.turns.Get(ctx, r.turnID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	return row
}

func (r *receiptRig) sandbox(ctx context.Context, t *testing.T) sqlcgen.Sandbox {
	t.Helper()
	row, err := r.sandboxes.Get(ctx, r.sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return row
}

// turnDeadline returns the turn_deadline timer's fires_at.
func (r *receiptRig) turnDeadline(ctx context.Context, t *testing.T) time.Time {
	t.Helper()
	var firesAt time.Time
	if err := r.pool.QueryRow(ctx, `SELECT fires_at FROM session_timers WHERE session_id = $1 AND name = $2`,
		r.sessionID, TimerTurnDeadline).Scan(&firesAt); err != nil {
		t.Fatalf("read turn_deadline: %v", err)
	}
	return firesAt
}

// executionCompleteRows counts the session's execution_complete events,
// and the synthetic ones among them.
func (r *receiptRig) executionCompleteRows(ctx context.Context, t *testing.T) (all, synthetic int) {
	t.Helper()
	if err := r.pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE payload ? 'synthetic') FROM events WHERE session_id = $1 AND type = 'execution_complete'`,
		r.sessionID).Scan(&all, &synthetic); err != nil {
		t.Fatalf("count execution_complete events: %v", err)
	}
	return all, synthetic
}

// dispatchedFacts are the columns of a dispatch a receipt re-send must
// never move.
type dispatchedFacts struct {
	dispatchedAt time.Time
	eventID      int64
	gen          int32
	messageID    string
	deadline     time.Time
}

func (r *receiptRig) dispatched(ctx context.Context, t *testing.T) dispatchedFacts {
	t.Helper()
	row := r.turn(ctx, t)
	if row.DispatchedEventID == nil || row.DispatchedSandboxGen == nil || row.DispatchedMessageID == nil {
		t.Fatalf("turn not dispatched: %+v", row)
	}
	return dispatchedFacts{
		dispatchedAt: row.DispatchedAt.Time, eventID: *row.DispatchedEventID, gen: *row.DispatchedSandboxGen,
		messageID: *row.DispatchedMessageID, deadline: r.turnDeadline(ctx, t),
	}
}

// promptResendCount reads turn_prompt_resend_total for outcome.
func promptResendCount(ctx context.Context, t *testing.T, outcome string) int64 {
	t.Helper()
	return readCounterSumByAttr(ctx, t, otelReader, "turn_prompt_resend_total", "outcome", outcome)
}

// TestPromptReceipt_IncapableGen_DispatchRequestsNothing: a gen whose ready
// advertised no capability is asked for no receipt -- the prompt is
// byte-for-byte a prompt without receipts, and the turn records no request
// -- and its same-gen reconnect re-sends nothing.
func TestPromptReceipt_IncapableGen_DispatchRequestsNothing(t *testing.T) {
	ctx := context.Background()
	rig := newReceiptRig(ctx, t, receiptRigOptions{})

	sendAndSettle(ctx, t, rig.actor, receiptReady(1, false), 1)
	prompts := rig.commander.prompts(t)
	if len(prompts) != 1 {
		t.Fatalf("%d prompts sent, want the one dispatch", len(prompts))
	}
	if prompts[0].asksReceipt(t) {
		t.Fatalf("prompt to an incapable gen carries receiptRequested: %s", prompts[0].raw)
	}
	got := rig.turn(ctx, t)
	if got.ReceiptRequestedMessageID != nil || got.ReceiptRequestedAt.Valid || got.ReceiptCheckedReadySeq != nil {
		t.Fatalf("turn records a receipt request (%v, %v, %v), want none", got.ReceiptRequestedMessageID, got.ReceiptRequestedAt, got.ReceiptCheckedReadySeq)
	}

	// The same gen reconnects, then heartbeats: never re-sent.
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, false), 1)
	for i := 0; i < 3; i++ {
		sendSandboxEvent(ctx, t, rig.actor, receiptHeartbeat(1))
	}
	sendAndSettle(ctx, t, rig.actor, receiptHeartbeat(1), 1)
	if got := len(rig.commander.prompts(t)); got != 1 {
		t.Fatalf("%d prompts sent after a same-gen reconnect, want 1: an incapable gen is never re-sent a prompt", got)
	}
}

// TestPromptReceipt_DispatchRecordsRequestInItsOwnCommit: the request is
// written in the dispatch's own commit -- it is there, tied to the
// prompt's messageId, while the send is still in flight.
func TestPromptReceipt_DispatchRecordsRequestInItsOwnCommit(t *testing.T) {
	ctx := context.Background()
	rig := newReceiptRig(ctx, t, receiptRigOptions{})
	held, release := rig.commander.holdNextSend()
	t.Cleanup(release)

	sendSandboxEvent(ctx, t, rig.actor, receiptReady(1, true))
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the dispatch never reached its send")
	}

	got := rig.turn(ctx, t)
	if got.Status != sqlcgen.TurnStatusProcessing || got.DispatchedMessageID == nil {
		t.Fatalf("turn status %s, dispatched_message_id %v, want processing with one stamped", got.Status, got.DispatchedMessageID)
	}
	if got.ReceiptRequestedMessageID == nil || *got.ReceiptRequestedMessageID != *got.DispatchedMessageID {
		t.Fatalf("receipt_requested_message_id = %v while the send is in flight, want %q, the dispatch's own", got.ReceiptRequestedMessageID, *got.DispatchedMessageID)
	}
	if !got.ReceiptRequestedAt.Valid {
		t.Fatal("receipt_requested_at not set while the send is in flight")
	}
	if got.ReceiptCheckedReadySeq == nil || *got.ReceiptCheckedReadySeq != 1 {
		t.Fatalf("receipt_checked_ready_seq = %v, want 1, the gen's ready_seq at the dispatch", got.ReceiptCheckedReadySeq)
	}
	prompts := rig.commander.prompts(t)
	if len(prompts) != 1 || !prompts[0].asksReceipt(t) || prompts[0].MessageId != *got.DispatchedMessageID {
		t.Fatalf("the prompt in flight = %+v, want one asking for a receipt under messageId %q", prompts, *got.DispatchedMessageID)
	}
	release()
}

// TestPromptReceipt_ReadyRecordsCapabilityForItsGenOnly: every ready of the
// live gen counts, and its capability is recorded against that gen -- the
// latest ready decides -- while a stale gen's ready records nothing, and a
// gen bump inherits nothing.
func TestPromptReceipt_ReadyRecordsCapabilityForItsGenOnly(t *testing.T) {
	ctx := context.Background()
	rig := newReceiptRig(ctx, t, receiptRigOptions{noTurn: true})

	steps := []struct {
		name        string
		event       SandboxEvent
		wantSeq     int32
		wantCapable bool
	}{
		{name: "capable ready", event: receiptReady(1, true), wantSeq: 1, wantCapable: true},
		{name: "a later ready without the capability clears it", event: receiptReady(1, false), wantSeq: 2, wantCapable: false},
		{name: "capable again", event: receiptReady(1, true), wantSeq: 3, wantCapable: true},
		{name: "a stale gen's ready records nothing", event: receiptReady(7, false), wantSeq: 3, wantCapable: true},
	}
	for _, step := range steps {
		sendAndSettle(ctx, t, rig.actor, step.event, 1)
		row := rig.sandbox(ctx, t)
		if row.ReadySeq != step.wantSeq || promptReceiptCapable(row) != step.wantCapable {
			t.Fatalf("%s: ready_seq = %d, capable = %v (prompt_receipt_gen %v, gen %d); want %d, %v",
				step.name, row.ReadySeq, promptReceiptCapable(row), row.PromptReceiptGen, row.Gen, step.wantSeq, step.wantCapable)
		}
	}

	// A respawn bumps the gen: the capability, recorded for gen 1, does not
	// count for gen 2, though nothing reset it.
	if _, err := rig.sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: rig.sessionID}); err != nil {
		t.Fatalf("respawn: %v", err)
	}
	row := rig.sandbox(ctx, t)
	if row.Gen != 2 || row.PromptReceiptGen == nil || *row.PromptReceiptGen != 1 || promptReceiptCapable(row) {
		t.Fatalf("after a respawn: gen %d, prompt_receipt_gen %v, capable %v; want gen 2, prompt_receipt_gen 1, not capable",
			row.Gen, row.PromptReceiptGen, promptReceiptCapable(row))
	}
}

// TestPromptReceipt_CapableGen_SameGenReadyResendsOnce_SameMessageID is the
// re-send itself: each same-gen reconnect without a stored receipt sends
// the original prompt once more -- its messageId, asking again -- and
// nothing else of the dispatch moves; heartbeats send nothing; a stored
// receipt ends it.
func TestPromptReceipt_CapableGen_SameGenReadyResendsOnce_SameMessageID(t *testing.T) {
	ctx := context.Background()
	rig := newReceiptRig(ctx, t, receiptRigOptions{})
	sentBefore := promptResendCount(ctx, t, promptResendOutcomeSent)

	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	first := rig.dispatched(ctx, t)
	prompts := rig.commander.prompts(t)
	if len(prompts) != 1 || !prompts[0].asksReceipt(t) || prompts[0].MessageId != first.messageID {
		t.Fatalf("dispatch sent %+v, want one prompt asking for a receipt", prompts)
	}

	// The reconnect: one re-send, the original messageId, asking again.
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	prompts = rig.commander.prompts(t)
	if len(prompts) != 2 {
		t.Fatalf("%d prompts after a same-gen reconnect with no receipt stored, want 2", len(prompts))
	}
	if prompts[1].MessageId != first.messageID || !prompts[1].asksReceipt(t) || prompts[1].Gen != 1 {
		t.Fatalf("re-sent prompt = %+v, want messageId %q, gen 1, asking for a receipt", prompts[1].Prompt, first.messageID)
	}
	if !bytes.Equal(prompts[0].raw, prompts[1].raw) {
		t.Fatalf("re-sent prompt differs from the original:\n%s\n%s", prompts[0].raw, prompts[1].raw)
	}

	// Heartbeats are no reconnect.
	for i := 0; i < 3; i++ {
		sendSandboxEvent(ctx, t, rig.actor, receiptHeartbeat(1))
	}
	sendAndSettle(ctx, t, rig.actor, receiptHeartbeat(1), 1)
	if got := len(rig.commander.prompts(t)); got != 2 {
		t.Fatalf("%d prompts after heartbeats, want still 2: only a reconnect is answered", got)
	}

	// Another reconnect: one more.
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	if got := len(rig.commander.prompts(t)); got != 3 {
		t.Fatalf("%d prompts after a second reconnect, want 3", got)
	}

	// The receipt arrives; a reconnect after it sends nothing.
	sendAndSettle(ctx, t, rig.actor, promptReceivedEvent(first.messageID, 1, false), 1)
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	if got := len(rig.commander.prompts(t)); got != 3 {
		t.Fatalf("%d prompts after a reconnect with the receipt stored, want still 3", got)
	}

	if after := rig.dispatched(ctx, t); after != first {
		t.Fatalf("the dispatch moved across the re-sends: before %+v, after %+v (no re-arm, no dispatched_* move)", first, after)
	}
	if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("turn status = %s, want processing", got.Status)
	}
	if got := promptResendCount(ctx, t, promptResendOutcomeSent) - sentBefore; got != 2 {
		t.Fatalf("turn_prompt_resend_total{sent} moved by %d, want 2", got)
	}
}

// previousUpdateTurnStatus is UpdateTurnStatus as the control plane without
// prompt receipts sends it: it names none of their columns.
const previousUpdateTurnStatus = `UPDATE turns
SET status = $2,
    dispatched_at = COALESCE($3, dispatched_at),
    completed_at = COALESCE($4, completed_at),
    dispatched_sandbox_gen = COALESCE($5, dispatched_sandbox_gen),
    dispatched_event_id = COALESCE($6, dispatched_event_id),
    dispatched_message_id = COALESCE($7, dispatched_message_id)
WHERE id = $1`

// previousBinaryDispatch dispatches turnID as that control plane does,
// with messageID to gen.
func previousBinaryDispatch(ctx context.Context, t *testing.T, pool *pgxpool.Pool, turnID pgtype.UUID, gen int32, messageID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, previousUpdateTurnStatus, turnID, sqlcgen.TurnStatusProcessing,
		pgtype.Timestamptz{Time: time.Now(), Valid: true}, nil, gen, int64(0), messageID); err != nil {
		t.Fatalf("the previous binary's dispatch: %v", err)
	}
}

// TestPromptReceipt_MixedReplicas_DispatchedByPreviousBinary_CompletesNeverFailed
// is the first deploy order: a turn dispatched by a replica without
// receipts, its sandbox then reconnecting -- capable -- to this one. That
// dispatch asked for nothing, so nothing is re-sent, and the turn
// completes on the agent's execution_complete, never failed. The stale
// variant: this replica's earlier dispatch to an older gen asked, and the
// previous binary's re-enqueue to the new gen left that request standing
// under a messageId it no longer has.
func TestPromptReceipt_MixedReplicas_DispatchedByPreviousBinary_CompletesNeverFailed(t *testing.T) {
	ctx := context.Background()

	t.Run("never asked", func(t *testing.T) {
		rig := newReceiptRig(ctx, t, receiptRigOptions{})
		previousBinaryDispatch(ctx, t, rig.pool, rig.turnID, 1, "msg-previous-binary")

		sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
		for i := 0; i < 3; i++ {
			sendSandboxEvent(ctx, t, rig.actor, receiptHeartbeat(1))
		}
		sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
		if got := len(rig.commander.prompts(t)); got != 0 {
			t.Fatalf("%d prompts sent for a turn whose dispatch asked for no receipt, want 0", got)
		}

		sendAndSettle(ctx, t, rig.actor, completedEvent(1), 1)
		assertCompletedNeverFailed(ctx, t, rig)
	})

	t.Run("a stale request from an earlier gen", func(t *testing.T) {
		rig := newReceiptRig(ctx, t, receiptRigOptions{})
		sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
		asked := rig.dispatched(ctx, t).messageID

		// The sandbox is lost and respawned at gen 2, Ready, and the
		// previous binary re-enqueues the turn there under a new messageId.
		if _, err := rig.sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: rig.sessionID}); err != nil {
			t.Fatalf("respawn: %v", err)
		}
		if _, err := rig.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: rig.sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
			t.Fatalf("move sandbox to ready: %v", err)
		}
		previousBinaryDispatch(ctx, t, rig.pool, rig.turnID, 2, "msg-previous-binary-reenqueue")
		if got := rig.turn(ctx, t); got.ReceiptRequestedMessageID == nil || *got.ReceiptRequestedMessageID != asked {
			t.Fatalf("receipt_requested_message_id = %v, want the stale %q the previous binary left", got.ReceiptRequestedMessageID, asked)
		}

		// Gen 2 is capable and reconnects, twice.
		sendAndSettle(ctx, t, rig.actor, receiptReady(2, true), 2)
		sendAndSettle(ctx, t, rig.actor, receiptReady(2, true), 2)
		if got := len(rig.commander.prompts(t)); got != 1 {
			t.Fatalf("%d prompts sent, want only gen 1's dispatch: the stale request is not the current dispatch's", got)
		}

		sendAndSettle(ctx, t, rig.actor, completedEvent(2), 2)
		assertCompletedNeverFailed(ctx, t, rig)
	})
}

func assertCompletedNeverFailed(ctx context.Context, t *testing.T, rig *receiptRig) {
	t.Helper()
	if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusCompleted {
		t.Fatalf("turn status = %s, want completed", got.Status)
	}
	all, synthetic := rig.executionCompleteRows(ctx, t)
	if all != 1 || synthetic != 0 {
		t.Fatalf("execution_complete events = %d (%d synthetic), want the agent's one, no synthetic", all, synthetic)
	}
}

// TestPromptReceipt_MixedReplicas_ReceiptStoredByPreviousBinary_CompletesNeverFailed
// is the second deploy order: this replica dispatches to a capable gen,
// asking for a receipt, and a replica without receipts then stores the
// receipt the agent replays -- through its generic path, under the wire
// messageId, with no ack. When the session returns to a replica with
// receipts on the gen's next reconnect, that row is found by its key:
// nothing is re-sent, and the turn completes, never failed.
func TestPromptReceipt_MixedReplicas_ReceiptStoredByPreviousBinary_CompletesNeverFailed(t *testing.T) {
	ctx := context.Background()
	rig := newReceiptRig(ctx, t, receiptRigOptions{})
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	messageID := rig.dispatched(ctx, t).messageID

	// This replica goes; the previous binary takes the session and stores
	// the receipt.
	_ = rig.registry.Shutdown()
	previous := newPreviousBinary(ctx, t, rig.pool, rig.sessionID)
	receipt := promptReceivedEvent(messageID, 1, false)
	if inserted := previous.store(ctx, t, receipt); !inserted {
		t.Fatal("the previous binary stored no receipt row")
	}
	if ack := peekAckID(receipt.Raw); ack != "" {
		t.Fatalf("a receipt carries ackId %q, want none: it is not critical", ack)
	}

	// The session returns to a replica with receipts, on the gen's next
	// reconnect.
	r2, err := NewRegistry(ctx, rig.pool, platform.DefaultTimeouts(), nil, rig.commander, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r2.Shutdown() })
	a2, err := r2.GetOrSpawn(ctx, rig.sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendAndSettle(ctx, t, a2, receiptReady(1, true), 1)
	if got := len(rig.commander.prompts(t)); got != 1 {
		t.Fatalf("%d prompts sent, want only the dispatch: the receipt the previous binary stored is found", got)
	}
	got := rig.turn(ctx, t)
	if got.ReceiptCheckedReadySeq == nil || *got.ReceiptCheckedReadySeq != rig.sandbox(ctx, t).ReadySeq {
		t.Fatalf("receipt_checked_ready_seq = %v, want the answered ready %d", got.ReceiptCheckedReadySeq, rig.sandbox(ctx, t).ReadySeq)
	}

	sendAndSettle(ctx, t, a2, completedEvent(1), 1)
	assertCompletedNeverFailed(ctx, t, rig)
}

// TestPromptReceipt_RestoreFromPreChangeSnapshot_NoResendNoDoubleRun: a
// capable gen is lost and restored from a snapshot whose agent predates
// receipts. The restore bumps the gen, so the capability recorded for the
// old one counts for nothing: the turn is re-enqueued to the new gen
// without asking, its old request cleared, and the new gen's same-gen
// reconnect re-sends nothing -- each messageId reaches an agent once, so
// an agent that cannot dedup runs nothing twice. Whether this replica
// recorded the new gen's boot or an older one did, the answer is the same.
func TestPromptReceipt_RestoreFromPreChangeSnapshot_NoResendNoDoubleRun(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		boot func(ctx context.Context, t *testing.T, rig *receiptRig)
	}{
		{name: "the new gen's boot recorded here", boot: func(ctx context.Context, t *testing.T, rig *receiptRig) {
			sendSandboxEvent(ctx, t, rig.actor, receiptReady(2, false))
			sendSandboxEvent(ctx, t, rig.actor, bootProgressEvent("bp-"+uuid.NewString(), 2, "web:starting"))
			sendAndSettle(ctx, t, rig.actor, receiptHeartbeat(2), 2)
		}},
		{name: "the new gen's boot recorded by a replica that counts no readies", boot: func(ctx context.Context, t *testing.T, rig *receiptRig) {
			if _, err := rig.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: rig.sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
				t.Fatalf("move sandbox to ready: %v", err)
			}
			sendEnsureDispatched(ctx, t, rig.actor)
			sendAndSettle(ctx, t, rig.actor, receiptHeartbeat(2), 2)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &fakeSpawnProvider{nextRestoreRef: ports.SandboxRef{ProviderID: "restored-" + uuid.NewString()}}
			rig := newReceiptRig(ctx, t, receiptRigOptions{provider: provider})
			sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
			first := rig.dispatched(ctx, t)

			// Gen 1 is lost, with a snapshot taken before the change.
			if _, err := rig.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: rig.sessionID, Status: sqlcgen.SandboxStatusStopped}); err != nil {
				t.Fatalf("move sandbox to stopped: %v", err)
			}
			snapshotID := "snap-pre-change"
			if _, err := rig.sandboxes.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{SessionID: rig.sessionID, SnapshotID: &snapshotID}); err != nil {
				t.Fatalf("seed snapshot id: %v", err)
			}
			sendEnsureDispatched(ctx, t, rig.actor)
			waitUntil(t, 5*time.Second, func() bool {
				row, err := rig.sandboxes.Get(ctx, rig.sessionID)
				return err == nil && row.Gen == 2 && row.Status == sqlcgen.SandboxStatusConnecting
			})
			if got := provider.restoreCallCount(); got != 1 {
				t.Fatalf("RestoreFromSnapshot called %d times, want 1", got)
			}

			tc.boot(ctx, t, rig)
			waitUntil(t, 5*time.Second, func() bool { return len(rig.commander.prompts(t)) == 2 })
			prompts := rig.commander.prompts(t)
			reenqueued := prompts[1]
			if reenqueued.Gen != 2 || reenqueued.MessageId == first.messageID {
				t.Fatalf("re-enqueued prompt = %+v, want gen 2 under a new messageId", reenqueued.Prompt)
			}
			if reenqueued.asksReceipt(t) {
				t.Fatalf("the prompt to the restored gen asks for a receipt: it inherited gen 1's capability (%s)", reenqueued.raw)
			}
			got := rig.turn(ctx, t)
			if got.ReceiptRequestedMessageID != nil || got.ReceiptRequestedAt.Valid || got.ReceiptCheckedReadySeq != nil {
				t.Fatalf("turn's receipt request = (%v, %v, %v), want cleared by the re-enqueue",
					got.ReceiptRequestedMessageID, got.ReceiptRequestedAt, got.ReceiptCheckedReadySeq)
			}

			// The restored gen reconnects, then heartbeats: nothing more.
			sendAndSettle(ctx, t, rig.actor, receiptReady(2, false), 2)
			for i := 0; i < 3; i++ {
				sendSandboxEvent(ctx, t, rig.actor, receiptHeartbeat(2))
			}
			sendAndSettle(ctx, t, rig.actor, receiptReady(2, false), 2)
			perMessage := map[string]int{}
			for _, p := range rig.commander.prompts(t) {
				perMessage[p.MessageId]++
			}
			if len(perMessage) != 2 || perMessage[first.messageID] != 1 || perMessage[reenqueued.MessageId] != 1 {
				t.Fatalf("prompts per messageId = %v, want each of the two exactly once", perMessage)
			}
		})
	}
}

// TestPromptReceipt_StopFlaggedUnacknowledged_NeverResent: a turn a
// person's stop flagged is never re-sent, its receipt missing or not: the
// reconnect sends nothing, and the stop timer then sends `stop`, never a
// prompt.
func TestPromptReceipt_StopFlaggedUnacknowledged_NeverResent(t *testing.T) {
	ctx := context.Background()
	rig := newReceiptRig(ctx, t, receiptRigOptions{})
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)

	// The REST stop's own writes, under the session-row lock.
	tx, err := rig.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sessions := narvipg.NewSessionStore(rig.pool)
	if _, err := sessions.WithTx(tx).GetActorEpochForUpdate(ctx, rig.sessionID); err != nil {
		t.Fatalf("lock the session: %v", err)
	}
	flagged, err := rig.turns.WithTx(tx).RequestStopOpen(ctx, rig.sessionID)
	if err != nil || len(flagged) != 1 {
		t.Fatalf("flag the open turn: %v (%d flagged)", err, len(flagged))
	}
	requestedAt, err := sessions.WithTx(tx).RequestStop(ctx, rig.sessionID)
	if err != nil {
		t.Fatalf("record the stop: %v", err)
	}
	if _, err := narvipg.NewTimerStore(rig.pool).WithTx(tx).Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: rig.sessionID, Name: TimerStop, FiresAt: requestedAt,
	}); err != nil {
		t.Fatalf("arm the stop timer: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	if got := len(rig.commander.prompts(t)); got != 1 {
		t.Fatalf("%d prompts sent, want only the dispatch: a stop-flagged turn is never re-sent", got)
	}

	if err := rig.registry.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		types := rig.commander.commandTypes(t)
		return len(types) > 0 && types[len(types)-1] == "stop"
	})
	sendAndSettle(ctx, t, rig.actor, receiptHeartbeat(1), 1)
	prompts := 0
	for _, typ := range rig.commander.commandTypes(t) {
		if typ == "prompt" {
			prompts++
		}
	}
	if prompts != 1 {
		t.Fatalf("commands sent = %v, want one prompt then the stop", rig.commander.commandTypes(t))
	}
}

// countLogLines counts the log lines whose msg is msg.
func countLogLines(t *testing.T, buf *syncLogBuffer, msg string) int {
	t.Helper()
	n := 0
	for _, line := range bytes.Split([]byte(buf.String()), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if entry["msg"] == msg {
			n++
		}
	}
	return n
}

const windowExpiredMsg = "sessionactor: prompt not receipted by its sandbox, but its dispatch asked longer ago than the re-send window; not re-sent, the turn ends at its deadline"

// TestPromptReceipt_WindowExpired_NoResend_TurnNotFailed: past
// PromptResendWindow nothing is sent; each reconnect logs one WARN and
// counts once; the turn stays processing, to end at its deadline as
// before receipts existed -- no new failure path.
func TestPromptReceipt_WindowExpired_NoResend_TurnNotFailed(t *testing.T) {
	logs := captureDefaultLoggerJSONSync(t)
	ctx := context.Background()
	rig := newReceiptRig(ctx, t, receiptRigOptions{})
	expiredBefore := promptResendCount(ctx, t, promptResendOutcomeWindowExpired)

	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	window := platform.DefaultTimeouts().PromptResendWindow
	if _, err := rig.pool.Exec(ctx,
		`UPDATE turns SET receipt_requested_at = now() - make_interval(secs => $2::double precision) WHERE id = $1`,
		rig.turnID, (window + time.Minute).Seconds()); err != nil {
		t.Fatalf("age the request past the window: %v", err)
	}

	for i := 1; i <= 2; i++ {
		sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
		if got := len(rig.commander.prompts(t)); got != 1 {
			t.Fatalf("reconnect %d: %d prompts sent, want only the dispatch", i, got)
		}
		if got := countLogLines(t, logs, windowExpiredMsg); got != i {
			t.Fatalf("reconnect %d: %d window WARN lines, want %d (one per reconnect)", i, got, i)
		}
		if got := promptResendCount(ctx, t, promptResendOutcomeWindowExpired) - expiredBefore; got != int64(i) {
			t.Fatalf("reconnect %d: turn_prompt_resend_total{window_expired} moved by %d, want %d", i, got, i)
		}
		got := rig.turn(ctx, t)
		if got.ReceiptCheckedReadySeq == nil || *got.ReceiptCheckedReadySeq != rig.sandbox(ctx, t).ReadySeq {
			t.Fatalf("reconnect %d: receipt_checked_ready_seq = %v, want the answered ready", i, got.ReceiptCheckedReadySeq)
		}
	}

	if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("turn status = %s, want processing: the window adds no failure path", got.Status)
	}
	if all, _ := rig.executionCompleteRows(ctx, t); all != 0 {
		t.Fatalf("%d execution_complete events, want none", all)
	}
}

// TestPromptReceipt_ResendSendFailure_TurnStaysProcessing: a re-send whose
// write fails fails nothing -- the original prompt may be running -- and
// the next reconnect asks again.
func TestPromptReceipt_ResendSendFailure_TurnStaysProcessing(t *testing.T) {
	ctx := context.Background()
	rig := newReceiptRig(ctx, t, receiptRigOptions{})
	failedBefore := promptResendCount(ctx, t, promptResendOutcomeSendFailed)
	sentBefore := promptResendCount(ctx, t, promptResendOutcomeSent)

	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	messageID := rig.dispatched(ctx, t).messageID

	rig.commander.setFail(errors.New("the socket went away"))
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	if got := len(rig.commander.prompts(t)); got != 2 {
		t.Fatalf("%d prompts attempted, want the dispatch and one re-send", got)
	}
	if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("turn status = %s after a failed re-send, want processing", got.Status)
	}
	if all, _ := rig.executionCompleteRows(ctx, t); all != 0 {
		t.Fatalf("%d execution_complete events after a failed re-send, want none", all)
	}
	if got := promptResendCount(ctx, t, promptResendOutcomeSendFailed) - failedBefore; got != 1 {
		t.Fatalf("turn_prompt_resend_total{send_failed} moved by %d, want 1", got)
	}

	rig.commander.setFail(nil)
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	prompts := rig.commander.prompts(t)
	if len(prompts) != 3 || prompts[2].MessageId != messageID {
		t.Fatalf("prompts = %d, last %q; want a third, the same messageId %q", len(prompts), prompts[len(prompts)-1].MessageId, messageID)
	}
	if got := promptResendCount(ctx, t, promptResendOutcomeSent) - sentBefore; got != 1 {
		t.Fatalf("turn_prompt_resend_total{sent} moved by %d, want 1", got)
	}
}

const capReachedMsg = "sessionactor: prompt not receipted by its sandbox, but it has been re-sent the most times a turn allows; not re-sent, the turn ends at its deadline"

// TestPromptReceipt_ResendCapStopsADeterministicLoss: a prompt lost on
// every delivery -- each send answered by another reconnect, as a frame
// the sandbox can never take was -- is re-sent PromptResendMaxPerTurn
// times and then no more: each later reconnect logs one WARN and counts
// cap_reached, and the turn stays processing for its deadline.
func TestPromptReceipt_ResendCapStopsADeterministicLoss(t *testing.T) {
	logs := captureDefaultLoggerJSONSync(t)
	ctx := context.Background()
	rig := newReceiptRig(ctx, t, receiptRigOptions{})
	maxResends := platform.DefaultTimeouts().PromptResendMaxPerTurn
	capBefore := promptResendCount(ctx, t, promptResendOutcomeCapReached)

	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	messageID := rig.dispatched(ctx, t).messageID
	// Every send is lost, and the loss is a reconnect.
	for i := 1; i <= maxResends; i++ {
		sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
		if got := len(rig.commander.prompts(t)); got != 1+i {
			t.Fatalf("reconnect %d: %d prompts sent, want %d", i, got, 1+i)
		}
	}
	for i := 1; i <= 2; i++ {
		sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
		if got := len(rig.commander.prompts(t)); got != 1+maxResends {
			t.Fatalf("reconnect past the cap: %d prompts sent, want %d", got, 1+maxResends)
		}
		if got := countLogLines(t, logs, capReachedMsg); got != i {
			t.Fatalf("%d cap WARN lines after %d reconnects past the cap, want %d", got, i, i)
		}
		if got := promptResendCount(ctx, t, promptResendOutcomeCapReached) - capBefore; got != int64(i) {
			t.Fatalf("turn_prompt_resend_total{cap_reached} moved by %d, want %d", got, i)
		}
	}
	for _, p := range rig.commander.prompts(t) {
		if p.MessageId != messageID {
			t.Fatalf("a re-send carried messageId %q, want %q", p.MessageId, messageID)
		}
	}
	got := rig.turn(ctx, t)
	if got.Status != sqlcgen.TurnStatusProcessing || got.ReceiptResendCount != int32(maxResends) {
		t.Fatalf("turn status %s, receipt_resend_count %d; want processing, %d", got.Status, got.ReceiptResendCount, maxResends)
	}
}

// TestPromptReceipt_PromptFrameOverTheLimit_RefusedNeverSent: a prompt
// whose encoded frame is larger than a sandbox accepts
// (platform.MaxPromptFrameBytes) is never written: its turn fails at
// dispatch as a refusal, with a session warning naming both sizes and a
// synthetic execution_complete saying why. The size measured is the
// encoded frame's: a text of '<' is a sixth of its frame.
func TestPromptReceipt_PromptFrameOverTheLimit_RefusedNeverSent(t *testing.T) {
	ctx := context.Background()
	text := strings.Repeat("<", platform.MaxPromptFrameBytes/6+1024)
	if len(text) >= platform.MaxPromptFrameBytes {
		t.Fatal("the text alone must fit: only its encoding exceeds the limit")
	}
	rig := newReceiptRig(ctx, t, receiptRigOptions{prompt: text})

	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	if got := len(rig.commander.prompts(t)); got != 0 {
		t.Fatalf("%d prompts sent, want 0: a frame over the limit is never written", got)
	}
	if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusFailed {
		t.Fatalf("turn status = %s, want failed", got.Status)
	}
	var reason, warning string
	if err := rig.pool.QueryRow(ctx,
		`SELECT (SELECT payload->>'reason' FROM events WHERE session_id = $1 AND type = 'execution_complete'),
		        (SELECT payload->>'message' FROM events WHERE session_id = $1 AND type = 'warning')`,
		rig.sessionID).Scan(&reason, &warning); err != nil {
		t.Fatalf("read the turn's end: %v", err)
	}
	if !strings.Contains(reason, "larger than the 33554432 bytes a sandbox accepts") {
		t.Fatalf("synthetic execution_complete reason = %q, want the sizes named", reason)
	}
	if !strings.Contains(warning, "32.0 MiB a sandbox accepts") {
		t.Fatalf("session warning = %q, want the limit named for a person", warning)
	}
}

// TestPromptReceipt_ResendOverTheFrameLimit_RefusedNotSent: a re-send
// whose frame would exceed the limit -- which its dispatch's did not, so
// only a row changed since could make it so -- is refused and counted,
// and the turn stays processing.
func TestPromptReceipt_ResendOverTheFrameLimit_RefusedNotSent(t *testing.T) {
	ctx := context.Background()
	rig := newReceiptRig(ctx, t, receiptRigOptions{})
	tooLargeBefore := promptResendCount(ctx, t, promptResendOutcomeFrameTooLarge)

	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	if _, err := rig.pool.Exec(ctx, `UPDATE turns SET prompt = $2 WHERE id = $1`,
		rig.turnID, strings.Repeat("<", platform.MaxPromptFrameBytes/6+1024)); err != nil {
		t.Fatal(err)
	}
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	if got := len(rig.commander.prompts(t)); got != 1 {
		t.Fatalf("%d prompts sent, want only the dispatch", got)
	}
	if got := promptResendCount(ctx, t, promptResendOutcomeFrameTooLarge) - tooLargeBefore; got != 1 {
		t.Fatalf("turn_prompt_resend_total{frame_too_large} moved by %d, want 1", got)
	}
	if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("turn status = %s, want processing", got.Status)
	}
}

// TestPromptReceipt_CrossGenReenqueueToACapableGen_AsksAndIsResent: a turn
// in flight on gen 1 is re-enqueued to a respawned gen 2 whose ready
// advertises the capability: that dispatch asks for a receipt, records the
// request under its new messageId, and gen 2's next reconnect re-sends it
// under that same messageId.
func TestPromptReceipt_CrossGenReenqueueToACapableGen_AsksAndIsResent(t *testing.T) {
	ctx := context.Background()
	rig := newReceiptRig(ctx, t, receiptRigOptions{})
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	first := rig.dispatched(ctx, t)

	// Gen 1 is lost and respawned as gen 2, Ready.
	if _, err := rig.sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: rig.sessionID}); err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if _, err := rig.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: rig.sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	// Gen 2's capable ready: the turn is re-enqueued to it, asking.
	sendAndSettle(ctx, t, rig.actor, receiptReady(2, true), 2)
	prompts := rig.commander.prompts(t)
	if len(prompts) != 2 {
		t.Fatalf("%d prompts sent, want the dispatch and the re-enqueue", len(prompts))
	}
	reenqueued := prompts[1]
	if reenqueued.Gen != 2 || reenqueued.MessageId == first.messageID || !reenqueued.asksReceipt(t) {
		t.Fatalf("re-enqueued prompt = %+v (%s), want gen 2, a new messageId, asking for a receipt", reenqueued.Prompt, reenqueued.raw)
	}
	got := rig.turn(ctx, t)
	if got.ReceiptRequestedMessageID == nil || *got.ReceiptRequestedMessageID != reenqueued.MessageId {
		t.Fatalf("receipt_requested_message_id = %v, want the re-enqueue's %q", got.ReceiptRequestedMessageID, reenqueued.MessageId)
	}

	// Gen 2 reconnects: the re-enqueued prompt is re-sent, same messageId.
	sendAndSettle(ctx, t, rig.actor, receiptReady(2, true), 2)
	prompts = rig.commander.prompts(t)
	if len(prompts) != 3 || prompts[2].MessageId != reenqueued.MessageId || prompts[2].Gen != 2 || !prompts[2].asksReceipt(t) {
		t.Fatalf("after gen 2 reconnects: %d prompts, last %+v; want a third, %q on gen 2, asking", len(prompts), prompts[len(prompts)-1].Prompt, reenqueued.MessageId)
	}
}

// TestPromptReceipt_RolloutRefusesTheResend_NothingSent: a re-send passes
// the turn-dispatch-time rollout re-check: once the session's repo is
// un-enrolled, a same-gen reconnect sends nothing, counts refused, and
// fails nothing.
func TestPromptReceipt_RolloutRefusesTheResend_NothingSent(t *testing.T) {
	ctx := context.Background()
	repo := "acme/zz-resend-refusal-" + uuid.NewString()[:8]
	rig := newReceiptRig(ctx, t, receiptRigOptions{repoFullName: repo})
	refusedBefore := promptResendCount(ctx, t, promptResendOutcomeRefused)
	sentBefore := promptResendCount(ctx, t, promptResendOutcomeSent)

	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	if got := len(rig.commander.prompts(t)); got != 1 {
		t.Fatalf("%d prompts sent, want the dispatch to the enrolled repo", got)
	}
	if _, err := narvipg.NewRepoSettingsStore(rig.pool).UpsertSessionsEnabled(ctx, repo, false); err != nil {
		t.Fatalf("un-enroll the repo: %v", err)
	}
	sendAndSettle(ctx, t, rig.actor, receiptReady(1, true), 1)
	if got := len(rig.commander.prompts(t)); got != 1 {
		t.Fatalf("%d prompts sent, want no re-send to an un-enrolled repo", got)
	}
	if got := promptResendCount(ctx, t, promptResendOutcomeRefused) - refusedBefore; got != 1 {
		t.Fatalf("turn_prompt_resend_total{refused} moved by %d, want 1", got)
	}
	if got := promptResendCount(ctx, t, promptResendOutcomeSent) - sentBefore; got != 0 {
		t.Fatalf("turn_prompt_resend_total{sent} moved by %d, want 0", got)
	}
	if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("turn status = %s, want processing: a refused re-send fails nothing", got.Status)
	}
}
