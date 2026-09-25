package mcpscope_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/narvidev/narvi/internal/domain/mcpscope"
)

// TestSatisfies_Matrix is the full (granted, required) matrix, including
// every degenerate input: the empty scope and an unknown scope, on either
// side, are satisfied by nothing and grant nothing.
func TestSatisfies_Matrix(t *testing.T) {
	t.Parallel()

	const unknown = mcpscope.Scope("mcp:admin")
	tests := []struct {
		name     string
		granted  []mcpscope.Scope
		required mcpscope.Scope
		want     bool
	}{
		{"nothing granted, read required", nil, mcpscope.Read, false},
		{"empty grant, read required", []mcpscope.Scope{}, mcpscope.Read, false},
		{"nothing granted, write required", nil, mcpscope.Write, false},
		{"read grants read", []mcpscope.Scope{mcpscope.Read}, mcpscope.Read, true},
		{"read does not grant write", []mcpscope.Scope{mcpscope.Read}, mcpscope.Write, false},
		{"write grants write", []mcpscope.Scope{mcpscope.Write}, mcpscope.Write, true},
		{"write implies read", []mcpscope.Scope{mcpscope.Write}, mcpscope.Read, true},
		{"both grant read", []mcpscope.Scope{mcpscope.Read, mcpscope.Write}, mcpscope.Read, true},
		{"unknown granted grants nothing", []mcpscope.Scope{unknown}, mcpscope.Read, false},
		{"unknown required is satisfied by nothing", []mcpscope.Scope{mcpscope.Read, mcpscope.Write}, unknown, false},
		{"empty required is satisfied by nothing", []mcpscope.Scope{mcpscope.Read, mcpscope.Write}, "", false},
		{"empty granted string grants nothing", []mcpscope.Scope{""}, mcpscope.Read, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mcpscope.Satisfies(tc.granted, tc.required); got != tc.want {
				t.Fatalf("Satisfies(%v, %q) = %v, want %v", tc.granted, tc.required, got, tc.want)
			}
		})
	}
}

// TestCovers_Matrix: narrowing is allowed, widening never is -- whatever
// the requested set, a covered one grants nothing held did not.
func TestCovers_Matrix(t *testing.T) {
	t.Parallel()

	const unknown = mcpscope.Scope("mcp:admin")
	read, write := mcpscope.Read, mcpscope.Write
	tests := []struct {
		name      string
		held      []mcpscope.Scope
		requested []mcpscope.Scope
		want      bool
	}{
		{"nothing held, nothing requested", nil, nil, true},
		{"nothing held, read requested", nil, []mcpscope.Scope{read}, false},
		{"read held, nothing requested", []mcpscope.Scope{read}, []mcpscope.Scope{}, true},
		{"read held, read requested", []mcpscope.Scope{read}, []mcpscope.Scope{read}, true},
		{"read held, write requested", []mcpscope.Scope{read}, []mcpscope.Scope{write}, false},
		{"read held, read and write requested", []mcpscope.Scope{read}, []mcpscope.Scope{read, write}, false},
		{"write held, read requested (narrowing through the hierarchy)", []mcpscope.Scope{write}, []mcpscope.Scope{read}, true},
		{"write held, write requested", []mcpscope.Scope{write}, []mcpscope.Scope{write}, true},
		{"unknown held covers nothing", []mcpscope.Scope{unknown}, []mcpscope.Scope{read}, false},
		{"unknown requested is covered by nothing", []mcpscope.Scope{read, write}, []mcpscope.Scope{unknown}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mcpscope.Covers(tc.held, tc.requested); got != tc.want {
				t.Fatalf("Covers(%v, %v) = %v, want %v", tc.held, tc.requested, got, tc.want)
			}
		})
	}
}

func TestImplies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   mcpscope.Scope
		want []mcpscope.Scope
	}{
		{mcpscope.Read, []mcpscope.Scope{mcpscope.Read}},
		{mcpscope.Write, []mcpscope.Scope{mcpscope.Read, mcpscope.Write}},
		{"", nil},
		{"mcp:other", nil},
	}
	for _, tc := range tests {
		if got := mcpscope.Implies(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Implies(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestAdvertised pins "only what a tool requires is offered": write is
// never advertised while no tool requires it, unknown and empty entries
// are dropped, and the result is deduplicated in canonical order.
func TestAdvertised(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		required []mcpscope.Scope
		want     []mcpscope.Scope
	}{
		{"no tools", nil, []mcpscope.Scope{}},
		{"three read tools", []mcpscope.Scope{mcpscope.Read, mcpscope.Read, mcpscope.Read}, []mcpscope.Scope{mcpscope.Read}},
		{"read and write tools, canonical order", []mcpscope.Scope{mcpscope.Write, mcpscope.Read}, []mcpscope.Scope{mcpscope.Read, mcpscope.Write}},
		{"a tool with no scope set is not an offer", []mcpscope.Scope{"", mcpscope.Read}, []mcpscope.Scope{mcpscope.Read}},
		{"an unknown scope is not an offer", []mcpscope.Scope{"mcp:other"}, []mcpscope.Scope{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mcpscope.Advertised(tc.required); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Advertised(%v) = %v, want %v", tc.required, got, tc.want)
			}
		})
	}
}

func TestParseRequested(t *testing.T) {
	t.Parallel()

	readOnly := []mcpscope.Scope{mcpscope.Read}
	tests := []struct {
		name    string
		raw     string
		offered []mcpscope.Scope
		want    []mcpscope.Scope
		wantErr bool
	}{
		{"absent scope is a request for nothing", "", readOnly, []mcpscope.Scope{}, false},
		{"whitespace only is a request for nothing", "   ", readOnly, []mcpscope.Scope{}, false},
		{"one offered scope", "mcp:read", readOnly, []mcpscope.Scope{mcpscope.Read}, false},
		{"duplicates collapse", "mcp:read mcp:read", readOnly, []mcpscope.Scope{mcpscope.Read}, false},
		{"known but not offered is refused", "mcp:write", readOnly, nil, true},
		{"one bad value refuses the whole request", "mcp:read mcp:write", readOnly, nil, true},
		{"unknown value refused", "openid", readOnly, nil, true},
		{"case-sensitive", "MCP:READ", readOnly, nil, true},
		{"canonical order", "mcp:write mcp:read", []mcpscope.Scope{mcpscope.Read, mcpscope.Write}, []mcpscope.Scope{mcpscope.Read, mcpscope.Write}, false},
		{"nothing offered refuses everything", "mcp:read", nil, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := mcpscope.ParseRequested(tc.raw, tc.offered)
			if tc.wantErr {
				if !errors.Is(err, mcpscope.ErrUnknownScope) {
					t.Fatalf("ParseRequested(%q) err = %v, want ErrUnknownScope", tc.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRequested(%q) err = %v", tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseRequested(%q) = %#v, want %#v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestJoinAndStrings(t *testing.T) {
	t.Parallel()

	if got := mcpscope.Join(nil); got != "" {
		t.Errorf("Join(nil) = %q, want empty", got)
	}
	if got := mcpscope.Join([]mcpscope.Scope{mcpscope.Read, mcpscope.Write}); got != "mcp:read mcp:write" {
		t.Errorf("Join = %q", got)
	}
	if got := mcpscope.Strings(nil); got == nil || len(got) != 0 {
		t.Errorf("Strings(nil) = %#v, want empty non-nil", got)
	}
	if got := mcpscope.FromStrings([]string{"mcp:read", "x"}); !reflect.DeepEqual(got, []mcpscope.Scope{mcpscope.Read, "x"}) {
		t.Errorf("FromStrings = %#v", got)
	}
}
