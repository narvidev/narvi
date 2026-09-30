package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
)

// githubSign mirrors GitHub's own "X-Hub-Signature-256: sha256=<hex>"
// scheme -- HMAC-SHA256 over the raw body, no timestamp -- the SAME
// helper internal/platform/webhooksig_test.go's own githubSign
// implements, duplicated here (this package can't import an internal
// _test.go helper from another package) rather than shared.
func githubSign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// newTestHandler builds a handler with a real (but never-actually-used-by-
// these-tests) WebhookDeliveryStore/SessionCoalescer -- every test in this
// file exercises a rejection path (signature/body/header validation) that
// returns BEFORE the handler ever touches either, so a nil-backed
// pool is safe here: these fields are only dereferenced on a path these
// tests never reach.
func newTestHandler(secret, botHandle string) http.HandlerFunc {
	deliveries := postgres.NewWebhookDeliveryStore(nil)
	coalescer := &SessionCoalescer{}
	handler, err := NewHandler(coalescer, deliveries, Config{WebhookSecret: secret, BotHandle: botHandle, Outbound: platform.MustNewGitHubOutboundConfig("test-bot-token")})
	if err != nil {
		panic(err) // unreachable: Outbound is set
	}
	return handler
}

// TestNewHandler_RefusesNilOutbound proves GitHub ingress cannot be built
// without GitHub outbound (§12.5): the handler resolves pull requests,
// fetches review context and replies as the bot, so a nil
// Config.Outbound is a construction error wrapping
// platform.ErrGitHubOutboundRequired -- never a handler that would call
// GitHub with an empty token.
func TestNewHandler_RefusesNilOutbound(t *testing.T) {
	handler, err := NewHandler(&SessionCoalescer{}, postgres.NewWebhookDeliveryStore(nil), Config{WebhookSecret: "s", BotHandle: "narvi-bot"})
	if !errors.Is(err, platform.ErrGitHubOutboundRequired) {
		t.Errorf("NewHandler(nil Outbound) error = %v, want one wrapping platform.ErrGitHubOutboundRequired", err)
	}
	if handler != nil {
		t.Error("NewHandler(nil Outbound) returned a handler, want nil")
	}
}

// TestHandler_SignatureVerification_Rejects is table-driven over
// GitHub-shaped signature REJECTION at the full handler level -- mirrors
// internal/platform/webhooksig_test.go's own GitHub-shaped rejection
// cases (tampered body, malformed/missing signature), exercised through
// the real HTTP handler rather than calling
// platform.VerifyWebhookSignature directly. Every case here is rejected
// BEFORE the handler ever reaches WebhookDeliveryStore.Claim, so a
// nil-backed store (newTestHandler) is safe -- the ACCEPTED-signature
// path (which DOES reach Claim, needing a real Postgres pool) is covered
// by handler_integration_test.go instead.
func TestHandler_SignatureVerification_Rejects(t *testing.T) {
	const secret = "test-github-webhook-secret"
	body := []byte(`{"action":"created","issue":{"number":1},"comment":{"body":"no mention here"},"repository":{"full_name":"acme/widgets","name":"widgets","clone_url":"https://github.com/acme/widgets.git"}}`)
	validSig := githubSign([]byte(secret), body)
	tamperedBody := append([]byte(nil), append(body, '!')...)

	tests := []struct {
		name      string
		body      []byte
		signature string
	}{
		{name: "tampered body (signature no longer matches)", body: tamperedBody, signature: "sha256=" + validSig},
		{name: "wrong secret", body: body, signature: "sha256=" + githubSign([]byte("a-completely-different-secret"), body)},
		{name: "malformed signature (non-hex)", body: body, signature: "sha256=not-hex-zzzz"},
		{name: "empty signature header", body: body, signature: ""},
		{name: "whitespace-only signature header", body: body, signature: "   "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := newTestHandler(secret, "narvi-bot")

			req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(string(tc.body)))
			req.Header.Set("X-Hub-Signature-256", tc.signature)
			req.Header.Set("X-GitHub-Event", "some_unrecognized_event")
			req.Header.Set("X-GitHub-Delivery", "11111111-1111-1111-1111-111111111111")

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

// TestHandler_MissingDeliveryHeader proves a validly-signed request with
// no X-GitHub-Delivery header is rejected 400 BEFORE ever attempting a
// dedupe claim (this codepath runs with a nil-backed WebhookDeliveryStore
// specifically to prove Claim is never reached).
func TestHandler_MissingDeliveryHeader(t *testing.T) {
	const secret = "test-github-webhook-secret"
	body := []byte(`{}`)
	sig := githubSign([]byte(secret), body)

	handler := newTestHandler(secret, "narvi-bot")

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", "sha256="+sig)
	req.Header.Set("X-GitHub-Event", "issue_comment")
	// X-GitHub-Delivery deliberately omitted.

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestHandler_OversizedBody proves a body larger than maxWebhookBodyBytes
// is rejected 413 before signature verification even runs.
func TestHandler_OversizedBody(t *testing.T) {
	handler := newTestHandler("test-github-webhook-secret", "narvi-bot")

	oversized := strings.Repeat("a", maxWebhookBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(oversized))
	req.Header.Set("X-Hub-Signature-256", "sha256=irrelevant")
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-GitHub-Delivery", "22222222-2222-2222-2222-222222222222")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}
