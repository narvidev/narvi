// Package githubapp implements §30.4's own lever: a GitHub App fine-grained
// read-only installation access token, minted fresh per credential
// request. This is the credential internal/adapters/inbound/httpapi.
// ScmCredentials hands to a shadow sandbox, a build boot and every pull
// request review session (read-only in every egress mode), instead of the
// write-capable creator OAuth token or bot token each of them used to
// receive -- a credential that physically cannot write is equally
// harmless in a process environment, a disk cache, a baked image, or a
// restored snapshot (§30.4's own words for why this shape was chosen over
// the smaller fine-grained-PAT stopgap the design also weighed).
//
// Client authenticates as the App itself (a short-lived RS256 JWT, signed
// with the App's own private key -- reusing internal/adapters/outbound/
// oidcsigning.Sign, this codebase's own existing hand-rolled RS256 JWT
// signer, rather than adding a new JWT dependency) to make exactly two
// kinds of GitHub REST calls: resolving a repo's own installation id, and
// minting that installation's own access token, explicitly narrowed to
// contents:read + metadata:read (GitHub allows a mint request to REQUEST a
// subset of an App's own granted permissions; it can never grant more than
// the App's installation was itself given). AppPermissions makes a third
// kind -- GET /app, the App's own maximum granted permissions -- for
// cmd/control-plane's own boot-time scope check (§30.4(4)).
//
// This package's own request/response shapes are this Step's invented,
// documented model of GitHub's real REST API (the same "invent, document,
// test against a fake httptest.Server" discipline internal/sandboxagent/
// credentials.CPClient's own top comment already establishes for the
// control-plane's own scm-credentials endpoint) -- there is no real GitHub
// App reachable from this environment to verify against, so every test in
// this package exercises the client against a fake server standing in for
// api.github.com. See this Step's own PR description for what that leaves
// unproven.
package githubapp
