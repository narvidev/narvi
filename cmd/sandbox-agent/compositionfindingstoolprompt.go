// This file (compositionfindingstoolprompt.go) closes the other half of
// The release composition review's own (§15.3/§12.2 item 9)
// composition-findings-posting tool: internal/domain/review.
// RenderCompositionReviewPrompt renders the aggregate-diff composition
// review turn's own prompt with a FIXED, deterministic block instructing
// the agent how to call POST /sessions/{sessionID}/release-manifest/
// composition-findings -- but that package runs at TURN-CREATION time, in
// the control plane, before any sandbox exists for this turn's own
// session -- so it can only ever emit PLACEHOLDER tokens (review.
// CompositionFindingsToolURLPlaceholder et al., see that package's own
// doc comment) in place of this turn's real, live, CURRENT-gen
// URL/bearer/gen.
//
// Mirrors renderVerdictToolPromptText's own mechanism EXACTLY (see
// reviewverdicttoolprompt.go's own top doc comment for the full
// rationale) -- a FOURTH tool sharing the SAME substitution scheme, never
// a second one invented for it: this sandbox-agent process is the ONE
// place in the whole system where a specific, about-to-run turn's
// sessionID, SandboxToken, and Gen are all simultaneously and CURRENTLY
// in scope together (cfg.SessionConfig, read from its own
// NARVI_SESSION_CONFIG at boot).
package main

import (
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/review"
)

// renderCompositionFindingsToolPromptText substitutes review.
// CompositionFindingsToolURLPlaceholder/CompositionFindingsToolBearerPlaceholder/
// CompositionFindingsToolGenPlaceholder in text with this sandbox's OWN,
// live, current-gen values, derived from cfg -- mirrors
// renderVerdictToolPromptText's own structure and error handling exactly
// (nil cfg / malformed ControlPlaneWsUrl both degrade to "leave
// placeholders unresolved", never turn-fatal; see that function's own doc
// comment for the full reasoning, which applies here without
// modification).
func renderCompositionFindingsToolPromptText(text string, cfg *sessionconfig.SessionConfig) string {
	if cfg == nil {
		return text
	}
	if !strings.Contains(text, review.CompositionFindingsToolURLPlaceholder) &&
		!strings.Contains(text, review.CompositionFindingsToolBearerPlaceholder) &&
		!strings.Contains(text, review.CompositionFindingsToolGenPlaceholder) {
		// The overwhelming common case (every turn that isn't a
		// composition-review turn): nothing to substitute, skip deriving a
		// URL at all.
		return text
	}

	toolURL, err := compositionFindingsToolURL(cfg.ControlPlaneWsUrl, cfg.SessionId)
	if err != nil {
		slog.Warn("sandbox-agent: derive composition-findings-tool URL failed; leaving prompt placeholders unresolved", "error", err)
		return text
	}

	text = strings.ReplaceAll(text, review.CompositionFindingsToolURLPlaceholder, toolURL)
	text = strings.ReplaceAll(text, review.CompositionFindingsToolBearerPlaceholder, cfg.SandboxToken)
	text = strings.ReplaceAll(text, review.CompositionFindingsToolGenPlaceholder, strconv.Itoa(cfg.Gen))
	return text
}

// compositionFindingsToolURL derives the CP-HTTP POST /sessions/
// {sessionID}/release-manifest/composition-findings URL by appending its
// own fixed path onto controlPlaneHTTPBase's own scheme://host derivation
// (reviewverdicttoolprompt.go) -- mirrors reviewVerdictToolURL/
// epistemicOutcomeToolURL's own identical shape, one path over.
func compositionFindingsToolURL(controlPlaneWsURL, sessionID string) (string, error) {
	base, err := controlPlaneHTTPBase(controlPlaneWsURL)
	if err != nil {
		return "", err
	}
	return base + "/sessions/" + url.PathEscape(sessionID) + "/release-manifest/composition-findings", nil
}
