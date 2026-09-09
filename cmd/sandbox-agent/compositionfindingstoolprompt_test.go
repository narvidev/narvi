// This file (deliberately NOT behind the "integration" build tag, mirrors
// epistemicoutcometoolprompt_test.go/reviewverdicttoolprompt_test.go's
// own identical precedent) proves renderCompositionFindingsToolPromptText/
// compositionFindingsToolURL (compositionfindingstoolprompt.go) directly,
// in-process.
package main

import (
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/review"
)

// TestRenderCompositionFindingsToolPromptText mirrors
// TestRenderEpistemicOutcomeToolPromptText's own table shape exactly
// (§15.3): the SAME mechanism, a fourth placeholder set,
// resolved against the SAME controlPlaneHTTPBase derivation
// reviewVerdictToolURL/epistemicOutcomeToolURL both share.
func TestRenderCompositionFindingsToolPromptText(t *testing.T) {
	t.Parallel()

	promptText := review.RenderCompositionReviewPrompt("Review this release's composition.", review.PreFetchedContext{})

	tests := []struct {
		name            string
		text            string
		cfg             *sessionconfig.SessionConfig
		wantExact       string
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:      "no placeholders present: byte-for-byte no-op regardless of cfg",
			text:      "an ordinary build turn's own prompt, nothing composition-review-shaped here",
			cfg:       &sessionconfig.SessionConfig{ControlPlaneWsUrl: "wss://cp.example.com/sessions/abc/ws?type=sandbox", SessionId: "abc", SandboxToken: "tok", Gen: 3},
			wantExact: "an ordinary build turn's own prompt, nothing composition-review-shaped here",
		},
		{
			name:      "nil cfg: no-op even with placeholders present",
			text:      promptText,
			cfg:       nil,
			wantExact: promptText,
		},
		{
			name: "composition-review turn, production wss:// control plane: all three placeholders resolved",
			text: promptText,
			cfg: &sessionconfig.SessionConfig{
				ControlPlaneWsUrl: "wss://cp.example.com/sessions/session-123/ws?type=sandbox",
				SessionId:         "session-123",
				SandboxToken:      "s3cr3t-token",
				Gen:               7,
			},
			wantContains: []string{
				"POST https://cp.example.com/sessions/session-123/release-manifest/composition-findings",
				"Authorization: Bearer s3cr3t-token",
				"X-Sandbox-Gen: 7",
			},
			wantNotContains: []string{
				review.CompositionFindingsToolURLPlaceholder, review.CompositionFindingsToolBearerPlaceholder, review.CompositionFindingsToolGenPlaceholder,
			},
		},
		{
			name: "composition-review turn, loopback ws:// control plane (dev/test): resolved via http",
			text: promptText,
			cfg: &sessionconfig.SessionConfig{
				ControlPlaneWsUrl: "ws://127.0.0.1:8080/sessions/session-9/ws?type=sandbox",
				SessionId:         "session-9",
				SandboxToken:      "dev-token",
				Gen:               1,
			},
			wantContains: []string{
				"POST http://127.0.0.1:8080/sessions/session-9/release-manifest/composition-findings",
				"Authorization: Bearer dev-token",
				"X-Sandbox-Gen: 1",
			},
		},
		{
			name: "composition-review turn, non-loopback ws:// control plane: refused, placeholders left unresolved",
			text: promptText,
			cfg: &sessionconfig.SessionConfig{
				ControlPlaneWsUrl: "ws://cp.example.com/sessions/session-5/ws?type=sandbox",
				SessionId:         "session-5",
				SandboxToken:      "should-never-appear",
				Gen:               2,
			},
			wantExact: promptText,
		},
		{
			name: "composition-review turn, malformed control plane url: refused, placeholders left unresolved",
			text: promptText,
			cfg: &sessionconfig.SessionConfig{
				ControlPlaneWsUrl: "://not a url",
				SessionId:         "session-1",
				SandboxToken:      "should-never-appear",
				Gen:               1,
			},
			wantExact: promptText,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := renderCompositionFindingsToolPromptText(tc.text, tc.cfg)

			if tc.wantExact != "" && got != tc.wantExact {
				t.Fatalf("renderCompositionFindingsToolPromptText() = %q, want exactly %q", got, tc.wantExact)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("renderCompositionFindingsToolPromptText() = %q, want it to contain %q", got, want)
				}
			}
			for _, notWant := range tc.wantNotContains {
				if strings.Contains(got, notWant) {
					t.Errorf("renderCompositionFindingsToolPromptText() = %q, want it to NOT contain %q", got, notWant)
				}
			}
		})
	}
}

// TestRenderCompositionFindingsToolPromptText_NeverLeaksTokenWhenNothingToSubstitute
// mirrors TestRenderEpistemicOutcomeToolPromptText_NeverLeaksTokenWhenNothingToSubstitute's
// own identical proof, for the composition-findings-tool placeholder set:
// the byte-for-byte no-op case that matters most in practice, since the
// overwhelming majority of turns are not composition-review turns at all.
func TestRenderCompositionFindingsToolPromptText_NeverLeaksTokenWhenNothingToSubstitute(t *testing.T) {
	t.Parallel()

	const liveToken = "super-secret-live-sandbox-token"
	got := renderCompositionFindingsToolPromptText("build this feature please", &sessionconfig.SessionConfig{
		ControlPlaneWsUrl: "wss://cp.example.com/sessions/abc/ws?type=sandbox",
		SessionId:         "abc",
		SandboxToken:      liveToken,
		Gen:               3,
	})

	if got != "build this feature please" {
		t.Fatalf("renderCompositionFindingsToolPromptText() = %q, want an exact no-op", got)
	}
	if strings.Contains(got, liveToken) {
		t.Errorf("renderCompositionFindingsToolPromptText() leaked the live sandbox token into a prompt that never asked for it: %q", got)
	}
}

// TestCompositionFindingsToolURL is a narrow, direct proof of the URL
// derivation itself, mirroring epistemicOutcomeToolURL's own identical
// test shape one level down.
func TestCompositionFindingsToolURL(t *testing.T) {
	t.Parallel()

	got, err := compositionFindingsToolURL("wss://cp.example.com/sessions/session-42/ws?type=sandbox", "session-42")
	if err != nil {
		t.Fatalf("compositionFindingsToolURL() unexpected error: %v", err)
	}
	want := "https://cp.example.com/sessions/session-42/release-manifest/composition-findings"
	if got != want {
		t.Errorf("compositionFindingsToolURL() = %q, want %q", got, want)
	}
}
