package review

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestPlaceholderTokensMatchTurnPackage is deliberately an INTERNAL test
// (package review, not review_test) so it can reach placeholderTokens
// itself, which is unexported -- see that var's own doc comment
// (sanitize.go) for why turn's own three placeholder literals are
// duplicated here as raw strings rather than imported by production code
// (this package's own doc.go fixes production imports at "zero external
// imports"; internal/domain/turn is not this package). Test files are
// exempt from that production-import discipline the same way this
// package's own context_test.go is already exempt from it (importing
// contracts/gen/go/restdtos and internal/domain/reviewtriage) -- neither
// ships in a production build regardless of whether the test package is
// internal or external, so importing turn HERE, for this one cross-package
// consistency check, does not reopen the layering concern doc.go
// documents. Mirrors internal/domain/upload's own IDENTICAL
// TestPlaceholderTokensMatchTurnPackage (upload/placeholders_internal_test.go)
// byte-for-byte in spirit.
//
// If internal/domain/turn ever renames or rotates one of these three
// without a matching update to placeholderTokens (sanitize.go), this test
// fails CI immediately -- rather than silently reopening the exact
// bearer-token-leak vulnerability StripPlaceholderTokens exists to close
// (an attacker-controlled diff/title/body containing a STALE copy of a
// since-rotated turn placeholder would no longer be neutralized).
func TestPlaceholderTokensMatchTurnPackage(t *testing.T) {
	t.Parallel()

	for _, tok := range []string{
		turn.EpistemicOutcomeToolURLPlaceholder,
		turn.EpistemicOutcomeToolBearerPlaceholder,
		turn.EpistemicOutcomeToolGenPlaceholder,
	} {
		if !containsToken(placeholderTokens, tok) {
			t.Errorf("placeholderTokens = %v, want it to contain turn's own real placeholder %q", placeholderTokens, tok)
		}
	}
}

// There is deliberately NO TestPlaceholderTokensMatchUploadPackage here to
// mirror TestPlaceholderTokensMatchTurnPackage above: internal/domain/upload
// is NOT importable from this package's own tests, even though this file
// is a test file exempt from doc.go's production-import ceiling --
// internal/domain/upload imports internal/app/ports (upload/doc.go), and
// internal/app/ports itself imports internal/domain/review
// (descriptionautofixpayload.go), so `package review` (this file's own
// package) importing upload would close a real import cycle
// (review -> upload -> ports -> review), which `go test` correctly refuses
// to build. internal/domain/turn has no such path back to this package
// (verified: turn imports neither internal/app/ports nor review anywhere),
// which is exactly why the turn-matching test above is safe while an
// upload-matching one is not.
//
// upload's own three literals are instead covered by
// TestPlaceholderTokens_DiscoversEveryDomainPlaceholderLiteral
// (placeholderdrift_internal_test.go): a source-text scan (go/parser, never
// a Go import) that finds every "{{ALL_CAPS}}"-shaped string literal
// anywhere under internal/domain -- including upload's own
// BaseURLPlaceholder/BearerPlaceholder/GenPlaceholder declarations
// (upload/prompt.go) -- and asserts each is present in placeholderTokens
// here. A scan sidesteps the cycle entirely: it reads upload's source TEXT,
// it never imports the upload PACKAGE.

// TestPlaceholderTokensExactCount pins placeholderTokens' own total size --
// this package's own seven (VerdictToolURLPlaceholder/BearerPlaceholder/
// GenPlaceholder, ReviewCostBudgetToolURLPlaceholder, plus §15.3/§12.2
// item 9's own CompositionFindingsToolURLPlaceholder/BearerPlaceholder/
// GenPlaceholder, compositionreview.go), plus turn's own three, plus
// upload's own three, no more no less. A future family that grows this
// list without a corresponding drift-matcher test above (or without the
// general cross-domain-package scan, placeholderdrift_internal_test.go)
// fails here first, forcing a deliberate update to this exact number
// rather than an unnoticed size change.
func TestPlaceholderTokensExactCount(t *testing.T) {
	t.Parallel()

	// This package's own seven, for completeness -- trivially true by
	// construction today, but guards against a future refactor of
	// placeholderTokens' own literal accidentally dropping one.
	for _, tok := range []string{
		VerdictToolURLPlaceholder,
		VerdictToolBearerPlaceholder,
		VerdictToolGenPlaceholder,
		ReviewCostBudgetToolURLPlaceholder,
		CompositionFindingsToolURLPlaceholder,
		CompositionFindingsToolBearerPlaceholder,
		CompositionFindingsToolGenPlaceholder,
	} {
		if !containsToken(placeholderTokens, tok) {
			t.Errorf("placeholderTokens = %v, want it to contain this package's own %q", placeholderTokens, tok)
		}
	}

	if len(placeholderTokens) != 13 {
		t.Errorf("len(placeholderTokens) = %d, want exactly 13 (this package's own 7, plus turn's own 3, plus upload's own 3, no more no less)", len(placeholderTokens))
	}
}

// containsToken reports whether want appears anywhere in tokens -- shared
// by every drift-matcher test in this file.
func containsToken(tokens []string, want string) bool {
	for _, got := range tokens {
		if got == want {
			return true
		}
	}
	return false
}
