//go:build integration

package mcpauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves the refresh-token grant (technical plan §43.16):
// rotation on every use, reuse detection that deletes the grant, scopes
// that can only narrow relative to the presented refresh token (never the
// grant's), the resource binding, and a grant past its absolute lifetime
// refreshing nothing.

// refreshWith posts form to the token endpoint and decodes the answer.
func (r *asRig) refreshWith(t *testing.T, form url.Values) (*httptest.ResponseRecorder, tokenBody) {
	t.Helper()
	rec := r.exchange(form, nil)
	return rec, decodeToken(t, rec)
}

// tokenCounts returns how many access and refresh tokens exist under the
// user's grants.
func (r *asRig) tokenCounts(t *testing.T, userID any) (access, refresh int) {
	t.Helper()
	if err := r.pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM mcp_oauth_access_tokens t JOIN mcp_oauth_grants g ON g.id = t.grant_id WHERE g.user_id = $1),
		       (SELECT count(*) FROM mcp_oauth_refresh_tokens t JOIN mcp_oauth_grants g ON g.id = t.grant_id WHERE g.user_id = $1)`,
		userID).Scan(&access, &refresh); err != nil {
		t.Fatal(err)
	}
	return access, refresh
}

// TestRefresh_RotationAndReuseRevokesGrant: a refresh rotates the refresh
// token and issues a new access token; presenting the rotated refresh
// token again is a replay, which deletes the grant -- every access token
// under it, the one the legitimate refresh issued included, is refused on
// its next /mcp call, the new refresh token refreshes nothing -- and the
// revocation is audited as refresh_reuse, attributed to the grant's user
// with actor "system". No secret reaches the audit detail.
func TestRefresh_RotationAndReuseRevokesGrant(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	first := r.issuePair(t, cookie, "mcp:read")
	grantID := r.grantIDs(t, user.ID)[0]

	rec, second := r.refreshWith(t, r.refreshForm(first.RefreshToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: status %d body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Pragma") != "no-cache" {
		t.Errorf("refresh response cache headers = %q/%q, want no-store/no-cache", rec.Header().Get("Cache-Control"), rec.Header().Get("Pragma"))
	}
	if !strings.HasPrefix(second.AccessToken, "narvi_mcp_at_") || second.AccessToken == first.AccessToken ||
		!strings.HasPrefix(second.RefreshToken, "narvi_mcp_rt_") || second.RefreshToken == first.RefreshToken ||
		second.TokenType != "Bearer" || second.Scope != "mcp:read" ||
		second.ExpiresIn <= 0 || second.ExpiresIn > int64(platform.DefaultTimeouts().MCPAccessTokenTTL.Seconds()) {
		t.Fatalf("refresh response = %+v, want a new access token and a new refresh token holding mcp:read", second)
	}
	if status, scopes := r.mcpScopes(t, second.AccessToken); status != http.StatusOK || strings.Join(scopes, ",") != "mcp:read" {
		t.Fatalf("/mcp with the refreshed access token: status %d scopes %v, want 200 [mcp:read]", status, scopes)
	}
	// An access token issued earlier stays valid until its own expiry: a
	// refresh replaces the refresh token, not the access tokens.
	if got := r.callMCP(first.AccessToken); got != http.StatusOK {
		t.Fatalf("/mcp with the earlier access token after a refresh: status %d, want 200", got)
	}
	old, successor := r.refreshRow(t, first.RefreshToken), r.refreshRow(t, second.RefreshToken)
	if !old.RotatedAt.Valid || old.SupersededBy != successor.ID || successor.RotatedAt.Valid {
		t.Fatalf("rotation: old %+v successor %+v, want old rotated into the successor", old, successor)
	}

	// The replay.
	rec, replay := r.refreshWith(t, r.refreshForm(first.RefreshToken))
	if rec.Code != http.StatusBadRequest || replay.Error != "invalid_grant" || replay.AccessToken != "" {
		t.Fatalf("replayed refresh token: status %d body %s, want 400 invalid_grant", rec.Code, rec.Body.String())
	}
	for name, token := range map[string]string{"earlier": first.AccessToken, "refreshed": second.AccessToken} {
		if got := r.callMCP(token); got != http.StatusUnauthorized {
			t.Errorf("/mcp with the %s access token after the replay: status %d, want 401", name, got)
		}
	}
	if rec, body := r.refreshWith(t, r.refreshForm(second.RefreshToken)); rec.Code != http.StatusBadRequest || body.Error != "invalid_grant" {
		t.Errorf("refresh with the successor after the replay: status %d body %s, want 400 invalid_grant", rec.Code, rec.Body.String())
	}
	if ids := r.grantIDs(t, user.ID); len(ids) != 0 {
		t.Fatalf("grants after the replay = %v, want none", ids)
	}
	if access, refresh := r.tokenCounts(t, user.ID); access != 0 || refresh != 0 {
		t.Fatalf("tokens left after the replay: %d access, %d refresh, want none", access, refresh)
	}
	rows := r.auditRows(t, grantID)
	last := rows[len(rows)-1]
	if last.action != "mcp_authorization.revoked" || last.detail["reason"] != "refresh_reuse" || last.detail["actor"] != "system" || last.actor != user.ID {
		t.Fatalf("audit rows = %+v, want a final mcp_authorization.revoked, reason refresh_reuse, actor system, attributed to the user", rows)
	}
	for _, row := range rows {
		for _, v := range row.detail {
			s, _ := v.(string)
			for _, secret := range []string{first.AccessToken, first.RefreshToken, second.AccessToken, second.RefreshToken} {
				if s != "" && strings.Contains(s, secret) {
					t.Fatalf("a secret leaked into the audit detail: %+v", row.detail)
				}
			}
		}
	}
}

// TestRefresh_ConcurrentUseIsAReplay: two refreshes racing with the same
// refresh token are one use and one replay -- there is no grace window --
// so at most one answers 200, the grant is revoked either way, and no
// request fails (no deadlock, no 500).
func TestRefresh_ConcurrentUseIsAReplay(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	pair := r.issuePair(t, cookie, "mcp:read")
	errs := captureErrorLog(t)

	const racers = 4
	recs := make([]*httptest.ResponseRecorder, racers)
	var eg errgroup.Group
	for i := range recs {
		eg.Go(func() error {
			recs[i] = r.exchange(r.refreshForm(pair.RefreshToken), nil)
			return nil
		})
	}
	_ = eg.Wait()

	succeeded := 0
	for _, rec := range recs {
		switch body := decodeToken(t, rec); {
		case rec.Code == http.StatusOK:
			succeeded++
		case rec.Code == http.StatusBadRequest && body.Error == "invalid_grant":
		default:
			t.Errorf("racer: status %d body %s, want 200 or 400 invalid_grant", rec.Code, rec.Body.String())
		}
	}
	if succeeded > 1 {
		t.Fatalf("%d concurrent refreshes with one token succeeded, want at most 1", succeeded)
	}
	if ids := r.grantIDs(t, user.ID); len(ids) != 0 {
		t.Fatalf("grants after a concurrent replay = %v, want none", ids)
	}
	if log := errs.String(); log != "" {
		t.Fatalf("a racer failed:\n%s", log)
	}
}

// TestRefresh_CannotWidenScope: a refresh may narrow the scopes the
// PRESENTED refresh token holds, never widen them -- not even to what a
// later, wider consent for the same client recorded on the grant. A
// refused widening spends nothing: the same refresh token then refreshes,
// with its own scopes.
func TestRefresh_CannotWidenScope(t *testing.T) {
	t.Run("after a later wider consent for the same client", func(t *testing.T) {
		r := newASRig(t)
		user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
		scopeless := r.issuePair(t, cookie)
		if scopeless.Scope != "" {
			t.Fatalf("scope-less approval answered scope %q", scopeless.Scope)
		}
		wider := r.issuePair(t, cookie, "mcp:read")
		var grantScopes []string
		if err := r.pool.QueryRow(context.Background(), `SELECT scopes FROM mcp_oauth_grants WHERE user_id = $1`, user.ID).Scan(&grantScopes); err != nil || strings.Join(grantScopes, ",") != "mcp:read" {
			t.Fatalf("grant scopes after the wider consent = %v (err %v), want [mcp:read]", grantScopes, err)
		}

		widen := r.refreshForm(scopeless.RefreshToken)
		widen.Set("scope", "mcp:read")
		if rec, body := r.refreshWith(t, widen); rec.Code != http.StatusBadRequest || body.Error != "invalid_scope" || body.AccessToken != "" {
			t.Fatalf("widening to the grant's scopes: status %d body %s, want 400 invalid_scope", rec.Code, rec.Body.String())
		}
		if r.refreshRow(t, scopeless.RefreshToken).RotatedAt.Valid {
			t.Fatalf("a refused widening spent the refresh token")
		}

		rec, kept := r.refreshWith(t, r.refreshForm(scopeless.RefreshToken))
		if rec.Code != http.StatusOK || kept.Scope != "" || !strings.Contains(rec.Body.String(), `"scope":""`) {
			t.Fatalf("refresh with no scope parameter: status %d body %s, want 200 with the presented token's own (empty) scope", rec.Code, rec.Body.String())
		}
		if status, scopes := r.mcpScopes(t, kept.AccessToken); status != http.StatusOK || len(scopes) != 0 {
			t.Fatalf("/mcp with the refreshed scope-less token: status %d scopes %v, want 200 and no scopes", status, scopes)
		}
		if rt := r.refreshRow(t, kept.RefreshToken); len(rt.Scopes) != 0 {
			t.Fatalf("the refreshed refresh token holds %v, want no scopes", rt.Scopes)
		}
		widen = r.refreshForm(kept.RefreshToken)
		widen.Set("scope", "mcp:read")
		if rec, body := r.refreshWith(t, widen); rec.Code != http.StatusBadRequest || body.Error != "invalid_scope" {
			t.Fatalf("widening the refreshed token: status %d body %s, want 400 invalid_scope", rec.Code, rec.Body.String())
		}
		// The wider consent's own tokens are untouched by any of this.
		if status, scopes := r.mcpScopes(t, wider.AccessToken); status != http.StatusOK || strings.Join(scopes, ",") != "mcp:read" {
			t.Fatalf("the wider consent's token: status %d scopes %v, want 200 [mcp:read]", status, scopes)
		}
	})

	t.Run("a scope this build does not offer", func(t *testing.T) {
		r := newASRig(t)
		_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
		pair := r.issuePair(t, cookie, "mcp:read")
		for _, scope := range []string{"mcp:write", "mcp:read mcp:write", "mcp:admin"} {
			form := r.refreshForm(pair.RefreshToken)
			form.Set("scope", scope)
			if rec, body := r.refreshWith(t, form); rec.Code != http.StatusBadRequest || body.Error != "invalid_scope" {
				t.Fatalf("scope %q: status %d body %s, want 400 invalid_scope", scope, rec.Code, rec.Body.String())
			}
		}
		if r.refreshRow(t, pair.RefreshToken).RotatedAt.Valid {
			t.Fatalf("a refused scope spent the refresh token")
		}
	})

	t.Run("narrowing to what it holds", func(t *testing.T) {
		r := newASRig(t)
		_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
		pair := r.issuePair(t, cookie, "mcp:read")
		form := r.refreshForm(pair.RefreshToken)
		form.Set("scope", "mcp:read")
		if rec, body := r.refreshWith(t, form); rec.Code != http.StatusOK || body.Scope != "mcp:read" {
			t.Fatalf("status %d body %s, want 200 scope mcp:read", rec.Code, rec.Body.String())
		}
	})
}

// TestRefresh_ResourceMustMatch: a refresh request's resource is optional
// (a standard OAuth library sends none) but one that is sent must be this
// deployment's canonical /mcp -- case-folded host, one trailing slash
// allowed -- or the answer is invalid_target; and a grant whose stored
// resource is not this deployment's refreshes nothing. A refusal spends
// nothing.
func TestRefresh_ResourceMustMatch(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	pair := r.issuePair(t, cookie, "mcp:read")

	for _, foreign := range []string{"http://other.test/mcp", rigBase + "/mcp?x=1", rigBase + "/mcp#f", rigBase + "/MCP", "not a url"} {
		form := r.refreshForm(pair.RefreshToken)
		form.Set("resource", foreign)
		if rec, body := r.refreshWith(t, form); rec.Code != http.StatusBadRequest || body.Error != "invalid_target" {
			t.Fatalf("resource %q: status %d body %s, want 400 invalid_target", foreign, rec.Code, rec.Body.String())
		}
	}
	if r.refreshRow(t, pair.RefreshToken).RotatedAt.Valid {
		t.Fatalf("a refused resource spent the refresh token")
	}

	form := r.refreshForm(pair.RefreshToken)
	form.Set("resource", "HTTP://NARVI.TEST/mcp/")
	rec, next := r.refreshWith(t, form)
	if rec.Code != http.StatusOK {
		t.Fatalf("the canonical resource, case-folded with a trailing slash: status %d body %s, want 200", rec.Code, rec.Body.String())
	}
	if ids := r.grantIDs(t, user.ID); len(ids) != 1 {
		t.Fatalf("grants after the refusals and one refresh = %v, want the one grant (no refusal counted as a replay)", ids)
	}

	if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_grants SET resource = 'http://other.test/mcp' WHERE user_id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if rec, body := r.refreshWith(t, r.refreshForm(next.RefreshToken)); rec.Code != http.StatusBadRequest || body.Error != "invalid_target" {
		t.Fatalf("a grant bound to another resource: status %d body %s, want 400 invalid_target", rec.Code, rec.Body.String())
	}
	if r.refreshRow(t, next.RefreshToken).RotatedAt.Valid {
		t.Fatalf("a refused refresh spent the refresh token")
	}
}

// TestRefresh_RefreshesNothingItShouldNot: an expired grant -- its
// absolute lifetime over, whatever its refresh tokens say -- an expired
// refresh token, a refresh token presented by another client, an access
// token presented as a refresh token, and a token nobody holds are each
// invalid_grant: nothing is issued, nothing is spent, and nothing is
// revoked.
func TestRefresh_RefreshesNothingItShouldNot(t *testing.T) {
	r := newASRig(t)
	other := r.newClient(t, "narvi_mcp_c_other", "Other App", loopbackRedirect)

	tests := []struct {
		name  string
		setup func(t *testing.T, userID any, pair tokenBody) url.Values
	}{
		{"the grant is past its absolute lifetime", func(t *testing.T, userID any, pair tokenBody) url.Values {
			if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_grants SET expires_at = now() - interval '1 second' WHERE user_id = $1`, userID); err != nil {
				t.Fatal(err)
			}
			return r.refreshForm(pair.RefreshToken)
		}},
		{"the refresh token expired", func(t *testing.T, _ any, pair tokenBody) url.Values {
			if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_refresh_tokens SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, platform.HashToken(pair.RefreshToken)); err != nil {
				t.Fatal(err)
			}
			return r.refreshForm(pair.RefreshToken)
		}},
		{"another client presents it", func(_ *testing.T, _ any, pair tokenBody) url.Values {
			form := r.refreshForm(pair.RefreshToken)
			form.Set("client_id", other.ClientID)
			return form
		}},
		{"an access token presented as a refresh token", func(_ *testing.T, _ any, pair tokenBody) url.Values {
			return r.refreshForm(pair.AccessToken)
		}},
		{"a token nobody holds", func(_ *testing.T, _ any, _ tokenBody) url.Values {
			return r.refreshForm("narvi_mcp_rt_never-issued")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
			pair := r.issuePair(t, cookie, "mcp:read")
			grantID := r.grantIDs(t, user.ID)[0]
			form := tc.setup(t, user.ID, pair)
			accessBefore, refreshBefore := r.tokenCounts(t, user.ID)

			rec, body := r.refreshWith(t, form)
			if rec.Code != http.StatusBadRequest || body.Error != "invalid_grant" || body.AccessToken != "" || body.RefreshToken != "" {
				t.Fatalf("status %d body %s, want 400 invalid_grant and no token", rec.Code, rec.Body.String())
			}
			if access, refresh := r.tokenCounts(t, user.ID); access != accessBefore || refresh != refreshBefore {
				t.Fatalf("tokens %d/%d -> %d/%d, want nothing issued", accessBefore, refreshBefore, access, refresh)
			}
			if r.refreshRow(t, pair.RefreshToken).RotatedAt.Valid {
				t.Fatalf("the refused refresh spent the refresh token")
			}
			if ids := r.grantIDs(t, user.ID); len(ids) != 1 {
				t.Fatalf("grants = %v, want the one grant, not revoked", ids)
			}
			for _, row := range r.auditRows(t, grantID) {
				if row.action == "mcp_authorization.revoked" {
					t.Fatalf("a refused refresh was audited as a revocation: %+v", row)
				}
			}
		})
	}
}

// TestRefresh_ExpiryCappedByGrant: every refresh token lives
// MCPRefreshTokenTTL from its issuance -- a fresh lifetime per rotation --
// and neither a refresh token nor an access token ever outlives its grant.
func TestRefresh_ExpiryCappedByGrant(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	pair := r.issuePair(t, cookie, "mcp:read")
	want := time.Now().Add(platform.DefaultTimeouts().MCPRefreshTokenTTL)
	if got := r.refreshRow(t, pair.RefreshToken).ExpiresAt.Time; got.Before(want.Add(-time.Minute)) || got.After(want.Add(time.Minute)) {
		t.Fatalf("refresh token expires at %v, want about %v (MCPRefreshTokenTTL from issuance)", got, want)
	}

	if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_grants SET expires_at = now() + interval '10 minutes' WHERE user_id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	rec, next := r.refreshWith(t, r.refreshForm(pair.RefreshToken))
	if rec.Code != http.StatusOK || next.ExpiresIn <= 0 || next.ExpiresIn > 600 {
		t.Fatalf("refresh under a grant expiring in 10 minutes: status %d body %s, want expires_in within 600", rec.Code, rec.Body.String())
	}
	var grantExpires time.Time
	if err := r.pool.QueryRow(context.Background(), `SELECT expires_at FROM mcp_oauth_grants WHERE user_id = $1`, user.ID).Scan(&grantExpires); err != nil {
		t.Fatal(err)
	}
	if got := r.refreshRow(t, next.RefreshToken).ExpiresAt.Time; got.After(grantExpires) {
		t.Fatalf("refresh token expires at %v, after its grant (%v)", got, grantExpires)
	}
}

// TestRefresh_RefreshTokenIsNeverABearer: a refresh token presented at
// /mcp authenticates nothing -- the bearer check only ever reads access
// tokens.
func TestRefresh_RefreshTokenIsNeverABearer(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	pair := r.issuePair(t, cookie, "mcp:read")
	if got := r.callMCP(pair.RefreshToken); got != http.StatusUnauthorized {
		t.Fatalf("/mcp with a refresh token as the bearer: status %d, want 401", got)
	}
}
