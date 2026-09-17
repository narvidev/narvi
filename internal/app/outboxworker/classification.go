// This file (classification.go) implements §30.2's own outbox seam:
// "Classification therefore happens inside NewBuilder, which receives
// the finished map and refuses to start if any registered kind lacks an
// explicit External/Internal classification."
//
// The obvious wrap point -- the map at its main.go population site -- is
// wrong, and the reason is a standing trap: that map is mutated AFTER
// wiring (rwx_preview_dispatch/github_preview_link inserted conditionally,
// blob_delete inserted conditionally) -- a check placed at the wiring
// line would exempt every later insert. NewBuilder receives the map only
// after every one of those conditional mutations has already happened
// (cmd/control-plane/main.go constructs outboxNotifiers, then may add
// three more entries, THEN calls NewBuilder), so checking it here, and
// only here, is what makes insertion order stop mattering: a 20th kind
// registered anywhere in that map, at any point in main.go, cannot ship
// unclassified.

package outboxworker

import (
	"fmt"

	"github.com/narvidev/narvi/internal/app/ports"
)

// EgressClass is §30.2's own External/Internal classification for one
// ports.NotificationKind -- the answer to "does this kind's own Deliver
// call reach a customer-visible surface" that decides whether
// Builder.attempt must honor §30.8's suppress-wins check before ever
// calling notifier.Deliver.
type EgressClass int

const (
	// ClassSuppress marks a kind whose Deliver call is customer-visible
	// egress -- Builder.attempt must suppress it (deliver into the
	// ledger instead of the world) whenever the row's own effective
	// egress mode is shadow. The default for every kind not explicitly
	// named otherwise below (§30.2: "SUPPRESS for every customer-
	// destined kind").
	ClassSuppress EgressClass = iota
	// ClassPassThrough marks a kind that must always run, unconditionally,
	// regardless of egress mode. §30.2 names exactly three, by name, and
	// requires each for a different reason -- see the map below.
	ClassPassThrough
)

// notificationKindClassification is §30.2's own exhaustive External/
// Internal classification of all 20 ports.NotificationKind constants (see
// internal/app/ports/notifier.go -- every const in that file's own block
// has an entry here). NewBuilder below checks every kind actually
// registered in its own notifiers map against this table and refuses to
// start on a miss; a SEPARATE table-driven test
// (classification_test.go) checks the reverse direction -- that this
// table itself covers every constant ports/notifier.go declares, not
// merely whatever main.go happens to wire today -- so a kind added to
// the port but never wired into ANY notifiers map at all is still caught
// before it could ship silently unclassified.
//
// PASS-THROUGH is mandatory for exactly three kinds, each for its own
// reason (§30.2):
//   - blob_delete: Narvi-internal storage hygiene. Suppressing it leaks
//     orphaned blobs forever -- the trap in any blanket
//     suppress-everything reading of shadow mode.
//   - sentinel_auto_fix: the ONE hybrid kind. Its own Deliver performs
//     internal work that must run (the child-session spawn,
//     sentinelautofix.go) while its external writes go through the
//     ALREADY shadow-aware decorated port (shadowscm.Decorator) --
//     suppressing the outbox row here would also skip that internal
//     work, which has nothing to do with customer-visible egress.
//   - linear_digest: a deliberate dead-letter, unchanged by this Step --
//     its own notifier (digestlinearnotifier.go) always returns a typed
//     error today (no organization-level Linear post capability exists
//     yet), so it needs to keep reaching that SAME retry-then-dead-letter
//     path regardless of egress mode.
//
// github_description_autofix is explicitly NOT a hybrid, and is
// SUPPRESS, not PASS-THROUGH: its own Deliver performs zero internal
// state mutation of its own (every precondition it checks is a read);
// its only effect is the external UpdatePRBody call, already covered by
// the §30.2 layer 1 decorator/layer 0 transport gate. Named here so a
// future reader does not "fix" it into PASS-THROUGH by analogy with
// sentinel_auto_fix.
var notificationKindClassification = map[ports.NotificationKind]EgressClass{
	ports.NotificationKindSlack:                    ClassSuppress,
	ports.NotificationKindLinear:                   ClassSuppress,
	ports.NotificationKindGitHub:                   ClassSuppress,
	ports.NotificationKindSlackPlanApproval:        ClassSuppress,
	ports.NotificationKindSlackPlanDecided:         ClassSuppress,
	ports.NotificationKindLinearProgress:           ClassSuppress,
	ports.NotificationKindGitHubVerdict:            ClassSuppress,
	ports.NotificationKindSentinelAutoFix:          ClassPassThrough,
	ports.NotificationKindHandoffSentinel:          ClassSuppress,
	ports.NotificationKindReleaseManifest:          ClassSuppress,
	ports.NotificationKindSlackWorkflowDecision:    ClassSuppress,
	ports.NotificationKindLinearWorkflowDecision:   ClassSuppress,
	ports.NotificationKindGitHubWorkflowDecision:   ClassSuppress,
	ports.NotificationKindRWXPreviewDispatch:       ClassSuppress,
	ports.NotificationKindGitHubPreviewLink:        ClassSuppress,
	ports.NotificationKindBlobDelete:               ClassPassThrough,
	ports.NotificationKindSlackDigest:              ClassSuppress,
	ports.NotificationKindGitHubDescriptionAutofix: ClassSuppress,
	ports.NotificationKindLinearDigest:             ClassPassThrough,
	// NotificationKindGitHubReviewCheck (the review's own GitHub-native
	// result surface, §8.2/§21.1/§21.1b): its own Deliver performs zero
	// internal state mutation independent of the external write -- the
	// claim-table row it updates exists ONLY to make that external write
	// correct (identity/supersession), mirroring
	// github_description_autofix's own identical "not a hybrid" reasoning
	// immediately above. SUPPRESS, never PASS-THROUGH: a shadow-mode
	// repository must never have a real check run created or updated on
	// its customer-visible pull request.
	ports.NotificationKindGitHubReviewCheck: ClassSuppress,
}

// classifyNotifiers checks every kind actually registered in notifiers
// against notificationKindClassification, returning an error naming
// every miss (never just the first) if any registered kind has no
// explicit classification. Called from NewBuilder, which receives the
// FINISHED map -- see this file's own top comment for why checking
// anywhere earlier would miss main.go's own later conditional inserts.
func classifyNotifiers(notifiers map[ports.NotificationKind]ports.Notifier) error {
	var unclassified []ports.NotificationKind
	for kind := range notifiers {
		if _, ok := notificationKindClassification[kind]; !ok {
			unclassified = append(unclassified, kind)
		}
	}
	if len(unclassified) == 0 {
		return nil
	}
	return fmt.Errorf("outboxworker: refusing to start: %d notification kind(s) have no egress classification: %v", len(unclassified), unclassified)
}
