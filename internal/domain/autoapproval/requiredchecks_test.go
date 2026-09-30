package autoapproval_test

import (
	"reflect"
	"testing"

	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
)

// reasonBuildHasNotReported is the Reason ComputeEligible gives when the
// base requires "build" and nothing named "build" has reported at the head.
const reasonBuildHasNotReported = autoapproval.Reason(`required check "build" has not reported at the current head`)

func withRequiredChecks(in autoapproval.EligibilityInput, r autoapproval.RequiredChecks) autoapproval.EligibilityInput {
	in.RequiredChecks = r
	return in
}

func checkRun(name string, appID int64, state autoapproval.CheckState) autoapproval.HeadCheck {
	return autoapproval.HeadCheck{Name: name, Source: autoapproval.CheckSourceCheckRun, AppID: appID, State: state}
}

func status(name string, state autoapproval.CheckState) autoapproval.HeadCheck {
	return autoapproval.HeadCheck{Name: name, Source: autoapproval.CheckSourceStatus, State: state}
}

func required(name string, appID int64) autoapproval.RequiredCheck {
	return autoapproval.RequiredCheck{Name: name, AppID: appID}
}

func shortfall(name string, appID int64, kind autoapproval.ShortfallKind) autoapproval.RequiredCheckShortfall {
	return autoapproval.RequiredCheckShortfall{Check: required(name, appID), Kind: kind}
}

const (
	passed  = autoapproval.CheckStatePassed
	pending = autoapproval.CheckStatePending
	failed  = autoapproval.CheckStateFailed
)

func TestEvaluateRequiredChecks(t *testing.T) {
	t.Parallel()

	const ciApp, otherApp = int64(15368), int64(99)

	tests := []struct {
		name         string
		required     []autoapproval.RequiredCheck
		head         []autoapproval.HeadCheck
		headComplete bool
		want         []autoapproval.RequiredCheckShortfall
	}{
		{
			name:         "a base requiring nothing has no shortfall, whatever the head says",
			head:         []autoapproval.HeadCheck{checkRun("lint", 0, failed)},
			headComplete: true,
		},
		{
			name:         "a required check with no report at all is missing",
			required:     []autoapproval.RequiredCheck{required("build", 0)},
			head:         []autoapproval.HeadCheck{checkRun("lint", 0, passed)},
			headComplete: true,
			want:         []autoapproval.RequiredCheckShortfall{shortfall("build", 0, autoapproval.ShortfallMissing)},
		},
		{
			name:         "a passing check run satisfies a check naming no App",
			required:     []autoapproval.RequiredCheck{required("build", 0)},
			head:         []autoapproval.HeadCheck{checkRun("build", ciApp, passed)},
			headComplete: true,
		},
		{
			name:         "a passing commit status satisfies a check naming no App",
			required:     []autoapproval.RequiredCheck{required("build", 0)},
			head:         []autoapproval.HeadCheck{status("build", passed)},
			headComplete: true,
		},
		{
			name:         "a failing check run beside a passing status of the same name fails: both must pass",
			required:     []autoapproval.RequiredCheck{required("build", 0)},
			head:         []autoapproval.HeadCheck{checkRun("build", ciApp, failed), status("build", passed)},
			headComplete: true,
			want:         []autoapproval.RequiredCheckShortfall{shortfall("build", 0, autoapproval.ShortfallFailed)},
		},
		{
			name:         "a passing check run beside a failing status of the same name fails: both must pass",
			required:     []autoapproval.RequiredCheck{required("build", 0)},
			head:         []autoapproval.HeadCheck{status("build", failed), checkRun("build", ciApp, passed)},
			headComplete: true,
			want:         []autoapproval.RequiredCheckShortfall{shortfall("build", 0, autoapproval.ShortfallFailed)},
		},
		{
			name:         "a passing check run beside a pending status of the same name is still running",
			required:     []autoapproval.RequiredCheck{required("build", 0)},
			head:         []autoapproval.HeadCheck{checkRun("build", ciApp, passed), status("build", pending)},
			headComplete: true,
			want:         []autoapproval.RequiredCheckShortfall{shortfall("build", 0, autoapproval.ShortfallPending)},
		},
		{
			name:         "a failed report outranks a pending one",
			required:     []autoapproval.RequiredCheck{required("build", 0)},
			head:         []autoapproval.HeadCheck{checkRun("build", ciApp, pending), status("build", failed)},
			headComplete: true,
			want:         []autoapproval.RequiredCheckShortfall{shortfall("build", 0, autoapproval.ShortfallFailed)},
		},
		{
			name:         "an unrecognised state never passes",
			required:     []autoapproval.RequiredCheck{required("build", 0)},
			head:         []autoapproval.HeadCheck{checkRun("build", ciApp, autoapproval.CheckState("mystery"))},
			headComplete: true,
			want:         []autoapproval.RequiredCheckShortfall{shortfall("build", 0, autoapproval.ShortfallFailed)},
		},
		{
			name:         "a check the base ties to one App, reported only by another App, is missing",
			required:     []autoapproval.RequiredCheck{required("build", ciApp)},
			head:         []autoapproval.HeadCheck{checkRun("build", otherApp, passed)},
			headComplete: true,
			want:         []autoapproval.RequiredCheckShortfall{shortfall("build", ciApp, autoapproval.ShortfallMissing)},
		},
		{
			name:         "a check the base ties to one App is not satisfied by a commit status of its name",
			required:     []autoapproval.RequiredCheck{required("build", ciApp)},
			head:         []autoapproval.HeadCheck{status("build", passed)},
			headComplete: true,
			want:         []autoapproval.RequiredCheckShortfall{shortfall("build", ciApp, autoapproval.ShortfallMissing)},
		},
		{
			name:         "a check the base ties to one App is satisfied by that App's passing check run",
			required:     []autoapproval.RequiredCheck{required("build", ciApp)},
			head:         []autoapproval.HeadCheck{checkRun("build", ciApp, passed)},
			headComplete: true,
		},
		{
			// GitHub counts only the named App's report for the
			// requirement; the other App's failure is the CI read's to
			// refuse on (ComputeEligible's CIGreen), not this rule's.
			name:         "only the named App's report decides a check tied to it",
			required:     []autoapproval.RequiredCheck{required("build", ciApp)},
			head:         []autoapproval.HeadCheck{checkRun("build", ciApp, passed), checkRun("build", otherApp, failed)},
			headComplete: true,
		},
		{
			name:         "narvi/review is taken out of the required set: absent at the head, it is no shortfall",
			required:     []autoapproval.RequiredCheck{required(reviewcheck.CheckName, 0)},
			headComplete: true,
		},
		{
			name:         "narvi/review is taken out of the required set: failing at the head, it is no shortfall",
			required:     []autoapproval.RequiredCheck{required(reviewcheck.CheckName, ciApp), required("build", 0)},
			head:         []autoapproval.HeadCheck{checkRun(reviewcheck.CheckName, ciApp, failed), checkRun("build", 0, passed)},
			headComplete: true,
		},
		{
			name:         "a check naming no App, not seen while the head was not read in full, is unconfirmed",
			required:     []autoapproval.RequiredCheck{required("build", 0)},
			head:         []autoapproval.HeadCheck{checkRun("lint", 0, passed)},
			headComplete: false,
			want:         []autoapproval.RequiredCheckShortfall{shortfall("build", 0, autoapproval.ShortfallUnconfirmed)},
		},
		{
			name:         "a check naming an App, not seen while the head was not read in full, is missing",
			required:     []autoapproval.RequiredCheck{required("build", ciApp)},
			headComplete: false,
			want:         []autoapproval.RequiredCheckShortfall{shortfall("build", ciApp, autoapproval.ShortfallMissing)},
		},
		{
			name:         "a check seen passing is satisfied even when the head was not read in full",
			required:     []autoapproval.RequiredCheck{required("build", 0)},
			head:         []autoapproval.HeadCheck{checkRun("build", 0, passed)},
			headComplete: false,
		},
		{
			name:         "a check both sources declare is reported once",
			required:     []autoapproval.RequiredCheck{required("build", 0), required("build", 0)},
			headComplete: true,
			want:         []autoapproval.RequiredCheckShortfall{shortfall("build", 0, autoapproval.ShortfallMissing)},
		},
		{
			name:         "a name required once with an App and once without keeps both requirements",
			required:     []autoapproval.RequiredCheck{required("build", ciApp), required("build", 0)},
			head:         []autoapproval.HeadCheck{status("build", passed)},
			headComplete: true,
			want:         []autoapproval.RequiredCheckShortfall{shortfall("build", ciApp, autoapproval.ShortfallMissing)},
		},
		{
			name:         "an empty required name is ignored",
			required:     []autoapproval.RequiredCheck{required("", 0)},
			headComplete: true,
		},
		{
			name:         "several shortfalls are reported sorted by name",
			required:     []autoapproval.RequiredCheck{required("test", 0), required("build", 0), required("lint", 0)},
			head:         []autoapproval.HeadCheck{checkRun("test", 0, pending), checkRun("lint", 0, passed)},
			headComplete: true,
			want: []autoapproval.RequiredCheckShortfall{
				shortfall("build", 0, autoapproval.ShortfallMissing),
				shortfall("test", 0, autoapproval.ShortfallPending),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := autoapproval.EvaluateRequiredChecks(tc.required, tc.head, tc.headComplete)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("EvaluateRequiredChecks() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestComputeEligible_RequiredChecks is the eligibility engine's side of
// §21.2's "CI green means the required checks" amendment: every refusal
// names the check, the required set never replaces the CI read, and a
// requirement that could not be read refuses.
func TestComputeEligible_RequiredChecks(t *testing.T) {
	t.Parallel()

	cfg := autoapproval.DefaultEligibilityConfig()
	read := autoapproval.ReadRequiredChecks

	tests := []struct {
		name         string
		in           autoapproval.EligibilityInput
		wantEligible bool
		wantReason   autoapproval.Reason
	}{
		{
			name:       "requirements that could not be read refuse, never fall back to the CI read",
			in:         withRequiredChecks(cleanInput(), autoapproval.RequiredChecks{}),
			wantReason: autoapproval.ReasonRequiredChecksUnknown,
		},
		{
			// CI reads green from the checks that did report: the gap the
			// amendment closes.
			name:       "a required check that has not reported refuses and is named, CI read green or not",
			in:         withRequiredChecks(cleanInput(), read([]autoapproval.RequiredCheck{{Name: "build"}}, []autoapproval.HeadCheck{checkRun("lint", 0, passed)}, true)),
			wantReason: reasonBuildHasNotReported,
		},
		{
			name: "a failing check beside a passing status of the same name refuses, naming the check",
			in: withCIGreen(withRequiredChecks(cleanInput(), read(
				[]autoapproval.RequiredCheck{{Name: "build"}},
				[]autoapproval.HeadCheck{checkRun("build", 0, failed), status("build", passed)}, true)), false),
			wantReason: `required check "build" did not pass at the current head`,
		},
		{
			name: "a required check satisfied by another App than the one named does not count",
			in: withRequiredChecks(cleanInput(), read(
				[]autoapproval.RequiredCheck{{Name: "build", AppID: 15368}},
				[]autoapproval.HeadCheck{checkRun("build", 99, passed)}, true)),
			wantReason: `required check "build" has not reported at the current head from the App the base branch names (App id 15368)`,
		},
		{
			name: "a required check still running refuses as running, not as CI not green",
			in: withCIGreen(withRequiredChecks(cleanInput(), read(
				[]autoapproval.RequiredCheck{{Name: "build"}},
				[]autoapproval.HeadCheck{checkRun("build", 0, pending)}, true)), false),
			wantReason: `required check "build" is still running at the current head`,
		},
		{
			name: "a required check not seen in a head read short of its statuses is unconfirmed",
			in: withRequiredChecks(cleanInput(), read(
				[]autoapproval.RequiredCheck{{Name: "build"}}, nil, false)),
			wantReason: `required check "build" could not be confirmed at the current head (its commit statuses were not all read)`,
		},
		{
			name: "every unmet required check is named",
			in: withRequiredChecks(cleanInput(), read(
				[]autoapproval.RequiredCheck{{Name: "test"}, {Name: "build"}},
				[]autoapproval.HeadCheck{checkRun("test", 0, failed)}, true)),
			wantReason: `required check "build" has not reported at the current head; required check "test" did not pass at the current head`,
		},
		{
			name:         "a base requiring narvi/review is not made ineligible by it",
			in:           withRequiredChecks(cleanInput(), read([]autoapproval.RequiredCheck{{Name: reviewcheck.CheckName}}, nil, true)),
			wantEligible: true,
			wantReason:   autoapproval.ReasonNone,
		},
		{
			name: "every required check satisfied, a failing check the base does not require still refuses",
			in: withCIGreen(withRequiredChecks(cleanInput(), read(
				[]autoapproval.RequiredCheck{{Name: "build"}},
				[]autoapproval.HeadCheck{checkRun("build", 0, passed), checkRun("security-scan", 0, failed)}, true)), false),
			wantReason: autoapproval.ReasonCINotGreen,
		},
		{
			name: "every required check satisfied and CI green is eligible",
			in: withRequiredChecks(cleanInput(), read(
				[]autoapproval.RequiredCheck{{Name: "build", AppID: 15368}, {Name: "deploy/preview"}},
				[]autoapproval.HeadCheck{checkRun("build", 15368, passed), status("deploy/preview", passed)}, true)),
			wantEligible: true,
			wantReason:   autoapproval.ReasonNone,
		},
		{
			// A half-read head can make a required check look missing: the
			// degraded CI read speaks first.
			name:       "a degraded CI read is reported before required checks",
			in:         withCIConclusionDegraded(withRequiredChecks(cleanInput(), read([]autoapproval.RequiredCheck{{Name: "build"}}, nil, false)), true),
			wantReason: autoapproval.ReasonCIConclusionDegraded,
		},
		{
			name:       "freshness is reported before required checks",
			in:         withCurrentHeadSHA(withRequiredChecks(cleanInput(), autoapproval.RequiredChecks{}), "moved"),
			wantReason: autoapproval.ReasonStaleVerdict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotEligible, gotReason := autoapproval.ComputeEligible(tc.in, cfg)
			if gotEligible != tc.wantEligible || gotReason != tc.wantReason {
				t.Errorf("ComputeEligible() = (%v, %q), want (%v, %q)", gotEligible, gotReason, tc.wantEligible, tc.wantReason)
			}
		})
	}
}

// TestComputeEligible_BaseRequiringNothingKeepsTodaysAnswer runs the whole
// existing eligibility corpus (TestComputeEligible's table) under every
// shape of "the base requires nothing" -- nothing declared, or only
// narvi/review, which is taken out -- with the head's checks listed or
// not, read in full or not, and asserts each case's answer is exactly the
// one it had before required checks existed.
func TestComputeEligible_BaseRequiringNothingKeepsTodaysAnswer(t *testing.T) {
	t.Parallel()

	head := []autoapproval.HeadCheck{checkRun("build", 1, passed), status("deploy/preview", failed), checkRun(reviewcheck.CheckName, 2, pending)}
	requiresNothing := map[string]autoapproval.RequiredChecks{
		"nothing declared, no checks listed":                  autoapproval.ReadRequiredChecks(nil, nil, true),
		"nothing declared, head listed":                       autoapproval.ReadRequiredChecks(nil, head, true),
		"nothing declared, head not read in full":             autoapproval.ReadRequiredChecks(nil, head, false),
		"only narvi/review declared, no checks listed":        autoapproval.ReadRequiredChecks([]autoapproval.RequiredCheck{{Name: reviewcheck.CheckName}}, nil, true),
		"only narvi/review declared, from an App, head short": autoapproval.ReadRequiredChecks([]autoapproval.RequiredCheck{{Name: reviewcheck.CheckName, AppID: 2}}, head, false),
	}

	for _, tc := range computeEligibleCases() {
		for shape, r := range requiresNothing {
			t.Run(tc.name+"/"+shape, func(t *testing.T) {
				t.Parallel()
				gotEligible, gotReason := autoapproval.ComputeEligible(withRequiredChecks(tc.in, r), tc.cfg)
				if gotEligible != tc.wantEligible || gotReason != tc.wantReason {
					t.Errorf("ComputeEligible() = (%v, %q), want today's (%v, %q)", gotEligible, gotReason, tc.wantEligible, tc.wantReason)
				}
			})
		}
	}
}

// TestComputeEligibleWithAcceptance_RequiredChecksStayMandatory pins that
// an acceptance never waives a required check: CI green at the current head
// is one of §21.1b's conditions that are not about judgment.
func TestComputeEligibleWithAcceptance_RequiredChecksStayMandatory(t *testing.T) {
	t.Parallel()

	notAuto := withVerdict(cleanInput(), func(v review.Verdict) review.Verdict { v.Shippable = review.ShippableNeedsHuman; return v })
	cfg := autoapproval.DefaultEligibilityConfig()
	for name, r := range map[string]autoapproval.RequiredChecks{
		"requirements unread":     {},
		"required check missing":  autoapproval.ReadRequiredChecks([]autoapproval.RequiredCheck{{Name: "build"}}, nil, true),
		"required check failing":  autoapproval.ReadRequiredChecks([]autoapproval.RequiredCheck{{Name: "build"}}, []autoapproval.HeadCheck{checkRun("build", 0, failed)}, true),
		"required check from App": autoapproval.ReadRequiredChecks([]autoapproval.RequiredCheck{{Name: "build", AppID: 7}}, []autoapproval.HeadCheck{checkRun("build", 8, passed)}, true),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			eligible, reason, via := autoapproval.ComputeEligibleWithAcceptance(withRequiredChecks(notAuto, r), cfg, true)
			if eligible || via || reason == autoapproval.ReasonNotShippableAuto {
				t.Errorf("ComputeEligibleWithAcceptance(accepted=true) = (%v, %q, %v), want a refusal on the required check", eligible, reason, via)
			}
		})
	}
}
