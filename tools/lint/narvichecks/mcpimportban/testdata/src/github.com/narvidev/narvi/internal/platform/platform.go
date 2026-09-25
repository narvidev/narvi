// Package platform stands for the real platform package -- ALLOWED
// inside internal/adapters/inbound/mcp (platform.UserFromContext/Logger).
package platform

import "context"

type AuthenticatedUser struct {
	ID string
}

func UserFromContext(ctx context.Context) (AuthenticatedUser, bool) {
	return AuthenticatedUser{}, false
}
