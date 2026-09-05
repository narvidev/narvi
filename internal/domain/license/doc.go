// Package license is Narvi's pure, offline licence-grant domain: decoding
// and verifying a signed capability grant against an embedded Ed25519
// keyset. Stdlib only -- no I/O, no time.Now() (every temporal check
// takes `now` as an explicit parameter, per this repository's own
// /internal/domain convention, technical plan §11), no randomness. See
// technical plan §34.5 and docs/design/boundaries-design.md, section 1, for the
// full design this package implements: a capability is enabled only when
// it is installed (a composed module supplies it), licensed (a verified
// grant names it) and valid now (the grant's own [Grant.ValidAt] window
// contains the current instant) -- that conjunction lives one layer up,
// in internal/app/capability, which is this package's only production
// caller besides the composition root (controlplane) and the extension
// façade.
//
// # The v1 capability vocabulary is fixed
//
// [CapabilityOrganizationGovernance], [CapabilityCompliance] and
// [CapabilityKnowledgeRetrieval] are the three capabilities this build
// defines, snake_case throughout to match this repository's own REST
// enum convention (e.g. not_a_pr, public_api, needs_human, data_layer).
// The first two originally shipped as PLACEHOLDERS pending a product
// decision -- CapabilityGovernance ("governance") and
// CapabilityKnowledgeRetrieval ("knowledge-retrieval"), kebab-case -- see
// docs/design/boundaries-design.md, section 7, for that decision's own
// history. It has now been made: GET /api/capabilities
// (contracts/rest/v1/dtos.schema.json's own CapabilityStatus.name enum)
// is the published wire contract the three constants above now feed, so
// renaming any of them from here on is the breaking contract change
// technical plan §34.4 warns a published enum value becomes -- treat all
// three names as durable, not provisional.
//
// # Key custody
//
// The signing seed that produces a valid licence key never exists in
// this repository -- see [IssuerKeys]'s own doc comment. This package can
// verify a signature; it cannot produce one, by design (see the design
// note's own "the signing tool and its key custody are the private
// repo's").
package license
