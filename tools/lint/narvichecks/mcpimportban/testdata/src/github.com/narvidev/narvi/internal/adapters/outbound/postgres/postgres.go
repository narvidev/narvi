// Package postgres stands for the real Postgres adapter -- this
// analyzer's own fixture for "a store" the mcp package must never import
// directly.
package postgres

type SessionStore struct{}
