package identitylink

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
	domainidentitylink "github.com/narvidev/narvi/internal/domain/identitylink"
	"github.com/narvidev/narvi/internal/platform"
)

// uniqueViolationCode is Postgres' own SQLSTATE for a unique-constraint
// violation -- used below to recognize the one race this package's own
// two INSERTs can lose to a concurrent winner (identities.
// identities_provider_external_id_key, migrations/000003_identities.up.sql),
// without needing a second, separate SELECT-then-check round trip on
// every call's own happy path.
const uniqueViolationCode = "23505"

// MagicLinkPath is the route internal/adapters/inbound/identitylink
// mounts its magic-link consume handler at (cmd/control-plane/main.go:
// router.Get(identitylink.MagicLinkPath+"{nonce}", ...)) -- defined HERE,
// in the app-layer package, rather than in that inbound package itself,
// so BuildMagicLinkURL below and the inbound package's own route
// registration can never drift apart (the inbound package imports this
// one, never the reverse -- see this package's own doc.go).
const MagicLinkPath = "/auth/identity-link/"

// BuildMagicLinkURL renders the full, clickable magic-link URL for nonce
// -- publicBaseURL is platform.Config.PublicBaseURL, the SAME base every
// other externally-reachable URL this codebase constructs uses (e.g.
// auth.NewGitHubOAuthConfig's own RedirectURL).
func BuildMagicLinkURL(publicBaseURL, nonce string) string {
	return publicBaseURL + MagicLinkPath + nonce
}

// Deps bundles every store/config Resolve/Consume need -- mirrors this
// codebase's own established "Deps struct, not a long positional
// parameter list" precedent (internal/adapters/inbound/slack.Deps et al).
type Deps struct {
	Pool        *pgxpool.Pool
	Users       *postgres.UserStore
	Identities  *postgres.IdentityStore
	LinkPrompts *postgres.IdentityLinkPromptStore
	AuditLog    *postgres.AuditLogStore

	PublicBaseURL string
	PromptTTL     time.Duration
}

// Resolution is Resolve's own verdict for one inbound event from
// (provider, externalID) -- see this package's own doc.go for the full
// algorithm each field corresponds to.
type Resolution struct {
	// UserID is Valid iff this (provider, externalID) identity is now
	// known to belong to a real user -- either it already did (the fast
	// path, no provider API call at all) or Resolve just auto-linked it
	// THIS call. Invalid means: keep bot attribution, exactly as before.
	UserID pgtype.UUID

	// AutoLinked is true iff THIS call performed a BRAND NEW auto-link
	// (as opposed to UserID being Valid because the identity was already
	// linked coming in) -- the caller's own signal to post the
	// in-channel "connected your account" notice (§13.2 step 3).
	AutoLinked bool

	// MagicLinkURL is non-empty iff a NEW link prompt was just minted
	// this call -- the caller's own signal to post it in-channel (§13.2
	// step 4). Empty on every other outcome, INCLUDING when a still-live
	// prompt from an earlier call already exists (see service.go's own
	// "never re-mint/re-send on every message" policy) -- the caller
	// must not re-post a magic link it has already posted before.
	MagicLinkURL string
}

// NotificationText renders the §13.2 step-3/4 "notify the user in-channel"
// text for r, or "" when there's nothing to say (the identity was already
// linked coming in, the fetch failed/found no email, or a still-live
// prompt from an earlier call was silently reused). Centralized here, not
// duplicated per-provider, so the wording an ingress caller posts into
// Slack/Linear never drifts between the two -- each caller decides WHERE
// to put this text (appended to an existing outbound message, a fresh
// one, or skipped entirely when no natural channel/thread context exists
// for it), never what it says.
func (r Resolution) NotificationText() string {
	switch {
	case r.AutoLinked:
		return "I've connected this to your Narvi account."
	case r.MagicLinkURL != "":
		return "I couldn't automatically match this to a Narvi account. Connect it here: " + r.MagicLinkURL
	default:
		return ""
	}
}

// ProfileEmail is one provider profile-email fetch's answer: the address
// the provider holds for the identity, and whether the provider vouches for
// it under §13.2's definition of a verified email for that provider.
type ProfileEmail struct {
	// Address is the email; "" when the provider reported none.
	Address string

	// Verified is true only when the provider vouches for Address under
	// §13.2's definition for that provider. For Slack that is
	// slackapi.UserEmail.Verified: Slack reports the email as confirmed,
	// for a full member of this installation's own workspace. For Linear
	// it is any email Linear returns for the user. Resolve links an
	// identity automatically only on a Verified email, and AutoLink stores
	// Verified as identities.email_verified, never more.
	Verified bool
}

// autoLinkUnchecked reports whether i is an automatic link
// (linked_via=auto_email) that nothing has confirmed under §13.2's
// definition of a verified email: one whose email is not verified, or a
// Slack one with no email_checked_at -- made by a binary that took any
// Slack profile email as verified (migrations/000177). Such a link is not
// used: LookupLinkedUserID reads it as not linked, Resolve re-checks it
// before resolving it to anyone, and its email is not evidence for
// MatchUserIDs (queries/identities.sql's ListVerifiedIdentityUserIDsByEmail
// and DeleteUncheckedAutoLinkedIdentity name the same rows). A link a
// person or an admin made (linked_via prompt or admin) is never unchecked.
func autoLinkUnchecked(i sqlcgen.Identity) bool {
	if i.LinkedVia != sqlcgen.IdentityLinkedViaAutoEmail {
		return false
	}
	return !i.EmailVerified || (i.Provider == sqlcgen.IdentityProviderSlack && !i.EmailCheckedAt.Valid)
}

// LookupLinkedUserID performs the SAME existing-identity lookup Resolve's
// own internal fast path (below) does, but is EXPORTED for a caller that
// wants to pre-check BEFORE spending a provider profile-email fetch it
// would otherwise just discard on a Resolve fast-path hit -- see
// internal/adapters/inbound/{slack,linear}/identity.go's own pre-check call
// sites for the "why" (every event, for every already-linked identity, used
// to pay a fetch round trip whose result Resolve's own fast path below
// never even reads).
//
// Returns (userID, true, nil) on a hit (this (provider, externalID) is
// already linked -- userID is who to), (pgtype.UUID{}, false, nil) on a
// definitive miss (pgx.ErrNoRows -- not linked, the caller should proceed
// with its own fetch+Resolve exactly as before this pre-check existed) and
// on an automatic link awaiting its re-check (autoLinkUnchecked: not used
// until Resolve, given the email the caller then fetches, confirms it), or
// (pgtype.UUID{}, false, err) on any OTHER lookup error -- deliberately NOT
// collapsed into "not linked": a transient DB error is not evidence of
// anything about this identity's link state, so every current caller
// treats it as "unknown, fall through to the fetch", the same behavior this
// pre-check didn't change (Resolve's own identical internal lookup below
// would hit the exact same error a moment later regardless).
func LookupLinkedUserID(ctx context.Context, deps Deps, provider sqlcgen.IdentityProvider, externalID string) (userID pgtype.UUID, linked bool, err error) {
	existing, found, err := lookupExistingIdentity(ctx, deps, provider, externalID)
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	if !found || autoLinkUnchecked(existing) {
		return pgtype.UUID{}, false, nil
	}
	return existing.UserID, true, nil
}

// lookupExistingIdentity is Resolve's own fast-path lookup AND
// LookupLinkedUserID's shared implementation -- factored out so the one
// (provider, externalID) -> identities row query used by both has exactly
// one place it can drift from.
func lookupExistingIdentity(ctx context.Context, deps Deps, provider sqlcgen.IdentityProvider, externalID string) (sqlcgen.Identity, bool, error) {
	existing, err := deps.Identities.GetByProviderAndExternalID(ctx, provider, externalID)
	if err == nil {
		return existing, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.Identity{}, false, nil
	}
	return sqlcgen.Identity{}, false, err
}

// Resolve implements §13.2's auto-linking algorithm for one inbound event
// from (provider, externalID) -- see this package's own doc.go for the
// complete step-by-step design. email/emailOK is the CALLER's own
// already-fetched-and-retried provider profile email (see that doc
// comment for why fetching itself is not this function's job).
//
// Only a Verified email links an identity automatically: an email the
// provider does not vouch for (for Slack, one Slack does not report as
// confirmed for a full member of this workspace) skips the match entirely
// and gets the link prompt, the account-link flow, exactly as zero matches
// do. An already-linked identity resolves at once, unless it is an
// automatic link awaiting its re-check (autoLinkUnchecked), which
// recheckAutoLink settles first on this same email.
//
// Kept as a SAFETY NET even though every current caller now pre-checks via
// LookupLinkedUserID above before ever fetching (see that function's own
// doc comment): a future caller that forgets to pre-check still gets the
// exact same correct fast-path outcome here, just without the fetch having
// been skipped -- belt and braces, one indexed lookup, deliberately never
// removed.
func Resolve(ctx context.Context, deps Deps, provider sqlcgen.IdentityProvider, externalID string, email ProfileEmail, emailOK bool) (Resolution, error) {
	existing, found, err := lookupExistingIdentity(ctx, deps, provider, externalID)
	if err != nil {
		return Resolution{}, fmt.Errorf("identitylink: look up existing identity: %w", err)
	}
	if found {
		if !autoLinkUnchecked(existing) {
			return Resolution{UserID: existing.UserID}, nil
		}
		res, settled, err := recheckAutoLink(ctx, deps, existing, email, emailOK)
		if err != nil || settled {
			return res, err
		}
		// The unchecked link is gone: this identity is unlinked, and the
		// algorithm below runs on the email just fetched.
	}

	if !emailOK {
		// The caller's own fetch (already retried) never succeeded --
		// §13.2's own rule is that this is a retryable failure, not an
		// empty identity: never guess, never null anything out, just
		// leave this attempt for a LATER event from the same identity to
		// try again. Bot attribution, nothing else to do.
		return Resolution{}, nil
	}

	if !email.Verified {
		// §13.2: an email the provider does not vouch for links nothing
		// automatically -- never matched, never stored as verified. The
		// link prompt is the way to link it, the same outcome (and the
		// same notice) as an email that matches no one.
		return createOrReuseLinkPrompt(ctx, deps, provider, externalID)
	}

	matchedUserIDs, err := MatchUserIDs(ctx, deps, email.Address)
	if err != nil {
		return Resolution{}, fmt.Errorf("identitylink: match user ids: %w", err)
	}

	if userIDStr, ok := domainidentitylink.Decide(matchedUserIDs); ok {
		// nil: Resolve's own Slack/Linear callers never have a provider
		// access token to store on the auto-linked identity row -- see
		// AutoLink's own accessTokenEncrypted parameter doc comment for
		// the ONE caller that does (internal/adapters/inbound/auth's own
		// GitHub-side first-time merge).
		return AutoLink(ctx, deps, provider, externalID, email, userIDStr, nil)
	}

	return createOrReuseLinkPrompt(ctx, deps, provider, externalID)
}

// recheckAutoLink settles existing, an automatic link awaiting its re-check
// (autoLinkUnchecked), on the email the caller has just fetched for it.
// settled=false means the link is gone and Resolve carries on as for an
// unlinked identity; otherwise res is Resolve's answer.
//
//   - No email learned (emailOK false: the fetch failed, or the provider
//     showed none): nothing is decided. The link is neither kept nor
//     removed, and resolves to no one for this event; the next event
//     re-checks it.
//   - The provider vouches for the same email (Verified, equal regardless
//     of case, as matching compares it): the link is kept and stamped
//     (ConfirmAutoLinkEmail), and resolves as before.
//   - Anything else -- the email is not verified, or it is no longer the
//     email the link was made on: the link is removed (revokeAutoLink),
//     audited, and the identity goes through the algorithm again, which
//     links it automatically only on a verified email that matches
//     exactly one user, and otherwise sends the link prompt.
//
// Only an automatic link is ever re-checked or removed: a link a person
// confirmed through the link prompt, or one an admin made, is never
// unchecked (autoLinkUnchecked), and both store calls are scoped to
// linked_via auto_email in SQL too. When either store call finds the row
// already changed by a concurrent event, the identity is looked up again:
// a confirmed link resolves, one still unchecked resolves to no one for
// this event, and a removed one leaves the identity unlinked.
func recheckAutoLink(ctx context.Context, deps Deps, existing sqlcgen.Identity, email ProfileEmail, emailOK bool) (Resolution, bool, error) {
	if !emailOK {
		return Resolution{}, true, nil
	}

	if email.Verified && existing.Email != nil && strings.EqualFold(*existing.Email, email.Address) {
		confirmed, err := deps.Identities.ConfirmAutoLinkEmail(ctx, existing.ID)
		if err != nil {
			return Resolution{}, true, fmt.Errorf("identitylink: confirm automatic link: %w", err)
		}
		if confirmed == 1 {
			return Resolution{UserID: existing.UserID}, true, nil
		}
	} else {
		reason := "email_not_verified"
		if email.Verified {
			reason = "email_changed"
		}
		revoked, err := revokeAutoLink(ctx, deps, existing, reason)
		if err != nil {
			return Resolution{}, true, err
		}
		if revoked {
			return Resolution{}, false, nil
		}
	}

	again, found, err := lookupExistingIdentity(ctx, deps, existing.Provider, existing.ExternalID)
	if err != nil {
		return Resolution{}, true, fmt.Errorf("identitylink: look up identity again after re-check: %w", err)
	}
	switch {
	case !found:
		return Resolution{}, false, nil
	case autoLinkUnchecked(again):
		return Resolution{}, true, nil
	default:
		return Resolution{UserID: again.UserID}, true, nil
	}
}

// revokeAutoLink removes existing, an automatic link whose re-check failed,
// with its identity.auto_link_revoked audit entry, in ONE transaction
// (§13.3) -- the entry names the user the identity was linked to and why
// (reason), so the removal is never silent. Reports whether it removed the
// row: DeleteUncheckedAutoLink deletes only a row that is still an
// unchecked automatic link, so a concurrent event that confirmed or
// removed it first leaves nothing here to remove, and no entry is written.
func revokeAutoLink(ctx context.Context, deps Deps, existing sqlcgen.Identity, reason string) (bool, error) {
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("identitylink: begin revoke tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	deleted, err := deps.Identities.WithTx(tx).DeleteUncheckedAutoLink(ctx, existing.ID)
	if err != nil {
		return false, fmt.Errorf("identitylink: delete unchecked automatic link: %w", err)
	}
	if deleted == 0 {
		return false, nil
	}

	if err := auditlog.Record(ctx, deps.AuditLog.WithTx(tx), pgtype.UUID{}, "identity.auto_link_revoked", "identity", existing.ID.String(), map[string]any{
		"provider":    string(existing.Provider),
		"external_id": existing.ExternalID,
		"user_id":     existing.UserID.String(),
		"reason":      reason,
	}); err != nil {
		return false, fmt.Errorf("identitylink: record audit log: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("identitylink: commit revoke tx: %w", err)
	}
	return true, nil
}

// MatchUserIDs runs §13.2 step 2's own two lookups (users.primary_email,
// verified identities.email) and returns the DEDUPLICATED union of user
// ids either matched -- the same user matching both ways at once (e.g.
// their GitHub-derived primary_email happens to equal their own Slack
// profile email, which is ALSO independently verified on some other
// identity) must still count as exactly one match, never two.
//
// Exported (capital M) so internal/adapters/inbound/auth's own OIDC
// callback (§41.3) can run the EXACT SAME email-based graph
// merge Resolve's own zero-match branch below does for Slack/Linear,
// rather than a second, drifting copy of these two lookups -- that
// caller's own "zero matches" case differs from Resolve's (a brand new
// sign-in creates a user instead of minting a link prompt), so it cannot
// just call Resolve itself, but the MATCHING half is identical either
// way.
func MatchUserIDs(ctx context.Context, deps Deps, email string) ([]string, error) {
	seen := make(map[string]struct{}, 2)
	var out []string
	add := func(id pgtype.UUID) {
		s := id.String()
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}

	byPrimaryEmail, err := deps.Users.GetByPrimaryEmail(ctx, email)
	switch {
	case err == nil:
		add(byPrimaryEmail.ID)
	case errors.Is(err, pgx.ErrNoRows):
		// no primary_email match -- fine, the verified-identity-email
		// half below might still match.
	default:
		return nil, fmt.Errorf("get user by primary email: %w", err)
	}

	// An automatic link awaiting its re-check (autoLinkUnchecked) is left
	// out by the query itself: its email is no evidence until confirmed.
	byVerifiedIdentity, err := deps.Identities.ListVerifiedUserIDsByEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("list verified identity user ids by email: %w", err)
	}
	for _, id := range byVerifiedIdentity {
		add(id)
	}

	return out, nil
}

// AutoLink inserts the identities row (linked_via=auto_email) and its
// audit-log entry in ONE transaction (§13.3: "written in the same
// transaction as the change"), then deletes any still-pending link
// prompt for this same identity -- a resolved auto-link supersedes an
// earlier "we couldn't tell yet" prompt.
//
// Exported (capital A) for the SAME reason MatchUserIDs above is:
// internal/adapters/inbound/auth's own resolveFirstTimeIdentity (shared,
// verbatim, by BOTH the GitHub and OIDC callbacks, §41.3/review round 1
// findings O1/O2/O9) calls this EXACT function -- never a second copy --
// once its own caller-supplied email/provider/externalID resolve to
// exactly one existing user via MatchUserIDs+domain/identitylink.Decide,
// so a second sign-in through a DIFFERENT provider that happens to share
// a verified email merges onto the same user row exactly like an
// unrecognized Slack/Linear identity already does (§13.2 step 3).
//
// email.Verified is stored as identities.email_verified exactly as the
// caller attests it, never assumed: "verified" means the provider vouches
// for the email under §13.2's definition for that provider, and only a
// verified email is evidence for matching another identity (MatchUserIDs)
// or leaves this link usable (autoLinkUnchecked). Resolve passes only a
// Verified email -- for Slack, one Slack reports as confirmed, for a full
// member of this workspace (slackapi.UserEmail.Verified); for Linear, the
// email Linear returns for the user. The GitHub callback passes its
// verified primary email (githubUser's own doc comment, internal/adapters/
// inbound/auth/callback.go), the OIDC callback an email whose ID token
// carries email_verified true (checked before this is ever called). A
// Verified email is also stamped email_checked_at, which tells a Slack
// automatic link made under §13.2's Slack definition from one an older
// binary made without it (migrations/000177).
//
// accessTokenEncrypted is nil for every caller except the GitHub-side
// first-time merge (resolveFirstTimeIdentity's own encryptedToken
// parameter): Resolve's own Slack/Linear callers above, and the OIDC
// callback (§41.3: "this package never stores an OIDC provider token"),
// never have one to store. A first-time GitHub sign-in whose verified
// email merges onto an EXISTING user (rather than creating a new one)
// must still store the just-obtained GitHub OAuth token on the newly
// auto-linked identity row -- omitting it here would silently leave that
// merged user with a GitHub identity but no usable git credential,
// defeating §41.3's own "the graph merges... exactly as §13.2 step 3"
// promise (a merge that can't actually push or open a PR is not a
// working merge) -- this was review round 1's own O1/O2/O9 follow-on
// finding, caught by this package's own cross-provider integration test
// asserting the decrypted token on the merged identity row.
//
// actor_user_id is NULL on the audit-log row: this is a SYSTEM-driven
// match (an automated algorithm resolved it), not a human clicking
// anything -- mirrors sessions.created_by/plans.decided_by's own
// established NULL-for-no-human-actor convention (§17.5), never a
// fabricated "system user" row; the matched user's own id is still
// recorded, in detail_json, for a reader of the audit log to see exactly
// who was linked.
func AutoLink(ctx context.Context, deps Deps, provider sqlcgen.IdentityProvider, externalID string, email ProfileEmail, matchedUserIDStr string, accessTokenEncrypted []byte) (Resolution, error) {
	var matchedUserID pgtype.UUID
	if err := matchedUserID.Scan(matchedUserIDStr); err != nil {
		return Resolution{}, fmt.Errorf("identitylink: parse matched user id: %w", err)
	}

	var emailCheckedAt pgtype.Timestamptz
	if email.Verified {
		emailCheckedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return Resolution{}, fmt.Errorf("identitylink: begin auto-link tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, err := deps.Identities.WithTx(tx).Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:               matchedUserID,
		Provider:             provider,
		ExternalID:           externalID,
		Email:                &email.Address,
		EmailVerified:        email.Verified,
		LinkedVia:            sqlcgen.IdentityLinkedViaAutoEmail,
		AccessTokenEncrypted: accessTokenEncrypted,
		EmailCheckedAt:       emailCheckedAt,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Lost a race: a concurrent event for the SAME (provider,
			// externalID) already linked it (auto-link or an admin
			// force-link) between this call's own initial
			// GetByProviderAndExternalID miss and this INSERT. Resolve
			// the WINNER's real row instead of surfacing a spurious
			// error -- mirrors internal/adapters/inbound/slack's own
			// identical "lost the claim race, resolve the winner"
			// precedent (resolveOrClaimSession, handler.go).
			winner, getErr := deps.Identities.GetByProviderAndExternalID(ctx, provider, externalID)
			if getErr != nil {
				return Resolution{}, fmt.Errorf("identitylink: resolve winner after lost auto-link race: %w", getErr)
			}
			if autoLinkUnchecked(winner) {
				// The winner is an automatic link nothing has confirmed
				// (an older binary's, during a rollout): not used until a
				// later event re-checks it.
				return Resolution{}, nil
			}
			return Resolution{UserID: winner.UserID}, nil
		}
		return Resolution{}, fmt.Errorf("identitylink: create auto-linked identity: %w", err)
	}

	if err := auditlog.Record(ctx, deps.AuditLog.WithTx(tx), pgtype.UUID{}, "identity.auto_linked", "identity", created.ID.String(), map[string]any{
		"provider":    string(provider),
		"external_id": externalID,
		"user_id":     matchedUserIDStr,
	}); err != nil {
		return Resolution{}, fmt.Errorf("identitylink: record audit log: %w", err)
	}

	if err := deps.LinkPrompts.WithTx(tx).DeleteForProviderAndExternalID(ctx, provider, externalID); err != nil {
		return Resolution{}, fmt.Errorf("identitylink: delete superseded link prompts: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Resolution{}, fmt.Errorf("identitylink: commit auto-link tx: %w", err)
	}

	return Resolution{UserID: matchedUserID, AutoLinked: true}, nil
}

// createOrReuseLinkPrompt implements §13.2 step 4's own "never guess:
// create a link prompt" branch, WITH one added policy this table's own
// migration explicitly leaves open ("no UNIQUE constraint... this table
// only provides the storage shape, not yet the concurrency/resend
// policy"): if a still-unexpired prompt already exists for this
// (provider, externalID), it is reused (MagicLinkURL left empty in the
// returned Resolution, since the caller has ALREADY posted that same link
// once and must not spam it again on every subsequent inbound message) --
// only once the latest one has expired (or none exists yet) is a fresh
// nonce minted and a NEW magic link returned for the caller to post.
func createOrReuseLinkPrompt(ctx context.Context, deps Deps, provider sqlcgen.IdentityProvider, externalID string) (Resolution, error) {
	latest, err := deps.LinkPrompts.GetLatestForProviderAndExternalID(ctx, provider, externalID)
	switch {
	case err == nil && latest.ExpiresAt.Valid && latest.ExpiresAt.Time.After(time.Now()):
		return Resolution{}, nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return Resolution{}, fmt.Errorf("identitylink: look up latest link prompt: %w", err)
	}

	nonce, err := platform.GenerateToken()
	if err != nil {
		return Resolution{}, fmt.Errorf("identitylink: generate link prompt nonce: %w", err)
	}
	expiresAt := time.Now().Add(deps.PromptTTL)

	if _, err := deps.LinkPrompts.Create(ctx, sqlcgen.CreateIdentityLinkPromptParams{
		Provider:   provider,
		ExternalID: externalID,
		NonceHash:  platform.HashToken(nonce),
		ExpiresAt:  pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		return Resolution{}, fmt.Errorf("identitylink: create link prompt: %w", err)
	}

	return Resolution{MagicLinkURL: BuildMagicLinkURL(deps.PublicBaseURL, nonce)}, nil
}

// Sentinel errors Consume returns -- distinct from a genuine (500-worthy)
// failure, so internal/adapters/inbound/identitylink's own handler can
// render the right user-facing outcome for each.
var (
	// ErrLinkPromptNotFound means the presented nonce matches no row --
	// wrong, already consumed, or never existed (this package does not
	// distinguish those, mirroring auth.Middleware's own identical
	// "no differential signal" precedent).
	ErrLinkPromptNotFound = errors.New("identitylink: link prompt not found")
	// ErrLinkPromptExpired means the row exists but IdentityLinkPromptTTL
	// has elapsed -- Consume deletes the stale row as a side effect of
	// detecting this (best-effort; a failure to delete does not change
	// this return value) so a later click of the SAME expired link
	// short-circuits to ErrLinkPromptNotFound instead next time.
	ErrLinkPromptExpired = errors.New("identitylink: link prompt expired")
	// ErrIdentityAlreadyLinked means (provider, externalID) got linked to
	// someone by a DIFFERENT path (a concurrent auto-link, or an admin
	// force-link) between this prompt being created and this click being
	// consumed.
	ErrIdentityAlreadyLinked = errors.New("identitylink: identity already linked")
)

// Consume implements the magic-link counterpart of §13.2 step 4: nonce is
// the PLAINTEXT value from the clicked URL (Consume hashes it itself,
// platform.HashToken, before looking anything up -- mirrors user_sessions/
// ws_tokens' own identical hash-at-rest convention); authenticatedUserID
// is the ALREADY-authenticated visiting user (internal/adapters/inbound/
// identitylink's own handler resolves this via the existing GitHub OAuth
// web-login cookie BEFORE ever calling Consume -- see that package's own
// doc comment).
//
// On success, inserts the identities row (linked_via=prompt) and its
// audit-log entry (actor_user_id = authenticatedUserID THIS time -- a
// real human explicitly completed this action by visiting their own
// confirmed link, unlike autoLink's own NULL-actor system-driven case
// above) in ONE transaction, then deletes EVERY pending prompt for that
// same (provider, externalID) so a stale link can never be replayed.
//
// # Deliberate design note ("identities + full RBAC", §13.2 --
// security review remediation)
//
// This function performs NO correlation at all between authenticatedUserID
// (whoever is visiting) and the identity WHO ORIGINALLY TRIGGERED this
// prompt (the Slack/Linear event that caused createOrReuseLinkPrompt to
// mint it, service.go) -- it links whichever authenticated Narvi user
// presents a valid, unexpired, not-yet-consumed nonce, structurally the
// SAME shape a confirmed review finding demonstrated as an account-hijack
// in a shared channel (any other member with their own already-
// authenticated web session could open the link first and get the
// identity linked to THEIR account instead of its rightful owner's).
//
// This is deliberately NOT fixed here, because it cannot be: nonce
// possession is the ONLY signal this function -- or any code reachable
// from a bare (nonce, authenticatedUserID) pair -- has to work with.
// There is no independent, external proof of "authenticatedUserID is the
// same human as the Slack/Linear identity behind this nonce" available to
// check against; establishing exactly that correspondence is the whole
// point of the magic-link mechanism in the first place, so requiring it
// as a PRECONDITION here would be circular.
//
// The real fix is upstream, at DELIVERY: the caller that mints and posts
// this nonce's own URL (internal/adapters/inbound/slack, via ack.go's own
// postEphemeral) now scopes it to chat.postEphemeral, visible ONLY to the
// triggering Slack user -- never the whole channel/thread
// (chat.postMessage) a confirmed review finding demonstrated hijacking
// through. Once delivery is scoped that way, nonce possession itself
// becomes a sufficient proxy for "is the rightful recipient": only that
// one user's own Slack client ever renders the link at all. Consume
// remains structurally capable of linking a cross-user nonce if one ever
// leaked through some OTHER channel (e.g. a compromised Slack export, a
// browser history sync) -- accepted as a documented, practically
// unreachable residual, not a gap this function itself closes. (Linear's
// own delivery is NOT scoped this way -- see internal/adapters/inbound/
// linear/identity.go's own appendNotice doc comment for that accepted,
// separately-documented residual.)
//
// # Investigated and NOT done: narrowing Consume to the ambiguous
// candidate set (§13.2's own SECOND fix-pass re-review)
//
// One sub-case of createOrReuseLinkPrompt's own "zero or multiple
// matches -> never guess" branch (identitylink.Decide) is narrower than
// the other: "multiple matches" (createOrReuseLinkPrompt reached via more
// than one already-matched candidate user id) at least has a small,
// KNOWN set of legitimate recipients, unlike "zero matches" (any
// authenticated user is equally "unmatched"). Restricting Consume to
// accept authenticatedUserID only when it is one of THAT ambiguous call's
// own candidate ids would shrink (not close) the hijack window for that
// one sub-case.
//
// This was investigated and deliberately NOT implemented here: Decide
// (internal/domain/identitylink) does not distinguish "zero" from
// "multiple" in its own return value, createOrReuseLinkPrompt takes no
// candidate list parameter today, and identity_link_prompts itself
// (migrations/000036) has no column to persist one -- landing this would
// require a new migration, a sqlc-generated column/query change, a new
// Consume-side sentinel error, and a new outcome page in internal/
// adapters/inbound/identitylink's own consume handler, not a change that
// fits this function's existing signature/flow. Left as the SAME
// documented, accepted residual this doc comment already described,
// exactly as the prior fix pass left it -- a genuinely smaller redesign
// than it first appears, but still a redesign, not a targeted fix.
func Consume(ctx context.Context, deps Deps, nonce string, authenticatedUserID pgtype.UUID) (sqlcgen.Identity, error) {
	prompt, err := deps.LinkPrompts.GetByNonceHash(ctx, platform.HashToken(nonce))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Identity{}, ErrLinkPromptNotFound
		}
		return sqlcgen.Identity{}, fmt.Errorf("identitylink: look up link prompt: %w", err)
	}

	if !prompt.ExpiresAt.Valid || !prompt.ExpiresAt.Time.After(time.Now()) {
		// Best-effort cleanup -- its own failure must never mask the
		// real ErrLinkPromptExpired outcome below.
		_ = deps.LinkPrompts.Delete(ctx, prompt.ID)
		return sqlcgen.Identity{}, ErrLinkPromptExpired
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return sqlcgen.Identity{}, fmt.Errorf("identitylink: begin consume tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, err := deps.Identities.WithTx(tx).Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:     authenticatedUserID,
		Provider:   prompt.Provider,
		ExternalID: prompt.ExternalID,
		LinkedVia:  sqlcgen.IdentityLinkedViaPrompt,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return sqlcgen.Identity{}, ErrIdentityAlreadyLinked
		}
		return sqlcgen.Identity{}, fmt.Errorf("identitylink: create identity from link prompt: %w", err)
	}

	if err := auditlog.Record(ctx, deps.AuditLog.WithTx(tx), authenticatedUserID, "identity.linked_via_prompt", "identity", created.ID.String(), map[string]any{
		"provider":    string(prompt.Provider),
		"external_id": prompt.ExternalID,
	}); err != nil {
		return sqlcgen.Identity{}, fmt.Errorf("identitylink: record audit log: %w", err)
	}

	if err := deps.LinkPrompts.WithTx(tx).DeleteForProviderAndExternalID(ctx, prompt.Provider, prompt.ExternalID); err != nil {
		return sqlcgen.Identity{}, fmt.Errorf("identitylink: delete consumed link prompts: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return sqlcgen.Identity{}, fmt.Errorf("identitylink: commit consume tx: %w", err)
	}

	return created, nil
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505) -- the one race both autoLink and Consume's
// own INSERT can lose to a concurrent winner targeting the SAME
// identities.(provider, external_id) unique key.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode
}
