// This file (integrations.go) implements §12.5's own ("integrations read
// model & routes" amendment) GET /api/integrations: one row per ingress
// surface (Slack, Linear, GitHub) reporting whether it is configured and
// evidence of its last inbound/outbound delivery. A DERIVED read only --
// nothing here is persisted for its own sake, and there is no
// connect/disconnect write anywhere in this file: a surface is connected
// by deploying its configuration (§27.3's own cloud-identity capability
// already takes this same posture).
//
// The pure mapping/predicate logic (which platform.Config values each
// surface needs, and how an outbox.kind attributes to a provider) lives
// in internal/domain/integrations -- see that package's own doc comment.
// This file is the one place that actually reads platform.Config and
// touches Postgres, composing that domain logic into the wire response.
//
// Gated by the EXISTING authz.ActionManageIntegrations (admin only,
// §13.3 row 6) -- this Step is its first HTTP consumer; its only prior
// consumer (internal/adapters/inbound/linear/authz.go) gates a Linear
// ingress path, not a route. No repo scoping: integrations are a
// deployment-wide concept, not per-repo, so this route has no
// resolveKnownRepo call the way reposettings.go's own repo-scoped
// handlers do.

package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/integrations"
	"github.com/narvidev/narvi/internal/platform"
)

// GetIntegrations backs GET /api/integrations: 403 if the caller fails
// authz.ActionManageIntegrations (admin only); 200 with
// restdtos.ListIntegrationsResponse otherwise -- one row per
// internal/domain/integrations.Providers entry, in that package's own
// fixed order.
func GetIntegrations(cfg *platform.Config, outbox *postgres.OutboxStore, deliveries *postgres.WebhookDeliveryStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionManageIntegrations, authz.Resource{}) {
			return
		}

		rows := make([]restdtos.Integration, 0, len(integrations.Providers))
		for _, p := range integrations.Providers {
			row := restdtos.Integration{
				Surface:    restdtos.IntegrationSurface(p),
				Configured: configuredForProvider(cfg, p),
			}

			// lastInboundAt: webhook_deliveries, an EXACT provider match
			// (postgres.WebhookDeliveryStore.GetLastInboundAt's own doc
			// comment) -- a bare MAX() aggregate, so this never returns
			// pgx.ErrNoRows, only Valid=false for "never heard from this
			// provider".
			lastInboundAt, err := deliveries.GetLastInboundAt(ctx, string(p))
			if err != nil {
				logger.Error("httpapi: get last inbound delivery failed", "error", err, "provider", string(p))
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			row.LastInboundAt = timestamptzPtr(lastInboundAt)

			// lastOutboundAt/lastOutboundStatus/lastOutboundError: outbox,
			// the OTHER direction -- a prefix match on kind
			// (postgres.OutboxStore.GetLatestByKindPrefix's own doc
			// comment), so pgx.ErrNoRows genuinely means "no outbound
			// attempt on record for this provider yet", left as the DTO's
			// own all-nil zero value below rather than an error.
			outboxRow, err := outbox.GetLatestByKindPrefix(ctx, integrations.OutboxKindPrefix(p))
			switch {
			case err == nil:
				row.LastOutboundAt = timestamptzPtr(outboxRow.CreatedAt)
				status := string(outboxRow.Status)
				row.LastOutboundStatus = restdtos.IntegrationLastOutboundStatus(&status)
				// last_error is NOT cleared when a row later succeeds:
				// MarkOutboxDelivered sets status/delivered_at only
				// (queries/outbox.sql). So a row that failed once and
				// succeeded on retry still carries the old message, and
				// reporting it verbatim would render a DELIVERED surface
				// with an error beside it -- the same fact-versus-verdict
				// confusion §12.5 forbids, one field down. On a delivered
				// row the error is history, not state.
				if outboxRow.Status != sqlcgen.OutboxStatusDelivered {
					row.LastOutboundError = restdtos.IntegrationLastOutboundError(outboxRow.LastError)
				}
			case errors.Is(err, pgx.ErrNoRows):
				// No outbound attempt on record for this provider --
				// row.LastOutboundAt/LastOutboundStatus/LastOutboundError
				// already carry their own nil zero value.
			default:
				logger.Error("httpapi: get latest outbox entry by kind prefix failed", "error", err, "provider", string(p))
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}

			rows = append(rows, row)
		}

		writeJSON(w, http.StatusOK, restdtos.ListIntegrationsResponse{Integrations: rows})
	}
}

// configuredForProvider reports whether p reads as "connected": p must be
// EXPLICITLY ENABLED on this deployment (cfg.IngressEnabled, §12.5's own
// ingress-optionality decision) AND have its own full credential set
// present, dispatched to internal/domain/integrations' own per-surface
// predicate (ConfiguredSlack/ConfiguredLinear/ConfiguredGitHub) -- see
// those doc comments for the full "why this exact set, and why not
// GitHubClientID/GitHubClientSecret" reasoning. The default branch is
// unreachable in practice (integrations.Providers is this package's own
// fixed, exhaustive list) but defended against anyway rather than a panic,
// mirroring authz.Authorize's own "should be unreachable" ErrUnknownAction
// fallback.
//
// This is genuinely a two-valued question now, not the "structurally
// always true" field an earlier draft of §12.5 shipped: a surface this
// deployment never enabled reads configured=false, and a running process
// can never observe "enabled but missing a secret" as a THIRD state --
// platform.Load's own fail-fast boot gate refuses to start in that case,
// so the only two states a live response can ever report are "enabled and
// fully configured" and "not enabled here" (the cfg.IngressEnabled check
// short-circuits before the predicate call, but the predicate is still
// checked explicitly rather than assumed, exactly as its own doc comment
// explains, since nothing here should quietly start trusting the boot
// invariant instead of verifying it). A dedicated third wire value for
// that unreachable state would encode a distinction no running deployment
// can ever actually be in.
func configuredForProvider(cfg *platform.Config, p integrations.Provider) bool {
	if !cfg.IngressEnabled[p] {
		return false
	}
	switch p {
	case integrations.ProviderSlack:
		return integrations.ConfiguredSlack(cfg.SlackSigningSecret, cfg.SlackBotToken)
	case integrations.ProviderLinear:
		return integrations.ConfiguredLinear(cfg.LinearWebhookSecret, cfg.LinearOAuthClientID, cfg.LinearOAuthClientSecret)
	case integrations.ProviderGitHub:
		return integrations.ConfiguredGitHub(cfg.GitHubWebhookSecret, cfg.GitHubBotHandle, cfg.GitHubBotToken)
	default:
		return false
	}
}

// timestamptzPtr converts a pgtype.Timestamptz into a *time.Time -- nil
// when !t.Valid (a genuine SQL NULL, or an aggregate over zero rows),
// otherwise a pointer to its own Time value. Shared by both
// pgtype.Timestamptz->restdtos nullable-date-time field conversions
// above (LastInboundAt/LastOutboundAt), rather than repeating the Valid
// check inline at each call site. Both restdtos fields are the plain
// *time.Time Go type (never a named pointer-type wrapper) -- see
// restdtos.Plan's own DecidedAt field doc comment (contracts/gen/go/
// restdtos/restdtos.go) for why go-jsonschema's own default named-wrapper
// codegen for a nullable date-time property silently breaks
// encoding/json's decode path, and why this schema's own
// lastInboundAt/lastOutboundAt descriptions (contracts/rest/v1/
// dtos.schema.json) now call that out explicitly for the next person who
// regenerates this file.
func timestamptzPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	tm := t.Time
	return &tm
}
