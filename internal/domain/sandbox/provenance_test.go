package sandbox_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/sandbox"
)

// TestEvaluateSnapshotRestore is every row of technical plan §35.5b's
// three-state restore decision: what the snapshot's minting agent
// reported, against the control plane's protocol and the floors.
func TestEvaluateSnapshotRestore(t *testing.T) {
	t.Parallel()

	floor := sandbox.RestoreFloor{AgentProtocol: "1.25.0", ControlPlaneProtocol: "1.27.3"}
	runtimeFloor := sandbox.RestoreFloor{AgentProtocol: "1.25.0", RuntimeVersion: "1.14.0", ControlPlaneProtocol: "1.27.3"}
	recorded := func(protocol, runtime string) sandbox.SnapshotProvenance {
		return sandbox.SnapshotProvenance{Recorded: true, AgentProtocol: protocol, RuntimeVersion: runtime}
	}
	unknown := func(reason sandbox.ProvenanceReason) sandbox.RestoreVerdict {
		return sandbox.RestoreVerdict{State: sandbox.ProvenanceUnknown, Restore: true, Reason: reason}
	}
	refused := func(reason sandbox.ProvenanceReason) sandbox.RestoreVerdict {
		return sandbox.RestoreVerdict{State: sandbox.ProvenanceIncompatible, Restore: false, Reason: reason}
	}
	compatible := sandbox.RestoreVerdict{State: sandbox.ProvenanceCompatible, Restore: true}

	tests := []struct {
		name  string
		p     sandbox.SnapshotProvenance
		floor sandbox.RestoreFloor
		want  sandbox.RestoreVerdict
	}{
		{"nothing recorded for the snapshot", sandbox.SnapshotProvenance{}, floor, unknown(sandbox.ReasonNotRecorded)},
		{"nothing recorded, whatever the columns hold", sandbox.SnapshotProvenance{AgentProtocol: "0.1.0"}, floor, unknown(sandbox.ReasonNotRecorded)},
		{"recorded, the agent reported no protocol", recorded("", "1.14.19"), floor, unknown(sandbox.ReasonNotReported)},
		{"recorded, a protocol that does not parse", recorded("1.25", ""), floor, unknown(sandbox.ReasonUnreadable)},
		{"recorded, a prefixed protocol", recorded("v1.25.0", ""), floor, unknown(sandbox.ReasonUnreadable)},
		{"recorded, a pre-release protocol", recorded("1.25.0-rc.1", ""), floor, unknown(sandbox.ReasonUnreadable)},
		{"an older MAJOR", recorded("0.30.0", ""), floor, refused(sandbox.ReasonMajor)},
		{"a newer MAJOR", recorded("2.0.0", ""), floor, refused(sandbox.ReasonMajor)},
		{"below the floor by MINOR", recorded("1.24.9", "1.14.19"), floor, refused(sandbox.ReasonBelowFloor)},
		{"below the floor by PATCH", recorded("1.25.0", ""), sandbox.RestoreFloor{AgentProtocol: "1.25.1", ControlPlaneProtocol: "1.27.3"}, refused(sandbox.ReasonBelowFloor)},
		{"at the floor", recorded("1.25.0", ""), floor, compatible},
		{"above the floor", recorded("1.26.4", "1.14.19"), floor, compatible},
		{"newer than the control plane, same MAJOR (a control-plane rollback)", recorded("1.31.0", ""), floor, compatible},
		{"surrounding space", recorded(" 1.25.0\n", ""), floor, compatible},
		{"runtime floor set, no runtime reported", recorded("1.25.0", ""), runtimeFloor, unknown(sandbox.ReasonRuntimeUnknown)},
		{"runtime floor set, a runtime that does not parse", recorded("1.25.0", "local-build"), runtimeFloor, unknown(sandbox.ReasonRuntimeUnknown)},
		{"runtime floor set, below it", recorded("1.25.0", "1.13.99"), runtimeFloor, refused(sandbox.ReasonRuntimeBelowFloor)},
		{"runtime floor set, at it", recorded("1.25.0", "1.14.0"), runtimeFloor, compatible},
		{"runtime floor set, above it", recorded("1.25.0", "1.14.19"), runtimeFloor, compatible},
		{"runtime floor set, the protocol below its floor first", recorded("1.24.0", "1.14.19"), runtimeFloor, refused(sandbox.ReasonBelowFloor)},
		{"runtime floor set, an unreadable protocol stays unknown", recorded("one", "1.13.0"), runtimeFloor, unknown(sandbox.ReasonUnreadable)},
		{"no floor and no control-plane protocol check nothing", recorded("0.1.0", ""), sandbox.RestoreFloor{}, compatible},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sandbox.EvaluateSnapshotRestore(tc.p, tc.floor); got != tc.want {
				t.Errorf("EvaluateSnapshotRestore(%+v, %+v) = %+v, want %+v", tc.p, tc.floor, got, tc.want)
			}
		})
	}
}

// TestEvaluateSnapshotRestore_UnknownIsNeverCompatible pins that every
// unknown verdict carries the unknown state and a reason, whatever the
// restore policy says: an unrecorded version is never read as "probably
// fine".
func TestEvaluateSnapshotRestore_UnknownIsNeverCompatible(t *testing.T) {
	t.Parallel()
	floor := sandbox.RestoreFloor{AgentProtocol: sandbox.MinRestorableAgentProtocol, RuntimeVersion: "1.0.0", ControlPlaneProtocol: "1.25.0"}
	for _, p := range []sandbox.SnapshotProvenance{
		{},
		{Recorded: true},
		{Recorded: true, AgentProtocol: "garbage"},
		{Recorded: true, AgentProtocol: "1.25.0"},
	} {
		got := sandbox.EvaluateSnapshotRestore(p, floor)
		if got.State != sandbox.ProvenanceUnknown || got.Reason == "" || got.Restore != sandbox.RestoreWhenProvenanceUnknown {
			t.Errorf("EvaluateSnapshotRestore(%+v) = %+v, want unknown with a reason, restored as RestoreWhenProvenanceUnknown says", p, got)
		}
	}
}

// TestRestoreFloors pins the floors technical plan §35.5b sets: the agent
// protocol floor is the first protocol that reports at all, and parses,
// and there is no runtime floor.
func TestRestoreFloors(t *testing.T) {
	t.Parallel()
	if _, ok := sandbox.ParseVersion(sandbox.MinRestorableAgentProtocol); !ok || sandbox.MinRestorableAgentProtocol != "1.25.0" {
		t.Errorf("MinRestorableAgentProtocol = %q, want 1.25.0, parsed", sandbox.MinRestorableAgentProtocol)
	}
	if sandbox.MinRestorableRuntimeVersion != "" {
		t.Errorf("MinRestorableRuntimeVersion = %q, want none", sandbox.MinRestorableRuntimeVersion)
	}
	if !sandbox.RestoreWhenProvenanceUnknown {
		t.Error("RestoreWhenProvenanceUnknown = false, want a snapshot of unknown provenance restored")
	}
}

// TestParseVersion pins the strict MAJOR.MINOR.PATCH parser the restore
// decision reads versions with.
func TestParseVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want sandbox.Version
		ok   bool
	}{
		{"1.25.0", sandbox.Version{Major: 1, Minor: 25}, true},
		{"0.0.0", sandbox.Version{}, true},
		{"10.200.3000", sandbox.Version{Major: 10, Minor: 200, Patch: 3000}, true},
		{" 1.2.3\t", sandbox.Version{Major: 1, Minor: 2, Patch: 3}, true},
		{"01.2.3", sandbox.Version{Major: 1, Minor: 2, Patch: 3}, true},
		{"", sandbox.Version{}, false},
		{"1.2", sandbox.Version{}, false},
		{"1.2.3.4", sandbox.Version{}, false},
		{"v1.2.3", sandbox.Version{}, false},
		{"1.2.3-beta", sandbox.Version{}, false},
		{"1.2.3+build", sandbox.Version{}, false},
		{"-1.2.3", sandbox.Version{}, false},
		{"+1.2.3", sandbox.Version{}, false},
		{"1.x.3", sandbox.Version{}, false},
		{"99999999999999999999.0.0", sandbox.Version{}, false},
	}
	for _, tc := range tests {
		got, ok := sandbox.ParseVersion(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseVersion(%q) = (%+v, %v), want (%+v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestVersionLess pins the order the floors compare by.
func TestVersionLess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b sandbox.Version
		want bool
	}{
		{sandbox.Version{Major: 1, Minor: 24, Patch: 9}, sandbox.Version{Major: 1, Minor: 25}, true},
		{sandbox.Version{Major: 1, Minor: 25}, sandbox.Version{Major: 1, Minor: 25}, false},
		{sandbox.Version{Major: 1, Minor: 25, Patch: 1}, sandbox.Version{Major: 1, Minor: 25}, false},
		{sandbox.Version{Major: 0, Minor: 99, Patch: 99}, sandbox.Version{Major: 1}, true},
		{sandbox.Version{Major: 2}, sandbox.Version{Major: 1, Minor: 99}, false},
		{sandbox.Version{Major: 1, Minor: 2, Patch: 3}, sandbox.Version{Major: 1, Minor: 2, Patch: 4}, true},
	}
	for _, tc := range tests {
		if got := tc.a.Less(tc.b); got != tc.want {
			t.Errorf("%+v.Less(%+v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
