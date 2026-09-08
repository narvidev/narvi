package sessionactor

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	"github.com/narvidev/narvi/internal/platform"
)

// meterName is this package's own OTel meter name (§14.3, "mocking +
// contract drift" -- the contract_drift_detected counter, below, is the
// first metric this package registers) -- mirrors app/reconciler's and
// app/imagebuild's own "narvi/<package>" convention exactly.
const meterName = "narvi/sessionactor"

// storeBundle bundles the store handles this package's Registry and every
// Actor it hydrates share -- built once in NewRegistry, then referenced
// (never rebuilt) everywhere else. Each store is pool-scoped by default;
// callers needing a transaction-scoped one call its WithTx method (see
// actor.go's transact and timerpump.go's claimDueTimers).
type storeBundle struct {
	session *postgres.SessionStore
	turn    *postgres.TurnStore
	sandbox *postgres.SandboxStore
	timer   *postgres.TimerStore
	event   *postgres.EventStore

	// identity and artifact are §9.3's ("e2e happy path") own additions
	// -- both used only by pushpr.go's createPRBestEffort: identity to
	// decrypt the session's own creator's GitHub OAuth access token,
	// artifact to record a successfully created PR as an artifact row (the
	// ONLY durable place a PR's URL is written -- see that file's own doc
	// comment for why this is what makes it visible to a client at all,
	// with no new wire contract needed).
	identity *postgres.IdentityStore
	artifact *postgres.ArtifactStore

	// user is §13.2's ("identities + full RBAC", §13.3) own addition --
	// pushpr.go's own createPRBestEffort uses it for the viewer guard
	// ("viewers never gain PR-reviewer attribution or git identity on
	// session artifacts"): a defense-in-depth check of the session
	// creator's CURRENT role, distinct from (and in addition to)
	// domain/authz.Authorize already refusing a viewer at session-creation
	// time (httpapi.CreateSession) -- this second check catches a role
	// downgraded to viewer AFTER a session was already created by a
	// non-viewer, which Authorize's own create-time check cannot.
	user *postgres.UserStore

	// imageBuild is §8.5's ("image builds") own addition -- used by
	// dispatch.go/imageresolve.go's resolveAndSetImage to look up an
	// already-built image by fingerprint, and to best-effort upsert a
	// pending tracking row on any miss (internal/app/imagebuild's own
	// background loop is this table's OTHER writer, via its own
	// independently-constructed *postgres.ImageBuildStore -- see
	// cmd/control-plane/main.go).
	imageBuild *postgres.ImageBuildStore

	// environment and contractDrift are §14.3's ("mocking + contract
	// drift") own additions, mirroring imageBuild's own addition for §8.5
	// exactly: environment is used by dispatch.go/contractdrift.go's
	// checkContractDrift to read a spawn/restore plan's own Environment
	// row (MockConfigured, ContractsPath) back by id; contractDrift reads/
	// best-effort-upserts the per-repo contract_drift_snapshots row that
	// same function compares against.
	environment   *postgres.EnvironmentStore
	contractDrift *postgres.ContractDriftStore

	// outbox, slackThreadSession, githubPRSession, and linearAgentSession
	// are §5.1's ("outbox delivery", §5.1) own additions: outbox is
	// where pushpr.go's own completeProcessingTurn writes exactly one
	// notification row per non-'web'-origin session's turn completion,
	// inside that SAME transaction (§5.1: "written in the same tx as the
	// state change"); the other three are the REVERSE (session_id ->
	// origin-channel-address) lookups that write needs to know WHERE to
	// route that notification -- each mirrors imageBuild/environment's own
	// identical "added by the Step that first needs it" precedent.
	outbox *postgres.OutboxStore
	// shadowLedgerStore is the CONCRETE store, held alongside the Actor's
	// shadowledger.Store handle, because the suppressed-push record must
	// join completeProcessingTurn's own transaction (§30.6 record-or-fail)
	// and only the concrete store exposes WithTx -- the same reason
	// outbox above is concrete rather than a port.
	shadowLedgerStore  *postgres.ShadowSCMWriteStore
	slackThreadSession *postgres.SlackThreadSessionStore
	githubPRSession    *postgres.GitHubPRSessionStore
	linearAgentSession *postgres.LinearAgentSessionStore

	// plan is §8.1's ("plan mode, web", §8.1/§12.2 item 3) own
	// addition: pushpr.go's own completeProcessingTurn calls
	// recordPlanIfNeeded (planrecord.go) right after persisting a turn's
	// terminal state, inside that SAME transaction, exactly mirroring
	// outbox's own precedent above.
	plan *postgres.PlanStore

	// auditLog is an audit-fix batch's own addition (completeness/
	// observability finding against planrecord.go): recordPlanIfNeeded now
	// writes a "plan.superseded" audit_log row, in the SAME transaction, for
	// every prior awaiting_approval plan a new plan-mode turn's completion
	// supersedes -- mirroring httpapi.DecidePlanOnTx's own identical
	// "audit_log written in the same tx as the change" precedent (§13.3) for
	// a real human/bot decision, now extended to this automatic,
	// system-triggered transition too.
	auditLog *postgres.AuditLogStore

	// sentinelFix/reviewFinding are §8.2's ("sentinels + suggestions",
	// §17) own additions -- pushpr.go's own createSentinelFixPRBestEffort
	// reads sentinelFix (by this Actor's own session id, the FIX session)
	// to learn the origin PR's own head branch/number, then writes both
	// stores back once the fix PR actually opens.
	sentinelFix   *postgres.SentinelFixStore
	reviewFinding *postgres.ReviewFindingStore

	// handoffSentinelRuns is §14.4's ("handoff-readiness sentinel",
	// §14.4) own addition -- pushpr.go's own createPRBestEffort (via
	// handoffsentinel.go's runHandoffSentinelBestEffort) claims a row here,
	// in the SAME transaction as its own outbox enqueue, exactly mirroring
	// sentinelFix's own claim-before-outbox-enqueue precedent above.
	handoffSentinelRuns *postgres.HandoffSentinelStore

	// workflow is §25.6's ("workflow execution engine", §25.6) own
	// addition: pushpr.go's completeProcessingTurn, timerfired.go's
	// handleTurnDeadlineTimer, and dispatch.go's failDispatchedTurn each
	// call internal/app/workflowengine.OnTurnCompleted with this store
	// (WithTx'd onto their own already-open transact) the moment a turn
	// reaches a real terminal state -- see that package's own doc.go for
	// why all three call sites matter, not just the first.
	workflow *postgres.WorkflowStore

	// repoSettings is §4.1's ("RWX provider + previews", §4.1.2 point
	// 1) own addition -- pushpr.go's own enqueuePreviewBestEffort (called
	// from createPRBestEffort, the ONE enqueue point) reads it, per
	// pushed repo, to decide whether that repo's RWX preview setting
	// ({dispatchKey, endpointTemplate, orgSlug}) is present -- a plain,
	// pool-scoped read (never WithTx: a config lookup gating a LATER
	// fresh transact, not itself part of any state-writing transaction),
	// mirroring how createPRBestEffort already reads sessionRow via a
	// plain a.stores.session.Get before ever opening one.
	repoSettings *postgres.RepoSettingsStore

	// reviewVerdict is §24's ("review: automatic re-review on new
	// commits", §24.3) own addition -- handleReviewRetriggerDebounceTimer
	// (reviewretrigger.go) reads GetLatest to compare this PR's own
	// latest posted verdict head_sha against pending_retrigger_head_sha,
	// the same GetLatestReviewVerdict/DISTINCT-ON-per-PR reduction §21.1
	// already defines and every other caller of "the latest verdict for
	// this PR" (the auto-approval eligibility engine, the decision inbox)
	// already reuses -- never a second, independently-derived reduction.
	reviewVerdict *postgres.ReviewVerdictStore

	// falsePositivePattern is a rereview fix (finding 1) own
	// addition: handleReviewRetriggerDebounceTimer's own phase 2
	// (composeAutoRetriggerPrompt, reviewretrigger.go) calls
	// reviewcontext.FetchFalsePositivePatterns with this store, exactly
	// like httpapi.RetriggerReview's own manual-button lane and
	// internal/adapters/inbound/github/handler.go's own mention/label
	// lane already do -- before this fix, the automatic lane was the
	// ONLY review-turn producer in this codebase that never prepended
	// §22.3's own learned false-positive advisory block.
	falsePositivePattern *postgres.FalsePositivePatternStore

	// providerCredential is a B2 fix (adversarial review of §26.4)
	// own addition: sessionconfig.go's own reviewCredentialedProviders
	// (called from reviewCounterReviewerModel) reads it to learn which of
	// counterReviewerProviderPreference's fixed 3 providers this SESSION
	// actually has a resolvable credential for, before ever pinning the
	// counter-reviewer sub-task to one -- "prefer no pin over guessing when
	// the opposing provider is not known-credentialed" (existence only,
	// via ListForResolution + providercredential.Resolve; this package
	// never decrypts anything, mirroring httpapi.ProviderCredentialsDelivery's
	// own identical grouping one step earlier than that handler's own
	// decrypt call).
	providerCredential *postgres.ProviderCredentialStore
}

func newStoreBundle(pool *pgxpool.Pool, platformShadow bool) storeBundle {
	return storeBundle{
		session:              postgres.NewSessionStore(pool),
		turn:                 postgres.NewTurnStore(pool),
		sandbox:              postgres.NewSandboxStore(pool),
		timer:                postgres.NewTimerStore(pool),
		event:                postgres.NewEventStore(pool),
		identity:             postgres.NewIdentityStore(pool),
		artifact:             postgres.NewArtifactStore(pool),
		user:                 postgres.NewUserStore(pool),
		imageBuild:           postgres.NewImageBuildStore(pool),
		environment:          postgres.NewEnvironmentStore(pool),
		contractDrift:        postgres.NewContractDriftStore(pool),
		outbox:               postgres.NewOutboxStore(pool, platformShadow),
		shadowLedgerStore:    postgres.NewShadowSCMWriteStore(pool),
		slackThreadSession:   postgres.NewSlackThreadSessionStore(pool),
		githubPRSession:      postgres.NewGitHubPRSessionStore(pool),
		linearAgentSession:   postgres.NewLinearAgentSessionStore(pool),
		plan:                 postgres.NewPlanStore(pool),
		auditLog:             postgres.NewAuditLogStore(pool),
		sentinelFix:          postgres.NewSentinelFixStore(pool),
		reviewFinding:        postgres.NewReviewFindingStore(pool),
		handoffSentinelRuns:  postgres.NewHandoffSentinelStore(pool),
		workflow:             postgres.NewWorkflowStore(pool),
		repoSettings:         postgres.NewRepoSettingsStore(pool),
		reviewVerdict:        postgres.NewReviewVerdictStore(pool),
		falsePositivePattern: postgres.NewFalsePositivePatternStore(pool),
		providerCredential:   postgres.NewProviderCredentialStore(pool),
	}
}

// Registry is the process-wide supervisor of session actors (§2's "one
// goroutine + mailbox per active session", scoped to THIS process --
// other pods run their own independent Registry against the same
// Postgres). At most one live *Actor per session id exists in this
// process's actors map at any time; Registry's own mutex is what makes
// that true within a process, while the Postgres advisory lock
// (hydrateAndAcquire, hydrate.go) is what makes it true ACROSS processes.
type Registry struct {
	mu     sync.Mutex
	actors map[pgtype.UUID]*Actor

	pool     *pgxpool.Pool
	timeouts platform.Timeouts
	stores   storeBundle

	// broadcaster is threaded through to every Actor this Registry
	// hydrates (§6.2's "→ broadcast stream", made real via
	// internal/adapters/inbound/wshub's *Hub -- see
	// internal/app/ports.EventBroadcaster's own doc comment for the full
	// rationale). May be nil (some tests construct a Registry without
	// one) -- Actor.broadcastPending already guards against that.
	broadcaster ports.EventBroadcaster

	// commander, provider, and publicBaseURL are §9.3's ("e2e happy
	// path") own additions, threaded through to every Actor this Registry
	// hydrates exactly the same way broadcaster already is -- see Actor's
	// own field doc comments (actor.go) for what each is used for. All
	// three may be nil/empty (some tests, e.g. the resilience test in
	// design decision 12, never exercise the spawn/dispatch path at all).
	commander     ports.SandboxCommander
	provider      ports.SandboxProvider
	publicBaseURL string

	// sourceControl and tokenEncryptionKey are §9.3's ("e2e happy
	// path") own remaining additions, threaded through to every Actor this
	// Registry hydrates exactly the same way commander/provider already
	// are: sourceControl is the ports.SourceControl every Actor's
	// createPRBestEffort (pushpr.go) calls CreatePR on once a push_complete
	// event arrives; tokenEncryptionKey decrypts the session creator's own
	// stored identities.access_token_encrypted (§13.1) to obtain the
	// plaintext OAuth token CreatePR needs (§8.11: "PR created with the
	// prompting user's OAuth token"). Both may be nil/empty (tests that
	// never exercise the push/PR path).
	sourceControl      ports.SourceControl
	tokenEncryptionKey []byte

	// openCodeRuntimeVersion is §8.5's ("image builds") own remaining
	// addition, threaded through to every Actor this Registry hydrates
	// exactly like sourceControl/tokenEncryptionKey already are: the
	// RuntimeVersion input to domain/imagebuild.Fingerprint (dispatch.go/
	// imageresolve.go's own resolveAndSetImage), sourced from
	// platform.Config.OpenCodeRuntimeVersion. May be empty (tests that
	// never exercise the image-resolution path) -- an empty runtime
	// version still fingerprints deterministically, it just means every
	// session's own fingerprint shares that one (test-only) value.
	openCodeRuntimeVersion string

	// githubBotToken is §8.2's ("sentinels + suggestions", §17.2) own
	// addition, threaded through to every Actor this Registry hydrates
	// exactly like the fields above: pushpr.go's own
	// createSentinelFixPRBestEffort uses this SAME static bot credential
	// (platform.Config.GitHubBotToken -- the identical one internal/
	// adapters/outbound/githubapi.BotNotifier/VerdictNotifier already
	// authenticate with) to open the fix PR, since a sentinel-auto-fix
	// child session has NO human creator of its own to decrypt an OAuth
	// token FOR (sessionRow.CreatedBy is invalid/NULL, SpawnChildSession's
	// own doc comment) -- the fix PR is a system-initiated action,
	// bot-attributed by design, mirroring §17.4's own "system-initiated,
	// not a delegated human one" framing for the eventual merge. May be
	// empty (tests that never exercise the sentinel-fix PR path).
	githubBotToken string

	// reviewModelDeep is §26.3's own addition (§26.3) -- see
	// RegistryOptions.ReviewModelDeep's own doc comment.
	reviewModelDeep string

	// diffFetcher is §14.4's ("handoff-readiness sentinel", §14.4) own
	// addition, threaded through to every Actor this Registry hydrates
	// exactly like the fields above: handoffsentinel.go's own
	// runHandoffSentinelBestEffort uses it to fetch a just-created PR's own
	// diff (GetPullRequestDiff) to scan for backend-adjacent TODO/FIXME
	// markers. A narrow, locally-defined interface (PRDiffFetcher,
	// handoffsentinel.go) -- NOT ports.SourceControl -- mirrors internal/
	// app/reviewcontext.Fetcher's own identical, deliberate choice for the
	// SAME underlying capability: diff-fetching is a GitHub-specific
	// capability with no GitLab (ports.SourceControl's own still-stubbed
	// second implementation) equivalent designed anywhere in this plan, so
	// it does not belong on that port (see reviewcontext's own doc comment
	// for the full precedent this mirrors). May be nil (tests that never
	// exercise the handoff-sentinel path) -- runHandoffSentinelBestEffort
	// treats a nil diffFetcher as "no diff available", degrading to a
	// TODO-scan of nothing, never a panic.
	diffFetcher PRDiffFetcher

	// reviewDiffFetcher is §24's ("review: automatic re-review on new
	// commits", §24.3) own addition, threaded through to every Actor this
	// Registry hydrates exactly like diffFetcher above -- a DIFFERENT,
	// wider interface than PRDiffFetcher (reviewcontext.Fetcher's own
	// GetPullRequest+GetCompareDiff pair, not GetPullRequestDiff alone),
	// since handleReviewRetriggerDebounceTimer needs the SAME "diff
	// providably anchored to a live-fetched head sha" guarantee every
	// OTHER review-trigger path already gets via internal/app/
	// reviewcontext.Fetch -- see that
	// package's own doc comment. *githubapi.Adapter (the SAME instance
	// diffFetcher/sourceControl above already wire) satisfies this
	// directly, with no adapter-side change. May be nil (tests that never
	// exercise the automatic-re-review path) --
	// handleReviewRetriggerDebounceTimer treats a nil reviewDiffFetcher as
	// "no diff fetcher configured", logging and declining to enqueue a
	// turn it could not honestly anchor to a real head sha, never a
	// panic.
	reviewDiffFetcher reviewcontext.Fetcher

	// knowledgeRanker (§31.6/§34.7) orders whatever the automatic
	// re-review lane's own arch-decisions gate admits (a.stores.
	// reviewVerdict, which already satisfies reviewcontext.
	// ArchDecisionsFetcher directly -- no separate field needed for the
	// fetcher half) -- threaded through to every Actor this Registry
	// hydrates exactly like reviewDiffFetcher above. Nil is a valid,
	// common value (RegistryOptions.KnowledgeRanker's own doc comment):
	// reviewcontext.FetchPriorArchDecisions degrades a nil ranker to
	// knowledge.RecencyRanker{}, never a panic.
	knowledgeRanker ports.KnowledgeRanker

	// githubBotHandle is §24's own further addition -- the SAME
	// configured bot/app username internal/adapters/inbound/github's own
	// mention-pattern compiler already matches comment bodies against
	// (platform.Config.GitHubBotHandle) --
	// handleReviewRetriggerDebounceTimer's own budget-exhausted notice
	// (§24.6) embeds reviewpost.RerunGuidance(botHandle), the SAME
	// server-side, deterministic re-run phrasing every OTHER posted
	// verdict already carries (§5.2), which needs this handle to render
	// an "@botHandle review" mention. May be empty (tests that never
	// exercise the budget-exhausted notice path) -- RerunGuidance("")
	// still renders a (degenerate but harmless) string.
	githubBotHandle string

	// contractDriftDetected is §14.3's ("mocking + contract drift", §14.3)
	// own OTel counter, constructed exactly once here (NewRegistry), then
	// threaded through to every Actor this Registry hydrates -- mirroring
	// how every other Actor-shared field above is threaded, and mirroring
	// app/reconciler.NewReconciler's/app/imagebuild.NewBuilder's own
	// "construct the counter once, at construction time" precedent for the
	// counter itself. Incremented by dispatch.go/contractdrift.go's own
	// checkContractDrift whenever contractdrift.HasDrifted reports true for
	// a mock-configured Environment's repo.
	contractDriftDetected metric.Int64Counter

	// opsMetrics is §5.3's ("ops: dashboards, alerts, runbooks", §5.3)
	// own bundle of five OTel instruments (opsmetrics.go), constructed
	// exactly once here from the SAME meter as contractDriftDetected
	// above, then threaded through to every Actor this Registry hydrates
	// -- mirroring contractDriftDetected's own threading exactly.
	opsMetrics opsMetrics

	// repoAccessCache is the audit fix's ("warm-boot image access control",
	// HIGH) own addition: the process-wide, in-memory TTL cache backing
	// resolveAndSetImage's own repo-access gate (imageresolve.go),
	// constructed exactly once here and threaded through to every Actor
	// this Registry hydrates -- mirroring how contractDriftDetected above
	// is already threaded through. See repoaccesscache.go's own top
	// comment for why this is keyed by (user, repo), not per-session, and
	// therefore must be shared across every Actor rather than owned by
	// any one of them.
	repoAccessCache *repoAccessCache

	// epistemicCheckDefault (F6, adversarial review) is
	// platform.Config.EpistemicCheckDefault, threaded through to every
	// Actor this Registry hydrates exactly like the fields above --
	// workflowengine.Deps.EpistemicCheckDefault (each Actor's own three
	// OnTurnCompleted call sites: pushpr.go, dispatch.go, timerfired.go)
	// is this value's one consumer, needed so a machine-triggered
	// workflow-advance turn (ApplyStepOutcome's own NextAdvance case,
	// internal/app/workflowengine/advance.go) resolves the epistemic-check
	// gate identically to every other build-turn-creating call site in
	// this codebase.
	epistemicCheckDefault bool

	// rolloutMode (§10 Phase 6, §32) is RegistryOptions.
	// RolloutMode's own resolved value (see that field's own doc comment
	// for why this is an OPTIONS field, not a required NewRegistry
	// parameter, unlike epistemicCheckDefault immediately above), threaded
	// through to every Actor this Registry hydrates. dispatch.go's own
	// refuseIfRolloutUnenrolled (beside refuseIfSubstrateUnsupported) is
	// this value's one consumer: the dispatch-time HALF of §32's own
	// "fail-closed, twice" pair, re-checked fresh on every Spawn/Restore/
	// Resume attempt against the SAME *postgres.RepoSettingsStore already
	// available on every Actor via stores.repoSettings (storeBundle,
	// above) -- no new store parameter needed on NewRegistry, only this
	// one mode value.
	rolloutMode platform.RolloutMode

	// group tracks every actor's mailbox-loop goroutine, so evicted/
	// crashed actors are cleanly reaped and Shutdown can wait on all of
	// them. Deliberately the zero value, NOT errgroup.WithContext(...) --
	// see doc.go's Concurrency section: a shared cancel-on-first-error
	// context would let one session's failure tear down every OTHER
	// session's actor sharing this process, which is exactly what
	// single-session ownership must not allow.
	group errgroup.Group

	// lifecycleCtx is the parent context every actor's run loop derives
	// its own cancellation from -- intentionally the PROCESS's lifetime
	// (supplied once at construction), never any individual caller's
	// request-scoped context: an actor spawned to satisfy one inbound
	// request or one timer-pump delivery must keep running long after
	// that request's own context is done. Storing a context in a struct
	// field is unusual, but this is the recognized exception -- a
	// long-lived component's own lifetime signal, not a request-scoped
	// value threaded through call arguments.
	lifecycleCtx context.Context
	cancel       context.CancelFunc
}

// NewRegistry builds a Registry backed by pool. ctx represents the
// process's own lifetime; Shutdown cancels the context every spawned
// actor's run loop derives from. broadcaster is threaded through to every
// Actor this Registry hydrates (§6.2's "→ broadcast stream") -- may be
// nil, in which case every Actor simply never broadcasts (see
// Actor.broadcastPending).
//
// commander/provider/publicBaseURL are §9.3's ("e2e happy path") own
// additions (design decisions 3/4/6): commander is the
// ports.SandboxCommander every Actor's handleEnsureDispatched uses to push
// a dispatched turn's prompt to a live sandbox connection; provider is the
// ports.SandboxProvider every Actor's handleEnsureDispatched calls
// CreateSandbox on; publicBaseURL is this control plane's own externally-
// reachable http(s):// base URL, used to derive SessionConfig.
// ControlPlaneWsUrl for a freshly spawned sandbox. sourceControl and
// tokenEncryptionKey are this SAME Step's remaining additions (design
// decision 9): sourceControl is the ports.SourceControl every Actor's
// createPRBestEffort (pushpr.go) calls CreatePR on; tokenEncryptionKey
// decrypts the session creator's own stored GitHub OAuth access token for
// that same call. openCodeRuntimeVersion is §8.5's ("image builds") own
// addition: the RuntimeVersion input to every Actor's own image-fingerprint
// computation (dispatch.go/imageresolve.go). diffFetcher is §14.4's
// ("handoff-readiness sentinel", §14.4) own addition: the narrow
// PRDiffFetcher (handoffsentinel.go) createPRBestEffort's own handoff-
// sentinel hook uses to fetch a just-created PR's diff. All seven may be
// nil/empty -- callers that never exercise the spawn/dispatch/push/PR/
// image-resolution/handoff-sentinel path (e.g. the resilience test,
// design decision 12) can safely omit them.
//
// §14.3 ("mocking + contract drift") adds the contract_drift_detected
// OTel counter's construction here -- exactly once per Registry, mirroring
// app/reconciler.NewReconciler's/app/imagebuild.NewBuilder's own identical
// precedent (see each of their own doc comments) -- which is why NewRegistry
// now returns an error: construction can fail exactly the same way theirs
// can (an invalid/misconfigured MeterProvider), and that failure is
// propagated up through whatever already handles Reconciler/Builder
// construction errors today (cmd/control-plane/main.go).
//
// opts is a trailing variadic of RegistryOptions (§8.2's own
// githubBotToken started this "one small options struct, not more
// positional parameters" pattern as a bare `...string`; §24 widens it
// into a real struct since it needs to add two further optional fields
// of DIFFERENT types) -- every real caller passes at most one; only the
// first is read. This means adding a new optional field here NEVER
// requires touching NewRegistry's ~40 existing call sites across this
// codebase's test suite, which simply omit opts entirely.
func NewRegistry(
	ctx context.Context,
	pool *pgxpool.Pool,
	timeouts platform.Timeouts,
	broadcaster ports.EventBroadcaster,
	commander ports.SandboxCommander,
	provider ports.SandboxProvider,
	publicBaseURL string,
	sourceControl ports.SourceControl,
	tokenEncryptionKey []byte,
	openCodeRuntimeVersion string,
	diffFetcher PRDiffFetcher,
	epistemicCheckDefault bool,
	opts ...RegistryOptions,
) (*Registry, error) {
	meter := otel.Meter(meterName)

	contractDriftDetected, err := meter.Int64Counter(
		"contract_drift_detected",
		metric.WithDescription("Number of times a mock-configured Environment's repo was found to have drifted from its own declared contracts/api spec (§14.3)."),
		metric.WithUnit("{repo}"),
	)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: construct contract_drift_detected counter: %w", err)
	}

	// §5.3 ("ops: dashboards, alerts, runbooks", §5.3): the five
	// remaining instruments §5.3's own metric list names but nothing
	// before this Step ever registered -- see opsmetrics.go's own top
	// comment for the full gap analysis. Built from the SAME meter as
	// contractDriftDetected above, not a second otel.Meter(meterName) call.
	opsMetrics, err := newOpsMetrics(meter)
	if err != nil {
		return nil, err
	}

	var opt RegistryOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	lifecycleCtx, cancel := context.WithCancel(ctx)
	return &Registry{
		actors:                 make(map[pgtype.UUID]*Actor),
		pool:                   pool,
		timeouts:               timeouts,
		stores:                 newStoreBundle(pool, opt.PlatformShadow),
		broadcaster:            broadcaster,
		commander:              commander,
		provider:               provider,
		publicBaseURL:          publicBaseURL,
		sourceControl:          sourceControl,
		tokenEncryptionKey:     tokenEncryptionKey,
		openCodeRuntimeVersion: openCodeRuntimeVersion,
		diffFetcher:            diffFetcher,
		reviewDiffFetcher:      opt.ReviewDiffFetcher,
		knowledgeRanker:        opt.KnowledgeRanker,
		githubBotHandle:        opt.GitHubBotHandle,
		githubBotToken:         opt.GitHubBotToken,
		reviewModelDeep:        opt.ReviewModelDeep,
		rolloutMode:            opt.RolloutMode,
		contractDriftDetected:  contractDriftDetected,
		opsMetrics:             opsMetrics,
		repoAccessCache:        newRepoAccessCache(),
		epistemicCheckDefault:  epistemicCheckDefault,
		lifecycleCtx:           lifecycleCtx,
		cancel:                 cancel,
	}, nil
}

// RegistryOptions bundles NewRegistry's own less-frequently-set,
// nil/empty-safe additions -- see NewRegistry's own doc comment for why
// this is a trailing variadic struct rather than more required
// positional parameters.
type RegistryOptions struct {
	// GitHubBotToken is §8.2's ("sentinels + suggestions", §17.2) own
	// addition: the SAME static bot credential (platform.Config.
	// GitHubBotToken) pushpr.go's own createSentinelFixPRBestEffort uses
	// to open a sentinel-auto-fix child session's own fix PR. May be
	// empty (tests that never exercise the sentinel-fix PR path).
	GitHubBotToken string
	// GitHubBotHandle is §24's own addition -- see Registry.
	// githubBotHandle's own doc comment.
	GitHubBotHandle string
	// ReviewDiffFetcher is §24's own addition -- see Registry.
	// reviewDiffFetcher's own doc comment.
	ReviewDiffFetcher reviewcontext.Fetcher
	// KnowledgeRanker (§31.6/§34.7) is Registry.knowledgeRanker's own
	// doc comment -- knowledge.RecencyRanker{} (the public product's own
	// default, controlplane.selectKnowledgeRanker's return value with no
	// module composed) when nil.
	KnowledgeRanker ports.KnowledgeRanker
	// ReviewModelDeep is §26.3's own addition (§26.3): platform.Config.
	// ReviewModelDeep, threaded through to reviewretrigger.go's own
	// automatic re-review turn insert exactly like internal/adapters/
	// inbound/github's own SessionCoalescer.ReviewModelDeep field. Empty
	// means "not configured" -- see internal/domain/reviewtriage.
	// ModelAndEffort's own doc comment.
	ReviewModelDeep string

	// RolloutMode is §10's own master switch (§10 Phase 6, §32):
	// platform.Config.RolloutMode, threaded through to dispatch.go's own
	// refuseIfRolloutUnenrolled (beside refuseIfSubstrateUnsupported),
	// the dispatch-time HALF of §32's "fail-closed, twice" pair. Placed
	// in this options struct, NOT as a required NewRegistry parameter
	// (unlike httpapi.CreateSessionOnTx's own rolloutMode/repoSettings,
	// both required there) -- a deliberate, narrower choice than that
	// function's own "an omittable gate parameter is an omitted gate"
	// rule: THIS zero value (the empty string) is not a distinct,
	// weaker-but-plausible state the way an omitted *postgres.
	// RepoSettingsStore would be (a nil store cannot be read from at
	// all, so omitting it structurally disables the check) -- an unset
	// RolloutMode here is rollout.Mode(""), which internal/domain/
	// rollout.Decide already treats identically to rollout.ModeOpen (its
	// own "mode != ModeCohort admits unconditionally" gate), the exact
	// same safe, no-op behavior every existing deployment gets when
	// NARVI_ROLLOUT_MODE itself is unset. Leaving this field at its zero
	// value in this registry's own ~50 existing test call sites is
	// therefore indistinguishable from those tests running against an
	// ordinary open-mode deployment, not a silently-disabled gate --
	// mirroring GitHubBotToken/ReviewModelDeep's own identical "safe to
	// default, so it belongs here, not on every call site" reasoning
	// immediately above. Production wiring (cmd/control-plane/main.go)
	// is this field's one real, non-test caller, and passes the actual
	// cfg.RolloutMode value.
	RolloutMode platform.RolloutMode

	// PlatformShadow is §30.8's own deployment-level master switch
	// (platform.Config.ShadowMode, NARVI_SHADOW_MODE), threaded through
	// to storeBundle.outbox's own postgres.NewOutboxStore construction --
	// mirrors RolloutMode's own reasoning immediately above field for
	// field: the zero value (false) is not a distinct, weaker-but-
	// plausible state, it IS "an ordinary, non-shadow deployment", the
	// exact behavior every existing test/deployment already has today
	// (nothing consulted this bit before this Step). Production wiring
	// (cmd/control-plane/main.go) is this field's one real, non-test
	// caller, and passes the actual cfg.ShadowMode value.
	PlatformShadow bool
}

// Provider returns this Registry's own configured ports.SandboxProvider --
// the SAME provider every Actor it hydrates shares (r.provider, threaded
// through at hydration time, actor.go's own field doc comment) -- so a
// caller that needs to consult provider capabilities BEFORE a session (and
// therefore an Actor) exists at all can do so without reaching into an
// unexported field. §27.5's own up-front fail-closed substrate check
// (httpapi.CreateSessionCore, §27.5/§27.6 "refused up-front when the
// configured provider reports no support") is this method's one caller
// today. May be nil (some tests construct a Registry without one, e.g.
// the resilience test) -- callers must nil-check before calling
// Capabilities() on the result, exactly like every other r.provider
// consumer in this package already does.
func (r *Registry) Provider() ports.SandboxProvider {
	return r.provider
}

// SetKnowledgeRanker sets this Registry's own knowledgeRanker AFTER
// construction -- the one exception to every other RegistryOptions field's
// "set once, at NewRegistry time, read-only forever after" convention,
// needed because controlplane.Build's own knowledgeRanker (selectKnowledgeRanker's
// return value, docs/design/boundaries-design.md section 2) is not
// resolved until AFTER this Registry already exists: it depends on the
// composed modules' own declared capabilities, computed later in that
// SAME function for reasons unrelated to session-actor wiring. Safe to
// call with no lock (mirroring every other field here, which is likewise
// read without one): this Registry hydrates its first Actor only once
// Run starts serving real traffic, strictly after Build (and therefore
// this call) has already returned -- see hydrate.go's own
// knowledgeRanker: r.knowledgeRanker copy for the one place this value is
// ever read.
func (r *Registry) SetKnowledgeRanker(ranker ports.KnowledgeRanker) {
	r.knowledgeRanker = ranker
}

// GetOrSpawn returns the live local Actor for sessionID if this process
// already has one running, otherwise hydrates and starts a new one (§2:
// "hydration on demand"). Returns ErrSessionActorElsewhere if another
// owner already holds the session's advisory lock -- deliberately never
// blocks waiting for it, so a caller in a later Step can route the
// request to whichever pod actually holds the session rather than
// hanging on a lock that may not release for the rest of that actor's
// lifetime.
func (r *Registry) GetOrSpawn(ctx context.Context, sessionID pgtype.UUID) (*Actor, error) {
	if a := r.lookup(sessionID); a != nil {
		return a, nil
	}

	// Hydration + the advisory-lock attempt run WITHOUT the registry
	// mutex held: they are the slow, I/O-bound part, and the Postgres
	// advisory lock itself -- not this mutex -- is the mechanism that
	// must arbitrate two concurrent attempts for the SAME sessionID,
	// whether those two attempts come from two different processes, or
	// two goroutines in this same process each racing past the lookup
	// above before either has inserted into the map. Holding the mutex
	// across this whole sequence would needlessly serialize spawning of
	// completely UNRELATED sessions in this process, for no correctness
	// benefit.
	a, err := r.hydrateAndAcquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// No re-check-the-map-after-hydrating race is possible here: the
	// Postgres advisory lock this call just won is the sole arbiter of
	// ownership, so by construction no OTHER goroutine (in this process
	// or any other) could have concurrently also won it and already
	// inserted a competing entry for sessionID.
	r.mu.Lock()
	r.actors[sessionID] = a
	r.mu.Unlock()

	r.group.Go(func() error {
		return a.run(r.lifecycleCtx)
	})

	return a, nil
}

func (r *Registry) lookup(sessionID pgtype.UUID) *Actor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.actors[sessionID]
}

// evict removes a from the registry's map, but only if a is still the
// entry on file for sessionID -- defensive against a (should-be-
// impossible, per GetOrSpawn's own reasoning) double-insert.
func (r *Registry) evict(sessionID pgtype.UUID, a *Actor) {
	r.mu.Lock()
	if cur, ok := r.actors[sessionID]; ok && cur == a {
		delete(r.actors, sessionID)
	}
	r.mu.Unlock()
}

// Shutdown cancels every live actor's run loop (each releases its
// advisory lock and evicts itself as it exits, per Actor.shutdown) and
// waits for all of them, plus any timer-pump goroutine started through
// this Registry's own group, to finish.
func (r *Registry) Shutdown() error {
	r.cancel()
	return r.group.Wait()
}
