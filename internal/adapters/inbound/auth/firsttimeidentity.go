package auth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/app/identitylink"
	domainidentitylink "github.com/narvidev/narvi/internal/domain/identitylink"
)

// firstTimeIdentityDeps bundles resolveFirstTimeIdentity's own store
// dependencies -- the shared subset of NewCallbackHandler's/
// NewOIDCCallbackHandler's own params this merge logic needs
// (pool/users/identities/auditLog/linkPrompts). Allowlist evaluation is
// deliberately NOT included here -- see resolveFirstTimeIdentity's own
// checkAllowed parameter.
type firstTimeIdentityDeps struct {
	pool        *pgxpool.Pool
	users       *postgres.UserStore
	identities  *postgres.IdentityStore
	auditLog    *postgres.AuditLogStore
	linkPrompts *postgres.IdentityLinkPromptStore
}

// firstTimeOutcome is resolveFirstTimeIdentity's own generic verdict.
// Each caller (callback.go/oidccallback.go) translates it into that
// caller's own named CallbackOutcome/OIDCCallbackOutcome for logging, so
// this package's server-side log lines keep their existing,
// provider-specific vocabulary (outcome=first_time_allowed vs.
// outcome=oidc_first_time_allowed) even though the underlying decision is
// now made in exactly one place.
type firstTimeOutcome int

const (
	firstTimeAutoLinked firstTimeOutcome = iota
	firstTimeCreated
	firstTimeDenied
	firstTimeAmbiguous
)

// resolveFirstTimeIdentity implements §13.2 step 3's own email-based graph
// merge for a FIRST-TIME (provider, externalID) identity -- i.e. the
// caller has already run GetByProviderAndExternalID and gotten
// pgx.ErrNoRows for this exact (provider, externalID) pair. Shared,
// verbatim, by BOTH callback.go's GitHub callback and oidccallback.go's
// OIDC callback (§41.3: "the graph merges on verified email exactly as
// §13.2 step 3").
//
// # Why this exists (review round 1, findings O1/O2/O9)
//
// Before this function existed, only the OIDC callback ran this merge --
// resolveOIDCUser called identitylink.MatchUserIDs/AutoLink directly. The
// GitHub callback's own first-time branch went straight from an identity
// lookup miss to the allowlist to createUserAndIdentity, with no email
// match at all. Once OIDC sign-in could create a user with no GitHub
// identity (this Step's own whole point), that gap became reachable: an
// OIDC-only user's LATER GitHub sign-in with the SAME verified email hit
// users.primary_email's own UNIQUE constraint and failed with a 500, and
// a case-DIFFERENT verified email (users.primary_email is a plain,
// case-sensitive UNIQUE; MatchUserIDs/GetByPrimaryEmail compare
// lower(email) on both sides) silently created a SECOND user for the same
// person instead of erroring -- either way, never the §13.2 merge §41.3
// promises ("an OIDC-only user can link GitHub later through the
// ordinary GitHub flow"). Sharing this ONE function between both
// callbacks is what makes that structurally impossible now: neither
// caller ever reaches its own createFirstTime closure without first
// proving (via MatchUserIDs) that no existing user's primary_email or
// verified identity email matches, case-insensitively -- so
// createUserAndIdentity/createOIDCUserAndIdentity can never again attempt
// to INSERT a users row whose lower(primary_email) collides with an
// existing one.
//
// # The three branches
//
//   - Exactly one existing user matches verifiedEmail (by primary_email
//     or another verified identity's own email, case-insensitively,
//     domain/identitylink.Decide) -> identitylink.AutoLink's onto that
//     SAME account (firstTimeAutoLinked). checkAllowed is NEVER called on
//     this branch: this user already passed the allowlist once, under
//     whichever identity first created their account -- mirrors
//     resolveOIDCUser's own pre-existing "the allowlist is skipped here
//     too" behavior, now shared rather than re-derived per caller.
//   - Zero existing users match -> checkAllowed() decides between
//     createFirstTime (a brand-new user+identity, firstTimeCreated) and a
//     denial (firstTimeDenied, no row ever created). checkAllowed is a
//     func, called LAZILY -- exactly once, only on this branch -- so a
//     GitHub caller's own org-membership check (a real network round trip
//     to the GitHub API, checkAnyOrgMembership) is never spent when a
//     merge match already resolved this identity for free.
//   - More than one existing user matches -> refused, never a guess
//     (firstTimeAmbiguous), audited under auditAction (each caller's own
//     action string -- e.g. the OIDC callback keeps writing exactly
//     "identity.oidc_ambiguous_match", asserted by this package's own
//     pre-existing tests) -- regardless of what checkAllowed would have
//     said; this branch never even calls it.
//
// # Return shape
//
// (userID, firstTimeAutoLinked/firstTimeCreated, nil) on a success the
// caller should mint a session for; (pgtype.UUID{}, firstTimeDenied/
// firstTimeAmbiguous, nil) on a clean, already-handled refusal -- err is
// nil here on purpose, exactly like resolveOIDCUser's own pre-existing
// httpStatus!=0 convention: these are not failures, they are this
// function's OWN considered verdicts, and the caller's only job is to
// translate the outcome into the right HTTP response; or
// (pgtype.UUID{}, 0, err) on a genuine failure (a DB error, a broken
// invariant) the caller must turn into a 500.
func resolveFirstTimeIdentity(
	ctx context.Context,
	deps firstTimeIdentityDeps,
	provider sqlcgen.IdentityProvider,
	externalID, verifiedEmail string,
	encryptedToken []byte,
	auditAction string,
	checkAllowed func() bool,
	createFirstTime func(ctx context.Context) (pgtype.UUID, error),
) (pgtype.UUID, firstTimeOutcome, error) {
	linkDeps := identitylink.Deps{
		Pool:       deps.pool,
		Users:      deps.users,
		Identities: deps.identities,
		AuditLog:   deps.auditLog,
	}

	matched, err := identitylink.MatchUserIDs(ctx, linkDeps, verifiedEmail)
	if err != nil {
		return pgtype.UUID{}, 0, fmt.Errorf("auth: match user ids: %w", err)
	}

	if matchedUserIDStr, ok := domainidentitylink.Decide(matched); ok {
		linkDeps.LinkPrompts = deps.linkPrompts
		// encryptedToken (review round 1, O1/O2/O9 follow-on): nil for
		// OIDC (never stores a provider token), the caller-supplied,
		// just-obtained GitHub OAuth token for a GitHub merge -- see
		// identitylink.AutoLink's own accessTokenEncrypted doc comment for
		// why omitting this would silently leave a merged GitHub identity
		// with no usable git credential.
		res, err := identitylink.AutoLink(ctx, linkDeps, provider, externalID, verifiedEmail, matchedUserIDStr, encryptedToken)
		if err != nil {
			return pgtype.UUID{}, 0, fmt.Errorf("auth: auto-link first-time identity: %w", err)
		}
		return res.UserID, firstTimeAutoLinked, nil
	}

	switch len(matched) {
	case 0:
		if !checkAllowed() {
			return pgtype.UUID{}, firstTimeDenied, nil
		}
		createdUserID, createErr := createFirstTime(ctx)
		if createErr != nil {
			return pgtype.UUID{}, 0, fmt.Errorf("auth: create first-time user: %w", createErr)
		}
		return createdUserID, firstTimeCreated, nil

	default:
		// More than one existing user matches this verified email --
		// §13.2's own "never guess" rule. Audited: a human should look at
		// why more than one account shares a verified email. Never blocks
		// the refusal on the audit write failing -- the refusal itself is
		// the safe direction either way; a lost audit row here is an
		// observability gap, not a security one (mirrors resolveOIDCUser's
		// own pre-existing identical discipline).
		auditErr := auditlog.Record(ctx, deps.auditLog, pgtype.UUID{}, auditAction, "identity", externalID, map[string]any{
			"provider":         string(provider),
			"external_id":      externalID,
			"matched_user_ids": matched,
			"matched_count":    len(matched),
		})
		if auditErr != nil {
			_ = auditErr
		}
		return pgtype.UUID{}, firstTimeAmbiguous, nil
	}
}
