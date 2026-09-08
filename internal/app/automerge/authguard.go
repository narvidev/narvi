package automerge

import (
	"errors"
	"sync"
	"time"

	"github.com/narvidev/narvi/internal/app/ports"
	domainautomerge "github.com/narvidev/narvi/internal/domain/automerge"
)

// This file implements docs/TECHNICAL_PLAN.md §17's own automerge
// dead-letter fix: "the automerge worker has no dead-letter and no
// backoff for an authentication failure... every other outbound path in
// this repository either dead-letters or backs off; this one does
// neither". §5.1's own general outbox-pattern requirement
// ("a retry worker delivers with exponential backoff + dead-letter after
// N attempts") is what internal/app/outboxworker's own recordFailure
// (builder.go) already implements for notification delivery -- authGuard
// is that SAME shape, reused rather than reinvented, for a completely
// different outbound path this worker owns: a GitHub call authenticated
// with the deployment's single, statically-configured bot credential
// (Worker.deps.BotToken).
//
// The one structural difference from outbox's own backoff, and why this
// is an in-memory authGuard rather than a Postgres-backed retry queue
// like outbox_entries: automerge has no durable, per-candidate retry row
// of its own to attach a next_attempt_at column to -- worker.go's own top
// comment is explicit that every tick REDISCOVERS candidates fresh from
// review_verdicts, never replaying a queued item. What is being backed
// off/dead-lettered here is not one candidate PR, it is the CREDENTIAL's
// own ability to act -- either worker-wide (ports.ErrAuthenticationFailed:
// the bot token itself is bad, which is true for every repo this worker
// touches, since they all share the one token) or scoped to a single
// repository (ports.ErrPermissionDenied: this repository specifically
// denies the token, which says nothing about any other repository).
// Once dead-lettered, a scope stays dead-lettered for the rest of this
// process's own lifetime: a statically-configured bot token cannot start
// working again without a redeploy (a fixed environment variable, which
// this process could not observe anyway without restarting), and a
// per-repository revocation has no signal this worker could poll for
// short of the exact GitHub call this dead-letter exists to stop making.
type authGuard struct {
	mu      sync.Mutex
	worker  authFailureState
	perRepo map[string]*authFailureState
	cfg     domainautomerge.BackoffConfig
}

// authFailureState is one scope's own (worker-wide, or a single
// repository's) consecutive-classified-failure bookkeeping.
type authFailureState struct {
	consecutiveFailures int
	deadLettered        bool
	nextAttemptAt       time.Time
}

// authScope names which of authGuard's two independent failure states a
// classified error updates.
type authScope int

const (
	// authScopeNone means err did not classify as either sentinel below
	// -- authGuard leaves every counter untouched (rate limits, 5xxs,
	// "not mergeable", a stale head SHA, etc. are all unrelated
	// conditions with their own existing handling, unchanged by this
	// fix).
	authScopeNone authScope = iota
	// authScopeWorker is ports.ErrAuthenticationFailed's own scope -- see
	// that sentinel's doc comment (internal/app/ports/authfailure.go) for
	// why a 401 against Worker.deps.BotToken is always worker-wide.
	authScopeWorker
	// authScopeRepo is ports.ErrPermissionDenied's own scope -- a 403
	// GitHub's own rate-limit/abuse-detection classification
	// (isRateLimitedResponse, githubapi) has already ruled out, denying
	// this ONE repository specifically.
	authScopeRepo
)

func (s authScope) String() string {
	switch s {
	case authScopeWorker:
		return "worker"
	case authScopeRepo:
		return "repo"
	default:
		return "none"
	}
}

// newAuthGuard builds an authGuard backed by cfg -- populated by the
// caller from platform.Timeouts.AutoMergeAuthBackoffBase/
// AutoMergeAuthBackoffMax (this package imports no duration literals of
// its own -- CLAUDE.md/§11, enforced by tools/lint/narvichecks/
// notimeliteral).
func newAuthGuard(cfg domainautomerge.BackoffConfig) *authGuard {
	return &authGuard{perRepo: make(map[string]*authFailureState), cfg: cfg}
}

// allow reports whether repoFullName may attempt a real outbound GitHub
// call right now -- false when EITHER the worker-wide state OR this
// repository's own state is dead-lettered, or either is still cooling
// down inside its own backoff window. Checked once per candidate
// (mergeCandidate, worker.go), immediately before RevalidateForAutoMerge
// -- the one call this fix must stop from firing at "full rate" once a
// failure has been classified (worker.go's own top-of-file doc comment
// citing docs/TECHNICAL_PLAN.md §17).
func (g *authGuard) allow(repoFullName string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !stateReady(&g.worker, now) {
		return false
	}
	if repo, ok := g.perRepo[repoFullName]; ok && !stateReady(repo, now) {
		return false
	}
	return true
}

func stateReady(s *authFailureState, now time.Time) bool {
	if s.deadLettered {
		return false
	}
	return !now.Before(s.nextAttemptAt)
}

// recordSuccess resets BOTH the worker-wide streak and repoFullName's own
// per-repository streak: a real, successful authenticated GitHub call
// against repoFullName is direct evidence that Worker.deps.BotToken
// itself currently works (worker-wide, per authScopeWorker's own doc
// comment) AND that repoFullName specifically still grants it access.
// It never touches a DIFFERENT repository's own state -- a per-repository
// denial for repo B is not evidence about repo B just because repo A's
// own call happened to succeed.
//
// Called ONLY after a genuinely successful MergePR (worker.go's own
// mergeCandidate) -- deliberately NOT after RevalidateForAutoMerge's own
// success. A read-only GetOpenPR call succeeding is real evidence
// Worker.deps.BotToken can still read, but it says nothing about whether
// it can still WRITE (merge) to repoFullName specifically -- the exact
// distinction ports.ErrPermissionDenied's own repo-scope exists to draw.
// Resetting on a mere read success would let every OTHER eligible
// candidate's own successful revalidate silently erase THIS repo's own
// accumulating MergePR-failure streak on every tick (confirmed empirically:
// the first version of this fix did exactly that, and
// domainautomerge.MaxAuthFailures was never reachable as a result --
// worker_integration_test.go's own two auth-dead-letter tests are what
// caught it). Only the strongest available evidence -- a merge that
// actually went through -- resets either streak.
func (g *authGuard) recordSuccess(repoFullName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.worker = authFailureState{}
	delete(g.perRepo, repoFullName)
}

// recordFailure classifies err against ports.ErrAuthenticationFailed/
// ports.ErrPermissionDenied (errors.Is, so it sees through both
// *ports.MergePRError and githubapi.APIError's own Unwrap chains
// identically -- internal/app/ports/authfailure.go's own doc comment).
// Any OTHER error (a rate-limited 403, a transient 5xx, a plain
// transport failure, "not mergeable"/409, etc.) leaves every counter
// here untouched and reports authScopeNone -- recordFailure is this
// classifier's own state-update half, not a general-purpose error
// handler.
//
// Returns the scope a NEW transition into a dead-lettered state just
// happened for (authScopeNone if err did not classify, or if it
// classified but this scope was already dead-lettered/not yet at
// MaxAuthFailures), the target this failure applies to (repoFullName for
// authScopeRepo, "" for authScopeWorker -- worker.go's own audit-log
// call uses this as detail_json's own repo_full_name), and the
// consecutive-failure count at the moment of transition. The caller
// (worker.go) uses a true transition to fire the one-time, durable
// audit_log row (docs/TECHNICAL_PLAN.md §17.5's own "no actor" audit
// precedent) that is this package's own answer to "how does an operator
// learn" -- never re-fired on every subsequent already-dead-lettered
// tick, mirroring internal/app/imagebuild's own permanentlyFailed
// counter precedent ("fires exactly ONCE per fingerprint... an operator
// dashboard can alert on it without the 'why does this keep paging me'
// confusion a repeating trip for the same, un-clearable condition would
// cause").
func (g *authGuard) recordFailure(repoFullName string, err error, now time.Time) (scope authScope, target string, consecutiveFailures int) {
	scope = classify(err)
	if scope == authScopeNone {
		return authScopeNone, "", 0
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	var state *authFailureState
	switch scope {
	case authScopeWorker:
		state = &g.worker
	case authScopeRepo:
		repo, ok := g.perRepo[repoFullName]
		if !ok {
			repo = &authFailureState{}
			g.perRepo[repoFullName] = repo
		}
		state = repo
		target = repoFullName
	}

	if state.deadLettered {
		// Already terminal -- no further bookkeeping, and never reported
		// as a NEW transition (the caller must not re-audit-log/re-alert
		// on every subsequent tick, see this method's own doc comment).
		return authScopeNone, "", 0
	}

	state.consecutiveFailures++
	decision := domainautomerge.EvaluateBackoff(state.consecutiveFailures, g.cfg, now)
	state.nextAttemptAt = decision.NextRetryAt
	consecutiveFailures = state.consecutiveFailures
	if !decision.DeadLetter {
		return authScopeNone, "", 0
	}
	state.deadLettered = true
	return scope, target, consecutiveFailures
}

// classify maps err to the scope its Unwrap chain resolves to, via
// ports.ErrAuthenticationFailed/ports.ErrPermissionDenied -- the ONE
// place this package decides what "an authentication failure" is, reused
// by recordFailure above rather than duplicated at each of
// mergeCandidate's own two call sites (worker.go).
func classify(err error) authScope {
	switch {
	case errors.Is(err, ports.ErrAuthenticationFailed):
		return authScopeWorker
	case errors.Is(err, ports.ErrPermissionDenied):
		return authScopeRepo
	default:
		return authScopeNone
	}
}
