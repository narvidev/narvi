package automerge

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/app/ports"
	domainautomerge "github.com/narvidev/narvi/internal/domain/automerge"
)

func testBackoffConfig() domainautomerge.BackoffConfig {
	return domainautomerge.BackoffConfig{
		BaseDelay: 2 * time.Minute,
		MaxDelay:  30 * time.Minute,
	}
}

// TestClassify_TableDriven proves the ONE classification decision this
// whole fix hinges on (docs/TECHNICAL_PLAN.md §17): a 401 is always
// worker-wide, a genuinely-denied 403 is always repo-scoped, and neither
// a rate-limited 403 nor any other error ever classifies as either --
// getting either direction wrong is a real defect (a rate limit
// misclassified as terminal stops merges that should resume on their
// own; a revoked credential misclassified as transient reproduces the
// exact "hammers GitHub forever" bug this fix exists to close).
func TestClassify_TableDriven(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want authScope
	}{
		{
			name: "nil error classifies as none",
			err:  nil,
			want: authScopeNone,
		},
		{
			name: "plain error classifies as none",
			err:  errors.New("some transient failure"),
			want: authScopeNone,
		},
		{
			name: "MergePRError 401 classifies as worker-wide auth failure",
			err:  &ports.MergePRError{Status: 401, Message: "Bad credentials"},
			want: authScopeWorker,
		},
		{
			name: "MergePRError 403 non-rate-limited classifies as repo-scoped permission denial",
			err:  &ports.MergePRError{Status: 403, Message: "Resource not accessible", RateLimited: false},
			want: authScopeRepo,
		},
		{
			name: "MergePRError 403 rate-limited must NEVER classify as either sentinel",
			err:  &ports.MergePRError{Status: 403, Message: "API rate limit exceeded", RateLimited: true},
			want: authScopeNone,
		},
		{
			name: "MergePRError 405 (not mergeable) classifies as none",
			err:  &ports.MergePRError{Status: 405, Message: "not mergeable"},
			want: authScopeNone,
		},
		{
			name: "MergePRError 409 (stale head sha) classifies as none",
			err:  &ports.MergePRError{Status: 409, Message: "head sha mismatch"},
			want: authScopeNone,
		},
		{
			name: "a wrapped ErrAuthenticationFailed classifies as worker-wide (adapter-agnostic path)",
			err:  fmt.Errorf("some adapter: %w", ports.ErrAuthenticationFailed),
			want: authScopeWorker,
		},
		{
			name: "a wrapped ErrPermissionDenied classifies as repo-scoped (adapter-agnostic path)",
			err:  fmt.Errorf("some adapter: %w", ports.ErrPermissionDenied),
			want: authScopeRepo,
		},
		{
			name: "ErrShadowSuppressed classifies as none",
			err:  ports.ErrShadowSuppressed,
			want: authScopeNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err); got != tc.want {
				t.Errorf("classify(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestAuthGuard_Allow_InitiallyOpen proves a fresh authGuard never blocks
// a repo that has seen no failures yet -- the guard must be transparent
// in the ordinary, healthy case.
func TestAuthGuard_Allow_InitiallyOpen(t *testing.T) {
	g := newAuthGuard(testBackoffConfig())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if allowed, _ := g.allow("acme/repo", now); !allowed {
		t.Fatal("allow() = false on a fresh authGuard, want true")
	}
}

// TestAuthGuard_RepoScope_BacksOffThenDeadLetters drives a single repo's
// own consecutive 403 (non-rate-limited) failures through the full
// schedule and proves: (1) immediately after a failure, allow() blocks
// further attempts at the SAME now; (2) allow() reopens once now passes
// the scheduled backoff window; (3) at domainautomerge.MaxAuthFailures,
// the repo is dead-lettered PERMANENTLY -- allow() stays false no matter
// how far now advances afterward.
func TestAuthGuard_RepoScope_BacksOffThenDeadLetters(t *testing.T) {
	g := newAuthGuard(testBackoffConfig())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const repo = "acme/flaky-permissions"
	permErr := &ports.MergePRError{Status: 403, Message: "Resource not accessible by integration"}

	for i := 1; i < domainautomerge.MaxAuthFailures; i++ {
		allowed, res := g.allow(repo, now)
		if !allowed {
			t.Fatalf("attempt %d: allow() = false BEFORE this attempt's own failure, want true (still inside an earlier window that should have elapsed)", i)
		}

		scope, target, failures := g.recordFailure(repo, permErr, now, res)
		if scope != authScopeNone {
			t.Fatalf("attempt %d: recordFailure() scope = %v, want authScopeNone (not yet at MaxAuthFailures=%d)", i, scope, domainautomerge.MaxAuthFailures)
		}
		if failures != 0 {
			// recordFailure only reports a non-zero count on an actual
			// dead-letter transition -- see its own doc comment.
			t.Fatalf("attempt %d: recordFailure() consecutiveFailures = %d, want 0 (not a transition)", i, failures)
		}
		_ = target

		if allowed, _ := g.allow(repo, now); allowed {
			t.Fatalf("attempt %d: allow() = true immediately after a classified failure at the SAME now, want false (no backoff applied)", i)
		}

		// Advance now past this attempt's own scheduled backoff window --
		// mirrors domainautomerge.EvaluateBackoff's own doubling schedule
		// exactly, so this loop proves the window computed really does
		// reopen allow(), not merely that SOME large jump does.
		decision := domainautomerge.EvaluateBackoff(i, testBackoffConfig(), now)
		now = decision.NextRetryAt
		if allowed, _ := g.allow(repo, now); !allowed {
			t.Fatalf("attempt %d: allow() = false once now reached the scheduled NextRetryAt (%v), want true", i, now)
		}
	}

	// The MaxAuthFailures-th consecutive failure must dead-letter.
	allowed, res := g.allow(repo, now)
	if !allowed {
		t.Fatalf("final attempt: allow() = false BEFORE the dead-lettering failure, want true")
	}
	scope, target, failures := g.recordFailure(repo, permErr, now, res)
	if scope != authScopeRepo {
		t.Fatalf("final recordFailure() scope = %v, want authScopeRepo (MaxAuthFailures=%d reached)", scope, domainautomerge.MaxAuthFailures)
	}
	if target != repo {
		t.Errorf("final recordFailure() target = %q, want %q", target, repo)
	}
	if failures != domainautomerge.MaxAuthFailures {
		t.Errorf("final recordFailure() consecutiveFailures = %d, want %d", failures, domainautomerge.MaxAuthFailures)
	}

	// Permanently dead: allow() must stay false no matter how far now
	// advances -- a dead-lettered scope is never retried again for the
	// rest of this process's own lifetime (authguard.go's own doc
	// comment).
	farFuture := now.Add(365 * 24 * time.Hour)
	if allowed, _ := g.allow(repo, farFuture); allowed {
		t.Fatal("allow() = true a full year after dead-lettering, want false -- dead-letter must be permanent, not merely a very long backoff")
	}

	// A further recordFailure call must be a no-op (never re-reports a
	// transition, never double-counts) -- this is what stops
	// worker.recordAuthOutcome from re-auditing/re-alerting on every
	// subsequent skipped tick. The reservation passed here is irrelevant
	// -- the state.deadLettered check in recordFailure short-circuits
	// before it ever looks at the observed generation.
	scope, _, failures = g.recordFailure(repo, permErr, farFuture, authReservation{})
	if scope != authScopeNone || failures != 0 {
		t.Errorf("recordFailure() after already dead-lettered = (%v, _, %d), want (authScopeNone, _, 0) -- must never re-report a transition", scope, failures)
	}
}

// TestAuthGuard_WorkerScope_BlocksEveryRepo proves ports.
// ErrAuthenticationFailed's own worker-wide scope really is worker-wide:
// once dead-lettered via ONE repo's own call, EVERY other repo -- even
// one that has never itself failed -- is blocked too, because they all
// share the SAME bot credential.
func TestAuthGuard_WorkerScope_BlocksEveryRepo(t *testing.T) {
	g := newAuthGuard(testBackoffConfig())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	authErr := &ports.MergePRError{Status: 401, Message: "Bad credentials"}

	var scope authScope
	for i := 1; i <= domainautomerge.MaxAuthFailures; i++ {
		_, res := g.allow("acme/repo-a", now)
		scope, _, _ = g.recordFailure("acme/repo-a", authErr, now, res)
		decision := domainautomerge.EvaluateBackoff(i, testBackoffConfig(), now)
		now = decision.NextRetryAt
	}
	if scope != authScopeWorker {
		t.Fatalf("final recordFailure() scope = %v, want authScopeWorker", scope)
	}

	if allowed, _ := g.allow("acme/repo-a", now); allowed {
		t.Error("allow(repo-a) = true after worker-wide dead-letter, want false")
	}
	if allowed, _ := g.allow("acme/repo-that-never-failed", now); allowed {
		t.Error("allow(repo-that-never-failed) = true after a WORKER-WIDE dead-letter, want false -- every repo shares the same bot token")
	}
}

// TestAuthGuard_RepoScope_NeverBlocksOtherRepos is WorkerScope's own
// negative counterpart: a PER-REPOSITORY dead-letter (ports.
// ErrPermissionDenied) must never leak into blocking an unrelated
// repository -- getting scope isolation backwards in EITHER direction is
// the exact defect this Step's own design points call out.
func TestAuthGuard_RepoScope_NeverBlocksOtherRepos(t *testing.T) {
	g := newAuthGuard(testBackoffConfig())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	permErr := &ports.MergePRError{Status: 403, Message: "Resource not accessible by integration"}

	for i := 1; i <= domainautomerge.MaxAuthFailures; i++ {
		_, res := g.allow("acme/denied-repo", now)
		g.recordFailure("acme/denied-repo", permErr, now, res)
		decision := domainautomerge.EvaluateBackoff(i, testBackoffConfig(), now)
		now = decision.NextRetryAt
	}

	if allowed, _ := g.allow("acme/denied-repo", now); allowed {
		t.Error("allow(denied-repo) = true after its own dead-letter, want false")
	}
	if allowed, _ := g.allow("acme/unrelated-repo", now); !allowed {
		t.Error("allow(unrelated-repo) = false after a DIFFERENT repo's own per-repo dead-letter, want true -- per-repo scope must never leak")
	}
}

// TestAuthGuard_RecordSuccess_ResetsWorkerAndThisRepoOnly proves
// recordSuccess's own documented scope: it resets the shared worker-wide
// streak (a real successful call proves the credential works, worker-
// wide) AND this one repo's own per-repo streak, but must never clear a
// DIFFERENT repo's own already-accumulated failures.
//
// repo-a deliberately accumulates BOTH kinds of failure -- a worker-wide
// one (401) AND its own, independent per-repo one (403) -- so this test
// actually exercises "ThisRepo": an earlier version of this test drove
// repo-a through ONLY a worker-wide (401) failure, so repo-a itself
// never had a per-repo entry for recordSuccess's own `delete(g.perRepo,
// repoFullName)` to remove -- deleting that line kept the old test green.
func TestAuthGuard_RecordSuccess_ResetsWorkerAndThisRepoOnly(t *testing.T) {
	g := newAuthGuard(testBackoffConfig())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	authErr := &ports.MergePRError{Status: 401, Message: "Bad credentials"}
	permErr := &ports.MergePRError{Status: 403, Message: "Resource not accessible by integration"}

	// repo-a's worker-wide failure and its own, separate per-repo
	// failure, plus repo-b's own, entirely unrelated per-repo failure --
	// each of these three recordFailure calls is the FIRST-ever failure
	// for its own (state, scope) pair, so a zero-value authReservation{}
	// (observed generation 0) matches the freshly-created state's own
	// generation (0) exactly.
	g.recordFailure("acme/repo-a", authErr, now, authReservation{})
	g.recordFailure("acme/repo-a", permErr, now, authReservation{})
	g.recordFailure("acme/repo-b", permErr, now, authReservation{})

	if allowed, _ := g.allow("acme/repo-a", now); allowed {
		t.Fatal("allow(repo-a) = true immediately after its own failures, want false (still inside backoff)")
	}
	if allowed, _ := g.allow("acme/repo-c", now); allowed {
		t.Fatal("allow(repo-c) = true, want false (worker-wide streak from repo-a's own failure must block every repo)")
	}

	// A successful call against repo-a resets BOTH the worker-wide streak
	// AND repo-a's own per-repo streak...
	g.recordSuccess("acme/repo-a")

	if allowed, _ := g.allow("acme/repo-a", now); !allowed {
		t.Error("allow(repo-a) = false after recordSuccess(repo-a), want true -- both repo-a's own per-repo streak and the worker-wide streak must be cleared")
	}
	if allowed, _ := g.allow("acme/repo-c", now); !allowed {
		t.Error("allow(repo-c) = false after recordSuccess(repo-a) cleared the worker-wide streak, want true")
	}

	// Whitebox "ThisRepo" assertion: repo-a's own per-repo entry must be
	// gone entirely, not merely backed off past its own window.
	g.mu.Lock()
	_, stillPresent := g.perRepo["acme/repo-a"]
	g.mu.Unlock()
	if stillPresent {
		t.Error("g.perRepo[\"acme/repo-a\"] still present after recordSuccess(repo-a) -- repo-a's own per-repo streak must be cleared, not merely the worker-wide one")
	}

	// ...but repo-b's own INDEPENDENT per-repo streak must survive --
	// recordSuccess only ever names one repo.
	if allowed, _ := g.allow("acme/repo-b", now); allowed {
		t.Error("allow(repo-b) = true after an UNRELATED repo's own recordSuccess, want false -- per-repo state must not be cleared by a different repo's success")
	}
}

// TestAuthGuard_RecordSuccess_NeverUnlatchesWorkerDeadLetter is blocker
// #2 of this Step's own adversarial review: recordSuccess used to reset
// g.worker unconditionally, so the breaker never actually LATCHED -- any
// later successful call (even one racing in from BEFORE the
// dead-lettering transition landed, worker.go's own errgroup fan-out)
// would silently resurrect a worker-wide scope this package has already
// committed to treating as permanently terminal, resuming full-rate
// hammering and re-firing a fresh audit_log row/Error log on every
// SUBSEQUENT failure -- the exact operator noise this fix exists to
// remove, reproduced and amplified. Mutation-verified: delete the
// `if !g.worker.deadLettered` guard in recordSuccess and this test
// fails.
func TestAuthGuard_RecordSuccess_NeverUnlatchesWorkerDeadLetter(t *testing.T) {
	g := newAuthGuard(testBackoffConfig())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	authErr := &ports.MergePRError{Status: 401, Message: "Bad credentials"}

	for i := 1; i <= domainautomerge.MaxAuthFailures; i++ {
		_, res := g.allow("acme/repo-a", now)
		g.recordFailure("acme/repo-a", authErr, now, res)
		now = domainautomerge.EvaluateBackoff(i, testBackoffConfig(), now).NextRetryAt
	}

	g.mu.Lock()
	deadBefore := g.worker.deadLettered
	g.mu.Unlock()
	if !deadBefore {
		t.Fatal("setup: g.worker.deadLettered = false, want true before this test's own assertion")
	}

	// A later successful call -- against repo-a itself, or any other
	// repo, it does not matter which, since recordSuccess always touches
	// the SAME shared worker-wide scope -- must not resurrect it.
	g.recordSuccess("acme/repo-a")

	g.mu.Lock()
	deadAfter := g.worker.deadLettered
	g.mu.Unlock()
	if !deadAfter {
		t.Fatal("g.worker.deadLettered = false after recordSuccess -- the breaker must LATCH: a dead-lettered scope must never un-dead-letter (authguard.go's own doc comment)")
	}
	if allowed, _ := g.allow("acme/repo-that-never-failed", now); allowed {
		t.Error("allow() = true for an unrelated repo after recordSuccess on an already worker-wide-dead-lettered scope -- the dead-letter must keep blocking every repo")
	}
}

// TestAuthGuard_RecordSuccess_NeverUnlatchesRepoDeadLetter is the
// WorkerDeadLetter test's own per-repository counterpart -- a
// PER-REPOSITORY dead-letter must latch exactly the same way.
func TestAuthGuard_RecordSuccess_NeverUnlatchesRepoDeadLetter(t *testing.T) {
	g := newAuthGuard(testBackoffConfig())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	permErr := &ports.MergePRError{Status: 403, Message: "Resource not accessible by integration"}
	const repo = "acme/denied-repo"

	for i := 1; i <= domainautomerge.MaxAuthFailures; i++ {
		_, res := g.allow(repo, now)
		g.recordFailure(repo, permErr, now, res)
		now = domainautomerge.EvaluateBackoff(i, testBackoffConfig(), now).NextRetryAt
	}

	g.recordSuccess(repo)

	if allowed, _ := g.allow(repo, now); allowed {
		t.Error("allow(repo) = true after recordSuccess on an already per-repo-dead-lettered repo -- a per-repository dead-letter must also latch permanently")
	}
}

// TestAuthGuard_RecordFailure_StaleReservationDiscarded is a
// deterministic (no goroutines) proof of the SAME invariant
// TestAuthGuard_ConcurrentWorkerScopeFailures_CoalesceToOne exercises
// under real concurrency below: a recordFailure call whose reservation
// names a generation the scope has already moved past must be
// discarded, not double-counted, regardless of why the caller's own view
// went stale.
func TestAuthGuard_RecordFailure_StaleReservationDiscarded(t *testing.T) {
	g := newAuthGuard(testBackoffConfig())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	authErr := &ports.MergePRError{Status: 401, Message: "Bad credentials"}

	// Two callers both observe the SAME fresh (generation 0) worker
	// state via their own allow() call -- exactly what PumpOnce's own
	// concurrent errgroup fan-out produces when N armed repos all check
	// allow() before any of them has reported back (authReservation's
	// own doc comment, authguard.go).
	_, resFirst := g.allow("acme/repo-a", now)
	_, resSecond := g.allow("acme/repo-b", now)

	scope, _, failures := g.recordFailure("acme/repo-a", authErr, now, resFirst)
	if scope != authScopeNone || failures != 0 {
		t.Fatalf("first recordFailure() = (%v, _, %d), want (authScopeNone, _, 0) (1 < MaxAuthFailures)", scope, failures)
	}

	g.mu.Lock()
	gotFailures := g.worker.consecutiveFailures
	g.mu.Unlock()
	if gotFailures != 1 {
		t.Fatalf("after first recordFailure(): g.worker.consecutiveFailures = %d, want 1", gotFailures)
	}

	// The second caller's own reservation is now STALE -- the worker's
	// own generation already advanced past what it observed. Must be
	// discarded, not counted as a second consecutive failure.
	scope, _, failures = g.recordFailure("acme/repo-b", authErr, now, resSecond)
	if scope != authScopeNone || failures != 0 {
		t.Fatalf("second (stale) recordFailure() = (%v, _, %d), want (authScopeNone, _, 0) -- a stale reservation must never report a transition", scope, failures)
	}

	g.mu.Lock()
	gotFailures = g.worker.consecutiveFailures
	g.mu.Unlock()
	if gotFailures != 1 {
		t.Fatalf("after the stale recordFailure(): g.worker.consecutiveFailures = %d, want still 1 -- the stale report must be discarded, not double-counted", gotFailures)
	}
}

// TestAuthGuard_ConcurrentWorkerScopeFailures_CoalesceToOne is this
// Step's own end-to-end proof of blocker #1 (the adversarial review's
// own finding): "authGuard.allow() is a check-then-act with no in-flight
// accounting", so N goroutines calling allow() concurrently, at the SAME
// now, all observe the SAME pre-failure worker-wide state before any one
// of them has reported back -- exactly PumpOnce's own errgroup fan-out
// shape (worker.go), one goroutine per armed repo. A real GitHub round
// trip is modeled here by a real time.Sleep AFTER allow() succeeds and
// BEFORE recordFailure runs -- long enough that every goroutine is
// guaranteed to have already cleared its own allow() check (and so
// observed the SAME generation) before any of them reports its own
// failure, mirroring the review's own "every goroutine clears allow() in
// microseconds and then blocks on a real GitHub round trip" diagnosis.
//
// WITHOUT the fix (a plain g.mu-protected increment with no generation
// coalescing), all N goroutines' own recordFailure calls would each
// independently increment consecutiveFailures, driving it from 0 to N
// inside this single "tick" -- with N >= MaxAuthFailures, dead-lettering
// the worker-wide scope in one shot, exactly the counterfactual the
// review's own verifier describes ("only one failure is recorded" once
// concurrency/latency is removed, implying the OPPOSITE -- N failures --
// happens WITH it). This test proves the fixed behavior holds even WITH
// real concurrency and real latency: exactly ONE consecutive failure is
// recorded, regardless of how many goroutines raced.
func TestAuthGuard_ConcurrentWorkerScopeFailures_CoalesceToOne(t *testing.T) {
	g := newAuthGuard(testBackoffConfig())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	authErr := &ports.MergePRError{Status: 401, Message: "Bad credentials"}

	const armedRepos = 8 // > domainautomerge.MaxAuthFailures (5)
	const simulatedRoundTrip = 20 * time.Millisecond

	// errgroup, not a naked `go` statement -- CLAUDE.md/§11 grants no
	// test-file exemption for this (tools/lint/narvichecks/nakedgoroutine's
	// own doc comment is explicit: "test files are in scope too").
	var eg errgroup.Group
	start := make(chan struct{})
	for i := 0; i < armedRepos; i++ {
		repo := fmt.Sprintf("acme/repo-%d", i)
		eg.Go(func() error {
			<-start
			allowed, res := g.allow(repo, now)
			if !allowed {
				t.Errorf("allow(%s) = false on a fresh worker scope, want true", repo)
				return nil
			}
			// Models "blocks on a real GitHub round trip" -- every
			// goroutine has already passed its own allow() check (and so
			// captured the SAME pre-failure generation) well before this
			// sleep elapses on any of them.
			time.Sleep(simulatedRoundTrip)
			g.recordFailure(repo, authErr, now, res)
			return nil
		})
	}
	close(start)
	_ = eg.Wait()

	g.mu.Lock()
	gotFailures := g.worker.consecutiveFailures
	gotDeadLettered := g.worker.deadLettered
	g.mu.Unlock()

	if gotDeadLettered {
		t.Fatalf("g.worker.deadLettered = true after ONE tick's worth of %d concurrent contemporaneous failures -- want false: a single tick must never walk the whole backoff ladder in one shot", armedRepos)
	}
	if gotFailures != 1 {
		t.Fatalf("g.worker.consecutiveFailures = %d after %d concurrent contemporaneous failures against the SAME pre-failure state, want exactly 1 -- N concurrent reports of the SAME underlying event must coalesce into ONE consecutive failure, not N", gotFailures, armedRepos)
	}
}

// TestAuthGuard_NonClassifyingErrors_NeverTouchState proves an ordinary,
// unrelated failure (rate limit, 5xx, "not mergeable") never perturbs
// authGuard's own bookkeeping at all -- recordFailure must be provably
// inert for every error this fix is not about. classify() short-circuits
// before ever looking at the reservation, so a zero-value
// authReservation{} is fine here regardless of prior state.
func TestAuthGuard_NonClassifyingErrors_NeverTouchState(t *testing.T) {
	g := newAuthGuard(testBackoffConfig())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const repo = "acme/rate-limited-repo"

	rateLimited := &ports.MergePRError{Status: 403, Message: "API rate limit exceeded", RateLimited: true}
	for i := 0; i < domainautomerge.MaxAuthFailures*3; i++ {
		scope, _, failures := g.recordFailure(repo, rateLimited, now, authReservation{})
		if scope != authScopeNone || failures != 0 {
			t.Fatalf("iteration %d: recordFailure(rate-limited 403) = (%v, _, %d), want (authScopeNone, _, 0)", i, scope, failures)
		}
	}
	if allowed, _ := g.allow(repo, now); !allowed {
		t.Error("allow() = false after many rate-limited 403s, want true -- rate limiting must never trip this guard")
	}
}
