// This file implements the Decorator itself. See doc.go for why this is a
// client-method decorator rather than a transport gate, and why the
// repository it checks is resolved once, at construction, rather than per
// call.

package shadowslack

import (
	"context"
	"errors"

	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/shadowledger"
)

// Client is every Slack Web API operation internal/adapters/inbound/slack
// needs, live or decorated -- the interface that package's own Deps.
// SlackClient/InteractiveDeps.SlackClient are typed against, so that
// package can no longer construct a client of its own (§30.3's own
// "ingress packages lose the right to construct clients") and instead
// receives whichever implementation production wiring hands it.
//
// *slackapi.Client satisfies this directly (structural typing, no adapter
// needed) and is what *Decorator wraps as its own live implementation;
// *Decorator satisfies it too, decorating every write.
type Client interface {
	// GetUserEmail is a read, forwarded to the live client unchanged --
	// see doc.go's own "what is decorated, and what is not". It returns
	// users.info's profile email with the membership facts that decide
	// whether it is verified (slackapi.UserEmail.Verified, §13.2).
	GetUserEmail(ctx context.Context, userID string) (user slackapi.UserEmail, ok bool, err error)

	// PostAck posts a plain-text chat.postMessage reply, threaded under
	// threadTS -- internal/adapters/inbound/slack's own in-thread ack
	// (formerly ack.go's private ackClient.postAck).
	PostAck(ctx context.Context, channel, threadTS, text string) error

	// PostIdentityLinkNotice posts the identity-link prompt, visible only
	// to userID. Separate from PostEphemeral because its text carries a
	// live magic-link nonce that must never reach the ledger -- see the
	// Decorator method's own doc comment. Use this for the identity-link
	// notice and nothing else.
	PostIdentityLinkNotice(ctx context.Context, channel, userID, threadTS, text string) error

	// PostEphemeral posts a chat.postEphemeral message visible only to
	// userID -- the denial notices Slack ingress posts. NEVER the
	// identity-link notice: its text is recorded verbatim into a
	// permanent ledger, so a secret-bearing body must use
	// PostIdentityLinkNotice above.
	PostEphemeral(ctx context.Context, channel, userID, threadTS, text string) error

	// UpdateMessage calls chat.update against an existing message --
	// interactive.go's own plan-decision-outcome rewrite.
	UpdateMessage(ctx context.Context, channel, ts, text string) error

	// OpenView calls views.open -- interactive.go's own "Request changes"
	// feedback modal.
	OpenView(ctx context.Context, triggerID, planID, sessionID string) error
}

// Decorator wraps a live Client and suppresses its writes when
// repoFullName is not currently live.
type Decorator struct {
	live         Client
	ledger       shadowledger.Store
	repoFullName string

	// isLive reports whether repoFullName may really be written to right
	// now. Resolved on every call, never cached -- see doc.go's own "why
	// one fixed repository, not a per-call one".
	isLive func(ctx context.Context, repoFullName string) bool
}

// New builds a Decorator. Every argument is required, mirroring
// shadowscm.New's identical refusal of a convenience default: a
// pass-through obtained by omission is the failure mode this layer exists
// to remove.
func New(live Client, ledger shadowledger.Store, repoFullName string, isLive func(context.Context, string) bool) (*Decorator, error) {
	if live == nil {
		return nil, errors.New("shadowslack: a live Client is required")
	}
	if ledger == nil {
		return nil, errors.New("shadowslack: a ledger is required -- suppression that cannot be recorded is not suppression")
	}
	if repoFullName == "" {
		return nil, errors.New("shadowslack: a repoFullName is required -- the ledger is read per repository")
	}
	if isLive == nil {
		return nil, errors.New("shadowslack: an egress resolver is required")
	}
	return &Decorator{live: live, ledger: ledger, repoFullName: repoFullName, isLive: isLive}, nil
}

// This assertion is the tripwire, exactly like shadowscm.Decorator's own:
// a method added to Client stops this line compiling until it is handled
// below, explicitly -- never by embedding Client, which would satisfy the
// compiler for a new method and pass it straight to the live
// implementation, unsuppressed.
var _ Client = (*Decorator)(nil)

func (d *Decorator) record(ctx context.Context, e shadowledger.Entry) error {
	e.RepoFullName = d.repoFullName
	return shadowledger.Record(ctx, d.ledger, e)
}

// GetUserEmail is a read and is forwarded unchanged.
func (d *Decorator) GetUserEmail(ctx context.Context, userID string) (slackapi.UserEmail, bool, error) {
	return d.live.GetUserEmail(ctx, userID)
}

// PostAck posts the ack, or records that it would have.
func (d *Decorator) PostAck(ctx context.Context, channel, threadTS, text string) error {
	if d.isLive(ctx, d.repoFullName) {
		return d.live.PostAck(ctx, channel, threadTS, text)
	}
	return d.record(ctx, shadowledger.Entry{
		Operation: "slack_post_ack",
		Target:    channel,
		Spec:      shadowledger.SlackAck{Channel: channel, ThreadTS: threadTS, Text: text},
	})
}

// PostEphemeral posts the ephemeral notice, or records that it would have.
func (d *Decorator) PostEphemeral(ctx context.Context, channel, userID, threadTS, text string) error {
	if d.isLive(ctx, d.repoFullName) {
		return d.live.PostEphemeral(ctx, channel, userID, threadTS, text)
	}
	return d.record(ctx, shadowledger.Entry{
		Operation: "slack_post_ephemeral",
		Target:    channel,
		Spec:      shadowledger.SlackEphemeral{Channel: channel, UserID: userID, ThreadTS: threadTS, Text: text},
	})
}

// PostIdentityLinkNotice posts the identity-link prompt, or records that
// it would have -- WITHOUT its text.
//
// It exists as a separate method from PostEphemeral for one reason: the
// text carries a live magic-link nonce, and PostEphemeral records text
// verbatim into a permanent, append-only ledger. Routing this through
// there would durably store a credential-equivalent secret, which is the
// thing §30.6's record types exist to make impossible.
//
// The exclusion is structural rather than a redaction: SlackIdentityLinkNotice
// has no text field, so there is nowhere for the nonce to go. Stripping a
// URL out of a text field instead would hold only until someone rewords
// the notice.
func (d *Decorator) PostIdentityLinkNotice(ctx context.Context, channel, userID, threadTS, text string) error {
	if d.isLive(ctx, d.repoFullName) {
		return d.live.PostEphemeral(ctx, channel, userID, threadTS, text)
	}
	return d.record(ctx, shadowledger.Entry{
		Operation: "slack_post_identity_link_notice",
		Target:    channel,
		Spec:      shadowledger.SlackIdentityLinkNotice{Channel: channel, UserID: userID, ThreadTS: threadTS},
	})
}

// UpdateMessage updates the message, or records that it would have.
func (d *Decorator) UpdateMessage(ctx context.Context, channel, ts, text string) error {
	if d.isLive(ctx, d.repoFullName) {
		return d.live.UpdateMessage(ctx, channel, ts, text)
	}
	return d.record(ctx, shadowledger.Entry{
		Operation: "slack_update_message",
		Target:    channel + "/" + ts,
		Spec:      shadowledger.SlackMessageUpdate{Channel: channel, Ts: ts, Text: text},
	})
}

// OpenView opens the modal, or records that it would have.
func (d *Decorator) OpenView(ctx context.Context, triggerID, planID, sessionID string) error {
	if d.isLive(ctx, d.repoFullName) {
		return d.live.OpenView(ctx, triggerID, planID, sessionID)
	}
	return d.record(ctx, shadowledger.Entry{
		Operation: "slack_open_view",
		Target:    planID,
		Spec:      shadowledger.SlackViewOpen{TriggerID: triggerID, PlanID: planID, SessionID: sessionID},
	})
}
