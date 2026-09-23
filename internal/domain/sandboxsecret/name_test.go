package sandboxsecret

import (
	"errors"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/cloudidentity"
	"github.com/narvidev/narvi/internal/domain/clusterbinding"
	"github.com/narvidev/narvi/internal/domain/providercredential"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr error // nil means "no error"
	}{
		{"ordinary uppercase name", "MY_SECRET", nil},
		{"single letter", "A", nil},
		{"leading underscore", "_PRIVATE_KEY", nil},
		{"digits after first char", "SECRET_2", nil},
		{"empty", "", ErrNameEmpty},
		{"lowercase rejected", "my_secret", ErrNameShape},
		{"mixed case rejected", "My_Secret", ErrNameShape},
		{"starts with digit", "2FA_TOKEN", ErrNameShape},
		{"contains space", "MY SECRET", ErrNameShape},
		{"contains hyphen", "MY-SECRET", ErrNameShape},
		{"contains dot", "MY.SECRET", ErrNameShape},
		{"contains equals", "MY=SECRET", ErrNameShape},
		{"contains NUL", "MY_SECRET\x00", ErrNameShape},
		{"narvi exact prefix boundary", "NARVI_", ErrNameReservedNarviNamespace},
		{"narvi reserved boot mode", "NARVI_BOOT_MODE", ErrNameReservedNarviNamespace},
		{"narvi reserved session config", "NARVI_SESSION_CONFIG", ErrNameReservedNarviNamespace},
		{"narvi reserved future var still rejected by prefix", "NARVI_SOME_FUTURE_VAR", ErrNameReservedNarviNamespace},
		{"opencode exact prefix boundary", "OPENCODE_", ErrNameReservedOpenCodeNamespace},
		{"opencode reserved custom config slot", "OPENCODE_CONFIG", ErrNameReservedOpenCodeNamespace},
		{"opencode reserved inline config slot", "OPENCODE_CONFIG_CONTENT", ErrNameReservedOpenCodeNamespace},
		{"opencode reserved future var still rejected by prefix", "OPENCODE_SOME_FUTURE_VAR", ErrNameReservedOpenCodeNamespace},
		{"provider reserved anthropic", "ANTHROPIC_API_KEY", ErrNameReservedProviderCredential},
		{"provider reserved openai", "OPENAI_API_KEY", ErrNameReservedProviderCredential},
		{"provider reserved google api key", "GOOGLE_API_KEY", ErrNameReservedProviderCredential},
		{"provider reserved google generative", "GOOGLE_GENERATIVE_AI_API_KEY", ErrNameReservedProviderCredential},
		{"provider reserved gemini", "GEMINI_API_KEY", ErrNameReservedProviderCredential},
		{"cloud identity reserved aws token file", "AWS_WEB_IDENTITY_TOKEN_FILE", ErrNameReservedCloudIdentity},
		{"cloud identity reserved aws role arn", "AWS_ROLE_ARN", ErrNameReservedCloudIdentity},
		{"cloud identity reserved aws role session name", "AWS_ROLE_SESSION_NAME", ErrNameReservedCloudIdentity},
		{"cloud identity reserved google application credentials", "GOOGLE_APPLICATION_CREDENTIALS", ErrNameReservedCloudIdentity},
		{"cloud identity reserved azure federated token file", "AZURE_FEDERATED_TOKEN_FILE", ErrNameReservedCloudIdentity},
		{"cloud identity reserved azure client id", "AZURE_CLIENT_ID", ErrNameReservedCloudIdentity},
		{"cloud identity reserved azure tenant id", "AZURE_TENANT_ID", ErrNameReservedCloudIdentity},
		{"cluster binding reserved kubeconfig", "KUBECONFIG", ErrNameReservedClusterBinding},
		{"process hijack reserved path", "PATH", ErrNameReservedProcessHijack},
		{"process hijack reserved home", "HOME", ErrNameReservedProcessHijack},
		{"process hijack reserved ld_preload", "LD_PRELOAD", ErrNameReservedProcessHijack},
		{"process hijack reserved ld_library_path", "LD_LIBRARY_PATH", ErrNameReservedProcessHijack},
		{"process hijack reserved bash_env", "BASH_ENV", ErrNameReservedProcessHijack},
		{"process hijack reserved env", "ENV", ErrNameReservedProcessHijack},
		{"process hijack reserved node_options", "NODE_OPTIONS", ErrNameReservedProcessHijack},
		{"process hijack reserved pythonstartup", "PYTHONSTARTUP", ErrNameReservedProcessHijack},
		{"process hijack reserved git_ssh_command", "GIT_SSH_COMMAND", ErrNameReservedProcessHijack},
		{"process hijack reserved git_ssh", "GIT_SSH", ErrNameReservedProcessHijack},
		{"process hijack reserved git_allow_protocol (cross-PR hazard)", "GIT_ALLOW_PROTOCOL", ErrNameReservedProcessHijack},
		{"process hijack reserved git_config_ prefix boundary", "GIT_CONFIG_", ErrNameReservedProcessHijack},
		{"process hijack reserved git_config_count", "GIT_CONFIG_COUNT", ErrNameReservedProcessHijack},
		{"process hijack reserved git_config_key_0", "GIT_CONFIG_KEY_0", ErrNameReservedProcessHijack},
		{"process hijack reserved git_config_value_0", "GIT_CONFIG_VALUE_0", ErrNameReservedProcessHijack},
		{"too long", strings.Repeat("A", maxNameLength+1), ErrNameTooLong},
		{"exactly at max length is fine", strings.Repeat("A", maxNameLength), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateName(tc.input)
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("ValidateName(%q) = %v, want nil", tc.input, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("ValidateName(%q) = %v, want error wrapping %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

// TestValidateName_NarviPrefixTakesPriorityOverShape confirms a name that
// is BOTH NARVI_*-prefixed and otherwise POSIX-shaped fails specifically
// as ErrNameReservedNarviNamespace, not ErrNameShape -- the two checks
// never actually conflict in practice (a NARVI_ prefix is itself POSIX
// shaped), but this pins the observable error identity a caller might
// branch on.
func TestValidateName_NarviPrefixTakesPriorityOverShape(t *testing.T) {
	err := ValidateName("NARVI_CUSTOM_THING")
	if !errors.Is(err, ErrNameReservedNarviNamespace) {
		t.Errorf("ValidateName(%q) = %v, want ErrNameReservedNarviNamespace", "NARVI_CUSTOM_THING", err)
	}
}

// TestValidateName_RejectsOpenCodeConfigContent is a direct regression test
// for the adversarial-review CRITICAL finding: before this fix, nothing in
// ValidateName rejected a sandbox_secrets row named "OPENCODE_CONFIG_CONTENT"
// -- OpenCode's own documented "inline config" env var, which sits ABOVE
// even the project slot (the one §8.2's sentinel-fix capability
// restriction targets) in OpenCode's real, verified precedence order (see
// cmd/sandbox-agent/opencodeconfig.go's own top doc comment). A maintainer
// holding ActionManageEnvSecrets could therefore have saved exactly this
// name with a value re-enabling unrestricted tools, and
// applySandboxSecretEnv/opencodeproc.Spawn would have threaded it straight
// into `opencode serve`'s own env, silently overriding the
// capability-restricted sentinel-fix child session's own restriction --
// defeating §27.2's own stated structural guarantee ("a customer-authored
// config can never override the security-relevant agent restriction") by
// the engine's own documented ordering. This test proves that specific
// name is rejected at validation -- i.e. it can never be saved as a
// sandbox_secrets row in the first place, so it can never reach the
// delivery/injection path at all. See
// TestCapabilityRestrictedProjectConfig_NeverTouchedBySandboxSecretInjection
// (cmd/sandbox-agent) for the complementary, separate proof that this
// binary's own injection mechanism has no OTHER way to reach the project
// config file even if a name like this somehow arrived in a resolved map.
func TestValidateName_RejectsOpenCodeConfigContent(t *testing.T) {
	err := ValidateName("OPENCODE_CONFIG_CONTENT")
	if !errors.Is(err, ErrNameReservedOpenCodeNamespace) {
		t.Errorf("ValidateName(%q) = %v, want ErrNameReservedOpenCodeNamespace", "OPENCODE_CONFIG_CONTENT", err)
	}
}

// TestValidateName_OpenCodePrefixTakesPriorityOverShape mirrors
// TestValidateName_NarviPrefixTakesPriorityOverShape exactly, for the new
// OPENCODE_ reservation -- pins the observable error identity a caller
// might branch on.
func TestValidateName_OpenCodePrefixTakesPriorityOverShape(t *testing.T) {
	err := ValidateName("OPENCODE_CUSTOM_THING")
	if !errors.Is(err, ErrNameReservedOpenCodeNamespace) {
		t.Errorf("ValidateName(%q) = %v, want ErrNameReservedOpenCodeNamespace", "OPENCODE_CUSTOM_THING", err)
	}
}

// TestValidateName_EveryProviderCredentialEnvVarNameIsRejected is an
// exhaustive, non-hardcoded check that ValidateName rejects EVERY name
// providercredential.AllEnvVarNames currently returns, ranged over rather
// than copy-pasted -- so if a future Step adds a 4th provider (or a new
// env-var alias for an existing one), this test starts failing the moment
// ValidateName's own rejection set silently falls out of sync, rather
// than only when someone remembers to hand-add a new row above.
func TestValidateName_EveryProviderCredentialEnvVarNameIsRejected(t *testing.T) {
	for _, reserved := range providercredential.AllEnvVarNames() {
		t.Run(reserved, func(t *testing.T) {
			err := ValidateName(reserved)
			if !errors.Is(err, ErrNameReservedProviderCredential) {
				t.Errorf("ValidateName(%q) = %v, want ErrNameReservedProviderCredential", reserved, err)
			}
		})
	}
}

// TestValidateName_EveryCloudIdentityEnvVarNameIsRejected is §27.4's own
// exhaustive, non-hardcoded mirror of
// TestValidateName_EveryProviderCredentialEnvVarNameIsRejected, ranged over
// cloudidentity.ReservedEnvVarNames rather than copy-pasted -- this is the
// mutation-test-visible guard: removing any one of cloudidentity's own
// reserved names (or removing this function's own call from ValidateName)
// makes this test fail on that exact name.
func TestValidateName_EveryCloudIdentityEnvVarNameIsRejected(t *testing.T) {
	for _, reserved := range cloudidentity.ReservedEnvVarNames() {
		t.Run(reserved, func(t *testing.T) {
			err := ValidateName(reserved)
			if !errors.Is(err, ErrNameReservedCloudIdentity) {
				t.Errorf("ValidateName(%q) = %v, want ErrNameReservedCloudIdentity", reserved, err)
			}
		})
	}
}

// TestValidateName_EveryClusterBindingEnvVarNameIsRejected mirrors
// TestValidateName_EveryCloudIdentityEnvVarNameIsRejected exactly, for
// §27.4's own §27.4 KUBECONFIG reservation.
func TestValidateName_EveryClusterBindingEnvVarNameIsRejected(t *testing.T) {
	for _, reserved := range clusterbinding.ReservedEnvVarNames() {
		t.Run(reserved, func(t *testing.T) {
			err := ValidateName(reserved)
			if !errors.Is(err, ErrNameReservedClusterBinding) {
				t.Errorf("ValidateName(%q) = %v, want ErrNameReservedClusterBinding", reserved, err)
			}
		})
	}
}

// TestValidateName_EveryProcessHijackNameIsRejected is the exhaustive,
// non-hardcoded mirror of TestValidateName_EveryProviderCredentialEnvVarNameIsRejected
// for processHijackReservedNames (name.go's own "process-hijack surface"
// addition, adversarial-review LOW-severity fix) -- ranged over rather
// than copy-pasted, so removing any one name from that slice (or removing
// ValidateNotReserved's own call into it) makes this test fail on that
// exact name, the mutation-test-visible guard this codebase's sibling
// exhaustive tests already establish.
func TestValidateName_EveryProcessHijackNameIsRejected(t *testing.T) {
	for _, reserved := range processHijackReservedNames {
		t.Run(reserved, func(t *testing.T) {
			err := ValidateName(reserved)
			if !errors.Is(err, ErrNameReservedProcessHijack) {
				t.Errorf("ValidateName(%q) = %v, want ErrNameReservedProcessHijack", reserved, err)
			}
		})
	}
}

// TestValidateName_GitConfigPrefixRejectsEveryVariableTheMechanismUses
// proves gitConfigReservedPrefix closes the WHOLE git env-config-override
// mechanism (GIT_CONFIG_COUNT + as many GIT_CONFIG_KEY_<n>/GIT_CONFIG_
// VALUE_<n> pairs as COUNT declares), not merely the one name
// (GIT_CONFIG_COUNT) explicitly named in the adversarial-review finding
// -- an enumerated {GIT_CONFIG_COUNT} check alone would miss
// GIT_CONFIG_KEY_1/GIT_CONFIG_VALUE_1 the moment a caller declared a
// second override.
func TestValidateName_GitConfigPrefixRejectsEveryVariableTheMechanismUses(t *testing.T) {
	names := []string{
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
		"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			err := ValidateName(name)
			if !errors.Is(err, ErrNameReservedProcessHijack) {
				t.Errorf("ValidateName(%q) = %v, want ErrNameReservedProcessHijack", name, err)
			}
		})
	}
}
