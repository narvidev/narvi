// This file (identity.go) implements §13.2's ("identities + full RBAC",
// §13.2) own auto-linking wiring for Slack ingress -- shared by both
// handler.go (the Events API route) and interactive.go (the Interactivity
// route), replacing this package's PREVIOUS unconditional bot-attribution
// (an always-invalid creator/decidedBy/actor pgtype.UUID, every prior
// Step's own explicit precedent) with "try auto-link, else bot
// attribution + link-prompt side effect", per this Step's own brief.
//
// A plain function (not a method on either Deps or InteractiveDeps, which
// are two DISTINCT struct types in this package) since both call sites
// need the identical logic over just three collaborators
// (SlackClient/IdentityLink/Timeouts) -- passing those three directly
// avoids either duplicating this function once per Deps type or forcing
// one bloated shared struct neither file's own Deps otherwise needs.

package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/internal/app/shadowslack"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/platform"
)

// authzSurface is this package's own "surface" label passed to every
// actorauthz.AuthorizeResolvedActor call below -- see that function's own
// doc comment for why (keeps this package's log lines prefixed "slack: "
// exactly as they were before the §13.2 shared-helper extraction into
// internal/app/actorauthz, batch fix/audit-github-actor-rbac).
const authzSurface = "slack"

// resolveSlackActor resolves slackUserID to a real Narvi user_id via
// internal/app/identitylink.Resolve, fetching that user's profile email
// (with retry, platform.Retry via identitylink.FetchEmailWithRetry) from
// Slack's own users.info API first -- ONLY when slackUserID is non-empty
// (every real app_mention/message/interactivity payload this package
// processes carries one; empty is a defensive no-op, never expected in
// practice) AND this identity is not ALREADY linked (identitylink.
// LookupLinkedUserID pre-check below): every inbound event used to pay a
// users.info round trip regardless of link state, even though Resolve's own
// internal fast path never reads the fetched email on a hit -- in the
// steady state where most actors are already linked, that was a discarded
// network call on every single message. The pre-check is the SAME indexed
// lookup Resolve itself still performs internally (kept there as a safety
// net, see that function's own doc comment) -- this just does it BEFORE
// spending a fetch whose result would be thrown away.
//
// Returns (actorUserID, notice): actorUserID is Valid iff this identity is
// now known to belong to a real user (already linked, or auto-linked THIS
// call); notice is §13.2's own "notify in-channel" text (identitylink.
// Resolution.NotificationText), or "" when there's nothing to say -- the
// CALLER decides how to deliver it, and every caller delivers it
// EPHEMERALLY (visible only to the acting user), never appended to
// whole-channel-visible text: handler.go delivers it via
// postEphemeralBounded (deps.SlackClient.PostEphemeral, bounded);
// interactive.go delivers its own sibling resolveSlackActorSingleAttempt's
// notice (below) via SlackClient.PostEphemeral directly. This replaced
// this Step's own PREVIOUS
// behavior -- appending the notice to the existing in-thread ack
// (handler.go) or to the plan-decision outcome text posted via
// chat.update (interactive.go) -- which a confirmed security review
// found let anyone in the channel read another user's link-prompt
// notice; do not reintroduce that whole-channel-visible hijack path by
// trusting a future edit of this comment over the actual delivery code.
func resolveSlackActor(ctx context.Context, logger *slog.Logger, slackClient shadowslack.Client, identityLinkDeps identitylink.Deps, timeouts platform.Timeouts, slackUserID string) (actorUserID pgtype.UUID, notice string) {
	if slackUserID == "" {
		return pgtype.UUID{}, ""
	}

	// Pre-check: already linked -> skip the fetch entirely, exactly
	// mirroring Resolve's own internal fast path (identitylink/service.go)
	// with none of the round trip below. A lookup error is NOT "not
	// linked" -- fall through to the fetch/Resolve path unchanged, since
	// Resolve's own identical internal lookup will hit the SAME error a
	// moment later and this function's existing error handling (below)
	// already logs and degrades to bot attribution for it; a pre-check
	// error would just be the identical outcome one call earlier.
	if userID, linked, err := identitylink.LookupLinkedUserID(ctx, identityLinkDeps, sqlcgen.IdentityProviderSlack, slackUserID); err == nil && linked {
		return userID, ""
	}

	email, ok := identitylink.FetchEmailWithRetry(ctx, logger, timeouts, sqlcgen.IdentityProviderSlack, func(attemptCtx context.Context) (string, bool, error) {
		e, o, err := slackClient.GetUserEmail(attemptCtx, slackUserID)
		if err != nil {
			if errors.Is(err, slackapi.ErrSlackUserNotFound) {
				// Slack's own definitive "no such user" -- never
				// retryable (see that sentinel's own doc comment).
				return "", false, platform.Permanent(err)
			}
			return "", false, err
		}
		return e, o, nil
	})

	res, err := identitylink.Resolve(ctx, identityLinkDeps, sqlcgen.IdentityProviderSlack, slackUserID, email, ok)
	if err != nil {
		logger.Warn("slack: identity auto-link resolve failed", "error", err, "slack_user_id", slackUserID)
		return pgtype.UUID{}, ""
	}
	return res.UserID, res.NotificationText()
}

// resolveSlackActorSingleAttempt is resolveSlackActor's own deliberately
// FASTER sibling, used ONLY by interactive.go's decideAndUpdateMessage/
// handleViewSubmission -- both share Slack's own hard ~3s interactivity-
// ack window with a real Postgres write (DecidePlan/CreateTurnCore) AND,
// for a click, a second outbound Slack call (the reply: chat.update or
// chat.postEphemeral) inside the SAME window (see
// platform.Timeouts.SlackInteractivityAckTimeout's own doc comment; for a
// click this fetch runs inside the decision's share of it,
// SlackInteractivityDecisionTimeout(), which Validate keeps it strictly
// inside), so there is no room left for resolveSlackActor's own
// multi-attempt backoff loop (identitylink.FetchEmailWithRetry). This
// makes exactly ONE profile-email fetch attempt, bounded by fetchTimeout
// (platform.Timeouts.SlackInteractivityIdentityFetchTimeout in
// production wiring), and treats ANY failure (timeout, transient error,
// even Slack's own definitive "no such user") identically -- bot
// attribution for THIS click, deferring a full, properly-retried
// resolution to the next event from this same identity (an Events API
// message, a later click, a modal submission all naturally retry via
// resolveSlackActor's own full algorithm).
//
// Pre-checks identitylink.LookupLinkedUserID (same as resolveSlackActor's
// own pre-check above, see its doc comment) BEFORE spending any of
// fetchTimeout's already-tight budget on a fetch whose result would be
// discarded on an already-linked hit -- this is the SINGLE most exposed
// instance of the pre-fix defect this pre-check closes: a hanging/slow
// fetch here eats directly into the sliver of Slack's ~3s non-retryable
// interactivity-ack window this function's own doc comment above says is
// already fully spoken for by other work.
func resolveSlackActorSingleAttempt(ctx context.Context, logger *slog.Logger, slackClient shadowslack.Client, identityLinkDeps identitylink.Deps, fetchTimeout time.Duration, slackUserID string) (actorUserID pgtype.UUID, notice string) {
	if slackUserID == "" {
		return pgtype.UUID{}, ""
	}

	if userID, linked, err := identitylink.LookupLinkedUserID(ctx, identityLinkDeps, sqlcgen.IdentityProviderSlack, slackUserID); err == nil && linked {
		return userID, ""
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	email, ok, err := slackClient.GetUserEmail(fetchCtx, slackUserID)
	if err != nil {
		email, ok = "", false
	}

	res, err := identitylink.Resolve(ctx, identityLinkDeps, sqlcgen.IdentityProviderSlack, slackUserID, email, ok)
	if err != nil {
		logger.Warn("slack: identity auto-link resolve failed", "error", err, "slack_user_id", slackUserID)
		return pgtype.UUID{}, ""
	}
	return res.UserID, res.NotificationText()
}

// authorizeResolvedActor/ownedOrJoined used to live here (
// "identities + full RBAC", §13.2/§13.3) but moved verbatim into
// internal/app/actorauthz (batch fix/audit-github-actor-rbac) once GitHub
// ingress became a third consumer of the identical logic Linear's own
// identity.go already duplicated once -- see that package's own doc.go for
// the full "why". authorizeSessionAction below calls
// actorauthz.AuthorizeResolvedActor/actorauthz.OwnedOrJoined directly now;
// nothing about ITS OWN behavior (the sequencing, the short-circuit for an
// unresolved actor, the error logging) changed.

// ErrActorNotAuthorized is authorizeSessionAction's own sentinel for a
// final, non-retryable denial -- "a resolved, linked actor's role
// genuinely failed domain/authz.Authorize" (MEDIUM audit fix,
// "authorizeSessionAction conflates a genuine backend error with a real
// authorization denial"), mirroring github's own identical
// ErrActorNotAuthorized (coalesce.go). Deliberately DISTINCT from any other
// error authorizeSessionAction returns: a caller checks for this one
// specifically and treats it as a final, non-retryable denial (skip
// without releasing the webhook-delivery claim -- redelivering the
// identical event would just render the same denial again), while any
// OTHER error is a genuine backend failure encountered WHILE checking
// authorization (e.g. deps.Sessions.Get hitting a dropped connection),
// which the caller instead routes into the SAME release-the-claim-and-
// retry path this batch's H2 fix already wired up for every other
// post-claim failure -- BEFORE this fix, authorizeSessionAction's own bare
// bool made the two indistinguishable, so a one-off DB blip was silently
// treated as a deliberate "skip, no release" business decision, dropping
// the user's legitimate message forever with no chance of redelivery ever
// retrying it.
//
// Audit-fix batch update ("block unlinked actor state changes", SECOND
// review pass): this sentinel USED to also cover "actorUserID never
// resolved to a linked account at all" -- collapsed into this same value
// because every existing caller's own errors.Is(err, ErrActorNotAuthorized)
// handling already did the right generic thing for both cases (post a
// denial, don't release the claim). A confirmed 3-lens adversarial review
// of that batch found this collapse actively harmful for exactly ONE
// caller: interactive.go's own decideAndUpdateMessage then answered a
// denial by calling deps.updateMessage, whose own chat.update request (see
// slackapi.Client.UpdateMessage's own doc comment) carries no "blocks"
// field at all -- Slack's own API treats that as "remove every block from
// this message", PERMANENTLY stripping the Approve/Reject buttons. For the
// NOT-YET-LINKED case it directly broke this batch's own headline
// guarantee: the SAME actor clicking the SAME button again, after linking,
// should succeed -- but there was nothing left in Slack to click. See
// ErrActorNotLinked below, the fix: a SEPARATE, more specific sentinel for
// the not-yet-linked case, so decideAndUpdateMessage (and
// handleViewSubmission) can tell the two apart and say different things,
// while every OTHER caller's existing errors.Is(err,
// ErrActorNotAuthorized) check keeps matching BOTH cases unchanged
// (ErrActorNotLinked wraps this error, see below). The stripping was no
// more acceptable for a resolved-but-refused actor: the message is the
// channel's, so a viewer's click took the buttons from every member who
// could have decided. decideAndUpdateMessage now answers every denial, and
// every other click that decides nothing, to the clicking user alone
// (chat.postEphemeral), leaving the message and its buttons as they were.
var ErrActorNotAuthorized = errors.New("slack: actor not authorized")

// ErrActorNotLinked is authorizeSessionAction's own MORE SPECIFIC sentinel
// for the "actorUserID never resolved to a linked account at all" half of
// ErrActorNotAuthorized's own denial space (see that var's own doc comment
// for the full "why" -- this is the SECOND review pass's own fix for the
// button-stripping regression that collapsing the two cases into one
// sentinel caused).
//
// Deliberately WRAPS ErrActorNotAuthorized (via fmt.Errorf's own %w) rather
// than being a wholly independent error: every EXISTING caller's own
// errors.Is(err, ErrActorNotAuthorized) check must keep matching this case
// too, with NO caller-side change required, exactly as it did before this
// sentinel existed -- errors.Is(ErrActorNotLinked, ErrActorNotAuthorized)
// is true, but errors.Is(ErrActorNotAuthorized, ErrActorNotLinked) is
// false, so only a caller that explicitly wants to distinguish "not yet
// linked" from "resolved but denied" needs to check for THIS sentinel
// specifically (and must do so BEFORE the more general
// ErrActorNotAuthorized check, since the general check would otherwise
// swallow it first). Today, that is ONLY interactive.go's own
// decideAndUpdateMessage/handleViewSubmission -- every other caller
// (handler.go's authorizeExistingSessionReply) is unaffected and needs no
// changes at all.
var ErrActorNotLinked = fmt.Errorf("slack: actor not yet linked: %w", ErrActorNotAuthorized)

// authorizeSessionAction renders the exact §13.3 verdict domain/authz.
// Authorize would for actorUserID attempting action against sessionID --
// this file's own Deps (handler.go, the Events API ingress) twin of
// InteractiveDeps.authorizeSessionAction (interactive.go), added by this
// Step's own SECOND fix pass so handler.go's own addTurn call (an
// ordinary reply on an already-mapped thread, or a brand-new mention that
// lost the first-writer-wins race and falls back onto a DIFFERENT
// winning session -- see handler.go's own authorizeExistingSessionReply)
// renders the IDENTICAL verdict the REST API/interactivity route already
// render for the same (actor, session, action).
//
// actorUserID.Valid == false (not yet linked -- the auto-link attempt for
// this identity did not resolve, e.g. no email match found) now returns
// ErrActorNotLinked immediately, with NO session/participants lookup at
// all -- audit-fix batch update ("block unlinked actor state changes"):
// this used to return nil (allowed), preserving §13.2's own original
// "unlinked actors get bot attribution ... the action proceeds" precedent.
// That precedent was a deliberate, user-decided hardening target, not a
// "keep as-is": a not-yet-linked actor's state-changing action is now
// denied exactly like a linked-but-insufficient-role one, and the SAME
// magic-link prompt this identity already gets (resolveSlackActor's own
// notice, delivered by every caller regardless of this denial) is how they
// retry once actually linked. ErrActorNotLinked WRAPS ErrActorNotAuthorized
// (see that var's own doc comment, and ErrActorNotLinked's own, for the
// SECOND review pass's own fix for a confirmed button-stripping regression
// this distinction closes) -- so every existing caller's own
// errors.Is(err, ErrActorNotAuthorized) handling -- post the honest denial
// message, do NOT release the webhook claim, do NOT treat it as a
// retryable backend failure -- applies UNCHANGED, with no caller-side edit
// required; see handler.go's own authorizeExistingSessionReply (unchanged)
// and interactive.go's own decideAndUpdateMessage/handleViewSubmission
// (which DO tell ErrActorNotLinked apart from a resolved-but-denied
// ErrActorNotAuthorized, to tell a not-yet-linked actor to link and retry
// rather than that they lack permission; neither answer touches the
// approval message's Approve/Reject buttons) for the confirmed call sites.
// This twin still reads actorauthz.AuthorizeResolvedActor's bool, so a role
// it could not read is reported as ErrActorNotAuthorized; interactive.go's
// twin reads the verdict and reports that as a backend failure.
//
// Returns ErrActorNotLinked when actorUserID is not linked at all (above,
// itself matched by errors.Is(err, ErrActorNotAuthorized) too, see that
// wrapping relationship above) OR ErrActorNotAuthorized directly when a
// RESOLVED actor's own role genuinely fails domain/authz.Authorize -- both
// are final, non-retryable denials, handled identically by every caller
// that only checks for ErrActorNotAuthorized. Returns any OTHER (non-nil)
// error for a genuine backend failure encountered while checking
// (deps.Sessions.Get/actorauthz.OwnedOrJoined erroring) -- MEDIUM audit
// fix, see ErrActorNotAuthorized's own doc comment for why this distinction
// matters to the caller.
func (deps Deps) authorizeSessionAction(ctx context.Context, logger *slog.Logger, sessionID, actorUserID pgtype.UUID, action authz.Action) error {
	if !actorUserID.Valid {
		return ErrActorNotLinked
	}

	sessionRow, err := deps.Sessions.Get(ctx, sessionID)
	if err != nil {
		logger.Error("slack: get session for authorization failed", "error", err, "session_id", sessionID.String(), "action", string(action))
		return fmt.Errorf("slack: get session for authorization: %w", err)
	}

	joined, err := actorauthz.OwnedOrJoined(ctx, deps.Participants, sessionRow, actorUserID)
	if err != nil {
		logger.Error("slack: check participant for authorization failed", "error", err, "session_id", sessionID.String(), "action", string(action))
		return fmt.Errorf("slack: check participant for authorization: %w", err)
	}

	if !actorauthz.AuthorizeResolvedActor(ctx, logger, authzSurface, deps.IdentityLink.Users, actorUserID, action, authz.Resource{OwnedOrJoined: joined}) {
		return ErrActorNotAuthorized
	}
	return nil
}
