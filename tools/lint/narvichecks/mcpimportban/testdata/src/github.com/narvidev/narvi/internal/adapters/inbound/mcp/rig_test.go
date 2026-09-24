package mcp

import (
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

// TestRigConstructsARealStore stands for a real integration-rig test
// (httpapi's own *_integration_test.go convention) constructing a real
// Postgres store directly -- a test file's own import is not a
// production decision point (this analyzer's own doc comment,
// "_test.go files are exempt"), so this must NOT be reported even though
// bad.go's identical import one file over IS.
func TestRigConstructsARealStore(t *testing.T) {
	_ = postgres.SessionStore{}
}
