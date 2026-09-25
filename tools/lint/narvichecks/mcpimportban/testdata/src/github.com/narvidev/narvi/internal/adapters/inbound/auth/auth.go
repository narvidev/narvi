// Package auth stands for the real cookie-auth middleware package --
// ALLOWED inside internal/adapters/inbound/mcp (this surface sits behind
// the SAME auth.Middleware every other route group uses).
package auth

import "net/http"

func Middleware(next http.Handler) http.Handler { return next }
