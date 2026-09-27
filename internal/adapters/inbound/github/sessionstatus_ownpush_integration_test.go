//go:build integration

// Review round 4's P1 on real Postgres, end to end (technical plan
// §43.20): a pull request's review session pushes its own work to the pull
// request's head, and GitHub echoes that push back as
// pull_request/synchronize, which arms the automatic re-review of the same
// session. The server records that work when it creates the cause -- in
// the transaction that persists the push's push_complete, through
// sessionactor.RecordPullRequestPush, the write the webhook repeats -- so
// the status, read through the real httpapi.GetSessionStatus handler,
// never reads settled between the push and the review turn. Everything is
// real but GitHub and the sandbox: the session actor handles
// execution_complete and push_complete, the signed webhook goes through
// NewHandler, and the debounce fires through the real timer pump.
package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

var ownPushTokenKey = []byte("0123456789abcdef0123456789abcdef")

// ownPushSourceControl stands in for GitHub on the actor's own delivery
// path: CreatePR returns the pull request that already exists, and can be
// held open so a test reads the status while it runs.
type ownPushSourceControl struct {
	ports.SourceControl

	mu      sync.Mutex
	creates int
	entered chan struct{}
	release chan struct{}
	ref     ports.PRRef
}

func (f *ownPushSourceControl) ResolveBranchSHA(context.Context, ports.ResolveBranchSHASpec) (string, string, error) {
	return "0123456789abcdef0123456789abcdef01234567", "main", nil
}

func (f *ownPushSourceControl) CheckRepoAccess(context.Context, ports.CheckRepoAccessSpec) (bool, error) {
	return true, nil
}

func (f *ownPushSourceControl) ResolveContractsFingerprint(context.Context, ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return "", false, nil
}

func (f *ownPushSourceControl) RegisterPRStack(context.Context, ports.RegisterPRStackSpec) error {
	return nil
}

func (f *ownPushSourceControl) CreatePR(ctx context.Context, _ ports.CreatePRSpec) (ports.PRRef, error) {
	f.mu.Lock()
	f.creates++
	entered, release := f.entered, f.release
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return ports.PRRef{}, ctx.Err()
		}
	}
	return f.ref, nil
}

// ownPushCommander records the commands the actor sends the sandbox.
type ownPushCommander struct {
	mu       sync.Mutex
	payloads []json.RawMessage
}

func (c *ownPushCommander) SendCommand(_ string, payload json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, payload)
	return nil
}

func (c *ownPushCommander) pushes(t *testing.T) []sandboxws.Push {
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
	// ownPushHeadBefore is the pull request's head when the processing
	// turn was created -- the head that turn was created against.
	ownPushHeadBefore = "1111111111111111111111111111111111111111"
	// ownPushHeadAfter is the head the session's own push produced.
	ownPushHeadAfter = "2222222222222222222222222222222222222222"
)

// ownPushFixture is one review session of pull request #7 on its own
// repository, created by a member with a GitHub token, whose repos[].branch
// is the pull request's head ref -- repo1 cloned from cloneRepo, which is
// the pull request's own repository or, for a fork, another one -- with
// live egress on, one processing turn created against ownPushHeadBefore,
// and a real registry: a commander that records commands, a SourceControl
// that answers the pull request, and a review fetcher that reports the
// pushed head as the pull request's live head.
type ownPushFixture struct {
	rig          testRig
	timers       *narvipg.TimerStore
	registry     *sessionactor.Registry
	actor        *sessionactor.Actor
	commander    *ownPushCommander
	scm          *ownPushSourceControl
	status       http.Handler
	sessionID    pgtype.UUID
	turnID       pgtype.UUID
	repoFullName string
	prNumber     int
}

func newOwnPushFixture(ctx context.Context, t *testing.T, fork, optedIn bool) ownPushFixture {
	t.Helper()
	pool := newTestPool(t)
	timers := narvipg.NewTimerStore(pool)
	rig := newTestRig(t, withTimers(timers), func(cfg *githubingress.Config) { cfg.Timeouts = platform.DefaultTimeouts() })

	commenterID := time.Now().UnixNano()
	user := createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMember)
	encrypted, err := platform.EncryptToken(ownPushTokenKey, []byte("gh-fake-oauth-token"))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identities SET access_token_encrypted = $2 WHERE user_id = $1 AND provider = 'github'`, user.ID, encrypted); err != nil {
		t.Fatalf("store the creator's GitHub token: %v", err)
	}

	repoFullName := "example/own-push-" + strconv.FormatInt(commenterID, 10)
	cloneRepo := repoFullName
	if fork {
		cloneRepo = "a-contributor/own-push-" + strconv.FormatInt(commenterID, 10)
	}
	const prNumber = 7
	sessions := narvipg.NewSessionStore(pool)
	sess, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		CreatedBy:   user.ID,
		Repos:       []byte(`[{"name":"repo1","url":"https://github.com/` + cloneRepo + `.git","branch":"feature-x"}]`),
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
	for _, repo := range []string{repoFullName, cloneRepo} {
		if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repo, true); err != nil {
			t.Fatalf("arm live egress for %s: %v", repo, err)
		}
	}
	if optedIn {
		if _, err := repoSettings.UpsertAutoRetriggerReviewToggle(ctx, repoFullName, true); err != nil {
			t.Fatalf("opt in to the automatic re-review: %v", err)
		}
	}
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sess.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	head := ownPushHeadBefore
	processing, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &head})
	if err != nil {
		t.Fatalf("create the processing turn: %v", err)
	}

	commander := &ownPushCommander{}
	scm := &ownPushSourceControl{ref: ports.PRRef{Number: prNumber, URL: "https://github.com/" + repoFullName + "/pull/7"}}
	fetcher := &fakeReviewContextFetcher{pr: githubapi.PullRequest{HeadSHA: ownPushHeadAfter, BaseRef: "main"}, diff: "+ the line the session's own push changed"}
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "", scm, ownPushTokenKey, "", nil, false,
		sessionactor.RegistryOptions{ReviewDiffFetcher: fetcher, GitHubBotHandle: "narvi-bot", GitHubBotToken: "test-token"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })
	actor, err := registry.GetOrSpawn(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	router := chi.NewRouter()
	router.Get("/api/sessions/{sessionID}/status", httpapi.GetSessionStatus(sessions, platform.DefaultTimeouts()))
	return ownPushFixture{
		rig: rig, timers: timers, registry: registry, actor: actor, commander: commander, scm: scm, status: router,
		sessionID: sess.ID, turnID: processing.ID, repoFullName: repoFullName, prNumber: prNumber,
	}
}

func (f ownPushFixture) read() (restdtos.SessionActivity, error) {
	rec := httptest.NewRecorder()
	f.status.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+f.sessionID.String()+"/status", nil))
	var got restdtos.SessionActivity
	if rec.Code != http.StatusOK {
		return got, fmt.Errorf("GET status: %d %s", rec.Code, rec.Body.String())
	}
	return got, json.Unmarshal(rec.Body.Bytes(), &got)
}

func (f ownPushFixture) mustRead(t *testing.T) restdtos.SessionActivity {
	t.Helper()
	got, err := f.read()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// send hands one sandbox event to the actor and waits for it to commit.
func (f ownPushFixture) send(ctx context.Context, t *testing.T, eventType string, raw json.RawMessage, messageID string) {
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

func (f ownPushFixture) executionComplete(ctx context.Context, t *testing.T) {
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
}

// pushComplete is the sandbox's report of the push, naming the head it
// left on the branch; it returns once the actor has committed it (the
// pull request creation that follows runs after that commit).
func (f ownPushFixture) pushComplete(ctx context.Context, t *testing.T, sha string) {
	t.Helper()
	id := uuid.NewString()
	raw, err := json.Marshal(sandboxws.PushComplete{
		Type: "push_complete", MessageId: id, SessionId: f.sessionID.String(), Gen: 1, AckId: "push_complete:" + id,
		Repos: []sandboxws.PushCompleteReposElem{{Name: "repo1", Branch: "feature-x", Sha: sha}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.send(ctx, t, "push_complete", raw, id)
}

// webhook delivers GitHub's pull_request/synchronize for headSHA, signed,
// through NewHandler, under a delivery id of its own.
func (f ownPushFixture) webhook(t *testing.T, headSHA, delivery string) {
	t.Helper()
	if status := postWebhookEventType(t, f.rig, pullRequestSynchronizeBody(f.repoFullName, f.prNumber, headSHA), delivery+"-"+f.sessionID.String(), "pull_request"); status != http.StatusOK {
		t.Fatalf("synchronize webhook: %d, want 200", status)
	}
}

func (f ownPushFixture) debounce(ctx context.Context, t *testing.T) (sqlcgen.SessionTimer, bool) {
	t.Helper()
	return getReviewRetriggerDebounceTimer(ctx, t, f.timers, f.sessionID)
}

func (f ownPushFixture) pendingHead(ctx context.Context, t *testing.T) string {
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

func (f ownPushFixture) turns(ctx context.Context, t *testing.T) []sqlcgen.Turn {
	t.Helper()
	turns, err := f.rig.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	return turns
}

// fireDebounce brings the armed debounce due and runs real timer-pump
// ticks until its handler has run (created the turn or declined).
func (f ownPushFixture) fireDebounce(ctx context.Context, t *testing.T) {
	t.Helper()
	if _, err := f.rig.pool.Exec(ctx, `UPDATE session_timers SET fires_at = now() WHERE session_id = $1 AND name = $2`, f.sessionID, sessionactor.TimerReviewRetriggerDebounce); err != nil {
		t.Fatalf("bring the debounce due: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := f.registry.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
		if _, armed := f.debounce(ctx, t); !armed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the debounce timer was never handled")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// watchStatus polls the status route until stop is closed, recording every
// read; wait returns them. The reader runs in g.
func (f ownPushFixture) watchStatus(g *errgroup.Group, stop <-chan struct{}) func() []restdtos.SessionActivity {
	var mu sync.Mutex
	var seen []restdtos.SessionActivity
	g.Go(func() error {
		for {
			select {
			case <-stop:
				return nil
			default:
			}
			got, err := f.read()
			if err != nil {
				return err
			}
			mu.Lock()
			seen = append(seen, got)
			mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	})
	return func() []restdtos.SessionActivity {
		mu.Lock()
		defer mu.Unlock()
		return append([]restdtos.SessionActivity(nil), seen...)
	}
}

func isSettled(got restdtos.SessionActivity) bool {
	return got.Settled || got.Activity == restdtos.SessionActivityActivityFinished || got.Activity == restdtos.SessionActivityActivityIdle
}

// deliverOwnPush runs the session's own delivery up to the pull request:
// execution_complete, the push command, push_complete for pushedSHA, and
// CreatePR -- held open while the status is read. It returns the read
// taken while CreatePR ran and the one taken once it had returned.
func (f ownPushFixture) deliverOwnPush(ctx context.Context, t *testing.T, pushedSHA string) (duringCreatePR, afterCreatePR restdtos.SessionActivity) {
	t.Helper()
	f.executionComplete(ctx, t)
	if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityDelivering || got.Settled {
		t.Fatalf("execution_complete: activity %q settled %v, want delivering", got.Activity, got.Settled)
	}
	// The push command is sent once the event's transaction has committed,
	// after the actor's reply.
	var pushes []sandboxws.Push
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if pushes = f.commander.pushes(t); len(pushes) > 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(pushes) != 1 || len(pushes[0].Repos) != 1 || pushes[0].Repos[0].Branch != "feature-x" {
		t.Fatalf("push commands = %+v, want one push of feature-x, the pull request's head", pushes)
	}

	f.scm.mu.Lock()
	f.scm.entered, f.scm.release = make(chan struct{}), make(chan struct{})
	f.scm.mu.Unlock()
	f.pushComplete(ctx, t, pushedSHA)
	select {
	case <-f.scm.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("CreatePR was never called")
	}
	duringCreatePR = f.mustRead(t)
	close(f.scm.release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var stamped bool
		if err := f.rig.pool.QueryRow(ctx, `SELECT pr_delivery_started_at IS NOT NULL FROM sandboxes WHERE session_id = $1`, f.sessionID).Scan(&stamped); err != nil {
			t.Fatal(err)
		}
		if !stamped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the delivery stamp was never cleared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return duringCreatePR, f.mustRead(t)
}

// TestSessionStatus_OwnPushIsScheduledUntilItsReReviewExists is the
// reviewers' sequence: a turn of a pull request's review session commits
// and completes; execution_complete sends the push; push_complete arrives
// with the head the push produced, and the pull request is "created"
// (GitHub already has it). The status must never read settled from the
// turn's completion until the review turn the echo causes exists -- a
// reader polls the route through the whole sequence -- and the webhook,
// arriving late, re-arms the same head from its own later instant and
// never makes a second review.
func TestSessionStatus_OwnPushIsScheduledUntilItsReReviewExists(t *testing.T) {
	ctx := context.Background()

	t.Run("the webhook lands after the pull request, before the debounce fires", func(t *testing.T) {
		f := newOwnPushFixture(ctx, t, false, true)
		var readers errgroup.Group
		stop := make(chan struct{})
		seen := f.watchStatus(&readers, stop)

		during, after := f.deliverOwnPush(ctx, t, ownPushHeadAfter)
		if during.Activity != restdtos.SessionActivityActivityDelivering {
			close(stop)
			_ = readers.Wait()
			t.Fatalf("while CreatePR runs: activity %q, want delivering", during.Activity)
		}
		preArmed, armed := f.debounce(ctx, t)
		if !armed || f.pendingHead(ctx, t) != ownPushHeadAfter {
			close(stop)
			_ = readers.Wait()
			t.Fatalf("after push_complete: debounce armed %v, pending head %q; want armed for %s before any webhook", armed, f.pendingHead(ctx, t), ownPushHeadAfter)
		}
		if after.Activity != restdtos.SessionActivityActivityScheduled || after.Settled {
			close(stop)
			_ = readers.Wait()
			t.Fatalf("the pull request created, no webhook yet: activity %q settled %v, want scheduled -- the echo's re-review is coming", after.Activity, after.Settled)
		}

		time.Sleep(20 * time.Millisecond)
		f.webhook(t, ownPushHeadAfter, "late-synchronize")
		reArmed, armed := f.debounce(ctx, t)
		if !armed || !reArmed.FiresAt.Time.After(preArmed.FiresAt.Time) || f.pendingHead(ctx, t) != ownPushHeadAfter {
			close(stop)
			_ = readers.Wait()
			t.Fatalf("the late webhook: armed %v fires %v (pre-armed %v), pending %q; want the same head, re-armed from the webhook's later instant", armed, reArmed.FiresAt.Time, preArmed.FiresAt.Time, f.pendingHead(ctx, t))
		}

		f.fireDebounce(ctx, t)
		close(stop)
		if err := readers.Wait(); err != nil {
			t.Fatalf("status reader: %v", err)
		}
		reads := seen()
		if len(reads) == 0 {
			t.Fatal("the status was never read during the sequence")
		}
		for i, got := range reads {
			if isSettled(got) {
				t.Fatalf("read %d of %d: activity %q settled %v -- the session read settled before the review of its own push existed", i, len(reads), got.Activity, got.Settled)
			}
		}

		turns := f.turns(ctx, t)
		if len(turns) != 2 || turns[0].ID != f.turnID || turns[0].Status != sqlcgen.TurnStatusCompleted || !turns[1].IsReviewAttempt || turns[1].Status != sqlcgen.TurnStatusPending {
			t.Fatalf("turns = %+v, want [the completed turn, one pending review turn]", turns)
		}
		if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityQueued || got.Settled {
			t.Fatalf("the review turn exists: activity %q settled %v, want queued", got.Activity, got.Settled)
		}

		// GitHub delivers the synchronize once more (a redelivery): a turn
		// was created against that head already, so nothing is armed.
		f.webhook(t, ownPushHeadAfter, "redelivered-synchronize")
		if _, armed := f.debounce(ctx, t); armed {
			t.Fatal("a synchronize for a head the session already has a review turn for armed the debounce again")
		}
		if n := len(f.turns(ctx, t)); n != 2 {
			t.Fatalf("%d turns after the redelivery, want still 2 -- no second review of the same head", n)
		}
	})

	t.Run("the webhook lands only after the review turn exists", func(t *testing.T) {
		f := newOwnPushFixture(ctx, t, false, true)
		var readers errgroup.Group
		stop := make(chan struct{})
		seen := f.watchStatus(&readers, stop)

		_, after := f.deliverOwnPush(ctx, t, ownPushHeadAfter)
		if after.Activity != restdtos.SessionActivityActivityScheduled || after.Settled {
			close(stop)
			_ = readers.Wait()
			t.Fatalf("the pull request created: activity %q settled %v, want scheduled", after.Activity, after.Settled)
		}
		f.fireDebounce(ctx, t)
		close(stop)
		if err := readers.Wait(); err != nil {
			t.Fatalf("status reader: %v", err)
		}
		for i, got := range seen() {
			if isSettled(got) {
				t.Fatalf("read %d: activity %q settled %v before the review turn existed", i, got.Activity, got.Settled)
			}
		}
		if turns := f.turns(ctx, t); len(turns) != 2 || !turns[1].IsReviewAttempt || turns[1].ReviewHeadSha == nil || *turns[1].ReviewHeadSha != ownPushHeadAfter {
			t.Fatalf("turns = %+v, want the completed turn and one review turn of %s", turns, ownPushHeadAfter)
		}

		f.webhook(t, ownPushHeadAfter, "very-late-synchronize")
		if _, armed := f.debounce(ctx, t); armed {
			t.Fatal("the late webhook armed a second re-review of the head the review turn already covers")
		}
		if err := f.registry.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
		if n := len(f.turns(ctx, t)); n != 2 {
			t.Fatalf("%d turns after the late webhook, want 2 -- no duplicate review", n)
		}
	})

	t.Run("not opted in: armed, but the status reads finished at once and the fire declines", func(t *testing.T) {
		f := newOwnPushFixture(ctx, t, false, false)
		_, after := f.deliverOwnPush(ctx, t, ownPushHeadAfter)
		if _, armed := f.debounce(ctx, t); !armed {
			t.Fatal("the push was not recorded: the webhook would arm the debounce whatever the opt-in")
		}
		if after.Activity != restdtos.SessionActivityActivityFinished || !after.Settled {
			t.Fatalf("not opted in: activity %q settled %v, want finished -- the fire can only decline", after.Activity, after.Settled)
		}
		f.fireDebounce(ctx, t)
		if n := len(f.turns(ctx, t)); n != 1 {
			t.Fatalf("%d turns after the declined fire, want 1", n)
		}
	})

	t.Run("a fork pull request: the push never reaches the head in this repository, nothing pre-armed", func(t *testing.T) {
		f := newOwnPushFixture(ctx, t, true, true)
		_, after := f.deliverOwnPush(ctx, t, ownPushHeadAfter)
		if _, armed := f.debounce(ctx, t); armed {
			t.Fatal("a fork pull request's push pre-armed the debounce")
		}
		if head := f.pendingHead(ctx, t); head != "" {
			t.Fatalf("a fork pull request's push recorded pending head %q", head)
		}
		if after.Activity != restdtos.SessionActivityActivityFinished || !after.Settled {
			t.Fatalf("fork: activity %q settled %v, want finished", after.Activity, after.Settled)
		}
	})

	// git reports the branch's head after a push whether or not the push
	// moved it: a turn that committed nothing reports a head the server
	// already knows, no synchronize will follow, and nothing is recorded --
	// whichever way the server knows it.
	const knownHead = "3333333333333333333333333333333333333333"
	for _, known := range []struct {
		name string
		// seed makes knownHead known, and returns the debounce it armed,
		// if any.
		seed func(ctx context.Context, t *testing.T, f ownPushFixture) (sqlcgen.SessionTimer, bool)
	}{
		{"the head the finishing turn was created against", func(ctx context.Context, t *testing.T, f ownPushFixture) (sqlcgen.SessionTimer, bool) {
			if _, err := f.rig.pool.Exec(ctx, `UPDATE turns SET review_head_sha = $2 WHERE id = $1`, f.turnID, knownHead); err != nil {
				t.Fatal(err)
			}
			return sqlcgen.SessionTimer{}, false
		}},
		{"a head a verdict was posted for", func(ctx context.Context, t *testing.T, f ownPushFixture) (sqlcgen.SessionTimer, bool) {
			insertPriorReviewVerdict(ctx, t, f.rig, f.repoFullName, int32(f.prNumber), knownHead, "light")
			return sqlcgen.SessionTimer{}, false
		}},
		{"the head the webhook already recorded (it won the race)", func(ctx context.Context, t *testing.T, f ownPushFixture) (sqlcgen.SessionTimer, bool) {
			f.webhook(t, knownHead, "early-synchronize")
			return f.debounce(ctx, t)
		}},
	} {
		t.Run("a push that moved nothing: "+known.name, func(t *testing.T) {
			f := newOwnPushFixture(ctx, t, false, true)
			before, armedBefore := known.seed(ctx, t, f)
			pendingBefore := f.pendingHead(ctx, t)
			time.Sleep(20 * time.Millisecond)
			_, after := f.deliverOwnPush(ctx, t, knownHead)
			timer, armed := f.debounce(ctx, t)
			if armed != armedBefore || (armed && !timer.FiresAt.Time.Equal(before.FiresAt.Time)) {
				t.Fatalf("debounce armed %v at %v, want as before the push (armed %v at %v): nothing recorded for a known head", armed, timer.FiresAt.Time, armedBefore, before.FiresAt.Time)
			}
			if head := f.pendingHead(ctx, t); head != pendingBefore {
				t.Fatalf("pending head %q, want %q unchanged: nothing recorded for a known head", head, pendingBefore)
			}
			if !armedBefore && (after.Activity != restdtos.SessionActivityActivityFinished || !after.Settled) {
				t.Fatalf("nothing pushed: activity %q settled %v, want finished", after.Activity, after.Settled)
			}
		})
	}
}
