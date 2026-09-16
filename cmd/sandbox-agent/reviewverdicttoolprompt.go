// This file (reviewverdicttoolprompt.go) closes the other half of
// §8.2's own ("server-side verdict", cf. §5.2/§21.2) verdict-posting tool:
// internal/domain/review.RenderTurnPrompt (§8.2) renders a review
// turn's prompt with a FIXED, deterministic block instructing the agent
// how to call POST /sessions/{sessionID}/review/verdict -- but that
// package runs at TURN-CREATION time, in the control plane, before any
// sandbox exists (a brand-new review session) or before any respawn of an
// existing one (a NEW gen, a NEW rotated §5.2 token) -- so it can only
// ever emit PLACEHOLDER tokens (review.VerdictToolURLPlaceholder et al.,
// see that package's own doc comment) in place of this turn's real,
// live, CURRENT-gen URL/bearer/gen.
//
// §8.6 ("uploads, blob storage & the in-sandbox download_file tool",
// §28.5) extends this SAME mechanism for a second tool, never a second
// substitution scheme: internal/domain/upload's own
// BaseURLPlaceholder/BearerPlaceholder/GenPlaceholder (rendered into a
// build turn's own attachment block and upload-tool note by
// internal/adapters/inbound/httpapi's createTurnLocked, at turn-creation
// time, for the identical "no live sandbox/gen yet" reason review's own
// placeholders exist) are resolved by renderUploadToolPromptText below,
// via the SAME controlPlaneHTTPBase derivation reviewVerdictToolURL
// itself is built on -- BaseURLPlaceholder deliberately expands to
// scheme://host ONLY (no path): unlike the verdict tool's one fixed path,
// an attachment block can name an arbitrary number of per-attachment
// paths, each already baked into the rendered text verbatim (sessionID/
// uploadID are non-secret, already-known server-generated UUIDs at
// render time -- see internal/domain/upload/prompt.go's own doc comment).
//
// This sandbox-agent process is the ONE place in the whole system where a
// specific, about-to-run turn's sessionID, SandboxToken, and Gen are all
// simultaneously and CURRENTLY in scope together -- it already holds all
// three (cfg.SessionConfig, read from its own NARVI_SESSION_CONFIG at
// boot) for the sandbox WS handshake and the scm-credentials/
// snapshot-mint calls it already makes. renderVerdictToolPromptText/
// renderUploadToolPromptText below substitute their own placeholders for
// those real, live values immediately before a "prompt" command's own
// Text is handed to OpenCode (commandHandler.HandlePrompt, main.go) --
// never persisted, never logged.
//
// Deliberately unconditional, with no "is this a review/upload-carrying
// turn" flag anywhere on the wire (sandboxws.Prompt, §6.1, carries none,
// and this file adds none): a prompt with none of a given function's own
// placeholders -- every turn that function's own producer never rendered
// -- is returned byte-for-byte unchanged by strings.ReplaceAll's own
// documented no-op behavior when the substring it is asked to replace is
// absent. Each placeholder set's own presence already is the only signal
// its own substitution needs.
package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/review"
	domainupload "github.com/narvidev/narvi/internal/domain/upload"
)

// renderVerdictToolPromptText substitutes review.VerdictToolURLPlaceholder/
// VerdictToolBearerPlaceholder/VerdictToolGenPlaceholder/
// VerdictToolDispatchMessageIDPlaceholder in text with this sandbox's OWN,
// live, current-gen values, derived from cfg -- see this file's own top
// doc comment for the full rationale. cfg == nil (should be unreachable --
// commandHandler.HandlePrompt only ever calls this when
// h.cfg.SessionConfig is already known non-nil, see run()'s own
// "commandHandler only ever constructed within that same cfg.SessionConfig
// != nil branch" precedent) returns text unchanged, matching this
// package's existing "no live session, nothing to do" discipline
// elsewhere (e.g. HandlePrompt/HandleStop's own h.adapter == nil guards).
//
// dispatchMessageID (finding F3 (§21.1's amendment)) is DIFFERENT in kind from
// cfg's own three fields: it is not fixed at sandbox BOOT time (cfg is),
// it is the WIRE Prompt's own MessageId for THIS SPECIFIC "prompt"
// command -- the caller (HandlePrompt) already has it in scope as
// cmd.MessageId, and passes it straight through. This is the one
// identifier httpapi.PostReviewVerdict later resolves the posting turn
// BY, instead of "whichever turn is processing for this session right
// now" -- see review.VerdictToolDispatchMessageIDPlaceholder's own doc
// comment for the hazard this closes.
//
// A malformed cfg.ControlPlaneWsUrl (reviewVerdictToolURL's own error
// path) is logged and otherwise treated as "nothing to substitute" --
// this is an existing, separate bug in this sandbox's own boot config were
// it ever to happen, not something this function should newly promote to
// turn-fatal: a review agent that cannot resolve the placeholders simply
// cannot call the tool this turn, exactly as if this substitution had
// never run at all (it never has, before this Step).
func renderVerdictToolPromptText(text string, cfg *sessionconfig.SessionConfig, dispatchMessageID string) string {
	if cfg == nil {
		return text
	}
	if !strings.Contains(text, review.VerdictToolURLPlaceholder) &&
		!strings.Contains(text, review.VerdictToolBearerPlaceholder) &&
		!strings.Contains(text, review.VerdictToolGenPlaceholder) &&
		!strings.Contains(text, review.VerdictToolDispatchMessageIDPlaceholder) {
		// The overwhelming common case (every non-review turn): nothing to
		// substitute, skip deriving a URL at all.
		return text
	}

	verdictURL, err := reviewVerdictToolURL(cfg.ControlPlaneWsUrl, cfg.SessionId)
	if err != nil {
		slog.Warn("sandbox-agent: derive review-verdict-tool URL failed; leaving prompt placeholders unresolved", "error", err)
		return text
	}

	text = strings.ReplaceAll(text, review.VerdictToolURLPlaceholder, verdictURL)
	text = strings.ReplaceAll(text, review.VerdictToolBearerPlaceholder, cfg.SandboxToken)
	text = strings.ReplaceAll(text, review.VerdictToolGenPlaceholder, strconv.Itoa(cfg.Gen))
	text = strings.ReplaceAll(text, review.VerdictToolDispatchMessageIDPlaceholder, dispatchMessageID)
	return text
}

// renderUploadToolPromptText substitutes internal/domain/upload's own
// BaseURLPlaceholder/BearerPlaceholder/GenPlaceholder with this sandbox's
// OWN, live, current-gen values -- see this file's own top doc comment
// for the full rationale, and internal/domain/upload/prompt.go's own doc
// comment for why BaseURLPlaceholder is scheme://host ONLY, no path.
// Mirrors renderVerdictToolPromptText's own structure and error handling
// exactly (nil cfg / malformed ControlPlaneWsUrl both degrade to
// "leave placeholders unresolved", never turn-fatal).
func renderUploadToolPromptText(text string, cfg *sessionconfig.SessionConfig) string {
	if cfg == nil {
		return text
	}
	if !strings.Contains(text, domainupload.BaseURLPlaceholder) &&
		!strings.Contains(text, domainupload.BearerPlaceholder) &&
		!strings.Contains(text, domainupload.GenPlaceholder) {
		// The common case (a turn with no attachments -- RenderUploadToolNote
		// is unconditional, so this branch is actually rarer than the
		// verdict tool's own identical fast path, but kept for symmetry and
		// to skip the URL derivation below whenever genuinely nothing needs
		// it, e.g. a plain string field neither renderer ever touches).
		return text
	}

	base, err := controlPlaneHTTPBase(cfg.ControlPlaneWsUrl)
	if err != nil {
		slog.Warn("sandbox-agent: derive upload-tool base URL failed; leaving prompt placeholders unresolved", "error", err)
		return text
	}

	text = strings.ReplaceAll(text, domainupload.BaseURLPlaceholder, base)
	text = strings.ReplaceAll(text, domainupload.BearerPlaceholder, cfg.SandboxToken)
	text = strings.ReplaceAll(text, domainupload.GenPlaceholder, strconv.Itoa(cfg.Gen))
	return text
}

// controlPlaneHTTPBase derives the scheme://host prefix (no path) from
// controlPlaneWsURL (SessionConfig.ControlPlaneWsUrl, a
// ws(s)://.../sessions/{id}/ws?type=sandbox URL, §6.1) -- mirrors
// internal/sandboxagent/credentials.NewCPClient's own identical
// wss->https/ws->http scheme-swap-plus-loopback-guard exactly (that
// package's own doc comment: "there is no separate REST base URL field in
// SESSION_CONFIG... this is a documented, reasoned derivation"),
// duplicated here rather than exported cross-package for such a small,
// one-off derivation -- the same "small, local, dependency-free helper"
// precedent internal/adapters/inbound/httpapi's own sessionRepoHosts
// establishes elsewhere in this codebase. The loopback guard matters just
// as much here as it does in NewCPClient: this base URL feeds both
// reviewVerdictToolURL below and renderUploadToolPromptText above, each
// about to embed it (plus a bearer token) verbatim in a turn's own prompt
// text, so a plaintext ws:// control plane on anything but loopback would
// mean this sandbox token travels in the clear the moment the agent
// actually calls it -- refused here for the identical reason NewCPClient
// refuses it.
func controlPlaneHTTPBase(controlPlaneWsURL string) (string, error) {
	parsed, err := url.Parse(controlPlaneWsURL)
	if err != nil {
		return "", fmt.Errorf("sandbox-agent: parse control plane ws url %q: %w", controlPlaneWsURL, err)
	}

	var httpScheme string
	switch parsed.Scheme {
	case "wss":
		httpScheme = "https"
	case "ws":
		if !isLoopbackHost(parsed.Host) {
			return "", fmt.Errorf("sandbox-agent: plaintext ws:// control plane url is only allowed for a loopback host, got %q", parsed.Host)
		}
		httpScheme = "http"
	default:
		return "", fmt.Errorf("sandbox-agent: control plane ws url %q has unrecognized scheme %q, want ws or wss", controlPlaneWsURL, parsed.Scheme)
	}

	return httpScheme + "://" + parsed.Host, nil
}

// reviewVerdictToolURL derives the CP-HTTP POST /sessions/{sessionID}/
// review/verdict URL by appending its own fixed path onto
// controlPlaneHTTPBase's own scheme://host derivation above.
func reviewVerdictToolURL(controlPlaneWsURL, sessionID string) (string, error) {
	base, err := controlPlaneHTTPBase(controlPlaneWsURL)
	if err != nil {
		return "", err
	}
	return base + "/sessions/" + url.PathEscape(sessionID) + "/review/verdict", nil
}

// isLoopbackHost reports whether hostport's host component (a bare
// hostname, or "host:port") is loopback -- "localhost", or an IP for which
// net.IP.IsLoopback() is true (127.0.0.0/8, ::1). Duplicates internal/
// sandboxagent/credentials.CPClient's own identical, unexported helper of
// the same name -- see reviewVerdictToolURL's own doc comment for why this
// is a deliberate, small duplication rather than a cross-package export.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
