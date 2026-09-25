// Package mcpscope stands for the real scope vocabulary (technical plan
// §43.17) -- the one internal/domain package the MCP adapter may import.
package mcpscope

type Scope string

func Satisfies([]Scope, Scope) bool { return false }
