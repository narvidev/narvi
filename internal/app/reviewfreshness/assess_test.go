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
// and which reads it needed: none for what the record alone decides.
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
			name:   "policy bumped: stale, no live read",
			record: func(r *reviewverdict.Record) { r.Context.PolicyVersion = autoapproval.CurrentPolicyVersion - 1 },
			sc:     &fakeSourceControl{},
			want:   reviewfreshness.Assessment{State: reviewfreshness.StateStale, Reason: string(autoapproval.ReasonPolicyVersionMismatch)},
		},
		{
			name:   "no context recorded: unconfirmed, no live read",
			record: func(r *reviewverdict.Record) { r.Context = reviewverdict.Context{} },
			sc:     &fakeSourceControl{},
			want:   reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: string(autoapproval.ReasonContextUnknown)},
		},
		{
			name:   "recorded base commit unknown: unconfirmed, no live read",
			record: func(r *reviewverdict.Record) { r.Context.BaseSHA = "" },
			sc:     &fakeSourceControl{},
			want:   reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: string(autoapproval.ReasonBaseSHAUnknown)},
		},
		{
			name: "recorded ancestor link unknown: unconfirmed, no live read",
			record: func(r *reviewverdict.Record) {
				r.Context.AncestorChain = []review.AncestorLink{{Ref: "parent", SHA: ""}}
			},
			sc:   &fakeSourceControl{},
			want: reviewfreshness.Assessment{State: reviewfreshness.StateUnconfirmed, Reason: string(autoapproval.ReasonAncestorChainUnknown)},
		},
		{
			name:   "no head recorded: stale, no live read",
			record: func(r *reviewverdict.Record) { r.HeadSHA = "" },
			sc:     &fakeSourceControl{},
			want:   reviewfreshness.Assessment{State: reviewfreshness.StateStale, Reason: string(autoapproval.ReasonStaleVerdict)},
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
			name:      "the head moved: stale",
			sc:        &fakeSourceControl{found: true, openPR: openAt("h2"), branches: map[string]string{"main": "b1"}},
			want:      reviewfreshness.Assessment{State: reviewfreshness.StateStale, Reason: string(autoapproval.ReasonStaleVerdict)},
			wantCalls: 2,
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
