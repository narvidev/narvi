// Package automerge holds the pure, I/O-free decision logic
// internal/app/automerge's own authguard.go needs to turn a classified
// authentication/permission failure (ports.ErrAuthenticationFailed/
// ports.ErrPermissionDenied, internal/app/ports) into a backoff schedule
// and, eventually, a terminal (dead-lettered) verdict -- docs/
// TECHNICAL_PLAN.md §17's own automerge dead-letter fix, and §5.1's
// general "exponential backoff + dead-letter after N attempts" outbox-
// pattern requirement applied here to a SECOND outbound path this
// codebase's own audit found lacking either.
//
// This is a deliberate, close mirror of internal/domain/outbox/
// backoff.go's own EvaluateBackoff (which is itself already documented
// as "a deliberate, close mirror of internal/domain/imagebuild/
// backoff.go's own EvaluateBackoff") -- the SAME doubling-capped-at-max
// schedule shape, kept as this package's own independent copy rather
// than a shared import, matching this codebase's own established
// precedent of one small, independently-tunable copy per consuming
// package rather than a single shared implementation multiple unrelated
// call sites would have to agree on evolving in lockstep.
//
// What differs from outbox's own reasoning, and why MaxAuthFailures
// below is deliberately much smaller than outbox.MaxAttempts (10): an
// outbox delivery failure (a flaky Slack/Linear/GitHub API, a transient
// 5xx) is expected to be transient and self-healing given enough time,
// so outbox.MaxAttempts is tuned to comfortably outlast a real, observed
// outage window (that package's own doc comment: "Slack API 500s for 10
// min"). An authentication/permission failure classified by THIS
// package's own caller has no equivalent self-healing story: a bad or
// empty bot token, or a repository that revoked this credential's
// access, does not start working again merely because time passed --
// fixing it requires an operator's own out-of-band action (rotating the
// token and redeploying, or re-granting the repository's own access).
// Backing off at all here is not a bet that the condition will clear on
// its own; it exists ONLY to tolerate a possible misclassification or a
// genuinely momentary blip (a load balancer hiccup that happens to
// answer with a stray 401), never a real expectation of independent
// recovery -- so a handful of attempts is enough to be confident this is
// not noise, well short of outbox's own ten.
package automerge

import "time"

// MaxAuthFailures bounds how many consecutive authentication/permission-
// classified failures internal/app/automerge's own authguard.go
// tolerates, for either the worker-wide or one repository's own scope,
// before EvaluateBackoff reports DeadLetter instead of scheduling
// another retry. Chosen as 5 -- see this file's own top comment for why
// a much smaller figure than domain/outbox.MaxAttempts (10) is the
// correct choice here, not an oversight: with the shipped defaults
// (platform.Timeouts.AutoMergeAuthBackoffBase 2min,
// AutoMergeAuthBackoffMax 12min -- deliberately BELOW the 16min the 4th
// consecutive failure would otherwise double to unclamped, see that
// field's own doc comment for why, chosen precisely so the ceiling has a
// real, demonstrable effect rather than sitting above every value this
// schedule could ever produce and never binding at all), the schedule
// this produces is 2m, 4m, 8m, 12m, dead-letter -- a little over 25
// minutes of tolerating what might still be a transient blip before this
// package's own caller commits to treating it as the durable,
// operator-actionable condition it almost certainly is by then.
const MaxAuthFailures = 5

// BackoffConfig configures EvaluateBackoff's exponential schedule. Both
// fields are populated by the caller from platform.Timeouts (this
// package imports no duration literals of its own -- CLAUDE.md/§11: no
// time.Duration unit literal outside internal/platform, enforced by
// tools/lint/narvichecks/notimeliteral).
type BackoffConfig struct {
	// BaseDelay is the delay scheduled after the FIRST consecutive
	// classified failure. Populated from platform.Timeouts.
	// AutoMergeAuthBackoffBase.
	BaseDelay time.Duration
	// MaxDelay caps the exponential growth -- populated from platform.
	// Timeouts.AutoMergeAuthBackoffMax. Once reached, every subsequent
	// consecutive failure schedules the SAME MaxDelay again (a plateau,
	// not a further increase) -- mirrors domain/outbox.BackoffConfig.
	// MaxDelay's own identical precedent exactly.
	MaxDelay time.Duration
}

// BackoffDecision is EvaluateBackoff's verdict for one classified
// authentication/permission failure.
type BackoffDecision struct {
	// NextRetryAt is when this scope (worker-wide, or one repository) may
	// next attempt a real outbound call again -- meaningless (the zero
	// value) when DeadLetter is true, since a dead-lettered scope is
	// never retried again for the rest of this process's own lifetime
	// (authguard.go's own doc comment: neither a bad global credential
	// nor a per-repository revocation is something this process can
	// observe clearing on its own).
	NextRetryAt time.Time
	// DeadLetter reports whether THIS failed attempt has reached
	// MaxAuthFailures -- true means the caller must mark this scope
	// terminal (authGuard's own deadLettered flag) instead of scheduling
	// another retry.
	DeadLetter bool
}

// EvaluateBackoff computes the next retry time for one scope's own
// consecutive classified failure, or reports that this scope should be
// dead-lettered instead. consecutiveFailures is the TOTAL number of
// classified failures observed for this scope so far, INCLUDING the one
// that just happened (the caller increments it before calling this
// function) -- mirrors domain/outbox.EvaluateBackoff's own identical
// attemptCount convention exactly. consecutiveFailures < 1 is treated as
// 1 (defensive: there is no such thing as a "0th" failure) -- and, unlike
// an earlier loop-based version of this function where that clamp was
// pure dead code (a loop bounded by "i < consecutiveFailures" behaves
// identically for any consecutiveFailures <= 1, so no input could ever
// make the clamp's own effect observable), the shift below makes it
// load-bearing: consecutiveFailures-1 is a shift COUNT, and Go's runtime
// panics on a negative shift count. TestEvaluateBackoff_NegativeOrZero
// (backoff_test.go) is this clamp's own mutation-test witness -- delete
// the clamp and that test panics, rather than merely computing a
// different number.
//
// Doubling per additional failure (BaseDelay, 2×BaseDelay, 4×BaseDelay,
// ...), capped at MaxDelay, is the SAME schedule domain/outbox.
// EvaluateBackoff/domain/imagebuild.EvaluateBackoff already establish
// for the identical §5.1 requirement ("retry with exponential backoff,
// not fixed") -- reused here rather than inventing a third shape for
// what is, at this level, the same problem. consecutiveFailures is
// already bounded to [1, MaxAuthFailures) by the dead-letter check above
// by the time the shift runs, so BaseDelay<<(consecutiveFailures-1) only
// ever needs the delay<=0 guard below for defensive overflow protection
// (a pathological BaseDelay/MaxAuthFailures combination), not for any
// value this package's own callers actually produce.
func EvaluateBackoff(consecutiveFailures int, cfg BackoffConfig, now time.Time) BackoffDecision {
	if consecutiveFailures < 1 {
		consecutiveFailures = 1
	}

	if consecutiveFailures >= MaxAuthFailures {
		return BackoffDecision{DeadLetter: true}
	}

	delay := cfg.BaseDelay << (consecutiveFailures - 1)
	if delay <= 0 || delay > cfg.MaxDelay {
		// delay <= 0 catches signed overflow from the shift above wrapping
		// an int64 duration negative -- treated exactly like "exceeded
		// MaxDelay", the same plateau every other case at or above
		// MaxDelay already gets.
		delay = cfg.MaxDelay
	}

	return BackoffDecision{NextRetryAt: now.Add(delay)}
}
