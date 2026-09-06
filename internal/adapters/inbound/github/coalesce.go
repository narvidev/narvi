package github

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/app/intentclassifier"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewtriage "github.com/narvidev/narvi/internal/app/reviewtriage"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/authz"
	intentdomain "github.com/narvidev/narvi/internal/domain/intent"
	"github.com/narvidev/narvi/internal/platform"
)

// intentClassifierSurface is the sessions.spawn_source value (§18.1's
// IntentClassifierInput.Surface / §18.4's IntentDecisionRecord.Surface)
// this package's own mentions are classified/recorded under.
const intentClassifierSurface = "github"

// ErrActorNotAuthorized is CreateOrJoin's own sentinel for "a resolved,
// linked commenter's role failed domain/authz.Authorize" -- batch
// fix/audit-github-actor-rbac's own addition (see identity.go's own top
// doc comment for the full finding this closes). Deliberately DISTINCT
// from every other error CreateOrJoin returns: handler.go checks for this
// one specifically and responds 200 without releasing the claimed webhook
// delivery for a GitHub redelivery retry -- retrying a denied comment
// changes nothing (the SAME actor would be denied again), unlike every
// OTHER CreateOrJoin error (a transient Postgres failure, say), which
// SHOULD be retried via GitHub's own redelivery mechanism.
var ErrActorNotAuthorized = errors.New("github: actor not authorized")

// ErrRolloutNotEnrolled is CreateOrJoin's own sentinel for "the WINNER
// path's own httpapi.CreateSessionOnTx call refused because a named repo
// is not enrolled in §10's cohort rollout" (§10 Phase 6, §32:
// CreateSessionError.RolloutRefusal, checked structurally, never by
// string-matching cerr.Message). Deliberately DISTINCT from every other
// CreateOrJoin error, mirroring ErrActorNotAuthorized's own identical
// shape immediately above: this is a PERMANENT policy refusal (retrying
// the same GitHub redelivery would reproduce the identical refusal every
// time, since repo_settings.sessions_enabled does not change between
// redeliveries of the SAME webhook payload), so handler.go checks for
// this one specifically and takes §32's own permanent-denial idiom:
// acknowledge (200) WITHOUT releasing the claimed webhook delivery (a
// release would let GitHub redeliver into this exact same refusal
// forever), and post NO reply on the PR thread at all -- §32's own "an
// unenrolled repo must receive zero platform egress" requirement, one
// step stricter than ErrActorNotAuthorized's own unlinked-actor branch
// (which DOES post an actionable reply): an unenrolled repo has no
// action a commenter could even take to fix this themselves, so there is
// nothing honest to tell them beyond silence.
var ErrRolloutNotEnrolled = errors.New("github: repo not enrolled in cohort rollout")

// SessionCoalescer bundles the stores/registry CreateOrJoin needs -- a
// small struct rather than a long positional-parameter list, constructed
// once at wiring time (cmd/control-plane/main.go), mirroring this
// codebase's own construct-once-thread-through convention for every other
// store/registry pair. Environments is only here because
// httpapi.CreateSessionOnTx's own signature requires a
// *postgres.EnvironmentStore argument -- a GitHub-sourced
// restdtos.CreateSessionRequest never sets PathScope or MockConfig
// (handler.go never populates either), so the CreateSessionOnTx call below
// never actually exercises the environment-insert branch for any request
// this package ever hands it; Environments is simply threaded through
// unused on this path.
//
// IntentClassifier is §8.3's own wiring point (§8.3/§18): classify+
// record runs ONCE, on the WINNER (brand-new session) path only -- see
// CreateOrJoin's own doc comment below for why the REUSE path never
// re-classifies. Optional (nil-safe): a nil IntentClassifier simply skips
// classification entirely, so existing tests/wiring that don't care about
// this Step keep working unchanged.
type SessionCoalescer struct {
	Pool             *pgxpool.Pool
	PRSessions       *postgres.GitHubPRSessionStore
	Sessions         *postgres.SessionStore
	Turns            *postgres.TurnStore
	Environments     *postgres.EnvironmentStore
	Registry         *sessionactor.Registry
	IntentClassifier *intentclassifier.Service

	// Plans (a follow-up fix, §8.1) is threaded through to the
	// REUSE path's own httpapi.CreateTurnForBot call below, exactly like
	// every other createTurnLocked caller now gets -- see that function's
	// own doc comment (httpapi/turn.go) for the nil-safe "skips the
	// awaiting-plan gate" contract a nil value here keeps.
	Plans *postgres.PlanStore

	// AuditLog is §13.2's own addition (§13.3): threaded through to the
	// WINNER path's own httpapi.CreateSessionOnTx call below, exactly like
	// Environments already is, so a GitHub-originated session creation
	// gets the SAME audit_log row every other CreateSessionOnTx caller now
	// gets. actor_user_id is NULL only until batch fix/audit-github-actor-
	// rbac's own commenter-identity resolution (identity.go) resolves a
	// real user -- otherwise it carries that resolved user_id, exactly
	// mirroring created_by's own identical convention below.
	AuditLog *postgres.AuditLogStore

	// Identities/Users/Participants are batch fix/audit-github-actor-rbac's
	// own additions, closing the H4 audit finding that GitHub ingress never
	// gated session/turn creation behind domain/authz.Authorize at all
	// (Slack/Linear ingress already do). Identities backs
	// handler.go's own resolveCommenterActor (identity.go) -- a direct
	// (provider, external_id) lookup, no auto-linking algorithm needed (see
	// that file's own doc comment for why). Users/Participants are exactly
	// the SAME two collaborators actorauthz.AuthorizeLinkedActor (batch
	// fix/deny-unlinked-github-actors; formerly AuthorizeResolvedActor)/
	// actorauthz.OwnedOrJoined need, mirroring Slack's/Linear's own
	// Deps.IdentityLink.Users / Deps.Participants precedent -- production
	// wiring (cmd/control-plane/main.go) passes the SAME userStore/
	// participantStore/identityStore instances every other caller already
	// uses, never a second, independently-constructed copy of any of them.
	Identities   *postgres.IdentityStore
	Users        *postgres.UserStore
	Participants *postgres.ParticipantStore

	// F7 correction (adversarial review): this struct used to
	// carry its own EpistemicCheckDefault bool field, threaded through to
	// the REUSE path's own httpapi.CreateTurnForBot call below, with a doc
	// comment claiming that matched "every other createTurnLocked-reaching
	// caller" in this codebase. That claim was already false when written
	// (the functionally identical REST path, reviewretrigger.go, has
	// always deliberately hardcoded false instead, with its own documented
	// rationale) -- passing the real, operator-configured default there
	// let the builder-only devil's-advocate preamble (§20) get prepended
	// in front of a PR REVIEW turn's own review.RenderTurnPrompt
	// verdict-tool block, handing the review agent two competing
	// structured-reporting endpoints and polluting turns.epistemic_outcome
	// (gated only on plan_mode, never on "is this actually a build turn")
	// with review-turn rows.
	//
	// The fix is not a corrected field -- it is TWO hardcoded `false`
	// literals, at both this package's own httpapi.CreateSessionOnTx
	// (WINNER path, below) and httpapi.CreateTurnForBot (REUSE path,
	// below) call sites, each with its own doc comment mirroring
	// reviewretrigger.go's exact reasoning: internal/adapters/inbound/
	// github is EXCLUSIVELY a PR-review ingress (CreateOrJoin's own doc
	// comment; every session this package ever creates or joins is a
	// review session, verified via handler.go's own DeterministicTarget:
	// intentdomain.TargetReview), so there is no live case where this
	// package should ever consult the platform's real epistemic-check
	// default -- keeping a field for a value neither call site may
	// legitimately use would just be the next version of this exact bug,
	// waiting for a future edit to "fix" the field back into use. Removed
	// entirely rather than left unread; cmd/control-plane/main.go no
	// longer sets it either.

	// ReviewTriage (§26.3) bundles the two stores internal/app/
	// reviewtriage.ComputeDecision needs (repo_settings, for the
	// per-repo reviewDepth config; review_verdicts, for the "prior high
	// verdict" signal) -- constructed once at wiring time (cmd/control-
	// plane/main.go), mirroring every other Deps-shaped field this
	// struct already carries. Consulted on BOTH the WINNER (brand-new
	// session) and REUSE (existing session, new turn) paths below: every
	// review turn gets its own fresh depth decision, not just a
	// session's first one.
	ReviewTriage appreviewtriage.Deps
	// ReviewModelDeep (§26.3) is platform.Config.ReviewModelDeep,
	// threaded through for domainreviewtriage.ModelAndEffort -- empty
	// means "not configured", see that function's own doc comment
	// (internal/domain/reviewtriage/modeleffort.go).
	ReviewModelDeep string

	// RolloutMode/RepoSettings (§10 Phase 6, §32) are threaded
	// through to the WINNER path's own httpapi.CreateSessionOnTx call
	// below, exactly like AuditLog/Environments already are -- both are
	// REQUIRED parameters of that function (its own doc comment), so a
	// zero-value RolloutMode here is indistinguishable from
	// rollout.ModeOpen (the safe default) rather than an accidental gap.
	// A DIFFERENT, DEDICATED field from ReviewTriage.RepoSettings above,
	// even though both ultimately point at the same
	// *postgres.RepoSettingsStore in production wiring (cmd/control-
	// plane/main.go): ReviewTriage is a nested, review-domain-specific
	// dependency bundle that could reasonably be refactored or dropped
	// without anyone noticing this UNRELATED rollout-gating concern
	// silently broke along with it.
	RolloutMode  platform.RolloutMode
	RepoSettings *postgres.RepoSettingsStore
}

// CreateOrJoin is §8.2's own per-PR coalescing entry point -- see
// doc.go's own "Per-PR coalescing design" section for the full two-step
// atomic-claim sequencing this implements. isNewSession reports which
// branch was taken (true: req was used to create a brand-new review
// session; false: an existing session for this PR was reused and only a
// new turn was enqueued on it) -- callers use it purely for
// logging/observability, never for a different response to GitHub (both
// branches ack 200 identically).
//
// # Connection-pool safety note (why the WINNER path does NOT call
// httpapi.CreateSessionForBot)
//
// This function holds ONE claim-row lock (LockForUpdate below) inside ONE
// transaction (tx) for its own entire winner-path critical section. If
// that critical section called httpapi.CreateSessionForBot -- which opens
// its OWN, separate transaction via *pgxpool.Pool.Begin -- a single
// request would need TWO simultaneous connections out of the SAME pool:
// one held open by tx (this function's own claim transaction) and one
// acquired by CreateSessionForBot's own inner Begin. Under enough
// concurrent @mentions on the SAME PR (enough that every OTHER, losing
// goroutine's own LockForUpdate call has also already acquired a
// connection and is parked waiting on Postgres's own row lock), the pool
// could be fully exhausted by parked losers by the time the winner tries
// to acquire ITS OWN second connection -- a genuine connection-pool
// deadlock (nothing can release a connection until the winner commits,
// and the winner cannot commit until it acquires a second connection that
// will never come). This is NOT hypothetical: pgxpool's default MaxConns
// is a small, fixed number (independent of this request's own
// concurrency), so it is the wrong assumption to lean on "the pool
// probably has enough spare capacity".
//
// The fix: the winner path below calls httpapi.CreateSessionOnTx directly,
// INLINE on the SAME tx/connection the claim lock already holds -- never a
// second connection. CreateSessionOnTx is the shared, exported piece of
// CreateSessionCore's own logic (internal/adapters/inbound/httpapi/
// create.go) that takes an ALREADY-OPEN transaction the caller owns
// entirely, built for exactly this "already holding an unrelated lock on
// my own open transaction" shape -- so this package no longer needs to
// hand-duplicate any repo-validation/session-insert/turn-insert logic of
// its own to get the same never-a-second-connection guarantee.
// httpapi.CreateSessionForBot itself is untouched and still
// exported/tested (bot.go) as a general-purpose, no-coalescing entry
// point for a caller that is NOT simultaneously holding a claim-row lock
// (e.g. a future Slack/Linear ingress path with no per-thread coalescing
// of its own).
//
// The REUSE (loser) path below has no such risk: it commits tx BEFORE
// calling httpapi.CreateTurnForBot, so only ever one connection is open
// at a time there too.
//
// # REUSE-branch authz action: ActionPromptSession vs. ActionRetriggerReview
// # (audit fix, §13.3 row 5)
//
// The REUSE branch below is reached by TWO structurally different
// triggers landing on an already-tracked PR: an ordinary second @mention
// (just prompting the existing review session -- "what did you mean by
// X") and §8.2's own manual re-trigger-via-LABEL lane (payload.go's
// parsePullRequestLabeled). Both used to render the identical
// authz.ActionPromptSession verdict (member allowed on own/joined), which
// let a member re-trigger a review on any session they created/joined --
// but §13.3 row 5 ("re-trigger reviews") is admin/maintainer only, with NO
// member own/joined carve-out, unlike row 2's ordinary prompt/create
// carve-out ActionPromptSession correctly implements. isLabelRetrigger
// (threaded from mention.IsLabelRetrigger, handler.go) selects the
// correct action for THIS call: ActionRetriggerReview for a label
// re-trigger, ActionPromptSession (unchanged) for an ordinary @mention.
//
// # actor / domain/authz.Authorize gating (batch fix/audit-github-actor-rbac;
// # hardened to a DENY by batch fix/deny-unlinked-github-actors)
//
// actor is handler.go's own already-resolved commenter (identity.go's
// resolveCommenterActor) -- Valid iff this exact GitHub commenter already
// has a linked Narvi account, invalid otherwise, exactly mirroring Slack's/
// Linear's own resolved-actor precedent (§13.2).
//
// An invalid actor is now DENIED outright by BOTH authorization checks
// below (actorauthz.AuthorizeLinkedActor's own `!actorUserID.Valid ->
// false` short-circuit) -- a deliberate, repo-owner-decided reversal of
// this package's own PRIOR behavior (bot attribution: an unresolved
// commenter's action proceeded, gated by nothing). See
// AuthorizeLinkedActor's own doc comment (internal/app/actorauthz/
// authorize.go) for the full "why GitHub can now use this function, unlike
// when it was first written" reasoning, and handler.go's own
// ErrActorNotAuthorized branch for the actionable "please sign in" reply
// this denial now comes with (GitHub has no magic-link/pending-link
// mechanism to send in parallel the way Slack/Linear do, so a plain deny
// with no explanation would leave the commenter with nothing -- see
// actornotauthorizedreply.go's own doc comment).
//
// The WINNER path's own domain/authz.Authorize(ActionCreateSession) check
// (createAuthorized below) is deliberately resolved BEFORE tx.Begin, never
// inside the open claim transaction: actorauthz.AuthorizeLinkedActor
// performs its own Postgres read (users.GetByID) when actor IS resolved,
// and acquiring a SECOND pool connection while already holding tx open is
// exactly the connection-pool exhaustion risk this function's own
// "connection-pool safety note" above already goes to lengths to avoid
// for httpapi.CreateSessionForBot -- the same discipline applies here:
// resolve it once, cheaply, with no ambient transaction, then just
// consult the already-computed bool once inside the critical section (no
// query, no risk). The REUSE path's own domain/authz.Authorize check
// (below, ownership-aware for ActionPromptSession, admin/maintainer-only
// for ActionRetriggerReview -- see this function's own "REUSE-branch authz
// action" doc comment above) runs AFTER that path's own tx.Commit -- by
// then no transaction is open at all, so there is nothing to protect there
// either. Denying here (either path) leaves the claim row exactly as safe
// as an authorized denial always was -- see this function's own "claim row
// on the deny path" note further down, at each denial site, for why.
//
// isLabelRetrigger (mention.IsLabelRetrigger, handler.go) selects the
// REUSE branch's own authz action (see this function's own "REUSE-branch
// authz action" doc comment above) -- ignored entirely on the WINNER path,
// which always renders ActionCreateSession regardless of trigger kind (a
// label event creating a brand-new session is still a create, never a
// re-trigger of an existing review).
//
// classifyText is the text §8.3's own intent classifier call (WINNER
// path only, further down) classifies -- the mention's own ORIGINAL,
// un-enriched comment/command text (handler.go captures this BEFORE
// folding the pre-fetched diff/stack context into req.Prompt via
// review.RenderTurnPrompt). Audit fix: this used to be *req.Prompt
// directly, which by the time CreateOrJoin ran already had §8.2's own
// inline pre-fetched diff (up to several MB) appended -- feeding the
// classifier's LLM call the entire PR diff instead of just the triggering
// comment/label text, inflating cost/latency by orders of magnitude and
// risking exceeding the model's context window the moment this classifier
// is switched from shadow to active (§18.5). classifyText decouples the
// two: req.Prompt still carries the full context-enriched text for the
// turn itself, classifyText carries only the human's own words, exactly
// matching IntentClassifierInput.Text's own documented contract ("a
// session's initial prompt, a Slack message, a GitHub comment body").
//
// F1 (§23 follow-up fix, review Finding 1): this SAME raw text is now
// ALSO threaded through to the REUSE branch's own httpapi.CreateTurnForBot
// call below (its classifyText parameter), for the identical reason --
// that call's own `prompt` local is built from req.Prompt too, so without
// this it would classify the SAME diff-enriched text against the
// plan_followup category (ClassifyPlanFollowup, gated on an
// awaiting-approval plan) that §8.3's classifier was already fixed to
// avoid. Reused verbatim, never recaptured: both categories classify the
// EXACT same raw mention text.
//
// reviewHeadSHA is the commit
// SHA handler.go's own reviewcontext.Fetch call just anchored req.Prompt's
// own pre-fetched diff to (empty when that fetch failed/never ran) --
// threaded through to whichever of the two branches below actually
// creates the new turn (ChildSessionOptions.ReviewHeadSHA on the WINNER
// path's CreateSessionOnTx call, or a direct parameter on the REUSE
// path's CreateTurnForBot call), so it lands on THAT turn's own row
// (turns.review_head_sha) at creation time -- see that column's own
// migration doc comment for the full "why".
//
// reviewDepth/triageModelID/triageEffort/triageRecordJSON (
// §26.3) are the ALREADY-RESOLVED light/deep routing outcome -- computed
// by THIS function's own caller, handler.go, via appreviewtriage.
// ComputeDecision (plus domainreviewtriage.Floor/ModelAndEffort/
// NewDecisionRecord), BEFORE handler.go calls review.RenderTurnPrompt on
// req.Prompt and BEFORE this function is ever invoked (adversarial-review
// fix D2: "deep-path digest requirement contradicts the prompt the agent
// actually receives"). This function used to compute this itself, inline,
// AFTER handler.go had already rendered the prompt -- which meant the
// text an agent actually read could never honestly reflect the depth this
// function was about to persist. CreateOrJoin no longer computes triage
// at all: it just persists what its caller already decided, applied
// identically to BOTH the WINNER and REUSE branches below, mirroring
// reviewHeadSHA's own identical "resolved upstream, just threaded through
// and persisted here" shape. See handler.go's own call site for the full
// "why here, once, before rendering" reasoning, and for why applying the
// SAME (unfloored-for-WINNER, floored-for-REUSE) value to both branches
// uniformly is safe: review_verdicts can only ever carry a row for a PR
// that already has a github_pr_sessions claim row (every verdict-posting
// turn is created via one of this package's own two branches, or the
// manual/auto-retrigger lanes, all of which require an EXISTING claim
// row), so the WINNER branch (no existing claim row) can never actually
// have a prior verdict/depth to floor against in practice -- Floor(fresh,
// "") is a no-op by construction (domainreviewtriage.Floor's own doc
// comment) regardless.
func (c *SessionCoalescer) CreateOrJoin(ctx context.Context, repoFullName string, prNumber int32, req restdtos.CreateSessionRequest, actor pgtype.UUID, isLabelRetrigger bool, classifyText string, reviewHeadSHA string, reviewDepth *string, triageModelID *string, triageEffort *string, triageRecordJSON []byte) (session sqlcgen.Session, turn sqlcgen.Turn, isNewSession bool, err error) {
	var reviewHeadSHAPtr *string
	if reviewHeadSHA != "" {
		reviewHeadSHAPtr = &reviewHeadSHA
	}
	logger := platform.Logger(ctx)
	reviewDepthPtr := reviewDepth

	// Resolved BEFORE any transaction opens -- see this function's own
	// doc comment above for why. Only actually consulted by the WINNER
	// branch below (Resource{}: creating a session has no ownership
	// concept); the REUSE branch renders its OWN, ownership-aware
	// ActionPromptSession verdict further down instead, since a member
	// who may always create a session might still lack the "own/joined"
	// carve-out ActionPromptSession requires for the SAME actor against a
	// DIFFERENT, already-existing session.
	createAuthorized := actorauthz.AuthorizeLinkedActor(ctx, logger, authzSurface, c.Users, actor, authz.ActionCreateSession, authz.Resource{})

	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return sqlcgen.Session{}, sqlcgen.Turn{}, false, fmt.Errorf("github: begin claim tx: %w", err)
	}
	committed := false
	// Rollback is a safety net for every return path other than a
	// successful Commit below -- mirrors httpapi's own identical pattern
	// (create.go, turn.go, bot.go).
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	txPRSessions := c.PRSessions.WithTx(tx)
	if err := txPRSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		return sqlcgen.Session{}, sqlcgen.Turn{}, false, fmt.Errorf("github: ensure claim row: %w", err)
	}

	// Locks the claim row for the rest of THIS transaction -- any
	// concurrent caller's own EnsureRow+LockForUpdate for the SAME
	// (repoFullName, prNumber) blocks here until this transaction commits
	// or rolls back. See migrations/000028_github_pr_sessions.up.sql's own
	// doc comment for the full reasoning.
	existing, err := txPRSessions.LockForUpdate(ctx, repoFullName, prNumber)
	if err != nil {
		return sqlcgen.Session{}, sqlcgen.Turn{}, false, fmt.Errorf("github: lock claim row: %w", err)
	}

	if existing.Valid {
		// Reuse case: this PR already has a review session. Nothing to
		// write to the claim row itself -- commit now (releasing the
		// lock, and this transaction's own connection, for whoever, if
		// anyone, is still queued behind it) BEFORE doing the SEPARATE,
		// independent work of enqueuing a new turn on the existing
		// session. Only one connection is ever open at a time on this
		// path.
		if err := tx.Commit(ctx); err != nil {
			return sqlcgen.Session{}, sqlcgen.Turn{}, false, fmt.Errorf("github: commit claim tx (reuse path): %w", err)
		}
		committed = true

		// No transaction open from here on -- see this function's own top
		// doc comment for why the ownership-aware ActionPromptSession check
		// deliberately runs here, post-commit, rather than inside the
		// critical section above.
		existingSession, err := c.Sessions.Get(ctx, existing)
		if err != nil {
			return sqlcgen.Session{}, sqlcgen.Turn{}, false, fmt.Errorf("github: get existing session: %w", err)
		}

		// actorauthz.AuthorizeLinkedActor is now called UNCONDITIONALLY --
		// batch fix/deny-unlinked-github-actors removed the previous
		// "if actor.Valid" guard that used to skip this whole block for an
		// unresolved commenter (bot attribution, the pre-batch behavior).
		// AuthorizeLinkedActor's own `!actorUserID.Valid -> false`
		// short-circuit now does the denying for that case, exactly
		// mirroring Slack's/Linear's own authorizeSessionAction call
		// (§13.2's hardened "a not-yet-linked identity's state-changing
		// action is denied" precedent) -- see this function's own top doc
		// comment for the full "why" and handler.go's own
		// ErrActorNotAuthorized branch for the reply this now triggers.
		//
		// OwnedOrJoined's own Participants read is still only performed
		// when actor IS valid: an invalid actor is denied by
		// AuthorizeLinkedActor regardless of what Resource.OwnedOrJoined
		// holds, so computing it for an unresolved commenter would just be
		// a wasted Postgres read on what is, today, still the common case
		// -- the SAME "no query, no risk" discipline this function's own
		// top doc comment already applies to createAuthorized above,
		// simply preserved here rather than lost when the guard came out.
		// reuseAction (audit fix, §13.3 row 5 -- see this function's own
		// "REUSE-branch authz action" doc comment above): a label re-trigger
		// on an already-tracked PR is §13.3's admin/maintainer-only
		// "re-trigger reviews" row, never the member-on-own/joined
		// ActionPromptSession an ordinary second @mention correctly renders.
		reuseAction := authz.ActionPromptSession
		if isLabelRetrigger {
			reuseAction = authz.ActionRetriggerReview
		}

		// joined is only ever CONSULTED by Authorize for ActionPromptSession
		// (ActionRetriggerReview has no allowIfOwned entry at all,
		// domain/authz/authorize.go) -- skip the Participants read entirely
		// for a label re-trigger, the SAME "no query, no risk" discipline
		// this function's own top doc comment already applies to
		// createAuthorized above.
		var joined bool
		if actor.Valid && reuseAction == authz.ActionPromptSession {
			var err error
			joined, err = actorauthz.OwnedOrJoined(ctx, c.Participants, existingSession, actor)
			if err != nil {
				return sqlcgen.Session{}, sqlcgen.Turn{}, false, fmt.Errorf("github: check participant for authorization: %w", err)
			}
		}
		if !actorauthz.AuthorizeLinkedActor(ctx, logger, authzSurface, c.Users, actor, reuseAction, authz.Resource{OwnedOrJoined: joined}) {
			logger.Warn("github: prompt/retrigger on existing session denied by authz", "session_id", existingSession.ID, "repo", repoFullName, "pr_number", prNumber, "user_id", actor.String(), "action", string(reuseAction))
			return sqlcgen.Session{}, sqlcgen.Turn{}, false, ErrActorNotAuthorized
		}

		var prompt string
		if req.Prompt != nil {
			prompt = *req.Prompt
		}
		// c.AuditLog/actor (audit-fix batch addition, H7): CreateTurnForBot
		// now writes the SAME turn.create audit_log row every other
		// createTurnLocked caller does, inside its own transaction -- actor
		// is the SAME already-resolved commenter identity passed to the
		// authz checks above (Valid iff linked, invalid/bot-attributed
		// otherwise), never a second, independently-resolved actor.
		//
		// epistemicCheckDefault: hardcoded false (F7, adversarial review),
		// mirroring reviewretrigger.go's own identical, deliberate
		// carve-out -- see SessionCoalescer's own doc comment (the removed
		// EpistemicCheckDefault field, above) for the full "why". This
		// REUSE branch always creates a turn on an EXISTING PR
		// review session (a second @mention or a review-label re-trigger),
		// never a build turn; passing the real platform default here would
		// prepend the builder-only devil's-advocate preamble in front of
		// review.RenderTurnPrompt's own verdict-tool block.
		//
		// classifyText (F1, §23 follow-up fix): &classifyText, the SAME
		// raw, un-enriched mention text the WINNER path's own
		// ClassifyAndRecord call below uses -- see this function's own doc
		// comment on the classifyText parameter for the full "why" this
		// must never be `prompt` (which, unlike here, already carries
		// review.RenderTurnPrompt's own folded-in diff/stack/verdict-tool
		// text once cfg.DiffFetcher is wired).
		// triageModelID/triageEffort (§26.3): a GitHub-sourced
		// req never sets ModelId itself (this package's own request-
		// building code, handler.go, never populates it), so the
		// triage-computed override is the only model/effort signal this
		// REUSE-path turn ever gets -- light leaves both nil (today's
		// unchanged behavior), deep forces high effort (and, when
		// c.ReviewModelDeep is configured, a specific frontier model).
		createdTurn, err := httpapi.CreateTurnForBot(ctx, c.Pool, c.Sessions, c.Turns, c.Plans, c.IntentClassifier, c.AuditLog, c.Registry, existing, prompt, triageModelID, req.PlanMode, false, actor, reviewHeadSHAPtr, &classifyText, triageEffort, reviewDepthPtr, triageRecordJSON)
		if err != nil {
			return sqlcgen.Session{}, sqlcgen.Turn{}, false, fmt.Errorf("github: create turn on existing session: %w", err)
		}

		logger.Info("github: coalesced mention onto existing review session",
			"session_id", existingSession.ID, "turn_id", createdTurn.ID, "repo", repoFullName, "pr_number", prNumber)
		return existingSession, createdTurn, false, nil
	}

	// Winner case: still holding the claim row lock (this transaction is
	// uncommitted). createAuthorized was already resolved, with no ambient
	// transaction, before this function even opened tx -- see this
	// function's own top doc comment for why -- so denying here needs no
	// further query at all: just roll back (the deferred Rollback above
	// handles it, since committed is still false) and report the denial.
	//
	// Claim row on the deny path (batch fix/deny-unlinked-github-actors,
	// traced explicitly, not assumed, since this path now denies far more
	// often than before): EnsureRow's own INSERT (above) ran INSIDE this
	// same uncommitted tx, so the Rollback this !createAuthorized branch
	// triggers undoes it too -- if this was the very first claim attempt
	// for this (repoFullName, prNumber), a denied WINNER leaves NO claim
	// row behind at all, not an orphaned one. A concurrently blocked
	// second caller parked on this SAME PR's own LockForUpdate (a
	// different commenter's mention, say) proceeds exactly as if the
	// denied attempt never happened, and gets its own independent,
	// fair authorization verdict -- no fix needed here, the existing
	// transaction boundary already makes this safe.
	if !createAuthorized {
		logger.Warn("github: create-session denied by authz", "repo", repoFullName, "pr_number", prNumber, "user_id", actor.String())
		return sqlcgen.Session{}, sqlcgen.Turn{}, false, ErrActorNotAuthorized
	}

	// Create the session AND its turn INLINE, on this SAME tx/connection,
	// via the shared httpapi.CreateSessionOnTx (see this function's own
	// "connection-pool safety" doc comment above for why NOT httpapi.
	// CreateSessionForBot here). createdBy is actor -- batch
	// fix/audit-github-actor-rbac's own change: Valid (a real Narvi
	// user_id, attributed exactly like the REST API/Slack/Linear already
	// attribute a resolved creator) iff this commenter is linked, still
	// the pgtype.UUID zero value (Valid == false, a genuine SQL NULL,
	// today's existing bot-attribution behavior) otherwise -- mirrors
	// Slack's resolveOrClaimSession / Linear's handleCreated passing their
	// own resolved creator through to CreateSessionCore identically.
	// epistemicCheckDefault: hardcoded false (F7, adversarial review) --
	// see SessionCoalescer's own doc comment (the removed EpistemicCheckDefault
	// field, above) for the full "why". This WINNER branch creates a
	// brand-new session's FIRST turn, and every
	// session this package ever creates is a PR review session (this
	// function's own doc comment; handler.go's own DeterministicTarget:
	// intentdomain.TargetReview below confirms it deterministically), so
	// this is never a build turn either, for the identical reason the
	// REUSE branch's own CreateTurnForBot call (below) hardcodes false.
	// triageModelID/triageEffort (§26.3): a GitHub-sourced req
	// never sets ModelId/Effort itself (this package's own request-
	// building code, handler.go, never populates either) -- overwriting
	// them here, on this function's own local copy of req, is therefore
	// exactly equivalent to CreateSessionOnTx's own turn insert (which
	// reads req.ModelId/req.Effort directly) picking up the triage-
	// computed override with no further plumbing. See the REUSE branch's
	// own identical comment above for the "light leaves both nil, deep
	// forces high effort" summary.
	req.ModelId = restdtos.CreateSessionRequestModelId(triageModelID)
	req.Effort = restdtos.CreateSessionRequestEffort(triageEffort)
	// prSessions (§31.4): the plain, pool-backed c.PRSessions --
	// NOT txPRSessions above -- exactly mirroring c.RepoSettings/c.AuditLog
	// immediately alongside it. req.SpawnSource is
	// restdtos.CreateSessionRequestSpawnSourceGithub for every request this
	// WINNER path builds, so CreateSessionOnTx's own
	// checkRepoEntitlementGate (repoentitlementgate.go) exempts it
	// unconditionally and never actually dereferences this parameter here
	// -- see that gate's own doc comment for exactly why re-deriving
	// entitlement from req.Repos[0].Url would be actively WRONG for this
	// path (a cross-repo/fork PR's own clone URL is deliberately the
	// fork, never repoFullName's own base/upstream claim key above), not
	// merely redundant. Still threaded through as the real store, never a
	// nil/fake stand-in: CreateSessionOnTx's own required-parameter
	// discipline treats an omittable gate dependency as an omitted gate,
	// and a future change to the exemption's own conditions must not find
	// a nil pointer waiting here.
	created, hasPrompt, cerr := httpapi.CreateSessionOnTx(ctx, tx, c.Sessions, c.Turns, c.Environments, c.AuditLog, req, actor, false, c.RolloutMode, c.RepoSettings, c.PRSessions, httpapi.ChildSessionOptions{ReviewHeadSHA: reviewHeadSHAPtr, ReviewDepth: reviewDepthPtr, ReviewDepthDecision: triageRecordJSON})
	if cerr != nil {
		if cerr.RolloutRefusal {
			// §32's own permanent-denial idiom -- see ErrRolloutNotEnrolled's
			// own doc comment for the full "why" (handler.go acknowledges
			// without releasing the webhook-delivery claim, and stays
			// silent on the PR). The deferred Rollback above undoes
			// EnsureRow's own claim-row INSERT too (committed is still
			// false here) -- an unenrolled repo's own github_pr_sessions
			// row is left exactly as absent as it was before this attempt,
			// which is also why REST enrollment (confirmRepoKnown, httpapi/
			// reposettings.go) can never bootstrap itself for this repo --
			// see internal/app/seed/reposettings.go's own doc comment.
			return sqlcgen.Session{}, sqlcgen.Turn{}, false, ErrRolloutNotEnrolled
		}
		return sqlcgen.Session{}, sqlcgen.Turn{}, false, fmt.Errorf("github: create session: %w", cerr)
	}

	// A GitHub mention always carries a real comment body -- handler.go
	// always populates req.Prompt -- so hasPrompt is always true on this
	// path in practice; CreateSessionOnTx doesn't hand the inserted turn
	// row back directly, so it's fetched here, still INSIDE this same
	// uncommitted tx (WithTx(tx), not a fresh pool connection) and still
	// holding the claim-row lock -- the only turn that can possibly exist
	// for this brand-new session.ID at this point is the one
	// CreateSessionOnTx just inserted; no concurrent caller can have
	// enqueued a turn of its own onto this session yet, since SetSessionID
	// below (which is what makes this session visible to a concurrent
	// REUSE-path caller at all) hasn't even run yet, let alone committed.
	// Fetching this AFTER commit instead would be a genuine race: a
	// concurrent loser could observe the just-committed session_id and
	// enqueue its own turn before this function's own ListForSession call
	// ran, breaking the "exactly one turn" assumption below under real
	// concurrent load.
	var createdTurn sqlcgen.Turn
	if hasPrompt {
		turnRows, err := c.Turns.WithTx(tx).ListForSession(ctx, created.ID)
		if err != nil {
			return sqlcgen.Session{}, sqlcgen.Turn{}, false, fmt.Errorf("github: list turns for new session: %w", err)
		}
		if len(turnRows) != 1 {
			return sqlcgen.Session{}, sqlcgen.Turn{}, false, fmt.Errorf("github: expected exactly one turn for new session, got %d", len(turnRows))
		}
		createdTurn = turnRows[0]
	}

	if err := txPRSessions.SetSessionID(ctx, repoFullName, prNumber, created.ID); err != nil {
		return sqlcgen.Session{}, sqlcgen.Turn{}, false, fmt.Errorf("github: set claim session id: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return sqlcgen.Session{}, sqlcgen.Turn{}, false, fmt.Errorf("github: commit claim tx (winner path): %w", err)
	}
	committed = true

	// Fire-and-forget, OUTSIDE the transaction above, and ONLY if a
	// prompt/turn was actually created -- mirrors every other
	// CreateSessionOnTx caller's own post-commit TriggerDispatch
	// sequencing (create.go's own CreateSessionCore does the same,
	// gated on the same hasPrompt CreateSessionOnTx returned).
	if hasPrompt {
		httpapi.TriggerDispatch(ctx, c.Registry, created.ID)
	}

	// §8.3 ("intent classifier", §8.3/§18): classify + record ONCE, on
	// this winner (brand-new session) path only -- IntentDecisionRecord
	// is a per-SESSION record (§18.4), and every GitHub-originated session
	// is created exactly here, so there is no gap left by never
	// re-classifying on the REUSE path above (a later @mention on an
	// already-tracked PR reuses a session that already went through this
	// exact path once). Runs entirely OUTSIDE the transaction above (a
	// real outbound LLM call must never hold a Postgres transaction open,
	// mirroring ports.Notifier.Deliver's/ports.SourceControl.CreatePR's own
	// identical "network call always outside any tx" discipline), and
	// never blocks the ack response on its own outcome beyond this
	// synchronous call -- shadow mode (§18.5, the default for every
	// surface until explicitly configured active) means nothing downstream
	// yet consumes the recorded Target/Mode for real behavior regardless.
	if hasPrompt && c.IntentClassifier != nil && req.Prompt != nil {
		// classify+record is now the shared intentclassifier.Service.
		// ClassifyAndRecord (H9/L11 audit fix) -- see that method's own
		// doc comment for the full "why a single shared call" reasoning.
		// This site's own two genuine differences from Slack/Linear are
		// both still expressed here directly: DecidedAtStageCreate (this
		// package's own IntentClassifierInput never has a "first prompt"
		// stage; the full prompt is always in hand at session-create
		// time), and DeterministicTarget below.
		//
		// Text is classifyText, deliberately NOT *req.Prompt (audit fix,
		// §5.2/§18.5) -- see this function's own doc comment on the
		// classifyText parameter, above, for the full "why": req.Prompt is
		// this SAME mention text with §8.2's own inline pre-fetched
		// diff/stack context already folded in (handler.go, BEFORE
		// CreateOrJoin is ever called), which for a real PR can run to
		// several MB -- feeding that whole diff into the classifier's LLM
		// call on every new GitHub review session would inflate its
		// cost/latency by orders of magnitude and risk exceeding the
		// model's context window the moment this classifier is switched
		// from shadow to active.
		c.IntentClassifier.ClassifyAndRecord(ctx, created.ID, ports.IntentClassifierInput{
			Text:    classifyText,
			Surface: intentClassifierSurface,
			// DeterministicTarget IS a real, already-known signal here,
			// not an absent one: CreateOrJoin (this function) is only ever
			// reached via parseMention (handler.go/payload.go) resolving to
			// a genuine PR-scoped mention -- parseIssueComment explicitly
			// rejects a plain-issue comment (p.Issue.PullRequest == nil:
			// "A comment on a plain issue, not a PR -- §8.2 is PR review
			// only"), and parsePullRequestReviewComment's own event type
			// ("pull_request_review_comment") never fires for anything
			// other than a PR. So simply being on this code path at all --
			// regardless of which of the two event types produced it --
			// already deterministically means this mention landed on a
			// pull request, i.e. Target should be "review". This is
			// distinct from (and available strictly earlier than) the
			// existing-tracked-PR signal the REUSE path above has, which
			// never re-classifies anyway.
			DeterministicTarget: intentdomain.TargetReview,
		}, intentdomain.DecidedAtStageCreate)
	}

	logger.Info("github: created new review session for mention",
		"session_id", created.ID, "turn_id", createdTurn.ID, "repo", repoFullName, "pr_number", prNumber)
	return created, createdTurn, true, nil
}
