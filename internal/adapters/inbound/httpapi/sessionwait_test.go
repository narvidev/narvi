package httpapi

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/narvidev/narvi/internal/app/sessionactivity"
	"github.com/narvidev/narvi/internal/platform"
)

// TestParseWaitSeconds_Table pins ?waitSeconds= (technical plan §43.20,
// row 182's wait): absent or 0 is the plain read; a positive whole number
// is a wait, one past int64 included (clamped by the Waiter like any large
// value, never refused); negative, empty, fractional or non-decimal
// values are a 400.
func TestParseWaitSeconds_Table(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		query  string
		want   int64
		wantOK bool
	}{
		{"absent", "", 0, true},
		{"another parameter only", "?limit=3", 0, true},
		{"zero is the plain read", "?waitSeconds=0", 0, true},
		{"one", "?waitSeconds=1", 1, true},
		{"the shipped maximum", "?waitSeconds=25", 25, true},
		{"past the maximum is the Waiter's to clamp", "?waitSeconds=3600", 3600, true},
		{"the largest int64", "?waitSeconds=9223372036854775807", math.MaxInt64, true},
		{"past int64 reads as the largest", "?waitSeconds=99999999999999999999", math.MaxInt64, true},
		{"the first of two values", "?waitSeconds=4&waitSeconds=-1", 4, true},
		{"negative", "?waitSeconds=-1", 0, false},
		{"negative past int64", "?waitSeconds=-99999999999999999999", 0, false},
		{"empty", "?waitSeconds=", 0, false},
		{"a fraction", "?waitSeconds=1.5", 0, false},
		{"an exponent", "?waitSeconds=1e2", 0, false},
		{"a word", "?waitSeconds=forever", 0, false},
		{"hexadecimal", "?waitSeconds=0x10", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			got, ok := parseWaitSeconds(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/x/status"+tc.query, nil))
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("parseWaitSeconds(%q) = %d, %v, want %d, %v", tc.query, got, ok, tc.want, tc.wantOK)
			}
			if !ok && (rec.Code != http.StatusBadRequest || rec.Body.String() != `{"error":"malformed waitSeconds"}`+"\n") {
				t.Fatalf("refusal = %d %q, want 400 malformed waitSeconds", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestWaitCaller_GrantThenUser: a wait's key is the MCP grant it was
// authenticated under, else the signed-in user -- two grants of one user
// are two keys, and a grant and its user's cookie are two keys -- while its
// user is the signed-in user on every surface, so all of them count
// against the one per-user cap. Mutation: a bearer wait's user taken from
// the grant (each grant its own user) fails the shared-user check.
func TestWaitCaller_GrantThenUser(t *testing.T) {
	t.Parallel()
	user := platform.WithUser(context.Background(), platform.AuthenticatedUser{ID: "u1", Role: "member"})
	g1 := platform.WithMCPGrant(user, platform.MCPGrant{GrantID: "g1", Scopes: []string{"mcp:read"}})
	g2 := platform.WithMCPGrant(user, platform.MCPGrant{GrantID: "g2", Scopes: []string{"mcp:read"}})
	other := platform.WithMCPGrant(platform.WithUser(context.Background(), platform.AuthenticatedUser{ID: "u2", Role: "member"}),
		platform.MCPGrant{GrantID: "g3", Scopes: []string{"mcp:read"}})
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want sessionactivity.Caller
	}{
		{"a cookie request counts against its user, as key and as user", user, sessionactivity.Caller{Key: "user:u1", User: "u1"}},
		{"a bearer request counts against its grant, and its grant's user", g1, sessionactivity.Caller{Key: "grant:g1", User: "u1"}},
		{"another grant of the same user is another key, the same user", g2, sessionactivity.Caller{Key: "grant:g2", User: "u1"}},
		{"another user's grant is neither", other, sessionactivity.Caller{Key: "grant:g3", User: "u2"}},
		{"no principal counts against nobody in particular", context.Background(), sessionactivity.Caller{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := waitCaller(tc.ctx); got != tc.want {
				t.Fatalf("waitCaller = %+v, want %+v", got, tc.want)
			}
		})
	}
}
