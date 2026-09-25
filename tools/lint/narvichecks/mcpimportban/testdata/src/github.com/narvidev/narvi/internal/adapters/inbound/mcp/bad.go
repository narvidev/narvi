// Package mcp stands for the real internal/adapters/inbound/mcp package.
// This file (bad.go) is the mistake this analyzer exists to catch: a
// tool reaching a store, sqlcgen, the authz domain, an internal/app/*
// service, or Postgres directly -- by any of the several different
// import paths that lead there (pgx's own module root, its stdlib
// driver-registration subpackage, or pgxpool; Go's own generic
// database/sql; or golang-migrate's own database/postgres driver, round
// 3 review of PR #324, finding R7) -- bypassing the bridge entirely.
// None of these paths is named individually by this analyzer's own
// allow-list; each is banned simply by NOT appearing in it (round 3
// review, findings R7/R8: an allow-list needs no dodge to be discovered
// ahead of time the way the prior deny-list did).
package mcp

import (
	"database/sql"        // want `importing "database/sql" is not on the allow-list for internal/adapters/inbound/mcp`
	"database/sql/driver" // want `importing "database/sql/driver" is not on the allow-list for internal/adapters/inbound/mcp`
	"net/rpc"             // want `importing "net/rpc" is not on the allow-list for internal/adapters/inbound/mcp`
	"os/exec"             // want `importing "os/exec" is not on the allow-list for internal/adapters/inbound/mcp`

	"github.com/golang-migrate/migrate/v4/database/postgres" // want `importing "github.com/golang-migrate/migrate/v4/database/postgres" is not on the allow-list for internal/adapters/inbound/mcp`
	"github.com/jackc/pgx/v5"                                 // want `importing "github.com/jackc/pgx/v5" is not on the allow-list for internal/adapters/inbound/mcp`
	"github.com/jackc/pgx/v5/pgxpool"                         // want `importing "github.com/jackc/pgx/v5/pgxpool" is not on the allow-list for internal/adapters/inbound/mcp`
	_ "github.com/jackc/pgx/v5/stdlib"                        // want `importing "github.com/jackc/pgx/v5/stdlib" is not on the allow-list for internal/adapters/inbound/mcp`

	narvipostgres "github.com/narvidev/narvi/internal/adapters/outbound/postgres" // want `importing "github.com/narvidev/narvi/internal/adapters/outbound/postgres" is not on the allow-list for internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"    // want `importing "github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen" is not on the allow-list for internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/adapters/outbound/somestore"           // want `importing "github.com/narvidev/narvi/internal/adapters/outbound/somestore" is not on the allow-list for internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/app/actorauthz"                        // want `importing "github.com/narvidev/narvi/internal/app/actorauthz" is not on the allow-list for internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/app/modelcatalog"                      // want `importing "github.com/narvidev/narvi/internal/app/modelcatalog" is not on the allow-list for internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/domain/authz"                         // want `importing "github.com/narvidev/narvi/internal/domain/authz" is not on the allow-list for internal/adapters/inbound/mcp`

	"github.com/narvidev/narvi/contracts/gen/go/clientws" // want `importing "github.com/narvidev/narvi/contracts/gen/go/clientws" is not on the allow-list for internal/adapters/inbound/mcp`
)

// reachThroughDirectly stands for the mistake: reaching the store and
// rendering an authz verdict directly, instead of invoking an existing
// httpapi handler through the bridge (bridge.go's own doc comment).
// modelcatalog is a SECOND internal/app/* member alongside actorauthz --
// deliberately, so a mutant that narrows the internal/app/ prefix check
// to an exact match against "actorauthz" alone (round 2 review of PR
// #324, finding N18) still fails this test: it would report actorauthz
// but miss this second import entirely. pgx (module root), pgx/stdlib,
// pgxpool, database/sql, and golang-migrate's own postgres driver all
// stand for the same raw-SQL dodge (findings N9, R7, R8):
// platform.Load()'s own (allowed) Config.DatabaseURL is enough to reach
// any one of them directly. somestore stands for ANY
// internal/adapters/outbound/* package, proving the ban is not
// postgres-specific. clientws stands for an unrelated
// contracts/gen/go/* sibling of the one contracts subpackage this
// analyzer's allow-list actually names (restdtos), proving that entry is
// an EXACT match, never a prefix.
func reachThroughDirectly() {
	_ = narvipostgres.SessionStore{}
	_ = sqlcgen.ListSessionsParams{}
	_ = somestore.Store{}
	_ = actorauthz.Resolve()
	_ = modelcatalog.Catalog()
	_ = authz.Authorize(authz.Actor{}, "", authz.Resource{})
	_ = pgx.Conn{}
	_ = pgxpool.Pool{}
	var _ *sql.DB
	var _ driver.Driver
	migratePG := &postgres.Postgres{}
	_, _ = migratePG.Open("postgres://")
	_ = clientws.Envelope{}
	var _ *rpc.Client
	_, _ = exec.LookPath("sh")
}
