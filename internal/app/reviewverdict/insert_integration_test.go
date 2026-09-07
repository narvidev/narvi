//go:build integration

// Integration test for §21's own write-path digest sanitization (G1)
// against a REAL Postgres instance -- this package (internal/app/
// reviewverdict) had no test file of any kind before this Step; this file
// adds one, mirroring internal/app/reviewtriage's own established
// newTestPool convention exactly (compute_integration_test.go's own doc
// comment: "each DB-touching package builds its own copy of newTestPool
// rather than sharing one across package boundaries"). Run via `make
// test-integration`.
package reviewverdict_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/migrations"
)

// newTestPool spins up a throwaway Postgres container, runs every embedded
// migration up, and returns a ready *pgxpool.Pool -- a duplicate of
// internal/app/reviewtriage's own newTestPool, necessarily so (this
// codebase's established per-package precedent, see that file's own doc
// comment).
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	startCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	const containerStartWatchdog = 2*time.Minute + 15*time.Second
	type containerStartResult struct {
		container *tcpostgres.PostgresContainer
		err       error
	}
	startCh := make(chan containerStartResult, 1)
	var startGroup errgroup.Group
	startGroup.Go(func() error {
		container, err := tcpostgres.Run(startCtx, "postgres:17-alpine",
			tcpostgres.WithDatabase("narvi_test"),
			tcpostgres.WithUsername("narvi"),
			tcpostgres.WithPassword("narvi"),
			tcpostgres.BasicWaitStrategies(),
		)
		startCh <- containerStartResult{container: container, err: err}
		return nil
	})

	var container *tcpostgres.PostgresContainer
	var err error
	select {
	case res := <-startCh:
		container, err = res.container, res.err
		if err != nil {
			t.Fatalf("start postgres container: %v", err)
		}
	case <-time.After(containerStartWatchdog):
		t.Fatalf("start postgres container: tcpostgres.Run did not return within %s", containerStartWatchdog)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminate container: %v", err)
		}
	})

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	migrateDB, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = migrateDB.Close() })

	dbDriver, err := migratepg.WithInstance(migrateDB, &migratepg.Config{})
	if err != nil {
		t.Fatalf("migratepg.WithInstance: %v", err)
	}
	srcDriver, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", srcDriver, "pgx", dbDriver)
	if err != nil {
		t.Fatalf("migrate.NewWithInstance: %v", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	pool, err := narvipg.NewPool(ctx, connStr)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

// allTenPlaceholderTokensForTest mirrors internal/domain/reviewpost's own
// identical test fixture (sanitize_test.go) -- see that file's own doc
// comment for why these are raw literals rather than imports of
// internal/domain/turn/internal/domain/upload.
var allTenPlaceholderTokensForTest = []string{
	review.VerdictToolURLPlaceholder,
	review.VerdictToolBearerPlaceholder,
	review.VerdictToolGenPlaceholder,
	review.ReviewCostBudgetToolURLPlaceholder,
	"{{EPISTEMIC_OUTCOME_TOOL_URL}}",
	"{{EPISTEMIC_OUTCOME_TOOL_BEARER}}",
	"{{EPISTEMIC_OUTCOME_TOOL_GEN}}",
	"{{UPLOAD_TOOL_BASE_URL}}",
	"{{UPLOAD_TOOL_BEARER}}",
	"{{UPLOAD_TOOL_GEN}}",
}

// TestInsert_AllTenPlaceholderTokensStrippedFromStoredDigest is this
// Step's own direct, end-to-end regression test (the task brief's own
// explicit ask: "a VerdictInput whose digest fields carry all ten
// placeholder literals, persisted and read back, asserting none survive
// in storage") -- against a REAL Postgres instance, not a mock: every
// model-authored free-text digest field carries ALL TEN placeholder
// tokens at once, Insert persists the verdict, GetLatest reads it back,
// and the test asserts NONE of the ten tokens survive anywhere in the
// read-back Digest.
func TestInsert_AllTenPlaceholderTokensStrippedFromStoredDigest(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)

	const repoFullName = "acme/digest-sanitization-repo"
	const prNumber = int32(42)
	const headSHA = "sha-digest-sanitization-1"

	poison := strings.Join(allTenPlaceholderTokensForTest, " and also ")

	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)

	digest := reviewpost.Digest{
		Summary:             poison,
		StackRisks:          poison,
		UnverifiedLimits:    poison,
		DescriptionAdequacy: review.DescriptionAdequacyOK,
		AdequacyExplanation: poison,
		ProposedBody:        poison,
		ContestedPoints:     poison,
		ArchDecisions: []reviewpost.ArchDecision{
			{Decision: poison, RejectedAlternative: poison, ConventionConformance: poison},
		},
	}

	if _, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettings, false, repoFullName, prNumber, headSHA, pgtype.UUID{}, verdict, digest, reviewtriage.DepthDeep, review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	deps := appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts}
	record, ok, err := appreviewverdict.GetLatest(ctx, deps, repoFullName, prNumber)
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if !ok {
		t.Fatalf("GetLatest: ok = false, want true (the row this test just inserted)")
	}

	fields := map[string]string{
		"Digest.Summary":             record.Digest.Summary,
		"Digest.StackRisks":          record.Digest.StackRisks,
		"Digest.UnverifiedLimits":    record.Digest.UnverifiedLimits,
		"Digest.AdequacyExplanation": record.Digest.AdequacyExplanation,
		"Digest.ProposedBody":        record.Digest.ProposedBody,
		"Digest.ContestedPoints":     record.Digest.ContestedPoints,
	}
	if len(record.Digest.ArchDecisions) != 1 {
		t.Fatalf("record.Digest.ArchDecisions has %d entries, want 1", len(record.Digest.ArchDecisions))
	}
	fields["ArchDecisions[0].Decision"] = record.Digest.ArchDecisions[0].Decision
	fields["ArchDecisions[0].RejectedAlternative"] = record.Digest.ArchDecisions[0].RejectedAlternative
	fields["ArchDecisions[0].ConventionConformance"] = record.Digest.ArchDecisions[0].ConventionConformance

	for fieldName, val := range fields {
		for _, tok := range allTenPlaceholderTokensForTest {
			if strings.Contains(val, tok) {
				t.Errorf("read-back record.%s still contains placeholder token %q -- want it stripped at the WRITE path (reviewpost.SanitizeDigest, called from Insert) before it ever reached storage; stored value: %q", fieldName, tok, val)
			}
		}
		if val == "" {
			t.Errorf("read-back record.%s is empty -- want it non-empty (the sanitized-but-still-present remainder of the poisoned fixture), which would mean this test is vacuously passing", fieldName)
		}
	}
}
