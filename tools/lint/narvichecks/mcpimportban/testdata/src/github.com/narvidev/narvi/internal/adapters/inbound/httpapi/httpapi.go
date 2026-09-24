// Package httpapi stands for the real REST handler package -- ALLOWED
// inside internal/adapters/inbound/mcp (the bridge's whole point is to
// invoke an existing httpapi handler in-process).
package httpapi

import "net/http"

func GetModelCatalog() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {}
}
