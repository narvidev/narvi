// This file (freeze.go) classifies every ports.NotificationKind by what the
// autonomy freeze (technical plan §40.2) does to its delivery: most kinds
// deliver whatever the freeze, and two are automatic actions the freeze
// holds.
//
// §40.2 is explicit that a freeze does not pause the outbox: "notifications
// about work already done still deliver, because a freeze that also
// silences the audit trail is a freeze nobody can verify". But two kinds
// are not reports of work already done. They are the work: the sentinel
// auto-fix's delivery creates a branch and spawns a fix session (§17.2),
// and the description autofix's rewrites a pull request's description with
// agent-authored text (§26.2) -- each an action that starts without a
// person asking for it right then. Builder.attempt holds those two while
// frozen and delivers every other kind as before.
//
// The table is exhaustive by the same discipline as classification.go's
// egress table: NewBuilder refuses a registered kind with no entry here,
// and classificationsource_test.go checks the table against every kind
// ports/notifier.go declares, so a new automatic kind must say whether the
// freeze holds it before it can ship.

package outboxworker

import (
	"fmt"

	"github.com/narvidev/narvi/internal/app/ports"
	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
)

// FreezeClass is what the autonomy freeze does to one kind's delivery.
type FreezeClass int

const (
	// FreezeDelivers marks a kind delivered whatever the freeze: a report of
	// work already done, or the system's own hygiene.
	FreezeDelivers FreezeClass = iota
	// FreezeHolds marks a kind whose delivery is itself an automatic action
	// (§40.2): while autonomy is frozen it is not delivered, its attempt is
	// given back, and it is due again AutonomyFreezeRecheckInterval later.
	FreezeHolds
)

// freezeEntry is one kind's freeze classification, and for a held kind the
// site its skips are counted under.
type freezeEntry struct {
	class FreezeClass
	site  domainautonomy.Site
}

// notificationKindFreeze classifies all 20 ports.NotificationKind
// constants. Two hold; every other kind delivers, each a report of work
// already done or hygiene:
//   - the Slack, Linear and GitHub messages, plan approvals and decisions,
//     progress, workflow decisions and digests report what a session or a
//     person did, or ask a person to decide;
//   - github_verdict and github_review_check publish a review that already
//     ran; handoff_sentinel and release_manifest comment on a pull request
//     a person or a review already acted on;
//   - rwx_preview_dispatch and github_preview_link build and link a
//     preview of a change already pushed;
//   - blob_delete is storage hygiene, and linear_digest always returns its
//     typed error.
var notificationKindFreeze = map[ports.NotificationKind]freezeEntry{
	ports.NotificationKindSlack:                  {class: FreezeDelivers},
	ports.NotificationKindLinear:                 {class: FreezeDelivers},
	ports.NotificationKindGitHub:                 {class: FreezeDelivers},
	ports.NotificationKindSlackPlanApproval:      {class: FreezeDelivers},
	ports.NotificationKindSlackPlanDecided:       {class: FreezeDelivers},
	ports.NotificationKindLinearProgress:         {class: FreezeDelivers},
	ports.NotificationKindGitHubVerdict:          {class: FreezeDelivers},
	ports.NotificationKindHandoffSentinel:        {class: FreezeDelivers},
	ports.NotificationKindReleaseManifest:        {class: FreezeDelivers},
	ports.NotificationKindSlackWorkflowDecision:  {class: FreezeDelivers},
	ports.NotificationKindLinearWorkflowDecision: {class: FreezeDelivers},
	ports.NotificationKindGitHubWorkflowDecision: {class: FreezeDelivers},
	ports.NotificationKindRWXPreviewDispatch:     {class: FreezeDelivers},
	ports.NotificationKindGitHubPreviewLink:      {class: FreezeDelivers},
	ports.NotificationKindBlobDelete:             {class: FreezeDelivers},
	ports.NotificationKindSlackDigest:            {class: FreezeDelivers},
	ports.NotificationKindLinearDigest:           {class: FreezeDelivers},
	ports.NotificationKindGitHubReviewCheck:      {class: FreezeDelivers},

	// The sentinel auto-fix's branch and child session (§17.2).
	ports.NotificationKindSentinelAutoFix: {class: FreezeHolds, site: domainautonomy.SiteSentinelAutoFixSpawn},
	// The unattended rewrite of a pull request's description (§26.2).
	ports.NotificationKindGitHubDescriptionAutofix: {class: FreezeHolds, site: domainautonomy.SiteDescriptionAutofix},
}

// freezeOf returns kind's freeze classification. A kind the table does not
// carry delivers: NewBuilder refuses a registered one, so this is only
// reached for a row whose kind has no notifier, which never delivers.
func freezeOf(kind ports.NotificationKind) freezeEntry {
	return notificationKindFreeze[kind]
}

// classifyFreeze refuses a notifiers map carrying a kind the table above
// does not classify, naming every one -- the same seam, and the same
// reason, as classifyNotifiers (classification.go): NewBuilder receives the
// finished map, so no later insert escapes the check.
func classifyFreeze(notifiers map[ports.NotificationKind]ports.Notifier) error {
	var missing []ports.NotificationKind
	for kind := range notifiers {
		if _, ok := notificationKindFreeze[kind]; !ok {
			missing = append(missing, kind)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("outboxworker: refusing to start: %d notification kind(s) have no autonomy freeze classification: %v", len(missing), missing)
}
