// Package mcpclient stands for a real sibling domain package of mcpscope
// -- allowing mcpscope must not allow the internal/domain tree.
package mcpclient

func MatchRedirectURI([]string, string) bool { return false }
