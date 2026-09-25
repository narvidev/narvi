package platform_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/platform"
)

// TestCanonicalOrigin pins RFC 6454 origin canonicalization: case folding
// of scheme and host, default-port elision per scheme (and ONLY the
// scheme's own default), path/query dropped, IPv6 re-bracketed, and
// refusal of anything that is not an absolute URL.
func TestCanonicalOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"http://example.test", "http://example.test", false},
		{"HTTP://EXAMPLE.test", "http://example.test", false},
		{"http://example.test:80", "http://example.test", false},
		{"http://example.test:443", "http://example.test:443", false},
		{"https://example.test:443/some/path?q=1", "https://example.test", false},
		{"https://example.test:80", "https://example.test:80", false},
		{"https://[::1]:8443", "https://[::1]:8443", false},
		{"https://[::1]:443", "https://[::1]", false},
		{"https://[FE80::1]", "https://[fe80::1]", false},
		{"http://127.0.0.1:52345", "http://127.0.0.1:52345", false},
		{"null", "", true},
		{"/relative", "", true},
		{"example.test", "", true},
		{"", "", true},
		{"http://%zz", "", true},
	}
	for _, tc := range tests {
		got, err := platform.CanonicalOrigin(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("CanonicalOrigin(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("CanonicalOrigin(%q) = %q, %v, want %q", tc.in, got, err, tc.want)
		}
	}
}
