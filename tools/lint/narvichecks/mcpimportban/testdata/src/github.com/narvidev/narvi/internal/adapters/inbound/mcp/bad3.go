// Package mcp (bad3.go) exercises round 4 review of PR #324, finding
// S9: bad.go's own imports are all either a clearly-unrelated package
// tree (postgres, sqlcgen, authz, internal/app/*) or a clearly-unrelated
// contracts/gen/go/* sibling (clientws) -- none of them is a NEAR MISS of
// something actually on the allow-list, so a widened allow-list or a
// looser matching rule could pass every existing fixture here while still
// being wrong. This file adds the two near misses S9 found missing:
// wshub, a REAL sibling of this package under the SAME
// internal/adapters/inbound tree (proving httpapi/auth are named
// INDIVIDUALLY, never as "the whole inbound tree"), and httpapix, a
// package whose import path merely shares httpapi's own characters as a
// STRING PREFIX (proving allowedPrefixes' own matching is a "prefix + /"
// boundary, never a bare strings.HasPrefix -- exactly the check
// mcpgateway already pins for isTargetPackage, applied here to the
// allowedPrefixes loop instead).
package mcp

import (
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapix" // want `importing "github.com/narvidev/narvi/internal/adapters/inbound/httpapix" is not on the allow-list for internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"     // want `importing "github.com/narvidev/narvi/internal/adapters/inbound/wshub" is not on the allow-list for internal/adapters/inbound/mcp`
)

// reachThroughNearMisses stands for the same mistake bad.go's own
// reachThroughDirectly names, via two paths that are near misses of the
// allow-list rather than obviously unrelated ones.
func reachThroughNearMisses() {
	_ = httpapix.Client{}
	_ = wshub.Hub{}
}
