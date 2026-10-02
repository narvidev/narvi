// This file (repoentitlementgate.go) implements §31.4's own remaining
// deliverable (the four-handler URL-authorization fix -- reposettings.go's
// own resolveKnownRepo -- is a separate, already-shipped half of the SAME
// decision): ResolveRepoEntitlement, called by every CreateSessionOnTx
// caller in this codebase (create.go's own CreateSessionCore,
// childsession.go, internal/app/automation/fanout.go, internal/app/
// outboxworker/sentinelautofix.go, internal/adapters/inbound/github/
// coalesce.go) BEFORE any of them opens the transaction the eventual
// session insert runs on -- closing the clone amplification §31.4 names:
// the sandbox credential helper (internal/sandboxagent/gitclone/clone.go)
// serves whatever repo list sessions.repos names, so an entry that was
// never entitled must never reach that column at all. CreateSessionOnTx
// itself (create.go) never resolves this on its own -- it takes the
// already-made RepoEntitlementDecision as a required parameter and simply
// refuses on it -- see this file's own "Defect-1 audit fix" section below
// for why resolution and consultation are deliberately two different
// functions, run at two different times, never one.
//
// # Why github_pr_sessions is the entitlement source of truth
//
// reposettings.go's own resolveKnownRepo/confirmRepoKnown already answered
// this question, in depth, for the four settings-style handlers §31.4
// calls "already in flight, independently" -- see that file's own doc
// comment for the full three-table analysis (repo_settings disqualified
// twice over: sparse-by-design and self-referential for the very
// endpoints that would gate on it; sessions.repos disqualified as
// trivially self-serve at ordinary member privilege). This gate reuses
// the IDENTICAL conclusion and the IDENTICAL signal --
// GitHubPRSessionStore.RepoEntitlement's Known -- rather than re-deriving a
// second, possibly-drifting notion of "known repo": github_pr_sessions' only
// writer anywhere in this codebase is internal/adapters/inbound/github's
// own HMAC-verified webhook ingress (coalesce.go), so bare row existence
// is a sound, externally-verified proof this deployment is genuinely
// attached to the repository, never a fact any session-creation caller
// (of any role) could manufacture for itself by naming an arbitrary,
// never-onboarded repo. repo_settings/sessions.repos remain disqualified
// here for the exact same reasons.
//
// The honest limitation this inherits, unchanged: a freshly onboarded
// repository with zero PR mentions yet has no github_pr_sessions row, so
// no session (web, Slack, Linear, or automation) can name it until its
// first GitHub PR mention succeeds. Reported here plainly, matching
// resolveKnownRepo's own "reported here plainly, not silently worked
// around" convention, not silently special-cased.
//
// GitHub-originated sessions (req.SpawnSource == github --
// coalesce.go's own WINNER path, and outboxworker's own sentinel-auto-fix
// child sessions spawned from one) are EXEMPT from the "known" half of
// this gate, not merely coincidentally passing -- see
// ResolveRepoEntitlement's own doc comment for why re-deriving identity
// from req.Repos[i].Url would be actively WRONG there (a cross-repo/fork
// PR's own clone URL is deliberately the fork, never the
// github_pr_sessions claim key), not just redundant. They are NOT exempt
// from an administrator's revocation: see ResolveGitHubRepoEntitlement.
//
// # Un-entitlement: an administrator's revocation (§31.4)
//
// Eligibility only ever grows -- nothing removes a github_pr_sessions row.
// An administrator closes a repository instead by revoking it
// (repo_entitlement_revocations, POST /api/repos/{owner}/{repo}/
// entitlement/revoke, repoentitlement.go). Every read here takes the
// revocation in the SAME statement as eligibility
// (GitHubPRSessionStore.RepoEntitlement), and authz.AuthorizeRepo refuses a
// revoked repository before it looks at whether the repository is known.
// The refusal says so in its own words ("repository entitlement revoked by
// an administrator: ..."), with CreateSessionError.RepoEntitlementRevoked
// set beside RepoEntitlementDenied, so a caller that only checks the
// latter still treats it as permanent. A revocation that commits between
// this resolution and the caller's insert leaves a session that can never
// run: the session actor reads the revocation again before every spawn and
// every dispatch (internal/app/sessionactor, refuseIfRepoRevoked and
// revocationRefusalForDispatch), so no in-transaction re-read is needed
// here.
//
// # Why this predicate lives in domain/authz, not only here
//
// §31.4 asks for a predicate that "joins the authz path": repository
// identity entering Authorize's own vocabulary, not a second, parallel
// I/O helper. authz.AuthorizeRepo (repoentitlement.go) is that predicate
// -- a pure function taking authz.Actor alongside an already-resolved
// authz.RepoAdmission fact, mirroring internal/domain/rollout.Decide's own
// "caller resolves the I/O, domain only reasons over the already-gathered
// boolean" split (§11). This file is the I/O half: resolving each named
// repo to a trusted, host-verified identity (§32.3's own pairing, reused
// verbatim via resolveTrustedRepoFullName), reading github_pr_sessions
// (fail-closed), and turning AuthorizeRepo's verdict into a
// *CreateSessionError every CreateSessionOnTx caller already knows how to
// propagate -- exactly checkRolloutGate's own shape, one repo_settings
// read swapped for one github_pr_sessions read and one Decide swapped for
// one AuthorizeRepo.
//
// # Measurement: loud, never silent (§31.4's own explicit requirement)
//
// checkRolloutGate's own doc comment records a deliberate convention:
// "writes NO audit_log row... audit_log records completed STATE CHANGES
// only, never a refusal of any kind." This gate DIVERGES from that
// convention on purpose, per §31.4's own explicit brief: a repo-
// entitlement denial is a SECURITY-relevant signal (a plausible clone-
// amplification attempt, not merely an unfinished rollout), so it is both
// counted (session_repo_entitlement_denied_total, mirroring
// session_rollout_refused_total's own shape) AND audit-logged -- see
// denyRepoEntitlement's own doc comment for the full "how" this stays
// reliable under load.
//
// # Defect-1 audit fix: resolve the decision before any transaction exists
//
// This function used to be checkRepoEntitlementGate, taking a `tx pgx.Tx`
// parameter and called from INSIDE CreateSessionOnTx, on the SAME
// transaction the caller was already holding open. That was correct for
// the entitlement READ itself (a prSessions.WithTx(tx) read -- same
// connection, same tx, free), but wrong for the denial AUDIT WRITE:
// denyRepoEntitlement's own write always ran through the plain,
// POOL-backed auditLog store (never .WithTx(tx)) -- necessary so that row
// survives the caller's own eventual rollback of the session it just
// refused (every CreateSessionOnTx caller rolls its own tx back on any
// non-nil *CreateSessionError). A pool-backed write issued while the
// caller's OWN transaction is still open needs a SECOND connection out of
// the SAME pool. Reproduced twice against a real Postgres: with
// DBPoolMaxConns=1, a single denial blocked for the full context deadline
// (a known-repo control returned in ~2ms); with DBPoolMaxConns=4, four
// concurrent denials starved an unrelated query for its own deadline, and
// the audit write itself failed with context canceled on every single one
// of them -- so under exactly the concurrent-denial burst this gate
// exists to detect, it produced the metric but NOT the audit row,
// defeating this Step's own "loud, never silent" requirement above.
// pgxpool.Acquire has no acquire timeout of its own, and the HTTP server
// sets none either, so N >= DBPoolMaxConns concurrent denials was a
// circular wait any authenticated session-creation caller could reach.
//
// The fix is not an acquire timeout (that bounds the stall but still
// loses the audit row under load, which is the half that actually
// matters) -- it is moving the ENTIRE decision, denial side effects
// included, to run with NO transaction open at all: ResolveRepoEntitlement
// takes no tx parameter, is called by every CreateSessionOnTx caller
// BEFORE that caller's own pool.Begin/tx acquisition of any kind, and
// returns a RepoEntitlementDecision (admitted) plus, on refusal, the
// *CreateSessionError to return immediately -- often, on a denial, before
// a doomed transaction is ever even opened at all. With no tx in scope,
// the audit write needs only the ONE connection it always needed, and
// nothing can roll it back -- denyRepoEntitlement's own "why not
// .WithTx(tx)" framing no longer even applies, since there is no tx here
// to have used in the first place. CreateSessionOnTx itself now takes the
// resolved RepoEntitlementDecision as a required parameter and only ever
// consults it -- see RepoEntitlementDecision's own doc comment for why it
// is impossible, not just discouraged, for a caller to fabricate an
// admitting one without actually calling this resolver.

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/platform"
)

// repoEntitlementGateMeterName is this package's own OTel meter name for
// the entitlement-denial counter, mirroring rolloutGateMeterName's own
// "narvi/httpapi-<concern>" precedent exactly.
const repoEntitlementGateMeterName = "narvi/httpapi-repoentitlementgate"

// sessionRepoEntitlementDeniedTotalCounter is resolved LAZILY
// (sync.OnceValue), mirroring sessionRolloutRefusedTotalCounter's own doc
// comment exactly: CreateSessionOnTx is a free function with no
// per-process constructor object to anchor eager construction to.
var sessionRepoEntitlementDeniedTotalCounter = sync.OnceValue(newSessionRepoEntitlementDeniedTotalCounter)

func newSessionRepoEntitlementDeniedTotalCounter() metric.Int64Counter {
	c, err := otel.Meter(repoEntitlementGateMeterName).Int64Counter(
		"session_repo_entitlement_denied_total",
		metric.WithDescription("Count of every session-creation attempt refused by §31.4's per-repository entitlement predicate (authz.AuthorizeRepo) -- ResolveRepoEntitlement's and ResolveGitHubRepoEntitlement's own session-creation-time denials -- and of every spawn or turn dispatch the session actor refuses because an administrator revoked the session's repository (internal/app/sessionactor registers the SAME instrument). Tagged by \"spawn_source\" and \"reason\": \"unknown\" for a named repo never confirmed known to this deployment (github_pr_sessions), \"revoked\" for one an administrator revoked (repo_entitlement_revocations). A misconfigured entitlement is loud here, never silent: a sustained nonzero \"unknown\" rate on a repo an operator believes IS connected means it has not yet had a GitHub PR mention (see ResolveRepoEntitlement's own doc comment), not that Narvi is broken; a \"revoked\" rate is work still arriving for a repository an administrator closed."),
		metric.WithUnit("{denial}"),
	)
	if err != nil {
		// Structurally cannot fail for a fixed, well-formed instrument
		// name -- logged defensively anyway, mirroring
		// newSessionRolloutRefusedTotalCounter's own identical precedent.
		platform.Logger(context.Background()).Error("httpapi: construct session_repo_entitlement_denied_total counter failed", "error", err)
	}
	return c
}

// recordRepoEntitlementDenial increments the denial counter by one, tagged
// by spawnSource and reason (repoEntitlementDenialReason) -- mirrors
// recordRolloutRefusal's own identical shape.
func recordRepoEntitlementDenial(ctx context.Context, spawnSource, reason string) {
	sessionRepoEntitlementDeniedTotalCounter().Add(ctx, 1, metric.WithAttributes(
		attribute.String("spawn_source", spawnSource),
		attribute.String("reason", reason),
	))
}

// The two values of a denial's "reason" -- the counter attribute and the
// session.repo_entitlement_denied audit row's detail.
const (
	repoEntitlementDenialRevoked = "revoked"
	repoEntitlementDenialUnknown = "unknown"
)

// repoEntitlementDenialReason names the refusal authz.AuthorizeRepo
// returned: "revoked" when an administrator revoked the repository
// (authz.ErrRepoRevoked), "unknown" otherwise.
func repoEntitlementDenialReason(aerr error) string {
	if errors.Is(aerr, authz.ErrRepoRevoked) {
		return repoEntitlementDenialRevoked
	}
	return repoEntitlementDenialUnknown
}

// actorFromCreatedBy builds the authz.Actor AuthorizeRepo/RepoForbiddenError
// need from createdBy -- UserID left "" (the zero value) for an invalid
// (bot/webhook-originated) createdBy, exactly mirroring sessions.
// created_by/audit_log.actor_user_id's own established NULL-for-bot
// convention, never a fabricated system-user id.
func actorFromCreatedBy(createdBy pgtype.UUID) authz.Actor {
	if !createdBy.Valid {
		return authz.Actor{}
	}
	return authz.Actor{UserID: createdBy.String()}
}

// RepoEntitlementDecision is §31.4's own entitlement verdict for one
// session-creation request, ALREADY RESOLVED by ResolveRepoEntitlement
// before any transaction was opened -- see this file's own top doc
// comment ("Defect-1 audit fix") for the full "why". CreateSessionOnTx
// (create.go) takes this as a required parameter and refuses on it
// directly, never re-deriving or re-resolving it itself: re-resolving on
// the tx CreateSessionOnTx is handed would reintroduce the exact defect
// this type exists to close (a genuine Postgres read/write racing the
// caller's own already-open transaction for a second pool connection).
//
// The zero value, RepoEntitlementDecision{}, is DENIED: admitted defaults
// false, and is unexported -- settable only from within this file, by
// ResolveRepoEntitlement's own successful return. This mirrors
// RequireCapability's own "inject the decision, not the decider" shape
// (requirecapability.go's own doc comment) one level stricter: that
// function takes a bare func() bool a caller could still fake by closing
// over one that always answers true, where this type's unexported field
// means the ONLY way any other package can ever construct an admitting
// value is to call ResolveRepoEntitlement and receive one back. A caller
// that forgets to call it -- and passes this type's own zero value
// through some other path -- fails CLOSED, forced through the resolver at
// compile time to get anything else.
type RepoEntitlementDecision struct {
	admitted bool
}

// ResolveRepoEntitlement is §31.4's own resolver -- the I/O half of the
// "inject the decision, not the decider" split RepoEntitlementDecision's
// own doc comment describes, and this file's own top doc comment's
// "Defect-1 audit fix" section. Every CreateSessionOnTx caller in this
// codebase (create.go's own CreateSessionCore, childsession.go,
// automation/fanout.go, outboxworker/sentinelautofix.go, github/
// coalesce.go) calls this FIRST, with NO transaction open (strictly
// before its own pool.Begin/tx acquisition of any kind), and either bails
// out immediately on a non-nil *CreateSessionError or threads the
// returned RepoEntitlementDecision through to CreateSessionOnTx.
//
// UNLIKE checkRolloutGate, this resolver has no mode-gated "byte-for-byte
// no-op" escape hatch: §31.4's own vulnerability (an unentitled repo
// becoming a credentialed clone) exists on every deployment, in every
// stage, regardless of whether an operator has ever touched
// NARVI_ROLLOUT_MODE -- so every named repo is resolved and checked on
// every call, unconditionally.
//
// req.SpawnSource == github is EXEMPT from the "known" half of this
// predicate, before any repo is even iterated -- NOT a convenience
// short-circuit, a correctness requirement -- and is checked against an
// administrator's revocation alone (resolveGitHubRepoEntitlement, with no
// pull-request claim: the server-side GitHub callers name theirs through
// ResolveGitHubRepoEntitlement). Two reasons for the exemption, both
// load-bearing:
//
// What makes the exemption SAFE is a guard in another file, and the
// dependency is worth naming because nothing here can observe it: the
// session-creation endpoint rejects any request body claiming a
// spawnSource other than "web" with a 400, before any write. So on this
// gate's own reasoning, "github" is never a caller's assertion -- it is
// only ever a value a server-side ingress path (the webhook handler,
// coalesce's winner, the sentinel auto-fix child) set for itself from a
// verified payload. Were that rejection ever relaxed, an authenticated
// caller could name any repository, claim github provenance, and skip
// this gate's "known" check entirely -- the exact clone amplification it
// exists to close. TestCreateSession_NonWebSpawnSource_Rejected is what
// holds that end; its own doc comment points back here.
//
//  1. Trust: §31.4's own words are that "sessions.repos is lower-trust
//     than github_pr_sessions.repo_full_name (verified webhook payload)" --
//     this predicate exists SPECIFICALLY to compensate for sessions.repos
//     being self-serve, actor-chosen input on every OTHER spawn source.
//     For spawnSource == github, sessions.repos is never actor-chosen at
//     all: it is populated server-side, either directly from a real,
//     HMAC-verified GitHub webhook payload (internal/adapters/inbound/
//     github's own coalesce.go, the WINNER path) or from a
//     ports.SentinelAutoFixPayload the control plane itself constructed
//     from an already-verified origin session (outboxworker's own
//     sentinelAutoFixNotifier) -- never from a human's free-text request
//     body. There is no actor-supplied choice here for this predicate to
//     police.
//  2. Correctness: this predicate resolves identity from req.Repos[i].Url
//     via reposource.ParseOwnerRepo -- but for a CROSS-REPO (fork-based)
//     PR, that URL is deliberately the PR's own HEAD repo (payload.go's
//     own mention.RepoCloneURL doc comment: "head repo -- may be a fork;
//     the repo to actually clone"), while github_pr_sessions is keyed on
//     the PR's BASE/upstream repo instead (mention.RepoFullName's own doc
//     comment: "the claim key"). Checking Known against the FORK's own
//     owner/repo would find no row (a fork essentially never independently
//     accumulates its own github_pr_sessions history) and wrongly deny
//     EVERY fork-based PR review and every sentinel-auto-fix spawned from
//     one -- forever, not merely until some onboarding step, since a
//     one-off contributor fork has no realistic path to ever becoming
//     "known" on its own. This is not a hypothetical: it is exactly the
//     shape of defect this Step's own brief warns fail-direction mistakes
//     produce, caught here before it shipped rather than after.
//
// Every repo in req.Repos (for every OTHER spawn source) is checked, in
// order, stopping at the FIRST failure -- mirroring
// validateCreateSessionRequest/rollout.Decide's own identical "report the
// first failure, never attempt to collect every one at once" precedent.
// Four ways a repo can fail this gate:
//
//  1. The repo's URL cannot be resolved to a trusted, host-verified
//     owner/repo identity at all (resolveTrustedRepoFullName's own ok ==
//     false -- an unsupported host, or a URL ParseOwnerRepo cannot parse).
//     This is a DEMONSTRATED, permanent fact -- re-parsing the identical
//     URL can never produce a different answer -- so it is treated
//     IDENTICALLY to a genuinely unknown repo: denied, counted, audited.
//  2. An administrator revoked the repo (RepoEntitlement's Revoked):
//     denied, counted and audited with reason "revoked", whatever Known
//     holds -- authz.AuthorizeRepo checks it first.
//  3. github_pr_sessions has never seen this exact owner/repo
//     (RepoEntitlement's Known is false). Also a demonstrated, permanent
//     fact as of right now (it can change the moment a real GitHub PR
//     mention lands), denied/counted/audited identically to case 1, with
//     reason "unknown".
//  4. The RepoEntitlement read itself fails for an infrastructure reason
//     (a context cancellation, a query timeout, any other degraded-read
//     condition) -- NOT a demonstrated policy fact, so this refuses THIS
//     attempt (fail-closed never widens: the repo is never silently
//     admitted) but returns 503, not 403, and records NEITHER the denial
//     metric NOR an audit_log row -- mirroring checkRolloutGate's own
//     "fail-closed and terminal are different properties" split exactly
//     (rolloutgate.go's own doc comment): conflating an infrastructure
//     blip with a genuine, repeatable policy denial would make both the
//     metric and the audit trail lie to an operator about how many repos
//     are actually being kept out by this gate.
func ResolveRepoEntitlement(ctx context.Context, prSessions *postgres.GitHubPRSessionStore, auditLog *postgres.AuditLogStore, createdBy pgtype.UUID, req restdtos.CreateSessionRequest) (RepoEntitlementDecision, *CreateSessionError) {
	return resolveRepoEntitlement(ctx, prSessions, auditLog, createdBy, req, string(req.SpawnSource))
}

// resolveRepoEntitlement is ResolveRepoEntitlement with the source its
// Warn lines, denial counter and denial audit row are labelled with chosen
// by the caller (§43.1): CreateSessionCore passes the source the session
// would record, which is mcp for a create bridged from an MCP tool even
// though its body says web. The label decides nothing: the github branch
// still reads req.SpawnSource, so a create over MCP passes exactly the gate
// a web create does.
func resolveRepoEntitlement(ctx context.Context, prSessions *postgres.GitHubPRSessionStore, auditLog *postgres.AuditLogStore, createdBy pgtype.UUID, req restdtos.CreateSessionRequest, spawnSource string) (RepoEntitlementDecision, *CreateSessionError) {
	logger := platform.Logger(ctx)

	// A nil prSessions is a caller-wiring defect (every real caller of
	// this resolver is REQUIRED to supply one, per this function's own
	// doc comment), never a legitimate "no repos to check" signal --
	// exactly the "actor whose entitlement cannot be determined" case this
	// Step's own brief names explicitly. Fails closed the SAME way a
	// genuine RepoEntitlement read error does (503, no metric, no audit
	// row -- this is an infrastructure/configuration defect, not a
	// demonstrated policy denial) rather than a nil-pointer panic: a
	// missing dependency must degrade like every other degraded-read case
	// in this codebase, never crash the process that was about to create a
	// session. Checked before the github branch too: that branch reads
	// revocations.
	if prSessions == nil {
		logger.Error("httpapi: repo entitlement gate: prSessions is nil; failing closed (treating as not known)",
			"spawn_source", spawnSource)
		return RepoEntitlementDecision{}, &CreateSessionError{
			Status:  http.StatusServiceUnavailable,
			Message: "repository entitlement could not be verified: entitlement store unavailable",
		}
	}

	if req.SpawnSource == restdtos.CreateSessionRequestSpawnSourceGithub {
		return resolveGitHubRepoEntitlement(ctx, prSessions, auditLog, createdBy, req, "", spawnSource)
	}

	actor := actorFromCreatedBy(createdBy)

	for _, repo := range req.Repos {
		fullName, resolved := resolveTrustedRepoFullName(repo.Url)
		if !resolved {
			logger.Warn("httpapi: repo entitlement gate: repo url could not be resolved to a trusted, host-verified owner/repo identity; treating as not known",
				"url", repo.Url, "spawn_source", spawnSource)
			// A URL that names no repository is neither known nor
			// revocable: case 1 above, refused as unknown.
			unresolved := authz.RepoAdmission{FullName: repo.Url, Known: false, Revoked: false}
			return RepoEntitlementDecision{}, denyRepoEntitlement(ctx, auditLog, createdBy, actor, unresolved.FullName, authz.AuthorizeRepo(actor, unresolved), spawnSource)
		}

		// Plain, pool-backed read -- deliberately not .WithTx(tx): no
		// transaction is open at any point during this resolver's own
		// execution (this file's own top doc comment, "Defect-1 audit
		// fix"), so there is no tx left to scope this query to. One
		// statement reads both facts (§31.4), so Known is never read
		// without Revoked.
		facts, err := prSessions.RepoEntitlement(ctx, fullName)
		if err != nil {
			// Case 4 above -- fail-closed, but NOT a demonstrated policy
			// outcome. See this function's own doc comment.
			logger.Warn("httpapi: repo entitlement gate: read repository entitlement failed; failing closed (treating as not known)",
				"repo", fullName, "error", err, "spawn_source", spawnSource)
			return RepoEntitlementDecision{}, &CreateSessionError{
				Status:  http.StatusServiceUnavailable,
				Message: "repository entitlement could not be verified: " + fullName,
			}
		}

		if aerr := authz.AuthorizeRepo(actor, authz.RepoAdmission{FullName: fullName, Known: facts.Known, Revoked: facts.Revoked}); aerr != nil {
			return RepoEntitlementDecision{}, denyRepoEntitlement(ctx, auditLog, createdBy, actor, fullName, aerr, spawnSource)
		}
	}

	return RepoEntitlementDecision{admitted: true}, nil
}

// ResolveGitHubRepoEntitlement is ResolveRepoEntitlement for a session a
// GitHub-originated server-side path creates -- coalesce.go's mention
// (both its WINNER and REUSE branches stop on a refusal here) and the
// sentinel auto-fix child (outboxworker) -- whose request carries
// spawnSource github. claimRepoFullName is the pull request's BASE
// repository, the github_pr_sessions claim key the verified payload named:
// for a fork pull request req.Repos names the fork, which an
// administrator's revocation of the base repository would never match.
//
// Only revocation applies to this source (§31.4). The verified payload is
// the admission, so the claim and every repository req.Repos names are
// admitted unless an administrator revoked one of them: the claim is read
// first, then each URL that resolves to a trusted owner/repo (a URL that
// resolves to none can match no revocation). A revoked one is denied,
// counted and audited with reason "revoked", exactly as for every other
// source. A read error fails closed with 503, like ResolveRepoEntitlement's
// own.
//
// An empty claimRepoFullName fails closed with 503: every caller of this
// function has one, so an empty one is a wiring defect, never "nothing to
// check". A request whose spawnSource is not github never takes the GitHub
// exemption from here: it is resolved by ResolveRepoEntitlement's own full
// predicate instead, which never admits more.
func ResolveGitHubRepoEntitlement(ctx context.Context, prSessions *postgres.GitHubPRSessionStore, auditLog *postgres.AuditLogStore, createdBy pgtype.UUID, req restdtos.CreateSessionRequest, claimRepoFullName string) (RepoEntitlementDecision, *CreateSessionError) {
	spawnSource := string(req.SpawnSource)
	if req.SpawnSource != restdtos.CreateSessionRequestSpawnSourceGithub {
		return resolveRepoEntitlement(ctx, prSessions, auditLog, createdBy, req, spawnSource)
	}
	if claimRepoFullName == "" {
		platform.Logger(ctx).Error("httpapi: repo entitlement gate: GitHub-originated request names no pull-request repository; failing closed",
			"spawn_source", spawnSource)
		return RepoEntitlementDecision{}, &CreateSessionError{
			Status:  http.StatusServiceUnavailable,
			Message: "repository entitlement could not be verified: no pull-request repository named",
		}
	}
	return resolveGitHubRepoEntitlement(ctx, prSessions, auditLog, createdBy, req, claimRepoFullName, spawnSource)
}

// resolveGitHubRepoEntitlement is the GitHub-source branch both
// ResolveGitHubRepoEntitlement (with the pull request's claim) and
// ResolveRepoEntitlement (a github request with no claim, e.g.
// CreateSessionForBot) take -- see ResolveGitHubRepoEntitlement's doc
// comment.
func resolveGitHubRepoEntitlement(ctx context.Context, prSessions *postgres.GitHubPRSessionStore, auditLog *postgres.AuditLogStore, createdBy pgtype.UUID, req restdtos.CreateSessionRequest, claimRepoFullName, spawnSource string) (RepoEntitlementDecision, *CreateSessionError) {
	logger := platform.Logger(ctx)

	if prSessions == nil {
		logger.Error("httpapi: repo entitlement gate: prSessions is nil; failing closed",
			"spawn_source", spawnSource)
		return RepoEntitlementDecision{}, &CreateSessionError{
			Status:  http.StatusServiceUnavailable,
			Message: "repository entitlement could not be verified: entitlement store unavailable",
		}
	}

	actor := actorFromCreatedBy(createdBy)

	names := make([]string, 0, len(req.Repos)+1)
	if claimRepoFullName != "" {
		names = append(names, claimRepoFullName)
	}
	for _, repo := range req.Repos {
		fullName, resolved := resolveTrustedRepoFullName(repo.Url)
		if !resolved || fullName == claimRepoFullName {
			continue
		}
		names = append(names, fullName)
	}

	for _, fullName := range names {
		facts, err := prSessions.RepoEntitlement(ctx, fullName)
		if err != nil {
			logger.Warn("httpapi: repo entitlement gate: read repository entitlement failed; failing closed",
				"repo", fullName, "error", err, "spawn_source", spawnSource)
			return RepoEntitlementDecision{}, &CreateSessionError{
				Status:  http.StatusServiceUnavailable,
				Message: "repository entitlement could not be verified: " + fullName,
			}
		}
		// Known: true -- for this source the verified webhook payload is
		// the admission (ResolveRepoEntitlement's doc comment, "Trust" and
		// "Correctness"); only an administrator's revocation refuses.
		if aerr := authz.AuthorizeRepo(actor, authz.RepoAdmission{FullName: fullName, Known: true, Revoked: facts.Revoked}); aerr != nil {
			return RepoEntitlementDecision{}, denyRepoEntitlement(ctx, auditLog, createdBy, actor, fullName, aerr, spawnSource)
		}
	}

	return RepoEntitlementDecision{admitted: true}, nil
}

// denyRepoEntitlement renders a genuine, DEMONSTRATED entitlement denial
// (cases 1-3 in ResolveRepoEntitlement's own doc comment) into the side
// effects §31.4 explicitly requires -- a Warn log, the denial counter, and
// an audit_log row, each naming the reason (repoEntitlementDenialReason of
// aerr, authz.AuthorizeRepo's refusal) -- and the *CreateSessionError every
// caller already knows how to propagate. A revoked repository answers
// "repository entitlement revoked by an administrator: <repo>" with
// RepoEntitlementRevoked set beside RepoEntitlementDenied; any other,
// "repository not entitled: <repo>".
//
// The audit_log write deliberately does NOT run on any transaction: this
// function runs entirely from ResolveRepoEntitlement, called by every
// CreateSessionOnTx caller BEFORE that caller ever opens the transaction
// the eventual session insert (and its own success-path audit row) will
// run on -- see this file's own top doc comment ("Defect-1 audit fix") for
// the full "why". There is no tx in scope here to bind this write to, or
// to have it discarded by: auditLog is the plain, pool-backed store every
// ResolveRepoEntitlement caller already threads through, and writing
// through it directly commits this row on its own, ordinary connection,
// independently of whatever transaction the caller goes on to open (or,
// on this denial, never opens at all) afterward.
//
// A failure to write the audit row itself is logged and swallowed, never
// promoted to the returned error: the entitlement denial is already a
// fully-decided outcome by the time this function runs, and a best-effort
// side channel failing must never flip an already-correct 403 into a
// 500, nor -- the opposite, more dangerous mistake -- ever let the
// caller through because a logging nicety could not be written.
func denyRepoEntitlement(ctx context.Context, auditLogStore *postgres.AuditLogStore, createdBy pgtype.UUID, actor authz.Actor, repoFullName string, aerr error, spawnSource string) *CreateSessionError {
	logger := platform.Logger(ctx)
	reason := repoEntitlementDenialReason(aerr)
	logger.Warn("httpapi: repo entitlement gate: session creation refused, repo not entitled",
		"repo", repoFullName, "reason", reason, "spawn_source", spawnSource, "actor_user_id", actor.UserID)

	recordRepoEntitlementDenial(ctx, spawnSource, reason)

	if err := auditlog.Record(ctx, auditLogStore, createdBy, "session.repo_entitlement_denied", "repo", repoFullName, map[string]any{
		"spawn_source": spawnSource,
		"reason":       reason,
	}); err != nil {
		logger.Error("httpapi: repo entitlement gate: record denial audit log failed", "error", err, "repo", repoFullName)
	}

	if reason == repoEntitlementDenialRevoked {
		return &CreateSessionError{
			Status:                 http.StatusForbidden,
			Message:                "repository entitlement revoked by an administrator: " + repoFullName,
			RepoEntitlementDenied:  true,
			RepoEntitlementRevoked: true,
		}
	}
	return &CreateSessionError{
		Status:                http.StatusForbidden,
		Message:               "repository not entitled: " + repoFullName,
		RepoEntitlementDenied: true,
	}
}
