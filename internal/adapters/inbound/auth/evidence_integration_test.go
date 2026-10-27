//go:build integration

// Integration tests for §13.2's email evidence through the real sign-in
// handlers: every GitHub or OIDC sign-in records the verified email its
// provider presents for the identity it authenticated, stamped now; a
// first sign-in merges only on recently confirmed evidence; and a first
// sign-in whose email is a member's primary email, with no recent
// confirmation behind it, is refused and audited -- never merged, never a
// second account.
package auth_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/internal/platform"
)

// agedEvidence is older than the shipped IdentityEmailEvidenceMaxAge, so a
// row stamped this long ago is no evidence.
var agedEvidence = platform.DefaultTimeouts().IdentityEmailEvidenceMaxAge + 24*time.Hour

// fakeGitHubUserID is newFakeGitHubAPI's default user, the external_id of
// every GitHub sign-in these tests make.
const fakeGitHubUserID = "555000111"

// stampIdentityAgo sets identityID's email_checked_at to age before the
// database clock.
func stampIdentityAgo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, identityID pgtype.UUID, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE identities SET email_checked_at = now() - make_interval(secs => $2) WHERE id = $1`, identityID, age.Seconds()); err != nil {
		t.Fatalf("stamp identity %v: %v", identityID, err)
	}
}

// identityStampedJustNow reports whether identityID's email_checked_at lies
// within the last minute of the database clock.
func identityStampedJustNow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, identityID pgtype.UUID) bool {
	t.Helper()
	var recent bool
	if err := pool.QueryRow(ctx, `SELECT coalesce(email_checked_at > now() - interval '1 minute', false) FROM identities WHERE id = $1`, identityID).Scan(&recent); err != nil {
		t.Fatalf("read identity %v stamp: %v", identityID, err)
	}
	return recent
}

// githubSignIn runs one GitHub sign-in against rig with a fresh browser.
func githubSignIn(t *testing.T, rig testRig, code string) *http.Response {
	t.Helper()
	client := newClient(t)
	resp := doCallback(t, client, rig.server.URL, doLogin(t, client, rig.server.URL), code)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// oidcSignIn runs one OIDC sign-in against rig, the ID token naming sub
// with email verified.
func oidcSignIn(t *testing.T, rig oidcTestRig, sub, email, code string) *http.Response {
	t.Helper()
	client := newClient(t)
	state, nonce := doOIDCLogin(t, client, rig.server.URL)
	claims := rig.provider.defaultClaims(sub)
	claims["email"] = email
	claims["email_verified"] = true
	claims["nonce"] = nonce
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))
	resp := doOIDCCallback(t, client, rig.server.URL, state, code)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// sessionUser is the user the sign-in response's session cookie belongs to.
func sessionUser(ctx context.Context, t *testing.T, sessions interface {
	GetByHash(context.Context, string) (sqlcgen.UserSession, error)
}, resp *http.Response) pgtype.UUID {
	t.Helper()
	row, err := sessions.GetByHash(ctx, platform.HashToken(authCookieFrom(t, resp)))
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	return row.UserID
}

// auditActions counts the audit_log rows of resourceType/resourceID by
// action.
func auditActions(ctx context.Context, t *testing.T, pool *pgxpool.Pool, resourceType, resourceID string) map[string][]sqlcgen.AuditLog {
	t.Helper()
	out := map[string][]sqlcgen.AuditLog{}
	for _, row := range getAuditLogRowsForResource(ctx, t, pool, resourceType, resourceID) {
		out[row.Action] = append(out[row.Action], row)
	}
	return out
}

// usersWithEmail counts users whose primary email is email, ignoring case.
func usersWithEmail(ctx context.Context, t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(primary_email) = lower($1)`, email).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n
}

// TestCallback_ReturningSignInRefreshesEvidence proves a returning GitHub
// sign-in records the verified primary email GitHub presents now, stamped
// now, with the fresh token, and signs in the identity's user: the same
// address is confirmed silently, a new one is audited and moves the
// primary email it came from.
func TestCallback_ReturningSignInRefreshesEvidence(t *testing.T) {
	for _, tt := range []struct {
		name      string
		presented string
		wantMoved bool
	}{
		{"the same email", "octocat@example.com", false},
		{"a new email", "octocat-moved@example.com", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newTestRig(t, defaultRiggedOptions())
			if resp := githubSignIn(t, rig, "first-code"); resp.StatusCode != http.StatusFound {
				t.Fatalf("first sign-in status = %d, want %d", resp.StatusCode, http.StatusFound)
			}
			before, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, fakeGitHubUserID)
			if err != nil {
				t.Fatalf("GetByProviderAndExternalID: %v", err)
			}
			stampIdentityAgo(ctx, t, rig.pool, before.ID, agedEvidence)

			rig.github.setEmails([]map[string]any{{"email": tt.presented, "primary": true, "verified": true}})
			resp := githubSignIn(t, rig, "second-code")
			if resp.StatusCode != http.StatusFound {
				t.Fatalf("returning sign-in status = %d, want %d", resp.StatusCode, http.StatusFound)
			}
			if got := sessionUser(ctx, t, rig.userSessions, resp); got != before.UserID {
				t.Errorf("session user = %v, want %v", got, before.UserID)
			}

			after, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, fakeGitHubUserID)
			if err != nil {
				t.Fatalf("GetByProviderAndExternalID: %v", err)
			}
			if after.ID != before.ID || after.UserID != before.UserID || after.LinkedVia != before.LinkedVia {
				t.Errorf("identity = %v of %v via %s, want the same link as before", after.ID, after.UserID, after.LinkedVia)
			}
			if after.Email == nil || *after.Email != tt.presented || !after.EmailVerified || !identityStampedJustNow(ctx, t, rig.pool, after.ID) {
				t.Errorf("identity email = %v (verified %v), want %q verified and stamped just now", after.Email, after.EmailVerified, tt.presented)
			}
			token, err := platform.DecryptToken(githubTestTokenKey, after.AccessTokenEncrypted)
			if err != nil || string(token) != "gho_fake_access_token_second-code" {
				t.Errorf("stored token = %q (%v), want the second sign-in's", token, err)
			}

			user, err := rig.users.GetByID(ctx, before.UserID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if user.PrimaryEmail != tt.presented {
				t.Errorf("primary email = %q, want %q", user.PrimaryEmail, tt.presented)
			}
			updates := auditActions(ctx, t, rig.pool, "identity", before.ID.String())["identity.email_updated"]
			moves := auditActions(ctx, t, rig.pool, "user", before.UserID.String())["user.primary_email_changed"]
			if want := map[bool]int{true: 1, false: 0}[tt.wantMoved]; len(updates) != want || len(moves) != want {
				t.Errorf("identity.email_updated = %d, user.primary_email_changed = %d, want %d each", len(updates), len(moves), want)
			}
		})
	}
}

// TestOIDCCallback_ReturningSignInRefreshesEvidence is the OIDC twin: a
// returning sign-in records the ID token's verified email on the identity,
// stamped now, and the primary email follows a new one.
func TestOIDCCallback_ReturningSignInRefreshesEvidence(t *testing.T) {
	for _, tt := range []struct {
		name      string
		presented string
		wantMoved bool
	}{
		{"the same email", "oidc-returning@example.com", false},
		{"a new email", "oidc-returning-moved@example.com", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
			if resp := oidcSignIn(t, rig, "oidc-returning", "oidc-returning@example.com", "first-code"); resp.StatusCode != http.StatusFound {
				t.Fatalf("first sign-in status = %d, want %d", resp.StatusCode, http.StatusFound)
			}
			externalID := rig.provider.issuer() + "|oidc-returning"
			before, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, externalID)
			if err != nil {
				t.Fatalf("GetByProviderAndExternalID: %v", err)
			}
			stampIdentityAgo(ctx, t, rig.pool, before.ID, agedEvidence)

			resp := oidcSignIn(t, rig, "oidc-returning", tt.presented, "second-code")
			if resp.StatusCode != http.StatusFound {
				t.Fatalf("returning sign-in status = %d, want %d", resp.StatusCode, http.StatusFound)
			}
			if got := sessionUser(ctx, t, rig.userSessions, resp); got != before.UserID {
				t.Errorf("session user = %v, want %v", got, before.UserID)
			}

			after, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, externalID)
			if err != nil {
				t.Fatalf("GetByProviderAndExternalID: %v", err)
			}
			if after.ID != before.ID || after.UserID != before.UserID || after.LinkedVia != before.LinkedVia {
				t.Errorf("identity = %v of %v via %s, want the same link as before", after.ID, after.UserID, after.LinkedVia)
			}
			if after.Email == nil || *after.Email != tt.presented || !after.EmailVerified || !identityStampedJustNow(ctx, t, rig.pool, after.ID) {
				t.Errorf("identity email = %v (verified %v), want %q verified and stamped just now", after.Email, after.EmailVerified, tt.presented)
			}
			user, err := rig.users.GetByID(ctx, before.UserID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if user.PrimaryEmail != tt.presented {
				t.Errorf("primary email = %q, want %q", user.PrimaryEmail, tt.presented)
			}
			updates := auditActions(ctx, t, rig.pool, "identity", before.ID.String())["identity.email_updated"]
			if want := map[bool]int{true: 1, false: 0}[tt.wantMoved]; len(updates) != want {
				t.Errorf("identity.email_updated = %d, want %d", len(updates), want)
			}
		})
	}
}

// TestFirstSignIn_StampsEvidence proves a sign-in that creates its user
// stamps the identity it creates: the email its provider just presented is
// evidence from that moment.
func TestFirstSignIn_StampsEvidence(t *testing.T) {
	for _, tt := range []struct {
		name   string
		signIn func(t *testing.T) (*pgxpool.Pool, sqlcgen.Identity)
	}{
		{"github", func(t *testing.T) (*pgxpool.Pool, sqlcgen.Identity) {
			rig := newTestRig(t, defaultRiggedOptions())
			if resp := githubSignIn(t, rig, "stamp-code"); resp.StatusCode != http.StatusFound {
				t.Fatalf("sign-in status = %d, want %d", resp.StatusCode, http.StatusFound)
			}
			row, err := rig.identities.GetByProviderAndExternalID(context.Background(), sqlcgen.IdentityProviderGithub, fakeGitHubUserID)
			if err != nil {
				t.Fatalf("GetByProviderAndExternalID: %v", err)
			}
			return rig.pool, row
		}},
		{"oidc", func(t *testing.T) (*pgxpool.Pool, sqlcgen.Identity) {
			rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
			if resp := oidcSignIn(t, rig, "oidc-stamp", "oidc-stamp@example.com", "stamp-code"); resp.StatusCode != http.StatusFound {
				t.Fatalf("sign-in status = %d, want %d", resp.StatusCode, http.StatusFound)
			}
			row, err := rig.identities.GetByProviderAndExternalID(context.Background(), sqlcgen.IdentityProviderOidc, rig.provider.issuer()+"|oidc-stamp")
			if err != nil {
				t.Fatalf("GetByProviderAndExternalID: %v", err)
			}
			return rig.pool, row
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool, row := tt.signIn(t)
			if !row.EmailVerified || !row.EmailCheckedAt.Valid || !identityStampedJustNow(context.Background(), t, pool, row.ID) {
				t.Errorf("created identity verified %v, stamped %+v, want verified and stamped just now", row.EmailVerified, row.EmailCheckedAt)
			}
		})
	}
}

// heldIdentity is an identity of the holder carrying the presented email:
// verified or not, stamped agedEvidence ago when stamped.
type heldIdentity struct {
	provider sqlcgen.IdentityProvider
	verified bool
	stamped  bool
}

// signInTarget is one provider's first sign-in against a fresh rig, with
// what a test reads back about it.
type signInTarget struct {
	pool       *pgxpool.Pool
	externalID string
	signIn     func() *http.Response
	identity   func() (sqlcgen.Identity, error)
}

// newSignInTarget builds provider's rig with allowlist, its sign-in
// presenting email.
func newSignInTarget(t *testing.T, provider string, allowlist auth.AllowlistConfig, email, code string) signInTarget {
	t.Helper()
	ctx := context.Background()
	if provider == "github" {
		rig := newTestRig(t, riggedOptions{allowlist: allowlist})
		rig.github.setEmails([]map[string]any{{"email": email, "primary": true, "verified": true}})
		return signInTarget{
			pool:       rig.pool,
			externalID: fakeGitHubUserID,
			signIn:     func() *http.Response { return githubSignIn(t, rig, code) },
			identity: func() (sqlcgen.Identity, error) {
				return rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, fakeGitHubUserID)
			},
		}
	}
	rig := newOIDCTestRig(t, oidcRiggedOptions{allowlist: allowlist})
	externalID := rig.provider.issuer() + "|oidc-first"
	return signInTarget{
		pool:       rig.pool,
		externalID: externalID,
		signIn:     func() *http.Response { return oidcSignIn(t, rig, "oidc-first", email, code) },
		identity: func() (sqlcgen.Identity, error) {
			return rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, externalID)
		},
	}
}

// TestFirstSignIn_EmailHeldWithoutCurrentEvidenceIsRefused proves a first
// sign-in whose verified email no recent evidence names, but some member
// holds as their primary email -- one seeded from a manifest, one whose
// sign-in evidence has aged out, one held in another case -- is refused
// with the generic 403 and audited with the holder, through either
// provider: never merged onto the holder, never a second user. The
// allowlist allows the address, so the refusal is the held check's own.
func TestFirstSignIn_EmailHeldWithoutCurrentEvidenceIsRefused(t *testing.T) {
	const email = "held@example.com"
	holders := []struct {
		name     string
		primary  string
		identity *heldIdentity
	}{
		{name: "seeded", primary: email, identity: &heldIdentity{provider: sqlcgen.IdentityProviderGithub}},
		{name: "aged-out sign-in", primary: email, identity: &heldIdentity{provider: sqlcgen.IdentityProviderGithub, verified: true, stamped: true}},
		{name: "another case", primary: strings.ToUpper(email)},
	}
	for _, provider := range []string{"github", "oidc"} {
		for _, holder := range holders {
			t.Run(provider+", "+holder.name, func(t *testing.T) {
				ctx := context.Background()
				target := newSignInTarget(t, provider, auth.AllowlistConfig{EmailDomains: []string{"example.com"}}, email, "held-code")

				holderUser, err := narvipg.NewUserStore(target.pool).Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: holder.primary, DisplayName: "Holder", Role: sqlcgen.UserRoleMember})
				if err != nil {
					t.Fatalf("create holder: %v", err)
				}
				if spec := holder.identity; spec != nil {
					e := email
					row, err := narvipg.NewIdentityStore(target.pool).Create(ctx, sqlcgen.CreateIdentityParams{
						UserID: holderUser.ID, Provider: spec.provider, ExternalID: "holder-identity",
						Email: &e, EmailVerified: spec.verified, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
					})
					if err != nil {
						t.Fatalf("create holder identity: %v", err)
					}
					if spec.stamped {
						stampIdentityAgo(ctx, t, target.pool, row.ID, agedEvidence)
					}
				}

				resp := target.signIn()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "not authorized to sign up") {
					t.Fatalf("sign-in = %d %q, want 403 with the generic refusal", resp.StatusCode, body)
				}
				if got := usersWithEmail(ctx, t, target.pool, email); got != 1 {
					t.Errorf("users holding %s = %d, want the holder alone", email, got)
				}
				if row, err := target.identity(); !errorsIsNoRows(err) {
					t.Errorf("identity for the refused sign-in = %+v (%v), want none", row, err)
				}

				action := "identity." + provider + "_email_held"
				rows := getAuditLogRowsForResource(ctx, t, target.pool, "identity", target.externalID)
				if len(rows) != 1 || rows[0].Action != action {
					t.Fatalf("audit rows for the refusal = %+v, want exactly one %s", rows, action)
				}
				if rows[0].ActorUserID.Valid {
					t.Errorf("actor = %v, want none", rows[0].ActorUserID)
				}
				var detail map[string]any
				if err := json.Unmarshal(rows[0].DetailJson, &detail); err != nil {
					t.Fatalf("decode detail: %v", err)
				}
				if detail["held_by_user_id"] != holderUser.ID.String() || detail["provider"] != provider || detail["external_id"] != target.externalID {
					t.Errorf("detail = %v, want held_by_user_id %v, provider %s, external_id %s", detail, holderUser.ID, provider, target.externalID)
				}
			})
		}
	}
}

// TestFirstSignIn_AgedEvidenceDoesNotMerge proves a first sign-in whose
// email only aged-out evidence names -- and no member holds as a primary
// email -- is a new person: a new user behind the allowlist, or the
// allowlist's refusal, through either provider. Never the old owner.
func TestFirstSignIn_AgedEvidenceDoesNotMerge(t *testing.T) {
	const email = "aged-shared@example.com"
	for _, provider := range []string{"github", "oidc"} {
		for _, allowed := range []bool{true, false} {
			name := provider + ", the allowlist denies"
			allowlist := auth.AllowlistConfig{Emails: []string{"nobody@nowhere.invalid"}}
			if allowed {
				name = provider + ", the allowlist allows"
				allowlist = auth.AllowlistConfig{EmailDomains: []string{"example.com"}}
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				target := newSignInTarget(t, provider, allowlist, email, "aged-code")

				// The old owner: another primary email, and a Slack link on
				// the shared address confirmed longer ago than the limit.
				owner, err := narvipg.NewUserStore(target.pool).Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "aged-owner@example.com", DisplayName: "Old Owner", Role: sqlcgen.UserRoleMember})
				if err != nil {
					t.Fatalf("create owner: %v", err)
				}
				shared := email
				aged, err := narvipg.NewIdentityStore(target.pool).Create(ctx, sqlcgen.CreateIdentityParams{
					UserID: owner.ID, Provider: sqlcgen.IdentityProviderSlack, ExternalID: "U-AGED-OWNER",
					Email: &shared, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail,
				})
				if err != nil {
					t.Fatalf("create owner identity: %v", err)
				}
				stampIdentityAgo(ctx, t, target.pool, aged.ID, agedEvidence)

				resp := target.signIn()
				row, rowErr := target.identity()
				actions := auditActions(ctx, t, target.pool, "identity", target.externalID)
				if len(actions["identity."+provider+"_email_held"]) != 0 {
					t.Errorf("audit = %v, want no held refusal: no member holds %s", actions, email)
				}
				if !allowed {
					if resp.StatusCode != http.StatusForbidden || !errorsIsNoRows(rowErr) {
						t.Errorf("sign-in = %d (identity %+v, %v), want 403 and no identity", resp.StatusCode, row, rowErr)
					}
					if provider == "oidc" && len(actions["identity.oidc_first_time_denied"]) != 1 {
						t.Errorf("audit = %v, want the allowlist's identity.oidc_first_time_denied", actions)
					}
					return
				}
				if resp.StatusCode != http.StatusFound || rowErr != nil {
					t.Fatalf("sign-in = %d (identity %v), want 302 and a new identity", resp.StatusCode, rowErr)
				}
				if row.UserID == owner.ID || row.LinkedVia != sqlcgen.IdentityLinkedViaAdmin {
					t.Errorf("identity = %v via %s, want a new user's own sign-in identity, never the old owner %v", row.UserID, row.LinkedVia, owner.ID)
				}
				if got := sessionUser(ctx, t, narvipg.NewUserSessionStore(target.pool), resp); got != row.UserID {
					t.Errorf("session user = %v, want the new user %v", got, row.UserID)
				}
				newUser, err := narvipg.NewUserStore(target.pool).GetByID(ctx, row.UserID)
				if err != nil || newUser.PrimaryEmail != email {
					t.Errorf("new user = %+v (%v), want primary email %s", newUser, err, email)
				}
			})
		}
	}
}

// TestCrossProvider_AddressMovedAtProvider follows one address from one
// person to another, end to end. Person A signs in with GitHub on e1, then
// GitHub moves A's primary address to e2 and A signs in again: A's
// identity and primary email move to e2. From then on e1 names no one: a
// Slack member presenting it gets the link prompt, and person B signing in
// through OIDC with it becomes a new user, not A. Once B has, e1 is B's
// evidence, and the Slack member links to B.
func TestCrossProvider_AddressMovedAtProvider(t *testing.T) {
	ctx := context.Background()
	const (
		e1 = "moved-first@example.com"
		e2 = "moved-second@example.com"
	)

	githubRig := newTestRig(t, defaultRiggedOptions())
	githubRig.github.setEmails([]map[string]any{{"email": e1, "primary": true, "verified": true}})
	if resp := githubSignIn(t, githubRig, "moved-first-code"); resp.StatusCode != http.StatusFound {
		t.Fatalf("A's first sign-in status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	identityA, err := githubRig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, fakeGitHubUserID)
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	userA := identityA.UserID

	githubRig.github.setEmails([]map[string]any{{"email": e2, "primary": true, "verified": true}})
	if resp := githubSignIn(t, githubRig, "moved-second-code"); resp.StatusCode != http.StatusFound {
		t.Fatalf("A's second sign-in status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	identityA, err = githubRig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderGithub, fakeGitHubUserID)
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	a, err := githubRig.users.GetByID(ctx, userA)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if identityA.UserID != userA || identityA.Email == nil || *identityA.Email != e2 || a.PrimaryEmail != e2 {
		t.Fatalf("after A's second sign-in: identity %v carries %v, primary %q, want A's identity and primary on %s", identityA.UserID, identityA.Email, a.PrimaryEmail, e2)
	}

	slackDeps := identitylink.Deps{
		Pool:           githubRig.pool,
		Users:          githubRig.users,
		Identities:     githubRig.identities,
		LinkPrompts:    githubRig.linkPrompts,
		AuditLog:       githubRig.auditLog,
		PublicBaseURL:  "https://narvi.example.com",
		PromptTTL:      time.Hour,
		EvidenceMaxAge: platform.DefaultTimeouts().IdentityEmailEvidenceMaxAge,
	}
	res, err := identitylink.Resolve(ctx, slackDeps, sqlcgen.IdentityProviderSlack, "U-MOVED-FIRST", identitylink.ProfileEmail{Address: e1, Verified: true}, true)
	if err != nil {
		t.Fatalf("Resolve (before B): %v", err)
	}
	if res.UserID.Valid || res.MagicLinkURL == "" {
		t.Errorf("Slack member on %s before B = %+v, want the link prompt and no link", e1, res)
	}

	oidcRig := oidcRigOnPool(t, githubRig.pool, defaultOIDCRiggedOptions())
	resp := oidcSignIn(t, oidcRig, "person-b", e1, "person-b-code")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("B's sign-in status = %d, want %d: no member holds %s any more", resp.StatusCode, http.StatusFound, e1)
	}
	identityB, err := oidcRig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, oidcRig.provider.issuer()+"|person-b")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID (B): %v", err)
	}
	if identityB.UserID == userA {
		t.Fatalf("B's identity joined A's account %v", userA)
	}
	if got := sessionUser(ctx, t, oidcRig.userSessions, resp); got != identityB.UserID {
		t.Errorf("B's session user = %v, want %v", got, identityB.UserID)
	}

	res, err = identitylink.Resolve(ctx, slackDeps, sqlcgen.IdentityProviderSlack, "U-MOVED-SECOND", identitylink.ProfileEmail{Address: e1, Verified: true}, true)
	if err != nil {
		t.Fatalf("Resolve (after B): %v", err)
	}
	if !res.AutoLinked || res.UserID != identityB.UserID {
		t.Errorf("Slack member on %s after B = %+v, want an automatic link to B %v", e1, res, identityB.UserID)
	}
}
