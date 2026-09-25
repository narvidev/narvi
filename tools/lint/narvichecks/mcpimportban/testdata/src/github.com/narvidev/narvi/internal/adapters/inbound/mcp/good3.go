// Package mcp (good3.go) exercises the allow-list's one internal/domain
// entry: internal/domain/mcpscope, by EXACT match (technical plan §43.17).
// It must never be reported.
package mcp

import "github.com/narvidev/narvi/internal/domain/mcpscope"

// filterByScope stands for the real shape: tool visibility decided by the
// scope rule, never by an authz verdict.
func filterByScope(granted []mcpscope.Scope) bool {
	return mcpscope.Satisfies(granted, "mcp:read")
}
