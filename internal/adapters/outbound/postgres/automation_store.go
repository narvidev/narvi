package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// AutomationStore is a thin, pass-through wrapper around the sqlc-generated
// automations queries ("automations: engine", §3.5). No caching,
// no business rules -- EvaluateFailureStrike/Transition (internal/domain/
// automation) are pure and live there; the CAS-guarded claim-and-record
// loop lives in app/automation.
type AutomationStore struct {
	q *sqlcgen.Queries
}

// NewAutomationStore builds an AutomationStore backed by pool.
func NewAutomationStore(pool *pgxpool.Pool) *AutomationStore {
	return &AutomationStore{q: sqlcgen.New(pool)}
}

// WithTx returns an AutomationStore whose queries run on tx instead of the
// pool this store was built with -- LockForUpdate/ApplyFailureStrike MUST
// be called on a WithTx-scoped store, inside the SAME transaction as
// automation_invocations' own MarkFailureCounted (see this package's own
// AutomationInvocationStore), mirroring every other store's identical
// WithTx convention.
func (s *AutomationStore) WithTx(tx pgx.Tx) *AutomationStore {
	return &AutomationStore{q: s.q.WithTx(tx)}
}

// Create inserts a brand-new automations row -- no HTTP caller exists yet
// in this Step (§8.4/§10 own that surface); used directly by this
// package's own integration tests.
func (s *AutomationStore) Create(ctx context.Context, arg sqlcgen.CreateAutomationParams) (sqlcgen.Automation, error) {
	return s.q.CreateAutomation(ctx, arg)
}

// Get fetches the automations row for id, or pgx.ErrNoRows if none exists.
func (s *AutomationStore) Get(ctx context.Context, id pgtype.UUID) (sqlcgen.Automation, error) {
	return s.q.GetAutomation(ctx, id)
}

// LockForUpdate reads and row-locks the automations row for id -- callers
// MUST be inside an open transaction (WithTx) and MUST subsequently call
// either ApplyFailureStrike or nothing before committing/rolling back; see
// LockAutomationForUpdate's own generated doc comment for the concurrent-
// invocation-closure race this lock closes.
func (s *AutomationStore) LockForUpdate(ctx context.Context, id pgtype.UUID) (sqlcgen.Automation, error) {
	return s.q.LockAutomationForUpdate(ctx, id)
}

// ApplyFailureStrike records automation.EvaluateFailureStrike's own
// verdict -- callers MUST already hold LockForUpdate's own row lock, in
// the SAME transaction.
func (s *AutomationStore) ApplyFailureStrike(ctx context.Context, arg sqlcgen.ApplyFailureStrikeParams) (sqlcgen.Automation, error) {
	return s.q.ApplyFailureStrike(ctx, arg)
}

// ResetConsecutiveFailures resets id's own consecutive-failure streak to
// zero -- idempotent, no CAS guard needed (see ResetConsecutiveFailures's
// own generated doc comment).
func (s *AutomationStore) ResetConsecutiveFailures(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.ResetConsecutiveFailures(ctx, id)
}

// Resume applies automation.TriggerResume: Paused -> Active, resetting the
// consecutive-failure streak. Backs POST /api/automations/{id}/resume
// (§8.4). pgx.ErrNoRows means id is not currently Paused (a
// no-op, not an error the caller should surface as one).
func (s *AutomationStore) Resume(ctx context.Context, id pgtype.UUID) (sqlcgen.Automation, error) {
	return s.q.ResumeAutomation(ctx, id)
}

// Pause applies automation.TriggerAutoPause via a direct admin action
// (POST /api/automations/{id}/pause, §8.4) -- the manual-pause
// twin of Resume above; see PauseAutomation's own generated doc comment
// for why this reuses TriggerAutoPause rather than a dedicated trigger.
// pgx.ErrNoRows means id is not currently Active (a no-op, not an error
// the caller should surface as one).
func (s *AutomationStore) Pause(ctx context.Context, id pgtype.UUID) (sqlcgen.Automation, error) {
	return s.q.PauseAutomation(ctx, id)
}

// GetByWebhookTokenHash looks up the automation whose webhook_token_hash
// matches hash exactly -- backs the webhook trigger's own inbound-auth
// check (internal/adapters/inbound/automationwebhook's own handler.go).
// pgx.ErrNoRows means no automation currently carries this hash -- an
// unrecognized token, OR one that WAS recognized once but has since been
// rotated (RotateWebhookToken below, which overwrites webhook_token_hash
// with a fresh hash) or revoked (RevokeWebhookToken, which clears it to
// NULL entirely) -- either way this lookup can no longer find it, by
// design (rotate/revoke's own review-fix: httpapi.RotateAutomationWebhookToken/
// RevokeAutomationWebhookToken).
func (s *AutomationStore) GetByWebhookTokenHash(ctx context.Context, hash string) (sqlcgen.Automation, error) {
	return s.q.GetAutomationByWebhookTokenHash(ctx, &hash)
}

// RotateWebhookToken overwrites id's own webhook_token_hash with hash --
// backs POST /api/automations/{automationID}/webhook-token (review fix:
// "webhook token has no rotation/revocation/expiry"). Guarded by "AND
// trigger_type = 'webhook'": rotating a token on a non-webhook automation
// (which never had one to begin with) is a no-op, surfaced to the caller
// as pgx.ErrNoRows exactly like every other guarded single-row UPDATE in
// this store (Pause/Resume above) rather than a silent success. The OLD
// hash stops matching GetByWebhookTokenHash the instant this commits --
// there is no grace period, matching every other bearer-token rotation
// precedent in this codebase (ws_tokens, sandboxes.token_hash).
func (s *AutomationStore) RotateWebhookToken(ctx context.Context, id pgtype.UUID, hash string) (sqlcgen.Automation, error) {
	return s.q.RotateAutomationWebhookToken(ctx, sqlcgen.RotateAutomationWebhookTokenParams{ID: id, WebhookTokenHash: &hash})
}

// RevokeWebhookToken clears id's own webhook_token_hash to NULL -- backs
// DELETE /api/automations/{automationID}/webhook-token (review fix, same
// finding as RotateWebhookToken above). Unconditional (no "AND trigger_type
// = 'webhook'" guard, unlike RotateWebhookToken): clearing an
// already-NULL hash on a non-webhook automation is a harmless no-op, not
// an error worth distinguishing -- the caller (httpapi.
// RevokeAutomationWebhookToken) already does its own existence check
// first, exactly like Pause/Resume. Once this commits,
// GetByWebhookTokenHash can never again match this automation until a
// subsequent RotateWebhookToken call mints a new one (automationwebhook's
// own handler.go already 401s on any hash miss -- no handler-side change
// needed for a revoke to take effect).
func (s *AutomationStore) RevokeWebhookToken(ctx context.Context, id pgtype.UUID) (sqlcgen.Automation, error) {
	return s.q.RevokeAutomationWebhookToken(ctx, id)
}

// List returns every automation matching the given optional creator/status
// filters (§8.4's own "creator/status filters") -- backs GET
// /api/automations. A zero-value createdBy (Valid: false) or nil status
// matches every row for that filter.
func (s *AutomationStore) List(ctx context.Context, createdBy pgtype.UUID, status *sqlcgen.AutomationStatus) ([]sqlcgen.Automation, error) {
	return s.q.ListAutomations(ctx, sqlcgen.ListAutomationsParams{CreatedBy: createdBy, Status: status})
}

// ListActiveCronAutomations returns every active, cron-triggered automation
// -- backs the cron trigger pump's own per-tick scan (app/automation's own
// triggerpump.go).
func (s *AutomationStore) ListActiveCronAutomations(ctx context.Context) ([]sqlcgen.Automation, error) {
	return s.q.ListActiveCronAutomations(ctx)
}

// ListActiveGitHubAutomations returns every active, github-triggered
// automation -- backs the live GitHub webhook dispatch path (§8.4,
// app/automation's own githubdispatch.go).
func (s *AutomationStore) ListActiveGitHubAutomations(ctx context.Context) ([]sqlcgen.Automation, error) {
	return s.q.ListActiveGitHubAutomations(ctx)
}

// ListActiveLinearAutomations returns every active, linear-triggered
// automation -- backs the live Linear webhook dispatch path (§8.4,
// app/automation's own lineardispatch.go).
func (s *AutomationStore) ListActiveLinearAutomations(ctx context.Context) ([]sqlcgen.Automation, error) {
	return s.q.ListActiveLinearAutomations(ctx)
}

// ClaimCronFire is the cron trigger pump's own per-automation CAS guard --
// see ClaimCronFire's own generated doc comment. pgx.ErrNoRows means this
// automation already fired for the given minute bucket (a concurrent tick
// or pod won the race first) -- a harmless no-op, not an error.
func (s *AutomationStore) ClaimCronFire(ctx context.Context, id pgtype.UUID, minuteBucket pgtype.Timestamptz) (sqlcgen.Automation, error) {
	return s.q.ClaimCronFire(ctx, sqlcgen.ClaimCronFireParams{ID: id, LastCronFiredAt: minuteBucket})
}

// UpdateLastRun persists §8.4's own "last_run + artifact_summary
// populated" -- called by app/automation's own closeout.go the moment an
// invocation's own outcome is decided.
func (s *AutomationStore) UpdateLastRun(ctx context.Context, arg sqlcgen.UpdateAutomationLastRunParams) (sqlcgen.Automation, error) {
	return s.q.UpdateAutomationLastRun(ctx, arg)
}

// MarkCreatorUnauthorized records that id's own machine-origin dispatch
// (check_run/status -- GitHub -- or a non-"user" actor -- Linear) was just
// denied because created_by is not a linked, non-disabled account holding
// authz.ActionCreateSession -- U8 audit fix, see migrations/
// 000138_automations_creator_unauthorized.up.sql's own doc comment for the
// full "why". Idempotent: a row already marked keeps its own FIRST denial
// timestamp (MarkAutomationCreatorUnauthorized's own generated doc
// comment) -- rows == 0 means "already marked", never an error.
func (s *AutomationStore) MarkCreatorUnauthorized(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.MarkAutomationCreatorUnauthorized(ctx, id)
}

// ClearCreatorUnauthorized is MarkCreatorUnauthorized's own self-healing
// twin -- called the moment id's own machine-origin dispatch is
// authorized again. rows == 0 means "was not marked", never an error.
func (s *AutomationStore) ClearCreatorUnauthorized(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.ClearAutomationCreatorUnauthorized(ctx, id)
}
