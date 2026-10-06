// Unit tests for classification.go's own §30.2 exhaustiveness discipline
// -- no Postgres needed: classifyNotifiers is pure, and NewBuilder's own
// classification check runs before it ever touches store/pool, so nil
// values are safe to pass here.
package outboxworker_test

import (
	"context"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// noopNotifier is a minimal ports.Notifier for this file's own tests --
// deliberately NOT the fuller fakeNotifier (builder_integration_test.go),
// which lives behind the "integration" build tag: these tests exercise
// pure, in-memory classification logic and must run under plain `go
// test`, no Postgres/testcontainers required.
type noopNotifier struct{}

func (noopNotifier) Deliver(context.Context, ports.Notification) error { return nil }

// TestNewBuilder_RefusesToStart_OnUnclassifiedKind is this Step's own
// literal demonstration of §30.2's "refuses to start if any registered
// kind lacks an explicit External/Internal classification": a brand-new
// NotificationKind, wired into the SAME shape main.go's own
// outboxNotifiers map takes, must fail NewBuilder's construction --
// never boot successfully and silently deliver it unsuppressed the
// first time a row of that kind is enqueued in a shadow deployment.
func TestNewBuilder_RefusesToStart_OnUnclassifiedKind(t *testing.T) {
	// A kind that does not exist anywhere in internal/app/ports/notifier.go
	// today, standing in for a 20th kind added later and wired into
	// main.go's outboxNotifiers map without ALSO being added to
	// notificationKindClassification (classification.go).
	const unclassifiedKind ports.NotificationKind = "totally_new_kind_never_classified"

	notifiers := map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack: noopNotifier{}, // a real, already-classified kind alongside it
		unclassifiedKind:            noopNotifier{},
	}

	_, err := outboxworker.NewBuilder(nil, nil, notifiers, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err == nil {
		t.Fatal("NewBuilder returned a nil error for a notifiers map containing an unclassified kind, want a refusal")
	}
	if !strings.Contains(err.Error(), string(unclassifiedKind)) {
		t.Errorf("NewBuilder error = %q, want it to name the unclassified kind %q", err.Error(), unclassifiedKind)
	}
	t.Logf("NewBuilder correctly refused to start: %v", err)
}

// TestNewBuilder_StartsCleanly_WhenEveryRegisteredKindIsClassified is the
// control case for the test above: the SAME two-kind shape, but with
// only real, already-classified kinds, must construct successfully --
// proving the refusal above is specifically about the unclassified kind,
// not some unrelated construction failure.
func TestNewBuilder_StartsCleanly_WhenEveryRegisteredKindIsClassified(t *testing.T) {
	notifiers := map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack:      noopNotifier{},
		ports.NotificationKindBlobDelete: noopNotifier{},
	}
	// NewBuilder's own metric construction runs after the classification
	// check and needs no real store/pool to succeed (otel's global
	// MeterProvider is a package-level no-op default unless TestMain --
	// this package's own sharedpool_integration_test.go -- installs a
	// real one; either way Int64Gauge/Int64Counter construction cannot
	// fail on a valid name/description).
	if _, err := outboxworker.NewBuilder(nil, nil, notifiers, platform.DefaultTimeouts(), &platform.ShutdownState{}); err != nil {
		t.Fatalf("NewBuilder: %v, want success (every registered kind is classified)", err)
	}
}

// allKnownNotificationKinds is a DELIBERATELY hand-maintained, pinned
// list of every ports.NotificationKind constant (internal/app/ports/
// notifier.go) as of this Step -- Go has no way to enumerate a named
// string type's own declared constants at compile time or via
// reflection.
//
// This list does NOT catch a kind added to notifier.go, and an earlier
// version of this comment claimed it did. It cannot: a new constant added
// there alone leaves both this list and the classification map untouched,
// so the two still agree and this test stays green. Two hand-maintained
// copies of the same fact cannot check each other -- verified by adding a
// twentieth kind and watching this test pass.
//
// TestClassification_CoversEveryKindDeclaredInSource
// (classificationsource_test.go) is the guard for that, by reading the
// constants out of the declaring file. What this list is still good for
// is being read: it puts every kind and its classification side by side
// in one place, which the parse-based check cannot do.
var allKnownNotificationKinds = []ports.NotificationKind{
	ports.NotificationKindSlack,
	ports.NotificationKindLinear,
	ports.NotificationKindGitHub,
	ports.NotificationKindSlackPlanApproval,
	ports.NotificationKindSlackPlanDecided,
	ports.NotificationKindLinearProgress,
	ports.NotificationKindGitHubVerdict,
	ports.NotificationKindSentinelAutoFix,
	ports.NotificationKindHandoffSentinel,
	ports.NotificationKindReleaseManifest,
	ports.NotificationKindSlackWorkflowDecision,
	ports.NotificationKindLinearWorkflowDecision,
	ports.NotificationKindGitHubWorkflowDecision,
	ports.NotificationKindSlackSessionGuard,
	ports.NotificationKindLinearSessionGuard,
	ports.NotificationKindGitHubSessionGuard,
	ports.NotificationKindRWXPreviewDispatch,
	ports.NotificationKindGitHubPreviewLink,
	ports.NotificationKindBlobDelete,
	ports.NotificationKindSlackDigest,
	ports.NotificationKindGitHubDescriptionAutofix,
	ports.NotificationKindLinearDigest,
}

// TestNotificationKindClassification_CoversEveryKnownKind proves
// notificationKindClassification is exhaustive over allKnownNotificationKinds
// above -- registering all 22 in one notifiers map and constructing a
// Builder must succeed. If a new kind is added to ports/notifier.go and
// to allKnownNotificationKinds above but NOT to classification.go's own
// map, this test fails with that kind's own name.
func TestNotificationKindClassification_CoversEveryKnownKind(t *testing.T) {
	if got, want := len(allKnownNotificationKinds), 22; got != want {
		t.Fatalf("len(allKnownNotificationKinds) = %d, want %d -- this Step's own verified count of ports.NotificationKind constants; update both this list and classification.go's own map together if that count ever changes", got, want)
	}

	notifiers := make(map[ports.NotificationKind]ports.Notifier, len(allKnownNotificationKinds))
	for _, kind := range allKnownNotificationKinds {
		notifiers[kind] = noopNotifier{}
	}

	if _, err := outboxworker.NewBuilder(nil, nil, notifiers, platform.DefaultTimeouts(), &platform.ShutdownState{}); err != nil {
		t.Fatalf("NewBuilder: %v, want success (every one of allKnownNotificationKinds must have a classification.go entry)", err)
	}
}

// TestNewBuilder_RefusesToStart_WithoutShutdownState pins that a Builder
// cannot be built blind to this process's shutdown (technical plan §5.1):
// with no state to read, every delivery a deploy cuts short would be
// counted as a failed attempt, silently.
func TestNewBuilder_RefusesToStart_WithoutShutdownState(t *testing.T) {
	notifiers := map[ports.NotificationKind]ports.Notifier{ports.NotificationKindSlack: noopNotifier{}}
	_, err := outboxworker.NewBuilder(nil, nil, notifiers, platform.DefaultTimeouts(), nil)
	if err == nil || !strings.Contains(err.Error(), "shutdown state") {
		t.Fatalf("NewBuilder with no shutdown state = %v, want a refusal naming it", err)
	}
}
