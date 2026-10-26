package actorauthz

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/domain/authz"
)

// AuthorizeSessionRead is the session read check (technical plan §6.2,
// §13.3): whether userID may read sessionID right now. The ws-token mint
// (httpapi's MintWSToken) makes it before it mints, and the client socket
// (wshub's NewClientHandler) makes it at subscribe and again every
// platform.Timeouts.ClientWSAccessRecheckInterval while the socket is open.
// It is the one place a session-scoped read rule goes, so the mint and the
// socket can never disagree.
//
//	userID not set (a token whose user was deleted)  Denied
//	no users row for userID                          Denied
//	any other users read error                       Error
//	user disabled                                    Denied
//	role not allowed authz.ActionViewSessions        Denied
//	no sessions row for sessionID                    Denied
//	any other sessions read error                    Error
//	otherwise                                        Allowed
//
// It gives the verdict the REST session read gives (auth.Authenticate's
// user predicate, then the matrix's view row), for every user state and
// role; httpapi's TestSessionReadGate_AgreesWithRESTRead pins that.
// Unlike AuthorizeResolvedActorVerdict, a missing user row is Denied, not
// Error: here the id comes from a credential, and a user deleted since it
// was minted is a real "no", not a lookup that failed.
//
// surface prefixes the log lines, as in AuthorizeResolvedActor.
func AuthorizeSessionRead(ctx context.Context, logger *slog.Logger, surface string, users *postgres.UserStore, sessions *postgres.SessionStore, userID, sessionID pgtype.UUID) LinkedActorVerdict {
	if !userID.Valid {
		logger.Warn(surface + ": session read: the credential names no user, denying")
		return LinkedActorDenied
	}

	user, err := users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn(surface+": session read: user not found, denying", "user_id", userID.String())
			return LinkedActorDenied
		}
		logger.Error(surface+": session read: look up user failed", "error", err, "user_id", userID.String())
		return LinkedActorError
	}
	if v := verdictForUser(logger, surface, user, authz.ActionViewSessions, authz.Resource{}); v != LinkedActorAllowed {
		return v
	}

	if _, err := sessions.Get(ctx, sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn(surface+": session read: session not found, denying", "user_id", userID.String())
			return LinkedActorDenied
		}
		logger.Error(surface+": session read: look up session failed", "error", err, "user_id", userID.String())
		return LinkedActorError
	}
	return LinkedActorAllowed
}
