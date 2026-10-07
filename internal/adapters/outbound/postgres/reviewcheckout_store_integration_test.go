//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestTurnStore_RecordCheckoutRequest_KeepsTheBoundOnItsGen pins the
// columns a review turn's checkout bound and counts rest on (technical
// plan §21.1, migrations/000167_review_turn_checkout.up.sql), against real
// Postgres:
//
//   - the first request on a gen stamps checkout_requested_at, and every
//     later send on that gen keeps it, so ReviewCheckoutTimeout runs from
//     the first request, never from the latest send, while
//     checkout_sent_at moves with each send;
//   - each send on the gen counts one, and only a send made after a
//     failed reply counts a failure;
//   - a request on another gen starts the bound and both counts again, a
//     failure on the old gen counting nothing on the new one;
//   - a closed turn is written nothing.
//
// Each step reads the row back, the bound backdated an hour first, so a
// bound that restarted reads as one and not as an hour ago.
func TestTurnStore_RecordCheckoutRequest_KeepsTheBoundOnItsGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	turns := narvipg.NewTurnStore(pool)
	created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	id := created.ID

	type row struct {
		gen               int32
		messageID         string
		readySeq          int32
		sends, failures   int32
		sinceRequestHours bool // the bound is the backdated one, an hour old
		sinceSendFresh    bool // the latest send is this one, not an hour old
	}
	read := func(t *testing.T) row {
		t.Helper()
		var r row
		var sinceRequest, sinceSend time.Duration
		if err := pool.QueryRow(ctx, `
			SELECT checkout_gen, checkout_message_id, checkout_sent_ready_seq,
			       COALESCE(checkout_sends, 0), COALESCE(checkout_failures, 0),
			       (EXTRACT(EPOCH FROM now() - checkout_requested_at) * 1e9)::bigint,
			       (EXTRACT(EPOCH FROM now() - checkout_sent_at) * 1e9)::bigint
			FROM turns WHERE id = $1`, id).Scan(&r.gen, &r.messageID, &r.readySeq, &r.sends, &r.failures, &sinceRequest, &sinceSend); err != nil {
			t.Fatalf("read the turn's checkout: %v", err)
		}
		r.sinceRequestHours = sinceRequest >= 59*time.Minute
		r.sinceSendFresh = sinceSend < time.Minute
		return r
	}
	backdate := func(t *testing.T) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE turns SET checkout_requested_at = checkout_requested_at - interval '1 hour',
			checkout_sent_at = checkout_sent_at - interval '1 hour' WHERE id = $1`, id); err != nil {
			t.Fatalf("backdate: %v", err)
		}
	}

	for _, step := range []struct {
		name         string
		gen          int32
		messageID    string
		readySeq     int32
		afterFailure bool
		want         row
	}{
		{name: "the first request on gen 1 starts the bound", gen: 1, messageID: "co-1", readySeq: 1,
			want: row{gen: 1, messageID: "co-1", readySeq: 1, sends: 1, sinceSendFresh: true}},
		{name: "a re-fetch on gen 1 keeps the bound", gen: 1, messageID: "co-2", readySeq: 1,
			want: row{gen: 1, messageID: "co-2", readySeq: 1, sends: 2, sinceRequestHours: true, sinceSendFresh: true}},
		{name: "a send after a failed reply counts it, and keeps the bound", gen: 1, messageID: "co-3", readySeq: 2, afterFailure: true,
			want: row{gen: 1, messageID: "co-3", readySeq: 2, sends: 3, failures: 1, sinceRequestHours: true, sinceSendFresh: true}},
		{name: "a send after a busy reply counts no failure", gen: 1, messageID: "co-4", readySeq: 2,
			want: row{gen: 1, messageID: "co-4", readySeq: 2, sends: 4, failures: 1, sinceRequestHours: true, sinceSendFresh: true}},
		{name: "a new gen starts the bound and the counts again", gen: 2, messageID: "co-5", readySeq: 1, afterFailure: true,
			want: row{gen: 2, messageID: "co-5", readySeq: 1, sends: 1, sinceSendFresh: true}},
		{name: "and keeps its own bound", gen: 2, messageID: "co-6", readySeq: 1, afterFailure: true,
			want: row{gen: 2, messageID: "co-6", readySeq: 1, sends: 2, failures: 1, sinceRequestHours: true, sinceSendFresh: true}},
	} {
		n, err := turns.RecordCheckoutRequest(ctx, id, step.gen, step.messageID, step.readySeq, step.afterFailure)
		if err != nil || n != 1 {
			t.Fatalf("%s: RecordCheckoutRequest wrote %d rows (%v), want 1", step.name, n, err)
		}
		if got := read(t); got != step.want {
			t.Fatalf("%s: checkout = %+v, want %+v", step.name, got, step.want)
		}
		backdate(t)
	}

	// The facts the actor decides on read the same bound.
	state, err := turns.CheckoutState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if since := time.Duration(state.SinceRequestNanos); since < 59*time.Minute {
		t.Fatalf("CheckoutState's since-request = %s, want the gen's first request, an hour ago", since)
	}

	for _, status := range []sqlcgen.TurnStatus{sqlcgen.TurnStatusCancelled, sqlcgen.TurnStatusCompleted, sqlcgen.TurnStatusFailed, sqlcgen.TurnStatusDispatched} {
		if _, err := pool.Exec(ctx, `UPDATE turns SET status = $2 WHERE id = $1`, id, status); err != nil {
			t.Fatal(err)
		}
		if n, err := turns.RecordCheckoutRequest(ctx, id, 3, "co-closed", 1, false); err != nil || n != 0 {
			t.Fatalf("a %s turn: RecordCheckoutRequest wrote %d rows (%v), want 0", status, n, err)
		}
	}
	if got := read(t); got.gen != 2 || got.messageID != "co-6" {
		t.Fatalf("a closed turn's checkout moved to %+v", got)
	}
}
