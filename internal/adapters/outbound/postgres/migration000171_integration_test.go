//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

// This file runs the review_sessions_base_repository migration through
// golang-migrate against real Postgres, in a database of its own migrated
// to the version before it. The up moves a legacy review session of a pull
// request from a fork -- its spec naming the fork -- onto the pull
// request's base repository (technical plan §21.1, §30.4), only when its
// sandbox holds no live gen; every other session is untouched, a second
// run moves nothing, and the down changes nothing. It also pins that the
// actor's statement for one session, MoveReviewSessionToBaseRepository,
// moves what the migration left for it the same way and nothing else, and
// that a gen a spawn starts while the migration waits on its session is
// never moved under.

// reviewSessionsBaseRepositoryMigration is this migration's version.
const reviewSessionsBaseRepositoryMigration = 171

// baseRepositoryCase is one session the migration is run over.
type baseRepositoryCase struct {
	name string
	// repos is the session's spec before the migration.
	repos string
	// claims are the github_pr_sessions rows naming the session.
	claims []string
	// sandbox is the sandbox row's status; "" for no sandbox row.
	sandbox string
	// moved is the spec after the migration; "" when it is untouched.
	moved string
	// actorMoves is the spec the actor's statement moves a session the
	// migration left to; "" when it moves nothing.
	actorMoves string
}

func baseRepositoryCases() []baseRepositoryCase {
	const forked = `[{"name":"widgets-fork","url":"https://github.com/contributor/widgets-fork.git","branch":"main"}]`
	const onBase = `[{"name":"widgets-fork","url":"https://github.com/acme/widgets.git","branch":null}]`
	return []baseRepositoryCase{
		{name: "a fork's session with no sandbox moves", repos: forked, claims: []string{"acme/widgets"}, moved: onBase},
		{name: "a fork's session whose sandbox is pending moves", repos: forked, claims: []string{"acme/widgets"}, sandbox: "pending", moved: onBase},
		{name: "a fork's session whose sandbox stopped moves", repos: forked, claims: []string{"acme/widgets"}, sandbox: "stopped", moved: onBase},
		{name: "a fork's session whose sandbox failed moves", repos: forked, claims: []string{"acme/widgets"}, sandbox: "failed", moved: onBase},
		{name: "a fork's session whose gen is ready waits for the actor", repos: forked, claims: []string{"acme/widgets"}, sandbox: "ready", actorMoves: onBase},
		{name: "a fork's session whose gen is spawning waits for the actor", repos: forked, claims: []string{"acme/widgets"}, sandbox: "spawning", actorMoves: onBase},
		{name: "a fork's session whose gen is suspect waits for the actor", repos: forked, claims: []string{"acme/widgets"}, sandbox: "suspect", actorMoves: onBase},
		{name: "a fork's session whose gen is snapshotting waits for the actor", repos: forked, claims: []string{"acme/widgets"}, sandbox: "snapshotting", actorMoves: onBase},
		{name: "a fork's session whose gen is connecting waits for the actor", repos: forked, claims: []string{"acme/widgets"}, sandbox: "connecting", actorMoves: onBase},
		{name: "a fork's session whose gen is booting waits for the actor", repos: forked, claims: []string{"acme/widgets"}, sandbox: "booting", actorMoves: onBase},
		{
			// The actor's statement keeps the host, as the migration's does.
			name:       "a fork's session on another host whose gen is ready waits for the actor, which keeps the host",
			repos:      `[{"name":"tools","url":"https://ghes.example.test/someone/tools.git","branch":"patch-1"}]`,
			claims:     []string{"Acme/Tools"},
			sandbox:    "ready",
			actorMoves: `[{"name":"tools","url":"https://ghes.example.test/Acme/Tools.git","branch":null}]`,
		},
		{
			name:   "a fork url without .git moves, the host kept",
			repos:  `[{"name":"tools","url":"https://code.example.test/someone/tools","branch":"patch-1"}]`,
			claims: []string{"Acme/Tools"},
			moved:  `[{"name":"tools","url":"https://code.example.test/Acme/Tools.git","branch":null}]`,
		},
		{
			name:   "a same-repository session is untouched, its branch kept",
			repos:  `[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":"feature-x"}]`,
			claims: []string{"acme/widgets"},
		},
		{
			name:   "a same-repository session is untouched whatever the case and suffix",
			repos:  `[{"name":"widgets","url":"https://github.com/ACME/Widgets","branch":"feature-x"}]`,
			claims: []string{"acme/widgets"},
		},
		{
			name:  "a session with no claim is untouched",
			repos: `[{"name":"widgets","url":"https://github.com/contributor/widgets.git","branch":"main"}]`,
		},
		{
			name:   "a session with two claims is untouched",
			repos:  forked,
			claims: []string{"acme/widgets", "acme/other"},
		},
		{
			name:   "a multi-repo session is untouched",
			repos:  `[{"name":"a","url":"https://github.com/contributor/a.git","branch":"main"},{"name":"b","url":"https://github.com/acme/b.git","branch":null}]`,
			claims: []string{"acme/a"},
		},
		{
			name:   "a url that is not https is untouched",
			repos:  `[{"name":"widgets","url":"http://github.com/contributor/widgets.git","branch":"main"}]`,
			claims: []string{"acme/widgets"},
		},
		{
			name:   "a url with a nested path is untouched",
			repos:  `[{"name":"widgets","url":"https://github.com/group/sub/widgets.git","branch":"main"}]`,
			claims: []string{"acme/widgets"},
		},
		{name: "a session with no repos is untouched", repos: `[]`, claims: []string{"acme/widgets"}},
	}
}

// seedBaseRepositoryCase inserts tc's session, claims and sandbox, and
// returns the session's id.
func seedBaseRepositoryCase(ctx context.Context, t *testing.T, db *sql.DB, tc baseRepositoryCase, prNumber int) string {
	t.Helper()
	var id string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source, repos) VALUES ('github', $1::jsonb) RETURNING id::text`, tc.repos).Scan(&id); err != nil {
		t.Fatalf("%s: insert the session: %v", tc.name, err)
	}
	for i, claim := range tc.claims {
		if _, err := db.ExecContext(ctx, `INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id) VALUES ($1, $2, $3)`, claim, prNumber*10+i, id); err != nil {
			t.Fatalf("%s: insert a claim: %v", tc.name, err)
		}
	}
	if tc.sandbox != "" {
		if _, err := db.ExecContext(ctx, `INSERT INTO sandboxes (session_id, status) VALUES ($1, $2::sandbox_status)`, id, tc.sandbox); err != nil {
			t.Fatalf("%s: insert the sandbox: %v", tc.name, err)
		}
	}
	return id
}

// sessionRepos reads a session's spec, decoded so specs compare by value.
func sessionRepos(ctx context.Context, t *testing.T, db *sql.DB, id string) any {
	t.Helper()
	var raw []byte
	if err := db.QueryRowContext(ctx, `SELECT repos FROM sessions WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read session %s: %v", id, err)
	}
	return decodeRepos(t, raw)
}

func decodeRepos(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode repos %s: %v", raw, err)
	}
	return v
}

func TestMigrationReviewSessionsBaseRepository_MovesALegacyForkReviewSessionOnlyWithNoLiveGen(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, reviewSessionsBaseRepositoryMigration)
	connStr, db := migrationTestDatabase(ctx, t, previousVersion)

	cases := baseRepositoryCases()
	ids := make([]string, len(cases))
	for i, tc := range cases {
		ids[i] = seedBaseRepositoryCase(ctx, t, db, tc, i+1)
	}
	assertSpecs := func(stage string, want func(baseRepositoryCase) string) {
		t.Helper()
		for i, tc := range cases {
			wantRepos := decodeRepos(t, []byte(want(tc)))
			if got := sessionRepos(ctx, t, db, ids[i]); !reflect.DeepEqual(got, wantRepos) {
				t.Errorf("%s, %s: repos = %v, want %v", stage, tc.name, got, wantRepos)
			}
		}
	}
	afterUp := func(tc baseRepositoryCase) string {
		if tc.moved != "" {
			return tc.moved
		}
		return tc.repos
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(reviewSessionsBaseRepositoryMigration); err != nil {
		t.Fatalf("up to %d: %v", reviewSessionsBaseRepositoryMigration, err)
	}
	assertSpecs("after the up", afterUp)

	// Run again -- as a redeploy after `migrate force` to the previous version
	// does: nothing
	// already moved moves again, nothing else moves.
	if err := m.Force(int(previousVersion)); err != nil {
		t.Fatalf("force %d: %v", previousVersion, err)
	}
	if err := m.Migrate(reviewSessionsBaseRepositoryMigration); err != nil {
		t.Fatalf("up to %d again: %v", reviewSessionsBaseRepositoryMigration, err)
	}
	assertSpecs("after a second up", afterUp)

	// The down changes nothing.
	if err := m.Migrate(previousVersion); err != nil {
		t.Fatalf("down to %d: %v", previousVersion, err)
	}
	assertSpecs("after the down", afterUp)

	// The previous binary cannot boot on this version: golang-migrate refuses a
	// version it has no file for.
	if err := m.Migrate(reviewSessionsBaseRepositoryMigration); err != nil {
		t.Fatalf("up to %d after the down: %v", reviewSessionsBaseRepositoryMigration, err)
	}
	previous, pdb := previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(reviewSessionsBaseRepositoryMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", reviewSessionsBaseRepositoryMigration, err, reviewSessionsBaseRepositoryMigration)
	}
	_ = pdb.Close()

	// The actor's statement, for one session, moves exactly what the
	// migration left to it -- a session whose gen was live -- the same way,
	// and nothing else; a second call moves nothing.
	pool, err := narvipg.NewPool(ctx, connStr)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	sessions := narvipg.NewSessionStore(pool)
	for i, tc := range cases {
		var id pgtype.UUID
		if err := id.Scan(ids[i]); err != nil {
			t.Fatal(err)
		}
		repos, moved, err := sessions.MoveReviewSessionToBaseRepository(ctx, id)
		if err != nil {
			t.Fatalf("%s: MoveReviewSessionToBaseRepository: %v", tc.name, err)
		}
		if moved != (tc.actorMoves != "") {
			t.Errorf("%s: the actor's statement moved = %v, want %v", tc.name, moved, tc.actorMoves != "")
			continue
		}
		if !moved {
			continue
		}
		want := decodeRepos(t, []byte(tc.actorMoves))
		if got := decodeRepos(t, repos); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the actor's statement returned %v, want %v", tc.name, got, want)
		}
		if got := sessionRepos(ctx, t, db, ids[i]); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: after the actor's statement repos = %v, want %v", tc.name, got, want)
		}
		if _, again, err := sessions.MoveReviewSessionToBaseRepository(ctx, id); err != nil || again {
			t.Errorf("%s: a second call moved = %v (err %v), want nothing", tc.name, again, err)
		}
	}
}

// TestMigrationReviewSessionsBaseRepository_NeverMovesASessionUnderAGenSpawnedWhileItWaits: a
// spawn holds a legacy session's lock -- as every actor transaction does,
// GetSessionActorEpochForUpdate -- and moves its stopped sandbox to
// spawning, a gen whose SESSION_CONFIG names the fork. The migration starts
// while that spawn is open and waits on the session's row; once the spawn
// commits, the gen is live, so the session must stay on the fork's spec
// for the actor to move at its next boot.
func TestMigrationReviewSessionsBaseRepository_NeverMovesASessionUnderAGenSpawnedWhileItWaits(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, reviewSessionsBaseRepositoryMigration)
	connStr, db := migrationTestDatabase(ctx, t, previousVersion)

	const forked = `[{"name":"widgets","url":"https://github.com/contributor/widgets.git","branch":"main"}]`
	id := seedBaseRepositoryCase(ctx, t, db, baseRepositoryCase{name: "spawning while the migration waits", repos: forked, claims: []string{"acme/widgets"}, sandbox: "stopped"}, 1)
	control := seedBaseRepositoryCase(ctx, t, db, baseRepositoryCase{name: "stopped throughout", repos: forked, claims: []string{"acme/widgets"}, sandbox: "stopped"}, 2)

	spawn, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = spawn.Rollback() }()
	if _, err := spawn.ExecContext(ctx, `SELECT actor_epoch FROM sessions WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatalf("the spawn's session lock: %v", err)
	}
	if _, err := spawn.ExecContext(ctx, `UPDATE sandboxes SET status = 'spawning', gen = gen + 1 WHERE session_id = $1`, id); err != nil {
		t.Fatalf("the spawn's sandbox write: %v", err)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	var group errgroup.Group
	group.Go(func() error { return m.Migrate(reviewSessionsBaseRepositoryMigration) })

	// The migration is waiting on the spawn's lock.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the migration never waited on the spawn's lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := spawn.Commit(); err != nil {
		t.Fatalf("commit the spawn: %v", err)
	}
	if err := group.Wait(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("up to %d: %v", reviewSessionsBaseRepositoryMigration, err)
	}

	if got, want := sessionRepos(ctx, t, db, id), decodeRepos(t, []byte(forked)); !reflect.DeepEqual(got, want) {
		t.Errorf("the session a spawn started a gen for while the migration waited: repos = %v, want the fork's %v kept for that gen", got, want)
	}
	onBase := decodeRepos(t, []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":null}]`))
	if got := sessionRepos(ctx, t, db, control); !reflect.DeepEqual(got, onBase) {
		t.Errorf("the control session, stopped throughout: repos = %v, want %v", got, onBase)
	}
}

// TestMigrationReviewSessionsBaseRepository_NeverMovesASessionOpenedWhileItRuns:
// during a rolling deploy a previous replica keeps opening fork review
// sessions, on the fork's spec, and spawning their first gen. Session Y,
// a candidate, is held by an actor's transaction, so the migration's lock
// statement waits on it. Meanwhile a new fork session X is committed, and
// its first spawn locks X and starts a gen booting on the fork's spec. Once
// Y's holder commits, the migration moves Y -- it locked Y -- and must leave
// X, which it never locked, to the actor's next spawn or restore, whether
// the spawn commits before or after the migration's update would have
// reached X.
func TestMigrationReviewSessionsBaseRepository_NeverMovesASessionOpenedWhileItRuns(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, reviewSessionsBaseRepositoryMigration)
	connStr, db := migrationTestDatabase(ctx, t, previousVersion)

	const ySpec = `[{"name":"widgets","url":"https://github.com/contributor/widgets.git","branch":"main"}]`
	y := seedBaseRepositoryCase(ctx, t, db, baseRepositoryCase{name: "held while the migration starts", repos: ySpec, claims: []string{"acme/widgets"}, sandbox: "stopped"}, 1)

	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT actor_epoch FROM sessions WHERE id = $1 FOR UPDATE`, y); err != nil {
		t.Fatalf("hold Y: %v", err)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	var group errgroup.Group
	var migrated atomic.Bool
	group.Go(func() error {
		defer migrated.Store(true)
		return m.Migrate(reviewSessionsBaseRepositoryMigration)
	})
	waitingOn := func(pid int) bool {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		return n > 0
	}
	var holderPID int
	if err := holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the migration to wait on Y's holder", func() bool { return waitingOn(holderPID) })

	// A previous replica opens X, a fork's review session, and spawns its
	// first gen, booting on the fork's spec.
	const xSpec = `[{"name":"gadgets","url":"https://github.com/contributor/gadgets.git","branch":"feature"}]`
	x := seedBaseRepositoryCase(ctx, t, db, baseRepositoryCase{name: "opened while the migration runs", repos: xSpec, claims: []string{"acme/gadgets"}}, 2)
	spawn, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = spawn.Rollback() }()
	var spawnPID int
	if err := spawn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&spawnPID); err != nil {
		t.Fatal(err)
	}
	if _, err := spawn.ExecContext(ctx, `SELECT repos FROM sessions WHERE id = $1 FOR UPDATE`, x); err != nil {
		t.Fatalf("the spawn's lock on X: %v", err)
	}
	if _, err := spawn.ExecContext(ctx, `INSERT INTO sandboxes (session_id, status) VALUES ($1, 'spawning')`, x); err != nil {
		t.Fatalf("the spawn's sandbox: %v", err)
	}

	if err := holder.Commit(); err != nil {
		t.Fatalf("commit Y's holder: %v", err)
	}
	// Either the migration finishes without touching X, or -- a migration
	// that would move X -- it waits on the spawn's lock on X.
	waitFor(t, "the migration to finish or to wait on X's spawn", func() bool { return migrated.Load() || waitingOn(spawnPID) })
	if err := spawn.Commit(); err != nil {
		t.Fatalf("commit X's spawn: %v", err)
	}
	if err := group.Wait(); err != nil {
		t.Fatalf("up to %d: %v", reviewSessionsBaseRepositoryMigration, err)
	}

	if got, want := sessionRepos(ctx, t, db, x), decodeRepos(t, []byte(xSpec)); !reflect.DeepEqual(got, want) {
		t.Errorf("X, opened and spawned while the migration ran: repos = %v, want the fork's %v kept for its booting gen", got, want)
	}
	onBase := decodeRepos(t, []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":null}]`))
	if got := sessionRepos(ctx, t, db, y); !reflect.DeepEqual(got, onBase) {
		t.Errorf("Y, which the migration locked: repos = %v, want %v", got, onBase)
	}
}

// waitFor polls cond until it holds, or fails the test after 30 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
