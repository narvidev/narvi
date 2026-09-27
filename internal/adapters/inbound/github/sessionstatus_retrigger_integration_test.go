//go:build integration

// Review round 3's P1 on real Postgres, end to end (technical plan
// §43.20): a push to a pull request that has a review session arms the
// automatic re-review -- the real, signed pull_request/synchronize
// delivery through NewHandler writes pending_retrigger_head_sha and the
// review_retrigger_debounce timer, and no turn -- and the review turn is
// inserted only when that timer fires, with no further input, through the
// real timer pump (Registry.PumpOnce) and the real session actor. For that
// whole window the session's status, read through the real
// httpapi.GetSessionStatus handler, must not say settled.
package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// retriggerStatusFixture is one review session on its own repository and
// pull request, with one completed review turn, the status route mounted
// the way controlplane/serve.go mounts it (minus the sign-in middleware,
// which this route shares with every other session route), and a real
// session registry whose re-review fetches the pull request from a fake.
type retriggerStatusFixture struct {
	rig          testRig
	sessionID    pgtype.UUID
	lastTurnID   pgtype.UUID
	repoFullName string
	prNumber     int
	timers       *narvipg.TimerStore
	registry     *sessionactor.Registry
	status       http.Handler
}

func newRetriggerStatusFixture(ctx context.Context, t *testing.T, optedIn bool) retriggerStatusFixture {
	t.Helper()
	pool := newTestPool(t)
	timers := narvipg.NewTimerStore(pool)
	rig := newTestRig(t, withTimers(timers), func(cfg *githubingress.Config) { cfg.Timeouts = platform.DefaultTimeouts() })

	sessions := narvipg.NewSessionStore(pool)
	sess, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	repoFullName := fmt.Sprintf("example/retrigger-status-%d", time.Now().UnixNano())
	const prNumber = 42
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("ensure github pr session row: %v", err)
	}
	if err := prSessions.SetSessionID(ctx, repoFullName, prNumber, sess.ID); err != nil {
		t.Fatalf("set github pr session id: %v", err)
	}
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("arm live egress: %v", err)
	}
	if optedIn {
		if _, err := repoSettings.UpsertAutoRetriggerReviewToggle(ctx, repoFullName, true); err != nil {
			t.Fatalf("opt in to the automatic re-review: %v", err)
		}
	}
	review, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusCompleted, IsReviewAttempt: true})
	if err != nil {
		t.Fatalf("create the completed review turn: %v", err)
	}

	fetcher := &fakeReviewContextFetcher{pr: githubapi.PullRequest{HeadSHA: "sha-live-after-push", BaseRef: "main"}, diff: "+ a line the push changed"}
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false,
		sessionactor.RegistryOptions{ReviewDiffFetcher: fetcher, GitHubBotHandle: "narvi-bot", GitHubBotToken: "test-token"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	router := chi.NewRouter()
	router.Get("/api/sessions/{sessionID}/status", httpapi.GetSessionStatus(sessions, platform.DefaultTimeouts()))
	return retriggerStatusFixture{
		rig: rig, sessionID: sess.ID, lastTurnID: review.ID, repoFullName: repoFullName, prNumber: prNumber,
		timers: timers, registry: registry, status: router,
	}
}

// read answers the status route for the fixture's session, or an error --
// safe to call off the test goroutine.
func (f retriggerStatusFixture) read() (restdtos.SessionActivity, error) {
	rec := httptest.NewRecorder()
	f.status.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+f.sessionID.String()+"/status", nil))
	var got restdtos.SessionActivity
	if rec.Code != http.StatusOK {
		return got, fmt.Errorf("GET status: %d %s", rec.Code, rec.Body.String())
	}
	return got, json.Unmarshal(rec.Body.Bytes(), &got)
}

func (f retriggerStatusFixture) mustRead(t *testing.T) restdtos.SessionActivity {
	t.Helper()
	got, err := f.read()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (f retriggerStatusFixture) push(t *testing.T, headSHA string) {
	t.Helper()
	if status := postWebhookEventType(t, f.rig, pullRequestSynchronizeBody(f.repoFullName, f.prNumber, headSHA), "delivery-"+headSHA+"-"+f.sessionID.String(), "pull_request"); status != http.StatusOK {
		t.Fatalf("synchronize webhook: %d, want 200", status)
	}
}

func (f retriggerStatusFixture) debounceArmed(ctx context.Context, t *testing.T) (sqlcgen.SessionTimer, bool) {
	t.Helper()
	return getReviewRetriggerDebounceTimer(ctx, t, f.timers, f.sessionID)
}

// comeDue moves the armed debounce's fires_at to the database's now(),
// standing in for ReviewRetriggerDebounce passing.
func (f retriggerStatusFixture) comeDue(ctx context.Context, t *testing.T) {
	t.Helper()
	if _, err := f.rig.pool.Exec(ctx, `UPDATE session_timers SET fires_at = now() WHERE session_id = $1 AND name = $2`, f.sessionID, sessionactor.TimerReviewRetriggerDebounce); err != nil {
		t.Fatalf("bring the debounce due: %v", err)
	}
}

// fireWhileWatching runs real timer-pump ticks until the debounce row is
// gone (its handler created the turn or declined),
// while a reader polls the status route the whole time. It returns every
// activity that reader saw.
func (f retriggerStatusFixture) fireWhileWatching(ctx context.Context, t *testing.T) []restdtos.SessionActivity {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []restdtos.SessionActivity
		rerr error
	)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
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
	}()
	// Ticks until the debounce is handled, like RunTimerPump: the test
	// database is shared, so an earlier tick's batch may be other tests'
	// leftovers. A claimed timer is never claimed again within its claim
	// window, so a later tick never delivers ours twice.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := f.registry.PumpOnce(ctx); err != nil {
			close(stop)
			<-done
			t.Fatalf("PumpOnce: %v", err)
		}
		if _, armed := f.debounceArmed(ctx, t); !armed {
			break
		}
		if time.Now().After(deadline) {
			close(stop)
			<-done
			t.Fatal("the debounce timer was never handled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	<-done
	if rerr != nil {
		t.Fatalf("status read while the timer fired: %v", rerr)
	}
	return seen
}

func wantScheduled(t *testing.T, stage string, got restdtos.SessionActivity, wantDelay int, lastTurnID pgtype.UUID) {
	t.Helper()
	if got.Activity != restdtos.SessionActivityActivityScheduled || got.Settled || got.SuggestedDelaySeconds != wantDelay {
		t.Fatalf("%s: activity %q settled %v delay %d, want scheduled, not settled, %d", stage, got.Activity, got.Settled, got.SuggestedDelaySeconds, wantDelay)
	}
	if got.PendingTurns != 0 || got.InFlightTurn != nil || got.LastRun == nil || got.LastRun.TurnId != lastTurnID.String() {
		t.Fatalf("%s: pendingTurns %d inFlightTurn %+v lastRun %+v, want no turn yet and the old review as the last run", stage, got.PendingTurns, got.InFlightTurn, got.LastRun)
	}
}

// TestSessionStatus_AutomaticReReviewIsScheduledUntilItsTurnExists is the
// reviewers' reproduction: finished before the push; scheduled -- never
// settled, suggested delay capped at 15 s and never past the debounce's
// own due instant plus the 5 s margin -- from the moment the webhook
// commits until the review turn exists; then queued, with that turn.
func TestSessionStatus_AutomaticReReviewIsScheduledUntilItsTurnExists(t *testing.T) {
	ctx := context.Background()
	f := newRetriggerStatusFixture(ctx, t, true)

	if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.SuggestedDelaySeconds != 300 {
		t.Fatalf("before the push: activity %q settled %v delay %d, want finished, settled, 300", got.Activity, got.Settled, got.SuggestedDelaySeconds)
	}

	f.push(t, "sha-pushed")
	timer, armed := f.debounceArmed(ctx, t)
	if !armed {
		t.Fatal("the synchronize webhook armed no debounce")
	}
	wantScheduled(t, "the push accepted, the debounce armed "+time.Until(timer.FiresAt.Time).Round(time.Second).String()+" out", f.mustRead(t), 15, f.lastTurnID)
	if turns, err := f.rig.turns.ListForSession(ctx, f.sessionID); err != nil || len(turns) != 1 {
		t.Fatalf("after the push: %d turns (err %v), want still only the old review -- the webhook creates none", len(turns), err)
	}

	f.comeDue(ctx, t)
	wantScheduled(t, "the debounce due, not yet fired", f.mustRead(t), 5, f.lastTurnID)

	seen := f.fireWhileWatching(ctx, t)
	if len(seen) == 0 {
		t.Fatal("no status was read while the debounce fired")
	}
	for i, got := range seen {
		if got.Settled || got.Activity == restdtos.SessionActivityActivityFinished || got.Activity == restdtos.SessionActivityActivityIdle {
			t.Fatalf("read %d while the debounce fired: activity %q settled %v -- the session read settled before its review turn existed", i, got.Activity, got.Settled)
		}
	}

	turns, err := f.rig.turns.ListForSession(ctx, f.sessionID)
	if err != nil || len(turns) != 2 {
		t.Fatalf("after the fire: %d turns (err %v), want the old review and the new one", len(turns), err)
	}
	got := f.mustRead(t)
	if got.Settled || (got.Activity != restdtos.SessionActivityActivityQueued && got.Activity != restdtos.SessionActivityActivityRunning) {
		t.Fatalf("the review turn exists: activity %q settled %v, want queued or running", got.Activity, got.Settled)
	}
	if got.Activity == restdtos.SessionActivityActivityQueued && got.PendingTurns != 1 {
		t.Fatalf("the review turn exists: pendingTurns %d, want 1", got.PendingTurns)
	}
}

// TestSessionStatus_DeclinedReReviewSettlesWithinTheBound: a repository
// that has not opted in still gets the debounce armed on every push (the
// webhook reads no opt-in, §24), so the session reads scheduled -- the
// safe side -- until the timer fires and declines; then settled again, at
// once, well within the bound the suggested delay promised (the due
// instant plus MCPStatusScheduledMargin), with no turn created.
func TestSessionStatus_DeclinedReReviewSettlesWithinTheBound(t *testing.T) {
	ctx := context.Background()
	f := newRetriggerStatusFixture(ctx, t, false)

	f.push(t, "sha-not-opted-in")
	if _, armed := f.debounceArmed(ctx, t); !armed {
		t.Fatal("the synchronize webhook armed no debounce")
	}
	wantScheduled(t, "not opted in, the debounce armed", f.mustRead(t), 15, f.lastTurnID)

	f.comeDue(ctx, t)
	due := time.Now()
	wantScheduled(t, "the debounce due", f.mustRead(t), 5, f.lastTurnID)

	f.fireWhileWatching(ctx, t)
	got := f.mustRead(t)
	margin := platform.DefaultTimeouts().MCPStatusScheduledMargin
	if elapsed := time.Since(due); elapsed > margin {
		t.Fatalf("the declined fire took %v past its due instant, beyond the %v margin the suggestion promised", elapsed, margin)
	}
	if got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.SuggestedDelaySeconds != 300 {
		t.Fatalf("declined: activity %q settled %v delay %d, want finished, settled, 300", got.Activity, got.Settled, got.SuggestedDelaySeconds)
	}
	if turns, err := f.rig.turns.ListForSession(ctx, f.sessionID); err != nil || len(turns) != 1 {
		t.Fatalf("declined: %d turns (err %v), want only the old review", len(turns), err)
	}
}
