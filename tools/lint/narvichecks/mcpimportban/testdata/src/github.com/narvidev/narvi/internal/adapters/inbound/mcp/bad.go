// Package mcp stands for the real internal/adapters/inbound/mcp package.
// This file (bad.go) is the mistake this analyzer exists to catch: a
// tool reaching a store, sqlcgen, the authz domain, an internal/app/*
// service, or Postgres directly (either via pgx/pgxpool or via Go's own
// generic database/sql), bypassing the bridge entirely.
package mcp

import (
	"database/sql" // want `importing "database/sql" is banned inside internal/adapters/inbound/mcp`

	"github.com/jackc/pgx/v5/pgxpool" // want `importing "github.com/jackc/pgx/v5/pgxpool" is banned inside internal/adapters/inbound/mcp`

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"         // want `importing "github.com/narvidev/narvi/internal/adapters/outbound/postgres" is banned inside internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen" // want `importing "github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen" is banned inside internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/app/actorauthz"                     // want `importing "github.com/narvidev/narvi/internal/app/actorauthz" is banned inside internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/app/modelcatalog"                   // want `importing "github.com/narvidev/narvi/internal/app/modelcatalog" is banned inside internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/domain/authz"                       // want `importing "github.com/narvidev/narvi/internal/domain/authz" is banned inside internal/adapters/inbound/mcp`
)

// reachThroughDirectly stands for the mistake: reaching the store and
// rendering an authz verdict directly, instead of invoking an existing
// httpapi handler through the bridge (bridge.go's own doc comment).
// modelcatalog is a SECOND internal/app/* member alongside actorauthz --
// deliberately, so a mutant that narrows bannedPrefix's own prefix check
// to an exact match against "actorauthz" alone (round 2 review of PR
// #324, finding N18) still fails this test: it would report actorauthz
// but miss this second import entirely. pgxpool and database/sql stand
// for the raw-SQL dodge finding N9 found: platform.Load()'s own
// (allowed) Config.DatabaseURL is enough to reach either one directly.
func reachThroughDirectly() {
	_ = postgres.SessionStore{}
	_ = sqlcgen.ListSessionsParams{}
	_ = actorauthz.Resolve()
	_ = modelcatalog.Catalog()
	_ = authz.Authorize(authz.Actor{}, "", authz.Resource{})
	_ = pgxpool.Pool{}
	var _ *sql.DB
}
