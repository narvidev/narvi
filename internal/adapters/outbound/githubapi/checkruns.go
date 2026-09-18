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
// ExternalID (finding C1) is GitHub's own "external_id" request field --
// a caller-supplied, caller-readable discriminator, entirely distinct
// from the "id" GitHub itself assigns (what checkRunResponse.ID and this
// codebase's own broader "external id" vocabulary elsewhere both name --
// see reviewcheck.PRExternalID's own doc comment for the full "why two
// things are both called this" disambiguation). Only ever populated on
// CreateCheckRun -- UpdateCheckRun never sends it, mirroring
// name/head_sha's own "not re-sent on update" precedent immediately
// below: a caller updating an EXISTING check run already knows which one
// it means (by GitHub's own numeric id), and this value never changes
// for a check run's own lifetime once created.
type checkRunRequest struct {
	Name       string          `json:"name,omitempty"`
	HeadSHA    string          `json:"head_sha,omitempty"`
	Status     string          `json:"status,omitempty"`
	Conclusion *string         `json:"conclusion,omitempty"`
	Output     *checkRunOutput `json:"output,omitempty"`
	ExternalID *string         `json:"external_id,omitempty"`
}

type checkRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// checkRunResponse is the subset of GitHub's real check-run object this
// adapter reads back -- ID is what SetExternalID persists; the rest
// (HeadSHA, App.ID, Name) are what ListCheckRunsForRef's own callers
// filter recovery candidates by. Status/Conclusion (finding A1) let a
// recovery caller exclude an already-CONCLUDED run from adoption --
// CheckRunSummary carried neither before this fix, so the adoption
// predicate had no way to tell a genuinely-recoverable in-flight/orphaned
// run apart from a PREVIOUS attempt's own already-terminal one.
type checkRunResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	// ExternalID (finding C1) is GitHub's own "external_id" response
	// field, read back so a recovery caller (ListCheckRunsForRef, below)
	// can filter candidates by it -- see checkRunRequest.ExternalID's own
	// doc comment for what this is and is not.
	ExternalID string `json:"external_id"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

// CreateCheckRun creates a NEW check run named name against headSHA,
// returning GitHub's own assigned check-run id, PLUS the id of the App
// GitHub attributed this write to (finding A2) -- read back from the
// SAME create response, never asserted from local config. A writing
// credential's own identity is only known for certain by observing what
// GitHub itself reports for a write THAT credential actually made; see
// internal/app/outboxworker's own review-check notifier for the caller
// that records this as "this deployment's own observed writer App id"
// and uses it, instead of a separately-configured value that may name an
// entirely different credential's App. conclusion == "" (
// reviewcheck.ConclusionNone) omits the field entirely -- required for
// status == "queued"/"in_progress" (GitHub rejects a conclusion on an
// incomplete run). prExternalID (finding C1) is stamped onto GitHub's own
// "external_id" request field -- reviewcheck.PRExternalID's own doc
// comment has the full "why": without it, two pull requests sharing one
// head commit are indistinguishable to a later recovery/adoption read,
// which sees only the commit, never the pull request.
func (a *Adapter) CreateCheckRun(ctx context.Context, owner, repo, token, headSHA, name, status, conclusion, title, summary, prExternalID string) (id int64, appID int64, err error) {
	reqBody, err := json.Marshal(checkRunRequest{
		Name:       name,
		HeadSHA:    headSHA,
		Status:     status,
		Conclusion: nonEmptyStringPtr(conclusion),
		Output:     &checkRunOutput{Title: title, Summary: summary},
		ExternalID: nonEmptyStringPtr(prExternalID),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("githubapi: encode create-check-run request: %w", err)
	}

	path := fmt.Sprintf("%s/repos/%s/%s/check-runs", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo))
	body, err := a.doPost(ctx, path, token, reqBody)
	if err != nil {
		return 0, 0, fmt.Errorf("githubapi: create check run: %w", err)
	}
	var parsed checkRunResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, 0, fmt.Errorf("githubapi: decode create-check-run response: %w", err)
	}
	if parsed.ID == 0 {
		return 0, 0, fmt.Errorf("githubapi: create check run: empty id in response")
	}
	return parsed.ID, parsed.App.ID, nil
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
// entries whose App.ID matches this deployment's own OBSERVED writer App
// id (finding A2 -- never a separately-configured value that may name a
// different credential's App entirely) AND whose Name equals
// reviewcheck.CheckName, so another app's same-named check run is never
// adopted, AND whose Status is not yet "completed" (finding A1 -- a
// concluded check run is never a recovery candidate: "open a NEW check
// when a review restarts after a terminal result... never reopen a
// concluded one"), AND whose ExternalID matches the requesting pull
// request's own reviewcheck.PRExternalID (finding C1 -- a check run is
// scoped to a commit, never to a pull request, so two open pull requests
// sharing one head commit see the identical set of runs here; without
// this last filter, the second such pull request's own recovery read
// adopts the first one's run), so a crash between a successful create and
// this system's own record of its id can recover the real one rather than
// creating a duplicate.
//
// per_page=100, mirroring every other list GET in this adapter
// (fetchCIConclusionLive's own identical fix, listopenprs.go) --
// GitHub's undocumented default page size (30) would otherwise silently
// serve only a PREFIX of a ref carrying many check runs. A prefix that
// happens to omit narvi/review's own entry reads identically to "no
// existing check run" and this adapter's own caller falls back to
// CREATING one -- a BOUNDED consequence (only reachable on a ref already
// carrying more than 100 other check runs), but not a harmless one: the
// missed entry is the SAME shape of orphan finding A7 reassesses
// (internal/app/outboxworker's own reviewCheckNotifier.Deliver, its doc
// comment above the resolveOrCreateCheckRun call, is this claim's own
// source of truth) -- it sits on the ref's own Checks tab exactly as it
// was, nothing in this system ever writing to it again. Not a silent
// WRONG answer the way a missed CI failure signal would be (this method
// does not itself refuse on a partial page the way fetchCIConclusionLive's
// own live CI gate does), but not nothing either.
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
		out = append(out, CheckRunSummary{ID: r.ID, Name: r.Name, HeadSHA: r.HeadSHA, AppID: r.App.ID, Status: r.Status, Conclusion: r.Conclusion, ExternalID: r.ExternalID})
	}
	return out, nil
}

// CheckRunSummary is ListCheckRunsForRef's own return shape -- the
// fields a recovery caller needs to decide "is this MY app's
// narvi/review check run for this exact SHA, and is it still safe to
// adopt", never GitHub's full check-run object. Status/Conclusion
// (finding A1) are GitHub's own raw strings, deliberately NOT this
// codebase's own reviewcheck.Status/reviewcheck.Conclusion types --
// this adapter package has no dependency on internal/domain/reviewcheck
// (mirrors this file's own top doc comment: "the wire protocol only",
// every decision lives with the caller), so a caller compares Status
// against reviewcheck.StatusCompleted's own string value, never a type
// this package would need to import reviewcheck to produce.
type CheckRunSummary struct {
	ID         int64
	Name       string
	HeadSHA    string
	AppID      int64
	Status     string
	Conclusion string
	// ExternalID (finding C1) is GitHub's own "external_id" field, read
	// back verbatim -- the per-pull-request discriminator
	// reviewcheck.PRExternalID computes and CreateCheckRun stamps onto
	// every run this publisher creates. Empty for any check run created
	// before this fix shipped (GitHub never backfills a field an old
	// create call never sent), so it never accidentally matches a
	// caller's own non-empty expected value -- an old run degrades to
	// "never adopted", the same safe direction every other unrecognized-
	// candidate case in this file already degrades to, never a false
	// match.
	ExternalID string
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
