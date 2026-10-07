//go:build integration

// Resilience scenario #24, its private-fork half (§9.3, technical plan
// §21.1, §30.4, docs/IMPLEMENTATION_PLAN.md row 204): a pull request from a
// private fork whose owner never installed the App is reviewed, at the head
// its review turn recorded.
//
// Everything on the control-plane side is real: the GitHub ingress handler
// receiving a signed webhook, the session it opens, the actor that spawns
// its sandbox through a provider, the SESSION_CONFIG that spawn delivers,
// the sandbox WS handler, and the checkout gate. The sandbox is a real
// wsbridge.Bridge whose workspace is booted by the agent's own
// gitclone.CloneAll from that SESSION_CONFIG, and whose checkout runs the
// agent's own gitclone.CheckoutPullRef. The code host is git-http-backend
// over TLS serving the base repository, acme/widgets, which keeps the pull
// request's head as refs/pull/7/head; the contributor's fork,
// contributor/widgets, holds the head too, on a branch named main like the
// base's own, but every request for it answers 404 -- the fork is private,
// and nothing Narvi holds may read it.
package resilience_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/gitclone"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

const (
	forkWebhookSecret = "scenario24-fork-webhook-secret"
	forkBotHandle     = "narvi-bot"
)

// forkOrigin is the code host: the base repository served, the fork
// refused, and how often anything asked for the fork.
type forkOrigin struct {
	baseURL      string
	s1           string
	baseMain     string
	forkRequests atomic.Int64
}

func newForkOrigin(t *testing.T) *forkOrigin {
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
	base := filepath.Join(parent, "acme", "widgets.git")
	fork := filepath.Join(parent, "contributor", "widgets.git")
	for _, bare := range []string{base, fork} {
		if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
			t.Fatal(err)
		}
		gitIn(t, parent, "init", "--bare", "-q", "-b", "main", bare)
	}
	commit := func(work, file, content, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, work, "add", ".")
		gitIn(t, work, "commit", "-q", "-m", message)
		return gitIn(t, work, "rev-parse", "HEAD")
	}
	o := &forkOrigin{}

	// The base repository: main.
	baseWork := filepath.Join(t.TempDir(), "base")
	gitIn(t, parent, "clone", "-q", base, baseWork)
	gitIn(t, baseWork, "config", "user.email", "test@example.com")
	gitIn(t, baseWork, "config", "user.name", "Test")
	commit(baseWork, "README.md", "base\n", "base")
	o.baseMain = commit(baseWork, "main-only.txt", "main\n", "main moves on")
	gitIn(t, baseWork, "push", "-q", "origin", "main")

	// The fork: its own main carries the contributor's commit, S1. The base
	// keeps the pull request's head as refs/pull/7/head, as the code host
	// does when the pull request is opened.
	forkWork := filepath.Join(t.TempDir(), "fork")
	gitIn(t, parent, "clone", "-q", base, forkWork)
	gitIn(t, forkWork, "config", "user.email", "contributor@example.com")
	gitIn(t, forkWork, "config", "user.name", "Contributor")
	gitIn(t, forkWork, "reset", "-q", "--hard", "HEAD~1")
	o.s1 = commit(forkWork, "pr.txt", "the contributor's change\n", "the pull request's head")
	gitIn(t, forkWork, "push", "-q", fork, "HEAD:refs/heads/main")
	gitIn(t, forkWork, "push", "-q", base, o.s1+":"+scenario24Ref)

	backendHandler := &cgi.Handler{
		Path: backend, Root: "/",
		Env: []string{"GIT_HTTP_EXPORT_ALL=1", "GIT_PROJECT_ROOT=" + parent},
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/contributor/") {
			o.forkRequests.Add(1)
			http.NotFound(w, r)
			return
		}
		backendHandler.ServeHTTP(w, r)
	}))
	server.StartTLS()
	t.Cleanup(server.Close)
	o.baseURL = server.URL + "/acme/widgets.git"
	return o
}

// specProvider is a ports.SandboxProvider that records the spec of each
// sandbox it is asked to create, as a provider hands it to the sandbox.
type specProvider struct {
	mu    sync.Mutex
	specs []ports.CreateSpec
}

var _ ports.SandboxProvider = (*specProvider)(nil)

func (p *specProvider) Capabilities() ports.Capabilities { return ports.Capabilities{} }
func (p *specProvider) CreateSandbox(_ context.Context, spec ports.CreateSpec) (ports.SandboxRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.specs = append(p.specs, spec)
	return ports.SandboxRef{ProviderID: uuid.NewString()}, nil
}
func (p *specProvider) StopSandbox(context.Context, ports.SandboxRef) error   { return nil }
func (p *specProvider) ResumeSandbox(context.Context, ports.SandboxRef) error { return nil }
func (p *specProvider) TakeSnapshot(context.Context, ports.SandboxRef) (ports.SnapshotID, error) {
	return "", fmt.Errorf("specProvider: no snapshots")
}
func (p *specProvider) RestoreFromSnapshot(context.Context, ports.SnapshotID, ports.CreateSpec) (ports.SandboxRef, error) {
	return ports.SandboxRef{}, fmt.Errorf("specProvider: no snapshots")
}
func (p *specProvider) BuildImage(context.Context, ports.ImageSpec) (ports.BuildOutcome, error) {
	return ports.BuildOutcome{}, fmt.Errorf("specProvider: no image builds")
}
func (p *specProvider) DeleteImage(context.Context, ports.ImageRef) error { return nil }
func (p *specProvider) List(context.Context) ([]ports.SandboxRef, error)  { return nil, nil }

func (p *specProvider) created() []ports.CreateSpec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ports.CreateSpec(nil), p.specs...)
}

// forkPullRequest is the code host's API answer for the pull request: its
// head in the fork, on a branch named like the base's main, at S1.
type forkPullRequest struct {
	head, baseMain string
}

func (f forkPullRequest) GetPullRequest(context.Context, string, string, int32, string) (githubapi.PullRequest, error) {
	return githubapi.PullRequest{Title: "A contributor's change", HeadRef: "main", HeadSHA: f.head, HeadRepoFullName: "contributor/widgets", BaseRef: "main"}, nil
}
func (f forkPullRequest) GetCompareDiff(context.Context, string, string, string, string, string) (string, bool, error) {
	return "+ the contributor's change", false, nil
}
func (f forkPullRequest) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	return f.baseMain, spec.Branch, nil
}
func (f forkPullRequest) GetPullRequestDiff(context.Context, string, string, int32, string) (string, bool, error) {
	return "+ the contributor's change", false, nil
}

// newForkIngress mounts the real GitHub webhook handler, opening review
// sessions on registry, with forkPullRequest as the code host's API.
func newForkIngress(t *testing.T, h *Harness, registry *sessionactor.Registry, pr forkPullRequest) *httptest.Server {
	t.Helper()
	coalescer := &githubingress.SessionCoalescer{
		Pool:         h.Pool,
		PRSessions:   narvipg.NewGitHubPRSessionStore(h.Pool),
		Sessions:     h.Sessions,
		Turns:        h.Turns,
		Environments: narvipg.NewEnvironmentStore(h.Pool),
		Registry:     registry,
		SessionGuard: turnguard.New(h.Pool, nil, false),
		AuditLog:     narvipg.NewAuditLogStore(h.Pool),
		Plans:        narvipg.NewPlanStore(h.Pool),
		Identities:   narvipg.NewIdentityStore(h.Pool),
		Users:        narvipg.NewUserStore(h.Pool),
		Participants: narvipg.NewParticipantStore(h.Pool),
	}
	handler, err := githubingress.NewHandler(coalescer, narvipg.NewWebhookDeliveryStore(h.Pool), githubingress.Config{
		WebhookSecret: forkWebhookSecret,
		BotHandle:     forkBotHandle,
		Outbound:      platform.MustNewGitHubOutboundConfig("test-bot-token"),
		DiffFetcher:   pr,
		PullRequests:  pr,
		Timeouts:      h.Timeouts,
	})
	if err != nil {
		t.Fatalf("githubingress.NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// linkGitHubCommenter makes commenterID a maintainer linked to Narvi, as a
// GitHub sign-in does.
func linkGitHubCommenter(ctx context.Context, t *testing.T, h *Harness, commenterID int64) {
	t.Helper()
	email := fmt.Sprintf("scenario24-fork-%d@example.com", commenterID)
	user, err := narvipg.NewUserStore(h.Pool).Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: email, DisplayName: "Maintainer", Role: sqlcgen.UserRoleMaintainer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := narvipg.NewIdentityStore(h.Pool).Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: strconv.FormatInt(commenterID, 10),
		Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail,
	}); err != nil {
		t.Fatal(err)
	}
}

// postForkReviewComment delivers a signed pull_request_review_comment
// mentioning the bot on acme/widgets#7, whose head is in the fork.
func postForkReviewComment(t *testing.T, server *httptest.Server, origin *forkOrigin, commenterID int64) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"action": "created",
		"comment": map[string]any{
			"id":   int64(7001),
			"body": "@" + forkBotHandle + " please review",
			"user": map[string]any{"id": commenterID, "login": "maintainer"},
		},
		"pull_request": map[string]any{
			"number": scenario24Number,
			"head": map[string]any{"ref": "main", "sha": origin.s1, "repo": map[string]any{
				"name": "widgets", "full_name": "contributor/widgets", "clone_url": strings.Replace(origin.baseURL, "/acme/", "/contributor/", 1),
			}},
		},
		"repository": map[string]any{"full_name": scenario24Claim, "name": "widgets", "clone_url": origin.baseURL},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/webhooks/github", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(forkWebhookSecret))
	mac.Write(body)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Event", "pull_request_review_comment")
	req.Header.Set("X-GitHub-Delivery", "scenario24-fork-delivery-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200", resp.StatusCode)
	}
}

// TestResilience_Scenario24_PrivateForkWithoutTheApp_ReviewedAtItsRecordedHead:
// the webhook opens the review session on the base repository; the spawn
// hands the sandbox a SESSION_CONFIG naming the base and the pull
// request's ref; the agent boots from it and, asked by the checkout gate,
// checks out the head the turn recorded, S1, read from the base's pull ref;
// only then is the prompt sent, and the turn records S1 as the commit it
// was checked out at. The fork is never asked for anything, and the tree
// the review read is S1's, never the base's own main -- the branch name the
// fork's head carries.
func TestResilience_Scenario24_PrivateForkWithoutTheApp_ReviewedAtItsRecordedHead(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	origin := newForkOrigin(t)

	commander := wshub.NewSandboxRegistry(h.Timeouts)
	provider := &specProvider{}
	registry, err := sessionactor.NewRegistry(ctx, h.Pool, h.Timeouts, h.Hub, commander, provider, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("sessionactor.NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })
	router := chi.NewRouter()
	router.Get("/sessions/{sessionID}/ws", wshub.NewSandboxHandler(registry, h.Sandboxes, commander, h.Timeouts))
	controlPlane := httptest.NewServer(router)
	t.Cleanup(controlPlane.Close)

	const commenterID = 92400001
	linkGitHubCommenter(ctx, t, h, commenterID)
	ingress := newForkIngress(t, h, registry, forkPullRequest{head: origin.s1, baseMain: origin.baseMain})
	postForkReviewComment(t, ingress, origin, commenterID)

	// The session the webhook opened names the base repository, with no
	// branch, and its turn records S1.
	var sessionID pgtype.UUID
	var stored string
	if err := h.Pool.QueryRow(ctx, `SELECT id, repos::text FROM sessions WHERE spawn_source = 'github'`).Scan(&sessionID, &stored); err != nil {
		t.Fatalf("read the review session: %v", err)
	}
	if want := `[{"url": "` + origin.baseURL + `", "name": "widgets", "branch": null}]`; stored != want {
		t.Fatalf("the review session's spec = %s, want %s", stored, want)
	}
	var turnID pgtype.UUID
	var recorded *string
	if err := h.Pool.QueryRow(ctx, `SELECT id, review_head_sha FROM turns WHERE session_id = $1`, sessionID).Scan(&turnID, &recorded); err != nil {
		t.Fatalf("read the review turn: %v", err)
	}
	if recorded == nil || *recorded != origin.s1 {
		t.Fatalf("the turn recorded head %v, want S1 %s", recorded, origin.s1)
	}

	// The spawn's SESSION_CONFIG: the base repository at the pull request's
	// ref.
	waitUntil(t, scenario24Wait, func() bool { return len(provider.created()) == 1 })
	spec := provider.created()[0]
	repos := spec.SessionConfig.Repos
	if len(repos) != 1 || repos[0].Url != origin.baseURL || repos[0].Branch != nil || repos[0].Ref == nil || *repos[0].Ref != scenario24Ref {
		t.Fatalf("SESSION_CONFIG repos = %+v, want the base repository at %s, no branch", repos, scenario24Ref)
	}

	// The agent boots from it: the base repository, at the ref's tip.
	layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: t.TempDir()}
	results, err := gitclone.CloneAll(ctx, supervisor.New(), layout, nil, nil, repos, nil, h.Timeouts.RepoCloneTimeout, h.Timeouts.ProcessStopGracePeriod)
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("boot clone = %+v, %v", results, err)
	}
	worktree := filepath.Join(layout.WorkspaceDir, "widgets")

	runCtx, cancel := context.WithCancel(ctx)
	group := &errgroup.Group{}
	agent := &checkoutAgent{ctx: runCtx, group: group, layout: layout, repo: repos[0], worktree: worktree, timeouts: h.Timeouts}
	sc := spec.SessionConfig
	sc.ControlPlaneWsUrl = "ws" + strings.TrimPrefix(controlPlane.URL, "http") + "/sessions/" + sessionID.String() + "/ws?type=sandbox"
	sc.Repos = []sessionconfig.SessionConfigReposElem{repos[0]}
	agent.bridge = wsbridge.New(sc, "sbx-scenario24-fork", "test-agent-version", "test-image-digest", agent,
		ackTestDialTimeout, ackTestHeartbeat, ackTestMinBackoff, ackTestMaxBackoff)
	agent.bridge.ReportBootStarted()
	group.Go(func() error { return agent.bridge.Run(runCtx) })
	t.Cleanup(func() {
		cancel()
		if err := group.Wait(); err != nil {
			t.Errorf("agent: %v, want nil after ctx cancellation", err)
		}
	})
	agent.bridge.MarkBootComplete()

	waitUntil(t, scenario24Wait, func() bool {
		got, err := h.Turns.Get(ctx, turnID)
		return err == nil && got.Status == sqlcgen.TurnStatusCompleted
	})
	got, err := h.Turns.Get(ctx, turnID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CheckedOutSha == nil || *got.CheckedOutSha != origin.s1 {
		t.Fatalf("the turn was checked out at %v, want S1 %s", got.CheckedOutSha, origin.s1)
	}
	prompts := agent.seenPrompts()
	if len(prompts) != 1 || prompts[0].head != origin.s1 || prompts[0].status != "" {
		t.Fatalf("prompts %+v, want one, finding S1 %s in a clean tree", prompts, origin.s1)
	}
	checkouts := agent.seenCheckouts()
	if len(checkouts) != 1 || checkouts[0].Repos[0].Ref != scenario24Ref || checkouts[0].Repos[0].Sha != origin.s1 {
		t.Fatalf("checkouts %+v, want one of S1 on %s", checkouts, scenario24Ref)
	}
	if n := origin.forkRequests.Load(); n != 0 {
		t.Errorf("%d requests reached the private fork, want none", n)
	}
	if head := headIn(worktree); head != origin.s1 || head == origin.baseMain {
		t.Errorf("the worktree is at %s, want the pull request's head S1 %s, never the base's main %s", head, origin.s1, origin.baseMain)
	}
}
