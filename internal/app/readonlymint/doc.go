// Package readonlymint is §30.4's own self-validating installation-token
// mint: it mints a GitHub App installation access token (internal/
// adapters/outbound/githubapp.Client, scoped contents:read + metadata:read
// at request time) and, before ever handing that token back to a caller,
// introspects what GitHub actually granted (internal/domain/scmscope.
// ValidateReadOnly) -- §30.4(4)'s own "scope introspection, fail-closed,
// at boot and at mint," the mint half.
//
// A caller never sees a token this package has not already confirmed is
// read-only. A refusal is recorded into the shadow ledger with the SAME
// record-or-fail semantics the rest of that ledger already enforces
// (internal/app/shadowledger's own top doc comment) -- a refusal this
// package cannot evidence is a hard error, never a silent 403 a caller
// might be tempted to return anyway. So is a successful substitution
// (Mint). MintUnrecorded runs the same mint and the same scope check and
// records nothing, for a credential that is not a shadow-mode substitution
// at all -- a pull request's review session, read-only in every mode.
//
// This package does not decide WHEN a caller should mint a read-only
// credential instead of a write-capable one -- that decision (the
// server-side interception point, and the client-side forceReadOnly
// signal for a build boot) belongs to whichever call site consumes this
// package. This package's own job ends at "mint one, and never hand back
// one that isn't read-only."
package readonlymint
