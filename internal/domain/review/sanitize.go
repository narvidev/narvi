package review

// reviewCostBudgetToolURLPlaceholder is intentionally NOT redeclared here:
// see placeholderTokens below, which references this package's own real
// exported constants (VerdictToolURLPlaceholder et al.,
// ReviewCostBudgetToolURLPlaceholder) directly rather than duplicating
// them as raw literals -- this file lives in the SAME package those
// constants are declared in, so there is no import-ceiling reason to
// duplicate what can simply be named.

// epistemicOutcomeToolURLPlaceholderLiteral, epistemicOutcomeToolBearerPlaceholderLiteral,
// and epistemicOutcomeToolGenPlaceholderLiteral are byte-for-byte copies of
// internal/domain/turn's own EpistemicOutcomeToolURLPlaceholder/
// EpistemicOutcomeToolBearerPlaceholder/EpistemicOutcomeToolGenPlaceholder
// (turn/epistemicpreamble.go, §20.2) -- duplicated as raw string
// literals here, rather than imported, because this package's own doc
// comment (doc.go) fixes it at "zero external imports": internal/domain/turn
// is not this package, and reaching sideways into a sibling domain
// package's own vocabulary is exactly the kind of new dependency doc.go
// rules out. Mirrors internal/domain/upload's own IDENTICAL treatment of
// these same three literals (upload/prompt.go) -- see that file's own top
// doc comment for the full "why duplicate, not import" reasoning, which
// applies here without modification.
//
// uploadToolBaseURLPlaceholderLiteral, uploadToolBearerPlaceholderLiteral,
// and uploadToolGenPlaceholderLiteral are the SAME byte-for-byte treatment
// for internal/domain/upload's own BaseURLPlaceholder/BearerPlaceholder/
// GenPlaceholder (upload/prompt.go, §28.5) -- the identical "zero external
// imports" restriction bars importing internal/domain/upload here too.
const (
	epistemicOutcomeToolURLPlaceholderLiteral    = "{{EPISTEMIC_OUTCOME_TOOL_URL}}"
	epistemicOutcomeToolBearerPlaceholderLiteral = "{{EPISTEMIC_OUTCOME_TOOL_BEARER}}"
	epistemicOutcomeToolGenPlaceholderLiteral    = "{{EPISTEMIC_OUTCOME_TOOL_GEN}}"

	uploadToolBaseURLPlaceholderLiteral = "{{UPLOAD_TOOL_BASE_URL}}"
	uploadToolBearerPlaceholderLiteral  = "{{UPLOAD_TOOL_BEARER}}"
	uploadToolGenPlaceholderLiteral     = "{{UPLOAD_TOOL_GEN}}"
)

// placeholderTokens lists every literal placeholder token this whole
// system ever substitutes for a live secret (or, for
// ReviewCostBudgetToolURLPlaceholder, a live but non-secret local URL) at
// prompt-substitution time -- this package's own four (VerdictToolURLPlaceholder/
// VerdictToolBearerPlaceholder/VerdictToolGenPlaceholder/
// ReviewCostBudgetToolURLPlaceholder, referenced directly, not duplicated:
// this file is IN internal/domain/review), plus turn's own three and
// upload's own three (both duplicated as raw literals immediately above,
// for the identical "zero external imports" reason internal/domain/upload's
// own placeholderTokens duplicates review's/turn's literals).
//
// # The vulnerability this closes (Phase 5 audit, CRITICAL)
//
// RenderTurnPrompt (below) embeds ctx.Diff/ctx.Title/ctx.Body -- entirely
// attacker-controlled PR content, §5.2 -- verbatim into a review turn's
// prompt. That SAME prompt ALWAYS also carries this package's own
// verdict-tool instruction block (verdictToolInstructions), which ALWAYS
// contains the literal placeholder tokens VerdictToolURLPlaceholder/
// VerdictToolBearerPlaceholder/VerdictToolGenPlaceholder. Because those
// tokens are unconditionally present in every review prompt,
// cmd/sandbox-agent's own renderVerdictToolPromptText runs an unconditional
// strings.ReplaceAll of each token for its real, live value over the
// ENTIRE assembled prompt text -- diff/title/body included, since that
// substitution has no way to distinguish "this system's own trusted
// instruction text" from "an attacker's diff that happens to contain the
// same literal bytes". A PR author who writes the literal string
// "{{REVIEW_VERDICT_TOOL_BEARER}}" inside their diff, title, or body
// therefore gets it expanded into that turn's REAL, live sandbox bearer
// token -- authenticating the scm-credentials, provider-credentials,
// verdict-posting, and uploads endpoints -- which a prompt-injected agent
// can then be steered into exfiltrating.
//
// The SAME exposure applies to every OTHER placeholder family
// sandbox-agent ever substitutes system-wide, not just this package's own
// three: cmd/sandbox-agent runs several independent, blind, whole-prompt
// ReplaceAll passes (reviewverdicttoolprompt.go for the review-verdict and
// upload sets, epistemicoutcometoolprompt.go for the epistemic set,
// reviewcostbudgetprompt.go for the cost-budget URL) -- a token from ANY
// of those families, planted in the diff/title/body, is expanded by
// whichever pass owns it, exactly as readily as this package's own three.
//
// internal/domain/upload already solved this EXACT problem for its own
// untrusted Filename/ContentType fields (sanitizeUntrustedField,
// upload/prompt.go) -- this var and StripPlaceholderTokens (below) are
// this package's own mirror of that mechanism, applied at the one place
// THIS package embeds untrusted content into a rendered prompt
// (RenderTurnPrompt's own ctx.Diff/ctx.Title/ctx.Body handling).
//
// TestPlaceholderTokensMatchTurnPackage/TestPlaceholderTokensMatchUploadPackage
// (placeholders_internal_test.go, an internal test package free to import
// internal/domain/turn and internal/domain/upload for exactly this
// cross-package consistency check) assert the two duplicated triples above
// stay byte-for-byte identical to turn's/upload's own real exported
// constants; TestPlaceholderTokens_DiscoversEveryDomainPlaceholderLiteral
// (placeholderdrift_internal_test.go) is a general, self-updating source
// scan (mirrors internal/domain/upload's own identical mechanism) that
// fails CI the moment ANY future placeholder family, anywhere under
// internal/domain, is added without a matching entry here -- so this list
// cannot silently go stale the way turn's own three literals once did
// inside upload's copy (upload/prompt.go's own doc comment, "F1").
var placeholderTokens = []string{
	VerdictToolURLPlaceholder,
	VerdictToolBearerPlaceholder,
	VerdictToolGenPlaceholder,
	ReviewCostBudgetToolURLPlaceholder,
	CompositionFindingsToolURLPlaceholder,
	CompositionFindingsToolBearerPlaceholder,
	CompositionFindingsToolGenPlaceholder,
	epistemicOutcomeToolURLPlaceholderLiteral,
	epistemicOutcomeToolBearerPlaceholderLiteral,
	epistemicOutcomeToolGenPlaceholderLiteral,
	uploadToolBaseURLPlaceholderLiteral,
	uploadToolBearerPlaceholderLiteral,
	uploadToolGenPlaceholderLiteral,
}

// removeAllOccurrences returns s with every non-overlapping occurrence of
// tok deleted -- a dependency-free stand-in for strings.ReplaceAll(s, tok,
// "") -- kept inline so this file needs no import at all, matching this
// package's own "zero external imports" convention (doc.go) that every
// other file here already follows (RenderTurnPrompt's own itoa/
// hasTrailingNewline helpers, context.go, are the same kind of
// hand-rolled stand-in for an identical reason). tok == "" is treated as
// "nothing to remove" (returns s unchanged) rather than an infinite/
// pathological expansion -- not reachable via placeholderTokens today (no
// entry is ever empty), but a safe, boring default regardless.
func removeAllOccurrences(s, tok string) string {
	if tok == "" || len(s) < len(tok) {
		return s
	}
	var out []byte
	for i := 0; i < len(s); {
		if i+len(tok) <= len(s) && s[i:i+len(tok)] == tok {
			i += len(tok)
			continue
		}
		out = append(out, s[i])
		i++
	}
	return string(out)
}

// StripPlaceholderTokens destroys every exact occurrence of every literal
// in placeholderTokens from s -- see that var's own doc comment for the
// full attack this closes. Loops to a fixed point (repeats until a full
// pass over every token changes nothing) rather than a single pass, for
// the SAME reason internal/domain/upload's own sanitizeUntrustedField
// loops (upload/prompt.go): removing one token's exact literal could, in
// principle, splice two remaining fragments into a DIFFERENT token's exact
// literal (e.g. the text surrounding an embedded
// "{{REVIEW_VERDICT_TOOL_BEARER}}" could itself spell out
// "{{REVIEW_VERDICT_TOOL_GEN}}" once the middle is removed) -- looping
// closes that concatenation seam rather than depending on
// placeholderTokens' own declaration order.
//
// Deliberately does NOT touch '<'/'>' or any other byte -- see
// sanitizeDiffField's own doc comment for why the diff specifically must
// stay byte-for-byte faithful outside of placeholder tokens; a caller that
// also needs '<'/'>' neutralized (sanitizeDescriptionField below, and
// reviewpost's own write-path digest sanitizer, next paragraph) composes
// this with its own separate escaping step instead.
//
// EXPORTED (hardening, review digest write-path sanitization):
// this function was unexported until this Step, called only by
// sanitizeDiffField/sanitizeDescriptionField below for THIS package's own
// read/prompt path (RenderTurnPrompt). internal/domain/reviewpost's own
// SanitizeDigest (reviewpost/sanitize.go) -- the persistence-side sibling
// that neutralizes a review verdict's own model-authored digest fields
// (Summary/StackRisks/UnverifiedLimits/AdequacyExplanation/ProposedBody/
// ContestedPoints/ArchDecision.*) before internal/app/reviewverdict.Insert
// ever writes them to Postgres -- calls this EXACT function, rather than
// hand-duplicating placeholderTokens a fourth time (review/upload already
// each duplicate the other's tokens as raw literals, for their OWN
// "zero external imports" reasons; reviewpost has no such restriction --
// its own doc.go already permits exactly one non-stdlib import, this
// package, for the Verdict/RiskLevel/Shippable/Tag types it needs anyway).
// Exporting the ALREADY-canonical, ALREADY-drift-tested list this package
// maintains (placeholderdrift_internal_test.go's own whole-internal/domain
// source scan, which finds every placeholder family and fails CI the
// moment placeholderTokens goes stale) is what makes reviewpost's own
// write-path sanitizer pick up a future eleventh placeholder family
// automatically, the same way this package's own read path already does --
// a fourth hand-copied list, by contrast, would need its OWN drift test to
// get that property, duplicating machinery this package already owns
// rather than reusing it. See doc.go's own updated "exactly eight
// functions" section for why this one further export does not reopen the
// export-surface discipline that section documents.
func StripPlaceholderTokens(s string) string {
	for {
		before := s
		for _, tok := range placeholderTokens {
			s = removeAllOccurrences(s, tok)
		}
		if s == before {
			return s
		}
	}
}

// escapeAngleBrackets HTML-entity-escapes '<' and '>' in s -- a
// dependency-free stand-in for strings.NewReplacer("<", "&lt;", ">",
// "&gt;").Replace(s), matching this package's own "zero external imports"
// convention. Mirrors internal/domain/upload's own sanitizeUntrustedField
// treatment of the identical two characters, for the identical reason:
// '<'/'>' are the only two characters that can close this package's own
// delimited data blocks (diffContentDelimiter/descriptionContentDelimiter)
// early, or forge a fake one -- see sanitizeDescriptionField's own doc
// comment for where this is actually applied.
func escapeAngleBrackets(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			out = append(out, '&', 'l', 't', ';')
		case '>':
			out = append(out, '&', 'g', 't', ';')
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

// sanitizeDiffField neutralizes ctx.Diff (PreFetchedContext, entirely
// attacker-controlled, §5.2) before RenderTurnPrompt embeds it into a
// review turn's prompt: ONLY placeholder-token stripping, deliberately NO
// '<'/'>' escaping.
//
// This is a considered, asymmetric choice from sanitizeDescriptionField
// (below), not an oversight: the diff is SOURCE CODE the reviewing agent
// must read accurately to do its one job -- HTML-escaping every '<'/'>' in
// it would corrupt the code under review itself (C++/Go template and
// generic syntax, comparison operators, shell redirects, ...), silently
// feeding the agent a mangled transcript of the very diff it is asked to
// verdict over. The placeholder-token-forgery hazard
// (placeholderTokens' own doc comment) is the ONLY hazard this function
// closes for the diff; the pre-existing diffContentDelimiter wrapping
// (RenderTurnPrompt) is this package's own separate, already-accepted
// mitigation for the diff's own delimiter-fence risk, unchanged by this
// fix.
func sanitizeDiffField(s string) string {
	return StripPlaceholderTokens(s)
}

// sanitizeDescriptionField neutralizes ctx.Title/ctx.Body (PreFetchedContext,
// untrusted PR metadata, §5.2) before RenderTurnPrompt embeds them into a
// review turn's prompt: placeholder-token stripping (the load-bearing fix,
// identical reasoning to sanitizeDiffField above), PLUS '<'/'>' escaping.
//
// Unlike the diff, Title/Body are short, human-or-model-authored PROSE, not
// code the agent must parse byte-for-byte to do its job -- there is no
// legitimate reason a real PR title or description needs a literal '<' or
// '>' preserved verbatim, so this package makes the SAME judgment call
// internal/domain/upload already made for its own short untrusted metadata
// fields (Filename/ContentType, upload/prompt.go's sanitizeUntrustedField):
// escaping closes the delimiter-fence-escape hazard too (a title/body
// containing a literal "</pr_description>" can no longer close that block
// early or forge a fake one), at the cost of a title/body that legitimately
// contained "<" or ">" rendering as "&lt;"/"&gt;" in the agent's view --
// judged an acceptable, defensible trade-off for prose metadata the way it
// would NOT be for the diff's own code content.
func sanitizeDescriptionField(s string) string {
	return StripPlaceholderTokens(escapeAngleBrackets(s))
}
