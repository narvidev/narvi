//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// The queries technical plan §3.3's prompt receipts rest on
// (migrations/000155_prompt_receipts.up.sql), against real Postgres.

// TestSandboxStore_RecordReady_GuardedOnGen: a ready counts, and records
// its capability, only for the sandbox's live gen; the latest ready of the
// gen decides; a gen bump leaves the capability on a gen that is no longer
// live.
func TestSandboxStore_RecordReady_GuardedOnGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	store := narvipg.NewSandboxStore(pool)
	if _, err := store.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	steps := []struct {
		name          string
		gen           int32
		promptReceipt bool
		wantSeq       int32
		wantGen       *int32
	}{
		{name: "another gen's capable ready", gen: 2, promptReceipt: true, wantSeq: 0, wantGen: nil},
		{name: "the live gen's capable ready", gen: 1, promptReceipt: true, wantSeq: 1, wantGen: ptrInt32(1)},
		{name: "the live gen's ready without it", gen: 1, promptReceipt: false, wantSeq: 2, wantGen: nil},
		{name: "capable again", gen: 1, promptReceipt: true, wantSeq: 3, wantGen: ptrInt32(1)},
		{name: "another gen's ready without it", gen: 0, promptReceipt: false, wantSeq: 3, wantGen: ptrInt32(1)},
	}
	for _, step := range steps {
		if err := store.RecordReady(ctx, sessionID, step.gen, step.promptReceipt); err != nil {
			t.Fatalf("%s: RecordReady: %v", step.name, err)
		}
		row, err := store.Get(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if row.ReadySeq != step.wantSeq || !equalInt32Ptr(row.PromptReceiptGen, step.wantGen) {
			t.Fatalf("%s: ready_seq %d, prompt_receipt_gen %v; want %d, %v", step.name, row.ReadySeq, row.PromptReceiptGen, step.wantSeq, step.wantGen)
		}
	}

	// A respawn bumps the gen, and a late ready of the old gen records
	// nothing.
	token := "token-hash-2"
	if _, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &token}); err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if err := store.RecordReady(ctx, sessionID, 1, true); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	row, err := store.Get(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Gen != 2 || row.ReadySeq != 3 || !equalInt32Ptr(row.PromptReceiptGen, ptrInt32(1)) {
		t.Fatalf("after a respawn and a stale ready: gen %d, ready_seq %d, prompt_receipt_gen %v; want 2, 3, 1", row.Gen, row.ReadySeq, row.PromptReceiptGen)
	}
}

func ptrInt32(v int32) *int32 { return &v }

func equalInt32Ptr(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// processingTurnWithRequest creates a turn Processing on gen 1 under
// messageID, whose dispatch asked for a receipt at readySeq.
func processingTurnWithRequest(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, messageID string, readySeq int32) pgtype.UUID {
	t.Helper()
	turns := narvipg.NewTurnStore(pool)
	created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	gen := int32(1)
	if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: created.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedSandboxGen: &gen, DispatchedMessageID: &messageID,
		DispatchedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("dispatch turn: %v", err)
	}
	if err := turns.SetPromptReceiptRequest(ctx, created.ID, &messageID, readySeq); err != nil {
		t.Fatalf("record the request: %v", err)
	}
	return created.ID
}

// TestTurnStore_SetPromptReceiptRequest records the request with its
// instant and the ready it was made at, and a dispatch that asks nothing
// clears all three.
func TestTurnStore_SetPromptReceiptRequest(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	turns := narvipg.NewTurnStore(pool)
	turnID := processingTurnWithRequest(ctx, t, pool, sessionID, "msg-1", 4)

	got, err := turns.Get(ctx, turnID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReceiptRequestedMessageID == nil || *got.ReceiptRequestedMessageID != "msg-1" || !got.ReceiptRequestedAt.Valid ||
		got.ReceiptCheckedReadySeq == nil || *got.ReceiptCheckedReadySeq != 4 {
		t.Fatalf("request = (%v, %v, %v), want (msg-1, set, 4)", got.ReceiptRequestedMessageID, got.ReceiptRequestedAt, got.ReceiptCheckedReadySeq)
	}

	// A re-send counted, then the next dispatch's request starts it over.
	if moved, err := turns.MarkPromptReconnectAnswered(ctx, turnID, "msg-1", 4, 5, 0, true); err != nil || moved != 1 {
		t.Fatalf("claim with a re-send moved %d (%v), want 1", moved, err)
	}
	asked := "msg-1"
	if err := turns.SetPromptReceiptRequest(ctx, turnID, &asked, 5); err != nil {
		t.Fatal(err)
	}
	if got, err = turns.Get(ctx, turnID); err != nil || got.ReceiptResendCount != 0 {
		t.Fatalf("receipt_resend_count after a new request = %d (%v), want 0", got.ReceiptResendCount, err)
	}

	if err := turns.SetPromptReceiptRequest(ctx, turnID, nil, 9); err != nil {
		t.Fatal(err)
	}
	if got, err = turns.Get(ctx, turnID); err != nil {
		t.Fatal(err)
	}
	if got.ReceiptRequestedMessageID != nil || got.ReceiptRequestedAt.Valid || got.ReceiptCheckedReadySeq != nil {
		t.Fatalf("after a dispatch that asked nothing: (%v, %v, %v), want all cleared", got.ReceiptRequestedMessageID, got.ReceiptRequestedAt, got.ReceiptCheckedReadySeq)
	}
}

// TestTurnStore_MarkPromptReconnectAnswered_CAS: exactly one evaluation
// claims a given ready, and only for a Processing, unflagged turn on the
// dispatch that asked, with the re-send count the claim read; a claim
// answered by a re-send counts it.
func TestTurnStore_MarkPromptReconnectAnswered_CAS(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	turns := narvipg.NewTurnStore(pool)

	for _, tc := range []struct {
		name      string
		prepare   func(t *testing.T, turnID pgtype.UUID)
		messageID string
		checked   int32
		readySeq  int32
		resends   int32
		resend    bool
		want      int64
	}{
		{name: "the claim", messageID: "msg", checked: 3, readySeq: 5, want: 1},
		{name: "the claim, answered by a re-send", messageID: "msg", checked: 3, readySeq: 5, resend: true, want: 1},
		{name: "a stale re-send count", messageID: "msg", checked: 3, readySeq: 5, resends: 1, resend: true, want: 0},
		{name: "not a newer ready", messageID: "msg", checked: 3, readySeq: 3, want: 0},
		{name: "a message mismatch", messageID: "msg-other", checked: 3, readySeq: 5, want: 0},
		{name: "a stale checked seq", messageID: "msg", checked: 2, readySeq: 5, want: 0},
		{name: "stop-flagged", messageID: "msg", checked: 3, readySeq: 5, want: 0, prepare: func(t *testing.T, turnID pgtype.UUID) {
			if _, err := pool.Exec(ctx, `UPDATE turns SET stop_requested_at = now() WHERE id = $1`, turnID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "not processing", messageID: "msg", checked: 3, readySeq: 5, want: 0, prepare: func(t *testing.T, turnID pgtype.UUID) {
			if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turnID, Status: sqlcgen.TurnStatusCompleted}); err != nil {
				t.Fatal(err)
			}
		}},
		// The turn's current dispatch, under a new messageId, by a binary
		// without receipts: the request standing is the earlier dispatch's.
		{name: "a stale request of an earlier dispatch", messageID: "msg-redispatched", checked: 3, readySeq: 5, want: 0, prepare: func(t *testing.T, turnID pgtype.UUID) {
			other := "msg-redispatched"
			if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turnID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &other}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turnID := processingTurnWithRequest(ctx, t, pool, sessionID, "msg", 3)
			// Settle the turn whatever happens, so the next case's processing
			// turn is the session's only one (turns_one_processing_per_session).
			t.Cleanup(func() {
				if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turnID, Status: sqlcgen.TurnStatusCompleted}); err != nil {
					t.Error(err)
				}
			})
			if tc.prepare != nil {
				tc.prepare(t, turnID)
			}
			moved, err := turns.MarkPromptReconnectAnswered(ctx, turnID, tc.messageID, tc.checked, tc.readySeq, tc.resends, tc.resend)
			if err != nil {
				t.Fatal(err)
			}
			if moved != tc.want {
				t.Fatalf("rows moved = %d, want %d", moved, tc.want)
			}
			row, err := turns.Get(ctx, turnID)
			if err != nil {
				t.Fatal(err)
			}
			wantCount := int32(0)
			if tc.want == 1 && tc.resend {
				wantCount = 1
			}
			if row.ReceiptResendCount != wantCount {
				t.Fatalf("receipt_resend_count = %d, want %d", row.ReceiptResendCount, wantCount)
			}
			if tc.want == 1 {
				again, err := turns.MarkPromptReconnectAnswered(ctx, turnID, tc.messageID, tc.checked, tc.readySeq, row.ReceiptResendCount, tc.resend)
				if err != nil || again != 0 {
					t.Fatalf("a second claim with the old seq moved %d rows (%v), want 0", again, err)
				}
			}
		})
	}
}

// TestTurnStore_PromptReceiptState_ReadsReceiptStoredByAnyBinary: the
// receipt is the stored event row, found by its deterministic key, so one
// stored through the plain generic insert -- exactly what a binary that
// does not know the type does -- is found.
func TestTurnStore_PromptReceiptState_ReadsReceiptStoredByAnyBinary(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	turns := narvipg.NewTurnStore(pool)
	events := narvipg.NewEventStore(pool)
	turnID := processingTurnWithRequest(ctx, t, pool, sessionID, "msg-1", 1)

	stored, since, ok, err := turns.PromptReceiptState(ctx, turnID)
	if err != nil || !ok || stored || since < 0 || since > time.Minute {
		t.Fatalf("before any receipt: stored %v, since %v, ok %v (%v); want false, a few moments, true", stored, since, ok, err)
	}

	// Another prompt's receipt is not this one's.
	if _, err := events.Create(ctx, sqlcgen.CreateEventParams{SessionID: sessionID, Type: "prompt_received", MessageID: "prompt_received:msg-other",
		Payload: []byte(`{"type":"prompt_received","messageId":"prompt_received:msg-other","sessionId":"s","gen":1,"promptMessageId":"msg-other","duplicate":false}`)}); err != nil {
		t.Fatal(err)
	}
	if stored, _, _, err = turns.PromptReceiptState(ctx, turnID); err != nil || stored {
		t.Fatalf("another prompt's receipt read as this one's (%v)", err)
	}

	if _, err := events.Create(ctx, sqlcgen.CreateEventParams{SessionID: sessionID, Type: "prompt_received", MessageID: "prompt_received:msg-1",
		Payload: []byte(`{"type":"prompt_received","messageId":"prompt_received:msg-1","sessionId":"s","gen":1,"promptMessageId":"msg-1","duplicate":false}`)}); err != nil {
		t.Fatal(err)
	}
	if stored, _, _, err = turns.PromptReceiptState(ctx, turnID); err != nil || !stored {
		t.Fatalf("the receipt stored through the generic insert was not found (%v)", err)
	}

	// A turn whose dispatch asked nothing has no state to read.
	if err := turns.SetPromptReceiptRequest(ctx, turnID, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err = turns.PromptReceiptState(ctx, turnID); err != nil || ok {
		t.Fatalf("a turn that asked nothing reads ok = %v (%v), want false", ok, err)
	}
}
