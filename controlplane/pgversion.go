package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/platform"
)

// serverVersionNumQuery reads the server's version as one integer, the form
// platform.MinPostgresServerVersionNum is written in.
const serverVersionNumQuery = `SELECT current_setting('server_version_num')::int`

// rowQuerier is the one method requireSupportedPostgres needs of a pool, so
// a test can answer it with any version.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// requireSupportedPostgres refuses a Postgres server older than
// platform.MinPostgresServerVersionNum, naming the server's version and the
// floor (technical plan §5.1). The subcommands that migrate, serve and seed,
// call it first on their pool: before anything else reads the server, and
// before any migration runs against it. routes does not: it lists the routes
// without needing a reachable database (runRoutesCommand).
//
// The read is bounded by timeout. A read that fails refuses too: a server
// that cannot answer one query at boot cannot be shown to be supported, and
// would fail the migrations that follow.
func requireSupportedPostgres(ctx context.Context, q rowQuerier, timeout time.Duration) error {
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var num int
	if err := q.QueryRow(qctx, serverVersionNumQuery).Scan(&num); err != nil {
		return fmt.Errorf("read the Postgres server's version: %w", err)
	}
	if num < platform.MinPostgresServerVersionNum {
		return fmt.Errorf("refusing to start: the Postgres server runs version %s, and this control plane needs %s or later -- upgrade the server, then restart",
			postgresVersion(num), postgresVersion(platform.MinPostgresServerVersionNum))
	}
	return nil
}

// postgresVersion renders a server_version_num the way Postgres names its
// releases: major.minor from 10 on (150013 is 15.13), major.minor.patch
// before it (90624 is 9.6.24).
func postgresVersion(num int) string {
	if num >= 100000 {
		return fmt.Sprintf("%d.%d", num/10000, num%10000)
	}
	return fmt.Sprintf("%d.%d.%d", num/10000, num/100%100, num%100)
}
