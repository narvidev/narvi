// Package stdlib stands for github.com/jackc/pgx/v5/stdlib -- a
// database/sql driver registration package (round 3 review of PR #324,
// finding R8): a blank import alone registers a "pgx" database/sql
// driver, reachable through the standard library's own generic
// interface.
package stdlib
