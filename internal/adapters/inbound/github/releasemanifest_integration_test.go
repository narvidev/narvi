//go:build integration

// Integration coverage for §15's own ("release PR review", §15)
// end-to-end wiring -- updated for blocking-finding fix #1's own
// two-phase split: a real webhook POST, through the real
// coalescer.CreateOrJoin (a real Postgres session-creation transaction),
// through triggerReleaseManifestCheckBestEffort, into a REAL
// *postgres.ReleaseManifestPendingStore -- proving the whole ENQUEUE
// chain actually inserts a release_manifest_pending row FAST, without
// ever calling ListMergedBetween on this request path at all. A separate
// test then drives internal/app/releasereview.Worker.PumpOnce
// deterministically (mirroring outboxworker.Builder.PumpOnce/imagebuild.
// Builder.PumpOnce's own established "exported for exactly-one-tick test
// determinism" precedent) to prove the SECOND phase -- claiming that row
// and actually running the check -- still ends with a real
// ports.NotificationKindReleaseManifest row in the outbox table, exactly
// as before this fix. GitHub itself is still faked (SourceControl/
// PullRequests) -- no real outbound GitHub call is possible in this test
// environment -- mirrors this package's own established
// fakePullRequestResolver precedent (handler_integration_test.go) for the
// identical reason.
package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/releasereview"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeReleaseManifestSourceControl is a test-only releasereview.
// MergedPRLister -- no real HTTP round trip, mirroring this package's own
// fakePullRequestResolver/fakeCommentPoster precedent exactly. Consumed
// by Worker (the SECOND phase), never by the webhook handler itself any
// more (blocking-finding fix #1).
type fakeReleaseManifestSourceControl struct {
	merged []ports.MergedPR
}

func (f *fakeReleaseManifestSourceControl) ListMergedBetween(context.Context, ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return f.merged, false, nil
}

// TestGitHubIntegration_ReleasePRDetected_EnqueuesPendingCheckFast proves
// blocking-finding fix #1's own FIRST phase: a brand-new review session
// created on a PR whose head branch matches the configured release
// pattern ends -- immediately, on THIS webhook request, before any
// GitHub-API-heavy work ever runs -- with a REAL row in Postgres's own
// release_manifest_pending table, carrying the correctly-identified
// owner/repo/PR number/base/head. No outbox row exists yet at this
// point: the actual check has not run.
func TestGitHubIntegration_ReleasePRDetected_EnqueuesPendingCheckFast(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	pendingStore := narvipg.NewReleaseManifestPendingStore(pool)

	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.PullRequests = &fakePullRequestResolver{pr: githubapi.PullRequest{HeadRef: "release/9.9", BaseRef: "main"}}
		cfg.PendingChecks = pendingStore
		cfg.ReleaseLabel = "release"
		cfg.ReleaseBranchPattern = "release/*"
		cfg.Timeouts = platform.DefaultTimeouts()
	})

	const commenterID = 80000301
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	body := issueCommentBodyWithCommenter("acme/release-repo", "release-repo", "https://github.com/acme/release-repo.git", 301, "release-check", commenterID, "release-check-user")

	status := postWebhook(t, rig, body, "delivery-release-manifest-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	var sessionID string
	if err := rig.pool.QueryRow(ctx,
		`SELECT id FROM sessions WHERE spawn_source = 'github' ORDER BY created_at DESC LIMIT 1`,
	).Scan(&sessionID); err != nil {
		t.Fatalf("query session: %v", err)
	}

	var pendingSessionID, owner, repo string
	var prNumber int32
	if err := rig.pool.QueryRow(ctx,
		`SELECT session_id::text, owner, repo, pr_number FROM release_manifest_pending ORDER BY created_at DESC LIMIT 1`,
	).Scan(&pendingSessionID, &owner, &repo, &prNumber); err != nil {
		t.Fatalf("query release_manifest_pending: %v (no pending row was ever enqueued)", err)
	}

	if pendingSessionID != sessionID {
		t.Errorf("pending row session_id = %q, want %q (the just-created release-review session)", pendingSessionID, sessionID)
	}
	if owner != "acme" || repo != "release-repo" || prNumber != 301 {
		t.Errorf("pending row identity = owner=%q repo=%q pr_number=%d, want owner=acme repo=release-repo pr_number=301", owner, repo, prNumber)
	}

	var outboxCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE kind = 'release_manifest'`).Scan(&outboxCount); err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if outboxCount != 0 {
		t.Errorf("outbox release_manifest row count = %d, want 0 (the actual check has not run yet -- only the pending row was enqueued)", outboxCount)
	}
}

// TestGitHubIntegration_OrdinaryPR_NeverEnqueuesPendingCheck proves an
// ordinary (non-release) PR's own brand-new session never enqueues a
// release_manifest_pending row at all -- the negative-case sibling to the
// test above, over the SAME real Postgres wiring.
func TestGitHubIntegration_OrdinaryPR_NeverEnqueuesPendingCheck(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	pendingStore := narvipg.NewReleaseManifestPendingStore(pool)

	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.PullRequests = &fakePullRequestResolver{pr: githubapi.PullRequest{HeadRef: "feature/ordinary-thing", BaseRef: "main"}}
		cfg.PendingChecks = pendingStore
		cfg.ReleaseLabel = "release"
		cfg.ReleaseBranchPattern = "release/*"
		cfg.Timeouts = platform.DefaultTimeouts()
	})

	const commenterID = 80000302
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	body := issueCommentBodyWithCommenter("acme/ordinary-repo", "ordinary-repo", "https://github.com/acme/ordinary-repo.git", 302, "ordinary-check", commenterID, "ordinary-check-user")

	status := postWebhook(t, rig, body, "delivery-release-manifest-2")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	var count int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM release_manifest_pending`).Scan(&count); err != nil {
		t.Fatalf("query release_manifest_pending: %v", err)
	}
	if count != 0 {
		t.Errorf("release_manifest_pending row count = %d, want 0 (not a release PR)", count)
	}
}

// TestGitHubIntegration_WorkerPumpOnce_ClaimsPendingCheckAndEnqueuesOutboxRow
// proves blocking-finding fix #1's own SECOND phase end to end: given a
// real release_manifest_pending row (written exactly as the webhook
// handler above would), internal/app/releasereview.Worker.PumpOnce claims
// it, runs the actual check against a faked SourceControl, and ends with
// a REAL ports.NotificationKindReleaseManifest row in Postgres's own
// outbox table -- proving the async hand-off this fix introduces still
// delivers the SAME end result the pre-fix synchronous call used to,
// just decoupled from any webhook request's own context/lifetime. This
// is exactly the "async manifest-check mechanism actually completing
// after the handler returns" scenario the fix's own verification plan
// calls for.
func TestGitHubIntegration_WorkerPumpOnce_ClaimsPendingCheckAndEnqueuesOutboxRow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	pendingStore := narvipg.NewReleaseManifestPendingStore(pool)
	outboxStore := narvipg.NewOutboxStore(pool, false)

	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.PullRequests = &fakePullRequestResolver{pr: githubapi.PullRequest{HeadRef: "release/8.8", BaseRef: "main"}}
		cfg.PendingChecks = pendingStore
		cfg.ReleaseLabel = "release"
		cfg.ReleaseBranchPattern = "release/*"
		cfg.Timeouts = platform.DefaultTimeouts()
	})

	const commenterID = 80000303
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	body := issueCommentBodyWithCommenter("acme/worker-repo", "worker-repo", "https://github.com/acme/worker-repo.git", 303, "worker-check", commenterID, "worker-check-user")

	status := postWebhook(t, rig, body, "delivery-release-manifest-3")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	// The webhook request has already returned by this point -- there is
	// no request context left alive at all. Worker.PumpOnce below runs
	// against context.Background(), exactly like Worker.Run would against
	// cmd/control-plane/main.go's own process-lifetime groupCtx, never
	// against the (long since torn down) request context.
	sourceControl := &fakeReleaseManifestSourceControl{merged: []ports.MergedPR{
		{Number: 201, Title: "fix: something", HasApprovingReview: true, CIConclusionAtMergeSHA: ports.CIConclusionSuccess},
		{Number: 202, Title: "feat: another thing", HasApprovingReview: false},
	}}
	worker := releasereview.NewWorker(pendingStore, releasereview.Deps{
		SourceControl: sourceControl,
		Outbox:        outboxStore,
		Timeouts:      platform.DefaultTimeouts(),
	}, "gho_bottoken", platform.DefaultTimeouts())

	if err := worker.PumpOnce(ctx); err != nil {
		t.Fatalf("Worker.PumpOnce() error = %v", err)
	}

	var remainingPending int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM release_manifest_pending`).Scan(&remainingPending); err != nil {
		t.Fatalf("query release_manifest_pending: %v", err)
	}
	if remainingPending != 0 {
		t.Errorf("release_manifest_pending row count after PumpOnce = %d, want 0 (claimed rows are deleted)", remainingPending)
	}

	var outboxSessionID, kind, payloadText string
	if err := rig.pool.QueryRow(ctx,
		`SELECT session_id::text, kind, payload::text FROM outbox WHERE kind = 'release_manifest' ORDER BY created_at DESC LIMIT 1`,
	).Scan(&outboxSessionID, &kind, &payloadText); err != nil {
		t.Fatalf("query outbox: %v (no release_manifest row was ever enqueued by Worker.PumpOnce)", err)
	}

	if kind != string(ports.NotificationKindReleaseManifest) {
		t.Errorf("outbox row kind = %q, want %q", kind, ports.NotificationKindReleaseManifest)
	}

	var payload githubapi.ReleaseManifestPayload
	if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
		t.Fatalf("decode outbox payload: %v", err)
	}
	if payload.Owner != "acme" || payload.Repo != "worker-repo" || payload.PRNumber != 303 {
		t.Errorf("payload identity = %+v, want owner=acme repo=worker-repo pr_number=303", payload)
	}
	if !strings.Contains(payload.Body, "Release manifest check") {
		t.Errorf("payload.Body missing the expected rendered heading:\n%s", payload.Body)
	}
	if !strings.Contains(payload.Body, "PR #202") {
		t.Errorf("payload.Body missing the expected unreviewed-merge finding for PR #202:\n%s", payload.Body)
	}
}

// TestGitHubIntegration_WorkerPumpOnce_ConcurrentPodsNeverDoubleProcess
// proves the concurrency guarantee blocking-finding fix #1's own claim
// query (ClaimDueReleaseManifestPending, DELETE ... WHERE id IN (SELECT
// ... FOR UPDATE SKIP LOCKED), each claimed check recorded as running in
// the same transaction) rests on: several "pods" (goroutines here)
// calling Worker.PumpOnce CONCURRENTLY against the SAME real Postgres
// release_manifest_pending table must each claim DISJOINT rows --
// every seeded row gets processed EXACTLY once (never zero, never twice)
// -- run under -race so a genuine data race in this path would also be
// caught, not just a logical double-claim.
//
// Seeds every release_manifest_pending row DIRECTLY via pendingStore.
// Create, all against ONE real session (created via a single webhook
// POST, purely to get a real, FK-satisfying session_id) -- deliberately
// NOT one webhook POST per seeded row: this test's own subject is the
// CLAIM mechanism's concurrency safety, nothing about
// coalesce.CreateOrJoin's own session/actor-spawn machinery, and
// spawning a real sessionactor.Actor (its own errgroup-managed goroutine)
// per row would burn one pgxpool connection's worth of headroom per row
// for no reason this test cares about -- this package's own shared test
// pool sizes MaxConns off runtime.NumCPU() (postgres.NewPool's own doc
// comment), so a webhook-per-row loop here could exhaust it purely as an
// artifact of this test's own setup, unrelated to anything this test
// actually verifies.
func TestGitHubIntegration_WorkerPumpOnce_ConcurrentPodsNeverDoubleProcess(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	pendingStore := narvipg.NewReleaseManifestPendingStore(pool)
	outboxStore := narvipg.NewOutboxStore(pool, false)

	const numPRs = 12
	const commenterID = 80000304

	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.PullRequests = &fakePullRequestResolver{pr: githubapi.PullRequest{HeadRef: "release/concurrent", BaseRef: "main"}}
		cfg.PendingChecks = pendingStore
		cfg.ReleaseLabel = "release"
		cfg.ReleaseBranchPattern = "release/*"
		cfg.Timeouts = platform.DefaultTimeouts()
	})

	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	// ONE real webhook POST, purely to obtain a real session_id
	// release_manifest_pending's own FK requires -- see this test's own
	// doc comment for why every OTHER seeded row reuses this SAME
	// session_id rather than each triggering its own webhook POST.
	body := issueCommentBodyWithCommenter("acme/concurrent-repo", "concurrent-repo", "https://github.com/acme/concurrent-repo.git", 400, "concurrent-check", commenterID, "concurrent-check-user")
	status := postWebhook(t, rig, body, "delivery-release-manifest-concurrent-seed")
	if status != http.StatusOK {
		t.Fatalf("postWebhook (seed session) status = %d, want %d", status, http.StatusOK)
	}

	var sessionID pgtype.UUID
	if err := rig.pool.QueryRow(ctx,
		`SELECT id FROM sessions WHERE spawn_source = 'github' ORDER BY created_at DESC LIMIT 1`,
	).Scan(&sessionID); err != nil {
		t.Fatalf("query session: %v", err)
	}

	// The one real webhook POST above already enqueued row #400 for us
	// (release/concurrent matches the configured release pattern) --
	// seed numPRs-1 MORE rows directly, one per additional fake PR
	// number, all against the SAME real session_id.
	for i := 1; i < numPRs; i++ {
		if _, err := pendingStore.Create(ctx, sqlcgen.CreateReleaseManifestPendingParams{
			SessionID: sessionID,
			Owner:     "acme",
			Repo:      "concurrent-repo",
			PrNumber:  int32(400 + i),
			BaseRef:   "main",
			HeadRef:   "release/concurrent",
		}); err != nil {
			t.Fatalf("seed release_manifest_pending row %d: %v", i, err)
		}
	}

	var seededCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM release_manifest_pending`).Scan(&seededCount); err != nil {
		t.Fatalf("query release_manifest_pending: %v", err)
	}
	if seededCount != numPRs {
		t.Fatalf("seeded release_manifest_pending count = %d, want %d", seededCount, numPRs)
	}

	sourceControl := &fakeReleaseManifestSourceControl{}
	worker := releasereview.NewWorker(pendingStore, releasereview.Deps{
		SourceControl: sourceControl,
		Outbox:        outboxStore,
		Timeouts:      platform.DefaultTimeouts(),
	}, "gho_bottoken", platform.DefaultTimeouts())

	const numConcurrentPods = 4
	var wg sync.WaitGroup
	for i := 0; i < numConcurrentPods; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := worker.PumpOnce(ctx); err != nil {
				t.Errorf("Worker.PumpOnce() (concurrent pod) error = %v", err)
			}
		}()
	}
	wg.Wait()

	// A single PumpOnce call only claims pendingBatchSize(5) rows -- drain
	// whatever the 4 concurrent calls above didn't collectively reach,
	// sequentially, exactly like a real Worker.Run loop's own NEXT tick
	// would.
	for i := 0; i < 10; i++ {
		var remaining int
		if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM release_manifest_pending`).Scan(&remaining); err != nil {
			t.Fatalf("query release_manifest_pending: %v", err)
		}
		if remaining == 0 {
			break
		}
		if err := worker.PumpOnce(ctx); err != nil {
			t.Fatalf("Worker.PumpOnce() (drain) error = %v", err)
		}
	}

	var remaining int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM release_manifest_pending`).Scan(&remaining); err != nil {
		t.Fatalf("query release_manifest_pending: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("release_manifest_pending rows left unclaimed = %d, want 0", remaining)
	}

	rows, err := rig.pool.Query(ctx, `SELECT payload::text FROM outbox WHERE kind = 'release_manifest'`)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()

	seenPRNumbers := make(map[int]int) // pr_number -> how many times it was processed
	total := 0
	for rows.Next() {
		var payloadText string
		if err := rows.Scan(&payloadText); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		var payload githubapi.ReleaseManifestPayload
		if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
			t.Fatalf("decode outbox payload: %v", err)
		}
		seenPRNumbers[payload.PRNumber]++
		total++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox rows: %v", err)
	}

	if total != numPRs {
		t.Fatalf("total release_manifest outbox rows = %d, want %d (some seeded checks were dropped)", total, numPRs)
	}
	for pr, count := range seenPRNumbers {
		if count != 1 {
			t.Errorf("PR #%d was processed %d times, want exactly 1 (double-processing under concurrent PumpOnce calls)", pr, count)
		}
	}
}
