// Package somestore stands for ANY internal/adapters/outbound/* package,
// not merely postgres by name -- round 3 review of PR #324: an
// allow-list bans the whole internal/adapters/outbound tree
// structurally, by simply never naming it, with no need to enumerate
// each store adapter one at a time the way the prior deny-list did.
package somestore

type Store struct{}
