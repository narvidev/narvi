// Package actorauthz stands for one real internal/app/* application
// service -- this analyzer's own fixture for the bannedPrefix rule
// (every internal/app/* package, not an enumerated list of today's
// members).
package actorauthz

func Resolve() bool { return true }
