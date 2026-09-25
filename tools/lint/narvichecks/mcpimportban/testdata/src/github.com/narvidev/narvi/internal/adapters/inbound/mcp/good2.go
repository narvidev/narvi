// Package mcp (good2.go) exercises the REST of the allow-list round 3
// review of PR #324 (findings R7/R8) introduced: the official MCP SDK,
// the two other third-party libraries this package's own tools/schemas
// genuinely depend on, this repo's own auth/contracts packages (contracts
// and contracts/gen/go/restdtos allowed by EXACT match -- see bad.go's
// own clientws case for the sibling that is NOT), and a plain
// standard-library import that is not one of the four explicit
// exceptions. None of these may ever be reported.
package mcp

import (
	"context"

	"github.com/go-chi/chi/v5"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/narvidev/narvi/contracts"
	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
)

// useEveryAllowedThirdPartyAndRepoImport stands for the real shape:
// every one of these imports is ALLOWED, and must never be reported.
func useEveryAllowedThirdPartyAndRepoImport(ctx context.Context) {
	_ = sdkmcp.Server{}
	_ = jsonschema.Schema{}
	var _ chi.Router
	_ = contracts.Version
	_ = restdtos.ListSessionsToolRequest{}
	_ = auth.Middleware
	_ = ctx
}
