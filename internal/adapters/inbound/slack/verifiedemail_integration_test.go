//go:build integration

// Integration tests for §13.2's definition of a verified Slack email, end
// to end through the function both Slack ingress routes resolve an actor
// with (resolveSlackActor, via export_test.go's ResolveSlackActorForTest):
// a real slackapi client reads a fake Slack Web API's users.info and
// auth.test, and a real identitylink.Resolve writes to a real database. A
// Slack identity links automatically only on an email Slack reports as
// confirmed, for a full member of this workspace, and the identity it
// stores says so; an automatic link made before that rule is re-checked at
// the member's next event.
package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/inbound/slack"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/internal/platform"
)

// newFakeSlackMember is a fake Slack Web API answering users.info, for any
// user, with member -- or, when member is nil, with a retryable error --
// and auth.test with testSlackTeam. It counts users.info calls.
func newFakeSlackMember(t *testing.T, member map[string]any) (*slackapi.Client, *int32) {
	t.Helper()
	client, usersInfoCalls, _ := newSlowFakeSlackMember(t, member, 0)
	return client, usersInfoCalls
}

// newSlowFakeSlackMember is newFakeSlackMember answering every call after
// delay, and counting auth.test calls too.
func newSlowFakeSlackMember(t *testing.T, member map[string]any, delay time.Duration) (*slackapi.Client, *int32, *int32) {
	t.Helper()
	var usersInfoCalls, authTestCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		if r.URL.Path == "/auth.test" {
			atomic.AddInt32(&authTestCalls, 1)
		}
		if answerSlackAuthTest(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/users.info" {
			atomic.AddInt32(&usersInfoCalls, 1)
			if member == nil {
				_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": member})
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	return slackapi.New(server.Client(), server.URL, "test-bot-token"), &usersInfoCalls, &authTestCalls
}

// fastIdentityFetchTimeouts is DefaultTimeouts with a short profile-email
// retry, so a fetch that keeps failing gives up in milliseconds.
func fastIdentityFetchTimeouts() platform.Timeouts {
	to := platform.DefaultTimeouts()
	to.IdentityEmailFetchTimeout = 2 * time.Second
	to.IdentityEmailFetchMaxAttempts = 2
	to.IdentityEmailFetchRetryBaseDelay = time.Millisecond
	to.IdentityEmailFetchRetryMaxDelay = 2 * time.Millisecond
	return to
}

// memberChange edits a confirmedSlackMember payload.
type memberChange func(map[string]any)

func setField(key string, value any) memberChange {
	return func(m map[string]any) { m[key] = value }
}

func dropField(key string) memberChange {
	return func(m map[string]any) { delete(m, key) }
}

const autoLinkedNotice = "I've connected this to your Narvi account."

// TestResolveSlackActor_LinksOnlyOnAVerifiedEmail proves a Slack member
// whose profile email matches exactly one Narvi user is linked
// automatically only when Slack reports the email as confirmed and the
// member is a full member of this workspace -- and that the stored
// identity's email_verified says so. Every other member gets the link
// prompt, and no identity.
func TestResolveSlackActor_LinksOnlyOnAVerifiedEmail(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	deps := newIdentityLinkDepsForTest(pool, narvipg.NewAuditLogStore(pool))

	tests := []struct {
		name       string
		change     memberChange
		wantLinked bool
	}{
		{"full member, confirmed email", nil, true},
		{"email not confirmed", setField("is_email_confirmed", false), false},
		{"is_email_confirmed absent", dropField("is_email_confirmed"), false},
		{"deleted", setField("deleted", true), false},
		{"bot", setField("is_bot", true), false},
		{"guest", setField("is_restricted", true), false},
		{"single-channel guest", setField("is_ultra_restricted", true), false},
		{"stranger", setField("is_stranger", true), false},
		{"another team's member", setField("team_id", "T0OTHER"), false},
		{"team_id absent", dropField("team_id"), false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slackUserID := fmt.Sprintf("U-VERIFIED-%02d", i)
			email := fmt.Sprintf("verified-member-%02d@example.com", i)
			user, err := deps.Users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: email, DisplayName: email, Role: sqlcgen.UserRoleMember})
			if err != nil {
				t.Fatalf("create fixture user: %v", err)
			}
			member := confirmedSlackMember(email)
			if tt.change != nil {
				tt.change(member)
			}
			client, _ := newFakeSlackMember(t, member)

			gotUser, notice := slack.ResolveSlackActorForTest(ctx, client, deps, fastIdentityFetchTimeouts(), slackUserID)

			row, rowErr := deps.Identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, slackUserID)
			if tt.wantLinked {
				if gotUser != user.ID || notice != autoLinkedNotice {
					t.Fatalf("resolveSlackActor = (%v, %q), want (%v, %q)", gotUser, notice, user.ID, autoLinkedNotice)
				}
				if rowErr != nil {
					t.Fatalf("GetByProviderAndExternalID: %v", rowErr)
				}
				if row.LinkedVia != sqlcgen.IdentityLinkedViaAutoEmail || !row.EmailVerified || !row.EmailCheckedAt.Valid {
					t.Errorf("stored identity = %+v, want auto_email, email_verified, email_checked_at set", row)
				}
				return
			}

			if gotUser.Valid {
				t.Errorf("resolveSlackActor linked %v, want no one", gotUser)
			}
			if !strings.Contains(notice, deps.PublicBaseURL+identitylink.MagicLinkPath) {
				t.Errorf("notice = %q, want the link prompt", notice)
			}
			if !errors.Is(rowErr, pgx.ErrNoRows) {
				t.Errorf("stored identity = %+v (err %v), want none", row, rowErr)
			}
		})
	}
}

// TestResolveSlackActor_RechecksAnAutoLinkMadeBeforeTheConfirmedEmailRule
// proves an automatic Slack link the previous binary made (email_verified
// true, no email_checked_at) is re-checked against Slack at the member's
// next event: kept when Slack confirms the email for a full member of this
// workspace, removed and the link prompt sent when not, left as it is when
// users.info cannot be read -- and that a link the person confirmed
// through the link prompt is never re-checked at all.
func TestResolveSlackActor_RechecksAnAutoLinkMadeBeforeTheConfirmedEmailRule(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	auditLog := narvipg.NewAuditLogStore(pool)
	deps := newIdentityLinkDepsForTest(pool, auditLog)

	tests := []struct {
		name      string
		linkedVia sqlcgen.IdentityLinkedVia
		// change edits the confirmed member Slack answers with; failFetch
		// makes users.info fail instead.
		change    memberChange
		failFetch bool

		wantUser      bool
		wantRow       bool
		wantStamped   bool
		wantPrompt    bool
		wantRevoke    bool
		wantUsersInfo int32
	}{
		{name: "confirmed full member: kept", linkedVia: sqlcgen.IdentityLinkedViaAutoEmail,
			wantUser: true, wantRow: true, wantStamped: true, wantUsersInfo: 1},
		{name: "email not confirmed: removed", linkedVia: sqlcgen.IdentityLinkedViaAutoEmail, change: setField("is_email_confirmed", false),
			wantPrompt: true, wantRevoke: true, wantUsersInfo: 1},
		{name: "another team's member: removed", linkedVia: sqlcgen.IdentityLinkedViaAutoEmail, change: setField("team_id", "T0OTHER"),
			wantPrompt: true, wantRevoke: true, wantUsersInfo: 1},
		{name: "users.info fails: left as it is, unused", linkedVia: sqlcgen.IdentityLinkedViaAutoEmail, failFetch: true,
			wantRow: true, wantUsersInfo: 2},
		{name: "linked through the link prompt: never re-checked", linkedVia: sqlcgen.IdentityLinkedViaPrompt, change: setField("is_email_confirmed", false),
			wantUser: true, wantRow: true, wantUsersInfo: 0},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slackUserID := fmt.Sprintf("U-RECHECK-%02d", i)
			email := fmt.Sprintf("recheck-member-%02d@example.com", i)
			user, err := deps.Users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: email, DisplayName: email, Role: sqlcgen.UserRoleMember})
			if err != nil {
				t.Fatalf("create fixture user: %v", err)
			}
			// Exactly what the previous binary wrote for an automatic link
			// (or what the link prompt writes, for the prompt case).
			seed := sqlcgen.CreateIdentityParams{UserID: user.ID, Provider: sqlcgen.IdentityProviderSlack, ExternalID: slackUserID, LinkedVia: tt.linkedVia}
			if tt.linkedVia == sqlcgen.IdentityLinkedViaAutoEmail {
				seed.Email, seed.EmailVerified = &email, true
			}
			seeded, err := deps.Identities.Create(ctx, seed)
			if err != nil {
				t.Fatalf("seed identity: %v", err)
			}

			var member map[string]any
			if !tt.failFetch {
				member = confirmedSlackMember(email)
				if tt.change != nil {
					tt.change(member)
				}
			}
			client, usersInfoCalls := newFakeSlackMember(t, member)

			gotUser, notice := slack.ResolveSlackActorForTest(ctx, client, deps, fastIdentityFetchTimeouts(), slackUserID)

			if got := atomic.LoadInt32(usersInfoCalls); got != tt.wantUsersInfo {
				t.Errorf("users.info calls = %d, want %d", got, tt.wantUsersInfo)
			}
			if (gotUser == user.ID) != tt.wantUser || (!tt.wantUser && gotUser.Valid) {
				t.Errorf("resolveSlackActor = %v, want the link to %v: %v", gotUser, user.ID, tt.wantUser)
			}
			if hasPrompt := strings.Contains(notice, deps.PublicBaseURL+identitylink.MagicLinkPath); hasPrompt != tt.wantPrompt {
				t.Errorf("notice = %q, want the link prompt: %v", notice, tt.wantPrompt)
			}

			row, rowErr := deps.Identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, slackUserID)
			switch {
			case tt.wantRow:
				if rowErr != nil || row.ID != seeded.ID || row.LinkedVia != tt.linkedVia {
					t.Errorf("identity = %+v (err %v), want the seeded row", row, rowErr)
				}
				if row.EmailCheckedAt.Valid != tt.wantStamped {
					t.Errorf("email_checked_at set = %v, want %v", row.EmailCheckedAt.Valid, tt.wantStamped)
				}
			case !errors.Is(rowErr, pgx.ErrNoRows):
				t.Errorf("identity = %+v (err %v), want it removed", row, rowErr)
			}

			entries, err := auditLog.List(ctx, 500, 0)
			if err != nil {
				t.Fatalf("AuditLog.List: %v", err)
			}
			revokes := 0
			for _, e := range entries {
				if e.Action == "identity.auto_link_revoked" && e.ResourceID == seeded.ID.String() {
					revokes++
				}
			}
			if want := map[bool]int{true: 1, false: 0}[tt.wantRevoke]; revokes != want {
				t.Errorf("identity.auto_link_revoked entries = %d, want %d", revokes, want)
			}
		})
	}
}

// TestResolveSlackActor_SlowSlackLinksOnTheFirstEvent proves a cold client
// links a confirmed full member on the member's first event when Slack
// answers every call in 450ms -- the latency the shipped per-attempt
// budgets call healthy -- under platform.DefaultTimeouts(), through both
// resolution paths: the Events API's (identitylink.FetchEmailWithRetry,
// IdentityEmailFetchTimeout) and a click's single attempt
// (SlackInteractivityIdentityFetchTimeout). auth.test runs alongside
// users.info on its own budget, so the first attempt is enough.
func TestResolveSlackActor_SlowSlackLinksOnTheFirstEvent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	deps := newIdentityLinkDepsForTest(pool, narvipg.NewAuditLogStore(pool))
	timeouts := platform.DefaultTimeouts()
	const latency = 450 * time.Millisecond

	tests := []struct {
		name    string
		resolve func(client *slackapi.Client, slackUserID string) (pgtype.UUID, string, error)
	}{
		{"Events API, retried fetch", func(client *slackapi.Client, slackUserID string) (pgtype.UUID, string, error) {
			userID, notice := slack.ResolveSlackActorForTest(ctx, client, deps, timeouts, slackUserID)
			return userID, notice, nil
		}},
		{"click, single attempt", func(client *slackapi.Client, slackUserID string) (pgtype.UUID, string, error) {
			return slack.ResolveSlackActorSingleAttemptForTest(ctx, client, deps, timeouts.SlackInteractivityIdentityFetchTimeout, slackUserID)
		}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slackUserID := fmt.Sprintf("U-SLOW-%02d", i)
			email := fmt.Sprintf("slow-member-%02d@example.com", i)
			user, err := deps.Users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: email, DisplayName: email, Role: sqlcgen.UserRoleMember})
			if err != nil {
				t.Fatalf("create fixture user: %v", err)
			}
			client, usersInfoCalls, authTestCalls := newSlowFakeSlackMember(t, confirmedSlackMember(email), latency)

			gotUser, notice, err := tt.resolve(client, slackUserID)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if gotUser != user.ID || notice != autoLinkedNotice {
				t.Fatalf("first event resolved to (%v, %q), want (%v, %q)", gotUser, notice, user.ID, autoLinkedNotice)
			}
			if got := atomic.LoadInt32(usersInfoCalls); got != 1 {
				t.Errorf("users.info calls = %d, want 1: the first attempt must succeed", got)
			}
			if got := atomic.LoadInt32(authTestCalls); got != 1 {
				t.Errorf("auth.test calls = %d, want 1", got)
			}
		})
	}
}
