//go:build integration

// Integration tests pinning the ordering guarantee every `id > cursor`
// reader of the events table depends on: within one session, event ids
// are allocated in commit order (queries/events.sql, CreateEvent).
//
// The readers are the web client's backfill (web/src/ws/sessionStream.ts,
// highestId()), REST ?cursor= pagination (ListEventsForSession) and the
// dispatch high-water mark (MaxEventIDForSession). Each one remembers the
// highest id it has seen and asks only for ids above it, so an event that
// becomes visible AFTER a higher id of the same session has committed is
// skipped by all of them, permanently.
//
// events.id is one global sequence, and a sequence value is handed out
// when the row is formed, not when it commits. The actor serializes its
// own writes on the session row (GetSessionActorEpochForUpdate, FOR
// UPDATE), but writers outside the actor -- httpapi's upload confirm,
// uploadsweep, and the shadow ledger's suppression event -- used to reach
// the session row only through the foreign-key check, which runs after
// the id is drawn. Such a writer drew id N, blocked on the actor's lock,
// and committed after the actor's own later event N+1. These tests hold
// that open window deliberately and assert on which id each side gets,
// or on whether the waiting writer has drawn one yet.
//
// Waiting is always on observed state -- the writer's own backend showing
// an ungranted lock in pg_locks -- never on elapsed time.
package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// lockWaitObservationTimeout bounds how long a test waits to SEE the
// writer blocked in pg_locks. It is a failure deadline, not a pacing
// delay: the poll returns on the first observation, normally within a
// few round trips.
const lockWaitObservationTimeout = 10 * time.Second

// outsideEventWriter is one shape of a non-actor event writer. open
// returns the backend pid the writer's INSERT will run on (so a test can
// find exactly that backend in pg_locks), a create func that performs the
// insert, and a cleanup to run once create has returned.
type outsideEventWriter struct {
	name string
	open func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, connStr string) (pid uint32, create func(ctx context.Context, arg sqlcgen.CreateEventParams) (sqlcgen.CreateEventRow, error), cleanup func())
}

// outsideEventWriters covers both shapes a non-actor writer takes in
// production: an explicit transaction of its own (httpapi's upload
// confirm and uploadsweep: artifact transition + event + outbox), and a
// bare autocommit statement (ShadowSCMWriteStore.AppendSuppressionEvent
// on the pool). The fix lives in the statement itself precisely so the
// autocommit shape is covered without any caller opening a transaction.
func outsideEventWriters() []outsideEventWriter {
	return []outsideEventWriter{
		{
			name: "own transaction",
			open: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, _ string) (uint32, func(context.Context, sqlcgen.CreateEventParams) (sqlcgen.CreateEventRow, error), func()) {
				t.Helper()
				conn, err := pool.Acquire(ctx)
				if err != nil {
					t.Fatalf("acquire writer connection: %v", err)
				}
				create := func(ctx context.Context, arg sqlcgen.CreateEventParams) (sqlcgen.CreateEventRow, error) {
					tx, err := conn.Begin(ctx)
					if err != nil {
						return sqlcgen.CreateEventRow{}, fmt.Errorf("begin writer tx: %w", err)
					}
					defer func() { _ = tx.Rollback(ctx) }()
					row, err := narvipg.NewEventStore(pool).WithTx(tx).Create(ctx, arg)
					if err != nil {
						return sqlcgen.CreateEventRow{}, fmt.Errorf("writer create: %w", err)
					}
					if err := tx.Commit(ctx); err != nil {
						return sqlcgen.CreateEventRow{}, fmt.Errorf("commit writer tx: %w", err)
					}
					return row, nil
				}
				return conn.Conn().PgConn().PID(), create, conn.Release
			},
		},
		{
			name: "autocommit",
			open: func(ctx context.Context, t *testing.T, _ *pgxpool.Pool, connStr string) (uint32, func(context.Context, sqlcgen.CreateEventParams) (sqlcgen.CreateEventRow, error), func()) {
				t.Helper()
				// A one-connection pool, so the store's own autocommit
				// Create is guaranteed to run on the backend whose pid is
				// read here.
				single, err := narvipg.NewPoolWithMaxConns(ctx, connStr, 1)
				if err != nil {
					t.Fatalf("open single-connection writer pool: %v", err)
				}
				conn, err := single.Acquire(ctx)
				if err != nil {
					single.Close()
					t.Fatalf("acquire single writer connection: %v", err)
				}
				pid := conn.Conn().PgConn().PID()
				conn.Release()
				return pid, narvipg.NewEventStore(single).Create, single.Close
			},
		},
	}
}

// startOutsideWrite runs create on g and returns a channel closed when it
// has returned, plus a pointer the inserted row is written to (read it
// only after g.Wait).
func startOutsideWrite(ctx context.Context, g *errgroup.Group, create func(context.Context, sqlcgen.CreateEventParams) (sqlcgen.CreateEventRow, error), arg sqlcgen.CreateEventParams) (<-chan struct{}, *sqlcgen.CreateEventRow) {
	done := make(chan struct{})
	var row sqlcgen.CreateEventRow
	g.Go(func() error {
		defer close(done)
		created, err := create(ctx, arg)
		if err != nil {
			return err
		}
		row = created
		return nil
	})
	return done, &row
}

// waitUntilBackendWaitsOnLock returns once backend pid holds an ungranted
// lock in pg_locks -- i.e. it is blocked behind another transaction. It
// fails the test if the writer finishes first (it never waited, so the
// lock it should have queued on was not taken or did not conflict) or if
// the observation deadline passes.
func waitUntilBackendWaitsOnLock(ctx context.Context, t *testing.T, observer *pgxpool.Pool, pid uint32, writerDone <-chan struct{}) {
	t.Helper()

	waitCtx, cancel := context.WithTimeout(ctx, lockWaitObservationTimeout)
	defer cancel()

	for {
		select {
		case <-writerDone:
			t.Fatalf("writer (backend pid %d) finished without ever waiting on a lock: nothing serialized it behind the open transaction", pid)
		default:
		}
		var waiting bool
		if err := observer.QueryRow(waitCtx,
			`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = $1 AND NOT granted)`, int32(pid),
		).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_locks for writer backend pid %d: %v", pid, err)
		}
		if waiting {
			return
		}
	}
}

// eventsIDSequenceLastValue reads last_value of the sequence behind
// events.id, resolved with pg_get_serial_sequence rather than named. A
// sequence is not transactional, and events.id is a BIGSERIAL (CACHE 1),
// so this is the last id any writer has drawn, committed or not.
func eventsIDSequenceLastValue(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var sequence string
	if err := pool.QueryRow(ctx, `SELECT pg_get_serial_sequence('events', 'id')`).Scan(&sequence); err != nil {
		t.Fatalf("resolve the sequence behind events.id: %v", err)
	}
	var lastValue int64
	if err := pool.QueryRow(ctx, `SELECT last_value FROM `+sequence).Scan(&lastValue); err != nil {
		t.Fatalf("read last_value of %s: %v", sequence, err)
	}
	return lastValue
}

func eventParams(sessionID pgtype.UUID, eventType, messageID string) sqlcgen.CreateEventParams {
	return sqlcgen.CreateEventParams{
		SessionID: sessionID,
		Type:      eventType,
		MessageID: messageID,
		Payload:   []byte(fmt.Sprintf(`{"messageId":%q}`, messageID)),
	}
}

// TestEventStore_CreateAllocatesIDAfterOpenSessionTxCommits: an outside
// writer that arrives while an actor transaction holds the session row
// must not draw its id until that transaction commits, so the actor's
// event (committed first) gets the lower id.
func TestEventStore_CreateAllocatesIDAfterOpenSessionTxCommits(t *testing.T) {
	for _, writer := range outsideEventWriters() {
		t.Run(writer.name, func(t *testing.T) {
			ctx := context.Background()
			pool, connStr := IntegrationTestPoolAndConnStr(t)
			sessionID := createTestSession(ctx, t, pool)
			events := narvipg.NewEventStore(pool)

			// tx1 is an actor transaction: transact's first statement.
			tx1, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin actor tx: %v", err)
			}
			if _, err := narvipg.NewSessionStore(pool).WithTx(tx1).GetActorEpochForUpdate(ctx, sessionID); err != nil {
				_ = tx1.Rollback(ctx)
				t.Fatalf("GetActorEpochForUpdate: %v", err)
			}

			pid, create, cleanup := writer.open(ctx, t, pool, connStr)
			defer cleanup()

			g, gctx := errgroup.WithContext(ctx)
			// Release the session lock before waiting, so a failed
			// assertion below can never leave the writer blocked.
			defer func() {
				_ = tx1.Rollback(ctx)
				_ = g.Wait()
			}()

			writerDone, outside := startOutsideWrite(gctx, g, create, eventParams(sessionID, "artifact", "outside-writer"))
			waitUntilBackendWaitsOnLock(ctx, t, pool, pid, writerDone)

			// The actor appends its own event exactly as appendRawEvent
			// does, then commits.
			actorRow, err := events.WithTx(tx1).Create(ctx, eventParams(sessionID, "token", "actor"))
			if err != nil {
				t.Fatalf("actor create: %v", err)
			}
			if err := tx1.Commit(ctx); err != nil {
				t.Fatalf("commit actor tx: %v", err)
			}

			if err := g.Wait(); err != nil {
				t.Fatalf("outside writer: %v", err)
			}

			if outside.ID <= actorRow.ID {
				t.Fatalf("outside writer's id %d <= actor's id %d, but the actor committed first: a reader between the two "+
					"commits sees %d, advances its cursor past %d, and never sees the outside writer's event",
					outside.ID, actorRow.ID, actorRow.ID, outside.ID)
			}
		})
	}
}

// TestEventStore_CreateSerializesNonActorWriters: two outside writers on
// one session. The first holds its insert in an open transaction (upload
// confirm's shape: more statements follow before commit). The second must
// be observed waiting on a lock while the events id sequence still stands
// at the first writer's id: it queued on the session BEFORE drawing an id.
//
// That, and not "the second gets the higher id", is what this proves: the
// first drew its id before the second started, so the second's id is
// higher whatever the query does. The property matters for a writer that
// draws first and locks after (say, a session-row lock taken after the
// INSERT): it also waits here, but two such writers racing can draw N and
// N+1 and take the lock in the opposite order, so N+1 commits while N is
// still invisible -- the same skip, between two writers neither of which
// is the actor.
func TestEventStore_CreateSerializesNonActorWriters(t *testing.T) {
	for _, writer := range outsideEventWriters() {
		t.Run(writer.name, func(t *testing.T) {
			ctx := context.Background()
			pool, connStr := IntegrationTestPoolAndConnStr(t)
			sessionID := createTestSession(ctx, t, pool)

			tx1, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin first writer tx: %v", err)
			}
			firstRow, err := narvipg.NewEventStore(pool).WithTx(tx1).Create(ctx, eventParams(sessionID, "artifact", "first-writer"))
			if err != nil {
				_ = tx1.Rollback(ctx)
				t.Fatalf("first writer create: %v", err)
			}

			pid, create, cleanup := writer.open(ctx, t, pool, connStr)
			defer cleanup()

			g, gctx := errgroup.WithContext(ctx)
			defer func() {
				_ = tx1.Rollback(ctx)
				_ = g.Wait()
			}()

			writerDone, _ := startOutsideWrite(gctx, g, create, eventParams(sessionID, "shadow_egress_suppressed", "second-writer"))
			waitUntilBackendWaitsOnLock(ctx, t, pool, pid, writerDone)

			// The second writer is blocked and cannot move until tx1
			// commits below, so this read cannot race its nextval.
			if lastValue := eventsIDSequenceLastValue(ctx, t, pool); lastValue != firstRow.ID {
				t.Fatalf("while the second writer waits, the events id sequence stands at %d, not the first writer's id %d: "+
					"the second drew its id before queuing on the session, so two such writers can commit out of id order",
					lastValue, firstRow.ID)
			}

			if err := tx1.Commit(ctx); err != nil {
				t.Fatalf("commit first writer tx: %v", err)
			}
			if err := g.Wait(); err != nil {
				t.Fatalf("second writer: %v", err)
			}
		})
	}
}

// TestEventStore_CreateSemantics pins CreateEvent's contract, which the
// commit-order lock must leave untouched: a fresh messageId inserts
// (Inserted true); a resend of the same messageId on the same session
// returns the already-stored row (Inserted false, same id, stored type and
// payload NOT overwritten); the dedupe key is (session_id, message_id), so
// the same messageId on another session is a fresh row; and a session that
// does not exist is an error, pgx.ErrNoRows -- the locking CTE finds no
// session row, so the INSERT has nothing to insert and RETURNING yields no
// row. (Before the lock this was the foreign-key violation, SQLSTATE
// 23503; no caller distinguishes the two, every one treats any error as
// failure.)
func TestEventStore_CreateSemantics(t *testing.T) {
	type seed struct {
		sameSession bool
		eventType   string
		payload     string
	}
	tests := []struct {
		name          string
		sessionExists bool
		seed          *seed // an earlier Create with the same messageId, or nil
		eventType     string
		payload       string

		wantErr      error
		wantInserted bool
		wantSameID   bool // the returned id is the seed's id
		wantType     string
		wantPayload  string
		wantRows     int // rows stored for the target session afterwards
	}{
		{
			name:          "fresh insert",
			sessionExists: true,
			eventType:     "token",
			payload:       `{"n": 1}`,
			wantInserted:  true,
			wantType:      "token",
			wantPayload:   `{"n": 1}`,
			wantRows:      1,
		},
		{
			name:          "resend of the same messageId is deduped, not overwritten",
			sessionExists: true,
			seed:          &seed{sameSession: true, eventType: "token", payload: `{"n": 1}`},
			eventType:     "tool_call",
			payload:       `{"n": 2}`,
			wantInserted:  false,
			wantSameID:    true,
			wantType:      "token",
			wantPayload:   `{"n": 1}`,
			wantRows:      1,
		},
		{
			name:          "same messageId on another session is a fresh insert",
			sessionExists: true,
			seed:          &seed{sameSession: false, eventType: "token", payload: `{"n": 1}`},
			eventType:     "tool_call",
			payload:       `{"n": 2}`,
			wantInserted:  true,
			wantType:      "tool_call",
			wantPayload:   `{"n": 2}`,
			wantRows:      1,
		},
		{
			name:          "nonexistent session",
			sessionExists: false,
			eventType:     "token",
			payload:       `{"n": 1}`,
			wantErr:       pgx.ErrNoRows,
			wantRows:      0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			events := narvipg.NewEventStore(pool)

			sessionID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
			if tc.sessionExists {
				sessionID = createTestSession(ctx, t, pool)
			}
			const messageID = "msg-under-test"

			var seedRow sqlcgen.CreateEventRow
			if tc.seed != nil {
				seedSession := sessionID
				if !tc.seed.sameSession {
					seedSession = createTestSession(ctx, t, pool)
				}
				var err error
				seedRow, err = events.Create(ctx, sqlcgen.CreateEventParams{
					SessionID: seedSession, Type: tc.seed.eventType, MessageID: messageID, Payload: []byte(tc.seed.payload),
				})
				if err != nil {
					t.Fatalf("seed create: %v", err)
				}
				if !seedRow.Inserted {
					t.Fatalf("seed create: Inserted = false, want true")
				}
			}

			row, err := events.Create(ctx, sqlcgen.CreateEventParams{
				SessionID: sessionID, Type: tc.eventType, MessageID: messageID, Payload: []byte(tc.payload),
			})

			stored, listErr := events.ListForSession(ctx, sessionID, 0, 10)
			if listErr != nil {
				t.Fatalf("ListForSession: %v", listErr)
			}
			if len(stored) != tc.wantRows {
				t.Fatalf("rows stored for the session = %d, want %d", len(stored), tc.wantRows)
			}

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Create error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			if row.Inserted != tc.wantInserted {
				t.Errorf("Inserted = %v, want %v", row.Inserted, tc.wantInserted)
			}
			if tc.wantSameID && row.ID != seedRow.ID {
				t.Errorf("id = %d, want the already-stored row's id %d", row.ID, seedRow.ID)
			}
			if !tc.wantSameID && tc.seed != nil && row.ID == seedRow.ID {
				t.Errorf("id = %d, want a new row, not the seed's", row.ID)
			}
			if row.SessionID != sessionID || row.MessageID != messageID {
				t.Errorf("returned (session, messageId) = (%v, %q), want (%v, %q)", row.SessionID, row.MessageID, sessionID, messageID)
			}

			// Both what Create returned and what is stored.
			for _, got := range []struct {
				source, eventType string
				id                int64
				payload           []byte
			}{
				{"returned", row.Type, row.ID, row.Payload},
				{"stored", stored[0].Type, stored[0].ID, stored[0].Payload},
			} {
				if got.id != row.ID {
					t.Errorf("%s id = %d, want %d", got.source, got.id, row.ID)
				}
				if got.eventType != tc.wantType {
					t.Errorf("%s type = %q, want %q", got.source, got.eventType, tc.wantType)
				}
				assertJSONEqual(t, got.source+" payload", got.payload, tc.wantPayload)
			}
		})
	}
}

func assertJSONEqual(t *testing.T, what string, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: unmarshal %s: %v", what, got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: unmarshal want %s: %v", what, want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}
