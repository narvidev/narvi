package cimdfetch

import (
	"errors"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

// TestCheckAddr_Table is the address policy, block by block: every
// special-purpose range refused, its IPv4-mapped and NAT64-embedded forms
// refused with it, and the addresses just outside each range allowed (so
// a block is not accidentally wider than it should be).
func TestCheckAddr_Table(t *testing.T) {
	t.Parallel()
	tests := []struct {
		addr    string
		allowed bool
	}{
		{"1.2.3.4", true},
		{"0.0.0.0", false},
		{"0.1.2.3", false},
		{"10.0.0.1", false},
		{"10.255.255.255", false},
		{"11.0.0.1", true},
		{"100.63.255.255", true},
		{"100.64.0.1", false},
		{"100.127.255.255", false},
		{"100.128.0.0", true},
		{"127.0.0.1", false},
		{"127.255.255.254", false},
		{"169.254.169.254", false},
		{"169.254.0.1", false},
		{"172.15.255.255", true},
		{"172.16.0.1", false},
		{"172.31.255.255", false},
		{"172.32.0.0", true},
		{"192.0.0.8", false},
		{"192.0.2.10", false},
		{"192.88.99.1", false},
		{"192.167.255.255", true},
		{"192.168.1.1", false},
		{"192.169.0.0", true},
		{"198.18.0.1", false},
		{"198.51.100.7", false},
		{"203.0.113.9", false},
		{"224.0.0.251", false},
		{"239.255.255.250", false},
		{"240.0.0.1", false},
		{"255.255.255.255", false},

		{"::", false},
		{"::1", false},
		{"::127.0.0.1", false}, // IPv4-compatible (deprecated): outside global unicast
		{"::ffff:127.0.0.1", false},
		{"::ffff:10.0.0.1", false},
		{"::ffff:169.254.169.254", false},
		{"::ffff:1.2.3.4", true},
		{"64:ff9b::a00:1", false},     // NAT64 embedding 10.0.0.1
		{"64:ff9b::7f00:1", false},    // NAT64 embedding 127.0.0.1
		{"64:ff9b::102:304", true},    // NAT64 embedding 1.2.3.4
		{"64:ff9b:1::102:304", false}, // local-use NAT64: outside global unicast
		{"fc00::1", false},
		{"fd12:3456::1", false},
		{"fe80::1", false},
		{"febf::1", false},
		{"fec0::1", false},
		{"ff02::1", false},
		{"100::1", false},
		{"2001::1", false},       // Teredo
		{"2001:2::1", false},     // benchmarking
		{"2001:db8::1", false},   // documentation
		{"2002:a00:1::1", false}, // 6to4 embedding 10.0.0.1
		{"3fff::1", false},
		{"5f00::1", false},
		{"2400:1::1", true},
		{"2a00:1::1", true},
	}
	for _, tc := range tests {
		err := checkAddr(netip.MustParseAddr(tc.addr))
		if (err == nil) != tc.allowed {
			t.Errorf("checkAddr(%s) = %v, want allowed=%v", tc.addr, err, tc.allowed)
		}
		if err != nil && !errors.Is(err, ErrForbiddenAddress) {
			t.Errorf("checkAddr(%s) = %v, want it to wrap ErrForbiddenAddress", tc.addr, err)
		}
	}
	if err := checkAddr(netip.MustParseAddr("fe80::1%eth0")); err == nil {
		t.Errorf("a zoned address was allowed")
	}
	if err := checkAddr(netip.Addr{}); err == nil {
		t.Errorf("the zero address was allowed")
	}
}

// TestControl_ChecksTheConnectingAddress: the dial-time hook refuses by
// the address it is handed, admits a test-allowed pair only exactly, and
// refuses anything that is not TCP.
func TestControl_ChecksTheConnectingAddress(t *testing.T) {
	t.Parallel()
	g := &guard{allowed: map[netip.AddrPort]bool{netip.MustParseAddrPort("127.0.0.1:8443"): true}}
	tests := []struct {
		network, address string
		allowed          bool
	}{
		{"tcp4", "1.2.3.4:443", true},
		{"tcp4", "127.0.0.1:8443", true},
		{"tcp6", "[::ffff:127.0.0.1]:8443", true}, // the same allowed pair, spelled mapped
		{"tcp4", "127.0.0.1:8444", false},         // another port on the allowed address
		{"tcp4", "127.0.0.2:8443", false},
		{"tcp4", "10.0.0.1:443", false},
		{"tcp6", "[fe80::1%eth0]:443", false},
		{"udp4", "1.2.3.4:443", false},
		{"unix", "/var/run/socket", false},
		{"tcp4", "not-an-address", false},
	}
	for _, tc := range tests {
		err := g.control(tc.network, tc.address, nil)
		if (err == nil) != tc.allowed {
			t.Errorf("control(%s, %s) = %v, want allowed=%v", tc.network, tc.address, err, tc.allowed)
		}
	}
}

// TestCacheMaxAge_Table: what a response's Cache-Control says about how
// long the document may be trusted -- which the caller may only use to
// shorten its own ceiling.
func TestCacheMaxAge_Table(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values []string
		want   time.Duration
		has    bool
	}{
		{"absent", nil, 0, false},
		{"max-age", []string{"max-age=600"}, 10 * time.Minute, true},
		{"quoted, mixed case, with other directives", []string{`public, Max-Age="120"`}, 2 * time.Minute, true},
		{"the smallest of several", []string{"max-age=900", "max-age=60"}, time.Minute, true},
		{"zero", []string{"max-age=0"}, 0, true},
		{"no-store wins", []string{"max-age=900, no-store"}, 0, true},
		{"no-cache wins", []string{"no-cache", "max-age=900"}, 0, true},
		{"malformed ignored", []string{"max-age=soon, max-age=-5, max-age"}, 0, false},
		{"beyond any integer is malformed, ignored", []string{"max-age=99999999999999999999"}, 0, false},
		{"large but parseable is capped", []string{"max-age=9999999999"}, time.Duration(maxAgeSeconds) * time.Second, true},
		{"s-maxage is not read", []string{"s-maxage=5"}, 0, false},
	}
	for _, tc := range tests {
		h := http.Header{}
		for _, v := range tc.values {
			h.Add("Cache-Control", v)
		}
		got, has := cacheMaxAge(h)
		if got != tc.want || has != tc.has {
			t.Errorf("%s: cacheMaxAge(%q) = (%v, %v), want (%v, %v)", tc.name, tc.values, got, has, tc.want, tc.has)
		}
	}
}
