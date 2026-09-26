package cimdfetch

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
)

// TestOrigin_Table is origin() as RFC 6454 compares origins: an https URL
// naming no port is on 443, so the implicit and the explicit spelling are
// one origin; ASCII letters fold.
func TestOrigin_Table(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, url, want string
	}{
		{"an https URL naming no port is on 443", "https://doc.example/client.json", "https://doc.example:443"},
		{"an explicit 443", "https://doc.example:443/client.json", "https://doc.example:443"},
		{"another port", "https://doc.example:8443/client.json", "https://doc.example:8443"},
		{"ASCII case folds, scheme and host", "HTTPS://DOC.Example/client.json", "https://doc.example:443"},
		{"an IPv6 literal", "https://[2001:db8::1]/client.json", "https://[2001:db8::1]:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.url)
			if err != nil {
				t.Fatal(err)
			}
			if got := origin(u); got != tc.want {
				t.Fatalf("origin(%s) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// TestCheckRedirect_Table is the redirect policy on its own, given the
// Location as the client hands it over: the implicit and the explicit
// 443 are one origin, both ways.
func TestCheckRedirect_Table(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, from, location string
		wantErr              error // nil: the redirect is followed
	}{
		{"implicit 443 to explicit 443", "https://doc.example/client.json", "https://doc.example:443/moved/client.json", nil},
		{"explicit 443 to implicit 443", "https://doc.example:443/client.json", "https://doc.example/moved/client.json", nil},
		{"implicit 443 to another port", "https://doc.example/client.json", "https://doc.example:8443/moved/client.json", ErrCrossOriginRedirect},
		{"a relative Location", "https://doc.example/client.json", "/moved/client.json", nil},
		{"a scheme-relative Location", "https://doc.example/client.json", "//doc.example/moved/client.json", nil},
		{"ASCII case only", "https://doc.example/client.json", "https://DOC.EXAMPLE/moved/client.json", nil},
		{"another host", "https://doc.example/client.json", "https://other.example/client.json", ErrCrossOriginRedirect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from, err := url.Parse(tc.from)
			if err != nil {
				t.Fatal(err)
			}
			// As net/http builds the next request: the Location resolved
			// against the current URL, and the response that named it.
			to, err := from.Parse(tc.location)
			if err != nil {
				t.Fatal(err)
			}
			resp := &http.Response{Header: http.Header{"Location": {tc.location}}}
			err = checkRedirect(&http.Request{URL: to, Response: resp}, []*http.Request{{URL: from}})
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
				t.Fatalf("checkRedirect(%s -> %s) = %v, want %v", tc.from, tc.location, err, tc.wantErr)
			}
		})
	}
}
