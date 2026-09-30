//go:build integration

package outboxworker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/platform"
)

// runsOn returns the ids of every narvi/review check run this fake holds
// for headSHA, ascending.
func (f *fakeCheckRunGitHub) runsOn(headSHA string) []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []int64
	for id, run := range f.runs {
		if run.HeadSHA == headSHA && run.Name == reviewcheck.CheckName {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// createCount is how many creates this fake has recorded, read under its
// lock: the handler writes the count under that lock, so a read racing a
// request must take it too.
func (f *fakeCheckRunGitHub) createCount() int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

// runningReviewCheckPayload is a running-phase review check payload for one
// pull request's head, on a real attempt row.
func runningReviewCheckPayload(ctx context.Context, t *testing.T, pool *pgxpool.Pool, owner, repo string, prNumber int, headSHA string) []byte {
	t.Helper()
	payload, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repo, PRNumber: prNumber, HeadSHA: headSHA,
		AttemptID: newTestAttempt(ctx, t, pool), AttemptCreatedAt: time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC),
		Phase: "running",
	})
	if err != nil {
		t.Fatalf("marshal review check payload: %v", err)
	}
	return payload
}

// TestShutdown_InterruptedReviewCheckIsRepeatedOnlyAfterItLands is the
// two-replica ordering §5.1's settle delay exists for. Replica A's
// shutdown cuts its create of a pull request's first narvi/review check
// run after the code host received it; the host goes on processing it,
// and it lands a second later. Replica B pumps at once, while the create
// is still in flight: the interrupted row must not be due yet, or B finds
// no run to adopt and creates a second one, which nothing ever updates
// again. Once the settle delay has passed, B repeats the delivery, finds
// A's run and adopts it: exactly one run, recorded as the row's.
func TestShutdown_InterruptedReviewCheckIsRepeatedOnlyAfterItLands(t *testing.T) {
	ctx := context.Background()
	pool := IntegrationTestPool(t)
	outbox := narvipg.NewOutboxStore(pool, false)
	checks := narvipg.NewReviewCheckRunStore(pool)
	logs := captureLogs(t)

	fake := newFakeCheckRunGitHub()
	// Armed for replica A's create only. Set before the server serves
	// anything, so the handler reads a field nobody writes afterwards.
	var armed atomic.Bool
	a := newProcess(t)
	const landingDelay = time.Second
	createsBeforeA := make(chan int32, 1)
	fake.onCreate = func() {
		if !armed.CompareAndSwap(true, false) {
			return
		}
		// A's shutdown begins with its create received; A stops
		// listening, and the host lands the create landingDelay later.
		a.shutDown()
		time.Sleep(landingDelay)
		createsBeforeA <- fake.createCount()
	}
	server := fake.server()
	defer server.Close()
	adapter := githubapi.New(server.Client(), server.URL)
	outbound := platform.MustNewGitHubOutboundConfig("tok")
	kind := ports.NotificationKindGitHubReviewCheck

	// Warm-up on another pull request: the deployment learns and persists
	// its writer App, so a repeat that runs after A's create has landed
	// adopts it.
	owner, repo := "acme", fmt.Sprintf("settle-repo-%d", time.Now().UnixNano())
	notifierA := mustNotifier(outboxworker.NewReviewCheckNotifier(pool, checks, adapter, outbound))
	if err := notifierA.Deliver(ctx, ports.Notification{Kind: kind, Payload: runningReviewCheckPayload(ctx, t, pool, owner, repo, 1, "0000aaaa")}); err != nil {
		t.Fatalf("warm-up delivery: %v", err)
	}

	const prNumber, headSHA = 7, "5e771e5e"
	row, err := outbox.Create(ctx, sqlcgen.CreateOutboxEntryParams{Kind: string(kind), Payload: runningReviewCheckPayload(ctx, t, pool, owner, repo, prNumber, headSHA)})
	if err != nil {
		t.Fatalf("enqueue review check: %v", err)
	}

	// Replica A: the delivery is cut short with the create in flight.
	armed.Store(true)
	builderA := newShutdownTestBuilder(t, pool, outbox, map[ports.NotificationKind]ports.Notifier{kind: notifierA}, platform.DefaultTimeouts(), a.state)
	if err := builderA.PumpOnce(a.ctx); err != nil {
		t.Fatalf("replica A: PumpOnce: %v", err)
	}
	requireRow(t, getRow(t, outbox, row.ID), want{attempts: 0, interrupted: 1, settling: true})
	logs.requireShutdownWarning(t, row.ID, "shutdown_interrupted")

	// Replica B, a process of its own with a notifier that has never
	// created anything, pumps at once: A's create has not landed.
	b := newProcess(t)
	notifierB := mustNotifier(outboxworker.NewReviewCheckNotifier(pool, checks, adapter, outbound))
	builderB := newShutdownTestBuilder(t, pool, outbox, map[ports.NotificationKind]ports.Notifier{kind: notifierB}, platform.DefaultTimeouts(), b.state)
	absorbClockSkew(t, pool, row.ID)
	if err := builderB.PumpOnce(b.ctx); err != nil {
		t.Fatalf("replica B, at once: PumpOnce: %v", err)
	}

	// A's create lands.
	before := <-createsBeforeA
	deadline := time.Now().Add(5 * time.Second)
	for fake.createCount() <= before {
		if time.Now().After(deadline) {
			t.Fatal("replica A's create never landed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The settle delay passes; B repeats the delivery.
	makeDue(t, pool, row.ID)
	if err := builderB.PumpOnce(b.ctx); err != nil {
		t.Fatalf("replica B, after the settle delay: PumpOnce: %v", err)
	}

	runs := fake.runsOn(headSHA)
	if len(runs) != 1 {
		t.Fatalf("narvi/review runs on the head = %v, want exactly one: a repeat that ran before the cut create landed made its own", runs)
	}
	recorded, err := checks.GetByRepoAndPRNumber(ctx, owner+"/"+repo, prNumber)
	if err != nil {
		t.Fatalf("read the review check row: %v", err)
	}
	if recorded.ExternalID == nil || *recorded.ExternalID != runs[0] {
		t.Fatalf("recorded external id = %v, want the one run, %d, adopted", recorded.ExternalID, runs[0])
	}
	if got := getRow(t, outbox, row.ID); got.Status != sqlcgen.OutboxStatusDelivered {
		t.Fatalf("outbox row status = %q, want delivered by replica B", got.Status)
	}
	logs.requireNoErrorLogs(t)
}
