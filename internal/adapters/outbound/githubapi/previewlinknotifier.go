package githubapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// previewCommitStatusContext is the fixed GitHub commit-status "context"
// value every preview link uses (§4.1.2 point 3: "context: narvi/preview").
const previewCommitStatusContext = "narvi/preview"

// PreviewLinkPayload is the JSON shape internal/app/outboxworker expects
// to find in an outbox entry's own payload column for a
// ports.NotificationKindGitHubPreviewLink row — enqueued by
// internal/app/sessionactor's own createPRBestEffort (pushpr.go, §4.1.2
// point 1) alongside the companion rwx.PreviewDispatchPayload row, in the
// SAME transaction. TargetURL is the deterministic "friendly" preview URL
// (rwx.FriendlyPreviewURL), already rendered at enqueue time — this
// package never renders it itself, mirroring ReleaseManifestPayload's own
// "deliver already-rendered content" discipline.
type PreviewLinkPayload struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	SHA   string `json:"sha"`
	// TargetURL is the rendered friendly preview URL.
	TargetURL string `json:"target_url"`
	// Description carries the ephemerality caveat (§4.1.2 point 3: "live
	// while RWX serves it") — rendered once, at enqueue time, by the
	// enqueuer, never by this notifier.
	Description string `json:"description"`
}

// PreviewLinkNotifier implements ports.Notifier for
// ports.NotificationKindGitHubPreviewLink: ONE Deliver call posts a
// `narvi/preview` commit status via the NEW Adapter.CreateCommitStatus
// capability (§4.1.2 point 3) — never an issue comment, and never a
// GitHub Deployment (that section's own "a preview that dies with RWX's
// own idle reaper should not masquerade as a deployment environment"
// reasoning).
type PreviewLinkNotifier struct {
	adapter  *Adapter
	outbound *platform.GitHubOutboundConfig
}

// var _ ports.Notifier = (*PreviewLinkNotifier)(nil) makes a Notifier
// signature drift a build error, not a runtime surprise.
var _ ports.Notifier = (*PreviewLinkNotifier)(nil)

// NewPreviewLinkNotifier builds a PreviewLinkNotifier wrapping adapter,
// authenticating every call with outbound's bot credential (non-nil:
// refused otherwise, §12.5) — a preview link is a
// system-generated fact about a commit, never attributed to any
// individual PR author or reviewer, mirroring NewReleaseManifestNotifier's
// own identical "single, statically-configured bot credential" choice.
func NewPreviewLinkNotifier(adapter *Adapter, outbound *platform.GitHubOutboundConfig) (*PreviewLinkNotifier, error) {
	if err := platform.RequireGitHubOutbound(outbound, "githubapi: new PreviewLinkNotifier"); err != nil {
		return nil, err
	}
	return &PreviewLinkNotifier{adapter: adapter, outbound: outbound}, nil
}

// Deliver implements ports.Notifier: decodes n.Payload as
// PreviewLinkPayload and posts it as a `narvi/preview` commit status with
// state "success" (§4.1.2 point 3) — state is always "success" here: a
// preview LINK existing is itself the only fact this status reports
// (whether RWX's own build behind it is still healthy is not tracked by
// this Step, named as unverified in §4.1.3's own "first-click-during-
// first-build" item). n.Kind is not checked — mirrors every other
// Notifier implementation in this package (e.g. ReleaseManifestNotifier).
func (n *PreviewLinkNotifier) Deliver(ctx context.Context, notification ports.Notification) error {
	var payload PreviewLinkPayload
	if err := json.Unmarshal(notification.Payload, &payload); err != nil {
		return fmt.Errorf("githubapi: decode preview link payload: %w", err)
	}

	if err := n.adapter.CreateCommitStatus(ctx, payload.Owner, payload.Repo, payload.SHA,
		"success", payload.TargetURL, payload.Description, previewCommitStatusContext, n.outbound.BotToken()); err != nil {
		return fmt.Errorf("githubapi: deliver preview link (create commit status): %w", err)
	}
	return nil
}
