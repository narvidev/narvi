package mcpauth

import (
	"bytes"
	"html"
	"html/template"
	"regexp"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/mcpclient"
)

var (
	consentTitle = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
	consentH1    = regexp.MustCompile(`(?s)<h1>(.*?)</h1>`)
	anyTag       = regexp.MustCompile(`<[^>]*>`)
)

// visibleText is what a reader sees of re's first match in body: tags
// stripped, entities decoded, runs of space collapsed.
func visibleText(t *testing.T, re *regexp.Regexp, body string) string {
	t.Helper()
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the page has no match for %s:\n%s", re, body)
	}
	return strings.Join(strings.Fields(html.UnescapeString(anyTag.ReplaceAllString(m[1], ""))), " ")
}

// TestConsentPage_SelfRegisteredNameNeverHeadsThePage: a dynamically
// registered client's name is its unauthenticated registrant's free choice,
// so it can copy the very words the page keeps for a metadata-document
// client's verified host -- "the app at tools.example". Rendered through
// the real embedded template, with both kinds of client carrying that name,
// the dynamic client's title and headline are fixed words that never carry
// its name and never read like the verified-host form; its name comes
// second, quoted and marked as the name it gave itself, as a
// metadata-document client's name comes second after its host (technical
// plan §43.14, and the client identity spoofing threat row of §43.19).
func TestConsentPage_SelfRegisteredNameNeverHeadsThePage(t *testing.T) {
	tpl, err := template.ParseFS(templatesFS, "templates/consent.html")
	if err != nil {
		t.Fatalf("parse the embedded consent template: %v", err)
	}
	const spoof = "the app at tools.example"
	// The registration path accepts the name, so an attacker can reach
	// the page with it.
	md, err := mcpclient.ParseRegistrationRequest([]byte(`{"client_name":"` + spoof + `","redirect_uris":["https://attacker.example/cb"]}`))
	if err != nil || md.ClientName != spoof {
		t.Fatalf("ParseRegistrationRequest = %+v, %v; want the name %q accepted", md, err, spoof)
	}
	render := func(client sqlcgen.McpOauthClient, redirectURI string) string {
		t.Helper()
		page := consentPage{
			ClientName:       client.ClientName,
			UserEmail:        "member@alpha.example",
			RedirectHost:     mcpclient.RedirectHost(redirectURI),
			RedirectLoopback: mcpclient.IsLoopbackRedirect(redirectURI),
			RequestID:        "00000000-0000-0000-0000-000000000001",
			Nonce:            "nonce",
		}
		if !identityFor(&page, client) {
			t.Fatalf("identityFor(%s client) refused it", client.Kind)
		}
		var buf bytes.Buffer
		if err := tpl.Execute(&buf, page); err != nil {
			t.Fatalf("render the consent page for a %s client: %v", client.Kind, err)
		}
		return buf.String()
	}
	verified := render(sqlcgen.McpOauthClient{
		ClientID:   "https://tools.example/mcp/client.json",
		Kind:       sqlcgen.McpOauthClientKindMetadataDocument,
		ClientName: spoof,
	}, "https://tools.example/cb")
	dynamic := render(sqlcgen.McpOauthClient{
		ClientID:   "narvi_mcp_d_spoof",
		Kind:       sqlcgen.McpOauthClientKindDynamic,
		ClientName: md.ClientName,
	}, md.RedirectURIs[0])

	for _, el := range []struct {
		what         string
		re           *regexp.Regexp
		verifiedWant string
		dynamicWant  string
	}{
		{"title", consentTitle, "Allow the app at tools.example? - Narvi", "Allow an app that registered itself? - Narvi"},
		{"headline", consentH1, "Allow the app at tools.example to use Narvi as you?", "Allow an app that registered itself to use Narvi as you?"},
	} {
		// The control: the verified-host form the name imitates.
		if got := visibleText(t, el.re, verified); got != el.verifiedWant {
			t.Errorf("metadata-document %s = %q, want %q", el.what, got, el.verifiedWant)
		}
		got := visibleText(t, el.re, dynamic)
		if got != el.dynamicWant {
			t.Errorf("dynamic %s = %q, want %q", el.what, got, el.dynamicWant)
		}
		if strings.Contains(strings.ToLower(got), strings.ToLower(spoof)) {
			t.Errorf("dynamic %s %q carries the name the client gave itself", el.what, got)
		}
		if strings.Contains(strings.ToLower(got), "the app at") || got == visibleText(t, el.re, verified) {
			t.Errorf("dynamic %s %q reads like a metadata-document client's verified host", el.what, got)
		}
	}

	// The name is there, second: quoted and marked as its own choice,
	// after the headline, with the plain statement that nothing vouches
	// for it.
	const second = `It calls itself <strong>"the app at tools.example"</strong>, a name it gave itself. ` + dynamicIdentity
	switch at := strings.Index(dynamic, second); {
	case at < 0:
		t.Errorf("the dynamic page lacks %q:\n%s", second, dynamic)
	case at < strings.Index(dynamic, "</h1>"):
		t.Error("the dynamic page shows the name before its headline")
	}
}
