// capabilities.go stands in for the real GET /api/capabilities handler
// file -- one of the two named httpapi files allowed to import the
// capability registry directly.
package httpapi

import (
	_ "github.com/narvidev/narvi/internal/app/capability"
	_ "github.com/narvidev/narvi/internal/domain/license"
)

func getCapabilities() {}
