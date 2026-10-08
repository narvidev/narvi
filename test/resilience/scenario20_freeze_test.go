//go:build integration

// Resilience scenario #20, the freeze half (§9.3, technical plan §40.2,
// docs/IMPLEMENTATION_PLAN.md row 149): "the freeze flipped with
// auto-merge candidates pending -> no merge, no auto-fix spawn, no
// re-review enqueue and no automation invocation occurs while frozen,
// every candidate is still a candidate after unfreeze, a human command
// still works, and no running turn is severed." The cap half is Step 148's.
//
// One database, the real components the control plane runs: a session
// actor registry whose sandboxes are real wsbridge agents on the real
// sandbox WS handler, the auto-merge worker, the outbox builder with the
// sentinel auto-fix notifier, the automation engine, the held workflow
// advance releaser, and the admin routes that freeze and unfreeze --
// driven by hand, tick by tick, so each site's tick is the test's to
// place. A candidate of every automatic kind is pending when an
// administrator freezes autonomy through the admin action: an armed
// auto-merge, a sentinel auto-fix delivery, a debounced re-review, a cron
// schedule, an invocation recorded as an event's is, and a workflow step
// whose turn ends while frozen. While frozen none starts, a person's
// prompt is dispatched and completes, and a turn processing since before
// the freeze is left alone. After the unfreeze each candidate runs exactly
// once, the processing turn completes on its own, and no turn anywhere
// failed or was cancelled.
package resilience_test

import (
	"bytes"
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
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/automation"
	"github.com/narvidev/narvi/internal/app/automerge"
	"github.com/narvidev/narvi/internal/app/autonomy"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

const (
	scenario20Wait    = 15 * time.Second
	scenario20BaseRef = "main"
	scenario20BaseSHA = "sha-s20-base"
)

// scenario20CodeHost is the code host every automatic site reads: the
// armed pull request the auto-merge worker re-validates and merges -- open
// until merged, as a real one is -- and the branch the sentinel auto-fix
// creates. It counts every read and write a site makes.
type scenario20CodeHost struct {
	mu             sync.Mutex
	pr             ports.OpenPR
	merged         bool
	getOpenPRs     int
	merges         int
	branchesMade   int
	notImplemented []string
}

var _ ports.SourceControl = (*scenario20CodeHost)(nil)

func (c *scenario20CodeHost) refuse(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notImplemented = append(c.notImplemented, name)
	return fmt.Errorf("scenario20CodeHost: %s not implemented", name)
}

func (c *scenario20CodeHost) GetOpenPR(_ context.Context, owner, repo string, number int, _ string) (ports.OpenPR, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getOpenPRs++
	if c.merged || owner != c.pr.Owner || repo != c.pr.Repo || number != c.pr.Number {
		return ports.OpenPR{}, false, nil
	}
	return c.pr, true, nil
}

func (c *scenario20CodeHost) MergePR(context.Context, ports.MergePRSpec) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.merges++
	c.merged = true
	return "sha-s20-merged", nil
}

func (c *scenario20CodeHost) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	if spec.Branch == scenario20BaseRef {
		return scenario20BaseSHA, spec.Branch, nil
	}
	return "sha-s20-fix-base", spec.Branch, nil
}

func (c *scenario20CodeHost) CreateBranch(context.Context, ports.CreateBranchSpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.branchesMade++
	return nil
}

func (c *scenario20CodeHost) IsAncestor(context.Context, ports.IsAncestorSpec) (bool, error) {
	return false, nil
}

func (c *scenario20CodeHost) ListRequiredChecks(context.Context, ports.ListRequiredChecksSpec) ([]ports.RequiredCheck, error) {
	return nil, nil
}

func (c *scenario20CodeHost) ResolveAppID(_ context.Context, spec ports.ResolveAppIDSpec) (int64, error) {
	return 0, fmt.Errorf("scenario20CodeHost: slug %s: %w", spec.Slug, ports.ErrAppNotFound)
}

func (c *scenario20CodeHost) CreatePR(context.Context, ports.CreatePRSpec) (ports.PRRef, error) {
	return ports.PRRef{}, c.refuse("CreatePR")
}

func (c *scenario20CodeHost) ResolveContractsFingerprint(context.Context, ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return "", false, c.refuse("ResolveContractsFingerprint")
}

func (c *scenario20CodeHost) CheckRepoAccess(context.Context, ports.CheckRepoAccessSpec) (bool, error) {
	return false, c.refuse("CheckRepoAccess")
}

func (c *scenario20CodeHost) GetFileContent(context.Context, ports.GetFileContentSpec) (string, string, bool, error) {
	return "", "", false, c.refuse("GetFileContent")
}

func (c *scenario20CodeHost) UpdateFileContent(context.Context, ports.UpdateFileContentSpec) (string, error) {
	return "", c.refuse("UpdateFileContent")
}

func (c *scenario20CodeHost) GetPRBody(context.Context, string, string, int, string) (string, bool, error) {
	return "", false, c.refuse("GetPRBody")
}

func (c *scenario20CodeHost) UpdatePRBody(context.Context, ports.UpdatePRBodySpec) error {
	return c.refuse("UpdatePRBody")
}

func (c *scenario20CodeHost) RegisterPRStack(context.Context, ports.RegisterPRStackSpec) error {
	return c.refuse("RegisterPRStack")
}

func (c *scenario20CodeHost) ListMergedBetween(context.Context, ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return nil, false, c.refuse("ListMergedBetween")
}

func (c *scenario20CodeHost) ListOpenPRsForUser(context.Context, ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	return nil, false, c.refuse("ListOpenPRsForUser")
}

func (c *scenario20CodeHost) ResolveCodeOwners(context.Context, ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	return nil, c.refuse("ResolveCodeOwners")
}

func (c *scenario20CodeHost) counts() (getOpenPRs, merges, branchesMade int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getOpenPRs, c.merges, c.branchesMade
}

// freezeAgent is one sandbox's agent: a real wsbridge.Bridge on the real
// sandbox WS handler. It answers every snapshot command, counts every stop
// command, and completes each prompt at once when auto is set, otherwise
// when the test says -- a turn left processing for as long as the test
// needs.
type freezeAgent struct {
	noopCommandHandler
	ctx    context.Context
	bridge *wsbridge.Bridge

	mu      sync.Mutex
	auto    bool
	prompts []sandboxws.Prompt
	stops   int
}

func (a *freezeAgent) HandlePrompt(_ context.Context, cmd sandboxws.Prompt) {
	a.mu.Lock()
	a.prompts = append(a.prompts, cmd)
	auto := a.auto
	a.mu.Unlock()
	if auto {
		a.complete(cmd)
	}
}

func (a *freezeAgent) HandleStop(context.Context, sandboxws.Stop) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stops++
}

func (a *freezeAgent) HandleSnapshot(_ context.Context, cmd sandboxws.Snapshot) {
	id := "sr-" + cmd.MessageId
	commandID := cmd.MessageId
	_ = a.bridge.SendCritical(a.ctx, sandboxws.SnapshotReady{
		Type: "snapshot_ready", MessageId: id, SessionId: cmd.SessionId, Gen: cmd.Gen,
		AckId: "snapshot_ready:" + id, SnapshotId: "snap-" + cmd.MessageId, CommandMessageId: &commandID,
	}, "snapshot_ready:"+id)
}

// complete ends cmd's run with an execution_complete, as the agent runtime
// does.
func (a *freezeAgent) complete(cmd sandboxws.Prompt) {
	id := "ec-" + cmd.MessageId
	_ = a.bridge.SendCritical(a.ctx, sandboxws.ExecutionComplete{
		Type: "execution_complete", MessageId: id, SessionId: cmd.SessionId, Gen: cmd.Gen,
		AckId: "execution_complete:" + id, Outcome: sandboxws.ExecutionCompleteOutcomeCompleted,
	}, "execution_complete:"+id)
}

// completeLast completes the last prompt the agent received.
func (a *freezeAgent) completeLast(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	if len(a.prompts) == 0 {
		a.mu.Unlock()
		t.Fatal("completeLast: the agent has received no prompt")
	}
	last := a.prompts[len(a.prompts)-1]
	a.mu.Unlock()
	a.complete(last)
}

func (a *freezeAgent) setAuto(auto bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.auto = auto
}

func (a *freezeAgent) counts() (prompts, stops int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.prompts), a.stops
}

// scenario20 is the rig: the harness, the registry and its WS server, the
// admin routes, and every automatic component.
type scenario20 struct {
	h          *Harness
	registry   *sessionactor.Registry
	wsURL      string
	adminURL   string
	adminToken string
	guard      *turnguard.Guard
	inboxDeps  decisioninbox.Deps
	auditLog   *narvipg.AuditLogStore
}

func newScenario20(ctx context.Context, t *testing.T) *scenario20 {
	t.Helper()
	h := newHarness(t)
	commander := wshub.NewSandboxRegistry(h.Timeouts)
	review := movedPR{head: "sha-s20-pushed", diff: "+ a line the push added"}
	registry, err := sessionactor.NewRegistry(ctx, h.Pool, h.Timeouts, h.Hub, commander, &recordingProvider{}, "http://localhost:8080", nil, nil, "", nil, false,
		sessionactor.RegistryOptions{
			ReviewDiffFetcher: review, ReviewLiveReader: review, GitHubBotHandle: "narvi-bot",
			GitHubOutbound:          platform.MustNewGitHubOutboundConfig("s20-bot-token"),
			ReviewRequestAuthorizer: &allowingAuthorizer{},
		})
	if err != nil {
		t.Fatalf("sessionactor.NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	wsRouter := chi.NewRouter()
	wsRouter.Get("/sessions/{sessionID}/ws", wshub.NewSandboxHandler(registry, h.Sandboxes, commander, h.Timeouts))
	wsServer := httptest.NewServer(wsRouter)
	t.Cleanup(wsServer.Close)

	// The admin action, mounted as controlplane/serve.go mounts it.
	users := narvipg.NewUserStore(h.Pool)
	userSessions := narvipg.NewUserSessionStore(h.Pool)
	auditLog := narvipg.NewAuditLogStore(h.Pool)
	settings := narvipg.NewPlatformSettingsStore(h.Pool)
	adminRouter := chi.NewRouter()
	adminRouter.Route("/api/autonomy", func(r chi.Router) {
		r.Use(auth.Middleware(userSessions, users))
		r.Get("/", httpapi.GetAutonomyFreeze(settings))
		r.Post("/freeze", httpapi.PostFreezeAutonomy(h.Pool, settings, auditLog))
		r.Post("/unfreeze", httpapi.PostUnfreezeAutonomy(h.Pool, settings, auditLog))
	})
	adminServer := httptest.NewServer(adminRouter)
	t.Cleanup(adminServer.Close)
	admin, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "s20-admin@example.com", DisplayName: "Scenario Admin", Role: sqlcgen.UserRoleAdmin})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if _, err := userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID: admin.ID, TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("create admin session: %v", err)
	}

	reviewFindings := narvipg.NewReviewFindingStore(h.Pool)
	repoSettings := narvipg.NewRepoSettingsStore(h.Pool)
	return &scenario20{
		h: h, registry: registry, wsURL: "ws" + strings.TrimPrefix(wsServer.URL, "http"), adminURL: adminServer.URL, adminToken: token,
		guard:    turnguard.New(h.Pool, nil, false),
		auditLog: auditLog,
		inboxDeps: decisioninbox.Deps{
			GitHubOutbound: platform.MustNewGitHubOutboundConfig("s20-bot-token"),
			Plans:          narvipg.NewPlanStore(h.Pool), Sessions: h.Sessions, Participants: narvipg.NewParticipantStore(h.Pool),
			Automations: narvipg.NewAutomationStore(h.Pool), Outbox: narvipg.NewOutboxStore(h.Pool, false),
			ReviewFindings: reviewFindings, SentinelFixes: narvipg.NewSentinelFixStore(h.Pool),
			Artifacts: narvipg.NewArtifactStore(h.Pool), Identities: narvipg.NewIdentityStore(h.Pool),
			PlatformSettings: settings, Workflows: narvipg.NewWorkflowStore(h.Pool),
			Timeouts: h.Timeouts,
			ReviewVerdict: appreviewverdict.Deps{
				ReviewVerdicts: narvipg.NewReviewVerdictStore(h.Pool), RepoSettings: repoSettings, ReviewFindings: reviewFindings,
				AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(h.Pool), Acceptances: narvipg.NewReviewVerdictAcceptanceStore(h.Pool),
				Turns: h.Turns, Timeouts: h.Timeouts,
			},
		},
	}
}

// admin calls one admin route and returns its status.
func (s *scenario20) admin(t *testing.T, path string, body string) int {
	t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(http.MethodPost, s.adminURL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: s.adminToken})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// sandboxSession creates a session with a ready sandbox at gen 1 and a
// connected agent, and waits for the agent's ready to land.
func (s *scenario20) sandboxSession(ctx context.Context, t *testing.T, repos string, auto bool) (pgtype.UUID, *freezeAgent) {
	t.Helper()
	params := sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb}
	if repos != "" {
		params.Repos = []byte(repos)
	}
	session, err := s.h.Sessions.Create(ctx, params)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := s.h.Sandboxes.Create(ctx, session.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := s.h.Sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: session.ID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	agent := &freezeAgent{ctx: runCtx, auto: auto}
	sc := sessionconfig.SessionConfig{
		BootMode:          sessionconfig.SessionConfigBootModeFresh,
		ControlPlaneWsUrl: s.wsURL + "/sessions/" + session.ID.String() + "/ws?type=sandbox",
		Gen:               1,
		SandboxToken:      "s20-test-token", // the fresh row's NULL token_hash admits any (scenario #7's note)
		SessionId:         session.ID.String(),
	}
	agent.bridge = wsbridge.New(sc, "sbx-s20-"+session.ID.String(), "s20-agent", "s20-image", agent,
		ackTestDialTimeout, ackTestHeartbeat, ackTestMinBackoff, ackTestMaxBackoff)
	var group errgroup.Group
	group.Go(func() error { return agent.bridge.Run(runCtx) })
	t.Cleanup(func() {
		cancel()
		if err := group.Wait(); err != nil {
			t.Errorf("bridge.Run() error = %v, want nil after ctx cancellation", err)
		}
	})
	waitUntil(t, scenario20Wait, func() bool {
		var version *string
		if err := s.h.Pool.QueryRow(ctx, `SELECT agent_version FROM sandboxes WHERE session_id = $1`, session.ID).Scan(&version); err != nil {
			t.Fatalf("read the sandbox: %v", err)
		}
		return version != nil && *version == "s20-agent"
	})
	return session.ID, agent
}

// prompt creates a person's turn on sessionID through the REST turn path's
// core, and returns it.
func (s *scenario20) prompt(ctx context.Context, t *testing.T, sessionID pgtype.UUID, text string) sqlcgen.Turn {
	t.Helper()
	created, _, cerr := httpapi.CreateTurnCore(ctx, s.h.Pool, s.h.Sessions, s.h.Turns, nil, nil, s.auditLog, s.registry, s.guard, sessionID,
		text, nil, false, false, pgtype.UUID{}, httpapi.RejectIfOpen)
	if cerr != nil {
		t.Fatalf("CreateTurnCore: %d %s", cerr.Status, cerr.Message)
	}
	return created
}

func (s *scenario20) turnStatus(ctx context.Context, t *testing.T, id pgtype.UUID) sqlcgen.TurnStatus {
	t.Helper()
	got, err := s.h.Turns.Get(ctx, id)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	return got.Status
}

func (s *scenario20) count(ctx context.Context, t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.h.Pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// armedMergeCandidate seeds an armed repository's eligible candidate: a
// platform-authored pull request with an auto-approved verdict at its head,
// in a live repository with auto-merge on. It returns the code host that
// answers for it.
func (s *scenario20) armedMergeCandidate(ctx context.Context, t *testing.T) *scenario20CodeHost {
	t.Helper()
	const repoFullName, number, head = "acme/s20-merge", 1, "sha-s20-merge-head"
	htmlURL := fmt.Sprintf("https://github.com/%s/pull/%d", repoFullName, number)
	author, err := s.h.Sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create the authoring session: %v", err)
	}
	if _, err := narvipg.NewArtifactStore(s.h.Pool).Create(ctx, sqlcgen.CreateArtifactParams{SessionID: author.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}")}); err != nil {
		t.Fatalf("record the pull request artifact: %v", err)
	}
	repoSettings := narvipg.NewRepoSettingsStore(s.h.Pool)
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote to live egress: %v", err)
	}
	verdict := review.Verdict{
		RiskLevel: review.RiskLevelLow, Premise: review.PremiseStateOK, TestsCoverage: review.TestsCoverageStateAdequate,
		DocsDrift: review.DocsDriftStateNone, ProposedShippable: review.ProposedShippableAuto, FilesChanged: 3,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone).Class()
	verdictContext := reviewverdict.Context{BaseRef: scenario20BaseRef, BaseSHA: scenario20BaseSHA, PolicyVersion: autoapproval.CurrentPolicyVersion}
	if _, err := appreviewverdict.Insert(ctx, narvipg.NewReviewVerdictStore(s.h.Pool), repoSettings, false, repoFullName, number, head, pgtype.UUID{}, verdict,
		reviewpost.Digest{Summary: "Scenario 20 verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, reviewpost.SecondFactCheck{}, nil, nil, "", false, verdictContext, pgtype.UUID{}); err != nil {
		t.Fatalf("seed the auto-approved verdict: %v", err)
	}
	if _, err := repoSettings.UpsertAutoMergeToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("arm auto-merge: %v", err)
	}
	return &scenario20CodeHost{pr: ports.OpenPR{
		Owner: "acme", Repo: "s20-merge", Number: number, HTMLURL: htmlURL, HeadSHA: head,
		BaseRef: scenario20BaseRef, BaseSHA: scenario20BaseSHA, CIConclusion: ports.CIConclusionSuccess,
	}}
}

// sentinelAutoFixDelivery enqueues a sentinel auto-fix delivery for a
// claimed fix and an open finding, and returns its row.
func (s *scenario20) sentinelAutoFixDelivery(ctx context.Context, t *testing.T) sqlcgen.Outbox {
	t.Helper()
	const repoFullName, prNumber = "acme/s20-fix", 7
	origin, err := s.h.Sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create the origin session: %v", err)
	}
	fix, err := narvipg.NewSentinelFixStore(s.h.Pool).Claim(ctx, repoFullName, prNumber, origin.ID, "feature-s20")
	if err != nil {
		t.Fatalf("claim the fix: %v", err)
	}
	const identityHash = "2020202020202020202020202020202020202020202020202020202020202020"
	if _, err := narvipg.NewReviewFindingStore(s.h.Pool).Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName, PrNumber: prNumber, IdentityHash: identityHash,
		Severity: "medium", FilePath: "internal/s20/fix.go", Description: "Missing test coverage.",
	}); err != nil {
		t.Fatalf("upsert the finding: %v", err)
	}
	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID: fix.ID.String(), RepoFullName: repoFullName, OriginPRNumber: prNumber,
		OriginReviewSessionID: origin.ID.String(), OriginHeadBranch: "feature-s20",
		RepoName: "s20-fix", RepoCloneURL: "https://github.com/acme/s20-fix.git",
		FindingIdentityHashes: []string{identityHash}, FindingDescriptions: []string{"Missing test coverage."},
	})
	if err != nil {
		t.Fatalf("marshal the payload: %v", err)
	}
	row, err := narvipg.NewOutboxStore(s.h.Pool, false).Create(ctx, sqlcgen.CreateOutboxEntryParams{SessionID: origin.ID, Kind: string(ports.NotificationKindSentinelAutoFix), Payload: payload})
	if err != nil {
		t.Fatalf("enqueue the sentinel auto-fix: %v", err)
	}
	return row
}

// debouncedReReview leaves a pushed head pending on an opted-in pull
// request's review session, with its debounce due: the automatic
// re-review's candidate.
func (s *scenario20) debouncedReReview(ctx context.Context, t *testing.T) pgtype.UUID {
	t.Helper()
	const repoFullName, prNumber = "acme/s20-review", 3
	session, err := s.h.Sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		Repos:       []byte(`[{"name":"s20-review","url":"https://github.com/acme/s20-review.git","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create the review session: %v", err)
	}
	prSessions := narvipg.NewGitHubPRSessionStore(s.h.Pool)
	if err := prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatal(err)
	}
	if err := prSessions.SetSessionID(ctx, repoFullName, prNumber, session.ID); err != nil {
		t.Fatal(err)
	}
	repoSettings := narvipg.NewRepoSettingsStore(s.h.Pool)
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatal(err)
	}
	if _, err := repoSettings.UpsertAutoRetriggerReviewToggle(ctx, repoFullName, true); err != nil {
		t.Fatal(err)
	}
	if _, err := prSessions.UpsertPendingRetriggerHeadSHA(ctx, repoFullName, prNumber, "sha-s20-pushed"); err != nil {
		t.Fatal(err)
	}
	s.makeDebounceDue(ctx, t, session.ID)
	return session.ID
}

func (s *scenario20) makeDebounceDue(ctx context.Context, t *testing.T, sessionID pgtype.UUID) {
	t.Helper()
	if _, err := s.h.Timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID, Name: sessionactor.TimerReviewRetriggerDebounce, FiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Second), Valid: true},
	}); err != nil {
		t.Fatalf("arm the debounce: %v", err)
	}
}

// automationOn creates an active automation on a known repository: a cron
// schedule when schedule is set, otherwise one whose invocations are
// recorded directly, as an event's are.
func (s *scenario20) automationOn(ctx context.Context, t *testing.T, repo, schedule string) (sqlcgen.Automation, []byte) {
	t.Helper()
	if err := narvipg.NewGitHubPRSessionStore(s.h.Pool).EnsureRow(ctx, "acme/"+repo, 1); err != nil {
		t.Fatalf("make acme/%s known: %v", repo, err)
	}
	targets, err := json.Marshal([]domainautomation.Target{{Name: repo, URL: "https://github.com/acme/" + repo}})
	if err != nil {
		t.Fatal(err)
	}
	params := sqlcgen.CreateAutomationParams{Name: "s20 " + repo, Repos: targets, EnvVars: []byte("[]"), TriggerType: sqlcgen.AutomationTriggerTypeManual, TriggerConfig: []byte("{}")}
	if schedule != "" {
		params.TriggerType = sqlcgen.AutomationTriggerTypeCron
		params.TriggerConfig = []byte(`{"schedule":"` + schedule + `"}`)
	}
	prompt := "do the scheduled thing"
	params.Prompt = &prompt
	row, err := narvipg.NewAutomationStore(s.h.Pool).Create(ctx, params)
	if err != nil {
		t.Fatalf("create automation %s: %v", repo, err)
	}
	return row, targets
}

// boundTwoStepWorkflow binds a custom two-step definition -- first, then
// second on ok, by order -- to repo's request lane.
func (s *scenario20) boundTwoStepWorkflow(ctx context.Context, t *testing.T, repoFullName string) {
	t.Helper()
	var defID pgtype.UUID
	if err := s.h.Pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', 's20 build then check', false, 1) RETURNING id`).Scan(&defID); err != nil {
		t.Fatalf("insert the definition: %v", err)
	}
	for i := 1; i <= 2; i++ {
		if _, err := s.h.Pool.Exec(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, $2, 'agent', '{{prompt}}')`, defID, i); err != nil {
			t.Fatalf("insert step %d: %v", i, err)
		}
	}
	if _, err := s.h.Pool.Exec(ctx, `INSERT INTO workflow_bindings (lane, repo_full_name, workflow_definition_id, definition_version) VALUES ('request', $1, $2, 1)`, repoFullName, defID); err != nil {
		t.Fatalf("bind the definition: %v", err)
	}
}

// TestResilience_Scenario20_Freeze_NothingAutomaticStarts is scenario
// #20's freeze half, end to end: exits 1 (nothing automatic while frozen),
// 2 (every candidate runs exactly once after) and 3 (no running turn
// severed), with a person's command working throughout.
func TestResilience_Scenario20_Freeze_NothingAutomaticStarts(t *testing.T) {
	ctx := context.Background()
	s := newScenario20(ctx, t)
	h := s.h

	// --- The candidates, every one pending before the freeze. ---

	codeHost := s.armedMergeCandidate(ctx, t)
	gate, err := autonomy.NewGate(h.Pool)
	if err != nil {
		t.Fatalf("autonomy.NewGate: %v", err)
	}
	mergeInbox := s.inboxDeps
	worker, err := automerge.New(automerge.Deps{
		DecisionInbox: mergeInbox, SourceControl: codeHost, AuditLog: s.auditLog,
		Outbound: platform.MustNewGitHubOutboundConfig("s20-bot-token"), Timeouts: h.Timeouts, Autonomy: gate,
	})
	if err != nil {
		t.Fatalf("automerge.New: %v", err)
	}

	fixRow := s.sentinelAutoFixDelivery(ctx, t)
	outbox := narvipg.NewOutboxStore(h.Pool, false)
	notifier, err := outboxworker.NewSentinelAutoFixNotifier(h.Pool, h.Sessions, h.Turns, narvipg.NewEnvironmentStore(h.Pool), s.auditLog, s.registry,
		narvipg.NewSentinelFixStore(h.Pool), narvipg.NewReviewFindingStore(h.Pool), codeHost, platform.MustNewGitHubOutboundConfig("s20-bot-token"), h.Timeouts,
		false, platform.RolloutModeOpen, narvipg.NewRepoSettingsStore(h.Pool), narvipg.NewGitHubPRSessionStore(h.Pool),
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(h.Pool))
	if err != nil {
		t.Fatalf("NewSentinelAutoFixNotifier: %v", err)
	}
	builder, err := outboxworker.NewBuilder(outbox, h.Pool, map[ports.NotificationKind]ports.Notifier{ports.NotificationKindSentinelAutoFix: notifier}, h.Timeouts, &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("outboxworker.NewBuilder: %v", err)
	}

	reviewSession := s.debouncedReReview(ctx, t)

	cronAutomation, _ := s.automationOn(ctx, t, "s20-cron", "* * * * *")
	eventAutomation, eventTargets := s.automationOn(ctx, t, "s20-event", "")
	invocations := narvipg.NewAutomationInvocationStore(h.Pool)
	engine, err := automation.NewEngine(narvipg.NewAutomationStore(h.Pool), invocations, narvipg.NewAutomationRunStore(h.Pool), h.Sessions, h.Turns,
		narvipg.NewEnvironmentStore(h.Pool), s.auditLog, h.Pool, s.registry, h.Timeouts, false, platform.RolloutModeOpen,
		narvipg.NewRepoSettingsStore(h.Pool), narvipg.NewGitHubPRSessionStore(h.Pool))
	if err != nil {
		t.Fatalf("automation.NewEngine: %v", err)
	}

	releaser, err := workflowengine.NewHeldAdvanceReleaser(h.Pool, s.guard, h.Timeouts, false, false)
	if err != nil {
		t.Fatalf("NewHeldAdvanceReleaser: %v", err)
	}
	s.boundTwoStepWorkflow(ctx, t, "acme/s20-workflow")
	workflowSession, workflowAgent := s.sandboxSession(ctx, t, `[{"name":"s20-workflow","url":"https://github.com/acme/s20-workflow.git","branch":null}]`, false)
	firstAttempt := s.prompt(ctx, t, workflowSession, "build the thing")
	waitUntil(t, scenario20Wait, func() bool { return s.turnStatus(ctx, t, firstAttempt.ID) == sqlcgen.TurnStatusProcessing })
	workflows := narvipg.NewWorkflowStore(h.Pool)
	run, err := workflows.GetRunningRunForSession(ctx, workflowSession)
	if err != nil {
		t.Fatalf("the workflow session's run: %v", err)
	}

	// A person's turn, processing when the freeze lands, and through it.
	spanningSession, spanningAgent := s.sandboxSession(ctx, t, "", false)
	spanning := s.prompt(ctx, t, spanningSession, "a long task a person asked for")
	waitUntil(t, scenario20Wait, func() bool { return s.turnStatus(ctx, t, spanning.ID) == sqlcgen.TurnStatusProcessing })

	humanSession, _ := s.sandboxSession(ctx, t, "", true)

	// --- The freeze, through the audited admin action. ---

	if status := s.admin(t, "/api/autonomy/freeze", `{"reason":"scenario 20: an incident, hold every automatic action"}`); status != http.StatusOK {
		t.Fatalf("freeze: status %d, want 200", status)
	}

	// Exit 1: nothing automatic starts.
	for tick := 0; tick < 2; tick++ {
		if err := worker.PumpOnce(ctx, time.Now()); err != nil {
			t.Fatalf("auto-merge tick: %v", err)
		}
	}
	if reads, merges, _ := codeHost.counts(); reads != 0 || merges != 0 {
		t.Fatalf("while frozen: %d pull request reads and %d merges, want none", reads, merges)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("outbox tick: %v", err)
	}
	held, err := outbox.Get(ctx, fixRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != sqlcgen.OutboxStatusPending || held.Attempts != 0 || held.LastError == nil || !strings.HasPrefix(*held.LastError, "skipped (frozen)") {
		t.Fatalf("the sentinel auto-fix row while frozen: status %s, attempts %d, last_error %v; want held, no attempt counted", held.Status, held.Attempts, held.LastError)
	}
	if _, _, branches := codeHost.counts(); branches != 0 {
		t.Fatalf("while frozen: %d fix branches made, want none", branches)
	}
	if n := s.count(ctx, t, `SELECT count(*) FROM sessions WHERE provenance_tag = 'sentinel_auto_fix'`); n != 0 {
		t.Fatalf("while frozen: %d sentinel fix sessions, want none", n)
	}

	if err := s.registry.PumpOnce(ctx); err != nil {
		t.Fatalf("timer pump: %v", err)
	}
	recheck := h.Timeouts.AutonomyFreezeRecheckInterval
	waitUntil(t, scenario20Wait, func() bool {
		return s.count(ctx, t, `SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = $2 AND fires_at > now() + make_interval(secs => $3)`,
			reviewSession, sessionactor.TimerReviewRetriggerDebounce, recheck.Seconds()/2) == 1
	})
	if n := s.count(ctx, t, `SELECT count(*) FROM turns WHERE session_id = $1`, reviewSession); n != 0 {
		t.Fatalf("while frozen: %d review turns, want none", n)
	}

	if err := engine.EvaluateCronTriggersOnce(ctx); err != nil {
		t.Fatalf("cron tick: %v", err)
	}
	if n := s.count(ctx, t, `SELECT count(*) FROM automation_invocations WHERE automation_id = $1`, cronAutomation.ID); n != 0 {
		t.Fatalf("while frozen: %d cron invocations, want none", n)
	}
	// An event arriving while frozen still records its invocation.
	eventInvocation, err := invocations.Create(ctx, sqlcgen.CreateAutomationInvocationParams{AutomationID: eventAutomation.ID, Targets: eventTargets, TotalRuns: 1})
	if err != nil {
		t.Fatalf("record the event's invocation: %v", err)
	}
	if err := engine.PumpOnce(ctx); err != nil {
		t.Fatalf("fan-out tick: %v", err)
	}
	if n := s.count(ctx, t, `SELECT count(*) FROM automation_runs`); n != 0 {
		t.Fatalf("while frozen: %d automation runs, want none", n)
	}
	if n := s.count(ctx, t, `SELECT count(*) FROM automation_invocations WHERE id = $1 AND status = 'pending' AND fanned_out_at IS NULL`, eventInvocation.ID); n != 1 {
		t.Fatal("while frozen: the event's invocation is not pending and unclaimed")
	}

	// The workflow step's turn ends while frozen: it completes, and the
	// advance to the second step is held.
	workflowAgent.completeLast(t)
	waitUntil(t, scenario20Wait, func() bool { return s.turnStatus(ctx, t, firstAttempt.ID) == sqlcgen.TurnStatusCompleted })
	waitUntil(t, scenario20Wait, func() bool {
		_, err := workflows.GetAdvanceHold(ctx, run.ID)
		return err == nil
	})
	if n := s.count(ctx, t, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, run.ID); n != 1 {
		t.Fatalf("while frozen: %d step runs, want the first alone", n)
	}
	if n := s.count(ctx, t, `SELECT count(*) FROM turns WHERE session_id = $1`, workflowSession); n != 1 {
		t.Fatalf("while frozen: %d workflow turns, want the first alone", n)
	}

	// Every role's inbox shows the freeze, and lists the held advance.
	inbox, err := decisioninbox.Build(ctx, s.inboxDeps, pgtype.UUID{}, authz.RoleAdmin, time.Now())
	if err != nil {
		t.Fatalf("decisioninbox.Build: %v", err)
	}
	if !inbox.AutonomyFreeze.AutonomyFrozen || inbox.AutonomyFreezeUnread || len(inbox.HeldWorkflowAdvances) != 1 || inbox.HeldWorkflowAdvances[0].WorkflowRunID != run.ID.String() {
		t.Fatalf("the inbox while frozen: frozen %v, unread %v, held advances %+v; want the freeze and the workflow run", inbox.AutonomyFreeze.AutonomyFrozen, inbox.AutonomyFreezeUnread, inbox.HeldWorkflowAdvances)
	}

	// A person's command still works: a prompt is dispatched and completes.
	human := s.prompt(ctx, t, humanSession, "a person's prompt, while frozen")
	waitUntil(t, scenario20Wait, func() bool { return s.turnStatus(ctx, t, human.ID) == sqlcgen.TurnStatusCompleted })

	// Exit 3, the freeze side: the turn processing when the freeze landed
	// is still processing, and nothing was sent to stop it.
	if got := s.turnStatus(ctx, t, spanning.ID); got != sqlcgen.TurnStatusProcessing {
		t.Fatalf("the spanning turn while frozen is %s, want processing", got)
	}

	// --- The unfreeze, through the audited admin action. ---

	if status := s.admin(t, "/api/autonomy/unfreeze", ""); status != http.StatusOK {
		t.Fatalf("unfreeze: status %d, want 200", status)
	}

	// Exit 2: every candidate runs, exactly once.
	for tick := 0; tick < 2; tick++ {
		if err := worker.PumpOnce(ctx, time.Now()); err != nil {
			t.Fatalf("auto-merge tick after the unfreeze: %v", err)
		}
	}
	if _, merges, _ := codeHost.counts(); merges != 1 {
		t.Fatalf("after the unfreeze: %d merges, want 1", merges)
	}

	for tick := 0; tick < 2; tick++ {
		if _, err := h.Pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() WHERE id = $1 AND status = 'pending'`, fixRow.ID); err != nil {
			t.Fatal(err)
		}
		if err := builder.PumpOnce(ctx); err != nil {
			t.Fatalf("outbox tick after the unfreeze: %v", err)
		}
	}
	delivered, err := outbox.Get(ctx, fixRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if delivered.Status != sqlcgen.OutboxStatusDelivered || delivered.Attempts != 1 {
		t.Fatalf("the sentinel auto-fix row after the unfreeze: status %s, attempts %d; want delivered on its one counted attempt", delivered.Status, delivered.Attempts)
	}
	if _, _, branches := codeHost.counts(); branches != 1 {
		t.Fatalf("after the unfreeze: %d fix branches made, want 1", branches)
	}
	if n := s.count(ctx, t, `SELECT count(*) FROM sessions WHERE provenance_tag = 'sentinel_auto_fix'`); n != 1 {
		t.Fatalf("after the unfreeze: %d sentinel fix sessions, want 1", n)
	}

	s.makeDebounceDue(ctx, t, reviewSession)
	waitUntil(t, scenario20Wait, func() bool {
		if err := s.registry.PumpOnce(ctx); err != nil {
			t.Fatalf("timer pump after the unfreeze: %v", err)
		}
		return s.count(ctx, t, `SELECT count(*) FROM turns WHERE session_id = $1 AND is_review_attempt`, reviewSession) > 0
	})
	if n := s.count(ctx, t, `SELECT count(*) FROM turns WHERE session_id = $1 AND is_review_attempt AND review_head_sha = 'sha-s20-pushed'`, reviewSession); n != 1 {
		t.Fatalf("after the unfreeze: %d automatic reviews of the pushed head, want 1", n)
	}

	if err := engine.EvaluateCronTriggersOnce(ctx); err != nil {
		t.Fatalf("cron tick after the unfreeze: %v", err)
	}
	if n := s.count(ctx, t, `SELECT count(*) FROM automation_invocations WHERE automation_id = $1`, cronAutomation.ID); n != 1 {
		t.Fatalf("after the unfreeze: %d cron invocations, want the held occurrence once", n)
	}
	for tick := 0; tick < 2; tick++ {
		if err := engine.PumpOnce(ctx); err != nil {
			t.Fatalf("fan-out tick after the unfreeze: %v", err)
		}
	}
	for _, a := range []sqlcgen.Automation{cronAutomation, eventAutomation} {
		if n := s.count(ctx, t, `SELECT count(*) FROM automation_runs r JOIN automation_invocations i ON i.id = r.invocation_id WHERE i.automation_id = $1`, a.ID); n != 1 {
			t.Fatalf("after the unfreeze: %d runs of %s, want 1", n, a.Name)
		}
	}

	workflowAgent.setAuto(true)
	for tick := 0; tick < 2; tick++ {
		released, err := releaser.ReleaseOnce(ctx)
		if err != nil {
			t.Fatalf("release tick: %v", err)
		}
		if want := 1 - tick; released != want {
			t.Fatalf("release tick %d released %d advances, want %d", tick, released, want)
		}
	}
	waitUntil(t, scenario20Wait, func() bool {
		if err := s.registry.PumpOnce(ctx); err != nil {
			t.Fatalf("timer pump after the release: %v", err)
		}
		got, err := workflows.GetRun(ctx, run.ID)
		return err == nil && got.Status == sqlcgen.WorkflowRunStatusCompleted
	})
	if n := s.count(ctx, t, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, run.ID); n != 2 {
		t.Fatalf("after the release: %d step runs, want the second attempt once", n)
	}
	if _, err := workflows.GetAdvanceHold(ctx, run.ID); err == nil {
		t.Fatal("the hold outlived its release")
	}

	// Exit 3, the unfreeze side: the turn that spanned the freeze completes
	// on its own; nothing ever told its sandbox to stop, and its gen lives.
	spanningAgent.completeLast(t)
	waitUntil(t, scenario20Wait, func() bool { return s.turnStatus(ctx, t, spanning.ID) == sqlcgen.TurnStatusCompleted })
	if _, stops := spanningAgent.counts(); stops != 0 {
		t.Fatalf("the spanning turn's sandbox was sent %d stop commands, want none", stops)
	}
	if n := s.count(ctx, t, `SELECT count(*) FROM sandboxes WHERE session_id = $1 AND gen = 1`, spanningSession); n != 1 {
		t.Fatal("the spanning turn's sandbox is no longer at gen 1: its gen was retired")
	}

	if n := s.count(ctx, t, `SELECT count(*) FROM turns WHERE status IN ('failed', 'cancelled')`); n != 0 {
		t.Fatalf("%d turns failed or cancelled across the scenario, want none", n)
	}
	for _, action := range []string{"autonomy.frozen", "autonomy.unfrozen"} {
		if n := s.count(ctx, t, `SELECT count(*) FROM audit_log WHERE action = $1 AND resource_type = 'platform' AND resource_id = 'autonomy'`, action); n != 1 {
			t.Fatalf("%d %s audit rows, want 1", n, action)
		}
	}
	codeHost.mu.Lock()
	refused := append([]string(nil), codeHost.notImplemented...)
	codeHost.mu.Unlock()
	if len(refused) != 0 {
		t.Fatalf("a site called code host methods the scenario does not model: %v", refused)
	}
}
