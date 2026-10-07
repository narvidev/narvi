package sessionactor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestReportedSnapshotProvenance pins how the control plane reads the
// provenance a snapshot_ready carries (technical plan §35.5b): leniently,
// each member a string or nothing, and nothing at all for a provenance
// that is not an object.
func TestReportedSnapshotProvenance(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		raw               string
		protocol, runtime string // "" for nil
	}{
		{name: "both reported", raw: `{"type":"snapshot_ready","provenance":{"agentProtocol":"1.25.0","runtimeVersion":"1.14.19"}}`, protocol: "1.25.0", runtime: "1.14.19"},
		{name: "a runtime version of null", raw: `{"provenance":{"agentProtocol":"1.25.0","runtimeVersion":null}}`, protocol: "1.25.0"},
		{name: "no runtime version", raw: `{"provenance":{"agentProtocol":"1.25.0"}}`, protocol: "1.25.0"},
		{name: "no protocol", raw: `{"provenance":{"runtimeVersion":"1.14.19"}}`, runtime: "1.14.19"},
		{name: "an empty protocol", raw: `{"provenance":{"agentProtocol":"","runtimeVersion":""}}`},
		{name: "a protocol that is a number", raw: `{"provenance":{"agentProtocol":42,"runtimeVersion":"1.14.19"}}`, runtime: "1.14.19"},
		{name: "a runtime version that is a list", raw: `{"provenance":{"agentProtocol":"1.25.0","runtimeVersion":["1.14.19"]}}`, protocol: "1.25.0"},
		{name: "a provenance that is a string", raw: `{"provenance":"1.25.0"}`},
		{name: "a provenance that is null", raw: `{"provenance":null}`},
		{name: "a provenance that is a list", raw: `{"provenance":[]}`},
		{name: "no provenance", raw: `{"type":"snapshot_ready","snapshotId":"snap-1"}`},
		{name: "a frame that is not JSON", raw: `{"provenance":{"agentProtocol":"1.25.0"}`},
		{name: "the key in another case, as the generated decode matches it", raw: `{"Provenance":{"AgentProtocol":"1.25.0"}}`, protocol: "1.25.0"},
		{name: "unknown members beside", raw: `{"provenance":{"agentProtocol":"1.25.0","imageDigest":"sha256:x"}}`, protocol: "1.25.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			protocol, runtime := reportedSnapshotProvenance(json.RawMessage(tc.raw))
			if stringOrEmpty(protocol) != tc.protocol || stringOrEmpty(runtime) != tc.runtime ||
				(protocol != nil && *protocol == "") || (runtime != nil && *runtime == "") {
				t.Errorf("reportedSnapshotProvenance(%s) = (%v, %v), want (%q, %q), nil for empty", tc.raw, protocol, runtime, tc.protocol, tc.runtime)
			}
		})
	}
}

// TestSnapshotProvenance pins which provenance a sandbox row's snapshot
// reads: only what is keyed to that very snapshot.
func TestSnapshotProvenance(t *testing.T) {
	t.Parallel()
	s := func(v string) *string { return &v }
	tests := []struct {
		name string
		row  sqlcgen.Sandbox
		want sandbox.SnapshotProvenance
	}{
		{"no snapshot", sqlcgen.Sandbox{SnapshotProvenanceID: s("snap-1"), SnapshotAgentProtocol: s("1.25.0")}, sandbox.SnapshotProvenance{}},
		{"nothing recorded", sqlcgen.Sandbox{SnapshotID: s("snap-1")}, sandbox.SnapshotProvenance{}},
		{"another snapshot's provenance", sqlcgen.Sandbox{SnapshotID: s("snap-2"), SnapshotProvenanceID: s("snap-1"), SnapshotAgentProtocol: s("1.24.0")}, sandbox.SnapshotProvenance{}},
		{"recorded, nothing reported", sqlcgen.Sandbox{SnapshotID: s("snap-1"), SnapshotProvenanceID: s("snap-1")}, sandbox.SnapshotProvenance{Recorded: true}},
		{"recorded", sqlcgen.Sandbox{SnapshotID: s("snap-1"), SnapshotProvenanceID: s("snap-1"), SnapshotAgentProtocol: s("1.25.0"), SnapshotRuntimeVersion: s("1.14.19")},
			sandbox.SnapshotProvenance{Recorded: true, AgentProtocol: "1.25.0", RuntimeVersion: "1.14.19"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := snapshotProvenance(tc.row); got != tc.want {
				t.Errorf("snapshotProvenance = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRestoreFloor pins what a restore is measured against: the domain's
// floors, and this binary's own protocol -- never the agent's.
func TestRestoreFloor(t *testing.T) {
	t.Parallel()
	got := restoreFloor()
	want := sandbox.RestoreFloor{AgentProtocol: sandbox.MinRestorableAgentProtocol, RuntimeVersion: sandbox.MinRestorableRuntimeVersion, ControlPlaneProtocol: contracts.Version}
	if got != want {
		t.Fatalf("restoreFloor() = %+v, want %+v", got, want)
	}
	if _, ok := sandbox.ParseVersion(got.ControlPlaneProtocol); !ok {
		t.Fatalf("the control plane's protocol %q does not parse, so no MAJOR is checked", got.ControlPlaneProtocol)
	}
}

// TestSnapshotRefusalWarnings pins what each warning of a snapshot whose
// context is not brought back names: the snapshot, when it was taken when
// that is recorded, and why -- with the versions on either side for a
// refusal on its provenance.
func TestSnapshotRefusalWarnings(t *testing.T) {
	t.Parallel()
	s := func(v string) *string { return &v }
	minted := pgtype.Timestamptz{Time: time.Date(2026, 9, 30, 14, 5, 59, 0, time.FixedZone("CEST", 2*60*60)), Valid: true}
	recordedRow := sqlcgen.Sandbox{SnapshotID: s("snap-9"), SnapshotProvenanceID: s("snap-9"), SnapshotAgentProtocol: s("1.24.0"), SnapshotMintedAt: minted}
	unrecordedRow := sqlcgen.Sandbox{SnapshotID: s("snap-9"), SnapshotProvenanceID: s("snap-8"), SnapshotMintedAt: minted}
	floor := sandbox.RestoreFloor{AgentProtocol: "1.25.0", RuntimeVersion: "1.14.0", ControlPlaneProtocol: "1.27.0"}

	tests := []struct {
		name    string
		warning string
		want    []string
		not     []string
	}{
		{
			name: "below the floor",
			warning: snapshotProvenanceRefusalWarning("snap-9", minted, sandbox.SnapshotProvenance{Recorded: true, AgentProtocol: "1.24.0"},
				sandbox.RestoreVerdict{State: sandbox.ProvenanceIncompatible, Reason: sandbox.ReasonBelowFloor}, floor),
			want: []string{"started fresh instead of from its last snapshot, snap-9 (taken 2026-09-30 12:05 UTC)", "built for protocol 1.24.0", "from protocol 1.25.0 on", "not in the new sandbox", "rebuild the sandbox image"},
		},
		{
			name: "another MAJOR",
			warning: snapshotProvenanceRefusalWarning("snap-9", pgtype.Timestamptz{}, sandbox.SnapshotProvenance{Recorded: true, AgentProtocol: "2.0.0"},
				sandbox.RestoreVerdict{State: sandbox.ProvenanceIncompatible, Reason: sandbox.ReasonMajor}, floor),
			want: []string{"from its last snapshot, snap-9: ", "built for protocol 2.0.0", "at protocol 1.27.0, cannot restore"},
			not:  []string{"(taken"},
		},
		{
			name: "the runtime below its floor",
			warning: snapshotProvenanceRefusalWarning("snap-9", minted, sandbox.SnapshotProvenance{Recorded: true, AgentProtocol: "1.25.0", RuntimeVersion: "1.13.2"},
				sandbox.RestoreVerdict{State: sandbox.ProvenanceIncompatible, Reason: sandbox.ReasonRuntimeBelowFloor}, floor),
			want: []string{"holds agent runtime 1.13.2, older than the 1.14.0"},
		},
		{
			name:    "the shadow downgrade",
			warning: shadowRestoreRefusal(recordedRow, "snap-9").warning,
			want:    []string{"snap-9 (taken 2026-09-30 12:05 UTC)", "shadow mode", "not in the new sandbox"},
			not:     []string{"rebuild the sandbox image"},
		},
		{
			name:    "the Docker downgrade, a snapshot recorded by the previous binary",
			warning: dockerRestoreRefusal(unrecordedRow, "snap-9").warning,
			want:    []string{"from its last snapshot, snap-9: ", "requires Docker"},
			not:     []string{"(taken"},
		},
		{
			name:    "the review checkout's retirement of an old agent, its snapshot's protocol recorded",
			warning: checkoutRetirementWarning(recordedRow, "snap-9", turn.CheckoutVerdict{Retirement: turn.CheckoutRetiredOldAgent}),
			want:    []string{"last snapshot, snap-9, discarded", "cannot check out the commit", "an agent of protocol 1.24.0", "starting from the repository"},
		},
		{
			name:    "the review checkout's retirement of an old agent, nothing recorded",
			warning: checkoutRetirementWarning(unrecordedRow, "snap-9", turn.CheckoutVerdict{Retirement: turn.CheckoutRetiredOldAgent}),
			want:    []string{"nothing was recorded of the agent the snapshot holds"},
			not:     []string{"protocol"},
		},
		{
			name:    "the review checkout's retirement of a failing worktree",
			warning: checkoutRetirementWarning(recordedRow, "snap-9", turn.CheckoutVerdict{Retirement: turn.CheckoutRetiredFailing}),
			want:    []string{"last snapshot, snap-9, discarded", "kept failing", "which the snapshot holds too"},
			not:     []string{"protocol"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, w := range tc.want {
				if !strings.Contains(tc.warning, w) {
					t.Errorf("warning %q does not contain %q", tc.warning, w)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(tc.warning, w) {
					t.Errorf("warning %q contains %q", tc.warning, w)
				}
			}
		})
	}
}

// TestEvaluateSnapshotRestore_RefusalClearsOnlyOnProvenance pins which
// fresh spawns clear the snapshot: a refusal on its provenance, which never
// stops applying, and neither downgrade, which may.
func TestEvaluateSnapshotRestore_RefusalClearsOnlyOnProvenance(t *testing.T) {
	t.Parallel()
	s := func(v string) *string { return &v }
	row := func(protocol string) sqlcgen.Sandbox {
		return sqlcgen.Sandbox{SnapshotID: s("snap-1"), SnapshotProvenanceID: s("snap-1"), SnapshotAgentProtocol: s(protocol)}
	}
	if d, r := evaluateSnapshotRestore(row("1.24.0"), "snap-1"); r == nil || !r.clear || r.snapshotID != "snap-1" || d.verdict.Restore {
		t.Errorf("a protocol below the floor: decision %+v, refusal %+v; want refused and cleared", d, r)
	}
	if d, r := evaluateSnapshotRestore(row(contracts.Version), "snap-1"); r != nil || d.verdict.State != sandbox.ProvenanceCompatible {
		t.Errorf("this binary's protocol: decision %+v, refusal %+v; want restored as compatible", d, r)
	}
	if d, r := evaluateSnapshotRestore(sqlcgen.Sandbox{SnapshotID: s("snap-1")}, "snap-1"); r != nil || d.verdict.State != sandbox.ProvenanceUnknown {
		t.Errorf("nothing recorded: decision %+v, refusal %+v; want restored as unknown", d, r)
	}
	if r := shadowRestoreRefusal(row("1.25.0"), "snap-1"); r.clear {
		t.Error("the shadow downgrade clears the snapshot, want it kept")
	}
	if r := dockerRestoreRefusal(row("1.25.0"), "snap-1"); r.clear {
		t.Error("the Docker downgrade clears the snapshot, want it kept")
	}
}
