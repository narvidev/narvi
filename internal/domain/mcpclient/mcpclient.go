// Package mcpclient holds the registration and redirect rules for the MCP
// authorization server's OAuth clients (technical plan §43.15) -- pure
// functions, no I/O, shared by the two places that must agree on them:
// the admin route that registers a client (a URI it would later refuse is
// never stored) and GET /oauth/authorize (a presented redirect URI is
// matched against what was stored).
//
// # The redirect URI rule
//
// Registration accepts exactly three shapes: https:// with any host;
// http:// on a loopback IP literal (127.0.0.1 or [::1]); and
// http://localhost. Anything else -- another http host, a custom scheme, a
// fragment, userinfo, a relative or opaque URI, a non-printable or
// non-ASCII byte -- is refused. Matching is exact string comparison, with
// one exception taken from RFC 8252 section 7.3: for a registered loopback IP
// literal (never "localhost"), the port of the presented URI is ignored,
// because a native client picks a free port at run time. Everything else
// about that presented URI -- scheme, host, path, query -- must still be
// byte-identical.
package mcpclient

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Registration bounds. Plain counts, not durations.
const (
	// MaxRedirectURIs bounds how many redirect URIs one client registers.
	MaxRedirectURIs = 10
	// MaxRedirectURILength bounds one redirect URI, in bytes.
	MaxRedirectURILength = 2048
	// MaxClientNameRunes bounds a client's display name.
	MaxClientNameRunes = 100
	// MaxClientURILength bounds a client's optional homepage URI.
	MaxClientURILength = 2048
)

// loopbackIPHosts are the loopback IP literals whose port a presented
// redirect URI may vary (url.URL.Hostname() form, brackets stripped).
var loopbackIPHosts = map[string]bool{
	"127.0.0.1": true,
	"::1":       true,
}

// ErrInvalidRedirectURI is wrapped by every ValidateRedirectURI refusal.
var ErrInvalidRedirectURI = errors.New("invalid redirect URI")

// parseRedirectURI applies every shape rule and returns the parsed URI.
func parseRedirectURI(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("%w: empty", ErrInvalidRedirectURI)
	}
	if len(raw) > MaxRedirectURILength {
		return nil, fmt.Errorf("%w: longer than %d bytes", ErrInvalidRedirectURI, MaxRedirectURILength)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 || raw[i] >= 0x7f {
			return nil, fmt.Errorf("%w: contains a space, control or non-ASCII character", ErrInvalidRedirectURI)
		}
	}
	if strings.Contains(raw, "#") {
		return nil, fmt.Errorf("%w: must not contain a fragment", ErrInvalidRedirectURI)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: does not parse", ErrInvalidRedirectURI)
	}
	if !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("%w: must be an absolute URI with a host", ErrInvalidRedirectURI)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: must not carry user information", ErrInvalidRedirectURI)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%w: invalid port", ErrInvalidRedirectURI)
		}
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		host := u.Hostname()
		if loopbackIPHosts[host] || host == "localhost" {
			return u, nil
		}
		return nil, fmt.Errorf("%w: http is allowed only for 127.0.0.1, [::1] or localhost; use https", ErrInvalidRedirectURI)
	default:
		return nil, fmt.Errorf("%w: scheme %q is not allowed (https, or http on loopback)", ErrInvalidRedirectURI, u.Scheme)
	}
}

// ValidateRedirectURI reports whether raw may be registered as a redirect
// URI (this package's own doc comment has the rule).
func ValidateRedirectURI(raw string) error {
	_, err := parseRedirectURI(raw)
	return err
}

// MatchRedirectURI reports whether presented -- the redirect_uri an
// authorization request carries -- matches one of registered: exact string
// equality, or, for a registered http loopback IP literal, equality of
// everything but the port. presented must itself satisfy the registration
// shape rules; a presented URI that could never have been registered
// matches nothing.
func MatchRedirectURI(registered []string, presented string) bool {
	p, err := parseRedirectURI(presented)
	if err != nil {
		return false
	}
	for _, r := range registered {
		if r == presented {
			return true
		}
		if loopbackIPMatchIgnoringPort(r, p) {
			return true
		}
	}
	return false
}

// loopbackIPMatchIgnoringPort reports whether registered is an http
// loopback IP literal URI equal to p in every component but the port.
func loopbackIPMatchIgnoringPort(registered string, p *url.URL) bool {
	r, err := parseRedirectURI(registered)
	if err != nil {
		return false
	}
	if r.Scheme != "http" || !loopbackIPHosts[r.Hostname()] {
		return false
	}
	return p.Scheme == r.Scheme &&
		p.Hostname() == r.Hostname() &&
		p.EscapedPath() == r.EscapedPath() &&
		p.RawQuery == r.RawQuery &&
		p.ForceQuery == r.ForceQuery
}

// RedirectHost returns the host (no port) a redirect URI sends the user
// to -- what the consent page must show (the spec's "clearly display the
// redirect URI hostname") -- or "" when raw does not parse as a
// registrable redirect URI.
func RedirectHost(raw string) string {
	u, err := parseRedirectURI(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// IsLoopbackRedirect reports whether raw sends the user back to this
// same machine (127.0.0.1, [::1] or localhost) -- the case the consent
// page warns about, since any local program listening on that port
// receives the code.
func IsLoopbackRedirect(raw string) bool {
	u, err := parseRedirectURI(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return loopbackIPHosts[host] || host == "localhost"
}

// ValidateClientName trims name and checks it is non-empty, at most
// MaxClientNameRunes runes, valid UTF-8 and free of control and format
// characters, returning the trimmed value. Format characters (Unicode
// category Cf: bidirectional overrides, zero-width joiners and spaces) are
// refused because the name is what the consent page shows a user deciding
// whether to trust the client: a right-to-left override or an invisible
// character is a way to make one name read as another.
func ValidateClientName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("client name is required")
	}
	if !utf8.ValidString(name) {
		return "", errors.New("client name is not valid UTF-8")
	}
	if utf8.RuneCountInString(name) > MaxClientNameRunes {
		return "", fmt.Errorf("client name is longer than %d characters", MaxClientNameRunes)
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", errors.New("client name contains a control or invisible formatting character")
		}
	}
	return name, nil
}

// ValidateClientURI checks an optional client homepage: an absolute https
// URI with a host, no userinfo, printable ASCII only.
func ValidateClientURI(raw string) error {
	if len(raw) > MaxClientURILength {
		return fmt.Errorf("client URI is longer than %d bytes", MaxClientURILength)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 || raw[i] >= 0x7f {
			return errors.New("client URI contains a space, control or non-ASCII character")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.Hostname() == "" || u.User != nil {
		return errors.New("client URI must be an absolute https URI with a host")
	}
	return nil
}
