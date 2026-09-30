package reviewtriage

import (
	"strings"

	"github.com/narvidev/narvi/internal/domain/codeowners"
)

// DefaultSizeExclusions is the size rule's built-in pattern set (§26.3:
// "the size that routes counts source changes only"): the files whose lines
// are left out of the count a review is sized by. A deployment replaces it
// through platform.Config.ReviewSizeExcludedPaths
// (NARVI_REVIEW_SIZE_EXCLUDED_PATHS); this list is what an unset variable
// means.
//
// Patterns use the gitignore dialect internal/domain/codeowners already
// implements, so the repository has one glob dialect, not two: a pattern
// with no "/" matches a file name at any depth.
//
// The default is deliberately narrow. A file it excludes must be one that
// cannot be hand-written production code, judged by its NAME alone -- never
// by a directory name (a package named doc, docs or test is ordinary
// production code in many repositories), and never by anything the pull
// request carries about its own files (.gitattributes, a "generated"
// header), which its author controls. Each entry, and why it cannot match
// hand-written production code:
//
//   - "*_test.go": the Go toolchain compiles a _test.go file only into a
//     test binary; it is never part of a production build.
//   - "*.test.ts", "*.test.tsx": the test runner's own discovery suffix for
//     TypeScript, the convention this repository's web code uses; a module
//     named so is collected as a test file.
//   - "*.md", "*.rst", "*.adoc": Markdown, reStructuredText and AsciiDoc
//     are documentation formats, not program source.
//   - "*.pb.go", "*_pb2.py", "*_pb2_grpc.py": the output names the protocol
//     buffer compiler gives the code it generates; the .proto source that
//     defines it stays counted.
//   - "zz_generated.*", "*_generated.go", "*.gen.go": file names that say
//     the file is generated, by the conventions of the Kubernetes code
//     generators and of Go generators generally.
//
// Left out on purpose, so they COUNT: test directories and other test-name
// conventions (another language's tests are sized with its code until an
// operator adds them), lockfiles (the only place a dependency's resolved
// source and hash are recorded, and no sensitive-path rule covers
// dependencies, so a large lockfile rewrite must still be able to route
// deep by size), minified bundles and snapshots (shipped or asserted
// content, not documentation), and MDX (compiled into pages). Residuals: a
// Markdown file a product embeds and ships (a prompt template, say) is
// excluded; a file named like a test or a generated file that is really
// hand-written production code is excluded; the path rules still read both.
//
// Returned fresh on every call, so no caller can mutate the shared
// default.
func DefaultSizeExclusions() []string {
	return []string{
		// Tests, by the language conventions this repository's own code
		// uses.
		"*_test.go",
		"*.test.ts",
		"*.test.tsx",
		// Documentation, by extension.
		"*.md",
		"*.rst",
		"*.adoc",
		// Generated, by names that say so.
		"*.pb.go",
		"*_pb2.py",
		"*_pb2_grpc.py",
		"zz_generated.*",
		"*_generated.go",
		"*.gen.go",
	}
}

// UnsupportedSizeExclusionConstruct names the construct in pattern that the
// codeowners dialect does not implement, so a pattern using it would be read
// as literal text and silently never match what its author meant: a leading
// "!" (negation), "[" or "]" (a character class), "{" or "}" (a brace list;
// a comma-separated setting also splits it), a backslash (an escape), or a
// leading "#" (a comment). ok is false when pattern uses none of them.
// platform.Config refuses such an entry at boot.
func UnsupportedSizeExclusionConstruct(pattern string) (construct string, ok bool) {
	switch {
	case strings.HasPrefix(pattern, "!"):
		return `negation (a leading "!")`, true
	case strings.HasPrefix(pattern, "#"):
		return `comment (a leading "#")`, true
	case strings.ContainsAny(pattern, "[]"):
		return `character class ("[...]")`, true
	case strings.ContainsAny(pattern, "{}"):
		return `brace list ("{a,b}")`, true
	case strings.Contains(pattern, `\`):
		return `escape (a backslash)`, true
	default:
		return "", false
	}
}

// sizeExclusion matches changed files against a deployment's size
// exclusion patterns. The zero value (no patterns) excludes nothing.
type sizeExclusion struct {
	matcher *codeowners.Matcher
}

func newSizeExclusion(patterns []string) sizeExclusion {
	if len(patterns) == 0 {
		return sizeExclusion{}
	}
	rules := make([]codeowners.Rule, len(patterns))
	for i, p := range patterns {
		rules[i] = codeowners.Rule{Pattern: p}
	}
	return sizeExclusion{matcher: codeowners.Compile(rules)}
}

func (e sizeExclusion) matches(p string) bool {
	if e.matcher == nil || p == "" {
		return false
	}
	_, ok := e.matcher.Match(p)
	return ok
}

// excludes reports whether f's lines are left out of the size: every path
// the file section names must match, so a rename that moves a source file
// into a test directory still counts (its old path is source).
func (e sizeExclusion) excludes(f FileLines) bool {
	if !e.matches(f.Path) {
		return false
	}
	if f.OldPath != "" && !e.matches(f.OldPath) {
		return false
	}
	return true
}

// diffSize is what one diff shows of a change's size: every line it adds
// or deletes (total), and those not in a file the deployment's patterns
// exclude (source). Both come from the single diff read pinned to the head
// under review, never mixed with the pull request's own reported line
// counts, which GitHub computes separately and which could describe another
// head: for a complete diff, source is the change's exact source size; for
// a partial one, a lower bound.
func diffSize(files []FileLines, patterns []string) (total, source int) {
	exclusion := newSizeExclusion(patterns)
	for _, f := range files {
		lines := f.Added + f.Deleted
		total += lines
		if !exclusion.excludes(f) {
			source += lines
		}
	}
	return total, source
}
