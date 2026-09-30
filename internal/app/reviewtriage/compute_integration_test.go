//go:build integration

// Integration tests for internal/app/reviewtriage against a REAL Postgres
// instance -- gated behind the "integration" build tag, mirroring
// internal/app/actorauthz's own testcontainers-Postgres-plus-embedded-
// migrations convention exactly (each DB-touching package builds its own
// copy of newTestPool rather than sharing one across package boundaries).
// Run via `make test-integration`.
//
// The "already-rolled-back tx" fault-injection idiom below (brokenTx)
// mirrors internal/app/decisioninbox's own
// TestBuild_CredentialResolutionErrorDegradesRatherThanRenderingNoGitHub
// precedent: a store built via .WithTx(tx) on a tx that has already been
// rolled back fails every subsequent query with a genuine Postgres error
// ("tx is closed"), standing in for a real store outage without needing
// to fake an interface -- Deps' own fields are concrete *postgres.XStore
// types, not interfaces, so this is the one available fault-injection
// mechanism.
package reviewtriage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/review"
	domainreviewtriage "github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/migrations"
)

// newTestPool spins up a throwaway Postgres container, runs every embedded
// migration up, and returns a ready *pgxpool.Pool -- a duplicate of
// internal/app/actorauthz's own newTestPool, necessarily so (this
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

// brokenTxReal returns an already-rolled-back transaction -- any query
// run through a store built via .WithTx(brokenTxReal(...)) fails with a
// genuine Postgres error, standing in for a real store outage (this
// file's own top doc comment).
func brokenTxReal(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}
	return tx
}

func repoFullNameForTest(t *testing.T) string {
	t.Helper()
	return "acme/widgets-" + t.Name()
}

// TestComputeDecision_BasicRouting proves ComputeDecision correctly wires
// LoadConfig + the "prior high verdict" read + Decide together against a
// real, empty (never-configured) repo: a sensitive-glob-touching diff
// routes deep with no repo_settings row and no prior verdict at all.
func TestComputeDecision_BasicRouting(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repoFullName := repoFullNameForTest(t)

	deps := reviewtriage.Deps{
		RepoSettings:   narvipg.NewRepoSettingsStore(pool),
		ReviewVerdicts: narvipg.NewReviewVerdictStore(pool),
	}

	prCtx := review.PreFetchedContext{InputRead: review.InputReadComplete, ChangedPaths: []string{"migrations/000099_x.up.sql"}}
	decision, cfg, _ := reviewtriage.ComputeDecision(ctx, deps, repoFullName, 1, prCtx)

	if decision.Depth != domainreviewtriage.DepthDeep {
		t.Errorf("Depth = %q, want deep", decision.Depth)
	}
	if decision.Reason != domainreviewtriage.ReasonSensitiveGlob {
		t.Errorf("Reason = %q, want %q", decision.Reason, domainreviewtriage.ReasonSensitiveGlob)
	}
	if cfg.Mode != domainreviewtriage.ModeAuto {
		t.Errorf("cfg.Mode = %q, want auto (no repo_settings row exists yet)", cfg.Mode)
	}
}

// TestComputeDecision_FailsOpenOnBrokenRepoSettings pins §26.3's own
// "any triage error fails open to light" rule at the RepoSettings read
// specifically: a genuinely broken store must never propagate an error
// out of ComputeDecision (it has no error return at all -- this test
// would fail to compile if it ever grew one), and a light-looking diff
// must still route light rather than being forced deep by the failure.
func TestComputeDecision_FailsOpenOnBrokenRepoSettings(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repoFullName := repoFullNameForTest(t)

	deps := reviewtriage.Deps{
		RepoSettings:   narvipg.NewRepoSettingsStore(pool).WithTx(brokenTxReal(t, pool)),
		ReviewVerdicts: narvipg.NewReviewVerdictStore(pool),
	}

	prCtx := review.PreFetchedContext{InputRead: review.InputReadComplete, Additions: 5, Deletions: 5, ChangedPaths: []string{"internal/app/foo/a.go"}}
	decision, cfg, _ := reviewtriage.ComputeDecision(ctx, deps, repoFullName, 1, prCtx)

	if decision.Depth != domainreviewtriage.DepthLight {
		t.Errorf("Depth = %q, want light (a broken repo_settings read must fall open to the built-in default, never force deep)", decision.Depth)
	}
	if cfg.Mode != domainreviewtriage.ModeAuto || len(cfg.DeepPaths) != 0 {
		t.Errorf("cfg = %+v, want the built-in default (auto, no deepPaths)", cfg)
	}
}

// TestComputeDecision_FailsOpenOnBrokenReviewVerdicts mirrors the
// RepoSettings case above for the OTHER real read ComputeDecision
// performs (the "prior high verdict" signal): a broken review_verdicts
// store must degrade PriorVerdictRiskHigh to false, never propagate an
// error or force deep.
func TestComputeDecision_FailsOpenOnBrokenReviewVerdicts(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repoFullName := repoFullNameForTest(t)

	deps := reviewtriage.Deps{
		RepoSettings:   narvipg.NewRepoSettingsStore(pool),
		ReviewVerdicts: narvipg.NewReviewVerdictStore(pool).WithTx(brokenTxReal(t, pool)),
	}

	prCtx := review.PreFetchedContext{InputRead: review.InputReadComplete, Additions: 5, Deletions: 5, ChangedPaths: []string{"internal/app/foo/a.go"}}
	decision, _, _ := reviewtriage.ComputeDecision(ctx, deps, repoFullName, 1, prCtx)

	if decision.Depth != domainreviewtriage.DepthLight {
		t.Errorf("Depth = %q, want light (a broken review_verdicts read must degrade the prior-high-verdict signal to false, never force deep)", decision.Depth)
	}
}

// TestComputeDecision_PriorHighVerdict_RoutesDeep is D7's own regression
// test for rule 4 (§26.3's own "the PR's own verdict history -- a prior
// high verdict routes deep", doc.go's own "v1 rules -- five, not three"
// section): before this test, the exact line computing
// priorVerdictRiskHigh (compute.go) had ZERO coverage across this whole
// repo -- a mutation there (== "zzz-never", permanently disabling the
// rule) passed the full integration suite. This seeds a REAL high-risk
// review_verdicts row (via the real store, not a fake) on an otherwise
// light-looking PR (a small diff touching one ordinary, non-sensitive
// path) and asserts the resulting Depth/Reason.
func TestComputeDecision_PriorHighVerdict_RoutesDeep(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repoFullName := repoFullNameForTest(t)

	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	if _, err := reviewVerdicts.Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName:      repoFullName,
		PrNumber:          1,
		HeadSha:           "sha-prior-high-risk",
		RiskLevel:         "high",
		Premise:           "ok",
		BlastRadius:       []byte(`[]`),
		FilesChanged:      1,
		TestsCoverage:     "adequate",
		DocsDrift:         "none",
		ProposedShippable: "auto",
		Shippable:         "auto",
		ArchDecisionTags:  []byte(`[]`),
		ArchDecisionRoots: []byte(`[]`),
		AncestorChain:     []byte(`[]`),
	}); err != nil {
		t.Fatalf("seed prior high-risk review verdict: %v", err)
	}

	deps := reviewtriage.Deps{
		RepoSettings:   narvipg.NewRepoSettingsStore(pool),
		ReviewVerdicts: reviewVerdicts,
	}

	// Deliberately light-looking on every OTHER signal: small diff,
	// ordinary non-sensitive path, no repo config, no needs-human label --
	// isolating rule 4 as the ONE thing that could route this deep.
	prCtx := review.PreFetchedContext{InputRead: review.InputReadComplete, Additions: 3, Deletions: 2, ChangedPaths: []string{"internal/app/foo/a.go"}}
	decision, _, _ := reviewtriage.ComputeDecision(ctx, deps, repoFullName, 1, prCtx)

	if decision.Depth != domainreviewtriage.DepthDeep {
		t.Errorf("Depth = %q, want deep (a prior high-risk verdict must route this PR deep, §26.3 rule 4)", decision.Depth)
	}
	if decision.Reason != domainreviewtriage.ReasonPriorHighVerdict {
		t.Errorf("Reason = %q, want %q", decision.Reason, domainreviewtriage.ReasonPriorHighVerdict)
	}
}

// TestComputeDecision_NeedsHumanLabel_RoutesDeep is D7's own regression
// test for rule 5 (doc.go's own fifth trigger: "the PR's existing
// review:needs-human label ... routing an already-flagged-needs-human PR
// through the MORE rigorous deep path is strictly the safer direction").
// Before this test, hasNeedsHumanLabel's own mapping (compute.go) had
// ZERO coverage for the identical underlying reason as rule 4 above: no
// test in this package ever seeds a real review:needs-human label on a
// triage-reachable PR. No prior verdict this time -- isolating rule 5
// specifically (rule 4 never fires: ReviewVerdicts.GetLatest returns
// pgx.ErrNoRows for a PR with no verdict on record at all).
func TestComputeDecision_NeedsHumanLabel_RoutesDeep(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repoFullName := repoFullNameForTest(t)

	deps := reviewtriage.Deps{
		RepoSettings:   narvipg.NewRepoSettingsStore(pool),
		ReviewVerdicts: narvipg.NewReviewVerdictStore(pool),
	}

	// Deliberately light-looking on every OTHER signal (small diff,
	// ordinary non-sensitive path, no repo config, no prior verdict at
	// all) except Labels, which carries the real reviewpost.LabelNeedsHuman
	// string verbatim (hand-copied here rather than imported -- this
	// package's own test file has no existing dependency on
	// internal/domain/reviewpost, and doc.go's own "review:needs-human"
	// citation is this package's authoritative source for the exact
	// string).
	prCtx := review.PreFetchedContext{InputRead: review.InputReadComplete, Additions: 3, Deletions: 2, ChangedPaths: []string{"internal/app/foo/a.go"}, Labels: []string{"review:needs-human"}}
	decision, _, _ := reviewtriage.ComputeDecision(ctx, deps, repoFullName, 2, prCtx)

	if decision.Depth != domainreviewtriage.DepthDeep {
		t.Errorf("Depth = %q, want deep (an existing review:needs-human label must route this PR deep, §26.3 rule 5)", decision.Depth)
	}
	if decision.Reason != domainreviewtriage.ReasonNeedsHumanLabel {
		t.Errorf("Reason = %q, want %q", decision.Reason, domainreviewtriage.ReasonNeedsHumanLabel)
	}
}

// TestLoadConfig_MissingRowUsesDefault proves the "never configured yet"
// path resolves to reviewtriage.DefaultConfig(), err=nil -- mirroring
// internal/app/reviewverdict.LoadEligibilityConfig's own identical
// three-outcome shape.
func TestLoadConfig_MissingRowUsesDefault(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repoFullName := repoFullNameForTest(t)

	deps := reviewtriage.Deps{RepoSettings: narvipg.NewRepoSettingsStore(pool)}
	cfg, err := reviewtriage.LoadConfig(ctx, deps, repoFullName)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil for a missing row", err)
	}
	if cfg.Mode != domainreviewtriage.ModeAuto || len(cfg.DeepPaths) != 0 {
		t.Errorf("cfg = %+v, want the built-in default", cfg)
	}
}

// TestLoadConfig_ConfiguredRow proves a real, admin-configured
// review_depth_mode/review_depth_deep_paths row round-trips correctly
// through UpsertReviewDepthConfig -> LoadConfig.
func TestLoadConfig_ConfiguredRow(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repoFullName := repoFullNameForTest(t)

	store := narvipg.NewRepoSettingsStore(pool)
	mode := "always_deep"
	if _, err := store.UpsertReviewDepthConfig(ctx, repoFullName, &mode, []byte(`["internal/billing"]`)); err != nil {
		t.Fatalf("UpsertReviewDepthConfig: %v", err)
	}

	deps := reviewtriage.Deps{RepoSettings: store}
	cfg, err := reviewtriage.LoadConfig(ctx, deps, repoFullName)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil", err)
	}
	if cfg.Mode != domainreviewtriage.ModeAlwaysDeep {
		t.Errorf("cfg.Mode = %q, want always_deep", cfg.Mode)
	}
	if len(cfg.DeepPaths) != 1 || cfg.DeepPaths[0] != "internal/billing" {
		t.Errorf("cfg.DeepPaths = %v, want [internal/billing]", cfg.DeepPaths)
	}
}

// TestResolveProvenance_NarviAuthored proves a real artifacts row (Type
// 'pr', the SAME shape internal/app/sessionactor/pushpr.go's own
// recordPRArtifact writes) resolves NarviAuthored=true plus the
// authoring session's own build_model_id.
func TestResolveProvenance_NarviAuthored(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repoFullName := repoFullNameForTest(t)

	sessions := narvipg.NewSessionStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	buildModel := "anthropic/claude-frontier"
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, BuildModelID: &buildModel})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	htmlURL := "https://github.com/" + repoFullName + "/pull/7"
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{SessionID: session.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}")}); err != nil {
		t.Fatalf("create pr artifact: %v", err)
	}

	deps := reviewtriage.Deps{Artifacts: artifacts, Sessions: sessions}
	got := reviewtriage.ResolveProvenance(ctx, deps, repoFullName, 7)
	if !got.NarviAuthored {
		t.Error("NarviAuthored = false, want true")
	}
	if got.AuthoringModel != buildModel {
		t.Errorf("AuthoringModel = %q, want %q", got.AuthoringModel, buildModel)
	}
}

// TestResolveProvenance_NotAuthored proves a PR with no matching
// artifacts row (a human-opened PR, the common case) resolves to the
// zero-value Provenance{}, never an error.
func TestResolveProvenance_NotAuthored(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repoFullName := repoFullNameForTest(t)

	deps := reviewtriage.Deps{Artifacts: narvipg.NewArtifactStore(pool), Sessions: narvipg.NewSessionStore(pool)}
	got := reviewtriage.ResolveProvenance(ctx, deps, repoFullName, 999)
	if got.NarviAuthored {
		t.Error("NarviAuthored = true, want false for a PR with no matching artifact")
	}
	if got.AuthoringModel != "" {
		t.Errorf("AuthoringModel = %q, want empty", got.AuthoringModel)
	}
}

// TestResolveProvenance_NilStoresDegradesToZeroValue proves the nil-safe
// contract Deps.Artifacts/Sessions' own doc comment states.
func TestResolveProvenance_NilStoresDegradesToZeroValue(t *testing.T) {
	got := reviewtriage.ResolveProvenance(context.Background(), reviewtriage.Deps{}, "acme/widgets", 1)
	if got.NarviAuthored || got.AuthoringModel != "" {
		t.Errorf("got = %+v, want the zero value", got)
	}
}

// TestResolveProvenance_FailsOpenOnBrokenArtifactsStore proves a broken
// Artifacts read degrades to Provenance{} (treated as human-authored),
// never an error -- mirroring ComputeDecision's own identical fail-open
// posture.
func TestResolveProvenance_FailsOpenOnBrokenArtifactsStore(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	deps := reviewtriage.Deps{
		Artifacts: narvipg.NewArtifactStore(pool).WithTx(brokenTxReal(t, pool)),
		Sessions:  narvipg.NewSessionStore(pool),
	}
	got := reviewtriage.ResolveProvenance(ctx, deps, repoFullNameForTest(t), 1)
	if got.NarviAuthored {
		t.Error("NarviAuthored = true, want false on a broken artifacts read")
	}
}

// reviewOnce runs one review of (repoFullName, prNumber) the way every
// review lane does -- ComputeDecision, then §24's floor unless the fresh
// decision is an always_light override -- and persists what a lane and a
// posted verdict persist: a turn carrying the routing record and the
// final depth, then a verdict whose attempt_id is that turn and whose
// review_path is that depth. Returns the fresh decision, the prior depth
// ComputeDecision read, and the final depth.
func reviewOnce(ctx context.Context, t *testing.T, pool *pgxpool.Pool, deps reviewtriage.Deps, repoFullName string, prNumber int32, prCtx review.PreFetchedContext) (domainreviewtriage.Decision, domainreviewtriage.ReviewDepth, domainreviewtriage.ReviewDepth) {
	t.Helper()
	decision, cfg, prior := reviewtriage.ComputeDecision(ctx, deps, repoFullName, prNumber, prCtx)
	final := decision.Depth
	if decision.Reason != domainreviewtriage.ReasonAlwaysLightConfig {
		final = domainreviewtriage.Floor(decision.Depth, prior)
	}
	recordJSON, err := json.Marshal(domainreviewtriage.NewDecisionRecord(decision, cfg, final, domainreviewtriage.Provenance{}, nil, nil, prCtx.ChangedFilesCount, prCtx.Diff == "", prCtx.DiffTruncated, nil, nil))
	if err != nil {
		t.Fatalf("marshal decision record: %v", err)
	}

	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	finalStr := string(final)
	turn, err := narvipg.NewTurnStore(pool).Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:           session.ID,
		Status:              sqlcgen.TurnStatusCompleted,
		ReviewDepth:         &finalStr,
		ReviewDepthDecision: recordJSON,
		IsReviewAttempt:     true,
	})
	if err != nil {
		t.Fatalf("create review turn: %v", err)
	}
	insertVerdict(ctx, t, pool, repoFullName, prNumber, &finalStr, turn.ID)
	return decision, prior, final
}

// insertVerdict posts a low-risk verdict for (repoFullName, prNumber)
// with reviewPath, produced by attempt (an invalid UUID for a verdict
// posted before verdicts recorded their attempt).
func insertVerdict(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, prNumber int32, reviewPath *string, attempt pgtype.UUID) {
	t.Helper()
	if _, err := narvipg.NewReviewVerdictStore(pool).Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName:      repoFullName,
		PrNumber:          prNumber,
		HeadSha:           "sha-" + t.Name(),
		RiskLevel:         "low",
		Premise:           "ok",
		BlastRadius:       []byte(`[]`),
		FilesChanged:      1,
		TestsCoverage:     "adequate",
		DocsDrift:         "none",
		ProposedShippable: "auto",
		Shippable:         "auto",
		ReviewPath:        reviewPath,
		ArchDecisionTags:  []byte(`[]`),
		ArchDecisionRoots: []byte(`[]`),
		AncestorChain:     []byte(`[]`),
		AttemptID:         attempt,
	}); err != nil {
		t.Fatalf("insert review verdict: %v", err)
	}
}

// TestComputeDecision_UnreadableDepthIsNeverAFloor pins §26.3's floor
// exception on real Postgres, across consecutive reviews of one pull
// request: a depth chosen only because the input could not be read is
// never the next review's floor, while a depth chosen for a real reason
// still is -- including when an unreadable review came between.
func TestComputeDecision_UnreadableDepthIsNeverAFloor(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	deps := reviewtriage.Deps{
		RepoSettings:   narvipg.NewRepoSettingsStore(pool),
		ReviewVerdicts: narvipg.NewReviewVerdictStore(pool),
	}

	unreadable := review.PreFetchedContext{InputRead: review.InputReadDiffUnreadable, Additions: 3, Deletions: 1, ChangedFilesCount: 1}
	sensitive := review.PreFetchedContext{InputRead: review.InputReadComplete, Additions: 2, ChangedFilesCount: 1, ChangedPaths: []string{"migrations/000200_x.up.sql"}}
	smallReadable := review.PreFetchedContext{InputRead: review.InputReadComplete, Additions: 4, Deletions: 2, ChangedFilesCount: 1, ChangedPaths: []string{"internal/app/foo/a.go"}}

	tests := []struct {
		name      string
		history   []review.PreFetchedContext
		wantPrior domainreviewtriage.ReviewDepth
		wantFinal domainreviewtriage.ReviewDepth
	}{
		{
			name:      "an unreadable review does not floor the next one",
			history:   []review.PreFetchedContext{unreadable},
			wantPrior: "",
			wantFinal: domainreviewtriage.DepthLight,
		},
		{
			name:      "two unreadable reviews in a row floor nothing",
			history:   []review.PreFetchedContext{unreadable, unreadable},
			wantPrior: "",
			wantFinal: domainreviewtriage.DepthLight,
		},
		{
			name:      "a review deep for a real reason still floors the next one",
			history:   []review.PreFetchedContext{sensitive},
			wantPrior: domainreviewtriage.DepthDeep,
			wantFinal: domainreviewtriage.DepthDeep,
		},
		{
			name:      "a real deep review before an unreadable one still floors",
			history:   []review.PreFetchedContext{sensitive, unreadable},
			wantPrior: domainreviewtriage.DepthDeep,
			wantFinal: domainreviewtriage.DepthDeep,
		},
		{
			name:      "a light review after an unreadable one floors at light",
			history:   []review.PreFetchedContext{smallReadable, unreadable},
			wantPrior: domainreviewtriage.DepthLight,
			wantFinal: domainreviewtriage.DepthLight,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoFullName := repoFullNameForTest(t)
			prNumber := int32(100 + i)
			for j, prCtx := range tt.history {
				decision, _, final := reviewOnce(ctx, t, pool, deps, repoFullName, prNumber, prCtx)
				if prCtx.InputRead == review.InputReadDiffUnreadable && (decision.Reason != domainreviewtriage.ReasonInputUnreadable || final != domainreviewtriage.DepthDeep) {
					t.Fatalf("history[%d]: an unreadable review routed (%q, %q), want deep under %q", j, final, decision.Reason, domainreviewtriage.ReasonInputUnreadable)
				}
			}

			decision, prior, final := reviewOnce(ctx, t, pool, deps, repoFullName, prNumber, smallReadable)
			if decision.Depth != domainreviewtriage.DepthLight || decision.Reason != domainreviewtriage.ReasonLightDefault {
				t.Fatalf("fresh decision = (%q, %q), want (light, %q)", decision.Depth, decision.Reason, domainreviewtriage.ReasonLightDefault)
			}
			if prior != tt.wantPrior {
				t.Errorf("prior depth = %q, want %q", prior, tt.wantPrior)
			}
			if final != tt.wantFinal {
				t.Errorf("final depth = %q, want %q", final, tt.wantFinal)
			}
		})
	}

	t.Run("a nil exclusion list skips nothing, never matches nothing", func(t *testing.T) {
		repoFullName := repoFullNameForTest(t)
		deep := string(domainreviewtriage.DepthDeep)
		insertVerdict(ctx, t, pool, repoFullName, 1, &deep, pgtype.UUID{})
		path, err := narvipg.NewReviewVerdictStore(pool).GetLatestFloorReviewPath(ctx, repoFullName, 1, nil)
		if err != nil {
			t.Fatalf("GetLatestFloorReviewPath(nil) error = %v, want the deep verdict", err)
		}
		if path == nil || *path != deep {
			t.Errorf("GetLatestFloorReviewPath(nil) = %v, want deep", path)
		}
	})

	t.Run("a verdict recorded before verdicts named their attempt floors as before", func(t *testing.T) {
		repoFullName := repoFullNameForTest(t)
		deep := string(domainreviewtriage.DepthDeep)
		insertVerdict(ctx, t, pool, repoFullName, 1, &deep, pgtype.UUID{})
		_, prior, final := reviewOnce(ctx, t, pool, deps, repoFullName, 1, smallReadable)
		if prior != domainreviewtriage.DepthDeep || final != domainreviewtriage.DepthDeep {
			t.Errorf("prior/final = %q/%q, want deep/deep", prior, final)
		}
	})
}

// TestComputeDecision_RecordsTheCause pins, through the real persisted
// column, that the routing record names how the input was read -- an
// always_light override with an unreadable diff routes light and still
// records the cause.
func TestComputeDecision_RecordsTheCause(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	deps := reviewtriage.Deps{RepoSettings: repoSettings, ReviewVerdicts: narvipg.NewReviewVerdictStore(pool)}

	tests := []struct {
		name          string
		mode          string
		prCtx         review.PreFetchedContext
		wantDepth     string
		wantReason    domainreviewtriage.Reason
		wantInputRead string
	}{
		{"always_light with an unreadable diff", "always_light", review.PreFetchedContext{InputRead: review.InputReadDiffUnreadable, Additions: 3}, "light", domainreviewtriage.ReasonAlwaysLightConfig, "diff_unreadable"},
		{"auto with a truncated file list", "auto", review.PreFetchedContext{InputRead: review.InputReadDiffTruncated, Additions: 3, ChangedPaths: []string{"internal/a.go"}}, "deep", domainreviewtriage.ReasonInputUnreadable, "diff_truncated"},
		{"auto with a genuinely empty change", "auto", review.PreFetchedContext{InputRead: review.InputReadEmpty}, "light", domainreviewtriage.ReasonLightDefault, "empty"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoFullName := repoFullNameForTest(t)
			mode := tt.mode
			if _, err := repoSettings.UpsertReviewDepthConfig(ctx, repoFullName, &mode, []byte(`[]`)); err != nil {
				t.Fatalf("UpsertReviewDepthConfig: %v", err)
			}
			prNumber := int32(200 + i)
			reviewOnce(ctx, t, pool, deps, repoFullName, prNumber, tt.prCtx)

			var raw []byte
			if err := pool.QueryRow(ctx, `SELECT t.review_depth_decision FROM review_verdicts rv JOIN turns t ON t.id = rv.attempt_id WHERE rv.repo_full_name = $1 AND rv.pr_number = $2`, repoFullName, prNumber).Scan(&raw); err != nil {
				t.Fatalf("read persisted record: %v", err)
			}
			var record domainreviewtriage.DecisionRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatalf("unmarshal persisted record %s: %v", raw, err)
			}
			if record.Depth != tt.wantDepth || record.Reason != string(tt.wantReason) || record.InputRead != tt.wantInputRead {
				t.Errorf("persisted record depth/reason/inputRead = %q/%q/%q, want %q/%q/%q", record.Depth, record.Reason, record.InputRead, tt.wantDepth, tt.wantReason, tt.wantInputRead)
			}
		})
	}
}
