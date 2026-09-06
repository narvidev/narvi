package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/internal/app/intentclassifier"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/shadowlinear"
	"github.com/narvidev/narvi/internal/domain/authz"
	intentdomain "github.com/narvidev/narvi/internal/domain/intent"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/platform"
)

// intentClassifierSurface is the sessions.spawn_source value (§18.1's
// IntentClassifierInput.Surface / §18.4's IntentDecisionRecord.Surface)
// this package's own agent sessions are classified/recorded under.
const intentClassifierSurface = "linear"

// maxRequestBodyBytes bounds the webhook body this handler reads --
// mirrors internal/adapters/inbound/httpapi's own identical constant
// (a package-private copy, not shared, matching this codebase's own
// per-package convention for this exact constant already established by
// that package).
const maxRequestBodyBytes = 1 << 20 // 1 MiB

// acknowledgmentBody is the fixed text of the single `thought` Agent
// Activity this Step posts back immediately after creating a session --
// see internal/adapters/outbound/linearapi's own doc.go for why this is
// the one, minimal, direct outbound call this Step makes.
const acknowledgmentBody = "Narvi has started working on this."

// busyReplyText is the M6 audit fix's own honest reply, posted back to
// the thread when handlePrompted's own ordinary-reply path (below) drops
// a reply because a turn is already open for this session -- PREVIOUSLY
// this case was only ever logged, with NO visible response to the user at
// all (worse than Slack's own false "I'll pick this up next" promise,
// which at least said something). Mirrors Slack's own ackBusyText
// wording/tone (internal/adapters/inbound/slack/handler.go).
const busyReplyText = "Still working on the previous message in this thread — this reply wasn't queued, please try again once it's done."

// stopNotSupportedText is the L7 audit fix's own minimal, honest reply for
// Linear's `stop` signal (see handlePrompted's own doc comment on the
// stop-signal branch) -- posted instead of the PREVIOUS silent
// log-and-discard. Narrow scope only: this does NOT implement any real
// turn/session cancellation (that remains separate, later work) -- it
// only tells the user the truth instead of nothing at all.
const stopNotSupportedText = "Stopping an in-progress turn isn't supported yet — this request wasn't cancelled."

// planAwaitingApprovalReplyText is this batch's own honest reply
// (§8.1 follow-up fix), posted back to the thread when
// handlePrompted's own ordinary-reply path declines to create a build turn
// because sessionID currently has a plan in StatusAwaitingApproval --
// CLOSING the hole where a reply matching neither plandomain.MatchVerdict
// nor plandomain.MatchRevise previously fell through into an ordinary,
// plan_mode=false turn the human never approved. Built (like
// planApprovalLinearText, internal/app/sessionactor/outboxenqueue.go) off
// the SAME plandomain.ApproveKeywords/RejectKeywords/RevisePrefix exports
// the parsing side (MatchVerdict/MatchRevise) itself checks against, so the
// instructions here can never drift out of sync with what is actually
// accepted.
var planAwaitingApprovalReplyText = fmt.Sprintf(
	"A plan is awaiting your approval for this session. Reply %s to approve it, %s to reject it, or start your reply with %q to request changes.",
	strings.Join(plandomain.ApproveKeywords, "/"),
	strings.Join(plandomain.RejectKeywords, "/"),
	plandomain.RevisePrefix,
)

// emptyReviseFeedbackReplyText is the audit-remediation batch's own SECOND
// fix-pass addition (LOW audit finding, "the honest reply reused for the
// new empty-feedback case is generic boilerplate ... gives the user no
// indication that their revise: reply WAS recognized ... but was rejected
// specifically because the feedback was empty"): posted instead of
// planAwaitingApprovalReplyText specifically for the emptyReviseFeedback
// case (handlePrompted, below) -- a reply that DID match
// plandomain.RevisePrefix, unlike every other case
// planAwaitingApprovalReplyText itself still covers (a reply matching
// neither an approve/reject keyword nor the revise: prefix at all). Mirrors
// Slack's identical ackEmptyReviseFeedbackText (handler.go).
var emptyReviseFeedbackReplyText = fmt.Sprintf(
	"Your %q reply was recognized, but no feedback followed it — reply again with your requested changes after %q.",
	plandomain.RevisePrefix, plandomain.RevisePrefix,
)

// Deps bundles every dependency NewWebhookHandler needs -- a plain struct
// (rather than 10+ positional constructor parameters) since this handler
// genuinely needs this many collaborators: the webhook toolkit pieces
// (§5.1), the full session-creation path (CreateSessionCore), the
// Linear-specific dedupe/installation stores, and the outbound client.
type Deps struct {
	Pool          *pgxpool.Pool
	Sessions      *postgres.SessionStore
	Turns         *postgres.TurnStore
	Environments  *postgres.EnvironmentStore
	Registry      *sessionactor.Registry
	Deliveries    *postgres.WebhookDeliveryStore
	AgentSessions *postgres.LinearAgentSessionStore
	Installations *postgres.LinearInstallationStore
	// LinearClient is typed as the shadowlinear.Client interface, never
	// the concrete *linearapi.Client (§30.3's "mutation methods behind
	// decorated interfaces") -- production wiring (cmd/control-plane/
	// main.go) hands over a shadowlinear.Decorator wrapping the raw
	// *linearapi.Client every OTHER caller in that binary (the OAuth
	// install callback, the outbox's own LinearNotifier -- both reads or
	// already covered by §30.2's outbox classification) still uses
	// directly.
	LinearClient shadowlinear.Client

	// Plans/Outbox are §8.1's ("plan mode, cross-channel", §8.1/§13.3)
	// own additions -- handlePrompted's new plan-verdict keyword check
	// (below) needs Plans to find this session's own awaiting_approval
	// plan (if any) and, alongside AgentSessions/Registry above, to call
	// the shared httpapi.DecidePlan; Outbox is DecidePlan's own
	// cross-channel-notify dependency (decideplan.go).
	Plans  *postgres.PlanStore
	Outbox *postgres.OutboxStore

	// Events/PlanDocuments (§31.3) -- handlePrompted's own
	// httpapi.DecidePlan call (below) needs these for its own
	// approved-plan snapshot dependency (decideplan.go), mirroring
	// Slack's identical Deps.Events/Deps.PlanDocuments (handler.go).
	Events        *postgres.EventStore
	PlanDocuments *postgres.PlanDocumentStore

	// Participants is §13.2's own addition ("identities + full RBAC",
	// §13.2/§13.3) -- identity.go's own authorizeSessionAction/ownedOrJoined
	// need this to resolve a `member` actor's own "own/joined" carve-out
	// exactly like httpapi's canActOnPlan/CreateTurn already do, so a
	// Linear-decided plan verdict or ordinary reply ("request changes")
	// renders the IDENTICAL §13.3 verdict a REST caller would for the same
	// (actor, session).
	Participants *postgres.ParticipantStore

	// AuditLog is §13.2's own addition (§13.3) -- threaded through to
	// httpapi.CreateSessionCore/DecidePlan below exactly like Plans/Outbox
	// already are, so a Linear-originated session creation or plan
	// decision gets the SAME audit_log row every other caller of those two
	// shared functions now gets. actor_user_id is NULL only until identity
	// auto-linking (IdentityLink below) resolves a real user -- see this
	// file's own resolveActor for the replacement of the old unconditional
	// bot-attribution precedent.
	AuditLog *postgres.AuditLogStore

	// IdentityLink is §13.2's own auto-linking wiring (§13.2): resolves
	// a Linear user id (AgentSession.CreatorID for a `created` event,
	// AgentActivity.UserID for a `prompted` one) to a real Narvi user_id,
	// auto-linking or creating a magic-link prompt the first time this
	// package sees a given Linear user id it doesn't already know about.
	// See resolveActor's own doc comment for the full replacement of this
	// package's previous unconditional bot-attribution behavior.
	IdentityLink identitylink.Deps

	// IntentClassifier is §8.3's own wiring point (§8.3/§18): classify
	// + record runs ONCE, right after a `created` AgentSessionEvent's own
	// winning claim creates the backing session (decided_at_stage="create"
	// -- the full prompt text is already available at that point, via
	// payload.PromptContext). A `prompted` event on an already-backed
	// session never re-classifies. Optional (nil-safe): a nil
	// IntentClassifier simply skips classification entirely.
	IntentClassifier *intentclassifier.Service

	// EpistemicCheckDefault ("builder epistemic pre-action
	// check", §20.4) is threaded through to handlePrompted's own
	// httpapi.CreateTurnCore call below exactly like every other caller
	// now gets -- production wiring (cmd/control-plane/main.go) passes
	// the SAME platform.Config.EpistemicCheckDefault value every other
	// caller does.
	EpistemicCheckDefault bool

	// RolloutMode/RepoSettings (§10 Phase 6, §32) are threaded
	// through to handleCreated's own httpapi.CreateSessionCore call below
	// exactly like EpistemicCheckDefault already is -- both are REQUIRED
	// parameters of that function now (its own doc comment), so a
	// zero-value RolloutMode here is indistinguishable from
	// rollout.ModeOpen (the safe default) rather than an accidental gap.
	RolloutMode  platform.RolloutMode
	RepoSettings *postgres.RepoSettingsStore

	// PRSessions (§31.4) is the SAME further REQUIRED
	// httpapi.CreateSessionCore parameter every other caller now threads
	// through -- see RolloutMode/RepoSettings' own doc comment immediately
	// above for the identical "required, not optional" reasoning. Linear
	// sessions, like Slack's, always target the SAME operator-configured
	// default repo (DefaultRepoURL below), never a per-message human
	// choice, so this deployment's admin must ensure that repository has
	// had at least one real GitHub PR mention (github_pr_sessions row)
	// before Linear-originated sessions succeed.
	PRSessions *postgres.GitHubPRSessionStore

	WebhookSecret      []byte
	TokenEncryptionKey []byte
	DefaultRepoName    string
	DefaultRepoURL     string

	Timeouts platform.Timeouts

	// SessionIDSetter, when non-nil, is used INSTEAD of AgentSessions for
	// setSessionIDWithRetry's own retried call (retry.go) -- nil-safe:
	// every real caller (cmd/control-plane/main.go) leaves this unset,
	// falling back to AgentSessions itself (which already satisfies this
	// narrow interface), exactly as if this field did not exist at all.
	// Exists ONLY so this package's own tests can substitute a fake that
	// fails SetSessionID a controlled number of times before delegating to
	// a real store, without needing to also fake Claim/Release/
	// GetByAgentSessionID (mirrors github's own PullRequestResolver
	// nil-safe-fallback precedent, headresolve.go).
	SessionIDSetter sessionIDSetter
}

// NewWebhookHandler backs POST /webhooks/linear: verifies Linear's own
// real webhook signature (signature.go), dedupes via
// Deliveries.Claim (provider "linear"), and routes an AgentSessionEvent
// to CreateSessionCore (a `created` action) or an existing session/turn
// (a `prompted` action) -- see doc.go for the full design and payload.go
// for the exact wire shapes this parses.
//
// Response codes: 401 on a bad/missing signature or an expired
// webhookTimestamp (fail closed -- Linear's own request is never trusted
// enough to even parse further); 400 on a malformed body or a missing
// Linear-Delivery header (this handler refuses to process anything it
// cannot dedupe); 200 for a duplicate delivery, an ignored event category,
// or a genuinely handled `created`/`prompted` event (including a
// deliberate business decision inside it, like an authz denial -- see
// handleCreated's/handlePrompted's own doc comments for which branches
// those are); 500, WITH the webhook-delivery claim released
// (WebhookDeliveryStore.Release), for a genuine post-claim processing
// failure (a DB error resolving/creating the session or turn) --
// H2 audit fix ("webhook claim/release parity"), correcting this
// comment's own previous, factually stale claim that no such release
// mechanism exists: WebhookDeliveryStore.Release already exists
// (postgres/webhookdelivery_store.go) and github's own handler.go
// already uses it identically. Releasing lets a redelivery of this same
// Linear-Delivery id actually retry, rather than the event being silently
// and permanently dropped now that it's claimed.
func NewWebhookHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Signature verified over the RAW body, before any JSON parsing --
		// see signature.go's own doc comment for the full scheme
		// confirmation. Fails closed: any error (missing/malformed header,
		// mismatched HMAC) is 401, the body is never even unmarshaled.
		if err := verifySignatureHeader(deps.WebhookSecret, rawBody, signatureHeaderFrom(r)); err != nil {
			logger.Warn("linear: webhook rejected: signature verification failed", "error", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var payload agentSessionEventWebhookPayload
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			logger.Warn("linear: webhook rejected: malformed body", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// webhookTimestamp is milliseconds (Linear's own schema -- see
		// payload.go's own doc comment); platform.VerifyWebhookTimestamp
		// takes unix SECONDS, so convert before checking. Checked AFTER
		// the signature (Linear's own worked example does the same order),
		// against LinearWebhookTimestampWindow (60s, Linear's own explicit
		// recommendation -- NOT the generic, wider
		// WebhookTimestampFreshnessWindow).
		webhookTimestampSeconds := int64(payload.WebhookTimestamp / 1000)
		if err := platform.VerifyWebhookTimestamp(webhookTimestampSeconds, time.Now(), deps.Timeouts.LinearWebhookTimestampWindow); err != nil {
			logger.Warn("linear: webhook rejected: stale timestamp", "error", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		deliveryID := deliveryIDFrom(r)
		if deliveryID == "" {
			logger.Warn("linear: webhook rejected: missing Linear-Delivery header")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		claim, err := deps.Deliveries.Claim(ctx, "linear", deliveryID)
		if err != nil {
			logger.Error("linear: claim webhook delivery failed", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !claim.Inserted {
			logger.Info("linear: duplicate webhook delivery, skipping", "delivery_id", deliveryID)
			w.WriteHeader(http.StatusOK)
			return
		}

		// From here on, the delivery is claimed. H2 audit fix ("webhook
		// claim/release parity"): handleCreated/handlePrompted below report
		// ok=false ONLY for a genuine post-claim processing failure -- see
		// each one's own doc comment for the exact ok=false branches --
		// which this func releases the claim for and answers non-2xx,
		// mirroring github's own identical release-on-failure pattern
		// (handler.go), so a redelivery of this same Linear-Delivery id can
		// actually retry rather than being silently skipped forever as an
		// already-claimed duplicate.
		eventType := eventTypeFrom(r)
		if eventType != agentSessionEventType && payload.Type != agentSessionEventType {
			logger.Info("linear: ignoring non-AgentSessionEvent webhook category", "event_type", eventType)
			w.WriteHeader(http.StatusOK)
			return
		}

		ok := true
		switch payload.Action {
		case "created":
			ok = deps.handleCreated(ctx, payload)
		case "prompted":
			ok = deps.handlePrompted(ctx, payload)
		default:
			logger.Warn("linear: unrecognized AgentSessionEvent action, ignoring", "action", payload.Action)
		}

		if !ok {
			if releaseErr := deps.Deliveries.Release(ctx, "linear", deliveryID); releaseErr != nil {
				logger.Error("linear: release webhook delivery claim failed", "error", releaseErr, "delivery_id", deliveryID)
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	}
}

// handleCreated processes a `created` AgentSessionEvent: claims this
// Linear agent session's own identity (first-writer-wins -- see
// migrations/000030_linear_agent_sessions.up.sql's own doc comment),
// and -- only for the winner -- creates the backing Narvi session via
// CreateSessionCore, attaches the resulting session id back onto the
// claimed row, and posts the minimal acknowledgment Agent Activity.
//
// Returns ok=false ONLY for a genuine post-(webhook-delivery-)claim
// processing failure that happens BEFORE any session/turn exists at all --
// H2 audit fix ("webhook claim/release parity"): the AgentSessions.Claim
// call itself erroring, or a CreateSessionCore error. The caller
// (NewWebhookHandler) releases the WEBHOOK-DELIVERY claim and answers
// non-2xx on ok=false, so a redelivery of this same Linear-Delivery id can
// retry -- safe here specifically because nothing has been created or
// dispatched yet, so a redelivery-triggered re-run of this whole function
// can only ever produce ONE session, never a duplicate. Every other return
// (a duplicate `created` event, an authz denial, and -- see below -- a
// SetSessionID failure) is ok=true: either a deliberate business decision
// (retrying would render the identical duplicate/denial verdict again,
// mirroring github's own ErrActorNotAuthorized-vs-genuine-error
// distinction, coalesce.go) or, for SetSessionID, a case where a retry via
// redelivery would actively make things WORSE (see below).
//
// Independently of the webhook-delivery claim above, THIS function also
// manages the SEPARATE linear_agent_sessions claim it wins via
// AgentSessions.Claim (H3 audit fix, same "webhook claim/release parity"
// finding): on the authz-denial and CreateSessionCore-error branches ONLY,
// it releases that claim too (AgentSessions.Release, guarded to a
// still-NULL session_id) -- otherwise EITHER would leave the row
// permanently stuck (NULL session_id forever), dropping every future
// prompt/redelivery for this agent_session_id regardless of what the
// webhook-delivery claim itself does. The authz-denial branch releases the
// AGENT-SESSION claim but deliberately does NOT release the webhook-
// delivery claim (still ok=true) -- these are genuinely different
// identities with different "would a retry help" answers: a redelivery of
// this SAME delivery id would hit the identical denial again (no help),
// but a LATER, distinct event for this SAME agent_session_id (e.g. a
// subsequent `prompted` event, once the actor is actually granted access)
// must not find the agent-session claim already permanently poisoned.
//
// A SetSessionID failure is deliberately NOT a third release branch (HIGH
// audit fix, "releasing the linear_agent_sessions claim after a
// SetSessionID failure can spawn a duplicate, independently-dispatched
// agent" -- correcting this function's own PREVIOUS behavior, which
// treated it exactly like the two branches above): by the time
// SetSessionID could ever fail, CreateSessionCore has ALREADY committed a
// real session with a Pending turn AND fired TriggerDispatch below -- the
// session is genuinely alive and already being worked on, NOT an inert,
// never-dispatched row the way Slack's own lost-the-race orphan is (that
// prior comparison was wrong: Slack's bare-session orphan never sets
// Prompt, so it is genuinely never dispatched; Linear's is). Releasing
// either claim here and answering non-2xx would let Linear redeliver this
// SAME `created` event, running this ENTIRE function again and spawning a
// SECOND, independently-dispatched session/turn for the identical
// agent_session_id, while the FIRST, real, already-running session becomes
// permanently unreachable by any future Linear event for it -- strictly
// worse than the gap it would claim to close. setSessionIDWithRetry
// (retry.go) instead retries the safe, idempotent UPDATE itself a bounded
// number of times; if every attempt still fails, this logs at Error (this
// specific agent_session_id now needs manual reconciliation -- its
// linear_agent_sessions row has no session_id even though a real,
// dispatched session exists) and continues on to the SAME success path
// (acknowledgment, intent classification, ok=true) as if SetSessionID had
// succeeded -- the task IS genuinely progressing from Linear's own
// perspective regardless.
func (deps Deps) handleCreated(ctx context.Context, payload agentSessionEventWebhookPayload) bool {
	logger := platform.Logger(ctx)

	claim, err := deps.AgentSessions.Claim(ctx, payload.AgentSession.ID, payload.OrganizationID)
	if err != nil {
		logger.Error("linear: claim agent session failed", "error", err, "agent_session_id", payload.AgentSession.ID)
		return false
	}
	if !claim.Inserted {
		logger.Info("linear: duplicate created event for agent session, skipping", "agent_session_id", payload.AgentSession.ID)
		return true
	}

	var title *string
	if payload.AgentSession.Issue != nil {
		t := fmt.Sprintf("%s: %s", payload.AgentSession.Issue.Identifier, payload.AgentSession.Issue.Title)
		title = &t
	}

	// promptContext is documented as present for every `created` event
	// (payload.go's own doc comment); defended against a nil value anyway
	// (never a naked nil-deref) with a short, honest fallback rather than
	// silently creating a session with no prompt at all.
	prompt := "Linear delegated or mentioned this agent session; no promptContext was supplied."
	if payload.PromptContext != nil {
		prompt = *payload.PromptContext
	}

	// Repos: see internal/platform/config.go's own
	// linearDefaultRepoNameEnvVarName doc comment for the full scope note
	// this stopgap operates under -- Linear's own AgentSessionEvent
	// payload carries no repository information at all, and every
	// CreateSessionRequest requires a non-empty Repos list regardless of
	// ingress surface.
	req := restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceLinear,
		Title:       restdtos.CreateSessionRequestTitle(title),
		Prompt:      restdtos.CreateSessionRequestPrompt(&prompt),
		Repos: []restdtos.CreateSessionRequestReposElem{
			{Name: deps.DefaultRepoName, Url: deps.DefaultRepoURL},
		},
	}

	// §13.2 ("identities + full RBAC", §13.2) update: creator is no
	// longer unconditionally invalid -- resolveActor auto-links (or
	// creates a magic-link prompt for) payload.AgentSession.CreatorID the
	// first time this package sees it, and reports back whichever
	// notification text (if any) the caller should surface. Nil CreatorID
	// (an automation-initiated session, "unset if automation-initiated"
	// per Linear's own schema) resolves to bot attribution unconditionally
	// -- there is no external id to look up at all in that case.
	creatorID := ""
	if payload.AgentSession.CreatorID != nil {
		creatorID = *payload.AgentSession.CreatorID
	}
	creator, notice := deps.resolveActor(ctx, logger, payload.OrganizationID, creatorID)

	// §13.2 ("identities + full RBAC", §13.2/§13.3) update: a creator
	// that resolved to a REAL, linked user_id must still pass domain/authz.
	// Authorize(ActionCreateSession) -- exactly what the REST /api/sessions
	// handler already requires (create.go's own authorize call). Resource{}
	// is always correct here (no ownership carve-out on create).
	//
	// Audit-fix batch update ("block unlinked actor state changes"): a
	// still-unlinked (bot-attributed) creator whose event DID carry a real
	// Linear user id (creatorID != "") is NO LONGER let through -- that is
	// exactly the "actor not yet linked" case this hardening targets
	// (resolveActor above already attempted to auto-link it and either
	// failed or minted a magic-link prompt), so it now denies via
	// actorauthz.AuthorizeLinkedActor instead of AuthorizeResolvedActor: the
	// magic-link prompt already sent means this same actor can simply retry
	// the identical delegation once their account is linked. See that
	// function's own doc comment for why this is the correct call here
	// (Linear has a pending-link mechanism GitHub does not).
	//
	// A genuinely automation-initiated session (creatorID == "", Linear's
	// own "unset if automation-initiated") is DELIBERATELY carved OUT of
	// this hardening -- it is not "an actor not yet linked" at all: there is
	// no external id to ever resolve, resolveActor's own short-circuit for
	// an empty externalID never even attempts a lookup, and no magic-link
	// prompt is ever minted for it (identical to today, pre-batch). Denying
	// it would have NO possible retry path -- unlike a real Linear user
	// whose auto-link genuinely failed, there is no human who could ever
	// click a link to unblock it -- so this specific case keeps calling the
	// unconditional AuthorizeResolvedActor, preserving its existing
	// bot-attributed pass-through exactly as before this batch (still
	// covered by TestWebhookHandler_Created_CreatesSessionAndTurn,
	// webhook_integration_test.go, which exercises exactly this shape).
	authorize := actorauthz.AuthorizeLinkedActor
	if creatorID == "" {
		authorize = actorauthz.AuthorizeResolvedActor
	}
	if !authorize(ctx, logger, authzSurface, deps.IdentityLink.Users, creator, authz.ActionCreateSession, authz.Resource{}) {
		logger.Warn("linear: create session denied by authz", "agent_session_id", payload.AgentSession.ID, "user_id", creator.String())
		// H3 audit fix: release the SEPARATE linear_agent_sessions claim
		// this call just won -- see this function's own top doc comment for
		// why this is safe/needed even though the webhook-delivery claim
		// below is deliberately left alone.
		if releaseErr := deps.AgentSessions.Release(ctx, payload.AgentSession.ID); releaseErr != nil {
			logger.Error("linear: release agent session claim failed", "error", releaseErr, "agent_session_id", payload.AgentSession.ID)
		}
		// Audit-fix batch update: wording no longer says "Your linked Narvi
		// account" -- that phrasing assumed a link already existed, which is
		// now wrong for the NEW denial case this same text also covers (an
		// actor whose auto-link attempt hasn't resolved at all). Reads
		// correctly either way; the appended notice (when non-empty) is what
		// tells an unlinked actor specifically how to fix it.
		deps.postAcknowledgment(ctx, payload.OrganizationID, payload.AgentSession.ID, "This actor is not authorized to start new sessions from Linear.", notice)
		return true
	}

	created, cerr := httpapi.CreateSessionCore(ctx, deps.Pool, deps.Sessions, deps.Turns, deps.Environments, deps.AuditLog, deps.Registry, req, creator, deps.EpistemicCheckDefault, deps.RolloutMode, deps.RepoSettings, deps.PRSessions)
	if cerr != nil {
		// (§10 Phase 6, §32): a RolloutRefusal is a PERMANENT
		// policy refusal, never a transient failure -- checked
		// structurally (CreateSessionError.RolloutRefusal), never by
		// string-matching cerr.Message. Releasing the delivery claim
		// below (the generic branch) would let Linear redelivery retry
		// this SAME agent_session_id FOREVER, reproducing the identical
		// refusal every time, since repo_settings.sessions_enabled does
		// not change between redeliveries of the same event. Takes the
		// SAME authz-denial shape the block immediately above this
		// function's own `if !authorize(...)` uses instead: acknowledge,
		// release ONLY the linear_agent_sessions claim (so a LATER,
		// genuinely different agent session for this same PR/repo can
		// still be created once the repo IS enrolled), and ack
		// terminally (return true, never releasing the webhook-delivery
		// claim).
		if cerr.RolloutRefusal {
			logger.Warn("linear: create session refused: repo not enrolled in cohort rollout", "agent_session_id", payload.AgentSession.ID)
			if releaseErr := deps.AgentSessions.Release(ctx, payload.AgentSession.ID); releaseErr != nil {
				logger.Error("linear: release agent session claim failed", "error", releaseErr, "agent_session_id", payload.AgentSession.ID)
			}
			deps.postAcknowledgment(ctx, payload.OrganizationID, payload.AgentSession.ID, "This repository is not yet enrolled in Narvi's session rollout.", notice)
			return true
		}

		logger.Error("linear: create session failed", "status", cerr.Status, "message", cerr.Message, "agent_session_id", payload.AgentSession.ID)
		// H2/H3 audit fix: release BOTH claims this delivery is holding --
		// the linear_agent_sessions claim (guarded, see this function's own
		// top doc comment) and, via this func's own false return, the
		// webhook-delivery claim (NewWebhookHandler) -- a transient DB
		// failure here must not permanently strand either one.
		if releaseErr := deps.AgentSessions.Release(ctx, payload.AgentSession.ID); releaseErr != nil {
			logger.Error("linear: release agent session claim failed", "error", releaseErr, "agent_session_id", payload.AgentSession.ID)
		}
		return false
	}

	if err := deps.setSessionIDWithRetry(ctx, payload.AgentSession.ID, created.ID); err != nil {
		// HIGH audit fix ("releasing the linear_agent_sessions claim after
		// a SetSessionID failure can spawn a duplicate, independently-
		// dispatched agent"): created.ID is a REAL, ALREADY-DISPATCHED
		// session (CreateSessionCore committed it and fired TriggerDispatch
		// before this point) -- setSessionIDWithRetry (retry.go) already
		// retried this safe, idempotent UPDATE a bounded number of times.
		// Every attempt failing means this specific agent_session_id's own
		// linear_agent_sessions row now needs MANUAL reconciliation (it has
		// no session_id even though a real, dispatched session exists) --
		// logged at Error for exactly that. The claim is deliberately left
		// exactly as it is: NEVER released here (unlike the two branches
		// above), since releasing it would let a redelivery of this
		// identical `created` event run this whole function again,
		// spawning a SECOND, independently-dispatched session for the same
		// agent_session_id while this first, real one becomes permanently
		// unreachable -- see this function's own top doc comment for the
		// full failure mode this replaces. Falls through to the SAME
		// success path (acknowledgment, intent classification, ok=true)
		// below as if SetSessionID had succeeded: the task IS genuinely
		// progressing from Linear's own perspective regardless, and
		// returning a failure code here would only trigger the exact
		// duplicate-dispatch redelivery this fix exists to prevent.
		logger.Error("linear: attach session id to agent session claim failed after exhausting retries, manual reconciliation needed",
			"error", err, "agent_session_id", payload.AgentSession.ID, "session_id", created.ID.String())
	}

	// §8.3 ("intent classifier", §8.3/§18): classify + record ONCE,
	// right here -- IntentDecisionRecord is a per-SESSION record (§18.4),
	// and every Linear-originated session is created exactly here, with
	// its full prompt text already in hand. Runs entirely OUTSIDE any
	// Postgres transaction (a real outbound LLM call must never hold one
	// open) and never blocks the caller's own acknowledgment beyond this
	// synchronous call -- shadow mode (§18.5, the default until a surface
	// is explicitly configured active) means nothing downstream yet
	// consumes the recorded Target/Mode for real behavior regardless.
	if deps.IntentClassifier != nil {
		// classify+record is now the shared intentclassifier.Service.
		// ClassifyAndRecord (H9/L11 audit fix) -- see that method's own
		// doc comment for the full "why a single shared call" reasoning.
		// This package's own DecidedAtStage matches GitHub's exactly
		// (DecidedAtStageCreate: the full prompt is always in hand at
		// session-create time), and it has no deterministic Target signal
		// of its own to supply (DeterministicTarget left empty).
		deps.IntentClassifier.ClassifyAndRecord(ctx, created.ID, ports.IntentClassifierInput{
			Text:    prompt,
			Surface: intentClassifierSurface,
		}, intentdomain.DecidedAtStageCreate)
	}

	logger.Info("linear: created session from agent session", "agent_session_id", payload.AgentSession.ID, "session_id", created.ID.String())

	deps.postAcknowledgment(ctx, payload.OrganizationID, payload.AgentSession.ID, acknowledgmentBody, notice)
	return true
}

// handlePrompted processes a `prompted` AgentSessionEvent: routes to the
// existing Narvi session this agent session already backs (never
// creating a second one), unless the event carries Linear's own "stop"
// signal.
//
// §8.1 ("plan mode, cross-channel", §8.1/§13.3) update: BEFORE the
// existing unconditional turn-creation below, this now checks whether
// sessionID currently has an awaiting_approval plan and, if so, matches
// the reply's own trimmed/lower-cased text against plandomain.MatchVerdict
// -- on a match, calls the SAME shared httpapi.DecidePlan every other entry
// point uses (never a duplicated decision path), then posts a follow-up
// AgentActivity confirming the REAL outcome (honest either way: this
// call's own verdict if it won, or "already decided elsewhere" if a
// different channel won first -- outcome.Won/outcome.FinalStatus report
// the truth). On NO match (including when there is no awaiting_approval
// plan at all), this falls through to the ordinary create-turn path -- this
// IS "request changes" (§8.1 already established that reusing ordinary
// turn-creation for feedback is correct). Audit-fix batch update: that
// create-turn path is no longer this function's own direct, unlocked
// deps.Turns.Create call (the L2 finding: a genuine check-then-act race) --
// it now goes through the SAME shared httpapi.CreateTurnCore core
// (DropIfOpen) every other turn-creation call site in this codebase uses,
// closing that race and adding the turn.create audit_log row (H7).
//
// Returns ok=false ONLY for a genuine post-(webhook-delivery-)claim
// backend failure -- H2 audit fix ("webhook claim/release parity"):
// GetByAgentSessionID erroring for any reason OTHER than pgx.ErrNoRows,
// httpapi.CreateTurnCore returning a non-nil *CreateTurnError, (MEDIUM
// audit fix, "authorizeSessionAction conflates a genuine backend error
// with a real authorization denial") authorizeSessionAction returning a
// genuine backend error (any error other than ErrActorNotAuthorized)
// while checking whether this reply's own actor may prompt sessionID, or
// (LOW audit fix, second review pass -- "handlePlanVerdict has the same
// conflation, explicitly left out of the first fix's scope") a plan-verdict
// reply's own call into handlePlanVerdict below returning ok=false for the
// identical reason. The caller (NewWebhookHandler) releases the
// webhook-delivery claim and answers non-2xx on ok=false, so a redelivery
// of this same Linear-Delivery id can retry. Every other return (missing
// agentActivity, a stop signal -- now with an honest reply, L7 -- an
// unknown/still-claiming agent session, a genuine ErrActorNotAuthorized
// denial -- ordinary-reply or plan-verdict -- an already-open turn -- now
// with an honest busy reply, M6, instead of a silent drop) is ok=true: a
// deliberate business decision or an accepted, already-documented scope
// limitation (see each branch's own comment), never a failure a retry
// could plausibly change.
func (deps Deps) handlePrompted(ctx context.Context, payload agentSessionEventWebhookPayload) bool {
	logger := platform.Logger(ctx)

	if payload.AgentActivity == nil {
		logger.Warn("linear: prompted event missing agentActivity, ignoring", "agent_session_id", payload.AgentSession.ID)
		return true
	}

	if payload.AgentActivity.Signal != nil && *payload.AgentActivity.Signal == stopSignal {
		// Scope decision (§8.10, narrowed further by the L7 audit fix
		// below): no session/turn STOP mechanism exists in
		// internal/app/sessionactor yet (confirmed during this Step's
		// investigation -- no Stop command type). Wiring a real stop
		// remains out of scope (separate, later work) -- but PREVIOUSLY
		// this was only ever logged, with no reply telling the user `stop`
		// isn't supported at all. L7 audit fix: post a minimal, honest
		// reply instead of silently discarding the signal.
		logger.Info("linear: received stop signal, replying that cancellation isn't supported yet", "agent_session_id", payload.AgentSession.ID)
		deps.postThoughtNotice(ctx, payload.OrganizationID, payload.AgentSession.ID, stopNotSupportedText, "")
		return true
	}

	row, err := deps.AgentSessions.GetByAgentSessionID(ctx, payload.AgentSession.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("linear: prompted event for unknown agent session, ignoring", "agent_session_id", payload.AgentSession.ID)
			return true
		}
		logger.Error("linear: look up agent session failed", "error", err, "agent_session_id", payload.AgentSession.ID)
		return false
	}
	if !row.SessionID.Valid {
		logger.Warn("linear: prompted event for agent session still being claimed, ignoring", "agent_session_id", payload.AgentSession.ID)
		return true
	}
	sessionID := row.SessionID

	// §13.2 ("identities + full RBAC", §13.2) update: resolve the REAL
	// actor behind this activity ONCE, regardless of which branch below
	// ends up handling it -- the auto-link algorithm runs "on first event
	// from an unknown provider identity" (§13.2), not only on plan-verdict
	// replies, so an ordinary reply must trigger it exactly the same way.
	// UserID is Linear's own REQUIRED "who authored this activity" field
	// (payload.go's own doc comment) -- never nil/empty the way
	// AgentSession.CreatorID can be, so resolveActor is always given a
	// real external id to look up here.
	actorUserID, notice := deps.resolveActor(ctx, logger, payload.OrganizationID, payload.AgentActivity.UserID)

	// prompt/planMode are overridden below (a follow-up fix, §8.1)
	// when this reply is a deterministic revise:-prefixed "request changes"
	// reply -- everything else about the ordinary-reply path below (the
	// authorize check, the shared CreateTurnCore call, its busy/gated
	// handling) stays identical for both an ordinary reply and a revise one,
	// since a revise reply is exactly the SAME create-a-turn action, just
	// with plan_mode=true and the stripped feedback as its prompt.
	prompt := payload.AgentActivity.Content.Body
	planMode := false
	// emptyReviseFeedback is the audit-remediation batch's own fix
	// ("revise: accepts empty feedback"): plandomain.MatchRevise documents
	// ok=true, feedback=="" for a bare "revise:" (or whitespace-only
	// feedback) as an EXPLICIT caller's-own-job case (verdict.go's own doc
	// comment: "deciding what to do with an empty feedback prompt is
	// entirely the caller's own job") -- this codebase already has an
	// answer to that question, at the pre-existing Slack "Request changes"
	// Block Kit modal submission (slack/interactive.go's own
	// handleViewSubmission, which this SAME batch's own follow-up fix now
	// ALSO makes reject empty feedback with a user-visible inline modal
	// error, instead of the bare "ignoring" 200 it used to write -- see
	// that function's own doc comment). This applies the SAME rule here
	// (treating whitespace-only feedback as empty too), rather than
	// silently dispatching a genuine plan_mode=true revision turn with
	// nothing at all for the agent to act on. Checked below, AFTER the
	// authorizeSessionAction gate further down (an unauthorized actor must
	// still be denied outright, regardless of what their reply's own
	// feedback contains).
	emptyReviseFeedback := false

	if deps.Plans != nil {
		if planID, hasAwaiting := deps.findAwaitingApprovalPlanID(ctx, logger, sessionID); hasAwaiting {
			if verdict, ok := plandomain.MatchVerdict(payload.AgentActivity.Content.Body); ok {
				// handlePlanVerdict's own OTHER internal failures
				// (DecidePlan erroring, the outcome-activity post failing)
				// are still logged and swallowed there, exactly like before
				// this batch's own H2/H3 changes -- the session this plan
				// belongs to already exists and is otherwise healthy, so a
				// failed plan-verdict DECISION is deliberately left out of
				// THIS function's own ok signal. LOW audit fix (second
				// review pass, "handlePlanVerdict has the same conflation,
				// explicitly left out of the first fix's scope"): a genuine
				// backend error from handlePlanVerdict's own
				// authorizeSessionAction call is NOT swallowed the same
				// way -- it now returns ok=false here too, exactly mirroring
				// this function's own ordinary-reply gate below, so it flows
				// into the same release-the-claim-and-retry path.
				return deps.handlePlanVerdict(ctx, logger, sessionID, planID, verdict, actorUserID, notice, payload.OrganizationID, payload.AgentSession.ID)
			}
			// a follow-up fix (§8.1): a reply matching neither a
			// verdict keyword NOR this deterministic revise: prefix falls
			// through unchanged below -- httpapi.CreateTurnCore's own
			// awaiting-plan gate (turn.go) is what actually declines that
			// case (planMode stays false), surfacing
			// httpapi.ErrPlanAwaitingApproval, handled below.
			if feedback, ok := plandomain.MatchRevise(payload.AgentActivity.Content.Body); ok {
				// plandomain.IsBlankFeedback (LOW audit fix, confirmed
				// finding "MatchRevise's feedback-emptiness check ... does
				// not treat zero-width characters as whitespace") replaces
				// a bare strings.TrimSpace(feedback) == "" check here --
				// the shared definition also catches feedback made up ONLY
				// of invisible zero-width runes (U+200B/200C/200D/FEFF),
				// which TrimSpace alone would let through as "non-empty".
				if plandomain.IsBlankFeedback(feedback) {
					emptyReviseFeedback = true
				} else {
					prompt = feedback
					planMode = true
				}
			}
		}
	}

	// §13.2 ("identities + full RBAC", §13.2/§13.3) update: this
	// fallthrough IS "request changes" for Linear (this function's own top
	// doc comment) -- the same state-changing command POST .../turns
	// itself gates behind ActionPromptSession (turn.go's own authorize
	// call). An actorUserID that resolved to a REAL, linked user_id must
	// pass that same check before this reply is allowed to create a turn.
	//
	// Audit-fix batch update ("block unlinked actor state changes"): a
	// still-unlinked (bot-attributed) actorUserID is NO LONGER let through
	// -- authorizeSessionAction (identity.go) now returns
	// ErrActorNotAuthorized immediately for that case too (its own
	// top-of-function short-circuit changed from "return nil" to "return
	// ErrActorNotAuthorized"), replacing §13.2's own original "the action
	// proceeds" precedent for the not-yet-linked case with a denial,
	// identical to a linked-but-insufficient-role actorUserID.
	// AgentActivity.UserID is Linear's own REQUIRED field (never empty the
	// way AgentSession.CreatorID can be, see resolveActor's own call
	// above) -- so there is no automation-initiated carve-out needed here
	// the way handleCreated's own creator gate needs one.
	if err := deps.authorizeSessionAction(ctx, logger, sessionID, actorUserID, authz.ActionPromptSession); err != nil {
		if errors.Is(err, ErrActorNotAuthorized) {
			logger.Warn("linear: prompted reply denied by authz", "session_id", sessionID.String(), "user_id", actorUserID.String())
			// Audit-fix batch update: wording no longer says "Your linked
			// Narvi account" -- that phrasing assumed a link already
			// existed, which is now wrong for the NEW denial case this same
			// text also covers (an actor whose auto-link attempt hasn't
			// resolved at all). Reads correctly either way; the appended
			// notice (when non-empty) is what tells an unlinked actor
			// specifically how to fix it.
			deps.postIdentityNotice(ctx, payload.OrganizationID, payload.AgentSession.ID, "This actor is not authorized to prompt this session.", notice)
			return true
		}
		// MEDIUM audit fix ("authorizeSessionAction conflates a genuine
		// backend error with a real authorization denial"): a transient
		// backend failure WHILE checking authorization (already logged
		// inside authorizeSessionAction) is NOT a denial -- flows into the
		// SAME release-the-claim-and-retry path H2 already wired up for
		// every other post-claim failure in this function, instead of
		// being silently treated as "skip, no release" the way a one-off
		// DB blip previously was.
		return false
	}

	if emptyReviseFeedback {
		// No CreateTurnCore call at all here -- unlike the
		// ErrPlanAwaitingApproval branch just below (an ordinary,
		// non-revise reply that CreateTurnCore's own awaiting-plan gate
		// declines), an empty-feedback revise: reply must never even reach
		// turn creation. LOW audit fix (SECOND fix-pass, confirmed finding
		// "the honest reply reused for the new empty-feedback case is
		// generic boilerplate"): this used to reuse the SAME
		// planAwaitingApprovalReplyText reply the ErrPlanAwaitingApproval
		// branch posts -- but that generic text reads identically whether
		// the revise: prefix was never used at all, or used with nothing
		// after it, giving the user no way to tell which happened. Posts
		// emptyReviseFeedbackReplyText instead, which explicitly confirms
		// the revise: prefix WAS recognized and says exactly what's
		// missing.
		//
		// LOW audit fix (confirmed finding, "log-level inconsistency
		// between the new empty-feedback-guard branch and the pre-existing
		// ... 'blocked by awaiting-approval plan' branch"): logged at
		// Info, matching the functionally identical ErrPlanAwaitingApproval
		// branch just below -- both are routine, expected user mistakes
		// that produce the exact same kind of honest reply and no adverse
		// system state, so neither deserves a higher severity than the
		// other. Previously Warn, which would flag this routine case above
		// the identical one below on any Warn-level alert.
		logger.Info("linear: revise reply had empty feedback, blocked by awaiting-approval plan guard", "session_id", sessionID.String())
		deps.postThoughtNotice(ctx, payload.OrganizationID, payload.AgentSession.ID, emptyReviseFeedbackReplyText, notice)
		return true
	}

	// Audit-fix batch update (L2/H7/L12/M6/L20): this ordinary-reply
	// insert used to call deps.Turns.Create DIRECTLY on the raw pool, with
	// NO transaction and NO lock at all -- a genuine check-then-act race
	// (L2): a concurrent turn-creation request, or a plan-mode
	// implementation-turn insert, could race the hasOpenTurn check this
	// path used to do just above the insert. It now goes through the SAME
	// shared httpapi.CreateTurnCore core (turn.go) every other
	// turn-creation call site in this codebase uses, with
	// httpapi.DropIfOpen as its own policy -- the core's own
	// GetActorEpochForUpdate row lock (held BEFORE its own hasOpenTurn-
	// equivalent check) is what actually closes L2, not a change to the
	// business rule itself: an already-open session still drops this
	// reply exactly as before, just without the race window. This also
	// closes H7 (the turn.create audit_log row is now written, with
	// actorUserID attributed) and L12 (this package's own copy-pasted
	// hasOpenTurn helper is gone entirely -- httpapi's own copy, already
	// unexported there, is the only one left).
	createdTurn, wasCreated, cerr := httpapi.CreateTurnCore(ctx, deps.Pool, deps.Sessions, deps.Turns, deps.Plans, deps.IntentClassifier, deps.AuditLog, deps.Registry, sessionID, prompt, nil, planMode, deps.EpistemicCheckDefault, actorUserID, httpapi.DropIfOpen)
	if cerr != nil {
		if errors.Is(cerr, httpapi.ErrPlanAwaitingApproval) {
			// a follow-up fix (§8.1): honest reply, never a hard
			// failure -- mirrors the !wasCreated busy-reply branch just
			// below for the analogous open-turn case (M6 audit fix).
			logger.Info("linear: ordinary reply blocked by awaiting-approval plan", "session_id", sessionID.String())
			deps.postThoughtNotice(ctx, payload.OrganizationID, payload.AgentSession.ID, planAwaitingApprovalReplyText, notice)
			return true
		}
		logger.Error("linear: create turn failed", "status", cerr.Status, "message", cerr.Message, "session_id", sessionID.String())
		return false
	}
	if !wasCreated {
		// M6 audit fix: PREVIOUSLY this case was only ever logged, with NO
		// visible response to the user at all -- worse than Slack's own
		// false "I'll pick this up next" promise (which at least said
		// something, even if untrue). Post an honest reply instead.
		logger.Warn("linear: session already has an open turn, dropping prompted message", "session_id", sessionID.String())
		deps.postThoughtNotice(ctx, payload.OrganizationID, payload.AgentSession.ID, busyReplyText, notice)
		return true
	}

	// L20 audit fix: this path previously logged session creation
	// (handleCreated above) but NOT a reply turn -- an on-call engineer
	// investigating a bad push from a Linear-originated reply turn had no
	// session_id/turn_id to correlate against. Mirrors github's own
	// identical successful-mention log line shape (coalesce.go).
	//
	// plan_mode is the audit-remediation batch's own SECOND fix
	// ("neither Slack nor Linear logs the routing decision itself"):
	// PREVIOUSLY this line carried only session_id/turn_id, so a revise:
	// reply re-routed into a plan_mode=true revision turn was
	// indistinguishable, in the logs, from an ordinary plan_mode=false
	// build turn -- the negative branch (the ErrPlanAwaitingApproval log
	// line above) was already logged, but the POSITIVE re-routing decision
	// was not.
	//
	// createdTurn.PlanMode (not the local planMode variable computed above
	// the CreateTurnCore call) -- F2 audit fix: planMode is captured BEFORE
	// CreateTurnCore/createTurnLocked ever runs, so it never reflects
	// createTurnLocked's own §23 promotion of planMode=true when the
	// plan_followup classifier returns a confident "amend" (see that
	// function's own doc comment, turn.go). Logging the stale local
	// variable here would silently misreport a promoted turn as
	// plan_mode=false.
	logger.Info("linear: added turn", "session_id", sessionID, "turn_id", createdTurn.ID, "plan_mode", createdTurn.PlanMode)

	// turns carries no per-row actor column at all (migrations/
	// 000005_turns.up.sql) -- unlike sessions.created_by/plans.decided_by,
	// there is nothing further to attribute actorUserID onto for this
	// ordinary-reply path (mirrors internal/adapters/inbound/slack's own
	// addTurn, which attributes actorUserID onto the SAME audit_log row
	// only, for the identical reason). notice (if any) still needs to
	// reach the user, though -- posted as its own best-effort activity,
	// since (unlike the plan-verdict branch above) there is no other
	// outbound activity this path already sends to append it to.
	// httpapi.CreateTurnCore itself already fired the SAME
	// GetOrSpawn+EnsureDispatched post-commit dispatch sequencing this
	// function used to do here directly (turn.go's own createTurnLocked).
	deps.postIdentityNotice(ctx, payload.OrganizationID, payload.AgentSession.ID, "", notice)
	return true
}

// findAwaitingApprovalPlanID reports sessionID's own current
// awaiting_approval plan id, if any -- a plain scan over
// ListPlanSummariesForSession (the SAME query planrecord.go's own
// recordPlanIfNeeded already uses), since the partial unique index
// (plans_one_awaiting_approval_per_session) guarantees at most one match
// in practice; this scans defensively rather than assuming that. A lookup
// failure is logged and treated as "no awaiting plan" (false) -- a
// keyword-parsing convenience must never turn into a hard failure of the
// underlying `prompted` webhook handling.
func (deps Deps) findAwaitingApprovalPlanID(ctx context.Context, logger *slog.Logger, sessionID pgtype.UUID) (pgtype.UUID, bool) {
	summaries, err := deps.Plans.ListSummariesForSession(ctx, sessionID)
	if err != nil {
		logger.Warn("linear: list plan summaries for verdict check failed", "error", err, "session_id", sessionID.String())
		return pgtype.UUID{}, false
	}
	for _, s := range summaries {
		if s.Status == sqlcgen.PlanStatusAwaitingApproval {
			return s.ID, true
		}
	}
	return pgtype.UUID{}, false
}

// handlePlanVerdict calls the shared httpapi.DecidePlan with decidedBy --
// §13.2's ("identities + full RBAC", §13.2) own resolveActor result
// (Valid iff the replying Linear user is already linked, or was just
// auto-linked this call; invalid/bot-attribution otherwise, matching this
// package's own PREVIOUS unconditional-bot-attribution precedent for the
// still-unresolved case) -- then posts a follow-up `response` AgentActivity
// describing the REAL final outcome, whether this call itself won or a
// different channel already decided first, with identityNotice (§13.2's
// own "notify in-channel", empty when there's nothing to say) appended.
//
// Returns ok=false ONLY for a genuine backend error from its own
// authorizeSessionAction call -- LOW audit fix, second review pass
// ("handlePlanVerdict has the same conflation, explicitly left out of the
// first fix's scope"): this function's own OTHER internal failures
// (DecidePlan erroring for any reason other than ErrPlanOpenTurnInFlight,
// the outcome-activity post failing) are still only logged and swallowed
// here, exactly as before this fix -- the session/plan this verdict
// targets already exists and is otherwise healthy, so a failed DECISION
// itself remains an accepted, already-documented scope limitation, not
// something this fix's own narrow scope (authorizeSessionAction's
// conflation specifically) touches.
func (deps Deps) handlePlanVerdict(ctx context.Context, logger *slog.Logger, sessionID, planID pgtype.UUID, verdict string, decidedBy pgtype.UUID, identityNotice, organizationID, agentSessionID string) bool {
	// §13.2 ("identities + full RBAC", §13.2/§13.3) update: a decidedBy
	// that resolved to a REAL, linked user_id must still pass domain/authz.
	// Authorize(ActionApprovePlan) -- exactly what the REST approve/reject
	// endpoints already require via canActOnPlan (planauthz.go).
	//
	// Audit-fix batch update ("block unlinked actor state changes"): a
	// still-unlinked (bot-attributed) decidedBy is NO LONGER let through --
	// authorizeSessionAction now returns ErrActorNotAuthorized immediately
	// for that case too, replacing this package's previous "Linear verdicts
	// stay unauthenticated-per-user until linked" precedent (decideplan.go's
	// own top doc comment describes the OLD behavior) with a denial, exactly
	// like a linked-but-insufficient-role decidedBy.
	//
	// LOW audit fix (second review pass): this NOW distinguishes
	// ErrActorNotAuthorized from a genuine backend error, using the exact
	// same pattern already established for handlePrompted's own
	// ordinary-reply gate above -- a real denial keeps today's behavior
	// (post the denial message, return ok=true, no claim release); a
	// genuine backend error (already logged inside authorizeSessionAction)
	// returns ok=false instead, which this function's own caller
	// (handlePrompted) now propagates as ITS OWN return value, routing it
	// into the SAME release-the-claim-and-retry path H2 already wired up
	// for every other post-claim failure. Before this fix, a genuine
	// backend error here fell into the SAME branch as a real denial: it
	// posted the misleading "you don't have permission" message AND never
	// returned ok=false at all (this function had no return value), so the
	// webhook-delivery claim was never released and no redelivery could
	// ever retry the actual plan decision.
	if err := deps.authorizeSessionAction(ctx, logger, sessionID, decidedBy, authz.ActionApprovePlan); err != nil {
		if errors.Is(err, ErrActorNotAuthorized) {
			logger.Warn("linear: plan decision denied by authz", "plan_id", planID.String(), "session_id", sessionID.String(), "user_id", decidedBy.String())
			deps.postPlanOutcomeActivity(ctx, logger, organizationID, agentSessionID, "You don't have permission to approve or reject this plan.", identityNotice)
			return true
		}
		return false
	}

	outcome, err := httpapi.DecidePlan(ctx, deps.Pool, deps.Sessions, deps.Turns, deps.Plans, deps.Events, deps.PlanDocuments, deps.Outbox, deps.AgentSessions, deps.AuditLog, deps.Registry, sessionID, planID, httpapi.PlanVerdict(verdict), decidedBy, deps.EpistemicCheckDefault)
	if err != nil {
		if errors.Is(err, httpapi.ErrPlanOpenTurnInFlight) {
			deps.postPlanOutcomeActivity(ctx, logger, organizationID, agentSessionID, "A revision is already in progress for this plan -- try again once it completes.", identityNotice)
			return true
		}
		logger.Error("linear: decide plan failed", "error", err, "plan_id", planID.String(), "session_id", sessionID.String())
		return true
	}

	deps.postPlanOutcomeActivity(ctx, logger, organizationID, agentSessionID, renderLinearPlanOutcomeText(outcome), identityNotice)
	return true
}

// renderLinearPlanOutcomeText mirrors internal/adapters/inbound/slack's
// own renderPlanOutcomeText (interactive.go) -- reports outcome.FinalStatus
// honestly, whether THIS call won (the verdict it itself just rendered) or
// the plan was already decided by a DIFFERENT channel first (an honest
// "already decided elsewhere" reply, never a confusing duplicate --
// point 5 of this Step's own brief).
func renderLinearPlanOutcomeText(outcome httpapi.DecidePlanOutcome) string {
	switch outcome.FinalStatus {
	case "approved":
		if outcome.Won {
			return "Approved -- implementation started."
		}
		return "This plan was already approved via a different channel."
	case "rejected":
		if outcome.Won {
			return "Rejected."
		}
		return "This plan was already rejected via a different channel."
	case "superseded":
		return "This plan was superseded by a newer revision before your reply arrived."
	default:
		return "This plan is no longer awaiting approval."
	}
}

// postPlanOutcomeActivity posts a single `response` Agent Activity
// describing a plan decision's own outcome -- mirrors postAcknowledgment's
// own install-lookup/decrypt/bounded-call shape exactly, but always
// CreateResponseActivity (a rendered decision, approve or reject, is a
// normal outcome, never an "error" activity -- matches decideplan.go's own
// identical Success:true convention for the cross-channel notify path).
// Best-effort only: any failure is logged and swallowed, mirroring
// postAcknowledgment's own identical tolerance.
func (deps Deps) postPlanOutcomeActivity(ctx context.Context, logger *slog.Logger, organizationID, agentSessionID, text, identityNotice string) {
	install, err := deps.Installations.GetByOrganizationID(ctx, organizationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("linear: no installation for organization, skipping plan-outcome activity", "organization_id", organizationID)
			return
		}
		logger.Error("linear: look up installation failed", "error", err, "organization_id", organizationID)
		return
	}

	accessToken, err := platform.DecryptToken(deps.TokenEncryptionKey, install.AccessTokenEncrypted)
	if err != nil {
		logger.Error("linear: decrypt installation access token failed", "error", err, "organization_id", organizationID)
		return
	}

	activityCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.LinearOutboundActivityTimeout)
	defer cancel()

	if err := deps.LinearClient.CreateResponseActivity(activityCtx, string(accessToken), agentSessionID, text, identityNotice); err != nil {
		logger.Error("linear: post plan-outcome activity failed", "error", err, "agent_session_id", agentSessionID)
	}
}

// postThoughtNotice posts body as a best-effort `thought` Agent Activity --
// used by handlePrompted's own M6/L7 audit-fix reply paths (the busy-drop
// notice and the stop-not-supported notice), which -- unlike
// postIdentityNotice (identity.go) -- must ALWAYS post something, never
// no-op on an empty string (there is no "nothing to say" case for either
// of these two messages). Mirrors postAcknowledgment's/postIdentityNotice's
// own identical lookup+decrypt+bounded-call shape exactly (this package's
// own established "small, documented duplication over a forced shared
// abstraction" precedent -- see identity.go's own decryptLinearAccessToken
// doc comment).
func (deps Deps) postThoughtNotice(ctx context.Context, organizationID, agentSessionID, body, identityNotice string) {
	logger := platform.Logger(ctx)

	accessToken, ok := deps.decryptLinearAccessToken(ctx, logger, organizationID)
	if !ok {
		return
	}

	activityCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.LinearOutboundActivityTimeout)
	defer cancel()

	if err := deps.LinearClient.CreateThoughtActivity(activityCtx, accessToken, agentSessionID, body, identityNotice); err != nil {
		logger.Warn("linear: post thought notice activity failed", "error", err, "agent_session_id", agentSessionID)
	}
}

// postAcknowledgment posts the single, minimal `thought` Agent Activity
// this Step's own outbound scope covers (see this package's own doc.go
// and internal/adapters/outbound/linearapi's own doc.go). Best-effort
// only: any failure (no installation for this workspace, an expired
// token, a network error) is logged and otherwise swallowed -- it must
// never fail the webhook response itself, since the Narvi session this
// event backs has already been created successfully by this point.
//
// Known limitation (§8.10, explicitly scoped out): this does not
// refresh an expired access token before use. Linear's own OAuth access
// tokens are short-lived (confirmed during this Step's investigation:
// "valid for 24 hours"); linear_installations.refresh_token_encrypted is
// stored precisely so a future Step can add real refresh-before-expiry
// logic, but implementing that is beyond "the smallest immediate
// outbound need" this Step's own scope note calls for. Until a future
// Step adds it, this call simply starts failing (logged, non-fatal) once
// a workspace's stored token expires, until an admin reconnects it.
//
// body is now a parameter ("identities + full RBAC", §13.2
// update) rather than always the fixed acknowledgmentBody constant --
// handleCreated's own caller passes acknowledgmentBody with an identity-
// link notice appended (appendNotice), when there is one; every other
// property of this function is unchanged.
func (deps Deps) postAcknowledgment(ctx context.Context, organizationID, agentSessionID, body, identityNotice string) {
	logger := platform.Logger(ctx)

	install, err := deps.Installations.GetByOrganizationID(ctx, organizationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("linear: no installation for organization, skipping acknowledgment activity", "organization_id", organizationID)
			return
		}
		logger.Error("linear: look up installation failed", "error", err, "organization_id", organizationID)
		return
	}

	accessToken, err := platform.DecryptToken(deps.TokenEncryptionKey, install.AccessTokenEncrypted)
	if err != nil {
		logger.Error("linear: decrypt installation access token failed", "error", err, "organization_id", organizationID)
		return
	}

	activityCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.LinearOutboundActivityTimeout)
	defer cancel()

	if err := deps.LinearClient.CreateThoughtActivity(activityCtx, string(accessToken), agentSessionID, body, identityNotice); err != nil {
		logger.Error("linear: post acknowledgment activity failed", "error", err, "agent_session_id", agentSessionID)
	}
}
