//go:build integration

// The other deferred work technical plan §43.20's inventory found (review
// round 3's P1, closing the class rather than the one timer): a release
// PR's review session gets a release_manifest_pending row, and the
// background worker (releasereview.Worker) later runs the check, which
// inserts one more turn -- the composition review -- on that same session
// when the aggregate review is triggered. On real Postgres, through the
// real worker and the real status handler: the session reads scheduled
// while the check waits AND while it runs (the claim records the check as
// running until it returns, migrations/000146), queued once the composition turn
// exists, and settled again at once when the check triggers nothing.
package github_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/releasereview"
	"github.com/narvidev/narvi/internal/app/sessionactivity"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/platform"
)

// holdingMergedPRLister answers ListMergedBetween -- the check's first,
// slowest step -- with no merged PRs, truncated or not (truncated coverage
// alone triggers the composition pass), and can hold the call open until
// released, so a test reads the status while the check runs.
type holdingMergedPRLister struct {
	truncated bool
	entered   chan struct{}
	release   chan struct{}
}

func (f *holdingMergedPRLister) ListMergedBetween(ctx context.Context, _ ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	return nil, f.truncated, nil
}

// noopCompositionDispatch stands in for the registry's EnsureDispatched:
// the composition turn stays pending, which is all the status reads.
type noopCompositionDispatch struct{}

func (noopCompositionDispatch) EnsureDispatched(context.Context, pgtype.UUID) error { return nil }

func TestSessionStatus_ReleaseManifestCheckIsScheduledUntilItsCompositionTurnExists(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	// The worker claims the oldest row in the whole table: start from an
	// empty queue so the row it claims is this test's.
	clearQueue := func() {
		for _, table := range []string{"release_manifest_pending", "release_manifest_checks_running"} {
			if _, err := pool.Exec(ctx, `DELETE FROM `+table); err != nil {
				t.Fatalf("clear %s: %v", table, err)
			}
		}
	}
	clearQueue()
	t.Cleanup(clearQueue)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	pendingStore := narvipg.NewReleaseManifestPendingStore(pool)
	router := chi.NewRouter()
	router.Get("/api/sessions/{sessionID}/status", httpapi.GetSessionStatus(sessions, sessionactivity.NewWaiter(sessionactivity.ConfigFrom(platform.DefaultTimeouts())), platform.DefaultTimeouts()))
	fixture := retriggerStatusFixture{status: router}

	for _, tc := range []struct {
		name         string
		triggers     bool
		wantActivity restdtos.SessionActivityActivity
		wantTurns    int
	}{
		{"the check triggers the composition pass: scheduled until its turn exists, then queued", true, restdtos.SessionActivityActivityQueued, 2},
		{"the check triggers nothing: scheduled until it returns, then settled at once", false, restdtos.SessionActivityActivityFinished, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			review, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusCompleted, IsReviewAttempt: true})
			if err != nil {
				t.Fatalf("create the completed first review: %v", err)
			}
			f := fixture
			f.sessionID, f.lastTurnID = sess.ID, review.ID

			if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled {
				t.Fatalf("before the check is enqueued: activity %q settled %v, want finished and settled", got.Activity, got.Settled)
			}

			if _, err := pendingStore.Create(ctx, sqlcgen.CreateReleaseManifestPendingParams{
				SessionID: sess.ID, Owner: "example", Repo: fmt.Sprintf("release-status-%d", time.Now().UnixNano()), PrNumber: 7, BaseRef: "main", HeadRef: "release/1.0",
			}); err != nil {
				t.Fatalf("enqueue the release manifest check: %v", err)
			}
			got := f.mustRead(t)
			if got.Activity != restdtos.SessionActivityActivityScheduled || got.Settled || got.SuggestedDelaySeconds < 2 || got.SuggestedDelaySeconds > 15 {
				t.Fatalf("the check enqueued, not claimed: activity %q settled %v delay %d, want scheduled, not settled, within [2, 15]", got.Activity, got.Settled, got.SuggestedDelaySeconds)
			}

			lister := &holdingMergedPRLister{truncated: tc.triggers, entered: make(chan struct{}), release: make(chan struct{})}
			worker, workerErr := releasereview.NewWorker(pendingStore, releasereview.Deps{
				SourceControl:          lister,
				Outbox:                 narvipg.NewOutboxStore(pool, false),
				CompositionTemplates:   narvipg.NewPromptTemplateStore(pool),
				CompositionDiffFetcher: &fakeReviewContextFetcher{pr: githubapi.PullRequest{HeadSHA: "sha-release-head", BaseRef: "main"}, diff: "+ the release's aggregate diff"},
				CompositionTurns:       narvipg.NewLockedTurnCreator(pool),
				CompositionDispatch:    noopCompositionDispatch{},
				CompositionGuard:       turnguard.New(pool, nil, false),
				Timeouts:               platform.DefaultTimeouts(),
			}, platform.MustNewGitHubOutboundConfig("gho_bottoken"), platform.DefaultTimeouts())
			if workerErr != nil {
				t.Fatalf("releasereview.NewWorker: %v", workerErr)
			}

			var (
				mu   sync.Mutex
				seen []restdtos.SessionActivity
				rerr error
			)
			stop := make(chan struct{})
			var g errgroup.Group
			g.Go(func() error {
				for {
					select {
					case <-stop:
						return nil
					default:
					}
					got, err := f.read()
					mu.Lock()
					if err != nil && rerr == nil {
						rerr = err
					}
					seen = append(seen, got)
					mu.Unlock()
				}
			})
			// Cancelled on the way out, so a failed assertion never leaves
			// the tick waiting on a lister no one will release.
			workerCtx, cancelWorker := context.WithCancel(ctx)
			defer cancelWorker()
			g.Go(func() error { return worker.PumpOnce(workerCtx) })

			select {
			case <-lister.entered:
			case <-time.After(10 * time.Second):
				close(stop)
				t.Fatal("the worker never started the check")
			}
			running := f.mustRead(t)
			if running.Activity != restdtos.SessionActivityActivityScheduled || running.Settled || running.PendingTurns != 0 {
				close(stop)
				t.Fatalf("while the check runs: activity %q settled %v pending %d, want scheduled, not settled, no turn yet", running.Activity, running.Settled, running.PendingTurns)
			}
			var pending, runningRows int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM release_manifest_pending WHERE session_id = $1), (SELECT count(*) FROM release_manifest_checks_running WHERE session_id = $1)`, sess.ID).Scan(&pending, &runningRows); err != nil || pending != 0 || runningRows != 1 {
				close(stop)
				t.Fatalf("while the check runs: %d pending, %d running (%v), want the pending row claimed (deleted) and the check recorded as running", pending, runningRows, err)
			}
			close(lister.release)

			// The watcher stops once the worker's tick has returned.
			deadline := time.Now().Add(10 * time.Second)
			for {
				var left int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM release_manifest_checks_running WHERE session_id = $1`, sess.ID).Scan(&left); err != nil {
					t.Fatal(err)
				}
				if left == 0 {
					break
				}
				if time.Now().After(deadline) {
					close(stop)
					t.Fatal("the worker never finished the check's running row")
				}
				time.Sleep(10 * time.Millisecond)
			}
			close(stop)
			if err := g.Wait(); err != nil {
				t.Fatalf("worker tick: %v", err)
			}
			if rerr != nil {
				t.Fatalf("status read while the check ran: %v", rerr)
			}
			if len(seen) == 0 {
				t.Fatal("no status was read while the check ran")
			}
			for i, got := range seen {
				if got.Activity == restdtos.SessionActivityActivityFinished && tc.triggers {
					t.Fatalf("read %d while the check ran: finished -- the session read settled before its composition turn existed", i)
				}
			}

			list, err := turns.ListForSession(ctx, sess.ID)
			if err != nil || len(list) != tc.wantTurns {
				t.Fatalf("after the check: %d turns (%v), want %d", len(list), err, tc.wantTurns)
			}
			after := f.mustRead(t)
			if after.Activity != tc.wantActivity || after.Settled != (tc.wantActivity == restdtos.SessionActivityActivityFinished) {
				t.Fatalf("after the check: activity %q settled %v, want %q", after.Activity, after.Settled, tc.wantActivity)
			}
			if tc.triggers && after.PendingTurns != 1 {
				t.Fatalf("after the check: pendingTurns %d, want the composition turn queued", after.PendingTurns)
			}
			if !tc.triggers && after.SuggestedDelaySeconds != 300 {
				t.Fatalf("after a check that triggered nothing: delay %d, want 300", after.SuggestedDelaySeconds)
			}
		})
	}
}
