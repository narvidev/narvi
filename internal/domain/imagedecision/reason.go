// Package imagedecision is the closed, exhaustive vocabulary for why one
// spawn/restore attempt's own image resolution (internal/app/sessionactor's
// resolveAndSetImage, imageresolve.go) ended where it did -- a real,
// previously-built warm image (Reason = ReasonSelected), or one of the
// early-return fallback paths that leaves plan.spec.Image at
// defaultBaseImage instead (every other Reason value below).
//
// This package holds ONLY the vocabulary itself (a plain string type plus
// its closed set of named values), never the decision logic or any I/O --
// §11's own domain discipline (no I/O, no time.Now(), no randomness in
// /internal/domain) applies here exactly as it does to every sibling
// package this one sits beside (internal/domain/imagebuild,
// internal/domain/sandboxboot).
//
// # Why a closed Go vocabulary is not enough by itself
//
// Reason is a defined string type, not a sealed sum type -- Go has no
// exhaustiveness checking for that shape, so nothing stops a stray
// imagedecision.Reason("typo") from compiling. Four mechanisms take
// different, non-overlapping bites out of that gap -- each is pinned by
// a specific test or analyzer named below, and NONE of them alone closes
// it (an earlier version of this comment claimed "two independent
// backstops" and overstated both of the two it named -- this is the
// corrected, precise account, one symbol at a time):
//
//  1. Every return path in decideImage/repoAccessAllowedForSpawn
//     (imageresolve.go -- NOT resolveAndSetImage, which itself returns
//     nothing at all) is typed to return a Reason as a POSITIONAL,
//     unnamed return value -- Go refuses to compile a bare `return` in a
//     function with unnamed return types, so a new early-return branch
//     cannot be added without supplying SOME Reason value at that exact
//     call site. This guarantees a Reason is SUPPLIED; it says nothing
//     about whether the value supplied is one All() (below) actually
//     lists -- that gap is left wide open by this mechanism alone, and
//     is what points 2 and 3 below close.
//  2. tools/lint/narvichecks/reasoncoverage's own Analyzer (run by `make
//     lint`) walks every Reason-typed constant declared in this
//     package's own const block and fails the build if one is absent
//     from All(). This is what makes a ONE-SIDED EDIT -- a new Reason
//     constant, returned from a real call site, but never added to
//     All() -- a CI failure at the point the constant is declared,
//     rather than a silent Postgres enum rejection discovered later. See
//     that Analyzer's own doc comment for exactly what it does and does
//     not catch: it is a BUILD-time check over this package's own
//     source, so it cannot see a Reason value constructed any way other
//     than a package-level constant declared here.
//  3. persistImageDecisionBestEffort's own validatedPersistReason
//     (imageresolve.go) is the RUNTIME backstop for whatever point 2's
//     build-time check cannot see: any Reason not found in All(), for
//     any reason, is substituted with ReasonUnrecognized -- itself
//     always a member of All() -- before ever reaching the write, and
//     logged at Error. Without this, a value outside All() reaching the
//     ENUM-typed sandboxes.image_decision_reason column fails the whole
//     persisting transact and silently drops both the column write and
//     the bundled event-log entry -- the exact failure mode points 1 and
//     2 above do not, by themselves, prevent.
//  4. migrations/000139_sandboxes_image_decision.up.sql persists this
//     exact vocabulary as a REAL Postgres ENUM
//     (sandboxes.image_decision_reason) -- a value outside this closed
//     set is rejected by the database itself, not merely by a Go switch
//     someone could forget to update. All() (below) is the single source
//     both that migration's own value list and
//     imagedecision_integration_test.go's own drift check are written
//     against, so the two can never silently diverge without a failing
//     test. Point 3 above is meant to make this particular rejection
//     unreachable in practice; the schema constraint stays regardless,
//     as defense in depth, not as the only thing standing between an
//     invalid value and a lost record.
package imagedecision

// Reason is the closed vocabulary itself. Every value is a real, persisted
// outcome EXCEPT ReasonNone, which exists purely as a typed placeholder
// for a caller position where "there is no reason to report" is itself
// the correct answer (repoAccessAllowedForSpawn's own all-repos-allowed
// return) -- see ReasonNone's own doc comment for why it is deliberately
// excluded from All() and therefore from the Postgres enum.
type Reason string

const (
	// ReasonSelected reports that a real, previously-built 'ready' image_builds row
	// was found for this spawn's own fingerprint, and plan.spec.Image was
	// set to it -- the one non-fallback outcome. The point of persisting
	// this alongside every fallback reason, rather than only the
	// fallbacks, is that "warm-boot hit rate" is a single GROUP BY over
	// this column once ReasonSelected is itself a countable value.
	ReasonSelected Reason = "selected"

	// ReasonNoRepos reports that the session's own SessionConfig.Repos is empty --
	// decideImage's own first statement (imageresolve.go), an early return
	// of three explicit values ("nothing to fingerprint; stays on
	// defaultBaseImage"), previously the only early-return path in this
	// file with NO log line at all.
	ReasonNoRepos Reason = "no_repos"

	// ReasonRepoAccessNoCreator reports that plan.createdBy is not a valid user id (an
	// automation/bot-created session) -- there is no per-user credential
	// to check repo access under, so the repo-access gate denies
	// unconditionally. See imageresolve.go's own "# Repo-access gate" top
	// comment for why this is an accepted cost, not a bug.
	ReasonRepoAccessNoCreator Reason = "repo_access_no_creator"

	// ReasonRepoAccessCreatorLookupFailed reports that CheckCreatorGuard's own GetByID
	// call failed (a genuine DB error, or -- per that function's own doc
	// comment -- the structurally-unreachable missing-row case).
	ReasonRepoAccessCreatorLookupFailed Reason = "repo_access_creator_lookup_failed"

	// ReasonRepoAccessCreatorDisabled reports that the session creator's CURRENT row
	// (re-read fresh, not from session-creation time) is Disabled (§13.3
	// viewer-guard parity).
	ReasonRepoAccessCreatorDisabled Reason = "repo_access_creator_disabled"

	// ReasonRepoAccessCreatorViewer reports that the session creator's CURRENT role is
	// viewer (§13.3 viewer-guard parity), re-checked fresh on every spawn.
	ReasonRepoAccessCreatorViewer Reason = "repo_access_creator_viewer"

	// ReasonRepoAccessCreatorGuardUnknown reports that CheckCreatorGuard reported
	// !Allowed but NONE of Err/Disabled/Viewer was set -- CreatorGuardVerdict's
	// own doc comment states exactly one of the four is meaningful per
	// verdict, so this is a defensive, should-be-unreachable case (mirrors
	// EvaluateHook's own "unrecognized value is a programming error, not a
	// data error" convention) -- denied fail-closed regardless, never a
	// silent fall-through.
	ReasonRepoAccessCreatorGuardUnknown Reason = "repo_access_creator_guard_unknown"

	// ReasonRepoAccessUnsupportedHost reports that a repo's clone URL does not name one
	// of ports.SupportedSourceControlHosts() (reposource.CheckRepoHost).
	ReasonRepoAccessUnsupportedHost Reason = "repo_access_unsupported_host"

	// ReasonRepoAccessUnparseableURL reports that reposource.ParseOwnerRepo could not
	// derive an owner/repo pair from a repo's clone URL.
	ReasonRepoAccessUnparseableURL Reason = "repo_access_unparseable_url"

	// ReasonRepoAccessCachedDeny reports that repoAccessCache already holds a genuine
	// (non-error) deny verdict for this (creator, repo) pair, still within
	// RepoAccessCacheTTL.
	ReasonRepoAccessCachedDeny Reason = "repo_access_cached_deny"

	// ReasonRepoAccessCircuitBreakerOpen reports that repoAccessCache's own breaker is
	// open (repeated indeterminate CheckRepoAccess failures, recently
	// enough) -- denied without spending a further network call this
	// spawn.
	ReasonRepoAccessCircuitBreakerOpen Reason = "repo_access_circuit_breaker_open"

	// ReasonRepoAccessNoToken reports that decryptCreatorGitHubToken could not produce
	// a usable plaintext token for this creator (no linked GitHub
	// identity, no stored access token, or a decrypt failure -- that
	// function's own three internal cases collapse to this one Reason,
	// since repoAccessAllowedForSpawn only ever observes its bool result,
	// not which of the three fired; see imageresolve.go's own getToken
	// doc comment).
	ReasonRepoAccessNoToken Reason = "repo_access_no_token"

	// ReasonRepoAccessNoSourceControl reports that this Actor has no ports.
	// SourceControl configured at all -- a defensive guard (some tests,
	// and any future caller genuinely without one, must not panic).
	ReasonRepoAccessNoSourceControl Reason = "repo_access_no_source_control"

	// ReasonRepoAccessCheckIndeterminate reports that CheckRepoAccess itself failed to
	// answer the question (network/timeout/5xx/rate-limited) -- never a
	// definitive "no", and deliberately never cached (see
	// repoaccesscache.go), so the very next spawn re-checks live.
	ReasonRepoAccessCheckIndeterminate Reason = "repo_access_check_indeterminate"

	// ReasonRepoAccessDenied reports that CheckRepoAccess answered, definitively, that
	// this creator cannot read this repo -- the actual attack case the
	// repo-access gate exists to close.
	ReasonRepoAccessDenied Reason = "repo_access_denied"

	// ReasonImageBuildLookupFailed reports that image_builds.Get failed with something
	// other than pgx.ErrNoRows -- a genuine, unexpected DB error.
	ReasonImageBuildLookupFailed Reason = "image_build_lookup_failed"

	// ReasonImageBuildTrackingMarshalFailed reports that image_builds.Get
	// returned ErrNoRows (no row yet for this fingerprint), and json.Marshal of the
	// normalized repo-url map -- the best-effort pending-row tracking
	// write's own input -- failed.
	//
	// UNREACHABLE today, on purpose, and this is not a merely-theoretical
	// disclaimer: json.Marshal's only argument at this value's one
	// producer (upsertPendingImageBuildBestEffort, imageresolve.go) is a
	// map[string]string, and encoding/json cannot fail to marshal a
	// map[string]string -- every key and value is already a valid JSON
	// string, there is no cycle to detect, and no unsupported type to
	// reject. Kept, rather than removed, as defensive handling for if
	// that call site's own input type ever stops being a plain
	// map[string]string (e.g. gains a value type json.Marshal genuinely
	// can reject) -- at which point this branch becomes reachable without
	// anyone having to notice and add a new Reason for it. Of All()'s 22
	// persisted values, this is not the only one whose own call site
	// should never fire in a correctly-functioning build --
	// ReasonRepoAccessCreatorGuardUnknown and ReasonUnrecognized's own
	// doc comments make that same claim for themselves. What sets this
	// one apart: its unreachability is provable from json.Marshal's own
	// argument type, not merely assumed from another component's
	// contract holding.
	ReasonImageBuildTrackingMarshalFailed Reason = "image_build_tracking_marshal_failed"

	// ReasonImageBuildTrackingUpsertFailed reports that image_builds.Get
	// returned ErrNoRows, the repo-url map marshaled fine, but the best-effort
	// UpsertPending call itself failed (DB error/timeout).
	ReasonImageBuildTrackingUpsertFailed Reason = "image_build_tracking_upsert_failed"

	// ReasonImageBuildPending reports that image_builds.Get returned ErrNoRows and the
	// best-effort pending-row tracking write succeeded (or was a pure
	// ON-CONFLICT-DO-NOTHING no-op because some concurrent caller already
	// created it) -- the ordinary "no build has ever completed for this
	// repo set yet" case, not an error of any kind.
	ReasonImageBuildPending Reason = "image_build_pending"

	// ReasonImageBuildNotReady reports that a image_builds row exists for this
	// fingerprint, but its status is pending/building/failed-but-not-yet-
	// due -- a build is already tracked and in progress or awaiting retry;
	// nothing further is written on this spawn (see imageresolve.go's own
	// doc comment on why).
	ReasonImageBuildNotReady Reason = "image_build_not_ready"

	// ReasonImageBuildReadyRowMissingRef reports that a image_builds row's status is
	// 'ready' but its image_ref is nil or empty -- a data-integrity
	// anomaly (a ready row should always carry a real image_ref) rather
	// than an ordinary "still building" state; kept distinct from
	// ReasonImageBuildNotReady so an operator can tell the two apart at a
	// glance instead of one silently masquerading as the other.
	ReasonImageBuildReadyRowMissingRef Reason = "image_build_ready_row_missing_ref"

	// ReasonUnrecognized is persistImageDecisionBestEffort's own (imageresolve.go)
	// designated in-vocabulary fallback -- see this package's own top "# Why
	// a closed Go vocabulary is not enough by itself" comment, point 3.
	// validatedPersistReason substitutes it for any Reason value that is
	// not one of All()'s own members before that value ever reaches the
	// ENUM-typed sandboxes.image_decision_reason column, logging the
	// original (invalid) value at Error. Reachable in production only if
	// a future change adds a new Reason constant to this package without
	// also adding it to All() -- tools/lint/narvichecks/reasoncoverage's
	// own Analyzer exists specifically to catch that one-sided edit
	// before it ships, so in a correctly-linted build this is expected to
	// be as unreachable as ReasonRepoAccessCreatorGuardUnknown already is
	// -- defensive, should-be-unreachable, denied/substituted regardless,
	// never a silent fall-through.
	ReasonUnrecognized Reason = "unrecognized"

	// ReasonNone is NEVER persisted -- see this package's own top comment.
	// repoAccessAllowedForSpawn returns it alongside allowed=true, the one
	// case where its own Reason return value is meaningless and the
	// caller must ignore it. Deliberately excluded from All(): the
	// Postgres enum this package's other 22 values populate
	// (migrations/000139_sandboxes_image_decision.up.sql) has NO
	// 'none'/'not_applicable' member, on purpose -- if a future bug ever
	// did try to persist ReasonNone, persistImageDecisionBestEffort's own
	// validatedPersistReason (imageresolve.go) catches it before the
	// transact even opens, substitutes ReasonUnrecognized, and logs at
	// Error, so the record still survives rather than silently adding a
	// meaningless bucket to a vocabulary whose entire purpose is being
	// counted and compared.
	ReasonNone Reason = "none"
)

// All returns every PERSISTED closed-vocabulary value, in the exact order
// declared above -- deliberately excluding ReasonNone (see its own doc
// comment). This slice is the single source both
// migrations/000139_sandboxes_image_decision.up.sql's own ENUM value list
// and imagedecision_integration_test.go's own live-Postgres drift check
// are written against, so the Go vocabulary and the database schema can
// never silently disagree without a failing test.
func All() []Reason {
	return []Reason{
		ReasonSelected,
		ReasonNoRepos,
		ReasonRepoAccessNoCreator,
		ReasonRepoAccessCreatorLookupFailed,
		ReasonRepoAccessCreatorDisabled,
		ReasonRepoAccessCreatorViewer,
		ReasonRepoAccessCreatorGuardUnknown,
		ReasonRepoAccessUnsupportedHost,
		ReasonRepoAccessUnparseableURL,
		ReasonRepoAccessCachedDeny,
		ReasonRepoAccessCircuitBreakerOpen,
		ReasonRepoAccessNoToken,
		ReasonRepoAccessNoSourceControl,
		ReasonRepoAccessCheckIndeterminate,
		ReasonRepoAccessDenied,
		ReasonImageBuildLookupFailed,
		ReasonImageBuildTrackingMarshalFailed,
		ReasonImageBuildTrackingUpsertFailed,
		ReasonImageBuildPending,
		ReasonImageBuildNotReady,
		ReasonImageBuildReadyRowMissingRef,
		ReasonUnrecognized,
	}
}
