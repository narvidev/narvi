// This file (sessionspendcap.go) implements the audited write behind
// technical plan §40.1's spend cap: an administrator sets, raises, lowers
// or clears the session spend cap of a repository or of an automation.
//
//   - PUT /api/repos/{owner}/{repo}/session-spend-cap       (answers RepoSettings)
//   - PUT /api/automations/{automationID}/session-spend-cap (answers Automation)
//
// Both take restdtos.UpdateSessionSpendCapRequest -- {"capUsd": <number or
// null>}, null clearing the cap -- and are gated by one admin-only action,
// authz.ActionSetSessionSpendCap (§13.3): an automation's cap takes
// precedence over its repositories', so the two writes are one decision.
// The repository route is scoped like every repository-scoped route
// (resolveKnownRepo). A cap must be a number above zero with at most two
// decimal places, no larger than NUMERIC(10, 2) holds
// (sessionguard.ValidateSessionSpendCap, the one check every write makes,
// §37) -- a 400 otherwise -- and the columns' CHECK constraints back it
// (migrations/000165).
//
// Each write and its session_spend_cap.set audit row, naming the previous
// cap and the new one, commit together (§13.3's "written in the same
// transaction as the change", on repoentitlement.go's and autonomyfreeze.go's
// precedent): an audit insert that fails leaves the cap as it was. The
// write first locks the row and reads the cap it replaces, so of two writes
// at once the second audits the first one's cap as its previous value.
//
// A raise is what re-admits a session the cap refused (technical plan
// §40.1, internal/domain/sessionguard): the guard reads the cap afresh on
// every turn it decides, so the next turn created on the session -- a
// person's prompt, a plan approval, the automatic re-review's next push --
// is admitted, and its dispatch too. Nothing waits to be woken by the
// raise: a refused turn was either never inserted or ended at dispatch, so
// the write touches nothing but the cap.

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/auditlog"
	appreviewtriage "github.com/narvidev/narvi/internal/app/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/platform"
)

// The audit_log name of a session spend cap write (§40.1), and the two
// resources it names.
const (
	auditActionSessionSpendCapSet = "session_spend_cap.set"
	auditResourceTypeRepo         = "repo"
	auditResourceTypeAutomation   = "automation"
)

// The 400 answers of a session spend cap write's body.
const (
	sessionSpendCapRequiredMessage = "capUsd is required: a number of dollars, or null to clear the cap"
	sessionSpendCapInvalidMessage  = "capUsd must be above zero, with at most two decimal places and no larger than 99999999.99, or null to clear the cap"
)

// decodeSessionSpendCap reads a session spend cap write's body,
// restdtos.UpdateSessionSpendCapRequest, and validates its cap: nil is the
// cap cleared. capUsd is required, so a body that leaves it out clears
// nothing by accident -- decoded by hand rather than through the generated
// type, whose decoder accepts a bare JSON null as a body with every field
// null. Writes a 400 and returns ok=false for a malformed body, a missing
// capUsd, a capUsd that is no number, or a cap
// sessionguard.ValidateSessionSpendCap refuses.
func decodeSessionSpendCap(w http.ResponseWriter, r *http.Request) (capUSD *float64, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil {
		writeError(w, http.StatusBadRequest, "malformed request body")
		return nil, false
	}
	raw, present := body["capUsd"]
	if !present {
		writeError(w, http.StatusBadRequest, sessionSpendCapRequiredMessage)
		return nil, false
	}
	if err := json.Unmarshal(raw, &capUSD); err != nil {
		writeError(w, http.StatusBadRequest, sessionSpendCapInvalidMessage)
		return nil, false
	}
	if err := sessionguard.ValidateSessionSpendCap(capUSD); err != nil {
		writeError(w, http.StatusBadRequest, sessionSpendCapInvalidMessage)
		return nil, false
	}
	return capUSD, true
}

// sessionSpendCapUSD renders a stored session spend cap for the wire and
// for the audit row: nil for SQL NULL, no cap.
func sessionSpendCapUSD(n pgtype.Numeric) *float64 {
	if v, ok := appreviewtriage.NumericToFloat64(n); ok {
		return &v
	}
	return nil
}

// sessionSpendCapAuditDetail is the session_spend_cap.set row's detail: the
// cap before the write and the cap it wrote, each in USD, null for no cap.
func sessionSpendCapAuditDetail(previous, written pgtype.Numeric) map[string]any {
	detail := map[string]any{"previous_usd": nil, "cap_usd": nil}
	if v := sessionSpendCapUSD(previous); v != nil {
		detail["previous_usd"] = *v
	}
	if v := sessionSpendCapUSD(written); v != nil {
		detail["cap_usd"] = *v
	}
	return detail
}

// PutSessionSpendCap backs PUT /api/repos/{owner}/{repo}/session-spend-cap:
// 403 unless the caller passes authz.ActionSetSessionSpendCap (admin only);
// 404 for a repository this deployment does not know; 400 for a body
// decodeSessionSpendCap refuses; otherwise the cap and its
// session_spend_cap.set audit row commit together, and 200 answers the
// repository's restdtos.RepoSettings.
func PutSessionSpendCap(pool *pgxpool.Pool, repoSettings *postgres.RepoSettingsStore, auditLog *postgres.AuditLogStore, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionSetSessionSpendCap, authz.Resource{}) {
			return
		}
		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}
		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}
		capUSD, ok := decodeSessionSpendCap(w, r)
		if !ok {
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: set repo session spend cap: begin tx failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		txSettings := repoSettings.WithTx(tx)
		previous, err := txSettings.LockSessionSpendCap(ctx, repoFullName)
		if err != nil {
			logger.Error("httpapi: set repo session spend cap: lock failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		settings, err := txSettings.UpsertSessionSpendCap(ctx, repoFullName, capUSD)
		if err != nil {
			logger.Error("httpapi: set repo session spend cap: write failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := auditlog.Record(ctx, auditLog.WithTx(tx), actorUserID, auditActionSessionSpendCapSet, auditResourceTypeRepo, repoFullName,
			sessionSpendCapAuditDetail(previous, settings.SessionSpendCapUsd)); err != nil {
			logger.Error("httpapi: set repo session spend cap: record audit log failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: set repo session spend cap: commit failed", "error", err, "repo", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		logger.Info("httpapi: repository session spend cap set", "repo", repoFullName, "actor_user_id", actorUserID.String())
		writeJSON(w, http.StatusOK, repoSettingsFromRow(repoFullName, settings))
	}
}

// PutAutomationSessionSpendCap backs PUT
// /api/automations/{automationID}/session-spend-cap: 403 unless the caller
// passes authz.ActionSetSessionSpendCap (admin only); 400 for a malformed
// automation id or a body decodeSessionSpendCap refuses; 404 when the
// automation does not exist; otherwise the cap and its
// session_spend_cap.set audit row commit together, and 200 answers the
// automation's restdtos.Automation. Its run health is read in the same
// transaction, so a failed read rolls the write back rather than answering
// 500 for a cap that was set.
func PutAutomationSessionSpendCap(pool *pgxpool.Pool, automations *postgres.AutomationStore, runs *postgres.AutomationRunStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionSetSessionSpendCap, authz.Resource{}) {
			return
		}
		id, ok := parseAutomationID(w, r)
		if !ok {
			return
		}
		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}
		capUSD, ok := decodeSessionSpendCap(w, r)
		if !ok {
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: set automation session spend cap: begin tx failed", "error", err, "automation_id", id.String())
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		txAutomations := automations.WithTx(tx)
		existing, err := txAutomations.LockForUpdate(ctx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "automation not found")
				return
			}
			logger.Error("httpapi: set automation session spend cap: lock failed", "error", err, "automation_id", id.String())
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		updated, err := txAutomations.UpdateSessionSpendCap(ctx, id, capUSD)
		if err != nil {
			logger.Error("httpapi: set automation session spend cap: write failed", "error", err, "automation_id", id.String())
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := auditlog.Record(ctx, auditLog.WithTx(tx), actorUserID, auditActionSessionSpendCapSet, auditResourceTypeAutomation, id.String(),
			sessionSpendCapAuditDetail(existing.SessionSpendCapUsd, updated.SessionSpendCapUsd)); err != nil {
			logger.Error("httpapi: set automation session spend cap: record audit log failed", "error", err, "automation_id", id.String())
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		health, err := lookupRunHealth(ctx, runs.WithTx(tx), id)
		if err != nil {
			logger.Error("httpapi: set automation session spend cap: lookup run health failed", "error", err, "automation_id", id.String())
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: set automation session spend cap: commit failed", "error", err, "automation_id", id.String())
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		logger.Info("httpapi: automation session spend cap set", "automation_id", id.String(), "actor_user_id", actorUserID.String())
		writeJSON(w, http.StatusOK, automationToDTO(updated, health))
	}
}
