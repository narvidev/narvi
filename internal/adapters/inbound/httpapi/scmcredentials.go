// This file (scmcredentials.go) implements POST /sessions/{sessionID}/
// scm-credentials (§9.3, "e2e happy path", design decision 8) -- the
// control-plane side of the wire contract internal/sandboxagent/
// credentials.CPClient (§6.4) already built and tested the CLIENT side
// of. See that package's own cpclient.go doc comment: "THE CP ENDPOINT
// THIS TALKS TO DOES NOT EXIST YET... whoever builds the real endpoint
// reconciles the two sides then." This file is that reconciliation --
// every field name/shape below matches CPClient's own
// scmCredentialsRequest/scmCredentialsResponse exactly, deliberately, not
// coincidentally.
//
// Deliberately mounted OUTSIDE auth.Middleware (§13.1's cookie-based,
// browser-user auth): this is a SANDBOX-bearer-token-authenticated
// endpoint, matching internal/adapters/inbound/wshub/sandbox.go's own
// header-bearer-token handshake precedent from §3.2 exactly, not a
// browser-facing route at all -- see cmd/control-plane/main.go's own
// mounting (outside the /api prefix, alongside the sandbox/client WS
// route, not inside the /api/sessions auth-gated group).
//
// Audit remediation (security-crosscutting & docs-completeness-vs-plan
// lenses): this Step's own original design left two gaps against §5.2
// ("scoped https+host only") and the sandbox-liveness parity
// internal/adapters/inbound/wshub/sandbox.go already enforces on the
// sibling sandbox-WS handshake:
//
//  1. Host-scoping was a no-op (`_ = req.Host`) -- the decrypted GitHub
//     OAuth token was handed back for ANY requested host, not just a host
//     the session's own repos (sessions.repos) actually use. Fixed below
//     by deriving the session's own real repo hosts (sessionRepoHosts)
//     and rejecting (403, the same generic "no usable credential" body)
//     any req.Host that matches none of them.
//  2. No dead-sandbox or gen check existed at all -- unlike wshub's own
//     handshake, a sandbox that terminalized (Stopped/Stale/Failed) kept
//     minting fresh, real credentials off its last-known-live token_hash
//     indefinitely, and no gen fencing existed on this endpoint's request
//     shape whatsoever. Fixed below by mirroring wshub's own
//     IsDeadSandboxStatus check (410, same point in the handshake: right
//     after the sandbox row lookup, before the token comparison) plus a
//     new X-Sandbox-Gen request header, compared against the sandbox
//     row's own Gen (403 on mismatch) -- see the gen-mismatch check inside
//     ScmCredentials below for why this check is deliberately built anyway
//     despite being largely redundant with token_hash's own
//     overwrite-on-respawn behavior.
//
// A later, separate audit finding (M8, cross-step): this handler is the
// EARLIER half of the same push_complete chain internal/app/sessionactor's
// own pushpr.go createPRBestEffort forms the LATER half of -- both mint/use
// the session creator's stored GitHub OAuth token, off the SAME session,
// often mere seconds apart (this endpoint hands the sandbox a credential to
// `git push` with; createPRBestEffort then opens the PR once that push
// completes). createPRBestEffort's own creatorMayGetPRAttribution
// (internal/app/sessionactor/githubtoken.go) already re-checks the
// creator's CURRENT role and disabled flag fresh, right before using their
// token, with an explicit staleness rationale: a session can outlive the
// moment its creator was disabled or demoted. This handler had NO such
// check at all -- it decrypted and handed back the creator's real token to
// any caller holding a valid sandbox bearer token, even one whose creator
// an admin had JUST disabled (e.g. offboarding, incident response) or
// demoted to viewer mid-turn. Fixed below by re-checking the creator's
// CURRENT row (Disabled, then Role) immediately after the created_by-NULL
// check, before ever looking up their identity/token -- the SAME §13.3
// viewer-guard threshold creatorMayGetPRAttribution itself uses, not a
// different one invented for this endpoint (403, the same generic body as
// every other outcome in this class -- see step 8 below).
//
// A still later audit sweep (cross-step, cross-package) found TWO MORE
// call sites inside internal/app/sessionactor itself -- contractdrift.go's
// checkContractDrift and imageresolve.go's resolveAndSetImage -- carrying
// this SAME gap, this batch never having touched either. Rather than a
// third and fourth inline copy of the identical Disabled-then-Role recheck,
// that check is now sessionactor.CheckCreatorGuard (githubtoken.go), an
// EXPORTED function all four call sites -- this handler included -- call
// directly: this package already imports internal/app/sessionactor for
// Registry/EnsureDispatched, so sharing this one further check across that
// existing dependency is a smaller, safer surface than a third bespoke
// copy would have been. This handler's own step 8 (below) now reads
// sessionactor.CheckCreatorGuard's verdict rather than re-deriving it from
// a locally fetched sqlcgen.User row -- see that function's own doc
// comment for the complete rationale, and this handler's own body for how
// its verdict maps onto THIS endpoint's own status codes (a genuine,
// unexpected GetByID failure is now a 500, matching sessions.Get/
// sandboxes.Get's own established discipline elsewhere in this same
// handler, distinct from the expected "row absent" 403 -- a nitpick this
// same audit sweep also raised: this handler's own first version of this
// check logged Warn and returned 403 unconditionally on ANY GetByID
// failure, never distinguishing the two).
//
// Audit remediation ("server-side verdict", §8.2/§5.2 confirmed
// finding): a REVIEW session (one with a github_pr_sessions row,
// reviewverdict.go's own identical reverse-lookup precedent) never pushes
// or opens a PR -- it only clones a PR's head branch read-only, for inline
// code-review context (§8.2), and its own output reaches GitHub
// exclusively through the verdict-posting tool (reviewverdict.go,
// §8.2), which authenticates with cfg.GitHubBotToken, never a per-commenter
// OAuth token. Handing such a session's SANDBOX the session CREATOR's own
// broadly `repo`-scoped personal GitHub OAuth token (steps 7-9 below) for
// this exact purpose was itself a confirmed credential-exposure gap: an
// arbitrary human whose ONLY interaction with Narvi was commenting on a PR
// had their own full, cross-repo, cross-org personal credential cached to
// the reviewing sandbox's local disk (internal/sandboxagent/credentials.
// Cache) merely because a review session happened to need SOME credential
// to clone with -- a far broader blast radius than this endpoint's own
// host-scoping (step 6) or per-session repo list ever intended to expose.
// Fixed below (the NEW step 7, checked immediately after host-scoping,
// before the creator/identity lookups steps 8-10 exist to serve): a review
// session mints botToken instead, skipping the creator-guard/identity
// path entirely -- the SAME single, statically-configured bot credential
// already trusted to read (internal/app/reviewcontext.Fetch) and post
// (githubapi.VerdictNotifier) on this exact repo/PR, never the creator's
// own identity. This does not claim to make a bash-capable review agent
// structurally incapable of ever calling GitHub's API directly with
// SOME credential (that would require OS-level process isolation between
// sandbox-agent and the agent runtime it supervises) -- it closes
// the STRICTLY WORSE half of that gap: an arbitrary commenter's own
// broad, personal, cross-repo credential never reaches a review sandbox
// at all.

package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapp"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/egressmode"
	"github.com/narvidev/narvi/internal/app/readonlymint"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/shadowledger"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/platform"
)

// scmCredentialsRequest mirrors internal/sandboxagent/credentials.
// cpclient.go's own scmCredentialsRequest exactly.
//
// ForceReadOnly is §30.4(2)'s own addition: the sandbox's own credential
// helper sets this true when cfg.BootMode == sandboxboot.BootModeBuild --
// "a build only needs read". It can only NARROW what this handler
// returns, never widen it: a true value forces the shadow-substitution
// branch below regardless of what the egress-mode resolution would have
// said on its own, but a false value never bypasses that resolution --
// the sandbox cannot request a WRITE-capable credential by omitting this
// field, only ask (honestly or not) for the strictly safer one.
type scmCredentialsRequest struct {
	Host          string `json:"host"`
	ForceReadOnly bool   `json:"forceReadOnly,omitempty"`
}

// scmCredentialsResponse mirrors internal/sandboxagent/credentials.
// cpclient.go's own scmCredentialsResponse exactly.
type scmCredentialsResponse struct {
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// verifySandboxBearerToken duplicates internal/adapters/inbound/wshub's
// own (unexported) verifySandboxToken logic for the HASH COMPARISON half
// only -- SHA-256 hash (wshub.HashSandboxToken, already exported for
// exactly this reuse) + crypto/subtle.ConstantTimeCompare, never a bare
// `==` (§3.2's own established constant-time-comparison discipline).
//
// Deliberately DOES NOT copy wshub's own nil-token_hash bypass (a sandbox
// row with no token_hash yet minted is treated there as "accept any
// non-empty presented token" -- see internal/adapters/inbound/wshub/
// doc.go's own doc comment on verifySandboxToken). That bypass's blast
// radius on the WS handshake is bounded to gating a connection; a
// successful check HERE hands back a real, decrypted GitHub OAuth access
// token -- a genuine live credential, not just a connection -- so this
// endpoint is deliberately stricter: a nil/absent token_hash is an
// immediate reject, never an implicit accept. Confirmed currently
// unreachable in production (every sandbox-row-creation path this Step's
// own code uses -- tryPlanSpawn's own UpsertSandboxForSpawn call,
// internal/app/sessionactor/dispatch.go -- always sets token_hash), but
// the elevated stakes here mean this endpoint must not inherit that
// bypass regardless of whether it is reachable today.
func verifySandboxBearerToken(presented string, storedHash *string) bool {
	if presented == "" || storedHash == nil {
		return false
	}
	got := wshub.HashSandboxToken(presented)
	return subtle.ConstantTimeCompare([]byte(got), []byte(*storedHash)) == 1
}

// ReadOnlyMinter mints a §30.4 read-only GitHub App installation token
// scoped to owner's own repoNames -- satisfied in production by
// internal/adapters/outbound/githubapp.Client (see that package's own
// doc.go for why no real GitHub App is reachable to test against), and
// fakeable here without one.
type ReadOnlyMinter interface {
	MintInstallationToken(ctx context.Context, owner string, repoNames []string) (githubapp.Token, error)
}

// ScmCredentials backs POST /sessions/{sessionID}/scm-credentials (note:
// no /api prefix -- a sandbox-to-CP endpoint, not a browser-facing REST
// route, §5.2). Outcome table (design decision 8; steps 2 and 4 below are
// this audit remediation's own additions, deliberately placed at the SAME
// points internal/adapters/inbound/wshub/sandbox.go's own sandbox-WS
// handshake places its equivalent checks -- see that file's own doc
// comment steps 7/8; step 9 below is the M8 audit finding's own addition,
// calling internal/app/sessionactor's own CheckCreatorGuard; step 7 below
// is this audit remediation's own addition):
//
//  1. sessionID does not parse as a UUID, or no sandbox row exists for it
//     -> 404 (mirrors wshub/sandbox.go's own "malformed and nonexistent
//     both mean no such session" precedent -- this caller is
//     sandbox-agent code, never a browser).
//  2. sandbox.IsDeadSandboxStatus(sandboxRow.Status) -> 410 (mirrors
//     wshub's own "session stopped" status/message convention). Checked
//     immediately after the sandbox row lookup, BEFORE the token
//     comparison and gen check below -- same ordering wshub itself uses,
//     since a terminalized sandbox's last-known-live token_hash/gen are
//     no longer meaningful to compare against at all. This closes a real,
//     previously-open window: sandboxes.token_hash is neither rotated nor
//     cleared when a sandbox terminalizes (only overwritten by a later
//     respawn's own UpsertSandboxForSpawn), so before this check a dead
//     sandbox's last-known token could mint fresh, real GitHub credentials
//     indefinitely.
//  3. The presented X-Sandbox-Gen header is missing/malformed, or parses
//     but does not equal sandboxRow.Gen -> 403 (mirrors wshub's own
//     gen-mismatch reasoning, §9.3 scenario #6). Genuinely defense-in-depth
//     rather than the primary fix for an otherwise-open hole: sandboxes.
//     token_hash is overwritten on every respawn (UpsertSandboxForSpawn,
//     internal/app/sessionactor/dispatch.go), so an old gen's sandbox
//     token already implicitly fails the token comparison in step 5 below
//     once a respawn has happened. Built anyway to match wshub's own
//     parity and this endpoint's own elevated stakes (a live external
//     credential, not just a connection) -- never assume token_hash
//     overwrite alone is sufficient for every future code path.
//  4. Authorization: Bearer <token> missing/malformed, or the presented
//     token fails verifySandboxBearerToken -> 401.
//  5. Malformed request body -> 400.
//  6. req.Host does not case-insensitively match ANY host among the
//     session's own repos (sessions.repos, via sessionRepoHosts) -> 403,
//     the SAME generic body as steps 8-10 below (§5.2: "scoped https+host
//     only" -- the decrypted token must never be handed back for a host
//     the session's own repos don't actually use, e.g. a compromised
//     in-sandbox dependency probing an arbitrary host via the
//     credential-helper protocol).
//     6.5. §30.4's own shadow substitution -- a SINGLE, server-side-only
//     interception covering BOTH of steps 7 and 8-10 below (§30.4(1): "a
//     dedicated test asserts a review session in shadow receives a
//     read-only credential" -- substituting only the creator-OAuth branch
//     would still hand every shadow REVIEW sandbox the fully write-capable
//     bot token). Resolved from req.ForceReadOnly (§30.4(2), a build boot)
//     OR ANY of the session's own repos on req.Host resolving shadow
//     (egressmode.Resolve, monotone toward suppression -- mirrors
//     postgres.OutboxStore.ResolveEffectiveMode's own identical "any
//     suppressed repo suppresses the whole" reasoning for a multi-repo
//     session). When shadow: mints a read-only GitHub App installation
//     token (internal/adapters/outbound/githubapp), validates its granted
//     permissions are read-only (§30.4(4), internal/domain/scmscope.
//     ValidateReadOnly) before ever returning it, records a refusal into
//     the ledger and 500s if that scope check fails AND the record itself
//     fails (never silently), otherwise 403s the SAME generic body as
//     every other outcome in this class -> steps 7 and 8-10 never run at
//     all. A session whose repos on req.Host span more than one distinct
//     owner cannot be served by one substituted credential (a GitHub App
//     installation is per-account) -> the SAME 403, logged separately.
//  7. This session has a github_pr_sessions row (prSessions.
//     GetBySessionID succeeds) -> 200 with botToken, never the creator's
//     own identity -- see this file's own top comment ("Audit remediation")
//     for the full rationale. steps 8-10 below (the
//     creator-guard/identity/decrypt path) are skipped entirely for a
//     review session: they exist to find and gate a PER-USER OAuth
//     credential, which a review session has no legitimate use for at
//     all. A genuine, unexpected error from GetBySessionID OTHER than
//     "no such row" (pgx.ErrNoRows) is a 500, matching this handler's own
//     established "row absent vs genuine failure" discipline (step 9's
//     identical distinction, and sessions.Get/sandboxes.Get above).
//  8. The session's own created_by is NULL -> 403, the SAME generic body
//     as steps 6/9/10 -- no bot/service-account fallback exists (§8.11),
//     nothing further to even check once a session has no creator at all.
//  9. OR the session's own created_by user is now Disabled, OR their Role
//     is now viewer -> 403, the SAME generic body as steps 6/8/10 (M8 audit
//     finding: re-checked FRESH here, right before step 10's identity/token
//     lookup even begins -- not the role/disabled state as of session
//     creation). Calls internal/app/sessionactor's own CheckCreatorGuard
//     (githubtoken.go) -- the SAME §13.3 viewer-guard threshold
//     creatorMayGetPRAttribution itself uses, via the SAME shared function
//     (not a separately-maintained copy) -- see this file's own top
//     comment for the full "why this endpoint needs the identical
//     staleness recheck the later PR-creation step of the same
//     push_complete chain already performs" rationale. A missing user row
//     (should be unreachable -- created_by is a real FK) is treated the
//     SAME as this outcome class's other sub-cases: no usable credential,
//     nothing to fail loudly over -- 403, same as Disabled/Viewer. A
//     GENUINE, unexpected failure fetching that row is instead a 500,
//     matching how sessions.Get/sandboxes.Get above already treat a real
//     DB failure (only the row's clean absence is folded into this 403
//     class, not any other lookup failure).
//  10. OR (regardless of step 9 passing): that user has NO linked
//     identities row for provider=github at all -> the SAME botToken
//     fallback step 7 already mints for a review session (review round
//     1, finding O5; §8.11: "PR created with the prompting user's OAuth
//     token, fallback: bot + manual PR URL") -- the ordinary case for a
//     creator who signed in ONLY through OIDC (§41.3) and has never
//     linked GitHub. Without this, such a creator's live session could
//     never push AT ALL (this step would 403 unconditionally, the
//     sandbox's own credential helper would fail, and no push_complete
//     would ever follow that push attempt), which would make
//     pushpr.go's own §8.11 bot-fallback PR-creation path
//     (createPRBestEffort) theoretically correct but practically
//     UNREACHABLE. botToken=="" (no bot token configured for this
//     deployment) still falls through to the SAME 403 this step always
//     returned. OR that identity's access_token_encrypted is NULL, OR
//     platform.DecryptToken fails on it -> 403, NEVER the bot fallback
//     above -- a creator who DOES have a github identity, but whose
//     stored token is merely unusable, must not be silently
//     re-attributed to the bot (that would misrepresent a token problem
//     as "no linked account" to whoever reviews the resulting push/PR);
//     see createPRBestEffort's own identical, deliberate distinction
//     (creatorHasNoGitHubIdentity). These two decrypt-failure sub-cases,
//     plus step 6's host-scoping failure, step 8's no-creator failure,
//     and step 9's disabled/demoted failure above, are deliberately
//     grouped as ONE outcome class ("no usable OAuth credential is
//     available for this session/host"). 403 (not 500): this is an
//     authorization-shaped absence from the caller's perspective, not a
//     server malfunction, and mirrors auth.Middleware's own
//     generic-rejection-body discipline (never distinguishing WHICH
//     sub-case applied, in the response body -- an enumeration-hardening
//     precedent this package already established at §13.1). §5.3's
//     gen-mismatch reuses the SAME 403 status code but is logged
//     separately server-side (see the handler body) so it stays
//     observable without adding a caller-visible distinction.
//  11. Otherwise -> 200 with scmCredentialsResponse{Username:
//     "x-access-token", Password: <decrypted token>, ExpiresAt: now +
//     timeouts.ScmCredentialTTL}.
//
// The decrypted token is NEVER logged, at any point, under any
// circumstance -- grepped for in this Step's own diff before reporting
// done, exactly like every prior security-sensitive Step's own discipline.
func ScmCredentials(
	sessions *postgres.SessionStore,
	sandboxes *postgres.SandboxStore,
	identities *postgres.IdentityStore,
	users *postgres.UserStore,
	prSessions *postgres.GitHubPRSessionStore,
	repoSettings egressmode.RepoSettingsReader,
	ledger shadowledger.Store,
	readOnlyMinter ReadOnlyMinter,
	botToken string,
	tokenEncryptionKey []byte,
	timeouts platform.Timeouts,
	platformShadow bool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		var sessionID pgtype.UUID
		if err := sessionID.Scan(chi.URLParam(r, "sessionID")); err != nil {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		ctx = platform.WithSessionID(ctx, sessionID.String())
		logger := platform.Logger(ctx)

		token, ok := bearerTokenFromHeader(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or malformed authorization header")
			return
		}

		sandboxRow, err := sandboxes.Get(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: scm-credentials: get sandbox failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Dead-sandbox check FIRST, before the gen/token comparisons below
		// -- same ordering wshub/sandbox.go's own handshake uses (see this
		// func's own doc comment step 2 for why). A Suspect sandbox is
		// deliberately NOT dead (IsDeadSandboxStatus is a deny-list, not an
		// allow-list) -- it must still be able to mint credentials.
		if sandbox.IsDeadSandboxStatus(sandbox.State(sandboxRow.Status)) {
			writeError(w, http.StatusGone, "session stopped")
			return
		}

		// Gen check (this func's own doc comment step 3): defense-in-depth
		// alongside the token comparison below, not the primary fix for a
		// distinct hole -- see that doc comment for the full reasoning.
		// Missing/malformed gen is treated identically to a mismatch (403,
		// the same generic body every other sub-case in this class uses):
		// a well-formed caller (CPClient.Fetch, updated by this same audit
		// remediation to always send it) always presents one, so there is
		// no legitimate case to distinguish here.
		presentedGen, genErr := strconv.Atoi(r.Header.Get("X-Sandbox-Gen"))
		if genErr != nil || presentedGen != int(sandboxRow.Gen) {
			logger.Warn("httpapi: scm-credentials: rejecting: gen mismatch",
				"presented_gen_header", r.Header.Get("X-Sandbox-Gen"), "sandbox_gen", sandboxRow.Gen)
			writeError(w, http.StatusForbidden, "no usable git credential for this session")
			return
		}

		if !verifySandboxBearerToken(token, sandboxRow.TokenHash) {
			writeError(w, http.StatusUnauthorized, "invalid sandbox token")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req scmCredentialsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}

		sessionRow, err := sessions.Get(ctx, sessionID)
		if err != nil {
			logger.Error("httpapi: scm-credentials: get session failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Host-scoping check (this func's own doc comment step 6, §5.2
		// "scoped https+host only"): req.Host must match at least one of
		// the session's own real repo hosts before ANY credential is ever
		// minted for it.
		hasHost, err := sessionHasRepoHost(sessionRow.Repos, req.Host)
		if err != nil {
			logger.Error("httpapi: scm-credentials: parse session repos failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !hasHost {
			logger.Warn("httpapi: scm-credentials: rejecting: requested host not among session's own repo hosts",
				"requested_host", req.Host)
			writeError(w, http.StatusForbidden, "no usable git credential for this session")
			return
		}

		// §30.4's own shadow substitution (this func's own doc comment
		// step 6.5): a SINGLE interception, server-side only, covering
		// BOTH the review-bot-token branch (step 7) and the creator-OAuth
		// branch (steps 8-10) below. hostRepoFullNames is every session
		// repo on req.Host regardless of owner (used for the egress-mode
		// resolution, which must see all of them -- §30.8's own "any
		// suppressed repo suppresses the whole" rule); hostReposByOwner
		// groups the SAME repos by owner, since a GitHub App installation
		// token covers exactly one account and MintInstallationToken can
		// only be scoped to one.
		hostRepoFullNames, hostReposByOwner := shadowHostRepos(sessionRow.Repos, req.Host)

		// Two parsers read the same repos, and they do not agree. The
		// authorization gate above accepts any URL with a host;
		// shadowHostRepos additionally drops anything that is not
		// owner/repo shaped. A session repo like "https://github.com/acme"
		// -- which session creation accepts, since it validates only
		// scheme and host -- therefore passes the gate and contributes
		// NOTHING to the set resolved below.
		//
		// Left as a bare loop over that set, an empty set meant the loop
		// never ran, shadow stayed false, and the handler fell through to
		// the write-capable branches. Worse, platformShadow is only read
		// INSIDE the loop, so a dedicated evaluation deployment with the
		// master switch on would have handed out a write-capable token
		// too -- a fail-open in the one direction §30.8 forbids, and the
		// deployment-wide switch bypassed by a malformed URL.
		//
		// So the repo-less case resolves on the switch alone, through the
		// resolver's own ResolvePlatform, which exists for exactly this:
		// an artifact with no single customer repository to check the
		// per-repo flag against. And an unparseable repo is not treated as
		// absent -- see hostReposUnparseable below.
		shadow := req.ForceReadOnly
		if !shadow {
			if len(hostRepoFullNames) == 0 {
				shadow = egressmode.ResolvePlatform(platformShadow).Suppressed()
			}
			for _, repoFullName := range hostRepoFullNames {
				if egressmode.Resolve(ctx, egressmode.Deps{
					RepoSettings:   repoSettings,
					PlatformShadow: platformShadow,
				}, repoFullName).Suppressed() {
					shadow = true
					break
				}
			}
		}
		// A repo this code could not parse is a repo whose egress mode
		// nobody established. It resolves shadow rather than being
		// skipped: the alternative is that a URL shape decides whether a
		// credential can write.
		if !shadow && hostReposUnparseable(sessionRow.Repos, req.Host) {
			logger.Warn("httpapi: scm-credentials: a repo on this host could not be read as owner/repo; forcing shadow for this request",
				"requested_host", req.Host)
			shadow = true
		}

		if shadow {
			// Both arms below refuse, and both are correct: no
			// credential at all is strictly safer than a write-capable
			// one. They are separated only so each log line states the
			// reason that actually applies -- "spans more than one
			// owner" is false when the count is zero.
			switch {
			case len(hostReposByOwner) == 0:
				logger.Warn("httpapi: scm-credentials: rejecting: no repo on this host could be read as owner/repo, so no read-only credential can be scoped; serving none",
					"requested_host", req.Host)
				writeError(w, http.StatusForbidden, "no usable git credential for this session")
				return
			case len(hostReposByOwner) > 1:
				logger.Warn("httpapi: scm-credentials: rejecting: session's repos on this host span more than one owner; a single substituted read-only credential cannot cover them",
					"requested_host", req.Host, "distinct_owners", len(hostReposByOwner))
				writeError(w, http.StatusForbidden, "no usable git credential for this session")
				return
			}
			var owner string
			var repoNames []string
			for o, names := range hostReposByOwner {
				owner, repoNames = o, names
			}

			// One implementation of "mint, then refuse to serve what
			// came back over-scoped" -- internal/app/readonlymint.Mint,
			// which also owns §30.4(4)'s record-or-fail on a refusal.
			// This handler previously inlined a second copy of that
			// sequence; two copies of a security rule is one that can
			// silently drift out from under the other's tests.
			mintCtx, cancel := context.WithTimeout(ctx, timeouts.GitHubAppMintTimeout)
			token, mintErr := readonlymint.Mint(mintCtx, readOnlyMinter, ledger, owner, repoNames, hostRepoFullNames[0], req.Host, sessionID)
			cancel()
			var refused *readonlymint.ErrRefusedByScopeCheck
			switch {
			case errors.As(mintErr, &refused):
				// Refused AND recorded. 403: no credential exists to
				// serve, and the refusal is already durable evidence.
				logger.Warn("httpapi: scm-credentials: refusing: minted installation token failed the read-only scope check",
					"error", mintErr, "requested_host", req.Host)
				writeError(w, http.StatusForbidden, "no usable git credential for this session")
				return
			case mintErr != nil:
				// Either the mint itself failed, or a refusal could not
				// be recorded. Both are 500: "suppressed but unrecorded"
				// is exactly the contract violation the ledger exists to
				// prevent, so it must never present as a quiet 403.
				logger.Error("httpapi: scm-credentials: read-only installation token unavailable", "error", mintErr)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}

			writeJSON(w, http.StatusOK, scmCredentialsResponse{
				Username:  "x-access-token",
				Password:  token.Value,
				ExpiresAt: time.Now().Add(timeouts.ScmCredentialTTL),
			})
			return
		}

		// Review-session check (this func's own doc comment step 7,
		// audit remediation): a session with a github_pr_sessions row
		// never pushes/opens a PR -- see this file's own top comment for
		// the full "why the creator's own personal OAuth token has no
		// legitimate use here" rationale. Mints botToken directly,
		// skipping steps 8-10's creator-guard/identity/decrypt path
		// entirely -- that path exists to find a PER-USER credential,
		// which is exactly what this branch avoids handing to a review
		// sandbox at all.
		if _, err := prSessions.GetBySessionID(ctx, sessionID); err == nil {
			writeJSON(w, http.StatusOK, scmCredentialsResponse{
				Username:  "x-access-token",
				Password:  botToken,
				ExpiresAt: time.Now().Add(timeouts.ScmCredentialTTL),
			})
			return
		} else if !errors.Is(err, pgx.ErrNoRows) {
			logger.Error("httpapi: scm-credentials: get github pr session failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if !sessionRow.CreatedBy.Valid {
			logger.Warn("httpapi: scm-credentials: session has no created_by user; no bot fallback exists (§8.11)")
			writeError(w, http.StatusForbidden, "no usable git credential for this session")
			return
		}

		// Creator disabled/role recheck (this func's own doc comment step
		// 8, M8 audit finding): re-read the creator's CURRENT row fresh,
		// right here, before ever looking up their identity/token below --
		// calls internal/app/sessionactor's own CheckCreatorGuard
		// (githubtoken.go), the SAME shared function creatorMayGetPRAttribution
		// itself now calls (same Disabled-then-Role order, same §13.3
		// viewer-guard threshold), which already gates the LATER
		// PR-creation step of this SAME push_complete chain. A session can
		// outlive the moment its creator was disabled or demoted -- that
		// function's own doc comment's full rationale applies here
		// identically, since this endpoint mints the very credential that
		// later push uses.
		//
		// A genuine, unexpected GetByID failure (Err set, ErrNotFound
		// false) is a 500 -- matching sessions.Get/sandboxes.Get's own
		// established discipline above, and this SAME handler's own
		// identities.GetByUserAndProvider handling five lines below --
		// distinct from the expected "row absent" case (Err set,
		// ErrNotFound true), which denies exactly like Disabled/Viewer:
		// this endpoint's own first version of this check logged Warn and
		// returned 403 unconditionally for ANY GetByID failure, never
		// distinguishing the two (a nitpick this same audit sweep raised).
		guard := sessionactor.CheckCreatorGuard(ctx, users, sessionRow.CreatedBy)
		if guard.Err != nil {
			if !guard.ErrNotFound {
				logger.Error("httpapi: scm-credentials: get session creator for disabled/role recheck failed",
					"error", guard.Err, "user_id", sessionRow.CreatedBy.String())
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			logger.Warn("httpapi: scm-credentials: session creator row not found for disabled/role recheck; denying",
				"user_id", sessionRow.CreatedBy.String())
			writeError(w, http.StatusForbidden, "no usable git credential for this session")
			return
		}
		if guard.Disabled {
			logger.Warn("httpapi: scm-credentials: session creator is now disabled; refusing credential (audit M8, §13.3 viewer guard parity)",
				"user_id", sessionRow.CreatedBy.String())
			writeError(w, http.StatusForbidden, "no usable git credential for this session")
			return
		}
		if guard.Viewer {
			logger.Warn("httpapi: scm-credentials: session creator is now a viewer; refusing credential (audit M8, §13.3 viewer guard parity)",
				"user_id", sessionRow.CreatedBy.String())
			writeError(w, http.StatusForbidden, "no usable git credential for this session")
			return
		}

		identity, err := identities.GetByUserAndProvider(ctx, sessionRow.CreatedBy, sqlcgen.IdentityProviderGithub)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				logger.Error("httpapi: scm-credentials: get identity failed", "error", err)
				writeError(w, http.StatusForbidden, "no usable git credential for this session")
				return
			}
			// No github identity AT ALL -- the ordinary case for a
			// creator who signed in ONLY through OIDC (§41.3) and has
			// never linked a GitHub identity (review round 1, finding
			// O5). Without this fallback, such a creator's live session
			// could never push at all (this endpoint would 403, the
			// sandbox's own credential helper would fail, and no
			// push_complete would ever follow) -- so pushpr.go's own
			// §8.11 bot-fallback PR-creation path (createPRBestEffort)
			// could never even be REACHED, only theoretically correct.
			// Mints the SAME static bot credential the review-session
			// branch above already uses, mirroring
			// createSentinelFixPRBestEffort's own identical
			// no-human-creator bot-attributed push+PR precedent
			// (internal/app/sessionactor/pushpr.go).
			//
			// A creator who DOES have a github identity, but whose
			// stored token is merely unusable (nil, or fails to
			// decrypt, below), is a DIFFERENT case and is NEVER given
			// this fallback -- see createPRBestEffort's own identical,
			// deliberate distinction (creatorHasNoGitHubIdentity) for
			// why: re-attributing a push to the bot here would
			// misrepresent an existing account's broken credential as
			// "no linked account" to whoever reviews the resulting PR.
			if botToken == "" {
				logger.Warn("httpapi: scm-credentials: user has no github identity and no bot token is configured")
				writeError(w, http.StatusForbidden, "no usable git credential for this session")
				return
			}
			logger.Info("httpapi: scm-credentials: user has no github identity; falling back to bot credential for push (§8.11)")
			writeJSON(w, http.StatusOK, scmCredentialsResponse{
				Username:  "x-access-token",
				Password:  botToken,
				ExpiresAt: time.Now().Add(timeouts.ScmCredentialTTL),
			})
			return
		}

		if identity.AccessTokenEncrypted == nil {
			logger.Warn("httpapi: scm-credentials: github identity has no stored access token")
			writeError(w, http.StatusForbidden, "no usable git credential for this session")
			return
		}

		plaintext, err := platform.DecryptToken(tokenEncryptionKey, identity.AccessTokenEncrypted)
		if err != nil {
			// The decrypt error itself is safe to log (it never contains
			// the ciphertext or plaintext -- see platform.DecryptToken's
			// own doc comment), but the plaintext token it would have
			// produced is NEVER logged, here or anywhere else.
			logger.Error("httpapi: scm-credentials: decrypt access token failed", "error", err)
			writeError(w, http.StatusForbidden, "no usable git credential for this session")
			return
		}

		writeJSON(w, http.StatusOK, scmCredentialsResponse{
			Username:  "x-access-token",
			Password:  string(plaintext),
			ExpiresAt: time.Now().Add(timeouts.ScmCredentialTTL),
		})
	}
}

// bearerTokenFromHeader extracts the bearer token from r's Authorization
// header -- mirrors internal/adapters/inbound/wshub/sandbox.go's own
// bearerToken function exactly (that one is unexported to its own
// package; duplicated here rather than exported cross-package for such a
// tiny, dependency-free helper).
func bearerTokenFromHeader(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(h, prefix)
	if token == "" {
		return "", false
	}
	return token, true
}

// sessionRepoHosts unmarshals rawRepos (sessions.repos' own raw JSONB
// bytes, sqlcgen.Session.Repos) and returns the lowercased host of each
// repo whose Url parses successfully -- audit remediation (security-
// crosscutting & docs-completeness-vs-plan lenses), design decision 1.
//
// This is a local, httpapi-package equivalent of internal/app/sessionactor/
// sessionconfig.go's own (unexported, cross-package-unreachable)
// reposFromJSON: same JSON shape ({branch, name, url}), same generated
// wire TYPE (sessionconfig.SessionConfigReposElem, reused for the type
// only), but its own separate function -- httpapi is a different, adjacent
// inbound layer, and importing sessionactor's unexported helper isn't
// possible; exporting it purely for this one cross-layer reuse would risk
// an import-cycle/layering violation this batch's own design explicitly
// avoids.
//
// A repo whose Url fails to parse, or parses with an empty Host, is
// skipped rather than erroring the whole request: sessions.repos is
// already-trusted, already-persisted data by the time this endpoint runs
// (§9.3's CreateSession already accepted/persisted it) -- this is HOST
// COMPARISON against an already-trusted list, not input validation of a
// new untrusted value, so one defensively-malformed entry should not fail
// credential minting for every OTHER, well-formed repo host the session
// does have.
func sessionRepoHosts(rawRepos []byte) ([]string, error) {
	if len(rawRepos) == 0 {
		return nil, nil
	}
	var repos []sessionconfig.SessionConfigReposElem
	if err := json.Unmarshal(rawRepos, &repos); err != nil {
		return nil, fmt.Errorf("httpapi: unmarshal session repos: %w", err)
	}
	hosts := make([]string, 0, len(repos))
	for _, repo := range repos {
		parsed, err := url.Parse(repo.Url)
		if err != nil || parsed.Host == "" {
			continue
		}
		hosts = append(hosts, strings.ToLower(parsed.Host))
	}
	return hosts, nil
}

// shadowHostRepos parses rawRepos (sessions.repos' own raw JSONB bytes)
// and returns every repo whose clone URL's host case-insensitively
// matches host: repoFullNames is "owner/repo" for each -- every one of
// them, regardless of owner, since §30.4's own shadow-mode resolution
// below must see all of them (§30.8's "any suppressed repo suppresses the
// whole" rule, mirroring postgres.OutboxStore's own identical
// sessionRepoFullNames/ResolveEffectiveMode reasoning). byOwner groups
// the SAME repos' bare names by owner, since MintInstallationToken can
// only ever be scoped to one account's own installation -- a session
// whose matched repos span more than one owner therefore has
// len(byOwner) > 1, which this func's one caller (ScmCredentials) treats
// as "cannot be served by a single substituted credential" and refuses.
//
// A repo whose URL fails to parse, or whose host doesn't match, is
// skipped rather than erroring the whole request -- mirrors
// sessionRepoHosts' own identical "already-trusted, already-persisted
// data; one malformed entry should not fail credential minting for every
// other, well-formed repo" reasoning immediately below.
func shadowHostRepos(rawRepos []byte, host string) (repoFullNames []string, byOwner map[string][]string) {
	if len(rawRepos) == 0 {
		return nil, nil
	}
	var repos []sessionconfig.SessionConfigReposElem
	if err := json.Unmarshal(rawRepos, &repos); err != nil {
		return nil, nil
	}
	host = strings.ToLower(host)
	byOwner = make(map[string][]string)
	for _, repo := range repos {
		parsed, err := url.Parse(repo.Url)
		if err != nil || parsed.Host == "" || strings.ToLower(parsed.Host) != host {
			continue
		}
		owner, name, err := reposource.ParseOwnerRepo(repo.Url)
		if err != nil {
			continue
		}
		repoFullNames = append(repoFullNames, owner+"/"+name)
		byOwner[owner] = append(byOwner[owner], name)
	}
	return repoFullNames, byOwner
}

// sessionHasRepoHost reports whether host case-insensitively matches at
// least one of the hosts sessionRepoHosts derives from rawRepos -- ordinary
// HTTP host-header comparison convention (lowercase both sides), per this
// batch's own design decision 1.
func sessionHasRepoHost(rawRepos []byte, host string) (bool, error) {
	hosts, err := sessionRepoHosts(rawRepos)
	if err != nil {
		return false, err
	}
	host = strings.ToLower(host)
	for _, h := range hosts {
		if h == host {
			return true, nil
		}
	}
	return false, nil
}

// hostReposUnparseable reports whether any repo on host is one this code
// could not read as owner/repo.
//
// It exists because shadowHostRepos SKIPS such a repo, and a skip is
// silent: the caller sees a shorter list, not a problem. That is the right
// shape for building a substitution set -- you cannot substitute for a
// repository you cannot name -- and the wrong shape for deciding whether
// to substitute at all, where a skipped repo would quietly mean "nothing
// to suppress here".
//
// So the two questions get two functions. This one answers "was anything
// dropped", and its caller resolves shadow when the answer is yes.
func hostReposUnparseable(rawRepos []byte, host string) bool {
	if len(rawRepos) == 0 {
		return false
	}
	var repos []sessionconfig.SessionConfigReposElem
	if err := json.Unmarshal(rawRepos, &repos); err != nil {
		// Unreadable repos JSON is itself an unestablished egress mode.
		return true
	}
	host = strings.ToLower(host)
	for _, repo := range repos {
		parsed, err := url.Parse(repo.Url)
		if err != nil || parsed.Host == "" || strings.ToLower(parsed.Host) != host {
			continue
		}
		if _, _, err := reposource.ParseOwnerRepo(repo.Url); err != nil {
			return true
		}
	}
	return false
}
