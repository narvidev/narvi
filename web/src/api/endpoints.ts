// Typed endpoint functions over http.ts's generic request<T> -- the
// "typed API client" half of §12.1's data layer. Every request/response
// shape below is imported from contracts/gen/ts/rest-dtos.ts (via the
// "@narvi/contracts" alias); this file adds no field, renames no key, and
// declares no interface of its own for any of them -- see http.ts's own
// top comment for the full "why a thin wrapper, not a bigger generator"
// writeup.
//
// Routes mirrored 1:1 from cmd/control-plane/main.go's own router blocks
// (§6.3). This file described a four-route skeleton -- create a session,
// read it back, mint a WS token, page its events -- and said the rest
// would be added "as each one needs a route". They were: the whole UI is
// built on this file now, across every screen. The description outlived
// what it described by an entire phase, which is why it is worth saying
// plainly that a header comment naming a current SCOPE goes stale by
// default, while one naming a PATTERN does not.
//
// The pattern is the durable part and still holds: every route goes
// through the same request<T> plus a generated type from rest-dtos.ts,
// and nothing here declares an interface of its own.
import type {
  ApplySuggestionResponse,
  Automation,
  ArtifactsResponse,
  AuditLogEntry,
  CapabilitiesResponse,
  ChatGPTLinkStatus,
  CloudIdentityBinding,
  ClusterBinding,
  ConfirmUploadResponse,
  CreateAutomationRequest,
  CreateAutomationResponse,
  CreateCloudIdentityBindingRequest,
  CreateProviderCredentialRequest,
  CreateSandboxSecretRequest,
  CreateSessionRequest,
  CreateTurnRequest,
  CreateTurnResponse,
  CreateWorkflowDefinitionRequest,
  Environment,
  EventsResponse,
  FalsePositivePattern,
  LinkMemberIdentityRequest,
  ListAuditLogResponse,
  ListAutomationInvocationsResponse,
  ListAutomationsResponse,
  ListCloudIdentityBindingsResponse,
  ListDecisionInboxResponse,
  ListEnvironmentsResponse,
  ListFalsePositivePatternsResponse,
  ListIntegrationsResponse,
  ListMembersResponse,
  ListPlansResponse,
  ListPromptTemplatesResponse,
  ListProviderCredentialsResponse,
  ListSandboxSecretsResponse,
  ListSessionsResponse,
  ListWorkflowBindingsResponse,
  ListWorkflowDefinitionsResponse,
  ListWorkflowRunsResponse,
  Member,
  MergePullRequestRequest,
  MergePullRequestResponse,
  MintUploadRequest,
  MintUploadResponse,
  ModelCatalog,
  OpenCodeConfig,
  PlanActionResponse,
  PreviewIntentTemplateRequest,
  PreviewIntentTemplateResponse,
  ProviderCredential,
  PutClusterBindingRequest,
  PutOpenCodeConfigRequest,
  PutWorkflowBindingRequest,
  RebutFindingRequest,
  ReleaseManifestReadout,
  RepoDigestScope,
  RepoSettings,
  ReviewAnalytics,
  ReviewFinding,
  ReviewReadout,
  RotateCloudIdentitySigningKeyResponse,
  SandboxSecret,
  Session,
  ShadowLedgerSummary,
  UpdateAutoApprovalSettingsRequest,
  UpdateAutoMergeToggleRequest,
  UpdateAutoRetriggerReviewToggleRequest,
  UpdateCloudIdentityBindingRequest,
  UpdateDescriptionAutofixToggleRequest,
  UpdateMemberRoleRequest,
  UpdateProviderCredentialRequest,
  UpdateReviewCostBudgetRequest,
  UpdateReviewDepthConfigRequest,
  UpdateRepoSettingsRequest,
  UpdateSandboxSecretRequest,
  UpdateWorkflowDefinitionRequest,
  UpsertIntentTemplateRequest,
  WorkflowBinding,
  WorkflowDefinition,
  WorkflowRunDetail,
  WorkflowStepDecideRequest,
  WorkflowStepDecideResponse,
  WSTokenResponse,
} from '@narvi/contracts/rest-dtos'

import { request } from './http'

// -- decision inbox / home view (§16.2/§16.3, decisions 32-34). --

/** listDecisionInbox calls GET /api/decision-inbox -- the home view's own read model: every pending decision addressed to the signed-in caller (ready_to_merge/needs_review/awaiting_approval, plus needs_attention for an admin caller -- enforced server-side, decisioninbox.Build itself, never a client-side filter), already ranked by decision cost then age (§16.1) -- a caller renders this order as-is, never re-sorts. */
export function listDecisionInbox(signal?: AbortSignal): Promise<ListDecisionInboxResponse> {
  return request<ListDecisionInboxResponse>('/api/decision-inbox', { signal })
}

/**
 * mergePullRequest calls POST /api/decision-inbox/merge (§16.2's own Merge
 * endpoint, mockups.html decision 33: "Auto-approved still means
 * human-merged... re-validates CI, approval state, and RBAC server-side at
 * click time"). Sent unconditionally, regardless of the calling
 * component's own role/eligibility check -- RevalidateForMerge
 * (decisioninbox.go) re-checks CI/approval-state/Authorize against live
 * SCM data at click time; the rendered queue this request's own body was
 * built from is never trusted as authority. A 403/409 the server returns
 * surfaces as a genuine ApiError carrying the server's own message,
 * never a generic failure a caller could misattribute.
 */
export function mergePullRequest(body: MergePullRequestRequest, signal?: AbortSignal): Promise<MergePullRequestResponse> {
  return request<MergePullRequestResponse>('/api/decision-inbox/merge', { method: 'POST', body, signal })
}

export function createSession(body: CreateSessionRequest, signal?: AbortSignal): Promise<Session> {
  return request<Session>('/api/sessions', { method: 'POST', body, signal })
}

export function getSession(sessionId: string, signal?: AbortSignal): Promise<Session> {
  return request<Session>(`/api/sessions/${encodeURIComponent(sessionId)}`, { signal })
}

// -- §12.2 item 1: the session workspace sidebar's own list. --

/**
 * listSessions calls GET /api/sessions?filter=mine|all -- the sidebar's
 * own data source (status chips, My sessions/All filter). filter mirrors
 * the REST route's own two accepted values exactly (listsessions.go);
 * this wrapper does not default it itself, so the query key a caller
 * builds always names the actual filter in effect.
 */
export function listSessions(filter: 'mine' | 'all', signal?: AbortSignal): Promise<ListSessionsResponse> {
  return request<ListSessionsResponse>(`/api/sessions?filter=${filter}`, { signal })
}

export function listSessionEvents(sessionId: string, options: { cursor?: string; limit?: number } = {}, signal?: AbortSignal): Promise<EventsResponse> {
  const params = new URLSearchParams()
  if (options.cursor !== undefined) params.set('cursor', options.cursor)
  if (options.limit !== undefined) params.set('limit', String(options.limit))
  const query = params.size > 0 ? `?${params.toString()}` : ''
  return request<EventsResponse>(`/api/sessions/${encodeURIComponent(sessionId)}/events${query}`, { signal })
}

export function mintWsToken(sessionId: string, signal?: AbortSignal): Promise<WSTokenResponse> {
  return request<WSTokenResponse>(`/api/sessions/${encodeURIComponent(sessionId)}/ws-token`, { method: 'POST', signal })
}

export function createTurn(sessionId: string, body: CreateTurnRequest, signal?: AbortSignal): Promise<CreateTurnResponse> {
  return request<CreateTurnResponse>(`/api/sessions/${encodeURIComponent(sessionId)}/turns`, { method: 'POST', body, signal })
}

// -- §12.2 item 7, §13.1: the sign-in view's own two endpoints. --

/**
 * getMe calls GET /api/me -- the authenticated caller's own role +
 * currently-linked identities (internal/adapters/inbound/httpapi/me.go),
 * reusing the SAME generated Member shape GET /api/members returns for
 * each row. Resolves to a 401 ApiError (via http.ts's own request<T>)
 * when no valid session cookie is present -- callers distinguish "not
 * signed in" from a genuine failure by checking `error instanceof
 * ApiError && error.status === 401`, never by string-matching the
 * message.
 */
export function getMe(signal?: AbortSignal): Promise<Member> {
  return request<Member>('/api/me', { signal })
}

/**
 * logout calls POST /auth/logout (internal/adapters/inbound/auth/
 * logout.go) -- revokes the real user_sessions row server-side and clears
 * the narvi_auth_session cookie in the response. Idempotent (see that
 * handler's own doc comment); the 204 it returns carries no body, so
 * http.ts's own request<T> resolves this to undefined -- there is nothing
 * for a caller to do with the result beyond knowing the call completed.
 */
export function logout(signal?: AbortSignal): Promise<undefined> {
  return request<undefined>('/auth/logout', { method: 'POST', signal })
}

// -- §12.2 item 1 / §28: the composer's model/effort selector,
// file attachment, and the rail's own artifacts panel. --

/** listArtifacts calls GET /api/sessions/:id/artifacts (§6.3) -- the rail's own Artifacts panel data source, invalidated automatically on every 'artifact' WS event (ws/invalidation.ts's own existing EVENT_TYPE_INVALIDATION entry, wired before this Step ever needed it). */
export function listArtifacts(sessionId: string, signal?: AbortSignal): Promise<ArtifactsResponse> {
  return request<ArtifactsResponse>(`/api/sessions/${encodeURIComponent(sessionId)}/artifacts`, { signal })
}

/** getModelCatalog calls GET /api/models (§8.8) -- the composer's own model/effort selector data source. Available to every authenticated role including viewer (modelcatalog.go's own doc comment). */
export function getModelCatalog(signal?: AbortSignal): Promise<ModelCatalog> {
  return request<ModelCatalog>('/api/models', { signal })
}

/** mintUpload calls POST /api/sessions/:id/uploads (§28.4/§28.5) -- the browser-facing mint variant (attachments.ts's own runUpload calls this as step 1 of mint -> PUT -> confirm). */
export function mintUpload(sessionId: string, body: MintUploadRequest, signal?: AbortSignal): Promise<MintUploadResponse> {
  return request<MintUploadResponse>(`/api/sessions/${encodeURIComponent(sessionId)}/uploads`, { method: 'POST', body, signal })
}

/** confirmUpload calls POST /api/sessions/:id/uploads/:uploadId/complete (§28.4/§28.6) -- attachments.ts's own runUpload calls this as step 3, after the direct-to-storage PUT. */
export function confirmUpload(sessionId: string, uploadId: string, signal?: AbortSignal): Promise<ConfirmUploadResponse> {
  return request<ConfirmUploadResponse>(`/api/sessions/${encodeURIComponent(sessionId)}/uploads/${encodeURIComponent(uploadId)}/complete`, { method: 'POST', signal })
}

// -- code review + release review (§26.1/§12.2 item 2, §15.2/§15.3/§12.2 item 9). --

/** getReviewReadout calls GET /api/sessions/:id/review -- the code-review view's own merge-readout data source (digest, findings, history, epistemic heads-up). */
export function getReviewReadout(sessionId: string, signal?: AbortSignal): Promise<ReviewReadout> {
  return request<ReviewReadout>(`/api/sessions/${encodeURIComponent(sessionId)}/review`, { signal })
}

/** getReleaseManifestReadout calls GET /api/sessions/:id/release-manifest -- the dedicated release-review screen's own data source. */
export function getReleaseManifestReadout(sessionId: string, signal?: AbortSignal): Promise<ReleaseManifestReadout> {
  return request<ReleaseManifestReadout>(`/api/sessions/${encodeURIComponent(sessionId)}/release-manifest`, { signal })
}

/** retriggerReview calls POST /api/sessions/:id/review/retrigger (§12.2 item 2's "re-run action") -- admin/maintainer only server-side (authz.ActionRetriggerReview); the button itself is rendered role-aware but the server is the real gate. */
export function retriggerReview(sessionId: string, signal?: AbortSignal): Promise<CreateTurnResponse> {
  return request<CreateTurnResponse>(`/api/sessions/${encodeURIComponent(sessionId)}/review/retrigger`, { method: 'POST', signal })
}

/** rebutReviewFinding calls POST /api/sessions/:id/review/findings/:identityHash/rebut (§22.1) -- maintainer+ only server-side. */
export function rebutReviewFinding(sessionId: string, identityHash: string, body: RebutFindingRequest, signal?: AbortSignal): Promise<ReviewFinding> {
  return request<ReviewFinding>(`/api/sessions/${encodeURIComponent(sessionId)}/review/findings/${encodeURIComponent(identityHash)}/rebut`, { method: 'POST', body, signal })
}

/** applySuggestion calls POST /api/sessions/:id/review/findings/:identityHash/apply-suggestion (§12.2 item 2) -- maintainer+ only server-side. */
export function applySuggestion(sessionId: string, identityHash: string, signal?: AbortSignal): Promise<ApplySuggestionResponse> {
  return request<ApplySuggestionResponse>(`/api/sessions/${encodeURIComponent(sessionId)}/review/findings/${encodeURIComponent(identityHash)}/apply-suggestion`, { method: 'POST', signal })
}

/** listFalsePositivePatterns calls GET /api/repos/:owner/:repo/false-positive-patterns (§22.4) -- the per-repo audit view, maintainer+ only server-side (authz.ActionManageFalsePositivePatterns). */
export function listFalsePositivePatterns(owner: string, repo: string, signal?: AbortSignal): Promise<ListFalsePositivePatternsResponse> {
  return request<ListFalsePositivePatternsResponse>(`/api/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/false-positive-patterns`, { signal })
}

/** retireFalsePositivePattern calls POST /api/repos/:owner/:repo/false-positive-patterns/:patternId/retire (§22.4) -- maintainer+ only server-side. */
export function retireFalsePositivePattern(owner: string, repo: string, patternId: string, signal?: AbortSignal): Promise<FalsePositivePattern> {
  return request<FalsePositivePattern>(`/api/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/false-positive-patterns/${encodeURIComponent(patternId)}/retire`, { method: 'POST', signal })
}

// -- plan mode (§8.1/§12.2 item 3). --

/** listPlans calls GET /api/sessions/:id/plans -- every plan VERSION for the session, ordered by version, each carrying its own best-effort-extracted content (restdtos.Plan.content's own doc comment). */
export function listPlans(sessionId: string, signal?: AbortSignal): Promise<ListPlansResponse> {
  return request<ListPlansResponse>(`/api/sessions/${encodeURIComponent(sessionId)}/plans`, { signal })
}

/**
 * approvePlan calls POST /api/sessions/:id/plans/:planId/approve
 * (§12.2 item 3's "Approve & build"). Own/joined-aware server-side
 * (authz.ActionApprovePlan, planauthz.go) -- this call is sent regardless
 * of what the client-side affordance decided to render; a caller not
 * actually authorized gets a real 403 back, never a client-side-only
 * refusal (see PlanModeView.tsx's own top doc comment).
 */
export function approvePlan(sessionId: string, planId: string, signal?: AbortSignal): Promise<PlanActionResponse> {
  return request<PlanActionResponse>(`/api/sessions/${encodeURIComponent(sessionId)}/plans/${encodeURIComponent(planId)}/approve`, { method: 'POST', signal })
}

/** rejectPlan calls POST /api/sessions/:id/plans/:planId/reject (§12.2 item 3's "Reject") -- same server-side authorization as approvePlan above. */
export function rejectPlan(sessionId: string, planId: string, signal?: AbortSignal): Promise<PlanActionResponse> {
  return request<PlanActionResponse>(`/api/sessions/${encodeURIComponent(sessionId)}/plans/${encodeURIComponent(planId)}/reject`, { method: 'POST', signal })
}

// -- automations (§8.4/§12.2 item 4). --

/** listAutomations calls GET /api/automations, optionally filtered by creator/status (§8.4's own "creator/status filters" -- mockups.html's "My automations ▾ / All statuses ▾" toolbar). No extra RBAC beyond signed-in (automations.go's own doc comment). */
export function listAutomations(filter: { createdBy?: 'me' | string; status?: 'active' | 'paused' } = {}, signal?: AbortSignal): Promise<ListAutomationsResponse> {
  const params = new URLSearchParams()
  if (filter.createdBy !== undefined) params.set('createdBy', filter.createdBy)
  if (filter.status !== undefined) params.set('status', filter.status)
  const query = params.size > 0 ? `?${params.toString()}` : ''
  return request<ListAutomationsResponse>(`/api/automations${query}`, { signal })
}

/** createAutomation calls POST /api/automations -- admin/maintainer only server-side (authz.ActionManageAutomations); the button itself is rendered role-aware but the server is the real gate. */
export function createAutomation(body: CreateAutomationRequest, signal?: AbortSignal): Promise<CreateAutomationResponse> {
  return request<CreateAutomationResponse>('/api/automations', { method: 'POST', body, signal })
}

/** getAutomation calls GET /api/automations/:id. */
export function getAutomation(automationId: string, signal?: AbortSignal): Promise<Automation> {
  return request<Automation>(`/api/automations/${encodeURIComponent(automationId)}`, { signal })
}

/** listAutomationInvocations calls GET /api/automations/:id/invocations (the automations UI's own "runs table" addition) -- this automation's own most recent invocations, newest first, each with its own nested runs (automationinvocations.go's own doc comment). */
export function listAutomationInvocations(automationId: string, signal?: AbortSignal): Promise<ListAutomationInvocationsResponse> {
  return request<ListAutomationInvocationsResponse>(`/api/automations/${encodeURIComponent(automationId)}/invocations`, { signal })
}

/** pauseAutomation calls POST /api/automations/:id/pause -- admin/maintainer only server-side (authz.ActionManageAutomations). */
export function pauseAutomation(automationId: string, signal?: AbortSignal): Promise<Automation> {
  return request<Automation>(`/api/automations/${encodeURIComponent(automationId)}/pause`, { method: 'POST', signal })
}

/** resumeAutomation calls POST /api/automations/:id/resume -- same server-side gate as pauseAutomation above; this is the mockup's own "auto-paused chip + Resume" action. */
export function resumeAutomation(automationId: string, signal?: AbortSignal): Promise<Automation> {
  return request<Automation>(`/api/automations/${encodeURIComponent(automationId)}/resume`, { method: 'POST', signal })
}

// -- members & access, audit log (§13.2/§13.3, §12.2 item 5). --

/** listMembers calls GET /api/members -- every user with role/disabled and their own currently-linked identities, plus every system-wide still-pending link prompt. Admin-only server-side (authz.ActionManageMembers). */
export function listMembers(signal?: AbortSignal): Promise<ListMembersResponse> {
  return request<ListMembersResponse>('/api/members', { signal })
}

/** updateMemberRole calls PATCH /api/members/:userId/role -- admin-only server-side. */
export function updateMemberRole(userId: string, body: UpdateMemberRoleRequest, signal?: AbortSignal): Promise<Member> {
  return request<Member>(`/api/members/${encodeURIComponent(userId)}/role`, { method: 'PATCH', body, signal })
}

/** linkMemberIdentity calls POST /api/members/:userId/identities -- admin manual link, admin-only server-side. */
export function linkMemberIdentity(userId: string, body: LinkMemberIdentityRequest, signal?: AbortSignal): Promise<Member> {
  return request<Member>(`/api/members/${encodeURIComponent(userId)}/identities`, { method: 'POST', body, signal })
}

/** unlinkMemberIdentity calls DELETE /api/members/:userId/identities/:identityId -- admin-only server-side. */
export function unlinkMemberIdentity(userId: string, identityId: string, signal?: AbortSignal): Promise<Member> {
  return request<Member>(`/api/members/${encodeURIComponent(userId)}/identities/${encodeURIComponent(identityId)}`, { method: 'DELETE', signal })
}

/**
 * listAuditLog calls GET /api/audit-log -- ONE PAGE of audit_log rows,
 * newest first, never the whole log (members.go's own ListAuditLog:
 * defaultAuditLogPageSize 50, maxAuditLogPageSize 200, plus ?offset=).
 *
 * The page size is passed explicitly rather than defaulted so the caller
 * cannot mistake the response for the complete table: this comment used to
 * say "every audit_log row", and the members screen was built on that
 * reading -- filtering a 50-row page client-side and then rendering "No
 * entries." as though it were a statement about a member's entire history.
 * On an audit surface that is the worst possible thing to get wrong, so the
 * shape of this signature now makes the paging impossible to overlook.
 */
export function listAuditLog(params: { limit: number; offset: number }, signal?: AbortSignal): Promise<ListAuditLogResponse> {
  const query = new URLSearchParams({ limit: String(params.limit), offset: String(params.offset) })
  return request<ListAuditLogResponse>(`/api/audit-log?${query.toString()}`, { signal })
}

// -- integrations & ChatGPT-account (Codex) linking (§12.5, §29.3/§29.9,
// §12.2 item 5). --

/** getIntegrations calls GET /api/integrations -- one row per ingress surface (Slack/Linear/GitHub, §12.5's own "integrations read model & routes" amendment). Admin-only server-side (authz.ActionManageIntegrations). A DERIVED read: configured plus last-inbound/last-outbound evidence, never a stored connection row and never the secrets themselves in any form -- see restdtos.Integration's own doc comment for why configured/lastOutboundStatus are facts with timestamps, never a health verdict. */
export function getIntegrations(signal?: AbortSignal): Promise<ListIntegrationsResponse> {
  return request<ListIntegrationsResponse>('/api/integrations', { signal })
}

// -- extension & licensing boundaries (technical plan §34, docs/design/boundaries-design.md section 4). --

/** getCapabilities calls GET /api/capabilities -- one row per internal/domain/license.All entry (organization_governance, compliance, knowledge_retrieval), in that package's own fixed order. Readable by every role including viewer (authz.ActionViewCapabilities). A DERIVED read model: whether Narvi Gatekeeper's own module is composed into this binary, and each capability's installed/licensed/valid-now state -- never the licence key, a fingerprint of it, or the grant's own subject. Drives ext/useCapabilities.ts and, through it, the runtime slot registry (ext/slots.tsx) and GatekeeperAffordance. */
export function getCapabilities(signal?: AbortSignal): Promise<CapabilitiesResponse> {
  return request<CapabilitiesResponse>('/api/capabilities', { signal })
}

/** startChatGPTLink calls POST /api/me/chatgpt-link (§29.3 step 1: "Connect ChatGPT account") -- begins (or reuses a still-live) device-flow attempt for the authenticated caller. Self-service, own-user only server-side (authz.ActionLinkChatGPTAccount, member+ -- viewers excluded). */
export function startChatGPTLink(signal?: AbortSignal): Promise<ChatGPTLinkStatus> {
  return request<ChatGPTLinkStatus>('/api/me/chatgpt-link', { method: 'POST', signal })
}

/** getChatGPTLinkStatus calls GET /api/me/chatgpt-link (§29.3 step 2) -- the Settings page's own poll loop. There is no background worker driving this flow: the human sitting on the page IS the polling loop, so every call this client makes performs AT MOST one throttled upstream attempt server-side (chatgpt_link_attempts.last_polled_at) -- polling faster than the server's own throttle simply no-ops rather than double-spending attempts. */
export function getChatGPTLinkStatus(signal?: AbortSignal): Promise<ChatGPTLinkStatus> {
  return request<ChatGPTLinkStatus>('/api/me/chatgpt-link', { signal })
}

/** unlinkChatGPTAccount calls DELETE /api/me/chatgpt-link (§29.3: "unlink deletes it") -- idempotent, 204 whether or not an account was actually linked. */
export function unlinkChatGPTAccount(signal?: AbortSignal): Promise<undefined> {
  return request<undefined>('/api/me/chatgpt-link', { method: 'DELETE', signal })
}

// -- environments (§14.1) --

/** listEnvironments calls GET /api/environments -- every environments row, newest-first. Maintainer+ only server-side (authz.ActionManageEnvironments); see httpapi/environments.go's own doc comment for why this is list-only, no create/update. */
export function listEnvironments(signal?: AbortSignal): Promise<ListEnvironmentsResponse> {
  return request<ListEnvironmentsResponse>('/api/environments', { signal })
}

// -- prompt templates (§18.6, §12.2 item 5). --

/** listPromptTemplates calls GET /api/intent-templates -- every prompt_templates row, ordered by name. Admin-only server-side (authz.ActionActivatePromptTemplate). */
export function listPromptTemplates(signal?: AbortSignal): Promise<ListPromptTemplatesResponse> {
  return request<ListPromptTemplatesResponse>('/api/intent-templates', { signal })
}

/** previewIntentTemplate calls POST /api/intent-templates/preview -- assembles a DRAFT template's text against real variable values, never touching Postgres. Admin-only server-side. */
export function previewIntentTemplate(body: PreviewIntentTemplateRequest, signal?: AbortSignal): Promise<PreviewIntentTemplateResponse> {
  return request<PreviewIntentTemplateResponse>('/api/intent-templates/preview', { method: 'POST', body, signal })
}

/** upsertIntentTemplate calls POST /api/intent-templates -- creates or overwrites a named template's text. Admin-only server-side. */
export function upsertIntentTemplate(body: UpsertIntentTemplateRequest, signal?: AbortSignal) {
  return request<{ name: string; template: string; updatedAt: string }>('/api/intent-templates', { method: 'POST', body, signal })
}

// -- digest scope, review analytics (§21.3/§21.1, §12.2 item 5). --

/** getRepoDigestScope calls GET /api/repos/:owner/:repo/digest-scope -- which Slack channels/Linear organizations would receive this repo's own next daily digest (derived, read-only -- see httpapi/digestscope.go's own doc comment for why there is no editable cadence/scope setting). Every role including viewer (authz.ActionViewAnalytics). */
export function getRepoDigestScope(owner: string, repo: string, signal?: AbortSignal): Promise<RepoDigestScope> {
  return request<RepoDigestScope>(`/api/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/digest-scope`, { signal })
}

/** getReviewAnalytics calls GET /api/repos/:owner/:repo/review-analytics -- the review-risk analytics section's own read model (§21.1), each rollup carrying its own independent "not yet computed" sentinel. Every role including viewer. */
export function getReviewAnalytics(owner: string, repo: string, signal?: AbortSignal): Promise<ReviewAnalytics> {
  return request<ReviewAnalytics>(`/api/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/review-analytics`, { signal })
}

// -- per-repository settings (§21, §26.7, §26.8, §4.1.2): the per-repo
// policy-flag surface RepoSettingsView.tsx owns. EIGHT routes, not one --
// see httpapi/reposettings.go's own doc comments for why each PUT below is
// its own separately-gated endpoint rather than one combined write: a
// maintainer authorized only for the auto-approval-config row (§13.3 row
// 5) must never be forced through an admin-only gate (row 6) just to save
// the one field they ARE authorized for. Every PUT here carries the
// COMPLETE desired state for the fields it owns (never a partial patch,
// UpdateRepoSettingsRequest's own doc comment) -- callers load the current
// RepoSettings first, edit, then send the whole group back.

function repoSettingsPath(owner: string, repo: string, suffix?: string): string {
  const base = `/api/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/settings`
  return suffix ? `/api/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/${suffix}` : base
}

/** getRepoSettings calls GET /api/repos/:owner/:repo/settings -- every repo_settings column this screen renders, plus the §21.2 stage-2 contradiction-rate calibration read model. Readable by a maintainer authorized for ANY one of the several narrower PUT routes below, not admin-only alone (GetRepoSettings' own authorizeAny, httpapi/reposettings.go). A 404 ApiError means this deployment does not know the repo yet (resolveKnownRepo's own gate -- mirrors getRepoDigestScope's identical contract). */
export function getRepoSettings(owner: string, repo: string, signal?: AbortSignal): Promise<RepoSettings> {
  return request<RepoSettings>(repoSettingsPath(owner, repo), { signal })
}

/** putRepoSettings calls PUT /api/repos/:owner/:repo/settings -- blockOnHighRisk + sentinelAutofixEnabled ONLY. Admin-only server-side (BOTH authz.ActionConfigureBlockOnHighRisk AND authz.ActionToggleSentinelAutoFix). Deliberately excludes every §21.2 stage-1 field (maxAutoApproveFilesChanged/sensitiveBlastRadiusTags) -- see putAutoApprovalSettings below, which owns those under its own maintainer-level gate. */
export function putRepoSettings(owner: string, repo: string, body: UpdateRepoSettingsRequest, signal?: AbortSignal): Promise<RepoSettings> {
  return request<RepoSettings>(repoSettingsPath(owner, repo), { method: 'PUT', body, signal })
}

/** putAutoApprovalSettings calls PUT /api/repos/:owner/:repo/auto-approval-settings -- maxAutoApproveFilesChanged + sensitiveBlastRadiusTags. Maintainer+ server-side (authz.ActionConfigureAutoApprove, §13.3 row 5) -- the one write route on this screen a maintainer, not just an admin, is authorized for. */
export function putAutoApprovalSettings(owner: string, repo: string, body: UpdateAutoApprovalSettingsRequest, signal?: AbortSignal): Promise<RepoSettings> {
  return request<RepoSettings>(repoSettingsPath(owner, repo, 'auto-approval-settings'), { method: 'PUT', body, signal })
}

/** putAutoMergeToggle calls PUT /api/repos/:owner/:repo/auto-merge -- arms/disarms unattended merge of an auto-approved PR. Admin-only server-side (authz.ActionToggleAutoMerge). */
export function putAutoMergeToggle(owner: string, repo: string, body: UpdateAutoMergeToggleRequest, signal?: AbortSignal): Promise<RepoSettings> {
  return request<RepoSettings>(repoSettingsPath(owner, repo, 'auto-merge'), { method: 'PUT', body, signal })
}

/** putAutoRetriggerReviewToggle calls PUT /api/repos/:owner/:repo/auto-retrigger-review -- arms/disarms automatic re-review on new commits (§24.5). Admin-only server-side (authz.ActionToggleAutoRetriggerReview). */
export function putAutoRetriggerReviewToggle(owner: string, repo: string, body: UpdateAutoRetriggerReviewToggleRequest, signal?: AbortSignal): Promise<RepoSettings> {
  return request<RepoSettings>(repoSettingsPath(owner, repo, 'auto-retrigger-review'), { method: 'PUT', body, signal })
}

/** putDescriptionAutofixToggle calls PUT /api/repos/:owner/:repo/description-autofix -- arms/disarms automatic PR-description rewriting for Narvi-authored PRs (§26.2). Admin-only server-side (authz.ActionToggleDescriptionAutofix). */
export function putDescriptionAutofixToggle(owner: string, repo: string, body: UpdateDescriptionAutofixToggleRequest, signal?: AbortSignal): Promise<RepoSettings> {
  return request<RepoSettings>(repoSettingsPath(owner, repo, 'description-autofix'), { method: 'PUT', body, signal })
}

/** putReviewDepthConfig calls PUT /api/repos/:owner/:repo/review-depth -- reviewDepthMode + reviewDepthDeepPaths (§26.3/§26.8). Admin-only server-side (authz.ActionConfigureReviewDepth). */
export function putReviewDepthConfig(owner: string, repo: string, body: UpdateReviewDepthConfigRequest, signal?: AbortSignal): Promise<RepoSettings> {
  return request<RepoSettings>(repoSettingsPath(owner, repo, 'review-depth'), { method: 'PUT', body, signal })
}

/** putReviewCostBudget calls PUT /api/repos/:owner/:repo/review-cost-budget -- reviewCostBudgetLightUsd + reviewCostBudgetDeepUsd (§26.7). Admin-only server-side (authz.ActionConfigureReviewCostBudget). Zero/negative values are rejected 400 server-side (they would collide with the "unconfigured" sentinel and silently mean unlimited spend) -- callers validate this client-side too (repoSettingsFormat.ts's own parseOptionalPositiveUsd) so the refusal is never the caller's first hint. */
export function putReviewCostBudget(owner: string, repo: string, body: UpdateReviewCostBudgetRequest, signal?: AbortSignal): Promise<RepoSettings> {
  return request<RepoSettings>(repoSettingsPath(owner, repo, 'review-cost-budget'), { method: 'PUT', body, signal })
}

/** getShadowLedger calls GET /api/repos/:owner/:repo/shadow-ledger -- the shadow-operator surface's own read model (§30.6): suppressed writes grouped by category, the §30.1 LLM-spend line, and this repository's own current egress-mode state. ADMIN-ONLY server-side (authz.ActionViewShadowLedger) -- a non-admin gets 403, never a filtered/redacted body, since this ledger can hold a customer repository's own file content at rest. */
export function getShadowLedger(owner: string, repo: string, signal?: AbortSignal): Promise<ShadowLedgerSummary> {
  return request<ShadowLedgerSummary>(repoSettingsPath(owner, repo, 'shadow-ledger'), { signal })
}

/** postActivateShadowLedger calls POST /api/repos/:owner/:repo/shadow-ledger/activate -- the graduation gesture that flips this repository's own live_egress_enabled from shadow to live (§30.8). Admin-only server-side (authz.ActionActivateShadowLedger); a 409 ApiError means unhandled shadow-era rows still remain for this repository (§30.8's own quarantine) -- the caller's own message names the count. Returns the freshly-rebuilt ShadowLedgerSummary on success, so the caller never needs a follow-up GET. */
export function postActivateShadowLedger(owner: string, repo: string, signal?: AbortSignal): Promise<ShadowLedgerSummary> {
  return request<ShadowLedgerSummary>(repoSettingsPath(owner, repo, 'shadow-ledger/activate'), { method: 'POST', signal })
}

// -- secrets scope resolution (§27.1/§25.1, §12.2 item 5). --
//
// sandbox-secrets and provider-credentials both partition into the SAME
// 3 scopes (repo/environment/global), each its own separately-gated REST
// route group on the server (environments.go's own doc comment on why
// this mirrors that split). SecretScope + secretScopePath below name the
// ONE place that partition is expressed client-side, rather than 6
// independently-hand-written base-path strings (3 scopes x 2 resources)
// drifting from each other over time.

export type SecretScope = { kind: 'repo'; owner: string; repo: string } | { kind: 'environment'; environmentId: string } | { kind: 'global' }

function secretScopePath(resource: 'sandbox-secrets' | 'provider-credentials', scope: SecretScope): string {
  switch (scope.kind) {
    case 'repo':
      return `/api/repos/${encodeURIComponent(scope.owner)}/${encodeURIComponent(scope.repo)}/${resource}`
    case 'environment':
      return `/api/environments/${encodeURIComponent(scope.environmentId)}/${resource}`
    case 'global':
      return `/api/${resource}`
  }
}

/** listSandboxSecrets calls GET on the sandbox-secrets route group matching scope -- every row at that (scope, scopeTarget), NEVER the secret value itself (only SandboxSecret.maskedValue, a fixed non-secret placeholder). */
export function listSandboxSecrets(scope: SecretScope, signal?: AbortSignal): Promise<ListSandboxSecretsResponse> {
  return request<ListSandboxSecretsResponse>(secretScopePath('sandbox-secrets', scope), { signal })
}

/** createSandboxSecret calls POST on the sandbox-secrets route group matching scope. body.value is sent once, over this one request, and is never echoed back by any response this client ever reads. */
export function createSandboxSecret(scope: SecretScope, body: CreateSandboxSecretRequest, signal?: AbortSignal): Promise<SandboxSecret> {
  return request<SandboxSecret>(secretScopePath('sandbox-secrets', scope), { method: 'POST', body, signal })
}

/** updateSandboxSecretValue calls PUT on the sandbox-secrets route group matching scope -- rotates ONLY the value; name/scope are immutable once created. */
export function updateSandboxSecretValue(scope: SecretScope, secretId: string, body: UpdateSandboxSecretRequest, signal?: AbortSignal): Promise<SandboxSecret> {
  return request<SandboxSecret>(`${secretScopePath('sandbox-secrets', scope)}/${encodeURIComponent(secretId)}`, { method: 'PUT', body, signal })
}

/** deleteSandboxSecret calls DELETE on the sandbox-secrets route group matching scope. */
export function deleteSandboxSecret(scope: SecretScope, secretId: string, signal?: AbortSignal): Promise<undefined> {
  return request<undefined>(`${secretScopePath('sandbox-secrets', scope)}/${encodeURIComponent(secretId)}`, { method: 'DELETE', signal })
}

/** listProviderCredentials calls GET on the provider-credentials route group matching scope -- every row at that (scope, scopeTarget), one per configured AI provider, NEVER the credential value itself (only ProviderCredential.maskedValue). */
export function listProviderCredentials(scope: SecretScope, signal?: AbortSignal): Promise<ListProviderCredentialsResponse> {
  return request<ListProviderCredentialsResponse>(secretScopePath('provider-credentials', scope), { signal })
}

/** createProviderCredential calls POST on the provider-credentials route group matching scope. */
export function createProviderCredential(scope: SecretScope, body: CreateProviderCredentialRequest, signal?: AbortSignal): Promise<ProviderCredential> {
  return request<ProviderCredential>(secretScopePath('provider-credentials', scope), { method: 'POST', body, signal })
}

/** updateProviderCredentialValue calls PUT on the provider-credentials route group matching scope -- rotates ONLY the value. */
export function updateProviderCredentialValue(scope: SecretScope, credentialId: string, body: UpdateProviderCredentialRequest, signal?: AbortSignal): Promise<ProviderCredential> {
  return request<ProviderCredential>(`${secretScopePath('provider-credentials', scope)}/${encodeURIComponent(credentialId)}`, { method: 'PUT', body, signal })
}

/** deleteProviderCredential calls DELETE on the provider-credentials route group matching scope. */
export function deleteProviderCredential(scope: SecretScope, credentialId: string, signal?: AbortSignal): Promise<undefined> {
  return request<undefined>(`${secretScopePath('provider-credentials', scope)}/${encodeURIComponent(credentialId)}`, { method: 'DELETE', signal })
}

// -- cloud identity: OIDC bindings, cluster binding, signing-key rotation (§27.3/§27.4, §12.2 item 5). --

export type CloudIdentityBindingScope = { kind: 'environment'; environmentId: string } | { kind: 'global' }

function cloudIdentityBindingsPath(scope: CloudIdentityBindingScope): string {
  return scope.kind === 'environment' ? `/api/environments/${encodeURIComponent(scope.environmentId)}/cloud-identity-bindings` : '/api/cloud-identity-bindings'
}

/** listCloudIdentityBindings calls GET on the cloud-identity-bindings route group matching scope. A 503 ApiError means the capability is unconfigured (RequireCloudIdentityCapability, fail-closed) -- callers should render "no rotation/binding affordance at all" for that case, never retry it as a transient error. */
export function listCloudIdentityBindings(scope: CloudIdentityBindingScope, signal?: AbortSignal): Promise<ListCloudIdentityBindingsResponse> {
  return request<ListCloudIdentityBindingsResponse>(cloudIdentityBindingsPath(scope), { signal })
}

/** createCloudIdentityBinding calls POST on the cloud-identity-bindings route group matching scope. params carries identifiers only, never a secret (CloudIdentityBinding.params' own doc comment). */
export function createCloudIdentityBinding(scope: CloudIdentityBindingScope, body: CreateCloudIdentityBindingRequest, signal?: AbortSignal): Promise<CloudIdentityBinding> {
  return request<CloudIdentityBinding>(cloudIdentityBindingsPath(scope), { method: 'POST', body, signal })
}

/** updateCloudIdentityBinding calls PUT on the cloud-identity-bindings route group matching scope -- rotates audience/params; kind is immutable once created. */
export function updateCloudIdentityBinding(scope: CloudIdentityBindingScope, bindingId: string, body: UpdateCloudIdentityBindingRequest, signal?: AbortSignal): Promise<CloudIdentityBinding> {
  return request<CloudIdentityBinding>(`${cloudIdentityBindingsPath(scope)}/${encodeURIComponent(bindingId)}`, { method: 'PUT', body, signal })
}

/** deleteCloudIdentityBinding calls DELETE on the cloud-identity-bindings route group matching scope. */
export function deleteCloudIdentityBinding(scope: CloudIdentityBindingScope, bindingId: string, signal?: AbortSignal): Promise<undefined> {
  return request<undefined>(`${cloudIdentityBindingsPath(scope)}/${encodeURIComponent(bindingId)}`, { method: 'DELETE', signal })
}

/** getEnvironmentClusterBinding calls GET /api/environments/:id/cluster-binding -- the (at most one, per-Environment) cluster binding. A 404 ApiError means no binding exists for this Environment yet. */
export function getEnvironmentClusterBinding(environmentId: string, signal?: AbortSignal): Promise<ClusterBinding> {
  return request<ClusterBinding>(`/api/environments/${encodeURIComponent(environmentId)}/cluster-binding`, { signal })
}

/** putEnvironmentClusterBinding calls PUT /api/environments/:id/cluster-binding -- create-or-replace (upsert), the singleton-resource convention every §27 config table here uses. */
export function putEnvironmentClusterBinding(environmentId: string, body: PutClusterBindingRequest, signal?: AbortSignal): Promise<ClusterBinding> {
  return request<ClusterBinding>(`/api/environments/${encodeURIComponent(environmentId)}/cluster-binding`, { method: 'PUT', body, signal })
}

/** deleteEnvironmentClusterBinding calls DELETE /api/environments/:id/cluster-binding. */
export function deleteEnvironmentClusterBinding(environmentId: string, signal?: AbortSignal): Promise<undefined> {
  return request<undefined>(`/api/environments/${encodeURIComponent(environmentId)}/cluster-binding`, { method: 'DELETE', signal })
}

/** rotateCloudIdentitySigningKey calls POST /api/cloud-identity/signing-keys/rotate -- admin-only, destructive-adjacent (§27.3/§27.8): mints a fresh signing key and retires the previous one after the JWKS overlap window. Never call this without an explicit user confirmation first -- see the Settings view's own confirm-before-rotate UI. */
export function rotateCloudIdentitySigningKey(signal?: AbortSignal): Promise<RotateCloudIdentitySigningKeyResponse> {
  return request<RotateCloudIdentitySigningKeyResponse>('/api/cloud-identity/signing-keys/rotate', { method: 'POST', signal })
}

// -- OpenCode config (§27.2, §12.2 item 5). --

export type OpenCodeConfigScope = { kind: 'environment'; environmentId: string } | { kind: 'global' }

function openCodeConfigPath(scope: OpenCodeConfigScope): string {
  return scope.kind === 'environment' ? `/api/environments/${encodeURIComponent(scope.environmentId)}/opencode-config` : '/api/opencode-config'
}

/** getOpenCodeConfig calls GET on the opencode-config route matching scope -- returned in FULL, plaintext (this is configuration, not secret material -- OpenCodeConfig's own doc comment). A 404 ApiError means no document has been saved for this scope yet. */
export function getOpenCodeConfig(scope: OpenCodeConfigScope, signal?: AbortSignal): Promise<OpenCodeConfig> {
  return request<OpenCodeConfig>(openCodeConfigPath(scope), { signal })
}

/** putOpenCodeConfig calls PUT on the opencode-config route matching scope -- create-or-replace. */
export function putOpenCodeConfig(scope: OpenCodeConfigScope, body: PutOpenCodeConfigRequest, signal?: AbortSignal): Promise<OpenCodeConfig> {
  return request<OpenCodeConfig>(openCodeConfigPath(scope), { method: 'PUT', body, signal })
}

/** deleteOpenCodeConfig calls DELETE on the opencode-config route matching scope. */
export function deleteOpenCodeConfig(scope: OpenCodeConfigScope, signal?: AbortSignal): Promise<undefined> {
  return request<undefined>(openCodeConfigPath(scope), { method: 'DELETE', signal })
}

// -- workflow canvas editor: definitions + bindings (§25.10/§25.11/§25.12). --

/** listWorkflowDefinitions calls GET /api/workflow-definitions -- every workflow_definitions row, built-in and custom alike, each carrying its own full document (steps, each with its own outgoing edges) -- a definition is ONE document, never assembled from per-step/per-edge routes (§25.10: no such routes exist). Maintainer+ only server-side (authz.ActionManageWorkflowDefinitions). */
export function listWorkflowDefinitions(signal?: AbortSignal): Promise<ListWorkflowDefinitionsResponse> {
  return request<ListWorkflowDefinitionsResponse>('/api/workflow-definitions', { signal })
}

/**
 * createWorkflowDefinition calls POST /api/workflow-definitions -- EITHER a
 * whole new document (body.sourceDefinitionId null; lane/steps required) OR
 * a deep duplicate of an existing definition (body.sourceDefinitionId set;
 * lane/steps ignored, inherited from the source). The duplicate path is this
 * screen's own one-click escape hatch out of every structural edit refusal
 * (built-in / bound / has run history, §25.10) -- the copy always lands
 * unbound, not built-in, at version 1, whatever it was copied from.
 * Maintainer+ only server-side.
 */
export function createWorkflowDefinition(body: CreateWorkflowDefinitionRequest, signal?: AbortSignal): Promise<WorkflowDefinition> {
  return request<WorkflowDefinition>('/api/workflow-definitions', { method: 'POST', body, signal })
}

/**
 * putWorkflowDefinition calls PUT /api/workflow-definitions/:id -- the
 * complete desired state (name + steps, each with its own outgoing edges),
 * never a partial patch (§25.10, mirrors putRepoSettings' own "always full
 * state" convention). Maintainer+ only server-side; refused unconditionally
 * (409, a real ApiError this call surfaces to the caller verbatim -- never
 * swallowed) when the target definition is built-in, bound by any
 * workflow_bindings row, or has run history -- all three are STRUCTURAL
 * refusals, never an RBAC row, so an admin gets the identical refusal a
 * maintainer does (workflowdefinitions.go's own refusalReasonForMutation).
 */
export function putWorkflowDefinition(id: string, body: UpdateWorkflowDefinitionRequest, signal?: AbortSignal): Promise<WorkflowDefinition> {
  return request<WorkflowDefinition>(`/api/workflow-definitions/${encodeURIComponent(id)}`, { method: 'PUT', body, signal })
}

/** listWorkflowBindings calls GET /api/workflow-bindings -- every (lane, repo) binding, the 3 seeded global rows always included (§25.4: the global binding is never absent). Maintainer+ only server-side -- the SAME read gate as listWorkflowDefinitions above: an editor needs to see current bindings to know which definitions are safe to edit (the "unbound draft" check, §25.11's own amendment). */
export function listWorkflowBindings(signal?: AbortSignal): Promise<ListWorkflowBindingsResponse> {
  return request<ListWorkflowBindingsResponse>('/api/workflow-bindings', { signal })
}

/**
 * putWorkflowBinding calls PUT /api/workflow-bindings -- binds (lane,
 * repoFullName) to workflowDefinitionId at that definition's CURRENT
 * version, pinned server-side (never a client-supplied value). repoFullName
 * null targets the global (org-wide) binding for lane; a non-null
 * 'owner/repo' targets that repo's own override, shadowing the global
 * binding for that one repo only (§25.4). Admin-only server-side
 * (authz.ActionActivateWorkflowBinding, §25.11) -- activation is the ONE
 * action that changes what actually drives production dispatch, so this is
 * sent unconditionally regardless of the calling component's own role
 * check; a 403 the server returns surfaces as a genuine ApiError, an
 * authorization refusal, never a generic "save failed" (mirrors
 * settingsAuthorization.test.ts's own established proof for this exact
 * client-always-sends-the-real-request pattern).
 */
export function putWorkflowBinding(body: PutWorkflowBindingRequest, signal?: AbortSignal): Promise<WorkflowBinding> {
  return request<WorkflowBinding>('/api/workflow-bindings', { method: 'PUT', body, signal })
}

// -- workflow runs & the human decision gate (§25.9/§25.10, §25.15): the
// run view's own two reads plus the HITL decide endpoint. --

/** listSessionWorkflowRuns calls GET /api/sessions/:id/workflow-runs -- this session's own workflow_runs rows, newest first (§25.10). Same session-read gate as every other /api/sessions/:id/... route in this file (every role including viewer, mirrors listArtifacts/listPlans' own identical precedent) -- unlike listWorkflowDefinitions/listWorkflowBindings above, there is no separate maintainer+ gate on this read. */
export function listSessionWorkflowRuns(sessionId: string, signal?: AbortSignal): Promise<ListWorkflowRunsResponse> {
  return request<ListWorkflowRunsResponse>(`/api/sessions/${encodeURIComponent(sessionId)}/workflow-runs`, { signal })
}

/** getWorkflowRun calls GET /api/workflow-runs/:runId -- the run WITH its ordered step runs (§25.10: "a run without its steps answers no question anybody asks"). Same session-read gate as listSessionWorkflowRuns above, resolved server-side via this run's own sessionId -- the URL itself carries no sessionId. */
export function getWorkflowRun(runId: string, signal?: AbortSignal): Promise<WorkflowRunDetail> {
  return request<WorkflowRunDetail>(`/api/workflow-runs/${encodeURIComponent(runId)}`, { signal })
}

/**
 * decideWorkflowStep calls POST /api/workflow-runs/:runId/steps/:stepRunId/decide
 * (§25.9) -- approve/reject/revise the human decision gate the engine parks
 * a run's own attempt on. Own/joined-aware server-side
 * (authz.ActionDecideWorkflowStep, the SAME matrix row as
 * ActionApprovePlan, §25.11) -- sent unconditionally regardless of what the
 * calling component's own client-side approximation rendered enabled,
 * mirroring approvePlan/rejectPlan's own identical "the client always sends
 * the real request" precedent above; a caller not actually authorized gets
 * a real 403 back. A 409 means the target attempt was already decided (by
 * this verdict or another), or stepRunId/runId named a stale/mismatched
 * pair -- the server's own guarded UPDATE (decideworkflowstep.go). verdict
 * 'revise' requires body.text non-empty; the server rejects a blank one
 * with its own 400 regardless of what this call is given, so any
 * client-side check is a courtesy, never the real guarantee.
 */
export function decideWorkflowStep(runId: string, stepRunId: string, body: WorkflowStepDecideRequest, signal?: AbortSignal): Promise<WorkflowStepDecideResponse> {
  return request<WorkflowStepDecideResponse>(`/api/workflow-runs/${encodeURIComponent(runId)}/steps/${encodeURIComponent(stepRunId)}/decide`, { method: 'POST', body, signal })
}

export type { Environment, Member, AuditLogEntry, SandboxSecret, ProviderCredential, CloudIdentityBinding, ClusterBinding, OpenCodeConfig, ReviewAnalytics, RepoDigestScope, WorkflowDefinition, WorkflowBinding }
