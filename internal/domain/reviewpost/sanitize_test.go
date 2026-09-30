package reviewpost_test

import (
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
)

// allTenPlaceholderTokensForTest mirrors internal/domain/review's own
// unexported placeholderTokens var (sanitize.go) -- the ten literal
// secret-substitution placeholder tokens SanitizeDigest must strip from
// every model-authored digest field. review's own four are referenced via
// their real exported constants (review.VerdictToolURLPlaceholder et al.,
// review.ReviewCostBudgetToolURLPlaceholder); turn's/upload's own six are
// raw literals (this test package cannot import internal/domain/turn or
// internal/domain/upload without risking the exact import-cycle
// review/placeholders_internal_test.go's own doc comment already proves
// exists for upload specifically: reviewpost -> ... -> upload -> ports ->
// review is not this test's own concern, but staying with raw literals
// here needs no cycle analysis at all) -- byte-for-byte copies of
// turn.EpistemicOutcomeToolURLPlaceholder/...Bearer.../...Gen... and
// upload.BaseURLPlaceholder/BearerPlaceholder/GenPlaceholder, verified
// against those real constants by review's OWN
// TestPlaceholderTokensMatchTurnPackage/the whole-internal/domain scan
// (placeholders_internal_test.go/placeholderdrift_internal_test.go) --
// this test only needs them to be CORRECT, not to independently re-prove
// they match; that proof already lives where the canonical list itself
// lives.
var allTenPlaceholderTokensForTest = []string{
	review.VerdictToolURLPlaceholder,
	review.VerdictToolBearerPlaceholder,
	review.VerdictToolGenPlaceholder,
	review.ReviewCostBudgetToolURLPlaceholder,
	"{{EPISTEMIC_OUTCOME_TOOL_URL}}",
	"{{EPISTEMIC_OUTCOME_TOOL_BEARER}}",
	"{{EPISTEMIC_OUTCOME_TOOL_GEN}}",
	"{{UPLOAD_TOOL_BASE_URL}}",
	"{{UPLOAD_TOOL_BEARER}}",
	"{{UPLOAD_TOOL_GEN}}",
}

// allTenTokensJoined is every one of the ten tokens concatenated together
// -- a single field value carrying all ten at once, the fixture this
// file's own "regression test" (the task's own explicit ask: "a
// VerdictInput whose digest fields carry all ten placeholder literals")
// builds from.
func allTenTokensJoined() string {
	return strings.Join(allTenPlaceholderTokensForTest, " and also ")
}

// assertNoTokenSurvives fails t if s contains ANY of the ten placeholder
// tokens -- the one shared assertion every test below reduces to.
func assertNoTokenSurvives(t *testing.T, label, s string) {
	t.Helper()
	for _, tok := range allTenPlaceholderTokensForTest {
		if strings.Contains(s, tok) {
			t.Errorf("%s still contains placeholder token %q after SanitizeDigest -- want it stripped, got: %q", label, tok, s)
		}
	}
}

// TestSanitizeDigest_StripsAllTenPlaceholderTokensFromEveryField is this
// Step's own direct regression test (G1): table-driven over every one of
// SanitizeDigest's seven sanitized string sites (Summary, StackRisks,
// UnverifiedLimits, AdequacyExplanation, ProposedBody, ContestedPoints,
// and each of an ArchDecision's own three fields) -- each case plants ALL
// TEN placeholder tokens into ONE field (a single string carrying every
// token at once, allTenTokensJoined), calls SanitizeDigest, and asserts
// NONE of the ten survive anywhere in the returned Digest. Proves a
// planted token cannot reach reviewpost.SanitizeDigest's own output
// regardless of WHICH digest field an attacker-steered model echoes it
// into.
func TestSanitizeDigest_StripsAllTenPlaceholderTokensFromEveryField(t *testing.T) {
	t.Parallel()

	poison := allTenTokensJoined()

	tests := []struct {
		name    string
		digest  reviewpost.Digest
		extract func(reviewpost.Digest) string
	}{
		{
			name:    "Summary",
			digest:  reviewpost.Digest{Summary: poison},
			extract: func(d reviewpost.Digest) string { return d.Summary },
		},
		{
			name:    "StackRisks",
			digest:  reviewpost.Digest{Summary: "ok", StackRisks: poison},
			extract: func(d reviewpost.Digest) string { return d.StackRisks },
		},
		{
			name:    "UnverifiedLimits",
			digest:  reviewpost.Digest{Summary: "ok", UnverifiedLimits: poison},
			extract: func(d reviewpost.Digest) string { return d.UnverifiedLimits },
		},
		{
			name:    "AdequacyExplanation",
			digest:  reviewpost.Digest{Summary: "ok", AdequacyExplanation: poison},
			extract: func(d reviewpost.Digest) string { return d.AdequacyExplanation },
		},
		{
			name:    "ProposedBody",
			digest:  reviewpost.Digest{Summary: "ok", ProposedBody: poison},
			extract: func(d reviewpost.Digest) string { return d.ProposedBody },
		},
		{
			name:    "ContestedPoints",
			digest:  reviewpost.Digest{Summary: "ok", ContestedPoints: poison},
			extract: func(d reviewpost.Digest) string { return d.ContestedPoints },
		},
		{
			name: "ArchDecision.Decision",
			digest: reviewpost.Digest{Summary: "ok", ArchDecisions: []reviewpost.ArchDecision{
				{Decision: poison, RejectedAlternative: "alt", ConventionConformance: "conforms"},
			}},
			extract: func(d reviewpost.Digest) string { return d.ArchDecisions[0].Decision },
		},
		{
			name: "ArchDecision.RejectedAlternative",
			digest: reviewpost.Digest{Summary: "ok", ArchDecisions: []reviewpost.ArchDecision{
				{Decision: "decision", RejectedAlternative: poison, ConventionConformance: "conforms"},
			}},
			extract: func(d reviewpost.Digest) string { return d.ArchDecisions[0].RejectedAlternative },
		},
		{
			name: "ArchDecision.ConventionConformance",
			digest: reviewpost.Digest{Summary: "ok", ArchDecisions: []reviewpost.ArchDecision{
				{Decision: "decision", RejectedAlternative: "alt", ConventionConformance: poison},
			}},
			extract: func(d reviewpost.Digest) string { return d.ArchDecisions[0].ConventionConformance },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := reviewpost.SanitizeDigest(tt.digest)
			assertNoTokenSurvives(t, tt.name, tt.extract(got))
		})
	}
}

// TestSanitizeDigest_EscapesAngleBrackets proves SanitizeDigest applies
// the SAME '<'/'>' HTML-entity escaping treatment
// TestRenderVerdictComment_UntrustedDigestFieldsAreEscaped
// (rendercomment_untrustedfields_test.go) already pins at RENDER time --
// reusing that file's own digestInjectionPayload/digestInjectionEscaped
// fixtures so the two tests can never drift apart on what "an injection"
// means.
func TestSanitizeDigest_EscapesAngleBrackets(t *testing.T) {
	t.Parallel()

	got := reviewpost.SanitizeDigest(reviewpost.Digest{
		Summary:             digestInjectionPayload,
		StackRisks:          digestInjectionPayload,
		UnverifiedLimits:    digestInjectionPayload,
		AdequacyExplanation: digestInjectionPayload,
		ProposedBody:        digestInjectionPayload,
		ContestedPoints:     digestInjectionPayload,
		ArchDecisions: []reviewpost.ArchDecision{
			{Decision: digestInjectionPayload, RejectedAlternative: digestInjectionPayload, ConventionConformance: digestInjectionPayload},
		},
	})

	fields := map[string]string{
		"Summary":                            got.Summary,
		"StackRisks":                         got.StackRisks,
		"UnverifiedLimits":                   got.UnverifiedLimits,
		"AdequacyExplanation":                got.AdequacyExplanation,
		"ProposedBody":                       got.ProposedBody,
		"ContestedPoints":                    got.ContestedPoints,
		"ArchDecision.Decision":              got.ArchDecisions[0].Decision,
		"ArchDecision.RejectedAlternative":   got.ArchDecisions[0].RejectedAlternative,
		"ArchDecision.ConventionConformance": got.ArchDecisions[0].ConventionConformance,
	}
	for name, val := range fields {
		if val != digestInjectionEscaped {
			t.Errorf("SanitizeDigest().%s = %q, want the SAME escaped form RenderVerdictComment produces (%q)", name, val, digestInjectionEscaped)
		}
	}
}

// TestSanitizeDigest_SplitPlaceholderTokenAcrossFragmentsIsNeutralized
// mirrors review's own
// TestRenderTurnPrompt_SplitPlaceholderTokenAcrossFragmentsIsNeutralized
// (context_test.go) for SanitizeDigest specifically: proves
// SanitizeDigest inherits review.StripPlaceholderTokens' own fixed-point
// loop (not a single, non-looping pass) by constructing a field whose
// raw text is NOT itself one of the ten tokens, but SPLICES into one once
// an embedded token is removed.
func TestSanitizeDigest_SplitPlaceholderTokenAcrossFragmentsIsNeutralized(t *testing.T) {
	t.Parallel()

	// Removing the inner "{{REVIEW_VERDICT_TOOL_GEN}}" from
	// "{{REVIEW_VERDICT{{REVIEW_VERDICT_TOOL_GEN}}_TOOL_BEARER}}" leaves
	// "{{REVIEW_VERDICT_TOOL_BEARER}}" -- a second real token that only
	// exists once the middle is removed.
	spliced := "{{REVIEW_VERDICT" + review.VerdictToolGenPlaceholder + "_TOOL_BEARER}}"
	if !strings.Contains(spliced, review.VerdictToolGenPlaceholder) {
		t.Fatalf("test construction bug: spliced fixture does not contain the GEN token it is built from")
	}

	got := reviewpost.SanitizeDigest(reviewpost.Digest{Summary: spliced})

	if strings.Contains(got.Summary, review.VerdictToolBearerPlaceholder) {
		t.Errorf("SanitizeDigest().Summary = %q, still contains %q -- a single, non-looping strip pass would leave this SPLICED-together token behind; want the fixed-point loop to remove it too", got.Summary, review.VerdictToolBearerPlaceholder)
	}
	if strings.Contains(got.Summary, review.VerdictToolGenPlaceholder) {
		t.Errorf("SanitizeDigest().Summary = %q, still contains %q", got.Summary, review.VerdictToolGenPlaceholder)
	}
}

// TestSanitizeDigest_DescriptionAdequacyUntouched proves the one digest
// field this function deliberately does NOT sanitize -- review's own
// closed, validated DescriptionAdequacy enum, never free text (this
// function's own doc comment) -- survives completely unchanged.
func TestSanitizeDigest_DescriptionAdequacyUntouched(t *testing.T) {
	t.Parallel()

	got := reviewpost.SanitizeDigest(reviewpost.Digest{
		Summary:             "ok",
		DescriptionAdequacy: review.DescriptionAdequacyMisleading,
	})

	if got.DescriptionAdequacy != review.DescriptionAdequacyMisleading {
		t.Errorf("SanitizeDigest().DescriptionAdequacy = %q, want unchanged %q", got.DescriptionAdequacy, review.DescriptionAdequacyMisleading)
	}
}

// TestSanitizeDigest_NilArchDecisionsStaysNil mirrors BuildFindings' own
// "nil in, nil out" precedent (validate_test.go) -- SanitizeDigest must
// never turn an absent ArchDecisions into a non-nil, zero-length slice,
// which would change how a caller's own nil/len() check reads.
func TestSanitizeDigest_NilArchDecisionsStaysNil(t *testing.T) {
	t.Parallel()

	got := reviewpost.SanitizeDigest(reviewpost.Digest{Summary: "ok"})
	if got.ArchDecisions != nil {
		t.Errorf("SanitizeDigest().ArchDecisions = %#v, want nil", got.ArchDecisions)
	}
}

// TestSanitizeDigest_DoesNotMutateCallersArchDecisionsBackingArray proves
// SanitizeDigest allocates a NEW ArchDecisions slice rather than
// sanitizing in place -- the caller's own original slice (and the
// strings inside it) must stay observable/unchanged after SanitizeDigest
// returns, since internal/app/reviewverdict.Insert's own doc comment
// promises RenderVerdictComment's already-completed rendering (which read
// the SAME original Digest, by value, earlier in the SAME request) is
// never affected by this function's own write-path-only transformation.
func TestSanitizeDigest_DoesNotMutateCallersArchDecisionsBackingArray(t *testing.T) {
	t.Parallel()

	original := []reviewpost.ArchDecision{
		{Decision: review.VerdictToolBearerPlaceholder, RejectedAlternative: "alt", ConventionConformance: "conforms"},
	}
	digest := reviewpost.Digest{Summary: "ok", ArchDecisions: original}

	_ = reviewpost.SanitizeDigest(digest)

	if original[0].Decision != review.VerdictToolBearerPlaceholder {
		t.Errorf("caller's own original ArchDecisions[0].Decision = %q after SanitizeDigest, want unchanged %q (SanitizeDigest must never mutate its caller's own backing array)", original[0].Decision, review.VerdictToolBearerPlaceholder)
	}
}

// TestRenderVerdictComment_UnaffectedBySanitizeDigest is this Step's own
// "no double-escaping, verified not assumed" pin (SanitizeDigest's own
// doc comment, sanitize.go): RenderVerdictComment, given the ORIGINAL,
// unsanitized digest, must render escapeFindingDescription applied
// EXACTLY ONCE -- regardless of whether SanitizeDigest has ALSO been
// called, on a SEPARATE copy, in the same test. This is the regression
// test for the double-escaping hazard the task brief explicitly warned
// about: if a future change accidentally fed the SanitizeDigest-returned
// copy into RenderVerdictComment instead of the original, this test would
// start failing (want digestInjectionEscaped once; a double-escaped
// "&amp;lt;...&amp;gt;" would no longer match).
func TestRenderVerdictComment_UnaffectedBySanitizeDigest(t *testing.T) {
	t.Parallel()

	original := reviewpost.Digest{Summary: "ok", StackRisks: digestInjectionPayload}

	// Call SanitizeDigest first (as internal/app/reviewverdict.Insert
	// does), on a SEPARATE copy -- proving its existence/prior invocation
	// has no bearing on what RenderVerdictComment, given the ORIGINAL,
	// produces.
	_ = reviewpost.SanitizeDigest(original)

	got := reviewpost.RenderVerdictComment(baseVerdict(), assessmentOf(baseVerdict()), nil, original, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	if !strings.Contains(got, digestInjectionEscaped) {
		t.Errorf("RenderVerdictComment() missing the SINGLY-escaped StackRisks content, want to contain %q, got:\n%s", digestInjectionEscaped, got)
	}
	doubleEscaped := "&amp;lt;details&amp;gt;"
	if strings.Contains(got, doubleEscaped) {
		t.Errorf("RenderVerdictComment() rendered DOUBLE-escaped content (%q) -- SanitizeDigest's own write-path transformation must never be visible to this render, which reads the ORIGINAL digest:\n%s", doubleEscaped, got)
	}
}
