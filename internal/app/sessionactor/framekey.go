package sessionactor

import (
	"encoding/json"
	"strings"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// Keys a sandbox-agent added to a frame in contracts 1.25.0 that no decode
// through the generated sandboxws types may see (technical plan §35.2,
// §35.5b). Before that release the generated types did not name them, so
// whatever they held was ignored; a generated decode that sees one now
// refuses a value of the wrong type -- a lifetimeRemainingSeconds of 60.0
// or past an int, a provenance that is not an object -- and fails the
// whole frame, which costs a ready its capabilities and a snapshot_ready
// its snapshot. decodeReady and decodeSnapshotReady decode without them.
const (
	// lifetimeReportKey is ready's and heartbeat's lifetimeRemainingSeconds,
	// read by reportedLifetimeRemaining (lifetimereport.go).
	lifetimeReportKey = "lifetimeRemainingSeconds"
	// snapshotProvenanceKey is snapshot_ready's provenance, which nothing
	// in this package reads yet.
	snapshotProvenanceKey = "provenance"
)

// frameWithout returns raw without its top-level key, for a decode through
// a generated type that must not see it: every key that matches it
// case-insensitively goes, since encoding/json matches a struct field's
// name that way. raw is returned unchanged when it has no such key, or is
// not a JSON object -- the decode that follows then fails, or succeeds,
// exactly as it would have.
func frameWithout(raw json.RawMessage, key string) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	removed := false
	for name := range fields {
		if strings.EqualFold(name, key) {
			delete(fields, name)
			removed = true
		}
	}
	if !removed {
		return raw
	}
	stripped, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return stripped
}

// decodeSnapshotReady decodes a snapshot_ready event through the generated
// type, without its provenance: nothing in this package reads it yet, and
// no value the minting agent reported there may cost the snapshot, as none
// could before the generated type named it (technical plan §35.5b). Every
// other field decodes as it always has.
func decodeSnapshotReady(raw json.RawMessage) (sandboxws.SnapshotReady, error) {
	var evt sandboxws.SnapshotReady
	err := json.Unmarshal(frameWithout(raw, snapshotProvenanceKey), &evt)
	return evt, err
}

// decodeReady decodes a ready event through the generated type, without
// its lifetimeRemainingSeconds: technical plan §35.2's report is read on
// its own (reportedLifetimeRemaining, lifetimereport.go), and no value it
// holds may cost the ready its capabilities (promptreceipt.go), as none
// could before the generated type named it. Every other field decodes as
// it always has.
func decodeReady(raw json.RawMessage) (sandboxws.Ready, error) {
	var evt sandboxws.Ready
	err := json.Unmarshal(frameWithout(raw, lifetimeReportKey), &evt)
	return evt, err
}
