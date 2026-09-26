package mcpclient_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/mcpclient"
)

const docURL = "https://client.example/mcp/client.json"

// doc renders a metadata document for docURL with fields overridden:
// a value of "" drops the field.
func doc(overrides map[string]string) []byte {
	fields := map[string]string{
		"client_id":     `"` + docURL + `"`,
		"client_name":   `"Editor Plugin"`,
		"redirect_uris": `["http://127.0.0.1/callback","https://client.example/cb"]`,
	}
	for k, v := range overrides {
		if v == "" {
			delete(fields, k)
			continue
		}
		fields[k] = v
	}
	var b strings.Builder
	b.WriteString("{")
	first := true
	for _, k := range []string{"client_id", "client_name", "redirect_uris", "token_endpoint_auth_method", "grant_types", "response_types", "client_uri", "logo_uri", "Client_ID", "software_statement", "jwks"} {
		v, ok := fields[k]
		if !ok {
			continue
		}
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString(`"` + k + `":` + v)
	}
	b.WriteString("}")
	return []byte(b.String())
}

// TestCIMD_ClientIDMismatchRejected: a metadata document is accepted only
// when its client_id is the URL it was fetched from, byte for byte -- the
// confused-deputy row's own proof that a document cannot claim another
// client's identity (technical plan §43.19).
func TestCIMD_ClientIDMismatchRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		clientID string // "" drops the field
	}{
		{"another host", `"https://evil.test/mcp/client.json"`},
		{"another path", `"https://client.example/mcp/other.json"`},
		{"upper-case host", `"https://CLIENT.example/mcp/client.json"`},
		{"upper-case scheme", `"HTTPS://client.example/mcp/client.json"`},
		{"trailing slash", `"https://client.example/mcp/client.json/"`},
		{"default port spelled out", `"https://client.example:443/mcp/client.json"`},
		{"percent-encoded path", `"https://client.example/mcp/client%2Ejson"`},
		{"a query appended", `"https://client.example/mcp/client.json?x=1"`},
		{"missing", ""},
		{"null", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := doc(map[string]string{"client_id": tc.clientID})
			if _, err := mcpclient.ParseMetadataDocument(docURL, body); !errors.Is(err, mcpclient.ErrClientIDMismatch) {
				t.Fatalf("ParseMetadataDocument(%s) = %v, want ErrClientIDMismatch", body, err)
			}
		})
	}
	// A differently-cased duplicate key is not the client_id: field names
	// match exactly, never case-insensitively.
	body := doc(map[string]string{"client_id": "", "Client_ID": `"` + docURL + `"`})
	if _, err := mcpclient.ParseMetadataDocument(docURL, body); !errors.Is(err, mcpclient.ErrClientIDMismatch) {
		t.Fatalf("a Client_ID key stood in for client_id: %v", err)
	}
	if md, err := mcpclient.ParseMetadataDocument(docURL, doc(nil)); err != nil || md.ClientName != "Editor Plugin" {
		t.Fatalf("the matching document: %+v, %v, want accepted", md, err)
	}
}

// TestCIMD_ValidationTable is every other document rule (technical plan
// §43.15), each proven by a document that breaks it alone.
func TestCIMD_ValidationTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		overrides map[string]string
		wantErr   error // nil: accepted
	}{
		{"a valid document", nil, nil},
		{"unknown fields are ignored", map[string]string{"software_statement": `"x.y.z"`, "jwks": `{"keys":[]}`}, nil},
		{"client_uri and logo_uri are ignored", map[string]string{"client_uri": `"javascript:alert(1)"`, "logo_uri": `42`}, nil},
		{"token_endpoint_auth_method none", map[string]string{"token_endpoint_auth_method": `"none"`}, nil},
		{"grant_types with authorization_code", map[string]string{"grant_types": `["authorization_code","refresh_token"]`}, nil},
		{"response_types code", map[string]string{"response_types": `["code"]`}, nil},

		{"client_name missing", map[string]string{"client_name": ""}, mcpclient.ErrInvalidClientName},
		{"client_name empty", map[string]string{"client_name": `""`}, mcpclient.ErrInvalidClientName},
		{"client_name blank", map[string]string{"client_name": `"   "`}, mcpclient.ErrInvalidClientName},
		{"client_name with a control character", map[string]string{"client_name": `"Editor\u0007Plugin"`}, mcpclient.ErrInvalidClientName},
		{"client_name with a bidi override", map[string]string{"client_name": `"Editor\u202ePlugin"`}, mcpclient.ErrInvalidClientName},
		{"client_name with a zero-width space", map[string]string{"client_name": `"Edit\u200bor"`}, mcpclient.ErrInvalidClientName},
		{"client_name of 101 runes", map[string]string{"client_name": `"` + strings.Repeat("é", mcpclient.MaxClientNameRunes+1) + `"`}, mcpclient.ErrInvalidClientName},
		{"client_name not a string", map[string]string{"client_name": `["Editor"]`}, mcpclient.ErrMalformedMetadata},
		{"client_name stacking combining marks", map[string]string{"client_name": `"Editor` + strings.Repeat(`\u030d`, 94) + `"`}, mcpclient.ErrInvalidClientName},

		{"redirect_uris missing", map[string]string{"redirect_uris": ""}, mcpclient.ErrInvalidRedirectURI},
		{"redirect_uris empty", map[string]string{"redirect_uris": `[]`}, mcpclient.ErrInvalidRedirectURI},
		{"redirect_uris not a list", map[string]string{"redirect_uris": `"https://client.example/cb"`}, mcpclient.ErrMalformedMetadata},
		{"a redirect URI on plain http off loopback", map[string]string{"redirect_uris": `["http://client.example/cb"]`}, mcpclient.ErrInvalidRedirectURI},
		{"a redirect URI with a custom scheme", map[string]string{"redirect_uris": `["myapp://cb"]`}, mcpclient.ErrInvalidRedirectURI},
		{"a redirect URI with a fragment", map[string]string{"redirect_uris": `["https://client.example/cb#x"]`}, mcpclient.ErrInvalidRedirectURI},
		{"too many redirect URIs", map[string]string{"redirect_uris": `[` + strings.TrimSuffix(strings.Repeat(`"https://client.example/cb",`, mcpclient.MaxRedirectURIs+1), ",") + `]`}, mcpclient.ErrInvalidRedirectURI},

		{"token_endpoint_auth_method client_secret_basic", map[string]string{"token_endpoint_auth_method": `"client_secret_basic"`}, mcpclient.ErrConfidentialClient},
		{"token_endpoint_auth_method private_key_jwt", map[string]string{"token_endpoint_auth_method": `"private_key_jwt"`}, mcpclient.ErrConfidentialClient},
		{"token_endpoint_auth_method not a string", map[string]string{"token_endpoint_auth_method": `1`}, mcpclient.ErrMalformedMetadata},
		{"grant_types without authorization_code", map[string]string{"grant_types": `["client_credentials"]`}, mcpclient.ErrUnsupportedGrantType},
		{"grant_types empty", map[string]string{"grant_types": `[]`}, mcpclient.ErrUnsupportedGrantType},
		{"grant_types not a list", map[string]string{"grant_types": `"authorization_code"`}, mcpclient.ErrMalformedMetadata},
		{"response_types without code", map[string]string{"response_types": `["token"]`}, mcpclient.ErrUnsupportedResponseType},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := doc(tc.overrides)
			md, err := mcpclient.ParseMetadataDocument(docURL, body)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("ParseMetadataDocument(%s) = %v, want accepted", body, err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("ParseMetadataDocument(%s) = %v, want %v", body, err, tc.wantErr)
			case tc.wantErr == nil && (md.ClientName != "Editor Plugin" || len(md.RedirectURIs) != 2):
				t.Fatalf("ParseMetadataDocument(%s) = %+v, want the name and both redirect URIs", body, md)
			}
		})
	}
	for _, raw := range []string{``, `[]`, `"x"`, `null`, `{`, `{} {}`, `{"client_id":"` + docURL + `"} trailing`} {
		if _, err := mcpclient.ParseMetadataDocument(docURL, []byte(raw)); !errors.Is(err, mcpclient.ErrMalformedMetadata) {
			t.Errorf("ParseMetadataDocument(%q) = %v, want ErrMalformedMetadata", raw, err)
		}
	}
}

// TestParseRegistrationRequest: a dynamic registration request is held to
// the same rules, has no client_id to match (one it sends is ignored), and
// keeps only its name and deduplicated redirect URIs.
func TestParseRegistrationRequest(t *testing.T) {
	t.Parallel()
	md, err := mcpclient.ParseRegistrationRequest([]byte(`{"client_id":"chosen-by-me","client_name":" Desktop Assistant ","redirect_uris":["http://127.0.0.1/cb","http://127.0.0.1/cb"],"application_type":"native"}`))
	if err != nil || md.ClientName != "Desktop Assistant" || len(md.RedirectURIs) != 1 {
		t.Fatalf("ParseRegistrationRequest = %+v, %v, want the trimmed name and one redirect URI", md, err)
	}
	if _, err := mcpclient.ParseRegistrationRequest([]byte(`{"client_name":"x","redirect_uris":["http://evil.test/cb"]}`)); !errors.Is(err, mcpclient.ErrInvalidRedirectURI) {
		t.Fatalf("a bad redirect URI: %v, want ErrInvalidRedirectURI", err)
	}
	if _, err := mcpclient.ParseRegistrationRequest([]byte(`{"client_name":"x","redirect_uris":["http://127.0.0.1/cb"],"token_endpoint_auth_method":"client_secret_post"}`)); !errors.Is(err, mcpclient.ErrConfidentialClient) {
		t.Fatalf("a confidential client: %v, want ErrConfidentialClient", err)
	}
}

func TestValidateClientIDURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{"https://client.example/client.json", true},
		{"https://client.example:8443/mcp/client.json", true},
		{"https://client.example/client.json?v=2", true},
		{"https://127.0.0.1:8443/client.json", true}, // the fetch guard, not this rule, refuses loopback
		{"https://xn--bcher-kva.example/client.json", true},
		{"https://%D0%B0lpha.example/client.json", false}, // ASCII as written, a Cyrillic host once parsed
		{"https://client.example", false},
		{"https://client.example/", false},
		{"http://client.example/client.json", false},
		{"HTTPS://client.example/client.json", false},
		{"https://client.example/client.json#x", false},
		{"https://user@client.example/client.json", false},
		{"https://client.example/a/../client.json", false},
		{"https://client.example/./client.json", false},
		{"https://client.example/%2e%2e/client.json", false},
		{"https://client.example:0/client.json", false},
		{"https://bücher.example/client.json", false},
		{"https://client.example/client json", false},
		{"https:///client.json", false},
		{"https://" + strings.Repeat("a", mcpclient.MaxClientIDURLLength) + ".example/c", false},
		{"narvi_mcp_c_abc", false},
	} {
		if err := mcpclient.ValidateClientIDURL(tc.raw); (err == nil) != tc.ok {
			t.Errorf("ValidateClientIDURL(%q) = %v, want ok=%v", tc.raw, err, tc.ok)
		}
	}
	if got := mcpclient.IdentityHost("https://client.example:8443/mcp/client.json"); got != "client.example:8443" {
		t.Errorf("IdentityHost = %q, want the host with its port", got)
	}
	if got := mcpclient.IdentityHost("https://client.example/"); got != "" {
		t.Errorf("IdentityHost of an invalid client ID URL = %q, want empty", got)
	}
}

// TestHostsWrittenInPlainASCII: every URI a registration stores -- a
// metadata-document client_id, any client's redirect URIs, a
// pre-registered client's homepage -- writes its host in plain ASCII, so
// every host a page shows is the very string a fetch resolves and dials
// (technical plan §43.15). Go's url.Parse decodes a percent-encoded byte
// of 0x80 or above in a host, and net/http then dials that host's xn--
// form: "https://%D0%B0lpha.example/" would be SHOWN as a Cyrillic
// look-alike of a Latin host while the document came from
// xn--lpha-43d.example. Each row -- a look-alike, a right-to-left
// override, a whole-script look-alike, a percent-encoded ASCII byte, an
// IPv6 zone -- is refused BECAUSE its authority is percent-encoded, by
// the validator, by the document and registration parsers, and yields no
// host to display.
func TestHostsWrittenInPlainASCII(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, host string // host: what the URIs below put in their authority
	}{
		{"a percent-encoded Cyrillic look-alike", "%D0%B0lpha.example"},
		{"a percent-encoded right-to-left override", "%E2%80%AEtset.elpmaxe"},
		{"a percent-encoded look-alike with a port", "%D0%B0lpha.example:8443"},
		{"a whole-script look-alike", "%D1%95%D1%80%D0%B0%D1%81%D0%B5.example"},
		{"a percent-encoded percent sign", "client%25example"},
		{"an IPv6 zone", "[fe80::1%25en0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clientID := "https://" + tc.host + "/mcp/client.json"
			if err := mcpclient.ValidateClientIDURL(clientID); !errors.Is(err, mcpclient.ErrInvalidClientIDURL) || !errors.Is(err, mcpclient.ErrPercentEncodedHost) {
				t.Errorf("ValidateClientIDURL(%q) = %v, want ErrInvalidClientIDURL for ErrPercentEncodedHost", clientID, err)
			}
			if got := mcpclient.IdentityHost(clientID); got != "" {
				t.Errorf("IdentityHost(%q) = %q, want no host to show", clientID, got)
			}
			// The redirect-URI variant: a self-registered client supplies it.
			redirect := "https://" + tc.host + "/cb"
			if err := mcpclient.ValidateRedirectURI(redirect); !errors.Is(err, mcpclient.ErrInvalidRedirectURI) || !errors.Is(err, mcpclient.ErrPercentEncodedHost) {
				t.Errorf("ValidateRedirectURI(%q) = %v, want ErrInvalidRedirectURI for ErrPercentEncodedHost", redirect, err)
			}
			if got := mcpclient.RedirectHost(redirect); got != "" {
				t.Errorf("RedirectHost(%q) = %q, want no host to show", redirect, got)
			}
			if mcpclient.MatchRedirectURI([]string{redirect}, redirect) {
				t.Errorf("MatchRedirectURI matched %q, which could never have been registered", redirect)
			}
			body := doc(map[string]string{"redirect_uris": `["` + redirect + `"]`})
			if _, err := mcpclient.ParseMetadataDocument(docURL, body); !errors.Is(err, mcpclient.ErrPercentEncodedHost) {
				t.Errorf("ParseMetadataDocument with redirect URI %q = %v, want ErrPercentEncodedHost", redirect, err)
			}
			if _, err := mcpclient.ParseRegistrationRequest([]byte(`{"client_name":"Editor","redirect_uris":["` + redirect + `"]}`)); !errors.Is(err, mcpclient.ErrPercentEncodedHost) {
				t.Errorf("ParseRegistrationRequest with redirect URI %q = %v, want ErrPercentEncodedHost", redirect, err)
			}
			homepage := "https://" + tc.host + "/about"
			if err := mcpclient.ValidateClientURI(homepage); !errors.Is(err, mcpclient.ErrPercentEncodedHost) {
				t.Errorf("ValidateClientURI(%q) = %v, want ErrPercentEncodedHost", homepage, err)
			}
			if got := mcpclient.ClientURIHost(homepage); got != "" {
				t.Errorf("ClientURIHost(%q) = %q, want no host to show", homepage, got)
			}
		})
	}

	// The same hosts written in their xn-- form are accepted, and shown
	// exactly as written: plain ASCII, the string a fetch resolves.
	for _, host := range []string{"xn--lpha-43d.example", "xn--80ak5af1h.example", "client.example"} {
		for _, got := range []string{
			mcpclient.IdentityHost("https://" + host + "/mcp/client.json"),
			mcpclient.RedirectHost("https://" + host + "/cb"),
			mcpclient.ClientURIHost("https://" + host + "/about"),
		} {
			if got != host {
				t.Errorf("host shown for %s = %q, want %q exactly", host, got, host)
			}
		}
	}
}

func TestMechanisms_Accepts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		m    mcpclient.Mechanisms
		kind mcpclient.Kind
		want bool
	}{
		{mcpclient.Mechanisms{}, mcpclient.KindPreregistered, true},
		{mcpclient.Mechanisms{}, mcpclient.KindMetadataDocument, false},
		{mcpclient.Mechanisms{}, mcpclient.KindDynamic, false},
		{mcpclient.Mechanisms{MetadataDocuments: true}, mcpclient.KindMetadataDocument, true},
		{mcpclient.Mechanisms{MetadataDocuments: true}, mcpclient.KindDynamic, false},
		{mcpclient.Mechanisms{DynamicRegistration: true}, mcpclient.KindDynamic, true},
		{mcpclient.Mechanisms{DynamicRegistration: true}, mcpclient.KindMetadataDocument, false},
		{mcpclient.Mechanisms{MetadataDocuments: true, DynamicRegistration: true}, mcpclient.Kind("other"), false},
	} {
		if got := tc.m.Accepts(tc.kind); got != tc.want {
			t.Errorf("%+v.Accepts(%s) = %v, want %v", tc.m, tc.kind, got, tc.want)
		}
	}
}

// TestCIMD_CacheTTLOnlyShortened: a document's own Cache-Control can make
// it re-fetched sooner than the fixed ceiling, never later (technical plan
// §43.15, owner decision: fixed 1 h, cache headers may only shorten it).
func TestCIMD_CacheTTLOnlyShortened(t *testing.T) {
	t.Parallel()
	const ceiling = time.Hour
	for _, tc := range []struct {
		name      string
		maxAge    time.Duration
		hasMaxAge bool
		want      time.Duration
	}{
		{"no Cache-Control", 0, false, ceiling},
		{"a longer max-age does not extend it", 2 * time.Hour, true, ceiling},
		{"a much longer max-age does not extend it", 365 * 24 * time.Hour, true, ceiling},
		{"max-age equal to the ceiling", ceiling, true, ceiling},
		{"a shorter max-age shortens it", 10 * time.Minute, true, 10 * time.Minute},
		{"no-store or no-cache: re-fetch next time", 0, true, 0},
		{"a negative max-age is zero", -time.Minute, true, 0},
	} {
		if got := mcpclient.MetadataCacheTTL(ceiling, tc.maxAge, tc.hasMaxAge); got != tc.want {
			t.Errorf("%s: MetadataCacheTTL(%v, %v, %v) = %v, want %v", tc.name, ceiling, tc.maxAge, tc.hasMaxAge, got, tc.want)
		}
	}
}
