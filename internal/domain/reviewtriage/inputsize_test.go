package reviewtriage_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

// addedFileDiff renders one diff section adding n lines to path, each
// line body produced by line(i) -- enough of a real unified diff for
// ExtractFileLines to attribute the lines to path.
func addedFileDiff(path string, n int, line func(i int) string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, path, path, path, n)
	for i := 0; i < n; i++ {
		b.WriteString("+" + line(i) + "\n")
	}
	return b.String()
}

func plainLine(i int) string { return fmt.Sprintf("line %d", i) }

// sizedSignals builds readable Signals for diff, with the pull request's
// own reported additions equal to the lines the diff adds.
func sizedSignals(diff string) reviewtriage.Signals {
	files := reviewtriage.ExtractFileLines(diff)
	added := 0
	for _, f := range files {
		added += f.Added
	}
	return reviewtriage.Signals{
		Additions:    added,
		ChangedPaths: reviewtriage.ExtractChangedPaths(diff),
		FileLines:    files,
		InputRead:    review.InputReadComplete,
	}
}

func deploymentConfig() reviewtriage.Config {
	cfg := reviewtriage.DefaultConfig()
	cfg.SizeExclusions = reviewtriage.DefaultSizeExclusions()
	return cfg
}

// TestDecide_UnreadableInput pins §26.3's owner decision: an input that
// could not be read in full routes deep under a reason of its own, never
// light under the reason a one-line fix gets -- and the decision carries
// the cause whatever rule decided.
func TestDecide_UnreadableInput(t *testing.T) {
	tests := []struct {
		name       string
		sig        reviewtriage.Signals
		cfg        reviewtriage.Config
		wantDepth  reviewtriage.ReviewDepth
		wantReason reviewtriage.Reason
	}{
		{
			name:       "pull request unreadable: no size, no paths",
			sig:        reviewtriage.Signals{InputRead: review.InputReadPRUnreadable},
			cfg:        deploymentConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonInputUnreadable,
		},
		{
			name:       "diff unreadable: the size survives, the paths do not",
			sig:        reviewtriage.Signals{Additions: 3, Deletions: 1, InputRead: review.InputReadDiffUnreadable},
			cfg:        deploymentConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonInputUnreadable,
		},
		{
			name:       "truncated file list counts as unreadable",
			sig:        reviewtriage.Signals{Additions: 5, ChangedPaths: []string{"internal/app/a.go"}, InputRead: review.InputReadDiffTruncated},
			cfg:        deploymentConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonInputUnreadable,
		},
		{
			name:       "no read made at all",
			sig:        reviewtriage.Signals{InputRead: review.InputReadNotFetched},
			cfg:        deploymentConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonInputUnreadable,
		},
		{
			name:       "an unset cause is unreadable, never a small change",
			sig:        reviewtriage.Signals{},
			cfg:        deploymentConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonInputUnreadable,
		},
		{
			name:       "a genuinely empty change is told apart from a failed read",
			sig:        reviewtriage.Signals{InputRead: review.InputReadEmpty},
			cfg:        deploymentConfig(),
			wantDepth:  reviewtriage.DepthLight,
			wantReason: reviewtriage.ReasonLightDefault,
		},
		{
			name:       "an explicit always_light override still wins over an unreadable diff",
			sig:        reviewtriage.Signals{Additions: 3, InputRead: review.InputReadDiffUnreadable},
			cfg:        reviewtriage.Config{Mode: reviewtriage.ModeAlwaysLight},
			wantDepth:  reviewtriage.DepthLight,
			wantReason: reviewtriage.ReasonAlwaysLightConfig,
		},
		{
			name:       "always_deep keeps its own reason",
			sig:        reviewtriage.Signals{InputRead: review.InputReadPRUnreadable},
			cfg:        reviewtriage.Config{Mode: reviewtriage.ModeAlwaysDeep},
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonAlwaysDeepConfig,
		},
		{
			name:       "an unreadable input is routed before the size, which it cannot trust",
			sig:        reviewtriage.Signals{Additions: 700, ChangedPaths: []string{"internal/app/a.go"}, InputRead: review.InputReadDiffTruncated},
			cfg:        deploymentConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonInputUnreadable,
		},
		{
			name:       "a sensitive path seen in a truncated list is a real signal",
			sig:        reviewtriage.Signals{ChangedPaths: []string{"migrations/000001_x.up.sql"}, InputRead: review.InputReadDiffTruncated},
			cfg:        deploymentConfig(),
			wantDepth:  reviewtriage.DepthDeep,
			wantReason: reviewtriage.ReasonSensitiveGlob,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewtriage.Decide(tt.sig, tt.cfg)
			if got.Depth != tt.wantDepth || got.Reason != tt.wantReason {
				t.Errorf("Decide() = (%q, %q), want (%q, %q)", got.Depth, got.Reason, tt.wantDepth, tt.wantReason)
			}
			if got.InputRead != tt.sig.InputRead {
				t.Errorf("Decide().InputRead = %q, want the cause passed in, %q", got.InputRead, tt.sig.InputRead)
			}
		})
	}
}

// TestDecide_SourceSize pins §26.3's size rule: the count that routes
// leaves out the files the deployment's test, documentation and
// generated-file patterns match, while the path signals keep reading every
// changed path.
func TestDecide_SourceSize(t *testing.T) {
	source40 := addedFileDiff("internal/app/billing/charge.go", 40, plainLine)
	tests600 := addedFileDiff("internal/app/billing/charge_test.go", 600, plainLine)
	docs700 := addedFileDiff("docs/guide/billing.md", 700, plainLine)
	lock900 := addedFileDiff("web/package-lock.json", 900, plainLine)
	source601 := addedFileDiff("internal/app/billing/charge.go", 601, plainLine)

	tests := []struct {
		name        string
		sig         reviewtriage.Signals
		cfg         reviewtriage.Config
		wantDepth   reviewtriage.ReviewDepth
		wantReason  reviewtriage.Reason
		wantChanged int
		wantSource  int
	}{
		{
			name:        "40 source lines with 600 test lines route on 40",
			sig:         sizedSignals(source40 + tests600),
			cfg:         deploymentConfig(),
			wantDepth:   reviewtriage.DepthLight,
			wantReason:  reviewtriage.ReasonLightDefault,
			wantChanged: 640,
			wantSource:  40,
		},
		{
			name:        "documentation and generated lockfile lines are left out too",
			sig:         sizedSignals(source40 + lock900),
			cfg:         deploymentConfig(),
			wantDepth:   reviewtriage.DepthLight,
			wantReason:  reviewtriage.ReasonLightDefault,
			wantChanged: 940,
			wantSource:  40,
		},
		{
			name:        "601 source lines still route deep beside test lines",
			sig:         sizedSignals(source601 + tests600),
			cfg:         deploymentConfig(),
			wantDepth:   reviewtriage.DepthDeep,
			wantReason:  reviewtriage.ReasonChangedLinesOver,
			wantChanged: 1201,
			wantSource:  601,
		},
		{
			name:        "no deployment patterns counts every line",
			sig:         sizedSignals(source40 + tests600),
			cfg:         reviewtriage.DefaultConfig(),
			wantDepth:   reviewtriage.DepthDeep,
			wantReason:  reviewtriage.ReasonChangedLinesOver,
			wantChanged: 640,
			wantSource:  640,
		},
		{
			name:        "a sensitive path inside a test directory still routes deep",
			sig:         sizedSignals(addedFileDiff("internal/auth/testdata/expired_token.json", 5, plainLine)),
			cfg:         deploymentConfig(),
			wantDepth:   reviewtriage.DepthDeep,
			wantReason:  reviewtriage.ReasonSensitiveGlob,
			wantChanged: 5,
			wantSource:  0,
		},
		{
			name:        "root dispersion still counts a documentation root",
			sig:         sizedSignals(addedFileDiff("internal/a.go", 2, plainLine) + addedFileDiff("cmd/b/main.go", 2, plainLine) + docs700),
			cfg:         deploymentConfig(),
			wantDepth:   reviewtriage.DepthDeep,
			wantReason:  reviewtriage.ReasonRootDispersion,
			wantChanged: 704,
			wantSource:  4,
		},
		{
			name:        "a rename moving source into a test directory still counts",
			sig:         renamedIntoTests(),
			cfg:         deploymentConfig(),
			wantDepth:   reviewtriage.DepthDeep,
			wantReason:  reviewtriage.ReasonChangedLinesOver,
			wantChanged: 700,
			wantSource:  700,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewtriage.Decide(tt.sig, tt.cfg)
			if got.Depth != tt.wantDepth || got.Reason != tt.wantReason {
				t.Errorf("Decide() = (%q, %q), want (%q, %q)", got.Depth, got.Reason, tt.wantDepth, tt.wantReason)
			}
			if got.ChangedLines != tt.wantChanged || got.SourceLines != tt.wantSource {
				t.Errorf("ChangedLines/SourceLines = %d/%d, want %d/%d", got.ChangedLines, got.SourceLines, tt.wantChanged, tt.wantSource)
			}
		})
	}
}

// renamedIntoTests is a rename with a content change that moves a source
// file under test/: its old path is source, so its lines still count.
func renamedIntoTests() reviewtriage.Signals {
	var b strings.Builder
	b.WriteString("diff --git a/internal/app/core.go b/test/core_test.go\n" +
		"similarity index 51%\n" +
		"rename from internal/app/core.go\n" +
		"rename to test/core_test.go\n" +
		"--- a/internal/app/core.go\n" +
		"+++ b/test/core_test.go\n" +
		"@@ -0,0 +1,700 @@\n")
	for i := 0; i < 700; i++ {
		b.WriteString("+x\n")
	}
	return sizedSignals(b.String())
}

// TestDecide_SizeIgnoresPullRequestMarkers pins that the exclusion is by
// the deployment's patterns alone: a file the pull request itself marks
// as generated -- a .gitattributes linguist-generated entry, a "Code
// generated ... DO NOT EDIT." header -- is still counted, since the
// author controls both.
func TestDecide_SizeIgnoresPullRequestMarkers(t *testing.T) {
	gitattributes := addedFileDiff(".gitattributes", 2, func(i int) string {
		return []string{"internal/app/big.go linguist-generated=true", "internal/app/big.go -diff"}[i]
	})
	big := addedFileDiff("internal/app/big.go", 700, func(i int) string {
		if i == 0 {
			return "// Code generated by hand-waving. DO NOT EDIT."
		}
		return plainLine(i)
	})
	got := reviewtriage.Decide(sizedSignals(gitattributes+big), deploymentConfig())
	if got.Depth != reviewtriage.DepthDeep || got.Reason != reviewtriage.ReasonChangedLinesOver {
		t.Fatalf("Decide() = (%q, %q), want (deep, %q): a pull request's own markers must never shrink its size", got.Depth, got.Reason, reviewtriage.ReasonChangedLinesOver)
	}
	if got.SourceLines != 702 {
		t.Errorf("SourceLines = %d, want 702 (every line of both files)", got.SourceLines)
	}
}

// TestNewDecisionRecord_RecordsTheCause pins that the record names how the
// input was read in every case -- under an always_light override too --
// and the size the decision was made on.
func TestNewDecisionRecord_RecordsTheCause(t *testing.T) {
	tests := []struct {
		name          string
		sig           reviewtriage.Signals
		cfg           reviewtriage.Config
		wantDepth     string
		wantReason    reviewtriage.Reason
		wantInputRead string
	}{
		{
			name:          "an unreadable diff under always_light routes light and records the cause",
			sig:           reviewtriage.Signals{Additions: 3, InputRead: review.InputReadDiffUnreadable},
			cfg:           reviewtriage.Config{Mode: reviewtriage.ModeAlwaysLight},
			wantDepth:     "light",
			wantReason:    reviewtriage.ReasonAlwaysLightConfig,
			wantInputRead: "diff_unreadable",
		},
		{
			name:          "an unreadable pull request routes deep and records the cause",
			sig:           reviewtriage.Signals{InputRead: review.InputReadPRUnreadable},
			cfg:           deploymentConfig(),
			wantDepth:     "deep",
			wantReason:    reviewtriage.ReasonInputUnreadable,
			wantInputRead: "pr_unreadable",
		},
		{
			name:          "a genuinely empty change records empty",
			sig:           reviewtriage.Signals{InputRead: review.InputReadEmpty},
			cfg:           deploymentConfig(),
			wantDepth:     "light",
			wantReason:    reviewtriage.ReasonLightDefault,
			wantInputRead: "empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := reviewtriage.Decide(tt.sig, tt.cfg)
			record := reviewtriage.NewDecisionRecord(decision, tt.cfg, decision.Depth, reviewtriage.Provenance{}, nil, nil, 0, true, false, nil, nil)
			if record.Depth != tt.wantDepth || record.Reason != string(tt.wantReason) {
				t.Errorf("record depth/reason = %q/%q, want %q/%q", record.Depth, record.Reason, tt.wantDepth, tt.wantReason)
			}
			if record.InputRead != tt.wantInputRead {
				t.Errorf("record.InputRead = %q, want %q", record.InputRead, tt.wantInputRead)
			}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(raw), `"inputRead":"`+tt.wantInputRead+`"`) {
				t.Errorf("persisted record %s does not name the cause %q", raw, tt.wantInputRead)
			}
		})
	}

	t.Run("the size the decision was made on", func(t *testing.T) {
		sig := sizedSignals(addedFileDiff("internal/a.go", 40, plainLine) + addedFileDiff("internal/a_test.go", 600, plainLine))
		decision := reviewtriage.Decide(sig, deploymentConfig())
		record := reviewtriage.NewDecisionRecord(decision, deploymentConfig(), decision.Depth, reviewtriage.Provenance{}, nil, nil, 2, false, false, nil, nil)
		if record.ChangedLines != 640 || record.SourceLines == nil || *record.SourceLines != 40 {
			t.Errorf("record ChangedLines/SourceLines = %d/%v, want 640/40", record.ChangedLines, record.SourceLines)
		}
	})
}

// previousDecisionRecord is the decision record's shape as the binary
// before this change wrote and read it -- every field, same tags, and
// neither inputRead nor sourceLines.
type previousDecisionRecord struct {
	Depth                string   `json:"depth"`
	Reason               string   `json:"reason"`
	MatchedSensitiveTags []string `json:"matchedSensitiveTags,omitempty"`
	ChangedLines         int      `json:"changedLines"`
	DistinctRoots        int      `json:"distinctRoots"`
	Mode                 string   `json:"mode"`
	Floored              bool     `json:"floored"`
	NarviAuthored        bool     `json:"narviAuthored"`
	AuthoringModel       string   `json:"authoringModel,omitempty"`
	ResolvedModelID      string   `json:"resolvedModelId,omitempty"`
	ResolvedEffort       string   `json:"resolvedEffort,omitempty"`
	ChangedFilesCount    int      `json:"changedFilesCount,omitempty"`
	DiffEmpty            bool     `json:"diffEmpty,omitempty"`
	DiffTruncated        bool     `json:"diffTruncated,omitempty"`
	ArchDecisionTags     []string `json:"archDecisionTags,omitempty"`
	ArchDecisionRoots    []string `json:"archDecisionRoots,omitempty"`
}

// TestDecisionRecord_StoredShapeCompatibility pins the two directions a
// mixed-version deployment reads turns.review_depth_decision in.
func TestDecisionRecord_StoredShapeCompatibility(t *testing.T) {
	t.Run("a record the previous binary wrote decodes, the cause unknown", func(t *testing.T) {
		old := `{"depth":"deep","reason":"changed lines exceed threshold","changedLines":900,"distinctRoots":2,"mode":"auto","floored":false,"narviAuthored":false,"changedFilesCount":4,"diffEmpty":false}`
		var got reviewtriage.DecisionRecord
		if err := json.Unmarshal([]byte(old), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.InputRead != "" || got.SourceLines != nil {
			t.Errorf("InputRead/SourceLines = %q/%v, want both absent", got.InputRead, got.SourceLines)
		}
		if got.Depth != "deep" || got.ChangedLines != 900 || got.ChangedFilesCount != 4 {
			t.Errorf("decoded = %+v, want the stored fields intact", got)
		}
	})

	t.Run("a record this binary writes decodes in the previous shape, the new reason included", func(t *testing.T) {
		decision := reviewtriage.Decide(reviewtriage.Signals{InputRead: review.InputReadDiffTruncated, ChangedPaths: []string{"internal/a.go"}}, deploymentConfig())
		record := reviewtriage.NewDecisionRecord(decision, deploymentConfig(), decision.Depth, reviewtriage.Provenance{}, nil, nil, 3, false, true, []string{"x"}, []string{"internal"})
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var prev previousDecisionRecord
		if err := json.Unmarshal(raw, &prev); err != nil {
			t.Fatalf("the previous shape cannot read %s: %v", raw, err)
		}
		want := previousDecisionRecord{
			Depth:             "deep",
			Reason:            string(reviewtriage.ReasonInputUnreadable),
			DistinctRoots:     1,
			Mode:              "auto",
			ChangedFilesCount: 3,
			DiffTruncated:     true,
			ArchDecisionTags:  []string{"x"},
			ArchDecisionRoots: []string{"internal"},
		}
		if !reflect.DeepEqual(prev, want) {
			t.Errorf("previous shape = %+v, want %+v", prev, want)
		}
	})
}

// TestNonFloorReasons pins which depths are never a later review's floor:
// the unreadable-input reason, and only it.
func TestNonFloorReasons(t *testing.T) {
	got := reviewtriage.NonFloorReasons()
	want := []reviewtriage.Reason{reviewtriage.ReasonInputUnreadable}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NonFloorReasons() = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if reviewtriage.NonFloorReasons()[0] != reviewtriage.ReasonInputUnreadable {
		t.Error("NonFloorReasons() returned a shared slice a caller can mutate")
	}
}
