package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// orderedBoot is an ordered fake of every step completeBoot drives -- the
// bridge's two boot signals, the boot, the re-own pass and the boot_timing
// relay -- recording each call, in order, on one log. Its clock moves only
// inside the boot (90s) and the re-own pass (30s).
type orderedBoot struct {
	bootErr, reownErr error

	calls   []string
	clock   time.Time
	timings []sandboxws.BootTiming
}

func (o *orderedBoot) ReportBootStarted() { o.calls = append(o.calls, "report boot started") }
func (o *orderedBoot) MarkBootComplete()  { o.calls = append(o.calls, "mark boot complete") }

// steps is run()'s bootSteps with every call faked. liveSession gives it
// the bridge and the re-own pass, as run() does exactly when
// cfg.SessionConfig is set.
func (o *orderedBoot) steps(liveSession bool) bootSteps {
	steps := bootSteps{
		runBoot: func() error {
			o.calls = append(o.calls, "run boot")
			o.clock = o.clock.Add(90 * time.Second)
			return o.bootErr
		},
		sendBootTiming: func(evt sandboxws.BootTiming) {
			o.calls = append(o.calls, "send "+string(evt.Metric))
			o.timings = append(o.timings, evt)
		},
		bootMode: "repo_image",
		now:      func() time.Time { return o.clock },
	}
	if liveSession {
		steps.signals = o
		steps.reown = func() error {
			o.calls = append(o.calls, "reown")
			o.clock = o.clock.Add(30 * time.Second)
			return o.reownErr
		}
	}
	return steps
}

// TestCompleteBoot_BootDurationOnlyAfterTheReownPass pins the order run()
// boots in -- completeBoot, handed run()'s real calls -- which technical
// plan §3.2 needs: the start signal first; a boot_duration with
// failed=false is boot evidence the control plane acts on, so it goes out
// only once the workspace re-own pass -- part of the boot, whose failure
// fails it -- has run, and its failed tag covers that pass; the completion
// signal last, and only after a boot that succeeded whole. The seconds
// still bracket the boot alone (SLO 1): 90, not the 120 that would count
// the re-own pass too.
func TestCompleteBoot_BootDurationOnlyAfterTheReownPass(t *testing.T) {
	t.Parallel()

	errBoot := errors.New("setup.sh failed")
	errReown := errors.New("chown: permission denied")

	for _, tc := range []struct {
		name              string
		bootErr, reownErr error
		liveSession       bool
		wantCalls         []string
		wantFailed        bool
		wantErr           error
	}{
		{
			name:        "boot and re-own succeed",
			liveSession: true,
			wantCalls:   []string{"report boot started", "run boot", "reown", "send boot_duration", "mark boot complete"},
		},
		{
			name:        "re-own fails: the boot fails, says so, and is never marked complete",
			liveSession: true,
			reownErr:    errReown,
			wantCalls:   []string{"report boot started", "run boot", "reown", "send boot_duration"},
			wantFailed:  true,
			wantErr:     errReown,
		},
		{
			name:        "boot failed: no re-own, reported failed, never marked complete",
			liveSession: true,
			bootErr:     errBoot,
			wantCalls:   []string{"report boot started", "run boot", "send boot_duration"},
			wantFailed:  true,
			wantErr:     errBoot,
		},
		{
			name:      "no live session: no bridge, nothing to re-own",
			wantCalls: []string{"run boot", "send boot_duration"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &orderedBoot{bootErr: tc.bootErr, reownErr: tc.reownErr}
			err := completeBoot(fake.steps(tc.liveSession))

			if !slices.Equal(fake.calls, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", fake.calls, tc.wantCalls)
			}
			if len(fake.timings) != 1 {
				t.Fatalf("boot_timing events = %d, want exactly 1 boot_duration", len(fake.timings))
			}
			evt := fake.timings[0]
			if evt.Metric != sandboxws.BootTimingMetricBootDuration {
				t.Errorf("metric = %q, want %q", evt.Metric, sandboxws.BootTimingMetricBootDuration)
			}
			if evt.Failed == nil || *evt.Failed != tc.wantFailed {
				t.Errorf("failed = %v, want %v", evt.Failed, tc.wantFailed)
			}
			if evt.Seconds != 90 {
				t.Errorf("seconds = %v, want 90: the boot alone, not the re-own pass", evt.Seconds)
			}
			if evt.BootMode == nil || *evt.BootMode != "repo_image" {
				t.Errorf("bootMode = %v, want repo_image", evt.BootMode)
			}
			if tc.wantErr == nil && err != nil {
				t.Errorf("error = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want one wrapping %v", err, tc.wantErr)
			}
		})
	}
}

// TestBootDuration_BuiltOnlyByCompleteBoot is run()'s half of the order
// TestCompleteBoot_BootDurationOnlyAfterTheReownPass pins: the package
// builds a boot_duration event in one place, completeBoot, so run() can
// send one only in completeBoot's order. A boot_duration built anywhere
// else -- in run(), right after the boot, as agents built before
// 2026-09-28 did -- fails here, since no test can run run() itself.
func TestBootDuration_BuiltOnlyByCompleteBoot(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var sites []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			owner := "package-level declaration"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				owner = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "sandboxws" && n.Sel.Name == "BootTimingMetricBootDuration" {
						sites = append(sites, owner+" ("+fset.Position(n.Pos()).String()+")")
					}
				case *ast.BasicLit:
					if n.Kind == token.STRING {
						if v, err := strconv.Unquote(n.Value); err == nil && v == string(sandboxws.BootTimingMetricBootDuration) {
							sites = append(sites, owner+" ("+fset.Position(n.Pos()).String()+")")
						}
					}
				}
				return true
			})
		}
	}
	if len(sites) != 1 || !strings.HasPrefix(sites[0], "completeBoot ") {
		t.Errorf("boot_duration built at %v, want exactly once, in completeBoot", sites)
	}
}
