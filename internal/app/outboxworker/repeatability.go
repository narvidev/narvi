// This file (repeatability.go) classifies every ports.NotificationKind by
// what a second delivery of the same row does after the remote end already
// accepted the first -- the input technical plan §5.1's shutdown rule needs
// before it may give a cut-short delivery its attempt back.
//
// A delivery this process's shutdown cuts short may have been cut before the
// remote end saw it, or after it accepted the write and before its answer
// arrived; the worker cannot tell which. Counting the attempt, as the outbox
// always has, bounds how many times such a write can be repeated at
// domain/outbox.MaxAttempts. Giving the attempt back loosens that bound, so
// it is safe only for a kind whose repeat leaves what one delivery leaves.
// For any other kind the interruption counts, as before.

package outboxworker

import (
	"fmt"

	"github.com/narvidev/narvi/internal/app/ports"
)

// Repeatability is whether one ports.NotificationKind's delivery may run
// again after the remote end accepted it without adding to its effect.
type Repeatability int

const (
	// NotRepeatable means a second delivery adds a second effect: a second
	// message, comment, review, activity or build. The zero value, so a
	// kind is repeatable only by saying so below.
	NotRepeatable Repeatability = iota
	// Repeatable means a second delivery leaves what the first left.
	Repeatable
)

// notificationKindRepeatability classifies every kind ports/notifier.go
// declares, decided from what each notifier's Deliver actually does today
// (classificationsource_test.go checks it against the declaring file, and
// NewBuilder refuses a registered kind missing from it). A kind not safe to
// repeat regains the shutdown rule only once its write carries a receipt
// that finds the earlier delivery before repeating it -- for the formal
// review and the GitHub comments, the create-once receipt of technical plan
// §21.1b; the Slack and Linear posts have none planned.
var notificationKindRepeatability = map[ports.NotificationKind]Repeatability{
	// chat.postMessage (slackapi.Client.Deliver, digestSlackNotifier, and
	// planSlackNotifier's approval post): Slack takes no idempotency key,
	// so a repeat posts a second message. planSlackNotifier's own doc
	// comment names the duplicate post as an accepted cost of retrying.
	ports.NotificationKindSlack:                 NotRepeatable,
	ports.NotificationKindSlackPlanApproval:     NotRepeatable,
	ports.NotificationKindSlackWorkflowDecision: NotRepeatable,
	ports.NotificationKindSlackSessionGuard:     NotRepeatable,
	ports.NotificationKindSlackDigest:           NotRepeatable,
	// chat.update on the message whose channel and ts the payload already
	// carries: a repeat rewrites the same text onto the same message.
	ports.NotificationKindSlackPlanDecided: Repeatable,

	// agentActivityCreate (linearNotifier): the mutation is sent without an
	// id, so each call creates an activity; a repeat adds a second
	// response, error or thought to the agent session.
	ports.NotificationKindLinear:                 NotRepeatable,
	ports.NotificationKindLinearProgress:         NotRepeatable,
	ports.NotificationKindLinearWorkflowDecision: NotRepeatable,
	ports.NotificationKindLinearSessionGuard:     NotRepeatable,
	// digestLinearNotifier always returns its typed error and writes
	// nothing anywhere.
	ports.NotificationKindLinearDigest: Repeatable,

	// githubapi.BotNotifier's comments (PostIssueComment): a repeat posts a
	// second comment.
	ports.NotificationKindGitHub:                 NotRepeatable,
	ports.NotificationKindGitHubWorkflowDecision: NotRepeatable,
	ports.NotificationKindGitHubSessionGuard:     NotRepeatable,
	// The formal review (githubapi.VerdictNotifier, CreateReview) and its
	// label sync: a repeat submits a second review (§21.1b).
	ports.NotificationKindGitHubVerdict: NotRepeatable,
	// HandoffNotifier and ReleaseManifestNotifier end in PostIssueComment:
	// a repeat posts a second comment (HandoffNotifier's labels are
	// idempotent, its comment is not).
	ports.NotificationKindHandoffSentinel: NotRepeatable,
	ports.NotificationKindReleaseManifest: NotRepeatable,
	// A commit status for the same (context, sha): GitHub shows the latest
	// per context, so a repeat converges on the same link.
	ports.NotificationKindGitHubPreviewLink: Repeatable,
	// descriptionAutofixNotifier re-reads the body and
	// reviewpost.RenderAutofixBody re-extracts the original it already
	// preserved, so a repeat writes the same body (that function's own
	// "IDEMPOTENT" note and its double-render test).
	ports.NotificationKindGitHubDescriptionAutofix: Repeatable,
	// reviewCheckNotifier finds the run it created -- by head sha, check
	// name, this deployment's own recorded writer App and the pull
	// request's external id -- and updates it before it would create one,
	// so a repeat updates the same run, provided the cut create has landed
	// by then: which is why an interrupted delivery is due again only after
	// platform.Timeouts.OutboxInterruptedSettleDelay, never at once. The
	// one gap is a deployment's very first create, before any writer App
	// has been recorded: a repeat of that one delivery can leave a second
	// run, and it can today as well, since the cut attempt is retried
	// either way; later repeats adopt.
	ports.NotificationKindGitHubReviewCheck: Repeatable,

	// sentinelAutoFixNotifier claims its sentinel_fixes row, creates the
	// branch (CreateBranch treats "already exists" as success) and spawns
	// the child session in one transaction; a repeat finds the claim and
	// only finishes marking the findings.
	ports.NotificationKindSentinelAutoFix: Repeatable,

	// RWX's dispatch starts one build per call; rwx.PreviewNotifier's own
	// doc comment says it is not idempotent on redelivery.
	ports.NotificationKindRWXPreviewDispatch: NotRepeatable,

	// BlobStore.Delete succeeds on a key already gone.
	ports.NotificationKindBlobDelete: Repeatable,
}

// repeatabilityOf returns kind's classification, NotRepeatable for a kind
// the table does not carry (NewBuilder refuses a registered one, so this is
// only reached for a row whose kind has no notifier, which never delivers).
func repeatabilityOf(kind ports.NotificationKind) Repeatability {
	return notificationKindRepeatability[kind]
}

// checkRepeatability refuses a notifiers map carrying a kind the table above
// does not classify, naming every one -- the same seam, and the same reason,
// as classifyNotifiers (classification.go): NewBuilder receives the finished
// map, so no later insert escapes the check.
func checkRepeatability(notifiers map[ports.NotificationKind]ports.Notifier) error {
	var missing []ports.NotificationKind
	for kind := range notifiers {
		if _, ok := notificationKindRepeatability[kind]; !ok {
			missing = append(missing, kind)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("outboxworker: refusing to start: %d notification kind(s) have no repeatability classification: %v", len(missing), missing)
}
