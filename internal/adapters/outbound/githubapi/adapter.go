package githubapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/contractdrift"
)

// escapePathSegments url.PathEscape's EACH "/"-delimited segment of p,
// then rejoins them with "/" -- for a value like spec.Path (Environment.
// ContractsPath, §14.3) that is itself a real, multi-segment repo-relative
// directory path (e.g. "services/mock-api/contracts"), rather than a
// single opaque path component the way Owner/Repo/a branch name are.
// Escaping the WHOLE string as one blob via a single url.PathEscape call
// would percent-encode its own "/" separators too, turning a real,
// multi-segment directory path into a single bogus path segment GitHub's
// Contents API would never resolve -- so each segment is escaped on its
// own instead, exactly like GitHub's own Contents API documentation
// expects a multi-segment path to be built. Audit remediation
// (security-crosscutting lens): every outbound adapter in this codebase
// that builds a request path from caller-controlled string fields already
// escapes them this way (internal/adapters/inbound/auth/callback.go's own
// checkOrgMembership, internal/adapters/outbound/modal/provider.go,
// internal/adapters/outbound/opencode/session.go, internal/sandboxagent/
// snapshotclient/client.go, internal/sandboxagent/credentials/cpclient.go)
// -- this package was the one exception before this fix.
func escapePathSegments(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

// defaultAPIBaseURL is GitHub's own real REST API base -- the ONLY place
// this literal appears in this package; production wiring
// (cmd/control-plane/main.go) passes it explicitly, mirroring
// internal/adapters/inbound/auth's own githubAPIBaseURL constant
// precedent exactly, so it is never silently duplicated or hardcoded a
// second time deeper in this package.
const defaultAPIBaseURL = "https://api.github.com"

// maxResponseBodySize bounds how much of a GitHub API response body this
// adapter ever reads -- mirrors internal/adapters/outbound/modal/client.
// go's own maxResponseBodySize reasoning: http.Client.Timeout bounds
// wall-clock time, not response size.
const maxResponseBodySize = 1 << 20 // 1 MiB

// Adapter implements ports.SourceControl against GitHub's real REST API.
type Adapter struct {
	httpClient *http.Client
	apiBaseURL string
}

// var _ ports.SourceControl = (*Adapter)(nil) makes a SourceControl
// signature drift a build error, not a runtime surprise.
var _ ports.SourceControl = (*Adapter)(nil)

// New builds an Adapter. httpClient should come from NewGatedClient,
// which is the only way to obtain one carrying the shadow gate.
//
// It used to default to http.DefaultClient, and §30.2 names why that
// default had to go: a developer writing New(nil, baseURL) in a new
// package got a working, gate-free instance, invisible to every layer
// above it. Every egress guarantee this platform makes runs through the
// injected transport, so a constructor that silently supplies an ungated
// one is an attractive nuisance rather than a convenience -- the zero
// value must fail closed, and now it refuses to build at all.
//
// apiBaseURL still defaults to defaultAPIBaseURL when empty; production
// wiring should pass it explicitly (see doc.go).
func New(httpClient *http.Client, apiBaseURL string) *Adapter {
	if httpClient == nil {
		// Not http.DefaultClient, which is what this used to be and what
		// §30.2 calls an attractive nuisance: it made New(nil, base) a
		// WORKING, gate-free adapter that no layer above could see. A
		// refusing transport keeps the signature convenient for the many
		// tests that pass a real client, while making the omission
		// useless rather than dangerous -- the zero value fails closed.
		httpClient = &http.Client{Transport: refusingTransport{}}
	}
	if apiBaseURL == "" {
		apiBaseURL = defaultAPIBaseURL
	}
	return &Adapter{
		httpClient: httpClient,
		apiBaseURL: strings.TrimSuffix(apiBaseURL, "/"),
	}
}

// refusingTransport is what an adapter built without a transport gets. It
// answers every request with an error naming the cause, so a construction
// site that forgot the gate fails at its first call with something a
// reader can act on -- rather than silently reaching a customer's
// repository, which is what the old http.DefaultClient default did.
type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("githubapi: this adapter was built with no HTTP client, so it can make no requests -- build it with NewGatedClient, which is the only way to obtain one")
}

// createPRRequest is the body POSTed to /repos/{owner}/{repo}/pulls (real
// GitHub REST API shape -- https://docs.github.com/rest/pulls/pulls#create-a-pull-request,
// verified against GitHub's own live documentation during this Step's own
// design phase).
type createPRRequest struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
}

// createPRResponse is the subset of GitHub's real pull-request response
// shape this adapter needs.
type createPRResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// githubErrorBody is GitHub's own real error envelope shape on a non-2xx
// response (a top-level "message", optionally more detail in "errors").
type githubErrorBody struct {
	Message string `json:"message"`
}

// CreatePRError is the error CreatePR returns for any non-2xx GitHub
// response -- a plain, structured error (this port does not invent a
// transient/permanent classification the way ports.ProviderError does;
// see ports.SourceControl's own doc comment for why: no caller of this
// port retries or trips a circuit breaker on a PR-creation failure yet).
type CreatePRError struct {
	Status  int
	Message string
}

func (e *CreatePRError) Error() string {
	return fmt.Sprintf("githubapi: create pr: http %d: %s", e.Status, e.Message)
}

// CreatePR implements ports.SourceControl: a real POST
// https://api.github.com/repos/{owner}/{repo}/pulls call, authenticated
// with spec.Token as a GitHub OAuth user access token (Authorization:
// Bearer <token> -- GitHub's REST API accepts a user OAuth token this
// way). spec.Token is never logged, here or anywhere this error might
// surface to (CreatePRError carries only GitHub's own response message,
// never the request itself).
func (a *Adapter) CreatePR(ctx context.Context, spec ports.CreatePRSpec) (ports.PRRef, error) {
	reqBody, err := json.Marshal(createPRRequest{
		Title: spec.Title,
		Head:  spec.Head,
		Base:  spec.Base,
		Body:  spec.Body,
	})
	if err != nil {
		return ports.PRRef{}, fmt.Errorf("githubapi: encode create-pr request: %w", err)
	}

	// Owner/Repo are each a single opaque path segment (never containing a
	// real "/" separator the way spec.Path can) -- audit remediation: both
	// escaped via url.PathEscape before being interpolated, mirroring every
	// other outbound adapter's own discipline (see escapePathSegments' own
	// doc comment above).
	path := fmt.Sprintf("%s/repos/%s/%s/pulls", a.apiBaseURL, url.PathEscape(spec.Owner), url.PathEscape(spec.Repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(reqBody))
	if err != nil {
		return ports.PRRef{}, fmt.Errorf("githubapi: build create-pr request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+spec.Token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return ports.PRRef{}, fmt.Errorf("githubapi: create-pr request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return ports.PRRef{}, fmt.Errorf("githubapi: read create-pr response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := "no error body"
		var parsed githubErrorBody
		if err := json.Unmarshal(respBody, &parsed); err == nil && parsed.Message != "" {
			message = parsed.Message
		} else if len(respBody) > 0 {
			message = "error body did not match GitHub's expected error envelope"
		}
		createErr := &CreatePRError{Status: resp.StatusCode, Message: message}

		// Idempotency, mirroring CreateBranch's own identical
		// already-exists-is-success precedent (createRefAlreadyExistsMarker's
		// own doc comment): a 422 whose message names a PR as already
		// existing means a PREVIOUS turn's CreatePR call already succeeded
		// for this exact head/base pair, and the caller (createPRBestEffort,
		// pushpr.go) runs on every completed turn, not just the first --
		// without this, turn 2+ would treat a real, already-open PR as a
		// hard error and never reach recordPRArtifact/the handoff sentinel.
		// Recovering the EXISTING PR's own Number/URL (rather than just
		// swallowing the error) is necessary because callers need real
		// PRRef data, not just "it's fine" -- unlike CreateBranch, which
		// has no return value to reconstruct.
		if resp.StatusCode == http.StatusUnprocessableEntity &&
			strings.Contains(strings.ToLower(message), alreadyExistsMarker) {
			if ref, lookupErr := a.findExistingOpenPR(ctx, spec.Owner, spec.Repo, spec.Head, spec.Base, spec.Token); lookupErr == nil {
				return ref, nil
			}
			// Lookup itself failed (or found no matching open PR) -- fall
			// through to the ORIGINAL error. Never worse than today: the
			// caller sees the exact same CreatePRError it would have seen
			// without this idempotency path at all.
		}

		return ports.PRRef{}, createErr
	}

	var parsed createPRResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ports.PRRef{}, fmt.Errorf("githubapi: decode create-pr response: %w", err)
	}

	return ports.PRRef{Number: parsed.Number, URL: parsed.HTMLURL}, nil
}

// pullRequestListItem is the subset of GitHub's real GET
// /repos/{owner}/{repo}/pulls list-item response shape findExistingOpenPR
// needs (https://docs.github.com/rest/pulls/pulls#list-pull-requests).
type pullRequestListItem struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// findExistingOpenPR recovers the already-open pull request CreatePR's own
// idempotency path needs: GitHub's create-PR 422 message names a PR as
// already existing but never includes its number/URL, so a second, real API
// call is required to get PRRef data a caller can actually use. Scoped by
// BOTH head and base (never head alone) -- head/base together is what
// "already exists" actually means on GitHub's own side; matching head alone
// could recover a PR against an unrelated base branch. An empty result list
// is a plain error, never silently swallowed -- the caller decides whether
// to fall back to the original CreatePRError.
func (a *Adapter) findExistingOpenPR(ctx context.Context, owner, repo, head, base, token string) (ports.PRRef, error) {
	path := fmt.Sprintf("%s/repos/%s/%s/pulls?head=%s&base=%s&state=open",
		a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo),
		url.QueryEscape(owner+":"+head), url.QueryEscape(base))

	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return ports.PRRef{}, fmt.Errorf("githubapi: find existing open pr: %w", err)
	}

	var items []pullRequestListItem
	if err := json.Unmarshal(body, &items); err != nil {
		return ports.PRRef{}, fmt.Errorf("githubapi: decode existing-pr list: %w", err)
	}
	if len(items) == 0 {
		return ports.PRRef{}, fmt.Errorf("githubapi: no open pull request found for %s:%s -> %s", owner, head, base)
	}

	return ports.PRRef{Number: items[0].Number, URL: items[0].HTMLURL}, nil
}

// repoInfoResponse is the subset of GitHub's real GET /repos/{owner}/{repo}
// response shape this adapter needs (https://docs.github.com/rest/repos/repos#get-a-repository).
type repoInfoResponse struct {
	DefaultBranch string `json:"default_branch"`
}

// commitResponse is the subset of GitHub's real GET
// /repos/{owner}/{repo}/commits/{ref} response shape this adapter needs
// (https://docs.github.com/rest/commits/commits#get-a-commit) -- the
// top-level "sha" field IS the commit's own SHA for whatever ref was
// requested (a branch name resolves to its current HEAD commit).
type commitResponse struct {
	SHA string `json:"sha"`
}

// APIError is the error doGet returns for any non-2xx GitHub response --
// mirrors CreatePRError exactly (a plain, structured error; see
// ports.SourceControl's own doc comment for why neither method invents a
// transient/permanent classification). Originally named
// ResolveBranchSHAError (§8.5, "image builds") back when ResolveBranchSHA
// was doGet's only caller; generalized here (§14.3, "mocking + contract
// drift") once ResolveContractsFingerprint became doGet's second caller --
// a small, mechanical, internal-only rename (this type is never part of any
// wire contract), preferred over adding a second, near-duplicate error
// struct for the same shared underlying signal.
type APIError struct {
	Status  int
	Message string

	// RateLimited is set (only ever on a 403, see isRateLimitedResponse
	// below) when this response's own headers/body indicate GitHub's real
	// primary or secondary rate-limit/abuse-detection mechanism answered,
	// rather than a genuine "this token cannot read this resource" denial
	// -- GitHub returns HTTP 403 for BOTH conditions; the status code alone
	// can never tell them apart. Audit fix (security-adversarial, findings
	// #1/#4): CheckRepoAccess's own doc comment requires this distinction
	// -- a rate-limited 403 must be treated as an INDETERMINATE failure
	// (never a definitive, cacheable deny), exactly like a 5xx/timeout
	// already is.
	RateLimited bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("githubapi: http %d: %s", e.Status, e.Message)
}

// Unwrap lets errors.Is(err, ports.ErrAuthenticationFailed)/errors.Is(err,
// ports.ErrPermissionDenied) see through this adapter-internal type to
// the port-level, adapter-agnostic classification a caller like
// internal/app/automerge needs (docs/TECHNICAL_PLAN.md §17's own
// automerge dead-letter fix) -- reusing the SAME Status/RateLimited
// fields every other classification on this type already relies on
// (isRateLimitedResponse below), never a second, parallel
// classification. A 401 is unambiguous (ports.ErrAuthenticationFailed's
// own doc comment: GitHub never uses 401 for rate limiting). A 403
// unwraps to ports.ErrPermissionDenied ONLY when RateLimited is false --
// mirrors ports.MergePRError.Unwrap's own identical logic for MergePR's
// own separately-typed error exactly, so a caller never has to know
// which of this port's two error shapes it is holding to classify it.
func (e *APIError) Unwrap() error {
	switch {
	case e.Status == http.StatusUnauthorized:
		return ports.ErrAuthenticationFailed
	case e.Status == http.StatusForbidden && !e.RateLimited:
		return ports.ErrPermissionDenied
	default:
		return nil
	}
}

// isRateLimitedResponse reports whether a 403 response resp actually
// signals GitHub's own real primary or secondary rate-limit/abuse-detection
// mechanism, rather than a genuine "this token cannot read this resource"
// denial -- GitHub's own real, documented behavior returns HTTP 403 for
// BOTH conditions (see https://docs.github.com/rest/overview/rate-limits-for-the-rest-api
// and https://docs.github.com/rest/guides/best-practices-for-integrators#dealing-with-secondary-rate-limits),
// so resp.StatusCode alone can never distinguish them. Detected via, in
// order:
//
//  1. X-RateLimit-Remaining: "0" -- GitHub's own primary rate-limit
//     exhaustion signal, always present on a primary-limit 403;
//  2. a Retry-After header -- GitHub's own documented secondary-rate-limit
//     / abuse-detection signal (these responses carry Retry-After; a
//     genuine permission-denial 403 normally does not);
//  3. message (the parsed githubErrorBody.Message doGet already extracted)
//     mentioning "rate limit" or "abuse detection" -- a fallback in case a
//     header got stripped somewhere between GitHub and this adapter (a
//     proxy, a test double), matching GitHub's own real error message
//     text for both conditions ("API rate limit exceeded..." / "You have
//     exceeded a secondary rate limit...").
//
// Audit fix (security-adversarial, findings #1/#4): without this,
// CheckRepoAccess treated EVERY 403 as a definitive, cacheable deny --
// including one caused by the REQUESTER'S OWN token hitting a transient
// rate limit (plausible under concurrent multi-repo checks, since
// RepoSHAResolutionTimeout/ContractsFingerprintResolutionTimeout/
// CheckRepoAccess/CreatePR all share the same token's rate-limit budget) --
// which would then wrongly deny, and CACHE that denial for
// platform.Timeouts.RepoAccessCacheTTL, warm-boot for a user who genuinely
// has access.
func isRateLimitedResponse(resp *http.Response, message string) bool {
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return true
	}
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "rate limit") || strings.Contains(lower, "abuse detection")
}

// doGet performs one authenticated GET against a.apiBaseURL+path, returning
// the raw response body on any 2xx status -- the shared request-building/
// auth/bounded-read/error-envelope-parse logic CreatePR's own inline block
// duplicates once; factored out here since ResolveBranchSHA and
// ResolveContractsFingerprint both need the IDENTICAL sequence (repo info/
// commit/contents-listing GETs) rather than each keeping their own verbatim
// copy of it. Returns *APIError on any non-2xx response -- callers that
// need to distinguish a 404 from every other failure (ResolveContractsFingerprint
// below) do so via errors.As against this one shared type.
func (a *Adapter) doGet(ctx context.Context, path, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("githubapi: build get request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubapi: get request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return nil, fmt.Errorf("githubapi: read get response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := "no error body"
		var parsed githubErrorBody
		if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
			message = parsed.Message
		} else if len(body) > 0 {
			message = "error body did not match GitHub's expected error envelope"
		}
		apiErr := &APIError{Status: resp.StatusCode, Message: message}
		if resp.StatusCode == http.StatusForbidden {
			// Audit fix (security-adversarial, findings #1/#4): only ever
			// computed for a 403 -- see isRateLimitedResponse's own doc
			// comment for why a 403 specifically (never a 404 or a 5xx)
			// needs this extra classification.
			//
			// string(body) -- the RAW response body -- not `message` above:
			// message is OVERWRITTEN with the fixed placeholder string
			// "error body did not match GitHub's expected error envelope"
			// whenever body does not parse as GitHub's own JSON error
			// envelope, which is EXACTLY the case isRateLimitedResponse's
			// own message-text fallback exists to catch (its own doc
			// comment: "in case a header got stripped somewhere between
			// GitHub and this adapter (a proxy, a test double)") -- an
			// edge/WAF HTML page (e.g. "error code: 1015 (you are being
			// rate limited)") is never valid JSON, so passing the
			// overwritten placeholder here made that fallback permanently
			// unreachable for the one case its own doc comment names it
			// for. Passing message here would misclassify that response as
			// a genuine, permanent per-repo denial -- the exact direction
			// ports.ErrPermissionDenied's own doc comment forbids.
			apiErr.RateLimited = isRateLimitedResponse(resp, string(body))
		}
		return nil, apiErr
	}

	return body, nil
}

// ResolveBranchSHA implements ports.SourceControl (§8.5, "image
// builds"): resolves spec.Branch's current commit SHA via a real GET
// https://api.github.com/repos/{owner}/{repo}/commits/{branch} call. When
// spec.Branch is empty ("the repo's own default branch" -- ports.
// ResolveBranchSHASpec's own doc comment), this FIRST resolves the repo's
// real default_branch via GET https://api.github.com/repos/{owner}/{repo},
// then uses THAT as the ref -- "main"/"master" is never hardcoded or
// guessed.
//
// This is the concrete implementation of the design decision already made
// for this Step: the control plane resolves each repo's own current SHA
// directly via a real GitHub API call, BEFORE assembling a spawn's
// CreateSpec -- not by waiting for a sandbox to report anything back (that
// would need a new wire message; deliberately not built, see this Step's
// own PR description).
func (a *Adapter) ResolveBranchSHA(ctx context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	branch := spec.Branch
	if branch == "" {
		// Owner/Repo escaped via url.PathEscape -- audit remediation, see
		// escapePathSegments' own doc comment above.
		repoPath := fmt.Sprintf("%s/repos/%s/%s", a.apiBaseURL, url.PathEscape(spec.Owner), url.PathEscape(spec.Repo))
		body, err := a.doGet(ctx, repoPath, spec.Token)
		if err != nil {
			return "", "", fmt.Errorf("githubapi: resolve default branch: %w", err)
		}

		var repoInfo repoInfoResponse
		if err := json.Unmarshal(body, &repoInfo); err != nil {
			return "", "", fmt.Errorf("githubapi: decode repo info response: %w", err)
		}
		if repoInfo.DefaultBranch == "" {
			return "", "", fmt.Errorf("githubapi: repo %s/%s reported an empty default_branch", spec.Owner, spec.Repo)
		}
		branch = repoInfo.DefaultBranch
	}

	// branch comes from spec.Branch (session-controlled repos[].branch,
	// §14.1) when non-empty, otherwise GitHub's own reported default_branch
	// above -- either way, escaped via url.PathEscape (audit remediation):
	// a branch name is a single opaque path segment here (GitHub's own
	// "commits/{ref}" route never expects a "/"-delimited multi-segment
	// value the way spec.Path does below), so a plain PathEscape, not
	// escapePathSegments, is the correct granularity.
	commitPath := fmt.Sprintf("%s/repos/%s/%s/commits/%s", a.apiBaseURL, url.PathEscape(spec.Owner), url.PathEscape(spec.Repo), url.PathEscape(branch))
	body, err := a.doGet(ctx, commitPath, spec.Token)
	if err != nil {
		return "", "", fmt.Errorf("githubapi: resolve commit sha for branch %q: %w", branch, err)
	}

	var commit commitResponse
	if err := json.Unmarshal(body, &commit); err != nil {
		return "", "", fmt.Errorf("githubapi: decode commit response: %w", err)
	}
	if commit.SHA == "" {
		return "", "", fmt.Errorf("githubapi: repo %s/%s branch %q reported an empty commit sha", spec.Owner, spec.Repo, branch)
	}

	return commit.SHA, branch, nil
}

// CheckRepoAccess implements ports.SourceControl (audit fix, "warm-boot
// image access control"): answers "can spec.Token read this repo at all"
// via the SAME GET https://api.github.com/repos/{owner}/{repo} call
// ResolveBranchSHA's own empty-branch case already makes (adapter.go's own
// ResolveBranchSHA, above) -- reusing that exact request shape rather than
// inventing a second one, since GitHub's own repo-info endpoint already
// 404s for a repo this token cannot see (private, or simply nonexistent)
// and 403s for one it can see but not read (e.g. rate-limited or blocked),
// which is precisely the "no access" signal this method exists to report.
// The response body is discarded -- this method only ever needs the
// status code, never repoInfoResponse's own DefaultBranch field.
//
// A 404 is reported as (false, nil): a legitimate, definitive "this token
// cannot read this repo" answer, not an error (ports.SourceControl's own
// doc comment on this method). A 403 is ALSO reported as (false, nil) --
// but ONLY when isRateLimitedResponse (above) says this 403 does not, in
// fact, signal GitHub's own rate-limit/abuse-detection mechanism; a 403
// that DOES carry that signal is treated exactly like a 5xx below (audit
// fix, security-adversarial, findings #1/#4: GitHub returns 403 for BOTH a
// genuine permission denial AND a rate-limited/abuse-flagged request, and
// this method must never conflate the two -- a token merely rate-limited
// at check time is not a token that has lost access). Any OTHER non-2xx
// status, or a transport/timeout failure, is passed through as (false,
// err) -- the caller (app/sessionactor's resolveAndSetImage) is documented
// to treat that as "could not determine access right now", never as a
// definitive deny.
func (a *Adapter) CheckRepoAccess(ctx context.Context, spec ports.CheckRepoAccessSpec) (bool, error) {
	// Owner/Repo escaped via url.PathEscape -- single opaque path segments,
	// mirroring ResolveBranchSHA's own identical discipline (see
	// escapePathSegments' own doc comment above).
	repoPath := fmt.Sprintf("%s/repos/%s/%s", a.apiBaseURL, url.PathEscape(spec.Owner), url.PathEscape(spec.Repo))
	_, err := a.doGet(ctx, repoPath, spec.Token)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			if apiErr.Status == http.StatusNotFound {
				return false, nil
			}
			if apiErr.Status == http.StatusForbidden && !apiErr.RateLimited {
				return false, nil
			}
		}
		return false, fmt.Errorf("githubapi: check repo access: %w", err)
	}
	return true, nil
}

// contentsEntry is the subset of GitHub's real GET
// /repos/{owner}/{repo}/contents/{path} response shape this adapter needs
// (https://docs.github.com/rest/repos/contents#get-repository-content) --
// requested WITHOUT a trailing filename (i.e. path names a directory),
// this endpoint returns a JSON ARRAY of these, one per immediate entry
// (file or subdirectory) -- never a single object. Type is "file" or
// "dir"; this adapter does not need to branch on it (see
// ResolveContractsFingerprint's own doc comment: a subdirectory's own Sha
// is already the recursive git tree hash of everything nested under it,
// so every entry's Sha is used identically regardless of Type).
type contentsEntry struct {
	Path string `json:"path"`
	Sha  string `json:"sha"`
	Type string `json:"type"`
}

// issueCommentRequest is the body POSTed to
// /repos/{owner}/{repo}/issues/{issue_number}/comments (real GitHub REST
// API shape -- https://docs.github.com/rest/issues/comments#create-an-issue-comment).
// A pull request is itself always addressable through GitHub's Issues API
// (every PR IS an issue, sharing the same numbering) -- there is no
// separate "PR comment" endpoint distinct from this one.
type issueCommentRequest struct {
	Body string `json:"body"`
}

// doPost performs one authenticated POST against a.apiBaseURL+path with
// reqBody as the JSON request body, returning the raw response body on any
// 2xx status -- the doGet-analog this package's own doc.go promises: same
// bounded-read/error-envelope-parsing shape, just a POST with a body
// instead of a bodyless GET.
func (a *Adapter) doPost(ctx context.Context, path, token string, reqBody []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("githubapi: build post request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubapi: post request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return nil, fmt.Errorf("githubapi: read post response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := "no error body"
		var parsed githubErrorBody
		if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
			message = parsed.Message
		} else if len(body) > 0 {
			message = "error body did not match GitHub's expected error envelope"
		}
		apiErr := &APIError{Status: resp.StatusCode, Message: message}
		if resp.StatusCode == http.StatusForbidden {
			// Audit fix: doPost previously never computed RateLimited at
			// all, so a textbook primary-rate-limited 403 through this
			// method unwrapped straight to ports.ErrPermissionDenied --
			// see doGet's own identical isRateLimitedResponse call/doc
			// comment above for why string(body) (the raw body), not
			// message, is what the fallback text check needs.
			apiErr.RateLimited = isRateLimitedResponse(resp, string(body))
		}
		return nil, apiErr
	}

	return body, nil
}

// pullRequestResponse is the subset of GitHub's real GET
// /repos/{owner}/{repo}/pulls/{pull_number} response shape this adapter
// needs (https://docs.github.com/rest/pulls/pulls#get-a-pull-request) --
// batch fix/audit-github-pr-payload-correctness's own H5 fix: resolving an
// issue_comment mention's TRUE head branch/repo, since that event type's
// own webhook payload never carries them directly (unlike
// pull_request_review_comment, whose payload already embeds this same
// head.ref/head.repo shape verbatim -- internal/adapters/inbound/github/
// payload.go's own pullRequestReviewCommentPayload).
//
// Title/Body (adversarial-review fix, §26.2's own follow-up: the
// review-digest "descriptionAdequacy" comparison needs the PR's real
// title/body as INPUT, and this endpoint already returns both -- see
// PullRequest.Title/Body's own doc comment below for the full "why" this
// adapter, not the reviewing agent, is the one fetching them).
type pullRequestResponse struct {
	// Title/Body are GitHub's own top-level "title"/"body" string fields on
	// this SAME PR resource -- "body" is nullable (a PR opened with no
	// description at all decodes it as JSON null, GitHub's own documented
	// shape), so a plain string field would fail to unmarshal a null body;
	// *string mirrors Head.Repo's own identical nullable-pointer precedent
	// immediately below, in this SAME struct, for the SAME reason.
	Title string  `json:"title"`
	Body  *string `json:"body"`

	Head struct {
		Ref string `json:"ref"`
		// SHA (§21.1) is this PR's own CURRENT head commit --
		// review_verdicts.head_sha's own ultimate source when this
		// response is the fallback fetch internal/app/reviewcontext.Fetch
		// makes for a trigger path with no head SHA already in hand (see
		// PullRequest.HeadSHA's own doc comment below).
		SHA string `json:"sha"`
		// Repo is a pointer -- GitHub's own webhook/REST documentation
		// states this field is nullable: null when the head repository has
		// been deleted (e.g. a fork removed after the PR was opened).
		// Mirrors payload.go's own identical nullable-pointer fix for the
		// SAME underlying GitHub concept (L15 audit fix).
		Repo *struct {
			Name     string `json:"name"`
			CloneURL string `json:"clone_url"`
		} `json:"repo"`
	} `json:"head"`

	// Base ("release PR review", §15.1) is this PR's own real
	// base branch name -- release detection's own "originates from/
	// targets a release/* branch" check needs this alongside Head.Ref,
	// which this response already decoded for an entirely different
	// reason (H5's head-branch resolution). Never nullable on a real
	// GitHub PR resource (unlike Head.Repo, a base branch/repo can never
	// be deleted while the PR referencing it is open/merged).
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`

	// Labels (§15.1) is this PR's own CURRENT label set --
	// release detection's own "carries a release label" check.
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`

	// Stack is GitHub's own "stack" object (§17.6's amendment,
	// "review sessions"), riding on this SAME PR resource -- confirmed
	// present via live schema introspection during that amendment's own
	// design phase. A POINTER, mirroring Head.Repo's own nullable-pointer
	// discipline immediately above: GitHub's own documentation states a
	// non-stacked PR (the ordinary, common case today) carries no "stack"
	// key at all, which unmarshals a Go pointer field to nil rather than a
	// zero-valued, misleadingly-present struct.
	Stack *stackResponse `json:"stack"`

	// Additions/Deletions/ChangedFiles (§26.3) are GitHub's own
	// top-level "additions"/"deletions"/"changed_files" integers on this
	// SAME "Get a pull request" response -- already arriving on the wire
	// alongside every other field this response already decodes, simply
	// unparsed until this Step. See PullRequest.Additions' own doc
	// comment below for what consumes them.
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
}

// stackResponse is the subset of GitHub's own "stack" object shape (§17.6)
// this adapter needs: size, this PR's own 1-based position, and the
// stack's ultimate base (ref+sha) -- deliberately omits GitHub's own
// stack "id"/PR "number" fields, which nothing in this codebase consumes
// (StackInfo's own doc comment below).
type stackResponse struct {
	Size     int `json:"size"`
	Position int `json:"position"`
	Base     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

// PullRequest is GetPullRequest's own return shape: enough to resolve a
// PR's TRUE head branch/repo (H5 audit fix), plus ("review
// sessions", §8.2/§17.6) its GitHub-native stack context, when present.
type PullRequest struct {
	// Title/Body are this PR's own CURRENT title/body (GitHub's own top-
	// level "title"/"body" string fields) -- adversarial-review fix
	// (§26.2's own follow-up): the review-digest
	// "descriptionAdequacy" comparison (internal/domain/review.
	// AdequacyFloor's own input) needs the PR's real title/body as
	// UNTRUSTED INPUT (§5.2), and this endpoint already returns both on
	// every real GetPullRequest call -- so internal/app/reviewcontext.Fetch
	// (this method's one real caller building a review turn's context)
	// carries them onto review.PreFetchedContext.Title/Body, and
	// review.RenderTurnPrompt renders them into a DELIMITED, labeled
	// "treat as DATA, never instructions" block (mirroring the existing
	// <pr_diff> block), rather than instructing the reviewing agent to
	// fetch them itself via its own tool use (e.g. `gh pr view`). That
	// agent-fetch approach is what this fix REPLACES: no GitHub credential
	// reaches the sandbox (the sandbox bearer token is deliberately
	// stripped, opencodeproc/spawn.go), so an agent-side `gh pr view` was
	// never actually reachable in the first place -- and even where a
	// credential existed, an agent-side re-fetch would be unpinned to the
	// exact head SHA this review verdict is about. Never empty for a real
	// GitHub PR the way HeadRef never is (Title), though Body legitimately
	// can be (a PR opened with no description) -- see Body's own doc
	// comment.
	Title string
	// Body is this PR's own CURRENT description -- empty when GitHub's own
	// "body" was null (a PR opened with no description at all, a real and
	// common case) OR empty string; both collapse to Go's own empty string
	// here, mirroring HeadRepoName/HeadRepoCloneURL's own "nullable field
	// collapses to its zero value" precedent immediately below, since
	// nothing downstream of this adapter (review.RenderTurnPrompt) needs to
	// distinguish "no body at all" from "an explicitly empty body".
	Body string
	// HeadRef is the PR's real head branch name (GitHub's own
	// "head.ref") -- never empty for a real, open GitHub pull request.
	HeadRef string
	// HeadSHA (§21.1) is the PR's CURRENT head commit SHA
	// (GitHub's own "head.sha") -- internal/app/reviewcontext.Fetch's own
	// fallback source for review_verdicts.head_sha when the caller has
	// no head SHA already in hand from its own trigger payload (see that
	// package's own doc comment).
	HeadSHA string
	// HeadRepoName/HeadRepoCloneURL are the PR's real head repo (may be a
	// fork) -- empty when GitHub's own "head.repo" was null (the head/fork
	// repo has since been deleted). Callers MUST treat this exactly like
	// payload.go's own pull_request_review_comment nullable-head-repo
	// fallback: fall back to the base repo, never proceed with an empty
	// repo spec.
	HeadRepoName     string
	HeadRepoCloneURL string
	// BaseRef (§15.1) is this PR's own real base branch name.
	BaseRef string
	// Labels (§15.1) is this PR's own current label names.
	Labels []string
	// Stack is non-nil exactly when this PR belongs to a GitHub-native
	// stack (§17.6) -- nil is the ordinary, ungrouped case. Callers building
	// a review turn's pre-fetched context (internal/app/reviewcontext)
	// convert this into internal/domain/review.StackContext; this adapter
	// itself stays domain-agnostic, mirroring how it already never imports
	// internal/adapters/inbound/github's own mention type.
	Stack *StackInfo

	// Additions/Deletions/ChangedFiles (§26.3) are this PR's own
	// server-reported diff-size facts, forwarded verbatim from GitHub's
	// own "Get a pull request" response (pullRequestResponse.Additions/
	// Deletions/ChangedFiles above) -- internal/app/reviewcontext.Fetch's
	// own one real consumer, carrying these onto review.
	// PreFetchedContext.Additions/Deletions/ChangedFilesCount for §26.3's
	// own light/deep triage decision (internal/domain/reviewtriage).
	Additions    int
	Deletions    int
	ChangedFiles int
}

// StackInfo is GetPullRequest's own stack-context return shape (§17.6) --
// GitHub's stack "size"/"position"/"base.ref"/"base.sha" fields, unchanged
// from the wire shape (stackResponse above) other than being exported.
// Deliberately omits GitHub's own stack "id" and PR "number" fields: nothing
// in this codebase's review-context rendering (internal/domain/review.
// StackContext, consumed by RenderTurnPrompt) needs either -- §21.1 names
// exactly three things a review needs from a stack as context: "position,
// size, and the stack's ultimate base".
type StackInfo struct {
	Size     int
	Position int
	BaseRef  string
	BaseSHA  string
}

// GetPullRequest resolves pull request number's real head branch/repo via
// a real GET https://api.github.com/repos/{owner}/{repo}/pulls/{number}
// call, authenticated with token as a Bearer token -- like PostIssueComment,
// this method is agnostic to whose token it's handed (production wiring,
// cmd/control-plane/main.go, passes the bot's own statically-configured
// credential, platform.Config.GitHubBotToken, since a GitHub webhook
// mention carries no per-commenter OAuth token the way CreatePR's caller
// already has one in hand).
func (a *Adapter) GetPullRequest(ctx context.Context, owner, repo string, number int32, token string) (PullRequest, error) {
	// Owner/Repo escaped via url.PathEscape (single opaque segments,
	// mirroring every other method's own identical discipline -- see
	// escapePathSegments' own doc comment above); number needs no escaping
	// (a plain decimal integer).
	path := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), number)
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return PullRequest{}, fmt.Errorf("githubapi: get pull request: %w", err)
	}

	var parsed pullRequestResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return PullRequest{}, fmt.Errorf("githubapi: decode pull request response: %w", err)
	}

	pr := PullRequest{
		Title:        parsed.Title,
		HeadRef:      parsed.Head.Ref,
		HeadSHA:      parsed.Head.SHA,
		BaseRef:      parsed.Base.Ref,
		Additions:    parsed.Additions,
		Deletions:    parsed.Deletions,
		ChangedFiles: parsed.ChangedFiles,
	}
	if parsed.Body != nil {
		pr.Body = *parsed.Body
	}
	if parsed.Head.Repo != nil {
		pr.HeadRepoName = parsed.Head.Repo.Name
		pr.HeadRepoCloneURL = parsed.Head.Repo.CloneURL
	}
	if len(parsed.Labels) > 0 {
		pr.Labels = make([]string, len(parsed.Labels))
		for i, l := range parsed.Labels {
			pr.Labels[i] = l.Name
		}
	}
	if parsed.Stack != nil {
		pr.Stack = &StackInfo{
			Size:     parsed.Stack.Size,
			Position: parsed.Stack.Position,
			BaseRef:  parsed.Stack.Base.Ref,
			BaseSHA:  parsed.Stack.Base.SHA,
		}
	}
	return pr, nil
}

// diffAcceptHeader requests GitHub's own raw unified-diff representation of
// a pull request via content negotiation on the IDENTICAL GET
// /repos/{owner}/{repo}/pulls/{number} endpoint GetPullRequest already
// calls (https://docs.github.com/rest/pulls/pulls#get-a-pull-request,
// "Custom media types" section) -- not a distinct endpoint, just a
// different Accept header against the same resource. Verified against
// GitHub's own live REST API documentation (fetched 2026-07-31, this
// Step's own design phase): "application/vnd.github.diff" is the CURRENT
// documented value --
// deliberately NOT "application/vnd.github.v3.diff" (an older, version-
// embedded media-type shape GitHub's own docs confirm it has since moved
// away from in favor of versionless media types plus the separate
// X-GitHub-Api-Version header; every other custom media type this
// endpoint documents today -- raw+json, text+json, html+json, full+json --
// follows the SAME versionless "vnd.github.PARAM[+json]" shape, with no
// "v3" component anywhere).
const diffAcceptHeader = "application/vnd.github.diff"

// maxPRDiffResponseBytes bounds how much of GitHub's own raw diff response
// GetPullRequestDiff reads -- deliberately a DIFFERENT (larger) cap than
// maxResponseBodySize (this package's own cap for every OTHER response,
// all small JSON payloads): a real PR's unified diff can legitimately run
// well past 1 MiB (a large refactor, a vendored or generated file changed
// wholesale), where every other response this adapter reads is a small,
// bounded JSON document. 4 MiB is generous for the large-but-ordinary case
// while still bounding worst-case memory for a truly enormous diff --
// GetPullRequestDiff reports (via its own truncated return) when this cap
// was actually hit, rather than silently handing back a partial diff with
// no signal that it is partial (see internal/domain/review.RenderTurnPrompt's
// own truncation-notice rendering, this method's one real consumer's own
// consumer).
const maxPRDiffResponseBytes = 4 << 20 // 4 MiB

// GetPullRequestDiff fetches pull request number's own current unified diff
// ("review sessions", §8.2: "inline diff pre-fetched into
// context") via the SAME GET https://api.github.com/repos/{owner}/{repo}/
// pulls/{number} call GetPullRequest makes, content-negotiated for GitHub's
// raw diff media type instead of its default JSON resource shape --
// authenticated with token as a Bearer token, agnostic to whose token it's
// handed, exactly like GetPullRequest/PostIssueComment above.
//
// truncated reports whether the real diff was cut short at
// maxPRDiffResponseBytes -- true means diff is a PREFIX of the PR's real,
// full diff, not the whole thing; callers must surface this honestly
// (RenderTurnPrompt's own explicit notice) rather than silently treating a
// partial diff as complete.
//
// Errors are plain, exactly like GetPullRequest/CreatePR above -- no
// caller of this method retries or trips a circuit breaker on a fetch
// failure; every real caller today (internal/app/reviewcontext.Fetch)
// treats a failure here as "no pre-fetched diff available", logged, never
// a reason to fail the review turn's own creation.
func (a *Adapter) GetPullRequestDiff(ctx context.Context, owner, repo string, number int32, token string) (diff string, truncated bool, err error) {
	// Owner/Repo escaped via url.PathEscape (single opaque segments,
	// mirroring GetPullRequest's own identical discipline); number needs no
	// escaping (a plain decimal integer).
	path := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", false, fmt.Errorf("githubapi: build get pull request diff request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", diffAcceptHeader)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("githubapi: get pull request diff request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read one byte past the cap so a full cap-sized read (len ==
	// maxPRDiffResponseBytes+1) is distinguishable from a real diff that
	// happens to end exactly at the cap (len == maxPRDiffResponseBytes) --
	// without this, a diff exactly maxPRDiffResponseBytes long would be
	// misreported as truncated even though nothing was actually cut.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPRDiffResponseBytes+1))
	if err != nil {
		return "", false, fmt.Errorf("githubapi: read pull request diff response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// GitHub's own error envelope is still plain JSON even when the
		// SUCCESS response would have been a raw diff -- the Accept header
		// only affects a 2xx body's shape, mirroring doGet's own identical
		// error-envelope parsing.
		message := "no error body"
		var parsedErr githubErrorBody
		if jsonErr := json.Unmarshal(body, &parsedErr); jsonErr == nil && parsedErr.Message != "" {
			message = parsedErr.Message
		} else if len(body) > 0 {
			message = "error body did not match GitHub's expected error envelope"
		}
		apiErr := &APIError{Status: resp.StatusCode, Message: message}
		if resp.StatusCode == http.StatusForbidden {
			// Audit fix: this method previously never computed RateLimited
			// at all -- see doGet's own identical isRateLimitedResponse
			// call/doc comment above for why string(body) (the raw body),
			// not message, is what the fallback text check needs.
			apiErr.RateLimited = isRateLimitedResponse(resp, string(body))
		}
		return "", false, apiErr
	}

	if len(body) > maxPRDiffResponseBytes {
		return string(body[:maxPRDiffResponseBytes]), true, nil
	}
	return string(body), false, nil
}

// GetCompareDiff fetches the unified diff between base and head -- EXACT
// commit SHAs (or refs), never re-resolved by GitHub at request time --
// via GitHub's own compare-two-commits endpoint (GET /repos/{owner}/
// {repo}/compare/{base}...{head}), content-negotiated for the SAME diff
// media type GetPullRequestDiff above uses (diffAcceptHeader): the
// compare endpoint documents the identical custom-media-type family
// (https://docs.github.com/rest/commits/commits#compare-two-commits,
// "Custom media types" section lists "diff" among this endpoint's own
// supported Accept values, fetched 2026-08-11) -- not a distinct diff
// FORMAT, just a distinct RESOURCE, requested the same way. This
// adapter's own ListMergedBetween (mergedbetween.go) already calls this
// exact endpoint (in GitHub's default JSON shape, for commit discovery)
// -- this method is the SAME URL family, content-negotiated for raw diff
// text instead.
//
// UNLIKE GetPullRequestDiff
// (which always reflects the PR's CURRENT head, a moving target a slow
// caller can race against), this PINS the diff to an EXACT head commit:
// two calls a second apart against the identical (base, head) pair
// return byte-identical results regardless of what has since been
// pushed to the PR, because head here is never "whatever this PR's
// current HEAD happens to be right now" the way GetPullRequestDiff's own
// /pulls/{number} target implicitly is. internal/app/reviewcontext.Fetch
// (this method's one real caller) relies on exactly this property: it
// resolves head (via GetPullRequest) FIRST, then fetches the diff PINNED
// to that SAME value, so the returned diff is, BY CONSTRUCTION, the diff
// at the SHA the caller goes on to persist as review_verdicts.head_sha
// -- never two independently-raceable reads that could silently
// disagree about which commit either one reflects.
//
// truncated/error handling both mirror GetPullRequestDiff's own
// identical discipline exactly (same size cap, same error-envelope
// parsing, same plain-error convention) -- deliberately not
// consolidated into one shared helper parameterized by URL: the two
// target semantically different GitHub resources (a PR's own current
// state vs. an explicit commit-range compare), and a future change to
// one's own error handling should not silently ripple into the other
// merely because they happened to share a helper today.
func (a *Adapter) GetCompareDiff(ctx context.Context, owner, repo, base, head, token string) (diff string, truncated bool, err error) {
	path := fmt.Sprintf("%s/repos/%s/%s/compare/%s...%s", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(base), url.PathEscape(head))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", false, fmt.Errorf("githubapi: build get compare diff request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", diffAcceptHeader)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("githubapi: get compare diff request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Same cap/off-by-one-avoidance discipline as GetPullRequestDiff above
	// -- see that method's own doc comment for why one byte past the cap
	// is read.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPRDiffResponseBytes+1))
	if err != nil {
		return "", false, fmt.Errorf("githubapi: read compare diff response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := "no error body"
		var parsedErr githubErrorBody
		if jsonErr := json.Unmarshal(body, &parsedErr); jsonErr == nil && parsedErr.Message != "" {
			message = parsedErr.Message
		} else if len(body) > 0 {
			message = "error body did not match GitHub's expected error envelope"
		}
		apiErr := &APIError{Status: resp.StatusCode, Message: message}
		if resp.StatusCode == http.StatusForbidden {
			// Audit fix: this method previously never computed RateLimited
			// at all -- see doGet's own identical isRateLimitedResponse
			// call/doc comment above for why string(body) (the raw body),
			// not message, is what the fallback text check needs.
			apiErr.RateLimited = isRateLimitedResponse(resp, string(body))
		}
		return "", false, apiErr
	}

	if len(body) > maxPRDiffResponseBytes {
		return string(body[:maxPRDiffResponseBytes]), true, nil
	}
	return string(body), false, nil
}

// PostIssueComment posts body as a new comment on repo owner/repo's
// issue/PR prNumber, authenticated with token as a Bearer token (
// "outbox delivery", §5.1). Unlike CreatePR/ResolveBranchSHA above, token
// here is typically a single, statically-configured bot credential
// (platform.Config.GitHubBotToken via BotNotifier, notifier.go) rather
// than a per-session decrypted OAuth token -- this method itself is
// agnostic to which kind of token it's handed, exactly like doGet/doPost
// are agnostic to whose token they carry.
func (a *Adapter) PostIssueComment(ctx context.Context, owner, repo string, prNumber int, token, body string) error {
	reqBody, err := json.Marshal(issueCommentRequest{Body: body})
	if err != nil {
		return fmt.Errorf("githubapi: encode post-issue-comment request: %w", err)
	}

	// Owner/Repo escaped via url.PathEscape (single opaque segments,
	// mirroring CreatePR/ResolveBranchSHA's own identical discipline); PR
	// numbers need no escaping (a plain decimal integer).
	path := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), prNumber)
	if _, err := a.doPost(ctx, path, token, reqBody); err != nil {
		return fmt.Errorf("githubapi: post issue comment: %w", err)
	}
	return nil
}

// createCommitStatusRequest is the body POSTed to
// /repos/{owner}/{repo}/statuses/{sha} (real GitHub REST API shape --
// https://docs.github.com/rest/commits/statuses#create-a-commit-status).
type createCommitStatusRequest struct {
	State       string `json:"state"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
	Context     string `json:"context,omitempty"`
}

// CreateCommitStatus posts a commit status on owner/repo at sha (
// "RWX provider + previews", §4.1.2 point 3): a real POST
// https://api.github.com/repos/{owner}/{repo}/statuses/{sha} call,
// authenticated with token as a Bearer token -- like PostIssueComment
// above, this method is agnostic to whose token it's handed; production
// wiring (PreviewLinkNotifier, previewlinknotifier.go) authenticates with
// the SAME static bot credential (platform.Config.GitHubBotToken) every
// other system-initiated write in this package already uses, since a
// preview link is a system-generated fact about a commit, never
// attributed to any individual PR author or reviewer.
//
// Redelivery of the SAME (context, sha) pair converges rather than
// duplicating -- GitHub's own documented commit-status behavior (unlike
// PostIssueComment, which posts a new, additional comment on every call)
// -- exactly the property §4.1.2 point 3 calls out as "strictly better
// here than PostIssueComment... and zero timeline noise per push".
func (a *Adapter) CreateCommitStatus(ctx context.Context, owner, repo, sha, state, targetURL, description, statusContext, token string) error {
	reqBody, err := json.Marshal(createCommitStatusRequest{
		State:       state,
		TargetURL:   targetURL,
		Description: description,
		Context:     statusContext,
	})
	if err != nil {
		return fmt.Errorf("githubapi: encode create-commit-status request: %w", err)
	}

	// Owner/Repo/sha escaped via url.PathEscape (single opaque segments,
	// mirroring PostIssueComment's own identical discipline) -- sha is a
	// commit SHA (or, per GitHub's own docs, technically any ref name);
	// still escaped defensively even though a real SHA never needs it.
	path := fmt.Sprintf("%s/repos/%s/%s/statuses/%s", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	if _, err := a.doPost(ctx, path, token, reqBody); err != nil {
		return fmt.Errorf("githubapi: create commit status: %w", err)
	}
	return nil
}

// ResolveContractsFingerprint implements ports.SourceControl (
// "mocking + contract drift", §14.3): fingerprints spec.Path's directory
// listing at spec.Ref via a real GET
// https://api.github.com/repos/{owner}/{repo}/contents/{path}?ref={ref}
// call (GitHub's Contents API), reusing doGet exactly like ResolveBranchSHA
// above.
//
// A 404 (no directory at that path/ref -- the common case: most repos/refs
// have no contracts directory) is detected by checking doGet's returned
// error via errors.As against *APIError and its own Status field -- the
// SAME typed-error signal ResolveBranchSHA's own callers could already use,
// reused here rather than inventing a second, differently-shaped error
// path. On a 404, this returns ("", false, nil): exists=false, err=nil,
// per ports.SourceControl's own doc comment ("a legitimate, expected
// outcome... NOT an error"). Any OTHER non-2xx status, or a transport
// failure, is a real error: ("", false, err).
//
// On success, the response is parsed as a JSON array of contentsEntry (a
// non-recursive directory listing -- GitHub's Contents API gives every
// entry, including subdirectories, its own "sha" field already covering
// everything nested under it, so this adapter never recurses into a
// subdirectory entry itself); the path->sha map built from it is handed to
// contractdrift.Fingerprint, and (digest, true, nil) is returned.
func (a *Adapter) ResolveContractsFingerprint(ctx context.Context, spec ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	// Audit remediation (security-crosscutting lens): Owner/Repo escaped
	// via url.PathEscape (single opaque segments); spec.Path escaped
	// segment-by-segment via escapePathSegments (its own doc comment above)
	// since Path is a real, multi-segment repo-relative directory path
	// (Environment.ContractsPath, §14.3, assigned verbatim from the request
	// body at CreateSession time -- see internal/adapters/inbound/httpapi/
	// create.go's own environment.ValidateContractsPath call, this same
	// remediation's other half); spec.Ref escaped via url.QueryEscape since
	// it is a QUERY value, not a path segment. Before this fix, an
	// attacker-influenced Path like "x?ref=other&" could rewrite this
	// request's own query string, and a "#" could truncate it via an
	// unintended URL fragment -- both closed by encoding every one of
	// these caller-controlled fields at its own correct escaping
	// granularity, matching the rest of this codebase's existing
	// discipline (see escapePathSegments' own doc comment for the other
	// adapters that already do this).
	contentsPath := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		a.apiBaseURL, url.PathEscape(spec.Owner), url.PathEscape(spec.Repo), escapePathSegments(spec.Path), url.QueryEscape(spec.Ref))
	body, err := a.doGet(ctx, contentsPath, spec.Token)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, fmt.Errorf("githubapi: resolve contracts fingerprint: %w", err)
	}

	var entries []contentsEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return "", false, fmt.Errorf("githubapi: decode contents response: %w", err)
	}

	shas := make(map[string]string, len(entries))
	for _, e := range entries {
		shas[e.Path] = e.Sha
	}

	return contractdrift.Fingerprint(shas), true, nil
}

// getFileContentResponse is the subset of GitHub's real GET
// /repos/{owner}/{repo}/contents/{path} response shape this adapter needs
// for a SINGLE FILE (as opposed to a directory listing, decoded via
// []contentsEntry above) -- https://docs.github.com/rest/repos/contents#get-repository-content.
// GitHub base64-encodes Content, with embedded newlines every 60 chars
// (its own documented behavior) -- base64.StdEncoding.DecodeString
// tolerates that fine (it ignores non-alphabet whitespace).
type getFileContentResponse struct {
	Content string `json:"content"`
	Sha     string `json:"sha"`
}

// GetFileContent implements ports.SourceControl ("sentinels +
// suggestions", §12.2 item 2): a real GET https://api.github.com/repos/
// {owner}/{repo}/contents/{path}?ref={ref} call, authenticated with
// spec.Token. exists=false, err=nil on a 404 -- mirrors
// ResolveContractsFingerprint's own identical "a missing path is a
// legitimate answer, never conflated with a real API failure" discipline
// immediately above.
func (a *Adapter) GetFileContent(ctx context.Context, spec ports.GetFileContentSpec) (content, sha string, exists bool, err error) {
	// Escaping discipline mirrors ResolveContractsFingerprint exactly:
	// Owner/Repo as opaque path segments, Path segment-by-segment
	// (review_findings.file_path is a real, multi-segment repo-relative
	// path), Ref as a query value.
	return a.fetchFileContent(ctx, spec.Owner, spec.Repo, spec.Path, spec.Ref, spec.Token)
}

// fetchFileContent is GetFileContent's own real implementation, factored
// out so a SECOND caller can reuse it without duplicating the base64-
// decode step: resolvecodeowners.go's own CODEOWNERS-file fetch (
// §16.2) needs the IDENTICAL "GET the Contents API, 404 is a legitimate
// non-error exists=false, base64-decode on success" sequence this method
// already established for GetFileContent (§8.2), just for a
// candidate-location loop instead of one caller-supplied path.
func (a *Adapter) fetchFileContent(ctx context.Context, owner, repo, path, ref, token string) (content, sha string, exists bool, err error) {
	contentsPath := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), escapePathSegments(path), url.QueryEscape(ref))
	body, err := a.doGet(ctx, contentsPath, token)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("githubapi: get file content: %w", err)
	}

	var parsed getFileContentResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", false, fmt.Errorf("githubapi: decode get-file-content response: %w", err)
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(parsed.Content, "\n", ""))
	if err != nil {
		return "", "", false, fmt.Errorf("githubapi: decode file content base64: %w", err)
	}

	return string(decoded), parsed.Sha, true, nil
}

// updateFileContentRequest is the body PUT to /repos/{owner}/{repo}/
// contents/{path} (real GitHub REST API shape --
// https://docs.github.com/rest/repos/contents#create-or-update-file-contents).
type updateFileContentRequest struct {
	Message string `json:"message"`
	Content string `json:"content"`
	Sha     string `json:"sha"`
	Branch  string `json:"branch"`
}

// updateFileContentResponse is the subset of GitHub's real response shape
// this adapter needs -- the new commit's own sha.
type updateFileContentResponse struct {
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// UpdateFileContent implements ports.SourceControl (§12.2 item
// 2): a real PUT https://api.github.com/repos/{owner}/{repo}/contents/
// {path} call, authenticated with spec.Token AS THE ACTING MAINTAINER
// (never the original session creator's token -- see
// ports.UpdateFileContentSpec's own doc comment) -- the commit this call
// creates is attributed to spec.Token's own identity by GitHub itself,
// exactly like CreatePR's own token-based attribution.
func (a *Adapter) UpdateFileContent(ctx context.Context, spec ports.UpdateFileContentSpec) (string, error) {
	reqBody, err := json.Marshal(updateFileContentRequest{
		Message: spec.Message,
		Content: base64.StdEncoding.EncodeToString([]byte(spec.Content)),
		Sha:     spec.SHA,
		Branch:  spec.Branch,
	})
	if err != nil {
		return "", fmt.Errorf("githubapi: encode update-file-content request: %w", err)
	}

	contentsPath := fmt.Sprintf("%s/repos/%s/%s/contents/%s",
		a.apiBaseURL, url.PathEscape(spec.Owner), url.PathEscape(spec.Repo), escapePathSegments(spec.Path))
	body, err := a.doPut(ctx, contentsPath, spec.Token, reqBody)
	if err != nil {
		return "", fmt.Errorf("githubapi: update file content: %w", err)
	}

	var parsed updateFileContentResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("githubapi: decode update-file-content response: %w", err)
	}

	return parsed.Commit.SHA, nil
}

// registerPRStackRequest is the body POSTed to /repos/{owner}/{repo}/stacks
// (§17.2/§17.6's own amendment -- "POST /repos/{owner}/{repo}/stacks with
// pull_requests: [originPR, fixPR] (bottom to top)").
type registerPRStackRequest struct {
	PullRequests []int `json:"pull_requests"`
}

// RegisterPRStack implements ports.SourceControl (§17.2/§17.6).
// Per that section's own design, a failure here is the CALLER's own
// policy to log-and-ignore (pushpr.go's createSentinelFixPRBestEffort) --
// this method itself still reports the plain error either way, never
// swallowing it silently on the caller's behalf.
func (a *Adapter) RegisterPRStack(ctx context.Context, spec ports.RegisterPRStackSpec) error {
	reqBody, err := json.Marshal(registerPRStackRequest{PullRequests: spec.PRNumbers})
	if err != nil {
		return fmt.Errorf("githubapi: encode register-pr-stack request: %w", err)
	}

	stacksPath := fmt.Sprintf("%s/repos/%s/%s/stacks", a.apiBaseURL, url.PathEscape(spec.Owner), url.PathEscape(spec.Repo))
	if _, err := a.doPost(ctx, stacksPath, spec.Token, reqBody); err != nil {
		return fmt.Errorf("githubapi: register pr stack: %w", err)
	}
	return nil
}

// createRefRequest is the body POSTed to /repos/{owner}/{repo}/git/refs
// (a confirmed-finding fix, §17.2 -- "Git References" API:
// https://docs.github.com/rest/git/refs#create-a-reference).
type createRefRequest struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// alreadyExistsMarker is the distinctive substring GitHub's own documented
// error message carries when a create call names something that already
// exists -- "Reference already exists" for POST .../git/refs,
// "A pull request already exists for ..." for POST .../pulls. GitHub
// reports both as a plain 422, the SAME status a dozen other validation
// failures also use, so there is no status-code-alone way to distinguish
// "idempotent retry, already created" from a genuine validation error;
// this adapter's own doGet/CreatePR error-envelope parsing already
// surfaces GitHub's message text for exactly this reason (see APIError's
// own doc comment). Shared by CreateBranch and CreatePR -- both treat this
// exact substring, case-insensitively, as their own already-exists signal.
const alreadyExistsMarker = "already exists"

// CreateBranch implements ports.SourceControl (§8.2 confirmed-finding
// fix, §17.2): a real POST /repos/{owner}/{repo}/git/refs call. Idempotent
// per ports.SourceControl.CreateBranch's own doc comment: a 422 whose
// message names the ref as already existing is treated as success, never
// an error -- see alreadyExistsMarker's own doc comment for why
// message-matching, not status-code-matching, is this adapter's only way
// to recognize it.
func (a *Adapter) CreateBranch(ctx context.Context, spec ports.CreateBranchSpec) error {
	reqBody, err := json.Marshal(createRefRequest{Ref: "refs/heads/" + spec.Branch, SHA: spec.SHA})
	if err != nil {
		return fmt.Errorf("githubapi: encode create-branch request: %w", err)
	}

	path := fmt.Sprintf("%s/repos/%s/%s/git/refs", a.apiBaseURL, url.PathEscape(spec.Owner), url.PathEscape(spec.Repo))
	if _, err := a.doPost(ctx, path, spec.Token, reqBody); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity &&
			strings.Contains(strings.ToLower(apiErr.Message), alreadyExistsMarker) {
			return nil
		}
		return fmt.Errorf("githubapi: create branch: %w", err)
	}
	return nil
}

// doPut performs one authenticated PUT against a.apiBaseURL+path with
// reqBody as the JSON request body -- the doPost-analog this package's
// own doc.go promises, needed because GitHub's Contents API create-or-
// update-file-contents endpoint (and MergePR's own merge endpoint,
// mergepr.go) is specifically a PUT, never a POST. Otherwise byte-for-
// byte the same bounded-read/error-envelope-parsing shape as doGet/
// doPost, INCLUDING doGet's own isRateLimitedResponse classification on
// a 403 (docs/TECHNICAL_PLAN.md §17's own automerge dead-letter fix):
// MergePR is this package's one caller that converts this method's
// *APIError into a SEPARATE typed error of its own (ports.MergePRError),
// so RateLimited must be computed HERE, before that conversion, or it
// would silently read as false for every 403 MergePR ever returns --
// indistinguishable from a genuine permission denial to any caller
// classifying via ports.ErrPermissionDenied.
func (a *Adapter) doPut(ctx context.Context, path, token string, reqBody []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, path, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("githubapi: build put request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubapi: put request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return nil, fmt.Errorf("githubapi: read put response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := "no error body"
		var parsed githubErrorBody
		if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
			message = parsed.Message
		} else if len(body) > 0 {
			message = "error body did not match GitHub's expected error envelope"
		}
		apiErr := &APIError{Status: resp.StatusCode, Message: message}
		if resp.StatusCode == http.StatusForbidden {
			// See this method's own doc comment above for why this must
			// be computed here rather than left to a caller further up.
			// string(body), not message -- see doGet's own identical fix
			// above for why the RAW body, not the placeholder message can
			// get overwritten to, is what isRateLimitedResponse's own
			// message-text fallback needs.
			apiErr.RateLimited = isRateLimitedResponse(resp, string(body))
		}
		return nil, apiErr
	}

	return body, nil
}

// doPatch performs one authenticated PATCH against a.apiBaseURL+path with
// reqBody as the JSON request body -- the doPut-analog (§26.2)
// needs, since GitHub's own "update a pull request" endpoint (the ONE
// real caller, prbody.go's own UpdatePRBody) is specifically a PATCH,
// never a PUT or POST. Otherwise byte-for-byte the same bounded-read/
// error-envelope-parsing shape as doGet/doPost/doPut.
func (a *Adapter) doPatch(ctx context.Context, path, token string, reqBody []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, path, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("githubapi: build patch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubapi: patch request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return nil, fmt.Errorf("githubapi: read patch response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := "no error body"
		var parsed githubErrorBody
		if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
			message = parsed.Message
		} else if len(body) > 0 {
			message = "error body did not match GitHub's expected error envelope"
		}
		apiErr := &APIError{Status: resp.StatusCode, Message: message}
		if resp.StatusCode == http.StatusForbidden {
			// Audit fix: doPatch previously never computed RateLimited at
			// all -- see doGet's own identical isRateLimitedResponse call/
			// doc comment above for why string(body) (the raw body), not
			// message, is what the fallback text check needs.
			apiErr.RateLimited = isRateLimitedResponse(resp, string(body))
		}
		return nil, apiErr
	}

	return body, nil
}
