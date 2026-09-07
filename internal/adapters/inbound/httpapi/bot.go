package httpapi

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/intentclassifier"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// CreateSessionForBot and CreateTurnForBot (below) are the two small,
// EXPORTED entry points create.go's own §5.1 doc comment anticipated:
// "a future webhook ingress handler (§8.2/§8.10) calls createSessionCore
// directly with its own already-decoded request and a NULL createdBy --
// never [CreateSession]." That anticipated caller living in the SAME
// package (since createSessionCore stays unexported). §8.2 ("GitHub
// ingress") instead places its handler in its own package,
// internal/adapters/inbound/github, mirroring
// internal/adapters/inbound/httpapi/doc.go's own alternative it left
// open ("or §8.2/§8.10 decide createSessionCore should be exported
// instead") -- a full export of createSessionCore was judged the wrong
// shape (it would hand a webhook adapter direct access to every REST-only
// concern: repo/pathScope/mockConfig validation error *shapes*, HTTP
// status codes, etc.), so instead these two thin wrappers translate to
// and from plain Go values/errors a non-HTTP caller actually wants.
//
// CreateSessionForBot forwards to CreateSessionCore with an explicit NULL
// creator (pgtype.UUID{}) -- every bot/automation-created session has no
// direct human creator, exactly CreateSessionCore's own doc comment and
// createcore_integration_test.go's own TestCreateSessionCore_NilCreator_*
// tests already establish and cover.
//
// epistemicCheckDefault (F6, adversarial review) mirrors
// CreateSessionCore's own identical required parameter -- see that
// function's own doc comment. This function has no real production caller
// today (coalesce.go's own doc comment explains why GitHub's own ingress
// deliberately calls CreateSessionOnTx directly instead, for connection-
// pool safety) -- kept parameter-complete/consistent regardless, exactly
// like every other createTurnLocked-adjacent entry point in this package.
func CreateSessionForBot(ctx context.Context, pool *pgxpool.Pool, sessions *postgres.SessionStore, turns *postgres.TurnStore, environments *postgres.EnvironmentStore, auditLog *postgres.AuditLogStore, registry *sessionactor.Registry, req restdtos.CreateSessionRequest, epistemicCheckDefault bool, rolloutMode platform.RolloutMode, repoSettings *postgres.RepoSettingsStore, prSessions *postgres.GitHubPRSessionStore) (sqlcgen.Session, error) {
	created, cerr := CreateSessionCore(ctx, pool, sessions, turns, environments, auditLog, registry, req, pgtype.UUID{}, epistemicCheckDefault, rolloutMode, repoSettings, prSessions)
	if cerr != nil {
		return sqlcgen.Session{}, cerr
	}
	return created, nil
}

// CreateTurnForBot enqueues a new Pending turn on an EXISTING session for
// a non-browser ingress caller (§8.2/§8.10) living in its own
// package. Reuses createTurnLocked (turn.go) -- the SAME shared core
// CreateTurnCore itself calls -- with its own fixed AlwaysQueue policy,
// so the lock-then-insert-then-dispatch sequencing (a session-row FOR
// UPDATE lock via GetActorEpochForUpdate -- so a concurrent CreateTurn
// REST call and a concurrent bot-ingress turn enqueue on the SAME session
// still serialize against each other correctly -- then insert, audit,
// commit, GetOrSpawn+Send(EnsureDispatched{})) is no longer a
// hand-duplicated copy of CreateTurn's own logic.
//
// Deliberately calls createTurnLocked directly, NOT CreateTurnCore:
// CreateTurnCore's own pre-transaction sessions.Get existence check is a
// REST-only nicety this function has never had (an unchanged, deliberate
// difference -- an unknown sessionID here still ends up as a 404-shaped
// CreateTurnError from inside createTurnLocked's own locked
// GetActorEpochForUpdate call, just without that one extra round trip
// first).
//
// Deliberately uses AlwaysQueue, NOT CreateTurn's own RejectIfOpen: that
// policy is a REST-endpoint-specific choice against a human queuing more
// than one relaunch at a time (CreateTurnPolicy's own doc comment, turn.go)
// -- it is not a general domain invariant. domain/turn.NextToDispatch
// already supports an arbitrary backlog of Pending turns on one session,
// dispatching the oldest one only once nothing is Dispatched/Processing --
// exactly the backlog §8.2's own per-PR @mention coalescing is meant to
// produce when N concurrent mentions land on a PR that already has a
// review session (internal/adapters/inbound/github/coalesce.go is this
// function's own caller for that case).
//
// actorUserID/auditLog are audit-fix batch additions (closing H7's own
// finding that a GitHub-bot-created turn was invisible in the audit
// trail): this now writes the SAME turn.create audit_log row every other
// createTurnLocked caller gets, inside this SAME transaction. actorUserID
// mirrors CreateTurnCore's own identical convention -- a real, resolved
// commenter's user_id (github/coalesce.go's own actor, when linked) or an
// explicit invalid pgtype.UUID{} for a still bot-attributed commenter;
// carries no authorization meaning here, exactly like every other
// createTurnLocked caller.
//
// plans (a follow-up fix, §8.1) is threaded through to
// createTurnLocked's own awaiting-plan gate exactly like every other
// caller -- see that function's own doc comment (turn.go) for the nil-safe
// "skips the gate" contract this shares with them.
//
// intentSvc (§23.1/§23.2) is threaded through exactly like plans
// immediately above -- github/coalesce.go's own REUSE-path caller passes
// the SAME real *intentclassifier.Service every other createTurnLocked
// caller does, so a GitHub-bot mention reply arriving while a plan is
// awaiting_approval gets the SAME real amend-vs-answer classification any
// other ingress channel's ordinary reply now does (see createTurnLocked's
// own doc comment, turn.go).
//
// epistemicCheckDefault (§20.4) is threaded through to
// createTurnLocked exactly like planMode immediately before it -- a
// REQUIRED parameter, not one left at a zero-value default, so a
// GitHub-bot-created build turn honors the SAME platform-wide
// epistemic-check default a REST-created one does (see CreateTurnCore's
// own doc comment, turn.go, for why this is required rather than bundled
// into a variadic options slot).
//
// reviewHeadSHA is non-nil ONLY
// for github/coalesce.go's own REUSE-path caller (an @mention or label
// re-trigger enqueuing a new turn on an ALREADY-EXISTING review session)
// -- the commit SHA THIS turn's own pre-fetched review diff was anchored
// to, threaded through to createTurnLocked's own CreateTurnOptions and
// stored on THIS turn's own row (turns.review_head_sha) at creation, per
// that field's own doc comment (turn.go). A REQUIRED parameter (not
// bundled into a variadic options slot the way CreateTurnCore's own
// StorageConfigured/Effort are) since this function's ONE real caller
// always has a real value (or an honest nil) to supply -- there is no
// "every other caller safely ignores this" population the way those two
// REST-only fields have.
//
// classifyText (F1, §23 follow-up fix, review Finding 1) mirrors
// reviewHeadSHA's own "this function's one real caller always has a real
// value to supply" shape -- github/coalesce.go's REUSE-path caller always
// has its own already-captured, raw, un-enriched mention text in scope
// (that function's own classifyText parameter, the SAME raw text its
// WINNER-path ClassifyAndRecord call already uses) to pass through here.
// Threaded into createTurnLocked's own CreateTurnOptions.ClassifyText --
// see that field's own doc comment (turn.go) for the full "why": prompt
// itself, by the time it reaches this function, already carries
// review.RenderTurnPrompt's own folded-in diff/stack/verdict-tool text,
// which must never reach the plan_followup classifier.
// effort/reviewDepth/reviewDepthDecision (§26.3) mirror
// reviewHeadSHA's own identical "non-nil ONLY for github/coalesce.go's
// own REUSE-path caller" shape, one field further -- see
// CreateTurnOptions.Effort/ReviewDepth/ReviewDepthDecision's own doc
// comments (turn.go).
func CreateTurnForBot(ctx context.Context, pool *pgxpool.Pool, sessions *postgres.SessionStore, turns *postgres.TurnStore, plans *postgres.PlanStore, intentSvc *intentclassifier.Service, auditLog *postgres.AuditLogStore, registry *sessionactor.Registry, sessionID pgtype.UUID, prompt string, modelID *string, planMode bool, epistemicCheckDefault bool, actorUserID pgtype.UUID, reviewHeadSHA *string, classifyText *string, effort *string, reviewDepth *string, reviewDepthDecision []byte) (sqlcgen.Turn, error) {
	created, _, cerr := createTurnLocked(ctx, pool, sessions, turns, plans, intentSvc, auditLog, registry, sessionID, prompt, modelID, planMode, epistemicCheckDefault, actorUserID, AlwaysQueue, CreateTurnOptions{ReviewHeadSHA: reviewHeadSHA, ClassifyText: classifyText, Effort: effort, ReviewDepth: reviewDepth, ReviewDepthDecision: reviewDepthDecision})
	if cerr != nil {
		// %w, NOT %s (a follow-up fix, Finding 1): cerr's own
		// Error() method returns exactly cerr.Message, so this produces the
		// IDENTICAL string as the old fmt.Errorf("...: %s", cerr.Message) --
		// but %w additionally preserves the error CHAIN, so a caller
		// (github/coalesce.go's own REUSE path, which wraps this error again
		// with its own %w) can still recover *CreateTurnError/
		// ErrPlanAwaitingApproval via errors.Is/errors.As through this
		// wrapper. Before this fix, %s discarded cerr entirely, so
		// errors.Is(err, httpapi.ErrPlanAwaitingApproval) could never
		// succeed for any caller of this function -- see
		// ErrPlanAwaitingApproval's own doc comment (turn.go) for the full
		// GitHub-specific consequence this closes.
		return sqlcgen.Turn{}, fmt.Errorf("httpapi: create turn for bot: %w", cerr)
	}
	return created, nil
}
