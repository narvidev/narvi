//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// mcpOAuthFixture seeds one user and one pre-registered client -- the
// minimum every MCP authorization-server store test starts from.
type mcpOAuthFixture struct {
	pool    *pgxpool.Pool
	users   *narvipg.UserStore
	clients *narvipg.MCPOAuthClientStore
	grants  *narvipg.MCPOAuthGrantStore
	user    sqlcgen.User
	client  sqlcgen.McpOauthClient
}

func newMCPOAuthFixture(ctx context.Context, t *testing.T) mcpOAuthFixture {
	t.Helper()
	pool := newTestPool(t)
	f := mcpOAuthFixture{
		pool:    pool,
		users:   narvipg.NewUserStore(pool),
		clients: narvipg.NewMCPOAuthClientStore(pool),
		grants:  narvipg.NewMCPOAuthGrantStore(pool),
	}
	var err error
	f.user, err = f.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "mcp-oauth-store@example.com",
		DisplayName:  "MCP OAuth Store",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	f.client, err = f.clients.Create(ctx, sqlcgen.CreateMCPOAuthClientParams{
		ClientID:     "narvi_mcp_c_store_test",
		Kind:         sqlcgen.McpOauthClientKindPreregistered,
		ClientName:   "Store Test Client",
		RedirectUris: []string{"http://127.0.0.1/callback"},
		CreatedBy:    f.user.ID,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	return f
}

func mcpTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// createGrantWithToken inserts a grant plus one access token under it,
// returning both.
func (f mcpOAuthFixture) createGrantWithToken(ctx context.Context, t *testing.T, scopes []string, tokenHash string, grantExpires, tokenExpires time.Time) (sqlcgen.McpOauthGrant, sqlcgen.McpOauthAccessToken) {
	t.Helper()
	g, err := f.grants.UpsertGrant(ctx, sqlcgen.UpsertMCPOAuthGrantParams{
		UserID:    f.user.ID,
		ClientID:  f.client.ID,
		Scopes:    scopes,
		Resource:  "http://127.0.0.1:9/mcp",
		ExpiresAt: mcpTS(grantExpires),
	})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	tok, err := f.grants.CreateAccessToken(ctx, sqlcgen.CreateMCPOAuthAccessTokenParams{
		GrantID:   g.ID,
		TokenHash: tokenHash,
		Scopes:    scopes,
		ExpiresAt: mcpTS(tokenExpires),
	})
	if err != nil {
		t.Fatalf("create access token: %v", err)
	}
	return g, tok
}

// TestMCPOAuthGrantStore_EmptyScopesStoredAsEmptyArray pins nonNilScopes:
// a scope-less grant (technical plan §43.17) is a first-class outcome, so
// a nil scope slice must land as '{}' -- never a NOT NULL violation, never
// read back as nil-vs-empty ambiguity the bearer check would have to guess
// about.
func TestMCPOAuthGrantStore_EmptyScopesStoredAsEmptyArray(t *testing.T) {
	ctx := context.Background()
	f := newMCPOAuthFixture(ctx, t)

	g, _ := f.createGrantWithToken(ctx, t, nil, "hash-empty-scopes", time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if g.Scopes == nil || len(g.Scopes) != 0 {
		t.Fatalf("grant scopes = %#v, want a non-nil empty slice", g.Scopes)
	}
	p, err := f.grants.LookupAccessToken(ctx, "hash-empty-scopes")
	if err != nil {
		t.Fatalf("LookupAccessToken: %v", err)
	}
	if p.TokenScopes == nil || len(p.TokenScopes) != 0 {
		t.Fatalf("principal scopes = %#v, want a non-nil empty slice", p.TokenScopes)
	}

	req, err := f.grants.CreateAuthorizationRequest(ctx, sqlcgen.CreateMCPOAuthAuthorizationRequestParams{
		ClientID:            f.client.ID,
		RedirectUri:         "http://127.0.0.1/callback",
		Scopes:              nil,
		CodeChallenge:       "challenge",
		CodeChallengeMethod: "S256",
		Resource:            "http://127.0.0.1:9/mcp",
		ExpiresAt:           mcpTS(time.Now().Add(time.Minute)),
	})
	if err != nil {
		t.Fatalf("CreateAuthorizationRequest with nil scopes: %v", err)
	}
	if req.Scopes == nil || len(req.Scopes) != 0 {
		t.Fatalf("request scopes = %#v, want a non-nil empty slice", req.Scopes)
	}
}

// TestMCPOAuthGrantStore_AuthorizationRequestBindAndConsume proves the
// consent page's two atomic steps: the first render binds the request to
// its user and every other user is refused afterwards; the decision
// consumes it exactly once, only for the bound user, only after a nonce
// was minted, and only before expiry.
func TestMCPOAuthGrantStore_AuthorizationRequestBindAndConsume(t *testing.T) {
	ctx := context.Background()
	f := newMCPOAuthFixture(ctx, t)
	other, err := f.users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "other@example.com", DisplayName: "Other", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}

	newReq := func(expires time.Time) sqlcgen.McpOauthAuthorizationRequest {
		t.Helper()
		r, err := f.grants.CreateAuthorizationRequest(ctx, sqlcgen.CreateMCPOAuthAuthorizationRequestParams{
			ClientID:            f.client.ID,
			RedirectUri:         "http://127.0.0.1/callback",
			Scopes:              []string{"mcp:read"},
			CodeChallenge:       "challenge",
			CodeChallengeMethod: "S256",
			Resource:            "http://127.0.0.1:9/mcp",
			ExpiresAt:           mcpTS(expires),
		})
		if err != nil {
			t.Fatalf("CreateAuthorizationRequest: %v", err)
		}
		return r
	}

	req := newReq(time.Now().Add(time.Minute))

	// A request nobody has rendered yet cannot be consumed: no nonce exists.
	if _, err := f.grants.ConsumeAuthorizationRequest(ctx, req.ID, f.user.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("consume before any render: err = %v, want pgx.ErrNoRows", err)
	}

	bound, err := f.grants.BindAuthorizationRequest(ctx, req.ID, f.user.ID, "nonce-hash-1")
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if bound.UserID != f.user.ID || bound.CsrfNonceHash == nil || *bound.CsrfNonceHash != "nonce-hash-1" {
		t.Fatalf("bound request = %+v, want user %v and nonce hash nonce-hash-1", bound, f.user.ID)
	}
	if _, err := f.grants.BindAuthorizationRequest(ctx, req.ID, other.ID, "nonce-hash-other"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("bind by another user: err = %v, want pgx.ErrNoRows", err)
	}
	rebound, err := f.grants.BindAuthorizationRequest(ctx, req.ID, f.user.ID, "nonce-hash-2")
	if err != nil || rebound.CsrfNonceHash == nil || *rebound.CsrfNonceHash != "nonce-hash-2" {
		t.Fatalf("re-render by the bound user: row = %+v, err = %v, want nonce rotated to nonce-hash-2", rebound, err)
	}

	if _, err := f.grants.ConsumeAuthorizationRequest(ctx, req.ID, other.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("consume by another user: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := f.grants.ConsumeAuthorizationRequest(ctx, req.ID, f.user.ID); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := f.grants.ConsumeAuthorizationRequest(ctx, req.ID, f.user.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second consume: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := f.grants.BindAuthorizationRequest(ctx, req.ID, f.user.ID, "nonce-hash-3"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("bind after consume: err = %v, want pgx.ErrNoRows", err)
	}

	expired := newReq(time.Now().Add(-time.Second))
	if _, err := f.grants.BindAuthorizationRequest(ctx, expired.ID, f.user.ID, "nonce-hash-x"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("bind an expired request: err = %v, want pgx.ErrNoRows", err)
	}
}

// TestMCPOAuthGrantStore_AuthorizationCodeSingleUse proves a code can be
// consumed exactly once -- also under a concurrent race -- and that a
// consumed code stays findable by hash so a replay is recognisable.
func TestMCPOAuthGrantStore_AuthorizationCodeSingleUse(t *testing.T) {
	ctx := context.Background()
	f := newMCPOAuthFixture(ctx, t)
	g, _ := f.createGrantWithToken(ctx, t, []string{"mcp:read"}, "hash-code-test", time.Now().Add(time.Hour), time.Now().Add(time.Hour))

	if _, err := f.grants.CreateAuthorizationCode(ctx, sqlcgen.CreateMCPOAuthAuthorizationCodeParams{
		GrantID:       g.ID,
		CodeHash:      "code-hash-1",
		CodeChallenge: "challenge",
		RedirectUri:   "http://127.0.0.1/callback",
		Resource:      "http://127.0.0.1:9/mcp",
		ExpiresAt:     mcpTS(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatalf("CreateAuthorizationCode: %v", err)
	}

	const racers = 8
	wins := make(chan struct{}, racers)
	var eg errgroup.Group
	for i := 0; i < racers; i++ {
		eg.Go(func() error {
			_, err := f.grants.ConsumeAuthorizationCode(ctx, "code-hash-1")
			switch {
			case err == nil:
				wins <- struct{}{}
				return nil
			case errors.Is(err, pgx.ErrNoRows):
				return nil
			default:
				return err
			}
		})
	}
	if err := eg.Wait(); err != nil {
		t.Fatalf("concurrent consume: %v", err)
	}
	close(wins)
	if n := len(wins); n != 1 {
		t.Fatalf("concurrent consumers that won = %d, want exactly 1", n)
	}

	row, err := f.grants.GetAuthorizationCodeByHash(ctx, "code-hash-1")
	if err != nil {
		t.Fatalf("GetAuthorizationCodeByHash after consume: %v", err)
	}
	if !row.ConsumedAt.Valid {
		t.Fatalf("consumed code row has consumed_at unset")
	}
	if _, err := f.grants.GetAuthorizationCodeByHash(ctx, "never-issued"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetAuthorizationCodeByHash(never-issued): err = %v, want pgx.ErrNoRows", err)
	}
}

// TestMCPOAuthGrantStore_LookupReadsLiveState proves LookupAccessToken is
// a live read of every fact that can revoke a call: a disabled user, a
// disabled client and a changed role all show on the very next lookup,
// and deleting the grant (revocation) or the client makes the token
// unfindable.
func TestMCPOAuthGrantStore_LookupReadsLiveState(t *testing.T) {
	ctx := context.Background()
	f := newMCPOAuthFixture(ctx, t)
	g, _ := f.createGrantWithToken(ctx, t, []string{"mcp:read"}, "hash-live", time.Now().Add(time.Hour), time.Now().Add(time.Hour))

	p, err := f.grants.LookupAccessToken(ctx, "hash-live")
	if err != nil {
		t.Fatalf("LookupAccessToken: %v", err)
	}
	if p.GrantID != g.ID || p.UserID != f.user.ID || p.UserRole != "member" || p.UserEmail != f.user.PrimaryEmail ||
		p.ClientID != f.client.ClientID || p.ClientDisabled || p.UserDisabled || p.GrantResource != "http://127.0.0.1:9/mcp" ||
		!p.GrantLastUsedAt.IsZero() {
		t.Fatalf("principal = %+v, want the seeded grant/user/client, nothing disabled, never used", p)
	}

	if _, err := f.pool.Exec(ctx, "UPDATE users SET role = 'viewer', disabled = true WHERE id = $1", f.user.ID); err != nil {
		t.Fatalf("update user: %v", err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE mcp_oauth_clients SET disabled_at = now() WHERE id = $1", f.client.ID); err != nil {
		t.Fatalf("disable client: %v", err)
	}
	p, err = f.grants.LookupAccessToken(ctx, "hash-live")
	if err != nil {
		t.Fatalf("LookupAccessToken after updates: %v", err)
	}
	if p.UserRole != "viewer" || !p.UserDisabled || !p.ClientDisabled {
		t.Fatalf("principal after updates = %+v, want role viewer, user disabled, client disabled", p)
	}

	if err := f.grants.TouchGrantLastUsed(ctx, g.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("TouchGrantLastUsed: %v", err)
	}
	p, err = f.grants.LookupAccessToken(ctx, "hash-live")
	if err != nil || p.GrantLastUsedAt.IsZero() {
		t.Fatalf("after touch: principal = %+v, err = %v, want last_used_at stamped", p, err)
	}
	stamped := p.GrantLastUsedAt
	// A second touch whose stale_before predates the stamp is a no-op.
	if err := f.grants.TouchGrantLastUsed(ctx, g.ID, stamped.Add(-time.Hour)); err != nil {
		t.Fatalf("second TouchGrantLastUsed: %v", err)
	}
	p, err = f.grants.LookupAccessToken(ctx, "hash-live")
	if err != nil || !p.GrantLastUsedAt.Equal(stamped) {
		t.Fatalf("after coalesced touch: last_used_at = %v (err %v), want unchanged %v", p.GrantLastUsedAt, err, stamped)
	}

	n, err := f.grants.DeleteGrant(ctx, g.ID)
	if err != nil || n != 1 {
		t.Fatalf("DeleteGrant: n = %d, err = %v, want 1 row", n, err)
	}
	if _, err := f.grants.LookupAccessToken(ctx, "hash-live"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lookup after revocation: err = %v, want pgx.ErrNoRows (the token must cascade with its grant)", err)
	}
}

// TestMCPOAuthClientStore_DeleteCascadesGrants proves deleting a client
// takes every grant and token issued to it with it -- the structural half
// of "a deleted client's users lose access on their next call".
func TestMCPOAuthClientStore_DeleteCascadesGrants(t *testing.T) {
	ctx := context.Background()
	f := newMCPOAuthFixture(ctx, t)
	g, _ := f.createGrantWithToken(ctx, t, []string{"mcp:read"}, "hash-cascade", time.Now().Add(time.Hour), time.Now().Add(time.Hour))

	listed, err := f.grants.ListGrantsForClient(ctx, f.client.ID)
	if err != nil || len(listed) != 1 || listed[0].ID != g.ID || listed[0].UserID != f.user.ID {
		t.Fatalf("ListGrantsForClient = %+v, err = %v, want the one seeded grant", listed, err)
	}

	deleted, err := f.clients.Delete(ctx, f.client.ID)
	if err != nil || deleted.ID != f.client.ID {
		t.Fatalf("Delete client: row = %+v, err = %v", deleted, err)
	}
	if _, err := f.grants.GetGrant(ctx, g.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("grant after client delete: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := f.grants.LookupAccessToken(ctx, "hash-cascade"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("token after client delete: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := f.clients.Delete(ctx, f.client.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second client delete: err = %v, want pgx.ErrNoRows", err)
	}
}

// TestMCPOAuthGrantStore_DeleteGrantForUserIsOwnOnly proves a user can
// only revoke their own grant: another user's id is indistinguishable
// from a missing one, and the grant survives.
func TestMCPOAuthGrantStore_DeleteGrantForUserIsOwnOnly(t *testing.T) {
	ctx := context.Background()
	f := newMCPOAuthFixture(ctx, t)
	other, err := f.users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "other2@example.com", DisplayName: "Other", Role: sqlcgen.UserRoleAdmin})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	g, _ := f.createGrantWithToken(ctx, t, []string{"mcp:read"}, "hash-own", time.Now().Add(time.Hour), time.Now().Add(time.Hour))

	if _, err := f.grants.DeleteGrantForUser(ctx, g.ID, other.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("delete by another user: err = %v, want pgx.ErrNoRows", err)
	}
	listed, err := f.grants.ListGrantsForUser(ctx, f.user.ID)
	if err != nil || len(listed) != 1 || listed[0].ID != g.ID || listed[0].ClientName != f.client.ClientName ||
		listed[0].ClientPublicID != f.client.ClientID || listed[0].ClientKind != sqlcgen.McpOauthClientKindPreregistered {
		t.Fatalf("ListGrantsForUser = %+v, err = %v, want the one grant with its client facts", listed, err)
	}
	if _, err := f.grants.DeleteGrantForUser(ctx, g.ID, f.user.ID); err != nil {
		t.Fatalf("delete by owner: %v", err)
	}
	listed, err = f.grants.ListGrantsForUser(ctx, f.user.ID)
	if err != nil || len(listed) != 0 {
		t.Fatalf("ListGrantsForUser after revoke = %+v, err = %v, want none", listed, err)
	}
}

// TestMCPOAuthGrantStore_UpsertKeepsOneGrantPerUserAndClient proves
// consenting again to the same client updates the one grant in place --
// same id, renewed expiry, original created_at, the latest approval's
// scopes recorded for display -- so the table cannot grow one row per
// consent; and that a token issued under the first consent keeps exactly
// the scopes it was issued with (technical plan §43.16): the bearer
// lookup reads the token's scopes, never the grant's.
func TestMCPOAuthGrantStore_UpsertKeepsOneGrantPerUserAndClient(t *testing.T) {
	ctx := context.Background()
	f := newMCPOAuthFixture(ctx, t)
	first, _ := f.createGrantWithToken(ctx, t, []string{"mcp:read"}, "hash-upsert", time.Now().Add(time.Hour), time.Now().Add(time.Hour))

	later := time.Now().Add(48 * time.Hour).Truncate(time.Microsecond)
	second, err := f.grants.UpsertGrant(ctx, sqlcgen.UpsertMCPOAuthGrantParams{
		UserID:    f.user.ID,
		ClientID:  f.client.ID,
		Scopes:    nil,
		Resource:  "http://127.0.0.1:9/mcp",
		ExpiresAt: mcpTS(later),
	})
	if err != nil {
		t.Fatalf("second UpsertGrant: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second consent id = %v, want the same grant %v", second.ID, first.ID)
	}
	if len(second.Scopes) != 0 || !second.ExpiresAt.Time.Equal(later) || !second.CreatedAt.Time.Equal(first.CreatedAt.Time) {
		t.Fatalf("second consent = %+v, want scopes {}, expiry %v, created_at unchanged %v", second, later, first.CreatedAt.Time)
	}
	p, err := f.grants.LookupAccessToken(ctx, "hash-upsert")
	if err != nil || len(p.TokenScopes) != 1 || p.TokenScopes[0] != "mcp:read" {
		t.Fatalf("first consent's token after re-consent: principal = %+v, err = %v, want it alive and still holding exactly [mcp:read]", p, err)
	}
	listed, err := f.grants.ListGrantsForUser(ctx, f.user.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListGrantsForUser = %+v, err = %v, want exactly one grant", listed, err)
	}
}

// createRefreshToken inserts one refresh token under grantID.
func (f mcpOAuthFixture) createRefreshToken(ctx context.Context, t *testing.T, grantID pgtype.UUID, tokenHash string, scopes []string, expires time.Time) sqlcgen.McpOauthRefreshToken {
	t.Helper()
	rt, err := f.grants.CreateRefreshToken(ctx, sqlcgen.CreateMCPOAuthRefreshTokenParams{
		GrantID:   grantID,
		TokenHash: tokenHash,
		Scopes:    scopes,
		ExpiresAt: mcpTS(expires),
	})
	if err != nil {
		t.Fatalf("create refresh token %s: %v", tokenHash, err)
	}
	return rt
}

// TestMCPOAuthGrantStore_RefreshTokenRotatesOnce proves the refresh
// grant's single-use step (technical plan §43.16): a refresh token is
// rotated at most once -- also under a concurrent race -- naming its
// successor, and a rotated token stays findable by hash so presenting it
// again is recognisable as a replay. Scopes are stored as issued, a nil
// set as '{}'.
func TestMCPOAuthGrantStore_RefreshTokenRotatesOnce(t *testing.T) {
	ctx := context.Background()
	f := newMCPOAuthFixture(ctx, t)
	g, _ := f.createGrantWithToken(ctx, t, []string{"mcp:read"}, "hash-refresh-test", time.Now().Add(time.Hour), time.Now().Add(time.Hour))

	first := f.createRefreshToken(ctx, t, g.ID, "refresh-hash-1", nil, time.Now().Add(time.Hour))
	if first.Scopes == nil || len(first.Scopes) != 0 || first.RotatedAt.Valid || first.SupersededBy.Valid {
		t.Fatalf("new refresh token = %+v, want non-nil empty scopes and not rotated", first)
	}

	const racers = 8
	successors := make([]sqlcgen.McpOauthRefreshToken, racers)
	for i := range successors {
		successors[i] = f.createRefreshToken(ctx, t, g.ID, fmt.Sprintf("refresh-successor-%d", i), []string{"mcp:read"}, time.Now().Add(time.Hour))
	}
	wins := make(chan pgtype.UUID, racers)
	var eg errgroup.Group
	for i := 0; i < racers; i++ {
		eg.Go(func() error {
			rotated, err := f.grants.RotateRefreshToken(ctx, first.ID, successors[i].ID)
			switch {
			case err == nil:
				wins <- rotated.SupersededBy
				return nil
			case errors.Is(err, pgx.ErrNoRows):
				return nil
			default:
				return err
			}
		})
	}
	if err := eg.Wait(); err != nil {
		t.Fatalf("concurrent rotate: %v", err)
	}
	close(wins)
	if n := len(wins); n != 1 {
		t.Fatalf("concurrent rotations that won = %d, want exactly 1", n)
	}
	winner := <-wins

	row, err := f.grants.GetRefreshTokenByHash(ctx, "refresh-hash-1")
	if err != nil {
		t.Fatalf("GetRefreshTokenByHash after rotation: %v", err)
	}
	if !row.RotatedAt.Valid || row.SupersededBy != winner {
		t.Fatalf("rotated row = %+v, want rotated_at set and superseded_by = the winning successor %v", row, winner)
	}
	if _, err := f.grants.GetRefreshTokenByHash(ctx, "never-issued"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetRefreshTokenByHash(never-issued): err = %v, want pgx.ErrNoRows", err)
	}
}

// TestMCPOAuthGrantStore_RevocationCascadesRefreshChain proves revocation
// stays structural with refresh tokens (technical plan §43.16): deleting
// the grant -- or the client above it -- takes a whole rotation chain
// with it, the self-reference (superseded_by) included, in one statement.
func TestMCPOAuthGrantStore_RevocationCascadesRefreshChain(t *testing.T) {
	for _, revoke := range []string{"grant", "client"} {
		t.Run(revoke, func(t *testing.T) {
			ctx := context.Background()
			f := newMCPOAuthFixture(ctx, t)
			g, _ := f.createGrantWithToken(ctx, t, []string{"mcp:read"}, "hash-chain-"+revoke, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
			chain := []sqlcgen.McpOauthRefreshToken{
				f.createRefreshToken(ctx, t, g.ID, "chain-0-"+revoke, []string{"mcp:read"}, time.Now().Add(time.Hour)),
				f.createRefreshToken(ctx, t, g.ID, "chain-1-"+revoke, []string{"mcp:read"}, time.Now().Add(time.Hour)),
				f.createRefreshToken(ctx, t, g.ID, "chain-2-"+revoke, []string{"mcp:read"}, time.Now().Add(time.Hour)),
			}
			for i := 0; i+1 < len(chain); i++ {
				if _, err := f.grants.RotateRefreshToken(ctx, chain[i].ID, chain[i+1].ID); err != nil {
					t.Fatalf("rotate %d -> %d: %v", i, i+1, err)
				}
			}

			switch revoke {
			case "grant":
				if n, err := f.grants.DeleteGrant(ctx, g.ID); err != nil || n != 1 {
					t.Fatalf("DeleteGrant: n = %d, err = %v, want 1 row", n, err)
				}
			default:
				if _, err := f.clients.Delete(ctx, f.client.ID); err != nil {
					t.Fatalf("Delete client: %v", err)
				}
			}
			var left int
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM mcp_oauth_refresh_tokens`).Scan(&left); err != nil || left != 0 {
				t.Fatalf("refresh tokens after revocation = %d (err %v), want 0", left, err)
			}
		})
	}
}

// TestMCPOAuthGrantStore_RefreshSuccessorOnlyOnceRotated pins the table's
// own CHECK: a refresh token can name a successor only once it has been
// rotated.
func TestMCPOAuthGrantStore_RefreshSuccessorOnlyOnceRotated(t *testing.T) {
	ctx := context.Background()
	f := newMCPOAuthFixture(ctx, t)
	g, _ := f.createGrantWithToken(ctx, t, []string{"mcp:read"}, "hash-check-test", time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	a := f.createRefreshToken(ctx, t, g.ID, "check-a", nil, time.Now().Add(time.Hour))
	b := f.createRefreshToken(ctx, t, g.ID, "check-b", nil, time.Now().Add(time.Hour))

	_, err := f.pool.Exec(ctx, `UPDATE mcp_oauth_refresh_tokens SET superseded_by = $1 WHERE id = $2`, b.ID, a.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "mcp_oauth_refresh_tokens_superseded_only_when_rotated" {
		t.Fatalf("naming a successor on an unrotated token: err = %v, want a check violation", err)
	}
}
