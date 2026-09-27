//go:build integration

// The echo of a pull request review session's own push, on real Postgres,
// end to end: a limit technical plan §43.20 states rather than closes. A
// pull request's review session pushes its own work to that pull request's
// head (its repos[].branch is the head ref), and the code host echoes the
// push back as pull_request/synchronize, which arms §24's automatic
// re-review on the same session. The server records nothing when the push
// completes, or when it reports push_error after reaching the remote: the
// synchronize webhook alone arms the debounce, exactly as it does for any
// other push. So from the end of the delivery until that webhook lands the
// status reads finished and settled; once it lands, scheduled; once the
// fire inserts the review turn, queued. Everything is real but the code
// host and the sandbox: the session actor handles execution_complete,
// push_complete and push_error, the signed webhook goes through
// NewHandler, the debounce fires through the real timer pump, and the
// status is read through the real httpapi.GetSessionStatus handler.
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
// repository, opted in to the automatic re-review, created by a member with
// a GitHub token, whose repos[].branch is the pull request's head ref, with
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

func newOwnPushFixture(ctx context.Context, t *testing.T) ownPushFixture {
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

func (f ownPushFixture) mustRead(t *testing.T) restdtos.SessionActivity {
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

// executionComplete completes the processing turn and waits for the push
// command it sends once its transaction has committed: one push of
// feature-x, the pull request's head.
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
	if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityDelivering || got.Settled {
		t.Fatalf("execution_complete: activity %q settled %v, want delivering", got.Activity, got.Settled)
	}
	var pushes []sandboxws.Push
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if pushes = f.commander.pushes(t); len(pushes) > 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(pushes) != 1 || len(pushes[0].Repos) != 1 || pushes[0].Repos[0].Branch != "feature-x" {
		t.Fatalf("push commands = %+v, want one push of feature-x, the pull request's head", pushes)
	}
}

// deliverOwnPush runs the session's own delivery up to the pull request:
// execution_complete, the push command, push_complete for pushedSHA, and
// CreatePR -- held open while the status is read. It returns the read
// taken while CreatePR ran and the one taken once the delivery stamp had
// cleared.
func (f ownPushFixture) deliverOwnPush(ctx context.Context, t *testing.T, pushedSHA string) (duringCreatePR, afterDelivery restdtos.SessionActivity) {
	t.Helper()
	f.executionComplete(ctx, t)

	f.scm.mu.Lock()
	f.scm.entered, f.scm.release = make(chan struct{}), make(chan struct{})
	f.scm.mu.Unlock()
	id := uuid.NewString()
	raw, err := json.Marshal(sandboxws.PushComplete{
		Type: "push_complete", MessageId: id, SessionId: f.sessionID.String(), Gen: 1, AckId: "push_complete:" + id,
		Repos: []sandboxws.PushCompleteReposElem{{Name: "repo1", Branch: "feature-x", Sha: pushedSHA}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.send(ctx, t, "push_complete", raw, id)
	select {
	case <-f.scm.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("CreatePR was never called")
	}
	duringCreatePR = f.mustRead(t)
	close(f.scm.release)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
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
	}
	return duringCreatePR, f.mustRead(t)
}

// pushError is the sandbox's report that the push failed -- here, after
// git push had already moved the remote branch, when reading the head back
// failed.
func (f ownPushFixture) pushError(ctx context.Context, t *testing.T, message string) {
	t.Helper()
	id := uuid.NewString()
	raw, err := json.Marshal(sandboxws.PushError{
		Type: "push_error", MessageId: id, SessionId: f.sessionID.String(), Gen: 1, AckId: "push_error:" + id, Error: message,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.send(ctx, t, "push_error", raw, id)
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

// wantSettledWithNothingRecorded is the stated limit's first half: the
// delivery is over, the push recorded nothing -- no pending head, no
// debounce -- and the status reads finished and settled, although the
// push moved the pull request's head and its synchronize is still to come.
func (f ownPushFixture) wantSettledWithNothingRecorded(ctx context.Context, t *testing.T, stage string, got restdtos.SessionActivity) {
	t.Helper()
	if got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.SuggestedDelaySeconds != 300 {
		t.Fatalf("%s: activity %q settled %v delay %d, want finished, settled, 300 -- §43.20's stated limit", stage, got.Activity, got.Settled, got.SuggestedDelaySeconds)
	}
	if _, armed := f.debounce(ctx, t); armed {
		t.Fatalf("%s: the debounce is armed before any synchronize -- the session's own push records nothing, as on main", stage)
	}
	if head := f.pendingHead(ctx, t); head != "" {
		t.Fatalf("%s: pending head %q before any synchronize, want none", stage, head)
	}
	if turns := f.turns(ctx, t); len(turns) == 0 || turns[0].ID != f.turnID || turns[0].Status != sqlcgen.TurnStatusCompleted {
		t.Fatalf("%s: turns = %+v, want the processing turn completed first", stage, turns)
	}
}

// lateSynchronizeArmsAsOnMain is the limit's second half: the code host's
// synchronize for the pushed head lands, and the webhook does exactly what
// it does on main -- pending_retrigger_head_sha set to the event's head,
// the debounce armed ReviewRetriggerDebounce after the webhook's own
// instant, whatever turns already exist -- so the status reads scheduled;
// the fire then inserts the review turn of that head, and it reads queued.
func (f ownPushFixture) lateSynchronizeArmsAsOnMain(ctx context.Context, t *testing.T) {
	t.Helper()
	turnsBefore := len(f.turns(ctx, t))
	debounce := platform.DefaultTimeouts().ReviewRetriggerDebounce
	before := time.Now()
	if status := postWebhookEventType(t, f.rig, pullRequestSynchronizeBody(f.repoFullName, f.prNumber, ownPushHeadAfter), "late-synchronize-"+f.sessionID.String(), "pull_request"); status != http.StatusOK {
		t.Fatalf("synchronize webhook: %d, want 200", status)
	}
	after := time.Now()
	timer, armed := f.debounce(ctx, t)
	if !armed || timer.FiresAt.Time.Before(before.Add(debounce).Truncate(time.Microsecond)) || timer.FiresAt.Time.After(after.Add(debounce)) {
		t.Fatalf("the late synchronize: debounce armed %v fires %v, want armed %v after the webhook's instant (between %v and %v)", armed, timer.FiresAt.Time, debounce, before.Add(debounce), after.Add(debounce))
	}
	if head := f.pendingHead(ctx, t); head != ownPushHeadAfter {
		t.Fatalf("the late synchronize: pending head %q, want the event's head %s", head, ownPushHeadAfter)
	}
	if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityScheduled || got.Settled {
		t.Fatalf("the late synchronize: activity %q settled %v, want scheduled", got.Activity, got.Settled)
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
	if len(turns) != turnsBefore+1 {
		t.Fatalf("%d turns after the fire, want %d: one review turn of the pushed head", len(turns), turnsBefore+1)
	}
	review := turns[len(turns)-1]
	if !review.IsReviewAttempt || review.Status != sqlcgen.TurnStatusPending || review.ReviewHeadSha == nil || *review.ReviewHeadSha != ownPushHeadAfter {
		t.Fatalf("the fire's turn = %+v, want a pending review attempt of %s", review, ownPushHeadAfter)
	}
	if got := f.mustRead(t); got.Activity != restdtos.SessionActivityActivityQueued || got.Settled {
		t.Fatalf("the review turn exists: activity %q settled %v, want queued", got.Activity, got.Settled)
	}
}

// TestSessionStatus_OwnPushEchoIsAStatedLimit pins technical plan
// §43.20's stated limit and main's synchronize behaviour under it: after a
// pull request review session's own push moves that pull request's head,
// the status reads finished and settled until the code host's synchronize
// lands; the webhook then arms the debounce exactly as on main, and the
// status reads scheduled, then queued once the review turn exists.
func TestSessionStatus_OwnPushEchoIsAStatedLimit(t *testing.T) {
	ctx := context.Background()

	t.Run("push_complete: settled until the synchronize lands, then scheduled, then queued", func(t *testing.T) {
		f := newOwnPushFixture(ctx, t)
		during, after := f.deliverOwnPush(ctx, t, ownPushHeadAfter)
		if during.Activity != restdtos.SessionActivityActivityDelivering || during.Settled {
			t.Fatalf("while CreatePR runs: activity %q settled %v, want delivering", during.Activity, during.Settled)
		}
		f.wantSettledWithNothingRecorded(ctx, t, "the delivery ended, no synchronize yet", after)
		f.lateSynchronizeArmsAsOnMain(ctx, t)
	})

	// Review round 5's P7: the push reached the remote, then reading its
	// head back failed, so the sandbox reports push_error. The head moved
	// all the same, and the same synchronize follows.
	t.Run("push_error after the push reached the remote: settled, then scheduled once the synchronize lands", func(t *testing.T) {
		f := newOwnPushFixture(ctx, t)
		f.executionComplete(ctx, t)
		f.pushError(ctx, t, "determine head sha for repo1: exit status 128")
		f.wantSettledWithNothingRecorded(ctx, t, "push_error committed", f.mustRead(t))
		f.lateSynchronizeArmsAsOnMain(ctx, t)
	})

	// Review round 5's P4-P6: main's webhook arms the debounce whatever
	// turns the session already has. An ordinary follow-up mention resolves
	// the live head when it is created, so its turn carries the pushed head
	// without being a review of it; the late synchronize still arms, and
	// the fire still reviews that head.
	t.Run("a turn already created against the pushed head does not stop the synchronize arming", func(t *testing.T) {
		f := newOwnPushFixture(ctx, t)
		_, after := f.deliverOwnPush(ctx, t, ownPushHeadAfter)
		f.wantSettledWithNothingRecorded(ctx, t, "the delivery ended", after)
		head := ownPushHeadAfter
		prompt := "why did you flag this?"
		if _, err := f.rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: f.sessionID, Status: sqlcgen.TurnStatusCompleted, Prompt: &prompt, ReviewHeadSha: &head}); err != nil {
			t.Fatalf("create the follow-up turn against the pushed head: %v", err)
		}
		f.lateSynchronizeArmsAsOnMain(ctx, t)
	})
}
