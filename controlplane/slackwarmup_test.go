package controlplane

import (
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/platform"
)

// TestSlackInstallationTeamToWarm pins which boots warm the Slack
// installation's workspace (§13.2): only with Slack ingress on, and never
// in the development stage, where `make dev` and the control-plane image
// check run serve() with placeholder Slack credentials -- a warm-up there
// would call Slack's real host on every boot. The client is never called
// here: it is built with no HTTP client, so it could make no request.
func TestSlackInstallationTeamToWarm(t *testing.T) {
	t.Parallel()

	client := slackapi.New(nil, "", "unused")
	for _, tc := range []struct {
		stage        platform.Stage
		slackIngress bool
		wantWarm     bool
	}{
		{platform.StageDevelopment, true, false},
		{platform.StageDevelopment, false, false},
		{platform.StageStaging, true, true},
		{platform.StageStaging, false, false},
		{platform.StageProduction, true, true},
		{platform.StageProduction, false, false},
	} {
		got := slackInstallationTeamToWarm(tc.stage, tc.slackIngress, client)
		if (got != nil) != tc.wantWarm || (got != nil && got != client) {
			t.Errorf("slackInstallationTeamToWarm(%s, slack ingress %v) = %p, want warm: %v (client %p)", tc.stage, tc.slackIngress, got, tc.wantWarm, client)
		}
	}
}
