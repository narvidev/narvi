package reviewtriage

import "github.com/narvidev/narvi/internal/domain/codeowners"

// DefaultSizeExclusions is the size rule's built-in pattern set (§26.3:
// "the size that routes counts source changes only"): test, documentation
// and generated files whose lines are left out of the count a review is
// sized by. A deployment replaces it through platform.Config.
// ReviewSizeExcludedPaths (NARVI_REVIEW_SIZE_EXCLUDED_PATHS); this list
// is what an unset variable means.
//
// Patterns use the gitignore dialect internal/domain/codeowners already
// implements, so the repository has one glob dialect, not two: a pattern
// with no "/" matches at any depth, "*" stays within one path segment,
// and "**" crosses them ("**/docs/**" is everything under a directory
// named docs, at any depth). Each entry is chosen so a file it matches is
// one a review does not have to size by: a file's own name or directory
// marks it as a test, a document, or the output of a tool. Every match is
// by path alone,
// never by anything the pull request carries about its own files
// (.gitattributes, a "generated" header), which the author controls.
//
// Returned fresh on every call, so no caller can mutate the shared
// default.
func DefaultSizeExclusions() []string {
	return []string{
		// Tests.
		"*_test.go",
		"test_*.py",
		"*_test.py",
		"*_spec.rb",
		"*.test.*",
		"*.spec.*",
		"*Test.java",
		"*Tests.java",
		"*Test.kt",
		"**/test/**",
		"**/tests/**",
		"**/testdata/**",
		"**/__tests__/**",
		"**/__snapshots__/**",
		"*.snap",
		// Documentation.
		"*.md",
		"*.mdx",
		"*.rst",
		"*.adoc",
		"**/docs/**",
		"**/doc/**",
		// Generated.
		"*.pb.go",
		"*_pb2.py",
		"*_pb2_grpc.py",
		"*.gen.go",
		"*_generated.go",
		"zz_generated.*",
		"*.min.js",
		"*.min.css",
		"go.sum",
		"package-lock.json",
		"yarn.lock",
		"pnpm-lock.yaml",
		"Cargo.lock",
		"Gemfile.lock",
		"poetry.lock",
		"Pipfile.lock",
		"composer.lock",
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

// sourceChangedLines is the size Decide routes on: the pull request's own
// reported changed lines, less the lines the diff attributes to excluded
// files. Subtracting only what is positively attributed to an excluded
// file means a line the diff parse missed stays counted. Never negative.
func sourceChangedLines(changedLines int, files []FileLines, patterns []string) int {
	exclusion := newSizeExclusion(patterns)
	excluded := 0
	for _, f := range files {
		if exclusion.excludes(f) {
			excluded += f.Added + f.Deleted
		}
	}
	if excluded >= changedLines {
		return 0
	}
	return changedLines - excluded
}
