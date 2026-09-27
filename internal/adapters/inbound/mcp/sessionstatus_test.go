package mcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// specNamed returns the table's own spec for name.
func specNamed(t *testing.T, name string) toolSpec {
	t.Helper()
	for _, spec := range toolSpecs(Twins{}) {
		if spec.Name == name {
			return spec
		}
	}
	t.Fatalf("no tool %q in the table", name)
	return toolSpec{}
}

// TestStatusOutputSchema_HasNoEvents pins "state and transcript are
// separate reads" (technical plan §43.20) on the wire contract itself:
// narvi_get_session_status' output schema is SessionActivity alone -- it
// bundles no other $def, so no EventsResponse, and no property anywhere in
// it is an events list or a transcript -- while the transcript tool's
// output is EventsResponse. A status shape that grew an events field
// would fail here (and the golden would drift).
func TestStatusOutputSchema_HasNoEvents(t *testing.T) {
	status := specNamed(t, "narvi_get_session_status")
	if status.OutputDef != "SessionActivity" {
		t.Fatalf("narvi_get_session_status OutputDef = %q, want SessionActivity", status.OutputDef)
	}
	bundle, err := bundleOutputSchema(status.OutputDef)
	if err != nil {
		t.Fatalf("bundleOutputSchema: %v", err)
	}
	defs, _ := bundle["$defs"].(map[string]any)
	if len(defs) != 1 || defs["SessionActivity"] == nil {
		names := make([]string, 0, len(defs))
		for n := range defs {
			names = append(names, n)
		}
		t.Fatalf("status output bundles $defs %v, want SessionActivity alone", names)
	}
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch v := node.(type) {
		case map[string]any:
			if props, ok := v["properties"].(map[string]any); ok {
				for name, sub := range props {
					switch name {
					case "events", "transcript", "history", "nextCursor":
						t.Errorf("status output schema has a %q property at %s -- the transcript belongs to narvi_get_session_transcript alone", name, path)
					}
					walk(path+"/properties/"+name, sub)
				}
			}
			for k, sub := range v {
				if k != "properties" {
					walk(path+"/"+k, sub)
				}
			}
		case []any:
			for _, sub := range v {
				walk(path, sub)
			}
		}
	}
	walk("#", defs["SessionActivity"])

	if transcript := specNamed(t, "narvi_get_session_transcript"); transcript.OutputDef != "EventsResponse" {
		t.Fatalf("narvi_get_session_transcript OutputDef = %q, want EventsResponse", transcript.OutputDef)
	}
}

// TestBuildGetSessionTranscriptRequest_Table pins how the transcript
// tool's arguments reach GET /api/sessions/{sessionID}/events: the
// session id as the chi param, and cursor/limit as query keys only when
// set, so the twin's own defaults run otherwise.
func TestBuildGetSessionTranscriptRequest_Table(t *testing.T) {
	const id = "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"
	tests := []struct {
		name      string
		arguments string
		wantQuery url.Values
		wantErr   bool
	}{
		{"session only: the twin's defaults", `{"sessionId":"` + id + `"}`, url.Values{}, false},
		{"a cursor, verbatim", `{"sessionId":"` + id + `","cursor":"9223372036854775807"}`, url.Values{"cursor": {"9223372036854775807"}}, false},
		{"cursor zero", `{"sessionId":"` + id + `","cursor":"0"}`, url.Values{"cursor": {"0"}}, false},
		{"a limit", `{"sessionId":"` + id + `","limit":250}`, url.Values{"limit": {"250"}}, false},
		{"a limit spelled with an exponent", `{"sessionId":"` + id + `","limit":1e2}`, url.Values{"limit": {"100"}}, false},
		{"a limit spelled with a zero fraction", `{"sessionId":"` + id + `","limit":7.0}`, url.Values{"limit": {"7"}}, false},
		{"both", `{"sessionId":"` + id + `","cursor":"12","limit":3}`, url.Values{"cursor": {"12"}, "limit": {"3"}}, false},
		{"a limit beyond int64 is an ordinary refusal", `{"sessionId":"` + id + `","limit":1e19}`, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params, query, err := buildGetSessionTranscriptRequest(json.RawMessage(tc.arguments))
			if tc.wantErr {
				var iae *invalidArgumentError
				if !errors.As(err, &iae) {
					t.Fatalf("err = %v, want an *invalidArgumentError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(params, map[string]string{"sessionID": id}) {
				t.Errorf("urlParams = %v, want sessionID only", params)
			}
			if !reflect.DeepEqual(query, tc.wantQuery) {
				t.Errorf("query = %v, want %v", query, tc.wantQuery)
			}
		})
	}
}

// TestToolCall_SessionStatusAndTranscript_ReachTheirTwins drives both
// tools through the full handler: each invokes its own twin with the
// session id as chi's URL param, and the transcript's cursor/limit arrive
// as the twin's own query string -- or none at all when omitted.
func TestToolCall_SessionStatusAndTranscript_ReachTheirTwins(t *testing.T) {
	const id = "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"
	type seen struct {
		param, query string
	}
	var status, transcript []seen
	twins := testTwins()
	twins.GetSessionStatus = func(w http.ResponseWriter, r *http.Request) {
		status = append(status, seen{chi.URLParam(r, "sessionID"), r.URL.RawQuery})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"activity":"queued"}`))
	}
	twins.ListEvents = func(w http.ResponseWriter, r *http.Request) {
		transcript = append(transcript, seen{chi.URLParam(r, "sessionID"), r.URL.RawQuery})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[],"nextCursor":null}`))
	}
	handler := newTestHandler(t, true, true, twins)

	for _, call := range []struct{ tool, args, want string }{
		{"narvi_get_session_status", `{"sessionId":"` + id + `"}`, `{"activity":"queued"}`},
		{"narvi_get_session_transcript", `{"sessionId":"` + id + `"}`, `{"events":[],"nextCursor":null}`},
		{"narvi_get_session_transcript", `{"sessionId":"` + id + `","cursor":"41","limit":2}`, `{"events":[],"nextCursor":null}`},
	} {
		text, isError := postToolCall(t, handler, call.tool, call.args)
		if isError || text != call.want {
			t.Fatalf("%s %s = (IsError %v, %q), want %q", call.tool, call.args, isError, text, call.want)
		}
	}
	if want := []seen{{id, ""}}; !reflect.DeepEqual(status, want) {
		t.Errorf("status twin saw %+v, want %+v", status, want)
	}
	sort.Slice(transcript, func(i, j int) bool { return transcript[i].query < transcript[j].query })
	if want := []seen{{id, ""}, {id, "cursor=41&limit=2"}}; !reflect.DeepEqual(transcript, want) {
		t.Errorf("transcript twin saw %+v, want %+v", transcript, want)
	}
}

// TestBuildWaitForSessionRequest_Table pins how narvi_wait_for_session's
// arguments reach GET /api/sessions/{sessionID}/status: the session id as
// the chi param, and waitSeconds ALWAYS on the query -- so the twin tells
// the wait from a plain read -- the caller's value in any integer spelling
// the schema accepts, or the largest the route parses (which it clamps to
// the deployment's maximum) when omitted or past int64: a large wait is
// clamped, never an argument error.
func TestBuildWaitForSessionRequest_Table(t *testing.T) {
	const id = "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"
	const longest = "9223372036854775807"
	tests := []struct {
		name      string
		arguments string
		want      string
	}{
		{"omitted: the longest wait", `{"sessionId":"` + id + `"}`, longest},
		{"one second", `{"sessionId":"` + id + `","waitSeconds":1}`, "1"},
		{"the shipped maximum", `{"sessionId":"` + id + `","waitSeconds":25}`, "25"},
		{"past the maximum, left to the twin's clamp", `{"sessionId":"` + id + `","waitSeconds":600}`, "600"},
		{"an exponent", `{"sessionId":"` + id + `","waitSeconds":1e1}`, "10"},
		{"a zero fraction", `{"sessionId":"` + id + `","waitSeconds":3.0}`, "3"},
		{"past int64: the longest wait, not a refusal", `{"sessionId":"` + id + `","waitSeconds":1e30}`, longest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params, query, err := buildWaitForSessionRequest(json.RawMessage(tc.arguments))
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(params, map[string]string{"sessionID": id}) {
				t.Errorf("urlParams = %v, want sessionID only", params)
			}
			if want := (url.Values{"waitSeconds": {tc.want}}); !reflect.DeepEqual(query, want) {
				t.Errorf("query = %v, want %v", query, want)
			}
		})
	}
}

// TestToolCall_WaitForSession_ReachesTheStatusTwin drives the wait through
// the full handler: it invokes the SAME twin as narvi_get_session_status
// -- the status route -- with ?waitSeconds= set, the status tool with no
// query at all, and the twin's body comes back verbatim, wait object
// included; a waitSeconds below one is refused before the twin runs.
func TestToolCall_WaitForSession_ReachesTheStatusTwin(t *testing.T) {
	const id = "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"
	const body = `{"activity":"finished","wait":{"reason":"settled","waitedMs":1200}}`
	var seen []string
	twins := testTwins()
	twins.GetSessionStatus = func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, chi.URLParam(r, "sessionID")+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
	handler := newTestHandler(t, true, true, twins)

	for _, call := range []struct{ tool, args string }{
		{"narvi_wait_for_session", `{"sessionId":"` + id + `","waitSeconds":7}`},
		{"narvi_wait_for_session", `{"sessionId":"` + id + `"}`},
		{"narvi_get_session_status", `{"sessionId":"` + id + `"}`},
	} {
		if text, isError := postToolCall(t, handler, call.tool, call.args); isError || text != body {
			t.Fatalf("%s %s = (IsError %v, %q), want the twin's body", call.tool, call.args, isError, text)
		}
	}
	want := []string{id + "?waitSeconds=7", id + "?waitSeconds=9223372036854775807", id + "?"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("the status twin saw %v, want %v", seen, want)
	}
	if text, isError := postToolCall(t, handler, "narvi_wait_for_session", `{"sessionId":"`+id+`","waitSeconds":0}`); !isError || !strings.Contains(text, "minimum") {
		t.Fatalf("waitSeconds 0 = (IsError %v, %q), want an argument refusal naming the minimum", isError, text)
	}
	if len(seen) != 3 {
		t.Fatalf("the twin ran for a refused argument: %v", seen)
	}
}
