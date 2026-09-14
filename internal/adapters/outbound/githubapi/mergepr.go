package githubapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/narvidev/narvi/internal/app/ports"
)

// mergePRRequest is the body PUT to
// /repos/{owner}/{repo}/pulls/{pull_number}/merge (real GitHub REST API
// shape -- https://docs.github.com/rest/pulls/pulls#merge-a-pull-request,
// fetched 2026-08-07, this Step's own design phase). Every field is
// documented by GitHub as optional -- merge_method defaults to the
// repository's own preference, commit_title/commit_message default to
// GitHub's own auto-generated text -- `omitempty` lets the caller omit
// any of them (ports.MergePRSpec's own doc comment: SHA is the one field
// this PORT makes mandatory, but the wire field itself stays a plain
// optional string as far as the JSON shape goes).
type mergePRRequest struct {
	CommitTitle   string `json:"commit_title,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
	SHA           string `json:"sha,omitempty"`
	MergeMethod   string `json:"merge_method,omitempty"`
}

// mergePRResponse is the subset of GitHub's real 200 response shape this
// adapter needs.
type mergePRResponse struct {
	SHA    string `json:"sha"`
	Merged bool   `json:"merged"`
}

// MergePR implements ports.SourceControl ("decision inbox: read
// model + API", §16.2's own Merge endpoint): a real PUT
// https://api.github.com/repos/{owner}/{repo}/pulls/{number}/merge call,
// authenticated with spec.Token as the ACTING user (the person who
// clicked Merge) -- mirrors CreatePR's own token-based attribution,
// reusing doPut exactly like every other write in this package.
//
// Unlike every other method in this file, a non-2xx response here is
// converted into *ports.MergePRError (never left as the raw *APIError
// doPut itself returns) -- see that type's own doc comment (internal/app/
// ports/sourcecontrol.go) for why this port's one interactive, human-
// facing method needs a typed, status-carrying error while every other
// (best-effort) method on this port stays plain. A transport/timeout
// failure that never produced a real HTTP response (doPut's error is NOT
// an *APIError) is left as a plain wrapped error instead -- errors.As
// against *ports.MergePRError correctly fails for it, mirroring
// CreatePRError's own identical "typed only for a real response" split.
func (a *Adapter) MergePR(ctx context.Context, spec ports.MergePRSpec) (string, error) {
	reqBody, err := json.Marshal(mergePRRequest{
		CommitTitle:   spec.CommitTitle,
		CommitMessage: spec.CommitMessage,
		SHA:           spec.HeadSHA,
		MergeMethod:   spec.MergeMethod,
	})
	if err != nil {
		return "", fmt.Errorf("githubapi: encode merge-pr request: %w", err)
	}

	// Owner/Repo escaped via url.PathEscape (single opaque segments,
	// mirroring every other method's own identical discipline -- see
	// escapePathSegments' own doc comment, adapter.go); spec.Number needs
	// no escaping (a plain decimal integer).
	path := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/merge", a.apiBaseURL, url.PathEscape(spec.Owner), url.PathEscape(spec.Repo), spec.Number)
	body, err := a.doPut(ctx, path, spec.Token, reqBody)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			// RateLimited carried through verbatim (doPut's own updated
			// doc comment, adapter.go) -- without it, ports.MergePRError.
			// Unwrap would read every 403 this method returns as
			// RateLimited=false, misclassifying a live rate limit as a
			// permanent ports.ErrPermissionDenied (docs/TECHNICAL_PLAN.md
			// §17's own automerge dead-letter fix).
			return "", &ports.MergePRError{Status: apiErr.Status, Message: apiErr.Message, RateLimited: apiErr.RateLimited}
		}
		return "", fmt.Errorf("githubapi: merge pr: %w", err)
	}

	var parsed mergePRResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("githubapi: decode merge-pr response: %w", err)
	}
	if !parsed.Merged {
		// Defensive: GitHub's own docs describe a 2xx response as success
		// outright, so this should be unreachable in practice -- treated
		// as a genuine failure (never silently reported as a successful
		// merge to a caller that only checks err == nil) rather than
		// trusted blindly.
		return "", &ports.MergePRError{Status: http.StatusOK, Message: "githubapi: GitHub reported a 2xx status but merged=false"}
	}

	return parsed.SHA, nil
}
