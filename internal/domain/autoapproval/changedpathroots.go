package autoapproval

import "sort"

// This file (changedpathroots.go) is the knowledge-retrieval gate's own
// second server-derived key (technical plan §31.6): the deterministic,
// path-scoped selector both modes share keys on "the PR's own
// ChangedPaths-derived tags/directory-roots" -- ClassifyChangedPaths
// (blastradius.go, this same package) is the tags half; ClassifyChangedRoots
// below is the roots half, deliberately living alongside it rather than
// in internal/domain/reviewtriage: that package already computes a
// STRUCTURALLY DIFFERENT reduction of the identical underlying fact
// (distinctRoots, decide.go) -- a bare COUNT feeding §26.3's own
// deep-routing threshold, never the actual root NAME strings a gate
// query needs to overlap against. Reusing reviewtriage's own topLevelRoot
// would mean this package importing a routing-decision package for one
// unexported helper; this file instead defines its own, matching
// reviewtriage.topLevelRoot's definition exactly (a path's first
// "/"-delimited segment) so both packages describe the identical concept
// of "root" without a cross-package dependency neither otherwise needs.
//
// Pure, no I/O (CLAUDE.md/§11) -- mirrors ClassifyChangedPaths' own
// identical purity and its own "deterministically from the PR's own
// server-fetched changed-file paths, never accepted from a caller"
// contract: both the write-time stamp (a verdict's own arch_decision_roots
// column, computed at INSERT from the SAME turn's own already-fetched
// ChangedPaths) and the read-time gate query (the current PR's own fresh
// ChangedPaths) call this SAME function, so the two sides are always
// comparable -- never two independently-drifting notions of "root".

// ClassifyChangedRoots deterministically maps paths (a PR's own
// server-fetched changed-file listing) onto the SORTED, DEDUPLICATED set
// of distinct top-level path roots they touch -- "internal/domain/review/
// context.go" contributes "internal"; a repo-root file with no "/" at all
// (e.g. "README.md") contributes itself, its own root. Sorted so two
// calls over the same SET of paths (regardless of input order -- GitHub's
// own changed-file listing order is not a fact this function's callers
// should have to depend on) always produce byte-for-byte identical
// output, the same determinism guarantee ClassifyChangedPaths' own FIXED
// tagClassifiers iteration order gives it one function over.
//
// A nil/empty paths returns nil, never a zero-length-but-non-nil slice --
// mirrors ClassifyChangedPaths' own identical "nil in, nil out" contract,
// so a caller's own len()/nil-check works identically against either
// function's result.
func ClassifyChangedRoots(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		root := topLevelChangedRoot(p)
		if root == "" {
			continue
		}
		seen[root] = true
	}
	if len(seen) == 0 {
		return nil
	}

	roots := make([]string, 0, len(seen))
	for root := range seen {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots
}

// topLevelChangedRoot returns p's own first path segment -- "internal/
// domain/review/context.go" -> "internal"; a path with no "/" at all
// returns itself, its own root; an empty path returns "". Deliberately
// NOT normalized (no case-folding, no leading-"/" trim) the way
// normalizeChangedPath (blastradius.go) is: a root is compared against
// another root computed the SAME way on both the write and read side of
// the gate (this file's own top doc comment), never against a
// human-authored allowlist the way a tag classifier's segment matching
// is -- there is no case-insensitivity requirement to satisfy here, only
// self-consistency, which an unconditional identity function already
// gives by construction. Mirrors internal/domain/reviewtriage's own
// topLevelRoot (decide.go) byte-for-byte, deliberately duplicated rather
// than imported -- see this file's own top doc comment for why.
func topLevelChangedRoot(p string) string {
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return p
}
