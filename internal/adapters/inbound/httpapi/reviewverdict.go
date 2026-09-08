// This file (reviewverdict.go) implements §8.2's ("server-side
// verdict", §8.2/§5.2/§21.2) own VERDICT-POSTING TOOL: POST
// /sessions/{sessionID}/review/verdict. This is the ONLY sanctioned way a
// review session's output reaches its pull request as a comment or formal
// review -- internal/app/sessionactor/outboxenqueue.go's own github-origin
// branch (§8.2's own RAW-COMMENT BLOCKING) no longer enqueues anything
// on its own, so a review session that never calls this endpoint simply
// posts nothing.
//
// # Why an HTTP endpoint, not a genuine OpenCode/LLM function-call tool
//
// The review agent is expected to invoke this the SAME way sandbox-agent's
// own git-credential-helper POSTs to CP's /sessions/{id}/scm-credentials
// (internal/adapters/inbound/httpapi/scmcredentials.go's own doc comment):
// an authenticated HTTP call, from inside the sandbox, using the SAME
// sandbox bearer token/gen-fencing scheme that endpoint and snapshot-mint
// (snapshotmint.go) already establish for "agent-initiated, server-
// validated" actions. Nothing in this codebase's AgentRuntime port (§4.2)
// or sandbox WS protocol (§6.1) defines a native LLM function-calling
// mechanism today (§7's own "no port change" precedent for adapter-local
// features), and inventing one from scratch -- registering a JSON-schema
// tool with OpenCode's own real tool/MCP surface, translating a NEW kind
// of tool_call/tool_result pair over the wire -- is real, novel scope this
// Step's own brief does not ask for and the existing wire contracts do not
// anticipate. Reusing the established sandbox-bearer-authenticated-
// endpoint pattern gives "the agent calls it with typed fields, validated
// server-side" (this Step's own central requirement) without touching the
// sandbox WS protocol or the OpenCode adapter at all -- the review turn's
// own prompt (review.RenderTurnPrompt, §8.2) is the natural place to
// instruct the agent HOW to call this endpoint (URL, bearer token, gen
// header, JSON shape), exactly like it already instructs the agent about
// pre-fetched diff/stack context. A genuine native tool-call integration
// remains a plausible LATER refinement once OpenCode's own real custom-
// tool/MCP configuration surface is confirmed against the pinned binary
// (mirroring §7's own contract-test discipline) -- not invented here on
// spec.
//
// # Server-computed Shippable -- the single most important invariant
//
// This endpoint NEVER accepts Shippable from the caller (restdtos.
// PostReviewVerdictRequest has no such field at all) -- it always
// recomputes it via review.ComputeShippable, through reviewpost.
// BuildVerdict, exactly matching review.Verdict's own CONTRACT. Nothing
// here, or anywhere downstream, ever parses a POSTED comment/review back
// into a Verdict -- the tool's payload (this request body) IS the
// structured verdict, and this handler's own job is validating it,
// authoritatively computing Shippable/the formal-review event/the synced
// label, and enqueueing exactly one outbox row for delivery -- the real
// GitHub network calls (formal review + label sync) happen entirely at
// delivery time, inside internal/adapters/outbound/githubapi.
// VerdictNotifier, never synchronously in this request path.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/findingposition"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/knowledge"
	"github.com/narvidev/narvi/internal/domain/provenance"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/platform"
)

// PostReviewVerdict backs POST /sessions/{sessionID}/review/verdict (note:
// no /api prefix -- a sandbox-to-CP endpoint, not a browser-facing REST
// route, mirroring scm-credentials/snapshot-mint exactly). Outcome table:
//
//  1. sessionID does not parse as a UUID, or no sandbox row exists for it
//     -> 404 (mirrors scmcredentials.go/snapshotmint.go's own identical
//     "malformed and nonexistent both mean no such session" precedent --
//     this caller is sandbox-agent/agent code, never a browser).
//  2. Authorization: Bearer <token> missing/malformed -> 401.
//  3. sandbox.IsDeadSandboxStatus(sandboxRow.Status) -> 410 (same ordering
//     as scmcredentials.go/snapshotmint.go: checked before the gen/token
//     comparisons, since a terminalized sandbox's last-known-live
//     token_hash/gen are no longer meaningful to compare against).
//  4. The presented X-Sandbox-Gen header is missing/malformed, or parses
//     but does not equal sandboxRow.Gen -> 403 (§9.3 scenario #6 parity).
//  5. The presented bearer token fails verifySandboxBearerToken -> 401.
//  6. This session has no github_pr_sessions row at all (pgx.ErrNoRows) --
//     meaningless call, this session is not a review session -> 400
//     (mirrors reviewretrigger.go's own identical "no PR to act on" 400).
//  7. Malformed request body (fails to decode as restdtos.
//     PostReviewVerdictRequest -- the generated type's own UnmarshalJSON
//     already rejects a missing required field, an out-of-enum value, a
//     negative filesChanged, or an empty summary) -> 400.
//  8. The decoded, syntactically-valid body still fails
//     reviewpost.ValidateVerdictInput (today: only a whitespace-only
//     summary reaches this far -- every other check above already caught
//     it at JSON-decode time; kept as real defense in depth, not dead
//     code -- one of several application-layer checks the schema alone
//     cannot close) -> 400.
//  9. Otherwise -> 201 with restdtos.PostReviewVerdictResponse, having
//     enqueued exactly one ports.NotificationKindGitHubVerdict outbox row
//     (internal/adapters/outbound/githubapi.VerdictNotifier delivers it:
//     the formal review, then the label sync).
//
// §8.2 ("sentinels + suggestions", §17/§22.1) extends this handler,
// never replaces it: after building the verdict, it ALSO builds
// []reviewpost.Finding from req.Findings (optional, additive -- see
// restdtos.PostReviewVerdictRequest's own doc comment), computes each
// finding's identity server-side, and upserts review_findings -- all
// inside the SAME transaction as the outbox write below (pool.Begin ...
// tx.Commit), never a second, independently-committed write. When a
// posted finding names a sentinel kind (coverage/docs_drift) AND the
// repo's own sentinel_autofix_enabled toggle is on AND this session is
// NOT itself a sentinel-auto-fix child session (§17.1's "no recursion"
// rule), a SECOND outbox row (ports.NotificationKindSentinelAutoFix) is
// enqueued in the SAME transaction, claiming (or reusing an
// already-claimed) sentinel_fixes row for this PR.
func PostReviewVerdict(
	pool *pgxpool.Pool,
	sandboxes *postgres.SandboxStore,
	sessions *postgres.SessionStore,
	prSessions *postgres.GitHubPRSessionStore,
	repoSettings *postgres.RepoSettingsStore,
	reviewFindings *postgres.ReviewFindingStore,
	sentinelFixes *postgres.SentinelFixStore,
	outbox *postgres.OutboxStore,
	reviewVerdicts *postgres.ReviewVerdictStore,
	turns *postgres.TurnStore,
	// events (§26.4/§7.1) backs post-hoc sub-task corroboration:
	// reading back this session's own already-persisted sub_task_start/
	// sub_task_finish trace, scoped to BOTH turns' own
	// dispatched_sandbox_gen AND dispatched_at -- see corroborateCounterReview's
	// own doc comment below for the full call-site wiring and why both are
	// required together.
	events *postgres.EventStore,
	botHandle string,
	// botToken (§22.1.1) is the SAME GitHub bot credential
	// platform.Config.GitHubBotToken already supplies to every other
	// diff-fetching call site (internal/app/reviewcontext.Fetch's own
	// callers, handler.go/reviewretrigger.go) -- distinct from botHandle
	// above (a plain username string, never a credential): this handler
	// needs a REAL token to authenticate FetchDiffAt's own GitHub API
	// calls, which botHandle alone cannot provide.
	botToken string,
	// diffFetcher/positionResolver/timeouts (§22.1.1) back this
	// handler's OWN content-anchored positioning: diffFetcher re-fetches
	// (internal/app/reviewcontext.FetchDiffAt) the SAME diff a review
	// turn's own prompt was anchored to, pinned to the resolved
	// verdictHeadSHA; positionResolver is the non-agentic §4.3 LLM-port
	// relocation fallback (internal/app/findingposition) consulted only
	// when reviewpost.MatchPosition's own pure sliding-window match
	// fails. Both are nil-safe: diffFetcher == nil (this package's own
	// *_test.go minimal wiring) simply skips the whole positioning step,
	// leaving every finding at its own honest StartLine=0/EndLine=0
	// default; positionResolver == nil (no usable LLM credential
	// configured) skips only the relocation fallback specifically --
	// findingposition.ResolveAll's own doc comment covers the exact
	// degradation either way.
	diffFetcher reviewcontext.Fetcher,
	positionResolver *findingposition.Resolver,
	timeouts platform.Timeouts,
	// platformShadow (§30.8) is platform.Config.ShadowMode
	// (NARVI_SHADOW_MODE) -- threaded through to appreviewverdict.
	// Insert's own egressmode.Resolve call below, exactly like
	// cmd/control-plane/main.go's own outboxStore/registry construction
	// already receives it.
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
			logger.Error("httpapi: review-verdict: get sandbox failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if sandbox.IsDeadSandboxStatus(sandbox.State(sandboxRow.Status)) {
			writeError(w, http.StatusGone, "session stopped")
			return
		}

		presentedGen, genErr := strconv.Atoi(r.Header.Get("X-Sandbox-Gen"))
		if genErr != nil || presentedGen != int(sandboxRow.Gen) {
			logger.Warn("httpapi: review-verdict: rejecting: gen mismatch",
				"presented_gen_header", r.Header.Get("X-Sandbox-Gen"), "sandbox_gen", sandboxRow.Gen)
			writeError(w, http.StatusForbidden, "no usable verdict-posting credential for this session")
			return
		}

		if !verifySandboxBearerToken(token, sandboxRow.TokenHash) {
			writeError(w, http.StatusUnauthorized, "invalid sandbox token")
			return
		}

		// This action is meaningful ONLY for a session that IS a GitHub PR
		// review session -- mirrors reviewretrigger.go's own identical
		// reverse lookup/400 precedent.
		prSession, err := prSessions.GetBySessionID(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusBadRequest, "this session has no associated GitHub pull request to post a verdict on")
				return
			}
			logger.Error("httpapi: review-verdict: get github pr session failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		owner, repo, ok := reposource.SplitFullName(prSession.RepoFullName)
		if !ok {
			logger.Error("httpapi: review-verdict: repo_full_name not in owner/repo shape", "repo_full_name", prSession.RepoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.PostReviewVerdictRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
			return
		}

		// resolve the head SHA
		// THIS session's own CURRENTLY-PROCESSING turn was anchored to --
		// never prSession.PendingHeadSha (github_pr_sessions' own shared,
		// mutable per-(repo,PR) column, REMOVED by this fix; see
		// migrations/000072_turns_review_head_sha.up.sql's own doc
		// comment for the full "why" that design let a LATER, unrelated
		// turn's own context-fetch silently overwrite the value THIS
		// verdict eventually forwards). The review agent calling THIS
		// endpoint is, by construction, the one whose own turn is right
		// now 'processing' for sessionID -- turns_one_processing_per_session
		// (migrations/000005_turns.up.sql) guarantees at most one such
		// row can ever exist.
		//
		// Moved here (§26.3 -- one step EARLIER than the
		// previous "before §22.1.1's own position-resolution step"
		// position this fetch previously held) so reviewDepth, below, is
		// available BEFORE ValidateVerdictInput runs: the deep-path
		// digest-completeness check (validate.go, this Step's own
		// addition) needs it. verdictHeadSHA's own downstream uses
		// (position resolution, the review_verdicts insert) are
		// unaffected -- it is computed once, here, and simply read
		// later, exactly as before.
		//
		// Both a genuine store error AND a not-found (ErrNoRows -- a
		// genuine race: the turn already completed/failed/was cancelled
		// between this agent's own HTTP call landing and this read)
		// degrade IDENTICALLY: logged, verdictHeadSHA/reviewDepth both
		// stay at their own zero value, and (per the existing skip branch
		// further down, unchanged by this fix) the review_verdicts
		// insert is skipped -- never a reason to fail this whole tool
		// call.
		// serverComputedChangedFiles (§21.1's own filesChanged drift
		// canary) is this SAME processing turn's own reviewtriage.
		// DecisionRecord.ChangedFilesCount, unmarshaled from
		// review_depth_decision below -- the server-computed count from
		// THIS turn's own context fetch, never re-derived here. Stays 0
		// (that field's own zero value, and DecisionRecord's own doc
		// comment: "indistinguishable from... genuinely empty diff") for
		// every case that skips or fails the unmarshal below -- no
		// processing turn found, a turn that predates this field, or a
		// turn whose own review_depth_decision marshal failed at
		// creation time -- reviewverdict.FilesChangedDrifted's own doc
		// comment covers why treating that identically to "no reliable
		// signal, never fire" is required, not merely convenient.
		//
		// diffDelivered (D4, adversarial review of PR #182, MEDIUM) is
		// this SAME processing turn's own reviewtriage.DecisionRecord.
		// DiffEmpty/DiffTruncated, collapsed into the ONE fact
		// FilesChangedDrifted's own diffDelivered parameter needs: "was
		// the reviewing agent actually handed a full diff to read at
		// all." Deliberately initialized to false (never delivered) here
		// -- NOT computed as "!decisionRecord.DiffEmpty &&
		// !decisionRecord.DiffTruncated" against a decisionRecord that
		// might itself be an unpopulated zero value: DiffEmpty/
		// DiffTruncated both false is ALSO decisionRecord's own zero
		// value (no processing turn found, a turn that predates this
		// field, or a failed unmarshal, exactly the same three cases
		// serverComputedChangedFiles' own comment names), and reading
		// that as "confirmed delivered" would be exactly the unsafe
		// misreading D1's own "authoritative-or-absent, never partial"
		// principle forbids elsewhere in this same PR. Set true ONLY
		// inside the successful-unmarshal branch below, from the real
		// decisionRecord.
		var verdictHeadSHA string
		var reviewDepth reviewtriage.ReviewDepth
		var serverComputedChangedFiles int
		var diffDelivered bool
		var archDecisionTags, archDecisionRoots []string
		// knowledgeMode/knowledgeInfluenced (§31.2/§31.7's own mode
		// buffer and G5) mirror archDecisionTags/archDecisionRoots' own
		// identical "read back from where the dispatching path stamped
		// it" shape, immediately above: knowledgeMode is this SAME
		// processing turn's own turns.review_knowledge_mode, forwarded
		// verbatim; knowledgeInfluenced is derived, not forwarded --
		// "this turn's prompt actually carried a prior-decisions block"
		// iff its own turns.review_knowledge_decision record is
		// non-empty. Both stay at their zero value (empty mode, not
		// influenced) for every case that skips or fails the turn lookup
		// below -- the safe direction: an un-attributable verdict is
		// never wrongly stamped as knowledge-influenced.
		var knowledgeMode string
		var knowledgeInfluenced bool
		// dispatchedSandboxGen/dispatchedEventID (§26.4/§7.1) are
		// this SAME processing turn's own turns.dispatched_sandbox_gen/
		// dispatched_event_id -- the sandbox gen this turn's prompt was
		// actually dispatched to (migrations/000026_turn_dispatch_gen.up.sql)
		// and the events-log high-water mark at the instant it was
		// dispatched (migrations/000089_turns_dispatched_event_id.up.sql).
		// Both stay nil for every case that skips or fails the turn lookup
		// below, exactly like verdictHeadSHA/reviewDepth's own identical
		// degradation -- corroborateCounterReview's own call site, further
		// down, treats either one being unset as NOT corroborated rather
		// than erroring or skipping the check (fail-conservative, see that
		// call site's own doc comment). BOTH are required together, not
		// merely gen alone: see events.sql's own doc comment on
		// ListSubTaskStart/FinishEventsForTurn for why gen-scoping alone was
		// found to be insufficient (an adversarial review finding on this
		// same PR) and why the id lower bound is what actually closes that
		// gap -- and why it is an events.id and not a timestamp.
		var dispatchedSandboxGen *int32
		var dispatchedEventID *int64
		if processingTurn, turnErr := turns.GetProcessingTurnForSession(ctx, sessionID); turnErr != nil {
			if errors.Is(turnErr, pgx.ErrNoRows) {
				logger.Warn("httpapi: review-verdict: no processing turn found for session, skipping review_verdicts insert", "repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
			} else {
				logger.Error("httpapi: review-verdict: get processing turn for session failed, skipping review_verdicts insert", "error", turnErr, "repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
			}
		} else {
			if processingTurn.ReviewHeadSha != nil {
				verdictHeadSHA = *processingTurn.ReviewHeadSha
			}
			if processingTurn.ReviewDepth != nil {
				reviewDepth = reviewtriage.ReviewDepth(*processingTurn.ReviewDepth)
			}
			dispatchedSandboxGen = processingTurn.DispatchedSandboxGen
			dispatchedEventID = processingTurn.DispatchedEventID
			if processingTurn.ReviewKnowledgeMode != nil {
				knowledgeMode = *processingTurn.ReviewKnowledgeMode
			}
			if len(processingTurn.ReviewKnowledgeDecision) > 0 {
				var injected knowledge.InjectedRecord
				if unmarshalErr := json.Unmarshal(processingTurn.ReviewKnowledgeDecision, &injected); unmarshalErr != nil {
					logger.Warn("httpapi: review-verdict: unmarshal review_knowledge_decision failed, verdict will be stamped as not knowledge-influenced",
						"error", unmarshalErr, "repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
				} else {
					knowledgeInfluenced = !injected.Empty()
				}
			}
			if len(processingTurn.ReviewDepthDecision) > 0 {
				var decisionRecord reviewtriage.DecisionRecord
				if unmarshalErr := json.Unmarshal(processingTurn.ReviewDepthDecision, &decisionRecord); unmarshalErr != nil {
					logger.Warn("httpapi: review-verdict: unmarshal review_depth_decision failed, filesChanged drift canary has no server-computed count to compare against",
						"error", unmarshalErr, "repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
				} else {
					serverComputedChangedFiles = decisionRecord.ChangedFilesCount
					diffDelivered = !decisionRecord.DiffEmpty && !decisionRecord.DiffTruncated
					// The gate's own key, read back from where the
					// dispatching path stamped it rather than recomputed
					// here: this endpoint never sees the PR's changed
					// paths, only what the sandbox posts, and the one
					// thing the gate must never key on is anything the
					// model authored. An unmarshal failure above leaves
					// both nil, so the verdict is stamped with no tags
					// and no roots -- it becomes reachable only through
					// the selector's recency fallback, never wrongly
					// matched, which is the safe direction.
					archDecisionTags = decisionRecord.ArchDecisionTags
					archDecisionRoots = decisionRecord.ArchDecisionRoots
				}
			}
		}

		input := reviewpost.VerdictInput{
			RiskLevel:         review.RiskLevel(req.RiskLevel),
			Premise:           review.PremiseState(req.Premise),
			ReviewDepth:       reviewDepth,
			FilesChanged:      req.FilesChanged,
			TestsCoverage:     review.TestsCoverageState(req.TestsCoverage),
			DocsDrift:         review.DocsDriftState(req.DocsDrift),
			ProposedShippable: review.ProposedShippable(req.ProposedShippable),
			Summary:           req.Summary,
			Digest:            digestInputFromWire(req.Digest),
			CounterReview:     counterReviewFromWire(req.CounterReview),
			FactCheck:         reviewpost.FactCheckStatus(req.FactCheck),
			FactCheckKilled:   req.FactCheckKilled,
		}
		for _, tag := range req.BlastRadius {
			input.BlastRadius = append(input.BlastRadius, review.Tag(tag))
		}
		for _, f := range req.Findings {
			input.Findings = append(input.Findings, findingInputFromWire(f))
		}

		// (§26.4/§7.1): post-hoc sub-task corroboration -- see
		// reviewpost.VerdictInput.CounterReviewCorroborated's own doc
		// comment and reviewpost.BuildVerdict's own "Second substitution"
		// doc comment for what this feeds and why. The two corroboration
		// queries (events.ListSubTaskStartsForTurn/ListSubTaskFinishesForTurn)
		// are ONLY run when they could possibly matter -- deep path AND
		// the self-report claims done -- so every light-path verdict, and
		// every deep-path verdict that already self-reports "skipped",
		// pays no extra DB round-trip at all: input.CounterReviewCorroborated
		// simply stays at its own zero value, false, exactly as
		// BuildVerdict's own gate (in.ReviewDepth == DepthDeep &&
		// in.CounterReview == CounterReviewDone) already requires before
		// this field can affect anything.
		if reviewDepth == reviewtriage.DepthDeep && input.CounterReview == review.CounterReviewDone {
			switch {
			case dispatchedSandboxGen == nil:
				// dispatched_sandbox_gen NULL (migrations/
				// 000026_turn_dispatch_gen.up.sql: "NULL before a turn's
				// first real dispatch") -- treated as NOT corroborated,
				// fail-conservative, the same direction every other
				// closed-enum default in this codebase already commits to
				// (review/doc.go's own "fail-conservative policy for
				// every closed enum" section), rather than erroring this
				// request or silently skipping the check. input.
				// CounterReviewCorroborated is simply left at its own
				// zero value, false, below.
				logger.Warn("httpapi: review-verdict: no dispatched_sandbox_gen on record for this turn, treating counter-review claim as uncorroborated",
					"repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
			case dispatchedEventID == nil:
				// dispatched_event_id NULL
				// (migrations/000089_turns_dispatched_event_id.up.sql:
				// nullable, set only once a turn is actually dispatched)
				// -- the IDENTICAL fail-conservative treatment as the
				// dispatchedSandboxGen-nil case immediately above, applied
				// to this Step's own new second precondition. Should be
				// genuinely unreachable in practice (a turn being
				// verdicted right now is, by construction, already
				// dispatched -- this very request is proof of that), but
				// the code must never ASSUME that rather than checking it,
				// exactly like the gen case does not assume gen is always
				// set. Note this is NOT interchangeable with "watermark 0":
				// 0 is a legitimate, meaningful value (a turn dispatched
				// before this session had any events at all) that admits
				// every subsequent event, whereas NULL means the turn was
				// never stamped and nothing can be trusted about its trace.
				logger.Warn("httpapi: review-verdict: no dispatched_event_id on record for this turn, treating counter-review claim as uncorroborated",
					"repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
			default:
				input.CounterReviewCorroborated = corroborateCounterReview(ctx, logger, events, sessionID, *dispatchedSandboxGen, *dispatchedEventID)
			}
		}

		if err := reviewpost.ValidateVerdictInput(input); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		verdict := reviewpost.BuildVerdict(input)
		findings := reviewpost.BuildFindings(input)

		// §21.1's own filesChanged drift canary, now wired: compares
		// verdict.FilesChanged (the reviewing agent's own self-report,
		// just built above) against serverComputedChangedFiles (this
		// SAME turn's own server-computed count, resolved above). Purely
		// diagnostic, by construction, not merely by convention --
		// reviewverdict.FilesChangedDrifted returns a plain bool, and
		// this call site does nothing with a true result but log: verdict
		// itself is never touched, no field on it is read again below
		// this point, and nothing here can affect Shippable, the formal
		// review event, the synced label, or this request's own response.
		// §21.1's own second constraint ("must tolerate
		// ChangedFilesCount == 0") is satisfied by FilesChangedDrifted
		// itself, not by a guard here -- see that function's own doc
		// comment. diffDelivered (D4, resolved above) is FilesChangedDrifted's
		// own THIRD guard, the exact symmetric case: a diff the reviewing
		// agent was never fully handed (empty or truncated) must never
		// make this canary blame the reviewer for a server-side delivery
		// failure -- see that function's own doc comment for the full
		// "why".
		if reviewverdict.FilesChangedDrifted(verdict.FilesChanged, serverComputedChangedFiles, diffDelivered) {
			logger.Warn("httpapi: review-verdict: filesChanged drift canary fired -- self-reported and server-computed changed-file counts diverge beyond both thresholds; diagnostic only, verdict unaffected",
				"self_reported_files_changed", verdict.FilesChanged,
				"server_computed_files_changed", serverComputedChangedFiles,
				"repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
		}

		// verdictHeadSHA/reviewDepth (§26.3) were both already resolved above, before
		// ValidateVerdictInput ran -- see that block's own doc comment
		// for the full "why" (the deep-path digest check needs
		// reviewDepth before validation, and this is simply the earliest
		// point verdictHeadSHA's own pre-existing move
		// already established).
		//
		// §22.1.1's own content-anchored positioning: resolved ONCE, here,
		// before RenderVerdictComment ever renders findings -- "no second
		// pass, by construction" (every finding already present in this
		// SAME payload, §8.2's structured-verdict invariant). Skipped
		// entirely (every finding stays at its own honest StartLine=0/
		// EndLine=0 zero value, reviewpost.BuildFindings' own default) when
		// there is nothing to anchor against at all: no findings, or no
		// confirmed head sha to pin a diff refetch to -- mirrors this
		// handler's own pre-existing "a missing head SHA is a safe,
		// non-fatal degradation" posture exactly, applied here to position
		// resolution instead of the review_verdicts insert.
		if len(findings) > 0 && verdictHeadSHA != "" && diffFetcher != nil {
			if diff, ok := reviewcontext.FetchDiffAt(ctx, logger, diffFetcher, timeouts, owner, repo, prSession.PrNumber, botToken, verdictHeadSHA); ok {
				findings = findingposition.ResolveAll(ctx, positionResolver, findings, diff, timeouts)
			} else {
				logger.Warn("httpapi: review-verdict: fetch diff for position anchoring failed, every finding stays unanchored",
					"repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
			}
		}

		// blockOnHighRisk/sentinelAutofixEnabled (§21.2, §17.1): both
		// admin, per-repo, strict-boolean settings -- a missing row OR any
		// read error defaults BOTH to false (fail-closed, mirroring
		// §24.5's own identical "if the setting cannot be read... treated
		// as OFF" precedent for a comparable per-repo policy flag). A
		// genuine read error is logged but never blocks verdict posting --
		// this is a policy nuance, not a precondition for the tool call to
		// succeed at all.
		blockOnHighRisk := false
		sentinelAutofixEnabled := false
		if settings, settingsErr := repoSettings.Get(ctx, prSession.RepoFullName); settingsErr != nil {
			if !errors.Is(settingsErr, pgx.ErrNoRows) {
				logger.Warn("httpapi: review-verdict: read repo settings failed, defaulting blockOnHighRisk/sentinelAutofixEnabled to false", "error", settingsErr)
			}
		} else {
			blockOnHighRisk = settings.BlockOnHighRisk
			sentinelAutofixEnabled = settings.SentinelAutofixEnabled
		}

		event := reviewpost.ComputeFormalReviewEvent(verdict.Shippable, verdict.RiskLevel, blockOnHighRisk)
		syncedLabel := reviewpost.RiskLabel(verdict.RiskLevel)
		body := reviewpost.RenderVerdictComment(verdict, findings, input.Digest, req.Summary, botHandle, syncedLabel)

		payload, err := json.Marshal(githubapi.VerdictPayload{
			Owner:     owner,
			Repo:      repo,
			PRNumber:  int(prSession.PrNumber),
			Event:     string(event),
			Body:      body,
			RiskLevel: string(verdict.RiskLevel),
		})
		if err != nil {
			logger.Error("httpapi: review-verdict: marshal outbox payload failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Correlation ID propagation -- mirrors internal/app/sessionactor/
		// outboxenqueue.go's own identical "read from ctx if present, else
		// NULL" convention exactly.
		var correlationID *string
		if id, ok := platform.CorrelationIDFromContext(ctx); ok && id != "" {
			correlationID = &id
		}

		// §17.6: is the sentinel-auto-fix flow even a CANDIDATE for this
		// verdict at all -- the toggle is on AND at least one posted
		// finding names a sentinel kind? Computed BEFORE opening the
		// transaction below (a pure, in-memory check) so the "fetch the
		// origin session's own repos/provenance_tag" read below is only
		// ever paid for when it could actually matter.
		autoFixCandidate := sentinelAutofixEnabled && hasSentinelFinding(findings)

		var originHeadBranch, repoName, repoCloneURL string
		if autoFixCandidate {
			sessionRow, sessErr := sessions.Get(ctx, sessionID)
			var repos []restdtos.CreateSessionRequestReposElem
			var reposErr error
			if sessErr == nil {
				reposErr = json.Unmarshal(sessionRow.Repos, &repos)
			}

			switch {
			case sessErr != nil:
				logger.Warn("httpapi: review-verdict: read session row for sentinel-auto-fix eligibility failed, skipping trigger", "error", sessErr)
				autoFixCandidate = false
			case provenance.IsSentinelAutoFix(sessionRow.ProvenanceTag):
				// §17.1's own "no recursion" rule: a PR opened by a
				// sentinel-auto-fix child session is never itself
				// eligible to trigger another, regardless of what its own
				// verdict finds.
				autoFixCandidate = false
			case reposErr != nil || len(repos) == 0 || repos[0].Branch == nil || *repos[0].Branch == "":
				logger.Warn("httpapi: review-verdict: could not determine origin head branch from session repos, skipping sentinel-auto-fix trigger",
					"error", reposErr)
				autoFixCandidate = false
			default:
				originHeadBranch = *repos[0].Branch
				repoName = repos[0].Name
				repoCloneURL = repos[0].Url
			}
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: review-verdict: begin tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		findingsTx := reviewFindings.WithTx(tx)
		findingIdentityHashes := make([]string, 0, len(findings))
		for _, f := range findings {
			if _, upsertErr := findingsTx.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
				RepoFullName: prSession.RepoFullName,
				PrNumber:     prSession.PrNumber,
				IdentityHash: f.IdentityHash,
				SentinelKind: sentinelKindColumn(f.SentinelKind),
				Severity:     string(f.Severity),
				FilePath:     f.FilePath,
				Line:         lineColumn(f.Line),
				Description:  f.Description,
				SuggestedFix: f.SuggestedFix,
			}); upsertErr != nil {
				logger.Error("httpapi: review-verdict: upsert review finding failed", "error", upsertErr)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			findingIdentityHashes = append(findingIdentityHashes, f.IdentityHash)
		}

		// (§21.1): append one review_verdicts row, in the SAME
		// transaction as the findings upserts/outbox write above -- pure
		// storage of the verdict already computed above, forwarding
		// head_sha verbatim from verdictHeadSHA (
		// resolved above from this session's own processing turn, never
		// re-derived or asked of the agent). A missing head SHA (empty --
		// e.g. a review turn whose own context fetch degraded to no diff
		// at all, or no processing turn could be resolved) is logged and
		// SKIPPED, never a reason to fail this tool call: review_verdicts
		// existing reliably matters for the auto-approval engine, but
		// that engine's own fail-CLOSED posture (internal/domain/
		// autoapproval) already treats "no verdict on record" as
		// ineligible, so a missing row here is a SAFE, not a dangerous,
		// degradation. A genuine store error on the INSERT itself, by
		// contrast, is treated exactly like a findings-upsert or
		// outbox-create failure immediately above/below: it fails this
		// whole request (500, rolled back), never silently proceeds with
		// an unpersisted verdict.
		if verdictHeadSHA == "" {
			logger.Warn("httpapi: review-verdict: no review head sha on record, skipping review_verdicts insert", "repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
		} else if _, insertErr := appreviewverdict.Insert(ctx, reviewVerdicts.WithTx(tx), repoSettings.WithTx(tx), platformShadow, prSession.RepoFullName, prSession.PrNumber, verdictHeadSHA, sessionID, verdict, input.Digest, reviewDepth, input.CounterReview, input.FactCheck, input.FactCheckKilled, archDecisionTags, archDecisionRoots, knowledgeMode, knowledgeInfluenced); insertErr != nil {
			logger.Error("httpapi: review-verdict: insert review_verdicts row failed", "error", insertErr)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if _, err := outbox.WithTx(tx).Create(ctx, sqlcgen.CreateOutboxEntryParams{
			SessionID:     sessionID,
			Kind:          string(ports.NotificationKindGitHubVerdict),
			Payload:       payload,
			CorrelationID: correlationID,
		}); err != nil {
			logger.Error("httpapi: review-verdict: enqueue outbox entry failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// §26.2: enqueue exactly one further outbox row whenever
		// the agent proposed a PR-body rewrite (Digest.ProposedBody
		// non-blank) AND the floor that rewrite is meant to remediate
		// actually fired (Digest.DescriptionAdequacy is "drift" or
		// "misleading", never "ok") -- adversarial-review fix: BEFORE this
		// fix, only the non-blank check ran here, so a verdict reporting
		// an ACCURATE description (adequacy "ok") plus an unsolicited,
		// unrelated stylistic proposedBody would still silently enqueue a
		// write that collapses an already-accurate, human-visible
		// description behind a rewrite -- on a verdict that had just
		// certified it adequate. DescriptionAdequacy is already validated
		// to one of review's own three closed enum values by
		// reviewpost.ValidateVerdictInput (above, before this transaction
		// even opens), so "!= review.DescriptionAdequacyOK" here is
		// exactly "== drift || == misleading", never a garbled/missing
		// value slipping through.
		//
		// Both preconditions checked HERE are themselves NEVER a decision
		// that a write will actually happen: the Narvi-authorship check
		// and this repo's own descriptionAutofix flag are BOTH
		// re-verified server-side, fresh, at DELIVERY time by the notifier
		// (internal/app/outboxworker's own description-autofix notifier,
		// §5.2: "never prompt-only, never trusting the agent to
		// self-enforce") -- neither of THOSE checks is performed here, so
		// this handler needs no repoSettings/artifacts read of its own for
		// this candidate path (DescriptionAutofixPayload's own doc
		// comment). DescriptionAdequacy, unlike authorship/the flag, is
		// ALSO carried onto the payload verbatim (never re-derived) and
		// re-asserted at delivery time as a THIRD, defense-in-depth check
		// -- see DescriptionAutofixPayload.DescriptionAdequacy's own doc
		// comment for why this ONE fact travels rather than being
		// re-looked-up. Enqueued in the SAME transaction as every other
		// write above, so a crash between them can never leave this
		// candidate silently dropped.
		if input.Digest.DescriptionAdequacy != review.DescriptionAdequacyOK && strings.TrimSpace(input.Digest.ProposedBody) != "" {
			autofixPayload, marshalErr := json.Marshal(ports.DescriptionAutofixPayload{
				Owner:               owner,
				Repo:                repo,
				PRNumber:            int(prSession.PrNumber),
				ProposedBody:        input.Digest.ProposedBody,
				DescriptionAdequacy: input.Digest.DescriptionAdequacy,
			})
			if marshalErr != nil {
				logger.Error("httpapi: review-verdict: marshal description-autofix outbox payload failed", "error", marshalErr)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			if _, err := outbox.WithTx(tx).Create(ctx, sqlcgen.CreateOutboxEntryParams{
				SessionID:     sessionID,
				Kind:          string(ports.NotificationKindGitHubDescriptionAutofix),
				Payload:       autofixPayload,
				CorrelationID: correlationID,
			}); err != nil {
				logger.Error("httpapi: review-verdict: enqueue description-autofix outbox entry failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}

		if autoFixCandidate {
			claim, claimErr := sentinelFixes.WithTx(tx).Claim(ctx, prSession.RepoFullName, prSession.PrNumber, sessionID, originHeadBranch)
			if claimErr != nil {
				logger.Error("httpapi: review-verdict: claim sentinel_fixes row failed", "error", claimErr)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			// FixChildSessionID.Valid means a fix is ALREADY in flight for
			// this PR (an earlier qualifying finding claimed this same
			// row) -- never enqueue a second, redundant spawn.
			if !claim.FixChildSessionID.Valid {
				var hashes, descriptions []string
				for _, f := range findings {
					if f.SentinelKind != nil {
						hashes = append(hashes, f.IdentityHash)
						descriptions = append(descriptions, f.Description)
					}
				}
				autoFixPayload, marshalErr := json.Marshal(ports.SentinelAutoFixPayload{
					SentinelFixID:         claim.ID.String(),
					RepoFullName:          prSession.RepoFullName,
					OriginPRNumber:        prSession.PrNumber,
					OriginReviewSessionID: sessionID.String(),
					OriginHeadBranch:      originHeadBranch,
					RepoName:              repoName,
					RepoCloneURL:          repoCloneURL,
					FindingIdentityHashes: hashes,
					FindingDescriptions:   descriptions,
				})
				if marshalErr != nil {
					logger.Error("httpapi: review-verdict: marshal sentinel-auto-fix outbox payload failed", "error", marshalErr)
					writeError(w, http.StatusInternalServerError, "internal error")
					return
				}
				if _, err := outbox.WithTx(tx).Create(ctx, sqlcgen.CreateOutboxEntryParams{
					SessionID:     sessionID,
					Kind:          string(ports.NotificationKindSentinelAutoFix),
					Payload:       autoFixPayload,
					CorrelationID: correlationID,
				}); err != nil {
					logger.Error("httpapi: review-verdict: enqueue sentinel-auto-fix outbox entry failed", "error", err)
					writeError(w, http.StatusInternalServerError, "internal error")
					return
				}
			}
		}

		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: review-verdict: commit tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusCreated, restdtos.PostReviewVerdictResponse{
			Shippable:             restdtos.PostReviewVerdictResponseShippable(verdict.Shippable),
			FormalReviewEvent:     restdtos.PostReviewVerdictResponseFormalReviewEvent(event),
			SyncedLabel:           syncedLabel,
			FindingIdentityHashes: findingIdentityHashes,
		})
	}
}

// findingInputFromWire converts one restdtos.PostedFinding (the wire
// shape) into reviewpost.FindingInput -- the one place this conversion
// happens, so a future field addition to either shape only ever needs
// updating here.
func findingInputFromWire(f restdtos.PostedFinding) reviewpost.FindingInput {
	in := reviewpost.FindingInput{
		Severity:    review.RiskLevel(f.Severity),
		FilePath:    f.FilePath,
		Description: f.Description,
	}
	if f.SentinelKind != nil {
		k := reviewpost.SentinelKind(*f.SentinelKind)
		in.SentinelKind = &k
	}
	if f.Line != nil {
		line := int(*f.Line)
		in.Line = &line
	}
	if f.SuggestedFix != nil {
		in.SuggestedFix = f.SuggestedFix
	}
	return in
}

// digestInputFromWire converts one restdtos.Digest (the wire shape,
// §26.1, extended by §26.2) into reviewpost.Digest --
// the one place this conversion happens, mirroring findingInputFromWire's
// own identical "one conversion site" convention immediately above.
// d.Summary/d.DescriptionAdequacy/d.AdequacyExplanation are decode-time-
// guaranteed non-empty/in-enum (restdtos.Digest's own generated
// UnmarshalJSON, minLength 1 / enum) -- ValidateVerdictInput's own
// ErrEmptyDigestSummary/ErrInvalidDescriptionAdequacy/
// ErrEmptyAdequacyExplanation checks are still real defense in depth for
// a whitespace-only or (in principle) otherwise-malformed value, exactly
// mirroring how ValidateVerdictInput already treats the top-level Summary
// field.
//
// Every ArchDecision sub-field, and StackRisks/UnverifiedLimits/
// ProposedBody, are nullable *string on the wire (contracts/rest/v1/
// dtos.schema.json's own ArchDecision/Digest defs carry no "required"/
// minLength on any of them, deliberately -- this Step validation-enforces
// ONLY Digest.summary/descriptionAdequacy/adequacyExplanation, see
// reviewpost.Digest's own doc comment) -- archDecisionStringField below
// nil-safely converts each to reviewpost.ArchDecision's own plain string
// fields, a nil pointer (the field was omitted or explicitly null)
// converting to "" exactly like StackRisks/UnverifiedLimits/ProposedBody
// already do, never a nil-pointer panic.
func digestInputFromWire(d restdtos.Digest) reviewpost.Digest {
	out := reviewpost.Digest{
		Summary:             d.Summary,
		DescriptionAdequacy: review.DescriptionAdequacy(d.DescriptionAdequacy),
		AdequacyExplanation: d.AdequacyExplanation,
	}
	if d.StackRisks != nil {
		out.StackRisks = *d.StackRisks
	}
	if d.UnverifiedLimits != nil {
		out.UnverifiedLimits = *d.UnverifiedLimits
	}
	if d.ProposedBody != nil {
		out.ProposedBody = *d.ProposedBody
	}
	if d.ContestedPoints != nil {
		out.ContestedPoints = *d.ContestedPoints
	}
	for _, ad := range d.ArchDecisions {
		out.ArchDecisions = append(out.ArchDecisions, reviewpost.ArchDecision{
			Decision:              archDecisionStringField(ad.Decision),
			RejectedAlternative:   archDecisionStringField(ad.RejectedAlternative),
			ConventionConformance: archDecisionStringField(ad.ConventionConformance),
		})
	}
	return out
}

// counterReviewFromWire nil-safely converts restdtos.
// PostReviewVerdictRequestCounterReview (a named *string type, go-
// jsonschema's own codegen convention for an unconstrained-nullable-string
// schema property, mirroring PostedFinding.SentinelKind's own identical
// shape) into review.CounterReviewStatus -- a nil pointer (the field was
// omitted, the ordinary case on every light-path verdict, §26.9) converts
// to the zero value "", exactly the value reviewpost.BuildVerdict's own
// light-path substitution expects to see and ValidateVerdictInput leaves
// completely unvalidated outside the deep path (validate.go's own doc
// comment). A present value is forwarded verbatim -- ValidateVerdictInput
// is what actually rejects anything other than review.CounterReviewDone/
// CounterReviewSkipped, and only when this session's own resolved
// ReviewDepth is deep.
func counterReviewFromWire(v restdtos.PostReviewVerdictRequestCounterReview) review.CounterReviewStatus {
	if v == nil {
		return ""
	}
	return review.CounterReviewStatus(*v)
}

// subTaskStartPayload/subTaskFinishPayload are the ONLY two fields this
// call site needs out of each persisted sub_task_start/sub_task_finish
// event's own `payload` JSONB column (contracts/sandbox-ws/v1/
// events.schema.json's own SubTaskStart/SubTaskFinish defs) -- a small
// local decode target, never the full generated sandboxws.SubTaskStart/
// SubTaskFinish wire type, mirroring this file's own existing "decode
// only what a call site actually reads" precedent (e.g.
// reviewtriage.DecisionRecord's own unmarshal above, which does not
// round-trip the WHOLE stored review_depth_decision shape either).
type subTaskStartPayload struct {
	SubTaskID    string `json:"subTaskId"`
	SubAgentType string `json:"subAgentType"`
}

type subTaskFinishPayload struct {
	SubTaskID string `json:"subTaskId"`
	Outcome   string `json:"outcome"`
}

// corroborateCounterReview (§26.4) is this handler's own I/O +
// decode half of post-hoc sub-task corroboration -- the pure comparison
// itself lives in reviewverdict.CounterReviewCorroborated (internal/
// domain/reviewverdict/corroboration.go), which this function is the
// ONE caller of in production. Queries this session's own already-
// persisted sub_task_start/sub_task_finish trace, scoped to BOTH gen (the
// turn being verdicted's own dispatched_sandbox_gen) AND a created_at
// lower bound at dispatchedEventID (that SAME turn's own
// turns.dispatched_event_id)
// -- see queries/events.sql's own ListSubTaskStartEventsForTurn/
// ListSubTaskFinishEventsForTurn doc comment for the full "why" both
// conditions are required together, not gen alone: a session can carry
// multiple review turns over its lifetime (§24's automatic re-review
// chief among the reasons why), and because turns.dispatched_sandbox_gen
// is bumped only on a fresh spawn/restore/resume -- never on an ordinary
// dispatch to an already-live sandbox -- two turns on the same session can
// (and, on the common "sandbox survives across re-review turns" path,
// routinely do) share the identical gen. gen scoping ALONE was found, by
// an adversarial review of this same PR, to let an EARLIER turn's own
// real counter-review trace spuriously corroborate a LATER turn's
// self-report in exactly that case; the dispatchedEventID lower bound is what
// actually closes that gap (turns_one_processing_per_session's own unique
// partial index guarantees turns execute strictly sequentially per
// session, so an earlier turn's own sub-task events all predate a later
// turn's own dispatched_at).
//
// A genuine store error on EITHER query, or a malformed payload on any
// individual row, is logged and treated as NOT corroborated for that row/
// call -- fail-conservative, never a reason to fail this whole verdict-
// posting request: this computation only ever feeds a floor that makes
// Shippable MORE conservative, mirroring every other degradation this
// handler already treats this way (e.g. serverComputedChangedFiles' own
// "a genuine store error... degrades... never a reason to fail this
// whole tool call" precedent above). A single malformed row is skipped,
// not fatal to the rest of the batch -- one corrupt event must not blind
// this function to every OTHER, perfectly good row in the same trace.
//
// # The accepted race -- do not "fix" this into something more complex
//
// This corroboration query runs against whatever sub_task_start/
// sub_task_finish rows are ALREADY committed to Postgres at the moment
// THIS HTTP request is processed. The verdict-posting POST (this
// handler) and the sandbox's own WS event stream (which carries
// sub_task_finish, one of the six ack-guaranteed critical event types)
// are two INDEPENDENT network round-trips from the sandbox, with no
// server-side ordering guarantee between them. The deep-path review
// prompt (review/context.go's own orchestration instructions) tells the
// agent to wait for the counter-reviewer sub-task's own result before
// composing the verdict it then POSTs here, so CAUSALLY the sub-task has
// already resolved -- but that gives no guarantee its own sub_task_finish
// event has actually landed in Postgres by the time this query runs. A
// false negative here (real "done", but the trace is not visible yet)
// fails toward NOT corroborated, which BuildVerdict's own second
// substitution then floors to needs_human -- MORE conservative, never
// less, the identical fail-conservative bias review.CounterReviewSkipped's
// own doc comment already commits to. This is accepted, not a bug: no
// retries, no polling, no new timeout constant belongs here to work
// around it -- see reviewpost.BuildVerdict's own doc comment ("The
// accepted race") for the fuller version of this same reasoning.
func corroborateCounterReview(ctx context.Context, logger *slog.Logger, events *postgres.EventStore, sessionID pgtype.UUID, gen int32, dispatchedEventID int64) bool {
	startRows, err := events.ListSubTaskStartsForTurn(ctx, sessionID, gen, dispatchedEventID)
	if err != nil {
		logger.Warn("httpapi: review-verdict: list sub_task_start events for corroboration failed, treating counter-review claim as uncorroborated", "error", err)
		return false
	}
	finishRows, err := events.ListSubTaskFinishesForTurn(ctx, sessionID, gen, dispatchedEventID)
	if err != nil {
		logger.Warn("httpapi: review-verdict: list sub_task_finish events for corroboration failed, treating counter-review claim as uncorroborated", "error", err)
		return false
	}

	starts := make([]reviewverdict.SubTaskStartRecord, 0, len(startRows))
	for _, row := range startRows {
		var p subTaskStartPayload
		if unmarshalErr := json.Unmarshal(row.Payload, &p); unmarshalErr != nil {
			logger.Warn("httpapi: review-verdict: unmarshal sub_task_start payload for corroboration failed, skipping this row", "error", unmarshalErr)
			continue
		}
		starts = append(starts, reviewverdict.SubTaskStartRecord{
			SubTaskID:    p.SubTaskID,
			SubAgentType: p.SubAgentType,
		})
	}

	finishes := make([]reviewverdict.SubTaskFinishRecord, 0, len(finishRows))
	for _, row := range finishRows {
		var p subTaskFinishPayload
		if unmarshalErr := json.Unmarshal(row.Payload, &p); unmarshalErr != nil {
			logger.Warn("httpapi: review-verdict: unmarshal sub_task_finish payload for corroboration failed, skipping this row", "error", unmarshalErr)
			continue
		}
		finishes = append(finishes, reviewverdict.SubTaskFinishRecord{
			SubTaskID: p.SubTaskID,
			Outcome:   p.Outcome,
		})
	}

	return reviewverdict.CounterReviewCorroborated(starts, finishes)
}

// archDecisionStringField nil-safely dereferences one of restdtos.
// ArchDecision's own three named *string field types (ArchDecisionDecision,
// ArchDecisionRejectedAlternative, ArchDecisionConventionConformance --
// each a distinct Go type sharing the identical *string underlying shape,
// go-jsonschema's own codegen convention for a nullable, non-enum string
// property) into a plain string, "" for a nil pointer.
func archDecisionStringField[T ~*string](p T) string {
	if p == nil {
		return ""
	}
	return *p
}

// hasSentinelFinding reports whether any of findings names a sentinel
// kind (coverage/docs_drift) -- §17.1: "no other sentinel or finding type
// triggers this [sentinel-auto-fix flow]".
func hasSentinelFinding(findings []reviewpost.Finding) bool {
	for _, f := range findings {
		if f.SentinelKind != nil {
			return true
		}
	}
	return false
}

// sentinelKindColumn converts a reviewpost.Finding's own *SentinelKind
// into review_findings.sentinel_kind's own *string column shape.
func sentinelKindColumn(k *reviewpost.SentinelKind) *string {
	if k == nil {
		return nil
	}
	s := string(*k)
	return &s
}

// lineColumn converts a reviewpost.Finding's own *int Line into
// review_findings.line's own *int32 column shape.
func lineColumn(line *int) *int32 {
	if line == nil {
		return nil
	}
	l := int32(*line)
	return &l
}
