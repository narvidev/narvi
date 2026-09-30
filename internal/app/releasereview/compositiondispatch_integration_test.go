//go:build integration

package releasereview_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/releasereview"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
)

// spawnRecorder is a ports.SandboxProvider counting CreateSandbox calls:
// the dispatch evaluation of a pending turn on a session with no sandbox.
type spawnRecorder struct {
	mu      sync.Mutex
	creates int
}

func (p *spawnRecorder) Capabilities() ports.Capabilities { return ports.Capabilities{} }
func (p *spawnRecorder) CreateSandbox(context.Context, ports.CreateSpec) (ports.SandboxRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates++
	return ports.SandboxRef{ProviderID: "composition-review-sandbox"}, nil
}
func (p *spawnRecorder) StopSandbox(context.Context, ports.SandboxRef) error   { return nil }
func (p *spawnRecorder) ResumeSandbox(context.Context, ports.SandboxRef) error { return nil }
func (p *spawnRecorder) TakeSnapshot(context.Context, ports.SandboxRef) (ports.SnapshotID, error) {
	return "", errors.New("spawnRecorder: no snapshots")
}
func (p *spawnRecorder) RestoreFromSnapshot(context.Context, ports.SnapshotID, ports.CreateSpec) (ports.SandboxRef, error) {
	return ports.SandboxRef{}, errors.New("spawnRecorder: no snapshots")
}
func (p *spawnRecorder) BuildImage(context.Context, ports.ImageSpec) (ports.BuildOutcome, error) {
	return ports.BuildOutcome{}, errors.New("spawnRecorder: no image builds")
}
func (p *spawnRecorder) DeleteImage(context.Context, ports.ImageRef) error { return nil }
func (p *spawnRecorder) List(context.Context) ([]ports.SandboxRef, error)  { return nil, nil }

func (p *spawnRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.creates
}

// registryDispatcher is controlplane's releaseCompositionDispatcher over a
// given registry: GetOrSpawn, then EnsureDispatched.
type registryDispatcher struct{ registry *sessionactor.Registry }

func (d registryDispatcher) EnsureDispatched(ctx context.Context, sessionID pgtype.UUID) error {
	actor, err := d.registry.GetOrSpawn(ctx, sessionID)
	if err != nil {
		return err
	}
	return actor.Send(ctx, sessionactor.EnsureDispatched{})
}

// TestRun_CompositionTurnWithAFailedTriggerIsDispatchedByThePump: the
// release composition review runs on the release manifest worker, which
// with more than one replica is often not on the one that can host the
// review session. Its turn is inserted with the session's dispatch timer
// (postgres.LockedTurnCreator), so when its post-commit trigger fails --
// here the worker's replica cannot host actors (actor_unavailable) -- and
// that replica's pump claims the timer first and fails again, keeping the
// row, the replica that can host the actor delivers the dispatch when it
// wins the lapsed claim -- here the single lapse that follows, so within
// one claim window of the failed claim (a replica that cannot host may win
// a lapse again, one more window each time): on a session with no
// sandbox, a spawn, exactly once.
func TestRun_CompositionTurnWithAFailedTriggerIsDispatchedByThePump(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	timeouts := platform.DefaultTimeouts()
	timeouts.TimerClaimDuration = 2 * time.Second
	hostProvider := &spawnRecorder{}
	host, err := sessionactor.NewRegistry(ctx, pool, timeouts, nil, nil, hostProvider, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Shutdown() })
	workerTimeouts := timeouts
	workerTimeouts.ActorHydrateTimeout = time.Nanosecond
	workerProvider := &spawnRecorder{}
	worker, err := sessionactor.NewRegistry(ctx, pool, workerTimeouts, nil, nil, workerProvider, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Shutdown() })

	lister := &fakeMergedPRLister{merged: []ports.MergedPR{
		{Number: 1, Title: "a", HasApprovingReview: true, Labels: []string{reviewpost.LabelHighRisk}},
	}}
	templates := &fakeCompositionTemplateFetcher{template: "Review this release's composition."}
	diffFetcher := &fakeCompositionDiffFetcher{
		pr:               githubapi.PullRequest{HeadSHA: "deadbeef", BaseRef: "main"},
		resolveBranchSHA: "main-tip-sha",
		diff:             "diff --git a/x b/x\n+hello\n",
	}
	deps := fullCompositionDeps(lister, &fakeOutboxEnqueuer{}, templates, diffFetcher, &fakeCompositionTurnInserter{}, &fakeCompositionDispatcher{})
	deps.CompositionTurns = narvipg.NewLockedTurnCreator(pool)
	deps.CompositionDispatch = registryDispatcher{registry: worker}

	releasereview.Run(ctx, discardLogger(), deps, releasereview.Input{
		SessionID: session.ID,
		Owner:     "acme", Repo: "widgets", PRNumber: 99, BaseRef: "main", HeadRef: "release/1.0", Token: "gho_bottoken",
	})

	turns, err := narvipg.NewTurnStore(pool).ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Status != sqlcgen.TurnStatusPending {
		t.Fatalf("turns = %+v, want the one pending composition review turn", turns)
	}
	timers := narvipg.NewTimerStore(pool)
	hasTimer := func() bool {
		t.Helper()
		_, err := timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: session.ID, Name: sessionactor.TimerDispatch})
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true
	}
	if !hasTimer() {
		t.Fatal("the composition review turn was committed without the session's dispatch timer")
	}

	claimedAt := time.Now()
	if err := worker.PumpOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !hasTimer() {
		t.Fatal("the worker's replica dropped the dispatch timer it could not deliver")
	}
	deadline := time.Now().Add(timeouts.TimerClaimDuration + time.Second)
	for hostProvider.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the composition review turn was not dispatched within one claim window (%v) of the failed claim", timeouts.TimerClaimDuration)
		}
		if err := host.PumpOnce(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if elapsed := time.Since(claimedAt); elapsed > timeouts.TimerClaimDuration+time.Second {
		t.Fatalf("dispatched %v after the failed claim, want within one claim window", elapsed)
	}
	for deadline := time.Now().Add(5 * time.Second); hasTimer(); {
		if time.Now().After(deadline) {
			t.Fatal("the dispatch timer outlived the evaluation it stood for")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := workerProvider.count(); n != 0 {
		t.Fatalf("the worker's replica spawned %d sandboxes, want 0", n)
	}
	if n := hostProvider.count(); n != 1 {
		t.Fatalf("the hosting replica spawned %d sandboxes, want 1", n)
	}
}
