// Package knowledge is the pure domain half of Narvi's knowledge-retrieval
// seam (technical plan §34.7; docs/design/boundaries-design.md, section 2):
// giving a repository a memory of its own prior architecture decisions,
// gated by a server-derived, path-scoped rule and ordered by a swappable
// ranker. Every function here is pure per CLAUDE.md/§11: no I/O, no
// time.Now(), no randomness.
//
// # Gate, then rank -- never the other way around
//
// §31.6 requires that eligibility and ordering never be decided by the
// same mechanism: a deterministic, path-scoped selector -- built entirely
// outside this package, against a SELECT that never imports it -- decides
// which prior decisions may be injected into a review prompt at all (the
// GATE); a KnowledgeRanker (internal/app/ports/knowledgeranker.go) decides
// only their order (the RANK). §34.7 makes that split structural rather
// than a convention an implementer has to remember: a KnowledgeRanker's
// Score method returns one float64 per already-gated Candidate and
// nothing else -- no Candidate return, no way to widen, narrow, or
// reorder the SET of candidates, only their sequence. OrderByScores,
// the only function in this codebase that turns scores into an order,
// builds its result by indexing into the caller's own []Candidate, so it
// is structurally incapable of returning an element the gate did not
// already admit.
//
// # What lives here, and what does not yet
//
// This package holds exactly the pieces a ranker (and the ranker's own
// caller) needs, so that the gate itself can be built against a finished
// seam rather than an imagined one: Candidate and Query (what a ranker
// sees), MaxInjected (the cap applied AFTER ordering, never something a
// ranker can raise), OrderByScores and TakeTop (the two pure operations
// every ranker's output feeds through, regardless of which ranker
// produced it), and RecencyRanker (the product's own public ranker -- no
// opinion of its own, it always keeps whatever order the gate already
// produced).
//
// Deliberately absent: the SQL gate itself, the impure fetch/render pair
// that calls a KnowledgeRanker and renders its result into a prompt block,
// the durable record of which candidates were actually injected, and the
// call sites that prepend that block to a review turn's own prompt. Those
// belong to a later Step, building the gate and its consumers against this
// already-finished seam rather than inventing the seam under deadline.
package knowledge
