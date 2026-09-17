// Package reviewcheck is the publisher's own pure domain: what a
// narvi/review GitHub check run should say (§21.1b), computed from an
// emission -- the phase a review is in, and the attempt/context it was
// produced for -- with no I/O, no time.Now(), no randomness (CLAUDE.md/
// §11). Two questions live here, and nowhere else:
//
//  1. Given a Phase (below), what GitHub status/conclusion pair, title
//     and summary express it? See ComputeOutput's own doc comment for
//     the full state -> (status, conclusion) mapping table -- this is
//     "where a reader will find it" for that mapping.
//  2. Given the emission ALREADY PUBLISHED for a pull request and a
//     CANDIDATE emission about to be published, does the candidate win?
//     See Supersedes' own doc comment.
//
// # Why this is not part of internal/domain/reviewverdict
//
// reviewverdict.Record/Context are the ANALYTICS/ELIGIBILITY read model
// (§21.1): a durable, append-only history of what a review concluded.
// This package is a DIFFERENT thing -- the narrow, current-state-only
// question "what does the check say RIGHT NOW" -- with its own identity
// (one external check-run per PR, rotating as the head moves) and its
// own supersession rule (an attempt/phase ordering, not a verdict
// history). Reusing reviewverdict.Context's shape for BaseRef/BaseSHA/
// PolicyVersion (Emission below) is deliberate -- the same fields, the
// same meaning -- but Emission is its own type: this package must never
// import reviewverdict (a verdict-history concept) merely to describe a
// check-run.
//
// # Why a check run's identity is not "one per verdict"
//
// A GitHub check run is scoped to ONE commit SHA (GitHub shows only the
// check runs that exist for a PR's CURRENT head on its Checks tab) --
// unlike review_verdicts, which is append-only and keeps every historical
// row. The publisher's own claim table (internal/adapters/outbound/
// postgres.ReviewCheckRunStore, migrations/
// 000132_review_check_runs.up.sql) therefore keeps exactly ONE row per
// (repo_full_name, pr_number): "two active identities for one pull
// request is worse than none" (§21.1b). See that migration's own doc
// comment for the full claim design, and internal/app/outboxworker's own
// review-check notifier for how a Phase transition decides whether the
// SAME external check run is updated in place or a NEW one is opened.
package reviewcheck
