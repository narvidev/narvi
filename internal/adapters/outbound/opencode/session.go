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

// resolveModel implements §7's own minimal model-catalog-fallback quirk.
// raw is cmd.Model (sandboxws.PromptModel, a *string) as a plain *string —
// nil means "omit model entirely, let OpenCode use its own configured
// default" (never resolved against the catalog or fallback, since there is
// nothing to validate). A non-nil value is resolved via resolveProviderModel
// below (shared with resolveModelForced, §7.2).
//
// This is the ONLY correct resolution for a wire dispatch whose own "model"
// field is genuinely optional (promptAsyncRequest.Model, types.go, "model"?
// in the real /doc OpenAPI schema) — StartTurn's own initial dispatch and
// attemptTransientRetry's own re-dispatch (adapter.go) both use this,
// exactly matching Composer/PlanModeView/Timeline resume/DecisionInbox's
// own default `modelId: null` (web/src/session): a request that names no
// model must NOT grow one on the wire that the client never asked for — an
// audit fix (§7.3) undoing an EARLIER, incorrect version of this function
// that force-resolved a nil raw to fallbackModelRef() here too, silently
// changing which model every default-configuration turn actually ran on
// (a production behavior change, not merely a diagnostic-completeness
// one — see ProviderFailureDiagnostic.Model's own doc comment, diagnostic.go,
// for why the diagnostic's own accuracy is now solved a different way, one
// that does not require this).
func (a *Adapter) resolveModel(ctx context.Context, raw *string) *promptModelRef {
	if raw == nil {
		return nil
	}
	return a.resolveProviderModel(ctx, *raw)
}

// resolveModelForced implements §7.2's own "forced" model-resolution
// variant, needed because POST /session/{id}/summarize has NO "omit and
// let OpenCode pick" option the way prompt_async's own optional "model"
// field does — VERIFIED live via GET /doc (the real OpenAPI schema):
// /summarize's requestBody schema is {"providerID","modelID","auto"?},
// with BOTH providerID and modelID listed under "required", and an empty
// {} body independently reproduced live to return HTTP 400
// {"name":"BadRequest","data":{"message":"Missing key\n  at
// [\"providerID\"]"}} against the pinned OpenCode 1.17.15 binary. So
// unlike resolveModel above, raw == nil here does NOT mean "omit" — there
// is nothing to omit onto — it is treated exactly the same as resolveModel
// treats a non-nil-but-unparseable raw: fall back to fallbackModelRef()
// directly, no catalog call needed (matching resolveProviderModel's own
// "unparseable raw skips the catalog entirely" branch, reused here rather
// than duplicated). A non-nil raw goes through the SAME
// resolveProviderModel resolution resolveModel itself uses. The return
// value is NEVER nil — every caller (forceCompaction, and the retried
// postPromptAsync that reuses the SAME resolved model, adapter.go's
// attemptCompactionRetry) can rely on that.
//
// attemptCompactionRetry (adapter.go) is the only caller that also reuses
// this forced model for its own RE-dispatch of the prompt, not only for
// forceCompaction's own /summarize call — deliberate: a compaction retry
// already forces a concrete model for /summarize regardless of what the
// original request named, so re-dispatching the retried prompt with that
// SAME resolved model (rather than going back through resolveModel and
// possibly omitting it) keeps both calls of one recovery attempt naming
// the same model, instead of compacting on one model and replying on
// whatever OpenCode would have defaulted to. attemptTransientRetry
// (adapter.go), which never calls forceCompaction at all, has no such
// reason to force one — it re-dispatches via resolveModel instead, so a
// transient-error retry's own request shape matches the original attempt
// it is retrying.
func (a *Adapter) resolveModelForced(ctx context.Context, raw *string) *promptModelRef {
	if raw == nil {
		return fallbackModelRef()
	}
	return a.resolveProviderModel(ctx, *raw)
}

// resolveProviderModel is the actual catalog-lookup logic resolveModel and
// resolveModelForced above BOTH share for a non-nil raw value: a raw string
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
// resolved (resolveModel for an ordinary dispatch, or resolveModelForced
// for attemptCompactionRetry's own retry, session.go) before this is
// called — nil is a legitimate value here (resolveModel's own "omit"
// case), matching promptAsyncRequest.Model's own "model,omitempty" tag.
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
