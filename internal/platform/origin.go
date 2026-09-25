// This file (origin.go) holds CanonicalOrigin: the one RFC 6454 origin
// comparison every same-origin check in this binary performs. It started
// life unexported in internal/adapters/inbound/mcp (the /mcp Origin gate,
// technical plan §43.2) and moved here when a second caller needed the
// identical comparison -- the MCP consent page's own same-origin check on
// its POST (§43.14). Two copies of an origin comparison drift; one does
// not.

package platform

import (
	"fmt"
	"net/url"
	"strings"
)

// CanonicalOrigin parses rawURL (a configured base URL such as
// Config.PublicBaseURL, or an incoming request's own Origin header value)
// into its CANONICAL origin -- scheme + host + non-default port, no path --
// the one string two origins are compared by. Per RFC 6454 section 4/section 5 ("Origin
// of a URI", "Serializing an Origin"), scheme and host compare
// case-insensitively, and an absent port is exactly the scheme's own
// default port made explicit (":80" for http, ":443" for https): so
// "HTTP://EXAMPLE.test" and "http://example.test:80" both canonicalize to
// "http://example.test". An IPv6 literal host is lower-cased and
// re-bracketed, the shape url.Parse and net/http.CrossOriginProtection
// expect back. Returns an error for anything that is not an absolute URL
// with a scheme and a host.
func CanonicalOrigin(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("not an absolute URL (missing scheme or host): %q", rawURL)
	}
	return canonicalOrigin(u.Scheme, u.Hostname(), u.Port()), nil
}

// defaultPortFor returns scheme's own default port ("" for a scheme this
// function does not know, which canonicalOrigin then never strips).
func defaultPortFor(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// canonicalOrigin serializes scheme/hostname/port per CanonicalOrigin's
// own doc comment. hostname must already be bracket-free
// (url.URL.Hostname()'s own contract).
func canonicalOrigin(scheme, hostname, port string) string {
	scheme = strings.ToLower(scheme)
	host := strings.ToLower(hostname)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" && port != defaultPortFor(scheme) {
		host += ":" + port
	}
	return scheme + "://" + host
}
