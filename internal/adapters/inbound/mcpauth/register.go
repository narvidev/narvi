package mcpauth

import (
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/narvidev/narvi/internal/adapters/outbound/cimdfetch"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/mcpclient"
	"github.com/narvidev/narvi/internal/platform"
)

// dynamicClientIDPrefix marks a dynamically registered client's public
// client_id (technical plan §43.15), as narvi_mcp_c_ marks a
// pre-registered one. An identifier, never a secret.
const dynamicClientIDPrefix = "narvi_mcp_d_"

// maxRegistrationBytes bounds a registration request body: the same cap
// a fetched metadata document gets, since the two describe a client with
// the same fields.
const maxRegistrationBytes = cimdfetch.MaxDocumentBytes

// RFC 7591 section 3.2.2 error codes.
const (
	errInvalidRedirectURI    = "invalid_redirect_uri"
	errInvalidClientMetadata = "invalid_client_metadata"
)

// The public-client registration every dynamically registered client
// gets, whatever it asked for (technical plan §43.15): no secret, the
// authorization-code grant with refresh tokens, the code response type.
var (
	registeredGrantTypes    = []string{"authorization_code", "refresh_token"}
	registeredResponseTypes = []string{"code"}
)

// registrationResponse is the RFC 7591 section 3.2.1 answer: everything
// registered, and nothing else -- no client_secret (public clients only),
// no registration_access_token or registration_client_uri (RFC 7592
// management is not offered: a client that wants other metadata registers
// again, and the unused one is swept).
type registrationResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// registrationRefusal maps a validation error to its RFC 7591 code and a
// fixed description -- never the client's own input echoed back.
func registrationRefusal(err error) (code, description string) {
	switch {
	case errors.Is(err, mcpclient.ErrInvalidRedirectURI):
		return errInvalidRedirectURI, "redirect_uris is required, and each must be https, or http on 127.0.0.1, [::1] or localhost, with no fragment"
	case errors.Is(err, mcpclient.ErrInvalidClientName):
		return errInvalidClientMetadata, "client_name is required, at most 100 printable characters"
	case errors.Is(err, mcpclient.ErrConfidentialClient):
		return errInvalidClientMetadata, "only public clients are registered: token_endpoint_auth_method must be none or absent"
	case errors.Is(err, mcpclient.ErrUnsupportedGrantType):
		return errInvalidClientMetadata, "grant_types must include authorization_code"
	case errors.Is(err, mcpclient.ErrUnsupportedResponseType):
		return errInvalidClientMetadata, "response_types must include code"
	default:
		return errInvalidClientMetadata, "the body must be a JSON object of client metadata"
	}
}

// Register backs POST /oauth/register (RFC 7591; technical plan §43.15):
// dynamic client registration, mounted behind the surface's enabled-gate,
// the dynamic-registration gate (off by default) and the per-address rate
// limit, and never behind the cookie middleware -- registration is
// unauthenticated by design, which is why it is braked and off by default.
// The request is validated exactly as a metadata document is
// (mcpclient.ParseRegistrationRequest); the server then forces a public
// client -- token_endpoint_auth_method none, the authorization_code and
// refresh_token grants, the code response type -- whatever else was
// asked, and answers 201 with the registration. application_type, a
// client_uri, a logo and every other field are accepted and ignored. No
// audit row: a registration grants nothing -- only a user's consent does,
// and that is audited. An unused registration is swept after
// MCPDynamicClientUnusedTTL.
func (s *Server) Register(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeTokenJSON(w, http.StatusBadRequest, tokenError{Error: errInvalidClientMetadata, ErrorDescription: "the body must be application/json"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRegistrationBytes))
	if err != nil {
		writeTokenJSON(w, http.StatusBadRequest, tokenError{Error: errInvalidClientMetadata, ErrorDescription: "the body could not be read, or is larger than 64 KiB"})
		return
	}
	md, err := mcpclient.ParseRegistrationRequest(body)
	if err != nil {
		code, description := registrationRefusal(err)
		logger.Warn("mcpauth: registration refused", "outcome", code, "error", err)
		writeTokenJSON(w, http.StatusBadRequest, tokenError{Error: code, ErrorDescription: description})
		return
	}

	suffix, err := platform.GenerateToken()
	if err != nil {
		logger.Error("mcpauth: register: generate client id failed", "error", err)
		writeTokenError(w, errServerError, "", false)
		return
	}
	row, err := s.deps.Clients.Create(ctx, sqlcgen.CreateMCPOAuthClientParams{
		ClientID:     dynamicClientIDPrefix + suffix,
		Kind:         sqlcgen.McpOauthClientKindDynamic,
		ClientName:   md.ClientName,
		RedirectUris: md.RedirectURIs,
	})
	if err != nil {
		logger.Error("mcpauth: register: insert client failed", "error", err)
		writeTokenError(w, errServerError, "", false)
		return
	}
	logger.Info("mcpauth: client registered dynamically", "client_id", row.ClientID)
	writeTokenJSON(w, http.StatusCreated, registrationResponse{
		ClientID:                row.ClientID,
		ClientIDIssuedAt:        row.CreatedAt.Time.Unix(),
		ClientName:              row.ClientName,
		RedirectURIs:            row.RedirectUris,
		GrantTypes:              registeredGrantTypes,
		ResponseTypes:           registeredResponseTypes,
		TokenEndpointAuthMethod: "none",
	})
}
