//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
)

// This file proves, on real Postgres, that a pull request's review session
// never resolves its requester's personal provider link (technical plan
// §29.4, §45.1): the sandbox's credential delivery serves the deployment's
// credential instead, and the counter-reviewer's credential read -- the
// other reader of the same resolution -- agrees with it on every kind of
// session.

// linkChatGPT gives userID a personal ChatGPT link whose access token is
// access, exactly as the link flow stores it.
func linkChatGPT(ctx context.Context, t *testing.T, rig testRig, userID pgtype.UUID, access string) {
	t.Helper()
	blob := fmt.Sprintf(`{"access":%q,"refresh":"refresh-never-sent","expires_ms":1234567890123,"account_id":"acct-%s"}`, access, access)
	if _, err := rig.providerCredentials.UpsertOAuth(ctx, userID.String(), sqlcgen.ProviderCredentialProviderOpenai, encryptForTest(t, rig, blob), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("UpsertOAuth: %v", err)
	}
}

// createReviewSession creates the session the GitHub ingress creates for a
// pull request's first linked requester, and the github_pr_sessions row
// naming it.
func createReviewSession(ctx context.Context, t *testing.T, rig testRig, requester pgtype.UUID, prNumber int32) sqlcgen.Session {
	t.Helper()
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		CreatedBy:   requester,
		Repos:       []byte(providerCredsRepos),
	})
	if err != nil {
		t.Fatalf("create review session: %v", err)
	}
	if err := rig.prSessions.EnsureRow(ctx, "acme/widgets", prNumber); err != nil {
		t.Fatalf("ensure github pr session row: %v", err)
	}
	if err := rig.prSessions.SetSessionID(ctx, "acme/widgets", prNumber, session.ID); err != nil {
		t.Fatalf("set github pr session id: %v", err)
	}
	return session
}

// TestProviderCredentialsDelivery_ReviewSession_ServesTheDeploymentCredential
// is the row's first exit criterion: a review session opened by a member
// with a linked ChatGPT account resolves the deployment's openai key, never
// the member's link, and with no deployment key for openai it serves no
// openai credential at all rather than the link.
func TestProviderCredentialsDelivery_ReviewSession_ServesTheDeploymentCredential(t *testing.T) {
	tests := []struct {
		name          string
		deploymentKey string // "" = no deployment openai credential
		wantOpenAIKey string // "" = openai absent from the response
	}{
		{name: "deployment key present", deploymentKey: "deployment-openai-key", wantOpenAIKey: "deployment-openai-key"},
		{name: "no deployment key", deploymentKey: ""},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()

			member, _ := rig.createAuthenticatedUser(ctx, t)
			linkChatGPT(ctx, t, rig, member.ID, "members-own-seat")
			if tc.deploymentKey != "" {
				if _, err := rig.providerCredentials.Create(ctx, sqlcgen.ProviderCredentialScopeGlobal, nil, sqlcgen.ProviderCredentialProviderOpenai, encryptForTest(t, rig, tc.deploymentKey)); err != nil {
					t.Fatalf("create global credential: %v", err)
				}
			}
			session := createReviewSession(ctx, t, rig, member.ID, int32(200+i))
			createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

			status, rawBody := postProviderCredentialsRaw(t, rig, session.ID.String(), "sandbox-bearer-token", "1")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d", status, http.StatusOK)
			}
			var got providerCredsResponse
			if err := json.Unmarshal([]byte(rawBody), &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			entry, present := got.Credentials["openai"]
			if tc.wantOpenAIKey == "" {
				if present {
					t.Errorf("Credentials[openai] = %+v, want absent: a review session never borrows its requester's seat", entry)
				}
				return
			}
			if entry.Type != "api" || apiKey(entry) != tc.wantOpenAIKey {
				t.Errorf("Credentials[openai] = %+v, want the deployment's api key %q, never the member's oauth link", entry, tc.wantOpenAIKey)
			}
		})
	}
}

// TestProviderCredentialResolution_SitesAgree pins the one rule across both
// of its readers, over every kind of session: for each, the provider set
// the sandbox's credential delivery serves and the provider set the
// counter-reviewer's credential read counts are the same set, and it is
// the set the kind should reach. The deployment holds a global anthropic
// key; every creator and participant holds a personal openai link and a
// linked GitHub identity, so only the session's kind -- read from its
// github_pr_sessions row and its parent, never from its spawn source or
// its creator -- tells the cases apart.
func TestProviderCredentialResolution_SitesAgree(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	if _, err := rig.providerCredentials.Create(ctx, sqlcgen.ProviderCredentialScopeGlobal, nil, sqlcgen.ProviderCredentialProviderAnthropic, encryptForTest(t, rig, "deployment-anthropic-key")); err != nil {
		t.Fatalf("create global credential: %v", err)
	}

	linkedUser := func(access string) pgtype.UUID {
		user, _ := rig.createAuthenticatedUser(ctx, t)
		linkChatGPT(ctx, t, rig, user.ID, access)
		return user.ID
	}
	createSession := func(params sqlcgen.CreateSessionParams) sqlcgen.Session {
		params.Repos = []byte(providerCredsRepos)
		session, err := rig.sessions.Create(ctx, params)
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		return session
	}

	deploymentOnly := []string{"anthropic"}
	withLink := []string{"anthropic", "openai"}

	tests := []struct {
		name string
		// build returns the session and, when the delivery serves openai,
		// the access token it must be (the creator's own).
		build func() (sqlcgen.Session, string)
		want  []string
	}{
		{
			name: "review session",
			build: func() (sqlcgen.Session, string) {
				return createReviewSession(ctx, t, rig, linkedUser("review-requester-seat"), 300), ""
			},
			want: deploymentOnly,
		},
		{
			name: "web session its owner created",
			build: func() (sqlcgen.Session, string) {
				return createSession(sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: linkedUser("web-owner-seat")}), "web-owner-seat"
			},
			want: withLink,
		},
		{
			name: "multiplayer session resolves its creator's link, never a participant's",
			build: func() (sqlcgen.Session, string) {
				session := createSession(sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: linkedUser("multiplayer-creator-seat")})
				if _, err := rig.participants.Create(ctx, session.ID, linkedUser("multiplayer-participant-seat")); err != nil {
					t.Fatalf("add participant: %v", err)
				}
				return session, "multiplayer-creator-seat"
			},
			want: withLink,
		},
		{
			name: "automation session",
			build: func() (sqlcgen.Session, string) {
				return createSession(sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb}), ""
			},
			want: deploymentOnly,
		},
		{
			name: "child of a review session, even with a creator",
			build: func() (sqlcgen.Session, string) {
				parent := createReviewSession(ctx, t, rig, linkedUser("parent-requester-seat"), 301)
				return createSession(sqlcgen.CreateSessionParams{
					SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: linkedUser("child-creator-seat"),
					ParentSessionID: parent.ID, SpawnDepth: int32(1),
				}), ""
			},
			want: deploymentOnly,
		},
		{
			name: "github-origin session no pull request row names",
			build: func() (sqlcgen.Session, string) {
				return createSession(sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: linkedUser("unclaimed-github-seat")}), "unclaimed-github-seat"
			},
			want: withLink,
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session, wantAccess := tc.build()
			token := fmt.Sprintf("sandbox-bearer-token-%d", i)
			createSandboxWithToken(ctx, t, rig, session.ID, token)

			status, delivered := postProviderCredentials(t, rig, session.ID.String(), token, "1")
			if status != http.StatusOK {
				t.Fatalf("delivery status = %d, want %d", status, http.StatusOK)
			}
			var deliverySet []string
			for provider := range delivered.Credentials {
				deliverySet = append(deliverySet, provider)
			}
			sort.Strings(deliverySet)

			var counterSet []string
			for provider, ok := range sessionactor.CounterReviewCredentialedProviders(ctx, rig.prSessions, rig.providerCredentials, session) {
				if ok {
					counterSet = append(counterSet, provider)
				}
			}
			sort.Strings(counterSet)

			if !reflect.DeepEqual(deliverySet, counterSet) {
				t.Errorf("the two sites disagree: delivery serves %v, the counter-reviewer counts %v", deliverySet, counterSet)
			}
			if !reflect.DeepEqual(deliverySet, tc.want) {
				t.Errorf("delivery serves %v, want %v", deliverySet, tc.want)
			}
			if !reflect.DeepEqual(counterSet, tc.want) {
				t.Errorf("counter-reviewer counts %v, want %v", counterSet, tc.want)
			}
			if wantAccess != "" {
				entry := delivered.Credentials["openai"]
				if entry.Type != "oauth" || entry.Access == nil || *entry.Access != wantAccess {
					t.Errorf("Credentials[openai] = %+v, want the creator's own link %q", entry, wantAccess)
				}
			}
		})
	}
}
