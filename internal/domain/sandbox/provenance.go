package sandbox

import (
	"regexp"
	"strconv"
	"strings"
)

// A restore brings back the sandbox-agent binary and the agent runtime the
// snapshot holds, whatever the control plane would boot today (technical
// plan §35.5b): a fresh start date does not make a snapshot's contents
// current. So a restore is decided on what the snapshot holds, as the
// agent that minted it reported it, in three states that are never
// collapsed -- compatible restores, incompatible is refused, and unknown
// is its own state, restored but never counted as compatible.

// The restore floors (technical plan §35.5b). Raising one is the one-line
// "refuse snapshots older than X"; a floor only ever rises, which is why a
// refused snapshot is cleared rather than kept for later.
const (
	// MinRestorableAgentProtocol is the oldest sandbox-agent protocol, the
	// contracts VERSION an agent was compiled with, whose snapshots a
	// restore accepts. 1.25.0 is the first protocol whose agents report it
	// at all, so the floor refuses nothing yet.
	MinRestorableAgentProtocol = "1.25.0"
	// MinRestorableRuntimeVersion is the oldest agent runtime version whose
	// snapshots a restore accepts; empty for no floor, as now.
	MinRestorableRuntimeVersion = ""
	// RestoreWhenProvenanceUnknown says what a restore of a snapshot whose
	// provenance is unknown does: restore. Every snapshot minted before the
	// agents that report it is unknown, and refusing them all would cost
	// every such session its context at once. Whether to refuse them is
	// decided once a fresh lineage carries a recap (technical plan §35.5),
	// from how many are restored (sandbox_snapshot_restore_total) and how
	// old they are (sandboxes.snapshot_minted_at).
	RestoreWhenProvenanceUnknown = true
)

// SnapshotProvenance is what a sandbox row records of the agent that
// minted its snapshot. Recorded is false when the row records nothing for
// this snapshot -- it predates the record, or the previous control-plane
// binary recorded the snapshot; AgentProtocol and RuntimeVersion are then
// empty. A recorded snapshot whose agent reported nothing has Recorded
// true and both empty.
type SnapshotProvenance struct {
	Recorded       bool
	AgentProtocol  string
	RuntimeVersion string
}

// RestoreFloor is what a snapshot's provenance is measured against: the
// floors above, and the control plane's own protocol, whose MAJOR a
// restorable agent must share.
type RestoreFloor struct {
	AgentProtocol        string
	RuntimeVersion       string
	ControlPlaneProtocol string
}

// ProvenanceState is a restore's state.
type ProvenanceState string

const (
	// ProvenanceCompatible means the snapshot's agent speaks a protocol of the
	// control plane's MAJOR, at or above the floor, and its runtime is at
	// or above the runtime floor, when one is set.
	ProvenanceCompatible ProvenanceState = "compatible"
	// ProvenanceIncompatible means the snapshot's recorded provenance is one the
	// control plane does not restore.
	ProvenanceIncompatible ProvenanceState = "incompatible"
	// ProvenanceUnknown means nothing readable was recorded to decide on.
	ProvenanceUnknown ProvenanceState = "unknown"
)

// ProvenanceReason says why a restore's state is what it is: empty for
// compatible.
type ProvenanceReason string

const (
	// ReasonNotRecorded means the row records no provenance for this snapshot.
	ReasonNotRecorded ProvenanceReason = "not_recorded"
	// ReasonNotReported means the minting agent reported no protocol.
	ReasonNotReported ProvenanceReason = "not_reported"
	// ReasonUnreadable means the reported protocol is not MAJOR.MINOR.PATCH.
	ReasonUnreadable ProvenanceReason = "unreadable"
	// ReasonRuntimeUnknown means a runtime floor is set, and the reported
	// runtime version is absent or not MAJOR.MINOR.PATCH.
	ReasonRuntimeUnknown ProvenanceReason = "runtime_unknown"
	// ReasonMajor means the agent's protocol has another MAJOR than the control
	// plane's.
	ReasonMajor ProvenanceReason = "major"
	// ReasonBelowFloor means the agent's protocol is below the floor.
	ReasonBelowFloor ProvenanceReason = "below_floor"
	// ReasonRuntimeBelowFloor means the agent runtime's version is below the
	// runtime floor.
	ReasonRuntimeBelowFloor ProvenanceReason = "runtime_below_floor"
)

// RestoreVerdict is EvaluateSnapshotRestore's answer: the state, whether
// the snapshot is restored, and why the state is what it is.
type RestoreVerdict struct {
	State   ProvenanceState
	Restore bool
	Reason  ProvenanceReason
}

// EvaluateSnapshotRestore decides the restore of a snapshot whose recorded
// provenance is p (technical plan §35.5b):
//
//   - unknown, restored (RestoreWhenProvenanceUnknown): nothing recorded,
//     no protocol reported, or one that does not parse;
//   - incompatible, refused: a protocol of another MAJOR than the control
//     plane's -- an agent newer than the control plane with the same MAJOR
//     is compatible, which covers a control-plane rollback -- or one below
//     the floor;
//   - then, with no runtime floor set, compatible; with one, unknown when
//     the runtime version is absent or does not parse, incompatible when
//     it is below the floor, and compatible otherwise.
//
// A floor or a control-plane protocol that does not parse checks nothing:
// TestRestoreFloors pins that the ones the control plane passes parse.
func EvaluateSnapshotRestore(p SnapshotProvenance, floor RestoreFloor) RestoreVerdict {
	unknown := func(reason ProvenanceReason) RestoreVerdict {
		return RestoreVerdict{State: ProvenanceUnknown, Restore: RestoreWhenProvenanceUnknown, Reason: reason}
	}
	refused := func(reason ProvenanceReason) RestoreVerdict {
		return RestoreVerdict{State: ProvenanceIncompatible, Restore: false, Reason: reason}
	}
	switch {
	case !p.Recorded:
		return unknown(ReasonNotRecorded)
	case p.AgentProtocol == "":
		return unknown(ReasonNotReported)
	}
	protocol, ok := ParseVersion(p.AgentProtocol)
	if !ok {
		return unknown(ReasonUnreadable)
	}
	if cp, ok := ParseVersion(floor.ControlPlaneProtocol); ok && protocol.Major != cp.Major {
		return refused(ReasonMajor)
	}
	if lowest, ok := ParseVersion(floor.AgentProtocol); ok && protocol.Less(lowest) {
		return refused(ReasonBelowFloor)
	}
	runtimeFloor, ok := ParseVersion(floor.RuntimeVersion)
	if !ok {
		return RestoreVerdict{State: ProvenanceCompatible, Restore: true}
	}
	runtime, ok := ParseVersion(p.RuntimeVersion)
	if !ok {
		return unknown(ReasonRuntimeUnknown)
	}
	if runtime.Less(runtimeFloor) {
		return refused(ReasonRuntimeBelowFloor)
	}
	return RestoreVerdict{State: ProvenanceCompatible, Restore: true}
}

// Version is a parsed MAJOR.MINOR.PATCH version.
type Version struct {
	Major, Minor, Patch int
}

var versionRE = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// ParseVersion parses a strict MAJOR.MINOR.PATCH version, surrounding
// space aside, with no prefix, pre-release or build metadata: the shape of
// contracts/VERSION. A copy of tools/contractscompat/compat's ParseSemVer,
// the repository's "duplicate small helpers" convention keeping this
// package free of a dependency on a tool. False for anything else, a
// component past an int included.
func ParseVersion(s string) (Version, bool) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, false
	}
	var parts [3]int
	for i, digits := range m[1:] {
		n, err := strconv.Atoi(digits)
		if err != nil {
			return Version{}, false
		}
		parts[i] = n
	}
	return Version{Major: parts[0], Minor: parts[1], Patch: parts[2]}, true
}

// Less reports whether v sorts strictly before o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}
