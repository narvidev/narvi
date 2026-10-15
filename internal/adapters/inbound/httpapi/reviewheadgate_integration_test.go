//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
)

// This file pins the readers that act on what a review session's head may
// reach, through the real endpoints (technical plan §27.1, §30.4): a pull
// request's review session whose head is not known to be a branch of its
// base repository -- reposource.ReviewHeadInBaseRepository: a fork's pull
// request, a deleted fork's, a head not read at the latest mention, a
// legacy session still naming the fork, a claim no mention has recorded
// yet (a deleted fork's legacy spec is byte for byte a same-repository
// one, so only the claim's record tells them apart) -- runs code a person outside the
// base repository may have written. It is delivered no sandbox secret and
// none of the base repository's repository-scoped provider credentials, and
// apply-suggestion and the sentinel auto-fix, which act on the base
// repository at the spec's branch, refuse it.

// reviewHeadCase is one review session shape, its spec written for repo
// acme/<name>.
type reviewHeadCase struct {
	name string
	// spec is the session's sessions.repos, %[1]s the repository's name.
	spec string
	// claimed: the session has a github_pr_sessions claim on acme/<name>.
	claimed bool
	// record is the claim's head_in_base, as the ingress's latest mention
	// recorded it; nil for a claim nothing has recorded yet.
	record *bool
	// headInBase is what reposource.ReviewHeadInBaseRepository reads.
	headInBase bool
	// caseOnly: the spec's url names the base in another case, so the
	// repository-scoped rows, keyed on the exact name, do not match it.
	caseOnly bool
}

func reviewHeadCases() []reviewHeadCase {
	inBase, notInBase := true, false
	const sameRepoSpec = `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":"feature-x"}]`
	// The spec the previous ingress wrote for a pull request whose fork was
	// already deleted: the base's url with the deleted fork's branch name --
	// byte for byte a same-repository spec. Only the claim's record tells
	// them apart.
	const deletedForkLegacySpec = `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":"main"}]`
	return []reviewHeadCase{
		{name: "a fork's pull request: the base, no branch", spec: `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":null}]`, claimed: true, record: &notInBase},
		{name: "a legacy fork session already moved onto the base", spec: `[{"name":"%[1]s-fork","url":"https://github.com/acme/%[1]s.git","branch":null}]`, claimed: true},
		{name: "a legacy fork session not yet moved: the fork, its branch", spec: `[{"name":"%[1]s-fork","url":"https://github.com/contributor/%[1]s-fork.git","branch":"main"}]`, claimed: true},
		{name: "a legacy deleted-fork session, unrecorded: the base, the deleted fork's branch", spec: deletedForkLegacySpec, claimed: true},
		{name: "a legacy deleted-fork session after a mention recorded its head not in the base", spec: deletedForkLegacySpec, claimed: true, record: &notInBase},
		{name: "a same-repository pull request whose head lookup failed", spec: `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":null}]`, claimed: true, record: &notInBase},
		{name: "a legacy same-repository session, unrecorded until its next mention", spec: sameRepoSpec, claimed: true},
		{name: "a same-repository pull request, its mention recorded in the base", spec: sameRepoSpec, claimed: true, record: &inBase, headInBase: true},
		{name: "a same-repository pull request, its url in another case", spec: `[{"name":"%[1]s","url":"https://github.com/ACME/%[1]s","branch":"feature-x"}]`, claimed: true, record: &inBase, headInBase: true, caseOnly: true},
		{name: "a session that claims no pull request", spec: `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":null}]`, headInBase: true},
	}
}

// seedReviewHeadCase creates tc's session for acme/<repoName>, its claim
// when it has one, and a live sandbox answering to "sandbox-bearer-token".
func seedReviewHeadCase(ctx context.Context, t *testing.T, rig testRig, tc reviewHeadCase, repoName string, prNumber int32) sqlcgen.Session {
	t.Helper()
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		Repos:       []byte(fmt.Sprintf(tc.spec, repoName)),
	})
	if err != nil {
		t.Fatalf("create the session: %v", err)
	}
	if tc.claimed {
		if err := rig.prSessions.EnsureRow(ctx, "acme/"+repoName, prNumber); err != nil {
			t.Fatal(err)
		}
		if err := rig.prSessions.SetSessionID(ctx, "acme/"+repoName, prNumber, session.ID); err != nil {
			t.Fatal(err)
		}
		if tc.record != nil {
			if err := rig.prSessions.RecordHeadInBase(ctx, "acme/"+repoName, prNumber, *tc.record); err != nil {
				t.Fatal(err)
			}
		}
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")
	return session
}

// TestSandboxSecretsDelivery_AReviewSessionWhoseHeadIsNotInItsBaseGetsNoSecret:
// with a global secret and a repository-scoped one on the base repository
// configured, a review session whose head is not known to be a branch of
// its base repository gets neither -- the answer a session with no secrets
// gets -- whatever its spec's shape; a same-repository pull request's
// review session, and a session that claims no pull request, resolve as
// before.
func TestSandboxSecretsDelivery_AReviewSessionWhoseHeadIsNotInItsBaseGetsNoSecret(t *testing.T) {
	for i, tc := range reviewHeadCases() {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			repoName := fmt.Sprintf("review-head-secrets-%d-%d", i, time.Now().UnixNano())
			repoFullName := "acme/" + repoName
			if _, err := rig.sandboxSecrets.Create(ctx, sqlcgen.SandboxSecretScopeGlobal, nil, "GLOBAL_TOKEN", encryptSecretForTest(t, rig, "global-value")); err != nil {
				t.Fatalf("create the global secret: %v", err)
			}
			if _, err := rig.sandboxSecrets.Create(ctx, sqlcgen.SandboxSecretScopeRepo, &repoFullName, "DEPLOY_TOKEN", encryptSecretForTest(t, rig, "base-repo-value")); err != nil {
				t.Fatalf("create the base repository's secret: %v", err)
			}
			session := seedReviewHeadCase(ctx, t, rig, tc, repoName, int32(80+i))

			status, got := postSandboxSecrets(t, rig, session.ID.String(), "sandbox-bearer-token", "1")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d", status, http.StatusOK)
			}
			want := map[string]string{}
			switch {
			case tc.headInBase && tc.caseOnly:
				want = map[string]string{"GLOBAL_TOKEN": "global-value"}
			case tc.headInBase:
				want = map[string]string{"GLOBAL_TOKEN": "global-value", "DEPLOY_TOKEN": "base-repo-value"}
			}
			if got.Secrets == nil {
				got.Secrets = map[string]string{}
			}
			if !reflect.DeepEqual(got.Secrets, want) {
				t.Errorf("secrets = %v, want %v", got.Secrets, want)
			}
		})
	}
}

// TestSandboxSecretsDelivery_AFailedClaimReadDeliversNothing: whether a
// session is a pull request's review session is asked of github_pr_sessions,
// and when that read fails the answer is a 500 that delivers nothing --
// never "no claim", which would deliver the base repository's secrets to a
// fork's head. No integration test in this package runs in parallel, so no
// other test can observe the table renamed away.
func TestSandboxSecretsDelivery_AFailedClaimReadDeliversNothing(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoName := fmt.Sprintf("review-head-claim-read-%d", time.Now().UnixNano())
	repoFullName := "acme/" + repoName
	if _, err := rig.sandboxSecrets.Create(ctx, sqlcgen.SandboxSecretScopeRepo, &repoFullName, "DEPLOY_TOKEN", encryptSecretForTest(t, rig, "base-repo-value")); err != nil {
		t.Fatal(err)
	}
	session := seedReviewHeadCase(ctx, t, rig, reviewHeadCases()[0], repoName, 90)

	if _, err := rig.pool.Exec(ctx, `ALTER TABLE github_pr_sessions RENAME TO github_pr_sessions_unreadable_for_test`); err != nil {
		t.Fatalf("rename github_pr_sessions away: %v", err)
	}
	t.Cleanup(func() {
		if _, err := rig.pool.Exec(context.Background(), `ALTER TABLE IF EXISTS github_pr_sessions_unreadable_for_test RENAME TO github_pr_sessions`); err != nil {
			t.Errorf("restore github_pr_sessions: %v", err)
		}
	})
	status, got := postSandboxSecrets(t, rig, session.ID.String(), "sandbox-bearer-token", "1")
	if _, err := rig.pool.Exec(ctx, `ALTER TABLE github_pr_sessions_unreadable_for_test RENAME TO github_pr_sessions`); err != nil {
		t.Fatalf("restore github_pr_sessions: %v", err)
	}
	if status != http.StatusInternalServerError || len(got.Secrets) != 0 {
		t.Fatalf("status = %d, secrets = %v; want a 500 delivering nothing", status, got.Secrets)
	}
}

// TestProviderCredentialsDelivery_AReviewSessionWhoseHeadIsNotInItsBaseGetsNoRepoScopedCredential:
// the base repository's repository-scoped provider credential is resolved
// only for a review session whose head is a branch of its base repository;
// any other review session resolves the global credential, as it did while
// its spec named the fork.
func TestProviderCredentialsDelivery_AReviewSessionWhoseHeadIsNotInItsBaseGetsNoRepoScopedCredential(t *testing.T) {
	for i, tc := range reviewHeadCases() {
		if tc.caseOnly {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			repoName := fmt.Sprintf("review-head-providers-%d-%d", i, time.Now().UnixNano())
			repoFullName := "acme/" + repoName
			if _, err := rig.providerCredentials.Create(ctx, sqlcgen.ProviderCredentialScopeGlobal, nil, sqlcgen.ProviderCredentialProviderOpenai, encryptForTest(t, rig, "global-openai-key")); err != nil {
				t.Fatal(err)
			}
			if _, err := rig.providerCredentials.Create(ctx, sqlcgen.ProviderCredentialScopeRepo, &repoFullName, sqlcgen.ProviderCredentialProviderOpenai, encryptForTest(t, rig, "base-repo-openai-key")); err != nil {
				t.Fatal(err)
			}
			session := seedReviewHeadCase(ctx, t, rig, tc, repoName, int32(100+i))

			status, got := postProviderCredentials(t, rig, session.ID.String(), "sandbox-bearer-token", "1")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d", status, http.StatusOK)
			}
			want := "global-openai-key"
			if tc.headInBase {
				want = "base-repo-openai-key"
			}
			if got := apiKey(got.Credentials["openai"]); got != want {
				t.Errorf("openai credential = %q, want %q", got, want)
			}
		})
	}
}

// suggestedFixVerdictBody is a verdict whose one finding carries a
// suggested fix to internal/foo/bar.go.
const suggestedFixVerdictBody = `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"insufficient","docsDrift":"none","proposedShippable":"auto","summary":"s","findings":[{"source":"primary","severity":"low","filePath":"internal/foo/bar.go","description":"Stale comment.","suggestedFix":"--- a/internal/foo/bar.go\n+++ b/internal/foo/bar.go\n@@ -1,2 +1,2 @@\n package foo\n-// old comment\n+// new comment\n"}],"digest":{"summary":"Fixes a stale comment.","descriptionAdequacy":"ok","adequacyExplanation":"Accurate."},"factCheck":"done","factCheckKilled":0}`

// legacyGateCase is a review session the previous ingress opened, whose
// spec names a branch: apply-suggestion and the sentinel auto-fix act on
// the base repository at that branch, so each must refuse it.
type legacyGateCase struct {
	name string
	// spec is the session's sessions.repos, %[1]s the repository's name.
	spec string
	// record is the claim's head_in_base; nil for an unrecorded claim.
	record *bool
}

func legacyGateCases() []legacyGateCase {
	notInBase := false
	return []legacyGateCase{
		// Opened from contributor/<name>, on the fork's own main; its gen was
		// live when the migration ran, or a previous replica opened it.
		{name: "a legacy fork session not yet moved", spec: `[{"name":"%[1]s","url":"https://github.com/contributor/%[1]s.git","branch":"main"}]`},
		// Its fork deleted before its first mention: the base's url with the
		// deleted fork's main, byte for byte a same-repository spec.
		{name: "a legacy deleted-fork session, unrecorded", spec: `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":"main"}]`},
		{name: "a legacy deleted-fork session after a mention recorded its head not in the base", spec: `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":"main"}]`, record: &notInBase},
	}
}

// seedLegacyGateCase creates tc's session for acme/<repoName>, claimed and
// recorded as tc says, with a ready sandbox and a dispatched turn.
func seedLegacyGateCase(ctx context.Context, t *testing.T, rig testRig, tc legacyGateCase, repoName string, prNumber int32) sqlcgen.Session {
	t.Helper()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := rig.createOwnedGitHubReviewSessionWithRepos(ctx, t, owner.ID, "acme/"+repoName, prNumber, fmt.Sprintf(tc.spec, repoName))
	if tc.record != nil {
		if err := rig.prSessions.RecordHeadInBase(ctx, "acme/"+repoName, prNumber, *tc.record); err != nil {
			t.Fatal(err)
		}
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")
	moveSandboxStatus(ctx, t, rig, session.ID, sqlcgen.SandboxStatusReady)
	seedDispatchedTurn(ctx, t, rig, session.ID)
	return session
}

// TestApplySuggestion_LegacyForkSession_ConflictsAndCommitsNothing: a
// legacy review session whose spec names a branch the pull request's head
// is not known to be -- the fork's own main, or a deleted fork's main next
// to the base's url. Apply-suggestion commits to the claim's repository, the
// base, so taking that branch would commit to the base's own main. It
// answers the fork's 409 and touches nothing.
func TestApplySuggestion_LegacyForkSession_ConflictsAndCommitsNothing(t *testing.T) {
	for i, tc := range legacyGateCases() {
		t.Run(tc.name, func(t *testing.T) {
			fake := &applySuggestionFakeSourceControl{content: "package foo\n// old comment\n", sha: "blob-sha", exists: true}
			rig := newTestRig(t, func(r *testRig) { r.sourceControl = fake })
			ctx := context.Background()
			repoName := fmt.Sprintf("legacy-apply-%d-%d", i, time.Now().UnixNano())
			session := seedLegacyGateCase(ctx, t, rig, tc, repoName, int32(72+i))
			status, verdict := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, suggestedFixVerdictBody)
			if status != http.StatusCreated {
				t.Fatalf("post verdict status = %d, want %d", status, http.StatusCreated)
			}

			_, maintainerToken := createMaintainerWithGitHubToken(ctx, t, rig, "acting-maintainer-token")
			var resp struct {
				Error string `json:"error"`
			}
			got := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/findings/"+verdict.FindingIdentityHashes[0]+"/apply-suggestion", nil, &resp, maintainerToken)
			if got != http.StatusConflict {
				t.Fatalf("status = %d (%q), want %d", got, resp.Error, http.StatusConflict)
			}
			// The remedy for a claim not yet recorded: a mention records it.
			if !strings.Contains(resp.Error, "mention") {
				t.Errorf("message %q does not say a mention records the head", resp.Error)
			}
			if fake.updateCalls != 0 {
				t.Errorf("UpdateFileContent called %d times (last %+v), want 0: nothing is committed to the base repository's main", fake.updateCalls, fake.lastUpdate)
			}
		})
	}
}

// TestReviewVerdict_LegacyForkSessionCutsNoSentinelFixBranch: with the
// sentinel auto-fix on, the same legacy sessions claim no fix and enqueue
// nothing: a fix cut from a branch that is not the pull request's head in
// the base would fix something else.
func TestReviewVerdict_LegacyForkSessionCutsNoSentinelFixBranch(t *testing.T) {
	for i, tc := range legacyGateCases() {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			repoName := fmt.Sprintf("legacy-sentinel-%d-%d", i, time.Now().UnixNano())
			repoFullName := "acme/" + repoName
			if _, err := rig.repoSettings.Upsert(ctx, repoFullName, false, true); err != nil {
				t.Fatalf("turn the sentinel auto-fix on: %v", err)
			}
			session := seedLegacyGateCase(ctx, t, rig, tc, repoName, int32(81+i))

			status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1",
				testDispatchMessageID, findingsVerdictRequestJSON("coverage", "Missing test for the retry path."))
			if status != http.StatusCreated {
				t.Fatalf("status = %d, want %d", status, http.StatusCreated)
			}
			if fix, err := rig.sentinelFixes.Get(ctx, repoFullName, int32(81+i)); err == nil {
				t.Fatalf("sentinel fix %+v claimed for a legacy session, want none", fix)
			}
			var queued int
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindSentinelAutoFix)).Scan(&queued); err != nil {
				t.Fatal(err)
			}
			if queued != 0 {
				t.Errorf("sentinel auto-fix outbox rows = %d, want 0", queued)
			}
		})
	}
}
