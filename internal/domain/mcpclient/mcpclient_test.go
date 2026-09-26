package mcpclient_test

import (
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/mcpclient"
)

func TestValidateRedirectURI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want bool
	}{
		{"https://client.example/callback", true},
		{"https://client.example:8443/cb?x=1", true},
		{"http://127.0.0.1/callback", true},
		{"http://127.0.0.1:43123/callback", true},
		{"http://[::1]:43123/callback", true},
		{"http://localhost:8080/callback", true},
		{"http://client.example/callback", false},    // http off loopback
		{"http://127.0.0.2/callback", false},         // only the two literals RFC 8252 names
		{"com.example.app:/callback", false},         // custom scheme
		{"myapp://callback", false},                  // custom scheme with a host
		{"https://client.example/cb#frag", false},    // fragment
		{"https://client.example/cb#", false},        // empty fragment is still a fragment
		{"https://user:pw@client.example/cb", false}, // userinfo
		{"/callback", false},                         // relative
		{"https:client.example", false},              // opaque
		{"https://client.example/c b", false},        // space
		{"https://client.example/caf\u00e9", false},  // non-ASCII
		{"https://client.example/cb\x00", false},     // control
		{"http://127.0.0.1:0/cb", false},             // port 0
		{"http://127.0.0.1:99999/cb", false},         // port out of range
		{"", false},
		{"https://" + strings.Repeat("a", mcpclient.MaxRedirectURILength) + ".example/", false},
	}
	for _, tc := range tests {
		err := mcpclient.ValidateRedirectURI(tc.raw)
		if (err == nil) != tc.want {
			t.Errorf("ValidateRedirectURI(%q) err = %v, want ok=%v", tc.raw, err, tc.want)
		}
	}
}

// TestMatchRedirectURI_Table is the redirect-substitution threat row's own
// rule table: exact match, the loopback-IP port exception (and only that
// exception), and every near miss an attacker would try.
func TestMatchRedirectURI_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		registered []string
		presented  string
		want       bool
	}{
		{"exact https", []string{"https://client.example/cb"}, "https://client.example/cb", true},
		{"second registered URI", []string{"https://a.example/cb", "https://client.example/cb"}, "https://client.example/cb", true},
		{"loopback IPv4 any port", []string{"http://127.0.0.1/callback"}, "http://127.0.0.1:51234/callback", true},
		{"loopback IPv4 registered with a port, other port", []string{"http://127.0.0.1:1/callback"}, "http://127.0.0.1:51234/callback", true},
		{"loopback IPv6 any port", []string{"http://[::1]/callback"}, "http://[::1]:51234/callback", true},
		{"loopback keeps the path exact", []string{"http://127.0.0.1/callback"}, "http://127.0.0.1:51234/callback2", false},
		{"loopback keeps the query exact", []string{"http://127.0.0.1/callback?a=1"}, "http://127.0.0.1:51234/callback?a=2", false},
		{"loopback path encoding is not normalised", []string{"http://127.0.0.1/callback"}, "http://127.0.0.1:5/%63allback", false},
		{"loopback IPv4 registered, IPv6 presented", []string{"http://127.0.0.1/callback"}, "http://[::1]:5/callback", false},
		{"loopback registered, https presented", []string{"http://127.0.0.1/callback"}, "https://127.0.0.1:5/callback", false},
		{"localhost exact port", []string{"http://localhost:8080/cb"}, "http://localhost:8080/cb", true},
		{"localhost wrong port", []string{"http://localhost:8080/cb"}, "http://localhost:9090/cb", false},
		{"https registered, http presented", []string{"https://client.example/cb"}, "http://client.example/cb", false},
		{"fragment appended", []string{"https://client.example/cb"}, "https://client.example/cb#x", false},
		{"custom scheme", []string{"https://client.example/cb"}, "myapp://client.example/cb", false},
		{"prefix-match attempt (longer path)", []string{"https://client.example/cb"}, "https://client.example/cb/evil", false},
		{"prefix-match attempt (dot segments)", []string{"https://client.example/cb"}, "https://client.example/cb/../evil", false},
		{"prefix-match attempt (host suffix)", []string{"https://client.example/cb"}, "https://client.example.evil.test/cb", false},
		{"case differs", []string{"https://client.example/cb"}, "https://CLIENT.example/cb", false},
		{"userinfo host confusion on loopback", []string{"http://127.0.0.1/cb"}, "http://127.0.0.1:80@evil.test/cb", false},
		{"trailing slash", []string{"https://client.example/cb"}, "https://client.example/cb/", false},
		{"empty presented", []string{"https://client.example/cb"}, "", false},
		{"nothing registered", nil, "https://client.example/cb", false},
		{"presented URI that could never be registered", []string{"http://evil.test/cb"}, "http://evil.test/cb", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mcpclient.MatchRedirectURI(tc.registered, tc.presented); got != tc.want {
				t.Fatalf("MatchRedirectURI(%v, %q) = %v, want %v", tc.registered, tc.presented, got, tc.want)
			}
		})
	}
}

func TestRedirectHostAndLoopback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw      string
		host     string
		loopback bool
	}{
		{"https://client.example:8443/cb", "client.example", false},
		{"http://127.0.0.1:5555/cb", "127.0.0.1", true},
		{"http://[::1]:5555/cb", "::1", true},
		{"http://localhost:5555/cb", "localhost", true},
		{"not a uri", "", false},
	}
	for _, tc := range tests {
		if got := mcpclient.RedirectHost(tc.raw); got != tc.host {
			t.Errorf("RedirectHost(%q) = %q, want %q", tc.raw, got, tc.host)
		}
		if got := mcpclient.IsLoopbackRedirect(tc.raw); got != tc.loopback {
			t.Errorf("IsLoopbackRedirect(%q) = %v, want %v", tc.raw, got, tc.loopback)
		}
	}
}

func TestValidateClientName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"  Editor Plugin  ", "Editor Plugin", true},
		{"", "", false},
		{"   ", "", false},
		{"bad\nname", "", false},
		{"zero\u200bwidth", "", false},
		{"bidi\u202eoverride", "", false},
		{"isolate\u2066name", "", false},
		{"line\u2028separator", "", false},
		{"no\u00a0break", "", false},
		{"private\ue000use", "", false},
		{"unassigned\u0378", "", false},
		{"tab\tinside", "", false},
		{"Caf\u00e9 Plugin", "Caf\u00e9 Plugin", true},
		{"\u7de8\u96c6\u30d7\u30e9\u30b0\u30a4\u30f3", "\u7de8\u96c6\u30d7\u30e9\u30b0\u30a4\u30f3", true},
		{strings.Repeat("x", mcpclient.MaxClientNameRunes), strings.Repeat("x", mcpclient.MaxClientNameRunes), true},
		{strings.Repeat("x", mcpclient.MaxClientNameRunes+1), "", false},
		{"\xff", "", false},
	}
	for _, tc := range tests {
		got, err := mcpclient.ValidateClientName(tc.in)
		if (err == nil) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("ValidateClientName(%q) = %q, %v, want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestValidateClientURI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw string
		ok  bool
	}{
		{"https://client.example", true},
		{"https://client.example/about", true},
		{"http://client.example", false},
		{"javascript:alert(1)", false},
		{"https://user@client.example", false},
		{"https://client.example/a b", false},
		{"", false},
	}
	for _, tc := range tests {
		if err := mcpclient.ValidateClientURI(tc.raw); (err == nil) != tc.ok {
			t.Errorf("ValidateClientURI(%q) err = %v, want ok=%v", tc.raw, err, tc.ok)
		}
	}
}
