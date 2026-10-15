package reposource

import (
	"encoding/json"
	"strings"
)

// URLNamesRepository reports whether a repo clone URL's path names the
// repository fullName ("owner/name"): ParseOwnerRepo's owner and name,
// compared with fullName's without regard to case, as GitHub compares
// names. It is host-agnostic, like ParseOwnerRepo: a pull request's claim
// (github_pr_sessions.repo_full_name) records no host to compare with.
func URLNamesRepository(rawURL, fullName string) bool {
	wantOwner, wantName, ok := SplitFullName(fullName)
	if !ok {
		return false
	}
	owner, name, err := ParseOwnerRepo(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(owner, wantOwner) && strings.EqualFold(name, wantName)
}

// ReviewHeadInBaseRepository reports whether a pull request's review
// session's head is known to be a branch of the pull request's base
// repository (technical plan §27.1, §30.4). Both must hold:
//
//   - claimHeadInBase, the session's github_pr_sessions claim's record
//     (head_in_base, migrations/000172), is true. The GitHub ingress writes
//     it on every mention that read the pull request's head, from its
//     head.repo: true only when the head repository is the claim's, false
//     for a fork's and a deleted fork's. A mention that read no head writes
//     nothing. A claim nothing has recorded yet (nil) is unknown, and reads
//     false until a mention reads the head.
//   - the session's primary repo, position 0 of sessionRepos
//     (sessions.repos), is an https URL naming claimRepoFullName and carries
//     a branch -- the spec the ingress writes for a same-repository pull
//     request, and the branch apply-suggestion and the sentinel auto-fix
//     act on.
//
// The spec alone cannot decide it: a pull request whose fork was deleted
// before its first mention got, from the ingress before the record existed,
// the base repository's url with the deleted fork's branch name -- the
// same-repository shape exactly. The record does. A fork's spec and a
// legacy session's still naming the fork fail the second condition whatever
// the record says, and so does a same-repository session opened while its
// head could not be read (no branch) -- until a mention that reads the head
// in the base gives its spec the branch (ReviewSpecAwaitsHeadBranch).
//
// It fails closed: an unrecorded claim, sessionRepos that do not decode, an
// empty list, a url that is not https or names no owner/name, and a claim
// not in owner/name shape all read false.
//
// Whatever reads false is code a person outside the base repository may
// have written, or a branch that is not the pull request's: the sandbox
// secrets and the repository-scoped provider credentials of the base
// repository are withheld from such a session, and apply-suggestion and the
// sentinel auto-fix, which act on the base repository at the spec's branch,
// refuse it.
func ReviewHeadInBaseRepository(claimRepoFullName string, claimHeadInBase *bool, sessionRepos []byte) bool {
	if claimHeadInBase == nil || !*claimHeadInBase {
		return false
	}
	var repos []struct {
		URL    string  `json:"url"`
		Branch *string `json:"branch"`
	}
	if err := json.Unmarshal(sessionRepos, &repos); err != nil || len(repos) == 0 {
		return false
	}
	primary := repos[0]
	if primary.Branch == nil || *primary.Branch == "" {
		return false
	}
	if ValidateRepoURL(primary.URL) != nil {
		return false
	}
	return URLNamesRepository(primary.URL, claimRepoFullName)
}

// ReviewSpecAwaitsHeadBranch reports whether a pull request's review
// session's spec is the one the GitHub ingress writes when it could not
// read the pull request's head: the primary repo, position 0 of
// sessionRepos, an https URL naming claimRepoFullName, with no branch. It
// returns that repo's url, which the repair (the sessions store's
// SetReviewSessionHeadBranch) requires unchanged when it writes the head
// branch a later mention read in the base (technical plan §27.1, §30.4).
// A spec that already carries a branch -- a same-repository one, or a
// legacy deleted fork's naming the fork's branch -- a fork's legacy spec
// still naming the fork, and anything that does not decode read false:
// the repair never replaces a branch and never acts on another repository.
func ReviewSpecAwaitsHeadBranch(claimRepoFullName string, sessionRepos []byte) (primaryURL string, ok bool) {
	var repos []struct {
		URL    string  `json:"url"`
		Branch *string `json:"branch"`
	}
	if err := json.Unmarshal(sessionRepos, &repos); err != nil || len(repos) == 0 {
		return "", false
	}
	primary := repos[0]
	if primary.Branch != nil && *primary.Branch != "" {
		return "", false
	}
	if ValidateRepoURL(primary.URL) != nil || !URLNamesRepository(primary.URL, claimRepoFullName) {
		return "", false
	}
	return primary.URL, true
}
