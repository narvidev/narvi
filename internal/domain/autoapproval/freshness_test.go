package autoapproval_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/review"
)

// freshnessReasons is every Reason CheckFreshness can return besides
// ReasonNone: the freshness prefix of ComputeEligible's criteria.
var freshnessReasons = map[autoapproval.Reason]bool{
	autoapproval.ReasonNotAssessed:           true,
	autoapproval.ReasonStaleVerdict:          true,
	autoapproval.ReasonContextUnknown:        true,
	autoapproval.ReasonBaseSHAUnknown:        true,
	autoapproval.ReasonBaseMoved:             true,
	autoapproval.ReasonAncestorChainUnknown:  true,
	autoapproval.ReasonAncestorChainChanged:  true,
	autoapproval.ReasonPolicyVersionMismatch: true,
}

// eligibilityCorpus is every EligibilityInput the eligibility tests use --
// TestComputeEligible's table and the three tables of
// eligibilityacceptance_test.go -- each with its own configuration.
func eligibilityCorpus() map[string]struct {
	in  autoapproval.EligibilityInput
	cfg autoapproval.EligibilityConfig
} {
	out := map[string]struct {
		in  autoapproval.EligibilityInput
		cfg autoapproval.EligibilityConfig
	}{}
	add := func(name string, in autoapproval.EligibilityInput, cfg autoapproval.EligibilityConfig) {
		out[name] = struct {
			in  autoapproval.EligibilityInput
			cfg autoapproval.EligibilityConfig
		}{in, cfg}
	}
	def := autoapproval.DefaultEligibilityConfig()
	for _, tc := range computeEligibleCases() {
		add("TestComputeEligible/"+tc.name, tc.in, tc.cfg)
	}
	for name, in := range acceptedFalseCases() {
		add("AcceptedFalseMatchesComputeEligible/"+name, in, def)
	}
	for _, tc := range waivesOnlyCases() {
		add("WaivesOnlyShippableAndDiffSize/"+tc.name, tc.in, def)
	}
	for _, tc := range everyCriterionCases() {
		add("EveryCriterionEnumerated/"+string(tc.reason), tc.in, def)
	}
	return out
}

// freshnessProduct is every combination of the freshness fields' own
// equivalence classes, over a clean input: assessed or not; head equal,
// differing, or unrecorded; base ref equal, differing, or unrecorded; base
// commit equal, differing, or unknown on either side; each fast-forward
// confirmation on or off; ancestor chains on each side drawn from none, a
// link, the same link moved, an unknown link, another ref, and two links;
// policy current or not. Every branch of the prefix, and every order
// between two of them, is decided by at least one of these.
func freshnessProduct() []autoapproval.EligibilityInput {
	type pair struct{ verdict, current string }
	heads := []pair{{"h1", "h1"}, {"", "h1"}, {"h1", "h2"}, {"", ""}}
	baseRefs := []pair{{"main", "main"}, {"", "main"}, {"main", "release"}}
	baseSHAs := []pair{{"b1", "b1"}, {"", "b1"}, {"b1", ""}, {"b1", "b2"}}
	chains := [][]review.AncestorLink{
		nil,
		{{Ref: "parent", SHA: "p1"}},
		{{Ref: "parent", SHA: "p2"}},
		{{Ref: "parent", SHA: ""}},
		{{Ref: "other", SHA: "p1"}},
		{{Ref: "parent", SHA: "p1"}, {Ref: "grandparent", SHA: "g1"}},
	}
	var out []autoapproval.EligibilityInput
	for _, assessed := range []bool{true, false} {
		for _, head := range heads {
			for _, ref := range baseRefs {
				for _, sha := range baseSHAs {
					for _, baseFF := range []bool{false, true} {
						for _, vc := range chains {
							for _, cc := range chains {
								for _, chainFF := range []bool{false, true} {
									for _, policy := range []int{autoapproval.CurrentPolicyVersion, autoapproval.CurrentPolicyVersion - 1, autoapproval.CurrentPolicyVersion + 1} {
										in := cleanInput()
										in.VerdictAssessed = assessed
										in.VerdictHeadSHA, in.CurrentHeadSHA = head.verdict, head.current
										in.VerdictBaseRef, in.CurrentBaseRef = ref.verdict, ref.current
										in.VerdictBaseSHA, in.CurrentBaseSHA = sha.verdict, sha.current
										in.BaseAdvancedWithoutRewrite = baseFF
										in.VerdictAncestorChain, in.CurrentAncestorChain = vc, cc
										in.AncestorChainAdvancedWithoutRewrite = chainFF
										in.VerdictPolicyVersion = policy
										out = append(out, in)
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return out
}

// prefixReason is the freshness-prefix part of an eligibility answer:
// the reason itself when it is one of the prefix's, ReasonNone otherwise
// (the input cleared the prefix and failed, or passed, something after it).
func prefixReason(r autoapproval.Reason) autoapproval.Reason {
	if freshnessReasons[r] {
		return r
	}
	return autoapproval.ReasonNone
}

// assertPrefixEquivalent checks one input: with the needs-human label on,
// the label wins before any freshness check; with it off, the freshness
// part of the merge engine's answer -- with and without an acceptance --
// is exactly CheckFreshness's.
func assertPrefixEquivalent(t *testing.T, name string, in autoapproval.EligibilityInput, cfg autoapproval.EligibilityConfig) {
	t.Helper()
	if in.HasNeedsHumanLabel {
		if _, got := autoapproval.ComputeEligible(in, cfg); got != autoapproval.ReasonNeedsHumanLabel {
			t.Errorf("%s: with the needs-human label, reason = %q, want the label first", name, got)
		}
		in.HasNeedsHumanLabel = false
	}
	want := autoapproval.CheckFreshness(in.Freshness())
	eligible, got := autoapproval.ComputeEligible(in, cfg)
	if prefixReason(got) != want {
		t.Errorf("%s: ComputeEligible reason %q (freshness part %q), CheckFreshness %q -- the merge path and the one comparison disagree", name, got, prefixReason(got), want)
	}
	if want != autoapproval.ReasonNone && eligible {
		t.Errorf("%s: eligible with CheckFreshness refusing on %q", name, want)
	}
	for _, accepted := range []bool{false, true} {
		_, gotA, _ := autoapproval.ComputeEligibleWithAcceptance(in, cfg, accepted)
		if prefixReason(gotA) != want {
			t.Errorf("%s: ComputeEligibleWithAcceptance(accepted=%v) reason %q, CheckFreshness %q", name, accepted, gotA, want)
		}
	}
}

// TestCheckFreshness_EquivalentToEligibilityPrefix pins row 182's "one
// comparison, not two" (technical plan §43.20, §21.1b): over the whole
// eligibility test corpus, and over every combination of the freshness
// fields, the part of ComputeEligible's (and ComputeEligibleWithAcceptance's)
// answer that is about freshness is exactly CheckFreshness's -- the same
// reason, in the same order. A second comparison that drifts from the
// first in any branch fails here.
func TestCheckFreshness_EquivalentToEligibilityPrefix(t *testing.T) {
	t.Parallel()

	corpus := eligibilityCorpus()
	if len(corpus) < 60 {
		t.Fatalf("eligibility corpus has %d inputs, want every table's (60+)", len(corpus))
	}
	for name, c := range corpus {
		assertPrefixEquivalent(t, name, c.in, c.cfg)
	}

	cfg := autoapproval.DefaultEligibilityConfig()
	seen := map[autoapproval.Reason]int{}
	for i, in := range freshnessProduct() {
		assertPrefixEquivalent(t, fmt.Sprintf("product[%d]", i), in, cfg)
		seen[autoapproval.CheckFreshness(in.Freshness())]++
	}
	// The product decides every branch: each reason, and a pass.
	for reason := range freshnessReasons {
		if seen[reason] == 0 {
			t.Errorf("the freshness product never yields %q -- a branch left undecided", reason)
		}
	}
	if seen[autoapproval.ReasonNone] == 0 {
		t.Error("the freshness product never passes -- current is never decided")
	}
}

// TestCheckFreshness_EveryReason isolates each freshness outcome from a
// fresh input, one field at a time.
func TestCheckFreshness_EveryReason(t *testing.T) {
	t.Parallel()

	fresh := cleanInput().Freshness()
	tests := []struct {
		name   string
		mutate func(*autoapproval.FreshnessInput)
		want   autoapproval.Reason
	}{
		{"fresh", func(*autoapproval.FreshnessInput) {}, autoapproval.ReasonNone},
		{"not assessed", func(f *autoapproval.FreshnessInput) { f.VerdictAssessed = false }, autoapproval.ReasonNotAssessed},
		{"head moved", func(f *autoapproval.FreshnessInput) { f.CurrentHeadSHA = "moved" }, autoapproval.ReasonStaleVerdict},
		{"head never recorded", func(f *autoapproval.FreshnessInput) { f.VerdictHeadSHA, f.CurrentHeadSHA = "", "" }, autoapproval.ReasonStaleVerdict},
		{"no context recorded", func(f *autoapproval.FreshnessInput) { f.VerdictBaseRef = "" }, autoapproval.ReasonContextUnknown},
		{"live base commit unknown", func(f *autoapproval.FreshnessInput) { f.CurrentBaseSHA = "" }, autoapproval.ReasonBaseSHAUnknown},
		{"recorded base commit unknown", func(f *autoapproval.FreshnessInput) { f.VerdictBaseSHA = "" }, autoapproval.ReasonBaseSHAUnknown},
		{"retargeted", func(f *autoapproval.FreshnessInput) { f.CurrentBaseRef = "release" }, autoapproval.ReasonBaseMoved},
		{"base moved, not confirmed forward", func(f *autoapproval.FreshnessInput) { f.CurrentBaseSHA = "base-new" }, autoapproval.ReasonBaseMoved},
		{"base moved, confirmed forward", func(f *autoapproval.FreshnessInput) {
			f.CurrentBaseSHA = "base-new"
			f.BaseAdvancedWithoutRewrite = true
		}, autoapproval.ReasonNone},
		{"ancestor link unknown", func(f *autoapproval.FreshnessInput) {
			f.VerdictAncestorChain = []review.AncestorLink{{Ref: "parent", SHA: ""}}
			f.CurrentAncestorChain = []review.AncestorLink{{Ref: "parent", SHA: "p1"}}
		}, autoapproval.ReasonAncestorChainUnknown},
		{"ancestor chain gained a link", func(f *autoapproval.FreshnessInput) {
			f.CurrentAncestorChain = []review.AncestorLink{{Ref: "parent", SHA: "p1"}}
		}, autoapproval.ReasonAncestorChainChanged},
		{"ancestor moved, confirmed forward", func(f *autoapproval.FreshnessInput) {
			f.VerdictAncestorChain = []review.AncestorLink{{Ref: "parent", SHA: "p1"}}
			f.CurrentAncestorChain = []review.AncestorLink{{Ref: "parent", SHA: "p2"}}
			f.AncestorChainAdvancedWithoutRewrite = true
		}, autoapproval.ReasonNone},
		{"policy bumped", func(f *autoapproval.FreshnessInput) { f.VerdictPolicyVersion = autoapproval.CurrentPolicyVersion - 1 }, autoapproval.ReasonPolicyVersionMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := fresh
			tc.mutate(&in)
			if got := autoapproval.CheckFreshness(in); got != tc.want {
				t.Errorf("CheckFreshness = %q, want %q", got, tc.want)
			}
		})
	}
}

// freshnessOnlyIdentifiers are the EligibilityInput fields and helpers only
// the freshness prefix reads. computeEligibleCore touching any of them
// directly means it compares freshness a second time, beside CheckFreshness.
var freshnessOnlyIdentifiers = map[string]bool{
	"VerdictAssessed":                     true,
	"VerdictHeadSHA":                      true,
	"CurrentHeadSHA":                      true,
	"VerdictBaseRef":                      true,
	"CurrentBaseRef":                      true,
	"VerdictBaseSHA":                      true,
	"CurrentBaseSHA":                      true,
	"VerdictAncestorChain":                true,
	"CurrentAncestorChain":                true,
	"VerdictPolicyVersion":                true,
	"BaseAdvancedWithoutRewrite":          true,
	"AncestorChainAdvancedWithoutRewrite": true,
	"CurrentPolicyVersion":                true,
	"ancestorChainEqual":                  true,
	"ancestorChainHasUnknownLink":         true,
}

// TestComputeEligibleCore_ComparesFreshnessOnlyThroughCheckFreshness pins
// the merge path's side of "one comparison": computeEligibleCore calls
// CheckFreshness and reads none of the freshness fields itself. A copy of
// the prefix pasted back into it -- even an identical one, which no
// behavioural test can tell apart -- fails here.
func TestComputeEligibleCore_ComparesFreshnessOnlyThroughCheckFreshness(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "eligibility.go")
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var core *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "computeEligibleCore" {
			core = fn
		}
	}
	if core == nil {
		t.Fatalf("%s declares no computeEligibleCore", path)
	}
	calls := 0
	ast.Inspect(core.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "CheckFreshness" {
				calls++
			}
		case *ast.SelectorExpr:
			if freshnessOnlyIdentifiers[n.Sel.Name] {
				t.Errorf("computeEligibleCore reads %s itself -- freshness must be compared only through CheckFreshness", n.Sel.Name)
			}
		case *ast.Ident:
			if freshnessOnlyIdentifiers[n.Name] {
				t.Errorf("computeEligibleCore uses %s itself -- freshness must be compared only through CheckFreshness", n.Name)
			}
		}
		return true
	})
	if calls != 1 {
		t.Errorf("computeEligibleCore calls CheckFreshness %d times, want exactly once", calls)
	}
}
