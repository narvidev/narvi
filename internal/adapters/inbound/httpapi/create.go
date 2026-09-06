package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/intentclassifier"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/environment"
	"github.com/narvidev/narvi/internal/domain/provenance"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// scopedEnvironmentProvenanceTag is the provenance_tag value (§14.1: "carry
// a provenance tag ... so the label automation and the handoff sentinel
// (§14.4) can act on it without re-deriving intent") CreateSession writes
// onto a session's sessions.provenance_tag column whenever
// environment.RequiresProvenanceTag reports true for the Environment it just
// created. §14.1 does not specify an exact wire value, so this was this
// package's own concrete choice -- a single fixed constant, not derived
// from anything about the request, since today there is exactly one
// reason a session ever carries a provenance tag at all (a non-empty
// pathScope).
//
// (§14.4, "handoff-readiness sentinel") promotes the underlying
// string into internal/domain/provenance.ScopedEnvironment (that
// package's own doc comment explains why: a fourth caller,
// internal/app/sessionactor, needs to read this exact value and cannot
// import httpapi). This alias keeps every reference in THIS file
// unchanged -- see provenance.ScopedEnvironment's own doc comment for the
// authoritative definition; this is deliberately the ONLY place the
// string literal itself is written in Go source.
const scopedEnvironmentProvenanceTag = provenance.ScopedEnvironment

// ChildSessionOptions is CreateSessionOnTx's own additive, OPTIONAL extra
// parameter ("sentinels + suggestions", §17.2) -- a variadic
// trailing parameter (see CreateSessionOnTx's own signature) so every
// EXISTING call site (every caller before this Step, including internal/
// adapters/inbound/github's own coalesce.go, which imports this function
// directly) keeps compiling and behaving byte-for-byte identically with
// no change of its own: omitting the argument entirely is exactly the
// same as passing a zero-value ChildSessionOptions{}, which sets nothing.
//
// SpawnChildSession (childsession.go) is this Step's own general-purpose
// supplier of a non-zero value, for a caller with no already-open
// transaction of its own. internal/app/outboxworker's own
// sentinelAutoFixNotifier (sentinelautofix.go) is, since the Finding-1
// audit fix, a SECOND supplier that instead calls CreateSessionOnTx
// directly (never through SpawnChildSession): that notifier needs to
// compose the session insert with its OWN atomic per-claim row lock, on
// its own already-open transaction -- exactly the shape this function's
// own doc comment above describes ("an atomic per-resource claim lock ...
// before ever reaching this function").
type ChildSessionOptions struct {
	// ParentSessionID is the review session that spawned this child
	// session (§17.2) -- pgtype.UUID{} (invalid/NULL) for every ordinary
	// session.
	ParentSessionID pgtype.UUID
	// SpawnDepth is 1 for a direct child of a depth-0 session -- this
	// Step's own one producer always sets exactly 1 (migrations/000045's
	// own doc comment: recorded as honest, queryable data, never gated on
	// numerically by any code path in this Step -- §17.1's "no recursion"
	// rule is enforced via ProvenanceTag below, not a depth check).
	SpawnDepth int32
	// ProvenanceTag, when non-nil, OVERRIDES whatever this function would
	// otherwise compute for sessions.provenance_tag from the request's own
	// pathScope/mockConfig (the "scoped_environment" tag, above) -- a
	// sentinel-auto-fix child session carries provenance.SentinelAutoFix
	// instead, and is never itself a scoped-Environment session at the
	// same time (§17.2: "in the origin PR's own environment -- full
	// access -- never a scoped prototyping environment").
	ProvenanceTag *string

	// ReviewHeadSHA is
	// unrelated to every other field on this struct (none of which this
	// function's OWN doc comment's "parent/child" framing describes) --
	// bundled into this SAME trailing-variadic options struct anyway,
	// mirroring httpapi.CreateTurnOptions' own identical "bundle a
	// genuinely unrelated, optional, caller-supplied concern behind the
	// one variadic slot a function already has, rather than a new
	// parameter that would ripple into every OTHER call site" precedent
	// (turn.go) -- CreateSessionOnTx is called from several packages
	// (this one, internal/adapters/inbound/github's own coalesce.go),
	// and only ONE of them (a brand-new GitHub PR review session's own
	// FIRST turn) ever has a review head SHA to supply. Non-nil only for
	// that ONE caller; every other caller (including this Step's own
	// sentinel-auto-fix spawner) leaves it nil. Stored verbatim onto the
	// new session's own first turn (turns.review_head_sha) at INSERT time
	// below -- see CreateTurnOptions.ReviewHeadSHA's own doc comment
	// (turn.go) for the full "why".
	ReviewHeadSHA *string

	// ReviewDepth/ReviewDepthDecision (§26.3) mirror
	// ReviewHeadSHA's own identical shape immediately above -- see
	// CreateTurnOptions.ReviewDepth/ReviewDepthDecision's own doc comment
	// (turn.go) for the full "why".
	ReviewDepth         *string
	ReviewDepthDecision []byte
}

// childSessionOptionsFrom returns opts[0] if the caller supplied one, or
// the zero value otherwise -- the one place CreateSessionOnTx unwraps its
// own variadic parameter, so every read site below stays a plain field
// access.
func childSessionOptionsFrom(opts []ChildSessionOptions) ChildSessionOptions {
	if len(opts) > 0 {
		return opts[0]
	}
	return ChildSessionOptions{}
}

// defaultContractsPath is the contracts_path value CreateSession stores
// when a request's mockConfig is present but omits (or nulls)
// contractsPath -- Row 27's ("mocking + contract drift", §14.3) own
// concrete choice, matching §14.3's own "a shared contracts/api/*.{yaml,json}
// spec" example path exactly.
const defaultContractsPath = "contracts/api"

// CreateSession backs POST /api/sessions (§6.3), mounted (§13.1, "auth
// v1") behind internal/adapters/inbound/auth.Middleware -- see doc.go's own
// updated writeup. Decodes restdtos.CreateSessionRequest from a body
// bounded by http.MaxBytesReader(maxRequestBodyBytes) -- an oversized body
// surfaces as *http.MaxBytesError, reported as 413; any other decode
// failure (malformed JSON) is 400. repos' own schema-level minItems:1 is
// not enforced by Go's plain json.Unmarshal, so it is checked explicitly
// here -- 400 on an empty list.
//
// Each repo's own Name/Url/Branch is then validated via
// internal/domain/reposource's exported, plain-string validators -- the
// SAME package/functions internal/sandboxagent/gitclone's own
// validateRepoSpec already calls at actual git-invocation time. This is
// the trust-boundary half of defense in depth: without it, a malformed
// repo spec sat in Postgres past a 201 and only surfaced as a confusing,
// delayed spawn failure deep inside the sandbox agent. Validated here, in
// order, stopping at the first failure (matching validateRepoSpec's own
// stop-at-first-failure precedent): Name (it later reaches
// filepath.Join(workspaceDir, repo.Name) in gitclone, so an unvalidated
// Name is exactly as path-traversal-shaped a risk as an unvalidated Url/
// Branch), then Url, then Branch -- but ONLY when Branch is non-nil (nil
// means "use the repo's own default branch", exactly gitclone's own
// precedent for this identical nullable field). This runs entirely
// BEFORE pool.Begin below, so a rejected repo spec never reaches Postgres
// at all. gitclone's own validation at the deep git-invocation site is
// left completely unchanged -- sandbox-agent must never trust what it
// receives, even from a layer that validates first.
//
// §9.3 ("e2e happy path") update: req.Repos is now actually PERSISTED
// (marshaled to the sessions.repos JSONB column -- design decision 1,
// migrations/000018_session_repos.up.sql). When req.Prompt is non-nil, a
// Turn row is ALSO inserted, in the SAME Postgres transaction as the
// session insert (mirroring internal/adapters/inbound/auth's own
// createUserAndIdentity pool.Begin/WithTx/Commit pattern exactly, so a
// failure partway through never leaves an orphaned session with no turn
// or vice versa). After a successful commit, GetOrSpawn + Send(
// EnsureDispatched{}) run SYNCHRONOUSLY but are still "fire and forget" in
// the sense that matters: GetOrSpawn only hydrates local actor state (a
// few fast Postgres round trips, no external network call) and Send only
// enqueues into the actor's own mailbox -- the SLOW work (a real
// SandboxProvider.CreateSandbox call, if a spawn decision fires) happens
// entirely on the actor's own already-running background goroutine,
// AFTER this handler has already returned its 201. No naked goroutine is
// spun up here for that reason -- see this func's own inline comment at
// the call site.
//
// Row 10 ("domain: Environment scoping", §14.1) update: req.PathScope is
// OPTIONAL -- absent or null leaves environment_id/provenance_tag both
// NULL, byte-for-byte today's existing unscoped behavior. When non-empty,
// internal/domain/environment.ValidatePathScope validates every pattern
// BEFORE any Postgres write, exactly the same trust-boundary precedent the
// repo validation above already established (reject with 400 on the
// first invalid pattern, never call pool.Begin on that path). When valid,
// a new environments row is inserted in the SAME transaction as the
// session itself, the session's environment_id is set to that row's id,
// and provenance_tag is set to scopedEnvironmentProvenanceTag whenever
// environment.RequiresProvenanceTag (the real domain function, not a
// re-derived local check) reports true for it.
//
// Row 27 ("mocking + contract drift", §14.3) update: req.MockConfig is a
// SECOND, independent optional Environment attribute alongside PathScope
// (§14.1: "an optional path_scope ... and an optional mock_config" -- two
// separate optional fields, not a package deal). An environments row is
// now created whenever EITHER hasPathScope OR hasMockConfig (mockConfig
// key present in the request body at all, even as {}) is true -- the
// pre-existing hasPathScope-only gate is widened to an OR, never narrowed.
// When hasMockConfig, contractsPath resolves to req.MockConfig.
// ContractsPath's own value when non-nil, otherwise the literal
// defaultContractsPath ("contracts/api") -- mock_configured is set true and
// contracts_path is set to that resolved value on the SAME environments
// row pathScope's own block already creates (or a freshly-created one, if
// mockConfig was supplied with no pathScope). provenance_tag's own
// RequiresProvenanceTag check is untouched -- it only ever depends on
// PathScope (environment.RequiresProvenanceTag's own doc comment), so a
// mockConfig-only Environment does not, by itself, cause a session to
// carry a provenance tag.
//
// Audit remediation (security-crosscutting lens): a caller-supplied
// mockConfig.contractsPath previously reached Postgres (and, downstream,
// a real outbound GitHub API request built by internal/adapters/outbound/
// githubapi.ResolveContractsFingerprint) with ZERO validation -- unlike
// pathScope, which ValidatePathScope already gated. Fixed below by running
// the newly added environment.ValidateContractsPath (this same batch's own
// addition, mirroring ValidatePathScope's own ".." rejection at minimum,
// plus rejecting "?"/"#" -- see that function's own doc comment) BEFORE
// pool.Begin, the SAME trust-boundary precedent every other request field
// this handler validates already follows.
//
// Audit remediation (wire-contract/security-adjacent lens): req.SpawnSource
// -- a plain client-supplied JSON field -- previously reached Postgres
// VERBATIM via CreateSessionOnTx's own session insert below, with no
// restriction to "web" on this handler, even though this is the ONLY
// authenticated REST session-creation path (every other spawnSource --
// Slack/Linear/GitHub ingress -- calls CreateSessionCore/CreateSessionOnTx
// directly, from its own separate package, never through here). An
// authenticated web caller could claim spawnSource: "slack"/"linear"/
// "github" and get it persisted as-is, forging provenance in the UI's own
// source icons/filters and audit rows, and steering app/sessionactor/
// outboxenqueue.go's own turn-completion outbox routing (which genuinely
// branches on sessions.spawn_source) down a channel the session was never
// actually created through. Fixed below, right after decoding the body and
// before ever calling CreateSessionCore: any decoded req.SpawnSource other
// than "web" is rejected with 400 -- CreateSessionCore/CreateSessionOnTx
// themselves are UNCHANGED by this fix, so the bot-ingress callers above
// keep passing their own genuine spawnSource exactly as before.
//
// §5.1 ("webhook toolkit") update: everything this func used to do
// AFTER decoding the request body is now CreateSessionCore below -- a
// pure extraction, not a behavior change (every case this func's own doc
// comment above describes, and every existing test in this package's own
// _test.go files, is unchanged). The only two things that stay HERE,
// specific to the browser/REST path, are decoding the body off an actual
// *http.Request and requiring a real authenticated human caller via
// authenticatedUserID -- a webhook ingress handler (§8.2/§8.10) calls
// CreateSessionCore directly with its own already-decoded request and a
// NULL createdBy (no cookie, no human), never this func.
//
// §8.10 ("Slack ingress") update: CreateSessionCore (and
// CreateSessionError, alongside it) is now EXPORTED -- doc.go's own §5.1
// writeup left the unexported-vs-exported question deliberately open
// for §8.2/§8.10 to decide ("Whether that turns out to be ... or
// §8.2/§8.10 decide createSessionCore should be exported instead, is left
// to that work"). internal/adapters/inbound/slack lives in its own
// package (mirroring httpapi/linear/github's own one-package-per-ingress-
// surface shape, not folded into this one), so it needs the exported
// form to reach this function at all -- an unexported identifier is not
// reachable from outside internal/adapters/inbound/httpapi. This is
// still a pure rename, not a behavior change: every existing call site
// and test in this package keeps compiling (Go does not care whether a
// same-package caller uses the exported or unexported spelling).
//
// Reconciliation update (tx-support split): CreateSessionCore itself is
// now a THIN pool-based wrapper around two smaller, EXPORTED pieces --
// CreateSessionOnTx (everything up to and including the optional turn
// insert, taking an ALREADY-OPEN transaction the caller owns) and
// TriggerDispatch (the post-commit GetOrSpawn+EnsureDispatched
// fire-and-forget pattern) -- see both functions' own doc comments below
// for why. CreateSession itself (this func) is untouched by that split:
// it still calls CreateSessionCore exactly as before, and every existing
// test in this package's own _test.go files passes unchanged. Likewise,
// CreateSessionCore's own external signature/behavior -- the two things
// §8.10's Slack ingress (above) actually depends on -- is unchanged by
// this split: same params, same (sqlcgen.Session, *CreateSessionError)
// return, same validate -> insert -> commit -> dispatch sequencing.
// intentSvc is nil-safe (see recordExplicitIntentDecision's own doc
// comment) so every existing call site that doesn't care about §8.3
// can keep passing nil unchanged.
func CreateSession(pool *pgxpool.Pool, sessions *postgres.SessionStore, turns *postgres.TurnStore, environments *postgres.EnvironmentStore, auditLog *postgres.AuditLogStore, registry *sessionactor.Registry, intentSvc *intentclassifier.Service, epistemicCheckDefault bool, rolloutMode platform.RolloutMode, repoSettings *postgres.RepoSettingsStore, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		createdBy, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		// §13.3 row 2: "Create sessions ... on own/joined sessions: admin,
		// maintainer, member" -- viewer never. No ownership resolution
		// needed here (there is no pre-existing resource to own yet, see
		// domain/authz's own doc comment on ActionCreateSession) -- the
		// zero-value Resource{} is always correct for this action.
		if !authorize(w, r, authz.ActionCreateSession, authz.Resource{}) {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

		var req restdtos.CreateSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}

		// Audit remediation (wire-contract/security-adjacent lens): this
		// handler is the only authenticated REST session-creation path
		// (every other spawnSource-bearing caller -- Slack/Linear/GitHub
		// ingress -- constructs sessions through its own separate code
		// path, calling CreateSessionCore/CreateSessionOnTx directly, never
		// this REST handler; see this func's own doc comment above).
		// req.SpawnSource is nonetheless a plain client-supplied JSON field
		// on this same request body, and was previously persisted onto the
		// new session row VERBATIM below -- letting an authenticated web
		// caller claim spawnSource: "slack"/"linear"/"github" here, forging
		// provenance in the UI's own source icons/filters and audit rows,
		// and steering app/sessionactor/outboxenqueue.go's own turn-
		// completion routing (which genuinely branches on
		// sessions.spawn_source) down a channel this session was never
		// actually created through. Rejected here instead: the decode step
		// above already guarantees req.SpawnSource is one of the 4 valid
		// enum values (restdtos.CreateSessionRequestSpawnSource's own
		// UnmarshalJSON rejects a missing key or any value outside
		// {web,slack,linear,github} before we ever reach this line), so
		// the only real question left is whether it's "web" -- anything
		// else is a deliberately-wrong claim, rejected with a 400 rather
		// than silently coerced, the same reject-don't-silently-coerce
		// convention validateCreateSessionRequest below already applies to
		// every other caller-supplied field.
		if req.SpawnSource != restdtos.CreateSessionRequestSpawnSourceWeb {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("spawnSource: must be %q on this endpoint, got %q", restdtos.CreateSessionRequestSpawnSourceWeb, req.SpawnSource))
			return
		}

		// rolloutMode/repoSettings (§32): a 403 with an explicit
		// "repository not enrolled" message (checkRolloutGate's own
		// Message) is exactly what the generic writeError(w, cerr.Status,
		// cerr.Message) branch immediately below already produces for
		// ANY CreateSessionError -- REST needs no special-casing of
		// CreateSessionError.RolloutRefusal at all, unlike the three
		// non-REST ingress channels (Linear/Slack/the sentinel-autofix
		// outbox), which route an ordinary error down a retry path that
		// must be bypassed for a permanent policy refusal (see each of
		// those call sites' own doc comments).
		created, cerr := CreateSessionCore(ctx, pool, sessions, turns, environments, auditLog, registry, req, createdBy, epistemicCheckDefault, rolloutMode, repoSettings, prSessions)
		if cerr != nil {
			writeError(w, cerr.Status, cerr.Message)
			return
		}

		// §8.3 ("intent classifier", §8.3/§18): this is the ONE
		// surface that ever supplies its own decision rather than calling
		// Classify -- a human's own explicit plan/build toggle on the web
		// UI, known the moment the session is created (§18.4's own
		// "architecturally capable of having classified it themselves"
		// carve-out). See recordExplicitIntentDecision's own doc comment.
		// The surface argument is hardcoded to "web", NOT req.SpawnSource:
		// req.SpawnSource is a client-supplied JSON field on this same
		// request body, and this handler (CreateSession, the generic
		// authenticated /api/sessions REST endpoint) is structurally only
		// ever reachable as the real web surface -- Slack/Linear/GitHub
		// ingress each construct sessions through their own separate code
		// paths (CreateSessionCore/CreateSessionOnTx/CreateTurnForBot
		// called directly from internal/adapters/inbound/{slack,linear,
		// github}), never through this REST handler. §18.4 requires this
		// check to be "server-side and never trust a client-supplied
		// claim" -- honoring req.SpawnSource here would let a client
		// hitting /api/sessions directly claim spawnSource: "slack" (or
		// linear/github) in its JSON body and get an "explicit" decision
		// recorded against a surface this handler never actually is.
		// req.SpawnSource itself is, by this point, already forced to
		// "web" (the audit-remediation check above, run before
		// CreateSessionCore was ever called) -- so the "session's own
		// sessions.spawn_source column below" concern this comment used to
		// flag as separate and untouched is now closed too, by that same
		// fix.
		recordExplicitIntentDecision(ctx, intentSvc, created.ID, "web", req.PlanMode)

		writeJSON(w, http.StatusCreated, sessionToDTO(created))
	}
}

// CreateSessionError carries the exact (status, message) pair the HTTP
// handler should surface for a CreateSessionCore/CreateSessionOnTx
// failure -- a distinct type (rather than a plain error) so CreateSession's
// own writeError call sites, and every message they produce, stay
// byte-for-byte identical to what this codebase's existing tests already
// assert. Exported (alongside
// CreateSessionCore/CreateSessionOnTx) so a caller outside this package
// can inspect Status/Message directly -- including internal/adapters/
// inbound/slack (§8.10), which reads cerr.Status/cerr.Message directly.
type CreateSessionError struct {
	Status  int
	Message string

	// RolloutRefusal (§10 Phase 6, §32) is true iff this error
	// came from checkRolloutGate (rolloutgate.go) -- a PERMANENT policy
	// refusal ("this repo is not enrolled in the cohort rollout"), never a
	// transient failure. §32's own "machine-checkable refusal marker"
	// requirement: three of CreateSessionOnTx's own callers (Linear,
	// Slack, the sentinel-autofix outbox) route an ordinary create error
	// down a RETRY path (release a claim, redeliver) that is correct for a
	// transient DB error but actively wrong for a refusal that will
	// reproduce identically on every retry forever -- checking this field
	// (never Message, which is prose, not an API) is how each of those
	// three call sites tells the two apart structurally.
	RolloutRefusal bool

	// RepoEntitlementDenied (§31.4) is true iff this error came
	// from checkRepoEntitlementGate (repoentitlementgate.go) with a
	// DEMONSTRATED (never merely transient) denial -- a further
	// machine-checkable refusal marker, mirroring RolloutRefusal's own
	// exact shape and reasoning immediately above: a repo that is not
	// entitled will be refused identically on every retry until it is
	// (i.e. until it has a real github_pr_sessions row), so a caller that
	// routes ANY error down a blind retry path should tell the two apart
	// structurally, never by string-matching Message. See
	// checkRepoEntitlementGate's own doc comment for the full fail-closed-
	// vs-terminal split this field depends on.
	RepoEntitlementDenied bool
}

func (e *CreateSessionError) Error() string { return e.Message }

// validatedCreateSessionInput carries every value validateCreateSessionRequest
// has already normalized/derived from a request -- reposJSON (the
// marshaled req.Repos, ready for the session insert), the resolved
// pathScope slice and its hasPathScope flag, and hasMockConfig/
// contractsPath -- so a caller that already validated does not need to
// re-derive any of it.
type validatedCreateSessionInput struct {
	reposJSON     []byte
	pathScope     []string
	hasPathScope  bool
	hasMockConfig bool
	contractsPath string

	// docker/hasDocker (§27.5) mirror pathScope/hasPathScope's own
	// shape: docker is req.Docker itself (a plain bool, no tri-state
	// needed -- absent and explicit-false are behaviorally identical), and
	// hasDocker is docker's own value, kept as a separate field purely for
	// symmetry with hasMockConfig/hasEgressPolicy at this struct's other
	// call sites (createSessionOnTx's own "does this request need a new
	// Environment row at all" gate).
	docker    bool
	hasDocker bool

	// egressPolicy/hasEgressPolicy (§27.6) mirror mockConfig's own
	// shape: hasEgressPolicy is true whenever the request body carried an
	// "egressPolicy" key at all (req.EgressPolicy != nil); egressPolicy is
	// the already-validated (environment.ValidateEgressPolicy) domain
	// value to persist. The zero value is EgressPolicy{} (Mode == ""),
	// exactly matching "no policy attached to this Environment" for the
	// hasEgressPolicy == false case.
	egressPolicy    environment.EgressPolicy
	hasEgressPolicy bool
}

// validateCreateSessionRequest performs every check CreateSession's own
// doc comment above describes as running "BEFORE any Postgres write" --
// repos non-empty, each repo's Name/Url/Branch (reposource), pathScope
// (environment.ValidatePathScope), and mockConfig.contractsPath
// (environment.ValidateContractsPath) -- and nothing else: no tx, no
// pool, no I/O of any kind, so it is always safe (and cheap) to call
// before a transaction/connection exists.
//
// It has exactly two callers, deliberately: CreateSessionCore calls it
// FIRST, before pool.Begin, so a request that fails this validation never
// acquires a pooled Postgres connection at all -- restoring the same
// trust-boundary invariant this handler documented before the tx-support
// split (a rejected repo/pathScope/contractsPath spec never reaches
// Postgres). CreateSessionOnTx ALSO calls it, at its own top, before
// touching tx -- necessary because CreateSessionOnTx is called directly
// by callers that already hold their own open transaction (e.g. a webhook
// ingress handler mid-critical-section) and have not necessarily
// revalidated the request themselves. Calling it twice on the
// CreateSessionCore path (once to gate pool.Begin, once again inside
// CreateSessionOnTx) is deliberate, harmless, in-memory-only duplication,
// not a bug -- see CreateSessionCore's own doc comment below.
func validateCreateSessionRequest(req restdtos.CreateSessionRequest) (validatedCreateSessionInput, *CreateSessionError) {
	if len(req.Repos) < 1 {
		return validatedCreateSessionInput{}, &CreateSessionError{Status: http.StatusBadRequest, Message: "repos must be non-empty"}
	}

	// Validate every repo's Name/Url/Branch. Stops at the first failure,
	// in order, matching gitclone's own validateRepoSpec precedent
	// exactly; does not attempt to collect/report every failure across
	// every repo at once.
	for i, repo := range req.Repos {
		if err := reposource.ValidateRepoName(repo.Name); err != nil {
			return validatedCreateSessionInput{}, &CreateSessionError{Status: http.StatusBadRequest, Message: fmt.Sprintf("repos[%d].name: %s", i, err)}
		}
		if err := reposource.ValidateRepoURL(repo.Url); err != nil {
			return validatedCreateSessionInput{}, &CreateSessionError{Status: http.StatusBadRequest, Message: fmt.Sprintf("repos[%d].url: %s", i, err)}
		}
		if repo.Branch != nil {
			if err := reposource.ValidateBranch(*repo.Branch); err != nil {
				return validatedCreateSessionInput{}, &CreateSessionError{Status: http.StatusBadRequest, Message: fmt.Sprintf("repos[%d].branch: %s", i, err)}
			}
		}
	}

	reposJSON, err := json.Marshal(req.Repos)
	if err != nil {
		return validatedCreateSessionInput{}, &CreateSessionError{Status: http.StatusInternalServerError, Message: "internal error"}
	}

	// pathScope is OPTIONAL (contracts/rest/v1/dtos.schema.json's
	// CreateSessionRequest.pathScope) -- req.PathScope may be nil
	// (absent) or point at a nil/empty slice (present but null/[]);
	// either way that means "unscoped", exactly today's existing
	// behavior. Only a genuinely non-empty pathScope triggers validation
	// + environment creation.
	var pathScope []string
	if req.PathScope != nil {
		pathScope = []string(*req.PathScope)
	}
	hasPathScope := len(pathScope) > 0

	if hasPathScope {
		if err := environment.ValidatePathScope(pathScope); err != nil {
			return validatedCreateSessionInput{}, &CreateSessionError{Status: http.StatusBadRequest, Message: fmt.Sprintf("pathScope: %s", err)}
		}
	}

	// mockConfig is OPTIONAL and INDEPENDENT of pathScope (row 27,
	// "mocking + contract drift", §14.3 -- see CreateSession's own doc
	// comment). hasMockConfig is true whenever the request body carried a
	// "mockConfig" key at all (req.MockConfig != nil), even as {} --
	// contractsPath resolves to the caller's own value when supplied,
	// otherwise defaultContractsPath.
	hasMockConfig := req.MockConfig != nil
	contractsPath := defaultContractsPath
	if hasMockConfig && req.MockConfig.ContractsPath != nil {
		contractsPath = *req.MockConfig.ContractsPath

		// Audit remediation (security-crosscutting lens): a
		// caller-supplied mockConfig.contractsPath previously reached
		// Postgres (and, downstream, a real outbound GitHub API request
		// built by internal/adapters/outbound/githubapi.
		// ResolveContractsFingerprint) with ZERO validation -- unlike
		// pathScope, which ValidatePathScope already gated.
		// defaultContractsPath itself is never run through this check --
		// it is this handler's own fixed, known-safe constant, not
		// caller input.
		if err := environment.ValidateContractsPath(contractsPath); err != nil {
			return validatedCreateSessionInput{}, &CreateSessionError{Status: http.StatusBadRequest, Message: fmt.Sprintf("mockConfig.contractsPath: %s", err)}
		}
	}

	// docker (§27.5) is a plain, always-present bool (unlike
	// pathScope/mockConfig's own genuinely-optional-key shape) -- see
	// validatedCreateSessionInput.docker's own doc comment for why no
	// tri-state is needed. Nothing to validate here: every bool value is
	// already valid; the fail-closed provider-capability check
	// (environment.CheckSubstrateCapabilities) runs separately, in
	// CreateSessionCore, once a real SandboxProvider is reachable -- this
	// function stays pure/I-O-free like every other check it runs.
	hasDocker := req.Docker

	// egressPolicy is OPTIONAL and INDEPENDENT of docker/pathScope/
	// mockConfig (row 27's own "either" gate, extended here), mirroring
	// mockConfig's own genuinely-optional-key shape exactly: hasEgressPolicy
	// is true whenever the request body carried an "egressPolicy" key at
	// all (req.EgressPolicy != nil).
	var egressPolicy environment.EgressPolicy
	hasEgressPolicy := req.EgressPolicy != nil
	if hasEgressPolicy {
		egressPolicy = environment.EgressPolicy{
			Mode:      environment.EgressMode(req.EgressPolicy.Mode),
			Allowlist: append([]string(nil), req.EgressPolicy.Allowlist...),
		}
		if err := environment.ValidateEgressPolicy(egressPolicy); err != nil {
			return validatedCreateSessionInput{}, &CreateSessionError{Status: http.StatusBadRequest, Message: fmt.Sprintf("egressPolicy: %s", err)}
		}
	}

	return validatedCreateSessionInput{
		reposJSON:       reposJSON,
		pathScope:       pathScope,
		hasPathScope:    hasPathScope,
		hasMockConfig:   hasMockConfig,
		contractsPath:   contractsPath,
		docker:          req.Docker,
		hasDocker:       hasDocker,
		egressPolicy:    egressPolicy,
		hasEgressPolicy: hasEgressPolicy,
	}, nil
}

// checkSubstrateCapabilitiesUpFront is §27.5's own up-front half of the
// "fail-closed, twice" rule (§27.5/§27.6, brief point A) -- the clearest-
// possible-UX refusal at session-creation time, BEFORE any Postgres
// write, when this request's own docker/egressPolicy asks for a substrate
// requirement the CONFIGURED provider does not report supporting.
//
// Deliberately its own function, not folded into validateCreateSessionRequest:
// that function is pure/I-O-free by design (CreateSessionCore's own doc
// comment: "no tx, no pool, no I/O of any kind, so it is always safe...
// to call before a transaction/connection exists") and is called from
// BOTH CreateSessionCore and CreateSessionOnTx -- but a live
// ports.SandboxProvider is only reachable via registry, which
// CreateSessionOnTx's OTHER direct callers (automation/sentinelfix/
// coalesce/child-session -- see that function's own doc comment) do not
// carry today, since none of them construct a request with docker/
// egressPolicy set (verified: none of their own req-building code sets
// either new field). This check therefore lives at the ONE call site
// that both has provider access AND is the sole path every externally-
// reachable, potentially docker/egressPolicy-carrying request funnels
// through today (CreateSessionCore -- Web/Slack/Linear/bot all call it).
// A future Step that threads Environment-level docker/egress inheritance
// into one of CreateSessionOnTx's other direct callers must add the
// identical check at that new call site -- named here, not silently left
// as a gap (mirroring this codebase's own "honest, documented limitation"
// convention).
//
// Nothing to check (returns nil immediately, without ever touching
// registry) when the request asks for neither req.Docker nor an
// enforcement-requiring req.EgressPolicy -- the overwhelming common case,
// and the reason a nil registry.Provider() (some test/dev setups) never
// blocks an ordinary session that never asked for either.
func checkSubstrateCapabilitiesUpFront(registry *sessionactor.Registry, req restdtos.CreateSessionRequest) *CreateSessionError {
	dockerRequired := req.Docker
	egressEnforcementRequired := req.EgressPolicy != nil && environment.EgressMode(req.EgressPolicy.Mode) == environment.EgressModeAllowlist
	if !dockerRequired && !egressEnforcementRequired {
		return nil
	}

	var caps ports.Capabilities
	if registry != nil {
		if provider := registry.Provider(); provider != nil {
			caps = provider.Capabilities()
		}
	}
	// A nil registry/provider leaves caps at its zero value (every field
	// false) -- fail-closed, the same as a real provider that genuinely
	// supports neither: a docker/enforced-egress request with no reachable
	// provider to honor it is refused exactly like one a real,
	// unsupporting provider refuses.

	if err := environment.CheckSubstrateCapabilities(dockerRequired, egressEnforcementRequired, caps.DockerInSandbox, caps.EgressPolicy); err != nil {
		return &CreateSessionError{Status: http.StatusUnprocessableEntity, Message: err.Error()}
	}
	return nil
}

// CreateSessionOnTx does everything CreateSession's own doc comment above
// describes AFTER decoding the request body, up to and including the
// optional turn insert: repo validation, pathScope/mockConfig validation,
// the conditional environment insert, the session insert, and the
// conditional turn insert -- all on tx. It deliberately does NOT call
// tx.Commit (or Rollback) and does NOT trigger post-commit dispatch: tx is
// an ALREADY-OPEN transaction the CALLER owns entirely (begins, commits or
// rolls back, in every case -- including every error path out of this
// function), so this function must never assume it is safe to finalize
// that transaction itself. This is what lets a caller that is already
// holding a different, unrelated lock on the SAME transaction (e.g. an
// atomic per-resource claim taken via SELECT ... FOR UPDATE before ever
// reaching this function) create the session+turn INLINE, on that same
// connection, instead of needing a second, simultaneous connection out of
// the pool for a nested pool.Begin -- exactly the connection-pool
// exhaustion/deadlock risk a second, independently-opened transaction
// would risk under real concurrent load.
//
// hasPrompt reports whether req.Prompt was non-nil (so a turn was
// actually inserted) -- the caller uses this, ONCE ITS OWN outer
// transaction has committed, to decide whether TriggerDispatch below is
// needed at all; CreateSessionOnTx itself never fires that trigger, since
// firing it before the caller's own commit would risk dispatching against
// a session/turn that a subsequent rollback then makes disappear.
//
// createdBy is a NULLABLE creator (pgtype.UUID with Valid == false stored
// as a genuine SQL NULL in sessions.created_by) -- matching sqlcgen.
// CreateSessionParams.CreatedBy's own pgtype.UUID nullability and this
// schema's own documented intent (contracts/rest/v1/dtos.schema.json:
// Session.createdBy, "Null for bot/automation-created sessions with no
// direct human user"). CreateSession (the HTTP handler above, via
// CreateSessionCore) always passes a Valid one today, since it still
// hard-requires authenticatedUserID -- but this function itself never
// assumes that: a webhook ingress caller (§8.2/§8.10) with no
// cookie-authenticated human passes an explicitly invalid pgtype.UUID{}
// here instead.
//
// auditLog is §13.2's own addition (§13.3: "written in the same
// transaction as the change"): an audit_log row is inserted on this SAME
// tx, right after the session row itself, for EVERY caller of this
// function -- the browser REST path (CreateSession, a real authenticated
// createdBy) and every webhook-ingress path (§8.2/§8.10's own GitHub/
// Slack/Linear session creation, createdBy left invalid) alike, mirroring
// sessions.created_by's own existing NULL-for-bot convention: actor_user_id
// is NULL on a bot-attributed row, never a fabricated "system user".
//
// epistemicCheckDefault (F6, adversarial review) is a REQUIRED
// parameter, exactly mirroring createTurnLocked's own identical parameter
// (turn.go's own doc comment on why: every call site must compile-time-
// decide what to pass, never a silently-defaulted zero value) -- this
// closes F6's own verified gap: this function's own turn insert below
// used to bypass turn.MaybeInjectEpistemicPreamble entirely (the ONE other
// raw turns.Create site, alongside workflowengine's dispatchNextAttempt
// and DecidePlanOnTx, that did), so a session created WITH a prompt here
// never got the devil's-advocate preamble even when the platform default
// (or this SAME session's own just-inserted EpistemicCheckEnabled
// override) called for it -- including the "compounding absurdity" F6
// names: a caller opting in with {"epistemicCheckEnabled": true, "prompt":
// "..."} used to get no preamble on the very turn it opted in for, since
// created.EpistemicCheckEnabled (read back from this SAME INSERT ...
// RETURNING *, so it reflects the value just written) is now consulted
// below instead. Most callers pass their own real, operator-configured
// platform.Config.EpistemicCheckDefault; internal/adapters/inbound/github's
// own coalesce.go (the WINNER path, a brand-new PR REVIEW session) passes
// a hardcoded false instead -- see that call site's own doc comment for
// why (F7: a review session must never get the builder-only preamble,
// mirroring reviewretrigger.go's identical REUSE-branch precedent).
//
// rolloutMode/repoSettings (§10 Phase 6, §32) are REQUIRED
// parameters, not a variadic/optional trailing slot -- deliberately
// mirroring epistemicCheckDefault's own "every call site must
// compile-time-decide what to pass, never a silently-defaulted zero
// value" precedent immediately above, one level stricter: an omittable
// gate parameter is an omitted gate, and §32's own standing rule is that
// a check every future call site must remember is not a guard. Every
// current and future caller of this function fails to compile until it
// supplies both -- see rolloutgate.go's own checkRolloutGate, called
// immediately below, right after validateCreateSessionRequest and BEFORE
// the environment/session inserts, on this SAME tx.
//
// prSessions (§31.4) is the IDENTICAL required-parameter
// discipline, one Step later: repoentitlementgate.go's own
// checkRepoEntitlementGate runs FIRST, before checkRolloutGate, on this
// SAME tx, so an unentitled repo is refused before this function does
// anything else that touches Postgres. It closes the clone amplification
// §31.4 names -- the sandbox credential helper
// (internal/sandboxagent/gitclone/clone.go) serves whatever repo list
// sessions.repos ends up naming, so the gate must run before that column
// is ever written, for every caller of this function (unlike rolloutMode,
// this gate has no NARVI_ROLLOUT_MODE-shaped no-op escape hatch) -- with
// exactly one deliberate exemption, req.SpawnSource == github, see
// checkRepoEntitlementGate's own doc comment for why that one is a
// correctness requirement, not a convenience.
func CreateSessionOnTx(ctx context.Context, tx pgx.Tx, sessions *postgres.SessionStore, turns *postgres.TurnStore, environments *postgres.EnvironmentStore, auditLog *postgres.AuditLogStore, req restdtos.CreateSessionRequest, createdBy pgtype.UUID, epistemicCheckDefault bool, rolloutMode platform.RolloutMode, repoSettings *postgres.RepoSettingsStore, prSessions *postgres.GitHubPRSessionStore, childOpts ...ChildSessionOptions) (session sqlcgen.Session, hasPrompt bool, cerr *CreateSessionError) {
	logger := platform.Logger(ctx)
	opts := childSessionOptionsFrom(childOpts)

	// All request validation (repos non-empty, each repo's Name/Url/
	// Branch, pathScope, mockConfig.contractsPath) lives in
	// validateCreateSessionRequest -- see its own doc comment for why
	// this function calls it too, even though CreateSessionCore (the
	// only caller with no already-open tx) already calls it before ever
	// reaching this function's tx parameter.
	validated, verr := validateCreateSessionRequest(req)
	if verr != nil {
		return sqlcgen.Session{}, false, verr
	}
	reposJSON := validated.reposJSON
	pathScope := validated.pathScope
	hasPathScope := validated.hasPathScope
	hasMockConfig := validated.hasMockConfig
	contractsPath := validated.contractsPath
	hasDocker := validated.hasDocker
	egressPolicy := validated.egressPolicy
	hasEgressPolicy := validated.hasEgressPolicy

	// §31.4's own entitlement gate: checked FIRST, right after
	// validation and BEFORE checkRolloutGate below -- see
	// checkRepoEntitlementGate's own doc comment (repoentitlementgate.go)
	// for the full "why here, why first" reasoning. Unlike checkRolloutGate,
	// this runs unconditionally, on every call, for every deployment.
	if eerr := checkRepoEntitlementGate(ctx, tx, prSessions, auditLog, createdBy, req); eerr != nil {
		return sqlcgen.Session{}, false, eerr
	}

	// §10's own primary gate (§10 Phase 6, §32): checked AFTER
	// validation, BEFORE the environment/session inserts below, on this
	// SAME tx -- see checkRolloutGate's own doc comment (rolloutgate.go)
	// for the full "why here" reasoning, including the no-op short-circuit
	// that keeps this a zero-cost no-op for every deployment that has
	// never set NARVI_ROLLOUT_MODE=cohort.
	if gerr := checkRolloutGate(ctx, tx, repoSettings, rolloutMode, req); gerr != nil {
		return sqlcgen.Session{}, false, gerr
	}

	// An environments row is inserted in this SAME transaction, BEFORE
	// the session row itself, so the session insert below can set
	// environment_id to it directly, whenever ANY of pathScope/mockConfig/
	// docker/egressPolicy was supplied -- matching CreateSession's own doc
	// comment (row 27's "either" gate, extended by §27.5/§27.6,
	// to the two new independent attributes). environment_id/
	// provenanceTag both stay their pgtype/Go zero values (NULL) when NONE
	// is present, identical to every session created before this batch.
	var environmentID pgtype.UUID
	var provenanceTag *string
	if hasPathScope || hasMockConfig || hasDocker || hasEgressPolicy {
		var pathScopeJSON []byte
		if hasPathScope {
			var marshalErr error
			pathScopeJSON, marshalErr = json.Marshal(pathScope)
			if marshalErr != nil {
				logger.Error("httpapi: marshal pathScope failed", "error", marshalErr)
				return sqlcgen.Session{}, false, &CreateSessionError{Status: http.StatusInternalServerError, Message: "internal error"}
			}
		}

		var contractsPathCol *string
		if hasMockConfig {
			contractsPathCol = &contractsPath
		}

		// egress_policy_mode/egress_policy_allowlist (§27.6)
		// store the CUSTOMER's own configured policy ONLY -- the
		// server-appended allowlist floor is never persisted here; it is
		// computed fresh every time a SessionConfig is assembled from
		// this row (internal/app/sessionactor's own assembleSessionConfig,
		// via environment.AppendAllowlistFloor) -- see migrations/
		// 000095_environment_docker_egress.up.sql's own doc comment for
		// why. Both columns stay nil/NULL unless egressPolicy was
		// actually supplied. egress_policy_allowlist is populated ONLY
		// when Mode == EgressModeAllowlist -- migrations/
		// 000095_environment_docker_egress.up.sql's own CHECK constraint
		// enforces this pairing at the schema level too (an "open" row
		// with a non-NULL allowlist column is structurally rejected), so
		// this must match it exactly rather than always marshaling
		// whatever egressPolicy.Allowlist happens to hold.
		var egressPolicyModeCol *string
		var egressPolicyAllowlistJSON []byte
		if hasEgressPolicy {
			mode := string(egressPolicy.Mode)
			egressPolicyModeCol = &mode
			if egressPolicy.Mode == environment.EgressModeAllowlist {
				var marshalErr error
				egressPolicyAllowlistJSON, marshalErr = json.Marshal(egressPolicy.Allowlist)
				if marshalErr != nil {
					logger.Error("httpapi: marshal egressPolicy.allowlist failed", "error", marshalErr)
					return sqlcgen.Session{}, false, &CreateSessionError{Status: http.StatusInternalServerError, Message: "internal error"}
				}
			}
		}

		env, envErr := environments.WithTx(tx).Create(ctx, sqlcgen.CreateEnvironmentParams{
			PathScope:             pathScopeJSON,
			MockConfigured:        hasMockConfig,
			ContractsPath:         contractsPathCol,
			DockerRequired:        hasDocker,
			EgressPolicyMode:      egressPolicyModeCol,
			EgressPolicyAllowlist: egressPolicyAllowlistJSON,
		})
		if envErr != nil {
			logger.Error("httpapi: create environment failed", "error", envErr)
			return sqlcgen.Session{}, false, &CreateSessionError{Status: http.StatusInternalServerError, Message: "internal error"}
		}
		environmentID = env.ID

		// The real domain function, not a re-derived local boolean --
		// see CreateSession's own doc comment. Depends only on PathScope,
		// exactly like RequiresProvenanceTag's own doc comment says --
		// a mockConfig-only Environment never causes this to fire.
		if environment.RequiresProvenanceTag(environment.Environment{PathScope: pathScope}) {
			tag := scopedEnvironmentProvenanceTag
			provenanceTag = &tag
		}
	}

	// (§17.2): an explicit ChildSessionOptions.ProvenanceTag
	// OVERRIDES whatever was just computed from pathScope/mockConfig above
	// -- see ChildSessionOptions' own doc comment for why a
	// sentinel-auto-fix child session is never ALSO a scoped-Environment
	// session at the same time.
	if opts.ProvenanceTag != nil {
		provenanceTag = opts.ProvenanceTag
	}

	created, err := sessions.WithTx(tx).Create(ctx, sqlcgen.CreateSessionParams{
		Title:         (*string)(req.Title),
		SpawnSource:   sqlcgen.SessionSpawnSource(req.SpawnSource),
		CreatedBy:     createdBy,
		Repos:         reposJSON,
		EnvironmentID: environmentID,
		ProvenanceTag: provenanceTag,
		// §8.1 ("plan mode, web", §12.2 item 3): only meaningful when
		// req.PlanMode is true, but stored unconditionally either way --
		// mirrors modelId's own "always stored, only meaningful in
		// context" convention (a non-plan-mode session simply never reads
		// it back).
		BuildModelID: (*string)(req.BuildModelId),
		// (§29.8): build_effort mirrors build_model_id's own
		// shape/storage convention exactly, one field over.
		BuildEffort: (*string)(req.BuildEffort),
		// ParentSessionID/SpawnDepth (§17.2, migrations/000045):
		// zero values (pgtype.UUID{}, int32(0)) for every ordinary
		// session -- see ChildSessionOptions' own doc comment.
		ParentSessionID: opts.ParentSessionID,
		SpawnDepth:      opts.SpawnDepth,
		// EpistemicCheckEnabled ("builder epistemic pre-action
		// check", §20.4) mirrors BuildModelID's own "always stored,
		// nil/absent means no session-level override" convention exactly
		// -- consulted later by turn.ResolveEpistemicCheckEnabled
		// (createTurnLocked, turn.go), never re-derived here.
		EpistemicCheckEnabled: (*bool)(req.EpistemicCheckEnabled),
	})
	if err != nil {
		logger.Error("httpapi: create session failed", "error", err)
		return sqlcgen.Session{}, false, &CreateSessionError{Status: http.StatusInternalServerError, Message: "internal error"}
	}

	if err := recordAuditLog(ctx, auditLog.WithTx(tx), createdBy, "session.create", "session", created.ID.String(), map[string]any{
		"spawn_source": string(created.SpawnSource),
	}); err != nil {
		logger.Error("httpapi: record session.create audit log failed", "error", err)
		return sqlcgen.Session{}, false, &CreateSessionError{Status: http.StatusInternalServerError, Message: "internal error"}
	}

	hasPrompt = req.Prompt != nil
	if hasPrompt {
		// F6 (adversarial review): the SAME shared gate
		// createTurnLocked/dispatchNextAttempt/DecidePlanOnTx now all
		// route through (internal/domain/turn.MaybeInjectEpistemicPreamble)
		// -- created.EpistemicCheckEnabled is THIS SAME session's own
		// just-inserted override (created is this function's own INSERT
		// ... RETURNING * result, above), never re-derived or re-read, so
		// a caller opting in via req.EpistemicCheckEnabled on this exact
		// request sees it take effect on this exact first turn. req.PlanMode
		// excludes per §20.3 exactly like every other caller.
		firstTurnPrompt := turn.MaybeInjectEpistemicPreamble(epistemicCheckDefault, created.EpistemicCheckEnabled, req.PlanMode, *req.Prompt)
		if _, err := turns.WithTx(tx).Create(ctx, sqlcgen.CreateTurnParams{
			SessionID:           created.ID,
			Status:              sqlcgen.TurnStatusPending,
			Prompt:              &firstTurnPrompt,
			ModelID:             (*string)(req.ModelId),
			Effort:              (*string)(req.Effort),
			PlanMode:            req.PlanMode,
			ReviewHeadSha:       opts.ReviewHeadSHA,
			ReviewDepth:         opts.ReviewDepth,
			ReviewDepthDecision: opts.ReviewDepthDecision,
		}); err != nil {
			logger.Error("httpapi: create turn failed", "error", err)
			return sqlcgen.Session{}, false, &CreateSessionError{Status: http.StatusInternalServerError, Message: "internal error"}
		}
	}

	return created, hasPrompt, nil
}

// TriggerDispatch is the post-commit "fire-and-forget" dispatch trigger
// every CreateSessionOnTx caller runs, once (and only once) its own outer
// transaction has committed successfully AND hasPrompt was true (a turn
// was actually created) -- GetOrSpawn hydrates local actor state (fast,
// no external network call) and Send only enqueues into the actor's own
// mailbox -- the actual spawn/dispatch decision (including any real
// SandboxProvider.CreateSandbox network call) runs entirely on the
// actor's own background goroutine, not on the caller's own goroutine, so
// this never blocks the caller on how long that decision takes. Errors
// from either step are only warn-logged, never returned -- by the time
// this runs, the session/turn are already durably committed, so a
// dispatch-trigger failure here must not itself surface as a
// session-creation failure to any caller.
func TriggerDispatch(ctx context.Context, registry *sessionactor.Registry, sessionID pgtype.UUID) {
	logger := platform.Logger(ctx)

	actor, spawnErr := registry.GetOrSpawn(ctx, sessionID)
	if spawnErr != nil {
		logger.Warn("httpapi: GetOrSpawn after session create failed", "error", spawnErr)
		return
	}
	if sendErr := actor.Send(ctx, sessionactor.EnsureDispatched{}); sendErr != nil {
		logger.Warn("httpapi: send EnsureDispatched after session create failed", "error", sendErr)
	}
}

// CreateSessionCore is the pool-based wrapper CreateSession (the HTTP
// handler above) and any other caller with no already-open transaction
// of its own use: it validates the request FIRST (validateCreateSession
// Request, below) -- a rejected repo/pathScope/mockConfig.contractsPath
// spec never reaches pool.Begin, let alone Postgres, restoring the same
// trust-boundary invariant this handler documented before the tx-support
// split (CreateSession's own doc comment above) -- then owns a SINGLE
// transaction start-to-finish (pool.Begin -> CreateSessionOnTx ->
// tx.Commit -> TriggerDispatch). Validating again inside CreateSessionOnTx
// is redundant on this path (harmless, in-memory only) but necessary for
// CreateSessionOnTx's OTHER callers -- see its own doc comment. With that
// pre-check in place, this is byte-for-byte the same validate -> insert ->
// commit sequencing this function performed before the tx-support split:
// a pure refactor for every existing caller, not a behavior change -- every
// existing CreateSession/CreateSessionCore test keeps passing unchanged.
//
// A caller that is ALREADY holding an open transaction of its own (e.g.
// one that took an atomic per-resource claim lock via SELECT ... FOR
// UPDATE before ever reaching this point) must NOT call CreateSessionCore
// -- doing so would open a SECOND, simultaneous connection out of the
// same pool while the first transaction's own connection is still held,
// risking connection-pool exhaustion/deadlock under real concurrent load.
// That caller should call CreateSessionOnTx directly, inline on its own
// already-open tx, and call TriggerDispatch itself once its own outer
// transaction has committed and hasPrompt is true.
//
// rolloutMode/repoSettings (§32) and prSessions (§31.4) are
// threaded straight through to CreateSessionOnTx below, unchanged -- see
// that function's own doc comment for why all three are required, not
// optional.
func CreateSessionCore(ctx context.Context, pool *pgxpool.Pool, sessions *postgres.SessionStore, turns *postgres.TurnStore, environments *postgres.EnvironmentStore, auditLog *postgres.AuditLogStore, registry *sessionactor.Registry, req restdtos.CreateSessionRequest, createdBy pgtype.UUID, epistemicCheckDefault bool, rolloutMode platform.RolloutMode, repoSettings *postgres.RepoSettingsStore, prSessions *postgres.GitHubPRSessionStore) (sqlcgen.Session, *CreateSessionError) {
	logger := platform.Logger(ctx)

	// Validate BEFORE ever acquiring a pooled connection -- see
	// validateCreateSessionRequest's own doc comment. A request that
	// fails this check returns its 400/500 having opened zero Postgres
	// connections/transactions, matching this function's pre-tx-support-
	// split behavior exactly.
	if _, verr := validateCreateSessionRequest(req); verr != nil {
		return sqlcgen.Session{}, verr
	}

	// §27.5's own up-front half of the "fail-closed, twice" rule
	// (§27.5/§27.6, brief point A): refused HERE, before any Postgres
	// write, when this request asks for a docker/enforced-egress
	// requirement the CONFIGURED provider does not report supporting --
	// "the clearest possible UX" per §27.5's own wording. This is
	// deliberately a SEPARATE, independent check from the one
	// sessionactor.tryPlanSpawn runs again at dispatch time (dispatch.go's
	// own doc comment) -- disabling either one alone must not disable the
	// other; see checkSubstrateCapabilitiesUpFront's own doc comment for
	// why this is where it runs (registry is the one thing this function
	// has that validateCreateSessionRequest itself does not: a live
	// ports.SandboxProvider to actually consult).
	if verr := checkSubstrateCapabilitiesUpFront(registry, req); verr != nil {
		return sqlcgen.Session{}, verr
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		logger.Error("httpapi: begin create-session tx failed", "error", err)
		return sqlcgen.Session{}, &CreateSessionError{Status: http.StatusInternalServerError, Message: "internal error"}
	}
	// Rollback is a safety net for every return path other than a
	// successful Commit below -- same pattern as internal/adapters/
	// inbound/auth's own createUserAndIdentity and app/sessionactor's
	// own transact.
	defer func() { _ = tx.Rollback(ctx) }()

	created, hasPrompt, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, createdBy, epistemicCheckDefault, rolloutMode, repoSettings, prSessions)
	if cerr != nil {
		return sqlcgen.Session{}, cerr
	}

	if err := tx.Commit(ctx); err != nil {
		logger.Error("httpapi: commit create-session tx failed", "error", err)
		return sqlcgen.Session{}, &CreateSessionError{Status: http.StatusInternalServerError, Message: "internal error"}
	}

	if hasPrompt {
		TriggerDispatch(ctx, registry, created.ID)
	}

	return created, nil
}
