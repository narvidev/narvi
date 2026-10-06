package platform_test

import (
	"errors"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/gitclone"
)

// TestReviewCheckoutAttemptCeiling_CountsEveryStep pins the longest one
// review checkout can run in the sandbox agent (technical plan §21.1) to
// the steps gitclone.CheckoutPullRef spawns: platform cannot import
// gitclone, which imports platform, so its two counts are copied here and
// fail this test when they drift apart. With the shipped values, one fetch
// (90s) and 18 local steps (30s), each plus the 10s stop grace: 13m40s.
func TestReviewCheckoutAttemptCeiling_CountsEveryStep(t *testing.T) {
	t.Parallel()

	if platform.ReviewCheckoutNetworkGitSteps != gitclone.PullCheckoutNetworkGitSpawns {
		t.Errorf("ReviewCheckoutNetworkGitSteps = %d, gitclone.PullCheckoutNetworkGitSpawns = %d: the ceiling no longer counts every fetch",
			platform.ReviewCheckoutNetworkGitSteps, gitclone.PullCheckoutNetworkGitSpawns)
	}
	if platform.ReviewCheckoutLocalGitSteps != gitclone.PullCheckoutMaxLocalGitSpawns {
		t.Errorf("ReviewCheckoutLocalGitSteps = %d, gitclone.PullCheckoutMaxLocalGitSpawns = %d: the ceiling no longer counts every local step",
			platform.ReviewCheckoutLocalGitSteps, gitclone.PullCheckoutMaxLocalGitSpawns)
	}

	to := platform.DefaultTimeouts()
	const grace = 10 * time.Second
	want := (90*time.Second + grace) + 18*(30*time.Second+grace)
	if want != 13*time.Minute+40*time.Second {
		t.Fatalf("hand-computed ceiling = %s, want 13m40s -- fix this test's arithmetic", want)
	}
	if got := to.ReviewCheckoutAttemptCeiling(); got != want {
		t.Errorf("ReviewCheckoutAttemptCeiling() = %s, want %s", got, want)
	}

	// Each step's own timeout and the grace count: moving any moves it.
	to.GitFetchStepTimeout += time.Second
	to.GitSyncStepTimeout += time.Second
	to.ProcessStopGracePeriod += time.Second
	if got, moved := to.ReviewCheckoutAttemptCeiling(), want+2*time.Second+18*2*time.Second; got != moved {
		t.Errorf("ReviewCheckoutAttemptCeiling() with each bound a second longer = %s, want %s", got, moved)
	}
}

// TestReviewCheckoutFailedRetireBackoff pins the waits before the send
// whose failed reply retires a gen (technical plan §21.1): 10s then 20s
// with the shipped values, none for a count of one, and a saturated
// value, never an overflow, for a count no bound could hold.
func TestReviewCheckoutFailedRetireBackoff(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		count int
		want  time.Duration
	}{
		{name: "shipped", count: 3, want: 30 * time.Second},
		{name: "one", count: 1, want: 0},
		{name: "two", count: 2, want: 10 * time.Second},
		{name: "four", count: 4, want: 70 * time.Second},
		{name: "far past any bound", count: 200, want: time.Duration(1<<63 - 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			to := platform.DefaultTimeouts()
			to.ReviewCheckoutFailuresBeforeRetire = tc.count
			if got := to.ReviewCheckoutFailedRetireBackoff(); got != tc.want {
				t.Errorf("ReviewCheckoutFailedRetireBackoff() with %d = %s, want %s", tc.count, got, tc.want)
			}
		})
	}
}

// TestValidate_ReviewCheckout pins the review checkout's bounds (technical
// plan §21.1): the shipped 15m, 1m, 10s and 3, each refused when it is not
// positive, and each link refused when either side moves past the other
// and accepted at exactly MinTimeoutMargin:
//
//   - the timeout above one slow attempt after a re-fetch's wait, so a
//     working sandbox is never refused for slowness;
//   - above a dead replica's takeover, so a command lost with it is sent
//     again on the reconnect inside the bound;
//   - above the lag window, which stays above the re-fetch interval, so a
//     lagging ref is fetched again at least once;
//   - above the waits before a gen whose checkouts fail at once is
//     retired;
//   - below TurnDeadline.
func TestValidate_ReviewCheckout(t *testing.T) {
	t.Parallel()

	d := platform.DefaultTimeouts()
	if d.ReviewCheckoutTimeout != 15*time.Minute || d.ReviewCheckoutRefLagWindow != time.Minute ||
		d.ReviewCheckoutRefetchInterval != 10*time.Second || d.ReviewCheckoutFailuresBeforeRetire != 3 {
		t.Fatalf("defaults = %s, %s, %s, %d; want 15m, 1m, 10s, 3", d.ReviewCheckoutTimeout, d.ReviewCheckoutRefLagWindow,
			d.ReviewCheckoutRefetchInterval, d.ReviewCheckoutFailuresBeforeRetire)
	}

	const (
		ceilingChain  = "ReviewCheckoutTimeout > ReviewCheckoutAttemptCeiling + ReviewCheckoutRefetchInterval"
		takeoverChain = "ReviewCheckoutTimeout > ActorLockServerReapTime + SandboxWSReconnectMaxBackoff + ActorHydrateTimeout"
		lagChain      = "ReviewCheckoutTimeout > ReviewCheckoutRefLagWindow"
		retireChain   = "ReviewCheckoutTimeout > ReviewCheckoutFailedRetireBackoff"
		refetchChain  = "ReviewCheckoutRefLagWindow > ReviewCheckoutRefetchInterval"
		deadlineChain = "TurnDeadline > ReviewCheckoutTimeout"
	)
	takeover := func(to platform.Timeouts) time.Duration {
		return to.ActorLockServerReapTime() + to.SandboxWSReconnectMaxBackoff + to.ActorHydrateTimeout
	}
	margin := platform.MinTimeoutMargin
	for _, tc := range []struct {
		name      string
		edit      func(*platform.Timeouts)
		wantField string
		wantCount string
		// chain is the link the case is about, broken whether it reads.
		chain  string
		broken bool
	}{
		{name: "timeout zero", edit: func(to *platform.Timeouts) { to.ReviewCheckoutTimeout = 0 }, wantField: "ReviewCheckoutTimeout"},
		{name: "lag window zero", edit: func(to *platform.Timeouts) { to.ReviewCheckoutRefLagWindow = 0 }, wantField: "ReviewCheckoutRefLagWindow"},
		{name: "re-fetch interval negative", edit: func(to *platform.Timeouts) { to.ReviewCheckoutRefetchInterval = -time.Second }, wantField: "ReviewCheckoutRefetchInterval"},
		{name: "failures zero", edit: func(to *platform.Timeouts) { to.ReviewCheckoutFailuresBeforeRetire = 0 }, wantCount: "ReviewCheckoutFailuresBeforeRetire"},

		{name: "timeout within the margin above a slow attempt after a re-fetch", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutTimeout = to.ReviewCheckoutAttemptCeiling() + to.ReviewCheckoutRefetchInterval + margin - time.Second
		}, chain: ceilingChain, broken: true},
		{name: "a local git step long enough to outlast the timeout", edit: func(to *platform.Timeouts) {
			to.GitSyncStepTimeout = to.ReviewCheckoutTimeout / platform.ReviewCheckoutLocalGitSteps
		}, chain: ceilingChain, broken: true},
		{name: "timeout exactly the margin above a slow attempt after a re-fetch", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutTimeout = to.ReviewCheckoutAttemptCeiling() + to.ReviewCheckoutRefetchInterval + margin
		}, chain: ceilingChain},

		{name: "timeout within the margin above the takeover", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutTimeout = takeover(*to) + margin - time.Second
		}, chain: takeoverChain, broken: true},
		{name: "timeout exactly the margin above the takeover", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutTimeout = takeover(*to) + margin
		}, chain: takeoverChain},

		{name: "lag window within the margin below the timeout", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutRefLagWindow = to.ReviewCheckoutTimeout - margin + time.Second
		}, chain: lagChain, broken: true},
		{name: "lag window exactly the margin below the timeout", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutRefLagWindow = to.ReviewCheckoutTimeout - margin
		}, chain: lagChain},

		{name: "retire waits past the timeout", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutFailuresBeforeRetire = 8 // 10s * (2^7 - 1) = 21m10s
		}, chain: retireChain, broken: true},
		{name: "retire waits within the margin below the timeout", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutTimeout = to.ReviewCheckoutFailedRetireBackoff() + margin - time.Second
		}, chain: retireChain, broken: true},
		{name: "retire waits exactly the margin below the timeout", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutTimeout = to.ReviewCheckoutFailedRetireBackoff() + margin
		}, chain: retireChain},

		{name: "re-fetch interval within the margin below the lag window", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutRefetchInterval = to.ReviewCheckoutRefLagWindow - margin + time.Second
		}, chain: refetchChain, broken: true},
		{name: "re-fetch interval exactly the margin below the lag window", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutRefetchInterval = to.ReviewCheckoutRefLagWindow - margin
		}, chain: refetchChain},

		{name: "timeout within the margin below TurnDeadline", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutTimeout = to.TurnDeadline - margin + time.Second
		}, chain: deadlineChain, broken: true},
		{name: "timeout exactly the margin below TurnDeadline", edit: func(to *platform.Timeouts) {
			to.ReviewCheckoutTimeout = to.TurnDeadline - margin
		}, chain: deadlineChain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			to := platform.DefaultTimeouts()
			tc.edit(&to)
			err := to.Validate()
			switch {
			case tc.wantField != "":
				var pos *platform.TimeoutMustBePositiveError
				if !errors.As(err, &pos) || pos.Field != tc.wantField {
					t.Fatalf("Validate() = %v, want %s refused as non-positive", err, tc.wantField)
				}
			case tc.wantCount != "":
				var cnt *platform.CountMustBePositiveError
				if !errors.As(err, &cnt) || cnt.Field != tc.wantCount {
					t.Fatalf("Validate() = %v, want %s refused as below one", err, tc.wantCount)
				}
			default:
				if got := hasBrokenLink(err, tc.chain); got != tc.broken {
					t.Fatalf("Validate() = %v: link %q broken = %v, want %v", err, tc.chain, got, tc.broken)
				}
			}
		})
	}
}

// hasBrokenLink reports whether err, a joined Validate error, names chain
// among its broken links.
func hasBrokenLink(err error, chain string) bool {
	if err == nil {
		return false
	}
	var errs []error
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs = joined.Unwrap()
	} else {
		errs = []error{err}
	}
	for _, e := range errs {
		var inv *platform.TimeoutInvariantError
		if errors.As(e, &inv) && inv.Chain == chain {
			return true
		}
	}
	return false
}
