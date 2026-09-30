// Package githubapi holds the GitHub SourceControl port implementation
// (createPR, credential minting, push specs) — implemented at §9.3
// ("e2e happy path", §4.3). Credential minting and push specs are handled
// elsewhere (internal/adapters/inbound/httpapi's own scm-credentials
// endpoint mints the git-over-https credential; cmd/sandbox-agent's own
// HandlePush builds the push spec) -- this package's own real job is
// exactly ports.SourceControl.CreatePR: a real POST https://api.github.com/
// repos/{owner}/{repo}/pulls call.
//
// §8.5 ("image builds", §8.5-note/§10-P2) adds ResolveBranchSHA: a real
// GET https://api.github.com/repos/{owner}/{repo}/commits/{branch} call
// (first resolving the repo's own real default_branch via GET
// https://api.github.com/repos/{owner}/{repo} when no explicit branch is
// given -- "main"/"master" is never hardcoded). This is the concrete
// implementation of an already-made design decision: the control plane
// resolves each repo's own current SHA directly via the GitHub API, BEFORE
// assembling a spawn's CreateSpec, rather than waiting for a sandbox to
// report its own locally-discovered boot fingerprint back over the wire
// (no new wire message is added for this).
//
// Adapter accepts an apiBaseURL constructor parameter (defaulting to
// "https://api.github.com" in production wiring, cmd/control-plane/main.go)
// so tests can override it with a local httptest.Server standing in for
// GitHub's real API -- the SAME apiBaseURL-parameter precedent
// internal/adapters/inbound/auth's own NewCallbackHandler already
// established for testability.
//
// Auth is a GitHub OAuth user access token (Authorization: Bearer
// <token>), the SAME token design decision 8's scm-credentials endpoint
// already decrypted for this session's own user -- a separate, correct use
// of that token from the git-over-https "x-access-token"/password
// convention used for the PUSH itself (see cmd/sandbox-agent's own
// HandlePush): these are two distinct, correct conventions built on the
// SAME underlying OAuth token, never conflated here.
//
// §5.1 ("outbox delivery", §5.1) adds PostIssueComment (a real POST
// https://api.github.com/repos/{owner}/{repo}/issues/{pr_number}/comments
// call -- GitHub's Issues API, which a pull request is itself always
// addressable through) plus BotNotifier, a small sibling type (notifier.go)
// implementing ports.Notifier by calling PostIssueComment with a single,
// statically-configured bot credential (platform.Config.GitHubOutbound)
// baked in at construction time -- deliberately NOT Adapter itself: every
// existing Adapter method (CreatePR/ResolveBranchSHA/
// ResolveContractsFingerprint) is authenticated PER-CALL with a
// caller-supplied token (the session creator's own decrypted OAuth
// identity), which webhook-originated sessions simply don't have
// (sessions.created_by is NULL for them, migrations/000004_sessions.
// up.sql) -- BotNotifier wraps the SAME Adapter/doPost machinery with a
// bot token baked in once, rather than duplicating the HTTP plumbing a
// second time or threading a bot-token special case through Adapter's
// existing per-call-token methods.
//
// Batch fix/audit-github-pr-payload-correctness (H5 audit fix) adds
// GetPullRequest: a real GET https://api.github.com/repos/{owner}/{repo}/
// pulls/{pull_number} call, resolving a pull request's TRUE head branch/
// repo. internal/adapters/inbound/github's own webhook handler calls this
// for an "issue_comment" mention specifically (that event type's own
// payload never carries head.ref/head.repo directly, unlike
// "pull_request_review_comment" -- see that package's own headresolve.go),
// authenticated with the SAME bot credential BotNotifier already uses
// (platform.Config.GitHubOutbound) -- a GitHub webhook mention carries no
// per-commenter OAuth token the way CreatePR's caller already has one in
// hand, and reading a PR's own already-public head branch/repo needs no
// per-user identity at all.
package githubapi
