package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// maxResponseBodySize bounds how much of any GitHub API response this
// client ever reads -- mirrors internal/sandboxagent/credentials.CPClient's
// own maxScmCredentialsResponseSize/internal/adapters/outbound/modal's own
// maxResponseBodySize precedent: an http.Client.Timeout bounds wall-clock
// time, not response size, so an unbounded read could exhaust memory
// against a misbehaving or compromised endpoint.
const maxResponseBodySize = 1 << 20 // 1 MiB

// Token is what MintInstallationToken returns: the credential itself, its
// real expiry as GitHub reported it, and the permissions GitHub actually
// granted -- which internal/domain/scmscope.ValidateReadOnly then checks
// (§30.4(4)'s own per-mint scope introspection) before this value ever
// reaches a sandbox.
type Token struct {
	Value       string
	ExpiresAt   time.Time
	Permissions map[string]string
}

// Client mints GitHub App installation access tokens and introspects the
// App's own granted permissions -- see doc.go for the full design.
type Client struct {
	httpClient *http.Client
	baseURL    string
	appID      int64
	privateKey *rsa.PrivateKey
	jwtTTL     time.Duration
	clockSkew  time.Duration
}

// New builds a Client. httpClient's own Timeout should be left unset (or
// generous): each call below is bounded by the ctx its own caller supplies
// (mirroring internal/adapters/outbound/githubapi's own "individual call
// deadlines come from each caller's own context" convention), via
// platform.Timeouts.GitHubAppMintTimeout/GitHubAppScopeCheckTimeout.
// jwtTTL is platform.Timeouts.GitHubAppJWTTTL, threaded through rather than
// read from a package-level default -- every duration literal lives in
// platform/timeouts.go, never here.
func New(httpClient *http.Client, baseURL string, appID int64, privateKey *rsa.PrivateKey, jwtTTL, clockSkew time.Duration) *Client {
	return &Client{
		httpClient: httpClient,
		baseURL:    baseURL,
		appID:      appID,
		privateKey: privateKey,
		jwtTTL:     jwtTTL,
		clockSkew:  clockSkew,
	}
}

// readOnlyMintPermissions is the exact, fixed permission set §30.4
// mandates the mint request: "installation-token minting scoped
// contents:read (+ metadata:read)". Requesting exactly these two --
// never a caller-supplied set -- means the only way this client ever asks
// GitHub for more than read access is a change to this literal, reviewed
// like any other code change; nothing about a single mint call can widen
// it.
var readOnlyMintPermissions = map[string]string{
	"contents": "read",
	"metadata": "read",
}

// doAppRequest issues one GitHub REST request authenticated as the App
// itself (a fresh, short-lived JWT -- see jwt.go), and decodes a 2xx JSON
// response into out. A non-2xx status is a plain error naming only the
// status code, never the response body -- mirrors CPClient.Fetch's own
// documented reasoning: a validation-failure body can echo request/secret
// data back verbatim, and this error can end up logged.
func (c *Client) doAppRequest(ctx context.Context, method, path string, body any, out any) error {
	jwtToken, err := signAppJWT(c.appID, c.privateKey, c.jwtTTL, c.clockSkew, time.Now())
	if err != nil {
		return err
	}

	var bodyReader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("githubapp: encode request body: %w", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("githubapp: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("githubapp: request %s %s failed: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return fmt.Errorf("githubapp: read response for %s %s: %w", method, path, err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return &statusError{method: method, path: path, status: resp.StatusCode}
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("githubapp: decode response for %s %s: %w", method, path, err)
	}
	return nil
}

// statusError is doAppRequest's non-2xx outcome: the request and the
// status code, never the response body (see doAppRequest). Typed only so
// MintInstallationToken can tell a missing installation (404 on the
// installation lookup) from every other failure.
type statusError struct {
	method string
	path   string
	status int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("githubapp: %s %s returned http %d", e.method, e.path, e.status)
}

// InstallationNotFoundError reports that the GitHub App has no
// installation covering Owner/Repo: GitHub answered the installation
// lookup (GET /repos/{owner}/{repo}/installation) with 404. It is a
// configuration state, not an outage -- the App has to be installed on that
// repository (on its owner's account) before a token can be minted for it
// -- so a caller can name the repository and the remedy instead of
// reporting a bare failure.
type InstallationNotFoundError struct {
	Owner string
	Repo  string
}

func (e *InstallationNotFoundError) Error() string {
	return fmt.Sprintf("githubapp: the GitHub App is not installed on %s/%s", e.Owner, e.Repo)
}

// appPermissionsResponse mirrors the subset of GitHub's real GET /app
// response this client actually reads.
type appPermissionsResponse struct {
	Permissions map[string]string `json:"permissions"`
}

// AppPermissions returns the configured App's own maximum granted
// permissions (GET /app) -- cmd/control-plane's own boot-time scope check
// (§30.4(4)) calls this once, before this process ever starts serving
// traffic, and refuses to boot unless internal/domain/scmscope.
// ValidateReadOnly accepts the result.
func (c *Client) AppPermissions(ctx context.Context) (map[string]string, error) {
	var resp appPermissionsResponse
	if err := c.doAppRequest(ctx, http.MethodGet, "/app", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Permissions, nil
}

// installationResponse mirrors the subset of GitHub's real GET
// /repos/{owner}/{repo}/installation response this client actually reads.
type installationResponse struct {
	ID int64 `json:"id"`
}

// accessTokenRequest is the body MintInstallationToken POSTs to
// /app/installations/{id}/access_tokens -- GitHub's own documented
// "create an installation access token" request shape.
type accessTokenRequest struct {
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

// accessTokenResponse mirrors the subset of GitHub's real access-token
// mint response this client actually reads.
type accessTokenResponse struct {
	TokenValue  string            `json:"token"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Permissions map[string]string `json:"permissions"`
}

// MintInstallationToken resolves owner's own GitHub App installation
// (via the first of repoNames -- an installation is per-account, so any
// one of the session's own repos under owner names the same installation
// every other one does) and mints that installation's own access token,
// explicitly narrowed to readOnlyMintPermissions and to exactly
// repoNames. GitHub can only ever narrow the returned permissions further
// than requested (an installation itself granted less), never widen them
// -- but the CALLER (internal/adapters/inbound/httpapi.ScmCredentials)
// still runs internal/domain/scmscope.ValidateReadOnly against
// Token.Permissions before trusting it, exactly as §30.4(4) requires,
// rather than trusting this request shape alone.
//
// owner and every entry in repoNames are passed straight into a URL path
// segment (url.PathEscape) -- both already come from
// internal/domain/reposource.ParseOwnerRepo's own validated output at this
// package's one call site, never raw session input reaching here
// unvalidated.
func (c *Client) MintInstallationToken(ctx context.Context, owner string, repoNames []string) (Token, error) {
	if len(repoNames) == 0 {
		return Token{}, fmt.Errorf("githubapp: mint installation token: at least one repo name is required")
	}

	installationPath := fmt.Sprintf("/repos/%s/%s/installation", url.PathEscape(owner), url.PathEscape(repoNames[0]))
	var installation installationResponse
	if err := c.doAppRequest(ctx, http.MethodGet, installationPath, nil, &installation); err != nil {
		var status *statusError
		if errors.As(err, &status) && status.status == http.StatusNotFound {
			return Token{}, &InstallationNotFoundError{Owner: owner, Repo: repoNames[0]}
		}
		return Token{}, fmt.Errorf("githubapp: resolve installation for %s/%s: %w", owner, repoNames[0], err)
	}
	if installation.ID == 0 {
		return Token{}, fmt.Errorf("githubapp: resolve installation for %s/%s: no installation id in response", owner, repoNames[0])
	}

	mintPath := fmt.Sprintf("/app/installations/%d/access_tokens", installation.ID)
	reqBody := accessTokenRequest{Repositories: repoNames, Permissions: readOnlyMintPermissions}
	var resp accessTokenResponse
	if err := c.doAppRequest(ctx, http.MethodPost, mintPath, reqBody, &resp); err != nil {
		return Token{}, fmt.Errorf("githubapp: mint installation token for installation %d: %w", installation.ID, err)
	}
	if resp.TokenValue == "" {
		return Token{}, fmt.Errorf("githubapp: mint installation token for installation %d: empty token in response", installation.ID)
	}

	return Token{Value: resp.TokenValue, ExpiresAt: resp.ExpiresAt, Permissions: resp.Permissions}, nil
}
