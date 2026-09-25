// Package pgxpool stands in for github.com/jackc/pgx/v5/pgxpool -- just
// enough for this analyzer's own testdata to reference the banned import
// PATH; the analyzer bans the path itself, never a particular symbol
// inside it, so this stand-in need not resemble the real package beyond
// existing under the same import path.
package pgxpool

// Pool stands for *pgxpool.Pool, the real package's own connection pool
// handle.
type Pool struct{}
