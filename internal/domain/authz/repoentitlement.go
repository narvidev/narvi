package authz

import (
	"errors"
	"fmt"
)

// RepoAdmission is one named repository's already-resolved entitlement
// fact, exactly mirroring internal/domain/rollout.RepoAdmission's own
// "caller resolves the I/O, this package only reasons over the
// already-gathered boolean" shape (§11: no I/O, no time.Now(), no
// randomness in /internal/domain). FullName is "owner/repo" when the
// caller could resolve one (reposource.CheckRepoHost + ParseOwnerRepo
// both succeeded) -- carried here purely for RepoForbiddenError's own
// logging/audit value, never compared or parsed by this package. Known is
// FAIL-CLOSED: false for a repository this deployment has never actually
// seen real, externally-verified traffic for, a genuine read error while
// checking, OR a repo whose clone URL could not be resolved to a trusted,
// host-verified owner/repo identity at all -- the caller (internal/
// adapters/inbound/httpapi's checkRepoEntitlementGate) folds every one of
// those into Known == false BEFORE ever calling AuthorizeRepo, the same
// discipline internal/domain/rollout.RepoAdmission.Enrolled already
// established for the cohort-rollout gate.
type RepoAdmission struct {
	FullName string
	Known    bool
}

// ErrRepoForbidden is the sentinel every AuthorizeRepo rejection wraps --
// deliberately DISTINCT from ErrForbidden (Authorize's own sentinel):
// the two answer different questions (a role's own action-level
// permission vs. a repository's own entitlement), and a caller must be
// able to tell which one it received without string-matching a message
// (mirrors ErrForbidden/ErrUnknownAction's own "distinct sentinels for
// distinct failure classes" precedent immediately above).
var ErrRepoForbidden = errors.New("authz: repository forbidden")

// RepoForbiddenError is the detailed error AuthorizeRepo returns for any
// (actor, repo) it rejects. Actor is carried verbatim, mirroring
// ForbiddenError's own shape -- an audit-log row or a 403 body can use it
// to attribute the denial without a second lookup.
type RepoForbiddenError struct {
	Actor        Actor
	RepoFullName string
}

func (e *RepoForbiddenError) Error() string {
	return fmt.Sprintf("authz: repository forbidden: %q is not entitled to repository %q", e.Actor.UserID, e.RepoFullName)
}

func (e *RepoForbiddenError) Unwrap() error { return ErrRepoForbidden }

// AuthorizeRepo renders §31.4's own entitlement verdict: may actor use
// admission's own named repository at all -- nil if so, *RepoForbiddenError
// (wrapping ErrRepoForbidden) otherwise. This is deliberately the minimal
// predicate §31.4 calls for, NOT a per-repo-roles RBAC rework:
// admission.Known is the ONLY admitting fact today, so the verdict does
// not yet vary by actor.Role at all -- every role that has already
// cleared the coarser, role-only Authorize(actor, ActionCreateSession,
// Resource{}) gate (admin/maintainer/member, §13.3 row 2) receives the
// IDENTICAL repo-entitlement verdict for the same repository. actor is
// still a real, required parameter (not dropped, not folded into a bare
// bool) for two reasons named explicitly in §31.4's own text: (1) a
// caller building an audit_log row or a log line needs actor.UserID
// attributed to the denial, exactly like Authorize's own ForbiddenError
// already carries Actor for that purpose; (2) this is the seam
// knowledge.RepoScope constructors are meant to grow into later ("take
// the actor alongside the trusted artifact") -- a future Step can extend
// this SAME function to consult actor.Role or a real per-actor-per-repo
// fact without changing any EXISTING caller's own call shape, whereas
// bolting actor on afterward would touch every call site a second time.
//
// This function does no I/O of its own (§11) -- the caller (httpapi's
// checkRepoEntitlementGate) resolves admission.Known from a real
// github_pr_sessions read BEFORE ever calling this function, exactly like
// checkRolloutGate resolves rollout.RepoAdmission.Enrolled from
// repo_settings before calling rollout.Decide. See that gate's own doc
// comment for why github_pr_sessions -- not repo_settings, not
// sessions.repos -- is the entitlement source of truth this predicate is
// built on.
func AuthorizeRepo(actor Actor, admission RepoAdmission) error {
	if admission.Known {
		return nil
	}
	return &RepoForbiddenError{Actor: actor, RepoFullName: admission.FullName}
}
