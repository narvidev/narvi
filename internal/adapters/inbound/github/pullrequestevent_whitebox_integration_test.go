//go:build integration

// White-box (package github, not github_test) integration tests covering
// handlePullRequestClosed's own decision.Allowed == true branch (the
// CherryPickAndMerge call, and the MarkMerged-vs-mergeErrString split) --
// a confirmed re-review finding found this branch completely unexercised
// by any test, in EITHER direction (real or fake): production wiring
// (handler.go) always constructs githubMergeGateDataSource (whose
// CIStatus/MergeableCleanly unconditionally error) and
// notImplementedFixMerger (whose CherryPickAndMerge unconditionally
// errors), so decision.Allowed can never be true through the real HTTP
// handler; and pullrequestevent_integration_test.go's own three tests
// (package github_test, external) can only ever drive the handler through
// that same real wiring, structurally unable to inject a fake
// mergeGateDataSource/fixMerger to reach the true branch at all.
//
// This file calls handlePullRequestClosed DIRECTLY (never through an HTTP
// server) with fake mergeGateDataSource/fixMerger implementations --
// possible only from package github itself, since both types and the
// function itself are unexported -- needing a real Postgres pool only
// because sentinelFixes/repoSettings/auditLog are concrete
// *postgres.Store types, not interfaces (mirroring
// internal/app/imagebuild's own established "duplicate newTestPool for
// this cross-package reason" precedent, builder_whitebox_integration_test.
// go).
//
// Also covers the confirmed "low" finding this same file's own production
// code (pullrequestevent.go) fixes: stackRegistered must be resolved
// FRESH via mergeGateDataSource.StackRegistered, never read off the
// persisted sentinel_fixes.stack_registered column -- proven here by
// seeding the persisted column to one value and configuring the fake
// data source to return the OPPOSITE value, then asserting fixMerger
// received the fake's (fresh) value.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/autonomy"
)

// newWhiteboxAutonomyGate builds the autonomy gate handlePullRequestClosed
// reads the freeze through (technical plan §40.2), on pool.
func newWhiteboxAutonomyGate(t *testing.T, pool *pgxpool.Pool) *autonomy.Gate {
	t.Helper()
	gate, err := autonomy.NewGate(pool)
	if err != nil {
		t.Fatalf("autonomy.NewGate: %v", err)
	}
	return gate
}

// newPullRequestEventWhiteboxTestPool returns this package's own single,
// shared Postgres pool -- started ONCE for the whole test binary by
// TestMain (sharedpool_integration_test.go), not freshly per test/
// container as this function used to do itself. Kept as a thin wrapper
// under its own original name/signature so this file's own call sites
// keep compiling unchanged. See sharedpool_integration_test.go's own top
// doc comment for the full container-reuse story: why this file used to
// duplicate handler_integration_test.go's own newTestPool at all (a
// reverse import package github_test's own unexported helper Go does not
// allow, mirroring internal/app/imagebuild's own newWhiteboxTestPool
// precedent), and why sharing one container across the whole binary is
// safe now.
func newPullRequestEventWhiteboxTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return IntegrationTestPool(t)
}

// fakeMergeGateDataSource is a test-only mergeGateDataSource -- every
// method returns a caller-configured, fixed value, letting these tests
// drive EvaluateMergeGate to Allowed=true deterministically (something
// githubMergeGateDataSource, this package's one real implementation, can
// never do today: CIStatus/MergeableCleanly always error).
type fakeMergeGateDataSource struct {
	changedFiles    []string
	changedFilesErr error
	ciGreen         bool
	ciErr           error
	mergeableClean  bool
	mergeableErr    error
	stackRegistered bool
	stackErr        error
}

func (f *fakeMergeGateDataSource) ChangedFiles(context.Context, string, string, int32) ([]string, error) {
	return f.changedFiles, f.changedFilesErr
}

func (f *fakeMergeGateDataSource) CIStatus(context.Context, string, string, int32) (bool, error) {
	return f.ciGreen, f.ciErr
}

func (f *fakeMergeGateDataSource) MergeableCleanly(context.Context, string, string, int32) (bool, error) {
	return f.mergeableClean, f.mergeableErr
}

func (f *fakeMergeGateDataSource) StackRegistered(context.Context, string, string, int32) (bool, error) {
	return f.stackRegistered, f.stackErr
}

var _ mergeGateDataSource = (*fakeMergeGateDataSource)(nil)

// cherryPickCall records one fakeFixMerger.CherryPickAndMerge invocation.
type cherryPickCall struct {
	owner, repo     string
	fixPRNumber     int
	stackRegistered bool
}

// fakeFixMerger is a test-only fixMerger -- records every call it
// receives and returns a caller-configured error, the polar opposite of
// notImplementedFixMerger (this package's one real, always-denying
// implementation).
type fakeFixMerger struct {
	mu    sync.Mutex
	calls []cherryPickCall
	err   error
}

func (f *fakeFixMerger) CherryPickAndMerge(_ context.Context, owner, repo string, fixPRNumber int, stackRegistered bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cherryPickCall{owner: owner, repo: repo, fixPRNumber: fixPRNumber, stackRegistered: stackRegistered})
	return f.err
}

func (f *fakeFixMerger) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeFixMerger) lastCall() cherryPickCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

var _ fixMerger = (*fakeFixMerger)(nil)

// wholeAllowedFixture seeds a fully-opened sentinel_fixes row (status
// fix_open, a real fix PR number) plus an enabled repo_settings row --
// exactly the precondition handlePullRequestClosed needs to even reach
// its own decision.Allowed branch at all.
func wholeAllowedFixtureForTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, originPRNumber, fixPRNumber int32, persistedStackRegistered bool) sqlcgen.SentinelFix {
	t.Helper()

	sessions := narvipg.NewSessionStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)

	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}
	childSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create child session: %v", err)
	}

	fix, err := sentinelFixes.Claim(ctx, repoFullName, originPRNumber, originSession.ID, "feature-x")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}
	if _, err := sentinelFixes.UpdateChildSession(ctx, fix.ID, childSession.ID); err != nil {
		t.Fatalf("update child session: %v", err)
	}
	if _, err := sentinelFixes.UpdateOpened(ctx, fix.ID, fixPRNumber); err != nil {
		t.Fatalf("update opened: %v", err)
	}
	if _, err := sentinelFixes.UpdateStackRegistered(ctx, fix.ID, persistedStackRegistered); err != nil {
		t.Fatalf("update stack registered: %v", err)
	}
	if _, err := repoSettings.Upsert(ctx, repoFullName, false, true); err != nil {
		t.Fatalf("upsert repo settings (sentinel_autofix_enabled=true): %v", err)
	}

	got, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	return got
}

// pullRequestClosedBodyForTest builds a real-shaped `pull_request` webhook
// payload with action=="closed" and the given merged value -- mirrors
// pullrequestevent_integration_test.go's own identical pullRequestClosedBody
// helper (that one lives in package github_test, not visible here).
func pullRequestClosedBodyForTest(t *testing.T, repoFullName string, prNumber int, merged bool) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"action": "closed",
		"repository": map[string]any{
			"full_name": repoFullName,
		},
		"pull_request": map[string]any{
			"number": prNumber,
			"merged": merged,
		},
	})
	if err != nil {
		t.Fatalf("marshal pull_request closed body: %v", err)
	}
	return body
}

// TestHandlePullRequestClosed_MergeAllowed_Succeeds_MarksMerged proves the
// confirmed re-review finding's first half: given a fake mergeGateDataSource/
// fixMerger that make decision.Allowed reachably true, a successful
// CherryPickAndMerge call actually marks the sentinel_fixes row
// fix_merged and records an audit_log row with merge_attempted=true.
func TestHandlePullRequestClosed_MergeAllowed_Succeeds_MarksMerged(t *testing.T) {
	ctx := context.Background()
	pool := newPullRequestEventWhiteboxTestPool(t)

	repoFullName := "acme/whitebox-merge-succeeds"
	fix := wholeAllowedFixtureForTest(ctx, t, pool, repoFullName, 300, 301, false)

	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	dataSource := &fakeMergeGateDataSource{
		changedFiles:    []string{"foo_test.go"},
		ciGreen:         true,
		mergeableClean:  true,
		stackRegistered: true,
	}
	merger := &fakeFixMerger{}

	w := httptest.NewRecorder()
	handlePullRequestClosed(ctx, w, pullRequestClosedBodyForTest(t, repoFullName, 300, true), sentinelFixes, repoSettings, auditLog, dataSource, merger, newWhiteboxAutonomyGate(t, pool))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	if merger.callCount() != 1 {
		t.Fatalf("CherryPickAndMerge called %d times, want exactly 1 -- this is the confirmed finding's own central gap: this branch was never reachable by any prior test", merger.callCount())
	}
	call := merger.lastCall()
	if call.fixPRNumber != 301 {
		t.Errorf("CherryPickAndMerge fixPRNumber = %d, want 301", call.fixPRNumber)
	}

	got, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if got.Status != "fix_merged" {
		t.Errorf("Status = %q, want %q (a successful CherryPickAndMerge must actually mark the row merged)", got.Status, "fix_merged")
	}

	var mergeAttempted, allowed bool
	if err := pool.QueryRow(ctx,
		`SELECT (detail_json->>'merge_attempted')::boolean, (detail_json->>'allowed')::boolean FROM audit_log WHERE resource_type = 'sentinel_fix' AND resource_id = $1`,
		fix.ID.String(),
	).Scan(&mergeAttempted, &allowed); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if !allowed || !mergeAttempted {
		t.Errorf("audit_log allowed/merge_attempted = %v/%v, want true/true", allowed, mergeAttempted)
	}
}

// TestHandlePullRequestClosed_MergeAllowed_CherryPickFails_RecordsErrorNeverMerges
// proves the confirmed finding's second half: when CherryPickAndMerge
// itself fails, the sentinel_fixes row must NEVER be marked merged, and
// the failure reason must be recorded in the audit_log row's own
// merge_error field.
func TestHandlePullRequestClosed_MergeAllowed_CherryPickFails_RecordsErrorNeverMerges(t *testing.T) {
	ctx := context.Background()
	pool := newPullRequestEventWhiteboxTestPool(t)

	repoFullName := "acme/whitebox-merge-fails"
	fix := wholeAllowedFixtureForTest(ctx, t, pool, repoFullName, 400, 401, false)

	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	dataSource := &fakeMergeGateDataSource{
		changedFiles:   []string{"foo_test.go"},
		ciGreen:        true,
		mergeableClean: true,
	}
	wantErr := errors.New("simulated cherry-pick conflict")
	merger := &fakeFixMerger{err: wantErr}

	w := httptest.NewRecorder()
	handlePullRequestClosed(ctx, w, pullRequestClosedBodyForTest(t, repoFullName, 400, true), sentinelFixes, repoSettings, auditLog, dataSource, merger, newWhiteboxAutonomyGate(t, pool))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if merger.callCount() != 1 {
		t.Fatalf("CherryPickAndMerge called %d times, want exactly 1", merger.callCount())
	}

	got, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if got.Status == "fix_merged" {
		t.Errorf("Status = %q, want it NEVER marked merged when CherryPickAndMerge itself failed", got.Status)
	}

	var mergeError string
	if err := pool.QueryRow(ctx,
		`SELECT detail_json->>'merge_error' FROM audit_log WHERE resource_type = 'sentinel_fix' AND resource_id = $1`,
		fix.ID.String(),
	).Scan(&mergeError); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if mergeError != wantErr.Error() {
		t.Errorf("audit_log merge_error = %q, want %q", mergeError, wantErr.Error())
	}
}

// TestHandlePullRequestClosed_UsesFreshStackRegistered_NeverPersistedColumn
// is the confirmed "low" finding's own regression test: sentinel_fixes.
// stack_registered is seeded to TRUE, but the fake mergeGateDataSource's
// own StackRegistered reports FALSE (simulating "registration silently
// stopped sticking since it was recorded") -- CherryPickAndMerge must
// receive the FRESH (false), never the persisted (true), value.
func TestHandlePullRequestClosed_UsesFreshStackRegistered_NeverPersistedColumn(t *testing.T) {
	ctx := context.Background()
	pool := newPullRequestEventWhiteboxTestPool(t)

	repoFullName := "acme/whitebox-fresh-stack"
	// Persisted column: TRUE.
	wholeAllowedFixtureForTest(ctx, t, pool, repoFullName, 500, 501, true)

	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	// Fake (fresh) data source: FALSE -- the opposite of the persisted
	// column, so this test fails loudly if the caller ever falls back to
	// reading fix.StackRegistered instead of calling this method.
	dataSource := &fakeMergeGateDataSource{
		changedFiles:   []string{"foo_test.go"},
		ciGreen:        true,
		mergeableClean: true,
		// stackRegistered zero-valued: false.
	}
	merger := &fakeFixMerger{}

	w := httptest.NewRecorder()
	handlePullRequestClosed(ctx, w, pullRequestClosedBodyForTest(t, repoFullName, 500, true), sentinelFixes, repoSettings, auditLog, dataSource, merger, newWhiteboxAutonomyGate(t, pool))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if merger.callCount() != 1 {
		t.Fatalf("CherryPickAndMerge called %d times, want exactly 1", merger.callCount())
	}
	if got := merger.lastCall().stackRegistered; got != false {
		t.Errorf("CherryPickAndMerge stackRegistered = %v, want false (the FRESH mergeGateDataSource.StackRegistered value, never the persisted true column)", got)
	}
}

// TestHandlePullRequestClosed_StackRegisteredCheckFails_NeverMerges proves
// a failure to determine the fresh stack-registration state is treated
// exactly like any other CherryPickAndMerge failure: no merge is
// attempted at all, and the reason is recorded.
func TestHandlePullRequestClosed_StackRegisteredCheckFails_NeverMerges(t *testing.T) {
	ctx := context.Background()
	pool := newPullRequestEventWhiteboxTestPool(t)

	repoFullName := "acme/whitebox-stack-check-fails"
	fix := wholeAllowedFixtureForTest(ctx, t, pool, repoFullName, 600, 601, false)

	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	dataSource := &fakeMergeGateDataSource{
		changedFiles:   []string{"foo_test.go"},
		ciGreen:        true,
		mergeableClean: true,
		stackErr:       errors.New("simulated GetPullRequest failure"),
	}
	merger := &fakeFixMerger{}

	w := httptest.NewRecorder()
	handlePullRequestClosed(ctx, w, pullRequestClosedBodyForTest(t, repoFullName, 600, true), sentinelFixes, repoSettings, auditLog, dataSource, merger, newWhiteboxAutonomyGate(t, pool))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if merger.callCount() != 0 {
		t.Errorf("CherryPickAndMerge called %d times, want 0 (a failed fresh stack-registration check must never fall through to a real merge attempt)", merger.callCount())
	}

	got, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if got.Status == "fix_merged" {
		t.Errorf("Status = %q, want it never merged", got.Status)
	}
}

// TestHandlePullRequestClosed_ChangedFilesBlindToRename_DeniesViaOldPath is
// an end-to-end regression test for the confirmed "high" rename-blindness
// finding: ChangedFiles here returns BOTH the rename's old (real,
// non-test) and new (test-looking) path -- exactly what
// parseChangedFilesFromDiff now reports for a real rename diff (see
// pullrequestevent_test.go's own unit tests for that parser in
// isolation) -- and EvaluateMergeGate must deny on the old path, never
// merging.
func TestHandlePullRequestClosed_ChangedFilesBlindToRename_DeniesViaOldPath(t *testing.T) {
	ctx := context.Background()
	pool := newPullRequestEventWhiteboxTestPool(t)

	repoFullName := "acme/whitebox-rename-attack"
	fix := wholeAllowedFixtureForTest(ctx, t, pool, repoFullName, 700, 701, false)

	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	dataSource := &fakeMergeGateDataSource{
		// The exact real-world shape parseChangedFilesFromDiff now returns
		// for `git mv real_impl.go real_impl_test.go` with zero content
		// change: BOTH the old (real) and new (test-looking) path.
		changedFiles:   []string{"internal/foo/real_impl.go", "internal/foo/real_impl_test.go"},
		ciGreen:        true,
		mergeableClean: true,
	}
	merger := &fakeFixMerger{}

	w := httptest.NewRecorder()
	handlePullRequestClosed(ctx, w, pullRequestClosedBodyForTest(t, repoFullName, 700, true), sentinelFixes, repoSettings, auditLog, dataSource, merger, newWhiteboxAutonomyGate(t, pool))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if merger.callCount() != 0 {
		t.Errorf("CherryPickAndMerge called %d times, want 0 -- the renamed-away real production file must deny the merge", merger.callCount())
	}

	got, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if got.Status == "fix_merged" {
		t.Error("Status = fix_merged, want it denied (a renamed-away real production file must never slip through as \"every file is test/doc\")")
	}
}

// TestSentinelFixMergeGate_Frozen_NoMergeAttempted pins technical plan
// §40.2 at the sentinel-fix merge gate: with every check passing, an
// allowed gate is an unattended merge the autonomy freeze holds. The
// merger is never called and no fresh stack read is made, the claim stays
// fix_open, and the gate's audit row records the skip -- allowed false,
// reason "skipped: autonomy is frozen", skipped "frozen" -- distinct from
// a refused or failed merge.
func TestSentinelFixMergeGate_Frozen_NoMergeAttempted(t *testing.T) {
	ctx := context.Background()
	pool := newPullRequestEventWhiteboxTestPool(t)

	repoFullName := "acme/whitebox-merge-frozen"
	fix := wholeAllowedFixtureForTest(ctx, t, pool, repoFullName, 800, 801, false)

	settings := narvipg.NewPlatformSettingsStore(pool)
	if _, err := settings.Freeze(ctx, pgtype.UUID{}, "an incident: hold every automatic action"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}
	t.Cleanup(func() { _, _ = settings.Unfreeze(context.Background()) })

	dataSource := &fakeMergeGateDataSource{
		changedFiles:    []string{"foo_test.go"},
		ciGreen:         true,
		mergeableClean:  true,
		stackErr:        errors.New("the fresh stack read must not be made while frozen"),
		stackRegistered: true,
	}
	merger := &fakeFixMerger{}

	w := httptest.NewRecorder()
	handlePullRequestClosed(ctx, w, pullRequestClosedBodyForTest(t, repoFullName, 800, true), narvipg.NewSentinelFixStore(pool), narvipg.NewRepoSettingsStore(pool), narvipg.NewAuditLogStore(pool), dataSource, merger, newWhiteboxAutonomyGate(t, pool))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := merger.callCount(); got != 0 {
		t.Fatalf("CherryPickAndMerge called %d times while frozen, want 0", got)
	}
	got, err := narvipg.NewSentinelFixStore(pool).GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if got.Status != "fix_open" {
		t.Errorf("Status = %q, want fix_open: a held merge leaves the fix pull request as an ordinary review item", got.Status)
	}

	var allowed, mergeAttempted bool
	var reason, skipped, mergeError string
	if err := pool.QueryRow(ctx,
		`SELECT (detail_json->>'allowed')::boolean, (detail_json->>'merge_attempted')::boolean,
		        detail_json->>'reason', coalesce(detail_json->>'skipped', ''), detail_json->>'merge_error'
		 FROM audit_log WHERE action = 'sentinel_fix.merge_gate_evaluated' AND resource_id = $1`,
		fix.ID.String(),
	).Scan(&allowed, &mergeAttempted, &reason, &skipped, &mergeError); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if allowed || mergeAttempted || reason != "skipped: autonomy is frozen" || skipped != "frozen" || mergeError != "" {
		t.Errorf("audit detail allowed=%v merge_attempted=%v reason=%q skipped=%q merge_error=%q; want false, false, %q, frozen and no error",
			allowed, mergeAttempted, reason, skipped, mergeError, "skipped: autonomy is frozen")
	}
}
