// This file (blockkit.go) implements §8.1's ("plan mode, cross-channel",
// §8.1/§13.3) own Slack-specific additions: real interactive Block Kit
// messages for a plan awaiting approval, chat.update to reflect a rendered
// decision on that same message (whichever channel actually decided), and
// views.open for the "Request changes" feedback modal. All three request/
// response shapes below were verified against Slack's own current, real
// Web API reference documentation during this Step's own investigation
// (docs.slack.dev/reference/methods/{chat.update,views.open},
// docs.slack.dev/reference/interaction-payloads/block_actions-payload) --
// not invented from a summary, matching this codebase's own established
// "verify against the real API" discipline (see client.go's own doc.go).
//
// # Button value encoding
//
// Each of the three buttons on the approval-request message (Approve &
// build / Request changes / Reject) carries the SAME EncodePlanActionValue
// output as its own "value" -- a small, delimited "planID|sessionID"
// string (deliberately not JSON: two UUIDs joined by a byte ('|') neither
// can ever contain, so no escaping is needed) -- rather than relying on
// Slack's own response_url alone, which Slack's docs describe as
// short-lived. internal/adapters/inbound/slack's own interactivity handler
// (interactive.go) is the ONLY other consumer of DecodePlanActionValue;
// living here, in the package that also constructs the value, keeps the
// encode/decode pair next to each other rather than splitting the format's
// definition across two packages.

package slackapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/narvidev/narvi/internal/domain/framecut"
)

// Block Kit interactive action_ids -- the fixed vocabulary this Step's own
// three approval-request buttons use, and the ONLY action_ids internal/
// adapters/inbound/slack's own block_actions dispatch (interactive.go)
// recognizes.
const (
	ActionApprovePlan        = "approve_plan"
	ActionRejectPlan         = "reject_plan"
	ActionRequestChangesPlan = "request_changes_plan"
)

// RequestChangesCallbackID/RequestChangesBlockID/RequestChangesActionID are
// the fixed identifiers the "Request changes" feedback modal (OpenView
// below) uses on its own single input block -- internal/adapters/inbound/
// slack's own view_submission handling (interactive.go) reads the
// submitted text back out by this SAME BlockID/ActionID pair.
const (
	RequestChangesCallbackID = "plan_request_changes"
	RequestChangesBlockID    = "feedback_block"
	RequestChangesActionID   = "feedback_input"
)

// planActionValueSeparator joins planID/sessionID into one button "value"
// string -- a byte neither UUID can ever contain, so this format needs no
// escaping and no JSON encode/decode overhead.
const planActionValueSeparator = "|"

// EncodePlanActionValue builds a Block Kit button's own "value" string
// identifying which plan/session it acts on -- the exact inverse of
// DecodePlanActionValue below.
func EncodePlanActionValue(planID, sessionID string) string {
	return planID + planActionValueSeparator + sessionID
}

// DecodePlanActionValue parses a button's own "value" string (as produced
// by EncodePlanActionValue) back into (planID, sessionID). ok is false for
// anything not shaped exactly "planID|sessionID" (defensive against a
// malformed/tampered payload -- Slack echoes back exactly what this
// package set, so this should be unreachable in practice, but a webhook
// body is untrusted input regardless of who claims to have sent it).
func DecodePlanActionValue(value string) (planID, sessionID string, ok bool) {
	parts := strings.SplitN(value, planActionValueSeparator, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// --- Block Kit block/element shapes -- deliberately the small subset this
// Step's own three messages need, not a general-purpose Block Kit library.

type textObject struct {
	Type string `json:"type"` // "plain_text" or "mrkdwn"
	Text string `json:"text"`
}

type sectionBlock struct {
	Type string      `json:"type"` // "section"
	Text *textObject `json:"text,omitempty"`
}

type dividerBlock struct {
	Type string `json:"type"` // "divider"
}

type contextBlock struct {
	Type     string       `json:"type"` // "context"
	Elements []textObject `json:"elements"`
}

type buttonElement struct {
	Type     string     `json:"type"` // "button"
	Text     textObject `json:"text"`
	ActionID string     `json:"action_id"`
	Value    string     `json:"value"`
	Style    string     `json:"style,omitempty"` // "primary"/"danger"/"" (default)
}

type actionsBlock struct {
	Type     string          `json:"type"` // "actions"
	Elements []buttonElement `json:"elements"`
}

// maxSectionTextRunes states the invariant this package actually owes
// Slack: how much of a plan's own FINAL, rendered (post-MarkdownToMrkdwn)
// content this package ever embeds in one Block Kit section's own "text"
// -- Slack's own real limit for a section's plain_text/mrkdwn text object
// is 3000 characters; this stays comfortably under that so the header/
// context/actions blocks around it are never at risk of pushing the WHOLE
// message over Slack's own separate, larger total-payload limit either.
// This invariant is about the text Slack actually receives -- it holds
// regardless of where truncation itself happens to be implemented; see
// maxRawTextRunes and truncateForSection below for that.
const maxSectionTextRunes = 2800

// maxRawTextRunes bounds truncateForSection's own cut point on the RAW
// markdown text, BEFORE MarkdownToMrkdwn ever runs on it (audit-fix batch:
// "truncation tag-boundary safety" finding -- LOW). Truncating the raw
// text rather than the already-converted mrkdwn means a chopped Markdown
// construct (e.g. a link's own unterminated "[text](url" with no closing
// paren, or a bold span's dangling "**" opener) degrades to harmless
// leftover plain-text-ish characters -- or an incomplete pattern that
// MarkdownToMrkdwn's own regexes then simply never match at all, rendering
// as literal text -- once conversion runs, rather than ever leaving a
// truncated, dangling, unterminated Slack "<url|label>" TAG in what is
// actually posted to Slack.
//
// This bound must still guarantee maxSectionTextRunes's own invariant
// above holds on the FINAL, converted text -- even though MarkdownToMrkdwn
// can make text LONGER than its raw input, not just shorter. Concretely:
// mdLinkPattern/mdBoldPattern/mdHeadingPattern (mrkdwn_outbound.go) each
// either shrink text (their converted form is never longer than the raw
// Markdown they replace) or leave it roughly unchanged -- but
// escapeMrkdwnEntities, which runs FIRST over the whole raw text, can grow
// it: a single raw "&" becomes the 5-rune entity "&amp;", this converter's
// single largest per-rune growth factor ("<"/">" each grow to a smaller
// 4-rune "&lt;"/"&gt;"). A raw input consisting entirely of "&" characters
// is therefore this converter's own mathematical worst case -- N raw runes
// become EXACTLY 5*N converted runes, since no other pattern in this file
// (no "**"/"__", no "[...](...)", no "#") can ever match a string of bare
// "&" characters.
//
// maxRawTextRunes is sized so even that worst case, PLUS the up-to-15-rune
// "\n\n_(truncated)_" marker truncateForSection appends when it truncates
// (itself immune to further growth -- it carries no "&"/"<"/">" of its
// own), still lands at or under maxSectionTextRunes:
//
//	5*550 + 15 = 2765 <= 2800 (maxSectionTextRunes) <= 3000 (Slack's real limit)
//
// See TestPostPlanApprovalMessage_TruncationHappensOnRawMarkdown
// (blockkit_test.go) for the empirical proof against this exact worst
// case, plus a realistic link-straddling-the-cutoff case.
const maxRawTextRunes = 550

// init asserts the sizing math documented on maxRawTextRunes above still
// holds -- turning that doc comment's arithmetic into a real, enforced
// invariant (rather than one only a human re-checks by eye) so a future
// change to either constant that breaks the guarantee fails loudly at
// package load instead of silently shipping a truncation that overruns
// Slack's own real section-text limit.
func init() {
	const worstCaseConvertedRunes = 5*maxRawTextRunes + 15
	if worstCaseConvertedRunes > maxSectionTextRunes {
		panic(fmt.Sprintf(
			"slackapi: maxRawTextRunes=%d/maxSectionTextRunes=%d invariant violated: "+
				"worst-case converted text is %d runes, want <= %d",
			maxRawTextRunes, maxSectionTextRunes, worstCaseConvertedRunes, maxSectionTextRunes))
	}
}

// truncateForSection bounds RAW markdown text (BEFORE MarkdownToMrkdwn
// conversion -- see maxRawTextRunes's own doc comment above for why
// truncation happens here, pre-conversion, rather than on the converted
// mrkdwn output) to maxRawTextRunes runes, appending an honest
// "(truncated)" marker when it does -- never silently drops content
// without saying so.
func truncateForSection(text string) string {
	runes := []rune(text)
	if len(runes) <= maxRawTextRunes {
		return text
	}
	return string(runes[:maxRawTextRunes]) + "\n\n_(truncated)_"
}

// PlanApprovalPayload is the JSON shape this package expects to find in an
// outbox entry's own payload column for a ports.NotificationKindSlackPlanApproval
// row -- enqueued by internal/app/sessionactor at plan-mode turn-completion
// time (internal/app/sessionactor/outboxenqueue.go), carrying the plan's own
// identity, the originating thread's channel+thread_ts (from the session's
// own reverse-looked-up slack_thread_sessions row -- unchanged from how the
// existing generic Payload already sources these two fields), and Text (the
// plan's own rendered content -- steps/scope, best-effort extracted from the
// producing turn's own event stream; see outboxenqueue.go's own doc comment
// for the extraction this Step adds).
//
// Cut is the plan's cut report (plan.Final.Cut, technical plan §6.1): set
// when the plan's text is a frame the sandbox-agent cut on its way to the
// control plane. Such a plan cannot be approved (httpapi.ErrPlanCut), so
// its message offers no Approve (PostPlanApprovalMessage). Omitted when
// nil, so a payload enqueued by a binary that knows no cut decodes the
// same, and one this binary enqueues for a whole plan is byte-identical to
// before.
type PlanApprovalPayload struct {
	PlanID    string        `json:"plan_id"`
	SessionID string        `json:"session_id"`
	ChannelID string        `json:"channel_id"`
	ThreadTS  string        `json:"thread_ts"`
	Version   int           `json:"version"`
	Text      string        `json:"text"`
	Cut       *framecut.Cut `json:"cut,omitempty"`
}

// PlanDecidedPayload is the JSON shape this package expects to find in an
// outbox entry's own payload column for a ports.NotificationKindSlackPlanDecided
// row -- enqueued by httpapi.DecidePlanOnTx (decideplan.go) whenever a plan
// with a stored slack_channel_id/slack_message_ts transitions, regardless of
// which entry point (Slack itself, Linear, or web) actually decided it.
type PlanDecidedPayload struct {
	ChannelID string `json:"channel_id"`
	MessageTS string `json:"message_ts"`
	Text      string `json:"text"`
}

// DigestPayload is the JSON shape this package expects to find in an
// outbox entry's own payload column for a ports.NotificationKindSlackDigest
// row (§21.3) -- enqueued by internal/app/digest.Pump. Text is
// ALREADY fully rendered (internal/domain/digest.Render's own
// deterministic output, Slack mrkdwn dialect) -- this package does no
// further conversion/templating of it, unlike PlanApprovalPayload's own
// Text field (which still goes through MarkdownToMrkdwn).
type DigestPayload struct {
	ChannelID string `json:"channel_id"`
	Text      string `json:"text"`
}

// postMessageWithBlocksRequest is chat.postMessage's own real request body
// shape when blocks are included -- Blocks is `[]any` (rather than a single
// concrete block type) since Block Kit blocks are a heterogeneous union;
// each element here is one of this file's own *Block/*Element struct
// literals, which json.Marshal renders correctly by its own concrete type.
type postMessageWithBlocksRequest struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts,omitempty"`
	Text     string `json:"text"`
	Blocks   []any  `json:"blocks,omitempty"`
}

// postMessageWithBlocksResponse is chat.postMessage's own real response
// envelope when the call succeeds: "ok", plus (per Slack's own documented
// response shape) the message's own real "channel" and "ts" -- this Step's
// own reason for calling chat.postMessage directly here rather than
// reusing Deliver's own plain-text Payload/postMessageRequest above: THIS
// caller needs channel+ts back, to persist onto the plans row (see this
// package's own PostPlanApprovalMessage doc comment).
type postMessageWithBlocksResponse struct {
	Ok      bool   `json:"ok"`
	Error   string `json:"error"`
	Channel string `json:"channel"`
	Ts      string `json:"ts"`
}

// PostPlanApprovalMessage posts payload's plan-approval-request message
// with real interactive Block Kit buttons (Approve & build / Request
// changes / Reject, each carrying EncodePlanActionValue(payload.PlanID,
// payload.SessionID) as its own button value) into payload.ChannelID,
// threaded under payload.ThreadTS. Returns the message's own REAL channel+ts
// from Slack's own response (never derived/guessed) -- the caller (internal/
// app/outboxworker) persists these onto the plans row via
// PlanStore.SetSlackMessageRef so a later decision (from any entry point)
// can chat.update this exact message.
//
// Block composition (this package's own judgment, kept deliberately simple
// per this Step's own brief): a header section naming the version, a
// section carrying the plan's own rendered content (truncated via
// truncateForSection), a context line, a divider, then the actions row.
//
// Audit-fix batch (§8.10's own "missing outbound mrkdwn contract" finding):
// payload.Text is an LLM's own freeform Markdown, embedded here into a
// mrkdwn-typed Block Kit text object -- Markdown syntax like "**bold**" or
// "[text](url)" does not render in Slack's own mrkdwn dialect, so it is run
// through MarkdownToMrkdwn (mrkdwn_outbound.go).
//
// A LATER audit-fix batch ("truncation tag-boundary safety" finding, LOW)
// reordered this to truncate BEFORE conversion (truncateForSection(payload.
// Text), then MarkdownToMrkdwn on the result) rather than after: truncating
// the already-converted mrkdwn risked chopping a generated "<url|label>"
// tag mid-construct, leaving a dangling, unterminated "<" fragment in what
// is actually posted. Truncating the raw Markdown first means a chopped
// construct degrades to harmless leftover plain-text-ish characters (or an
// incomplete pattern MarkdownToMrkdwn's own regexes simply never match)
// instead -- see maxRawTextRunes's own doc comment above for why the
// truncation bound itself is sized the way it is, so this reordering still
// keeps the FINAL, converted text within Slack's own real section-text
// length limit.
//
// A cut plan (payload.Cut set, technical plan §6.1) is posted without the
// Approve & build button, whose click could only be refused
// (httpapi.ErrPlanCut): Request changes, asking for a shorter plan, is the
// way on, and Reject stays. Its context line gives the cut's reason
// (framecut.Reason) in place of "Awaiting approval". A cut is settled by
// the time this message is posted -- it goes out as the plan's turn
// completes, and no frame adds a row once its turn has ended -- so a
// button offered here would never become approvable.
func (c *Client) PostPlanApprovalMessage(ctx context.Context, payload PlanApprovalPayload) (channel, ts string, err error) {
	value := EncodePlanActionValue(payload.PlanID, payload.SessionID)

	contextText := "Awaiting approval — first verdict wins, across Slack/Linear/web."
	var buttons []buttonElement
	if payload.Cut != nil {
		contextText = framecut.Reason(payload.Cut)
	} else {
		buttons = append(buttons, buttonElement{Type: "button", Text: textObject{Type: "plain_text", Text: "Approve & build"}, ActionID: ActionApprovePlan, Value: value, Style: "primary"})
	}
	buttons = append(buttons,
		buttonElement{Type: "button", Text: textObject{Type: "plain_text", Text: "Request changes"}, ActionID: ActionRequestChangesPlan, Value: value},
		buttonElement{Type: "button", Text: textObject{Type: "plain_text", Text: "Reject"}, ActionID: ActionRejectPlan, Value: value, Style: "danger"},
	)

	blocks := []any{
		sectionBlock{Type: "section", Text: &textObject{Type: "mrkdwn", Text: fmt.Sprintf("*Plan v%d ready for review*", payload.Version)}},
		sectionBlock{Type: "section", Text: &textObject{Type: "mrkdwn", Text: MarkdownToMrkdwn(truncateForSection(payload.Text))}},
		contextBlock{Type: "context", Elements: []textObject{
			{Type: "mrkdwn", Text: contextText},
		}},
		dividerBlock{Type: "divider"},
		actionsBlock{Type: "actions", Elements: buttons},
	}

	reqBody, err := json.Marshal(postMessageWithBlocksRequest{
		Channel:  payload.ChannelID,
		ThreadTS: payload.ThreadTS,
		Text:     fmt.Sprintf("Plan v%d is ready for review.", payload.Version), // notification-preview fallback text (Slack's own requirement whenever blocks are present)
		Blocks:   blocks,
	})
	if err != nil {
		return "", "", fmt.Errorf("slackapi: encode chat.postMessage (blocks) request: %w", err)
	}

	var parsed postMessageWithBlocksResponse
	if err := c.doPost(ctx, "/chat.postMessage", reqBody, &parsed); err != nil {
		return "", "", err
	}
	if !parsed.Ok {
		return "", "", &DeliveryError{SlackError: parsed.Error}
	}
	return parsed.Channel, parsed.Ts, nil
}

// PostMessage posts plain text (already fully rendered mrkdwn, e.g.
// internal/domain/digest.Render's own deterministic output,
// §21.3) to channel -- no Block Kit, no interactive elements, exactly
// the shape a compliance/status artifact needs and nothing more. Reuses
// postMessageWithBlocksRequest with Blocks left nil (its own `omitempty`
// tag), never a second, parallel chat.postMessage request struct for
// what is really the SAME endpoint with an optional field omitted.
func (c *Client) PostMessage(ctx context.Context, channel, text string) (channelOut, ts string, err error) {
	reqBody, err := json.Marshal(postMessageWithBlocksRequest{Channel: channel, Text: text})
	if err != nil {
		return "", "", fmt.Errorf("slackapi: encode chat.postMessage request: %w", err)
	}

	var parsed postMessageWithBlocksResponse
	if err := c.doPost(ctx, "/chat.postMessage", reqBody, &parsed); err != nil {
		return "", "", err
	}
	if !parsed.Ok {
		return "", "", &DeliveryError{SlackError: parsed.Error}
	}
	return parsed.Channel, parsed.Ts, nil
}

// postThreadMessageRequest is chat.postMessage's own real request body
// shape for a plain-text, non-Block-Kit reply threaded under an existing
// message -- mirrors postMessageWithBlocksRequest's own Channel/ThreadTS/
// Text fields with Blocks always omitted (this call never sends any).
type postThreadMessageRequest struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts,omitempty"`
	Text     string `json:"text"`
}

// PostAck posts a single plain-text chat.postMessage reply into channel,
// threaded under threadTS -- this package's own former sibling, internal/
// adapters/inbound/slack/ack.go's own private ackClient.postAck, folded in
// here as part of retiring that second, independently-constructed client
// (§30.3's "one client per provider"). Unlike PostPlanApprovalMessage/
// PostMessage above, this caller never needs the posted message's own
// channel/ts back (there is nothing later that would chat.update THIS
// message), so it returns only an error.
func (c *Client) PostAck(ctx context.Context, channel, threadTS, text string) error {
	reqBody, err := json.Marshal(postThreadMessageRequest{Channel: channel, ThreadTS: threadTS, Text: text})
	if err != nil {
		return fmt.Errorf("slackapi: encode chat.postMessage request: %w", err)
	}

	var parsed postMessageResponse
	if err := c.doPost(ctx, "/chat.postMessage", reqBody, &parsed); err != nil {
		return err
	}
	if !parsed.Ok {
		return &DeliveryError{SlackError: parsed.Error}
	}
	return nil
}

// chatUpdateRequest is chat.update's own real request body shape.
// Deliberately carries NO "blocks" field at all: Slack's own docs state
// that omitting blocks while supplying text REMOVES any existing blocks (so
// this Step never bothers constructing a "buttons removed" block set of its
// own -- the absence of Blocks here already achieves exactly that).
type chatUpdateRequest struct {
	Channel string `json:"channel"`
	Ts      string `json:"ts"`
	Text    string `json:"text"`
}

// UpdateMessage calls chat.update against an existing message (channel+ts,
// as returned by an earlier PostPlanApprovalMessage call and persisted via
// PlanStore.SetSlackMessageRef) to reflect a plan's final decided outcome --
// used both when Slack's own button click decided it (grey out/replace the
// buttons with the outcome) and when a DIFFERENT channel (Linear, web)
// decided it first (replace the still-pending buttons with an honest
// "already decided elsewhere" outcome line), AND for the plan-supersession
// notification (internal/app/sessionactor/planrecord.go's own audit-fix
// addition) -- all three share this one call site, so text is run through
// MarkdownToMrkdwn (mrkdwn_outbound.go, §8.10's own audit-fix finding)
// exactly once here rather than requiring each caller to remember to.
func (c *Client) UpdateMessage(ctx context.Context, channel, ts, text string) error {
	reqBody, err := json.Marshal(chatUpdateRequest{Channel: channel, Ts: ts, Text: MarkdownToMrkdwn(text)})
	if err != nil {
		return fmt.Errorf("slackapi: encode chat.update request: %w", err)
	}

	var parsed postMessageResponse
	if err := c.doPost(ctx, "/chat.update", reqBody, &parsed); err != nil {
		return err
	}
	if !parsed.Ok {
		return &DeliveryError{SlackError: parsed.Error}
	}
	return nil
}

// modalView is views.open's own real "view" payload shape (verified
// against Slack's own current views.open reference doc) -- deliberately
// only the fields this Step's own single-input feedback modal needs.
type modalView struct {
	Type            string     `json:"type"` // "modal"
	CallbackID      string     `json:"callback_id"`
	PrivateMetadata string     `json:"private_metadata"`
	Title           textObject `json:"title"`
	Submit          textObject `json:"submit"`
	Close           textObject `json:"close"`
	Blocks          []any      `json:"blocks"`
}

type plainTextInputElement struct {
	Type      string `json:"type"` // "plain_text_input"
	ActionID  string `json:"action_id"`
	Multiline bool   `json:"multiline"`
}

type inputBlock struct {
	Type    string                `json:"type"` // "input"
	BlockID string                `json:"block_id"`
	Label   textObject            `json:"label"`
	Element plainTextInputElement `json:"element"`
}

// viewsOpenRequest is views.open's own real top-level request body shape:
// trigger_id (Slack's own short-lived, ~3s-valid exchange token from the
// inbound block_actions payload that triggered this) plus the view payload
// itself.
type viewsOpenRequest struct {
	TriggerID string    `json:"trigger_id"`
	View      modalView `json:"view"`
}

// OpenView opens the "Request changes" feedback modal via a real
// views.open call, using triggerID from the inbound block_actions
// interaction that just fired (valid for only a few seconds -- this call
// must happen promptly, before responding to that interaction). planID/
// sessionID are encoded into the view's own private_metadata (max 255
// characters per Slack's own documented limit -- two UUIDs joined by
// EncodePlanActionValue's separator comfortably fits), so the LATER
// view_submission payload carries them back without a second DB round trip
// (this Step's own brief, point 2).
func (c *Client) OpenView(ctx context.Context, triggerID, planID, sessionID string) error {
	reqBody, err := json.Marshal(viewsOpenRequest{
		TriggerID: triggerID,
		View: modalView{
			Type:            "modal",
			CallbackID:      RequestChangesCallbackID,
			PrivateMetadata: EncodePlanActionValue(planID, sessionID),
			Title:           textObject{Type: "plain_text", Text: "Request changes"},
			Submit:          textObject{Type: "plain_text", Text: "Submit"},
			Close:           textObject{Type: "plain_text", Text: "Cancel"},
			Blocks: []any{
				inputBlock{
					Type:    "input",
					BlockID: RequestChangesBlockID,
					Label:   textObject{Type: "plain_text", Text: "What should change?"},
					Element: plainTextInputElement{
						Type:      "plain_text_input",
						ActionID:  RequestChangesActionID,
						Multiline: true,
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("slackapi: encode views.open request: %w", err)
	}

	var parsed postMessageResponse
	if err := c.doPost(ctx, "/views.open", reqBody, &parsed); err != nil {
		return err
	}
	if !parsed.Ok {
		return &DeliveryError{SlackError: parsed.Error}
	}
	return nil
}

// postEphemeralRequest is chat.postEphemeral's own real request body shape
// (docs.slack.dev/reference/methods/chat.postEphemeral) -- channel + user
// (the ONE person who can ever see this message) + text, optionally
// threaded via thread_ts exactly like chatUpdateRequest above.
type postEphemeralRequest struct {
	Channel  string `json:"channel"`
	User     string `json:"user"`
	ThreadTS string `json:"thread_ts,omitempty"`
	Text     string `json:"text"`
}

// PostEphemeral posts text into channel via chat.postEphemeral, visible
// ONLY to userID -- §13.2's own security-remediation addition
// ("identities + full RBAC", §13.2): a confirmed review finding proved
// that appending the magic-link identity-link notice to this package's
// own whole-channel-visible UpdateMessage/PostPlanApprovalMessage text let
// ANY other member of a shared channel who already had an authenticated
// Narvi web session open the link first and get the pending identity
// permanently linked to their OWN account instead of its rightful
// owner's. chat.postEphemeral is Slack's own documented mechanism for a
// message only the named user (never anyone else viewing the same
// channel/thread) can ever see -- used by internal/adapters/inbound/
// slack/interactive.go's own decideAndUpdateMessage to deliver that
// notice privately to the clicking user instead.
func (c *Client) PostEphemeral(ctx context.Context, channel, userID, threadTS, text string) error {
	reqBody, err := json.Marshal(postEphemeralRequest{Channel: channel, User: userID, ThreadTS: threadTS, Text: text})
	if err != nil {
		return fmt.Errorf("slackapi: encode chat.postEphemeral request: %w", err)
	}

	var parsed postMessageResponse
	if err := c.doPost(ctx, "/chat.postEphemeral", reqBody, &parsed); err != nil {
		return err
	}
	if !parsed.Ok {
		return &DeliveryError{SlackError: parsed.Error}
	}
	return nil
}

// doPost is the small shared HTTP mechanics every method in this file
// (PostPlanApprovalMessage/UpdateMessage/OpenView) uses: POST reqBody (a
// pre-marshaled JSON body) to c.apiBaseURL+path, authenticated with this
// Client's own bot token, decoding the bounded response body into out.
// Mirrors Deliver's own identical request/response mechanics (client.go) --
// factored out here since three new methods would otherwise repeat it
// three times.
func (c *Client) doPost(ctx context.Context, path string, reqBody []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBaseURL+path, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("slackapi: build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("slackapi: %s request failed: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return fmt.Errorf("slackapi: read %s response: %w", path, err)
	}

	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("slackapi: decode %s response: %w", path, err)
	}
	return nil
}
