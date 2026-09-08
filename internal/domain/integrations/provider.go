// Package integrations holds the pure, I/O-free read-model logic behind
// GET /api/integrations (§12.5's own "integrations read model & routes"
// amendment): which ingress/egress surfaces this deployment knows about,
// whether each one is fully configured, and how an outbox.kind string
// attributes to one of them. No I/O, no time.Now(), no randomness (§11)
// -- every input this package's own functions need (a config secret's
// already-loaded string value, an outbox row's already-fetched kind) is
// supplied by the caller (internal/adapters/inbound/httpapi/
// integrations.go), which is the one place in this codebase allowed to
// read platform.Config and touch Postgres.
package integrations

import "strings"

// Provider identifies one of the three ingress surfaces §12.5 names --
// Slack, Linear, GitHub. Deliberately just the three literal strings each
// provider's own adapter already writes into webhook_deliveries.provider
// (migrations/000027_webhook_deliveries.up.sql's own doc comment: "a
// short fixed string, e.g. 'github'/'slack'/'linear'") and that every
// outbox.kind this codebase defines is supposed to be PREFIXED with
// (internal/app/ports/notifier.go's own NotificationKind constants) --
// reusing the identical three strings for both purposes is deliberate:
// there is exactly one canonical spelling of each provider's name in this
// codebase, not two independently-maintained lists that could drift.
type Provider string

// The three known Provider values -- see Provider's own doc comment for
// why these three literal strings are the single canonical spelling
// reused across webhook_deliveries.provider and outbox.kind alike.
const (
	ProviderSlack  Provider = "slack"
	ProviderLinear Provider = "linear"
	ProviderGitHub Provider = "github"
)

// Providers is every known Provider, in a fixed, stable order -- GET
// /api/integrations renders its rows in this exact order, and
// ProviderForOutboxKind below checks prefixes in this exact order (moot
// today since "slack"/"linear"/"github" share no common prefix with each
// other, but kept explicit rather than ranging over a map, whose
// iteration order Go deliberately randomizes).
var Providers = []Provider{ProviderSlack, ProviderLinear, ProviderGitHub}

// OutboxKindPrefix is the literal prefix an outbox.kind must start with to
// count as posted to p -- the ONE definition of the convention, so the SQL
// that actually does the matching and the ProviderForOutboxKind model below
// cannot drift apart.
//
// That drift is not hypothetical: the matching runs as a LIKE in
// queries/outbox.sql, and ProviderForOutboxKind was unit-tested while nothing
// on the live path ever called it -- so its tests described production by
// resemblance rather than by exercising it. Routing the query's own argument
// through here puts the tested definition back on the live path; the
// integration test named for a convention-breaking kind exercises the SQL
// itself.
func OutboxKindPrefix(p Provider) string {
	return string(p)
}

// ProviderForOutboxKind maps kind (an outbox.kind value, e.g.
// "slack_digest", "linear_progress", "github_verdict", or a bare
// "slack"/"linear"/"github") to the Provider it was posted to, ok=false
// if kind matches none of them.
//
// This is a NAMING CONVENTION, not a constraint the outbox table itself
// enforces -- kind is free TEXT (migrations/000010_outbox.up.sql), and
// nothing stops a future NotificationKind from being added that does not
// begin with its own provider's literal name. When that happens, THIS
// function returns ok=false for it and GET /api/integrations' own row for
// that provider simply does not reflect that kind's own deliveries --
// silently, not a hard failure (§12.5's own explicit words: "a future
// kind that breaks it drops silently out of this read"). This is already
// true of several EXISTING kinds as of this package's own introduction --
// grepped against internal/app/ports/notifier.go: "sentinel_auto_fix"
// (posts to GitHub), "handoff_sentinel" (posts to GitHub),
// "release_manifest" (posts to GitHub), and "blob_delete" (not a
// provider-facing kind at all) all fail this prefix match today, despite
// the first three being genuine GitHub-directed outbound calls -- a real,
// pre-existing gap in the naming convention, not a hypothetical future
// one, and not this function's job to paper over with a second,
// hand-maintained kind->provider table (that table would itself become
// exactly the kind of silently-stale mapping this whole read model is
// warned against).
//
// Exported and unit-tested directly (provider_test.go) specifically so
// this fragility is provable in a test without needing a live outbox row
// -- see that test's own "kind that breaks the convention" case.
func ProviderForOutboxKind(kind string) (Provider, bool) {
	for _, p := range Providers {
		if strings.HasPrefix(kind, string(p)) {
			return p, true
		}
	}
	return "", false
}

// ParseProvider reports whether raw is exactly one of the three known
// Provider literal spellings ("slack"/"linear"/"github") -- the single
// canonical spelling Provider's own doc comment describes, reused here so
// platform.Load validates NARVI_INGRESS_ENABLED entries against the SAME
// vocabulary this package already defines, rather than a second,
// independently-typed switch. ok=false for anything else, including a
// near-miss typo ("slcak") -- platform.Load turns that into a loud boot
// failure (*platform.InvalidIngressEnabledError), never a silently-dropped
// entry: a typo'd surface name here is exactly the "indistinguishable from
// a deliberate disabling" failure mode this whole optionality design
// refuses, one layer up from a secret.
func ParseProvider(raw string) (Provider, bool) {
	p := Provider(raw)
	for _, known := range Providers {
		if p == known {
			return p, true
		}
	}
	return "", false
}

// ConfiguredSlack reports whether every value §8.10's Slack ingress
// adapter (internal/adapters/inbound/slack) needs to function is present:
// signingSecret (platform.Config.SlackSigningSecret, verifies
// "X-Slack-Signature" on every inbound webhook, fail-closed if absent)
// and botToken (platform.Config.SlackBotToken, the one direct
// chat.postMessage call the in-thread ack makes). Both are boot-required
// with no default WHEN THIS SURFACE IS ENABLED (platform.Config.
// IngressEnabled -- internal/platform/config.go's own
// slackSigningSecretEnvVarName/slackBotTokenEnvVarName doc comments); a
// deployment that never enables Slack ingress at all (§12.5's own
// ingress-optionality decision) leaves both empty by design, and this
// function correctly reports false for it -- ConfiguredSlack alone cannot
// tell "disabled" from "enabled but missing a secret" apart, which is why
// its caller (httpapi.configuredForProvider) ALSO checks IngressEnabled
// directly rather than relying on this predicate alone. This function
// still checks both explicitly, independent of that boot-time enforcement,
// since a caller here passes plain already-loaded
// strings, not platform.Config itself (§11: no I/O in domain -- Config is
// loaded by platform.Load() at boot, a concern this package stays
// independent of), and unit tests construct partially-empty inputs
// directly.
//
// Deliberately EXCLUDES SlackDefaultRepoName/SlackDefaultRepoURL -- both
// are optional, non-secret session-creation config (which repo a
// brand-new Slack-spawned session targets), not a credential the ingress
// surface itself needs to authenticate or operate at all; §12.5's
// "configured" is about the SURFACE, not about every optional knob it
// happens to also read.
func ConfiguredSlack(signingSecret, botToken string) bool {
	return signingSecret != "" && botToken != ""
}

// ConfiguredLinear is ConfiguredSlack's Linear sibling: webhookSecret
// (platform.Config.LinearWebhookSecret, verifies "Linear-Signature" on
// every inbound webhook), oauthClientID and oauthClientSecret
// (platform.Config.LinearOAuthClientID/LinearOAuthClientSecret, the
// workspace-installation OAuth2 app credentials §8.10's own adapter calls
// Linear's API with) -- all three boot-required together WHEN THIS SURFACE
// IS ENABLED (platform.Config.IngressEnabled, §12.5's own ingress-
// optionality decision; linearWebhookSecretEnvVarName's own doc comment
// group, internal/platform/config.go), so a genuinely working Linear
// ingress always has all three. A deployment that never enables Linear
// ingress leaves all three empty by design; see ConfiguredSlack's own doc
// comment for why this predicate alone cannot distinguish that from a
// half-configured enabled surface, and why its caller checks
// IngressEnabled separately.
//
// Deliberately EXCLUDES LinearDefaultRepoName/LinearDefaultRepoURL, for
// the identical reason ConfiguredSlack excludes their Slack counterparts
// above (session-creation config, not an ingress-surface credential).
func ConfiguredLinear(webhookSecret, oauthClientID, oauthClientSecret string) bool {
	return webhookSecret != "" && oauthClientID != "" && oauthClientSecret != ""
}

// ConfiguredGitHub is ConfiguredSlack/ConfiguredLinear's GitHub sibling:
// webhookSecret (platform.Config.GitHubWebhookSecret, verifies
// "X-Hub-Signature-256" on every inbound webhook), botHandle
// (platform.Config.GitHubBotHandle, the "@handle" substring §8.2's own
// mention-detection matches comment bodies against -- without it this
// ingress never detects a single mention, even though the value itself is
// a public username, not a secret), and botToken
// (platform.Config.GitHubBotToken, the credential §5.1's GitHub Notifier
// posts outbound verdicts/comments/preview-status with -- the OTHER
// direction §12.5's own lastOutboundAt/lastOutboundStatus reports on).
//
// Deliberately EXCLUDES GitHubClientID/GitHubClientSecret -- those
// authenticate a HUMAN signing into Narvi's own web UI via GitHub OAuth
// (§13.1), a completely separate feature from the GitHub ingress/egress
// surface §12.5 is asking about (internal/platform/config.go's own
// gitHubBotTokenEnvVarName doc comment draws this exact same line: "a
// webhook-originated ... session has ... no logged-in Narvi user's own
// token to reuse"). Also excludes GitHubReReviewLabel/
// GitHubReleaseLabel/GitHubReleaseBranchPattern/GitHubImageBuildToken --
// each individually optional with its own safe default or explicit
// "not configured" zero value, none of them gating whether this ingress
// surface can receive or send at all.
//
// All three are boot-required WHEN THIS SURFACE IS ENABLED
// (platform.Config.IngressEnabled, §12.5's own ingress-optionality
// decision) -- a deployment that never enables GitHub ingress leaves all
// three empty by design; see ConfiguredSlack's own doc comment for why
// this predicate alone cannot distinguish that from a half-configured
// enabled surface, and why its caller checks IngressEnabled separately.
func ConfiguredGitHub(webhookSecret, botHandle, botToken string) bool {
	return webhookSecret != "" && botHandle != "" && botToken != ""
}
