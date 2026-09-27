package reviewfreshness_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewfreshness"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/platform"
)

// freshRecord is an assessed verdict for acme/widgets#7 at head h1 on
// main at b1, under the current policy.
func freshRecord() reviewverdict.Record {
	return reviewverdict.Record{
		ID: "v1", AttemptID: "a1", HeadSHA: "h1",
		Context: reviewverdict.Context{BaseRef: "main", BaseSHA: "b1", PolicyVersion: autoapproval.CurrentPolicyVersion},
	}
}

// openAt answers GetOpenPR with the pull request at head, based on main.
func openAt(head string, chain ...ports.PRAncestorLink) ports.OpenPR {
	pr := target(chain...)
	pr.HeadSHA = head
	return pr
}

var pr7 = reviewfreshness.PullRequest{Owner: "acme", Repo: "widgets", Number: 7}

// TestAssess_Table pins every state Assess reports, the reason it gives,
// and which reads it needed: the pull request read alone for what the
// merge path's probe decides (the record, against the live head and base
// ref), none at all with no code host.
func TestAssess_Table(t *testing.T) {
	t.Parallel()
	failed := errors.New("code host unavailable")

	tests := []struct {
		name      string
		record    func(*reviewverdict.Record)
		sc        *fakeSourceControl
		nilSC     bool
		want      reviewfreshness.Assessment
		wantCalls int
	}{
		{
			name:      "policy bumped: stale, from the probe after the pull request read",
			record:    func(r *reviewverdict.Record) { r.Context.PolicyVersion = autoapproval.CurrentPolicyVersion - 1 },
			sc:        &fakeSourceControl{found: true, openPR: openAt("h1")},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateStale, Reason: string(autoapproval.ReasonPolicyVersionMismatch)},
			wantCalls: 1,
		},
		{
			name:      "no context recorded: unconfirmed, from the probe",
			record:    func(r *reviewverdict.Record) { r.Context = reviewverdict.Context{} },
			sc:        &fakeSourceControl{found: true, openPR: openAt("h1")},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: string(autoapproval.ReasonContextUnknown)},
			wantCalls: 1,
		},
		{
			// The merge path's probe compares the live head first: a verdict
			// that recorded no context and whose head moved is stale there,
			// and so here (finding P8).
			name:      "no context recorded and the head moved: stale, as the merge path says",
			record:    func(r *reviewverdict.Record) { r.Context = reviewverdict.Context{} },
			sc:        &fakeSourceControl{found: true, openPR: openAt("h2")},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateStale, Reason: string(autoapproval.ReasonStaleVerdict)},
			wantCalls: 1,
		},
		{
			name:      "recorded base commit unknown: unconfirmed, from the probe",
			record:    func(r *reviewverdict.Record) { r.Context.BaseSHA = "" },
			sc:        &fakeSourceControl{found: true, openPR: openAt("h1")},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: string(autoapproval.ReasonBaseSHAUnknown)},
			wantCalls: 1,
		},
		{
			name:      "recorded base commit unknown and the head moved: stale",
			record:    func(r *reviewverdict.Record) { r.Context.BaseSHA = "" },
			sc:        &fakeSourceControl{found: true, openPR: openAt("h2")},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateStale, Reason: string(autoapproval.ReasonStaleVerdict)},
			wantCalls: 1,
		},
		{
			name: "recorded ancestor link unknown: unconfirmed, from the probe",
			record: func(r *reviewverdict.Record) {
				r.Context.AncestorChain = []review.AncestorLink{{Ref: "parent", SHA: ""}}
			},
			sc:        &fakeSourceControl{found: true, openPR: openAt("h1")},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: string(autoapproval.ReasonAncestorChainUnknown)},
			wantCalls: 1,
		},
		{
			// A retargeted base refuses before the chain is looked at, on the
			// merge path's probe as here.
			name: "recorded ancestor link unknown and the base retargeted: stale",
			record: func(r *reviewverdict.Record) {
				r.Context.AncestorChain = []review.AncestorLink{{Ref: "parent", SHA: ""}}
			},
			sc:        &fakeSourceControl{found: true, openPR: func() ports.OpenPR { p := openAt("h1"); p.BaseRef = "release"; return p }()},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateStale, Reason: string(autoapproval.ReasonBaseMoved)},
			wantCalls: 1,
		},
		{
			name:      "no head recorded: stale, from the probe",
			record:    func(r *reviewverdict.Record) { r.HeadSHA = "" },
			sc:        &fakeSourceControl{found: true, openPR: openAt("h1")},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateStale, Reason: string(autoapproval.ReasonStaleVerdict)},
			wantCalls: 1,
		},
		{
			name:      "a record the probe decides, on a pull request no longer open: not applicable",
			record:    func(r *reviewverdict.Record) { r.Context = reviewverdict.Context{} },
			sc:        &fakeSourceControl{found: false},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateNotApplicable, Reason: reviewfreshness.ReasonNoLongerOpen},
			wantCalls: 1,
		},
		{
			name:  "no code host configured: unconfirmed",
			nilSC: true,
			want:  reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: reviewfreshness.ReasonNoCodeHost},
		},
		{
			name:      "the pull request read fails: unconfirmed",
			sc:        &fakeSourceControl{openErr: failed},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: reviewfreshness.ReasonPullRequestUnread},
			wantCalls: 1,
		},
		{
			name:      "the pull request is no longer open: not applicable",
			sc:        &fakeSourceControl{found: false},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateNotApplicable, Reason: reviewfreshness.ReasonNoLongerOpen},
			wantCalls: 1,
		},
		{
			// The probe catches it: no base or chain read, as on the merge path.
			name:      "the head moved: stale, from the probe",
			sc:        &fakeSourceControl{found: true, openPR: openAt("h2"), branches: map[string]string{"main": "b1"}},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateStale, Reason: string(autoapproval.ReasonStaleVerdict)},
			wantCalls: 1,
		},
		{
			name:      "the base resolution fails: unconfirmed",
			sc:        &fakeSourceControl{found: true, openPR: openAt("h1"), branchErr: failed},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: "the base branch's current commit could not be read from the code host"},
			wantCalls: 2,
		},
		{
			name:      "the base moved forward: current",
			sc:        &fakeSourceControl{found: true, openPR: openAt("h1"), branches: map[string]string{"main": "b2"}, ancestry: map[string]bool{"b1..b2": true}},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateCurrent},
			wantCalls: 3,
		},
		{
			name:      "the base was rewritten: stale",
			sc:        &fakeSourceControl{found: true, openPR: openAt("h1"), branches: map[string]string{"main": "b2"}, ancestry: map[string]bool{}},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateStale, Reason: string(autoapproval.ReasonBaseMoved)},
			wantCalls: 3,
		},
		{
			name:      "the ancestry check fails: unconfirmed",
			sc:        &fakeSourceControl{found: true, openPR: openAt("h1"), branches: map[string]string{"main": "b2"}, ancestryErr: failed},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: "the base branch moved, and whether it only moved forward could not be confirmed with the code host"},
			wantCalls: 3,
		},
		{
			name:      "stacked since the verdict: stale",
			sc:        &fakeSourceControl{found: true, openPR: openAt("h1", prLink("parent", "")), branches: map[string]string{"main": "b1", "parent": "p1"}},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateStale, Reason: string(autoapproval.ReasonAncestorChainChanged)},
			wantCalls: 3,
		},
		{
			name:      "everything as recorded: current",
			sc:        &fakeSourceControl{found: true, openPR: openAt("h1"), branches: map[string]string{"main": "b1"}},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateCurrent},
			wantCalls: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			record := freshRecord()
			if tc.record != nil {
				tc.record(&record)
			}
			deps := reviewfreshness.Deps{Token: "bot-token", Timeouts: platform.DefaultTimeouts()}
			if !tc.nilSC {
				deps.SourceControl = tc.sc
			}
			got := reviewfreshness.Assess(context.Background(), deps, record, pr7)
			if got != tc.want {
				t.Errorf("Assess = %+v, want %+v", got, tc.want)
			}
			if tc.sc != nil {
				if calls := tc.sc.callList(); len(calls) != tc.wantCalls {
					t.Errorf("live calls = %v, want %d", calls, tc.wantCalls)
				}
			}
		})
	}
}

// TestAssess_PartialReadTimeoutIsUnconfirmed pins the bound on the pull
// request read: a read cut short by GitHubGetOpenPRTimeout reports
// unconfirmed, never trusting what the cut-short read left behind -- here,
// a pull request that would otherwise read current.
func TestAssess_PartialReadTimeoutIsUnconfirmed(t *testing.T) {
	t.Parallel()
	timeouts := platform.DefaultTimeouts()
	timeouts.GitHubGetOpenPRTimeout = 20 * time.Millisecond
	sc := &fakeSourceControl{found: true, openPR: openAt("h1"), branches: map[string]string{"main": "b1"}, openDelay: time.Second}
	got := reviewfreshness.Assess(context.Background(), reviewfreshness.Deps{SourceControl: sc, Token: "t", Timeouts: timeouts}, freshRecord(), pr7)
	want := reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: reviewfreshness.ReasonPullRequestUnread}
	if got != want {
		t.Fatalf("Assess = %+v, want %+v", got, want)
	}
	if calls := sc.callList(); len(calls) != 1 {
		t.Errorf("live calls = %v, want the one cut-short read and nothing after it", calls)
	}
}

// TestAssess_NeverCurrentWithoutLiveConfirmation injects a failure at every
// live step, and every record-only refusal, into a verdict that would
// otherwise read current: not one of them answers current.
func TestAssess_NeverCurrentWithoutLiveConfirmation(t *testing.T) {
	t.Parallel()
	failed := errors.New("code host unavailable")
	current := func() *fakeSourceControl {
		return &fakeSourceControl{found: true, openPR: openAt("h1", prLink("parent", "")),
			branches: map[string]string{"main": "b2", "parent": "p2"}, ancestry: map[string]bool{"b1..b2": true, "p1..p2": true}}
	}
	record := freshRecord()
	record.Context.AncestorChain = []review.AncestorLink{{Ref: "parent", SHA: "p1"}}
	deps := func(sc ports.SourceControl) reviewfreshness.Deps {
		return reviewfreshness.Deps{SourceControl: sc, Token: "t", Timeouts: platform.DefaultTimeouts()}
	}

	if got := reviewfreshness.Assess(context.Background(), deps(current()), record, pr7); got.State != reviewfreshness.StateCurrent {
		t.Fatalf("baseline: Assess = %+v, want current -- this test proves nothing otherwise", got)
	}
	breaks := map[string]func(*fakeSourceControl){
		"pull request read":   func(f *fakeSourceControl) { f.openErr = failed },
		"base resolution":     func(f *fakeSourceControl) { delete(f.branches, "main"); f.branchErr = failed },
		"base ancestry":       func(f *fakeSourceControl) { f.ancestryErr = failed },
		"ancestor ref":        func(f *fakeSourceControl) { f.openPR.AncestorChain = []ports.PRAncestorLink{{}} },
		"ancestor resolution": func(f *fakeSourceControl) { delete(f.branches, "parent") },
		"ancestor commit":     func(f *fakeSourceControl) { f.branches["parent"] = "" },
		"ancestor ancestry":   func(f *fakeSourceControl) { f.ancestry = map[string]bool{"b1..b2": true}; f.ancestryErr = nil },
		"head moved":          func(f *fakeSourceControl) { f.openPR.HeadSHA = "h2" },
		"closed":              func(f *fakeSourceControl) { f.found = false },
	}
	for name, breakIt := range breaks {
		sc := current()
		breakIt(sc)
		if got := reviewfreshness.Assess(context.Background(), deps(sc), record, pr7); got.State == reviewfreshness.StateCurrent {
			t.Errorf("%s broken: Assess = %+v, want anything but current", name, got)
		}
	}
	if got := reviewfreshness.Assess(context.Background(), deps(nil), record, pr7); got.State == reviewfreshness.StateCurrent {
		t.Errorf("no code host: Assess = %+v, want anything but current", got)
	}
}

// TestAssess_OutOfTimeSaysSo pins the caller's own deadline -- a session
// result's live-read budget -- as its own reason: a read it cuts short is
// unconfirmed with ReasonOutOfTime, never the cut call's reason and never
// current. A read cut short by the pull request read's own bound, with the
// caller's deadline still ahead, keeps saying the pull request could not be
// read (TestAssess_PartialReadTimeoutIsUnconfirmed).
func TestAssess_OutOfTimeSaysSo(t *testing.T) {
	t.Parallel()
	sc := &fakeSourceControl{found: true, openPR: openAt("h1"), branches: map[string]string{"main": "b1"}, openDelay: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got := reviewfreshness.Assess(ctx, reviewfreshness.Deps{SourceControl: sc, Token: "t", Timeouts: platform.DefaultTimeouts()}, freshRecord(), pr7)
	want := reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: reviewfreshness.ReasonOutOfTime}
	if got != want {
		t.Fatalf("Assess = %+v, want %+v", got, want)
	}
	if calls := sc.callList(); len(calls) != 1 {
		t.Errorf("live calls = %v, want the one cut-short read and nothing after it", calls)
	}
}

// TestAssess_OutOfTimeDuringReadLiveSaysSo pins the caller's deadline when
// it runs out after the pull request read, inside ReadLive (round 2's
// finding P3): at the base resolution, the base's ancestry check, the
// ancestor link's resolution or the link's ancestry check. A read cut
// short there is unconfirmed with ReasonOutOfTime -- never the step's own
// reason, which would tell a client the code host failed on that fact. The
// same call cut short by its own bound, with the caller's deadline still
// ahead, keeps the step's reason: the caller's context decides, not the
// call's.
func TestAssess_OutOfTimeDuringReadLiveSaysSo(t *testing.T) {
	t.Parallel()
	// The base and the ancestor link both moved forward since the verdict,
	// so ReadLive makes every one of its calls.
	record := freshRecord()
	record.Context.AncestorChain = []review.AncestorLink{{Ref: "parent", SHA: "p1"}}
	moved := func(stall string) *fakeSourceControl {
		return &fakeSourceControl{found: true, openPR: openAt("h1", prLink("parent", "")),
			branches: map[string]string{"main": "b2", "parent": "p2"}, ancestry: map[string]bool{"b1..b2": true, "p1..p2": true},
			stall: map[string]bool{stall: true}}
	}
	baseline := moved("")
	if got := reviewfreshness.Assess(context.Background(), reviewfreshness.Deps{SourceControl: baseline, Token: "t", Timeouts: platform.DefaultTimeouts()}, record, pr7); got.State != reviewfreshness.StateCurrent || len(baseline.callList()) != 5 {
		t.Fatalf("baseline: Assess = %+v after %v, want current after all five calls -- the rows below prove nothing otherwise", got, baseline.callList())
	}

	tests := []struct {
		stall string
		step  reviewfreshness.Step
		calls int // up to and including the stalled one
	}{
		{"resolve main", reviewfreshness.StepResolveBase, 2},
		{"ancestor b1..b2", reviewfreshness.StepBaseAncestry, 3},
		{"resolve parent", reviewfreshness.StepResolveAncestor, 4},
		{"ancestor p1..p2", reviewfreshness.StepAncestorAncestry, 5},
	}
	for _, tc := range tests {
		t.Run(tc.stall, func(t *testing.T) {
			t.Parallel()
			stepReason := (&reviewfreshness.Failure{Step: tc.step}).Describe()

			t.Run("the caller's deadline passes: out of time", func(t *testing.T) {
				t.Parallel()
				sc := moved(tc.stall)
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				got := reviewfreshness.Assess(ctx, reviewfreshness.Deps{SourceControl: sc, Token: "t", Timeouts: platform.DefaultTimeouts()}, record, pr7)
				want := reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: reviewfreshness.ReasonOutOfTime}
				if got != want {
					t.Fatalf("Assess = %+v, want %+v -- not the step's own %q", got, want, stepReason)
				}
				if calls := sc.callList(); len(calls) != tc.calls || calls[len(calls)-1] != tc.stall {
					t.Errorf("live calls = %v, want %d, ending at the stalled %q", calls, tc.calls, tc.stall)
				}
			})

			t.Run("the call's own bound passes first: the step's reason", func(t *testing.T) {
				t.Parallel()
				sc := moved(tc.stall)
				timeouts := platform.DefaultTimeouts()
				timeouts.DecisionInboxResolveBranchSHATimeout = 20 * time.Millisecond
				timeouts.DecisionInboxIsAncestorTimeout = 20 * time.Millisecond
				got := reviewfreshness.Assess(context.Background(), reviewfreshness.Deps{SourceControl: sc, Token: "t", Timeouts: timeouts}, record, pr7)
				want := reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: stepReason}
				if got != want {
					t.Fatalf("Assess = %+v, want %+v", got, want)
				}
			})
		})
	}
}
