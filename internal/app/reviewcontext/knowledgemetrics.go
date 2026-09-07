package reviewcontext

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// This file is §31.2's own "instrumented trigger" -- the per-source token
// gauge: "A token gauge on the rendered knowledge block, emitted at the
// existing render site, split into three numbers: the false-positive-
// pattern block, the arch-decisions block, and the total... The split is
// load-bearing, not cosmetic... a single total gauge, alerted-on and
// flipped-on, therefore does not move after the flip -- the exact
// operator flapping this correction exists to prevent." Emitted from THIS
// package (the impure fetch layer both blocks are already assembled in)
// rather than internal/domain/review.RenderTurnPrompt, the actual "render
// site" prose: that function is pure per CLAUDE.md/§11 and must never
// carry an OTel side effect. Every one of this Step's three review-turn
// producers already has both block strings in hand at the exact point it
// is about to call RenderTurnPrompt -- see RecordKnowledgeBlockTokens'
// own call sites (internal/adapters/inbound/github/handler.go,
// internal/adapters/inbound/httpapi/reviewretrigger.go,
// internal/app/sessionactor/reviewretrigger.go).

// knowledgeMetricsMeterName mirrors this codebase's own "narvi/<package>"
// OTel meter-name convention (e.g. sessionactor's "narvi/sessionactor",
// reconciler's "narvi/reconciler").
const knowledgeMetricsMeterName = "narvi/reviewcontext"

// knowledgeBlockComponent is the one attribute knowledgeBlockTokens is
// tagged by -- §31.2's own three numbers, as one histogram with three
// label values rather than three separate instruments, so a dashboard can
// slice by component without a metrics-registry change every time a
// fourth knowledge source (never planned, but never assumed impossible
// either) is added.
type knowledgeBlockComponent string

const (
	componentFalsePositivePatterns knowledgeBlockComponent = "false_positive_patterns"
	componentArchDecisions         knowledgeBlockComponent = "arch_decisions"
	componentTotal                 knowledgeBlockComponent = "total"
)

var (
	knowledgeBlockTokensOnce sync.Once
	knowledgeBlockTokens     metric.Int64Histogram
)

// tokensHistogram lazily constructs knowledgeBlockTokens against the
// global MeterProvider (platform.SetupOTel's own target) exactly once,
// mirroring registry.go's own contractDriftDetected precedent one
// package over. A construction failure (rare -- a duplicate-registration
// conflict is the realistic case) leaves knowledgeBlockTokens at its nil
// interface zero value; RecordKnowledgeBlockTokens checks for that
// explicitly before ever calling Record, since a nil metric.Int64Histogram
// panics on any method call, unlike a nil pointer with nil-safe methods.
func tokensHistogram() metric.Int64Histogram {
	knowledgeBlockTokensOnce.Do(func() {
		h, err := otel.Meter(knowledgeMetricsMeterName).Int64Histogram(
			"knowledge_block_tokens_estimated",
			metric.WithDescription("Estimated token count of the knowledge block(s) prepended to a review turn's own prompt (§31.2), tagged by component (false_positive_patterns/arch_decisions/total) -- a health metric on false_positive_patterns (a maintainer-taught corpus growing unbounded is a real teaching-hygiene signal), never a mode-A/B indicator for any component: every component is bounded in both modes, so no threshold on this metric can correctly indicate relevance exhaustion -- see the §26.5 contestation KPI for that signal instead. The estimate is len(block)/4 (a widely used, coarse bytes-per-token approximation for English prose) -- this is NOT a real tokenizer count and must never be read as one."),
			metric.WithUnit("{token}"),
		)
		if err != nil {
			return
		}
		knowledgeBlockTokens = h
	})
	return knowledgeBlockTokens
}

// estimateTokens approximates block's own token count as len(block)/4 --
// a coarse, widely used heuristic for English prose (this codebase has no
// real tokenizer dependency, and one is not warranted for a health metric
// alone). Never used for anything budget-enforcing -- §26.7's own
// cost-budget mechanism is denominated in USD from real, post-hoc
// provider-reported cost, not a pre-flight estimate of any kind.
func estimateTokens(block string) int64 {
	return int64(len(block) / 4)
}

// RecordKnowledgeBlockTokens emits §31.2's own three-number split for one
// review turn's own assembled knowledge blocks -- falsePositiveBlock is
// FetchFalsePositivePatterns' own return value, archDecisionsBlock is
// FetchPriorArchDecisions' own returned block string, both already
// rendered (or "" when that source had nothing to inject) by the time the
// caller has them in hand. Never returns anything and never blocks the
// caller's own request path on a construction failure -- a nil histogram
// (tokensHistogram's own degradation) makes this call a silent no-op,
// exactly the fail-safe direction an operator-facing health metric
// requires: losing this ONE gauge must never be a reason to fail, delay,
// or alter a real review turn's own creation.
func RecordKnowledgeBlockTokens(ctx context.Context, falsePositiveBlock, archDecisionsBlock string) {
	h := tokensHistogram()
	if h == nil {
		return
	}
	fp := estimateTokens(falsePositiveBlock)
	arch := estimateTokens(archDecisionsBlock)
	h.Record(ctx, fp, metric.WithAttributes(attribute.String("component", string(componentFalsePositivePatterns))))
	h.Record(ctx, arch, metric.WithAttributes(attribute.String("component", string(componentArchDecisions))))
	h.Record(ctx, fp+arch, metric.WithAttributes(attribute.String("component", string(componentTotal))))
}
