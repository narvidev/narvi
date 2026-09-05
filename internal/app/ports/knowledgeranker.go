package ports

import (
	"context"

	"github.com/narvidev/narvi/internal/domain/knowledge"
)

// KnowledgeRanker orders the candidates a server-derived gate (a later
// Step's own deterministic, path-scoped SQL SELECT -- never this
// interface, and never any implementation of it) has already admitted.
// §31.6 requires eligibility and ordering to be two separate decisions
// that never merge; §34.7 makes that structural rather than a convention
// an implementer has to remember: Score's ONLY output is one float64 per
// candidate, in the same order it was given them. There is no Candidate
// anywhere in this interface's own RETURN types -- nothing to add one to,
// nothing to drop one from, nothing to re-select through -- so no
// implementation can widen eligibility by itself.
//
// What the signature does NOT close, and a caller must: cands is a slice
// header, so an implementation can rewrite the elements the caller still
// holds. That is content substitution rather than widening, and it is the
// stricter failure of the two, because substituted prose reaches the
// review prompt having bypassed the sanitization applied when the
// decision was written. A caller handing candidates to an implementation
// it does not control passes knowledge.CloneForRanking's output instead
// of its own slices; controlplane's capability-aware wrapper already does
// this at the boundary where a composed module's code begins. The only way to
// even ATTEMPT it is to widen this interface's own declared Score
// signature, which is a reviewable, CI-visible edit to this file (and the
// arch-test that pins it), never a silent one buried in an
// implementation.
//
// Two real implementations are expected, exactly like every other port
// this codebase holds to CLAUDE.md's "an interface must hold for more
// than one implementation" rule: knowledge.RecencyRanker (public, no
// opinion, keeps the gate's own order) and a private hybrid
// lexical+embeddings ranker, entering only through
// extension.Module.KnowledgeRanker and switched in per call by
// controlplane's own capability-aware wrapper -- never by this package,
// which has no notion of licensing at all.
//
// Degradation -- falling back to the gate's own order, never to empty,
// when Score is slow, fails, or returns something knowledge.
// OrderByScores rejects -- is a CALLER's responsibility, never an
// implementation's own (§34.7). platform.Timeouts.KnowledgeRankerTimeout
// exists for a caller to bound a Score call it does not otherwise trust
// to return promptly. An implementation should still honor ctx's own
// deadline/cancellation: it must never assume it will always be given
// unlimited time, even though bounding it is the caller's job, not its
// own.
type KnowledgeRanker interface {
	// Name identifies this ranker (e.g. "recency", or a private mode's
	// own name) -- recorded on the turn (a later Step) so a maintainer
	// reviewing an injected-decisions block can see which ranker actually
	// produced its order.
	Name() string

	// Score returns exactly one score per element of cands, in the same
	// order, or (nil, nil) for "no opinion, keep the caller's own order".
	// q is the current PR's own server-derived view; cands is exactly
	// what the gate admitted -- Score has no way to see, and therefore no
	// way to inject, anything the gate did not already put in cands.
	Score(ctx context.Context, q knowledge.Query, cands []knowledge.Candidate) ([]float64, error)
}

// Compile-time proof that the public product's own ranker satisfies this
// interface -- knowledge.RecencyRanker cannot import this package itself
// (internal/domain must not import internal/app/*), so this is the
// nearest production file that can assert it.
var _ KnowledgeRanker = knowledge.RecencyRanker{}
