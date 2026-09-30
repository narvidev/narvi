package sessionactor

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/review"
)

// tripwireCodeHost fails the test on any GitHub call it receives -- the
// stand-in for a code host an actor must NOT reach while GitHub outbound
// is off (§12.5). It satisfies reviewcontext.Fetcher explicitly, and
// ports.SourceControl through the embedded nil interface: any port
// method it does not override panics, which fails the test just as
// loudly.
type tripwireCodeHost struct {
	ports.SourceControl
	t     *testing.T
	calls int
}

func (h *tripwireCodeHost) GetPullRequest(context.Context, string, string, int32, string) (githubapi.PullRequest, error) {
	h.calls++
	h.t.Error("GetPullRequest called with GitHub outbound off")
	return githubapi.PullRequest{}, nil
}

func (h *tripwireCodeHost) GetCompareDiff(context.Context, string, string, string, string, string) (string, bool, error) {
	h.calls++
	h.t.Error("GetCompareDiff called with GitHub outbound off")
	return "", false, nil
}

func (h *tripwireCodeHost) ResolveBranchSHA(context.Context, ports.ResolveBranchSHASpec) (string, string, error) {
	h.calls++
	h.t.Error("ResolveBranchSHA called with GitHub outbound off")
	return "", "", nil
}

func (h *tripwireCodeHost) CreatePR(context.Context, ports.CreatePRSpec) (ports.PRRef, error) {
	h.calls++
	h.t.Error("CreatePR called with GitHub outbound off")
	return ports.PRRef{}, nil
}

// TestFetchAutoRetriggerReviewContext_NilOutbound_NoCall proves the
// actor's automatic re-review takes its existing degraded path -- the
// honest zero review.PreFetchedContext -- when GitHub outbound is off,
// WITHOUT calling the code host: a configured diff fetcher is not enough
// to read a pull request as the bot.
func TestFetchAutoRetriggerReviewContext_NilOutbound_NoCall(t *testing.T) {
	host := &tripwireCodeHost{t: t}
	a := &Actor{
		reviewDiffFetcher: host,
		githubOutbound:    nil,
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	got := a.fetchAutoRetriggerReviewContext(context.Background(), "acme/widgets", 7)

	if !reflect.DeepEqual(got, review.PreFetchedContext{}) {
		t.Errorf("fetchAutoRetriggerReviewContext() = %+v, want the zero PreFetchedContext", got)
	}
	if host.calls != 0 {
		t.Errorf("code host called %d times, want 0", host.calls)
	}
}

// TestCreateSentinelFixPRBestEffort_NilOutbound_NoCall proves the actor
// skips the sentinel fix PR when GitHub outbound is off, before any read
// or call: the actor here has no stores at all, so reaching the
// sentinel_fixes read would panic, and the tripwire code host fails the
// test on a CreatePR.
func TestCreateSentinelFixPRBestEffort_NilOutbound_NoCall(t *testing.T) {
	host := &tripwireCodeHost{t: t}
	a := &Actor{
		sourceControl:  host,
		githubOutbound: nil,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	a.createSentinelFixPRBestEffort(context.Background(), sandboxws.PushComplete{}, false)

	if host.calls != 0 {
		t.Errorf("code host called %d times, want 0", host.calls)
	}
}
