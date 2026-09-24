// Package authz stands for the real authz domain -- rendering a verdict
// directly, bypassing the bridge, is exactly what this analyzer exists
// to make impossible to compile.
package authz

type Action string

type Actor struct {
	UserID string
	Role   Role
}

type Role string

type Resource struct{}

func Authorize(Actor, Action, Resource) error { return nil }
