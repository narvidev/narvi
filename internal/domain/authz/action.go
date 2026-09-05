package authz

// Action names one state-changing (or read) command Authorize can render
// a verdict on — always a plain, discrete capability, never a resource
// type by itself (Resource, in authorize.go, carries whatever per-call
// context a specific Action's own matrix row needs, today just
// OwnedOrJoined). Grouped below by which §13.3 matrix row each belongs to.
type Action string

const (
	// -- Row 1: "View sessions / analytics" — admin, maintainer, member,
	// viewer (viewer read-only; there is no separate "write" verb for
	// either of these two, so read-only is simply the ONLY thing a
	// viewer, or anyone else, does with them).

	// ActionViewSessions is read access to session state/history.
	ActionViewSessions Action = "view_sessions"
	// ActionViewAnalytics is read access to analytics/reporting views.
	ActionViewAnalytics Action = "view_analytics"
	// ActionViewOwnProfile is read access to the AUTHENTICATED caller's
	// OWN identity/role/linked-identity graph (§12.2 item 7's "identity
	// auto-link status panel"; GET /api/me) -- the SAME row as
	// ActionViewSessions/ActionViewAnalytics, not a stretch reuse of
	// either: viewing your own account is, if anything, a MORE basic
	// capability than reading session/analytics data, so excluding
	// viewer here (the way ActionLinkChatGPTAccount's own row does) would
	// be wrong -- there is no "own/joined" carve-out to speak of either,
	// since a "/me" endpoint is by construction always the caller's own
	// resource (mirrors ActionLinkChatGPTAccount's own doc comment on
	// that point), so this has no Resource.OwnedOrJoined dependency at
	// all, unlike that action.
	ActionViewOwnProfile Action = "view_own_profile"
	// ActionViewCapabilities is read access to GET /api/capabilities (the
	// extension & licensing boundaries surface, technical plan §34,
	// docs/design/boundaries-design.md section 4) -- the SAME row as
	// ActionViewSessions/ActionViewAnalytics/ActionViewOwnProfile above,
	// not a stretch reuse of any of them: a distinct Action for a
	// distinct resource, mirroring how ActionViewOwnProfile's own doc
	// comment already reasons about this row. Every role including
	// viewer may read it -- the response is a derived read model that
	// never carries a licence key or a subject (internal/adapters/
	// inbound/httpapi/capabilities.go's own doc comment), so there is
	// nothing here for a viewer to be denied. No own/joined carve-out:
	// this is a deployment-wide fact, not a per-resource one.
	ActionViewCapabilities Action = "view_capabilities"

	// -- Row 2: "Create sessions, prompt, approve plans on own/joined
	// sessions" — admin, maintainer, member (never viewer). Prompting and
	// approving a plan each carry the row's own "own/joined" carve-out
	// for member (Resource.OwnedOrJoined) — admin/maintainer bypass it
	// entirely, per row 3 below. Creating a session has no ownership
	// concept at all (there is no pre-existing resource yet to own) — a
	// member is simply allowed, unconditionally.

	// ActionCreateSession is starting a brand new coding-agent session.
	ActionCreateSession Action = "create_session"
	// ActionPromptSession is enqueuing a new turn on an EXISTING session
	// (the web UI's "send a prompt" / the relaunch-and-resume REST API,
	// internal/adapters/inbound/httpapi/turn.go) — admin/maintainer on
	// any session, member only on one they created or joined.
	ActionPromptSession Action = "prompt_session"
	// ActionApprovePlan is rendering an approve/reject verdict on a
	// plan-mode plan (§8.1) — admin/maintainer on any session's plan,
	// member only on one they created or joined.
	ActionApprovePlan Action = "approve_plan"
	// ActionDecideWorkflowStep is rendering an approve/reject/revise
	// verdict on a workflow run's HITL-gated step (§25.9/§25.11)
	// — own/joined-aware, the SAME row shape as ActionApprovePlan
	// above by §25.11's explicit instruction ("same row as
	// ActionApprovePlan"): admin/maintainer on any run, member only on a
	// session they created or joined. No caller exists yet — the decide
	// endpoint (POST /api/workflow-runs/:runId/steps/:stepRunId/decide)
	// is §25.9's; reserved here so that Step's call site needs no
	// shape change, exactly like every other reserved Action below.
	ActionDecideWorkflowStep Action = "decide_workflow_step"
	// ActionUploadToSession is minting/confirming a file upload against an
	// EXISTING session (POST /api/sessions/:id/uploads and its own
	// /complete twin, §28.5) — the SAME row shape as
	// ActionPromptSession by that section's own explicit instruction ("a
	// new Authorize action mapped to the same §13.3 row as prompting"):
	// admin/maintainer on any session, member only on one they created or
	// joined; viewer never uploads (the viewer guard holds, same as
	// prompting). Downloading an upload (GET .../uploads/:id/content) is
	// NOT gated by this action at all — that is a READ, gated by session
	// visibility instead (a viewer may download), mirroring
	// ListArtifacts/ListEvents' own existing "session exists + logged in,
	// no separate Authorize call" precedent.
	ActionUploadToSession Action = "upload_to_session"
	// ActionMergePR covers clicking Merge on a decision-inbox
	// ready_to_merge row ("decision inbox: read model + API",
	// §16.2/§16.1 -- "Viewer role sees the queue read-only and cannot
	// merge"). §13.3's own table names no dedicated "merge PRs" row at
	// all; this Action is placed in THIS row, the SAME shape as
	// ActionPromptSession/ActionUploadToSession immediately above, by
	// direct analogy rather than an explicit table row: a PR only ever
	// reaches a given user's OWN ready_to_merge queue because it is
	// ALREADY "assigned to the user" (§16.1's own inclusion criterion --
	// directly, as requested reviewer, or via CODEOWNERS), which is
	// exactly the same kind of per-resource ownership §13.3 row 2's
	// "own/joined" carve-out already gates prompting/uploading/approving
	// on for a member — never a blanket "any PR, anywhere" grant the way
	// row 3's stop/resume-ANY-session actions are. The app-layer
	// decision-inbox aggregator (never the httpapi handler itself) is
	// what resolves Resource.OwnedOrJoined here: true iff the PR named by
	// the merge request is one THIS caller's own already-computed
	// provenance (internal/domain/decisioninbox.ResolveProvenance) found
	// them assigned to, re-derived fresh at click time exactly like every
	// other fact the Merge endpoint re-validates (§16.2, §5.2) — never
	// read back from whatever the client-rendered queue merely claims.
	ActionMergePR Action = "merge_pr"
	// ActionLinkChatGPTAccount covers self-service linking/unlinking of
	// the caller's OWN ChatGPT account (POST/DELETE /api/me/chatgpt-link,
	// §29.3/§29.9 — "self-service, own-user only... one new
	// action row, own-aware like ActionApprovePlan's own row"): admin/
	// maintainer unconditionally (though in practice every /api/me/...
	// call is already self-scoped by the caller's own identity), member
	// only via the allowIfOwned carve-out — which for a strictly
	// self-scoped endpoint is always satisfied for the caller's own
	// request; viewers never link (§13.3: viewers are read-only). Admin
	// unlink-of-ANY-user's account mirrors §13.2's own admin force-link
	// precedent by reusing ActionManageMembers (row 6, admin-only) instead
	// of a second action here — see internal/app/chatgptlink's own doc
	// comment for the exact split.
	ActionLinkChatGPTAccount Action = "link_chatgpt_account"

	// -- Row 3: "Stop/resume ANY session; approve ANY plan" — admin,
	// maintainer only. Deliberately no member own/joined carve-out here,
	// unlike row 2 — the matrix names this explicitly as "ANY session",
	// contrasted with row 2's "own/joined" qualifier; a member who
	// created a session still cannot stop or resume it themselves.
	// ActionApprovePlan above already covers "approve ANY plan" (the
	// SAME action, just satisfied unconditionally for admin/maintainer
	// via allow rather than allowIfOwned) — no separate constant needed.

	// ActionStopSession halts a running session. No caller exists yet —
	// this feature isn't built as of this Step (see doc.go) — reserved so
	// a future Step's call site needs no shape change here.
	ActionStopSession Action = "stop_session"
	// ActionResumeSession resumes a stopped session. Same "no caller yet"
	// note as ActionStopSession.
	ActionResumeSession Action = "resume_session"
	// ActionViewShadowComparison covers §8.8's own "shadow-comparison
	// tooling for review" deliverable (GET /api/admin/shadow-compare,
	// shadowcompare.go) -- reads across ANY two turns/sessions, never
	// scoped to ones the caller created or joined, so this sits in THIS
	// row ("ANY session", admin/maintainer only, no member own/joined
	// escape hatch) rather than row 1's "everyone including viewer" one:
	// introspective model-rollout tooling, not an ordinary product
	// surface a member should reach for their own sessions.
	ActionViewShadowComparison Action = "view_shadow_comparison"

	// -- Row 4: "Manage automations, environments, repo/env secrets" —
	// admin, maintainer only. No caller exists yet for any of these three
	// (automations, §8.4, and the environments/secrets management UI,
	// §12.2 item 5, are both built later) — reserved names only.

	// ActionManageAutomations covers creating/editing/deleting automation
	// rules.
	ActionManageAutomations Action = "manage_automations"
	// ActionManageEnvironments covers creating/editing/deleting
	// Environment scoping config (§14.1).
	ActionManageEnvironments Action = "manage_environments"
	// ActionManageRepoSecrets covers per-repo secret management.
	ActionManageRepoSecrets Action = "manage_repo_secrets"
	// ActionManageEnvSecrets covers per-environment secret management.
	ActionManageEnvSecrets Action = "manage_env_secrets"
	// ActionManageWorkflowDefinitions covers creating/editing/deleting a
	// CUSTOM workflow definition — an unbound draft (§25.11),
	// maintainer+ in this SAME row as ActionManageAutomations by
	// §25.11's explicit instruction. Deliberately NOT the action that
	// makes a definition live anywhere (that is
	// ActionActivateWorkflowBinding, admin-only, row 6) — and NOT the
	// gate on built-in definitions at all: PUT/DELETE on an is_built_in
	// row is refused unconditionally, even for an admin, as a
	// STRUCTURAL invariant (§25.4), never an RBAC verdict this matrix
	// could express. No caller exists yet — §25.6/§25.9 own the first
	// handlers.
	ActionManageWorkflowDefinitions Action = "manage_workflow_definitions"
	// ActionManageCloudIdentityBindings covers creating/editing/deleting a
	// cloud_identity_bindings row ("cloud identity: OIDC
	// issuer, bindings, minting", §27.3) -- both scope=environment AND
	// scope=global rows share this ONE action, per §27.3's own explicit
	// instruction ("params are... maintainer+ managed (the §13.3
	// environments row)"), unlike provider_credentials/sandbox_secrets'
	// own 3-way split (ActionManageRepoSecrets/ActionManageEnvSecrets/
	// ActionManageGlobalSecrets, the last of which is admin-only, row 6).
	// A binding's own params are identifiers, not secrets (this Step's
	// own migration doc comment), so a global-scope binding is not the
	// same security-sensitivity class provider_credentials'/
	// sandbox_secrets' own global-scoped SECRET rows are -- it stays in
	// THIS row rather than escalating to row 6 the way those two tables'
	// global scope does. internal/adapters/inbound/httpapi/
	// cloudidentitybindings.go is this Action's own caller.
	ActionManageCloudIdentityBindings Action = "manage_cloud_identity_bindings"
	// ActionManageClusterBindings covers creating/editing/deleting the
	// (at most one, per-Environment) cluster_bindings row (
	// "cloud identity: sandbox-side consumption + kubeconfig injection",
	// §27.4) -- this SAME row (maintainer+) as ActionManageCloudIdentityBindings
	// immediately above, by the identical reasoning that action's own doc
	// comment gives: a cluster binding's own name/serverUrl/caBundle/params
	// are identifiers, never secrets (§27.4's own "params JSONB" shape,
	// mirroring cloud_identity_bindings.params exactly), and it affects
	// one Environment's own deployment target, not a platform-wide
	// security posture -- never the admin-only row a real credential
	// value would sit at (the static rung's own uploaded kubeconfig is
	// itself stored through §27.1's sandbox_secrets, gated by
	// ActionManageEnvSecrets/ActionManageGlobalSecrets at ITS OWN write
	// path, not this one). A separate Action from
	// ActionManageCloudIdentityBindings (not a reuse) because the two
	// gate two structurally different resources (cloud_identity_bindings
	// vs cluster_bindings) -- mirroring how ActionMergePR/
	// ActionPromptSession stay two names despite sharing row 2's own
	// RBAC shape. internal/adapters/inbound/httpapi/clusterbindings.go is
	// this Action's own caller.
	ActionManageClusterBindings Action = "manage_cluster_bindings"

	// -- Row 5: "Edit review verdicts; re-trigger reviews; auto-approval
	// eligibility config" — admin, maintainer only.

	// ActionEditReviewVerdict covers overriding a rendered review
	// verdict.
	ActionEditReviewVerdict Action = "edit_review_verdict"
	// ActionRetriggerReview covers re-running an automated review.
	ActionRetriggerReview Action = "retrigger_review"
	// ActionConfigureAutoApprove covers §21.2 stage 1's auto-approval
	// eligibility CONFIG (repo_settings.max_auto_approve_files_changed/
	// sensitive_blast_radius_tags, migrations/000069_repo_settings_auto_
	// approval.up.sql -- internal/adapters/inbound/httpapi/reposettings.go's
	// own PutRepoSettings). Originally reserved (§8.2) for a label-
	// driven auto-approve rule config that §21.2 supersedes
	// entirely -- see internal/domain/review's own doc comment on why
	// auto-approval is now a deterministic, criteria-driven engine with
	// no per-PR human label in the loop at all; the CONFIGURATION of that
	// engine's own two per-repo-tunable criteria (diff-size threshold,
	// sensitive-path tag list) is what this action gates, never a
	// per-PR decision. Deliberately NOT the same action as
	// ActionToggleAutoMerge below (row 6, admin only) -- arming the
	// auto-merge toggle ends in an UNATTENDED merge, a stricter
	// consequence than tuning the eligibility criteria themselves, so it
	// sits one row down at admin-only, mirroring
	// ActionToggleSentinelAutoFix's own identical split from this row.
	ActionConfigureAutoApprove Action = "configure_auto_approve"
	// ActionTeachFalsePositivePattern covers §22.2's own capture command
	// (§22, "review: learned false-positive patterns"): a maintainer+
	// teaches a repo-scoped false-positive pattern via an explicit
	// `false positive: <reason>` PR-thread command
	// (internal/domain/falsepositive.Match), dispatched BEFORE the
	// ordinary mention/session router (internal/adapters/inbound/github's
	// own capture handler) -- reusing THIS SAME §13.3 gate directly,
	// never a parallel permission model invented for this one command.
	// Placed in THIS row (maintainer+, no member own/joined carve-out --
	// a taught pattern is repo-scoped, not session-scoped, so there is no
	// "owned" resource to carve out at all), the same reasoning
	// ActionEditReviewVerdict/ActionRetriggerReview immediately above
	// already establish for a maintainer-level review-adjacent write.
	ActionTeachFalsePositivePattern Action = "teach_false_positive_pattern"
	// ActionManageFalsePositivePatterns covers §22.4's own lifecycle
	// surface (§22): retiring an already-taught pattern and reading
	// the per-repo audit view (list every pattern, active or retired) --
	// ONE action gating both, mirroring ActionManageMembers' own
	// identical "one action gates every read+write endpoint of this
	// lifecycle-management surface" precedent (row 6, action.go's own
	// doc comment there: "gates every one of its endpoints... including
	// the audit-log read endpoint"). Deliberately a SEPARATE action from
	// ActionTeachFalsePositivePattern above even though both sit in this
	// SAME maintainer+ row today: capture is a PR-thread command with no
	// REST surface at all, retire/audit-view are REST endpoints with no
	// PR-thread surface -- two structurally distinct call sites are
	// exactly the kind of split this codebase names a distinct Action
	// for elsewhere (e.g. ActionMergePR vs. ActionPromptSession, both
	// this same row 2 shape, still two names) rather than merging into
	// one action whose meaning would depend on which caller invoked it.
	ActionManageFalsePositivePatterns Action = "manage_false_positive_patterns"
	// ActionContestArchRecap covers §26.5's own "arch recap
	// wrong: <reason>" PR-thread command -- a maintainer+ contests the
	// deep path's own architecture-recap digest section, mirroring
	// ActionTeachFalsePositivePattern's own capture-command shape EXACTLY
	// (§22.2, this SAME row): dispatched BEFORE the ordinary
	// mention/session router (internal/adapters/inbound/github's own
	// archrecapcontest.go), reusing THIS SAME §13.3 gate directly, never
	// a parallel permission model invented for this one command. Placed
	// in THIS row (maintainer+, no member own/joined carve-out -- a
	// contest is PR-scoped but the judgment "was this recap wrong" is the
	// same maintainer-level review-adjacent write ActionEditReviewVerdict/
	// ActionRetriggerReview/ActionTeachFalsePositivePattern already are,
	// never a session a member merely created or joined), the same
	// reasoning those three already establish.
	ActionContestArchRecap Action = "contest_arch_recap"

	// -- Row 6: "Integrations, global secrets, prompt-template
	// activation, members & roles, sentinel auto-fix toggle, per-repo
	// auto-merge toggle" — admin only. §13.3's own parenthetical singles
	// out the sentinel toggle (and, by the SAME reasoning, the auto-merge
	// toggle below) as ending in an unattended merge with no human Merge
	// click, unlike row 5's per-PR-judgment actions — exactly why both
	// sit in this admin-only row and ActionConfigureAutoApprove above
	// sits one row up, at maintainer. No caller exists yet for four of
	// these six (integrations/global-secrets/template-activation/
	// sentinel are later Steps) — ActionManageMembers is the exception:
	// this SAME Step's own "members API" deliverable (internal/adapters/
	// inbound/httpapi/members.go) gates every one of its endpoints (list
	// members, role-change, manual link/unlink, and the audit-log read
	// endpoint) behind this exact Action, per §13.3's own single, bundled
	// "members & roles" row (no separate read-vs-write Action was
	// invented for it).

	// ActionManageIntegrations covers connecting/disconnecting a
	// third-party integration (Slack/Linear workspace, etc).
	ActionManageIntegrations Action = "manage_integrations"
	// ActionManageGlobalSecrets covers org-wide (non-repo/env-scoped)
	// secret management.
	ActionManageGlobalSecrets Action = "manage_global_secrets"
	// ActionManageCloudIdentityKeys covers §27.3's own ("cloud
	// identity: OIDC issuer, bindings, minting", §27.3) admin-triggered
	// OIDC signing-key rotation (POST /api/cloud-identity/signing-keys/
	// rotate) -- admin only, in THIS row, deliberately NOT the same row
	// as ActionManageCloudIdentityBindings (row 4, maintainer+): a
	// binding CRUD change affects one Environment (or the global
	// fallback) at a time and touches no secret material at all (that
	// row's own doc comment); rotating the issuer's own signing key is a
	// platform-wide security-posture change affecting EVERY Environment's
	// currently-mintable tokens at once, the same class of "changes what
	// runs/verifies unattended, org-wide" judgment call
	// ActionManageIntegrations/ActionManageGlobalSecrets immediately
	// above already sit at admin-only for. See internal/domain/oidckey's
	// own doc comment for the full "why manual, admin-triggered rotation"
	// design decision (this Step's own gap-2 resolution) that this Action
	// gates the trigger for.
	ActionManageCloudIdentityKeys Action = "manage_cloud_identity_keys"
	// ActionActivatePromptTemplate covers activating/deactivating a
	// prompt template.
	ActionActivatePromptTemplate Action = "activate_prompt_template"
	// ActionManageMembers covers inviting/removing members and changing
	// a member's role.
	ActionManageMembers Action = "manage_members"
	// ActionToggleSentinelAutoFix covers §17's own sentinel auto-fix
	// on/off toggle.
	ActionToggleSentinelAutoFix Action = "toggle_sentinel_auto_fix"
	// ActionConfigureBlockOnHighRisk covers §8.2's own
	// blockOnHighRisk admin, per-repo, strict-boolean setting
	// (repo_settings, migrations/000044_repo_settings.up.sql) --
	// internal/adapters/inbound/httpapi/reposettings.go's own GET/PUT
	// routes both gate on this. Placed in this SAME admin-only row as
	// ActionToggleSentinelAutoFix above, not row 5's maintainer-level
	// ActionConfigureAutoApprove: blockOnHighRisk changes what runs
	// UNATTENDED on a repo's own PRs (which formal-review event a
	// verdict submits, up to and including a hard REQUEST_CHANGES block)
	// exactly like the sentinel toggle's own row-6 placement is justified
	// (§13.3's own parenthetical on that row), never a per-PR human
	// judgment call the way row 5's actions are.
	ActionConfigureBlockOnHighRisk Action = "configure_block_on_high_risk"
	// ActionToggleAutoMerge covers §21.2 stage 2's own per-repo auto-merge
	// toggle (repo_settings.auto_merge_enabled, migrations/
	// 000069_repo_settings_auto_approval.up.sql -- internal/adapters/
	// inbound/httpapi/reposettings.go's own PutRepoSettings). Admin only,
	// this SAME row as ActionToggleSentinelAutoFix, by the identical
	// reasoning that action's own doc comment already states: arming this
	// toggle is what turns an already-computed auto-approval into an
	// UNATTENDED merge (internal/app/automerge's own worker, machine-
	// initiated, no human Merge click) -- never a maintainer-level,
	// per-PR judgment call the way row 5's ActionConfigureAutoApprove is.
	// While this toggle is off (every repo's own starting state,
	// DEFAULT false), an auto-approved PR still only ever merges via a
	// human's own 1-click confirm through the EXISTING, unchanged
	// ActionMergePR gate (row 2) -- this action governs ARMING the
	// unattended path, never the merge action itself.
	ActionToggleAutoMerge Action = "toggle_auto_merge"
	// ActionActivateWorkflowBinding covers binding a (repo, lane) — or
	// the global (org-wide, repo_full_name = NULL) scope; the SAME
	// action gates both, per §25.11 — to a specific workflow definition
	// (workflow_bindings, migrations/000057_workflows.up.sql). Admin
	// only, in this SAME row as ActionActivatePromptTemplate by
	// §25.11's explicit instruction — activation changes what runs on
	// 100% of a lane's production traffic (§25.6), a system-posture
	// change like template activation, not a per-draft authoring step
	// like row 4's ActionManageWorkflowDefinitions. No caller exists
	// yet (§25.4 is dark; §25.6/§25.9 own the first handlers).
	ActionActivateWorkflowBinding Action = "activate_workflow_binding"
	// ActionToggleAutoRetriggerReview covers §24.5's own per-repo opt-in
	// for automatic re-review on new commits (repo_settings.
	// auto_retrigger_review_enabled, migrations/
	// 000076_repo_settings_auto_retrigger_review.up.sql --
	// internal/adapters/inbound/httpapi/reposettings.go's own
	// PutAutoRetriggerReviewToggle). Admin only, this SAME row as
	// ActionToggleSentinelAutoFix/ActionToggleAutoMerge, by the identical
	// reasoning those actions' own doc comments already state: arming
	// this toggle changes what runs UNATTENDED on a repo's own PRs (an
	// automatic review turn dispatched with no human in the loop at all)
	// -- never a maintainer-level, per-PR judgment call the way row 5's
	// actions are. Unlike ActionToggleAutoMerge, arming this toggle alone
	// never merges or approves anything by itself (§24.5: "this
	// automation never auto-approves anything on its own") -- it only
	// ever enqueues an ordinary review turn through §8.2's existing
	// dispatch, but the "changes what runs unattended" reasoning for
	// admin-only placement applies identically regardless of that
	// downstream distinction.
	ActionToggleAutoRetriggerReview Action = "toggle_auto_retrigger_review"
	// ActionToggleDescriptionAutofix covers §26.2's own per-repo opt-in
	// (§26.2, "review digest: description adequacy + graduated
	// remediation") for automatically rewriting a Narvi-authored PR's own
	// body when the reviewing agent's description-adequacy check finds it
	// drifted or misleading -- a precondition enforced structurally, not
	// just by prompt: httpapi/reviewverdict.go enqueues a rewrite
	// candidate ONLY when the posted verdict's own DescriptionAdequacy is
	// "drift"/"misleading" (never "ok"), and the delivering notifier
	// (internal/app/outboxworker's own description-autofix notifier)
	// re-asserts that SAME fact again at delivery time, alongside the
	// fresh Narvi-authorship/flag re-verification below
	// (repo_settings.description_autofix_enabled,
	// migrations/000079_repo_settings_description_autofix.up.sql --
	// internal/adapters/inbound/httpapi/reposettings.go's own
	// PutDescriptionAutofixToggle). Admin only, this SAME row as
	// ActionToggleSentinelAutoFix/ActionToggleAutoMerge/
	// ActionToggleAutoRetriggerReview immediately above, by the identical
	// reasoning those actions' own doc comments already state: §26.2
	// itself names no RBAC tier for this toggle, but arming it changes
	// what runs UNATTENDED on a repo's own PRs (an automatic body
	// rewrite, no human in the loop, delivered via the outbox and
	// re-verified server-side at delivery time) -- never a maintainer-
	// level, per-PR judgment call the way row 5's actions are. A
	// human-authored PR is never affected by this toggle at all (§26.2:
	// human PRs only ever get a rendered suggestion, never a write) --
	// this action governs ONLY the Narvi-authored, unattended-write path.
	ActionToggleDescriptionAutofix Action = "toggle_description_autofix"
	// ActionConfigureReviewDepth covers §26.3's own per-repo reviewDepth
	// config (§26.3: "reviewDepth: {mode: auto|always_light|always_deep,
	// deepPaths: [...]}", repo_settings.review_depth_mode/
	// review_depth_deep_paths, migrations/
	// 000082_repo_settings_review_depth.up.sql -- internal/adapters/
	// inbound/httpapi/reposettings.go's own PutReviewDepthConfig). Admin
	// only, this SAME row as ActionToggleSentinelAutoFix/
	// ActionToggleAutoMerge/ActionToggleAutoRetriggerReview/
	// ActionToggleDescriptionAutofix immediately above, by the identical
	// reasoning those actions' own doc comments already state: §26.3
	// names no RBAC tier for this config either, but arming
	// mode=always_deep (or a deepPaths list) changes what runs UNATTENDED
	// on a repo's own PRs -- which model/effort tier and how much cost
	// every automated review incurs, with no human in the loop -- never a
	// maintainer-level, per-PR judgment call the way row 5's actions are.
	// mode=always_light is the SAME action gate even though it could only
	// ever REDUCE unattended cost/rigor, not raise it: this is still a
	// system-posture change to what runs unattended on every future PR,
	// not a per-PR judgment call, so it belongs on this row for the same
	// reason ActionToggleAutoRetriggerReview does even though arming IT
	// also never auto-approves anything by itself (action.go's own doc
	// comment on that action) -- "changes unattended behavior" is the
	// admin-only trigger here, not "necessarily makes things riskier".
	ActionConfigureReviewDepth Action = "configure_review_depth"
	// ActionConfigureReviewCostBudget covers §26.4's own per-repo
	// reviewCostBudget config (§26.7: "reviewCostBudget: {light: <usd>,
	// deep: <usd>} joins §26.3's reviewDepth config on the SAME per-repo
	// settings row", repo_settings.review_cost_budget_light_usd/
	// review_cost_budget_deep_usd, migrations/
	// 000085_repo_settings_review_cost_budget.up.sql -- internal/adapters/
	// inbound/httpapi/reposettings.go's own PutReviewCostBudget). Admin
	// only, this SAME row as ActionConfigureReviewDepth immediately above,
	// by the identical reasoning: §26.7 names no RBAC tier for this config
	// either, but the cost ceiling changes what an automated review is
	// allowed to spend -- and therefore which optional passes it runs --
	// unattended, on every future PR, never a per-PR maintainer judgment
	// call the way row 5's actions are.
	ActionConfigureReviewCostBudget Action = "configure_review_cost_budget"
	// ActionConfigurePreviewLinks covers §4.1.2 amendment's own new
	// PUT /api/repos/{owner}/{repo}/preview-config endpoint
	// (internal/adapters/inbound/httpapi/previewconfig.go) --
	// (re)configures a repo's own RWX preview-link dispatch (repo_settings.
	// rwx_preview_dispatch_key/rwx_preview_endpoint_template/
	// rwx_preview_org_slug, migrations/000059_repo_settings_rwx_preview.
	// up.sql), previously reachable only by internal/app/sessionactor/
	// previewpr.go reading the row directly, on no REST shape at all.
	// Admin only, this SAME row as ActionToggleSentinelAutoFix/
	// ActionToggleAutoMerge/ActionToggleAutoRetriggerReview/
	// ActionToggleDescriptionAutofix/ActionConfigureReviewDepth/
	// ActionConfigureReviewCostBudget immediately above, by the identical
	// reasoning those actions' own doc comments already state: arming
	// this (setting a real dispatchKey/endpointTemplate/orgSlug) changes
	// what runs UNATTENDED on a repo's own pushes -- every future push
	// with a PR now triggers a build dispatch on an external provider
	// (RWX), with no human in the loop and no per-push judgment call the
	// way row 5's actions are. Deliberately its own action, not reused
	// from any §21/§24/§26 sibling: this is the ONLY row-6 action whose
	// own request body carries a credential (dispatchKey), which is why
	// it also gets its own dedicated endpoint rather than folding into
	// the combined PUT .../settings (§4.1.2 amendment's own explicit
	// instruction -- see httpapi/previewconfig.go's own doc comment for
	// the full "why a separate endpoint" reasoning).
	ActionConfigurePreviewLinks Action = "configure_preview_links"

	// ActionViewShadowLedger backs, alongside ActionActivateShadowLedger
	// immediately below, the shadow-operator surface (§30.6/§30.9): the
	// per-repo ledger of
	// suppressed platform-shadow effects (shadow_scm_writes + marked
	// outbox rows) plus the §30.1 LLM-spend line, and the "Activate"
	// graduation gesture that flips repo_settings.live_egress_enabled
	// (§30.8's promotion fence + shadow-era artifact quarantine).
	//
	// NO §13.3 TABLE ROW NAMES EITHER OF THESE -- deliberately, not by
	// omission: this ledger is the only surface in the product holding customer
	// source code at rest, IN FULL (shadowledger.UpdateFileContent.
	// Content is carried whole, never a hash), which is strictly more
	// exposure than even ActionManageMembers' own admin-only audit-log
	// row (§16.1's dead-lettered-outbox-deliveries precedent) already
	// covers -- and whose own retention/PII policy is still an open,
	// deferred decision (§30.9). Admin-only is the answer NOW, precisely
	// because that policy does not exist yet: deciding who may read a
	// body of customer code before deciding how long it lives is the
	// wrong order, and widening past admin-only is gated on retention
	// existing first, not on this matrix growing a row for it. This
	// mirrors ActionMergePR's own identical "no dedicated §13.3 table row
	// names this action explicitly" precedent (row 2, above) -- an Action
	// can be real and enforced without the markdown table ever listing
	// it by name.
	//
	// Two actions, not one, because they are different admin verdicts on
	// different things: viewing a ledger that already exists is a read;
	// Activate is the one-way (per repository; re-promoting an
	// already-live repo is a no-op, §30.8) state change that arms live
	// egress. Both admin-only, both this SAME row-6 grouping -- a repo's
	// egress mode is exactly the kind of "what runs unattended" system
	// posture change this row already gates for every sibling toggle.
	ActionViewShadowLedger Action = "view_shadow_ledger"
	// ActionActivateShadowLedger is the Activate endpoint itself -- see
	// ActionViewShadowLedger's own doc comment immediately above for why
	// this pair carries no §13.3 table row.
	ActionActivateShadowLedger Action = "activate_shadow_ledger"
)

// AllActions is every recognized Action, in this file's own declaration
// order (matrix row order) — exported so tests can exhaustively range
// over every (role, action) pair without hand-maintaining a second list.
var AllActions = []Action{
	ActionViewSessions,
	ActionViewAnalytics,
	ActionViewOwnProfile,
	ActionViewCapabilities,
	ActionCreateSession,
	ActionPromptSession,
	ActionApprovePlan,
	ActionDecideWorkflowStep,
	ActionUploadToSession,
	ActionMergePR,
	ActionLinkChatGPTAccount,
	ActionStopSession,
	ActionResumeSession,
	ActionViewShadowComparison,
	ActionManageAutomations,
	ActionManageEnvironments,
	ActionManageRepoSecrets,
	ActionManageEnvSecrets,
	ActionManageWorkflowDefinitions,
	ActionManageCloudIdentityBindings,
	ActionManageClusterBindings,
	ActionEditReviewVerdict,
	ActionRetriggerReview,
	ActionConfigureAutoApprove,
	ActionTeachFalsePositivePattern,
	ActionManageFalsePositivePatterns,
	ActionContestArchRecap,
	ActionManageIntegrations,
	ActionManageGlobalSecrets,
	ActionManageCloudIdentityKeys,
	ActionActivatePromptTemplate,
	ActionManageMembers,
	ActionToggleSentinelAutoFix,
	ActionConfigureBlockOnHighRisk,
	ActionToggleAutoMerge,
	ActionActivateWorkflowBinding,
	ActionToggleAutoRetriggerReview,
	ActionToggleDescriptionAutofix,
	ActionConfigureReviewDepth,
	ActionConfigureReviewCostBudget,
	ActionConfigurePreviewLinks,
	ActionViewShadowLedger,
	ActionActivateShadowLedger,
}
