package auditlog

import (
	"context"
	"maps"
	"reflect"
	"testing"

	"github.com/narvidev/narvi/internal/platform"
)

// TestStampMCPGrant_Table is the stamp's own rule (technical plan §43.18),
// without a database: under a grant the detail gains mcp = {grant_id,
// client_id} on a copy, the grant overriding a caller's own "mcp" key;
// without one the caller's map is returned as it is. The caller's map is
// never modified. TestAuditRecord_StampsMCPGrant (httpapi) proves Record
// writes the stamp to a real row.
func TestStampMCPGrant_Table(t *testing.T) {
	t.Parallel()
	grant := platform.MCPGrant{GrantID: "g-1", ClientID: "narvi_mcp_c_1", Scopes: []string{"mcp:write"}}
	stamp := map[string]any{"grant_id": "g-1", "client_id": "narvi_mcp_c_1"}
	tests := []struct {
		name   string
		ctx    context.Context
		detail map[string]any
		want   map[string]any
	}{
		{"no grant: the detail as it is", context.Background(), map[string]any{"a": 1}, map[string]any{"a": 1}},
		{"no grant, nil detail: nil", context.Background(), nil, nil},
		{"a grant: the detail and the stamp", platform.WithMCPGrant(context.Background(), grant), map[string]any{"a": 1}, map[string]any{"a": 1, "mcp": stamp}},
		{"a grant, nil detail: the stamp alone", platform.WithMCPGrant(context.Background(), grant), nil, map[string]any{"mcp": stamp}},
		{"a grant overrides the caller's own mcp key", platform.WithMCPGrant(context.Background(), grant), map[string]any{"mcp": "forged"}, map[string]any{"mcp": stamp}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := maps.Clone(tc.detail)
			got := stampMCPGrant(tc.ctx, tc.detail)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("stampMCPGrant = %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(tc.detail, before) {
				t.Fatalf("the caller's map became %v, was %v", tc.detail, before)
			}
		})
	}
}
