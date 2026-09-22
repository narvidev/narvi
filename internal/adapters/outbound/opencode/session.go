package opencode

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// fallbackModel is this adapter's own pinned, last-resort default model
// reference (§7: "Model catalog: injected server-side config; must survive
// OpenCode upgrades -- on empty/failed catalog, fall back to a pinned
// known-good set"). NOT verified against any specific OpenCode
// installation's own live catalog — deliberately: the whole point of a
// pinned fallback is to survive an upgrade that silently drops or renames
// whatever this adapter would otherwise trust. Not specified by the plan;
// chosen as a well-known, broadly-available model slug, the same
// "not specified in the plan; chosen" spirit as platform/timeouts.go's own
// invented defaults.
const fallbackModel = "anthropic/claude-sonnet-4-5"

// resolveSession implements the Prompt command's own resume-vs-fresh
// contract (§3.3, commands.schema.json's own Prompt.conversationId doc
// comment): a set cmd.ConversationId is reused directly as the OpenCode
// sessionID (no new POST /session call); nil/absent creates one via
// POST /session.
func (a *Adapter) resolveSession(ctx context.Context, cmd sandboxws.Prompt) (string, error) {
	if cmd.ConversationId != nil && *cmd.ConversationId != "" {
		return *cmd.ConversationId, nil
	}

	var resp sessionResponse
	if err := a.doJSON(ctx, http.MethodPost, "/session", nil, &resp); err != nil {
		return "", fmt.Errorf("opencode: create session: %w", err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("opencode: create session: response carried no id")
	}
	return resp.ID, nil
}

// resolveModelForced implements §7's own minimal model-catalog-fallback
// quirk, and is now the ONLY model-resolution path this adapter has —
// audit fix (§7.3, "the model is empty in the default configuration"): an
// EARLIER version of this file also had a plain resolveModel, which
// treated a nil raw (cmd.Model omitted — Composer/PlanModeView/Timeline
// resume/DecisionInbox all dispatch this way by default, web/src/session)
// as "omit the wire model field entirely, let OpenCode use its own
// configured default", and this adapter never learned which concrete
// model OpenCode actually ran. That meant ProviderFailureDiagnostic.Model
// (diagnostic.go) — the very fact §7.3 names FIRST as missing from the
// pre-Step status quo — stayed "" on the overwhelmingly common,
// no-model-requested path, silently defeating this Step's own purpose.
// Removed rather than kept as a second, rarely-taken branch: this Step's
// OWN "typed transient-error retry" work already established the
// precedent that a RETRIED dispatch (attemptCompactionRetry, adapter.go)
// always forces a concrete model via this exact function, even when the
// original request named none — extending that SAME, already-accepted
// resolution strategy to the FIRST dispatch (StartTurn, adapter.go) and to
// attemptTransientRetry's own re-dispatch (adapter.go) is a narrowing of
// existing behavior, not a new category of risk, and it is the only way
// every ProviderFailureDiagnostic this adapter ever builds can honestly
// keep its own doc comment's promise: "the model this turn actually
// dispatched with" (diagnostic.go), true of every path that can set it,
// never "" for a real turn.
//
// raw is cmd.Model (sandboxws.PromptModel, a *string) as a plain *string.
// nil (VERIFIED live via GET /doc, the real OpenAPI schema: /summarize's
// requestBody schema is {"providerID","modelID","auto"?}, with BOTH
// providerID and modelID listed under "required", and an empty {} body
// independently reproduced live to return HTTP 400
// {"name":"BadRequest","data":{"message":"Missing key\n  at
// [\"providerID\"]"}} against the pinned OpenCode 1.17.15 binary — so
// /summarize never had an "omit" option to begin with) falls back to
// fallbackModelRef() directly, no catalog call needed (matching
// resolveProviderModel's own "unparseable raw skips the catalog entirely"
// branch, reused here rather than duplicated). A non-nil raw is resolved
// via resolveProviderModel below. The return value is NEVER nil — every
// caller (StartTurn's own initial dispatch, forceCompaction, and every
// retried postPromptAsync re-dispatch, adapter.go) can rely on that.
func (a *Adapter) resolveModelForced(ctx context.Context, raw *string) *promptModelRef {
	if raw == nil {
		return fallbackModelRef()
	}
	return a.resolveProviderModel(ctx, *raw)
}

// resolveProviderModel is the actual catalog-lookup logic
// resolveModelForced above shares for a non-nil raw value: a raw string
// that does not parse as "provider/model", or a best-effort GET /api/model
// catalog call that fails or returns empty, falls back to fallbackModel —
// deliberately minimal: this does NOT check whether the requested model is
// actually present in the catalog, only that a live catalog exists at all
// (§7's own framing: a version bump silently dropping the catalog entirely
// is what this guards against, not per-request model validation — a
// "fallback of last resort", not a rich model-selection feature).
func (a *Adapter) resolveProviderModel(ctx context.Context, raw string) *promptModelRef {
	providerID, modelID, ok := strings.Cut(raw, "/")
	if !ok || providerID == "" || modelID == "" {
		return fallbackModelRef()
	}

	var catalog modelCatalogResponse
	if err := a.doJSON(ctx, http.MethodGet, "/api/model", nil, &catalog); err != nil || len(catalog.Data) == 0 {
		return fallbackModelRef()
	}

	return &promptModelRef{ProviderID: providerID, ModelID: modelID}
}

func fallbackModelRef() *promptModelRef {
	providerID, modelID, _ := strings.Cut(fallbackModel, "/")
	return &promptModelRef{ProviderID: providerID, ModelID: modelID}
}

// postPromptAsync POSTs the translated turn to OpenCode's own
// prompt_async endpoint (§7: "POSTs prompt_async"). model is already
// resolved (resolveModelForced) before this is called.
//
// cmd.PlanMode ("plan mode, web", §8.1) selects OpenCode's own
// native "plan" agent via the request's "agent" field when true, omitted
// (OpenCode's own default "build" agent) otherwise -- see
// promptAsyncRequest's own doc comment (types.go) for the full,
// empirically-verified rationale and its honest scope limits.
//
// cmd.Effort (§29.8: "reasoning-effort overrides") maps directly onto the
// request's own "variant" field when non-nil -- OpenCode's own per-model
// "variants" catalog (GET /provider) names each valid value and its
// provider-native meaning (openai: none/low/medium/high/xhigh ->
// reasoningEffort; anthropic: low/medium/high/(xhigh/)max -> adaptive
// thinking effort). Narvi validates nothing but nullability here, exactly
// like model itself (resolveProviderModel's own doc comment) -- valid
// values are per-model facts owned by OpenCode's catalog, not a
// Narvi-side allowlist.
//
// HONEST GAP: cmd.ScmName/cmd.ScmEmail (§6.1: Prompt "with author
// scmName/scmEmail for git attribution") are deliberately NOT threaded
// into this request or anywhere else in this package -- this Step's live
// research against the real /doc OpenAPI spec found no prompt_async (or
// any other OpenCode endpoint) accepting a git-author override. Real
// per-user git identity (attributing a commit to the actual prompting
// human) is explicitly out of scope for this Step and remains
// unscheduled -- cmd carries both fields end to end (they arrive on the
// wire Prompt command and reach this function unused), so no information
// is lost; a future Step can wire them once such a mechanism exists.
func (a *Adapter) postPromptAsync(ctx context.Context, sessionID string, cmd sandboxws.Prompt, model *promptModelRef) error {
	body := promptAsyncRequest{
		Model:   model,
		Parts:   []promptPartInput{{Type: "text", Text: cmd.Text}},
		Variant: cmd.Effort,
	}
	switch {
	case cmd.PlanMode:
		agent := planAgentName
		body.Agent = &agent
	case a.capabilityRestricted:
		// (§17.2): a.capabilityRestricted is set ONCE, at
		// construction (cmd/sandbox-agent/main.go, from SessionConfig.
		// CapabilityRestricted -- true exactly for a sentinel-auto-fix
		// child session) -- every BUILD-mode turn on such a session uses
		// OpenCode's own glob-restricted "sentinel-fix" custom agent
		// (sentinelfixagent.go) instead of the ordinary "build" agent.
		// PlanMode still takes precedence above (a sentinel-auto-fix
		// session is dispatched directly in build mode by design, §17.2,
		// so this branch is not expected to be reached in practice, but
		// is not disallowed either).
		agent := sentinelFixAgentName
		body.Agent = &agent
	}
	path := "/session/" + url.PathEscape(sessionID) + "/prompt_async"
	return a.doJSON(ctx, http.MethodPost, path, body, nil)
}

// postAbort aborts sessionID's own in-flight turn, if any — this Step's
// own live research against the real OpenCode 1.17.15 binary found
// POST /session/{id}/abort as the real endpoint the wire "stop" command
// maps directly onto (§7 names "stop" as a command this adapter must
// handle, but does not itself name this specific OpenCode endpoint).
// OpenCode's own response is a bare true/false (VERIFIED live) — neither
// outcome is
// an error: false simply means there was nothing in flight to abort,
// exactly matching this adapter's own "Stop with nothing in flight is a
// safe no-op" contract (ports.AgentRuntime.Stop's own doc comment).
func (a *Adapter) postAbort(ctx context.Context, sessionID string) error {
	path := "/session/" + url.PathEscape(sessionID) + "/abort"
	var aborted bool
	return a.doJSON(ctx, http.MethodPost, path, nil, &aborted)
}

// fetchFinalMessages implements §7's own "final-state fetch fallback"
// quirk: GET /session/{id}/message, used only once the SSE stream has
// gone inactive for longer than platform.Timeouts.SSEInactivityTimeout
// without session.idle/session.error ever arriving.
func (a *Adapter) fetchFinalMessages(ctx context.Context, sessionID string) ([]messageListEntry, error) {
	path := "/session/" + url.PathEscape(sessionID) + "/message"
	var entries []messageListEntry
	if err := a.doJSON(ctx, http.MethodGet, path, nil, &entries); err != nil {
		return nil, fmt.Errorf("opencode: fetch final messages: %w", err)
	}
	return entries, nil
}
