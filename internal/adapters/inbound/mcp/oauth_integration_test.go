//go:build integration

// This file holds the bearer-gate tests that need only a token minted
// straight through the stores (technical plan §43.16). The authorization
// flow itself -- the official SDK client discovering, consenting,
// exchanging and calling, revocation on the next call, the scope-less
// tool list -- is proven against controlplane.Build's own router, the
// production wiring, in controlplane's TestOAuth_ProductionRouter
// (technical plan §43.19).
package mcp_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// revokedCallFails asserts token now answers 401 with the exact generic
// body and error="invalid_token", on a raw call.
func revokedCallFails(t *testing.T, rig *mcpTestRig, token string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"narvi_list_models","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	status, header, raw := rig.postMCP(t, "tools/call", "narvi_list_models", body, mcpCredential{bearer: token})
	if status != http.StatusUnauthorized || string(raw) != `{"error":"unauthorized"}` {
		t.Fatalf("call with the refused token: status %d body %s, want 401 {\"error\":\"unauthorized\"}", status, raw)
	}
	challenges, err := oauthex.ParseWWWAuthenticate(header.Values("WWW-Authenticate"))
	if err != nil || len(challenges) != 1 || challenges[0].Params["error"] != "invalid_token" {
		t.Fatalf("challenge = %q (err %v), want error=\"invalid_token\"", header.Values("WWW-Authenticate"), err)
	}
}

// TestBearer_GrantResourceMismatchIs401: a grant stored for another
// resource (another deployment's /mcp) never authenticates here, however
// valid its token otherwise is.
func TestBearer_GrantResourceMismatchIs401(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rig := newMCPTestRig(t)
	user, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	token := mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read"})
	if status, _ := rig.callTool(t, "narvi_list_models", "{}", token); status != http.StatusOK {
		t.Fatalf("before: status %d, want 200", status)
	}
	if _, err := rig.pool.Exec(ctx, `UPDATE mcp_oauth_grants SET resource = 'https://other.example/mcp' WHERE user_id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	revokedCallFails(t, rig, token)
}
