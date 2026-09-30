//go:build integration

package controlplane

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/platform"
)

// imageMajor reads the Postgres major version an image's tag names:
// postgres:16-alpine names 16. A tag that names none (latest, or none at
// all) reports false.
func imageMajor(image string) (int, bool) {
	i := strings.LastIndex(image, ":")
	if i < 0 {
		return 0, false
	}
	tag := image[i+1:]
	end := 0
	for end < len(tag) && tag[end] >= '0' && tag[end] <= '9' {
		end++
	}
	major, err := strconv.Atoi(tag[:end])
	return major, err == nil
}

// TestPostgresPreflight_RealServer runs boot's reads of the Postgres server
// against a real one, in serve's order: the version check accepts it, every
// embedded migration applies, and the connection budget reads all three of
// its settings. It runs on postgres:17-alpine by default, and on the floor
// under `make test-integration-postgres-floor`; it first confirms that the
// server it reached is the major version the image named, so a run meant
// for the floor cannot pass on another version unnoticed.
func TestPostgresPreflight_RealServer(t *testing.T) {
	pool, connStr := newUnmigratedTestPool(t)
	ctx := t.Context()
	timeouts := platform.DefaultTimeouts()

	var num int
	if err := pool.QueryRow(ctx, serverVersionNumQuery).Scan(&num); err != nil {
		t.Fatalf("read server_version_num: %v", err)
	}
	image := testPostgresImage()
	t.Logf("Postgres server %s, from image %s", postgresVersion(num), image)
	if major, ok := imageMajor(image); ok && num/10000 != major {
		t.Fatalf("the server runs Postgres %s, but its image %q names %d", postgresVersion(num), image, major)
	}

	if err := requireSupportedPostgres(ctx, pool, timeouts.PostgresVersionCheckTimeout); err != nil {
		t.Fatalf("requireSupportedPostgres on Postgres %s: %v", postgresVersion(num), err)
	}
	if err := applyMigrations(connStr); err != nil {
		t.Fatalf("applyMigrations on Postgres %s: %v", postgresVersion(num), err)
	}

	var logs bytes.Buffer
	checkConnectionBudget(ctx, slog.New(slog.NewJSONHandler(&logs, nil)), pool, timeouts.HealthCheckTimeout)
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("checkConnectionBudget logged %q, want one JSON line: %v", logs.String(), err)
	}
	msg, _ := entry["msg"].(string)
	if !strings.Contains(msg, "postgres connection budget") && !strings.Contains(msg, "needs more postgres connections") {
		t.Fatalf("checkConnectionBudget logged %q, want the budget read from the server", msg)
	}
	if _, ok := entry["reserved_connections"].(float64); !ok {
		t.Fatalf("checkConnectionBudget logged %v, want reserved_connections read from the server", entry)
	}
}
