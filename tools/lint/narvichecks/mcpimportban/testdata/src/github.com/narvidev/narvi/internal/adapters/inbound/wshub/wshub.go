// Package wshub stands for a REAL sibling of internal/adapters/inbound/mcp
// under the SAME internal/adapters/inbound tree -- round 4 review of PR
// #324, finding S9: this analyzer's own allowedPrefixes names httpapi and
// auth individually, never the whole internal/adapters/inbound tree, but
// nothing in this package's own testdata previously imported an
// unrelated inbound sibling to prove that distinction is load-bearing. A
// mutant that widens those two entries to a single
// "internal/adapters/inbound" prefix (matching every sibling under it,
// this one included) must still be caught: this package holds no
// authorization logic of its own, so importing it from mcp would be
// exactly as unreviewed a dodge as importing postgres directly.
package wshub

type Hub struct{}
