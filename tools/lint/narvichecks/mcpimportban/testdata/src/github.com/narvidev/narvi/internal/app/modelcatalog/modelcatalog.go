// Package modelcatalog stands for a SECOND real internal/app/*
// application service, alongside actorauthz.go's own fixture (round 2
// review of PR #324, finding N18): the bannedPrefix rule bans the whole
// internal/app/* CLASS, not an enumerated list of today's members, and a
// testdata tree with only ONE member could not tell that apart from a
// narrower analyzer that only recognizes actorauthz by exact name.
package modelcatalog

func Catalog() bool { return true }
