package reviewtriage_test

import (
	"reflect"
	"testing"

	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

func TestNewDecisionRecord(t *testing.T) {
	sig := reviewtriage.Signals{ChangedPaths: []string{"migrations/x.sql"}}
	cfg := reviewtriage.DefaultConfig()
	decision := reviewtriage.Decide(sig, cfg)

	t.Run("not floored", func(t *testing.T) {
		got := reviewtriage.NewDecisionRecord(decision, cfg, decision.Depth, reviewtriage.Provenance{}, nil, nil, 0, false, false, nil, nil)
		want := reviewtriage.DecisionRecord{
			Depth:                "deep",
			Reason:               string(reviewtriage.ReasonSensitiveGlob),
			MatchedSensitiveTags: []string{"migrations"},
			ChangedLines:         0,
			DistinctRoots:        1,
			Mode:                 "auto",
			Floored:              false,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("NewDecisionRecord() = %#v, want %#v", got, want)
		}
	})

	t.Run("floored by a higher-ranked prior depth", func(t *testing.T) {
		lightSig := reviewtriage.Signals{ChangedPaths: []string{"internal/app/foo/a.go"}}
		lightDecision := reviewtriage.Decide(lightSig, cfg)
		floored := reviewtriage.Floor(lightDecision.Depth, reviewtriage.DepthDeep)

		got := reviewtriage.NewDecisionRecord(lightDecision, cfg, floored, reviewtriage.Provenance{}, nil, nil, 0, false, false, nil, nil)
		if got.Depth != "deep" {
			t.Errorf("Depth = %q, want deep", got.Depth)
		}
		if !got.Floored {
			t.Error("Floored = false, want true (final depth differs from the fresh decision)")
		}
	})

	// D4 (nice-to-have adversarial-review fix): ResolvedModelID/
	// ResolvedEffort record ModelAndEffort's own actual output for THIS
	// turn -- both nil in, both empty out (the light path, or an
	// unconfigured deep-tier model); both set in, both recorded verbatim
	// out.
	t.Run("resolved model/effort recorded verbatim", func(t *testing.T) {
		modelID := "anthropic/claude-frontier"
		effort := reviewtriage.EffortHigh
		got := reviewtriage.NewDecisionRecord(decision, cfg, decision.Depth, reviewtriage.Provenance{}, &modelID, &effort, 0, false, false, nil, nil)
		if got.ResolvedModelID != modelID {
			t.Errorf("ResolvedModelID = %q, want %q", got.ResolvedModelID, modelID)
		}
		if got.ResolvedEffort != effort {
			t.Errorf("ResolvedEffort = %q, want %q", got.ResolvedEffort, effort)
		}
	})

	t.Run("nil resolved model/effort records as empty, never a nil-pointer panic", func(t *testing.T) {
		got := reviewtriage.NewDecisionRecord(decision, cfg, decision.Depth, reviewtriage.Provenance{}, nil, nil, 0, false, false, nil, nil)
		if got.ResolvedModelID != "" || got.ResolvedEffort != "" {
			t.Errorf("ResolvedModelID/ResolvedEffort = %q/%q, want both empty", got.ResolvedModelID, got.ResolvedEffort)
		}
	})

	// changedFilesCount (§21.1's own filesChanged drift canary) is
	// recorded verbatim -- this record's own sole job for that field is
	// to carry it, unmodified, from turn-creation time to verdict-post
	// time (DecisionRecord.ChangedFilesCount's own doc comment).
	t.Run("changed files count recorded verbatim", func(t *testing.T) {
		got := reviewtriage.NewDecisionRecord(decision, cfg, decision.Depth, reviewtriage.Provenance{}, nil, nil, 42, false, false, nil, nil)
		if got.ChangedFilesCount != 42 {
			t.Errorf("ChangedFilesCount = %d, want 42", got.ChangedFilesCount)
		}
	})

	t.Run("zero changed files count records as zero, indistinguishable from unset", func(t *testing.T) {
		got := reviewtriage.NewDecisionRecord(decision, cfg, decision.Depth, reviewtriage.Provenance{}, nil, nil, 0, false, false, nil, nil)
		if got.ChangedFilesCount != 0 {
			t.Errorf("ChangedFilesCount = %d, want 0", got.ChangedFilesCount)
		}
	})

	// D4 (adversarial review of PR #182, MEDIUM): DiffEmpty/DiffTruncated
	// are recorded verbatim -- this record's own sole job for these two
	// fields, exactly like ChangedFilesCount above, is to carry them
	// unmodified from turn-creation time to verdict-post time
	// (DecisionRecord.DiffEmpty/DiffTruncated's own doc comment).
	t.Run("diff-delivery facts recorded verbatim: diff empty", func(t *testing.T) {
		got := reviewtriage.NewDecisionRecord(decision, cfg, decision.Depth, reviewtriage.Provenance{}, nil, nil, 0, true, false, nil, nil)
		if !got.DiffEmpty {
			t.Error("DiffEmpty = false, want true")
		}
		if got.DiffTruncated {
			t.Error("DiffTruncated = true, want false")
		}
	})

	t.Run("diff-delivery facts recorded verbatim: diff truncated", func(t *testing.T) {
		got := reviewtriage.NewDecisionRecord(decision, cfg, decision.Depth, reviewtriage.Provenance{}, nil, nil, 0, false, true, nil, nil)
		if got.DiffEmpty {
			t.Error("DiffEmpty = true, want false")
		}
		if !got.DiffTruncated {
			t.Error("DiffTruncated = false, want true")
		}
	})

	t.Run("diff fully delivered records both facts as false", func(t *testing.T) {
		got := reviewtriage.NewDecisionRecord(decision, cfg, decision.Depth, reviewtriage.Provenance{}, nil, nil, 0, false, false, nil, nil)
		if got.DiffEmpty || got.DiffTruncated {
			t.Errorf("DiffEmpty/DiffTruncated = %v/%v, want both false", got.DiffEmpty, got.DiffTruncated)
		}
	})

	// §31.6: ArchDecisionTags/ArchDecisionRoots are carried onto the
	// record VERBATIM -- this function does no classification of its
	// own (the caller has already run autoapproval.ClassifyChangedPaths/
	// ClassifyChangedRoots before calling this function), exactly like
	// ChangedFilesCount/DiffEmpty/DiffTruncated above.
	t.Run("arch-decision tags/roots recorded verbatim", func(t *testing.T) {
		tags := []string{"auth", "migrations"}
		roots := []string{"internal", "migrations"}
		got := reviewtriage.NewDecisionRecord(decision, cfg, decision.Depth, reviewtriage.Provenance{}, nil, nil, 0, false, false, tags, roots)
		if !reflect.DeepEqual(got.ArchDecisionTags, tags) {
			t.Errorf("ArchDecisionTags = %v, want %v", got.ArchDecisionTags, tags)
		}
		if !reflect.DeepEqual(got.ArchDecisionRoots, roots) {
			t.Errorf("ArchDecisionRoots = %v, want %v", got.ArchDecisionRoots, roots)
		}
	})

	t.Run("nil arch-decision tags/roots record as nil, never a nil-pointer panic", func(t *testing.T) {
		got := reviewtriage.NewDecisionRecord(decision, cfg, decision.Depth, reviewtriage.Provenance{}, nil, nil, 0, false, false, nil, nil)
		if got.ArchDecisionTags != nil || got.ArchDecisionRoots != nil {
			t.Errorf("ArchDecisionTags/ArchDecisionRoots = %v/%v, want both nil", got.ArchDecisionTags, got.ArchDecisionRoots)
		}
	})
}
