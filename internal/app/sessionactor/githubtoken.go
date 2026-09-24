// This file (githubtoken.go) holds decryptCreatorGitHubToken -- extracted
// out of pushpr.go (§9.3, "e2e happy path", design decision 8) so it
// reads as a genuinely shared Actor-level helper rather than something
// pushpr-specific: pushpr.go's own createPRBestEffort and dispatch.go's own
// resolveAndSetImage (§8.5, "image builds") both need the SAME "decrypt
// this session's creator's stored GitHub OAuth access token" logic, and
// duplicating it verbatim in two places was never the right call once a
// second caller existed. No behavior change from pushpr.go's own original
// version -- only the signature narrows from the whole sqlcgen.Session row
// down to just the one field (CreatedBy) this helper ever actually reads.
//
// This file ALSO holds CheckCreatorGuard/creatorMayGetPRAttribution (below)
// -- the shared "is this session creator's stored GitHub OAuth token still
// allowed to be used, right now" staleness recheck (§13.3 viewer guard).
// An audit sweep (cross-step, cross-package finding) found FOUR call sites
// that each mint or use a session creator's token long after session
// creation -- pushpr.go's own createPRBestEffort (the original
// check), internal/adapters/inbound/httpapi/scmcredentials.go's
// ScmCredentials (a DIFFERENT package, added its own inline copy of the
// identical check first, see that file's own doc comment), and this same
// audit sweep's own two further findings: contractdrift.go's
// checkContractDrift and imageresolve.go's resolveAndSetImage, which had
// NO such recheck at all. Rather than a third and fourth inline copy,
// CheckCreatorGuard is now the ONE place this check lives; every call site
// -- including scmcredentials.go, a different package that already
// imports this one for Registry/EnsureDispatched -- calls it directly.
// creatorMayGetPRAttribution itself is kept (pushpr.go's own call shape is
// unchanged) as a thin wrapper translating CheckCreatorGuard's verdict into
// this package's own pre-existing bool-plus-Warn-log idiom, so pushpr.go's
// own observable behavior is byte-for-byte unchanged by this extraction.
// contractdrift.go/imageresolve.go each call CheckCreatorGuard directly
// instead, translating the verdict into THEIR OWN pre-existing
// error-handling idiom (see each file's own call site) rather than being
// forced through creatorMayGetPRAttribution's specific bool/Warn shape,
// which does not fit either of them (a checkContractDrift or
// resolveAndSetImage failure is never something to "return false" from --
// both are void, log-and-skip functions, and imageresolve.go additionally
// needs to distinguish a genuine lookup failure (Error) from an
// expected/tolerated one (Warn), which creatorMayGetPRAttribution's own
// shape does not surface.

package sessionactor

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// decryptCreatorGitHubToken mirrors internal/adapters/inbound/httpapi's own
// scmcredentials.go ScmCredentials handler outcome table exactly (design
// decision 8) -- the SAME "no created_by user, or no github identity, or
// no stored token, or a decrypt failure -> no usable credential" class of
// absence, just logged rather than turned into an HTTP response, since
// every caller of this helper is an internal, best-effort side effect, not
// a request awaiting a status code. This is the honest "no bot/service-
// account fallback exists" gap named in §9.3's own brief (§8.11's
// fallback half), not a bug to work around by inventing one -- and, as of
// §8.5, ALSO the documented reason a session whose creator has no
// usable GitHub token still spawns successfully on the base image, never
// blocked or failed by image resolution (§10 Phase 2: "never block a
// session").
//
// createdBy is sessionRow.CreatedBy -- callers pass just this one field
// (not the whole session row) since it's the only one this helper reads.
func (a *Actor) decryptCreatorGitHubToken(ctx context.Context, createdBy pgtype.UUID) (string, bool) {
	if !createdBy.Valid {
		a.logger.Warn("sessionactor: session has no created_by user; no bot fallback exists (§8.11); skipping")
		return "", false
	}

	identity, err := a.stores.identity.GetByUserAndProvider(ctx, createdBy, sqlcgen.IdentityProviderGithub)
	if err != nil {
		a.logger.Warn("sessionactor: no usable github identity; skipping", "error", err)
		return "", false
	}
	if identity.AccessTokenEncrypted == nil {
		a.logger.Warn("sessionactor: github identity has no stored access token; skipping")
		return "", false
	}

	plaintext, err := platform.DecryptToken(a.tokenEncryptionKey, identity.AccessTokenEncrypted)
	if err != nil {
		// The decrypt error itself is safe to log (it never contains the
		// ciphertext/plaintext, see platform.DecryptToken's own doc
		// comment) -- the plaintext token it would have produced is NEVER
		// logged, here or anywhere else.
		a.logger.Error("sessionactor: decrypt access token failed", "error", err)
		return "", false
	}
	return string(plaintext), true
}

// creatorHasNoGitHubIdentity reports whether createdBy has NO github
// identity row at all -- as distinct from having one whose stored token
// is merely unusable (missing, undecryptable). Used ONLY by pushpr.go's
// own createPRBestEffort (review round 1, finding O5) to decide whether
// its §8.11 bot-identity fallback applies: an OIDC-only creator who has
// never linked GitHub (this reports true) is exactly the case that
// fallback exists for -- the honest PR body names them and points at
// Settings -> Identities. A creator who DOES have a github identity, but
// whose token has expired, been revoked, or fails to decrypt (this
// reports false), must NOT be silently re-attributed to the bot: that
// would misrepresent a token problem as "no linked account" to whoever
// reviews the PR. That case keeps origin/main's own pre-existing
// behavior -- decryptCreatorGitHubToken's caller simply skips PR
// creation entirely (see createPRBestEffort's own call site).
//
// A createdBy with no valid row at all (sentinel-auto-fix, no human
// creator) is treated as "no identity" here too -- never reached by
// createPRBestEffort's own call site in practice (that path returns
// earlier via the sentinel-auto-fix branch), but consistent with
// decryptCreatorGitHubToken's own identical createdBy.Valid short-circuit.
func (a *Actor) creatorHasNoGitHubIdentity(ctx context.Context, createdBy pgtype.UUID) bool {
	if !createdBy.Valid {
		return true
	}
	_, err := a.stores.identity.GetByUserAndProvider(ctx, createdBy, sqlcgen.IdentityProviderGithub)
	return errors.Is(err, pgx.ErrNoRows)
}

// CreatorGuardVerdict is CheckCreatorGuard's own result (below) -- the
// SAME §13.3 viewer-guard staleness recheck every one of this audit
// sweep's four call sites performs, deliberately returned as a small
// discriminated struct rather than a single bool: each call site's own
// pre-existing error-handling idiom differs (pushpr.go's Warn-only
// bool-return, scmcredentials.go's Error-vs-Warn/500-vs-403 split,
// contractdrift.go's uniform Warn-and-skip, imageresolve.go's own
// Error-vs-Warn split), and forcing all four through one identical
// bool-or-nothing signature would have lost exactly the information each
// needs to keep its own idiom. Exactly one of Allowed, Err!=nil, Disabled,
// or Viewer is meaningful per verdict (Allowed is also true for the
// "nothing for this guard to check" case -- see CheckCreatorGuard's own
// doc comment on createdBy).
type CreatorGuardVerdict struct {
	// Allowed is true when createdBy's CURRENT row was found, not
	// disabled, and not role==viewer -- the only case in which the
	// creator's stored GitHub token may still be used.
	Allowed bool

	// Err is non-nil only for a GetByID failure of ANY kind, including
	// pgx.ErrNoRows for a missing user row (should be unreachable --
	// created_by is a real FK, but every call site has always treated a
	// missing row the same as "nothing to fail loudly over"). ErrNotFound
	// records whether Err specifically was that pgx.ErrNoRows case, for
	// call sites that distinguish a genuine/unexpected DB failure
	// (Error-level log, sometimes a 500) from that expected-miss shape
	// (Warn-level log, a plain deny) -- e.g. scmcredentials.go's own
	// established sessions.Get/sandboxes.Get discipline.
	Err         error
	ErrNotFound bool

	// Disabled/Viewer record exactly which guard tripped, so each call
	// site's own log line can name it precisely without re-deriving it
	// from the (by then discarded) sqlcgen.User row.
	Disabled bool
	Viewer   bool
}

// CheckCreatorGuard re-reads createdBy's CURRENT row (Disabled, then
// Role) fresh, right now, against the §13.3 viewer-guard threshold
// ("viewers never gain PR-reviewer attribution or git identity on session
// artifacts") -- the shared staleness recheck every call site that mints
// or uses a session creator's stored GitHub OAuth token, long after
// session creation, must perform: a session can outlive the moment its
// creator was disabled (an admin's own offboarding/incident-response
// disable) or demoted to viewer (an admin's own role edit), and every
// OTHER place a resolved actor's authority is re-verified already denies
// exactly this (internal/adapters/inbound/slack/identity.go's
// authorizeResolvedActor, linear/identity.go's twin, auth/middleware.go's
// Authenticate) -- Disabled and Role are independent columns (migrations/
// 000002_users.up.sql), so checking Role alone would miss a disabled-but-
// non-viewer creator.
//
// Exported (capital C) so a DIFFERENT package --
// internal/adapters/inbound/httpapi's own scmcredentials.go, which
// already imports this package for Registry/EnsureDispatched -- can share
// this EXACT check rather than reimplementing a third, inline copy (audit
// finding, cross-step: two MORE call sites inside this package,
// contractdrift.go's checkContractDrift and imageresolve.go's
// resolveAndSetImage, were found carrying the identical gap alongside
// scmcredentials.go's own already-fixed inline copy -- all four now call
// this one function).
//
// createdBy is assumed already known-Valid by callers that care about
// distinguishing "no creator at all" from the checks this function
// performs: every one of today's four call sites already has its own
// established way of detecting/logging that specific, ordinary condition
// (pushpr.go/scmcredentials.go's own explicit CreatedBy.Valid checks,
// contractdrift.go/imageresolve.go's shared decryptCreatorGitHubToken's
// own identical check) -- so an invalid createdBy passed here regardless
// is simply reported Allowed (nothing for THIS guard to gate on), never a
// second, differently-worded silent deny competing with those.
func CheckCreatorGuard(ctx context.Context, users *postgres.UserStore, createdBy pgtype.UUID) CreatorGuardVerdict {
	if !createdBy.Valid {
		return CreatorGuardVerdict{Allowed: true}
	}

	creator, err := users.GetByID(ctx, createdBy)
	if err != nil {
		return CreatorGuardVerdict{Err: err, ErrNotFound: errors.Is(err, pgx.ErrNoRows)}
	}
	if creator.Disabled {
		return CreatorGuardVerdict{Disabled: true}
	}
	if creator.Role == sqlcgen.UserRoleViewer {
		return CreatorGuardVerdict{Viewer: true}
	}
	return CreatorGuardVerdict{Allowed: true}
}

// creatorMayGetPRAttribution is §13.2's own viewer guard (§13.3),
// called by pushpr.go's createPRBestEffort BEFORE it ever decrypts/uses
// createdBy's own GitHub token to open a pull request -- this is a
// SECOND, defense-in-depth check, distinct from (and in addition to)
// domain/authz.Authorize already refusing a viewer at session-CREATION
// time (httpapi.CreateSession, §13.3 row 2: "... on own/joined sessions:
// admin, maintainer, member" -- never viewer). That create-time check
// alone is not sufficient here: a session's creator's role AND disabled
// state are read FRESH, right here, at PR-creation time -- see
// CheckCreatorGuard's own doc comment (above) for the complete
// staleness/parity rationale, which this function now delegates to
// entirely.
//
// This function is now a thin wrapper translating CheckCreatorGuard's
// verdict into pushpr.go's own pre-existing bool-plus-Warn-log call shape
// -- extracted (audit finding, cross-step) once TWO MORE call sites
// (contractdrift.go, imageresolve.go) were found needing the identical
// underlying check but a DIFFERENT calling convention; see this file's
// own top comment. No behavior change from this function's own prior,
// inline version: the same createdBy.Valid short-circuit (silent, no
// log -- every call site's own established way of handling "no creator at
// all", not this guard's concern), the same unconditional Warn log on ANY
// GetByID failure (this function has never distinguished a genuine DB
// failure from a merely-absent row, unlike scmcredentials.go's own
// separate Error/Warn split), the same Warn-and-deny for Disabled/Viewer.
func (a *Actor) creatorMayGetPRAttribution(ctx context.Context, createdBy pgtype.UUID) bool {
	if !createdBy.Valid {
		return false
	}

	v := CheckCreatorGuard(ctx, a.stores.user, createdBy)
	switch {
	case v.Allowed:
		return true
	case v.Err != nil:
		a.logger.Warn("sessionactor: get session creator for viewer guard failed; skipping PR creation", "error", v.Err)
	case v.Disabled:
		a.logger.Warn("sessionactor: session creator is now disabled; refusing PR attribution (§13.3 viewer guard)", "user_id", createdBy.String())
	case v.Viewer:
		a.logger.Warn("sessionactor: session creator is now a viewer; refusing PR attribution (§13.3 viewer guard)", "user_id", createdBy.String())
	}
	return false
}
