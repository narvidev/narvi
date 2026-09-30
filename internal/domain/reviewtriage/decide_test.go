package reviewtriage_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

// readable marks sig's input as read in full -- the reading every case of
// TestDecide_Default is about. An unset InputRead is itself an unreadable
// input (review.InputRead's own doc comment), covered on its own in
// TestDecide_UnreadableInput.
func readable(sig reviewtriage.Signals) reviewtriage.Signals {
	if sig.InputRead == "" {
		sig.InputRead = review.InputReadComplete
	}
	return sig
}

func TestDecide_Default(t *testing.T) {
	tests := []struct {
		name       string
		sig        reviewtriage.Signals
		cfg        reviewtriage.Config
		wantDepth  reviewtriage.ReviewDepth
		wantReason reviewtriage.Reason
	}{
		{
			name:       "a readable input with no signals at all routes light",
			sig:        reviewtriage.Signals{},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthLight,
			wantReason: reviewtriage.ReasonLightDefault,
		},
		{
			name: "small, unremarkable diff routes light",
			sig: reviewtriage.Signals{
				Additions:    10,
				Deletions:    5,
				ChangedPaths: []string{"internal/app/foo/bar.go", "internal/app/foo/bar_test.go"},
			},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthLight,
			wantReason: reviewtriage.ReasonLightDefault,
		},
		{
			// Pin: sensitive-glob alone triggers deep even under the
			// line/root thresholds (task's own mutation-testing
			// requirement #1). One file, one root, zero lines changed,
			// still deep.
			name: "sensitive glob alone triggers deep under every other threshold",
			sig: reviewtriage.Signals{
				ChangedPaths: []string{"migrations/000080_x.up.sql"},
			},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonSensitiveGlob,
		},
		{
			name: "auth surface is a sensitive glob",
			sig: reviewtriage.Signals{
				ChangedPaths: []string{"internal/domain/authz/authorize.go"},
			},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonSensitiveGlob,
		},
		{
			name: "CI workflow is a sensitive glob (via infra)",
			sig: reviewtriage.Signals{
				ChangedPaths: []string{".github/workflows/ci.yml"},
			},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonSensitiveGlob,
		},
		{
			name: "contracts path is NOT in triage's own sensitive set (deliberate divergence from auto-approval)",
			sig: reviewtriage.Signals{
				ChangedPaths: []string{"contracts/rest/v1/dtos.schema.json"},
			},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthLight,
			wantReason: reviewtriage.ReasonLightDefault,
		},
		{
			// Pin: >600 lines triggers deep even with 1 root and no
			// sensitive glob (mutation-testing requirement #2).
			name: "601 changed lines, one root, no sensitive glob still routes deep",
			sig: reviewtriage.Signals{
				Additions:    500,
				Deletions:    101,
				ChangedPaths: []string{"internal/app/foo/a.go"},
				FileLines:    []reviewtriage.FileLines{{Path: "internal/app/foo/a.go", Added: 500, Deleted: 101}},
			},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonChangedLinesOver,
		},
		{
			name: "exactly 600 changed lines stays light (boundary)",
			sig: reviewtriage.Signals{
				Additions:    500,
				Deletions:    100,
				ChangedPaths: []string{"internal/app/foo/a.go"},
				FileLines:    []reviewtriage.FileLines{{Path: "internal/app/foo/a.go", Added: 500, Deleted: 100}},
			},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthLight,
			wantReason: reviewtriage.ReasonLightDefault,
		},
		{
			// Pin: >=3 roots triggers deep even under 600 lines
			// (mutation-testing requirement #3).
			name: "three distinct top-level roots under 600 lines still routes deep",
			sig: reviewtriage.Signals{
				Additions: 5,
				Deletions: 5,
				ChangedPaths: []string{
					"internal/app/foo/a.go",
					"cmd/bar/main.go",
					"docs/readme.md",
				},
			},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonRootDispersion,
		},
		{
			name: "exactly two distinct top-level roots stays light (boundary)",
			sig: reviewtriage.Signals{
				ChangedPaths: []string{
					"internal/app/foo/a.go",
					"cmd/bar/main.go",
				},
			},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthLight,
			wantReason: reviewtriage.ReasonLightDefault,
		},
		{
			name: "repeated paths under the same root count as one root",
			sig: reviewtriage.Signals{
				ChangedPaths: []string{
					"internal/app/foo/a.go",
					"internal/app/foo/b.go",
					"internal/app/bar/c.go",
				},
			},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthLight,
			wantReason: reviewtriage.ReasonLightDefault,
		},
		{
			name: "prior high verdict alone routes deep",
			sig: reviewtriage.Signals{
				PriorVerdictRiskHigh: true,
				ChangedPaths:         []string{"internal/app/foo/a.go"},
			},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonPriorHighVerdict,
		},
		{
			name: "needs-human label alone routes deep",
			sig: reviewtriage.Signals{
				NeedsHumanLabelPresent: true,
				ChangedPaths:           []string{"internal/app/foo/a.go"},
			},
			cfg:        reviewtriage.DefaultConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonNeedsHumanLabel,
		},
		{
			name: "repo-configured deepPaths entry (prefix form) routes deep",
			sig: reviewtriage.Signals{
				ChangedPaths: []string{"internal/billing/charge.go"},
			},
			cfg:        reviewtriage.Config{Mode: reviewtriage.ModeAuto, DeepPaths: []string{"internal/billing"}},
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonDeepPathConfig,
		},
		{
			name: "repo-configured deepPaths entry (glob form) routes deep",
			sig: reviewtriage.Signals{
				ChangedPaths: []string{"internal/billing/charge.go"},
			},
			cfg:        reviewtriage.Config{Mode: reviewtriage.ModeAuto, DeepPaths: []string{"internal/billing/*.go"}},
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonDeepPathConfig,
		},
		{
			name: "deepPaths entry that does not match anything stays light",
			sig: reviewtriage.Signals{
				ChangedPaths: []string{"internal/app/foo/a.go"},
			},
			cfg:        reviewtriage.Config{Mode: reviewtriage.ModeAuto, DeepPaths: []string{"internal/billing"}},
			wantDepth:  reviewtriage.DepthLight,
			wantReason: reviewtriage.ReasonLightDefault,
		},
		{
			name: "mode=always_light overrides every other signal",
			sig: reviewtriage.Signals{
				ChangedPaths:         []string{"migrations/000080_x.up.sql"},
				Additions:            10000,
				PriorVerdictRiskHigh: true,
			},
			cfg:        reviewtriage.Config{Mode: reviewtriage.ModeAlwaysLight},
			wantDepth:  reviewtriage.DepthLight,
			wantReason: reviewtriage.ReasonAlwaysLightConfig,
		},
		{
			name:       "mode=always_deep overrides an otherwise-trivial diff",
			sig:        reviewtriage.Signals{},
			cfg:        reviewtriage.Config{Mode: reviewtriage.ModeAlwaysDeep},
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonAlwaysDeepConfig,
		},
		{
			name:       "unrecognized mode falls back to auto, never a silent always_deep/always_light",
			sig:        reviewtriage.Signals{},
			cfg:        reviewtriage.Config{Mode: reviewtriage.Mode("bogus")},
			wantDepth:  reviewtriage.DepthLight,
			wantReason: reviewtriage.ReasonLightDefault,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewtriage.Decide(readable(tt.sig), tt.cfg)
			if got.Depth != tt.wantDepth {
				t.Errorf("Decide().Depth = %q, want %q (reason=%q)", got.Depth, tt.wantDepth, got.Reason)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("Decide().Reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}

// TestDecide_ModeCheckedBeforeAnySignal pins the precedence order itself:
// mutating Decide's own switch to run AFTER the sensitive-glob check
// (rather than before) must fail this test.
func TestDecide_ModeCheckedBeforeAnySignal(t *testing.T) {
	sig := readable(reviewtriage.Signals{ChangedPaths: []string{"migrations/x.sql"}})
	got := reviewtriage.Decide(sig, reviewtriage.Config{Mode: reviewtriage.ModeAlwaysLight})
	if got.Depth != reviewtriage.DepthLight {
		t.Fatalf("mode=always_light must override a sensitive-glob hit, got %q", got.Depth)
	}
}

// TestDecide_ChangedLinesIsSum pins that both counts are additions PLUS
// deletions, not either alone -- the recorded ChangedLines (GitHub's own
// count) and the routed SourceLines (the diff's): mutating either sum into
// one operand must fail this test (300+301 exceeds 600 only when summed).
func TestDecide_ChangedLinesIsSum(t *testing.T) {
	sig := readable(reviewtriage.Signals{
		Additions:    300,
		Deletions:    301,
		ChangedPaths: []string{"internal/app/foo/a.go"},
		FileLines:    []reviewtriage.FileLines{{Path: "internal/app/foo/a.go", Added: 300, Deleted: 301}},
	})
	got := reviewtriage.Decide(sig, reviewtriage.DefaultConfig())
	if got.Depth != reviewtriage.DepthDeep {
		t.Fatalf("300 added + 301 deleted = 601 > 600 must route deep, got %q (source=%d)", got.Depth, got.SourceLines)
	}
	if got.ChangedLines != 601 || got.SourceLines != 601 || got.DiffLines != 601 {
		t.Fatalf("ChangedLines/SourceLines/DiffLines = %d/%d/%d, want 601 each", got.ChangedLines, got.SourceLines, got.DiffLines)
	}
}
