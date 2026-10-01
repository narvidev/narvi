package reviewpost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"strings"

	"github.com/narvidev/narvi/internal/domain/review"
)

// This file implements §8.2's own resolution of the tension named at
// internal/domain/review/doc.go's design call #4: that package deliberately
// ships NO Finding type ("a Finding type is left for whichever Step actually
// needs it"). Finding lives HERE, in reviewpost -- a sibling package, never
// as a new field on review.Verdict or a new exported function in review
// itself -- for the exact reason reviewpost/doc.go's own top-level comment
// already gives for every other type in this package: review/doc.go pins
// "exactly eight exported functions" in that package on purpose, and
// extending review's own EXPORT SURFACE (as opposed to extending its types
// from a sibling package, the established precedent this package's own
// VerdictInput/BuildVerdict already set) would silently break an invariant
// its own maintainers documented on purpose. review.Verdict itself
// (verdict.go) stays exactly §8.2's seven-field shape, untouched.

// SentinelKind names which sentinel produced a Finding -- "coverage" or
// "docs_drift" (the only two sentinels §17.1 says can ever trigger the
// sentinel-auto-fix flow; §17.1: "no other sentinel or finding type
// triggers this"). A nil *SentinelKind on Finding/FindingInput means an
// ORDINARY risk-map finding with no sentinel origin at all -- never a
// third SentinelKind value; the distinction is presence vs. absence of the
// pointer, not a third enum member, so a caller can never construct a
// "sentinel kind: none" value distinct from nil.
type SentinelKind string

// The two recognized SentinelKind values -- see this type's own doc
// comment for why there is no third.
const (
	SentinelKindCoverage  SentinelKind = "coverage"
	SentinelKindDocsDrift SentinelKind = "docs_drift"
)

// FindingStatus is a review_findings row's own mutable lifecycle state
// (migrations/000046_review_findings.up.sql) -- persists ACROSS re-posted
// verdicts (the whole reason review_findings is its own table rather than
// a column on the append-only review_verdicts history, §21.1, that §8.6
// will add later: see that migration's own doc comment).
type FindingStatus string

const (
	// FindingStatusOpen is a finding no maintainer has rebutted and no fix
	// (automated or manual) has yet addressed -- the status every finding
	// starts at.
	FindingStatusOpen FindingStatus = "open"
	// FindingStatusRebutted is a finding a maintainer+ has explicitly
	// dismissed (§22.1's content-based rebuttal identity, scoped down to
	// what THIS Step needs -- the learned-false-positive-pattern table
	// itself is out of this Step's scope).
	FindingStatusRebutted FindingStatus = "rebutted"
	// FindingStatusFixPending is a finding a sentinel-auto-fix child
	// session has been spawned for, fix PR not yet open. Suppresses the
	// manual apply-suggestion action for this finding (§17.3: "the two
	// remediation paths are mutually exclusive per finding").
	FindingStatusFixPending FindingStatus = "fix_pending"
	// FindingStatusFixOpen is a finding whose sentinel-auto-fix child
	// session has opened its own fix PR, not yet merged.
	FindingStatusFixOpen FindingStatus = "fix_open"
	// FindingStatusFixMerged is a finding whose sentinel-auto-fix fix PR
	// has been merge-gated in (§17.4) -- fully remediated by the
	// automation.
	FindingStatusFixMerged FindingStatus = "fix_merged"
	// FindingStatusFixApplied is a finding a maintainer+ remediated via the
	// manual apply-suggestion endpoint (§12.2 item 2) -- the OTHER
	// mutually-exclusive remediation path from FindingStatusFixPending/
	// FixOpen/FixMerged above.
	FindingStatusFixApplied FindingStatus = "fix_applied"
	// FindingStatusFixRecorded is a finding whose SuggestedFix was
	// submitted through the manual apply-suggestion endpoint on a
	// repository whose outgoing changes are currently suppressed
	// (platform shadow mode, TECHNICAL_PLAN.md §30.7/§30.9): the
	// endpoint's own UpdateFileContent call is honored and recorded in
	// the shadow ledger, but nothing reaches the customer's real
	// repository -- the commit SHA it returns is a self-evidently
	// synthetic value (shadowscm.IsSyntheticCommitSHA), never a real git
	// object.
	//
	// Unlike FindingStatusFixApplied, this status is NOT a claim of
	// resolution: the finding's own file/description are unchanged on
	// the real head, so an automatic re-review (§24) still detects the
	// same defect and re-ingests it under the SAME identity hash
	// (UpsertReviewFinding's own ON CONFLICT preserves this status,
	// bumping only last_seen_at) -- an honest UPDATE to a still-open
	// finding, never a contradiction the way re-detecting a
	// FindingStatusFixApplied finding would be. Re-review reconciliation
	// (ListOpenAndRebuttedReviewFindings) includes this status alongside
	// Open/Rebutted for exactly that reason: a re-reviewing agent must
	// see it as still live, never as already resolved.
	FindingStatusFixRecorded FindingStatus = "fix_recorded"
)

// EligibleForManualApply reports whether a finding's suggested fix can
// still be applied by hand.
//
// Open is the obvious case. FixRecorded is the one that was missing, and
// missing it left a finding permanently unfixable: applying a suggestion
// while the repository's egress was suppressed marks it fix_recorded --
// correctly, since nothing reached the real head -- and the eligibility
// check was `status == Open`, so after the repository was promoted the
// maintainer clicking Apply to make the fix real got a 409 forever. The
// defect was still there, still blocking merge, and the one action that
// would have fixed it was refused because it had been attempted once in
// shadow.
//
// Every other status names remediation that really did reach the head,
// or is owned by another path.
func (s FindingStatus) EligibleForManualApply() bool {
	switch s {
	case FindingStatusOpen, FindingStatusFixRecorded:
		return true
	default:
		return false
	}
}

// BlocksMerge reports whether a finding in this status still represents an
// UNRESOLVED defect on the real head, and must therefore keep counting
// against the merge gate and against every other "is anything still
// open" question.
//
// It exists because that question was being asked as `status == Open` at
// each call site, and adding a status is exactly when per-call-site
// equality goes wrong. FindingStatusFixRecorded is the case that proved
// it: its own documentation says it "is NOT a claim of resolution", and
// yet an equality check against Open silently classed it as resolved --
// so recording a fix that never reached the customer's repository made an
// unresolved finding stop blocking merge, permanently, and across a later
// promotion to live.
//
// Add a status, and this function is where you must decide what it means.
// That is the point of it being here rather than at the call sites.
func (s FindingStatus) BlocksMerge() bool {
	switch s {
	case FindingStatusOpen, FindingStatusFixRecorded:
		// Open: nobody has answered it. FixRecorded: the fix was
		// recorded and never committed, so the defect is untouched on
		// the real head -- the honest reading is "still open, with a
		// prepared remedy", never "resolved".
		return true
	default:
		// Rebutted (a maintainer answered it), fix_pending/fix_open/
		// fix_merged/fix_applied (remediation is owned by another path,
		// and really did reach the head). Each has an explicit
		// resolution.
		return false
	}
}

// FindingInput is a review-verdict-posting-tool call's own typed
// per-finding fields (§8.2's VerdictInput, extended -- restdtos.
// PostReviewVerdictRequest.findings, additive and optional, so an old
// caller posting no findings at all keeps posting exactly as before)
// -- everything BuildFinding needs BEFORE IdentityHash is
// server-computed (never accepted from a caller, matching review.Verdict.
// Shippable's own "never client-supplied" CONTRACT for the identical
// reason: an agent-supplied identity would let a model launder a stale
// finding into looking "already answered", or a genuinely new one into
// looking identical to an old, already-rebutted one).
type FindingInput struct {
	// SentinelKind is nil for an ordinary risk-map finding -- see this
	// type's own doc comment.
	SentinelKind *SentinelKind
	// Severity reuses review.RiskLevel's own closed, three-tier vocabulary
	// (no new enum, per the design's own explicit choice) -- one finding's
	// own severity, independent of the verdict's OWN overall RiskLevel.
	Severity review.RiskLevel
	// FilePath is the finding's own file, repo-relative. Part of identity
	// (ComputeFindingIdentity) -- see that function's own doc comment for
	// why line is excluded but file path is not.
	FilePath string
	// Line is informational only -- NEVER part of identity (the whole
	// point: a finding re-reported at a SHIFTED line number must still be
	// recognized as the SAME finding). Nil means "no specific line" (a
	// file-level or PR-level finding).
	Line *int
	// Description is the agent's own finding text -- part of identity,
	// normalized (ComputeFindingIdentity) so whitespace-level
	// regeneration (not paraphrase-level, an explicit, accepted residual)
	// still matches.
	Description string
	// SuggestedFix is an optional unified-diff/patch the apply-suggestion
	// endpoint (§12.2 item 2) can attempt to apply -- nil means this
	// finding has no machine-suggested fix at all.
	SuggestedFix *string
	// Source is the pass that produced this finding, as the reviewer's
	// payload says (§26.6's amendment, findingsource.go): primary or
	// counter_review when present (ValidateVerdictInput, not
	// ValidateFindingInput: a finding another producer builds, such as
	// the handoff sentinel's, comes from neither pass and carries none).
	// "" when the payload names none -- a turn whose prompt was rendered
	// before sources existed (FindingSourceNotRecorded) -- and stored as
	// "source not recorded". Never part of identity: the same issue
	// re-reported by the other pass is the same finding.
	Source FindingSource
}

// Finding is FindingInput plus the ONE thing FindingInput can never carry:
// IdentityHash, computed server-side, always via ComputeFindingIdentity,
// exactly mirroring review.Verdict.Shippable's own "the ONLY way to obtain
// a legitimate value ... is [the pure compute function]'s return value"
// CONTRACT (verdict.go) -- BuildFinding (below) is this package's one
// sanctioned way to construct one from an already-validated FindingInput.
type Finding struct {
	IdentityHash string
	SentinelKind *SentinelKind
	Severity     review.RiskLevel
	FilePath     string
	Line         *int
	Description  string
	SuggestedFix *string

	// Source is FindingInput.Source, carried verbatim -- self-reported;
	// "" (FindingSourceNotRecorded) for a finding no reviewer pass
	// produced, one posted with no source, or a stored one last published
	// before sources were recorded (migrations/000154).
	Source FindingSource
	// AdditionCheck is the server's resolution for a counter-review
	// addition (findingsource.go): set by BuildFindings, from the one
	// verdict-level BuildSecondFactCheck, on every finding whose Source
	// is FindingSourceCounterReview, and "" on every other. A
	// counter-review finding whose AdditionCheck is not Checked() is
	// published marked unverified and counted apart.
	AdditionCheck AdditionCheck

	// StartLine/EndLine are §22.1.1's own content-anchored position --
	// computed server-side by MatchPosition (position.go), or by the
	// non-agentic LLM-port relocation fallback when that match fails
	// (internal/app/findingposition, §4.3), NEVER by the reviewing model
	// itself. Both are the explicit, typed "unanchored" zero value (0)
	// until BuildFindings' own caller (httpapi.PostReviewVerdict) resolves
	// them -- BuildFinding/BuildFindings below never populate these two
	// fields, exactly like they never populate IdentityHash from anything
	// but ComputeFindingIdentity: position resolution is a SEPARATE step,
	// run once, after a Finding already exists, with the diff in hand
	// (§22.1.1: "resolved once, together, at render time" -- no second
	// pass, by construction).
	//
	// Deliberately plain ints, not *int like Line above: for Line, nil
	// distinguishes "the model reported no line at all" from a real line
	// number -- but for StartLine/EndLine, 0 IS the sentinel itself ("not
	// found"), never a value MatchPosition or the relocation fallback
	// could legitimately return for a genuine match (both source line
	// numbers are always >= 1), so a pointer would only add an
	// indirection with no extra information to carry.
	//
	// Deliberately NOT persisted to review_findings (no migration for
	// these two fields, §22.1.1's own explicit instruction): a finding's
	// position is a function of THIS diff, rendered fresh at THIS
	// posting -- storing yesterday's position would reintroduce exactly
	// the staleness problem content-anchored positioning exists to solve
	// the moment the diff moves again. reviewverdict.go's own upsert loop
	// (UpsertReviewFindingParams) never reads either field.
	StartLine int
	EndLine   int
}

// The errors ValidateFindingInput returns -- one per rejected field,
// mirroring ValidateVerdictInput's own identical "named distinctly so a
// caller can render a specific 400 and a table-driven test can assert
// exactly WHICH check fired" discipline (validate.go).
var (
	ErrInvalidSentinelKind     = errors.New("reviewpost: sentinelKind must be one of coverage/docs_drift, or absent")
	ErrInvalidFindingSeverity  = errors.New("reviewpost: finding severity must be one of low/medium/high")
	ErrEmptyFindingFilePath    = errors.New("reviewpost: finding filePath must not be empty")
	ErrEmptyFindingDescription = errors.New("reviewpost: finding description must not be empty")
	ErrInvalidFindingLine      = errors.New("reviewpost: finding line, if present, must be >= 1")
)

// ValidateFindingInput rejects a malformed finding -- checked in a fixed
// order (SentinelKind, Severity, FilePath, Description, Line), mirroring
// ValidateVerdictInput's own deterministic-first-error discipline exactly.
func ValidateFindingInput(in FindingInput) error {
	if in.SentinelKind != nil {
		switch *in.SentinelKind {
		case SentinelKindCoverage, SentinelKindDocsDrift:
		default:
			return ErrInvalidSentinelKind
		}
	}

	switch in.Severity {
	case review.RiskLevelLow, review.RiskLevelMedium, review.RiskLevelHigh:
	default:
		return ErrInvalidFindingSeverity
	}

	if strings.TrimSpace(in.FilePath) == "" {
		return ErrEmptyFindingFilePath
	}

	if strings.TrimSpace(in.Description) == "" {
		return ErrEmptyFindingDescription
	}

	if in.Line != nil && *in.Line < 1 {
		return ErrInvalidFindingLine
	}

	return nil
}

// BuildFinding is the ONE sanctioned way this package turns an
// ALREADY-VALIDATED FindingInput (ValidateFindingInput must be called
// first) into a Finding -- IdentityHash is populated with EXACTLY
// ComputeFindingIdentity's own return value, never client-supplied,
// mirroring BuildVerdict's own identical "populate the authoritative field
// only via the one pure compute function" discipline (validate.go).
func BuildFinding(in FindingInput) Finding {
	return Finding{
		IdentityHash: ComputeFindingIdentity(in.SentinelKind, in.FilePath, in.Description),
		SentinelKind: in.SentinelKind,
		Severity:     in.Severity,
		FilePath:     in.FilePath,
		Line:         in.Line,
		Description:  in.Description,
		SuggestedFix: in.SuggestedFix,
		Source:       in.Source,
	}
}

// UnverifiedAddition reports whether f is a counter-review addition the
// server could not count as checked -- the findings published marked
// unverified and counted apart (§26.6's amendment). A finding with any
// other source, including none recorded, is never one.
func (f Finding) UnverifiedAddition() bool {
	return f.Source == FindingSourceCounterReview && !f.AdditionCheck.Checked()
}

// findingIdentitySeparator joins ComputeFindingIdentity's three
// normalized components before hashing -- a NUL byte, chosen because it
// can never appear in any of the three normalized components themselves
// (a file path or a description containing a literal NUL byte would
// already be pathological input), so there is no ambiguity between e.g.
// kind="a", path="b/c" and kind="a/b", path="c" the way a plain "/" or ":"
// join could introduce.
const findingIdentitySeparator = "\x00"

// findingIdentityGeneralKind is ComputeFindingIdentity's own fixed
// "sentinel kind" string for an ordinary (non-sentinel) finding -- see
// SentinelKind's own doc comment: this is a fixed STRING used only inside
// the hash input, never a third SentinelKind enum value a caller could
// construct.
const findingIdentityGeneralKind = "general"

// ComputeFindingIdentity is Tension 1's own central resolution (design
// scheme (A), review/doc.go's design call #4 and the plan's own §22.1):
// a finding's identity is a sha256 hash over a canonical join of exactly
// three normalized components -- sentinelKind (or findingIdentityGeneralKind
// for an ordinary, non-sentinel finding), filePath (normalized, see
// normalizeFindingFilePath), and description (normalized, see
// normalizeFindingDescription). Server-computed, ALWAYS -- never accepted
// from a caller (FindingInput carries no IdentityHash field at all) --
// the same "don't trust the model with anything authoritative" discipline
// review.Verdict.Shippable already establishes (verdict.go's own CONTRACT).
//
// Deliberately excludes Line: the whole point of this function existing
// (§22.1: "a file:line-only identity breaks the moment a line shifts...
// the same finding then silently reads as a new one") is that a finding
// re-reported at a SHIFTED line number, with the same kind/file/description,
// must hash identically and therefore be recognized as already answered.
//
// Deliberately INCLUDES filePath (rejected the file-path-independent
// scheme (B) considered in this Step's own design phase): dropping file
// path too would collide two genuinely different findings that happen to
// read alike in two unrelated files (e.g. the same "missing error-path
// test" wording in two different files), silently suppressing a real,
// distinct finding -- a worse failure than occasionally re-showing an
// already-answered one (fail-conservative, mirroring review/doc.go's own
// uniform "never silently reads as safe" policy).
//
// Honest residual (named, not hidden, matching review/doc.go's own
// candor): this survives WHITESPACE-level regeneration of the same
// underlying finding, not PARAPHRASE-level -- an LLM re-describing the
// same issue with materially different wording across two review passes
// will not match. This fails toward safety (occasional re-litigation, an
// under-match), never toward silently losing a legitimate new finding
// (an over-match) -- closable later only by a semantic/embedding match,
// out of this Step's scope.
func ComputeFindingIdentity(sentinelKind *SentinelKind, filePath, description string) string {
	kind := findingIdentityGeneralKind
	if sentinelKind != nil {
		kind = string(*sentinelKind)
	}

	joined := kind + findingIdentitySeparator +
		normalizeFindingFilePath(filePath) + findingIdentitySeparator +
		normalizeFindingDescription(description)

	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:])
}

// normalizeFindingFilePath cleans p (path.Clean -- POSIX-style, repo-
// relative paths always use "/", regardless of the host OS this control
// plane happens to run on, so "path", not "filepath", is deliberate) and
// strips a leading "./", so "./internal/foo.go", "internal/foo.go", and
// "internal//foo.go" all normalize identically.
func normalizeFindingFilePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	cleaned := path.Clean(p)
	return strings.TrimPrefix(cleaned, "./")
}

// normalizeFindingDescription trims, casefolds, and collapses every run of
// whitespace in d to a single space -- strings.Fields already splits on
// any run of Unicode whitespace, so strings.Join(strings.Fields(d), " ")
// is exactly "collapse whitespace", and strings.ToLower is this package's
// deliberately simple stand-in for "casefold" (no new dependency on
// golang.org/x/text/cases for this one normalization step).
func normalizeFindingDescription(d string) string {
	lower := strings.ToLower(strings.TrimSpace(d))
	return strings.Join(strings.Fields(lower), " ")
}
