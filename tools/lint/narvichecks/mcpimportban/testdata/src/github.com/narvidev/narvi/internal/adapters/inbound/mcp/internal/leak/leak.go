// Package leak stands for a SUBPACKAGE of internal/adapters/inbound/mcp
// -- the dodge finding M4 (adversarial review of PR #324) found: a prior
// version of this analyzer compared pass.Pkg.Path() for EXACT equality
// against internal/adapters/inbound/mcp only, so a subpackage one level
// down (like this one) could import postgres directly and go entirely
// unreported, even though package mcp itself was banned from the same
// import one directory up (bad.go). This file must be reported exactly
// like bad.go's own identical import.
package leak

import (
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres" // want `importing "github.com/narvidev/narvi/internal/adapters/outbound/postgres" is banned inside internal/adapters/inbound/mcp`
)

// Store stands for the dodge itself: a subpackage handing back a real
// store value that package mcp (vialeak.go, one directory up) can then
// call methods on directly -- without package mcp ever importing
// postgres itself, and so without ever tripping the ban AT the point
// where the store value is actually used.
func Store() postgres.SessionStore {
	return postgres.SessionStore{}
}
