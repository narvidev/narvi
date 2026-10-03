package wsbridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf8"
)

// ErrFrameTooLarge is returned by SendCritical for a critical event over
// platform.DefaultFrameReadLimitBytes, and by SendBestEffort for a
// best-effort event over platform.MaxEventFrameBytes that no cut brings
// under it. Neither is buffered, so neither is ever sent; a warning naming
// the frame goes out in its place (doc.go, "What a connection writes").
var ErrFrameTooLarge = errors.New("wsbridge: event frame too large to send")

// cutMarkerFormat is the line a cut string ends with, for people; machines
// read the frame's `cut` property instead (technical plan §6.1). It starts
// with a newline so the marker is a line of its own.
const cutMarkerFormat = "\n[text cut at %d of %d bytes on its way from the sandbox]"

// cutMarker is the marker for a string cut to kept of its total bytes.
func cutMarker(kept, total int) string {
	return fmt.Sprintf(cutMarkerFormat, kept, total)
}

// Fit returns payload, one encoded agent event, as it is written on a
// connection whose peer reads at most bound bytes: unchanged when it fits
// (len(payload) <= bound, inclusive, as the WebSocket library reads a
// message of exactly its limit), and otherwise cut to fit. ok is false
// when it cannot be made to fit: the frame is then not written on that
// connection (doc.go, "What a connection writes").
//
// Only a `token`, a `tool_call` or a `tool_result` is cut, and the cut
// shortens one string: a token's text, the longest string anywhere under a
// tool_result's output, the longest string anywhere under a tool_call's
// input -- longest in raw bytes, ties to the first in traversal order
// (object keys ascending, arrays by index). It keeps the longest prefix,
// ending at a UTF-8 boundary, that leaves room for the marker line
// (cutMarker) and for the `cut` property, {kept, total}, set at the
// frame's top level: kept and total are the bytes of that string kept and
// its whole length, both counted unescaped. cutPath is the path of the
// string an earlier cut shortened (the enqueue cap, SendBestEffort), nil
// for a frame never cut: that same string is cut again then, from what the
// earlier cut kept, and keeps its total, so every cut of one frame names
// the text it came from. path is the cut string's path when a cut was
// made, cutPath otherwise.
//
// The frame is decoded keeping its numbers as written, and re-encoded with
// its object keys sorted, so a cut is deterministic: the same frame and
// bound always give the same bytes, and a cut frame replayed twice dedupes
// on the control plane's storage key.
func Fit(payload []byte, bound int, cutPath []string) (fitted []byte, path []string, ok bool) {
	if len(payload) <= bound {
		return payload, cutPath, true
	}

	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var frame map[string]any
	if err := dec.Decode(&frame); err != nil {
		return nil, nil, false
	}
	frameType, _ := frame["type"].(string)
	var targetRoot string
	switch frameType {
	case "token":
		targetRoot = "text"
	case "tool_call":
		targetRoot = "input"
	case "tool_result":
		targetRoot = "output"
	default:
		return nil, nil, false
	}

	// A frame written by another encoder may encode longer than this one
	// does it; re-encoded, it may fit whole.
	if whole, err := json.Marshal(frame); err == nil && len(whole) <= bound {
		return whole, cutPath, true
	}

	var original string
	var total, keptBefore int
	if len(cutPath) > 0 {
		s, found := stringAt(frame, cutPath)
		prev, cutOK := frameCut(frame)
		if !found || !cutOK || prev.kept > len(s) {
			return nil, nil, false
		}
		path = cutPath
		original, total, keptBefore = s[:prev.kept], prev.total, prev.kept
	} else {
		var found bool
		if frameType == "token" {
			var s string
			s, found = frame[targetRoot].(string)
			path, original = []string{targetRoot}, s
		} else {
			path, original, found = longestString(frame[targetRoot], []string{targetRoot})
		}
		if !found {
			return nil, nil, false
		}
		total, keptBefore = len(original), len(original)
	}

	// The frame's bytes with the string emptied and the widest cut it can
	// carry -- kept is at most total, so has at most its digits.
	if !setStringAt(frame, path, "") {
		return nil, nil, false
	}
	frame["cut"] = map[string]any{"kept": total, "total": total}
	over, err := json.Marshal(frame)
	if err != nil {
		return nil, nil, false
	}
	budget := bound - len(over) - jsonEscapedLen(cutMarker(total, total))
	if budget < 0 {
		return nil, nil, false
	}

	kept := keptPrefixLen(original, budget)
	kept = min(kept, keptBefore)
	if kept >= total {
		return nil, nil, false
	}

	setStringAt(frame, path, original[:kept]+cutMarker(kept, total))
	frame["cut"] = map[string]any{"kept": kept, "total": total}
	out, err := json.Marshal(frame)
	if err != nil || len(out) > bound {
		return nil, nil, false
	}
	return out, path, true
}

// keptPrefixLen returns the length of the longest prefix of s that ends at
// a UTF-8 boundary and encodes, inside a JSON string, to at most budget
// bytes.
func keptPrefixLen(s string, budget int) int {
	kept, used := 0, 0
	for kept < len(s) {
		r, size := utf8.DecodeRuneInString(s[kept:])
		w := escapedRuneLen(r, size)
		if used+w > budget {
			break
		}
		used += w
		kept += size
	}
	return kept
}

// jsonEscapedLen returns how many bytes s takes inside a JSON string as
// encoding/json writes it, with HTML escaping on (json.Marshal): the
// quotes excluded. TestJSONEscapedLen_MatchesEncodingJSON and
// FuzzJSONEscapedLen hold it to len(json.Marshal(s))-2.
func jsonEscapedLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		n += escapedRuneLen(r, size)
		i += size
	}
	return n
}

// escapedRuneLen is the encoded length of one rune of a JSON string, size
// the bytes it took in the string: a two-byte escape for '"', '\\' and the
// control characters that have one; a six-byte \u escape for every other
// control character, for '<', '>' and '&' (HTML escaping), for U+2028 and
// U+2029, and for each byte of invalid UTF-8; otherwise its own bytes.
func escapedRuneLen(r rune, size int) int {
	switch {
	case r == utf8.RuneError && size == 1:
		return 6
	case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t' || r == '\b' || r == '\f':
		return 2
	case r < 0x20 || r == '<' || r == '>' || r == '&' || r == '\u2028' || r == '\u2029':
		return 6
	default:
		return size
	}
}

// longestString returns the path, under prefix, of the longest string in
// v and that string: by raw byte length, ties to the first met, object
// keys visited in ascending order and arrays by index. found is false when
// v holds no string.
func longestString(v any, prefix []string) (path []string, s string, found bool) {
	switch t := v.(type) {
	case string:
		return append([]string(nil), prefix...), t, true
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p, ks, ok := longestString(t[k], append(prefix, k)); ok && (!found || len(ks) > len(s)) {
				path, s, found = p, ks, true
			}
		}
	case []any:
		for i, e := range t {
			if p, es, ok := longestString(e, append(prefix, strconv.Itoa(i))); ok && (!found || len(es) > len(s)) {
				path, s, found = p, es, true
			}
		}
	}
	return path, s, found
}

// stringAt returns the string at path in frame, an array element named by
// its decimal index.
func stringAt(frame map[string]any, path []string) (string, bool) {
	var v any = frame
	for _, step := range path {
		next, ok := child(v, step)
		if !ok {
			return "", false
		}
		v = next
	}
	s, ok := v.(string)
	return s, ok
}

// setStringAt replaces the string at path in frame with s, reporting
// whether a string was there to replace.
func setStringAt(frame map[string]any, path []string, s string) bool {
	if len(path) == 0 {
		return false
	}
	var parent any = frame
	for _, step := range path[:len(path)-1] {
		next, ok := child(parent, step)
		if !ok {
			return false
		}
		parent = next
	}
	last := path[len(path)-1]
	switch p := parent.(type) {
	case map[string]any:
		if _, ok := p[last].(string); !ok {
			return false
		}
		p[last] = s
		return true
	case []any:
		i, err := strconv.Atoi(last)
		if err != nil || i < 0 || i >= len(p) {
			return false
		}
		if _, ok := p[i].(string); !ok {
			return false
		}
		p[i] = s
		return true
	default:
		return false
	}
}

// child returns the member step of v, an object's key or an array's
// decimal index.
func child(v any, step string) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		c, ok := t[step]
		return c, ok
	case []any:
		i, err := strconv.Atoi(step)
		if err != nil || i < 0 || i >= len(t) {
			return nil, false
		}
		return t[i], true
	default:
		return nil, false
	}
}

// recordedCut is a frame's own `cut` property, as Fit wrote it.
type recordedCut struct{ kept, total int }

// frameCut reads the `cut` an earlier Fit set at frame's top level.
func frameCut(frame map[string]any) (recordedCut, bool) {
	obj, ok := frame["cut"].(map[string]any)
	if !ok {
		return recordedCut{}, false
	}
	kept, keptOK := jsonInt(obj["kept"])
	total, totalOK := jsonInt(obj["total"])
	if !keptOK || !totalOK || kept < 0 || kept >= total {
		return recordedCut{}, false
	}
	return recordedCut{kept: kept, total: total}, true
}

// jsonInt reads v, a number decoded with UseNumber, as an int.
func jsonInt(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(n.String())
	return i, err == nil
}

// criticalTextMaxBytes bounds the free text a critical event carries from
// outside this process -- today push_error's error, which carries git
// push's stderr (cmd/sandbox-agent's sendPushError). 4096 bytes keep the
// head of the text, as capDiagnosticField keeps a diagnostic field's
// (internal/adapters/outbound/opencode), and hold the frame under
// platform.DefaultFrameReadLimitBytes even with every byte of the text
// six-fold escaped (4096 x 6 = 24 KiB, plus the frame's own fields), so a
// critical frame carrying it is never refused (SendCritical) and every
// control plane reads it.
const criticalTextMaxBytes = 4096

// criticalTextTruncationMarker ends a text CapCriticalText shortened, as
// capDiagnosticField's marker does, so a shortened text never reads as
// whole.
const criticalTextTruncationMarker = "...[truncated]"

// CapCriticalText returns s, shortened to at most criticalTextMaxBytes
// when it is longer: its head, cut back to a UTF-8 boundary, then
// criticalTextTruncationMarker.
func CapCriticalText(s string) string {
	if len(s) <= criticalTextMaxBytes {
		return s
	}
	limit := criticalTextMaxBytes - len(criticalTextTruncationMarker)
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + criticalTextTruncationMarker
}

// frameIdentity peeks a frame's type and messageId, for the warning that
// names a frame not written.
func frameIdentity(payload []byte) (frameType, messageID string) {
	var env struct {
		Type      string `json:"type"`
		MessageID string `json:"messageId"`
	}
	_ = json.Unmarshal(payload, &env)
	return env.Type, env.MessageID
}

// frameNotWrittenMessage is the warning's text for a frame not written:
// its type, messageId and size, and the bound it is over.
func frameNotWrittenMessage(payload []byte, bound int) string {
	frameType, messageID := frameIdentity(payload)
	return fmt.Sprintf("sandbox-agent: a %s event (messageId %q) of %d bytes was not sent: it is over the %d bytes the control plane reads, and no cut brings it under",
		frameType, messageID, len(payload), bound)
}
