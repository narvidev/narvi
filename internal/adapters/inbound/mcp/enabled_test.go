package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireEnabled_False503(t *testing.T) {
	var called bool
	h := RequireEnabled(false)(passthroughHandler(&called))

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if called {
		t.Fatal("RequireEnabled(false) called next")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := rec.Body.String(); got != disabledBody {
		t.Errorf("body = %q, want %q", got, disabledBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestRequireEnabled_TruePassesThrough(t *testing.T) {
	var called bool
	h := RequireEnabled(true)(passthroughHandler(&called))

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatal("RequireEnabled(true) did not call next")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want the passthrough sentinel %d", rec.Code, http.StatusTeapot)
	}
}
