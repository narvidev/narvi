package reviewcheck

import (
	"testing"
	"time"
)

var (
	t1 = time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)
)

// TestSupersedes_NothingPublishedYet: the zero Emission (nothing ever
// published for this pull request) always loses to a real candidate.
func TestSupersedes_NothingPublishedYet(t *testing.T) {
	var current Emission
	candidate := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: PhaseQueued}
	if !Supersedes(current, candidate) {
		t.Fatal("a fresh (never-published) row must always be superseded by a real candidate")
	}
}

// TestSupersedes_OlderAttemptLosesToNewerAttempt is the core identity
// test the brief asks for: "an older emission with a matching head must
// still lose to a newer attempt" (§21.1b), regardless of which one's own
// emission reaches Supersedes first -- a publisher keying on head alone
// would apply whichever arrived last; this one must not.
func TestSupersedes_OlderAttemptLosesToNewerAttempt(t *testing.T) {
	// current: the NEWER attempt already published (e.g. delivered
	// first, out of order, by outbox retry/backoff).
	current := Emission{
		RepoFullName: "acme/widgets", PRNumber: 7, HeadSHA: "deadbeef",
		AttemptID: "attempt-newer", AttemptCreatedAt: t2, Phase: PhaseTerminalAssessed,
	}
	// candidate: the OLDER attempt's own emission, arriving after.
	candidate := Emission{
		RepoFullName: "acme/widgets", PRNumber: 7, HeadSHA: "deadbeef",
		AttemptID: "attempt-older", AttemptCreatedAt: t1, Phase: PhaseTerminalAssessed,
	}
	if Supersedes(current, candidate) {
		t.Fatal("an older attempt's emission must never supersede an already-published newer attempt, even with a matching head")
	}
}

// TestSupersedes_NewerAttemptWinsOverOlder is the mirror image: a
// genuinely newer attempt must win even though it is compared AFTER an
// older one is already recorded as current -- the ordinary, expected
// case a review restarting produces.
func TestSupersedes_NewerAttemptWinsOverOlder(t *testing.T) {
	current := Emission{AttemptID: "attempt-older", AttemptCreatedAt: t1, Phase: PhaseTerminalAssessed}
	candidate := Emission{AttemptID: "attempt-newer", AttemptCreatedAt: t2, Phase: PhaseQueued}
	if !Supersedes(current, candidate) {
		t.Fatal("a newer attempt must supersede an older, already-terminal one")
	}
}

// TestSupersedes_SameAttemptPhaseProgressionAllowed: Queued -> Running ->
// Terminal, all for the SAME attempt, each supersedes the last.
func TestSupersedes_SameAttemptPhaseProgressionAllowed(t *testing.T) {
	queued := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: PhaseQueued}
	running := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: PhaseRunning}
	terminal := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: PhaseTerminalAssessed}

	if !Supersedes(queued, running) {
		t.Error("running must supersede queued for the same attempt")
	}
	if !Supersedes(running, terminal) {
		t.Error("terminal must supersede running for the same attempt")
	}
}

// TestSupersedes_SameAttemptPhaseRegressionRefused: a redelivered,
// stale "running" emission arriving AFTER a terminal result for the
// SAME attempt must never resurrect it.
func TestSupersedes_SameAttemptPhaseRegressionRefused(t *testing.T) {
	terminal := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: PhaseTerminalAssessed}
	staleRunning := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: PhaseRunning}
	if Supersedes(terminal, staleRunning) {
		t.Fatal("a redelivered running emission must never supersede an already-terminal result for the same attempt")
	}
}

// TestSupersedes_SameAttemptSameRankAllowsIdempotentOverwrite: an exact
// redelivery, or a same-rank correction between Stale and
// TerminalNotAssessed for the identical attempt, must still be allowed
// -- never refused merely for matching rank. (Before finding A8's own
// fix, this same test used TerminalAssessed -> TerminalNotAssessed as
// its own example of an allowed "correction" -- that was the defect
// itself: see TestSupersedes_SameAttemptTerminalAssessedNeverDisplacedByADifferentPhase
// below for why a REAL, already-posted success is not a "correction"
// candidate in the same sense Stale/TerminalNotAssessed are for each
// other.)
func TestSupersedes_SameAttemptSameRankAllowsIdempotentOverwrite(t *testing.T) {
	current := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: PhaseTerminalNotAssessed}
	candidate := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: PhaseStale}
	if !Supersedes(current, candidate) {
		t.Fatal("a same-rank, same-attempt candidate must be allowed to apply (idempotent redelivery / correction)")
	}
}

// TestSupersedes_SameAttemptTerminalAssessedRedeliveryAllowed: an exact
// redelivery of PhaseTerminalAssessed for the identical attempt (e.g.
// republished display text, or a plain outbox retry) is still allowed --
// finding A8's fix narrows same-rank overwrites OUT of an
// already-assessed current, never idempotent redelivery of the SAME
// phase.
func TestSupersedes_SameAttemptTerminalAssessedRedeliveryAllowed(t *testing.T) {
	current := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: PhaseTerminalAssessed}
	candidate := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: PhaseTerminalAssessed}
	if !Supersedes(current, candidate) {
		t.Fatal("an idempotent redelivery of PhaseTerminalAssessed for the same attempt must still be allowed")
	}
}

// TestSupersedes_SameAttemptTerminalAssessedNeverDisplacedByADifferentPhase
// is finding A8's own fix: once PhaseTerminalAssessed is current for an
// attempt (a real review.Verdict was posted), no same-attempt candidate
// other than ANOTHER PhaseTerminalAssessed may ever displace it -- not
// Stale, not TerminalNotAssessed, even though both share
// PhaseTerminalAssessed's own rank and would otherwise pass the
// same-rank-is-allowed rule. Before this fix, a same-attempt
// TerminalNotAssessed (or Stale) emission -- reachable via a redelivery
// race, never through the normal enqueue paths this codebase ships
// today, but not excluded by Supersedes itself -- could silently replace
// a real, already-posted success with "Review not completed".
func TestSupersedes_SameAttemptTerminalAssessedNeverDisplacedByADifferentPhase(t *testing.T) {
	current := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: PhaseTerminalAssessed}
	for _, p := range []Phase{PhaseTerminalNotAssessed, PhaseStale} {
		candidate := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: p}
		if Supersedes(current, candidate) {
			t.Errorf("a same-attempt %s candidate must never displace an already-posted PhaseTerminalAssessed/success", p)
		}
	}
}

// TestSupersedes_QueuedNeverSupersedesARealAttempt: a Queued emission
// (AttemptID == "", the zero time) enqueued for "PR enters scope" must
// never resurrect over a real, in-flight or concluded attempt -- even if
// its own outbox row is redelivered late, well after a real attempt has
// already published.
func TestSupersedes_QueuedNeverSupersedesARealAttempt(t *testing.T) {
	current := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: PhaseRunning}
	staleQueued := Emission{AttemptID: "", Phase: PhaseQueued}
	if Supersedes(current, staleQueued) {
		t.Fatal("a queued emission with no attempt must never supersede a real, already-published attempt")
	}
}

// TestSupersedes_InvalidCandidatePhaseAlwaysRefused: this package's own
// caller must never publish a Phase it cannot compute an Output for --
// Supersedes refuses it unconditionally, even against a virgin row.
func TestSupersedes_InvalidCandidatePhaseAlwaysRefused(t *testing.T) {
	var current Emission
	candidate := Emission{AttemptID: "attempt-a", AttemptCreatedAt: t1, Phase: Phase("bogus")}
	if Supersedes(current, candidate) {
		t.Fatal("an invalid candidate phase must never be reported as superseding, even a virgin row")
	}
}
