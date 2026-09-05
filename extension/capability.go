// This file (capability.go) re-exports internal/domain/license's own
// closed capability vocabulary, and defines Capabilities -- the narrow
// interface a module consults to ask "is this capability usable right
// now", satisfied by *internal/app/capability.Registry without either
// package importing the other's own concrete type.

package extension

import "github.com/narvidev/narvi/internal/domain/license"

// Capability is license.Capability, re-exported as a type alias so a
// value of this type IS a license.Capability (not merely convertible to
// one) on both sides of the module boundary. See internal/domain/license's
// own doc comment for the vocabulary's naming history -- all three
// constants below are now a published wire enum (GET /api/capabilities'
// own CapabilityStatus.name), not placeholders.
type Capability = license.Capability

// CapabilityOrganizationGovernance, CapabilityCompliance and
// CapabilityKnowledgeRetrieval mirror license.
// CapabilityOrganizationGovernance/CapabilityCompliance/
// CapabilityKnowledgeRetrieval exactly -- see this file's own doc comment.
const (
	CapabilityOrganizationGovernance = license.CapabilityOrganizationGovernance
	CapabilityCompliance             = license.CapabilityCompliance
	CapabilityKnowledgeRetrieval     = license.CapabilityKnowledgeRetrieval
)

// Capabilities is what a module receives (Runtime.Capabilities) to ask
// whether one of its own capabilities is currently usable. Satisfied by
// *internal/app/capability.Registry -- deliberately an interface rather
// than that concrete type, so this package never needs to re-export
// internal/app/capability itself, and so a module (or a test standing in
// for one) can substitute its own fake without needing a real licence
// key.
//
// This is the SAME conjunction internal/app/capability.Registry.Enabled
// itself implements (installed AND licensed AND valid-now) -- Capabilities
// adds nothing and can express nothing else: there is no method here
// that could turn a capability off from the module's own side.
type Capabilities interface {
	Enabled(Capability) bool
}
