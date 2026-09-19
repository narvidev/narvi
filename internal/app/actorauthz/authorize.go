package actorauthz

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/authz"
)

// AuthorizeResolvedActor closes the gap a confirmed security review found
// in §13.2's own auto-linking wiring (Slack/Linear): resolving an inbound
// event to a REAL, linked user_id/role is not enough by itself -- something
// must ALSO run that resolved actor's role back through domain/authz.
// Authorize before the caller's own state-changing effect, or a `viewer`
// (or a `member` with no ownership/participation in the target session)
// could act via a channel even though the identical action through the
// REST API (which DOES call authorize()/canActOnPlan) would be rejected.
// This directly implements docs/TECHNICAL_PLAN.md §13.3's own "a channel
// approval passes exactly the same check as a web one" requirement.
//
// surface is a short, caller-supplied label ("slack", "linear", "github")
// used only to prefix this function's own log lines -- so a log reader can
// still tell which ingress adapter produced them, exactly as if each
// package still had its own private copy of this function.
//
// actorUserID.Valid == false (still bot-attributed -- the identity has not
// resolved to a real user at all) always returns allowed=true immediately,
// with NO lookup/Authorize call at all: every caller's own "unlinked actors
// get bot attribution, the action proceeds" precedent for the not-yet-known
// case is preserved -- the thing this function exists to gate is
// specifically a RESOLVED, known identity's role.
//
// A role lookup failure (should be unreachable in practice: actorUserID was
// just resolved from identities.user_id, itself FK'd to users.id) or any
// unexpected Authorize error (ErrUnknownAction -- a caller bug, never a
// legitimate "no" verdict) fails CLOSED -- logged loudly, allowed=false --
// never silently treated as "proceed".
//
// user.Disabled is checked BEFORE ever calling domain/authz.Authorize: a
// disabled account's role would otherwise still pass Authorize (Disabled
// and Role are independent columns, migrations/000002_users.up.sql),
// letting a disabled user act via a channel even though auth.Middleware's
// own Authenticate already rejects that SAME disabled user's web session
// outright (internal/adapters/inbound/auth/middleware.go). Mirrors that
// check exactly -- denies immediately, never falls through to a role-based
// verdict for a disabled user.
func AuthorizeResolvedActor(ctx context.Context, logger *slog.Logger, surface string, users *postgres.UserStore, actorUserID pgtype.UUID, action authz.Action, resource authz.Resource) bool {
	return AuthorizeResolvedActorVerdict(ctx, logger, surface, users, actorUserID, action, resource) == LinkedActorAllowed
}

// LinkedActorVerdict is AuthorizeResolvedActorVerdict/AuthorizeLinkedActorVerdict's
// own richer return type -- W9 audit fix (confirmed LOW finding: "an error
// is recorded as a verdict"). AuthorizeResolvedActor/AuthorizeLinkedActor's
// plain bool return collapses two DIFFERENT conditions into the same
// `false`: a genuine, deliberate denial (the linked account is disabled, or
// domain/authz.Authorize returned ErrForbidden -- a real "no" this
// deployment's own RBAC computed) and a TRANSIENT failure (users.GetByID's
// own Postgres round trip erroring, or an authz.Authorize error that is
// NOT ErrForbidden, i.e. a caller bug like ErrUnknownAction) that says
// nothing at all about whether the actor is actually authorized. Both still
// fail closed identically at the bool call sites (neither should proceed
// the action either way) -- but a caller that PERSISTS this verdict as
// product-visible state (dispatchOneGitHubAutomation's own
// creator_unauthorized_since mark, githubdispatch.go) must not conflate
// them: recording "unauthorized" against a creator whose account is
// perfectly fine, merely because a Postgres blip made ONE lookup fail, is
// exactly the D8/U1 "an infrastructure hiccup is misread as a genuine,
// persisted verdict" shape this codebase has already fixed once for the
// dispatch-throttle gate (dispatchGateVerdict, githubdispatch.go) -- this
// type is the identical shape, reused rather than reinvented, for this
// SECOND site that needed it.
type LinkedActorVerdict int

const (
	// LinkedActorAllowed means the actor is a known, linked, non-disabled
	// account holding the requested action.
	LinkedActorAllowed LinkedActorVerdict = iota
	// LinkedActorDenied means authorization was evaluated to completion and
	// genuinely said no -- a disabled account, or domain/authz.Authorize's
	// own ErrForbidden. A caller may safely treat this as a durable,
	// product-visible verdict.
	LinkedActorDenied
	// LinkedActorError means authorization could NOT be evaluated at all --
	// a Postgres lookup failed, or domain/authz.Authorize returned
	// something other than ErrForbidden (a caller bug, never a legitimate
	// "no"). Fails closed exactly like LinkedActorDenied at every existing
	// bool call site, but a caller persisting this as state must not: an
	// error is not a verdict.
	LinkedActorError
)

// AuthorizeResolvedActorVerdict is AuthorizeResolvedActor's own verdict-
// returning form -- see LinkedActorVerdict's own doc comment for why this
// exists. AuthorizeResolvedActor itself is now a thin wrapper (immediately
// above) so every existing bool call site is completely unchanged.
func AuthorizeResolvedActorVerdict(ctx context.Context, logger *slog.Logger, surface string, users *postgres.UserStore, actorUserID pgtype.UUID, action authz.Action, resource authz.Resource) LinkedActorVerdict {
	if !actorUserID.Valid {
		return LinkedActorAllowed
	}

	user, err := users.GetByID(ctx, actorUserID)
	if err != nil {
		logger.Error(surface+": authz: look up resolved actor's role failed", "error", err, "user_id", actorUserID.String(), "action", string(action))
		return LinkedActorError
	}

	if user.Disabled {
		logger.Warn(surface+": authz: resolved actor's linked account is disabled, denying", "user_id", actorUserID.String(), "action", string(action))
		return LinkedActorDenied
	}

	actor := authz.Actor{UserID: actorUserID.String(), Role: authz.Role(user.Role)}
	if err := authz.Authorize(actor, action, resource); err != nil {
		if !errors.Is(err, authz.ErrForbidden) {
			logger.Error(surface+": authz.Authorize failed", "error", err, "action", string(action))
			return LinkedActorError
		}
		return LinkedActorDenied
	}
	return LinkedActorAllowed
}

// AuthorizeLinkedActor is the audit-hardening counterpart to
// AuthorizeResolvedActor above, for an inbound actor that has NOT (yet)
// resolved to a real Narvi user_id at all (actorUserID.Valid == false):
// DENIED outright, rather than allowed to proceed under bot attribution --
// the audit finding this function exists to close (docs/TECHNICAL_PLAN.md
// §13.2's own previous "the action proceeds, a magic-link prompt is sent
// in parallel" behavior was a user-decided hardening target, not a
// "keep as-is": letting a not-yet-linked identity's state-changing action
// through at all, even under bot attribution, is no longer acceptable).
// Once actorUserID IS Valid, this delegates to AuthorizeResolvedActor
// unchanged, so an already-linked actor's role/disabled/ownership verdict
// is identical either way.
//
// Originally (batch fix/audit-github-actor-rbac) this was Slack/Linear-
// only: GitHub's own callers (github/coalesce.go) kept calling
// AuthorizeResolvedActor, because -- unlike Slack/Linear's own auto-link
// algorithm (internal/app/identitylink), which treats an unresolved
// identity as "not yet linked, but a magic link is on its way", a
// transient, self-resolving state the actor can clear themselves by
// clicking the link and retrying -- GitHub's own commenter-identity
// resolution (github/identity.go) resolves directly from an existing
// GitHub-OAuth-login identity with no deferred "auto-link pending"
// mechanism at all: an unresolved GitHub commenter has simply never
// logged into Narvi via GitHub OAuth, a different and more permanent case
// with no pending link to wait for. That was read, at the time, as a
// reason GitHub could not be denied the SAME way -- there being nothing
// for the actor to do to self-resolve the pending state.
//
// Batch fix/deny-unlinked-github-actors reverses that call: the repo
// owner decided the asymmetry itself (unlinked-by-default beats
// linked-but-restricted, GitHub-only -- a confirmed, MEDIUM-severity
// authorization gap) outweighs the UX cost of "no self-resolving pending
// state to retry", and denies GitHub's unlinked actors here too, exactly
// like Slack/Linear. github/coalesce.go now calls THIS function
// (AuthorizeLinkedActor), not AuthorizeResolvedActor, on both its own
// gates (WINNER-path create-session, REUSE-path prompt-session) --
// GitHub's structural difference from Slack/Linear (no magic-link/pending-
// link mechanism) still holds exactly as described above, it just no
// longer justifies a different ALLOW/DENY verdict; instead, GitHub's own
// ingress (internal/adapters/inbound/github/actornotauthorizedreply.go)
// compensates for the missing "retry once linked" affordance with a
// one-time, honest reply pointing the commenter at the ordinary GitHub
// OAuth sign-in flow -- see that file's own doc comment.
//
// Do NOT change AuthorizeResolvedActor's own actorUserID.Valid == false
// short-circuit to match this one: it is shared with Slack's/Linear's own
// legitimate callers (via authorizeSessionAction, which already calls
// AuthorizeLinkedActor -- not AuthorizeResolvedActor -- for their own
// state-changing gates) and other, unrelated call sites that still rely
// on today's ALLOW default for a genuinely different reason. Changing
// AuthorizeResolvedActor's own general contract to fix THIS GitHub-
// specific gap would risk regressing behavior this batch never touched or
// re-verified.
func AuthorizeLinkedActor(ctx context.Context, logger *slog.Logger, surface string, users *postgres.UserStore, actorUserID pgtype.UUID, action authz.Action, resource authz.Resource) bool {
	return AuthorizeLinkedActorVerdict(ctx, logger, surface, users, actorUserID, action, resource) == LinkedActorAllowed
}

// AuthorizeLinkedActorVerdict is AuthorizeLinkedActor's own verdict-
// returning form -- see LinkedActorVerdict's own doc comment for why this
// exists. AuthorizeLinkedActor itself is now a thin wrapper (immediately
// above) so every existing bool call site is completely unchanged. The
// actorUserID.Valid == false case ("not yet linked at all") is
// LinkedActorDenied, never LinkedActorError: it is not a lookup failure,
// it is the exact, deliberate, structural denial this function exists to
// return (see this function's own top-level doc comment) -- a caller
// persisting THIS verdict is recording a true fact, not an infrastructure
// hiccup.
func AuthorizeLinkedActorVerdict(ctx context.Context, logger *slog.Logger, surface string, users *postgres.UserStore, actorUserID pgtype.UUID, action authz.Action, resource authz.Resource) LinkedActorVerdict {
	if !actorUserID.Valid {
		return LinkedActorDenied
	}
	return AuthorizeResolvedActorVerdict(ctx, logger, surface, users, actorUserID, action, resource)
}

// OwnedOrJoined mirrors internal/adapters/inbound/httpapi's own
// canActOnPlan/CreateTurn "own/joined" resolution exactly (§13.3 row 2):
// true iff sessionRow was created by actorUserID, or actorUserID has an
// existing participants row for it.
func OwnedOrJoined(ctx context.Context, participants *postgres.ParticipantStore, sessionRow sqlcgen.Session, actorUserID pgtype.UUID) (bool, error) {
	if sessionRow.CreatedBy.Valid && sessionRow.CreatedBy == actorUserID {
		return true, nil
	}
	return participants.Exists(ctx, sessionRow.ID, actorUserID)
}
