package reviewpost_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

// deepInputWithFindings is validInput on the deep path, with the digest
// fields the deep path requires, a counter-review reported done, and
// findings.
func deepInputWithFindings(findings ...reviewpost.FindingInput) reviewpost.VerdictInput {
	in := validInput()
	in.ReviewDepth = reviewtriage.DepthDeep
	in.Digest.ArchDecisions = []reviewpost.ArchDecision{{Decision: "Reused the retry helper."}}
	in.Digest.StackRisks = "None of note."
	in.Digest.UnverifiedLimits = "Did not run against production traffic."
	in.CounterReview = review.CounterReviewDone
	in.Findings = findings
	return in
}

func primaryFinding(description string) reviewpost.FindingInput {
	return reviewpost.FindingInput{Severity: review.RiskLevelMedium, FilePath: "internal/retry/retry.go", Description: description, Source: reviewpost.FindingSourcePrimary}
}

func counterReviewFinding(description string) reviewpost.FindingInput {
	return reviewpost.FindingInput{Severity: review.RiskLevelHigh, FilePath: "internal/retry/retry.go", Description: description, Source: reviewpost.FindingSourceCounterReview}
}

func TestResolveAdditionCheck(t *testing.T) {
	tests := []struct {
		name     string
		reported reviewpost.FactCheckStatus
		trace    reviewpost.AdditionsTrace
		want     reviewpost.AdditionCheck
	}{
		{"reported done, run found in the trace", reviewpost.FactCheckDone, reviewpost.AdditionsTraceRunFound, reviewpost.AdditionChecked},
		{"reported done, trace read in full shows none", reviewpost.FactCheckDone, reviewpost.AdditionsTraceNoRunFound, reviewpost.AdditionNotFound},
		{"reported done, trace not read in full", reviewpost.FactCheckDone, reviewpost.AdditionsTraceUnread, reviewpost.AdditionUnconfirmed},
		{"reported done, unknown trace value", reviewpost.FactCheckDone, "garbled", reviewpost.AdditionUnconfirmed},
		{"reported skipped, even with a run in the trace", reviewpost.FactCheckSkipped, reviewpost.AdditionsTraceRunFound, reviewpost.AdditionNotRun},
		{"not reported, even with a run in the trace", "", reviewpost.AdditionsTraceRunFound, reviewpost.AdditionNotRun},
		{"not reported, trace unread", "", reviewpost.AdditionsTraceUnread, reviewpost.AdditionNotRun},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewpost.ResolveAdditionCheck(tt.reported, tt.trace); got != tt.want {
				t.Errorf("ResolveAdditionCheck(%q, %q) = %q, want %q", tt.reported, tt.trace, got, tt.want)
			}
		})
	}
}

func TestAdditionCheck_OnlyCheckedCounts(t *testing.T) {
	tests := []struct {
		check reviewpost.AdditionCheck
		want  bool
	}{
		{reviewpost.AdditionChecked, true},
		{reviewpost.AdditionNotRun, false},
		{reviewpost.AdditionNotFound, false},
		{reviewpost.AdditionUnconfirmed, false},
		{"", false},
		{"CHECKED", false},
	}
	for _, tt := range tests {
		if got := tt.check.Checked(); got != tt.want {
			t.Errorf("AdditionCheck(%q).Checked() = %v, want %v", tt.check, got, tt.want)
		}
	}
}

func TestFinding_UnverifiedAddition(t *testing.T) {
	tests := []struct {
		name string
		f    reviewpost.Finding
		want bool
	}{
		{"primary", reviewpost.Finding{Source: reviewpost.FindingSourcePrimary}, false},
		{"no source recorded", reviewpost.Finding{}, false},
		{"checked addition", reviewpost.Finding{Source: reviewpost.FindingSourceCounterReview, AdditionCheck: reviewpost.AdditionChecked}, false},
		{"addition not run", reviewpost.Finding{Source: reviewpost.FindingSourceCounterReview, AdditionCheck: reviewpost.AdditionNotRun}, true},
		{"addition not found", reviewpost.Finding{Source: reviewpost.FindingSourceCounterReview, AdditionCheck: reviewpost.AdditionNotFound}, true},
		{"addition unconfirmed", reviewpost.Finding{Source: reviewpost.FindingSourceCounterReview, AdditionCheck: reviewpost.AdditionUnconfirmed}, true},
		{"addition with no resolution", reviewpost.Finding{Source: reviewpost.FindingSourceCounterReview}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.UnverifiedAddition(); got != tt.want {
				t.Errorf("UnverifiedAddition() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildSecondFactCheck(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(in *reviewpost.VerdictInput)
		want   reviewpost.SecondFactCheck
	}{
		{
			name:   "light path: nothing to record",
			mutate: func(in *reviewpost.VerdictInput) { in.ReviewDepth = reviewtriage.DepthLight },
			want:   reviewpost.SecondFactCheck{},
		},
		{
			name:   "deep, no additions, no second run reported: nothing to record",
			mutate: func(in *reviewpost.VerdictInput) { in.Findings = []reviewpost.FindingInput{primaryFinding("p")} },
			want:   reviewpost.SecondFactCheck{},
		},
		{
			name:   "deep, additions, no second run reported: not run",
			mutate: func(_ *reviewpost.VerdictInput) {},
			want:   reviewpost.SecondFactCheck{Resolved: reviewpost.AdditionNotRun},
		},
		{
			name: "deep, additions, reported done and found after the counter-review: checked",
			mutate: func(in *reviewpost.VerdictInput) {
				in.AdditionsFactCheck, in.AdditionsFactCheckKilled, in.AdditionsTrace = reviewpost.FactCheckDone, 1, reviewpost.AdditionsTraceRunFound
			},
			want: reviewpost.SecondFactCheck{Reported: reviewpost.FactCheckDone, ReportedKilled: 1, Resolved: reviewpost.AdditionChecked},
		},
		{
			name: "deep, additions, reported done but not in the trace: not found",
			mutate: func(in *reviewpost.VerdictInput) {
				in.AdditionsFactCheck, in.AdditionsTrace = reviewpost.FactCheckDone, reviewpost.AdditionsTraceNoRunFound
			},
			want: reviewpost.SecondFactCheck{Reported: reviewpost.FactCheckDone, Resolved: reviewpost.AdditionNotFound},
		},
		{
			name:   "deep, additions, reported done, trace unread: unconfirmed",
			mutate: func(in *reviewpost.VerdictInput) { in.AdditionsFactCheck = reviewpost.FactCheckDone },
			want:   reviewpost.SecondFactCheck{Reported: reviewpost.FactCheckDone, Resolved: reviewpost.AdditionUnconfirmed},
		},
		{
			name:   "deep, additions, reported skipped (cost budget): not run",
			mutate: func(in *reviewpost.VerdictInput) { in.AdditionsFactCheck = reviewpost.FactCheckSkipped },
			want:   reviewpost.SecondFactCheck{Reported: reviewpost.FactCheckSkipped, Resolved: reviewpost.AdditionNotRun},
		},
		{
			name: "deep, the second run killed every addition: its outcome is still recorded",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Findings = []reviewpost.FindingInput{primaryFinding("p")}
				in.AdditionsFactCheck, in.AdditionsFactCheckKilled, in.AdditionsTrace = reviewpost.FactCheckDone, 2, reviewpost.AdditionsTraceRunFound
			},
			want: reviewpost.SecondFactCheck{Reported: reviewpost.FactCheckDone, ReportedKilled: 2, Resolved: reviewpost.AdditionChecked},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := deepInputWithFindings(primaryFinding("p"), counterReviewFinding("c"))
			tt.mutate(&in)
			if got := reviewpost.BuildSecondFactCheck(in); got != tt.want {
				t.Errorf("BuildSecondFactCheck() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestBuildFindings_RecordsSourceAndMarksAdditions: every finding carries
// its source, and every counter-review addition -- only those -- carries
// the verdict's one resolution.
func TestBuildFindings_RecordsSourceAndMarksAdditions(t *testing.T) {
	for _, trace := range []reviewpost.AdditionsTrace{reviewpost.AdditionsTraceRunFound, reviewpost.AdditionsTraceNoRunFound, reviewpost.AdditionsTraceUnread} {
		t.Run(string(trace), func(t *testing.T) {
			in := deepInputWithFindings(primaryFinding("p1"), counterReviewFinding("c1"), primaryFinding("p2"), counterReviewFinding("c2"))
			in.AdditionsFactCheck, in.AdditionsTrace = reviewpost.FactCheckDone, trace
			want := reviewpost.BuildSecondFactCheck(in).Resolved

			got := reviewpost.BuildFindings(in)
			if len(got) != 4 {
				t.Fatalf("BuildFindings() returned %d findings, want 4", len(got))
			}
			for i, f := range got {
				if f.Source != in.Findings[i].Source {
					t.Errorf("finding %d Source = %q, want %q", i, f.Source, in.Findings[i].Source)
				}
				wantCheck := reviewpost.AdditionCheck("")
				if f.Source == reviewpost.FindingSourceCounterReview {
					wantCheck = want
				}
				if f.AdditionCheck != wantCheck {
					t.Errorf("finding %d (%s) AdditionCheck = %q, want %q", i, f.Source, f.AdditionCheck, wantCheck)
				}
			}
		})
	}
}

func TestValidateVerdictInput_FindingSourceAndSecondRun(t *testing.T) {
	tests := []struct {
		name    string
		in      func() reviewpost.VerdictInput
		wantErr error
	}{
		{"light, primary finding", func() reviewpost.VerdictInput {
			in := validInput()
			in.Findings = []reviewpost.FindingInput{primaryFinding("p")}
			return in
		}, nil},
		{"deep, primary and counter-review findings, second run done", func() reviewpost.VerdictInput {
			in := deepInputWithFindings(primaryFinding("p"), counterReviewFinding("c"))
			in.AdditionsFactCheck, in.AdditionsFactCheckKilled = reviewpost.FactCheckDone, 3
			return in
		}, nil},
		{"deep, additions with no second run reported", func() reviewpost.VerdictInput {
			return deepInputWithFindings(counterReviewFinding("c"))
		}, nil},
		{"deep, additions beside a counter-review reported skipped (admitted, resolved unverified)", func() reviewpost.VerdictInput {
			in := deepInputWithFindings(counterReviewFinding("c"))
			in.CounterReview = review.CounterReviewSkipped
			return in
		}, nil},
		{"finding with no source", func() reviewpost.VerdictInput {
			in := validInput()
			f := primaryFinding("p")
			f.Source = ""
			in.Findings = []reviewpost.FindingInput{f}
			return in
		}, reviewpost.ErrInvalidFindingSource},
		{"finding with a garbled source", func() reviewpost.VerdictInput {
			in := deepInputWithFindings(primaryFinding("p"))
			in.Findings[0].Source = "scribe"
			return in
		}, reviewpost.ErrInvalidFindingSource},
		{"counter-review finding on the light path", func() reviewpost.VerdictInput {
			in := validInput()
			in.ReviewDepth = reviewtriage.DepthLight
			in.Findings = []reviewpost.FindingInput{counterReviewFinding("c")}
			return in
		}, reviewpost.ErrCounterReviewAdditionOffDeepPath},
		{"counter-review finding with no resolved depth", func() reviewpost.VerdictInput {
			in := validInput()
			in.Findings = []reviewpost.FindingInput{counterReviewFinding("c")}
			return in
		}, reviewpost.ErrCounterReviewAdditionOffDeepPath},
		{"second run reported on the light path", func() reviewpost.VerdictInput {
			in := validInput()
			in.ReviewDepth = reviewtriage.DepthLight
			in.AdditionsFactCheck = reviewpost.FactCheckSkipped
			return in
		}, reviewpost.ErrCounterReviewAdditionOffDeepPath},
		{"garbled second run", func() reviewpost.VerdictInput {
			in := deepInputWithFindings(counterReviewFinding("c"))
			in.AdditionsFactCheck = "partial"
			return in
		}, reviewpost.ErrInvalidAdditionsFactCheck},
		{"negative second-run kill count", func() reviewpost.VerdictInput {
			in := deepInputWithFindings(counterReviewFinding("c"))
			in.AdditionsFactCheck, in.AdditionsFactCheckKilled = reviewpost.FactCheckDone, -1
			return in
		}, reviewpost.ErrNegativeAdditionsFactCheckKilled},
		{"kills reported for a skipped second run", func() reviewpost.VerdictInput {
			in := deepInputWithFindings(counterReviewFinding("c"))
			in.AdditionsFactCheck, in.AdditionsFactCheckKilled = reviewpost.FactCheckSkipped, 1
			return in
		}, reviewpost.ErrAdditionsFactCheckKilledWithoutRun},
		{"kills reported with no second run", func() reviewpost.VerdictInput {
			in := deepInputWithFindings(counterReviewFinding("c"))
			in.AdditionsFactCheckKilled = 1
			return in
		}, reviewpost.ErrAdditionsFactCheckKilledWithoutRun},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := reviewpost.ValidateVerdictInput(tt.in()); !errors.Is(err, tt.wantErr) {
				t.Errorf("ValidateVerdictInput() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateVerdictInput_FindingSourceCheckedAfterLengthCaps: the new
// checks run after every existing one, so a payload already failing a
// length cap keeps reporting it.
func TestValidateVerdictInput_FindingSourceCheckedAfterLengthCaps(t *testing.T) {
	in := validInput()
	in.Digest.Summary = strings.Repeat("a", reviewpost.MaxDigestSummaryBytes+1)
	f := primaryFinding("p")
	f.Source = ""
	in.Findings = []reviewpost.FindingInput{f}
	if err := reviewpost.ValidateVerdictInput(in); !errors.Is(err, reviewpost.ErrDigestSummaryTooLong) {
		t.Errorf("ValidateVerdictInput() = %v, want %v (length caps checked before finding sources)", err, reviewpost.ErrDigestSummaryTooLong)
	}
}

// TestBuildVerdict_UnverifiedAdditionsNeverMoveTheClass: §26.1 composes
// no finding into the Shippable class, and the plan does not add one, so
// whether additions are checked, unverified or absent, the class and its
// blockers are those of the same verdict with no additions at all.
func TestBuildVerdict_UnverifiedAdditionsNeverMoveTheClass(t *testing.T) {
	base := deepInputWithFindings(primaryFinding("p"))
	base.CounterReviewCorroborated = true
	_, wantAssessment := reviewpost.BuildVerdict(base)

	for _, trace := range []reviewpost.AdditionsTrace{reviewpost.AdditionsTraceRunFound, reviewpost.AdditionsTraceNoRunFound, reviewpost.AdditionsTraceUnread} {
		for _, reported := range []reviewpost.FactCheckStatus{"", reviewpost.FactCheckDone, reviewpost.FactCheckSkipped} {
			in := deepInputWithFindings(primaryFinding("p"), counterReviewFinding("c"))
			in.CounterReviewCorroborated = true
			in.AdditionsFactCheck, in.AdditionsTrace = reported, trace
			verdict, assessment := reviewpost.BuildVerdict(in)
			if verdict.Shippable != wantAssessment.Class() || assessment.Class() != wantAssessment.Class() || len(assessment.Blockers()) != len(wantAssessment.Blockers()) {
				t.Errorf("reported=%q trace=%q: class %q with %d blockers, want %q with %d", reported, trace, assessment.Class(), len(assessment.Blockers()), wantAssessment.Class(), len(wantAssessment.Blockers()))
			}
		}
	}
}

func renderFindingsAppendix(findings []reviewpost.Finding) string {
	v := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		Shippable:         review.ShippableAuto,
	}
	digest := reviewpost.Digest{Summary: "s", DescriptionAdequacy: review.DescriptionAdequacyOK, AdequacyExplanation: "ok"}
	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), findings, digest, "why", "narvi-bot", reviewpost.LabelLowRisk)
	start := strings.Index(got, "<details>")
	end := strings.Index(got, "</details>")
	return got[start:end]
}

// TestRenderVerdictComment_UnverifiedAdditionsListedApart pins the
// appendix: primary findings render as before, a checked addition stays
// among them marked as the counter-review's, and unverified additions sit
// under their own heading with their count and the server's reason --
// never among the findings.
func TestRenderVerdictComment_UnverifiedAdditionsListedApart(t *testing.T) {
	primary := reviewpost.Finding{Severity: review.RiskLevelMedium, FilePath: "a.go", Description: "Primary finding.", Source: reviewpost.FindingSourcePrimary}
	legacy := reviewpost.Finding{Severity: review.RiskLevelLow, FilePath: "b.go", Description: "No source recorded."}
	checked := reviewpost.Finding{Severity: review.RiskLevelHigh, FilePath: "c.go", Description: "Checked addition.", Source: reviewpost.FindingSourceCounterReview, AdditionCheck: reviewpost.AdditionChecked}
	notRun := reviewpost.Finding{Severity: review.RiskLevelHigh, FilePath: "d.go", Description: "Not run.", Source: reviewpost.FindingSourceCounterReview, AdditionCheck: reviewpost.AdditionNotRun}
	notFound := reviewpost.Finding{Severity: review.RiskLevelHigh, FilePath: "e.go", Description: "Not found.", Source: reviewpost.FindingSourceCounterReview, AdditionCheck: reviewpost.AdditionNotFound}
	unconfirmed := reviewpost.Finding{Severity: review.RiskLevelHigh, FilePath: "f.go", Description: "Unconfirmed.", Source: reviewpost.FindingSourceCounterReview, AdditionCheck: reviewpost.AdditionUnconfirmed}

	tests := []struct {
		name     string
		findings []reviewpost.Finding
		want     string
	}{
		{
			name:     "primary and no-source findings render as before",
			findings: []reviewpost.Finding{primary, legacy},
			want: "\n**Findings:**\n\n" +
				"- [general/medium] `a.go`: Primary finding.\n" +
				"- [general/low] `b.go`: No source recorded.\n",
		},
		{
			name:     "checked addition stays among the findings, marked",
			findings: []reviewpost.Finding{primary, checked},
			want: "\n**Findings:**\n\n" +
				"- [general/medium] `a.go`: Primary finding.\n" +
				"- [general/high] `c.go`: Checked addition. _(added by the counter-review; fact-checked after it)_\n",
		},
		{
			name:     "unverified additions listed apart with their count and reason",
			findings: []reviewpost.Finding{notRun, primary, notFound, unconfirmed},
			want: "\n**Findings:**\n\n" +
				"- [general/medium] `a.go`: Primary finding.\n" +
				"\n**Unverified -- added by the counter-review and not fact-checked (3, counted apart from the findings):**\n\n" +
				"- [general/high] `d.go`: Not run. _(unverified: no fact-check run over it after the counter-review was reported)_\n" +
				"- [general/high] `e.go`: Not found. _(unverified: a fact-check run over it was reported, but none that started after the counter-review and completed was found in this turn's trace when the verdict was posted)_\n" +
				"- [general/high] `f.go`: Unconfirmed. _(unverified: whether a fact-check ran over it could not be confirmed: this turn's trace could not be read in full when the verdict was posted)_\n",
		},
		{
			name:     "only unverified additions: no findings list at all",
			findings: []reviewpost.Finding{notRun},
			want: "\n**Unverified -- added by the counter-review and not fact-checked (1, counted apart from the findings):**\n\n" +
				"- [general/high] `d.go`: Not run. _(unverified: no fact-check run over it after the counter-review was reported)_\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderFindingsAppendix(tt.findings)
			head := "- **Files changed**: 0\n"
			idx := strings.Index(got, head)
			if idx < 0 {
				t.Fatalf("appendix missing %q:\n%s", head, got)
			}
			if body := got[idx+len(head):]; body != tt.want+"\n" {
				t.Errorf("appendix findings =\n%q\nwant\n%q", body, tt.want+"\n")
			}
		})
	}
}

// TestRenderVerdictComment_UnconfirmedNeverClaimsWhatTheTraceShows: a
// trace not read in full yields "could not be confirmed", never a claim
// about what the trace holds.
func TestRenderVerdictComment_UnconfirmedNeverClaimsWhatTheTraceShows(t *testing.T) {
	got := renderFindingsAppendix([]reviewpost.Finding{{Severity: review.RiskLevelHigh, FilePath: "f.go", Description: "d", Source: reviewpost.FindingSourceCounterReview, AdditionCheck: reviewpost.AdditionUnconfirmed}})
	if !strings.Contains(got, "could not be confirmed") {
		t.Errorf("unconfirmed addition does not say it could not be confirmed:\n%s", got)
	}
	for _, claim := range []string{"shows no", "shows none", "was found in", "no fact-check run over it after"} {
		if strings.Contains(got, claim) {
			t.Errorf("unconfirmed addition claims %q about the trace:\n%s", claim, got)
		}
	}
}
