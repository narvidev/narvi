package controlplane

import "github.com/narvidev/narvi/internal/adapters/outbound/cimdfetch"

// cimdFetchGuard configures the SSRF guard every client ID metadata
// document fetch dials through (technical plan §43.15; Build hands it to
// cimdfetch.NewGuardedClient). Production never changes it: the zero value
// is the full guard -- the system resolver and root CAs, every
// non-public address refused. It is a package variable, not a Build
// parameter or a Config field, precisely so nothing a deployment
// configures can reach it: this package's own integration test is its one
// writer (setting, and restoring, the seams that let an in-test HTTPS
// server on loopback be fetched, which the production guard must refuse
// -- and is proven to).
var cimdFetchGuard cimdfetch.GuardConfig
