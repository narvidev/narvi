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
// repository (technical plan §30.4): the session's primary repo, position 0
// of sessionRepos (sessions.repos), is an https URL naming
// claimRepoFullName, the repository the session's github_pr_sessions claim
// names, and carries a branch.
//
// The GitHub ingress writes a branch into a review session's spec only when
// the pull request's head repository is its base repository
// (internal/adapters/inbound/github's reviewSessionRepo), so this is false
// for a pull request from a fork, from a deleted fork, and one whose head
// could not be read when the review started -- and for a legacy session
// whose url still names the fork, whatever its branch. It fails closed:
// sessionRepos that do not decode, an empty list, a url that is not https
// or names no owner/name, and a claim not in owner/name shape all read
// false.
//
// Whatever reads false is code a person outside the base repository may
// have written, or a branch that is not the pull request's: the sandbox
// secrets and the repository-scoped provider credentials of the base
// repository are withheld from such a session, and apply-suggestion and the
// sentinel auto-fix, which act on the base repository at the spec's branch,
// refuse it.
func ReviewHeadInBaseRepository(claimRepoFullName string, sessionRepos []byte) bool {
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
