package githubapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// This file implements the review's own GitHub-native result surface
// (§8.2/§21.1/§21.1b): three low-level GitHub Checks API
// calls -- CreateCheckRun, UpdateCheckRun, ListCheckRunsForRef -- the
// wire protocol only. Every decision about WHICH of the three to call,
// WHEN, and how to reconcile concurrent writers lives in internal/app/
// outboxworker's own review-check notifier, the one caller of all three;
// this file, like CreateCommitStatus/PostIssueComment beside it, is pure
// request-building/response-decoding over doPost/doPatch/doGet.
//
// Today, check-runs are read (fetchCIConclusionLive, listopenprs.go) and
// never written -- these three methods are this codebase's first writes
// to that API.

// checkRunRequest is the body both CreateCheckRun and UpdateCheckRun
// POST/PATCH -- GitHub's own documented "create a check run"/"update a
// check run" request shape. Conclusion/CompletedAt are `omitempty`
// (via *string/*string so an EMPTY conclusion -- reviewcheck.
// ConclusionNone, for an incomplete queued/in_progress run -- is never
// serialized at all: GitHub's own API rejects a conclusion field on an
// incomplete check run, and an explicit empty string is not the same
// wire shape as an absent field).
type checkRunRequest struct {
	Name       string          `json:"name,omitempty"`
	HeadSHA    string          `json:"head_sha,omitempty"`
	Status     string          `json:"status,omitempty"`
	Conclusion *string         `json:"conclusion,omitempty"`
	Output     *checkRunOutput `json:"output,omitempty"`
}

type checkRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// checkRunResponse is the subset of GitHub's real check-run object this
// adapter reads back -- ID is what SetExternalID persists; the rest
// (HeadSHA, App.ID, Name) are what ListCheckRunsForRef's own callers
// filter recovery candidates by.
type checkRunResponse struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	HeadSHA string `json:"head_sha"`
	App     struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

// CreateCheckRun creates a NEW check run named name against headSHA,
// returning GitHub's own assigned check-run id. conclusion == "" (
// reviewcheck.ConclusionNone) omits the field entirely -- required for
// status == "queued"/"in_progress" (GitHub rejects a conclusion on an
// incomplete run).
func (a *Adapter) CreateCheckRun(ctx context.Context, owner, repo, token, headSHA, name, status, conclusion, title, summary string) (int64, error) {
	reqBody, err := json.Marshal(checkRunRequest{
		Name:       name,
		HeadSHA:    headSHA,
		Status:     status,
		Conclusion: nonEmptyStringPtr(conclusion),
		Output:     &checkRunOutput{Title: title, Summary: summary},
	})
	if err != nil {
		return 0, fmt.Errorf("githubapi: encode create-check-run request: %w", err)
	}

	path := fmt.Sprintf("%s/repos/%s/%s/check-runs", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo))
	body, err := a.doPost(ctx, path, token, reqBody)
	if err != nil {
		return 0, fmt.Errorf("githubapi: create check run: %w", err)
	}
	var parsed checkRunResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("githubapi: decode create-check-run response: %w", err)
	}
	if parsed.ID == 0 {
		return 0, fmt.Errorf("githubapi: create check run: empty id in response")
	}
	return parsed.ID, nil
}

// UpdateCheckRun PATCHes an EXISTING check run (checkRunID, GitHub's own
// assigned id -- never re-resolved by name/SHA here; the caller already
// knows which one it means to update) with a new status/conclusion/
// output. name/headSHA are NOT sent -- GitHub's own "update a check run"
// endpoint neither requires nor changes either; a caller that needs to
// target a DIFFERENT head SHA creates a new check run instead (this
// package's own doc comment on CreateCheckRun; internal/domain/
// reviewcheck's own doc comment on why a check run's identity rotates
// with the head).
func (a *Adapter) UpdateCheckRun(ctx context.Context, owner, repo, token string, checkRunID int64, status, conclusion, title, summary string) error {
	reqBody, err := json.Marshal(checkRunRequest{
		Status:     status,
		Conclusion: nonEmptyStringPtr(conclusion),
		Output:     &checkRunOutput{Title: title, Summary: summary},
	})
	if err != nil {
		return fmt.Errorf("githubapi: encode update-check-run request: %w", err)
	}

	path := fmt.Sprintf("%s/repos/%s/%s/check-runs/%d", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), checkRunID)
	if _, err := a.doPatch(ctx, path, token, reqBody); err != nil {
		return fmt.Errorf("githubapi: update check run: %w", err)
	}
	return nil
}

// listCheckRunsForRefResponse mirrors GitHub's own "list check runs for a
// git reference" response envelope -- total_count PLUS the served page,
// exactly like checkRunsResponse (mergedbetween.go) already does for the
// CI-reading surface; this is a SEPARATE, purpose-specific type (never
// widened/reused) because that one only ever reads Conclusion, while
// ListCheckRunsForRef below needs ID/Name/App.ID too.
type listCheckRunsForRefResponse struct {
	TotalCount int                `json:"total_count"`
	CheckRuns  []checkRunResponse `json:"check_runs"`
}

// ListCheckRunsForRef lists every check run GitHub has recorded for ref
// (a commit SHA) -- the "select by SHA and GitHub App" recovery read
// (the brief's own identity rule): a caller filters the result to
// entries whose App.ID matches this deployment's OWN configured GitHub
// App id AND whose Name equals reviewcheck.CheckName, so another app's
// same-named check run is never adopted, and so a crash between a
// successful create and this system's own record of its id can recover
// the real one rather than creating a duplicate.
//
// per_page=100, mirroring every other list GET in this adapter
// (fetchCIConclusionLive's own identical fix, listopenprs.go) --
// GitHub's undocumented default page size (30) would otherwise silently
// serve only a PREFIX of a ref carrying many check runs. A prefix that
// happens to omit narvi/review's own entry reads identically to "no
// existing check run" and this adapter's own caller falls back to
// CREATING one -- a harmless, bounded consequence (a second check run
// object on a ref that already carried more than 100 others), not a
// silent wrong answer the way a missed CI failure signal would be, so
// this method does not itself refuse on a partial page the way
// fetchCIConclusionLive's own live CI gate does.
func (a *Adapter) ListCheckRunsForRef(ctx context.Context, owner, repo, ref, token string) ([]CheckRunSummary, error) {
	path := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs?per_page=100", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref))
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return nil, fmt.Errorf("githubapi: list check runs for ref: %w", err)
	}
	var parsed listCheckRunsForRefResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("githubapi: decode list-check-runs-for-ref response: %w", err)
	}
	out := make([]CheckRunSummary, 0, len(parsed.CheckRuns))
	for _, r := range parsed.CheckRuns {
		out = append(out, CheckRunSummary{ID: r.ID, Name: r.Name, HeadSHA: r.HeadSHA, AppID: r.App.ID})
	}
	return out, nil
}

// CheckRunSummary is ListCheckRunsForRef's own return shape -- the
// fields a recovery caller needs to decide "is this MY app's
// narvi/review check run for this exact SHA", never GitHub's full
// check-run object.
type CheckRunSummary struct {
	ID      int64
	Name    string
	HeadSHA string
	AppID   int64
}

// nonEmptyStringPtr returns nil for an empty string, &s otherwise --
// checkRunRequest's own `omitempty` needs a nil pointer, not a pointer to
// an empty string, to omit the field entirely (see checkRunRequest's own
// doc comment for why that distinction matters to GitHub's own API).
func nonEmptyStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
