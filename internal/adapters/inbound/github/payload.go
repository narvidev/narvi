package github

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/narvidev/narvi/internal/domain/review"
)

// eventTypeIssueComment and eventTypePullRequestReviewComment are the two
// "X-GitHub-Event" values that can carry a PR @mention (doc.go's own
// writeup) -- GitHub's real event names, verbatim. eventTypePullRequest
// ("review sessions", §8.2) is a THIRD, unrelated trigger: not a
// mention at all, but GitHub's own "labeled" action on a pull request,
// detecting a maintainer's deliberate manual re-trigger command (§5.1: "a
// human applying a label ... is a legitimate, deliberate command").
const (
	eventTypeIssueComment             = "issue_comment"
	eventTypePullRequestReviewComment = "pull_request_review_comment"
	eventTypePullRequest              = "pull_request"
	commentActionCreated              = "created"
	pullRequestActionLabeled          = "labeled"
)

// mention is the detected result of a single webhook delivery: a real,
// action="created" comment on a pull request that mentions the
// configured bot handle. Zero value (ok=false from parseMention) means
// "nothing to act on" -- not an error.
type mention struct {
	// RepoFullName is the CLAIM KEY repo -- always the base/upstream repo
	// the webhook itself is registered on (GitHub's own top-level
	// "repository" field, guaranteed present and stable across event
	// types), never the head/fork repo. Two mentions on the SAME PR must
	// resolve to the SAME RepoFullName regardless of which fork the head
	// branch lives in, since this is exactly what github_pr_sessions'
	// (repo_full_name, pr_number) coalescing key is keyed on.
	RepoFullName string
	// RepoName/RepoCloneURL are the repository the review session clones:
	// always the pull request's base repository, the webhook's own
	// top-level "repository", for every event type and for a pull request
	// from a fork too (technical plan §21.1, §30.4). A code host keeps
	// every pull request's head in its base repository as
	// refs/pull/<number>/head, which the session's SESSION_CONFIG names
	// (sessionactor's pullRequestRef), so the head is read from there,
	// with the base repository's own installation -- never from the fork,
	// whose owner may never have installed the App.
	RepoName     string
	RepoCloneURL string
	// HeadRepoFullName is the "owner/name" of the repository the pull
	// request's head branch lives in (head.repo.full_name): RepoFullName
	// for a pull request opened from a branch of the base repository, a
	// fork's otherwise. Empty when it is not known: an issue_comment
	// mention before, or without, its head resolution
	// (resolveIssueCommentHead), and a pull request whose head repository
	// was deleted (head.repo null). It decides one thing only: whether the
	// session's spec may carry HeadBranch (reviewSessionRepo), a branch
	// that exists in the repository the spec names.
	HeadRepoFullName string
	PRNumber         int32
	// HeadBranch is the pull request's head branch name (head.ref): nil
	// straight out of parseIssueComment (issue_comment's own payload never
	// carries it -- see issueCommentPayload's own doc comment) until
	// handler.go's own resolveIssueCommentHead (headresolve.go) resolves it
	// via a real GitHub API call (H5 audit fix), and set by
	// parsePullRequestReviewComment and parsePullRequestLabeled, whose
	// payloads embed head.ref. A nil HeadBranch after that resolution only
	// ever means "the resolution itself failed" (logged). The session's
	// spec carries it only when the branch is in the base repository the
	// spec names (reviewSessionRepo, handler.go): a fork's branch name
	// names nothing there, and the session's readers that act on the
	// spec's branch -- the sentinel auto-fix and apply-suggestion -- act
	// on the spec's repository.
	HeadBranch *string
	// HeadSHA (§21.1) is the PR's head commit SHA AT THE MOMENT
	// this mention's own event carried/resolved it -- nil exactly when
	// HeadBranch is still nil-and-unresolved (issue_comment, before
	// resolveIssueCommentHead runs), set together with HeadBranch by
	// every other producer (parsePullRequestReviewComment,
	// parsePullRequestLabeled, resolveIssueCommentHead). Threaded into
	// internal/app/reviewcontext.Fetch's own knownHeadSHA parameter --
	// see that function's own doc comment for why this avoids a second,
	// redundant GetPullRequest call on every trigger path that already
	// has this value in hand.
	HeadSHA     *string
	CommentBody string

	// CommenterID/CommenterLogin are the GitHub user id/login of the real
	// human who triggered this event -- for a comment mention
	// (issue_comment/pull_request_review_comment), "comment.user.id"/
	// "comment.user.login" (GitHub's own real webhook shape, IDENTICAL
	// field names/shape across both event types; verified against GitHub's
	// own live webhook-events documentation during this batch's own design
	// phase); for a label retrigger (pull_request/"labeled",
	// "review sessions", §8.2), "sender.id"/"sender.login" -- the actor who
	// APPLIED the label, GitHub's own analogous field for an event with no
	// comment/commenter at all. Both shapes are handled identically by
	// every downstream consumer (identity.go's own resolveCommenterActor,
	// coalesce.go's own authz gates): "the GitHub user id of whoever is
	// asking for this" is the same question regardless of which surface
	// asked it. CommenterID is 0 only if the webhook payload itself omitted
	// the relevant user object entirely (never expected from a real GitHub
	// delivery, but not assumed away).
	//
	// identity.go's own resolveCommenterActor uses CommenterID (never
	// CommenterLogin, which GitHub allows a user to change at any time) to
	// look up a matching Narvi identities row -- see that file's own doc
	// comment for why this needs no auto-linking algorithm, unlike Slack/
	// Linear. CommenterLogin is carried only for logging.
	CommenterID    int64
	CommenterLogin string

	// IsLabelRetrigger (audit fix, §13.3 row 5) reports whether THIS event
	// is §8.2's own manual re-trigger-via-LABEL lane
	// (parsePullRequestLabeled below) rather than an ordinary @mention
	// comment (parseIssueComment/parsePullRequestReviewComment). coalesce.go's
	// own REUSE branch consults this to choose the right authz gate: an
	// ordinary second @mention on an already-tracked PR is just prompting
	// the existing review session (authz.ActionPromptSession, member
	// allowed on own/joined), but a label-triggered re-trigger on that SAME
	// already-tracked PR is §13.3 row 5's "re-trigger reviews"
	// (authz.ActionRetriggerReview, admin/maintainer only, no member
	// carve-out) -- a DIFFERENT, stricter action, even though both reach
	// the identical REUSE code path. false for every event type except a
	// genuine label re-trigger.
	IsLabelRetrigger bool

	// Stack ("review sessions", §17.6's amendment) is non-nil
	// exactly when this event's own webhook payload directly embeds
	// GitHub's own stack object -- today, only ever true for
	// parsePullRequestLabeled below (a native "pull_request" event, the
	// ONE event type §17.6 confirms carries it inline; comment-mention
	// events do not). nil here does NOT mean "not stacked" -- it means "not
	// learned from THIS payload"; handler.go's own review-context fetch
	// (internal/app/reviewcontext.Fetch) falls back to a fresh GitHub API
	// lookup for every OTHER trigger path, using this field as-is only when
	// already populated, to avoid a redundant network round trip for data
	// already in hand (see that function's own doc comment).
	Stack *review.StackContext
}

// parseMention dispatches on eventType and reports whether body is a
// nonEmptyStringPtr returns nil for an empty s, &s otherwise -- §21's
// own small helper for mention.HeadSHA's own "nil means genuinely
// unresolved, never an empty-string placeholder" convention (mirrors
// HeadBranch's own identical *string discipline elsewhere in this
// package), used wherever a webhook payload's own head.sha field is
// folded into a mention struct literal (a conditional assignment, unlike
// headresolve.go's own imperative "if pr.HeadSHA != {}" form, is not
// expressible inline inside a struct literal).
func nonEmptyStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// genuine, actionable review-session trigger: a comment mentioning
// reReviewLabel's own configured bot handle (mentionRE), or a
// pull_request/"labeled" event naming reReviewLabel ("review
// sessions", §8.2's own manual re-trigger-via-label lane). ok=false (nil
// error) means "ignore this delivery" -- an unrecognized event type, a
// non-"created" comment action, a plain-issue (not PR) comment, a comment
// that doesn't mention botHandle, or a pull_request action/label that isn't
// this deployment's own configured re-trigger label. A non-nil error means
// the body itself failed to parse as the shape its own X-GitHub-Event
// header claims.
func parseMention(eventType string, body []byte, mentionRE *regexp.Regexp, reReviewLabel string) (mention, bool, error) {
	switch eventType {
	case eventTypeIssueComment:
		return parseIssueComment(body, mentionRE)
	case eventTypePullRequestReviewComment:
		return parsePullRequestReviewComment(body, mentionRE)
	case eventTypePullRequest:
		return parsePullRequestLabeled(body, reReviewLabel)
	default:
		return mention{}, false, nil
	}
}

// issueCommentPayload is the subset of GitHub's real "issue_comment"
// webhook payload this adapter needs (verified against GitHub's own live
// webhook-events documentation during this Step's own design phase).
// issue_comment fires for comments on BOTH plain issues and pull requests
// -- GitHub's own REST/webhook model treats a PR as a kind of issue --
// distinguished only by whether Issue.PullRequest is present at all.
//
// Unlike pull_request_review_comment below, this payload does NOT embed
// the PR's own head repo/branch anywhere -- only a `pull_request.url` link.
// parseIssueComment below therefore always leaves HeadBranch nil and
// HeadRepoFullName empty, and sets RepoName/RepoCloneURL to the base repo
// as every event type does; handler.go's own resolveIssueCommentHead
// (headresolve.go) is what actually resolves the PR's REAL head
// branch/repo, via one authenticated GitHub REST API call
// (GET /repos/{owner}/{repo}/pulls/{number}), AFTER parseMention returns
// and BEFORE the mention is turned into the session's own repo spec (H5
// audit fix, batch fix/audit-github-pr-payload-correctness) -- §8.2's
// original version of this file left that resolution as a known,
// honestly-documented limitation (no outbound GitHub API credential
// existed in this codebase yet); internal/adapters/outbound/githubapi's
// existing bot credential (already used for PostIssueComment) closes it
// without needing any new kind of credential.
type issueCommentPayload struct {
	Action string `json:"action"`
	Issue  struct {
		Number      int32 `json:"number"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	} `json:"issue"`
	Comment struct {
		// ID (§22.2) is this comment's own globally-unique
		// GitHub id -- "the triggering comment id" a false-positive-
		// pattern capture command is keyed on (falsepositivecapture.go),
		// never used by ordinary mention detection/parseIssueComment
		// itself.
		ID   int64  `json:"id"`
		Body string `json:"body"`
		// User is "who actually wrote this comment" -- GitHub's own real
		// issue_comment webhook shape (comment.user.{id,login}), verified
		// against GitHub's own live webhook-events documentation. Parsed
		// into mention.CommenterID/CommenterLogin below (see that field's
		// own doc comment) -- this batch's own addition, closing the H4
		// audit finding that GitHub ingress never even parsed WHO
		// commented, let alone authorized them.
		User struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Repository struct {
		FullName string `json:"full_name"`
		Name     string `json:"name"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
}

func parseIssueComment(body []byte, mentionRE *regexp.Regexp) (mention, bool, error) {
	var p issueCommentPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return mention{}, false, fmt.Errorf("github: unmarshal issue_comment payload: %w", err)
	}
	if p.Action != commentActionCreated {
		return mention{}, false, nil
	}
	if p.Issue.PullRequest == nil {
		// A comment on a plain issue, not a PR -- §8.2 is PR review only.
		return mention{}, false, nil
	}
	if !mentionRE.MatchString(p.Comment.Body) {
		return mention{}, false, nil
	}
	return mention{
		RepoFullName:   p.Repository.FullName,
		RepoName:       p.Repository.Name,
		RepoCloneURL:   p.Repository.CloneURL,
		PRNumber:       p.Issue.Number,
		HeadBranch:     nil, // resolved later, by handler.go's own resolveIssueCommentHead -- see issueCommentPayload's own doc comment.
		CommentBody:    p.Comment.Body,
		CommenterID:    p.Comment.User.ID,
		CommenterLogin: p.Comment.User.Login,
	}, true, nil
}

// pullRequestReviewCommentPayload is the subset of GitHub's real
// "pull_request_review_comment" webhook payload this adapter needs
// (verified against GitHub's own live webhook-events documentation).
// Unlike issue_comment, this event's payload embeds the FULL pull_request
// object, including its head repo/branch directly -- no further API call
// needed to derive them.
type pullRequestReviewCommentPayload struct {
	Action  string `json:"action"`
	Comment struct {
		// ID (§22.2) mirrors issueCommentPayload.Comment.ID's own
		// identical doc comment -- see that field's own comment.
		ID   int64  `json:"id"`
		Body string `json:"body"`
		// User mirrors issueCommentPayload.Comment.User exactly -- GitHub
		// uses the IDENTICAL comment.user.{id,login} shape for both event
		// types (verified against GitHub's own live webhook-events
		// documentation).
		User struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	PullRequest struct {
		Number int32 `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
			// SHA (§21.1) is this PR's own current head commit
			// -- carried inline on this SAME payload, exactly like Ref,
			// so no further API call is needed to learn it either.
			SHA string `json:"sha"`
			// Repo is a POINTER (L15 audit fix) -- GitHub's own webhook
			// documentation states this field is nullable: null when the
			// head repository has been deleted (e.g. a fork removed after
			// the PR was opened). A plain, non-pointer struct would
			// silently unmarshal a JSON null into an empty-valued struct
			// rather than letting parsePullRequestReviewComment below tell
			// "no head repo" apart -- mirroring
			// issueCommentPayload.Issue.PullRequest's own identical
			// nullable-pointer convention (that field's own doc comment)
			// for the same "GitHub genuinely omits/nulls this" reason. Only
			// its full_name is read: it says whether the head branch is
			// in the base repository (mention.HeadRepoFullName).
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
		// Name/CloneURL are the base repository's, the repository every
		// review session clones -- mirrors issueCommentPayload.Repository's
		// own identical fields.
		Name     string `json:"name"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
}

func parsePullRequestReviewComment(body []byte, mentionRE *regexp.Regexp) (mention, bool, error) {
	var p pullRequestReviewCommentPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return mention{}, false, fmt.Errorf("github: unmarshal pull_request_review_comment payload: %w", err)
	}
	if p.Action != commentActionCreated {
		return mention{}, false, nil
	}
	if !mentionRE.MatchString(p.Comment.Body) {
		return mention{}, false, nil
	}
	headBranch := p.PullRequest.Head.Ref
	m := mention{
		RepoFullName:   p.Repository.FullName, // base/upstream repo -- the claim key (see mention.RepoFullName's own doc comment).
		RepoName:       p.Repository.Name,     // the base repository, cloned for a fork's pull request too (mention.RepoName's doc comment).
		RepoCloneURL:   p.Repository.CloneURL,
		PRNumber:       p.PullRequest.Number,
		HeadBranch:     &headBranch,
		HeadSHA:        nonEmptyStringPtr(p.PullRequest.Head.SHA),
		CommentBody:    p.Comment.Body,
		CommenterID:    p.Comment.User.ID,
		CommenterLogin: p.Comment.User.Login,
	}
	// L15 audit fix: head.repo is null once the head repository is deleted
	// (a fork removed after the PR was opened); HeadRepoFullName then stays
	// empty, unknown, and the spec carries no head branch.
	if p.PullRequest.Head.Repo != nil {
		m.HeadRepoFullName = p.PullRequest.Head.Repo.FullName
	}
	return m, true, nil
}

// labelRetriggerPromptText is the fixed, deterministically-synthesized
// "comment body" a label-triggered manual re-trigger carries as its own
// mention.CommentBody -- a pull_request/"labeled" event has no comment at
// all to reuse (unlike issue_comment/pull_request_review_comment, whose
// own comment body IS the trigger). A plain, fixed constant, never
// model-generated: §5.2's own requirement that any re-run/re-trigger
// phrasing be routable by the intent classifier's deterministic fallback,
// not only its model-based path, holds here trivially -- this text is not
// classified by an LLM at all (coalesce.go's own DeterministicTarget:
// intentdomain.TargetReview already applies to every trigger this package
// creates a session for, comment-mention or label alike), so there is no
// model path for it to depend on in the first place.
const labelRetriggerPromptText = "Manual re-review requested via the configured GitHub label."

// pullRequestPayload is the subset of GitHub's real "pull_request" webhook
// payload this adapter needs (verified against GitHub's own live
// webhook-events documentation) -- §8.2's ("review sessions", §8.2) own
// manual re-trigger-via-label lane. GitHub fires this event type for many
// actions (opened, closed, synchronize, labeled, ...); only "labeled" (with
// a matching label.name) is ever actionable here -- every other action is
// acknowledged and ignored by parsePullRequestLabeled below, mirroring
// issueCommentPayload's own "action != created" gate for comments.
//
// Unlike issue_comment, this event's payload embeds the FULL pull_request
// object directly -- head.ref/head.repo (like pull_request_review_comment
// already does) AND, per §17.6's own confirmed guarantee ("only the
// dedicated pull_request event type is confirmed to [carry stack]"), the
// stack object itself, when this PR belongs to one -- no separate
// GetPullRequest API call needed for either, unlike issue_comment's own
// head-branch resolution (headresolve.go) or every OTHER trigger path's own
// stack-context lookup (internal/app/reviewcontext.Fetch).
type pullRequestPayload struct {
	Action string `json:"action"`
	Label  struct {
		Name string `json:"name"`
	} `json:"label"`
	// Sender is GitHub's own field for "who performed this action" on an
	// event with no comment/commenter concept at all -- the label-applier,
	// mirroring issueCommentPayload.Comment.User's own id/login shape
	// exactly (mention.CommenterID/CommenterLogin's own doc comment above).
	Sender struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	} `json:"sender"`
	PullRequest struct {
		Number int32 `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
			// SHA (§21.1) is this PR's own current head
			// commit -- carried inline on this SAME payload, exactly
			// like Ref/Stack below, so no separate GetPullRequest
			// call is needed for it either.
			SHA string `json:"sha"`
			// Repo mirrors pullRequestReviewCommentPayload's own nullable
			// head.repo exactly: only its full_name is read.
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		// Stack mirrors internal/adapters/outbound/githubapi's own
		// stackResponse shape exactly (that package's own doc comment) --
		// two independent packages each decoding their own copy of the
		// identical GitHub wire shape, deliberately, matching this
		// package's own existing precedent of never sharing a payload
		// struct with the outbound adapter package (pullRequestResponse/
		// pullRequestReviewCommentPayload already each decode head.ref/
		// head.repo independently, for the same reason).
		Stack *struct {
			Size     int `json:"size"`
			Position int `json:"position"`
			Base     struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"base"`
		} `json:"stack"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
		Name     string `json:"name"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
}

// parsePullRequestLabeled detects a genuine manual re-trigger: a
// pull_request event whose action is "labeled" and whose own label.name
// case-sensitively equals reReviewLabel (this deployment's own configured
// label, Config.ReReviewLabel -- a plain, exact string comparison, the
// SAME kind of "deterministic, no model in the loop" check labelRetrigger
// PromptText's own doc comment already describes). Every other action
// value (opened, closed, synchronize, unlabeled, ...) is acknowledged and
// ignored, mirroring issueCommentPayload's own "action != created" gate.
//
// reReviewLabel == "" (a defensively-handled, never-expected-from-real-
// deployment misconfiguration -- platform.Config.Load's own
// defaultGitHubReReviewLabel always supplies a non-empty value) never
// matches any real label.name, so an empty configured label degrades to
// "this lane never fires" rather than a wildcard match against every
// labeled event on every PR.
func parsePullRequestLabeled(body []byte, reReviewLabel string) (mention, bool, error) {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return mention{}, false, fmt.Errorf("github: unmarshal pull_request payload: %w", err)
	}
	if p.Action != pullRequestActionLabeled {
		return mention{}, false, nil
	}
	if reReviewLabel == "" || p.Label.Name != reReviewLabel {
		return mention{}, false, nil
	}

	headBranch := p.PullRequest.Head.Ref
	m := mention{
		RepoFullName:     p.Repository.FullName, // base/upstream repo -- the claim key (see mention.RepoFullName's own doc comment).
		RepoName:         p.Repository.Name,     // the base repository, cloned for a fork's pull request too (mention.RepoName's doc comment).
		RepoCloneURL:     p.Repository.CloneURL,
		PRNumber:         p.PullRequest.Number,
		HeadBranch:       &headBranch,
		HeadSHA:          nonEmptyStringPtr(p.PullRequest.Head.SHA),
		CommentBody:      labelRetriggerPromptText,
		CommenterID:      p.Sender.ID,
		CommenterLogin:   p.Sender.Login,
		IsLabelRetrigger: true,
	}
	// Mirrors parsePullRequestReviewComment's own L15-style handling: a
	// null head.repo (the head repository deleted) leaves HeadRepoFullName
	// empty.
	if p.PullRequest.Head.Repo != nil {
		m.HeadRepoFullName = p.PullRequest.Head.Repo.FullName
	}
	if p.PullRequest.Stack != nil {
		m.Stack = &review.StackContext{
			Position:        p.PullRequest.Stack.Position,
			Size:            p.PullRequest.Stack.Size,
			UltimateBaseRef: p.PullRequest.Stack.Base.Ref,
			UltimateBaseSHA: p.PullRequest.Stack.Base.SHA,
		}
	}
	return m, true, nil
}

// compileMentionPattern builds the case-insensitive "@handle" detector
// mentionRE (used by parseIssueComment/parsePullRequestReviewComment
// above) for a configured bot handle. Deliberately a simple heuristic, not
// an attempt at fully robust NLP mention-parsing:
//
//   - A trailing negative class, `($|[^a-zA-Z0-9_./-])`, requires whatever
//     immediately follows the handle to NOT itself be a character a real
//     GitHub username can be extended with (alnum, "_", or "-"), NOR a
//     "/" or "." -- rejecting both a comment that mentions some OTHER,
//     longer handle merely starting with this one (e.g. handle "narvi"
//     must not match "@narvi-bot-2"; Go RE2's plain `\b` word-boundary
//     alone is NOT sufficient here, since "-" itself already counts as a
//     word boundary, letting "narvi-bot-2" slip through a bare `\b`
//     check) AND a GitHub team mention sharing the handle as a prefix
//     (e.g. handle "narvi" must not match "@narvi/maintainers" -- "/"
//     always starts a team's slug half in "@org/team", and "." can
//     appear in one directly after an org name too).
//   - A negative-lookbehind-equivalent leading class,
//     `(^|[^a-zA-Z0-9_./-])`, requires whatever immediately precedes the
//     "@" to NOT itself be an identifier character -- rejecting an email
//     address's local part directly touching "@" (e.g. "user@narvi...")
//     from being misread as a mention, since Go's RE2 engine has no real
//     lookbehind.
//
// regexp.QuoteMeta escapes botHandle so any regex metacharacter it
// happens to contain (unlikely for a real GitHub handle, but not
// validated elsewhere) is matched literally, never reinterpreted.
func compileMentionPattern(botHandle string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^a-zA-Z0-9_./-])@` + regexp.QuoteMeta(botHandle) + `($|[^a-zA-Z0-9_./-])`)
}
