//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

// This file is the session guard's store on a real database (technical
// plan §40.1): what GetSessionGuardFacts reads -- a session's spend, derived
// from its dispatched turns, and the caps its automation and its
// repositories set -- and the store's refusal of a turn without an
// admission for its session.

// guardTestSession creates a web session whose clone URLs name repos
// (owner/name, on github.com).
func guardTestSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repos ...string) pgtype.UUID {
	t.Helper()
	type repo struct {
		URL string `json:"url"`
	}
	list := make([]repo, 0, len(repos))
	for _, r := range repos {
		list = append(list, repo{URL: "https://github.com/" + r + ".git"})
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	created, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		Repos:       raw,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return created.ID
}

// guardTestTurn inserts a turn of sessionID: dispatched (and completed) or
// still pending, with cost, a NUMERIC literal, or none when "".
func guardTestTurn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, dispatched bool, cost string) {
	t.Helper()
	status, dispatchedAt := "pending", "NULL"
	if dispatched {
		status, dispatchedAt = "completed", "now()"
	}
	costSQL := "NULL"
	if cost != "" {
		costSQL = cost + "::numeric"
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO turns (session_id, status, dispatched_at, completed_at, cost_usd)
		VALUES ($1, '%s', %s, CASE WHEN %t THEN now() END, %s)`, status, dispatchedAt, dispatched, costSQL), sessionID); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
}

// guardTestRepoCap sets repo's session spend cap to limit ("" for none).
func guardTestRepoCap(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo, limit string) {
	t.Helper()
	var value any
	if limit != "" {
		value = limit
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, $2::numeric)
		ON CONFLICT (repo_full_name) DO UPDATE SET session_spend_cap_usd = EXCLUDED.session_spend_cap_usd`, repo, value); err != nil {
		t.Fatalf("set repo cap: %v", err)
	}
}

// guardTestAutomation creates an automation named name with the cap limit
// ("" for none) and an automation run of it that created sessionID.
func guardTestAutomation(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, name, limit string) pgtype.UUID {
	t.Helper()
	var value any
	if limit != "" {
		value = limit
	}
	var automationID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO automations (name, repos, session_spend_cap_usd) VALUES ($1, '[]'::jsonb, $2::numeric) RETURNING id`, name, value).Scan(&automationID); err != nil {
		t.Fatalf("create automation: %v", err)
	}
	if _, err := pool.Exec(ctx, `WITH inv AS (
			INSERT INTO automation_invocations (automation_id, targets, total_runs) VALUES ($1, '[]'::jsonb, 1) RETURNING id)
		INSERT INTO automation_runs (invocation_id, automation_id, target, session_id)
		SELECT inv.id, $1, '{}'::jsonb, $2 FROM inv`, automationID, sessionID); err != nil {
		t.Fatalf("create automation run: %v", err)
	}
	return automationID
}

func micro(t *testing.T, s string) sessionguard.MicroUSD {
	t.Helper()
	v, err := sessionguard.ParseMicroUSD(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestSessionGuardFacts_SumsDispatchedTurnsOnly: the spend is the sum of
// cost_usd over the session's dispatched turns -- a cost on a turn never
// dispatched, which no writer produces (TestEveryTurnCostWriterTargetsA
// ProcessingTurn), is not read, a turn with no cost adds nothing, and
// another session's turns are not the session's.
func TestSessionGuardFacts_SumsDispatchedTurnsOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSessionGuardStore(pool)

	cases := []struct {
		name  string
		turns []struct {
			dispatched bool
			cost       string
		}
		want string
	}{
		{name: "no turn", want: "0"},
		{name: "dispatched costs sum", turns: []struct {
			dispatched bool
			cost       string
		}{{true, "1.250000"}, {true, "0.000001"}, {true, ""}}, want: "1.250001"},
		{name: "an undispatched cost is not read", turns: []struct {
			dispatched bool
			cost       string
		}{{true, "2.000000"}, {false, "50.000000"}, {false, ""}}, want: "2.000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := guardTestSession(ctx, t, pool)
			for _, turn := range tc.turns {
				guardTestTurn(ctx, t, pool, sessionID, turn.dispatched, turn.cost)
			}
			other := guardTestSession(ctx, t, pool)
			guardTestTurn(ctx, t, pool, other, true, "99.000000")

			facts, err := store.Facts(ctx, sessionID, nil)
			if err != nil {
				t.Fatalf("Facts: %v", err)
			}
			if facts.SpentUSD != micro(t, tc.want) {
				t.Fatalf("spent = %s, want %s", facts.SpentUSD, micro(t, tc.want))
			}
			if facts.SessionID != sessionID.Bytes || facts.ObservedAt.IsZero() {
				t.Fatalf("facts = %+v: session id or observed-at missing", facts)
			}
			if facts.AutomationCap != nil || facts.RepoCap != nil {
				t.Fatalf("caps = %v, %v for a session with none", facts.AutomationCap, facts.RepoCap)
			}
		})
	}

	if _, err := store.Facts(ctx, pgtype.UUID{Bytes: [16]byte{9, 9, 9}, Valid: true}, nil); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Facts of a missing session = %v, want pgx.ErrNoRows", err)
	}
}

// TestSessionGuardFacts_AtTheCapToTheMicroDollar: the spend is summed and
// compared in whole micro-dollars, so a spend one micro-dollar under the
// cap is admitted and a spend exactly at it is refused -- including one
// whose turns' costs, summed in binary floating point, fall just short of
// the cap (0.7 + 0.2 + 0.1 is 0.9999999999999999 there).
func TestSessionGuardFacts_AtTheCapToTheMicroDollar(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSessionGuardStore(pool)

	for _, tc := range []struct {
		name       string
		limit      string
		costs      []string
		wantSpent  string
		wantRefuse bool
	}{
		{name: "a micro-dollar under", limit: "25.00", costs: []string{"20.000000", "4.999999"}, wantSpent: "24.999999", wantRefuse: false},
		{name: "exactly at", limit: "25.00", costs: []string{"20.000000", "5.000000"}, wantSpent: "25.000000", wantRefuse: true},
		{name: "a micro-dollar past", limit: "25.00", costs: []string{"20.000000", "5.000001"}, wantSpent: "25.000001", wantRefuse: true},
		{name: "exactly at, by costs a float sum falls short of", limit: "1.00", costs: []string{"0.700000", "0.200000", "0.100000"}, wantSpent: "1.000000", wantRefuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := guardTestSession(ctx, t, pool)
			repo := "acme/cap-" + sessionID.String()
			guardTestRepoCap(ctx, t, pool, repo, tc.limit)
			// Split over several turns, so the sum is what is compared.
			for _, cost := range tc.costs {
				guardTestTurn(ctx, t, pool, sessionID, true, cost)
			}

			facts, err := store.Facts(ctx, sessionID, []string{repo})
			if err != nil {
				t.Fatalf("Facts: %v", err)
			}
			if facts.SpentUSD != micro(t, tc.wantSpent) {
				t.Fatalf("spent = %s, want %s", facts.SpentUSD, tc.wantSpent)
			}
			_, refusal := sessionguard.Decide(facts, sessionguard.OriginPerson)
			if (refusal != nil) != tc.wantRefuse {
				t.Fatalf("spent %s against a cap of $%s: refused = %v, want %v", tc.wantSpent, tc.limit, refusal != nil, tc.wantRefuse)
			}
		})
	}
}

// TestSessionGuardFacts_StrictestRepositoryCap: a session naming several
// repositories is held to the strictest of their caps, named by its
// repository; a repository with no cap sets none.
func TestSessionGuardFacts_StrictestRepositoryCap(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSessionGuardStore(pool)

	sessionID := guardTestSession(ctx, t, pool)
	suffix := sessionID.String()
	loose, strict, uncapped := "acme/loose-"+suffix, "acme/strict-"+suffix, "acme/uncapped-"+suffix
	guardTestRepoCap(ctx, t, pool, loose, "50.00")
	guardTestRepoCap(ctx, t, pool, strict, "12.50")
	guardTestRepoCap(ctx, t, pool, uncapped, "")
	guardTestRepoCap(ctx, t, pool, "acme/unrelated-"+suffix, "0.01")

	facts, err := store.Facts(ctx, sessionID, []string{loose, uncapped, strict})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if facts.RepoCap == nil || *facts.RepoCap != micro(t, "12.50") || facts.RepoSource.Name != strict || facts.RepoSource.Kind != sessionguard.CapSourceRepo {
		t.Fatalf("repo cap = %v from %+v, want $12.50 from %s", facts.RepoCap, facts.RepoSource, strict)
	}
	if limit, source := facts.EffectiveCap(); limit == nil || *limit != micro(t, "12.50") || source.Name != strict {
		t.Fatalf("effective cap = %v from %+v", limit, source)
	}
}

// TestSessionGuardFacts_ForkReviewSessionTakesTheBaseRepositorysCap: a
// review session of a fork's pull request clones the fork, but its claim
// (github_pr_sessions) names the base repository, whose cap it takes.
func TestSessionGuardFacts_ForkReviewSessionTakesTheBaseRepositorysCap(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSessionGuardStore(pool)

	sessionID := guardTestSession(ctx, t, pool)
	suffix := sessionID.String()
	fork, base := "contributor/widgets-"+suffix, "acme/widgets-"+suffix
	guardTestRepoCap(ctx, t, pool, base, "3.00")
	if _, err := pool.Exec(ctx, `INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id) VALUES ($1, 41, $2)`, base, sessionID); err != nil {
		t.Fatal(err)
	}

	facts, err := store.Facts(ctx, sessionID, []string{fork})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if facts.RepoCap == nil || *facts.RepoCap != micro(t, "3.00") || facts.RepoSource.Name != base {
		t.Fatalf("repo cap = %v from %+v, want $3.00 from the base repository %s", facts.RepoCap, facts.RepoSource, base)
	}
}

// TestSessionGuardFacts_SentinelChildTakesItsRepositorysCap: a sentinel
// auto-fix child session's claim (sentinel_fixes.fix_child_session_id)
// names its repository, whose cap it takes even with no clone URL read.
func TestSessionGuardFacts_SentinelChildTakesItsRepositorysCap(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSessionGuardStore(pool)

	origin := guardTestSession(ctx, t, pool)
	child := guardTestSession(ctx, t, pool)
	repo := "acme/sentinel-" + child.String()
	guardTestRepoCap(ctx, t, pool, repo, "4.00")
	if _, err := pool.Exec(ctx, `INSERT INTO sentinel_fixes (repo_full_name, origin_pr_number, origin_review_session_id, origin_head_branch, fix_child_session_id)
		VALUES ($1, 7, $2, 'feature', $3)`, repo, origin, child); err != nil {
		t.Fatal(err)
	}

	facts, err := store.Facts(ctx, child, nil)
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if facts.RepoCap == nil || *facts.RepoCap != micro(t, "4.00") || facts.RepoSource.Name != repo {
		t.Fatalf("repo cap = %v from %+v, want $4.00 from %s", facts.RepoCap, facts.RepoSource, repo)
	}
}

// TestSessionGuardFacts_AutomationCapPrecedence: a session an automation
// created is held to the automation's cap when it sets one -- below or
// above its repository's -- and to its repository's when it sets none.
func TestSessionGuardFacts_AutomationCapPrecedence(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSessionGuardStore(pool)

	for _, tc := range []struct {
		name       string
		autoCap    string
		repoCap    string
		wantCap    string
		wantSource string
	}{
		{name: "automation below repository", autoCap: "2.00", repoCap: "10.00", wantCap: "2.00", wantSource: sessionguard.CapSourceAutomation},
		{name: "automation above repository", autoCap: "40.00", repoCap: "10.00", wantCap: "40.00", wantSource: sessionguard.CapSourceAutomation},
		{name: "automation without a cap", autoCap: "", repoCap: "10.00", wantCap: "10.00", wantSource: sessionguard.CapSourceRepo},
		{name: "automation only", autoCap: "7.00", repoCap: "", wantCap: "7.00", wantSource: sessionguard.CapSourceAutomation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := guardTestSession(ctx, t, pool)
			repo := "acme/auto-" + sessionID.String()
			guardTestRepoCap(ctx, t, pool, repo, tc.repoCap)
			automationID := guardTestAutomation(ctx, t, pool, sessionID, "nightly "+sessionID.String(), tc.autoCap)

			facts, err := store.Facts(ctx, sessionID, []string{repo})
			if err != nil {
				t.Fatalf("Facts: %v", err)
			}
			limit, source := facts.EffectiveCap()
			if limit == nil || *limit != micro(t, tc.wantCap) || source.Kind != tc.wantSource {
				t.Fatalf("effective cap = %v from %+v, want %s from %s", limit, source, tc.wantCap, tc.wantSource)
			}
			if source.Kind == sessionguard.CapSourceAutomation && (source.ID != automationID.String() || source.Name != "nightly "+sessionID.String()) {
				t.Fatalf("automation source = %+v, want automation %s", source, automationID.String())
			}
		})
	}
}

// TestCreateAndArmDispatch_RefusesWithoutAnAdmission: the store creates a
// turn only with the session guard's admission for that very session. The
// zero Admission and another session's are refused with ErrTurnNotAdmitted,
// writing no turn and arming no dispatch timer -- in a transaction, and
// through CreateLockedTurn, whose admission's refusal is returned as it
// came, the session's row lock released.
func TestCreateAndArmDispatch_RefusesWithoutAnAdmission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	turns := narvipg.NewTurnStore(pool)
	creator := narvipg.NewLockedTurnCreator(pool)
	prompt := "do the thing"

	for _, tc := range []struct {
		name      string
		admission func(self, other pgtype.UUID) sessionguard.Admission
		wantErr   error
	}{
		{name: "the zero admission", admission: func(_, _ pgtype.UUID) sessionguard.Admission { return sessionguard.Admission{} }, wantErr: narvipg.ErrTurnNotAdmitted},
		{name: "another session's admission", admission: func(_, other pgtype.UUID) sessionguard.Admission { return sessionguard.AdmitNewSession(other.Bytes) }, wantErr: narvipg.ErrTurnNotAdmitted},
		{name: "its own session's admission", admission: func(self, _ pgtype.UUID) sessionguard.Admission { return sessionguard.AdmitNewSession(self.Bytes) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			other := createTestSession(ctx, t, pool)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = turns.WithTx(tx).CreateAndArmDispatch(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt}, tc.admission(sessionID, other))
			if cerr := tx.Commit(ctx); cerr != nil {
				t.Fatal(cerr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateAndArmDispatch = %v, want %v", err, tc.wantErr)
			}
			wantTurns := 1
			if tc.wantErr != nil {
				wantTurns = 0
			}
			if n := countTurns(ctx, t, pool, sessionID); n != wantTurns {
				t.Fatalf("turns = %d, want %d", n, wantTurns)
			}
			if _, _, armed := dispatchTimerRow(ctx, t, pool, sessionID); armed != (tc.wantErr == nil) {
				t.Fatalf("dispatch timer armed = %v, want %v", armed, tc.wantErr == nil)
			}

			// The same admission through CreateLockedTurn.
			lockedSession := createTestSession(ctx, t, pool)
			admission := tc.admission(lockedSession, other)
			_, err = creator.CreateLockedTurn(ctx, sqlcgen.CreateTurnParams{SessionID: lockedSession, Status: sqlcgen.TurnStatusPending, Prompt: &prompt},
				func(context.Context, pgx.Tx) (sessionguard.Admission, error) { return admission, nil })
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateLockedTurn = %v, want %v", err, tc.wantErr)
			}
			if n := countTurns(ctx, t, pool, lockedSession); n != wantTurns {
				t.Fatalf("CreateLockedTurn turns = %d, want %d", n, wantTurns)
			}
		})
	}

	t.Run("a refusal from the admission writes nothing", func(t *testing.T) {
		sessionID := createTestSession(ctx, t, pool)
		refusal := &sessionguard.Refusal{Reason: sessionguard.ReasonSpendCap, SessionID: sessionID.Bytes}
		_, err := creator.CreateLockedTurn(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt},
			func(context.Context, pgx.Tx) (sessionguard.Admission, error) {
				return sessionguard.Admission{}, refusal
			})
		if got, ok := sessionguard.AsRefusal(err); !ok || got != refusal {
			t.Fatalf("CreateLockedTurn = %v, want the admission's refusal", err)
		}
		if n := countTurns(ctx, t, pool, sessionID); n != 0 {
			t.Fatalf("turns = %d after a refusal, want 0", n)
		}
		if _, _, armed := dispatchTimerRow(ctx, t, pool, sessionID); armed {
			t.Fatal("a refused turn armed the dispatch timer")
		}
		// The lock was released with the transaction: the session's row is
		// locked again at once.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE NOWAIT`, sessionID); err != nil {
			t.Fatalf("the session's row is still locked after a refused CreateLockedTurn: %v", err)
		}
	})
}
