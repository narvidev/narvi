package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitclone"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

// checkoutBootSignals is the bridge's bootSignals with one addition: it
// marks the handler's boot complete just before the bridge says so, so a
// checkout the control plane sends once the sandbox reads Ready is never
// answered busy for a boot that has ended.
type checkoutBootSignals struct {
	bootSignals
	handler *commandHandler
}

func (s checkoutBootSignals) MarkBootComplete() {
	s.handler.bootComplete.Store(true)
	s.bootSignals.MarkBootComplete()
}

// HandleCheckout implements wsbridge.CheckoutHandler, the "checkout"
// command (technical plan §21.1, §30.4): for each repo it names, check out
// the commit a review turn records, read from the pull request's ref in
// the base repository (gitclone.CheckoutPullRef), and answer with one
// best-effort checkout_result whose messageId is
// 'checkout_result:{command messageId}', so every answer to one command is
// one stored row. The work runs on h.group, never on the read loop, which
// must stay free for a stop or the next command.
func (h *commandHandler) HandleCheckout(_ context.Context, cmd sandboxws.Checkout) {
	if h.cfg.SessionConfig == nil {
		slog.Warn("sandbox-agent: received checkout but no live session is configured", "messageId", cmd.MessageId)
		return
	}
	h.group.Go(func() error {
		result := h.checkout(cmd)
		if err := h.bridge.SendBestEffort(h.runCtx, result); err != nil {
			slog.Warn("sandbox-agent: send checkout_result over WS bridge failed", "messageId", cmd.MessageId, "error", err)
		}
		return nil
	})
}

// checkout runs cmd and builds its checkout_result. Every repo is answered
// busy, and nothing is touched, while the boot is still running or a turn
// is: the boot owns the worktree until it completes, and a turn's runtime
// reads and writes it. Otherwise the checkout holds checkoutMu, so no turn
// starts until it is done. A repo is matched to the session's own
// SESSION_CONFIG repo by name, whose url is the one fetched from; a name
// the session does not have is failed.
func (h *commandHandler) checkout(cmd sandboxws.Checkout) sandboxws.CheckoutResult {
	result := sandboxws.CheckoutResult{
		Type:             "checkout_result",
		MessageId:        "checkout_result:" + cmd.MessageId,
		SessionId:        h.cfg.SessionConfig.SessionId,
		Gen:              h.cfg.SessionConfig.Gen,
		CommandMessageId: cmd.MessageId,
		Repos:            make([]sandboxws.CheckoutResultReposElem, 0, len(cmd.Repos)),
	}

	busy := ""
	if !h.bootComplete.Load() {
		busy = "the sandbox is still booting"
	} else {
		h.checkoutMu.Lock()
		defer h.checkoutMu.Unlock()
		if n := h.runningTurns.Load(); n > 0 {
			busy = fmt.Sprintf("a turn is running in the sandbox (%d)", n)
		}
	}
	if busy != "" {
		for _, repo := range cmd.Repos {
			result.Repos = append(result.Repos, checkoutRepoResult(repo.Name, sandboxws.CheckoutResultReposElemOutcomeBusy, "", "", busy))
		}
		return result
	}

	var pathScope []string
	if h.cfg.SessionConfig.PathScope != nil {
		pathScope = []string(*h.cfg.SessionConfig.PathScope)
	}
	chownRepo := func(dir string) error {
		return boot.ChownWorkspaceForRuntime(dir, h.cfg.RuntimeUID, h.cfg.RuntimeGID)
	}
	for _, want := range cmd.Repos {
		repo, ok := sessionRepo(h.cfg.SessionConfig.Repos, want.Name)
		if !ok {
			result.Repos = append(result.Repos, checkoutRepoResult(want.Name, sandboxws.CheckoutResultReposElemOutcomeFailed, "", "",
				fmt.Sprintf("this session has no repo named %q", want.Name)))
			continue
		}
		out := gitclone.CheckoutPullRef(h.runCtx, h.sup, h.layout, h.cred, chownRepo, repo, want.Ref, want.Sha, pathScope,
			h.timeouts.GitFetchStepTimeout, h.timeouts.GitSyncStepTimeout, h.timeouts.ProcessStopGracePeriod)
		errText := ""
		if out.Err != nil {
			errText = out.Err.Error()
		}
		slog.Info("sandbox-agent: checkout", "messageId", cmd.MessageId, "repo", want.Name, "ref", want.Ref, "sha", want.Sha,
			"outcome", string(out.Outcome), "head", out.HeadSHA, "ref_sha", out.RefSHA, "error", errText)
		result.Repos = append(result.Repos, checkoutRepoResult(want.Name, sandboxws.CheckoutResultReposElemOutcome(out.Outcome), out.HeadSHA, out.RefSHA, errText))
	}
	return result
}

// sessionRepo is the repo of repos named name.
func sessionRepo(repos []sessionconfig.SessionConfigReposElem, name string) (sessionconfig.SessionConfigReposElem, bool) {
	for _, repo := range repos {
		if repo.Name == name {
			return repo, true
		}
	}
	return sessionconfig.SessionConfigReposElem{}, false
}

// checkoutRepoResult is one repo's entry in a checkout_result: a sha that
// is "" is null, and so is an error on checked_out. Any other error is
// capped as a critical event's text is (wsbridge.CapCriticalText), so a
// result naming several repos still fits the smallest frame a control
// plane reads.
func checkoutRepoResult(name string, outcome sandboxws.CheckoutResultReposElemOutcome, headSHA, refSHA, errText string) sandboxws.CheckoutResultReposElem {
	entry := sandboxws.CheckoutResultReposElem{Name: name, Outcome: outcome}
	if headSHA != "" {
		entry.HeadSha = &headSHA
	}
	if refSHA != "" {
		entry.RefSha = &refSHA
	}
	if outcome != sandboxws.CheckoutResultReposElemOutcomeCheckedOut {
		capped := wsbridge.CapCriticalText(errText)
		entry.Error = &capped
	}
	return entry
}
