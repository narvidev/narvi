package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/narvidev/narvi/contracts"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/platform"
)

// rawRestDef independently re-parses contracts.FS's own rest/v1/
// dtos.schema.json (NOT via schemas.go's own loadedRestDefs) so this
// test's own "verbatim" assertions check schemas.go's output against the
// contract file itself, never against schemas.go's own idea of it.
func rawRestDef(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := contracts.FS.ReadFile("rest/v1/dtos.schema.json")
	if err != nil {
		t.Fatalf("read embedded schema: %v", err)
	}
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse embedded schema: %v", err)
	}
	raw, ok := doc.Defs[name]
	if !ok {
		t.Fatalf("no $def named %q", name)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal $def %q: %v", name, err)
	}
	return m
}

// TestToolInputSchemas_ComeFromContracts pins technical plan §43.10: each
// tool's InputSchema is the embedded $defs entry, VERBATIM -- no
// bundling, no rewriting, nothing added or removed.
func TestToolInputSchemas_ComeFromContracts(t *testing.T) {
	for _, spec := range toolSpecs(Twins{}) {
		t.Run(spec.Name, func(t *testing.T) {
			got, err := inputSchema(spec.InputDef)
			if err != nil {
				t.Fatalf("inputSchema(%q) error = %v", spec.InputDef, err)
			}
			want := rawRestDef(t, spec.InputDef)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("inputSchema(%q) = %#v,\nwant (verbatim from contracts) %#v", spec.InputDef, got, want)
			}
		})
	}
}

// TestToolOutputSchemas_ValidateRealBodies pins technical plan §43.10:
// the bundled OutputSchema for each of the three 180 tools is a
// self-contained 2020-12 document that VALIDATES a real encoding of the
// REST DTO it reuses (Session, ListSessionsResponse, ModelCatalog),
// including every transitively-$ref'd $def (Session -> AutomationReposElem;
// ModelCatalog -> ModelCatalogProvider -> ModelCatalogModel ->
// ModelCatalogCost).
func TestToolOutputSchemas_ValidateRealBodies(t *testing.T) {
	sessionBody := `{
		"id": "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a",
		"title": null,
		"status": "created",
		"failureReason": null,
		"archived": false,
		"spawnSource": "web",
		"createdBy": null,
		"createdAt": "2026-01-01T00:00:00Z",
		"updatedAt": "2026-01-01T00:00:00Z",
		"repos": [{"name": "repo", "url": "https://example.com/repo", "branch": null}],
		"sandboxStatus": null,
		"buildModelId": null,
		"buildEffort": null
	}`

	modelCatalogBody := `{
		"providers": [{
			"id": "openai",
			"models": [{
				"id": "gpt-5",
				"name": "GPT-5",
				"contextWindow": 128000,
				"toolCall": true,
				"reasoning": true,
				"variants": ["low", "high"],
				"cost": {"input": 1.5, "output": 3.0, "cacheRead": null, "cacheWrite": null}
			}]
		}]
	}`

	tests := []struct {
		outputDef string
		body      string
	}{
		{"Session", sessionBody},
		{"ListSessionsResponse", `{"sessions": [` + sessionBody + `]}`},
		{"ModelCatalog", modelCatalogBody},
	}

	for _, tt := range tests {
		t.Run(tt.outputDef, func(t *testing.T) {
			bundled, err := bundleOutputSchema(tt.outputDef)
			if err != nil {
				t.Fatalf("bundleOutputSchema(%q) error = %v", tt.outputDef, err)
			}

			bundledJSON, err := json.Marshal(bundled)
			if err != nil {
				t.Fatalf("marshal bundled schema: %v", err)
			}

			c := jsonschema.NewCompiler()
			c.AssertFormat()
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(bundledJSON))
			if err != nil {
				t.Fatalf("decode bundled schema: %v", err)
			}
			const resourceURL = "mem://bundled.json"
			if err := c.AddResource(resourceURL, doc); err != nil {
				t.Fatalf("add bundled schema resource: %v", err)
			}
			sch, err := c.Compile(resourceURL)
			if err != nil {
				t.Fatalf("compile bundled schema: %v", err)
			}

			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(tt.body)))
			if err != nil {
				t.Fatalf("decode instance: %v", err)
			}
			if err := sch.Validate(inst); err != nil {
				t.Errorf("bundled %q schema did not validate a real body: %v\nschema: %s\nbody: %s", tt.outputDef, err, bundledJSON, tt.body)
			}
		})
	}
}

// TestBundleOutputSchema_TransitiveClosure pins the bundler's own
// transitive-closure guarantee explicitly: bundling "ModelCatalog" pulls
// in ModelCatalogProvider, ModelCatalogModel, AND ModelCatalogCost (a
// three-hop chain), not merely its own direct $ref.
func TestBundleOutputSchema_TransitiveClosure(t *testing.T) {
	bundled, err := bundleOutputSchema("ModelCatalog")
	if err != nil {
		t.Fatalf("bundleOutputSchema error = %v", err)
	}
	defs, ok := bundled["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("bundled[$defs] = %#v, want a map", bundled["$defs"])
	}
	for _, want := range []string{"ModelCatalog", "ModelCatalogProvider", "ModelCatalogModel", "ModelCatalogCost"} {
		if _, ok := defs[want]; !ok {
			t.Errorf("bundled $defs missing %q; got keys %v", want, defKeys(defs))
		}
	}
	if len(defs) != 4 {
		t.Errorf("bundled $defs has %d entries (%v), want exactly 4", len(defs), defKeys(defs))
	}
}

func defKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestListModelsOutputSchema_ValidatesRealCatalogBody closes round 2
// review findings N6/N10: TestToolOutputSchemas_ValidateRealBodies above
// validates only a HAND-WRITTEN ModelCatalog body, in which every model
// already carries a non-empty "variants" array -- it cannot notice
// httpapi.GetModelCatalog()'s own nonNilStrings fix (round 1 finding M5)
// ever regressing, since a body that never exercises the empty-variants
// case validates identically whether or not that fix is even present.
// This test instead drives the REAL httpapi.GetModelCatalog() handler --
// the exact function narvi_list_models' own twin invokes -- directly
// (authorize's own check is pure role logic, no store, so no Postgres is
// needed here), and validates the REAL body it writes against the SAME
// bundled schema narvi_list_models advertises as its OutputSchema. The
// real catalog (internal/app/modelcatalog's own compiled-in snapshot)
// has multiple models with zero variants (google's gemini-2.0-flash and
// others), so a regression back to `Variants: m.Variants` (letting a nil
// slice encode as JSON null) fails this test where the hand-written-body
// test above cannot.
func TestListModelsOutputSchema_ValidatesRealCatalogBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	req = req.WithContext(platform.WithUser(req.Context(), testUser))
	rec := httptest.NewRecorder()
	httpapi.GetModelCatalog()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("httpapi.GetModelCatalog(): status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()

	if strings.Contains(string(body), `"variants":null`) {
		t.Errorf("the real model catalog body contains \"variants\":null -- round 1 finding M5's fix (nonNilStrings, httpapi/modelcatalog.go) has regressed:\n%s", body)
	}

	bundled, err := bundleOutputSchema("ModelCatalog")
	if err != nil {
		t.Fatalf("bundleOutputSchema(%q) error = %v", "ModelCatalog", err)
	}
	bundledJSON, err := json.Marshal(bundled)
	if err != nil {
		t.Fatalf("marshal bundled schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(bundledJSON))
	if err != nil {
		t.Fatalf("decode bundled schema: %v", err)
	}
	const resourceURL = "mem://bundled-real-catalog.json"
	if err := c.AddResource(resourceURL, doc); err != nil {
		t.Fatalf("add bundled schema resource: %v", err)
	}
	sch, err := c.Compile(resourceURL)
	if err != nil {
		t.Fatalf("compile bundled schema: %v", err)
	}

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decode real catalog body: %v", err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("the real model catalog body does not validate against narvi_list_models' own advertised outputSchema: %v\nbody: %s", err, body)
	}
}

// TestCompileInputSchemas_RealDefsSucceed and
// TestCompileInputSchemas_UnknownDefFails pin compileInputSchemas' own two
// documented outcomes (schemas.go): every one of toolInputDefs()'s real
// names compiles into a non-nil *jsonschema.Schema, and a name with no
// matching $def in rest/v1/dtos.schema.json returns an error rather than
// panicking or silently omitting an entry -- the exact error NewHandler
// now surfaces as a BOOT failure (round 2 review of PR #324, findings
// N1/N3/N4: a schema defect must fail boot, never a live request).
func TestCompileInputSchemas_RealDefsSucceed(t *testing.T) {
	defs := toolInputDefs()
	schemas, err := compileInputSchemas(defs)
	if err != nil {
		t.Fatalf("compileInputSchemas(%v) error = %v", defs, err)
	}
	if len(schemas) != len(defs) {
		t.Fatalf("len(schemas) = %d, want %d", len(schemas), len(defs))
	}
	for _, name := range defs {
		if schemas[name] == nil {
			t.Errorf("schemas[%q] = nil, want a compiled *jsonschema.Schema", name)
		}
	}
}

func TestCompileInputSchemas_UnknownDefFails(t *testing.T) {
	_, err := compileInputSchemas([]string{"ThisDefDoesNotExist"})
	if err == nil {
		t.Fatal("compileInputSchemas with an unknown $def name: error = nil, want an error")
	}
}
