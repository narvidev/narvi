package providercredential

import (
	"strings"
	"testing"
)

// TestUserScopeTarget pins the one rule over every kind of session the
// control plane creates. The integration agreement test
// (httpapi's TestProviderCredentialResolution_SitesAgree) runs the same
// kinds through both callers on real Postgres.
func TestUserScopeTarget(t *testing.T) {
	const creator = "0b7e2f64-8d7a-4b8e-9d5f-3a1c2e4f5a6b"
	tests := []struct {
		name     string
		origin   SessionOrigin
		wantUser string
		wantOK   bool
	}{
		{"web session its owner created", SessionOrigin{CreatedBy: creator}, creator, true},
		{"multiplayer session resolves its creator's link", SessionOrigin{CreatedBy: creator}, creator, true},
		{"review session", SessionOrigin{CreatedBy: creator, PullRequestReview: true}, "", false},
		{"review session with no linked requester", SessionOrigin{PullRequestReview: true}, "", false},
		{"automation session", SessionOrigin{}, "", false},
		{"child session with a creator", SessionOrigin{CreatedBy: creator, Child: true}, "", false},
		{"child session with no creator", SessionOrigin{Child: true}, "", false},
		{"child of a review session that is itself a review session", SessionOrigin{CreatedBy: creator, Child: true, PullRequestReview: true}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotUser, gotOK := UserScopeTarget(tc.origin)
			if gotUser != tc.wantUser || gotOK != tc.wantOK {
				t.Errorf("UserScopeTarget(%+v) = (%q, %v), want (%q, %v)", tc.origin, gotUser, gotOK, tc.wantUser, tc.wantOK)
			}
		})
	}
}

func TestModelProvider(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		want   Provider
		wantOK bool
	}{
		{"openai", "openai/gpt-5.4", ProviderOpenAI, true},
		{"anthropic", "anthropic/claude-opus-4-5", ProviderAnthropic, true},
		{"google", "google/gemini-3-pro", ProviderGoogle, true},
		{"case and space", "  OpenAI/gpt-5.4 ", ProviderOpenAI, true},
		{"no slash", "gpt-5.4", "", false},
		{"empty", "", "", false},
		{"provider with no stored credentials", "opencode/free-model", "", false},
		{"blank provider", "/gpt-5.4", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ModelProvider(tc.model)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("ModelProvider(%q) = (%q, %v), want (%q, %v)", tc.model, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestPersonalLinkOnly(t *testing.T) {
	openai := map[Provider]bool{ProviderOpenAI: true}
	anthropic := map[Provider]bool{ProviderAnthropic: true}
	tests := []struct {
		name       string
		model      string
		resolvable map[Provider]bool
		personal   map[Provider]bool
		want       Provider
		wantRefuse bool
	}{
		{"only the creator's link carries the provider", "openai/gpt-5.4", anthropic, openai, ProviderOpenAI, true},
		{"nothing resolves and the link carries it", "openai/gpt-5.4", nil, openai, ProviderOpenAI, true},
		{"the deployment carries it too", "openai/gpt-5.4", openai, openai, "", false},
		{"no link carries it either", "openai/gpt-5.4", anthropic, nil, "", false},
		{"the link carries another provider", "anthropic/claude-opus-4-5", anthropic, openai, "", false},
		{"a model naming no stored provider", "opencode/free-model", nil, openai, "", false},
		{"no model shape", "gpt-5.4", nil, openai, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, refuse := PersonalLinkOnly(tc.model, tc.resolvable, tc.personal)
			if got != tc.want || refuse != tc.wantRefuse {
				t.Errorf("PersonalLinkOnly(%q) = (%q, %v), want (%q, %v)", tc.model, got, refuse, tc.want, tc.wantRefuse)
			}
		})
	}
}

func TestPersonalLinkOnlyMessage_NamesTheRefusalFirst(t *testing.T) {
	msg := PersonalLinkOnlyMessage("openai/gpt-5.4", ProviderOpenAI)
	if !strings.HasPrefix(msg, string(RefusalPersonalLinkOnly)+": ") {
		t.Errorf("message = %q, want it to start with %q", msg, string(RefusalPersonalLinkOnly)+": ")
	}
	for _, want := range []string{"openai/gpt-5.4", "openai link", "deployment credential for openai"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want it to contain %q", msg, want)
		}
	}
}
