//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapp"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
)

// This file pins technical plan §30.4's readers of a review session's spec
// for a pull request from a fork, once the spec names the pull request's
// base repository (the GitHub ingress's reviewSessionRepo; legacy sessions
// moved by the review_sessions_base_repository migration or at their next
// boot): the credential is minted on the base repository's installation,
// read-only, in every mode; the sentinel auto-fix cuts no fix branch; and
// apply-suggestion answers a conflict naming the fork rather than an
// internal error.

// forkSpec is the spec of a review session of acme/widgets#n opened from
// contributor/widgets: the base repository, no branch.
func forkSpec(repoName string) string {
	return `[{"name":"` + repoName + `","url":"https://github.com/acme/` + repoName + `.git","branch":null}]`
}

// ownerMinter is a ReadOnlyMinter that knows two accounts: the base
// repository's owner, acme, has the GitHub App installed and is granted
// grant; the contributor, whose fork the pull request comes from, never
// installed it.
type ownerMinter struct {
	grant map[string]string

	mu     sync.Mutex
	owners []string
	repos  [][]string
}

func (m *ownerMinter) MintInstallationToken(_ context.Context, owner string, repoNames []string) (githubapp.Token, error) {
	m.mu.Lock()
	m.owners = append(m.owners, owner)
	m.repos = append(m.repos, repoNames)
	m.mu.Unlock()
	if owner != "acme" {
		repo := ""
		if len(repoNames) > 0 {
			repo = repoNames[0]
		}
		return githubapp.Token{}, &githubapp.InstallationNotFoundError{Owner: owner, Repo: repo}
	}
	return githubapp.Token{Value: "ghs_acmeReadOnlyInstallationToken", ExpiresAt: time.Now().Add(time.Hour), Permissions: m.grant}, nil
}

// TestScmCredentials_ForkPullRequestReviewSession_MintsTheBaseInstallationReadOnly_InEveryMode:
// a fork pull request's review session asks for a credential, in each
// mode a review session's credential is minted in -- a live repository, a
// shadow repository, a deployment in shadow, a build boot. The mint is
// always asked of the base repository's owner, for the base repository,
// never the contributor's account, where the App is not installed; a
// read-only grant is served, and a grant that can write is refused.
func TestScmCredentials_ForkPullRequestReviewSession_MintsTheBaseInstallationReadOnly_InEveryMode(t *testing.T) {
	modes := []struct {
		name           string
		live           bool
		platformShadow bool
		buildBoot      bool
	}{
		{name: "a live repository", live: true},
		{name: "a shadow repository"},
		{name: "a deployment in shadow", live: true, platformShadow: true},
		{name: "a build boot", live: true, buildBoot: true},
	}
	grants := []struct {
		name       string
		grant      map[string]string
		wantStatus int
	}{
		{name: "read-only", grant: map[string]string{"contents": "read", "metadata": "read"}, wantStatus: http.StatusOK},
		{name: "contents write", grant: map[string]string{"contents": "write", "metadata": "read"}, wantStatus: http.StatusForbidden},
	}
	for i, mode := range modes {
		for j, g := range grants {
			t.Run(mode.name+", "+g.name, func(t *testing.T) {
				minter := &ownerMinter{grant: g.grant}
				rig := newTestRig(t, func(r *testRig) {
					r.readOnlyMinter = minter
					r.platformShadow = mode.platformShadow
				})
				ctx := context.Background()

				repoName := fmt.Sprintf("fork-pr-mint-%d-%d-%d", i, j, time.Now().UnixNano())
				spec := forkSpec(repoName)
				if mode.live {
					promoteRepoLive(ctx, t, rig, spec)
				}
				session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, Repos: []byte(spec)})
				if err != nil {
					t.Fatalf("create the review session: %v", err)
				}
				if err := rig.prSessions.EnsureRow(ctx, "acme/"+repoName, 7); err != nil {
					t.Fatal(err)
				}
				if err := rig.prSessions.SetSessionID(ctx, "acme/"+repoName, 7, session.ID); err != nil {
					t.Fatal(err)
				}
				createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

				var status int
				var got scmCredResponse
				if mode.buildBoot {
					status, got = postScmCredentialsForceReadOnly(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "github.com")
				} else {
					status, got = postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
				}
				if status != g.wantStatus {
					t.Fatalf("status = %d, want %d", status, g.wantStatus)
				}
				minter.mu.Lock()
				owners, repos := minter.owners, minter.repos
				minter.mu.Unlock()
				if len(owners) != 1 || owners[0] != "acme" || len(repos[0]) != 1 || repos[0][0] != repoName {
					t.Fatalf("mints asked of %v for %v, want one, of acme, for [%s]: the base repository's installation, never the contributor's", owners, repos, repoName)
				}
				if g.wantStatus == http.StatusOK {
					if got.Username != "x-access-token" || got.Password != "ghs_acmeReadOnlyInstallationToken" {
						t.Errorf("credential = %q/%q, want the base installation's read-only token", got.Username, got.Password)
					}
				} else if got.Password != "" {
					t.Errorf("Password = %q on a %d, want none: a grant that can write is never served", got.Password, status)
				}
			})
		}
	}
}

// TestReviewVerdict_ForkPullRequestCutsNoSentinelFixBranch: with the
// sentinel auto-fix on, a review session that names its pull request's
// head branch claims a fix and enqueues the auto-fix; a fork's pull
// request's session, whose spec names no branch -- a fork's branch names
// nothing in the base repository a fix branch would be cut from -- claims
// none and enqueues nothing, and the verdict is posted all the same.
func TestReviewVerdict_ForkPullRequestCutsNoSentinelFixBranch(t *testing.T) {
	for i, tc := range []struct {
		name      string
		branch    string // "" for none
		wantClaim bool
	}{
		{name: "a pull request from a branch of the base repository", branch: "feature-branch", wantClaim: true},
		{name: "a pull request from a fork"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			repoName := fmt.Sprintf("fork-pr-sentinel-%d", i)
			repoFullName := "acme/" + repoName
			spec := forkSpec(repoName)
			if tc.branch != "" {
				spec = strings.Replace(spec, `"branch":null`, `"branch":"`+tc.branch+`"`, 1)
			}
			owner, _ := rig.createAuthenticatedUser(ctx, t)
			session := rig.createOwnedGitHubReviewSessionWithRepos(ctx, t, owner.ID, repoFullName, 70, spec)
			createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")
			seedDispatchedTurn(ctx, t, rig, session.ID)
			if _, err := rig.repoSettings.Upsert(ctx, repoFullName, false, true); err != nil {
				t.Fatalf("turn the sentinel auto-fix on: %v", err)
			}

			status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1",
				testDispatchMessageID, findingsVerdictRequestJSON("coverage", "Missing test for the retry path."))
			if status != http.StatusCreated {
				t.Fatalf("status = %d, want %d", status, http.StatusCreated)
			}

			fix, err := rig.sentinelFixes.Get(ctx, repoFullName, 70)
			if tc.wantClaim {
				if err != nil || fix.OriginHeadBranch != tc.branch {
					t.Fatalf("sentinel fix = %+v (%v), want one claimed from %s", fix, err, tc.branch)
				}
			} else if err == nil {
				t.Fatalf("sentinel fix %+v claimed for a fork's pull request, want none", fix)
			}
			var queued int
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindSentinelAutoFix)).Scan(&queued); err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.wantClaim]; queued != want {
				t.Errorf("sentinel auto-fix outbox rows = %d, want %d", queued, want)
			}
		})
	}
}

// TestApplySuggestion_ForkPullRequest_ConflictNamesTheFork: a fork's pull
// request keeps its head in the fork, which apply-suggestion never writes
// to -- it commits to the base repository the claim names, and the spec
// names no branch of it. Applying a suggestion there is a conflict with
// the pull request's shape, answered 409 with a message naming the fork
// and the base repository, before any credential or code-host read -- never
// the 500 a missing branch used to be, and never a commit to a same-named
// branch of the base.
func TestApplySuggestion_ForkPullRequest_ConflictNamesTheFork(t *testing.T) {
	fake := &applySuggestionFakeSourceControl{content: "package foo\n// old comment\n", sha: "blob-sha", exists: true}
	rig := newTestRig(t, func(r *testRig) { r.sourceControl = fake })
	ctx := context.Background()
	const repoFullName = "acme/fork-pr-apply"
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := rig.createOwnedGitHubReviewSessionWithRepos(ctx, t, owner.ID, repoFullName, 71, forkSpec("fork-pr-apply"))
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")
	seedDispatchedTurn(ctx, t, rig, session.ID)
	status, verdict := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, suggestedFixVerdictBody)
	if status != http.StatusCreated {
		t.Fatalf("post verdict status = %d, want %d", status, http.StatusCreated)
	}

	for _, withCredential := range []bool{true, false} {
		t.Run(map[bool]string{true: "a maintainer with a credential", false: "a maintainer with none"}[withCredential], func(t *testing.T) {
			var maintainerToken string
			if withCredential {
				_, maintainerToken = createMaintainerWithGitHubToken(ctx, t, rig, "acting-maintainer-token")
			} else {
				_, maintainerToken = createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
			}
			var resp struct {
				Error string `json:"error"`
			}
			got := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/findings/"+verdict.FindingIdentityHashes[0]+"/apply-suggestion", nil, &resp, maintainerToken)
			if got != http.StatusConflict {
				t.Fatalf("status = %d, want %d", got, http.StatusConflict)
			}
			for _, want := range []string{"fork", repoFullName} {
				if !strings.Contains(resp.Error, want) {
					t.Errorf("message %q does not name %q", resp.Error, want)
				}
			}
			if fake.updateCalls != 0 {
				t.Errorf("UpdateFileContent called %d times, want 0: nothing is committed to the base repository", fake.updateCalls)
			}
		})
	}

	row, err := rig.reviewFindings.Get(ctx, repoFullName, 71, verdict.FindingIdentityHashes[0])
	if err != nil {
		t.Fatal(err)
	}
	if row.Status == "fix_applied" || row.Status == "fix_recorded" {
		t.Errorf("finding status = %q, want it untouched", row.Status)
	}
}
