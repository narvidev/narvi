package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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
	"github.com/narvidev/narvi/internal/app/shadowslack"
	"github.com/narvidev/narvi/internal/domain/authz"
	intentdomain "github.com/narvidev/narvi/internal/domain/intent"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/platform"
)

// intentClassifierSurface is the sessions.spawn_source value (§18.1's
// IntentClassifierInput.Surface / §18.4's IntentDecisionRecord.Surface)
// this package's own messages are classified/recorded under.
const intentClassifierSurface = "slack"

// maxRequestBodyBytes bounds every Slack request body this package reads
// -- mirrors httpapi's own identical constant/reasoning; a real Slack
// event payload is always far smaller than this.
const maxRequestBodyBytes = 1 << 20 // 1 MiB

// slackWebhookSignatureVersion is the fixed "v0" prefix Slack's own
// signing scheme uses on both the signed-string prefix and the
// X-Slack-Signature header value (confirmed against Slack's own current
// documentation -- see doc.go's own step 2).
const slackWebhookSignatureVersion = "v0"

// slackMessageClaimProvider is a SECOND, distinct WebhookDeliveryStore
// "provider" value (deliberately never "slack") used ONLY for the
// message-level coalescing claim below -- L3 audit fix ("Slack's own
// dual-delivery for one logical mention isn't coalesced"): Slack sends
// BOTH an app_mention event AND a message event (two DISTINCT event_id
// values) for a single @mention posted inside a thread this adapter
// already has mapped to a session. The outer (provider="slack",
// event_id) claim already made below can't coalesce them -- the two
// deliveries carry different event_ids, so both independently win that
// claim, both pass isIgnorable, and both would otherwise flow all the way
// through resolveOrClaimSession's existing-mapping branch into
// handleEvent's own addTurn call for the exact same underlying human
// action (a redundant resolveSlackActor call each, and -- absent this fix
// -- a confusing double ack: one "on it" for whichever twin wins the
// addTurn race, immediately followed by a second, separate "still
// working on the previous message" for its sibling).
//
// This claim is reused against the SAME (provider, delivery_id)-keyed
// webhook_deliveries table/primitive the outer claim already uses (§5.1's
// own "INSERT ... ON CONFLICT atomic claims" house style), keyed by
// messageClaimKey (event.go) -- the identity of the underlying MESSAGE
// OBJECT itself (channel, ts), NOT threadKey()/ThreadTS, which identifies
// the THREAD: both twin events carry the IDENTICAL ts value, since they
// describe the same Slack message object twice, while a genuinely
// DIFFERENT message posted later in the same thread carries a different
// ts and must NOT be coalesced with this one. Using a distinct provider
// value (rather than reusing "slack") keeps this claim space from ever
// colliding with a real event_id claimed above, even in the
// vanishingly-unlikely case a real event_id happened to look exactly like
// a "channel:ts" pair.
const slackMessageClaimProvider = "slack-message"

// ackNewSessionText/ackReplyText/ackBusyText/ackNotConfiguredText are the
// fixed in-thread ack messages this Step posts -- deliberately plain,
// static strings (no templating beyond what's inlined here), matching
// this Step's own "smallest possible direct call" scoping decision
// (doc.go).
const (
	ackNewSessionText = "On it — starting work on this now."
	ackReplyText      = "Got it — continuing work on this thread."
	// ackBusyText's wording is the M6 audit fix: the PREVIOUS text ("...
	// I'll pick this up next.") promised a retry/queue that never
	// actually happened -- a turn dropped for an already-busy session was
	// simply discarded, never picked up later. This wording is honest
	// about that instead.
	ackBusyText          = "Still working on the previous message in this thread — this one wasn't queued, please try again once it's done."
	ackNotConfiguredText = "Slack ingress isn't configured with a default repo yet, so I can't start new work from a mention. A reply on an existing thread still works."

	// ackNotAuthorizedText is §13.2's own addition ("identities + full
	// RBAC", §13.2/§13.3): posted instead of ackNewSessionText when the
	// acting user isn't authorized to create a session -- mirrors the REST
	// API's own 403 semantics ("not authorized to perform this action",
	// helpers.go) rather than silently creating the session anyway.
	//
	// Audit-fix batch update ("block unlinked actor state changes"): the
	// wording no longer says "Your linked Narvi account" -- that phrasing
	// assumed a link already existed, which is now wrong for the NEW
	// denial case this same text also covers: an actor whose auto-link
	// attempt hasn't resolved at all (AuthorizeLinkedActor denies before
	// ever consulting a role). The wording below reads correctly for
	// EITHER case (not linked yet, or linked but insufficient role) --
	// the separately-posted magic-link notice (resolveSlackActor's own
	// notice, delivered regardless of this denial) is what tells an
	// unlinked actor specifically how to fix it.
	ackNotAuthorizedText = "You're not authorized to start new sessions from Slack."

	// ackNotAuthorizedReplyText is this Step's own SECOND fix-pass
	// addition ("identities + full RBAC", §13.2/§13.3 -- a confirmed
	// re-review finding): posted instead of enqueuing a turn when the
	// acting user isn't authorized to prompt this session (a reply on an
	// ALREADY-MAPPED thread, or a brand-new mention that lost the
	// first-writer-wins race and falls back onto a DIFFERENT,
	// already-existing session) -- mirrors ackNotAuthorizedText's own
	// wording above (same audit-fix-batch update: no longer assumes a
	// link already exists), and Linear's identical denial text for the
	// equivalent ordinary-reply fallthrough (webhook.go's own
	// handlePrompted).
	ackNotAuthorizedReplyText = "You're not authorized to prompt this session."

	// ackNotEnrolledText (§10 Phase 6, §32) is posted instead of
	// ackNewSessionText when checkRolloutGate refuses because this
	// deployment's own default repo is not enrolled in the cohort
	// rollout -- mirrors ackNotAuthorizedText's own terminal-in-thread-ack
	// idiom immediately above (same shape, different reason): a permanent
	// policy refusal, never a transient failure, so this is posted once
	// and the thread is left exactly as usable as ackNotAuthorizedText
	// leaves it (no retry ever changes the outcome).
	ackNotEnrolledText = "This repository is not yet enrolled in Narvi's session rollout."
)

// ackPlanAwaitingText is this batch's own honest reply (§8.1
// follow-up fix, §8.1), posted instead of enqueuing a build turn when a
// plain-text thread reply matches neither a plan verdict (plandomain.
// MatchVerdict -- see this batch's own follow-up addition, "honour a typed
// plan verdict", handleEvent below) nor plandomain.MatchRevise, while the
// mapped session currently has a plan in StatusAwaitingApproval --
// CLOSING the hole where such a reply previously fell straight through
// into an ordinary, plan_mode=false turn the human never approved.
// Mirrors ackBusyText's own wording/tone immediately above; built (like
// Linear's identical planAwaitingApprovalReplyText, webhook.go) off
// plandomain.ApproveKeywords/RejectKeywords/RevisePrefix themselves, so
// the instructions here can never drift out of sync with what
// MatchVerdict/MatchRevise actually accept. The Approve/Reject BUTTONS
// (interactive.go) remain the primary, always-available affordance either
// way -- this is an additional accepted input, not a replacement.
var ackPlanAwaitingText = fmt.Sprintf(
	"A plan is awaiting approval for this session — reply %s to approve it, %s to reject it, use the Approve/Reject buttons above, or start your reply with %q to request changes.",
	strings.Join(plandomain.ApproveKeywords, "/"),
	strings.Join(plandomain.RejectKeywords, "/"),
	plandomain.RevisePrefix,
)

// ackEmptyReviseFeedbackText is the audit-remediation batch's own SECOND
// fix-pass addition (LOW audit finding, "the honest reply reused for the
// new empty-feedback case is generic boilerplate ... gives the user no
// indication that their revise: reply WAS recognized ... but was rejected
// specifically because the feedback was empty"): posted instead of
// ackPlanAwaitingText specifically for the emptyReviseFeedback case (below)
// -- a reply that DID match plandomain.RevisePrefix, unlike every other
// case ackPlanAwaitingText itself still covers (a reply matching neither an
// approve/reject button nor the revise: prefix at all). Telling the user
// their revise: syntax WAS recognized, and exactly why nothing happened
// (no feedback followed the prefix), is strictly more actionable than
// ackPlanAwaitingText's own generic "here's how this works" wording, which
// reads identically whether the prefix was never used or used with nothing
// after it.
var ackEmptyReviseFeedbackText = fmt.Sprintf(
	"Your %q reply was recognized, but no feedback followed it — reply again with your requested changes after %q.",
	plandomain.RevisePrefix, plandomain.RevisePrefix,
)

// Deps bundles every dependency NewHandler needs -- a small config
// struct (rather than a long positional parameter list, given the
// number of stores this adapter touches) mirroring this codebase's own
// existing precedent of grouping related construction parameters (e.g.
// modal.Config).
type Deps struct {
	Pool         *pgxpool.Pool
	Sessions     *postgres.SessionStore
	Turns        *postgres.TurnStore
	Environments *postgres.EnvironmentStore
	Registry     *sessionactor.Registry
	Deliveries   *postgres.WebhookDeliveryStore
	Threads      *postgres.SlackThreadSessionStore

	// Plans (a follow-up fix, §8.1) -- handleEvent's own
	// awaiting-plan gate/verdict/revise-prefix check (below) needs this to
	// find a mapped session's own awaiting_approval plan, if any, mirroring
	// Linear's identical Deps.Plans (webhook.go). nil-safe: a nil Plans
	// simply skips the verdict/revise-prefix check entirely
	// (createTurnLocked's own awaiting-plan gate, httpapi/turn.go, is
	// likewise nil-safe), so existing tests/wiring that don't care about
	// plan mode keep working unchanged.
	Plans *postgres.PlanStore

	// Events/PlanDocuments (§31.3) -- handlePlanVerdict's own
	// httpapi.DecidePlan call (below) needs these for its own
	// approved-plan snapshot dependency (decideplan.go), mirroring
	// Linear's identical Deps.Events/Deps.PlanDocuments (webhook.go).
	Events        *postgres.EventStore
	PlanDocuments *postgres.PlanDocumentStore

	// Outbox/LinearAgentSessions are this batch's own addition ("honour a
	// typed plan verdict"): handlePlanVerdict below now calls the SAME
	// shared httpapi.DecidePlan every other plan-decision entry point uses
	// (interactive.go's decideAndUpdateMessage, Linear's own
	// handlePlanVerdict, webhook.go) -- which needs Outbox for its own
	// cross-channel-notify side effect and LinearAgentSessions to look up
	// whether this session is ALSO backed by a Linear agent session worth
	// notifying (decideplan.go). Mirrors InteractiveDeps's own identical
	// two fields (interactive.go) exactly -- production wiring
	// (cmd/control-plane/main.go) passes the SAME two store instances every
	// other caller of DecidePlan already uses, never a second,
	// independently-constructed copy.
	Outbox              *postgres.OutboxStore
	LinearAgentSessions *postgres.LinearAgentSessionStore

	// AuditLog is §13.2's own addition (§13.3) -- threaded through to
	// httpapi.CreateSessionCore below exactly like Environments already
	// is, so a Slack-originated session creation gets the SAME audit_log
	// row every other CreateSessionCore caller now gets. actor_user_id is
	// NULL only until identity auto-linking (IdentityLink below) resolves
	// a real user -- see identity.go's own resolveSlackActor for the
	// replacement of the old unconditional bot-attribution precedent.
	AuditLog *postgres.AuditLogStore

	// Participants is this Step's own SECOND fix-pass addition
	// ("identities + full RBAC", §13.2/§13.3): authorizeSessionAction
	// (identity.go's own ownedOrJoined) needs this to resolve a `member`
	// actor's own "own/joined" carve-out exactly like InteractiveDeps.
	// Participants already does for the interactivity route -- so an
	// ordinary Events-API reply on an already-mapped thread renders the
	// IDENTICAL §13.3 verdict a REST/interactivity caller would for the
	// same (actor, session). Production wiring passes the SAME
	// participantStore instance every other caller already uses, never a
	// second, independently-constructed copy.
	Participants *postgres.ParticipantStore

	// IdentityLink/SlackClient are §13.2's own auto-linking wiring
	// (§13.2): resolveSlackActor (identity.go) uses SlackClient.
	// GetUserEmail to fetch ev.User's own profile email (with retry, via
	// Timeouts.IdentityEmailFetch*), then IdentityLink.Resolve to
	// auto-link or create a magic-link prompt the first time this package
	// sees a given Slack user id it doesn't already know about.
	//
	// SlackClient is ALSO this file's own in-thread-ack/identity-notice
	// client now (§30.3's "one client per provider" -- this package used
	// to construct a SEPARATE, private ackClient of its own, ack.go, and
	// this struct used to carry a SlackHTTPClient/BotToken/SlackAPIBaseURL
	// trio just to build it; all four are retired). Typed as the
	// shadowslack.Client interface, never the
	// concrete *slackapi.Client, so this package can no longer construct
	// one itself: production wiring (cmd/control-plane/main.go) hands over
	// a shadowslack.Decorator wrapping the SAME *slackapi.Client instance
	// already constructed for the outbox delivery worker and the
	// interactivity route (interactive.go's own identical
	// InteractiveDeps.SlackClient), never a second, independently-
	// constructed, gate-free client.
	IdentityLink identitylink.Deps
	SlackClient  shadowslack.Client
	// Timeouts is §13.2's own addition, read for its
	// IdentityEmailFetch* fields only (identity.go) -- every OTHER
	// timeout this package needs is still an existing discrete field
	// below (TimestampWindow, AckTimeout), left untouched.
	Timeouts platform.Timeouts

	// IntentClassifier is §8.3's own wiring point (§8.3/§18): classify
	// + record runs ONCE, on the brand-new-thread's own first real turn
	// (decided_at_stage="first_prompt" -- a bare session is created with
	// no prompt at all, see resolveOrClaimSession; the real text only
	// arrives here, at handleEvent's own addTurn call). Optional (nil-
	// safe): a nil IntentClassifier simply skips classification entirely.
	IntentClassifier *intentclassifier.Service

	// EpistemicCheckDefault ("builder epistemic pre-action
	// check", §20.4) is threaded through to addTurn's own createTurnLocked
	// call (turn.go) exactly like every other caller now gets --
	// production wiring (cmd/control-plane/main.go) passes the SAME
	// platform.Config.EpistemicCheckDefault value every other caller does.
	EpistemicCheckDefault bool

	// RolloutMode/RepoSettings (§10 Phase 6, §32) are threaded
	// through to resolveOrClaimSession's own httpapi.CreateSessionCore
	// call below exactly like EpistemicCheckDefault already is -- both
	// are REQUIRED parameters of that function now (its own doc
	// comment), so a zero-value RolloutMode here is indistinguishable
	// from rollout.ModeOpen (the safe default) rather than an accidental
	// gap.
	RolloutMode  platform.RolloutMode
	RepoSettings *postgres.RepoSettingsStore

	SigningSecret   string
	DefaultRepoName string
	DefaultRepoURL  string
	TimestampWindow time.Duration
	// AckTimeout bounds each in-thread ack/ephemeral-notice call
	// (platform.Timeouts.SlackAckTimeout in production wiring) -- these
	// are genuine outbound network calls made synchronously in this
	// handler's own request path, so they must never run against the
	// bare, deadline-free r.Context() unbounded (mirrors sessionactor's
	// own PRCreateTimeout precedent). A zero value here means no deadline
	// is applied at all, so production wiring should always pass it
	// explicitly.
	AckTimeout time.Duration
}

// postAckBounded posts text into channel via deps.SlackClient.PostAck,
// threaded under threadTS, bounded by timeout -- every caller in this
// package uses this rather than deps.SlackClient.PostAck directly: it is
// a genuine outbound network call made synchronously in this handler's
// own request path, which otherwise carries no deadline of its own (a
// bare r.Context()). Mirrors internal/app/sessionactor/pushpr.go's own
// PRCreateTimeout-bounded CreatePR call precedent exactly. A zero or
// negative timeout would make context.WithTimeout expire the call
// immediately, so callers must always pass a real, positive value
// (production wiring passes platform.Timeouts.SlackAckTimeout).
func postAckBounded(ctx context.Context, client shadowslack.Client, timeout time.Duration, channel, threadTS, text string) error {
	ackCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return client.PostAck(ackCtx, channel, threadTS, text)
}

// postIdentityLinkNoticeBounded is postAckBounded's own sibling for the
// identity-link prompt -- see that function's own doc comment for the
// identical "why bounded" reasoning.
//
// It calls PostIdentityLinkNotice, never PostEphemeral, and the
// difference is not cosmetic: this text carries a live magic-link nonce,
// and PostEphemeral's shadow record stores its text verbatim in a
// permanent, append-only ledger. The method used here records the fact
// with no text field at all. Do not "simplify" the two back together.
//
// (This replaced a general postEphemeralBounded, which had exactly one
// caller -- this one. The remaining PostEphemeral call site posts a fixed
// constant and needs no helper.)
func postIdentityLinkNoticeBounded(ctx context.Context, client shadowslack.Client, timeout time.Duration, channel, userID, threadTS, text string) error {
	noticeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return client.PostIdentityLinkNotice(noticeCtx, channel, userID, threadTS, text)
}

// NewHandler builds the POST /webhooks/slack handler (§8.10 --
// see doc.go's own full request-handling writeup).
func NewHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		body, ok := readBoundedBody(w, r)
		if !ok {
			return
		}

		if !verifySlackRequest(w, r, body, deps.SigningSecret, deps.TimestampWindow, logger) {
			return
		}

		// Cheapest possible check first: a url_verification handshake
		// carries no "event"/"event_id" at all, so it is recognized (and
		// fully handled) before ever attempting the fuller eventEnvelope
		// decode below.
		var challenge challengeEnvelope
		if err := json.Unmarshal(body, &challenge); err == nil && challenge.Type == "url_verification" {
			writeJSON(w, http.StatusOK, map[string]string{"challenge": challenge.Challenge})
			return
		}

		var envelope eventEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}

		if envelope.Type != "event_callback" {
			logger.Warn("slack: ignoring unrecognized outer envelope type", "type", envelope.Type)
			w.WriteHeader(http.StatusOK)
			return
		}

		if envelope.EventID == "" {
			writeError(w, http.StatusBadRequest, "missing event_id")
			return
		}

		claim, err := deps.Deliveries.Claim(ctx, "slack", envelope.EventID)
		if err != nil {
			logger.Error("slack: claim webhook delivery failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !claim.Inserted {
			// Already processed -- a genuine Slack redelivery of the
			// SAME event_id (§5.1's dedupe claim). Never reprocessed.
			w.WriteHeader(http.StatusOK)
			return
		}

		var ev slackEvent
		if err := json.Unmarshal(envelope.Event, &ev); err != nil {
			logger.Error("slack: decode inner event failed", "error", err)
			// H2 audit fix ("webhook claim/release parity"): this delivery
			// was claimed above but never actually processed -- release the
			// claim so that a genuine redelivery of this same event_id (a
			// human manually resending it, or Slack's own real retry
			// behavior on a slow/failed response) can actually reprocess it,
			// rather than being silently skipped forever as an
			// already-claimed duplicate. Mirrors github's own identical
			// parse-failure release (handler.go).
			if releaseErr := deps.Deliveries.Release(ctx, "slack", envelope.EventID); releaseErr != nil {
				logger.Error("slack: release webhook delivery claim failed", "error", releaseErr, "event_id", envelope.EventID)
			}
			writeError(w, http.StatusBadRequest, "malformed event")
			return
		}

		if ev.isIgnorable() {
			w.WriteHeader(http.StatusOK)
			return
		}

		// L3 audit fix ("Slack's own dual-delivery for one logical mention
		// isn't coalesced") -- see slackMessageClaimProvider's own doc
		// comment above for the full "why". Claimed right here: after
		// isIgnorable (no need to coalesce an event that would never reach
		// handleEvent anyway) and before handleEvent ever runs (so a
		// coalesced twin never redundantly calls resolveSlackActor or
		// posts a second ack).
		msgClaim, err := deps.Deliveries.Claim(ctx, slackMessageClaimProvider, ev.messageClaimKey())
		if err != nil {
			logger.Error("slack: claim message-level webhook delivery failed", "error", err)
			// Mirrors the "decode inner event failed" release just above:
			// the outer event_id claim already succeeded, but this event
			// was never actually processed, so release it too and let a
			// redelivery retry from scratch.
			if releaseErr := deps.Deliveries.Release(ctx, "slack", envelope.EventID); releaseErr != nil {
				logger.Error("slack: release webhook delivery claim failed", "error", releaseErr, "event_id", envelope.EventID)
			}
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !msgClaim.Inserted {
			// This exact underlying Slack message was already handled via
			// its twin event type (app_mention <-> message, or a genuine
			// redelivery of this same event type) -- skip entirely, never
			// posting a second, confusing ack.
			//
			// Residual, accepted tradeoff (re-review, mirrors this
			// codebase's own established "narrow the race, don't
			// eliminate every last window" precedent used throughout this
			// audit series): in the ONE case handleEvent's own
			// ReleaseMessageClaim signal below deliberately releases this
			// claim (a plain "message" event skipping as "not ours" on a
			// brand-new, not-yet-mapped thread), the differently-typed
			// twin that later reclaims it (the app_mention that actually
			// creates the session) DOES call resolveSlackActor a second
			// time -- the "never a redundant second one" guarantee below
			// holds for every OTHER outcome (the common already-mapped-
			// thread coalescing case, and every Skip that keeps this claim
			// held), just not this specific reclaim path, where a second
			// call is the deliberate cost of not losing the mention
			// entirely.
			w.WriteHeader(http.StatusOK)
			return
		}

		result := handleEvent(ctx, deps, logger, ev)
		if !result.OK {
			// H2 audit fix: handleEvent hit a genuine post-claim processing
			// failure (a DB error resolving/creating the session or adding
			// the turn) -- release the claim and answer non-2xx so a
			// redelivery of this same event_id can retry, instead of
			// silently and permanently dropping the event now that it's
			// (correctly) claimed. Never released for a best-effort
			// notification failure (the in-thread ack, the identity-link
			// ephemeral notice) or a deliberate business skip (no default
			// repo configured, an authz denial) -- see handleEvent's own doc
			// comment for the exact boundary, mirroring github's own
			// ErrActorNotAuthorized-vs-genuine-error distinction
			// (coalesce.go).
			if releaseErr := deps.Deliveries.Release(ctx, "slack", envelope.EventID); releaseErr != nil {
				logger.Error("slack: release webhook delivery claim failed", "error", releaseErr, "event_id", envelope.EventID)
			}
			// L3 audit fix: the message-level claim above must ALSO be
			// released on this exact same failure path -- otherwise a
			// later genuine retry (of either twin event_id) would find this
			// claim already taken and be silently, incorrectly dropped
			// forever.
			if releaseErr := deps.Deliveries.Release(ctx, slackMessageClaimProvider, ev.messageClaimKey()); releaseErr != nil {
				logger.Error("slack: release message-level webhook delivery claim failed", "error", releaseErr, "claim_key", ev.messageClaimKey())
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// HIGH audit fix ("message-level claim can permanently orphan its
		// own app_mention twin on a brand-new thread"): result.OK is true
		// here, so this is NOT the failure path just above, yet the
		// message-level claim can still need releasing -- see
		// sessionResolution's own ReleaseMessageClaim field and
		// handleEvent's own doc comment for the full "why" (the ONE
		// asymmetric Skip outcome between the two twin event types: a plain
		// "message" event landing first on a brand-new, not-yet-mapped
		// thread). Every OTHER ok=true outcome (a deliberate business skip
		// reached identically by either twin -- no default repo configured,
		// an authz denial -- or a fully successful addTurn) leaves
		// ReleaseMessageClaim false, so the claim stays held exactly as
		// before this fix.
		if result.ReleaseMessageClaim {
			if releaseErr := deps.Deliveries.Release(ctx, slackMessageClaimProvider, ev.messageClaimKey()); releaseErr != nil {
				logger.Error("slack: release message-level webhook delivery claim failed (asymmetric message-before-app_mention skip)", "error", releaseErr, "claim_key", ev.messageClaimKey())
			}
		}
		w.WriteHeader(http.StatusOK)
	}
}

// sessionResolution is resolveOrClaimSession's own result: either a real
// session to add a turn to (Skip == false), or nothing further to do at
// all (Skip == true -- either a deliberate no-op, e.g. a plain message
// with no mapping, or a gap already acked inline, e.g. no default repo
// configured). A genuine error is reported separately (see
// resolveOrClaimSession's own ok return value) and is always already
// logged by the time it's returned.
type sessionResolution struct {
	SessionID   pgtype.UUID
	IsNewThread bool
	Skip        bool

	// ReleaseMessageClaim is HIGH audit fix ("message-level claim can
	// permanently orphan its own app_mention twin on a brand-new thread"):
	// set ONLY by the "no mapping yet AND this event is not an app_mention"
	// Skip branch immediately below in resolveOrClaimSession -- the ONE
	// outcome in this function that is genuinely ASYMMETRIC between the two
	// twin event types Slack sends for a single logical mention
	// (app_mention and message, see slackMessageClaimProvider's own doc
	// comment above). A plain "message" event can NEVER create a new
	// session/thread mapping (only an app_mention can, see the check right
	// below), so if that message twin happens to win NewHandler's own
	// (channel, ts)-keyed message-level claim race on a brand-new thread, it
	// takes THIS branch, and unless the claim is released here, it stays
	// held forever -- silently discarding the app_mention twin (the ONLY
	// one that could ever have actually created the session) the moment it
	// arrives afterward, with both Slack deliveries still answering 200 OK
	// (so Slack never retries) and zero operator-visible signal.
	//
	// Every OTHER Skip outcome in this function (no default repo
	// configured, an authz denial on create, an authz denial on an
	// existing-thread reply via authorizeExistingSessionReply) is reached
	// IDENTICALLY regardless of which twin got there first -- both twins
	// would render the exact same verdict, so releasing the message-level
	// claim there would only let a genuinely-denied/misconfigured twin
	// retry pointlessly. Those leave this field false (the default), and
	// NewHandler's own release-path comment (above) still applies to them
	// unchanged.
	//
	// Residual, ACCEPTED race (re-review; mirrors this codebase's own
	// established "narrow the window, don't eliminate every last one"
	// tradeoff already used throughout this audit series, e.g. the SCM-
	// credentials disabled/role recheck, the outbox claim-lease CAS): this
	// release still depends on ORDERING relative to the twin's own Claim
	// attempt. If the two twin deliveries are handled by truly concurrent
	// requests (not just arbitrarily ORDERED ones, which this fix fully
	// closes) and the app_mention twin's own Claim call lands inside the
	// narrow window between the message twin's INSERT and its later
	// Release, the app_mention twin can still lose the race and be
	// skipped-with-200, same failure mode as the bug this field exists to
	// close, just requiring genuine concurrency rather than firing on
	// every ordering. Closing this fully would need a held, cross-request
	// lock spanning the whole message-twin attempt rather than a point-in-
	// time claim -- a materially bigger, more invasive mechanism than this
	// narrow finding warrants; the window this fix leaves is small (a
	// handful of DB round trips) and, given Slack's own real-world twin-
	// delivery timing, expected to be rare in practice.
	ReleaseMessageClaim bool
}

// handleEventResult is handleEvent's own result -- mirrors
// sessionResolution's own small, explicit struct shape rather than a bare
// bool (this codebase's own established preference, see that type's doc
// comment) now that handleEvent has two independent things to report: OK
// (see handleEvent's own doc comment below) and, orthogonally,
// ReleaseMessageClaim (HIGH audit fix, "message-level claim can
// permanently orphan its own app_mention twin on a brand-new thread") --
// whether NewHandler's own message-level claim (slackMessageClaimProvider)
// must ALSO be released even though OK is true, because
// resolveOrClaimSession's own res.ReleaseMessageClaim fired (see that
// field's own doc comment for the full "why"). ReleaseMessageClaim is
// meaningless when OK is false -- NewHandler's own failure path already
// unconditionally releases both claims regardless of this field.
type handleEventResult struct {
	OK                  bool
	ReleaseMessageClaim bool
}

// handleEvent implements doc.go's own thread<->session mapping design
// (steps 7-8): resolve or create the mapped session, add a turn, then
// best-effort ack. Returns OK=false ONLY for a genuine post-claim
// persistence failure (resolveOrClaimSession's own DB errors, addTurn
// failing, or -- MEDIUM audit fix, "authorizeSessionAction conflates a
// genuine backend error with a real authorization denial" --
// authorizeExistingSessionReply's own authorizeSessionAction call
// returning a genuine backend error, distinct from ErrActorNotAuthorized)
// -- H2 audit fix ("webhook claim/release parity"): the caller (NewHandler)
// releases the webhook-delivery claim and answers non-2xx when OK is
// false, so a redelivery of this same event_id can actually retry, instead
// of the event being silently and permanently dropped now that it's
// claimed. Every OTHER failure here (a best-effort in-thread
// ack/ephemeral-notice post failing, or a deliberate business skip -- no
// default repo configured, a genuine ErrActorNotAuthorized denial) is
// still only ever logged, returning OK=true: retrying those would either
// change nothing (an authz denial renders the exact same verdict again)
// or risks double-posting/double-processing something that already fully
// succeeded (the ack/notice text is best-effort exactly because the
// underlying session/turn work is already durably committed by the time
// either runs). See handleEventResult's own doc comment for the SECOND,
// orthogonal thing this now reports: ReleaseMessageClaim.
func handleEvent(ctx context.Context, deps Deps, logger *slog.Logger, ev slackEvent) handleEventResult {
	channel := ev.Channel
	key := ev.threadKey()
	prompt := normalizeMrkdwn(ev.Text)

	// §13.2 ("identities + full RBAC", §13.2) update: resolve the REAL
	// actor behind ev.User ONCE, regardless of whether this event ends up
	// starting a brand-new thread or replying to an existing one --
	// resolveSlackActor itself is called on every event (session-creating
	// or a reply on an existing thread), since a reply needs actorUserID
	// for authorizeSessionAction below just as much as a new thread needs
	// it for resolveOrClaimSession's own created_by, and there is no
	// cheaper way to tell which kind of event this is before resolving it.
	// The actual auto-link ALGORITHM inside it (identitylink.Resolve's
	// fetch+match+link) is what §13.2 means by "runs on first event from
	// an unknown provider identity": resolveSlackActor's own pre-check
	// (identity.go) now short-circuits straight to the already-linked
	// user id for every OTHER event, with no provider fetch at all, so
	// that algorithm genuinely only executes once per never-before-seen
	// identity, matching §13.2 as written.
	// actorUserID is only actually CONSUMED by resolveOrClaimSession below
	// (a bare session's own created_by) -- a reply on an existing thread
	// has nowhere to attribute it (turns carries no actor column, mirrors
	// this file's own addTurn, which has never taken one), but identity
	// resolution/notification still needs to run for it regardless.
	actorUserID, notice := resolveSlackActor(ctx, logger, deps.SlackClient, deps.IdentityLink, deps.Timeouts, ev.User)

	res, ok := resolveOrClaimSession(ctx, deps, logger, ev, channel, key, actorUserID, prompt)

	// Security-remediation addition ("identities + full RBAC",
	// §13.2): notice (the "connected your account" confirmation, or --
	// far more sensitive -- the magic-link URL itself) is posted via
	// chat.postEphemeral, visible ONLY to ev.User, NEVER appended to the
	// ordinary, whole-channel-visible ack below anymore -- see
	// slackapi.Client.PostEphemeral's own doc comment for the confirmed
	// hijack this closes. Posted regardless of ok/res.Skip (a denied/skip
	// outcome already gets its own ack elsewhere above; the identity
	// notice, when there is one, is still this user's own business to
	// see either way) except when ev.User is empty (postEphemeral would
	// have nothing to scope to -- never expected in practice, see
	// resolveSlackActor's own identical defensive short-circuit).
	if notice != "" && ev.User != "" {
		if err := postIdentityLinkNoticeBounded(ctx, deps.SlackClient, deps.AckTimeout, channel, ev.User, key, notice); err != nil {
			logger.Warn("slack: post identity-link ephemeral notice failed", "error", err)
		}
	}

	if !ok {
		// A genuine backend error inside resolveOrClaimSession (already
		// logged there) -- distinct from res.Skip below, which reports a
		// deliberate business decision (never a failure). ReleaseMessageClaim
		// is irrelevant here (see handleEventResult's own doc comment) --
		// NewHandler's own OK==false path always releases both claims
		// regardless.
		return handleEventResult{OK: false}
	}
	if res.Skip {
		// HIGH audit fix: propagate res.ReleaseMessageClaim straight through
		// -- see sessionResolution's own doc comment for exactly which Skip
		// outcome sets this, and why every other one leaves it false.
		return handleEventResult{OK: true, ReleaseMessageClaim: res.ReleaseMessageClaim}
	}

	// This batch's own addition ("honour a typed plan verdict in a Slack
	// thread", closing the gap where a user who types "approve" instead of
	// clicking a button got no verdict honoured at all): while
	// res.SessionID has a plan in StatusAwaitingApproval, a plain-text
	// reply matching plandomain.MatchVerdict is a deterministic approve/
	// reject decision -- route it through handlePlanVerdict below, which
	// calls the EXACT SAME authorization check (Deps.authorizeSessionAction,
	// identity.go, ActionApprovePlan) and the EXACT SAME shared
	// httpapi.DecidePlan every other plan-decision entry point in this
	// codebase uses (interactive.go's own button-driven decideAndUpdateMessage,
	// Linear's own handlePlanVerdict, webhook.go) -- never a second,
	// independently-authorized path to a plan decision. Checked BEFORE
	// plandomain.MatchRevise below: MatchVerdict/MatchRevise can never both
	// match the same text (RevisePrefix, "revise:", is not itself one of
	// ApproveKeywords/RejectKeywords), but checking the verdict first keeps
	// this ordering identical to Linear's own handlePrompted (webhook.go),
	// which this Step's own domain package (plandomain) was written
	// alongside. A match returns IMMEDIATELY -- a verdict IS the decision,
	// never merely a prompt that also happens to fall through into an
	// ordinary turn.
	//
	// a follow-up fix (§8.1), unchanged by this batch: a plain-text
	// reply matching plandomain.RevisePrefix instead is a deterministic
	// "request changes" reply -- route it through as a REAL plan_mode=true
	// turn (the prompt becomes the stripped feedback) instead of an ordinary
	// one. Every OTHER reply (matching neither a verdict nor this prefix)
	// falls through unchanged into the ordinary addTurn call below;
	// createTurnLocked's own awaiting-plan gate (httpapi/turn.go) is what
	// actually declines it there, surfacing ErrPlanAwaitingApproval, handled
	// just below. The Approve/Reject BUTTONS (interactive.go) remain the
	// primary, always-available affordance either way -- this is an
	// additional accepted input, not a replacement (invariant #2 of this
	// batch's own brief). deps.Plans == nil (never true in production
	// wiring) skips this whole check entirely, exactly like the gate
	// itself does.
	planMode := false
	// emptyReviseFeedback is the audit-remediation batch's own fix
	// ("revise: accepts empty feedback"): plandomain.MatchRevise documents
	// ok=true, feedback=="" for a bare "revise:" (or whitespace-only
	// feedback) as an EXPLICIT caller's-own-job case (verdict.go's own doc
	// comment: "deciding what to do with an empty feedback prompt is
	// entirely the caller's own job") -- this codebase already has an
	// answer to that question, at the pre-existing "Request changes" Block
	// Kit modal submission (interactive.go's own handleViewSubmission,
	// which this SAME batch's own follow-up fix now ALSO makes reject
	// empty feedback with a user-visible inline modal error, instead of
	// the bare "ignoring" 200 it used to write -- see that function's own
	// doc comment). This applies the SAME rule here (treating
	// whitespace-only feedback as empty too), rather than silently
	// dispatching a genuine plan_mode=true revision turn with nothing at
	// all for the agent to act on.
	emptyReviseFeedback := false
	if deps.Plans != nil {
		if planID, hasAwaiting := findAwaitingApprovalPlanID(ctx, logger, deps.Plans, res.SessionID); hasAwaiting {
			if verdict, verdictOK := plandomain.MatchVerdict(prompt); verdictOK {
				return deps.handlePlanVerdict(ctx, logger, channel, key, res.SessionID, planID, verdict, actorUserID)
			}
			if feedback, reviseOK := plandomain.MatchRevise(prompt); reviseOK {
				// plandomain.IsBlankFeedback (LOW audit fix, confirmed finding
				// "MatchRevise's feedback-emptiness check ... does not treat
				// zero-width characters as whitespace") replaces a bare
				// strings.TrimSpace(feedback) == "" check here -- the shared
				// definition also catches feedback made up ONLY of invisible
				// zero-width runes (U+200B/200C/200D/FEFF), which TrimSpace
				// alone would let through as "non-empty".
				if plandomain.IsBlankFeedback(feedback) {
					emptyReviseFeedback = true
				} else {
					prompt = feedback
					planMode = true
				}
			}
		}
	}

	gatedByAwaitingPlan := false
	var createdTurn sqlcgen.Turn
	var created bool
	if emptyReviseFeedback {
		// No addTurn call at all here -- unlike the createTurnLocked-gated
		// case just below (an ordinary, non-revise reply that
		// createTurnLocked's own awaiting-plan gate declines), an empty-
		// feedback revise: reply must never even reach turn creation: this
		// reuses the SAME honest ackPlanAwaitingText reply that case gets
		// (the ackText switch below), so the human sees exactly the
		// clarification they'd see for any other reply that fails to
		// request changes properly.
		gatedByAwaitingPlan = true
		// LOW audit fix (confirmed finding, "log-level inconsistency
		// between the new empty-feedback-guard branch and the pre-existing
		// ... 'blocked by awaiting-approval plan' branch"): logged at Info,
		// matching the functionally identical ErrPlanAwaitingApproval
		// branch just below -- both are routine, expected user mistakes
		// (an ordinary reply or an empty revise: reply reaching an
		// awaiting-approval gate) that produce the exact same honest
		// ackPlanAwaitingText reply and no adverse system state, so neither
		// deserves a higher severity than the other. Previously Warn,
		// which would flag this routine case above the identical one below
		// on any Warn-level alert.
		logger.Info("slack: revise: reply had empty feedback, blocked by awaiting-approval plan guard", "session_id", res.SessionID)
	} else {
		var err error
		createdTurn, created, err = addTurn(ctx, deps.Pool, deps.Sessions, deps.Turns, deps.Plans, deps.IntentClassifier, deps.AuditLog, deps.Registry, res.SessionID, prompt, planMode, deps.EpistemicCheckDefault, actorUserID)
		if err != nil {
			if !errors.Is(err, httpapi.ErrPlanAwaitingApproval) {
				logger.Error("slack: add turn failed", "error", err)
				return handleEventResult{OK: false}
			}
			// Honest reply (this batch's own addition), never a hard failure --
			// mirrors DropIfOpen's own existing "still busy" precedent for the
			// analogous open-turn case (M6 audit fix) just below.
			gatedByAwaitingPlan = true
			logger.Info("slack: reply blocked by awaiting-approval plan", "session_id", res.SessionID)
		}
	}

	// L20 audit fix: this package previously logged NOTHING on a
	// successful turn add at all -- an on-call engineer investigating a
	// bad push from a Slack-originated turn had no session_id/turn_id to
	// correlate against in the logs. Mirrors github's own identical
	// successful-mention log line shape (coalesce.go). The busy/dropped
	// case (M6) gets its own, distinct log line instead of silence.
	//
	// plan_mode is the audit-remediation batch's own SECOND fix
	// ("neither Slack nor Linear logs the routing decision itself"):
	// PREVIOUSLY this line carried only session_id/turn_id, so a revise:
	// reply re-routed into a plan_mode=true revision turn was
	// indistinguishable, in the logs, from an ordinary plan_mode=false
	// build turn -- the negative branch (gatedByAwaitingPlan, just above)
	// was already logged, but the POSITIVE re-routing decision was not.
	if !gatedByAwaitingPlan {
		if created {
			// createdTurn.PlanMode (not the local planMode variable computed
			// above addTurn's own call) -- F2 audit fix: planMode is captured
			// BEFORE addTurn/createTurnLocked ever runs, so it never reflects
			// createTurnLocked's own §23 promotion of planMode=true when
			// the plan_followup classifier returns a confident "amend" (see
			// that function's own doc comment, turn.go). Logging the stale
			// local variable here would silently misreport a promoted turn
			// as plan_mode=false.
			logger.Info("slack: added turn", "session_id", res.SessionID, "turn_id", createdTurn.ID, "plan_mode", createdTurn.PlanMode)
		} else {
			logger.Warn("slack: session already has an open turn, dropping message", "session_id", res.SessionID)
		}
	}

	// §8.3 ("intent classifier", §8.3/§18): classify + record ONCE, on
	// the brand-new thread's own first real turn only -- IntentDecisionRecord
	// is a per-SESSION record (§18.4), and every Slack-originated session
	// gets its first (and, per this thread's own res.IsNewThread gate,
	// only) classify+record attempt right here, the first time a real
	// prompt exists for it at all (decided_at_stage="first_prompt" -- the
	// bare session itself, resolveOrClaimSession above, carries no prompt
	// text whatsoever). A later reply on the SAME thread (res.IsNewThread
	// == false) never re-classifies.
	if res.IsNewThread && created && deps.IntentClassifier != nil {
		// classify+record is now the shared intentclassifier.Service.
		// ClassifyAndRecord (H9/L11 audit fix) -- see that method's own
		// doc comment for the full "why a single shared call" reasoning.
		// This package's own one genuine difference from GitHub/Linear is
		// DecidedAtStageFirstPrompt below (a bare Slack thread carries no
		// prompt text of its own -- see this function's own doc comment
		// above for why classification only happens here, at the first
		// real turn).
		deps.IntentClassifier.ClassifyAndRecord(ctx, res.SessionID, ports.IntentClassifierInput{
			Text:    prompt,
			Surface: intentClassifierSurface,
		}, intentdomain.DecidedAtStageFirstPrompt)
	}

	ackText := ackReplyText
	switch {
	// LOW audit fix: emptyReviseFeedback gets its OWN, more specific text
	// (ackEmptyReviseFeedbackText) -- checked BEFORE the general
	// gatedByAwaitingPlan case below (emptyReviseFeedback always implies
	// gatedByAwaitingPlan == true, never the reverse), so a reply that DID
	// match the revise: prefix but carried no feedback gets told exactly
	// that, instead of ackPlanAwaitingText's own generic "here's how this
	// works" wording shared with every other awaiting-plan-gated case.
	case emptyReviseFeedback:
		ackText = ackEmptyReviseFeedbackText
	case gatedByAwaitingPlan:
		ackText = ackPlanAwaitingText
	case res.IsNewThread:
		ackText = ackNewSessionText
	case !created:
		ackText = ackBusyText
	}
	// notice is no longer appended here -- see this function's own top
	// doc comment for why it is now posted separately, ephemerally,
	// scoped to ev.User (§13.2's own security-remediation addition).
	if err := postAckBounded(ctx, deps.SlackClient, deps.AckTimeout, channel, key, ackText); err != nil {
		logger.Warn("slack: post in-thread ack failed", "error", err)
	}
	return handleEventResult{OK: true}
}

// handlePlanVerdict decides planID via the shared httpapi.DecidePlan, then
// posts a single in-thread reply describing the REAL final outcome --
// handleEvent's own new entry point (this batch's own addition, "honour a
// typed plan verdict") for a plain-text thread reply that matched
// plandomain.MatchVerdict while sessionID had planID sitting in
// StatusAwaitingApproval.
//
// Invariant #1 of this batch's own brief ("a text verdict MUST flow
// through the SAME authorization check the button path already
// performs"): this calls deps.authorizeSessionAction(..., authz.
// ActionApprovePlan) -- the EXACT SAME method (identity.go), over the
// EXACT SAME domain check (actorauthz.OwnedOrJoined +
// actorauthz.AuthorizeResolvedActor), that InteractiveDeps.
// authorizeSessionAction (interactive.go) renders for a button click --
// both are thin, per-Deps-struct-type copies of the identical §13.3
// verdict (see identity.go's own top doc comment for why this package
// keeps them as two separate methods rather than one shared function:
// Deps and InteractiveDeps are two distinct struct types). Never a second,
// looser gate: an actor who could not click Approve/Reject cannot type
// "approve" and have it stick either.
//
// actorUserID.Valid == false (not yet linked) is denied here too -- ordinary
// errors.Is(err, ErrActorNotAuthorized) covers both "never linked" and
// "linked but insufficient role" identically (see identity.go's own
// ErrActorNotAuthorized/ErrActorNotLinked doc comments for why that
// collapse is safe for every caller except interactive.go's own
// decideAndUpdateMessage/handleViewSubmission): unlike those two, there are
// no Block Kit buttons on THIS reply to worry about stripping (this is a
// plain-text thread reply, never a button message), so this deliberately
// does not need ErrActorNotLinked's own more specific handling. The denial
// text reused below (slackPlanForbiddenText) is interactive.go's own exact
// wording for this exact denial -- a plan-decision-specific message, not
// handler.go's own generic ackNotAuthorizedReplyText ("not authorized to
// prompt this session"), since this is not a prompt/turn-creation denial.
//
// Returns OK=false ONLY for a genuine backend error from
// authorizeSessionAction's own check (already logged there) -- mirrors
// authorizeExistingSessionReply's own identical MEDIUM-audit-fix
// conflation-avoidance immediately below, and Linear's own handlePlanVerdict
// (webhook.go): flows into the SAME release-the-claim-and-retry path H2
// already wired up for every other post-claim failure in this package.
// Every OTHER return is OK=true: a genuine authz denial (posted, honest,
// never a hard failure) or DecidePlan's own outcome (won or lost the
// decision elsewhere, or a revision already in flight) -- all deliberate
// business decisions a retry could not usefully change.
func (deps Deps) handlePlanVerdict(ctx context.Context, logger *slog.Logger, channel, key string, sessionID, planID pgtype.UUID, verdict string, actorUserID pgtype.UUID) handleEventResult {
	if err := deps.authorizeSessionAction(ctx, logger, sessionID, actorUserID, authz.ActionApprovePlan); err != nil {
		if errors.Is(err, ErrActorNotAuthorized) {
			logger.Warn("slack: text plan verdict denied by authz", "session_id", sessionID.String(), "plan_id", planID.String(), "user_id", actorUserID.String())
			if ackErr := postAckBounded(ctx, deps.SlackClient, deps.AckTimeout, channel, key, slackPlanForbiddenText); ackErr != nil {
				logger.Warn("slack: post not-authorized text-verdict ack failed", "error", ackErr)
			}
			return handleEventResult{OK: true}
		}
		// A genuine backend failure while checking authorization (already
		// logged inside authorizeSessionAction) -- distinct from the real
		// denial above -- flows into the SAME release-the-claim-and-retry
		// path H2 already wired up for every other post-claim failure in
		// this package (mirrors authorizeExistingSessionReply's own
		// identical MEDIUM-audit-fix distinction just below).
		return handleEventResult{OK: false}
	}

	outcome, err := httpapi.DecidePlan(ctx, deps.Pool, deps.Sessions, deps.Turns, deps.Plans, deps.Events, deps.PlanDocuments, deps.Outbox, deps.LinearAgentSessions, deps.AuditLog, deps.Registry, sessionID, planID, httpapi.PlanVerdict(verdict), actorUserID, deps.EpistemicCheckDefault)

	var text string
	switch {
	case err != nil && errors.Is(err, httpapi.ErrPlanOpenTurnInFlight):
		text = "A revision is already in progress for this plan — try again once it completes."
	case err != nil:
		logger.Error("slack: text plan verdict decide plan failed", "error", err, "session_id", sessionID.String(), "plan_id", planID.String())
		text = "Something went wrong recording this decision. Please try again."
	default:
		// renderPlanOutcomeText (interactive.go) reports outcome.FinalStatus
		// honestly, whether THIS reply won (the verdict it itself just
		// rendered) or the plan was already decided elsewhere first (a
		// button click, or a different channel entirely) -- reused
		// verbatim, never a second, drifted copy of the same wording.
		text = renderPlanOutcomeText(outcome)
	}

	logger.Info("slack: text plan verdict decided", "session_id", sessionID.String(), "plan_id", planID.String(), "verdict", verdict)
	if ackErr := postAckBounded(ctx, deps.SlackClient, deps.AckTimeout, channel, key, text); ackErr != nil {
		logger.Warn("slack: post plan-verdict outcome ack failed", "error", ackErr)
	}
	return handleEventResult{OK: true}
}

// resolveOrClaimSession implements doc.go's own numbered design: an
// existing mapping resolves directly; a brand-new thread creates a bare
// session, races to claim the mapping, and falls back to the winner's
// session id on a lost claim. ok reports whether the caller should
// continue at all (false on a genuine error, already logged). creator is
// handleEvent's own already-resolved actor ("identities + full
// RBAC", §13.2) -- Valid iff the mentioning Slack user is already linked,
// or was just auto-linked this call; invalid (bot attribution) otherwise,
// exactly matching this function's own PREVIOUS unconditional-bot-
// attribution precedent for the still-unresolved case.
//
// Both paths that resolve to an ALREADY-EXISTING session this event's own
// actor did not just create right here (the existing-mapping branch
// immediately below, and the "lost the race, fall back to the winner"
// branch at the bottom) route through authorizeExistingSessionReply,
// gating this event's eventual addTurn (handleEvent) behind exactly the
// same domain/authz.Authorize(ActionPromptSession) verdict the REST API's
// own POST .../turns endpoint already renders -- this Step's own SECOND
// fix pass, closing a confirmed re-review finding: the existing-mapping
// branch previously returned the resolved session id UNCONDITIONALLY,
// with no authz check at all, unlike the brand-new-thread/
// ActionCreateSession branch below (already gated by this Step's FIRST
// fix pass).
//
// prompt (the normalized reply text, handleEvent's own `prompt` variable)
// is threaded through to authorizeExistingSessionReply below -- LOW audit
// fix (confirmed finding, "a plan-verdict reply from an underprivileged
// actor is denied by this function's own ActionPromptSession gate before
// handleEvent's own plan-specific ActionApprovePlan check ever runs, so
// the denial text/log line don't match the button/Linear equivalents for
// the identical underlying decision"): authorizeExistingSessionReply needs
// it to recognize that shape of reply and choose the matching denial text.
func resolveOrClaimSession(ctx context.Context, deps Deps, logger *slog.Logger, ev slackEvent, channel, key string, creator pgtype.UUID, prompt string) (sessionResolution, bool) {
	existing, err := deps.Threads.Get(ctx, channel, key)
	if err == nil {
		return deps.authorizeExistingSessionReply(ctx, logger, channel, key, existing.SessionID, creator, prompt)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		logger.Error("slack: lookup thread mapping failed", "error", err)
		return sessionResolution{}, false
	}

	// No mapping yet. Only an app_mention may start a brand-new thread --
	// a plain, unmapped "message" event is simply not ours (doc.go's own
	// step 6/7 reasoning).
	//
	// HIGH audit fix ("message-level claim can permanently orphan its own
	// app_mention twin on a brand-new thread"): ReleaseMessageClaim: true
	// here is load-bearing, not decorative -- this is the ONE asymmetric
	// outcome between the two twin event types (see sessionResolution's own
	// ReleaseMessageClaim doc comment for the full reasoning). Without it, a
	// plain "message" twin that wins NewHandler's own message-level claim
	// race on a brand-new thread would hold that claim forever, silently
	// discarding its app_mention sibling -- the ONLY twin that can ever
	// actually create the session -- the moment it arrives afterward.
	if !ev.isAppMention() {
		return sessionResolution{Skip: true, ReleaseMessageClaim: true}, true
	}

	if deps.DefaultRepoName == "" || deps.DefaultRepoURL == "" {
		if ackErr := postAckBounded(ctx, deps.SlackClient, deps.AckTimeout, channel, key, ackNotConfiguredText); ackErr != nil {
			logger.Warn("slack: post not-configured ack failed", "error", ackErr)
		}
		return sessionResolution{Skip: true}, true
	}

	// §13.2 ("identities + full RBAC", §13.2/§13.3) update: creator is
	// no longer trusted unconditionally just because it resolved to a
	// REAL, linked user_id -- that user's own role must still pass
	// domain/authz.Authorize(ActionCreateSession), exactly like the REST
	// /api/sessions handler already requires (create.go's own authorize
	// call). Resource{} is always correct here (no ownership carve-out on
	// create, mirroring create.go's own identical reasoning).
	//
	// Audit-fix batch update ("block unlinked actor state changes"): a
	// still-unlinked (bot-attributed) creator is NO LONGER let through --
	// actorauthz.AuthorizeLinkedActor (unlike AuthorizeResolvedActor) denies
	// immediately when creator.Valid is false, since the magic-link prompt
	// already sent by resolveSlackActor above means this same actor can
	// simply retry the identical mention once their account is linked. See
	// that function's own doc comment for why this is the correct call here
	// (Slack has a pending-link mechanism GitHub does not).
	if !actorauthz.AuthorizeLinkedActor(ctx, logger, authzSurface, deps.IdentityLink.Users, creator, authz.ActionCreateSession, authz.Resource{}) {
		logger.Warn("slack: create-session denied by authz", "channel", channel, "thread_key", key, "user_id", creator.String())
		if ackErr := postAckBounded(ctx, deps.SlackClient, deps.AckTimeout, channel, key, ackNotAuthorizedText); ackErr != nil {
			logger.Warn("slack: post not-authorized ack failed", "error", ackErr)
		}
		return sessionResolution{Skip: true}, true
	}

	bare, cerr := httpapi.CreateSessionCore(ctx, deps.Pool, deps.Sessions, deps.Turns, deps.Environments, deps.AuditLog, deps.Registry, restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceSlack,
		Repos: []restdtos.CreateSessionRequestReposElem{
			{Name: deps.DefaultRepoName, Url: deps.DefaultRepoURL},
		},
	}, creator, deps.EpistemicCheckDefault, deps.RolloutMode, deps.RepoSettings)
	if cerr != nil {
		// (§10 Phase 6, §32): a RolloutRefusal is a PERMANENT
		// policy refusal, never a transient failure -- checked
		// structurally (CreateSessionError.RolloutRefusal), never by
		// string-matching cerr.Message. Returning (sessionResolution{},
		// false) below (the generic branch) propagates up to
		// handleEventResult{OK: false},
		// which releases the webhook-delivery claim for a Slack retry --
		// but retrying would only ever reproduce this SAME refusal, since
		// repo_settings.sessions_enabled does not change between
		// redeliveries of the same event. Mirrors ackNotAuthorizedText's
		// own terminal in-thread ack idiom immediately above instead:
		// post ackNotEnrolledText and return {Skip: true}, true -- ok=true
		// means the webhook-delivery claim is kept (never released),
		// exactly like an authz denial.
		if cerr.RolloutRefusal {
			logger.Warn("slack: create bare session refused: repo not enrolled in cohort rollout", "channel", channel, "thread_key", key)
			if ackErr := postAckBounded(ctx, deps.SlackClient, deps.AckTimeout, channel, key, ackNotEnrolledText); ackErr != nil {
				logger.Warn("slack: post not-enrolled ack failed", "error", ackErr)
			}
			return sessionResolution{Skip: true}, true
		}
		logger.Error("slack: create bare session failed", "status", cerr.Status, "message", cerr.Message)
		return sessionResolution{}, false
	}

	_, won, err := deps.Threads.Claim(ctx, channel, key, bare.ID)
	if err != nil {
		logger.Error("slack: claim thread mapping failed", "error", err)
		return sessionResolution{}, false
	}
	if won {
		return sessionResolution{SessionID: bare.ID, IsNewThread: true}, true
	}

	// Lost the race -- a concurrent first message claimed this thread
	// first. bare.ID is left as a harmless, never-dispatched orphan (see
	// doc.go's own tradeoff note); resolve the WINNER's real session
	// instead and continue exactly like an existing-mapping reply would --
	// including this Step's own authorizeExistingSessionReply gate: this
	// actor was only just authorized to CREATE a session (above), never to
	// prompt the DIFFERENT session a concurrent racer actually won, so the
	// same ActionPromptSession check applies here too.
	winner, err := deps.Threads.Get(ctx, channel, key)
	if err != nil {
		logger.Error("slack: lookup winning thread mapping after lost claim failed", "error", err)
		return sessionResolution{}, false
	}
	return deps.authorizeExistingSessionReply(ctx, logger, channel, key, winner.SessionID, creator, prompt)
}

// authorizeExistingSessionReply gates a session id that this event's own
// actor did NOT just create in resolveOrClaimSession above (either branch
// -- see that function's own doc comment) behind
// domain/authz.Authorize(ActionPromptSession), replying in-thread with
// ackNotAuthorizedReplyText on denial instead of letting handleEvent's own
// addTurn enqueue a turn.
//
// Audit-fix batch update ("block unlinked actor state changes"): a
// still-unlinked (bot-attributed) creator is NO LONGER let through --
// authorizeSessionAction (identity.go) now returns ErrActorNotAuthorized
// immediately for that case (its own top-of-function short-circuit changed
// from "return nil" to "return ErrActorNotAuthorized"), which this function
// handles identically to a resolved-but-insufficient-role denial below: an
// in-thread ackNotAuthorizedReplyText reply, no turn enqueued. The magic-
// link notice (resolveSlackActor's own notice, delivered by the caller
// regardless of this denial) is what tells this actor how to fix it.
//
// ok (the second return value) is false ONLY for a genuine backend error
// authorizeSessionAction hit while checking (MEDIUM audit fix,
// "authorizeSessionAction conflates a genuine backend error with a real
// authorization denial") -- distinct from ErrActorNotAuthorized, a real
// denial, which still returns ok=true (Skip: true) exactly as before this
// fix. See handleEvent's own doc comment for how ok=false here flows into
// the release-the-claim-and-retry path.
//
// prompt is handleEvent's own already-normalized reply text, used ONLY on
// the ErrActorNotAuthorized branch below -- LOW audit fix (confirmed
// finding, "an underprivileged actor's plan-verdict reply is denied HERE,
// by ActionPromptSession, before handleEvent's own plan-specific
// handlePlanVerdict/ActionApprovePlan check ever gets a chance to run, so
// the denial text/log line this function posts don't match the plan-
// specific wording the button path (interactive.go) and Linear's own
// equivalent text-verdict path (webhook.go) give for the identical
// underlying denial"): this gate still runs FIRST for every reply on an
// already-mapped thread, and its own ActionPromptSession check is still
// what actually renders the denial (never loosened or bypassed by the
// check below -- see this function's own "block unlinked actor state
// changes" doc comment above for why that gate must stay authoritative);
// the check below only decides WHICH honest text/log line describes that
// SAME denial, matching the plan-specific one handlePlanVerdict's own
// (still separately exercised, see authz.ActionApprovePlan's own call
// there) denial branch would give were it ever reached directly. deps.
// Plans == nil (never true in production wiring) skips this recognition
// entirely, falling back to the pre-existing generic wording.
func (deps Deps) authorizeExistingSessionReply(ctx context.Context, logger *slog.Logger, channel, key string, sessionID, creator pgtype.UUID, prompt string) (sessionResolution, bool) {
	err := deps.authorizeSessionAction(ctx, logger, sessionID, creator, authz.ActionPromptSession)
	if err == nil {
		return sessionResolution{SessionID: sessionID}, true
	}
	if errors.Is(err, ErrActorNotAuthorized) {
		ackText := ackNotAuthorizedReplyText
		isPlanVerdictReply := false
		if deps.Plans != nil {
			if planID, hasAwaiting := findAwaitingApprovalPlanID(ctx, logger, deps.Plans, sessionID); hasAwaiting {
				if _, verdictOK := plandomain.MatchVerdict(prompt); verdictOK {
					isPlanVerdictReply = true
					ackText = slackPlanForbiddenText
					logger.Warn("slack: text plan verdict denied by outer prompt-session gate", "channel", channel, "thread_key", key, "session_id", sessionID.String(), "plan_id", planID.String(), "user_id", creator.String())
				}
			}
		}
		if !isPlanVerdictReply {
			logger.Warn("slack: reply denied by authz", "channel", channel, "thread_key", key, "user_id", creator.String())
		}
		if ackErr := postAckBounded(ctx, deps.SlackClient, deps.AckTimeout, channel, key, ackText); ackErr != nil {
			logger.Warn("slack: post not-authorized-reply ack failed", "error", ackErr)
		}
		return sessionResolution{Skip: true}, true
	}
	// MEDIUM audit fix: a genuine backend failure while checking
	// authorization (already logged inside authorizeSessionAction) --
	// distinct from the real denial above -- must flow into the SAME
	// release-the-claim-and-retry path H2 already wired up for every other
	// post-claim failure, not be silently treated as "skip, no release"
	// the way a one-off DB blip previously was.
	return sessionResolution{}, false
}

// readBoundedBody reads r.Body (capped via http.MaxBytesReader, mirroring
// httpapi's own identical precedent) BEFORE any signature verification --
// Slack's own signature is computed over the exact raw bytes, so this
// must happen before any JSON decoding attempt (see doc.go's own step 1).
func readBoundedBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return nil, false
	}
	return body, true
}

// verifySlackRequest implements doc.go's own steps 2-3: assemble
// "v0:{timestamp}:{raw body}", verify the signature, then verify
// freshness. Fails closed (401) on any missing header, malformed
// timestamp, invalid signature, or expired timestamp -- never falls back
// to "assume valid".
func verifySlackRequest(w http.ResponseWriter, r *http.Request, body []byte, signingSecret string, window time.Duration, logger *slog.Logger) bool {
	tsHeader := r.Header.Get("X-Slack-Request-Timestamp")
	sigHeader := r.Header.Get("X-Slack-Signature")

	if tsHeader == "" || sigHeader == "" {
		writeError(w, http.StatusUnauthorized, "missing signature headers")
		return false
	}

	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "malformed timestamp header")
		return false
	}

	presentedHex := strings.TrimPrefix(sigHeader, slackWebhookSignatureVersion+"=")
	signedPayload := []byte(slackWebhookSignatureVersion + ":" + tsHeader + ":" + string(body))

	if err := platform.VerifyWebhookSignature([]byte(signingSecret), signedPayload, presentedHex); err != nil {
		logger.Warn("slack: signature verification failed", "error", err)
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return false
	}

	if err := platform.VerifyWebhookTimestamp(ts, time.Now(), window); err != nil {
		logger.Warn("slack: timestamp freshness check failed", "error", err)
		writeError(w, http.StatusUnauthorized, "expired timestamp")
		return false
	}

	return true
}

// writeError writes a minimal {"error": message} JSON body at status --
// mirrors httpapi's own identical helper (this package cannot import
// that one's unexported writeError, and duplicating one tiny function is
// simpler and more honest than exporting an httpapi internal for it).
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeJSON writes v as a JSON body at status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
