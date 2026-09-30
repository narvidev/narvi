package controlplane

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/narvidev/narvi/internal/domain/integrations"
	"github.com/narvidev/narvi/internal/platform"
)

// TestLogGitHubAxes proves the boot line an operator is told to trust
// (docs/PRODUCTION_CHECKLIST.md item 11, the outbox-delivery runbook)
// reports each axis from its own source: ingress from IngressEnabled,
// outbound and its credential from GitHubOutbound -- including the two
// shapes where they differ, the ones a line reading one axis for the other
// would get wrong.
func TestLogGitHubAxes(t *testing.T) {
	outbound := platform.MustNewGitHubOutboundConfig("tok")
	tests := []struct {
		name           string
		ingress        bool
		outbound       *platform.GitHubOutboundConfig
		wantOutbound   bool
		wantCredential string
	}{
		{"both on", true, outbound, true, "bot token"},
		{"ingress off, outbound on", false, outbound, true, "bot token"},
		{"both off", false, nil, false, "none"},
		{"ingress on, outbound off (Load refuses it; the line must not paper over it)", true, nil, false, "none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			cfg := &platform.Config{
				IngressEnabled: map[integrations.Provider]bool{integrations.ProviderGitHub: tc.ingress},
				GitHubOutbound: tc.outbound,
			}

			logGitHubAxes(logger, cfg)

			var line map[string]any
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("decode log line %q: %v", buf.String(), err)
			}
			if line["msg"] != "narvi control-plane: GitHub axes" {
				t.Errorf("msg = %v, want the GitHub axes line", line["msg"])
			}
			if line["ingress"] != tc.ingress || line["outbound"] != tc.wantOutbound || line["outbound_credential"] != tc.wantCredential {
				t.Errorf("line = ingress=%v outbound=%v outbound_credential=%v, want ingress=%v outbound=%v outbound_credential=%v",
					line["ingress"], line["outbound"], line["outbound_credential"], tc.ingress, tc.wantOutbound, tc.wantCredential)
			}
		})
	}
}
