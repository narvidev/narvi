//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/provenance"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves §14.4's ("handoff-readiness sentinel", §14.4) own
// end-to-end wiring: pushpr.go's own createPRBestEffort, extended by
// handoffsentinel.go's runHandoffSentinelBestEffort, against a REAL
// Postgres instance -- mirroring contractdrift_integration_test.go's own
// conventions exactly (createTestEnvironment, createTestSessionWithRepoAndEnvironment,
// fakeSourceControl, sendSandboxEventForTest, waitUntil). This package's
// own contractdrift_integration_test.go already defines TestMain for this
// whole test binary -- nothing here defines a second one.

// GetPullRequestDiff implements PRDiffFetcher for fakeSourceControl (§14.4's
// own extension of this pre-existing fake, pushpr_integration_test.go).
func (f *fakeSourceControl) GetPullRequestDiff(_ context.Context, owner, repo string, number int32, _ string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diffCalls = append(f.diffCalls, fmt.Sprintf("%s/%s#%d", owner, repo, number))
	return f.nextDiff, false, f.nextDiffErr
}

func (f *fakeSourceControl) diffCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.diffCalls)
}

// seedContractDriftSnapshot directly upserts a contract_drift_snapshots
// row -- the "previous" snapshot handoffsentinel.go's own
// checkHandoffContractDrift reads (read-only) and compares the freshly-
// resolved current Snapshot against, via contractdrift.HasDrifted.
func seedContractDriftSnapshot(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoKey, repoSHA, contractsFingerprint string) {
	t.Helper()
	if err := narvipg.NewContractDriftStore(pool).Upsert(ctx, repoKey, repoSHA, contractsFingerprint); err != nil {
		t.Fatalf("seed contract drift snapshot: %v", err)
	}
}

// countOutboxRowsForSessionKind counts outbox rows for sessionID whose
// kind matches kind -- a plain raw-SQL count (mirroring
// contractdrift_integration_test.go's own identical "SELECT count(*)
// FROM ..." precedent), since OutboxStore itself has no list-by-session
// convenience method.
func countOutboxRowsForSessionKind(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, kind string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, kind).Scan(&count); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	return count
}

// getOutboxPayloadForSessionKind fetches the (single) outbox payload for
// sessionID/kind -- callers must have already asserted exactly one row
// exists via countOutboxRowsForSessionKind.
func getOutboxPayloadForSessionKind(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, kind string) []byte {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, kind).Scan(&payload); err != nil {
		t.Fatalf("get outbox payload: %v", err)
	}
	return payload
}

// countHandoffSentinelRuns counts handoff_sentinel_runs rows for
// (repoFullName, prNumber) -- the idempotency claim table's own row.
func countHandoffSentinelRuns(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, prNumber int) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM handoff_sentinel_runs WHERE repo_full_name = $1 AND pr_number = $2`, repoFullName, prNumber).Scan(&count); err != nil {
		t.Fatalf("count handoff_sentinel_runs rows: %v", err)
	}
	return count
}

// getHandoffSentinelRunSummary reads back contract_drift_flagged/
// todo_count (§12.2 item 2's own "handoff-readiness display" gap,
// migrations/000123_handoff_sentinel_runs_summary.up.sql) for one claim
// row -- t.Fatal if the row does not exist (callers already know it
// should, via countHandoffSentinelRuns immediately above).
func getHandoffSentinelRunSummary(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, prNumber int) (contractDriftFlagged bool, todoCount int) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT contract_drift_flagged, todo_count FROM handoff_sentinel_runs WHERE repo_full_name = $1 AND pr_number = $2`,
		repoFullName, prNumber,
	).Scan(&contractDriftFlagged, &todoCount); err != nil {
		t.Fatalf("get handoff_sentinel_runs summary: %v", err)
	}
	return contractDriftFlagged, todoCount
}

func scopedProvenanceTag() *string {
	tag := provenance.ScopedEnvironment
	return &tag
}

// TestHandoffSentinel_ScopedPR_DriftAndTODOs_PostsCommentAndLabel proves
// the full happy path: a scoped, mock-configured session whose repo has
// (a) drifted from its last-known contracts snapshot and (b) a diff
// containing an added TODO marker -- both a "handoff_sentinel" outbox
// row (comment mentioning the repo and the TODO text, label "handoff")
// and a handoff_sentinel_runs claim row are created.
func TestHandoffSentinel_ScopedPR_DriftAndTODOs_PostsCommentAndLabel(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	creator := createTestUserWithGitHubToken(ctx, t, pool, "gh-fake-token-handoff-1")
	env := createTestEnvironment(ctx, t, pool, true, contractsPathPtr("contracts/api"))

	const repoName = "repo-handoff-1"
	const branch = "feature-handoff-1"
	sessionID := createTestSessionWithRepoAndEnvironment(ctx, t, pool, creator, env,
		repoName, "https://github.com/acme/"+repoName+".git", branch)

	// Set this session's own provenance_tag directly (createTestSessionWithRepoAndEnvironment
	// has no parameter for it) -- a plain, direct column update, exactly
	// like this file's own sibling helpers seed columns CreateSessionParams
	// itself does not expose a dedicated helper for.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET provenance_tag = $1 WHERE id = $2`, provenance.ScopedEnvironment, sessionID); err != nil {
		t.Fatalf("set provenance_tag: %v", err)
	}

	repoKey := "acme/" + repoName + "@" + branch
	seedContractDriftSnapshot(ctx, t, pool, repoKey, "old-sha", "fp-unchanged")

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	sourceControl := &fakeSourceControl{
		nextRef:               ports.PRRef{Number: 77, URL: "https://github.com/acme/" + repoName + "/pull/77"},
		defaultBranchName:     "main",
		nextFingerprint:       "fp-unchanged", // SAME as seeded -- repo changed, contract did not: real drift.
		nextFingerprintExists: true,
		nextDiff: "diff --git a/apps/web/src/api.ts b/apps/web/src/api.ts\n" +
			"--- a/apps/web/src/api.ts\n" +
			"+++ b/apps/web/src/api.ts\n" +
			"@@ -1,1 +1,2 @@\n" +
			" line1\n" +
			"+// TODO: wire this up to the real backend once it exists\n",
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", sourceControl, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, sessionID.String(), 1, repoName, branch, "new-sha"),
	})

	waitUntil(t, 5*time.Second, func() bool {
		return countOutboxRowsForSessionKind(ctx, t, pool, sessionID, string(ports.NotificationKindHandoffSentinel)) == 1
	})

	if got := countHandoffSentinelRuns(ctx, t, pool, "acme/"+repoName, 77); got != 1 {
		t.Errorf("handoff_sentinel_runs row count = %d, want 1", got)
	}
	// §12.2 item 2's own "handoff-readiness display" gap: the SAME
	// contractDrifted/todos values that decided this run posted at all
	// (both real here -- seeded drift + the diff's one TODO line above)
	// must land on the claim row itself, not just in the rendered comment
	// body this test already checks below.
	if driftFlagged, todoCount := getHandoffSentinelRunSummary(ctx, t, pool, "acme/"+repoName, 77); !driftFlagged || todoCount != 1 {
		t.Errorf("handoff_sentinel_runs summary = (contractDriftFlagged=%v, todoCount=%d), want (true, 1)", driftFlagged, todoCount)
	}

	payload := getOutboxPayloadForSessionKind(ctx, t, pool, sessionID, string(ports.NotificationKindHandoffSentinel))
	var decoded struct {
		Owner    string `json:"owner"`
		Repo     string `json:"repo"`
		PRNumber int    `json:"pr_number"`
		Body     string `json:"body"`
		Label    string `json:"label"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode outbox payload: %v", err)
	}
	if decoded.Owner != "acme" || decoded.Repo != repoName || decoded.PRNumber != 77 {
		t.Errorf("payload owner/repo/prNumber = %q/%q/%d, want acme/%s/77", decoded.Owner, decoded.Repo, decoded.PRNumber, repoName)
	}
	if decoded.Label != "handoff" {
		t.Errorf("payload label = %q, want %q", decoded.Label, "handoff")
	}
	if want := "acme/" + repoName; !strings.Contains(decoded.Body, want) {
		t.Errorf("payload body = %q, want it to mention %q (contract-drift finding)", decoded.Body, want)
	}
	if want := "wire this up to the real backend"; !strings.Contains(decoded.Body, want) {
		t.Errorf("payload body = %q, want it to mention %q (TODO finding)", decoded.Body, want)
	}
}

// TestHandoffSentinel_ScopedPR_Clean_PostsNothing proves silence is
// correct: a scoped, mock-configured session with NO prior contract-drift
// snapshot (first sighting -- no drift) and an empty diff (no TODOs)
// posts NOTHING at all -- no outbox row, no label, no
// handoff_sentinel_runs claim -- even though this IS a scoped session
// (the ordinary "post nothing when there is nothing to say" case, never
// confused with "not a scoped session at all", the next test).
func TestHandoffSentinel_ScopedPR_Clean_PostsNothing(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	creator := createTestUserWithGitHubToken(ctx, t, pool, "gh-fake-token-handoff-2")
	env := createTestEnvironment(ctx, t, pool, true, contractsPathPtr("contracts/api"))

	const repoName = "repo-handoff-2"
	const branch = "feature-handoff-2"
	sessionID := createTestSessionWithRepoAndEnvironment(ctx, t, pool, creator, env,
		repoName, "https://github.com/acme/"+repoName+".git", branch)
	if _, err := pool.Exec(ctx, `UPDATE sessions SET provenance_tag = $1 WHERE id = $2`, provenance.ScopedEnvironment, sessionID); err != nil {
		t.Fatalf("set provenance_tag: %v", err)
	}

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	// No seeded snapshot at all -- "first sighting", HasDrifted's own
	// documented false result. nextDiff left empty -- no TODOs.
	sourceControl := &fakeSourceControl{
		nextRef:               ports.PRRef{Number: 88, URL: "https://github.com/acme/" + repoName + "/pull/88"},
		defaultBranchName:     "main",
		nextFingerprint:       "fp-whatever",
		nextFingerprintExists: true,
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", sourceControl, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, sessionID.String(), 1, repoName, branch, "new-sha"),
	})

	// Wait for the ordinary PR artifact instead (proving createPRBestEffort
	// itself ran to completion) before asserting the ABSENCE of a handoff
	// outbox row -- otherwise a slow-but-eventually-posting bug could read
	// as a false pass.
	artifactStore := narvipg.NewArtifactStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		rows, err := artifactStore.ListForSession(ctx, sessionID)
		return err == nil && len(rows) == 1
	})

	if got := countOutboxRowsForSessionKind(ctx, t, pool, sessionID, string(ports.NotificationKindHandoffSentinel)); got != 0 {
		t.Errorf("handoff_sentinel outbox row count = %d, want 0 (nothing to report)", got)
	}
	if got := countHandoffSentinelRuns(ctx, t, pool, "acme/"+repoName, 88); got != 0 {
		t.Errorf("handoff_sentinel_runs row count = %d, want 0 (nothing was ever claimed)", got)
	}
}

// TestHandoffSentinel_OrdinaryPR_CompletelyUntouched proves an ORDINARY
// (non-scoped) session's PR is completely unaffected: zero extra
// ResolveContractsFingerprint/GetPullRequestDiff calls, no outbox row, no
// handoff_sentinel_runs claim -- even though this session's repo/diff
// would otherwise have plenty to report (a drifted fingerprint AND a
// TODO), proving the provenance_tag gate is checked FIRST, before any
// other work.
func TestHandoffSentinel_OrdinaryPR_CompletelyUntouched(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	creator := createTestUserWithGitHubToken(ctx, t, pool, "gh-fake-token-handoff-3")

	const repoName = "repo-handoff-3"
	const branch = "feature-handoff-3"
	// createTestSessionWithRepos -- NO environment, NO provenance_tag: the
	// ordinary, overwhelmingly common case.
	sessionID := createTestSessionWithRepos(ctx, t, pool, creator, repoName, "https://github.com/acme/"+repoName+".git", branch)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	sourceControl := &fakeSourceControl{
		nextRef:           ports.PRRef{Number: 99, URL: "https://github.com/acme/" + repoName + "/pull/99"},
		defaultBranchName: "main",
		// Configured so that IF the handoff sentinel ran despite the
		// provenance-tag gate, it would find plenty to report -- proving
		// the zero-calls assertion below is a real gate, not a fluke of
		// this fake returning nothing anyway.
		nextFingerprint:       "fp-would-signal-drift",
		nextFingerprintExists: true,
		nextDiff:              "diff --git a/x.ts b/x.ts\n--- a/x.ts\n+++ b/x.ts\n@@ -1,1 +1,2 @@\n line1\n+// TODO: should never be scanned\n",
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", sourceControl, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, sessionID.String(), 1, repoName, branch, "new-sha"),
	})

	artifactStore := narvipg.NewArtifactStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		rows, err := artifactStore.ListForSession(ctx, sessionID)
		return err == nil && len(rows) == 1
	})

	if got := sourceControl.fingerprintCallCount(); got != 0 {
		t.Errorf("ResolveContractsFingerprint call count = %d, want 0 (an ordinary PR must never even check for drift)", got)
	}
	if got := sourceControl.diffCallCount(); got != 0 {
		t.Errorf("GetPullRequestDiff call count = %d, want 0 (an ordinary PR must never even fetch a diff to scan)", got)
	}
	if got := countOutboxRowsForSessionKind(ctx, t, pool, sessionID, string(ports.NotificationKindHandoffSentinel)); got != 0 {
		t.Errorf("handoff_sentinel outbox row count = %d, want 0", got)
	}
	if got := countHandoffSentinelRuns(ctx, t, pool, "acme/"+repoName, 99); got != 0 {
		t.Errorf("handoff_sentinel_runs row count = %d, want 0", got)
	}
}

// TestHandoffSentinel_Idempotent_RunningTwiceDoesNotDuplicate proves
// running the sentinel twice for the SAME PR (simulated here as a
// redelivered/duplicate push_complete event for the same repo/branch/sha,
// which fakeSourceControl.CreatePR answers with the SAME PR number both
// times) never duplicates the outbox row (and therefore never duplicates
// the label/comment GitHub-side) or the handoff_sentinel_runs claim.
func TestHandoffSentinel_Idempotent_RunningTwiceDoesNotDuplicate(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	creator := createTestUserWithGitHubToken(ctx, t, pool, "gh-fake-token-handoff-4")
	env := createTestEnvironment(ctx, t, pool, true, contractsPathPtr("contracts/api"))

	const repoName = "repo-handoff-4"
	const branch = "feature-handoff-4"
	sessionID := createTestSessionWithRepoAndEnvironment(ctx, t, pool, creator, env,
		repoName, "https://github.com/acme/"+repoName+".git", branch)
	if _, err := pool.Exec(ctx, `UPDATE sessions SET provenance_tag = $1 WHERE id = $2`, provenance.ScopedEnvironment, sessionID); err != nil {
		t.Fatalf("set provenance_tag: %v", err)
	}

	repoKey := "acme/" + repoName + "@" + branch
	seedContractDriftSnapshot(ctx, t, pool, repoKey, "old-sha", "fp-unchanged")

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	sourceControl := &fakeSourceControl{
		nextRef:               ports.PRRef{Number: 55, URL: "https://github.com/acme/" + repoName + "/pull/55"},
		defaultBranchName:     "main",
		nextFingerprint:       "fp-unchanged",
		nextFingerprintExists: true,
		nextDiff:              "diff --git a/x.ts b/x.ts\n--- a/x.ts\n+++ b/x.ts\n@@ -1,1 +1,2 @@\n line1\n+// TODO: duplicate-safe\n",
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", sourceControl, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	// First delivery.
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, sessionID.String(), 1, repoName, branch, "new-sha"),
	})
	waitUntil(t, 5*time.Second, func() bool {
		return countOutboxRowsForSessionKind(ctx, t, pool, sessionID, string(ports.NotificationKindHandoffSentinel)) == 1
	})

	// Second, duplicate delivery -- same repo/branch/sha, same fake
	// (same nextRef.Number). CreatePR's own idempotency (githubapi/
	// adapter.go, a confirmed-finding fix) means this "succeeds" by
	// recovering the SAME PR rather than erroring, and recordPRArtifact's
	// own companion dedup guard means this does NOT create a second "pr"
	// artifact row for it -- both fixed together, not a pre-existing
	// accepted limitation anymore.
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, sessionID.String(), 1, repoName, branch, "new-sha"),
	})

	artifactStore := narvipg.NewArtifactStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		rows, err := artifactStore.ListForSession(ctx, sessionID)
		return err == nil && len(rows) == 1
	})

	if got := countOutboxRowsForSessionKind(ctx, t, pool, sessionID, string(ports.NotificationKindHandoffSentinel)); got != 1 {
		t.Errorf("handoff_sentinel outbox row count = %d, want exactly 1 (never duplicated by a second run)", got)
	}
	if got := countHandoffSentinelRuns(ctx, t, pool, "acme/"+repoName, 55); got != 1 {
		t.Errorf("handoff_sentinel_runs row count = %d, want exactly 1", got)
	}
}

// TestHandoffSentinel_ScopedPR_OwnCommitsAlone_NeverFalselyReportsDrift is
// the confirmed-finding regression test for checkHandoffContractDrift's own
// fix (handoffsentinel.go's doc comment on that function has the full
// bug writeup): the pre-fix code reused THIS session's own just-pushed
// branch/sha directly as the drift-check's "current" state, instead of the
// repo's real configured/default branch -- so the session's own commits,
// alone (on a branch/sha completely unrelated to the repo's tracked
// branch), must never be what drives the comparison.
//
// This session's repo config leaves branch nil (the common case --
// createTestSessionWithRepoAndEnvironmentNilBranch), which
// checkContractDriftForRepo resolves to the repo's real default branch,
// "main" (fakeSourceControl.defaultBranchName below) -- the snapshot is
// seeded under "acme/<repo>@main", genuinely drifted (a different SHA,
// same contracts fingerprint) from what ResolveBranchSHA/
// ResolveContractsFingerprint report as "main"'s current state. The
// push_complete event names a THIRD, unrelated branch/sha (this session's
// own generated branch and its own commit) that never appears anywhere in
// the seeded snapshot or the fake's configured current state.
//
// Pre-fix, this would have read repoKey "acme/<repo>@narvi/session-own-
// branch" (ErrNoRows -- never seeded) and returned false, missing real
// drift on "main" entirely. Post-fix, checkHandoffContractDrift re-resolves
// "main" itself (ignoring the session's own branch/sha) and correctly
// finds it.
func TestHandoffSentinel_ScopedPR_OwnCommitsAlone_NeverFalselyReportsDrift(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	creator := createTestUserWithGitHubToken(ctx, t, pool, "gh-fake-token-handoff-5")
	env := createTestEnvironment(ctx, t, pool, true, contractsPathPtr("contracts/api"))

	const repoName = "repo-handoff-5"
	sessionID := createTestSessionWithRepoAndEnvironmentNilBranch(ctx, t, pool, creator, env,
		repoName, "https://github.com/acme/"+repoName+".git")
	if _, err := pool.Exec(ctx, `UPDATE sessions SET provenance_tag = $1 WHERE id = $2`, provenance.ScopedEnvironment, sessionID); err != nil {
		t.Fatalf("set provenance_tag: %v", err)
	}

	repoKey := "acme/" + repoName + "@main"
	seedContractDriftSnapshot(ctx, t, pool, repoKey, "main-sha-old", "fp-stable")

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	sourceControl := &fakeSourceControl{
		nextRef:               ports.PRRef{Number: 66, URL: "https://github.com/acme/" + repoName + "/pull/66"},
		defaultBranchName:     "main",
		nextSHA:               "main-sha-new", // main moved -- genuine drift, contract fingerprint unchanged.
		nextFingerprint:       "fp-stable",
		nextFingerprintExists: true,
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", sourceControl, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	// This session's OWN push: its own generated branch and its own
	// commit sha, both deliberately absent from the seeded snapshot and
	// the fake's configured "main" state above.
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, sessionID.String(), 1, repoName, "narvi/session-own-branch", "session-own-commit-sha"),
	})

	waitUntil(t, 5*time.Second, func() bool {
		return countOutboxRowsForSessionKind(ctx, t, pool, sessionID, string(ports.NotificationKindHandoffSentinel)) == 1
	})

	payload := getOutboxPayloadForSessionKind(ctx, t, pool, sessionID, string(ports.NotificationKindHandoffSentinel))
	var decoded struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode outbox payload: %v", err)
	}
	if want := "acme/" + repoName; !strings.Contains(decoded.Body, want) {
		t.Errorf("payload body = %q, want it to mention %q (contract-drift finding, resolved against the repo's real default branch)", decoded.Body, want)
	}

	if got := sourceControl.lastSHASpec(); got.Branch != "" {
		t.Errorf("ResolveBranchSHA's last spec.Branch = %q, want \"\" (the repo's own nil-configured branch, never this session's own push branch %q)",
			got.Branch, "narvi/session-own-branch")
	}
}
