package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// This file implements §29.7's own 3 named CI contract cases against the
// REAL pinned OpenCode 1.17.15 binary (mirroring §25.2/§7.2's identical
// pinned-binary discipline) -- an OpenCode version bump that drops or
// reshapes any of these must fail CI, not production. All 3 are
// localhost-only (§29.7: "What CI deliberately does NOT do: call
// auth.openai.com"); none needs skipIfNoProvider (no AI-provider
// credential or model call involved, matching TestSentinelFixAgent_
// RegistersAgainstRealPinnedBinary's own identical "costs nothing to run
// in CI without one configured" reasoning).
//
// Every assertion below was independently, manually verified live against
// this exact pinned binary during this Step's own implementation research
// before being encoded here as a permanent, checked-in regression test --
// these are not hopeful guesses about the binary's behavior. Each test
// below runs against a clean-config instance: startServer (helpers_test.go)
// spawns it with its own isolated, per-test XDG_CONFIG_HOME/XDG_DATA_HOME
// (a fresh t.TempDir(), not this machine's or CI runner's real one), so
// TestChatGPTOAuth_RealBinary_SetAuthFlipsConnected's own real credential
// write below can never leak into the shared real OpenCode auth store.

// TestChatGPTOAuth_RealBinary_ProviderAuthListsOAuthForOpenAI is §29.7
// case 1: GET /provider/auth still lists an oauth method for openai,
// label prefixed "ChatGPT" -- the live finding §29.1's own "no plugin,
// native since 1.17.15" conclusion rests on.
func TestChatGPTOAuth_RealBinary_ProviderAuthListsOAuthForOpenAI(t *testing.T) {
	baseURL := startServer(t)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/provider/auth", nil)
	if err != nil {
		t.Fatalf("build GET /provider/auth request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /provider/auth: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /provider/auth status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var methodsByProvider map[string][]struct {
		Type  string `json:"type"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&methodsByProvider); err != nil {
		t.Fatalf("decode GET /provider/auth response: %v", err)
	}

	openaiMethods, ok := methodsByProvider["openai"]
	if !ok {
		t.Fatalf("GET /provider/auth response has no \"openai\" entry at all -- full response keys: %v", mapKeys(methodsByProvider))
	}

	var foundOAuth bool
	for _, m := range openaiMethods {
		if m.Type == "oauth" {
			foundOAuth = true
			if len(m.Label) < 7 || m.Label[:7] != "ChatGPT" {
				t.Errorf(`openai oauth method label = %q, want it to start with "ChatGPT"`, m.Label)
			}
		}
	}
	if !foundOAuth {
		t.Errorf(`openai's own GET /provider/auth methods have no "oauth"-typed entry -- got: %+v`, openaiMethods)
	}
}

func mapKeys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestChatGPTOAuth_RealBinary_SetAuthFlipsConnected is §29.7 case 2: PUT
// /auth/openai with the oauth shape (including refresh: "") returns true
// and flips openai into GET /provider's own "connected" list -- driven
// through this package's OWN SetOAuthAuth method (auth.go), not a raw
// HTTP call, so this test doubles as a real-binary regression test for
// that method's own wire shape, not just the endpoint's existence.
func TestChatGPTOAuth_RealBinary_SetAuthFlipsConnected(t *testing.T) {
	baseURL := startServer(t)
	a := New(baseURL, testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	ctx := context.Background()
	if err := a.SetOAuthAuth(ctx, "openai", OAuthCredential{
		Access:    "test-access-token",
		Expires:   9999999999999,
		AccountID: "acct-test-contract",
	}); err != nil {
		t.Fatalf("SetOAuthAuth() error = %v, want nil", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/provider", nil)
	if err != nil {
		t.Fatalf("build GET /provider request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /provider: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /provider status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var parsed struct {
		Connected []string `json:"connected"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode GET /provider response: %v", err)
	}

	var found bool
	for _, p := range parsed.Connected {
		if p == "openai" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf(`GET /provider's own "connected" list = %v, want it to include "openai" after a successful SetOAuthAuth`, parsed.Connected)
	}
}

// TestChatGPTOAuth_RealBinary_DocStillCarriesOAuthShapes is §29.7 case 3:
// the /doc OpenAPI document still carries POST /provider/{providerID}/
// oauth/authorize and .../oauth/callback, the Auth union's own OAuth
// member requiring exactly {type, refresh, access, expires} (accountId/
// enterpriseUrl optional), and -- §29.8's own identical "endpoint-
// existence discipline §7.2 applies to /summarize" requirement --
// prompt_async's own request schema still declares a "variant" string
// property.
func TestChatGPTOAuth_RealBinary_DocStillCarriesOAuthShapes(t *testing.T) {
	baseURL := startServer(t)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/doc", nil)
	if err != nil {
		t.Fatalf("build GET /doc request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /doc: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /doc status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read GET /doc response: %v", err)
	}

	var doc struct {
		Paths      map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode GET /doc response: %v", err)
	}

	for _, path := range []string{
		"/provider/{providerID}/oauth/authorize",
		"/provider/{providerID}/oauth/callback",
		"/auth/{providerID}",
	} {
		if _, ok := doc.Paths[path]; !ok {
			t.Errorf("GET /doc no longer lists path %q", path)
		}
	}
	if put, ok := doc.Paths["/auth/{providerID}"]["put"]; !ok || put == nil {
		t.Error(`GET /doc's own "/auth/{providerID}" no longer has a "put" operation`)
	}

	oauthSchemaRaw, ok := doc.Components.Schemas["OAuth"]
	if !ok {
		t.Fatal(`GET /doc's own components.schemas has no "OAuth" entry`)
	}
	var oauthSchema struct {
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(oauthSchemaRaw, &oauthSchema); err != nil {
		t.Fatalf("decode components.schemas.OAuth: %v", err)
	}
	wantRequired := map[string]bool{"type": true, "refresh": true, "access": true, "expires": true}
	if len(oauthSchema.Required) != len(wantRequired) {
		t.Errorf("components.schemas.OAuth.required = %v, want exactly %v", oauthSchema.Required, mapKeys(wantRequired))
	}
	for _, field := range oauthSchema.Required {
		if !wantRequired[field] {
			t.Errorf("components.schemas.OAuth.required unexpectedly includes %q", field)
		}
	}
	if oauthSchema.AdditionalProperties {
		t.Error("components.schemas.OAuth.additionalProperties = true, want false")
	}

	// prompt_async's own request schema is inlined directly on the path
	// item (verified live during this Step's own research, session.go's
	// own promptAsyncRequest doc comment) -- decode just enough of it to
	// assert "variant" is still a declared string property.
	promptAsync, ok := doc.Paths["/session/{sessionID}/prompt_async"]
	if !ok {
		t.Fatal(`GET /doc no longer lists "/session/{sessionID}/prompt_async"`)
	}
	promptAsyncJSON, err := json.Marshal(promptAsync)
	if err != nil {
		t.Fatalf("re-marshal prompt_async path item: %v", err)
	}
	var promptAsyncOp struct {
		Post struct {
			RequestBody struct {
				Content struct {
					ApplicationJSON struct {
						Schema struct {
							Properties struct {
								Variant *struct {
									Type string `json:"type"`
								} `json:"variant"`
							} `json:"properties"`
						} `json:"schema"`
					} `json:"application/json"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"post"`
	}
	if err := json.Unmarshal(promptAsyncJSON, &promptAsyncOp); err != nil {
		t.Fatalf("decode prompt_async path item: %v", err)
	}
	variant := promptAsyncOp.Post.RequestBody.Content.ApplicationJSON.Schema.Properties.Variant
	if variant == nil {
		t.Fatal(`prompt_async's own request schema no longer declares a "variant" property (§29.8)`)
	}
	if variant.Type != "string" {
		t.Errorf(`prompt_async's own "variant" property type = %q, want "string"`, variant.Type)
	}
}
