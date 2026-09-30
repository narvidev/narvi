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

// TestDeployment_ShippedRolloutFitsRollingDeployCeiling checks the one part
// of platform.Timeouts.RollingDeployCeiling the repository can check. The
// ceiling itself is an operational assumption (docs/PRODUCTION_CHECKLIST.md,
// item 12): Kubernetes bounds no rollout's length. progressDeadlineSeconds
// bounds only the gap between two progress events -- each new pod turning
// ready is one -- a rollout past it is flagged, not stopped, and a paused
// one is not timed. What the shipped manifest does bound is a rollout
// Kubernetes never flags: with no strategy and no progressDeadlineSeconds
// set, Kubernetes' defaults apply, each new pod turns ready within 600 s of
// the previous progress event, and the whole fleet is replaced within
// replicas x 600 s. That must fit under the ceiling. A manifest that sets a
// strategy or a progress deadline, or raises replicas past what the ceiling
// covers, fails here, so the ceiling and the checklist item are revisited
// with it.
func TestDeployment_ShippedRolloutFitsRollingDeployCeiling(t *testing.T) {
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
			t.Errorf("deployment.yaml sets spec.%s: the bound checked here assumes Kubernetes' default for it -- recompute the longest unflagged rollout, then revisit platform.Timeouts.RollingDeployCeiling and this test", key)
		}
	}
	replicas, ok := manifest.Spec["replicas"].(int)
	if !ok || replicas < 1 {
		t.Fatalf("deployment.yaml spec.replicas = %v, want a positive integer", manifest.Spec["replicas"])
	}
	ceiling := platform.DefaultTimeouts().RollingDeployCeiling
	if longest := time.Duration(replicas) * kubernetesDefaultProgressDeadline; longest > ceiling {
		t.Errorf("deployment.yaml's %d replicas can take %v to replace without Kubernetes flagging the rollout, past RollingDeployCeiling (%v): raise the ceiling (and UnknownTimerGrace above it), or state how this fleet keeps its rollouts under it", replicas, longest, ceiling)
	}
}
