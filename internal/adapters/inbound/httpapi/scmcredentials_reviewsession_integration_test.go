//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapp"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/provenance"
	"github.com/narvidev/narvi/internal/platform"
)

// createReviewCredentialCreator creates a member with a linked GitHub
// identity whose stored (encrypted) OAuth token is personalToken -- a
// creator whose own credential a non-review session would be served.
func createReviewCredentialCreator(ctx context.Context, t *testing.T, rig testRig, personalToken string) sqlcgen.User {
	t.Helper()
	externalID := fmt.Sprintf("review-credential-creator-%d", time.Now().UnixNano())
	user, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: externalID + "@example.com", DisplayName: "Review Credential Creator", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	encrypted, err := platform.EncryptToken(rig.tokenEncryptionKey, []byte(personalToken))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	email := externalID + "@example.com"
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: externalID,
		Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin, AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	return user
}

// TestScmCredentials_ReviewSession_ReceivesReadOnlyCredential: a pull
// request's review session is read-only, so its sandbox is only ever handed
// the §30.4 read-only GitHub App installation token -- on a LIVE
// repository too, not only in shadow -- minted through the same
// scope-checked path as the shadow substitution, and never the bot token or
// the creator's own token. Every row's repository is promoted live, so
// nothing here is the shadow branch doing the work.
//
// A token GitHub grants with more than read access is refused, and a mint
// that fails is a 500: neither falls back to a write-capable credential. A
// review session's credential is not a shadow-mode substitution, so no row
// reaches the shadow ledger on any outcome, and a failing ledger does not
// fail the credential.
func TestScmCredentials_ReviewSession_ReceivesReadOnlyCredential(t *testing.T) {
	const botToken = "bot-token-must-never-reach-a-review-sandbox"
	const personalToken = "gho_creatorsOwnPersonalToken"

	tests := []struct {
		name string
		// withCreator: the session has a creator with a linked GitHub
		// identity and a stored personal token.
		withCreator bool
		// grant is what the fake GitHub App mint reports granting.
		grant   map[string]string
		mintErr error
		// ledgerFails: every write to the shadow ledger fails.
		ledgerFails bool

		wantStatus int
	}{
		{
			name: "creator with a personal token", withCreator: true,
			grant: map[string]string{"contents": "read", "metadata": "read"}, wantStatus: http.StatusOK,
		},
		{
			name: "no creator", withCreator: false,
			grant: map[string]string{"contents": "read", "metadata": "read"}, wantStatus: http.StatusOK,
		},
		{
			name: "the shadow ledger is failing: served all the same", withCreator: true,
			grant: map[string]string{"contents": "read", "metadata": "read"}, ledgerFails: true, wantStatus: http.StatusOK,
		},
		{
			name: "the minted token can write: refused", withCreator: true,
			grant: map[string]string{"contents": "write", "metadata": "read"}, wantStatus: http.StatusForbidden,
		},
		{
			name: "the mint fails: an error, never a fallback", withCreator: true,
			mintErr: errors.New("simulated GitHub App outage"), wantStatus: http.StatusInternalServerError,
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			minter := newFakeReadOnlyMinter()
			minter.Token.Permissions = tc.grant
			minter.Err = tc.mintErr
			rig := newTestRig(t, func(r *testRig) {
				r.botToken = botToken
				r.readOnlyMinter = minter
				if tc.ledgerFails {
					r.shadowLedger = failingLedgerStore{}
				}
			})
			ctx := context.Background()

			repoName := fmt.Sprintf("review-credential-%d-%d", i, time.Now().UnixNano())
			repoFullName := "review-owner/" + repoName
			reposJSON := `[{"name":"` + repoName + `","url":"https://github.com/` + repoFullName + `.git","branch":"feature-x"}]`

			var creator pgtype.UUID
			if tc.withCreator {
				creator = createReviewCredentialCreator(ctx, t, rig, personalToken).ID
			}
			// Promotes the repository live, then writes the
			// github_pr_sessions row that makes this a review session.
			session := rig.createOwnedGitHubReviewSessionWithRepos(ctx, t, creator, repoFullName, int32(40+i), reposJSON)
			createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

			status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
			if got.Password == botToken {
				t.Fatal("a review sandbox received the bot token, a write-capable credential")
			}
			if got.Password == personalToken {
				t.Fatal("a review sandbox received its creator's own personal GitHub token")
			}
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			if minter.CallCount != 1 || minter.SawOwner != "review-owner" || len(minter.SawRepoNames) != 1 || minter.SawRepoNames[0] != repoName {
				t.Errorf("minter called %d times with owner=%q repoNames=%v, want once with owner=review-owner repoNames=[%s]",
					minter.CallCount, minter.SawOwner, minter.SawRepoNames, repoName)
			}
			if tc.wantStatus == http.StatusOK {
				if got.Username != "x-access-token" || got.Password != minter.Token.Value {
					t.Errorf("credential = %q/%q, want x-access-token and the minted read-only token", got.Username, got.Password)
				}
			} else if got.Password != "" {
				t.Errorf("Password = %q on a %d, want none", got.Password, status)
			}

			rows, err := narvipg.NewShadowSCMWriteStore(rig.pool).ListForRepo(ctx, repoFullName, 10)
			if err != nil {
				t.Fatalf("ListForRepo: %v", err)
			}
			if len(rows) != 0 {
				t.Errorf("shadow ledger rows = %+v, want none: a review session's credential is not a shadow-mode substitution", rows)
			}
		})
	}
}

// TestScmCredentials_ReadOnlyMint_OnlyShadowAndBuildBootAreRecorded pins
// which read-only mints reach the shadow ledger. A shadow sandbox and a
// build boot are shadow-mode substitutions and are recorded exactly as
// before -- a review session among them, on a shadow repository or in a
// build boot. A review session on a live repository is read-only for its
// own reason, not shadow, and records nothing.
func TestScmCredentials_ReadOnlyMint_OnlyShadowAndBuildBootAreRecorded(t *testing.T) {
	tests := []struct {
		name string
		// review: the session has a github_pr_sessions row.
		review bool
		// live: the repository is promoted to live egress.
		live bool
		// buildBoot: the request carries forceReadOnly, as a build boot's
		// does.
		buildBoot    bool
		wantRecorded bool
	}{
		{name: "review session, live repository: not recorded", review: true, live: true},
		{name: "review session, shadow repository: recorded", review: true, wantRecorded: true},
		{name: "review session, build boot on a live repository: recorded", review: true, live: true, buildBoot: true, wantRecorded: true},
		{name: "not a review session, shadow repository: recorded", wantRecorded: true},
		{name: "not a review session, build boot on a live repository: recorded", live: true, buildBoot: true, wantRecorded: true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			minter := newFakeReadOnlyMinter()
			rig := newTestRig(t, func(r *testRig) { r.readOnlyMinter = minter })
			ctx := context.Background()

			repoName := fmt.Sprintf("ledger-scope-%d-%d", i, time.Now().UnixNano())
			repoFullName := "ledger-owner/" + repoName
			reposJSON := `[{"name":"` + repoName + `","url":"https://github.com/` + repoFullName + `.git","branch":"feature-x"}]`
			if tc.live {
				promoteRepoLive(ctx, t, rig, reposJSON)
			}
			session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
				SpawnSource: sqlcgen.SessionSpawnSourceGithub,
				Repos:       []byte(reposJSON),
			})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			if tc.review {
				if err := rig.prSessions.EnsureRow(ctx, repoFullName, int32(60+i)); err != nil {
					t.Fatalf("ensure github_pr_sessions row: %v", err)
				}
				if err := rig.prSessions.SetSessionID(ctx, repoFullName, int32(60+i), session.ID); err != nil {
					t.Fatalf("set github_pr_sessions session id: %v", err)
				}
			}
			createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

			var status int
			var got scmCredResponse
			if tc.buildBoot {
				status, got = postScmCredentialsForceReadOnly(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "github.com")
			} else {
				status, got = postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
			}
			if status != http.StatusOK || got.Password != minter.Token.Value {
				t.Fatalf("status = %d, password = %q; want 200 and the minted read-only token", status, got.Password)
			}

			rows, err := narvipg.NewShadowSCMWriteStore(rig.pool).ListForRepo(ctx, repoFullName, 10)
			if err != nil {
				t.Fatalf("ListForRepo: %v", err)
			}
			if !tc.wantRecorded {
				if len(rows) != 0 {
					t.Fatalf("shadow ledger rows = %+v, want none", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0].Operation != "scm_credential_substituted" || rows[0].SessionID != session.ID {
				t.Fatalf("shadow ledger rows = %+v, want exactly one scm_credential_substituted for this session", rows)
			}
			if strings.Contains(string(rows[0].SpecJson), minter.Token.Value) {
				t.Errorf("ledger spec %s carries the token's own value", rows[0].SpecJson)
			}
		})
	}
}

// TestScmCredentials_SentinelFixChild_IsNotServedAsAReviewSession: the
// sentinel auto-fix child is spawned by a review session but is not one --
// it has no github_pr_sessions row of its own, and no creator
// (internal/app/outboxworker's sentinelautofix.go). So the read-only rule
// for review sessions does not reach it: on a live repository its request
// takes the same path it took before that rule existed, which, with no
// creator, is step 8's refusal. Pinned so that any change to what this
// child is served is a deliberate one.
func TestScmCredentials_SentinelFixChild_IsNotServedAsAReviewSession(t *testing.T) {
	const botToken = "bot-token-must-never-reach-a-sandbox"
	minter := newFakeReadOnlyMinter()
	rig := newTestRig(t, func(r *testRig) {
		r.botToken = botToken
		r.readOnlyMinter = minter
	})
	ctx := context.Background()

	const reposJSON = `[{"name":"widgets","url":"https://github.com/sentinel-owner/widgets.git","branch":"feature-fix-me"}]`
	parent := rig.createOwnedGitHubReviewSessionWithRepos(ctx, t, pgtype.UUID{}, "sentinel-owner/widgets", 9, reposJSON)

	tag := provenance.SentinelAutoFix
	child, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource:     sqlcgen.SessionSpawnSourceGithub,
		Repos:           []byte(`[{"name":"widgets","url":"https://github.com/sentinel-owner/widgets.git","branch":"narvi/sentinel-fix/1"}]`),
		ProvenanceTag:   &tag,
		ParentSessionID: parent.ID,
		SpawnDepth:      1,
	})
	if err != nil {
		t.Fatalf("create sentinel fix child: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, child.ID, "sandbox-bearer-token")

	status, got := postScmCredentials(t, rig, child.ID.String(), "sandbox-bearer-token")
	if got.Password == botToken {
		t.Fatal("the sentinel fix child received the bot token")
	}
	if minter.CallCount != 0 {
		t.Errorf("minter called %d times, want 0: the child is not a review session", minter.CallCount)
	}
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d: no creator, so step 8 refuses, as before the review-session rule", status, http.StatusForbidden)
	}
}

// TestScmCredentials_ReviewSession_GitHubAppNotInstalled: when the GitHub
// App is not installed on the repository a review sandbox clones (for a
// pull request from a fork, the fork owner's account), the credential is
// still refused -- never another credential in its place -- but the
// refusal is explicit: a 403, not a bare 500, and a session-visible
// warning naming the repository and that the App must be installed on it
// with read access. git asks again on every fetch, so the warning is
// recorded once per session and repository, and broadcast once.
func TestScmCredentials_ReviewSession_GitHubAppNotInstalled(t *testing.T) {
	minter := newFakeReadOnlyMinter()
	broadcaster := &recordingBroadcaster{}
	rig := newTestRig(t, func(r *testRig) {
		r.readOnlyMinter = minter
		r.broadcaster = broadcaster
	})
	ctx := context.Background()

	repoName := fmt.Sprintf("not-installed-%d", time.Now().UnixNano())
	repoFullName := "fork-owner/" + repoName
	minter.Err = &githubapp.InstallationNotFoundError{Owner: "fork-owner", Repo: repoName}
	reposJSON := `[{"name":"` + repoName + `","url":"https://github.com/` + repoFullName + `.git","branch":"feature-x"}]`
	session := rig.createOwnedGitHubReviewSessionWithRepos(ctx, t, pgtype.UUID{}, "base-owner/"+repoName, 51, reposJSON)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	for i := 0; i < 2; i++ {
		status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
		if status != http.StatusForbidden {
			t.Fatalf("request %d: status = %d, want %d", i+1, status, http.StatusForbidden)
		}
		if got.Username != "" || got.Password != "" {
			t.Fatalf("request %d: credential %q/%q served, want none", i+1, got.Username, got.Password)
		}
	}
	if minter.CallCount != 2 {
		t.Errorf("minter called %d times, want 2: each request mints afresh", minter.CallCount)
	}

	events, err := rig.events.ListForSession(ctx, session.ID, 0, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var warnings []sandboxws.Warning
	for _, e := range events {
		if e.Type != "warning" {
			continue
		}
		var w sandboxws.Warning
		if err := json.Unmarshal(e.Payload, &w); err != nil {
			t.Fatalf("decode warning %s: %v", e.Payload, err)
		}
		warnings = append(warnings, w)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %+v, want exactly one for this session and repository", warnings)
	}
	w := warnings[0]
	if w.SessionId != session.ID.String() || w.Gen != 1 {
		t.Errorf("warning session/gen = %s/%d, want %s/1", w.SessionId, w.Gen, session.ID)
	}
	for _, want := range []string{repoFullName, "GitHub App", "read access", "fork"} {
		if !strings.Contains(w.Message, want) {
			t.Errorf("warning %q does not mention %q", w.Message, want)
		}
	}
	if got := broadcaster.all(); len(got) != 1 || !strings.Contains(got[0], repoFullName) {
		t.Errorf("broadcasts = %v, want exactly the one warning", got)
	}

	rows, err := narvipg.NewShadowSCMWriteStore(rig.pool).ListForRepo(ctx, repoFullName, 10)
	if err != nil {
		t.Fatalf("ListForRepo: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("shadow ledger rows = %+v, want none", rows)
	}
}

// TestScmCredentials_ReviewSession_OtherMintFailureIsStill500: only a
// missing installation is named; any other mint failure stays a 500 with
// no warning, as before.
func TestScmCredentials_ReviewSession_OtherMintFailureIsStill500(t *testing.T) {
	minter := newFakeReadOnlyMinter()
	minter.Err = errors.New("simulated GitHub App outage")
	rig := newTestRig(t, func(r *testRig) { r.readOnlyMinter = minter })
	ctx := context.Background()

	repoName := fmt.Sprintf("mint-outage-%d", time.Now().UnixNano())
	reposJSON := `[{"name":"` + repoName + `","url":"https://github.com/review-owner/` + repoName + `.git","branch":"feature-x"}]`
	session := rig.createOwnedGitHubReviewSessionWithRepos(ctx, t, pgtype.UUID{}, "review-owner/"+repoName, 52, reposJSON)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	if status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token"); status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", status, http.StatusInternalServerError)
	}
	events, err := rig.events.ListForSession(ctx, session.ID, 0, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range events {
		if e.Type == "warning" {
			t.Errorf("warning recorded (%s) for a mint failure that is not a missing installation", e.Payload)
		}
	}
}
