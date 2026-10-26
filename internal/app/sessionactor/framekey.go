package sessionactor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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
	// snapshotProvenanceKey is snapshot_ready's provenance, read by
	// reportedSnapshotProvenance (snapshotrestore.go).
	snapshotProvenanceKey = "provenance"
	// interruptionKey is execution_complete's interruption (contracts
	// 1.30.0, technical plan §35.4), read by reportedInterruption. The
	// generated type names it as an enum of one value, so a generated
	// decode refuses any value a later agent sends, and the whole frame
	// with it; decodeExecutionComplete decodes without it.
	interruptionKey = "interruption"
)

// frameWithout returns raw without its top-level key, for a decode through
// a generated type that must not see it: every member whose name matches
// it case-insensitively goes, since encoding/json matches a struct field's
// name that way. Every other member is kept as it is in raw, byte for byte
// and in its place, duplicates included, so a decode of the result reads
// what a decode of raw reads for every other field -- encoding/json lets
// the last of several members that name one field win. raw is returned
// unchanged when it has no such member, or is not a single JSON object --
// the decode that follows then fails, or succeeds, exactly as it would
// have.
func frameWithout(raw json.RawMessage, key string) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return raw
	}
	var kept [][]byte
	removed := false
	for dec.More() {
		// A member runs from the end of the one before it (or the opening
		// brace), its separating comma included, to the end of its value.
		start := dec.InputOffset()
		name, err := dec.Token()
		if err != nil {
			return raw
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return raw
		}
		if name, ok := name.(string); ok && strings.EqualFold(name, key) {
			removed = true
			continue
		}
		member := bytes.TrimLeft(raw[start:dec.InputOffset()], " \t\r\n")
		kept = append(kept, bytes.TrimPrefix(member, []byte(",")))
	}
	if !removed {
		return raw
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return raw
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return raw
	}
	stripped := append([]byte("{"), bytes.Join(kept, []byte(","))...)
	return append(stripped, '}')
}

// decodeSnapshotReady decodes a snapshot_ready event through the generated
// type, without its provenance: reportedSnapshotProvenance
// (snapshotrestore.go) reads that on its own, and no value the minting
// agent reported there may cost the snapshot, as none could before the
// generated type named it (technical plan §35.5b). Every other field
// decodes as it always has.
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

// decodeExecutionComplete decodes an execution_complete event through the
// generated type, without its interruption: reportedInterruption reads
// that on its own, and no value an agent reports there -- one a later
// release adds, or one of the wrong type -- may cost the turn its end, as
// none could before the generated type named the key (technical plan
// §35.4). Every other field decodes as it always has.
func decodeExecutionComplete(raw json.RawMessage) (sandboxws.ExecutionComplete, error) {
	var evt sandboxws.ExecutionComplete
	err := json.Unmarshal(frameWithout(raw, interruptionKey), &evt)
	return evt, err
}

// reportedInterruption returns the interruption an execution_complete
// reports, read leniently and on its own: any non-empty JSON string, the
// one value this release knows or a value a later one adds, and false for
// a key that is absent, null, empty or not a string. The key is matched as
// the generated decode matches a field, its name in any case, the last
// such member winning; a frame that does not decode as an object reports
// none. Nothing here reads the frame's reason or diagnostic.
func reportedInterruption(raw json.RawMessage) (string, bool) {
	var frame struct {
		Interruption json.RawMessage `json:"interruption"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		return "", false
	}
	value := reportedString(frame.Interruption)
	if value == nil {
		return "", false
	}
	return *value, true
}
