//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestEventStore_SandboxStatusAge pins the read the session actor logs a
// held turn's snapshot age from (queries/events.sql, GetSandboxStatusAge):
// the session's latest sandbox_status event decides, and only when it
// reports the gen and status asked is the age known, counted on the
// database's clock from when it was stored. A later event of another type
// hides nothing; no event at all is unknown, not an error; and no payload,
// however malformed, fails the read -- it runs inside a dispatch
// evaluation's transaction, which a failed statement would abort.
func TestEventStore_SandboxStatusAge(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	const (
		readyGen1        = `{"sandbox": {"gen": 1, "status": "ready"}}`
		snapshottingGen1 = `{"sandbox": {"gen": 1, "status": "snapshotting"}}`
	)
	type stored struct {
		eventType, payload, ago string // ago: how long before now() it was stored
	}
	snapshotting := sqlcgen.SandboxStatusSnapshotting
	tests := []struct {
		name        string
		events      []stored // oldest first
		gen         int32
		wantKnown   bool
		wantSeconds int64 // the age, to the second, when known
	}{
		{name: "none stored", gen: 1},
		{name: "the latest reports it", events: []stored{
			{"sandbox_status", readyGen1, "20 minutes"},
			{"sandbox_status", snapshottingGen1, "10 minutes"},
		}, gen: 1, wantKnown: true, wantSeconds: 600},
		{name: "a later event of another type hides nothing", events: []stored{
			{"sandbox_status", snapshottingGen1, "10 minutes"},
			{"heartbeat", `{"gen": 1}`, "1 minute"},
		}, gen: 1, wantKnown: true, wantSeconds: 600},
		{name: "the latest reports another status", events: []stored{
			{"sandbox_status", snapshottingGen1, "20 minutes"},
			{"sandbox_status", readyGen1, "10 minutes"},
		}, gen: 1},
		{name: "the latest reports another gen", events: []stored{
			{"sandbox_status", snapshottingGen1, "10 minutes"},
		}, gen: 2},
		{name: "a gen that is not a number", events: []stored{
			{"sandbox_status", `{"sandbox": {"gen": "one", "status": "snapshotting"}}`, "10 minutes"},
		}, gen: 1},
		{name: "a sandbox that is not an object", events: []stored{
			{"sandbox_status", `{"sandbox": "snapshotting"}`, "10 minutes"},
		}, gen: 1},
		{name: "an empty payload", events: []stored{
			{"sandbox_status", `{}`, "10 minutes"},
		}, gen: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			for i, e := range tc.events {
				if _, err := pool.Exec(ctx,
					`INSERT INTO events (session_id, type, payload, message_id, created_at) VALUES ($1, $2, $3::jsonb, $4, now() - $5::interval)`,
					sessionID, e.eventType, e.payload, fmt.Sprintf("status-age-%d", i), e.ago); err != nil {
					t.Fatalf("store event %d: %v", i, err)
				}
			}
			seconds, known, err := narvipg.NewEventStore(pool).SandboxStatusAge(ctx, sessionID, tc.gen, snapshotting)
			if err != nil {
				t.Fatalf("SandboxStatusAge: %v", err)
			}
			if known != tc.wantKnown {
				t.Fatalf("SandboxStatusAge known = %v (%d s), want %v", known, seconds, tc.wantKnown)
			}
			if known && (seconds < tc.wantSeconds || seconds > tc.wantSeconds+30) {
				t.Errorf("SandboxStatusAge = %d s, want %d s (within the test's own run time)", seconds, tc.wantSeconds)
			}
		})
	}
}
