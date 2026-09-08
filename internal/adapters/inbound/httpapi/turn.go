package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/intentclassifier"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/authz"
	intentdomain "github.com/narvidev/narvi/internal/domain/intent"
	"github.com/narvidev/narvi/internal/domain/turn"
	domainupload "github.com/narvidev/narvi/internal/domain/upload"
	"github.com/narvidev/narvi/internal/platform"
)

// hasOpenTurn reports whether ANY turn in turns is non-terminal (Pending,
// Dispatched, or Processing) -- deliberately NOT turn.HasInFlightTurn,
// which only counts Dispatched/Processing (correct for its own callers:
// dispatch.go's NextToDispatch needs Pending turns to NOT count as
// "in flight", since a Pending turn is exactly the one it's looking to
// dispatch next). CreateTurn's own precondition is stricter: this
// endpoint must refuse to queue a SECOND turn while an earlier one is
// still Pending too, not just once it's actually Dispatched/Processing --
// otherwise concurrent relaunch calls against a session with zero
// existing turns would each see "no Dispatched/Processing turn" and all
// insert their own Pending row, defeating the 409 this handler exists to
// return (see this file's own CreateTurn doc comment).
func hasOpenTurn(turns []sqlcgen.Turn) bool {
	for _, t := range turns {
		if !turn.IsTerminal(turn.State(t.Status)) {
			return true
		}
	}
	return false
}

// CreateTurn backs POST /api/sessions/{sessionID}/turns ("turn
// recovery", §8.7 "Recovery UX: relaunch-and-resume (conversation id
// replay)"): the relaunch-and-resume REST API. Enqueues a new Pending turn
// on an EXISTING session -- 404 if the session doesn't exist, 409 if
// another turn for it hasn't reached a terminal state yet, otherwise 201
// with restdtos.CreateTurnResponse.
//
// Because sessions.opencode_conversation_id already persists across turns
// (§3.3) and internal/app/sessionactor/dispatch.go's own buildPromptPayload
// already includes it automatically in every Prompt it builds, the new
// turn created here automatically resumes the SAME OpenCode conversation
// the moment it dispatches -- no separate "resume" flag or branch is
// needed in either the request DTO (CreateTurnRequest has no
// conversationId field at all) or this handler.
//
// The precondition check below (hasOpenTurn) is deliberately stricter than
// turn.NextToDispatch's own "in flight" concept (Dispatched/Processing
// only) -- this endpoint refuses to queue a SECOND turn while an earlier
// one is still merely Pending too, not just once it's Dispatched or
// Processing, since a session with zero prior turns would otherwise let
// every concurrent relaunch call see "nothing in flight yet" and each
// insert its own Pending row. It is also an APPLICATION-level enforcement
// of the "exactly one processing per session" invariant the domain turn
// machine and the DB's own partial unique index
// (turns_one_processing_per_session, migration 000005_turns.up.sql)
// already enforce elsewhere -- deliberately checked here, before ever
// inserting, rather than relying on that index to reject the insert: the
// index is scoped to status = 'processing' ONLY, so inserting a new
// 'pending' row while another turn is already Dispatched/Processing would
// NOT violate it at all -- it would silently succeed, creating a turn that
// can never legally dispatch while the OTHER one holds the session's one
// in-flight slot. This check closes that gap up front with a clear 409,
// rather than letting a caller queue something that would otherwise sit
// invisibly stuck.
//
// The check runs INSIDE the same transaction as the insert, serialized by
// SessionStore.GetActorEpochForUpdate's own row-level `SELECT ... FOR
// UPDATE` on the session (the exact same lock sessionactor's own dispatch
// path already uses, actor.go) -- a plain pre-transaction list-then-insert
// would leave a check-then-act race open: two concurrent requests could
// both observe "no in-flight turn" before either commits, both insert a
// Pending row, and neither gets the 409 this handler exists to return.
// Locking the session row first forces a second concurrent request to
// block until the first's transaction commits (or rolls back), so it
// re-reads the turns list only after that outcome is visible.
//
// objCfg (FIX D, this batch's own follow-up fix, §28.7) is threaded
// through ONLY so this handler can populate CreateTurnOptions.
// StorageConfigured (objCfg != nil, the identical signal mintUploadCore
// checks) -- never consulted for anything else here; nil is a completely
// valid, ordinary value (a deployment with no object storage configured
// at all), never a caller error.
func CreateTurn(pool *pgxpool.Pool, sessions *postgres.SessionStore, turns *postgres.TurnStore, plans *postgres.PlanStore, participants *postgres.ParticipantStore, auditLog *postgres.AuditLogStore, registry *sessionactor.Registry, intentSvc *intentclassifier.Service, objCfg *platform.ObjectStorageConfig, epistemicCheckDefault bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		r = r.WithContext(ctx)
		logger := platform.Logger(ctx)

		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		// §13.3 row 2: "... prompt ... on own/joined sessions: admin,
		// maintainer, member" (viewer never; admin/maintainer bypass
		// ownership entirely). A plain, pool-scoped read (not WithTx) is
		// enough to resolve ownership here -- mirrors
		// lockSessionForPlanAction/authorizePlanAction's own identical
		// "fetch once for authz" precedent (planapprove.go): sessions.
		// created_by is immutable once set, so there is no meaningful
		// TOCTOU between this read and CreateTurnCore's own separate,
		// LOCKED re-fetch below. 404 here (not found) takes priority over
		// any 403 -- a caller must never learn "you can't prompt this"
		// about a session that doesn't exist at all.
		sessionRow, err := sessions.Get(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: get session for authorization failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		ownedOrJoined := sessionRow.CreatedBy.Valid && sessionRow.CreatedBy == actorUserID
		if !ownedOrJoined {
			exists, err := participants.Exists(ctx, sessionRow.ID, actorUserID)
			if err != nil {
				logger.Error("httpapi: check participant for authorization failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			ownedOrJoined = exists
		}
		if !authorize(w, r, authz.ActionPromptSession, authz.Resource{OwnedOrJoined: ownedOrJoined}) {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.CreateTurnRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}

		// (§28.5): attachmentIds is parsed here, at the REST
		// boundary, rather than deep inside createTurnLocked's own
		// transaction -- a malformed uuid string is a client-input
		// mistake the same 400 turn.go's own other decode failures
		// already return, not something worth surfacing as a generic
		// "internal error" from inside a locked transaction.
		attachmentIDs := make([]pgtype.UUID, 0, len(req.AttachmentIds))
		for _, raw := range req.AttachmentIds {
			var id pgtype.UUID
			if err := id.Scan(raw); err != nil {
				writeError(w, http.StatusBadRequest, "malformed attachmentIds entry")
				return
			}
			attachmentIDs = append(attachmentIDs, id)
		}

		created, _, cerr := CreateTurnCore(ctx, pool, sessions, turns, plans, intentSvc, auditLog, registry, sessionID, req.Prompt, (*string)(req.ModelId), req.PlanMode, epistemicCheckDefault, actorUserID, RejectIfOpen, CreateTurnOptions{
			AttachmentIDs:     attachmentIDs,
			StorageConfigured: objCfg != nil,
			Effort:            (*string)(req.Effort),
		})
		if cerr != nil {
			logger.Error("httpapi: create turn failed", "status", cerr.Status, "message", cerr.Message)
			writeError(w, cerr.Status, cerr.Message)
			return
		}

		writeJSON(w, http.StatusCreated, restdtos.CreateTurnResponse{
			Id:     created.ID.String(),
			Status: restdtos.CreateTurnResponseStatus(created.Status),
		})
	}
}

// CreateTurnError carries the exact (status, message) pair a caller of
// CreateTurnCore should surface -- mirrors CreateSessionError's own
// identical purpose (create.go): a distinct type, not a plain error, so
// CreateTurn's own existing tests/messages stay byte-for-byte unchanged
// after this extraction, and so a non-HTTP caller (Slack's view_submission
// handling, internal/adapters/inbound/slack/interactive.go) can inspect
// Status/Message directly the same way internal/adapters/inbound/{slack,
// linear} already do for CreateSessionError.
type CreateTurnError struct {
	Status  int
	Message string

	// sentinel is set ONLY for a small, well-known set of reasons a caller
	// might need to recognize distinctly via errors.Is -- currently just
	// ErrPlanAwaitingApproval below. Every OTHER CreateTurnError
	// construction leaves this nil, so errors.Is against anything is
	// simply false for those, exactly as before this field existed.
	// Mirrors internal/domain/plan.IllegalTransitionError's own identical
	// Unwrap-to-sentinel shape.
	sentinel error
}

func (e *CreateTurnError) Error() string { return e.Message }

// Unwrap lets a caller recognize a specific well-known CreateTurnError via
// errors.Is (e.g. errors.Is(cerr, ErrPlanAwaitingApproval)) without needing
// to string-match Message or duplicate the exact Status this core uses.
func (e *CreateTurnError) Unwrap() error { return e.sentinel }

// ErrPlanAwaitingApproval is createTurnLocked's own sentinel (this batch's
// own follow-up fix to §8.1, closing the "reply matching no
// verdict keyword dispatches an ordinary build turn anyway" hole found
// during design review) for the one new reason a turn creation can be
// declined that every other CreateTurnError construction never carries:
// sessionID currently has a plan in plan.StatusAwaitingApproval AND the
// turn being created is an ordinary (planMode == false) one. Recovered via
// errors.Is against the *CreateTurnError this core returns.
//
// REST's own CreateTurn handler needs no special handling for this at all
// -- it already forwards cerr.Status/cerr.Message verbatim, which IS this
// gate's own required 409 shape. Slack's addTurn (turn.go) and Linear's
// handlePrompted (webhook.go) -- both of which, before this fix, treated
// ANY cerr from this core as a hard failure -- now recognize this ONE
// specific reason via errors.Is and post an honest, non-error reply
// instead (mirroring DropIfOpen's own existing "still busy" honest-reply
// precedent for the analogous open-turn case). GitHub's own handler.go
// error switch now does the same for its bot-ingress path (bot.go's
// CreateTurnForBot, reused by github/coalesce.go's REUSE path) -- this
// needed its own separate fix (Finding 1 of this batch's follow-up),
// since CreateTurnForBot used to re-wrap cerr via fmt.Errorf's "%s" verb,
// which discarded the error chain entirely and made this sentinel
// unrecoverable via errors.Is for any caller of that function; it now
// wraps with "%w" instead, so GitHub's handler.go can recognize this ONE
// specific reason exactly like Slack/Linear do, acknowledge 200 without
// releasing the webhook delivery claim (this is a deterministic,
// expected business state, not a transient failure a redelivery could
// ever fix), and post an honest reply on the PR thread instead of
// producing a silent, self-inflicted redelivery retry storm.
var ErrPlanAwaitingApproval = errors.New("httpapi: plan awaiting approval")

// planAwaitingApprovalMessage is ErrPlanAwaitingApproval's own REST-facing
// text -- REST's 409 body verbatim (CreateTurn's own doc comment above),
// mirroring the existing open-turn 409's shape exactly (same CreateTurnError
// type, just a different Message).
const planAwaitingApprovalMessage = "a plan is awaiting approval for this session; approve or reject it, or submit a plan-revision turn"

// CreateTurnPolicy selects CreateTurnCore's own behavior when sessionID
// already has a non-terminal ("open") turn -- audit-fix batch addition
// (findings H7/L12/L2/M6): before this batch, REST/Slack/Linear/GitHub-bot
// ingress each independently decided (and, for Linear, never even locked
// for) this exact question, in four separately-written, only-partly-
// consistent implementations. One shared enum, consulted by the one
// shared core (createTurnLocked below) that ALL FOUR now call through,
// replaces every one of those copies.
type CreateTurnPolicy int

const (
	// RejectIfOpen refuses to enqueue a second turn while one is already
	// open, returning a 409 CreateTurnError -- the REST relaunch
	// endpoint's own long-standing policy (CreateTurn's own doc comment
	// above). Also used, unchanged, by Slack's "Request changes" modal
	// submission (interactive.go's own handleViewSubmission), which has
	// always gone through this exact function.
	RejectIfOpen CreateTurnPolicy = iota
	// DropIfOpen silently declines to enqueue while a turn is already
	// open: the returned turn is the zero value, wasCreated is false, and
	// cerr is nil -- there is no 409 to surface here, only a caller-
	// rendered "still busy" response. Slack's addTurn (turn.go) and
	// Linear's handlePrompted ordinary-reply path (webhook.go) both use
	// this (M6 audit fix: before this batch each swallowed the same case
	// inconsistently -- Slack posted a false "I'll pick this up next"
	// promise that nothing ever fulfilled, Linear said nothing back to
	// the user at all).
	DropIfOpen
	// AlwaysQueue skips the open-turn check entirely, unconditionally
	// enqueuing -- CreateTurnForBot's own fixed policy (bot.go),
	// preserving GitHub's deliberate per-PR mention-coalescing Pending
	// backlog (see that function's own doc comment for why this is NOT a
	// general-purpose policy any other caller should reach for).
	AlwaysQueue
)

// CreateTurnOptions bundles CreateTurnCore/createTurnLocked's own REST-only,
// opt-in concerns (§28.5) into the ONE trailing variadic parameter
// a bare "attachmentIDs ...pgtype.UUID" used to occupy alone -- Go permits
// only one variadic parameter, and it must be last, so a second,
// independent optional concern (StorageConfigured, added by this batch's
// own FIX D) cannot be a second variadic; bundling both into one struct
// behind the SAME single variadic slot keeps every one of this core's
// other five call sites (reviewretrigger.go, linear/webhook.go,
// slack/turn.go, slack/interactive.go, bot.go's own direct createTurnLocked
// call) compiling completely UNCHANGED -- omitting a variadic argument is
// always valid Go regardless of its element type, exactly as it always has
// been for the bare []pgtype.UUID this replaces. Only CreateTurn's own REST
// handler (turn.go, below) ever populates either field.
type CreateTurnOptions struct {
	// AttachmentIDs is validated inside createTurnLocked's own locked
	// transaction exactly as the bare variadic parameter it replaces
	// always was -- see createTurnLocked's own doc comment.
	AttachmentIDs []pgtype.UUID

	// StorageConfigured reports whether THIS deployment has object
	// storage configured (§28.7's own feature flag -- the identical
	// signal mintUploadCore checks to return "uploads not configured",
	// objCfg != nil) at the moment this turn is created. Gates
	// RenderUploadToolNote's own visibility at the render site below,
	// INDEPENDENT of whether this turn carries any attachments at all
	// (FIX D, §28.5: "surfaced to the agent ... in build-turn prompts" --
	// not "only on turns that also happen to attach a file"). Left at its
	// Go zero value (false) by every caller other than CreateTurn's own
	// REST handler -- review-retrigger, Slack, Linear, and GitHub-bot
	// turns therefore NEVER render this note regardless of deployment
	// config, preserving §28.5's own "build-turn prompts" scoping without
	// any of those four packages needing to learn anything about object
	// storage at all.
	StorageConfigured bool

	// Effort mirrors modelID's own "per-message override" role one field
	// over (§29.8) -- bundled into this same options struct
	// rather than a new positional parameter alongside modelID for the
	// identical reason AttachmentIDs/StorageConfigured are: every one of
	// this core's five OTHER call sites (reviewretrigger.go, linear/
	// webhook.go, slack/turn.go, slack/interactive.go, bot.go) has no
	// per-message effort selector of its own to supply (confirmed: every
	// one of them already passes modelID=nil too, the same "no per-surface
	// override" reality effort now shares) -- left at its Go zero value
	// (nil, "use the default") by all of them, exactly like Storage
	// Configured. Only CreateTurn's own REST handler below ever sets it.
	Effort *string

	// ReviewHeadSHA is the
	// commit SHA THIS turn's own pre-fetched review diff was anchored to
	// -- non-nil ONLY for a review-session turn (reviewretrigger.go's own
	// manual-retrigger path; the GitHub mention/label-retrigger path via
	// CreateSessionOnTx's own ChildSessionOptions.ReviewHeadSHA, one turn
	// earlier in a brand-new session's own lifecycle -- see that type's
	// own doc comment, create.go). Stored verbatim onto turns.
	// review_head_sha (migrations/000072_turns_review_head_sha.up.sql) at
	// INSERT time below -- immutable for this turn's own lifetime, unlike
	// the PREVIOUS github_pr_sessions.pending_head_sha design this fix
	// replaces (a single, shared, mutable per-(repo,PR) column ANY
	// later, unrelated turn's own context-fetch could silently
	// overwrite -- see that migration's own doc comment for the full
	// "why"). Every OTHER caller of this core (every non-review turn)
	// leaves this nil, exactly like Effort/StorageConfigured above.
	ReviewHeadSHA *string

	// ReviewDepth/ReviewDepthDecision (§26.3) mirror
	// ReviewHeadSHA's own identical shape one field further -- non-nil
	// ONLY for a review-session turn, the SAME callers that set
	// ReviewHeadSHA. Stored verbatim onto turns.review_depth/
	// review_depth_decision (migrations/000080_turns_review_depth.up.sql,
	// 000083_turns_review_depth_decision.up.sql) at INSERT time below.
	// ReviewDepthDecision is pre-marshaled JSON (internal/domain/
	// reviewtriage.DecisionRecord) -- this core does no encoding of its
	// own, mirroring this codebase's own "caller owns the encoding"
	// convention (e.g. internal/app/reviewverdict.marshalTags).
	ReviewDepth         *string
	ReviewDepthDecision []byte

	// ReviewKnowledgeMode/ReviewKnowledgeDecision (§31.2/§31.6's own mode
	// buffer) mirror ReviewDepth/ReviewDepthDecision's own identical
	// shape two fields further: non-nil ONLY for a review-session turn,
	// the SAME callers that set ReviewDepth/ReviewDepthDecision. Stored
	// verbatim onto turns.review_knowledge_mode/review_knowledge_decision
	// (migrations/000114_turns_review_knowledge_mode.up.sql,
	// 000116_turns_review_knowledge_decision.up.sql) at INSERT time
	// below. ReviewKnowledgeDecision is pre-marshaled JSON (internal/
	// domain/knowledge.InjectedRecord) -- this core does no encoding of
	// its own, mirroring ReviewDepthDecision's own identical convention.
	ReviewKnowledgeMode     *string
	ReviewKnowledgeDecision []byte

	// ClassifyText (a follow-up fix, review Finding 1) is the raw,
	// unprefixed human reply text the plan_followup block below (just
	// before tx.Begin) should classify -- mirrors github/coalesce.go's own
	// pre-existing classifyText parameter for the §8.3 classifier
	// (that function's own doc comment: "Audit fix: this used to be
	// *req.Prompt directly, which ... already had ... the entire PR diff
	// ... appended -- feeding the classifier's LLM call the entire PR
	// diff instead of just the triggering comment ... text"). The SAME
	// bug class existed here for ClassifyPlanFollowup: github/coalesce.go's
	// REUSE-path call to CreateTurnForBot, and reviewretrigger.go's own
	// call to CreateTurnCore, both used to hand createTurnLocked a
	// `prompt` value ALREADY folded with review.RenderTurnPrompt's own
	// full diff/stack/verdict-tool-instructions text -- inflating the
	// classifier's own LLM call cost/latency by orders of magnitude and
	// risking exceeding the model's context window, exactly like before.
	//
	// nil (every caller other than github/coalesce.go's REUSE-path
	// CreateTurnForBot call) means "no raw text was captured separately
	// for this call site" -- createTurnLocked falls back to classifying
	// `prompt` itself, correct for every caller where prompt genuinely IS
	// the human's own raw words with nothing folded in (REST's own
	// CreateTurn, Slack's addTurn/interactive.go, Linear's webhook.go).
	// Non-nil means the caller captured a raw text distinct from the
	// (possibly enriched) prompt this turn will actually dispatch with --
	// *ClassifyText, not prompt, is what gets classified.
	ClassifyText *string
}

// CreateTurnCore is everything CreateTurn's own doc comment above
// describes AFTER decoding the request body: fetch (404), lock (the SAME
// GetActorEpochForUpdate call, closing the identical check-then-act race
// this function's own top doc comment already documents), then delegates
// the rest (the policy-gated open-turn check, insert, audit, commit,
// dispatch) to createTurnLocked below.
//
// Exported ("plan mode, cross-channel", §8.1/§13.3) so Slack's
// own "Request changes" modal submission (internal/adapters/inbound/slack/
// interactive.go) can create a real plan_mode=true turn through the EXACT
// SAME path POST .../turns itself uses, rather than a third, duplicated
// turn-creation call site -- mirrors CreateSessionCore's own identical
// cross-package reuse precedent (internal/adapters/inbound/{slack,linear}
// already call that one directly).
//
// policy (audit-fix batch addition -- see CreateTurnPolicy's own doc
// comment) is now the ONE remaining difference between this function's
// current callers: REST's own CreateTurn above (RejectIfOpen), Slack's
// addTurn (turn.go, DropIfOpen) and its "Request changes" modal submission
// (interactive.go, RejectIfOpen -- unchanged), and Linear's handlePrompted
// ordinary-reply path (webhook.go, DropIfOpen). Every OTHER step this
// function performs is now identical for all of them -- closing L2
// (Linear's own previous unlocked, raced Turns.Create), H7 (Slack/Linear
// never wrote this turn's own audit_log row at all), and L12 (hasOpenTurn-
// shaped logic copy-pasted, not shared, across three packages) all at
// once. GitHub's bot-ingress path (CreateTurnForBot, bot.go) reuses the
// SAME createTurnLocked core with AlwaysQueue, but deliberately WITHOUT
// this function's own pre-transaction existence check below -- see that
// function's own doc comment for why.
//
// actorUserID is §13.2's own addition, for the audit_log row
// createTurnLocked writes on the SAME tx as the turn insert (§13.3): a
// real authenticated caller's id from CreateTurn (the REST handler above,
// which ALSO already ran authz.Authorize against this same actor before
// ever reaching this function), or an explicit invalid pgtype.UUID{} for
// a still-unresolved bot-attributed caller (Slack/Linear, when the
// mentioning/replying identity hasn't been auto-linked) -- mirrors
// CreateSessionOnTx's own identical createdBy convention exactly. This
// parameter carries NO authorization meaning here: CreateTurnCore itself
// still runs no Authorize check (that stays each caller's own job,
// precisely so a still-unlinked actor's call can keep its existing,
// documented bot-attribution behavior unchanged).
//
// opts (§28.5, extended by this batch's own FIX D) is a TRAILING
// VARIADIC parameter, deliberately not a plain struct: mirrors workflows'
// own "constructed fresh from pool, not threaded as a new parameter"
// precedent just above in spirit, but for genuinely caller-supplied values
// that DO need a signature change -- see CreateTurnOptions' own doc comment
// for why bundling AttachmentIDs/StorageConfigured behind one variadic slot
// (rather than a bare "attachmentIDs ...pgtype.UUID" plus a second, separate
// parameter) lets every one of this core's five pre-existing call sites
// (reviewretrigger.go, linear/webhook.go, slack/turn.go,
// slack/interactive.go, bot.go's own direct createTurnLocked call) stay
// completely UNCHANGED, since neither concept is anything they carry --
// only CreateTurn's own handler below, the sole caller that ever has
// either to pass, changed at all.
//
// epistemicCheckDefault ("builder epistemic pre-action check",
// §20.4) is a REQUIRED positional parameter, deliberately NOT bundled into
// CreateTurnOptions' own trailing variadic slot: unlike StorageConfigured/
// Effort there (genuinely REST-only concerns, §28.5/§29.8, safe to leave
// at their Go zero value for every other caller), the platform-wide
// epistemic-check default is a real, deployment-wide setting every
// ingress surface must apply identically -- leaving it implicit would mean
// Slack/Linear/GitHub-bot-created turns could never receive the operator's
// own configured default (platform.Config.EpistemicCheckDefault) even
// when REST turns do, a silent, surface-dependent inconsistency. Making it
// a required parameter instead means every call site (this function, plus
// createTurnLocked's five other callers) must compile-time-decide what to
// pass, exactly like planMode itself immediately before it -- the two
// travel together into createTurnLocked's own turn.ResolveEpistemicCheck
// Enabled/turn.ShouldInjectEpistemicPreamble calls (see that function's
// own doc comment).
func CreateTurnCore(ctx context.Context, pool *pgxpool.Pool, sessions *postgres.SessionStore, turns *postgres.TurnStore, plans *postgres.PlanStore, intentSvc *intentclassifier.Service, auditLog *postgres.AuditLogStore, registry *sessionactor.Registry, sessionID pgtype.UUID, prompt string, modelID *string, planMode bool, epistemicCheckDefault bool, actorUserID pgtype.UUID, policy CreateTurnPolicy, opts ...CreateTurnOptions) (sqlcgen.Turn, bool, *CreateTurnError) {
	logger := platform.Logger(ctx)

	if _, err := sessions.Get(ctx, sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusNotFound, Message: "session not found"}
		}
		logger.Error("httpapi: get session failed", "error", err)
		return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusInternalServerError, Message: "internal error"}
	}

	return createTurnLocked(ctx, pool, sessions, turns, plans, intentSvc, auditLog, registry, sessionID, prompt, modelID, planMode, epistemicCheckDefault, actorUserID, policy, opts...)
}

// createTurnLocked is the genuinely shared core every one of this batch's
// four (now-consolidated) call sites eventually runs through -- factored
// out of CreateTurnCore specifically so CreateTurnForBot (bot.go) can
// reuse it WITHOUT CreateTurnCore's own pre-transaction sessions.Get
// existence check above: CreateTurnForBot has never had that pre-check
// (its own doc comment), and preserving that exact, deliberate difference
// is what "unchanged behavior" for GitHub's own ingress means here.
//
// Sequencing: lock the session row (GetActorEpochForUpdate -- the SAME
// race-closing lock CreateTurn's own top doc comment documents; this is
// what closes L2 for Linear's own caller, which previously took no lock
// at all before its own equivalent check), the policy-gated open-turn
// check (skipped entirely for AlwaysQueue -- see CreateTurnPolicy's own
// doc comment), the awaiting-plan gate (this batch's own addition -- see
// ErrPlanAwaitingApproval's own doc comment; only reached when the
// open-turn check above did NOT already return), insert, the SAME
// turn.create audit_log row every caller now gets (H7 audit fix), commit,
// then the SAME fire-and-forget GetOrSpawn+EnsureDispatched post-commit
// sequencing every turn-creation call site in this codebase has always
// used.
//
// Finding 3 of this batch's own follow-up fix reordered these two checks
// (the awaiting-plan gate used to run FIRST, unconditionally, before the
// policy-gated open-turn check): internal/app/sessionactor/planrecord.go's
// own recordPlanIfNeeded only supersedes the OLD awaiting_approval plan
// row at the END of a revise turn's (plan_mode=true) own processing, not
// at that turn's own creation time -- so for the ENTIRE duration an
// in-flight revise turn is open/dispatched/processing, BOTH "an open turn
// exists" AND "the old plan is still awaiting_approval" are simultaneously
// true. Running the awaiting-plan gate first used to mean an ordinary
// message arriving during that exact overlap window got the "plan is
// awaiting your approval" reply instead of the busy reply -- misleading,
// since the user's own revision request is already being processed, not
// idly waiting on a decision. Running the policy-gated open-turn/busy
// check FIRST fixes this: whenever a turn is already open/in-flight, the
// busy reply is always the more accurate message regardless of plan
// state, and the awaiting-plan gate's own job (refusing a NEW, first,
// unapproved build turn) is moot anyway when nothing new could be
// dispatched right now regardless. The two checks were always independent
// early-returns against the SAME already-locked session row, so swapping
// their order changes nothing else -- only which message wins in this one
// narrow overlap window.
//
// plans is §8.1's own follow-up fix (§8.1) addition, nil-safe like
// this codebase's other optional collaborators (e.g. Deps.IntentClassifier
// elsewhere) -- a nil plans skips the awaiting-plan gate entirely rather
// than panicking, so a caller/test that genuinely has no use for plan mode
// is never forced to wire one up. Every real production caller
// (cmd/control-plane/main.go) always passes the SAME, real *postgres.
// PlanStore, so this is never nil outside tests.
//
// intentSvc (§23.1/§23.2) is this function's own plan_followup
// classifier collaborator -- nil-safe exactly like plans immediately
// above (a nil intentSvc, or a nil plans, skips classification entirely
// and falls back to the pre-existing "always decline" awaiting-plan gate
// behavior, never a panic). Every real production caller
// (cmd/control-plane/main.go) passes the SAME, real *intentclassifier.
// Service every OTHER intentSvc-consuming caller in this codebase does
// (create.go's own CreateSession/CreateSessionCore), never a second,
// independently-constructed copy. See the plan_followup block below (just
// before tx.Begin) and the awaiting-plan gate further down for how this is
// actually consulted.
func createTurnLocked(ctx context.Context, pool *pgxpool.Pool, sessions *postgres.SessionStore, turns *postgres.TurnStore, plans *postgres.PlanStore, intentSvc *intentclassifier.Service, auditLog *postgres.AuditLogStore, registry *sessionactor.Registry, sessionID pgtype.UUID, prompt string, modelID *string, planMode bool, epistemicCheckDefault bool, actorUserID pgtype.UUID, policy CreateTurnPolicy, opts ...CreateTurnOptions) (sqlcgen.Turn, bool, *CreateTurnError) {
	logger := platform.Logger(ctx)

	// opts is a trailing variadic (CreateTurnOptions' own doc comment)
	// carrying at most one element -- every one of this core's five
	// pre-existing callers omits it entirely, leaving both fields at
	// their Go zero value (no attachments, storage not "configured" as
	// far as THIS turn's own rendering is concerned).
	var attachmentIDs []pgtype.UUID
	var storageConfigured bool
	var effort *string
	var reviewHeadSHA *string
	var classifyText *string
	var reviewDepth *string
	var reviewDepthDecision []byte
	var reviewKnowledgeMode *string
	var reviewKnowledgeDecision []byte
	if len(opts) > 0 {
		attachmentIDs = opts[0].AttachmentIDs
		storageConfigured = opts[0].StorageConfigured
		effort = opts[0].Effort
		reviewHeadSHA = opts[0].ReviewHeadSHA
		classifyText = opts[0].ClassifyText
		reviewDepth = opts[0].ReviewDepth
		reviewDepthDecision = opts[0].ReviewDepthDecision
		reviewKnowledgeMode = opts[0].ReviewKnowledgeMode
		reviewKnowledgeDecision = opts[0].ReviewKnowledgeDecision
	}

	// §23 ("plan mode: follow-up intent classification", §23.1/§23.2):
	// plan_followup classification, gated STRICTLY on "planMode is false
	// AND sessionID currently has a plan sitting in plan.
	// StatusAwaitingApproval" (§23.1: "the classifier is never invoked for
	// this purpose outside that state"). Runs BEFORE tx.Begin below, UNLOCKED
	// -- a real outbound LLM call must never hold a Postgres transaction/row
	// lock open (mirrors intentclassifier.Service.ClassifyAndRecord's own
	// identical rule, doc comment: "a real outbound LLM call must never hold
	// one open"). This unlocked read is deliberately NOT authoritative on its
	// own -- mirrors CreateTurnCore's own "sessions.Get" (unlocked
	// pre-check) vs "GetActorEpochForUpdate" (locked re-check) precedent,
	// this file's own top doc comment: the awaiting-plan gate below,
	// running INSIDE the transaction on the SAME already-locked session
	// row every other check in this function uses, re-verifies plan state
	// and is what actually enforces the invariant. A plan approved/
	// rejected, or newly created, in the narrow window between this read
	// and that locked re-check simply means this call's own classification
	// goes unused -- the gate below treats that exactly like classification
	// never having run at all (§23.3's own fail-open floor).
	//
	// answerOnly stays nil (never computed, "classification did not apply")
	// whenever: planMode is already true (a revise:-prefixed reply, a
	// Request-changes modal submission, or any other explicit
	// planMode=true caller -- §23 intro: "the revise: prefix stays as a
	// deterministic override that bypasses classification entirely"),
	// plans or intentSvc is nil (never true in production wiring), the
	// unlocked read errors (logged, non-fatal -- the locked re-check's own
	// existing fail-safe default still protects correctness), or no
	// awaiting_approval plan is found here at all.
	//
	// F6 (review synthesis, deliberate no-op): classification also runs
	// BEFORE the open-turn/busy check further down (DropIfOpen/RejectIfOpen),
	// so a paid LLM call can be made and thrown away when the turn is about
	// to be dropped/rejected for being busy -- accepted deliberately, not an
	// oversight: moving classification behind an unlocked busy probe would
	// make the skip decision RACY in the harmful direction (a legitimate
	// confident-amend reply could get incorrectly held instead of promoted
	// if the in-flight turn's own state changes between that probe and the
	// lock), which is strictly worse than occasionally paying for a call
	// whose result goes unused.
	//
	// classifyText (F1, a follow-up fix, review Finding 1) is used
	// here INSTEAD OF prompt when non-nil -- see CreateTurnOptions.
	// ClassifyText's own doc comment for the full "why": prompt itself may
	// already carry review.RenderTurnPrompt's own diff/stack/verdict-tool
	// text folded in (github/coalesce.go's REUSE path; reviewretrigger.go's
	// own manual-retrigger prompt), which must never reach this classifier
	// call -- only the reply's own raw, unprefixed body may
	// (ClassifyPlanFollowup's own doc comment, planfollowup.go).
	var answerOnly *bool
	if !planMode && plans != nil && intentSvc != nil {
		summaries, err := plans.ListSummariesForSession(ctx, sessionID)
		if err != nil {
			logger.Warn("httpapi: list plan summaries for plan_followup pre-check failed; classification skipped", "error", err, "session_id", sessionID.String())
		} else {
			for _, s := range summaries {
				if s.Status == sqlcgen.PlanStatusAwaitingApproval {
					textToClassify := prompt
					if classifyText != nil {
						textToClassify = *classifyText
					}
					decision := intentSvc.ClassifyPlanFollowup(ctx, textToClassify)
					ao := intentdomain.ResolveAnswerOnly(decision.Source, decision.Target, decision.Confidence)
					answerOnly = &ao
					break
				}
			}
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		logger.Error("httpapi: begin create-turn tx failed", "error", err)
		return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusInternalServerError, Message: "internal error"}
	}
	// Rollback is a safety net for every return path other than a
	// successful Commit below -- mirrors CreateSession's own identical
	// pattern (create.go).
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the session row before ever reading the turns list below --
	// see this function's own doc comment for why this closes the
	// check-then-act race a plain pre-transaction read would leave
	// open. A concurrent DELETE of the session between an earlier
	// existence check (CreateTurnCore's own sessions.Get, when the caller
	// went through that wrapper) and this lock is the only way this can
	// miss (vanishingly rare, and 404 is still the right answer either
	// way).
	if _, err := sessions.WithTx(tx).GetActorEpochForUpdate(ctx, sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusNotFound, Message: "session not found"}
		}
		logger.Error("httpapi: lock session row failed", "error", err)
		return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusInternalServerError, Message: "internal error"}
	}

	// Policy-gated open-turn/busy check (skipped entirely for AlwaysQueue --
	// see CreateTurnPolicy's own doc comment). Finding 3 of this batch's own
	// follow-up fix moved this check ahead of the awaiting-plan gate below
	// -- see createTurnLocked's own top doc comment for why: whenever a
	// turn is already open/in-flight, the busy reply this produces is
	// always the more accurate message, regardless of the session's plan
	// state.
	if policy != AlwaysQueue {
		existingTurns, err := turns.WithTx(tx).ListForSession(ctx, sessionID)
		if err != nil {
			logger.Error("httpapi: list turns failed", "error", err)
			return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusInternalServerError, Message: "internal error"}
		}
		if hasOpenTurn(existingTurns) {
			if policy == DropIfOpen {
				return sqlcgen.Turn{}, false, nil
			}
			return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusConflict, Message: "a turn is already pending, dispatched, or processing for this session"}
		}
	}

	// Awaiting-plan gate (this batch's own follow-up fix, §8.1; extended by
	// §23.2/§23.3): an ordinary (planMode == false) turn must never
	// dispatch while sessionID has a plan sitting in StatusAwaitingApproval
	// -- that plan is work a human has not yet approved, and BEFORE the
	// original fix, any reply matching neither plandomain.MatchVerdict nor
	// plandomain.MatchRevise silently fell through into exactly this
	// ordinary-turn path, starting unapproved work. Runs INSIDE this same
	// locked transaction (the session row is already held above), reusing
	// PlanStore.ListSummariesForSession -- the SAME minimal query
	// internal/adapters/inbound/linear's own findAwaitingApprovalPlanID
	// (webhook.go) already scans identically, so no new PlanStore method is
	// needed. A planMode == true turn (the request-changes flow, whether
	// reached via Slack's "Request changes" modal, Linear/Slack's revise:
	// prefix, or a web client setting planMode directly) is NEVER gated
	// here -- see CreateTurnPolicy's own doc comment for why plan_mode
	// turns stay unconditionally allowed. Only reached when the open-turn
	// check above did NOT already return (Finding 3): if a turn is already
	// open/in flight, that is always the more accurate reply, regardless of
	// whether sessionID also happens to have a plan awaiting approval right
	// now (the common overlap case being an in-flight revise turn itself,
	// which has not yet superseded the very plan row this gate would
	// otherwise still see as awaiting_approval).
	if !planMode && plans != nil {
		summaries, err := plans.WithTx(tx).ListSummariesForSession(ctx, sessionID)
		if err != nil {
			logger.Error("httpapi: list plan summaries for awaiting-plan gate failed", "error", err)
			return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusInternalServerError, Message: "internal error"}
		}
		for _, s := range summaries {
			if s.Status == sqlcgen.PlanStatusAwaitingApproval {
				// (§23.2/§23.3): consult the PRE-COMPUTED
				// plan_followup classification (answerOnly, computed above,
				// before tx.Begin) -- never re-run the classifier here,
				// inside the transaction/row lock. answerOnly == nil
				// (classification never ran, for any of the reasons listed
				// at that block's own doc comment) is treated EXACTLY like
				// answerOnly == true: fail open to this gate's own
				// pre-existing hold-and-clarify behavior (§23.3's own
				// floor -- "a classifier failure must never let a build
				// turn dispatch against an unapproved plan, under any
				// failure mode").
				if answerOnly == nil || *answerOnly {
					logger.Info("httpapi: ordinary turn creation blocked by awaiting-approval plan", "session_id", sessionID.String(), "plan_id", s.ID.String(), "answer_only_unknown", answerOnly == nil)
					return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusConflict, Message: planAwaitingApprovalMessage, sentinel: ErrPlanAwaitingApproval}
				}
				// answerOnly != nil && *answerOnly == false: a confident
				// "amend" verdict (§23.1) -- promote this turn to a REAL
				// plan-revision turn, exactly like a revise:-prefixed reply
				// already is (plandomain.RevisePrefix's own doc comment,
				// verdict.go: "a future Step is expected to replace
				// prefix-detection with a real amend-vs-answer LLM
				// classifier for the common case"). Reassigning planMode
				// itself (not a separate "effective" variable) so every
				// OTHER planMode-gated branch below in this SAME function
				// (the epistemic-preamble exclusion, the turns.plan_mode
				// column write) sees the SAME promoted value consistently,
				// with no second place that could disagree about it.
				logger.Info("httpapi: ordinary turn promoted to plan-revision turn by plan_followup classification", "session_id", sessionID.String(), "plan_id", s.ID.String())
				planMode = true
				break
			}
		}
	}

	// §25.6 ("workflow execution engine", §25.6): resolve which
	// WorkflowDefinition/StepDefinition governs this new turn, and use its
	// PromptTemplate/ModelID to build it -- internal/app/workflowengine's
	// own doc.go documents the full design and its fail-open contract in
	// detail. workflows is constructed fresh from pool here (rather than
	// threaded through as a new parameter to this function, like sessions/
	// turns/plans/auditLog above) specifically so this Step's own diff to
	// createTurnLocked/CreateTurnCore stays minimal: adding a required
	// parameter would cascade into every one of this core's own callers
	// (REST, Slack's addTurn/interactive.go, Linear's webhook.go,
	// GitHub's bot.go) across three different packages, none of which
	// otherwise need to change at all for this Step -- a materially larger
	// diff, in the single riskiest Step so far, for no behavioral benefit
	// (postgres.NewWorkflowStore is a trivial, side-effect-free wrapper
	// around the SAME pool/tx every other store here already uses).
	//
	// A failure reading the session row here (vanishingly unlikely --
	// GetActorEpochForUpdate above already just confirmed this exact row
	// exists and holds its lock for this whole transaction) degrades to
	// the engine's own safest fallback -- prompt/modelID dispatched
	// UNCHANGED, exactly as if this Step did not exist -- rather than
	// failing turn creation over what is fundamentally an engine
	// bookkeeping concern.
	// §8.6 ("uploads, blob storage & the in-sandbox download_file
	// tool", §28.5): validate attachmentIDs INSIDE this same locked
	// transaction -- every id must be a status='ready' upload artifact of
	// THIS session, else a structured 4xx; a failed or foreign upload can
	// never silently ride a prompt. artifacts is constructed fresh from
	// pool here for the SAME reason workflows is below (that block's own
	// doc comment): attachmentIDs's own variadic signature already keeps
	// every OTHER caller's diff at zero, and this lookup needs no new
	// parameter of its own either.
	var attachmentInfos []domainupload.AttachmentInfo
	if len(attachmentIDs) > 0 {
		artifacts := postgres.NewArtifactStore(pool).WithTx(tx)
		readyRows, err := artifacts.ListReadyUploadsByIDsForSession(ctx, sessionID, attachmentIDs)
		if err != nil {
			logger.Error("httpapi: list ready upload artifacts for attachmentIds validation failed", "error", err)
			return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusInternalServerError, Message: "internal error"}
		}
		byID := make(map[pgtype.UUID]sqlcgen.Artifact, len(readyRows))
		for _, row := range readyRows {
			byID[row.ID] = row
		}
		attachmentInfos = make([]domainupload.AttachmentInfo, 0, len(attachmentIDs))
		for _, id := range attachmentIDs {
			row, ok := byID[id]
			if !ok {
				return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusBadRequest, Message: "one or more attachmentIds are unknown, not this session's, or not yet ready"}
			}
			attachmentInfos = append(attachmentInfos, domainupload.AttachmentInfo{
				SessionID:   sessionID.String(),
				UploadID:    id.String(),
				Filename:    stringOrEmpty(row.Filename),
				SizeBytes:   int64OrZero(row.SizeBytes),
				ContentType: stringOrEmpty(row.ContentType),
			})
		}
	}

	effectivePrompt, effectiveModelID, effectiveEffort := prompt, modelID, effort
	// epistemicCheckOverride/epistemicCheckSessionKnown (
	// §20.2/§20.4): epistemicCheckSessionKnown starts false -- the same
	// safe, off-by-default fallback the sessErr != nil branch below already
	// applies to workflow-engine resolution, reused here rather than a
	// second, differently-reasoned fallback for a second concern that reads
	// the SAME session row -- so the MaybeInjectEpistemicPreamble call
	// below never even runs (effectivePrompt is left exactly as workflow-
	// engine resolution set it) when the session row could not be read.
	var epistemicCheckOverride *bool
	epistemicCheckSessionKnown := false
	workflows := postgres.NewWorkflowStore(pool).WithTx(tx)
	var resolution workflowengine.Resolution
	if sessionRow, sessErr := sessions.WithTx(tx).Get(ctx, sessionID); sessErr != nil {
		logger.Warn("httpapi: get session row for workflow engine resolution failed; dispatching unchanged", "error", sessErr)
	} else {
		resolution = workflowengine.ResolveStepForNewTurn(ctx, workflows, sessionRow, prompt, modelID, effort)
		effectivePrompt, effectiveModelID, effectiveEffort = resolution.Prompt, resolution.ModelID, resolution.Effort
		epistemicCheckOverride = sessionRow.EpistemicCheckEnabled
		epistemicCheckSessionKnown = true
	}

	// (§28.5): the attachment block (deterministic per-attachment
	// listing + download_file command) and the upload-tool note are
	// appended to the FULLY RESOLVED prompt -- after, never before,
	// workflowengine's own {{prompt}} template substitution above -- so
	// neither can end up captured mid-template by some future custom
	// workflow step whose own PromptTemplate wraps {{prompt}} in
	// unrelated surrounding text.
	//
	// FIX D (design-conformance, this batch's own follow-up fix): the two
	// blocks are now gated INDEPENDENTLY, not on the same condition --
	// an earlier version of this comment recorded the note's own
	// attachment-gating as a named, accepted gap ("telling the agent it
	// can produce a brand-new file on a turn with NO attachments at all"),
	// deferred pending "a real plan for threading a ... storage configured
	// ... signal through createTurnLocked" -- CreateTurnOptions.
	// StorageConfigured (this batch's own addition) is that plan.
	//
	// The attachment block STAYS gated on len(attachmentInfos) > 0 --
	// correct, unchanged: no attachments, no block, byte-for-byte no-op
	// preserved for every turn that never names one.
	//
	// The upload-tool note is now gated on opts[0].StorageConfigured
	// instead (§28.7's own feature flag -- the identical signal
	// mintUploadCore checks to answer "uploads not configured", never a
	// second, invented config knob), independent of whether THIS turn
	// happens to carry any attachments -- §28.5's own literal wording is
	// "surfaced to the agent ... in build-turn prompts", not "only on
	// turns that also attach a file". This DOES change prompt bytes for a
	// zero-attachment REST-created (web) turn once a deployment configures
	// object storage -- workflowengine_characterization_integration_test.go
	// and upload_integration_test.go's own TestCreateTurn_NoAttachments_*
	// tests were updated for this Step to match, since the change is
	// intentional and spec-mandated, not a regression.
	//
	// Why this does NOT reopen the byte-for-byte characterization
	// invariant those tests still protect: StorageConfigured lives on
	// CreateTurnOptions, this core's own trailing VARIADIC parameter (see
	// that type's own doc comment) -- every one of this core's five
	// OTHER callers (reviewretrigger.go's review turns, linear/webhook.go,
	// slack/turn.go, slack/interactive.go, bot.go's GitHub-bot turns) omits
	// it entirely, so storageConfigured is unconditionally false for all
	// of them regardless of deployment config, and the note never renders
	// on their turns at all -- preserving §28.5's own "build-turn prompts"
	// scoping (a review/bot/Slack/Linear turn is never a build turn a
	// human composed with attachments in mind) without any of those four
	// packages needing to learn anything about object storage. Only
	// CreateTurn's own REST handler (below) ever sets it, so the
	// characterization tests (which call CreateTurnCore directly, with no
	// trailing CreateTurnOptions at all) see storageConfigured == false
	// and keep their own existing zero-byte-added assertions intact
	// unconditionally, regardless of whether the TEST RIG's own objCfg
	// happens to be configured.
	if len(attachmentInfos) > 0 {
		effectivePrompt += domainupload.RenderAttachmentBlock(attachmentInfos)
	}
	if storageConfigured {
		effectivePrompt += domainupload.RenderUploadToolNote(sessionID.String())
	}

	// (§20.1/§20.3): the devil's-advocate preamble is PRECEDED --
	// prepended, never appended -- onto the turn's own FULLY assembled
	// prompt (after workflow-template resolution and the attachment/
	// upload-tool blocks above, so it is the very first thing the agent
	// reads), exactly when turn.MaybeInjectEpistemicPreamble decides to --
	// the ONE place §20.3's plan-mode exclusion is enforced, structural
	// and impossible to silently drop (that function's own doc comment).
	// F6 (adversarial review): this now calls the SAME shared helper
	// httpapi/create.go's CreateSessionOnTx, workflowengine's
	// dispatchNextAttempt, and DecidePlanOnTx also route through, rather
	// than each (including this, the ORIGINAL site) keeping its own
	// hand-written copy of the resolve/exclude/render/prepend sequence --
	// "duplication is exactly how the fifth site gets forgotten" (F6's own
	// words). epistemicCheckSessionKnown guards the call exactly like the
	// inline version this replaced did (skip entirely, never guess, when
	// the session row above could not be read). With epistemicCheckDefault
	// always false (feature off, the default, §20.4) and no session
	// override, this is always a no-op and effectivePrompt is UNCHANGED
	// versus every prior Step -- the required byte-for-byte no-op (pinned
	// by TestCreateTurnCore_EpistemicCheckOff_ByteForByteNoOp,
	// turn_integration_test.go).
	if epistemicCheckSessionKnown {
		effectivePrompt = turn.MaybeInjectEpistemicPreamble(epistemicCheckDefault, epistemicCheckOverride, planMode, effectivePrompt)
	}

	// correlationID (§12.2 item 1's own session-rail gap, migrations/
	// 000121_turns_correlation_id.up.sql): this request's own id, when the
	// ingress that dispatched it minted one.
	var correlationID *string
	if id, ok := platform.CorrelationIDFromContext(ctx); ok && id != "" {
		correlationID = &id
	}
	created, err := turns.WithTx(tx).Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:               sessionID,
		Status:                  sqlcgen.TurnStatusPending,
		Prompt:                  &effectivePrompt,
		ModelID:                 effectiveModelID,
		Effort:                  effectiveEffort,
		PlanMode:                planMode,
		ReviewHeadSha:           reviewHeadSHA,
		ReviewDepth:             reviewDepth,
		ReviewDepthDecision:     reviewDepthDecision,
		ReviewKnowledgeMode:     reviewKnowledgeMode,
		ReviewKnowledgeDecision: reviewKnowledgeDecision,
		// answerOnly (§23.2) is nil ("classification did not
		// apply") for every turn that predates this Step, or that never hit
		// the plan_followup block above -- see that block's own doc
		// comment for the full enumeration. By construction, the only real
		// value it can hold here is a pointer to false: the awaiting-plan
		// gate above already returned early (no row ever inserted) for
		// every case answerOnly points to true.
		AnswerOnly:    answerOnly,
		CorrelationID: correlationID,
	})
	if err != nil {
		logger.Error("httpapi: create turn failed", "error", err)
		return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusInternalServerError, Message: "internal error"}
	}
	// AttachTurn is a no-op unless ResolveStepForNewTurn actually created a
	// new workflow_step_runs attempt above (Resolution.Tracked) -- see that
	// function's own doc comment for the (deliberately untracked) cases
	// this correctly skips.
	workflowengine.AttachTurn(ctx, workflows, resolution, created.ID)

	if err := recordAuditLog(ctx, auditLog.WithTx(tx), actorUserID, "turn.create", "turn", created.ID.String(), map[string]any{
		"session_id": sessionID.String(),
		"plan_mode":  planMode,
	}); err != nil {
		logger.Error("httpapi: record turn.create audit log failed", "error", err)
		return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusInternalServerError, Message: "internal error"}
	}

	if err := tx.Commit(ctx); err != nil {
		logger.Error("httpapi: commit create-turn tx failed", "error", err)
		return sqlcgen.Turn{}, false, &CreateTurnError{Status: http.StatusInternalServerError, Message: "internal error"}
	}

	// Fire-and-forget, OUTSIDE the transaction above, exactly mirroring
	// CreateSession's own identical post-commit sequencing (create.go) --
	// see that handler's own doc comment for why this never blocks the
	// response on how long the resulting spawn/dispatch decision takes.
	actor, spawnErr := registry.GetOrSpawn(ctx, sessionID)
	if spawnErr != nil {
		logger.Warn("httpapi: GetOrSpawn after turn create failed", "error", spawnErr)
	} else if sendErr := actor.Send(ctx, sessionactor.EnsureDispatched{}); sendErr != nil {
		logger.Warn("httpapi: send EnsureDispatched after turn create failed", "error", sendErr)
	}

	return created, true, nil
}
