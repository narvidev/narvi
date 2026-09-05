// This file (knowledge.go) re-exports the knowledge-retrieval seam
// (technical plan §34.7; docs/design/boundaries-design.md, section 2) a
// module needs to supply its own ranker: the port itself, and the two
// value types Score's own signature carries. See
// internal/app/ports/knowledgeranker.go for the interface's full
// contract -- in particular the "no return channel for a candidate"
// property that makes gate-then-rank structural rather than a convention
// a module has to remember.

package extension

import (
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/knowledge"
)

// KnowledgeRanker is ports.KnowledgeRanker, re-exported as a type alias
// so a module's own ranker implementation -- of any concrete type --
// satisfies the internal interface directly, with no adapter or wrapper
// needed on either side of the module boundary.
type KnowledgeRanker = ports.KnowledgeRanker

// KnowledgeQuery is knowledge.Query, re-exported as a type alias --
// what a module's own KnowledgeRanker receives about the PR under
// review.
type KnowledgeQuery = knowledge.Query

// KnowledgeCandidate is knowledge.Candidate, re-exported as a type alias --
// one already-gated prior architecture decision a module's own
// KnowledgeRanker may reorder, never add to or remove from.
type KnowledgeCandidate = knowledge.Candidate
