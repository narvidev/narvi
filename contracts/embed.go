// Package contracts embeds every versioned JSON Schema under this directory
// into the compiled binary (mirrors migrations/embed.go, PR-04) — so the
// eventual single-binary self-host story (§12.1) and this package's own
// round-trip contract tests (contracts/contractstest) never need to read
// schema files off disk by path.
package contracts

import (
	"embed"
	"strings"
)

// FS embeds every *.schema.json file under sandbox-ws/, client-ws/,
// session-config/, and rest/ (all versioned v1/ subdirectories today, §6).
// It deliberately does NOT embed gen/ (generated code, not schema source)
// or the npm project under this directory.
//
//go:embed sandbox-ws/v1/*.schema.json client-ws/v1/*.schema.json session-config/v1/*.schema.json rest/v1/*.schema.json
var FS embed.FS

// versionRaw embeds contracts/VERSION verbatim (including its trailing
// newline); Version below is the trimmed form every consumer actually
// wants. Embedding the file itself -- rather than hand-copying the string
// into a Go const -- is what makes it structurally impossible for the
// compiled binary's own idea of "which contracts version is this" to
// drift from the file tools/contractscompat's CI job actually checks
// (§6.3: "contracts.Version embedded and exposed as optional
// CapabilitiesResponse.contractsVersion").
//
//go:embed VERSION
var versionRaw string

// Version is contracts/VERSION's own contents, trimmed -- e.g. "1.0.0".
// GetCapabilities threads this into CapabilitiesResponse.contractsVersion
// (controlplane.buildCapabilitiesResponse) so a caller of GET
// /api/capabilities can see which contracts version the running control
// plane was built against.
var Version = strings.TrimSpace(versionRaw)
