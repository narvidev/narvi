// This file (interactive.go) implements §8.1's ("plan mode,
// cross-channel", §8.1/§13.3) own new Slack ingress route: POST
// /webhooks/slack/interactive, a STRUCTURALLY DIFFERENT payload shape from
// this package's existing POST /webhooks/slack (handler.go, §8.10's
// Events API ingress) -- Slack's own real Interactivity payload arrives
// form-encoded (Content-Type: application/x-www-form-urlencoded) with a
// single "payload" field carrying URL-encoded JSON, never the Events API's
// raw JSON body. This is deliberately a SEPARATE handler/route, never
// folded into NewHandler above: the two request shapes share almost
// nothing (form vs JSON body) beyond both being HMAC-signed the same way
// (platform.VerifyWebhookSignature operates on raw bytes regardless of
// encoding, so it is reused here unchanged -- see verifySlackRequest,
// handler.go).
//
// # Operator setup note (real, external, one-time -- not automatable here)
//
// This endpoint requires enabling "Interactivity & Shortcuts" in the
// Slack App's own configuration (api.slack.com/apps -> your app ->
// Interactivity & Shortcuts), with its "Request URL" pointed at this
// route (POST .../webhooks/slack/interactive) -- a DIFFERENT configured
// URL from the Events API's own "Request URL" (POST .../webhooks/slack).
// Until an operator does this, real button clicks and modal submissions
// will either never arrive at all, or (if pointed at the wrong URL) fail
// this handler's own signature verification -- no code change here can
// perform that one-time App-dashboard configuration step.
//
// # Dispatch, by the inbound payload's own "type" field
//
//   - "block_actions" (a button was clicked): approve_plan/reject_plan call
//     the shared httpapi.DecidePlan synchronously, then answer the click
//     synchronously: an outcome -- a plan this click decided, or one
//     already decided or superseded -- through a real chat.update
//     (slackapi.Client.UpdateMessage) on the SAME message, which removes
//     its buttons; anything that left the plan awaiting approval -- a
//     refusal, a failure, a budget run out -- through chat.postEphemeral
//     to the clicking user alone, leaving the message, its plan and its
//     buttons as they were for the next click. The decision runs on
//     platform.Timeouts.SlackInteractivityDecisionTimeout() and the reply
//     on SlackInteractivityReplyTimeout, on a context detached from the
//     decision's, the two together filling
//     SlackInteractivityAckTimeout (see those fields' own doc comments,
//     platform/timeouts.go, for why this is a SEPARATE, much tighter
//     budget than SlackAckTimeout), so the pair together never blows past
//     Slack's own real ~3-second interactivity-ack budget, and a decision
//     that ran out of its share is still answered.
//     request_changes_plan calls views.open (using the inbound trigger_id,
//     valid only a few seconds), bounded by the same constant, and does NO
//     turn-creation work yet -- that happens on the LATER view_submission
//     below. Every path responds 200 immediately after, regardless of
//     whether the bounded work above finished or hit its own deadline.
//   - "view_submission" (the request-changes modal was submitted): the
//     submitted feedback text + planId/sessionId (from the view's own
//     private_metadata, set when views.open was called) create a new
//     turn (plan_mode=true) via httpapi.CreateTurnCore -- the EXACT SAME
//     function POST .../turns itself calls, never a third, duplicated
//     turn-creation call site. Responds with Slack's own required
//     empty-body 200 (closes the modal) once the turn is created; a
//     submission that creates nothing is answered with an inline modal
//     error instead (writeViewSubmissionError), so the modal stays open
//     with the feedback still in it.
//   - anything else: logged and 200'd as a no-op -- a future Slack
//     interaction type this handler doesn't yet understand must degrade
//     gracefully, never crash or 500.
//
// Every decision/turn this handler causes now resolves the REAL Slack
// user behind it to a Narvi user_id/role, via identity auto-linking
// (§13.2, identity.go's own resolveSlackActorSingleAttempt) -- and, once
// resolved, that role is checked against domain/authz.Authorize before
// the state-changing effect (decideAndUpdateMessage's own
// authorizeSessionAction(ActionApprovePlan) call;
// handleViewSubmission's own identical authorizeSessionAction
// (ActionPromptSession) call) -- a confirmed security review found this
// gate MISSING for a resolved, linked actor (any role, any ownership)
// even after auto-linking already existed, contradicting §13.3's own
// "channel-agnostic" requirement; see decideplan.go's own top doc comment
// for the full "why" writeup. A still-UNLINKED (bot-attributed, Valid ==
// false) actor is now DENIED too (audit-fix batch, "block unlinked actor
// state changes") rather than let through under bot attribution -- see
// authorizeSessionAction's own doc comment below for the current behavior
// and the distinct ErrActorNotLinked sentinel it returns for this case.
// decideAndUpdateMessage answers that denial, like every other click that
// leaves the plan awaiting approval, WITHOUT touching the original
// message's Approve/Reject buttons (so the same click can be retried once
// the actor links), and handleViewSubmission keeps its modal open.

package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/shadowslack"
	"github.com/narvidev/narvi/internal/domain/authz"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/platform"
)

// InteractiveDeps bundles every dependency NewInteractivityHandler needs --
// a separate struct from Deps (handler.go): this route's own payload shape
// and downstream calls (DecidePlan, CreateTurnCore, real Block Kit
// updates/modals) share almost nothing with the Events API ingress's own
// Deps, so keeping them distinct mirrors how internal/adapters/inbound/
// linear already gives its OAuth-install handlers and its webhook handler
// each their own parameter shape rather than one bloated shared struct.
type InteractiveDeps struct {
	Pool     *pgxpool.Pool
	Sessions *postgres.SessionStore
	Turns    *postgres.TurnStore
	Plans    *postgres.PlanStore
	// Events/PlanDocuments (§31.3) -- decideAndUpdateMessage's own
	// httpapi.DecidePlan call (below) needs these for its own
	// approved-plan snapshot dependency (decideplan.go), mirroring
	// slack.Deps's/linear.Deps's identical fields exactly.
	Events              *postgres.EventStore
	PlanDocuments       *postgres.PlanDocumentStore
	Outbox              *postgres.OutboxStore
	LinearAgentSessions *postgres.LinearAgentSessionStore
	Registry            *sessionactor.Registry
	// SlackClient is typed as the shadowslack.Client interface, never the
	// concrete *slackapi.Client -- this package no longer constructs a
	// client of its own (§30.3's "one client per provider, ingress
	// packages lose the right to construct clients"). Production wiring
	// (cmd/control-plane/main.go) hands over a shadowslack.Decorator
	// wrapping the SAME *slackapi.Client instance handler.go's own
	// identical Deps.SlackClient field uses, never a second one.
	SlackClient shadowslack.Client

	// Participants is §13.2's own addition ("identities + full RBAC",
	// §13.2/§13.3) -- authorizeSessionAction below (identity.go's own
	// ownedOrJoined) needs this to resolve a `member` actor's own
	// "own/joined" carve-out exactly like httpapi's canActOnPlan/CreateTurn
	// already do, so a Slack-decided plan verdict or Slack "Request
	// changes" turn renders the IDENTICAL §13.3 verdict a REST caller
	// would for the same (actor, session).
	Participants *postgres.ParticipantStore

	// AuditLog is §13.2's own addition (§13.3) -- threaded through to
	// httpapi.DecidePlan/CreateTurnCore below exactly like Plans/Outbox
	// already are, so a Slack-decided plan verdict or a Slack "Request
	// changes" turn gets the SAME audit_log row every other caller of
	// those two shared functions now gets. actor_user_id is NULL only
	// until identity auto-linking (IdentityLink below) resolves a real
	// user -- see identity.go's own resolveSlackActor for the replacement
	// of the old unconditional bot-attribution precedent.
	AuditLog *postgres.AuditLogStore

	// IdentityLink is §13.2's own auto-linking wiring (§13.2) --
	// resolveSlackActor (identity.go) uses this handler's own SlackClient
	// above (already present, for chat.update/views.open) to fetch the
	// clicking/submitting Slack user's own profile email, then this to
	// auto-link or create a magic-link prompt.
	IdentityLink identitylink.Deps

	// EpistemicCheckDefault ("builder epistemic pre-action
	// check", §20.4) is threaded through to handleViewSubmission's own
	// httpapi.CreateTurnCore call below exactly like Deps.
	// EpistemicCheckDefault (handler.go) -- production wiring
	// (cmd/control-plane/main.go) passes the SAME platform.Config.
	// EpistemicCheckDefault value both places. That call always names
	// planMode=true (a "request changes" turn is always a plan-mode
	// revise turn), so ShouldInjectEpistemicPreamble never actually
	// injects the preamble here regardless of this value (§20.3) -- this
	// field is threaded anyway, rather than a hardcoded false, so this
	// call site stays correct by construction if it is ever reused for a
	// non-plan-mode turn.
	EpistemicCheckDefault bool

	SigningSecret string
	Timeouts      platform.Timeouts
}

// blockActionsPayload is the subset of Slack's own real block_actions
// interaction payload this handler needs (verified against Slack's current
// reference docs, docs.slack.dev/reference/interaction-payloads/
// block_actions-payload, during this Step's own investigation).
type blockActionsPayload struct {
	Type      string `json:"type"`
	TriggerID string `json:"trigger_id"`
	Channel   struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		Ts string `json:"ts"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
	// User is Slack's own real top-level "the user who clicked" field,
	// present on every block_actions payload (verified against Slack's
	// current reference docs) -- §13.2's ("identities + full RBAC",
	// §13.2) own auto-linking wiring: the Slack user id
	// decideAndUpdateMessage resolves against.
	User struct {
		ID string `json:"id"`
	} `json:"user"`
}

// viewSubmissionPayload is the subset of Slack's own real view_submission
// interaction payload this handler needs. State.Values is Slack's own
// documented "block_id -> action_id -> {value}" shape for a plain_text_input
// element's submitted value.
type viewSubmissionPayload struct {
	Type string `json:"type"`
	View struct {
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
		State           struct {
			Values map[string]map[string]struct {
				Value *string `json:"value"`
			} `json:"values"`
		} `json:"state"`
	} `json:"view"`
	// User is Slack's own real top-level "the user who submitted this
	// modal" field, present on every view_submission payload (verified
	// against Slack's current reference docs) -- §13.2's own auto-
	// linking wiring: the Slack user id handleViewSubmission resolves
	// against.
	User struct {
		ID string `json:"id"`
	} `json:"user"`
}

// interactionEnvelope is the minimal shape read first, JUST to learn the
// payload's own "type" -- mirrors handler.go's own challengeEnvelope/
// eventEnvelope "peek the cheap common field first" precedent.
type interactionEnvelope struct {
	Type string `json:"type"`
}

// NewInteractivityHandler backs POST /webhooks/slack/interactive -- see
// this file's own top doc comment for the full design and the real,
// external, one-time operator setup step this route requires.
func NewInteractivityHandler(deps InteractiveDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		body, ok := readBoundedBody(w, r)
		if !ok {
			return
		}

		// Signature verification is IDENTICAL to the Events API ingress's
		// own (verifySlackRequest, handler.go) -- Slack signs the exact raw
		// request body regardless of its own Content-Type/encoding, so
		// platform.VerifyWebhookSignature is directly reusable here
		// unchanged (this file's own top doc comment).
		if !verifySlackRequest(w, r, body, deps.SigningSecret, deps.Timeouts.WebhookTimestampFreshnessWindow, logger) {
			return
		}

		// The body is form-encoded (application/x-www-form-urlencoded),
		// NOT JSON -- url.ParseQuery on the already-read raw bytes (rather
		// than r.ParseForm, which would try to re-read the already-drained
		// r.Body) extracts the single "payload" field, Slack's own
		// URL-encoded JSON.
		values, err := url.ParseQuery(string(body))
		if err != nil {
			writeError(w, http.StatusBadRequest, "malformed form-encoded body")
			return
		}
		rawPayload := values.Get("payload")
		if rawPayload == "" {
			writeError(w, http.StatusBadRequest, "missing payload field")
			return
		}

		var envelope interactionEnvelope
		if err := json.Unmarshal([]byte(rawPayload), &envelope); err != nil {
			writeError(w, http.StatusBadRequest, "malformed payload JSON")
			return
		}

		switch envelope.Type {
		case "block_actions":
			deps.handleBlockActions(ctx, logger, []byte(rawPayload))
			w.WriteHeader(http.StatusOK)
		case "view_submission":
			// handleViewSubmission ("identities + full RBAC",
			// §13.2/§13.3 update) now writes its OWN response: an ordinary
			// empty-body 200 on success/no-op (Slack's own documented
			// contract, closes the modal), or a real Slack
			// "response_action": "errors" body when the submission is
			// refused -- blank feedback, an authorization denial, a turn
			// already open, or a failure of the check or of the create.
			// This view_submission payload has no channel/thread to post
			// an ordinary reply into at all (see that function's own doc
			// comment), so Slack's own inline-modal-error mechanism is this
			// path's equivalent of the REST API's 4xx/5xx.
			deps.handleViewSubmission(ctx, w, logger, []byte(rawPayload))
		default:
			// A future Slack interaction type this handler doesn't yet
			// understand -- logged, never a crash or 500 (this file's own
			// top doc comment).
			logger.Info("slack: interactivity: ignoring unrecognized payload type", "type", envelope.Type)
			w.WriteHeader(http.StatusOK)
		}
	}
}

// handleBlockActions dispatches on the clicked action's own action_id --
// approve_plan/reject_plan decide synchronously (and update the message
// synchronously, for immediate visual feedback); request_changes_plan
// opens the feedback modal. Every path here must stay fast: Slack requires
// an ack within 3 seconds, and the caller (NewInteractivityHandler above)
// writes that 200 immediately after this returns.
func (deps InteractiveDeps) handleBlockActions(ctx context.Context, logger *slog.Logger, raw []byte) {
	var payload blockActionsPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		logger.Warn("slack: interactivity: decode block_actions payload failed", "error", err)
		return
	}
	if len(payload.Actions) == 0 {
		logger.Warn("slack: interactivity: block_actions payload has no actions")
		return
	}
	action := payload.Actions[0]

	planIDStr, sessionIDStr, ok := slackapi.DecodePlanActionValue(action.Value)
	if !ok {
		logger.Warn("slack: interactivity: malformed action value", "action_id", action.ActionID, "value", action.Value)
		return
	}

	switch action.ActionID {
	case slackapi.ActionApprovePlan, slackapi.ActionRejectPlan:
		deps.decideAndUpdateMessage(ctx, logger, planIDStr, sessionIDStr, action.ActionID, payload.Channel.ID, payload.Message.Ts, payload.User.ID)
	case slackapi.ActionRequestChangesPlan:
		deps.openRequestChangesModal(ctx, logger, payload.TriggerID, planIDStr, sessionIDStr)
	default:
		logger.Info("slack: interactivity: unrecognized action_id", "action_id", action.ActionID)
	}
}

// slackPlanForbiddenText/slackPromptForbiddenErrorText are §13.2's own
// additions ("identities + full RBAC", §13.2/§13.3): posted/shown instead
// of proceeding when the acting user isn't authorized -- mirroring the
// REST API's own 403 semantics ("not authorized to ...",
// helpers.go/planapprove.go) rather than silently dropping the
// click/submission.
//
// Audit-fix batch update ("block unlinked actor state changes"): these same
// two constants are now ALSO shown when the actor isn't linked to a Narvi
// account at all yet (authorizeSessionAction below denies that case
// immediately too, not only a resolved-but-insufficient-role denial).
// Unlike handler.go's own ackNotAuthorizedText/ackNotAuthorizedReplyText,
// no wording change was needed here -- neither constant ever claimed a link
// already existed, so both already read correctly either way. The
// separately-delivered magic-link ephemeral notice (decideAndUpdateMessage's
// own notice via SlackClient.PostEphemeral) is what tells an unlinked actor
// specifically how to fix it.
//
// slackDecisionErrorText/slackRequestChangesErrorText are this same Step's
// own MEDIUM audit-fix addition ("Slack's interactive.go has its OWN
// separate, still-unfixed copy" of identity.go's authorizeSessionAction
// conflation): shown instead of the two ...ForbiddenText constants above
// when authorizeSessionAction (below) hits a genuine backend error rather
// than a real denial -- a session or participant read that failed, or a
// role that could not be read (actorauthz.LinkedActorError) -- so an
// internal failure is never misreported to the actor as a permission
// denial. slackDecisionErrorText is also what decideAndUpdateMessage shows
// when DecidePlan itself fails, the decision's own budget running out
// included, so the actor sees the identical honest message regardless of
// which call underneath actually failed. Like every reply to a click that
// leaves the plan awaiting approval, it reaches the clicking user alone
// (chat.postEphemeral) and leaves the approval message and its buttons as
// they were, so its "try again" points at buttons that are still there.
// slackRequestChangesErrorText is its handleViewSubmission counterpart,
// and is also what that function shows when CreateTurnCore itself fails
// for any reason other than a turn already being open
// (requestChangesRefusalText) -- that failure used to be only logged,
// closing the modal as accepted with the feedback lost.
const (
	slackPlanForbiddenText        = "You don't have permission to approve or reject this plan."
	slackPromptForbiddenErrorText = "You don't have permission to request changes on this plan."

	slackDecisionErrorText       = "Something went wrong recording this decision. Please try again."
	slackRequestChangesErrorText = "Something went wrong submitting this. Please try again."

	// slackPlanOpenTurnText answers an Approve click DecidePlan refused
	// because a turn of the session -- the plan's own revision, typically
	// -- is still pending, dispatched or processing
	// (httpapi.ErrPlanOpenTurnInFlight), on a plan still awaiting approval
	// (answerOpenTurnRefusal reads it; a plan already decided gets its
	// outcome instead). It reaches the clicking user
	// alone, and the approval message keeps its plan and its buttons, so
	// once that turn ends without recording a newer version, which would
	// supersede this one, a click on the same message approves it. The
	// same words a Slack thread reply gets for the same refusal
	// (handlePlanVerdict, handler.go).
	slackPlanOpenTurnText = "A revision is already in progress for this plan — try again once it completes."

	// slackRequestChangesBusyErrorText is what the modal shows when
	// CreateTurnCore refuses the revision because a turn of the session
	// (typically the approved implementation) is still pending, dispatched
	// or processing (httpapi.ErrTurnAlreadyOpen). Nothing is queued
	// (technical plan §43.21(a)): the modal stays open with the feedback in
	// it, so the person can submit it again once that turn ends -- the
	// modal's counterpart of the busy reply a Slack thread reply or a
	// Linear reply gets for the same condition (ackBusyText, handler.go).
	slackRequestChangesBusyErrorText = "A turn is still running on this session, so this change wasn't submitted or queued. Submit it again once that turn ends."

	// slackEmptyFeedbackErrorText is this batch's own audit-remediation fix
	// (CONFIRMED MEDIUM finding, "the pre-existing 'Request changes' modal
	// still silently drops empty/whitespace-only feedback with zero
	// user-visible signal"): handleViewSubmission's own empty-feedback guard
	// below used to write a bare 200 with no response_action at all --
	// Slack's own documented behavior for that shape is to close the modal
	// as though the submission succeeded, giving the submitter no signal
	// whatsoever that their feedback was discarded. This mirrors
	// slackRequestChangesErrorText's own inline-modal-error mechanism
	// (viewSubmissionErrorResponse) instead, matching the SAME honest
	// "you must be told this failed" rule the new Slack/Linear revise: text
	// path (handler.go's ackPlanAwaitingText / webhook.go's
	// planAwaitingApprovalReplyText) already applies for the identical
	// empty-feedback case reached via a chat reply.
	slackEmptyFeedbackErrorText = "Feedback can't be empty. Please describe the changes you'd like."

	// slackActorNotLinkedDecisionText is the SECOND review pass's own fix
	// for the HIGH-severity button-stripping regression (see
	// authorizeSessionAction's own doc comment, and ErrActorNotLinked's,
	// identity.go, for the full incident): posted via chat.postEphemeral
	// (visible ONLY to the clicking user) INSTEAD OF calling
	// deps.updateMessage, like every reply to a click that decided
	// nothing, so the original message's Approve/Reject buttons
	// are never touched -- the magic-link URL itself was already posted
	// ephemerally, separately, just above this check (decideAndUpdateMessage's
	// own resolveSlackActorSingleAttempt call) regardless of this denial;
	// this text's own job is only to explain that THIS click specifically
	// did not go through, so the actor knows to link first, then click
	// Approve/Reject again -- not to repeat the link itself.
	slackActorNotLinkedDecisionText = "Your Slack account isn't linked to a Narvi account yet, so this decision wasn't recorded. Once you're linked (see the message just above), click Approve/Reject again."
)

// authorizeSessionAction renders the exact §13.3 verdict
// domain/authz.Authorize would for actorUserID attempting action against
// sessionID -- shared by decideAndUpdateMessage (ActionApprovePlan) and
// handleViewSubmission (ActionPromptSession, this package's own "Request
// changes" flow) below, both of which need the identical "resolve
// own/joined, then Authorize" sequencing canActOnPlan/CreateTurn already
// establish for the REST API (planauthz.go, httpapi/turn.go).
//
// actorUserID.Valid == false (not yet linked -- the auto-link attempt for
// this identity completed and found no link; one that did not complete is
// answered as a backend failure by both callers before they get here, see
// resolveSlackActorSingleAttempt's err) now returns ErrActorNotLinked immediately,
// with NO session/participants lookup at all -- audit-fix batch update
// ("block unlinked actor state changes"): this used to return nil
// (allowed), preserving §13.2's own original "unlinked actors get bot
// attribution ... the action proceeds" precedent. That precedent was a
// deliberate, user-decided hardening target, not a "keep as-is": a
// not-yet-linked actor's plan decision / request-changes submission is now
// denied exactly like a linked-but-insufficient-role one, and the SAME
// magic-link prompt this identity already gets (resolveSlackActorSingleAttempt's
// own notice, delivered by both callers regardless of this denial) is how
// they retry once actually linked.
//
// This is a SEPARATE function from identity.go's own (Deps.
// authorizeSessionAction, the Events-API ingress's twin) -- a different
// receiver type (InteractiveDeps, not Deps), never the same function --
// but both share the package-level ErrActorNotAuthorized/ErrActorNotLinked
// sentinels (identity.go) directly: a denial means the identical thing
// regardless of which route rendered it. Returns ErrActorNotLinked when
// actorUserID is not linked at all (above -- itself ALSO matched by
// errors.Is(err, ErrActorNotAuthorized), since ErrActorNotLinked wraps it,
// see identity.go's own doc comment) OR ErrActorNotAuthorized directly when
// a RESOLVED actor's role was read and genuinely refused
// (actorauthz.LinkedActorDenied: domain/authz.Authorize said no, or the
// linked account is disabled).
//
// Audit-fix batch update (SECOND review pass, "unlinked-actor plan-decision
// denial permanently strips Slack's Approve/Reject buttons"): UNLIKE
// identity.go's own Deps.authorizeSessionAction (whose only caller,
// authorizeExistingSessionReply, responds to EITHER denial identically),
// decideAndUpdateMessage/handleViewSubmission below tell these two denials
// apart, checking ErrActorNotLinked FIRST, to tell a not-yet-linked actor
// to link and click again rather than that they lack permission. Neither
// denial touches the approval message any more: a confirmed 3-lens
// adversarial review found that decideAndUpdateMessage once answered a
// denial through deps.updateMessage, whose chat.update request carries no
// "blocks" field at all (slackapi.Client.UpdateMessage's own doc comment),
// which Slack's own API treats as "strip every block from this message" --
// removing the plan and its Approve/Reject buttons for everyone in the
// channel, not only for the clicker: a refused click took the buttons from
// every member who could have decided. decideAndUpdateMessage now answers
// every denial, like every other click that decides nothing, to the
// clicking user alone (chat.postEphemeral), so the buttons stay for the
// next click -- the same actor's once linked, or a permitted member's.
//
// Returns any OTHER (non-nil) error for a genuine backend failure
// encountered while checking -- deps.Sessions.Get/actorauthz.OwnedOrJoined
// erroring, or the actor's role not read at all (actorauthz.
// LinkedActorError: the users lookup failed, or domain/authz.Authorize
// errored with something other than ErrForbidden), which is
// errActorRoleUnreadable, never ErrActorNotAuthorized: a role this check
// could not read says nothing about whether the actor may act. MEDIUM
// audit fix ("Slack's interactive.go has its OWN separate, still-unfixed
// copy" of the same conflation identity.go's own twin already had fixed):
// before that fix, this function's own bare bool made a transient backend
// error indistinguishable from a real denial, so BOTH
// decideAndUpdateMessage and handleViewSubmission (below) showed the
// misleading "you don't have permission" text on an internal failure and
// silently discarded the actor's real click/submission. This endpoint has
// no webhook-delivery-claim/release plumbing the way the Events-API
// ingress does (this file's own top doc comment -- Slack's own tight ~3s
// interactivity-ack budget), so there is no claim to route a backend error
// into releasing; instead, both call sites show an honest generic-error
// message on that branch -- decideAndUpdateMessage to the clicker alone,
// with the buttons left in place, handleViewSubmission inline in the modal
// it keeps open -- which is what makes clicking the button again /
// resubmitting the modal an actually meaningful retry.
func (deps InteractiveDeps) authorizeSessionAction(ctx context.Context, logger *slog.Logger, sessionID, actorUserID pgtype.UUID, action authz.Action) error {
	if !actorUserID.Valid {
		return ErrActorNotLinked
	}

	sessionRow, err := deps.Sessions.Get(ctx, sessionID)
	if err != nil {
		logger.Error("slack: interactivity: get session for authorization failed", "error", err, "session_id", sessionID.String(), "action", string(action))
		return fmt.Errorf("slack: interactivity: get session for authorization: %w", err)
	}

	joined, err := actorauthz.OwnedOrJoined(ctx, deps.Participants, sessionRow, actorUserID)
	if err != nil {
		logger.Error("slack: interactivity: check participant for authorization failed", "error", err, "session_id", sessionID.String(), "action", string(action))
		return fmt.Errorf("slack: interactivity: check participant for authorization: %w", err)
	}

	// The verdict, not its bool: the bool folds a role this check could not
	// read (LinkedActorError, already logged by the verdict itself) into a
	// refusal, and reporting a database failure as "you don't have
	// permission" is the conflation the two reads above already avoid.
	switch actorauthz.AuthorizeResolvedActorVerdict(ctx, logger, authzSurface, deps.IdentityLink.Users, actorUserID, action, authz.Resource{OwnedOrJoined: joined}) {
	case actorauthz.LinkedActorAllowed:
		return nil
	case actorauthz.LinkedActorDenied:
		return ErrActorNotAuthorized
	default:
		return errActorRoleUnreadable
	}
}

// errActorRoleUnreadable is authorizeSessionAction's error for a resolved
// actor whose role could not be read (actorauthz.LinkedActorError): a
// backend failure, answered like the session and participant reads'
// failures, with the generic text and its "try again" -- never a denial.
var errActorRoleUnreadable = errors.New("slack: interactivity: the actor's role could not be read for authorization")

// decideAndUpdateMessage resolves and authorizes the clicking user, calls
// the shared httpapi.DecidePlan (decideplan.go) as them, then answers the
// click synchronously. When the plan has an outcome, it updates the SAME
// Slack message to reflect the REAL final outcome -- whether THIS click
// won or the plan was already decided elsewhere
// (DecidePlanOutcome.FinalStatus reports the truth either way, so this
// never shows a misleading "pending" state after the fact).
//
// Every click is answered, and how depends on what it did. An outcome --
// a plan this click decided, or one already decided or superseded, whose
// buttons have nothing left to do -- replaces the message's text through
// updateMessage, whose chat.update carries no blocks and so removes the
// plan and its buttons, for everyone in the channel -- an Approve click
// refused because a turn of the session is open included, when the plan
// it names turns out to be decided already (answerOpenTurnRefusal).
// Anything that leaves the plan awaiting approval -- an identity that
// could not be resolved, a not-yet-linked actor, a refused one, a failed
// authorization check, a cut plan, a turn of the session still open, any
// other DecidePlan failure, the decision's budget running out -- is
// answered to the clicking user alone through replyPrivately
// (chat.postEphemeral), and the message keeps its plan and its buttons for
// the next click, this actor's or a permitted member's. Leaving the
// message alone on an error never strands a decided plan behind live
// buttons: a decision that commits enqueues a slack_plan_decided outbox
// row for the plan's message in its own transaction, whichever channel
// made it (enqueuePlanDecisionNotifications, httpapi/decideplan.go), so
// even a commit whose acknowledgement was lost reaches the message, and a
// later click on a decided plan is answered with its outcome.
//
// The decision -- the identity resolution, the authorization check and
// DecidePlan's own guarded-UPDATE transaction -- runs on a context bounded
// by deps.Timeouts.SlackInteractivityDecisionTimeout(), and the reply on a
// context of its own (replyContext), detached from the decision's and
// bounded by SlackInteractivityReplyTimeout: the two shares together fill
// SlackInteractivityAckTimeout, so the pair never blows past Slack's real
// ~3s interactivity-ack window (see those fields' own doc comments,
// platform/timeouts.go), and a decision that ran out of its share (DB
// contention: the session row's lock held by another transaction) is still
// answered, with the generic error text, to the clicker alone. A reply
// that fails even on its own budget is logged and reaches nobody: the
// buttons are still there, and a second click is the retry. The caller
// (NewInteractivityHandler above) still answers Slack with its own
// unconditional 200 right after this function returns, exactly matching
// Slack's own documented contract of acking within the window regardless of
// whether the underlying work has actually finished.
func (deps InteractiveDeps) decideAndUpdateMessage(ctx context.Context, logger *slog.Logger, planIDStr, sessionIDStr, actionID, channel, messageTS, slackUserID string) {
	var planID, sessionID pgtype.UUID
	if err := planID.Scan(planIDStr); err != nil {
		logger.Warn("slack: interactivity: parse plan id failed", "error", err, "plan_id", planIDStr)
		return
	}
	if err := sessionID.Scan(sessionIDStr); err != nil {
		logger.Warn("slack: interactivity: parse session id failed", "error", err, "session_id", sessionIDStr)
		return
	}

	verdict := httpapi.PlanVerdictApprove
	if actionID == slackapi.ActionRejectPlan {
		verdict = httpapi.PlanVerdictReject
	}

	decideCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.SlackInteractivityDecisionTimeout())
	defer cancel()

	// §13.2 ("identities + full RBAC", §13.2) update: decidedBy is no
	// longer unconditionally invalid -- resolveSlackActorSingleAttempt
	// auto-links (or creates a magic-link prompt for) the clicking Slack
	// user the first time this package sees them, WITHOUT the general
	// algorithm's own multi-attempt retry (see that function's own doc
	// comment for why this tightly-bounded interactivity path can't
	// afford one).
	//
	// A resolution that did not complete -- the linked-identity read or
	// Resolve failing, or the decision's share running out while they
	// wait -- says nothing about whether the clicker is linked, so it is
	// a backend failure, answered like every other: the generic text and
	// its "try again", to the clicker alone, the buttons left in place.
	// Only a resolution that completed and found no link is denied below
	// as ErrActorNotLinked, with the not-linked text.
	decidedBy, notice, err := resolveSlackActorSingleAttempt(decideCtx, logger, deps.SlackClient, deps.IdentityLink, deps.Timeouts.SlackInteractivityIdentityFetchTimeout, slackUserID)
	if err != nil {
		logger.Error("slack: interactivity: plan decision not made, the clicking user's identity could not be resolved", "error", err, "plan_id", planIDStr, "session_id", sessionIDStr)
		deps.replyPrivately(decideCtx, logger, channel, slackUserID, messageTS, slackDecisionErrorText)
		return
	}

	// Security-remediation addition ("identities + full RBAC",
	// §13.2): notice (the "connected your account" confirmation, or --
	// far more sensitive -- the magic-link URL itself) is delivered via
	// chat.postEphemeral, visible ONLY to slackUserID (the clicking user),
	// NEVER appended to the chat.update text below anymore -- that text
	// updates a message the WHOLE channel can see, so appending a magic
	// link there is exactly the confirmed hijack slackapi.Client.
	// PostEphemeral's own doc comment describes. Best-effort: a failure
	// here never blocks the actual plan decision below. It is not the
	// reply to the click, which follows the decision on a budget of its
	// own: it runs inside the decision's share, on decideCtx, so the
	// decision, this notice and the reply still fit the ack window.
	if notice != "" {
		if err := deps.SlackClient.PostIdentityLinkNotice(decideCtx, channel, slackUserID, messageTS, notice); err != nil {
			logger.Warn("slack: interactivity: post identity-link ephemeral notice failed", "error", err)
		}
	}

	// §13.2 ("identities + full RBAC", §13.2/§13.3) update: a decidedBy
	// that resolved to a REAL, linked user_id must still pass domain/authz.
	// Authorize(ActionApprovePlan) -- exactly what the REST approve/reject
	// endpoints already require via canActOnPlan (planauthz.go) -- before
	// this click is allowed to decide anything.
	//
	// Audit-fix batch update ("block unlinked actor state changes"): a
	// still-unlinked (bot-attributed) decidedBy is NO LONGER let through --
	// authorizeSessionAction now returns ErrActorNotLinked immediately for
	// that case (SECOND review pass: a DISTINCT sentinel from
	// ErrActorNotAuthorized, checked FIRST below -- see identity.go's own
	// ErrActorNotLinked doc comment for why), replacing this package's
	// previous "Slack verdicts stay unauthenticated-per-user until linked"
	// precedent (decideplan.go's own top doc comment describes the OLD
	// behavior) with a denial, exactly like a linked-but-insufficient-role
	// decidedBy, but told apart in its text.
	//
	// None of the three answers touches the approval message: each goes to
	// the clicking user alone (replyPrivately), and the plan and its
	// buttons stay for the next click. updateMessage's chat.update carries
	// no "blocks" field (slackapi.Client.UpdateMessage's own doc comment),
	// which Slack's API treats as "strip every block from this message";
	// for a not-yet-linked actor that would take the buttons the same
	// actor needs once linked (the HIGH audit fix, SECOND review pass), and
	// for a refused actor it would take them from every member who could
	// decide -- the message is the channel's, not the clicker's.
	if err := deps.authorizeSessionAction(decideCtx, logger, sessionID, decidedBy, authz.ActionApprovePlan); err != nil {
		switch {
		case errors.Is(err, ErrActorNotLinked):
			// The magic-link URL itself was already posted ephemerally above
			// (notice, from resolveSlackActorSingleAttempt) regardless of
			// this denial -- this second, distinct ephemeral message only
			// explains that THIS click specifically wasn't recorded, so the
			// actor knows to link first and then click Approve/Reject again.
			logger.Warn("slack: interactivity: plan decision denied, actor not yet linked", "plan_id", planIDStr, "session_id", sessionIDStr)
			deps.replyPrivately(decideCtx, logger, channel, slackUserID, messageTS, slackActorNotLinkedDecisionText)
		case errors.Is(err, ErrActorNotAuthorized):
			logger.Warn("slack: interactivity: plan decision denied by authz", "plan_id", planIDStr, "session_id", sessionIDStr, "user_id", decidedBy.String())
			deps.replyPrivately(decideCtx, logger, channel, slackUserID, messageTS, slackPlanForbiddenText)
		default:
			// MEDIUM audit fix ("Slack's interactive.go has its OWN separate,
			// still-unfixed copy" of authorizeSessionAction's conflation): a
			// genuine backend failure while checking authorization -- a
			// session or participant read that failed, or a role that could
			// not be read (errActorRoleUnreadable), each already logged -- is
			// NOT a denial: the same honest generic-error text the
			// DecidePlan-failure branch below uses, never "you don't have
			// permission". Clicking the button again, still there, is this
			// endpoint's own retry path (there is no webhook-delivery-claim/
			// release plumbing here to route into instead -- this file's own
			// top doc comment).
			deps.replyPrivately(decideCtx, logger, channel, slackUserID, messageTS, slackDecisionErrorText)
		}
		return
	}

	outcome, err := httpapi.DecidePlan(decideCtx, deps.Pool, deps.Sessions, deps.Turns, deps.Plans, deps.Events, deps.PlanDocuments, deps.Outbox, deps.LinearAgentSessions, deps.AuditLog, deps.Registry, sessionID, planID, verdict, decidedBy, deps.EpistemicCheckDefault)

	switch {
	case errors.Is(err, httpapi.ErrPlanCut):
		// The plan's text was cut on its way from the sandbox, so it cannot
		// be approved (httpapi.ErrPlanCut) -- a click on an Approve button
		// a binary built before cuts posted, since this one posts none for
		// a cut plan (slackapi.PostPlanApprovalMessage). The reason goes to
		// the clicking user alone, and the approval message is left as it
		// is: Request changes is the way on.
		logger.Info("slack: interactivity: approval refused, plan text was cut", "plan_id", planIDStr, "session_id", sessionIDStr)
		deps.replyPrivately(decideCtx, logger, channel, slackUserID, messageTS, err.Error())
	case errors.Is(err, httpapi.ErrPlanOpenTurnInFlight):
		// A turn of the session is still pending, dispatched or
		// processing. DecidePlanOnTx refuses before it reads the plan, so
		// the plan may still await approval -- its own revision is the
		// open turn, typically -- or be decided already, its own
		// implementation the open turn: answerOpenTurnRefusal reads it and
		// answers either.
		logger.Info("slack: interactivity: approval refused, a turn of the session is open", "plan_id", planIDStr, "session_id", sessionIDStr)
		deps.answerOpenTurnRefusal(decideCtx, logger, sessionID, planID, channel, slackUserID, messageTS)
	case err != nil:
		// Any other failure, the decision's own budget running out
		// included: the plan is as it was, unless the failure is the
		// commit's own -- and a commit that did land has enqueued the
		// message's own update (this function's doc comment).
		logger.Error("slack: interactivity: decide plan failed", "error", err, "plan_id", planIDStr, "session_id", sessionIDStr)
		deps.replyPrivately(decideCtx, logger, channel, slackUserID, messageTS, slackDecisionErrorText)
	default:
		deps.updateMessage(decideCtx, logger, channel, messageTS, renderPlanOutcomeText(outcome))
	}
}

// renderPlanOutcomeText renders outcome.FinalStatus honestly, whether THIS
// call won (Won == true, the verdict it itself just rendered) or the plan
// was already decided by a different entry point (Won == false) -- see
// decideAndUpdateMessage's own doc comment for why both cases update the
// SAME message with the real truth, never a stale "pending" state.
func renderPlanOutcomeText(outcome httpapi.DecidePlanOutcome) string {
	switch outcome.FinalStatus {
	case "approved":
		if outcome.Won {
			return "✅ Approved — implementation started."
		}
		return "✅ Already approved (via a different channel)."
	case "rejected":
		if outcome.Won {
			return "❌ Rejected."
		}
		return "❌ Already rejected (via a different channel)."
	case "superseded":
		return "This plan was superseded by a newer revision."
	default:
		return "This plan is no longer awaiting approval."
	}
}

// replyContext returns the context a reply to a click runs on: detached
// from decideCtx by context.WithoutCancel, which keeps its values (the
// logger, the correlation id) and drops its deadline and cancellation, so a
// decision that ran out of its share is still answered; and bounded by
// SlackInteractivityReplyTimeout from the moment the reply starts, after
// the decision has ended, so the decision and the reply together fit
// SlackInteractivityAckTimeout (that field's own doc comment).
func (deps InteractiveDeps) replyContext(decideCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(decideCtx), deps.Timeouts.SlackInteractivityReplyTimeout)
}

// replyPrivately answers a click that left the plan awaiting approval to
// the clicking user alone: a chat.postEphemeral threaded on the approval
// message (postPrivately), on replyContext's budget. It never touches the
// approval message, so the plan and its buttons stay for the next click. A
// failure is logged and reaches nobody; the buttons are still there, and a
// second click is the retry.
func (deps InteractiveDeps) replyPrivately(decideCtx context.Context, logger *slog.Logger, channel, slackUserID, messageTS, text string) {
	replyCtx, cancel := deps.replyContext(decideCtx)
	defer cancel()
	deps.postPrivately(replyCtx, logger, channel, slackUserID, messageTS, text)
}

// postPrivately is replyPrivately's send, on a reply context its caller
// already holds.
func (deps InteractiveDeps) postPrivately(replyCtx context.Context, logger *slog.Logger, channel, slackUserID, messageTS, text string) {
	if err := deps.SlackClient.PostEphemeral(replyCtx, channel, slackUserID, messageTS, text); err != nil {
		logger.Warn("slack: interactivity: private reply to a plan click failed, the approval message is left as it was", "error", err)
	}
}

// updateMessage shows an outcome on the approval message itself: a
// chat.update (showOutcome), on replyContext's budget, whose text replaces
// the plan and whose missing blocks remove its buttons
// (slackapi.Client.UpdateMessage's own doc comment). Only an outcome comes
// here -- a plan this click decided, or one already decided or superseded
// -- since only then do the buttons have nothing left to do; every other
// answer goes through replyPrivately. A failure is logged: a decision that
// committed has also enqueued the message's own update
// (decideAndUpdateMessage's doc comment).
func (deps InteractiveDeps) updateMessage(decideCtx context.Context, logger *slog.Logger, channel, messageTS, text string) {
	replyCtx, cancel := deps.replyContext(decideCtx)
	defer cancel()
	deps.showOutcome(replyCtx, logger, channel, messageTS, text)
}

// showOutcome is updateMessage's send, on a reply context its caller
// already holds.
func (deps InteractiveDeps) showOutcome(replyCtx context.Context, logger *slog.Logger, channel, messageTS, text string) {
	if channel == "" || messageTS == "" {
		logger.Warn("slack: interactivity: missing channel/message ts, skipping chat.update")
		return
	}
	if err := deps.SlackClient.UpdateMessage(replyCtx, channel, messageTS, text); err != nil {
		logger.Warn("slack: interactivity: chat.update failed", "error", err)
	}
}

// answerOpenTurnRefusal answers an Approve click DecidePlan refused because
// a turn of the session is pending, dispatched or processing
// (httpapi.ErrPlanOpenTurnInFlight). DecidePlanOnTx runs that gate before
// it reads the plan, so the refusal says nothing about the plan's own
// status: right after an approval, the plan's own implementation is the
// open turn. So the plan is read again, the read and the answer sharing one
// replyContext, so the two together stay inside the reply's budget:
//   - still awaiting approval: the busy text, to the clicker alone, the
//     plan and its buttons left for a click once that turn ends;
//   - decided or superseded: its outcome, shown on the message through
//     chat.update, as a Reject click on the same message -- which that gate
//     never refuses -- shows it;
//   - not this session's plan, or gone: answered as DecidePlan answers
//     such a plan, "no longer awaiting approval", never leaking another
//     session's status;
//   - a read that fails: the busy text, still true of this click.
//
// The REST, MCP and Linear answers to the same refusal are untouched: this
// is the Slack button's own reading of it.
func (deps InteractiveDeps) answerOpenTurnRefusal(decideCtx context.Context, logger *slog.Logger, sessionID, planID pgtype.UUID, channel, slackUserID, messageTS string) {
	replyCtx, cancel := deps.replyContext(decideCtx)
	defer cancel()

	plan, err := deps.Plans.Get(replyCtx, planID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		deps.showOutcome(replyCtx, logger, channel, messageTS, renderPlanOutcomeText(httpapi.DecidePlanOutcome{}))
	case err != nil:
		logger.Warn("slack: interactivity: read the plan after an open-turn refusal failed, answering with the busy text", "error", err, "plan_id", planID.String())
		deps.postPrivately(replyCtx, logger, channel, slackUserID, messageTS, slackPlanOpenTurnText)
	case plan.SessionID != sessionID:
		deps.showOutcome(replyCtx, logger, channel, messageTS, renderPlanOutcomeText(httpapi.DecidePlanOutcome{}))
	case plan.Status == sqlcgen.PlanStatusAwaitingApproval:
		deps.postPrivately(replyCtx, logger, channel, slackUserID, messageTS, slackPlanOpenTurnText)
	default:
		deps.showOutcome(replyCtx, logger, channel, messageTS, renderPlanOutcomeText(httpapi.DecidePlanOutcome{FinalStatus: string(plan.Status)}))
	}
}

// openRequestChangesModal calls views.open using triggerID from the
// inbound block_actions payload that just fired -- Slack's own trigger_id
// is valid for only a few seconds, so this must run promptly (no slow work
// before it, matching handleBlockActions's own fast-path requirement).
// Bounded by SlackInteractivityAckTimeout, not SlackAckTimeout: this call is
// on the SAME block_actions interactivity-ack path decideAndUpdateMessage's
// own doc comment describes (a single, real outbound API call, so it was
// already comfortably inside either budget), reused here for consistency
// now that a dedicated, correctly-scoped constant exists for this path.
func (deps InteractiveDeps) openRequestChangesModal(ctx context.Context, logger *slog.Logger, triggerID, planIDStr, sessionIDStr string) {
	if triggerID == "" {
		logger.Warn("slack: interactivity: request_changes_plan action with no trigger_id, cannot open modal")
		return
	}
	openCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.SlackInteractivityAckTimeout)
	defer cancel()
	if err := deps.SlackClient.OpenView(openCtx, triggerID, planIDStr, sessionIDStr); err != nil {
		logger.Error("slack: interactivity: views.open failed", "error", err)
	}
}

// viewSubmissionErrorResponse is Slack's own documented shape for
// rejecting a view_submission with an inline validation error rather than
// silently closing the modal (docs.slack.dev/surfaces/modals's own
// "Responding to a view_submission" — "response_action": "errors" plus a
// block_id -> message map) -- used here for §13.2's own ("identities +
// full RBAC", §13.2/§13.3) "reply with a clear denial message" fix: this
// payload type has no channel/thread to post an ordinary message into at
// all (see handleViewSubmission's own doc comment on why an identity
// notice is already skipped here for the identical reason), so Slack's
// own inline-modal-error mechanism is this path's equivalent of the REST
// API's 403.
type viewSubmissionErrorResponse struct {
	ResponseAction string            `json:"response_action"`
	Errors         map[string]string `json:"errors"`
}

// writeViewSubmissionError is the ONE builder of handleViewSubmission's
// inline modal error: a 200 carrying viewSubmissionErrorResponse with text
// keyed on the modal's single input block (slackapi.RequestChangesBlockID,
// the block_id OpenView renders), so Slack keeps the modal open, the
// feedback still in the field, and shows text under that field. Every
// refusal of the submission -- blank feedback, an authorization denial or
// its failure, a turn already open, a failed create -- answers through it.
func writeViewSubmissionError(w http.ResponseWriter, text string) {
	writeJSON(w, http.StatusOK, viewSubmissionErrorResponse{
		ResponseAction: "errors",
		Errors: map[string]string{
			slackapi.RequestChangesBlockID: text,
		},
	})
}

// requestChangesRefusalText picks the modal text for a CreateTurnCore
// failure: the busy text when the refusal is RejectIfOpen's open-turn 409,
// recognized by its sentinel (httpapi.ErrTurnAlreadyOpen) and never by its
// Message or its Status alone, and the generic error text for anything
// else.
func requestChangesRefusalText(cerr *httpapi.CreateTurnError) string {
	if errors.Is(cerr, httpapi.ErrTurnAlreadyOpen) {
		return slackRequestChangesBusyErrorText
	}
	return slackRequestChangesErrorText
}

// handleViewSubmission processes the "Request changes" modal's own
// submission: the feedback text (read back from view.state.values, keyed
// by slackapi.RequestChangesBlockID/RequestChangesActionID) becomes a new
// plan_mode=true turn's prompt, via httpapi.CreateTurnCore -- the EXACT
// SAME function POST .../turns itself calls (turn.go), never a third,
// duplicated turn-creation call site. planId is decoded from
// private_metadata only to validate the payload's own shape; the actual
// turn creation only needs sessionId (a plan's own identity is not
// re-checked here -- creating a plan_mode=true "request changes" turn is
// unconditionally safe regardless of the named plan's current status,
// exactly matching how the existing POST .../turns endpoint already
// behaves for every other "request changes" submission, §8.1's own
// design).
//
// w is §13.2's own addition: this function owns its own response entirely
// (NewInteractivityHandler's own switch writes none on this branch). A
// payload it cannot read is answered with a bare 200; a created turn with
// a bare 200 too, which closes the modal. Every refusal -- blank feedback,
// an authorization denial or a failure checking it, and a CreateTurnCore
// failure -- answers through writeViewSubmissionError instead, so the
// modal stays open with the feedback in it. A CreateTurnCore refusal
// because a turn is already open (RejectIfOpen's 409, technical plan
// §43.21(a)) says so and asks for the change once that turn ends; nothing
// is queued.
func (deps InteractiveDeps) handleViewSubmission(ctx context.Context, w http.ResponseWriter, logger *slog.Logger, raw []byte) {
	var payload viewSubmissionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		logger.Warn("slack: interactivity: decode view_submission payload failed", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	if payload.View.CallbackID != slackapi.RequestChangesCallbackID {
		logger.Info("slack: interactivity: unrecognized view_submission callback_id", "callback_id", payload.View.CallbackID)
		w.WriteHeader(http.StatusOK)
		return
	}

	// planIDStr is now ALSO kept (previously discarded via `_`) -- LOW audit
	// fix (SECOND review pass, "handleViewSubmission discards the
	// magic-link notice on denial"): postViewSubmissionLinkNotice below
	// needs it to look up this plan's own stored Slack channel/message-ts,
	// the only way this payload shape can scope an ephemeral notice at all
	// (see that function's own doc comment).
	planIDStr, sessionIDStr, ok := slackapi.DecodePlanActionValue(payload.View.PrivateMetadata)
	if !ok {
		logger.Warn("slack: interactivity: malformed private_metadata", "private_metadata", payload.View.PrivateMetadata)
		w.WriteHeader(http.StatusOK)
		return
	}

	var feedback string
	if block, ok := payload.View.State.Values[slackapi.RequestChangesBlockID]; ok {
		if elem, ok := block[slackapi.RequestChangesActionID]; ok && elem.Value != nil {
			feedback = *elem.Value
		}
	}
	if plandomain.IsBlankFeedback(feedback) {
		// Audit-remediation batch fix (CONFIRMED MEDIUM finding): this used
		// to log a Warn and write a bare 200 here, with NO response_action
		// -- Slack's own documented behavior for that shape is to close the
		// modal as though the submission had succeeded, so the submitter
		// had zero signal their "request changes" feedback was silently
		// discarded, unlike the new Slack/Linear revise: TEXT path
		// (handler.go/webhook.go), which now posts an honest reply for the
		// identical empty-feedback case. plandomain.IsBlankFeedback (LOW
		// audit fix) is used here rather than a bare strings.TrimSpace ==
		// "" check -- the shared definition also catches invisible
		// zero-width-character-only feedback, matching handler.go/
		// webhook.go's own identical guard exactly. Responds with
		// viewSubmissionErrorResponse instead -- Slack's own inline
		// validation-error mechanism, re-opening the modal with
		// slackEmptyFeedbackErrorText shown against the feedback field, so
		// the submitter can see why nothing happened and correct it.
		logger.Warn("slack: interactivity: empty feedback text in view_submission, rejecting")
		writeViewSubmissionError(w, slackEmptyFeedbackErrorText)
		return
	}

	var sessionID pgtype.UUID
	if err := sessionID.Scan(sessionIDStr); err != nil {
		logger.Warn("slack: interactivity: parse session id failed", "error", err, "session_id", sessionIDStr)
		w.WriteHeader(http.StatusOK)
		return
	}

	// §13.2 ("identities + full RBAC", §13.2) update: actorUserID is no
	// longer unconditionally invalid -- resolveSlackActorSingleAttempt
	// (identity.go) auto-links (or creates a magic-link prompt for) the
	// submitting Slack user the first time this package sees them,
	// without a multi-attempt retry (this modal-submission response has
	// its own tight ack requirement, mirroring decideAndUpdateMessage's
	// own identical reasoning).
	//
	// LOW audit fix (SECOND review pass, "handleViewSubmission discards the
	// magic-link notice on denial"): notice USED to be discarded outright
	// here (`actorUserID, _ := ...`) -- a confirmed 3-lens adversarial
	// review found that meant a submission denied because the actor isn't
	// linked yet got a generic "not authorized" modal error with NO
	// magic-link prompt anywhere, contradicting this batch's own "the
	// magic-link prompt is still sent on every denial" principle. notice is
	// now captured and, specifically for that denial case (below,
	// ErrActorNotLinked), delivered via postViewSubmissionLinkNotice --
	// this modal-submission payload STILL has no channel/message field of
	// its own the way an ordinary event does (unlike handler.go's own
	// handleEvent, which always has channel/key from the inbound event),
	// so that helper looks the plan's own already-stored Slack channel/
	// message-ts back up instead of relying on one being handed in here.
	//
	// A resolution that did not complete (resolveSlackActorSingleAttempt's
	// err) is a backend failure, not an unlinked submitter: the modal says
	// so with the generic error and stays open, never the permission text.
	actorUserID, notice, err := resolveSlackActorSingleAttempt(ctx, logger, deps.SlackClient, deps.IdentityLink, deps.Timeouts.SlackInteractivityIdentityFetchTimeout, payload.User.ID)
	if err != nil {
		logger.Error("slack: interactivity: request-changes turn not created, the submitting user's identity could not be resolved", "error", err, "session_id", sessionIDStr)
		writeViewSubmissionError(w, slackRequestChangesErrorText)
		return
	}

	// §13.2 ("identities + full RBAC", §13.2/§13.3) update: a resolved,
	// linked actorUserID must still pass domain/authz.
	// Authorize(ActionPromptSession) -- this "Request changes" turn is
	// exactly the same state-changing command POST .../turns itself gates
	// (turn.go's own authorize call).
	//
	// Audit-fix batch update ("block unlinked actor state changes"): a
	// still-unlinked (bot-attributed) actorUserID is NO LONGER let through
	// -- authorizeSessionAction now returns ErrActorNotLinked immediately
	// for that case (SECOND review pass: a distinct sentinel, checked
	// FIRST below), denying this submission exactly like a
	// linked-but-insufficient-role actorUserID (ErrActorNotAuthorized).
	if err := deps.authorizeSessionAction(ctx, logger, sessionID, actorUserID, authz.ActionPromptSession); err != nil {
		if errors.Is(err, ErrActorNotLinked) {
			logger.Warn("slack: interactivity: request-changes turn denied, actor not yet linked", "session_id", sessionIDStr)
			// LOW audit fix (SECOND review pass): deliver the magic-link
			// notice this denial's actor is otherwise never told about on
			// this payload shape -- see postViewSubmissionLinkNotice's own
			// doc comment and this function's own top doc comment on notice
			// above. Best-effort: never blocks the modal response below.
			deps.postViewSubmissionLinkNotice(ctx, logger, planIDStr, payload.User.ID, notice)
			writeViewSubmissionError(w, slackPromptForbiddenErrorText)
			return
		}
		if errors.Is(err, ErrActorNotAuthorized) {
			logger.Warn("slack: interactivity: request-changes turn denied by authz", "session_id", sessionIDStr, "user_id", actorUserID.String())
			writeViewSubmissionError(w, slackPromptForbiddenErrorText)
			return
		}
		// MEDIUM audit fix ("Slack's interactive.go has its OWN separate,
		// still-unfixed copy" of authorizeSessionAction's conflation): a
		// genuine backend failure while checking authorization (already
		// logged inside authorizeSessionAction) is NOT a denial -- show the
		// honest generic-error text via the same modal-error mechanism,
		// instead of misreporting an internal error as "you don't have
		// permission" and silently discarding the submitter's feedback text.
		// Resubmitting the modal is this endpoint's own retry path.
		writeViewSubmissionError(w, slackRequestChangesErrorText)
		return
	}

	// intentSvc: nil here, not deps.IntentClassifier -- this call always
	// passes planMode=true (this endpoint IS the Request-changes modal
	// submission, i.e. already a plan-revision turn by construction), so
	// createTurnLocked's own plan_followup block (turn.go, guarded on
	// !planMode) never runs regardless of what's passed. InteractiveDeps
	// carries no IntentClassifier field at all (unlike Deps, handler.go) --
	// adding one purely to thread an argument that would never be consulted
	// here would be dead plumbing.
	//
	// A failure never closes the modal (technical plan §43.21(b)): the
	// open-turn refusal and every other failure both answer through
	// writeViewSubmissionError, with the text requestChangesRefusalText
	// picks, so the feedback stays in the field. Nothing is queued. Warn,
	// not Error: CreateTurnCore already logs its own internal failures at
	// Error, and an open turn is an expected state, not a fault.
	if _, _, cerr := httpapi.CreateTurnCore(ctx, deps.Pool, deps.Sessions, deps.Turns, deps.Plans, nil, deps.AuditLog, deps.Registry, sessionID, feedback, nil, true, deps.EpistemicCheckDefault, actorUserID, httpapi.RejectIfOpen); cerr != nil {
		logger.Warn("slack: interactivity: create request-changes turn failed", "status", cerr.Status, "message", cerr.Message, "session_id", sessionIDStr)
		writeViewSubmissionError(w, requestChangesRefusalText(cerr))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// postViewSubmissionLinkNotice is handleViewSubmission's own best-effort
// delivery of notice (the magic-link URL, or the "connected your account"
// confirmation -- empty when there's nothing to say) for the SPECIFIC
// "actor not yet linked" denial (ErrActorNotLinked) -- LOW audit fix
// (SECOND review pass, "handleViewSubmission discards the magic-link
// notice on denial"). A view_submission payload carries no channel/
// message-ts of its own the way an ordinary block_actions click does
// (blockActionsPayload's own Channel/Message fields have no view_submission
// equivalent -- verified against Slack's own reference docs during the
// original Step's investigation, see this file's own top doc comment), so
// this looks planIDStr's own stored Slack channel/message-ts back up
// (PlanStore.SetSlackMessageRef, populated when the approval message this
// modal's own "Request changes" button lives on was first posted via
// PostPlanApprovalMessage) to scope a chat.postEphemeral call to --
// mirroring decideAndUpdateMessage's own identical ephemeral-delivery
// pattern for this exact kind of sensitive, single-user-scoped text.
//
// A deliberate no-op (never an error, never escalated) when: notice is
// empty (nothing to say); planIDStr fails to parse or look up (should be
// unreachable -- it was already validated shaped-correctly by
// DecodePlanActionValue above, and the plan it names is the SAME one this
// modal's own private_metadata was built from); or the plan has no stored
// Slack message ref at all (e.g., a defensive fallback -- every REAL
// Slack-originated plan approval message sets this, but this function
// never assumes it). Every failure path is logged and swallowed -- the
// same still-unlinked identity still gets this SAME notice the next time
// any OTHER event from it arrives (resolveSlackActor's own general
// algorithm, identity.go), so silently skipping delivery here never loses
// the notice permanently, only defers it.
func (deps InteractiveDeps) postViewSubmissionLinkNotice(ctx context.Context, logger *slog.Logger, planIDStr, slackUserID, notice string) {
	if notice == "" {
		return
	}

	var planID pgtype.UUID
	if err := planID.Scan(planIDStr); err != nil {
		logger.Warn("slack: interactivity: parse plan id for link-notice delivery failed", "error", err, "plan_id", planIDStr)
		return
	}

	plan, err := deps.Plans.Get(ctx, planID)
	if err != nil {
		logger.Warn("slack: interactivity: look up plan for link-notice delivery failed", "error", err, "plan_id", planIDStr)
		return
	}
	if plan.SlackChannelID == nil || plan.SlackMessageTs == nil || *plan.SlackChannelID == "" || *plan.SlackMessageTs == "" {
		logger.Warn("slack: interactivity: plan has no stored Slack message ref, skipping link-notice delivery", "plan_id", planIDStr)
		return
	}

	if err := deps.SlackClient.PostIdentityLinkNotice(ctx, *plan.SlackChannelID, slackUserID, *plan.SlackMessageTs, notice); err != nil {
		logger.Warn("slack: interactivity: post identity-link ephemeral notice failed", "error", err)
	}
}
