// Package actorauthz is the shared "resolved actor vs domain/authz.
// Authorize" gate used by every non-web ingress surface that resolves an
// inbound event to a real Narvi user_id before letting it cause a
// state-changing effect (create a session, prompt an existing one,
// approve/reject a plan).
//
// # Why this package exists
//
// §13.2 ("identities + full RBAC", §13.2/§13.3) added identical
// "resolved actor's role must still pass domain/authz.Authorize" gating to
// both internal/adapters/inbound/slack and internal/adapters/inbound/linear
// -- authorizeResolvedActor (the role/disabled check) and ownedOrJoined
// (the §13.3 row 2 "own/joined" carve-out resolution) were confirmed, by
// direct reading, to be identical between the two packages' own identity.go
// files, save for a "slack:"/"linear:" log-message prefix. A later audit
// (the batch this package itself was extracted for) found GitHub ingress
// was about to become a THIRD consumer of the exact same logic --
// duplicating it a third time crosses this codebase's own established
// "small, documented duplication over a forced shared abstraction"
// threshold (see e.g. internal/adapters/inbound/linear's own hasOpenTurn
// doc comment for that threshold's usual place: two copies), so this Step
// extracts it once, here, rather than hand-copying it again.
//
// Every ingress package (slack, linear, github) still keeps its own thin
// authorizeSessionAction-shaped wrapper (resolving a session row + calling
// OwnedOrJoined + one of AuthorizeResolvedActor/AuthorizeLinkedActor in
// sequence -- github, like slack/linear, calls AuthorizeLinkedActor since
// batch fix/deny-unlinked-github-actors) rather than importing a fourth,
// fully-generic "do everything" function from here -- each
// caller's own Deps/SessionCoalescer shape (which stores it already has
// threaded through, how it fetches a session row) differs enough that
// forcing one single shared function over that too would cost more than it
// saves; only the two genuinely-identical leaf functions moved.
//
// # What did NOT move
//
// Neither function performs any I/O to RESOLVE a provider identity to a
// user_id in the first place (that is Slack's resolveSlackActor / Linear's
// resolveActor / GitHub's resolveCommenterActor, each package's own,
// provider-specific job -- Slack/Linear auto-link via
// internal/app/identitylink, GitHub does a direct identities lookup, no
// auto-linking algorithm needed at all since every Narvi user already has a
// GitHub identity from OAuth sign-in). This package starts from an
// ALREADY-RESOLVED actorUserID and answers only "is this specific actor
// allowed to do this specific thing" -- it is deliberately blind to how
// that actorUserID was obtained.
//
// # The session read check
//
// AuthorizeSessionRead (sessionread.go) answers a read question rather
// than a state-changing one: may this user read this session right now.
// The ws-token mint and the client socket both call it (technical plan
// §6.2, §13.3), at mint, at subscribe and periodically on an open
// socket. It reuses the disabled-then-role verdict AuthorizeResolvedActor
// renders (verdictForUser), with ActionViewSessions, and adds what a read
// needs: a token with no user and a user or session that no longer exists
// are each a denial.
package actorauthz
