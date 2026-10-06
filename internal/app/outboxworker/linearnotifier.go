package outboxworker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// linearNotifier implements ports.Notifier for BOTH
// ports.NotificationKindLinear (a terminal outcome) and, as of an
// audit-fix batch (finding M16, "completeness"),
// ports.NotificationKindLinearProgress (a mid-turn "thought") rows --
// mirroring planSlackNotifier's own identical "one small wrapper type per
// provider-specific notification family, registered under multiple kinds
// in the same map" shape (planslacknotifier.go). See this package's own
// doc.go for why the real Linear API credential lookup
// (LinearInstallationStore.GetByOrganizationID + platform.DecryptToken)
// lives HERE, in app/outboxworker, rather than inside internal/adapters/
// outbound/linearapi itself: a hard security requirement (a decrypted
// token must never sit in the outbox payload at rest, only an
// organization_id reference -- decrypt fresh at delivery time) that also
// happens to keep linearapi free of a postgres-store dependency no other
// outbound adapter package carries.
type linearNotifier struct {
	client             *linearapi.Client
	installations      *postgres.LinearInstallationStore
	tokenEncryptionKey []byte
}

// var _ ports.Notifier = (*linearNotifier)(nil) makes a Notifier signature
// drift a build error, not a runtime surprise.
var _ ports.Notifier = (*linearNotifier)(nil)

// NewLinearNotifier builds a ports.Notifier for BOTH
// ports.NotificationKindLinear and ports.NotificationKindLinearProgress
// rows, backed by client/installations/tokenEncryptionKey -- called once
// by cmd/control-plane/main.go's own kind->Notifier map assembly (and
// registered under both kinds there -- see that map's own construction
// site), mirroring how internal/adapters/outbound/{slackapi,githubapi}'s
// own constructors are each called exactly once there too. The concrete
// *linearNotifier type stays unexported -- callers depend on the
// ports.Notifier interface it returns, never the type itself.
func NewLinearNotifier(client *linearapi.Client, installations *postgres.LinearInstallationStore, tokenEncryptionKey []byte) ports.Notifier {
	return &linearNotifier{
		client:             client,
		installations:      installations,
		tokenEncryptionKey: tokenEncryptionKey,
	}
}

// Deliver implements ports.Notifier, dispatching on notification.Kind --
// mirrors planSlackNotifier's own identical dispatch shape
// (planslacknotifier.go) for "one small wrapper type handling more than
// one Kind under the same provider". The delivery worker only ever routes
// a row to this notifier under one of the three kinds it is registered for
// in cmd/control-plane/main.go's own kind->Notifier map, so an
// unrecognized fourth kind here is defensive, not expected.
//
// ports.NotificationKindLinearWorkflowDecision is §25.9's own addition
// ("workflow HITL gate + circuit breaker", §25.9) -- its payload is a
// plain linearapi.Payload with Success always true (internal/app/
// workflowengine's own enqueueWorkflowNotice, notify.go), the EXACT same
// shape ports.NotificationKindLinear's own deliverOutcome already handles,
// so this reuses that method verbatim rather than duplicating its
// CreateResponseActivity call here. The session guard's notice
// (ports.NotificationKindLinearSessionGuard, technical plan §40.1) carries
// the same payload shape and is delivered the same way.
func (n *linearNotifier) Deliver(ctx context.Context, notification ports.Notification) error {
	switch notification.Kind {
	case ports.NotificationKindLinear:
		return n.deliverOutcome(ctx, notification.Payload)
	case ports.NotificationKindLinearProgress:
		return n.deliverProgress(ctx, notification.Payload)
	case ports.NotificationKindLinearWorkflowDecision, ports.NotificationKindLinearSessionGuard:
		return n.deliverOutcome(ctx, notification.Payload)
	default:
		return fmt.Errorf("outboxworker: linearNotifier: unrecognized notification kind %q", notification.Kind)
	}
}

// resolveAccessToken looks up organizationID's own stored
// linear_installations row and decrypts its access token FRESH (never
// cached, never persisted anywhere outside that one already-encrypted-
// at-rest row) -- the half of Deliver shared identically by both
// deliverOutcome and deliverProgress below, factored out once this
// notifier gained a second Kind to deliver (an audit-fix batch's own
// addition, finding M16). A pgx.ErrNoRows result from the installation
// lookup (no admin has connected this workspace) is a real delivery
// failure like any other -- returned as an error so the caller's own
// backoff/dead-letter machinery handles it identically to a real network
// failure, never silently swallowed.
func (n *linearNotifier) resolveAccessToken(ctx context.Context, organizationID string) (string, error) {
	installation, err := n.installations.GetByOrganizationID(ctx, organizationID)
	if err != nil {
		return "", fmt.Errorf("outboxworker: get linear installation for organization %q: %w", organizationID, err)
	}

	accessToken, err := platform.DecryptToken(n.tokenEncryptionKey, installation.AccessTokenEncrypted)
	if err != nil {
		// The decrypt error itself is safe to log (see platform.
		// DecryptToken's own doc comment: it never contains the ciphertext
		// or plaintext) -- the plaintext token it would have produced is
		// NEVER logged, here or anywhere else.
		return "", fmt.Errorf("outboxworker: decrypt linear access token: %w", err)
	}
	return string(accessToken), nil
}

// deliverOutcome implements the original ports.NotificationKindLinear
// path: decodes n.Payload as linearapi.Payload and posts the
// outcome-shaped AgentActivity Payload.Success selects
// (CreateResponseActivity vs CreateErrorActivity).
func (n *linearNotifier) deliverOutcome(ctx context.Context, raw json.RawMessage) error {
	var payload linearapi.Payload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("outboxworker: decode linear payload: %w", err)
	}

	accessToken, err := n.resolveAccessToken(ctx, payload.OrganizationID)
	if err != nil {
		return err
	}

	if payload.Success {
		return n.client.CreateResponseActivity(ctx, accessToken, payload.AgentSessionID, payload.Text, "")
	}
	return n.client.CreateErrorActivity(ctx, accessToken, payload.AgentSessionID, payload.Text)
}

// deliverProgress implements the new ports.NotificationKindLinearProgress
// path (audit finding M16, "completeness"): decodes n.Payload as
// linearapi.ProgressPayload and posts a "thought"-shaped AgentActivity
// (linearapi.Client.CreateThoughtActivity) -- the already-built call §8.10's
// ingress handler uses synchronously at session creation, now also
// reachable asynchronously, mid-turn, through this same retried outbox
// path, closing the gap that package's own doc.go named as future work.
func (n *linearNotifier) deliverProgress(ctx context.Context, raw json.RawMessage) error {
	var payload linearapi.ProgressPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("outboxworker: decode linear progress payload: %w", err)
	}

	accessToken, err := n.resolveAccessToken(ctx, payload.OrganizationID)
	if err != nil {
		return err
	}

	return n.client.CreateThoughtActivity(ctx, accessToken, payload.AgentSessionID, payload.Text, "")
}
