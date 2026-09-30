//go:build integration

// This file proves audit fix M1's own deliverApproval staleness recheck
// against a REAL Postgres instance (planSlackNotifier's own PlanStore is a
// concrete *postgres.PlanStore, not an interface -- there is nothing to
// fake here) -- mirrors builder_integration_test.go's own newTestPool/
// direct-seed house style exactly, gated behind the "integration" build
// tag for the same reason (a real *pgxpool.Pool, run via
// `make test-integration`).
package outboxworker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// seedPlanForApprovalTest inserts a session, a plan_mode=true turn, and a
// plans row with the given status directly (bypassing app/sessionactor
// entirely -- mirrors seedOutboxEntry's own "this package's own tests
// exercise Builder [here, planSlackNotifier] in isolation" precedent,
// builder_integration_test.go), returning the created plan row.
func seedPlanForApprovalTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool, status sqlcgen.PlanStatus) sqlcgen.Plan {
	t.Helper()

	var sessionID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id`).Scan(&sessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	turn, err := narvipg.NewTurnStore(pool).Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID,
		Status:    sqlcgen.TurnStatusProcessing,
		PlanMode:  true,
	})
	if err != nil {
		t.Fatalf("seed turn: %v", err)
	}

	plan, err := narvipg.NewPlanStore(pool).Create(ctx, sqlcgen.CreatePlanParams{
		SessionID: sessionID,
		TurnID:    turn.ID,
		Version:   1,
		Status:    status,
	})
	if err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	return plan
}

// planApprovalPayloadFor builds the slack plan-approval outbox payload for
// plan -- the exact shape app/sessionactor/outboxenqueue.go would have
// enqueued back when the plan really was awaiting_approval.
func planApprovalPayloadFor(t *testing.T, plan sqlcgen.Plan) []byte {
	t.Helper()
	payload, err := json.Marshal(slackapi.PlanApprovalPayload{
		PlanID:    plan.ID.String(),
		SessionID: plan.SessionID.String(),
		ChannelID: "C-origin-thread",
		ThreadTS:  "100.001",
		Version:   int(plan.Version),
		Text:      "do the thing",
	})
	if err != nil {
		t.Fatalf("marshal plan approval payload: %v", err)
	}
	return payload
}

// TestPlanSlackNotifier_DeliverApproval_StillAwaitingApproval_PostsMessageAndPersistsRef
// proves the M1 recheck does not interfere with the normal, unaffected
// path: a plan that is genuinely still awaiting_approval at delivery time
// gets its approval-request message posted exactly once, and the real
// channel+ts Slack returns is persisted onto the plans row.
func TestPlanSlackNotifier_DeliverApproval_StillAwaitingApproval_PostsMessageAndPersistsRef(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	plans := narvipg.NewPlanStore(pool)

	plan := seedPlanForApprovalTest(ctx, t, pool, sqlcgen.PlanStatusAwaitingApproval)

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "111.222"})
	}))
	defer server.Close()

	client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")
	notifier := outboxworker.NewPlanSlackNotifier(client, plans)

	err := notifier.Deliver(ctx, ports.Notification{
		Kind:    ports.NotificationKindSlackPlanApproval,
		Payload: planApprovalPayloadFor(t, plan),
	})
	if err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}
	if requests != 1 {
		t.Fatalf("slack API requests = %d, want exactly 1 (message must still be posted)", requests)
	}

	got, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if got.SlackChannelID == nil || *got.SlackChannelID != "C123" {
		t.Errorf("SlackChannelID = %v, want %q", got.SlackChannelID, "C123")
	}
	if got.SlackMessageTs == nil || *got.SlackMessageTs != "111.222" {
		t.Errorf("SlackMessageTs = %v, want %q", got.SlackMessageTs, "111.222")
	}
}

// TestPlanSlackNotifier_DeliverApproval_AlreadyDecided_SkipsPostAndReturnsNil
// is audit fix M1's own core proof: a plan whose status has ALREADY moved
// off awaiting_approval (approved, here) by the time deliverApproval runs
// must NOT get a fresh approval-request message posted -- no Slack API
// call at all -- and Deliver must return nil (a legitimate "no longer
// needed" outcome, never retried/dead-lettered), leaving the plans row's
// own slack_channel_id/slack_message_ts untouched (still nil: nothing was
// ever posted to persist a ref for).
func TestPlanSlackNotifier_DeliverApproval_AlreadyDecided_SkipsPostAndReturnsNil(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	plans := narvipg.NewPlanStore(pool)

	plan := seedPlanForApprovalTest(ctx, t, pool, sqlcgen.PlanStatusApproved)

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "111.222"})
	}))
	defer server.Close()

	client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")
	notifier := outboxworker.NewPlanSlackNotifier(client, plans)

	err := notifier.Deliver(ctx, ports.Notification{
		Kind:    ports.NotificationKindSlackPlanApproval,
		Payload: planApprovalPayloadFor(t, plan),
	})
	if err != nil {
		t.Fatalf("Deliver() error = %v, want nil (a stale, already-decided plan is a no-op, not a failure)", err)
	}
	if requests != 0 {
		t.Fatalf("slack API requests = %d, want 0 (must never post a fresh approval message for an already-decided plan)", requests)
	}

	got, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if got.Status != sqlcgen.PlanStatusApproved {
		t.Fatalf("Status = %q, want unchanged %q", got.Status, sqlcgen.PlanStatusApproved)
	}
	if got.SlackChannelID != nil || got.SlackMessageTs != nil {
		t.Errorf("SlackChannelID/SlackMessageTs = %v/%v, want both nil (no post ever happened)", got.SlackChannelID, got.SlackMessageTs)
	}
}

// TestPlanSlackNotifier_DeliverApproval_RejectedBetweenEnqueueAndDelivery_SkipsStalePost
// is the finding's own race-shaped variant: the outbox row is enqueued
// back when the plan really WAS awaiting_approval (a real
// PlanStore.Create + a real OutboxStore.Create, exactly as
// app/sessionactor/outboxenqueue.go would have done), the plan is THEN
// decided out from under it (PlanStore.RejectIfAwaitingApproval, exactly
// the guarded UPDATE httpapi.DecidePlanOnTx uses for a REST/Slack/Linear
// verdict arriving before this row is ever delivered), and only THEN does
// a full Builder.PumpOnce tick actually attempt delivery -- proving the
// stale post is correctly suppressed (and the outbox row still ends up
// marked 'delivered', not stuck retrying) even though nothing was wrong
// at enqueue time.
func TestPlanSlackNotifier_DeliverApproval_RejectedBetweenEnqueueAndDelivery_SkipsStalePost(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	plans := narvipg.NewPlanStore(pool)
	outbox := narvipg.NewOutboxStore(pool, false)

	plan := seedPlanForApprovalTest(ctx, t, pool, sqlcgen.PlanStatusAwaitingApproval)

	outboxRow, err := outbox.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID: plan.SessionID,
		Kind:      string(ports.NotificationKindSlackPlanApproval),
		Payload:   planApprovalPayloadFor(t, plan),
	})
	if err != nil {
		t.Fatalf("enqueue outbox row: %v", err)
	}

	// Race: some OTHER entry point (REST, Slack interactivity, Linear)
	// decides this plan before the outbox worker ever gets to deliver the
	// approval-request row above -- the exact scenario an arbitrarily
	// delayed outbox delivery (retries/backoff) makes possible.
	var decidedBy pgtype.UUID // Valid == false: a bot/channel-attributed decision, mirroring decidedBy's own NULL-for-bot convention.
	rowsAffected, err := plans.RejectIfAwaitingApproval(ctx, plan.ID, plan.SessionID, decidedBy)
	if err != nil {
		t.Fatalf("reject plan out from under the pending notification: %v", err)
	}
	if rowsAffected != 1 {
		t.Fatalf("RejectIfAwaitingApproval rowsAffected = %d, want 1", rowsAffected)
	}

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "111.222"})
	}))
	defer server.Close()

	client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")
	notifier := outboxworker.NewPlanSlackNotifier(client, plans)

	builder, err := outboxworker.NewBuilder(outbox, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlackPlanApproval: notifier,
	}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if requests != 0 {
		t.Fatalf("slack API requests = %d, want 0 (plan was rejected before delivery ever ran)", requests)
	}

	gotOutbox, err := outbox.Get(ctx, outboxRow.ID)
	if err != nil {
		t.Fatalf("get outbox row: %v", err)
	}
	if gotOutbox.Status != sqlcgen.OutboxStatusDelivered {
		t.Fatalf("outbox row Status = %q, want %q (a suppressed stale post is a successful no-op delivery, not a failure to retry)", gotOutbox.Status, sqlcgen.OutboxStatusDelivered)
	}

	got, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if got.Status != sqlcgen.PlanStatusRejected {
		t.Fatalf("Status = %q, want unchanged %q", got.Status, sqlcgen.PlanStatusRejected)
	}
	if got.SlackChannelID != nil || got.SlackMessageTs != nil {
		t.Errorf("SlackChannelID/SlackMessageTs = %v/%v, want both nil (no post ever happened)", got.SlackChannelID, got.SlackMessageTs)
	}
}

// TestPlanSlackNotifier_Deliver_WorkflowDecision_PostsPlainMessage proves
// §25.9's own addition ("workflow HITL gate + circuit breaker", §25.9):
// ports.NotificationKindSlackWorkflowDecision forwards to n.client.Deliver
// UNCHANGED (planslacknotifier.go's own updated Deliver switch) -- a plain
// slackapi.Payload posts via an ordinary chat.postMessage call, with no
// plan-specific staleness recheck or persisted message ref (there is no
// plan involved at all).
func TestPlanSlackNotifier_Deliver_WorkflowDecision_PostsPlainMessage(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	plans := narvipg.NewPlanStore(pool) // unused by this Kind's own path; a real store is still threaded through, mirroring every other test in this file

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "111.222"})
	}))
	defer server.Close()

	client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")
	notifier := outboxworker.NewPlanSlackNotifier(client, plans)

	payload, err := json.Marshal(slackapi.Payload{ChannelID: "C-decision", ThreadTS: "222.333", Text: "a workflow step needs your decision"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(ctx, ports.Notification{
		Kind:    ports.NotificationKindSlackWorkflowDecision,
		Payload: payload,
	}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}

	if gotBody["channel"] != "C-decision" {
		t.Errorf("posted channel = %v, want %q", gotBody["channel"], "C-decision")
	}
	if gotBody["thread_ts"] != "222.333" {
		t.Errorf("posted thread_ts = %v, want %q", gotBody["thread_ts"], "222.333")
	}
	if gotBody["text"] != "a workflow step needs your decision" {
		t.Errorf("posted text = %v, want %q", gotBody["text"], "a workflow step needs your decision")
	}
}
