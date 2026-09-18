//go:build integration

// Integration tests proving the review-check publisher's own identity
// rules (§21.1b) against a REAL Postgres instance -- the SAME
// shared testcontainers pool every other *_integration_test.go file in
// this package uses (builder_integration_test.go's own TestMain/
// newTestPool). A fake GitHub server (httptest) stands in for the real
// Checks API, with HONEST list semantics (finding A9): CreateCheckRun/
// UpdateCheckRun mutate real per-id state (name/head_sha/app_id/status/
// conclusion), and ListCheckRunsForRef reports exactly that state back,
// filtered by ref -- never an unconditionally empty page. Before this
// fix, the fake's own GET handler always answered `{"total_count": 0}`
// regardless of what had been created, which meant the adoption/recovery
// branch (resolveOrCreateCheckRun's own "select by SHA and GitHub App"
// path) -- where BOTH the App-id (finding A2) and status (finding A1)
// identity rules actually live -- was never exercised by this suite at
// all.
package outboxworker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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

// fakeCheckRun is one check run's own durable state inside
// fakeCheckRunGitHub -- the fields real GitHub's Checks API would report
// back on a create/update/list call.
type fakeCheckRun struct {
	Name       string
	HeadSHA    string
	AppID      int64
	Status     string
	Conclusion string
}

// fakeCheckRunGitHub is an in-memory stand-in for GitHub's real Checks
// API with HONEST semantics (finding A9): CreateCheckRun assigns
// incrementing ids and records name/head_sha/status/conclusion, all
// attributed to appID (this fake's own stand-in for "the App this
// credential's writes are attributed to"); UpdateCheckRun mutates the
// SAME per-id record's status/conclusion (never name/head_sha, mirroring
// real GitHub); ListCheckRunsForRef reports every run whose head_sha
// matches the requested ref, exactly as currently recorded -- never an
// unconditionally empty page.
type fakeCheckRunGitHub struct {
	mu      sync.Mutex
	nextID  int64
	runs    map[int64]*fakeCheckRun
	creates int32
	updates int32
	// appID is the App id this fake attributes every CreateCheckRun to
	// (real GitHub's own create-response app.id) -- defaults to a fixed,
	// nonzero value (newFakeCheckRunGitHub) so a notifier under test can
	// self-learn a real, consistent value across calls, the same way a
	// real, stable bot-token identity would behave.
	appID int64
	// onPatch, when set, is invoked (with the fake's own lock NOT held)
	// immediately before this fake responds to a PATCH for the named
	// check-run id -- the sole purpose is finding A3's own test, which
	// needs to inject "a concurrently-racing, faster Deliver call already
	// committed a newer row to Postgres" at the exact moment this call's
	// own network round trip is in flight.
	onPatch func(id int64)
	// onCreate, when set, is invoked (with the fake's own lock NOT held,
	// before this fake assigns an id or responds) on every POST -- finding
	// B1's own reproduction: it lets a test run an ENTIRELY SEPARATE,
	// genuinely newer Deliver call to completion (claim, clear, create its
	// OWN check run, record its OWN external id) nested inside the exact
	// window a slower, older call's own CreateCheckRun is still in flight
	// -- no goroutines or real concurrency needed, since Postgres's own
	// claim-row transaction for the older call already committed and
	// released its lock BEFORE this HTTP call was ever made (Deliver's own
	// "claim, then release, then call GitHub" sequencing), so a nested,
	// synchronous call from inside this hook reaches Postgres exactly as
	// a genuinely concurrent second process would.
	onCreate func()
}

func newFakeCheckRunGitHub() *fakeCheckRunGitHub {
	return &fakeCheckRunGitHub{runs: map[int64]*fakeCheckRun{}, appID: 555}
}

// seedRun directly inserts a check run into this fake's own state, as if
// some OTHER credential/process had created it -- used to simulate
// "another GitHub App's same-named check run already exists at this
// head sha" (finding A2's own adversarial case) without going through
// this fake's own CreateCheckRun path (which would attribute it to
// f.appID, defeating the point).
func (f *fakeCheckRunGitHub) seedRun(id int64, name, headSHA string, appID int64, status, conclusion string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[id] = &fakeCheckRun{Name: name, HeadSHA: headSHA, AppID: appID, Status: status, Conclusion: conclusion}
	if id > f.nextID {
		f.nextID = id
	}
}

func (f *fakeCheckRunGitHub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		switch {
		case r.Method == http.MethodPost:
			if f.onCreate != nil {
				f.onCreate()
			}
			f.mu.Lock()
			f.nextID++
			id := f.nextID
			name, _ := body["name"].(string)
			headSHA, _ := body["head_sha"].(string)
			status, _ := body["status"].(string)
			conclusion, _ := body["conclusion"].(string)
			f.runs[id] = &fakeCheckRun{Name: name, HeadSHA: headSHA, AppID: f.appID, Status: status, Conclusion: conclusion}
			f.creates++
			appID := f.appID
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "app": map[string]any{"id": appID}})
		case r.Method == http.MethodPatch:
			id := parseTrailingID(r.URL.Path)
			if f.onPatch != nil {
				f.onPatch(id)
			}
			f.mu.Lock()
			if run, ok := f.runs[id]; ok {
				if status, present := body["status"].(string); present {
					run.Status = status
				}
				// A real "conclusion" field is either a real string or
				// absent entirely (checkRunRequest's own omitempty doc
				// comment, githubapi/checkruns.go) -- an incomplete
				// status legitimately clears any PRIOR conclusion.
				if conclusion, present := body["conclusion"].(string); present {
					run.Conclusion = conclusion
				} else {
					run.Conclusion = ""
				}
			}
			f.updates++
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case r.Method == http.MethodGet:
			ref := refFromListPath(r.URL.Path)
			f.mu.Lock()
			type entry struct {
				ID   int64
				Run  fakeCheckRun
				Sort int64
			}
			var matched []entry
			for id, run := range f.runs {
				if run.HeadSHA == ref {
					matched = append(matched, entry{ID: id, Run: *run, Sort: id})
				}
			}
			f.mu.Unlock()
			checkRuns := make([]map[string]any, 0, len(matched))
			for _, e := range matched {
				checkRuns = append(checkRuns, map[string]any{
					"id": e.ID, "name": e.Run.Name, "head_sha": e.Run.HeadSHA,
					"status": e.Run.Status, "conclusion": e.Run.Conclusion,
					"app": map[string]any{"id": e.Run.AppID},
				})
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(checkRuns), "check_runs": checkRuns})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// refFromListPath extracts the commit ref from
// "/repos/{owner}/{repo}/commits/{ref}/check-runs" -- the exact path
// shape githubapi.ListCheckRunsForRef builds.
func refFromListPath(path string) string {
	const marker = "/commits/"
	i := strings.Index(path, marker)
	if i < 0 {
		return ""
	}
	rest := path[i+len(marker):]
	if j := strings.Index(rest, "/check-runs"); j >= 0 {
		return rest[:j]
	}
	return rest
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
	run, ok := f.runs[id]
	if !ok {
		return nil
	}
	return map[string]any{"status": run.Status, "conclusion": run.Conclusion, "name": run.Name, "head_sha": run.HeadSHA, "app_id": run.AppID}
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
	notifier := outboxworker.NewReviewCheckNotifier(pool, store, adapter, "tok")

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
	notifier := outboxworker.NewReviewCheckNotifier(pool, store, adapter, "tok")

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

// TestReviewCheckNotifier_TerminalRestart_NeverReopensConcludedRun is
// finding A1's own reproduction, fixed: a review restarting after a
// terminal result must open a NEW check identity, never PATCH the
// already-concluded one back open. Before the fix, resolveOrCreateCheckRun's
// own adoption predicate had no status field to exclude a concluded run
// with (CheckRunSummary carried none), so it re-adopted the very run
// newIdentityNeeded had just decided must NOT be reused -- see this
// file's own report for the exact before/after reproduction this test
// pins.
func TestReviewCheckNotifier_TerminalRestart_NeverReopensConcludedRun(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewReviewCheckRunStore(pool)

	fake := newFakeCheckRunGitHub()
	server := fake.server()
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := outboxworker.NewReviewCheckNotifier(pool, store, adapter, "tok")

	owner, repoName := "acme", fmt.Sprintf("restart-repo-%d", time.Now().UnixNano())
	const prNumber = 101
	const headSHA = "0ddba11"

	firstAttempt := newTestAttempt(ctx, t, pool)
	firstCreated := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	terminalPayload, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prNumber, HeadSHA: headSHA,
		AttemptID: firstAttempt, AttemptCreatedAt: firstCreated,
		Phase: "terminal_assessed",
	})
	if err != nil {
		t.Fatalf("marshal terminal payload: %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: terminalPayload}); err != nil {
		t.Fatalf("Deliver(first attempt terminal) error = %v", err)
	}
	creates1, _ := fake.counts()
	if creates1 != 1 {
		t.Fatalf("creates after first attempt = %d, want 1", creates1)
	}
	row1, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prNumber)
	if err != nil {
		t.Fatalf("get row after first attempt: %v", err)
	}
	if row1.ExternalID == nil {
		t.Fatal("row.ExternalID is nil after first attempt, want a real external id")
	}
	firstRunID := *row1.ExternalID
	firstState := fake.stateFor(firstRunID)
	if firstState["status"] != "completed" || firstState["conclusion"] != "success" {
		t.Fatalf("first run state = %+v, want completed/success", firstState)
	}

	// A review restart: a genuinely NEWER attempt, same head sha.
	secondAttempt := newTestAttempt(ctx, t, pool)
	secondCreated := firstCreated.Add(time.Hour)
	runningPayload, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prNumber, HeadSHA: headSHA,
		AttemptID: secondAttempt, AttemptCreatedAt: secondCreated,
		Phase: "running",
	})
	if err != nil {
		t.Fatalf("marshal running payload: %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: runningPayload}); err != nil {
		t.Fatalf("Deliver(second attempt running) error = %v", err)
	}

	creates2, _ := fake.counts()
	if creates2 != 2 {
		t.Fatalf("creates after the restart = %d, want 2 (a NEW check identity, never a reopened concluded one)", creates2)
	}

	row2, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prNumber)
	if err != nil {
		t.Fatalf("get row after restart: %v", err)
	}
	if row2.ExternalID == nil {
		t.Fatal("row.ExternalID is nil after restart, want a real external id")
	}
	if *row2.ExternalID == firstRunID {
		t.Fatalf("row.ExternalID after restart = %d, SAME as the first (already-concluded) run %d -- the concluded run was reopened instead of a fresh one created", *row2.ExternalID, firstRunID)
	}

	// The FIRST run must be untouched by the restart -- still concluded,
	// still success.
	firstStateAfter := fake.stateFor(firstRunID)
	if firstStateAfter["status"] != "completed" || firstStateAfter["conclusion"] != "success" {
		t.Errorf("first run state after restart = %+v, want UNCHANGED completed/success -- it must never be reopened", firstStateAfter)
	}

	// The SECOND (new) run reflects the restart's own running output.
	secondState := fake.stateFor(*row2.ExternalID)
	if secondState["status"] != "in_progress" {
		t.Errorf("second run state = %+v, want in_progress", secondState)
	}
}

// TestReviewCheckNotifier_Recovery_AdoptsOwnInFlightRun is finding A5's
// own missing coverage for the adoption/recovery branch's HAPPY path,
// through a COLD second notifier (finding B2's own fix, and the gap the
// prior version of this test could not see): a crash between a
// successful CreateCheckRun and this system's own local record of
// external_id (SetReviewCheckRunExternalID's own doc comment) must
// recover the REAL check run rather than creating a duplicate -- and the
// realistic shape of that crash is a PROCESS RESTART, which is exactly
// what a second, brand-new *reviewCheckNotifier -- sharing nothing but
// the same pool/store/adapter/bot token, never the first notifier's own
// in-process writerAppID cache -- reproduces. Before finding B2's fix,
// this test used the SAME warm notifier instance for both deliveries:
// the second call's own observedWriterAppID() read straight from the
// FIRST call's already-learned in-process cache, so it could never
// distinguish "this process learned its own app id" from "some process,
// at some point, did" -- the exact distinction finding B2 is about. A
// genuinely cold notifier can only recover via
// ReviewCheckRunStore.GetWriterAppID's own durable row.
func TestReviewCheckNotifier_Recovery_AdoptsOwnInFlightRun(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewReviewCheckRunStore(pool)

	fake := newFakeCheckRunGitHub()
	server := fake.server()
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	warmNotifier := outboxworker.NewReviewCheckNotifier(pool, store, adapter, "tok")

	owner, repoName := "acme", fmt.Sprintf("recovery-repo-%d", time.Now().UnixNano())
	const prNumber = 55
	const headSHA = "f00dcafe"

	attempt := newTestAttempt(ctx, t, pool)
	created := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	runningPayload, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prNumber, HeadSHA: headSHA,
		AttemptID: attempt, AttemptCreatedAt: created,
		Phase: "running",
	})
	if err != nil {
		t.Fatalf("marshal running payload: %v", err)
	}
	// First delivery, on the WARM notifier: a genuine create, and how
	// this notifier learns its own writer App id (writerAppID's own doc
	// comment) -- ALSO how it durably persists that id (finding B2).
	if err := warmNotifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: runningPayload}); err != nil {
		t.Fatalf("Deliver(running) error = %v", err)
	}
	creates1, _ := fake.counts()
	if creates1 != 1 {
		t.Fatalf("creates after first delivery = %d, want 1", creates1)
	}
	row, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prNumber)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.ExternalID == nil {
		t.Fatal("row.ExternalID is nil, want a real external id")
	}
	runID := *row.ExternalID

	// Simulate "the local record of external_id was lost" -- e.g. this
	// process's own SetExternalID write failed/never landed after a
	// genuinely successful GitHub create (SetReviewCheckRunExternalID's
	// own doc comment on this exact residual) -- while the check run
	// genuinely exists on GitHub, at the SAME head sha, under THIS
	// notifier's own already-learned App id.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin clear-external-id tx: %v", err)
	}
	if err := store.WithTx(tx).ClearExternalID(ctx, owner+"/"+repoName, prNumber); err != nil {
		t.Fatalf("clear external id: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit clear-external-id tx: %v", err)
	}

	// A second delivery for the SAME attempt/head sha, same phase
	// (mirrors an outbox redelivery after a PROCESS RESTART, following
	// the "lost" write above) -- on a BRAND-NEW notifier that has never
	// itself observed a writer app id: resolveOrCreateCheckRun must
	// still ADOPT the existing, still-in-progress run rather than create
	// a second one, recovering the durably-persisted app id finding B2
	// gives it, never guessing and never falling back to "create".
	coldNotifier := outboxworker.NewReviewCheckNotifier(pool, store, adapter, "tok")
	if err := coldNotifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: runningPayload}); err != nil {
		t.Fatalf("Deliver(running, redelivery on a COLD notifier) error = %v", err)
	}

	creates2, updates2 := fake.counts()
	if creates2 != 1 {
		t.Fatalf("creates after recovery = %d, want STILL 1 -- the existing in-flight run must be adopted, never duplicated, even by a notifier that never itself created it", creates2)
	}
	if updates2 < 1 {
		t.Fatalf("updates after recovery = %d, want at least 1 (the adoption PATCH)", updates2)
	}

	rowAfter, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prNumber)
	if err != nil {
		t.Fatalf("get row after recovery: %v", err)
	}
	if rowAfter.ExternalID == nil || *rowAfter.ExternalID != runID {
		t.Errorf("row.ExternalID after recovery = %v, want the SAME original run id %d re-adopted", rowAfter.ExternalID, runID)
	}
}

// TestReviewCheckNotifier_Recovery_NeverAdoptsAnotherAppsRun is finding
// A2/A5's own missing coverage for the adoption/recovery branch's OTHER
// half: a DIFFERENT app's same-named, same-head-sha check run must never
// be adopted, even though it shares reviewcheck.CheckName and the exact
// head sha -- only a matching App id makes a candidate adoptable.
func TestReviewCheckNotifier_Recovery_NeverAdoptsAnotherAppsRun(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewReviewCheckRunStore(pool)

	fake := newFakeCheckRunGitHub()
	server := fake.server()
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := outboxworker.NewReviewCheckNotifier(pool, store, adapter, "tok")

	owner, repoName := "acme", fmt.Sprintf("otherapp-repo-%d", time.Now().UnixNano())
	const prNumber = 88
	const headSHA = "beeff00d"

	// First, teach this notifier its OWN writer App id via a genuine
	// create on an unrelated PR -- mirrors any earlier delivery this
	// process would ordinarily have already made.
	warmupAttempt := newTestAttempt(ctx, t, pool)
	warmupPayload, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prNumber + 1, HeadSHA: "warmupsha",
		AttemptID: warmupAttempt, AttemptCreatedAt: time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC),
		Phase: "running",
	})
	if err != nil {
		t.Fatalf("marshal warmup payload: %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: warmupPayload}); err != nil {
		t.Fatalf("Deliver(warmup) error = %v", err)
	}

	// Seed a DIFFERENT app's own check run at the target head sha --
	// same name, same head sha, DIFFERENT app id, deliberately
	// non-concluded (in_progress) so a status-only guard could not, by
	// itself, explain refusing to adopt it: only the App-id mismatch can.
	const otherAppID = 999999
	fake.seedRun(1000, "narvi/review", headSHA, otherAppID, "in_progress", "")

	attempt := newTestAttempt(ctx, t, pool)
	payload, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prNumber, HeadSHA: headSHA,
		AttemptID: attempt, AttemptCreatedAt: time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC),
		Phase: "running",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}

	row, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prNumber)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.ExternalID == nil {
		t.Fatal("row.ExternalID is nil, want a real external id")
	}
	if *row.ExternalID == 1000 {
		t.Fatalf("row.ExternalID = 1000, the OTHER app's own run was adopted -- the App-id guard failed to exclude it")
	}
	otherAppState := fake.stateFor(1000)
	if otherAppState["status"] != "in_progress" || otherAppState["conclusion"] != "" {
		t.Errorf("the other app's own run was mutated: %+v, want untouched (in_progress, no conclusion)", otherAppState)
	}
}

// TestReviewCheckNotifier_SelfHealsWhenSupersededDuringGitHubCall is
// finding A3's own reproduction: a slower Deliver call's own GitHub
// write can land AFTER a concurrently-racing, genuinely newer attempt's
// own (faster) write already completed -- even though Postgres's own
// claim row already, correctly, reflects the newer attempt by the time
// EITHER network call returns. Without a supersession re-check spanning
// the network call, the slower call's own now-stale PATCH is simply the
// last word GitHub sees. This test injects exactly that ordering via the
// fake server's own onPatch hook, which writes the "newer" row directly
// (mirroring what a REAL concurrently-racing Deliver call would have
// already committed) at the exact moment the slower call's own PATCH
// would otherwise be the final word.
func TestReviewCheckNotifier_SelfHealsWhenSupersededDuringGitHubCall(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewReviewCheckRunStore(pool)

	fake := newFakeCheckRunGitHub()
	server := fake.server()
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := outboxworker.NewReviewCheckNotifier(pool, store, adapter, "tok")

	owner, repoName := "acme", fmt.Sprintf("race3-repo-%d", time.Now().UnixNano())
	const prNumber = 13
	const headSHA = "5ca1ab1e"

	attemptA := newTestAttempt(ctx, t, pool)
	attemptB := newTestAttempt(ctx, t, pool)
	older := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	// attempt A reaches "running" first -- creates the one check run
	// both attempts will share.
	runningPayload, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prNumber, HeadSHA: headSHA,
		AttemptID: attemptA, AttemptCreatedAt: older,
		Phase: "running",
	})
	if err != nil {
		t.Fatalf("marshal running payload: %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: runningPayload}); err != nil {
		t.Fatalf("Deliver(A running) error = %v", err)
	}
	row, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prNumber)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.ExternalID == nil {
		t.Fatal("row.ExternalID is nil, want a real external id")
	}
	runID := *row.ExternalID

	// Arm the race: the NEXT PATCH to runID directly commits attempt B's
	// own (newer, terminal_assessed) row -- simulating a concurrently
	// racing Deliver(B) call that already won its own claim AND already
	// completed its own GitHub write, entirely between attempt A's own
	// claim commit and attempt A's own network call finishing.
	var raced sync.Once
	fake.onPatch = func(id int64) {
		if id != runID {
			return
		}
		raced.Do(func() {
			var attemptBUUID pgtype.UUID
			if scanErr := attemptBUUID.Scan(attemptB); scanErr != nil {
				t.Errorf("scan attempt B uuid: %v", scanErr)
				return
			}
			raceTx, beginErr := pool.Begin(context.Background())
			if beginErr != nil {
				t.Errorf("begin race tx: %v", beginErr)
				return
			}
			defer func() { _ = raceTx.Rollback(context.Background()) }()
			raceStore := store.WithTx(raceTx)
			if ensureErr := raceStore.EnsureRow(context.Background(), owner+"/"+repoName, prNumber); ensureErr != nil {
				t.Errorf("race ensure row: %v", ensureErr)
				return
			}
			if _, lockErr := raceStore.LockForUpdate(context.Background(), owner+"/"+repoName, prNumber); lockErr != nil {
				t.Errorf("race lock row: %v", lockErr)
				return
			}
			if _, updErr := raceStore.UpdatePublished(context.Background(), owner+"/"+repoName, prNumber, headSHA, attemptBUUID, pgtype.Timestamptz{Time: newer, Valid: true}, "terminal_assessed", nil, nil, 0); updErr != nil {
				t.Errorf("race update published: %v", updErr)
				return
			}
			if commitErr := raceTx.Commit(context.Background()); commitErr != nil {
				t.Errorf("commit race tx: %v", commitErr)
			}
		})
	}

	// attempt A progresses to terminal_not_assessed -- SAME attempt, so
	// this passes Supersedes at claim time (rank 1 -> rank 2) and commits
	// BEFORE the race above fires; the race then overwrites the row with
	// attempt B's own data WHILE this call's own UpdateCheckRun PATCH is
	// in flight.
	notAssessedPayload, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prNumber, HeadSHA: headSHA,
		AttemptID: attemptA, AttemptCreatedAt: older,
		Phase: "terminal_not_assessed",
	})
	if err != nil {
		t.Fatalf("marshal not-assessed payload: %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: notAssessedPayload}); err != nil {
		t.Fatalf("Deliver(A terminal_not_assessed) error = %v", err)
	}

	finalState := fake.stateFor(runID)
	if finalState["status"] != "completed" || finalState["conclusion"] != "success" {
		t.Fatalf("final GitHub-side state = %+v, want completed/success (attempt B's own, genuinely newer truth) -- attempt A's own stale terminal_not_assessed/action_required write must have been self-corrected, not left standing",
			finalState)
	}

	// Postgres's own row was already attempt B's own truth (the race
	// itself wrote it) -- unaffected by this call's own self-heal, which
	// only ever touches GitHub.
	rowAfter, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prNumber)
	if err != nil {
		t.Fatalf("get row after: %v", err)
	}
	if rowAfter.Phase != "terminal_assessed" || !rowAfter.AttemptID.Valid || rowAfter.AttemptID.String() != attemptB {
		t.Errorf("row after = phase %q attempt %v, want terminal_assessed/%s", rowAfter.Phase, rowAfter.AttemptID, attemptB)
	}
}

// TestReviewCheckNotifier_SelfHealNeverReopensARunANewerIdentityAbandoned
// is finding B1's own reproduction: guardAgainstSupersessionDuringCall's
// self-heal (finding A3, above) used to PATCH externalID -- the run THIS
// call published to -- onto the row's current truth whenever attempt or
// phase had moved on for the SAME head sha, without ever checking that
// row.ExternalID still NAMED externalID. When the row moved on because
// newIdentityNeeded (Deliver) opened a FRESH check run for a genuinely
// newer attempt at the SAME head sha -- exactly the shape finding A1's
// own fix (resolveOrCreateCheckRun) already refuses to ADOPT -- the old
// external id belongs to an already-concluded run this row no longer
// claims, and the self-heal reopened it anyway: completed/success back
// to in_progress, conclusion dropped entirely (checkRunRequest's own
// omitempty behavior clearing it when the correction carries none).
//
// Reproduced deterministically, no goroutines: attempt A's own terminal,
// FIRST-EVER create for this PR is slow (its own POST is still being
// handled by the fake); attempt B -- a genuinely NEWER attempt, same head
// sha -- races entirely INSIDE that window via the fake's own onCreate
// hook, nested and synchronous (Deliver's own claim-then-release-then-
// call sequencing means Postgres's row lock for attempt A's claim is
// already released by the time its own HTTP POST is in flight, so a
// nested call reaches Postgres exactly as a genuinely concurrent second
// process would): attempt B claims the row (already terminal-shaped,
// genuinely newer attempt -- newIdentityNeeded fires), creates its OWN
// check run, and records its OWN external id -- all before attempt A's
// own POST response is even returned to it.
func TestReviewCheckNotifier_SelfHealNeverReopensARunANewerIdentityAbandoned(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewReviewCheckRunStore(pool)

	fake := newFakeCheckRunGitHub()
	server := fake.server()
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := outboxworker.NewReviewCheckNotifier(pool, store, adapter, "tok")

	owner, repoName := "acme", fmt.Sprintf("b1-repo-%d", time.Now().UnixNano())
	const prNumber = 21
	const headSHA = "cafef00d"

	attemptA := newTestAttempt(ctx, t, pool)
	attemptB := newTestAttempt(ctx, t, pool)
	older := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	// A plain bool guard, NOT sync.Once: attempt B's own nested Deliver
	// call (below) reaches this SAME onCreate hook again for its OWN
	// CreateCheckRun POST -- sync.Once.Do is not reentrant, and calling it
	// again from inside its own function on the same Once deadlocks. A
	// CompareAndSwap makes the re-entrant call a harmless no-op instead.
	var fired atomic.Bool
	var runBID int64
	fake.onCreate = func() {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		runningPayload, marshalErr := json.Marshal(ports.ReviewCheckPayload{
			Owner: owner, Repo: repoName, PRNumber: prNumber, HeadSHA: headSHA,
			AttemptID: attemptB, AttemptCreatedAt: newer,
			Phase: "running",
		})
		if marshalErr != nil {
			t.Errorf("marshal attempt B payload: %v", marshalErr)
			return
		}
		if delivErr := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: runningPayload}); delivErr != nil {
			t.Errorf("Deliver(B running, nested inside A's own create) error = %v", delivErr)
			return
		}
		row, getErr := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prNumber)
		if getErr != nil {
			t.Errorf("get row after nested B delivery: %v", getErr)
			return
		}
		if row.ExternalID == nil {
			t.Error("row.ExternalID is nil after nested B delivery, want attempt B's own real external id")
			return
		}
		runBID = *row.ExternalID
	}

	// attempt A: the FIRST-EVER emission for this PR, straight to
	// terminal_assessed -- its own CreateCheckRun call is where attempt
	// B's own full Deliver race (above) is nested.
	terminalPayload, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prNumber, HeadSHA: headSHA,
		AttemptID: attemptA, AttemptCreatedAt: older,
		Phase: "terminal_assessed",
	})
	if err != nil {
		t.Fatalf("marshal attempt A payload: %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: terminalPayload}); err != nil {
		t.Fatalf("Deliver(A terminal_assessed) error = %v", err)
	}

	creates, _ := fake.counts()
	if creates != 2 {
		t.Fatalf("creates = %d, want 2 (attempt A's own run, and attempt B's own run created inside the race)", creates)
	}
	if runBID == 0 {
		t.Fatal("runBID was never recorded -- the nested race inside onCreate did not run")
	}

	// The row (Postgres's own truth) must reflect attempt B -- the
	// genuinely newer attempt -- never attempt A, which lost the claim
	// race entirely (Supersedes: B's own AttemptCreatedAt is newer).
	rowAfter, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prNumber)
	if err != nil {
		t.Fatalf("get row after: %v", err)
	}
	if rowAfter.Phase != "running" || !rowAfter.AttemptID.Valid || rowAfter.AttemptID.String() != attemptB {
		t.Fatalf("row after = phase %q attempt %v, want running/%s (attempt B)", rowAfter.Phase, rowAfter.AttemptID, attemptB)
	}
	if rowAfter.ExternalID == nil || *rowAfter.ExternalID != runBID {
		t.Fatalf("row.ExternalID after = %v, want %d (attempt B's own run)", rowAfter.ExternalID, runBID)
	}

	// attempt A's own run -- abandoned when B's genuinely newer attempt
	// claimed the row -- must be left exactly as attempt A's own create
	// call originally published it: completed/success. The self-heal
	// (guardAgainstSupersessionDuringCall, triggered by attempt A's own
	// create returning into a row that has since moved to B) must NEVER
	// reopen it -- that is finding B1's own defect, fixed.
	//
	// Exactly two ids were ever assigned (creates == 2, asserted above),
	// starting from 1 -- attempt A's own is simply "the other one" of
	// {1, 2}, whichever B's own row didn't claim.
	var runAID int64
	for id := int64(1); id <= int64(creates); id++ {
		if id != runBID {
			runAID = id
			break
		}
	}
	if runAID == 0 {
		t.Fatal("could not determine attempt A's own run id")
	}
	stateA := fake.stateFor(runAID)
	if stateA == nil {
		t.Fatalf("attempt A's own run (id %d) not found in the fake's own state", runAID)
	}
	if stateA["status"] != "completed" || stateA["conclusion"] != "success" {
		t.Fatalf("attempt A's own run (id %d) = %+v, want UNCHANGED completed/success -- the self-heal must never reopen a run a newer identity has already abandoned this row for", runAID, stateA)
	}
}
