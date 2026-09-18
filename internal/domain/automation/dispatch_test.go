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

// TestRepoFullNameFromCloneURL is D7's own pinned input/output table --
// every row here was RUN (not merely reasoned about) against this exact
// function via a throwaway program before this fix, and again after, to
// confirm each verdict (see this batch's own PR body for the full table).
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
		// D7 audit fix: the host is no longer discarded -- a target
		// hosted on a different forge with the identical owner/repo path
		// must not match a github.com event.
		{"gitlab host, same path shape, rejected", "https://gitlab.com/acme/repo", "", false},
		{"bitbucket host, same path shape, rejected", "https://bitbucket.org/acme/repo", "", false},
		{"github enterprise host, rejected (not configurable anywhere today)", "https://github.example.com/acme/repo", "", false},
		{"host case-insensitive", "https://GitHub.COM/acme/repo", "acme/repo", true},
		// D7 audit fix: a trailing slash (with or without a preceding
		// ".git" suffix) is now stripped -- previously left dangling in
		// the returned full name, so a legitimately-typed clone URL with
		// a trailing slash could never again match a real GitHub event
		// (full_name never carries one).
		{"trailing slash", "https://github.com/acme/repo/", "acme/repo", true},
		{"dot-git suffix then trailing slash", "https://github.com/acme/repo.git/", "acme/repo", true},
		// D7 audit fix: an empty path segment (e.g. a doubled slash) used
		// to slip through the old bare strings.Contains(path, "/") check.
		{"empty owner segment (doubled slash)", "https://github.com//repo", "", false},
		{"extra path segment beyond owner/repo", "https://github.com/acme/repo/extra", "", false},
		// SSH remotes: the scp-like "user@host:path" form is not parsed
		// as a host by net/url at all (Host == "" -- fails closed, same
		// as before this fix); the real "ssh://" scheme form IS parsed
		// with a genuine Host and is accepted exactly like an https one.
		{"scp-like ssh remote (not host-parseable, fails closed)", "git@github.com:acme/repo.git", "", false},
		{"ssh scheme remote", "ssh://git@github.com/acme/repo.git", "acme/repo", true},
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
	// Branch-agnostic repo scoping is exercised through an EXPLICITLY
	// branch-scoped target (never the default "" target -- since D4, an
	// unconfigured target's own branch semantics are a SEPARATE, dedicated
	// question, covered by TestTargetMatchesGitHubEvent_UnconfiguredBranch*
	// below), so this test stays a pure proof of repo-scoping alone.
	in := automation.GitHubEventInput{
		EventType: "pull_request", RepoFullName: "acme/repo",
		SHA:      "shaMain",
		Branches: []automation.GitHubEventBranch{{Name: "main", HeadSHA: "shaMain"}},
	}

	sameRepo := automation.Target{Name: "repo", URL: "https://github.com/acme/repo", Branch: "main"}
	otherRepo := automation.Target{Name: "other", URL: "https://github.com/acme/other", Branch: "main"}
	caseDiffers := automation.Target{Name: "repo", URL: "https://github.com/Acme/Repo", Branch: "main"}

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

// TestTargetMatchesGitHubEvent_UnconfiguredBranchMatchesOnlyDefaultBranchTip
// is D4's own pinned proof for the DEFAULT automation configuration (no UI/
// API caller is forced to set Target.Branch at all) -- the mutation-visible
// guard TestTargetMatchesGitHubEvent_BranchTipNotContainment already proves
// CORRECT when reached; this test proves it is actually REACHED for the
// unconfigured case too. Before this fix, target.Branch == "" returned true
// immediately, before ever reaching this same tip-check -- an unconfigured
// target fired for an event on ANY branch, while the run it created still
// silently checked out the repo's own default branch regardless (fanout.go,
// Target.Branch == "" -> "use the repo's own default branch"). An event
// whose own tip-branch is something OTHER than the repo's own default
// branch must NOT match an unconfigured target.
func TestTargetMatchesGitHubEvent_UnconfiguredBranchMatchesOnlyDefaultBranchTip(t *testing.T) {
	in := automation.GitHubEventInput{
		EventType:     "push",
		RepoFullName:  "acme/repo",
		DefaultBranch: "main",
		SHA:           "shaFeature",
		Branches:      []automation.GitHubEventBranch{{Name: "feature-x", HeadSHA: "shaFeature"}},
	}
	unconfigured := automation.Target{Name: "repo", URL: "https://github.com/acme/repo"}

	if automation.TargetMatchesGitHubEvent(unconfigured, in) {
		t.Fatalf("unconfigured target matched a push to feature-x (not the repo's own default branch, main), want false")
	}
}

// TestTargetMatchesGitHubEvent_UnconfiguredBranchMatchesDefaultBranchTip is
// the positive half of the proof immediately above: an event whose own
// tip IS the repo's own default branch (in.DefaultBranch) DOES match an
// unconfigured target.
func TestTargetMatchesGitHubEvent_UnconfiguredBranchMatchesDefaultBranchTip(t *testing.T) {
	in := automation.GitHubEventInput{
		EventType:     "push",
		RepoFullName:  "acme/repo",
		DefaultBranch: "main",
		SHA:           "shaMain",
		Branches:      []automation.GitHubEventBranch{{Name: "main", HeadSHA: "shaMain"}},
	}
	unconfigured := automation.Target{Name: "repo", URL: "https://github.com/acme/repo"}

	if !automation.TargetMatchesGitHubEvent(unconfigured, in) {
		t.Fatalf("unconfigured target did not match a push to main (the repo's own default branch), want true")
	}
}

// TestTargetMatchesGitHubEvent_UnconfiguredBranchAlwaysMatchesNoBranchConceptEvents
// proves the carve-out immediately above the D4 fix: "issues"/
// "issue_comment" have no branch identity at all, so an unconfigured
// target keeps matching unconditionally for THEM, even with no
// DefaultBranch/Branches evidence whatsoever -- unlike every OTHER event
// type (proven by the sibling FailsClosed test below, using "push").
func TestTargetMatchesGitHubEvent_UnconfiguredBranchAlwaysMatchesNoBranchConceptEvents(t *testing.T) {
	for _, eventType := range []string{"issues", "issue_comment"} {
		t.Run(eventType, func(t *testing.T) {
			in := automation.GitHubEventInput{EventType: eventType, RepoFullName: "acme/repo"}
			unconfigured := automation.Target{Name: "repo", URL: "https://github.com/acme/repo"}
			if !automation.TargetMatchesGitHubEvent(unconfigured, in) {
				t.Fatalf("unconfigured target did not match a bare %s event (no branch concept, should always match), want true", eventType)
			}
		})
	}
}

// TestTargetMatchesGitHubEvent_UnconfiguredBranchFailsClosedWithNoDefaultBranchEvidence
// covers a caller that never resolved in.DefaultBranch at all -- fail
// closed, never assume an unconfigured target is "close enough".
func TestTargetMatchesGitHubEvent_UnconfiguredBranchFailsClosedWithNoDefaultBranchEvidence(t *testing.T) {
	in := automation.GitHubEventInput{
		EventType:    "push",
		RepoFullName: "acme/repo",
		SHA:          "shaMain",
		Branches:     []automation.GitHubEventBranch{{Name: "main", HeadSHA: "shaMain"}},
	}
	unconfigured := automation.Target{Name: "repo", URL: "https://github.com/acme/repo"}

	if automation.TargetMatchesGitHubEvent(unconfigured, in) {
		t.Fatalf("unconfigured target matched with no in.DefaultBranch evidence at all, want false (fail closed)")
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
