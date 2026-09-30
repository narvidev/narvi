// Package outbox holds the pure, I/O-free decision logic for the outbox
// delivery worker (internal/app/outboxworker "outbox delivery",
// §5.1: "a retry worker delivers with exponential backoff + dead-letter
// after N attempts"). No I/O, no time.Now(), no randomness (CLAUDE.md) --
// every input this package's own EvaluateBackoff needs (attempt count,
// backoff config, "now") is supplied by the caller.
//
// This is a deliberate, close mirror of internal/domain/imagebuild/
// backoff.go's own EvaluateBackoff (same doubling-capped-at-max schedule,
// same BackoffConfig{BaseDelay,MaxDelay} shape) plus ONE addition that
// package does not need: a dead-letter decision. image_builds rows retry
// forever (a persistently-broken build fingerprint just keeps circling at
// MaxDelay, alerting via image_build_failure_streak) -- but §5.1's own
// outbox-specific language ("dead-letter after N attempts") requires an
// outbox entry to eventually stop retrying and be given up on, which
// EvaluateBackoff (below) reports via BackoffDecision.DeadLetter.
//
// EvaluateFailure (failure.go) is the worker's one entry point for a failed
// attempt: it decides the failure's class -- counted, on EvaluateBackoff's
// schedule, or deferred, the class that gives the attempt back (§5.1's
// shutdown interruptions, and §44.2's rate limits next) -- and calls
// EvaluateBackoff for every counted one.
package outbox

import "time"

// MaxAttempts is the number of delivery attempts an outbox entry may make
// before EvaluateBackoff reports DeadLetter instead of scheduling another
// retry. Not given an explicit figure in the plan; chosen as 10 --
// generous enough that resilience scenario 9 (§9.3: "Slack API 500s for
// 10 min -> notification eventually delivered, no loss") never dead-letters
// partway through a real 10-minute outage: with the shipped defaults
// (platform.Timeouts.OutboxBackoffBase 30s, OutboxBackoffMax 5min), the
// schedule this produces is 30s, 1m, 2m, 4m, 5m, 5m, 5m, ... -- the first
// 5 attempts alone already span roughly 12.5 minutes of cumulative
// elapsed time before a 6th is even due, comfortably outlasting a 10-minute
// outage with margin to spare, while still giving up eventually on a
// genuinely, permanently broken notifier rather than retrying forever
// (unlike image_builds, an outbox entry names a one-time notification
// whose value decays -- there is no equivalent of "a fingerprint might get
// spawned against again later" to justify infinite retry here).
const MaxAttempts = 10

// BackoffConfig configures EvaluateBackoff's exponential schedule. Both
// fields are populated by the caller from platform.Timeouts (this package
// imports no duration literals of its own).
type BackoffConfig struct {
	// BaseDelay is the delay scheduled after the FIRST failed attempt
	// (attemptCount == 1). Populated from platform.Timeouts.
	// OutboxBackoffBase.
	BaseDelay time.Duration
	// MaxDelay caps the exponential growth -- populated from platform.
	// Timeouts.OutboxBackoffMax. Once reached, every subsequent
	// consecutive failure schedules the SAME MaxDelay again (a plateau,
	// not a further increase), mirroring domain/imagebuild.BackoffConfig.
	// MaxDelay's own identical precedent exactly.
	MaxDelay time.Duration
}

// BackoffDecision is EvaluateBackoff's verdict for one failed delivery
// attempt.
type BackoffDecision struct {
	// NextRetryAt is when this outbox entry becomes eligible to
	// (re)attempt delivery again -- meaningless (zero value) when
	// DeadLetter is true, since a dead-lettered entry is never retried by
	// this worker again.
	NextRetryAt time.Time
	// DeadLetter reports whether THIS failed attempt has reached
	// MaxAttempts -- true means the caller must mark the entry
	// dead_letter (MarkOutboxEntryDeadLetter) instead of scheduling
	// another retry (RecordOutboxEntryFailure).
	DeadLetter bool
}

// EvaluateBackoff computes the next retry time for a delivery attempt that
// just failed, or reports that this outbox entry should be dead-lettered
// instead. attemptCount is the TOTAL number of delivery attempts made
// against this entry so far, INCLUDING the one that just failed (the
// caller increments it at claim time, before the real notifier call --
// mirrors domain/imagebuild.EvaluateBackoff's own identical
// attemptCount convention exactly). attemptCount < 1 is treated as 1
// (defensive: there is no such thing as a "0th" failed attempt).
//
// Doubling per additional failure (BaseDelay, 2×BaseDelay, 4×BaseDelay,
// ...), capped at MaxDelay, is the SAME schedule domain/imagebuild.
// EvaluateBackoff already establishes for the identical §3.5 requirement
// ("retry with exponential backoff, not fixed") -- reused here rather than
// inventing a different shape for what is, at this level, the same
// problem.
func EvaluateBackoff(attemptCount int, cfg BackoffConfig, now time.Time) BackoffDecision {
	if attemptCount < 1 {
		attemptCount = 1
	}

	if attemptCount >= MaxAttempts {
		return BackoffDecision{DeadLetter: true}
	}

	delay := cfg.BaseDelay
	for i := 1; i < attemptCount; i++ {
		if delay >= cfg.MaxDelay {
			delay = cfg.MaxDelay
			break
		}
		delay *= 2
	}
	if delay > cfg.MaxDelay {
		delay = cfg.MaxDelay
	}

	return BackoffDecision{NextRetryAt: now.Add(delay)}
}
