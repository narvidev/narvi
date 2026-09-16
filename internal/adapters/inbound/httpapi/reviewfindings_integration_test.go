//go:build integration

// Integration tests for §8.2's ("sentinels + suggestions", §12.2 item
// 2, §17, §22.1) own findings-upsert extension to the verdict-posting
// tool, the rebut endpoint, the apply-suggestion endpoint, and the
// sentinel-auto-fix trigger -- against a real Postgres instance, sharing
// this package's own testRig (httpapi_integration_test.go).
package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/shadowscm"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
)

// findingsVerdictRequestJSON is validVerdictRequestJSON (reviewverdict_
// integration_test.go) extended with one coverage-sentinel finding.
func findingsVerdictRequestJSON(sentinelKind, description string) string {
	kindJSON := "null"
	if sentinelKind != "" {
		kindJSON = fmt.Sprintf("%q", sentinelKind)
	}
	return fmt.Sprintf(`{
		"riskLevel": "medium",
		"premise": "ok",
		"blastRadius": [],
		"filesChanged": 3,
		"testsCoverage": "insufficient",
		"docsDrift": "none",
		"proposedShippable": "auto",
		"summary": "One coverage gap found.",
		"findings": [
			{
				"sentinelKind": %s,
				"severity": "medium",
				"filePath": "internal/foo/bar.go",
				"line": 42,
				"description": %q
			}
		],
		"digest": {
			"summary": "Adds a helper function; one coverage gap found.",
			"descriptionAdequacy": "ok",
			"adequacyExplanation": "The PR body accurately describes this change."
		},
		"factCheck": "done",
		"factCheckKilled": 0
	}`, kindJSON, description)
}

// TestPostReviewVerdict_UpsertsFindingsWithServerComputedIdentity proves
// the findings-upsert extension: a posted finding lands in review_findings
// with the SAME identity hash reviewpost.ComputeFindingIdentity computes
// independently, and the response's own findingIdentityHashes echoes it.
func TestPostReviewVerdict_UpsertsFindingsWithServerComputedIdentity(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoFullName := "acme/findings-upsert-repo"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, 21)

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1",
		"", findingsVerdictRequestJSON("coverage", "Missing test for the timeout path."))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if len(resp.FindingIdentityHashes) != 1 {
		t.Fatalf("FindingIdentityHashes = %v, want exactly 1 entry", resp.FindingIdentityHashes)
	}

	kind := reviewpost.SentinelKindCoverage
	wantHash := reviewpost.ComputeFindingIdentity(&kind, "internal/foo/bar.go", "Missing test for the timeout path.")
	if resp.FindingIdentityHashes[0] != wantHash {
		t.Errorf("FindingIdentityHashes[0] = %q, want %q (server-computed, matching reviewpost.ComputeFindingIdentity)", resp.FindingIdentityHashes[0], wantHash)
	}

	row, err := rig.reviewFindings.Get(ctx, repoFullName, 21, wantHash)
	if err != nil {
		t.Fatalf("get review finding: %v", err)
	}
	if row.Status != "open" {
		t.Errorf("Status = %q, want %q", row.Status, "open")
	}
	if row.FilePath != "internal/foo/bar.go" {
		t.Errorf("FilePath = %q, want %q", row.FilePath, "internal/foo/bar.go")
	}
}

// TestPostReviewVerdict_FindingReReportedAtShiftedLine_SameIdentity is
// §8.2's own explicitly required test at the HTTP layer (finding.go's
// own unit tests already prove the pure function; this proves the whole
// posting path preserves it end to end): posting the SAME finding twice,
// at two DIFFERENT line numbers, upserts the SAME review_findings row
// (never two rows), and a rebuttal recorded after the FIRST post survives
// the second post at the shifted line.
func TestPostReviewVerdict_FindingReReportedAtShiftedLine_SameIdentity(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoFullName := "acme/findings-line-shift-repo"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, 22)

	body1 := `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"insufficient","docsDrift":"none","proposedShippable":"auto","summary":"s","findings":[{"sentinelKind":"coverage","severity":"medium","filePath":"a.go","line":10,"description":"Missing coverage for X."}],"digest":{"summary":"Adds a helper; one coverage gap found.","descriptionAdequacy":"ok","adequacyExplanation":"Accurate."},"factCheck":"done","factCheckKilled":0}`
	status, resp1 := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "", body1)
	if status != http.StatusCreated {
		t.Fatalf("first post status = %d, want %d", status, http.StatusCreated)
	}

	// A maintainer rebuts it.
	_, maintainerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
	rebutStatus := rig.doJSON(t, http.MethodPost,
		"/api/sessions/"+session.ID.String()+"/review/findings/"+resp1.FindingIdentityHashes[0]+"/rebut",
		[]byte(`{"rebuttalText":"Not a real gap, covered by table test elsewhere."}`), nil, maintainerToken)
	if rebutStatus != http.StatusOK {
		t.Fatalf("rebut status = %d, want %d", rebutStatus, http.StatusOK)
	}

	// Re-report the SAME finding at a SHIFTED line (10 -> 25).
	body2 := `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"insufficient","docsDrift":"none","proposedShippable":"auto","summary":"s","findings":[{"sentinelKind":"coverage","severity":"medium","filePath":"a.go","line":25,"description":"Missing coverage for X."}],"digest":{"summary":"Adds a helper; one coverage gap found.","descriptionAdequacy":"ok","adequacyExplanation":"Accurate."},"factCheck":"done","factCheckKilled":0}`
	status2, resp2 := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "", body2)
	if status2 != http.StatusCreated {
		t.Fatalf("second post status = %d, want %d", status2, http.StatusCreated)
	}

	if resp2.FindingIdentityHashes[0] != resp1.FindingIdentityHashes[0] {
		t.Fatalf("identity hash changed after a line shift: %q != %q, want the SAME identity", resp2.FindingIdentityHashes[0], resp1.FindingIdentityHashes[0])
	}

	row, err := rig.reviewFindings.Get(ctx, repoFullName, 22, resp1.FindingIdentityHashes[0])
	if err != nil {
		t.Fatalf("get review finding: %v", err)
	}
	if row.Status != "rebutted" {
		t.Errorf("Status = %q after re-report at a shifted line, want %q (the rebuttal must SURVIVE)", row.Status, "rebutted")
	}
	var count int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM review_findings WHERE repo_full_name = $1 AND pr_number = $2`, repoFullName, 22).Scan(&count); err != nil {
		t.Fatalf("count review_findings: %v", err)
	}
	if count != 1 {
		t.Errorf("review_findings row count = %d, want exactly 1 (never a second row for the SAME identity)", count)
	}
}

// TestRebutReviewFinding_MemberDenied proves a plain member cannot rebut.
func TestRebutReviewFinding_MemberDenied(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoFullName := "acme/rebut-denied-repo"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, 30)
	postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "", findingsVerdictRequestJSON("coverage", "x"))

	_, memberToken := rig.createAuthenticatedUser(ctx, t)
	kind := reviewpost.SentinelKindCoverage
	hash := reviewpost.ComputeFindingIdentity(&kind, "internal/foo/bar.go", "x")
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/findings/"+hash+"/rebut",
		[]byte(`{"rebuttalText":"nope"}`), nil, memberToken)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestRebutReviewFinding_UnknownIdentityHash_NotFound proves a
// non-existent finding 404s.
func TestRebutReviewFinding_UnknownIdentityHash_NotFound(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoFullName := "acme/rebut-unknown-repo"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, 31)

	_, maintainerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/findings/"+strings.Repeat("0", 64)+"/rebut",
		[]byte(`{"rebuttalText":"nope"}`), nil, maintainerToken)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// createOwnedGitHubReviewSessionWithBranch mirrors createOwnedGitHubReviewSession
// (reviewretrigger_integration_test.go) but ALSO sets sessions.repos with a
// real branch name -- the sentinel-auto-fix trigger's own read (reviewverdict.go)
// needs this to determine the origin PR's own head branch, which the
// plain, repo-less createOwnedGitHubReviewSession fixture does not carry.
func createOwnedGitHubReviewSessionWithBranch(ctx context.Context, t *testing.T, r testRig, ownerID pgtype.UUID, repoFullName string, prNumber int32, repoName, repoURL, branch string) sqlcgen.Session {
	t.Helper()
	reposJSON, err := json.Marshal([]map[string]string{{"name": repoName, "url": repoURL, "branch": branch}})
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}
	session, err := r.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: ownerID, Repos: reposJSON})
	if err != nil {
		t.Fatalf("create test github review session: %v", err)
	}
	if err := r.prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("ensure github_pr_sessions row: %v", err)
	}
	if err := r.prSessions.SetSessionID(ctx, repoFullName, prNumber, session.ID); err != nil {
		t.Fatalf("set github_pr_sessions session id: %v", err)
	}
	return session
}

// TestPostReviewVerdict_SentinelAutoFix_TriggersWhenToggleOn proves the
// admin toggle's own OTHER half: a repo with sentinel_autofix_enabled=true
// and a posted coverage finding claims a sentinel_fixes row AND enqueues a
// ports.NotificationKindSentinelAutoFix outbox row.
func TestPostReviewVerdict_SentinelAutoFix_TriggersWhenToggleOn(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoFullName := "acme/sentinel-trigger-on-repo"
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := createOwnedGitHubReviewSessionWithBranch(ctx, t, rig, owner.ID, repoFullName, 40, "widgets", "https://github.com/acme/widgets.git", "feature-branch")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	if _, err := rig.repoSettings.Upsert(ctx, repoFullName, false, true); err != nil {
		t.Fatalf("upsert repo settings: %v", err)
	}

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1",
		"", findingsVerdictRequestJSON("coverage", "Missing test for the retry path."))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	fix, err := rig.sentinelFixes.Get(ctx, repoFullName, 40)
	if err != nil {
		t.Fatalf("get sentinel_fixes row: %v", err)
	}
	if fix.OriginHeadBranch != "feature-branch" {
		t.Errorf("OriginHeadBranch = %q, want %q (captured from the origin session's own repos config)", fix.OriginHeadBranch, "feature-branch")
	}

	var kind string
	if err := rig.pool.QueryRow(ctx, `SELECT kind FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindSentinelAutoFix)).Scan(&kind); err != nil {
		t.Fatalf("query outbox for sentinel-auto-fix row: %v (want exactly one enqueued)", err)
	}
}

// TestPostReviewVerdict_SentinelAutoFix_NeverTriggersWhenToggleOff proves
// §8.2's own explicitly required test: the admin toggle DEFAULTS OFF,
// and while off, a coverage finding never claims a sentinel_fixes row or
// enqueues a trigger, even on a repo that otherwise qualifies.
func TestPostReviewVerdict_SentinelAutoFix_NeverTriggersWhenToggleOff(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoFullName := "acme/sentinel-trigger-off-repo"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, 41)
	// Deliberately never calling rig.repoSettings.Upsert -- proving the
	// DEFAULT (no repo_settings row at all) is off.

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1",
		"", findingsVerdictRequestJSON("coverage", "Missing test for the retry path."))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	if _, err := rig.sentinelFixes.Get(ctx, repoFullName, 41); err == nil {
		t.Error("sentinel_fixes row exists with the toggle off (default), want none")
	}

	var count int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindSentinelAutoFix)).Scan(&count); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if count != 0 {
		t.Errorf("sentinel-auto-fix outbox rows = %d, want 0 (toggle off, default)", count)
	}
}

// TestPostReviewVerdict_SentinelAutoFix_NoRecursion proves §17.1's own "no
// recursion" rule: a session whose OWN provenance_tag is
// provenance.SentinelAutoFix never triggers ANOTHER sentinel auto-fix,
// regardless of what its own verdict finds, even with the toggle on.
func TestPostReviewVerdict_SentinelAutoFix_NoRecursion(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoFullName := "acme/sentinel-no-recursion-repo"

	if _, err := rig.repoSettings.Upsert(ctx, repoFullName, false, true); err != nil {
		t.Fatalf("upsert repo settings: %v", err)
	}

	owner, _ := rig.createAuthenticatedUser(ctx, t)
	tag := "sentinel_auto_fix"
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource:   sqlcgen.SessionSpawnSourceGithub,
		CreatedBy:     owner.ID,
		ProvenanceTag: &tag,
	})
	if err != nil {
		t.Fatalf("create sentinel-auto-fix-tagged session: %v", err)
	}
	if err := rig.prSessions.EnsureRow(ctx, repoFullName, 50); err != nil {
		t.Fatalf("ensure github_pr_sessions row: %v", err)
	}
	if err := rig.prSessions.SetSessionID(ctx, repoFullName, 50, session.ID); err != nil {
		t.Fatalf("set github_pr_sessions session id: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1",
		"", findingsVerdictRequestJSON("coverage", "Missing test for the retry path."))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	if _, err := rig.sentinelFixes.Get(ctx, repoFullName, 50); err == nil {
		t.Error("sentinel_fixes row exists for a session whose OWN provenance_tag is sentinel_auto_fix, want none (no recursion, §17.1)")
	}
}

// applySuggestionFakeSourceControl is a minimal, in-memory ports.
// SourceControl backing this file's own ApplySuggestion tests --
// GetFileContent/UpdateFileContent are the only two methods this endpoint
// calls; every other method returns a clear "not implemented" error,
// mirroring internal/app/sessionactor's own fakeSourceControl precedent.
type applySuggestionFakeSourceControl struct {
	content string
	sha     string
	exists  bool

	updateCalls int
	lastUpdate  ports.UpdateFileContentSpec
}

func (f *applySuggestionFakeSourceControl) CreatePR(context.Context, ports.CreatePRSpec) (ports.PRRef, error) {
	return ports.PRRef{}, errors.New("not implemented")
}
func (f *applySuggestionFakeSourceControl) ResolveBranchSHA(context.Context, ports.ResolveBranchSHASpec) (string, string, error) {
	return "", "", errors.New("not implemented")
}
func (f *applySuggestionFakeSourceControl) ResolveContractsFingerprint(context.Context, ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return "", false, errors.New("not implemented")
}
func (f *applySuggestionFakeSourceControl) CheckRepoAccess(context.Context, ports.CheckRepoAccessSpec) (bool, error) {
	return false, errors.New("not implemented")
}
func (f *applySuggestionFakeSourceControl) GetFileContent(_ context.Context, spec ports.GetFileContentSpec) (string, string, bool, error) {
	return f.content, f.sha, f.exists, nil
}
func (f *applySuggestionFakeSourceControl) UpdateFileContent(_ context.Context, spec ports.UpdateFileContentSpec) (string, error) {
	f.updateCalls++
	f.lastUpdate = spec
	return "new-commit-sha", nil
}
func (f *applySuggestionFakeSourceControl) RegisterPRStack(context.Context, ports.RegisterPRStackSpec) error {
	return errors.New("not implemented")
}
func (f *applySuggestionFakeSourceControl) CreateBranch(context.Context, ports.CreateBranchSpec) error {
	return errors.New("not implemented")
}
func (f *applySuggestionFakeSourceControl) GetOpenPR(context.Context, string, string, int, string) (ports.OpenPR, bool, error) {
	return ports.OpenPR{}, false, errors.New("not implemented")
}
func (f *applySuggestionFakeSourceControl) GetPRBody(context.Context, string, string, int, string) (string, bool, error) {
	return "", false, errors.New("not implemented")
}
func (f *applySuggestionFakeSourceControl) UpdatePRBody(context.Context, ports.UpdatePRBodySpec) error {
	return errors.New("not implemented")
}
func (f *applySuggestionFakeSourceControl) ListMergedBetween(context.Context, ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return nil, false, errors.New("not implemented")
}

// ListOpenPRsForUser/ResolveCodeOwners/MergePR ("decision inbox:
// read model + API", §16.2) are never reached from this test -- same
// "not implemented" precedent as every other unused method above.
func (f *applySuggestionFakeSourceControl) ListOpenPRsForUser(context.Context, ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	return nil, false, errors.New("not implemented")
}
func (f *applySuggestionFakeSourceControl) ResolveCodeOwners(context.Context, ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	return nil, errors.New("not implemented")
}
func (f *applySuggestionFakeSourceControl) MergePR(context.Context, ports.MergePRSpec) (string, error) {
	return "", errors.New("not implemented")
}

var _ ports.SourceControl = (*applySuggestionFakeSourceControl)(nil)

// setupFindingWithSuggestedFix creates an owned GitHub review session,
// posts a verdict carrying one finding WITH a SuggestedFix, and returns
// (session, identityHash, suggestedFix).
func setupFindingWithSuggestedFix(ctx context.Context, t *testing.T, rig testRig, repoFullName string, prNumber int32) (sqlcgen.Session, string) {
	t.Helper()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := createOwnedGitHubReviewSessionWithBranch(ctx, t, rig, owner.ID, repoFullName, prNumber, "widgets", "https://github.com/acme/widgets.git", "pr-head-branch")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	body := `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"insufficient","docsDrift":"none","proposedShippable":"auto","summary":"s","findings":[{"severity":"low","filePath":"internal/foo/bar.go","description":"Stale comment.","suggestedFix":"--- a/internal/foo/bar.go\n+++ b/internal/foo/bar.go\n@@ -1,2 +1,2 @@\n package foo\n-// old comment\n+// new comment\n"}],"digest":{"summary":"Fixes a stale comment.","descriptionAdequacy":"ok","adequacyExplanation":"Accurate."},"factCheck":"done","factCheckKilled":0}`
	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "", body)
	if status != http.StatusCreated {
		t.Fatalf("post verdict status = %d, want %d", status, http.StatusCreated)
	}
	return session, resp.FindingIdentityHashes[0]
}

// createMaintainerWithGitHubToken creates a maintainer user with a real,
// round-trippable encrypted GitHub access token -- ApplySuggestion's own
// acting-maintainer credential. createUserWithRole (planapprove_
// integration_test.go) already creates this user's own github identity
// (with no access token) -- this function UPDATES that SAME row via
// UpdateAccessToken, rather than inserting a second (user_id, provider)
// row, which GetByUserAndProvider (identities are looked up by exactly
// that pair) would otherwise have to arbitrarily choose between.
func createMaintainerWithGitHubToken(ctx context.Context, t *testing.T, rig testRig, plaintextToken string) (sqlcgen.User, string) {
	t.Helper()
	user, sessionToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	identity, err := rig.identities.GetByUserAndProvider(ctx, user.ID, sqlcgen.IdentityProviderGithub)
	if err != nil {
		t.Fatalf("get existing github identity: %v", err)
	}

	encrypted, err := platform.EncryptToken(rig.tokenEncryptionKey, []byte(plaintextToken))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	if _, err := rig.identities.UpdateAccessToken(ctx, sqlcgen.UpdateIdentityAccessTokenParams{
		ID:                   identity.ID,
		AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("update github identity access token: %v", err)
	}
	return user, sessionToken
}

// TestApplySuggestion_Success_CommitsUsingActingMaintainerToken proves the
// happy path: a valid, still-applying SuggestedFix is committed via
// UpdateFileContent, using the ACTING maintainer's own token (never the
// session creator's).
func TestApplySuggestion_Success_CommitsUsingActingMaintainerToken(t *testing.T) {
	fake := &applySuggestionFakeSourceControl{
		content: "package foo\n// old comment\n",
		sha:     "blob-sha-1",
		exists:  true,
	}
	rig := newTestRig(t, func(r *testRig) { r.sourceControl = fake })
	ctx := context.Background()
	repoFullName := "acme/apply-suggestion-success-repo"
	session, hash := setupFindingWithSuggestedFix(ctx, t, rig, repoFullName, 60)

	const actingToken = "acting-maintainer-token"
	_, maintainerToken := createMaintainerWithGitHubToken(ctx, t, rig, actingToken)

	var resp restdtos.ApplySuggestionResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/findings/"+hash+"/apply-suggestion", nil, &resp, maintainerToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if resp.CommitSha != "new-commit-sha" {
		t.Errorf("CommitSha = %q, want %q", resp.CommitSha, "new-commit-sha")
	}
	if fake.updateCalls != 1 {
		t.Fatalf("UpdateFileContent called %d times, want 1", fake.updateCalls)
	}
	if fake.lastUpdate.Token != actingToken {
		t.Errorf("UpdateFileContent Token = %q, want the ACTING maintainer's own token %q", fake.lastUpdate.Token, actingToken)
	}
	if !strings.Contains(fake.lastUpdate.Content, "// new comment") {
		t.Errorf("UpdateFileContent Content = %q, want it to contain the patched line", fake.lastUpdate.Content)
	}

	row, err := rig.reviewFindings.Get(ctx, repoFullName, 60, hash)
	if err != nil {
		t.Fatalf("get review finding: %v", err)
	}
	if row.Status != "fix_applied" {
		t.Errorf("Status = %q, want %q", row.Status, "fix_applied")
	}
}

// TestApplySuggestion_ShadowSuppressed_RecordedNotCommitted proves §30.7/
// §30.9's own resolved apply-suggestion behavior: on a repository whose
// outgoing changes are suppressed, UpdateFileContent returns a nil error
// and a self-evidently synthetic commit SHA (never a sentinel error) --
// the handler must recognize that result and respond honestly rather than
// naively marking the finding fix_applied with a SHA that exists nowhere.
// Proves: (1) response.applied is false and response.message says
// "recorded, not committed"; (2) response.commitSha is the shadow-scheme
// value the decorator produced, never a real commit; (3) the finding's
// own status is fix_recorded, never fix_applied; (4) the ledger recorded
// exactly one suppressed update_file_content write.
func TestApplySuggestion_ShadowSuppressed_RecordedNotCommitted(t *testing.T) {
	fake := &applySuggestionFakeSourceControl{
		content: "package foo\n// old comment\n",
		sha:     "blob-sha-shadow",
		exists:  true,
	}
	rig := newTestRig(t, func(r *testRig) {
		ledger := narvipg.NewShadowSCMWriteStore(r.pool)
		decorated, err := shadowscm.New(fake, ledger, func(context.Context, string) bool { return false })
		if err != nil {
			t.Fatalf("shadowscm.New: %v", err)
		}
		r.sourceControl = decorated
		r.shadowLedger = ledger
	})
	ctx := context.Background()
	repoFullName := "acme/apply-suggestion-shadow-repo"
	session, hash := setupFindingWithSuggestedFix(ctx, t, rig, repoFullName, 63)

	const actingToken = "acting-maintainer-token"
	_, maintainerToken := createMaintainerWithGitHubToken(ctx, t, rig, actingToken)

	var resp restdtos.ApplySuggestionResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/findings/"+hash+"/apply-suggestion", nil, &resp, maintainerToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if resp.Applied {
		t.Error("Applied = true, want false -- nothing was really committed")
	}
	if !shadowscm.IsSyntheticCommitSHA(resp.CommitSha) {
		t.Errorf("CommitSha = %q, want the shadow-suppressed synthetic value", resp.CommitSha)
	}
	if !strings.Contains(resp.Message, "Recorded, not committed") {
		t.Errorf("Message = %q, want it to say recorded, not committed", resp.Message)
	}
	// The fake's own live UpdateFileContent must never have been reached --
	// the decorator intercepts it before the wrapped adapter ever sees the
	// call.
	if fake.updateCalls != 0 {
		t.Errorf("live UpdateFileContent called %d times, want 0 -- a shadow-suppressed write must never reach the wrapped adapter", fake.updateCalls)
	}

	row, err := rig.reviewFindings.Get(ctx, repoFullName, 63, hash)
	if err != nil {
		t.Fatalf("get review finding: %v", err)
	}
	if row.Status != "fix_recorded" {
		t.Errorf("Status = %q, want %q -- naive suppression would mark this fix_applied with a SHA that exists nowhere", row.Status, "fix_recorded")
	}

	rows, err := rig.shadowLedger.(*narvipg.ShadowSCMWriteStore).ListForRepo(ctx, repoFullName, 10)
	if err != nil {
		t.Fatalf("ListForRepo: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows for %s = %d, want 1", repoFullName, len(rows))
	}
	if rows[0].Operation != "update_file_content" {
		t.Errorf("Operation = %q, want %q", rows[0].Operation, "update_file_content")
	}
}

// TestApplySuggestion_StalePatch_Conflict proves §8.2's own explicitly
// required test at the HTTP layer: a SuggestedFix that no longer applies
// against the PR's CURRENT head is rejected with 409, and never commits
// anything.
func TestApplySuggestion_StalePatch_Conflict(t *testing.T) {
	fake := &applySuggestionFakeSourceControl{
		// The file's CURRENT content no longer contains "// old comment"
		// at all -- the patch's own old-text can never match.
		content: "package foo\n// something else entirely\n",
		sha:     "blob-sha-2",
		exists:  true,
	}
	rig := newTestRig(t, func(r *testRig) { r.sourceControl = fake })
	ctx := context.Background()
	repoFullName := "acme/apply-suggestion-stale-repo"
	session, hash := setupFindingWithSuggestedFix(ctx, t, rig, repoFullName, 61)

	_, maintainerToken := createMaintainerWithGitHubToken(ctx, t, rig, "tok")

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/findings/"+hash+"/apply-suggestion", nil, nil, maintainerToken)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d", status, http.StatusConflict)
	}
	if fake.updateCalls != 0 {
		t.Errorf("UpdateFileContent called %d times, want 0 (a stale patch must never commit anything)", fake.updateCalls)
	}
}

// TestApplySuggestion_NoSuggestedFix_BadRequest proves a finding with no
// SuggestedFix at all is rejected with 400.
func TestApplySuggestion_NoSuggestedFix_BadRequest(t *testing.T) {
	fake := &applySuggestionFakeSourceControl{content: "x", sha: "s", exists: true}
	rig := newTestRig(t, func(r *testRig) { r.sourceControl = fake })
	ctx := context.Background()
	repoFullName := "acme/apply-suggestion-none-repo"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, 62)
	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "", findingsVerdictRequestJSON("", "No fix available."))
	if status != http.StatusCreated {
		t.Fatalf("post verdict status = %d, want %d", status, http.StatusCreated)
	}

	_, maintainerToken := createMaintainerWithGitHubToken(ctx, t, rig, "tok")
	applyStatus := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/findings/"+resp.FindingIdentityHashes[0]+"/apply-suggestion", nil, nil, maintainerToken)
	if applyStatus != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", applyStatus, http.StatusBadRequest)
	}
	if fake.updateCalls != 0 {
		t.Errorf("UpdateFileContent called %d times, want 0", fake.updateCalls)
	}
}
