//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

// messageIDLookupMaxBuffers bounds the buffers the lookup of one event by
// its storage key reads: a descent of events_session_id_message_id_idx and
// the event's heap page -- three or four on these logs -- with room to
// spare. A lookup that reads a session's entries reads hundreds.
const messageIDLookupMaxBuffers = 8

// TestEventStore_IDForMessageID_OneIndexProbe pins the read the session
// actor makes before storing a tool_call, tool_result or step_finish
// (sessionactor/toolevent.go): the id of the row under an assistant
// message's bare id, GetEventIDByMessageID. It must be one probe of
// events_session_id_message_id_idx, the unique index on (session_id,
// message_id), however long the session and whatever the planner expects
// of it, under a custom plan and the generic plan pgx's statement cache
// lets Postgres settle on from a statement's sixth run, for a key that is
// stored and for one that is not. It reads the logs the page walk is
// pinned on that turn the planner's expectation most: many small sessions
// beside one long one, and a long one stored after the table was analyzed.
func TestEventStore_IDForMessageID_OneIndexProbe(t *testing.T) {
	for _, log := range []pageWalkLog{
		{name: "200 sessions beside one of 50,000 events", others: 200, otherEvents: 100_000, long: 50_000},
		{name: "5,000 sessions of 8 events beside one of 20,000", others: 5_000, otherEvents: 40_000, long: 20_000},
		{name: "4,000 sessions of 25 events, and one of 8,000 stored after ANALYZE", others: 4_000, otherEvents: 100_000, long: 8_000, longAfterAnalyze: true},
	} {
		t.Run(log.name, func(t *testing.T) {
			ctx := context.Background()
			pool := pagePlanDatabase(ctx, t)
			long, middle, fresh := storePageWalkLog(ctx, t, pool, log)
			events := narvipg.NewEventStore(pool)

			var middleKey string
			if err := pool.QueryRow(ctx, `SELECT message_id FROM events WHERE id = $1`, middle).Scan(&middleKey); err != nil {
				t.Fatalf("read the long session's middle key: %v", err)
			}
			for _, read := range []struct {
				name      string
				sessionID string
				key       string
				wantID    int64
			}{
				{name: "a key of the long session", sessionID: long.String(), key: middleKey, wantID: middle},
				{name: "a key the long session does not hold", sessionID: long.String(), key: "msg_absent"},
				{name: "a key of a new session", sessionID: fresh.String(), key: "fresh-150"},
			} {
				sessionID := long
				if read.sessionID == fresh.String() {
					sessionID = fresh
				}
				// Run past five times, so the statement is prepared on the
				// pool's one connection and its cache may settle on a
				// generic plan.
				var id int64
				var found bool
				for i := 0; i < 8; i++ {
					var err error
					id, found, err = events.IDForMessageID(ctx, sessionID, read.key)
					if err != nil {
						t.Fatalf("%s: IDForMessageID: %v", read.name, err)
					}
				}
				if read.wantID != 0 && (!found || id != read.wantID) {
					t.Fatalf("%s: IDForMessageID = %d, %v; want %d", read.name, id, found, read.wantID)
				}
				if read.key == "msg_absent" && found {
					t.Fatalf("%s: IDForMessageID found %d, want nothing", read.name, id)
				}

				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					if _, err := pool.Exec(ctx, "SET plan_cache_mode = "+mode); err != nil {
						t.Fatalf("set plan_cache_mode: %v", err)
					}
					scans := explainPageStatement(ctx, t, pool, "GetEventIDByMessageID", fmt.Sprintf("'%s'::uuid, '%s'", read.sessionID, read.key))
					t.Logf("%s, %s: %s", read.name, mode, scans)
					if len(scans) != 1 || scans[0].Index != "events_session_id_message_id_idx" || scans[0].Loops != 1 {
						t.Errorf("%s, %s: scans %v, want one probe of events_session_id_message_id_idx", read.name, mode, scans)
						continue
					}
					if scans[0].Buffers > messageIDLookupMaxBuffers || scans[0].RowsRemoved != 0 {
						t.Errorf("%s, %s: %s; want at most %d buffers and no row filtered out", read.name, mode, scans[0], messageIDLookupMaxBuffers)
					}
				}
				if _, err := pool.Exec(ctx, `RESET plan_cache_mode`); err != nil {
					t.Fatalf("reset plan_cache_mode: %v", err)
				}
			}
		})
	}
}
