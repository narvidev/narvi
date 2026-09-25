// Package mcpgateway stands for an UNRELATED sibling of
// internal/adapters/inbound/mcp that merely shares the same "mcp"
// characters as a PREFIX of its own import path (round 2 review of PR
// #324, finding N18): isTargetPackage's own doc comment calls its "+/"
// boundary check load-bearing, but until this fixture existed nothing in
// this analyzer's own testdata actually exercised a package shaped like
// this one to pin it -- a bare strings.HasPrefix(path, targetPackage)
// (dropping the "+/") would falsely ban this package's own import below.
package mcpgateway

import (
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

// UseStoreFreely stands for this package's own legitimate, UNRELATED use
// of postgres -- this import must NEVER be reported: mcpgateway is not
// internal/adapters/inbound/mcp, nor one of its subpackages, merely a
// same-directory sibling whose name happens to start the same way.
func UseStoreFreely() postgres.SessionStore {
	return postgres.SessionStore{}
}
