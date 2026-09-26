// Package mcpclient holds the registration and redirect rules for the MCP
// authorization server's OAuth clients (technical plan §43.15) -- pure
// functions, no I/O, shared by every place that must agree on them: the
// three ways a client is registered -- an administrator's route, a
// fetched client ID metadata document, a dynamic registration request
// (registration.go) -- so a URI or name one of them would refuse is never
// stored by another, and GET /oauth/authorize, which matches a presented
// redirect URI against what was stored.
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
	// MaxConsecutiveCombiningMarks bounds how many nonspacing or enclosing
	// marks (Unicode categories Mn, Me) a client name may stack on one
	// character. Written text needs a few -- a Vietnamese vowel with two
	// diacritics, a Thai consonant with a vowel and a tone mark, a keycap
	// -- while dozens stacked on one letter paint far outside the name's
	// own line, over the consent page's identity headline.
	MaxConsecutiveCombiningMarks = 3
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

// Host refusals, each wrapped -- beside the refusing validator's own
// error -- by every refusal of a URI whose host is not written in plain
// ASCII (plainASCIIAuthority).
var (
	// ErrPercentEncodedHost: the URI's authority carries a percent sign.
	ErrPercentEncodedHost = errors.New("the host must not be percent-encoded (an internationalized host is written in its xn-- form)")
	// ErrHostNotAsWritten: the host a parse yields is not the authority's
	// own bytes, or is not printable ASCII.
	ErrHostNotAsWritten = errors.New("the host must be plain ASCII, exactly as written")
)

// plainASCIIAuthority checks that raw -- a URI already checked to be
// printable ASCII throughout -- writes its authority in plain ASCII, with
// u its parse: no percent sign anywhere in the authority, the parsed host
// exactly the authority's own bytes, and that host printable ASCII.
//
// A raw-byte check alone is not enough: url.Parse decodes a
// percent-encoded byte of 0x80 or above in a host, so
// "https://%D0%B0lpha.example/" -- ASCII as written -- parses to a host
// beginning with a Cyrillic letter, which net/http then converts to its
// IDNA ASCII form (xn--lpha-43d.example) to resolve, dial and verify.
// Shown decoded, that host reads as another one while the request goes to
// a third. With this rule the host every page shows -- the consent
// page's identity headline, its redirect line, the homepage line, the
// audit row -- is byte for byte the string a fetch resolves, and an
// internationalized host is only ever written, stored and shown in its
// xn-- form. The percent-sign refusal is the rule; the two checks after
// it hold the property should a parser ever rewrite a host some other
// way.
func plainASCIIAuthority(raw string, u *url.URL) error {
	i := strings.Index(raw, "://")
	if i < 0 {
		return ErrHostNotAsWritten
	}
	authority := raw[i+len("://"):]
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	if strings.Contains(authority, "%") {
		return ErrPercentEncodedHost
	}
	if authority != u.Host {
		return ErrHostNotAsWritten
	}
	for j := 0; j < len(u.Host); j++ {
		if u.Host[j] <= 0x20 || u.Host[j] >= 0x7f {
			return ErrHostNotAsWritten
		}
	}
	return nil
}

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
	if err := plainASCIIAuthority(raw, u); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRedirectURI, err)
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
// registrable redirect URI. It is always plain ASCII, exactly as the URI
// writes it (plainASCIIAuthority): an internationalized host shows in its
// xn-- form, never decoded into a look-alike.
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
// MaxClientNameRunes runes, valid UTF-8 and printable throughout, with at
// most MaxConsecutiveCombiningMarks nonspacing or enclosing marks in a
// row, returning the trimmed value. Printable is unicode.IsPrint: letters,
// marks, numbers, punctuation, symbols and the ASCII space -- so control
// and format characters (Unicode category Cf: bidirectional overrides,
// zero-width joiners and spaces), line and paragraph separators, every
// space but U+0020, and private-use and unassigned code points are all
// refused. The name is what the consent page shows a user deciding whether
// to trust the client, and for a client that registered itself (§43.15)
// it is chosen by whoever registered it: a right-to-left override, an
// invisible character or a glyph no font defines is a way to make one
// name read as another, and a letter carrying a tall stack of combining
// marks is a way to draw over the identity shown above the name. The same
// rule holds for every client kind, so a name any registration path
// stores is one the consent page can show.
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
	marks := 0
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return "", errors.New("client name contains a control, invisible formatting or otherwise unprintable character")
		}
		if !unicode.In(r, unicode.Mn, unicode.Me) {
			marks = 0
			continue
		}
		if marks++; marks > MaxConsecutiveCombiningMarks {
			return "", fmt.Errorf("client name stacks more than %d combining marks on one character", MaxConsecutiveCombiningMarks)
		}
	}
	return name, nil
}

// ValidateClientURI checks an optional client homepage: an absolute https
// URI with a host, no userinfo, printable ASCII only, its host written in
// plain ASCII (plainASCIIAuthority).
func ValidateClientURI(raw string) error {
	_, err := parseClientURI(raw)
	return err
}

// parseClientURI applies ValidateClientURI's rules and returns the parsed
// URI.
func parseClientURI(raw string) (*url.URL, error) {
	if len(raw) > MaxClientURILength {
		return nil, fmt.Errorf("client URI is longer than %d bytes", MaxClientURILength)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 || raw[i] >= 0x7f {
			return nil, errors.New("client URI contains a space, control or non-ASCII character")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("client URI must be an absolute https URI with a host")
	}
	if err := plainASCIIAuthority(raw, u); err != nil {
		return nil, fmt.Errorf("client URI: %w", err)
	}
	return u, nil
}

// ClientURIHost is the host (no port) of a client homepage URI -- what the
// consent page shows on its homepage line -- or "" when raw is not a valid
// client URI (ValidateClientURI). Like RedirectHost, always plain ASCII,
// exactly as the URI writes it.
func ClientURIHost(raw string) string {
	u, err := parseClientURI(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
