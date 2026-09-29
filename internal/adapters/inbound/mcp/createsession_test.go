package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
)

// createArgs is a narvi_create_session arguments object every field of
// which is set, so a test can tell the DTO the bridge built from it apart
// from the arguments themselves.
const createArgs = `{"title":"Flaky test","prompt":"Fix the flaky test","repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":"main"},{"name":"docs","url":"https://github.com/acme/docs"}],"modelId":"m","effort":"high","planMode":true,"buildModelId":"b","buildEffort":"low","idempotencyKey":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`

// minimalCreateArgs sets only the required fields.
const minimalCreateArgs = `{"prompt":"Fix it","repos":[{"name":"widgets","url":"https://github.com/acme/widgets"}],"idempotencyKey":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`

// seenRequest is what a recording twin saw of the request the bridge
// synthesized.
type seenRequest struct {
	method        string
	path          string
	header        http.Header
	contentLength int64
	body          []byte
}

// recordingCreateTwin answers 201 with a fixed body, recording each request.
func recordingCreateTwin(mu *sync.Mutex, seen *[]seenRequest) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*seen = append(*seen, seenRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), contentLength: r.ContentLength, body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"created"}`))
	}
}

// TestBridge_PostTwinGetsTheDTOBodyAndNoHeader is the bridge's body rule
// (technical plan §43.7): narvi_create_session's twin receives POST
// /api/sessions with, as its body, exactly the JSON encoding of the
// restdtos.CreateSessionRequest BuildRequest built -- spawnSource web, every
// field the tool does not offer at its zero value, an omitted optional as
// null -- never the raw arguments the client sent; its length set; and no
// header at all: no Content-Type, and neither the bearer token nor the
// cookie the MCP request arrived with. The 201 the twin answers is a
// successful result carrying its body.
func TestBridge_PostTwinGetsTheDTOBodyAndNoHeader(t *testing.T) {
	branch := "main"
	title, model, effort, buildModel, buildEffort := "Flaky test", "m", "high", "b", "low"
	full := "Fix the flaky test"
	minimal := "Fix it"
	key := "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"
	tests := []struct {
		name      string
		arguments string
		want      restdtos.CreateSessionRequest
	}{
		{"every field set", createArgs, restdtos.CreateSessionRequest{
			SpawnSource: restdtos.CreateSessionRequestSpawnSourceWeb,
			Title:       &title,
			Prompt:      &full,
			Repos: []restdtos.CreateSessionRequestReposElem{
				{Name: "widgets", Url: "https://github.com/acme/widgets", Branch: &branch},
				{Name: "docs", Url: "https://github.com/acme/docs"},
			},
			ModelId:        &model,
			Effort:         &effort,
			PlanMode:       true,
			BuildModelId:   &buildModel,
			BuildEffort:    &buildEffort,
			IdempotencyKey: &key,
		}},
		{"only the required fields", minimalCreateArgs, restdtos.CreateSessionRequest{
			SpawnSource:    restdtos.CreateSessionRequestSpawnSourceWeb,
			Prompt:         &minimal,
			Repos:          []restdtos.CreateSessionRequestReposElem{{Name: "widgets", Url: "https://github.com/acme/widgets"}},
			IdempotencyKey: &key,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []seenRequest
			twins := testTwins()
			twins.CreateSession = recordingCreateTwin(&mu, &seen)
			handler := newTestHandlerWithGrant(t, twins, scopes("mcp:read", "mcp:write"))
			headers := callToolHeaders("narvi_create_session")
			headers["Authorization"] = "Bearer narvi_mcp_at_must-never-reach-a-twin"
			headers["Cookie"] = "narvi_auth_session=also-never"

			text, isError := postToolCall(t, handlerWithHeaders(handler, headers), "narvi_create_session", tc.arguments)
			if isError || text != `{"id":"created"}` {
				t.Fatalf("result = (IsError %v, %q), want the twin's 201 body as a success", isError, text)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 {
				t.Fatalf("twin called %d times, want 1", len(seen))
			}
			got := seen[0]
			if got.method != http.MethodPost || got.path != "/api/sessions" {
				t.Errorf("twin got %s %s, want POST /api/sessions", got.method, got.path)
			}
			if len(got.header) != 0 {
				t.Errorf("twin received headers %v, want none at all -- not even Content-Type", got.header)
			}
			want, err := json.Marshal(tc.want)
			if err != nil {
				t.Fatal(err)
			}
			if string(got.body) != string(want) {
				t.Errorf("twin body =\n %s\nwant the DTO's own encoding\n %s", got.body, want)
			}
			if string(got.body) == tc.arguments {
				t.Errorf("twin body is the raw arguments")
			}
			if got.contentLength != int64(len(got.body)) {
				t.Errorf("ContentLength = %d, want %d", got.contentLength, len(got.body))
			}
			// The body decodes as the route decodes it.
			var decoded restdtos.CreateSessionRequest
			if err := json.Unmarshal(got.body, &decoded); err != nil {
				t.Errorf("the body does not decode as a CreateSessionRequest: %v", err)
			}
		})
	}
}

// callRequest is a tools/call request for a tool handler called directly.
func callRequest(name, arguments string) *sdkmcp.CallToolRequest {
	return &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: name, Arguments: json.RawMessage(arguments)}}
}

// handlerWithHeaders wraps handler so every request carries headers too --
// postToolCall sends only the tools/call ones.
func handlerWithHeaders(handler http.Handler, headers map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		handler.ServeHTTP(w, r)
	})
}

// TestBridge_GetTwinWithBodyIsADefect: a GET twin handed a body -- a read
// tool whose BuildRequest built one -- is this package's own defect.
// callTwin refuses it with a 500 without invoking the twin, and the tool
// call answers -32603, never the twin's body; a POST twin with the same
// body is invoked (the control).
func TestBridge_GetTwinWithBodyIsADefect(t *testing.T) {
	var calls atomic.Int32
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	logs := swapDefaultLogger(t)

	status, body := callTwin(context.Background(), twin{method: http.MethodGet, pathTemplate: "/api/models", handler: stub}, twinCall{Body: map[string]string{"x": "y"}})
	if status != http.StatusInternalServerError || len(body) != 0 || calls.Load() != 0 {
		t.Fatalf("GET twin with a body: status %d body %q twin calls %d, want 500, no body, not invoked", status, body, calls.Load())
	}
	if !strings.Contains(logs.String(), "a GET twin was handed a request body") {
		t.Errorf("the defect was not logged: %s", logs.String())
	}
	if _, err := mapOutcome(status, body); err == nil {
		t.Errorf("mapOutcome(500) = no error, want -32603")
	}

	status, _ = callTwin(context.Background(), twin{method: http.MethodPost, pathTemplate: "/api/sessions", handler: stub}, twinCall{Body: map[string]string{"x": "y"}})
	if status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("POST twin with a body: status %d twin calls %d, want 200 and invoked once", status, calls.Load())
	}

	// Through a whole tool call: a read tool whose BuildRequest builds a
	// body answers -32603 and never reaches its twin.
	calls.Store(0)
	spec := toolSpecs(Twins{ListModels: stub})[0]
	if spec.Twin.method != http.MethodGet {
		t.Fatalf("toolSpecs(...)[0] = %s over %s, want a GET tool", spec.Name, spec.Twin.method)
	}
	spec.BuildRequest = func(json.RawMessage) (twinCall, error) { return twinCall{Body: "a body"}, nil }
	schemas, err := compileInputSchemas(toolInputDefs())
	if err != nil {
		t.Fatal(err)
	}
	res, err := spec.toolHandler(t.Context(), schemas, allowAllBrake{})(t.Context(), callRequest(spec.Name, `{}`))
	if res != nil || err == nil || calls.Load() != 0 {
		t.Fatalf("read tool building a body: result %+v err %v twin calls %d, want -32603 and no twin call", res, err, calls.Load())
	}
}

// TestCreateSessionTool_SpawnSourceArgumentRefused: the tool's input has
// no spawnSource -- the server records mcp from the grant (technical plan
// §43.1) -- so an argument naming one, whatever its value, is refused by
// argument validation as an unknown field before BuildRequest or the twin
// runs.
func TestCreateSessionTool_SpawnSourceArgumentRefused(t *testing.T) {
	var calls atomic.Int32
	handler := newTestHandler(t, true, true, countingTwins(&calls))
	for _, source := range []string{"web", "mcp", "slack"} {
		t.Run(source, func(t *testing.T) {
			calls.Store(0)
			arguments := strings.TrimSuffix(minimalCreateArgs, "}") + `,"spawnSource":"` + source + `"}`
			text, isError := postToolCall(t, handler, "narvi_create_session", arguments)
			if !isError || text != "invalid arguments: - at '': additional properties 'spawnSource' not allowed" {
				t.Fatalf("result = (IsError %v, %q), want spawnSource refused as an unknown argument", isError, text)
			}
			if calls.Load() != 0 {
				t.Fatalf("twin invoked %d time(s), want 0", calls.Load())
			}
		})
	}
	def, err := inputSchema("CreateSessionToolRequest")
	if err != nil {
		t.Fatal(err)
	}
	if props, _ := def["properties"].(map[string]any); props == nil || props["spawnSource"] != nil || def["additionalProperties"] != false {
		t.Fatalf("CreateSessionToolRequest = %v, want no spawnSource and additionalProperties false", def)
	}
}

// TestCreateSessionTool_BrakeRefusesBeforeTheTwin is the create brake
// (technical plan §43.8): narvi_create_session takes one start from the
// grant's bucket -- keyed by the grant id, nothing else -- after its
// arguments validate and before its twin runs. Past the burst the call
// answers isError with how long to wait, rounded up to a whole second, and
// the twin never runs. Invalid arguments take nothing from the bucket, and
// a read tool never consults it.
func TestCreateSessionTool_BrakeRefusesBeforeTheTwin(t *testing.T) {
	var calls atomic.Int32
	brake := &recordingBrake{allow: 1, retryAfter: 41200 * time.Millisecond}
	handler := newTestHandlerWithGrantAndBrake(t, countingTwins(&calls), scopes("mcp:read", "mcp:write"), brake)

	if text, isError := postToolCall(t, handler, "narvi_create_session", `{"prompt":""}`); !isError || !strings.HasPrefix(text, "invalid arguments: ") {
		t.Fatalf("invalid arguments: (IsError %v, %q), want a validation refusal", isError, text)
	}
	if got := brake.asked(); len(got) != 0 {
		t.Fatalf("invalid arguments consulted the brake %v, want never", got)
	}

	text, isError := postToolCall(t, handler, "narvi_create_session", minimalCreateArgs)
	assertTwinAnswered(t, "narvi_create_session", minimalCreateArgs, calls.Load(), text, isError)

	calls.Store(0)
	text, isError = postToolCall(t, handler, "narvi_create_session", minimalCreateArgs)
	if !isError || text != "too many sessions started through this authorization; retry in 42 s" {
		t.Fatalf("past the burst: (IsError %v, %q), want the brake's refusal", isError, text)
	}
	if calls.Load() != 0 {
		t.Fatalf("past the burst the twin ran %d time(s), want 0", calls.Load())
	}
	if got := brake.asked(); len(got) != 2 || got[0] != testGrantID || got[1] != testGrantID {
		t.Fatalf("brake asked about %v, want the grant id twice", got)
	}

	postToolCall(t, handler, "narvi_list_models", `{}`)
	if got := brake.asked(); len(got) != 2 {
		t.Fatalf("a read tool consulted the create brake: %v", got)
	}
}

// TestNewHandler_RefusesNoCreateBrake: a Config with no CreateBrake builds
// no handler -- a brake every wiring must remember to pass is not a brake.
func TestNewHandler_RefusesNoCreateBrake(t *testing.T) {
	if _, err := NewHandler(Config{PublicBaseURL: testPublicBaseURL}, testTwins()); !errors.Is(err, errNoCreateBrake) {
		t.Fatalf("NewHandler with no CreateBrake = %v, want errNoCreateBrake", err)
	}
}
