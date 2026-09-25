//go:build integration

package mcpauth_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestConsent_FrameHeaders: the consent page cannot be framed, cached,
// referred from, or made to load anything, and its forms may submit only
// to itself and the stored redirect target.
func TestConsent_FrameHeaders(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	requestID := r.startConsent(t, r.authorizeParams(newVerifier(t)), cookie)
	rec, _ := r.renderConsent(t, requestID, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	want := map[string]string{
		"X-Frame-Options":         "DENY",
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' http://127.0.0.1:51234; frame-ancestors 'none'; base-uri 'none'",
	}
	for h, v := range want {
		if got := rec.Header().Get(h); got != v {
			t.Errorf("%s = %q, want %q", h, got, v)
		}
	}
}

// TestConsent_ShowsClientIdentityAndRedirectHost: the page states who
// vouched for the client and the true host the browser returns to, with
// a warning for this machine and none for a real https host.
func TestConsent_ShowsClientIdentityAndRedirectHost(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)

	loopback := r.authorizeParams(newVerifier(t))
	requestID := r.startConsent(t, loopback, cookie)
	rec, _ := r.renderConsent(t, requestID, cookie)
	body := rec.Body.String()
	for _, want := range []string{
		"Editor Plugin",
		"Registered by an administrator of this deployment.",
		"<strong>127.0.0.1</strong>",
		"That is this computer.",
		user.PrimaryEmail,
		`value="mcp:read" checked`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("loopback consent page lacks %q", want)
		}
	}

	https := r.authorizeParams(newVerifier(t))
	https.Set("redirect_uri", httpsRedirect)
	requestID = r.startConsent(t, https, cookie)
	rec, _ = r.renderConsent(t, requestID, cookie)
	body = rec.Body.String()
	if !strings.Contains(body, "<strong>client.example</strong>") || strings.Contains(body, "That is this computer.") {
		t.Errorf("https consent page: want host client.example and no loopback warning")
	}

	scopeless := r.authorizeParams(newVerifier(t))
	scopeless.Del("scope")
	requestID = r.startConsent(t, scopeless, cookie)
	rec, _ = r.renderConsent(t, requestID, cookie)
	if !strings.Contains(rec.Body.String(), "asked for no access") || strings.Contains(rec.Body.String(), `name="scope"`) {
		t.Errorf("scope-less consent page: want the no-access notice and no checkbox")
	}
}

// TestConsent_ClientNameIsEscaped: the one client-influenced string on the
// page renders as text.
func TestConsent_ClientNameIsEscaped(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	evil := r.newClient(t, "narvi_mcp_c_evil", `<script>alert(1)</script>`, loopbackRedirect)
	params := r.authorizeParams(newVerifier(t))
	params.Set("client_id", evil.ClientID)
	requestID := r.startConsent(t, params, cookie)
	rec, _ := r.renderConsent(t, requestID, cookie)
	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") || !strings.Contains(rec.Body.String(), "&lt;script&gt;") {
		t.Fatalf("client name was not escaped")
	}
}

// TestConsent_MissingOrWrongNonceRefused: the decision needs the nonce of
// the page as last rendered -- missing, wrong, or superseded by a newer
// render are all refused, and nothing is decided.
func TestConsent_MissingOrWrongNonceRefused(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	requestID := r.startConsent(t, r.authorizeParams(newVerifier(t)), cookie)
	_, first := r.renderConsent(t, requestID, cookie)
	_, second := r.renderConsent(t, requestID, cookie)

	for name, nonce := range map[string]string{"missing": "", "wrong": "not-the-nonce", "superseded by a newer render": first} {
		form := url.Values{"request": {requestID}, "decision": {"approve"}, "scope": {"mcp:read"}}
		if nonce != "" {
			form.Set("nonce", nonce)
		}
		rec := r.postConsent(form, sameOriginHeaders(), cookie)
		if rec.Code != http.StatusForbidden || rec.Header().Get("Location") != "" {
			t.Errorf("%s nonce: status %d Location %q, want 403 and no redirect", name, rec.Code, rec.Header().Get("Location"))
		}
	}
	// The current nonce still decides: nothing above consumed the request.
	form := url.Values{"request": {requestID}, "nonce": {second}, "decision": {"approve"}, "scope": {"mcp:read"}}
	if rec := r.postConsent(form, sameOriginHeaders(), cookie); rec.Code != http.StatusFound {
		t.Fatalf("current nonce: status %d, want 302", rec.Code)
	}
}

// TestConsent_CrossSiteOriginRefused: a decision must come from this
// deployment's own page. A foreign Origin, a cross-site Sec-Fetch-Site, or
// neither header at all is refused; Origin: null is accepted only when
// the browser-set Sec-Fetch-Site vouches same-origin (what a browser sends
// from a no-referrer page).
func TestConsent_CrossSiteOriginRefused(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)

	tests := []struct {
		name    string
		headers map[string]string
		ok      bool
	}{
		{"same origin", map[string]string{"Origin": rigBase, "Sec-Fetch-Site": "same-origin"}, true},
		{"same origin, upper-case, explicit default port", map[string]string{"Origin": "HTTP://NARVI.TEST:80"}, true},
		{"null origin vouched by Sec-Fetch-Site", map[string]string{"Origin": "null", "Sec-Fetch-Site": "same-origin"}, true},
		{"foreign origin", map[string]string{"Origin": "https://evil.test"}, false},
		{"foreign origin claiming same-origin fetch", map[string]string{"Origin": "https://evil.test", "Sec-Fetch-Site": "same-origin"}, false},
		{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"same-site fetch", map[string]string{"Origin": rigBase, "Sec-Fetch-Site": "same-site"}, false},
		{"null origin alone", map[string]string{"Origin": "null"}, false},
		{"neither header", map[string]string{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requestID := r.startConsent(t, r.authorizeParams(newVerifier(t)), cookie)
			_, nonce := r.renderConsent(t, requestID, cookie)
			headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
			for k, v := range tc.headers {
				headers[k] = v
			}
			form := url.Values{"request": {requestID}, "nonce": {nonce}, "decision": {"approve"}, "scope": {"mcp:read"}}
			rec := r.postConsent(form, headers, cookie)
			if tc.ok && rec.Code != http.StatusFound {
				t.Fatalf("status %d, want 302 (body %s)", rec.Code, rec.Body.String())
			}
			if !tc.ok && (rec.Code != http.StatusForbidden || rec.Header().Get("Location") != "") {
				t.Fatalf("status %d Location %q, want 403 and no redirect", rec.Code, rec.Header().Get("Location"))
			}
		})
	}
}

// TestConsent_OtherUserCannotDecide: the first render binds the request;
// another signed-in user can neither render nor decide it, even holding
// the nonce.
func TestConsent_OtherUserCannotDecide(t *testing.T) {
	r := newASRig(t)
	_, owner := r.newUser(t, sqlcgen.UserRoleMember)
	_, other := r.newUser(t, sqlcgen.UserRoleAdmin)
	requestID := r.startConsent(t, r.authorizeParams(newVerifier(t)), owner)
	_, nonce := r.renderConsent(t, requestID, owner)

	if rec, _ := r.renderConsent(t, requestID, other); rec.Code != http.StatusForbidden {
		t.Fatalf("other user's render: status %d, want 403", rec.Code)
	}
	form := url.Values{"request": {requestID}, "nonce": {nonce}, "decision": {"approve"}, "scope": {"mcp:read"}}
	if rec := r.postConsent(form, sameOriginHeaders(), other); rec.Code != http.StatusForbidden || rec.Header().Get("Location") != "" {
		t.Fatalf("other user's decision: status %d Location %q, want 403 and no redirect", rec.Code, rec.Header().Get("Location"))
	}
	if rec := r.postConsent(form, sameOriginHeaders(), ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("signed-out decision: status %d, want 401", rec.Code)
	}
	if rec := r.postConsent(form, sameOriginHeaders(), owner); rec.Code != http.StatusFound {
		t.Fatalf("owner's decision: status %d, want 302", rec.Code)
	}
}

// TestConsent_CannotAddUnrequestedScope: the user can narrow, never widen
// -- a scope the request did not ask for refuses the decision outright.
func TestConsent_CannotAddUnrequestedScope(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	params := r.authorizeParams(newVerifier(t))
	params.Del("scope")
	requestID := r.startConsent(t, params, cookie)
	_, nonce := r.renderConsent(t, requestID, cookie)

	for _, extra := range []string{"mcp:read", "mcp:write"} {
		form := url.Values{"request": {requestID}, "nonce": {nonce}, "decision": {"approve"}, "scope": {extra}}
		if rec := r.postConsent(form, sameOriginHeaders(), cookie); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Fatalf("unrequested %s: status %d Location %q, want 400 and no redirect", extra, rec.Code, rec.Header().Get("Location"))
		}
	}
	if ids := r.grantIDs(t, user.ID); len(ids) != 0 {
		t.Fatalf("grants = %v, want none", ids)
	}
}

// TestConsent_RedirectsOnlyToStoredURI: a redirect_uri smuggled into the
// form is ignored; the code goes to the URI stored with the request.
func TestConsent_RedirectsOnlyToStoredURI(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	requestID := r.startConsent(t, r.authorizeParams(newVerifier(t)), cookie)
	_, nonce := r.renderConsent(t, requestID, cookie)
	form := url.Values{
		"request":      {requestID},
		"nonce":        {nonce},
		"decision":     {"approve"},
		"scope":        {"mcp:read"},
		"redirect_uri": {"https://evil.test/steal"},
		"state":        {"forged"},
	}
	rec := r.postConsent(form, sameOriginHeaders(), cookie)
	loc, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || err != nil {
		t.Fatalf("status %d Location %q", rec.Code, rec.Header().Get("Location"))
	}
	if loc.Scheme+"://"+loc.Host+loc.Path != "http://127.0.0.1:51234/callback" {
		t.Fatalf("redirected to %s, want the stored redirect URI", loc)
	}
	if loc.Query().Get("state") != "state-123" || loc.Query().Get("code") == "" {
		t.Fatalf("redirect query = %v, want the stored state and a code", loc.Query())
	}
}

// TestAuthorize_IssOnSuccessAndError: RFC 9207 iss on the success redirect
// too (the error side is covered by every redirectErr assertion), and the
// approval is audited once.
func TestAuthorize_IssOnSuccessAndError(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	loc := r.approve(t, r.authorizeParams(newVerifier(t)), cookie, "mcp:read")
	q := loc.Query()
	if q.Get("iss") != rigBase+"/oauth" || q.Get("state") != "state-123" || !strings.HasPrefix(q.Get("code"), "narvi_mcp_ac_") || q.Get("error") != "" {
		t.Fatalf("success redirect query = %v, want code, state and iss", q)
	}

	ids := r.grantIDs(t, user.ID)
	if len(ids) != 1 {
		t.Fatalf("grants = %v, want one", ids)
	}
	rows := r.auditRows(t, ids[0])
	if len(rows) != 1 || rows[0].action != "mcp_authorization.granted" || rows[0].actor != user.ID ||
		rows[0].detail["client_id"] != r.client.ClientID || rows[0].detail["redirect_host"] != "127.0.0.1" {
		t.Fatalf("audit rows = %+v, want one mcp_authorization.granted by the user", rows)
	}
	if strings.Contains(strings.Join([]string{rows[0].detail["client_name"].(string)}, ""), q.Get("code")) {
		t.Fatalf("the code leaked into the audit detail")
	}

	// Deny: an access_denied redirect with state and iss, no grant, no
	// audit row (refusals are not audit rows).
	other, otherCookie := r.newUser(t, sqlcgen.UserRoleViewer)
	requestID := r.startConsent(t, r.authorizeParams(newVerifier(t)), otherCookie)
	_, nonce := r.renderConsent(t, requestID, otherCookie)
	rec := r.postConsent(url.Values{"request": {requestID}, "nonce": {nonce}, "decision": {"deny"}}, sameOriginHeaders(), otherCookie)
	redirectErr(t, rec, rec.Code, "access_denied", "state-123")
	if ids := r.grantIDs(t, other.ID); len(ids) != 0 {
		t.Fatalf("denied request produced grants %v", ids)
	}
}

// TestConsent_DecisionIsSingleUse: a decided request cannot be decided
// again -- neither re-approved nor flipped to a denial.
func TestConsent_DecisionIsSingleUse(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	requestID := r.startConsent(t, r.authorizeParams(newVerifier(t)), cookie)
	_, nonce := r.renderConsent(t, requestID, cookie)
	form := url.Values{"request": {requestID}, "nonce": {nonce}, "decision": {"approve"}, "scope": {"mcp:read"}}
	if rec := r.postConsent(form, sameOriginHeaders(), cookie); rec.Code != http.StatusFound {
		t.Fatalf("first decision: status %d", rec.Code)
	}
	for _, decision := range []string{"approve", "deny"} {
		form.Set("decision", decision)
		if rec := r.postConsent(form, sameOriginHeaders(), cookie); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Fatalf("second %s: status %d Location %q, want 400 and no redirect", decision, rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec, _ := r.renderConsent(t, requestID, cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("render after decision: status %d, want 400", rec.Code)
	}
}

// TestConsent_ExpiredRequestRefused: a request past its consent window
// can be neither rendered nor decided.
func TestConsent_ExpiredRequestRefused(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	requestID := r.startConsent(t, r.authorizeParams(newVerifier(t)), cookie)
	_, nonce := r.renderConsent(t, requestID, cookie)
	if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_authorization_requests SET expires_at = now() - interval '1 second' WHERE id = $1`, requestID); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"request": {requestID}, "nonce": {nonce}, "decision": {"approve"}, "scope": {"mcp:read"}}
	if rec := r.postConsent(form, sameOriginHeaders(), cookie); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("expired decision: status %d Location %q, want 400 and no redirect", rec.Code, rec.Header().Get("Location"))
	}
	if rec, _ := r.renderConsent(t, requestID, cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("expired render: status %d, want 400", rec.Code)
	}
}

// TestConsent_ReconsentKeepsOneGrant: approving the same client again
// updates the user's one grant (same id) instead of adding a row per
// consent; the row records the latest approval's scopes, for display
// only -- what each token may do is its own (the two
// TestToken_ScopesFixedAtIssuance tests below).
func TestConsent_ReconsentKeepsOneGrant(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	r.approve(t, r.authorizeParams(newVerifier(t)), cookie, "mcp:read")
	first := r.grantIDs(t, user.ID)
	r.approve(t, r.authorizeParams(newVerifier(t)), cookie)
	second := r.grantIDs(t, user.ID)
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Fatalf("grants after two consents = %v then %v, want the same single grant", first, second)
	}
	var scopes []string
	if err := r.pool.QueryRow(context.Background(), `SELECT scopes FROM mcp_oauth_grants WHERE id = $1`, second[0]).Scan(&scopes); err != nil || len(scopes) != 0 {
		t.Fatalf("grant scopes after the second (scope-less) approval = %v (err %v), want the latest approval recorded: none", scopes, err)
	}
}

// wantScopes asserts the bearer gate accepts token and attaches exactly
// want (in order) -- what the MCP handler decides tool visibility from.
func (r *asRig) wantScopes(t *testing.T, label, token string, want ...string) {
	t.Helper()
	status, got := r.mcpScopes(t, token)
	if status != http.StatusOK || strings.Join(got, " ") != strings.Join(want, " ") || got == nil {
		t.Fatalf("%s: /mcp status %d scopes %q, want 200 with %q", label, status, got, want)
	}
}

// TestToken_ScopesFixedAtIssuance_LaterConsentCannotWiden is the round-1
// review's probe (technical plan §43.16): a token issued scope-less stays
// scope-less after the same user approves the same client again with
// mcp:read -- on another install, say -- and so does a code approved
// scope-less but exchanged only after that wider consent. All of them
// hang off the one grant, which is why the grant's scopes are never read.
func TestToken_ScopesFixedAtIssuance_LaterConsentCannotWiden(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)

	scopeless, scope := r.issueToken(t, cookie)
	if scope != "" {
		t.Fatalf("scope-less approval: token response scope %q, want empty", scope)
	}
	r.wantScopes(t, "scope-less token", scopeless)

	// A second scope-less approval whose code is not exchanged yet.
	pendingVerifier := newVerifier(t)
	pendingCode := r.approve(t, r.authorizeParams(pendingVerifier), cookie).Query().Get("code")

	read, scope := r.issueToken(t, cookie, "mcp:read")
	if scope != "mcp:read" {
		t.Fatalf("mcp:read approval: token response scope %q, want mcp:read", scope)
	}
	if ids := r.grantIDs(t, user.ID); len(ids) != 1 {
		t.Fatalf("grants = %v, want the one grant every token hangs off", ids)
	}
	r.wantScopes(t, "mcp:read token", read, "mcp:read")
	r.wantScopes(t, "scope-less token after a wider consent", scopeless)

	rec := r.exchange(r.exchangeForm(pendingCode, pendingVerifier), nil)
	body := decodeToken(t, rec)
	if rec.Code != http.StatusOK || body.Scope != "" {
		t.Fatalf("scope-less code exchanged after a wider consent: status %d scope %q, want 200 with an empty scope", rec.Code, body.Scope)
	}
	r.wantScopes(t, "token from the scope-less code exchanged after a wider consent", body.AccessToken)
}

// TestToken_ScopesFixedAtIssuance_LaterConsentCannotNarrow: a later,
// narrower approval of the same client does not strip a token issued
// earlier -- withdrawing access is revocation, not re-consent -- and
// revoking the grant still stops every token under it on the next call.
func TestToken_ScopesFixedAtIssuance_LaterConsentCannotNarrow(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)

	read, _ := r.issueToken(t, cookie, "mcp:read")
	scopeless, scope := r.issueToken(t, cookie)
	if scope != "" {
		t.Fatalf("scope-less approval: token response scope %q, want empty", scope)
	}
	r.wantScopes(t, "mcp:read token after a narrower consent", read, "mcp:read")
	r.wantScopes(t, "scope-less token", scopeless)

	ids := r.grantIDs(t, user.ID)
	if len(ids) != 1 {
		t.Fatalf("grants = %v, want one", ids)
	}
	if _, err := r.pool.Exec(context.Background(), `DELETE FROM mcp_oauth_grants WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"mcp:read": read, "scope-less": scopeless} {
		if got := r.callMCP(token); got != http.StatusUnauthorized {
			t.Errorf("%s token after the grant was revoked: status %d, want 401", name, got)
		}
	}
}

// TestConsent_ClientDisabledBeforeRenderRefused: a client an operator
// disables between /oauth/authorize and the consent page's first render
// gets no page -- 400, no nonce minted, the request left unbound -- so
// it can be neither approved nor denied (denying would redirect to the
// disabled client). decide()'s own re-check is the second layer
// (TestConsent_ClientDisabledAfterRenderGrantsNothing).
func TestConsent_ClientDisabledBeforeRenderRefused(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	requestID := r.startConsent(t, r.authorizeParams(newVerifier(t)), cookie)
	if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_clients SET disabled_at = now() WHERE id = $1`, r.client.ID); err != nil {
		t.Fatal(err)
	}
	rec, nonce := r.renderConsent(t, requestID, cookie)
	if rec.Code != http.StatusBadRequest || nonce != "" || strings.Contains(rec.Body.String(), `name="nonce"`) {
		t.Fatalf("render for a disabled client: status %d nonce %q, want 400 and no consent form", rec.Code, nonce)
	}
	var bound bool
	if err := r.pool.QueryRow(context.Background(), `SELECT user_id IS NOT NULL OR csrf_nonce_hash IS NOT NULL FROM mcp_oauth_authorization_requests WHERE id = $1`, requestID).Scan(&bound); err != nil || bound {
		t.Fatalf("request after a refused render: bound = %v (err %v), want unbound and without a nonce", bound, err)
	}
}

// TestConsent_ClientDisabledAfterRenderGrantsNothing: a client an
// operator disables while its consent page is open cannot be approved --
// no grant, no code, no redirect.
func TestConsent_ClientDisabledAfterRenderGrantsNothing(t *testing.T) {
	r := newASRig(t)
	user, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	requestID := r.startConsent(t, r.authorizeParams(newVerifier(t)), cookie)
	_, nonce := r.renderConsent(t, requestID, cookie)
	if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_clients SET disabled_at = now() WHERE id = $1`, r.client.ID); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"request": {requestID}, "nonce": {nonce}, "decision": {"approve"}, "scope": {"mcp:read"}}
	if rec := r.postConsent(form, sameOriginHeaders(), cookie); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("status %d Location %q, want 400 and no redirect", rec.Code, rec.Header().Get("Location"))
	}
	if ids := r.grantIDs(t, user.ID); len(ids) != 0 {
		t.Fatalf("grants = %v, want none", ids)
	}
	var codes int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM mcp_oauth_authorization_codes`).Scan(&codes); err != nil || codes != 0 {
		t.Fatalf("codes = %d (err %v), want none", codes, err)
	}
}
