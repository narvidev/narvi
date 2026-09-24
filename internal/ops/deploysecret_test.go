package ops

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// k8sSecretManifest decodes just enough of deploy/control-plane/secret.yaml
// for TestDeploySecretTemplate's own two checks -- the full Kubernetes
// Secret schema has many more fields, none of which this guard cares
// about.
type k8sSecretManifest struct {
	Kind       string            `yaml:"kind"`
	StringData map[string]string `yaml:"stringData"`
}

// loadDeploySecretTemplate reads and parses deploy/control-plane/
// secret.yaml (root-relative), returning the set of NARVI_-prefixed keys
// its stringData block names.
func loadDeploySecretTemplate(t *testing.T, root string) map[string]bool {
	t.Helper()
	path := filepath.Join(root, "deploy", "control-plane", "secret.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var manifest k8sSecretManifest
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if manifest.Kind != "Secret" {
		t.Fatalf("%s: kind = %q, want %q", path, manifest.Kind, "Secret")
	}
	if len(manifest.StringData) == 0 {
		t.Fatalf("%s: stringData is empty -- almost certainly a fixture or parse bug, not a genuinely empty template", path)
	}

	keys := make(map[string]bool, len(manifest.StringData))
	for k := range manifest.StringData {
		if !strings.HasPrefix(k, "NARVI_") {
			t.Errorf("%s: key %q is not NARVI_-prefixed -- not a platform.Config env var at all", path, k)
			continue
		}
		keys[k] = true
	}
	return keys
}

// TestDeploySecretTemplate is this repository's own drift guard (§41.1: "a
// Secret template naming every required platform.Config variable... and
// nothing else"). deploysecret.go's own RequiredConfigEnvVars parses
// internal/platform/config.go's REAL source -- the ground truth Load
// itself validates against -- for two sets: every NARVI_* variable that
// can trip a "missing" boot failure (required, mechanically derived plus
// the documented deploySecretExtraVars exceptions -- see deploysecret.go's
// own top comment for exactly which two groups those are and why), and
// every NARVI_* variable Load reads at all (read, the superset).
//
// Two failure directions, matching this Step's own exit criterion
// verbatim:
//   - deploy/control-plane/secret.yaml omits a variable platform.Config
//     requires (a real deployment boots, then a feature it turned on
//     refuses at Load with no placeholder in this file to have caught it
//     first) -- missing, below.
//   - deploy/control-plane/secret.yaml names a variable platform.Config
//     no longer reads at all (a stale placeholder, e.g. a renamed or
//     removed env var config.go moved on from) -- extraNotRead, below.
//
// Mutation-verified by hand (see this Step's own PR description for the
// exact commands): deleting a required key from secret.yaml makes this
// test fail naming it under "omits"; adding a bogus
// NARVI_NOT_A_REAL_VAR key makes it fail naming it under "names... no
// longer reads". Both mutations were reverted byte-identical afterward.
func TestDeploySecretTemplate(t *testing.T) {
	root := repoRoot(t)

	required, read, err := RequiredConfigEnvVars(filepath.Join(root, "internal", "platform", "config.go"))
	if err != nil {
		t.Fatalf("RequiredConfigEnvVars: %v", err)
	}
	if len(required) == 0 {
		t.Fatal("RequiredConfigEnvVars found zero required vars -- almost certainly a scan-path bug (platform.Config has real required fields), not a genuinely unconstrained config")
	}

	template := loadDeploySecretTemplate(t, root)

	var missing []string
	for v := range required {
		if !template[v] {
			missing = append(missing, v)
		}
	}
	for _, v := range deploySecretExtraVars {
		if !template[v] {
			missing = append(missing, v+" (documented extra -- deploysecret.go's own deploySecretExtraVars)")
		}
	}
	sort.Strings(missing)

	var extraNotRead []string
	for v := range template {
		if !read[v] {
			extraNotRead = append(extraNotRead, v)
		}
	}
	sort.Strings(extraNotRead)

	if len(missing) > 0 {
		t.Errorf("deploy/control-plane/secret.yaml omits %d variable(s) platform.Config requires:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(extraNotRead) > 0 {
		t.Errorf("deploy/control-plane/secret.yaml names %d variable(s) platform.Config no longer reads at all:\n  %s",
			len(extraNotRead), strings.Join(extraNotRead, "\n  "))
	}
}
