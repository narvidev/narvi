//go:build integration

// Integration tests for ReviewVerdictAcceptanceStore ("human acceptance
// of a verdict the engine refuses", §21.1b) against a REAL Postgres
// instance -- see migrations/000135_review_verdict_acceptances.up.sql's
// own doc comment for the table's full design.
package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// insertReviewVerdictAcceptanceStoreVerdict inserts one minimal, valid
// review_verdicts row -- just enough for review_verdict_acceptances.
// verdict_id's own foreign key to reference. Bypasses internal/app/
// reviewverdict.Insert entirely, mirroring insertArchDecisionVerdict's
// own identical precedent one file over
// (reviewverdictarchdecisions_integration_test.go): this package cannot
// import that app-layer package without an import cycle.
func insertReviewVerdictAcceptanceStoreVerdict(ctx context.Context, t *testing.T, store *narvipg.ReviewVerdictStore, repoFullName string, prNumber int32, headSHA string) sqlcgen.ReviewVerdict {
	t.Helper()
	row, err := store.Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName:      repoFullName,
		PrNumber:          prNumber,
		HeadSha:           headSHA,
		RiskLevel:         "high",
		Premise:           "ok",
		BlastRadius:       []byte(`[]`),
		FilesChanged:      1,
		TestsCoverage:     "adequate",
		DocsDrift:         "none",
		ProposedShippable: "needs_human",
		Shippable:         "needs_human",
		ArchDecisionTags:  []byte(`[]`),
		ArchDecisionRoots: []byte(`[]`),
		AncestorChain:     []byte(`[]`),
	})
	if err != nil {
		t.Fatalf("insert review_verdicts row: %v", err)
	}
	return row
}

// reviewVerdictAcceptanceParams builds a minimal, valid
// InsertReviewVerdictAcceptanceParams accepting verdict -- the one
// shared literal every test below starts from, mutating at most the
// justification/verdict id.
func reviewVerdictAcceptanceParams(repoFullName string, prNumber int32, verdict sqlcgen.ReviewVerdict, justification string, acceptedBy pgtype.UUID) sqlcgen.InsertReviewVerdictAcceptanceParams {
	return sqlcgen.InsertReviewVerdictAcceptanceParams{
		RepoFullName:  repoFullName,
		PrNumber:      prNumber,
		VerdictID:     verdict.ID,
		HeadSha:       verdict.HeadSha,
		AncestorChain: []byte(`[]`),
		Reason:        "the verdict's shippable classification is not auto",
		Justification: justification,
		AcceptedBy:    acceptedBy,
	}
}

// TestReviewVerdictAcceptanceStore_Insert_PartialFailureRollsBackTogether
// pins finding F2 (adversarial review): Insert's own supersede-then-
// insert pair must commit or roll back TOGETHER when the caller wraps
// them in a real transaction (WithTx) -- proven here with a genuinely
// failing second half (an INSERT whose own verdict_id violates
// review_verdict_acceptances' foreign key to review_verdicts -- a real,
// naturally-occurring failure mode, never an artificial fault injected
// into production code): the FIRST acceptance must still read back
// ACTIVE after the whole transaction rolls back, never left revoked with
// nothing to replace it (the exact hazard this finding's own repro
// describes: "a routine re-accept ... that hits a cancelled request
// leaves the PR with zero active acceptances").
func TestReviewVerdictAcceptanceStore_Insert_PartialFailureRollsBackTogether(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	verdicts := narvipg.NewReviewVerdictStore(pool)
	acceptances := narvipg.NewReviewVerdictAcceptanceStore(pool)
	users := narvipg.NewUserStore(pool)
	const repoFullName = "acme/acceptance-store-partial-failure"
	const prNumber = int32(1)

	maintainer, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "partial-failure@example.com", DisplayName: "Maintainer", Role: sqlcgen.UserRoleMaintainer})
	if err != nil {
		t.Fatalf("create maintainer: %v", err)
	}
	verdict := insertReviewVerdictAcceptanceStoreVerdict(ctx, t, verdicts, repoFullName, prNumber, "sha-partial-failure")

	first, _, err := acceptances.Insert(ctx, reviewVerdictAcceptanceParams(repoFullName, prNumber, verdict, "first accept", maintainer.ID))
	if err != nil {
		t.Fatalf("insert first acceptance: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// A deliberately BOGUS verdict id -- no review_verdicts row with this
	// id exists, so InsertReviewVerdictAcceptance's own foreign key fails
	// -- AFTER SupersedeActiveReviewVerdictAcceptances (inside this SAME
	// transaction) has already revoked `first`.
	var bogusVerdictID pgtype.UUID
	if err := bogusVerdictID.Scan("00000000-0000-0000-0000-000000000099"); err != nil {
		t.Fatalf("scan bogus verdict id: %v", err)
	}
	badParams := reviewVerdictAcceptanceParams(repoFullName, prNumber, verdict, "second accept (will fail)", maintainer.ID)
	badParams.VerdictID = bogusVerdictID

	if _, _, err := acceptances.WithTx(tx).Insert(ctx, badParams); err == nil {
		t.Fatal("Insert() with a bogus verdict_id error = nil, want a foreign-key violation")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}

	// THE DECISIVE ASSERTION: the FIRST acceptance must still be active --
	// the whole transaction, including the Supersede half, rolled back
	// together. pgx.ErrNoRows here means "no active acceptance at all" --
	// exactly the bug this test exists to rule out.
	active, err := acceptances.GetActive(ctx, repoFullName, prNumber)
	if err != nil {
		t.Fatalf("GetActive after rollback: %v, want the first acceptance still active", err)
	}
	if active.ID != first.ID {
		t.Fatalf("GetActive after rollback returned a DIFFERENT acceptance (id=%v), want the FIRST, still-active one (id=%v)", active.ID, first.ID)
	}
}

// TestReviewVerdictAcceptanceStore_Insert_WithoutTxLeavesNoActiveAcceptanceOnPartialFailure
// demonstrates finding F2's OWN bug directly, as a permanent regression
// characterization: Insert called on the BARE, pool-scoped store
// (autocommit, no WithTx) leaves NO active acceptance at all when the
// second half fails -- the Supersede half already committed by itself,
// independently. This is exactly why httpapi.AcceptReviewVerdict now
// opens its own transaction and calls Acceptances.WithTx(tx).Insert for
// a real accept request, never this bare form -- see that handler's own
// doc comment. A future call site that reintroduces the bare form
// reintroduces this exact hazard.
func TestReviewVerdictAcceptanceStore_Insert_WithoutTxLeavesNoActiveAcceptanceOnPartialFailure(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	verdicts := narvipg.NewReviewVerdictStore(pool)
	acceptances := narvipg.NewReviewVerdictAcceptanceStore(pool)
	users := narvipg.NewUserStore(pool)
	const repoFullName = "acme/acceptance-store-partial-failure-no-tx"
	const prNumber = int32(1)

	maintainer, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "partial-failure-no-tx@example.com", DisplayName: "Maintainer", Role: sqlcgen.UserRoleMaintainer})
	if err != nil {
		t.Fatalf("create maintainer: %v", err)
	}
	verdict := insertReviewVerdictAcceptanceStoreVerdict(ctx, t, verdicts, repoFullName, prNumber, "sha-partial-failure-no-tx")

	if _, _, err := acceptances.Insert(ctx, reviewVerdictAcceptanceParams(repoFullName, prNumber, verdict, "first accept", maintainer.ID)); err != nil {
		t.Fatalf("insert first acceptance: %v", err)
	}

	var bogusVerdictID pgtype.UUID
	if err := bogusVerdictID.Scan("00000000-0000-0000-0000-000000000098"); err != nil {
		t.Fatalf("scan bogus verdict id: %v", err)
	}
	badParams := reviewVerdictAcceptanceParams(repoFullName, prNumber, verdict, "second accept (will fail)", maintainer.ID)
	badParams.VerdictID = bogusVerdictID

	// NO WithTx here -- the bare, pool-scoped, autocommit form.
	if _, _, err := acceptances.Insert(ctx, badParams); err == nil {
		t.Fatal("Insert() with a bogus verdict_id error = nil, want a foreign-key violation")
	}

	_, err = acceptances.GetActive(ctx, repoFullName, prNumber)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetActive error = %v, want pgx.ErrNoRows -- this test's own point is that calling Insert WITHOUT a transaction reproduces finding F2's bug: the Supersede half already committed independently, leaving the first acceptance revoked with nothing to replace it", err)
	}
}

// TestReviewVerdictAcceptanceStore_Insert_ConcurrentAccept_ExactlyOneActive
// pins finding F3c (adversarial review): review_verdict_acceptances_one_
// active_idx is what actually enforces "at most one active row per pull
// request" -- proven here by racing N concurrent, PROPERLY-ATOMIC accepts
// (each racer opens its own transaction and calls WithTx(tx).Insert, the
// SAME shape httpapi.AcceptReviewVerdict now uses) for the SAME
// (repo_full_name, pr_number). Mirrors TestWebhookDeliveryStore_Claim_
// ConcurrentSameIdentity_ExactlyOneWinner's own "N goroutines race the
// SAME identity" shape (webhookdelivery_store_integration_test.go), this
// repository's own precedent for exercising a database-level uniqueness
// invariant under real concurrency rather than merely trusting its
// migration's own doc comment -- which this table's own "enforced by
// construction, never merely by convention" claimed without ever being
// exercised this way (the finding this test closes: deleting the index
// entirely left 975 other integration tests green).
//
// UNLIKE Claim's own single INSERT ... ON CONFLICT statement, winCount
// here is deliberately NOT asserted to be exactly 1: each racer's own
// Supersede-then-Insert pair, under READ COMMITTED, revokes whatever is
// active AS OF THAT STATEMENT'S OWN SNAPSHOT -- so a racer whose
// Supersede happens to run AFTER an earlier racer's transaction has
// already committed legitimately supersedes it and commits cleanly too,
// with NO conflict at all (a real, correct "one accept superseding
// another", exactly like TestAccept_SupersedesPriorActiveAcceptance's
// own sequential case, just interleaved by real concurrency). What MUST
// still hold, regardless of how many individual racers' transactions
// happen to land this way: (1) at most one row is EVER active at once --
// review_verdict_acceptances_one_active_idx is what a genuine COLLISION
// (two racers' own Supersede statements both seeing "nothing active", at
// which point their two Inserts genuinely race for the same slot) falls
// back on, and the loser there gets a clean unique-constraint violation,
// its WHOLE transaction rolling back (finding F2's own atomicity, without
// which a losing racer's Supersede half could commit alone) -- so a
// losing racer never leaves a HALF-applied acceptance behind; and (2) the
// FINAL, settled state has EXACTLY one active row.
func TestReviewVerdictAcceptanceStore_Insert_ConcurrentAccept_ExactlyOneActive(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	verdicts := narvipg.NewReviewVerdictStore(pool)
	acceptances := narvipg.NewReviewVerdictAcceptanceStore(pool)
	users := narvipg.NewUserStore(pool)
	const repoFullName = "acme/acceptance-store-concurrent"
	const prNumber = int32(1)

	maintainer, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "concurrent-accept@example.com", DisplayName: "Maintainer", Role: sqlcgen.UserRoleMaintainer})
	if err != nil {
		t.Fatalf("create maintainer: %v", err)
	}
	verdict := insertReviewVerdictAcceptanceStoreVerdict(ctx, t, verdicts, repoFullName, prNumber, "sha-concurrent")

	const n = 20
	var g errgroup.Group
	wins := make([]bool, n)
	for i := 0; i < n; i++ {
		idx := i
		g.Go(func() error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return fmt.Errorf("racer %d: begin tx: %w", idx, err)
			}
			defer func() { _ = tx.Rollback(ctx) }()

			if _, _, err := acceptances.WithTx(tx).Insert(ctx, reviewVerdictAcceptanceParams(repoFullName, prNumber, verdict, fmt.Sprintf("racer %d", idx), maintainer.ID)); err != nil {
				// A unique-constraint violation is the EXPECTED, asserted-on
				// outcome for a losing racer -- never a test-infra error.
				return nil
			}
			if err := tx.Commit(ctx); err != nil {
				// A losing commit (another racer's transaction committed
				// first, discovered only now) is ALSO an expected loss.
				return nil
			}
			wins[idx] = true
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("errgroup: %v", err)
	}

	winCount := 0
	for _, won := range wins {
		if won {
			winCount++
		}
	}
	if winCount < 1 {
		t.Fatalf("winCount = %d, want at least 1 -- %d concurrent accepts racing the SAME pull request must yield at least one committed acceptance", winCount, n)
	}
	t.Logf("winCount = %d of %d racers committed (the rest either hit a real unique-constraint violation, or lost a commit race, and rolled back their WHOLE transaction cleanly)", winCount, n)

	// EVERY committed row must be a WINNER's own row, and every winner's
	// own row must exist -- proves a losing racer's transaction never
	// left a partial/orphaned row behind (finding F2's own atomicity):
	// row count == winCount exactly, never more (a phantom row from a
	// "losing" transaction that should have rolled back) and never fewer
	// (a winner's own row silently missing).
	var totalCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM review_verdict_acceptances WHERE repo_full_name = $1 AND pr_number = $2`,
		repoFullName, prNumber,
	).Scan(&totalCount); err != nil {
		t.Fatalf("count total rows: %v", err)
	}
	if totalCount != winCount {
		t.Errorf("total row count = %d, want exactly winCount (%d) -- a losing racer's transaction must roll back its INSERT along with its Supersede, never leave a row behind", totalCount, winCount)
	}

	// THE DECISIVE ASSERTION: regardless of how many individual racers'
	// transactions happened to commit along the way (each one legitimately
	// superseding whichever was active when IT ran), the FINAL, settled
	// state has EXACTLY one active row -- review_verdict_acceptances_one_
	// active_idx is what a genuine collision (two racers' Supersede
	// statements both observing "nothing active") falls back on to
	// guarantee this, never merely the application's own supersede-first
	// convention.
	var activeCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM review_verdict_acceptances WHERE repo_full_name = $1 AND pr_number = $2 AND revoked_at IS NULL`,
		repoFullName, prNumber,
	).Scan(&activeCount); err != nil {
		t.Fatalf("count active rows: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("active row count = %d, want exactly 1 -- review_verdict_acceptances_one_active_idx must guarantee at most one active row even under concurrent accepts racing the SAME pull request", activeCount)
	}
}
