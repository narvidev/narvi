// scmcredentials.go stands for the real §30.4 sandbox-credential
// suppression path in this same real package -- and doubles as this
// analyzer's own reproduction of the bridge an exit audit of this
// guarantee found. Before the file-level allow-list was removed,
// capabilities.go and requirecapability.go were
// each individually exempted inside this package (httpapi), which is
// otherwise banned like any other: a package-level identifier either one
// declared would have been reachable from THIS file with no import of
// its own, exactly the gap the real scmcredentials.go was used to
// reproduce. This fixture pins the fix directly: an import of a banned
// path in ANY file in this package, including this one, is reported
// exactly like it would be in shadowscm or sessionactor.
package httpapi

import (
	_ "github.com/narvidev/narvi/internal/app/capability" // want `importing "github.com/narvidev/narvi/internal/app/capability" is banned outside the composition root`
)

func scmCredentials() {}
