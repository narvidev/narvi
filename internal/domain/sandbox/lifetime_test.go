package sandbox_test

import (
	"slices"
	"testing"

	"github.com/narvidev/narvi/internal/domain/sandbox"
)

// TestLifetimeKindFor pins which kind a session's sandbox is given: review
// for a pull request's review session, default for every other one.
func TestLifetimeKindFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		isReview bool
		want     sandbox.LifetimeKind
	}{
		{"a review session", true, sandbox.LifetimeKindReview},
		{"any other session", false, sandbox.LifetimeKindDefault},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sandbox.LifetimeKindFor(tc.isReview); got != tc.want {
				t.Errorf("LifetimeKindFor(%v) = %q, want %q", tc.isReview, got, tc.want)
			}
		})
	}
}

// TestAllLifetimeKinds pins the kinds platform.Timeouts.Validate checks:
// every kind LifetimeKindFor can return is among them, each once, so no
// kind's lifetime escapes the chain.
func TestAllLifetimeKinds(t *testing.T) {
	t.Parallel()

	all := sandbox.AllLifetimeKinds()
	want := []sandbox.LifetimeKind{sandbox.LifetimeKindDefault, sandbox.LifetimeKindReview}
	if !slices.Equal(all, want) {
		t.Fatalf("AllLifetimeKinds() = %v, want %v", all, want)
	}
	for _, isReview := range []bool{false, true} {
		if kind := sandbox.LifetimeKindFor(isReview); !slices.Contains(all, kind) {
			t.Errorf("LifetimeKindFor(%v) = %q, which AllLifetimeKinds does not list", isReview, kind)
		}
	}
}
