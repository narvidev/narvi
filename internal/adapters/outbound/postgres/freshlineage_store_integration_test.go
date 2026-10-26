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

// This file pins the statements of technical plan §35.5's fresh lineage
// on real Postgres: the mark a fresh claim writes and every claim clears,
// the clear that only the recap turn's arrival on the marked gen makes,
// the read that says whether a turn was dispatched before a gen, the
// recap stored on a turn, and the conversation id a fresh spawn forgets.

// freshLineageGen reads sessionID's sandbox gen and its mark.
func freshLineageGen(ctx context.Context, t *testing.T, store *narvipg.SandboxStore, sessionID pgtype.UUID) (gen int32, mark *int32) {
	t.Helper()
	row, err := store.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return row.Gen, row.FreshLineageGen
}

// TestUpsertSandboxForSpawn_ClearsTheFreshLineageMark: the mark is written
// only for the live gen, and every claim's upsert -- a spawn, a restore and
// a resume run the same statement -- clears it, whatever lifetime it
// passes, so no claim starts marked.
func TestUpsertSandboxForSpawn_ClearsTheFreshLineageMark(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)
	token := "token-hash-fresh-lineage"
	lifetime := int32(7200)

	for _, tc := range []struct {
		name     string
		lifetime *int32
	}{
		{"a spawn or restore, which stamps a lifetime", &lifetime},
		{"a resume, which carries one", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			if _, err := store.Create(ctx, sessionID); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkFreshLineage(ctx, sessionID, 2); err != nil {
				t.Fatalf("MarkFreshLineage of a gen that is not live: %v", err)
			}
			if gen, mark := freshLineageGen(ctx, t, store, sessionID); mark != nil {
				t.Fatalf("gen %d marked %d by a mark of gen 2, want unmarked: only the live gen is marked", gen, *mark)
			}
			if err := store.MarkFreshLineage(ctx, sessionID, 1); err != nil {
				t.Fatalf("MarkFreshLineage: %v", err)
			}
			if gen, mark := freshLineageGen(ctx, t, store, sessionID); mark == nil || *mark != gen {
				t.Fatalf("gen %d marked %v, want marked for itself", gen, mark)
			}

			row, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &token, LifetimeSeconds: tc.lifetime})
			if err != nil {
				t.Fatalf("UpsertForSpawn: %v", err)
			}
			if row.Gen != 2 || row.FreshLineageGen != nil {
				t.Fatalf("after the claim: gen %d, mark %v; want gen 2 and no mark", row.Gen, row.FreshLineageGen)
			}
		})
	}
}

// TestClearSandboxFreshLineageOnReach_OnlyTheRecapTurnOnTheMarkedGen: the
// mark is cleared only for the gen it names, by evidence of that gen,
// while a turn is processing there with a recap built for it -- and, for a
// prompt_received, only when it names that turn's prompt.
func TestClearSandboxFreshLineageOnReach_OnlyTheRecapTurnOnTheMarkedGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sandboxes := narvipg.NewSandboxStore(pool)
	turns := narvipg.NewTurnStore(pool)
	const recapMessageID = "prompt-with-the-recap"
	other := "another-prompt"
	recapPrompt := recapMessageID

	type seed struct {
		markGen   int32 // 0: no mark
		status    sqlcgen.TurnStatus
		turnGen   int32
		recapGen  int32 // 0: no recap
		messageID string
	}
	base := seed{markGen: 2, status: sqlcgen.TurnStatusProcessing, turnGen: 2, recapGen: 2, messageID: recapMessageID}
	tests := []struct {
		name            string
		seed            func(seed) seed
		gen             int32
		promptMessageID *string
		want            int64
	}{
		{"an event of the recap turn", func(s seed) seed { return s }, 2, nil, 1},
		{"the recap turn's prompt_received", func(s seed) seed { return s }, 2, &recapPrompt, 1},
		{"another prompt's prompt_received", func(s seed) seed { return s }, 2, &other, 0},
		{"an event of another gen", func(s seed) seed { return s }, 1, nil, 0},
		{"no mark", func(s seed) seed { s.markGen = 0; return s }, 2, nil, 0},
		{"a mark of an earlier gen", func(s seed) seed { s.markGen = 1; return s }, 2, nil, 0},
		{"the turn is no longer processing", func(s seed) seed { s.status = sqlcgen.TurnStatusCompleted; return s }, 2, nil, 0},
		{"the turn processing on another gen", func(s seed) seed { s.turnGen = 1; return s }, 2, nil, 0},
		{"the turn carries no recap", func(s seed) seed { s.recapGen = 0; return s }, 2, nil, 0},
		{"the turn's recap was built for another gen", func(s seed) seed { s.recapGen = 1; return s }, 2, nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.seed(base)
			sessionID := createTestSession(ctx, t, pool)
			if _, err := sandboxes.Create(ctx, sessionID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET gen = 2, fresh_lineage_gen = NULLIF($2::integer, 0) WHERE session_id = $1`, sessionID, s.markGen); err != nil {
				t.Fatal(err)
			}
			created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: s.status})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
				ID: created.ID, Status: s.status, DispatchedSandboxGen: &s.turnGen, DispatchedMessageID: &s.messageID,
			}); err != nil {
				t.Fatal(err)
			}
			if s.recapGen != 0 {
				if err := turns.SetContinuityRecap(ctx, created.ID, "the recap", s.recapGen); err != nil {
					t.Fatalf("SetContinuityRecap: %v", err)
				}
			}

			n, err := sandboxes.ClearFreshLineageOnReach(ctx, sessionID, tc.gen, tc.promptMessageID)
			if err != nil {
				t.Fatalf("ClearFreshLineageOnReach: %v", err)
			}
			if n != tc.want {
				t.Fatalf("ClearFreshLineageOnReach cleared %d rows, want %d", n, tc.want)
			}
			_, mark := freshLineageGen(ctx, t, sandboxes, sessionID)
			if cleared := s.markGen != 0 && mark == nil; cleared != (tc.want == 1) {
				t.Fatalf("mark after the call = %v, want cleared %v", mark, tc.want == 1)
			}
		})
	}
}

// TestHasTurnDispatchedBeforeGen: a turn counts at the gen it was last
// dispatched to, below the gen asked about; a turn never dispatched, and
// another session's, do not count.
func TestHasTurnDispatchedBeforeGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	turns := narvipg.NewTurnStore(pool)
	seedTurn := func(t *testing.T, sessionID pgtype.UUID, gen *int32) {
		t.Helper()
		created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending})
		if err != nil {
			t.Fatal(err)
		}
		if gen == nil {
			return
		}
		if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: created.ID, Status: sqlcgen.TurnStatusCompleted, DispatchedSandboxGen: gen}); err != nil {
			t.Fatal(err)
		}
	}
	gen := func(g int32) *int32 { return &g }
	tests := []struct {
		name  string
		seed  func(t *testing.T, sessionID, otherSession pgtype.UUID)
		askAt int32
		want  bool
	}{
		{"no turn", func(*testing.T, pgtype.UUID, pgtype.UUID) {}, 2, false},
		{"a turn never dispatched", func(t *testing.T, s, _ pgtype.UUID) { seedTurn(t, s, nil) }, 2, false},
		{"a turn dispatched to the gen below", func(t *testing.T, s, _ pgtype.UUID) { seedTurn(t, s, gen(1)) }, 2, true},
		{"a turn dispatched to the gen asked about", func(t *testing.T, s, _ pgtype.UUID) { seedTurn(t, s, gen(2)) }, 2, false},
		{"another session's turn", func(t *testing.T, _, o pgtype.UUID) { seedTurn(t, o, gen(1)) }, 2, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionID, otherSession := createTestSession(ctx, t, pool), createTestSession(ctx, t, pool)
			tc.seed(t, sessionID, otherSession)
			got, err := turns.HasTurnDispatchedBeforeGen(ctx, sessionID, tc.askAt)
			if err != nil {
				t.Fatalf("HasTurnDispatchedBeforeGen: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasTurnDispatchedBeforeGen(%d) = %v, want %v", tc.askAt, got, tc.want)
			}
		})
	}
}

// TestSetTurnContinuityRecap_StoredWithItsGen: the recap is stored on the
// turn with the gen it was built for, and a recap built for a later gen
// replaces it.
func TestSetTurnContinuityRecap_StoredWithItsGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	turns := narvipg.NewTurnStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusProcessing})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		recap string
		gen   int32
	}{{"the recap for gen 2", 2}, {"the recap for gen 3", 3}} {
		if err := turns.SetContinuityRecap(ctx, created.ID, step.recap, step.gen); err != nil {
			t.Fatalf("SetContinuityRecap: %v", err)
		}
		row, err := turns.Get(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.ContinuityRecap == nil || *row.ContinuityRecap != step.recap || row.ContinuityRecapGen == nil || *row.ContinuityRecapGen != step.gen {
			t.Fatalf("turn's recap = %v for gen %v, want %q for gen %d", row.ContinuityRecap, row.ContinuityRecapGen, step.recap, step.gen)
		}
	}
}

// TestClearSessionConversationID_LeavesUpdatedAt: a fresh spawn forgets
// the session's conversation id, reports whether it had one, and leaves
// updated_at as it was -- the decision inbox orders failed sessions by it.
func TestClearSessionConversationID_LeavesUpdatedAt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	conversation := "conv-before-the-fresh-spawn"
	if _, err := sessions.UpdateConversationID(ctx, sqlcgen.UpdateSessionConversationIDParams{ID: sessionID, OpencodeConversationID: &conversation}); err != nil {
		t.Fatal(err)
	}
	updatedAt := sessionUpdatedAt(ctx, t, pool, sessionID)

	for i, want := range []bool{true, false} {
		had, err := sessions.ClearConversationID(ctx, sessionID)
		if err != nil {
			t.Fatalf("ClearConversationID: %v", err)
		}
		if had != want {
			t.Fatalf("call %d: ClearConversationID reported %v, want %v", i+1, had, want)
		}
		row, err := sessions.Get(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if row.OpencodeConversationID != nil {
			t.Fatalf("conversation id %q after the clear, want none", *row.OpencodeConversationID)
		}
		if got := sessionUpdatedAt(ctx, t, pool, sessionID); !got.Equal(updatedAt) {
			t.Fatalf("updated_at moved from %v to %v, want it left as it was", updatedAt, got)
		}
	}
}

func sessionUpdatedAt(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) time.Time {
	t.Helper()
	var at time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM sessions WHERE id = $1`, sessionID).Scan(&at); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	return at
}
