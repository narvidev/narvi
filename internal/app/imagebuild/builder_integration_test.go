//go:build integration

// Integration tests proving Builder.PumpOnce against a REAL Postgres
// instance (§9.1) -- gated behind the "integration" build tag, matching
// internal/app/reconciler/reconciler_integration_test.go's own conventions
// exactly (testcontainers Postgres, embedded migrations via golang-migrate's
// iofs source driver, a real *pgxpool.Pool, a single global OTel
// MeterProvider wired once in TestMain). Run via `make test-integration`.
//
// internal/app/sessionactor/imagebuild_integration_test.go covers the
// spawn-side half of this Step end to end (scenarios a/b/d in this Step's
// own brief); this file covers the background-builder-side half in
// isolation: backoff/not-retried-before-due (scenario c) and the
// failure-streak alert threshold (scenario e).
package imagebuild_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/imagebuild"
	"github.com/narvidev/narvi/internal/app/ports"
	domainimagebuild "github.com/narvidev/narvi/internal/domain/imagebuild"
	"github.com/narvidev/narvi/internal/platform"
)

// newTestPool returns this package's own single, shared Postgres pool --
// started ONCE for the whole test binary by TestMain (sharedpool_
// integration_test.go, package imagebuild), not freshly per test/
// container as this function used to do itself. Kept as a thin wrapper
// under its own original name/signature so every existing call site in
// this file keeps compiling unchanged. See sharedpool_integration_test.
// go's own top doc comment for the full container-reuse story.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return imagebuild.IntegrationTestPool(t)
}

// fakeSourceControl is a minimal test-only ports.SourceControl, narrowed
// to exactly the one method this package's own Builder ever calls
// (ResolveBranchSHA) -- mirrors internal/app/sessionactor/
// pushpr_integration_test.go's own fakeSourceControl precedent, duplicated
// here rather than shared across package boundaries (this package's own
// test package cannot import sessionactor's unexported test type anyway).
type fakeSourceControl struct {
	mu sync.Mutex

	shaCalls []ports.ResolveBranchSHASpec
	shaFor   map[string]string // keyed by repo name; falls back to nextSHA if absent
	errFor   map[string]error  // keyed by repo name; checked BEFORE shaFor/nextErr -- lets a test model a repo whose resolution PERSISTENTLY fails (a renamed/deleted repo, a token missing org access) alongside other repos that resolve normally, which the single global nextErr below cannot express (it fails EVERY call, not a chosen subset)
	nextSHA  string
	nextErr  error
}

var _ ports.SourceControl = (*fakeSourceControl)(nil)

func (f *fakeSourceControl) CreatePR(context.Context, ports.CreatePRSpec) (ports.PRRef, error) {
	return ports.PRRef{}, errors.New("fakeSourceControl: CreatePR not implemented")
}

func (f *fakeSourceControl) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shaCalls = append(f.shaCalls, spec)
	if err, ok := f.errFor[spec.Repo]; ok {
		return "", "", err
	}
	if f.nextErr != nil {
		return "", "", f.nextErr
	}
	resolvedBranch := spec.Branch
	if resolvedBranch == "" {
		resolvedBranch = "main"
	}
	if sha, ok := f.shaFor[spec.Repo]; ok {
		return sha, resolvedBranch, nil
	}
	return f.nextSHA, resolvedBranch, nil
}

// IsAncestor (D3, second adversarial-review round) is never exercised by
// this file's own tests -- stubbed only for ports.SourceControl interface
// satisfaction.
func (f *fakeSourceControl) IsAncestor(context.Context, ports.IsAncestorSpec) (bool, error) {
	return false, errors.New("fakeSourceControl: IsAncestor not implemented")
}

func (f *fakeSourceControl) ResolveContractsFingerprint(context.Context, ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return "", false, errors.New("fakeSourceControl: ResolveContractsFingerprint not implemented")
}

// CheckRepoAccess is never called by this package's own Builder (the
// audit fix's repo-access gate lives entirely in app/sessionactor,
// upstream of this package -- see imageresolve.go's own "why this runs
// where it runs" reasoning) -- mirrors CreatePR/ResolveContractsFingerprint
// above's own "not implemented" precedent for the same reason.
func (f *fakeSourceControl) CheckRepoAccess(context.Context, ports.CheckRepoAccessSpec) (bool, error) {
	return false, errors.New("fakeSourceControl: CheckRepoAccess not implemented")
}

// GetFileContent/UpdateFileContent/RegisterPRStack (§8.2, "sentinels +
// suggestions") are never reached from this package -- same "not
// implemented" precedent as CheckRepoAccess above.
func (f *fakeSourceControl) GetFileContent(context.Context, ports.GetFileContentSpec) (string, string, bool, error) {
	return "", "", false, errors.New("fakeSourceControl: GetFileContent not implemented")
}

func (f *fakeSourceControl) UpdateFileContent(context.Context, ports.UpdateFileContentSpec) (string, error) {
	return "", errors.New("fakeSourceControl: UpdateFileContent not implemented")
}

func (f *fakeSourceControl) RegisterPRStack(context.Context, ports.RegisterPRStackSpec) error {
	return errors.New("fakeSourceControl: RegisterPRStack not implemented")
}

// CreateBranch (a confirmed-finding fix) is never reached from this
// package either -- same "not implemented" precedent as
// GetFileContent/UpdateFileContent/RegisterPRStack above.
func (f *fakeSourceControl) CreateBranch(context.Context, ports.CreateBranchSpec) error {
	return errors.New("fakeSourceControl: CreateBranch not implemented")
}
func (f *fakeSourceControl) GetOpenPR(context.Context, string, string, int, string) (ports.OpenPR, bool, error) {
	return ports.OpenPR{}, false, errors.New("fakeSourceControl: GetOpenPR not implemented")
}
func (f *fakeSourceControl) GetPRBody(context.Context, string, string, int, string) (string, bool, error) {
	return "", false, errors.New("fakeSourceControl: GetPRBody not implemented")
}
func (f *fakeSourceControl) UpdatePRBody(context.Context, ports.UpdatePRBodySpec) error {
	return errors.New("fakeSourceControl: UpdatePRBody not implemented")
}

// ListMergedBetween ("release PR review", §15.2) is never
// reached from this package either -- same "not implemented" precedent
// as CreateBranch above.
func (f *fakeSourceControl) ListMergedBetween(context.Context, ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return nil, false, errors.New("fakeSourceControl: ListMergedBetween not implemented")
}

// ListOpenPRsForUser/ResolveCodeOwners/MergePR ("decision inbox:
// read model + API", §16.2) are never reached from this package -- same
// "not implemented" precedent as ListMergedBetween above.
func (f *fakeSourceControl) ListOpenPRsForUser(context.Context, ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	return nil, false, errors.New("fakeSourceControl: ListOpenPRsForUser not implemented")
}

func (f *fakeSourceControl) ResolveCodeOwners(context.Context, ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	return nil, errors.New("fakeSourceControl: ResolveCodeOwners not implemented")
}

func (f *fakeSourceControl) MergePR(context.Context, ports.MergePRSpec) (string, error) {
	return "", errors.New("fakeSourceControl: MergePR not implemented")
}

func (f *fakeSourceControl) shaCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.shaCalls)
}

// fakeBuildProvider is a test-only ports.SandboxProvider recording every
// BuildImage call and returning a caller-configured (ref, err) pair --
// mirrors internal/app/reconciler's own fakeReconcileProvider precedent
// exactly (configurable behavior + a recorded-calls slice, mutex-guarded),
// narrowed to the one method this package's own Builder actually calls.
//
// echoPublishedCacheVersion (§19.1's closing paragraph(c), third
// iteration) models the modal adapter's own real BuildImage contract
// (BuildOutcome.PublishedCacheVersion's own doc comment): when true (the
// default a caller opts into per test, mirroring a real adapter's
// eventual-success-still-carried-the-mount case), a successful call whose
// spec carries a CacheMount echoes spec.CacheMount.PublishVersion back;
// when false, it is left empty even on success, modeling the adapter's
// own decline-and-retry-cold fallback having silently dropped the mount.
type fakeBuildProvider struct {
	mu sync.Mutex

	buildCalls                []ports.ImageSpec
	nextRef                   ports.BuildRef
	nextErr                   error
	echoPublishedCacheVersion bool

	// onBuildImage, when set, runs synchronously inside BuildImage --
	// AFTER the call is recorded (so buildCalls/spec.CacheMount already
	// reflects what THIS attempt resolved and sent) but BEFORE this method
	// returns -- letting a test inject an action that must be observed as
	// having happened "during" this specific BuildImage call, e.g. a
	// concurrent, independent publish to the SAME cache key
	// (TestPumpOnce_MountedVersionImmuneToConcurrentPublish's own use).
	onBuildImage func(spec ports.ImageSpec)
}

var _ ports.SandboxProvider = (*fakeBuildProvider)(nil)

func (f *fakeBuildProvider) Capabilities() ports.Capabilities {
	return ports.Capabilities{ImageBuilds: true}
}

func (f *fakeBuildProvider) CreateSandbox(context.Context, ports.CreateSpec) (ports.SandboxRef, error) {
	return ports.SandboxRef{}, errors.New("fakeBuildProvider: CreateSandbox not implemented")
}
func (f *fakeBuildProvider) StopSandbox(context.Context, ports.SandboxRef) error {
	return errors.New("fakeBuildProvider: StopSandbox not implemented")
}
func (f *fakeBuildProvider) ResumeSandbox(context.Context, ports.SandboxRef) error {
	return errors.New("fakeBuildProvider: ResumeSandbox not implemented")
}
func (f *fakeBuildProvider) TakeSnapshot(context.Context, ports.SandboxRef) (ports.SnapshotID, error) {
	return "", errors.New("fakeBuildProvider: TakeSnapshot not implemented")
}
func (f *fakeBuildProvider) RestoreFromSnapshot(context.Context, ports.SnapshotID, ports.CreateSpec) (ports.SandboxRef, error) {
	return ports.SandboxRef{}, errors.New("fakeBuildProvider: RestoreFromSnapshot not implemented")
}

func (f *fakeBuildProvider) BuildImage(_ context.Context, spec ports.ImageSpec) (ports.BuildOutcome, error) {
	f.mu.Lock()
	f.buildCalls = append(f.buildCalls, spec)
	hook := f.onBuildImage
	nextErr := f.nextErr
	nextRef := f.nextRef
	echoPublishedCacheVersion := f.echoPublishedCacheVersion
	f.mu.Unlock()

	// hook runs OUTSIDE the lock (never re-entrant against this same
	// provider's own mutex) but every value this call itself returns was
	// already captured, under the lock, before hook ran -- so hook's own
	// side effects (e.g. a concurrent publish to Postgres) can never
	// retroactively change what THIS call reports.
	if hook != nil {
		hook(spec)
	}

	if nextErr != nil {
		return ports.BuildOutcome{}, nextErr
	}
	outcome := ports.BuildOutcome{Ref: nextRef}
	if echoPublishedCacheVersion && spec.CacheMount != nil {
		outcome.PublishedCacheVersion = spec.CacheMount.PublishVersion
	}
	return outcome, nil
}

func (f *fakeBuildProvider) DeleteImage(context.Context, ports.ImageRef) error {
	return errors.New("fakeBuildProvider: DeleteImage not implemented")
}
func (f *fakeBuildProvider) List(context.Context) ([]ports.SandboxRef, error) { return nil, nil }

func (f *fakeBuildProvider) buildCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.buildCalls)
}

// readFailureStreak sums every data point of the narvi/imagebuild meter's
// own image_build_failure_streak counter -- CUMULATIVE across every test in
// this binary (see TestMain's own doc comment / reconciler's identical
// precedent), so callers must diff a "before" and "after" reading around
// their own PumpOnce call(s) rather than asserting on the absolute value.
func readFailureStreak(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "narvi/imagebuild" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "image_build_failure_streak" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("image_build_failure_streak metric data = %T, want metricdata.Sum[int64]", m.Data)
			}
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	return 0
}

// readRefreshClaimReclaimed is readFailureStreak's own sibling for the
// image_refresh_claim_reclaimed OTel counter (audit-remediation batch B2)
// -- CUMULATIVE across every test in this binary, same caveat as
// readFailureStreak's own doc comment.
func readRefreshClaimReclaimed(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "narvi/imagebuild" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "image_refresh_claim_reclaimed" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("image_refresh_claim_reclaimed metric data = %T, want metricdata.Sum[int64]", m.Data)
			}
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	return 0
}

// readPermanentlyFailed is readFailureStreak's own sibling for the
// image_build_permanently_failed OTel counter (audit-remediation batch B3
// round 2, finding #3) -- CUMULATIVE across every test in this binary,
// same caveat as readFailureStreak's own doc comment.
func readPermanentlyFailed(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "narvi/imagebuild" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "image_build_permanently_failed" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("image_build_permanently_failed metric data = %T, want metricdata.Sum[int64]", m.Data)
			}
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	return 0
}

// dataPointAttrString looks up a single string-valued attribute on a
// metricdata data point's own attribute.Set -- shared by
// readImageBuildAttemptTotal/sumImageBuildDurationCount below, mirroring
// internal/sandboxagent/boot/telemetry_test.go's own
// findHookRerunDurationDataPointForRepo precedent (dp.Attributes.Value(...))
// for the identical "filter a cumulative, multi-test-binary metric stream
// down to the one attribute combination this test cares about" need.
func dataPointAttrString(attrs attribute.Set, key string) (string, bool) {
	v, ok := attrs.Value(attribute.Key(key))
	if !ok {
		return "", false
	}
	return v.AsString(), true
}

// readImageBuildAttemptTotal sums every image_build_attempt_total data
// point whose phase/outcome attributes match the given values --
// §19.9's own build-duration/failure-rate instrumentation (telemetry.go,
// §19.9's closing paragraph). CUMULATIVE across every test in this binary
// (readFailureStreak's own identical caveat, immediately above): callers
// must diff a "before" and "after" reading around their own
// PumpOnce/RefreshOnce call(s).
func readImageBuildAttemptTotal(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader, phase, outcome string) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "narvi/imagebuild" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "image_build_attempt_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("image_build_attempt_total metric data = %T, want metricdata.Sum[int64]", m.Data)
			}
			var total int64
			for _, dp := range sum.DataPoints {
				gotPhase, _ := dataPointAttrString(dp.Attributes, "phase")
				gotOutcome, _ := dataPointAttrString(dp.Attributes, "outcome")
				if gotPhase == phase && gotOutcome == outcome {
					total += dp.Value
				}
			}
			return total
		}
	}
	return 0
}

// sumImageBuildDurationCount is readImageBuildAttemptTotal's own sibling
// for the image_build_duration_seconds histogram -- same phase/outcome
// filter, same cumulative-across-the-binary caveat, returning the summed
// data-point COUNT (how many observations landed), not a value sum -- this
// package's own tests only need to prove a real data point was recorded
// for the right (phase, outcome), not any particular duration value (build
// duration in these tests is dominated by test-harness/DB latency, not
// anything meaningful to assert an exact bucket for).
func sumImageBuildDurationCount(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader, phase, outcome string) uint64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "narvi/imagebuild" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "image_build_duration_seconds" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("image_build_duration_seconds metric data = %T, want metricdata.Histogram[float64]", m.Data)
			}
			var total uint64
			for _, dp := range hist.DataPoints {
				gotPhase, _ := dataPointAttrString(dp.Attributes, "phase")
				gotOutcome, _ := dataPointAttrString(dp.Attributes, "outcome")
				if gotPhase == phase && gotOutcome == outcome {
					total += dp.Count
				}
			}
			return total
		}
	}
	return 0
}

// seedPendingImageBuild inserts a fresh 'pending' image_builds row directly
// (bypassing app/sessionactor entirely -- this package's own tests exercise
// Builder in isolation, matching its own doc.go's scope). repo_urls is
// seeded EMPTY (a base+runtime-only fingerprint) deliberately: §19.1's
// own "warm boot: shared fingerprint" mechanism has no claim-time SHA
// resolution mechanism yet (that's §19.2/§19.9 -- see attempt's
// own doc comment), so a row naming any repo can never actually reach a
// real BuildImage call in this package's own tests -- only a repo-less row
// can, which is exactly what these backoff/streak tests need to exercise
// (they're testing the retry/streak MECHANISM, orthogonal to which
// fingerprints §19.1 can build). See
// TestPumpOnce_RepoBearingRow_NoSHAResolutionYet_SkipsBuildImageCleanly
// below for the repo-bearing case's own dedicated coverage.
func seedPendingImageBuild(ctx context.Context, t *testing.T, store *narvipg.ImageBuildStore, fingerprint string) {
	t.Helper()

	repoURLs, err := json.Marshal(map[string]string{})
	if err != nil {
		t.Fatalf("marshal repo urls: %v", err)
	}
	if err := store.UpsertPending(ctx, sqlcgen.UpsertPendingImageBuildParams{
		Fingerprint:    fingerprint,
		Base:           "narvi/base:test",
		RepoUrls:       repoURLs,
		RuntimeVersion: "1.0.0-test",
	}); err != nil {
		t.Fatalf("seed pending image_builds row: %v", err)
	}
}

// seedPendingImageBuildWithRepos is seedPendingImageBuild's own sibling for
// tests that specifically need a REPO-BEARING pending row (i.e. exercising
// the "no claim-time SHA resolution yet" skip path, not the backoff/streak
// mechanism above).
func seedPendingImageBuildWithRepos(ctx context.Context, t *testing.T, store *narvipg.ImageBuildStore, fingerprint string, repoURLsIn map[string]string) {
	t.Helper()

	repoURLs, err := json.Marshal(repoURLsIn)
	if err != nil {
		t.Fatalf("marshal repo urls: %v", err)
	}
	if err := store.UpsertPending(ctx, sqlcgen.UpsertPendingImageBuildParams{
		Fingerprint:    fingerprint,
		Base:           "narvi/base:test",
		RepoUrls:       repoURLs,
		RuntimeVersion: "1.0.0-test",
	}); err != nil {
		t.Fatalf("seed pending image_builds row: %v", err)
	}
}

// seedReadyImageBuildWithRepos drives a repo-bearing fingerprint's own row
// from nonexistent through 'pending' -> 'building' -> 'ready' (UpsertPending,
// Claim, RecordSuccess), landing exactly the shape a real successful
// claim-time build leaves behind -- used by every freshness-pump test
// below, which all need a real 'ready' row to refresh, not a pending one.
func seedReadyImageBuildWithRepos(ctx context.Context, t *testing.T, store *narvipg.ImageBuildStore, fingerprint string, repoURLsIn, builtRepoSHAsIn map[string]string, imageRef string) {
	t.Helper()

	repoURLs, err := json.Marshal(repoURLsIn)
	if err != nil {
		t.Fatalf("marshal repo urls: %v", err)
	}
	if err := store.UpsertPending(ctx, sqlcgen.UpsertPendingImageBuildParams{
		Fingerprint:    fingerprint,
		Base:           "narvi/base:test",
		RepoUrls:       repoURLs,
		RuntimeVersion: "1.0.0-test",
	}); err != nil {
		t.Fatalf("seed pending image_builds row: %v", err)
	}
	if _, err := store.Claim(ctx, fingerprint); err != nil {
		t.Fatalf("claim image_builds row: %v", err)
	}
	builtRepoSHAs, err := json.Marshal(builtRepoSHAsIn)
	if err != nil {
		t.Fatalf("marshal built repo shas: %v", err)
	}
	if _, err := store.RecordSuccess(ctx, sqlcgen.RecordImageBuildSuccessParams{
		Fingerprint:   fingerprint,
		ImageRef:      &imageRef,
		BuiltRepoShas: builtRepoSHAs,
		BuiltAt:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("record image_builds success: %v", err)
	}
}

// TestRefreshOnce_StaleReadyRow_TipDiffers_RefreshesInPlace proves §19.2's
// own headline behavior: a 'ready' row whose recorded built_repo_shas no
// longer matches a repo's CURRENT default-branch tip gets refreshed --
// BuildImage is called with the NEW resolved SHA, and the row's own
// image_ref/built_repo_shas/built_at are atomically swapped IN PLACE
// (status stays 'ready' throughout, never touched).
func TestRefreshOnce_StaleReadyRow_TipDiffers_RefreshesInPlace(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-stale"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-old"},
		"narvi/built-image:old-ref")

	provider := &fakeBuildProvider{nextRef: "narvi/built-image:refreshed"}
	sourceControl := &fakeSourceControl{shaFor: map[string]string{"repo1": "sha-new"}}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	if got := provider.buildCallCount(); got != 1 {
		t.Fatalf("BuildImage call count = %d, want 1", got)
	}
	if got := provider.buildCalls[0].Repos["repo1"].SHA; got != "sha-new" {
		t.Errorf("BuildImage called with Repos[repo1].SHA = %q, want the NEW resolved tip %q", got, "sha-new")
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Status != sqlcgen.ImageBuildStatusReady {
		t.Errorf("status = %q, want %q (never leaves ready)", row.Status, sqlcgen.ImageBuildStatusReady)
	}
	if row.ImageRef == nil || *row.ImageRef != "narvi/built-image:refreshed" {
		t.Errorf("image_ref = %v, want the NEW ref %q", row.ImageRef, "narvi/built-image:refreshed")
	}
	if row.RefreshInProgress {
		t.Error("refresh_in_progress = true after a successful refresh, want false (claim released)")
	}

	var builtRepoSHAs map[string]string
	if err := json.Unmarshal(row.BuiltRepoShas, &builtRepoSHAs); err != nil {
		t.Fatalf("unmarshal built_repo_shas: %v", err)
	}
	if builtRepoSHAs["repo1"] != "sha-new" {
		t.Errorf("built_repo_shas[repo1] = %q, want the NEW resolved tip %q", builtRepoSHAs["repo1"], "sha-new")
	}
}

// TestRefreshOnce_FreshReadyRow_TipUnchanged_NoRefreshAttempted proves the
// other half of §19.2's own comparison: a 'ready' row whose recorded
// built_repo_shas ALREADY matches every repo's current tip is left
// completely untouched as far as its OWN build/status/image_ref go --
// BuildImage is never even called -- but this row WAS genuinely inspected
// this tick (its current tip was resolved and compared), so it must still
// call touchChecked and advance its own updated_at ordering key exactly
// like every other early-return branch (attemptRefresh's own top doc
// comment, invariant 1) -- a single-row test isolating EXACTLY this branch
// (unlike TestRefreshOnce_StarvationFreedom_GenuinelyStaleRowNotStarvedByStaticFrontCohort's
// own population-level, mixed-branch proof of the SAME invariant): reverting
// just this touchChecked call must fail ONLY this test, not merely the
// starvation test (audit-remediation batch B2 round 2 -- a prior adversarial
// review found this exact branch had no isolated coverage of its own).
func TestRefreshOnce_FreshReadyRow_TipUnchanged_NoRefreshAttempted(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-still-fresh"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-same"},
		"narvi/built-image:still-fresh")

	rowBefore, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row before: %v", err)
	}

	provider := &fakeBuildProvider{nextRef: "should-never-be-used"}
	sourceControl := &fakeSourceControl{shaFor: map[string]string{"repo1": "sha-same"}}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	time.Sleep(5 * time.Millisecond) // ensure a real clock delta from seeding
	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	if got := provider.buildCallCount(); got != 0 {
		t.Errorf("BuildImage call count = %d, want 0 (tip unchanged -- still fresh)", got)
	}

	rowAfter, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after: %v", err)
	}
	if !rowAfter.UpdatedAt.Time.After(rowBefore.UpdatedAt.Time) {
		t.Errorf("updated_at did not advance (before=%v after=%v) -- the NeedsRefresh-false branch must call touchChecked, or a genuinely-not-stale row would permanently occupy the front of ListReadyImageBuilds' own ORDER BY updated_at window (see ListReadyImageBuilds' own doc comment)", rowBefore.UpdatedAt.Time, rowAfter.UpdatedAt.Time)
	}
	if rowAfter.RefreshInProgress {
		t.Error("refresh_in_progress = true, want false (this branch never takes a claim)")
	}
	if rowAfter.ImageRef == nil || *rowAfter.ImageRef != "narvi/built-image:still-fresh" {
		t.Errorf("image_ref = %v, want unchanged %q", rowAfter.ImageRef, "narvi/built-image:still-fresh")
	}
}

// TestRefreshOnce_BaseOnlyReadyRow_NeverConsidered proves ListReadyImageBuilds'
// own SQL-level exclusion: a base-only (repo-less) ready row is never even
// inspected by the freshness pump -- it is never stale in the sense this
// design cares about (there is no repo tip to drift from).
func TestRefreshOnce_BaseOnlyReadyRow_NeverConsidered(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-base-only"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint, map[string]string{}, map[string]string{}, "narvi/built-image:base-only")

	provider := &fakeBuildProvider{nextRef: "should-never-be-used"}
	// nil sourceControl too -- if this row were ever (wrongly) inspected,
	// resolveRepoSHAs would fail loudly on the missing SourceControl long
	// before ever reaching BuildImage, so a zero build-call-count alone
	// already proves the row was never even considered.
	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), nil, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	if got := provider.buildCallCount(); got != 0 {
		t.Errorf("BuildImage call count = %d, want 0 (base-only row is never stale)", got)
	}
}

// TestRefreshOnce_NoCredentialConfigured_DegradesCleanly_OldRefStaysServable
// proves the deliberately-optional credential's degrade-cleanly behavior
// (§19.2) for the FRESHNESS PUMP specifically (the companion claim-time
// build case is covered by TestPumpOnce_RepoBearingRow_NoCredentialConfigured_DegradesCleanly
// above): with no platform credential configured, a stale 'ready' row is
// left completely untouched -- still 'ready', still serving its own OLD
// image_ref -- rather than crashing or corrupting the row.
func TestRefreshOnce_NoCredentialConfigured_DegradesCleanly_OldRefStaysServable(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-no-credential"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-old"},
		"narvi/built-image:old-ref")

	provider := &fakeBuildProvider{nextRef: "should-never-be-used"}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), nil, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	if got := provider.buildCallCount(); got != 0 {
		t.Errorf("BuildImage call count = %d, want 0 (no platform credential configured)", got)
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Status != sqlcgen.ImageBuildStatusReady {
		t.Errorf("status = %q, want %q (untouched)", row.Status, sqlcgen.ImageBuildStatusReady)
	}
	if row.ImageRef == nil || *row.ImageRef != "narvi/built-image:old-ref" {
		t.Errorf("image_ref = %v, want the OLD ref still intact %q", row.ImageRef, "narvi/built-image:old-ref")
	}
	if row.RefreshInProgress {
		t.Error("refresh_in_progress = true, want false (never claimed at all)")
	}
}

// TestRefreshOnce_BuildFails_ReleasesClaim_OldRefStaysServable proves the
// refresh path's own failure handling: a BuildImage failure during a
// refresh attempt releases the refresh_in_progress claim WITHOUT touching
// anything else -- the row is left exactly as it was (status 'ready', OLD
// image_ref/built_repo_shas intact), ready to be picked up again at the
// next tick.
func TestRefreshOnce_BuildFails_ReleasesClaim_OldRefStaysServable(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-build-fails"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-old"},
		"narvi/built-image:old-ref")

	provider := &fakeBuildProvider{nextErr: errors.New("provider: refresh build failed")}
	sourceControl := &fakeSourceControl{shaFor: map[string]string{"repo1": "sha-new"}}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Status != sqlcgen.ImageBuildStatusReady {
		t.Errorf("status = %q, want %q (a failed refresh never changes status)", row.Status, sqlcgen.ImageBuildStatusReady)
	}
	if row.ImageRef == nil || *row.ImageRef != "narvi/built-image:old-ref" {
		t.Errorf("image_ref = %v, want the OLD ref still intact %q", row.ImageRef, "narvi/built-image:old-ref")
	}
	if row.RefreshInProgress {
		t.Error("refresh_in_progress = true after a failed refresh, want false (claim released)")
	}

	// The claim must be genuinely released, not merely left readable as
	// 'false' by coincidence: a SECOND RefreshOnce call must be able to
	// re-claim and retry (the refresh path's own natural retry cadence, no
	// separate backoff schedule needed).
	provider.nextErr = nil
	provider.nextRef = "narvi/built-image:retried-successfully"
	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce (retry): %v", err)
	}
	row2, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after retry: %v", err)
	}
	if row2.ImageRef == nil || *row2.ImageRef != "narvi/built-image:retried-successfully" {
		t.Errorf("image_ref after retry = %v, want %q (claim was genuinely re-claimable)", row2.ImageRef, "narvi/built-image:retried-successfully")
	}
}

// TestRefreshOnce_OldRefStaysServableDuringRefresh is this Step's own
// direct proof of the resilience property the new "refresh-in-flight
// spawn" scenario (test/resilience) also exercises end to end: while a
// refresh build is genuinely IN FLIGHT (BuildImage blocked, not yet
// returned), a concurrent GetImageBuild-style read of the SAME row (the
// exact query a live spawn's own resolveAndSetImage performs) sees
// status='ready' and the OLD image_ref -- never 'building', never a gap.
func TestRefreshOnce_OldRefStaysServableDuringRefresh(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-in-flight"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-old"},
		"narvi/built-image:old-ref")

	release := make(chan struct{})
	provider := &blockingBuildProvider{nextRef: "narvi/built-image:refreshed", release: release}
	sourceControl := &fakeSourceControl{shaFor: map[string]string{"repo1": "sha-new"}}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- builder.RefreshOnce(ctx) }()

	// Wait for BuildImage to actually be entered (genuinely in flight),
	// then confirm a spawn-style read STILL sees the OLD ready image --
	// never blocked, never a gap, exactly as §19.2 requires.
	provider.waitUntilEntered(t, 5*time.Second)

	rowDuringRefresh, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row during in-flight refresh: %v", err)
	}
	if rowDuringRefresh.Status != sqlcgen.ImageBuildStatusReady {
		t.Fatalf("status during in-flight refresh = %q, want %q (a new spawn must never be blocked or degraded)", rowDuringRefresh.Status, sqlcgen.ImageBuildStatusReady)
	}
	if rowDuringRefresh.ImageRef == nil || *rowDuringRefresh.ImageRef != "narvi/built-image:old-ref" {
		t.Fatalf("image_ref during in-flight refresh = %v, want the OLD ref %q still servable", rowDuringRefresh.ImageRef, "narvi/built-image:old-ref")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	rowAfter, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after refresh completed: %v", err)
	}
	if rowAfter.ImageRef == nil || *rowAfter.ImageRef != "narvi/built-image:refreshed" {
		t.Errorf("image_ref after refresh completed = %v, want the NEW ref %q", rowAfter.ImageRef, "narvi/built-image:refreshed")
	}
}

// wantRefreshBatchSize mirrors imagebuild.refreshBatchSize's own value
// (builder.go) -- kept as a plain literal, not an import, because that
// constant is unexported and this file is package imagebuild_test (a
// black-box test package, matching every other test in this file). If
// builder.go's own refreshBatchSize ever changes, this must be updated to
// match, or TestRefreshOnce_BatchCap_BoundsRowsPerTickButPicksUpRemainderLater
// below will fail loudly rather than silently passing against the wrong
// number.
const wantRefreshBatchSize = 20

// TestRefreshOnce_BatchCap_BoundsRowsPerTickButPicksUpRemainderLater proves
// the batch-cap fix for this Step's own correctness/scalability review
// finding: RefreshOnce used to run ListReadyImageBuilds with NO limit at
// all, so an arbitrarily large fleet of simultaneously-stale Environments
// would all be attempted, strictly sequentially, in a single tick -- one
// slow/blocked BuildImage call could delay even STARTING every other
// Environment's own tip-SHA check for the rest of that tick.
//
// Seeds MORE genuinely-stale 'ready' rows than refreshBatchSize, runs
// exactly ONE RefreshOnce tick, and asserts EXACTLY refreshBatchSize of
// them were actually claimed/attempted (BuildImage called, image_ref
// swapped) -- not merely "the LIMIT clause exists somewhere in the SQL",
// but a real, observable bound on THIS tick's own effect. The remainder is
// then confirmed to be picked up on a SECOND, later tick -- proving the
// cap defers work rather than dropping it.
func TestRefreshOnce_BatchCap_BoundsRowsPerTickButPicksUpRemainderLater(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const totalRows = wantRefreshBatchSize + 5 // deliberately more than one tick's own cap
	const staleSHA = "sha-old"
	const currentSHA = "sha-new" // every repo's own "current" tip, per sourceControl.nextSHA below

	fingerprints := make([]string, totalRows)
	oldRefs := make([]string, totalRows)
	for i := 0; i < totalRows; i++ {
		fingerprint := fmt.Sprintf("fp-refresh-batch-cap-%02d", i)
		repoName := fmt.Sprintf("repo%02d", i)
		oldRef := fmt.Sprintf("narvi/built-image:old-ref-%02d", i)
		fingerprints[i] = fingerprint
		oldRefs[i] = oldRef

		seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
			map[string]string{repoName: fmt.Sprintf("https://github.com/acme/%s", repoName)},
			map[string]string{repoName: staleSHA},
			oldRef)
	}

	provider := &fakeBuildProvider{nextRef: "narvi/built-image:refreshed"}
	sourceControl := &fakeSourceControl{nextSHA: currentSHA} // every repo not explicitly listed falls back to this

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	// countRefreshed reports how many of the totalRows fingerprints have
	// already been swapped to the NEW image_ref (i.e. genuinely refreshed
	// so far, across however many ticks have run).
	countRefreshed := func() int {
		t.Helper()
		refreshed := 0
		for i := 0; i < totalRows; i++ {
			row, err := store.Get(ctx, fingerprints[i])
			if err != nil {
				t.Fatalf("get row %q: %v", fingerprints[i], err)
			}
			if row.ImageRef == nil {
				t.Fatalf("row %q has a nil image_ref", fingerprints[i])
			}
			switch *row.ImageRef {
			case oldRefs[i]:
				// still untouched this tick
			case "narvi/built-image:refreshed":
				refreshed++
			default:
				t.Fatalf("row %q has unexpected image_ref %q", fingerprints[i], *row.ImageRef)
			}
		}
		return refreshed
	}

	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce (tick 1): %v", err)
	}

	if got := provider.buildCallCount(); got != wantRefreshBatchSize {
		t.Fatalf("BuildImage call count after ONE tick = %d, want exactly %d (the batch cap) -- an unbounded RefreshOnce would call BuildImage %d times in this one tick", got, wantRefreshBatchSize, totalRows)
	}
	if got := countRefreshed(); got != wantRefreshBatchSize {
		t.Fatalf("refreshed row count after tick 1 = %d, want exactly %d", got, wantRefreshBatchSize)
	}

	// The remainder (totalRows - wantRefreshBatchSize rows) must NOT have
	// been silently dropped -- a SECOND tick must pick them up. Every row
	// refreshed in tick 1 just had its own updated_at bumped (by
	// ClaimImageBuildForRefresh, then RecordImageRefreshSuccess), so
	// ListReadyImageBuilds' own ORDER BY updated_at guarantees the
	// still-untouched rows (whose updated_at dates back to seeding, before
	// tick 1 ever ran) sort first in tick 2's own batch -- they are
	// therefore ALL included this time, genuinely stale, and get refreshed
	// for real (not a no-op: their own built_repo_shas still says staleSHA,
	// so domainimagebuild.NeedsRefresh still reports true for them).
	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce (tick 2): %v", err)
	}

	wantTotalBuildCalls := totalRows // every originally-stale row refreshed exactly once, across the two ticks combined
	if got := provider.buildCallCount(); got != wantTotalBuildCalls {
		t.Fatalf("BuildImage call count after tick 2 = %d, want %d (every originally-stale row refreshed exactly once, across both ticks combined)", got, wantTotalBuildCalls)
	}
	if got := countRefreshed(); got != totalRows {
		t.Fatalf("refreshed row count after tick 2 = %d, want %d (every row picked up eventually, none dropped)", got, totalRows)
	}
}

// TestRefreshOnce_StarvationFreedom_GenuinelyStaleRowNotStarvedByStaticFrontCohort
// proves the correctness review finding on the batch-cap fix above (see
// ListReadyImageBuilds' and attemptRefresh's own generated/doc comments
// for the full mechanism): TestRefreshOnce_BatchCap_BoundsRowsPerTickButPicksUpRemainderLater
// cannot catch this, because EVERY row it seeds is uniformly,
// permanently stale against the SAME fake tip -- every row eventually
// reaches ClaimForRefresh and gets its own updated_at bumped that way, so
// that test's own population shape structurally cannot exercise the
// starvation case: a mix where >= refreshBatchSize rows are genuinely NOT
// stale (or persistently SHA-resolution-failing) alongside a newer,
// genuinely-stale row.
//
// This test builds exactly that population:
//   - A "front cohort" of EXACTLY refreshBatchSize rows, seeded FIRST (so
//     their own updated_at is OLDER), split evenly between:
//   - genuinely NOT stale (their built_repo_shas already match the
//     fake SourceControl's current tip -- NeedsRefresh reports false
//     every single tick), and
//   - PERSISTENTLY SHA-resolution-failing (models a renamed/deleted
//     repo, or a token missing org access -- resolveRepoSHAs errors
//     every single tick).
//     Neither sub-population EVER reaches ClaimForRefresh.
//   - One additional row that IS genuinely stale, seeded AFTER the front
//     cohort (so it starts with a NEWER updated_at) -- exactly the "went
//     stale after the front cohort" shape the bug report describes.
//
// Before this Step's fix: the front cohort's own updated_at never
// advances (neither early-return branch touched it), so it sorts first
// FOREVER and permanently fills every tick's own LIMIT refreshBatchSize
// window -- the genuinely-stale row, sorting behind it, is never even
// returned by ListReadyImageBuilds, let alone refreshed, no matter how
// many ticks run.
//
// After the fix: the front cohort's own updated_at is bumped every tick
// via TouchImageBuildChecked, so by the SECOND tick the genuinely-stale
// row (never touched, still older than anything the first tick touched)
// becomes the oldest row in the table and is guaranteed to appear in that
// tick's own batch. This test asserts the EXACT tick the refresh happens
// on (tick 2, never tick 1, never "eventually") -- an audit finding on
// this test itself: the original version only asserted "refreshed within
// maxTicks=3", which cannot distinguish genuine, deterministic rotation
// from merely "got lucky within the batch window", and (this exact
// population's own arithmetic) also cannot by itself distinguish a full
// fix from a PARTIAL regression that removes just one of attemptRefresh's
// own touchChecked call sites -- with a 10/10 split front cohort, either
// half alone rotating is already enough to open a slot for the stale row
// by tick 2. Isolating a single call site's own regression is instead
// what the dedicated per-branch tests below (TestAttemptRefresh_*) are
// for -- each one seeds a population of exactly ONE, mutation-tested
// completely independently of this test's own population shape.
func TestRefreshOnce_StarvationFreedom_GenuinelyStaleRowNotStarvedByStaticFrontCohort(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const frontCohortSize = wantRefreshBatchSize // exactly refreshBatchSize -- fills one whole tick's own window by itself

	sourceControl := &fakeSourceControl{
		shaFor: map[string]string{},
		errFor: map[string]error{},
	}

	frontFingerprints := make([]string, frontCohortSize)
	frontOldRefs := make([]string, frontCohortSize)
	for i := 0; i < frontCohortSize; i++ {
		fingerprint := fmt.Sprintf("fp-starvation-front-%02d", i)
		oldRef := fmt.Sprintf("narvi/built-image:front-%02d", i)
		frontFingerprints[i] = fingerprint
		frontOldRefs[i] = oldRef

		if i%2 == 0 {
			// Genuinely NOT stale: built_repo_shas already matches the
			// current tip this test's sourceControl will resolve.
			repoName := fmt.Sprintf("not-stale-repo-%02d", i)
			sourceControl.shaFor[repoName] = "sha-not-stale"
			seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
				map[string]string{repoName: fmt.Sprintf("https://github.com/acme/%s", repoName)},
				map[string]string{repoName: "sha-not-stale"},
				oldRef)
		} else {
			// Persistently SHA-resolution-failing: every resolveRepoSHAs
			// call for this repo errors, every tick.
			repoName := fmt.Sprintf("failing-repo-%02d", i)
			sourceControl.errFor[repoName] = fmt.Errorf("fake: repo %q renamed/deleted, or token lacks org access", repoName)
			seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
				map[string]string{repoName: fmt.Sprintf("https://github.com/acme/%s", repoName)},
				map[string]string{repoName: "sha-irrelevant"},
				oldRef)
		}
	}

	// The genuinely-stale row: seeded AFTER the entire front cohort above,
	// so it starts with a NEWER updated_at -- it "went stale after" the
	// front cohort, exactly as the review's own failure scenario requires.
	const staleFingerprint = "fp-starvation-genuinely-stale"
	const staleRepoName = "genuinely-stale-repo"
	const staleOldRef = "narvi/built-image:stale-old-ref"
	const staleNewRef = "narvi/built-image:stale-refreshed"
	sourceControl.shaFor[staleRepoName] = "sha-new-tip"
	seedReadyImageBuildWithRepos(ctx, t, store, staleFingerprint,
		map[string]string{staleRepoName: fmt.Sprintf("https://github.com/acme/%s", staleRepoName)},
		map[string]string{staleRepoName: "sha-old-tip"},
		staleOldRef)

	provider := &fakeBuildProvider{nextRef: staleNewRef}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	// This population's own arithmetic makes the refresh tick EXACTLY
	// predictable, not merely "eventually within some headroom" (an audit
	// finding on this test itself: asserting only "nonzero within maxTicks"
	// cannot distinguish genuine fair rotation from a partial regression --
	// see this test's own top doc comment for why a per-branch test, not
	// this population-shape test, is what actually isolates a SINGLE
	// touchChecked call going missing). With the fix applied: tick 1's own
	// ListReady(20) ORDER BY updated_at returns exactly the front cohort
	// (all 20 strictly older than the stale row, which was seeded last) --
	// the stale row is not even RETURNED yet, so it must NOT be refreshed
	// after tick 1. Every front-cohort row advances its own updated_at
	// during tick 1 (touchChecked, both halves), landing all 20 at
	// "tick 1's own now()" -- strictly after the stale row's own
	// (untouched) updated_at -- so tick 2's own ListReady(20) is guaranteed
	// to include the stale row (now the single oldest of all 21) and
	// refresh it for real.
	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce (tick 1): %v", err)
	}
	rowAfterTick1, err := store.Get(ctx, staleFingerprint)
	if err != nil {
		t.Fatalf("get stale row after tick 1: %v", err)
	}
	if rowAfterTick1.ImageRef == nil || *rowAfterTick1.ImageRef != staleOldRef {
		t.Fatalf("stale row image_ref after tick 1 = %v, want unchanged %q -- it must not even be RETURNED by ListReady yet (the full front cohort of %d fills that tick's own LIMIT window)", rowAfterTick1.ImageRef, staleOldRef, frontCohortSize)
	}

	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce (tick 2): %v", err)
	}
	rowAfterTick2, err := store.Get(ctx, staleFingerprint)
	if err != nil {
		t.Fatalf("get stale row after tick 2: %v", err)
	}
	if rowAfterTick2.ImageRef == nil || *rowAfterTick2.ImageRef != staleNewRef {
		t.Fatalf("genuinely-stale row %q was NOT refreshed at EXACTLY tick 2 (image_ref = %v) -- starved behind a static front cohort of %d rows that never advance their own ordering key", staleFingerprint, rowAfterTick2.ImageRef, frontCohortSize)
	}

	// The front cohort itself must never have been (wrongly) refreshed --
	// the not-stale half because BuildImage must never be called for a
	// row that isn't stale, the persistently-failing half because
	// resolution never succeeds for it.
	for i, fingerprint := range frontFingerprints {
		row, err := store.Get(ctx, fingerprint)
		if err != nil {
			t.Fatalf("get front-cohort row %q: %v", fingerprint, err)
		}
		if row.ImageRef == nil || *row.ImageRef != frontOldRefs[i] {
			t.Errorf("front-cohort row %q image_ref = %v, want unchanged %q (must never be refreshed)", fingerprint, row.ImageRef, frontOldRefs[i])
		}
		if row.Status != sqlcgen.ImageBuildStatusReady {
			t.Errorf("front-cohort row %q status = %q, want %q", fingerprint, row.Status, sqlcgen.ImageBuildStatusReady)
		}
	}
}

// blockingBuildProvider is a test-only ports.SandboxProvider whose
// BuildImage signals entered (closed once) the INSTANT it is called, then
// blocks until release is closed -- lets a test observe genuinely
// in-flight behavior (as opposed to fakeBuildProvider's own
// immediately-returning shape).
type blockingBuildProvider struct {
	mu      sync.Mutex
	entered chan struct{}
	once    sync.Once

	nextRef ports.BuildRef
	release <-chan struct{}
}

var _ ports.SandboxProvider = (*blockingBuildProvider)(nil)

func (f *blockingBuildProvider) Capabilities() ports.Capabilities {
	return ports.Capabilities{ImageBuilds: true}
}
func (f *blockingBuildProvider) CreateSandbox(context.Context, ports.CreateSpec) (ports.SandboxRef, error) {
	return ports.SandboxRef{}, errors.New("blockingBuildProvider: CreateSandbox not implemented")
}
func (f *blockingBuildProvider) StopSandbox(context.Context, ports.SandboxRef) error {
	return errors.New("blockingBuildProvider: StopSandbox not implemented")
}
func (f *blockingBuildProvider) ResumeSandbox(context.Context, ports.SandboxRef) error {
	return errors.New("blockingBuildProvider: ResumeSandbox not implemented")
}
func (f *blockingBuildProvider) TakeSnapshot(context.Context, ports.SandboxRef) (ports.SnapshotID, error) {
	return "", errors.New("blockingBuildProvider: TakeSnapshot not implemented")
}
func (f *blockingBuildProvider) RestoreFromSnapshot(context.Context, ports.SnapshotID, ports.CreateSpec) (ports.SandboxRef, error) {
	return ports.SandboxRef{}, errors.New("blockingBuildProvider: RestoreFromSnapshot not implemented")
}

func (f *blockingBuildProvider) BuildImage(ctx context.Context, _ ports.ImageSpec) (ports.BuildOutcome, error) {
	f.mu.Lock()
	if f.entered == nil {
		f.entered = make(chan struct{})
	}
	entered := f.entered
	f.mu.Unlock()
	f.once.Do(func() { close(entered) })

	select {
	case <-f.release:
		return ports.BuildOutcome{Ref: f.nextRef}, nil
	case <-ctx.Done():
		return ports.BuildOutcome{}, ctx.Err()
	}
}

func (f *blockingBuildProvider) DeleteImage(context.Context, ports.ImageRef) error {
	return errors.New("blockingBuildProvider: DeleteImage not implemented")
}
func (f *blockingBuildProvider) List(context.Context) ([]ports.SandboxRef, error) { return nil, nil }

func (f *blockingBuildProvider) waitUntilEntered(t *testing.T, timeout time.Duration) {
	t.Helper()
	f.mu.Lock()
	if f.entered == nil {
		f.entered = make(chan struct{})
	}
	entered := f.entered
	f.mu.Unlock()

	select {
	case <-entered:
	case <-time.After(timeout):
		t.Fatal("BuildImage was never entered within the timeout")
	}
}

// TestPumpOnce_FailedBuild_BacksOffAndNotRetriedBeforeNextRetryAt proves
// scenario (c): a failed build attempt is NOT retried before its own
// next_retry_at, confirmed by DIRECT Postgres inspection (not log-reading)
// -- a second PumpOnce call immediately after the first must not re-claim
// the still-not-due row (attempt_count/BuildImage call count stay at 1),
// but once next_retry_at has genuinely elapsed, a later PumpOnce DOES pick
// it up again (attempt_count advances to 2).
func TestPumpOnce_FailedBuild_BacksOffAndNotRetriedBeforeNextRetryAt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-scenario-c"
	seedPendingImageBuild(ctx, t, store, fingerprint)

	provider := &fakeBuildProvider{nextErr: errors.New("provider: build failed")}
	timeouts := platform.DefaultTimeouts()
	timeouts.ImageBuildBackoffBase = 200 * time.Millisecond
	timeouts.ImageBuildBackoffMax = 500 * time.Millisecond

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, timeouts, nil, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	// First tick: claims the pending row, BuildImage fails, backs off.
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce (1st): %v", err)
	}
	if provider.buildCallCount() != 1 {
		t.Fatalf("BuildImage call count after 1st PumpOnce = %d, want 1", provider.buildCallCount())
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after 1st PumpOnce: %v", err)
	}
	if row.Status != sqlcgen.ImageBuildStatusFailed {
		t.Fatalf("status after 1st failure = %q, want %q", row.Status, sqlcgen.ImageBuildStatusFailed)
	}
	if row.AttemptCount != 1 {
		t.Fatalf("attempt_count after 1st failure = %d, want 1", row.AttemptCount)
	}
	if !row.NextRetryAt.Valid {
		t.Fatal("next_retry_at is not set after a failed attempt")
	}
	nextRetryAt := row.NextRetryAt.Time
	if !nextRetryAt.After(time.Now()) {
		t.Fatalf("next_retry_at = %v, want strictly in the future immediately after recording the failure", nextRetryAt)
	}

	// Second tick, immediately: next_retry_at has NOT elapsed yet -- must
	// NOT be re-claimed. Confirmed by BOTH the provider's own call count
	// (still 1) AND a direct Postgres re-read (attempt_count still 1,
	// next_retry_at UNCHANGED).
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce (2nd, immediate): %v", err)
	}
	if provider.buildCallCount() != 1 {
		t.Fatalf("BuildImage call count after 2nd (too-early) PumpOnce = %d, want still 1 (not yet due)", provider.buildCallCount())
	}
	row2, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after 2nd PumpOnce: %v", err)
	}
	if row2.AttemptCount != 1 {
		t.Fatalf("attempt_count after 2nd (too-early) PumpOnce = %d, want still 1", row2.AttemptCount)
	}
	if !row2.NextRetryAt.Time.Equal(nextRetryAt) {
		t.Fatalf("next_retry_at changed on a too-early tick: was %v, now %v", nextRetryAt, row2.NextRetryAt.Time)
	}

	// Wait out the real backoff window, then a THIRD tick must genuinely
	// retry (attempt_count advances to 2) -- proving this isn't merely
	// "never retries again", but specifically "not before next_retry_at".
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && provider.buildCallCount() < 2 {
		time.Sleep(20 * time.Millisecond)
		if err := builder.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce (retry loop): %v", err)
		}
	}
	if provider.buildCallCount() != 2 {
		t.Fatalf("BuildImage call count after waiting out the backoff = %d, want 2 (retried once due)", provider.buildCallCount())
	}
	row3, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after retry: %v", err)
	}
	if row3.AttemptCount != 2 {
		t.Fatalf("attempt_count after the due retry = %d, want 2", row3.AttemptCount)
	}
}

// TestPumpOnce_FailureStreak_FiresAtThresholdNotBefore proves scenario (e):
// the streak-threshold log/metric (image_build_failure_streak) fires after
// domain/imagebuild.ImageBuildStreakThreshold consecutive failed attempts
// for the SAME fingerprint, and NOT before -- driven by real,
// consecutive PumpOnce ticks against real Postgres, asserted via the real
// OTel counter's delta (readFailureStreak).
func TestPumpOnce_FailureStreak_FiresAtThresholdNotBefore(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-scenario-e"
	seedPendingImageBuild(ctx, t, store, fingerprint)

	provider := &fakeBuildProvider{nextErr: errors.New("provider: build always fails")}
	// A tiny, test-only backoff so consecutive due ticks resolve almost
	// immediately -- this test cares about attempt_count/streak behavior,
	// not real backoff timing (already covered directly by
	// TestPumpOnce_FailedBuild_BacksOffAndNotRetriedBeforeNextRetryAt
	// above).
	timeouts := platform.DefaultTimeouts()
	timeouts.ImageBuildBackoffBase = 1 * time.Millisecond
	timeouts.ImageBuildBackoffMax = 5 * time.Millisecond

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, timeouts, nil, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	before := readFailureStreak(ctx, t, imagebuild.IntegrationOtelReader)

	tickUntilDue := func(wantAttempt int32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if err := builder.PumpOnce(ctx); err != nil {
				t.Fatalf("PumpOnce: %v", err)
			}
			row, err := store.Get(ctx, fingerprint)
			if err != nil {
				t.Fatalf("get row: %v", err)
			}
			if row.AttemptCount == wantAttempt {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("attempt_count never reached %d (stuck at %d)", wantAttempt, row.AttemptCount)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Attempts 1 and 2 (below domain/imagebuild.ImageBuildStreakThreshold,
	// which is 3): the streak counter must NOT move.
	tickUntilDue(1)
	if got := readFailureStreak(ctx, t, imagebuild.IntegrationOtelReader) - before; got != 0 {
		t.Fatalf("failure streak delta after attempt 1 = %d, want 0 (below threshold %d)", got, domainimagebuild.ImageBuildStreakThreshold)
	}

	tickUntilDue(2)
	if got := readFailureStreak(ctx, t, imagebuild.IntegrationOtelReader) - before; got != 0 {
		t.Fatalf("failure streak delta after attempt 2 = %d, want 0 (still below threshold %d)", got, domainimagebuild.ImageBuildStreakThreshold)
	}

	// Attempt 3 crosses the threshold: the counter must increment.
	tickUntilDue(3)
	if got := readFailureStreak(ctx, t, imagebuild.IntegrationOtelReader) - before; got != 1 {
		t.Fatalf("failure streak delta after attempt 3 (at threshold %d) = %d, want 1", domainimagebuild.ImageBuildStreakThreshold, got)
	}

	// Attempt 4 (still at/beyond threshold): fires again.
	tickUntilDue(4)
	if got := readFailureStreak(ctx, t, imagebuild.IntegrationOtelReader) - before; got != 2 {
		t.Fatalf("failure streak delta after attempt 4 (beyond threshold) = %d, want 2", got)
	}
}

// TestPumpOnce_RepoBearingRow_NoCredentialConfigured_DegradesCleanly proves
// §19.2's own degrade-cleanly design for the deliberately-optional
// platform credential (§19.2): a missing/invalid credential is logged and
// recorded as a failed attempt via the same retry/backoff path any other
// resolution failure uses -- never a crash, never something that blocks
// a spawn. A claimed row naming at least one repo, with NO platform
// GitHub credential configured (the
// Builder built with an empty token, mirroring platform.Config.
// GitHubImageBuildToken's own documented "optional, empty means not
// configured" contract), is handled as a clean, well-defined, tested
// behavior -- NEVER a crash and NEVER a BuildImage call carrying an
// empty/zero SHA:
//   - BuildImage is never even called;
//   - the row is recorded as a failed attempt (attempt_count/next_retry_at
//     advance via the SAME domain/imagebuild.EvaluateBackoff schedule any
//     other failure uses), so it keeps cycling through backoff rather than
//     being stuck in 'building' forever -- ready to actually build for real
//     the moment an operator provisions the credential (see the companion
//     success test immediately below).
func TestPumpOnce_RepoBearingRow_NoCredentialConfigured_DegradesCleanly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-repo-bearing-no-credential"
	seedPendingImageBuildWithRepos(ctx, t, store, fingerprint, map[string]string{
		"repo1": "https://github.com/acme/repo1",
	})

	// A provider that would fail the test outright if BuildImage were ever
	// actually invoked -- this test's whole point is that it must NOT be.
	provider := &fakeBuildProvider{nextRef: "should-never-be-used"}

	timeouts := platform.DefaultTimeouts()
	timeouts.ImageBuildBackoffBase = 200 * time.Millisecond
	timeouts.ImageBuildBackoffMax = 500 * time.Millisecond

	sourceControl := &fakeSourceControl{nextSHA: "sha-should-never-be-used"}

	// gitHubImageBuildToken is DELIBERATELY empty here -- the credential
	// is not configured, mirroring a real deploy that has not yet
	// provisioned it.
	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, timeouts, sourceControl, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if got := provider.buildCallCount(); got != 0 {
		t.Fatalf("BuildImage call count = %d, want 0 (no platform credential configured -- degrade cleanly)", got)
	}
	if got := sourceControl.shaCallCount(); got != 0 {
		t.Fatalf("ResolveBranchSHA call count = %d, want 0 (the missing-credential check must short-circuit before ANY resolution call)", got)
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Status != sqlcgen.ImageBuildStatusFailed {
		t.Errorf("status = %q, want %q (recorded as a retryable failure, not stuck in 'building')", row.Status, sqlcgen.ImageBuildStatusFailed)
	}
	if row.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1", row.AttemptCount)
	}
	if !row.NextRetryAt.Valid || !row.NextRetryAt.Time.After(time.Now()) {
		t.Errorf("next_retry_at = %v, want set and strictly in the future", row.NextRetryAt)
	}
	if row.ImageRef != nil {
		t.Errorf("image_ref = %v, want nil (never built)", row.ImageRef)
	}
	if row.BuiltRepoShas != nil {
		t.Errorf("built_repo_shas = %v, want nil (never built)", row.BuiltRepoShas)
	}
	if row.BuiltAt.Valid {
		t.Errorf("built_at = %v, want unset (never built)", row.BuiltAt)
	}
}

// TestPumpOnce_RepoBearingRow_CredentialConfigured_ResolvesSHAsAndBuilds
// proves §19.2's own headline claim-time SHA resolution (§19.1/§19.2/
// §19.9): with a real platform credential AND SourceControl configured, a
// claimed row naming a repo now DOES build for real -- ResolveBranchSHA is
// called once per named repo (with an empty Branch, resolving the repo's
// own default-branch tip, never a session-specific branch), BuildImage
// receives a REAL, concrete ports.RepoRef{URL, SHA} (never an empty/zero
// SHA), and the recorded built_repo_shas carries exactly the resolved
// SHA -- the shape §19.2's own later freshness pump needs to compare a
// future tip against.
func TestPumpOnce_RepoBearingRow_CredentialConfigured_ResolvesSHAsAndBuilds(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-repo-bearing-with-credential"
	seedPendingImageBuildWithRepos(ctx, t, store, fingerprint, map[string]string{
		"repo1": "https://github.com/acme/repo1",
	})

	provider := &fakeBuildProvider{nextRef: "narvi/built-image:claim-time-resolved"}
	sourceControl := &fakeSourceControl{shaFor: map[string]string{"repo1": "sha-resolved-repo1"}}

	timeouts := platform.DefaultTimeouts()
	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, timeouts, sourceControl, "test-platform-github-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if got := sourceControl.shaCallCount(); got != 1 {
		t.Fatalf("ResolveBranchSHA call count = %d, want 1 (one repo named)", got)
	}
	gotSpec := sourceControl.shaCalls[0]
	if gotSpec.Owner != "acme" || gotSpec.Repo != "repo1" {
		t.Errorf("ResolveBranchSHA called with Owner=%q Repo=%q, want Owner=%q Repo=%q", gotSpec.Owner, gotSpec.Repo, "acme", "repo1")
	}
	if gotSpec.Branch != "" {
		t.Errorf("ResolveBranchSHA called with Branch=%q, want empty (always the repo's own default branch tip)", gotSpec.Branch)
	}
	if gotSpec.Token != "test-platform-github-token" {
		t.Errorf("ResolveBranchSHA called with Token=%q, want the platform credential %q", gotSpec.Token, "test-platform-github-token")
	}

	if got := provider.buildCallCount(); got != 1 {
		t.Fatalf("BuildImage call count = %d, want 1", got)
	}
	gotBuildSpec := provider.buildCalls[0]
	if gotBuildSpec.Repos["repo1"].SHA != "sha-resolved-repo1" {
		t.Errorf("BuildImage called with Repos[repo1].SHA = %q, want the resolved %q", gotBuildSpec.Repos["repo1"].SHA, "sha-resolved-repo1")
	}
	if gotBuildSpec.Repos["repo1"].URL != "https://github.com/acme/repo1" {
		t.Errorf("BuildImage called with Repos[repo1].URL = %q, want %q", gotBuildSpec.Repos["repo1"].URL, "https://github.com/acme/repo1")
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Status != sqlcgen.ImageBuildStatusReady {
		t.Fatalf("status = %q, want %q", row.Status, sqlcgen.ImageBuildStatusReady)
	}
	if row.ImageRef == nil || *row.ImageRef != "narvi/built-image:claim-time-resolved" {
		t.Errorf("image_ref = %v, want %q", row.ImageRef, "narvi/built-image:claim-time-resolved")
	}

	var builtRepoSHAs map[string]string
	if err := json.Unmarshal(row.BuiltRepoShas, &builtRepoSHAs); err != nil {
		t.Fatalf("unmarshal built_repo_shas: %v", err)
	}
	if builtRepoSHAs["repo1"] != "sha-resolved-repo1" {
		t.Errorf("built_repo_shas[repo1] = %q, want %q", builtRepoSHAs["repo1"], "sha-resolved-repo1")
	}
	if !row.BuiltAt.Valid {
		t.Error("built_at is not set, want a real timestamp")
	}
}

// TestPumpOnce_RepoBearingRow_UnsupportedHost_FailsCleanlyNeverCallsSourceControl
// is audit-remediation batch B3's own regression test for the finding
// this batch closes: a repo_urls entry naming a NON-GitHub host (a
// GitLab URL passes reposource.ValidateRepoURL -- it accepts any https
// host -- and used to reach parseOwnerRepo/ResolveBranchSHA completely
// unchecked) must never reach b.sourceControl.ResolveBranchSHA at all --
// doing so would silently query GitHub's real API for a coincidentally-
// matching owner/repo path, either failing confusingly or, worse,
// resolving a SHA against a completely unrelated repo. Proves the fix
// fails LOUDLY (recorded as a failed attempt, same as any other
// resolveRepoSHAs failure) rather than silently succeeding against the
// wrong host.
//
// Audit-remediation batch B3 round 2 (finding #3) extends this test: this
// is a PERMANENT condition (no retry ever makes an unsupported host become
// supported), so the row is now recorded via recordPermanentFailure
// (permanently_failed=true, next_retry_at cleared) rather than cycling
// through the ordinary EvaluateBackoff schedule forever -- and a SECOND
// PumpOnce tick, run after this row's own attempt_count and updated_at
// would otherwise make it look "due" again under the OLD behavior, proves
// it is never reclaimed a second time (ListDueImageBuilds' own
// "AND permanently_failed = false" guard).
func TestPumpOnce_RepoBearingRow_UnsupportedHost_FailsCleanlyNeverCallsSourceControl(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-repo-bearing-unsupported-host"
	seedPendingImageBuildWithRepos(ctx, t, store, fingerprint, map[string]string{
		"repo1": "https://gitlab.example.com/acme/widgets",
	})

	// A SourceControl that would fail the test outright if ResolveBranchSHA
	// were ever actually invoked -- this test's whole point is that it must
	// NOT be, for a repo url naming a host other than github.com.
	sourceControl := &fakeSourceControl{nextErr: errors.New("fakeSourceControl: ResolveBranchSHA must never be called for a non-GitHub host")}
	provider := &fakeBuildProvider{nextRef: "should-never-be-used"}

	timeouts := platform.DefaultTimeouts()
	timeouts.ImageBuildBackoffBase = 200 * time.Millisecond
	timeouts.ImageBuildBackoffMax = 500 * time.Millisecond

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, timeouts, sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	beforePermanentlyFailed := readPermanentlyFailed(ctx, t, imagebuild.IntegrationOtelReader)

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if got := sourceControl.shaCallCount(); got != 0 {
		t.Fatalf("ResolveBranchSHA call count = %d, want 0 (an unsupported host must be rejected BEFORE any resolution call, never silently resolved against the wrong host)", got)
	}
	if got := provider.buildCallCount(); got != 0 {
		t.Fatalf("BuildImage call count = %d, want 0", got)
	}
	if after := readPermanentlyFailed(ctx, t, imagebuild.IntegrationOtelReader); after-beforePermanentlyFailed != 1 {
		t.Errorf("image_build_permanently_failed counter delta = %d, want 1", after-beforePermanentlyFailed)
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Status != sqlcgen.ImageBuildStatusFailed {
		t.Errorf("status = %q, want %q (recorded as a real, loud failure -- never a silent success against the wrong host)", row.Status, sqlcgen.ImageBuildStatusFailed)
	}
	if !row.PermanentlyFailed {
		t.Errorf("permanently_failed = false, want true (an unsupported host is a PERMANENT condition -- no retry will ever clear it)")
	}
	if row.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1", row.AttemptCount)
	}
	if row.NextRetryAt.Valid {
		t.Errorf("next_retry_at = %v, want unset/NULL (a permanently-failed row is never scheduled for another retry)", row.NextRetryAt)
	}
	if row.ImageRef != nil {
		t.Errorf("image_ref = %v, want nil (never built)", row.ImageRef)
	}
	if row.BuiltRepoShas != nil {
		t.Errorf("built_repo_shas = %v, want nil (never built)", row.BuiltRepoShas)
	}

	// A second tick must never reclaim this fingerprint again -- this is
	// the core of finding #3's own fix: before it, a 'failed' row with a
	// past-due next_retry_at would be picked right back up by
	// ListDueImageBuilds, re-attempted, and fail again, forever.
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce (second tick): %v", err)
	}
	if got := sourceControl.shaCallCount(); got != 0 {
		t.Errorf("ResolveBranchSHA call count after second tick = %d, want 0 (a permanently-failed fingerprint must never be reclaimed again)", got)
	}

	rowAfterSecondTick, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after second tick: %v", err)
	}
	if rowAfterSecondTick.AttemptCount != 1 {
		t.Errorf("attempt_count after second tick = %d, want 1 (unchanged -- never reclaimed)", rowAfterSecondTick.AttemptCount)
	}
	if after := readPermanentlyFailed(ctx, t, imagebuild.IntegrationOtelReader); after-beforePermanentlyFailed != 1 {
		t.Errorf("image_build_permanently_failed counter delta after second tick = %d, want 1 (still one-shot -- never reclaimed, so never re-fires)", after-beforePermanentlyFailed)
	}
}

// TestPumpOnce_RepoBearingRow_UnsupportedHost_ExampleOrg_FailsCleanly is
// audit-remediation batch B3 round 2's own regression test for finding #7
// (allowlist drift between this package's own resolveRepoSHAs gate and
// app/sessionactor's identical imageresolve.go gate): deliberately uses
// "example.org" -- the EXACT unsupported-host fixture
// internal/app/sessionactor/repoaccessgate_integration_test.go's own
// TestRepoAccessGate_UnsupportedRepoHost_DeniesWarmBootNoAccessCall uses --
// rather than this file's own pre-existing "gitlab.example.com" fixture
// (TestPumpOnce_RepoBearingRow_UnsupportedHost_FailsCleanlyNeverCallsSourceControl,
// above). Before this batch, an adversarial NARROWING or divergence of
// ONLY this package's own gate could have passed unnoticed since no test
// here ever exercised "example.org" specifically -- this test, together
// with imageresolve's own "gitlab.example.com" sibling test, proves both
// gates agree on the SAME extra host in both directions, now that both
// route through the shared ports.SupportedSourceControlHosts().
func TestPumpOnce_RepoBearingRow_UnsupportedHost_ExampleOrg_FailsCleanly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-repo-bearing-unsupported-host-example-org"
	seedPendingImageBuildWithRepos(ctx, t, store, fingerprint, map[string]string{
		"repo1": "https://example.org/acme/tools.git",
	})

	sourceControl := &fakeSourceControl{nextErr: errors.New("fakeSourceControl: ResolveBranchSHA must never be called for a non-GitHub host")}
	provider := &fakeBuildProvider{nextRef: "should-never-be-used"}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if got := sourceControl.shaCallCount(); got != 0 {
		t.Fatalf("ResolveBranchSHA call count = %d, want 0 (an unsupported host must be rejected BEFORE any resolution call)", got)
	}
	if got := provider.buildCallCount(); got != 0 {
		t.Fatalf("BuildImage call count = %d, want 0", got)
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Status != sqlcgen.ImageBuildStatusFailed {
		t.Errorf("status = %q, want %q", row.Status, sqlcgen.ImageBuildStatusFailed)
	}
	if !row.PermanentlyFailed {
		t.Errorf("permanently_failed = false, want true")
	}
}

// TestRefreshOnce_UnsupportedHost_FailsCleanlyNeverCallsSourceControl is
// TestPumpOnce_RepoBearingRow_UnsupportedHost_FailsCleanlyNeverCallsSourceControl's
// own freshness-pump-side sibling: an already-'ready' row whose repo_urls
// names a non-GitHub host must never reach ResolveBranchSHA during a
// refresh check either -- the OLD image_ref stays servable (§19.2's own
// "never degrades availability"), and the row still advances its own
// ordering key (touchChecked), exactly like every other resolveRepoSHAs
// failure attemptRefresh's own top doc comment already documents.
func TestRefreshOnce_UnsupportedHost_FailsCleanlyNeverCallsSourceControl(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-unsupported-host"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://gitlab.example.com/acme/widgets"},
		map[string]string{"repo1": "sha-old"},
		"narvi/built-image:old-ref")

	rowBefore, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row before: %v", err)
	}

	sourceControl := &fakeSourceControl{nextErr: errors.New("fakeSourceControl: ResolveBranchSHA must never be called for a non-GitHub host")}
	provider := &fakeBuildProvider{nextRef: "should-never-be-used"}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	time.Sleep(5 * time.Millisecond) // ensure a real clock delta from seeding
	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	if got := sourceControl.shaCallCount(); got != 0 {
		t.Fatalf("ResolveBranchSHA call count = %d, want 0 (an unsupported host must be rejected before any resolution call)", got)
	}
	if got := provider.buildCallCount(); got != 0 {
		t.Errorf("BuildImage call count = %d, want 0", got)
	}

	rowAfter, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after: %v", err)
	}
	if rowAfter.Status != sqlcgen.ImageBuildStatusReady {
		t.Errorf("status = %q, want %q (untouched)", rowAfter.Status, sqlcgen.ImageBuildStatusReady)
	}
	if rowAfter.ImageRef == nil || *rowAfter.ImageRef != "narvi/built-image:old-ref" {
		t.Errorf("image_ref = %v, want the OLD ref still intact %q", rowAfter.ImageRef, "narvi/built-image:old-ref")
	}
	if rowAfter.RefreshInProgress {
		t.Error("refresh_in_progress = true, want false (this branch never takes a claim)")
	}
	if !rowAfter.UpdatedAt.Time.After(rowBefore.UpdatedAt.Time) {
		t.Errorf("updated_at did not advance (before=%v after=%v) -- a PERSISTENTLY failing resolution (an unsupported host will never resolve) must still rotate this row to the back of ListReadyImageBuilds' own next window, or it would permanently occupy the front of every tick's batch", rowBefore.UpdatedAt.Time, rowAfter.UpdatedAt.Time)
	}
}

// --- Audit-remediation batch B2: per-branch attemptRefresh coverage ---
//
// The tests below each isolate exactly ONE of attemptRefresh's own
// early-return branches and prove BOTH halves of that function's own two
// invariants (see builder.go's own top doc comment): a branch that never
// took a claim advances the ordering key (touchChecked) and takes no
// claim action; a branch that DID take a claim releases it on every path
// out, including when RecordRefreshSuccess itself fails -- the root
// defect this batch closes (attemptRefresh used to leak the claim on
// exactly that failure, wedging refresh_in_progress=true forever and,
// because that row's own updated_at froze too, recreating the very
// starvation TouchImageBuildChecked was added to close via a different
// door). See builder_whitebox_integration_test.go (package imagebuild) for
// the two remaining branches (the base-only guard, and a genuinely lost
// ClaimForRefresh race) that cannot be reached through RefreshOnce's own
// public entry point at all.

// TestAttemptRefresh_DecodeRepoURLsFailure_TouchesOrderingKeyOnly proves
// the repo_urls-decode-failure branch: seeds a 'ready' row whose repo_urls
// is VALID jsonb (so Postgres itself accepts it) but the WRONG shape for
// map[string]string (a number where a string is expected) -- decodes
// cleanly as jsonb, fails Go's own json.Unmarshal. Believed unreachable in
// practice today (imageresolve.go, the only writer of repo_urls, never
// produces this shape), but attemptRefresh's own invariant 1 must still
// hold if it ever does (future schema drift, manual data repair).
func TestAttemptRefresh_DecodeRepoURLsFailure_TouchesOrderingKeyOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-decode-failure"
	oldRef := "narvi/built-image:decode-failure-old-ref"
	if err := store.UpsertPending(ctx, sqlcgen.UpsertPendingImageBuildParams{
		Fingerprint:    fingerprint,
		Base:           "narvi/base:test",
		RepoUrls:       []byte(`{"repo1": 123}`), // valid jsonb, wrong shape for map[string]string
		RuntimeVersion: "1.0.0-test",
	}); err != nil {
		t.Fatalf("seed pending image_builds row: %v", err)
	}
	if _, err := store.Claim(ctx, fingerprint); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.RecordSuccess(ctx, sqlcgen.RecordImageBuildSuccessParams{
		Fingerprint:   fingerprint,
		ImageRef:      &oldRef,
		BuiltRepoShas: []byte(`{}`),
		BuiltAt:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("record success: %v", err)
	}

	rowBefore, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row before: %v", err)
	}

	// nil sourceControl: if this branch were ever (wrongly) skipped and
	// resolveRepoSHAs reached instead, it would fail loudly there too --
	// but the zero-BuildImage-calls assertion below pins the decode branch
	// specifically, not merely "something failed before BuildImage".
	provider := &fakeBuildProvider{nextRef: "should-never-be-used"}
	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), nil, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	time.Sleep(5 * time.Millisecond) // ensure a real clock delta from seeding
	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	if got := provider.buildCallCount(); got != 0 {
		t.Errorf("BuildImage call count = %d, want 0 (a decode failure must never reach BuildImage)", got)
	}

	rowAfter, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after: %v", err)
	}
	if !rowAfter.UpdatedAt.Time.After(rowBefore.UpdatedAt.Time) {
		t.Errorf("updated_at did not advance (before=%v after=%v) -- the decode-failure branch must call touchChecked", rowBefore.UpdatedAt.Time, rowAfter.UpdatedAt.Time)
	}
	if rowAfter.RefreshInProgress {
		t.Error("refresh_in_progress = true, want false (this branch never takes a claim)")
	}
	if rowAfter.ImageRef == nil || *rowAfter.ImageRef != oldRef {
		t.Errorf("image_ref = %v, want unchanged %q", rowAfter.ImageRef, oldRef)
	}
}

// TestAttemptRefresh_ResolveRepoSHAsError_TouchesOrderingKeyOnly proves the
// resolveRepoSHAs-error branch in isolation (a single row, distinct from
// TestRefreshOnce_StarvationFreedom_GenuinelyStaleRowNotStarvedByStaticFrontCohort's
// own population-level proof of the SAME branch): a repo whose resolution
// PERSISTENTLY fails (a renamed/deleted repo, a token missing org access)
// must still advance this row's own ordering key, taking no claim.
func TestAttemptRefresh_ResolveRepoSHAsError_TouchesOrderingKeyOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-resolve-error"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-old"},
		"narvi/built-image:resolve-error-old-ref")

	rowBefore, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row before: %v", err)
	}

	provider := &fakeBuildProvider{nextRef: "should-never-be-used"}
	sourceControl := &fakeSourceControl{errFor: map[string]error{"repo1": errors.New("fake: repo renamed/deleted, or token lacks org access")}}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	time.Sleep(5 * time.Millisecond)
	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	if got := provider.buildCallCount(); got != 0 {
		t.Errorf("BuildImage call count = %d, want 0 (a resolution error must never reach BuildImage)", got)
	}

	rowAfter, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after: %v", err)
	}
	if !rowAfter.UpdatedAt.Time.After(rowBefore.UpdatedAt.Time) {
		t.Errorf("updated_at did not advance (before=%v after=%v) -- a persistently-failing resolution must still call touchChecked", rowBefore.UpdatedAt.Time, rowAfter.UpdatedAt.Time)
	}
	if rowAfter.RefreshInProgress {
		t.Error("refresh_in_progress = true, want false (this branch never takes a claim)")
	}
}

// TestAttemptRefresh_ClaimForRefreshGenericError_TouchesOrderingKeyOnly
// proves the GENERIC (non-pgx.ErrNoRows) ClaimForRefresh-error branch in
// isolation (audit-remediation batch B2 round 2 -- a prior adversarial
// review found this branch, unlike its RecordRefreshSuccess-generic-error
// sibling, had no dedicated test at all): a row whose ClaimForRefresh call
// fails with a genuine DB error (e.g. a connection reset) -- as opposed to
// the normal, expected "lost the race" pgx.ErrNoRows outcome
// TestAttemptRefresh_ClaimForRefreshLostRace_TouchesOrderingKeyNoRelease
// covers -- must still call touchChecked and advance its own ordering key,
// or it could occupy the front of ListReadyImageBuilds' own queue on every
// tick indefinitely.
//
// Forces a genuine (non-ErrNoRows) Postgres error at EXACTLY the
// ClaimImageBuildForRefresh call site via a test-only trigger that raises
// on the one UPDATE transition that query's own CAS performs
// (refresh_in_progress flipping false -> true) -- unlike
// TestAttemptRefresh_RecordRefreshSuccessGenericError_ReleasesClaim's own
// NUL-byte jsonb-poisoning trick, which cannot reach this query at all
// (ClaimImageBuildForRefresh's own params are a fingerprint and a
// timestamp, no jsonb) -- and deliberately does NOT cancel ctx to force
// the failure, because that would ALSO break this test's own
// touchChecked-advanced assertion (touchChecked runs on the SAME ctx
// attemptRefresh was given, so a canceled ctx would make that call fail
// too, for an unrelated reason, producing a false pass/fail unrelated to
// the branch under test).
func TestAttemptRefresh_ClaimForRefreshGenericError_TouchesOrderingKeyOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-claim-generic-error"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-old"},
		"narvi/built-image:claim-error-old-ref")

	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION test_fail_claim_for_refresh() RETURNS TRIGGER AS $$
		BEGIN
			IF OLD.refresh_in_progress = false AND NEW.refresh_in_progress = true THEN
				RAISE EXCEPTION 'simulated claim-for-refresh failure (test)';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_claim_for_refresh_trigger
		BEFORE UPDATE ON image_builds
		FOR EACH ROW EXECUTE FUNCTION test_fail_claim_for_refresh();
	`); err != nil {
		t.Fatalf("install test trigger: %v", err)
	}
	// DROP both, via t.Cleanup, once this test is done: this trigger fires
	// on EVERY refresh_in_progress false->true UPDATE to image_builds, not
	// just this test's own rows -- against this package's own SHARED
	// integration-test container (sharedpool_integration_test.go), a
	// per-test TRUNCATE resets table DATA but never touches schema objects
	// like a trigger/function, so leaving this installed would silently
	// break every OTHER test's own ClaimForRefresh call for the rest of
	// this binary's life (caught directly here, during this file's own
	// verification, as a cascade of "simulated claim-for-refresh failure"
	// errors in unrelated tests that ran afterward). A fresh per-test
	// container never had this problem, since the whole schema -- not
	// just the data -- was thrown away between tests.
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS test_fail_claim_for_refresh_trigger ON image_builds;
			DROP FUNCTION IF EXISTS test_fail_claim_for_refresh();
		`); err != nil {
			t.Errorf("drop test trigger/function: %v", err)
		}
	})

	rowBefore, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row before: %v", err)
	}

	provider := &fakeBuildProvider{nextRef: "should-never-be-used"}
	sourceControl := &fakeSourceControl{shaFor: map[string]string{"repo1": "sha-new"}} // genuinely stale -- NeedsRefresh must report true, reaching ClaimForRefresh

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	time.Sleep(5 * time.Millisecond) // ensure a real clock delta from seeding
	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v (a per-row failure must never propagate)", err)
	}

	if got := provider.buildCallCount(); got != 0 {
		t.Errorf("BuildImage call count = %d, want 0 (a ClaimForRefresh failure must never reach BuildImage)", got)
	}

	rowAfter, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after: %v", err)
	}
	if !rowAfter.UpdatedAt.Time.After(rowBefore.UpdatedAt.Time) {
		t.Errorf("updated_at did not advance (before=%v after=%v) -- the generic ClaimForRefresh-error branch must call touchChecked, or a row whose claim call persistently errors could occupy the front of ListReadyImageBuilds' own queue indefinitely", rowBefore.UpdatedAt.Time, rowAfter.UpdatedAt.Time)
	}
	if rowAfter.RefreshInProgress {
		t.Error("refresh_in_progress = true, want false (the failed claim attempt must never leave a claim taken)")
	}
}

// TestAttemptRefresh_RecordRefreshSuccessNoOp_ReleasesClaim proves the
// RecordRefreshSuccess pgx.ErrNoRows branch: while a refresh build is
// genuinely in flight (the claim already taken), the row becomes
// no-longer-'ready' through some unrelated path (a should-be-rare, benign
// race per RecordImageRefreshSuccess's own doc comment) -- its own "AND
// status = 'ready'" guard then fails to match. Before audit-remediation
// batch B2, this branch returned without ever releasing the claim,
// wedging refresh_in_progress=true forever; this test proves the claim IS
// now released.
func TestAttemptRefresh_RecordRefreshSuccessNoOp_ReleasesClaim(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-record-success-noop"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-old"},
		"narvi/built-image:noop-old-ref")

	release := make(chan struct{})
	provider := &blockingBuildProvider{nextRef: "narvi/built-image:should-not-persist", release: release}
	sourceControl := &fakeSourceControl{shaFor: map[string]string{"repo1": "sha-new"}}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- builder.RefreshOnce(ctx) }()

	provider.waitUntilEntered(t, 5*time.Second)

	// Simulate the row becoming no-longer-'ready' WHILE the refresh is in
	// flight -- e.g. status flipping away from 'ready' through some other,
	// unrelated path -- so RecordImageRefreshSuccess's own guard fails to
	// match, surfacing pgx.ErrNoRows.
	if _, err := pool.Exec(ctx, `UPDATE image_builds SET status = 'building' WHERE fingerprint = $1`, fingerprint); err != nil {
		t.Fatalf("simulate status race: %v", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.RefreshInProgress {
		t.Error("refresh_in_progress = true after a RecordRefreshSuccess no-op, want false (claim released -- the root defect audit-remediation batch B2 closes)")
	}
	if row.RefreshStartedAt.Valid {
		t.Errorf("refresh_started_at = %v, want NULL (cleared alongside the release)", row.RefreshStartedAt.Time)
	}
	if row.ImageRef == nil || *row.ImageRef != "narvi/built-image:noop-old-ref" {
		t.Errorf("image_ref = %v, want unchanged (the swap never committed)", row.ImageRef)
	}
}

// TestAttemptRefresh_RecordRefreshSuccessGenericError_ReleasesClaim proves
// the RecordRefreshSuccess GENERIC (non-ErrNoRows) error branch -- the
// root defect audit-remediation batch B2 closes (finding: attemptRefresh
// leaks the refresh_in_progress claim when RecordRefreshSuccess fails).
//
// The resolved "current tip" SHA carries an embedded NUL byte -- a
// perfectly valid Go string, and json.Marshal encodes it as the Unicode
// escape sequence for codepoint zero, but Postgres's own jsonb input type
// flatly rejects that escape sequence ("unsupported Unicode escape
// sequence", SQLSTATE 22P05) -- a genuine, non-ErrNoRows database error
// from RecordImageRefreshSuccess's
// own UPDATE that has NOTHING to do with connectivity/context health, so
// (unlike a context-cancellation fault, which would ALSO break the
// subsequent release attempt on the very same ctx) the SAME ctx remains
// perfectly usable for the very next query -- letting this test prove the
// releaseRefreshClaim call this branch now makes actually succeeds, not
// merely that it was attempted.
func TestAttemptRefresh_RecordRefreshSuccessGenericError_ReleasesClaim(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-record-success-generic-error"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-old"},
		"narvi/built-image:generic-error-old-ref")

	provider := &fakeBuildProvider{nextRef: "narvi/built-image:should-not-persist"}
	sourceControl := &fakeSourceControl{shaFor: map[string]string{"repo1": "sha-new\x00-poison"}}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v (a per-row failure must never propagate)", err)
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.RefreshInProgress {
		t.Error("refresh_in_progress = true after a RecordRefreshSuccess error, want false (claim released -- the root defect audit-remediation batch B2 closes: this branch used to return without ever releasing)")
	}
	if row.RefreshStartedAt.Valid {
		t.Errorf("refresh_started_at = %v, want NULL", row.RefreshStartedAt.Time)
	}
	if row.ImageRef == nil || *row.ImageRef != "narvi/built-image:generic-error-old-ref" {
		t.Errorf("image_ref = %v, want unchanged (the swap never committed)", row.ImageRef)
	}
}

// TestRecordRefreshSuccess_FencedAgainstReclaimedClaim_NeverClobbersNewerWrite
// proves audit-remediation batch B2 round 2's own fencing-token fix
// (queries/image_builds.sql's RecordImageRefreshSuccess, now guarded by
// "AND refresh_started_at = @claimed_refresh_started_at" in addition to
// "AND status = 'ready'"): a lease alone (ClaimImageBuildForRefresh's own
// staleness bound) is not sufficient to stop a DELAYED WRITER -- a caller
// whose own RecordRefreshSuccess call outlives that same staleness bound
// (e.g. blocked on this row's own Postgres lock for reasons entirely
// unrelated to the refresh itself: an unrelated long-running transaction,
// a stalled connection, a replica failover) -- from unconditionally
// overwriting whatever a SECOND, concurrent tick has since legitimately
// reclaimed and already written, because "AND status = 'ready'" ALONE
// still matches (status never changes across a reclaim).
//
// Reproduces, at the store level (mirroring TestListReady_
// ExcludesActivelyRefreshingRow_ButIncludesStaleClaim's own "simulate a
// concurrent pod by calling the store directly" technique), EXACTLY the
// interleaving an adversarial review of this batch's own first attempt
// found: "Pod A" takes a claim; that claim goes stale (a delayed write, not
// a crash); "Pod B" reclaims it and completes its own genuinely NEWER
// build; only THEN does Pod A's own originally-blocked write finally land.
func TestRecordRefreshSuccess_FencedAgainstReclaimedClaim_NeverClobbersNewerWrite(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-fencing-clobber"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-original"},
		"narvi/built-image:original-ref")

	// "Pod A" takes the claim first.
	farPastCutoff := pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true}
	podA, err := store.ClaimForRefresh(ctx, fingerprint, farPastCutoff)
	if err != nil {
		t.Fatalf("pod A claim: %v", err)
	}
	podAClaimedAt := podA.RefreshStartedAt

	// Simulate Pod A's own delayed writer: its subsequent RecordRefreshSuccess
	// call is (for this test) about to land long after its own claim should
	// have gone stale -- backdate refresh_started_at directly (there is no
	// in-app way to reach "a write outlives the staleness bound" on purpose)
	// so a later reclaim, below, sees exactly what a real stale claim looks
	// like.
	staleBackdate := time.Now().Add(-1 * time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE image_builds SET refresh_started_at = $2 WHERE fingerprint = $1`, fingerprint, staleBackdate); err != nil {
		t.Fatalf("backdate pod A's claim to simulate staleness: %v", err)
	}

	// "Pod B" reclaims the now-stale lease and completes its OWN successful
	// refresh -- a genuinely NEWER build, using a genuinely newer resolved
	// tip SHA.
	thisTickCutoff := pgtype.Timestamptz{Time: time.Now().Add(-30 * time.Minute), Valid: true}
	podB, err := store.ClaimForRefresh(ctx, fingerprint, thisTickCutoff)
	if err != nil {
		t.Fatalf("pod B reclaim: %v", err)
	}
	newRef := "narvi/built-image:pod-b-fresh-build"
	if _, err := store.RecordRefreshSuccess(ctx, sqlcgen.RecordImageRefreshSuccessParams{
		Fingerprint:             fingerprint,
		ImageRef:                &newRef,
		BuiltRepoShas:           []byte(`{"repo1":"sha-newer"}`),
		BuiltAt:                 pgtype.Timestamptz{Time: time.Now(), Valid: true},
		ClaimedRefreshStartedAt: podB.RefreshStartedAt,
	}); err != nil {
		t.Fatalf("pod B record success: %v", err)
	}

	rowAfterPodB, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row after pod B: %v", err)
	}
	if rowAfterPodB.ImageRef == nil || *rowAfterPodB.ImageRef != newRef {
		t.Fatalf("test setup: pod B's write did not land, image_ref = %v", rowAfterPodB.ImageRef)
	}

	// NOW Pod A's own originally-blocked write finally lands -- using ITS
	// OWN claimed_refresh_started_at, read at ITS OWN claim time, long
	// before Pod B's reclaim. Without the fencing-token fix, this call's
	// only guard was "AND status = 'ready'" -- which still matches (status
	// never changed) -- so it would unconditionally overwrite Pod B's
	// fresher write with Pod A's own stale one.
	staleRef := "narvi/built-image:pod-a-stale-build"
	_, err = store.RecordRefreshSuccess(ctx, sqlcgen.RecordImageRefreshSuccessParams{
		Fingerprint:             fingerprint,
		ImageRef:                &staleRef,
		BuiltRepoShas:           []byte(`{"repo1":"sha-original"}`),
		BuiltAt:                 pgtype.Timestamptz{Time: time.Now(), Valid: true},
		ClaimedRefreshStartedAt: podAClaimedAt,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("pod A's delayed write err = %v, want pgx.ErrNoRows (its claim was superseded by pod B's reclaim -- the fencing token must reject this write)", err)
	}

	rowFinal, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get final row: %v", err)
	}
	if rowFinal.ImageRef == nil || *rowFinal.ImageRef != newRef {
		t.Errorf("image_ref = %v, want pod B's fresh ref %q UNCHANGED by pod A's delayed, superseded write", rowFinal.ImageRef, newRef)
	}
	if rowFinal.RefreshInProgress {
		t.Error("refresh_in_progress = true, want false (pod B's own successful RecordRefreshSuccess already released its own claim)")
	}
}

// TestRecordRefreshFailure_FencedAgainstReclaimedClaim_NeverReleasesLiveClaim
// proves the OTHER half of the SAME fencing-token fix, for the release path
// (RecordImageRefreshFailure, releaseRefreshClaim's own store call) -- the
// adversarial review's own WORSE variant of this finding: "If Pod B's own
// new build were still in flight at that moment instead of already
// complete, Pod A's stray RecordRefreshFailure/Success call would
// additionally release/overwrite Pod B's live, legitimate claim mid-
// refresh". Reproduces exactly that: Pod A's delayed release call (what
// attemptRefresh's own releaseRefreshClaim makes after a BuildImage
// failure, a marshal failure, or a RecordRefreshSuccess failure) lands
// AFTER Pod B has reclaimed the lease and is STILL actively, legitimately
// holding it (Pod B's own build has not yet completed) -- before this
// batch's own fencing-token fix, RecordImageRefreshFailure had NO guard at
// all beyond "fingerprint = $1", so it would have unconditionally released
// Pod B's own live claim out from under it, mid-refresh.
func TestRecordRefreshFailure_FencedAgainstReclaimedClaim_NeverReleasesLiveClaim(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-fencing-release"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-original"},
		"narvi/built-image:original-ref")

	farPastCutoff := pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true}
	podA, err := store.ClaimForRefresh(ctx, fingerprint, farPastCutoff)
	if err != nil {
		t.Fatalf("pod A claim: %v", err)
	}
	podAClaimedAt := podA.RefreshStartedAt

	staleBackdate := time.Now().Add(-1 * time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE image_builds SET refresh_started_at = $2 WHERE fingerprint = $1`, fingerprint, staleBackdate); err != nil {
		t.Fatalf("backdate pod A's claim to simulate staleness: %v", err)
	}

	thisTickCutoff := pgtype.Timestamptz{Time: time.Now().Add(-30 * time.Minute), Valid: true}
	podB, err := store.ClaimForRefresh(ctx, fingerprint, thisTickCutoff)
	if err != nil {
		t.Fatalf("pod B reclaim: %v", err)
	}
	// Pod B's own build is STILL IN FLIGHT here -- no RecordRefreshSuccess/
	// RecordRefreshFailure call has happened for it yet, exactly the
	// "still legitimately holding it" scenario this test proves is safe.

	// Pod A's own delayed release call finally lands, using ITS OWN
	// claimed_refresh_started_at -- read long before Pod B's reclaim.
	_, err = store.RecordRefreshFailure(ctx, fingerprint, podAClaimedAt)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("pod A's delayed release err = %v, want pgx.ErrNoRows (its claim was superseded by pod B's reclaim -- the fencing token must reject this release)", err)
	}

	rowFinal, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get final row: %v", err)
	}
	if !rowFinal.RefreshInProgress {
		t.Error("refresh_in_progress = false, want true -- pod A's delayed, superseded release must NOT release pod B's own still-live, legitimate claim")
	}
	if !rowFinal.RefreshStartedAt.Time.Equal(podB.RefreshStartedAt.Time) {
		t.Errorf("refresh_started_at = %v, want pod B's own claim timestamp %v UNCHANGED", rowFinal.RefreshStartedAt.Time, podB.RefreshStartedAt.Time)
	}
}

// TestListReady_ExcludesActivelyRefreshingRow_ButIncludesStaleClaim proves
// the OTHER half of audit-remediation batch B2's own ListReadyImageBuilds
// fix (finding: the poll query used to omit refresh_in_progress from its
// own WHERE clause entirely, so with more than one control-plane pod, a
// SECOND pod's own tick would re-select a row a FIRST pod was already
// genuinely, actively refreshing -- burning a wasted resolveRepoSHAs/
// GitHub-API round trip only to lose ClaimForRefresh's own CAS): a row
// whose refresh_in_progress claim is FRESH (well within
// ImageRefreshClaimStaleAfter) must be excluded from ListReady entirely,
// while a row whose claim has gone STALE (the crash-recovery case,
// TestRefreshOnce_CrashRecovery_StaleClaimReclaimed's own scenario) must
// still be returned so it can be reclaimed -- proving ListReady's own
// predicate matches ClaimForRefresh's own identical precondition exactly,
// in BOTH directions, directly at the store level (no Builder/provider
// involved).
func TestListReady_ExcludesActivelyRefreshingRow_ButIncludesStaleClaim(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const freshFingerprint = "fp-listready-fresh-claim-excluded"
	const staleFingerprint = "fp-listready-stale-claim-included"
	for _, fp := range []string{freshFingerprint, staleFingerprint} {
		seedReadyImageBuildWithRepos(ctx, t, store, fp,
			map[string]string{"repo1": "https://github.com/acme/repo1"},
			map[string]string{"repo1": "sha-old"},
			"narvi/built-image:"+fp)
	}

	// freshFingerprint: claimed "just now" by a simulated concurrent pod --
	// well within the staleness bound, so it must read as actively,
	// genuinely in flight.
	farPastCutoffForClaiming := pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true}
	if _, err := store.ClaimForRefresh(ctx, freshFingerprint, farPastCutoffForClaiming); err != nil {
		t.Fatalf("claim fresh row: %v", err)
	}

	// staleFingerprint: simulate the aftermath of a crash -- claimed a full
	// hour ago, never released.
	if _, err := pool.Exec(ctx, `UPDATE image_builds SET refresh_in_progress = true, refresh_started_at = $2 WHERE fingerprint = $1`,
		staleFingerprint, time.Now().Add(-1*time.Hour)); err != nil {
		t.Fatalf("simulate wedged claim: %v", err)
	}

	// This tick's own cutoff: 5 minutes -- the fresh claim (seconds old) is
	// well inside it (excluded); the stale claim (1h old) is far outside it
	// (included).
	thisTickCutoff := pgtype.Timestamptz{Time: time.Now().Add(-5 * time.Minute), Valid: true}
	rows, err := store.ListReady(ctx, wantRefreshBatchSize, thisTickCutoff)
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}

	seen := map[string]bool{}
	for _, row := range rows {
		seen[row.Fingerprint] = true
	}
	if seen[freshFingerprint] {
		t.Errorf("ListReady returned %q, want it EXCLUDED -- its refresh_in_progress claim is still fresh (another pod's own active refresh)", freshFingerprint)
	}
	if !seen[staleFingerprint] {
		t.Errorf("ListReady did not return %q, want it INCLUDED -- its refresh_in_progress claim has gone stale and must be reclaimable", staleFingerprint)
	}
}

// TestRefreshOnce_CrashRecovery_StaleClaimReclaimed is the crash-recovery
// test for audit-remediation batch B2's own lease design: simulates the
// AFTERMATH of a crash/SIGTERM/pod-eviction (refresh_in_progress=true,
// refresh_started_at stamped, and nothing ever reached RecordRefreshSuccess/
// RecordRefreshFailure to release it -- exactly the state a real crash
// between ClaimForRefresh and either of those leaves behind, and exactly
// what internal/app/imagebuild/doc.go used to falsely call "self-healing
// by construction") by writing that state directly (there is no in-app way
// to reach it on purpose), then proves ClaimForRefresh's own lease
// genuinely reclaims it: the row IS refreshed, the claim IS released
// afterward, and the stale-claim detection IS logged/counted.
func TestRefreshOnce_CrashRecovery_StaleClaimReclaimed(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-refresh-crash-recovery"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-old"},
		"narvi/built-image:crash-old-ref")

	staleStart := time.Now().Add(-1 * time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE image_builds SET refresh_in_progress = true, refresh_started_at = $2 WHERE fingerprint = $1`, fingerprint, staleStart); err != nil {
		t.Fatalf("simulate wedged claim: %v", err)
	}

	provider := &fakeBuildProvider{nextRef: "narvi/built-image:crash-recovered"}
	sourceControl := &fakeSourceControl{shaFor: map[string]string{"repo1": "sha-new"}}

	timeouts := platform.DefaultTimeouts()
	timeouts.ImageRefreshClaimStaleAfter = 5 * time.Minute // shorter than the simulated 1h-old claim

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, timeouts, sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	before := readRefreshClaimReclaimed(ctx, t, imagebuild.IntegrationOtelReader)

	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	if got := provider.buildCallCount(); got != 1 {
		t.Fatalf("BuildImage call count = %d, want 1 (the stale claim must be reclaimed and refreshed, not left wedged forever)", got)
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.ImageRef == nil || *row.ImageRef != "narvi/built-image:crash-recovered" {
		t.Errorf("image_ref = %v, want the NEW ref -- the wedged claim was never reclaimed", row.ImageRef)
	}
	if row.RefreshInProgress {
		t.Error("refresh_in_progress = true after a successful reclaimed refresh, want false")
	}
	if row.RefreshStartedAt.Valid {
		t.Errorf("refresh_started_at = %v, want NULL", row.RefreshStartedAt.Time)
	}

	if got := readRefreshClaimReclaimed(ctx, t, imagebuild.IntegrationOtelReader) - before; got != 1 {
		t.Errorf("image_refresh_claim_reclaimed delta = %d, want 1 (the stale claim must be logged/counted as detected)", got)
	}
}

// TestBuilderRun_RefreshPumpGoroutineStarts proves Builder.Run actually
// fans out AND RUNS the refresh-pump goroutine (an audit finding: a
// silently-dropped `g.Go(func() error { return b.runRefreshPump(ctx) })`
// in Run would break ZERO other tests in this file, since every one of
// them drives RefreshOnce/PumpOnce directly rather than through Run).
// Only the freshness pump can ever touch a 'ready', repo-bearing row (the
// build pump only ever claims 'pending'/'failed' rows) -- so a real
// BuildImage call against a seeded 'ready' row, observed only through
// Run, is proof positive the refresh-pump goroutine started and ticked.
func TestBuilderRun_RefreshPumpGoroutineStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-run-refresh-pump-starts"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-old"},
		"narvi/built-image:run-smoke-old-ref")

	provider := &fakeBuildProvider{nextRef: "narvi/built-image:run-smoke-refreshed"}
	sourceControl := &fakeSourceControl{shaFor: map[string]string{"repo1": "sha-new"}}

	timeouts := platform.DefaultTimeouts()
	timeouts.ImageBuildPumpInterval = 20 * time.Millisecond
	timeouts.ImageRefreshCheckInterval = 20 * time.Millisecond

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, timeouts, sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- builder.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for provider.buildCallCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := provider.buildCallCount(); got == 0 {
		t.Fatal("BuildImage was never called via Builder.Run -- the refresh pump goroutine did not start/tick")
	}

	cancel()
	if err := <-runErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v, want context.Canceled", err)
	}
}

// --- Build-time dependency cache (§19.1's closing paragraph) ---

// TestPumpOnce_Success_RequestsCacheMountKeyedOnBaseAndRuntimeVersion proves
// attempt (PumpOnce's own per-row body) now populates ImageSpec.CacheMount
// on every real BuildImage call, keyed EXACTLY as ports.CacheMount's own
// doc comment specifies: domain/imagebuild.CacheVolumeKey(row.Base,
// row.RuntimeVersion) -- no rotation epoch anymore (third iteration:
// immutable versioned cache snapshots) -- with Paths carrying
// domain/imagebuild.WellKnownCachePaths() verbatim, MountVersion empty
// (this cache key's first build -- no version has ever been confirmed
// yet), and PublishVersion populated with a freshly minted version.
func TestPumpOnce_Success_RequestsCacheMountKeyedOnBaseAndRuntimeVersion(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)
	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)

	const fingerprint = "fp-cache-mount-claim"
	seedPendingImageBuild(ctx, t, store, fingerprint) // Base="narvi/base:test", RuntimeVersion="1.0.0-test"

	provider := &fakeBuildProvider{nextRef: "narvi/built-image:cache-mount-claim"}

	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), nil, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if got := provider.buildCallCount(); got != 1 {
		t.Fatalf("BuildImage call count = %d, want 1", got)
	}
	mount := provider.buildCalls[0].CacheMount
	if mount == nil {
		t.Fatal("BuildImage called with ImageSpec.CacheMount = nil, want it populated (§19.1's closing paragraph(c))")
	}
	wantKey := domainimagebuild.CacheVolumeKey("narvi/base:test", "1.0.0-test")
	if mount.Key != wantKey {
		t.Errorf("CacheMount.Key = %q, want domain/imagebuild.CacheVolumeKey(base, runtimeVersion) = %q", mount.Key, wantKey)
	}
	if mount.MountVersion != "" {
		t.Errorf("CacheMount.MountVersion = %q, want empty (this cache key's very first cache-requesting build -- no version confirmed yet)", mount.MountVersion)
	}
	if mount.PublishVersion == "" {
		t.Error("CacheMount.PublishVersion is empty, want a freshly minted version")
	}
	wantPaths := domainimagebuild.WellKnownCachePaths()
	if len(mount.Paths) != len(wantPaths) {
		t.Errorf("CacheMount.Paths = %v, want domain/imagebuild.WellKnownCachePaths() verbatim (%v)", mount.Paths, wantPaths)
	}
	for i, p := range wantPaths {
		if i >= len(mount.Paths) || mount.Paths[i] != p {
			t.Errorf("CacheMount.Paths[%d] = %v, want %q", i, mount.Paths, p)
			break
		}
	}
}

// TestPumpOnce_MountedVersionImmuneToConcurrentPublish is a MANDATORY
// regression test (task requirement): "one proving a build mounts a
// pinned version and cannot observe a concurrently-published one."
//
// Build A's own cacheMount resolution happens once, before BuildImage is
// called, and that value travels UNCHANGED all the way onto the wire
// (fakeBuildProvider's own recorded buildCalls) -- even though a SECOND,
// fully independent build (B) publishes a brand-new CONFIRMED version for
// the SAME cache key WHILE A's own BuildImage call is still in flight.
// "In flight" is not simulated loosely here: B's own MintVersion+
// PublishVersion calls run from INSIDE fakeBuildProvider's own
// onBuildImage hook, which fires synchronously between A's request being
// recorded and A's own call returning -- so B's commit to Postgres is
// guaranteed to land strictly BEFORE A's PumpOnce attempt completes, not
// merely "at some point in the test."
//
// This test MUST fail if the pinning mechanism is ever regressed -- e.g.
// if a future change resolved MountVersion lazily/symbolically (say, by
// sending the literal string "latest" and letting the provider resolve it
// at mount time) instead of resolving a concrete version once, before
// BuildImage is ever called: A's own recorded request would then reflect
// B's newer, concurrently published version instead of the one that was
// latest when A's attempt started.
func TestPumpOnce_MountedVersionImmuneToConcurrentPublish(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)
	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)

	const base = "narvi/base:test"
	const runtimeVersion = "1.0.0-test"
	key := domainimagebuild.CacheVolumeKey(base, runtimeVersion)

	// Seed an already-CONFIRMED version for this key, modeling an earlier,
	// unrelated successful build -- this is what build A's own cacheMount
	// resolution must see as MountVersion.
	v1, err := cacheVersionStore.MintVersion(ctx, key)
	if err != nil {
		t.Fatalf("seed: MintVersion: %v", err)
	}
	if err := cacheVersionStore.PublishVersion(ctx, key, v1, "fp-pin-seed"); err != nil {
		t.Fatalf("seed: PublishVersion: %v", err)
	}

	seedPendingImageBuild(ctx, t, store, "fp-pin-a") // Base/RuntimeVersion match the constants above

	var capturedMountVersion, capturedPublishVersion string
	var concurrentPublishRan bool
	var v2 int64
	provider := &fakeBuildProvider{nextRef: "narvi/built-image:pin-a"}
	provider.onBuildImage = func(spec ports.ImageSpec) {
		if spec.CacheMount == nil {
			t.Fatal("BuildImage called with CacheMount = nil, want it populated")
		}
		capturedMountVersion = spec.CacheMount.MountVersion
		capturedPublishVersion = spec.CacheMount.PublishVersion

		// Build B: a SECOND, fully independent successful publish against
		// the SAME key, committed to Postgres WHILE A's own BuildImage
		// call is still executing (this hook runs synchronously inside
		// it, before it returns).
		var mintErr, pubErr error
		v2, mintErr = cacheVersionStore.MintVersion(ctx, key)
		if mintErr != nil {
			t.Fatalf("concurrent publish: MintVersion: %v", mintErr)
		}
		if pubErr = cacheVersionStore.PublishVersion(ctx, key, v2, "fp-pin-concurrent-b"); pubErr != nil {
			t.Fatalf("concurrent publish: PublishVersion: %v", pubErr)
		}
		concurrentPublishRan = true
	}

	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), nil, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	if !concurrentPublishRan {
		t.Fatal("test setup: the concurrent publish inside BuildImage's own hook never ran")
	}

	wantMountVersion := strconv.FormatInt(v1, 10)
	if capturedMountVersion != wantMountVersion {
		t.Errorf("build A's own request carried CacheMount.MountVersion = %q, want %q (the version that was latest BEFORE the concurrent publish) -- a build must mount a PINNED version and must never observe one published after it started", capturedMountVersion, wantMountVersion)
	}
	if capturedPublishVersion == capturedMountVersion {
		t.Fatalf("build A's own CacheMount.PublishVersion (%q) equals its own MountVersion -- publishing must always target a DIFFERENT version than the one a build reads from, or a build could observe its own not-yet-finished write", capturedPublishVersion)
	}
	if capturedPublishVersion == strconv.FormatInt(v2, 10) {
		t.Fatalf("build A's own CacheMount.PublishVersion (%q) collides with build B's concurrently minted version -- two independent publishes against the same key must never be assigned the same version number", capturedPublishVersion)
	}

	// A FRESH resolution (a later build against the SAME key) DOES see the
	// concurrently published version -- proving the mechanism works
	// forward (new builds pick up new versions), not merely that
	// MountVersion never updates at all.
	latest, err := cacheVersionStore.LatestVersion(ctx, key)
	if err != nil {
		t.Fatalf("LatestVersion after concurrent publish: %v", err)
	}
	if latest != v2 {
		t.Errorf("LatestVersion = %d, want %d (build B's own concurrently published version, now the real latest)", latest, v2)
	}
}

// TestPumpOnce_RecordsConfirmedPublishAndPrunesOldVersions exercises
// recordCachePublish (builder.go:306) through the ONLY path any real build
// reaches it: a PumpOnce call whose fakeBuildProvider echoes
// PublishedCacheVersion back on success, exactly as a real adapter's
// success-confirmation contract requires (see fakeBuildProvider's own doc
// comment). Every other test in this file leaves echoPublishedCacheVersion
// at its zero value (false), so recordCachePublish's own early-return guard
// (`publishedCacheVersion == ""`) is the only branch any of them exercise —
// this test is what actually drives PublishVersion/ListVersions/
// DeleteVersions against real Postgres, closing the gap the adversarial
// review of this Step's third iteration found: a safety mechanism that
// reads correctly but nothing ever ran.
func TestPumpOnce_RecordsConfirmedPublishAndPrunesOldVersions(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)
	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)

	const base = "narvi/base:test"
	const runtimeVersion = "1.0.0-test"
	key := domainimagebuild.CacheVolumeKey(base, runtimeVersion)

	// Seed RetainedCacheVersions (5) already-confirmed versions, so this
	// attempt's own confirmed publish is the (RetainedCacheVersions+1)th --
	// exactly the boundary at which recordCachePublish's own prune call
	// must remove exactly one version (the oldest) to stay at the cap.
	var oldestSeeded int64
	for i := 0; i < domainimagebuild.RetainedCacheVersions; i++ {
		v, err := cacheVersionStore.MintVersion(ctx, key)
		if err != nil {
			t.Fatalf("seed: MintVersion: %v", err)
		}
		if err := cacheVersionStore.PublishVersion(ctx, key, v, "fp-seed"); err != nil {
			t.Fatalf("seed: PublishVersion: %v", err)
		}
		if i == 0 {
			oldestSeeded = v
		}
	}

	seedPendingImageBuild(ctx, t, store, "fp-publish-confirm")

	provider := &fakeBuildProvider{nextRef: "narvi/built-image:publish-confirm", echoPublishedCacheVersion: true}
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), nil, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if len(provider.buildCalls) != 1 {
		t.Fatalf("buildCalls = %d, want 1", len(provider.buildCalls))
	}
	cm := provider.buildCalls[0].CacheMount
	if cm == nil {
		t.Fatal("BuildImage called with CacheMount = nil, want it populated")
	}

	versions, err := cacheVersionStore.ListVersions(ctx, key)
	if err != nil {
		t.Fatalf("ListVersions after PumpOnce: %v", err)
	}
	if len(versions) != domainimagebuild.RetainedCacheVersions {
		t.Fatalf("ListVersions after PumpOnce = %d versions, want %d (RetainedCacheVersions) -- the confirmed publish and/or the prune-to-cap step never ran", len(versions), domainimagebuild.RetainedCacheVersions)
	}
	publishedVersion, err := strconv.ParseInt(cm.PublishVersion, 10, 64)
	if err != nil {
		t.Fatalf("parse CacheMount.PublishVersion %q: %v", cm.PublishVersion, err)
	}
	found := false
	for _, v := range versions {
		if v == oldestSeeded {
			t.Fatalf("ListVersions still contains the oldest seeded version %d after publishing a %dth version -- retention pruning never ran", oldestSeeded, domainimagebuild.RetainedCacheVersions+1)
		}
		if v == publishedVersion {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListVersions %v does not contain this attempt's own published version %d -- PublishVersion never committed it", versions, publishedVersion)
	}

	latest, err := cacheVersionStore.LatestVersion(ctx, key)
	if err != nil {
		t.Fatalf("LatestVersion after PumpOnce: %v", err)
	}
	if latest != publishedVersion {
		t.Errorf("LatestVersion = %d, want %d (this attempt's own confirmed publish)", latest, publishedVersion)
	}
}

// TestPumpOnce_CacheMountKey_SharedAcrossDifferentRepoSets_SameBaseAndRuntime
// is the core regression test for §19.1's own "keyed on Base +
// RuntimeVersion ONLY -- never on repo content" requirement, exercised
// through Builder's REAL claim-time build path rather than only at
// domain/imagebuild.CacheVolumeKey's own unit-test level
// (cachemount_test.go): two PENDING rows with genuinely different repo
// sets (different Fingerprints -- see the two distinct fingerprint
// constants and repo maps below) but the identical seeded Base/
// RuntimeVersion (seedPendingImageBuildWithRepos always seeds
// "narvi/base:test"/"1.0.0-test") must resolve to the SAME
// CacheMount.Key on their own independent BuildImage calls -- proving the
// cache volume really is shared across repo sets, not accidentally
// re-partitioned by whatever a caller happens to pass alongside it.
func TestPumpOnce_CacheMountKey_SharedAcrossDifferentRepoSets_SameBaseAndRuntime(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	seedPendingImageBuildWithRepos(ctx, t, store, "fp-cache-shared-a", map[string]string{
		"frontend": "https://github.com/acme/frontend",
	})
	seedPendingImageBuildWithRepos(ctx, t, store, "fp-cache-shared-b", map[string]string{
		"backend": "https://github.com/acme/backend",
		"infra":   "https://github.com/acme/infra",
	})

	provider := &fakeBuildProvider{nextRef: "narvi/built-image:cache-shared"}
	sourceControl := &fakeSourceControl{nextSHA: "sha-any"}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if got := provider.buildCallCount(); got != 2 {
		t.Fatalf("BuildImage call count = %d, want 2 (both pending rows claimed in one batch)", got)
	}

	mountA := provider.buildCalls[0].CacheMount
	mountB := provider.buildCalls[1].CacheMount
	if mountA == nil || mountB == nil {
		t.Fatalf("CacheMount = %v / %v, want both populated", mountA, mountB)
	}
	if mountA.Key != mountB.Key {
		t.Errorf("CacheMount.Key diverged across two Fingerprints that differ ONLY in their repo set (%q vs %q) -- the cache key must never depend on repos", mountA.Key, mountB.Key)
	}
	// Sanity: the two BuildImage calls really did target different repo
	// sets (i.e. this test is actually exercising two different
	// Fingerprints, not accidentally the same row twice).
	if len(provider.buildCalls[0].Repos) == len(provider.buildCalls[1].Repos) {
		t.Fatalf("test setup invalid: both BuildImage calls saw the same repo count (%d) -- want genuinely different repo sets", len(provider.buildCalls[0].Repos))
	}
}

// TestPumpOnce_Success_RecordsBuildDurationAndAttemptTotal_ClaimPhaseSuccess
// proves the build-duration/failure-rate instrumentation §19.9's closing
// paragraph calls for (telemetry.go) actually fires on a real, successful
// claim-time BuildImage call, tagged phase="claim" outcome="success".
func TestPumpOnce_Success_RecordsBuildDurationAndAttemptTotal_ClaimPhaseSuccess(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-telemetry-claim-success"
	seedPendingImageBuild(ctx, t, store, fingerprint)

	provider := &fakeBuildProvider{nextRef: "narvi/built-image:telemetry-claim-success"}
	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), nil, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	beforeAttempts := readImageBuildAttemptTotal(ctx, t, imagebuild.IntegrationOtelReader, "claim", "success")
	beforeDuration := sumImageBuildDurationCount(ctx, t, imagebuild.IntegrationOtelReader, "claim", "success")

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if got := readImageBuildAttemptTotal(ctx, t, imagebuild.IntegrationOtelReader, "claim", "success") - beforeAttempts; got != 1 {
		t.Errorf("image_build_attempt_total{phase=claim,outcome=success} delta = %d, want 1", got)
	}
	if got := sumImageBuildDurationCount(ctx, t, imagebuild.IntegrationOtelReader, "claim", "success") - beforeDuration; got != 1 {
		t.Errorf("image_build_duration_seconds{phase=claim,outcome=success} observation-count delta = %d, want 1", got)
	}
}

// TestPumpOnce_FailedBuild_RecordsBuildDurationAndAttemptTotal_ClaimPhaseFailure
// is the failure-outcome sibling of the success test above -- a failed
// claim-time BuildImage call must still record an observation, tagged
// outcome="failure", proving the counter/histogram genuinely distinguish
// outcomes rather than firing identically regardless.
func TestPumpOnce_FailedBuild_RecordsBuildDurationAndAttemptTotal_ClaimPhaseFailure(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-telemetry-claim-failure"
	seedPendingImageBuild(ctx, t, store, fingerprint)

	provider := &fakeBuildProvider{nextErr: errors.New("simulated BuildImage failure")}
	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), nil, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	beforeAttempts := readImageBuildAttemptTotal(ctx, t, imagebuild.IntegrationOtelReader, "claim", "failure")
	beforeDuration := sumImageBuildDurationCount(ctx, t, imagebuild.IntegrationOtelReader, "claim", "failure")

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if got := readImageBuildAttemptTotal(ctx, t, imagebuild.IntegrationOtelReader, "claim", "failure") - beforeAttempts; got != 1 {
		t.Errorf("image_build_attempt_total{phase=claim,outcome=failure} delta = %d, want 1", got)
	}
	if got := sumImageBuildDurationCount(ctx, t, imagebuild.IntegrationOtelReader, "claim", "failure") - beforeDuration; got != 1 {
		t.Errorf("image_build_duration_seconds{phase=claim,outcome=failure} observation-count delta = %d, want 1", got)
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Status != sqlcgen.ImageBuildStatusFailed {
		t.Errorf("status = %q, want %q (this test's own real point is the metric, but the ordinary failure path must still hold)", row.Status, sqlcgen.ImageBuildStatusFailed)
	}
}

// TestRefreshOnce_Success_RecordsBuildDurationAndAttemptTotal_RefreshPhaseSuccess
// is the "refresh" phase's own sibling of the claim-phase telemetry tests
// above -- a real, successful in-place refresh build must be tagged
// phase="refresh", not "claim", so the two call sites (attempt vs.
// attemptRefresh, builder.go) are genuinely distinguishable in the
// resulting metrics.
func TestRefreshOnce_Success_RecordsBuildDurationAndAttemptTotal_RefreshPhaseSuccess(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-telemetry-refresh-success"
	seedReadyImageBuildWithRepos(ctx, t, store, fingerprint,
		map[string]string{"repo1": "https://github.com/acme/repo1"},
		map[string]string{"repo1": "sha-old"},
		"narvi/built-image:telemetry-refresh-old")

	provider := &fakeBuildProvider{nextRef: "narvi/built-image:telemetry-refresh-new"}
	sourceControl := &fakeSourceControl{shaFor: map[string]string{"repo1": "sha-new"}}

	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), sourceControl, "test-token", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	beforeAttempts := readImageBuildAttemptTotal(ctx, t, imagebuild.IntegrationOtelReader, "refresh", "success")
	beforeDuration := sumImageBuildDurationCount(ctx, t, imagebuild.IntegrationOtelReader, "refresh", "success")

	if err := builder.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	if got := readImageBuildAttemptTotal(ctx, t, imagebuild.IntegrationOtelReader, "refresh", "success") - beforeAttempts; got != 1 {
		t.Errorf("image_build_attempt_total{phase=refresh,outcome=success} delta = %d, want 1", got)
	}
	if got := sumImageBuildDurationCount(ctx, t, imagebuild.IntegrationOtelReader, "refresh", "success") - beforeDuration; got != 1 {
		t.Errorf("image_build_duration_seconds{phase=refresh,outcome=success} observation-count delta = %d, want 1", got)
	}
}

// TestPumpOnce_ProviderIgnoresCacheMount_BuildStillSucceeds is Builder's
// own (narrow) share of the pure-accelerator rule's dedicated test
// coverage the task's own instructions call for: "a corrupted/unavailable/
// declined cache must produce a successful cold build, never a failure."
// fakeBuildProvider (this file) already never inspects ImageSpec.CacheMount
// at all -- it always returns whatever (ref, err) the test configured,
// completely independent of what was requested -- which is EXACTLY a
// provider that has silently declined the cache mount, per ports.
// CacheMount's own doc comment ("a caller must never be able to
// distinguish 'the cache was used' from 'the cache was declined' from
// BuildImage's return value alone"). This test makes that already-true
// property explicit rather than merely incidental: Builder adds no
// cache-conditional branch anywhere on its own success/failure path. The
// DEEPER coverage of this same rule -- an adapter that actively reports
// cache trouble (corrupted/locked/unavailable) still degrading to a
// successful cold build -- lives at the one layer that can meaningfully
// exercise it, internal/adapters/outbound/modal's own
// TestProvider_BuildImage_CacheMountTrouble_FallsBackToColdBuild, since
// that decline-and-retry mechanism is deliberately adapter-internal
// (ports.CacheMount's own "per-provider semantics differ enough that the
// port must not assume one" contract) and Builder itself has no visibility
// into it at all.
func TestPumpOnce_ProviderIgnoresCacheMount_BuildStillSucceeds(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewImageBuildStore(pool)

	const fingerprint = "fp-cache-declined-still-succeeds"
	seedPendingImageBuild(ctx, t, store, fingerprint)

	provider := &fakeBuildProvider{nextRef: "narvi/built-image:declined-cache-cold-build"}
	cacheVersionStore := narvipg.NewImageCacheVersionStore(pool)
	builder, err := imagebuild.NewBuilder(store, pool, provider, platform.DefaultTimeouts(), nil, "", cacheVersionStore)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	// CacheMount WAS requested (Builder always populates it, see
	// TestPumpOnce_Success_RequestsCacheMountKeyedOnBaseAndRuntimeVersion
	// above) -- proving this test genuinely exercises "requested but
	// effectively declined/ignored," not merely "never requested at all."
	if provider.buildCalls[0].CacheMount == nil {
		t.Fatal("test setup invalid: CacheMount was not requested on this BuildImage call")
	}

	row, err := store.Get(ctx, fingerprint)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Status != sqlcgen.ImageBuildStatusReady {
		t.Errorf("status = %q, want %q -- a provider that silently ignores/declines CacheMount must still produce an ordinary successful (cold) build, never a failure", row.Status, sqlcgen.ImageBuildStatusReady)
	}
	if row.ImageRef == nil || *row.ImageRef != "narvi/built-image:declined-cache-cold-build" {
		t.Errorf("image_ref = %v, want %q", row.ImageRef, "narvi/built-image:declined-cache-cold-build")
	}
}
