// This file (config.go) implements typed config validated at boot,
// fail-fast, with named errors (§5.4, §11).

package platform

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/narvidev/narvi/internal/domain/integrations"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/rollout"
)

// Stage identifies which deployment stage the control plane is running in.
// Typed (rather than a bare string) so an invalid value fails fast at boot
// instead of silently propagating.
//
// Named Stage, not Environment: the technical plan already reserves
// "Environment" for a distinct, load-bearing domain entity (repo/automation
// config with path scoping and secrets — §14.1, §12.2 item 5, PR_PLAN row
// 10 "domain: Environment scoping"). Reusing that name here for an unrelated
// deploy-stage concept would collide with it once PR-10 lands.
type Stage string

// The only valid Stage values. Load rejects anything else.
const (
	StageDevelopment Stage = "development"
	StageStaging     Stage = "staging"
	StageProduction  Stage = "production"
)

// envVarName is the process environment variable Load reads to select
// Stage. Required in every stage -- no default (see Load: an unset value
// appends a *MissingRequiredEnvError, it is never silently treated as
// StageDevelopment). A production deploy that simply forgot to set this
// must fail to boot, not boot as if it were development -- WithAuthSessionCookie
// (authcookie.go) derives the Secure cookie attribute directly from
// cfg.Stage != StageDevelopment, so a silent development default would
// silently omit Secure on every auth cookie a misconfigured production
// deploy mints.
const envVarName = "NARVI_STAGE"

// InvalidStageError is returned by Load when NARVI_STAGE is set to a value
// that is not one of StageDevelopment, StageStaging, or StageProduction.
type InvalidStageError struct {
	Value string
}

func (e *InvalidStageError) Error() string {
	return fmt.Sprintf("invalid %s=%q: must be one of %q, %q, %q",
		envVarName, e.Value, StageDevelopment, StageStaging, StageProduction)
}

// logLevelEnvVarName is the process environment variable Load reads to
// select LogLevel (PR-03, §5.3).
const logLevelEnvVarName = "NARVI_LOG_LEVEL"

// defaultLogLevelValue is the NARVI_LOG_LEVEL value Load assumes when the
// variable is unset. Unlike NARVI_STAGE (required, no default -- see
// envVarName above), a missing log level has a genuinely safe fallback,
// so this one still defaults quietly.
const defaultLogLevelValue = "info"

// InvalidLogLevelError is returned by Load when NARVI_LOG_LEVEL is set to a
// value that is not (case-insensitively) one of debug, info, warn, or
// error. Named and structured the same way as InvalidStageError.
type InvalidLogLevelError struct {
	Value string
}

func (e *InvalidLogLevelError) Error() string {
	return fmt.Sprintf("invalid %s=%q: must be one of %q, %q, %q, %q (case-insensitive)",
		logLevelEnvVarName, e.Value, "debug", "info", "warn", "error")
}

// parseLogLevel validates raw (already defaulted to defaultLogLevelValue if
// the env var was unset) against the four accepted spellings, case-
// insensitively, and returns the corresponding slog.Level. Deliberately an
// explicit switch (not slog.Level.UnmarshalText) so only exactly these four
// names are accepted — UnmarshalText also accepts numeric offset suffixes
// like "error-8", which is more than this env var is specified to take.
func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, &InvalidLogLevelError{Value: raw}
	}
}

// databaseURLEnvVarName is the process environment variable Load reads for
// the Postgres DSN (PR-06, §1's stack-choices line: golang-migrate +
// pgx/v5). Required — no default, since there is no safe placeholder DSN.
const databaseURLEnvVarName = "NARVI_DATABASE_URL"

// httpAddrEnvVarName is the process environment variable Load reads for the
// control-plane HTTP listen address (PR-06). Optional: an unset/empty value
// defaults to defaultHTTPAddr.
const httpAddrEnvVarName = "NARVI_HTTP_ADDR"

// defaultHTTPAddr is the NARVI_HTTP_ADDR value Load assumes when the
// variable is unset -- a genuinely safe fallback, unlike NARVI_STAGE
// (required, no default -- see envVarName above).
const defaultHTTPAddr = ":8080"

// dbPoolMaxConnsEnvVarName is the process environment variable Load reads
// for the Postgres pool's MaxConns (adapters/outbound/postgres.
// NewPoolWithMaxConns, cmd/control-plane/main.go). Optional: an unset/empty
// value defaults to defaultDBPoolMaxConns.
//
// Exists because pgxpool's OWN default -- unset MaxConns resolves to
// max(4, runtime.NumCPU()), confirmed against the vendored
// github.com/jackc/pgx/v5@v5.10.0/pgxpool/pool.go source -- ties pool size
// to host core count, not to this control plane's actual concurrency
// needs. That default is a real, already-documented risk here, not a
// hypothetical one: internal/app/sessionactor's own hydrateAndAcquire
// (hydrate.go) pins ONE pool connection per live session Actor for that
// Actor's entire lifetime (holding a Postgres advisory lock), never
// released until ActorIdleTTL fires (30 min, §2) or the process shuts
// down -- and internal/adapters/inbound/github's own coalesce.go carries
// an identical warning ("this is NOT hypothetical") about the same
// small-fixed-default risk in a different call path. A small container
// (1-2 CPUs) left on the pgx default could exhaust its own pool once a
// handful of sessions are concurrently active, and Registry.
// hydrateAndAcquire's own pool.Acquire(ctx) call has no bounded timeout of
// its own -- it would then hang rather than fail fast, inheriting
// whichever caller ctx it was given (an HTTP request's, in the
// CreateSession/CreateTurn paths that call TriggerDispatch synchronously).
const dbPoolMaxConnsEnvVarName = "NARVI_DB_POOL_MAX_CONNS"

// defaultDBPoolMaxConns is the NARVI_DB_POOL_MAX_CONNS value Load assumes
// when the variable is unset -- a fixed, documented floor (not specified by
// the plan; chosen as comfortably larger than pgx's own CPU-tied default,
// and Postgres's own common max_connections=100 default leaves ample room
// for it), so a self-hosted deploy that never discovers this knob still
// gets a pool sized independently of host core count, matching
// dbPoolMaxConnsEnvVarName's own doc comment above for the full reasoning.
const defaultDBPoolMaxConns = 20

// hmacSandboxSecretEnvVarName, hmacBotsSecretEnvVarName, and
// hmacWebhookSecretEnvVarName are the three per-direction HMAC secret
// env vars §5.2 requires: "Separate secrets per direction (sandbox→CP,
// CP→bots, webhook ingress) so one rotation doesn't touch everything."
// All three are required in every stage, including development — Load
// never supplies a baked-in default value for any of them.
const (
	hmacSandboxSecretEnvVarName = "NARVI_HMAC_SANDBOX_SECRET"
	hmacBotsSecretEnvVarName    = "NARVI_HMAC_BOTS_SECRET"
	hmacWebhookSecretEnvVarName = "NARVI_HMAC_WEBHOOK_SECRET"
)

// MissingRequiredEnvError is returned by Load when a required environment
// variable that has no safe default (NARVI_DATABASE_URL today) is unset or
// empty.
type MissingRequiredEnvError struct {
	EnvVar string
}

func (e *MissingRequiredEnvError) Error() string {
	return fmt.Sprintf("missing required %s (no default)", e.EnvVar)
}

// InvalidDBPoolMaxConnsError is returned by Load when NARVI_DB_POOL_MAX_CONNS
// is set to a value that does not parse as a positive integer.
type InvalidDBPoolMaxConnsError struct {
	Value string
}

func (e *InvalidDBPoolMaxConnsError) Error() string {
	return fmt.Sprintf("invalid %s=%q: must be a positive integer", dbPoolMaxConnsEnvVarName, e.Value)
}

// InvalidHMACSecretError is returned by Load when one of the three
// direction-specific HMAC secret env vars (§5.2) is unset or empty. A
// distinct, named error (same pattern as InvalidStageError/
// InvalidLogLevelError above) so a misconfigured deploy names exactly
// which direction's secret is missing, never a single generic "config
// invalid" — and so that, per §5.2, no direction is ever silently
// defaulted.
type InvalidHMACSecretError struct {
	EnvVar string
}

func (e *InvalidHMACSecretError) Error() string {
	return fmt.Sprintf(
		"missing required %s: every stage (including development) must set its own HMAC secret — §5.2 requires separate secrets per direction, never a baked-in default",
		e.EnvVar,
	)
}

// gitHubClientIDEnvVarName, gitHubClientSecretEnvVarName, and
// publicBaseURLEnvVarName are the env vars Load reads for §13.1's
// ("auth v1", §13.1) GitHub OAuth wiring. All three are required in every
// stage — never defaulted, matching the 3 HMAC secrets' own "never a
// baked-in default" convention.
const (
	gitHubClientIDEnvVarName     = "NARVI_GITHUB_CLIENT_ID"
	gitHubClientSecretEnvVarName = "NARVI_GITHUB_CLIENT_SECRET"
	publicBaseURLEnvVarName      = "NARVI_PUBLIC_BASE_URL"
)

// ingressEnabledEnvVarName is the process environment variable Load reads
// for §12.5's own ingress-surface-optionality decision: an explicit,
// comma-separated list of which of the three ingress surfaces (Slack,
// Linear, GitHub -- internal/domain/integrations.Provider's own three
// literal spellings) this deployment mounts at all. A surface named here
// gets exactly today's pre-existing behavior: its own full credential set
// is required, and Load fails fast (MissingRequiredEnvError) if any of it
// is missing. A surface NOT named here is not mounted anywhere in
// controlplane/serve.go's own wiring -- no webhook route, no outbox
// notifier registered for any of its kinds -- and its own credential env
// vars are read but never enforced, mirroring objectStoreEndpointEnvVarName's
// own "endpoint absent, nothing else even inspected" gating rule one level
// up (a whole surface instead of one sub-config).
//
// Deliberately NEVER inferred from which secrets happen to be present --
// that would make a typo in one secret's own env var name indistinguishable
// from a deliberate decision to leave that surface unconfigured, exactly
// the fail direction this repository refuses everywhere else (see
// InvalidHMACSecretError's own doc comment for the same principle applied
// to a different var). An explicit list is the only way to tell those two
// apart.
//
// UNSET means every surface is enabled -- chosen deliberately over the
// alternative (unset means NONE enabled) after weighing both: every
// deployment that exists before this Step already configures all three
// surfaces' full credential sets, because Load already refused to boot
// otherwise -- so "unset = all enabled" reproduces that deployment's own
// existing behavior byte-for-byte, without it setting a single new env var,
// mirroring rolloutModeEnvVarName's own identical "must land as a no-op for
// every existing deployment" requirement. The rejected alternative --
// unset means none enabled -- fails in exactly the way this whole feature
// exists to prevent: it would silently unmount every ingress route on every
// one of those already-correctly-configured deployments the moment this
// binary was rebuilt, with no boot-time error to notice by (an unmounted
// route answers 404, it does not refuse to start) -- inferring a surface's
// intended state from this var's own absence, the identical failure mode
// §12.5's decision refuses for a secret's absence, just moved one layer up.
// A brand-new deployment that only wants GitHub still narrows scope
// explicitly (NARVI_INGRESS_ENABLED=github); simply forgetting to set this
// var at all reproduces today's existing "configure all three or refuse to
// boot" behavior -- an annoyance, never a silent loss.
//
// Explicitly SET to an empty string is different from unset, and is the
// one place in this file where that distinction matters enough to check
// for it (os.LookupEnv, not the usual os.Getenv-returns-"" idiom every
// other var in this file uses): a deployment that genuinely wants zero
// ingress surfaces (web-UI-only) has no other way to say so, since an
// empty comma-separated list and an absent one would otherwise collapse
// into the same "" Load can't tell apart. Setting the var to "" is a
// deliberate, explicit declaration of "no surfaces" -- consistent with
// this whole feature's own "explicit, never inferred" principle -- while
// never setting it at all keeps every existing deployment's own default.
const ingressEnabledEnvVarName = "NARVI_INGRESS_ENABLED"

// InvalidIngressEnabledError is returned by Load when NARVI_INGRESS_ENABLED
// carries an entry that is not one of internal/domain/integrations.
// Provider's three known spellings ("slack"/"linear"/"github"). Rejected
// rather than silently dropped: a typo'd surface name here (e.g. "slcak")
// must fail loudly -- the identical reasoning ingressEnabledEnvVarName's
// own doc comment gives for never inferring a surface's intended state
// from an absent secret applies just as much to an unrecognized entry in
// this list, one layer up.
type InvalidIngressEnabledError struct {
	Value string
}

func (e *InvalidIngressEnabledError) Error() string {
	return fmt.Sprintf(
		"invalid entry %q in %s: must be one of %q, %q, %q",
		e.Value, ingressEnabledEnvVarName,
		integrations.ProviderSlack, integrations.ProviderLinear, integrations.ProviderGitHub,
	)
}

// gitHubWebhookSecretEnvVarName and gitHubBotHandleEnvVarName configure
// §8.2's ("GitHub ingress", §8.2) own webhook adapter --
// internal/adapters/inbound/github. gitHubWebhookSecretEnvVarName is
// DELIBERATELY DISTINCT from hmacWebhookSecretEnvVarName above: that
// secret backs Narvi's OWN internal bearer HMAC scheme (a later, unrelated
// concern -- see internal/platform/webhooksig.go's own doc comment for
// the full reasoning), never a real third-party provider signature.
// GitHubWebhookSecret is the REAL secret GitHub itself signs
// "X-Hub-Signature-256" with, configured on GitHub's own webhook settings
// screen. gitHubBotHandleEnvVarName is the bot/app username §8.2's own
// mention-detection matches comment bodies against (a plain "@handle"
// substring check, internal/adapters/inbound/github). Both required --
// never defaulted, matching every other secret/credential this file
// already reads (the 3 HMAC secrets, the GitHub OAuth credentials,
// TokenEncryptionKey, Modal's own BaseURL/AuthToken) -- WHEN GitHub
// ingress is enabled (ingressEnabledEnvVarName's own doc comment, §12.5):
// there is no safe placeholder webhook secret, and a misconfigured/empty
// bot handle would silently make this entire ingress route never detect a
// single mention. A deployment that never enables GitHub ingress at all
// never has either checked, and both simply read as "" (Config.
// IngressEnabled gates the check, not this pair's own zero value).
const (
	gitHubWebhookSecretEnvVarName = "NARVI_GITHUB_WEBHOOK_SECRET"
	gitHubBotHandleEnvVarName     = "NARVI_GITHUB_BOT_HANDLE"
)

// gitHubBotTokenEnvVarName configures §5.1's ("outbox delivery", §5.1)
// own GitHub Notifier adapter (internal/adapters/outbound/githubapi's new
// issue-comment-posting method) -- read from NARVI_GITHUB_BOT_TOKEN.
// Required -- never defaulted, matching every other secret this file
// already reads -- WHEN GitHub ingress is enabled (ingressEnabledEnvVarName's
// own doc comment, §12.5): internal/domain/integrations.ConfiguredGitHub
// bundles this credential together with GitHubWebhookSecret/GitHubBotHandle
// as one surface (its own doc comment explains why -- posting a review
// verdict/comment back to GitHub is the OUTBOUND half of the same "GitHub
// ingress" surface the webhook adapter forms the INBOUND half of), so a
// deployment that never enables GitHub ingress leaves this empty too, and
// every GitHub-flavored outbox notifier this credential backs
// (controlplane/serve.go's own outboxNotifiers map) is simply never
// registered -- any stray row enqueued for one of those kinds anyway
// dead-letters through outboxworker's existing "no notifier registered for
// kind" path rather than posting with an empty credential. Deliberately a
// SEPARATE credential from every
// existing GitHub-flavored value in this struct: GitHubClientID/
// GitHubClientSecret authenticate the OAuth APP a human signs into Narvi
// through (§13.1); ports.SourceControl.CreatePR (githubapi.Adapter,
// already-existing) authenticates PER-CALL with the SESSION CREATOR's own
// decrypted OAuth access token (internal/adapters/outbound/postgres.
// IdentityStore) -- but a webhook-originated (GitHub/Slack/Linear) session
// has sessions.created_by left NULL (migrations/000004_sessions.up.sql),
// so there is no logged-in Narvi user's own token to reuse for posting an
// async turn-outcome comment back to a PR. GitHubBotToken is instead a
// single, statically-configured bot/app credential (a real GitHub personal
// access token or a GitHub App installation token, whichever the deploying
// operator provisions), baked into the GitHub Notifier adapter once at
// construction time (cmd/control-plane/main.go), never looked up per
// session the way CreatePR's own spec.Token is.
//
// Batch fix/audit-github-pr-payload-correctness (H5 audit fix) adds a
// SECOND consumer of this SAME credential: internal/adapters/inbound/
// github's own webhook handler authenticates its own GetPullRequest call
// (resolving an issue_comment mention's TRUE head branch/repo) with it too
// -- the same "no logged-in Narvi user's own token available" reasoning
// applies identically there (a GitHub webhook mention carries no
// per-commenter OAuth token either).
const gitHubBotTokenEnvVarName = "NARVI_GITHUB_BOT_TOKEN"

// gitHubImageBuildTokenEnvVarName configures §19.2's ("warm boot:
// refresh pump + hook policy", §19.2) own platform-level GitHub
// credential, read from NARVI_GITHUB_IMAGE_BUILD_TOKEN. §19.2's own
// words: "the freshness pump needs GitHub credentials belonging to no
// session creator... a shared image has no creator... the recommendation
// is a platform-level credential (GitHub App installation token) shared
// by the freshness pump and the build service."
//
// DELIBERATELY OPTIONAL -- unlike every other GitHub-flavored secret this
// file reads (GitHubClientID/Secret, GitHubWebhookSecret, GitHubBotToken,
// all required, never defaulted): this credential's own consumers (the
// freshness pump's per-repo tip-SHA resolution, app/imagebuild.Builder's
// claim-time SHA resolution for a repo-bearing row) are explicitly
// designed to degrade cleanly on its absence rather than treat a missing
// value as a boot-time configuration error: a missing/invalid credential
// or a GitHub API failure resolving a tip SHA is logged and recorded as a
// failed attempt via the SAME retry/backoff path any other resolution
// failure uses (imagebuild.Builder's resolveRepoSHAs/attempt) -- never a
// crash, and never something Load itself rejects at startup. A deploy
// that has not yet provisioned this credential simply never
// refreshes/builds a repo-bearing shared image -- every spawn still
// falls back to the base image exactly as it always has (§10 Phase 2's
// own "always fall back to base image on any miss" invariant,
// unaffected).
//
// How this differs from GitHubBotToken (see that field's own doc comment
// for its full reasoning): GitHubBotToken is a REQUIRED, already-used,
// operator-provisioned-once credential backing a real GitHub identity
// (posting PR comments as a bot, resolving a webhook mention's true PR)
// -- its own doc comment describes it as "a real GitHub personal access
// token or a GitHub App installation token, whichever the deploying
// operator provisions," never auto-refreshed, with no App-ID/private-key/
// installation-token-refresh plumbing anywhere in this codebase.
// GitHubImageBuildToken is a DIFFERENT, NEW, distinct credential for a
// completely different purpose (read-only tip-SHA resolution for a
// session-creator-independent background pump, not comment-posting or
// PR-lookup identity) -- reusing GitHubBotToken here would conflate two
// unrelated rotation/scoping boundaries the whole point of "separate
// secrets per direction" (§5.2) argues against, and would make this
// pump's own failure mode (a missing/invalid credential) impossible to
// diagnose independently of GitHubBotToken's own, unrelated consumers.
// Plain string, same shape as every other token this file reads (a real
// GitHub personal access token or a GitHub App installation token,
// whichever the deploying operator provisions) -- never logged anywhere.
const gitHubImageBuildTokenEnvVarName = "NARVI_GITHUB_IMAGE_BUILD_TOKEN"

// NARVI_CACHE_VOLUME_EPOCH (§19.1's attempt-2 rotation escape hatch)
// is GONE, deliberately, not merely unread: domain/imagebuild.
// CacheVolumeKey (this Step's third iteration -- immutable versioned
// cache snapshots) no longer takes an epoch argument at all. See that
// function's own doc comment for why an immutable-version model makes a
// separate rotation config surface redundant -- a bad published version
// is escaped by pointing a later build's own MountVersion resolution at
// an earlier, known-good one (an operator action against this control
// plane's own version-history bookkeeping), never by an operator
// redeploying with a new epoch value. Removed rather than left as a
// silently-ignored env var precisely so this codebase never carries two
// rotation mechanisms side by side, one live and one vestigial.

// reviewModelDeepEnvVarName configures §26.3's ("review triage:
// deterministic light/deep routing", §26.3) own deep-path model override,
// read from NARVI_REVIEW_MODEL_DEEP. §26.3 states "depth drives model/
// effort ... deep = frontier tier + high effort", but this codebase has
// no existing per-purpose model-tier config anywhere (grepped directly:
// the only comparable precedent, IntentClassifierModel above, configures
// the CLASSIFIER's own internal LLM call, an unrelated concern) -- so
// this is the ONE new config knob this Step adds. Deliberately OPTIONAL,
// unlike IntentClassifierModel (§8.3's own load-bearing, required
// feature): reviewtriage.ModelAndEffort's own doc comment (internal/
// domain/reviewtriage/modeleffort.go) explains why leaving this unset
// still forces high effort unconditionally on the deep path (safe on any
// model, no config needed) while simply leaving the model id itself
// unset (inheriting whatever this deployment's OpenCode-side default
// model already is, exactly like every review turn today) -- an operator
// who wants a genuinely different, more capable model on the deep path
// opts in by setting this; one who does not keeps booting with zero new
// required configuration.
const reviewModelDeepEnvVarName = "NARVI_REVIEW_MODEL_DEEP"

// gitHubReReviewLabelEnvVarName configures §8.2's ("review sessions",
// §8.2) own manual re-trigger-via-label lane (internal/adapters/inbound/
// github's new pull_request/"labeled" handling): the exact label NAME a
// maintainer applies to a PR to manually re-trigger its review session,
// reusing the SAME atomic per-PR claim/coalescing (github_pr_sessions,
// §8.2) an @mention already goes through -- never a NEW mechanism.
// DELIBERATELY OPTIONAL, unlike gitHubBotHandleEnvVarName/
// gitHubWebhookSecretEnvVarName above: this is a product/UX naming choice
// with a genuinely safe out-of-the-box default (defaultGitHubReReviewLabel
// below), not a secret or an identity a misconfiguration could silently
// corrupt -- an operator who never sets this still gets a working,
// sensibly-named re-trigger label, matching httpAddrEnvVarName's own
// "optional: an unset/empty value defaults" precedent rather than the
// three GitHub secrets/identity fields' own "never defaulted" one.
//
// Deliberately NOT reusing the `review:*` label PREFIX §8.2's own
// verdict-posting tool will later write (`review: needs-human`, §21.2) --
// this is a HUMAN-issued COMMAND label (§5.1's own distinction: "a human
// applying a label ... is a legitimate, deliberate command"), never a
// bot-written STATUS label the system reads back as its own memory (the
// SAME section's own warning against that). Sharing one prefix across both
// kinds would blur a distinction this codebase's own house style (§5.1)
// treats as load-bearing.
const gitHubReReviewLabelEnvVarName = "NARVI_GITHUB_REREVIEW_LABEL"

// defaultGitHubReReviewLabel is the gitHubReReviewLabelEnvVarName value
// Load assumes when the variable is unset -- a genuinely safe fallback,
// mirroring defaultHTTPAddr's own precedent (httpAddrEnvVarName's doc
// comment above). "run-review" is a verb phrase, deliberately distinct in
// SHAPE (not just prefix) from the noun-phrase `review: <state>` labels
// used elsewhere (gitHubReReviewLabelEnvVarName's own doc comment
// above) -- a human applying it reads unambiguously as "please run a
// review", never confusable with a bot-posted status.
const defaultGitHubReReviewLabel = "run-review"

// gitHubReleaseLabelEnvVarName and gitHubReleaseBranchPatternEnvVarName
// configure §15's own ("release PR review", §15.1) deterministic
// release-PR detection rule: "a PR is treated as a release review when
// it matches a configurable pattern: originates from/targets a
// release/* branch, or carries a release label." Both DELIBERATELY
// OPTIONAL, mirroring gitHubReReviewLabelEnvVarName's own identical
// "product/UX naming choice with a genuinely safe out-of-the-box
// default" precedent immediately above -- an operator who never sets
// either still gets a working, sensibly-named release-detection rule.
//
// A single, GLOBAL pair rather than a per-repo repo_settings row
// (unlike block_on_high_risk/sentinel_autofix_enabled): §15.1 itself
// gives no indication this needs to vary per repo the way those two
// admin-toggled POLICY flags do, and internal/platform/config.go's own
// gitHubReReviewLabelEnvVarName is the SAME kind of value (a naming/
// pattern convention, not a per-repo risk-tolerance decision) already
// configured this exact way -- adding a repo_settings column (and its
// own httpapi GET/PUT route extension) for a value with no described
// per-repo variation need would be speculative scope this Step's own
// brief does not ask for.
const gitHubReleaseLabelEnvVarName = "NARVI_GITHUB_RELEASE_LABEL"
const gitHubReleaseBranchPatternEnvVarName = "NARVI_GITHUB_RELEASE_BRANCH_PATTERN"

// defaultGitHubReleaseLabel/defaultGitHubReleaseBranchPattern are the
// values Load assumes when their own env vars are unset -- mirrors
// defaultGitHubReReviewLabel's own precedent immediately above.
// "release" and "release/*" are §15.1's own literal example values
// ("originates from/targets a release/* branch, or carries a release
// label").
const defaultGitHubReleaseLabel = "release"
const defaultGitHubReleaseBranchPattern = "release/*"

// tokenEncryptionKeyEnvVarName is the env var Load reads for the AES-256-GCM
// key protecting provider tokens at rest (§13.1: "Provider tokens encrypted
// at rest (AES-GCM), per-user"). Required in every stage; the raw value
// must base64-decode to exactly tokenEncryptionKeyByteLength bytes.
const tokenEncryptionKeyEnvVarName = "NARVI_TOKEN_ENCRYPTION_KEY"

// tokenEncryptionKeyByteLength is the exact decoded key length AES-256-GCM
// requires. A plain byte count, not a duration/interval, so (matching
// tokenhash.go's own wsTokenByteLength precedent) it is an ordinary Go
// constant rather than a platform.Timeouts field.
const tokenEncryptionKeyByteLength = 32

// InvalidTokenEncryptionKeyError is returned by Load when
// NARVI_TOKEN_ENCRYPTION_KEY fails either of its two checks: the raw value
// isn't valid base64, or it decodes to something other than exactly
// tokenEncryptionKeyByteLength bytes. Reason names which check failed —
// never a bare "invalid key".
type InvalidTokenEncryptionKeyError struct {
	Reason string
}

func (e *InvalidTokenEncryptionKeyError) Error() string {
	return fmt.Sprintf("invalid %s: %s", tokenEncryptionKeyEnvVarName, e.Reason)
}

// allowedEmailDomainsEnvVarName, allowedGitHubOrgsEnvVarName, and
// allowedEmailsEnvVarName are the 3 signup-allowlist mechanisms §13.1
// names ("allowlist of email domains / GitHub orgs / explicit users").
// Each is individually optional (a comma-separated list, parsed by
// parseCommaSeparatedList), but Load fails fast if ALL THREE are empty —
// see EmptyAllowlistError.
const (
	allowedEmailDomainsEnvVarName = "NARVI_ALLOWED_EMAIL_DOMAINS"
	allowedGitHubOrgsEnvVarName   = "NARVI_ALLOWED_GITHUB_ORGS"
	allowedEmailsEnvVarName       = "NARVI_ALLOWED_EMAILS"
)

// EmptyAllowlistError is returned by Load when NARVI_ALLOWED_EMAIL_DOMAINS,
// NARVI_ALLOWED_GITHUB_ORGS, and NARVI_ALLOWED_EMAILS are ALL empty. An
// allowlist that allows nobody by omission is a footgun this codebase's
// own "never a baked-in permissive default" convention (the 3 HMAC
// secrets already never have a default) argues against — the operator
// must configure at least one allowlist mechanism explicitly, in every
// stage including development.
type EmptyAllowlistError struct{}

func (e *EmptyAllowlistError) Error() string {
	return fmt.Sprintf(
		"at least one of %s, %s, %s must be set (§13.1's signup allowlist) — an allowlist that allows nobody by omission is not a safe default",
		allowedEmailDomainsEnvVarName, allowedGitHubOrgsEnvVarName, allowedEmailsEnvVarName,
	)
}

// modalBaseURLEnvVarName, modalAuthTokenEnvVarName, and
// modalEgressProxyURLEnvVarName configure the real
// internal/adapters/outbound/modal.Provider construction in cmd/
// control-plane/main.go (§9.3, "e2e happy path" -- this Step is the
// SandboxProvider's first real production caller). BaseURL/AuthToken are
// required in every stage, matching every other "never a baked-in
// default" secret this file already reads (the 3 HMAC secrets, the GitHub
// OAuth credentials) -- there genuinely is no safe placeholder Modal
// endpoint/token. EgressProxyURL is optional (§4.1: "the configurable
// egress proxy" is itself optional/fail-open at the modal package's own
// New constructor).
const (
	modalBaseURLEnvVarName        = "NARVI_MODAL_BASE_URL"
	modalAuthTokenEnvVarName      = "NARVI_MODAL_AUTH_TOKEN"
	modalEgressProxyURLEnvVarName = "NARVI_MODAL_EGRESS_PROXY_URL"
)

// rwxAccessTokenEnvVarName configures the real internal/adapters/outbound/
// rwx package's Dispatches API notifier construction in cmd/control-plane/
// main.go (§4.1.1/§4.1.2). Unlike modalAuthTokenEnvVarName above,
// this is OPTIONAL in every stage: RWX preview links are an off-by-default,
// per-repo opt-in feature layered ON TOP of this platform-wide credential
// (§4.1.2 point 1: "absent = feature off"; §24.5's posture) — a deployment
// that never turns RWX previews on for any repo has no reason to be forced
// to configure a real RWX account just to boot, unlike Modal (the actual
// sandbox-lifecycle provider every session depends on). When empty,
// cmd/control-plane/main.go does not construct the rwx Dispatches notifier
// (or its githubapi commit-status companion) at all — the two new outbox
// kinds (rwx_preview_dispatch/github_preview_link) simply have no notifier
// registered in that configuration, so any row enqueued for them (which
// requires a repo admin to have separately opted in — an operator
// misconfiguration, since the two are meant to be configured together)
// dead-letters with a clear, logged "no notifier registered for kind"
// error rather than silently vanishing.
const rwxAccessTokenEnvVarName = "NARVI_RWX_ACCESS_TOKEN"

// openCodeRuntimeVersionEnvVarName is the env var Load reads for §8.5's
// ("image builds", §8.5-note/§10-P2) own RuntimeVersion fingerprint input
// (domain/imagebuild.Fingerprint's third argument) -- the pinned OpenCode
// version this control plane assumes every base/prebuilt sandbox image
// carries. Optional: defaults to defaultOpenCodeRuntimeVersion.
//
// Residual drift risk, named honestly rather than silently accepted: this
// default is a SEPARATE literal from .github/workflows/ci.yml's own
// `opencode-ai@1.17.15` pin (confirmed by a repo-wide grep before this Step
// started: nothing today centralizes the OpenCode version pin as a single
// Go-level constant/config value that CI and this default could both read
// from) -- nothing mechanically keeps the two in sync, so a future version
// bump that updates ci.yml without ALSO updating either this default or a
// deploy's own NARVI_OPENCODE_RUNTIME_VERSION override would silently
// fingerprint every image against a stale runtime version. Wiring CI to
// read this same value (or vice versa) is a natural follow-up, not built
// here (this Step's own scope is the fingerprint mechanism, not a build-
// tooling refactor of an unrelated, already-merged CI workflow).
const openCodeRuntimeVersionEnvVarName = "NARVI_OPENCODE_RUNTIME_VERSION"

// defaultOpenCodeRuntimeVersion is the NARVI_OPENCODE_RUNTIME_VERSION value
// Load assumes when the variable is unset -- kept equal to ci.yml's own
// current pin at the time this Step was written; see
// openCodeRuntimeVersionEnvVarName's own doc comment for the drift risk
// this equality is not mechanically guaranteed to survive.
const defaultOpenCodeRuntimeVersion = "1.17.15"

// linearWebhookSecretEnvVarName, linearOAuthClientIDEnvVarName, and
// linearOAuthClientSecretEnvVarName are the env vars Load reads for
// §8.10's ("Linear ingress") Linear wiring. All three are required --
// never defaulted, matching every other "never a baked-in default" secret
// this file already reads -- WHEN Linear ingress is enabled
// (ingressEnabledEnvVarName's own doc comment, §12.5); a deployment that
// never enables it never has any of the three checked.
//
// linearWebhookSecretEnvVarName is deliberately a SEPARATE secret from
// hmacWebhookSecretEnvVarName above: that one backs platform.Sign/Verify's
// own internal "{timestamp}.{signature}" bearer format (see
// internal/platform/webhooksig.go's own doc comment), never a real
// provider's webhook signature. This one is Linear's own real webhook
// signing secret ("You can find the signing secret on the webhook's
// detail page" — confirmed against Linear's real, current developer docs
// during this Step's investigation), verified via
// platform.VerifyWebhookSignature against Linear's own real scheme: a
// hex-encoded HMAC-SHA256 of the raw request body, presented in the
// Linear-Signature header — no "sha256="-style prefix, unlike GitHub's.
//
// linearOAuthClientIDEnvVarName/linearOAuthClientSecretEnvVarName are a
// SEPARATE OAuth application from GitHubClientID/GitHubClientSecret above:
// that one is a human signing into Narvi's own web UI (§13.1); this one
// authorizes a Linear WORKSPACE installation (Linear's own OAuth2 flow,
// with the `actor=app` authorization-url parameter that "switches to an
// app installation" at workspace scope) so the control plane can call
// Linear's own API on that workspace's behalf — never a second way for a
// human to log into Narvi itself.
const (
	linearWebhookSecretEnvVarName     = "NARVI_LINEAR_WEBHOOK_SECRET"
	linearOAuthClientIDEnvVarName     = "NARVI_LINEAR_CLIENT_ID"
	linearOAuthClientSecretEnvVarName = "NARVI_LINEAR_CLIENT_SECRET"
)

// linearDefaultRepoNameEnvVarName and linearDefaultRepoURLEnvVarName name
// the single repo a Linear-originated session's Repos field is populated
// with (both required, no default, WHEN Linear ingress is enabled --
// ingressEnabledEnvVarName's own doc comment, §12.5; a deployment that
// never enables Linear ingress never has either checked, since no
// Linear-originated session can ever exist to need one). Scope note
// (§8.10): Linear's own
// AgentSessionEvent webhook payload carries no repository information at
// all (confirmed against Linear's real schema during this Step's
// investigation — an agent is expected to either already know its own
// candidate repos, via the SEPARATE issueRepositorySuggestions API, or be
// told out of band), and every CreateSessionRequest requires a non-empty
// Repos list (contracts/rest/v1/dtos.schema.json) regardless of ingress
// surface. §14/§8.4's own per-workspace/per-team repo mapping (Automations
// config) does not exist yet — building it is out of this Step's scope.
// Until that lands, every Linear-originated session targets this ONE
// operator-configured repo; a future Step naturally replaces this with a
// real per-team/per-workspace mapping once Automations config exists.
const (
	linearDefaultRepoNameEnvVarName = "NARVI_LINEAR_DEFAULT_REPO_NAME"
	linearDefaultRepoURLEnvVarName  = "NARVI_LINEAR_DEFAULT_REPO_URL"
)

// slackSigningSecretEnvVarName and slackBotTokenEnvVarName configure
// §8.10's ("Slack ingress") real Slack Events API adapter
// (internal/adapters/inbound/slack). Deliberately NOT HMACWebhookSecret --
// see internal/platform/webhooksig.go's own doc comment for why a real
// provider's own signature scheme never matches that internal bearer
// format. SlackSigningSecret verifies "X-Slack-Signature"/
// "X-Slack-Request-Timestamp" on every inbound webhook request (fail
// closed); SlackBotToken authenticates the one direct
// chat.postMessage call this Step's own in-thread ack makes (see that
// package's own doc.go for why this is a single direct API call, not the
// general Notifier/outbox abstraction (§5.1)). Both required -- never
// defaulted, matching every other secret this file already reads -- WHEN
// Slack ingress is enabled (ingressEnabledEnvVarName's own doc comment,
// §12.5); a deployment that never enables it never has either checked.
const (
	slackSigningSecretEnvVarName = "NARVI_SLACK_SIGNING_SECRET"
	slackBotTokenEnvVarName      = "NARVI_SLACK_BOT_TOKEN"
)

// slackDefaultRepoNameEnvVarName and slackDefaultRepoURLEnvVarName name the
// single repo a brand-new Slack-spawned session's CreateSessionRequest
// carries (§8.10, "Slack ingress"). Deliberately OPTIONAL, unlike the two
// vars above: the technical plan has no per-channel/per-workspace repo
// routing design yet (that is a genuinely open gap, left to a future
// automations/routing Step), so this Step's own honest, minimal stand-in is
// exactly one operator-configured default repo. Leaving either one unset
// disables NEW-thread session creation only (see internal/adapters/inbound/
// slack's own doc.go) -- it does not affect replies on an already-mapped
// thread, which never need a repo at all.
const (
	slackDefaultRepoNameEnvVarName = "NARVI_SLACK_DEFAULT_REPO_NAME"
	slackDefaultRepoURLEnvVarName  = "NARVI_SLACK_DEFAULT_REPO_URL"
)

// anthropicAPIKeyEnvVarName and intentClassifierModelEnvVarName configure
// §8.3's ("intent classifier", §8.3/§18) real Anthropic adapter
// (internal/adapters/outbound/llm), read from NARVI_ANTHROPIC_API_KEY /
// NARVI_INTENT_CLASSIFIER_MODEL. Both required in every stage -- never
// defaulted, matching every other secret/required-choice this file
// already reads. intentClassifierProviderEnvVarName names the SEPARATE,
// explicitly-validated provider dimension (§8's own "multi-provider by
// nature" requirement: Provider is never inferred from the model string's
// own naming convention) -- also required, no default, so a deploy that
// forgets to set it fails fast at boot rather than silently resolving to
// whichever provider happens to be registered first.
const (
	anthropicAPIKeyEnvVarName          = "NARVI_ANTHROPIC_API_KEY"
	intentClassifierProviderEnvVarName = "NARVI_INTENT_CLASSIFIER_PROVIDER"
	intentClassifierModelEnvVarName    = "NARVI_INTENT_CLASSIFIER_MODEL"
)

// intentClassifierActiveSurfacesEnvVarName is the env var Load reads for
// §18.5's permanent shadow-vs-active gate (see
// Config.IntentClassifierActiveSurfaces's own doc comment). Optional --
// an empty/unset value means every surface defaults to shadow mode.
const intentClassifierActiveSurfacesEnvVarName = "NARVI_INTENT_CLASSIFIER_ACTIVE_SURFACES"

// Model choice (the Makefile dev target's own example value:
// "claude-haiku-4-5", Claude Haiku 4.5 -- NOT applied as a silent Load()
// default; Provider/Model are both required, no default, per the const
// block above): a classification call run on EVERY session across EVERY
// ingress surface is a high-volume, cost- and latency-sensitive internal
// call, quite unlike a user-facing generative task (§18.1: "a fast, low-
// complexity, high-volume, latency-sensitive call, not a 'remotely
// complicated' reasoning task"). Haiku 4.5 is Anthropic's fastest/
// cheapest current-generation model with structured-output support --
// the right tier for a call this codebase's own SessionStore will end up
// making once per session, forever, rather than the biggest available
// model. An unrecognized/misconfigured NARVI_INTENT_CLASSIFIER_MODEL
// value maps to FallbackReasonUnsupportedProvider at classification time
// (never a silent substitution) -- see internal/adapters/outbound/llm's
// own model-recognition table.

// cloudIdentityIssuerURLEnvVarName is the env var Load reads for
// Config.CloudIdentityIssuerURL (§27.3). DELIBERATELY OPTIONAL --
// an empty value means the entire cloud-identity feature is off, fail-
// closed (see that field's own doc comment) -- but a NON-empty value gets
// real validation (InvalidCloudIdentityIssuerURLError below), unlike most
// of this file's other optional URL-shaped fields (e.g.
// SlackDefaultRepoURL is validated by a domain validator for a different
// reason -- routability, not URL shape): AWS/GCP/Azure STS fetch this
// value's own discovery/JWKS documents over the public internet with no
// Narvi credential at all (this Step's own gap-1 resolution, see
// httpapi/oidcdiscovery.go's own doc comment) -- a malformed value here
// would silently break every customer's federation attempt, cloud-side,
// with an error Narvi itself never observes, so Load catches the
// shape-level mistakes (missing scheme, missing host, a trailing slash
// that would double up against the fixed "/.well-known/..." suffixes)
// as early and as loudly as possible instead.
const cloudIdentityIssuerURLEnvVarName = "NARVI_CLOUD_IDENTITY_ISSUER_URL"

// InvalidCloudIdentityIssuerURLError is returned by Load when
// NARVI_CLOUD_IDENTITY_ISSUER_URL is set but is not a well-formed absolute
// http(s) URL with no path/query/fragment (the discovery/JWKS handlers
// append fixed "/.well-known/..." suffixes themselves -- see
// httpapi/oidcdiscovery.go -- so a value already carrying a path would
// produce a malformed, doubled-up URL no cloud's own STS could ever
// successfully fetch).
type InvalidCloudIdentityIssuerURLError struct {
	Value  string
	Reason string
}

func (e *InvalidCloudIdentityIssuerURLError) Error() string {
	return fmt.Sprintf("invalid %s=%q: %s", cloudIdentityIssuerURLEnvVarName, e.Value, e.Reason)
}

// otlpEndpointEnvVarName is the env var Load reads for Config.OTLPEndpoint
// (§33: "control-plane OTLP export"). Follows objectStoreEndpointEnvVarName's
// own "absent = feature off" precedent exactly, one field deep instead of a
// whole typed sub-config: an EMPTY value is not a boot-time error, it is the
// off switch -- Config.OTLPEndpoint stays "", platform.SetupOTel keeps
// building the stdouttrace/stdoutmetric exporters it always has, and every
// existing deployment that never sets this var is byte-identical to before
// §33 shipped. A NON-empty value gets real shape validation
// (InvalidOTLPEndpointError below, canonicalOTLPEndpointURL), mirroring
// cloudIdentityIssuerURLEnvVarName's own reasoning for the same choice: a
// malformed collector address would otherwise surface as a confusing runtime
// export failure (or, worse, a silently-never-exporting process) rather than
// a loud boot-time refusal.
const otlpEndpointEnvVarName = "NARVI_OTLP_ENDPOINT"

// licenseKeyEnvVarName is the env var Load reads for Config.LicenseKey
// (docs/design/boundaries-design.md, section 1.5) -- see that field's own doc
// comment for why, unlike every other credential-shaped field in this
// file, a non-empty value here gets NO validation at Load time: this
// value is opaque to platform entirely, and its own shape is
// internal/domain/license's own concern.
const licenseKeyEnvVarName = "NARVI_LICENSE_KEY"

// InvalidOTLPEndpointError is returned by Load when NARVI_OTLP_ENDPOINT is
// set but is not a well-formed absolute http(s) URL with no path/query/
// fragment -- platform.SetupOTel's own OTLP exporters each use their own
// fixed "/v1/traces"/"/v1/metrics" route as the DEFAULT for this SAME base
// URL (the OTel spec's own "general endpoint" behavior: the exporter's
// cleanPath step REPLACES whatever path it is given with that default, it
// does not append to it), so a value already carrying a path would not
// double up against that route -- it would silently override it and send
// telemetry somewhere the real collector is not listening.
type InvalidOTLPEndpointError struct {
	Value  string
	Reason string
}

func (e *InvalidOTLPEndpointError) Error() string {
	return fmt.Sprintf("invalid %s=%q: %s", otlpEndpointEnvVarName, e.Value, e.Reason)
}

// objectStoreEndpointEnvVarName, objectStorePublicEndpointEnvVarName,
// objectStoreRegionEnvVarName, objectStoreBucketEnvVarName,
// objectStoreAccessKeyIDEnvVarName, objectStoreSecretAccessKeyEnvVarName,
// and objectStoreUsePathStyleEnvVarName configure §8.6's ("uploads,
// blob storage & the in-sandbox download_file tool", §28.7) object-storage
// block: "The root storage credential exists in exactly one place:
// platform.Config ... endpoint, region, bucket, access key/secret (or
// ambient IAM where the deployment provides one), optional
// PublicEndpoint, path-style toggle for MinIO-style backends."
//
// FEATURE-FLAGGED on objectStoreEndpointEnvVarName alone (§28.7: "with no
// object-storage config present, the mint endpoints return a structured
// 'uploads not configured' error and nothing else degrades") -- unlike
// every required secret this file reads elsewhere, an EMPTY endpoint is
// not a boot-time error, it is the off switch: Config.ObjectStorage stays
// nil, and every other NARVI_OBJECT_STORE_* var below is read and
// validated ONLY once a non-empty endpoint turns the feature on (see
// Load's own object-storage block). This mirrors rwxAccessTokenEnvVarName's
// own "absent = feature off" precedent immediately above, one level
// deeper: RWX gates a single optional credential, this gates an entire
// typed sub-config whose OTHER fields (Region, Bucket) become required
// only conditionally.
//
// objectStoreAccessKeyIDEnvVarName/objectStoreSecretAccessKeyEnvVarName are
// BOTH optional together: leaving both empty selects the AWS SDK's own
// default credential chain (env vars, shared config files, or IMDS/IRSA)
// inside internal/adapters/outbound/objstore -- §28.7's "ambient IAM where
// the deployment provides one". Setting exactly one without the other is
// rejected (InvalidObjectStoreCredentialsError) as an almost-certain
// misconfiguration, never silently treated as "use ambient IAM anyway".
//
// objectStorePublicEndpointEnvVarName is optional: §28.7's "Presigning
// binds the host" -- when set, it is used to SIGN presigned URLs instead
// of objectStoreEndpointEnvVarName, so a deployment where the control
// plane reaches storage over an internal address but browsers/sandboxes
// must reach it over a different public one does not mint signatures that
// break the moment a client resolves the public host.
//
// objectStoreUsePathStyleEnvVarName is optional, default false (virtual-
// hosted-style addressing, AWS S3's own default) -- MinIO-style backends
// need this set true (path-style: bucket in the URL path).
const (
	objectStoreEndpointEnvVarName        = "NARVI_OBJECT_STORE_ENDPOINT"
	objectStorePublicEndpointEnvVarName  = "NARVI_OBJECT_STORE_PUBLIC_ENDPOINT"
	objectStoreRegionEnvVarName          = "NARVI_OBJECT_STORE_REGION"
	objectStoreBucketEnvVarName          = "NARVI_OBJECT_STORE_BUCKET"
	objectStoreAccessKeyIDEnvVarName     = "NARVI_OBJECT_STORE_ACCESS_KEY_ID"
	objectStoreSecretAccessKeyEnvVarName = "NARVI_OBJECT_STORE_SECRET_ACCESS_KEY"
	objectStoreUsePathStyleEnvVarName    = "NARVI_OBJECT_STORE_USE_PATH_STYLE"
)

// objectStoreMaxUploadBytesEnvVarName and
// objectStoreMaxSessionUploadBytesEnvVarName optionally override the two
// upload caps §28.4 names: "checks the declared size against
// MaxUploadBytes (propose 100 MiB, per-deployment config) and the
// session's running total against MaxSessionUploadBytes (propose 1 GiB --
// SUM(size_bytes) over the session's pending+ready uploads, derived from
// rows that already exist, never a dedicated counter column)". Both have
// safe defaults (defaultMaxUploadBytes/defaultMaxSessionUploadBytes below)
// and are read/validated regardless of whether object storage itself is
// configured -- they are plain per-deployment tuning knobs, not part of
// the credential/endpoint block that gates the feature on or off.
const (
	objectStoreMaxUploadBytesEnvVarName        = "NARVI_OBJECT_STORE_MAX_UPLOAD_BYTES"
	objectStoreMaxSessionUploadBytesEnvVarName = "NARVI_OBJECT_STORE_MAX_SESSION_UPLOAD_BYTES"
)

// defaultMaxUploadBytes and defaultMaxSessionUploadBytes are the byte-size
// defaults §28.4 proposes explicitly ("100 MiB" / "1 GiB"). Plain byte
// counts, not durations, so (matching tokenEncryptionKeyByteLength's own
// precedent immediately below) these are ordinary Go constants rather than
// platform.Timeouts fields -- §11's grep-test (no time.Duration literal
// outside platform/timeouts.go) does not apply to a byte count.
const (
	defaultMaxUploadBytes        int64 = 100 * 1024 * 1024  // §28.4, explicit ("propose 100 MiB")
	defaultMaxSessionUploadBytes int64 = 1024 * 1024 * 1024 // §28.4, explicit ("propose 1 GiB")
)

// InvalidObjectStoreCredentialsError is returned by Load when exactly one
// of NARVI_OBJECT_STORE_ACCESS_KEY_ID/NARVI_OBJECT_STORE_SECRET_ACCESS_KEY
// is set. The only two valid states are BOTH set (static credentials) or
// BOTH empty (ambient IAM, §28.7) -- exactly one being set is almost
// certainly a copy-paste/typo mistake, never a valid half-configured
// state, so this is rejected rather than silently falling back to ambient
// IAM with half a credential pair quietly ignored.
type InvalidObjectStoreCredentialsError struct{}

func (e *InvalidObjectStoreCredentialsError) Error() string {
	return fmt.Sprintf(
		"%s and %s must be set together or both left empty (both empty selects ambient IAM, §28.7) -- exactly one being set is almost certainly a misconfiguration",
		objectStoreAccessKeyIDEnvVarName, objectStoreSecretAccessKeyEnvVarName,
	)
}

// InvalidObjectStoreUsePathStyleError is returned by Load when
// NARVI_OBJECT_STORE_USE_PATH_STYLE is set to a value strconv.ParseBool
// does not recognize.
type InvalidObjectStoreUsePathStyleError struct {
	Value string
}

func (e *InvalidObjectStoreUsePathStyleError) Error() string {
	return fmt.Sprintf("invalid %s=%q: must be a boolean (true/false/1/0/T/F/...)", objectStoreUsePathStyleEnvVarName, e.Value)
}

// InvalidObjectStoreMaxBytesError is returned by Load when
// NARVI_OBJECT_STORE_MAX_UPLOAD_BYTES or
// NARVI_OBJECT_STORE_MAX_SESSION_UPLOAD_BYTES is set to a value that does
// not parse as a positive integer. EnvVar names which of the two failed --
// never a single generic "invalid byte limit".
type InvalidObjectStoreMaxBytesError struct {
	EnvVar string
	Value  string
}

func (e *InvalidObjectStoreMaxBytesError) Error() string {
	return fmt.Sprintf("invalid %s=%q: must be a positive integer (bytes)", e.EnvVar, e.Value)
}

// initialAdminEmailsEnvVarName is the env var Load reads for the
// first-run-seeding initial-admin list (§13.4: "initial admins set by
// config"). Optional — an empty list simply means every first-time
// sign-in defaults to role "member".
const initialAdminEmailsEnvVarName = "NARVI_INITIAL_ADMIN_EMAILS"

// epistemicCheckDefaultEnvVarName configures §20's ("builder
// epistemic pre-action check", §20.4) own platform-wide default for the
// devil's-advocate pre-action check on build turns, read from
// NARVI_EPISTEMIC_CHECK_DEFAULT. Optional, default false (§20.4: "Off by
// default") — mirrors objectStoreUsePathStyleEnvVarName's own optional-
// boolean-with-a-safe-default precedent exactly (Load's own object-storage
// block, below), just unconditional rather than gated behind a separate
// feature-on check: this default applies to every session regardless of
// deployment config, only ever overridden per-session by
// sessions.epistemic_check_enabled (migrations/000066_builder_epistemic_
// check.up.sql, internal/domain/turn.ResolveEpistemicCheckEnabled).
const epistemicCheckDefaultEnvVarName = "NARVI_EPISTEMIC_CHECK_DEFAULT"

// InvalidEpistemicCheckDefaultError is returned by Load when
// NARVI_EPISTEMIC_CHECK_DEFAULT is set to a value strconv.ParseBool does
// not recognize -- mirrors InvalidObjectStoreUsePathStyleError's own
// identical shape, one boolean env var over.
type InvalidEpistemicCheckDefaultError struct {
	Value string
}

func (e *InvalidEpistemicCheckDefaultError) Error() string {
	return fmt.Sprintf("invalid %s=%q: must be a boolean (true/false/1/0/T/F/...)", epistemicCheckDefaultEnvVarName, e.Value)
}

// shadowModeEnvVarName configures §30.8's own deployment-level master
// switch for platform shadow mode, read from NARVI_SHADOW_MODE. Optional,
// default false -- mirrors epistemicCheckDefaultEnvVarName's own
// "optional boolean, safe default" precedent immediately above. Unlike
// EVERY other boolean/enum switch in this file, true here is NOT a
// deployment's steady-state configuration: §30.8 reserves it for
// dedicated evaluation deployments, evaluated once at process start and
// never intended to change on a running fleet -- see Config.ShadowMode's
// own doc comment for the full "why", and docs/PRODUCTION_CHECKLIST.md
// for the operator-facing form of the same warning.
const shadowModeEnvVarName = "NARVI_SHADOW_MODE"

// InvalidShadowModeError is returned by Load when NARVI_SHADOW_MODE is
// set to a value strconv.ParseBool does not recognize -- mirrors
// InvalidEpistemicCheckDefaultError's own identical shape immediately
// above, one boolean env var over.
type InvalidShadowModeError struct {
	Value string
}

func (e *InvalidShadowModeError) Error() string {
	return fmt.Sprintf("invalid %s=%q: must be a boolean (true/false/1/0/T/F/...)", shadowModeEnvVarName, e.Value)
}

// RolloutMode is a type ALIAS (not a new, parallel type) for
// internal/domain/rollout.Mode -- Config.RolloutMode below is spelled
// platform.RolloutMode purely so every one of this Step's own call sites
// outside internal/platform (httpapi, sessionactor, the ingress adapters)
// can name it without importing internal/domain/rollout themselves just
// for a type reference; being a genuine alias (`=`, not a defined type)
// means platform.RolloutMode and rollout.Mode are the IDENTICAL type, so
// there is still exactly one enum in this codebase, never two that could
// drift out of sync (see rollout.Mode's own doc comment for the full
// "why one, not two" reasoning).
type RolloutMode = rollout.Mode

// RolloutModeOpen/RolloutModeCohort re-export rollout.ModeOpen/
// rollout.ModeCohort under this package's own name -- purely a
// convenience so a caller that already imports internal/platform (every
// adapter package in this codebase) can write platform.RolloutModeOpen
// without ALSO importing internal/domain/rollout just for a constant
// reference. Identical values either way (RolloutMode is a genuine type
// alias, above) -- these are not a second, independently-defined pair
// that could drift from rollout.Mode's own two constants.
const (
	RolloutModeOpen   = rollout.ModeOpen
	RolloutModeCohort = rollout.ModeCohort
)

// rolloutModeEnvVarName configures §10's ("feature-flagged cohort
// rollout of sessions, with documented rollback", §10 Phase 6, §32) own
// master switch, read from NARVI_ROLLOUT_MODE. Optional -- unlike
// NARVI_STAGE (required, no default: envVarName's own doc comment), an
// unset value here has a genuinely safe default (rollout.ModeOpen)
// mirroring epistemicCheckDefaultEnvVarName's own "optional, safe
// default" precedent immediately above, NOT envVarName's own
// "required, no default" one -- §32's own central requirement is that
// this Step lands as a byte-for-byte no-op for every existing deployment
// and for CI, which an unset-means-required-error default would violate
// outright.
const rolloutModeEnvVarName = "NARVI_ROLLOUT_MODE"

// InvalidRolloutModeError is returned by Load when NARVI_ROLLOUT_MODE is
// set to a value that is not (case-sensitively) one of rollout.ModeOpen
// ("open") or rollout.ModeCohort ("cohort") -- mirrors InvalidStageError's
// own identical "named, typed, fail-fast" shape (§5.4, §11): an invalid
// value refuses to boot rather than silently falling back to open (which
// would be indistinguishable from a deliberately-configured cohort
// deployment that simply mistyped its own env var, silently disabling
// the enrollment gate it thought it had armed).
type InvalidRolloutModeError struct {
	Value string
}

func (e *InvalidRolloutModeError) Error() string {
	return fmt.Sprintf("invalid %s=%q: must be one of %q, %q", rolloutModeEnvVarName, e.Value, rollout.ModeOpen, rollout.ModeCohort)
}

// parseCommaSeparatedList splits raw on commas, trims whitespace from each
// entry, and drops empty entries — used for every optional
// comma-separated-list env var this file reads (the 3 allowlist mechanisms
// plus InitialAdminEmails). An empty/unset raw value returns a nil slice,
// never a slice containing one empty string.
func parseCommaSeparatedList(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// canonicalCloudIdentityIssuerURL enforces the URL shape
// Config.CloudIdentityIssuerURL needs to be safely usable as the fixed
// prefix for GET {issuer}/.well-known/openid-configuration and GET
// {issuer}/.well-known/jwks.json (httpapi/oidcdiscovery.go), and returns
// the canonical "scheme://host" string Load assigns to that field: a
// well-formed absolute URL, http or https scheme, non-empty host, and NO
// path of its own -- not even a bare trailing slash. Mirrors
// gitHubRelease*'s own "validate the shape now, at boot, rather than let a
// malformed value surface as a confusing runtime failure" reasoning --
// see cloudIdentityIssuerURLEnvVarName's own doc comment for why THIS
// field, uniquely among this file's other optional URL-shaped values,
// earns real validation.
//
// Deliberately rejects url.Parse's own Path == "/" case -- NOT just
// Path == "" -- unlike an earlier version of this function. A trailing
// slash is a REAL path segment as far as plain string concatenation is
// concerned: httpapi/oidcdiscovery.go builds jwks_uri as
// issuerURL + "/.well-known/jwks.json" by direct concatenation, so
// "https://cp.example.com/" would silently produce
// "https://cp.example.com//.well-known/jwks.json" -- a double slash this
// codebase's own chi router (mounted with no CleanPath/StripSlashes
// middleware, cmd/control-plane/main.go) does NOT collapse, so that
// jwks_uri 404s on every real cloud STS fetch while Narvi's own discovery
// endpoint keeps reporting 200. Returning the canonicalized string (built
// from parsed.Scheme/parsed.Host alone, never the caller's own raw
// string) means the raw, unnormalized env value never reaches
// Config.CloudIdentityIssuerURL in the first place -- every downstream
// consumer (this discovery/JWKS concatenation AND the `iss` claim
// cloudidentitytoken.go mints verbatim) gets the already-canonical form
// with nothing left for either call site to remember to normalize itself.
func canonicalCloudIdentityIssuerURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", &InvalidCloudIdentityIssuerURLError{Value: raw, Reason: fmt.Sprintf("not a valid URL: %v", err)}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", &InvalidCloudIdentityIssuerURLError{Value: raw, Reason: "scheme must be http or https"}
	}
	// Hostname(), not Host -- see canonicalOTLPEndpointURL's own comment on
	// the same check: a port-only authority has a non-empty Host and an
	// empty Hostname(). An issuer URL naming no host is worse here than
	// there: it is published in the discovery document and becomes the
	// `iss` claim customer clouds match on.
	if parsed.Hostname() == "" {
		return "", &InvalidCloudIdentityIssuerURLError{Value: raw, Reason: "must include a host (a port-only value names no host)"}
	}
	if parsed.Path != "" {
		return "", &InvalidCloudIdentityIssuerURLError{Value: raw, Reason: "must not carry a path, including a bare trailing slash -- the discovery/JWKS handlers append their own fixed /.well-known/... suffix by plain string concatenation, so a trailing slash here would double up against it"}
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", &InvalidCloudIdentityIssuerURLError{Value: raw, Reason: "must not carry a query string or fragment"}
	}
	canonical := url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
	return canonical.String(), nil
}

// canonicalOTLPEndpointURL enforces the URL shape Config.OTLPEndpoint needs
// to be safely usable as the shared base URL platform.SetupOTel hands to
// BOTH otlptracehttp.WithEndpointURL and otlpmetrichttp.WithEndpointURL
// (§33): a well-formed absolute URL, http or https scheme (which also
// selects TLS vs. plaintext for the exporter's own HTTP client), non-empty
// host, and no path of its own -- not even a bare trailing slash. UNLIKE
// canonicalCloudIdentityIssuerURL's own neighboring reasoning (that
// function's own doc comment), this is not a concatenation hazard: each
// OTLP HTTP exporter's own cleanPath step REPLACES whatever path its base
// URL carries with its fixed "/v1/traces"/"/v1/metrics" default when none
// was supplied, it does not append to one that was (internal/oconf's own
// cleanPath -- see the inline comment on the Path-rejection check below
// for the full mechanism). So a value already carrying a path -- including
// a trailing slash, a real path segment as far as that logic is concerned
// -- would not double up against the exporter's own route; it would
// silently OVERRIDE it instead, and never reach a real collector's actual
// route either way. Rejecting it here is still correct; only the
// mechanism differs from canonicalCloudIdentityIssuerURL's own genuine
// concatenation case. Returns the canonicalized "scheme://host" string
// built from parsed.Scheme/parsed.Host alone, exactly like canonicalCloudIdentityIssuerURL,
// so the raw env value never reaches Config.OTLPEndpoint and no downstream
// caller needs to remember to normalize it itself.
func canonicalOTLPEndpointURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", &InvalidOTLPEndpointError{Value: raw, Reason: fmt.Sprintf("not a valid URL: %v", err)}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", &InvalidOTLPEndpointError{Value: raw, Reason: "scheme must be http or https"}
	}
	// Hostname(), not Host: url.Parse("http://:4318") yields Host==":4318"
	// with an EMPTY Hostname(), so a port-only authority satisfies a
	// non-empty-Host check while naming no host at all. WithEndpointURL then
	// hands ":4318" to the exporter as its endpoint and Go's transport
	// resolves the empty host to the local machine -- a value that passes
	// boot validation and silently exports to localhost instead of the
	// operator's collector, which is exactly the silently-never-exporting
	// process this validation exists to prevent.
	if parsed.Hostname() == "" {
		return "", &InvalidOTLPEndpointError{Value: raw, Reason: "must include a host (a port-only value like \"http://:4318\" names no host and would export to the local machine)"}
	}
	if parsed.Path != "" {
		// The exporters do NOT concatenate: internal/oconf's own cleanPath
		// REPLACES the signal path, using its default (/v1/traces,
		// /v1/metrics) only when none was supplied. So a path here does not
		// double up -- it silently overrides the signal route and sends
		// telemetry somewhere the collector is not listening. Rejecting it
		// is still right; the reason an operator reads must describe the
		// mechanism that actually applies.
		return "", &InvalidOTLPEndpointError{Value: raw, Reason: "must not carry a path, including a bare trailing slash -- each OTLP exporter uses its own fixed /v1/traces or /v1/metrics route, and a path supplied here REPLACES that route rather than being appended to it"}
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", &InvalidOTLPEndpointError{Value: raw, Reason: "must not carry a query string or fragment"}
	}
	canonical := url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
	return canonical.String(), nil
}

// gitHubAppIDEnvVarName and gitHubAppPrivateKeyEnvVarName configure §30.4's
// own GitHub App plumbing ("the other half of the same named debt": App id
// + private key in platform.Config, fail-fast validation) -- read from
// NARVI_GITHUB_APP_ID / NARVI_GITHUB_APP_PRIVATE_KEY. Both required in every
// stage -- never defaulted, matching every other "never a baked-in default"
// secret this file already reads (GitHubBotToken, TokenEncryptionKey, the 3
// HMAC secrets).
//
// Required rather than optional (unlike GitHubImageBuildToken, this file's
// one other GitHub-flavored credential with a genuine off switch): §30.8's
// own repo_settings.live_egress_enabled defaults to false for EVERY repo,
// promoted only by a future Step's explicit "Activate" gesture --
// §30.4's read-only App installation token is therefore not a
// dedicated-evaluation-deployment nicety, it is the credential every
// not-yet-promoted repo's sandbox needs merely to clone at all, on every
// deployment, starting the moment this Step ships (§30.4: "until this
// chantier, 'refuse to mint write' and 'refuse to mint at all' were the
// same thing... starving the mint breaks 'Narvi must actually run'"). A
// deployment that boots without this credential configured would silently
// regress every one of its not-yet-promoted repos back to that same
// all-or-nothing failure the instant internal/adapters/inbound/httpapi.
// ScmCredentials' own shadow branch (this Step) tries to substitute a
// credential it does not have.
const (
	gitHubAppIDEnvVarName         = "NARVI_GITHUB_APP_ID"
	gitHubAppPrivateKeyEnvVarName = "NARVI_GITHUB_APP_PRIVATE_KEY"
)

// InvalidGitHubAppIDError is returned by Load when NARVI_GITHUB_APP_ID is
// set to a value that does not parse as a positive integer -- mirrors
// InvalidDBPoolMaxConnsError's own identical "named, typed, positive-integer"
// shape.
type InvalidGitHubAppIDError struct {
	Value string
}

func (e *InvalidGitHubAppIDError) Error() string {
	return fmt.Sprintf("invalid %s=%q: must be a positive integer (the GitHub App's own numeric id)", gitHubAppIDEnvVarName, e.Value)
}

// InvalidGitHubAppPrivateKeyError is returned by Load when
// NARVI_GITHUB_APP_PRIVATE_KEY fails to decode into a usable RSA private
// key -- Reason names which of the two checks failed (not valid base64, or
// the decoded bytes are not a PEM-encoded RSA private key), mirroring
// InvalidTokenEncryptionKeyError's own identical two-stage "decode, then
// validate the decoded shape" reasoning.
type InvalidGitHubAppPrivateKeyError struct {
	Reason string
}

func (e *InvalidGitHubAppPrivateKeyError) Error() string {
	return fmt.Sprintf("invalid %s: %s", gitHubAppPrivateKeyEnvVarName, e.Reason)
}

// parseGitHubAppPrivateKey decodes raw (expected to be the App's own PEM
// private-key file, base64-encoded exactly like TokenEncryptionKey above --
// chosen for the identical reason: a multi-line PEM block survives a single
// env var, untouched by shell/.env-file newline handling, only when it is
// not carrying literal newlines itself) into a parsed *rsa.PrivateKey.
// GitHub issues App private keys as PKCS#1 ("BEGIN RSA PRIVATE KEY"); both
// PKCS#1 and PKCS#8 ("BEGIN PRIVATE KEY") are accepted so an operator who
// re-encodes the downloaded .pem into the more modern container is not
// penalized for it. The key is parsed once, here, at boot -- never
// re-decoded per mint call (internal/adapters/outbound/githubapp.Client
// takes the already-parsed value directly, mirroring TokenEncryptionKey's
// own "decoded once at Load time" discipline).
func parseGitHubAppPrivateKey(raw string) (*rsa.PrivateKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, &InvalidGitHubAppPrivateKeyError{Reason: fmt.Sprintf("not valid base64: %v", err)}
	}

	block, _ := pem.Decode(decoded)
	if block == nil {
		return nil, &InvalidGitHubAppPrivateKeyError{Reason: "base64 decoded, but the result is not a PEM block"}
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, &InvalidGitHubAppPrivateKeyError{Reason: fmt.Sprintf("PEM block is neither a PKCS#1 nor a PKCS#8 private key: %v", err)}
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, &InvalidGitHubAppPrivateKeyError{Reason: "PEM block decodes to a private key, but it is not RSA -- GitHub Apps are always issued RSA keys"}
	}
	return rsaKey, nil
}

// Config is the top-level, typed control-plane configuration, validated
// once at boot (§5.4, §11: "typed config validated at boot, fail-fast,
// named errors").
type Config struct {
	Stage    Stage
	Timeouts Timeouts

	// LogLevel is the minimum slog level the process logs at (PR-03, §5.3),
	// read from NARVI_LOG_LEVEL (debug/info/warn/error, case-insensitive,
	// default "info").
	LogLevel slog.Level

	// DatabaseURL is the Postgres DSN used by
	// adapters/outbound/postgres.NewPool and the boot-time migration run
	// (PR-06), read from NARVI_DATABASE_URL. Required; Load does not
	// validate that the DSN itself parses beyond non-empty — pgxpool.New
	// surfaces a real connection error at boot if it's malformed.
	DatabaseURL string

	// HTTPAddr is the address `narvi serve` listens on (PR-06), read from
	// NARVI_HTTP_ADDR. Optional: defaults to ":8080".
	HTTPAddr string

	// DBPoolMaxConns overrides the Postgres pool's MaxConns (adapters/
	// outbound/postgres.NewPoolWithMaxConns), read from
	// NARVI_DB_POOL_MAX_CONNS. Optional: defaults to defaultDBPoolMaxConns
	// -- see dbPoolMaxConnsEnvVarName's own doc comment above for why this
	// is deliberately NOT left to pgxpool's own CPU-tied default.
	DBPoolMaxConns int32

	// IngressEnabled is §12.5's own ingress-surface-optionality decision,
	// read from NARVI_INGRESS_ENABLED (ingressEnabledEnvVarName's own doc
	// comment for the full "why explicit, why this default" reasoning). A
	// map rather than three booleans so callers can key it by
	// internal/domain/integrations.Provider directly, the SAME type/
	// vocabulary GET /api/integrations already uses (httpapi.
	// GetIntegrations) -- there is exactly one notion of "which surfaces
	// exist", never a second, independently-spelled one.
	//
	// A surface absent from this map (Go's own zero value for a missing
	// key: false) is not mounted anywhere in controlplane/serve.go's own
	// wiring -- no webhook route, no outbox notifier registered for any of
	// its kinds -- and Load never enforces that surface's own credential
	// env vars. A surface present with value true has its own full
	// credential set enforced exactly as every ingress surface always has
	// been (a *MissingRequiredEnvError on anything missing). Load never
	// stores an explicit false entry -- checking IngressEnabled[p] on a
	// nil/missing key already reads false, so there is only ever one
	// representation of "not enabled", never a second one worth telling
	// apart from it.
	IngressEnabled map[integrations.Provider]bool

	// HMACSandboxSecret, HMACBotsSecret, and HMACWebhookSecret are the
	// three direction-specific secrets §5.2 requires ("Separate secrets
	// per direction (sandbox→CP, CP→bots, webhook ingress) so one
	// rotation doesn't touch everything"), read from
	// NARVI_HMAC_SANDBOX_SECRET, NARVI_HMAC_BOTS_SECRET, and
	// NARVI_HMAC_WEBHOOK_SECRET respectively. All three are required in
	// every stage, including development — never defaulted.
	//
	// HMACWebhookSecret note (§5.1, "webhook toolkit"): this secret
	// pairs with platform.Sign/Verify's own internal "{timestamp}.
	// {signature}" bearer format (hmacauth.go) -- it is NOT the secret
	// GitHub/Slack/Linear ingress adapters (§8.2/§8.10) use to verify
	// their OWN provider's webhook signature (a real provider signature
	// never matches this bearer format at all; see
	// internal/platform/webhooksig.go's own doc comment for the full
	// reasoning and each provider's own scheme). Each of those adapters
	// reads its own, separate, provider-specific secret instead.
	HMACSandboxSecret string
	HMACBotsSecret    string
	HMACWebhookSecret string

	// GitHubClientID and GitHubClientSecret are the GitHub OAuth App
	// credentials (§13.1: "GitHub OAuth is the primary login"), read from
	// NARVI_GITHUB_CLIENT_ID / NARVI_GITHUB_CLIENT_SECRET. Both required in
	// every stage — never defaulted. GitHubClientSecret is never logged
	// anywhere (see internal/adapters/inbound/auth's own security notes).
	GitHubClientID     string
	GitHubClientSecret string

	// GitHubWebhookSecret and GitHubBotHandle configure §8.2's
	// ("GitHub ingress", §8.2) webhook adapter, read from
	// NARVI_GITHUB_WEBHOOK_SECRET / NARVI_GITHUB_BOT_HANDLE. Both required
	// in every stage -- never defaulted. See gitHubWebhookSecretEnvVarName's
	// own doc comment above for why GitHubWebhookSecret is a DISTINCT
	// secret from HMACWebhookSecret.
	GitHubWebhookSecret string
	GitHubBotHandle     string

	// GitHubReReviewLabel is §8.2's ("review sessions", §8.2) own manual
	// re-trigger-via-label lane's configured label NAME, read from
	// NARVI_GITHUB_REREVIEW_LABEL. Deliberately OPTIONAL, unlike
	// GitHubWebhookSecret/GitHubBotHandle immediately above -- see
	// gitHubReReviewLabelEnvVarName's own doc comment for why a safe
	// default (defaultGitHubReReviewLabel) exists here at all.
	GitHubReReviewLabel string

	// GitHubReleaseLabel/GitHubReleaseBranchPattern are §15's own
	// ("release PR review", §15.1) deterministic release-PR detection
	// configuration, read from NARVI_GITHUB_RELEASE_LABEL/
	// NARVI_GITHUB_RELEASE_BRANCH_PATTERN. Both deliberately OPTIONAL --
	// see gitHubReleaseLabelEnvVarName's own doc comment for the safe
	// defaults (defaultGitHubReleaseLabel/defaultGitHubReleaseBranchPattern).
	GitHubReleaseLabel         string
	GitHubReleaseBranchPattern string

	// GitHubBotToken configures §5.1's ("outbox delivery", §5.1) own
	// GitHub Notifier adapter, read from NARVI_GITHUB_BOT_TOKEN. Required
	// in every stage -- never defaulted. See gitHubBotTokenEnvVarName's own
	// doc comment above for why this is a distinct credential from every
	// other GitHub-flavored value in this struct -- and, since batch
	// fix/audit-github-pr-payload-correctness (H5 audit fix), also for
	// GitHub ingress's own GetPullRequest call (cmd/control-plane/main.go's
	// githubingress.Config.BotToken). Never logged.
	GitHubBotToken string

	// GitHubImageBuildToken is §19.2's ("warm boot: refresh pump + hook
	// policy", §19.2) own platform-level GitHub credential, read from
	// NARVI_GITHUB_IMAGE_BUILD_TOKEN. Empty string means "not configured" --
	// see gitHubImageBuildTokenEnvVarName's own doc comment for why this,
	// uniquely among this struct's GitHub-flavored fields, is deliberately
	// OPTIONAL and how it differs from GitHubBotToken. Never logged.
	GitHubImageBuildToken string

	// ReviewModelDeep is §26.3's own optional deep-path model override,
	// read from NARVI_REVIEW_MODEL_DEEP -- empty string means "not
	// configured" (see reviewModelDeepEnvVarName's own doc comment for
	// the full "why this is optional and how the deep path degrades when
	// it is unset").
	ReviewModelDeep string

	// PublicBaseURL is this control plane's own externally-reachable base
	// URL (e.g. "http://localhost:8080" in development, a real https://
	// URL in production), read from NARVI_PUBLIC_BASE_URL. Required — used
	// to construct the OAuth RedirectURL as PublicBaseURL +
	// "/auth/github/callback" (internal/adapters/inbound/auth.
	// NewGitHubOAuthConfig). Not validated as a well-formed URL beyond
	// non-empty, matching DatabaseURL's own precedent above.
	PublicBaseURL string

	// TokenEncryptionKey is the already-decoded, exactly-32-byte AES-256-GCM
	// key protecting provider tokens at rest (§13.1), read from
	// NARVI_TOKEN_ENCRYPTION_KEY and base64-decoded + length-validated once
	// here at Load() time — never re-decoded per call (internal/platform.
	// EncryptToken/DecryptToken take this value directly).
	TokenEncryptionKey []byte

	// AllowedEmailDomains, AllowedGitHubOrgs, and AllowedEmails are the 3
	// signup-allowlist mechanisms §13.1 names, each parsed from a
	// comma-separated env var (NARVI_ALLOWED_EMAIL_DOMAINS /
	// NARVI_ALLOWED_GITHUB_ORGS / NARVI_ALLOWED_EMAILS), trimmed, with
	// empty entries dropped. Each is individually optional, but Load fails
	// fast if all three end up empty (see EmptyAllowlistError).
	AllowedEmailDomains []string
	AllowedGitHubOrgs   []string
	AllowedEmails       []string

	// InitialAdminEmails is the first-run-seeding initial-admin list
	// (§13.4: "initial admins set by config"), parsed the same
	// comma-separated way from NARVI_INITIAL_ADMIN_EMAILS. Optional — a
	// verified sign-in email found here gets role "admin" at creation
	// time instead of the enum's own "member" default.
	InitialAdminEmails []string

	// EpistemicCheckDefault is §20's ("builder epistemic pre-action
	// check", §20.4) own platform-wide default for the devil's-advocate
	// pre-action check on build turns, read from
	// NARVI_EPISTEMIC_CHECK_DEFAULT. Optional: defaults to false (§20.4:
	// "Off by default"). A session's own sessions.epistemic_check_enabled
	// override, when set, always wins over this value regardless of
	// direction (internal/domain/turn.ResolveEpistemicCheckEnabled) — this
	// field is consulted only when that override is unset (NULL).
	EpistemicCheckDefault bool

	// RolloutMode is §10's own master switch (§10 Phase 6, §32:
	// "feature-flagged cohort rollout of sessions"), read from
	// NARVI_ROLLOUT_MODE. Optional: defaults to rollout.ModeOpen when
	// unset (§32: "so this Step lands as a byte-for-byte no-op for every
	// existing deployment and for CI"). In rollout.ModeCohort,
	// httpapi.CreateSessionOnTx's own per-repo repo_settings.
	// sessions_enabled read (on the same transaction as the session
	// insert) and internal/app/sessionactor's own dispatch-time re-check
	// both gate on this value -- see internal/domain/rollout's own doc
	// comment for the shared pure decision both sites call.
	RolloutMode rollout.Mode

	// ShadowMode is §30.8's own deployment-level master switch for
	// platform shadow mode (§30, "zero-trace evaluation on live customer
	// repositories"), read from NARVI_SHADOW_MODE. Optional: defaults to
	// false when unset, matching EpistemicCheckDefault's own "off by
	// default" shape immediately above.
	//
	// Effective per-repo egress mode is "ShadowMode OR NOT
	// repo_settings.live_egress_enabled" (internal/app/egressmode.
	// Resolve, the ONE place that formula is evaluated) -- true here
	// forces EVERY repo shadow for the whole process regardless of what
	// any individual repo_settings row says, exactly like RolloutMode's
	// own "master switch overrides the per-repo flag" shape above, one
	// section over.
	//
	// The sharp edge, named because an operator WILL meet it: this
	// switch is per-PROCESS, and the fleet is multi-pod (the same reason
	// the outbox worker's own claim uses `FOR UPDATE SKIP LOCKED` rather
	// than trusting a single process). Changing this value with a
	// ROLLING restart produces a mixed fleet in which a pod that has
	// already picked up the new value coexists, for the whole rollout
	// window, with a pod still running the old one -- and if the change
	// is shadow-to-live, that still-live pod really delivers outbox rows
	// a shadow pod enqueued (§30.8's own "born-shadow is terminally
	// shadow" epoch stamp closes THAT specific race once the outbox's own
	// enqueue-time stamp lands, but nothing about a bare env var flip is
	// safe to assume closes every other one). This switch is therefore
	// reserved for DEDICATED EVALUATION deployments stood up
	// already-configured -- never a value flipped on an existing running
	// fleet. The per-repo Postgres
	// flag (repo_settings.live_egress_enabled) remains the transactional
	// authority every pod in a real fleet actually shares; see
	// docs/PRODUCTION_CHECKLIST.md for the operator-facing checklist
	// item this backs.
	ShadowMode bool

	// GitHubAppID and GitHubAppPrivateKey are §30.4's own GitHub App
	// plumbing, read from NARVI_GITHUB_APP_ID / NARVI_GITHUB_APP_PRIVATE_KEY
	// (both required in every stage -- see gitHubAppIDEnvVarName's own doc
	// comment for why this is required rather than optional). Together
	// they let internal/adapters/outbound/githubapp.Client authenticate AS
	// THE APP (a short-lived, RS256-signed JWT) to mint fine-grained
	// read-only installation access tokens (contents:read + metadata:read)
	// -- the credential internal/adapters/inbound/httpapi.ScmCredentials'
	// own shadow-substitution branch hands to a shadow sandbox instead of
	// the write-capable creator OAuth token or bot token every session
	// used to receive regardless of egress mode.
	//
	// GitHubAppPrivateKey is the already-decoded, already-parsed RSA
	// private key -- decoded and parsed exactly once, here, at Load()
	// time, mirroring TokenEncryptionKey's own "never re-decoded per call"
	// discipline immediately above. Never logged anywhere.
	GitHubAppID         int64
	GitHubAppPrivateKey *rsa.PrivateKey

	// ModalBaseURL and ModalAuthToken configure the real
	// internal/adapters/outbound/modal.Provider cmd/control-plane/main.go
	// constructs (§9.3, "e2e happy path"), read from
	// NARVI_MODAL_BASE_URL / NARVI_MODAL_AUTH_TOKEN. Both required in
	// every stage — never defaulted (there is no real Modal account
	// reachable from this codebase's own tests/CI, see
	// internal/adapters/outbound/modal/doc.go; a real value must be
	// supplied by whoever deploys this binary against an actual Modal
	// account, or a mock standing in for one, e.g. §14.3's own future
	// Prism-based mock server).
	ModalBaseURL   string
	ModalAuthToken string

	// ModalEgressProxyURL optionally routes all Modal traffic through an
	// egress proxy (§4.1), read from NARVI_MODAL_EGRESS_PROXY_URL. Empty
	// (the default) means a direct connection.
	ModalEgressProxyURL string

	// RWXAccessToken optionally configures the real internal/adapters/
	// outbound/rwx package's Dispatches API notifier (§4.1.1/
	// §4.1.2), read from NARVI_RWX_ACCESS_TOKEN. See that env var's own
	// doc comment above for why this is optional, unlike ModalAuthToken.
	RWXAccessToken string

	// OpenCodeRuntimeVersion is §8.5's ("image builds") own
	// RuntimeVersion fingerprint input, read from
	// NARVI_OPENCODE_RUNTIME_VERSION. Optional: defaults to
	// defaultOpenCodeRuntimeVersion (see that constant's own doc comment
	// for the residual drift risk against .github/workflows/ci.yml's own
	// separate pin).
	OpenCodeRuntimeVersion string

	// LinearWebhookSecret, LinearOAuthClientID, and LinearOAuthClientSecret
	// are §8.10's ("Linear ingress", §8.10) own Linear wiring, read from
	// NARVI_LINEAR_WEBHOOK_SECRET / NARVI_LINEAR_CLIENT_ID /
	// NARVI_LINEAR_CLIENT_SECRET respectively — see those env var names'
	// own doc comments above for why each is a separate secret from its
	// same-shaped-sounding GitHub/HMAC counterpart. All three required in
	// every stage — never defaulted.
	LinearWebhookSecret     string
	LinearOAuthClientID     string
	LinearOAuthClientSecret string

	// LinearDefaultRepoName and LinearDefaultRepoURL are the single repo
	// every Linear-originated session's Repos field is populated with —
	// see linearDefaultRepoNameEnvVarName's own doc comment for the scope
	// note this stopgap exists under. Both required, no default.
	LinearDefaultRepoName string
	LinearDefaultRepoURL  string

	// SlackSigningSecret and SlackBotToken configure §8.10's ("Slack
	// ingress") real Slack Events API adapter, read from
	// NARVI_SLACK_SIGNING_SECRET / NARVI_SLACK_BOT_TOKEN. Both required in
	// every stage -- never defaulted (see slackSigningSecretEnvVarName's
	// own doc comment above for the full reasoning).
	SlackSigningSecret string
	SlackBotToken      string

	// SlackDefaultRepoName and SlackDefaultRepoURL are the single repo a
	// brand-new Slack-spawned session's CreateSessionRequest carries,
	// read from NARVI_SLACK_DEFAULT_REPO_NAME / NARVI_SLACK_DEFAULT_REPO_URL.
	// Both optional -- see slackDefaultRepoNameEnvVarName's own doc
	// comment above for why, and for what leaving either one empty means.
	SlackDefaultRepoName string
	SlackDefaultRepoURL  string

	// AnthropicAPIKey, IntentClassifierProvider, and IntentClassifierModel
	// configure §8.3's ("intent classifier", §8.3/§18) real Anthropic
	// adapter, read from NARVI_ANTHROPIC_API_KEY /
	// NARVI_INTENT_CLASSIFIER_PROVIDER / NARVI_INTENT_CLASSIFIER_MODEL. All
	// three required in every stage -- never defaulted. See
	// anthropicAPIKeyEnvVarName's own doc comment above for the model
	// choice's full reasoning. AnthropicAPIKey is never logged anywhere.
	AnthropicAPIKey          string
	IntentClassifierProvider string
	IntentClassifierModel    string

	// IntentClassifierActiveSurfaces is the permanent shadow-vs-active
	// gating config §18.5/§9.4 requires: "shadow mode ... permanently
	// available, never a one-time launch gate". Parsed the same comma-
	// separated way as AllowedEmailDomains/AllowedGitHubOrgs/AllowedEmails
	// above, from NARVI_INTENT_CLASSIFIER_ACTIVE_SURFACES -- each entry
	// one of sessions.spawn_source's own values ("web", "slack", "linear",
	// "github"). A surface NOT listed here defaults to shadow mode (§18.5:
	// "never silently flip a surface to active without an explicit config
	// value saying so") -- optional, an empty/unset value means every
	// surface runs in shadow.
	IntentClassifierActiveSurfaces []string

	// CloudIdentityIssuerURL (§27.3) is the public, externally
	// reachable base URL the control plane's own OIDC issuer serves
	// discovery/JWKS from (GET {CloudIdentityIssuerURL}/.well-known/
	// openid-configuration, GET {CloudIdentityIssuerURL}/.well-known/
	// jwks.json), read from NARVI_CLOUD_IDENTITY_ISSUER_URL --
	// deliberately a SEPARATE field from PublicBaseURL above, not a reuse
	// of it: §27.3 gives this capability its own fail-closed gate ("the
	// whole capability is off, and binding CRUD refuses, when unset"),
	// which only works if the field can genuinely be UNSET -- PublicBaseURL
	// is required in every stage (see its own doc comment) and so can never
	// serve as a double-duty off-switch. Empty string means the entire
	// cloud-identity feature (discovery/JWKS/minting/binding CRUD/
	// signing-key rotation) is OFF -- rather than inferring "off" from a
	// zero-value Timeouts field or a missing store, so there is exactly
	// one place a deploy flips this capability. Discovery/JWKS/minting
	// (httpapi/oidcdiscovery.go, cloudidentitytoken.go) each check this
	// field directly, inline, since each is a single free-function
	// handler; binding CRUD and signing-key rotation (httpapi/
	// cloudidentitybindings.go, cloudidentitykeys.go) are each mounted
	// behind a chi route group instead, so they check it exactly once,
	// group-wide, via httpapi.RequireCloudIdentityCapability(cfg.
	// CloudIdentityIssuerURL) (cmd/control-plane/main.go's own r.Use(...)
	// on all three of those groups) rather than a fourth/fifth per-handler
	// copy of the same inline check.
	// A non-empty value IS validated (unlike PublicBaseURL's own
	// "non-empty only" laxness) -- see cloudIdentityIssuerURLEnvVarName's
	// own doc comment for why this one gets real URL-shape validation:
	// AWS/GCP/Azure STS parse this value themselves to build discovery/
	// JWKS fetch URLs, so a malformed value here fails silently and
	// externally (a customer-side federation error Narvi never sees)
	// rather than loudly at boot.
	CloudIdentityIssuerURL string

	// OTLPEndpoint is §33's ("metrics export path") control-plane-only OTLP
	// opt-in: platform.SetupOTel's own endpoint parameter, threaded through
	// by cmd/control-plane/main.go alone (never cmd/sandbox-agent, which
	// hardcodes an empty string at its own call site regardless of anything
	// this process's environment carries -- see that call site's own doc
	// comment for why sandbox-agent structurally cannot reach this field
	// even by accident: §27.6's server-appended egress allowlist floor
	// admits the control-plane host plus the session's git hosts, never a
	// collector, and this is the property that keeps it that way rather
	// than widening it).
	//
	// Empty means the feature is OFF -- SetupOTel keeps building the
	// stdouttrace/stdoutmetric exporters it always has, byte-identical to
	// every deployment before §33 (objectStoreEndpointEnvVarName's own
	// "absent = feature off" precedent, one field instead of a whole typed
	// sub-config). A non-empty value is always the canonical
	// "scheme://host" string canonicalOTLPEndpointURL returns, never the
	// raw env value -- see otlpEndpointEnvVarName's own doc comment for the
	// full gating rule and every validation detail.
	OTLPEndpoint string

	// ObjectStorage is §8.6's ("uploads, blob storage & the in-sandbox
	// download_file tool", §28.7) typed, boot-validated object-storage
	// block. Nil means the feature is OFF (the mint endpoints return a
	// structured "uploads not configured" error and nothing else
	// degrades) -- see objectStoreEndpointEnvVarName's own doc comment
	// above for the exact gating rule and every field's env var.
	ObjectStorage *ObjectStorageConfig

	// LicenseKey is docs/design/boundaries-design.md, section 1's own raw licence
	// key, read from NARVI_LICENSE_KEY -- DELIBERATELY OPTIONAL and
	// DELIBERATELY UNVALIDATED here: empty means no key configured (the
	// public binary's own default, and a fully supported one -- §34.5:
	// "the public binary composes no module, so installed is empty and
	// Enabled is false for everything regardless of any key"), and a
	// NON-empty value is stored exactly as given, with no parsing, no
	// signature check, and no error path that could fail Load over it.
	// This is the one deliberate exception to every other credential-
	// shaped field's own "validate a non-empty value at boot" precedent
	// (see cloudIdentityIssuerURLEnvVarName's doc comment for that
	// precedent) -- a licence key is verified by internal/domain/license.
	// Parse, at the composition root, specifically so a malformed,
	// expired, or wrong-product key degrades every capability to
	// disabled and logs a warning (in a private build) rather than
	// failing this process's own boot: "a licensing lapse must not
	// become an outage" (technical plan §34.5). Never logged, never
	// echoed on any wire response, in whole or in part.
	LicenseKey string
}

// ObjectStorageConfig is §8.6's typed object-storage configuration
// (§28.7) -- endpoint, region, bucket, access key/secret (or ambient IAM
// when both are empty), an optional PublicEndpoint used to sign presigned
// URLs instead of Endpoint, a path-style toggle for MinIO-style backends,
// and the two upload byte-size caps §28.4 names. Constructed by Load only
// when NARVI_OBJECT_STORE_ENDPOINT is non-empty -- see
// objectStoreEndpointEnvVarName's own doc comment for the full gating
// rule. internal/adapters/outbound/objstore.New consumes this (via its own
// Config, constructed from these fields in cmd/control-plane/main.go) to
// build the real ports.BlobStore adapter.
type ObjectStorageConfig struct {
	// Endpoint is the internal/private S3-compatible endpoint the control
	// plane itself calls directly for Stat/Delete, and the fallback
	// signing host for PresignPut/PresignGet when PublicEndpoint is empty.
	Endpoint string

	// PublicEndpoint, when set, is used to SIGN PresignPut/PresignGet URLs
	// instead of Endpoint (§28.7: "presigning binds the host"). Optional.
	PublicEndpoint string

	// Region is required by SigV4 signing even against a non-AWS backend
	// (MinIO accepts any string).
	Region string

	// Bucket is the single configured bucket per deployment (§28.3: "one
	// configured bucket per deployment... the tenancy boundary IS the
	// deployment").
	Bucket string

	// AccessKeyID/SecretAccessKey are static credentials. Both empty
	// together selects the AWS SDK's own default credential chain
	// (ambient IAM, §28.7). Exactly one set without the other is a boot
	// error (InvalidObjectStoreCredentialsError) -- see
	// objectStoreAccessKeyIDEnvVarName's own doc comment.
	AccessKeyID     string
	SecretAccessKey string

	// UsePathStyle selects path-style addressing (bucket in the URL path)
	// instead of virtual-hosted-style -- required for MinIO-style
	// backends. Default false (AWS S3's own default).
	UsePathStyle bool

	// MaxUploadBytes is the per-file cap (§28.4: "propose 100 MiB").
	MaxUploadBytes int64

	// MaxSessionUploadBytes is the per-session running-total cap (§28.4:
	// "propose 1 GiB -- SUM(size_bytes) over the session's pending+ready
	// uploads").
	MaxSessionUploadBytes int64
}

// Load reads process configuration and validates it fail-fast, returning
// named, structured errors (joined via errors.Join when more than one
// check fails) instead of letting an invalid config boot silently. Callers
// (cmd/control-plane/main.go) call this once at process start.
func Load() (*Config, error) {
	stage := Stage(os.Getenv(envVarName))

	var errs []error

	switch stage {
	case StageDevelopment, StageStaging, StageProduction:
		// valid
	case "":
		// Required, no default -- see envVarName's own doc comment: a
		// deploy that forgets to set this must fail to boot, not silently
		// boot as StageDevelopment (which would weaken every auth cookie's
		// Secure attribute, authcookie.go).
		errs = append(errs, &MissingRequiredEnvError{EnvVar: envVarName})
	default:
		errs = append(errs, &InvalidStageError{Value: string(stage)})
	}

	timeouts := DefaultTimeouts()
	if err := timeouts.Validate(); err != nil {
		errs = append(errs, err)
	}

	rawLogLevel := os.Getenv(logLevelEnvVarName)
	if rawLogLevel == "" {
		rawLogLevel = defaultLogLevelValue
	}
	logLevel, err := parseLogLevel(rawLogLevel)
	if err != nil {
		errs = append(errs, err)
	}

	databaseURL := os.Getenv(databaseURLEnvVarName)
	if databaseURL == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: databaseURLEnvVarName})
	}

	httpAddr := os.Getenv(httpAddrEnvVarName)
	if httpAddr == "" {
		httpAddr = defaultHTTPAddr
	}

	dbPoolMaxConns := int32(defaultDBPoolMaxConns)
	if rawDBPoolMaxConns := os.Getenv(dbPoolMaxConnsEnvVarName); rawDBPoolMaxConns != "" {
		parsed, parseErr := strconv.Atoi(rawDBPoolMaxConns)
		if parseErr != nil || parsed <= 0 {
			errs = append(errs, &InvalidDBPoolMaxConnsError{Value: rawDBPoolMaxConns})
		} else {
			dbPoolMaxConns = int32(parsed)
		}
	}

	// ingressEnabled (§12.5): unset means every surface is enabled -- see
	// ingressEnabledEnvVarName's own doc comment for the full "why this
	// default, why the alternative is worse" reasoning. os.LookupEnv, not
	// the usual os.Getenv-returns-"" idiom every other var in this
	// function uses, because this is the one var in this file where "unset"
	// and "explicitly set to empty" must mean two different things (that
	// same doc comment's own last paragraph).
	ingressEnabled := map[integrations.Provider]bool{
		integrations.ProviderSlack:  true,
		integrations.ProviderLinear: true,
		integrations.ProviderGitHub: true,
	}
	if rawIngressEnabled, isSet := os.LookupEnv(ingressEnabledEnvVarName); isSet {
		ingressEnabled = make(map[integrations.Provider]bool, len(integrations.Providers))
		for _, entry := range parseCommaSeparatedList(rawIngressEnabled) {
			p, ok := integrations.ParseProvider(entry)
			if !ok {
				errs = append(errs, &InvalidIngressEnabledError{Value: entry})
				continue
			}
			ingressEnabled[p] = true
		}
	}

	hmacSandboxSecret := os.Getenv(hmacSandboxSecretEnvVarName)
	if hmacSandboxSecret == "" {
		errs = append(errs, &InvalidHMACSecretError{EnvVar: hmacSandboxSecretEnvVarName})
	}

	hmacBotsSecret := os.Getenv(hmacBotsSecretEnvVarName)
	if hmacBotsSecret == "" {
		errs = append(errs, &InvalidHMACSecretError{EnvVar: hmacBotsSecretEnvVarName})
	}

	hmacWebhookSecret := os.Getenv(hmacWebhookSecretEnvVarName)
	if hmacWebhookSecret == "" {
		errs = append(errs, &InvalidHMACSecretError{EnvVar: hmacWebhookSecretEnvVarName})
	}

	gitHubClientID := os.Getenv(gitHubClientIDEnvVarName)
	if gitHubClientID == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: gitHubClientIDEnvVarName})
	}

	gitHubClientSecret := os.Getenv(gitHubClientSecretEnvVarName)
	if gitHubClientSecret == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: gitHubClientSecretEnvVarName})
	}

	publicBaseURL := os.Getenv(publicBaseURLEnvVarName)
	if publicBaseURL == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: publicBaseURLEnvVarName})
	}

	// WebhookSecret/BotHandle/BotToken (below) are required ONLY when
	// GitHub ingress is enabled (ingressEnabled[integrations.ProviderGitHub]
	// -- ingressEnabledEnvVarName's own doc comment, §12.5). A deployment
	// that never enables GitHub ingress still reads whatever value happens
	// to be set (never validated), but Config.IngressEnabled -- not this
	// pair's own non-empty-ness -- is what every consumer downstream
	// (controlplane/serve.go's route mounting, httpapi.
	// configuredForProvider) actually gates on.
	gitHubWebhookSecret := os.Getenv(gitHubWebhookSecretEnvVarName)
	if ingressEnabled[integrations.ProviderGitHub] && gitHubWebhookSecret == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: gitHubWebhookSecretEnvVarName})
	}

	gitHubBotHandle := os.Getenv(gitHubBotHandleEnvVarName)
	if ingressEnabled[integrations.ProviderGitHub] && gitHubBotHandle == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: gitHubBotHandleEnvVarName})
	}

	// gitHubReReviewLabel is DELIBERATELY OPTIONAL -- see its own env-var
	// doc comment above. No MissingRequiredEnvError is ever appended for
	// it; an unset value defaults to defaultGitHubReReviewLabel, mirroring
	// httpAddr's own defaulting immediately above.
	gitHubReReviewLabel := os.Getenv(gitHubReReviewLabelEnvVarName)
	if gitHubReReviewLabel == "" {
		gitHubReReviewLabel = defaultGitHubReReviewLabel
	}

	// gitHubReleaseLabel/gitHubReleaseBranchPattern are DELIBERATELY
	// OPTIONAL -- see their own env-var doc comment above. No
	// MissingRequiredEnvError is ever appended for either.
	gitHubReleaseLabel := os.Getenv(gitHubReleaseLabelEnvVarName)
	if gitHubReleaseLabel == "" {
		gitHubReleaseLabel = defaultGitHubReleaseLabel
	}
	gitHubReleaseBranchPattern := os.Getenv(gitHubReleaseBranchPatternEnvVarName)
	if gitHubReleaseBranchPattern == "" {
		gitHubReleaseBranchPattern = defaultGitHubReleaseBranchPattern
	}

	gitHubBotToken := os.Getenv(gitHubBotTokenEnvVarName)
	if ingressEnabled[integrations.ProviderGitHub] && gitHubBotToken == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: gitHubBotTokenEnvVarName})
	}

	// gitHubImageBuildToken is DELIBERATELY OPTIONAL -- see its own env-var
	// doc comment above. No MissingRequiredEnvError is ever appended for
	// it; an empty value here is a valid, expected, degraded-gracefully
	// configuration, not a boot-time failure.
	gitHubImageBuildToken := os.Getenv(gitHubImageBuildTokenEnvVarName)

	// reviewModelDeep (§26.3): OPTIONAL, no default -- an empty
	// value here is a valid, expected, degraded-gracefully configuration
	// (reviewModelDeepEnvVarName's own doc comment), not a boot-time
	// failure.
	reviewModelDeep := os.Getenv(reviewModelDeepEnvVarName)

	var tokenEncryptionKey []byte
	rawTokenEncryptionKey := os.Getenv(tokenEncryptionKeyEnvVarName)
	if rawTokenEncryptionKey == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: tokenEncryptionKeyEnvVarName})
	} else {
		decoded, decodeErr := base64.StdEncoding.DecodeString(rawTokenEncryptionKey)
		switch {
		case decodeErr != nil:
			errs = append(errs, &InvalidTokenEncryptionKeyError{
				Reason: fmt.Sprintf("not valid base64: %v", decodeErr),
			})
		case len(decoded) != tokenEncryptionKeyByteLength:
			errs = append(errs, &InvalidTokenEncryptionKeyError{
				Reason: fmt.Sprintf("must base64-decode to exactly %d bytes for AES-256-GCM, got %d", tokenEncryptionKeyByteLength, len(decoded)),
			})
		default:
			tokenEncryptionKey = decoded
		}
	}

	allowedEmailDomains := parseCommaSeparatedList(os.Getenv(allowedEmailDomainsEnvVarName))
	allowedGitHubOrgs := parseCommaSeparatedList(os.Getenv(allowedGitHubOrgsEnvVarName))
	allowedEmails := parseCommaSeparatedList(os.Getenv(allowedEmailsEnvVarName))
	if len(allowedEmailDomains) == 0 && len(allowedGitHubOrgs) == 0 && len(allowedEmails) == 0 {
		errs = append(errs, &EmptyAllowlistError{})
	}

	initialAdminEmails := parseCommaSeparatedList(os.Getenv(initialAdminEmailsEnvVarName))

	// epistemicCheckDefault (§20.4): optional, default false --
	// mirrors objectStoreUsePathStyle's own identical "empty means
	// unset, parse only when present, reject anything ParseBool doesn't
	// recognize" idiom (Load's own object-storage block, below).
	epistemicCheckDefault := false
	if raw := os.Getenv(epistemicCheckDefaultEnvVarName); raw != "" {
		parsed, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			errs = append(errs, &InvalidEpistemicCheckDefaultError{Value: raw})
		} else {
			epistemicCheckDefault = parsed
		}
	}

	// rolloutMode (§32): optional, default rollout.ModeOpen --
	// mirrors epistemicCheckDefault's own identical "empty means unset,
	// parse only when present" idiom immediately above, but with an
	// explicit two-value switch (like Stage, envVarName's own block
	// above) rather than strconv.ParseBool, since this is a two-value
	// enum, not a boolean.
	rolloutMode := rollout.ModeOpen
	if raw := os.Getenv(rolloutModeEnvVarName); raw != "" {
		switch rollout.Mode(raw) {
		case rollout.ModeOpen, rollout.ModeCohort:
			rolloutMode = rollout.Mode(raw)
		default:
			errs = append(errs, &InvalidRolloutModeError{Value: raw})
		}
	}

	// shadowMode (§30.8): optional, default false -- mirrors
	// epistemicCheckDefault's own identical "empty means unset, parse
	// only when present, reject anything ParseBool doesn't recognize"
	// idiom above.
	shadowMode := false
	if raw := os.Getenv(shadowModeEnvVarName); raw != "" {
		parsed, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			errs = append(errs, &InvalidShadowModeError{Value: raw})
		} else {
			shadowMode = parsed
		}
	}

	// gitHubAppID/gitHubAppPrivateKey (§30.4): required in every stage --
	// see gitHubAppIDEnvVarName's own doc comment for why this differs
	// from gitHubImageBuildToken's own optional precedent immediately
	// above despite both being GitHub-flavored platform credentials.
	var gitHubAppID int64
	rawGitHubAppID := os.Getenv(gitHubAppIDEnvVarName)
	if rawGitHubAppID == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: gitHubAppIDEnvVarName})
	} else {
		parsed, parseErr := strconv.ParseInt(rawGitHubAppID, 10, 64)
		if parseErr != nil || parsed <= 0 {
			errs = append(errs, &InvalidGitHubAppIDError{Value: rawGitHubAppID})
		} else {
			gitHubAppID = parsed
		}
	}

	var gitHubAppPrivateKey *rsa.PrivateKey
	rawGitHubAppPrivateKey := os.Getenv(gitHubAppPrivateKeyEnvVarName)
	if rawGitHubAppPrivateKey == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: gitHubAppPrivateKeyEnvVarName})
	} else {
		parsed, parseErr := parseGitHubAppPrivateKey(rawGitHubAppPrivateKey)
		if parseErr != nil {
			errs = append(errs, parseErr)
		} else {
			gitHubAppPrivateKey = parsed
		}
	}

	modalBaseURL := os.Getenv(modalBaseURLEnvVarName)
	if modalBaseURL == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: modalBaseURLEnvVarName})
	}

	modalAuthToken := os.Getenv(modalAuthTokenEnvVarName)
	if modalAuthToken == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: modalAuthTokenEnvVarName})
	}

	modalEgressProxyURL := os.Getenv(modalEgressProxyURLEnvVarName)

	// rwxAccessToken is optional -- see its own env-var-name doc comment
	// above. No MissingRequiredEnvError is ever appended for it.
	rwxAccessToken := os.Getenv(rwxAccessTokenEnvVarName)

	openCodeRuntimeVersion := os.Getenv(openCodeRuntimeVersionEnvVarName)
	if openCodeRuntimeVersion == "" {
		openCodeRuntimeVersion = defaultOpenCodeRuntimeVersion
	}

	// All five Linear fields below (and both Slack fields further down) are
	// required ONLY when their own ingress surface is enabled -- see the
	// identical GitHub comment above this same pattern started with.
	linearWebhookSecret := os.Getenv(linearWebhookSecretEnvVarName)
	if ingressEnabled[integrations.ProviderLinear] && linearWebhookSecret == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: linearWebhookSecretEnvVarName})
	}

	linearOAuthClientID := os.Getenv(linearOAuthClientIDEnvVarName)
	if ingressEnabled[integrations.ProviderLinear] && linearOAuthClientID == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: linearOAuthClientIDEnvVarName})
	}

	linearOAuthClientSecret := os.Getenv(linearOAuthClientSecretEnvVarName)
	if ingressEnabled[integrations.ProviderLinear] && linearOAuthClientSecret == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: linearOAuthClientSecretEnvVarName})
	}

	linearDefaultRepoName := os.Getenv(linearDefaultRepoNameEnvVarName)
	if ingressEnabled[integrations.ProviderLinear] && linearDefaultRepoName == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: linearDefaultRepoNameEnvVarName})
	}

	linearDefaultRepoURL := os.Getenv(linearDefaultRepoURLEnvVarName)
	if ingressEnabled[integrations.ProviderLinear] && linearDefaultRepoURL == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: linearDefaultRepoURLEnvVarName})
	}

	slackSigningSecret := os.Getenv(slackSigningSecretEnvVarName)
	if ingressEnabled[integrations.ProviderSlack] && slackSigningSecret == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: slackSigningSecretEnvVarName})
	}

	slackBotToken := os.Getenv(slackBotTokenEnvVarName)
	if ingressEnabled[integrations.ProviderSlack] && slackBotToken == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: slackBotTokenEnvVarName})
	}

	// SlackDefaultRepoName/URL are optional (see their own env-var-name
	// doc comment above) -- but a NON-empty value is validated with the
	// SAME domain validators the REST create-session path already trusts
	// (internal/adapters/inbound/httpapi.CreateSessionCore), so a typo'd
	// operator-configured repo fails fast at boot rather than surfacing
	// as a confusing 500 on the first Slack mention that tries to use it.
	slackDefaultRepoName := os.Getenv(slackDefaultRepoNameEnvVarName)
	if slackDefaultRepoName != "" {
		if err := reposource.ValidateRepoName(slackDefaultRepoName); err != nil {
			errs = append(errs, fmt.Errorf("invalid %s: %w", slackDefaultRepoNameEnvVarName, err))
		}
	}
	slackDefaultRepoURL := os.Getenv(slackDefaultRepoURLEnvVarName)
	if slackDefaultRepoURL != "" {
		if err := reposource.ValidateRepoURL(slackDefaultRepoURL); err != nil {
			errs = append(errs, fmt.Errorf("invalid %s: %w", slackDefaultRepoURLEnvVarName, err))
		}
	}

	anthropicAPIKey := os.Getenv(anthropicAPIKeyEnvVarName)
	if anthropicAPIKey == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: anthropicAPIKeyEnvVarName})
	}

	intentClassifierProvider := os.Getenv(intentClassifierProviderEnvVarName)
	if intentClassifierProvider == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: intentClassifierProviderEnvVarName})
	}

	intentClassifierModel := os.Getenv(intentClassifierModelEnvVarName)
	if intentClassifierModel == "" {
		errs = append(errs, &MissingRequiredEnvError{EnvVar: intentClassifierModelEnvVarName})
	}

	intentClassifierActiveSurfaces := parseCommaSeparatedList(os.Getenv(intentClassifierActiveSurfacesEnvVarName))

	// cloudIdentityIssuerURL (§27.3): DELIBERATELY OPTIONAL --
	// see cloudIdentityIssuerURLEnvVarName's own doc comment for the full
	// "off when unset" gating rule and why a non-empty value gets real
	// URL-shape validation (unlike PublicBaseURL above). Assigned the
	// CANONICAL string canonicalCloudIdentityIssuerURL returns, never the
	// raw env value -- see that function's own doc comment for why the
	// raw value must never reach Config.CloudIdentityIssuerURL.
	cloudIdentityIssuerURL := ""
	if raw := os.Getenv(cloudIdentityIssuerURLEnvVarName); raw != "" {
		canonical, err := canonicalCloudIdentityIssuerURL(raw)
		if err != nil {
			errs = append(errs, err)
		} else {
			cloudIdentityIssuerURL = canonical
		}
	}

	// otlpEndpoint (§33): DELIBERATELY OPTIONAL, one field rather than a
	// whole typed sub-config -- see otlpEndpointEnvVarName's own doc
	// comment for the full "off when unset" gating rule and
	// canonicalOTLPEndpointURL's own doc comment for why a non-empty value
	// gets real URL-shape validation. Assigned the CANONICAL string
	// canonicalOTLPEndpointURL returns, never the raw env value, mirroring
	// cloudIdentityIssuerURL's own identical assignment immediately above.
	otlpEndpoint := ""
	if raw := os.Getenv(otlpEndpointEnvVarName); raw != "" {
		canonical, err := canonicalOTLPEndpointURL(raw)
		if err != nil {
			errs = append(errs, err)
		} else {
			otlpEndpoint = canonical
		}
	}

	// Object storage (§28.7): feature-flagged on objectStoreEndpointEnvVarName
	// alone -- see that const's own doc comment for the full gating rule.
	// Every other NARVI_OBJECT_STORE_* var is read and validated ONLY
	// inside this branch, so a deploy that never sets an endpoint never
	// trips a boot error over an unrelated stray/leftover object-store
	// var.
	var objectStorage *ObjectStorageConfig
	objectStoreEndpoint := os.Getenv(objectStoreEndpointEnvVarName)
	if objectStoreEndpoint != "" {
		objectStoreRegion := os.Getenv(objectStoreRegionEnvVarName)
		if objectStoreRegion == "" {
			errs = append(errs, &MissingRequiredEnvError{EnvVar: objectStoreRegionEnvVarName})
		}

		objectStoreBucket := os.Getenv(objectStoreBucketEnvVarName)
		if objectStoreBucket == "" {
			errs = append(errs, &MissingRequiredEnvError{EnvVar: objectStoreBucketEnvVarName})
		}

		objectStoreAccessKeyID := os.Getenv(objectStoreAccessKeyIDEnvVarName)
		objectStoreSecretAccessKey := os.Getenv(objectStoreSecretAccessKeyEnvVarName)
		if (objectStoreAccessKeyID == "") != (objectStoreSecretAccessKey == "") {
			errs = append(errs, &InvalidObjectStoreCredentialsError{})
		}

		objectStoreUsePathStyle := false
		if raw := os.Getenv(objectStoreUsePathStyleEnvVarName); raw != "" {
			parsed, parseErr := strconv.ParseBool(raw)
			if parseErr != nil {
				errs = append(errs, &InvalidObjectStoreUsePathStyleError{Value: raw})
			} else {
				objectStoreUsePathStyle = parsed
			}
		}

		// MaxUploadBytes/MaxSessionUploadBytes are read/validated only
		// inside this same feature-on branch: with uploads off entirely,
		// a stray/leftover override for either is simply never looked at,
		// matching this whole block's own "endpoint absent = fully off,
		// nothing else even inspected" gating rule rather than treating
		// these two as independently-always-validated knobs.
		maxUploadBytes := defaultMaxUploadBytes
		if raw := os.Getenv(objectStoreMaxUploadBytesEnvVarName); raw != "" {
			parsed, parseErr := strconv.ParseInt(raw, 10, 64)
			if parseErr != nil || parsed <= 0 {
				errs = append(errs, &InvalidObjectStoreMaxBytesError{EnvVar: objectStoreMaxUploadBytesEnvVarName, Value: raw})
			} else {
				maxUploadBytes = parsed
			}
		}

		maxSessionUploadBytes := defaultMaxSessionUploadBytes
		if raw := os.Getenv(objectStoreMaxSessionUploadBytesEnvVarName); raw != "" {
			parsed, parseErr := strconv.ParseInt(raw, 10, 64)
			if parseErr != nil || parsed <= 0 {
				errs = append(errs, &InvalidObjectStoreMaxBytesError{EnvVar: objectStoreMaxSessionUploadBytesEnvVarName, Value: raw})
			} else {
				maxSessionUploadBytes = parsed
			}
		}

		objectStorage = &ObjectStorageConfig{
			Endpoint:              objectStoreEndpoint,
			PublicEndpoint:        os.Getenv(objectStorePublicEndpointEnvVarName),
			Region:                objectStoreRegion,
			Bucket:                objectStoreBucket,
			AccessKeyID:           objectStoreAccessKeyID,
			SecretAccessKey:       objectStoreSecretAccessKey,
			UsePathStyle:          objectStoreUsePathStyle,
			MaxUploadBytes:        maxUploadBytes,
			MaxSessionUploadBytes: maxSessionUploadBytes,
		}
	}

	// licenseKey (docs/design/boundaries-design.md, section 1.5): DELIBERATELY
	// UNVALIDATED -- see Config.LicenseKey's own doc comment for why a
	// malformed value here must never fail this function, unlike every
	// other credential-shaped field above.
	licenseKey := os.Getenv(licenseKeyEnvVarName)

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return &Config{
		Stage:                      stage,
		Timeouts:                   timeouts,
		LogLevel:                   logLevel,
		DatabaseURL:                databaseURL,
		HTTPAddr:                   httpAddr,
		DBPoolMaxConns:             dbPoolMaxConns,
		IngressEnabled:             ingressEnabled,
		HMACSandboxSecret:          hmacSandboxSecret,
		HMACBotsSecret:             hmacBotsSecret,
		HMACWebhookSecret:          hmacWebhookSecret,
		GitHubClientID:             gitHubClientID,
		GitHubClientSecret:         gitHubClientSecret,
		GitHubWebhookSecret:        gitHubWebhookSecret,
		GitHubBotHandle:            gitHubBotHandle,
		GitHubReReviewLabel:        gitHubReReviewLabel,
		GitHubReleaseLabel:         gitHubReleaseLabel,
		GitHubReleaseBranchPattern: gitHubReleaseBranchPattern,
		GitHubBotToken:             gitHubBotToken,
		GitHubImageBuildToken:      gitHubImageBuildToken,
		ReviewModelDeep:            reviewModelDeep,
		PublicBaseURL:              publicBaseURL,
		TokenEncryptionKey:         tokenEncryptionKey,
		AllowedEmailDomains:        allowedEmailDomains,
		AllowedGitHubOrgs:          allowedGitHubOrgs,
		AllowedEmails:              allowedEmails,
		InitialAdminEmails:         initialAdminEmails,
		EpistemicCheckDefault:      epistemicCheckDefault,
		RolloutMode:                rolloutMode,
		ShadowMode:                 shadowMode,
		GitHubAppID:                gitHubAppID,
		GitHubAppPrivateKey:        gitHubAppPrivateKey,
		ModalBaseURL:               modalBaseURL,
		ModalAuthToken:             modalAuthToken,
		ModalEgressProxyURL:        modalEgressProxyURL,
		RWXAccessToken:             rwxAccessToken,
		OpenCodeRuntimeVersion:     openCodeRuntimeVersion,

		LinearWebhookSecret:     linearWebhookSecret,
		LinearOAuthClientID:     linearOAuthClientID,
		LinearOAuthClientSecret: linearOAuthClientSecret,
		LinearDefaultRepoName:   linearDefaultRepoName,
		LinearDefaultRepoURL:    linearDefaultRepoURL,

		SlackSigningSecret:   slackSigningSecret,
		SlackBotToken:        slackBotToken,
		SlackDefaultRepoName: slackDefaultRepoName,
		SlackDefaultRepoURL:  slackDefaultRepoURL,

		AnthropicAPIKey:          anthropicAPIKey,
		IntentClassifierProvider: intentClassifierProvider,
		IntentClassifierModel:    intentClassifierModel,

		IntentClassifierActiveSurfaces: intentClassifierActiveSurfaces,

		CloudIdentityIssuerURL: cloudIdentityIssuerURL,

		OTLPEndpoint: otlpEndpoint,

		ObjectStorage: objectStorage,

		LicenseKey: licenseKey,
	}, nil
}
