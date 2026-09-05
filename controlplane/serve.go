// Package controlplane is Narvi's composition root: config, wiring,
// migrations, HTTP+WS server. Extracted, verbatim, from
// cmd/control-plane/main.go (which now does nothing but call Main) so a
// second binary composed on top of Narvi can import this wiring instead of
// duplicating it -- an importable package is reachable from outside this
// module's own internal/... tree in a way cmd/control-plane's own
// package main never was (Go's own internal-import rule). Config loading
// + validation landed in PR-02 (§5.4); structured logging + OTel
// bootstrap landed in PR-03 (§5.3); PR-06 added the real dev-loop server:
// a Postgres pool + boot-time migrations, a chi router with a real
// /health, and errgroup-managed graceful shutdown (§5.2, §10-P0). The
// full REST/WS API landed in later PRs.
//
// Main dispatches "serve"/"seed"; the "serve" path loads config, opens the
// pool, applies migrations, and verifies the configured GitHub App's own
// scope at boot (none of that is wiring), then calls Build (every store,
// adapter, decorator, route group, and background-loop constructor,
// assembled against the already-open pool) and Run (the listener and every
// background loop, through one errgroup).
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/extension"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/automationwebhook"
	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	identitylinkhttp "github.com/narvidev/narvi/internal/adapters/inbound/identitylink"
	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	"github.com/narvidev/narvi/internal/adapters/inbound/slack"
	"github.com/narvidev/narvi/internal/adapters/inbound/webui"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	"github.com/narvidev/narvi/internal/adapters/outbound/chatgptoauth"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapp"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/llm"
	"github.com/narvidev/narvi/internal/adapters/outbound/modal"
	"github.com/narvidev/narvi/internal/adapters/outbound/objstore"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/rwx"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/app/automation"
	"github.com/narvidev/narvi/internal/app/automerge"
	"github.com/narvidev/narvi/internal/app/capability"
	"github.com/narvidev/narvi/internal/app/chatgptlink"
	"github.com/narvidev/narvi/internal/app/chatgptrefresh"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/digest"
	"github.com/narvidev/narvi/internal/app/egressmode"
	"github.com/narvidev/narvi/internal/app/findingposition"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/internal/app/imagebuild"
	"github.com/narvidev/narvi/internal/app/intentclassifier"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reconciler"
	"github.com/narvidev/narvi/internal/app/releasereview"
	appreviewtriage "github.com/narvidev/narvi/internal/app/reviewtriage"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/shadowlinear"
	"github.com/narvidev/narvi/internal/app/shadowscm"
	"github.com/narvidev/narvi/internal/app/shadowslack"
	"github.com/narvidev/narvi/internal/app/uploadsweep"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/platform"
)

// githubAPIBaseURL is GitHub's own real REST API base, passed to
// auth.NewCallbackHandler's own apiBaseURL parameter in production wiring.
// That parameter exists specifically so internal/adapters/inbound/auth's
// own tests can override it with a local httptest.Server standing in for
// GitHub's API — this constant is the ONLY place the real
// "https://api.github.com" literal appears in this binary's wiring.
const githubAPIBaseURL = "https://api.github.com"

// linearAPIBaseURL is Linear's own real API base (its GraphQL endpoint and
// OAuth2 token endpoint both live under this host), passed to
// linearapi.New's own apiBaseURL parameter in production wiring -- the
// ONLY place this literal appears in this binary's wiring, mirroring
// githubAPIBaseURL's own identical precedent immediately above (
// "Linear ingress", §8.10).
const linearAPIBaseURL = "https://api.linear.app"

// slackAPIBaseURL is Slack's own real Web API base, passed to
// slack.Deps.SlackAPIBaseURL in production wiring (§8.10, "Slack
// ingress") -- the ONLY place this literal appears in this binary's
// wiring, mirroring githubAPIBaseURL's own identical precedent exactly.
const slackAPIBaseURL = "https://slack.com/api"

// App is the built control plane: the router Build assembled, plus every
// background worker Run drives through its own errgroup. Constructed by
// Build; driven by Run; introspected by Routes (the route-table
// characterization test's own subject). Every field below other than
// Router is unexported wiring Run needs to start the background loops --
// see Build's own doc comment for why each exists.
type App struct {
	Router chi.Router

	cfg  *platform.Config
	pool *pgxpool.Pool

	registry                *sessionactor.Registry
	recon                   *reconciler.Reconciler
	builder                 *imagebuild.Builder
	outboxBuilder           *outboxworker.Builder
	releaseManifestWorker   *releasereview.Worker
	automationEngine        *automation.Engine
	automergeWorker         *automerge.Worker
	digestPump              *digest.Pump
	uploadSweeper           *uploadsweep.Sweeper
	providerCredentialStore *postgres.ProviderCredentialStore
	chatGPTDeviceFlow       *chatgptoauth.Client

	// capabilities is docs/design/boundaries-design.md, section 1's own
	// capability registry -- built once, here, from cfg.LicenseKey and the union of
	// every composed module's own declared Capabilities. Named
	// "capabilities", not "registry", specifically to stay distinct from
	// the sessionactor.Registry field immediately above -- two
	// completely unrelated concepts that happen to share the word
	// "registry" in this codebase.
	capabilities *capability.Registry

	// knowledgeRanker is docs/design/boundaries-design.md, section 2's own
	// ranker: knowledge.RecencyRanker{} unless a composed module supplies
	// its own (extension.Module.KnowledgeRanker), in which case it is
	// capabilitySwitchRanker wrapping that ranker over the SAME
	// capabilities registry immediately above -- see
	// selectKnowledgeRanker's own doc comment. Not yet read anywhere in
	// this file: the review-turn producers that will thread it into their
	// own prompt composition are a later Step's own seam, built against
	// this already-wired value rather than inventing the wiring itself.
	knowledgeRanker ports.KnowledgeRanker

	// moduleWorkers is the combined list of every worker every composed
	// module contributed (extension.Module.Workers) -- started through
	// Run's own errgroup exactly like every internal background loop.
	moduleWorkers []extension.Worker
}

// Main is intentionally a bare-bones dispatch, not a flag-parsing
// library: two subcommands, "serve" and "seed" ("config/data
// seeding", §10-P6/§13.4 -- see seed.go). "seed" lives here, as a
// control-plane subcommand, rather than its own cmd/ binary: it needs
// the SAME DB access and the SAME platform.Load() config "serve" already
// has (postgres pool, TokenEncryptionKey, InitialAdminEmails), and
// cmd/sandbox-agent's own "credential-helper" is the existing precedent
// in this repo for a second subcommand living alongside a binary's main
// server mode rather than forcing a whole new cmd/ tree for one
// operator-run tool. Anything else prints a one-line usage message to
// stderr and exits non-zero.
//
// Returns the process exit code instead of calling os.Exit itself, so
// cmd/control-plane's own main() -- now just
// `os.Exit(controlplane.Main(os.Args))` -- is the ONLY place this binary
// ever actually exits, and this function stays unit-testable.
//
// modules is the extension seam docs/design/boundaries-design.md,
// section 3, adds: zero for the public binary (cmd/control-plane's own call site
// passes none), one or more for a private binary composed on top of this
// package. Threaded straight through to serve, unchanged.
func Main(args []string, modules ...extension.Module) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: control-plane <serve|seed> [args...]")
		return 1
	}

	var err error
	switch args[1] {
	case "serve":
		err = serve(modules...)
	case "seed":
		err = runSeedCommand(args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: control-plane <serve|seed> [args...]")
		return 1
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// serve loads config, wires logging/OTel (unchanged from PR-02/PR-03),
// opens the Postgres pool and applies embedded migrations, then runs the
// chi-routed HTTP server until SIGINT/SIGTERM, shutting down gracefully
// within Timeouts.ShutdownGracePeriod. The listen goroutine and the
// shutdown-watcher goroutine are both launched via errgroup.Group.Go —
// never a bare `go` statement (§11: no naked goroutines).
func serve(modules ...extension.Module) error {
	cfg, err := platform.Load()
	if err != nil {
		return err
	}

	logger := platform.NewLogger(os.Stdout, cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// cfg.OTLPEndpoint is §33's control-plane-only OTLP opt-in
	// (platform.Config.OTLPEndpoint's own doc comment has the full gating
	// rule) -- empty keeps building the stdouttrace/stdoutmetric exporters
	// this call has always used, non-empty swaps in a real OTLP/HTTP
	// exporter pointed at it instead. cmd/sandbox-agent/main.go's own
	// identical-looking call hardcodes "" here instead of threading any
	// config through -- see that call site's own doc comment for why.
	shutdownOTel, err := platform.SetupOTel(ctx, "narvi-control-plane", cfg.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer func() {
		// Deliberately a fresh background context, not ctx: by the time
		// this deferred call runs, ctx may already be canceled (that's
		// exactly what triggers shutdown below), and a canceled context
		// would make the flush itself fail immediately.
		//
		// Bounded by cfg.Timeouts.OTelShutdownTimeout and warn-and-continue
		// on error, never fatal -- see shutdownControlPlaneOTel's own doc
		// comment for why this call is no longer the unbounded one it used
		// to be: with cfg.OTLPEndpoint set, this flush is a real network
		// call to an operator's collector, with a real hang mode a bare
		// stdout write never had, and a down/unreachable collector must
		// not be allowed to block this process's own graceful exit past
		// its configured budget.
		if err := shutdownControlPlaneOTel(shutdownOTel, cfg.Timeouts.OTelShutdownTimeout); err != nil {
			slog.Error("otel shutdown failed", "error", err)
		}
	}()

	pool, err := postgres.NewPoolWithMaxConns(ctx, cfg.DatabaseURL, cfg.DBPoolMaxConns)
	if err != nil {
		return fmt.Errorf("open postgres pool: %w", err)
	}
	defer pool.Close()
	// Logged at boot, not just documented, so an operator sizing a
	// deployment (or diagnosing a hang) has the resolved value in hand
	// without reading source -- see platform.Config.DBPoolMaxConns's own
	// doc comment for why this number matters independently of host core
	// count.
	slog.Info("narvi control-plane: postgres pool configured", "max_conns", pool.Config().MaxConns)

	if err := applyMigrations(cfg.DatabaseURL); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	// §30.4(4)'s own boot-time half of "scope introspection, fail-closed,
	// at boot and at mint": before this process ever starts serving
	// traffic -- before ANY wiring, not merely before the listener --
	// confirm the configured GitHub App's own maximum granted permissions
	// are read-only. An operator who pastes a broad App (or misconfigures
	// its own permissions to include Contents: Read & write) into this
	// slot must get a loud boot refusal, never a silent re-arming of every
	// shadow sandbox on the first real mint.
	//
	// This client is used for the check ONLY and then discarded -- Build
	// constructs its OWN githubAppClient (same constructor, same
	// arguments) for the wiring that actually uses one (ScmCredentials'
	// shadow branch, per its own doc comment). githubapp.New itself does
	// no I/O and holds no state a second construction could conflict
	// with, so this is one extra allocation, not a second code path: the
	// alternative -- threading a pre-built client into Build as a
	// parameter -- would put a boot pre-flight's own output on Build's
	// signature, which is supposed to take nothing but an already-open
	// pool and hand back a fully wired App.
	preflightGitHubAppClient := githubapp.New(http.DefaultClient, githubAPIBaseURL, cfg.GitHubAppID, cfg.GitHubAppPrivateKey, cfg.Timeouts.GitHubAppJWTTTL, cfg.Timeouts.GitHubAppJWTClockSkew)
	if err := verifyGitHubAppScopeAtBoot(ctx, preflightGitHubAppClient, cfg.Timeouts.GitHubAppScopeCheckTimeout); err != nil {
		return err
	}

	app, err := Build(ctx, cfg, pool, modules...)
	if err != nil {
		return err
	}
	return app.Run(ctx, cfg.HTTPAddr)
}

// Build is serve()'s own former wiring, with the boot pre-flights that are
// not wiring (migrations, the GitHub App scope check) and the listening/
// signal-handling removed to Main/serve and Run respectively: every store,
// adapter, decorator, route group, notifier map and background-loop
// constructor, assembled against an already-open pool. No listening, no
// signal handling, no os.Exit -- a caller that already has a *pgxpool.Pool
// (this package's own integration tests; a future private module) can call
// this directly without going through Main at all.
//
// modules (docs/design/boundaries-design.md, section 3) is validated FIRST,
// before this function does anything else with cfg or pool: a module
// declaring a malformed Name, a Name already used by another composed
// module, or a capability this build does not define fails boot loudly,
// once, rather than mounting a broken or half-wired route table. With
// zero modules -- the public binary's own shape -- every module-shaped
// insertion point below (migrations, the route mount loop, the worker
// list) is exactly a no-op, and the capability registry's own installed
// set is empty (internal/app/capability.Registry.Enabled is then false
// for everything, whatever cfg.LicenseKey holds -- technical plan
// §34.5).
func Build(ctx context.Context, cfg *platform.Config, pool *pgxpool.Pool, modules ...extension.Module) (*App, error) {
	if err := validateModules(modules); err != nil {
		return nil, err
	}

	for _, m := range modules {
		if m.Migrations == nil {
			continue
		}
		if err := applyModuleMigrations(cfg.DatabaseURL, m.Name+"_schema_migrations", m.Migrations); err != nil {
			return nil, fmt.Errorf("apply module %q migrations: %w", m.Name, err)
		}
	}

	// hub is the single shared piece of state connecting the app-layer
	// actor to the adapter-layer client sockets (§6.2's "→ broadcast
	// stream"): constructed once here, then threaded through to
	// BOTH sessionactor.NewRegistry (as the ports.EventBroadcaster every
	// Actor's successful transact commits to) and wshub.NewClientHandler
	// (so it can register/unregister each subscribed connection) -- see
	// internal/adapters/inbound/wshub's own Hub doc comment.
	hub := wshub.NewHub()

	// commander is the ports.SandboxCommander every Actor uses to push an
	// outbound command (a dispatched turn's prompt) to a session's live
	// sandbox WS connection (§9.3, "e2e happy path", design decision
	// 4) -- constructed once here, then threaded through to BOTH
	// sessionactor.NewRegistry (as the port) and wshub.NewSandboxHandler
	// (so it can Register each connection as it completes its handshake),
	// mirroring hub's own dual-threading immediately above exactly.
	commander := wshub.NewSandboxRegistry(cfg.Timeouts)

	// sandboxProvider is the real internal/adapters/outbound/modal.
	// Provider (§9.3 is its first real production caller anywhere in
	// this codebase -- see that package's own doc.go for the "no real
	// Modal account reachable from this codebase's own tests/CI" caveat;
	// a real deploy of this binary must point NARVI_MODAL_BASE_URL/
	// NARVI_MODAL_AUTH_TOKEN at an actual Modal account, or a mock
	// standing in for one).
	sandboxProvider, err := modal.New(modal.Config{
		BaseURL:        cfg.ModalBaseURL,
		AuthToken:      cfg.ModalAuthToken,
		Timeouts:       cfg.Timeouts,
		EgressProxyURL: cfg.ModalEgressProxyURL,
	})
	if err != nil {
		return nil, fmt.Errorf("construct modal provider: %w", err)
	}

	// sourceControl is the ports.SourceControl every Actor's own
	// createPRBestEffort (internal/app/sessionactor/pushpr.go) calls
	// CreatePR on once a push_complete event arrives -- constructed once
	// here, exactly mirroring modal.New's own real-adapter-in-production-
	// wiring precedent immediately above.
	//
	// This is the ONE production construction site for the GitHub adapter,
	// which is what makes §30.2's layer 0 possible with a single seam: the
	// gate is installed as the adapter's transport here, so every request
	// the package makes rides through it -- including the mutating methods
	// that live outside the port and the synchronous comments the GitHub
	// ingress posts through this same instance.
	//
	// There is no nil default any more. §30.2 removed it deliberately: a
	// developer writing githubapi.New(nil, baseURL) in a new package used
	// to get a working, gate-free adapter invisible to every layer above.
	// Individual call deadlines still come from each caller's own context
	// (platform.Timeouts.PRCreateTimeout), not a package-level Timeout.
	shadowLedger := postgres.NewShadowSCMWriteStore(pool)
	// shadowOperatorReads backs the shadow-operator surface's own read model
	// (§30.6's own "UNION over marked outbox rows + shadow_scm_writes") --
	// a pure reader, never a writer: outboxworker's own Builder keeps
	// writing suppressed_in_shadow/delivered_to_ledger through OutboxStore
	// exactly as before, this store only ever reads what that already
	// wrote.
	shadowOperatorReads := postgres.NewShadowOperatorReadStore(pool)
	repoSettingsForEgress := postgres.NewRepoSettingsStore(pool)
	isLiveEgress := func(ctx context.Context, repoFullName string) bool {
		return egressmode.Resolve(ctx, egressmode.Deps{
			PlatformShadow: cfg.ShadowMode,
			RepoSettings:   repoSettingsForEgress,
		}, repoFullName).Live()
	}

	// unattributedShadowRepoName is what internal/app/shadowslack and
	// internal/app/shadowlinear attribute a suppressed write to when this
	// deployment's own default repo is unparseable or unconfigured --
	// isLiveEgress above still resolves a real answer for it (repo_settings.
	// Get on a nonexistent row is pgx.ErrNoRows, which egressmode.Resolve's
	// own fail-closed posture already treats as shadow), so this string
	// only ever affects what an operator READS on the ledger row, never
	// whether one is written or which mode governs it.
	const unattributedShadowRepoName = "(no default repo configured)"

	// shadowRepoFullName resolves rawRepoURL (platform.Config's
	// SlackDefaultRepoURL/LinearDefaultRepoURL) into the single "owner/repo"
	// isLiveEgress checks §30.8's per-repo flag against for that whole
	// provider integration -- see internal/app/shadowslack's own doc
	// comment ("why one fixed repository, not a per-call one") for why
	// this is resolved ONCE, here, rather than per call or per session:
	// every session either ingress package can ever create names exactly
	// this one, deployment-wide-configured repository, so there is only
	// ever one answer for this whole deployment.
	shadowRepoFullName := func(rawRepoURL string) string {
		if rawRepoURL == "" {
			return unattributedShadowRepoName
		}
		owner, repo, err := reposource.ParseOwnerRepo(rawRepoURL)
		if err != nil {
			slog.Warn("narvi control-plane: could not parse a default repo URL for shadow-egress attribution; suppressed writes on it will be recorded unattributed", "error", err)
			return unattributedShadowRepoName
		}
		return owner + "/" + repo
	}

	// Say out loud, at boot, what this deployment will and will not send.
	//
	// §30.8's polarity means a repository is suppressed unless something
	// explicitly promoted it, and that is the right default -- a newly
	// connected repository must never leak while someone decides. But the
	// same default applies to a deployment that simply upgraded into this
	// capability, where repositories that were sending yesterday stop
	// today. The operator surface that promotes them is a later Step, so
	// between here and there the only honest thing available is to tell
	// them, by name, at boot, rather than let them learn it from a
	// customer who stopped receiving notifications.
	//
	// Deliberately NOT a backfill migration promoting existing rows.
	// Promotion is the one direction this design refuses to take on
	// anyone's behalf: §30.8 requires a fence timestamp and the explicit
	// quarantine of shadow-era artifacts, and a migration can honour
	// neither. Silence would be worse than the warning; a silent promotion
	// would be worse than both.
	if cfg.ShadowMode {
		slog.Warn("narvi control-plane: shadow mode is forced for this whole process -- no outgoing effect will reach any customer system, whatever any repository's own setting says")
	} else if suppressed, err := repoSettingsForEgress.CountSuppressedRepos(ctx); err != nil {
		slog.Warn("narvi control-plane: could not determine how many repositories have outgoing effects suppressed", "error", err)
	} else if suppressed > 0 {
		slog.Warn("narvi control-plane: outgoing effects are suppressed for at least this many repositories -- they are recorded rather than sent, and stay that way until each is explicitly promoted",
			"at_least", suppressed)
	}

	// githubAppClient (§30.4) mints the read-only GitHub App installation
	// tokens ScmCredentials' own shadow branch below substitutes for the
	// write-capable creator OAuth/bot-token credentials. Constructed once
	// here, the ONE production construction site, mirroring
	// gatedHTTPClient/liveSourceControl's own identical "one seam" pattern
	// immediately below.
	githubAppClient := githubapp.New(http.DefaultClient, githubAPIBaseURL, cfg.GitHubAppID, cfg.GitHubAppPrivateKey, cfg.Timeouts.GitHubAppJWTTTL, cfg.Timeouts.GitHubAppJWTClockSkew)

	gatedHTTPClient := githubapi.NewGatedClient(shadowLedger, isLiveEgress)
	liveSourceControl := githubapi.New(gatedHTTPClient, githubAPIBaseURL)

	// Layer 1 on top of layer 0 (§30.2), redundant in one direction only:
	// the decorator records the six port writes with their real types and
	// keeps the state machines coherent; the transport underneath
	// guarantees nothing escapes even when the decorator's coverage is
	// stale.
	sourceControl, err := shadowscm.New(liveSourceControl, shadowLedger, isLiveEgress)
	if err != nil {
		return nil, fmt.Errorf("build shadow source-control decorator: %w", err)
	}

	// Consumers typed on ports.SourceControl take the decorator. A few take
	// the CONCRETE adapter instead -- the notifiers, and the ingress comment
	// poster, whose PostIssueComment is one of the mutating methods §30.2
	// observes living outside the port -- and those keep liveSourceControl.
	//
	// That is the design rather than a gap, and it is why layer 0 exists.
	// The decorator cannot be substituted where a concrete type is demanded,
	// and a codebase where some callers hold a gated object and others hold
	// an ungated one under similar names is exactly the per-call-site
	// discipline §30 refuses. liveSourceControl is not ungated: its
	// transport is the gate, so every mutating request it makes -- including
	// the ones no typed layer can see -- is intercepted and recorded.

	// Registry/wshub are wired into the real binary here -- an intended,
	// natural consequence: the timer pump becomes genuinely live here for
	// the first time too, run via the errgroup below, alongside the
	// client-hub half (hub above), the store handles its own handlers/REST
	// endpoints need, and commander/sandboxProvider/
	// cfg.PublicBaseURL/sourceControl/cfg.TokenEncryptionKey -- see
	// internal/app/sessionactor.NewRegistry's own doc comment for what
	// each is used for. §14.3 ("mocking + contract drift") makes
	// NewRegistry fallible (constructs the contract_drift_detected OTel
	// counter), mirroring recon/builder's own identical error handling
	// immediately below. §14.4 ("handoff-readiness sentinel") adds
	// diffFetcher -- the SAME sourceControl instance (the shadow decorator,
	// which forwards diff reads to the adapter beneath it)
	// passed a second time, satisfying sessionactor.PRDiffFetcher exactly
	// like it already satisfies the github inbound handler's own
	// reviewcontext.Fetcher below (DiffFetcher: sourceControl) -- never a
	// second, independently-constructed client. §24 ("review:
	// automatic re-review on new commits") adds ReviewDiffFetcher --
	// the SAME sourceControl decorator a THIRD time, satisfying
	// reviewcontext.Fetcher directly (GetPullRequest/GetCompareDiff are
	// both real *githubapi.Adapter methods) -- plus GitHubBotHandle/
	// GitHubBotToken, bundled into sessionactor.RegistryOptions (see that
	// type's own doc comment for why this is a trailing options struct,
	// not more positional parameters).
	registry, err := sessionactor.NewRegistry(ctx, pool, cfg.Timeouts, hub, commander, sandboxProvider, cfg.PublicBaseURL,
		sourceControl, cfg.TokenEncryptionKey, cfg.OpenCodeRuntimeVersion, sourceControl, cfg.EpistemicCheckDefault,
		sessionactor.RegistryOptions{
			GitHubBotToken:    cfg.GitHubBotToken,
			GitHubBotHandle:   cfg.GitHubBotHandle,
			ReviewDiffFetcher: sourceControl,
			ReviewModelDeep:   cfg.ReviewModelDeep,
			// RolloutMode (§10 Phase 6, §32): dispatch.go's own
			// refuseIfRolloutUnenrolled -- the dispatch-time half of the
			// "fail-closed, twice" pair -- consults this on every
			// Spawn/Restore/Resume attempt.
			RolloutMode: cfg.RolloutMode,
			// PlatformShadow (§30.8): this Registry's own storeBundle
			// constructs its OWN *postgres.OutboxStore (newStoreBundle,
			// registry.go), separate from the outboxStore variable below --
			// every sessionactor-internal enqueue call site (outboxenqueue.go,
			// reviewretrigger.go, handoffsentinel.go, planrecord.go,
			// previewpr.go, progressnotify.go) writes through THIS one, so it
			// needs the SAME cfg.ShadowMode outboxStore itself receives below.
			PlatformShadow: cfg.ShadowMode,
			// ShadowLedger (§30.7/§30.9, resolved: no git mirror --
			// short-circuit the push, done properly): the SAME
			// shadowLedger instance shadowscm.Decorator already writes
			// to, below -- sendPushBestEffort (pushpr.go) records here
			// directly when a turn's own frozen push/PR decision is
			// shadow, since no push_complete/push_error wire event ever
			// arrives on that path to drive a recording through the
			// decorated sourceControl the way CreatePR/CreateBranch
			// already do.
		})
	if err != nil {
		return nil, fmt.Errorf("construct session actor registry: %w", err)
	}
	sessionStore := postgres.NewSessionStore(pool)
	turnStore := postgres.NewTurnStore(pool)
	sandboxStore := postgres.NewSandboxStore(pool)
	eventStore := postgres.NewEventStore(pool)
	artifactStore := postgres.NewArtifactStore(pool)
	wsTokenStore := postgres.NewWSTokenStore(pool)
	environmentStore := postgres.NewEnvironmentStore(pool)
	imageBuildStore := postgres.NewImageBuildStore(pool)
	// imageCacheVersionStore is §19.1's own build-time dependency
	// cache bookkeeping, third iteration: immutable versioned cache
	// snapshots (§19.1's closing paragraph). Backs app/imagebuild.
	// Builder's own cacheMount/recordCachePublish (mint a version before
	// a real BuildImage attempt, resolve the latest confirmed version to
	// mount, confirm a real publish afterward, prune old versions per
	// domain/imagebuild.RetainedCacheVersions) -- see that store's own
	// doc comment.
	imageCacheVersionStore := postgres.NewImageCacheVersionStore(pool)

	// automationStore/automationInvocationStore/automationRunStore are
	// §3.5's ("automations: engine", §3.5) own three tables -- see
	// internal/app/automation's own doc.go for the full engine writeup.
	// Constructed here, alongside every other core store, so the engine
	// below (and any future §8.4 trigger-evaluation caller) can share
	// them rather than each constructing its own copy.
	automationStore := postgres.NewAutomationStore(pool)
	automationInvocationStore := postgres.NewAutomationInvocationStore(pool)
	automationRunStore := postgres.NewAutomationRunStore(pool)

	// planStore/participantStore are §8.1's ("plan mode, web", §8.1)
	// own additions, backing the two new approve/reject REST endpoints
	// below (internal/adapters/inbound/httpapi/planapprove.go).
	planStore := postgres.NewPlanStore(pool)
	participantStore := postgres.NewParticipantStore(pool)

	// auditLogStore is §13.2's ("identities + full RBAC", §13.3) own
	// addition -- every Authorize-gated state change (CreateSession,
	// CreateTurn, ApprovePlan/RejectPlan below) writes one audit_log row
	// on the SAME transaction as the change itself; threaded into every
	// GitHub/Slack/Linear ingress Deps struct too (below), so bot-
	// attributed session/turn/plan-decision writes get the identical
	// treatment (actor_user_id NULL), not a second, REST-only code path.
	auditLogStore := postgres.NewAuditLogStore(pool)

	// outboxStore/linearAgentSessionStore are constructed here (rather than
	// down where the outbox delivery worker/Linear ingress blocks live
	// below) because §8.1's ("plan mode, cross-channel", §8.1/§13.3) own
	// httpapi.DecidePlanOnTx -- shared by the /api/sessions plan approve/
	// reject routes immediately below AND by internal/adapters/inbound/
	// {slack,linear}'s own new plan-decision entry points -- needs both, to
	// enqueue this Step's own cross-channel-notify outbox rows.
	outboxStore := postgres.NewOutboxStore(pool, cfg.ShadowMode)
	// releaseManifestPendingStore (blocking-finding fix #1, "release PR
	// review", §15.2) is the durable release_manifest_pending queue --
	// the github Config below writes to it inline (a single, fast
	// INSERT, before its own webhook ack); releasereview.Worker (started
	// alongside every other background loop below) is what later claims
	// and actually runs the manifest check -- see
	// migrations/000050_release_manifest_pending.up.sql's own doc
	// comment for the full "why" this exists as its own table/loop,
	// never folded into outboxStore/outboxworker itself.
	releaseManifestPendingStore := postgres.NewReleaseManifestPendingStore(pool)
	linearAgentSessionStore := postgres.NewLinearAgentSessionStore(pool)

	// blobStore/uploadSweeper ("uploads, blob storage & the
	// in-sandbox download_file tool", §28.7) are constructed ONLY when
	// cfg.ObjectStorage is non-nil -- mirrors cfg.RWXAccessToken's own
	// "absent = feature off" precedent below, one level deeper: with no
	// object-storage config present, blobStore stays a nil
	// ports.BlobStore, and every upload mint endpoint (route registration
	// below) returns its own structured "uploads not configured" error
	// rather than failing to boot. Constructed here, early, rather than
	// down where RWX's own optional adapter lives: unlike RWX (needed only
	// by the outbox notifier map), blobStore is threaded into the upload
	// routes registered further down in this same function, so it must
	// exist before that point.
	var blobStore ports.BlobStore
	var objStore *objstore.Store
	var uploadSweeper *uploadsweep.Sweeper
	if cfg.ObjectStorage != nil {
		objStore, err = objstore.New(objstore.Config{
			Endpoint:        cfg.ObjectStorage.Endpoint,
			PublicEndpoint:  cfg.ObjectStorage.PublicEndpoint,
			Region:          cfg.ObjectStorage.Region,
			Bucket:          cfg.ObjectStorage.Bucket,
			AccessKeyID:     cfg.ObjectStorage.AccessKeyID,
			SecretAccessKey: cfg.ObjectStorage.SecretAccessKey,
			UsePathStyle:    cfg.ObjectStorage.UsePathStyle,
			Timeouts:        cfg.Timeouts,
		})
		if err != nil {
			return nil, fmt.Errorf("construct object storage adapter: %w", err)
		}
		blobStore = objStore

		uploadSweeper, err = uploadsweep.NewSweeper(pool, artifactStore, eventStore, outboxStore, sandboxStore, hub, cfg.Timeouts)
		if err != nil {
			return nil, fmt.Errorf("construct upload abandonment sweeper: %w", err)
		}
	}

	// slackNotifier/planSlackNotifier are constructed here (rather than
	// down where the outbox delivery worker block lives below) because the
	// new Slack interactivity route (right below) ALSO needs a real
	// *slackapi.Client, for its own synchronous chat.update/views.open
	// calls -- one client, reused for both the outbox notifier registration
	// and this route, mirroring how registry/commander are each
	// constructed once and threaded through multiple call sites elsewhere
	// in this same function.
	//
	// http.DefaultClient here (not nil) is deliberate, and safe: this
	// package's own New used to default a nil client to http.DefaultClient
	// silently, which §30.2 calls an attractive nuisance and removed --
	// New(nil, ...) now builds a client that can make NO request at all
	// (slackapi.refusingTransport), so this call site must hand over a
	// real one explicitly. Handing over a WORKING client here does not
	// reopen the gap that removal closed: slackNotifier.Deliver (the
	// outbox path, ports.NotificationKindSlack/SlackPlanApproval/...) is
	// already suppressed at the outbox layer (outboxworker's own
	// notificationKindClassification, §30.2), and slackNotifier's other
	// synchronous methods are never called directly by any ingress
	// handler any more -- every one of those calls goes through
	// slackDecorated below instead (§30.3's "one client per provider").
	slackNotifier := slackapi.New(http.DefaultClient, slackAPIBaseURL, cfg.SlackBotToken)
	planSlackNotifier := outboxworker.NewPlanSlackNotifier(slackNotifier, planStore)

	// slackDecorated is §30.3's second compensating control for Slack: the
	// SAME slackNotifier instance above, wrapped so its ack/interactive
	// mutation methods are suppressed-and-recorded in shadow -- handed to
	// BOTH Slack ingress routes below (Deps.SlackClient, InteractiveDeps.
	// SlackClient) as the shadowslack.Client interface, never the
	// concrete *slackapi.Client, so neither route can construct (or reach
	// past) a gate-free client of its own.
	slackDecorated, err := shadowslack.New(slackNotifier, shadowLedger, shadowRepoFullName(cfg.SlackDefaultRepoURL), isLiveEgress)
	if err != nil {
		return nil, fmt.Errorf("build shadow slack decorator: %w", err)
	}

	// webhookDeliveryStore is §5.1's own provider-agnostic dedupe claim,
	// shared across §8.2/§8.10's own GitHub/Slack/Linear ingress (see
	// the Linear ingress block below, which reuses this SAME store rather
	// than constructing its own). slackThreadSessionStore ("Slack
	// ingress", §8.10) is the thread<->session mapping (see
	// internal/adapters/inbound/slack's own doc.go); githubPRSessionStore
	// ("GitHub ingress", §8.2) is the per-PR review-session
	// coalescing claim (see internal/adapters/inbound/github's own doc.go).
	webhookDeliveryStore := postgres.NewWebhookDeliveryStore(pool)
	slackThreadSessionStore := postgres.NewSlackThreadSessionStore(pool)
	githubPRSessionStore := postgres.NewGitHubPRSessionStore(pool)
	// repoSettingsStore ("server-side verdict", §8.2/§21.2) backs
	// the admin repo-settings REST routes below AND the verdict-posting
	// tool's own blockOnHighRisk read (reviewverdict.go) -- one store,
	// shared, never a second independently-constructed copy.
	repoSettingsStore := postgres.NewRepoSettingsStore(pool)
	// timerStore ("review: automatic re-review on new commits",
	// §24.1) backs the synchronize webhook lane's own DIRECT, actor-
	// bypassing session_timers write below (githubingress.Config.Timers)
	// -- a standalone instance over the SAME pool every other store here
	// already shares (sessionactor.Registry constructs its own, separate
	// *postgres.TimerStore internally, never exported, so this webhook
	// handler needs its own).
	timerStore := postgres.NewTimerStore(pool)
	// providerCredentialStore ("provider credential injection",
	// §25.1/§25.3) backs the 3 scoped management CRUD route groups below
	// AND the sandbox-facing delivery endpoint (providercredentialsdelivery.go)
	// -- one store, shared, never a second independently-constructed copy.
	providerCredentialStore := postgres.NewProviderCredentialStore(pool)
	// sandboxSecretStore/openCodeConfigStore ("sandbox secrets &
	// opencode config", §27.1/§27.2) back their own scoped management CRUD
	// route groups below AND their own sandbox-facing delivery endpoints
	// (sandboxsecretsdelivery.go/opencodeconfigdelivery.go) -- mirrors
	// providerCredentialStore's own identical "one store, shared" pattern.
	sandboxSecretStore := postgres.NewSandboxSecretStore(pool)
	openCodeConfigStore := postgres.NewOpenCodeConfigStore(pool)
	// cloudIdentityBindingStore/oidcSigningKeyStore ("cloud
	// identity: OIDC issuer, bindings, minting", §27.3) back the binding
	// management CRUD route groups, the signing-key rotation trigger, the
	// public discovery/JWKS routes, AND the sandbox-facing minting
	// endpoint (cloudidentitytoken.go) -- mirrors providerCredentialStore/
	// sandboxSecretStore's own identical "one store, shared" pattern.
	cloudIdentityBindingStore := postgres.NewCloudIdentityBindingStore(pool)
	oidcSigningKeyStore := postgres.NewOIDCSigningKeyStore(pool)
	// clusterBindingStore ("cloud identity: sandbox-side
	// consumption + kubeconfig injection", §27.4) backs the cluster-binding
	// management route group (clusterbindings.go) AND the sandbox-facing
	// cloud-identity-config delivery endpoint (cloudidentityconfigdelivery.go)
	// -- mirrors cloudIdentityBindingStore's own identical "one store,
	// shared" pattern.
	clusterBindingStore := postgres.NewClusterBindingStore(pool)
	// chatGPTLinkAttemptStore/chatGPTDeviceFlow ("models: Codex
	// via ChatGPT-account OAuth", §29.3/§29.5/§29.9) back the self-service
	// link-flow REST routes (chatgptlink.go) AND the refresh pump
	// (chatgptrefresh) -- one store/client each, shared, never a second
	// independently-constructed copy. chatGPTDeviceFlow's own httpClient
	// deliberately does NOT set http.Client.Timeout -- chatgptoauth.Client
	// bounds every call itself via a per-call context.WithTimeout wrap
	// (platform.Timeouts.ChatGPTOAuthHTTPClientTimeout), mirroring
	// internal/adapters/outbound/opencode's own doJSONTimeout precedent
	// (see that package's own client.go doc comment).
	chatGPTLinkAttemptStore := postgres.NewChatGPTLinkAttemptStore(pool)
	chatGPTDeviceFlow := chatgptoauth.New(http.DefaultClient, chatgptoauth.DefaultBaseURL, cfg.Timeouts.ChatGPTOAuthHTTPClientTimeout)
	chatGPTLinkDeps := chatgptlink.Deps{
		Pool:                pool,
		LinkAttempts:        chatGPTLinkAttemptStore,
		ProviderCredentials: providerCredentialStore,
		AuditLog:            auditLogStore,
		DeviceFlow:          chatGPTDeviceFlow,
		TokenEncryptionKey:  cfg.TokenEncryptionKey,
		Timeouts:            cfg.Timeouts,
	}
	// reviewFindingStore/sentinelFixStore ("sentinels +
	// suggestions", §17/§22.1) back the verdict-posting tool's own
	// per-finding upsert + sentinel-auto-fix claim (reviewverdict.go), the
	// rebut/apply-suggestion endpoints (reviewfindings.go), and re-review
	// reconciliation (internal/app/reviewcontext) -- one store each,
	// shared, never a second independently-constructed copy.
	reviewFindingStore := postgres.NewReviewFindingStore(pool)
	sentinelFixStore := postgres.NewSentinelFixStore(pool)
	// releaseManifestCheckStore (§12.2 item 9, "dedicated release-review
	// screen") persists the release manifest check's own typed result --
	// see internal/app/releasereview/persist.go's own doc comment. Shared
	// between releaseManifestWorker's own Deps (below) and the
	// release-review readout's own GET handler.
	releaseManifestCheckStore := postgres.NewReleaseManifestCheckStore(pool)
	// falsePositivePatternStore ("review: learned false-positive
	// patterns", §22.2/§22.3/§22.4) backs the GitHub capture command, the
	// advisory-injection fetch (internal/app/reviewcontext), and the
	// audit-view/retire REST endpoints -- one store, shared, never a
	// second independently-constructed copy.
	falsePositivePatternStore := postgres.NewFalsePositivePatternStore(pool)
	// reviewDigestSectionFeedbackStore ("review deep path:
	// adversarial counter-review + readout measurement", §26.5) backs the
	// GitHub `arch recap wrong: <reason>` capture command -- one store,
	// shared, never a second independently-constructed copy, mirroring
	// falsePositivePatternStore's own identical precedent immediately
	// above.
	reviewDigestSectionFeedbackStore := postgres.NewReviewDigestSectionFeedbackStore(pool)
	// reviewVerdictStore/autoApprovalOutcomeStore/digestSendStateStore
	// ("review verdict persistence, analytics, digest &
	// automated approval", §21) back the verdict-posting tool's own
	// review_verdicts insert (reviewverdict.go), the real auto-approval
	// eligibility engine's own latest-verdict read (both decision-inbox
	// call sites, internal/app/decisioninbox), the contradiction-rate
	// calibration read model, and the daily digest -- one store each,
	// shared, never a second independently-constructed copy.
	reviewVerdictStore := postgres.NewReviewVerdictStore(pool)
	autoApprovalOutcomeStore := postgres.NewAutoApprovalOutcomeStore(pool)
	digestSendStateStore := postgres.NewDigestSendStateStore(pool)
	reviewVerdictDeps := appreviewverdict.Deps{
		ReviewVerdicts:       reviewVerdictStore,
		RepoSettings:         repoSettingsStore,
		ReviewFindings:       reviewFindingStore,
		AutoApprovalOutcomes: autoApprovalOutcomeStore,
		// DigestSectionFeedback (§26.5) backs appreviewverdict.
		// DigestContestationRate -- the SAME reviewDigestSectionFeedbackStore
		// instance the GitHub capture command above already uses.
		DigestSectionFeedback: reviewDigestSectionFeedbackStore,
		// §30.7: stamps each recorded auto-approval outcome with the
		// epoch it was observed in, so a shadow-era contradiction never
		// moves the rate that justifies arming auto-merge.
		PlatformShadow: cfg.ShadowMode,
		Timeouts:       cfg.Timeouts,
	}
	// digestChannelStore (§21.3) backs internal/app/digest's own
	// channel-discovery step -- constructed here, alongside its own
	// sibling stores, though the digest.Deps/automerge.Deps bundles that
	// actually use it are assembled further below, once decisionInboxDeps
	// (automerge.Deps embeds the full decisioninbox.Deps) exists.
	digestChannelStore := postgres.NewDigestChannelStore(pool)
	// workflowStore ("workflow execution engine", §25.6) backs
	// the generic step-outcome-posting tool (workflowstepoutcome.go) --
	// sessionactor's own Registry constructs its OWN WorkflowStore
	// internally (newStoreBundle, registry.go), and createTurnLocked
	// constructs one inline from pool (turn.go's own doc comment explains
	// why: avoiding a cascading signature change to CreateTurnCore's many
	// callers) -- this is a THIRD, independent instance, exactly like
	// sandboxStore/sessionStore above are each already constructed once
	// per real top-level consumer rather than shared across unrelated
	// ones.
	workflowStore := postgres.NewWorkflowStore(pool)
	// githubActorLinkNoticeStore (batch fix/deny-unlinked-github-actors)
	// backs the GitHub webhook ingress's own anti-spam dedupe for the
	// "please sign in" reply posted to an unlinked commenter's denied
	// mention (see githubingress.Config.LinkNotices' own doc comment,
	// below).
	githubActorLinkNoticeStore := postgres.NewGitHubActorLinkNoticeStore(pool)

	// intentClassifierSvc is §8.3's own real classifier (§8.3, §18):
	// llm.New resolves cfg.IntentClassifierProvider against this
	// codebase's own small provider registry (internal/adapters/outbound/
	// llm's own doc.go) -- Anthropic is the one real adapter this Step
	// ships; an unrecognized provider name never fails process boot (see
	// that package's own registry.go), it simply makes every future
	// Classify call fall back with FallbackReasonUnsupportedProvider,
	// exactly like a misconfigured model string would. promptTemplateStore
	// backs the DB-editable prompt templates (§18.6); intentClassifierSvc
	// composes both plus sessionStore's own write-once persistence
	// (UpdateIntentDecisionIfNull) and cfg.IntentClassifierActiveSurfaces'
	// own permanent shadow-vs-active gate (§18.5).
	promptTemplateStore := postgres.NewPromptTemplateStore(pool)
	intentLLM := llm.New(llm.Config{
		Provider: cfg.IntentClassifierProvider,
		APIKey:   cfg.AnthropicAPIKey,
		Timeout:  cfg.Timeouts.IntentClassifierLLMTimeout,
	})
	intentClassifierSvc := intentclassifier.New(
		intentLLM,
		cfg.IntentClassifierProvider,
		cfg.IntentClassifierModel,
		promptTemplateStore,
		sessionStore,
		cfg.IntentClassifierActiveSurfaces,
	)

	// findingRelocationResolver is §22's own §22.1.1 relocation
	// fallback (internal/app/findingposition) -- reuses intentLLM (the
	// SAME already-constructed ports.LLM client/config intentClassifierSvc
	// above uses, never a second, independently-configured adapter) and
	// the SAME cfg.IntentClassifierProvider/Model values, per that
	// package's own doc comment: both this call and intent classification
	// are small, structured, non-agentic utility calls, the identical
	// KIND of call, so there is no reason to introduce a second
	// provider/model configuration surface for it.
	findingRelocationResolver := findingposition.New(intentLLM, cfg.IntentClassifierProvider, cfg.IntentClassifierModel)

	// recon is §5.3's ("reconciler + GC", §5.3) process-wide
	// provider-reconciliation/orphan-GC loop, run below via the errgroup
	// exactly once per process -- constructed from the SAME sandboxStore/
	// sandboxProvider/cfg.Timeouts already built above for everything
	// else, mirroring how registry/commander were threaded through rather
	// than built twice. See internal/app/reconciler's own doc.go for what
	// it does and why.
	recon, err := reconciler.NewReconciler(sandboxStore, repoSettingsStore, sandboxProvider, cfg.Timeouts)
	if err != nil {
		return nil, fmt.Errorf("construct reconciler: %w", err)
	}

	// builder is §8.5's ("image builds", §8.5-note/§10-P2) own
	// process-wide background image-build loop, run below via the
	// errgroup exactly once per process -- constructed from the SAME
	// sandboxProvider/cfg.Timeouts already built above, mirroring recon's
	// own construction immediately above exactly. See internal/app/
	// imagebuild's own doc.go for what it does and why. §19.2 ("warm
	// boot: refresh pump + hook policy", §19.2) adds the trailing
	// sourceControl/cfg.GitHubImageBuildToken pair: the SAME *githubapi.
	// Adapter instance already constructed above (for CreatePR/
	// ResolveBranchSHA/ResolveContractsFingerprint) plus the new
	// platform-level credential (deliberately DISTINCT from
	// cfg.GitHubBotToken -- see platform.Config.GitHubImageBuildToken's own
	// doc comment), both consulted only by the freshness pump's own
	// per-repo tip-SHA resolution and by claim-time SHA resolution for a
	// repo-bearing build (attempt) -- never by anything on the spawn path
	// itself. §19.1 adds the final imageCacheVersionStore argument:
	// the build-time dependency cache's own version-history bookkeeping,
	// third iteration (immutable versioned cache snapshots, §19.1's
	// closing paragraph) -- no rotation-epoch config exists anymore (see
	// domain/imagebuild.CacheVolumeKey's own doc comment for why an
	// immutable-version model made that escape hatch redundant).
	builder, err := imagebuild.NewBuilder(imageBuildStore, pool, sandboxProvider, cfg.Timeouts,
		sourceControl, cfg.GitHubImageBuildToken, imageCacheVersionStore)
	if err != nil {
		return nil, fmt.Errorf("construct image builder: %w", err)
	}

	// automationEngine is §3.5's ("automations: engine", §3.5) own
	// process-wide background automation engine, run below via the
	// errgroup exactly once per process -- constructed from the SAME
	// sessionStore/turnStore/environmentStore/auditLogStore/registry/pool
	// already built above for everything else, mirroring builder's/recon's
	// own construction immediately above exactly. See internal/app/
	// automation's own doc.go for what it does and why. registry is the
	// SAME *sessionactor.Registry every other CreateSessionOnTx caller in
	// this file already threads through (e.g. the GitHub/Slack/Linear
	// ingress Deps below) -- a run's own session dispatch reuses that
	// identical TriggerDispatch path, never a second one.
	automationEngine := automation.NewEngine(
		automationStore, automationInvocationStore, automationRunStore,
		sessionStore, turnStore, environmentStore, auditLogStore,
		pool, registry, cfg.Timeouts, cfg.EpistemicCheckDefault,
		cfg.RolloutMode, repoSettingsStore,
	)

	// The 3 stores backing §13.1's ("auth v1", §13.1/§13.4) own GitHub
	// OAuth login, backend-issued session cookies, and route middleware --
	// see internal/adapters/inbound/auth's own doc.go for the full writeup.
	userStore := postgres.NewUserStore(pool)
	identityStore := postgres.NewIdentityStore(pool)
	userSessionStore := postgres.NewUserSessionStore(pool)

	// identityLinkPromptStore/appIdentityLinkDeps are §13.2's own
	// ("identities + full RBAC", §13.2) auto-linking wiring -- threaded
	// into every Slack/Linear ingress Deps struct below (so a first event
	// from an unknown provider identity auto-links or creates a magic-link
	// prompt instead of always falling back to bot attribution) AND into
	// the magic-link consume route's own Deps further down. One shared
	// identitylink.Deps value, built once here from the SAME userStore/
	// identityStore/auditLogStore every other §13.1/§13.2 caller already
	// uses -- never a second, independently-constructed copy of any of
	// them.
	identityLinkPromptStore := postgres.NewIdentityLinkPromptStore(pool)
	appIdentityLinkDeps := identitylink.Deps{
		Pool:          pool,
		Users:         userStore,
		Identities:    identityStore,
		LinkPrompts:   identityLinkPromptStore,
		AuditLog:      auditLogStore,
		PublicBaseURL: cfg.PublicBaseURL,
		PromptTTL:     cfg.Timeouts.IdentityLinkPromptTTL,
	}

	oauthConfig := auth.NewGitHubOAuthConfig(*cfg)
	allowlist := auth.AllowlistConfig{
		EmailDomains: cfg.AllowedEmailDomains,
		GitHubOrgs:   cfg.AllowedGitHubOrgs,
		Emails:       cfg.AllowedEmails,
	}
	// A cookie marked Secure is simply never sent over plain http://,
	// which is exactly what a local dev loop needs relaxed — everywhere
	// else (staging, production) it must always be true (§13.1, see
	// internal/platform/authcookie.go's own doc comment).
	secureCookies := cfg.Stage != platform.StageDevelopment

	// capabilities (docs/design/boundaries-design.md, section 1) is built here,
	// once, before the router: RequireCapability closures wired onto
	// module routes below all close over this SAME *capability.Registry,
	// re-checked per request rather than once at boot -- see
	// buildCapabilityRegistry's own doc comment.
	capabilities := buildCapabilityRegistry(slog.Default(), cfg, modules)

	// knowledgeRanker (docs/design/boundaries-design.md, section 2) is
	// selected here, alongside capabilities: the public default
	// (knowledge.RecencyRanker{}) unless a composed module supplies its
	// own, wrapped so THAT capability is re-checked per call rather than
	// once at boot -- see selectKnowledgeRanker's own doc comment. With
	// zero modules composed this never touches capabilities at all,
	// preserving TestBuild_WithoutModules_NeverConsultsCapabilities'
	// own guarantee exactly as buildCapabilityRegistry does immediately
	// above.
	knowledgeRanker := selectKnowledgeRanker(capabilities, modules, cfg.Timeouts.KnowledgeRankerTimeout)

	router := chi.NewRouter()
	router.Use(middleware.Recoverer)
	router.Use(platform.CorrelationIDMiddleware)
	// Deliberately NOT chi's own middleware.Logger/RequestID: PR-03 already
	// built our own correlation-id + platform.Logger(ctx) convention above,
	// and stacking chi's competing convention on top would give every
	// request two different request-identity mechanisms.
	router.Get("/health", healthHandler(pool, cfg.Timeouts))

	// Web UI (§12.1: "narvi serve serves API + WS + UI on one port"):
	// wired via chi's own r.NotFound hook (webui.Mount's own doc comment
	// has the full "why NotFound, not a wildcard route or an outer
	// http.Handler wrap" reasoning), which by chi's routing contract is
	// only ever invoked once every OTHER route on this router -- present
	// now or registered further down this function -- has already failed
	// to match. That is why this call sits here, before any of those
	// routes exist yet, rather than at the bottom after them: the
	// no-shadowing guarantee does not depend on this being called last
	// (internal/adapters/inbound/webui's own TestMount_OrderIndependent
	// pins exactly that property). webui.DistFS is nil unless this binary
	// was built with `-tags web_assets` (after `make web-build`) -- see
	// that package's own doc comment for why the default build never
	// requires its embed source directory to exist on disk at all.
	// selectWebAssets substitutes a composed module's own WebAssets when
	// present (docs/design/boundaries-design.md, sections 3.2 and 4.3's
	// own future private-bundle seam) -- webui.DistFS unchanged for the
	// public binary, which composes none.
	webui.Mount(router, selectWebAssets(webui.DistFS, modules))

	// OIDC discovery + JWKS ("cloud identity: OIDC issuer,
	// bindings, minting", §27.3): deliberately mounted PUBLICLY,
	// UNAUTHENTICATED -- outside auth.Middleware AND, unlike every other
	// route this file mounts outside that middleware (scm-credentials,
	// provider-credentials, sandbox-secrets/opencode-config below,
	// snapshot-mint, review/verdict, workflow/step-outcome, turn/
	// epistemic-outcome, uploads, the Slack/GitHub webhook routes, the
	// identity-link consume route), with NO sandbox-bearer-token or
	// provider-signature check of any kind either. This is deliberate,
	// not an oversight: AWS/GCP/Azure's own STS implementations fetch
	// BOTH of these documents directly, over the public internet, with no
	// Narvi credential whatsoever -- a cloud's STS has no mechanism to
	// present one, because establishing that trust is the very
	// capability federation grants, not a precondition of it (the exact
	// pattern GitHub Actions' own OIDC provider already uses for
	// CI<->cloud federation, §27.3's own stated precedent -- see
	// httpapi/oidcdiscovery.go's own top doc comment for the FULL "why
	// public, and why this would silently break federation entirely if
	// it were ever gated" reasoning, and its own _test.go for the pinning
	// test proving a request with NO credential at all gets 200 from
	// both). Both handlers fail closed (503) when cfg.CloudIdentityIssuerURL
	// is unset -- "the whole capability is off... when unset" (§27.3).
	router.Get("/.well-known/openid-configuration", httpapi.OIDCDiscovery(cfg.CloudIdentityIssuerURL))
	router.Get("/.well-known/jwks.json", httpapi.OIDCJWKS(oidcSigningKeyStore, cfg.CloudIdentityIssuerURL, cfg.Timeouts))

	router.Get("/sessions/{sessionID}/ws", wshub.NewHandler(
		wshub.NewSandboxHandler(registry, sandboxStore, commander, cfg.Timeouts),
		wshub.NewClientHandler(registry, sessionStore, turnStore, sandboxStore, eventStore, artifactStore, wsTokenStore, hub, cfg.Timeouts),
	))

	// scm-credentials (§9.3, "e2e happy path", design decision 8):
	// deliberately mounted OUTSIDE /api/sessions and outside auth.
	// Middleware entirely -- a sandbox-bearer-token-authenticated
	// endpoint, not a browser-facing one (see that handler's own doc
	// comment in internal/adapters/inbound/httpapi/scmcredentials.go).
	// githubPRSessionStore/cfg.GitHubBotToken (an audit remediation)
	// are the SAME instances review/verdict below already uses, so a
	// review session mints the SAME bot credential either way, never the
	// creator's own personal OAuth token. repoSettingsForEgress/
	// shadowLedger/cfg.ShadowMode (§30.4) are the SAME instances
	// isLiveEgress/gatedHTTPClient already use above, so this handler's
	// own shadow-substitution branch resolves egress mode identically to
	// every other §30 seam in this binary; githubAppClient is this Step's
	// own read-only mint.
	router.Post("/sessions/{sessionID}/scm-credentials",
		httpapi.ScmCredentials(sessionStore, sandboxStore, identityStore, userStore, githubPRSessionStore, repoSettingsForEgress, shadowLedger, githubAppClient, cfg.GitHubBotToken, cfg.TokenEncryptionKey, cfg.Timeouts, cfg.ShadowMode))

	// provider-credentials ("provider credential injection",
	// §25.1/§25.3): deliberately mounted OUTSIDE /api/sessions and outside
	// auth.Middleware entirely, mirroring scm-credentials immediately above
	// exactly (see httpapi/providercredentialsdelivery.go's own doc
	// comment) -- another sandbox-bearer-token-authenticated route, not a
	// browser-facing one.
	router.Post("/sessions/{sessionID}/provider-credentials",
		httpapi.ProviderCredentialsDelivery(sessionStore, sandboxStore, providerCredentialStore, cfg.TokenEncryptionKey))

	// sandbox-secrets / opencode-config ("sandbox secrets &
	// opencode config", §27.1/§27.2): deliberately mounted OUTSIDE
	// /api/sessions and outside auth.Middleware entirely, mirroring
	// provider-credentials immediately above VERBATIM (see
	// httpapi/sandboxsecretsdelivery.go's own doc comment) -- two more
	// sandbox-bearer-token-authenticated routes, not browser-facing ones.
	router.Post("/sessions/{sessionID}/sandbox-secrets",
		httpapi.SandboxSecretsDelivery(sessionStore, sandboxStore, sandboxSecretStore, cfg.TokenEncryptionKey))
	router.Post("/sessions/{sessionID}/opencode-config",
		httpapi.OpenCodeConfigDelivery(sessionStore, sandboxStore, openCodeConfigStore))

	// cloud-identity-token ("cloud identity: OIDC issuer,
	// bindings, minting", §27.3): deliberately mounted OUTSIDE
	// /api/sessions and outside auth.Middleware entirely, mirroring
	// provider-credentials/sandbox-secrets immediately above VERBATIM
	// (see httpapi/cloudidentitytoken.go's own doc comment) -- another
	// sandbox-bearer-token-authenticated route, not a browser-facing one.
	// Refuses any audience no binding for this session's Environment (or
	// global fallback) declares, and fails closed (503) when the cloud-
	// identity capability itself is off (cfg.CloudIdentityIssuerURL
	// unset) -- distinct from the /.well-known/... routes above, which
	// are PUBLIC and unauthenticated; this one still goes through the
	// full sandbox-bearer handshake, only the issuer's own discovery
	// metadata is public.
	router.Post("/sessions/{sessionID}/cloud-identity-token",
		httpapi.MintCloudIdentityToken(sessionStore, sandboxStore, cloudIdentityBindingStore, oidcSigningKeyStore, cfg.TokenEncryptionKey, cfg.CloudIdentityIssuerURL, cfg.Timeouts))

	// cloud-identity-config ("cloud identity: sandbox-side
	// consumption + kubeconfig injection", §27.3/§27.4): deliberately
	// mounted OUTSIDE /api/sessions and outside auth.Middleware entirely,
	// mirroring cloud-identity-token immediately above -- another
	// sandbox-bearer-token-authenticated route, not a browser-facing one.
	// The boot-time DISCOVERY step 73a's own minting endpoint alone cannot
	// provide: which bindings apply to this session at all, and this
	// session's own Environment's cluster_bindings row -- see
	// httpapi/cloudidentityconfigdelivery.go's own doc comment for the
	// full "why this endpoint exists" reasoning. Deliberately NOT gated by
	// RequireCloudIdentityCapability -- see that file's own doc comment
	// for why (a read of existing, secret-free rows, harmless when the
	// capability is off; enforcement happens at the already-gated mint
	// call each resolved binding still has to go through).
	router.Post("/sessions/{sessionID}/cloud-identity-config",
		httpapi.CloudIdentityConfigDelivery(sessionStore, sandboxStore, cloudIdentityBindingStore, clusterBindingStore))

	// snapshot-mint (§3.2, "snapshots & restore", design decision 2):
	// deliberately mounted OUTSIDE /api/sessions and outside auth.
	// Middleware entirely, mirroring scm-credentials immediately above
	// exactly (see that handler's own doc comment, and
	// httpapi/snapshotmint.go's own) -- another sandbox-bearer-token-
	// authenticated route, not a browser-facing one.
	router.Post("/sessions/{sessionID}/snapshot",
		httpapi.SnapshotMint(sandboxStore, sandboxProvider))

	// review/verdict ("server-side verdict", §8.2/§5.2): the
	// verdict-posting TOOL -- deliberately mounted OUTSIDE /api/sessions
	// and outside auth.Middleware entirely, mirroring scm-credentials/
	// snapshot-mint immediately above exactly (see httpapi/reviewverdict.go's
	// own doc comment for the full "why an HTTP endpoint, sandbox-bearer
	// authenticated, not a browser route" reasoning). repoSettingsStore/
	// outboxStore are the SAME instances every other caller above already
	// uses; cfg.GitHubBotHandle is the SAME handle GitHub ingress's own
	// mention detector already matches against (githubingress.Config.
	// BotHandle above) -- the rendered re-run guidance (internal/domain/
	// reviewpost.RerunGuidance) is built to be recognized by that SAME
	// regex (§5.2).
	router.Post("/sessions/{sessionID}/review/verdict",
		httpapi.PostReviewVerdict(pool, sandboxStore, sessionStore, githubPRSessionStore, repoSettingsStore, reviewFindingStore, sentinelFixStore, outboxStore, reviewVerdictStore, turnStore, eventStore, cfg.GitHubBotHandle, cfg.GitHubBotToken, sourceControl, findingRelocationResolver, cfg.Timeouts, cfg.ShadowMode))

	// workflow/step-outcome ("workflow execution engine", §25.6):
	// the GENERIC step-outcome-posting tool -- deliberately mounted
	// OUTSIDE /api/sessions and outside auth.Middleware entirely,
	// mirroring review/verdict immediately above exactly (see
	// httpapi/workflowstepoutcome.go's own doc comment). Unlike
	// review/verdict, no §25.8 built-in workflow's own prompt is wired to
	// actually call this in this Step -- it exists as real, callable
	// infrastructure for whichever future non-built-in workflow step
	// needs it (this Step's own generic engine + tool, never a specific
	// audit workflow).
	router.Post("/sessions/{sessionID}/workflow/step-outcome",
		httpapi.PostWorkflowStepOutcome(sandboxStore, workflowStore))

	// turn/epistemic-outcome ("builder epistemic pre-action
	// check", §20.2): the devil's-advocate preamble's own required
	// structured-signal-reporting tool -- deliberately mounted OUTSIDE
	// /api/sessions and outside auth.Middleware entirely, mirroring
	// review/verdict and workflow/step-outcome immediately above exactly
	// (see httpapi/epistemicoutcome.go's own doc comment).
	router.Post("/sessions/{sessionID}/turn/epistemic-outcome",
		httpapi.PostEpistemicOutcome(sandboxStore, turnStore))

	// uploads mint/confirm/content ("uploads, blob storage & the
	// in-sandbox download_file tool", §28.4/§28.5): deliberately mounted
	// OUTSIDE /api/sessions and outside auth.Middleware entirely, mirroring
	// scm-credentials/snapshot-mint/review-verdict/workflow-step-outcome
	// immediately above exactly -- the download_file tool's and the
	// agent-produced-upload direction's own sandbox-bearer endpoints.
	// blobStore/objectStorage may be nil (cfg.ObjectStorage absent, feature
	// off) -- each handler's own core returns a structured "uploads not
	// configured" error in that case rather than failing to boot.
	router.Post("/sessions/{sessionID}/uploads",
		httpapi.MintUpload(sandboxStore, artifactStore, blobStore, cfg.ObjectStorage, cfg.Timeouts))
	router.Post("/sessions/{sessionID}/uploads/{uploadID}/complete",
		httpapi.ConfirmUpload(sandboxStore, pool, artifactStore, eventStore, outboxStore, hub, blobStore, cfg.ObjectStorage))
	router.Get("/sessions/{sessionID}/uploads/{uploadID}/content",
		httpapi.UploadContent(sandboxStore, artifactStore, blobStore, cfg.ObjectStorage, cfg.Timeouts))

	// Slack ingress (§8.10): deliberately mounted OUTSIDE
	// /api/sessions and outside auth.Middleware entirely -- Slack itself
	// is the caller here, authenticated via its own request-signing
	// scheme (X-Slack-Signature/X-Slack-Request-Timestamp), not a
	// narvi_auth_session cookie. See internal/adapters/inbound/slack's
	// own doc.go for the full request-handling writeup.
	router.Post("/webhooks/slack", slack.NewHandler(slack.Deps{
		Pool:         pool,
		Sessions:     sessionStore,
		Turns:        turnStore,
		Environments: environmentStore,
		Registry:     registry,
		Deliveries:   webhookDeliveryStore,
		Threads:      slackThreadSessionStore,
		AuditLog:     auditLogStore,
		// Plans (a follow-up fix, §8.1): the SAME planStore
		// instance every other caller above already uses -- handleEvent's
		// own awaiting-plan gate/verdict/revise-prefix check (handler.go)
		// needs this to find a mapped session's own awaiting_approval plan,
		// if any, exactly like Linear's identical Deps.Plans wiring below.
		Plans: planStore,
		// Outbox/LinearAgentSessions (this batch's own addition, "honour a
		// typed plan verdict"): handlePlanVerdict's own httpapi.DecidePlan
		// call (handler.go) needs these exactly like the interactivity
		// route's own identical Outbox/LinearAgentSessions wiring
		// immediately below -- the SAME outboxStore/linearAgentSessionStore
		// instances every other caller of DecidePlan already uses, never a
		// second, independently-constructed copy.
		Outbox:              outboxStore,
		LinearAgentSessions: linearAgentSessionStore,
		// Participants (this Step's own SECOND fix-pass addition,
		// "identities + full RBAC", §13.2/§13.3): the SAME participantStore
		// instance every other caller (the interactivity route immediately
		// below, Linear's own Deps) already uses, never a second,
		// independently-constructed copy.
		Participants:     participantStore,
		IntentClassifier: intentClassifierSvc,
		// EpistemicCheckDefault (§20.4): the SAME platform.Config
		// value every other CreateTurnCore-reaching caller below also
		// receives.
		EpistemicCheckDefault: cfg.EpistemicCheckDefault,
		// RolloutMode/RepoSettings (§10 Phase 6, §32): the SAME
		// cfg.RolloutMode/repoSettingsStore every other CreateSessionCore-
		// reaching caller in this file also receives.
		RolloutMode:     cfg.RolloutMode,
		RepoSettings:    repoSettingsStore,
		SigningSecret:   cfg.SlackSigningSecret,
		DefaultRepoName: cfg.SlackDefaultRepoName,
		DefaultRepoURL:  cfg.SlackDefaultRepoURL,
		TimestampWindow: cfg.Timeouts.WebhookTimestampFreshnessWindow,
		AckTimeout:      cfg.Timeouts.SlackAckTimeout,
		// IdentityLink/SlackClient/Timeouts ("identities + full
		// RBAC", §13.2): SlackClient is the shadowslack-decorated wrapper
		// (§30.3) around the SAME slackNotifier instance already
		// constructed above (for the outbox delivery worker and the
		// interactivity route immediately below), never a third,
		// independently-constructed, gate-free client.
		IdentityLink: appIdentityLinkDeps,
		SlackClient:  slackDecorated,
		Timeouts:     cfg.Timeouts,
	}))

	// Slack INTERACTIVITY ingress ("plan mode, cross-channel",
	// §8.1/§13.3) -- a SEPARATE route from the Events API ingress
	// immediately above (structurally different payload shape; see
	// internal/adapters/inbound/slack/interactive.go's own top doc comment
	// for the real, external "Interactivity & Shortcuts" App-config step
	// this route requires before Slack ever sends it anything). Mounted
	// OUTSIDE auth.Middleware entirely, mirroring the Events API route
	// exactly -- authenticated via Slack's own request signature, not a
	// cookie.
	router.Post("/webhooks/slack/interactive", slack.NewInteractivityHandler(slack.InteractiveDeps{
		Pool:                pool,
		Sessions:            sessionStore,
		Turns:               turnStore,
		Plans:               planStore,
		Outbox:              outboxStore,
		LinearAgentSessions: linearAgentSessionStore,
		Registry:            registry,
		SlackClient:         slackDecorated,
		AuditLog:            auditLogStore,
		IdentityLink:        appIdentityLinkDeps,
		// Participants ("identities + full RBAC", §13.2/§13.3):
		// the SAME participantStore instance §8.1's own REST plan
		// approve/reject endpoints already use (constructed once, above),
		// never a second, independently-constructed copy.
		Participants: participantStore,
		// EpistemicCheckDefault (§20.4): see slack.Deps' own
		// identical field above -- this route's own CreateTurnCore call
		// always names planMode=true, so this value never actually
		// changes behavior here today (§20.3), but is threaded for the
		// same "correct by construction" reason documented on
		// InteractiveDeps.EpistemicCheckDefault itself.
		EpistemicCheckDefault: cfg.EpistemicCheckDefault,
		SigningSecret:         cfg.SlackSigningSecret,
		Timeouts:              cfg.Timeouts,
	}))

	// GitHub webhook ingress ("GitHub ingress", §8.2): mounted
	// OUTSIDE auth.Middleware entirely, mirroring scm-credentials/
	// snapshot-mint immediately above exactly -- this route authenticates
	// via GitHub's own HMAC webhook signature, not a browser cookie. See
	// internal/adapters/inbound/github's own doc.go for the full
	// verify -> dedupe-claim -> parse -> detect -> per-PR-coalesce
	// sequencing.
	router.Post("/webhooks/github", githubingress.NewHandler(
		&githubingress.SessionCoalescer{
			Pool:             pool,
			PRSessions:       githubPRSessionStore,
			Sessions:         sessionStore,
			Turns:            turnStore,
			Environments:     environmentStore,
			Registry:         registry,
			IntentClassifier: intentClassifierSvc,
			AuditLog:         auditLogStore,
			// Identities/Users/Participants (batch fix/audit-github-actor-
			// rbac): the SAME identityStore/userStore/participantStore
			// instances every other caller above already uses (§13.1's
			// own auth wiring, §8.1's own plan approve/reject
			// endpoints), never a second, independently-constructed copy.
			Identities:   identityStore,
			Users:        userStore,
			Participants: participantStore,
			// Plans (a follow-up fix, §8.1): the SAME planStore
			// instance every other caller above already uses -- threaded
			// through to CreateTurnForBot's own awaiting-plan gate.
			Plans: planStore,
			// ReviewTriage/ReviewModelDeep (§26.3): the SAME
			// repoSettingsStore/reviewVerdictStore instances every other
			// caller above already uses, never a second, independently-
			// constructed copy.
			ReviewTriage: appreviewtriage.Deps{
				RepoSettings:   repoSettingsStore,
				ReviewVerdicts: reviewVerdictStore,
				Artifacts:      artifactStore,
				Sessions:       sessionStore,
			},
			ReviewModelDeep: cfg.ReviewModelDeep,
			// RolloutMode/RepoSettings (§10 Phase 6, §32): the
			// SAME cfg.RolloutMode/repoSettingsStore every other
			// CreateSessionOnTx-reaching caller in this file also
			// receives -- a DEDICATED field pair, not a reuse of
			// ReviewTriage.RepoSettings immediately above (see
			// SessionCoalescer.RolloutMode's own doc comment for why).
			RolloutMode:  cfg.RolloutMode,
			RepoSettings: repoSettingsStore,
			// F7 correction (adversarial review): SessionCoalescer
			// no longer has an EpistemicCheckDefault field -- both of its
			// own CreateSessionOnTx/CreateTurnForBot call sites now
			// hardcode false instead (coalesce.go's own doc comment on the
			// removed field explains the full "why": every session/turn
			// this package creates or joins is a PR review session, never a
			// build turn, so the platform's real epistemic-check default
			// must never reach it).
		},
		webhookDeliveryStore,
		githubingress.Config{
			WebhookSecret: cfg.GitHubWebhookSecret,
			BotHandle:     cfg.GitHubBotHandle,
			// ReReviewLabel/DiffFetcher ("review sessions", §8.2):
			// the manual re-trigger-via-label lane's own configured label
			// name, and the SAME instance already
			// constructed above (sourceControl) as PullRequests/Comments --
			// never a second, independently-constructed copy -- now ALSO
			// wired as this Step's own diff/stack pre-fetch source.
			ReReviewLabel: cfg.GitHubReReviewLabel,
			DiffFetcher:   sourceControl,
			// ReviewFindings (§22.1): the SAME reviewFindingStore
			// instance every other caller above already uses.
			ReviewFindings: reviewFindingStore,
			// FalsePositivePatterns (§22.3): the SAME
			// falsePositivePatternStore instance every other caller
			// (RetriggerReview, the capture/lifecycle endpoints below)
			// already uses.
			FalsePositivePatterns: falsePositivePatternStore,
			// FalsePositivePatternCapture (§22.2): the SAME
			// falsePositivePatternStore instance, satisfying this
			// structurally different (write) interface.
			FalsePositivePatternCapture: falsePositivePatternStore,
			// ArchRecapContestCapture/ArchRecapVerdicts (§26.5):
			// reviewDigestSectionFeedbackStore is the SAME instance this
			// deployment has exactly one of; reviewVerdictDeps is the SAME
			// bundle every other review-verdict reader in this file already
			// shares (constructed once, above, alongside reviewVerdictStore).
			ArchRecapContestCapture: reviewDigestSectionFeedbackStore,
			ArchRecapVerdicts:       reviewVerdictDeps,
			// BotToken/PullRequests (batch fix/audit-github-pr-payload-
			// correctness, H5 audit fix): resolve an issue_comment
			// mention's TRUE head branch/repo via one authenticated
			// GET /repos/{owner}/{repo}/pulls/{number} call. sourceControl
			// is the SAME instance already constructed
			// above for CreatePR/ResolveBranchSHA/ResolveContractsFingerprint
			// -- never a second, independently-constructed copy -- and
			// cfg.GitHubBotToken is the SAME bot credential githubNotifier
			// (below) already authenticates its own PostIssueComment calls
			// with, never a per-commenter credential.
			BotToken:     cfg.GitHubBotToken,
			PullRequests: sourceControl,
			// Comments (a follow-up fix, Finding 1; also posts
			// batch fix/deny-unlinked-github-actors' own "please sign in"
			// reply): the SAME *githubapi.Adapter instance as
			// PullRequests above -- never a second, independently-
			// constructed copy. It is liveSourceControl rather than the
			// decorator because PostIssueComment lives outside the port;
			// its suppression comes from the transport gate underneath,
			// which is why that layer exists (§30.2).
			Comments: liveSourceControl,
			Timeouts: cfg.Timeouts,
			// PublicBaseURL/LinkNotices (batch fix/deny-unlinked-github-
			// actors): PublicBaseURL is the SAME base identitylink.
			// BuildMagicLinkURL already uses (appIdentityLinkDeps above),
			// never a second, independently-configured base. LinkNotices
			// is a freshly constructed store over the SAME pool every
			// other store here already shares -- see
			// githubActorLinkNoticeStore's own construction below.
			PublicBaseURL: cfg.PublicBaseURL,
			LinkNotices:   githubActorLinkNoticeStore,
			// SentinelFixes/RepoSettings/AuditLog (§17.4/§17.5):
			// the SAME instances every other caller above already uses.
			SentinelFixes: sentinelFixStore,
			RepoSettings:  repoSettingsStore,
			AuditLog:      auditLogStore,
			// PendingChecks/ReleaseLabel/ReleaseBranchPattern (
			// "release PR review", §15; PendingChecks itself is
			// blocking-finding fix #1): releaseManifestPendingStore is the
			// SAME instance constructed above, alongside outboxStore --
			// the webhook handler only ever writes ONE cheap row here now;
			// the actual check (ListMergedBetween, sourceControl, and
			// outboxStore's own release_manifest row) runs LATER, on
			// releaseManifestWorker's own background loop (started below,
			// alongside every other background loop), never inline on
			// this request's own context.
			PendingChecks:        releaseManifestPendingStore,
			ReleaseLabel:         cfg.GitHubReleaseLabel,
			ReleaseBranchPattern: cfg.GitHubReleaseBranchPattern,
			// Timers (§24.1): the standalone timerStore instance
			// constructed above, backing this lane's own direct,
			// actor-bypassing review_retrigger_debounce timer arm.
			Timers: timerStore,
		},
	))

	// Auth routes (§13.1/§13.4): how a session is obtained/
	// discarded in the first place, so — obviously — mounted OUTSIDE any
	// auth gate. See internal/adapters/inbound/auth's own doc.go for the
	// full routes/outcome-table writeup.
	router.Get("/auth/github/login", auth.NewLoginHandler(oauthConfig, cfg.Timeouts, secureCookies))
	router.Get("/auth/github/callback", auth.NewCallbackHandler(
		pool,
		oauthConfig,
		userStore,
		identityStore,
		auditLogStore,
		userSessionStore,
		allowlist,
		cfg.InitialAdminEmails,
		cfg.TokenEncryptionKey,
		cfg.Timeouts,
		secureCookies,
		githubAPIBaseURL,
	))
	router.Post("/auth/logout", auth.NewLogoutHandler(userSessionStore, secureCookies))

	// /auth/identity-link/{nonce}: the magic-link consume flow (
	// "identities + full RBAC", §13.2 step 4's own "connect your account"
	// link) -- deliberately mounted OUTSIDE auth.Middleware entirely, like
	// the auth routes immediately above: this handler authenticates the
	// visitor ITSELF (auth.Authenticate), redirecting through the SAME
	// GitHub OAuth login flow above (with its own ?next= carrying them
	// back here) when they aren't signed in yet, rather than the bare 401
	// Middleware would give a not-yet-authenticated request. See internal/
	// adapters/inbound/identitylink's own doc.go for the complete design.
	router.Get("/auth/identity-link/{nonce}", identitylinkhttp.NewConsumeHandler(identitylinkhttp.Deps{
		UserSessions:    userSessionStore,
		Users:           userStore,
		AppIdentityLink: appIdentityLinkDeps,
	}))

	// /api/members, /api/audit-log ("identities + full RBAC",
	// §13.2/§13.3): the backend-only members API -- list members (with
	// role, linked identities, pending-link state), an admin-only
	// role-change endpoint, admin manual link/unlink of an identity, and a
	// read endpoint over audit_log. Gated behind auth.Middleware (a "must
	// be logged in" gate) exactly like /api/sessions above; each handler
	// itself renders the REAL admin-only §13.3 verdict via
	// domain/authz.Authorize. The actual Settings -> Members UI is Phase 7
	// and out of scope here -- see httpapi/members.go's own doc comment.
	router.Route("/api/members", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.ListMembers(userStore, identityStore, identityLinkPromptStore))
		r.Patch("/{userID}/role", httpapi.UpdateMemberRole(pool, userStore, identityStore, auditLogStore))
		r.Post("/{userID}/identities", httpapi.LinkMemberIdentity(pool, userStore, identityStore, auditLogStore))
		r.Delete("/{userID}/identities/{identityID}", httpapi.UnlinkMemberIdentity(pool, identityStore, auditLogStore))
	})

	// /api/integrations (§12.5's own "integrations read model & routes"
	// amendment): one row per ingress surface (Slack, Linear, GitHub) --
	// see httpapi/integrations.go's own doc comment. A DERIVED read, no
	// connect/disconnect write, so this is a GET-only route group,
	// deployment-wide like /api/members above (no {owner}/{repo} in the
	// path -- integrations are not per-repo). Gated by the EXISTING
	// authz.ActionManageIntegrations (admin only, §13.3 row 6); the
	// handler itself renders the real verdict.
	router.Route("/api/integrations", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.GetIntegrations(cfg, outboxStore, webhookDeliveryStore))
	})

	// /api/me ("web UI: sign-in", §12.2 item 7/§13.1): the
	// "who am I" endpoint the sign-in view's identity auto-link panel and
	// already-signed-in state read -- gated behind auth.Middleware only,
	// like every other route in this file, with the handler itself
	// rendering the real authz.ActionViewOwnProfile verdict (everyone
	// including viewer, §13.3 row 1 -- see that action's own doc comment
	// for why, and httpapi/me.go's own doc comment for why this is
	// deliberately NOT /api/members with a self filter).
	router.Route("/api/me", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.GetMe(userStore, identityStore))
	})

	// /api/me/chatgpt-link ("models: Codex via ChatGPT-account
	// OAuth", §29.3/§29.9): self-service link/status/unlink -- gated
	// behind auth.Middleware exactly like /api/members above; each
	// handler renders the real authz.ActionLinkChatGPTAccount verdict
	// (own-aware, satisfied unconditionally here since every one of these
	// three handlers only ever acts on the caller's OWN userID, never a
	// path parameter naming a different user -- see chatgptlink.go's own
	// doc comment for why there is no admin "/api/members/{userID}/
	// chatgpt-link" surface yet).
	router.Route("/api/me/chatgpt-link", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Post("/", httpapi.StartChatGPTLink(chatGPTLinkDeps))
		r.Get("/", httpapi.GetChatGPTLinkStatus(chatGPTLinkDeps))
		r.Delete("/", httpapi.DeleteChatGPTLink(chatGPTLinkDeps))
	})

	// /api/models ("models: Catalog", §8 item 8/§29/§25.2) --
	// mounted exactly like /api/members above: gated behind auth.
	// Middleware only, with the handler itself rendering the real
	// authz.ActionViewAnalytics verdict (everyone including viewer).
	router.Route("/api/models", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.GetModelCatalog())
	})

	// /api/admin/shadow-compare ("shadow-comparison tooling for
	// review", §9.4/§18.5) -- mounted exactly like /api/members above:
	// gated behind auth.Middleware, with the handler itself rendering the
	// real authz.ActionViewShadowComparison verdict (admin/maintainer
	// only).
	router.Route("/api/admin/shadow-compare", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.GetShadowComparison(turnStore))
	})
	router.Route("/api/audit-log", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.ListAuditLog(auditLogStore))
	})

	// /api/decision-inbox ("decision inbox: read model + API",
	// §16 -- Phase 5 half: read model + endpoints; the UI is Phase 7).
	// decisionInboxDeps bundles every Postgres store the read model
	// aggregates (internal/app/decisioninbox.Build's own doc comment: a
	// read model over plans/sessions/automations/outbox/review_findings/
	// sentinel_fixes/artifacts, all already constructed above for their
	// own existing purposes) plus decisionInboxSCMCache, the §16.2 short-
	// TTL cache wrapping the SAME sourceControl instance every other
	// GitHub-facing route already shares.
	decisionInboxSCMCache := decisioninbox.NewSCMCache(sourceControl, cfg.Timeouts)
	decisionInboxDeps := decisioninbox.Deps{
		Plans:              planStore,
		Sessions:           sessionStore,
		Participants:       participantStore,
		Automations:        automationStore,
		Outbox:             outboxStore,
		ReviewFindings:     reviewFindingStore,
		SentinelFixes:      sentinelFixStore,
		Artifacts:          artifactStore,
		Identities:         identityStore,
		SCMCache:           decisionInboxSCMCache,
		TokenEncryptionKey: cfg.TokenEncryptionKey,
		Timeouts:           cfg.Timeouts,
		ReviewVerdict:      reviewVerdictDeps,
	}

	// automergeWorker/digestPump (§21.2 stage 2/§21.3): both
	// started below, alongside every other background loop, through the
	// SAME errgroup (§11: no naked goroutine). automergeWorker reuses
	// decisionInboxDeps in full (internal/app/decisioninbox.
	// RevalidateForAutoMerge, its own re-validation call, needs every
	// store that function already depends on) plus sourceControl/
	// cfg.GitHubBotToken -- the bot credential, since a background
	// worker has no clicking human's own token to reuse (see
	// automerge.Deps' own doc comment).
	automergeWorker := automerge.New(automerge.Deps{
		DecisionInbox: decisionInboxDeps,
		SourceControl: sourceControl,
		AuditLog:      auditLogStore,
		BotToken:      cfg.GitHubBotToken,
		Timeouts:      cfg.Timeouts,
	})
	digestPump := digest.New(digest.Deps{
		Channels:      digestChannelStore,
		SendState:     digestSendStateStore,
		Outbox:        outboxStore,
		ReviewVerdict: reviewVerdictDeps,
		Timeouts:      cfg.Timeouts,
	})

	router.Route("/api/decision-inbox", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.ListDecisionInbox(decisionInboxDeps))
		r.Post("/merge", httpapi.MergePullRequest(decisionInboxDeps, sourceControl, auditLogStore))
	})

	// /api/intent-templates, /api/intent-templates/preview (audit finding
	// M5, completeness): postgres.PromptTemplateStore's own Upsert method
	// (constructed above, alongside intentClassifierSvc) had ZERO callers
	// anywhere in this codebase until this batch -- these two routes are
	// its first ones. Gated behind auth.Middleware (a "must be logged in"
	// gate) exactly like /api/members above; each handler itself renders
	// the REAL admin-only §13.3 verdict via domain/authz.Authorize
	// (authz.ActionActivatePromptTemplate -- row 6's own "prompt-template
	// activation" action, itself likewise unused anywhere before this
	// batch). See httpapi/classifiertemplates.go's own doc comment for the
	// full design.
	router.Route("/api/intent-templates", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Post("/preview", httpapi.PreviewIntentTemplate())
		r.Post("/", httpapi.UpsertIntentTemplate(pool, promptTemplateStore, auditLogStore))
		// GET / ("ui settings + analytics", §12.2 item 5): the
		// Settings -> Prompt templates screen's own list data source --
		// see httpapi/classifiertemplates.go's own ListPromptTemplates doc
		// comment for why this reuses the SAME admin-only gate as
		// preview/upsert above.
		r.Get("/", httpapi.ListPromptTemplates(promptTemplateStore))
	})

	// /api/environments ("ui settings + analytics", §12.2 item 5):
	// the first standalone read over the environments table -- see
	// httpapi/environments.go's own doc comment for the full "why a list
	// endpoint, why not full CRUD" design. Mounted behind auth.Middleware
	// like every other browser-facing REST route in this package.
	router.Route("/api/environments", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.ListEnvironments(environmentStore))
	})

	// REST routes the UI needs (§6.3, §6.2's own plan row: "create/get/
	// events/artifacts", + ws-token named separately by §6.2), all gated
	// behind auth.Middleware — see
	// internal/adapters/inbound/httpapi/doc.go's own updated writeup. This
	// is a "must be logged in" gate only: it does not apply to /health or
	// to GET /sessions/{sessionID}/ws above, which already has its OWN,
	// type-specific auth (the sandbox half's header-bearer-token handshake
	// and the client half's own post-upgrade ws-token subscribe message,
	// both §3.2/§6.2's own precedent, untouched) — gating the WS UPGRADE
	// itself behind a cookie check would break the sandbox-agent's own
	// connection, which carries no cookie at all, only its own
	// Authorization header.
	router.Route("/api/sessions", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Post("/", httpapi.CreateSession(pool, sessionStore, turnStore, environmentStore, auditLogStore, registry, intentClassifierSvc, cfg.EpistemicCheckDefault, cfg.RolloutMode, repoSettingsStore))
		// GET / (list, §12.2 item 1's own sidebar addition) -- mounted
		// alongside POST / above; chi disambiguates the two by method,
		// and separately from GET /{sessionID} below by the literal "/"
		// path never matching that param segment.
		r.Get("/", httpapi.ListSessions(sessionStore))
		r.Get("/{sessionID}", httpapi.GetSession(sessionStore))
		r.Get("/{sessionID}/events", httpapi.ListEvents(sessionStore, eventStore))
		r.Get("/{sessionID}/artifacts", httpapi.ListArtifacts(sessionStore, artifactStore))
		// uploads ("uploads, blob storage & the in-sandbox
		// download_file tool", §28.4/§28.5): the browser twins of the
		// sandbox-bearer mint/confirm/content endpoints registered outside
		// /api above. mint/confirm are gated by authz.ActionUploadToSession
		// (the same §13.3 row as prompting, checked inside each handler);
		// content/download is gated by session visibility only (a download
		// is a read, so a read-only viewer may) -- mirrors ListArtifacts/
		// ListEvents immediately above, no separate Authorize call.
		r.Post("/{sessionID}/uploads", httpapi.MintUploadAPI(sessionStore, participantStore, artifactStore, blobStore, cfg.ObjectStorage, cfg.Timeouts))
		r.Post("/{sessionID}/uploads/{uploadID}/complete", httpapi.ConfirmUploadAPI(sessionStore, participantStore, pool, artifactStore, eventStore, outboxStore, sandboxStore, hub, blobStore, cfg.ObjectStorage))
		r.Get("/{sessionID}/uploads/{uploadID}/content", httpapi.UploadContentAPI(sessionStore, artifactStore, blobStore, cfg.ObjectStorage, cfg.Timeouts))
		r.Post("/{sessionID}/ws-token", httpapi.MintWSToken(sessionStore, wsTokenStore, cfg.Timeouts))
		// turns ("turn recovery", §8.7): the relaunch-and-resume
		// REST API -- enqueues a new turn on an existing session, 409 if
		// one is already in flight. See httpapi/turn.go's own doc comment.
		r.Post("/{sessionID}/turns", httpapi.CreateTurn(pool, sessionStore, turnStore, planStore, participantStore, auditLogStore, registry, intentClassifierSvc, cfg.ObjectStorage, cfg.EpistemicCheckDefault))
		// plans ("plan mode, web", §8.1/§12.2 item 3): the
		// approve/reject HITL actions -- see httpapi/planapprove.go's own
		// doc comment for the full sequencing. outboxStore/
		// linearAgentSessionStore (§8.1, "plan mode, cross-channel") feed
		// DecidePlanOnTx's own cross-channel-notify step (decideplan.go).
		r.Post("/{sessionID}/plans/{planId}/approve", httpapi.ApprovePlan(pool, sessionStore, turnStore, planStore, participantStore, outboxStore, linearAgentSessionStore, auditLogStore, registry, cfg.EpistemicCheckDefault))
		r.Post("/{sessionID}/plans/{planId}/reject", httpapi.RejectPlan(pool, sessionStore, turnStore, planStore, participantStore, outboxStore, linearAgentSessionStore, auditLogStore, cfg.EpistemicCheckDefault))
		// Audit-fix batch (completeness/discoverability, M3): the read half
		// plans/{planId}/approve|reject above was always missing -- a web
		// client had no way to ever discover a planId to approve. See
		// httpapi/plans.go's own doc comment.
		r.Get("/{sessionID}/plans", httpapi.ListPlans(sessionStore, planStore, turnStore, eventStore))
		// review/retrigger ("review sessions", §8.2's own manual
		// re-trigger-via-BUTTON surface, §12.2 item 2's "re-run action") --
		// see httpapi/reviewretrigger.go's own doc comment. githubPRSessionStore/
		// sourceControl/cfg.GitHubBotToken are the SAME instances the
		// GitHub webhook ingress wiring above already constructs, never a
		// second, independently-constructed copy.
		r.Post("/{sessionID}/review/retrigger", httpapi.RetriggerReview(pool, sessionStore, turnStore, planStore, auditLogStore, registry, githubPRSessionStore, sourceControl, reviewFindingStore, falsePositivePatternStore, cfg.GitHubBotToken, cfg.Timeouts, appreviewtriage.Deps{RepoSettings: repoSettingsStore, ReviewVerdicts: reviewVerdictStore, Artifacts: artifactStore, Sessions: sessionStore}, cfg.ReviewModelDeep))
		// review/findings/{identityHash}/rebut + apply-suggestion (
		// "sentinels + suggestions", §12.2 item 2/§22.1) -- maintainer+
		// only (authz.ActionEditReviewVerdict, checked inside each
		// handler). identityStore/sourceControl/cfg.TokenEncryptionKey are
		// the SAME instances every other caller above already uses --
		// ApplySuggestion decrypts the ACTING (authenticated) maintainer's
		// own GitHub token, never the session creator's.
		r.Post("/{sessionID}/review/findings/{identityHash}/rebut", httpapi.RebutReviewFinding(sessionStore, githubPRSessionStore, reviewFindingStore, auditLogStore))
		r.Post("/{sessionID}/review/findings/{identityHash}/apply-suggestion", httpapi.ApplySuggestion(sessionStore, githubPRSessionStore, reviewFindingStore, identityStore, sourceControl, cfg.TokenEncryptionKey, cfg.Timeouts))
		// review (§26.1's merge readout, §12.2 item 2) -- the code-review
		// view's own read model, see httpapi/reviewreadout.go's own doc
		// comment. sourceControl/findingRelocationResolver/cfg.GitHubBotToken
		// are the SAME instances every other GitHub-facing route above
		// already uses.
		r.Get("/{sessionID}/review", httpapi.GetReviewReadout(sessionStore, githubPRSessionStore, reviewVerdictDeps, reviewFindingStore, turnStore, sourceControl, findingRelocationResolver, cfg.GitHubBotToken, cfg.Timeouts))
		// release-manifest (§15.2/§15.3, §12.2 item 9) -- the dedicated
		// release-review screen's own read model, see httpapi/
		// releasemanifestreadout.go's own doc comment.
		r.Get("/{sessionID}/release-manifest", httpapi.GetReleaseManifestReadout(sessionStore, githubPRSessionStore, releaseManifestCheckStore))
		// workflow-runs ("workflow definition & run API", §25.10): a
		// session's own runs, newest first -- the SAME session-read
		// gate every other route in this group uses (see httpapi/
		// workflowruns.go's own doc comment: session exists + logged in,
		// no separate authz.Authorize call).
		r.Get("/{sessionID}/workflow-runs", httpapi.ListSessionWorkflowRuns(sessionStore, workflowStore))
	})

	// /api/workflow-runs/{runId}/steps/{stepRunId}/decide ("workflow
	// HITL gate + circuit breaker", §25.9/§25.10/§25.11): the HITL
	// approve/reject/revise verdict endpoint -- see httpapi/decideworkflowstep.go's
	// own doc comment for the full sequencing. slackThreadSessionStore/
	// linearAgentSessionStore/githubPRSessionStore/outboxStore are the SAME
	// instances every other caller above already uses -- notification
	// destination resolution reuses those exact reverse-lookup stores,
	// never a second, independently-constructed copy.
	//
	// GET /{runId} ("workflow definition & run API", §25.10) is this
	// same group's own read-only twin -- one run WITH its ordered step
	// runs, see httpapi/workflowruns.go's own doc comment.
	router.Route("/api/workflow-runs", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Post("/{runId}/steps/{stepRunId}/decide", httpapi.DecideWorkflowStep(pool, sessionStore, turnStore, participantStore, workflowStore, slackThreadSessionStore, linearAgentSessionStore, githubPRSessionStore, outboxStore, registry, cfg.EpistemicCheckDefault))
		r.Get("/{runId}", httpapi.GetWorkflowRun(sessionStore, workflowStore))
	})

	// /api/workflow-definitions, /api/workflow-bindings ("workflow
	// definition & run API", §25.10/§25.11): the definition-
	// authoring surface (list/get/create-or-duplicate/replace/delete) and
	// the binding-activation surface §25.4 shipped dark -- see httpapi/
	// workflowdefinitions.go's own doc comment for the two structural
	// refusals (built-in, bound) plus a third guard added there (run
	// history), and httpapi/workflowbindings.go's own doc comment for the
	// two-partial-unique-index binding upsert. Definitions are gated by
	// authz.ActionManageWorkflowDefinitions (maintainer+); binding writes
	// are gated by authz.ActionActivateWorkflowBinding (admin-only) --
	// both checked inside each handler, mirroring every other
	// admin/maintainer-gated route group in this file.
	router.Route("/api/workflow-definitions", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.ListWorkflowDefinitions(workflowStore))
		r.Post("/", httpapi.CreateWorkflowDefinition(pool, workflowStore))
		r.Get("/{id}", httpapi.GetWorkflowDefinition(workflowStore))
		r.Put("/{id}", httpapi.PutWorkflowDefinition(pool, workflowStore))
		r.Delete("/{id}", httpapi.DeleteWorkflowDefinition(pool, workflowStore))
	})
	router.Route("/api/workflow-bindings", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.ListWorkflowBindings(workflowStore))
		r.Put("/", httpapi.PutWorkflowBinding(pool, workflowStore))
	})

	// /api/repos/{owner}/{repo}/settings ("server-side verdict",
	// §8.2/§21.2): admin-only read/write of a repo's own blockOnHighRisk
	// policy flag -- see httpapi/reposettings.go's own doc comment. Mounted
	// behind auth.Middleware like every other browser-facing REST route in
	// this package (unlike review/verdict below, this is an admin
	// configuring a setting, not the sandbox agent calling a tool).
	router.Route("/api/repos/{owner}/{repo}/settings", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.GetRepoSettings(repoSettingsStore, reviewVerdictDeps, githubPRSessionStore))
		r.Put("/", httpapi.PutRepoSettings(repoSettingsStore, githubPRSessionStore))
	})

	// /api/repos/{owner}/{repo}/preview-config (§4.1.2 amendment): a
	// FURTHER, separately-gated route -- deliberately NOT folded into
	// /settings above (a request body carrying a credential must not
	// share a shape with ordinary configuration) -- see
	// httpapi/previewconfig.go's own doc comment for the full "why a
	// dedicated GET too" reasoning. Gated by the NEW admin-only authz.
	// ActionConfigurePreviewLinks (§13.3 row 6).
	router.Route("/api/repos/{owner}/{repo}/preview-config", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.GetPreviewConfig(repoSettingsStore, githubPRSessionStore))
		r.Put("/", httpapi.PutPreviewConfig(repoSettingsStore, githubPRSessionStore))
	})

	// /api/repos/{owner}/{repo}/shadow-ledger[/activate] (§30.6/§30.8/§30.9):
	// the shadow-operator surface's own read model
	// (GET) and graduation gesture (POST .../activate) -- see
	// httpapi/shadowledger.go's own doc comment. Gated by the NEW
	// admin-only authz.ActionViewShadowLedger/ActionActivateShadowLedger,
	// deliberately carrying no §13.3 table row (that action's own doc
	// comment explains why). shadowOperatorReads/shadowLedger are the SAME
	// stores githubapi's own shadow transport gate and port decorator are
	// already wired with above -- this surface only ever READS what they
	// already write, never a second writer.
	router.Route("/api/repos/{owner}/{repo}/shadow-ledger", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.GetShadowLedger(shadowLedger, shadowOperatorReads, repoSettingsStore, githubPRSessionStore))
		r.Post("/activate", httpapi.PostActivateShadowLedger(shadowLedger, shadowOperatorReads, repoSettingsStore, auditLogStore, githubPRSessionStore))
	})

	// /api/repos/{owner}/{repo}/false-positive-patterns ("review:
	// learned false-positive patterns", §22.4): the audit-view/retire
	// lifecycle surface -- see httpapi/falsepositivepatterns.go's own doc
	// comment. Capture itself (§22.2) has no REST route at all; it is the
	// GitHub webhook's own dispatch-before-router `false positive:
	// <reason>` command instead. Gated by authz.
	// ActionManageFalsePositivePatterns (maintainer+, §13.3 row 5) and
	// mounted behind auth.Middleware, mirroring every other browser-facing
	// REST route in this package.
	router.Route("/api/repos/{owner}/{repo}/false-positive-patterns", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.ListFalsePositivePatterns(falsePositivePatternStore, githubPRSessionStore))
		r.Post("/{patternID}/retire", httpapi.RetireFalsePositivePattern(falsePositivePatternStore, auditLogStore, githubPRSessionStore))
	})

	// /api/repos/{owner}/{repo}/auto-approval-settings,
	// /api/repos/{owner}/{repo}/auto-merge (§21.2): TWO further,
	// separately-gated routes -- see httpapi/reposettings.go's own
	// PutAutoApprovalSettings/PutAutoMergeToggle doc comments for why
	// these are not folded into PUT /settings above (a maintainer
	// authorized only for the auto-approval-config row, §13.3 row 5,
	// must never be forced through that endpoint's own admin-only gates,
	// row 6, just to reach it).
	router.Route("/api/repos/{owner}/{repo}/auto-approval-settings", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Put("/", httpapi.PutAutoApprovalSettings(repoSettingsStore, reviewVerdictDeps, githubPRSessionStore))
	})
	router.Route("/api/repos/{owner}/{repo}/auto-merge", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Put("/", httpapi.PutAutoMergeToggle(repoSettingsStore, reviewVerdictDeps, githubPRSessionStore))
	})

	// /api/repos/{owner}/{repo}/auto-retrigger-review (§24.5): a
	// further, separately-gated route mirroring auto-merge above -- see
	// httpapi/reposettings.go's own PutAutoRetriggerReviewToggle doc
	// comment.
	router.Route("/api/repos/{owner}/{repo}/auto-retrigger-review", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Put("/", httpapi.PutAutoRetriggerReviewToggle(repoSettingsStore, githubPRSessionStore))
	})

	// /api/repos/{owner}/{repo}/description-autofix (§26.2): a
	// further, separately-gated route mirroring auto-retrigger-review
	// above -- see httpapi/reposettings.go's own
	// PutDescriptionAutofixToggle doc comment.
	router.Route("/api/repos/{owner}/{repo}/description-autofix", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Put("/", httpapi.PutDescriptionAutofixToggle(repoSettingsStore, githubPRSessionStore))
	})

	// /api/repos/{owner}/{repo}/review-depth (§26.3): a further,
	// separately-gated route mirroring description-autofix above -- see
	// httpapi/reposettings.go's own PutReviewDepthConfig doc comment.
	router.Route("/api/repos/{owner}/{repo}/review-depth", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Put("/", httpapi.PutReviewDepthConfig(repoSettingsStore, githubPRSessionStore))
	})

	// /api/repos/{owner}/{repo}/review-cost-budget (§26.7): a
	// further, separately-gated route mirroring review-depth above -- see
	// httpapi/reposettings.go's own PutReviewCostBudget doc comment.
	router.Route("/api/repos/{owner}/{repo}/review-cost-budget", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Put("/", httpapi.PutReviewCostBudget(repoSettingsStore, githubPRSessionStore))
	})

	// /api/repos/{owner}/{repo}/review-analytics (§21.1):
	// read-only GET over the three analytics rollups (timeseries,
	// top-risk-driver breakdown, "Review finding outcomes" KPI) -- see
	// httpapi/reviewanalytics.go's own doc comment. Gated by the existing
	// authz.ActionViewAnalytics (§13.3 row 1: every role, including
	// viewer), unlike every §21.2 write-side route above.
	router.Route("/api/repos/{owner}/{repo}/review-analytics", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.GetReviewAnalytics(reviewVerdictDeps, githubPRSessionStore))
	})

	// /api/repos/{owner}/{repo}/digest-scope ("ui settings +
	// analytics", §12.2 item 5, §21.3): read-only derived view of which
	// Slack channels/Linear organizations are in scope for this repo's own
	// daily digest -- "in scope for", not "will receive": see
	// httpapi/digestscope.go's own doc comment both for why this is
	// read-only (§21.3's scope is computed, never stored) and for the
	// pump's repo-enumeration cap that sits in front of this derivation.
	// Gated by the SAME authz.ActionViewAnalytics as review-analytics
	// immediately above.
	router.Route("/api/repos/{owner}/{repo}/digest-scope", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.GetRepoDigestScope(digestChannelStore, githubPRSessionStore, cfg.Timeouts))
	})

	// /api/repos/{owner}/{repo}/provider-credentials,
	// /api/environments/{environmentID}/provider-credentials,
	// /api/provider-credentials ("provider credential injection",
	// §25.1/§25.3): the 3 scope-partitioned CRUD route groups over
	// provider_credentials -- see httpapi/providercredentials.go's own doc
	// comment for the full route table and RBAC-per-scope rationale. Each
	// handler renders its own §13.3 verdict via domain/authz.Authorize
	// (ActionManageRepoSecrets/ActionManageEnvSecrets/
	// ActionManageGlobalSecrets respectively) -- mounted behind
	// auth.Middleware like every other browser-facing REST route in this
	// package (unlike provider-credentials' OWN sandbox-facing sibling
	// above, this is an admin/maintainer configuring a secret, not the
	// sandbox agent fetching one).
	router.Route("/api/repos/{owner}/{repo}/provider-credentials", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Post("/", httpapi.CreateRepoProviderCredential(providerCredentialStore, cfg.TokenEncryptionKey, githubPRSessionStore))
		r.Get("/", httpapi.ListRepoProviderCredentials(providerCredentialStore, githubPRSessionStore))
		r.Put("/{credentialID}", httpapi.UpdateRepoProviderCredentialValue(providerCredentialStore, cfg.TokenEncryptionKey, githubPRSessionStore))
		r.Delete("/{credentialID}", httpapi.DeleteRepoProviderCredential(providerCredentialStore, githubPRSessionStore))
	})
	router.Route("/api/environments/{environmentID}/provider-credentials", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Post("/", httpapi.CreateEnvironmentProviderCredential(providerCredentialStore, cfg.TokenEncryptionKey))
		r.Get("/", httpapi.ListEnvironmentProviderCredentials(providerCredentialStore))
		r.Put("/{credentialID}", httpapi.UpdateEnvironmentProviderCredentialValue(providerCredentialStore, cfg.TokenEncryptionKey))
		r.Delete("/{credentialID}", httpapi.DeleteEnvironmentProviderCredential(providerCredentialStore))
	})
	router.Route("/api/provider-credentials", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Post("/", httpapi.CreateGlobalProviderCredential(providerCredentialStore, cfg.TokenEncryptionKey))
		r.Get("/", httpapi.ListGlobalProviderCredentials(providerCredentialStore))
		r.Put("/{credentialID}", httpapi.UpdateGlobalProviderCredentialValue(providerCredentialStore, cfg.TokenEncryptionKey))
		r.Delete("/{credentialID}", httpapi.DeleteGlobalProviderCredential(providerCredentialStore))
	})

	// /api/repos/{owner}/{repo}/sandbox-secrets,
	// /api/environments/{environmentID}/sandbox-secrets,
	// /api/sandbox-secrets ("sandbox secrets & opencode config",
	// §27.1): the 3 scope-partitioned CRUD route groups over
	// sandbox_secrets -- mirrors the 3 provider-credentials route groups
	// immediately above verbatim (see httpapi/sandboxsecrets.go's own doc
	// comment for the full route table and RBAC-per-scope rationale).
	// Deliberately NO automation-scoped route group -- §27.1's own
	// schema-only carve-out for that scope.
	router.Route("/api/repos/{owner}/{repo}/sandbox-secrets", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Post("/", httpapi.CreateRepoSandboxSecret(sandboxSecretStore, cfg.TokenEncryptionKey, githubPRSessionStore))
		r.Get("/", httpapi.ListRepoSandboxSecrets(sandboxSecretStore, githubPRSessionStore))
		r.Put("/{secretID}", httpapi.UpdateRepoSandboxSecretValue(sandboxSecretStore, cfg.TokenEncryptionKey, githubPRSessionStore))
		r.Delete("/{secretID}", httpapi.DeleteRepoSandboxSecret(sandboxSecretStore, githubPRSessionStore))
	})
	router.Route("/api/environments/{environmentID}/sandbox-secrets", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Post("/", httpapi.CreateEnvironmentSandboxSecret(sandboxSecretStore, cfg.TokenEncryptionKey))
		r.Get("/", httpapi.ListEnvironmentSandboxSecrets(sandboxSecretStore))
		r.Put("/{secretID}", httpapi.UpdateEnvironmentSandboxSecretValue(sandboxSecretStore, cfg.TokenEncryptionKey))
		r.Delete("/{secretID}", httpapi.DeleteEnvironmentSandboxSecret(sandboxSecretStore))
	})
	router.Route("/api/sandbox-secrets", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Post("/", httpapi.CreateGlobalSandboxSecret(sandboxSecretStore, cfg.TokenEncryptionKey))
		r.Get("/", httpapi.ListGlobalSandboxSecrets(sandboxSecretStore))
		r.Put("/{secretID}", httpapi.UpdateGlobalSandboxSecretValue(sandboxSecretStore, cfg.TokenEncryptionKey))
		r.Delete("/{secretID}", httpapi.DeleteGlobalSandboxSecret(sandboxSecretStore))
	})

	// /api/environments/{environmentID}/cloud-identity-bindings,
	// /api/cloud-identity-bindings ("cloud identity: OIDC
	// issuer, bindings, minting", §27.3): the 2 scope-partitioned CRUD
	// route groups over cloud_identity_bindings -- narrower than
	// provider-credentials/sandbox-secrets' own 3-way split (no repo
	// scope, §27.3), and BOTH groups share the SAME action
	// (ActionManageCloudIdentityBindings, maintainer+) rather than
	// escalating global scope to admin-only the way those two tables do
	// -- see that action's own doc comment for why (params here are
	// identifiers, never secrets). Mounted behind auth.Middleware like
	// every other browser-facing REST route in this package -- see
	// httpapi/cloudidentitybindings.go's own doc comment for the full
	// route table. ALSO mounted behind httpapi's own
	// requireCloudIdentityCapability(cfg.CloudIdentityIssuerURL) --
	// §27.3's own explicit "binding CRUD refuses, fail-closed, when
	// unset" requirement, applied once per group (see that middleware's
	// own doc comment for why a group-level r.Use(...) beats a per-handler
	// inline check repeated 4 times).
	router.Route("/api/environments/{environmentID}/cloud-identity-bindings", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Use(httpapi.RequireCloudIdentityCapability(cfg.CloudIdentityIssuerURL))
		r.Post("/", httpapi.CreateEnvironmentCloudIdentityBinding(pool, cloudIdentityBindingStore, auditLogStore))
		r.Get("/", httpapi.ListEnvironmentCloudIdentityBindings(cloudIdentityBindingStore))
		r.Put("/{bindingID}", httpapi.UpdateEnvironmentCloudIdentityBinding(pool, cloudIdentityBindingStore, auditLogStore))
		r.Delete("/{bindingID}", httpapi.DeleteEnvironmentCloudIdentityBinding(pool, cloudIdentityBindingStore, auditLogStore))
	})
	router.Route("/api/cloud-identity-bindings", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Use(httpapi.RequireCloudIdentityCapability(cfg.CloudIdentityIssuerURL))
		r.Post("/", httpapi.CreateGlobalCloudIdentityBinding(pool, cloudIdentityBindingStore, auditLogStore))
		r.Get("/", httpapi.ListGlobalCloudIdentityBindings(cloudIdentityBindingStore))
		r.Put("/{bindingID}", httpapi.UpdateGlobalCloudIdentityBinding(pool, cloudIdentityBindingStore, auditLogStore))
		r.Delete("/{bindingID}", httpapi.DeleteGlobalCloudIdentityBinding(pool, cloudIdentityBindingStore, auditLogStore))
	})
	// /api/cloud-identity/signing-keys/rotate (§27.3/§27.8):
	// the admin-only, manual rotation TRIGGER -- see
	// httpapi/cloudidentitykeys.go's own doc comment for the full
	// "why manual, admin-triggered" design (this Step's own gap-2
	// resolution) and internal/domain/oidckey's own doc comment for the
	// complete justification against §5.2's sandbox-token rotation
	// precedent. Admin only (ActionManageCloudIdentityKeys), one row
	// stricter than binding CRUD immediately above. ALSO mounted behind
	// the SAME requireCloudIdentityCapability gate as the two binding
	// groups above -- RotateCloudIdentitySigningKey itself no longer
	// carries its own inline issuerURL check, this group-level r.Use(...)
	// is now the ONLY place that decision is made for this route.
	router.Route("/api/cloud-identity/signing-keys", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Use(httpapi.RequireCloudIdentityCapability(cfg.CloudIdentityIssuerURL))
		r.Post("/rotate", httpapi.RotateCloudIdentitySigningKey(pool, oidcSigningKeyStore, auditLogStore, cfg.TokenEncryptionKey, cfg.Timeouts))
	})

	// /api/environments/{environmentID}/cluster-binding ("cloud
	// identity: sandbox-side consumption + kubeconfig injection", §27.4):
	// the ONE scope (environment-only, no global fallback -- §27.4's own
	// "one cluster per Environment in v1") GET/PUT/DELETE singleton route
	// group over cluster_bindings -- mirrors opencode-config's own
	// singleton-resource route shape (GET/PUT/DELETE, no POST/list) rather
	// than cloud-identity-bindings' own list-of-many-rows CRUD, since this
	// table has at most one row per Environment. authz.ActionManageClusterBindings
	// (maintainer+, this Step's own row -- see that action's own doc
	// comment). Deliberately NOT mounted behind
	// RequireCloudIdentityCapability -- see httpapi/
	// cloudidentityconfigdelivery.go's own doc comment for why this
	// feature's own capability gate is scoped to §27.3's OIDC-issuer-
	// dependent surfaces only, never this table (a static-rung cluster
	// binding needs no OIDC issuer at all).
	router.Route("/api/environments/{environmentID}/cluster-binding", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.GetEnvironmentClusterBinding(clusterBindingStore))
		r.Put("/", httpapi.PutEnvironmentClusterBinding(clusterBindingStore))
		r.Delete("/", httpapi.DeleteEnvironmentClusterBinding(clusterBindingStore))
	})

	// /api/environments/{environmentID}/opencode-config,
	// /api/opencode-config (§27.2): the 2 scope-partitioned
	// GET/PUT/DELETE singleton route groups over opencode_configs --
	// reuses the SAME 2 actions (ActionManageEnvSecrets/
	// ActionManageGlobalSecrets) the sandbox-secrets/provider-credentials
	// environment/global route groups already use (see
	// httpapi/opencodeconfig.go's own doc comment for the full RBAC
	// rationale). Deliberately no repo-scoped route group -- §27.2 has no
	// repo scope at all (a repo's own committed opencode.json already
	// occupies OpenCode's native project slot).
	router.Route("/api/environments/{environmentID}/opencode-config", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.GetEnvironmentOpenCodeConfig(openCodeConfigStore))
		r.Put("/", httpapi.PutEnvironmentOpenCodeConfig(openCodeConfigStore))
		r.Delete("/", httpapi.DeleteEnvironmentOpenCodeConfigHandler(openCodeConfigStore))
	})
	router.Route("/api/opencode-config", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/", httpapi.GetGlobalOpenCodeConfig(openCodeConfigStore))
		r.Put("/", httpapi.PutGlobalOpenCodeConfig(openCodeConfigStore))
		r.Delete("/", httpapi.DeleteGlobalOpenCodeConfigHandler(openCodeConfigStore))
	})

	// /api/automations ("automations: triggers & extras", §8.4):
	// the CRUD surface §3.5 ("automations: engine") never built --
	// automationStore is the SAME instance automationEngine (constructed
	// above) already uses, never a second, independently-constructed copy.
	// Create/Pause/Resume/RotateAutomationWebhookToken/
	// RevokeAutomationWebhookToken are further gated inside each handler
	// itself via domain/authz.Authorize(actor, authz.ActionManageAutomations,
	// ...) (admin/maintainer only); Get/List carry no further RBAC beyond
	// "must be logged in" -- see httpapi/automations.go's own doc comment
	// for why. The webhook-token rotate/revoke pair (review fix: "webhook
	// token has no rotation/revocation/expiry") is mounted here, inside
	// this SAME already-authenticated block, rather than as a separate
	// top-level route -- it manages a sub-resource of an existing
	// automation, exactly like pause/resume above.
	router.Route("/api/automations", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Post("/", httpapi.CreateAutomation(automationStore))
		r.Get("/", httpapi.ListAutomations(automationStore))
		r.Get("/{automationID}", httpapi.GetAutomation(automationStore))
		// invocations ("automations health/runs table", §12.2 item 4): the
		// expandable invocation -> runs read model automations.go's own
		// automation-level lastRunAt/lastRunStatus/artifactSummary fields
		// could not close by themselves -- see httpapi/automationinvocations.go's
		// own doc comment. Same "no extra RBAC beyond logged in" gate as
		// Get/List immediately above.
		r.Get("/{automationID}/invocations", httpapi.ListAutomationInvocations(automationStore, automationInvocationStore, automationRunStore))
		r.Post("/{automationID}/pause", httpapi.PauseAutomation(automationStore))
		r.Post("/{automationID}/resume", httpapi.ResumeAutomation(automationStore))
		r.Post("/{automationID}/webhook-token", httpapi.RotateAutomationWebhookToken(automationStore))
		r.Delete("/{automationID}/webhook-token", httpapi.RevokeAutomationWebhookToken(automationStore))
	})

	// /webhooks/automations/{automationID} (§8.4's own "webhook-
	// facing API surface"): deliberately mounted OUTSIDE auth.Middleware
	// entirely, mirroring /webhooks/linear's own precedent immediately
	// below -- this is authenticated by a per-automation bearer token
	// (internal/adapters/inbound/automationwebhook's own doc comment),
	// never a browser cookie. automationStore/automationInvocationStore
	// are the SAME instances automationEngine already uses -- see that
	// package's own doc comment for why this handler lives in its own
	// adapter package rather than httpapi (an import-cycle constraint,
	// not a style preference).
	router.Post("/webhooks/automations/{automationID}", automationwebhook.NewHandler(automationStore, automationInvocationStore))

	// Linear ingress ("Linear ingress", §8.10) -- see
	// internal/adapters/inbound/linear's own doc.go for the full design.
	// Kept as one self-contained block, separate from the auth/REST
	// sections above, to keep this Step's own diff to this shared file
	// minimal (§8.2/§8.10's own GitHub/Slack ingress land their own
	// analogous blocks here independently, in separate worktrees).
	linearOAuthConfig := linear.NewOAuthConfig(*cfg)
	// http.DefaultClient here (not nil) mirrors slackNotifier's own
	// identical fix above, for the identical reason: linearapi.New used
	// to default a nil client to http.DefaultClient silently, which
	// §30.2 removed as an attractive nuisance, so this call site must now
	// hand over a real transport explicitly. linearClient itself stays
	// reachable directly by the OAuth install callback (a one-time
	// identity READ, no different in kind from GitHub's own excluded
	// OAuth GETs, §30.1) and by the outbox's own LinearNotifier (already
	// suppressed at the outbox layer, ports.NotificationKindLinear is
	// ClassSuppress) -- only the SYNCHRONOUS webhook-handler calls below
	// go through linearDecorated instead (§30.3's family 4).
	linearClient := linearapi.New(http.DefaultClient, linearAPIBaseURL)
	linearDecorated, err := shadowlinear.New(linearClient, shadowLedger, shadowRepoFullName(cfg.LinearDefaultRepoURL), isLiveEgress)
	if err != nil {
		return nil, fmt.Errorf("build shadow linear decorator: %w", err)
	}
	// linearAgentSessionStore is constructed earlier, alongside outboxStore
	// -- see that construction site's own doc comment for why.
	linearInstallationStore := postgres.NewLinearInstallationStore(pool)

	// /auth/linear/install + /auth/linear/callback: the workspace OAuth
	// connection flow (§8.10's own "OAuth" scope) -- mounted behind
	// auth.Middleware (a signed-in Narvi user must initiate/complete a
	// workspace connection) AND, additionally, gated admin-only inside
	// each handler itself via domain/authz.ActionManageIntegrations (see
	// internal/adapters/inbound/linear's own authz.go for the full
	// reasoning -- a confirmed audit finding: this was never actually
	// role-gated, despite an earlier doc comment here deferring it to a
	// later Step that never added it).
	router.Route("/auth/linear", func(r chi.Router) {
		r.Use(auth.Middleware(userSessionStore, userStore))
		r.Get("/install", linear.NewInstallHandler(linearOAuthConfig, cfg.Timeouts, secureCookies))
		r.Get("/callback", linear.NewInstallCallbackHandler(linearOAuthConfig, linearClient, pool, linearInstallationStore, auditLogStore, cfg.TokenEncryptionKey, secureCookies))
	})

	// /webhooks/linear: Linear's own real AgentSessionEvent webhook --
	// deliberately mounted OUTSIDE auth.Middleware entirely, mirroring
	// scm-credentials/snapshot-mint's own precedent above exactly: this
	// is authenticated by Linear's own webhook signature (verified inside
	// the handler itself), never a browser cookie.
	router.Post("/webhooks/linear", linear.NewWebhookHandler(linear.Deps{
		Pool:               pool,
		Sessions:           sessionStore,
		Turns:              turnStore,
		Environments:       environmentStore,
		Registry:           registry,
		Deliveries:         webhookDeliveryStore,
		AgentSessions:      linearAgentSessionStore,
		Installations:      linearInstallationStore,
		LinearClient:       linearDecorated,
		IntentClassifier:   intentClassifierSvc,
		WebhookSecret:      []byte(cfg.LinearWebhookSecret),
		TokenEncryptionKey: cfg.TokenEncryptionKey,
		DefaultRepoName:    cfg.LinearDefaultRepoName,
		DefaultRepoURL:     cfg.LinearDefaultRepoURL,
		Timeouts:           cfg.Timeouts,
		// Plans/Outbox ("plan mode, cross-channel", §8.1/§13.3):
		// handlePrompted's own new plan-verdict keyword check.
		Plans:  planStore,
		Outbox: outboxStore,
		// AuditLog/IdentityLink/Participants ("identities + full
		// RBAC", §13.2/§13.3): Participants is the SAME participantStore
		// instance §8.1's own REST plan approve/reject endpoints already
		// use (constructed once, above), never a second, independently-
		// constructed copy.
		AuditLog:     auditLogStore,
		IdentityLink: appIdentityLinkDeps,
		Participants: participantStore,
		// EpistemicCheckDefault (§20.4): the SAME platform.Config
		// value every other CreateTurnCore-reaching caller above also
		// receives.
		EpistemicCheckDefault: cfg.EpistemicCheckDefault,
		// RolloutMode/RepoSettings (§10 Phase 6, §32): the SAME
		// cfg.RolloutMode/repoSettingsStore every other CreateSessionCore-
		// reaching caller in this file also receives.
		RolloutMode:  cfg.RolloutMode,
		RepoSettings: repoSettingsStore,
	}))

	// Module routes (docs/design/boundaries-design.md, section 3.2): mounted
	// AFTER every public route group, under /api/ext/<Name>/, behind the
	// SAME auth.Middleware every public route above already uses --
	// mountModules' own doc comment. moduleRuntime is the single value
	// every composed module's own Mount/Workers receives; every field on
	// it is either a plain external type or an extension.-prefixed
	// alias, per extension.Runtime's own doc comment -- never an
	// internal type, so this stays constructible from a private module
	// that cannot import internal/... at all.
	requireAuth := auth.Middleware(userSessionStore, userStore)
	moduleRuntime := extension.Runtime{
		Pool:         pool,
		Logger:       slog.Default(),
		Capabilities: capabilities,
		RequireAuth:  requireAuth,
		RequireCapability: func(c extension.Capability) func(http.Handler) http.Handler {
			return httpapi.RequireCapability(capabilities, c)
		},
		PublicBaseURL: cfg.PublicBaseURL,
		Audit: func(ctx context.Context, actorUserID string, action, targetType, targetID string, detail map[string]any) error {
			var actorID pgtype.UUID
			if actorUserID != "" {
				if err := actorID.Scan(actorUserID); err != nil {
					return fmt.Errorf("extension: parse actor user id for audit log: %w", err)
				}
			}
			return auditlog.Record(ctx, auditLogStore, actorID, action, targetType, targetID, detail)
		},
	}
	moduleWorkers := mountModules(router, modules, requireAuth, moduleRuntime)

	// Outbox delivery worker ("outbox delivery", §5.1/§9.3
	// scenario 9): three real ports.Notifier implementations, one per
	// NotificationKind, assembled into a single kind->Notifier routing map
	// -- see internal/app/outboxworker's own doc.go for the full pump
	// design this Builder runs. slackNotifier is the SAME *slackapi.Client
	// instance §8.10's own synchronous in-thread ack/interactivity routes
	// reach (through slackDecorated, above) -- §30.3's "one client per
	// provider", never a second, independently-constructed one.
	// githubNotifier wraps the SAME
	// sourceControl Adapter already constructed above (design decision:
	// BotNotifier is a sibling type over the same Adapter/doPost
	// machinery, not a second, independently-constructed client),
	// authenticated with the NEW, separate cfg.GitHubBotToken rather than
	// any per-session OAuth token (see internal/platform/config.go's own
	// gitHubBotTokenEnvVarName doc comment for why). linearNotifier looks
	// up each workspace's own real Linear API credential fresh, by
	// organization_id, at delivery time (linearInstallationStore +
	// cfg.TokenEncryptionKey) -- never a token cached in this map itself --
	// and, as of an audit-fix batch (finding M16, "completeness"), is
	// registered under BOTH NotificationKindLinear and
	// NotificationKindLinearProgress below (the SAME instance/Deliver
	// implementation for both -- see that type's own doc comment,
	// linearnotifier.go). slackNotifier/planSlackNotifier are constructed
	// earlier, alongside outboxStore -- see that construction site's own
	// doc comment for why.
	githubNotifier := githubapi.NewBotNotifier(liveSourceControl, cfg.GitHubBotToken)
	linearNotifier := outboxworker.NewLinearNotifier(linearClient, linearInstallationStore, cfg.TokenEncryptionKey)
	// githubVerdictNotifier ("server-side verdict", §8.2) wraps
	// the SAME sourceControl *githubapi.Adapter instance every other
	// GitHub-flavored notifier/caller above already uses, authenticated
	// with the SAME cfg.GitHubBotToken githubNotifier itself uses --
	// posting a verdict is a bot-attributed action exactly like posting
	// the (now-blocked-for-review-sessions) generic outcome comment used
	// to be, never a per-commenter credential.
	githubVerdictNotifier := githubapi.NewVerdictNotifier(liveSourceControl, cfg.GitHubBotToken)
	// sentinelAutoFixNotifier ("sentinels + suggestions", §17.2)
	// spawns the child session -- reviewFindingStore/sentinelFixStore are
	// the SAME instances every other caller above already uses.
	sentinelAutoFixNotifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessionStore, turnStore, environmentStore, auditLogStore, registry, sentinelFixStore, reviewFindingStore, sourceControl, cfg.GitHubBotToken, cfg.Timeouts, cfg.EpistemicCheckDefault, cfg.RolloutMode, repoSettingsStore, isLiveEgress, shadowLedger)
	// handoffNotifier ("handoff-readiness sentinel", §14.4) posts
	// the handoff-readiness comment and applies the "handoff" label on a
	// scoped session's PR -- the SAME sourceControl/cfg.GitHubBotToken
	// every other GitHub-flavored notifier above already uses.
	handoffNotifier := githubapi.NewHandoffNotifier(liveSourceControl, cfg.GitHubBotToken)
	// releaseManifestNotifier ("release PR review", §15.2) posts
	// the release manifest check's own summary comment -- the SAME
	// sourceControl/cfg.GitHubBotToken every other GitHub-flavored
	// notifier above already uses.
	releaseManifestNotifier := githubapi.NewReleaseManifestNotifier(liveSourceControl, cfg.GitHubBotToken)
	// descriptionAutofixNotifier ("review digest: description
	// adequacy + graduated remediation", §26.2) re-verifies Narvi-
	// authorship and this repo's own descriptionAutofix flag, fresh, at
	// delivery time, then rewrites a Narvi-authored PR's own body -- the
	// SAME repoSettingsStore/artifactStore/sourceControl/cfg.GitHubBotToken
	// every other caller above already uses.
	descriptionAutofixNotifier := outboxworker.NewDescriptionAutofixNotifier(repoSettingsStore, artifactStore, sourceControl, cfg.GitHubBotToken, cfg.Timeouts)

	// outboxStore is constructed earlier, alongside linearAgentSessionStore
	// -- see that construction site's own doc comment for why.
	outboxNotifiers := map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack:             slackNotifier,
		ports.NotificationKindGitHub:            githubNotifier,
		ports.NotificationKindLinear:            linearNotifier,
		ports.NotificationKindLinearProgress:    linearNotifier,
		ports.NotificationKindSlackPlanApproval: planSlackNotifier,
		ports.NotificationKindSlackPlanDecided:  planSlackNotifier,
		ports.NotificationKindGitHubVerdict:     githubVerdictNotifier,
		ports.NotificationKindSentinelAutoFix:   sentinelAutoFixNotifier,
		ports.NotificationKindHandoffSentinel:   handoffNotifier,
		ports.NotificationKindReleaseManifest:   releaseManifestNotifier,
		// §26.2 ("review digest: description adequacy + graduated
		// remediation", §26.2): re-verifies Narvi-authorship and this
		// repo's own descriptionAutofix flag, fresh, at delivery time,
		// then rewrites a Narvi-authored PR's own body.
		ports.NotificationKindGitHubDescriptionAutofix: descriptionAutofixNotifier,
		// §25.9 ("workflow HITL gate + circuit breaker", §25.9): a
		// workflow step awaiting decision, or a run escalating to
		// needs_review, notifies a human via whichever of these three the
		// originating session supports (internal/app/workflowengine's own
		// enqueueWorkflowNotice, notify.go). Slack/Linear reuse the SAME
		// planSlackNotifier/linearNotifier instances already registered
		// above, each now handling a THIRD kind (see those types' own
		// updated Deliver switch); GitHub reuses the SAME githubNotifier
		// instance too -- BotNotifier.Deliver never inspects notification.
		// Kind at all, so registering it under a second key needs no new
		// githubapi code whatsoever.
		ports.NotificationKindSlackWorkflowDecision:  planSlackNotifier,
		ports.NotificationKindLinearWorkflowDecision: linearNotifier,
		ports.NotificationKindGitHubWorkflowDecision: githubNotifier,
		// §21 ("review verdict persistence, analytics, digest &
		// automated approval", §21.3): the deterministic daily digest's
		// own two outbox kinds. digestSlackNotifier reuses the SAME
		// *slackapi.Client every other Slack notifier above already
		// uses; digestLinearNotifier takes no dependencies at all -- see
		// that type's own doc comment (outboxworker/digestlinearnotifier.go)
		// for why it always returns a clear, typed error rather than
		// actually delivering anything (no organization-level Linear post
		// capability exists in this codebase yet).
		ports.NotificationKindSlackDigest:  outboxworker.NewDigestSlackNotifier(slackNotifier),
		ports.NotificationKindLinearDigest: outboxworker.NewDigestLinearNotifier(),
	}

	// rwxPreviewNotifier/githubPreviewLinkNotifier ("RWX provider
	// + previews", §4.1.1/§4.1.2) are registered ONLY when cfg.RWXAccessToken
	// is configured -- see that env var's own doc comment (platform/
	// config.go) for why this platform-wide credential is optional, unlike
	// Modal's/GitHub's own mandatory secrets: RWX previews are an
	// off-by-default, per-repo opt-in feature layered on top of it, and a
	// deployment that never turns previews on for any repo should not be
	// forced to configure a real RWX account just to boot. When absent, any
	// row enqueued for either of these two kinds (which requires a repo
	// admin to have separately opted in -- an operator misconfiguration,
	// since the two are meant to be configured together) dead-letters with
	// a clear, logged "no notifier registered for kind" error rather than
	// silently vanishing. githubPreviewLinkNotifier reuses the SAME
	// sourceControl *githubapi.Adapter instance and cfg.GitHubBotToken every
	// other GitHub-flavored notifier above already uses -- a preview link
	// is a system-generated fact about a commit, never attributed to any
	// individual PR author or reviewer.
	if cfg.RWXAccessToken != "" {
		// http.DefaultClient, not nil: §30.2 removed the nil default from
		// this constructor, so nil here builds a client whose transport
		// refuses every request -- RWX preview dispatch would have been
		// dead the moment an operator configured it, silently.
		rwxDispatchClient := rwx.NewDispatchClient(http.DefaultClient, "", cfg.RWXAccessToken)
		outboxNotifiers[ports.NotificationKindRWXPreviewDispatch] = rwx.NewPreviewNotifier(rwxDispatchClient)
		outboxNotifiers[ports.NotificationKindGitHubPreviewLink] = githubapi.NewPreviewLinkNotifier(liveSourceControl, cfg.GitHubBotToken)
	}

	// blob_delete (§28.4) is registered ONLY when blobStore is
	// configured -- mirrors the RWX block immediately above exactly. When
	// absent, a blob_delete row (which can only ever be enqueued by
	// confirmUploadCore/uploadsweep, both of which are themselves
	// unreachable/inert without cfg.ObjectStorage) dead-letters with the
	// same clear, logged "no notifier registered for kind" error every
	// other unconfigured kind gets -- not a real-world case, since nothing
	// produces this kind's rows without the SAME config this notifier
	// itself requires, but handled identically rather than specially.
	if objStore != nil {
		outboxNotifiers[ports.NotificationKindBlobDelete] = objstore.NewBlobDeleteNotifier(objStore)
	}

	outboxBuilder, err := outboxworker.NewBuilder(outboxStore, pool, outboxNotifiers, cfg.Timeouts)
	if err != nil {
		return nil, fmt.Errorf("construct outbox delivery worker: %w", err)
	}

	// releaseManifestWorker (blocking-finding fix #1, "release PR
	// review", §15.2) is the SEPARATE background loop that claims
	// release_manifest_pending rows (releaseManifestPendingStore,
	// constructed earlier alongside outboxStore) and runs the actual
	// manifest check (releasereview.Run) for each -- entirely decoupled
	// from any webhook request's own context/lifetime, started below via
	// this SAME errgroup as outboxBuilder/every other background loop.
	// deps mirrors what the pre-fix inline call used to pass directly:
	// the SAME sourceControl instance (it satisfies releasereview.
	// MergedPRLister directly) and the SAME outboxStore instance every
	// other enqueue site in this file already uses -- Run itself still
	// enqueues the check's own already-rendered comment there, unchanged
	// by this fix. cfg.GitHubBotToken is the SAME bot credential the
	// pre-fix inline call authenticated ListMergedBetween with -- never
	// persisted onto a release_manifest_pending row itself (see
	// releasereview.Enqueue's own doc comment).
	releaseManifestWorker := releasereview.NewWorker(releaseManifestPendingStore, releasereview.Deps{
		SourceControl:         sourceControl,
		Outbox:                outboxStore,
		ReleaseManifestChecks: releaseManifestCheckStore,
		Timeouts:              cfg.Timeouts,
	}, cfg.GitHubBotToken, cfg.Timeouts)

	return &App{
		Router: router,

		cfg:  cfg,
		pool: pool,

		registry:                registry,
		recon:                   recon,
		builder:                 builder,
		outboxBuilder:           outboxBuilder,
		releaseManifestWorker:   releaseManifestWorker,
		automationEngine:        automationEngine,
		automergeWorker:         automergeWorker,
		digestPump:              digestPump,
		uploadSweeper:           uploadSweeper,
		providerCredentialStore: providerCredentialStore,
		chatGPTDeviceFlow:       chatGPTDeviceFlow,

		capabilities:    capabilities,
		knowledgeRanker: knowledgeRanker,
		moduleWorkers:   moduleWorkers,
	}, nil
}

// Run listens on addr and runs every background loop through ONE errgroup
// with the same context.Canceled carve-out serve() has always had for
// normal shutdown. Blocks until ctx is canceled (SIGINT/SIGTERM in
// production; a test's own cancel elsewhere) and every loop has returned.
func (a *App) Run(ctx context.Context, addr string) error {
	cfg := a.cfg
	pool := a.pool
	router := a.Router
	registry := a.registry
	recon := a.recon
	builder := a.builder
	outboxBuilder := a.outboxBuilder
	releaseManifestWorker := a.releaseManifestWorker
	automationEngine := a.automationEngine
	automergeWorker := a.automergeWorker
	digestPump := a.digestPump
	uploadSweeper := a.uploadSweeper
	providerCredentialStore := a.providerCredentialStore
	chatGPTDeviceFlow := a.chatGPTDeviceFlow
	moduleWorkers := a.moduleWorkers

	srv := &http.Server{
		Addr:    addr,
		Handler: router,
	}

	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		slog.Info("narvi control-plane: listening", "addr", addr, "stage", cfg.Stage)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})

	group.Go(func() error {
		if err := registry.RunTimerPump(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
			// RunTimerPump returns ctx.Err() (context.Canceled) on normal
			// shutdown -- that must NOT be treated as a fatal error the way
			// http.ErrServerClosed is already specially unwrapped for the
			// listener goroutine above; only a genuinely different error is
			// surfaced here.
			return fmt.Errorf("timer pump: %w", err)
		}
		return nil
	})

	// Audit-remediation (config/platform-hardening batch): purges expired
	// ws_tokens/user_sessions rows (neither is ever deleted otherwise --
	// see internal/adapters/outbound/postgres/expiredcleanup.go's own doc
	// comment). Started/shut down through this SAME errgroup as every
	// other background loop above -- no naked goroutine (§11).
	group.Go(func() error {
		if err := postgres.RunExpiredTokenCleanup(groupCtx, pool, cfg.Timeouts.ExpiredCredentialCleanupInterval); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("expired credential cleanup: %w", err)
		}
		return nil
	})

	// §5.3 ("reconciler + GC", §5.3): started/shut down through this
	// SAME errgroup as every other background loop above -- no naked
	// goroutine (§11) -- with the identical context.Canceled carve-out
	// RunTimerPump/RunExpiredTokenCleanup already establish for normal
	// shutdown.
	group.Go(func() error {
		if err := recon.Run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("reconciler: %w", err)
		}
		return nil
	})

	// §8.5 ("image builds"): started/shut down through this SAME
	// errgroup as every other background loop above -- no naked goroutine
	// (§11) -- with the identical context.Canceled carve-out RunTimerPump/
	// RunExpiredTokenCleanup/recon.Run each already establish for normal
	// shutdown.
	group.Go(func() error {
		if err := builder.Run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("image builder: %w", err)
		}
		return nil
	})

	// §5.1 ("outbox delivery", §5.1): started/shut down through this
	// SAME errgroup as every other background loop above -- no naked
	// goroutine (§11) -- with the identical context.Canceled carve-out
	// RunTimerPump/RunExpiredTokenCleanup/recon.Run/builder.Run each
	// already establish for normal shutdown.
	group.Go(func() error {
		if err := outboxBuilder.Run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("outbox delivery worker: %w", err)
		}
		return nil
	})

	// Blocking-finding fix #1 ("release PR review", §15.2): started/shut
	// down through this SAME errgroup as every other background loop
	// above -- no naked goroutine (§11) -- with the identical
	// context.Canceled carve-out every other background loop already
	// establishes for normal shutdown. Runs against groupCtx, the SAME
	// process-lifetime context every other background loop uses -- NEVER
	// any individual webhook request's own context, which is the entire
	// point of this fix (see releaseManifestWorker's own construction
	// site doc comment, and migrations/000050_release_manifest_pending.
	// up.sql's own doc comment, for the full "why").
	group.Go(func() error {
		if err := releaseManifestWorker.Run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("release manifest check worker: %w", err)
		}
		return nil
	})

	// §3.5 ("automations: engine", §3.5): started/shut down through
	// this SAME errgroup as every other background loop above -- no naked
	// goroutine (§11) -- with the identical context.Canceled carve-out
	// every other background loop already establishes for normal
	// shutdown. Engine.Run itself fans out its own three ticker loops
	// (fan-out, reconcile, sweep) via a further, internal errgroup -- see
	// internal/app/automation's own doc.go.
	group.Go(func() error {
		if err := automationEngine.Run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("automation engine: %w", err)
		}
		return nil
	})

	// (§21.2 stage 2, "auto-merge"): started/shut down through
	// this SAME errgroup as every other background loop above -- no
	// naked goroutine (§11) -- with the identical context.Canceled
	// carve-out every other background loop already establishes for
	// normal shutdown.
	group.Go(func() error {
		if err := automergeWorker.Run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("auto-merge worker: %w", err)
		}
		return nil
	})

	// (§21.3, "deterministic daily digest"): started/shut down
	// through this SAME errgroup as every other background loop above --
	// no naked goroutine (§11) -- with the identical context.Canceled
	// carve-out every other background loop already establishes for
	// normal shutdown.
	group.Go(func() error {
		if err := digestPump.Run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("digest pump: %w", err)
		}
		return nil
	})

	// §8.6 ("uploads, blob storage & the in-sandbox download_file
	// tool", §28.4): uploadSweeper is nil when cfg.ObjectStorage is absent
	// (feature off) -- started/shut down through this SAME errgroup as
	// every other background loop above -- no naked goroutine (§11) --
	// with the identical context.Canceled carve-out every other
	// background loop already establishes for normal shutdown.
	if uploadSweeper != nil {
		group.Go(func() error {
			if err := uploadSweeper.Run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("upload abandonment sweeper: %w", err)
			}
			return nil
		})
	}

	// §8.8 ("models: Codex via ChatGPT-account OAuth", §29.5):
	// chatGPTRefreshPump is the single control-plane refresher for every
	// linked ChatGPT account -- unconditional (unlike uploadSweeper above,
	// this needs no optional external dependency to be configured; it is
	// a plain, always-on ticker that simply finds zero rows to do until a
	// user actually links an account). Started/shut down through this
	// SAME errgroup as every other background loop above -- no naked
	// goroutine (§11) -- with the identical context.Canceled carve-out.
	chatGPTRefreshPump := chatgptrefresh.NewPump(providerCredentialStore, pool, chatGPTDeviceFlow, cfg.TokenEncryptionKey, cfg.Timeouts)
	group.Go(func() error {
		if err := chatGPTRefreshPump.Run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("chatgpt oauth refresh pump: %w", err)
		}
		return nil
	})

	// Module workers (docs/design/boundaries-design.md, section 3.2): every
	// worker every composed module contributed (extension.Module.Workers),
	// started/shut down through this SAME errgroup as every internal
	// background loop above -- no naked goroutine (§11) -- with the
	// identical context.Canceled carve-out every internal loop already
	// establishes for normal shutdown. Empty for the public binary, which
	// composes no module.
	for _, worker := range moduleWorkers {
		group.Go(func() error {
			if err := worker.Run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("module worker: %w", err)
			}
			return nil
		})
	}

	group.Go(func() error {
		<-groupCtx.Done()
		slog.Info("narvi control-plane: shutting down", "grace_period", cfg.Timeouts.ShutdownGracePeriod.String())

		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Timeouts.ShutdownGracePeriod)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http server shutdown: %w", err)
		}

		// Registry.Shutdown cancels every live actor's run loop and waits
		// for all of them, plus the timer-pump goroutine above, to finish.
		// Its own errgroup.Wait() will very likely surface context.Canceled
		// from every actor whose run loop was still alive at shutdown time
		// -- expected/benign, not a real failure, so it gets the exact same
		// context.Canceled carve-out as the timer pump above; anything else
		// is a genuine shutdown failure.
		if err := registry.Shutdown(); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("session actor registry shutdown: %w", err)
		}
		return nil
	})

	return group.Wait()
}

// shutdownControlPlaneOTel bounds one call to shutdown (platform.SetupOTel's
// own returned func) by timeout, against a fresh background context --
// mirrors cmd/sandbox-agent/main.go's own shutdownSandboxAgentOTel exactly,
// now that §33 gives this binary's own OTel shutdown the SAME real failure
// mode sandbox-agent's always had: an OTLP exporter's flush is a network
// call to an operator's collector, and a down/unreachable collector must
// not be allowed to hang serve()'s own graceful exit indefinitely.
//
// Previously this call (serve()'s own deferred shutdownOTel) was left
// deliberately unbounded -- see shutdownSandboxAgentOTel's own doc comment
// for the reasoning at the time: a bare stdout write essentially never
// hangs, and this is a long-running daemon that gets another periodic
// export anyway even if one flush were somehow missed. That reasoning held
// only as long as stdout was the only exporter this binary could ever have.
// §33 ends that by giving serve() a real OTLP endpoint option
// (cfg.OTLPEndpoint) -- platform.Timeouts.OTelShutdownTimeout now bounds
// BOTH binaries' shutdown calls identically; see that field's own doc
// comment.
//
// Factored out of serve() specifically so it is unit-testable in isolation
// (see main_test.go's own TestShutdownControlPlaneOTel_BoundsAHungShutdown),
// exactly like shutdownSandboxAgentOTel's own precedent.
func shutdownControlPlaneOTel(shutdown func(context.Context) error, timeout time.Duration) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return shutdown(shutdownCtx)
}
