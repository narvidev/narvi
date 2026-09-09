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

	// generation is bumped every time recordFailure actually increments
	// consecutiveFailures for THIS scope, and every time recordSuccess
	// resets the worker-wide scope -- NEVER reused/rewound, so it is safe
	// as a version stamp across this scope's entire process lifetime, not
	// merely between one reset and the next (no ABA hazard: a stale
	// caller's own observed generation can never coincidentally match a
	// LATER generation the same numeric value once already passed
	// through). See authReservation's own doc comment for what reads it.
	generation uint64
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

// authReservation is what allow returns alongside its bool verdict when a
// real outbound GitHub call is permitted: the exact generation this
// caller OBSERVED for the worker-wide scope (and, when repoFullName
// already had per-repo state at check time, that scope's own generation
// too) -- carried back into recordFailure so a caller reporting an
// outcome can tell whether ANOTHER concurrent caller has ALREADY
// recorded a newer outcome for the SAME scope since this reservation was
// taken.
//
// This -- not merely holding g.mu across the whole outbound call, which
// recordFailure already serializes internally -- is what actually closes
// docs/TECHNICAL_PLAN.md §17's own concurrency finding. PumpOnce's own
// errgroup (worker.go) fans every armed repo out concurrently, one
// goroutine per repo, each calling allow() with the SAME now. Every
// goroutine's own allow() call clears in microseconds and then blocks on
// a REAL GitHub round trip -- so with >= domainautomerge.MaxAuthFailures
// armed repos sharing one worker-wide scope, ALL of them observe the
// SAME pre-failure generation before any single one has reported back.
// Serializing the outbound calls themselves behind a single lock would
// fix the counting but silently undo PumpOnce's own documented "fans
// them out" concurrent design for EVERY tick, healthy or not -- not just
// during an actual incident. Coalescing at report time, via this
// generation stamp, fixes the ACTUAL bug (N concurrent reports of the
// SAME underlying event -- one broken/rotated credential, one instant --
// must collapse into ONE consecutive failure, not N) without touching
// the happy path's own concurrency at all: the first concurrent caller
// to reach recordFailure for a given generation is the one whose report
// counts; every other caller reporting against that SAME
// now-superseded generation is stale evidence about a state that has
// already moved on, and is discarded rather than double-counted.
//
// Deliberately NOT threaded through recordSuccess: a genuinely
// successful call is this package's OWN strongest possible evidence
// (recordSuccess's doc comment) and must reset state regardless of
// whether some OTHER concurrent caller's report landed first -- gating
// success on a stale reservation would risk discarding fresh, current
// proof the credential works. recordSuccess still cannot un-latch an
// ALREADY-dead-lettered scope (its own doc comment covers that
// separately).
type authReservation struct {
	workerGeneration uint64
	repoGeneration   uint64
}

// allow reports whether repoFullName may attempt a real outbound GitHub
// call right now -- false when EITHER the worker-wide state OR this
// repository's own state is dead-lettered, or either is still cooling
// down inside its own backoff window. Checked once per candidate
// (mergeCandidate, worker.go), immediately before RevalidateForAutoMerge
// -- the one call this fix must stop from firing at "full rate" once a
// failure has been classified (worker.go's own top-of-file doc comment
// citing docs/TECHNICAL_PLAN.md §17). The returned authReservation must
// be threaded back into recordFailure by the SAME caller reporting this
// SAME attempt's own outcome -- see that type's own doc comment for why.
func (g *authGuard) allow(repoFullName string, now time.Time) (bool, authReservation) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !stateReady(&g.worker, now) {
		return false, authReservation{}
	}
	res := authReservation{workerGeneration: g.worker.generation}
	if repo, ok := g.perRepo[repoFullName]; ok {
		if !stateReady(repo, now) {
			return false, authReservation{}
		}
		res.repoGeneration = repo.generation
	}
	return true, res
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
//
// A scope that is ALREADY dead-lettered is left completely untouched --
// authFailureState.deadLettered's own doc intent ("a scope stays
// dead-lettered for the rest of this process's own lifetime", this
// file's own top comment) means recordSuccess must never be the thing
// that un-latches it. Concretely reachable even though allow() already
// gates every call BEFORE it goes out: PumpOnce's own errgroup can have
// several goroutines mid-flight, past allow(), from BEFORE a dead-letter
// transition landed; if one of those in-flight calls happens to succeed
// (a flapping/rotating credential, this file's own top comment) AFTER
// another one's failure already dead-lettered the scope, that late
// success must not resurrect a scope this package has already committed
// to treating as terminal -- resurrecting it would resume "full-rate"
// outbound calls against a scope an operator has just been told, via the
// one-time audit_log row, is permanently dead, reproducing the exact
// noise (a fresh audit row/Error log on every subsequent failure) that
// row exists to be the ONE time it happens.
func (g *authGuard) recordSuccess(repoFullName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.worker.deadLettered {
		// generation is preserved (bumped, not zeroed) across this reset
		// -- see authFailureState.generation's own doc comment for why a
		// PLAIN zero-value reset here would reopen an ABA hazard for any
		// recordFailure call still holding a reservation from before this
		// success landed.
		g.worker = authFailureState{generation: g.worker.generation + 1}
	}
	if repo, ok := g.perRepo[repoFullName]; ok && !repo.deadLettered {
		// Per-repository state is deleted outright, not reset in place --
		// safe against the SAME ABA hazard only because pumpRepo (worker.go)
		// processes one repo's own candidates strictly sequentially, so no
		// OTHER goroutine can be holding a stale reservation for this SAME
		// repoFullName at the moment this delete happens (unlike the
		// worker-wide scope above, which every armed repo's own goroutine
		// can reach concurrently).
		delete(g.perRepo, repoFullName)
	}
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
// happened for (authScopeNone if err did not classify, if it classified
// but this scope was already dead-lettered/not yet at MaxAuthFailures,
// or if res is STALE -- see below), the target this failure applies to
// (repoFullName for authScopeRepo, "" for authScopeWorker -- worker.go's
// own audit-log call uses this as detail_json's own repo_full_name), and
// the consecutive-failure count at the moment of transition. The caller
// (worker.go) uses a true transition to fire the one-time, durable
// audit_log row (docs/TECHNICAL_PLAN.md §17.5's own "no actor" audit
// precedent) that is this package's own answer to "how does an operator
// learn" -- never re-fired on every subsequent already-dead-lettered
// tick, mirroring internal/app/imagebuild's own permanentlyFailed
// counter precedent ("fires exactly ONCE per fingerprint... an operator
// dashboard can alert on it without the 'why does this keep paging me'
// confusion a repeating trip for the same, un-clearable condition would
// cause").
//
// res is the EXACT authReservation the caller's own earlier allow() call
// returned for this SAME attempt. If the relevant scope's own current
// generation no longer matches what res observed, this failure is stale:
// another concurrent caller has ALREADY recorded an outcome (failure or
// success) for this SAME scope since this reservation was taken, so this
// report is discarded rather than incremented -- see authReservation's
// own doc comment (allow, above) for why this, not a coarser lock, is
// what keeps N concurrent contemporaneous failures against the SAME
// pre-failure state from being counted as N separate consecutive
// failures.
func (g *authGuard) recordFailure(repoFullName string, err error, now time.Time, res authReservation) (scope authScope, target string, consecutiveFailures int) {
	scope = classify(err)
	if scope == authScopeNone {
		return authScopeNone, "", 0
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	var state *authFailureState
	var observedGeneration uint64
	switch scope {
	case authScopeWorker:
		state = &g.worker
		observedGeneration = res.workerGeneration
	case authScopeRepo:
		repo, ok := g.perRepo[repoFullName]
		if !ok {
			repo = &authFailureState{}
			g.perRepo[repoFullName] = repo
		}
		state = repo
		target = repoFullName
		observedGeneration = res.repoGeneration
	}

	if state.deadLettered {
		// Already terminal -- no further bookkeeping, and never reported
		// as a NEW transition (the caller must not re-audit-log/re-alert
		// on every subsequent tick, see this method's own doc comment).
		return authScopeNone, "", 0
	}

	if state.generation != observedGeneration {
		// Stale: this report reflects a pre-failure (or pre-success)
		// state ANOTHER concurrent caller has already superseded --
		// discarding it here, rather than incrementing anyway, is the
		// actual fix for docs/TECHNICAL_PLAN.md §17's own concurrency
		// finding (authReservation's own doc comment, allow above).
		return authScopeNone, "", 0
	}

	state.consecutiveFailures++
	state.generation++
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
