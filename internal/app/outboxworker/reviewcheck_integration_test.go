//go:build integration

// Integration tests proving the review-check publisher's own identity
// rules (§21.1b) against a REAL Postgres instance -- the SAME
// shared testcontainers pool every other *_integration_test.go file in
// this package uses (builder_integration_test.go's own TestMain/
// newTestPool). A fake GitHub server (httptest) stands in for the real
// Checks API.
package outboxworker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
)

// newTestAttempt creates a real session + turn -- review_check_runs.
// attempt_id is a genuine foreign key onto turns(id) (migrations/
// 000132's own doc comment), so a payload's AttemptID must name a REAL
// row, never a made-up UUID -- and returns the turn's own id as a plain
// string, exactly the shape ports.ReviewCheckPayload.AttemptID carries.
func newTestAttempt(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	turn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusPending})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	return turn.ID.String()
}

// fakeCheckRunGitHub is a minimal, in-memory stand-in for GitHub's real
// Checks API -- CreateCheckRun assigns incrementing ids; UpdateCheckRun
// and CreateCheckRun both record the posted status/conclusion/title
// keyed by id, so a test can assert what the LAST write for a given id
// actually was; ListCheckRunsForRef always reports empty (this suite
// never exercises the recovery/adoption path -- checkruns_test.go's own
// TestListCheckRunsForRef_FiltersByAppAndName already covers that in
// isolation).
type fakeCheckRunGitHub struct {
	mu      sync.Mutex
	nextID  int64
	states  map[int64]map[string]any
	creates int32
	updates int32
}

func newFakeCheckRunGitHub() *fakeCheckRunGitHub {
	return &fakeCheckRunGitHub{states: map[int64]map[string]any{}}
}

func (f *fakeCheckRunGitHub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		switch {
		case r.Method == http.MethodPost:
			f.mu.Lock()
			f.nextID++
			id := f.nextID
			f.states[id] = body
			f.creates++
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case r.Method == http.MethodPatch:
			id := parseTrailingID(r.URL.Path)
			f.mu.Lock()
			f.states[id] = body
			f.updates++
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "check_runs": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func parseTrailingID(path string) int64 {
	var id int64
	_, _ = fmt.Sscanf(lastSegment(path), "%d", &id)
	return id
}

func lastSegment(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

func (f *fakeCheckRunGitHub) stateFor(id int64) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[id]
}

func (f *fakeCheckRunGitHub) counts() (creates, updates int32) {
	return atomic.LoadInt32(&f.creates), atomic.LoadInt32(&f.updates)
}

// TestReviewCheckNotifier_OlderAttemptRefusedAfterNewerAlreadyPublished
// is the identity-refusal test the brief asks for, proven through the
// full publisher (store + notifier + a real GitHub-shaped server), not
// only the pure domain function (internal/domain/reviewcheck's own
// TestSupersedes_OlderAttemptLosesToNewerAttempt already proves the
// rule in isolation): a NEWER attempt's terminal result is published
// first; an OLDER attempt's own (redelivered, out-of-order) emission for
// the SAME head sha must then be refused outright -- no GitHub call at
// all, and the stored row must still reflect the newer attempt
// afterward.
func TestReviewCheckNotifier_OlderAttemptRefusedAfterNewerAlreadyPublished(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewReviewCheckRunStore(pool)

	fake := newFakeCheckRunGitHub()
	server := fake.server()
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := outboxworker.NewReviewCheckNotifier(pool, store, adapter, "tok", 999)

	const prNumber = 42
	const headSHA = "deadbeef"

	older := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)

	newerAttemptID := newTestAttempt(ctx, t, pool)
	olderAttemptID := newTestAttempt(ctx, t, pool)

	// Timestamp-suffixed repo name so concurrent test runs never collide
	// on the same claim row.
	owner, repoName := "acme", fmt.Sprintf("refusal-repo-%d", time.Now().UnixNano())
	newerPayload, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prNumber, HeadSHA: headSHA,
		AttemptID: newerAttemptID, AttemptCreatedAt: newer,
		Phase: "terminal_assessed",
	})
	if err != nil {
		t.Fatalf("marshal newer payload: %v", err)
	}
	olderPayload, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prNumber, HeadSHA: headSHA,
		AttemptID: olderAttemptID, AttemptCreatedAt: older,
		Phase: "running",
	})
	if err != nil {
		t.Fatalf("marshal older payload: %v", err)
	}

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: newerPayload}); err != nil {
		t.Fatalf("Deliver(newer) error = %v", err)
	}
	creatsAfterNewer, updatesAfterNewer := fake.counts()
	if creatsAfterNewer != 1 {
		t.Fatalf("creates after newer = %d, want 1", creatsAfterNewer)
	}

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: olderPayload}); err != nil {
		t.Fatalf("Deliver(older) error = %v", err)
	}

	creatsAfterOlder, updatesAfterOlder := fake.counts()
	if creatsAfterOlder != creatsAfterNewer || updatesAfterOlder != updatesAfterNewer {
		t.Fatalf("the older, superseded attempt made a GitHub call: creates %d->%d, updates %d->%d -- want no change",
			creatsAfterNewer, creatsAfterOlder, updatesAfterNewer, updatesAfterOlder)
	}

	row, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prNumber)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Phase != "terminal_assessed" {
		t.Errorf("row.Phase = %q, want terminal_assessed (the newer attempt must still stand)", row.Phase)
	}
	if !row.AttemptID.Valid || row.AttemptID.String() != newerAttemptID {
		t.Errorf("row.AttemptID = %v, want the newer attempt's id %s", row.AttemptID, newerAttemptID)
	}
}

// TestReviewCheckNotifier_ConcurrentAttempts_ResolveToOneIdentity is the
// creation-race test the brief asks for: two concurrent Deliver calls
// for the SAME pull request, carrying two DIFFERENT attempts (a genuine
// race on which one claims the row), must resolve to exactly ONE
// identity -- the row converges on the NEWER attempt's own data
// regardless of which goroutine happens to acquire the claim lock
// first (Postgres's own SELECT ... FOR UPDATE serializes the two
// claim sequences; whichever runs second always re-evaluates
// reviewcheck.Supersedes against the other's already-committed result),
// and the external check run GitHub itself ends up holding reflects that
// SAME newer attempt, never a stale, lost update from the loser.
func TestReviewCheckNotifier_ConcurrentAttempts_ResolveToOneIdentity(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewReviewCheckRunStore(pool)

	fake := newFakeCheckRunGitHub()
	server := fake.server()
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := outboxworker.NewReviewCheckNotifier(pool, store, adapter, "tok", 999)

	owner, repoName := "acme", fmt.Sprintf("race-repo-%d", time.Now().UnixNano())
	const prNumber = 7
	const headSHA = "cafef00d"

	older := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)

	attemptA := newTestAttempt(ctx, t, pool)
	attemptB := newTestAttempt(ctx, t, pool)

	payloadA, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prNumber, HeadSHA: headSHA,
		AttemptID: attemptA, AttemptCreatedAt: older,
		Phase: "queued",
	})
	if err != nil {
		t.Fatalf("marshal payload A: %v", err)
	}
	payloadB, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prNumber, HeadSHA: headSHA,
		AttemptID: attemptB, AttemptCreatedAt: newer,
		Phase: "running",
	})
	if err != nil {
		t.Fatalf("marshal payload B: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errsCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errsCh <- notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: payloadA})
	}()
	go func() {
		defer wg.Done()
		<-start
		errsCh <- notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: payloadB})
	}()
	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			t.Fatalf("concurrent Deliver() error = %v", err)
		}
	}

	row, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prNumber)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Phase != "running" {
		t.Errorf("row.Phase = %q, want running (attempt B, the newer one, must win regardless of race interleaving)", row.Phase)
	}
	if !row.AttemptID.Valid || row.AttemptID.String() != attemptB {
		t.Errorf("row.AttemptID = %v, want attempt B's id %s", row.AttemptID, attemptB)
	}
	if row.ExternalID == nil {
		t.Fatal("row.ExternalID is nil, want a real external check-run id recorded")
	}

	finalState := fake.stateFor(*row.ExternalID)
	if finalState == nil {
		t.Fatalf("no GitHub-side state recorded for external id %d", *row.ExternalID)
	}
	if finalState["status"] != "in_progress" {
		t.Errorf("final GitHub-side status for the winning check run = %v, want in_progress (attempt B's own output) -- a stale write from the LOSING attempt would leave this as queued instead",
			finalState["status"])
	}

	// This does NOT assert GitHub-side creates == 1: an empirical run of
	// this exact test (repeated with -count=20) found the OLDER attempt
	// can still reach its own CreateCheckRun call before the newer
	// attempt's claim commits (the row lock serializes the CLAIM step
	// only, never the GitHub call itself, which must run outside any
	// transaction -- ports.Notifier.Deliver's own contract). When that
	// happens the older attempt's own SetExternalID call loses its guard
	// (logged: "external id write lost the guard") and its own,
	// now-orphaned check run is never referenced again -- Postgres's own
	// claim row (the row/AttemptID/Phase/ExternalID assertions above)
	// still converges to exactly ONE identity regardless, which is the
	// invariant this system actually promises (§21.1b: "Two active
	// identities... worse than none" -- ACTIVE, i.e. ones this system
	// still treats as current). A GitHub-side orphan from the losing
	// attempt is the same accepted, bounded residual already documented
	// on SetReviewCheckRunExternalID's own generated doc comment -- named
	// here explicitly rather than asserted away, since asserting it away
	// would require holding a Postgres transaction across the GitHub
	// call, which this codebase's own Notifier.Deliver contract forbids.
}
