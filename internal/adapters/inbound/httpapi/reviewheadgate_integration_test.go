//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
)

// This file pins the readers that act on what a review session's head may
// reach, through the real endpoints (technical plan §27.1, §30.4): a pull
// request's review session whose head is not known to be a branch of its
// base repository -- reposource.ReviewHeadInBaseRepository: a fork's pull
// request, a deleted fork's, a head not read when the review started, a
// legacy session still naming the fork -- runs code a person outside the
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
	// headInBase is what reposource.ReviewHeadInBaseRepository reads.
	headInBase bool
	// caseOnly: the spec's url names the base in another case, so the
	// repository-scoped rows, keyed on the exact name, do not match it.
	caseOnly bool
}

func reviewHeadCases() []reviewHeadCase {
	return []reviewHeadCase{
		{name: "a fork's pull request: the base, no branch", spec: `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":null}]`, claimed: true},
		{name: "a legacy fork session already moved onto the base", spec: `[{"name":"%[1]s-fork","url":"https://github.com/acme/%[1]s.git","branch":null}]`, claimed: true},
		{name: "a legacy fork session not yet moved: the fork, its branch", spec: `[{"name":"%[1]s-fork","url":"https://github.com/contributor/%[1]s-fork.git","branch":"main"}]`, claimed: true},
		{name: "a same-repository pull request whose head lookup failed", spec: `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":null}]`, claimed: true},
		{name: "a same-repository pull request", spec: `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":"feature-x"}]`, claimed: true, headInBase: true},
		{name: "a same-repository pull request, its url in another case", spec: `[{"name":"%[1]s","url":"https://github.com/ACME/%[1]s","branch":"feature-x"}]`, claimed: true, headInBase: true, caseOnly: true},
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

// legacyForkSpec is the spec a review session of acme/<name> opened from
// contributor/<name> had before its spec named the base repository: the
// fork, on the fork's own main. A session whose gen was live when the
// migration ran, or one a previous replica opened during the rolling
// deploy, keeps it until its next spawn or restore.
func legacyForkSpec(repoName string) string {
	return `[{"name":"` + repoName + `","url":"https://github.com/contributor/` + repoName + `.git","branch":"main"}]`
}

// TestApplySuggestion_LegacyForkSession_ConflictsAndCommitsNothing: a
// legacy review session not yet moved names the fork and the fork's main.
// Apply-suggestion commits to the claim's repository, the base, so taking
// that branch would commit to the base's own main. It answers the fork's
// 409 and touches nothing.
func TestApplySuggestion_LegacyForkSession_ConflictsAndCommitsNothing(t *testing.T) {
	fake := &applySuggestionFakeSourceControl{content: "package foo\n// old comment\n", sha: "blob-sha", exists: true}
	rig := newTestRig(t, func(r *testRig) { r.sourceControl = fake })
	ctx := context.Background()
	const repoName = "legacy-fork-apply"
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := rig.createOwnedGitHubReviewSessionWithRepos(ctx, t, owner.ID, "acme/"+repoName, 72, legacyForkSpec(repoName))
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")
	moveSandboxStatus(ctx, t, rig, session.ID, sqlcgen.SandboxStatusReady)
	seedDispatchedTurn(ctx, t, rig, session.ID)
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
	if fake.updateCalls != 0 {
		t.Errorf("UpdateFileContent called %d times (last %+v), want 0: nothing is committed to the base repository's main", fake.updateCalls, fake.lastUpdate)
	}
}

// TestReviewVerdict_LegacyForkSessionCutsNoSentinelFixBranch: with the
// sentinel auto-fix on, a legacy review session not yet moved -- the fork,
// the fork's main -- claims no fix and enqueues nothing: a fix child would
// act on the fork, at a branch that is not the pull request's head in the
// base.
func TestReviewVerdict_LegacyForkSessionCutsNoSentinelFixBranch(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	const repoName = "legacy-fork-sentinel"
	repoFullName := "acme/" + repoName
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := rig.createOwnedGitHubReviewSessionWithRepos(ctx, t, owner.ID, repoFullName, 81, legacyForkSpec(repoName))
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")
	moveSandboxStatus(ctx, t, rig, session.ID, sqlcgen.SandboxStatusReady)
	seedDispatchedTurn(ctx, t, rig, session.ID)
	if _, err := rig.repoSettings.Upsert(ctx, repoFullName, false, true); err != nil {
		t.Fatalf("turn the sentinel auto-fix on: %v", err)
	}

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1",
		testDispatchMessageID, findingsVerdictRequestJSON("coverage", "Missing test for the retry path."))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if fix, err := rig.sentinelFixes.Get(ctx, repoFullName, 81); err == nil {
		t.Fatalf("sentinel fix %+v claimed for a legacy fork session, want none", fix)
	}
	var queued int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindSentinelAutoFix)).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Errorf("sentinel auto-fix outbox rows = %d, want 0", queued)
	}
}
