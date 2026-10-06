package ops

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestScanAutonomyFreezeSites_RepoTree is the backstop behind technical
// plan §40.2's call-site checks, run against this repository's real tree:
// every registered automatic-action site still reads the freeze where it
// is registered, every site the domain declares is registered, and no
// person's command consults the freeze.
func TestScanAutonomyFreezeSites_RepoTree(t *testing.T) {
	violations, err := ScanAutonomyFreezeSites(repoRoot(t))
	if err != nil {
		t.Fatalf("ScanAutonomyFreezeSites: %v", err)
	}
	if len(violations) > 0 {
		lines := make([]string, len(violations))
		for i, v := range violations {
			lines[i] = v.String()
		}
		t.Errorf("%d autonomy freeze violation(s) -- every automatic action reads the freeze, and no person's command does (technical plan §40.2):\n\t%s",
			len(violations), strings.Join(lines, "\n\t"))
	}
}

// TestAutonomyFreezeTables_EachEntrySaysWhy keeps both tables honest: every
// entry names a file that exists and says why, and the human paths name
// each file once.
func TestAutonomyFreezeTables_EachEntrySaysWhy(t *testing.T) {
	root := repoRoot(t)
	for _, c := range AutonomyFreezeChecks {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(c.File))); err != nil {
			t.Errorf("%s's read: %v", c.Site, err)
		}
		if c.Reads < 1 {
			t.Errorf("%s|%s registers %d reads, want at least one", c.File, c.Function, c.Reads)
		}
		if len(strings.Fields(c.Reason)) < 8 {
			t.Errorf("%s|%s: reason %q does not say why", c.File, c.Function, c.Reason)
		}
	}
	seen := map[string]bool{}
	for _, h := range AutonomyFreezeHumanPaths {
		if seen[h.File] {
			t.Errorf("%s is listed twice as a person's path", h.File)
		}
		seen[h.File] = true
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(h.File))); err != nil {
			t.Errorf("a person's path: %v", err)
		}
		if len(strings.Fields(h.Reason)) < 5 {
			t.Errorf("%s: reason %q does not say why", h.File, h.Reason)
		}
	}
}

// The synthetic tree TestScanAutonomyFreezeSites_Rules checks: a domain
// package declaring two sites and reserving one, a site reading the freeze
// twice, a site reading it through a table, and a person's path.
const (
	fixtureDomain = `package autonomy

type Site string

const (
	SiteMerge Site = "merge"
	SiteSpawn Site = "spawn"
)

const SiteLater Site = "later"

var Reserved = []Site{SiteLater}
`
	fixtureWorker = `package worker

import (
	"context"

	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
)

type gate interface{ Check(context.Context, domainautonomy.Site) (bool, string) }

type deps struct{ Autonomy gate }

type W struct{ deps deps }

func (w *W) merge(ctx context.Context) {
	if skip, _ := w.deps.Autonomy.Check(ctx, domainautonomy.SiteMerge); skip {
		return
	}
	if skip, _ := w.deps.Autonomy.Check(ctx, domainautonomy.SiteMerge); skip {
		return
	}
}
`
	fixtureOutboxTable = `package outbox

import domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"

var table = map[string]domainautonomy.Site{"spawn": domainautonomy.SiteSpawn}
`
	fixtureOutbox = `package outbox

import "context"

type B struct{ gate interface{ Check(context.Context, string) (bool, string) } }

func (b *B) attempt(ctx context.Context, kind string) {
	if skip, _ := b.gate.Check(ctx, kind); skip {
		return
	}
}
`
	fixtureHuman = `package sessionactor

import "context"

type A struct{}

func (a *A) consumeOwed(ctx context.Context) {}
`
)

// TestScanAutonomyFreezeSites_Rules proves each rule fires on the shape it
// exists for, and stays quiet on the fixture tree as written, over a
// synthetic tree per case.
func TestScanAutonomyFreezeSites_Rules(t *testing.T) {
	const (
		domainFile = "internal/domain/autonomy/site.go"
		workerFile = "internal/app/worker/worker.go"
		tableFile  = "internal/app/outbox/freeze.go"
		outboxFile = "internal/app/outbox/builder.go"
		humanFile  = "internal/app/sessionactor/owedreviewrequest.go"
	)
	base := map[string]string{
		domainFile: fixtureDomain,
		workerFile: fixtureWorker,
		tableFile:  fixtureOutboxTable,
		outboxFile: fixtureOutbox,
		humanFile:  fixtureHuman,
	}
	checks := []AutonomyFreezeCheck{
		{Site: "SiteMerge", File: workerFile, Function: "merge", Reads: 2, Reason: "fixture"},
		{Site: "SiteSpawn", File: outboxFile, Function: "attempt", Reads: 1, NamedInFile: tableFile, NamedIn: "table", Reason: "fixture"},
	}
	humans := []AutonomyFreezeHumanPath{{File: humanFile, Reason: "fixture"}}

	for _, tc := range []struct {
		name   string
		edit   func(files map[string]string, checks []AutonomyFreezeCheck) []AutonomyFreezeCheck
		rules  []string // the rules expected to fire, in order
		detail string   // a word every violation's detail must carry
	}{
		{name: "the fixture tree as written", edit: func(_ map[string]string, c []AutonomyFreezeCheck) []AutonomyFreezeCheck { return c }},
		{
			name: "a site whose first check is removed",
			edit: func(files map[string]string, c []AutonomyFreezeCheck) []AutonomyFreezeCheck {
				files[workerFile] = strings.Replace(files[workerFile], "if skip, _ := w.deps.Autonomy.Check(ctx, domainautonomy.SiteMerge); skip {\n\t\treturn\n\t}\n", "", 1)
				return c
			},
			rules: []string{"a"}, detail: "1 time(s)",
		},
		{
			name: "a site whose every check is removed",
			edit: func(files map[string]string, c []AutonomyFreezeCheck) []AutonomyFreezeCheck {
				files[outboxFile] = strings.Replace(files[outboxFile], "b.gate.Check(ctx, kind)", "false, \"\"", 1)
				return c
			},
			rules: []string{"a"}, detail: "0 time(s)",
		},
		{
			name: "a site's function renamed away",
			edit: func(files map[string]string, c []AutonomyFreezeCheck) []AutonomyFreezeCheck {
				files[workerFile] = strings.Replace(files[workerFile], "func (w *W) merge(", "func (w *W) mergeLater(", 1)
				return c
			},
			rules: []string{"a"}, detail: "no longer declares",
		},
		{
			name: "a table that no longer names its site",
			edit: func(files map[string]string, c []AutonomyFreezeCheck) []AutonomyFreezeCheck {
				files[tableFile] = strings.Replace(files[tableFile], `{"spawn": domainautonomy.SiteSpawn}`, `{}`, 1)
				return c
			},
			rules: []string{"a"}, detail: "SiteSpawn",
		},
		{
			name: "a check on a receiver not held as a gate does not count",
			edit: func(files map[string]string, c []AutonomyFreezeCheck) []AutonomyFreezeCheck {
				files[outboxFile] = strings.Replace(files[outboxFile], "b.gate.Check(ctx, kind)", "b.other.Check(ctx, kind)", 1)
				files[outboxFile] = strings.Replace(files[outboxFile], "type B struct{ gate interface", "type B struct{ other interface", 1)
				return c
			},
			rules: []string{"a"}, detail: "0 time(s)",
		},
		{
			name: "an unregistered site constant",
			edit: func(files map[string]string, c []AutonomyFreezeCheck) []AutonomyFreezeCheck {
				files[domainFile] = strings.Replace(files[domainFile], "SiteSpawn Site = \"spawn\"\n", "SiteSpawn Site = \"spawn\"\n\tSiteNew   Site = \"new\"\n", 1)
				return c
			},
			rules: []string{"b"}, detail: "SiteNew",
		},
		{
			name: "a registration of a reserved site",
			edit: func(_ map[string]string, c []AutonomyFreezeCheck) []AutonomyFreezeCheck {
				return append(slices.Clone(c), AutonomyFreezeCheck{Site: "SiteLater", File: workerFile, Function: "merge", Reads: 2, Reason: "fixture"})
			},
			rules: []string{"a", "b"}, detail: "SiteLater",
		},
		{
			name: "a stale registration",
			edit: func(_ map[string]string, c []AutonomyFreezeCheck) []AutonomyFreezeCheck {
				return append(slices.Clone(c), AutonomyFreezeCheck{Site: "SiteGone", File: workerFile, Function: "merge", Reads: 2, Reason: "fixture"})
			},
			rules: []string{"a", "b"}, detail: "SiteGone",
		},
		{
			name: "a check added in the owed request's consumer",
			edit: func(files map[string]string, c []AutonomyFreezeCheck) []AutonomyFreezeCheck {
				files[humanFile] = strings.Replace(files[humanFile],
					"type A struct{}\n\nfunc (a *A) consumeOwed(ctx context.Context) {}",
					"type A struct{ autonomy interface{ Check(context.Context, string) (bool, string) } }\n\nfunc (a *A) consumeOwed(ctx context.Context) {\n\tif skip, _ := a.autonomy.Check(ctx, \"x\"); skip {\n\t\treturn\n\t}\n}", 1)
				return c
			},
			rules: []string{"c"}, detail: "Check",
		},
		{
			name: "the gate's package imported by a person's path",
			edit: func(files map[string]string, c []AutonomyFreezeCheck) []AutonomyFreezeCheck {
				files[humanFile] = strings.Replace(files[humanFile], "import \"context\"", "import (\n\t\"context\"\n\n\t_ \"github.com/narvidev/narvi/internal/app/autonomy\"\n)", 1)
				return c
			},
			rules: []string{"c"}, detail: "imports",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			for k, v := range base {
				files[k] = v
			}
			c := tc.edit(files, slices.Clone(checks))
			root := t.TempDir()
			for rel, src := range files {
				path := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			violations, err := scanAutonomyFreezeSites(root, c, humans)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			var rules []string
			for _, v := range violations {
				rules = append(rules, v.Rule)
				if tc.detail != "" && !strings.Contains(v.Detail, tc.detail) {
					t.Errorf("violation %s does not name %q", v, tc.detail)
				}
			}
			slices.Sort(rules)
			if !slices.Equal(rules, tc.rules) {
				t.Fatalf("rules fired = %v, want %v:\n%v", rules, tc.rules, violations)
			}
		})
	}
}
