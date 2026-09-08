package automerge

import (
	"errors"
	"fmt"
	"testing"
	"time"

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
	if !g.allow("acme/repo", now) {
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
		if !g.allow(repo, now) {
			t.Fatalf("attempt %d: allow() = false BEFORE this attempt's own failure, want true (still inside an earlier window that should have elapsed)", i)
		}

		scope, target, failures := g.recordFailure(repo, permErr, now)
		if scope != authScopeNone {
			t.Fatalf("attempt %d: recordFailure() scope = %v, want authScopeNone (not yet at MaxAuthFailures=%d)", i, scope, domainautomerge.MaxAuthFailures)
		}
		if failures != 0 {
			// recordFailure only reports a non-zero count on an actual
			// dead-letter transition -- see its own doc comment.
			t.Fatalf("attempt %d: recordFailure() consecutiveFailures = %d, want 0 (not a transition)", i, failures)
		}
		_ = target

		if g.allow(repo, now) {
			t.Fatalf("attempt %d: allow() = true immediately after a classified failure at the SAME now, want false (no backoff applied)", i)
		}

		// Advance now past this attempt's own scheduled backoff window --
		// mirrors domainautomerge.EvaluateBackoff's own doubling schedule
		// exactly, so this loop proves the window computed really does
		// reopen allow(), not merely that SOME large jump does.
		decision := domainautomerge.EvaluateBackoff(i, testBackoffConfig(), now)
		now = decision.NextRetryAt
		if !g.allow(repo, now) {
			t.Fatalf("attempt %d: allow() = false once now reached the scheduled NextRetryAt (%v), want true", i, now)
		}
	}

	// The MaxAuthFailures-th consecutive failure must dead-letter.
	scope, target, failures := g.recordFailure(repo, permErr, now)
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
	if g.allow(repo, farFuture) {
		t.Fatal("allow() = true a full year after dead-lettering, want false -- dead-letter must be permanent, not merely a very long backoff")
	}

	// A further recordFailure call must be a no-op (never re-reports a
	// transition, never double-counts) -- this is what stops
	// worker.recordAuthOutcome from re-auditing/re-alerting on every
	// subsequent skipped tick.
	scope, _, failures = g.recordFailure(repo, permErr, farFuture)
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
		scope, _, _ = g.recordFailure("acme/repo-a", authErr, now)
		decision := domainautomerge.EvaluateBackoff(i, testBackoffConfig(), now)
		now = decision.NextRetryAt
	}
	if scope != authScopeWorker {
		t.Fatalf("final recordFailure() scope = %v, want authScopeWorker", scope)
	}

	if g.allow("acme/repo-a", now) {
		t.Error("allow(repo-a) = true after worker-wide dead-letter, want false")
	}
	if g.allow("acme/repo-that-never-failed", now) {
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
		g.recordFailure("acme/denied-repo", permErr, now)
		decision := domainautomerge.EvaluateBackoff(i, testBackoffConfig(), now)
		now = decision.NextRetryAt
	}

	if g.allow("acme/denied-repo", now) {
		t.Error("allow(denied-repo) = true after its own dead-letter, want false")
	}
	if !g.allow("acme/unrelated-repo", now) {
		t.Error("allow(unrelated-repo) = false after a DIFFERENT repo's own per-repo dead-letter, want true -- per-repo scope must never leak")
	}
}

// TestAuthGuard_RecordSuccess_ResetsWorkerAndThisRepoOnly proves
// recordSuccess's own documented scope: it resets the shared worker-wide
// streak (a real successful call proves the credential works, worker-
// wide) AND this one repo's own streak, but must never clear a
// DIFFERENT repo's own already-accumulated failures.
func TestAuthGuard_RecordSuccess_ResetsWorkerAndThisRepoOnly(t *testing.T) {
	g := newAuthGuard(testBackoffConfig())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	authErr := &ports.MergePRError{Status: 401, Message: "Bad credentials"}
	permErr := &ports.MergePRError{Status: 403, Message: "Resource not accessible by integration"}

	// One worker-wide failure (short of dead-lettering) via repo-a, and
	// one per-repo failure via repo-b.
	g.recordFailure("acme/repo-a", authErr, now)
	g.recordFailure("acme/repo-b", permErr, now)

	if g.allow("acme/repo-a", now) {
		t.Fatal("allow(repo-a) = true immediately after its own failure, want false (still inside backoff)")
	}
	if g.allow("acme/repo-c", now) {
		t.Fatal("allow(repo-c) = true, want false (worker-wide streak from repo-a's own failure must block every repo)")
	}

	// A successful call against repo-a resets the worker-wide streak...
	g.recordSuccess("acme/repo-a")
	if !g.allow("acme/repo-a", now) {
		t.Error("allow(repo-a) = false after recordSuccess(repo-a), want true")
	}
	if !g.allow("acme/repo-c", now) {
		t.Error("allow(repo-c) = false after recordSuccess(repo-a) cleared the worker-wide streak, want true")
	}

	// ...but repo-b's own INDEPENDENT per-repo streak must survive --
	// recordSuccess only ever names one repo.
	if g.allow("acme/repo-b", now) {
		t.Error("allow(repo-b) = true after an UNRELATED repo's own recordSuccess, want false -- per-repo state must not be cleared by a different repo's success")
	}
}

// TestAuthGuard_NonClassifyingErrors_NeverTouchState proves an ordinary,
// unrelated failure (rate limit, 5xx, "not mergeable") never perturbs
// authGuard's own bookkeeping at all -- recordFailure must be provably
// inert for every error this fix is not about.
func TestAuthGuard_NonClassifyingErrors_NeverTouchState(t *testing.T) {
	g := newAuthGuard(testBackoffConfig())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const repo = "acme/rate-limited-repo"

	rateLimited := &ports.MergePRError{Status: 403, Message: "API rate limit exceeded", RateLimited: true}
	for i := 0; i < domainautomerge.MaxAuthFailures*3; i++ {
		scope, _, failures := g.recordFailure(repo, rateLimited, now)
		if scope != authScopeNone || failures != 0 {
			t.Fatalf("iteration %d: recordFailure(rate-limited 403) = (%v, _, %d), want (authScopeNone, _, 0)", i, scope, failures)
		}
	}
	if !g.allow(repo, now) {
		t.Error("allow() = false after many rate-limited 403s, want true -- rate limiting must never trip this guard")
	}
}
