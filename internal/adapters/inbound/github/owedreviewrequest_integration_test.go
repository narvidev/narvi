//go:build integration

// Technical plan §24.9's owed human review requests end to end, on real
// Postgres: a person's review request reaches the real signed
// pull_request/labeled delivery through NewHandler and queues behind a
// running review; a push moves the pull request through the real signed
// synchronize delivery; the running review ends with a real
// execution_complete sent to the session's actor, whose dispatch finds the
// queued request's context moved; the owed_review_request timer reaches
// the actor through the real timer pump (Registry.PumpOnce), and the
// requester's authorization is asked again through the control plane's own
// role-based authorizer. The session's status is read through the real
// httpapi.GetSessionStatus route throughout.
package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactivity"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// movingPullRequest is the pull request as the code host answers it, for
// every reader at once: the label lane's pre-fetch (the handler's
// DiffFetcher), the actor's re-run fetch (ReviewDiffFetcher) and its
// context check (ReviewLiveReader). Its head and its diff move together
// when the test pushes.
type movingPullRequest struct {
	mu   sync.Mutex
	head string
	diff string
}

func (p *movingPullRequest) move(head, diff string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.head, p.diff = head, diff
}

func (p *movingPullRequest) GetPullRequest(context.Context, string, string, int32, string) (githubapi.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return githubapi.PullRequest{HeadSHA: p.head, BaseRef: "main"}, nil
}

func (p *movingPullRequest) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	return "base-tip", spec.Branch, nil
}

func (p *movingPullRequest) GetCompareDiff(context.Context, string, string, string, string, string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.diff, false, nil
}

func (p *movingPullRequest) GetPullRequestDiff(context.Context, string, string, int32, string) (string, bool, error) {
	return "", false, fmt.Errorf("movingPullRequest: GetPullRequestDiff is not read here")
}

func (p *movingPullRequest) GetOpenPR(_ context.Context, owner, repo string, number int, _ string) (ports.OpenPR, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ports.OpenPR{Owner: owner, Repo: repo, Number: number, HeadSHA: p.head, BaseRef: "main"}, true, nil
}

func (p *movingPullRequest) IsAncestor(context.Context, ports.IsAncestorSpec) (bool, error) {
	return true, nil
}

// owedLabelFixture is a review session of a pull request on a repository
// that has NOT opted into the automatic re-review, with a review running
// on a ready sandbox, a maintainer linked to the GitHub account that
// applies the re-review label, and one session registry -- the real
// handler's post-commit trigger and the test's own pump tick both reach
// it -- wired like the control plane: the pull request read through pr,
// the requester's authorization asked of httpapi's authorizer.
type owedLabelFixture struct {
	rig          testRig
	registry     *sessionactor.Registry
	status       http.Handler
	pr           *movingPullRequest
	sessionID    pgtype.UUID
	reviewID     pgtype.UUID
	repoFullName string
	cloneURL     string
	prNumber     int
	senderID     int64
	maintainer   sqlcgen.User
}

func newOwedLabelFixture(ctx context.Context, t *testing.T) owedLabelFixture {
	t.Helper()
	pool := newTestPool(t)
	f := owedLabelFixture{
		pr:           &movingPullRequest{head: "sha-under-review", diff: "+ a line the review is reading"},
		repoFullName: fmt.Sprintf("example/owed-label-%d", time.Now().UnixNano()),
		prNumber:     77,
		senderID:     time.Now().UnixNano() % 1_000_000_000,
	}
	f.cloneURL = "https://github.com/" + f.repoFullName + ".git"
	users, sessions, participants := narvipg.NewUserStore(pool), narvipg.NewSessionStore(pool), narvipg.NewParticipantStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false,
		sessionactor.RegistryOptions{
			ReviewDiffFetcher: f.pr, ReviewLiveReader: f.pr, GitHubBotHandle: testBotHandleIntegration,
			GitHubOutbound:          platform.MustNewGitHubOutboundConfig("test-token"),
			ReviewRequestAuthorizer: httpapi.NewReviewRequestAuthorizer(users),
		})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })
	f.registry = registry

	f.rig = testRig{
		pool: pool, turns: narvipg.NewTurnStore(pool), plans: narvipg.NewPlanStore(pool),
		users: users, identities: narvipg.NewIdentityStore(pool), linkNotices: narvipg.NewGitHubActorLinkNoticeStore(pool),
	}
	coalescer := &githubingress.SessionCoalescer{
		Pool: pool, PRSessions: narvipg.NewGitHubPRSessionStore(pool), Sessions: sessions, Turns: f.rig.turns,
		Environments: narvipg.NewEnvironmentStore(pool), Registry: registry, AuditLog: narvipg.NewAuditLogStore(pool),
		Plans: f.rig.plans, Identities: f.rig.identities, Users: users, Participants: participants,
	}
	handler, err := githubingress.NewHandler(coalescer, narvipg.NewWebhookDeliveryStore(pool), githubingress.Config{
		WebhookSecret: testWebhookSecret, BotHandle: testBotHandleIntegration,
		Outbound:    platform.MustNewGitHubOutboundConfig("test-bot-token"),
		LinkNotices: f.rig.linkNotices, ReReviewLabel: "run-review",
		DiffFetcher: f.pr, Timers: narvipg.NewTimerStore(pool), Timeouts: platform.DefaultTimeouts(),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", handler)
	f.rig.server = httptest.NewServer(mux)
	t.Cleanup(f.rig.server.Close)

	router := chi.NewRouter()
	router.Get("/api/sessions/{sessionID}/status", httpapi.GetSessionStatus(sessions, sessionactivity.NewWaiter(sessionactivity.ConfigFrom(platform.DefaultTimeouts())), platform.DefaultTimeouts()))
	f.status = router

	f.maintainer = createLinkedGitHubUser(ctx, t, users, f.rig.identities, f.senderID, sqlcgen.UserRoleMaintainer)
	sess, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create the review session: %v", err)
	}
	f.sessionID = sess.ID
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, f.repoFullName, int32(f.prNumber)); err != nil {
		t.Fatal(err)
	}
	if err := prSessions.SetSessionID(ctx, f.repoFullName, int32(f.prNumber), sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, f.repoFullName, true); err != nil {
		t.Fatal(err)
	}
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sess.ID); err != nil {
		t.Fatalf("create the sandbox: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready', gen = 1 WHERE session_id = $1`, sess.ID); err != nil {
		t.Fatal(err)
	}
	head := "sha-under-review"
	prompt := "review this pull request"
	review, err := f.rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt, ReviewHeadSha: &head, IsReviewAttempt: true})
	if err != nil {
		t.Fatalf("create the running review: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE turns SET created_at = now() - interval '1 minute' WHERE id = $1`, review.ID); err != nil {
		t.Fatal(err)
	}
	gen := int32(1)
	messageID := uuid.NewString()
	if _, err := f.rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: review.ID, Status: sqlcgen.TurnStatusProcessing,
		DispatchedAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
		DispatchedSandboxGen: &gen, DispatchedMessageID: &messageID,
	}); err != nil {
		t.Fatalf("start the running review: %v", err)
	}
	f.reviewID = review.ID
	return f
}

func (f owedLabelFixture) read(t *testing.T) restdtos.SessionActivity {
	t.Helper()
	rec := httptest.NewRecorder()
	f.status.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+f.sessionID.String()+"/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status: %d %s", rec.Code, rec.Body.String())
	}
	var got restdtos.SessionActivity
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// endReview ends the running review with a real execution_complete,
// delivered to the session's actor the way the sandbox socket delivers it.
func (f owedLabelFixture) endReview(ctx context.Context, t *testing.T) {
	t.Helper()
	messageID := uuid.NewString()
	raw, err := json.Marshal(sandboxws.ExecutionComplete{
		Type: "execution_complete", MessageId: messageID, SessionId: f.sessionID.String(), Gen: 1,
		AckId: "execution_complete:" + messageID, Outcome: sandboxws.ExecutionCompleteOutcomeCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.registry.GetOrSpawn(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	if err := a.Send(ctx, sessionactor.SandboxEvent{Type: "execution_complete", Gen: 1, MessageID: messageID, Raw: raw, Reply: reply}); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-reply:
		if !outcome.Persisted {
			t.Fatal("execution_complete not persisted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the execution_complete outcome")
	}
}

// waitFor polls cond until it holds or ten seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 10s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestOwedReviewRequest_NotOptedIn_AMovedHumanRequestReRunsForTheNewHead
// is the exit's third sentence (technical plan §24.9): on a repository
// that has NOT opted into the automatic re-review, a person's review
// request through the configured label queues behind a running review,
// recording its lane, its requester and its own text; a push moves the
// pull request while it waits; when the running review ends, the request
// does not start on the old head -- it ends context_moved and is owed,
// and the session reads scheduled, never settled -- and the next pump tick
// re-runs it, on the person's path, with a prompt built for the new head:
// its diff and the label's own sentence. No automatic review runs, and no
// budget slot is spent.
func TestOwedReviewRequest_NotOptedIn_AMovedHumanRequestReRunsForTheNewHead(t *testing.T) {
	ctx := context.Background()
	f := newOwedLabelFixture(ctx, t)

	labelBody := pullRequestLabeledBody(f.repoFullName, "owed-label", f.cloneURL, f.prNumber, "run-review", f.senderID, "owed-label-maintainer")
	if status := postWebhookEventType(t, f.rig, labelBody, "delivery-owed-label-"+f.sessionID.String(), "pull_request"); status != http.StatusOK {
		t.Fatalf("label delivery: %d, want 200", status)
	}
	var request sqlcgen.Turn
	waitFor(t, "the label's request queued", func() bool {
		turns, err := f.rig.turns.ListForSession(ctx, f.sessionID)
		if err != nil || len(turns) != 2 {
			return false
		}
		request = turns[1]
		return true
	})
	// The request is read as having waited when the review ended after it
	// was created -- the ending replica's clock against the database's --
	// so it is moved a few seconds back, after the review's own creation:
	// skew between the two clocks never decides the test.
	if _, err := f.rig.pool.Exec(ctx, `UPDATE turns SET created_at = created_at - interval '5 seconds' WHERE id = $1`, request.ID); err != nil {
		t.Fatal(err)
	}
	if request.Status != sqlcgen.TurnStatusPending || !request.IsReviewAttempt || request.ReviewHeadSha == nil || *request.ReviewHeadSha != "sha-under-review" ||
		request.RequestTrigger == nil || *request.RequestTrigger != "label" || request.RequestedBy != f.maintainer.ID ||
		request.RequestText == nil || *request.RequestText != "Manual re-review requested via the configured GitHub label." {
		t.Fatalf("the label's request = %+v, want a pending review attempt of the head under review, recording the label, the maintainer and its sentence", request)
	}

	const pushed = "sha-pushed-while-queued"
	const pushedDiff = "+ a line the push added while the request waited"
	if status := postWebhookEventType(t, f.rig, pullRequestSynchronizeBody(f.repoFullName, f.prNumber, pushed), "delivery-owed-push-"+f.sessionID.String(), "pull_request"); status != http.StatusOK {
		t.Fatalf("synchronize delivery: %d, want 200", status)
	}
	f.pr.move(pushed, pushedDiff)
	if got := f.read(t); got.Activity != restdtos.SessionActivityActivityRunning || got.Settled {
		t.Fatalf("while the review runs: activity %q settled %v, want running", got.Activity, got.Settled)
	}

	f.endReview(ctx, t)
	waitFor(t, "the queued request ended", func() bool {
		row, err := f.rig.turns.Get(ctx, request.ID)
		return err == nil && row.Status != sqlcgen.TurnStatusPending
	})
	ended, err := f.rig.turns.Get(ctx, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ended.Status != sqlcgen.TurnStatusFailed || ended.EndReason == nil || *ended.EndReason != "context_moved" || ended.DispatchedAt.Valid {
		t.Fatalf("the moved request: status %s end reason %v dispatched %v; want it ended context_moved without starting", ended.Status, ended.EndReason, ended.DispatchedAt.Valid)
	}
	var owed int
	if err := f.rig.pool.QueryRow(ctx, `SELECT count(*) FROM owed_review_requests WHERE session_id = $1 AND moved_turn_id = $2 AND trigger = 'label' AND requested_by = $3 AND context_moves = 1`,
		f.sessionID, request.ID, f.maintainer.ID).Scan(&owed); err != nil || owed != 1 {
		t.Fatalf("owed requests naming the moved one = %d (err %v), want 1", owed, err)
	}
	if got := f.read(t); got.Activity != restdtos.SessionActivityActivityScheduled || got.Settled {
		t.Fatalf("while the request is owed: activity %q settled %v, want scheduled", got.Activity, got.Settled)
	}

	waitFor(t, "the owed request re-run", func() bool {
		if err := f.registry.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
		var n int
		if err := f.rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1 AND review_head_sha = $2`, f.sessionID, pushed).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	})
	turns, err := f.rig.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	rerun := turns[len(turns)-1]
	if rerun.Status != sqlcgen.TurnStatusPending || !rerun.IsReviewAttempt || rerun.RequestTrigger == nil || *rerun.RequestTrigger != "label" ||
		rerun.RequestedBy != f.maintainer.ID || rerun.ContextMoves == nil || *rerun.ContextMoves != 1 {
		t.Fatalf("the re-run = %+v, want a pending review attempt of the label's request by the maintainer, one move carried", rerun)
	}
	if rerun.Prompt == nil || !strings.Contains(*rerun.Prompt, pushedDiff) || !strings.Contains(*rerun.Prompt, "Manual re-review requested via the configured GitHub label.") ||
		strings.Contains(*rerun.Prompt, "a line the review is reading") {
		t.Fatalf("the re-run's prompt = %v, want the label's sentence and the new head's diff, never the old head's", rerun.Prompt)
	}
	waitFor(t, "the owed timer gone", func() bool {
		var n int
		if err := f.rig.pool.QueryRow(ctx, `SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = $2`, f.sessionID, sessionactor.TimerOwedReviewRequest).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 0
	})
	var left, automatic int
	var budget int32
	if err := f.rig.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM owed_review_requests WHERE session_id = $1),
		(SELECT count(*) FROM turns WHERE session_id = $1 AND request_trigger = 'auto'),
		(SELECT auto_retrigger_count FROM github_pr_sessions WHERE session_id = $1)`, f.sessionID).Scan(&left, &automatic, &budget); err != nil {
		t.Fatal(err)
	}
	if left != 0 || automatic != 0 || budget != 0 {
		t.Fatalf("after the re-run: owed %d, automatic reviews %d, budget spent %d; want none of each", left, automatic, budget)
	}
	if got := f.read(t); got.Activity != restdtos.SessionActivityActivityQueued || got.Settled {
		t.Fatalf("once re-run: activity %q settled %v, want queued", got.Activity, got.Settled)
	}
}
