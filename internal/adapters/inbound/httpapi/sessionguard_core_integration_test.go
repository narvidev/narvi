//go:build integration

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/platform"
)

// This file is the session guard (technical plan §40.1) at the core every
// person's and bot's new turn passes, createTurnLocked, and at the
// creation of a session with its first turn.

// coreCapSession creates a session of source spawnSource naming repo, with
// a Slack thread when it came from Slack, a completed, dispatched turn that
// cost spent, and repo's cap set to cap.
func coreCapSession(ctx context.Context, t *testing.T, rig *turnCoreTestRig, spawnSource sqlcgen.SessionSpawnSource, repo, limit, spent string) sqlcgen.Session {
	t.Helper()
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: spawnSource,
		Repos:       []byte(`[{"url": "https://github.com/` + repo + `.git"}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if spawnSource == sqlcgen.SessionSpawnSourceSlack {
		if _, err := rig.pool.Exec(ctx, `INSERT INTO slack_thread_sessions (channel_id, thread_ts, session_id) VALUES ('C-CAP', $1, $2)`, session.ID.String(), session.ID); err != nil {
			t.Fatalf("map the slack thread: %v", err)
		}
	}
	if _, err := rig.pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, $2::numeric)
		ON CONFLICT (repo_full_name) DO UPDATE SET session_spend_cap_usd = EXCLUDED.session_spend_cap_usd`, repo, limit); err != nil {
		t.Fatalf("set the cap: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `INSERT INTO turns (session_id, status, dispatched_at, completed_at, cost_usd) VALUES ($1, 'completed', now(), now(), $2::numeric)`, session.ID, spent); err != nil {
		t.Fatalf("store the spend: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `UPDATE sessions SET status = 'completed' WHERE id = $1`, session.ID); err != nil {
		t.Fatalf("settle the session: %v", err)
	}
	return session
}

func coreCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// TestCreateTurnCore_AtSpendCap_RefusedWithTypedReason: at its cap, a
// session refuses the next turn with a 409 whose error carries the typed
// reason (errors.Is ErrSpendCapReached); no turn is inserted, no dispatch
// timer armed, the session's status is as it was; the crossing's warning is
// stored once, at its deterministic message id, and one outbox notice goes
// to a chat-origin session's thread -- none for a web session, which has
// no channel. A second refusal of the same crossing adds neither.
func TestCreateTurnCore_AtSpendCap_RefusedWithTypedReason(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)

	for _, tc := range []struct {
		source      sqlcgen.SessionSpawnSource
		wantNotices int
	}{
		{source: sqlcgen.SessionSpawnSourceWeb, wantNotices: 0},
		{source: sqlcgen.SessionSpawnSourceSlack, wantNotices: 1},
	} {
		t.Run(string(tc.source), func(t *testing.T) {
			repo := "acme/core-" + string(tc.source)
			session := coreCapSession(ctx, t, rig, tc.source, repo, "3.00", "3.000000")
			want := sessionguard.Refusal{Reason: sessionguard.ReasonSpendCap, SessionID: session.ID.Bytes, Cap: 3_000_000, Spent: 3_000_000,
				Source: sessionguard.CapSource{Kind: sessionguard.CapSourceRepo, Name: repo, ID: repo}}

			for attempt := 1; attempt <= 2; attempt++ {
				_, created, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false),
					session.ID, "one more", nil, false, false, pgtype.UUID{}, RejectIfOpen)
				if cerr == nil || created {
					t.Fatalf("attempt %d: created %v, error %v; want a refusal", attempt, created, cerr)
				}
				if cerr.Status != http.StatusConflict || !errors.Is(cerr, sessionguard.ErrSpendCapReached) || cerr.Message != sessionguard.Text(want) {
					t.Fatalf("attempt %d: %d %q (is spend cap: %v); want 409 with the refusal's text", attempt, cerr.Status, cerr.Message, errors.Is(cerr, sessionguard.ErrSpendCapReached))
				}
				if got, ok := sessionguard.AsRefusal(cerr); !ok || got.Cap != want.Cap || got.Source != want.Source {
					t.Fatalf("attempt %d: refusal %+v, want %+v", attempt, got, want)
				}
				if n := coreCount(ctx, t, rig.pool, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID); n != 1 {
					t.Fatalf("attempt %d: %d turns, want the one it ran", attempt, n)
				}
				if n := coreCount(ctx, t, rig.pool, `SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = 'dispatch'`, session.ID); n != 0 {
					t.Fatalf("attempt %d: a refused turn armed the dispatch timer", attempt)
				}
				row, err := rig.sessions.Get(ctx, session.ID)
				if err != nil {
					t.Fatal(err)
				}
				if row.Status != sqlcgen.SessionStatusCompleted {
					t.Fatalf("attempt %d: session status %q, want completed as it was", attempt, row.Status)
				}
				if n := coreCount(ctx, t, rig.pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND message_id = $2`, session.ID, turnguard.WarningMessageID(want)); n != 1 {
					t.Fatalf("attempt %d: %d warnings at the crossing's id, want 1", attempt, n)
				}
				if n := coreCount(ctx, t, rig.pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindSlackSessionGuard)); n != tc.wantNotices {
					t.Fatalf("attempt %d: %d notices, want %d", attempt, n, tc.wantNotices)
				}
			}
		})
	}
}

// TestSpendCap_ACostCommittedAheadOfTheCreateIsSeen: the guard reads the
// spend under the session's row lock. A transaction holding that lock
// records a step cost that takes the session past its cap; a create that
// begins meanwhile waits for the lock, and once the cost commits it reads
// it and refuses. Read before the lock, it would have admitted the turn.
func TestSpendCap_ACostCommittedAheadOfTheCreateIsSeen(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := coreCapSession(ctx, t, rig, sqlcgen.SessionSpawnSourceWeb, "acme/lock-order", "1.00", "0.500000")
	if _, err := rig.pool.Exec(ctx, `INSERT INTO turns (session_id, status, dispatched_at) VALUES ($1, 'processing', now())`, session.ID); err != nil {
		t.Fatal(err)
	}

	holder, err := rig.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := rig.sessions.WithTx(holder).GetActorEpochForUpdate(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := rig.turns.WithTx(holder).RecordStepCostUSD(ctx, session.ID, "step-past-the-cap", 0.75); err != nil || n != 1 {
		t.Fatalf("record the step cost: %d, %v", n, err)
	}

	var cerr *CreateTurnError
	var g errgroup.Group
	g.Go(func() error {
		// AlwaysQueue, as a mention's turn is created: the processing turn
		// is no busy refusal for it.
		_, _, cerr = CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false),
			session.ID, "queued behind it", nil, false, false, pgtype.UUID{}, AlwaysQueue)
		return nil
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if n := coreCount(ctx, t, rig.pool, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%FOR UPDATE%'`); n > 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = holder.Rollback(ctx)
			_ = g.Wait()
			t.Fatal("the create never waited for the session's row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	refusal, ok := sessionguard.AsRefusal(cerr)
	if !ok {
		t.Fatalf("create after the cost committed = %v, want a refusal: the guard read the spend before the lock", cerr)
	}
	if refusal.Spent != 1_250_000 {
		t.Fatalf("refusal read %s spent, want $1.25: the cost committed ahead of it", refusal.Spent)
	}
	if n := coreCount(ctx, t, rig.pool, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID); n != 2 {
		t.Fatalf("%d turns, want the two it had", n)
	}
}

// TestCreateSessionOnTx_FirstTurnAlwaysAdmitted: a session created with its
// first turn in one transaction is admitted without a read -- it has spent
// nothing -- even under the strictest cap a repository can set.
func TestCreateSessionOnTx_FirstTurnAlwaysAdmitted(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	const repo = "acme/first-turn"
	if _, err := rig.pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, 0.01)
		ON CONFLICT (repo_full_name) DO UPDATE SET session_spend_cap_usd = 0.01`, repo); err != nil {
		t.Fatal(err)
	}
	prompt := "start"
	req := restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
		Prompt:      restdtos.CreateSessionRequestPrompt(&prompt),
		Repos:       []restdtos.CreateSessionRequestReposElem{{Name: "first-turn", Url: "https://github.com/" + repo}},
	}
	// The repository is one this deployment knows, as a mention's
	// would be.
	known := rig.newFixtureSession(t, ctx)
	prSessions := narvipg.NewGitHubPRSessionStore(rig.pool)
	if err := prSessions.EnsureRow(ctx, repo, 1); err != nil {
		t.Fatal(err)
	}
	if err := prSessions.SetSessionID(ctx, repo, 1, known.ID); err != nil {
		t.Fatal(err)
	}
	entitlement, everr := ResolveRepoEntitlement(ctx, prSessions, rig.auditLog, pgtype.UUID{}, req)
	if everr != nil {
		t.Fatalf("ResolveRepoEntitlement: %d %q", everr.Status, everr.Message)
	}
	tx, err := rig.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, hasPrompt, serr := CreateSessionOnTx(ctx, tx, rig.sessions, rig.turns, narvipg.NewEnvironmentStore(rig.pool), rig.auditLog, req, pgtype.UUID{}, false, platform.RolloutModeOpen, narvipg.NewRepoSettingsStore(rig.pool), entitlement)
	if serr != nil || !hasPrompt {
		t.Fatalf("CreateSessionOnTx: %v (prompt %v)", serr, hasPrompt)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := coreCount(ctx, t, rig.pool, `SELECT count(*) FROM turns WHERE session_id = $1 AND status = 'pending'`, created.ID); n != 1 {
		t.Fatalf("%d pending turns, want the session's first", n)
	}
}
