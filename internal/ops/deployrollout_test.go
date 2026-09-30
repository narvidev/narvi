package ops

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/narvidev/narvi/internal/platform"
)

// kubernetesDefaultProgressDeadline is a Deployment's progressDeadlineSeconds
// when the manifest sets none: 600 seconds.
const kubernetesDefaultProgressDeadline = 600 * time.Second

// defaultRolloutMaxWaves is the most waves a RollingUpdate at Kubernetes'
// default maxSurge (25%, rounded up) and maxUnavailable (25%, rounded down)
// replaces a Deployment in, at any replica count: see
// defaultRolloutWaves.
const defaultRolloutMaxWaves = 3

// defaultRolloutWaves is how many waves a RollingUpdate at Kubernetes'
// default percentages replaces replicas pods in: each wave replaces at most
// maxSurge + maxUnavailable of them.
func defaultRolloutWaves(replicas int) int {
	surge := (replicas + 3) / 4 // 25%, rounded up
	unavailable := replicas / 4 // 25%, rounded down
	perWave := surge + unavailable
	return (replicas + perWave - 1) / perWave
}

// TestDeployment_KeepsTheRolloutDefaultsRollingDeployCeilingRestsOn pins
// what platform.Timeouts.RollingDeployCeiling is derived from -- the bound
// a timer kind this binary does not know is kept at the claim cadence
// beyond (UnknownTimerGrace, technical plan §2). The control plane's
// Deployment (deploy/control-plane/deployment.yaml) sets neither a strategy
// nor progressDeadlineSeconds, so Kubernetes' defaults apply: a
// RollingUpdate at 25% surge and 25% unavailable, which replaces a
// Deployment of any size in at most three waves, each within a 600-second
// progress deadline. A manifest that sets either of them fails here, so the
// ceiling is revisited with it rather than silently outgrown.
func TestDeployment_KeepsTheRolloutDefaultsRollingDeployCeilingRestsOn(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "control-plane", "deployment.yaml"))
	if err != nil {
		t.Fatalf("read deployment.yaml: %v", err)
	}
	var manifest struct {
		Kind string         `yaml:"kind"`
		Spec map[string]any `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse deployment.yaml: %v", err)
	}
	if manifest.Kind != "Deployment" {
		t.Fatalf("deployment.yaml kind = %q, want Deployment", manifest.Kind)
	}
	for _, key := range []string{"strategy", "progressDeadlineSeconds"} {
		if _, set := manifest.Spec[key]; set {
			t.Errorf("deployment.yaml sets spec.%s: platform.Timeouts.RollingDeployCeiling is derived from Kubernetes' default for it -- recompute the ceiling from the new value, then update this test", key)
		}
	}

	maxWaves := 0
	for replicas := 1; replicas <= 1000; replicas++ {
		maxWaves = max(maxWaves, defaultRolloutWaves(replicas))
	}
	if maxWaves != defaultRolloutMaxWaves {
		t.Fatalf("the default rollout takes up to %d waves over 1..1000 replicas, want %d", maxWaves, defaultRolloutMaxWaves)
	}
	if got, floor := platform.DefaultTimeouts().RollingDeployCeiling, defaultRolloutMaxWaves*kubernetesDefaultProgressDeadline; got < floor {
		t.Errorf("RollingDeployCeiling = %v, want at least %d waves x %v = %v", got, defaultRolloutMaxWaves, kubernetesDefaultProgressDeadline, floor)
	}
}
