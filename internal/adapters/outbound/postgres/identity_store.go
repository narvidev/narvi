package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// IdentityStore is a thin, pass-through wrapper around the sqlc-generated
// identities queries (§13.2 identity graph, migrations/000003_identities.
// up.sql + migrations/000017_auth_v1.up.sql's own access_token_encrypted
// column). No caching, no retries, no business rules -- the OAuth
// returning-vs-first-time-sign-in branch and the encrypt-before-store step
// are internal/adapters/inbound/auth's job (§13.1).
type IdentityStore struct {
	q *sqlcgen.Queries
}

// NewIdentityStore builds an IdentityStore backed by pool.
func NewIdentityStore(pool *pgxpool.Pool) *IdentityStore {
	return &IdentityStore{q: sqlcgen.New(pool)}
}

// WithTx returns an IdentityStore whose queries run on tx instead of the
// pool this store was built with -- see UserStore.WithTx's own doc comment;
// both are used together, inside the SAME transaction, by the OAuth
// callback handler's first-time-sign-in path.
func (s *IdentityStore) WithTx(tx pgx.Tx) *IdentityStore {
	return &IdentityStore{q: s.q.WithTx(tx)}
}

// Create inserts a new identities row linking a user to a provider
// identity, and returns it.
func (s *IdentityStore) Create(ctx context.Context, arg sqlcgen.CreateIdentityParams) (sqlcgen.Identity, error) {
	return s.q.CreateIdentity(ctx, arg)
}

// GetByProviderAndExternalID fetches an identities row by its (provider,
// external_id) unique key -- the OAuth callback's own returning-vs-
// first-time-sign-in check (a pgx.ErrNoRows result means "first time").
func (s *IdentityStore) GetByProviderAndExternalID(ctx context.Context, provider sqlcgen.IdentityProvider, externalID string) (sqlcgen.Identity, error) {
	return s.q.GetIdentityByProviderAndExternalID(ctx, sqlcgen.GetIdentityByProviderAndExternalIDParams{
		Provider:   provider,
		ExternalID: externalID,
	})
}

// GetByUserAndProvider fetches a user's identity for one provider (§9.3
// "e2e happy path"'s own scm-credentials endpoint uses this to find a
// session's created_by user's GitHub identity).
func (s *IdentityStore) GetByUserAndProvider(ctx context.Context, userID pgtype.UUID, provider sqlcgen.IdentityProvider) (sqlcgen.Identity, error) {
	return s.q.GetIdentityByUserAndProvider(ctx, sqlcgen.GetIdentityByUserAndProviderParams{
		UserID:   userID,
		Provider: provider,
	})
}

// GetByIDForUpdate fetches one identities row by id and locks it for the
// rest of the transaction this store runs on (WithTx) -- the read
// internal/app/identitylink.RefreshSignInEvidence makes before it replaces
// the row's email. pgx.ErrNoRows when the row is gone.
func (s *IdentityStore) GetByIDForUpdate(ctx context.Context, id pgtype.UUID) (sqlcgen.Identity, error) {
	return s.q.GetIdentityByIDForUpdate(ctx, id)
}

// RefreshSignInEmail records, on an already-linked identity, the verified
// email a GitHub or OIDC sign-in just presented for it, stamped now
// (§13.2), and the sign-in's fresh provider access token when it has one:
// a nil accessTokenEncrypted keeps the stored token. Never changes the
// row's user_id or linked_via. Called only by internal/app/identitylink.
// RefreshSignInEvidence, inside its transaction.
func (s *IdentityStore) RefreshSignInEmail(ctx context.Context, id pgtype.UUID, email string, accessTokenEncrypted []byte) (sqlcgen.Identity, error) {
	return s.q.RefreshSignInIdentityEmail(ctx, sqlcgen.RefreshSignInIdentityEmailParams{
		ID:                   id,
		Email:                email,
		AccessTokenEncrypted: accessTokenEncrypted,
	})
}

// ListVerifiedUserIDsByEmail is the auto-link algorithm's match step
// (§13.2 step 2): the users whose identity carries email, verified, and
// confirmed by its provider within maxAge of the database clock (see
// queries/identities.sql's own doc comment). Deduplicated by user_id.
// maxAge is platform.Timeouts.IdentityEmailEvidenceMaxAge; the caller
// refuses a non-positive one before it gets here.
func (s *IdentityStore) ListVerifiedUserIDsByEmail(ctx context.Context, email string, maxAge time.Duration) ([]pgtype.UUID, error) {
	return s.q.ListVerifiedIdentityUserIDsByEmail(ctx, sqlcgen.ListVerifiedIdentityUserIDsByEmailParams{
		Email:  email,
		MaxAge: pgtype.Interval{Microseconds: maxAge.Microseconds(), Valid: true},
	})
}

// ListForUser returns every identity linked to userID, oldest-first --
// backs the members API's own "linked identities" listing per member
// ("identities + full RBAC", §13.2/§13.3).
func (s *IdentityStore) ListForUser(ctx context.Context, userID pgtype.UUID) ([]sqlcgen.Identity, error) {
	return s.q.ListIdentitiesForUser(ctx, userID)
}

// Delete removes one identities row by id -- backs the admin manual-unlink
// endpoint (§13.2 point 5). Returns the number of rows actually deleted
// (0 if id didn't exist) so the caller can distinguish "already gone" from
// a genuine failure; callers must independently verify the row's own
// UserID matches whatever the caller expected BEFORE calling Delete (see
// queries/identities.sql's own doc comment -- this alone does not scope
// by user).
func (s *IdentityStore) Delete(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.DeleteIdentity(ctx, id)
}

// ConfirmAutoLinkEmail records that the re-check of an automatic link found
// its email verified under §13.2's definition for its provider, as of now
// (internal/app/identitylink.Resolve). Touches only a linked_via auto_email
// row; returns the number of rows updated (0 if id is gone or is not an
// automatic link).
func (s *IdentityStore) ConfirmAutoLinkEmail(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.ConfirmAutoLinkedIdentityEmail(ctx, id)
}

// DeleteUncheckedAutoLink removes an automatic link whose re-check failed
// (internal/app/identitylink.Resolve) -- only while it is still an
// automatic link nothing has confirmed, so never a link a person or an
// admin made, nor one a concurrent re-check has just confirmed. Returns the
// number of rows deleted (0 when there was nothing of that kind left).
func (s *IdentityStore) DeleteUncheckedAutoLink(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.DeleteUncheckedAutoLinkedIdentity(ctx, id)
}

// PromoteUncheckedAutoLinkToAdmin turns id, an automatic link nothing has
// confirmed (the rows DeleteUncheckedAutoLink deletes), into an admin link
// with no verified email -- an admin's force-link of the same identity to
// the same user (internal/adapters/inbound/httpapi's LinkMemberIdentity).
// promoted is false, with a nil error, when id is not such a row.
func (s *IdentityStore) PromoteUncheckedAutoLinkToAdmin(ctx context.Context, id pgtype.UUID) (identity sqlcgen.Identity, promoted bool, err error) {
	identity, err = s.q.PromoteUncheckedAutoLinkToAdmin(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.Identity{}, false, nil
	}
	if err != nil {
		return sqlcgen.Identity{}, false, err
	}
	return identity, true, nil
}
