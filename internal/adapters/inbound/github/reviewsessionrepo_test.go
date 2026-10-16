package github

import (
	"testing"
)

// TestReviewSessionRepo pins the repository spec a pull request's review
// session is created with (technical plan §21.1, §30.4): always the base
// repository, and the head branch only when it is a branch of the base
// repository -- never a fork's branch name, which would name a branch of
// the base that is not the pull request's.
func TestReviewSessionRepo(t *testing.T) {
	branch := func(s string) *string { return &s }
	tests := []struct {
		name string
		m    mention
		// wantBranch is nil when the spec carries no branch.
		wantBranch *string
	}{
		{
			name:       "a same-repository pull request carries its head branch",
			m:          mention{HeadRepoFullName: "acme/widgets", HeadBranch: branch("feature-x")},
			wantBranch: branch("feature-x"),
		},
		{
			name:       "the head repository is compared without regard to case",
			m:          mention{HeadRepoFullName: "Acme/Widgets", HeadBranch: branch("feature-x")},
			wantBranch: branch("feature-x"),
		},
		{
			name: "a pull request from a fork carries no branch",
			m:    mention{HeadRepoFullName: "contributor/widgets", HeadBranch: branch("feature-x")},
		},
		{
			name: "a fork opened from its own main never names the base's main",
			m:    mention{HeadRepoFullName: "contributor/widgets", HeadBranch: branch("main")},
		},
		{
			name: "a deleted head repository is unknown: no branch",
			m:    mention{HeadBranch: branch("feature-x")},
		},
		{
			name: "an issue_comment mention whose head lookup failed: no branch",
			m:    mention{},
		},
		{
			name: "a same-named owner prefix is another repository",
			m:    mention{HeadRepoFullName: "acme/widgets-fork", HeadBranch: branch("feature-x")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.m.RepoFullName = "acme/widgets"
			tc.m.RepoName = "widgets"
			tc.m.RepoCloneURL = "https://github.com/acme/widgets.git"

			got := reviewSessionRepo(tc.m)
			if got.Name != "widgets" || got.Url != "https://github.com/acme/widgets.git" {
				t.Errorf("repo = %s at %s, want the base repository widgets at https://github.com/acme/widgets.git", got.Name, got.Url)
			}
			switch {
			case tc.wantBranch == nil && got.Branch != nil:
				t.Errorf("branch = %q, want none", *got.Branch)
			case tc.wantBranch != nil && (got.Branch == nil || *got.Branch != *tc.wantBranch):
				t.Errorf("branch = %v, want %q", got.Branch, *tc.wantBranch)
			}
		})
	}
}
