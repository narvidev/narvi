package githubapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// Payload is the JSON shape internal/app/outboxworker expects to find in
// an outbox entry's own payload column for a ports.NotificationKindGitHub
// row -- enqueued by internal/app/sessionactor at turn-completion time
// (owner/repo/PR number from the session's own reverse-looked-up
// github_pr_sessions row, split out of its own repo_full_name at enqueue
// time so this package never needs its own "owner/repo" string-splitting
// logic) with a short, human-readable outcome message.
type Payload struct {
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	PRNumber int    `json:"pr_number"`
	Text     string `json:"text"`
}

// BotNotifier implements ports.Notifier by calling Adapter.PostIssueComment
// with a single, statically-configured bot credential baked in at
// construction time (platform.Config.GitHubOutbound, §12.5's GitHub
// outbound axis) -- see this
// package's own doc.go for why this is a separate, sibling type from
// Adapter itself rather than Adapter implementing ports.Notifier directly.
type BotNotifier struct {
	adapter  *Adapter
	outbound *platform.GitHubOutboundConfig
}

// var _ ports.Notifier = (*BotNotifier)(nil) makes a Notifier signature
// drift a build error, not a runtime surprise.
var _ ports.Notifier = (*BotNotifier)(nil)

// NewBotNotifier builds a BotNotifier wrapping adapter, authenticating
// every PostIssueComment call it makes with outbound's bot credential.
// outbound must be non-nil: posting as the bot is the whole of this
// notifier, so it refuses to exist with GitHub outbound off (§12.5).
func NewBotNotifier(adapter *Adapter, outbound *platform.GitHubOutboundConfig) (*BotNotifier, error) {
	if err := platform.RequireGitHubOutbound(outbound, "githubapi: new BotNotifier"); err != nil {
		return nil, err
	}
	return &BotNotifier{adapter: adapter, outbound: outbound}, nil
}

// Deliver implements ports.Notifier: decodes n.Payload as Payload and
// posts it as a real issue comment via Adapter.PostIssueComment,
// authenticated with this BotNotifier's own bot token. n.Kind is not
// checked -- this BotNotifier is only ever asked to Deliver
// ports.NotificationKindGitHub rows in practice (the delivery worker's own
// kind->Notifier routing is what guarantees that; see ports.Notifier's own
// doc comment).
func (n *BotNotifier) Deliver(ctx context.Context, notification ports.Notification) error {
	var payload Payload
	if err := json.Unmarshal(notification.Payload, &payload); err != nil {
		return fmt.Errorf("githubapi: decode payload: %w", err)
	}

	return n.adapter.PostIssueComment(ctx, payload.Owner, payload.Repo, payload.PRNumber, n.outbound.BotToken(), payload.Text)
}
