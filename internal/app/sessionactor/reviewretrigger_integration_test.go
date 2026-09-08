//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	"github.com/narvidev/narvi/internal/domain/knowledge"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeReviewDiffFetcher implements reviewcontext.Fetcher (GetPullRequest +
// GetCompareDiff) with fully scripted, per-test responses -- a dedicated
// fake for this file's own scenarios rather than extending pushpr_
// integration_test.go's own shared fakeSourceControl (which only ever
// needed the narrower PRDiffFetcher/ports.SourceControl surfaces), so a
// test here can freely script "the live fetch fails" without touching a
// fixture every other test file in this package also relies on.
type fakeReviewDiffFetcher struct {
	mu             sync.Mutex
	getPRCalls     int
	nextHeadSHA    string
	nextBaseRef    string
	nextPRErr      error
	nextDiff       string
	nextCompareErr error
}

func (f *fakeReviewDiffFetcher) GetPullRequest(_ context.Context, _, _ string, _ int32, _ string) (githubapi.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getPRCalls++
	if f.nextPRErr != nil {
		return githubapi.PullRequest{}, f.nextPRErr
	}
	return githubapi.PullRequest{HeadSHA: f.nextHeadSHA, BaseRef: f.nextBaseRef}, nil
}

func (f *fakeReviewDiffFetcher) GetCompareDiff(_ context.Context, _, _, _, _, _ string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nextCompareErr != nil {
		return "", false, f.nextCompareErr
	}
	return f.nextDiff, false, nil
}

// autoRetriggerFixture bundles one seeded review-session PR identity
// (session + github_pr_sessions, session_id already set) plus the store
// handles every scenario test below needs to seed/assert further state.
type autoRetriggerFixture struct {
	pool         *pgxpool.Pool
	sessionID    pgtype.UUID
	repoFullName string
	prNumber     int32

	prSessions     *narvipg.GitHubPRSessionStore
	repoSettings   *narvipg.RepoSettingsStore
	reviewVerdicts *narvipg.ReviewVerdictStore
	turns          *narvipg.TurnStore
	timers         *narvipg.TimerStore
}

func newAutoRetriggerFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) *autoRetriggerFixture {
	t.Helper()

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceGithub)

	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	repoFullName := "acme/widgets"
	var prNumber int32 = 42
	if err := prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("ensure github pr session row: %v", err)
	}
	if err := prSessions.SetSessionID(ctx, repoFullName, prNumber, sessionID); err != nil {
		t.Fatalf("set github pr session id: %v", err)
	}

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	// §30.8: reviewretrigger.go now reads GetLatestNonShadow, which
	// excludes any verdict predating this repo's own live_egress_
	// promoted_at fence -- promote it here, once, so every scenario
	// below that seeds a verdict (insertVerdict/insertVerdictWithReviewPath)
	// gets a verdict this fixture's own repo genuinely counts as
	// non-shadow, matching what every one of these tests already assumes
	// ("the latest posted verdict for this PR" is visible to the
	// auto-retrigger decision). Idempotent -- re-affirming an
	// already-live repo never moves the fence (queries/reposettings.sql's
	// own UpsertLiveEgressEnabled doc comment).
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}

	return &autoRetriggerFixture{
		pool:           pool,
		sessionID:      sessionID,
		repoFullName:   repoFullName,
		prNumber:       prNumber,
		prSessions:     prSessions,
		repoSettings:   repoSettings,
		reviewVerdicts: narvipg.NewReviewVerdictStore(pool),
		turns:          narvipg.NewTurnStore(pool),
		timers:         narvipg.NewTimerStore(pool),
	}
}

// setPendingHeadSHA seeds github_pr_sessions.pending_retrigger_head_sha
// directly, mirroring what the synchronize webhook handler would have
// written.
func (f *autoRetriggerFixture) setPendingHeadSHA(ctx context.Context, t *testing.T, sha string) {
	t.Helper()
	if _, err := f.prSessions.UpsertPendingRetriggerHeadSHA(ctx, f.repoFullName, f.prNumber, sha); err != nil {
		t.Fatalf("seed pending_retrigger_head_sha: %v", err)
	}
}

// armDebounceTimer inserts a real session_timers row for
// TimerReviewRetriggerDebounce -- so a test asserting it was DELETED
// (this handler's own re-arm-or-delete contract) is asserting a real
// deletion, not a no-op DELETE against a row that was never there.
func (f *autoRetriggerFixture) armDebounceTimer(ctx context.Context, t *testing.T) {
	t.Helper()
	if _, err := f.timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: f.sessionID,
		Name:      TimerReviewRetriggerDebounce,
		FiresAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("arm review_retrigger_debounce timer: %v", err)
	}
}

func (f *autoRetriggerFixture) timerExists(ctx context.Context, t *testing.T) bool {
	t.Helper()
	_, err := f.timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: f.sessionID, Name: TimerReviewRetriggerDebounce})
	if err == nil {
		return true
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	t.Fatalf("get review_retrigger_debounce timer: %v", err)
	return false
}

func (f *autoRetriggerFixture) getPRSession(ctx context.Context, t *testing.T) sqlcgen.GithubPrSession {
	t.Helper()
	row, err := f.prSessions.GetBySessionID(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("get github pr session: %v", err)
	}
	return row
}

func (f *autoRetriggerFixture) insertVerdict(ctx context.Context, t *testing.T, headSHA, riskLevel string) {
	t.Helper()
	if _, err := f.reviewVerdicts.Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName:      f.repoFullName,
		PrNumber:          f.prNumber,
		HeadSha:           headSHA,
		RiskLevel:         riskLevel,
		Premise:           "ok",
		BlastRadius:       []byte(`[]`),
		FilesChanged:      1,
		TestsCoverage:     "adequate",
		DocsDrift:         "none",
		ProposedShippable: "auto",
		Shippable:         "auto",
		SessionID:         f.sessionID,
		ArchDecisionTags:  []byte(`[]`),
		ArchDecisionRoots: []byte(`[]`),
	}); err != nil {
		t.Fatalf("insert review verdict: %v", err)
	}
}

// insertVerdictWithReviewPath mirrors insertVerdict, additionally seeding
// review_path -- the D9/D1 test below needs a PRIOR verdict with a REAL
// review_path on record (§24's own re-review floor input) that
// insertVerdict's own callers never needed before this Step.
func (f *autoRetriggerFixture) insertVerdictWithReviewPath(ctx context.Context, t *testing.T, headSHA, riskLevel, reviewPath string) {
	t.Helper()
	if _, err := f.reviewVerdicts.Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName:      f.repoFullName,
		PrNumber:          f.prNumber,
		HeadSha:           headSHA,
		RiskLevel:         riskLevel,
		Premise:           "ok",
		BlastRadius:       []byte(`[]`),
		FilesChanged:      1,
		TestsCoverage:     "adequate",
		DocsDrift:         "none",
		ProposedShippable: "auto",
		Shippable:         "auto",
		SessionID:         f.sessionID,
		ReviewPath:        &reviewPath,
		ArchDecisionTags:  []byte(`[]`),
		ArchDecisionRoots: []byte(`[]`),
	}); err != nil {
		t.Fatalf("insert review verdict with review_path: %v", err)
	}
}

func (f *autoRetriggerFixture) countOutboxVerdictRows(ctx context.Context, t *testing.T) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`,
		f.sessionID, string(ports.NotificationKindGitHubVerdict),
	).Scan(&count); err != nil {
		t.Fatalf("count outbox verdict rows: %v", err)
	}
	return count
}

func (f *autoRetriggerFixture) countAuditLogRows(ctx context.Context, t *testing.T, action string) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE resource_type = 'turn' AND action = $1 AND detail_json->>'session_id' = $2`,
		action, f.sessionID.String(),
	).Scan(&count); err != nil {
		t.Fatalf("count audit_log rows: %v", err)
	}
	return count
}

// newAutoRetriggerRegistry builds a Registry wired with diffFetcher as
// its ReviewDiffFetcher -- botHandle/botToken are fixed test values
// (never asserted on directly, only that they were threaded through to
// RerunGuidance's own rendered text where relevant).
func newAutoRetriggerRegistry(ctx context.Context, t *testing.T, pool *pgxpool.Pool, diffFetcher *fakeReviewDiffFetcher) *Registry {
	t.Helper()
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false,
		RegistryOptions{ReviewDiffFetcher: diffFetcher, GitHubBotHandle: "narvi-bot", GitHubBotToken: "test-token"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	return r
}

// fireDebounceTimer sends TimerFired{Name: TimerReviewRetriggerDebounce}
// through a real Actor and waits for the timer row this handler must
// re-arm-or-delete (this file's own top-level contract) to actually be
// gone -- every scenario below expects deletion (this handler never
// re-arms, per §24.3's own "otherwise... clears... and deletes" wording
// in every branch), so waiting for absence is the right, un-racy
// condition to poll for regardless of which branch a given test
// exercises.
func fireDebounceTimer(ctx context.Context, t *testing.T, r *Registry, f *autoRetriggerFixture) {
	t.Helper()
	a, err := r.GetOrSpawn(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	if err := a.Send(ctx, TimerFired{Name: TimerReviewRetriggerDebounce}); err != nil {
		t.Fatalf("Send TimerFired: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool { return !f.timerExists(ctx, t) })
}

// TestReviewRetriggerDebounceTimer_OptInOff_DropsTimerNoTurn covers §24.3
// step 1's own fail-closed branch: no repo_settings row at all (an
// unconfigured repo) means auto_retrigger_review_enabled reads as OFF --
// the timer is deleted (re-arm-or-delete contract) but NO turn is
// created and pending_retrigger_head_sha is left untouched (§24.3 step
// 1's own wording never mentions clearing it in this branch).
func TestReviewRetriggerDebounceTimer_OptInOff_DropsTimerNoTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)
	f.setPendingHeadSHA(ctx, t, "sha-pending-1")
	f.armDebounceTimer(ctx, t)
	// Deliberately NO repo_settings row for f.repoFullName at all.

	diffFetcher := &fakeReviewDiffFetcher{nextHeadSHA: "sha-pending-1", nextBaseRef: "main", nextDiff: "diff"}
	r := newAutoRetriggerRegistry(ctx, t, pool, diffFetcher)
	fireDebounceTimer(ctx, t, r, f)

	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("turns created = %d, want 0 (opt-in off must never dispatch a review)", len(turns))
	}

	row := f.getPRSession(ctx, t)
	if row.PendingRetriggerHeadSha == nil || *row.PendingRetriggerHeadSha != "sha-pending-1" {
		t.Errorf("pending_retrigger_head_sha = %v, want unchanged %q (§24.3 step 1 only drops the timer, never clears this column)", row.PendingRetriggerHeadSha, "sha-pending-1")
	}
	if row.AutoRetriggerCount != 0 {
		t.Errorf("auto_retrigger_count = %d, want 0", row.AutoRetriggerCount)
	}
	if diffFetcher.getPRCalls != 0 {
		t.Errorf("GetPullRequest call count = %d, want 0 (opt-in off must never even attempt a live fetch)", diffFetcher.getPRCalls)
	}
}

// TestReviewRetriggerDebounceTimer_HeadsMatch_ClearsPendingNoTurn covers
// §24.3 step 3: pending_retrigger_head_sha already equals the latest
// posted verdict's own head_sha (a race already reviewed this exact
// commit -- a manual re-trigger, or an earlier automatic one) -- clear
// pending_retrigger_head_sha, delete the timer, no turn.
func TestReviewRetriggerDebounceTimer_HeadsMatch_ClearsPendingNoTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)
	if _, err := f.repoSettings.UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, true); err != nil {
		t.Fatalf("enable auto-retrigger-review: %v", err)
	}
	f.insertVerdict(ctx, t, "sha-already-reviewed", "low")
	f.setPendingHeadSHA(ctx, t, "sha-already-reviewed")
	f.armDebounceTimer(ctx, t)

	diffFetcher := &fakeReviewDiffFetcher{}
	r := newAutoRetriggerRegistry(ctx, t, pool, diffFetcher)
	fireDebounceTimer(ctx, t, r, f)

	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("turns created = %d, want 0 (already reviewed at this exact head)", len(turns))
	}

	row := f.getPRSession(ctx, t)
	if row.PendingRetriggerHeadSha != nil {
		t.Errorf("pending_retrigger_head_sha = %v, want nil (cleared)", row.PendingRetriggerHeadSha)
	}
	if diffFetcher.getPRCalls != 0 {
		t.Errorf("GetPullRequest call count = %d, want 0 (nothing to enqueue, no fetch needed)", diffFetcher.getPRCalls)
	}
}

// TestReviewRetriggerDebounceTimer_Enqueue_CreatesReviewTurn covers §24.3
// step 4's "otherwise" branch: pending_retrigger_head_sha does not match
// the latest verdict's head (here: no verdict at all yet) -- a fresh
// review turn is inserted directly (a.stores.turn.Create, never
// httpapi.CreateTurnForBot, §24.3's own import-cycle constraint), with
// PlanMode=false and ReviewHeadSha anchored to the LIVE-fetched head sha
// (not the webhook-reported pending value), an audit_log row is recorded,
// auto_retrigger_count increments, pending_retrigger_head_sha clears, and
// the timer deletes.
func TestReviewRetriggerDebounceTimer_Enqueue_CreatesReviewTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)
	if _, err := f.repoSettings.UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, true); err != nil {
		t.Fatalf("enable auto-retrigger-review: %v", err)
	}
	f.setPendingHeadSHA(ctx, t, "sha-pending-2")
	f.armDebounceTimer(ctx, t)

	const liveHeadSHA = "sha-live-fetched"
	diffFetcher := &fakeReviewDiffFetcher{nextHeadSHA: liveHeadSHA, nextBaseRef: "main", nextDiff: "+ line changed"}
	r := newAutoRetriggerRegistry(ctx, t, pool, diffFetcher)
	fireDebounceTimer(ctx, t, r, f)

	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns created = %d, want 1", len(turns))
	}
	got := turns[0]
	if got.PlanMode {
		t.Error("PlanMode = true, want false (a review turn is never a build/plan turn)")
	}
	if got.ReviewHeadSha == nil || *got.ReviewHeadSha != liveHeadSHA {
		t.Errorf("ReviewHeadSha = %v, want the LIVE-fetched %q, never the webhook-reported pending value", got.ReviewHeadSha, liveHeadSHA)
	}
	if got.Prompt == nil || *got.Prompt == "" {
		t.Error("Prompt is empty, want the rendered auto-retrigger prompt + diff context")
	}

	row := f.getPRSession(ctx, t)
	if row.PendingRetriggerHeadSha != nil {
		t.Errorf("pending_retrigger_head_sha = %v, want nil (cleared)", row.PendingRetriggerHeadSha)
	}
	if row.AutoRetriggerCount != 1 {
		t.Errorf("auto_retrigger_count = %d, want 1", row.AutoRetriggerCount)
	}

	if n := f.countAuditLogRows(ctx, t, "turn.create"); n != 1 {
		t.Errorf("audit_log turn.create rows = %d, want 1 (system-triggered turn creation must be audited)", n)
	}
}

// TestReviewRetriggerDebounceTimer_FlooredDeep_PromptReflectsDeepPath is
// §26.4's own regression test for a genuine, pre-existing defect this
// Step's own restructuring of handleReviewRetriggerDebounceTimer fixed:
// this handler used to call composeAutoRetriggerPrompt (the ONE place
// this lane calls review.RenderTurnPrompt) BEFORE computing the floored
// review depth, so reviewCtx.DeepPath -- never assigned at all, before
// this fix -- stayed permanently false no matter what turns.review_depth
// went on to record. A PR floored to "deep" by §24's own re-review floor
// (a prior verdict on record with review_path "deep", mirroring
// TestReviewRetriggerDebounceTimer_AlwaysLightConfig_SkipsFloor_StaysLight's
// own fixture immediately below) must now get a prompt that ACTUALLY
// tells the agent the three deep-path digest fields and counterReview are
// REQUIRED, not merely requested -- the exact wording
// reviewpost.ValidateVerdictInput's own deep-path checks (validate.go)
// enforce at the posting endpoint. Before this fix, this same scenario
// would persist review_depth="deep" alongside a prompt promising
// "REQUESTED, not required" and never mentioning counterReview at all --
// guaranteeing ValidateVerdictInput would 400 any honest verdict this
// turn's own agent tried to post.
func TestReviewRetriggerDebounceTimer_FlooredDeep_PromptReflectsDeepPath(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)
	if _, err := f.repoSettings.UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, true); err != nil {
		t.Fatalf("enable auto-retrigger-review: %v", err)
	}
	// A PRIOR verdict that already went deep -- §24's own re-review floor
	// ("once deep, a PR stays deep") means THIS re-review floors to deep
	// too, regardless of how light-looking its own fresh delta is.
	f.insertVerdictWithReviewPath(ctx, t, "sha-prior-deep-2", "low", "deep")
	f.setPendingHeadSHA(ctx, t, "sha-pending-floored-deep")
	f.armDebounceTimer(ctx, t)

	// A deliberately light-looking delta (small diff, no sensitive path) --
	// chosen so this test proves the FLOOR alone drives the outcome, not
	// some other deep-routing signal a bigger/sensitive diff would also
	// trigger.
	diffFetcher := &fakeReviewDiffFetcher{nextHeadSHA: "sha-live-floored-deep", nextBaseRef: "main", nextDiff: "+ trivial line changed"}
	r := newAutoRetriggerRegistry(ctx, t, pool, diffFetcher)
	fireDebounceTimer(ctx, t, r, f)

	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns created = %d, want 1", len(turns))
	}
	got := turns[0]
	if got.ReviewDepth == nil || *got.ReviewDepth != "deep" {
		t.Fatalf("turns.review_depth = %v, want %q (the re-review floor must apply -- test setup is broken, not the property under test)", got.ReviewDepth, "deep")
	}
	if got.Prompt == nil {
		t.Fatal("Prompt is nil, want the rendered auto-retrigger prompt")
	}
	prompt := *got.Prompt

	if !strings.Contains(prompt, "REQUIRED on this deep-path review") {
		t.Errorf("prompt does not mention deep-path-REQUIRED wording even though turns.review_depth = %q -- reviewCtx.DeepPath was not set before RenderTurnPrompt ran (the bug this test pins): %s", *got.ReviewDepth, prompt)
	}
	if strings.Contains(prompt, `"archDecisions": [zero or more of the following object -- REQUESTED, not required`) {
		t.Errorf("prompt still describes archDecisions as merely REQUESTED on a deep-floored turn: %s", prompt)
	}
	if !strings.Contains(prompt, "\"counterReview\": \"done\" | \"skipped\" (REQUIRED on this deep-path review") {
		t.Errorf("prompt does not tell the agent counterReview is required on a deep-floored turn: %s", prompt)
	}
}

// TestReviewRetriggerDebounceTimer_Enqueue_PromptIncludesFalsePositiveAdvisoryAndAlreadyAnsweredFacts
// is rereview fix (finding 1)'s own regression test: before this fix,
// insertAutoRetriggerTurn built its own turn prompt by calling
// review.RenderTurnPrompt directly on autoRetriggerPromptText, with NO
// §22.3 false-positive advisory block and NO §22.1 already-answered-facts
// block ever prepended -- the automatic re-review lane was the ONLY
// review-turn producer in this codebase that skipped both (every other
// lane -- httpapi.RetriggerReview's own manual-button path, internal/
// adapters/inbound/github/handler.go's own mention/label path -- already
// prepends them, see each of THEIR OWN "AlreadyAnsweredFacts_Prepended..."
// -style regression tests). Proves the composed prompt this handler
// actually persists CONTAINS both blocks' own distinguishing content, not
// merely that SOME prompt was rendered.
func TestReviewRetriggerDebounceTimer_Enqueue_PromptIncludesFalsePositiveAdvisoryAndAlreadyAnsweredFacts(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)
	if _, err := f.repoSettings.UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, true); err != nil {
		t.Fatalf("enable auto-retrigger-review: %v", err)
	}
	f.setPendingHeadSHA(ctx, t, "sha-pending-composed-prompt")
	f.armDebounceTimer(ctx, t)

	const wantAdvisoryReason = "Logging a variable name that merely CONTAINS 'password' is not a real secret leak."
	falsePositivePatterns := narvipg.NewFalsePositivePatternStore(pool)
	if _, _, err := falsePositivePatterns.Upsert(ctx, f.repoFullName, 1001, "issue_comment", wantAdvisoryReason, pgtype.UUID{}); err != nil {
		t.Fatalf("seed false-positive pattern: %v", err)
	}

	const wantFindingDescription = "Missing test coverage for the timeout path."
	reviewFindings := narvipg.NewReviewFindingStore(pool)
	if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: f.repoFullName,
		PrNumber:     f.prNumber,
		IdentityHash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Severity:     "medium",
		FilePath:     "internal/foo/bar.go",
		Description:  wantFindingDescription,
	}); err != nil {
		t.Fatalf("seed review finding: %v", err)
	}

	diffFetcher := &fakeReviewDiffFetcher{nextHeadSHA: "sha-live-composed", nextBaseRef: "main", nextDiff: "+ line changed"}
	r := newAutoRetriggerRegistry(ctx, t, pool, diffFetcher)
	fireDebounceTimer(ctx, t, r, f)

	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns created = %d, want 1", len(turns))
	}
	prompt := turns[0].Prompt
	if prompt == nil {
		t.Fatal("turn prompt is nil")
	}

	if !strings.Contains(*prompt, wantAdvisoryReason) {
		t.Errorf("prompt = %q, want it to contain the false-positive advisory's own reason %q (§22.3)", *prompt, wantAdvisoryReason)
	}
	if !strings.Contains(*prompt, wantFindingDescription) {
		t.Errorf("prompt = %q, want it to contain the already-answered finding's own description %q (§22.1)", *prompt, wantFindingDescription)
	}
	if !strings.Contains(*prompt, autoRetriggerPromptText) {
		t.Errorf("prompt = %q, want it to STILL contain the automatic re-trigger's own prose fallback %q (prepended TO, never REPLACING it)", *prompt, autoRetriggerPromptText)
	}

	// composeAutoRetriggerPrompt prepends the advisory block to the base
	// prompt FIRST, then prepends the already-answered-facts block to
	// THAT -- exactly mirroring httpapi.RetriggerReview's and github/
	// handler.go's own identical two-step prepend order -- so the final
	// byte order is already-answered-facts, THEN the advisory, THEN the
	// prose fallback.
	advisoryIdx := strings.Index(*prompt, wantAdvisoryReason)
	factsIdx := strings.Index(*prompt, wantFindingDescription)
	proseIdx := strings.Index(*prompt, autoRetriggerPromptText)
	if advisoryIdx == -1 || factsIdx == -1 || proseIdx == -1 {
		t.Fatal("one or more expected substrings missing -- see the individual assertions above")
	}
	if !(factsIdx < advisoryIdx && advisoryIdx < proseIdx) {
		t.Errorf("want ordering already-answered-facts(%d) < advisory(%d) < prose fallback(%d), got a different order",
			factsIdx, advisoryIdx, proseIdx)
	}
}

// TestReviewRetriggerDebounceTimer_AwaitingPlan_DeclinesToEnqueue is
// rereview fix (finding 4)'s own regression test: mirrors httpapi's own
// TestRetriggerReview_AwaitingPlanAlwaysDeclines_NeverClassifies
// (internal/adapters/inbound/httpapi/reviewretrigger_integration_test.go)
// for the AUTOMATIC lane -- before this fix, insertAutoRetriggerTurn's
// own doc comment claimed "no review session can EVER have an
// awaiting_approval plan row to gate against", a claim this test
// disproves directly: a maintainer/admin CAN submit planMode=true on a
// GitHub review session via the ordinary REST API (httpapi.CreateTurn
// forwards client-supplied req.PlanMode into CreateTurnCore with no
// session-kind restriction), producing exactly the awaiting_approval row
// seeded below.
func TestReviewRetriggerDebounceTimer_AwaitingPlan_DeclinesToEnqueue(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)
	if _, err := f.repoSettings.UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, true); err != nil {
		t.Fatalf("enable auto-retrigger-review: %v", err)
	}
	f.setPendingHeadSHA(ctx, t, "sha-pending-awaiting-plan")
	f.armDebounceTimer(ctx, t)

	// Seed a producing turn (Completed, plan_mode true) and an
	// awaiting_approval plans row atop the session -- mirrors httpapi's
	// own TestRetriggerReview_AwaitingPlanAlwaysDeclines_NeverClassifies
	// precedent exactly (that file lives in package httpapi_test,
	// unreachable from here; this is this file's own equivalent).
	plans := narvipg.NewPlanStore(pool)
	producingTurn, err := f.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: f.sessionID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	if _, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: f.sessionID, TurnID: producingTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval}); err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}

	diffFetcher := &fakeReviewDiffFetcher{nextHeadSHA: "sha-live-awaiting-plan", nextBaseRef: "main", nextDiff: "diff"}
	r := newAutoRetriggerRegistry(ctx, t, pool, diffFetcher)
	fireDebounceTimer(ctx, t, r, f)

	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	// producingTurn is the only turn that should exist -- no automatic
	// re-review turn was inserted atop it while its own plan sits
	// awaiting_approval.
	if len(turns) != 1 {
		t.Fatalf("turns after firing = %d, want 1 (only the seeded producing turn -- no automatic re-review turn while a plan is awaiting approval)", len(turns))
	}

	row := f.getPRSession(ctx, t)
	if row.PendingRetriggerHeadSha != nil {
		t.Errorf("pending_retrigger_head_sha = %v, want nil (cleared even though nothing was enqueued)", row.PendingRetriggerHeadSha)
	}
	if row.AutoRetriggerCount != 0 {
		t.Errorf("auto_retrigger_count = %d, want 0 (no budget spent while declining for an awaiting-approval plan)", row.AutoRetriggerCount)
	}
	if diffFetcher.getPRCalls != 1 {
		t.Errorf("GetPullRequest call count = %d, want 1 (phase 2 still live-fetches BEFORE the awaiting-plan gate runs inside the transaction -- see handleReviewRetriggerDebounceTimer's own two-phase shape)", diffFetcher.getPRCalls)
	}
}

// TestReviewRetriggerDebounceTimer_NoLiveHeadSHA_DeclinesToEnqueue covers
// §24's own fail-closed direction: when the live reviewcontext.Fetch call
// cannot resolve a head sha (here: the fake's GetPullRequest call fails),
// this handler must NOT guess-and-dispatch a turn with no honest head sha
// to anchor to -- it declines or, this cycle, treats it identically to
// "nothing to do": pending_retrigger_head_sha still clears and the timer
// still deletes (only a fresh synchronize event tries again -- the timer
// this firing just deleted means there is no "next debounce firing" left
// to retry on its own), but the per-PR budget is never spent on a turn
// that was never created.
func TestReviewRetriggerDebounceTimer_NoLiveHeadSHA_DeclinesToEnqueue(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)
	if _, err := f.repoSettings.UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, true); err != nil {
		t.Fatalf("enable auto-retrigger-review: %v", err)
	}
	f.setPendingHeadSHA(ctx, t, "sha-pending-3")
	f.armDebounceTimer(ctx, t)

	diffFetcher := &fakeReviewDiffFetcher{nextPRErr: errors.New("simulated GitHub API failure")}
	r := newAutoRetriggerRegistry(ctx, t, pool, diffFetcher)
	fireDebounceTimer(ctx, t, r, f)

	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("turns created = %d, want 0 (must never guess-and-dispatch with no confirmed head sha)", len(turns))
	}

	row := f.getPRSession(ctx, t)
	if row.PendingRetriggerHeadSha != nil {
		t.Errorf("pending_retrigger_head_sha = %v, want nil (cleared -- only a fresh synchronize event retries)", row.PendingRetriggerHeadSha)
	}
	if row.AutoRetriggerCount != 0 {
		t.Errorf("auto_retrigger_count = %d, want 0 (no budget spent on a turn that was never created)", row.AutoRetriggerCount)
	}
}

// TestReviewRetriggerDebounceTimer_BudgetExhausted_PostsNoticeOnce covers
// §24.6 in full: once auto_retrigger_count already reaches the budget,
// the "otherwise" branch stops enqueueing (still clears pending +
// deletes the timer) and posts exactly ONE server-side verdict-tool
// notice (a ports.NotificationKindGitHubVerdict outbox row, never a raw
// comment) -- proven here by firing the SAME exhausted-budget condition
// TWICE (two separate synchronize/debounce cycles) and asserting the
// second firing posts no second notice.
func TestReviewRetriggerDebounceTimer_BudgetExhausted_PostsNoticeOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)
	if _, err := f.repoSettings.UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, true); err != nil {
		t.Fatalf("enable auto-retrigger-review: %v", err)
	}
	for i := 0; i < reviewAutoRetriggerBudget; i++ {
		if _, err := f.prSessions.IncrementAutoRetriggerCount(ctx, f.repoFullName, f.prNumber); err != nil {
			t.Fatalf("seed auto_retrigger_count: %v", err)
		}
	}

	diffFetcher := &fakeReviewDiffFetcher{nextHeadSHA: "sha-should-not-be-fetched", nextBaseRef: "main"}
	r := newAutoRetriggerRegistry(ctx, t, pool, diffFetcher)

	// First firing: budget already exhausted -- posts the one-time notice.
	f.setPendingHeadSHA(ctx, t, "sha-pending-4")
	f.armDebounceTimer(ctx, t)
	fireDebounceTimer(ctx, t, r, f)

	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("turns created = %d, want 0 (budget exhausted)", len(turns))
	}
	if n := f.countOutboxVerdictRows(ctx, t); n != 1 {
		t.Fatalf("github_verdict outbox rows after first exhausted firing = %d, want 1", n)
	}
	row := f.getPRSession(ctx, t)
	if row.PendingRetriggerHeadSha != nil {
		t.Errorf("pending_retrigger_head_sha = %v, want nil (cleared even though nothing was enqueued)", row.PendingRetriggerHeadSha)
	}
	if row.AutoRetriggerCount != reviewAutoRetriggerBudget {
		t.Errorf("auto_retrigger_count = %d, want unchanged %d (budget-exhausted branch never increments further)", row.AutoRetriggerCount, reviewAutoRetriggerBudget)
	}

	// Second firing: still exhausted -- must NOT post a second notice.
	f.setPendingHeadSHA(ctx, t, "sha-pending-5")
	f.armDebounceTimer(ctx, t)
	fireDebounceTimer(ctx, t, r, f)

	if n := f.countOutboxVerdictRows(ctx, t); n != 1 {
		t.Errorf("github_verdict outbox rows after SECOND exhausted firing = %d, want still 1 (one-time notice, not repeated)", n)
	}
}

// TestReviewRetriggerDebounceTimer_RaceWithNewerPush_NeverClobbersFresherValue
// proves the guarded-clear compare-and-swap this handler's own top
// comment describes: if pending_retrigger_head_sha has already moved on
// to a NEWER value (a synchronize event that raced in after this firing
// read its own decision) by the time this handler tries to clear it, the
// clear must be a no-op -- the newer value survives untouched.
// ClearPendingRetriggerHeadSHA is exercised directly here (the store
// layer) since reliably forcing this exact interleaving through the full
// two-phase actor handler would require artificial synchronization hooks
// this codebase does not expose; the store-level guarantee is what the
// handler's own correctness rests on, and is fully exercised by every
// other scenario test above using it as documented (a real firing that
// finds no race always succeeds).
//
// Rereview fix (finding 2) extends this test past the store-level CAS: a
// guard miss like the one just proved above must ALSO make
// finishReviewRetrigger skip its own subsequent deleteTimer call, since
// session_timers has UNIQUE(session_id, name) -- there is no SECOND,
// separate timer row this firing "owns" apart from the one the newer
// push's own re-arm (armDebounceTimer below, simulating
// pullrequestsynchronize.go's real upsert-pending-sha+re-arm-timer pair)
// just updated. Before this fix, every branch unconditionally deleted the
// timer regardless of a guard miss, silently stranding the newer push's
// own pending head sha with no timer left to ever act on it again --
// exactly the trailing-edge-debounce failure §24.2 exists to prevent.
// Exercised by calling the real finishReviewRetrigger method directly
// (this file's own package, not the full two-phase handler -- same
// reasoning as the store-level call above) with a deliberately STALE
// decision (pendingHeadSHA "sha-old", exactly what a firing would have
// read from readReviewRetriggerState BEFORE the race).
func TestReviewRetriggerDebounceTimer_RaceWithNewerPush_NeverClobbersFresherValue(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)

	f.setPendingHeadSHA(ctx, t, "sha-old")
	// Simulates a NEW synchronize webhook event racing in after a
	// firing's own read but before its own clear -- ALSO re-arming the
	// SAME session_timers row, exactly like pullrequestsynchronize.go's
	// real handler does (pending_retrigger_head_sha and the debounce
	// timer are always upserted together, in one transaction).
	f.setPendingHeadSHA(ctx, t, "sha-new")
	f.armDebounceTimer(ctx, t)

	_, err := f.prSessions.ClearPendingRetriggerHeadSHA(ctx, f.repoFullName, f.prNumber, "sha-old")
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ClearPendingRetriggerHeadSHA(expected=sha-old) = %v, want pgx.ErrNoRows (the column no longer holds the value this clear expected)", err)
	}

	row := f.getPRSession(ctx, t)
	if row.PendingRetriggerHeadSha == nil || *row.PendingRetriggerHeadSha != "sha-new" {
		t.Errorf("pending_retrigger_head_sha = %v, want unchanged %q (a stale clear must never clobber a fresher push)", row.PendingRetriggerHeadSha, "sha-new")
	}

	registry := newAutoRetriggerRegistry(ctx, t, pool, &fakeReviewDiffFetcher{})
	a, err := registry.GetOrSpawn(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	staleDecision := &reviewRetriggerDecision{
		action:         reviewRetriggerActionHeadsMatch,
		repoFullName:   f.repoFullName,
		prNumber:       f.prNumber,
		pendingHeadSHA: "sha-old",
	}
	enqueued, err := a.finishReviewRetrigger(ctx, staleDecision, review.PreFetchedContext{}, "")
	if err != nil {
		t.Fatalf("finishReviewRetrigger: %v", err)
	}
	if enqueued {
		t.Error("enqueued = true, want false (reviewRetriggerActionHeadsMatch never enqueues a turn)")
	}

	if !f.timerExists(ctx, t) {
		t.Error("review_retrigger_debounce timer was deleted on a guard miss -- it is the SAME row the newer push's own re-arm just set (session_timers has UNIQUE(session_id, name)), so deleting it strands that newer push with no timer left to ever act on it again")
	}
	row = f.getPRSession(ctx, t)
	if row.PendingRetriggerHeadSha == nil || *row.PendingRetriggerHeadSha != "sha-new" {
		t.Errorf("pending_retrigger_head_sha after finishReviewRetrigger = %v, want still unchanged %q", row.PendingRetriggerHeadSha, "sha-new")
	}
}

// TestReviewRetriggerDebounceTimer_MarkAutoRetriggerBudgetNoticeSent_GuardsAgainstDoubleMark
// is rereview fix (finding 7)'s own store-level isolation test: §24.6's
// own "post the notice exactly once" property is enforced by BOTH an
// in-memory check (decision.budgetNoticeAlreadySent, read once per firing
// -- this actor processes one command at a time for a given session, §2)
// AND a SQL guard clause (AND auto_retrigger_budget_notice_sent_at IS
// NULL, MarkAutoRetriggerBudgetNoticeSent's own generated query) --
// TestReviewRetriggerDebounceTimer_BudgetExhausted_PostsNoticeOnce above
// only kills the COMBINED mutation of both (removing the SQL guard alone
// still passes that test, since its own single-actor execution path is
// already covered by the in-memory check). This isolates and proves the
// SQL guard specifically, calling the store method directly twice --
// mirroring TestReviewRetriggerDebounceTimer_RaceWithNewerPush_
// NeverClobbersFresherValue's own identical store-level-CAS-proof pattern
// for ClearPendingRetriggerHeadSHA, above.
func TestReviewRetriggerDebounceTimer_MarkAutoRetriggerBudgetNoticeSent_GuardsAgainstDoubleMark(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)

	if _, err := f.prSessions.MarkAutoRetriggerBudgetNoticeSent(ctx, f.repoFullName, f.prNumber); err != nil {
		t.Fatalf("first MarkAutoRetriggerBudgetNoticeSent = %v, want success", err)
	}

	_, err := f.prSessions.MarkAutoRetriggerBudgetNoticeSent(ctx, f.repoFullName, f.prNumber)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second MarkAutoRetriggerBudgetNoticeSent = %v, want pgx.ErrNoRows (the SQL guard -- auto_retrigger_budget_notice_sent_at IS NULL -- must refuse a second mark)", err)
	}

	row := f.getPRSession(ctx, t)
	if !row.AutoRetriggerBudgetNoticeSentAt.Valid {
		t.Error("auto_retrigger_budget_notice_sent_at is NULL, want set by the first (successful) mark")
	}
}

// TestReviewRetriggerDebounceTimer_AlwaysLightConfig_SkipsFloor_StaysLight
// is D9's own regression test (adversarial-review fix): a PR that
// previously went deep (a prior verdict on record with review_path
// "deep") would, before this fix, be floored back to deep on EVERY
// subsequent automatic re-review FOREVER, even after an admin explicitly
// set this repo's reviewDepth.mode to always_light -- silently defeating
// that explicit admin cost-control override, and persisting a self-
// contradictory decision record (mode "always_light" alongside depth
// "deep"). With the fix, an always_light-configured repo's fresh
// decision (ReasonAlwaysLightConfig, checked first in Decide's own fixed
// order -- decide.go) is used AS-IS, never floored against the PR's own
// deep history.
func TestReviewRetriggerDebounceTimer_AlwaysLightConfig_SkipsFloor_StaysLight(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)
	if _, err := f.repoSettings.UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, true); err != nil {
		t.Fatalf("enable auto-retrigger-review: %v", err)
	}
	alwaysLight := "always_light"
	if _, err := f.repoSettings.UpsertReviewDepthConfig(ctx, f.repoFullName, &alwaysLight, []byte(`[]`)); err != nil {
		t.Fatalf("configure reviewDepth.mode=always_light: %v", err)
	}
	// A PRIOR verdict that already went deep -- exactly the state that
	// used to permanently pin every later auto-retrigger to deep too,
	// pre-fix.
	f.insertVerdictWithReviewPath(ctx, t, "sha-prior-deep", "low", "deep")
	f.setPendingHeadSHA(ctx, t, "sha-pending-always-light")
	f.armDebounceTimer(ctx, t)

	// A light-looking delta (small diff, no sensitive path) -- with the
	// repo forced to always_light, this is irrelevant to the outcome
	// either way, but chosen deliberately unremarkable so this test proves
	// the MODE override, not some OTHER deep-routing signal.
	diffFetcher := &fakeReviewDiffFetcher{nextHeadSHA: "sha-live-always-light", nextBaseRef: "main", nextDiff: "+ trivial line changed"}
	r := newAutoRetriggerRegistry(ctx, t, pool, diffFetcher)
	fireDebounceTimer(ctx, t, r, f)

	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns created = %d, want 1", len(turns))
	}
	got := turns[0]
	if got.ReviewDepth == nil || *got.ReviewDepth != "light" {
		t.Errorf("turns.review_depth = %v, want %q (an explicit always_light admin override must outrank the re-review floor)", got.ReviewDepth, "light")
	}

	if got.ReviewDepthDecision == nil {
		t.Fatalf("turns.review_depth_decision is nil, want a recorded decision")
	}
	var record struct {
		Depth string `json:"depth"`
		Mode  string `json:"mode"`
	}
	if err := json.Unmarshal(got.ReviewDepthDecision, &record); err != nil {
		t.Fatalf("unmarshal review_depth_decision: %v", err)
	}
	if record.Depth != "light" {
		t.Errorf("review_depth_decision.depth = %q, want %q", record.Depth, "light")
	}
	if record.Mode != "always_light" {
		t.Errorf("review_depth_decision.mode = %q, want %q", record.Mode, "always_light")
	}
	// The self-contradiction this fix closes: mode/depth must never
	// disagree on their own face.
	if record.Mode == "always_light" && record.Depth == "deep" {
		t.Errorf("review_depth_decision = %+v, self-contradictory: mode always_light alongside depth deep", record)
	}
}

// TestReviewRetriggerDebounceTimer_Enqueue_PersistsKnowledgeModeAndDecision
// is this Step's own regression test for the automatic re-review lane's
// own half of §31.2/§31.6/§31.7's "mode buffer": insertAutoRetriggerTurn
// (reviewretrigger.go) stamps turns.review_knowledge_mode/
// review_knowledge_decision on EVERY turn it creates -- composeAutoRetriggerPrompt's
// own knowledgeMode/knowledgeDecisionJSON return values, computed one
// call earlier. Before this test, nothing anywhere in this repository
// asserted either column is ever actually written by a real timer firing
// through a real Actor -- an adversarial audit of this branch confirmed
// both fields can be silently reset to nil at this exact call site (Insert's
// own last two turn-creation arguments) with the WHOLE suite, this package
// included, still reporting green.
//
// This automatic lane is chosen as this Step's own turn-side coverage
// over the OTHER two review-turn producers (httpapi.RetriggerReview's own
// manual button, internal/adapters/inbound/github/handler.go's own
// mention/label lanes) because it is the only one of the three that fires
// completely unattended, repeatedly, for as long as a PR keeps receiving
// pushes -- the highest-volume path in production, and, per this same
// file's own TestReviewRetriggerDebounceTimer_Enqueue_PromptIncludesFalsePositiveAdvisoryAndAlreadyAnsweredFacts
// doc comment, the one lane with an actual track record of silently
// diverging from its own two siblings. The manual-button and mention/
// label lanes remain genuinely UNCOVERED for this specific pair of
// columns after this change -- both are structurally identical call sites
// (commit eae059e's own wiring: CreateTurnOptions.ReviewKnowledgeMode/
// ReviewKnowledgeDecision threaded through CreateTurnCore/CreateTurnForBot),
// left for a follow-up rather than covered here.
//
// No prior review_verdicts row exists anywhere in this fixture's own
// repo (newAutoRetriggerFixture seeds none, and this test deliberately
// never calls insertVerdict/insertVerdictWithReviewPath) -- so
// FetchPriorArchDecisions' own gate finds nothing to gate against AND
// nothing recent (reviewcontext/archdecisions.go's own "nothing gated,
// nothing recent -- an honestly empty repository history" branch),
// producing a real, non-nil, but EMPTY knowledge.InjectedRecord. That is
// deliberate: this test's own point is that the STAMP survives to the
// turn row at all, not that this particular fixture's history is
// non-empty (TestPostReviewVerdict_PersistsKnowledgeAndArchDecisionStamps,
// internal/adapters/inbound/httpapi, covers the non-empty/knowledge-
// influenced case, one layer further down this same pipeline, at
// verdict-post time).
func TestReviewRetriggerDebounceTimer_Enqueue_PersistsKnowledgeModeAndDecision(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newAutoRetriggerFixture(ctx, t, pool)
	if _, err := f.repoSettings.UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, true); err != nil {
		t.Fatalf("enable auto-retrigger-review: %v", err)
	}
	f.setPendingHeadSHA(ctx, t, "sha-pending-knowledge")
	f.armDebounceTimer(ctx, t)

	diffFetcher := &fakeReviewDiffFetcher{nextHeadSHA: "sha-live-knowledge", nextBaseRef: "main", nextDiff: "+ line changed"}
	r := newAutoRetriggerRegistry(ctx, t, pool, diffFetcher)
	fireDebounceTimer(ctx, t, r, f)

	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns created = %d, want 1", len(turns))
	}
	got := turns[0]

	if got.ReviewKnowledgeMode == nil || *got.ReviewKnowledgeMode != knowledge.ModeA {
		t.Errorf("ReviewKnowledgeMode = %v, want %q -- every automatically-created review turn must be stamped with the mode buffer's own fixed value, never left NULL", got.ReviewKnowledgeMode, knowledge.ModeA)
	}

	if len(got.ReviewKnowledgeDecision) == 0 {
		t.Fatal("ReviewKnowledgeDecision is empty, want a marshaled knowledge.InjectedRecord -- even an honestly-empty repo history must still be RECORDED (Selector/Ranker populated), never left NULL")
	}
	var injected knowledge.InjectedRecord
	if err := json.Unmarshal(got.ReviewKnowledgeDecision, &injected); err != nil {
		t.Fatalf("unmarshal ReviewKnowledgeDecision: %v", err)
	}
	if injected.Selector != reviewcontext.SelectorRecencyFallback {
		t.Errorf("ReviewKnowledgeDecision.Selector = %q, want %q (this fixture's repo has no prior review_verdicts row at all, gated or recent)", injected.Selector, reviewcontext.SelectorRecencyFallback)
	}
	if !injected.Empty() {
		t.Errorf("ReviewKnowledgeDecision = %+v, want Empty() true (no candidates exist anywhere in this fixture's repo to inject)", injected)
	}
}
