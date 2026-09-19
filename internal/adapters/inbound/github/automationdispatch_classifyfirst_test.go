package github

// U6/U10 audit fix's own required proof: an event type outside
// domainautomation.GitHubDispatchAllowlist must never reach the identity
// lookup (resolveCommenterActor's own Postgres round trip) or the
// automation list call -- classification needs only the event type,
// already available before either. Package github (whitebox), not
// github_test: dispatchAutomationsBestEffort is unexported.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// countingCommenterIdentityLookup is a CommenterIdentityLookup fake that
// counts its own calls instead of touching a real Postgres connection --
// mirrors internal/app/automation's own countingFailNTimesLister
// precedent (githubdispatch_test.go).
type countingCommenterIdentityLookup struct{ calls int }

func (f *countingCommenterIdentityLookup) GetByProviderAndExternalID(_ context.Context, _ sqlcgen.IdentityProvider, _ string) (sqlcgen.Identity, error) {
	f.calls++
	return sqlcgen.Identity{}, errors.New("countingCommenterIdentityLookup: unexpected call")
}

// countingGitHubTriggerLister/countingDeliveryInvocationCreator are the
// SAME shape, for automation.GitHubTriggerLister/DeliveryInvocationCreator.
type countingGitHubTriggerLister struct{ calls int }

func (f *countingGitHubTriggerLister) ListActiveGitHubAutomations(_ context.Context) ([]sqlcgen.Automation, error) {
	f.calls++
	return nil, errors.New("countingGitHubTriggerLister: unexpected call")
}

func (f *countingGitHubTriggerLister) MarkCreatorUnauthorized(_ context.Context, _ pgtype.UUID) (int64, error) {
	return 0, errors.New("countingGitHubTriggerLister: unexpected call")
}

func (f *countingGitHubTriggerLister) ClearCreatorUnauthorized(_ context.Context, _ pgtype.UUID) (int64, error) {
	return 0, errors.New("countingGitHubTriggerLister: unexpected call")
}

type countingDeliveryInvocationCreator struct{ calls int }

func (f *countingDeliveryInvocationCreator) CreateForDelivery(_ context.Context, _ sqlcgen.CreateAutomationInvocationForDeliveryParams) (sqlcgen.CreateAutomationInvocationForDeliveryRow, error) {
	f.calls++
	return sqlcgen.CreateAutomationInvocationForDeliveryRow{}, errors.New("countingDeliveryInvocationCreator: unexpected CreateForDelivery call")
}

func (f *countingDeliveryInvocationCreator) CountRecentInvocations(_ context.Context, _ pgtype.UUID, _ pgtype.Timestamptz) (int64, error) {
	f.calls++
	return 0, errors.New("countingDeliveryInvocationCreator: unexpected CountRecentInvocations call")
}

// TestDispatchAutomationsBestEffort_UnclassifiedEventTypeSkipsIdentityAndListCalls
// is U6/U10's own required proof: "deployment" is a real GitHub webhook
// event type (deployment created/updated) that domainautomation.
// GitHubDispatchAllowlist does not cover at all -- before this fix, this
// function still resolved the sender's identity (a Postgres round trip)
// for every such delivery before ever reaching ClassifyGitHubDispatch
// (which only ran later, inside automation.DispatchGitHubWebhookEvent).
func TestDispatchAutomationsBestEffort_UnclassifiedEventTypeSkipsIdentityAndListCalls(t *testing.T) {
	identities := &countingCommenterIdentityLookup{}
	lister := &countingGitHubTriggerLister{}
	invocations := &countingDeliveryInvocationCreator{}
	users := narvipg.NewUserStore(nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := Config{Automations: lister, AutomationInvocations: invocations}
	body := []byte(`{"action": "created", "repository": {"full_name": "acme/repo"}, "sender": {"id": 42}}`)

	dispatchAutomationsBestEffort(context.Background(), logger, cfg, identities, users, "deployment", "delivery-unclassified-1", body)

	if identities.calls != 0 {
		t.Errorf("CommenterIdentityLookup.GetByProviderAndExternalID call count = %d, want 0 (classification must run BEFORE identity resolution)", identities.calls)
	}
	if lister.calls != 0 {
		t.Errorf("GitHubTriggerLister.ListActiveGitHubAutomations call count = %d, want 0", lister.calls)
	}
	if invocations.calls != 0 {
		t.Errorf("DeliveryInvocationCreator call count = %d, want 0", invocations.calls)
	}
}

// TestDispatchAutomationsBestEffort_AllowlistedEventTypeStillReachesIdentityLookup
// is the companion positive proof: an ALLOWLISTED event type must still
// reach identity resolution exactly as before -- U6/U10's own fix must
// only skip work for event types that were never actionable, never
// silently disable dispatch for real ones.
func TestDispatchAutomationsBestEffort_AllowlistedEventTypeStillReachesIdentityLookup(t *testing.T) {
	identities := &countingCommenterIdentityLookup{}
	lister := &countingGitHubTriggerLister{}
	invocations := &countingDeliveryInvocationCreator{}
	users := narvipg.NewUserStore(nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := Config{Automations: lister, AutomationInvocations: invocations}
	body := []byte(`{"action": "opened", "repository": {"full_name": "acme/repo"}, "sender": {"id": 42}}`)

	dispatchAutomationsBestEffort(context.Background(), logger, cfg, identities, users, "pull_request", "delivery-allowlisted-1", body)

	if identities.calls != 1 {
		t.Errorf("CommenterIdentityLookup.GetByProviderAndExternalID call count = %d, want 1 (an allowlisted event type's sender must still be resolved)", identities.calls)
	}
}
