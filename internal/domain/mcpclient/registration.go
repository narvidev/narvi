package mcpclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Kind is how a client came to be registered -- Postgres
// mcp_oauth_client_kind exactly (migrations/000141_mcp_oauth.up.sql).
type Kind string

// The three client kinds (technical plan §43.15).
const (
	// KindPreregistered: an administrator registered it.
	KindPreregistered Kind = "preregistered"
	// KindDynamic: it registered itself through RFC 7591 dynamic client
	// registration.
	KindDynamic Kind = "dynamic"
	// KindMetadataDocument: it is identified by an https URL, and its
	// registration is the client ID metadata document at that URL.
	KindMetadataDocument Kind = "metadata_document"
)

// Mechanisms is which registration mechanisms a deployment accepts beside
// pre-registration (platform.Config.MCPCIMDEnabled/MCPDCREnabled).
type Mechanisms struct {
	MetadataDocuments   bool
	DynamicRegistration bool
}

// Accepts reports whether a client of kind may take part in an
// authorization at all -- be authorized, be consented to, be issued or
// refresh a token, or authenticate a /mcp call. Pre-registration is always
// accepted. A client registered through a mechanism the deployment has
// switched off is refused everywhere, exactly like a disabled client, so
// switching a mechanism off is a kill switch for every client it
// registered, not only for new registrations. An unknown kind is refused.
func (m Mechanisms) Accepts(kind Kind) bool {
	switch kind {
	case KindPreregistered:
		return true
	case KindMetadataDocument:
		return m.MetadataDocuments
	case KindDynamic:
		return m.DynamicRegistration
	default:
		return false
	}
}

// MaxClientIDURLLength bounds a metadata-document client_id, in bytes.
const MaxClientIDURLLength = 2048

// ErrInvalidClientIDURL is wrapped by every ValidateClientIDURL refusal.
var ErrInvalidClientIDURL = errors.New("invalid client ID metadata document URL")

// IsClientIDURL reports whether a presented client_id is shaped as a
// metadata-document client (it starts with "https://") -- the routing
// decision only; ValidateClientIDURL decides whether it is a valid one. No
// other kind's client_id can start that way: pre-registered and dynamic
// client ids are generated with their own narvi_mcp_ prefixes.
func IsClientIDURL(clientID string) bool {
	return strings.HasPrefix(clientID, "https://")
}

// ValidateClientIDURL checks a metadata-document client_id: printable
// ASCII throughout, and its host written in plain ASCII -- no
// percent-encoding in the authority, the parsed host exactly the bytes
// written (plainASCIIAuthority) -- so an internationalized host is only
// ever seen, and shown, in its xn-- form, the very string the fetch
// resolves, never decoded into a look-alike; at most MaxClientIDURLLength
// bytes, the literal scheme "https://", a host, a path other than "/", no
// single- or double-dot path segment (encoded or not), no userinfo, no
// fragment. A query and a port are allowed, as the client ID metadata
// document draft allows them. The URL is the client's identity and is
// compared byte for byte, so nothing here normalizes it.
func ValidateClientIDURL(raw string) error {
	if len(raw) > MaxClientIDURLLength {
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidClientIDURL, MaxClientIDURLLength)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 || raw[i] >= 0x7f {
			return fmt.Errorf("%w: contains a space, control or non-ASCII character", ErrInvalidClientIDURL)
		}
	}
	if !IsClientIDURL(raw) {
		return fmt.Errorf("%w: must start with https://", ErrInvalidClientIDURL)
	}
	if strings.Contains(raw, "#") {
		return fmt.Errorf("%w: must not contain a fragment", ErrInvalidClientIDURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: does not parse", ErrInvalidClientIDURL)
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return fmt.Errorf("%w: must be an absolute https URL with a host", ErrInvalidClientIDURL)
	}
	if u.User != nil {
		return fmt.Errorf("%w: must not carry user information", ErrInvalidClientIDURL)
	}
	if err := plainASCIIAuthority(raw, u); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidClientIDURL, err)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%w: invalid port", ErrInvalidClientIDURL)
		}
	}
	if u.Path == "" || u.Path == "/" {
		return fmt.Errorf("%w: must have a path", ErrInvalidClientIDURL)
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("%w: must not contain a dot segment", ErrInvalidClientIDURL)
		}
	}
	return nil
}

// IdentityHost is what the consent page shows as a metadata-document
// client's identity: the host of its client_id URL, with its port when it
// names one -- the one thing about the client its own document cannot
// choose, since the document was fetched from that very origin over TLS
// (the fetcher follows a redirect only within the client_id URL's own
// scheme, host and port). Always plain ASCII, byte for byte as the URL
// writes it and as the fetch resolves it (ValidateClientIDURL). "" when
// raw is not a valid client ID URL.
func IdentityHost(clientIDURL string) string {
	if ValidateClientIDURL(clientIDURL) != nil {
		return ""
	}
	u, err := url.Parse(clientIDURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// Registration refusals. Every ParseMetadataDocument and
// ParseRegistrationRequest error wraps exactly one of these, or
// ErrInvalidRedirectURI.
var (
	// ErrMalformedMetadata: not a JSON object, or a known field of the
	// wrong JSON type.
	ErrMalformedMetadata = errors.New("client metadata is not a JSON object with the expected field types")
	// ErrClientIDMismatch: a metadata document's client_id is not, byte
	// for byte, the URL it was fetched from.
	ErrClientIDMismatch = errors.New("the metadata document's client_id is not the URL it was fetched from")
	// ErrInvalidClientName: client_name is missing or refused by
	// ValidateClientName.
	ErrInvalidClientName = errors.New("invalid client_name")
	// ErrConfidentialClient: token_endpoint_auth_method asks for a client
	// secret or key; every client here is public.
	ErrConfidentialClient = errors.New("only public clients (token_endpoint_auth_method none) are supported")
	// ErrUnsupportedGrantType: grant_types does not include
	// authorization_code.
	ErrUnsupportedGrantType = errors.New("grant_types must include authorization_code")
	// ErrUnsupportedResponseType: response_types does not include code.
	ErrUnsupportedResponseType = errors.New("response_types must include code")
)

// Metadata is what a registration -- a fetched metadata document or a
// dynamic registration request -- registers: all the server keeps of it.
// Every other field the client sent is ignored (client_uri and logo_uri
// included: a homepage or logo the client chose itself would only lend a
// self-chosen name more credibility on the consent page).
type Metadata struct {
	// ClientName is trimmed and satisfies ValidateClientName.
	ClientName string
	// RedirectURIs are each valid under ValidateRedirectURI, deduplicated,
	// in the order given.
	RedirectURIs []string
}

// ParseMetadataDocument validates a fetched client ID metadata document
// against the URL it was fetched from (technical plan §43.15): a JSON
// object whose client_id equals clientIDURL BYTE FOR BYTE -- no case
// folding, no normalization, no trailing-slash tolerance, since the URL
// is the client's identity -- and whose other fields pass the rules
// ParseRegistrationRequest applies. Field names match exactly
// (encoding/json's case-insensitive matching would let "Client_ID" stand
// in for client_id); unknown fields are ignored.
func ParseMetadataDocument(clientIDURL string, body []byte) (Metadata, error) {
	fields, err := jsonObject(body)
	if err != nil {
		return Metadata{}, err
	}
	clientID, present, err := stringField(fields, "client_id")
	if err != nil {
		return Metadata{}, err
	}
	if !present || clientID != clientIDURL {
		return Metadata{}, ErrClientIDMismatch
	}
	return clientMetadata(fields)
}

// ParseRegistrationRequest validates an RFC 7591 dynamic client
// registration request (technical plan §43.15) by the same rules as a
// metadata document, minus the client_id, which a registering client
// does not choose (one it sends is ignored).
func ParseRegistrationRequest(body []byte) (Metadata, error) {
	fields, err := jsonObject(body)
	if err != nil {
		return Metadata{}, err
	}
	return clientMetadata(fields)
}

// clientMetadata applies the rules both registration paths share:
// client_name required and displayable; redirect_uris required, between
// one and MaxRedirectURIs, each registrable; token_endpoint_auth_method
// absent or "none"; grant_types absent or including authorization_code;
// response_types absent or including code.
func clientMetadata(fields map[string]json.RawMessage) (Metadata, error) {
	rawName, present, err := stringField(fields, "client_name")
	if err != nil {
		return Metadata{}, err
	}
	if !present {
		return Metadata{}, fmt.Errorf("%w: client_name is required", ErrInvalidClientName)
	}
	name, err := ValidateClientName(rawName)
	if err != nil {
		return Metadata{}, fmt.Errorf("%w: %v", ErrInvalidClientName, err)
	}

	uris, present, err := stringsField(fields, "redirect_uris")
	if err != nil {
		return Metadata{}, err
	}
	if !present || len(uris) == 0 {
		return Metadata{}, fmt.Errorf("%w: redirect_uris is required", ErrInvalidRedirectURI)
	}
	if len(uris) > MaxRedirectURIs {
		return Metadata{}, fmt.Errorf("%w: at most %d redirect URIs", ErrInvalidRedirectURI, MaxRedirectURIs)
	}
	seen := make(map[string]bool, len(uris))
	redirects := make([]string, 0, len(uris))
	for _, uri := range uris {
		if err := ValidateRedirectURI(uri); err != nil {
			return Metadata{}, err
		}
		if !seen[uri] {
			seen[uri] = true
			redirects = append(redirects, uri)
		}
	}

	method, present, err := stringField(fields, "token_endpoint_auth_method")
	if err != nil {
		return Metadata{}, err
	}
	if present && method != "none" {
		return Metadata{}, fmt.Errorf("%w: got %q", ErrConfidentialClient, method)
	}
	if err := requireListed(fields, "grant_types", "authorization_code", ErrUnsupportedGrantType); err != nil {
		return Metadata{}, err
	}
	if err := requireListed(fields, "response_types", "code", ErrUnsupportedResponseType); err != nil {
		return Metadata{}, err
	}
	return Metadata{ClientName: name, RedirectURIs: redirects}, nil
}

// requireListed: field absent, or a list of strings containing want.
func requireListed(fields map[string]json.RawMessage, field, want string, refusal error) error {
	values, present, err := stringsField(fields, field)
	if err != nil || !present {
		return err
	}
	for _, v := range values {
		if v == want {
			return nil
		}
	}
	return fmt.Errorf("%w: got %q", refusal, values)
}

// jsonObject decodes body as one JSON object, keyed by exact field name.
func jsonObject(body []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return nil, ErrMalformedMetadata
	}
	if dec.More() {
		return nil, ErrMalformedMetadata
	}
	return fields, nil
}

// stringField reads a string field; a JSON null counts as absent.
func stringField(fields map[string]json.RawMessage, name string) (string, bool, error) {
	raw, ok := fields[name]
	if !ok || string(raw) == "null" {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, fmt.Errorf("%w: %s must be a string", ErrMalformedMetadata, name)
	}
	return s, true, nil
}

// stringsField reads an array-of-strings field; a JSON null counts as
// absent.
func stringsField(fields map[string]json.RawMessage, name string) ([]string, bool, error) {
	raw, ok := fields[name]
	if !ok || string(raw) == "null" {
		return nil, false, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, false, fmt.Errorf("%w: %s must be an array of strings", ErrMalformedMetadata, name)
	}
	return list, true, nil
}

// MetadataCacheTTL is how long a fetched metadata document is trusted
// before it is re-fetched (technical plan §43.15): ceiling
// (platform.Timeouts.MCPClientMetadataCacheTTL), shortened -- and only
// shortened -- by the lifetime the document's own response gave it
// (Cache-Control max-age, zero for no-store/no-cache). A response can
// make Narvi re-fetch sooner, never trust a document longer.
func MetadataCacheTTL(ceiling, maxAge time.Duration, hasMaxAge bool) time.Duration {
	if hasMaxAge && maxAge < ceiling {
		if maxAge < 0 {
			return 0
		}
		return maxAge
	}
	return ceiling
}
