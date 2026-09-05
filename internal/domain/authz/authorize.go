package authz

import (
	"errors"
	"fmt"
)

// Actor is who is asking. UserID is carried purely for the caller's own
// convenience (e.g. attaching it to an audit-log row, or to a log line)
// — Authorize's own verdict depends ONLY on Role; two Actors with the
// same Role and a different UserID always get the identical verdict for
// the same (action, resource). Kept a plain string (not pgtype.UUID),
// mirroring internal/domain/plan.ID's own "adapter-independent" precedent
// (§11) — callers convert at the boundary.
type Actor struct {
	UserID string
	Role   Role
}

// Resource is the state-changing command's target, as far as Authorize
// needs to know to render a verdict — deliberately minimal. OwnedOrJoined
// is the ONE bit of context the matrix's own "own/joined" carve-out
// (§13.3 row 2) depends on: true iff the resource is a session the actor
// either created or has joined (a participants row exists) — the CALLER
// resolves this (a plain Postgres lookup, e.g. sessionRow.CreatedBy ==
// actor.UserID || participants.Exists(...)) before ever calling Authorize;
// this package does no I/O of its own (§11) and so cannot answer that
// question itself. Every Action without an own/joined carve-out ignores
// this field entirely — its zero value (false) is always the correct,
// safe thing to pass for those.
type Resource struct {
	OwnedOrJoined bool
}

// ErrForbidden is the sentinel every Authorize rejection wraps — mirrors
// internal/domain/turn.ErrIllegalTransition's own sentinel-plus-detail-
// struct shape exactly, so a caller can either check errors.Is(err,
// ErrForbidden) for a plain yes/no, or errors.As into *ForbiddenError for
// the full (actor, action) detail.
var ErrForbidden = errors.New("authz: forbidden")

// ForbiddenError is the detailed error Authorize returns for any (actor,
// action, resource) it rejects. Actor/Action are carried verbatim (never
// Resource — nothing this package's own callers do with the error needs
// to know OwnedOrJoined, and the (actor, action) pair alone is already
// enough to explain "why" in a log line or an HTTP 403 body).
type ForbiddenError struct {
	Actor  Actor
	Action Action
}

func (e *ForbiddenError) Error() string {
	return fmt.Sprintf("authz: forbidden: role %s may not %s", e.Actor.Role, e.Action)
}

func (e *ForbiddenError) Unwrap() error { return ErrForbidden }

// ErrUnknownAction is returned by Authorize for an Action not present in
// the matrix below — a caller bug (a typo'd/undeclared Action constant),
// never a legitimate "no" verdict, so it is a DISTINCT error, never
// wrapped by ErrForbidden: a caller that only checks errors.Is(err,
// ErrForbidden) to decide "403 vs 500" must not mistake this for a normal
// permission denial.
var ErrUnknownAction = errors.New("authz: unknown action")

// roleSet is a small set-of-Role helper, used only to build the matrix
// table below readably (roles(RoleAdmin, RoleMaintainer) reads as a list,
// not a map literal).
type roleSet map[Role]bool

func roles(rs ...Role) roleSet {
	set := make(roleSet, len(rs))
	for _, r := range rs {
		set[r] = true
	}
	return set
}

// actionRule is one matrix row: allow is the set of roles permitted
// unconditionally; allowIfOwned is an ADDITIONAL set of roles permitted
// only when the caller's own Resource.OwnedOrJoined is true — nil for
// every action with no own/joined carve-out (which is most of them; only
// ActionPromptSession/ActionApprovePlan, per §13.3 row 2, and
// ActionDecideWorkflowStep, per §25.11's "same row as ActionApprovePlan",
// set it).
type actionRule struct {
	allow        roleSet
	allowIfOwned roleSet
}

// matrix is the literal §13.3 permission table, as data — see doc.go for
// the full table and design rationale. Every Action in action.go's
// AllActions has exactly one entry here (proven by
// TestMatrix_CoversEveryAction in authorize_test.go); Authorize's own
// ErrUnknownAction path is a defensive fallback for an Action that is
// somehow NOT in AllActions (should be unreachable in practice, mirrors
// internal/domain/turn's own "default case is unreachable dead-code
// protection" precedent), not something a well-formed caller ever hits.
var matrix = map[Action]actionRule{
	// Row 1: view sessions/analytics -- everyone, including viewer
	// (read-only, but there is no separate write verb for either of
	// these to withhold from a viewer).
	ActionViewSessions:  {allow: roles(RoleAdmin, RoleMaintainer, RoleMember, RoleViewer)},
	ActionViewAnalytics: {allow: roles(RoleAdmin, RoleMaintainer, RoleMember, RoleViewer)},
	// ActionViewOwnProfile (§12.2 item 7, GET /api/me) -- same row,
	// same reasoning: see action.go's own doc comment on why this is NOT
	// ActionLinkChatGPTAccount's own (admin/maintainer + owned-member,
	// viewer excluded) row.
	ActionViewOwnProfile: {allow: roles(RoleAdmin, RoleMaintainer, RoleMember, RoleViewer)},
	// ActionViewCapabilities (technical plan §34, GET /api/capabilities)
	// -- same row, same reasoning as ActionViewOwnProfile immediately
	// above: see action.go's own doc comment.
	ActionViewCapabilities: {allow: roles(RoleAdmin, RoleMaintainer, RoleMember, RoleViewer)},

	// Row 2: create/prompt/approve-plan on own/joined -- viewer excluded
	// entirely; member gated by ownership on the two actions that name an
	// EXISTING resource (prompt, approve), unconditional for create
	// (there is no existing resource to own yet).
	ActionCreateSession: {allow: roles(RoleAdmin, RoleMaintainer, RoleMember)},
	ActionPromptSession: {allow: roles(RoleAdmin, RoleMaintainer), allowIfOwned: roles(RoleMember)},
	ActionApprovePlan:   {allow: roles(RoleAdmin, RoleMaintainer), allowIfOwned: roles(RoleMember)},
	// (§25.11): deciding a workflow run's HITL-gated step is
	// own/joined-aware, the SAME row as plan approval by that section's
	// explicit instruction -- see action.go's own doc comment.
	ActionDecideWorkflowStep: {allow: roles(RoleAdmin, RoleMaintainer), allowIfOwned: roles(RoleMember)},
	// (§28.5): uploading to a session is own/joined-aware, the
	// SAME row as prompting by that section's own explicit instruction --
	// see action.go's own doc comment.
	ActionUploadToSession: {allow: roles(RoleAdmin, RoleMaintainer), allowIfOwned: roles(RoleMember)},
	// (§29.9): linking/unlinking the caller's OWN ChatGPT account
	// is own/joined-aware, the SAME row as plan approval by that
	// section's explicit "own-aware like ActionApprovePlan's own row"
	// instruction -- see action.go's own doc comment.
	ActionLinkChatGPTAccount: {allow: roles(RoleAdmin, RoleMaintainer), allowIfOwned: roles(RoleMember)},
	// (§16.2): merging a decision-inbox PR is own/joined-aware,
	// the SAME row shape as prompting/uploading above, by direct analogy
	// -- see action.go's own ActionMergePR doc comment for the full
	// reasoning (no dedicated §13.3 table row names this action
	// explicitly).
	ActionMergePR: {allow: roles(RoleAdmin, RoleMaintainer), allowIfOwned: roles(RoleMember)},

	// Row 3: stop/resume ANY session -- admin/maintainer only, no member
	// own/joined escape hatch (see action.go's own doc comment on why).
	ActionStopSession:   {allow: roles(RoleAdmin, RoleMaintainer)},
	ActionResumeSession: {allow: roles(RoleAdmin, RoleMaintainer)},
	// §29: shadow-comparison tooling reads across ANY two turns --
	// same row as stop/resume, same reasoning (action.go's own doc
	// comment).
	ActionViewShadowComparison: {allow: roles(RoleAdmin, RoleMaintainer)},

	// Row 4: automations/environments/repo+env secrets -- admin/maintainer.
	// (§25.11) adds workflow-definition authoring (an unbound
	// draft) to this SAME row, per that section's explicit "same row as
	// ActionManageAutomations" instruction.
	ActionManageAutomations:         {allow: roles(RoleAdmin, RoleMaintainer)},
	ActionManageEnvironments:        {allow: roles(RoleAdmin, RoleMaintainer)},
	ActionManageRepoSecrets:         {allow: roles(RoleAdmin, RoleMaintainer)},
	ActionManageEnvSecrets:          {allow: roles(RoleAdmin, RoleMaintainer)},
	ActionManageWorkflowDefinitions: {allow: roles(RoleAdmin, RoleMaintainer)},
	// (§27.3): cloud-identity binding CRUD (both environment and
	// global scope) -- maintainer+, this SAME row, per that action's own
	// doc comment.
	ActionManageCloudIdentityBindings: {allow: roles(RoleAdmin, RoleMaintainer)},
	// (§27.4): cluster-binding CRUD (the at-most-one,
	// per-Environment row) -- maintainer+, this SAME row, per that
	// action's own doc comment.
	ActionManageClusterBindings: {allow: roles(RoleAdmin, RoleMaintainer)},

	// Row 5: review verdicts/re-trigger/auto-approve config --
	// admin/maintainer. (§22.2/§22.4) adds the learned
	// false-positive pattern capture/lifecycle actions to this SAME row,
	// no member own/joined carve-out -- see action.go's own doc comment
	// on each ("a taught pattern is repo-scoped, not session-scoped, so
	// there is no 'owned' resource to carve out at all").
	ActionEditReviewVerdict:           {allow: roles(RoleAdmin, RoleMaintainer)},
	ActionRetriggerReview:             {allow: roles(RoleAdmin, RoleMaintainer)},
	ActionConfigureAutoApprove:        {allow: roles(RoleAdmin, RoleMaintainer)},
	ActionTeachFalsePositivePattern:   {allow: roles(RoleAdmin, RoleMaintainer)},
	ActionManageFalsePositivePatterns: {allow: roles(RoleAdmin, RoleMaintainer)},
	// (§26.5): contesting a deep-path arch-recap digest section
	// via the `arch recap wrong: <reason>` PR-thread command -- same row,
	// same reasoning as ActionTeachFalsePositivePattern immediately above
	// (action.go's own doc comment).
	ActionContestArchRecap: {allow: roles(RoleAdmin, RoleMaintainer)},

	// Row 6: integrations/global secrets/template activation/members &
	// roles/sentinel toggle/blockOnHighRisk -- admin only. §25.4
	// (§25.11) adds workflow-binding activation to this SAME row, per
	// that section's explicit "same row as ActionActivatePromptTemplate"
	// instruction (see action.go for why activation is a system-posture
	// change, not row-4 authoring).
	ActionManageIntegrations:  {allow: roles(RoleAdmin)},
	ActionManageGlobalSecrets: {allow: roles(RoleAdmin)},
	// (§27.3): admin-triggered OIDC signing-key rotation -- this
	// SAME admin-only row, per that action's own doc comment.
	ActionManageCloudIdentityKeys:  {allow: roles(RoleAdmin)},
	ActionActivatePromptTemplate:   {allow: roles(RoleAdmin)},
	ActionManageMembers:            {allow: roles(RoleAdmin)},
	ActionToggleSentinelAutoFix:    {allow: roles(RoleAdmin)},
	ActionConfigureBlockOnHighRisk: {allow: roles(RoleAdmin)},
	// (§21.2): arming the per-repo auto-merge toggle -- same row,
	// same reasoning as ActionToggleSentinelAutoFix immediately above
	// (action.go's own doc comment).
	ActionToggleAutoMerge:         {allow: roles(RoleAdmin)},
	ActionActivateWorkflowBinding: {allow: roles(RoleAdmin)},
	// (§24.5): arming the per-repo automatic-re-review opt-in --
	// same row, same reasoning as ActionToggleSentinelAutoFix/
	// ActionToggleAutoMerge immediately above (action.go's own doc
	// comment).
	ActionToggleAutoRetriggerReview: {allow: roles(RoleAdmin)},
	// (§26.2): arming the per-repo description-autofix opt-in --
	// same row, same reasoning as ActionToggleSentinelAutoFix/
	// ActionToggleAutoMerge/ActionToggleAutoRetriggerReview immediately
	// above (action.go's own doc comment: §26.2 names no tier itself,
	// reasoned here to match every sibling unattended-write toggle).
	ActionToggleDescriptionAutofix: {allow: roles(RoleAdmin)},
	// (§26.3): configuring the per-repo reviewDepth mode/
	// deepPaths -- same row, same reasoning as ActionToggleSentinelAutoFix/
	// ActionToggleAutoMerge/ActionToggleAutoRetriggerReview/
	// ActionToggleDescriptionAutofix immediately above (action.go's own
	// doc comment: §26.3 names no tier itself, reasoned here to match
	// every sibling unattended-behavior config).
	ActionConfigureReviewDepth: {allow: roles(RoleAdmin)},
	// (§26.7): configuring the per-repo reviewCostBudget
	// light/deep ceilings -- same row, same reasoning as
	// ActionConfigureReviewDepth immediately above (action.go's own doc
	// comment).
	ActionConfigureReviewCostBudget: {allow: roles(RoleAdmin)},
	// (§4.1.2 amendment): configuring a repo's own RWX preview-link
	// dispatch (dispatchKey/endpointTemplate/orgSlug) -- same row, same
	// reasoning as ActionConfigureReviewCostBudget/ActionConfigureReviewDepth/
	// ActionToggleDescriptionAutofix/ActionToggleAutoRetriggerReview/
	// ActionToggleAutoMerge/ActionToggleSentinelAutoFix immediately above
	// (action.go's own doc comment).
	ActionConfigurePreviewLinks: {allow: roles(RoleAdmin)},
	// (§30.6/§30.9): the shadow-operator ledger view and its
	// Activate promotion -- admin only, this SAME row, though no §13.3
	// table row names either explicitly (action.go's own doc comment on
	// ActionViewShadowLedger explains why, mirroring ActionMergePR's own
	// precedent for an enforced-but-untabled action).
	ActionViewShadowLedger:     {allow: roles(RoleAdmin)},
	ActionActivateShadowLedger: {allow: roles(RoleAdmin)},
}

// Authorize renders the §13.3 verdict for actor attempting action against
// resource: nil if permitted, *ForbiddenError (wrapping ErrForbidden) if
// not, ErrUnknownAction if action names nothing in the matrix at all.
//
// Every state-changing actor command in this codebase — the session actor
// mailbox, plan approval, and (once later Steps land them) verdict edits,
// automation toggles — calls this identically, so a Slack approval
// renders the exact same verdict a web one would (§13.3's own "channel-
// agnostic" requirement), once a later Step resolves a channel actor to a
// real user_id (§13.2's own auto-linking scope, NOT this Step's).
func Authorize(actor Actor, action Action, resource Resource) error {
	rule, ok := matrix[action]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownAction, action)
	}

	if rule.allow[actor.Role] {
		return nil
	}
	if resource.OwnedOrJoined && rule.allowIfOwned[actor.Role] {
		return nil
	}

	return &ForbiddenError{Actor: actor, Action: action}
}
