package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/releasereview"
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	appreviewtriage "github.com/narvidev/narvi/internal/app/reviewtriage"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/review"
	domainreviewtriage "github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/platform"
)

// maxWebhookBodyBytes bounds every GitHub webhook body this handler reads
// (via http.MaxBytesReader) -- mirrors internal/adapters/inbound/httpapi's
// own maxRequestBodyBytes precedent (1 MiB is generous for a comment
// body plus the surrounding PR/repo metadata GitHub includes).
const maxWebhookBodyBytes = 1 << 20 // 1 MiB

// githubDeliveryProvider is the "provider" value this adapter passes to
// postgres.WebhookDeliveryStore.Claim -- §5.1's own doc comment names
// this exact literal for GitHub.
const githubDeliveryProvider = "github"

// signatureHeaderPrefix is the fixed prefix GitHub's own
// "X-Hub-Signature-256" header value always carries before the actual hex
// digest.
const signatureHeaderPrefix = "sha256="

// diffFetcher is Config.DiffFetcher's own declared type -- the union of
// reviewcontext.Fetcher (review-turn-context assembly
// C2's own narrower interface) and prDiffFetcher (pullrequestevent.go's
// sentinel-fix merge-gate, a genuinely different consumer of the SAME
// underlying *githubapi.Adapter) -- see Config.DiffFetcher's own doc
// comment for why this ONE config field serves both. *githubapi.Adapter
// satisfies this directly (GetPullRequest/GetCompareDiff/
// GetPullRequestDiff are all real adapter methods); a fake in this
// package's own tests only needs to implement whichever subset the test
// actually exercises, plus stub the rest.
type diffFetcher interface {
	reviewcontext.Fetcher
	prDiffFetcher
}

// Config bundles what NewHandler needs beyond the stores/registry already
// bundled in a *SessionCoalescer -- both required in every stage, see
// internal/platform/config.go's own gitHubWebhookSecretEnvVarName doc
// comment.
type Config struct {
	// WebhookSecret verifies "X-Hub-Signature-256" -- GitHub's own real
	// webhook secret, DISTINCT from platform.Config.HMACWebhookSecret.
	WebhookSecret string
	// BotHandle is the bot/app username mention detection matches comment
	// bodies against (compileMentionPattern, payload.go).
	BotHandle string

	// BotToken/PullRequests/Timeouts (batch fix/audit-github-pr-payload-
	// correctness, H5 audit fix) together resolve an issue_comment
	// mention's TRUE head branch/repo via one authenticated GitHub REST
	// API call -- see headresolve.go's own doc comments for the full
	// fallback behavior.
	//
	// BotToken is the SAME bot credential githubapi.BotNotifier already
	// authenticates its own PostIssueComment calls with
	// (platform.Config.GitHubBotToken) -- never a per-commenter
	// credential: a GitHub webhook mention carries no OAuth token for the
	// commenter (unlike CreatePR's own per-session, per-creator token),
	// and resolving a PR's own already-public head branch/repo needs none
	// of that per-user identity.
	//
	// PullRequests is nil-safe: nil (this package's own handler_test.go,
	// or any other minimal wiring that doesn't care about this fix) simply
	// keeps today's pre-fix fallback behavior for every issue_comment
	// mention -- see resolveIssueCommentHead's own doc comment.
	// githubapi.Adapter (the SAME instance production wiring already
	// constructs for CreatePR/ResolveBranchSHA/ResolveContractsFingerprint,
	// cmd/control-plane/main.go) satisfies this directly.
	//
	// Timeouts.GitHubGetPRTimeout bounds the one GetPullRequest call this
	// handler makes synchronously, inline, in its own request path --
	// mirrors internal/adapters/inbound/slack's own AckTimeout precedent
	// exactly (that field's own doc comment): a genuine outbound network
	// call made inline in a webhook handler must never run against the
	// bare, deadline-free r.Context() unbounded.
	BotToken     string
	PullRequests PullRequestResolver
	Timeouts     platform.Timeouts

	// ReReviewLabel ("review sessions", §8.2) is this deployment's
	// own configured label NAME (platform.Config.GitHubReReviewLabel) a
	// maintainer applies to a PR to manually re-trigger its review session
	// -- parsePullRequestLabeled's own matching criterion (payload.go).
	// Empty (this package's own handler_test.go, or any other minimal
	// wiring that doesn't care about this lane) means this lane never
	// fires -- see that function's own doc comment.
	ReReviewLabel string

	// DiffFetcher ("review sessions", §8.2) fetches this PR's own
	// inline pre-fetched diff (and, when not already known from the
	// triggering event's own payload, its GitHub-native stack context,
	// §17.6) via internal/app/reviewcontext.Fetch -- folded into every
	// review turn's own prompt text (review.RenderTurnPrompt) BEFORE
	// coalescer.CreateOrJoin, so both the WINNER and REUSE branches get it
	// identically without either needing its own copy of this logic. This
	// SAME field also backs pullrequestevent.go's own
	// githubMergeGateDataSource.diffFetcher (§8.2's sentinel-fix
	// merge-gate, a genuinely different consumer with a genuinely
	// different need -- see prDiffFetcher's own doc comment,
	// pullrequestevent.go) -- diffFetcher (below) is the union of both
	// interfaces so this ONE config field can keep serving both, exactly
	// as it always has. Nil-safe: nil (this package's own
	// handler_test.go, or any other minimal wiring that doesn't care
	// about this Step) simply skips the fetch entirely -- see this
	// file's own call site doc comment. *githubapi.Adapter (the SAME
	// instance PullRequests/Comments above already wire,
	// cmd/control-plane/main.go) satisfies this directly.
	DiffFetcher diffFetcher

	// ReviewFindings ("sentinels + suggestions", §22.1) fetches
	// this PR's own currently open+rebutted review_findings rows
	// (internal/app/reviewcontext.FetchAlreadyAnswered), rendered as a
	// deterministic "already answered" fact block PREPENDED to (never
	// replacing) the mention's own prose text, BEFORE DiffFetcher/
	// RenderTurnPrompt append the diff/stack/tool-instructions blocks.
	// Nil-safe: nil (this package's own handler_test.go) simply skips
	// this fetch entirely. *postgres.ReviewFindingStore (cmd/control-
	// plane/main.go) satisfies this directly.
	ReviewFindings reviewcontext.FindingsFetcher

	// FalsePositivePatterns ("review: learned false-positive
	// patterns", §22.3) fetches this repo's own currently-active learned
	// false-positive patterns and renders them as an advisory content
	// block, prepended to every review turn's own prompt exactly like
	// ReviewFindings' own already-answered-facts block immediately above
	// -- "injected into every review pass, first pass and re-review
	// alike" applies identically to a brand-new mention (this handler's
	// own WINNER path) and a coalesced second mention (REUSE path), since
	// both build a turn prompt through this SAME code path. Nil-safe: nil
	// (this package's own handler_test.go) simply skips this fetch
	// entirely. *postgres.FalsePositivePatternStore (cmd/control-plane/
	// main.go) satisfies this directly.
	FalsePositivePatterns reviewcontext.FalsePositivePatternsFetcher

	// FalsePositivePatternCapture (§22.2) is this handler's own
	// dispatch-before-router capture surface -- see
	// falsepositivecapture.go's own doc comment for the full "why before
	// parseMention" reasoning. Nil-safe: nil (this package's own
	// handler_test.go, or any other minimal wiring that doesn't care
	// about this Step) simply skips capture detection entirely, falling
	// through to the ordinary mention pipeline exactly as if every
	// comment were an ordinary one. The SAME *postgres.
	// FalsePositivePatternStore instance as FalsePositivePatterns above
	// satisfies this directly (a structurally different interface --
	// read vs. write -- but one concrete store, cmd/control-plane/
	// main.go).
	FalsePositivePatternCapture FalsePositivePatternCapturer

	// ArchRecapContestCapture (§26.5) is this handler's own
	// dispatch-before-router capture surface for the `arch recap wrong:
	// <reason>` command -- see archrecapcontest.go's own doc comment for
	// the full "why before parseMention" reasoning (identical to
	// FalsePositivePatternCapture's own). Nil-safe: nil (this package's
	// own handler_test.go, or any other minimal wiring that doesn't care
	// about this Step) simply skips capture detection entirely, falling
	// through to the ordinary mention pipeline exactly as if every
	// comment were an ordinary one. *postgres.
	// ReviewDigestSectionFeedbackStore (cmd/control-plane/main.go)
	// satisfies this directly.
	ArchRecapContestCapture ArchRecapContestCapturer
	// ArchRecapVerdicts (§26.5) resolves the PR's own latest
	// posted verdict so tryCaptureArchRecapContest can hash the CURRENTLY-
	// CONTESTED arch recap content -- see that function's own doc comment.
	// A zero-value appreviewverdict.Deps{} (ReviewVerdicts == nil) is
	// explicitly guarded by tryCaptureArchRecapContest itself (never a nil-
	// pointer panic through *postgres.ReviewVerdictStore's own un-nil-
	// guarded methods) -- production wiring (cmd/control-plane/main.go)
	// always sets this alongside ArchRecapContestCapture, but this field
	// is independently defended anyway, mirroring this codebase's own
	// established "guard a nil dependency defensively, never assume a
	// sibling field's own non-nil-ness" convention (e.g. internal/app/
	// reviewtriage.LoadConfig's own `deps.RepoSettings == nil` check).
	ArchRecapVerdicts appreviewverdict.Deps

	// Comments (a follow-up fix, Finding 1) posts the honest
	// planAwaitingApprovalReplyText reply (planawaitingreply.go) back to a
	// PR thread when coalesce.go's CreateOrJoin declines to enqueue a build
	// turn because the session's plan is currently awaiting approval. ALSO
	// posts actorNotAuthorizedReplyText (batch fix/deny-unlinked-github-
	// actors) for an UNLINKED commenter's denied mention -- the SAME
	// interface, never a second one, since both are just "post an honest
	// reply back to this PR thread". Nil-safe: nil (this package's own
	// handler_test.go, or any other minimal wiring that doesn't care about
	// either reply) simply skips posting -- see postPlanAwaitingReply's/
	// postActorNotAuthorizedReply's own doc comments. githubapi.Adapter
	// (the SAME instance production wiring already constructs for
	// PullRequests above) satisfies this directly.
	Comments CommentPoster

	// PublicBaseURL (batch fix/deny-unlinked-github-actors) is this
	// control plane's own externally-reachable base URL
	// (platform.Config.PublicBaseURL -- the SAME base identitylink.
	// BuildMagicLinkURL already uses, internal/app/identitylink/
	// service.go), used to build the sign-in URL
	// (PublicBaseURL+signInPath) actorNotAuthorizedReplyText points an
	// unlinked commenter at. Empty (this package's own handler_test.go, or
	// any other minimal wiring) simply renders a base-less/relative-looking
	// URL in that reply -- harmless for tests that never post a real
	// comment, but production wiring (cmd/control-plane/main.go) always
	// sets this from cfg.PublicBaseURL.
	PublicBaseURL string

	// LinkNotices (batch fix/deny-unlinked-github-actors) backs
	// claimActorNotAuthorizedNotice's own anti-spam dedupe check
	// (actornotauthorizedreply.go): one row per (repo, PR, commenter)
	// already told to sign in, atomically claimed against
	// Timeouts.GitHubActorNoticeTTL. Nil-safe: nil (this package's own
	// handler_test.go, or any other minimal wiring that doesn't care about
	// this dedupe) always allows the reply -- see that function's own doc
	// comment for why. Typed as the narrow ActorLinkNoticeClaimer
	// interface (not the concrete store type) so a test can inject a fake
	// that forces a genuine claim failure -- postgres.
	// NewGitHubActorLinkNoticeStore(pool) (the SAME pool every other store
	// in this Config/SessionCoalescer already shares) satisfies this
	// directly in production.
	LinkNotices ActorLinkNoticeClaimer

	// SentinelFixes/RepoSettings/AuditLog ("sentinels +
	// suggestions", §17.4/§17.5) back the NEW `pull_request`/action=="closed"
	// merge-gating lane (pullrequestevent.go) -- SentinelFixes looks up
	// whether the closing PR had a sentinel-auto-fix in flight at all
	// (the overwhelming common case: no, nothing to do); RepoSettings
	// re-reads the toggle FRESH at gating time; AuditLog records the
	// outcome (§17.5). Nil-safe: a nil SentinelFixes means this lane never
	// fires (every "closed" pull_request event is acknowledged as a
	// plain no-op) -- this package's own handler_test.go, or any other
	// minimal wiring that doesn't care about this Step, leaves it nil.
	SentinelFixes *postgres.SentinelFixStore
	RepoSettings  *postgres.RepoSettingsStore
	AuditLog      *postgres.AuditLogStore

	// PendingChecks/ReleaseLabel/ReleaseBranchPattern ("release
	// PR review", §15; PendingChecks itself is blocking-finding fix #1)
	// back triggerReleaseManifestCheckBestEffort (releasemanifest.go):
	// PendingChecks is where a detected release PR's own manifest-check
	// request is durably enqueued (internal/app/releasereview.Enqueue) --
	// a single, fast, cheap INSERT into release_manifest_pending; the
	// ACTUAL check (ListMergedBetween, up to ~80+ sequential GitHub API
	// calls) is run LATER, by a separate internal/app/releasereview.Worker
	// background loop, never inline on this webhook request's own
	// context -- see migrations/000050_release_manifest_pending.up.sql's
	// own doc comment for the full "why" (this fixes a real bug: the
	// check used to run synchronously here, and silently, permanently
	// died whenever a webhook delivery outlasted GitHub's own ~10s
	// timeout). ReleaseLabel/ReleaseBranchPattern are this deployment's
	// own configured release-PR detection values (platform.Config.
	// GitHubReleaseLabel/GitHubReleaseBranchPattern). Nil-safe: a nil
	// PendingChecks (this package's own handler_test.go, or any other
	// minimal wiring that doesn't care about this Step) simply skips
	// this entirely -- see triggerReleaseManifestCheckBestEffort's own
	// doc comment.
	PendingChecks        releasereview.PendingEnqueuer
	ReleaseLabel         string
	ReleaseBranchPattern string

	// Timers ("review: automatic re-review on new commits",
	// §24.1) backs the NEW `pull_request`/action=="synchronize" lane
	// (pullrequestsynchronize.go): the exported postgres.TimerStore.Upsert
	// this handler calls DIRECTLY, bypassing the actor's mailbox entirely
	// (§24.1's 4th cost item -- command.go's own Command sum type has no
	// member for an inbound "new commits pushed" signal), to re-arm the
	// review_retrigger_debounce named timer (session_timers, §2) in the
	// SAME transaction as this lane's own github_pr_sessions.
	// pending_retrigger_head_sha upsert. Nil-safe: a nil Timers means
	// this lane never fires (every `synchronize` event is acknowledged as
	// a plain no-op) -- this package's own handler_test.go, or any other
	// minimal wiring that doesn't care about this Step, leaves it nil.
	Timers *postgres.TimerStore
}

// NewHandler builds the POST /webhooks/github handler (cmd/control-plane/
// main.go). See doc.go's own "Request handling" section for the full
// verify -> dedupe-claim -> parse -> detect -> coalesce sequencing this
// implements.
func NewHandler(coalescer *SessionCoalescer, deliveries *postgres.WebhookDeliveryStore, cfg Config) http.HandlerFunc {
	mentionRE := compileMentionPattern(cfg.BotHandle)
	secret := []byte(cfg.WebhookSecret)

	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Signature verification happens over these EXACT raw bytes,
		// fail-closed on ANY error (missing header, malformed hex,
		// mismatch) -- see platform.VerifyWebhookSignature's own doc
		// comment (internal/platform/webhooksig.go).
		presentedSig := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), signatureHeaderPrefix)
		if err := platform.VerifyWebhookSignature(secret, body, presentedSig); err != nil {
			logger.Warn("github: webhook signature verification failed", "error", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		deliveryID := r.Header.Get("X-GitHub-Delivery")
		if deliveryID == "" {
			logger.Warn("github: webhook request missing X-GitHub-Delivery header")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		claim, err := deliveries.Claim(ctx, githubDeliveryProvider, deliveryID)
		if err != nil {
			logger.Error("github: claim webhook delivery failed", "error", err, "delivery_id", deliveryID)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !claim.Inserted {
			// A redelivery of an already-claimed delivery id -- L4 audit
			// fix: GitHub does NOT automatically retry/redeliver a webhook
			// on a non-2xx response or a timeout (corrected from this
			// comment's own previous, factually wrong claim that it does);
			// redelivery is manual-only, triggered by a human via GitHub's
			// own "Redeliver" UI/API action once they notice a failure and
			// choose to retry it. Acknowledge without reprocessing either
			// way.
			logger.Info("github: duplicate webhook delivery, skipping", "delivery_id", deliveryID)
			w.WriteHeader(http.StatusOK)
			return
		}

		eventType := r.Header.Get("X-GitHub-Event")

		// (§17.4/§17.5): a `pull_request` event whose own action is
		// "closed" is the merge-gating trigger -- a STRUCTURALLY DIFFERENT
		// thing from the "labeled" manual re-trigger lane parseMention
		// already handles for this SAME event type (payload.go's own doc
		// comment on eventTypePullRequest) -- never a mention, never
		// reaching CreateOrJoin. Checked BEFORE parseMention so a closed-PR
		// event is routed here, not misparsed as (or silently ignored by)
		// the mention pipeline. cfg.SentinelFixes == nil (this package's
		// own handler_test.go, or any other minimal wiring that doesn't
		// care about this Step) falls through to the ordinary pipeline
		// unchanged, which acknowledges it as a no-op exactly like today.
		if eventType == eventTypePullRequest && cfg.SentinelFixes != nil && readPullRequestEventAction(body) == "closed" {
			dataSource := &githubMergeGateDataSource{diffFetcher: cfg.DiffFetcher, pullRequests: cfg.PullRequests, botToken: cfg.BotToken, timeouts: cfg.Timeouts}
			handlePullRequestClosed(ctx, w, body, cfg.SentinelFixes, cfg.RepoSettings, cfg.AuditLog, dataSource, notImplementedFixMerger{})
			return
		}

		// (§24.1): a `pull_request` event whose own action is
		// "synchronize" (GitHub's own name for "new commits landed on
		// this PR's head") is this feature's own second, automatic
		// re-review trigger -- a STRUCTURALLY DIFFERENT thing from the
		// "labeled" manual re-trigger lane parseMention already handles
		// for this SAME event type, and from the "closed" merge-gating
		// lane immediately above -- never a mention, never reaching
		// CreateOrJoin. Checked BEFORE parseMention for the identical
		// reason the "closed" check above already is. cfg.Timers == nil
		// (this package's own handler_test.go, or any other minimal
		// wiring that doesn't care about this Step) falls through to the
		// ordinary pipeline unchanged, which acknowledges it as a no-op
		// exactly like today.
		if eventType == eventTypePullRequest && cfg.Timers != nil && readPullRequestEventAction(body) == "synchronize" {
			handlePullRequestSynchronize(ctx, w, body, coalescer.Pool, coalescer.PRSessions, cfg.Timers, cfg.Timeouts, deliveries, deliveryID)
			return
		}

		// (§22.2): a `false positive: <reason>` capture command is
		// dispatched HERE, BEFORE parseMention -- dispatch-before-router,
		// mirroring the pull_request/"closed" merge-gating check above
		// exactly (this file's own doc comment on that check). Checked
		// only for the two comment-bearing event types parseMention would
		// otherwise interpret as a possible mention; cfg.
		// FalsePositivePatternCapture == nil (this package's own
		// handler_test.go, or any other minimal wiring that doesn't care
		// about this Step) falls through to the ordinary pipeline
		// unchanged, exactly like every other nil-safe Config field above.
		if cfg.FalsePositivePatternCapture != nil && (eventType == eventTypeIssueComment || eventType == eventTypePullRequestReviewComment) {
			switch tryCaptureFalsePositivePattern(ctx, logger, coalescer.Identities, coalescer.Users, coalescer.AuditLog, cfg.FalsePositivePatternCapture, eventType, body) {
			case falsePositiveHandled:
				w.WriteHeader(http.StatusOK)
				return
			case falsePositiveFailed:
				// Mirrors parseMention's own error-path precedent below:
				// this delivery was claimed but never actually acted on,
				// so release the claim so a human-triggered GitHub
				// redelivery (L4's own "manual-only" precedent) can retry
				// once the backend recovers.
				if releaseErr := deliveries.Release(ctx, githubDeliveryProvider, deliveryID); releaseErr != nil {
					logger.Error("github: release webhook delivery claim failed", "error", releaseErr, "delivery_id", deliveryID)
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			case falsePositiveNotApplicable:
				// Not a capture command -- fall through to the ordinary
				// mention pipeline below, completely unaffected.
			}
		}

		// (§26.5): an `arch recap wrong: <reason>` capture command
		// is dispatched HERE, BEFORE parseMention -- dispatch-before-
		// router, mirroring the false-positive capture check immediately
		// above exactly (archrecapcontest.go's own doc comment). Checked
		// only for the SAME two comment-bearing event types; cfg.
		// ArchRecapContestCapture == nil (this package's own
		// handler_test.go, or any other minimal wiring that doesn't care
		// about this Step) falls through to the ordinary pipeline
		// unchanged, exactly like every other nil-safe Config field above.
		if cfg.ArchRecapContestCapture != nil && (eventType == eventTypeIssueComment || eventType == eventTypePullRequestReviewComment) {
			switch tryCaptureArchRecapContest(ctx, logger, coalescer.Identities, coalescer.Users, coalescer.AuditLog, cfg.ArchRecapVerdicts, cfg.ArchRecapContestCapture, eventType, body) {
			case archRecapContestHandled:
				w.WriteHeader(http.StatusOK)
				return
			case archRecapContestFailed:
				// Mirrors the false-positive capture's own identical
				// error-path precedent immediately above.
				if releaseErr := deliveries.Release(ctx, githubDeliveryProvider, deliveryID); releaseErr != nil {
					logger.Error("github: release webhook delivery claim failed", "error", releaseErr, "delivery_id", deliveryID)
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			case archRecapContestNotApplicable:
				// Not a capture command -- fall through to the ordinary
				// mention pipeline below, completely unaffected.
			}
		}

		m, ok, err := parseMention(eventType, body, mentionRE, cfg.ReReviewLabel)
		if err != nil {
			logger.Error("github: parse webhook payload failed", "error", err, "event_type", eventType, "delivery_id", deliveryID)
			// This delivery was claimed above but never actually processed --
			// release the claim so that IF a human notices this failure and
			// manually redelivers this same X-GitHub-Delivery id (L4 audit
			// fix: via GitHub's own "Redeliver" UI/API action -- GitHub does
			// NOT automatically retry/redeliver on a non-2xx response or a
			// timeout, corrected from this comment's own previous, factually
			// wrong claim that it does), that redelivery can actually
			// reprocess it rather than being silently skipped forever as an
			// already-claimed duplicate.
			if releaseErr := deliveries.Release(ctx, githubDeliveryProvider, deliveryID); releaseErr != nil {
				logger.Error("github: release webhook delivery claim failed", "error", releaseErr, "delivery_id", deliveryID)
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !ok {
			// Not an event type this adapter acts on, not a PR comment, a
			// non-"created" comment action, or the bot wasn't mentioned --
			// acknowledge, nothing to do.
			w.WriteHeader(http.StatusOK)
			return
		}

		// M14 audit fix (completeness, self-comment filter): a comment
		// authored by the bot's OWN GitHub identity must never be treated as
		// a fresh mention-worthy event. Without this, the bot's own posted
		// comment (githubapi.Adapter's async turn-outcome notification back
		// to this SAME PR, wired via GitHubBotToken) could itself satisfy
		// compileMentionPattern -- e.g. quoting or echoing the handle back --
		// and re-trigger mention detection, a bot-replies-to-its-own-comment
		// loop this filter closes. Checked as early as possible (before the
		// resolveIssueCommentHead network call and before actor
		// resolution/CreateOrJoin below), so a self-authored comment does
		// none of that work either.
		//
		// cfg.BotHandle (m.CommenterLogin's own comparison target) is the
		// best available signal for "is this the bot" today -- the SAME
		// configured handle mention-detection itself already matches comment
		// bodies against (Config.BotHandle's own doc comment). internal/
		// platform/config.go's own GitHubBotToken doc comment describes that
		// credential as "a real GitHub personal access token or a GitHub App
		// installation token, whichever the deploying operator provisions"
		// -- so this filter must recognize BOTH realistic shapes a
		// comment.user.login can take for that SAME configured bot: a plain
		// PAT-authenticated bot account, whose own login matches BotHandle
		// exactly, and a GitHub App installation, which always posts its own
		// comments under the fixed, well-known "<slug>[bot]" login form
		// (e.g. "narvi-bot[bot]" for a configured handle of "narvi-bot" --
		// this is a standard GitHub convention, not a vague edge case).
		// Matching both closes the gap this comment used to document as an
		// unresolved, known limitation: a GitHub App's own turn-outcome
		// comment on its own PR is now correctly filtered, not
		// mistaken for a fresh mention. Two genuine residual gaps remain,
		// both accepted as out of this minimal filter's scope:
		//  - Under-inclusion: if the deploying operator ever reconfigures
		//    BotHandle to a value that no longer matches the ACTUAL identity
		//    posting comments (the PAT account renamed, or the GitHub App
		//    installed under a different slug than the newly configured
		//    handle), this filter -- keyed entirely off BotHandle -- would
		//    silently stop recognizing that identity's own comments as
		//    self-comments. Closing that fully would need this codebase to
		//    independently discover/verify the bot's own real login (e.g. a
		//    GET /user call against GitHubBotToken at startup).
		//  - Over-inclusion (the "[bot]" branch specifically): GitHub App
		//    slugs are globally unique, but nothing stops an unrelated,
		//    independently-installed third-party App from happening to share
		//    this deployment's configured BotHandle string -- that app's own
		//    genuine comment on the same PR would then also match and be
		//    silently dropped, never spawning a session/turn. Low likelihood
		//    (requires that exact slug coincidence) and not a new class of
		//    risk (the plain-BotHandle-equality branch already accepts the
		//    analogous risk for a human/org account named exactly
		//    BotHandle), but worth naming alongside the drift gap above.
		// Compared case-insensitively, mirroring compileMentionPattern's own
		// case-insensitive ("(?i)") mention matching.
		if m.CommenterLogin != "" && cfg.BotHandle != "" &&
			(strings.EqualFold(m.CommenterLogin, cfg.BotHandle) || strings.EqualFold(m.CommenterLogin, cfg.BotHandle+"[bot]")) {
			w.WriteHeader(http.StatusOK)
			return
		}

		// H5 audit fix (batch fix/audit-github-pr-payload-correctness):
		// issue_comment's own payload never carries the PR's real head
		// branch/repo directly (see issueCommentPayload's own doc comment)
		// -- resolve it here, via one authenticated GitHub API call, BEFORE
		// m is turned into the session's own repo spec below.
		// pull_request_review_comment already carries the real head
		// branch/repo straight out of parseMention, so this is never
		// called for that event type. Bounded by its own timeout (never
		// the bare, deadline-free ctx) -- see Config.Timeouts' own doc
		// comment for why; resolveIssueCommentHead itself never fails this
		// request, only logs and falls back.
		if eventType == eventTypeIssueComment {
			resolveCtx, cancel := context.WithTimeout(ctx, cfg.Timeouts.GitHubGetPRTimeout)
			m = resolveIssueCommentHead(resolveCtx, logger, cfg.PullRequests, cfg.BotToken, m)
			cancel()
		}

		// mentionText (audit fix, §5.2/§18.5) is the mention's own ORIGINAL,
		// un-enriched comment/label text -- captured BEFORE the pre-fetched
		// diff/stack context below is folded into m.CommentBody, and passed
		// through to coalescer.CreateOrJoin as its own, separate classifyText
		// argument. Without this, §8.3's own intent classifier call
		// (coalesce.go, WINNER path) would classify the FULL diff-enriched
		// turn prompt instead of just the human's own words the moment a
		// diff fetcher is wired -- see CreateOrJoin's own doc comment on its
		// classifyText parameter for the full "why".
		mentionText := m.CommentBody

		// §8.2 ("review sessions", §8.2): fold this PR's own inline
		// pre-fetched diff (and, when present, its GitHub-native stack
		// context, §17.6) into the turn's own prompt text -- BEFORE
		// coalescer.CreateOrJoin, so this happens identically for BOTH the
		// WINNER (brand-new session) and REUSE (existing session) branches
		// without either of coalesce.go's own two code paths needing to
		// know about it (see this file's own doc comment on why the fetch
		// itself must never run with a Postgres transaction open -- it
		// doesn't: CreateOrJoin has not been called yet at this point).
		// Nil-safe: cfg.DiffFetcher == nil (this package's own handler_test.go,
		// or any other minimal wiring that doesn't care about this Step)
		// simply skips the fetch entirely, leaving m.CommentBody as the
		// turn's own prompt verbatim -- today's pre-existing behavior.
		// (§22.1)/(§22.1.2 retirement, this Step): prepend
		// this PR's own already-answered facts BEFORE the diff/stack/
		// tool-instructions blocks below -- prepended to, never replacing,
		// the mention's own prose text captured as mentionText above. This
		// call is DELIBERATELY placed AFTER prCtx is resolved below (moved
		// by this Step; it used to sit here, immediately after the
		// false-positive-patterns block) so it can pass prCtx.ChangedPaths
		// through to FetchAlreadyAnswered for §22.1.2's own out-of-diff
		// retirement check -- see that function's own doc comment for why
		// reordering it after the diff fetch changes nothing about
		// correctness (neither read depends on the other) while still
		// producing byte-for-byte the SAME final prompt text order as
		// before this Step: the "m.CommentBody = X + m.CommentBody"
		// prepend idiom used throughout this handler means the LAST block
		// prepended ends up FIRST in the rendered prompt, so moving this
		// call later in EXECUTION order, while keeping it the LAST prepend
		// before RenderTurnPrompt, preserves its own existing FIRST
		// position in the text exactly.
		// (§22.3): prepend this repo's own currently-active
		// learned false-positive patterns FIRST (broadest, repo-wide
		// context), before the PR-specific already-answered facts below --
		// "injected into every review pass, first pass and re-review
		// alike" applies to every mention this handler ever processes,
		// coalesced or not, since both CreateOrJoin branches share this
		// SAME prompt-building code, upstream of the branch itself.
		if cfg.FalsePositivePatterns != nil {
			if advisory := reviewcontext.FetchFalsePositivePatterns(ctx, logger, cfg.FalsePositivePatterns, m.RepoFullName); advisory != "" {
				m.CommentBody = advisory + m.CommentBody
			}
		}
		// fetchedHeadSHA is
		// captured OUTSIDE the block below so it survives to the
		// CreateOrJoin call further down, which threads it onto the
		// turn this mention actually creates/joins (turns.
		// review_head_sha) -- see CreateOrJoin's own doc comment
		// (coalesce.go) for the full "why" this is no longer a
		// post-CreateOrJoin best-effort write to a shared
		// github_pr_sessions column the way it was before this fix.
		//
		// m.HeadSHA (this mention's own webhook-payload-sourced head SHA,
		// when the triggering event carries one -- payload.go/
		// headresolve.go) is deliberately NOT consulted here anymore:
		// reviewcontext.Fetch now unconditionally resolves its own
		// HeadSHA fresh, immediately before pinning the diff fetch to it
		// (that function's own doc comment explains why a webhook-sourced
		// value is no longer trustworthy for that purpose) -- m.HeadSHA
		// itself is left fully populated by its own existing parsing
		// logic regardless, simply unread on this one path now.
		// prCtx (§26.3) is hoisted to this outer scope -- unlike
		// fetchedHeadSHA (its own pre-existing identical hoist, one line
		// below), the WHOLE struct is needed further down, to compute this
		// mention's own light/deep triage decision BEFORE rendering the
		// prompt (see this block's own doc comment immediately below).
		// review.PreFetchedContext{} (every field its own honest zero
		// value) is exactly what a nil cfg.DiffFetcher, or a repo_full_name
		// that fails to split, degrades to -- internal/app/reviewtriage.
		// ComputeDecision's own fail-open posture already treats an
		// all-zero Signals as "route light", so no special-casing is
		// needed here for either branch below.
		var fetchedHeadSHA string
		var prCtx review.PreFetchedContext
		havePrCtx := false
		if cfg.DiffFetcher != nil {
			if owner, repo, ok := reposource.SplitFullName(m.RepoFullName); ok {
				prCtx = reviewcontext.Fetch(ctx, logger, cfg.DiffFetcher, cfg.Timeouts, owner, repo, m.PRNumber, cfg.BotToken, m.Stack)
				havePrCtx = true
				fetchedHeadSHA = prCtx.HeadSHA
			} else {
				logger.Warn("github: could not split repo_full_name into owner/repo, skipping pre-fetched review context",
					"repo_full_name", m.RepoFullName, "pr_number", m.PRNumber)
			}
		}

		// (§26.3): the depth decision, computed from prCtx above.
		// Adversarial-review fix D2 ("deep-path digest requirement
		// contradicts the prompt the agent actually receives"): this MUST
		// run, and be floored (D1, immediately below), BEFORE
		// review.RenderTurnPrompt renders m.CommentBody -- prCtx.DeepPath
		// (set right below) is the ONE fact that makes RenderTurnPrompt's
		// own verdictToolInstructions tell the agent the truth about
		// whether digest.archDecisions/stackRisks/unverifiedLimits are
		// REQUIRED for THIS turn. This used to run INSIDE coalescer.
		// CreateOrJoin, called by this handler AFTER the prompt was
		// already rendered -- moved here, into this handler, so the
		// SAME depth value both informs the prompt text and gets
		// persisted onto the resulting turn, never two independently-
		// computed values that could disagree. ComputeDecision never
		// errors (its own doc comment) -- there is no failure path here
		// to propagate.
		//
		// D1 (adversarial-review fix, "re-review depth floor applied at
		// only 1 of 3 lanes"): this handler's own REUSE-branch turns
		// (coalescer.CreateOrJoin's own second-mention/label-retrigger
		// path) used to feed the FRESH, unfloored decision straight
		// through, never applying §24's re-review floor at all --
		// applied here, uniformly, for BOTH the WINNER and REUSE branches
		// CreateOrJoin may end up taking (which one is only decided
		// further inside CreateOrJoin's own claim-row transaction, not
		// knowable yet at this point in the handler) -- this is safe
		// because a WINNER (brand-new github_pr_sessions claim row) can
		// never have a real prior review_verdicts row to floor against in
		// practice (posting a verdict always requires a turn created via
		// an EXISTING claim row first), so priorReviewDepth is always ""
		// on that branch and Floor(fresh, "") is a no-op by construction
		// -- see CreateOrJoin's own doc comment (coalesce.go) for the
		// full "why" this uniform application is equivalent to a
		// REUSE-only floor in every reachable state.
		//
		// D9 (adversarial-review fix): skip the floor entirely when the
		// fresh decision's own Reason is ReasonAlwaysLightConfig -- an
		// explicit admin reviewDepth.mode=always_light override outranks
		// this history-based floor, mirroring internal/app/sessionactor/
		// reviewretrigger.go's own identical guard (see
		// domainreviewtriage.Floor's own doc comment, depth.go, for the
		// full "why").
		triageDecision, triageConfig, priorReviewDepth := appreviewtriage.ComputeDecision(ctx, coalescer.ReviewTriage, m.RepoFullName, m.PRNumber, prCtx)
		triageProvenance := appreviewtriage.ResolveProvenance(ctx, coalescer.ReviewTriage, m.RepoFullName, m.PRNumber)
		flooredDepth := triageDecision.Depth
		if triageDecision.Reason != domainreviewtriage.ReasonAlwaysLightConfig {
			flooredDepth = domainreviewtriage.Floor(triageDecision.Depth, priorReviewDepth)
		}
		prCtx.DeepPath = flooredDepth == domainreviewtriage.DepthDeep
		// ReviewCostBudgetUSD (§26.7): the SAME triageConfig
		// ComputeDecision already resolved, above -- no second
		// repo_settings read, mirroring internal/adapters/inbound/httpapi/
		// reviewretrigger.go's own identical addition.
		prCtx.ReviewCostBudgetUSD = triageConfig.CostBudget.ForDepth(flooredDepth)
		// B5 fix: threads reviewtriage.CostBudgetSafetyMargin through as a
		// whole percentage -- review.PreFetchedContext.
		// CostBudgetSafetyMarginPercent's own doc comment for why this
		// package (which already imports reviewtriage) is the one that
		// must set it, never review itself (doc.go's own "zero external
		// imports" convention).
		prCtx.CostBudgetSafetyMarginPercent = int(domainreviewtriage.CostBudgetSafetyMargin * 100)
		// (§22.1)/(§22.1.2 retirement): see this block's
		// own earlier comment (where it used to sit, right after the
		// false-positive-patterns block) for why it moved here --
		// prCtx.ChangedPaths is only known now, after the diff fetch
		// above, and §22.1.2's retirement check needs it. This is still
		// the LAST block prepended before RenderTurnPrompt below, so the
		// final prompt text order is unchanged from before this Step.
		if cfg.ReviewFindings != nil {
			if alreadyAnswered := reviewcontext.FetchAlreadyAnswered(ctx, logger, cfg.ReviewFindings, m.RepoFullName, m.PRNumber, prCtx.ChangedPaths, prCtx.DiffTruncated); alreadyAnswered != "" {
				m.CommentBody = alreadyAnswered + m.CommentBody
			}
		}
		if havePrCtx {
			m.CommentBody = review.RenderTurnPrompt(m.CommentBody, prCtx)
		}
		reviewDepthStr := string(flooredDepth)
		triageModelID, triageEffort := domainreviewtriage.ModelAndEffort(flooredDepth, coalescer.ReviewModelDeep)
		// D4 (nice-to-have adversarial-review fix): see internal/adapters/
		// inbound/httpapi/reviewretrigger.go's own identical log line for
		// the full "why" -- an operator otherwise has no signal that a
		// deep-routed turn's model-tier override is silently inert.
		if flooredDepth == domainreviewtriage.DepthDeep && coalescer.ReviewModelDeep == "" {
			logger.Info("github: review routed deep but no deep-tier model configured (NARVI_REVIEW_MODEL_DEEP unset), dispatching with the default model at forced high effort", "repo", m.RepoFullName, "pr_number", m.PRNumber)
		}
		// (§31.6): this turn's own server-derived knowledge-gate
		// key -- the SAME deterministic classifiers the gate query itself
		// (a later Step) overlaps against, computed here (never from
		// anything a reviewing model posts) and carried onto
		// triageRecordJSON below so it survives to verdict-post time --
		// see reviewtriage.DecisionRecord.ArchDecisionTags/ArchDecisionRoots'
		// own doc comment for the full "why this carrier, not a new
		// column" reasoning.
		archDecisionTags := autoapproval.TagStrings(autoapproval.ClassifyChangedPaths(prCtx.ChangedPaths))
		archDecisionRoots := autoapproval.ClassifyChangedRoots(prCtx.ChangedPaths)
		triageRecordJSON, triageRecordErr := json.Marshal(domainreviewtriage.NewDecisionRecord(triageDecision, triageConfig, flooredDepth, triageProvenance, triageModelID, triageEffort, prCtx.ChangedFilesCount, prCtx.Diff == "", prCtx.DiffTruncated, archDecisionTags, archDecisionRoots))
		if triageRecordErr != nil {
			logger.Warn("github: marshal review-depth decision record failed, turn will carry review_depth but no review_depth_decision", "error", triageRecordErr, "repo", m.RepoFullName, "pr_number", m.PRNumber)
			triageRecordJSON = nil
		}

		req := restdtos.CreateSessionRequest{
			SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
			Prompt:      restdtos.CreateSessionRequestPrompt(&m.CommentBody),
			Repos: []restdtos.CreateSessionRequestReposElem{
				{
					Name:   m.RepoName,
					Url:    m.RepoCloneURL,
					Branch: restdtos.CreateSessionRequestReposElemBranch(m.HeadBranch),
				},
			},
		}

		// Batch fix/audit-github-actor-rbac's own addition (the H4 audit
		// finding this closes, see identity.go's own top doc comment):
		// resolve the real commenter behind m.CommenterID to a Narvi
		// user_id, ONCE, regardless of which CreateOrJoin branch this
		// mention ends up taking -- a direct identities lookup, never an
		// auto-linking algorithm (unlike Slack/Linear). actor stays invalid
		// for a commenter who has genuinely never signed into Narvi --
		// batch fix/deny-unlinked-github-actors means that invalid actor
		// is now DENIED by coalesce.go's own AuthorizeLinkedActor gates,
		// not bot-attributed allow-and-proceed (see this branch's own
		// ErrActorNotAuthorized handling below).
		//
		// err here is DISTINCT from "genuinely never linked" (a confirmed
		// audit finding on an earlier version of this batch: the two used
		// to be conflated into the same invalid pgtype.UUID{}, so a
		// transient identities-lookup error on an ALREADY-linked, fully-
		// authorized commenter -- e.g. a DB connection reset -- silently
		// became a permanent, wrongly-worded "I don't recognize your
		// GitHub account" denial for that real user). A non-nil err here
		// means resolveCommenterActor's own lookup itself failed for a
		// reason that says NOTHING about whether this commenter is
		// linked -- treated exactly like every other genuine backend
		// failure in this handler (parseMention's own error path above,
		// the generic CreateOrJoin failure path below): release the claim
		// so a real GitHub redelivery can retry once the backend recovers,
		// and never post the (potentially false) "please sign in" reply.
		actor, err := resolveCommenterActor(ctx, coalescer.Identities, m.CommenterID)
		if err != nil {
			logger.Error("github: resolve commenter identity failed", "error", err, "repo", m.RepoFullName, "pr_number", m.PRNumber)
			if releaseErr := deliveries.Release(ctx, githubDeliveryProvider, deliveryID); releaseErr != nil {
				logger.Error("github: release webhook delivery claim failed", "error", releaseErr, "delivery_id", deliveryID)
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		session, turn, isNew, err := coalescer.CreateOrJoin(ctx, m.RepoFullName, m.PRNumber, req, actor, m.IsLabelRetrigger, mentionText, fetchedHeadSHA, &reviewDepthStr, triageModelID, triageEffort, triageRecordJSON)
		if err != nil {
			if errors.Is(err, ErrActorNotAuthorized) {
				// ErrActorNotAuthorized fires for TWO distinct reasons
				// (coalesce.go already logged which, specifically) --
				// batch fix/deny-unlinked-github-actors' own addition
				// distinguishes them here using the SAME already-resolved
				// actor value passed into CreateOrJoin above, never a
				// second lookup:
				//
				//  - !actor.Valid: an UNLINKED commenter -- this batch's
				//    own repo-owner-decided hardening (aligning GitHub
				//    with Slack/Linear, see coalesce.go's own updated doc
				//    comment) now denies this instead of the pre-batch
				//    bot-attributed allow. Unlike a linked-but-denied
				//    actor below, this commenter has had NOTHING happen
				//    for them at all and no other channel to learn why --
				//    GitHub has no magic-link/pending-link mechanism to
				//    fall back on (identity.go's own top doc comment) --
				//    so this is the one case that gets an actionable
				//    reply, gated by claimActorNotAuthorizedNotice's own
				//    atomic anti-spam dedupe so a repeat mention within
				//    GitHubActorNoticeTTL doesn't spam the thread again
				//    (and so concurrent deliveries of the SAME still-
				//    unlinked commenter's mention can't both slip past the
				//    dedupe check and both post -- see that function's own
				//    doc comment, actornotauthorizedreply.go).
				//  - actor.Valid: a LINKED commenter whose role failed
				//    domain/authz.Authorize (e.g. a viewer denied
				//    ActionCreateSession, or a non-owning member denied
				//    ActionPromptSession) -- pre-existing, out of this
				//    batch's own scope, so this keeps today's silent-200
				//    behavior completely unchanged: that commenter already
				//    knows they're signed in, and "sign in via GitHub
				//    OAuth" would be confusing and wrong to tell them.
				//
				// Either way, acknowledge without releasing the claim:
				// unlike a transient failure, retrying a genuine
				// redelivery of the SAME comment would render the exact
				// same denial again, so there is nothing a GitHub retry
				// could fix here.
				if !actor.Valid {
					if claimActorNotAuthorizedNotice(ctx, logger, cfg.LinkNotices, cfg.Timeouts.GitHubActorNoticeTTL, m.RepoFullName, m.PRNumber, m.CommenterID) {
						postActorNotAuthorizedReply(ctx, logger, cfg.Comments, cfg.PublicBaseURL, cfg.BotToken, m.RepoFullName, m.PRNumber, m.CommenterID)
					}
				}
				w.WriteHeader(http.StatusOK)
				return
			}
			if errors.Is(err, ErrRolloutNotEnrolled) {
				// §10's own permanent-denial idiom (§10 Phase 6, §32):
				// this repo is not enrolled in the cohort rollout --
				// coalesce.go's own ErrRolloutNotEnrolled doc comment for
				// the full "why". Mirrors ErrActorNotAuthorized's own
				// "acknowledge without releasing the claim" shape
				// immediately above (retrying an identical GitHub
				// redelivery would reproduce this exact same refusal every
				// time, since repo_settings.sessions_enabled does not
				// change between redeliveries of the SAME webhook
				// payload) -- but, UNLIKE that branch, posts NO reply at
				// all: §32's own "an unenrolled repo must receive zero
				// platform egress" requirement. An unenrolled repo has no
				// action a commenter could take to fix this themselves
				// (unlike an unlinked actor, who can sign in), so there is
				// nothing honest to tell them beyond silence.
				logger.Info("github: mention refused: repo not enrolled in cohort rollout", "repo", m.RepoFullName, "pr_number", m.PRNumber)
				w.WriteHeader(http.StatusOK)
				return
			}
			if errors.Is(err, ErrRepoEntitlementDenied) {
				// §31.4's own permanent-denial idiom (Defect-2 audit fix):
				// this repo failed the per-repository entitlement
				// predicate -- coalesce.go's own ErrRepoEntitlementDenied
				// doc comment for the full "why". Mirrors
				// ErrRolloutNotEnrolled's own "acknowledge without
				// releasing the claim, post no reply" shape immediately
				// above in every respect, including reachability: today
				// this branch can never actually fire (the WINNER path's
				// own spawn source is always exempt from this predicate),
				// but is kept for the same defensive-symmetry reason
				// ErrRolloutNotEnrolled's own check is.
				logger.Info("github: mention refused: repo not entitled", "repo", m.RepoFullName, "pr_number", m.PRNumber)
				w.WriteHeader(http.StatusOK)
				return
			}
			if errors.Is(err, httpapi.ErrPlanAwaitingApproval) {
				// a follow-up fix (Finding 1): the session's plan
				// is currently awaiting approval, so the REUSE path's own
				// httpapi.CreateTurnForBot call (coalesce.go) declined to
				// enqueue an ordinary build turn -- a deterministic,
				// expected business state, not a transient failure. Mirrors
				// ErrActorNotAuthorized's own "no release, retrying changes
				// nothing" reasoning exactly: the awaiting-plan condition
				// persists until a human decides (elsewhere -- see
				// planAwaitingApprovalReplyText's own doc comment), so
				// releasing the claim for a GitHub redelivery retry would
				// only ever reproduce this SAME outcome, forever, as a
				// self-inflicted retry storm. Unlike ErrActorNotAuthorized,
				// this ALSO posts an honest reply on the PR thread --
				// today's PRE-fix behavior left the thread with no reply at
				// all, unlike Slack/Linear's own equivalent honest replies
				// for this exact sentinel.
				logger.Info("github: mention blocked by awaiting-approval plan", "repo", m.RepoFullName, "pr_number", m.PRNumber)
				postPlanAwaitingReply(ctx, logger, cfg.Comments, cfg.BotToken, m.RepoFullName, m.PRNumber)
				w.WriteHeader(http.StatusOK)
				return
			}
			logger.Error("github: create-or-join session failed", "error", err, "repo", m.RepoFullName, "pr_number", m.PRNumber)
			// Same reasoning as the parseMention error path above -- this
			// delivery was claimed but never actually acted on, so release
			// the claim to let a genuine redelivery retry.
			if releaseErr := deliveries.Release(ctx, githubDeliveryProvider, deliveryID); releaseErr != nil {
				logger.Error("github: release webhook delivery claim failed", "error", releaseErr, "delivery_id", deliveryID)
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// fetchedHeadSHA is
		// already persisted onto the turn CreateOrJoin just created/joined
		// (turns.review_head_sha, set atomically as part of that turn's
		// own INSERT) -- no separate post-hoc write needed here anymore;
		// the PREVIOUS best-effort github_pr_sessions.pending_head_sha
		// write this comment used to describe is REMOVED by this fix (see
		// migrations/000072_turns_review_head_sha.up.sql's own doc
		// comment for the full "why" a shared, mutable per-(repo,PR)
		// column was the wrong place for this fact).

		// §15 ("release PR review", §15): only the WINNER (brand-new
		// session) path ever triggers release detection/the manifest check
		// -- see triggerReleaseManifestCheckBestEffort's own doc comment
		// (releasemanifest.go) for the full "why". Runs AFTER the mention
		// itself is fully processed and best-effort throughout: a failure
		// here never turns an already-successful mention into a failed
		// webhook response.
		if isNew {
			triggerReleaseManifestCheckBestEffort(ctx, logger, cfg, m.RepoFullName, m.PRNumber, session.ID)
		}

		logger.Info("github: mention processed",
			"session_id", session.ID, "turn_id", turn.ID, "new_session", isNew,
			"repo", m.RepoFullName, "pr_number", m.PRNumber, "event_type", eventType)
		w.WriteHeader(http.StatusOK)
	}
}
