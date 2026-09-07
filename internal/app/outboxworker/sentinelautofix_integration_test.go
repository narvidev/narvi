//go:build integration

// Integration test for §8.2's ("sentinels + suggestions", §17.2) own
// sentinel-auto-fix notifier (sentinelautofix.go), against a real Postgres
// instance -- gated behind the "integration" build tag, reusing this
// package's own newTestPool helper (builder_integration_test.go).
package outboxworker_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeSentinelAutoFixSourceControl is a minimal test-only ports.
// SourceControl -- narrowed to exactly the two methods Deliver's own
// createFixBranch calls (ResolveBranchSHA, CreateBranch, confirmed-finding
// fix) -- every other method returns a clear "not implemented" error,
// mirroring internal/app/sessionactor's own fakeSourceControl precedent.
type fakeSentinelAutoFixSourceControl struct {
	mu sync.Mutex

	shaCalls   []ports.ResolveBranchSHASpec
	nextSHA    string
	nextSHAErr error

	createBranchCalls []ports.CreateBranchSpec
	createBranchErr   error
}

var _ ports.SourceControl = (*fakeSentinelAutoFixSourceControl)(nil)

func (f *fakeSentinelAutoFixSourceControl) CreatePR(context.Context, ports.CreatePRSpec) (ports.PRRef, error) {
	return ports.PRRef{}, errors.New("fakeSentinelAutoFixSourceControl: CreatePR not implemented")
}

func (f *fakeSentinelAutoFixSourceControl) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shaCalls = append(f.shaCalls, spec)
	if f.nextSHAErr != nil {
		return "", "", f.nextSHAErr
	}
	return f.nextSHA, spec.Branch, nil
}

func (f *fakeSentinelAutoFixSourceControl) shaCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.shaCalls)
}

func (f *fakeSentinelAutoFixSourceControl) lastSHASpec() ports.ResolveBranchSHASpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shaCalls[len(f.shaCalls)-1]
}

func (f *fakeSentinelAutoFixSourceControl) ResolveContractsFingerprint(context.Context, ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return "", false, errors.New("fakeSentinelAutoFixSourceControl: ResolveContractsFingerprint not implemented")
}

func (f *fakeSentinelAutoFixSourceControl) CheckRepoAccess(context.Context, ports.CheckRepoAccessSpec) (bool, error) {
	return false, errors.New("fakeSentinelAutoFixSourceControl: CheckRepoAccess not implemented")
}

func (f *fakeSentinelAutoFixSourceControl) GetFileContent(context.Context, ports.GetFileContentSpec) (string, string, bool, error) {
	return "", "", false, errors.New("fakeSentinelAutoFixSourceControl: GetFileContent not implemented")
}

func (f *fakeSentinelAutoFixSourceControl) UpdateFileContent(context.Context, ports.UpdateFileContentSpec) (string, error) {
	return "", errors.New("fakeSentinelAutoFixSourceControl: UpdateFileContent not implemented")
}

func (f *fakeSentinelAutoFixSourceControl) RegisterPRStack(context.Context, ports.RegisterPRStackSpec) error {
	return errors.New("fakeSentinelAutoFixSourceControl: RegisterPRStack not implemented")
}

// ListMergedBetween ("release PR review", §15.2) is never
// reached from this package -- same "not implemented" precedent as
// RegisterPRStack above.
func (f *fakeSentinelAutoFixSourceControl) ListMergedBetween(context.Context, ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return nil, false, errors.New("fakeSentinelAutoFixSourceControl: ListMergedBetween not implemented")
}

func (f *fakeSentinelAutoFixSourceControl) CreateBranch(_ context.Context, spec ports.CreateBranchSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createBranchCalls = append(f.createBranchCalls, spec)
	return f.createBranchErr
}
func (f *fakeSentinelAutoFixSourceControl) GetOpenPR(context.Context, string, string, int, string) (ports.OpenPR, bool, error) {
	return ports.OpenPR{}, false, errors.New("fakeSentinelAutoFixSourceControl: GetOpenPR not implemented")
}
func (f *fakeSentinelAutoFixSourceControl) GetPRBody(context.Context, string, string, int, string) (string, bool, error) {
	return "", false, errors.New("fakeSentinelAutoFixSourceControl: GetPRBody not implemented")
}
func (f *fakeSentinelAutoFixSourceControl) UpdatePRBody(context.Context, ports.UpdatePRBodySpec) error {
	return errors.New("fakeSentinelAutoFixSourceControl: UpdatePRBody not implemented")
}

func (f *fakeSentinelAutoFixSourceControl) createBranchCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.createBranchCalls)
}

func (f *fakeSentinelAutoFixSourceControl) lastCreateBranchSpec() ports.CreateBranchSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createBranchCalls[len(f.createBranchCalls)-1]
}

// ListOpenPRsForUser/ResolveCodeOwners/MergePR ("decision inbox:
// read model + API", §16.2) are never reached from this package -- same
// "not implemented" precedent as RegisterPRStack/ListMergedBetween above.
func (f *fakeSentinelAutoFixSourceControl) ListOpenPRsForUser(context.Context, ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	return nil, false, errors.New("fakeSentinelAutoFixSourceControl: ListOpenPRsForUser not implemented")
}

func (f *fakeSentinelAutoFixSourceControl) ResolveCodeOwners(context.Context, ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	return nil, errors.New("fakeSentinelAutoFixSourceControl: ResolveCodeOwners not implemented")
}

func (f *fakeSentinelAutoFixSourceControl) MergePR(context.Context, ports.MergePRSpec) (string, error) {
	return "", errors.New("fakeSentinelAutoFixSourceControl: MergePR not implemented")
}

// TestSentinelAutoFixNotifier_SpawnsChildSessionAndUpdatesStores proves
// the notifier's own real Deliver: a real child session is spawned
// (httpapi.SpawnChildSession), sentinel_fixes.fix_child_session_id is
// recorded, and every named finding moves to 'fix_pending'.
func TestSentinelAutoFixNotifier_SpawnsChildSessionAndUpdatesStores(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	repoFullName := "acme/notifier-test-repo"
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 77, originSession.ID, "feature-fix-me")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}

	const identityHash = "abc123abc123abc123abc123abc123abc123abc123abc123abc123abc123ab"
	if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName,
		PrNumber:     77,
		IdentityHash: identityHash,
		Severity:     "medium",
		FilePath:     "internal/foo/bar.go",
		Description:  "Missing test coverage.",
	}); err != nil {
		t.Fatalf("upsert review finding: %v", err)
	}

	sourceControl := &fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessions, turns, environments, auditLog, registry, sentinelFixes, reviewFindings,
		sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts(), false, platform.RolloutModeOpen, repoSettings, prSessions,
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool))

	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID:         fix.ID.String(),
		RepoFullName:          repoFullName,
		OriginPRNumber:        77,
		OriginReviewSessionID: originSession.ID.String(),
		OriginHeadBranch:      "feature-fix-me",
		RepoName:              "widgets",
		RepoCloneURL:          "https://github.com/acme/widgets.git",
		FindingIdentityHashes: []string{identityHash},
		FindingDescriptions:   []string{"Missing test coverage."},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}

	updatedFix, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if !updatedFix.FixChildSessionID.Valid {
		t.Fatal("FixChildSessionID is still invalid after Deliver, want a real child session id")
	}
	if updatedFix.Status != "spawned" {
		t.Errorf("Status = %q, want %q", updatedFix.Status, "spawned")
	}

	childSession, err := sessions.Get(ctx, updatedFix.FixChildSessionID)
	if err != nil {
		t.Fatalf("get child session: %v", err)
	}
	if childSession.ProvenanceTag == nil || *childSession.ProvenanceTag != "sentinel_auto_fix" {
		t.Errorf("child session ProvenanceTag = %v, want %q", childSession.ProvenanceTag, "sentinel_auto_fix")
	}
	if childSession.SpawnDepth != 1 {
		t.Errorf("child session SpawnDepth = %d, want 1", childSession.SpawnDepth)
	}
	if !childSession.ParentSessionID.Valid || childSession.ParentSessionID != originSession.ID {
		t.Errorf("child session ParentSessionID = %v, want %v", childSession.ParentSessionID, originSession.ID)
	}

	// Confirmed-finding fix: the child session's own repos[0].branch must
	// be a BRAND-NEW, distinct branch -- NEVER the origin PR's own literal
	// head branch ("feature-fix-me") -- since a session's repos[].branch
	// is what both the boot-time clone/checkout AND the eventual push
	// target. Before this fix, this was "feature-fix-me" verbatim, which
	// would have checked out and pushed back to the SAME branch as the
	// still-open origin PR.
	var childRepos []struct {
		Name   string  `json:"name"`
		Url    string  `json:"url"`
		Branch *string `json:"branch"`
	}
	if err := json.Unmarshal(childSession.Repos, &childRepos); err != nil {
		t.Fatalf("unmarshal child session repos: %v", err)
	}
	if len(childRepos) != 1 {
		t.Fatalf("child session repos = %d, want 1", len(childRepos))
	}
	if childRepos[0].Branch == nil {
		t.Fatal("child session repos[0].branch is nil, want a real, distinct branch name")
	}
	gotChildBranch := *childRepos[0].Branch
	if gotChildBranch == "feature-fix-me" {
		t.Errorf("child session repos[0].branch = %q, want it DISTINCT from the origin PR's own head branch %q -- checking out/pushing the SAME branch as the origin silently fast-forwards the still-open origin PR with an unreviewed commit, and dooms the eventual fix-PR CreatePR call to Head == Base",
			gotChildBranch, "feature-fix-me")
	}
	if !strings.Contains(gotChildBranch, fix.ID.String()) {
		t.Errorf("child session repos[0].branch = %q, want it to reference the sentinel_fixes claim id %q so it is stable/deterministic across redeliveries", gotChildBranch, fix.ID.String())
	}

	// The new branch must be created FROM the origin head branch's own
	// current tip -- ResolveBranchSHA called with Branch: "feature-fix-me"
	// (never a guess), and CreateBranch called with that exact resolved
	// SHA and the SAME branch name just asserted above.
	if sourceControl.shaCallCount() != 1 {
		t.Fatalf("ResolveBranchSHA called %d times, want 1", sourceControl.shaCallCount())
	}
	shaSpec := sourceControl.lastSHASpec()
	if shaSpec.Owner != "acme" || shaSpec.Repo != "widgets" || shaSpec.Branch != "feature-fix-me" {
		t.Errorf("ResolveBranchSHASpec = %+v, want Owner=acme Repo=widgets Branch=feature-fix-me", shaSpec)
	}
	if sourceControl.createBranchCallCount() != 1 {
		t.Fatalf("CreateBranch called %d times, want 1", sourceControl.createBranchCallCount())
	}
	createSpec := sourceControl.lastCreateBranchSpec()
	if createSpec.Branch != gotChildBranch {
		t.Errorf("CreateBranchSpec.Branch = %q, want it to match the child session's own repos[0].branch %q", createSpec.Branch, gotChildBranch)
	}
	if createSpec.SHA != "deadbeef" {
		t.Errorf("CreateBranchSpec.SHA = %q, want %q (the origin branch's own resolved current tip)", createSpec.SHA, "deadbeef")
	}

	finding, err := reviewFindings.Get(ctx, repoFullName, 77, identityHash)
	if err != nil {
		t.Fatalf("get review finding: %v", err)
	}
	if finding.Status != "fix_pending" {
		t.Errorf("finding Status = %q, want %q", finding.Status, "fix_pending")
	}
	if !finding.FixChildSessionID.Valid || finding.FixChildSessionID != updatedFix.FixChildSessionID {
		t.Errorf("finding FixChildSessionID = %v, want %v", finding.FixChildSessionID, updatedFix.FixChildSessionID)
	}

	// Idempotency: a redelivered/retried outbox entry must never spawn a
	// SECOND child session for the SAME claim.
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err != nil {
		t.Fatalf("second Deliver() error = %v", err)
	}
	refetched, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes after second deliver: %v", err)
	}
	if refetched.FixChildSessionID != updatedFix.FixChildSessionID {
		t.Errorf("FixChildSessionID changed after a redelivered outbox entry: %v -> %v, want it unchanged (idempotent, never a second spawn)",
			updatedFix.FixChildSessionID, refetched.FixChildSessionID)
	}
	// The idempotency short-circuit (fix.FixChildSessionID.Valid) fires
	// BEFORE createFixBranch is ever called again -- a redelivery must
	// never re-resolve/re-create the branch either.
	if got := sourceControl.shaCallCount(); got != 1 {
		t.Errorf("ResolveBranchSHA called %d times after a redelivered outbox entry, want still 1 (idempotency short-circuit fires before it)", got)
	}
	if got := sourceControl.createBranchCallCount(); got != 1 {
		t.Errorf("CreateBranch called %d times after a redelivered outbox entry, want still 1 (idempotency short-circuit fires before it)", got)
	}
}

// TestSentinelAutoFixNotifier_ResolveBranchSHAFails_NeverSpawnsChildSession
// proves the confirmed-finding fix's own error path: when the origin head
// branch's own current SHA cannot be resolved, Deliver returns a real
// error (so the outbox worker's own backoff/retry machinery retries
// later) and never spawns a child session with a wrong/fallback branch.
func TestSentinelAutoFixNotifier_ResolveBranchSHAFails_NeverSpawnsChildSession(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	repoFullName := "acme/notifier-resolve-sha-fails-repo"
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 88, originSession.ID, "feature-fix-me-2")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}

	sourceControl := &fakeSentinelAutoFixSourceControl{nextSHAErr: errors.New("simulated GitHub API failure resolving origin head branch")}
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessions, turns, environments, auditLog, registry, sentinelFixes, reviewFindings,
		sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts(), false, platform.RolloutModeOpen, repoSettings, prSessions,
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool))

	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID:         fix.ID.String(),
		RepoFullName:          repoFullName,
		OriginPRNumber:        88,
		OriginReviewSessionID: originSession.ID.String(),
		OriginHeadBranch:      "feature-fix-me-2",
		RepoName:              "widgets",
		RepoCloneURL:          "https://github.com/acme/widgets.git",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err == nil {
		t.Fatal("Deliver() error = nil, want a real error when the origin head branch's own SHA cannot be resolved")
	}

	if sourceControl.createBranchCallCount() != 0 {
		t.Errorf("CreateBranch called %d times, want 0 (never called when ResolveBranchSHA already failed)", sourceControl.createBranchCallCount())
	}

	refetched, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if refetched.FixChildSessionID.Valid {
		t.Error("FixChildSessionID is valid, want it to stay unset -- no child session should ever be spawned when the fix branch could not be created")
	}
}

// TestSentinelAutoFixNotifier_CreateBranchFails_NeverSpawnsChildSession is
// the sibling of the test above for CreateBranch's own failure: the SHA
// resolves fine, but creating the new branch ref itself fails.
func TestSentinelAutoFixNotifier_CreateBranchFails_NeverSpawnsChildSession(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	repoFullName := "acme/notifier-create-branch-fails-repo"
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 89, originSession.ID, "feature-fix-me-3")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}

	sourceControl := &fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef", createBranchErr: errors.New("simulated GitHub API failure creating branch")}
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessions, turns, environments, auditLog, registry, sentinelFixes, reviewFindings,
		sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts(), false, platform.RolloutModeOpen, repoSettings, prSessions,
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool))

	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID:         fix.ID.String(),
		RepoFullName:          repoFullName,
		OriginPRNumber:        89,
		OriginReviewSessionID: originSession.ID.String(),
		OriginHeadBranch:      "feature-fix-me-3",
		RepoName:              "widgets",
		RepoCloneURL:          "https://github.com/acme/widgets.git",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err == nil {
		t.Fatal("Deliver() error = nil, want a real error when CreateBranch itself fails")
	}

	refetched, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if refetched.FixChildSessionID.Valid {
		t.Error("FixChildSessionID is valid, want it to stay unset -- no child session should ever be spawned when the fix branch could not be created")
	}
}

// TestSentinelAutoFixNotifier_ConcurrentDeliver_NeverDoubleSpawnsChildSession
// is the audit fix's own proof for Finding 1 (HIGH, "sentinel-auto-fix
// child session can be spawned twice"). Before this fix, the ONLY dedupe
// guard was a plain FixChildSessionID.Valid check, with the child
// session's own insert (httpapi.SpawnChildSession's own commit) and the
// guard write (a LATER, separate SentinelFixStore.UpdateChildSession call)
// on TWO SEPARATE, non-atomic transactions -- a redelivered/retried
// Deliver call landing in the window between those two writes would spawn
// a SECOND child session for the SAME claim. Deliver's own doc comment
// notes this window is reachable not just via a retried redelivery of the
// SAME outbox row, but also via a genuinely SECOND, independent outbox row
// for the SAME sentinel_fixes claim: reviewverdict.go's own
// SentinelFixStore.Claim only sets fix_child_session_id HERE, in this
// notifier, never at claim time, so two qualifying findings posted
// concurrently on the SAME PR can each legitimately enqueue their own
// NotificationKindSentinelAutoFix row against the SAME claim id. This test
// models exactly that second shape (the more realistic one to reproduce
// deterministically): two DIFFERENT outbox notifications for the SAME
// claim, fired at a real Postgres instance truly concurrently (a
// synchronized start barrier, not a fixed sleep), so the invariant is
// proven against the REAL SentinelFixStore.LockForUpdate row lock, not a
// mock.
//
// Asserts, regardless of which goroutine's own transaction happens to win
// the race: EXACTLY ONE child session is ever created for the claim, AND
// the LOSER's own finding still gets marked fix_pending against the
// winner's session (proving the fix does not just prevent a double-spawn,
// but also does not silently drop the loser's own tail work).
func TestSentinelAutoFixNotifier_ConcurrentDeliver_NeverDoubleSpawnsChildSession(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	repoFullName := "acme/notifier-concurrent-repo"
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 111, originSession.ID, "feature-concurrent-fix")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}

	const hashA = "aaa111aaa111aaa111aaa111aaa111aaa111aaa111aaa111aaa111aaa111aa"
	const hashB = "bbb222bbb222bbb222bbb222bbb222bbb222bbb222bbb222bbb222bbb222bb"
	for _, h := range []string{hashA, hashB} {
		if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
			RepoFullName: repoFullName,
			PrNumber:     111,
			IdentityHash: h,
			Severity:     "medium",
			FilePath:     "internal/foo/bar.go",
			Description:  "Missing test coverage.",
		}); err != nil {
			t.Fatalf("upsert review finding %q: %v", h, err)
		}
	}

	sourceControl := &fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessions, turns, environments, auditLog, registry, sentinelFixes, reviewFindings,
		sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts(), false, platform.RolloutModeOpen, repoSettings, prSessions,
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool))

	buildPayload := func(hash string) []byte {
		payload, err := json.Marshal(ports.SentinelAutoFixPayload{
			SentinelFixID:         fix.ID.String(),
			RepoFullName:          repoFullName,
			OriginPRNumber:        111,
			OriginReviewSessionID: originSession.ID.String(),
			OriginHeadBranch:      "feature-concurrent-fix",
			RepoName:              "widgets",
			RepoCloneURL:          "https://github.com/acme/widgets.git",
			FindingIdentityHashes: []string{hash},
			FindingDescriptions:   []string{"Missing test coverage."},
		})
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return payload
	}
	payloadA := buildPayload(hashA)
	payloadB := buildPayload(hashB)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errsCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errsCh <- notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payloadA})
	}()
	go func() {
		defer wg.Done()
		<-start
		errsCh <- notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payloadB})
	}()
	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			t.Fatalf("concurrent Deliver() error = %v", err)
		}
	}

	// The invariant this whole fix exists for: AT MOST ONE child session
	// ever spawned per sentinel_fixes row, even under this exact
	// concurrent race.
	var childSessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE parent_session_id = $1`, originSession.ID).Scan(&childSessionCount); err != nil {
		t.Fatalf("count child sessions: %v", err)
	}
	if childSessionCount != 1 {
		t.Fatalf("child sessions created for one sentinel_fixes claim = %d, want exactly 1 (a double-spawn -- the Finding 1 bug this test exists to catch)", childSessionCount)
	}

	updatedFix, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if !updatedFix.FixChildSessionID.Valid {
		t.Fatal("FixChildSessionID is still invalid after concurrent Deliver, want a real child session id")
	}

	// BOTH findings -- one from each racing outbox row -- must still be
	// marked fix_pending against the SAME winning child session: the
	// LOSER of the claim race must not skip its own markFindingsFixPending
	// tail just because it did not spawn anything itself.
	for _, h := range []string{hashA, hashB} {
		finding, err := reviewFindings.Get(ctx, repoFullName, 111, h)
		if err != nil {
			t.Fatalf("get review finding %q: %v", h, err)
		}
		if finding.Status != "fix_pending" {
			t.Errorf("finding %q Status = %q, want %q", h, finding.Status, "fix_pending")
		}
		if !finding.FixChildSessionID.Valid || finding.FixChildSessionID != updatedFix.FixChildSessionID {
			t.Errorf("finding %q FixChildSessionID = %v, want %v (the SAME winning child session both racing outbox rows should record)", h, finding.FixChildSessionID, updatedFix.FixChildSessionID)
		}
	}
}

// TestSentinelAutoFixNotifier_SecondOutboxRowForSameClaim_ReusesWinningSession
// is the SEQUENTIAL, deterministic sibling of the concurrency test above.
// Where that test exercises Deliver's atomic LockForUpdate race (both
// calls start before either has spawned), this one exercises Deliver's
// OTHER "already spawned" path -- the cheap, non-atomic
// FixChildSessionID.Valid fast path -- reached here because the first
// Deliver call is allowed to run to completion before the second one (a
// genuinely different outbox row for the SAME claim, carrying a DIFFERENT
// finding hash) starts. Proves the same two properties under ordinary,
// non-racy sequential execution: no second spawn, and the second outbox
// row's own finding still gets marked fix_pending -- plus that the fast
// path genuinely skips re-resolving/re-creating the fix branch (only ONE
// ResolveBranchSHA/CreateBranch call total).
func TestSentinelAutoFixNotifier_SecondOutboxRowForSameClaim_ReusesWinningSession(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	repoFullName := "acme/notifier-sequential-second-row-repo"
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 112, originSession.ID, "feature-sequential-fix")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}

	const hashA = "ccc333ccc333ccc333ccc333ccc333ccc333ccc333ccc333ccc333ccc333cc"
	const hashB = "ddd444ddd444ddd444ddd444ddd444ddd444ddd444ddd444ddd444ddd444dd"
	for _, h := range []string{hashA, hashB} {
		if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
			RepoFullName: repoFullName,
			PrNumber:     112,
			IdentityHash: h,
			Severity:     "medium",
			FilePath:     "internal/foo/bar.go",
			Description:  "Missing test coverage.",
		}); err != nil {
			t.Fatalf("upsert review finding %q: %v", h, err)
		}
	}

	sourceControl := &fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessions, turns, environments, auditLog, registry, sentinelFixes, reviewFindings,
		sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts(), false, platform.RolloutModeOpen, repoSettings, prSessions,
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool))

	buildPayload := func(hash string) []byte {
		payload, err := json.Marshal(ports.SentinelAutoFixPayload{
			SentinelFixID:         fix.ID.String(),
			RepoFullName:          repoFullName,
			OriginPRNumber:        112,
			OriginReviewSessionID: originSession.ID.String(),
			OriginHeadBranch:      "feature-sequential-fix",
			RepoName:              "widgets",
			RepoCloneURL:          "https://github.com/acme/widgets.git",
			FindingIdentityHashes: []string{hash},
			FindingDescriptions:   []string{"Missing test coverage."},
		})
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return payload
	}

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: buildPayload(hashA)}); err != nil {
		t.Fatalf("first Deliver() error = %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: buildPayload(hashB)}); err != nil {
		t.Fatalf("second Deliver() error = %v", err)
	}

	var childSessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE parent_session_id = $1`, originSession.ID).Scan(&childSessionCount); err != nil {
		t.Fatalf("count child sessions: %v", err)
	}
	if childSessionCount != 1 {
		t.Fatalf("child sessions created for one sentinel_fixes claim = %d, want exactly 1", childSessionCount)
	}

	updatedFix, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}

	for _, h := range []string{hashA, hashB} {
		finding, err := reviewFindings.Get(ctx, repoFullName, 112, h)
		if err != nil {
			t.Fatalf("get review finding %q: %v", h, err)
		}
		if finding.Status != "fix_pending" {
			t.Errorf("finding %q Status = %q, want %q", h, finding.Status, "fix_pending")
		}
		if !finding.FixChildSessionID.Valid || finding.FixChildSessionID != updatedFix.FixChildSessionID {
			t.Errorf("finding %q FixChildSessionID = %v, want %v", h, finding.FixChildSessionID, updatedFix.FixChildSessionID)
		}
	}

	// The fast path (fix.FixChildSessionID.Valid, read before ever
	// attempting to claim+spawn) must skip createFixBranch entirely on the
	// second Deliver call -- a genuinely different outbox row for an
	// already-fulfilled claim must never re-resolve/re-create the branch.
	if got := sourceControl.shaCallCount(); got != 1 {
		t.Errorf("ResolveBranchSHA called %d times across both Deliver calls, want 1 (the second call's own fast path must skip it)", got)
	}
	if got := sourceControl.createBranchCallCount(); got != 1 {
		t.Errorf("CreateBranch called %d times across both Deliver calls, want 1 (the second call's own fast path must skip it)", got)
	}
}

// TestReviewFindingStore_MarkFixPending_GuardedAgainstRegressionPastFixPending
// proves the premise the Finding 2 fix depends on directly at the store
// layer: MarkFixPending's own guarded UPDATE (status IN ('open',
// 'fix_pending'), queries/reviewfindings.sql) makes it safe for
// sentinelAutoFixNotifier.markFindingsFixPending to re-run on a retried
// Deliver call -- re-running for a finding that has SINCE progressed past
// fix_pending (e.g. the fix session finished and its PR already opened
// before the retry ran) must be a benign pgx.ErrNoRows no-op, never a
// silent regression of that finding's own status back to fix_pending.
func TestReviewFindingStore_MarkFixPending_GuardedAgainstRegressionPastFixPending(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)

	childSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create child session: %v", err)
	}

	repoFullName := "acme/mark-fix-pending-guard-repo"
	const prNumber = 113
	const identityHash = "eee555eee555eee555eee555eee555eee555eee555eee555eee555eee555ee"
	if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		IdentityHash: identityHash,
		Severity:     "medium",
		FilePath:     "internal/foo/bar.go",
		Description:  "Missing test coverage.",
	}); err != nil {
		t.Fatalf("upsert review finding: %v", err)
	}

	// open -> fix_pending: the ordinary, expected first transition.
	if _, err := reviewFindings.MarkFixPending(ctx, repoFullName, prNumber, identityHash, childSession.ID); err != nil {
		t.Fatalf("MarkFixPending (open -> fix_pending): %v", err)
	}

	// fix_pending -> fix_pending, SAME session: a retried Deliver call
	// re-running this write for a finding it already reached successfully
	// must be a harmless no-op, never an error.
	if _, err := reviewFindings.MarkFixPending(ctx, repoFullName, prNumber, identityHash, childSession.ID); err != nil {
		t.Fatalf("MarkFixPending (fix_pending -> fix_pending, idempotent retry): %v", err)
	}

	// fix_pending -> fix_open: the fix session's own PR opened (pushpr.go's
	// createSentinelFixPRBestEffort) before any later retry ran.
	if _, err := reviewFindings.MarkFixOpen(ctx, childSession.ID, 4242); err != nil {
		t.Fatalf("MarkFixOpen: %v", err)
	}

	// A LATE retry's own MarkFixPending call must now be a benign
	// pgx.ErrNoRows no-op -- the guard's whole reason to exist -- never a
	// silent regression back to fix_pending.
	if _, err := reviewFindings.MarkFixPending(ctx, repoFullName, prNumber, identityHash, childSession.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("MarkFixPending after fix_open: err = %v, want pgx.ErrNoRows (the guard blocking a regression past fix_pending)", err)
	}

	finding, err := reviewFindings.Get(ctx, repoFullName, prNumber, identityHash)
	if err != nil {
		t.Fatalf("get review finding: %v", err)
	}
	if finding.Status != "fix_open" {
		t.Errorf("finding Status = %q after a late MarkFixPending retry, want it to STAY %q (a regression back to fix_pending is exactly the bug this guard exists to prevent)", finding.Status, "fix_open")
	}
}

// TestSentinelAutoFixNotifier_MissingFindingRow_IsBenignNoOp proves
// markFindingsFixPending's own pgx.ErrNoRows filter still works after the
// Finding 2 refactor: a finding hash with no corresponding review_findings
// row (already deleted, or never actually qualified) must not fail the
// whole delivery -- the child session itself is still spawned normally.
func TestSentinelAutoFixNotifier_MissingFindingRow_IsBenignNoOp(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}

	repoFullName := "acme/notifier-missing-finding-repo"
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 114, originSession.ID, "feature-missing-finding-fix")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}

	sourceControl := &fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessions, turns, environments, auditLog, registry, sentinelFixes, reviewFindings,
		sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts(), false, platform.RolloutModeOpen, repoSettings, prSessions,
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool))

	// Deliberately NO reviewFindings.Upsert call for this hash -- it never
	// qualified (or its row has since disappeared).
	const missingHash = "fff666fff666fff666fff666fff666fff666fff666fff666fff666fff666ff"
	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID:         fix.ID.String(),
		RepoFullName:          repoFullName,
		OriginPRNumber:        114,
		OriginReviewSessionID: originSession.ID.String(),
		OriginHeadBranch:      "feature-missing-finding-fix",
		RepoName:              "widgets",
		RepoCloneURL:          "https://github.com/acme/widgets.git",
		FindingIdentityHashes: []string{missingHash},
		FindingDescriptions:   []string{"Missing test coverage."},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil (a missing finding row must be a benign no-op)", err)
	}

	updatedFix, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if !updatedFix.FixChildSessionID.Valid {
		t.Error("FixChildSessionID is still invalid, want the child session to have been spawned normally despite the missing finding row")
	}
}
