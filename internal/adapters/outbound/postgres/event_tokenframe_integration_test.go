//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/framecut"
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
// payload's own messageId whatever the storage key is -- the bare part id
// a part's first frame is stored under, or a later frame's per-frame key
// -- the id and text of its first stored frame and the text of its newest
// one, and never a frame of another part, another event type or another
// session.
func TestEventStore_StoredTokenPart(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	events := narvipg.NewEventStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	otherSession := createTestSession(ctx, t, pool)

	// Another session's frame of the same part id, stored first so a lookup
	// that ignored the session would report it as the first frame.
	seedTokenPartEvent(ctx, t, events, otherSession, "token", "prt_a#9", "prt_a", "Other session")
	// A part's first frame: storage key == part id.
	firstA := seedTokenPartEvent(ctx, t, events, sessionID, "token", "prt_a", "prt_a", "")
	seedTokenPartEvent(ctx, t, events, sessionID, "token", "prt_a#2", "prt_a", "Hello")
	firstB := seedTokenPartEvent(ctx, t, events, sessionID, "token", "prt_b#1", "prt_b", "Other part")
	seedTokenPartEvent(ctx, t, events, sessionID, "tool_call", "call-1", "prt_a", "not a token")

	tests := []struct {
		name      string
		partID    string
		want      narvipg.StoredTokenPart
		wantFound bool
	}{
		{name: "first frame and newest text of the part", partID: "prt_a", want: narvipg.StoredTokenPart{FirstFrameID: firstA, FirstText: "", LatestText: "Hello"}, wantFound: true},
		{name: "another part is its own", partID: "prt_b", want: narvipg.StoredTokenPart{FirstFrameID: firstB, FirstText: "Other part", LatestText: "Other part"}, wantFound: true},
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

	// A part stored as its first frame alone -- every part stored before
	// per-frame keys -- is both its first and its newest frame.
	legacy := createTestSession(ctx, t, pool)
	legacyID := seedTokenPartEvent(ctx, t, events, legacy, "token", "prt_old", "prt_old", "first frame only")
	want := narvipg.StoredTokenPart{FirstFrameID: legacyID, FirstText: "first frame only", LatestText: "first frame only"}
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

// seedTokenFrameWithCut stores one `token` frame of partID under storageKey
// whose payload carries cut as its raw `cut` property ("" for none).
func seedTokenFrameWithCut(ctx context.Context, t *testing.T, events *narvipg.EventStore, session pgtype.UUID, storageKey, partID, text, cut string) int64 {
	t.Helper()
	payload := `{"type":"token","messageId":` + mustJSON(t, partID) + `,"text":` + mustJSON(t, text)
	if cut != "" {
		payload += `,"cut":` + cut
	}
	payload += "}"
	row, err := events.Create(ctx, sqlcgen.CreateEventParams{SessionID: session, Type: "token", MessageID: storageKey, Payload: []byte(payload)})
	if err != nil {
		t.Fatalf("seed token %s: %v", storageKey, err)
	}
	return row.ID
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	return string(raw)
}

// TestEventStore_StoredTokenPart_ReadsTheFramesCuts pins the two `cut`
// properties the lookup returns beside the texts (technical plan §6.1):
// the first and the newest stored frame's each, nil for a frame that
// carries none or carries null, the cut itself when well formed, and
// framecut.Malformed -- never nil -- when present but unreadable, so the
// session actor's guard fails closed on it.
func TestEventStore_StoredTokenPart_ReadsTheFramesCuts(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	events := narvipg.NewEventStore(pool)
	sessionID := createTestSession(ctx, t, pool)

	malformed := framecut.Malformed
	tests := []struct {
		name                 string
		firstCut, latestCut  string
		wantFirst, wantLatst *framecut.Cut
	}{
		{name: "no cut on either frame", wantFirst: nil, wantLatst: nil},
		{name: "a whole first frame and a cut newest one", latestCut: `{"kept":5,"total":40}`, wantLatst: &framecut.Cut{Kept: 5, Total: 40}},
		{name: "a cut first frame and a null cut newest one", firstCut: `{"kept":3,"total":9}`, latestCut: `null`, wantFirst: &framecut.Cut{Kept: 3, Total: 9}},
		{name: "a malformed cut is read as malformed", firstCut: `{"kept":"3","total":9}`, latestCut: `[1,2]`, wantFirst: &malformed, wantLatst: &malformed},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			part := fmt.Sprintf("prt_cut_%d", i)
			seedTokenFrameWithCut(ctx, t, events, sessionID, part, part, "first", tt.firstCut)
			seedTokenFrameWithCut(ctx, t, events, sessionID, part+"#2", part, "newest", tt.latestCut)
			got, found, err := events.StoredTokenPart(ctx, sessionID, part)
			if err != nil || !found {
				t.Fatalf("StoredTokenPart: found %v, err %v", found, err)
			}
			if got.FirstText != "first" || got.LatestText != "newest" {
				t.Errorf("texts = (%q, %q), want (first, newest)", got.FirstText, got.LatestText)
			}
			if !reflect.DeepEqual(got.FirstCut, tt.wantFirst) || !reflect.DeepEqual(got.LatestCut, tt.wantLatst) {
				t.Errorf("cuts = (%+v, %+v), want (%+v, %+v)", got.FirstCut, got.LatestCut, tt.wantFirst, tt.wantLatst)
			}
		})
	}
}

// TestEventStore_ListTokenFramesInWindow pins the decision inbox's read of
// one turn's text (sessionactor.ReadPlanFinal): the session's `token`
// frames with id above lower and, when upper is set, at or below it --
// FinalText's own bounds -- newest first, capped at limit, and never
// another event type or another session's frame.
func TestEventStore_ListTokenFramesInWindow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	events := narvipg.NewEventStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	otherSession := createTestSession(ctx, t, pool)

	before := seedTokenPartEvent(ctx, t, events, sessionID, "token", "prt_before", "prt_before", "an earlier turn")
	first := seedTokenPartEvent(ctx, t, events, sessionID, "token", "prt_a", "prt_a", "")
	seedTokenPartEvent(ctx, t, events, sessionID, "tool_call", "call-1", "prt_a", "not a token")
	seedTokenPartEvent(ctx, t, events, otherSession, "token", "prt_other", "prt_other", "another session")
	last := seedTokenPartEvent(ctx, t, events, sessionID, "token", "prt_a#2", "prt_a", "the text")
	after := seedTokenPartEvent(ctx, t, events, sessionID, "token", "prt_later", "prt_later", "a later turn")

	ids := func(rows []sqlcgen.Event) []int64 {
		out := make([]int64, len(rows))
		for i, r := range rows {
			out[i] = r.ID
		}
		return out
	}
	upper := last
	tests := []struct {
		name  string
		lower int64
		upper *int64
		limit int32
		want  []int64
	}{
		{name: "bounded above, the upper bound included", lower: before, upper: &upper, limit: 10, want: []int64{last, first}},
		{name: "unbounded above", lower: before, upper: nil, limit: 10, want: []int64{after, last, first}},
		{name: "the lower bound excluded", lower: first, upper: &upper, limit: 10, want: []int64{last}},
		{name: "capped, newest first", lower: before, upper: nil, limit: 2, want: []int64{after, last}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := events.ListTokenFramesInWindow(ctx, sessionID, tt.lower, tt.upper, tt.limit)
			if err != nil {
				t.Fatalf("ListTokenFramesInWindow: %v", err)
			}
			if got := ids(rows); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ids = %v, want %v", got, tt.want)
			}
		})
	}
}
