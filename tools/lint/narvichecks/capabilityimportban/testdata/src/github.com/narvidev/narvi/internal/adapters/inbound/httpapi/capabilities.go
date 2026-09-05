// capabilities.go stands in for the real GET /api/capabilities handler
// file. It used to be one of two files individually allow-listed by name
// to import the capability registry directly -- see this fixture
// package's own scmcredentials.go for why that exemption was removed:
// this file imports nothing banned at all any more, exactly like the
// real one after the fix.
package httpapi

func getCapabilities() {}
