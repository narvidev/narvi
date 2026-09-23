package boot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"

	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// dependencyManifestFilenames is the closed, fixed set of per-ecosystem
// dependency-lockfile BASENAMES §19.6's first bullet discovers, at ANY
// depth under a repo's root (adversarial-review finding B2: a root-only
// scan silently ignored a monorepo's own per-subdirectory lockfiles --
// see ComputeDependencyManifestDigest's own doc comment for the walk this
// set now feeds). This Go slice IS this codebase's own canonical
// specification of "the set discovered at build time" (§19.6:
// "package-lock.json, pnpm-lock.yaml, requirements.txt, go.sum, ...") --
// whatever bakes /narvi/image-manifest.json's own dependencyManifestDigests
// field (an external, unmodeled build service, exactly like
// ImageManifest.Fingerprint's own doc comment already establishes for the
// analogous baked-vs-recomputed split) is expected to discover the SAME
// set, at the SAME depth (unbounded, modulo dependencyManifestSkipDirs
// below); see docs/environments.md and docs/TECHNICAL_PLAN.md §19.1 item 4
// for the contract stated in prose, next to the delta-script contract this
// same Step adds.
var dependencyManifestFilenames = []string{
	"Cargo.lock",
	"composer.lock",
	"Gemfile.lock",
	"go.sum",
	"package-lock.json",
	"Pipfile.lock",
	"pnpm-lock.yaml",
	"poetry.lock",
	"requirements.txt",
	"yarn.lock",
}

// dependencyManifestFilenameSet is dependencyManifestFilenames as a set,
// for O(1) membership testing against each file basename the walk below
// visits -- built once at package init from the same literal above, so
// the two can never drift apart.
var dependencyManifestFilenameSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(dependencyManifestFilenames))
	for _, name := range dependencyManifestFilenames {
		set[name] = struct{}{}
	}
	return set
}()

// dependencyManifestSkipDirs is the closed, fixed set of directory
// BASENAMES ComputeDependencyManifestDigest's own recursive walk never
// descends into, at any depth -- version-control internals plus the
// well-known vendored-dependency and build-output directories of every
// ecosystem dependencyManifestFilenames itself covers. Exactly like that
// set, this one IS the canonical specification (docs/TECHNICAL_PLAN.md
// §19.1 item 4, docs/environments.md): an external build-time producer
// implementing the SAME algorithm must skip exactly this set, no more, no
// less, or its baked digest can permanently disagree with every boot-time
// recompute for a repo that happens to nest a real, in-scope manifest
// under a directory only ONE side's implementation skips.
//
//   - ".git"             -- version-control internals, never a workspace file.
//   - "node_modules"     -- npm/yarn/pnpm's own installed-package tree.
//   - "vendor"           -- Go/PHP/Ruby's own vendored-dependency tree.
//   - "bower_components" -- legacy front-end vendored-dependency tree.
//   - "dist", "build"    -- generic build-output directories.
//   - "target"           -- Rust (cargo) / JVM (Maven/Gradle) build output.
//   - ".venv", "venv"    -- Python virtual environments.
//   - "__pycache__"      -- Python bytecode cache.
//
// The repo root itself is always descended into regardless of its own
// basename (ComputeDependencyManifestDigest special-cases the root path
// explicitly, before this set is ever consulted) -- a repo directory that
// happens to be named e.g. "build" must never be treated as its own
// skip-dir.
var dependencyManifestSkipDirs = map[string]struct{}{
	".git":             {},
	"node_modules":     {},
	"vendor":           {},
	"bower_components": {},
	"dist":             {},
	"build":            {},
	"target":           {},
	".venv":            {},
	"venv":             {},
	"__pycache__":      {},
}

// ComputeDependencyManifestDigest implements §19.6's own digest, as
// redefined by adversarial-review findings B1/B2 against the pre-fix
// version (which scanned repoDir's own root only, and could never tell
// "no recognized manifest anywhere" apart from "manifests found, all
// empty"). This is now this codebase's own canonical, precise
// specification for the algorithm docs/TECHNICAL_PLAN.md §19.1 item 4 and
// docs/environments.md describe in prose -- an external build-time
// producer must match it byte for byte, or its baked digest can never
// agree with a boot-time recompute even for a genuinely-unchanged repo.
//
// Algorithm:
//
//  1. Walk repoDir recursively (filepath.WalkDir), never descending into
//     any directory whose BASENAME is in dependencyManifestSkipDirs (at
//     any depth) -- the repo root itself is always descended into
//     regardless of its own basename. Symlinks are never followed
//     (fs.WalkDir's own documented behavior: a symlink is visited as a
//     leaf, never traversed as a directory), so this walk can never escape
//     repoDir or loop forever on a cyclic symlink.
//  2. For every remaining regular file whose BASENAME is in
//     dependencyManifestFilenameSet, record its path relative to repoDir,
//     canonicalized with forward slashes (filepath.ToSlash) regardless of
//     host OS -- both a build-time producer running on Linux and a
//     boot-time recompute on any OS must produce the identical string for
//     the identical logical path.
//  3. Sort the collected relative paths lexically (byte-wise on the
//     slash-joined string) -- canonical and platform-independent by
//     construction, so the digest never depends on filesystem iteration
//     order.
//  4. In that sorted order, SHA-256 each file's own content individually,
//     and fold "<relative-path>\x00<hex(sha256(content))>\n" into one
//     outer SHA-256. Folding the PATH, not just the basename, means both
//     WHICH manifests are present, AT WHICH relative location, and their
//     own content are all baked into the digest: adding, removing, or
//     RELOCATING a manifest (e.g. hoisting web/package-lock.json to the
//     repo root) changes the digest even when every byte of every
//     remaining file stays identical -- never a silent collision.
//
// Returns (digest, found, err):
//
//   - found is true iff at least one recognized manifest was actually
//     discovered anywhere under repoDir. This is the B1 fix's own
//     explicit, impossible-to-ignore signal: a repo with ZERO recognized
//     manifests anywhere (an ecosystem this codebase doesn't recognize at
//     all -- Maven/Gradle's pom.xml/build.gradle, bun, uv, deno, mix,
//     Swift, pubspec, .NET -- or a monorepo whose lockfiles this scan's own
//     bound excludes) is fundamentally different from a repo whose
//     manifests were found and legitimately hash to some particular value:
//     the FIRST case has produced zero evidence, and digest is the empty
//     string in that case -- there is deliberately no well-defined "digest
//     of nothing" value a caller could mistake for real evidence.
//     evaluateDependencySkip (below) resolves found: false to
//     DependencySkipIneligible unconditionally, before it ever looks at
//     digest's own value, and this holds regardless of what any baked
//     value says: an empty scan must never be comparable at all, on
//     either side of the comparison.
//   - err is non-nil ONLY for a genuine I/O failure -- walking a directory
//     this process cannot list, or reading a manifest file that DOES exist
//     (e.g. permission denied). Every caller treats a non-nil error
//     identically to found: false (evaluateDependencySkip's own
//     "unreadable... means ineligible" case) -- conservative, never a
//     silent skip.
func ComputeDependencyManifestDigest(repoDir string) (digest string, found bool, err error) {
	var relPaths []string
	walkErr := filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == repoDir {
			// The repo root itself: always descended into, regardless of
			// its own basename -- see dependencyManifestSkipDirs' own doc
			// comment for why this check comes before the skip-dir lookup
			// below rather than folding the root into that same lookup.
			return nil
		}
		if d.IsDir() {
			if _, skip := dependencyManifestSkipDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if _, recognized := dependencyManifestFilenameSet[d.Name()]; !recognized {
			return nil
		}
		rel, relErr := filepath.Rel(repoDir, path)
		if relErr != nil {
			return fmt.Errorf("boot: compute relative path for %s under %s: %w", path, repoDir, relErr)
		}
		relPaths = append(relPaths, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		return "", false, fmt.Errorf("boot: walk %s for dependency manifests: %w", repoDir, walkErr)
	}
	if len(relPaths) == 0 {
		// found: false -- deliberately no "digest of nothing" value here
		// (see this function's own doc comment): an empty scan returns an
		// empty digest, never sha256("")'s own well-defined-looking hex
		// string, so a caller that forgets to check found first gets an
		// obviously-wrong value instead of a plausible one.
		return "", false, nil
	}
	sort.Strings(relPaths)

	outer := sha256.New()
	for _, rel := range relPaths {
		data, readErr := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(rel)))
		if readErr != nil {
			return "", false, fmt.Errorf("boot: read dependency manifest %s: %w", rel, readErr)
		}
		inner := sha256.Sum256(data)
		// hash.Hash.Write (via outer, a *sha256 digest) never returns a
		// non-nil error per its own documented contract -- explicitly
		// discarded rather than checked, matching the standard library's
		// own convention for writing into a hash.Hash.
		_, _ = fmt.Fprintf(outer, "%s\x00%x\n", rel, inner)
	}
	return hex.EncodeToString(outer.Sum(nil)), true, nil
}

// DependencySkipOutcome is the digest tier's own per-repo verdict (§19.6
// first bullet): whether the baked dependency-manifest digest -- read from
// /narvi/image-manifest.json's own new dependencyManifestDigests field,
// beside built_repo_shas -- proves this repo's dependencies are unchanged
// since the image was built, and, when it does not, WHY not. Match and
// Ineligible are deliberately DIFFERENT outcomes even though both fall
// through to the tier below identically in behavior: Match's absence means
// either "the digests genuinely disagree" (Mismatch -- a clean, POSITIVE
// proof dependencies moved) or "there is nothing trustworthy to compare at
// all" (Ineligible -- an unreadable/absent/unrecognized baked digest, or a
// boot-side compute error) -- §19.6's own explicit instruction is that
// these two must never be conflated in behavior OR in an operator's own
// reading of the boot log.
type DependencySkipOutcome string

const (
	// DependencySkipIneligible reports that no baked digest for this repo
	// could be trusted (manifest missing entirely, this repo has no entry,
	// the baked value is not a recognized digest encoding, or the
	// boot-side recompute itself failed) -- §19.6's own "unreadable,
	// absent, or unrecognized... means ineligible, fall through... never a
	// silent skip".
	DependencySkipIneligible DependencySkipOutcome = "ineligible"
	// DependencySkipMatch reports that the baked and recomputed digests
	// agree -- setup.sh is skipped entirely, no delta script needed.
	DependencySkipMatch DependencySkipOutcome = "match"
	// DependencySkipMismatch reports that both digests were read/computed
	// successfully and they disagree -- a genuine, POSITIVE proof
	// dependencies moved (§19.6's own "a manifest-digest mismatch DOES
	// prove dependencies moved" -- unlike workspaceMoved's own SHA
	// inequality, which proves nothing about dependencies at all). Still
	// falls through to the delta/full tiers below, and still never fatal
	// on a subsequent install failure -- see EvaluateHook's own HookDelta
	// doc comment and §19.6's own "the reason changes... but the rule
	// still holds, on availability grounds" instruction. This proof does
	// NOT license skipping the delta tier -- delta eligibility is an
	// orthogonal question (has setup.sh ITSELF changed), evaluated
	// independently regardless of this outcome.
	DependencySkipMismatch DependencySkipOutcome = "mismatch"
)

// isRecognizedDigestHex reports whether s looks like a hex-encoded SHA-256
// digest (64 lowercase hex characters) -- §19.6's own "unrecognized digest"
// case for a baked value that IS present but is not, in fact, a digest this
// reader could ever have produced itself (a build-service bug, a
// differently-shaped placeholder, ...). ComputeDependencyManifestDigest's
// own output always satisfies this by construction, so this check is only
// ever meaningful against a BAKED value, never a freshly-recomputed one.
func isRecognizedDigestHex(s string) bool {
	if len(s) != hex.EncodedLen(sha256.Size) {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// evaluateDependencySkip is the pure decision behind DependencySkipOutcome:
// given whether a manifest was found at all, this repo's own baked digest
// (bakedDigest, bakedOK -- bakedOK is false when this repo has no entry in
// the manifest's dependencyManifestDigests map at all, mirroring
// ComputeWorkspaceMoved's own "no entry -> no basis to claim warm" missing-
// key handling exactly), this repo's freshly-recomputed digest (currentDigest,
// currentFound, currentErr from ComputeDependencyManifestDigest above), and
// scoped (§19.7, adversarial-review finding B5 -- true whenever THIS boot
// applied sparse-checkout, i.e. the session's own Environment.PathScope is
// non-empty), returns the tier's own verdict. Every ineligible path is
// deliberately ordered before the comparison itself, per §19.6's own
// "unreadable, absent, or unrecognized... means ineligible" -- never a
// comparison against a value this function cannot actually trust.
//
// # currentFound (adversarial-review finding B1)
//
// currentFound: false means ComputeDependencyManifestDigest's own recursive
// scan discovered ZERO recognized manifests anywhere under this repo's
// checked-out tree -- an ecosystem this codebase's own dependencyManifestFilenames
// doesn't recognize at all (Maven/Gradle, bun, uv, deno, mix, Swift,
// pubspec, .NET, ...), or a monorepo whose lockfiles this scan's own bound
// excludes. This is resolved to Ineligible UNCONDITIONALLY, before
// bakedDigest is even consulted: the pre-fix version of this function
// compared currentDigest (the SHA-256 of an empty input in this exact case)
// against whatever bakedDigest happened to be, and a build-time producer
// running the SAME algorithm against the SAME kind of repo bakes that
// identical empty-input digest -- so an unsupported-ecosystem repo matched
// FOREVER, on every boot, regardless of what actually changed in it. A scan
// that found zero evidence must never be treated as evidence of anything,
// on EITHER side of the comparison -- it is not "hashed a magic marker so
// it merely differs", it is structurally excluded from ever being compared
// at all.
//
// # scoped (adversarial-review finding B5)
//
// scoped: true is likewise resolved to Ineligible unconditionally,
// regardless of currentFound/currentDigest. §19.1 states shared images are
// "always built unscoped, ever" -- the baked bakedDigest therefore always
// reflects a FULL, unscoped tree. Under a scoped session, gitclone.SyncAll
// applies sparse-checkout BEFORE this tier ever runs (§19.7), so the
// boot-side recompute walks a TRUNCATED tree that may be missing manifests
// the baked digest accounted for. Without this guard, a scope that excludes
// some (not all) of a repo's manifests would still produce currentFound:
// true (the surviving manifests ARE found) with a currentDigest that
// legitimately differs from bakedDigest -- resolving to DependencySkipMismatch,
// which the caller's own structured log records as POSITIVE PROOF
// dependencies moved (§19.6's own "a manifest-digest mismatch DOES prove
// dependencies moved" reasoning). That proof does not hold here: the
// mismatch would be a pure artifact of scope coverage, not evidence of any
// real dependency change, and logging it as proof would be a fleet-wide
// false claim for every scoped session. Ineligible is the correct,
// conservative verdict regardless of what the scoped scan happens to find:
// there is nothing under a scoped checkout this tier can ever trust as
// complete evidence, so it never gets to run at all.
func evaluateDependencySkip(manifestFound bool, bakedDigest string, bakedOK bool, currentDigest string, currentFound bool, currentErr error, scoped bool) DependencySkipOutcome {
	if scoped || !manifestFound || !bakedOK || !isRecognizedDigestHex(bakedDigest) || currentErr != nil || !currentFound {
		return DependencySkipIneligible
	}
	if bakedDigest == currentDigest {
		return DependencySkipMatch
	}
	return DependencySkipMismatch
}

// builtSHAPattern matches a full, lowercase, 40-character hex commit sha --
// the only shape a REAL `git rev-parse HEAD` (image-build-time, an
// EXTERNAL, unmodeled build service -- ImageManifest's own doc comment)
// ever produces for a BuiltRepoShas entry. Mirrors
// internal/app/sessionactor/previewpr.go's own pushedShaPattern: this
// codebase's existing, already-shipped fix for the identical class of
// problem -- an externally-produced string (there, a sandbox-reported push
// sha arriving over the wire; here, ImageManifest.BuiltRepoShas[name],
// decoded straight from /narvi/image-manifest.json's own JSON, manifest.go,
// with no schema constraint on shape) reaching a git subprocess ARGUMENT
// with no shell in between.
//
// This is never shell injection (exec.CommandContext passes argv directly,
// no shell parses it) -- it is git ARGUMENT injection, and setupUnchangedSinceBuild's
// own "--" (which ends OPTION parsing for everything after it) does nothing
// to protect builtSHA specifically: builtSHA is passed BEFORE that "--", in
// git's own option/revision zone, so a value beginning with "-" (e.g.
// something shaped like "--upload-pack=...") is consumed by git's own
// argument parser as an OPTION, not a revision -- silently changing what
// the diff command means, and therefore what this predicate concludes.
// That matters here specifically because this exact predicate now gates
// the digest-tier skip (§19.6): a manipulated verdict would re-open the
// "skip setup.sh when it should have run" hole this Step's own ladder
// exists to close.
var builtSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// setupUnchangedSinceBuild answers §19.6's third-bullet delta-script
// eligibility predicate EXACTLY as specified, no new hashing scheme: `git
// -C repoDir diff --quiet builtSHA HEAD -- setup.sh`. A clean (exit 0) diff
// means setup.sh is byte-identical between the built SHA and the checked-
// out HEAD -- returns (true, nil). A real diff (exit 1, git's own documented
// convention for `--quiet`) returns (false, nil): a clean, definitive "no",
// never an error. This SAME single predicate also correctly covers a branch
// that ADDED or REMOVED setup.sh entirely (§19.6: "no separate empty-case
// handling needed") -- git diff already treats a path's appearance/
// disappearance as a real diff on that path, exit 1, handled identically to
// any other content change.
//
// builtSHA is validated against builtSHAPattern BEFORE it ever reaches
// exec.CommandContext below -- see that var's own doc comment for why "--"
// alone does not protect this specific argument. A malformed value (empty,
// non-hex, wrong length, or shaped like an option) is returned as a genuine
// error here, exactly like every other failure this function reports; it is
// NEVER silently treated as "unchanged". The caller, ComputeSetupRerunLadder,
// already folds any non-nil error from this function into DeltaEligible:
// false -- §19.6's own "any git error on this check is conservative:
// ineligible, fall through to full setup.sh" rule, so a rejected builtSHA
// resolves to the SAME conservative "ineligible" outcome §19.6 requires,
// never a spurious skip.
//
// Any OTHER outcome (git itself missing, builtSHA not resolvable in this
// repo's own object store, a timeout, any exit code other than 0/1) is
// likewise returned as a genuine error -- §19.6's own "any git error on
// this check is conservative: ineligible, fall through to full setup.sh".
//
// Step 171 (§30.5): run via gitdir.Run -- the SAME choke point
// (SyncHeadIn/SyncHeadOut bracket around a githarden.Args-hardened spawn,
// through sup, never a bare exec.CommandContext) every other sandbox-agent
// git invocation now uses -- rather than the bare, unhardened
// `exec.CommandContext("git", "-C", repoDir, ...)` this used to run
// directly against the runtime-owned worktree .git. This diff never moves
// HEAD, so gitdir.Run's own SyncHeadOut bracket is always a no-op here in
// practice; SyncHeadIn still runs first, keeping the agent-owned HEAD this
// spawn implicitly resolves "HEAD" against up to date with whatever the
// runtime currently has checked out.
func setupUnchangedSinceBuild(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, cred *syscall.Credential, builtSHA string, timeout, stopGrace time.Duration) (bool, error) {
	if !builtSHAPattern.MatchString(builtSHA) {
		return false, fmt.Errorf("boot: image manifest built_repo_shas entry for %s is not a well-formed git object id: %q", repo.WorkTree, builtSHA)
	}

	spec := supervisor.Spec{
		Path: "git",
		Args: githarden.Args(repo, "diff", "--quiet", builtSHA, "HEAD", "--", string(dependencyManifestSetupFilename)),
		Env:  githarden.Env(nil),
	}
	result, err := gitdir.Run(ctx, sup, repo, cred, spec, timeout, stopGrace)
	if err != nil {
		return false, fmt.Errorf("boot: git diff --quiet %s HEAD -- setup.sh in %s: %w", builtSHA, repo.WorkTree, err)
	}
	switch {
	case result.Err != nil:
		return false, fmt.Errorf("boot: git diff --quiet %s HEAD -- setup.sh in %s: %w", builtSHA, repo.WorkTree, result.Err)
	case result.ExitCode == 0:
		return true, nil
	case result.ExitCode == 1:
		return false, nil
	default:
		return false, fmt.Errorf("boot: git diff --quiet %s HEAD -- setup.sh in %s: exited %d", builtSHA, repo.WorkTree, result.ExitCode)
	}
}

// dependencyManifestSetupFilename names the one file setupUnchangedSinceBuild
// diffs -- a local alias for sandboxboot.HookSetup's own literal value
// ("setup.sh") kept as a plain string here rather than importing
// internal/domain/sandboxboot into this file: every other file in this
// package already imports that package for its own reasons (hooks.go,
// runboot.go), so this avoids adding a fourth redundant import purely for
// one string literal used by exactly one function.
const dependencyManifestSetupFilename = "setup.sh"

// SetupRerunLadder is one repo's own precomputed input to §19.6's graduated
// setup-rerun ladder -- both fields computed ONCE per boot (never per-hook)
// by ComputeSetupRerunLadder below, mirroring workspaceMoved's own "one
// manifest read covers every repo" shape exactly. Consulted by
// internal/sandboxagent/boot's own runSetupRerunLadder (hooks.go) for
// exactly the one cell it applies to: BootModeRepoImage's HookSetup, when
// workspaceMoved is already true.
type SetupRerunLadder struct {
	// DependencySkip is the digest tier's own verdict (§19.6 first bullet).
	DependencySkip DependencySkipOutcome
	// DeltaEligible is §19.6's third-bullet predicate: setup.sh itself is
	// provably unchanged since the built SHA (setupUnchangedSinceBuild
	// returned (true, nil)). This says nothing about whether sync.sh
	// actually EXISTS on disk -- that is checked at run time, exactly like
	// every other hook's own presence check (hookScriptPresent).
	//
	// Adversarial-review finding B3: this SAME fact ALSO gates the digest
	// tier, not just the delta tier -- internal/sandboxagent/boot's own
	// runSetupRerunLadder (hooks.go) requires DependencySkip ==
	// DependencySkipMatch AND DeltaEligible before it ever skips setup.sh
	// entirely. A digest match only proves the DEPENDENCY MANIFEST
	// (lockfile) surface is unchanged; it says nothing about setup.sh's own
	// non-package-manager work (§19.4's own "may provision local service
	// stacks, run codegen, seed local state" framing) -- work no lockfile
	// digest can ever speak for. So a digest match can never by itself
	// license skipping a setup.sh that has ITSELF changed since the image
	// was built; only a digest match COMBINED WITH a provably-unchanged
	// setup.sh together prove the rerun is genuinely unnecessary.
	DeltaEligible bool
}

// RerunReason is the closed, structured vocabulary §19.6's own instruction
// requires every ladder decision to log (§5.3): "skip / delta / full /
// ineligible-fallback", extended by RerunReasonRetry below for the
// full-setup.sh tier's own required retry (§19.6's manifest-digest bullet).
// internal/sandboxagent/boot's own runSetupRerunLadder logs one line per
// tier/decision actually consulted, each carrying exactly one of these
// values as its own "outcome" attribute -- so a single repo's boot can
// produce more than one logged decision (e.g. "digest tier:
// ineligible-fallback" immediately followed by "delta tier: delta"), each
// individually auditable, not merely the final result.
type RerunReason string

const (
	// RerunReasonSkip reports that the digest tier proved dependencies
	// unchanged -- nothing runs.
	RerunReasonSkip RerunReason = "skip"
	// RerunReasonDelta reports that the delta script (sync.sh) is eligible
	// and present -- it runs instead of full setup.sh.
	RerunReasonDelta RerunReason = "delta"
	// RerunReasonFull reports that full setup.sh runs -- the ladder's own
	// floor, reached whenever neither tier above actually resolved the
	// decision (a clean digest mismatch, a delta script that is
	// ineligible/absent, or a delta script that ran and failed).
	RerunReasonFull RerunReason = "full"
	// RerunReasonIneligibleFallback reports that a TIER's own check could
	// not be trusted (no/unrecognized baked digest, a boot-side compute
	// error, a git error on the setup.sh-unchanged check) -- that tier
	// falls through to the next one down, conservatively, never a silent
	// skip.
	RerunReasonIneligibleFallback RerunReason = "ineligible-fallback"
	// RerunReasonRetry reports that the FULL setup.sh tier's own first
	// attempt failed and §19.6's own required retry (the manifest-digest
	// bullet: "retry the install on transient failure, then warn -- never
	// fail the boot on it") is about to make ONE more attempt, before
	// falling back to the same warn-and-continue outcome as today. Logged
	// as its own decision -- distinct from RerunReasonFull's own "the
	// floor was reached, about to run" line -- so an operator reading the
	// boot log can tell "ran once, succeeded" apart from "ran once,
	// failed, retried" purely from structured log lines, consistent with
	// §19.6's own "every ladder decision logs a structured reason" rule.
	RerunReasonRetry RerunReason = "retry"
)

// ComputeSetupRerunLadder computes §19.6's own two additional per-repo
// predicates -- beyond workspaceMoved itself -- for every repo currentSHAs
// names, unconditionally, regardless of mode/workspaceMoved: exactly
// ComputeWorkspaceMoved's own precedent (manifest.go), computed
// unconditionally at the same point in the boot sequence because
// EvaluateHook-driven callers only ever actually CONSULT these values for
// the one cell that matters (repo_image + HookSetup + workspaceMoved), so
// computing them uniformly keeps this call site as simple as
// ComputeWorkspaceMoved's own.
//
// A missing/unreadable manifest (manifestFound: false) makes every repo's
// own DependencySkip resolve to DependencySkipIneligible and DeltaEligible
// to false -- the identical safe default ComputeWorkspaceMoved documents
// for its own "assume moved" case, applied here as "assume neither
// cheaper tier is available, fall all the way through to full setup.sh" --
// never a silent skip, never a spuriously-preferred delta script.
//
// scoped (§19.7, adversarial-review finding B5) is true whenever THIS
// boot's own session applied sparse-checkout (a non-empty
// SessionConfig.PathScope) -- passed straight through to
// evaluateDependencySkip for every repo, forcing DependencySkip to
// DependencySkipIneligible unconditionally regardless of what the
// (necessarily scope-truncated) recursive manifest scan finds. See
// evaluateDependencySkip's own doc comment for why: shared images are
// always built unscoped (§19.1), so a baked digest can never be trusted
// against a boot-time tree this session itself narrowed. This does NOT
// affect DeltaEligible below -- setupUnchangedSinceBuild diffs two
// COMMITS via git's own object store, never the working tree, so
// sparse-checkout has no bearing on its own correctness.
func ComputeSetupRerunLadder(ctx context.Context, sup *supervisor.Supervisor, layout gitdir.Layout, cred *syscall.Credential, manifest ImageManifest, manifestFound bool, scoped bool, workspaceDir string, currentSHAs map[string]string, timeout, stopGrace time.Duration) map[string]SetupRerunLadder {
	ladder := make(map[string]SetupRerunLadder, len(currentSHAs))
	for name := range currentSHAs {
		repoDir := filepath.Join(workspaceDir, name)

		currentDigest, currentFound, digestErr := ComputeDependencyManifestDigest(repoDir)
		bakedDigest, bakedOK := manifest.DependencyManifestDigests[name]
		skip := evaluateDependencySkip(manifestFound, bakedDigest, bakedOK, currentDigest, currentFound, digestErr, scoped)

		var eligible bool
		if manifestFound {
			if builtSHA, ok := manifest.BuiltRepoShas[name]; ok {
				repo := layout.Repo(name)
				unchanged, err := setupUnchangedSinceBuild(ctx, sup, repo, cred, builtSHA, timeout, stopGrace)
				eligible = err == nil && unchanged
			}
		}

		ladder[name] = SetupRerunLadder{DependencySkip: skip, DeltaEligible: eligible}
	}
	return ladder
}
