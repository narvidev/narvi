//go:build integration

// Same-provider identity-conflict integration tests for review round 2,
// findings P2/P3: resolveFirstTimeIdentity (firsttimeidentity.go) must
// refuse -- never merge -- a first-time sign-in whose verified email
// matches exactly one existing user who ALREADY has an identity for the
// SAME provider (a different GitHub external_id, or an OIDC identity
// under the SAME issuer). Before this fix, that sign-in silently attached
// a second identity row, and every consumer of identities.
// GetByUserAndProvider (a ":one" query, no ORDER BY) then read whichever
// row Postgres happened to scan first -- often the OLDER one, never the
// account that just signed in.
package auth_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestCallback_SameProviderConflict_Refused proves the GitHub-side half:
// a user's verified email moves to a NEW GitHub account (the old
// account's own identity row is untouched, exactly like a real account
// switch) -- the second sign-in must be refused, audited, and must never
// attach a second github identity row onto the first account.
func TestCallback_SameProviderConflict_Refused(t *testing.T) {
	rig := newTestRig(t, defaultRiggedOptions())
	ctx := context.Background()
	client := newClient(t)

	// First sign-in: the fake's own default account (userID 555000111,
	// login "octocat", email octocat@example.com) creates a brand-new
	// user with a github identity.
	state := doLogin(t, client, rig.server.URL)
	resp := doCallback(t, client, rig.server.URL, state, "first-account-code")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("first sign-in status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	_ = resp.Body.Close()

	userA, err := rig.users.GetByPrimaryEmail(ctx, "octocat@example.com")
	if err != nil {
		t.Fatalf("get userA by primary email: %v", err)
	}

	// The SAME verified email now belongs to a DIFFERENT GitHub account
	// (a real-world account switch: a personal account replaced by a work
	// one, both once carrying this verified email) -- a fresh client/
	// cookie jar, so this is an entirely unrelated sign-in attempt, not a
	// returning-user flow for userA's own session.
	rig.github.mu.Lock()
	rig.github.userID = 999000111
	rig.github.login = "octocat-work"
	rig.github.mu.Unlock()

	client2 := newClient(t)
	state2 := doLogin(t, client2, rig.server.URL)
	resp2 := doCallback(t, client2, rig.server.URL, state2, "second-account-code")
	defer func() { _ = resp2.Body.Close() }()

	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("second sign-in status = %d, want %d (never merge a second github identity onto an existing github-linked user)", resp2.StatusCode, http.StatusForbidden)
	}

	identities, err := rig.identities.ListForUser(ctx, userA.ID)
	if err != nil {
		t.Fatalf("list identities for userA: %v", err)
	}
	if len(identities) != 1 {
		t.Fatalf("identities for userA = %d, want 1 -- the second github account must never have been attached", len(identities))
	}
	if identities[0].ExternalID != "555000111" {
		t.Errorf("userA's remaining identity external_id = %q, want %q (the FIRST account's, untouched)", identities[0].ExternalID, "555000111")
	}

	rows := getAuditLogRowsForResource(ctx, t, rig.pool, "identity", "999000111")
	if len(rows) != 1 {
		t.Fatalf("audit_log rows for the same-provider-conflict refusal = %d, want 1", len(rows))
	}
	if rows[0].Action != "identity.github_already_linked" {
		t.Errorf("audit_log action = %q, want %q", rows[0].Action, "identity.github_already_linked")
	}
}

// TestOIDCCallback_SameProviderConflict_Refused proves the OIDC-side half:
// a user's verified email matches exactly one existing user who already
// has an OIDC identity under the SAME issuer (a different `sub`) --
// refused, never merged, exactly like the GitHub-side test above. A
// DIFFERENT issuer is deliberately NOT this test's concern -- see
// identityConflictsWithExistingProvider's own doc comment
// (firsttimeidentity.go) for why that case is allowed to coexist.
func TestOIDCCallback_SameProviderConflict_Refused(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	ctx := context.Background()

	const sharedEmail = "oidc-conflict@example.com"

	client := newClient(t)
	state, nonce := doOIDCLogin(t, client, rig.server.URL)
	claims := rig.provider.defaultClaims("oidc-subject-conflict-a")
	claims["email"] = sharedEmail
	claims["email_verified"] = true
	claims["nonce"] = nonce
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims))
	resp := doOIDCCallback(t, client, rig.server.URL, state, "conflict-code-a")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("first sign-in status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	_ = resp.Body.Close()

	userA, err := rig.users.GetByPrimaryEmail(ctx, sharedEmail)
	if err != nil {
		t.Fatalf("get userA by primary email: %v", err)
	}

	// A DIFFERENT `sub`, SAME issuer, SAME verified email -- a second
	// sign-in attempt through the SAME OIDC issuer this deployment is
	// configured against.
	client2 := newClient(t)
	state2, nonce2 := doOIDCLogin(t, client2, rig.server.URL)
	claims2 := rig.provider.defaultClaims("oidc-subject-conflict-b")
	claims2["email"] = sharedEmail
	claims2["email_verified"] = true
	claims2["nonce"] = nonce2
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claims2))
	resp2 := doOIDCCallback(t, client2, rig.server.URL, state2, "conflict-code-b")
	defer func() { _ = resp2.Body.Close() }()

	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("second sign-in status = %d, want %d (never merge a second same-issuer oidc identity onto an existing oidc-linked user)", resp2.StatusCode, http.StatusForbidden)
	}

	identities, err := rig.identities.ListForUser(ctx, userA.ID)
	if err != nil {
		t.Fatalf("list identities for userA: %v", err)
	}
	if len(identities) != 1 {
		t.Fatalf("identities for userA = %d, want 1 -- the second oidc sub must never have been attached", len(identities))
	}
	wantExternalIDA := rig.provider.issuer() + "|oidc-subject-conflict-a"
	if identities[0].ExternalID != wantExternalIDA {
		t.Errorf("userA's remaining identity external_id = %q, want %q (the FIRST sub's, untouched)", identities[0].ExternalID, wantExternalIDA)
	}

	wantExternalIDB := rig.provider.issuer() + "|oidc-subject-conflict-b"
	rows := getAuditLogRowsForResource(context.Background(), t, rig.pool, "identity", wantExternalIDB)
	if len(rows) != 1 {
		t.Fatalf("audit_log rows for the same-provider-conflict refusal = %d, want 1", len(rows))
	}
	if rows[0].Action != "identity.oidc_already_linked" {
		t.Errorf("audit_log action = %q, want %q", rows[0].Action, "identity.oidc_already_linked")
	}
}

// TestOIDCCallback_SameIssuerConflict_RefusedEvenWithEarlierIssuerIdentity
// proves review round 3's own finding Q3: identityConflictsWithExistingProvider
// (firsttimeidentity.go) used to compare a new OIDC sign-in's issuer
// against only ONE existing row (identities.GetByUserAndProvider, a ":one"
// query with no ORDER BY) -- for a user who already carries an identity
// from an EARLIER issuer (a legitimate, allowed case: see this test's own
// first phase below), that one row was often the stale, old-issuer one,
// so the same-issuer comparison never matched and a second SAME-issuer sub
// was merged instead of refused. The fix checks every one of the user's
// oidc rows.
//
// Two phases, both against the SAME existing user:
//
//  1. A sign-in through the CURRENT (different) issuer merges onto the
//     user despite their pre-existing OLD-issuer identity -- a genuinely
//     different issuer is a different provider instance and may still be
//     linked (identityConflictsWithExistingProvider's own doc comment).
//  2. A SECOND sign-in through that SAME current issuer, a different sub,
//     the same verified email -- must be refused (403), never merged, even
//     though the stale old-issuer row is still on file too.
func TestOIDCCallback_SameIssuerConflict_RefusedEvenWithEarlierIssuerIdentity(t *testing.T) {
	rig := newOIDCTestRig(t, defaultOIDCRiggedOptions())
	ctx := context.Background()

	const sharedEmail = "migrated@example.com"

	existingUser, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: sharedEmail,
		DisplayName:  "Migrated Person",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create existing user: %v", err)
	}

	oldExternalID := "https://old-issuer.example|old-sub"
	oldIdentityEmail := sharedEmail
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:        existingUser.ID,
		Provider:      sqlcgen.IdentityProviderOidc,
		ExternalID:    oldExternalID,
		Email:         &oldIdentityEmail,
		EmailVerified: true,
		LinkedVia:     sqlcgen.IdentityLinkedViaAutoEmail,
	}); err != nil {
		t.Fatalf("seed old-issuer identity: %v", err)
	}

	// Phase 1: a DIFFERENT issuer (this rig's own current one) merges onto
	// the existing user -- allowed, by design.
	clientA := newClient(t)
	stateA, nonceA := doOIDCLogin(t, clientA, rig.server.URL)
	claimsA := rig.provider.defaultClaims("oidc-subject-migrated-a")
	claimsA["email"] = sharedEmail
	claimsA["email_verified"] = true
	claimsA["nonce"] = nonceA
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claimsA))
	respA := doOIDCCallback(t, clientA, rig.server.URL, stateA, "migrated-code-a")
	defer func() { _ = respA.Body.Close() }()
	if respA.StatusCode != http.StatusFound {
		t.Fatalf("first (different-issuer) sign-in status = %d, want %d (a different issuer may legitimately coexist)", respA.StatusCode, http.StatusFound)
	}

	wantExternalIDA := rig.provider.issuer() + "|oidc-subject-migrated-a"
	identityA, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, wantExternalIDA)
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID (a): %v", err)
	}
	if identityA.UserID != existingUser.ID {
		t.Errorf("identityA.UserID = %v, want %v (merged onto the existing user)", identityA.UserID, existingUser.ID)
	}

	// Phase 2: a SECOND sign-in through the SAME current issuer, a
	// different sub, the same email -- must be refused, never merged, even
	// though the OLD-issuer row from before phase 1 is still on file.
	clientB := newClient(t)
	stateB, nonceB := doOIDCLogin(t, clientB, rig.server.URL)
	claimsB := rig.provider.defaultClaims("oidc-subject-migrated-b")
	claimsB["email"] = sharedEmail
	claimsB["email_verified"] = true
	claimsB["nonce"] = nonceB
	rig.provider.setNextIDToken(rig.provider.signIDToken(t, claimsB))
	respB := doOIDCCallback(t, clientB, rig.server.URL, stateB, "migrated-code-b")
	defer func() { _ = respB.Body.Close() }()
	if respB.StatusCode != http.StatusForbidden {
		t.Errorf("second (same-issuer) sign-in status = %d, want %d (never attach a second same-issuer identity, even with an earlier-issuer row present)", respB.StatusCode, http.StatusForbidden)
	}

	wantExternalIDB := rig.provider.issuer() + "|oidc-subject-migrated-b"
	if _, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderOidc, wantExternalIDB); !errorsIsNoRows(err) {
		t.Errorf("an identities row was created for the refused same-issuer sign-in (err=%v) -- want none", err)
	}

	identities, err := rig.identities.ListForUser(ctx, existingUser.ID)
	if err != nil {
		t.Fatalf("list identities for existingUser: %v", err)
	}
	if len(identities) != 2 {
		t.Fatalf("identities for existingUser = %d, want 2 (the seeded old-issuer row + phase 1's merged current-issuer row only)", len(identities))
	}

	rows := getAuditLogRowsForResource(context.Background(), t, rig.pool, "identity", wantExternalIDB)
	if len(rows) != 1 {
		t.Fatalf("audit_log rows for the same-provider-conflict refusal = %d, want 1", len(rows))
	}
	if rows[0].Action != "identity.oidc_already_linked" {
		t.Errorf("audit_log action = %q, want %q", rows[0].Action, "identity.oidc_already_linked")
	}
}
