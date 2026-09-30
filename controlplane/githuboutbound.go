package controlplane

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/automerge"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/releasereview"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/shadowscm"
	"github.com/narvidev/narvi/internal/domain/integrations"
	"github.com/narvidev/narvi/internal/platform"
)

// githubOutboundKinds is every outbox kind a GitHub outbound notifier
// delivers -- the kinds a deployment registers exactly when
// cfg.GitHubOutbound is non-nil (§12.5's outbound axis), whatever
// NARVI_INGRESS_ENABLED says. github_preview_link is among them only when
// RWX previews are configured (cfg.RWXAccessToken). A row of one of these
// kinds on a deployment with GitHub outbound off dead-letters with "no
// notifier registered for kind" (docs/runbooks/outbox-delivery.md).
//
// Listed for the composition-root tests, which prove the registered set
// with outbound on minus the set with it off is exactly this list.
var githubOutboundKinds = []ports.NotificationKind{
	// Publication (§44.4: what a publish credential posts).
	ports.NotificationKindGitHub,
	ports.NotificationKindGitHubWorkflowDecision,
	ports.NotificationKindGitHubVerdict,
	ports.NotificationKindHandoffSentinel,
	ports.NotificationKindReleaseManifest,
	ports.NotificationKindGitHubReviewCheck,
	// Bot-only writes.
	ports.NotificationKindSentinelAutoFix,
	ports.NotificationKindGitHubDescriptionAutofix,
	ports.NotificationKindGitHubPreviewLink,
}

// githubOutbound is §12.5's GitHub outbound axis, wired: every notifier
// and background worker that acts on GitHub as the bot, built in ONE place
// from cfg.GitHubOutbound. Build merges notifiers into the outbox's
// kind->Notifier map and hands both workers to Run, which starts each only
// when it is non-nil. A nil *githubOutbound -- GitHub outbound off -- has
// no notifiers and no workers, and its methods say so.
//
// Nothing outside this file may construct a GitHub outbound consumer's
// required class. The per-surface ingress gates that used to decide these
// registrations are gone: registering a GitHub notifier is a question of
// whether the deployment calls GitHub as its bot, never of whether it
// mounts the webhook.
type githubOutbound struct {
	notifiers             map[ports.NotificationKind]ports.Notifier
	automergeWorker       *automerge.Worker
	releaseManifestWorker *releasereview.Worker
}

// githubOutboundDeps is what buildGitHubOutbound needs from Build: the
// SAME instances Build already constructed, never second copies.
type githubOutboundDeps struct {
	pool *pgxpool.Pool

	// liveSourceControl is the concrete, transport-gated adapter the
	// githubapi notifiers wrap; sourceControl is the §30.2 shadow
	// decorator over it, for the consumers typed on the port.
	liveSourceControl *githubapi.Adapter
	sourceControl     *shadowscm.Decorator

	registry *sessionactor.Registry

	sessions             *postgres.SessionStore
	turns                *postgres.TurnStore
	environments         *postgres.EnvironmentStore
	auditLog             *postgres.AuditLogStore
	sentinelFixes        *postgres.SentinelFixStore
	reviewFindings       *postgres.ReviewFindingStore
	repoSettings         *postgres.RepoSettingsStore
	prSessions           *postgres.GitHubPRSessionStore
	artifacts            *postgres.ArtifactStore
	outbox               *postgres.OutboxStore
	releaseManifestQueue *postgres.ReleaseManifestPendingStore
	releaseChecks        *postgres.ReleaseManifestCheckStore
	promptTemplates      *postgres.PromptTemplateStore

	isLiveEgress func(ctx context.Context, repoFullName string) bool
	shadowLedger *postgres.ShadowSCMWriteStore

	decisionInbox decisioninbox.Deps
}

// buildGitHubOutbound builds the GitHub outbound axis, or returns nil when
// cfg.GitHubOutbound is nil. Every constructor it calls refuses a nil
// config, so an error here is a wiring bug, returned as a Build error.
func buildGitHubOutbound(cfg *platform.Config, deps githubOutboundDeps) (*githubOutbound, error) {
	outbound := cfg.GitHubOutbound
	if outbound == nil {
		return nil, nil
	}
	o := &githubOutbound{notifiers: map[ports.NotificationKind]ports.Notifier{}}

	// Publication: what the bot posts ABOUT a pull request -- §44.4's
	// publication class, the one a later purpose-scoped publish
	// credential (§44.5) takes over.
	//
	// githubNotifier wraps the SAME transport-gated adapter as every other
	// GitHub notifier (BotNotifier is a sibling type over its doPost
	// machinery, not a second client). It serves two kinds: the generic
	// turn-outcome comment and §25.9's workflow decision notice --
	// Deliver never inspects the kind.
	githubNotifier, err := githubapi.NewBotNotifier(deps.liveSourceControl, outbound)
	if err != nil {
		return nil, fmt.Errorf("construct github comment notifier: %w", err)
	}
	o.notifiers[ports.NotificationKindGitHub] = githubNotifier
	o.notifiers[ports.NotificationKindGitHubWorkflowDecision] = githubNotifier
	// The formal review and the review:*-risk labels ("server-side
	// verdict", §8.2).
	verdictNotifier, err := githubapi.NewVerdictNotifier(deps.liveSourceControl, outbound)
	if err != nil {
		return nil, fmt.Errorf("construct github verdict notifier: %w", err)
	}
	o.notifiers[ports.NotificationKindGitHubVerdict] = verdictNotifier
	// The handoff-readiness comment and "handoff" label (§14.4).
	handoffNotifier, err := githubapi.NewHandoffNotifier(deps.liveSourceControl, outbound)
	if err != nil {
		return nil, fmt.Errorf("construct github handoff notifier: %w", err)
	}
	o.notifiers[ports.NotificationKindHandoffSentinel] = handoffNotifier
	// The release manifest check's summary comment (§15.2).
	releaseManifestNotifier, err := githubapi.NewReleaseManifestNotifier(deps.liveSourceControl, outbound)
	if err != nil {
		return nil, fmt.Errorf("construct github release manifest notifier: %w", err)
	}
	o.notifiers[ports.NotificationKindReleaseManifest] = releaseManifestNotifier
	// The narvi/review check run (§8.2/§21.1/§21.1b), over its own
	// claim-table store. It self-learns its writer App id from its own
	// CreateCheckRun responses (reviewcheck.go): §30.4's GitHub App is a
	// different, read-only credential, never the one these writes carry.
	reviewCheckNotifier, err := outboxworker.NewReviewCheckNotifier(deps.pool, postgres.NewReviewCheckRunStore(deps.pool), deps.liveSourceControl, outbound)
	if err != nil {
		return nil, fmt.Errorf("construct github review check notifier: %w", err)
	}
	o.notifiers[ports.NotificationKindGitHubReviewCheck] = reviewCheckNotifier

	// Bot-only writes: what the bot does to a repository or a pull request
	// beyond publishing about it -- these stay on the bot credential even
	// once a publish credential exists (§44.4).
	//
	// The sentinel auto-fix lane (§17.2): creates the fix branch as the
	// bot and spawns the fix session, whose actor opens the fix PR with
	// the same credential (sessionactor.RegistryOptions.GitHubOutbound).
	sentinelAutoFixNotifier, err := outboxworker.NewSentinelAutoFixNotifier(deps.pool, deps.sessions, deps.turns, deps.environments, deps.auditLog, deps.registry, deps.sentinelFixes, deps.reviewFindings, deps.sourceControl, outbound, cfg.Timeouts, cfg.EpistemicCheckDefault, cfg.RolloutMode, deps.repoSettings, deps.prSessions, deps.isLiveEgress, deps.shadowLedger)
	if err != nil {
		return nil, fmt.Errorf("construct sentinel auto-fix notifier: %w", err)
	}
	o.notifiers[ports.NotificationKindSentinelAutoFix] = sentinelAutoFixNotifier
	// The PR description rewrite (§26.2), re-verified at delivery time.
	descriptionAutofixNotifier, err := outboxworker.NewDescriptionAutofixNotifier(deps.repoSettings, deps.artifacts, deps.sourceControl, outbound, cfg.Timeouts)
	if err != nil {
		return nil, fmt.Errorf("construct description autofix notifier: %w", err)
	}
	o.notifiers[ports.NotificationKindGitHubDescriptionAutofix] = descriptionAutofixNotifier
	// The narvi/preview commit status (§4.1.2), only with RWX previews
	// configured -- its dispatch half (rwx_preview_dispatch) needs no
	// GitHub credential and is registered on cfg.RWXAccessToken alone, in
	// Build. platform.Load refuses RWX previews without GitHub outbound
	// (RWXPreviewsRequireGitHubOutboundError); this registration does not
	// rely on that.
	if cfg.RWXAccessToken != "" {
		previewLinkNotifier, err := githubapi.NewPreviewLinkNotifier(deps.liveSourceControl, outbound)
		if err != nil {
			return nil, fmt.Errorf("construct github preview link notifier: %w", err)
		}
		o.notifiers[ports.NotificationKindGitHubPreviewLink] = previewLinkNotifier
	}
	// Auto-merge (§21.2 stage 2): reads and merges pull requests as the
	// bot, re-validating each candidate through the decision inbox's own
	// stores (decisioninbox.RevalidateForAutoMerge).
	o.automergeWorker, err = automerge.New(automerge.Deps{
		DecisionInbox: deps.decisionInbox,
		SourceControl: deps.sourceControl,
		AuditLog:      deps.auditLog,
		Outbound:      outbound,
		Timeouts:      cfg.Timeouts,
	})
	if err != nil {
		return nil, fmt.Errorf("construct automerge worker: %w", err)
	}
	// The release-manifest check worker (§15.2/§15.3): claims
	// release_manifest_pending rows -- written by the GitHub webhook
	// handler, so idle without ingress -- and reads merged pull requests
	// as the bot, entirely decoupled from any webhook request's lifetime.
	// The composition review dispatches through releaseCompositionDispatcher
	// (registry.GetOrSpawn + EnsureDispatched, the same fire-and-forget
	// sequencing every other turn-creation path uses).
	o.releaseManifestWorker, err = releasereview.NewWorker(deps.releaseManifestQueue, releasereview.Deps{
		SourceControl:          deps.sourceControl,
		Outbox:                 deps.outbox,
		ReleaseManifestChecks:  deps.releaseChecks,
		CompositionTemplates:   deps.promptTemplates,
		CompositionDiffFetcher: deps.sourceControl,
		CompositionTurns:       deps.turns,
		CompositionDispatch:    releaseCompositionDispatcher{registry: deps.registry},
		CompositionAnchor:      deps.releaseChecks,
		Timeouts:               cfg.Timeouts,
	}, outbound, cfg.Timeouts)
	if err != nil {
		return nil, fmt.Errorf("construct release manifest check worker: %w", err)
	}

	return o, nil
}

// registerNotifiers adds every GitHub outbound notifier to into; a nil
// receiver (GitHub outbound off) adds none.
func (o *githubOutbound) registerNotifiers(into map[ports.NotificationKind]ports.Notifier) {
	if o == nil {
		return
	}
	for kind, n := range o.notifiers {
		into[kind] = n
	}
}

// workers returns the axis's two background workers, both nil when GitHub
// outbound is off.
func (o *githubOutbound) workers() (*automerge.Worker, *releasereview.Worker) {
	if o == nil {
		return nil, nil
	}
	return o.automergeWorker, o.releaseManifestWorker
}

// logGitHubAxes says out loud, once at boot, which GitHub directions this
// deployment runs: whether it mounts the webhook (ingress) and whether it
// calls GitHub as its bot (outbound), and with what credential -- the line
// docs/PRODUCTION_CHECKLIST.md asks an operator to check.
func logGitHubAxes(cfg *platform.Config) {
	credential := "none"
	if cfg.GitHubOutbound != nil {
		credential = "bot token"
	}
	slog.Info("narvi control-plane: GitHub axes",
		"ingress", cfg.IngressEnabled[integrations.ProviderGitHub],
		"outbound", cfg.GitHubOutbound != nil,
		"outbound_credential", credential)
}
