package automation_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/automation"
)

func TestClassifyGitHubDispatch(t *testing.T) {
	tests := []struct {
		eventType string
		want      automation.GitHubDispatchSkipReason
	}{
		{"pull_request", automation.GitHubDispatchNotSkipped},
		{"issues", automation.GitHubDispatchNotSkipped},
		{"issue_comment", automation.GitHubDispatchNotSkipped},
		{"push", automation.GitHubDispatchNotSkipped},
		{"check_run", automation.GitHubDispatchNotSkipped},
		{"status", automation.GitHubDispatchNotSkipped},
		{"pull_request_review_comment", automation.GitHubDispatchSkipEventNotAllowlisted},
		{"release", automation.GitHubDispatchSkipEventNotAllowlisted},
		{"", automation.GitHubDispatchSkipEventNotAllowlisted},
	}
	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			if got := automation.ClassifyGitHubDispatch(tt.eventType); got != tt.want {
				t.Fatalf("ClassifyGitHubDispatch(%q) = %q, want %q", tt.eventType, got, tt.want)
			}
		})
	}
}

func TestClassifyLinearDispatch(t *testing.T) {
	tests := []struct {
		eventType string
		want      automation.LinearDispatchSkipReason
	}{
		{"Issue", automation.LinearDispatchNotSkipped},
		{"Comment", automation.LinearDispatchNotSkipped},
		{"AgentSessionEvent", automation.LinearDispatchSkipEventNotAllowlisted},
		{"PermissionChange", automation.LinearDispatchSkipEventNotAllowlisted},
		{"", automation.LinearDispatchSkipEventNotAllowlisted},
	}
	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			if got := automation.ClassifyLinearDispatch(tt.eventType); got != tt.want {
				t.Fatalf("ClassifyLinearDispatch(%q) = %q, want %q", tt.eventType, got, tt.want)
			}
		})
	}
}

func TestRepoFullNameFromCloneURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantFull string
		wantOK   bool
	}{
		{"plain", "https://github.com/acme/repo", "acme/repo", true},
		{"dot-git suffix", "https://github.com/acme/repo.git", "acme/repo", true},
		{"no host", "not-a-url", "", false},
		{"single segment path", "https://github.com/acme", "", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := automation.RepoFullNameFromCloneURL(tt.url)
			if ok != tt.wantOK || got != tt.wantFull {
				t.Fatalf("RepoFullNameFromCloneURL(%q) = (%q, %v), want (%q, %v)", tt.url, got, ok, tt.wantFull, tt.wantOK)
			}
		})
	}
}

func TestTipBranchNames(t *testing.T) {
	branches := []automation.GitHubEventBranch{
		{Name: "main", HeadSHA: "shaMain"},
		{Name: "release", HeadSHA: "shaMain"}, // a tie: two branches sharing one tip commit
		{Name: "feature-x", HeadSHA: "shaOld"},
	}

	tests := []struct {
		name string
		sha  string
		want []string
	}{
		{"matches two tied tips", "shaMain", []string{"main", "release"}},
		{"matches the older tip", "shaOld", []string{"feature-x"}},
		{"matches nothing", "shaNeverSeen", nil},
		{"empty sha matches nothing, even if a branch happens to have an empty HeadSHA", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := automation.TipBranchNames(tt.sha, branches)
			if len(got) != len(tt.want) {
				t.Fatalf("TipBranchNames(%q) = %v, want %v", tt.sha, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("TipBranchNames(%q) = %v, want %v", tt.sha, got, tt.want)
				}
			}
		})
	}
}

// TestTargetMatchesGitHubEvent_BranchTipNotContainment is §8.4's own
// required proof: a test that would PASS under a naive "is target.Branch
// present anywhere in the event's own branches[] list" containment check,
// and FAILS under that same naive check for the one reason that matters --
// only asserting tip identity (branches[i].HeadSHA == in.SHA) closes it.
//
// Setup: a commit ("shaMain") that is main's own current tip AND is
// reachable from (hence "contained by", per GitHub's own branches[] field
// semantics) a feature branch cut from main earlier. The event's branches[]
// array reflects exactly that shape: feature-x IS listed (the naive
// membership check would find its name there and stop), but its own
// recorded HeadSHA ("shaFeatureTip") is NOT the commit this event pertains
// to -- feature-x has moved on since. A target scoped to feature-x must
// NOT fire for this event; a target scoped to main (the real tip) must.
func TestTargetMatchesGitHubEvent_BranchTipNotContainment(t *testing.T) {
	in := automation.GitHubEventInput{
		EventType:    "status",
		RepoFullName: "acme/repo",
		SHA:          "shaMain",
		Branches: []automation.GitHubEventBranch{
			{Name: "main", HeadSHA: "shaMain"},
			// feature-x CONTAINS shaMain (it was cut from main at or after
			// that commit) but its own tip has since moved to shaFeatureTip
			// -- exactly the "branch that merely contains the commit" case
			// §8.4 names.
			{Name: "feature-x", HeadSHA: "shaFeatureTip"},
		},
	}

	mainTarget := automation.Target{Name: "repo", URL: "https://github.com/acme/repo", Branch: "main"}
	featureTarget := automation.Target{Name: "repo", URL: "https://github.com/acme/repo", Branch: "feature-x"}

	if !automation.TargetMatchesGitHubEvent(mainTarget, in) {
		t.Fatalf("TargetMatchesGitHubEvent(main) = false, want true: main IS this commit's own current tip")
	}
	if automation.TargetMatchesGitHubEvent(featureTarget, in) {
		t.Fatalf("TargetMatchesGitHubEvent(feature-x) = true, want false: feature-x only CONTAINS this commit, it is not its tip -- " +
			"a naive membership-in-branches[] check would wrongly pass this case")
	}
}

func TestTargetMatchesGitHubEvent_RepoScoping(t *testing.T) {
	in := automation.GitHubEventInput{EventType: "pull_request", RepoFullName: "acme/repo"}

	sameRepo := automation.Target{Name: "repo", URL: "https://github.com/acme/repo"}
	otherRepo := automation.Target{Name: "other", URL: "https://github.com/acme/other"}
	caseDiffers := automation.Target{Name: "repo", URL: "https://github.com/Acme/Repo"}

	if !automation.TargetMatchesGitHubEvent(sameRepo, in) {
		t.Fatalf("same repo: got false, want true")
	}
	if automation.TargetMatchesGitHubEvent(otherRepo, in) {
		t.Fatalf("different repo: got true, want false")
	}
	if !automation.TargetMatchesGitHubEvent(caseDiffers, in) {
		t.Fatalf("case-insensitive repo match: got false, want true")
	}
}

func TestTargetMatchesGitHubEvent_BranchScopedTargetRequiresTipEvidence(t *testing.T) {
	// No Branches data at all (e.g. issues/issue_comment, or any event
	// type a caller failed to populate Branches for) -- a branch-scoped
	// target must fail closed, never assume a match.
	in := automation.GitHubEventInput{EventType: "issues", RepoFullName: "acme/repo"}
	scoped := automation.Target{Name: "repo", URL: "https://github.com/acme/repo", Branch: "main"}

	if automation.TargetMatchesGitHubEvent(scoped, in) {
		t.Fatalf("branch-scoped target matched an event with no branch-tip evidence at all, want false (fail closed)")
	}
}

func TestMatchesGitHubTrigger_NameAndConclusion(t *testing.T) {
	tests := []struct {
		name string
		cfg  automation.GitHubTriggerConfig
		in   automation.GitHubEventInput
		want bool
	}{
		{
			"name required and matches",
			automation.GitHubTriggerConfig{Event: "check_run", Name: "ci/lint"},
			automation.GitHubEventInput{EventType: "check_run", Name: "ci/lint"},
			true,
		},
		{
			"name required but mismatches",
			automation.GitHubTriggerConfig{Event: "check_run", Name: "ci/lint"},
			automation.GitHubEventInput{EventType: "check_run", Name: "ci/test"},
			false,
		},
		{
			"conclusion required and matches",
			automation.GitHubTriggerConfig{Event: "status", Conclusion: "success"},
			automation.GitHubEventInput{EventType: "status", Conclusion: "success"},
			true,
		},
		{
			"conclusion required but mismatches",
			automation.GitHubTriggerConfig{Event: "status", Conclusion: "success"},
			automation.GitHubEventInput{EventType: "status", Conclusion: "failure"},
			false,
		},
		{
			"name and conclusion both required and both match",
			automation.GitHubTriggerConfig{Event: "check_run", Name: "ci/lint", Conclusion: "failure"},
			automation.GitHubEventInput{EventType: "check_run", Name: "ci/lint", Conclusion: "failure"},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := automation.MatchesGitHubTrigger(tt.cfg, tt.in); got != tt.want {
				t.Fatalf("MatchesGitHubTrigger() = %v, want %v", got, tt.want)
			}
		})
	}
}
