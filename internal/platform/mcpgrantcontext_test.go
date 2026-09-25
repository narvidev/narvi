package platform_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/narvidev/narvi/internal/platform"
)

func TestMCPGrantContext(t *testing.T) {
	t.Parallel()

	if _, ok := platform.MCPGrantFromContext(context.Background()); ok {
		t.Fatalf("MCPGrantFromContext(empty ctx) ok = true, want false")
	}

	scopes := []string{"mcp:read"}
	ctx := platform.WithMCPGrant(context.Background(), platform.MCPGrant{GrantID: "g", ClientID: "c", Scopes: scopes})
	scopes[0] = "mcp:write" // a caller's later mutation must not leak in

	got, ok := platform.MCPGrantFromContext(ctx)
	if !ok {
		t.Fatalf("MCPGrantFromContext ok = false, want true")
	}
	want := platform.MCPGrant{GrantID: "g", ClientID: "c", Scopes: []string{"mcp:read"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MCPGrantFromContext = %+v, want %+v", got, want)
	}
	got.Scopes[0] = "mutated"
	again, _ := platform.MCPGrantFromContext(ctx)
	if again.Scopes[0] != "mcp:read" {
		t.Fatalf("a reader's mutation leaked into the context: %+v", again)
	}

	empty := platform.WithMCPGrant(context.Background(), platform.MCPGrant{GrantID: "g"})
	g, ok := platform.MCPGrantFromContext(empty)
	if !ok || g.Scopes == nil || len(g.Scopes) != 0 {
		t.Fatalf("scope-less grant = %+v, ok = %v, want present with empty non-nil scopes", g, ok)
	}
}
