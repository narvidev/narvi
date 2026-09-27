//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestEventStore_LatestTokenFrameText pins the lookup the session actor
// makes before storing a streamed `token` frame: the newest frame (by id)
// of ONE part, matched by the payload's own messageId whatever the storage
// key is -- a per-frame key or, for frames stored before per-frame keys
// existed, the bare part id -- and never a frame of another part, another
// event type or another session.
func TestEventStore_LatestTokenFrameText(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	events := narvipg.NewEventStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	otherSession := createTestSession(ctx, t, pool)

	seed := func(session pgtype.UUID, eventType, storageKey, partID, text string) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"type": eventType, "messageId": partID, "text": text})
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		if _, err := events.Create(ctx, sqlcgen.CreateEventParams{
			SessionID: session, Type: eventType, MessageID: storageKey, Payload: payload,
		}); err != nil {
			t.Fatalf("seed %s %s: %v", eventType, storageKey, err)
		}
	}

	// A frame stored before per-frame keys: storage key == part id.
	seed(sessionID, "token", "prt_a", "prt_a", "")
	seed(sessionID, "token", "prt_a#2", "prt_a", "Hello")
	seed(sessionID, "token", "prt_b#1", "prt_b", "Other part")
	seed(sessionID, "tool_call", "call-1", "prt_a", "not a token")
	seed(otherSession, "token", "prt_a#9", "prt_a", "Other session")

	tests := []struct {
		name      string
		partID    string
		wantText  string
		wantFound bool
	}{
		{name: "newest frame of the part wins", partID: "prt_a", wantText: "Hello", wantFound: true},
		{name: "another part is its own", partID: "prt_b", wantText: "Other part", wantFound: true},
		{name: "a part with no stored frame", partID: "prt_none", wantFound: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, found, err := events.LatestTokenFrameText(ctx, sessionID, tt.partID)
			if err != nil {
				t.Fatalf("LatestTokenFrameText: %v", err)
			}
			if found != tt.wantFound || text != tt.wantText {
				t.Errorf("LatestTokenFrameText(%q) = (%q, %v), want (%q, %v)", tt.partID, text, found, tt.wantText, tt.wantFound)
			}
		})
	}

	// The legacy row alone is found too.
	legacy := createTestSession(ctx, t, pool)
	seed(legacy, "token", "prt_old", "prt_old", "first frame only")
	if text, found, err := events.LatestTokenFrameText(ctx, legacy, "prt_old"); err != nil || !found || text != "first frame only" {
		t.Errorf("legacy frame: LatestTokenFrameText = (%q, %v, %v), want (\"first frame only\", true, nil)", text, found, err)
	}

	// events_token_part_idx is a usable plan for the query's exact shape --
	// enable_seqscan=off, as in TestListDueTimers_UsesFiresAtIndex, so the
	// tiny test table does not make a sequential scan the cheaper choice.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatalf("SET LOCAL enable_seqscan = off: %v", err)
	}
	rows, err := tx.Query(ctx, `
		EXPLAIN
		SELECT id, COALESCE(payload->>'text', '')::text AS text
		FROM events
		WHERE session_id = $1
		  AND type = 'token'
		  AND payload->>'messageId' = $2::text
		ORDER BY id DESC
		LIMIT 1
	`, sessionID, "prt_a")
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN line: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	if !strings.Contains(plan.String(), "events_token_part_idx") {
		t.Fatalf("plan does not use events_token_part_idx; plan:\n%s", plan.String())
	}
}
