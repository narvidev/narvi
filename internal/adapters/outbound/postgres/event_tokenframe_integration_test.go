//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// seedTokenPartEvent stores one event whose payload names partID as its
// messageId, under storageKey, and returns its id.
func seedTokenPartEvent(ctx context.Context, t *testing.T, events *narvipg.EventStore, session pgtype.UUID, eventType, storageKey, partID, text string) int64 {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"type": eventType, "messageId": partID, "text": text})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	row, err := events.Create(ctx, sqlcgen.CreateEventParams{
		SessionID: session, Type: eventType, MessageID: storageKey, Payload: payload,
	})
	if err != nil {
		t.Fatalf("seed %s %s: %v", eventType, storageKey, err)
	}
	return row.ID
}

// TestEventStore_StoredTokenPart pins the lookup the session actor makes
// before storing a streamed `token` frame: for ONE part, matched by the
// payload's own messageId whatever the storage key is -- a per-frame key
// or, for frames stored before per-frame keys existed, the bare part id --
// the id of its first stored frame and the text of its newest one, and
// never a frame of another part, another event type or another session.
func TestEventStore_StoredTokenPart(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	events := narvipg.NewEventStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	otherSession := createTestSession(ctx, t, pool)

	// Another session's frame of the same part id, stored first so a lookup
	// that ignored the session would report it as the first frame.
	seedTokenPartEvent(ctx, t, events, otherSession, "token", "prt_a#9", "prt_a", "Other session")
	// A frame stored before per-frame keys: storage key == part id.
	legacyA := seedTokenPartEvent(ctx, t, events, sessionID, "token", "prt_a", "prt_a", "")
	seedTokenPartEvent(ctx, t, events, sessionID, "token", "prt_a#2", "prt_a", "Hello")
	firstB := seedTokenPartEvent(ctx, t, events, sessionID, "token", "prt_b#1", "prt_b", "Other part")
	seedTokenPartEvent(ctx, t, events, sessionID, "tool_call", "call-1", "prt_a", "not a token")

	tests := []struct {
		name      string
		partID    string
		want      narvipg.StoredTokenPart
		wantFound bool
	}{
		{name: "first frame and newest text of the part", partID: "prt_a", want: narvipg.StoredTokenPart{FirstFrameID: legacyA, LatestText: "Hello"}, wantFound: true},
		{name: "another part is its own", partID: "prt_b", want: narvipg.StoredTokenPart{FirstFrameID: firstB, LatestText: "Other part"}, wantFound: true},
		{name: "a part with no stored frame", partID: "prt_none", wantFound: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found, err := events.StoredTokenPart(ctx, sessionID, tt.partID)
			if err != nil {
				t.Fatalf("StoredTokenPart: %v", err)
			}
			if found != tt.wantFound || got != tt.want {
				t.Errorf("StoredTokenPart(%q) = (%+v, %v), want (%+v, %v)", tt.partID, got, found, tt.want, tt.wantFound)
			}
		})
	}

	// The legacy row alone is found too.
	legacy := createTestSession(ctx, t, pool)
	legacyID := seedTokenPartEvent(ctx, t, events, legacy, "token", "prt_old", "prt_old", "first frame only")
	want := narvipg.StoredTokenPart{FirstFrameID: legacyID, LatestText: "first frame only"}
	if got, found, err := events.StoredTokenPart(ctx, legacy, "prt_old"); err != nil || !found || got != want {
		t.Errorf("legacy frame: StoredTokenPart = (%+v, %v, %v), want (%+v, true, nil)", got, found, err, want)
	}
}

// queryRecorder is a pgx.QueryTracer that records the SQL text and
// arguments of every query sent on the pool it is installed on.
type queryRecorder struct {
	mu      sync.Mutex
	queries []pgx.TraceQueryStartData
}

func (r *queryRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, data)
	return ctx
}

func (*queryRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// explainNode is one node of an EXPLAIN (FORMAT JSON) plan tree, as far as
// the index assertions below need it.
type explainNode struct {
	NodeType      string        `json:"Node Type"`
	RelationName  string        `json:"Relation Name"`
	IndexName     string        `json:"Index Name"`
	IndexCond     string        `json:"Index Cond"`
	ScanDirection string        `json:"Scan Direction"`
	Plans         []explainNode `json:"Plans"`
}

// walk calls visit on n and every node below it.
func (n explainNode) walk(visit func(explainNode)) {
	visit(n)
	for _, child := range n.Plans {
		child.walk(visit)
	}
}

// TestEventStore_StoredTokenPart_UsesPartIndex proves events_token_part_idx
// serves the lookup the session actor runs on every `token` frame -- the
// SQL EventStore.StoredTokenPart actually sends, captured on the wire, not
// a copy of it: both of its scans (the newest frame, backward; the first
// frame, forward) are index scans on events_token_part_idx whose index
// condition carries the payload's messageId, and nothing sorts. A query
// edit that stops matching the index's expression or its partial
// predicate keeps returning the right rows, only at the cost of a scan
// per frame, so only a plan check can see it.
func TestEventStore_StoredTokenPart_UsesPartIndex(t *testing.T) {
	ctx := context.Background()
	pool, connStr := IntegrationTestPoolAndConnStr(t)
	sessionID := createTestSession(ctx, t, pool)
	otherSession := createTestSession(ctx, t, pool)

	// Enough rows, across parts, event types and sessions, that a plan is
	// a real choice, then fresh statistics.
	events := narvipg.NewEventStore(pool)
	for i := 0; i < 400; i++ {
		part := fmt.Sprintf("prt_%d", i%40)
		seedTokenPartEvent(ctx, t, events, sessionID, "token", fmt.Sprintf("%s#%d", part, i), part, strings.Repeat("x", i%7))
		seedTokenPartEvent(ctx, t, events, otherSession, "token", fmt.Sprintf("%s#%d", part, i), part, "other session")
		seedTokenPartEvent(ctx, t, events, sessionID, "tool_call", fmt.Sprintf("call-%d", i), part, "not a token")
	}
	if _, err := pool.Exec(ctx, "ANALYZE events"); err != nil {
		t.Fatalf("ANALYZE events: %v", err)
	}

	// The query the store sends, recorded by a tracer on a pool of its own.
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	recorder := &queryRecorder{}
	cfg.ConnConfig.Tracer = recorder
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	defer traced.Close()
	if _, found, err := narvipg.NewEventStore(traced).StoredTokenPart(ctx, sessionID, "prt_7"); err != nil || !found {
		t.Fatalf("StoredTokenPart on the traced pool = (found %v, err %v), want a stored part", found, err)
	}
	recorder.mu.Lock()
	sent := append([]pgx.TraceQueryStartData(nil), recorder.queries...)
	recorder.mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("StoredTokenPart sent %d queries, want 1", len(sent))
	}
	if !strings.HasPrefix(sent[0].SQL, "-- name: GetLatestTokenFrameForPart :one") {
		t.Fatalf("StoredTokenPart sent %q, want the sqlc-generated GetLatestTokenFrameForPart", sent[0].SQL)
	}

	// enable_seqscan=off, as in TestListDueTimers_UsesFiresAtIndex, so a
	// test-sized table does not make a sequential scan the cheaper choice.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatalf("SET LOCAL enable_seqscan = off: %v", err)
	}
	var planJSON []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sent[0].SQL, sent[0].Args...).Scan(&planJSON); err != nil {
		t.Fatalf("EXPLAIN the sent query: %v", err)
	}
	var plans []struct {
		Plan explainNode `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode EXPLAIN output (%v): %s", err, planJSON)
	}

	var scans []explainNode
	sorts := 0
	plans[0].Plan.walk(func(n explainNode) {
		if n.RelationName == "events" {
			scans = append(scans, n)
		}
		if strings.Contains(n.NodeType, "Sort") {
			sorts++
		}
	})
	if len(scans) != 2 {
		t.Fatalf("plan scans events %d times, want 2 (newest frame, first frame); plan: %s", len(scans), planJSON)
	}
	directions := map[string]bool{}
	for _, scan := range scans {
		if scan.NodeType != "Index Scan" || scan.IndexName != "events_token_part_idx" {
			t.Errorf("events scanned by %q on %q, want an Index Scan on events_token_part_idx; plan: %s", scan.NodeType, scan.IndexName, planJSON)
		}
		if !strings.Contains(scan.IndexCond, "session_id") || !strings.Contains(scan.IndexCond, "payload ->> 'messageId'") {
			t.Errorf("index condition %q does not carry both session_id and the payload's messageId; plan: %s", scan.IndexCond, planJSON)
		}
		directions[scan.ScanDirection] = true
	}
	if !directions["Backward"] || !directions["Forward"] {
		t.Errorf("scan directions = %v, want one Backward (newest frame) and one Forward (first frame); plan: %s", directions, planJSON)
	}
	if sorts != 0 {
		t.Errorf("plan sorts %d times, want none (the index's trailing id column orders both scans); plan: %s", sorts, planJSON)
	}
}
