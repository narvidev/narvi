// Command contractscompat implements technical plan §6.3's "CI that FAILS
// a breaking change rather than reporting it": given a BASE and a HEAD
// copy of the /contracts directory (manifest.json, VERSION, CHANGELOG.md,
// and every versioned schema file) plus the base/head copies of
// controlplane/testdata/routes.golden, it classifies every change as
// PATCH, MINOR, or MAJOR under the closed rule table implemented by
// ./compat, and exits non-zero on any MAJOR or fail-closed finding.
//
// This command deliberately does none of its own git work -- the
// Makefile's contracts-compat target extracts the merge-base copy with
// `git archive` into a temp directory and passes both directories in by
// path. tools/lint/narvichecks/execimportban forbids "os/exec" anywhere
// under tools/ (outside the sandbox/outbound trees this platform's own
// egress guarantee already carves out), so this binary only ever opens
// files by path -- never shells out to git or anything else.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/narvidev/narvi/tools/contractscompat/compat"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	flagSet := flag.NewFlagSet("contractscompat", flag.ContinueOnError)
	flagSet.SetOutput(stderr)
	base := flagSet.String("base", "", "path to the BASE /contracts directory")
	head := flagSet.String("head", "", "path to the HEAD /contracts directory")
	routesBase := flagSet.String("routes-base", "", "path to the BASE controlplane/testdata/routes.golden")
	routesHead := flagSet.String("routes-head", "", "path to the HEAD controlplane/testdata/routes.golden")
	if err := flagSet.Parse(args); err != nil {
		return 2
	}
	if *base == "" || *head == "" || *routesBase == "" || *routesHead == "" {
		_, _ = fmt.Fprintln(stderr, "usage: contractscompat --base DIR --head DIR --routes-base FILE --routes-head FILE")
		return 2
	}

	in, err := loadInput(*base, *head, *routesBase, *routesHead)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "contractscompat:", err)
		return 1
	}

	report, err := compat.Compare(in)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "contractscompat:", err)
		return 1
	}

	findings := append([]compat.Finding{}, report.Findings...)
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Surface != b.Surface {
			return a.Surface < b.Surface
		}
		if a.Pointer != b.Pointer {
			return a.Pointer < b.Pointer
		}
		return a.RuleID < b.RuleID
	})

	if len(findings) == 0 {
		_, _ = fmt.Fprintln(stdout, "contractscompat: no findings")
		return 0
	}

	for _, f := range findings {
		_, _ = fmt.Fprintf(stdout, "[%s] rule %s %s %s: %s\n", f.Severity, f.RuleID, f.Surface, f.Pointer, f.Message)
	}

	if report.HasBreaking() {
		_, _ = fmt.Fprintln(stdout, "contractscompat: BREAKING changes found (MAJOR or fail-closed) -- see COMPATIBILITY.md")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "contractscompat: no breaking changes (MINOR/PATCH only)")
	return 0
}

// loadInput does every bit of this command's own file I/O, then hands a
// fully in-memory compat.Input to the pure library.
func loadInput(baseDir, headDir, routesBasePath, routesHeadPath string) (compat.Input, error) {
	headManifest, err := os.ReadFile(filepath.Join(headDir, "manifest.json"))
	if err != nil {
		return compat.Input{}, fmt.Errorf("read head manifest.json: %w", err)
	}
	headVersion, err := os.ReadFile(filepath.Join(headDir, "VERSION"))
	if err != nil {
		return compat.Input{}, fmt.Errorf("read head VERSION: %w", err)
	}
	headChangelog, err := os.ReadFile(filepath.Join(headDir, "CHANGELOG.md"))
	if err != nil {
		return compat.Input{}, fmt.Errorf("read head CHANGELOG.md: %w", err)
	}

	// Genesis case: the PR that FIRST adds manifest.json/VERSION/
	// CHANGELOG.md (this Step's own PR) has no base copy of any of the
	// three to compare against at all -- BASE predates contracts
	// governance existing. Rather than hard-erroring on every PR until
	// this one lands on main, treat a missing base manifest.json as "base
	// governance was identical to head's" for the SURFACE SET (so
	// DiffSurfaceSet finds nothing to complain about for files that
	// aren't actually new) and synthesize "0.0.0"/empty for VERSION/
	// CHANGELOG so the strictly-greater-version and CHANGELOG-heading
	// checks still run meaningfully against a real content diff.
	//
	// E8 (round 3): this must NOT extend to openEnums or a `retired`
	// status -- those are relaxations, and the reasoning "safe because
	// there IS no prior state to escape scrutiny of" is wrong: the base
	// SCHEMA FILES genuinely exist and their enums were genuinely
	// closed; only the GOVERNANCE files (manifest.json/VERSION/
	// CHANGELOG.md) are new. Substituting HEAD's own manifest as BASE
	// would let a genesis-mode PR open a closed enum and add a value to
	// it in the very same diff -- exactly what the merge-base-only rule
	// (C1, compare.go) exists to prevent. compat.Compare's own Genesis
	// flag (set below) strips both relaxations from the parsed copy
	// regardless of what this substituted content says.
	baseManifest, err := os.ReadFile(filepath.Join(baseDir, "manifest.json"))
	genesis := false
	if err != nil {
		if !os.IsNotExist(err) {
			return compat.Input{}, fmt.Errorf("read base manifest.json: %w", err)
		}
		baseManifest = headManifest
		genesis = true
	}
	baseVersion, err := os.ReadFile(filepath.Join(baseDir, "VERSION"))
	if err != nil {
		if !os.IsNotExist(err) {
			return compat.Input{}, fmt.Errorf("read base VERSION: %w", err)
		}
		baseVersion = []byte("0.0.0")
	}
	baseChangelog, err := os.ReadFile(filepath.Join(baseDir, "CHANGELOG.md"))
	if err != nil {
		if !os.IsNotExist(err) {
			return compat.Input{}, fmt.Errorf("read base CHANGELOG.md: %w", err)
		}
		baseChangelog = nil
	}
	baseRoutes, err := os.ReadFile(routesBasePath)
	if err != nil {
		return compat.Input{}, fmt.Errorf("read base routes.golden: %w", err)
	}
	headRoutes, err := os.ReadFile(routesHeadPath)
	if err != nil {
		return compat.Input{}, fmt.Errorf("read head routes.golden: %w", err)
	}

	baseFiles, err := loadSchemaFiles(baseDir)
	if err != nil {
		return compat.Input{}, fmt.Errorf("read base schema files: %w", err)
	}
	headFiles, err := loadSchemaFiles(headDir)
	if err != nil {
		return compat.Input{}, fmt.Errorf("read head schema files: %w", err)
	}

	return compat.Input{
		BaseManifestRaw:  baseManifest,
		HeadManifestRaw:  headManifest,
		BaseVersion:      strings.TrimSpace(string(baseVersion)),
		HeadVersion:      strings.TrimSpace(string(headVersion)),
		BaseChangelogRaw: baseChangelog,
		HeadChangelogRaw: headChangelog,
		BaseRoutes:       baseRoutes,
		HeadRoutes:       headRoutes,
		BaseSchemaFiles:  baseFiles,
		HeadSchemaFiles:  headFiles,
		Genesis:          genesis,
	}, nil
}

// loadSchemaFiles walks dir for every *.schema.json file under a
// "<surface>/v<N>/" layout and returns them keyed by their path relative
// to dir, e.g. "rest/v1/dtos.schema.json" -- exactly the form
// manifest.json's own "path" field uses.
func loadSchemaFiles(dir string) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".schema.json") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
