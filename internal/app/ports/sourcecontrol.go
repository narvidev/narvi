package ports

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// GitHubSourceControlHost is the ONLY source-control host this codebase's
// one real SourceControl implementation (internal/adapters/outbound/
// githubapi) ever actually queries -- production wiring always talks to
// GitHub's own real API (api.github.com) regardless of what host a
// session's own repo URL names. Shared (audit-remediation batch B3) by
// every caller that must reject a repo URL naming a host this
// deployment's configured adapter cannot serve: app/imagebuild.Builder.
// resolveRepoSHAs (before ever calling ResolveBranchSHA -- otherwise a
// GitLab/other repo URL's owner/repo path silently resolves against
// GitHub's real API for a coincidentally-matching path) and app/
// sessionactor's own warm-boot repo-access gate (imageresolve.go's
// repoAccessAllowedForSpawn, before ever deriving an owner/repo or
// spending a CheckRepoAccess call on it). Both used to hand-roll this
// same check independently (one had it, the other didn't) -- unified here
// since both are, structurally, asking the exact same question: "does
// this URL name the host our one wired SourceControl adapter actually
// talks to?" via reposource.CheckRepoHost.
//
// Update this (or make it configurable per-adapter) the day a second
// SourceControl implementation (e.g. GitLab) is actually wired -- see the
// SourceControl interface's own doc comment on why this port
// intentionally stays adapter-agnostic in its signatures; this one const
// is a deliberate, narrow, explicitly-called-out exception, not a
// precedent for leaking GitHub specifics into this port more broadly.
const GitHubSourceControlHost = "github.com"

// SupportedSourceControlHosts returns the full allowlist every
// reposource.CheckRepoHost call site in this codebase must use -- audit-
// remediation batch B3 round 2 (finding #7: two independently-maintained
// call sites -- app/imagebuild.Builder.resolveRepoSHAs and app/
// sessionactor's own warm-boot repo-access gate, imageresolve.go's
// repoAccessAllowedForSpawn -- happened to reference the SAME
// GitHubSourceControlHost constant today, but nothing structurally
// prevented one of them from being edited to accept an extra host (e.g. a
// future GitLab rollout, or a careless copy-paste) without the other
// following along, silently reopening exactly the drift this whole
// constant exists to close. Both call sites now call THIS function --
// never GitHubSourceControlHost directly -- so there is exactly one place
// in the codebase where "which hosts can this deployment's wired
// SourceControl actually serve" is decided; a future second adapter (e.g.
// GitLab) is wired by changing this one function's own return value, and
// every caller picks it up identically, by construction, not by
// convention.
func SupportedSourceControlHosts() []string {
	return []string{GitHubSourceControlHost}
}

// CreatePRSpec is what SourceControl.CreatePR needs to open a pull
// request. Owner/Repo/Head/Base are generic source-control concepts, not
// GitHub API field names -- nothing GitHub-specific leaks into this
// signature even though internal/adapters/outbound/githubapi is this
// port's only real implementation today (§4.3: "SourceControl (GitHub +
// GitLab: createPR, credential minting, push specs)").
type CreatePRSpec struct {
	// Owner is the repo owner/organization.
	Owner string
	// Repo is the repo name (without owner prefix).
	Repo string
	// Head is the branch the PR is created FROM (the session's own
	// already-pushed branch).
	Head string
	// Base is the branch the PR targets.
	Base string
	// Title is the PR title.
	Title string
	// Body is the PR description.
	Body string

	// Token is the SAME plaintext, decrypted OAuth access token the
	// scm-credentials endpoint already obtained for this session's own
	// user (§8.11: "PR created with the prompting user's OAuth token") --
	// the caller that already succeeded at the push already has this
	// value in hand, so CreatePR never re-fetches/re-decrypts it itself.
	// Never logged by any caller or implementation of this port.
	Token string
}

// PRRef is what CreatePR returns on success: the created PR's own number
// and URL.
type PRRef struct {
	Number int
	URL    string
}

// ResolveBranchSHASpec is what SourceControl.ResolveBranchSHA (§8.5,
// "image builds") needs to resolve a repo's current commit SHA for
// fingerprinting (§10 Phase 2: "fingerprint = repo SHAs + runtime
// version"). Owner/Repo are the same generic source-control concepts
// CreatePRSpec already uses, not GitHub-specific field names.
type ResolveBranchSHASpec struct {
	// Owner is the repo owner/organization.
	Owner string
	// Repo is the repo name (without owner prefix).
	Repo string
	// Branch is the branch to resolve. Empty means "the repo's own
	// default branch" -- mirroring how a null/empty
	// SessionConfigReposElem.Branch is already handled elsewhere for this
	// session's own repos (domain/gitstate's "checkout session branch,
	// create from base if absent" semantics): a not-yet-existing session
	// branch does not exist to resolve a SHA for in the first place, so
	// this always resolves the repo's OWN default branch's current SHA
	// instead -- the branch a fresh session branch would be created FROM.
	Branch string
	// Token is the same plaintext, decrypted OAuth access token shape
	// CreatePRSpec.Token already uses. Never logged by any caller or
	// implementation of this port.
	Token string
}

// ResolveContractsFingerprintSpec is what SourceControl.
// ResolveContractsFingerprint ("mocking + contract drift", §14.3)
// needs to fingerprint a repo's configured contracts directory at one ref.
// Owner/Repo/Token are the same generic source-control concepts
// CreatePRSpec/ResolveBranchSHASpec already use, not GitHub-specific field
// names.
type ResolveContractsFingerprintSpec struct {
	// Owner is the repo owner/organization.
	Owner string
	// Repo is the repo name (without owner prefix).
	Repo string
	// Ref is the commit SHA (or branch/tag) to resolve the contracts
	// directory listing AT -- app/sessionactor's own checkContractDrift
	// (contractdrift.go) always passes the repo's own just-resolved
	// current commit SHA here, never a branch name, so drift detection
	// compares fingerprints at precise, stable commits rather than a
	// moving branch ref.
	Ref string
	// Path is the repo-relative path to the contracts directory (e.g.
	// "contracts/api") -- the Postgres Environment row's own
	// contracts_path column (sqlcgen.Environment.ContractsPath), resolved
	// by the caller (app/sessionactor/contractdrift.go's
	// checkContractDrift).
	Path string
	// Token is the same plaintext, decrypted OAuth access token shape
	// CreatePRSpec.Token/ResolveBranchSHASpec.Token already use. Never
	// logged by any caller or implementation of this port.
	Token string
}

// CheckRepoAccessSpec is what SourceControl.CheckRepoAccess (audit fix,
// "warm-boot image access control") needs to answer a single question:
// can spec.Token read spec.Owner/spec.Repo AT ALL, independent of any
// branch or commit. Owner/Repo/Token are the same generic source-control
// concepts CreatePRSpec/ResolveBranchSHASpec already use, not GitHub-
// specific field names.
type CheckRepoAccessSpec struct {
	// Owner is the repo owner/organization.
	Owner string
	// Repo is the repo name (without owner prefix).
	Repo string
	// Token is the same plaintext, decrypted OAuth access token shape
	// CreatePRSpec.Token/ResolveBranchSHASpec.Token already use. Never
	// logged by any caller or implementation of this port.
	Token string
}

// GetFileContentSpec is what SourceControl.GetFileContent (
// "sentinels + suggestions", §12.2 item 2) needs to read one file's own
// current content at a ref -- the apply-suggestion endpoint's own
// precondition read, before it ever attempts to validate or commit a
// finding's SuggestedFix.
type GetFileContentSpec struct {
	Owner string
	Repo  string
	// Path is the repo-relative file path (review_findings.file_path).
	Path string
	// Ref is the commit SHA/branch to read at -- the PR's own CURRENT
	// head branch, so the apply-suggestion endpoint always validates
	// against the PR's real, live state, never a stale snapshot.
	Ref string
	// Token is the ACTING maintainer's own plaintext, decrypted OAuth
	// access token (§17.4/apply-suggestion's own "using the acting
	// maintainer's own OAuth token for attribution -- not the original
	// session creator's" requirement) -- never logged.
	Token string
}

// UpdateFileContentSpec is what SourceControl.UpdateFileContent (
// §12.2 item 2) needs to commit a new version of one file directly onto a
// branch, via the source-control host's Contents API -- the apply-
// suggestion endpoint's own write, once ValidateSuggestionApplies (pure,
// internal/domain/reviewpost) has already confirmed the patch still
// applies against the file's current content.
type UpdateFileContentSpec struct {
	Owner string
	Repo  string
	Path  string
	// Content is the file's own NEW, full content (this port's
	// implementation is never asked to apply a patch itself -- the caller
	// has already computed the resulting content).
	Content string
	// SHA is the blob SHA of the CURRENT content being replaced (GitHub's
	// Contents API requires this on every update, as its own optimistic-
	// concurrency check -- a mismatched SHA means the file changed since
	// it was last read, and the API itself rejects the write).
	SHA string
	// Branch is the branch to commit onto (the PR's own current head
	// branch).
	Branch string
	// Message is the commit message.
	Message string
	// Token is the acting maintainer's own OAuth token, exactly like
	// GetFileContentSpec.Token above -- the resulting commit is
	// attributed to THIS user, never the original session creator.
	Token string
}

// UpdatePRBodySpec is what SourceControl.UpdatePRBody (§26.2's
// own "graduated remediation") needs to overwrite one pull request's own
// body field directly, via the source-control host's own pull-request
// update endpoint. Body is the caller's own ALREADY-COMPOSED new body
// text (internal/domain/reviewpost.RenderAutofixBody's result) -- this
// port's implementation never composes content itself, mirroring
// UpdateFileContentSpec.Content's own identical "the caller has already
// computed the resulting content" discipline immediately above. Never
// touches the PR's title (§26.2: "the title is never rewritten
// automatically") -- this spec carries no title field at all, so there is
// structurally nothing for a caller to even pass one through.
type UpdatePRBodySpec struct {
	Owner  string
	Repo   string
	Number int
	Body   string
	// Token authenticates this write -- §26.2's own one real caller
	// (internal/app/outboxworker's own description-autofix notifier) is a
	// SYSTEM-INITIATED action with no per-PR human creator to attribute it
	// to (the target PR may have been opened by a different session
	// entirely than the one whose verdict triggered this rewrite), so this
	// is always the deployment's own static bot credential
	// (platform.Config.GitHubBotToken) -- never a per-user OAuth token,
	// mirroring pushpr.go's own createSentinelFixPRBestEffort precedent
	// for the identical "no human creator to attribute to" situation.
	Token string
}

// RegisterPRStackSpec is what SourceControl.RegisterPRStack (
// §17.2/§17.6) needs to register an already-open origin+fix PR pair as a
// real GitHub stack, once both exist.
type RegisterPRStackSpec struct {
	Owner string
	Repo  string
	// PRNumbers is bottom-to-top (§17.2: "pull_requests: [originPR,
	// fixPR]") -- always exactly two elements for this Step's one real
	// producer (§17.6: "the one pair, not an N-deep producer"), though
	// this port itself places no length restriction of its own.
	PRNumbers []int
	Token     string
}

// CreateBranchSpec is what SourceControl.CreateBranch (§17.2) needs to create a brand-new branch ref pointing at
// an already-known commit SHA -- the sentinel-auto-fix flow's own fix for
// giving a fix child session an upstream branch DISTINCT from the origin
// PR's own head branch to check out and push to (see
// internal/app/outboxworker/sentinelautofix.go's own Deliver doc comment
// for the full "why": without this, the fix session's repos[].branch was
// the origin's own literal head-branch name, so its clone/checkout/push
// all targeted that SAME branch -- silently fast-forwarding the still-
// open origin PR with an unreviewed commit, and dooming the eventual fix-
// PR CreatePR call to Head == Base, which GitHub rejects outright).
type CreateBranchSpec struct {
	Owner string
	Repo  string
	// Branch is the NEW branch name to create (refs/heads/<Branch>).
	Branch string
	// SHA is the commit this new branch is created FROM -- the caller's
	// own already-resolved value (e.g. ResolveBranchSHA's own first return
	// value), never re-resolved by this method itself.
	SHA string
	// Token is the same plaintext, decrypted OAuth/bot access token shape
	// every other spec in this file already uses. Never logged.
	Token string
}

// ListMergedBetweenSpec is what SourceControl.ListMergedBetween (
// "release PR review", §15.2) needs: BaseRef/HeadRef mirror a release
// PR's own base/head branches (or SHAs) exactly the way a real `git
// compare BaseRef...HeadRef` names a range -- ListMergedBetween reports
// every PR merged into HeadRef since it diverged from BaseRef. Owner/Repo/
// Token are the same generic source-control concepts every other spec in
// this file already uses.
type ListMergedBetweenSpec struct {
	Owner   string
	Repo    string
	BaseRef string
	HeadRef string
	Token   string
}

// CIConclusion is a constituent PR's own CI result AT THE COMMIT THAT
// ACTUALLY MERGED (§15.2: "CI conclusion at the merge SHA specifically --
// not the latest SHA, a force-push after approval can hide a run that
// was red when it actually merged"). A plain, port-facing mirror of
// internal/domain/review.CIConclusion's own three values -- this package
// cannot import that domain type (ports/doc.go: this package imports
// only contracts/gen/go/* and the standard library), so it is
// necessarily its own, separately-declared type, converted by whichever
// caller builds a domain/review.MergedPR from this port's own MergedPR
// below.
type CIConclusion string

const (
	// CIConclusionSuccess is CI passing (or reporting no failure) at the
	// merge SHA.
	CIConclusionSuccess CIConclusion = "success"
	// CIConclusionFailure is CI genuinely failing at the merge SHA.
	CIConclusionFailure CIConclusion = "failure"
	// CIConclusionUnknown is "no CI signal could be found or determined
	// at this commit" -- never itself evidence of failure; see
	// review.CIConclusionUnknown's own doc comment for the full
	// reasoning a caller building a manifest finding from this value
	// relies on.
	CIConclusionUnknown CIConclusion = "unknown"
)

// RevertReviewState is whether a constituent PR's own revert (WasReverted
// == true) itself carried an approving review, at the time the port
// checked -- a plain, port-facing mirror of internal/domain/review.
// RevertReviewState's own three values, mirroring CIConclusion's own
// identical "positively confirmed vs. genuinely unknown" shape directly
// above for the SAME reason: audit-fix (should-fix #4, "release PR
// review", §15.2) -- a failed sub-fetch of the revert PR's own review
// state must never silently manufacture RevertReviewStateNotReviewed
// (the ONE value that ever triggers review.ManifestFindingUnreviewedRevert),
// the exact same "never assert without positive confirmation" discipline
// CIConclusionUnknown already establishes for the red-at-merge finding.
type RevertReviewState string

const (
	// RevertReviewStateReviewed is a CONFIRMED approving review on the
	// revert PR itself.
	RevertReviewStateReviewed RevertReviewState = "reviewed"
	// RevertReviewStateNotReviewed is a CONFIRMED absence of any
	// approving review on the revert PR itself -- the only value that
	// ever produces review.ManifestFindingUnreviewedRevert.
	RevertReviewStateNotReviewed RevertReviewState = "not_reviewed"
	// RevertReviewStateUnknown is "the revert PR's own review state could
	// not be determined" (the sub-fetch itself failed) -- NOT evidence of
	// an unreviewed revert. The zero value of this type is treated
	// identically to this value, mirroring CIConclusion's own identical
	// "unset field is exactly as uninformative as an explicit unknown"
	// convention.
	RevertReviewStateUnknown RevertReviewState = "unknown"
)

// MergedPR is one PR ListMergedBetween reports as merged into a release
// PR's own head since it diverged from its base (§15.2). Each field
// mirrors §15.2's own explicit list verbatim: "PR number/title, approving
// reviews, CI conclusion at the merge SHA..., whether it merged via an
// admin/policy override, and whether it was later reverted (and whether
// that revert was itself reviewed)" -- plus two further fields §15.3's
// own aggregate-review decision function needs from the SAME per-PR
// fetch (ChangedPathPrefixes, HadManualConflictResolution), so a caller
// never has to make a second round of port calls to answer a question
// this port's one real adapter already had every fact in hand for while
// building MergedPR the first time.
type MergedPR struct {
	Number int
	Title  string

	// HasApprovingReview is whether this PR carried at least one review
	// with state APPROVED at merge time.
	HasApprovingReview bool
	// MergedViaAdminOverride is whether this PR merged via an admin/
	// policy bypass of a review requirement it did not satisfy -- see
	// the githubapi adapter's own doc comment for exactly how this is
	// determined (and when it conservatively stays false rather than
	// guessing).
	MergedViaAdminOverride bool

	// CIConclusionAtMergeSHA is this PR's CI result at the exact commit
	// that landed -- see CIConclusion's own doc comment.
	CIConclusionAtMergeSHA CIConclusion

	// MergedAt is when this PR's merge commit landed -- the reference
	// point RevertedAt (below) is measured against.
	MergedAt time.Time

	// WasReverted is whether this PR was later reverted; RevertedAt is
	// when (nil when WasReverted is false); RevertReviewState is whether
	// THAT revert itself carried an approving review (meaningless when
	// WasReverted is false) -- see RevertReviewState's own doc comment
	// for why this is a tri-state, not a plain bool (audit-fix should-fix
	// #4).
	WasReverted       bool
	RevertedAt        *time.Time
	RevertReviewState RevertReviewState

	// HadManualConflictResolution is whether landing this PR required
	// manually resolving a merge conflict against its base -- one of
	// §15.3's own three OR-conditions for the aggregate review; see the
	// githubapi adapter's own doc comment for how this is inferred.
	HadManualConflictResolution bool
	// ChangedPathPrefixes is the set of top-level path prefixes this
	// PR's diff touches (e.g. "internal/domain/review") -- §15.3's own
	// "≥3 constituent PRs touch overlapping path prefixes" input.
	ChangedPathPrefixes []string
	// Labels is this PR's own CURRENT GitHub labels -- a caller checks
	// this against internal/domain/reviewpost.LabelHighRisk (or any
	// other repo-specific high-risk convention) to derive §15.3's own
	// "flagged high-risk/critical by the team's own PR-tiering" signal
	// (domain/review.MergedPR.HighRiskFlagged) before converting into
	// that domain type -- this port stays agnostic to that vocabulary,
	// exactly like it stays agnostic to every other posting-layer
	// concern (CreatePRSpec's own doc comment).
	Labels []string
}

// PRPerson identifies one source-control account attached to a pull
// request in some role (assignee, requested reviewer, author) -- §16,
// "decision inbox: read model + API", §16.2. ExternalID is the account's
// STABLE, provider-native identifier (GitHub: the numeric account id, as
// a decimal string) -- deliberately NOT Login: this codebase's own
// identity graph (§13.2, migrations/000003_identities.up.sql) keys
// identities.external_id on exactly this same stable id (populated at
// GitHub OAuth sign-in from the OAuth /user response's own "id" field,
// internal/adapters/inbound/auth/callback.go), so a caller resolving this
// PRPerson to a Narvi user_id (the app-layer decision-inbox aggregator)
// does so via that SAME already-established (provider, external_id)
// lookup -- never a second, parallel login-keyed identity mechanism. Login
// is carried alongside purely for DISPLAY (rendering a person's name on
// an inbox row) -- never itself an identity key, since GitHub logins can
// change while the numeric id never does (the exact reason GitHub's own
// REST API offers "GET /user/{account_id}" as a durable id->login
// resolution path, https://docs.github.com/rest/users/users#get-a-user-using-their-id,
// fetched 2026-08-07 -- see githubapi.ListOpenPRsForUser's own doc
// comment for why this port needs that resolution at all).
type PRPerson struct {
	ExternalID string
	Login      string
}

// OpenPR is one open pull request ListOpenPRsForUser reports (
// §16.2: "ListOpenPRsForUser(ctx, user) ([]OpenPR, error) (review state,
// CI at head SHA, labels, assignees/reviewers)"). Every field is this
// PR's CURRENT, live state -- there is no historical/as-of-an-earlier-SHA
// concept here the way MergedPR's own CIConclusionAtMergeSHA needs one;
// §16.2's own short-TTL cache (never modeled inside this port -- see that
// section's own "SCM data is cached with a short TTL" text) is what turns
// a snapshot of these fields into the "as of 2 min ago" staleness a
// caller displays, entirely at the app layer.
type OpenPR struct {
	// Owner/Repo are the same generic source-control concepts every other
	// spec/return type in this file already uses -- carried here (unlike
	// MergedPR, which never needs to since ListMergedBetween's own caller
	// already knows the one repo it asked about) because
	// ListOpenPRsForUser fans out across POTENTIALLY MANY different
	// repos in one call, so each returned OpenPR must say which repo it
	// belongs to.
	Owner string
	Repo  string

	Number  int
	Title   string
	HTMLURL string

	HeadSHA string
	BaseRef string
	Draft   bool

	Author             PRPerson
	Assignees          []PRPerson
	RequestedReviewers []PRPerson
	// RequestedTeams is every team slug ("org/team-slug") GitHub reports
	// as a requested reviewer on this PR -- kept alongside
	// RequestedReviewers (individuals) rather than merged into it, since a
	// team is not itself a PRPerson (it has no single stable account id);
	// resolving a team to the PERSONS who satisfy it is ResolveCodeOwners'
	// own job when the team also happens to be a CODEOWNERS entry, and the
	// identity graph's own job more generally -- this port only reports
	// what GitHub itself reports.
	RequestedTeams []string

	// HasApprovingReview/HasChangesRequested are this PR's own CURRENT
	// review-decision facts (§16.1's own "verdict is >= medium risk or a
	// formal review is gated" needs SOME positive signal for "is a review
	// still pending/blocking" beyond just the risk label) -- computed
	// from every review GitHub reports, exactly like MergedPR.
	// HasApprovingReview already does for a MERGED PR (mergedbetween.go's
	// fetchHasApprovingReview), just evaluated for an OPEN PR's current
	// state instead of one fixed historical SHA.
	HasApprovingReview  bool
	HasChangesRequested bool
	// ReviewDecisionDegraded is
	// true iff the fetch that produced HasApprovingReview/
	// HasChangesRequested above itself failed (a transient HTTP error, or
	// a response that did not decode) -- githubapi.fetchReviewDecision's
	// own doc comment. BEFORE this fix, that failure silently returned
	// both fields false, indistinguishable from a genuine, confirmed "no
	// reviewer has requested changes" read -- exactly satisfying
	// decisioninbox.RevalidateForMerge's own HARD unattended-merge block
	// on HasChangesRequested with a degraded, not a confirmed, negative.
	//
	// FAIL CLOSED: every caller that gates on HasChangesRequested MUST
	// also check this field and treat true identically to
	// HasChangesRequested itself being true -- "we could not tell" must
	// never be read as "no". HasApprovingReview/HasChangesRequested both
	// stay their zero value (false) when this is true; neither carries any
	// real signal in that case.
	ReviewDecisionDegraded bool

	// CIConclusion is this PR's CI result AT HeadSHA specifically (§16.2:
	// "CI at head SHA") -- reuses CIConclusion's own three-value,
	// positively-confirmed-vs-genuinely-unknown discipline verbatim (see
	// that type's own doc comment).
	CIConclusion CIConclusion

	// Labels is this PR's own current GitHub labels -- a caller checks
	// this against reviewpost.LabelLowRisk/.../LabelNeedsHuman to derive
	// the decision-inbox taxonomy's own risk/escape-hatch signals, exactly
	// like MergedPR.Labels' own identical "this port stays agnostic to
	// that vocabulary" doc comment already establishes for the release-
	// manifest read model.
	Labels []string

	// ChangedFiles is this PR's own current changed-file paths (first
	// page, per_page=100 -- mirrors mergedbetween.go's own
	// fetchChangedPathPrefixes precedent and its identical "an honestly-
	// scoped approximation" acceptance) -- the input a caller feeds to
	// ResolveCodeOwners' own Paths, without a second round-trip to
	// re-fetch what this call already had in hand.
	//
	// Phase 5 audit findings 1+2 (both fixed): this field is deliberately
	// NOT what the auto-approval eligibility engine's diff-size gate
	// compares against threshold anymore -- len(ChangedFiles) silently
	// undercounts any PR over the one-page cap above, and used to also
	// silently read as "0 files" whenever the separate Pull Request Files
	// fetch that populates it failed outright (nil, indistinguishable
	// from a confirmed-empty diff). ChangedFilesCount below is the
	// authoritative count the size gate now uses; ChangedFilesListDegraded
	// below is whether THIS field can still be trusted for sensitive-path
	// classification. ChangedFiles itself is unchanged by this fix (still
	// nil on a genuine fetch failure, still capped at one page on a
	// genuinely large PR) -- ResolveCodeOwners' own consumption of it
	// remains the same honestly-scoped approximation it always was; only
	// the auto-approval eligibility gate's OWN reading of this data
	// changed.
	ChangedFiles []string
	// ChangedFilesCount is this PR's own CURRENT changed-file count,
	// GitHub's own authoritative "changed_files" scalar on the SAME "Get
	// a pull request" response ChangedFiles' own first paragraph already
	// reads -- Additions/Deletions' own sibling scalar, immediately below,
	// and exactly as reliable: the central detail fetch either succeeds,
	// in which case GitHub reports this field on every real PR resource,
	// or fails outright, in which case buildOpenPR/GetOpenPR both drop
	// the whole row rather than return a half-built OpenPR (their own doc
	// comments) -- there is no partial-success state in which this
	// specific field is present but wrong the way ChangedFiles' own
	// SEPARATE, independent Pull Request Files fetch can fail or
	// truncate. Phase 5 audit finding 2 (fixed): the auto-approval
	// eligibility engine's diff-size gate (autoapproval.ComputeEligible,
	// EligibilityInput.ChangedFileCount) compares THIS field against
	// EligibilityConfig.MaxFilesChanged -- never len(ChangedFiles), which
	// a PR author fully controls the ability to game past a page boundary
	// (filenames and diff order are both attacker-influenceable).
	ChangedFilesCount int
	// ChangedFilesListDegraded is true iff ChangedFiles above is NOT a
	// complete listing of this PR's changed paths -- either the Pull
	// Request Files fetch itself failed outright (ChangedFiles is nil),
	// OR it succeeded but was truncated at its own one-page cap
	// (ChangedFilesCount above, the authoritative total, exceeds
	// len(ChangedFiles)). Phase 5 audit findings 1+2 (both fixed): a
	// caller deriving sensitive-path facts (autoapproval.
	// ClassifyChangedPaths) from ChangedFiles MUST treat a true value
	// here as "these facts are UNKNOWN, never confirmed clean" -- see
	// EligibilityInput.TouchedBlastRadiusKnown's own doc comment
	// (internal/domain/autoapproval/eligibility.go) for where this is
	// actually enforced, fail-closed. false (the zero value) alongside a
	// nil/empty ChangedFiles legitimately means "confirmed: this PR
	// touches zero files" -- the SAME "confirmed negative" vs. "could not
	// confirm" distinction ReviewDecisionDegraded/CIConclusionUnknown
	// already draw elsewhere on this same struct, applied here to the
	// identical ambiguity already closed for
	// Verdict.FilesChanged/BlastRadius, now closed for THIS field's own
	// failure/truncation modes too.
	ChangedFilesListDegraded bool
	Additions                int
	Deletions                int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Owner is one CODEOWNERS entry resolved for ONE input path (
// §16.2: "ResolveCodeOwners(ctx, repo, paths) ([]Owner, error)...
// CODEOWNERS teams resolve to persons through the identity graph
// (§13.2)"). Exactly one of two shapes:
//
//   - A resolved GitHub account (ExternalID/Login both set) -- either an
//     individual CODEOWNERS entry ("@login") or one member of a team
//     entry ("@org/team-slug"), already expanded to that member's own
//     account by the adapter (TeamSlug non-empty iff this Owner reached
//     the caller via team expansion rather than a direct "@login" entry
//     -- the port itself does the identity-graph-adjacent resolution
//     GitHub's own API can do (id/login), but stops there: converting
//     ExternalID into a Narvi user_id is the caller's own identities.
//     GetByProviderAndExternalID lookup, exactly like PRPerson above,
//     keeping this adapter-agnostic port ignorant of Narvi's own users
//     table).
//   - An unresolved email entry (Email set, ExternalID/Login both empty)
//     -- CODEOWNERS also accepts a bare email address as an owner, which
//     GitHub's own API offers no account-lookup-by-email endpoint for (a
//     deliberate GitHub privacy boundary); the caller instead matches
//     Email against the identity graph's OWN identities.email /
//     users.primary_email columns, which this codebase already populates
//     independently of GitHub.
//
// Path/Pattern are always both set (whichever of the two shapes above):
// Path is which INPUT path (of the spec's own Paths slice) this Owner was
// resolved for, and Pattern is the winning CODEOWNERS pattern
// (codeowners.Rule.Pattern) that matched it -- carried so a caller never
// has to re-derive "why is this person listed" (§16.1's own "with the
// matched pattern" requirement) by re-running the match itself. The same
// person can legitimately appear more than once in one ResolveCodeOwners
// response -- once per distinct (Path, Pattern) they were matched
// through -- this port does not deduplicate across paths on the caller's
// behalf, since which specific path/pattern earned a given row its
// provenance is exactly the fact §16.1 requires surfacing.
type Owner struct {
	ExternalID string
	Login      string
	TeamSlug   string // non-empty iff resolved via team-member expansion
	Email      string // set instead of ExternalID/Login for an email-form entry

	Path    string
	Pattern string
}

// ListOpenPRsForUserSpec is what SourceControl.ListOpenPRsForUser (§16.2)
// needs: GitHubExternalID is the SAME stable, provider-native
// account id PRPerson.ExternalID and identities.external_id already use
// (this method's own implementation resolves it to a live login itself --
// see githubapi.ListOpenPRsForUser's own doc comment for why the caller
// never has to). Token is the SAME plaintext, decrypted OAuth access
// token shape every other spec in this file already uses -- deliberately
// the SUBJECT user's own token (never a bot/service credential): running
// GitHub's search as that person means the result is naturally scoped to
// exactly the repos they themselves can see, with no separate managed-
// repo allowlist for this port to maintain.
type ListOpenPRsForUserSpec struct {
	GitHubExternalID string
	Token            string
}

// ResolveCodeOwnersSpec is what SourceControl.ResolveCodeOwners (
// §16.2) needs: Owner/Repo/Token are the same generic source-control
// concepts every other spec in this file already uses. Ref is the commit
// SHA (or branch) the CODEOWNERS file itself is read at -- callers MUST
// pass the repo's own BASE ref/branch here, never the PR's head (correcting
// this doc comment, which
// previously said the opposite -- "the PR's own current head branch/SHA"
// -- the exact attacker-controlled value finding B3, first round, moved
// callers away from): a PR's head is chosen by whoever opened/pushed the
// PR, so resolving CODEOWNERS there would let a PR's own author dictate
// which CODEOWNERS file this call reads when classifying THEIR OWN PR's
// provenance. GitHub's own real CODEOWNERS enforcement is likewise always
// evaluated against the repo's base branch, never a PR's head -- see
// internal/app/decisioninbox's own resolvePRProvenance (aggregate.go),
// this port's one real caller, which passes pr.BaseRef here for exactly
// this reason; a future caller/adapter must do the same, not reintroduce
// the vulnerability this fix removed. Paths is the set of repo-relative
// file paths to resolve owners for (a PR's own changed-files listing).
type ResolveCodeOwnersSpec struct {
	Owner string
	Repo  string
	Ref   string
	Paths []string
	Token string
}

// MergePRError is the typed error SourceControl.MergePR returns for any
// non-2xx GitHub response -- a DELIBERATE departure from this port's own
// established "errors are plain" precedent (CreatePR's own doc comment:
// "this Step does not invent a typed transient/permanent classification
// for source-control errors -- no caller of this port needs one yet").
// MergePR is this port's FIRST method backing a synchronous, HUMAN-FACING
// action (§16.2's own Merge endpoint, re-validating and then calling
// through to here at click time) -- every other method on this port is
// best-effort/fire-and-forget from the caller's own perspective
// (createPRBestEffort, the release-manifest audit, etc.), so a plain,
// opaque error was always an acceptable design there. A human who just
// clicked Merge needs to know WHY it failed (their own view of the PR was
// stale and CI is now red; someone else already merged and the head SHA
// no longer matches; branch protection genuinely blocks it) well enough
// to decide whether to just retry -- Status carries GitHub's own real
// HTTP status (405: not mergeable; 409: spec.HeadSHA no longer matches
// the PR's actual current head, GitHub's own documented optimistic-
// concurrency check; anything else: a generic failure) so the httpapi
// handler can render a genuinely different message per case, per GitHub's
// own documented merge-endpoint semantics (https://docs.github.com/rest/pulls/pulls#merge-a-pull-request,
// fetched 2026-08-07) rather than one generic "merge failed".
type MergePRError struct {
	Status  int
	Message string

	// RateLimited mirrors githubapi.APIError.RateLimited exactly (same
	// field name, same meaning, same "only ever set on a 403" scope):
	// true when Status==403 was GitHub's own real primary/secondary
	// rate-limit or abuse-detection mechanism answering, rather than a
	// genuine "this credential cannot merge this PR" denial. Populated by
	// the adapter (githubapi.MergePR) from the SAME classification
	// CheckRepoAccess already relies on for an identical reason -- see
	// Unwrap below, this field's own reason for existing on THIS type.
	RateLimited bool
}

func (e *MergePRError) Error() string {
	return fmt.Sprintf("sourcecontrol: merge pr: http %d: %s", e.Status, e.Message)
}

// Unwrap lets errors.Is(err, ErrAuthenticationFailed)/errors.Is(err,
// ErrPermissionDenied) see through a *MergePRError to GitHub's own real
// HTTP status underneath (docs/TECHNICAL_PLAN.md §17's own automerge
// dead-letter fix) -- reusing this type's own EXISTING Status/RateLimited
// fields rather than inventing a second, parallel classification. A 401
// is unambiguous (see ErrAuthenticationFailed's own doc comment: GitHub
// never uses 401 for rate limiting). A 403 unwraps to ErrPermissionDenied
// ONLY when RateLimited is false -- a rate-limited 403 must never unwrap
// to either sentinel, since that would make a live, self-resolving rate
// limit indistinguishable from a permanently revoked credential to any
// caller that only checks errors.Is (ErrPermissionDenied's own doc
// comment spells out both misclassification directions this guards
// against).
func (e *MergePRError) Unwrap() error {
	switch {
	case e.Status == http.StatusUnauthorized:
		return ErrAuthenticationFailed
	case e.Status == http.StatusForbidden && !e.RateLimited:
		return ErrPermissionDenied
	default:
		return nil
	}
}

// MergePRSpec is what SourceControl.MergePR (§16.2's own Merge
// endpoint) needs. Owner/Repo/Number/Token are the same generic source-
// control concepts every other spec in this file already uses (Number
// mirrors PRRef.Number/RegisterPRStackSpec.PRNumbers' own plain-int
// convention for a PR number). HeadSHA is REQUIRED, never optional --
// GitHub's own merge endpoint accepts an optional "sha" field precisely
// as an optimistic-concurrency guard ("SHA that pull request head must
// match to allow merge"); this port makes it mandatory because §16.2's
// own central invariant -- "the rendered queue is never trusted as
// authority" -- means the httpapi handler MUST have already re-fetched
// the PR's live head SHA moments earlier as part of its own re-
// validation, and passing that freshly-observed value through here is
// what makes a concurrent human push between re-validation and this call
// fail loudly (MergePRError{Status: 409}) instead of silently merging
// code nobody just re-checked.
type MergePRSpec struct {
	Owner  string
	Repo   string
	Number int

	HeadSHA string

	// MergeMethod is "merge"/"squash"/"rebase" (GitHub's own three
	// documented values) -- empty defers to the repository's own default
	// merge-method preference (GitHub's own doc comment on this same
	// field: "merge_method... Default: merge").
	MergeMethod string
	// CommitTitle/CommitMessage are both optional -- empty defers to
	// GitHub's own automatically generated commit title/message.
	CommitTitle   string
	CommitMessage string

	// Token is the ACTING user's own decrypted OAuth token (the person who
	// clicked Merge) -- the resulting merge commit (for the "merge"/
	// "rebase" methods) or squash commit is attributed to THIS identity by
	// GitHub itself, mirroring CreatePR's own token-based attribution.
	Token string
}

// SourceControl is the port that creates a pull request against a source-
// control host (§4.3). internal/adapters/outbound/githubapi (§9.3) is
// the first real implementation; internal/adapters/outbound/gitlabapi
// remains an untouched stub -- this port exists specifically so a future
// GitLab implementation can satisfy the SAME interface (CLAUDE.md: "don't
// couple a port to a single adapter").
type SourceControl interface {
	// CreatePR opens a pull request per spec. Errors are plain (unlike
	// SandboxProvider, this Step does not invent a typed
	// transient/permanent classification for source-control errors --
	// no caller of this port needs one yet: PR creation is not retried by
	// any circuit-breaker-style mechanism this Step builds).
	//
	// Idempotent (a confirmed-finding fix, mirroring CreateBranch's
	// own identical guarantee): a caller that already opened this exact
	// head/base pull request -- createPRBestEffort (pushpr.go) runs on
	// every completed turn, not just the first -- gets back the EXISTING
	// PR's own Number/URL instead of an error, recovered via a follow-up
	// lookup when GitHub's own create call reports "already exists".
	CreatePR(ctx context.Context, spec CreatePRSpec) (PRRef, error)

	// ResolveBranchSHA returns spec.Branch's current commit SHA (or the
	// repo's own default branch's, if spec.Branch is empty) -- §8.5's
	// ("image builds") own real, control-plane-side fingerprint input,
	// resolved directly via the source-control host's API rather than
	// waiting for a sandbox to report anything back (a deliberate design
	// decision: the control plane resolves SHAs itself, independently, see
	// internal/adapters/outbound/githubapi's own implementation doc
	// comment). Errors are plain, exactly like CreatePR above -- no
	// caller of this port retries or trips a circuit-breaker on a
	// resolution failure; a failure here means the caller falls back to
	// the base image for this spawn (§10 Phase 2: "always fall back to
	// base image on any miss -- never block a session"), never a fatal
	// condition.
	//
	// The second return, resolvedBranch, is spec.Branch verbatim when
	// non-empty, or the repo's own real default branch name (the SAME
	// name this call already resolved internally to pick which ref to
	// read the SHA at) when spec.Branch was empty -- audit finding F5's
	// follow-up: a caller that keys any per-branch state (e.g.
	// sessionactor/contractdrift.go's contract-drift snapshot key) off
	// spec.Branch directly would otherwise treat a session left with no
	// explicit branch and one that explicitly names the repo's actual
	// default branch as two different branches, even though they resolve
	// to the exact same ref -- splitting what should be one branch's
	// tracked state into two.
	ResolveBranchSHA(ctx context.Context, spec ResolveBranchSHASpec) (sha string, resolvedBranch string, err error)

	// ResolveContractsFingerprint fingerprints spec.Path's directory
	// listing at spec.Ref ("mocking + contract drift", §14.3).
	// exists=false, err=nil means "no directory exists at that path/ref"
	// -- a legitimate, expected outcome (most repos/refs have no
	// contracts directory at all), NOT an error: callers MUST be able to
	// tell that apart from "the API call itself failed" (exists=false,
	// err!=nil never happens -- on any real failure, fingerprint is ""
	// and exists is false, but err is the one and only signal a caller
	// checks first). On success (err == nil), exists and fingerprint
	// together are authoritative: exists=true means fingerprint is
	// contractdrift.Fingerprint's own real, non-guessed output over the
	// directory's actual current contents.
	ResolveContractsFingerprint(ctx context.Context, spec ResolveContractsFingerprintSpec) (fingerprint string, exists bool, err error)

	// CheckRepoAccess reports whether spec.Token can read spec.Owner/
	// spec.Repo at all, independent of any branch/commit -- the audit fix
	// ("warm-boot image access control") that gates
	// app/sessionactor.resolveAndSetImage (imageresolve.go) before it will
	// either mint a pending image_builds row or warm-hit an already-ready
	// one for a given repo set: neither may happen unless the SPAWNING
	// session's own creator can currently read every one of its repos.
	//
	// (true, nil) means definitively yes. (false, nil) means definitively
	// no (e.g. a real GitHub 404, or a 403 that is NOT itself a rate-limit/
	// abuse-detection response, for this token against this repo) -- a
	// legitimate, expected answer, not a failure; callers use this to deny
	// warm-boot for exactly this spawn, exactly like ResolveContracts
	// Fingerprint's own exists=false, err=nil is a legitimate "no
	// directory" answer, not an error. err != nil means the check itself
	// could not be completed (network/timeout/5xx, OR a 403 that GitHub's
	// own real rate-limit/abuse-detection mechanism produced -- see
	// internal/adapters/outbound/githubapi.isRateLimitedResponse: GitHub
	// returns 403 for both a genuine denial and a rate-limited request, so
	// an implementation MUST distinguish them rather than reporting every
	// 403 as a definitive deny) -- callers MUST NOT treat err != nil as a
	// definitive "no": see resolveAndSetImage's own doc comment for why a
	// genuine "cannot read this repo" and "could not determine access
	// right now" must never collapse into the same on-disk consequence
	// for every session in the fleet.
	CheckRepoAccess(ctx context.Context, spec CheckRepoAccessSpec) (bool, error)

	// GetFileContent reads spec.Path's own content at spec.Ref (
	// §12.2 item 2). exists=false, err=nil means the file does not exist
	// at that ref -- a legitimate answer (the apply-suggestion endpoint
	// treats this as "stale: the file this finding named no longer
	// exists"), never conflated with a genuine API failure (err != nil),
	// mirroring ResolveContractsFingerprint's own identical
	// exists/err-are-independent-signals discipline above.
	GetFileContent(ctx context.Context, spec GetFileContentSpec) (content, sha string, exists bool, err error)

	// UpdateFileContent commits spec.Content onto spec.Branch at spec.Path,
	// replacing the blob spec.SHA names (§12.2 item 2) -- returns
	// the new commit's own SHA. Errors are plain, exactly like CreatePR/
	// ResolveBranchSHA above -- a stale spec.SHA (the file changed since
	// it was read) is one real, expected way this call fails; the caller
	// (reviewfindings.go's own ApplySuggestion handler) surfaces that as a
	// 409, never retries or auto-resolves.
	UpdateFileContent(ctx context.Context, spec UpdateFileContentSpec) (commitSHA string, err error)

	// GetPRBody fetches owner/repo#number's CURRENT body text directly
	// (§26.2) -- the description-autofix notifier's own "read
	// the live body before overwriting it" step, so the original text it
	// preserves (internal/domain/reviewpost.RenderAutofixBody) is always
	// this PR's REAL current body, never the reviewing agent's own
	// possibly-stale copy of it (title+body are untrusted input, §5.2,
	// and may have changed since the agent last saw them). found=false,
	// err=nil means the PR does not exist, or is no longer open/reachable
	// -- a legitimate, expected outcome (mirrors GetOpenPR's own identical
	// "exists=false, err=nil for a confirmed-absent resource" discipline),
	// never conflated with a genuine API failure (err != nil). Errors are
	// plain, exactly like GetFileContent/GetOpenPR above.
	GetPRBody(ctx context.Context, owner, repo string, number int, token string) (body string, found bool, err error)

	// UpdatePRBody overwrites owner/repo#number's own body field with
	// spec.Body (§26.2) -- the description-autofix notifier's
	// own real write, called ONLY after this port's caller has already
	// (a) re-verified server-side that the target PR is Narvi-authored
	// AND this repo's own descriptionAutofix flag is on (§5.2: never
	// trusted from the caller alone), and (b) composed spec.Body itself
	// (this method never composes content, mirroring UpdateFileContent's
	// own identical "caller already computed the resulting content"
	// discipline). NEVER touches the PR's title -- see UpdatePRBodySpec's
	// own doc comment for why that is structurally impossible here, not
	// merely a convention this method happens to follow. Errors are
	// plain, exactly like UpdateFileContent above.
	UpdatePRBody(ctx context.Context, spec UpdatePRBodySpec) error

	// RegisterPRStack groups spec.PRNumbers into a real GitHub stack
	// (§17.2/§17.6) -- a SECOND call, made only after every named PR
	// already exists (this port never creates a PR itself here). Per
	// §17.2's own explicit design: a 404 or any other failure from this
	// call is meant to be logged and otherwise IGNORED by the caller --
	// this method itself still returns a plain error either way (the
	// ignoring is the caller's own policy decision, pushpr.go's
	// createSentinelFixPRBestEffort, not something this port silently
	// swallows on the caller's behalf).
	RegisterPRStack(ctx context.Context, spec RegisterPRStackSpec) error

	// ListMergedBetween reports every PR merged into spec.HeadRef since it
	// diverged from spec.BaseRef ("release PR review", §15.2) --
	// for a release PR itself, spec.BaseRef/HeadRef are simply that PR's
	// own base/head branches, so this answers exactly "which already-
	// individually-reviewed PRs does this release PR bundle". Errors are
	// plain, exactly like every other method on this port -- no caller
	// retries or trips a circuit breaker on a failure here; the manifest
	// check (§15.2) this feeds is best-effort and never blocks anything
	// else in the system on its own success.
	//
	// truncated (audit-fix should-fix #5) reports whether the returned
	// merged slice is KNOWN to be an incomplete picture of everything
	// actually merged in this range -- mirrors GetPullRequestDiff's own
	// identical truncated return exactly, and for the same reason: a
	// caller rendering "no compliance issues found" from an incomplete
	// merged slice would be asserting a completeness guarantee this port
	// never actually gave it. See the githubapi adapter's own
	// implementation doc comment for every source this can be true from
	// (the constituent-PR-count cap, GitHub's own compare-API commit-count
	// cap, and any individual constituent PR silently dropped on its own
	// sub-fetch failure).
	ListMergedBetween(ctx context.Context, spec ListMergedBetweenSpec) (merged []MergedPR, truncated bool, err error)

	// CreateBranch creates a new branch ref (refs/heads/spec.Branch)
	// pointing at spec.SHA (a confirmed-finding fix, §17.2) --
	// IDEMPOTENT: a spec.Branch that already exists is treated as success,
	// never an error, since this method's own one real caller (the
	// sentinel-auto-fix notifier) may be redelivered for the SAME claim
	// before the branch's own creation is durably recorded anywhere else,
	// and re-observing an already-created ref is exactly the outcome an
	// idempotent retry must produce. Errors are plain, exactly like
	// CreatePR/ResolveBranchSHA above -- no caller of this port retries or
	// trips a circuit-breaker on a creation failure beyond the outbox
	// worker's own existing backoff/retry machinery.
	CreateBranch(ctx context.Context, spec CreateBranchSpec) error

	// ListOpenPRsForUser reports every open pull request the account named
	// by spec.GitHubExternalID can currently see and is involved in as an
	// assignee or requested reviewer ("decision inbox: read
	// model + API", §16.2) -- the decision inbox's own primary PR-
	// discovery call. Errors are plain, exactly like every other method on
	// this port except MergePR below -- this is a best-effort READ feeding
	// a cached, explicitly-stale-labeled read model (§16.2), never an
	// interactive action a caller needs a granular failure reason for.
	//
	// truncated mirrors ListMergedBetween's own identical (merged,
	// truncated, err) shape below: true whenever
	// this adapter KNOWS the returned prs slice is an incomplete picture
	// of every PR spec's own account is actually involved in right now --
	// e.g. one of the underlying discovery queries itself failed (a
	// transient 5xx, a rate limit) while another still returned a real,
	// if partial, result; prs itself is never blanked out just because
	// truncated is true (mirrors ListMergedBetween's own "a confirmed
	// exclusion is never folded into truncated, only a genuine coverage
	// gap is" discipline). A caller must never cache a truncated=true
	// result and present it later identically to a confirmed-complete
	// read -- see internal/app/decisioninbox.SCMCache's own handling.
	ListOpenPRsForUser(ctx context.Context, spec ListOpenPRsForUserSpec) (prs []OpenPR, truncated bool, err error)

	// GetOpenPR fetches ONE specific (owner, repo, number) pull request's
	// current OpenPR state DIRECTLY -- no search, no scoping to any
	// particular account's own assignments (§21.2 stage 2). The
	// minimal port extension internal/app/automerge's own machine-
	// initiated caller needs: ListOpenPRsForUser above is fundamentally
	// shaped around "which PRs is THIS human account involved in", a
	// question that has no sensible answer for a background worker
	// acting with a bot credential rather than any particular human's
	// own identity. This method answers the DIFFERENT, simpler question
	// a machine caller actually has: "what is the CURRENT state of this
	// one already-known PR" -- the exact same OpenPR shape (CI
	// conclusion, labels, HasChangesRequested, etc.) ListOpenPRsForUser
	// already builds per candidate, reused via the SAME internal
	// construction (githubapi's own buildOpenPR), never a second,
	// independently-maintained OpenPR builder.
	//
	// found=false, err=nil means the PR does not exist, or is no longer
	// open (closed/merged) -- a legitimate, expected outcome (mirrors
	// GetFileContent's own "exists=false, err=nil" discipline for an
	// absent-but-not-erroneous read), never conflated with a genuine API
	// failure (err != nil). Errors are plain, exactly like every other
	// method on this port except MergePR.
	GetOpenPR(ctx context.Context, owner, repo string, number int, token string) (pr OpenPR, found bool, err error)

	// ResolveCodeOwners resolves CODEOWNERS ownership for every path in
	// spec.Paths against the repo's own CODEOWNERS file at spec.Ref
	// (§16.2) -- see githubapi.ResolveCodeOwners' own doc comment for
	// which of the file's several documented candidate locations is
	// actually read, and internal/domain/codeowners for the pure
	// pattern-matching this method's implementation delegates to. A path
	// with no matching CODEOWNERS rule at all (a real, common outcome for
	// any repo without a catch-all "*" pattern) simply contributes no
	// Owner to the result -- never an error. Errors are plain, exactly
	// like every other method on this port except MergePR below, for the
	// same "best-effort read feeding a cached read model" reason
	// ListOpenPRsForUser's own doc comment gives.
	ResolveCodeOwners(ctx context.Context, spec ResolveCodeOwnersSpec) ([]Owner, error)

	// MergePR merges spec.Number (§16.2's own Merge endpoint) --
	// the ONE method on this port an interactive, human-facing HTTP
	// handler calls synchronously and must render a genuinely different
	// outcome for (see MergePRError's own doc comment for why this is the
	// one method here with a typed error, unlike every other plain-error
	// method on this port). mergeCommitSHA is GitHub's own newly created
	// merge/squash commit SHA on success. A non-2xx GitHub response is
	// always returned as *MergePRError (errors.As-checkable) -- a 405
	// means GitHub itself refuses the merge as not currently mergeable
	// (branch protection, unresolved conflicts, a failing REQUIRED check);
	// a 409 means spec.HeadSHA no longer matches the PR's real current
	// head (someone pushed since the caller's own re-validation read it) --
	// GitHub's own documented optimistic-concurrency guard, load-bearing
	// for §16.2's own "the rendered queue is never trusted as authority"
	// invariant, not a decoration on top of it. Any OTHER non-2xx status
	// is also *MergePRError, with GitHub's own real status/message. A
	// transport/timeout failure that never reached GitHub at all is a
	// PLAIN error instead (errors.As against *MergePRError fails) --
	// mirrors CreatePRError's own identical "typed only for a real HTTP
	// response, plain for a failure below that" precedent.
	MergePR(ctx context.Context, spec MergePRSpec) (mergeCommitSHA string, err error)
}
