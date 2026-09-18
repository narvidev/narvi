package github

import (
	"testing"

	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
)

func TestBuildGitHubEventInput_MalformedBodyReturnsNotOK(t *testing.T) {
	_, ok := buildGitHubEventInput("pull_request", []byte("{not json"))
	if ok {
		t.Fatalf("ok = true, want false for a malformed body")
	}
}

func TestBuildGitHubEventInput_PullRequestLabeled_MergesTopLevelAndArrayLabels(t *testing.T) {
	body := []byte(`{
		"action": "labeled",
		"label": {"name": "automation:run"},
		"repository": {"full_name": "acme/repo"},
		"pull_request": {
			"labels": [{"name": "bug"}, {"name": "automation:run"}],
			"head": {"ref": "feature-x", "sha": "shaHead"}
		}
	}`)
	in, ok := buildGitHubEventInput("pull_request", body)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if in.RepoFullName != "acme/repo" {
		t.Fatalf("RepoFullName = %q, want %q", in.RepoFullName, "acme/repo")
	}
	if in.SHA != "shaHead" {
		t.Fatalf("SHA = %q, want %q", in.SHA, "shaHead")
	}
	if len(in.Branches) != 1 || in.Branches[0].Name != "feature-x" || in.Branches[0].HeadSHA != "shaHead" {
		t.Fatalf("Branches = %v, want one {feature-x, shaHead}", in.Branches)
	}
	// The top-level "label" (the ONE that was just applied) must appear
	// even though pull_request.labels[] ALSO already lists it -- a
	// minimal, real-shaped payload that omits pull_request.labels[]
	// entirely (GitHub does not always include it) must still surface the
	// label that triggered this delivery.
	wantLabels := map[string]bool{"automation:run": true, "bug": true}
	if len(in.Labels) < 1 {
		t.Fatalf("Labels = %v, want at least automation:run present", in.Labels)
	}
	for _, l := range in.Labels {
		if !wantLabels[l] {
			t.Fatalf("unexpected label %q in %v", l, in.Labels)
		}
	}
	found := false
	for _, l := range in.Labels {
		if l == "automation:run" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Labels = %v, want automation:run present", in.Labels)
	}
}

func TestBuildGitHubEventInput_PullRequestLabeled_TopLevelLabelOnly(t *testing.T) {
	// The exact minimal shape this package's own handler_integration_test.
	// go pullRequestLabeledBody produces: only the top-level "label"
	// object, no pull_request.labels[] array at all.
	body := []byte(`{
		"action": "labeled",
		"label": {"name": "automation:run"},
		"repository": {"full_name": "acme/repo"},
		"pull_request": {"head": {"ref": "feature-x"}}
	}`)
	in, ok := buildGitHubEventInput("pull_request", body)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if len(in.Labels) != 1 || in.Labels[0] != "automation:run" {
		t.Fatalf("Labels = %v, want [automation:run]", in.Labels)
	}
}

func TestBuildGitHubEventInput_Issues_LabelsFromIssueLabelsArray(t *testing.T) {
	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "acme/repo"},
		"issue": {"labels": [{"name": "triage"}]}
	}`)
	in, ok := buildGitHubEventInput("issues", body)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if len(in.Labels) != 1 || in.Labels[0] != "triage" {
		t.Fatalf("Labels = %v, want [triage]", in.Labels)
	}
	if len(in.Branches) != 0 {
		t.Fatalf("Branches = %v, want none (issues has no branch concept)", in.Branches)
	}
}

func TestBuildGitHubEventInput_Push_BranchNotTag(t *testing.T) {
	tests := []struct {
		name       string
		ref        string
		after      string
		wantBranch string
	}{
		{"ordinary branch push", "refs/heads/main", "shaAfter", "main"},
		{"tag push is not a branch", "refs/tags/v1.0.0", "shaAfter", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"ref": "` + tt.ref + `", "after": "` + tt.after + `", "repository": {"full_name": "acme/repo"}}`)
			in, ok := buildGitHubEventInput("push", body)
			if !ok {
				t.Fatalf("ok = false, want true")
			}
			if tt.wantBranch == "" {
				if len(in.Branches) != 0 {
					t.Fatalf("Branches = %v, want none for a tag push", in.Branches)
				}
				return
			}
			if len(in.Branches) != 1 || in.Branches[0].Name != tt.wantBranch || in.Branches[0].HeadSHA != tt.after {
				t.Fatalf("Branches = %v, want one {%s, %s}", in.Branches, tt.wantBranch, tt.after)
			}
			if in.SHA != tt.after {
				t.Fatalf("SHA = %q, want %q", in.SHA, tt.after)
			}
		})
	}
}

func TestBuildGitHubEventInput_CheckRun_NormalizesNameConclusionAndBranch(t *testing.T) {
	body := []byte(`{
		"action": "completed",
		"repository": {"full_name": "acme/repo"},
		"check_run": {
			"name": "ci/lint",
			"conclusion": "success",
			"head_sha": "shaCheck",
			"check_suite": {"head_branch": "main"}
		}
	}`)
	in, ok := buildGitHubEventInput("check_run", body)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if in.Name != "ci/lint" {
		t.Fatalf("Name = %q, want %q", in.Name, "ci/lint")
	}
	if in.Conclusion != "success" {
		t.Fatalf("Conclusion = %q, want %q", in.Conclusion, "success")
	}
	if in.SHA != "shaCheck" {
		t.Fatalf("SHA = %q, want %q", in.SHA, "shaCheck")
	}
	if len(in.Branches) != 1 || in.Branches[0].Name != "main" || in.Branches[0].HeadSHA != "shaCheck" {
		t.Fatalf("Branches = %v, want one {main, shaCheck}", in.Branches)
	}
}

func TestBuildGitHubEventInput_Status_PreservesBranchesContainmentVerbatim(t *testing.T) {
	body := []byte(`{
		"repository": {"full_name": "acme/repo"},
		"sha": "shaMain",
		"state": "success",
		"context": "ci/build",
		"branches": [
			{"name": "main", "commit": {"sha": "shaMain"}},
			{"name": "feature-x", "commit": {"sha": "shaFeatureTip"}}
		]
	}`)
	in, ok := buildGitHubEventInput("status", body)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if in.Name != "ci/build" {
		t.Fatalf("Name = %q, want %q", in.Name, "ci/build")
	}
	if in.Conclusion != "success" {
		t.Fatalf("Conclusion = %q, want %q", in.Conclusion, "success")
	}
	if in.SHA != "shaMain" {
		t.Fatalf("SHA = %q, want %q", in.SHA, "shaMain")
	}
	want := []domainautomation.GitHubEventBranch{
		{Name: "main", HeadSHA: "shaMain"},
		{Name: "feature-x", HeadSHA: "shaFeatureTip"},
	}
	if len(in.Branches) != len(want) {
		t.Fatalf("Branches = %v, want %v", in.Branches, want)
	}
	for i := range want {
		if in.Branches[i] != want[i] {
			t.Fatalf("Branches[%d] = %v, want %v", i, in.Branches[i], want[i])
		}
	}
}
