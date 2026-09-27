package mcp

import (
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestResultOutputSchema_HasNoEvents pins "state, summarised result and
// detailed transcript are three separate things" (row 182) for the result:
// narvi_get_session_result's output is SessionOutcome with its own
// sub-shapes and nothing else -- no events, no transcript, no cursor
// anywhere in it -- and its summary is a bounded text, not a list.
func TestResultOutputSchema_HasNoEvents(t *testing.T) {
	result := specNamed(t, "narvi_get_session_result")
	if result.OutputDef != "SessionOutcome" || result.InputDef != "GetSessionResultToolRequest" {
		t.Fatalf("narvi_get_session_result = %s -> %s, want GetSessionResultToolRequest -> SessionOutcome", result.InputDef, result.OutputDef)
	}
	bundle, err := bundleOutputSchema(result.OutputDef)
	if err != nil {
		t.Fatalf("bundleOutputSchema: %v", err)
	}
	defs, _ := bundle["$defs"].(map[string]any)
	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	sort.Strings(names)
	if want := []string{"SessionOutcome", "SessionOutcomeExcludedPullRequest", "SessionOutcomePullRequest", "SessionOutcomeReview", "SessionOutcomeVerdict"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("result output bundles $defs %v, want %v", names, want)
	}
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch v := node.(type) {
		case map[string]any:
			if props, ok := v["properties"].(map[string]any); ok {
				for name, sub := range props {
					switch name {
					case "events", "transcript", "history", "nextCursor":
						t.Errorf("result output schema has a %q property at %s -- the transcript belongs to narvi_get_session_transcript alone", name, path)
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
	for _, name := range names {
		walk("#/$defs/"+name, defs[name])
	}
}

// TestToolCall_GetSessionResult_ReachesTheResultTwin drives the tool
// through the real handler: sessionId becomes the twin's own path
// parameter, no query rides along, the twin's body is the tool's result,
// and a malformed id is refused before the twin runs.
func TestToolCall_GetSessionResult_ReachesTheResultTwin(t *testing.T) {
	const id = "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"
	const body = `{"reviewScope":"none","pullRequests":[]}`
	var seen []string
	twins := testTwins()
	twins.GetSessionResult = func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, chi.URLParam(r, "sessionID")+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
	handler := newTestHandler(t, true, true, twins)

	if text, isError := postToolCall(t, handler, "narvi_get_session_result", `{"sessionId":"`+id+`"}`); isError || text != body {
		t.Fatalf("narvi_get_session_result = (IsError %v, %q), want the twin's body", isError, text)
	}
	if want := []string{id + "?"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("the result twin saw %v, want %v", seen, want)
	}
	if text, isError := postToolCall(t, handler, "narvi_get_session_result", `{"sessionId":"not-a-uuid"}`); !isError || !strings.Contains(text, "uuid") {
		t.Fatalf("malformed id = (IsError %v, %q), want an argument refusal", isError, text)
	}
	if len(seen) != 1 {
		t.Fatalf("the twin ran for a refused argument: %v", seen)
	}
}

// TestGetSessionResultText_SaysWhatCurrentMeans pins the tool's own text on
// what a client most needs not to misread: current only with the code
// host's confirmation in that call, reviewScope none is not a clean
// review, a record naming no pull request is listed apart rather than
// dropped, the summary is bounded and never model-written, the delay hint
// points at the wait, and the result carries no transcript.
func TestGetSessionResultText_SaysWhatCurrentMeans(t *testing.T) {
	spec := specNamed(t, "narvi_get_session_result")
	for _, want := range []string{
		"current only when the code host confirmed, during this call",
		"reviewScope none means there is no pull request to review, which is not a clean review",
		"never written by a model, at most 4,000 characters",
		"not_assessed (the older verdict is in supersededVerdict and is not the answer)",
		"listed apart in excludedPullRequests, with why (shadow_suppressed: the repository was in shadow mode, so no pull request was created",
		"merged or no longer open",
		"suggestedDelaySeconds how long to wait before reading it again: every read asks the code host, so to learn when the session settles, prefer narvi_wait_for_session",
		"Does not include the transcript.",
	} {
		if !strings.Contains(spec.Description, want) {
			t.Errorf("narvi_get_session_result's description does not say %q:\n%s", want, spec.Description)
		}
	}
}
