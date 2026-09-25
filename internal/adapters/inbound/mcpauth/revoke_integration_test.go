//go:build integration

package mcpauth_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// assertRevokedAnswer: 200, an empty body, never cached (RFC 7009 section
// 2.2) -- the one answer every readable revocation request from an
// identified client gets, whatever was revoked.
func assertRevokedAnswer(t *testing.T, status int, header http.Header, body string) {
	t.Helper()
	if status != http.StatusOK || body != "" || header.Get("Cache-Control") != "no-store" {
		t.Fatalf("revoke: status %d Cache-Control %q body %q, want 200, no-store, an empty body", status, header.Get("Cache-Control"), body)
	}
}

// TestRevoke_RFC7009_StopsOnNextCall: a client revoking either of its
// tokens -- access or refresh, with the right hint, the wrong one, or
// none -- revokes the whole authorization: its access token is refused on
// the next /mcp call, its refresh token refreshes nothing, the grant is
// gone, and the revocation is audited (reason client). An unknown token,
// and a token issued to ANOTHER client, get the very same 200 and an empty
// body -- and revoke nothing.
func TestRevoke_RFC7009_StopsOnNextCall(t *testing.T) {
	r := newASRig(t)
	other := r.newClient(t, "narvi_mcp_c_other", "Other App", loopbackRedirect)
	basic := func(id string) map[string]string {
		return map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(id)+":"))}
	}

	revoking := []struct {
		name    string
		form    func(pair tokenBody) url.Values
		headers map[string]string
	}{
		{"access token", func(p tokenBody) url.Values {
			return url.Values{"token": {p.AccessToken}, "client_id": {r.client.ClientID}}
		}, nil},
		{"access token, hinted", func(p tokenBody) url.Values {
			return url.Values{"token": {p.AccessToken}, "token_type_hint": {"access_token"}, "client_id": {r.client.ClientID}}
		}, nil},
		{"access token under the refresh-token hint", func(p tokenBody) url.Values {
			return url.Values{"token": {p.AccessToken}, "token_type_hint": {"refresh_token"}, "client_id": {r.client.ClientID}}
		}, nil},
		{"refresh token, hinted", func(p tokenBody) url.Values {
			return url.Values{"token": {p.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {r.client.ClientID}}
		}, nil},
		{"refresh token, no hint", func(p tokenBody) url.Values {
			return url.Values{"token": {p.RefreshToken}, "client_id": {r.client.ClientID}}
		}, nil},
		{"refresh token under an unknown hint", func(p tokenBody) url.Values {
			return url.Values{"token": {p.RefreshToken}, "token_type_hint": {"id_token"}, "client_id": {r.client.ClientID}}
		}, nil},
		{"HTTP Basic with an empty secret", func(p tokenBody) url.Values {
			return url.Values{"token": {p.AccessToken}}
		}, basic(r.client.ClientID)},
	}
	for _, tc := range revoking {
		t.Run("revokes: "+tc.name, func(t *testing.T) {
			user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
			pair := r.issuePair(t, cookie, "mcp:read")
			grantID := r.grantIDs(t, user.ID)[0]
			if got := r.callMCP(pair.AccessToken); got != http.StatusOK {
				t.Fatalf("/mcp before the revocation: status %d, want 200", got)
			}

			rec := r.revoke(tc.form(pair), tc.headers)
			assertRevokedAnswer(t, rec.Code, rec.Header(), rec.Body.String())

			if got := r.callMCP(pair.AccessToken); got != http.StatusUnauthorized {
				t.Fatalf("/mcp on the next call after the revocation: status %d, want 401", got)
			}
			if rec, body := r.refreshWith(t, r.refreshForm(pair.RefreshToken)); rec.Code != http.StatusBadRequest || body.Error != "invalid_grant" {
				t.Fatalf("refresh after the revocation: status %d body %s, want 400 invalid_grant", rec.Code, rec.Body.String())
			}
			if ids := r.grantIDs(t, user.ID); len(ids) != 0 {
				t.Fatalf("grants after the revocation = %v, want none", ids)
			}
			rows := r.auditRows(t, grantID)
			last := rows[len(rows)-1]
			if last.action != "mcp_authorization.revoked" || last.detail["reason"] != "client" || last.detail["actor"] != "client" ||
				last.detail["client_id"] != r.client.ClientID || last.actor != user.ID {
				t.Fatalf("audit rows = %+v, want a final mcp_authorization.revoked, reason client, actor client, attributed to the user", rows)
			}

			// Revoking again: already gone, the same answer, nothing new
			// audited.
			again := r.revoke(tc.form(pair), tc.headers)
			assertRevokedAnswer(t, again.Code, again.Header(), again.Body.String())
			if n := len(r.auditRows(t, grantID)); n != len(rows) {
				t.Fatalf("revoking a revoked token added audit rows: %d -> %d", len(rows), n)
			}
		})
	}

	sparing := []struct {
		name    string
		form    func(pair tokenBody) url.Values
		headers map[string]string
	}{
		{"a token nobody holds", func(tokenBody) url.Values {
			return url.Values{"token": {"narvi_mcp_at_never-issued"}, "client_id": {r.client.ClientID}}
		}, nil},
		{"another client's access token", func(p tokenBody) url.Values {
			return url.Values{"token": {p.AccessToken}, "client_id": {other.ClientID}}
		}, nil},
		{"another client's refresh token", func(p tokenBody) url.Values {
			return url.Values{"token": {p.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {other.ClientID}}
		}, nil},
		{"another client's token, over HTTP Basic", func(p tokenBody) url.Values {
			return url.Values{"token": {p.RefreshToken}}
		}, basic(other.ClientID)},
	}
	for _, tc := range sparing {
		t.Run("spares: "+tc.name, func(t *testing.T) {
			user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
			pair := r.issuePair(t, cookie, "mcp:read")
			grantID := r.grantIDs(t, user.ID)[0]

			rec := r.revoke(tc.form(pair), tc.headers)
			assertRevokedAnswer(t, rec.Code, rec.Header(), rec.Body.String())

			if got := r.callMCP(pair.AccessToken); got != http.StatusOK {
				t.Fatalf("/mcp after a revocation that must revoke nothing: status %d, want 200", got)
			}
			if ids := r.grantIDs(t, user.ID); len(ids) != 1 {
				t.Fatalf("grants = %v, want the one grant, untouched", ids)
			}
			for _, row := range r.auditRows(t, grantID) {
				if row.action == "mcp_authorization.revoked" {
					t.Fatalf("a revocation that must revoke nothing was audited: %+v", row)
				}
			}
			if rec, body := r.refreshWith(t, r.refreshForm(pair.RefreshToken)); rec.Code != http.StatusOK {
				t.Fatalf("refresh after a revocation that must revoke nothing: status %d body %+v, want 200", rec.Code, body)
			}
		})
	}
}

// TestRevoke_Refusals: a request that cannot be read, or whose client
// cannot be identified, is an error in RFC 6749 section 5.2's shape -- and
// revokes nothing. The endpoint reads no cookie: a signed-in browser with
// no client_id is an unidentified client. A disabled client may still
// give its tokens back.
func TestRevoke_Refusals(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	pair := r.issuePair(t, cookie, "mcp:read")

	tests := []struct {
		name       string
		body       string
		headers    map[string]string
		cookie     string
		wantStatus int
		wantError  string
	}{
		{"no client", url.Values{"token": {pair.AccessToken}}.Encode(), nil, "", http.StatusBadRequest, "invalid_client"},
		{"a signed-in browser, no client", url.Values{"token": {pair.AccessToken}}.Encode(), nil, cookie, http.StatusBadRequest, "invalid_client"},
		{"unknown client", url.Values{"token": {pair.AccessToken}, "client_id": {"narvi_mcp_c_nope"}}.Encode(), nil, "", http.StatusBadRequest, "invalid_client"},
		{"a client secret", url.Values{"token": {pair.AccessToken}, "client_id": {r.client.ClientID}, "client_secret": {"s"}}.Encode(), nil, "", http.StatusBadRequest, "invalid_client"},
		{"no token", url.Values{"client_id": {r.client.ClientID}}.Encode(), nil, "", http.StatusBadRequest, "invalid_request"},
		{"a repeated parameter", url.Values{"token": {pair.AccessToken, pair.RefreshToken}, "client_id": {r.client.ClientID}}.Encode(), nil, "", http.StatusBadRequest, "invalid_request"},
		{"a JSON body", `{"token":"` + pair.AccessToken + `","client_id":"` + r.client.ClientID + `"}`, map[string]string{"Content-Type": "application/json"}, "", http.StatusBadRequest, "invalid_request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
			for k, v := range tc.headers {
				headers[k] = v
			}
			rec := r.do(http.MethodPost, "/oauth/revoke", tc.body, headers, tc.cookie)
			if body := decodeToken(t, rec); rec.Code != tc.wantStatus || body.Error != tc.wantError {
				t.Fatalf("status %d body %s, want %d %s", rec.Code, rec.Body.String(), tc.wantStatus, tc.wantError)
			}
			if got := r.callMCP(pair.AccessToken); got != http.StatusOK {
				t.Fatalf("a refused revocation revoked the token: /mcp status %d, want 200", got)
			}
		})
	}

	t.Run("a disabled client may still revoke", func(t *testing.T) {
		if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_clients SET disabled_at = now() WHERE id = $1`, r.client.ID); err != nil {
			t.Fatal(err)
		}
		rec := r.revoke(url.Values{"token": {pair.RefreshToken}, "client_id": {r.client.ClientID}}, nil)
		assertRevokedAnswer(t, rec.Code, rec.Header(), rec.Body.String())
		if ids := r.grantIDs(t, user.ID); len(ids) != 0 {
			t.Fatalf("grants after a disabled client's revocation = %v, want none", ids)
		}
	})
}
