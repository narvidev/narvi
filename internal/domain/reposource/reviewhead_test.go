package reposource

import "testing"

// TestReviewHeadInBaseRepository pins the one rule that says a review
// session's head is a branch of its pull request's base repository: the
// claim records the head in the base, and the primary repo names the claim's
// repository and carries a branch. Every other shape -- a fork's, a deleted
// fork's, a failed head lookup's, a legacy session still naming the fork, an
// unrecorded claim, anything that does not parse -- is false.
func TestReviewHeadInBaseRepository(t *testing.T) {
	const claim = "acme/widgets"
	yes, no := true, false
	const sameRepo = `[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":"feature-x"}]`
	// The spec the ingress wrote, before the claim recorded anything, for a
	// pull request whose fork was already deleted: the base's url, the
	// deleted fork's branch name -- byte for byte a same-repository spec.
	const deletedForkLegacy = `[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":"main"}]`
	recordTests := []struct {
		name   string
		record *bool
		repos  string
		want   bool
	}{
		{name: "a same-repository spec, recorded in the base", record: &yes, repos: sameRepo, want: true},
		{name: "a same-repository spec, unrecorded: unknown, so not in the base", repos: sameRepo},
		{name: "a same-repository spec, recorded not in the base", record: &no, repos: sameRepo},
		{name: "a deleted fork's legacy spec, unrecorded", repos: deletedForkLegacy},
		{name: "a deleted fork's legacy spec, recorded not in the base", record: &no, repos: deletedForkLegacy},
		{name: "a fork's spec, recorded in the base: the spec still fails", record: &yes, repos: `[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":null}]`},
		{name: "a legacy fork spec, recorded in the base: the spec still fails", record: &yes, repos: `[{"name":"widgets","url":"https://github.com/contributor/widgets.git","branch":"main"}]`},
	}
	for _, tc := range recordTests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReviewHeadInBaseRepository(claim, tc.record, []byte(tc.repos)); got != tc.want {
				t.Errorf("ReviewHeadInBaseRepository(%q, %v, %s) = %v, want %v", claim, tc.record, tc.repos, got, tc.want)
			}
		})
	}

	// The spec's own conditions, on a claim that records the head in the
	// base.
	tests := []struct {
		name  string
		claim string
		repos string
		want  bool
	}{
		{name: "a branch of the base repository", claim: claim, repos: `[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":"feature-x"}]`, want: true},
		{name: "names compared without regard to case or a .git suffix", claim: "Acme/Widgets", repos: `[{"name":"widgets","url":"https://github.com/ACME/widgets","branch":"feature-x"}]`, want: true},
		{name: "the host is not compared: the claim records none", claim: claim, repos: `[{"name":"widgets","url":"https://ghes.example.test/acme/widgets.git","branch":"feature-x"}]`, want: true},
		{name: "a secondary repo does not count; the primary does", claim: claim, repos: `[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":"feature-x"},{"name":"tools","url":"https://github.com/contributor/tools.git","branch":"main"}]`, want: true},
		{name: "a pull request from a fork: the base, no branch", claim: claim, repos: `[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":null}]`},
		{name: "a failed head lookup: the base, no branch", claim: claim, repos: `[{"name":"widgets","url":"https://github.com/acme/widgets.git"}]`},
		{name: "an empty branch", claim: claim, repos: `[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":""}]`},
		{name: "a legacy session still naming the fork, with the fork's branch", claim: claim, repos: `[{"name":"widgets","url":"https://github.com/contributor/widgets.git","branch":"main"}]`},
		{name: "a primary repo that is another repository", claim: claim, repos: `[{"name":"tools","url":"https://github.com/contributor/tools.git","branch":"main"},{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":"feature-x"}]`},
		{name: "a same-named owner prefix is another repository", claim: claim, repos: `[{"name":"widgets","url":"https://github.com/acme/widgets-fork.git","branch":"main"}]`},
		{name: "a url that is not https", claim: claim, repos: `[{"name":"widgets","url":"http://github.com/acme/widgets.git","branch":"feature-x"}]`},
		{name: "a nested path names no owner/name", claim: claim, repos: `[{"name":"widgets","url":"https://github.com/group/acme/widgets.git","branch":"feature-x"}]`},
		{name: "a claim not in owner/name shape", claim: "widgets", repos: `[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":"feature-x"}]`},
		{name: "no repos", claim: claim, repos: `[]`},
		{name: "repos that are not a list", claim: claim, repos: `{"url":"https://github.com/acme/widgets.git","branch":"feature-x"}`},
		{name: "no spec at all", claim: claim, repos: ``},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReviewHeadInBaseRepository(tc.claim, &yes, []byte(tc.repos)); got != tc.want {
				t.Errorf("ReviewHeadInBaseRepository(%q, true, %s) = %v, want %v", tc.claim, tc.repos, got, tc.want)
			}
		})
	}
}

func TestURLNamesRepository(t *testing.T) {
	tests := []struct {
		url, fullName string
		want          bool
	}{
		{"https://github.com/acme/widgets.git", "acme/widgets", true},
		{"https://github.com/Acme/Widgets", "acme/widgets", true},
		{"https://github.com/acme/widgets.git", "acme/gadgets", false},
		{"https://github.com/acme/widgets.git", "acme", false},
		{"not a url %%", "acme/widgets", false},
		{"https://github.com/acme", "acme/widgets", false},
	}
	for _, tc := range tests {
		if got := URLNamesRepository(tc.url, tc.fullName); got != tc.want {
			t.Errorf("URLNamesRepository(%q, %q) = %v, want %v", tc.url, tc.fullName, got, tc.want)
		}
	}
}
