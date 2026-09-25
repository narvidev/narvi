// Package mcp (bad4.go) pins that internal/domain/mcpscope is allowed by
// EXACT match only: a subpackage of it and a sibling domain package are
// both still refused, the same way bad.go already refuses authz.
package mcp

import (
	"github.com/narvidev/narvi/internal/domain/mcpclient"    // want `importing "github.com/narvidev/narvi/internal/domain/mcpclient" is not on the allow-list for internal/adapters/inbound/mcp`
	"github.com/narvidev/narvi/internal/domain/mcpscope/sub" // want `importing "github.com/narvidev/narvi/internal/domain/mcpscope/sub" is not on the allow-list for internal/adapters/inbound/mcp`
)

func reachThroughDomainNearMisses() {
	_ = mcpclient.MatchRedirectURI
	_ = sub.Anything{}
}
