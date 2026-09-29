package httpapi

import (
	"encoding/json"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestSessionToDTO_SpawnSourcePassesThrough pins the platform's side of
// Session.spawnSource being an OPEN enum (contracts/manifest.json's
// openEnums): whatever sessions.spawn_source holds is scanned as it is and
// reaches the wire as it is -- including a value this binary has no
// constant for, which an older binary reads during a rolling deploy once a
// newer migration has added it. It is never dropped, rewritten to a known
// source, or turned into an error; tolerating it is the consumer's job.
func TestSessionToDTO_SpawnSourcePassesThrough(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		stored string
	}{
		{name: "a known source", stored: "slack"},
		{name: "a source this binary predates", stored: "a_future_source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var source sqlcgen.SessionSpawnSource
			if err := source.Scan([]byte(tc.stored)); err != nil {
				t.Fatalf("Scan(%q) = %v, want nil", tc.stored, err)
			}

			body, err := json.Marshal(sessionToDTO(sqlcgen.Session{SpawnSource: source, Repos: []byte("[]")}))
			if err != nil {
				t.Fatalf("marshal session DTO: %v", err)
			}
			var wire struct {
				SpawnSource *string `json:"spawnSource"`
			}
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatalf("unmarshal %s: %v", body, err)
			}
			if wire.SpawnSource == nil || *wire.SpawnSource != tc.stored {
				t.Errorf("spawnSource on the wire = %v, want %q (body %s)", wire.SpawnSource, tc.stored, body)
			}
		})
	}
}

// TestSessionToDTO_McpSource pins the REST side of the 'mcp' source
// (technical plan §43.1): a session recorded with it reaches the wire as
// "mcp", never coerced to another source or dropped, and the generated
// contract names it, so a Go consumer decoding into restdtos.Session
// accepts it rather than refusing an unknown enum value.
func TestSessionToDTO_McpSource(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(sessionToDTO(sqlcgen.Session{SpawnSource: sqlcgen.SessionSpawnSourceMcp, Repos: []byte("[]")}))
	if err != nil {
		t.Fatalf("marshal session DTO: %v", err)
	}
	var wire struct {
		SpawnSource *string `json:"spawnSource"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if wire.SpawnSource == nil || *wire.SpawnSource != "mcp" {
		t.Errorf("spawnSource on the wire = %v, want \"mcp\" (body %s)", wire.SpawnSource, body)
	}

	var source restdtos.SessionSpawnSource
	if err := json.Unmarshal([]byte(`"mcp"`), &source); err != nil {
		t.Fatalf("restdtos.SessionSpawnSource refuses \"mcp\": %v", err)
	}
	if source != restdtos.SessionSpawnSourceMcp {
		t.Errorf("decoded %q, want %q", source, restdtos.SessionSpawnSourceMcp)
	}
}
