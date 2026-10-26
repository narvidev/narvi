//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// This file pins the record technical plan §35.5's restore instrumentation
// reads (migrations/000176): the statement that records a snapshot records
// the conversation the session held at that moment, keyed by the
// snapshot's id, and the statement that clears the snapshot clears it.

// conversationRecord is the snapshot-conversation part of a sandbox row,
// "NULL" for a NULL column.
type conversationRecord struct{ snapshotID, conversation, recordedID string }

func conversationRecordOf(row sqlcgen.Sandbox) conversationRecord {
	str := func(p *string) string {
		if p == nil {
			return "NULL"
		}
		return *p
	}
	return conversationRecord{snapshotID: str(row.SnapshotID), conversation: str(row.SnapshotConversationID), recordedID: str(row.SnapshotConversationRecordedID)}
}

// setSessionConversation sets sessionID's recorded conversation, nil for
// none, through the statement a heartbeat's report runs.
func setSessionConversation(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, conversation *string) {
	t.Helper()
	if conversation == nil {
		if _, err := pool.Exec(ctx, `UPDATE sessions SET opencode_conversation_id = NULL WHERE id = $1`, sessionID); err != nil {
			t.Fatalf("clear the session's conversation: %v", err)
		}
		return
	}
	if _, err := narvipg.NewSessionStore(pool).UpdateConversationID(ctx, sqlcgen.UpdateSessionConversationIDParams{
		ID: sessionID, OpencodeConversationID: conversation,
	}); err != nil {
		t.Fatalf("record the session's conversation: %v", err)
	}
}

// TestUpdateSandboxSnapshotID_RecordsTheConversationKeyedToTheSnapshot: each
// mint records the conversation its own session holds as the statement
// runs -- never another session's -- keyed by the snapshot it records; a
// mint under no conversation records none rather than keeping the last
// one; and a spawn, restore or resume claim leaves the record as it is.
func TestUpdateSandboxSnapshotID_RecordsTheConversationKeyedToTheSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	if _, err := store.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	// Another session, with a sandbox and a conversation of its own, that
	// no mint of sessionID's may read.
	otherID := createTestSession(ctx, t, pool)
	if _, err := store.Create(ctx, otherID); err != nil {
		t.Fatalf("create the other sandbox: %v", err)
	}
	other := "conv-other-session"
	setSessionConversation(ctx, t, pool, otherID, &other)

	mint := func(snapshotID string) conversationRecord {
		t.Helper()
		row, err := store.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{SessionID: sessionID, SnapshotID: &snapshotID})
		if err != nil {
			t.Fatalf("UpdateSnapshotID(%s): %v", snapshotID, err)
		}
		return conversationRecordOf(row)
	}

	conv1 := "conv-1"
	setSessionConversation(ctx, t, pool, sessionID, &conv1)
	if got, want := mint("snap-1"), (conversationRecord{"snap-1", "conv-1", "snap-1"}); got != want {
		t.Fatalf("after a mint under conv-1: %+v, want %+v", got, want)
	}

	// A claim keeps the record: a restored gen still describes its snapshot.
	tokenHash := "token-hash-conversation"
	claimed, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &tokenHash, LifetimeSeconds: ptrInt32(7200)})
	if err != nil {
		t.Fatalf("UpsertForSpawn: %v", err)
	}
	if got, want := conversationRecordOf(claimed), (conversationRecord{"snap-1", "conv-1", "snap-1"}); got != want {
		t.Fatalf("after a claim: %+v, want %+v, kept", got, want)
	}

	// A mint under no conversation records none, keyed to its own id.
	setSessionConversation(ctx, t, pool, sessionID, nil)
	if got, want := mint("snap-2"), (conversationRecord{"snap-2", "NULL", "snap-2"}); got != want {
		t.Fatalf("after a mint under no conversation: %+v, want %+v", got, want)
	}

	// The conversation the session holds when the statement runs.
	conv2 := "conv-2"
	setSessionConversation(ctx, t, pool, sessionID, &conv2)
	if got, want := mint("snap-3"), (conversationRecord{"snap-3", "conv-2", "snap-3"}); got != want {
		t.Fatalf("after a mint under conv-2: %+v, want %+v", got, want)
	}

	otherRow, err := store.Get(ctx, otherID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := conversationRecordOf(otherRow), (conversationRecord{"NULL", "NULL", "NULL"}); got != want {
		t.Fatalf("the other session's sandbox after sessionID's mints: %+v, want %+v, untouched", got, want)
	}
}

// TestClearSandboxSnapshot_ClearsTheConversationRecord: the clear that
// forgets a snapshot forgets the conversation recorded for it, and a clear
// guarded on another snapshot leaves both.
func TestClearSandboxSnapshot_ClearsTheConversationRecord(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	if _, err := store.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	conversation, snapshotID := "conv-cleared", "snap-cleared"
	setSessionConversation(ctx, t, pool, sessionID, &conversation)
	if _, err := store.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{SessionID: sessionID, SnapshotID: &snapshotID}); err != nil {
		t.Fatalf("seed the snapshot: %v", err)
	}
	get := func() conversationRecord {
		t.Helper()
		row, err := store.Get(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		return conversationRecordOf(row)
	}
	seeded := conversationRecord{snapshotID, conversation, snapshotID}
	if got := get(); got != seeded {
		t.Fatalf("seeded %+v, want %+v", got, seeded)
	}

	if n, err := store.ClearSnapshot(ctx, sessionID, "snap-decided-on-earlier"); err != nil || n != 0 {
		t.Fatalf("ClearSnapshot of another snapshot = (%d, %v), want (0, nil)", n, err)
	}
	if got := get(); got != seeded {
		t.Fatalf("after clearing another snapshot: %+v, want %+v, kept", got, seeded)
	}

	if n, err := store.ClearSnapshot(ctx, sessionID, snapshotID); err != nil || n != 1 {
		t.Fatalf("ClearSnapshot of the snapshot = (%d, %v), want (1, nil)", n, err)
	}
	if got, want := get(), (conversationRecord{"NULL", "NULL", "NULL"}); got != want {
		t.Fatalf("after the clear: %+v, want %+v", got, want)
	}
}
