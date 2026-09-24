// Package d stands for an ORDINARY, unrelated production package --
// httpapi itself, or any other real caller of postgres. This analyzer's
// scope is exactly ONE package (internal/adapters/inbound/mcp); this
// package's own identical import must NEVER be reported, proving the ban
// does not leak beyond that one package.
package d

import "github.com/narvidev/narvi/internal/adapters/outbound/postgres"

func UseStoreFreely() postgres.SessionStore {
	return postgres.SessionStore{}
}
