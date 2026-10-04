//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

// This file pins the statements technical plan §24.9's context check of a
// queued automatic review attempt reads by, on real Postgres: the pre-read
// of the turn a dispatch evaluation is about to pick, and the order of a
// session's turns the pick is made from.

// insertTurnAt inserts a turn of sessionID in status, created secsAgo
// seconds before now() and, when endedSecsAgo is non-nil, completed that
// many seconds before now(); it returns the turn's id.
func insertTurnAt(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, status string, secsAgo int, endedSecsAgo *int, reviewAttempt bool, trigger *string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO turns (session_id, status, created_at, completed_at, is_review_attempt, request_trigger, review_head_sha)
		VALUES ($1, $2::turn_status, now() - make_interval(secs => $3::int),
		        CASE WHEN $4::int IS NULL THEN NULL ELSE now() - make_interval(secs => $4::int) END,
		        $5, $6, CASE WHEN $5 THEN 'sha-recorded' END)
		RETURNING id`, sessionID, status, secsAgo, endedSecsAgo, reviewAttempt, trigger).Scan(&id); err != nil {
		t.Fatalf("insert a %s turn: %v", status, err)
	}
	return id
}

func secs(n int) *int { return &n }

// TestReviewAttemptToCheck_ReadsThePickTheDispatchMakes pins
// GetReviewAttemptToCheck's answer: the session's oldest pending turn no
// stop flagged, when none is in flight; whether it waited behind a turn
// created before it that is still open or ended after it was created; and
// whether the sandbox can take it now.
func TestReviewAttemptToCheck_ReadsThePickTheDispatchMakes(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	turns := narvipg.NewTurnStore(pool)
	auto, label := "auto", "label"

	for _, tc := range []struct {
		name string
		// seed stores the session's turns and returns the pick it expects,
		// invalid when it expects no row.
		seed        func(sessionID pgtype.UUID) pgtype.UUID
		sandbox     string
		wantQueued  bool
		wantLive    bool
		wantAttempt bool
	}{
		{
			name: "an automatic attempt behind a turn still pending",
			seed: func(s pgtype.UUID) pgtype.UUID {
				older := insertTurnAt(ctx, t, pool, s, "pending", 60, nil, false, nil)
				insertTurnAt(ctx, t, pool, s, "pending", 30, nil, true, &auto)
				return older
			},
			// The older pending turn is the pick: the attempt waits.
			sandbox: "ready", wantLive: true,
		},
		{
			name: "an automatic attempt behind a turn that ended after it was created",
			seed: func(s pgtype.UUID) pgtype.UUID {
				insertTurnAt(ctx, t, pool, s, "completed", 60, secs(5), false, nil)
				return insertTurnAt(ctx, t, pool, s, "pending", 30, nil, true, &auto)
			},
			sandbox: "ready", wantQueued: true, wantLive: true, wantAttempt: true,
		},
		{
			name: "an automatic attempt behind a turn that ended before it was created",
			seed: func(s pgtype.UUID) pgtype.UUID {
				insertTurnAt(ctx, t, pool, s, "completed", 60, secs(45), false, nil)
				return insertTurnAt(ctx, t, pool, s, "pending", 30, nil, true, &auto)
			},
			sandbox: "ready", wantLive: true, wantAttempt: true,
		},
		{
			name: "an automatic attempt behind a turn that ended with no recorded end",
			seed: func(s pgtype.UUID) pgtype.UUID {
				insertTurnAt(ctx, t, pool, s, "failed", 60, nil, false, nil)
				return insertTurnAt(ctx, t, pool, s, "pending", 30, nil, true, &auto)
			},
			sandbox: "suspect", wantQueued: true, wantLive: true, wantAttempt: true,
		},
		{
			name: "a turn in flight: nothing to dispatch",
			seed: func(s pgtype.UUID) pgtype.UUID {
				insertTurnAt(ctx, t, pool, s, "processing", 60, nil, false, nil)
				insertTurnAt(ctx, t, pool, s, "pending", 30, nil, true, &auto)
				return pgtype.UUID{}
			},
			sandbox: "ready",
		},
		{
			name: "a pick flagged by a stop is passed over",
			seed: func(s pgtype.UUID) pgtype.UUID {
				flagged := insertTurnAt(ctx, t, pool, s, "pending", 60, nil, false, nil)
				if _, err := pool.Exec(ctx, `UPDATE turns SET stop_requested_at = now() WHERE id = $1`, flagged); err != nil {
					t.Fatal(err)
				}
				return insertTurnAt(ctx, t, pool, s, "pending", 30, nil, true, &auto)
			},
			sandbox: "booting", wantQueued: true, wantAttempt: true,
		},
		{
			name: "no sandbox yet",
			seed: func(s pgtype.UUID) pgtype.UUID {
				insertTurnAt(ctx, t, pool, s, "completed", 60, secs(5), false, nil)
				return insertTurnAt(ctx, t, pool, s, "pending", 30, nil, true, &auto)
			},
			wantQueued: true, wantAttempt: true,
		},
		{
			// The walk runs for a person's review attempt too: the check
			// applies to it, and one whose context moved is owed to its
			// requester (technical plan §24.9's third PR).
			name: "a person's review attempt that waited is read as queued",
			seed: func(s pgtype.UUID) pgtype.UUID {
				insertTurnAt(ctx, t, pool, s, "completed", 60, secs(5), false, nil)
				return insertTurnAt(ctx, t, pool, s, "pending", 30, nil, true, &label)
			},
			sandbox: "ready", wantQueued: true, wantLive: true, wantAttempt: true,
		},
		{
			name: "a review attempt with no trigger recorded is never read as queued",
			seed: func(s pgtype.UUID) pgtype.UUID {
				insertTurnAt(ctx, t, pool, s, "completed", 60, secs(5), false, nil)
				return insertTurnAt(ctx, t, pool, s, "pending", 30, nil, true, nil)
			},
			sandbox: "ready", wantLive: true, wantAttempt: true,
		},
		{
			name: "a turn that is no review attempt is never read as queued",
			seed: func(s pgtype.UUID) pgtype.UUID {
				insertTurnAt(ctx, t, pool, s, "completed", 60, secs(5), false, nil)
				return insertTurnAt(ctx, t, pool, s, "pending", 30, nil, false, nil)
			},
			sandbox: "ready", wantLive: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			if tc.sandbox != "" {
				if _, err := pool.Exec(ctx, `INSERT INTO sandboxes (session_id, status, gen) VALUES ($1, $2::sandbox_status, 1)`, sessionID, tc.sandbox); err != nil {
					t.Fatalf("seed the sandbox: %v", err)
				}
			}
			want := tc.seed(sessionID)
			got, err := turns.ReviewAttemptToCheck(ctx, sessionID)
			if !want.Valid {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("ReviewAttemptToCheck = %+v (err %v), want no row", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReviewAttemptToCheck: %v", err)
			}
			if got.ID != want {
				t.Fatalf("pick = %v, want %v", got.ID, want)
			}
			if got.Queued != tc.wantQueued || got.SandboxLive != tc.wantLive || got.IsReviewAttempt != tc.wantAttempt {
				t.Fatalf("queued %v live %v attempt %v; want %v %v %v", got.Queued, got.SandboxLive, got.IsReviewAttempt, tc.wantQueued, tc.wantLive, tc.wantAttempt)
			}
		})
	}
}

// TestListTurnsForSession_BreaksACreatedAtTieByID: turns one transaction
// created share its now(), and the session's turns are read in created_at
// order, then id's -- the order GetReviewAttemptToCheck picks by -- so the
// dispatch's pick from them is the turn the context check's pre-read
// named (technical plan §24.9).
func TestListTurnsForSession_BreaksACreatedAtTieByID(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	low := pgtype.UUID{Bytes: [16]byte{0x01}, Valid: true}
	high := pgtype.UUID{Bytes: [16]byte{0xfe}, Valid: true}

	// One transaction, the higher id stored first.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, id := range []pgtype.UUID{high, low} {
		if _, err := tx.Exec(ctx, `INSERT INTO turns (id, session_id, status) VALUES ($1, $2, 'pending')`, id, sessionID); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	listed, err := narvipg.NewTurnStore(pool).ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ID != low || listed[1].ID != high {
		t.Fatalf("turns listed %v, want the lower id first", []pgtype.UUID{listed[0].ID, listed[len(listed)-1].ID})
	}
	pick, err := narvipg.NewTurnStore(pool).ReviewAttemptToCheck(ctx, sessionID)
	if err != nil || pick.ID != low {
		t.Fatalf("the pre-read's pick = %v (err %v), want the lower id, the list's first", pick.ID, err)
	}
}
