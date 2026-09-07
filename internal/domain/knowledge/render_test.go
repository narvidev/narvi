package knowledge_test

import (
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/knowledge"
)

// TestRenderPriorDecisionsBlock_Empty pins RenderAdvisoryBlock's own
// "empty string for zero candidates" contract, mirrored here so a caller
// can unconditionally prepend this function's return value with no
// special-casing.
func TestRenderPriorDecisionsBlock_Empty(t *testing.T) {
	t.Parallel()

	if got := knowledge.RenderPriorDecisionsBlock(nil); got != "" {
		t.Errorf("RenderPriorDecisionsBlock(nil) = %q, want empty string", got)
	}
	if got := knowledge.RenderPriorDecisionsBlock([]knowledge.Candidate{}); got != "" {
		t.Errorf("RenderPriorDecisionsBlock([]) = %q, want empty string", got)
	}
}

// TestRenderPriorDecisionsBlock_Delimited asserts the rendered block is
// wrapped in a fixed, closed delimiter tag, and that every candidate's
// own three free-text fields actually appear in the rendered text.
//
// Mutation-verified: temporarily changing the closing tag write to
// "</wrong_tag>\n\n" (a byte-for-byte typo of the real close) made this
// test fail on the "properly closed" assertion below; reverted after
// confirming the failure.
func TestRenderPriorDecisionsBlock_Delimited(t *testing.T) {
	t.Parallel()

	cands := []knowledge.Candidate{
		{
			ID:                    "v1:0",
			PRNumber:              42,
			Decision:              "introduced a retry queue table",
			RejectedAlternative:   "reusing the existing outbox",
			ConventionConformance: "matches the outbox pattern used elsewhere",
		},
	}

	got := knowledge.RenderPriorDecisionsBlock(cands)

	const openTag = "<prior_architecture_decisions>"
	const closeTag = "</prior_architecture_decisions>"
	openIdx := strings.Index(got, openTag)
	closeIdx := strings.Index(got, closeTag)
	if openIdx == -1 || closeIdx == -1 || closeIdx < openIdx {
		t.Fatalf("RenderPriorDecisionsBlock() = %q, want a properly opened-then-closed %q/%q block", got, openTag, closeTag)
	}

	for _, want := range []string{"introduced a retry queue table", "reusing the existing outbox", "matches the outbox pattern used elsewhere", "42"} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderPriorDecisionsBlock() = %q, want it to contain %q", got, want)
		}
	}

	// "DATA, never instruction" framing, mirroring RenderAdvisoryBlock's
	// own precedent.
	if !strings.Contains(got, "DATA") {
		t.Errorf("RenderPriorDecisionsBlock() = %q, want the DATA-not-instruction framing preamble", got)
	}
}

// TestRenderPriorDecisionsBlock_StripsPlaceholders is G2's own render-time
// second layer (§31.7: "Render-time sanitization remains as defense in
// depth, never as the primary") -- a candidate whose free-text field
// somehow still carries a placeholder-shaped token (G1 should already
// have stripped it at write time; this is the belt-and-suspenders check)
// must never reach the rendered block able to be expanded into a live
// secret by cmd/sandbox-agent's own unconditional whole-prompt
// strings.ReplaceAll passes.
//
// Mutation-verified: temporarily removing the stripPlaceholderTokens call
// in RenderPriorDecisionsBlock (render.go) made this test fail (the
// literal token surfaced verbatim in the rendered output); reverted after
// confirming the failure.
func TestRenderPriorDecisionsBlock_StripsPlaceholders(t *testing.T) {
	t.Parallel()

	cands := []knowledge.Candidate{
		{
			ID:                    "v1:0",
			Decision:              "leaked {{REVIEW_VERDICT_TOOL_BEARER}} into the decision text",
			RejectedAlternative:   "plain alternative",
			ConventionConformance: "{{UPLOAD_TOOL_BEARER}} also leaked here",
		},
	}

	got := knowledge.RenderPriorDecisionsBlock(cands)

	for _, forbidden := range []string{"{{REVIEW_VERDICT_TOOL_BEARER}}", "{{UPLOAD_TOOL_BEARER}}"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("RenderPriorDecisionsBlock() = %q, must not contain the placeholder token %q verbatim", got, forbidden)
		}
	}
	// The surrounding, non-placeholder text must survive untouched.
	if !strings.Contains(got, "leaked") || !strings.Contains(got, "also leaked here") {
		t.Errorf("RenderPriorDecisionsBlock() = %q, want the non-placeholder surrounding text preserved", got)
	}
}

// TestRenderPriorDecisionsBlock_TrimsWhitespace is RenderAdvisoryBlock's
// own "strip pass" (per-field strings.TrimSpace) mirrored here.
func TestRenderPriorDecisionsBlock_TrimsWhitespace(t *testing.T) {
	t.Parallel()

	cands := []knowledge.Candidate{
		{ID: "v1:0", Decision: "  padded decision  \n", RejectedAlternative: "alt", ConventionConformance: "conf"},
	}
	got := knowledge.RenderPriorDecisionsBlock(cands)
	if strings.Contains(got, "  padded decision") || strings.Contains(got, "padded decision  ") {
		t.Errorf("RenderPriorDecisionsBlock() = %q, want leading/trailing whitespace trimmed from Decision", got)
	}
	if !strings.Contains(got, "padded decision") {
		t.Errorf("RenderPriorDecisionsBlock() = %q, want the trimmed decision text present", got)
	}
}

// TestCandidate_ContentHash pins ContentHash's own contract: stable
// across calls, sensitive to any of the three fields it joins, and
// distinct from another candidate's hash whenever content differs.
func TestCandidate_ContentHash(t *testing.T) {
	t.Parallel()

	a := knowledge.Candidate{Decision: "d1", RejectedAlternative: "r1", ConventionConformance: "c1"}
	aAgain := knowledge.Candidate{Decision: "d1", RejectedAlternative: "r1", ConventionConformance: "c1"}
	b := knowledge.Candidate{Decision: "d2", RejectedAlternative: "r1", ConventionConformance: "c1"}

	if a.ContentHash() != aAgain.ContentHash() {
		t.Errorf("ContentHash() not stable across identical candidates: %q vs %q", a.ContentHash(), aAgain.ContentHash())
	}
	if a.ContentHash() == b.ContentHash() {
		t.Errorf("ContentHash() collided for candidates with different Decision fields: both %q", a.ContentHash())
	}
	if a.ContentHash() == "" {
		t.Error("ContentHash() = empty string, want a real hex digest")
	}
}

// TestInjectedRecord_Empty covers the predicate review_verdicts.
// knowledge_influenced (§31.7's own G5) is derived from.
func TestInjectedRecord_Empty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rec  knowledge.InjectedRecord
		want bool
	}{
		{"zero value", knowledge.InjectedRecord{}, true},
		{"nil IDs, non-nil ContentHashes", knowledge.InjectedRecord{ContentHashes: []string{}}, true},
		{"one id", knowledge.InjectedRecord{IDs: []string{"v1:0"}}, false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.rec.Empty(); got != tt.want {
				t.Errorf("InjectedRecord(%+v).Empty() = %v, want %v", tt.rec, got, tt.want)
			}
		})
	}
}
