package boot

import (
	"os"
	"testing"
)

// TestMain lets this package's test binary answer the hard-link probe,
// which re-executes /proc/self/exe as the runtime: without it, that child
// would run the whole test suite instead, and the probe run by a walk as
// root on Linux would always be inconclusive.
func TestMain(m *testing.M) {
	RunHardlinkProbeIfRequested(os.Args)
	os.Exit(m.Run())
}
