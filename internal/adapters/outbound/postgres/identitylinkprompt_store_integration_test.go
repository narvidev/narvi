//go:build integration

// Integration tests for IdentityLinkPromptStore and the §13.2
// ("identities + full RBAC", §13.2) additions to UserStore/IdentityStore/
// AuditLogStore -- mirrors slackthreadsession_store_integration_test.go's
// own "focused file per query" convention.
package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

func createTestUser(ctx context.Context, t *testing.T, users *narvipg.UserStore, email, displayName string, role sqlcgen.UserRole) sqlcgen.User {
	t.Helper()
	u, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: email, DisplayName: displayName, Role: role})
	if err != nil {
		t.Fatalf("create fixture user %q: %v", email, err)
	}
	return u
}

// TestIdentityLinkPromptStore_CreateGetDelete proves the round trip a
// magic-link consume handler depends on: create a prompt, look it up by
// its nonce hash, then delete it so it can never be consumed twice.
func TestIdentityLinkPromptStore_CreateGetDelete(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewIdentityLinkPromptStore(pool)

	expiresAt := time.Now().Add(24 * time.Hour)
	created, err := store.Create(ctx, sqlcgen.CreateIdentityLinkPromptParams{
		Provider:   sqlcgen.IdentityProviderSlack,
		ExternalID: "U123",
		NonceHash:  "deadbeef",
		ExpiresAt:  pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Provider != sqlcgen.IdentityProviderSlack || created.ExternalID != "U123" {
		t.Errorf("created row = %+v, want provider=slack external_id=U123", created)
	}

	got, err := store.GetByNonceHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("GetByNonceHash: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("GetByNonceHash id = %v, want %v", got.ID, created.ID)
	}

	latest, err := store.GetLatestForProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, "U123")
	if err != nil {
		t.Fatalf("GetLatestForProviderAndExternalID: %v", err)
	}
	if latest.ID != created.ID {
		t.Errorf("GetLatestForProviderAndExternalID id = %v, want %v", latest.ID, created.ID)
	}

	if err := store.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := store.GetByNonceHash(ctx, "deadbeef"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetByNonceHash after Delete = %v, want pgx.ErrNoRows", err)
	}
}

// TestIdentityLinkPromptStore_DeleteForProviderAndExternalID proves EVERY
// prompt for one (provider, external_id) is removed together -- the
// cleanup step §13.2's own algorithm runs once that identity is genuinely
// linked, so a stale magic link minted before the identity was linked can
// never be clicked afterward.
func TestIdentityLinkPromptStore_DeleteForProviderAndExternalID(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewIdentityLinkPromptStore(pool)

	expiresAt := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	for _, nonce := range []string{"nonce-a", "nonce-b"} {
		if _, err := store.Create(ctx, sqlcgen.CreateIdentityLinkPromptParams{
			Provider:   sqlcgen.IdentityProviderLinear,
			ExternalID: "linear-user-1",
			NonceHash:  nonce,
			ExpiresAt:  expiresAt,
		}); err != nil {
			t.Fatalf("create fixture prompt %q: %v", nonce, err)
		}
	}
	// A prompt for a DIFFERENT identity must survive.
	if _, err := store.Create(ctx, sqlcgen.CreateIdentityLinkPromptParams{
		Provider:   sqlcgen.IdentityProviderLinear,
		ExternalID: "linear-user-2",
		NonceHash:  "nonce-other",
		ExpiresAt:  expiresAt,
	}); err != nil {
		t.Fatalf("create unrelated fixture prompt: %v", err)
	}

	if err := store.DeleteForProviderAndExternalID(ctx, sqlcgen.IdentityProviderLinear, "linear-user-1"); err != nil {
		t.Fatalf("DeleteForProviderAndExternalID: %v", err)
	}

	for _, nonce := range []string{"nonce-a", "nonce-b"} {
		if _, err := store.GetByNonceHash(ctx, nonce); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("GetByNonceHash(%q) after DeleteForProviderAndExternalID = %v, want pgx.ErrNoRows", nonce, err)
		}
	}
	if _, err := store.GetByNonceHash(ctx, "nonce-other"); err != nil {
		t.Errorf("GetByNonceHash(nonce-other) = %v, want nil (unrelated identity's own prompt must survive)", err)
	}
}

// TestUserStore_GetByPrimaryEmail_CaseInsensitive proves the auto-link
// algorithm's own email-match half is case-insensitive (§13.2 step 2 --
// see queries/users.sql's own doc comment for why).
func TestUserStore_GetByPrimaryEmail_CaseInsensitive(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	users := narvipg.NewUserStore(pool)

	created := createTestUser(ctx, t, users, "Ada@Example.com", "Ada", sqlcgen.UserRoleMember)

	got, err := users.GetByPrimaryEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetByPrimaryEmail: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("GetByPrimaryEmail id = %v, want %v", got.ID, created.ID)
	}

	if _, err := users.GetByPrimaryEmail(ctx, "nobody@example.com"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetByPrimaryEmail(unknown) = %v, want pgx.ErrNoRows", err)
	}
}

// TestUserStore_ListAndUpdateRole proves List returns every user
// oldest-first and UpdateRole is the ONLY thing that changes an existing
// user's own role -- backs the members API's own list + role-change
// endpoints.
func TestUserStore_ListAndUpdateRole(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	users := narvipg.NewUserStore(pool)

	first := createTestUser(ctx, t, users, "first@example.com", "First", sqlcgen.UserRoleMember)
	second := createTestUser(ctx, t, users, "second@example.com", "Second", sqlcgen.UserRoleViewer)

	list, err := users.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(List()) = %d, want 2", len(list))
	}
	if list[0].ID != first.ID || list[1].ID != second.ID {
		t.Errorf("List() order = [%v, %v], want oldest-first [%v, %v]", list[0].ID, list[1].ID, first.ID, second.ID)
	}

	updated, err := users.UpdateRole(ctx, second.ID, sqlcgen.UserRoleMaintainer)
	if err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	if updated.Role != sqlcgen.UserRoleMaintainer {
		t.Errorf("UpdateRole result role = %v, want maintainer", updated.Role)
	}

	reFetched, err := users.GetByID(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetByID after UpdateRole: %v", err)
	}
	if reFetched.Role != sqlcgen.UserRoleMaintainer {
		t.Errorf("GetByID after UpdateRole role = %v, want maintainer (persisted)", reFetched.Role)
	}
}

// TestIdentityStore_ListVerifiedUserIDsByEmail_DeduplicatesAndFiltersUnverified
// proves: (a) only email_verified=true rows count, (b) the same user
// linked twice with the same email counts once (DISTINCT), (c) the
// match is case-insensitive, matching GetByPrimaryEmail's own identical
// convention, and (d) only a row whose email_checked_at lies within maxAge
// counts -- an older stamp and no stamp at all never do (§13.2).
func TestIdentityStore_ListVerifiedUserIDsByEmail_DeduplicatesAndFiltersUnverified(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	users := narvipg.NewUserStore(pool)
	identities := narvipg.NewIdentityStore(pool)

	user := createTestUser(ctx, t, users, "user-only@example.com", "User", sqlcgen.UserRoleMember)
	otherUser := createTestUser(ctx, t, users, "other@example.com", "Other", sqlcgen.UserRoleMember)
	staleUser := createTestUser(ctx, t, users, "stale@example.com", "Stale", sqlcgen.UserRoleMember)
	unstampedUser := createTestUser(ctx, t, users, "unstamped@example.com", "Unstamped", sqlcgen.UserRoleMember)

	email := "shared@example.com"
	upperEmail := "SHARED@EXAMPLE.COM"
	const maxAge = time.Hour

	mustCreateIdentity := func(externalID string, provider sqlcgen.IdentityProvider, u sqlcgen.User, verified bool, checkedAt pgtype.Timestamptz) {
		t.Helper()
		e := email
		if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
			UserID:         u.ID,
			Provider:       provider,
			ExternalID:     externalID,
			Email:          &e,
			EmailVerified:  verified,
			LinkedVia:      sqlcgen.IdentityLinkedViaAdmin,
			EmailCheckedAt: checkedAt,
		}); err != nil {
			t.Fatalf("create fixture identity %q: %v", externalID, err)
		}
	}
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}

	// user has TWO verified identities sharing the same email -- must
	// still count as ONE match.
	mustCreateIdentity("slack-1", sqlcgen.IdentityProviderSlack, user, true, now)
	mustCreateIdentity("linear-1", sqlcgen.IdentityProviderLinear, user, true, now)
	// otherUser has an UNVERIFIED identity with the same email -- must
	// never count as a match at all.
	mustCreateIdentity("slack-2", sqlcgen.IdentityProviderSlack, otherUser, false, now)
	// staleUser's verified email was confirmed longer than maxAge ago, and
	// unstampedUser's never -- neither is evidence.
	mustCreateIdentity("slack-3", sqlcgen.IdentityProviderSlack, staleUser, true, pgtype.Timestamptz{Time: time.Now().Add(-2 * maxAge), Valid: true})
	mustCreateIdentity("github-4", sqlcgen.IdentityProviderGithub, unstampedUser, true, pgtype.Timestamptz{})

	// Nobody else has a fixture with no matching identity -- irrelevant
	// user, unused email.
	_ = createTestUser(ctx, t, users, "unrelated@example.com", "Unrelated", sqlcgen.UserRoleMember)

	matches, err := identities.ListVerifiedUserIDsByEmail(ctx, upperEmail, maxAge)
	if err != nil {
		t.Fatalf("ListVerifiedUserIDsByEmail: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("len(matches) = %d, want 1 (deduplicated, unverified, stale and unstamped excluded); got %v", len(matches), matches)
	}
	if matches[0] != user.ID {
		t.Errorf("matches[0] = %v, want %v", matches[0], user.ID)
	}

	noMatches, err := identities.ListVerifiedUserIDsByEmail(ctx, "nobody-has-this@example.com", maxAge)
	if err != nil {
		t.Fatalf("ListVerifiedUserIDsByEmail(no match): %v", err)
	}
	if len(noMatches) != 0 {
		t.Errorf("len(noMatches) = %d, want 0", len(noMatches))
	}
}

// TestIdentityStore_ListForUserAndDelete proves ListForUser returns every
// identity linked to a user, and Delete removes exactly one by id --
// backing the members API's own per-member identity listing and admin
// manual-unlink endpoint.
func TestIdentityStore_ListForUserAndDelete(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	users := narvipg.NewUserStore(pool)
	identities := narvipg.NewIdentityStore(pool)

	user := createTestUser(ctx, t, users, "listforuser@example.com", "User", sqlcgen.UserRoleMember)

	slackIdentity, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:     user.ID,
		Provider:   sqlcgen.IdentityProviderSlack,
		ExternalID: "U999",
		LinkedVia:  sqlcgen.IdentityLinkedViaAutoEmail,
	})
	if err != nil {
		t.Fatalf("create slack identity: %v", err)
	}
	if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:     user.ID,
		Provider:   sqlcgen.IdentityProviderLinear,
		ExternalID: "linear-999",
		LinkedVia:  sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create linear identity: %v", err)
	}

	list, err := identities.ListForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(ListForUser()) = %d, want 2", len(list))
	}

	rowsDeleted, err := identities.Delete(ctx, slackIdentity.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if rowsDeleted != 1 {
		t.Errorf("Delete rowsDeleted = %d, want 1", rowsDeleted)
	}

	remaining, err := identities.ListForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListForUser after Delete: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Provider != sqlcgen.IdentityProviderLinear {
		t.Errorf("ListForUser after Delete = %+v, want only the linear identity left", remaining)
	}

	// Deleting an already-gone id is a documented no-op (0 rows, no error).
	rowsDeleted, err = identities.Delete(ctx, slackIdentity.ID)
	if err != nil {
		t.Fatalf("Delete (already gone): %v", err)
	}
	if rowsDeleted != 0 {
		t.Errorf("Delete (already gone) rowsDeleted = %d, want 0", rowsDeleted)
	}
}

// TestAuditLogStore_List proves List returns rows newest-first and
// respects limit/offset -- backs the members API's own audit-log read
// endpoint.
func TestAuditLogStore_List(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)

	var created []sqlcgen.AuditLog
	for i := 0; i < 3; i++ {
		row, err := auditLog.Record(ctx, sqlcgen.CreateAuditLogEntryParams{
			Action:       "test.action",
			ResourceType: "test_resource",
			ResourceID:   "resource-id",
			DetailJson:   []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("Record #%d: %v", i, err)
		}
		created = append(created, row)
	}

	list, err := auditLog.List(ctx, 2, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(List(limit=2)) = %d, want 2", len(list))
	}
	// Newest first: the 3rd and 2nd created rows, in that order.
	if list[0].ID != created[2].ID || list[1].ID != created[1].ID {
		t.Errorf("List() order = [%v, %v], want newest-first [%v, %v]", list[0].ID, list[1].ID, created[2].ID, created[1].ID)
	}

	page2, err := auditLog.List(ctx, 2, 2)
	if err != nil {
		t.Fatalf("List (page 2): %v", err)
	}
	if len(page2) != 1 || page2[0].ID != created[0].ID {
		t.Errorf("List(limit=2, offset=2) = %+v, want just the oldest row", page2)
	}
}
