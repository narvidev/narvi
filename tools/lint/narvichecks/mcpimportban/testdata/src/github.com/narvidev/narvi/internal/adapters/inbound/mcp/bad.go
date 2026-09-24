// Package mcp stands for the real internal/adapters/inbound/mcp package.
// This file (bad.go) is the mistake this analyzer exists to catch: a
// tool reaching a store, sqlcgen, the authz domain, and an internal/app/*
// service directly, bypassing the bridge entirely.
package mcp

import (
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"         // want `importing "github.com/narvidev/narvi/internal/adapters/outbound/postgres" is banned inside internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen" // want `importing "github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen" is banned inside internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/app/actorauthz"                     // want `importing "github.com/narvidev/narvi/internal/app/actorauthz" is banned inside internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/domain/authz"                       // want `importing "github.com/narvidev/narvi/internal/domain/authz" is banned inside internal/adapters/inbound/mcp`
)

// reachThroughDirectly stands for the mistake: reaching the store and
// rendering an authz verdict directly, instead of invoking an existing
// httpapi handler through the bridge (bridge.go's own doc comment).
func reachThroughDirectly() {
	_ = postgres.SessionStore{}
	_ = sqlcgen.ListSessionsParams{}
	_ = actorauthz.Resolve()
	_ = authz.Authorize(authz.Actor{}, "", authz.Resource{})
}
