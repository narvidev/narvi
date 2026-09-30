package reviewtriage_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

func TestDecide_SensitiveGlobDoesNotIncludeContracts(t *testing.T) {
	// Regression pin for the deliberate divergence from
	// internal/domain/autoapproval.DefaultSensitiveTags -- see
	// sensitiveglob.go's own doc comment.
	sig := readable(reviewtriage.Signals{ChangedPaths: []string{"contracts/rest/v1/dtos.schema.json"}})
	got := reviewtriage.Decide(sig, reviewtriage.DefaultConfig())
	if got.Depth != reviewtriage.DepthLight {
		t.Fatalf("a contracts-only change must not trip triage's own sensitive-glob rule, got %q (reason=%q)", got.Depth, got.Reason)
	}
}

func TestMatchDeepPath_Semantics(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"empty pattern never matches", "", "internal/billing/a.go", false},
		{"exact prefix match", "internal/billing", "internal/billing/a.go", true},
		{"prefix match nested deeper", "internal/billing", "internal/billing/sub/a.go", true},
		{"prefix does not match a sibling directory with a shared prefix", "internal/billing", "internal/billingx/a.go", false},
		{"bare file exact match", "internal/billing/a.go", "internal/billing/a.go", true},
		{"single-star glob matches one segment", "internal/billing/*.go", "internal/billing/a.go", true},
		{"single-star glob does not cross a slash", "internal/billing/*.go", "internal/billing/sub/a.go", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig := readable(reviewtriage.Signals{ChangedPaths: []string{tt.path}})
			cfg := reviewtriage.Config{Mode: reviewtriage.ModeAuto, DeepPaths: []string{tt.pattern}}
			got := reviewtriage.Decide(sig, cfg)
			gotMatch := got.Depth == reviewtriage.DepthDeep && got.Reason == reviewtriage.ReasonDeepPathConfig
			if gotMatch != tt.want {
				t.Errorf("matchDeepPath(%q, %q) via Decide = %v, want %v", tt.pattern, tt.path, gotMatch, tt.want)
			}
		})
	}
}
