// This file (repoentitlementgate.go) implements §31.4's own remaining
// deliverable (the four-handler URL-authorization fix -- reposettings.go's
// own resolveKnownRepo -- is a separate, already-shipped half of the SAME
// decision): checkRepoEntitlementGate, called from CreateSessionOnTx
// (create.go) BEFORE the environment/session inserts, on the SAME
// transaction that is about to insert the session -- closing the clone
// amplification §31.4 names: the sandbox credential helper
// (internal/sandboxagent/gitclone/clone.go) serves whatever repo list
// sessions.repos names, so an entry that was never entitled must never
// reach that column at all.
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
// GitHubPRSessionStore.RepoKnown -- rather than re-deriving a second,
// possibly-drifting notion of "known repo": github_pr_sessions' only
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
// child sessions spawned from one) are EXEMPT from this gate entirely,
// not merely coincidentally passing -- see checkRepoEntitlementGate's own
// doc comment for why re-deriving identity from req.Repos[i].Url would be
// actively WRONG there (a cross-repo/fork PR's own clone URL is
// deliberately the fork, never the github_pr_sessions claim key), not
// just redundant.
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
// denyRepoEntitlement's own doc comment for why that write must NOT run on
// tx.

package httpapi

import (
	"context"
	"net/http"
	"sync"

	"github.com/jackc/pgx/v5"
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
		metric.WithDescription("Count of every session-creation attempt refused by §31.4's per-repository entitlement predicate (authz.AuthorizeRepo) because a named repo has never been confirmed known to this deployment (github_pr_sessions, via GitHubPRSessionStore.RepoKnown) -- checkRepoEntitlementGate's own session-creation-time denials. Tagged by the \"spawn_source\" attribute. A misconfigured entitlement is loud here, never silent: a sustained nonzero rate on a repo an operator believes IS connected means it has not yet had a GitHub PR mention (see checkRepoEntitlementGate's own doc comment), not that Narvi is broken."),
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
// by spawnSource -- mirrors recordRolloutRefusal's own identical shape.
func recordRepoEntitlementDenial(ctx context.Context, spawnSource string) {
	sessionRepoEntitlementDeniedTotalCounter().Add(ctx, 1, metric.WithAttributes(attribute.String("spawn_source", spawnSource)))
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

// checkRepoEntitlementGate is §31.4's own primary gate, called from
// CreateSessionOnTx (create.go) BEFORE the environment/session inserts, on
// the SAME transaction that is about to insert the session -- see this
// file's own top doc comment for the full "why here, why github_pr_
// sessions" reasoning.
//
// UNLIKE checkRolloutGate, this gate has no mode-gated "byte-for-byte
// no-op" escape hatch: §31.4's own vulnerability (an unentitled repo
// becoming a credentialed clone) exists on every deployment, in every
// stage, regardless of whether an operator has ever touched
// NARVI_ROLLOUT_MODE -- so every named repo is resolved and checked on
// every call, unconditionally.
//
// req.SpawnSource == github is EXEMPT, unconditionally, before any repo is
// even iterated -- NOT a convenience short-circuit, a correctness
// requirement. Two reasons, both load-bearing:
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
// this gate entirely -- the exact clone amplification it exists to close.
// TestCreateSession_NonWebSpawnSource_Rejected is what holds that end;
// its own doc comment points back here.
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
//     comment: "the claim key"). Checking RepoKnown against the FORK's own
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
// Three ways a repo can fail this gate, each folded into authz.
// RepoAdmission.Known == false (fail-closed) before AuthorizeRepo ever
// runs:
//
//  1. The repo's URL cannot be resolved to a trusted, host-verified
//     owner/repo identity at all (resolveTrustedRepoFullName's own ok ==
//     false -- an unsupported host, or a URL ParseOwnerRepo cannot parse).
//     This is a DEMONSTRATED, permanent fact -- re-parsing the identical
//     URL can never produce a different answer -- so it is treated
//     IDENTICALLY to a genuinely unknown repo: denied, counted, audited.
//  2. github_pr_sessions has never seen this exact owner/repo (RepoKnown
//     returns false, no error). Also a demonstrated, permanent fact as of
//     right now (it can change the moment a real GitHub PR mention lands),
//     denied/counted/audited identically to case 1.
//  3. The github_pr_sessions read itself fails for an infrastructure
//     reason (a context cancellation, a query timeout, any other degraded-
//     read condition) -- NOT a demonstrated policy fact, so this refuses
//     THIS attempt (fail-closed never widens: the repo is never silently
//     admitted) but returns 503, not 403, and records NEITHER the denial
//     metric NOR an audit_log row -- mirroring checkRolloutGate's own
//     "fail-closed and terminal are different properties" split exactly
//     (rolloutgate.go's own doc comment): conflating an infrastructure
//     blip with a genuine, repeatable policy denial would make both the
//     metric and the audit trail lie to an operator about how many repos
//     are actually being kept out by this gate.
func checkRepoEntitlementGate(ctx context.Context, tx pgx.Tx, prSessions *postgres.GitHubPRSessionStore, auditLog *postgres.AuditLogStore, createdBy pgtype.UUID, req restdtos.CreateSessionRequest) *CreateSessionError {
	if req.SpawnSource == restdtos.CreateSessionRequestSpawnSourceGithub {
		return nil
	}

	logger := platform.Logger(ctx)

	// A nil prSessions is a caller-wiring defect (every real caller of
	// CreateSessionOnTx is REQUIRED to supply one, per this function's own
	// doc comment), never a legitimate "no repos to check" signal --
	// exactly the "actor whose entitlement cannot be determined" case this
	// Step's own brief names explicitly. Fails closed the SAME way a
	// genuine RepoKnown read error does (503, no metric, no audit row --
	// this is an infrastructure/configuration defect, not a demonstrated
	// policy denial) rather than a nil-pointer panic: a missing dependency
	// must degrade like every other degraded-read case in this codebase,
	// never crash the process that was about to create a session.
	if prSessions == nil {
		logger.Error("httpapi: repo entitlement gate: prSessions is nil; failing closed (treating as not known)",
			"spawn_source", string(req.SpawnSource))
		return &CreateSessionError{
			Status:  http.StatusServiceUnavailable,
			Message: "repository entitlement could not be verified: entitlement store unavailable",
		}
	}

	actor := actorFromCreatedBy(createdBy)

	for _, repo := range req.Repos {
		fullName, resolved := resolveTrustedRepoFullName(repo.Url)
		if !resolved {
			logger.Warn("httpapi: repo entitlement gate: repo url could not be resolved to a trusted, host-verified owner/repo identity; treating as not known",
				"url", repo.Url, "spawn_source", string(req.SpawnSource))
			return denyRepoEntitlement(ctx, auditLog, createdBy, actor, authz.RepoAdmission{FullName: repo.Url, Known: false}, req.SpawnSource)
		}

		known, err := prSessions.WithTx(tx).RepoKnown(ctx, fullName)
		if err != nil {
			// Case 3 above -- fail-closed, but NOT a demonstrated policy
			// outcome. See this function's own doc comment.
			logger.Warn("httpapi: repo entitlement gate: read github_pr_sessions failed; failing closed (treating as not known)",
				"repo", fullName, "error", err, "spawn_source", string(req.SpawnSource))
			return &CreateSessionError{
				Status:  http.StatusServiceUnavailable,
				Message: "repository entitlement could not be verified: " + fullName,
			}
		}

		if aerr := authz.AuthorizeRepo(actor, authz.RepoAdmission{FullName: fullName, Known: known}); aerr != nil {
			return denyRepoEntitlement(ctx, auditLog, createdBy, actor, authz.RepoAdmission{FullName: fullName, Known: known}, req.SpawnSource)
		}
	}

	return nil
}

// denyRepoEntitlement renders a genuine, DEMONSTRATED entitlement denial
// (cases 1/2 in checkRepoEntitlementGate's own doc comment) into the
// side effects §31.4 explicitly requires -- a Warn log, the denial
// counter, and an audit_log row -- and the *CreateSessionError every
// caller already knows how to propagate.
//
// The audit_log write deliberately does NOT run on tx: tx belongs to the
// caller that is about to return this exact error, and every
// CreateSessionOnTx caller in this codebase rolls its own transaction back
// on ANY non-nil *CreateSessionError (create.go's own CreateSessionCore,
// childsession.go's SpawnChildSession, and every direct-tx caller's own
// "defer rollback" -- see each one's own doc comment). An INSERT issued
// via auditLog.WithTx(tx) would therefore be discarded along with
// everything else the instant that rollback runs, silently losing the
// exact audit trail this Step requires. auditLog here is the plain,
// pool-backed store every CreateSessionOnTx caller already threads
// through (the SAME parameter the SUCCESS-path audit row further down in
// CreateSessionOnTx binds to tx via .WithTx(tx) -- this is the one call
// site in this package that deliberately does NOT) -- writing through it
// directly commits this row on its own, independent connection,
// regardless of what happens to tx afterward.
//
// A failure to write the audit row itself is logged and swallowed, never
// promoted to the returned error: the entitlement denial is already a
// fully-decided outcome by the time this function runs, and a best-effort
// side channel failing must never flip an already-correct 403 into a
// 500, nor -- the opposite, more dangerous mistake -- ever let the
// caller through because a logging nicety could not be written.
func denyRepoEntitlement(ctx context.Context, auditLogStore *postgres.AuditLogStore, createdBy pgtype.UUID, actor authz.Actor, admission authz.RepoAdmission, spawnSource restdtos.CreateSessionRequestSpawnSource) *CreateSessionError {
	logger := platform.Logger(ctx)
	logger.Warn("httpapi: repo entitlement gate: session creation refused, repo not entitled",
		"repo", admission.FullName, "spawn_source", string(spawnSource), "actor_user_id", actor.UserID)

	recordRepoEntitlementDenial(ctx, string(spawnSource))

	if err := auditlog.Record(ctx, auditLogStore, createdBy, "session.repo_entitlement_denied", "repo", admission.FullName, map[string]any{
		"spawn_source": string(spawnSource),
	}); err != nil {
		logger.Error("httpapi: repo entitlement gate: record denial audit log failed", "error", err, "repo", admission.FullName)
	}

	return &CreateSessionError{
		Status:                http.StatusForbidden,
		Message:               "repository not entitled: " + admission.FullName,
		RepoEntitlementDenied: true,
	}
}
