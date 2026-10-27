// Package identitylink implements the pure, I/O-free HALF of §13.2's
// auto-link algorithm's own step 2/3 verdict: "Match against identity
// emails whose provider confirmed them recently. Exactly one verified
// match -> auto-link... Zero or multiple matches -> never guess."
//
// No I/O, no time.Now(), no randomness (§11) -- Decide below takes the
// ALREADY-QUERIED, deduplicated set of user ids a caller's own Postgres
// lookup (verified identities.email confirmed within
// IdentityEmailEvidenceMaxAge, internal/app/identitylink.MatchUserIDs)
// found to match a fetched provider profile email, and renders nothing
// more than that one verdict: exactly one -> auto-link that user;
// anything else (zero, or more than one) -> never guess, create a link
// prompt instead.
//
// This mirrors internal/domain/authz's own "the matrix lives here as
// data, the caller resolves whatever context it needs first" shape: just
// as authz.Resource.OwnedOrJoined is a pre-computed bool the CALLER
// derives via its own Postgres query before ever calling Authorize, the
// matched-user-id list here is resolved by the caller
// (internal/app/identitylink, which is NOT this package -- that one is
// the I/O-performing orchestrator: the DB lookup, the actual identities/
// identity_link_prompts writes, the audit-log entry, and the in-channel
// notification) before ever calling Decide.
//
// The REST of the algorithm -- fetching the provider profile email at all
// (§13.2: "a provider email-API failure is a retryable error"; see
// platform.Retry), running the SQL match, writing the resulting
// identities/identity_link_prompts row inside a transaction, and sending
// the in-channel/in-app notification -- is all real I/O and lives in
// internal/app/identitylink instead, exactly the same domain/app split
// internal/domain/turn (the pure state machine) vs internal/app/
// sessionactor (the actor that actually calls it, with real Postgres/
// sandbox I/O around it) already establishes.
package identitylink
