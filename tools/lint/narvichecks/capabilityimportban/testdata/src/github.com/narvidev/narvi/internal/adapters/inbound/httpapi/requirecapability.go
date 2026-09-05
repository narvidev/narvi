// requirecapability.go stands for the real RequireCapability middleware
// file. It used to be the second of two files individually allow-listed
// by name to import the capability registry directly -- see this fixture
// package's own scmcredentials.go for why that exemption was removed:
// this file imports nothing banned at all any more, exactly like the
// real one after the fix.
package httpapi

func requireCapability() {}
