// Package session implements session-status derivation (§3.1) — NOT an
// independent Transition(from, trigger) state machine like
// internal/domain/sandbox or internal/domain/turn. §3.1 states the rule
// directly: "Status is derived after each turn: pending work → active;
// else terminal per last turn outcome." A session has no triggers of its
// own to react to; its status is a pure RECOMPUTATION over its turns'
// outcomes, so this package exposes DeriveStatus (status.go) instead of a
// Transition table.
//
// DeriveStatus takes an ordered (oldest-first — the same convention
// internal/domain/turn's queue functions use) slice of turn.Summary for
// one session:
//
//   - Zero turns: Created.
//   - Any turn still non-terminal (Pending/Dispatched/Processing):
//     Active, no failure reason — regardless of what any OTHER turn's
//     outcome was; there is pending work.
//   - All turns terminal: derive from the LAST (most recent) turn's
//     outcome. Completed → Completed, no failure reason. Failed or
//     Cancelled → Failed/Cancelled respectively, with FailureReason taken
//     directly from that turn's Summary.FailureReason (itself produced by
//     turn.DeriveFailureReason) — this package does not re-derive the
//     failure reason from scratch, it reuses turn's mapping verbatim.
//
// FailureReason here is a type ALIAS of turn.FailureReason (not a new,
// separate type), and the four exported constants below simply re-export
// turn's, so this package's public API stays self-describing (callers
// don't need to import turn just to name a session failure reason) while
// the underlying values are, and remain, identical — no conversion is
// ever needed between the two packages' failure reasons.
//
// internal/domain/session imports internal/domain/turn. This is expected,
// not a layering violation of §1's "domain has zero external
// dependencies": that clause is about domain having no dependency on
// adapters/platform/infrastructure. Depending on a SIBLING pure domain
// package is fine, and here it's the whole point — session's job is
// defined purely in terms of turn outcomes.
//
// Archived is carried on the Session struct as a plain bool. It is an
// orthogonal flag toggled independently of status derivation (§3.1
// parenthetically lists it alongside the status enum but gives it no
// transition rule of its own) — this package invents no transition logic
// for it.
//
// DeriveActivity (activity.go) is a second, read-time derivation, not a
// second store: Status is persisted only when a turn reaches a terminal
// state, never when one is queued or dispatched, so it does not tell
// whether a follow-up turn is queued or running.
// Activity (queued, running, delivering, scheduled, awaiting_approval,
// idle, finished) is derived from the turn queue, a completed turn's push
// and pull request still under way, server-side work armed to create a
// turn with no new input, and the session's human gates, all read in one
// snapshot, whenever a client asks (technical plan §43.20), and is never
// written back. SuggestedReadDelay maps it to how long a client should
// wait before asking again.
package session
