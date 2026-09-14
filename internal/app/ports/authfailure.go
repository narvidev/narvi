package ports

import "errors"

// ErrAuthenticationFailed is the port-level, adapter-agnostic signal that
// a SourceControl method's own underlying call was rejected because the
// CREDENTIAL ITSELF is invalid -- revoked, expired, malformed, or empty
// -- never a rate limit, and never a permission scoped to one repository
// (see ErrPermissionDenied below for that, different, case). GitHub's
// real, documented behavior reserves HTTP 401 ("Bad credentials")
// exclusively for this condition; unlike 403, GitHub never returns 401
// for its own rate-limit/abuse-detection mechanism (see
// internal/adapters/outbound/githubapi.isRateLimitedResponse's own doc
// comment for the 403 case this sentinel deliberately does not cover),
// so a 401 is unambiguous by construction, not a heuristic guess.
//
// docs/TECHNICAL_PLAN.md §17's own automerge dead-letter fix is this
// sentinel's first real consumer: internal/app/automerge.Worker signs
// EVERY outbound call it makes, against EVERY repo it ever touches, with
// the SAME single, statically-configured deployment-wide bot credential
// (platform.Config.GitHubBotToken) -- so a 401 against any one repo is,
// by construction, a worker-wide condition, never scoped to just that
// repo. See internal/app/automerge's own authguard.go for where that
// scope decision is actually made and acted on.
//
// Any SourceControl adapter method may surface this by having its own
// returned error's Unwrap chain resolve to this exact value (never a
// second, differently-shaped sentinel per adapter or per method) -- see
// MergePRError.Unwrap (this package) and githubapi.APIError.Unwrap for
// this port's one real adapter's own two call shapes that already do.
var ErrAuthenticationFailed = errors.New("sourcecontrol: authentication failed -- the credential itself was rejected")

// ErrPermissionDenied is ErrAuthenticationFailed's own per-repository
// sibling: a real, definitive "this credential cannot act on this
// SPECIFIC repository" denial (HTTP 403) that the adapter has already
// confirmed does NOT signal rate limiting or abuse detection (see
// githubapi.isRateLimitedResponse). A repository can deny one credential
// while every OTHER repository that same credential is used against
// keeps working fine (e.g. the bot account's own collaborator access to
// just this one repository was removed, independent of the credential's
// own overall validity) -- the opposite scope from ErrAuthenticationFailed
// above, which can only ever mean the credential itself is broken
// everywhere it is used.
//
// A rate-limited 403 must NEVER unwrap to this value. GitHub's real API
// returns 403 for both a genuine permission denial and a rate-limited/
// abuse-flagged request, and treating one as the other in either
// direction is a genuine defect: a live rate limit reported as a
// permanent denial would stop a repository's own merges that should
// resume on their own once the limit resets, while a genuine denial
// reported merely as a rate limit would reproduce the exact "keeps
// hammering forever" bug this sentinel and ErrAuthenticationFailed above
// exist to close (docs/TECHNICAL_PLAN.md §17).
var ErrPermissionDenied = errors.New("sourcecontrol: permission denied for this repository")
