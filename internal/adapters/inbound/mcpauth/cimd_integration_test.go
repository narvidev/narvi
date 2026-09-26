//go:build integration

package mcpauth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/cimdfetch"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/mcpclient"
	"github.com/narvidev/narvi/internal/platform"
)

// assertPageNotRedirect: an authorization refused as a client_id error --
// a 400 page, never a redirect anywhere (technical plan §43.14).
func assertPageNotRedirect(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%s: status %d Location %q Content-Type %q, want a 400 page and no redirect", what, rec.Code, rec.Header().Get("Location"), rec.Header().Get("Content-Type"))
	}
}

// clientRow reads the client carrying clientID, or reports none.
func (r *asRig) clientRow(t *testing.T, clientID string) (sqlcgen.McpOauthClient, bool) {
	t.Helper()
	c, err := r.clients.GetByClientID(context.Background(), clientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.McpOauthClient{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return c, true
}

// TestCIMD_AuthorizationEndpoint: an https client_id is resolved through
// its metadata document (technical plan §43.15) -- fetched on first use and
// stored as a metadata-document client, used from the cache while fresh,
// fetched again once stale; a stale document whose re-fetch fails is kept
// for one more cache lifetime; a document never fetched before that
// cannot be fetched or used refuses the authorization with a page, never a
// redirect, and stores nothing.
func TestCIMD_AuthorizationEndpoint(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	r.serveDocument("Editor Plugin")
	params := forClient(r.authorizeParams(newVerifier(t)), docClientURL)

	r.startConsent(t, params, cookie)
	c, ok := r.clientRow(t, docClientURL)
	if !ok || mcpclient.Kind(c.Kind) != mcpclient.KindMetadataDocument || c.ClientName != "Editor Plugin" ||
		strings.Join(c.RedirectUris, " ") != loopbackRedirect+" "+httpsRedirect || c.CreatedBy.Valid {
		t.Fatalf("stored client = %+v (found %v), want a metadata-document client with the document's name and redirect URIs", c, ok)
	}
	if n := r.documents.count(docClientURL); n != 1 {
		t.Fatalf("fetches after first use = %d, want 1", n)
	}

	r.startConsent(t, params, cookie)
	if n := r.documents.count(docClientURL); n != 1 {
		t.Fatalf("fetches while the cached document is fresh = %d, want still 1", n)
	}

	r.staleDocument(t, docClientURL)
	r.serveDocument("Editor Plugin 2")
	r.startConsent(t, params, cookie)
	if c2, _ := r.clientRow(t, docClientURL); r.documents.count(docClientURL) != 2 || c2.ClientName != "Editor Plugin 2" || c2.ID != c.ID {
		t.Fatalf("after a stale re-fetch: fetches %d, client %+v; want 2 fetches and the same client renamed", r.documents.count(docClientURL), c2)
	}

	// A stale document whose re-fetch fails: kept, and trusted one more
	// cache lifetime -- the failing host is not asked again right away.
	r.staleDocument(t, docClientURL)
	r.documents.set(docClientURL, fakeDocument{err: errors.New("document host unreachable")})
	before := time.Now()
	r.startConsent(t, params, cookie)
	kept, _ := r.clientRow(t, docClientURL)
	if r.documents.count(docClientURL) != 3 || kept.ClientName != "Editor Plugin 2" ||
		kept.MetadataStaleAt.Time.Before(before.Add(time.Hour-time.Minute)) || kept.MetadataStaleAt.Time.After(time.Now().Add(time.Hour)) {
		t.Fatalf("after a failed re-fetch: fetches %d, client %+v; want 3 fetches, the cached document kept for one more hour", r.documents.count(docClientURL), kept)
	}
	if !kept.MetadataRefetchFailedAt.Valid || !kept.MetadataStaleAt.Time.Equal(kept.MetadataRefetchFailedAt.Time.Add(time.Hour)) {
		t.Fatalf("after a failed re-fetch: failure %v, stale %v; want the failure recorded and the grace ending an hour after it", kept.MetadataRefetchFailedAt, kept.MetadataStaleAt.Time)
	}
	r.startConsent(t, params, cookie)
	if n := r.documents.count(docClientURL); n != 3 {
		t.Fatalf("fetches right after a kept document = %d, want still 3", n)
	}
	// A re-fetch that succeeds but no longer validates is a failed one too.
	r.staleDocument(t, docClientURL)
	r.documents.set(docClientURL, fakeDocument{body: metadataDocument("https://evil.test/client.json", "Evil", loopbackRedirect)})
	r.startConsent(t, params, cookie)
	if kept, _ := r.clientRow(t, docClientURL); kept.ClientName != "Editor Plugin 2" {
		t.Fatalf("an invalid re-fetched document replaced the cached one: %+v", kept)
	}

	for _, tc := range []struct {
		name     string
		clientID string
		doc      *fakeDocument
		fetched  bool
	}{
		{"a document that cannot be fetched", "https://down.example/client.json", &fakeDocument{err: errors.New("connection refused")}, true},
		{"a document whose client_id is another URL", "https://mismatch.example/client.json",
			&fakeDocument{body: metadataDocument("https://other.example/client.json", "Impostor", loopbackRedirect)}, true},
		{"a confidential client's document", "https://secret.example/client.json",
			&fakeDocument{body: `{"client_id":"https://secret.example/client.json","client_name":"Secretive","redirect_uris":["` + loopbackRedirect + `"],"token_endpoint_auth_method":"client_secret_basic"}`}, true},
		{"a document naming a bidi-overridden client_name", "https://bidi.example/client.json",
			&fakeDocument{body: metadataDocument("https://bidi.example/client.json", "Editor\u202ePlugin", loopbackRedirect)}, true},
		{"a client_id URL with no path", "https://nopath.example/", nil, false},
		{"a client_id URL with a fragment", "https://frag.example/client.json#x", nil, false},
		{"a client_id URL with userinfo", "https://user@userinfo.example/client.json", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.doc != nil {
				r.documents.set(tc.clientID, *tc.doc)
			}
			rec := r.authorize(forClient(r.authorizeParams(newVerifier(t)), tc.clientID), cookie)
			assertPageNotRedirect(t, tc.name, rec)
			if _, stored := r.clientRow(t, tc.clientID); stored {
				t.Errorf("a client was stored for %s", tc.clientID)
			}
			if fetched := r.documents.count(tc.clientID) > 0; fetched != tc.fetched {
				t.Errorf("fetched = %v, want %v", fetched, tc.fetched)
			}
		})
	}

	t.Run("a redirect_uri the document does not register", func(t *testing.T) {
		p := forClient(r.authorizeParams(newVerifier(t)), docClientURL)
		p.Set("redirect_uri", "https://attacker.test/cb")
		assertPageNotRedirect(t, "unregistered redirect_uri", r.authorize(p, cookie))
	})

	// An operator disabled the client: refused with a page, and its
	// document is not fetched again however stale its cache.
	t.Run("a disabled client is refused without a fetch", func(t *testing.T) {
		const disabledURL = "https://disabled.example/client.json"
		r.documents.set(disabledURL, fakeDocument{body: metadataDocument(disabledURL, "Disabled Plugin", loopbackRedirect)})
		r.startConsent(t, forClient(r.authorizeParams(newVerifier(t)), disabledURL), cookie)
		if _, err := r.pool.Exec(context.Background(), `UPDATE mcp_oauth_clients SET disabled_at = now() WHERE client_id = $1`, disabledURL); err != nil {
			t.Fatal(err)
		}
		r.staleDocument(t, disabledURL)
		before := r.documents.count(disabledURL)
		assertPageNotRedirect(t, "disabled client", r.authorize(forClient(r.authorizeParams(newVerifier(t)), disabledURL), cookie))
		if n := r.documents.count(disabledURL); n != before {
			t.Fatalf("fetches for a disabled client = %d, want still %d", n, before)
		}
	})
}

// TestCIMD_ClientIDMismatchRejectedAtAuthorization: at the authorization
// endpoint itself, a document claiming another client_id -- differing
// from the URL it came from in nothing but case -- is refused with a page,
// never a redirect, and never stored (the domain rule is
// mcpclient's TestCIMD_ClientIDMismatchRejected).
func TestCIMD_ClientIDMismatchRejectedAtAuthorization(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	r.documents.set(docClientURL, fakeDocument{body: metadataDocument(strings.Replace(docClientURL, "client.example", "Client.example", 1), "Editor Plugin", loopbackRedirect)})
	assertPageNotRedirect(t, "mismatched client_id", r.authorize(forClient(r.authorizeParams(newVerifier(t)), docClientURL), cookie))
	if _, stored := r.clientRow(t, docClientURL); stored {
		t.Fatal("a document naming another client_id was stored")
	}
}

// TestCIMD_CacheTTLOnlyShortened_Stored: the document's own Cache-Control
// shortens how long the stored document is trusted, and never extends it
// past MCPClientMetadataCacheTTL; no-store makes the very next
// authorization fetch again.
func TestCIMD_CacheTTLOnlyShortened_Stored(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	for _, tc := range []struct {
		name      string
		maxAge    time.Duration
		hasMaxAge bool
		want      time.Duration
	}{
		{"no Cache-Control", 0, false, time.Hour},
		{"max-age two hours", 2 * time.Hour, true, time.Hour},
		{"max-age a year", 365 * 24 * time.Hour, true, time.Hour},
		{"max-age one minute", time.Minute, true, time.Minute},
		{"no-store", 0, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientID := "https://ttl.example/" + strings.ReplaceAll(tc.name, " ", "-") + ".json"
			r.documents.set(clientID, fakeDocument{body: metadataDocument(clientID, "TTL", loopbackRedirect), maxAge: tc.maxAge, hasMaxAge: tc.hasMaxAge})
			r.startConsent(t, forClient(r.authorizeParams(newVerifier(t)), clientID), cookie)
			c, _ := r.clientRow(t, clientID)
			if got := c.MetadataStaleAt.Time.Sub(c.MetadataFetchedAt.Time); got != tc.want {
				t.Fatalf("trusted for %v, want %v", got, tc.want)
			}
			r.startConsent(t, forClient(r.authorizeParams(newVerifier(t)), clientID), cookie)
			wantFetches := 1
			if tc.want == 0 {
				wantFetches = 2
			}
			if n := r.documents.count(clientID); n != wantFetches {
				t.Fatalf("fetches after two authorizations = %d, want %d", n, wantFetches)
			}
		})
	}
}

// TestCIMD_RefetchNeverChangesIssuedCredentials is the first rule 181's
// own reviews established (technical plan §43.16: what a credential may
// do is fixed when it is issued), for metadata documents: the document is
// re-fetched after a code, an access token and a refresh token were
// issued, and now names other redirect URIs and another name. The code
// still exchanges -- for its own bound redirect URI only, never the
// document's new one -- the access token still authenticates with its own
// scopes, and the refresh token still refreshes; a request authorized
// before the change still returns to the URI it was authorized for.
func TestCIMD_RefetchNeverChangesIssuedCredentials(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	r.serveDocument("Editor Plugin")
	boundRedirect := "http://127.0.0.1:51234/callback" // matches the document's loopback registration

	// Issued under the first document: a live token pair, two unexchanged
	// codes, and one request authorized but not yet decided.
	pairVerifier := newVerifier(t)
	loc := r.approve(t, forClient(r.authorizeParams(pairVerifier), docClientURL), cookie, "mcp:read")
	rec := r.exchange(forClient(r.exchangeForm(loc.Query().Get("code"), pairVerifier), docClientURL), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("first exchange: status %d body %s", rec.Code, rec.Body.String())
	}
	pair := decodeToken(t, rec)
	codeVerifier, spareVerifier := newVerifier(t), newVerifier(t)
	code := r.approve(t, forClient(r.authorizeParams(codeVerifier), docClientURL), cookie, "mcp:read").Query().Get("code")
	spare := r.approve(t, forClient(r.authorizeParams(spareVerifier), docClientURL), cookie, "mcp:read").Query().Get("code")
	pendingID := r.startConsent(t, forClient(r.authorizeParams(newVerifier(t)), docClientURL), cookie)
	_, pendingNonce := r.renderConsent(t, pendingID, cookie)

	// The document changes: every redirect URI replaced, and a new name.
	const newRedirect = "https://moved.example/cb"
	r.documents.set(docClientURL, fakeDocument{body: metadataDocument(docClientURL, "Renamed Plugin", newRedirect)})
	r.staleDocument(t, docClientURL)
	p := forClient(r.authorizeParams(newVerifier(t)), docClientURL)
	p.Set("redirect_uri", newRedirect)
	r.startConsent(t, p, cookie)
	if c, _ := r.clientRow(t, docClientURL); c.ClientName != "Renamed Plugin" || strings.Join(c.RedirectUris, " ") != newRedirect {
		t.Fatalf("the re-fetch did not take: %+v", c)
	}

	// The spare code, presented with the document's NEW redirect URI, is
	// refused: it was bound to the old one when it was issued.
	spareForm := forClient(r.exchangeForm(spare, spareVerifier), docClientURL)
	spareForm.Set("redirect_uri", newRedirect)
	refused := r.exchange(spareForm, nil)
	if body := decodeToken(t, refused); refused.Code != http.StatusBadRequest || body.Error != "invalid_grant" {
		t.Fatalf("code with the document's new redirect URI: status %d body %s, want 400 invalid_grant", refused.Code, refused.Body.String())
	}
	// The other code still exchanges for the redirect URI it was bound to.
	form := forClient(r.exchangeForm(code, codeVerifier), docClientURL)
	if form.Get("redirect_uri") != boundRedirect {
		t.Fatalf("exchange form redirect_uri = %q, want the bound %q", form.Get("redirect_uri"), boundRedirect)
	}
	if rec := r.exchange(form, nil); rec.Code != http.StatusOK || decodeToken(t, rec).Scope != "mcp:read" {
		t.Fatalf("code with its bound redirect URI: status %d body %s, want 200 with scope mcp:read", rec.Code, rec.Body.String())
	}

	// Tokens issued before the change: unaffected.
	if status, scopes := r.mcpScopes(t, pair.AccessToken); status != http.StatusOK || strings.Join(scopes, ",") != "mcp:read" {
		t.Fatalf("access token after the re-fetch: status %d scopes %v, want 200 [mcp:read]", status, scopes)
	}
	refreshForm := r.refreshForm(pair.RefreshToken)
	refreshForm.Set("client_id", docClientURL)
	if rec := r.exchange(refreshForm, nil); rec.Code != http.StatusOK || decodeToken(t, rec).Scope != "mcp:read" {
		t.Fatalf("refresh after the re-fetch: status %d body %s, want 200 with scope mcp:read", rec.Code, rec.Body.String())
	}

	// A request authorized before the change returns to the URI it was
	// authorized for, never one read from the new document.
	decision := r.postConsent(url.Values{"request": {pendingID}, "nonce": {pendingNonce}, "decision": {"approve"}, "scope": {"mcp:read"}}, sameOriginHeaders(), cookie)
	if decision.Code != http.StatusFound || !strings.HasPrefix(decision.Header().Get("Location"), boundRedirect+"?") {
		t.Fatalf("pending decision: status %d Location %q, want a redirect to %s", decision.Code, decision.Header().Get("Location"), boundRedirect)
	}
}

// setMetadataStamps sets a metadata-document client's cache stamps
// outright, as the passing of time would have left them; a zero failedAt
// records no failed re-fetch.
func (r *asRig) setMetadataStamps(t *testing.T, clientIDURL string, fetchedAt, staleAt, failedAt time.Time) {
	t.Helper()
	failed := pgtype.Timestamptz{Time: failedAt, Valid: !failedAt.IsZero()}
	tag, err := r.pool.Exec(context.Background(), `
		UPDATE mcp_oauth_clients SET metadata_fetched_at = $2, metadata_stale_at = $3, metadata_refetch_failed_at = $4
		WHERE client_id = $1`, clientIDURL, fetchedAt, staleAt, failed)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("set the cache stamps of %s: rows %d err %v", clientIDURL, tag.RowsAffected(), err)
	}
}

// TestCIMD_FailedRefetchKeptForOneGraceOnly: a stale document whose
// re-fetch fails is kept for ONE more MCPClientMetadataCacheTTL, measured
// from the first failure since its last successful fetch, and no longer
// (technical plan §43.15). Inside that grace it is used -- a later
// failure neither restarts nor extends the grace; past it, every
// authorization is refused with a page, never a redirect, however long
// the document keeps failing (a document withdrawn, or replaced by one
// this deployment refuses, is never trusted indefinitely); and the first
// fetch that succeeds again ends the failure.
func TestCIMD_FailedRefetchKeptForOneGraceOnly(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	ttl := platform.DefaultTimeouts().MCPClientMetadataCacheTTL
	r.serveDocument("Editor Plugin")
	params := forClient(r.authorizeParams(newVerifier(t)), docClientURL)
	r.startConsent(t, params, cookie)

	// The first failure: kept, and the grace starts.
	r.staleDocument(t, docClientURL)
	r.documents.set(docClientURL, fakeDocument{err: fmt.Errorf("%w: status %d", cimdfetch.ErrUnexpectedStatus, http.StatusNotFound)})
	before := time.Now().Truncate(time.Microsecond) // what a timestamptz keeps
	r.startConsent(t, params, cookie)
	first, _ := r.clientRow(t, docClientURL)
	failedAt := first.MetadataRefetchFailedAt.Time
	if !first.MetadataRefetchFailedAt.Valid || failedAt.Before(before) || failedAt.After(time.Now()) || !first.MetadataStaleAt.Time.Equal(failedAt.Add(ttl)) {
		t.Fatalf("after the first failure: failure %v, stale %v; want the failure recorded now and the grace ending one TTL after it", first.MetadataRefetchFailedAt, first.MetadataStaleAt.Time)
	}

	// A later failure inside the grace: still used, the grace unchanged.
	r.setMetadataStamps(t, docClientURL, first.MetadataFetchedAt.Time, time.Now().Add(-time.Second), failedAt.Add(-ttl/2))
	fetches := r.documents.count(docClientURL)
	r.startConsent(t, params, cookie)
	inside, _ := r.clientRow(t, docClientURL)
	if r.documents.count(docClientURL) != fetches+1 || !inside.MetadataRefetchFailedAt.Time.Equal(failedAt.Add(-ttl/2)) || inside.MetadataStaleAt.Time.After(time.Now()) {
		t.Fatalf("a failure inside the grace: fetches %d, failure %v, stale %v; want one more fetch, the grace neither restarted nor extended", r.documents.count(docClientURL)-fetches, inside.MetadataRefetchFailedAt.Time, inside.MetadataStaleAt.Time)
	}

	// Past the grace -- by a second, then by days on end: refused every
	// time, a fetch tried each time, the grace never extended.
	for _, since := range []time.Duration{ttl + time.Second, 24 * time.Hour, 5 * 24 * time.Hour, 30 * 24 * time.Hour} {
		// Microseconds: what a timestamptz keeps, so the stored stamps
		// compare equal to these.
		failing := time.Now().Add(-since).Truncate(time.Microsecond)
		r.setMetadataStamps(t, docClientURL, failing.Add(-time.Hour), failing.Add(ttl), failing)
		fetches := r.documents.count(docClientURL)
		assertPageNotRedirect(t, fmt.Sprintf("failing for %v", since), r.authorize(forClient(r.authorizeParams(newVerifier(t)), docClientURL), cookie))
		c, ok := r.clientRow(t, docClientURL)
		if !ok || r.documents.count(docClientURL) != fetches+1 || !c.MetadataRefetchFailedAt.Time.Equal(failing) || !c.MetadataStaleAt.Time.Equal(failing.Add(ttl)) {
			t.Fatalf("failing for %v: found %v, fetches %d, failure %v, stale %v; want one fetch tried, the client kept as it was and refused", since, ok, r.documents.count(docClientURL)-fetches, c.MetadataRefetchFailedAt.Time, c.MetadataStaleAt.Time)
		}
	}

	// The document is served again: used at once, and the failure ends.
	r.serveDocument("Editor Plugin, back")
	r.startConsent(t, params, cookie)
	if c, _ := r.clientRow(t, docClientURL); c.ClientName != "Editor Plugin, back" || c.MetadataRefetchFailedAt.Valid || !c.MetadataStaleAt.Time.Equal(c.MetadataFetchedAt.Time.Add(ttl)) {
		t.Fatalf("after a successful fetch: %+v; want the new document, no failure recorded, a fresh TTL", c)
	}
}

// TestCIMD_FailedRefetchNeverOverridesANewerFetch: a failed re-fetch
// records its failure only on the row it read -- if another
// authorization's re-fetch succeeded meanwhile (here with no-store, so
// its document may not be cached at all), the failure changes nothing:
// the newer document stands, trusted no longer than its own response
// allowed, and the authorization goes on with it (technical plan §43.15:
// Cache-Control only ever shortens how long a document is trusted).
func TestCIMD_FailedRefetchNeverOverridesANewerFetch(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	r.serveDocument("Editor Plugin")
	params := forClient(r.authorizeParams(newVerifier(t)), docClientURL)
	r.startConsent(t, params, cookie)
	r.staleDocument(t, docClientURL)

	r.documents.set(docClientURL, fakeDocument{
		err: errors.New("document host unreachable"),
		onFetch: func() {
			now := time.Now()
			if _, err := r.clients.UpsertMetadataDocument(context.Background(), sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams{
				ClientID: docClientURL, ClientName: "Editor Plugin (fetched meanwhile)", RedirectUris: []string{loopbackRedirect},
				MetadataFetchedAt: pgtype.Timestamptz{Time: now, Valid: true},
				MetadataStaleAt:   pgtype.Timestamptz{Time: now, Valid: true}, // no-store
			}); err != nil {
				t.Errorf("the concurrent re-fetch: %v", err)
			}
		},
	})
	r.startConsent(t, params, cookie)
	c, _ := r.clientRow(t, docClientURL)
	if c.ClientName != "Editor Plugin (fetched meanwhile)" || c.MetadataRefetchFailedAt.Valid || !c.MetadataStaleAt.Time.Equal(c.MetadataFetchedAt.Time) {
		t.Fatalf("after a failure racing a newer no-store fetch: name %q, failure %v, trusted for %v; want the newer document, no failure, trusted for 0s",
			c.ClientName, c.MetadataRefetchFailedAt, c.MetadataStaleAt.Time.Sub(c.MetadataFetchedAt.Time))
	}
}

// TestCIMD_LoweredCacheTTLCapsAStoredStaleTime: a stored stale time lying
// further out than the current MCPClientMetadataCacheTTL allows from now
// -- stored under a longer ceiling -- is not trusted past the current
// one: the document is fetched again (technical plan §43.15).
func TestCIMD_LoweredCacheTTLCapsAStoredStaleTime(t *testing.T) {
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	r.serveDocument("Editor Plugin")
	params := forClient(r.authorizeParams(newVerifier(t)), docClientURL)
	r.startConsent(t, params, cookie)
	now := time.Now()
	r.setMetadataStamps(t, docClientURL, now, now.Add(time.Hour), time.Time{})

	lowered := platform.DefaultTimeouts()
	lowered.MCPClientMetadataCacheTTL = 10 * time.Minute
	if err := lowered.Validate(); err != nil {
		t.Fatalf("the lowered timeouts do not validate: %v", err)
	}
	restarted := r.rebuilt(t, rigOptions{mechanisms: mcpclient.Mechanisms{MetadataDocuments: true, DynamicRegistration: true}, timeouts: &lowered})
	restarted.serveDocument("Editor Plugin 2")
	fetches := r.documents.count(docClientURL)
	restarted.startConsent(t, params, cookie)
	if n := r.documents.count(docClientURL); n != fetches+1 {
		t.Fatalf("fetches after the ceiling was lowered below the stored stale time = %d, want 1", n-fetches)
	}
}

// TestCIMD_DisabledClientSurvivesTheSweep: a metadata-document client an
// operator disabled is never swept, however long unused -- its disabled
// row IS the block, since the URL is the client's identity and a deleted
// row would let the next authorization register it afresh, enabled -- so
// the next authorization is still refused, and nothing is fetched
// (technical plan §43.15).
func TestCIMD_DisabledClientSurvivesTheSweep(t *testing.T) {
	ctx := context.Background()
	r := newASRig(t)
	_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
	const blocked = "https://blocked.example/client.json"
	r.documents.set(blocked, fakeDocument{body: metadataDocument(blocked, "Blocked Plugin", loopbackRedirect)})
	r.startConsent(t, forClient(r.authorizeParams(newVerifier(t)), blocked), cookie)
	if _, err := r.pool.Exec(ctx, `UPDATE mcp_oauth_clients SET disabled_at = now() WHERE client_id = $1`, blocked); err != nil {
		t.Fatal(err)
	}
	assertPageNotRedirect(t, "the disabled client", r.authorize(forClient(r.authorizeParams(newVerifier(t)), blocked), cookie))

	// Long past the unused TTL: its requests expired and swept, every
	// stamp aged beyond the sweep's cutoff.
	c, _ := r.clientRow(t, blocked)
	if _, err := r.pool.Exec(ctx, `DELETE FROM mcp_oauth_authorization_requests WHERE client_id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-platform.DefaultTimeouts().MCPDynamicClientUnusedTTL - 2*time.Hour)
	if _, err := r.pool.Exec(ctx, `UPDATE mcp_oauth_clients SET created_at = $2, disabled_at = $2 WHERE id = $1`, c.ID, old); err != nil {
		t.Fatal(err)
	}
	r.setMetadataStamps(t, blocked, old, old.Add(time.Hour), time.Time{})
	if swept, err := r.clients.DeleteUnused(ctx, sweepCutoff()); err != nil || swept != 0 {
		t.Fatalf("the sweep deleted %d clients (err %v), want 0: a disabled client is never swept", swept, err)
	}

	fetches := r.documents.count(blocked)
	assertPageNotRedirect(t, "the disabled client after the sweep", r.authorize(forClient(r.authorizeParams(newVerifier(t)), blocked), cookie))
	if after, ok := r.clientRow(t, blocked); !ok || after.ID != c.ID || !after.DisabledAt.Valid {
		t.Fatalf("after the sweep: client %+v (found %v), want the same row, still disabled", after, ok)
	}
	if n := r.documents.count(blocked); n != fetches {
		t.Fatalf("fetches for the disabled client after the sweep = %d, want none", n-fetches)
	}
}
