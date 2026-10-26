package linear

// This file's types mirror the REAL fields of Linear's own
// AgentSessionEventWebhookPayload GraphQL object (and the AgentSession/
// AgentActivity "*WebhookPayload" child types it embeds), fetched
// directly from Linear's own live, current SDL schema
// (https://raw.githubusercontent.com/linear/linear/refs/heads/master/
// packages/sdk/src/schema.graphql, linked from Linear's own developer
// docs as the authoritative reference) during this Step's investigation
// -- not guessed. Only the fields this package actually reads are
// declared; encoding/json silently ignores every field Linear's real
// payload carries that isn't listed here, so this intentionally stays
// narrow rather than mirroring the schema exhaustively.

// agentSessionEventWebhookPayload is the top-level webhook body for
// Linear's own "AgentSessionEvent" category (Linear-Event header ==
// agentSessionEventType).
type agentSessionEventWebhookPayload struct {
	// Action is "created" or "prompted" -- Linear's own docs, verified
	// live: "There will be two types of actions in the AgentSessionEvent
	// category, denoted by the action field of the payload." A `stop`
	// signal is NOT a third action value -- it arrives as a `prompted`
	// event whose AgentActivity.Signal == "stop" (see webhook.go).
	Action string `json:"action"`

	// Type is AgentSessionEventWebhookPayload's own "type of resource"
	// field -- expected to equal agentSessionEventType too, checked
	// defensively alongside the Linear-Event header itself (belt and
	// suspenders: the header is this package's primary routing signal,
	// since it is available before the body is even parsed).
	Type string `json:"type"`

	// OrganizationID keys both linear_agent_sessions.organization_id (at
	// claim time) and linear_installations.organization_id (the outbound
	// acknowledgment's own token lookup) -- the SAME id
	// ViewerAndOrganization fetches at install time.
	OrganizationID string `json:"organizationId"`

	// WebhookTimestamp is a Unix timestamp IN MILLISECONDS (Linear's own
	// schema: "webhookTimestamp: Float!... Unix timestamp in milliseconds
	// when the webhook was sent") -- NOT seconds, unlike most providers;
	// webhook.go converts before calling platform.VerifyWebhookTimestamp.
	WebhookTimestamp float64 `json:"webhookTimestamp"`

	// AgentSession is always present (required for both actions).
	AgentSession agentSessionWebhookPayload `json:"agentSession"`

	// AgentActivity is present only for `prompted` events ("The agent
	// activity that was created" -- nullable in Linear's own schema).
	AgentActivity *agentActivityWebhookPayload `json:"agentActivity"`

	// PromptContext is present only for `created` events -- "A formatted
	// prompt string containing the relevant context for the agent
	// session, including issue details, comments, and guidance." Text
	// anyone who can write on the issue wrote, so delegationPrompt
	// (prompt.go) quotes it whole as data (§5.2), never as the session's
	// instruction.
	PromptContext *string `json:"promptContext"`
}

// agentSessionWebhookPayload mirrors Linear's own AgentSessionWebhookPayload
// object (the CHILD "webhook payload" shape embedded in the event, distinct
// from the full AgentSession GraphQL object -- Linear's own schema defines
// both separately).
type agentSessionWebhookPayload struct {
	// ID is Linear's own AgentSession identity -- the key
	// linear_agent_sessions.agent_session_id is claimed/looked-up by (see
	// migrations/000030_linear_agent_sessions.up.sql's own doc comment).
	ID string `json:"id"`

	// Issue is present when this session is attached to an issue (the
	// common "delegated an issue" / "@mentioned in an issue" case) -- nil
	// for e.g. a direct-chat/document-comment session. webhook.go's own
	// handleCreated still creates a Narvi session either way (Linear's own
	// promptContext carries whatever context DOES exist regardless); a nil
	// Issue only means the constructed session Title falls back to no
	// issue-derived text.
	Issue *issueChildWebhookPayload `json:"issue"`

	// URL is the Linear-hosted URL of this agent session/issue thread --
	// used only for this Step's own log lines, never persisted.
	URL string `json:"url"`

	// CreatorID is Linear's own "ID of human user; unset if
	// automation-initiated" (AgentSessionWebhookPayload.creatorId,
	// verified against Linear's real, current GraphQL schema during this
	// Step's investigation) -- §13.2's ("identities + full RBAC", §13.2)
	// own auto-linking wiring: the external_id a `created` event's own
	// session-creation actor resolves against. Nil/empty means no
	// responsible human at all (a purely automation-initiated session) --
	// webhook.go's own resolveLinearActor treats that as bot attribution
	// unconditionally, with no identity lookup attempted (there is no
	// external id to look up).
	CreatorID *string `json:"creatorId"`

	// Comment is the comment the session was started from (Linear's
	// AgentSessionWebhookPayload.comment, a CommentChildWebhookPayload):
	// set when someone mentioned the agent in a comment, nil when the
	// issue was delegated without one. delegatorRequest (prompt.go) takes
	// its body as the session's instruction only when its UserID equals
	// CreatorID.
	Comment *commentChildWebhookPayload `json:"comment"`
}

// commentChildWebhookPayload mirrors Linear's own CommentChildWebhookPayload
// -- only the two fields delegatorRequest reads.
type commentChildWebhookPayload struct {
	Body string `json:"body"`

	// UserID is Linear's "ID of the user who created this comment",
	// nullable there (empty here): a comment with no user id is never
	// attributed to anyone.
	UserID string `json:"userId"`
}

// issueChildWebhookPayload mirrors Linear's own
// IssueWithDescriptionChildWebhookPayload -- only the fields this
// package's own session Title construction reads.
type issueChildWebhookPayload struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
}

// agentActivityWebhookPayload mirrors Linear's own
// AgentActivityWebhookPayload object.
type agentActivityWebhookPayload struct {
	// Content is a JSONObject in Linear's own schema (deliberately
	// untyped there -- "This object is not strictly typed"); this package
	// only ever reads Content.Body (the `prompt`-type activity's own
	// message text, Linear's own docs: "This message is located in the
	// agentActivity.body field of the webhook payload" -- shorthand,
	// confirmed against the real schema, for agentActivity.content.body).
	Content agentActivityContent `json:"content"`

	// Signal is an optional modifier -- this Step's own only use is
	// detecting Linear's own "stop" signal (Linear's docs: a `prompted`
	// event whose Signal == "stop" means the user cancelled the task).
	Signal *string `json:"signal"`

	// UserID is Linear's own "ID of the user who created this agent
	// activity" (AgentActivityWebhookPayload.userId, verified against
	// Linear's real, current GraphQL schema during this Step's
	// investigation, REQUIRED/non-null there -- unlike AgentSession's own
	// nullable creatorId, every individual activity has a real
	// originating user; §8.10's own payload.go doc comment already notes
	// "An agent cannot generate a prompt type activity", i.e. a
	// `prompted` event's own activity is always human-authored). §13.2's
	// ("identities + full RBAC", §13.2) own auto-linking wiring: the
	// external_id a `prompted` event's own actor (a plan verdict, or an
	// ordinary reply) resolves against -- distinct from, and potentially a
	// DIFFERENT person than, AgentSession's own CreatorID above (multiple
	// people can reply in the same Linear comment thread).
	UserID string `json:"userId"`
}

// agentActivityContent mirrors the "prompt"-type activity content shape
// (Linear's own docs: `{"type": "prompt", "body": "..."}` for a
// user-generated message) -- the only content type a `prompted` webhook
// event's own AgentActivity ever carries (Linear's docs: "An agent cannot
// generate a prompt type activity", i.e. this is always inbound).
type agentActivityContent struct {
	Type string `json:"type"`
	Body string `json:"body"`
}

// stopSignal is the exact Signal value Linear's own docs use for a
// cancelled task ("Stopped: delivered as a prompted event with
// agentActivity.signal: 'stop'").
const stopSignal = "stop"
