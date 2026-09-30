package httpapi

import (
	"testing"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// outboundTestHost is a stand-in code host this test only compares by
// identity: it is never called.
type outboundTestHost struct{ ports.SourceControl }

// TestSessionResultDeps_FreshnessDeps is the unit half of the result
// route's GitHub-outbound-off path (§12.5; the integration half is
// TestGetSessionResult_GitHubOutboundOff_FreshnessUnconfirmed): with
// Outbound nil the live freshness read gets no code host at all -- the
// "no code host configured" path -- rather than the wired one with an
// empty token; with Outbound set it gets the code host and the bot token.
func TestSessionResultDeps_FreshnessDeps(t *testing.T) {
	host := &outboundTestHost{}
	timeouts := platform.DefaultTimeouts()

	off := SessionResultDeps{SourceControl: host, Outbound: nil, Timeouts: timeouts}.freshnessDeps()
	if off.SourceControl != nil || off.Token != "" {
		t.Errorf("freshnessDeps() with Outbound nil = {SourceControl: %v, Token: %q}, want no code host and no token", off.SourceControl, off.Token)
	}

	on := SessionResultDeps{SourceControl: host, Outbound: platform.MustNewGitHubOutboundConfig("bot-tok"), Timeouts: timeouts}.freshnessDeps()
	if on.SourceControl != host || on.Token != "bot-tok" {
		t.Errorf("freshnessDeps() with Outbound set = {SourceControl: %v, Token: %q}, want the wired code host and the bot token", on.SourceControl, on.Token)
	}
}
