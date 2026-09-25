// Package contracts stands for the real contracts package (contracts.FS,
// contracts.Version) -- ALLOWED inside internal/adapters/inbound/mcp by
// EXACT match (never as a prefix -- contracts/contractstest and the
// OTHER contracts/gen/go/* siblings are not).
package contracts

const Version = "test"
