// This file (sessionconfig.go) implements real SessionConfig assembly
// (§9.3, "e2e happy path", design decision 6) -- the FIRST real caller
// anywhere in the repo that constructs a sessionconfig.SessionConfig
// struct literal (confirmed by a repo-wide grep before this Step started).

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/credentialscope"
	appreviewtriage "github.com/narvidev/narvi/internal/app/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/environment"
	"github.com/narvidev/narvi/internal/domain/provenance"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/platform"
)

// publicWsBaseURL derives a ws(s):// base URL from httpBaseURL (platform.
// Config.PublicBaseURL, an http(s):// URL) by swapping the scheme
// (http->ws, https->wss) and keeping everything else -- the same
// derive-rather-than-add-a-field choice internal/sandboxagent/credentials.
// NewCPClient already makes in the opposite direction (ws/wss ->
// http/https). Documented alternative NOT taken: a second, separately
// configured platform.Config field (e.g. PublicWsBaseURL) -- deriving
// avoids a second config value that could silently drift from
// PublicBaseURL's own host/port.
func publicWsBaseURL(httpBaseURL string) (string, error) {
	parsed, err := url.Parse(httpBaseURL)
	if err != nil {
		return "", fmt.Errorf("sessionactor: parse public base url %q: %w", httpBaseURL, err)
	}

	var wsScheme string
	switch parsed.Scheme {
	case "https":
		wsScheme = "wss"
	case "http":
		wsScheme = "ws"
	default:
		return "", fmt.Errorf("sessionactor: public base url %q has unrecognized scheme %q, want http or https", httpBaseURL, parsed.Scheme)
	}

	return wsScheme + "://" + parsed.Host, nil
}

// reposFromJSON unmarshals sessions.repos' raw JSONB bytes (design
// decision 1, migrations/000018_session_repos.up.sql) into the
// SessionConfig wire shape. Both CreateSessionRequestReposElem (the wire
// shape httpapi.CreateSession originally persisted) and
// SessionConfigReposElem share the identical JSON shape
// ({branch, name, url}), so a direct unmarshal into the SessionConfig
// type is correct -- no intermediate conversion type needed.
func reposFromJSON(raw []byte) ([]sessionconfig.SessionConfigReposElem, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var repos []sessionconfig.SessionConfigReposElem
	if err := json.Unmarshal(raw, &repos); err != nil {
		return nil, fmt.Errorf("sessionactor: unmarshal session repos: %w", err)
	}
	return repos, nil
}

// environmentSubstrate resolves everything assembleSessionConfig needs
// from the session's own Environment row in exactly ONE Postgres read:
// PathScope (§14.1 -- §14.1's own clone-step enforcement needs
// the sandbox process to actually receive these glob patterns, since
// sandbox-agent is a separate process from the control plane and only
// knows what it's told via NARVI_SESSION_CONFIG), DockerRequired (§27.5),
// and EgressPolicy (§27.6) -- this folds its own
// two new derivations into what §3.4's own environmentPathScope
// (now this function) already fetched, rather than adding two more
// independent queries against the SAME row inside the SAME transact.
//
// Returns every zero value (nil pathScope, false dockerRequired, the
// EgressPolicy zero value) when environmentID is invalid (pgtype.UUID's
// own zero value) -- the overwhelming common, unscoped case -- WITHOUT
// ever querying the environments table at all, mirroring
// contractdrift.go's own checkContractDrift precedent ("no
// environment_id, ... a plain, logged, no-op") for the identical "no
// Environment attached" gate. Uses tx (the SAME already-open transact
// planFreshSpawn/planRestore run this from), not a's own pool, since this
// runs inside their transact, not after it.
func (a *Actor) environmentSubstrate(ctx context.Context, tx pgx.Tx, environmentID pgtype.UUID) (pathScope []string, dockerRequired bool, egressPolicy environment.EgressPolicy, err error) {
	if !environmentID.Valid {
		return nil, false, environment.EgressPolicy{}, nil
	}

	env, err := a.stores.environment.WithTx(tx).Get(ctx, environmentID)
	if err != nil {
		return nil, false, environment.EgressPolicy{}, fmt.Errorf("sessionactor: get environment: %w", err)
	}

	if len(env.PathScope) > 0 {
		// An Environment can be created for its mock_config/docker/
		// egressPolicy alone, with no path_scope attached (§14.1: "an
		// optional path_scope ... and an optional mock_config" -- several
		// independent optional attributes, same reasoning as
		// contractdrift.go's own MockConfigured check) -- nothing to
		// unmarshal, nothing to report, when it is absent.
		if err := json.Unmarshal(env.PathScope, &pathScope); err != nil {
			return nil, false, environment.EgressPolicy{}, fmt.Errorf("sessionactor: unmarshal environment path_scope: %w", err)
		}
	}

	dockerRequired = env.DockerRequired

	if env.EgressPolicyMode != nil {
		var allowlist []string
		if len(env.EgressPolicyAllowlist) > 0 {
			if err := json.Unmarshal(env.EgressPolicyAllowlist, &allowlist); err != nil {
				return nil, false, environment.EgressPolicy{}, fmt.Errorf("sessionactor: unmarshal environment egress_policy_allowlist: %w", err)
			}
		}
		egressPolicy = environment.EgressPolicy{Mode: environment.EgressMode(*env.EgressPolicyMode), Allowlist: allowlist}
	}

	return pathScope, dockerRequired, egressPolicy, nil
}

// allowlistFloorHosts computes §27.6's own non-negotiable allowlist
// floor for THIS session: the control plane's own WS/API host (derived
// from a.publicBaseURL, the SAME value publicWsBaseURL above already
// derives the sandbox WS URL from) plus this session's own ACTUAL git
// hosts (derived from sessionRow.Repos, never a static list -- a
// customer's own repos may live on a self-hosted GitLab/Bitbucket host
// this control plane has no other opinion about). Best-effort: a repo
// clone URL (or the public base URL itself) that fails to parse is
// simply omitted from the floor rather than blocking the whole spawn
// (environment.HostFromURL's own doc comment) -- an honest degrade, not
// a silent one: logged at Warn so a malformed value is still visible.
// Deduplicated (case-insensitively, matching AppendAllowlistFloor's own
// comparison) before being handed to that function.
func (a *Actor) allowlistFloorHosts(ctx context.Context, repos []sessionconfig.SessionConfigReposElem) []string {
	logger := platform.Logger(ctx)

	seen := make(map[string]bool, len(repos)+1)
	var floor []string

	addHost := func(source, rawURL string) {
		host, ok := environment.HostFromURL(rawURL)
		if !ok {
			logger.Warn("sessionactor: allowlist floor: could not derive a host; omitting from the server-appended floor",
				"source", source, "value", rawURL)
			return
		}
		key := strings.ToLower(host)
		if seen[key] {
			return
		}
		seen[key] = true
		floor = append(floor, host)
	}

	addHost("control_plane", a.publicBaseURL)
	for _, repo := range repos {
		addHost("repo:"+repo.Name, repo.Url)
	}
	return floor
}

// reviewCounterReviewerModel resolves §26.4's own §26.4 opposing-model-
// family override for THIS session, when one applies -- nil (no override
// at all, sessionconfig.SessionConfig.ReviewCounterReviewerModel's own
// documented "no override" zero value) for every session that is not a
// GitHub PR review session at all (pgx.ErrNoRows from GetBySessionID, the
// overwhelming common case: most sessions are ordinary build sessions with
// no github_pr_sessions row), OR that IS a review session but has no
// resolvable authoring-model provenance (a human-authored PR -- see
// reviewtriage.ResolveCounterReviewerModel's own doc comment for why this
// is the common case even among review sessions, not a degraded one), OR
// (B2 fix) has a resolvable authoring model but no OTHER catalog provider
// this session actually has a usable credential for -- see
// reviewCredentialedProviders' own doc comment. Best-effort, NEVER errors
// (mirrors reviewtriage.ResolveProvenance's own "the review depth decision
// this signal rides alongside must never be delayed or blocked by a
// degraded authorship lookup" contract, applied here to session boot
// instead) -- a failed lookup degrades to no override, never a blocked
// spawn (§10-P2: "never block a spawn"). Logs the resolved decision either
// way (B2 fix: "no observability on the counter-reviewer pin") -- the ONE
// place this decision is made, so an operator investigating "why did the
// counter-reviewer run under model X" (or "why didn't it get an opposing
// pin at all") has a single log line to search for, keyed by repo/PR.
//
// prSession is the session's pull request claim, read once by reviewClaim
// for this and the pull request ref alike; nil for every session that has
// none.
func (a *Actor) reviewCounterReviewerModel(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, prSession *sqlcgen.GithubPrSession) *string {
	if prSession == nil {
		return nil
	}

	triageDeps := appreviewtriage.Deps{Artifacts: a.stores.artifact, Sessions: a.stores.session}
	prov := appreviewtriage.ResolveProvenance(ctx, triageDeps, prSession.RepoFullName, prSession.PrNumber)
	if prov.AuthoringModel == "" {
		// The overwhelming common case (human-authored PR, or a Narvi-
		// authored one with no recorded build_model_id) -- nothing to
		// oppose, so skip the credential lookup below entirely rather than
		// paying for a query whose answer could never change this outcome.
		return nil
	}

	credentialedProviders := a.reviewCredentialedProviders(ctx, tx, sessionRow)
	model := appreviewtriage.ResolveCounterReviewerModel(prov.AuthoringModel, credentialedProviders)
	logger := platform.Logger(ctx)
	if model == "" {
		logger.Info("sessionactor: review counter-reviewer: no opposing-model override resolved",
			"repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber,
			"authoring_model", prov.AuthoringModel, "credentialed_providers", credentialedProviders)
		return nil
	}
	logger.Info("sessionactor: review counter-reviewer: pinned opposing model",
		"repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber,
		"authoring_model", prov.AuthoringModel, "counter_reviewer_model", model)
	return &model
}

// reviewClaim reads sessionRow's pull request claim (github_pr_sessions),
// the one read assembleSessionConfig makes for both the pull request ref
// (pullRequestRef) and the counter-reviewer override
// (reviewCounterReviewerModel). nil for a session that is not a pull
// request's review session (pgx.ErrNoRows, the overwhelming common case),
// and nil, logged, on any other failure: both readers are best-effort and
// never block a spawn (§10-P2), and a review session assembled without its
// ref boots exactly as one did before the ref existed.
func (a *Actor) reviewClaim(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session) *sqlcgen.GithubPrSession {
	if a.stores.githubPRSession == nil {
		return nil
	}
	prSession, err := a.stores.githubPRSession.WithTx(tx).GetBySessionID(ctx, sessionRow.ID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			platform.Logger(ctx).Warn("sessionactor: read the session's pull request claim for its session config failed, assembling it without",
				"error", err)
		}
		return nil
	}
	return &prSession
}

// pullRequestRef is the ref a review session's primary repo is checked out
// at (technical plan §21.1, §30.4): the pull request's head ref, which a
// code host keeps in the pull request's base repository. It is derived
// here, each time a SESSION_CONFIG is assembled, from the session's claim,
// and never stored. nil when there is no claim, when the claim's number
// makes no valid ref, or when repo's url does not name the claim's
// repository: such a ref is not in the repository cloned. The GitHub
// ingress writes the base repository for every pull request; a session
// created before it did names the fork, and is moved onto the base in the
// transaction that spawns or restores its next gen, before this runs
// (reviewbaserepository.go). A spec the move cannot read -- not one https
// repo in owner/name shape -- is left as it is, and that session boots on
// its branch as before.
func pullRequestRef(prSession *sqlcgen.GithubPrSession, repo sessionconfig.SessionConfigReposElem) *string {
	if prSession == nil {
		return nil
	}
	owner, name, err := reposource.ParseOwnerRepo(repo.Url)
	if err != nil || !strings.EqualFold(owner+"/"+name, prSession.RepoFullName) {
		return nil
	}
	ref := reposource.PullHeadRef(prSession.PrNumber)
	if reposource.ValidatePullHeadRef(ref) != nil {
		return nil
	}
	return &ref
}

// reviewCredentialedProviders resolves the set of counterReviewerProviderPreference
// providers (internal/app/reviewtriage) that sessionRow actually has a
// usable credential for -- B2 fix (adversarial review of §26.4): "prefer
// no pin over guessing when the opposing provider is not
// known-credentialed". A thin wrapper over CounterReviewCredentialedProviders
// (below) on this actor's own stores and transaction.
//
// nil (a.stores.providerCredential/githubPRSession == nil, or any
// read/parse failure) is the SAME safe degradation this file's own
// reviewCounterReviewerModel already established for every other
// best-effort lookup: a nil map read is always false in Go, so
// ResolveCounterReviewerModel's own credential gate treats "we could not
// determine this" identically to "nothing is credentialed" -- never a
// guess, and never a blocked spawn either (§10-P2).
func (a *Actor) reviewCredentialedProviders(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session) map[string]bool {
	if a.stores.providerCredential == nil || a.stores.githubPRSession == nil {
		return nil
	}
	return CounterReviewCredentialedProviders(ctx, a.stores.githubPRSession.WithTx(tx), a.stores.providerCredential.WithTx(tx), sessionRow)
}

// CounterReviewCredentialedProviders is the counter-reviewer's credential
// read: the providers sessionRow's own resolution reaches, keyed on
// exactly what httpapi.ProviderCredentialsDelivery keys it on -- both load
// the session's credentialscope.Scope and list its candidates through it,
// so the user scope is read by the one rule (providercredential.
// UserScopeTarget) and a pull request's review session never counts its
// requester's personal link as a credentialed provider: its opposing model
// is chosen among the deployment's credentials only, and with none, no
// override is pinned (ResolveCounterReviewerModel's own fallback). It
// stops at EXISTENCE (reviewtriage.CredentialedProviders): nothing is
// decrypted, and ValueEncrypted is never read.
//
// Exported so httpapi's integration tests can run this function and the
// delivery endpoint over the same sessions and pin that the two agree.
// nil on any read or parse failure, logged: never a guess, never a blocked
// spawn (§10-P2).
func CounterReviewCredentialedProviders(ctx context.Context, prSessions credentialscope.PRSessionReader, credentials credentialscope.CandidateLister, sessionRow sqlcgen.Session) map[string]bool {
	logger := platform.Logger(ctx)
	scope, err := credentialscope.Load(ctx, prSessions, sessionRow)
	if err != nil {
		logger.Warn("sessionactor: review counter-reviewer: load credential scope for opposing-model gating failed", "error", err)
		return nil
	}
	rows, err := scope.Candidates(ctx, credentials)
	if err != nil {
		logger.Warn("sessionactor: review counter-reviewer: list provider credentials for opposing-model gating failed", "error", err)
		return nil
	}
	return appreviewtriage.CredentialedProviders(rows)
}

// assembleSessionConfig builds the real SESSION_CONFIG document (§6.4) a
// freshly spawned OR restored sandbox receives -- design decision 6's own
// exact field mapping:
//
//   - BootMode: the caller-supplied bootMode -- Fresh for a plain spawn
//     (dispatch.go's planFreshSpawn), SnapshotRestore for a restore
//     (dispatch.go's planRestore, §3.2 "snapshots & restore", design
//     decision 6b: "thread a boolean/enum parameter through
//     assembleSessionConfig rather than hardcoding a second copy of this
//     function"). §8.5 ("image builds") upgrades a Fresh value to
//     RepoImage AFTER this function returns (dispatch.go's own
//     resolveAndSetImage, imageresolve.go), once -- and only once -- a
//     real, ready, matching prebuilt image is actually found for that
//     spawn's own fingerprint: internal/domain/sandboxboot.EvaluateHook's
//     own hook policy (§6.4) treats repo_image as "setup.sh already ran at
//     build time and does not run again", which is exactly the case a real
//     prebuilt-image spawn is; reporting Fresh in that case would make
//     sandbox-agent redundantly re-run setup.sh at every boot regardless,
//     defeating the entire point of image prebuilding. A restore's own
//     BootMode is deliberately never upgraded this way (see
//     resolveAndSetImage's own doc comment for why). BootModeBuild remains
//     an unused placeholder even after §8.5 -- ports.SandboxProvider.
//     BuildImage's own signature (§4.1) carries no SessionConfig at all, so
//     there is no SessionConfig for this control plane to ever stamp
//     BootModeBuild onto; that value is reserved for whatever
//     provider-internal (or future) mechanism actually drives a real
//     image-baking boot sequence, out of this Step's own scope since it
//     doesn't go through CreateSandbox/assembleSessionConfig at all.
//   - ControlPlaneWsUrl: publicWsBaseURL(a.publicBaseURL) +
//     "/sessions/{id}/ws?type=sandbox".
//   - CorrelationId: always nil (no ingress webhook exists yet to have
//     minted one -- SessionConfig.CorrelationId's own doc comment: "Null
//     only when no upstream correlation id exists").
//   - Gen: the sandbox row's own just-bumped gen.
//   - PathScope: §3.4's own addition -- environmentSubstrate(ctx, tx,
//     sessionRow.EnvironmentID), above; nil (absent from the wire document
//     entirely, via its own omitempty) for the overwhelming common,
//     unscoped case.
//   - Docker/EgressPolicy: §27.5's own additions (§27.5/§27.6) -- the
//     SAME environmentSubstrate call's other two return values. Docker is
//     env.DockerRequired verbatim (a plain bool, no further processing).
//     EgressPolicy, when its Mode == EgressModeAllowlist, is threaded
//     through environment.AppendAllowlistFloor with THIS session's own
//     freshly-computed floor (allowlistFloorHosts, below) before ever
//     reaching the wire -- computed fresh on every assembly, never read
//     back from a previously-appended value, so the floor can never go
//     stale (brief point B: "appended on the server as the policy is
//     built, never merely validated at input").
//   - Repos: read back from sessions.repos; on a pull request's review
//     session the primary repo also carries the pull request's head ref,
//     derived from its claim (pullRequestRef), never stored.
//   - SandboxId: sandboxID, the caller's already-known sandboxes.id
//     (row.ID.String() at the one production call site, tryPlanSpawn) --
//     this sandbox's own stable, real identity, closing the env-leak
//     remediation batch's other honest gap: the ONLY channel into the
//     sandbox's own environment (NARVI_SESSION_CONFIG) is now how
//     sandbox-agent learns its own X-Sandbox-ID for the sandbox WS
//     handshake (§6.1), instead of always defaulting to "".
//   - SandboxToken: the freshly minted PLAINTEXT token (never logged).
//   - SessionId: the session's own id string.
func (a *Actor) assembleSessionConfig(
	ctx context.Context, tx pgx.Tx,
	sessionRow sqlcgen.Session, gen int, plaintextToken, sandboxID string, bootMode sessionconfig.SessionConfigBootMode,
) (sessionconfig.SessionConfig, error) {
	wsBase, err := publicWsBaseURL(a.publicBaseURL)
	if err != nil {
		return sessionconfig.SessionConfig{}, err
	}

	repos, err := reposFromJSON(sessionRow.Repos)
	if err != nil {
		return sessionconfig.SessionConfig{}, err
	}

	prSession := a.reviewClaim(ctx, tx, sessionRow)
	if len(repos) > 0 {
		repos[0].Ref = pullRequestRef(prSession, repos[0])
	}

	pathScope, dockerRequired, egressPolicy, err := a.environmentSubstrate(ctx, tx, sessionRow.EnvironmentID)
	if err != nil {
		return sessionconfig.SessionConfig{}, err
	}
	var pathScopeField *sessionconfig.SessionConfigPathScope
	if len(pathScope) > 0 {
		typed := sessionconfig.SessionConfigPathScope(pathScope)
		pathScopeField = &typed
	}

	// egressPolicyField stays nil (absent from the wire document, §27.6's
	// own "genuinely OPTIONAL... absent or null both mean no egress
	// policy is attached") unless egressPolicy actually has a Mode set --
	// environment.EgressPolicy's own zero value ("not configured") must
	// never round-trip onto the wire as an empty-but-present object.
	var egressPolicyField *sessionconfig.SessionConfigEgressPolicy
	if egressPolicy.Mode != "" {
		if egressPolicy.RequiresEnforcement() {
			// Point B (brief): the floor is appended HERE, structurally,
			// every single time -- never merely validated once when the
			// customer's own allowlist was first saved. See
			// allowlistFloorHosts' own doc comment for what it computes.
			egressPolicy = environment.AppendAllowlistFloor(egressPolicy, a.allowlistFloorHosts(ctx, repos))
		}
		// allowlist is normalized to a non-nil (possibly empty) slice
		// before hitting the wire -- the schema types "allowlist" as a
		// plain (non-nullable) array; a nil Go slice would otherwise
		// marshal as JSON null, which satisfies the generated decoder's
		// own "key present" check but is not the array shape the schema
		// actually declares.
		allowlist := egressPolicy.Allowlist
		if allowlist == nil {
			allowlist = []string{}
		}
		egressPolicyField = &sessionconfig.SessionConfigEgressPolicy{
			Mode:      sessionconfig.SessionConfigEgressPolicyMode(egressPolicy.Mode),
			Allowlist: allowlist,
		}
	}

	sessionID := sessionRow.ID.String()
	controlPlaneWsURL := strings.TrimSuffix(wsBase, "/") + "/sessions/" + sessionID + "/ws?type=sandbox"

	return sessionconfig.SessionConfig{
		BootMode:          bootMode,
		ControlPlaneWsUrl: controlPlaneWsURL,
		CorrelationId:     nil,
		Gen:               gen,
		PathScope:         pathScopeField,
		Docker:            dockerRequired,
		EgressPolicy:      egressPolicyField,
		Repos:             repos,
		SandboxId:         sandboxID,
		SandboxToken:      plaintextToken,
		SessionId:         sessionID,
		// CapabilityRestricted (§17.2): true exactly for a
		// sentinel-auto-fix child session -- see provenance.
		// IsSentinelAutoFix's own doc comment for the three independent
		// things that key off this SAME provenance_tag value; this is the
		// third: sandbox-agent writes the glob-restricted OpenCode agent
		// config into the workspace before ever spawning `opencode serve`
		// for this ONE kind of session.
		CapabilityRestricted: provenance.IsSentinelAutoFix(sessionRow.ProvenanceTag),
		// ReviewCounterReviewerModel (§26.4): nil for every
		// session that either is not a GitHub PR review session at all, or
		// is one but has no resolvable authoring-model provenance to
		// oppose -- see reviewCounterReviewerModel's own doc comment,
		// above, for the full "why nil is the common case, not a
		// degradation".
		ReviewCounterReviewerModel: sessionconfig.SessionConfigReviewCounterReviewerModel(a.reviewCounterReviewerModel(ctx, tx, sessionRow, prSession)),
	}, nil
}
