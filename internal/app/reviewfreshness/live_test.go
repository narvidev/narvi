package reviewfreshness_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewfreshness"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeSourceControl answers the live calls ReadLive and Assess make and
// records each one. Any other port method is never called: the embedded
// nil interface would panic.
type fakeSourceControl struct {
	ports.SourceControl

	mu sync.Mutex
	// branches maps a branch name to its live tip; a missing branch
	// answers branchErr (or an error of its own when that is nil).
	branches  map[string]string
	branchErr error
	// ancestry maps "ancestor..descendant" to IsAncestor's answer.
	ancestry    map[string]bool
	ancestryErr error
	// openPR/found/openErr answer GetOpenPR; openDelay holds it first.
	openPR    ports.OpenPR
	found     bool
	openErr   error
	openDelay time.Duration

	calls []string
	// deadlines is how far each call's deadline was from its start.
	deadlines []time.Duration
}

func (f *fakeSourceControl) record(ctx context.Context, call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if dl, ok := ctx.Deadline(); ok {
		f.deadlines = append(f.deadlines, time.Until(dl))
	} else {
		f.deadlines = append(f.deadlines, -1)
	}
}

func (f *fakeSourceControl) ResolveBranchSHA(ctx context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	f.record(ctx, "resolve "+spec.Branch)
	sha, ok := f.branches[spec.Branch]
	if !ok {
		if f.branchErr != nil {
			return "", "", f.branchErr
		}
		return "", "", errors.New("fake: no such branch " + spec.Branch)
	}
	return sha, spec.Branch, nil
}

func (f *fakeSourceControl) IsAncestor(ctx context.Context, spec ports.IsAncestorSpec) (bool, error) {
	f.record(ctx, "ancestor "+spec.Ancestor+".."+spec.Descendant)
	if f.ancestryErr != nil {
		return false, f.ancestryErr
	}
	return f.ancestry[spec.Ancestor+".."+spec.Descendant], nil
}

func (f *fakeSourceControl) GetOpenPR(ctx context.Context, _, _ string, _ int, _ string) (ports.OpenPR, bool, error) {
	f.record(ctx, "open")
	if f.openDelay > 0 {
		select {
		case <-time.After(f.openDelay):
		case <-ctx.Done():
		}
	}
	return f.openPR, f.found, f.openErr
}

func (f *fakeSourceControl) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func target(chain ...ports.PRAncestorLink) ports.OpenPR {
	return ports.OpenPR{Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "h1", BaseRef: "main", AncestorChain: chain}
}

func prLink(ref, sha string) ports.PRAncestorLink { return ports.PRAncestorLink{Ref: ref, SHA: sha} }

// TestReadLive_Table pins every fact ReadLive establishes, every call it
// makes -- in order, and only where it can change the comparison's answer
// -- and where it stops.
func TestReadLive_Table(t *testing.T) {
	t.Parallel()
	link := func(ref, sha string) review.AncestorLink { return review.AncestorLink{Ref: ref, SHA: sha} }
	failed := errors.New("code host unavailable")

	tests := []struct {
		name      string
		sc        *fakeSourceControl
		target    ports.OpenPR
		recorded  reviewverdict.Context
		want      reviewfreshness.LiveFacts
		wantStep  reviewfreshness.Step
		wantErr   error
		wantCalls []string
	}{
		{
			name:      "nothing moved: one resolution",
			sc:        &fakeSourceControl{branches: map[string]string{"main": "b1"}},
			target:    target(),
			recorded:  reviewverdict.Context{BaseRef: "main", BaseSHA: "b1"},
			want:      reviewfreshness.LiveFacts{HeadSHA: "h1", BaseRef: "main", BaseSHA: "b1"},
			wantCalls: []string{"resolve main"},
		},
		{
			name:      "base moved forward under the same ref: confirmed",
			sc:        &fakeSourceControl{branches: map[string]string{"main": "b2"}, ancestry: map[string]bool{"b1..b2": true}},
			target:    target(),
			recorded:  reviewverdict.Context{BaseRef: "main", BaseSHA: "b1"},
			want:      reviewfreshness.LiveFacts{HeadSHA: "h1", BaseRef: "main", BaseSHA: "b2", BaseAdvancedWithoutRewrite: true},
			wantCalls: []string{"resolve main", "ancestor b1..b2"},
		},
		{
			name:      "base rewritten under the same ref: not confirmed",
			sc:        &fakeSourceControl{branches: map[string]string{"main": "b2"}, ancestry: map[string]bool{}},
			target:    target(),
			recorded:  reviewverdict.Context{BaseRef: "main", BaseSHA: "b1"},
			want:      reviewfreshness.LiveFacts{HeadSHA: "h1", BaseRef: "main", BaseSHA: "b2"},
			wantCalls: []string{"resolve main", "ancestor b1..b2"},
		},
		{
			name:      "retargeted: no ancestry check, the ref change refuses regardless",
			sc:        &fakeSourceControl{branches: map[string]string{"main": "b2"}},
			target:    target(),
			recorded:  reviewverdict.Context{BaseRef: "release", BaseSHA: "b1"},
			want:      reviewfreshness.LiveFacts{HeadSHA: "h1", BaseRef: "main", BaseSHA: "b2"},
			wantCalls: []string{"resolve main"},
		},
		{
			name:      "no recorded base commit: no ancestry check",
			sc:        &fakeSourceControl{branches: map[string]string{"main": "b2"}},
			target:    target(),
			recorded:  reviewverdict.Context{BaseRef: "main"},
			want:      reviewfreshness.LiveFacts{HeadSHA: "h1", BaseRef: "main", BaseSHA: "b2"},
			wantCalls: []string{"resolve main"},
		},
		{
			name:      "base resolution fails: stops there",
			sc:        &fakeSourceControl{branchErr: failed},
			target:    target(prLink("parent", "p1")),
			recorded:  reviewverdict.Context{BaseRef: "main", BaseSHA: "b1"},
			wantStep:  reviewfreshness.StepResolveBase,
			wantErr:   failed,
			wantCalls: []string{"resolve main"},
		},
		{
			name:      "base ancestry check fails: stops there",
			sc:        &fakeSourceControl{branches: map[string]string{"main": "b2", "parent": "p1"}, ancestryErr: failed},
			target:    target(prLink("parent", "p1")),
			recorded:  reviewverdict.Context{BaseRef: "main", BaseSHA: "b1"},
			wantStep:  reviewfreshness.StepBaseAncestry,
			wantErr:   failed,
			wantCalls: []string{"resolve main", "ancestor b1..b2"},
		},
		{
			name:      "stacked, link unchanged: the link resolved live",
			sc:        &fakeSourceControl{branches: map[string]string{"main": "b1", "parent": "p1"}},
			target:    target(prLink("parent", "cached-stale")),
			recorded:  reviewverdict.Context{BaseRef: "main", BaseSHA: "b1", AncestorChain: []review.AncestorLink{link("parent", "p1")}},
			want:      reviewfreshness.LiveFacts{HeadSHA: "h1", BaseRef: "main", BaseSHA: "b1", AncestorChain: []review.AncestorLink{link("parent", "p1")}},
			wantCalls: []string{"resolve main", "resolve parent"},
		},
		{
			name:     "stacked, link moved forward: confirmed",
			sc:       &fakeSourceControl{branches: map[string]string{"main": "b1", "parent": "p2"}, ancestry: map[string]bool{"p1..p2": true}},
			target:   target(prLink("parent", "")),
			recorded: reviewverdict.Context{BaseRef: "main", BaseSHA: "b1", AncestorChain: []review.AncestorLink{link("parent", "p1")}},
			want: reviewfreshness.LiveFacts{HeadSHA: "h1", BaseRef: "main", BaseSHA: "b1",
				AncestorChain: []review.AncestorLink{link("parent", "p2")}, AncestorChainAdvancedWithoutRewrite: true},
			wantCalls: []string{"resolve main", "resolve parent", "ancestor p1..p2"},
		},
		{
			name:      "stacked since the verdict: no chain ancestry check",
			sc:        &fakeSourceControl{branches: map[string]string{"main": "b1", "parent": "p2"}},
			target:    target(prLink("parent", "")),
			recorded:  reviewverdict.Context{BaseRef: "main", BaseSHA: "b1"},
			want:      reviewfreshness.LiveFacts{HeadSHA: "h1", BaseRef: "main", BaseSHA: "b1", AncestorChain: []review.AncestorLink{link("parent", "p2")}},
			wantCalls: []string{"resolve main", "resolve parent"},
		},
		{
			name:      "a link with no readable ref: not established",
			sc:        &fakeSourceControl{branches: map[string]string{"main": "b1"}},
			target:    target(prLink("", "")),
			recorded:  reviewverdict.Context{BaseRef: "main", BaseSHA: "b1"},
			wantStep:  reviewfreshness.StepAncestorRefUnreadable,
			wantCalls: []string{"resolve main"},
		},
		{
			name:      "the link's resolution fails",
			sc:        &fakeSourceControl{branches: map[string]string{"main": "b1"}, branchErr: failed},
			target:    target(prLink("parent", "")),
			recorded:  reviewverdict.Context{BaseRef: "main", BaseSHA: "b1"},
			wantStep:  reviewfreshness.StepResolveAncestor,
			wantErr:   failed,
			wantCalls: []string{"resolve main", "resolve parent"},
		},
		{
			name:      "the link resolves to no commit",
			sc:        &fakeSourceControl{branches: map[string]string{"main": "b1", "parent": ""}},
			target:    target(prLink("parent", "")),
			recorded:  reviewverdict.Context{BaseRef: "main", BaseSHA: "b1"},
			wantStep:  reviewfreshness.StepResolveAncestor,
			wantCalls: []string{"resolve main", "resolve parent"},
		},
		{
			name:      "the link's ancestry check fails",
			sc:        &fakeSourceControl{branches: map[string]string{"main": "b1", "parent": "p2"}, ancestryErr: failed},
			target:    target(prLink("parent", "")),
			recorded:  reviewverdict.Context{BaseRef: "main", BaseSHA: "b1", AncestorChain: []review.AncestorLink{link("parent", "p1")}},
			wantStep:  reviewfreshness.StepAncestorAncestry,
			wantErr:   failed,
			wantCalls: []string{"resolve main", "resolve parent", "ancestor p1..p2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			timeouts := platform.DefaultTimeouts()
			got, failure := reviewfreshness.ReadLive(context.Background(), timeouts, tc.sc, "bot-token", tc.target, tc.recorded)
			if tc.wantStep == "" {
				if failure != nil {
					t.Fatalf("ReadLive failed at %q (%v), want the facts", failure.Step, failure.Err)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("ReadLive = %+v, want %+v", got, tc.want)
				}
			} else {
				if failure == nil {
					t.Fatalf("ReadLive = %+v, want a failure at %q", got, tc.wantStep)
				}
				if failure.Step != tc.wantStep || !errors.Is(failure.Err, tc.wantErr) || (tc.wantErr == nil && failure.Err != nil) {
					t.Errorf("failure = %q (%v), want %q (%v)", failure.Step, failure.Err, tc.wantStep, tc.wantErr)
				}
				if !reflect.DeepEqual(got, reviewfreshness.LiveFacts{}) {
					t.Errorf("a failed read returned facts %+v, want none", got)
				}
			}
			if calls := tc.sc.callList(); !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", calls, tc.wantCalls)
			}
			// Every call is bounded by its platform.Timeouts constant.
			for i, call := range tc.sc.callList() {
				bound := timeouts.DecisionInboxResolveBranchSHATimeout
				if len(call) > 8 && call[:8] == "ancestor" {
					bound = timeouts.DecisionInboxIsAncestorTimeout
				}
				if d := tc.sc.deadlines[i]; d <= 0 || d > bound {
					t.Errorf("call %q had deadline %v away, want within its bound %v", call, d, bound)
				}
			}
		})
	}
}
