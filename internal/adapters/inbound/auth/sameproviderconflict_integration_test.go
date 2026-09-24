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
