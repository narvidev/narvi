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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
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

	if _, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettings, false, repoFullName, prNumber, headSHA, pgtype.UUID{}, verdict, digest, reviewtriage.DepthDeep, review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false, reviewverdict.Context{}, pgtype.UUID{}); err != nil {
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

// TestInsert_ContextAndAttemptIDRoundTrip (§21.1's amendment) proves the
// new base_ref/base_sha/ancestor_chain/policy_version/attempt_id columns
// persist and read back verbatim against a REAL Postgres instance -- the
// exact round trip internal/domain/autoapproval.ComputeEligible's own
// context-freshness comparison depends on. Two rows: one with a REAL,
// fully-populated Context (an ordinary stacked-PR review) and one with
// the ZERO-VALUE Context (mirroring a pre-amendment row, or a review
// turn whose own context-fetch never resolved a base ref) -- proving
// both directions read back exactly as this Step's own backfill decision
// requires: a real context round-trips exactly, and an absent one reads
// back as BaseRef == "" (autoapproval's own "unknown, never a match"
// signal), never silently coerced into looking like either.
func TestInsert_ContextAndAttemptIDRoundTrip(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)

	// A real turn row -- attempt_id's own FK (migrations/
	// 000130_review_verdicts_context.up.sql: "ON DELETE SET NULL") means a
	// caller cannot honestly stamp a made-up UUID here; this is the
	// minimal real (session, turn) pair the FK requires.
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	turn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}

	baseVerdict := func() review.Verdict {
		v := review.Verdict{
			RiskLevel:         review.RiskLevelLow,
			Premise:           review.PremiseStateOK,
			TestsCoverage:     review.TestsCoverageStateAdequate,
			DocsDrift:         review.DocsDriftStateNone,
			ProposedShippable: review.ProposedShippableAuto,
		}
		v.Shippable = review.ComputeShippable(v.RiskLevel, v.TestsCoverage, v.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
		return v
	}
	digest := reviewpost.Digest{Summary: "Context round-trip test-seeded verdict.", DescriptionAdequacy: review.DescriptionAdequacyOK, AdequacyExplanation: "n/a"}

	t.Run("a real context and attempt id round-trip exactly", func(t *testing.T) {
		const repoFullName = "acme/context-roundtrip-repo"
		const prNumber = int32(1)
		const headSHA = "sha-context-roundtrip-1"

		wantContext := reviewverdict.Context{
			BaseRef:       "feature/parent-pr-branch",
			BaseSHA:       "sha-parent-head",
			AncestorChain: []review.AncestorLink{{Ref: "main", SHA: "sha-main-tip"}},
			PolicyVersion: autoapproval.CurrentPolicyVersion,
		}

		if _, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettings, false, repoFullName, prNumber, headSHA, pgtype.UUID{}, baseVerdict(), digest, reviewtriage.DepthLight, "", reviewpost.FactCheckSkipped, 0, nil, nil, "", false, wantContext, turn.ID); err != nil {
			t.Fatalf("Insert: %v", err)
		}

		record, ok, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts}, repoFullName, prNumber)
		if err != nil {
			t.Fatalf("GetLatest: %v", err)
		}
		if !ok {
			t.Fatalf("GetLatest: ok = false, want true")
		}
		if record.Context.BaseRef != wantContext.BaseRef {
			t.Errorf("record.Context.BaseRef = %q, want %q", record.Context.BaseRef, wantContext.BaseRef)
		}
		if record.Context.BaseSHA != wantContext.BaseSHA {
			t.Errorf("record.Context.BaseSHA = %q, want %q", record.Context.BaseSHA, wantContext.BaseSHA)
		}
		if record.Context.PolicyVersion != wantContext.PolicyVersion {
			t.Errorf("record.Context.PolicyVersion = %d, want %d", record.Context.PolicyVersion, wantContext.PolicyVersion)
		}
		if !slices.Equal(record.Context.AncestorChain, wantContext.AncestorChain) {
			t.Errorf("record.Context.AncestorChain = %+v, want %+v", record.Context.AncestorChain, wantContext.AncestorChain)
		}
		wantAttemptID := uuid.UUID(turn.ID.Bytes).String()
		if record.AttemptID != wantAttemptID {
			t.Errorf("record.AttemptID = %q, want %q (the seeded turn's own id)", record.AttemptID, wantAttemptID)
		}
	})

	t.Run("a zero-value context (a pre-amendment row) reads back with an empty base ref, never a fabricated match", func(t *testing.T) {
		const repoFullName = "acme/context-roundtrip-repo"
		const prNumber = int32(2)
		const headSHA = "sha-context-roundtrip-2"

		if _, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettings, false, repoFullName, prNumber, headSHA, pgtype.UUID{}, baseVerdict(), digest, reviewtriage.DepthLight, "", reviewpost.FactCheckSkipped, 0, nil, nil, "", false, reviewverdict.Context{}, pgtype.UUID{}); err != nil {
			t.Fatalf("Insert: %v", err)
		}

		record, ok, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts}, repoFullName, prNumber)
		if err != nil {
			t.Fatalf("GetLatest: %v", err)
		}
		if !ok {
			t.Fatalf("GetLatest: ok = false, want true")
		}
		if record.Context.BaseRef != "" {
			t.Errorf("record.Context.BaseRef = %q, want empty (an absent/never-recorded context)", record.Context.BaseRef)
		}
		if record.Context.PolicyVersion != 0 {
			t.Errorf("record.Context.PolicyVersion = %d, want 0 (never equal to autoapproval.CurrentPolicyVersion=%d by coincidence)", record.Context.PolicyVersion, autoapproval.CurrentPolicyVersion)
		}
		if len(record.Context.AncestorChain) != 0 {
			t.Errorf("record.Context.AncestorChain = %+v, want empty", record.Context.AncestorChain)
		}
		if record.AttemptID != "" {
			t.Errorf("record.AttemptID = %q, want empty (no attempt was ever recorded)", record.AttemptID)
		}
	})
}

// TestGetLatest_OrdersByProducingAttemptRecency_NeverPostTime is round-10
// finding D's own regression test: GetLatest used to order strictly by
// review_verdicts.created_at DESC -- "whichever request landed last" --
// so two attempts in flight for one PR made the AUTHORITATIVE verdict
// whichever one's own INSERT happened to commit last, never necessarily
// the one belonging to the more recent attempt. This reproduces the exact
// race: an OLDER attempt (turn created first) whose own HTTP POST reaches
// review_verdicts AFTER a NEWER attempt's (turn created second, but posts
// FIRST) -- proving GetLatest still returns the newer attempt's own
// verdict, never the one that merely committed last.
//
// Mutation-test target: reverting queries/reviewverdicts.sql's own
// GetLatestReviewVerdict query from `ORDER BY COALESCE(t.created_at,
// rv.created_at) DESC` back to `ORDER BY created_at DESC` (and
// regenerating sqlcgen) must turn this test's own "record.HeadSHA ==
// newer-attempt-sha" assertion into "record.HeadSHA ==
// older-attempt-sha".
func TestGetLatest_OrdersByProducingAttemptRecency_NeverPostTime(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)

	const repoFullName = "acme/attempt-recency-repo"
	const prNumber = int32(1)

	newSession := func() sqlcgen.Session {
		s, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		return s
	}

	// olderTurn is created FIRST -- an EARLIER attempt.
	olderSession := newSession()
	olderTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: olderSession.ID, Status: sqlcgen.TurnStatusCompleted})
	if err != nil {
		t.Fatalf("create older turn: %v", err)
	}
	// newerTurn is created SECOND -- Postgres now() has real sub-millisecond
	// resolution and these are two sequential, real INSERTs, so
	// newerTurn.CreatedAt > olderTurn.CreatedAt holds reliably in
	// practice (no sleep needed, no test-controlled Clock exists at this
	// layer to fake it more directly).
	newerSession := newSession()
	newerTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: newerSession.ID, Status: sqlcgen.TurnStatusCompleted})
	if err != nil {
		t.Fatalf("create newer turn: %v", err)
	}
	if !newerTurn.CreatedAt.Time.After(olderTurn.CreatedAt.Time) {
		t.Fatalf("newerTurn.CreatedAt = %v, want it strictly after olderTurn.CreatedAt = %v -- test setup assumption broken", newerTurn.CreatedAt.Time, olderTurn.CreatedAt.Time)
	}

	verdict := func() review.Verdict {
		v := review.Verdict{
			RiskLevel:         review.RiskLevelLow,
			Premise:           review.PremiseStateOK,
			TestsCoverage:     review.TestsCoverageStateAdequate,
			DocsDrift:         review.DocsDriftStateNone,
			ProposedShippable: review.ProposedShippableAuto,
		}
		v.Shippable = review.ComputeShippable(v.RiskLevel, v.TestsCoverage, v.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
		return v
	}
	digest := reviewpost.Digest{Summary: "Attempt-recency test-seeded verdict.", DescriptionAdequacy: review.DescriptionAdequacyOK, AdequacyExplanation: "n/a"}

	// The NEWER attempt's own verdict is INSERTED FIRST -- an earlier
	// review_verdicts.created_at than what follows.
	if _, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettings, false, repoFullName, prNumber, "sha-newer-attempt", pgtype.UUID{}, verdict(), digest, reviewtriage.DepthLight, "", reviewpost.FactCheckSkipped, 0, nil, nil, "", false, reviewverdict.Context{}, newerTurn.ID); err != nil {
		t.Fatalf("insert newer attempt's verdict: %v", err)
	}
	// The OLDER attempt's own verdict is INSERTED SECOND -- reaching the
	// database AFTER the newer attempt's own, exactly the race finding D
	// names: "an older attempt's own POST landing at the database AFTER a
	// newer attempt's must not win this read". Its review_verdicts.created_at
	// is therefore the LATER of the two.
	if _, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettings, false, repoFullName, prNumber, "sha-older-attempt", pgtype.UUID{}, verdict(), digest, reviewtriage.DepthLight, "", reviewpost.FactCheckSkipped, 0, nil, nil, "", false, reviewverdict.Context{}, olderTurn.ID); err != nil {
		t.Fatalf("insert older attempt's verdict (landing at the DB second): %v", err)
	}

	record, ok, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts}, repoFullName, prNumber)
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if !ok {
		t.Fatalf("GetLatest: ok = false, want true")
	}
	if record.HeadSHA != "sha-newer-attempt" {
		t.Errorf("GetLatest() returned head_sha = %q, want %q -- the NEWER attempt's own verdict must win even though the OLDER attempt's own row was inserted (committed) last", record.HeadSHA, "sha-newer-attempt")
	}
	wantAttemptID := uuid.UUID(newerTurn.ID.Bytes).String()
	if record.AttemptID != wantAttemptID {
		t.Errorf("GetLatest() returned attempt_id = %q, want %q (the newer attempt's own turn id)", record.AttemptID, wantAttemptID)
	}
}

// TestGetLatestAndListLatestAutoApproved_AgreeOnAttemptRecency is round-11
// finding B's own regression test: ListLatestAutoApprovedInRepo (the
// automerge worker's own discovery query, internal/app/automerge) used to
// reduce "latest per PR" by review_verdicts.created_at (post time) ALONE
// -- a DIFFERENT reduction than GetLatestReviewVerdict/GetLatest above,
// which order by the PRODUCING ATTEMPT's own creation time (round-10
// finding D). Two components deciding about the SAME pull request from
// two DIFFERENT "latest" verdicts is exactly the failure this closes: the
// decision inbox and the eligibility engine's own callers read via
// GetLatest, but the worker that actually ARMS auto-merge discovers
// candidates via ListLatestAutoApproved -- if the two ever disagreed
// about which verdict is "latest" for the same PR, one component could
// treat a PR as freshly reviewed while the other treats an entirely
// different verdict as the one of record. This reproduces the EXACT
// SAME race TestGetLatest_OrdersByProducingAttemptRecency_NeverPostTime
// does (an older attempt's own post landing at the database AFTER a
// newer attempt's), then proves BOTH GetLatest AND ListLatestAutoApproved
// agree on which verdict is authoritative.
//
// Mutation-test target: reverting queries/reviewverdicts.sql's own
// ListLatestAutoApprovedInRepo inner DISTINCT ON from `ORDER BY
// rv.repo_full_name, rv.pr_number, COALESCE(t.created_at, rv.created_at)
// DESC, rv.created_at DESC, rv.id DESC` back to `ORDER BY
// rv.repo_full_name, rv.pr_number, rv.created_at DESC` (and regenerating
// sqlcgen) must turn this test's own "candidate.HeadSha ==
// newer-attempt-sha" assertion into "candidate.HeadSha ==
// older-attempt-sha", disagreeing with GetLatest's own unchanged answer.
func TestGetLatestAndListLatestAutoApproved_AgreeOnAttemptRecency(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)

	const repoFullName = "acme/attempt-recency-auto-merge-repo"
	const prNumber = int32(2)

	// ListLatestAutoApproved (unlike GetLatest) filters out any verdict
	// stamped suppressed_in_shadow -- egressmode.Resolve's own default for
	// a repo with NO repo_settings row at all is shadowCapability()
	// (resolve.go), so this repo must be explicitly promoted to live
	// egress first, or appreviewverdict.Insert below would stamp both
	// seeded verdicts shadow and ListLatestAutoApproved would legitimately
	// (and uninterestingly) return zero candidates for an unrelated
	// reason.
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}

	newSession := func() sqlcgen.Session {
		s, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		return s
	}

	olderSession := newSession()
	olderTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: olderSession.ID, Status: sqlcgen.TurnStatusCompleted})
	if err != nil {
		t.Fatalf("create older turn: %v", err)
	}
	newerSession := newSession()
	newerTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: newerSession.ID, Status: sqlcgen.TurnStatusCompleted})
	if err != nil {
		t.Fatalf("create newer turn: %v", err)
	}
	if !newerTurn.CreatedAt.Time.After(olderTurn.CreatedAt.Time) {
		t.Fatalf("newerTurn.CreatedAt = %v, want it strictly after olderTurn.CreatedAt = %v -- test setup assumption broken", newerTurn.CreatedAt.Time, olderTurn.CreatedAt.Time)
	}

	verdict := func() review.Verdict {
		v := review.Verdict{
			RiskLevel:         review.RiskLevelLow,
			Premise:           review.PremiseStateOK,
			TestsCoverage:     review.TestsCoverageStateAdequate,
			DocsDrift:         review.DocsDriftStateNone,
			ProposedShippable: review.ProposedShippableAuto,
		}
		v.Shippable = review.ComputeShippable(v.RiskLevel, v.TestsCoverage, v.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
		return v
	}
	digest := reviewpost.Digest{Summary: "Attempt-recency auto-merge-agreement test-seeded verdict.", DescriptionAdequacy: review.DescriptionAdequacyOK, AdequacyExplanation: "n/a"}

	// The NEWER attempt's own verdict is INSERTED FIRST -- an earlier
	// review_verdicts.created_at than what follows.
	if _, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettings, false, repoFullName, prNumber, "sha-newer-attempt-auto", pgtype.UUID{}, verdict(), digest, reviewtriage.DepthLight, "", reviewpost.FactCheckSkipped, 0, nil, nil, "", false, reviewverdict.Context{}, newerTurn.ID); err != nil {
		t.Fatalf("insert newer attempt's verdict: %v", err)
	}
	// The OLDER attempt's own verdict is INSERTED SECOND -- reaching the
	// database AFTER the newer attempt's own.
	if _, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettings, false, repoFullName, prNumber, "sha-older-attempt-auto", pgtype.UUID{}, verdict(), digest, reviewtriage.DepthLight, "", reviewpost.FactCheckSkipped, 0, nil, nil, "", false, reviewverdict.Context{}, olderTurn.ID); err != nil {
		t.Fatalf("insert older attempt's verdict (landing at the DB second): %v", err)
	}

	record, ok, err := appreviewverdict.GetLatest(ctx, appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts}, repoFullName, prNumber)
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if !ok {
		t.Fatalf("GetLatest: ok = false, want true")
	}
	if record.HeadSHA != "sha-newer-attempt-auto" {
		t.Fatalf("GetLatest() returned head_sha = %q, want %q", record.HeadSHA, "sha-newer-attempt-auto")
	}

	// ListLatestAutoApproved is internal/app/automerge's own discovery
	// query -- must agree with GetLatest above about which verdict is
	// "latest" for this exact PR, never reduce by post time alone.
	since := time.Now().Add(-24 * time.Hour)
	candidates, err := reviewVerdicts.ListLatestAutoApproved(ctx, repoFullName, pgtype.Timestamptz{Time: since, Valid: true}, 20)
	if err != nil {
		t.Fatalf("ListLatestAutoApproved: %v", err)
	}
	var candidate *sqlcgen.ReviewVerdict
	for i := range candidates {
		if candidates[i].PrNumber == prNumber {
			candidate = &candidates[i]
			break
		}
	}
	if candidate == nil {
		t.Fatalf("ListLatestAutoApproved returned no candidate for pr_number=%d, want exactly one (both seeded verdicts are Shippable=auto)", prNumber)
	}
	if candidate.HeadSha != "sha-newer-attempt-auto" {
		t.Errorf("ListLatestAutoApproved() returned head_sha = %q, want %q -- it must agree with GetLatest on which verdict is authoritative for this PR (the NEWER attempt's, never the OLDER attempt's own row that merely committed last)", candidate.HeadSha, "sha-newer-attempt-auto")
	}
}
