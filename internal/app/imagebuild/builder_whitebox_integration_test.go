//go:build integration

// White-box (package imagebuild, not imagebuild_test) integration tests
// covering attemptRefresh's own early-return branches that CANNOT be
// reached through RefreshOnce's own public entry point at all: the
// base-only defense-in-depth guard (ListReadyImageBuilds already excludes
// a base-only row at the SQL level, by design, so a black-box test can
// never make RefreshOnce return one to attemptRefresh) and a genuinely
// lost ClaimForRefresh race (deterministically reproducing a concurrent
// pod's own already-successful claim requires calling ClaimForRefresh
// directly, ahead of attemptRefresh, rather than relying on real
// goroutine-timing luck).
//
// Every other early-return branch (decode failure, resolveRepoSHAs error,
// NeedsRefresh-false, both RecordRefreshSuccess failure modes) IS
// reachable through RefreshOnce and is covered black-box in
// builder_integration_test.go instead, matching that file's own
// established black-box convention -- this file exists only for the two
// branches that structurally cannot be.
package imagebuild

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// newWhiteboxTestPool returns this package's own single, shared Postgres
// pool -- started ONCE for the whole test binary by TestMain
// (sharedpool_integration_test.go), not freshly per test/container as
// this function used to do itself. Kept as a thin wrapper under its own
// original name/signature so this file's own call sites keep compiling
// unchanged. See sharedpool_integration_test.go's own top doc comment
// for the full container-reuse story: why this file used to duplicate
// builder_integration_test.go's own newTestPool at all (a reverse import
// package imagebuild_test's own unexported helper Go does not allow),
// and why sharing one container across the whole binary is safe now.
func newWhiteboxTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return IntegrationTestPool(t)
}

// whiteboxFakeSourceControl is a minimal test-only ports.SourceControl,
// duplicated from builder_integration_test.go's own fakeSourceControl for
// the identical cross-package reason newWhiteboxTestPool is.
type whiteboxFakeSourceControl struct {
	shaFor map[string]string
}

var _ ports.SourceControl = (*whiteboxFakeSourceControl)(nil)

func (f *whiteboxFakeSourceControl) CreatePR(context.Context, ports.CreatePRSpec) (ports.PRRef, error) {
	return ports.PRRef{}, errors.New("whiteboxFakeSourceControl: CreatePR not implemented")
}

func (f *whiteboxFakeSourceControl) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	sha := f.shaFor[spec.Repo]
	return sha, "main", nil
}

// IsAncestor (D3, second adversarial-review round) is never exercised by
// this file's own tests -- stubbed only for ports.SourceControl interface
// satisfaction.
func (f *whiteboxFakeSourceControl) IsAncestor(context.Context, ports.IsAncestorSpec) (bool, error) {
	return false, errors.New("whiteboxFakeSourceControl: IsAncestor not implemented")
}

func (f *whiteboxFakeSourceControl) ResolveContractsFingerprint(context.Context, ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return "", false, errors.New("whiteboxFakeSourceControl: ResolveContractsFingerprint not implemented")
}

// CheckRepoAccess is never reached from this package: the warm-boot
// repo-access gate lives entirely in app/sessionactor, upstream of the
// builder -- mirrors builder_integration_test.go's own fakeSourceControl,
// and CreatePR/ResolveContractsFingerprint above, in returning a clear
// "not implemented" rather than a silent zero value.
func (f *whiteboxFakeSourceControl) CheckRepoAccess(context.Context, ports.CheckRepoAccessSpec) (bool, error) {
	return false, errors.New("whiteboxFakeSourceControl: CheckRepoAccess not implemented")
}

// GetFileContent/UpdateFileContent/RegisterPRStack (§8.2, "sentinels +
// suggestions") are never reached from this package either -- same
// "not implemented" precedent as CheckRepoAccess above.
func (f *whiteboxFakeSourceControl) GetFileContent(context.Context, ports.GetFileContentSpec) (string, string, bool, error) {
	return "", "", false, errors.New("whiteboxFakeSourceControl: GetFileContent not implemented")
}

func (f *whiteboxFakeSourceControl) UpdateFileContent(context.Context, ports.UpdateFileContentSpec) (string, error) {
	return "", errors.New("whiteboxFakeSourceControl: UpdateFileContent not implemented")
}

func (f *whiteboxFakeSourceControl) RegisterPRStack(context.Context, ports.RegisterPRStackSpec) error {
	return errors.New("whiteboxFakeSourceControl: RegisterPRStack not implemented")
}

// CreateBranch (a confirmed-finding fix) is never reached from this
// package either -- same "not implemented" precedent as the methods above.
func (f *whiteboxFakeSourceControl) CreateBranch(context.Context, ports.CreateBranchSpec) error {
	return errors.New("whiteboxFakeSourceControl: CreateBranch not implemented")
}
func (f *whiteboxFakeSourceControl) GetOpenPR(context.Context, string, string, int, string) (ports.OpenPR, bool, error) {
	return ports.OpenPR{}, false, errors.New("whiteboxFakeSourceControl: GetOpenPR not implemented")
}
func (f *whiteboxFakeSourceControl) GetPRBody(context.Context, string, string, int, string) (string, bool, error) {
	return "", false, errors.New("whiteboxFakeSourceControl: GetPRBody not implemented")
}
func (f *whiteboxFakeSourceControl) UpdatePRBody(context.Context, ports.UpdatePRBodySpec) error {
	return errors.New("whiteboxFakeSourceControl: UpdatePRBody not implemented")
}

// ListMergedBetween ("release PR review", §15.2) is never
// reached from this package either -- same "not implemented" precedent
// as the methods above.
func (f *whiteboxFakeSourceControl) ListMergedBetween(context.Context, ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return nil, false, errors.New("whiteboxFakeSourceControl: ListMergedBetween not implemented")
}

// ListOpenPRsForUser/ResolveCodeOwners/MergePR ("decision inbox:
// read model + API", §16.2) are never reached from this package -- same
// "not implemented" precedent as the methods above.
func (f *whiteboxFakeSourceControl) ListOpenPRsForUser(context.Context, ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	return nil, false, errors.New("whiteboxFakeSourceControl: ListOpenPRsForUser not implemented")
}

func (f *whiteboxFakeSourceControl) ResolveCodeOwners(context.Context, ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	return nil, errors.New("whiteboxFakeSourceControl: ResolveCodeOwners not implemented")
}

func (f *whiteboxFakeSourceControl) MergePR(context.Context, ports.MergePRSpec) (string, error) {
	return "", errors.New("whiteboxFakeSourceControl: MergePR not implemented")
}

// whiteboxFakeBuildProvider is a minimal test-only ports.SandboxProvider,
// duplicated from builder_integration_test.go's own fakeBuildProvider for
// the identical cross-package reason.
type whiteboxFakeBuildProvider struct {
	buildCalls int
	nextRef    ports.BuildRef
}

var _ ports.SandboxProvider = (*whiteboxFakeBuildProvider)(nil)

func (f *whiteboxFakeBuildProvider) Capabilities() ports.Capabilities {
	return ports.Capabilities{ImageBuilds: true}
}
func (f *whiteboxFakeBuildProvider) CreateSandbox(context.Context, ports.CreateSpec) (ports.SandboxRef, error) {
	return ports.SandboxRef{}, errors.New("whiteboxFakeBuildProvider: CreateSandbox not implemented")
}
func (f *whiteboxFakeBuildProvider) StopSandbox(context.Context, ports.SandboxRef) error {
	return errors.New("whiteboxFakeBuildProvider: StopSandbox not implemented")
}
func (f *whiteboxFakeBuildProvider) ResumeSandbox(context.Context, ports.SandboxRef) error {
	return errors.New("whiteboxFakeBuildProvider: ResumeSandbox not implemented")
}
func (f *whiteboxFakeBuildProvider) TakeSnapshot(context.Context, ports.SandboxRef) (ports.SnapshotID, error) {
	return "", errors.New("whiteboxFakeBuildProvider: TakeSnapshot not implemented")
}
func (f *whiteboxFakeBuildProvider) RestoreFromSnapshot(context.Context, ports.SnapshotID, ports.CreateSpec) (ports.SandboxRef, error) {
	return ports.SandboxRef{}, errors.New("whiteboxFakeBuildProvider: RestoreFromSnapshot not implemented")
}
func (f *whiteboxFakeBuildProvider) BuildImage(context.Context, ports.ImageSpec) (ports.BuildOutcome, error) {
	f.buildCalls++
	return ports.BuildOutcome{Ref: f.nextRef}, nil
}
func (f *whiteboxFakeBuildProvider) DeleteImage(context.Context, ports.ImageRef) error {
	return errors.New("whiteboxFakeBuildProvider: DeleteImage not implemented")
}
func (f *whiteboxFakeBuildProvider) List(context.Context) ([]ports.SandboxRef, error) {
	return nil, nil
}

// whiteboxSeedReadyImageBuild seeds fingerprint through the SAME
// pending->building->ready sequence builder_integration_test.go's own
// seedReadyImageBuildWithRepos does, duplicated here for the same
// cross-package reason.
func whiteboxSeedReadyImageBuild(ctx context.Context, t *testing.T, store *narvipg.ImageBuildStore, fingerprint string, repoURLsJSON, builtRepoSHAsJSON []byte, imageRef string) {
	t.Helper()

	if err := store.UpsertPending(ctx, sqlcgen.UpsertPendingImageBuildParams{
		Fingerprint:    fingerprint,
		Base:           "narvi/base:test",
		RepoUrls:       repoURLsJSON,
		RuntimeVersion: "1.0.0-test",
	}); err != nil {
		t.Fatalf("seed pending image_builds row: %v", err)
	}
	if _, err := store.Claim(ctx, fingerprint); err != nil {
		t.Fatalf("claim image_builds row: %v", err)
	}
	if _, err := store.RecordSuccess(ctx, sqlcgen.RecordImageBuildSuccessParams{
		Fingerprint:   fingerprint,
		ImageRef:      &imageRef,
		BuiltRepoShas: builtRepoSHAsJSON,
		BuiltAt:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("record image_builds success: %v", err)
	}
}

// TestAttemptRefresh_BaseOnlyGuard_TouchesOrderingKeyOnly proves the
// base-only defense-in-depth guard (attemptRefresh, len(repoURLs) == 0):
// unreachable through RefreshOnce's own ListReadyImageBuilds query today
// (which already excludes repo_urls = '{}' at the SQL level), but this
// guard must still advance this row's own ordering key before returning,
// per attemptRefresh's own top doc comment invariant 1 -- proven here by
// calling attemptRefresh directly with a genuinely seeded base-only
// 'ready' row.
func TestAttemptRefresh_BaseOnlyGuard_TouchesOrderingKeyOnly(t *testing.T) {
	ctx := context.Background()
	pool := newWhiteboxTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-whitebox-base-only-guard"
	oldRef := "narvi/built-image:base-only-guard-old-ref"
	whiteboxSeedReadyImageBuild(ctx, t, store, fingerprint, []byte(`{}`), []byte(`{}`), oldRef)

	rowBefore, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row before: %v", err)
	}
	if len(rowBefore.RepoUrls) == 0 {
		t.Fatal("test setup: repo_urls scanned as empty/nil, want the literal 2-byte '{}'")
	}

	provider := &whiteboxFakeBuildProvider{nextRef: "should-never-be-used"}
	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := NewBuilder(store, pool, provider, platform.DefaultTimeouts(), nil, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	time.Sleep(5 * time.Millisecond) // ensure a real clock delta from seeding
	staleClaimCutoff := pgtype.Timestamptz{Time: time.Now().Add(-builder.timeouts.ImageRefreshClaimStaleAfter), Valid: true}
	builder.attemptRefresh(ctx, rowBefore, staleClaimCutoff)

	if provider.buildCalls != 0 {
		t.Errorf("BuildImage call count = %d, want 0 (a base-only row is never stale)", provider.buildCalls)
	}

	rowAfter, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after: %v", err)
	}
	if !rowAfter.UpdatedAt.Time.After(rowBefore.UpdatedAt.Time) {
		t.Errorf("updated_at did not advance (before=%v after=%v) -- the base-only guard must call touchChecked", rowBefore.UpdatedAt.Time, rowAfter.UpdatedAt.Time)
	}
	if rowAfter.RefreshInProgress {
		t.Error("refresh_in_progress = true, want false (this branch never takes a claim)")
	}
	if rowAfter.ImageRef == nil || *rowAfter.ImageRef != oldRef {
		t.Errorf("image_ref = %v, want unchanged %q", rowAfter.ImageRef, oldRef)
	}
}

// TestAttemptRefresh_ClaimForRefreshLostRace_TouchesOrderingKeyNoRelease
// proves the ClaimForRefresh lost-race branch (pgx.ErrNoRows): this row was
// genuinely inspected (repo_urls decoded, current tips resolved, NeedsRefresh
// reported true), reached ClaimForRefresh, and lost the race to a
// concurrent, still-fresh claim -- it must still call touchChecked
// (invariant 1), and, because THIS attemptRefresh call never actually held
// the claim, it must NOT touch refresh_in_progress/refresh_started_at at
// all (invariant 2 only applies to a claim THIS call itself took).
//
// Deterministically reproduces the race (rather than relying on real
// goroutine timing) by calling ClaimForRefresh directly, simulating a
// concurrent pod's own already-successful claim, BEFORE invoking
// attemptRefresh with the row as it looked at read time (RefreshInProgress
// == false, exactly what a real ListReady call would have returned a
// moment earlier).
func TestAttemptRefresh_ClaimForRefreshLostRace_TouchesOrderingKeyNoRelease(t *testing.T) {
	ctx := context.Background()
	pool := newWhiteboxTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-whitebox-lost-claim-race"
	oldRef := "narvi/built-image:lost-race-old-ref"
	whiteboxSeedReadyImageBuild(ctx, t, store, fingerprint,
		[]byte(`{"repo1":"https://github.com/acme/repo1"}`),
		[]byte(`{"repo1":"sha-old"}`),
		oldRef)

	rowBefore, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row before: %v", err)
	}
	if rowBefore.RefreshInProgress {
		t.Fatal("test setup: row already refresh_in_progress before the simulated concurrent claim")
	}

	// Simulate a concurrent pod's own ALREADY-SUCCESSFUL, still-fresh
	// claim -- staleClaimCutoff far in the past, so nothing is considered
	// stale, matching a real fresh claim exactly.
	farPastCutoff := pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true}
	if _, err := store.ClaimForRefresh(ctx, fingerprint, farPastCutoff); err != nil {
		t.Fatalf("simulate concurrent claim: %v", err)
	}
	claimedRow, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after simulated concurrent claim: %v", err)
	}
	if !claimedRow.RefreshInProgress {
		t.Fatal("test setup: simulated concurrent claim did not set refresh_in_progress")
	}

	provider := &whiteboxFakeBuildProvider{nextRef: "should-never-be-used"}
	sourceControl := &whiteboxFakeSourceControl{shaFor: map[string]string{"repo1": "sha-new"}} // genuinely stale -- NeedsRefresh must report true

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	time.Sleep(5 * time.Millisecond)
	// This tick's own cutoff is comfortably NEWER than the simulated
	// concurrent claim's own refresh_started_at, so that claim reads as
	// still-fresh, never stale -- attemptRefresh's own ClaimForRefresh call
	// below must genuinely lose the race (pgx.ErrNoRows), not reclaim a
	// stale one.
	thisTickCutoff := pgtype.Timestamptz{Time: time.Now().Add(-builder.timeouts.ImageRefreshClaimStaleAfter), Valid: true}
	builder.attemptRefresh(ctx, rowBefore, thisTickCutoff)

	if provider.buildCalls != 0 {
		t.Errorf("BuildImage call count = %d, want 0 (must never build after losing the claim race)", provider.buildCalls)
	}

	rowAfter, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after: %v", err)
	}
	// Compare against claimedRow (the state right after the SIMULATED
	// concurrent claim, immediately before attemptRefresh runs), NOT
	// rowBefore -- ClaimForRefresh's own CAS already bumps updated_at as
	// part of taking that simulated claim, so comparing against the
	// earlier rowBefore would trivially "pass" on that bump alone even if
	// attemptRefresh's own touchChecked call were missing entirely (a real
	// false-negative this test's own first draft had, caught by this
	// batch's own mutation-testing pass: reverting touchChecked here left
	// this assertion passing anyway until the baseline was corrected).
	if !rowAfter.UpdatedAt.Time.After(claimedRow.UpdatedAt.Time) {
		t.Errorf("updated_at did not advance past the simulated concurrent claim (claimed=%v after=%v) -- a lost claim race must still call touchChecked (this was, before audit-remediation batch B2, the ONE branch that didn't -- see attemptRefresh's own top doc comment)", claimedRow.UpdatedAt.Time, rowAfter.UpdatedAt.Time)
	}
	if !rowAfter.RefreshInProgress {
		t.Error("refresh_in_progress = false, want true -- this attemptRefresh call never held the claim, so it must not release the CONCURRENT claim it lost the race to")
	}
	if !rowAfter.RefreshStartedAt.Time.Equal(claimedRow.RefreshStartedAt.Time) {
		t.Errorf("refresh_started_at changed (was %v, now %v) -- a lost claim race must not perturb the concurrent winner's own claim timestamp", claimedRow.RefreshStartedAt.Time, rowAfter.RefreshStartedAt.Time)
	}
	if rowAfter.ImageRef == nil || *rowAfter.ImageRef != oldRef {
		t.Errorf("image_ref = %v, want unchanged %q", rowAfter.ImageRef, oldRef)
	}
}

// Note: TestMain is intentionally NOT redefined here -- a Go test binary
// allows exactly one TestMain across every package contributing test files
// in this directory; builder_integration_test.go's own package
// imagebuild_test already provides it (registering the global OTel
// MeterProvider these white-box tests' own NewBuilder calls also rely on).
