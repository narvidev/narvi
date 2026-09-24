package mcp

import (
	"net/http"

	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/platform"
)

// buildThroughTheBridge stands for the REAL shape: importing httpapi
// (the bridge's own twin handlers) and platform (context helpers) --
// both ALLOWED, must never be reported.
func buildThroughTheBridge() http.HandlerFunc {
	_, _ = platform.UserFromContext(nil)
	return httpapi.GetModelCatalog()
}
