//go:build integration

package mcpauth_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// approvedCode runs consent and returns a fresh code and its verifier.
func (r *asRig) approvedCode(t *testing.T, cookie string) (code, verifier string) {
	t.Helper()
	verifier = newVerifier(t)
	loc := r.approve(t, r.authorizeParams(verifier), cookie, "mcp:read")
	return loc.Query().Get("code"), verifier
}

// TestToken_ExchangeIssuesWorkingToken is the happy path, both client
// authentication forms: body client_id, and HTTP Basic with an empty
// secret (what a standard OAuth library's auto-detection sends first). The
// response carries a refresh token beside the access token, and neither
// plaintext is stored.
func TestToken_ExchangeIssuesWorkingToken(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)

	for _, form := range []string{"body client_id", "basic, empty secret"} {
		t.Run(form, func(t *testing.T) {
			code, verifier := r.approvedCode(t, cookie)
			params := r.exchangeForm(code, verifier)
			headers := map[string]string{}
			if form == "basic, empty secret" {
				params.Del("client_id")
				headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(r.client.ClientID)+":"))
			}
			rec := r.exchange(params, headers)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Pragma") != "no-cache" {
				t.Errorf("token response cache headers = %q/%q, want no-store/no-cache", rec.Header().Get("Cache-Control"), rec.Header().Get("Pragma"))
			}
			body := decodeToken(t, rec)
			if !strings.HasPrefix(body.AccessToken, "narvi_mcp_at_") || body.TokenType != "Bearer" || body.Scope != "mcp:read" ||
				body.ExpiresIn <= 0 || body.ExpiresIn > int64(platform.DefaultTimeouts().MCPAccessTokenTTL.Seconds()) {
				t.Fatalf("token body = %+v", body)
			}
			if got := r.callMCP(body.AccessToken); got != http.StatusOK {
				t.Fatalf("/mcp with the new token: status %d, want 200", got)
			}
			if !strings.HasPrefix(body.RefreshToken, "narvi_mcp_rt_") || body.RefreshToken == body.AccessToken {
				t.Fatalf("refresh_token = %q, want a narvi_mcp_rt_ token distinct from the access token", body.RefreshToken)
			}
			if rt := r.refreshRow(t, body.RefreshToken); rt.RotatedAt.Valid || strings.Join(rt.Scopes, ",") != "mcp:read" {
				t.Fatalf("stored refresh token = %+v, want unrotated, holding the code's scopes", rt)
			}
			var stored int
			if err := r.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM mcp_oauth_access_tokens WHERE token_hash IN ($1, $2)) + (SELECT count(*) FROM mcp_oauth_refresh_tokens WHERE token_hash IN ($1, $2))`, body.AccessToken, body.RefreshToken).Scan(&stored); err != nil || stored != 0 {
				t.Fatalf("plaintext token found in token_hash (%d rows, err %v): tokens must be stored hashed only", stored, err)
			}
		})
	}
}

// TestToken_PKCE_WrongVerifierIsInvalidGrant: a stolen code without the
// verifier is useless -- and the attempt spends it.
func TestToken_PKCE_WrongVerifierIsInvalidGrant(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	code, verifier := r.approvedCode(t, cookie)

	rec := r.exchange(r.exchangeForm(code, newVerifier(t)), nil)
	if rec.Code != http.StatusBadRequest || decodeToken(t, rec).Error != "invalid_grant" {
		t.Fatalf("wrong verifier: status %d body %s, want invalid_grant", rec.Code, rec.Body.String())
	}
	rec = r.exchange(r.exchangeForm(code, verifier), nil)
	if rec.Code != http.StatusBadRequest || decodeToken(t, rec).Error != "invalid_grant" {
		t.Fatalf("right verifier after a failed attempt: status %d body %s, want invalid_grant (the code was spent)", rec.Code, rec.Body.String())
	}

	for name, v := range map[string]string{"missing": "", "too short": "abc", "bad character": strings.Repeat("a", 42) + "+"} {
		code, _ := r.approvedCode(t, cookie)
		rec := r.exchange(r.exchangeForm(code, v), nil)
		if rec.Code != http.StatusBadRequest || decodeToken(t, rec).Error != "invalid_request" {
			t.Errorf("verifier %s: status %d body %s, want invalid_request", name, rec.Code, rec.Body.String())
		}
	}
}

// TestToken_ExpiredCodeIsInvalidGrant.
func TestToken_ExpiredCodeIsInvalidGrant(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	code, verifier := r.approvedCode(t, cookie)
	if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_authorization_codes SET expires_at = now() - interval '1 second' WHERE code_hash = $1`, platform.HashToken(code)); err != nil {
		t.Fatal(err)
	}
	rec := r.exchange(r.exchangeForm(code, verifier), nil)
	if rec.Code != http.StatusBadRequest || decodeToken(t, rec).Error != "invalid_grant" {
		t.Fatalf("status %d body %s, want invalid_grant", rec.Code, rec.Body.String())
	}
}

// TestToken_CodeForOtherClientIsInvalidGrant: a code is bound to the
// client it was issued to.
func TestToken_CodeForOtherClientIsInvalidGrant(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	other := r.newClient(t, "narvi_mcp_c_other", "Other App", loopbackRedirect)
	code, verifier := r.approvedCode(t, cookie)
	form := r.exchangeForm(code, verifier)
	form.Set("client_id", other.ClientID)
	rec := r.exchange(form, nil)
	if rec.Code != http.StatusBadRequest || decodeToken(t, rec).Error != "invalid_grant" {
		t.Fatalf("status %d body %s, want invalid_grant", rec.Code, rec.Body.String())
	}
}

// TestToken_BindingMismatchesAreRefused: redirect_uri must match what the
// code was issued for (the resource binding is
// TestToken_ResourceMismatchIsInvalidTarget's).
func TestToken_BindingMismatchesAreRefused(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)

	code, verifier := r.approvedCode(t, cookie)
	form := r.exchangeForm(code, verifier)
	form.Set("redirect_uri", "http://127.0.0.1:51235/callback")
	if rec := r.exchange(form, nil); rec.Code != http.StatusBadRequest || decodeToken(t, rec).Error != "invalid_grant" {
		t.Errorf("redirect_uri mismatch: status %d body %s, want invalid_grant", rec.Code, rec.Body.String())
	}

	code, verifier = r.approvedCode(t, cookie)
	form = r.exchangeForm(code, verifier)
	form.Del("redirect_uri")
	if rec := r.exchange(form, nil); rec.Code != http.StatusOK {
		t.Errorf("redirect_uri omitted: status %d body %s, want 200 (optional; the stored one binds)", rec.Code, rec.Body.String())
	}
}

// TestToken_ResourceMismatchIsInvalidTarget pins the token endpoint's
// resource binding to RFC 8707 section 2 (technical plan §43.14): a
// missing or foreign resource is invalid_target, decided from the request
// before the code is looked up, so the code is NOT spent -- the same code
// with this deployment's resource still exchanges. A code whose STORED
// resource is not this deployment's (PublicBaseURL changed between
// authorization and exchange) is invalid_target too, and, having reached
// the code, spends it.
func TestToken_ResourceMismatchIsInvalidTarget(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)

	for _, bad := range []string{"", "http://other.test/mcp"} {
		code, verifier := r.approvedCode(t, cookie)
		form := r.exchangeForm(code, verifier)
		if bad == "" {
			form.Del("resource")
		} else {
			form.Set("resource", bad)
		}
		if rec := r.exchange(form, nil); rec.Code != http.StatusBadRequest || decodeToken(t, rec).Error != "invalid_target" {
			t.Fatalf("resource %q: status %d body %s, want invalid_target", bad, rec.Code, rec.Body.String())
		}
		if rec := r.exchange(r.exchangeForm(code, verifier), nil); rec.Code != http.StatusOK {
			t.Fatalf("the same code with the right resource after resource %q was refused: status %d body %s, want 200 (the refusal must not spend the code)", bad, rec.Code, rec.Body.String())
		}
	}

	code, verifier := r.approvedCode(t, cookie)
	if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_authorization_codes SET resource = 'http://other.test/mcp' WHERE code_hash = $1`, platform.HashToken(code)); err != nil {
		t.Fatal(err)
	}
	if rec := r.exchange(r.exchangeForm(code, verifier), nil); rec.Code != http.StatusBadRequest || decodeToken(t, rec).Error != "invalid_target" {
		t.Fatalf("code stored for another resource: status %d body %s, want invalid_target", rec.Code, rec.Body.String())
	}
	var spent bool
	if err := r.pool.QueryRow(context.Background(), `SELECT consumed_at IS NOT NULL FROM mcp_oauth_authorization_codes WHERE code_hash = $1`, platform.HashToken(code)).Scan(&spent); err != nil || !spent {
		t.Fatalf("code stored for another resource: spent = %v (err %v), want spent", spent, err)
	}
}

// TestToken_CodeReuseRevokesGrant: the second exchange of a code is
// refused AND deletes the grant, so the token the FIRST exchange issued
// stops working on its next call; the revocation is audited as
// code_reuse, attributed to the grant's user with actor "system".
func TestToken_CodeReuseRevokesGrant(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	code, verifier := r.approvedCode(t, cookie)
	grantID := r.grantIDs(t, user.ID)[0]

	first := r.exchange(r.exchangeForm(code, verifier), nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first exchange: status %d", first.Code)
	}
	token := decodeToken(t, first).AccessToken
	if got := r.callMCP(token); got != http.StatusOK {
		t.Fatalf("token before replay: status %d, want 200", got)
	}

	second := r.exchange(r.exchangeForm(code, verifier), nil)
	if second.Code != http.StatusBadRequest || decodeToken(t, second).Error != "invalid_grant" {
		t.Fatalf("replay: status %d body %s, want invalid_grant", second.Code, second.Body.String())
	}
	if got := r.callMCP(token); got != http.StatusUnauthorized {
		t.Fatalf("first token after replay: status %d, want 401", got)
	}
	if ids := r.grantIDs(t, user.ID); len(ids) != 0 {
		t.Fatalf("grants after replay = %v, want none", ids)
	}
	rows := r.auditRows(t, grantID)
	last := rows[len(rows)-1]
	if last.action != "mcp_authorization.revoked" || last.detail["reason"] != "code_reuse" || last.detail["actor"] != "system" || last.actor != user.ID {
		t.Fatalf("audit rows = %+v, want a final mcp_authorization.revoked, reason code_reuse, actor system, attributed to the user", rows)
	}
	for _, row := range rows {
		for _, v := range row.detail {
			if s, ok := v.(string); ok && (strings.Contains(s, code) || strings.Contains(s, token)) {
				t.Fatalf("a secret leaked into the audit detail: %+v", row.detail)
			}
		}
	}
}

// TestToken_ClientAuthentication: every client is public -- a secret is
// refused, Basic and body client_id must agree, an unknown or disabled
// client is invalid_client (401 with a Basic challenge when Basic was
// tried), and the token endpoint ignores cookies entirely.
func TestToken_ClientAuthentication(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	basic := func(id, secret string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(id)+":"+url.QueryEscape(secret)))
	}
	// A second, real client: the disagreement row below needs both ids to
	// exist, or the answer would be the unknown-client refusal whatever
	// the agreement check does.
	other := r.newClient(t, "narvi_mcp_c_other", "Other App", loopbackRedirect)

	tests := []struct {
		name        string
		mutate      func(url.Values)
		headers     map[string]string
		wantStatus  int
		wantError   string
		wantBasicCh bool
	}{
		{"client_secret in body", func(f url.Values) { f.Set("client_secret", "s") }, nil, http.StatusBadRequest, "invalid_client", false},
		{"basic with a secret", func(f url.Values) { f.Del("client_id") }, map[string]string{"Authorization": basic(r.client.ClientID, "s")}, http.StatusUnauthorized, "invalid_client", true},
		{"basic disagrees with body", func(f url.Values) { f.Set("client_id", other.ClientID) }, map[string]string{"Authorization": basic(r.client.ClientID, "")}, http.StatusUnauthorized, "invalid_client", true},
		{"bearer header instead", nil, map[string]string{"Authorization": "Bearer x"}, http.StatusUnauthorized, "invalid_client", true},
		{"no client at all", func(f url.Values) { f.Del("client_id") }, nil, http.StatusBadRequest, "invalid_client", false},
		{"unknown client", func(f url.Values) { f.Set("client_id", "narvi_mcp_c_nope") }, nil, http.StatusBadRequest, "invalid_client", false},
		{"unsupported grant type", func(f url.Values) { f.Set("grant_type", "client_credentials") }, nil, http.StatusBadRequest, "unsupported_grant_type", false},
		{"refresh grant without a refresh token", func(f url.Values) { f.Set("grant_type", "refresh_token") }, nil, http.StatusBadRequest, "invalid_request", false},
		{"grant_type missing", func(f url.Values) { f.Del("grant_type") }, nil, http.StatusBadRequest, "invalid_request", false},
		{"code missing", func(f url.Values) { f.Del("code") }, nil, http.StatusBadRequest, "invalid_request", false},
		{"repeated parameter", func(f url.Values) { f.Add("code", "x") }, nil, http.StatusBadRequest, "invalid_request", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, verifier := r.approvedCode(t, cookie)
			form := r.exchangeForm(code, verifier)
			if tc.mutate != nil {
				tc.mutate(form)
			}
			rec := r.exchange(form, tc.headers)
			if rec.Code != tc.wantStatus || decodeToken(t, rec).Error != tc.wantError {
				t.Fatalf("status %d body %s, want %d %s", rec.Code, rec.Body.String(), tc.wantStatus, tc.wantError)
			}
			if got := rec.Header().Get("WWW-Authenticate") != ""; got != tc.wantBasicCh {
				t.Fatalf("WWW-Authenticate = %q, want present=%v", rec.Header().Get("WWW-Authenticate"), tc.wantBasicCh)
			}
		})
	}

	t.Run("json body refused", func(t *testing.T) {
		rec := r.do(http.MethodPost, "/oauth/token", `{"grant_type":"authorization_code"}`, map[string]string{"Content-Type": "application/json"}, "")
		if rec.Code != http.StatusBadRequest || decodeToken(t, rec).Error != "invalid_request" {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("parameters in the query string are not read", func(t *testing.T) {
		code, verifier := r.approvedCode(t, cookie)
		rec := r.do(http.MethodPost, "/oauth/token?"+r.exchangeForm(code, verifier).Encode(), "", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, cookie)
		if rec.Code == http.StatusOK {
			t.Fatalf("a token was issued from query-string parameters")
		}
	})
	t.Run("disabled client", func(t *testing.T) {
		code, verifier := r.approvedCode(t, cookie)
		if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_clients SET disabled_at = now() WHERE id = $1`, r.client.ID); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = r.pool.Exec(context.Background(), `UPDATE mcp_oauth_clients SET disabled_at = NULL WHERE id = $1`, r.client.ID)
		}()
		rec := r.exchange(r.exchangeForm(code, verifier), nil)
		if rec.Code != http.StatusBadRequest || decodeToken(t, rec).Error != "invalid_client" {
			t.Fatalf("status %d body %s, want invalid_client", rec.Code, rec.Body.String())
		}
	})
}

// TestToken_ScopelessGrantAnswersEmptyScope: a consent with every box
// cleared still issues a token, whose scope is "" -- the token works, and
// what it may SEE is decided per call (technical plan §43.17).
func TestToken_ScopelessGrantAnswersEmptyScope(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	verifier := newVerifier(t)
	loc := r.approve(t, r.authorizeParams(verifier), cookie)
	rec := r.exchange(r.exchangeForm(loc.Query().Get("code"), verifier), nil)
	body := decodeToken(t, rec)
	if rec.Code != http.StatusOK || body.Scope != "" || !strings.Contains(rec.Body.String(), `"scope":""`) {
		t.Fatalf("status %d body %s, want 200 with an explicit empty scope", rec.Code, rec.Body.String())
	}
	if got := r.callMCP(body.AccessToken); got != http.StatusOK {
		t.Fatalf("scope-less token at /mcp: status %d, want 200 (authenticated; it just sees no tools)", got)
	}
}

// TestMetadata_RoutesServeDocuments pins the two discovery routes as
// mounted: the documents' own contents are TestMetadata_Documents' job.
func TestMetadata_RoutesServeDocuments(t *testing.T) {
	r := newASRig(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server/oauth"} {
		rec := r.do(http.MethodGet, path, "", nil, "")
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("GET %s: status %d Content-Type %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}
