// This file (capability.go) defines the closed capability vocabulary --
// three values, finalised in snake_case -- see doc.go for the naming
// history (the first two shipped as kebab-case placeholders; this file
// is where they became a published wire enum).

package license

// Capability is the closed vocabulary of capabilities a composed module
// may implement and a licence grant may entitle. Closed deliberately:
// adding a value is a reviewable PR to this file, never a string an
// operator or a private module can invent on the fly (a grant naming
// anything outside [All] fails [Parse] with [ErrUnknownCapability], and a
// module declaring anything outside [All] fails controlplane.Build the
// same way) -- see docs/design/boundaries-design.md, section 1.2. Whether
// or how any of these is ever priced is undecided, and nothing in this
// package (or its callers) depends on an answer -- technical plan §34's
// own "how either is distributed is undecided; nothing here depends on
// that answer".
type Capability string

const (
	// CapabilityOrganizationGovernance is organization-scale review
	// governance -- one of the two things technical plan §34.1 names a
	// composed module may add ("organization scale and compliance, never
	// base safety"). See doc.go for this constant's own naming history.
	CapabilityOrganizationGovernance Capability = "organization_governance"

	// CapabilityCompliance is Narvi Gatekeeper's compliance surface --
	// the other of the two things technical plan §34.1 names a composed
	// module may add.
	CapabilityCompliance Capability = "compliance"

	// CapabilityKnowledgeRetrieval is the private, cross-repository
	// knowledge-retrieval ranking mode (docs/design/boundaries-design.md,
	// section 2, "mode B"). See doc.go for this constant's own naming
	// history.
	CapabilityKnowledgeRetrieval Capability = "knowledge_retrieval"
)

// All enumerates every Capability this build defines, in the fixed order
// every consumer that must iterate the full vocabulary (boot-time
// logging, GET /api/capabilities) uses -- see [Parse], which rejects any
// grant naming a capability not in this list.
var All = []Capability{CapabilityOrganizationGovernance, CapabilityCompliance, CapabilityKnowledgeRetrieval}

// Product is the audience every licence key must carry (the wire
// format's own "product" claim, design note section 1.5) -- a key
// minted for a different Narvi-family product fails [Parse] with
// [ErrWrongProduct] rather than silently entitling capabilities it was
// never meant to.
const Product = "narvi-gatekeeper"
