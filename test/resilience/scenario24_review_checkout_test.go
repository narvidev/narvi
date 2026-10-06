//go:build integration

// Resilience scenario #24 (§9.3, docs/IMPLEMENTATION_PLAN.md row 204): "A
// review turn reads the commit it recorded -> each review turn's
// checked-out commit is its own recorded head, including a re-review in a
// warm sandbox whose tree the previous turn modified, and a control plane
// that restarts between the checkout's send and its reply sends the
// prompt once."
//
// The control plane is real: a registry with a real wshub commander, the
// real sandbox WS handler, real Postgres. The sandbox is a real
// wsbridge.Bridge whose CheckoutHandler runs the agent's own
// gitclone.CheckoutPullRef against a real base repository served by
// git-http-backend over TLS, which keeps the pull request's head as
// refs/pull/7/head (technical plan §21.1, §30.4). The rest of the agent is
// a stand-in: a prompt records the commit and the status of the worktree
// it found, may leave edits behind as a turn does, and completes; a
// snapshot is answered at once.
package resilience_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/gitclone"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

const (
	scenario24Wait   = 20 * time.Second
	scenario24Ref    = "refs/pull/7/head"
	scenario24Claim  = "acme/widgets"
	scenario24Number = 7
)

// scenario24Origin is the pull request's base repository, acme/widgets,
// served over TLS: main holds a file of its own, and refs/pull/7/head
// holds S1, a contributor's commit; S2, S1's child, exists only in the
// contributor's clone until advance pushes it.
type scenario24Origin struct {
	url    string
	work   string
	s1, s2 string
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newScenario24Origin(t *testing.T) *scenario24Origin {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not available at %s: %v", backend, err)
	}
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	parent := t.TempDir()
	bare := filepath.Join(parent, "acme", "widgets.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, parent, "init", "--bare", "-q", "-b", "main", bare)
	work := filepath.Join(t.TempDir(), "work")
	gitIn(t, parent, "clone", "-q", bare, work)
	gitIn(t, work, "config", "user.email", "test@example.com")
	gitIn(t, work, "config", "user.name", "Test")
	commit := func(file, content, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, work, "add", ".")
		gitIn(t, work, "commit", "-q", "-m", message)
		return gitIn(t, work, "rev-parse", "HEAD")
	}
	o := &scenario24Origin{work: work}
	commit("README.md", "base\n", "base")
	gitIn(t, work, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.s1 = commit("pr.txt", "first\n", "the pull request's first head")
	o.s2 = commit("pr.txt", "second\n", "the pull request's second head")
	gitIn(t, work, "checkout", "-q", "main")
	commit("main-only.txt", "main\n", "main moves on")
	gitIn(t, work, "push", "-q", "origin", "main")
	gitIn(t, work, "push", "-q", "origin", o.s1+":"+scenario24Ref)

	server := httptest.NewUnstartedServer(&cgi.Handler{
		Path: backend, Root: "/",
		Env: []string{"GIT_HTTP_EXPORT_ALL=1", "GIT_PROJECT_ROOT=" + parent},
	})
	server.StartTLS()
	t.Cleanup(server.Close)
	o.url = server.URL + "/acme/widgets.git"
	return o
}

// advance points the base repository's pull request ref at sha, as the
// code host does when the contributor pushes.
func (o *scenario24Origin) advance(t *testing.T, sha string) {
	t.Helper()
	gitIn(t, o.work, "push", "-q", "--force", "origin", sha+":"+scenario24Ref)
}

// promptSeen is what the stand-in runtime found when a prompt arrived.
type promptSeen struct {
	messageID string
	head      string
	status    string
}

// checkoutAgent is a sandbox-agent whose checkout is the real one: its
// CheckoutHandler runs gitclone.CheckoutPullRef off the read loop, one at
// a time, and answers with a checkout_result, as cmd/sandbox-agent does.
type checkoutAgent struct {
	noopCommandHandler
	ctx      context.Context
	bridge   *wsbridge.Bridge
	group    *errgroup.Group
	layout   gitdir.Layout
	repo     sessionconfig.SessionConfigReposElem
	worktree string
	timeouts platform.Timeouts

	checkoutMu sync.Mutex

	mu        sync.Mutex
	checkouts []sandboxws.Checkout
	hold      chan struct{}
	prompts   []promptSeen
	// afterPrompt runs on the worktree once a prompt is recorded, as a
	// turn's runtime leaves its edits behind.
	afterPrompt func()
}

var _ wsbridge.CheckoutHandler = (*checkoutAgent)(nil)

func (a *checkoutAgent) HandleCheckout(_ context.Context, cmd sandboxws.Checkout) {
	a.mu.Lock()
	a.checkouts = append(a.checkouts, cmd)
	hold := a.hold
	a.hold = nil
	a.mu.Unlock()
	a.group.Go(func() error {
		if hold != nil {
			select {
			case <-hold:
			case <-a.ctx.Done():
				return nil
			}
		}
		a.checkoutMu.Lock()
		defer a.checkoutMu.Unlock()
		out := gitclone.CheckoutPullRef(a.ctx, supervisor.New(), a.layout, nil, nil, a.repo, cmd.Repos[0].Ref, cmd.Repos[0].Sha, nil,
			a.timeouts.GitFetchStepTimeout, a.timeouts.GitSyncStepTimeout, a.timeouts.ProcessStopGracePeriod)
		entry := sandboxws.CheckoutResultReposElem{Name: cmd.Repos[0].Name, Outcome: sandboxws.CheckoutResultReposElemOutcome(out.Outcome)}
		if out.HeadSHA != "" {
			entry.HeadSha = &out.HeadSHA
		}
		if out.RefSHA != "" {
			entry.RefSha = &out.RefSHA
		}
		if out.Err != nil {
			text := out.Err.Error()
			entry.Error = &text
		}
		_ = a.bridge.SendBestEffort(a.ctx, sandboxws.CheckoutResult{
			Type: "checkout_result", MessageId: "checkout_result:" + cmd.MessageId, SessionId: cmd.SessionId,
			Gen: cmd.Gen, CommandMessageId: cmd.MessageId, Repos: []sandboxws.CheckoutResultReposElem{entry},
		})
		return nil
	})
}

func (a *checkoutAgent) HandlePrompt(_ context.Context, cmd sandboxws.Prompt) {
	a.checkoutMu.Lock()
	seen := promptSeen{messageID: cmd.MessageId, head: headIn(a.worktree), status: statusIn(a.worktree)}
	a.mu.Lock()
	a.prompts = append(a.prompts, seen)
	after := a.afterPrompt
	a.afterPrompt = nil
	a.mu.Unlock()
	if after != nil {
		after()
	}
	a.checkoutMu.Unlock()
	id := "ec-" + cmd.MessageId
	_ = a.bridge.SendCritical(a.ctx, sandboxws.ExecutionComplete{
		Type: "execution_complete", MessageId: id, SessionId: cmd.SessionId, Gen: cmd.Gen,
		AckId: "execution_complete:" + id, Outcome: sandboxws.ExecutionCompleteOutcomeCompleted,
	}, "execution_complete:"+id)
}

func (a *checkoutAgent) HandleSnapshot(_ context.Context, cmd sandboxws.Snapshot) {
	id := uuid.NewString()
	command := cmd.MessageId
	_ = a.bridge.SendCritical(a.ctx, sandboxws.SnapshotReady{
		Type: "snapshot_ready", MessageId: id, SessionId: cmd.SessionId, Gen: cmd.Gen,
		AckId: "snapshot_ready:" + id, SnapshotId: "snap-" + id, CommandMessageId: &command,
	}, "snapshot_ready:"+id)
}

func (a *checkoutAgent) seenPrompts() []promptSeen {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]promptSeen(nil), a.prompts...)
}

func (a *checkoutAgent) seenCheckouts() []sandboxws.Checkout {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sandboxws.Checkout(nil), a.checkouts...)
}

// holdNextCheckout makes the next checkout wait, unanswered, until the
// returned func runs.
func (a *checkoutAgent) holdNextCheckout() func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hold = make(chan struct{})
	hold := a.hold
	var once sync.Once
	return func() { once.Do(func() { close(hold) }) }
}

func headIn(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "error: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}

func statusIn(dir string) string {
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		return "error: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}

// scenario24Rig is one review session of acme/widgets#7 on the real
// control-plane stack, its sandbox's only way in through a proxy, and its
// workspace booted from the base repository at the pull request's ref.
type scenario24Rig struct {
	h         *Harness
	origin    *scenario24Origin
	sessionID pgtype.UUID
	proxy     *wsProxy
	proxyURL  string
	backend   atomic.Pointer[http.Handler]
	layout    gitdir.Layout
	repo      sessionconfig.SessionConfigReposElem
}

// newScenario24Rig seeds the review session, its claim, a Ready sandbox at
// gen 1 and the booted workspace, and serves the sandbox WS through a
// backend registry can be swapped behind (useBackend).
func newScenario24Rig(ctx context.Context, t *testing.T) *scenario24Rig {
	t.Helper()
	h := newHarness(t)
	rig := &scenario24Rig{h: h, origin: newScenario24Origin(t)}
	repoURL := rig.origin.url
	ref := scenario24Ref
	rig.repo = sessionconfig.SessionConfigReposElem{Name: "widgets", Url: repoURL, Ref: &ref}

	session, err := h.Sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		Repos:       []byte(`[{"name":"widgets","url":"` + repoURL + `","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create the review session: %v", err)
	}
	rig.sessionID = session.ID
	prSessions := narvipg.NewGitHubPRSessionStore(h.Pool)
	if err := prSessions.EnsureRow(ctx, scenario24Claim, scenario24Number); err != nil {
		t.Fatal(err)
	}
	if err := prSessions.SetSessionID(ctx, scenario24Claim, scenario24Number, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Sandboxes.Create(ctx, session.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := h.Sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: session.ID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	// The boot: a review session's repo carries the pull request's ref, so
	// the workspace is the base repository at the ref's tip.
	rig.layout = gitdir.Layout{Root: t.TempDir(), WorkspaceDir: t.TempDir()}
	results, err := gitclone.CloneAll(ctx, supervisor.New(), rig.layout, nil, nil,
		[]sessionconfig.SessionConfigReposElem{rig.repo}, nil, h.Timeouts.RepoCloneTimeout, h.Timeouts.ProcessStopGracePeriod)
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("boot clone = %+v, %v", results, err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*rig.backend.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(backend.Close)
	rig.proxy = newWSProxy("ws"+strings.TrimPrefix(backend.URL, "http"), func([]byte) {})
	proxyServer := httptest.NewServer(rig.proxy)
	t.Cleanup(proxyServer.Close)
	rig.proxyURL = "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/sessions/" + session.ID.String() + "/ws?type=sandbox"
	return rig
}

// newReplicaBackend builds a control-plane replica -- a registry with its
// own wshub commander -- and makes its sandbox WS handler the one the
// proxy dials from now on.
func (r *scenario24Rig) newReplicaBackend(ctx context.Context, t *testing.T) *sessionactor.Registry {
	t.Helper()
	commander := wshub.NewSandboxRegistry(r.h.Timeouts)
	registry := r.h.NewRegistryWithCommander(ctx, t, commander)
	t.Cleanup(func() { _ = registry.Shutdown() })
	router := chi.NewRouter()
	router.Get("/sessions/{sessionID}/ws", wshub.NewSandboxHandler(registry, r.h.Sandboxes, commander, r.h.Timeouts))
	var handler http.Handler = router
	r.backend.Store(&handler)
	return registry
}

// startAgent runs a real Bridge against the rig's proxy with a
// checkoutAgent as its handler.
func (r *scenario24Rig) startAgent(ctx context.Context, t *testing.T) *checkoutAgent {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	group := &errgroup.Group{}
	agent := &checkoutAgent{
		ctx: runCtx, group: group, layout: r.layout, repo: r.repo,
		worktree: filepath.Join(r.layout.WorkspaceDir, "widgets"), timeouts: r.h.Timeouts,
	}
	sc := sessionconfig.SessionConfig{
		BootMode:          sessionconfig.SessionConfigBootModeFresh,
		ControlPlaneWsUrl: r.proxyURL,
		Gen:               1,
		SandboxToken:      "scenario24-test-token", // the fresh row's NULL token_hash admits any (scenario #7's note)
		SessionId:         r.sessionID.String(),
		Repos:             []sessionconfig.SessionConfigReposElem{r.repo},
	}
	agent.bridge = wsbridge.New(sc, "sbx-scenario24", "test-agent-version", "test-image-digest", agent,
		ackTestDialTimeout, ackTestHeartbeat, ackTestMinBackoff, ackTestMaxBackoff)
	group.Go(func() error { return agent.bridge.Run(runCtx) })
	t.Cleanup(func() {
		cancel()
		if err := group.Wait(); err != nil {
			t.Errorf("agent: %v, want nil after ctx cancellation", err)
		}
	})
	// The agent's ready advertises the checkout, recorded against gen 1: a
	// sandbox is Ready only after its ready in production; this one was
	// seeded Ready, so the turn waits for the ready to be stored.
	waitUntil(t, scenario24Wait, func() bool {
		sb, err := r.h.Sandboxes.Get(ctx, r.sessionID)
		return err == nil && sb.ReviewCheckoutGen != nil && *sb.ReviewCheckoutGen == sb.Gen
	})
	return agent
}

// createReviewTurn creates a pending review turn recording head, and asks
// for its dispatch the way the turn's creation does.
func (r *scenario24Rig) createReviewTurn(ctx context.Context, t *testing.T, registry *sessionactor.Registry, head string) pgtype.UUID {
	t.Helper()
	prompt := "review the pull request at " + head
	created, err := r.h.Turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: r.sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt, ReviewHeadSha: &head, IsReviewAttempt: true,
	})
	if err != nil {
		t.Fatalf("create the review turn: %v", err)
	}
	actor, err := registry.GetOrSpawn(ctx, r.sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	if err := actor.Send(ctx, sessionactor.EnsureDispatched{}); err != nil {
		t.Fatalf("EnsureDispatched: %v", err)
	}
	return created.ID
}

func (r *scenario24Rig) turn(ctx context.Context, t *testing.T, id pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	got, err := r.h.Turns.Get(ctx, id)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	return got
}

func (r *scenario24Rig) waitCompleted(ctx context.Context, t *testing.T, id pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	waitUntil(t, scenario24Wait, func() bool { return r.turn(ctx, t, id).Status == sqlcgen.TurnStatusCompleted })
	return r.turn(ctx, t, id)
}

// waitReady waits for the sandbox to be ready again after a turn's
// snapshot.
func (r *scenario24Rig) waitReady(ctx context.Context, t *testing.T) {
	t.Helper()
	waitUntil(t, scenario24Wait, func() bool {
		sb, err := r.h.Sandboxes.Get(ctx, r.sessionID)
		return err == nil && sb.Status == sqlcgen.SandboxStatusReady && sb.PendingSnapshotMessageID == nil
	})
}

// TestResilience_Scenario24_WarmReReview_EachTurnChecksOutItsOwnHead: the
// first review turn finds the head it recorded, S1, in its tree; it leaves
// edits behind -- a tracked change, a staged one, a deleted file, an
// untracked file and an ignored one -- the contributor pushes S2, and the
// re-review, on the same gen, finds S2 in a clean tree, the ignored file
// kept. Each turn's prompt was sent only after its checkout's reply, and
// each records the commit it was checked out at.
func TestResilience_Scenario24_WarmReReview_EachTurnChecksOutItsOwnHead(t *testing.T) {
	ctx := context.Background()
	rig := newScenario24Rig(ctx, t)
	registry := rig.newReplicaBackend(ctx, t)
	agent := rig.startAgent(ctx, t)
	worktree := agent.worktree

	agent.mu.Lock()
	agent.afterPrompt = func() {
		if err := os.WriteFile(filepath.Join(worktree, "pr.txt"), []byte("a turn's edit\n"), 0o644); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
			t.Error(err)
		}
		if out, err := exec.Command("git", "-C", worktree, "add", "staged.txt").CombinedOutput(); err != nil {
			t.Errorf("stage a file: %v\n%s", err, out)
		}
		if err := os.Remove(filepath.Join(worktree, "README.md")); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, "untracked.txt"), []byte("left\n"), 0o644); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, "deps.log"), []byte("installed\n"), 0o644); err != nil {
			t.Error(err)
		}
	}
	agent.mu.Unlock()

	first := rig.createReviewTurn(ctx, t, registry, rig.origin.s1)
	got := rig.waitCompleted(ctx, t, first)
	if got.CheckedOutSha == nil || *got.CheckedOutSha != rig.origin.s1 {
		t.Fatalf("first turn checked out %v, want S1 %s", got.CheckedOutSha, rig.origin.s1)
	}
	rig.waitReady(ctx, t)
	if status := statusIn(worktree); status == "" {
		t.Fatal("the first turn left no edit behind; the scenario needs a dirty tree")
	}

	rig.origin.advance(t, rig.origin.s2)
	second := rig.createReviewTurn(ctx, t, registry, rig.origin.s2)
	got = rig.waitCompleted(ctx, t, second)
	if got.CheckedOutSha == nil || *got.CheckedOutSha != rig.origin.s2 || got.DispatchedSandboxGen == nil || *got.DispatchedSandboxGen != 1 {
		t.Fatalf("second turn checked out %v on gen %v, want S2 %s on the same gen 1", got.CheckedOutSha, got.DispatchedSandboxGen, rig.origin.s2)
	}

	prompts := agent.seenPrompts()
	if len(prompts) != 2 {
		t.Fatalf("%d prompts, want 2", len(prompts))
	}
	if prompts[0].head != rig.origin.s1 || prompts[0].status != "" {
		t.Fatalf("the first prompt found %s with status %q, want S1 %s in a clean tree", prompts[0].head, prompts[0].status, rig.origin.s1)
	}
	if prompts[1].head != rig.origin.s2 || prompts[1].status != "" {
		t.Fatalf("the re-review found %s with status %q, want S2 %s in a clean tree: the previous turn's edits discarded", prompts[1].head, prompts[1].status, rig.origin.s2)
	}
	if _, err := os.Stat(filepath.Join(worktree, "deps.log")); err != nil {
		t.Fatalf("the ignored file is gone (%v), want it kept: setup's work survives a checkout", err)
	}
	checkouts := agent.seenCheckouts()
	if len(checkouts) != 2 || checkouts[0].Repos[0].Sha != rig.origin.s1 || checkouts[1].Repos[0].Sha != rig.origin.s2 ||
		checkouts[0].Repos[0].Ref != scenario24Ref || checkouts[0].Gen != 1 || checkouts[1].Gen != 1 {
		t.Fatalf("checkouts %+v, want S1 then S2, on %s, gen 1", checkouts, scenario24Ref)
	}
	// The order on the wire: each turn's checkout reply is stored before its
	// prompt was sent, so before the turn's dispatch watermark.
	for i, id := range []pgtype.UUID{first, second} {
		turnRow := rig.turn(ctx, t, id)
		var replyID int64
		if err := rig.h.Pool.QueryRow(ctx, `SELECT id FROM events WHERE session_id = $1 AND message_id = $2`,
			rig.sessionID, "checkout_result:"+checkouts[i].MessageId).Scan(&replyID); err != nil {
			t.Fatalf("turn %d's checkout reply: %v", i+1, err)
		}
		if turnRow.DispatchedEventID == nil || replyID > *turnRow.DispatchedEventID {
			t.Fatalf("turn %d dispatched at watermark %v, before its checkout reply (event %d)", i+1, turnRow.DispatchedEventID, replyID)
		}
	}
}

// TestResilience_Scenario24_ControlPlaneRestartBetweenSendAndReply_PromptSentOnce:
// the replica that sent the checkout goes away before the reply lands. The
// sandbox reconnects to another replica, whose actor finds the request
// recorded and the reconnect counted, and asks again; the reply that was
// in flight is stored too, under its own key. The prompt is sent once, on
// the head the turn recorded.
func TestResilience_Scenario24_ControlPlaneRestartBetweenSendAndReply_PromptSentOnce(t *testing.T) {
	ctx := context.Background()
	rig := newScenario24Rig(ctx, t)
	first := rig.newReplicaBackend(ctx, t)
	agent := rig.startAgent(ctx, t)
	release := agent.holdNextCheckout()
	t.Cleanup(release)

	turnID := rig.createReviewTurn(ctx, t, first, rig.origin.s1)
	waitUntil(t, scenario24Wait, func() bool { return len(agent.seenCheckouts()) == 1 })
	if got := rig.turn(ctx, t, turnID); got.Status != sqlcgen.TurnStatusPending || got.CheckoutMessageID == nil {
		t.Fatalf("turn after the send: %s, request %v; want pending with the request recorded", got.Status, got.CheckoutMessageID)
	}

	// The replica that sent it shuts down; the sandbox's socket goes with it,
	// and it reconnects to the next one.
	if err := first.Shutdown(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("shut the first replica down: %v", err)
	}
	rig.newReplicaBackend(ctx, t)
	rig.proxy.sever()
	release()

	waitUntil(t, scenario24Wait, func() bool { return rig.turn(ctx, t, turnID).Status == sqlcgen.TurnStatusCompleted })

	prompts := agent.seenPrompts()
	if len(prompts) != 1 {
		t.Fatalf("%d prompts, want the turn's one", len(prompts))
	}
	if prompts[0].head != rig.origin.s1 {
		t.Fatalf("the prompt found %s, want the recorded head S1 %s", prompts[0].head, rig.origin.s1)
	}
	got := rig.turn(ctx, t, turnID)
	if got.CheckedOutSha == nil || *got.CheckedOutSha != rig.origin.s1 {
		t.Fatalf("checked out %v, want S1 %s", got.CheckedOutSha, rig.origin.s1)
	}
	// The reconnect's ready is the new connection's first frame, so the
	// second replica asks again before the reply in flight is replayed;
	// both replies are stored, each under its own command's key.
	checkouts := agent.seenCheckouts()
	if len(checkouts) != 2 || checkouts[0].MessageId == checkouts[1].MessageId {
		t.Fatalf("checkouts %+v, want the first replica's and the one the second asked again after the reconnect", checkouts)
	}
	for _, c := range checkouts {
		if n := countEventsByMessageID(ctx, t, rig.h, rig.sessionID, "checkout_result:"+c.MessageId); n != 1 {
			t.Fatalf("%d stored replies to %s, want 1", n, c.MessageId)
		}
	}
	var syntheticEnds int
	if err := rig.h.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'execution_complete' AND payload ? 'synthetic'`, rig.sessionID).Scan(&syntheticEnds); err != nil {
		t.Fatal(err)
	}
	if syntheticEnds != 0 {
		t.Fatalf("%d synthetic ends, want none: the turn ran and completed", syntheticEnds)
	}
}
