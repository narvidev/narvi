package httpapi

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

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

// TestWaitKey_GrantThenUser: a wait counts against the MCP grant it was
// authenticated under, else the signed-in user -- two grants of one user
// are two keys, and a grant and its user's cookie are two keys.
func TestWaitKey_GrantThenUser(t *testing.T) {
	t.Parallel()
	user := platform.WithUser(context.Background(), platform.AuthenticatedUser{ID: "u1", Role: "member"})
	if got := waitKey(user); got != "user:u1" {
		t.Fatalf("cookie request key = %q, want user:u1", got)
	}
	g1 := platform.WithMCPGrant(user, platform.MCPGrant{GrantID: "g1", Scopes: []string{"mcp:read"}})
	g2 := platform.WithMCPGrant(user, platform.MCPGrant{GrantID: "g2", Scopes: []string{"mcp:read"}})
	if waitKey(g1) != "grant:g1" || waitKey(g2) != "grant:g2" {
		t.Fatalf("bearer keys = %q, %q, want grant:g1 and grant:g2", waitKey(g1), waitKey(g2))
	}
	if got := waitKey(context.Background()); got != "" {
		t.Fatalf("no principal key = %q, want empty", got)
	}
}
