package autoapproval

import (
	"path"
	"strings"

	"github.com/narvidev/narvi/internal/domain/review"
)

// This file (blastradius.go) is the path→tag classifier §21.2's own
// sensitive-path criterion needs (C1): "no
// sensitive path touched -- a configurable-per-repo list", checked against
// the PR's own SERVER-FETCHED changed-file paths (ports.OpenPR.
// ChangedFiles), never the reviewing model's own self-reported
// Verdict.BlastRadius (eligibility.go's own doc comment explains why that
// distinction is the whole point of this Step's fix).
//
// Checked directly before writing this: no such classifier exists anywhere
// in this tree today (grepped for every BlastRadius/review.Tag construction
// site -- reviewpost/validate.go, reviewverdict/{convert,insert}.go,
// httpapi/reviewverdict.go, and this package's own eligibility.go are the
// only five, and every one of them either VALIDATES or FORWARDS a
// caller-supplied []review.Tag, never DERIVES one from paths). §26.3's own
// "sensitive globs ... mapped deterministically onto the same BlastRadius
// tags" is a LATER, broader design (per-repo-configurable
// deepPaths, feeding the light/deep review-triage fork, coming after
// this file's own gate) this file deliberately does NOT
// attempt to build ahead of schedule. What follows is a narrower,
// self-contained v1 sufficient for THIS Step's own gate: a FIXED, built-in
// mapping, no per-repo configuration of the globs themselves (only WHICH
// of the resulting tags counts as "sensitive" is per-repo-configurable,
// via EligibilityConfig.SensitiveTags -- unchanged by this file). A
// straightforward, additive rewrite once §26.3 actually lands, never a
// migration this file needs to anticipate.
//
// # Design: deliberately over-inclusive, never under-inclusive
//
// Every rule below is a coarse, best-effort heuristic over a REPO-RELATIVE
// PATH STRING alone -- there is no way to definitively know, from a path
// alone, whether "author.go" is about authentication or a book's author
// field. This file resolves every such ambiguity toward INCLUSION: the
// cost of a false positive here is one PR that could have auto-approved
// instead routing to a human (an inconvenience); the cost of a false
// negative is a genuinely sensitive change (a real migration, a real
// authz rewrite) silently auto-merging unattended (a safety incident).
// §21's own review verdict names this exact asymmetry as the theme of the
// whole round ("fail direction"). Every rule is intentionally simple
// (path-segment/filename-prefix/extension checks, no per-repo tuning) --
// a repo with unusual conventions this file's rules do not anticipate can
// still widen ITS OWN sensitive posture by lowering
// EligibilityConfig.MaxFilesChanged, or by naming `review:needs-human`
// on a specific PR (§21.2's own escape hatch) -- this file does not need
// to be a perfect, self-tuning classifier to close the C1 hole, only a
// genuinely SERVER-DERIVED one that cannot be lied to via a POST body.
//
// # Matching is on repo-relative, forward-slash-separated paths
//
// Every real caller (ports.OpenPR.ChangedFiles, via githubapi's own
// pullFileResponse.Filename) already hands this exactly this shape --
// GitHub's Pull Request Files API always reports forward-slash-separated,
// repo-relative paths with no leading slash, regardless of the reviewing
// machine's own OS -- so this file uses the "path" package (URL/POSIX-
// style, forward-slash-only) throughout, deliberately never
// "path/filepath" (OS-separator-dependent, wrong choice for a path string
// that never actually touches this process's own filesystem).

// tagClassifier pairs one review.Tag with the pure predicate that decides
// whether a single normalized path touches it.
type tagClassifier struct {
	tag   review.Tag
	match func(normalizedPath string) bool
}

// tagClassifiers is consulted in this FIXED order for every path -- the
// order ClassifyChangedPaths' own output slice is built in, so two calls
// over the same input always produce byte-identical output (a table-driven
// test's own "compare a []review.Tag literal directly" convenience, and a
// stable ordering for anything that renders this list to a human).
var tagClassifiers = []tagClassifier{
	{review.TagMigrations, matchesMigrations},
	{review.TagAuth, matchesAuth},
	{review.TagContracts, matchesContracts},
	{review.TagSecrets, matchesSecrets},
	{review.TagInfra, matchesInfra},
	{review.TagPublicAPI, matchesPublicAPI},
	{review.TagDataLayer, matchesDataLayer},
	{review.TagDependencies, matchesDependencies},
}

// ClassifyChangedPaths deterministically maps paths (a PR's own
// server-fetched changed-file listing, ports.OpenPR.ChangedFiles) onto the
// subset of review's eight-value Tag vocabulary those paths touch -- the
// SAME shape review.Verdict.BlastRadius carries, but computed here from
// facts GitHub itself reports, never accepted from a caller. Pure, no I/O
// (CLAUDE.md/§11). A nil/empty paths (a PR whose changed-files listing
// could not be determined, or a genuinely empty diff) returns nil, never a
// zero-length-but-non-nil slice, mirroring reviewpost.BuildFindings' own
// identical "nil in, nil out" convention -- a caller's own
// len()/nil-check works identically either way.
func ClassifyChangedPaths(paths []string) []review.Tag {
	if len(paths) == 0 {
		return nil
	}

	touched := make(map[review.Tag]bool, len(tagClassifiers))
	remaining := len(tagClassifiers)
	for _, p := range paths {
		if remaining == 0 {
			// Every tag already matched by some earlier path -- no further
			// path can add anything new to the result, so stop scanning
			// early. A minor optimization (this is never a hot loop: a
			// real PR's changed-file count and review.Tag's own
			// eight-value vocabulary are both small), kept because it is
			// free and makes the common "most PRs touch at most one or
			// two tags" case visibly cheap.
			break
		}
		normalized := normalizeChangedPath(p)
		if normalized == "" {
			continue
		}
		for _, c := range tagClassifiers {
			if touched[c.tag] {
				continue
			}
			if c.match(normalized) {
				touched[c.tag] = true
				remaining--
			}
		}
	}

	if len(touched) == 0 {
		return nil
	}
	out := make([]review.Tag, 0, len(touched))
	for _, c := range tagClassifiers {
		if touched[c.tag] {
			out = append(out, c.tag)
		}
	}
	return out
}

// TagStrings converts tags (typically ClassifyChangedPaths' own return
// value, above) into plain strings -- the JSON-serializable shape a
// caller needs to persist tags OUTSIDE this package, e.g. internal/
// domain/reviewtriage.DecisionRecord.ArchDecisionTags (this Step's own
// §31.6 knowledge-gate carrier, riding turns.review_depth_decision) --
// mirrors internal/app/reviewverdict's own identical marshalTags
// conversion for review_verdicts.blast_radius one field over, and
// NewDecisionRecord's own pre-existing inline conversion of
// decision.MatchedSensitiveTags. nil in, nil out (never a
// zero-length-but-non-nil slice), matching ClassifyChangedPaths' own
// identical contract.
func TagStrings(tags []review.Tag) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, len(tags))
	for i, t := range tags {
		out[i] = string(t)
	}
	return out
}

// normalizeChangedPath trims a leading "/" (GitHub's own changed-file
// paths never carry one, but a defensive normalization costs nothing) and
// lower-cases the result -- every classifier in this file matches
// case-INsensitively against path segments/filenames (a repo that names
// its migrations directory "Migrations" or "DB/Migrate" is exactly as
// sensitive as one that spells it lowercase; nothing about §21.2's own
// sensitive-path concern is case-sensitive, including
// dependencyManifestFilenames' own lookup -- see that map's own doc
// comment).
func normalizeChangedPath(p string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(p), "/"))
}

// pathSegments splits a normalized, forward-slash-separated repo-relative
// path into its component segments, dropping any empty segment a
// leading/trailing/doubled "/" would otherwise produce.
func pathSegments(p string) []string {
	raw := strings.Split(p, "/")
	segments := make([]string, 0, len(raw))
	for _, s := range raw {
		if s != "" {
			segments = append(segments, s)
		}
	}
	return segments
}

// hasSegment reports whether ANY path segment (a directory name OR the
// final filename component, extension included) exactly equals one of
// names.
func hasSegment(p string, names ...string) bool {
	for _, seg := range pathSegments(p) {
		for _, name := range names {
			if seg == name {
				return true
			}
		}
	}
	return false
}

// baseNoExt returns p's final path segment (the filename) with its own
// extension (the last "." onward) stripped -- "internal/domain/auth.go"
// -> "auth"; a filename with no "." returns unchanged; a filename
// beginning with "." (e.g. ".env") is left fully intact (a leading dot is
// never treated as this file's own "extension" separator, matching
// ordinary dotfile convention).
func baseNoExt(p string) string {
	base := path.Base(p)
	if idx := strings.LastIndex(base, "."); idx > 0 {
		return base[:idx]
	}
	return base
}

// matchesMigrations: any path segment named "migrations" or "migrate" --
// §21.2's own first named default sensitive path, verbatim. Covers this
// very repo's own top-level migrations/ convention, and the equally common
// db/migrate/ (Rails) shape, via a plain segment match at any depth.
func matchesMigrations(p string) bool {
	return hasSegment(p, "migrations", "migrate")
}

// matchesAuth: §21.2's own second named default ("auth code"). A path
// segment (directory or filename WITH extension) named exactly "auth",
// "authn", or "authz", OR a filename (extension stripped) that STARTS WITH
// "auth" -- deliberately broad (this also matches "author.go", a known,
// accepted false positive; see this file's own top doc comment on why
// over-inclusion here is the correct, safe default and never a
// false-negative risk).
func matchesAuth(p string) bool {
	if hasSegment(p, "auth", "authn", "authz") {
		return true
	}
	return strings.HasPrefix(baseNoExt(p), "auth")
}

// matchesContracts: §21.2's own third named default, verbatim ("/contracts
// by default") -- a top-level (or nested) "contracts" directory segment.
func matchesContracts(p string) bool {
	return hasSegment(p, "contracts")
}

// matchesSecrets: TagSecrets' own doc comment ("secrets/credential
// handling"). A filename (extension stripped) containing "secret" or
// "credential", a dotenv-shaped filename ("." + "env" prefix, covering
// .env/.env.local/.env.production/...), a private-key-shaped extension, or
// a conventional SSH private-key filename.
func matchesSecrets(p string) bool {
	base := baseNoExt(p)
	if strings.Contains(base, "secret") || strings.Contains(base, "credential") {
		return true
	}
	filename := path.Base(p)
	if strings.HasPrefix(filename, ".env") {
		return true
	}
	switch path.Ext(filename) {
	case ".pem", ".key", ".p12", ".pfx":
		return true
	}
	for _, prefix := range []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"} {
		if strings.HasPrefix(filename, prefix) {
			return true
		}
	}
	return false
}

// matchesInfra: TagInfra's own doc comment ("deployment/infrastructure/CI
// configuration"). A path segment named for a common infra-as-code/deploy
// convention, a GitHub Actions workflow file, a Dockerfile-shaped or
// docker-compose-shaped filename, or a Terraform file extension.
func matchesInfra(p string) bool {
	if hasSegment(p, "infra", "deploy", "deployment", "k8s", "kubernetes", "helm", "terraform") {
		return true
	}
	if strings.HasPrefix(p, ".github/workflows/") {
		return true
	}
	filename := path.Base(p)
	if strings.HasPrefix(filename, "dockerfile") || strings.HasPrefix(filename, "docker-compose") {
		return true
	}
	switch path.Ext(filename) {
	case ".tf", ".tfvars":
		return true
	}
	return false
}

// matchesPublicAPI: TagPublicAPI's own doc comment ("externally-facing API
// surface"). A path segment named for a conventional API-surface
// directory, or an OpenAPI/Swagger-shaped filename.
func matchesPublicAPI(p string) bool {
	if hasSegment(p, "api", "rest", "graphql") {
		return true
	}
	base := baseNoExt(p)
	return base == "openapi" || base == "swagger"
}

// matchesDataLayer: TagDataLayer's own doc comment ("persistence/store
// code, distinct from TagMigrations"). A path segment named for a
// conventional data-access-layer directory -- deliberately excludes the
// bare segment "db" (too broad/ambiguous on its own -- Rails' own
// "db/migrate" would otherwise double-count as data_layer AND migrations
// for every single migration file) and excludes "migrations"/"migrate"
// themselves (matchesMigrations' own, deliberately SEPARATE tag).
func matchesDataLayer(p string) bool {
	return hasSegment(p, "models", "model", "repository", "repositories", "dao", "store", "stores")
}

// dependencyManifestFilenames is the fixed filename allowlist
// matchesDependencies checks against, keyed by each real manifest/
// lockfile's own LOWER-CASED name -- matched against normalizedPath's own
// already-lower-cased filename (normalizeChangedPath, ClassifyChangedPaths'
// one normalization pass every classifier in this file shares), so the
// comparison is case-insensitive end to end: "Gemfile" and "gemfile" name
// the same real file to Bundler either way, and this file's own
// established convention (every OTHER classifier above already matches
// case-insensitively, for the identical "a repo's own incidental casing
// choice is not a meaningful signal" reason) applies here too, with no
// exception -- a plain map lookup, O(1), simpler than the EqualFold scan
// an exact-case allowlist would otherwise force.
var dependencyManifestFilenames = map[string]bool{
	"go.mod": true, "go.sum": true,
	"package.json": true, "package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"gemfile": true, "gemfile.lock": true,
	"requirements.txt": true,
	"pipfile":          true, "pipfile.lock": true,
	"cargo.toml": true, "cargo.lock": true,
	"pom.xml":          true,
	"build.gradle":     true,
	"build.gradle.kts": true,
	"composer.json":    true,
	"composer.lock":    true,
}

// matchesDependencies: TagDependencies' own doc comment ("a third-party
// dependency version change") -- normalizedPath's own final path segment
// (already lower-cased) against dependencyManifestFilenames above.
func matchesDependencies(normalizedPath string) bool {
	return dependencyManifestFilenames[path.Base(normalizedPath)]
}
