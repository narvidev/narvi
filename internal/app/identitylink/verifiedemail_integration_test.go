//go:build integration

// Integration tests for §13.2's definition of a verified email in the
// identity graph: Resolve links an identity automatically only on an email
// the provider vouches for, AutoLink stores exactly the attestation it is
// given, and a Slack automatic link made before Slack's confirmed-email
// rule (no email_checked_at, migrations/000177) is re-checked at its next
// use -- kept when the provider confirms it, removed and audited when not,
// and never confused with a link a person or an admin made.
package identitylink_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/identitylink"
)

// auditEntriesFor returns the audit-log entries recorded for resourceID
// with action, oldest first.
func auditEntriesFor(ctx context.Context, t *testing.T, deps identitylink.Deps, action, resourceID string) []sqlcgen.AuditLog {
	t.Helper()
	all, err := deps.AuditLog.List(ctx, 500, 0)
	if err != nil {
		t.Fatalf("AuditLog.List: %v", err)
	}
	var out []sqlcgen.AuditLog
	for _, e := range all {
		if e.Action == action && e.ResourceID == resourceID {
			out = append(out, e)
		}
	}
	return out
}

// matchedUserIDs is MatchUserIDs, failing the test on an error.
func matchedUserIDs(ctx context.Context, t *testing.T, deps identitylink.Deps, email string) []string {
	t.Helper()
	ids, err := identitylink.MatchUserIDs(ctx, deps, email)
	if err != nil {
		t.Fatalf("MatchUserIDs(%q): %v", email, err)
	}
	return ids
}

// seedIdentity inserts an identities row exactly as given -- for an
// automatic Slack link, with no EmailCheckedAt, that is what the previous
// binary wrote.
func seedIdentity(ctx context.Context, t *testing.T, deps identitylink.Deps, arg sqlcgen.CreateIdentityParams) sqlcgen.Identity {
	t.Helper()
	row, err := deps.Identities.Create(ctx, arg)
	if err != nil {
		t.Fatalf("seed identity %s/%s: %v", arg.Provider, arg.ExternalID, err)
	}
	return row
}

// TestResolve_AutoLinksOnlyOnAVerifiedEmail proves Resolve's gate: an email
// matching exactly one user links the identity automatically only when the
// provider vouches for it, and the stored email_verified says so. An
// unverified one gets the link prompt instead, and the account-link flow
// still links it -- with no email marked verified.
func TestResolve_AutoLinksOnlyOnAVerifiedEmail(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	tests := []struct {
		name       string
		externalID string
		email      string
		verified   bool
	}{
		{"verified email", "U-GATE-VERIFIED", "gate-verified@example.com", true},
		{"unverified email", "U-GATE-UNVERIFIED", "gate-unverified@example.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := createFixtureUser(ctx, t, deps.Users, tt.email)

			res, err := identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderSlack, tt.externalID, identitylink.ProfileEmail{Address: tt.email, Verified: tt.verified}, true)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			row, err := deps.Identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, tt.externalID)
			if tt.verified {
				if !res.AutoLinked || res.UserID != user.ID {
					t.Fatalf("Resolve = %+v, want an automatic link to %v", res, user.ID)
				}
				if err != nil {
					t.Fatalf("GetByProviderAndExternalID: %v", err)
				}
				if row.LinkedVia != sqlcgen.IdentityLinkedViaAutoEmail || !row.EmailVerified || !row.EmailCheckedAt.Valid {
					t.Errorf("stored identity = %+v, want auto_email, email_verified, email_checked_at set", row)
				}
				return
			}

			if res.AutoLinked || res.UserID.Valid {
				t.Fatalf("Resolve = %+v, want no automatic link on an unverified email", res)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("GetByProviderAndExternalID err = %v (row %+v), want no identity stored", err, row)
			}
			if !strings.HasPrefix(res.MagicLinkURL, deps.PublicBaseURL+identitylink.MagicLinkPath) {
				t.Fatalf("MagicLinkURL = %q, want the link prompt", res.MagicLinkURL)
			}

			// The account-link flow still links it, as the person who
			// signs in -- and stores no verified email.
			nonce := res.MagicLinkURL[len(deps.PublicBaseURL+identitylink.MagicLinkPath):]
			linked, err := identitylink.Consume(ctx, deps, nonce, user.ID)
			if err != nil {
				t.Fatalf("Consume: %v", err)
			}
			if linked.UserID != user.ID || linked.LinkedVia != sqlcgen.IdentityLinkedViaPrompt || linked.EmailVerified {
				t.Errorf("Consume stored %+v, want linked_via prompt to %v with email_verified false", linked, user.ID)
			}
		})
	}
}

// TestAutoLink_StoresTheAttestationItIsGiven proves AutoLink never assumes
// an email verified: email_verified and email_checked_at follow
// ProfileEmail.Verified, and an automatic link on an unverified email is
// neither used (LookupLinkedUserID) nor evidence for matching (MatchUserIDs)
// -- for every provider. It also proves AutoLink, losing its INSERT race to
// an automatic link nothing has confirmed, resolves to no one.
func TestAutoLink_StoresTheAttestationItIsGiven(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	tests := []struct {
		name     string
		provider sqlcgen.IdentityProvider
		verified bool
	}{
		{"slack, verified", sqlcgen.IdentityProviderSlack, true},
		{"slack, unverified", sqlcgen.IdentityProviderSlack, false},
		{"linear, verified", sqlcgen.IdentityProviderLinear, true},
		{"linear, unverified", sqlcgen.IdentityProviderLinear, false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suffix := string(rune('a' + i))
			user := createFixtureUser(ctx, t, deps.Users, "attest-primary-"+suffix+"@example.com")
			// Not the user's primary email: only the identity row can make
			// it match.
			identityEmail := "attest-identity-" + suffix + "@example.com"
			externalID := "X-ATTEST-" + suffix

			res, err := identitylink.AutoLink(ctx, deps, tt.provider, externalID, identitylink.ProfileEmail{Address: identityEmail, Verified: tt.verified}, user.ID.String(), nil)
			if err != nil {
				t.Fatalf("AutoLink: %v", err)
			}
			if !res.AutoLinked {
				t.Errorf("AutoLinked = false, want true")
			}

			row, err := deps.Identities.GetByProviderAndExternalID(ctx, tt.provider, externalID)
			if err != nil {
				t.Fatalf("GetByProviderAndExternalID: %v", err)
			}
			if row.EmailVerified != tt.verified {
				t.Errorf("email_verified = %v, want %v (the attestation AutoLink was given)", row.EmailVerified, tt.verified)
			}
			if row.EmailCheckedAt.Valid != tt.verified {
				t.Errorf("email_checked_at set = %v, want %v", row.EmailCheckedAt.Valid, tt.verified)
			}

			gotUser, linked, err := identitylink.LookupLinkedUserID(ctx, deps, tt.provider, externalID)
			if err != nil {
				t.Fatalf("LookupLinkedUserID: %v", err)
			}
			if linked != tt.verified || (linked && gotUser != user.ID) {
				t.Errorf("LookupLinkedUserID = (%v, %v), want linked=%v to %v", gotUser, linked, tt.verified, user.ID)
			}

			matched := matchedUserIDs(ctx, t, deps, identityEmail)
			if wantMatch := tt.verified; (len(matched) == 1 && matched[0] == user.ID.String()) != wantMatch || len(matched) > 1 {
				t.Errorf("MatchUserIDs(%q) = %v, want [%v] only when verified (%v)", identityEmail, matched, user.ID, tt.verified)
			}
		})
	}

	t.Run("losing the race to an unchecked automatic link", func(t *testing.T) {
		holder := createFixtureUser(ctx, t, deps.Users, "race-holder@example.com")
		email := "race-holder@example.com"
		seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
			UserID: holder.ID, Provider: sqlcgen.IdentityProviderSlack, ExternalID: "U-RACE-UNCHECKED",
			Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail,
		})

		res, err := identitylink.AutoLink(ctx, deps, sqlcgen.IdentityProviderSlack, "U-RACE-UNCHECKED", identitylink.ProfileEmail{Address: email, Verified: true}, holder.ID.String(), nil)
		if err != nil {
			t.Fatalf("AutoLink: %v", err)
		}
		if res.UserID.Valid || res.AutoLinked {
			t.Errorf("AutoLink = %+v, want no one: the winner is an automatic link nothing has confirmed", res)
		}
	})
}

// TestResolve_RechecksAnAutoLinkMadeBeforeTheConfirmedEmailRule proves the
// re-check of a Slack automatic link with no email_checked_at -- what the
// previous binary wrote for every Slack profile email: until re-checked it
// resolves to no one and is no evidence; at its next use it is kept and
// stamped when the provider vouches for the same email, removed with an
// identity.auto_link_revoked entry otherwise, and left as it is when the
// fetch learned nothing. A link a person or an admin made, and another
// provider's automatic link, are never touched.
func TestResolve_RechecksAnAutoLinkMadeBeforeTheConfirmedEmailRule(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	type outcome int
	const (
		kept outcome = iota
		revoked
		untouched
	)
	tests := []struct {
		name string
		// fetched is what the provider says now; fetchedOK false is a
		// fetch that learned nothing.
		fetched   func(linkedEmail, otherEmail string) identitylink.ProfileEmail
		fetchedOK bool
		want      outcome
		// wantReason is the revoke entry's reason; wantRelinkToOther, the
		// new automatic link the fetched email then makes.
		wantReason        string
		wantRelinkToOther bool
	}{
		{
			name:      "fetch learned nothing",
			fetched:   func(string, string) identitylink.ProfileEmail { return identitylink.ProfileEmail{} },
			fetchedOK: false,
			want:      untouched,
		},
		{
			name: "same email confirmed, in another case",
			fetched: func(linked, _ string) identitylink.ProfileEmail {
				return identitylink.ProfileEmail{Address: strings.ToUpper(linked), Verified: true}
			},
			fetchedOK: true,
			want:      kept,
		},
		{
			name: "same email, not confirmed",
			fetched: func(linked, _ string) identitylink.ProfileEmail {
				return identitylink.ProfileEmail{Address: linked, Verified: false}
			},
			fetchedOK:  true,
			want:       revoked,
			wantReason: "email_not_verified",
		},
		{
			name: "another email, confirmed, matching another user",
			fetched: func(_, other string) identitylink.ProfileEmail {
				return identitylink.ProfileEmail{Address: other, Verified: true}
			},
			fetchedOK:         true,
			want:              revoked,
			wantReason:        "email_changed",
			wantRelinkToOther: true,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suffix := string(rune('a' + i))
			holder := createFixtureUser(ctx, t, deps.Users, "recheck-holder-"+suffix+"@example.com")
			other := createFixtureUser(ctx, t, deps.Users, "recheck-other-"+suffix+"@example.com")
			// The linked email is not the holder's primary email, so only
			// the identity row can make it match.
			linkedEmail := "recheck-linked-" + suffix + "@example.com"
			externalID := "U-RECHECK-" + suffix
			seeded := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
				UserID: holder.ID, Provider: sqlcgen.IdentityProviderSlack, ExternalID: externalID,
				Email: &linkedEmail, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail,
			})

			// Before its re-check: used by nothing.
			if _, linked, err := identitylink.LookupLinkedUserID(ctx, deps, sqlcgen.IdentityProviderSlack, externalID); err != nil || linked {
				t.Fatalf("LookupLinkedUserID before re-check = (linked %v, %v), want not linked", linked, err)
			}
			if got := matchedUserIDs(ctx, t, deps, linkedEmail); len(got) != 0 {
				t.Fatalf("MatchUserIDs before re-check = %v, want none: an unchecked link is no evidence", got)
			}

			res, err := identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderSlack, externalID, tt.fetched(linkedEmail, other.PrimaryEmail), tt.fetchedOK)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			row, rowErr := deps.Identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, externalID)
			revokes := auditEntriesFor(ctx, t, deps, "identity.auto_link_revoked", seeded.ID.String())

			switch tt.want {
			case untouched:
				if res.UserID.Valid || res.AutoLinked || res.MagicLinkURL != "" {
					t.Errorf("Resolve = %+v, want nothing: no email learned", res)
				}
				if rowErr != nil || row.ID != seeded.ID || row.EmailCheckedAt.Valid {
					t.Errorf("identity = %+v (err %v), want the seeded row, still unchecked", row, rowErr)
				}
				if len(revokes) != 0 {
					t.Errorf("revoke entries = %d, want 0", len(revokes))
				}

			case kept:
				if res.UserID != holder.ID || res.AutoLinked {
					t.Errorf("Resolve = %+v, want the existing link to %v", res, holder.ID)
				}
				if rowErr != nil || row.ID != seeded.ID || !row.EmailVerified || !row.EmailCheckedAt.Valid {
					t.Errorf("identity = %+v (err %v), want the seeded row, stamped", row, rowErr)
				}
				if len(revokes) != 0 {
					t.Errorf("revoke entries = %d, want 0", len(revokes))
				}
				if gotUser, linked, err := identitylink.LookupLinkedUserID(ctx, deps, sqlcgen.IdentityProviderSlack, externalID); err != nil || !linked || gotUser != holder.ID {
					t.Errorf("LookupLinkedUserID after re-check = (%v, %v, %v), want linked to %v", gotUser, linked, err, holder.ID)
				}
				if got := matchedUserIDs(ctx, t, deps, linkedEmail); len(got) != 1 || got[0] != holder.ID.String() {
					t.Errorf("MatchUserIDs after re-check = %v, want [%v]", got, holder.ID)
				}

			case revoked:
				if len(revokes) != 1 {
					t.Fatalf("revoke entries = %d, want 1", len(revokes))
				}
				var detail map[string]any
				if err := json.Unmarshal(revokes[0].DetailJson, &detail); err != nil {
					t.Fatalf("decode revoke detail: %v", err)
				}
				if detail["user_id"] != holder.ID.String() || detail["reason"] != tt.wantReason || detail["provider"] != "slack" || detail["external_id"] != externalID {
					t.Errorf("revoke detail = %v, want user_id %v, reason %q, provider slack, external_id %q", detail, holder.ID, tt.wantReason, externalID)
				}
				if revokes[0].ActorUserID.Valid {
					t.Errorf("revoke actor = %v, want none (system-driven)", revokes[0].ActorUserID)
				}
				if res.UserID == holder.ID {
					t.Errorf("Resolve resolved to the revoked link's user %v", holder.ID)
				}
				if tt.wantRelinkToOther {
					if !res.AutoLinked || res.UserID != other.ID {
						t.Errorf("Resolve = %+v, want a new automatic link to %v", res, other.ID)
					}
					if rowErr != nil || row.ID == seeded.ID || row.UserID != other.ID || !row.EmailCheckedAt.Valid {
						t.Errorf("identity = %+v (err %v), want a new, stamped link to %v", row, rowErr, other.ID)
					}
					return
				}
				if res.UserID.Valid || res.MagicLinkURL == "" {
					t.Errorf("Resolve = %+v, want no link and the link prompt", res)
				}
				if !errors.Is(rowErr, pgx.ErrNoRows) {
					t.Errorf("identity after revoke = %+v (err %v), want none", row, rowErr)
				}
			}
		})
	}
}

// TestResolve_RecheckLeavesOtherLinksAlone proves the re-check reads and
// removes only a Slack automatic link nothing has confirmed: a link a
// person confirmed through the link prompt, one an admin made, a stamped
// Slack automatic link, and another provider's automatic link (unstamped,
// as every one made before migrations/000177 is) all resolve as they are,
// on an email the provider does not vouch for, and the store refuses to
// delete any of them as unchecked. Another provider's verified email still
// counts as evidence without the stamp.
func TestResolve_RecheckLeavesOtherLinksAlone(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	checked := pgtype.Timestamptz{Valid: true}
	tests := []struct {
		name     string
		provider sqlcgen.IdentityProvider
		via      sqlcgen.IdentityLinkedVia
		verified bool
		stamp    pgtype.Timestamptz
	}{
		{"slack link via prompt", sqlcgen.IdentityProviderSlack, sqlcgen.IdentityLinkedViaPrompt, false, pgtype.Timestamptz{}},
		{"slack link via admin", sqlcgen.IdentityProviderSlack, sqlcgen.IdentityLinkedViaAdmin, false, pgtype.Timestamptz{}},
		{"slack automatic link, stamped", sqlcgen.IdentityProviderSlack, sqlcgen.IdentityLinkedViaAutoEmail, true, checked},
		{"linear automatic link, unstamped", sqlcgen.IdentityProviderLinear, sqlcgen.IdentityLinkedViaAutoEmail, true, pgtype.Timestamptz{}},
		{"github automatic link, unstamped", sqlcgen.IdentityProviderGithub, sqlcgen.IdentityLinkedViaAutoEmail, true, pgtype.Timestamptz{}},
		{"oidc automatic link, unstamped", sqlcgen.IdentityProviderOidc, sqlcgen.IdentityLinkedViaAutoEmail, true, pgtype.Timestamptz{}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suffix := string(rune('a' + i))
			user := createFixtureUser(ctx, t, deps.Users, "leave-alone-"+suffix+"@example.com")
			email := "leave-alone-identity-" + suffix + "@example.com"
			externalID := "X-LEAVE-" + suffix
			stamp := tt.stamp
			if stamp.Valid {
				stamp.Time = time.Now()
			}
			seeded := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
				UserID: user.ID, Provider: tt.provider, ExternalID: externalID,
				Email: &email, EmailVerified: tt.verified, LinkedVia: tt.via, EmailCheckedAt: stamp,
			})

			if gotUser, linked, err := identitylink.LookupLinkedUserID(ctx, deps, tt.provider, externalID); err != nil || !linked || gotUser != user.ID {
				t.Errorf("LookupLinkedUserID = (%v, %v, %v), want linked to %v", gotUser, linked, err, user.ID)
			}
			// A verified email here is evidence, stamped or not: only a
			// Slack automatic link needs the stamp. (The email is not the
			// user's primary email, so only this row can match it.)
			matched := matchedUserIDs(ctx, t, deps, email)
			if tt.verified && (len(matched) != 1 || matched[0] != user.ID.String()) {
				t.Errorf("MatchUserIDs(%q) = %v, want [%v]: a verified %s email is evidence", email, matched, user.ID, tt.provider)
			}
			if !tt.verified && len(matched) != 0 {
				t.Errorf("MatchUserIDs(%q) = %v, want none: the email is not verified", email, matched)
			}

			res, err := identitylink.Resolve(ctx, deps, tt.provider, externalID, identitylink.ProfileEmail{Address: email, Verified: false}, true)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.UserID != user.ID || res.AutoLinked || res.MagicLinkURL != "" {
				t.Errorf("Resolve = %+v, want the existing link to %v, nothing else", res, user.ID)
			}

			deleted, err := deps.Identities.DeleteUncheckedAutoLink(ctx, seeded.ID)
			if err != nil {
				t.Fatalf("DeleteUncheckedAutoLink: %v", err)
			}
			if deleted != 0 {
				t.Errorf("DeleteUncheckedAutoLink deleted %d rows, want 0", deleted)
			}
			if row, err := deps.Identities.GetByProviderAndExternalID(ctx, tt.provider, externalID); err != nil || row.ID != seeded.ID {
				t.Errorf("identity = %+v (err %v), want the seeded row unchanged", row, err)
			}
			if got := auditEntriesFor(ctx, t, deps, "identity.auto_link_revoked", seeded.ID.String()); len(got) != 0 {
				t.Errorf("revoke entries = %d, want 0", len(got))
			}
		})
	}
}

// waitForLockedStatement waits until another backend is blocked on a row
// lock while running a statement on identities, so the test knows a
// Resolve has reached its store call and is waiting behind the test's own
// transaction.
func waitForLockedStatement(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query ILIKE '%identities%' AND pid <> pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no statement on identities ever waited on the test's row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestResolve_RecheckLosesARaceToAnotherEvent proves the re-check's race
// branches, deterministically: the test holds the unchecked link's row
// lock, lets a Resolve reach its store call and wait behind it, then
// settles the link the other way, as a concurrent event would. The store
// call then changes no row, and Resolve looks the identity up again
// rather than trusting the answer it set out to write.
func TestResolve_RecheckLosesARaceToAnotherEvent(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	tests := []struct {
		name string
		// fetched is what this Resolve's provider says; other is what the
		// concurrent event does to the row first.
		fetched   func(linkedEmail string) identitylink.ProfileEmail
		other     string
		wantOwner bool
	}{
		{
			name:    "confirm finds the link already removed",
			fetched: func(e string) identitylink.ProfileEmail { return identitylink.ProfileEmail{Address: e, Verified: true} },
			other:   `DELETE FROM identities WHERE id = $1`,
		},
		{
			name: "removal finds the link already confirmed",
			fetched: func(e string) identitylink.ProfileEmail {
				return identitylink.ProfileEmail{Address: e, Verified: false}
			},
			other:     `UPDATE identities SET email_checked_at = now() WHERE id = $1`,
			wantOwner: true,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suffix := string(rune('a' + i))
			holder := createFixtureUser(ctx, t, deps.Users, "race-owner-"+suffix+"@example.com")
			// Not the holder's primary email: once the row is gone, nothing
			// matches it.
			linkedEmail := "race-linked-" + suffix + "@example.com"
			externalID := "U-RACE-RECHECK-" + suffix
			seeded := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
				UserID: holder.ID, Provider: sqlcgen.IdentityProviderSlack, ExternalID: externalID,
				Email: &linkedEmail, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail,
			})

			lock, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = lock.Rollback(ctx) }()
			if _, err := lock.Exec(ctx, `SELECT id FROM identities WHERE id = $1 FOR UPDATE`, seeded.ID); err != nil {
				t.Fatalf("lock row: %v", err)
			}

			var res identitylink.Resolution
			var group errgroup.Group
			group.Go(func() error {
				var resolveErr error
				res, resolveErr = identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderSlack, externalID, tt.fetched(linkedEmail), true)
				return resolveErr
			})

			waitForLockedStatement(ctx, t, pool)
			if _, err := lock.Exec(ctx, tt.other, seeded.ID); err != nil {
				t.Fatalf("concurrent event: %v", err)
			}
			if err := lock.Commit(ctx); err != nil {
				t.Fatalf("commit concurrent event: %v", err)
			}
			if err := group.Wait(); err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			revokes := auditEntriesFor(ctx, t, deps, "identity.auto_link_revoked", seeded.ID.String())
			if len(revokes) != 0 {
				t.Errorf("revoke entries = %d, want 0: this Resolve removed nothing", len(revokes))
			}
			if tt.wantOwner {
				if res.UserID != holder.ID || res.MagicLinkURL != "" {
					t.Errorf("Resolve = %+v, want the link the other event confirmed, to %v", res, holder.ID)
				}
				if row, err := deps.Identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, externalID); err != nil || row.ID != seeded.ID {
					t.Errorf("identity = %+v (err %v), want the confirmed row kept", row, err)
				}
				return
			}
			if res.UserID.Valid || res.MagicLinkURL == "" {
				t.Errorf("Resolve = %+v, want no one and the link prompt: the link it confirmed was already gone", res)
			}
		})
	}
}

// TestResolve_ConcurrentRechecksOfOneLink runs many Resolves of the same
// unchecked link at once, verified and unverified mixed, round after round.
// Whichever settles it first decides for all: either the link is confirmed
// and every Resolve resolves to its user, or it is removed exactly once and
// none does -- never a Resolve answering for a link that is gone.
func TestResolve_ConcurrentRechecksOfOneLink(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	const (
		rounds  = 20
		callers = 6
	)
	for round := range rounds {
		holder := createFixtureUser(ctx, t, deps.Users, fmt.Sprintf("concurrent-owner-%02d@example.com", round))
		linkedEmail := fmt.Sprintf("concurrent-linked-%02d@example.com", round)
		externalID := fmt.Sprintf("U-CONCURRENT-%02d", round)
		seeded := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
			UserID: holder.ID, Provider: sqlcgen.IdentityProviderSlack, ExternalID: externalID,
			Email: &linkedEmail, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail,
		})

		results := make([]identitylink.Resolution, callers)
		var group errgroup.Group
		for i := range callers {
			// Round 0 is all unverified; every other round alternates.
			verified := round > 0 && i%2 == round%2
			group.Go(func() error {
				var err error
				results[i], err = identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderSlack, externalID, identitylink.ProfileEmail{Address: linkedEmail, Verified: verified}, true)
				return err
			})
		}
		if err := group.Wait(); err != nil {
			t.Fatalf("round %d: Resolve: %v", round, err)
		}

		revokes := auditEntriesFor(ctx, t, deps, "identity.auto_link_revoked", seeded.ID.String())
		_, rowErr := deps.Identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, externalID)
		removed := errors.Is(rowErr, pgx.ErrNoRows)
		if !removed && rowErr != nil {
			t.Fatalf("round %d: GetByProviderAndExternalID: %v", round, rowErr)
		}
		switch {
		case removed && len(revokes) != 1:
			t.Errorf("round %d: link removed with %d revoke entries, want exactly 1", round, len(revokes))
		case !removed && len(revokes) != 0:
			t.Errorf("round %d: link kept with %d revoke entries, want 0", round, len(revokes))
		}
		for i, res := range results {
			if removed && res.UserID == holder.ID {
				t.Errorf("round %d, caller %d: resolved to %v through a link that was removed", round, i, holder.ID)
			}
			if !removed && res.UserID != holder.ID {
				t.Errorf("round %d, caller %d: Resolve = %+v, want the confirmed link to %v", round, i, res, holder.ID)
			}
		}
	}
}
