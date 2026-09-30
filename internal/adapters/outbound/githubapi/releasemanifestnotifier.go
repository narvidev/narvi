package githubapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// ReleaseManifestPayload is the JSON shape internal/app/outboxworker
// expects to find in an outbox entry's own payload column for a
// ports.NotificationKindReleaseManifest row -- enqueued by
// internal/app/releasereview ("release PR review", §15.2) once
// a detected release PR's manifest check has been computed. Owner/Repo/
// PRNumber are the release PR's own identity; Body is the ALREADY-
// RENDERED comment (internal/domain/reviewpost.RenderManifestComment's
// own result -- this package never renders anything itself, only
// delivers already-rendered text, mirroring VerdictPayload/HandoffPayload's
// own identical discipline).
type ReleaseManifestPayload struct {
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	PRNumber int    `json:"pr_number"`
	Body     string `json:"body"`
}

// ReleaseManifestNotifier implements ports.Notifier for
// ports.NotificationKindReleaseManifest: ONE Deliver call posts the
// manifest check's summary as a plain issue comment -- never a formal
// review (see ports.NotificationKindReleaseManifest's own doc comment
// for why: this check has no RiskLevel/Shippable of its own to gate a
// formal-review event on).
type ReleaseManifestNotifier struct {
	adapter  *Adapter
	outbound *platform.GitHubOutboundConfig
}

var _ ports.Notifier = (*ReleaseManifestNotifier)(nil)

// NewReleaseManifestNotifier builds a ReleaseManifestNotifier wrapping
// adapter, authenticating every call with outbound's bot credential
// (non-nil: refused otherwise, §12.5) -- this check's own
// comment is a system-generated audit, never attributed to any
// individual reviewer or the release PR's own author, mirroring
// NewHandoffNotifier's own identical "single, statically-configured bot
// credential" choice.
func NewReleaseManifestNotifier(adapter *Adapter, outbound *platform.GitHubOutboundConfig) (*ReleaseManifestNotifier, error) {
	if err := platform.RequireGitHubOutbound(outbound, "githubapi: new ReleaseManifestNotifier"); err != nil {
		return nil, err
	}
	return &ReleaseManifestNotifier{adapter: adapter, outbound: outbound}, nil
}

// Deliver implements ports.Notifier: decodes n.Payload as
// ReleaseManifestPayload and posts it as a plain issue comment. n.Kind
// is not checked -- mirrors VerdictNotifier/HandoffNotifier's own
// identical "only ever asked to Deliver its own matching Kind in
// practice" precedent. Not idempotent on retry (PostIssueComment posts a
// SECOND comment on a redelivered attempt) -- an accepted limitation
// shared with every other Notifier in this codebase today (VerdictNotifier/
// HandoffNotifier's own identical doc comments); internal/app/
// releasereview's own caller only ever enqueues this once per detected
// release PR (session creation is itself a one-time event per PR,
// github_pr_sessions' own per-PR coalescing), so a retry is only ever a
// genuine transient-failure redelivery of the SAME still-pending
// attempt, never a routine re-trigger.
func (n *ReleaseManifestNotifier) Deliver(ctx context.Context, notification ports.Notification) error {
	var payload ReleaseManifestPayload
	if err := json.Unmarshal(notification.Payload, &payload); err != nil {
		return fmt.Errorf("githubapi: decode release manifest payload: %w", err)
	}

	if err := n.adapter.PostIssueComment(ctx, payload.Owner, payload.Repo, payload.PRNumber, n.outbound.BotToken(), payload.Body); err != nil {
		return fmt.Errorf("githubapi: deliver release manifest (post comment): %w", err)
	}
	return nil
}
