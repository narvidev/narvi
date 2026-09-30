package reviewpost

import "fmt"

// RerunGuidance renders the exact, server-side re-run instruction every
// posted verdict carries (§5.2: "Any re-run/re-review phrasing a posted
// verdict... recommends to a user is rendered server-side from the
// verdict's own typed fields..., never generated or reproduced by a model
// directly"). botHandle is this deployment's own configured GitHub bot
// handle (platform.Config.GitHubBotHandle, the SAME value internal/
// adapters/inbound/github's own mention detector matches comment bodies
// against).
//
// # Why an @mention, not free natural-language phrasing
//
// §5.2's own corollary: this exact phrasing "must be recognizable by the
// intent classifier's deterministic fail-open fallback... not only by its
// model-based path." A review session exists ONLY on GitHub in this
// codebase today (github_pr_sessions is the sole mechanism creating one,
// internal/adapters/inbound/github/doc.go) -- so the one real detector
// this phrasing must satisfy is that package's own compileMentionPattern
// regex (payload.go), which is ALREADY fully deterministic and
// non-LLM-dependent: parseIssueComment/parsePullRequestReviewComment
// decide "is this a review request" via a plain, compiled regex match on
// the comment body, never a model call at all (coalesce.go's own doc
// comment already states this: "there is no model-based path for either
// trigger to depend on in the first place"). Rendering this guidance as a
// literal "@botHandle ..." mention therefore satisfies §5.2's requirement
// in the STRONGEST available way -- not merely "recognized by a
// deterministic fallback path", but recognized by a mechanism that never
// touches the classifier's model-based path AT ALL, so it holds
// regardless of the classifier's own health. internal/adapters/inbound/
// github's own rerunguidance_test.go proves this exact rendered string
// against the REAL compileMentionPattern regex, not merely by inspection.
//
// The mention itself is deliberately surrounded by plain spaces (never
// wrapped in markdown backticks or punctuation touching the "@" directly)
// -- compileMentionPattern requires whatever immediately precedes "@" and
// immediately follows the handle to NOT itself be an identifier character
// (alnum/"_"/"."/"-"/"/"); an ordinary space on both sides is always a
// safe, unambiguous boundary, avoiding any dependency on exactly which
// punctuation happens to be adjacent.
//
// # An empty handle renders the button-only sentence
//
// The bot handle is required only with GitHub ingress on (§12.5), while a
// verdict is posted whenever GitHub OUTBOUND is on -- so a deployment that
// posts reviews without mounting the webhook has no mention anyone could
// type, and nothing that would detect one. It gets the label/button
// guidance alone, never "comment @ review".
func RerunGuidance(botHandle string) string {
	if botHandle == "" {
		return "To ask for another review after pushing changes, use this deployment's configured re-review label or button on this pull request."
	}
	return fmt.Sprintf(
		"To ask for another review after pushing changes, comment @%s review on this pull request (or use this deployment's configured re-review label/button).",
		botHandle,
	)
}
