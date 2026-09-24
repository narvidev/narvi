package compat

import (
	"bytes"
	"encoding/json"
	"fmt"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// CompileCheck compiles a schema file's root and every one of its own
// top-level $defs entries with santhosh-tekuri/jsonschema (format
// assertions on, mirroring contracts/contractstest/helpers_test.go's own
// newCompiler) -- part of the vacuous-pass guard "parse/compile failure
// either side is a hard error" (§6.3 design spec §3, guard 4). This is
// deliberately independent of walkSchema's own allowlist pass: a schema
// can satisfy the allowlist's keyword/shape rules yet still be an invalid
// JSON Schema in some other way (a $ref this checker's own
// localRefTarget accepts but jsonschema itself rejects, for instance) --
// running the real compiler is what catches that.
func CompileCheck(data []byte) error {
	var probe struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("probe $id: %w", err)
	}
	if probe.ID == "" {
		return fmt.Errorf("schema has no $id")
	}

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("decode schema: %w", err)
	}

	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(probe.ID, doc); err != nil {
		return fmt.Errorf("add resource: %w", err)
	}
	if _, err := c.Compile(probe.ID); err != nil {
		return fmt.Errorf("compile root: %w", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		return fmt.Errorf("decode generic: %w", err)
	}
	if defs, ok := generic["$defs"].(map[string]any); ok {
		for name := range defs {
			frag := probe.ID + "#/$defs/" + name
			if _, err := c.Compile(frag); err != nil {
				return fmt.Errorf("compile %s: %w", frag, err)
			}
		}
	}
	return nil
}
