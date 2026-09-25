package mcpauth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/narvidev/narvi/internal/platform"
)

// OAuth error codes this server answers with (RFC 6749 §4.1.2.1/§5.2,
// RFC 8707 §2).
const (
	errInvalidRequest          = "invalid_request"
	errUnsupportedResponseType = "unsupported_response_type"
	errInvalidScope            = "invalid_scope"
	errInvalidTarget           = "invalid_target"
	errAccessDenied            = "access_denied"
	errServerError             = "server_error"
	errInvalidClient           = "invalid_client"
	errInvalidGrant            = "invalid_grant"
	errUnsupportedGrantType    = "unsupported_grant_type"
)

// setPageHeaders sets the headers every HTML page this package renders
// carries (technical plan §43.14): no framing, no caching, no referrer,
// nothing loaded from anywhere, and forms may submit only where
// formAction allows.
func setPageHeaders(w http.ResponseWriter, formAction string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action "+formAction+"; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}

// errorPage is error.html's own data.
type errorPage struct {
	Title   string
	Message string
}

// renderError renders error.html at status. It is the ONLY answer to a
// request whose client_id or redirect_uri could not be validated: such a
// request is never redirected anywhere (technical plan §43.14). Title and
// Message are fixed strings chosen by the caller, never request input.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	var buf bytes.Buffer
	if err := s.errorTmpl.Execute(&buf, errorPage{Title: title, Message: message}); err != nil {
		platform.Logger(r.Context()).Error("mcpauth: render error page failed", "error", err)
		buf.Reset()
		buf.WriteString("error")
	}
	setPageHeaders(w, "'none'")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// redirectToClient sends the browser to redirectURI -- which the caller
// has already validated against the client's registration, or read back
// from a stored request -- with params appended to its query (any query
// the registered URI already carries is kept verbatim, RFC 6749 §3.1.2)
// plus iss (RFC 9207), on success and error alike.
func (s *Server) redirectToClient(w http.ResponseWriter, r *http.Request, redirectURI string, params url.Values) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		// Unreachable: redirectURI passed mcpclient's own parse rules
		// before it was ever stored or accepted. Refused, never redirected.
		platform.Logger(r.Context()).Error("mcpauth: validated redirect URI failed to parse", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The authorization could not be completed.")
		return
	}
	params.Set("iss", s.ids.Issuer)
	extra := params.Encode()
	if u.RawQuery == "" {
		u.RawQuery = extra
	} else {
		u.RawQuery = strings.TrimSuffix(u.RawQuery, "&") + "&" + extra
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// redirectError redirects to a validated redirect URI with an OAuth
// error. state is echoed only when the caller passes it.
func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, redirectURI, code, description, state string) {
	params := url.Values{}
	params.Set("error", code)
	if description != "" {
		params.Set("error_description", description)
	}
	if state != "" {
		params.Set("state", state)
	}
	s.redirectToClient(w, r, redirectURI, params)
}

// tokenError is the token endpoint's own JSON error body (RFC 6749 §5.2).
type tokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// writeTokenError answers the token endpoint with an OAuth error: 400,
// or 401 with a Basic challenge for invalid_client when the client
// attempted HTTP Basic authentication (RFC 6749 §5.2). description is a
// fixed string chosen by the caller -- never a code, token or verifier.
func writeTokenError(w http.ResponseWriter, code, description string, basicAttempted bool) {
	status := http.StatusBadRequest
	if code == errInvalidClient && basicAttempted {
		status = http.StatusUnauthorized
		w.Header().Set("WWW-Authenticate", `Basic realm="narvi-mcp"`)
	}
	if code == errServerError {
		status = http.StatusInternalServerError
	}
	writeTokenJSON(w, status, tokenError{Error: code, ErrorDescription: description})
}

// writeTokenJSON writes v with the token endpoint's own headers (RFC 6749
// §5.1: never cached).
func writeTokenJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
