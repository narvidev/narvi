package contractstest

import (
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
)

func TestSessionConfigRoundTrip(t *testing.T) {
	// session-config.schema.json's root is a "$ref" to #/$defs/SessionConfig,
	// so compiling the document itself (no fragment) already validates the
	// full SESSION_CONFIG shape (§4.1: "sandbox env passed as one
	// SESSION_CONFIG JSON document").
	sch := compileSchema(t, "session-config/v1/session-config.schema.json", "")

	t.Run("WithCorrelationIdAndBranch", func(t *testing.T) {
		correlationID := "corr-123"
		branch := "main"
		roundTrip(t, sch, sessionconfig.SessionConfig{
			SessionId:         testSessionID,
			Gen:               1,
			SandboxId:         testSandboxID,
			SandboxToken:      "sandbox-token-plaintext",
			BootMode:          sessionconfig.SessionConfigBootModeFresh,
			ControlPlaneWsUrl: "wss://cp.narvi.dev/sessions/" + testSessionID + "/ws?type=sandbox",
			Repos: []sessionconfig.SessionConfigReposElem{
				{Name: "narvi", Url: "https://github.com/narvidev/narvi.git", Branch: &branch},
			},
			CorrelationId: &correlationID,
		})
	})

	t.Run("NullCorrelationIdAndBranch", func(t *testing.T) {
		// correlationId null means no upstream correlation id exists; repo
		// branch null means create the session branch from the repo's
		// default base branch (§4.1/§3.4).
		roundTrip(t, sch, sessionconfig.SessionConfig{
			SessionId:         testSessionID,
			Gen:               2,
			SandboxId:         testSandboxID,
			SandboxToken:      "sandbox-token-plaintext",
			BootMode:          sessionconfig.SessionConfigBootModeSnapshotRestore,
			ControlPlaneWsUrl: "wss://cp.narvi.dev/sessions/" + testSessionID + "/ws?type=sandbox",
			Repos: []sessionconfig.SessionConfigReposElem{
				{Name: "narvi", Url: "https://github.com/narvidev/narvi.git", Branch: nil},
			},
			CorrelationId: nil,
		})
	})

	t.Run("WithPullRequestRef", func(t *testing.T) {
		// Technical plan §21.1, §30.4: a pull request's review session names
		// its base repository and the pull request's head ref in it; ref is
		// optional, so every document above, which omits it, stays valid.
		ref := "refs/pull/7/head"
		roundTrip(t, sch, sessionconfig.SessionConfig{
			SessionId:         testSessionID,
			Gen:               3,
			SandboxId:         testSandboxID,
			SandboxToken:      "sandbox-token-plaintext",
			BootMode:          sessionconfig.SessionConfigBootModeFresh,
			ControlPlaneWsUrl: "wss://cp.narvi.dev/sessions/" + testSessionID + "/ws?type=sandbox",
			Repos: []sessionconfig.SessionConfigReposElem{
				{Name: "widgets", Url: "https://github.com/acme/widgets.git", Branch: nil, Ref: &ref},
			},
			CorrelationId: nil,
		})
	})

	// ref names a pull request's head ref and nothing else: a branch, the
	// merge ref or any other shape is refused by the schema.
	t.Run("RefOtherThanAPullRequestHeadRejected", func(t *testing.T) {
		for _, ref := range []string{"refs/heads/main", "refs/pull/7/merge", "refs/pull/0/head", "main"} {
			payload := []byte(`{"sessionId":"` + testSessionID + `","gen":1,"sandboxId":"` + testSandboxID +
				`","sandboxToken":"t","bootMode":"fresh","controlPlaneWsUrl":"wss://cp.narvi.dev/ws","correlationId":null,` +
				`"repos":[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":null,"ref":"` + ref + `"}]}`)
			if err := validateJSON(t, sch, payload); err == nil {
				t.Errorf("ref %q: expected schema validation to fail, got nil error", ref)
			}
		}
	})
}
