// Package automationwebhook implements §8.4's ("automations: triggers &
// extras", §8.4) inbound webhook-facing API surface: the ONE piece of
// item 1's own "This needs webhook-facing API surface (an inbound
// endpoint automations can be triggered from)" requirement that cannot
// live in internal/adapters/inbound/httpapi (this package's own doc
// comment on that constraint, below).
//
// A single route: POST /webhooks/automations/{automationID}, bearer-token
// authenticated (Authorization: Bearer <token> -- the SAME convention
// internal/adapters/inbound/httpapi's own scmcredentials.go/snapshotmint.go
// already establish for a non-cookie, non-human caller) against
// automations.webhook_token_hash (migrations/
// 000055_automations_triggers_and_extras.up.sql, platform.HashToken --
// the SAME SHA-256, unsalted convention ws_tokens.token_hash already
// uses). A correctly authenticated call creates one new automation_
// invocations row (internal/app/automation.CreateInvocation) targeting a
// fresh snapshot of the automation's own current repos -- this endpoint's
// own "condition" IS successful authentication; there is no further
// per-request filter to evaluate (unlike the GitHub/Linear trigger types,
// whose own condition -- event/action/label/name/conclusion or
// event/action/team -- IS modeled, validated, AND live-dispatched,
// internal/domain/automation's own trigger.go/dispatch.go plus
// internal/app/automation's own githubdispatch.go/lineardispatch.go; see
// internal/domain/automation/doc.go for the full design).
//
// # Why this is its own package, not internal/adapters/inbound/httpapi
//
// internal/app/automation (fanout.go) already imports httpapi, for
// httpapi.CreateSessionOnTx -- so httpapi importing internal/app/automation
// BACK, to reach CreateInvocation for this endpoint, would be a Go import
// cycle. Every other webhook-facing inbound surface in this codebase
// already lives in its OWN adapter package for an unrelated but
// compatible reason (internal/adapters/inbound/github, linear, slack are
// each self-contained, registered directly on cmd/control-plane/main.go's
// own chi router alongside, not inside, httpapi) -- this package follows
// that SAME established shape, for a reason that happens to also be a
// hard compiler constraint here.
package automationwebhook

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/automation"
	"github.com/narvidev/narvi/internal/platform"
)

// bearerTokenFromHeader extracts the bearer token from r's Authorization
// header -- mirrors internal/adapters/inbound/wshub/sandbox.go's own
// bearerToken/internal/adapters/inbound/httpapi's own identical
// bearerTokenFromHeader (scmcredentials.go), each already its own small,
// duplicated, dependency-free copy rather than one shared/exported
// helper -- this package's own copy follows that SAME established
// precedent.
func bearerTokenFromHeader(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(h, prefix)
	if token == "" {
		return "", false
	}
	return token, true
}

// NewHandler builds the POST /webhooks/automations/{automationID} handler.
// Outcomes:
//
//  1. Malformed {automationID} path param -> 400.
//  2. Authorization: Bearer <token> missing/malformed -> 401.
//  3. Token does not hash-match any automation's own webhook_token_hash,
//     OR the matched automation's own id does not equal the path's
//     {automationID} (never distinguished in the response -- both read as
//     "unauthorized", never leaking which one it was) -> 401.
//  4. Matched automation's own trigger_type is not 'webhook' -> 401 (this
//     token mechanism only ever exists for a webhook-triggered
//     automation; a non-webhook automation has no webhook_token_hash to
//     ever match in the first place, so this is a defensive, expected-
//     unreachable check, not a live code path).
//  5. Matched automation's own status is 'paused' -> 409 (rejected
//     outright here, rather than silently creating a pending invocation
//     that ListDueForFanOut's own "AND a.status = 'active'" guard would
//     leave stuck forever -- an honest, immediate signal beats a silent
//     no-op).
//  6. Otherwise: 202 Accepted, having durably created one new
//     automation_invocations row (automation.CreateInvocation) targeting
//     a fresh snapshot of the automation's own current repos -- the fan-
//     out engine (internal/app/automation.Engine, already running)
//     claims and fans it out on its own next tick, exactly like every
//     other invocation this codebase creates.
func NewHandler(automations *postgres.AutomationStore, invocations *postgres.AutomationInvocationStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		rawID := chi.URLParam(r, "automationID")
		var automationID pgtype.UUID
		if err := automationID.Scan(rawID); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		token, ok := bearerTokenFromHeader(r)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		row, err := automations.GetByWebhookTokenHash(ctx, platform.HashToken(token))
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				logger.Error("automationwebhook: get automation by webhook token hash failed", "error", err)
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if row.ID != automationID || row.TriggerType != sqlcgen.AutomationTriggerTypeWebhook {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if row.Status != sqlcgen.AutomationStatusActive {
			w.WriteHeader(http.StatusConflict)
			return
		}

		targets, err := automation.UnmarshalTargets(row.Repos)
		if err != nil {
			logger.Error("automationwebhook: decode automation repos failed", "error", err, "automation_id", row.ID.String())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if _, err := automation.CreateInvocation(ctx, invocations, row.ID, targets); err != nil {
			logger.Error("automationwebhook: create invocation failed", "error", err, "automation_id", row.ID.String())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusAccepted)
	}
}
