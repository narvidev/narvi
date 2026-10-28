//go:build integration

// Integration tests for §13.2's email evidence: MatchUserIDs counts only an
// identity email its provider confirmed within Deps.EvidenceMaxAge, and
// RefreshSignInEvidence records, at each GitHub or OIDC sign-in, the email
// the provider presents -- in one transaction, never changing the link,
// with the user's primary email following the identity it came from.
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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/identitylink"
)

// testEvidenceMaxAge is the evidence limit these tests run with: short
// enough that a stamp just outside it is easy to write, long enough that
// the test's own running time never matters.
const testEvidenceMaxAge = time.Hour

// stampAgo sets identityID's email_checked_at to age before the database
// clock -- the clock MatchUserIDs compares against.
func stampAgo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, identityID pgtype.UUID, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE identities SET email_checked_at = now() - make_interval(secs => $2) WHERE id = $1`, identityID, age.Seconds()); err != nil {
		t.Fatalf("stamp identity %v: %v", identityID, err)
	}
}

// stampedJustNow reports whether identityID's email_checked_at lies within
// the last minute of the database clock.
func stampedJustNow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, identityID pgtype.UUID) bool {
	t.Helper()
	var recent bool
	if err := pool.QueryRow(ctx, `SELECT coalesce(email_checked_at > now() - interval '1 minute', false) FROM identities WHERE id = $1`, identityID).Scan(&recent); err != nil {
		t.Fatalf("read identity %v stamp: %v", identityID, err)
	}
	return recent
}

// auditDetail decodes one audit entry's detail.
func auditDetail(t *testing.T, entry sqlcgen.AuditLog) map[string]any {
	t.Helper()
	var detail map[string]any
	if err := json.Unmarshal(entry.DetailJson, &detail); err != nil {
		t.Fatalf("decode audit detail: %v", err)
	}
	return detail
}

// readEvidenceRefreshCount sums identity_email_evidence_refreshes_total
// for provider and result -- cumulative across this test binary (retry_
// test.go's TestMain owns the reader), so callers diff two readings.
func readEvidenceRefreshCount(ctx context.Context, t *testing.T, provider sqlcgen.IdentityProvider, result string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "narvi/identitylink" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "identity_email_evidence_refreshes_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("identity_email_evidence_refreshes_total data = %T, want metricdata.Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				p, _ := dp.Attributes.Value(attribute.Key("provider"))
				r, _ := dp.Attributes.Value(attribute.Key("result"))
				if p.AsString() == string(provider) && r.AsString() == result {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// evidenceRow is one identities row a MatchUserIDs case seeds for its user,
// carrying the case's email.
type evidenceRow struct {
	provider sqlcgen.IdentityProvider
	via      sqlcgen.IdentityLinkedVia
	verified bool
	// age is how long ago its provider confirmed the email; unstamped
	// leaves email_checked_at NULL.
	age       time.Duration
	unstamped bool
}

// TestMatchUserIDs_CountsOnlyCurrentEvidence proves §13.2 step 2's one
// rule: an email matches a user only through an identity row whose email
// is verified and was confirmed within EvidenceMaxAge of the database
// clock, compared ignoring case. A row never stamped -- a sign-in row from
// before sign-ins stamped, or a Slack automatic link nothing has checked --
// is no evidence, and neither is a user's primary email on its own.
func TestMatchUserIDs_CountsOnlyCurrentEvidence(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)
	deps.EvidenceMaxAge = testEvidenceMaxAge

	signIn := evidenceRow{provider: sqlcgen.IdentityProviderGithub, via: sqlcgen.IdentityLinkedViaAdmin, verified: true}
	tests := []struct {
		name string
		rows []evidenceRow
		// primary makes the case's email the user's primary email too.
		primary bool
		// lookup is the address MatchUserIDs is asked about.
		lookup func(email string) string
		want   bool
	}{
		{name: "checked now", rows: []evidenceRow{signIn}, want: true},
		{name: "checked just inside the limit", rows: []evidenceRow{{provider: sqlcgen.IdentityProviderOidc, via: sqlcgen.IdentityLinkedViaAdmin, verified: true, age: testEvidenceMaxAge - time.Minute}}, want: true},
		{name: "checked just outside the limit", rows: []evidenceRow{{provider: sqlcgen.IdentityProviderGithub, via: sqlcgen.IdentityLinkedViaAdmin, verified: true, age: testEvidenceMaxAge + time.Minute}}},
		{name: "a sign-in row never stamped", rows: []evidenceRow{{provider: sqlcgen.IdentityProviderGithub, via: sqlcgen.IdentityLinkedViaAdmin, verified: true, unstamped: true}}},
		{name: "unverified, checked now", rows: []evidenceRow{{provider: sqlcgen.IdentityProviderGithub, via: sqlcgen.IdentityLinkedViaAdmin}}},
		{name: "a Slack automatic link nothing has checked", rows: []evidenceRow{{provider: sqlcgen.IdentityProviderSlack, via: sqlcgen.IdentityLinkedViaAutoEmail, verified: true, unstamped: true}}},
		{name: "the primary email alone", primary: true},
		{name: "the primary email and two recent identities, counted once", primary: true, rows: []evidenceRow{
			signIn,
			{provider: sqlcgen.IdentityProviderLinear, via: sqlcgen.IdentityLinkedViaAutoEmail, verified: true, age: time.Minute},
		}, want: true},
		{name: "asked in another case", rows: []evidenceRow{signIn}, lookup: strings.ToUpper, want: true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			email := fmt.Sprintf("evidence-%02d@example.com", i)
			primary := fmt.Sprintf("evidence-primary-%02d@example.com", i)
			if tt.primary {
				primary = email
			}
			user := createFixtureUser(ctx, t, deps.Users, primary)
			for j, r := range tt.rows {
				row := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
					UserID: user.ID, Provider: r.provider, ExternalID: fmt.Sprintf("X-EVIDENCE-%02d-%d", i, j),
					Email: &email, EmailVerified: r.verified, LinkedVia: r.via,
				})
				if !r.unstamped {
					stampAgo(ctx, t, pool, row.ID, r.age)
				}
			}

			lookup := email
			if tt.lookup != nil {
				lookup = tt.lookup(email)
			}
			got := matchedUserIDs(ctx, t, deps, lookup)
			if tt.want && (len(got) != 1 || got[0] != user.ID.String()) {
				t.Errorf("MatchUserIDs(%q) = %v, want exactly [%v]", lookup, got, user.ID)
			}
			if !tt.want && len(got) != 0 {
				t.Errorf("MatchUserIDs(%q) = %v, want no match", lookup, got)
			}
		})
	}
}

// TestMatchUserIDs_RefusesWithoutAMaxAge proves a zero or negative evidence
// limit is a configuration mistake MatchUserIDs reports, never a silent "no
// match" -- on an email that does match under a positive limit.
func TestMatchUserIDs_RefusesWithoutAMaxAge(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	user := createFixtureUserWithEvidence(ctx, t, deps, "max-age@example.com")
	for _, tt := range []struct {
		name    string
		maxAge  time.Duration
		wantErr bool
	}{
		{"a positive limit matches", testEvidenceMaxAge, false},
		{"a zero limit", 0, true},
		{"a negative limit", -testEvidenceMaxAge, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := deps
			d.EvidenceMaxAge = tt.maxAge
			got, err := identitylink.MatchUserIDs(ctx, d, "max-age@example.com")
			if tt.wantErr {
				if err == nil || got != nil {
					t.Errorf("MatchUserIDs = (%v, %v), want an error and no ids", got, err)
				}
				return
			}
			if err != nil || len(got) != 1 || got[0] != user.ID.String() {
				t.Errorf("MatchUserIDs = (%v, %v), want [%v]", got, err, user.ID)
			}
		})
	}
}

// TestResolve_EvidenceOutsideTheMaxAgeGetsTheLinkPrompt proves the limit
// reaches Slack and Linear: a verified profile email whose only evidence
// was confirmed longer ago than the limit gets the link prompt and links
// nothing; inside the limit, it links automatically.
func TestResolve_EvidenceOutsideTheMaxAgeGetsTheLinkPrompt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)
	deps.EvidenceMaxAge = testEvidenceMaxAge

	tests := []struct {
		name     string
		provider sqlcgen.IdentityProvider
		age      time.Duration
		wantLink bool
	}{
		{"slack, evidence inside the limit", sqlcgen.IdentityProviderSlack, testEvidenceMaxAge / 2, true},
		{"slack, evidence outside the limit", sqlcgen.IdentityProviderSlack, 2 * testEvidenceMaxAge, false},
		{"linear, evidence outside the limit", sqlcgen.IdentityProviderLinear, 2 * testEvidenceMaxAge, false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			email := fmt.Sprintf("aged-evidence-%02d@example.com", i)
			externalID := fmt.Sprintf("X-AGED-%02d", i)
			user := createFixtureUser(ctx, t, deps.Users, email)
			evidence := addCurrentEvidence(ctx, t, deps, user, email)
			stampAgo(ctx, t, pool, evidence.ID, tt.age)

			res, err := identitylink.Resolve(ctx, deps, tt.provider, externalID, identitylink.ProfileEmail{Address: email, Verified: true}, true)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			row, rowErr := deps.Identities.GetByProviderAndExternalID(ctx, tt.provider, externalID)
			if tt.wantLink {
				if !res.AutoLinked || res.UserID != user.ID || rowErr != nil || row.UserID != user.ID {
					t.Errorf("Resolve = %+v (row %+v, %v), want an automatic link to %v", res, row, rowErr, user.ID)
				}
				return
			}
			if res.UserID.Valid || res.AutoLinked || !strings.HasPrefix(res.MagicLinkURL, deps.PublicBaseURL+identitylink.MagicLinkPath) {
				t.Errorf("Resolve = %+v, want no link and the link prompt", res)
			}
			if !errors.Is(rowErr, pgx.ErrNoRows) {
				t.Errorf("identity = %+v (%v), want none stored", row, rowErr)
			}
		})
	}
}

// signInRow seeds the identity a sign-in authenticates: email (nil for
// none), verified as given, stamped twice the test limit ago when stamped,
// with token stored as its access token.
type signInRow struct {
	provider sqlcgen.IdentityProvider
	via      sqlcgen.IdentityLinkedVia
	email    *string
	verified bool
	stamped  bool
	token    []byte
}

func seedSignInRow(ctx context.Context, t *testing.T, deps identitylink.Deps, pool *pgxpool.Pool, user sqlcgen.User, externalID string, r signInRow) sqlcgen.Identity {
	t.Helper()
	row := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: r.provider, ExternalID: externalID,
		Email: r.email, EmailVerified: r.verified, LinkedVia: r.via, AccessTokenEncrypted: r.token,
	})
	if r.stamped {
		stampAgo(ctx, t, pool, row.ID, 2*testEvidenceMaxAge)
	}
	return row
}

func strPtr(s string) *string { return &s }

// TestRefreshSignInEvidence_RecordsWhatTheProviderPresents proves a sign-in
// writes the verified email its provider presents onto the identity it
// authenticated, stamped now -- so it counts as evidence again -- and its
// fresh token when it brings one. A sign-in that brings a new email, or
// verifies one the identity held unverified or not at all, is audited as
// identity.email_updated, naming the user as actor, and counted as
// result=updated; one that confirms the email already held, a change of
// case included, writes no entry and is counted as result=confirmed.
func TestRefreshSignInEvidence_RecordsWhatTheProviderPresents(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)
	deps.EvidenceMaxAge = testEvidenceMaxAge

	tests := []struct {
		name      string
		seed      func(i int) signInRow
		presented func(i int) string
		token     []byte
		// wantUpdate is whether an identity.email_updated entry is written;
		// wantPrevious its previous_email (nil for none).
		wantUpdate   bool
		wantPrevious func(i int) any
		wantPrevVer  bool
		wantToken    []byte
	}{
		{
			name: "the same address, confirmed again",
			seed: func(i int) signInRow {
				return signInRow{provider: sqlcgen.IdentityProviderGithub, via: sqlcgen.IdentityLinkedViaAdmin, email: strPtr(fmt.Sprintf("same-%02d@example.com", i)), verified: true, stamped: true, token: []byte("old-token")}
			},
			presented: func(i int) string { return fmt.Sprintf("same-%02d@example.com", i) },
			token:     []byte("new-token"),
			wantToken: []byte("new-token"),
		},
		{
			name: "the same address in another case",
			seed: func(i int) signInRow {
				return signInRow{provider: sqlcgen.IdentityProviderOidc, via: sqlcgen.IdentityLinkedViaAdmin, email: strPtr(fmt.Sprintf("Case-Only-%02d@Example.com", i)), verified: true, stamped: true}
			},
			presented: func(i int) string { return fmt.Sprintf("case-only-%02d@example.com", i) },
		},
		{
			name: "a changed address",
			seed: func(i int) signInRow {
				return signInRow{provider: sqlcgen.IdentityProviderGithub, via: sqlcgen.IdentityLinkedViaAdmin, email: strPtr(fmt.Sprintf("before-%02d@example.com", i)), verified: true, stamped: true, token: []byte("old-token")}
			},
			presented:    func(i int) string { return fmt.Sprintf("after-%02d@example.com", i) },
			token:        []byte("new-token"),
			wantUpdate:   true,
			wantPrevious: func(i int) any { return fmt.Sprintf("before-%02d@example.com", i) },
			wantPrevVer:  true,
			wantToken:    []byte("new-token"),
		},
		{
			name: "no email before (an admin's link)",
			seed: func(int) signInRow {
				return signInRow{provider: sqlcgen.IdentityProviderGithub, via: sqlcgen.IdentityLinkedViaAdmin, token: []byte("old-token")}
			},
			presented:    func(i int) string { return fmt.Sprintf("first-seen-%02d@example.com", i) },
			wantUpdate:   true,
			wantPrevious: func(int) any { return nil },
			wantToken:    []byte("old-token"),
		},
		{
			name: "a seeded, unverified email",
			seed: func(i int) signInRow {
				return signInRow{provider: sqlcgen.IdentityProviderGithub, via: sqlcgen.IdentityLinkedViaAdmin, email: strPtr(fmt.Sprintf("seeded-%02d@example.com", i))}
			},
			presented:    func(i int) string { return fmt.Sprintf("seeded-%02d@example.com", i) },
			wantUpdate:   true,
			wantPrevious: func(i int) any { return fmt.Sprintf("seeded-%02d@example.com", i) },
		},
		{
			name: "a sign-in with no token keeps the stored one",
			seed: func(i int) signInRow {
				return signInRow{provider: sqlcgen.IdentityProviderOidc, via: sqlcgen.IdentityLinkedViaAutoEmail, email: strPtr(fmt.Sprintf("keep-token-%02d@example.com", i)), verified: true, stamped: true, token: []byte("kept-token")}
			},
			presented: func(i int) string { return fmt.Sprintf("keep-token-%02d@example.com", i) },
			wantToken: []byte("kept-token"),
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primary := fmt.Sprintf("record-primary-%02d@example.com", i)
			user := createFixtureUser(ctx, t, deps.Users, primary)
			seed := tt.seed(i)
			before := seedSignInRow(ctx, t, deps, pool, user, fmt.Sprintf("X-RECORD-%02d", i), seed)
			presented := tt.presented(i)
			if seed.email != nil && seed.verified && seed.stamped {
				if got := matchedUserIDs(ctx, t, deps, *seed.email); len(got) != 0 {
					t.Fatalf("MatchUserIDs before the sign-in = %v, want none: the fixture's evidence must have aged out", got)
				}
			}

			result := "confirmed"
			if tt.wantUpdate {
				result = "updated"
			}
			countBefore := readEvidenceRefreshCount(ctx, t, before.Provider, result)

			got, err := identitylink.RefreshSignInEvidence(ctx, deps, before.ID, identitylink.ProfileEmail{Address: presented, Verified: true}, tt.token)
			if err != nil {
				t.Fatalf("RefreshSignInEvidence: %v", err)
			}
			if delta := readEvidenceRefreshCount(ctx, t, before.Provider, result) - countBefore; delta != 1 {
				t.Errorf("identity_email_evidence_refreshes_total{provider=%s,result=%s} grew by %d, want 1", before.Provider, result, delta)
			}

			row, err := deps.Identities.GetByProviderAndExternalID(ctx, before.Provider, before.ExternalID)
			if err != nil {
				t.Fatalf("GetByProviderAndExternalID: %v", err)
			}
			if got.ID != row.ID || got.UserID != user.ID || row.UserID != user.ID {
				t.Errorf("returned %v/%v, stored user %v, want the identity %v of %v", got.ID, got.UserID, row.UserID, before.ID, user.ID)
			}
			if row.Email == nil || *row.Email != presented || !row.EmailVerified {
				t.Errorf("stored email = %v (verified %v), want %q verified", row.Email, row.EmailVerified, presented)
			}
			if !stampedJustNow(ctx, t, pool, row.ID) {
				t.Errorf("email_checked_at = %+v, want just now", row.EmailCheckedAt)
			}
			if string(row.AccessTokenEncrypted) != string(tt.wantToken) {
				t.Errorf("access token = %q, want %q", row.AccessTokenEncrypted, tt.wantToken)
			}
			if matched := matchedUserIDs(ctx, t, deps, presented); len(matched) != 1 || matched[0] != user.ID.String() {
				t.Errorf("MatchUserIDs(%q) after the sign-in = %v, want [%v]: the presented email is evidence again", presented, matched, user.ID)
			}
			if seed.email != nil && !strings.EqualFold(*seed.email, presented) {
				if matched := matchedUserIDs(ctx, t, deps, *seed.email); len(matched) != 0 {
					t.Errorf("MatchUserIDs(%q) after the sign-in = %v, want none: the identity no longer carries it", *seed.email, matched)
				}
			}

			entries := auditEntriesFor(ctx, t, deps, "identity.email_updated", before.ID.String())
			if !tt.wantUpdate {
				if len(entries) != 0 {
					t.Errorf("identity.email_updated entries = %d, want 0: the provider confirmed the email already held", len(entries))
				}
			} else {
				if len(entries) != 1 {
					t.Fatalf("identity.email_updated entries = %d, want 1", len(entries))
				}
				if entries[0].ActorUserID != user.ID {
					t.Errorf("actor = %v, want the signed-in user %v", entries[0].ActorUserID, user.ID)
				}
				detail := auditDetail(t, entries[0])
				want := map[string]any{
					"provider": string(before.Provider), "external_id": before.ExternalID, "user_id": user.ID.String(),
					"previous_email": tt.wantPrevious(i), "previous_verified": tt.wantPrevVer, "email": presented,
				}
				for k, v := range want {
					if detail[k] != v {
						t.Errorf("detail[%q] = %v, want %v (detail %v)", k, detail[k], v, detail)
					}
				}
			}

			reread, err := deps.Users.GetByID(ctx, user.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if reread.PrimaryEmail != primary {
				t.Errorf("primary email = %q, want %q: it came from no identity of this case", reread.PrimaryEmail, primary)
			}
		})
	}
}

// TestRefreshSignInEvidence_NeverChangesTheLink proves a sign-in records
// the email and nothing else about the link: the identity keeps its user
// and how it was linked -- an admin's link, a prompt's, an automatic one --
// even when the email it presents is another member's primary email and
// another member's current evidence.
func TestRefreshSignInEvidence_NeverChangesTheLink(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	for i, via := range []sqlcgen.IdentityLinkedVia{sqlcgen.IdentityLinkedViaAdmin, sqlcgen.IdentityLinkedViaPrompt, sqlcgen.IdentityLinkedViaAutoEmail} {
		t.Run(string(via), func(t *testing.T) {
			owner := createFixtureUser(ctx, t, deps.Users, fmt.Sprintf("link-owner-%02d@example.com", i))
			presented := fmt.Sprintf("link-other-%02d@example.com", i)
			other := createFixtureUserWithEvidence(ctx, t, deps, presented)
			row := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
				UserID: owner.ID, Provider: sqlcgen.IdentityProviderOidc, ExternalID: fmt.Sprintf("issuer|link-%02d", i),
				Email: strPtr(fmt.Sprintf("link-before-%02d@example.com", i)), EmailVerified: true, LinkedVia: via,
			})

			got, err := identitylink.RefreshSignInEvidence(ctx, deps, row.ID, identitylink.ProfileEmail{Address: presented, Verified: true}, nil)
			if err != nil {
				t.Fatalf("RefreshSignInEvidence: %v", err)
			}
			stored, err := deps.Identities.GetByProviderAndExternalID(ctx, row.Provider, row.ExternalID)
			if err != nil {
				t.Fatalf("GetByProviderAndExternalID: %v", err)
			}
			for _, r := range []sqlcgen.Identity{got, stored} {
				if r.ID != row.ID || r.UserID != owner.ID || r.LinkedVia != via {
					t.Errorf("identity = %v of %v via %s, want %v of %v via %s", r.ID, r.UserID, r.LinkedVia, row.ID, owner.ID, via)
				}
			}
			if stored.UserID == other.ID {
				t.Errorf("the identity moved to %v, who holds the presented email", other.ID)
			}
		})
	}
}

// TestRefreshSignInEvidence_PrimaryEmailFollows proves a user's primary
// email follows the sign-in identity it came from to the email that
// identity's provider now presents, audited as user.primary_email_changed
// in the same transaction -- and stays put when it came from elsewhere, or
// when another member already holds the new address, whatever its case.
func TestRefreshSignInEvidence_PrimaryEmailFollows(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	tests := []struct {
		name string
		// primary is the user's primary email, identity the identity's
		// email, before the sign-in; held, if set, another member's.
		primary, identity, presented, held string
		wantMoved                          bool
	}{
		{name: "the primary email came from this identity", primary: "follow-a-old@example.com", identity: "follow-a-old@example.com", presented: "follow-a-new@example.com", wantMoved: true},
		{name: "the primary email came from this identity, in another case", primary: "Follow-B-Old@Example.com", identity: "follow-b-old@example.com", presented: "follow-b-new@example.com", wantMoved: true},
		{name: "the primary email came from another identity", primary: "follow-c-elsewhere@example.com", identity: "follow-c-old@example.com", presented: "follow-c-new@example.com"},
		{name: "another member holds the new address, in another case", primary: "follow-d-old@example.com", identity: "follow-d-old@example.com", presented: "follow-d-taken@example.com", held: "FOLLOW-D-TAKEN@example.com"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := createFixtureUser(ctx, t, deps.Users, tt.primary)
			if tt.held != "" {
				createFixtureUser(ctx, t, deps.Users, tt.held)
			}
			row := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
				UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: fmt.Sprintf("X-FOLLOW-%02d", i),
				Email: strPtr(tt.identity), EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
			})

			if _, err := identitylink.RefreshSignInEvidence(ctx, deps, row.ID, identitylink.ProfileEmail{Address: tt.presented, Verified: true}, nil); err != nil {
				t.Fatalf("RefreshSignInEvidence: %v", err)
			}

			reread, err := deps.Users.GetByID(ctx, user.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			changes := auditEntriesFor(ctx, t, deps, "user.primary_email_changed", user.ID.String())
			if !tt.wantMoved {
				if reread.PrimaryEmail != tt.primary || len(changes) != 0 {
					t.Errorf("primary email = %q with %d change entries, want %q unchanged and none", reread.PrimaryEmail, len(changes), tt.primary)
				}
			} else {
				if reread.PrimaryEmail != tt.presented {
					t.Errorf("primary email = %q, want %q", reread.PrimaryEmail, tt.presented)
				}
				if len(changes) != 1 {
					t.Fatalf("user.primary_email_changed entries = %d, want 1", len(changes))
				}
				if changes[0].ActorUserID != user.ID {
					t.Errorf("actor = %v, want %v", changes[0].ActorUserID, user.ID)
				}
				detail := auditDetail(t, changes[0])
				want := map[string]any{"previous_email": tt.identity, "email": tt.presented, "identity_id": row.ID.String(), "provider": "github"}
				for k, v := range want {
					if detail[k] != v {
						t.Errorf("detail[%q] = %v, want %v (detail %v)", k, detail[k], v, detail)
					}
				}
			}
			// The sign-in itself went through either way.
			if updates := auditEntriesFor(ctx, t, deps, "identity.email_updated", row.ID.String()); len(updates) != 1 {
				t.Errorf("identity.email_updated entries = %d, want 1", len(updates))
			}
		})
	}
}

// failAuditInsertsFor makes every audit_log insert for resourceID fail,
// for the rest of the test.
func failAuditInsertsFor(ctx context.Context, t *testing.T, pool *pgxpool.Pool, resourceID string) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION evidence_test_fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$`,
		`CREATE TRIGGER evidence_test_fail_audit BEFORE INSERT ON audit_log FOR EACH ROW WHEN (NEW.resource_id = '` + resourceID + `') EXECUTE FUNCTION evidence_test_fail_audit()`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("inject the audit failure: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS evidence_test_fail_audit ON audit_log`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS evidence_test_fail_audit()`)
	})
}

// TestRefreshSignInEvidence_AuditFailureRollsBack proves the refresh and
// its audit entries commit together (§13.3): an entry that cannot be
// written -- the identity's, or the primary email's after the identity's
// was written -- fails the refresh and leaves the identity, its stamp, its
// token, the primary email and the audit log exactly as they were.
func TestRefreshSignInEvidence_AuditFailureRollsBack(t *testing.T) {
	for _, tt := range []struct {
		name string
		// failOn picks the resource whose audit entry fails.
		failOn func(user sqlcgen.User, row sqlcgen.Identity) string
	}{
		{"the identity's entry fails", func(_ sqlcgen.User, row sqlcgen.Identity) string { return row.ID.String() }},
		{"the primary email's entry fails", func(user sqlcgen.User, _ sqlcgen.Identity) string { return user.ID.String() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			deps := newDeps(pool)

			user := createFixtureUser(ctx, t, deps.Users, "rollback-old@example.com")
			row := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
				UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: "X-ROLLBACK",
				Email: strPtr("rollback-old@example.com"), EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
				AccessTokenEncrypted: []byte("old-token"),
			})
			failAuditInsertsFor(ctx, t, pool, tt.failOn(user, row))

			if _, err := identitylink.RefreshSignInEvidence(ctx, deps, row.ID, identitylink.ProfileEmail{Address: "rollback-new@example.com", Verified: true}, []byte("new-token")); err == nil {
				t.Fatal("RefreshSignInEvidence = nil, want the audit failure")
			}

			after, err := deps.Identities.GetByProviderAndExternalID(ctx, row.Provider, row.ExternalID)
			if err != nil {
				t.Fatalf("GetByProviderAndExternalID: %v", err)
			}
			if after.Email == nil || *after.Email != "rollback-old@example.com" || after.EmailCheckedAt.Valid || string(after.AccessTokenEncrypted) != "old-token" {
				t.Errorf("identity = %v stamped %v token %q, want the old email, unstamped, the old token", after.Email, after.EmailCheckedAt.Valid, after.AccessTokenEncrypted)
			}
			if reread, err := deps.Users.GetByID(ctx, user.ID); err != nil || reread.PrimaryEmail != "rollback-old@example.com" {
				t.Errorf("primary email = %q (%v), want it unchanged", reread.PrimaryEmail, err)
			}
			for _, e := range []struct{ action, resource string }{
				{"identity.email_updated", row.ID.String()},
				{"user.primary_email_changed", user.ID.String()},
			} {
				if got := auditEntriesFor(ctx, t, deps, e.action, e.resource); len(got) != 0 {
					t.Errorf("%s entries = %d, want 0: nothing was committed", e.action, len(got))
				}
			}
		})
	}
}

// TestRefreshSignInEvidence_FollowCollisionKeepsTheSignIn proves a unique
// violation on the primary email's move -- what a concurrent insert of the
// same address raises -- is absorbed in its savepoint: the sign-in's own
// record commits, and the primary email stays as it was, unaudited.
func TestRefreshSignInEvidence_FollowCollisionKeepsTheSignIn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	user := createFixtureUser(ctx, t, deps.Users, "collision-old@example.com")
	row := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: "X-COLLISION",
		Email: strPtr("collision-old@example.com"), EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	})
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION evidence_test_unique_violation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected collision' USING ERRCODE = 'unique_violation'; END $$`,
		`CREATE TRIGGER evidence_test_unique_violation BEFORE UPDATE ON users FOR EACH ROW WHEN (OLD.id = '` + user.ID.String() + `') EXECUTE FUNCTION evidence_test_unique_violation()`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("inject the collision: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS evidence_test_unique_violation ON users`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS evidence_test_unique_violation()`)
	})

	got, err := identitylink.RefreshSignInEvidence(ctx, deps, row.ID, identitylink.ProfileEmail{Address: "collision-new@example.com", Verified: true}, nil)
	if err != nil {
		t.Fatalf("RefreshSignInEvidence: %v, want the sign-in to go through", err)
	}
	if got.Email == nil || *got.Email != "collision-new@example.com" {
		t.Errorf("returned email = %v, want the presented one", got.Email)
	}
	after, err := deps.Identities.GetByProviderAndExternalID(ctx, row.Provider, row.ExternalID)
	if err != nil || after.Email == nil || *after.Email != "collision-new@example.com" || !stampedJustNow(ctx, t, pool, row.ID) {
		t.Errorf("identity = %+v (%v), want the presented email, stamped", after, err)
	}
	if updates := auditEntriesFor(ctx, t, deps, "identity.email_updated", row.ID.String()); len(updates) != 1 {
		t.Errorf("identity.email_updated entries = %d, want 1", len(updates))
	}
	if reread, err := deps.Users.GetByID(ctx, user.ID); err != nil || reread.PrimaryEmail != "collision-old@example.com" {
		t.Errorf("primary email = %q (%v), want it unchanged", reread.PrimaryEmail, err)
	}
	if changes := auditEntriesFor(ctx, t, deps, "user.primary_email_changed", user.ID.String()); len(changes) != 0 {
		t.Errorf("user.primary_email_changed entries = %d, want 0", len(changes))
	}
}

// TestRefreshSignInEvidence_RowGone proves a sign-in whose identity was
// removed since it was looked up fails, writing nothing: the caller then
// answers 500 and mints no session.
func TestRefreshSignInEvidence_RowGone(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	user := createFixtureUser(ctx, t, deps.Users, "gone@example.com")
	row := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: "X-GONE",
		Email: strPtr("gone@example.com"), EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	})
	if _, err := deps.Identities.Delete(ctx, row.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := identitylink.RefreshSignInEvidence(ctx, deps, row.ID, identitylink.ProfileEmail{Address: "gone-new@example.com", Verified: true}, nil)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("RefreshSignInEvidence = %v, want an error wrapping pgx.ErrNoRows", err)
	}
	if got := auditEntriesFor(ctx, t, deps, "identity.email_updated", row.ID.String()); len(got) != 0 {
		t.Errorf("identity.email_updated entries = %d, want 0", len(got))
	}
	if reread, err := deps.Users.GetByID(ctx, user.ID); err != nil || reread.PrimaryEmail != "gone@example.com" {
		t.Errorf("primary email = %q (%v), want it unchanged", reread.PrimaryEmail, err)
	}
}

// TestRefreshSignInEvidence_RefusesAnUnverifiedEmail proves the refresh
// never records an email no provider vouched for: an unverified or empty
// one is an error, and the identity stays as it was.
func TestRefreshSignInEvidence_RefusesAnUnverifiedEmail(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	user := createFixtureUser(ctx, t, deps.Users, "refuse@example.com")
	row := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: "X-REFUSE",
		Email: strPtr("refuse@example.com"), EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	})
	for _, email := range []identitylink.ProfileEmail{
		{Address: "refuse-new@example.com", Verified: false},
		{Address: "", Verified: true},
	} {
		if _, err := identitylink.RefreshSignInEvidence(ctx, deps, row.ID, email, nil); err == nil {
			t.Errorf("RefreshSignInEvidence(%+v) = nil, want an error", email)
		}
	}
	after, err := deps.Identities.GetByProviderAndExternalID(ctx, row.Provider, row.ExternalID)
	if err != nil || after.Email == nil || *after.Email != "refuse@example.com" || after.EmailCheckedAt.Valid {
		t.Errorf("identity = %+v (%v), want it unchanged", after, err)
	}
}

// TestRefreshSignInEvidence_ConcurrentSignIns proves the row lock: sign-ins
// of one identity record their changes one after the other, each against
// the email the previous one left. Deterministically, a sign-in waiting on
// a concurrent change names that change as its previous email; under load,
// the identity.email_updated entries form one unbroken chain from the
// first email to the one stored, and the primary email follows to the end.
func TestRefreshSignInEvidence_ConcurrentSignIns(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	t.Run("a sign-in waits for a concurrent change", func(t *testing.T) {
		user := createFixtureUser(ctx, t, deps.Users, "wait-old@example.com")
		row := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
			UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: "X-WAIT",
			Email: strPtr("wait-old@example.com"), EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
		})

		lock, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = lock.Rollback(ctx) }()
		if _, err := lock.Exec(ctx, `UPDATE identities SET email = 'wait-between@example.com' WHERE id = $1`, row.ID); err != nil {
			t.Fatalf("concurrent change: %v", err)
		}

		var group errgroup.Group
		group.Go(func() error {
			_, err := identitylink.RefreshSignInEvidence(ctx, deps, row.ID, identitylink.ProfileEmail{Address: "wait-new@example.com", Verified: true}, nil)
			return err
		})
		waitForLockedStatement(ctx, t, pool)
		if err := lock.Commit(ctx); err != nil {
			t.Fatalf("commit the concurrent change: %v", err)
		}
		if err := group.Wait(); err != nil {
			t.Fatalf("RefreshSignInEvidence: %v", err)
		}

		entries := auditEntriesFor(ctx, t, deps, "identity.email_updated", row.ID.String())
		if len(entries) != 1 {
			t.Fatalf("identity.email_updated entries = %d, want 1", len(entries))
		}
		if detail := auditDetail(t, entries[0]); detail["previous_email"] != "wait-between@example.com" {
			t.Errorf("previous_email = %v, want the concurrent change's wait-between@example.com", detail["previous_email"])
		}
	})

	t.Run("many sign-ins at once", func(t *testing.T) {
		const (
			rounds  = 5
			callers = 6
		)
		for round := range rounds {
			first := fmt.Sprintf("chain-%02d-start@example.com", round)
			user := createFixtureUser(ctx, t, deps.Users, first)
			row := seedIdentity(ctx, t, deps, sqlcgen.CreateIdentityParams{
				UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: fmt.Sprintf("X-CHAIN-%02d", round),
				Email: &first, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
			})

			var group errgroup.Group
			for i := range callers {
				group.Go(func() error {
					email := fmt.Sprintf("chain-%02d-%d@example.com", round, i)
					_, err := identitylink.RefreshSignInEvidence(ctx, deps, row.ID, identitylink.ProfileEmail{Address: email, Verified: true}, nil)
					return err
				})
			}
			if err := group.Wait(); err != nil {
				t.Fatalf("round %d: RefreshSignInEvidence: %v", round, err)
			}

			stored, err := deps.Identities.GetByProviderAndExternalID(ctx, row.Provider, row.ExternalID)
			if err != nil {
				t.Fatalf("round %d: GetByProviderAndExternalID: %v", round, err)
			}
			reread, err := deps.Users.GetByID(ctx, user.ID)
			if err != nil {
				t.Fatalf("round %d: GetByID: %v", round, err)
			}
			if reread.PrimaryEmail != *stored.Email {
				t.Errorf("round %d: primary email = %q, want it to follow to %q", round, reread.PrimaryEmail, *stored.Email)
			}
			for _, chain := range []struct{ action, resource string }{
				{"identity.email_updated", row.ID.String()},
				{"user.primary_email_changed", user.ID.String()},
			} {
				entries := auditEntriesFor(ctx, t, deps, chain.action, chain.resource)
				next := make(map[any]any, len(entries))
				for _, e := range entries {
					d := auditDetail(t, e)
					if _, dup := next[d["previous_email"]]; dup {
						t.Errorf("round %d: two %s entries both replace %v", round, chain.action, d["previous_email"])
					}
					next[d["previous_email"]] = d["email"]
				}
				at, steps := any(first), 0
				for steps < len(entries) {
					to, ok := next[at]
					if !ok {
						break
					}
					at, steps = to, steps+1
				}
				if len(entries) != callers || steps != callers || at != *stored.Email {
					t.Errorf("round %d: %d %s entries chain %d steps from %s to %v, want %d steps to %s", round, len(entries), chain.action, steps, first, at, callers, *stored.Email)
				}
			}
		}
	})
}
