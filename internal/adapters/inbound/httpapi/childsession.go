// This file (childsession.go) implements §8.2's own ("sentinels +
// suggestions", §17.2) "child session" mechanism -- the plan text at
// §14.4/§17.2 describes this as an "existing mechanism", but it is not:
// this Step is the first one that actually builds it (see this Step's own
// PR description for the "what's already there vs. what the plan assumes"
// writeup, and migrations/000045_sessions_child_sessions.up.sql's own doc
// comment).
//
// SpawnChildSession is EXPORTED specifically so a package that can import
// httpapi -- but that httpapi itself must never import back, avoiding a
// cycle -- can call it, mirroring the EXACT precedent internal/adapters/
// inbound/github's own coalesce.go already establishes for
// CreateSessionOnTx/CreateTurnForBot ("already callable from outside
// httpapi by design"). internal/app/outboxworker's own sentinel-auto-fix
// notifier (sentinelautofix.go) is the reason this exists, but -- since
// the Finding-1 audit fix -- is no longer this function's caller: that
// notifier needs to compose the session insert with its OWN atomic
// per-claim lock, on ITS OWN already-open transaction, so it calls
// CreateSessionOnTx/TriggerDispatch directly instead (see
// sentinelautofix.go's own top-of-file doc comment for the full "why").
// SpawnChildSession itself is unchanged, still exported, and remains the
// right entry point for any FUTURE child-session caller that does NOT
// already hold an open transaction of its own to compose with.

package httpapi

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// SpawnChildSession is CreateSessionCore's own child-session-aware sibling
// -- validates req exactly the same way, but threads parentSessionID/
// spawnDepth/provenanceTag through to CreateSessionOnTx via
// ChildSessionOptions (create.go), rather than leaving them at their
// zero-value defaults the way CreateSessionCore always does. Every other
// step (single transaction, TriggerDispatch once committed and a prompt
// was supplied) is identical to CreateSessionCore -- this is NOT a
// parallel reimplementation of session creation, just CreateSessionCore's
// own sequencing with one additional argument threaded through.
//
// provenanceTag is REQUIRED (never empty) -- a child session, by
// definition, always carries SOME provenance tag identifying why it
// exists (today, always provenance.SentinelAutoFix); a caller with no
// provenance tag to set should call CreateSessionCore instead, not this
// function with an empty string.
//
// epistemicCheckDefault (F6, adversarial review) mirrors
// CreateSessionOnTx's own identical required parameter -- see that
// function's own doc comment. A sentinel-auto-fix child session is an
// ordinary build session (it edits test/doc files to fix a finding),
// never a review session, so no F7-style hardcoded-false carve-out
// applies to it -- the real, operator-configured
// platform.Config.EpistemicCheckDefault is always the right value to pass
// (whether through this function, or -- as internal/app/outboxworker's
// own sentinelAutoFixNotifier now does, since the Finding-1 audit fix --
// through the identical CreateSessionOnTx parameter directly).
// rolloutMode/repoSettings (§32) and prSessions (§31.4) mirror
// CreateSessionOnTx's own identical required parameters -- see that
// function's own doc comment. This function has no real production caller today (childsession.go's
// own top doc comment), but stays parameter-complete/consistent
// regardless, exactly like every other CreateSessionOnTx-adjacent entry
// point in this package.
func SpawnChildSession(
	ctx context.Context,
	pool *pgxpool.Pool,
	sessions *postgres.SessionStore,
	turns *postgres.TurnStore,
	environments *postgres.EnvironmentStore,
	auditLog *postgres.AuditLogStore,
	registry *sessionactor.Registry,
	req restdtos.CreateSessionRequest,
	parentSessionID pgtype.UUID,
	spawnDepth int32,
	provenanceTag string,
	epistemicCheckDefault bool,
	rolloutMode platform.RolloutMode,
	repoSettings *postgres.RepoSettingsStore,
	prSessions *postgres.GitHubPRSessionStore,
) (sqlcgen.Session, *CreateSessionError) {
	logger := platform.Logger(ctx)

	if _, verr := validateCreateSessionRequest(req); verr != nil {
		return sqlcgen.Session{}, verr
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		logger.Error("httpapi: begin spawn-child-session tx failed", "error", err)
		return sqlcgen.Session{}, &CreateSessionError{Status: http.StatusInternalServerError, Message: "internal error"}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag := provenanceTag
	created, hasPrompt, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, pgtype.UUID{}, epistemicCheckDefault, rolloutMode, repoSettings, prSessions, ChildSessionOptions{
		ParentSessionID: parentSessionID,
		SpawnDepth:      spawnDepth,
		ProvenanceTag:   &tag,
	})
	if cerr != nil {
		return sqlcgen.Session{}, cerr
	}

	if err := tx.Commit(ctx); err != nil {
		logger.Error("httpapi: commit spawn-child-session tx failed", "error", err)
		return sqlcgen.Session{}, &CreateSessionError{Status: http.StatusInternalServerError, Message: "internal error"}
	}

	if hasPrompt {
		TriggerDispatch(ctx, registry, created.ID)
	}

	return created, nil
}
