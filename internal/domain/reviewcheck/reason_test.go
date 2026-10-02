package reviewcheck

import (
	"strings"
	"testing"
)

func TestComputeOutputWithReason(t *testing.T) {
	phases := []Phase{PhaseQueued, PhaseRunning, PhaseStale, PhaseTerminalAssessed, PhaseTerminalNotAssessed, Phase("bogus")}
	reasons := []NotAssessedReason{"", NotAssessedPersonalLinkOnly, NotAssessedRolloutNotEnrolled, NotAssessedSubstrateUnsupported, NotAssessedPromptNotDelivered, NotAssessedRepoEntitlementRevoked, NotAssessedReason("a_reason_this_binary_does_not_know")}
	for _, p := range phases {
		for _, r := range reasons {
			t.Run(string(p)+"/"+string(r), func(t *testing.T) {
				base := ComputeOutput(p)
				got := ComputeOutputWithReason(p, r)
				if got.Status != base.Status || got.Conclusion != base.Conclusion || got.Title != base.Title {
					t.Errorf("status/conclusion/title = %q/%q/%q, want ComputeOutput's %q/%q/%q", got.Status, got.Conclusion, got.Title, base.Status, base.Conclusion, base.Title)
				}
				if !strings.HasPrefix(got.Summary, base.Summary) {
					t.Errorf("summary = %q, want it to begin with ComputeOutput's %q", got.Summary, base.Summary)
				}
				extended := got.Summary != base.Summary
				named := r == NotAssessedPersonalLinkOnly || r == NotAssessedRolloutNotEnrolled || r == NotAssessedSubstrateUnsupported || r == NotAssessedPromptNotDelivered || r == NotAssessedRepoEntitlementRevoked
				wantExtended := p == PhaseTerminalNotAssessed && named
				if extended != wantExtended {
					t.Errorf("summary extended = %v, want %v (summary %q)", extended, wantExtended, got.Summary)
				}
			})
		}
	}
}

func TestComputeOutputWithReason_PersonalLinkOnlyNamesTheCause(t *testing.T) {
	got := ComputeOutputWithReason(PhaseTerminalNotAssessed, NotAssessedPersonalLinkOnly)
	for _, want := range []string{"person's own provider link", "never runs on one", "deployment credential"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("summary = %q, want it to contain %q", got.Summary, want)
		}
	}
}

// TestComputeOutputWithReason_SpawnRefusalsNameTheCauseAndTheRemedy pins
// the sentences the control plane's own refusals and failed deliveries add
// to the check: what stopped the review, and what can be done.
func TestComputeOutputWithReason_SpawnRefusalsNameTheCauseAndTheRemedy(t *testing.T) {
	for _, tc := range []struct {
		reason NotAssessedReason
		want   []string
	}{
		{NotAssessedRolloutNotEnrolled, []string{"not enrolled", "enroll the repository", "request the review again"}},
		{NotAssessedSubstrateUnsupported, []string{"cannot give this session's environment", "change the provider or the environment", "request the review again"}},
		{NotAssessedPromptNotDelivered, []string{"could not be delivered", "did not run", "Requesting the review again"}},
	} {
		got := ComputeOutputWithReason(PhaseTerminalNotAssessed, tc.reason)
		for _, want := range tc.want {
			if !strings.Contains(got.Summary, want) {
				t.Errorf("%s: summary = %q, want it to contain %q", tc.reason, got.Summary, want)
			}
		}
	}
}

// TestReasonExplanation_RepoEntitlementRevoked pins the sentence a review
// check closed by an administrator's revocation adds (technical plan
// §31.4): who stopped the review, and what lifts it. It never reads as an
// installation or configuration problem.
func TestReasonExplanation_RepoEntitlementRevoked(t *testing.T) {
	got := ComputeOutputWithReason(PhaseTerminalNotAssessed, NotAssessedRepoEntitlementRevoked)
	for _, want := range []string{"An administrator of this deployment revoked new work on its repository", "the review was not run", "An administrator can restore the repository, then request the review again."} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("summary = %q, want it to contain %q", got.Summary, want)
		}
	}
	for _, banned := range []string{"installed", "configured", "access"} {
		if strings.Contains(strings.ToLower(got.Summary), banned) {
			t.Errorf("summary = %q, must not say %q", got.Summary, banned)
		}
	}
}
