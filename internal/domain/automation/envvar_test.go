package automation_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/domain/sandboxsecret"
)

func TestValidateEnvVars(t *testing.T) {
	maxVars := make([]automation.EnvVar, automation.MaxEnvVars)
	for i := range maxVars {
		maxVars[i] = automation.EnvVar{Name: "VAR" + string(rune('A'+i%26)) + string(rune('0'+i/26)), Value: "v"}
	}
	overMax := append(append([]automation.EnvVar{}, maxVars...), automation.EnvVar{Name: "ONE_MORE", Value: "v"})

	tests := []struct {
		name    string
		vars    []automation.EnvVar
		wantErr error
	}{
		{"nil is valid", nil, nil},
		{"empty is valid", []automation.EnvVar{}, nil},
		{"one valid var", []automation.EnvVar{{Name: "FOO", Value: "bar"}}, nil},
		{"empty value is legitimate", []automation.EnvVar{{Name: "FOO", Value: ""}}, nil},
		{"underscore and digits", []automation.EnvVar{{Name: "_FOO_2", Value: "bar"}}, nil},
		{"empty name", []automation.EnvVar{{Name: "", Value: "bar"}}, automation.ErrEmptyEnvVarName},
		{"name starts with digit", []automation.EnvVar{{Name: "2FOO", Value: "bar"}}, automation.ErrInvalidEnvVarName},
		{"name has dash", []automation.EnvVar{{Name: "FOO-BAR", Value: "bar"}}, automation.ErrInvalidEnvVarName},
		{"name has space", []automation.EnvVar{{Name: "FOO BAR", Value: "bar"}}, automation.ErrInvalidEnvVarName},
		{"duplicate name", []automation.EnvVar{{Name: "FOO", Value: "1"}, {Name: "FOO", Value: "2"}}, automation.ErrDuplicateEnvVarName},
		{"exactly the max", maxVars, nil},
		{"one over the max", overMax, automation.ErrTooManyEnvVars},
		// §25.1/§27.1 reservation checks (this Step's own addition: once
		// automation env vars are threaded into cmd.Env alongside provider
		// credentials/sandbox secrets, a name owned by either mechanism
		// must be refused here, not merely accepted into a prompt
		// preamble only). Reuses sandboxsecret.ValidateNotReserved, so
		// this table only needs one representative per reserved category
		// -- that function's own exhaustive table (name_test.go) already
		// covers every individual reserved name/prefix.
		{"reserved NARVI_ namespace", []automation.EnvVar{{Name: "NARVI_SESSION_CONFIG", Value: "v"}}, automation.ErrReservedEnvVarName},
		{"reserved OPENCODE_ namespace", []automation.EnvVar{{Name: "OPENCODE_CONFIG", Value: "v"}}, automation.ErrReservedEnvVarName},
		{"reserved provider credential name", []automation.EnvVar{{Name: "ANTHROPIC_API_KEY", Value: "v"}}, automation.ErrReservedEnvVarName},
		{"reserved cloud identity name", []automation.EnvVar{{Name: "AWS_ROLE_ARN", Value: "v"}}, automation.ErrReservedEnvVarName},
		{"reserved cluster binding name", []automation.EnvVar{{Name: "KUBECONFIG", Value: "v"}}, automation.ErrReservedEnvVarName},
		// A reserved-namespace-shaped name that ALSO fails the plain
		// POSIX-identifier shape check must report the shape problem, not
		// a reservation one -- shape is checked first (isValidEnvVarName,
		// before ValidateNotReserved, envvar.go).
		{"shape check wins over reservation when both fail", []automation.EnvVar{{Name: "2NARVI_FOO", Value: "v"}}, automation.ErrInvalidEnvVarName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := automation.ValidateEnvVars(tt.vars)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateEnvVars_ReservedNameUnwrapsToSandboxsecretReason proves a
// reserved-name rejection carries BOTH the generic
// automation.ErrReservedEnvVarName a caller can branch on without
// importing sandboxsecret, AND the specific underlying sandboxsecret
// sentinel naming exactly which mechanism owns the name -- double-%w
// wrapping (envvar.go's own ValidateEnvVars), not a re-stated string.
func TestValidateEnvVars_ReservedNameUnwrapsToSandboxsecretReason(t *testing.T) {
	err := automation.ValidateEnvVars([]automation.EnvVar{{Name: "ANTHROPIC_API_KEY", Value: "v"}})
	if !errors.Is(err, automation.ErrReservedEnvVarName) {
		t.Errorf("errors.Is(err, automation.ErrReservedEnvVarName) = false, want true (err = %v)", err)
	}
	if !errors.Is(err, sandboxsecret.ErrNameReservedProviderCredential) {
		t.Errorf("errors.Is(err, sandboxsecret.ErrNameReservedProviderCredential) = false, want true (err = %v)", err)
	}
}
