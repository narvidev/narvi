//go:build integration

package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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
// A token GitHub grants with more than read access is refused and the
// refusal recorded, and a mint that fails is a 500: neither falls back to a
// write-capable credential.
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

		wantStatus int
		// wantLedgerOperation is the shadow_scm_writes row the request
		// must leave for this session, "" for none.
		wantLedgerOperation string
	}{
		{
			name: "creator with a personal token", withCreator: true,
			grant: map[string]string{"contents": "read", "metadata": "read"}, wantStatus: http.StatusOK,
			wantLedgerOperation: "scm_credential_substituted",
		},
		{
			name: "no creator", withCreator: false,
			grant: map[string]string{"contents": "read", "metadata": "read"}, wantStatus: http.StatusOK,
			wantLedgerOperation: "scm_credential_substituted",
		},
		{
			name: "the minted token can write: refused and recorded", withCreator: true,
			grant: map[string]string{"contents": "write", "metadata": "read"}, wantStatus: http.StatusForbidden,
			wantLedgerOperation: "scm_credential_mint_refused",
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
			if tc.wantLedgerOperation == "" {
				if len(rows) != 0 {
					t.Errorf("ledger rows = %d, want none", len(rows))
				}
				return
			}
			if len(rows) != 1 || rows[0].Operation != tc.wantLedgerOperation || rows[0].SessionID != session.ID {
				t.Fatalf("ledger rows = %+v, want exactly one %q for this session", rows, tc.wantLedgerOperation)
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
