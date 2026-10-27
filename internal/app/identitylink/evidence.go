package identitylink

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
)

// Audit actions RefreshSignInEvidence records, each in the refresh's own
// transaction (§13.3).
const (
	// auditActionIdentityEmailUpdated: a sign-in presented an email the
	// identity did not hold as verified before -- none stored, an
	// unverified one, or a different address.
	auditActionIdentityEmailUpdated = "identity.email_updated"
	// auditActionUserPrimaryEmailChanged: the user's primary email
	// followed the sign-in identity it came from to its new email.
	auditActionUserPrimaryEmailChanged = "user.primary_email_changed"
)

// Values of identity_email_evidence_refreshes_total's "result" attribute.
const (
	evidenceResultConfirmed = "confirmed"
	evidenceResultUpdated   = "updated"
)

// evidenceRefreshes counts every committed RefreshSignInEvidence, by
// provider and by whether the sign-in confirmed the email the identity
// already held as verified (result "confirmed", a change of case included)
// or brought a new one (result "updated"). A package-level var for the
// same reason as emailFetchFailures (retry.go): RefreshSignInEvidence is a
// free function called off a plain Deps value.
var evidenceRefreshes = newEvidenceRefreshesCounter()

func newEvidenceRefreshesCounter() metric.Int64Counter {
	counter, err := otel.Meter(meterName).Int64Counter(
		"identity_email_evidence_refreshes_total",
		metric.WithDescription("Count of GitHub and OIDC sign-ins that recorded the email their provider presented for the signed-in identity (§13.2's email evidence). Labeled by provider, and by result: confirmed (the identity already held this email as verified) or updated (no email, an unverified one, or a different address before)."),
		metric.WithUnit("{sign_in}"),
	)
	if err != nil {
		// Unreachable for a well-formed constant name; see
		// newEmailFetchFailuresCounter for why this panics at load time.
		panic(fmt.Sprintf("identitylink: construct identity_email_evidence_refreshes_total counter: %v", err))
	}
	return counter
}

// RefreshSignInEvidence records what a GitHub or OIDC sign-in's provider
// has just presented for the identity it authenticated (§13.2): the
// verified email, stamped now, so it counts as evidence for matching
// (MatchUserIDs) for IdentityEmailEvidenceMaxAge from this sign-in, plus
// the sign-in's fresh provider access token when it has one (GitHub; nil
// keeps the stored token, as for OIDC, which stores none).
//
// Everything happens in one transaction, on the identity row locked FOR
// UPDATE, so concurrent sign-ins of one identity record their changes one
// after the other:
//
//   - The row's email, email_verified and email_checked_at are written.
//     user_id and linked_via never are: a sign-in records what the
//     provider presents, it never changes the link, whoever made it.
//   - When the identity held no email, an unverified one, or a different
//     address ignoring case, an identity.email_updated audit entry names
//     the previous and new email.
//   - When it held a different address, the user's primary email follows
//     it, in a savepoint: only while the primary email still is the
//     identity's previous email, and only when no other member holds the
//     new one (UserStore.FollowPrimaryEmail), audited as
//     user.primary_email_changed. A unique violation from a concurrent
//     insert of the same address leaves the primary email as it was and
//     the sign-in goes on: the primary email is not evidence.
//
// Any other failure -- the row is gone, a write or an audit entry fails
// -- rolls the whole refresh back and returns an error: the caller answers
// the sign-in with a 500 and mints no session. email must be verified and
// non-empty; both callers refuse a sign-in without one before getting
// here. The returned row is the identity as stored now; the caller signs
// in its UserID.
func RefreshSignInEvidence(ctx context.Context, deps Deps, identityID pgtype.UUID, email ProfileEmail, accessTokenEncrypted []byte) (sqlcgen.Identity, error) {
	if !email.Verified || email.Address == "" {
		return sqlcgen.Identity{}, errors.New("identitylink: refresh sign-in evidence: the provider presented no verified email")
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return sqlcgen.Identity{}, fmt.Errorf("identitylink: begin sign-in evidence tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	identities := deps.Identities.WithTx(tx)
	before, err := identities.GetByIDForUpdate(ctx, identityID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Identity{}, fmt.Errorf("identitylink: the signed-in identity %s is gone: %w", identityID.String(), err)
		}
		return sqlcgen.Identity{}, fmt.Errorf("identitylink: lock the signed-in identity: %w", err)
	}

	after, err := identities.RefreshSignInEmail(ctx, identityID, email.Address, accessTokenEncrypted)
	if err != nil {
		return sqlcgen.Identity{}, fmt.Errorf("identitylink: record the sign-in email: %w", err)
	}

	addressChanged := before.Email != nil && !strings.EqualFold(*before.Email, email.Address)
	updated := before.Email == nil || !before.EmailVerified || addressChanged
	if updated {
		var previous any
		if before.Email != nil {
			previous = *before.Email
		}
		if err := auditlog.Record(ctx, deps.AuditLog.WithTx(tx), before.UserID, auditActionIdentityEmailUpdated, "identity", before.ID.String(), map[string]any{
			"provider":          string(before.Provider),
			"external_id":       before.ExternalID,
			"user_id":           before.UserID.String(),
			"previous_email":    previous,
			"previous_verified": before.EmailVerified,
			"email":             email.Address,
		}); err != nil {
			return sqlcgen.Identity{}, fmt.Errorf("identitylink: record audit log: %w", err)
		}
	}

	if addressChanged {
		if err := followPrimaryEmail(ctx, deps, tx, after, *before.Email, email.Address); err != nil {
			return sqlcgen.Identity{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return sqlcgen.Identity{}, fmt.Errorf("identitylink: commit sign-in evidence tx: %w", err)
	}

	result := evidenceResultConfirmed
	if updated {
		result = evidenceResultUpdated
	}
	evidenceRefreshes.Add(ctx, 1, metric.WithAttributes(
		attribute.String("provider", string(after.Provider)),
		attribute.String("result", result),
	))
	return after, nil
}

// followPrimaryEmail moves identity's user's primary email from previous
// (the identity's email before this sign-in) to next, inside a savepoint
// of tx, with its user.primary_email_changed audit entry. Not moving --
// the primary email came from elsewhere, or another member holds next --
// is not an error, and neither is a unique violation from a concurrent
// insert of next, which rolls back to the savepoint. Any other error is.
func followPrimaryEmail(ctx context.Context, deps Deps, tx pgx.Tx, identity sqlcgen.Identity, previous, next string) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("identitylink: begin primary email savepoint: %w", err)
	}
	defer func() { _ = sp.Rollback(ctx) }()

	moved, err := deps.Users.WithTx(sp).FollowPrimaryEmail(ctx, identity.UserID, previous, next)
	if err != nil {
		if isUniqueViolation(err) {
			if rbErr := sp.Rollback(ctx); rbErr != nil {
				return fmt.Errorf("identitylink: roll back primary email savepoint: %w", rbErr)
			}
			return nil
		}
		return fmt.Errorf("identitylink: follow primary email: %w", err)
	}
	if moved == 0 {
		return nil
	}

	if err := auditlog.Record(ctx, deps.AuditLog.WithTx(sp), identity.UserID, auditActionUserPrimaryEmailChanged, "user", identity.UserID.String(), map[string]any{
		"previous_email": previous,
		"email":          next,
		"identity_id":    identity.ID.String(),
		"provider":       string(identity.Provider),
	}); err != nil {
		return fmt.Errorf("identitylink: record audit log: %w", err)
	}

	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("identitylink: release primary email savepoint: %w", err)
	}
	return nil
}
