//go:build integration

package mcpauth_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/cimdfetch"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/mcpclient"
)

// registration is the RFC 7591 answer, decoded field by field -- and
// the fields that must never appear, kept to prove they do not.
type registration struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Error                   string   `json:"error"`
	ErrorDescription        string   `json:"error_description"`
}

// postRegister posts body to POST /oauth/register as contentType.
func (r *asRig) postRegister(body, contentType string) (int, registration, map[string]json.RawMessage, http.Header) {
	rec := r.do(http.MethodPost, "/oauth/register", body, map[string]string{"Content-Type": contentType}, "")
	var reg registration
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &reg)
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	return rec.Code, reg, raw, rec.Header()
}

// register registers a client dynamically and returns its client_id.
func (r *asRig) register(t *testing.T, body string) string {
	t.Helper()
	code, reg, _, _ := r.postRegister(body, "application/json")
	if code != http.StatusCreated || reg.ClientID == "" {
		t.Fatalf("register: status %d body %+v", code, reg)
	}
	return reg.ClientID
}

// TestRegister_ValidationAndForcedFields: a dynamic registration request
// is validated exactly as a metadata document is, and every accepted one
// is registered as a public client whatever it asked for (technical plan
// §43.15): client_id narvi_mcp_d_..., token_endpoint_auth_method none, the
// authorization_code and refresh_token grants, the code response type --
// with no client_secret and no RFC 7592 management token -- and stored as
// a dynamic client nobody created. Refusals are RFC 7591's own codes, and
// store nothing.
func TestRegister_ValidationAndForcedFields(t *testing.T) {
	r := newASRig(t)

	t.Run("accepted, and forced to a public client", func(t *testing.T) {
		before := time.Now().Add(-time.Minute).Unix()
		code, reg, raw, header := r.postRegister(`{
			"client_name": "  Desktop Assistant  ",
			"redirect_uris": ["http://127.0.0.1/callback", "http://127.0.0.1/callback", "https://assistant.example/cb"],
			"grant_types": ["authorization_code", "client_credentials", "urn:ietf:params:oauth:grant-type:device_code"],
			"response_types": ["code", "token"],
			"application_type": "native",
			"client_uri": "https://assistant.example",
			"logo_uri": "https://assistant.example/logo.png",
			"scope": "mcp:write",
			"contacts": ["ops@assistant.example"],
			"client_id": "chosen-by-the-client",
			"client_secret": "chosen-by-the-client-too"
		}`, "application/json; charset=utf-8")
		if code != http.StatusCreated {
			t.Fatalf("register: status %d body %+v", code, reg)
		}
		if !strings.HasPrefix(reg.ClientID, "narvi_mcp_d_") || len(reg.ClientID) < len("narvi_mcp_d_")+40 {
			t.Errorf("client_id = %q, want a generated narvi_mcp_d_ identifier", reg.ClientID)
		}
		if reg.ClientName != "Desktop Assistant" || strings.Join(reg.RedirectURIs, " ") != "http://127.0.0.1/callback https://assistant.example/cb" {
			t.Errorf("registered name %q redirect URIs %v, want the trimmed name and the deduplicated URIs", reg.ClientName, reg.RedirectURIs)
		}
		if reg.TokenEndpointAuthMethod != "none" || strings.Join(reg.GrantTypes, ",") != "authorization_code,refresh_token" || strings.Join(reg.ResponseTypes, ",") != "code" {
			t.Errorf("forced fields = %q %v %v, want none, [authorization_code refresh_token], [code]", reg.TokenEndpointAuthMethod, reg.GrantTypes, reg.ResponseTypes)
		}
		if reg.ClientIDIssuedAt < before {
			t.Errorf("client_id_issued_at = %d, want now", reg.ClientIDIssuedAt)
		}
		for _, never := range []string{"client_secret", "client_secret_expires_at", "registration_access_token", "registration_client_uri", "client_uri", "logo_uri", "scope", "application_type"} {
			if _, present := raw[never]; present {
				t.Errorf("the registration answer carries %q", never)
			}
		}
		if header.Get("Cache-Control") != "no-store" || header.Get("Content-Type") != "application/json" {
			t.Errorf("headers = %v, want application/json and no-store", header)
		}
		stored, ok := r.clientRow(t, reg.ClientID)
		if !ok || mcpclient.Kind(stored.Kind) != mcpclient.KindDynamic || stored.CreatedBy.Valid || stored.ClientUri != nil ||
			stored.MetadataFetchedAt.Valid || stored.ClientName != "Desktop Assistant" {
			t.Errorf("stored client = %+v, want a dynamic client with no creator, no homepage and no metadata stamps", stored)
		}
		if _, ok := r.clientRow(t, "chosen-by-the-client"); ok {
			t.Error("the client_id the request chose was registered")
		}
	})

	var clientsBefore int
	if err := r.pool.QueryRow(t.Context(), `SELECT count(*) FROM mcp_oauth_clients`).Scan(&clientsBefore); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, contentType, body, wantError string
	}{
		{"not JSON", "text/plain", `{"client_name":"x","redirect_uris":["http://127.0.0.1/cb"]}`, "invalid_client_metadata"},
		{"no Content-Type", "", `{"client_name":"x","redirect_uris":["http://127.0.0.1/cb"]}`, "invalid_client_metadata"},
		{"malformed JSON", "application/json", `{"client_name":`, "invalid_client_metadata"},
		{"a JSON array", "application/json", `[]`, "invalid_client_metadata"},
		{"over 64 KiB", "application/json", `{"client_name":"` + strings.Repeat("x", cimdfetch.MaxDocumentBytes) + `","redirect_uris":["http://127.0.0.1/cb"]}`, "invalid_client_metadata"},
		{"no redirect_uris", "application/json", `{"client_name":"x"}`, "invalid_redirect_uri"},
		{"empty redirect_uris", "application/json", `{"client_name":"x","redirect_uris":[]}`, "invalid_redirect_uri"},
		{"an http redirect URI off loopback", "application/json", `{"client_name":"x","redirect_uris":["http://assistant.example/cb"]}`, "invalid_redirect_uri"},
		{"a custom-scheme redirect URI", "application/json", `{"client_name":"x","redirect_uris":["myapp://cb"]}`, "invalid_redirect_uri"},
		{"a redirect URI with a fragment", "application/json", `{"client_name":"x","redirect_uris":["https://assistant.example/cb#f"]}`, "invalid_redirect_uri"},
		{"no client_name", "application/json", `{"redirect_uris":["http://127.0.0.1/cb"]}`, "invalid_client_metadata"},
		{"a client_name with a bidi override", "application/json", `{"client_name":"Assist\u202eant","redirect_uris":["http://127.0.0.1/cb"]}`, "invalid_client_metadata"},
		{"a client_name with a control character", "application/json", `{"client_name":"Assist\u0000ant","redirect_uris":["http://127.0.0.1/cb"]}`, "invalid_client_metadata"},
		{"a client_name of 101 characters", "application/json", `{"client_name":"` + strings.Repeat("a", 101) + `","redirect_uris":["http://127.0.0.1/cb"]}`, "invalid_client_metadata"},
		{"a confidential client", "application/json", `{"client_name":"x","redirect_uris":["http://127.0.0.1/cb"],"token_endpoint_auth_method":"client_secret_basic"}`, "invalid_client_metadata"},
		{"grant_types without authorization_code", "application/json", `{"client_name":"x","redirect_uris":["http://127.0.0.1/cb"],"grant_types":["client_credentials"]}`, "invalid_client_metadata"},
		{"response_types without code", "application/json", `{"client_name":"x","redirect_uris":["http://127.0.0.1/cb"],"response_types":["token"]}`, "invalid_client_metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, reg, _, _ := r.postRegister(tc.body, tc.contentType)
			if code != http.StatusBadRequest || reg.Error != tc.wantError || reg.ClientID != "" || reg.ErrorDescription == "" {
				t.Fatalf("status %d body %+v, want 400 %s with a description and no client", code, reg, tc.wantError)
			}
			if strings.Contains(reg.ErrorDescription, "\u202e") || strings.Contains(reg.ErrorDescription, "assistant.example") {
				t.Errorf("the description echoes the request: %q", reg.ErrorDescription)
			}
		})
	}
	var clientsAfter int
	if err := r.pool.QueryRow(t.Context(), `SELECT count(*) FROM mcp_oauth_clients`).Scan(&clientsAfter); err != nil || clientsAfter != clientsBefore {
		t.Fatalf("clients after the refusals = %d (err %v), want %d: a refusal stored something", clientsAfter, err, clientsBefore)
	}
}

// TestClientMechanismSwitchedOff_RefusedEverywhere: a client registered
// through a mechanism the deployment has since switched off is refused
// like a disabled one (technical plan §43.15) -- no authorization, no
// consent, no token by code or by refresh, and its live access token stops
// on its very next /mcp call -- while it can still give its tokens back
// (RFC 7009). The switch is a pause, not a disconnection: no grant is
// deleted, the user's connected apps still list it, and with the
// mechanism back on the same access token works and the refresh token
// issued before the switch renews, with no consent in between.
// Pre-registered clients are unaffected.
func TestClientMechanismSwitchedOff_RefusedEverywhere(t *testing.T) {
	on := newASRig(t)
	user, cookie := on.newUser(t, sqlcgen.UserRoleMember)
	on.serveDocument("Editor Plugin")
	dynamicID := on.register(t, `{"client_name":"Desktop Assistant","redirect_uris":["`+loopbackRedirect+`"]}`)

	type issued struct {
		clientID         string
		pair             tokenBody
		code, verifier   string
		pendingID, nonce string
		switchedOff      func() *asRig
	}
	issue := func(clientID string) issued {
		verifier := newVerifier(t)
		loc := on.approve(t, forClient(on.authorizeParams(verifier), clientID), cookie, "mcp:read")
		rec := on.exchange(forClient(on.exchangeForm(loc.Query().Get("code"), verifier), clientID), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: exchange: status %d body %s", clientID, rec.Code, rec.Body.String())
		}
		out := issued{clientID: clientID, pair: decodeToken(t, rec), verifier: newVerifier(t)}
		out.code = on.approve(t, forClient(on.authorizeParams(out.verifier), clientID), cookie, "mcp:read").Query().Get("code")
		out.pendingID = on.startConsent(t, forClient(on.authorizeParams(newVerifier(t)), clientID), cookie)
		_, out.nonce = on.renderConsent(t, out.pendingID, cookie)
		return out
	}
	document, dynamic := issue(docClientURL), issue(dynamicID)
	document.switchedOff = func() *asRig {
		return on.rebuilt(t, rigOptions{mechanisms: mcpclient.Mechanisms{DynamicRegistration: true}})
	}
	dynamic.switchedOff = func() *asRig {
		return on.rebuilt(t, rigOptions{mechanisms: mcpclient.Mechanisms{MetadataDocuments: true}})
	}

	sortedGrantIDs := func(r *asRig) []string {
		ids := r.grantIDs(t, user.ID)
		slices.Sort(ids)
		return ids
	}
	for _, c := range []issued{document, dynamic} {
		t.Run(c.clientID, func(t *testing.T) {
			grantsBefore := sortedGrantIDs(on)
			off := c.switchedOff()
			if rec := off.authorize(forClient(off.authorizeParams(newVerifier(t)), c.clientID), cookie); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
				t.Errorf("authorize: status %d Location %q, want a 400 page", rec.Code, rec.Header().Get("Location"))
			}
			if rec := off.do(http.MethodGet, "/oauth/consent?request="+c.pendingID, "", nil, cookie); rec.Code != http.StatusBadRequest {
				t.Errorf("consent page: status %d, want 400", rec.Code)
			}
			if rec := off.postConsent(url.Values{"request": {c.pendingID}, "nonce": {c.nonce}, "decision": {"approve"}, "scope": {"mcp:read"}}, sameOriginHeaders(), cookie); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
				t.Errorf("consent decision: status %d Location %q, want a 400 page", rec.Code, rec.Header().Get("Location"))
			}
			if rec := off.exchange(forClient(off.exchangeForm(c.code, c.verifier), c.clientID), nil); decodeToken(t, rec).Error != "invalid_client" {
				t.Errorf("code exchange: status %d body %s, want invalid_client", rec.Code, rec.Body.String())
			}
			refresh := off.refreshForm(c.pair.RefreshToken)
			refresh.Set("client_id", c.clientID)
			if rec := off.exchange(refresh, nil); decodeToken(t, rec).Error != "invalid_client" {
				t.Errorf("refresh: status %d body %s, want invalid_client", rec.Code, rec.Body.String())
			}
			if status := off.callMCP(c.pair.AccessToken); status != http.StatusUnauthorized {
				t.Errorf("/mcp with the live access token: status %d, want 401", status)
			}
			// A pause deletes nothing: every grant is still there, and the
			// user's connected apps still list this client.
			if got := sortedGrantIDs(off); !slices.Equal(got, grantsBefore) {
				t.Errorf("grants with the mechanism off = %v, want %v unchanged", got, grantsBefore)
			}
			listed, err := off.grants.ListGrantsForUser(t.Context(), user.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(listed, func(g sqlcgen.ListMCPOAuthGrantsForUserRow) bool { return g.ClientPublicID == c.clientID }) {
				t.Errorf("connected apps with the mechanism off do not list %s", c.clientID)
			}
			// Back on, the same token works: it was the switch. The refresh
			// token issued before the switch renews with no new consent --
			// the refusal while off spent nothing.
			if status := on.callMCP(c.pair.AccessToken); status != http.StatusOK {
				t.Errorf("/mcp with the mechanism back on: status %d, want 200", status)
			}
			renew := on.refreshForm(c.pair.RefreshToken)
			renew.Set("client_id", c.clientID)
			rec := on.exchange(renew, nil)
			renewed := decodeToken(t, rec)
			if rec.Code != http.StatusOK || renewed.AccessToken == "" {
				t.Fatalf("refresh with the mechanism back on: status %d body %s, want 200 and a new pair", rec.Code, rec.Body.String())
			}
			if status := on.callMCP(renewed.AccessToken); status != http.StatusOK {
				t.Errorf("/mcp with the renewed token: status %d, want 200", status)
			}
			// Giving access back is always allowed, the mechanism off.
			if rec := off.revoke(url.Values{"token": {renewed.RefreshToken}, "client_id": {c.clientID}}, nil); rec.Code != http.StatusOK {
				t.Errorf("RFC 7009 revocation: status %d, want 200", rec.Code)
			}
			for _, token := range []string{c.pair.AccessToken, renewed.AccessToken} {
				if status := on.callMCP(token); status != http.StatusUnauthorized {
					t.Errorf("after the client revoked: status %d, want 401", status)
				}
			}
		})
	}

	t.Run("pre-registered clients are unaffected", func(t *testing.T) {
		off := on.rebuilt(t, rigOptions{})
		token, _ := off.issueToken(t, cookie, "mcp:read")
		if status := off.callMCP(token); status != http.StatusOK {
			t.Fatalf("a pre-registered client's token with every other mechanism off: status %d, want 200", status)
		}
	})
}
