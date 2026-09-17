// This file (listopenprs.go) implements ports.SourceControl.
// ListOpenPRsForUser ("decision inbox: read model + API", §16.2):
// "review state, CI at head SHA, labels, assignees/reviewers" for every
// open PR a user is involved in.
//
// # Resolving a login from spec.GitHubExternalID
//
// This codebase's own identity graph (§13.2) stores ONLY the stable,
// numeric GitHub account id (identities.external_id, populated from the
// OAuth /user response's own "id" field at sign-in,
// internal/adapters/inbound/auth/callback.go) -- never the account's
// LOGIN. Verified directly before writing this file (grepped the whole
// codebase for github_login/GitHubLogin/githubUsername and every schema
// file): the login is used ONLY transiently, at OAuth callback time (for
// the allowlist org-membership check and as a display-name fallback), and
// is never persisted anywhere. GitHub's own search qualifiers this file
// needs (assignee:, review-requested:) require a LOGIN, not a numeric id
// -- so this method's own first call resolves one via GitHub's real,
// documented "Get a user using their ID" endpoint (GET /user/{account_id},
// https://docs.github.com/rest/users/users#get-a-user-using-their-id,
// fetched 2026-08-07: "This method takes their durable user ID instead of
// their login, which can change over time"), exactly the durable-id-to-
// current-login resolution this endpoint exists for.
//
// # Discovering candidate PRs
//
// GitHub's Search API supports exactly the qualifiers this method needs,
// verified directly against GitHub's own documentation (fetched
// 2026-08-07): "assignee:USERNAME" and "review-requested:USERNAME".
// review-requested's own documented behavior already folds in team-based
// requests: "If the requested person is on a team that is requested for
// review, then review requests for that team will also appear in the
// search results" -- so this ONE query surfaces both an individually-
// requested reviewer AND one reached via team membership, with no
// separate team-membership lookup needed for DISCOVERY (ResolveCodeOwners
// is still needed separately to determine WHETHER a given match came via
// a CODEOWNERS pattern specifically, for §16.1's own provenance
// requirement -- that is an ENRICHMENT of an already-discovered PR, not a
// second discovery mechanism; see the app-layer aggregator).
//
// This deliberately does NOT run an "author:" query: §16.1's own three
// assignment-provenance paths are "directly, as requested reviewer, or
// via CODEOWNERS" -- authorship on its own is not one of them (a PR the
// user AUTHORED but is neither assigned to nor requested to review is not
// "assigned to the user" in the sense this Step's taxonomy means; the
// decision inbox is about decisions ADDRESSED to the user, not a list of
// their own outgoing work).
//
// This also does NOT scope the search to a configured set of Narvi-
// managed repos/orgs -- there is no such registry anywhere in this
// codebase today (repo_settings rows only exist for repos an admin has
// already touched a toggle for, not a canonical "every repo Narvi
// manages" list). Running the user's own token unscoped means the result
// is exactly "every open PR on GitHub this person can see and is
// assigned/requested on" -- a known, honestly-scoped choice: a user whose
// GitHub account also touches repos outside anything Narvi cares about
// would see those PRs too. The cost is a UX nicety gap (a few
// out-of-scope rows), never a correctness/security one -- every row still
// goes through the SAME server-side re-validation before any action is
// taken (§16.2, §5.2), so an out-of-scope row is inert, not exploitable.
//
// # Cost
//
// Two Search API calls (30 req/min budget -- see mergedbetween.go's own
// identical caution) plus up to five further ordinary REST calls (detail,
// reviews, CI's own two surfaces, changed files) PER discovered candidate
// PR, bounded by maxOpenPRsForUser -- the SAME "genuinely expensive, no
// cheaper way through GitHub's REST API as it exists today" cost class
// mergedbetween.go's own top doc comment already accepts for
// ListMergedBetween, amortized here by §16.2's own short-TTL cache at the
// app layer rather than by this adapter itself (which holds no state
// between calls, matching every other method in this package).

package githubapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/narvidev/narvi/internal/app/ports"
)

// maxOpenPRsForUser bounds how many discovered candidate PRs
// ListOpenPRsForUser ever builds full OpenPR detail for -- mirrors
// maxConstituentPRs' own identical "bounded from day one" reasoning
// (§21.1) rather than an unbounded per-user candidate list driving an
// unbounded number of outbound calls. A person legitimately assigned to
// or requested on more than this many open PRs at once is a real, if
// unusual, case this file accepts as a known limitation (the search
// results beyond this bound are simply never resolved into full OpenPR
// rows) rather than an unbounded worst case.
const maxOpenPRsForUser = 50

// maxOpenPRSearchResultsPerQuery mirrors maxRevertSearchResults'
// (mergedbetween.go) own identical "GitHub's Search API caps a single
// page at 100 results, and this adapter never paginates beyond that one
// page" acceptance.
const maxOpenPRSearchResultsPerQuery = 100

// openPRSearchItemResponse is the subset of one entry in GitHub's real GET
// /search/issues response's own "items" array this file needs -- a
// DIFFERENT subset than mergedbetween.go's own searchIssueItemResponse
// (which needs Title/Body/ClosedAt for revert detection): this file only
// ever needs enough to identify WHICH repo/PR a search hit names, since
// buildOpenPR (below) re-fetches everything else it needs via a full
// detail call regardless.
type openPRSearchItemResponse struct {
	Number        int    `json:"number"`
	RepositoryURL string `json:"repository_url"`
}

type openPRSearchResponse struct {
	Items []openPRSearchItemResponse `json:"items"`
}

// openPRDetailResponse is the subset of GitHub's real GET
// /repos/{owner}/{repo}/pulls/{pull_number} response shape this file
// needs, beyond what pullRequestResponse (adapter.go, H5's narrower head-
// branch-resolution need) or mergedPRDetailResponse (mergedbetween.go,
// scoped to an already-MERGED PR) already decode -- verified directly
// against GitHub's own documentation (fetched 2026-08-07): requested_
// reviewers/requested_teams/assignees/draft are real, documented fields
// on this exact response.
type openPRDetailResponse struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	// State (round-10 finding E; doc comment corrected, round-11 finding
	// E) is GitHub's own documented "open"/"closed" scalar on this SAME
	// "Get a pull request" response -- GetOpenPR's own doc comment
	// (ports/sourcecontrol.go) promises "found=false, err=nil means the
	// PR does not exist, or is no longer open (closed/merged)", a
	// two-halved contract only HALF of which was ever true before this
	// fix. The confirmed-ABSENT half (a 404 from GitHub) was ALREADY
	// correctly handled from GetOpenPR's own first version -- that
	// function's own fetchOpenPRDetail error path checks
	// apiErr.Status == http.StatusNotFound and returns found=false
	// unconditionally, entirely independent of this field, which a 404
	// response never even carries (there is no response body to decode
	// State out of at all). The gap this fix closes is narrower, and
	// different in kind: a confirmed-PRESENT-but-closed-or-merged PR (a
	// real 200 response, this field reading "closed") reached GetOpenPR's
	// own caller as found=true, identically to a genuinely open PR --
	// until this field was decoded at all, nothing on this response shape
	// could tell the two apart. A merged PR ALSO reports state=="closed"
	// here (GitHub never uses a separate "merged" state value), so
	// checking this one field alone catches both closed and merged,
	// matching that doc comment's own "closed/merged" wording exactly.
	State     string `json:"state"`
	Draft     bool   `json:"draft"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	// ChangedFiles (Phase 5 audit finding 2, fixed) is GitHub's own
	// top-level "changed_files" scalar on this SAME "Get a pull request"
	// response -- mirrors pullRequestResponse.ChangedFiles' own identical
	// field (adapter.go, §26.3), simply unparsed on THIS response shape
	// until this fix: ports.OpenPR.ChangedFilesCount below is populated
	// from this scalar, never from len() of the SEPARATE, page-capped
	// Pull Request Files listing fetchChangedFilePaths (below) fetches.
	ChangedFiles int    `json:"changed_files"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`

	User *simpleUserResponse `json:"user"`

	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
	// Base.SHA (§21.1's amendment) is this response shape's own decode of
	// GitHub's per-PR CACHED base commit -- ports.OpenPR.BaseSHA below is
	// this response's one consumer, and that field's own doc comment
	// (ports/sourcecontrol.go) is normative for why it is retained for
	// display/audit only, never wired into an eligibility comparison.
	// adapter.go's own pullRequestResponse -- a DIFFERENT response shape,
	// GetPullRequest's, not this one -- no longer decodes the equivalent
	// field at all (D11): the two shapes are not mirrors of each other on
	// this point, unlike before D11.
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`

	// Stack (§21.1's amendment) mirrors pullRequestResponse.Stack's own
	// identical GitHub-native-stack object (adapter.go, §17.6) -- reuses
	// that file's own stackResponse shape verbatim, this package's one
	// other decode target for it, so ports.OpenPR.AncestorChain below can
	// be derived the SAME way review.PreFetchedContext.AncestorChain
	// already is at review-context-fetch time.
	Stack *stackResponse `json:"stack"`

	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`

	Assignees          []simpleUserResponse `json:"assignees"`
	RequestedReviewers []simpleUserResponse `json:"requested_reviewers"`
	// RequestedTeams only carries Slug -- a requested team's own
	// "organization" is, on a real GitHub repo, always the SAME org that
	// owns the repo itself (an org-owned repo can only grant a team
	// permissions if that team belongs to that same org; a personal-
	// account-owned repo can carry no teams at all) -- buildOpenPR below
	// combines this Slug with the ALREADY-KNOWN repo owner rather than
	// decoding a second, nested organization object for a fact the repo
	// owner already gives it.
	RequestedTeams []struct {
		Slug string `json:"slug"`
	} `json:"requested_teams"`
}

// ListOpenPRsForUser implements ports.SourceControl -- see this file's own
// top doc comment for the full design (login resolution, candidate
// discovery, cost).
//
// truncated is true iff at least one of the two
// discovery queries below itself failed -- a genuine coverage gap, since
// the SURVIVING query's own results are still returned (never blanked
// out, this function's own established best-effort-per-query discipline,
// unchanged), but the FAILING query's own candidates are simply never
// discovered at all. This method still reports no analogous truncation
// signal for a PER-PR buildOpenPR failure below (unlike the top-level
// discovery queries) -- §16.2 already frames this whole read as advisory,
// short-TTL-cached, and re-validated at action time, so an occasionally-
// missing ROW is a staleness/coverage nicety, never a correctness hazard
// the way missing an entire QUERY's worth of candidates is.
func (a *Adapter) ListOpenPRsForUser(ctx context.Context, spec ports.ListOpenPRsForUserSpec) (prs []ports.OpenPR, truncated bool, err error) {
	login, err := a.resolveLoginByID(ctx, spec.GitHubExternalID, spec.Token)
	if err != nil {
		return nil, false, fmt.Errorf("githubapi: list open prs for user: resolve login: %w", err)
	}

	type prKey struct {
		owner, repo string
		number      int
	}
	seen := make(map[prKey]bool)
	var candidates []prKey

	for _, qualifier := range []string{"assignee", "review-requested"} {
		items, searchErr := a.searchOpenPRs(ctx, qualifier, login, spec.Token)
		if searchErr != nil {
			// Best-effort per-query: one qualifier's own search failing
			// (e.g. a transient 5xx) should never blank out whatever the
			// OTHER qualifier already found -- but it DOES mean this
			// call's own result is an incomplete picture, so truncated is
			// set regardless of whether the OTHER qualifier's own query
			// still succeeds.
			truncated = true
			continue
		}
		for _, item := range items {
			owner, repo, ok := splitRepositoryURL(item.RepositoryURL)
			if !ok {
				continue
			}
			key := prKey{owner: owner, repo: repo, number: item.Number}
			if seen[key] {
				continue
			}
			seen[key] = true
			candidates = append(candidates, key)
			if len(candidates) >= maxOpenPRsForUser {
				break
			}
		}
		if len(candidates) >= maxOpenPRsForUser {
			break
		}
	}

	result := make([]ports.OpenPR, 0, len(candidates))
	for _, c := range candidates {
		pr, ok := a.buildOpenPR(ctx, c.owner, c.repo, c.number, spec.Token)
		if !ok {
			// Best-effort per-PR, mirrors buildMergedPR's own identical
			// "a genuine sub-fetch failure excludes just this one row"
			// discipline (mergedbetween.go) -- see this method's own top
			// doc comment for why this specific degrade is NOT folded
			// into truncated, unlike a whole discovery query failing
			// above.
			continue
		}
		result = append(result, pr)
	}
	return result, truncated, nil
}

// resolveLoginByID resolves externalID (a numeric GitHub account id, as a
// decimal string -- ports.ListOpenPRsForUserSpec.GitHubExternalID's own
// doc comment) to that account's CURRENT login, via GET
// /user/{account_id} -- see this file's own top doc comment.
func (a *Adapter) resolveLoginByID(ctx context.Context, externalID, token string) (string, error) {
	path := fmt.Sprintf("%s/user/%s", a.apiBaseURL, url.PathEscape(externalID))
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return "", err
	}
	var parsed simpleUserResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("githubapi: decode user-by-id response: %w", err)
	}
	if parsed.Login == "" {
		return "", fmt.Errorf("githubapi: user id %s reported an empty login", externalID)
	}
	return parsed.Login, nil
}

// searchOpenPRs runs "is:pr is:open <qualifier>:<login>" -- see this
// file's own top doc comment for why review-requested alone (no separate
// team-review-requested query) already covers team-based requests too.
func (a *Adapter) searchOpenPRs(ctx context.Context, qualifier, login, token string) ([]openPRSearchItemResponse, error) {
	query := fmt.Sprintf("is:pr is:open %s:%s", qualifier, login)
	path := fmt.Sprintf("%s/search/issues?q=%s&per_page=%d", a.apiBaseURL, url.QueryEscape(query), maxOpenPRSearchResultsPerQuery)
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return nil, fmt.Errorf("githubapi: search open prs (%s): %w", qualifier, err)
	}
	var parsed openPRSearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("githubapi: decode open-pr search results (%s): %w", qualifier, err)
	}
	return parsed.Items, nil
}

// splitRepositoryURL extracts owner/repo from GitHub's own search-item
// "repository_url" field, shaped "https://api.github.com/repos/{owner}/
// {repo}" (GitHub's own documented, stable shape for this field).
func splitRepositoryURL(repositoryURL string) (owner, repo string, ok bool) {
	const marker = "/repos/"
	idx := strings.Index(repositoryURL, marker)
	if idx < 0 {
		return "", "", false
	}
	rest := repositoryURL[idx+len(marker):]
	owner, repo, ok = strings.Cut(rest, "/")
	if !ok || owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

// buildOpenPR assembles one ports.OpenPR for (owner, repo, number) --
// ok=false means a genuine sub-fetch failure on the CENTRAL detail call
// (mirrors buildMergedPR's own identical "the detail fetch itself failing
// excludes the whole row" precedent); a failure on any of the other,
// SECONDARY fetches (reviews, CI, changed files) instead degrades that
// ONE field to its own honest zero-value/Unknown rather than excluding
// the PR outright -- the SAME per-field-degrade discipline buildMergedPR
// already establishes.
// buildOpenPR fetches number's own detail and builds a full ports.OpenPR
// -- ok=false means the detail fetch itself failed (any reason: not
// found, transient, rate-limited -- this caller, ListOpenPRsForUser's
// own per-candidate loop, treats every failure identically as "drop this
// one row", so it has never needed to distinguish them). §21's own
// GetOpenPR (getopenpr.go) below needs a FINER distinction (genuinely
// not found vs. a real error worth propagating), so it calls
// fetchOpenPRDetail itself and hands the result to buildOpenPRFromDetail
// directly, rather than through this wrapper -- see that file's own doc
// comment.
func (a *Adapter) buildOpenPR(ctx context.Context, owner, repo string, number int, token string) (ports.OpenPR, bool) {
	detail, err := a.fetchOpenPRDetail(ctx, owner, repo, number, token)
	if err != nil {
		return ports.OpenPR{}, false
	}
	// Round-11 finding C: GetOpenPR (getopenpr.go, round-10 finding E)
	// checks detail.State to exclude a closed-or-merged PR that a
	// confirmed-present 200 response can still report -- that fix landed
	// on GetOpenPR alone, leaving THIS caller's identical hazard open:
	// searchOpenPRs' own "is:pr is:open" qualifier (this file's own top
	// doc comment) queries GitHub's Search API index, which is
	// eventually consistent and can still surface a candidate that has
	// since closed or merged in the gap between the search and this
	// detail fetch. This caller's own established "ok=false, drop this
	// one row" semantics (this function's own doc comment above) is
	// exactly the right shape for that race -- mirrors GetOpenPR's
	// identical check, never a second, independently-decided threshold,
	// so the human merge-click path (ListOpenPRsForUser, via
	// decisioninbox.RevalidateForMerge) no longer treats a closed/merged
	// PR as open while the machine-initiated path (GetOpenPR, via
	// RevalidateForAutoMerge) already refuses it.
	if detail.State != "open" {
		return ports.OpenPR{}, false
	}
	return a.buildOpenPRFromDetail(ctx, owner, repo, detail, token), true
}

// buildOpenPRFromDetail is buildOpenPR's own construction half, taking an
// ALREADY-FETCHED detail -- the one place both buildOpenPR (above) and
// GetOpenPR (getopenpr.go) build a ports.OpenPR from, so the two never
// drift into two independently-maintained constructions of the identical
// shape.
func (a *Adapter) buildOpenPRFromDetail(ctx context.Context, owner, repo string, detail openPRDetailResponse, token string) ports.OpenPR {
	number := detail.Number

	hasApproving, hasChangesRequested, reviewDecisionDegraded := a.fetchReviewDecision(ctx, owner, repo, number, token)

	ci := ports.CIConclusionUnknown
	if detail.Head.SHA != "" {
		// fetchCIConclusionLive, deliberately NOT fetchCIConclusion -- see that function's own doc comment for
		// why a LIVE, pre-merge gate needs a STRICT conclusion, distinct
		// from mergedbetween.go's retrospective-audit-only lenient one.
		ci = a.fetchCIConclusionLive(ctx, owner, repo, detail.Head.SHA, token)
	}

	// Phase 5 audit findings 1+2 (both fixed). Two INDEPENDENT ways this
	// PR's changed-file LISTING can fail to be the complete picture
	// detail.ChangedFiles (GitHub's own authoritative scalar, ALWAYS
	// reliable here -- see ports.OpenPR.ChangedFilesCount's own doc
	// comment for why) reports:
	//
	//  1. The fetch itself fails outright (finding 1) -- files is nil,
	//     exactly like before this fix. Before this fix, THIS was the
	//     silent-permissive hole: a caller reading len(nil)==0 as "zero
	//     files changed" for an auto-merge eligibility gate.
	//  2. The fetch succeeds but detail.ChangedFiles (the true total)
	//     exceeds len(files) -- this one page (per_page=100,
	//     fetchChangedFilePaths' own doc comment) is a genuine, truncated
	//     PREFIX of a larger diff (finding 2). A PR author fully controls
	//     both filenames and diff order, so this is attacker-
	//     influenceable: padding a diff with 100+ innocuous files pushes
	//     a genuinely sensitive one past the page boundary.
	//
	// Either way, changedFilesListDegraded is set true -- ports.OpenPR.
	// ChangedFilesListDegraded's own doc comment for the fail-closed
	// contract this signals to a caller deriving sensitive-path facts
	// from files. files itself is left as whatever was actually fetched
	// (nil on failure, a real but partial slice on truncation) rather
	// than blanked out on truncation specifically -- ResolveCodeOwners'
	// own consumption of ChangedFiles already accepts a page-capped
	// listing as an honestly-scoped approximation (this field's own doc
	// comment), and blanking a real partial listing would only make that
	// unrelated, already-accepted use worse for no eligibility-side
	// benefit (the eligibility gate reads changedFilesListDegraded
	// directly, never files' own nil-ness, to decide "known" vs.
	// "unknown").
	files, filesErr := a.fetchChangedFilePaths(ctx, owner, repo, number, token)
	changedFilesListDegraded := filesErr != nil
	if filesErr != nil {
		files = nil
	} else if detail.ChangedFiles > len(files) {
		changedFilesListDegraded = true
	}

	labels := make([]string, len(detail.Labels))
	for i, l := range detail.Labels {
		labels[i] = l.Name
	}

	assignees := make([]ports.PRPerson, 0, len(detail.Assignees))
	for _, u := range detail.Assignees {
		assignees = append(assignees, ports.PRPerson{ExternalID: strconv.FormatInt(u.ID, 10), Login: u.Login})
	}
	reviewers := make([]ports.PRPerson, 0, len(detail.RequestedReviewers))
	for _, u := range detail.RequestedReviewers {
		reviewers = append(reviewers, ports.PRPerson{ExternalID: strconv.FormatInt(u.ID, 10), Login: u.Login})
	}
	teams := make([]string, 0, len(detail.RequestedTeams))
	for _, tm := range detail.RequestedTeams {
		if tm.Slug != "" {
			teams = append(teams, owner+"/"+tm.Slug)
		}
	}

	var author ports.PRPerson
	if detail.User != nil {
		author = ports.PRPerson{ExternalID: strconv.FormatInt(detail.User.ID, 10), Login: detail.User.Login}
	}

	pr := ports.OpenPR{
		Owner: owner,
		Repo:  repo,

		Number:  detail.Number,
		Title:   detail.Title,
		HTMLURL: detail.HTMLURL,

		HeadSHA:       detail.Head.SHA,
		BaseRef:       detail.Base.Ref,
		BaseSHA:       detail.Base.SHA,
		AncestorChain: ancestorChainFromDetailStack(detail.Stack),
		Draft:         detail.Draft,

		Author:             author,
		Assignees:          assignees,
		RequestedReviewers: reviewers,
		RequestedTeams:     teams,

		HasApprovingReview:  hasApproving,
		HasChangesRequested: hasChangesRequested,
		// fetchReviewDecision's own third return --
		// see that field's own doc comment (ports.OpenPR) for the full
		// "why" and which callers must fail closed on it.
		ReviewDecisionDegraded: reviewDecisionDegraded,

		CIConclusion: ci,
		Labels:       labels,

		ChangedFiles: files,
		// Phase 5 audit findings 1+2: ChangedFilesCount is GitHub's own
		// authoritative scalar (never len(files), which is truncated at
		// one page); ChangedFilesListDegraded is set immediately above.
		ChangedFilesCount:        detail.ChangedFiles,
		ChangedFilesListDegraded: changedFilesListDegraded,
		Additions:                detail.Additions,
		Deletions:                detail.Deletions,
	}
	if t, parseErr := time.Parse(time.RFC3339, detail.CreatedAt); parseErr == nil {
		pr.CreatedAt = t
	}
	if t, parseErr := time.Parse(time.RFC3339, detail.UpdatedAt); parseErr == nil {
		pr.UpdatedAt = t
	}

	return pr
}

// ancestorChainFromDetailStack derives the SAME "position <= 1 or no
// stack -> no link, otherwise exactly one link" SHAPE internal/domain/
// review.AncestorChainFromStack derives, one layer down -- this port
// (§4.3) stays domain-free, so it builds its own ports.PRAncestorLink
// slice directly from stack rather than importing that domain function.
//
// D1 (round-12 sweep, execution-verified -- this is the fix, not merely a
// restated intent): round-11 changed AncestorChainFromStack so that
// "stack.Position > 1" ALONE proves a link exists, dropping that
// function's own PREVIOUS UltimateBaseRef-must-be-non-empty clause -- a
// degraded stack read (position > 1 but no base ref decoded) now
// collapses into the SAME explicit unknown-marker link every OTHER
// resolution failure in this codebase already uses, never into the nil
// that means "genuinely no ancestor" (see that function's own doc
// comment, internal/domain/review/context.go). ROUND-11 CHANGED ONE SIDE
// OF THIS PORT AND NOT THE OTHER: this function, the SOLE producer of
// ports.OpenPR.AncestorChain (the live side every real ComputeEligible
// caller actually reads), still carried its OWN stack.Base.Ref-is-empty
// clause and still returned nil for the identical degraded input --
// executed side by side on stackResponse{Position: 2, Base.Ref: ""}
// (githubapi, this function) versus StackContext{Position: 2,
// UltimateBaseRef: ""} (review.AncestorChainFromStack): nil here, a
// real one-link unknown marker there. Fixed by dropping the SAME clause
// this function's own sibling dropped -- position > 1 now reports a
// link UNCONDITIONALLY, exactly like that sibling, so a degraded read
// (an empty stack.Base.Ref, stack.Base.SHA, or both) still reports a
// non-nil, single-element chain a caller can tell apart from "not
// stacked at all" by its own empty Ref -- ports.PRAncestorLink.Ref == ""
// in a NON-NIL chain is this port's own dedicated "could not be
// established" marker (internal/app/decisioninbox's aggregate.go/
// revalidate.go both handle it as such, round-12 sweep), mirroring
// review.AncestorLink.SHA == ""'s identical role one layer down --
// distinct fields carry the marker on each side only because this
// function has no live SHA-resolution capability at all (this comment's
// own next paragraph) and so can never itself produce a link with a
// known ref but an unknown sha the way the domain function's
// caller-supplied liveUltimateBaseSHA can.
//
// Round-11 finding E (corrected): the PREVIOUS version of this comment
// claimed this function is "kept in exact sync" with AncestorChainFromStack
// -- true when it was written, no longer true since round-10 finding B
// changed that domain function's own signature to take a SEPARATE,
// caller-supplied, LIVE-resolved SHA parameter rather than reading a
// stack's own embedded SHA field directly. This function has no
// equivalent live-resolution capability at all -- it is a pure decode of
// one already-fetched GitHub response, with no SourceControl port in
// scope to call -- so stack.Base.SHA below is, and can only ever be,
// GitHub's own PER-PR CACHED stack field (the same shape ports.OpenPR.
// BaseSHA's own doc comment already documents as stale-by-design for the
// immediate base, finding F1). ports.PRAncestorLink.SHA is therefore a
// CACHED value here, never a live one -- this function's own two real
// callers (buildOpenPRFromDetail, above, feeding ports.OpenPR.AncestorChain;
// and GetOpenPR, getopenpr.go) both document that their own SHA is
// display/audit data only, and BOTH of ComputeEligible's real call sites
// (internal/app/decisioninbox's revalidateCore/computeRealEligibility)
// deliberately read ONLY this result's Ref field, re-resolving the SHA
// live themselves via SourceControl.ResolveBranchSHA -- exactly the
// "never the cached field" discipline BaseSHA already established one
// layer up, applied here by the CALLER rather than by this function
// itself.
func ancestorChainFromDetailStack(stack *stackResponse) []ports.PRAncestorLink {
	if stack == nil || stack.Position <= 1 {
		return nil
	}
	return []ports.PRAncestorLink{{Ref: stack.Base.Ref, SHA: stack.Base.SHA}}
}

// fetchCIConclusionLive determines an OPEN PR's own CI conclusion AT ITS
// CURRENT HEAD SHA, for a LIVE, PRE-MERGE gate -- the decision inbox's own
// read-model classification (KindReadyToMerge) and, via the exact same
// SourceControl.ListOpenPRsForUser call, decisioninbox.RevalidateForMerge's
// own re-check at click time (§16.2's own "the rendered queue is never
// trusted as authority"). See mergedbetween.go's fetchCIConclusion for the
// RETROSPECTIVE §15.2 audit sibling this function is deliberately NOT --
// that function's own doc comment now cross-references this one; the two
// must never be merged back into one, and never share a caller.
//
// STRICT, unlike fetchCIConclusion's lenient "any confirmed success, no
// confirmed failure" rule: an incomplete check run (Conclusion == nil,
// i.e. still queued/in_progress on GitHub's Checks API) or a "cancelled"
// conclusion from EITHER CI surface means CI is NOT green here, full
// stop -- reported as ports.CIConclusionUnknown (never Success), which
// this port's own callers already treat identically to Failure for
// eligibility purposes (ComputeAutoApprovalEligible checks
// CIConclusion == CIConclusionSuccess, nothing weaker). Reusing
// fetchCIConclusion's own lenient rule here would let a single fast,
// low-value check (e.g. lint) finishing green stand in for "the whole
// required suite is green", while five other required checks are still
// queued -- exactly the "not yet red" masquerading as "green" gap this
// function exists to close. A genuine, confirmed failure signal still
// wins over an incomplete one (both already mean "not green" for
// ComputeAutoApprovalEligible's own purposes, but Failure is the more
// specific, more useful fact to report when both are true at once).
//
// The combined-status "pending" branch below additionally requires
// TotalCount > 0 before treating it as an incomplete signal -- see combinedStatusResponse's own doc
// comment (mergedbetween.go) and the inline comment at that branch for
// the full "why": GitHub reports state=="pending" both for a genuinely
// in-flight legacy status AND for a repo with no legacy statuses at all,
// and only TotalCount can tell the two apart.
func (a *Adapter) fetchCIConclusionLive(ctx context.Context, owner, repo, headSHA, token string) ports.CIConclusion {
	sawFailure := false
	sawSuccess := false
	sawIncomplete := false

	statusPath := fmt.Sprintf("%s/repos/%s/%s/commits/%s/status", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(headSHA))
	if body, err := a.doGet(ctx, statusPath, token); err == nil {
		var status combinedStatusResponse
		if json.Unmarshal(body, &status) == nil {
			switch status.State {
			case "failure", "error":
				sawFailure = true
			case "success":
				sawSuccess = true
			case "pending":
				// The comment previously here claimed "pending" was "the
				// combined-status surface's exact analogue of the Checks
				// API's Conclusion == nil below" -- factually wrong:
				// Conclusion == nil only ever exists once a check run
				// genuinely exists and simply has not concluded yet, but
				// GitHub's own documented rule for THIS endpoint is
				// "pending if there are no statuses or a context is
				// pending" -- so "pending" ALSO fires when nothing was
				// ever posted here at all. A repo whose CI runs
				// exclusively through GitHub Actions check-runs (the
				// SEPARATE surface read immediately below -- the dominant
				// modern CI configuration, including this repo's own) has
				// ZERO legacy commit statuses, so this endpoint reports
				// state=="pending", total_count==0 for every commit,
				// forever. Reading that alone as sawIncomplete meant
				// fetchCIConclusionLive could NEVER report Success on such
				// a repo no matter how green its real CI was: ciGreen
				// would always be false downstream (aggregate.go),
				// ComputeAutoApprovalEligible would always refuse, and
				// RevalidateForMerge would 409 every single merge --
				// making the headline ready_to_merge feature entirely
				// non-functional on the dominant modern CI setup. Only
				// treat "pending" as a genuinely in-flight legacy status
				// when TotalCount confirms at least one status actually
				// exists here; a statusless "pending" carries no signal at
				// all and is left for the check-runs loop below to decide.
				if status.TotalCount > 0 {
					sawIncomplete = true
				}
			}
		}
	}

	checksPath := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(headSHA))
	if body, err := a.doGet(ctx, checksPath, token); err == nil {
		var runs checkRunsResponse
		if json.Unmarshal(body, &runs) == nil {
			for _, r := range runs.CheckRuns {
				if r.Conclusion == nil {
					// Still queued/in_progress -- UNLIKE fetchCIConclusion's
					// own identical loop, this is never simply skipped: a
					// live pre-merge gate must never let a check that has
					// not finished yet read as though it were never
					// required at all.
					sawIncomplete = true
					continue
				}
				switch *r.Conclusion {
				case "cancelled":
					// Also never green for a live gate, unlike
					// fetchCIConclusion's own lenient rule, which leaves a
					// cancelled run contributing to neither sawFailure nor
					// sawSuccess at all (matching that rule's own
					// "cancelled is likewise ignored" text).
					sawIncomplete = true
				case "success", "neutral":
					sawSuccess = true
				default:
					if ciFailureConclusions[*r.Conclusion] {
						sawFailure = true
					}
				}
			}
		}
	}

	switch {
	case sawFailure:
		return ports.CIConclusionFailure
	case sawIncomplete:
		return ports.CIConclusionUnknown
	case sawSuccess:
		return ports.CIConclusionSuccess
	default:
		return ports.CIConclusionUnknown
	}
}

func (a *Adapter) fetchOpenPRDetail(ctx context.Context, owner, repo string, number int, token string) (openPRDetailResponse, error) {
	path := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), number)
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return openPRDetailResponse{}, fmt.Errorf("githubapi: fetch open pr detail: %w", err)
	}
	var parsed openPRDetailResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return openPRDetailResponse{}, fmt.Errorf("githubapi: decode open pr detail: %w", err)
	}
	return parsed, nil
}

// fetchReviewDecision scans EVERY review GitHub reports for number (first
// page, per_page=100, mirroring fetchHasApprovingReview's own identical
// bound) and reduces it to each REVIEWER's own LATEST decision before
// reporting whether at least one carries state APPROVED and whether at
// least one carries state CHANGES_REQUESTED (a third return, degraded,
// added below -- replacing this function's own previous "any
// CHANGES_REQUESTED review exists, ever" rule). GitHub's own review list
// is APPEND-ONLY: a reviewer who requested changes and later re-reviewed
// and approved leaves the OLD CHANGES_REQUESTED row in place forever,
// alongside the new APPROVED one, both returned by this SAME endpoint.
// Since A4 (first review round) promoted HasChangesRequested to a HARD
// merge blocker (decisioninbox.RevalidateForMerge), the naive "any
// CHANGES_REQUESTED row, ever" rule made a legitimately re-approved PR
// PERMANENTLY unmergeable through Narvi. GitHub's own real review-decision
// semantics (what the "Changes requested"/"Approved" banner on a real PR
// page reflects) is exactly each reviewer's OWN most recent decision,
// independent of every other reviewer and independent of that SAME
// reviewer's own earlier reviews.
//
// Reviews are grouped by reviewer (User.ID; a review with no User at all
// -- should not happen for a real submitted review, but defensively
// skipped rather than crashing) and the LATEST one GitHub returns for each
// is kept, in the SAME order GitHub itself returns this list
// (chronological, oldest first -- GitHub's own documented, stable order
// for this endpoint), so simply overwriting a running per-reviewer map
// while scanning forward always leaves that reviewer's most recent
// decision standing. A COMMENTED review (GitHub's own "left comments
// without an approve/reject decision" state) never overwrites a
// reviewer's own standing decision -- it does not itself carry a
// decision, so a reviewer who requested changes and LATER merely
// commented has not thereby withdrawn that decision; only a LATER
// APPROVED or CHANGES_REQUESTED review from that SAME reviewer does.
//
// degraded is true iff this fetch
// itself failed (transient HTTP error OR a response that did not decode) --
// BEFORE this fix, either failure silently returned (false, false),
// indistinguishable from a genuine, confirmed "nobody has requested
// changes" read. hasChangesRequested is this codebase's own HARD
// unattended-merge blocker (decisioninbox.RevalidateForMerge, reused
// unchanged by the auto-merge worker's own RevalidateForAutoMerge) -- a
// degraded read that silently satisfies that block is a failure isolated
// to ONE GitHub endpoint quietly widening what an unattended worker will
// merge. buildOpenPRFromDetail (below) threads this onto ports.OpenPR.
// ReviewDecisionDegraded; every caller of THAT field must fail closed
// (treat a degraded read as equivalent to "changes requested", never as
// "no changes requested") -- see revalidateCore's own doc comment
// (internal/app/decisioninbox/revalidate.go) for where that fail-closed
// direction is actually enforced on the unattended path, and
// buildPROpenItem's own doc comment (aggregate.go) for the read-model
// side. hasApproving/hasChangesRequested are both left false alongside a
// true degraded (never a meaningful reading of either), exactly mirroring
// RevertReviewStateUnknown/CIConclusionUnknown's own established
// "degraded means the OTHER fields carry no signal at all" convention
// elsewhere in this package.
func (a *Adapter) fetchReviewDecision(ctx context.Context, owner, repo string, number int, token string) (hasApproving, hasChangesRequested, degraded bool) {
	path := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews?per_page=100", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), number)
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return false, false, true
	}
	var reviews []reviewItemResponse
	if err := json.Unmarshal(body, &reviews); err != nil {
		return false, false, true
	}

	latestDecisionByReviewer := make(map[int64]string, len(reviews))
	for _, r := range reviews {
		if r.User == nil {
			continue
		}
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED":
			latestDecisionByReviewer[r.User.ID] = r.State
		}
	}

	for _, state := range latestDecisionByReviewer {
		switch state {
		case "APPROVED":
			hasApproving = true
		case "CHANGES_REQUESTED":
			hasChangesRequested = true
		}
	}
	return hasApproving, hasChangesRequested, false
}

// fetchChangedFilePaths fetches number's own changed files (first page,
// per_page=100, mirroring fetchChangedPathPrefixes' own identical bound
// and acceptance) -- FULL filenames (unlike fetchChangedPathPrefixes'
// own reduced top-level-prefix form), since this file's own caller needs
// them for ResolveCodeOwners' own Paths input, which matches CODEOWNERS
// patterns against complete file paths, not prefixes.
func (a *Adapter) fetchChangedFilePaths(ctx context.Context, owner, repo string, number int, token string) ([]string, error) {
	path := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/files?per_page=100", a.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), number)
	body, err := a.doGet(ctx, path, token)
	if err != nil {
		return nil, fmt.Errorf("githubapi: fetch open pr changed files: %w", err)
	}
	var files []pullFileResponse
	if err := json.Unmarshal(body, &files); err != nil {
		return nil, fmt.Errorf("githubapi: decode open pr changed files: %w", err)
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Filename
	}
	return paths, nil
}
