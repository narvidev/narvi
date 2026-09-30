//go:build integration

// A pull request's review session never pushes, seen from the session's
// status, on real Postgres, end to end. Its repos[].branch is the pull
// request's head ref, which it only reads: when its turn completes, the
// actor sends no push command and starts no delivery, so the status reads
// finished and settled at once, with nothing armed -- there is no push of
// its own for the code host to echo back as pull_request/synchronize. A
// push that does move the head is someone else's, and its synchronize arms
// §24's automatic re-review exactly as on any pull request: scheduled, then
// queued once the fire inserts the review turn. Everything is real but the
// code host and the sandbox: the session actor handles execution_complete,
// the signed webhook goes through NewHandler, the debounce fires through
// the real timer pump, and the status is read through the real
// httpapi.GetSessionStatus handler.
package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	"github.com/narvidev/narvi/internal/app/sessionactivity"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

var reviewNoPushTokenKey = []byte("0123456789abcdef0123456789abcdef")

// reviewNoPushCommander records the commands the actor sends the sandbox.
type reviewNoPushCommander struct {
	mu       sync.Mutex
	payloads []json.RawMessage
}

func (c *reviewNoPushCommander) SendCommand(_ string, payload json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, payload)
	return nil
}

func (c *reviewNoPushCommander) pushes(t *testing.T) []sandboxws.Push {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []sandboxws.Push
	for _, raw := range c.payloads {
		var probe struct{ Type string }
		if err := json.Unmarshal(raw, &probe); err != nil || probe.Type != "push" {
			continue
		}
		var p sandboxws.Push
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode push command: %v", err)
		}
		out = append(out, p)
	}
	return out
}

const (
	// reviewNoPushHeadBefore is the pull request's head when the
	// processing turn was created -- the head that turn reviewed.
	reviewNoPushHeadBefore = "1111111111111111111111111111111111111111"
	// reviewNoPushHeadAfter is the head after a push by someone else.
	reviewNoPushHeadAfter = "2222222222222222222222222222222222222222"
)

// reviewNoPushFixture is one review session of pull request #7 on its own
// repository, opted in to the automatic re-review, created by a member with
// a GitHub token, whose repos[].branch is the pull request's head ref, with
// live egress on and one processing turn created against
// reviewNoPushHeadBefore -- every condition under which a session that did
// push would push -- and a real registry whose commander records commands
// and whose review fetcher reports reviewNoPushHeadAfter as the pull
// request's live head.
type reviewNoPushFixture struct {
	rig          testRig
	timers       *narvipg.TimerStore
	registry     *sessionactor.Registry
	actor        *sessionactor.Actor
	commander    *reviewNoPushCommander
	status       http.Handler
	sessionID    pgtype.UUID
	turnID       pgtype.UUID
	repoFullName string
	prNumber     int
}

func newReviewNoPushFixture(ctx context.Context, t *testing.T) reviewNoPushFixture {
	t.Helper()
	pool := newTestPool(t)
	timers := narvipg.NewTimerStore(pool)
	rig := newTestRig(t, withTimers(timers), func(cfg *githubingress.Config) { cfg.Timeouts = platform.DefaultTimeouts() })

	commenterID := time.Now().UnixNano()
	user := createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMember)
	encrypted, err := platform.EncryptToken(reviewNoPushTokenKey, []byte("gh-fake-oauth-token"))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identities SET access_token_encrypted = $2 WHERE user_id = $1 AND provider = 'github'`, user.ID, encrypted); err != nil {
		t.Fatalf("store the creator's GitHub token: %v", err)
	}

	repoFullName := "example/review-no-push-" + strconv.FormatInt(commenterID, 10)
	const prNumber = 7
	sessions := narvipg.NewSessionStore(pool)
	sess, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		CreatedBy:   user.ID,
		Repos:       []byte(`[{"name":"repo1","url":"https://github.com/` + repoFullName + `.git","branch":"feature-x"}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
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
	if _, err := repoSettings.UpsertAutoRetriggerReviewToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("opt in to the automatic re-review: %v", err)
	}
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sess.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	head := reviewNoPushHeadBefore
	processing, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &head})
	if err != nil {
		t.Fatalf("create the processing turn: %v", err)
	}

	commander := &reviewNoPushCommander{}
	fetcher := &fakeReviewContextFetcher{pr: githubapi.PullRequest{HeadSHA: reviewNoPushHeadAfter, BaseRef: "main"}, diff: "+ the line someone else's push changed"}
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "", nil, reviewNoPushTokenKey, "", nil, false,
		sessionactor.RegistryOptions{ReviewDiffFetcher: fetcher, GitHubBotHandle: "narvi-bot", GitHubOutbound: platform.MustNewGitHubOutboundConfig("test-token")})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })
	actor, err := registry.GetOrSpawn(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	router := chi.NewRouter()
	router.Get("/api/sessions/{sessionID}/status", httpapi.GetSessionStatus(sessions, sessionactivity.NewWaiter(sessionactivity.ConfigFrom(platform.DefaultTimeouts())), platform.DefaultTimeouts()))
	return reviewNoPushFixture{
		rig: rig, timers: timers, registry: registry, actor: actor, commander: commander, status: router,
		sessionID: sess.ID, turnID: processing.ID, repoFullName: repoFullName, prNumber: prNumber,
	}
}

func (f reviewNoPushFixture) mustRead(t *testing.T) restdtos.SessionActivity {
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

// send hands one sandbox event to the actor and waits for it to commit.
func (f reviewNoPushFixture) send(ctx context.Context, t *testing.T, eventType string, raw json.RawMessage, messageID string) {
	t.Helper()
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	if err := f.actor.Send(ctx, sessionactor.SandboxEvent{Type: eventType, Gen: 1, MessageID: messageID, Raw: raw, Reply: reply}); err != nil {
		t.Fatalf("send %s: %v", eventType, err)
	}
	select {
	case outcome := <-reply:
		if !outcome.Persisted {
			t.Fatalf("%s was not persisted", eventType)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no reply to %s", eventType)
	}
}

// executionComplete completes the processing turn. The event is delivered
// twice: the actor handles one command at a time and would send a push
// after the first delivery's reply, still inside its handling, so once the
// redelivery's reply arrives any push the first delivery was going to send
// has been sent. The redelivery itself completes nothing.
func (f reviewNoPushFixture) executionComplete(ctx context.Context, t *testing.T) {
	t.Helper()
	id := uuid.NewString()
	raw, err := json.Marshal(sandboxws.ExecutionComplete{
		Type: "execution_complete", MessageId: id, SessionId: f.sessionID.String(), Gen: 1,
		AckId: "execution_complete:" + id, Outcome: sandboxws.ExecutionCompleteOutcomeCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.send(ctx, t, "execution_complete", raw, id)
	f.send(ctx, t, "execution_complete", raw, id)
}

func (f reviewNoPushFixture) debounce(ctx context.Context, t *testing.T) (sqlcgen.SessionTimer, bool) {
	t.Helper()
	return getReviewRetriggerDebounceTimer(ctx, t, f.timers, f.sessionID)
}

func (f reviewNoPushFixture) pendingHead(ctx context.Context, t *testing.T) string {
	t.Helper()
	var head *string
	if err := f.rig.pool.QueryRow(ctx, `SELECT pending_retrigger_head_sha FROM github_pr_sessions WHERE session_id = $1`, f.sessionID).Scan(&head); err != nil {
		t.Fatalf("read pending_retrigger_head_sha: %v", err)
	}
	if head == nil {
		return ""
	}
	return *head
}

func (f reviewNoPushFixture) turns(ctx context.Context, t *testing.T) []sqlcgen.Turn {
	t.Helper()
	turns, err := f.rig.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	return turns
}

// TestSessionStatus_ReviewSessionNeverPushesSoItsTurnEndsSettled: the
// review session's turn completes, and nothing about it leaves the server
// -- no push command, no delivery -- so the status reads finished and
// settled at once with nothing armed, never delivering. The re-review is
// then armed only by a push that is not the session's: the signed
// synchronize for it arms the debounce from its own instant, the status
// reads scheduled, and queued once the fire inserts the review turn of the
// new head.
func TestSessionStatus_ReviewSessionNeverPushesSoItsTurnEndsSettled(t *testing.T) {
	ctx := context.Background()
	f := newReviewNoPushFixture(ctx, t)

	f.executionComplete(ctx, t)
	if pushes := f.commander.pushes(t); len(pushes) != 0 {
		t.Fatalf("push commands = %+v, want none: a review session never pushes", pushes)
	}
	var stamped bool
	if err := f.rig.pool.QueryRow(ctx, `SELECT pr_delivery_started_at IS NOT NULL FROM sandboxes WHERE session_id = $1`, f.sessionID).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped {
		t.Fatal("a delivery was started for a review session's turn")
	}
	if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.SuggestedDelaySeconds != 300 {
		t.Fatalf("after execution_complete: activity %q settled %v delay %d, want finished, settled, 300", got.Activity, got.Settled, got.SuggestedDelaySeconds)
	}
	if _, armed := f.debounce(ctx, t); armed {
		t.Fatal("the debounce is armed with no push to the pull request at all")
	}
	if head := f.pendingHead(ctx, t); head != "" {
		t.Fatalf("pending head %q with no push to the pull request at all, want none", head)
	}
	if turns := f.turns(ctx, t); len(turns) != 1 || turns[0].ID != f.turnID || turns[0].Status != sqlcgen.TurnStatusCompleted {
		t.Fatalf("turns = %+v, want only the processing turn, completed", turns)
	}

	// A push by someone else moves the head; its synchronize lands.
	debounce := platform.DefaultTimeouts().ReviewRetriggerDebounce
	before := time.Now()
	if status := postWebhookEventType(t, f.rig, pullRequestSynchronizeBody(f.repoFullName, f.prNumber, reviewNoPushHeadAfter), "synchronize-"+f.sessionID.String(), "pull_request"); status != http.StatusOK {
		t.Fatalf("synchronize webhook: %d, want 200", status)
	}
	after := time.Now()
	timer, armed := f.debounce(ctx, t)
	if !armed || timer.FiresAt.Time.Before(before.Add(debounce).Truncate(time.Microsecond)) || timer.FiresAt.Time.After(after.Add(debounce)) {
		t.Fatalf("the synchronize: debounce armed %v fires %v, want armed %v after the webhook's instant (between %v and %v)", armed, timer.FiresAt.Time, debounce, before.Add(debounce), after.Add(debounce))
	}
	if head := f.pendingHead(ctx, t); head != reviewNoPushHeadAfter {
		t.Fatalf("the synchronize: pending head %q, want the event's head %s", head, reviewNoPushHeadAfter)
	}
	if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityScheduled || got.Settled {
		t.Fatalf("the synchronize: activity %q settled %v, want scheduled", got.Activity, got.Settled)
	}

	if _, err := f.rig.pool.Exec(ctx, `UPDATE session_timers SET fires_at = now() WHERE session_id = $1 AND name = $2`, f.sessionID, sessionactor.TimerReviewRetriggerDebounce); err != nil {
		t.Fatalf("bring the debounce due: %v", err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if err := f.registry.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
		if _, armed := f.debounce(ctx, t); !armed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the debounce timer was never handled")
		}
	}
	turns := f.turns(ctx, t)
	if len(turns) != 2 {
		t.Fatalf("%d turns after the fire, want 2: one review turn of the new head", len(turns))
	}
	review := turns[len(turns)-1]
	if !review.IsReviewAttempt || review.Status != sqlcgen.TurnStatusPending || review.ReviewHeadSha == nil || *review.ReviewHeadSha != reviewNoPushHeadAfter {
		t.Fatalf("the fire's turn = %+v, want a pending review attempt of %s", review, reviewNoPushHeadAfter)
	}
	if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityQueued || got.Settled {
		t.Fatalf("the review turn exists: activity %q settled %v, want queued", got.Activity, got.Settled)
	}
}
