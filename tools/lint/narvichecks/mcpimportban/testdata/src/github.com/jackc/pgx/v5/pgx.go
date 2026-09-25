// Package pgx stands for github.com/jackc/pgx/v5's own MODULE ROOT --
// round 3 review of PR #324, finding R8: only pgxpool's own import path
// had a fixture pinning it; pgx.Connect (this package) is a second,
// equally direct path to Postgres the prior fixture never exercised.
package pgx

type Conn struct{}
