// Command sandbox-agent is the static binary shipped into sandbox images
// (§1). Its boot dispatch makes boot-mode/hook-policy
// decisions (internal/domain/sandboxboot) plus native process supervision
// (internal/sandboxagent/supervisor) -- process groups, killpg-style
// signaling, reaping, and bounded graceful-then-forceful shutdown -- and
// supervises a per-repo .narvi/
// services.yml multi-service manifest (internal/sandboxagent/services,
// §14.2) when one is present, falling back to the original setup.sh/
// start.sh hook contract otherwise -- both orchestrated by
// internal/sandboxagent/boot.RunBoot. It logs a boot fingerprint first
// (§5.3), runs the boot sequence for whatever repo list names, then blocks
// until told to shut down.
//
// Two things follow: (1) when Config.SessionConfig is present (the
// NARVI_SESSION_CONFIG env var was set), run() clones every repo it names
// (internal/sandboxagent/gitclone.CloneAll) and writes the generated
// AGENTS.md manifest BEFORE handing the successfully-cloned subset to
// boot.RunBoot as its []boot.RepoInfo -- when SessionConfig is nil (the
// common dev/test case), repos stays nil; (2)
// a SEPARATE "credential-helper" subcommand (main's own dispatch, mirroring
// cmd/control-plane/main.go's own subcommand pattern) that implements
// git's credential-helper protocol end to end (internal/sandboxagent/
// credentials) -- this is the exact command gitclone configures every
// `git clone` to invoke via `-c credential.helper=!'<this binary>'
// credential-helper` (§5.2).
//
// The real sandbox WS bridge (internal/sandboxagent/wsbridge) replaces the
// slog-only boot_progress reporter's earlier placeholder role:
// when Config.SessionConfig is present, run() builds
// a *wsbridge.Bridge and drives it via bridge.Run(ctx) alongside the
// existing OS-signal-driven shutdown -- whichever finishes first (an OS
// signal cancels ctx, or the control plane sends a "shutdown" command, or
// the handshake returns a fatal 401/403/404/410 status) converges on the
// SAME StopAll-based graceful shutdown the process supervisor built, except a fatal
// connect status propagates as run()'s own error instead. Originally,
// prompt/stop/push/snapshot/git_sync_complete were all wired to a log-only
// stub handler.
//
// The real OpenCode adapter (internal/adapters/outbound/
// opencode) and its process-spawning sibling
// (internal/sandboxagent/opencodeproc) round this out: when Config.SessionConfig is
// present, run() spawns `opencode serve` (via opencodeproc.Spawn, which
// itself reuses the SAME Supervisor already tracking every other
// supervised process -- StopAll's own existing graceful shutdown reaps it
// too, so this path adds no cleanup code of its own; that reaping is
// CONDITIONAL on this process receiving a catchable signal, since StopAll
// runs off the signal.NotifyContext below -- anything that SIGKILLs
// sandbox-agent strands `opencode serve` as an orphan, because the
// supervisor spawns every child into its own process group and so nothing
// else will ever signal it) BEFORE the WS bridge starts accepting
// commands -- a "prompt" command can arrive as soon as the bridge connects,
// concurrently with the boot/clone sequence (§6.1's own design), so the
// adapter must already exist by then. commandHandler.HandlePrompt now
// launches the actual turn (adapter.StartTurn) on its own goroutine (via
// this Step's own new commandHandler.group field, an errgroup.Group --
// never a bare `go` statement, §11) rather than running it inline: wsbridge
// dispatches HandlePrompt synchronously from its own readLoop, and a turn
// can run for minutes -- if it blocked readLoop directly, a "stop" command
// arriving while a turn is in flight would never be read, let alone acted
// on, defeating Stop's entire purpose. That goroutine deliberately uses
// run()'s own long-lived, OS-signal-driven ctx (captured in commandHandler
// at construction), NOT the shorter-lived, per-WS-connection ctx wsbridge's
// dispatch hands to HandlePrompt/HandleStop -- a turn that outlives a mere
// WS reconnect must not be aborted just because the connection blipped
// (the wsbridge ack protocol already handles redelivering the turn's own
// events across a reconnect independently of whether the turn itself is
// still running). git_sync_complete remains the EXACT log-only stub
// originally shipped -- §3.4 (gitstate in-sandbox) is that command's own job,
// per §3.4's own assignment of that work; leave it
// exactly as it is.
//
// §9.3 ("e2e happy path") gives push its own real behavior:
// commandHandler.HandlePush now runs a real `git push` (via the SAME
// Supervisor every other supervised process already uses, configured with
// the SAME per-invocation credential-helper convention CloneAll already
// established for `git clone`), then reports a real sandboxws.
// PushComplete (with the resulting HEAD sha per repo) or PushError over
// the WS bridge.
//
// §3.2 ("snapshots & restore") gives snapshot its own real behavior:
// commandHandler.HandleSnapshot now calls the control plane's new
// snapshot-mint endpoint (internal/sandboxagent/snapshotclient, design
// decision 2) to obtain a real, sandbox-confirmed snapshotId, then reports
// a real CRITICAL sandboxws.SnapshotReady over the WS bridge (design
// decision 4) -- see HandleSnapshot's own doc comment for the full round
// trip and its one honest, documented failure-reporting gap (no NACK
// event exists on the wire for a failed snapshot attempt).
//
// §3.3 ("turn recovery", §3.3) fixes commandHandler.HandlePrompt's own
// conversation-id reporting to genuinely happen "at turn start... never
// lazily": it now passes a ports.ConversationIDReporter callback into
// StartTurn (adapter.go invokes it immediately once resolveSession
// resolves a real id, well before the rest of a turn's own, possibly-
// minutes-long execution), calling h.bridge.SetConversationID from inside
// that callback -- replacing the old §7 wiring, which only ever read
// StartTurn's own RETURN value, meaning the first report of a real
// conversation id used to happen only after a turn had basically already
// ended. wsbridge.Bridge.SetConversationID itself (internal/sandboxagent/
// wsbridge) now also triggers an immediate, out-of-band heartbeat send the
// first time it observes a genuinely new, non-nil id, rather than waiting
// for its own next regular tick.
//
// §3.4 ("gitstate in-sandbox", §3.4) gives runBootSequence real
// boot-mode-aware dispatch: BootModeBuild/BootModeFresh keep calling
// gitclone.CloneAll (a fresh clone into an empty directory) unchanged;
// BootModeRepoImage/BootModeSnapshotRestore instead call the new
// gitclone.SyncAll against the workspace's own ALREADY-EXISTING repo
// (baked into the image or restored from a snapshot) -- the real
// "stash-if-dirty -> checkout session branch (create from base if absent)
// -> stash pop" sequence, driven through internal/domain/gitstate's own
// Transition table. Each real phase fires a best-effort sandboxws.GitSync
// event over the WS bridge (onGitSync, run()); HandleGitSyncComplete's own
// log-only behavior is now documented as deliberate, final behavior, not a
// stub. A BootModeBuild boot additionally runs gitclone.CleanForImageBuild
// once RunBoot itself succeeds, discarding untracked/modified residue
// setup.sh may have left behind, so the tree is guaranteed clean by the
// time a provider is free to snapshot it (§3.4: "Image builds must
// snapshot a clean tree"). CloneAll itself also gained sparse-checkout
// support (§14.1): a session's own Environment.PathScope, now threaded
// through SessionConfig's own new optional pathScope field, restricts a
// freshly-cloned repo's working tree to exactly those patterns. SyncAll
// (gitclone) re-applies that same pathScope against an already-existing
// workspace too -- see runBootSequence's own comment, below, for why a
// repo_image/snapshot_restore boot needs this every bit as much as a fresh
// clone does.
//
// §25.1 ("provider credential injection", §25.1/§25.3) closes the
// long-standing gap named in every prior Step's own opencodeproc.Spawn
// call: no ANTHROPIC_API_KEY/OPENAI_API_KEY/Google-equivalent was ever
// wired into the spawned `opencode serve` process for ANY provider, even
// though per-turn model selection has worked end to end already.
// fetchProviderCredentialSpawnEnv (below) resolves this session's own
// repo/environment/global-scoped provider credentials from CP (a NEW
// sandbox-bearer-authenticated delivery endpoint, mirroring scm-
// credentials' own security posture exactly) immediately before Spawn,
// mapping each resolved provider onto its own env-var name(s)
// (internal/domain/providercredential.EnvVarNames) and appending them to
// Spawn's own filtered base environment. Deliberately best-effort: the
// overwhelming common case (nothing configured for this session) resolves
// to nil, and any fetch failure degrades the SAME way, changing nothing
// about this binary's own pre-existing behavior.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/opencode"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/providercredential"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/credentials"
	"github.com/narvidev/narvi/internal/sandboxagent/gitclone"
	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/opencodeproc"
	"github.com/narvidev/narvi/internal/sandboxagent/services"
	"github.com/narvidev/narvi/internal/sandboxagent/snapshotclient"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

func main() {
	// A bare-bones dispatch, not a flag-parsing library, mirroring
	// cmd/control-plane/main.go's own subcommand pattern: ONE alternate
	// subcommand exists today -- "credential-helper" (the process git
	// itself invokes per gitclone's own `-c credential.helper=...`
	// configuration) -- everything else falls through to the normal boot
	// sequence.
	//
	// (§27.4) originally shipped a SECOND subcommand here,
	// "kube-credential", for the AuthKindOIDC cluster rung's own exec
	// plugin. Adversarial-review HIGH fix: that subcommand needed
	// NARVI_SESSION_CONFIG (via boot.Load()) to mint anything, but every
	// process that can ever run `kubectl` (opencodeproc.Spawn, boot/
	// hooks.go's runHook, boot/runboot.go's services.yml dispatch) strips
	// that var from the child env on purpose -- it carries this sandbox's
	// own live bearer token -- so the exec plugin's own re-invocation of
	// this binary had no env to inherit it FROM and failed 100% of the
	// time. Removed entirely, never replaced with an equivalent
	// subcommand: kubeconfig.go's own applyClusterBinding now mints that
	// rung's own token the SAME way §27.3's cloud_identity_bindings
	// tokens are minted and points the rendered kubeconfig's own
	// `tokenFile` field straight at the resulting file -- see
	// kubeconfig.go's own top doc comment ("Design correction") for the
	// full reasoning and the client-go source verification behind it.
	if len(os.Args) >= 2 && os.Args[1] == "credential-helper" {
		if err := runCredentialHelper(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runCredentialHelper implements the "credential-helper <get|store|erase>"
// subcommand git itself invokes (via gitclone's own `-c
// credential.helper=!'<this binary>' credential-helper` configuration on
// every clone). This is a SEPARATE process git spawns, inheriting
// sandbox-agent's own environment (supervisor.Spec.Env is nil/inherit in
// gitclone's Spawn call) -- so re-reading the same NARVI_* env vars here
// via boot.Load() is correct and sufficient, not a duplication of that
// config-loading logic.
func runCredentialHelper(args []string) error {
	if len(args) != 1 {
		return errors.New("sandbox-agent: credential-helper: usage: sandbox-agent credential-helper <get|store|erase>")
	}

	op := args[0]
	if op != "get" && op != "store" && op != "erase" {
		return fmt.Errorf("sandbox-agent: credential-helper: unknown op %q, want get/store/erase", op)
	}

	cfg, err := boot.Load()
	if err != nil {
		return fmt.Errorf("sandbox-agent: credential-helper: load config: %w", err)
	}

	if op == "store" {
		return credentials.RunStore(os.Stdin)
	}

	cache := &credentials.Cache{Dir: cfg.CredentialCacheDir}

	if op == "erase" {
		return credentials.RunErase(os.Stdin, cache)
	}

	// op == "get" from here on -- the only op that actually needs a live
	// SessionConfig (ControlPlaneWsUrl/SessionId/SandboxToken to mint a
	// fresh credential from CP). store/erase need neither, so gating them
	// on it too would (in some future scenario where erase is invoked
	// without one) block purging a bad cache entry -- working against the
	// very goal RunErase exists for.
	if cfg.SessionConfig == nil {
		return errors.New(
			"sandbox-agent: credential-helper: get: NARVI_SESSION_CONFIG is unset -- nothing to fetch credentials for",
		)
	}

	timeouts := platform.DefaultTimeouts()
	client, err := credentials.NewCPClient(cfg.SessionConfig.ControlPlaneWsUrl, timeouts.CredentialFetchTimeout)
	if err != nil {
		return fmt.Errorf("sandbox-agent: credential-helper: build CP client: %w", err)
	}

	// forceReadOnly (§30.4(2)): "the image-build path must never hold a
	// write token... force the read-only mint for BootModeBuild (a build
	// only needs read)". This is the primary fix -- the credential cache
	// purge inside gitclone.CleanForImageBuild (below, cfg.BootMode ==
	// sandboxboot.BootModeBuild branch) is defense-in-depth on top of it,
	// not a substitute for it.
	forceReadOnly := cfg.BootMode == sandboxboot.BootModeBuild

	return credentials.RunGet(
		context.Background(), os.Stdin, os.Stdout, cache, client,
		cfg.SessionConfig.SessionId, cfg.SessionConfig.SandboxToken, cfg.SessionConfig.Gen, timeouts.CredentialExpiryBuffer,
		forceReadOnly,
	)
}

// commandHandler is sandbox-agent's own wsbridge.CommandHandler
// implementation. It started as an empty, log-only struct for all
// 5 commands; HandlePrompt/HandleStop now have their real behavior
// (push/snapshot/git_sync_complete are untouched, still unimplemented,
// confirmed against the plan rather
// than guessed).
//
// A *commandHandler (pointer receiver, unlike §6.1's value-receiver
// empty struct) because adapter/bridge are populated in TWO phases: run()
// constructs a *commandHandler with adapter already set, passes it to
// wsbridge.New as the CommandHandler interface value, THEN sets .bridge on
// that SAME pointer once the Bridge itself exists -- Bridge and
// commandHandler each need a reference to the other (HandlePrompt forwards
// events onto the bridge; the bridge dispatches commands to the handler),
// so one of the two references must be filled in after construction.
type commandHandler struct {
	// adapter is nil exactly when cfg.SessionConfig is nil (see run()) --
	// HandlePrompt/HandleStop below treat that as "no live session, no
	// OpenCode to talk to" and log a warning instead of dispatching,
	// mirroring this Step's own "no real session, no work" precedent from
	// every prior sandbox-agent Step. In practice a *commandHandler is
	// only ever constructed at all within that same cfg.SessionConfig !=
	// nil branch (see run()), so this nil check is defense-in-depth, not
	// a path this binary's own current wiring can actually reach.
	adapter *opencode.Adapter
	bridge  *wsbridge.Bridge

	// runCtx is run()'s own long-lived, OS-signal-driven context --
	// deliberately NOT the shorter-lived, per-WS-connection ctx wsbridge's
	// own dispatch hands to HandlePrompt/HandleStop (see the package doc
	// comment's own §7 paragraph for why: a turn must survive a mere
	// WS reconnect, not be aborted by one).
	runCtx context.Context

	// cfg/timeouts/sup are §9.3's ("e2e happy path") own additions,
	// needed by HandlePush: cfg.WorkspaceDir/SessionConfig.Repos locate
	// each repo and its original clone URL (to determine which host to
	// mint a git credential for); timeouts bounds the push/rev-parse
	// subprocesses; sup is the SAME Supervisor run() already constructs
	// and uses for every other supervised process (hooks, services,
	// opencode) -- HandlePush reuses it rather than spawning bare
	// exec.Command calls, matching internal/sandboxagent/gitclone's own
	// "never a bare exec.Command" precedent exactly.
	cfg      boot.Config
	timeouts platform.Timeouts
	sup      *supervisor.Supervisor

	// reviewCostBudgetURL is §26.5's own addition (§26.7/§26.9): the
	// real, already-bound http://127.0.0.1:<port>/review-cost-budget URL
	// this sandbox's own loopback budget server resolved at startup (run(),
	// via budgetServer.URL()) -- empty exactly when cfg.SessionConfig was
	// nil (no server was ever started, see run()'s own budgetServer
	// declaration). HandlePrompt substitutes this for
	// review.ReviewCostBudgetToolURLPlaceholder in a review turn's own
	// prompt text, mirroring cfg's own identical role for
	// renderVerdictToolPromptText/renderUploadToolPromptText.
	reviewCostBudgetURL string

	// group launches each HandlePrompt's own StartTurn call on its own
	// goroutine (via errgroup.Group.Go, never a bare `go` statement, §11)
	// so wsbridge's own readLoop -- which calls HandlePrompt synchronously
	// -- is never blocked for an entire turn's duration; a "stop" command
	// arriving mid-turn must still reach HandleStop promptly. Deliberately
	// a zero-value errgroup.Group, never Wait()'d on except once, at
	// shutdown (run()'s own final drain) -- the SAME "one launched unit's
	// own failure/completion must never cancel a sibling's independent
	// work" reasoning as internal/sandboxagent/supervisor.Supervisor's own
	// analogous field.
	group errgroup.Group
}

func (h *commandHandler) HandlePrompt(_ context.Context, cmd sandboxws.Prompt) {
	if h.adapter == nil {
		slog.Warn("sandbox-agent: received prompt but no OpenCode adapter is configured (no live session)",
			"messageId", cmd.MessageId)
		return
	}

	// §8.2 ("server-side verdict", §8.2/§5.2): a review turn's own text
	// (internal/domain/review.RenderTurnPrompt) carries FIXED placeholder
	// tokens in place of this turn's real verdict-posting-tool URL/bearer/
	// gen -- see reviewverdicttoolprompt.go's own top doc comment for why
	// this is the one place those placeholders can actually be resolved. A
	// no-op for every non-review turn (no placeholders present).
	cmd.Text = renderVerdictToolPromptText(cmd.Text, h.cfg.SessionConfig)
	// (§28.5): the SAME mechanism, extended for the
	// download_file/upload tools' own placeholders (internal/domain/
	// upload's attachment block + upload-tool note, rendered at
	// turn-creation time by createTurnLocked) -- a no-op for a turn with
	// none of those placeholders present.
	cmd.Text = renderUploadToolPromptText(cmd.Text, h.cfg.SessionConfig)
	// (§20.2): the SAME mechanism, extended for the devil's-
	// advocate preamble's own epistemic-outcome-reporting tool
	// (internal/domain/turn.RenderEpistemicPreamble, rendered at
	// turn-creation time by createTurnLocked when the check is enabled
	// for a non-plan-mode turn, §20.3/§20.4) -- a no-op for every turn
	// with none of those placeholders present, i.e. the overwhelming
	// common case while the feature stays off by default.
	cmd.Text = renderEpistemicOutcomeToolPromptText(cmd.Text, h.cfg.SessionConfig)
	// (§26.7/§26.9): the SAME mechanism once more, for the
	// review-cost-budget loopback endpoint's own URL placeholder
	// (internal/domain/review.ReviewCostBudgetToolURLPlaceholder,
	// subAgentOrchestrationInstructions) -- a no-op for every turn without
	// that placeholder present (every non-review turn, and a review turn
	// whose ceiling was never configured, review/context.go's own gating).
	cmd.Text = renderReviewCostBudgetToolPromptText(cmd.Text, h.reviewCostBudgetURL)
	// (§15.3): the SAME mechanism once more, for the
	// aggregate-diff composition review turn's own composition-findings-
	// posting tool (internal/domain/review.RenderCompositionReviewPrompt,
	// dispatched by internal/app/releasereview's own dispatchCompositionReview)
	// -- a no-op for every turn without those placeholders present (every
	// turn that isn't a composition-review turn).
	cmd.Text = renderCompositionFindingsToolPromptText(cmd.Text, h.cfg.SessionConfig)

	h.group.Go(func() error {
		sink := func(event ports.AgentEvent) {
			if event.Critical {
				if err := h.bridge.SendCritical(h.runCtx, event.Payload, event.AckID); err != nil {
					slog.Warn("sandbox-agent: send critical agent event over WS bridge failed",
						"messageId", cmd.MessageId, "ackId", event.AckID, "error", err)
				}
				return
			}
			if err := h.bridge.SendBestEffort(h.runCtx, event.Payload); err != nil {
				slog.Warn("sandbox-agent: send best-effort agent event over WS bridge failed",
					"messageId", cmd.MessageId, "error", err)
			}
		}

		// §3.3 ("turn recovery"), §3.3 "at turn start... never lazily":
		// report the conversation id to the bridge THE INSTANT StartTurn
		// itself resolves it (adapter.go's own resolveSession, called
		// long before the rest of a turn's own, possibly-minutes-long
		// execution) via this callback -- moved out of its old §7
		// position of reading StartTurn's own RETURN value only after the
		// whole call completed (which meant the FIRST report of a real
		// conversation id used to only ever happen after a turn had
		// basically already ended, directly contradicting §3.3). That old
		// post-return read is gone entirely: onConversationID's callback
		// already covers every path that ever resolves a real id (see
		// ports.ConversationIDReporter's own doc comment), so a second
		// read of StartTurn's own return value here would be redundant,
		// dead code, not a genuinely different case.
		onConversationID := func(id string) {
			h.bridge.SetConversationID(&id)
		}

		if _, err := h.adapter.StartTurn(h.runCtx, cmd, sink, onConversationID); err != nil {
			slog.Warn("sandbox-agent: StartTurn returned an error", "messageId", cmd.MessageId, "error", err)
		}
		// Never propagate a per-turn error into this shared group -- one
		// turn's own failure must never affect a sibling launched call.
		return nil
	})
}

func (h *commandHandler) HandleStop(_ context.Context, cmd sandboxws.Stop) {
	if h.adapter == nil {
		slog.Warn("sandbox-agent: received stop but no OpenCode adapter is configured (no live session)",
			"messageId", cmd.MessageId)
		return
	}
	if err := h.adapter.Stop(h.runCtx, cmd); err != nil {
		slog.Warn("sandbox-agent: Stop failed", "messageId", cmd.MessageId, "error", err)
	}
}

// HandlePush implements the real `push` command (§9.3, "e2e happy
// path", design decision 7): for each repo named in cmd.Repos, runs a
// plain `git push <remote> <branch>` (this Step's own happy-path scope --
// no pre-existing dirty-tree reconciliation; internal/domain/gitstate's
// stash/checkout/pop machinery is explicitly §3.4's own job, not
// touched here), configured with the SAME per-invocation, never-
// persisted `-c credential.helper=!'<this binary>' credential-helper`
// convention internal/sandboxagent/gitclone.CloneAll already uses for
// cloning (gitclone.CredHelperGitArg, exported this Step specifically for
// this second caller) -- git itself invokes the credential-helper
// subcommand (internal/sandboxagent/credentials) exactly as it already
// does for clone, so no new credential-fetching code path is needed here
// at all, and §5.2's "never long-lived in sandbox" holds by construction
// (nothing is ever written to a persistent git-credential store).
//
// On the FIRST repo that fails, sends a single sandboxws.PushError (this
// wire type carries one error string, not a per-repo breakdown) and stops
// -- matching Push's own schema, which has no partial-success shape to
// report into.
func (h *commandHandler) HandlePush(_ context.Context, cmd sandboxws.Push) {
	if h.cfg.SessionConfig == nil {
		slog.Warn("sandbox-agent: received push but no live session is configured", "messageId", cmd.MessageId)
		return
	}

	completed := make([]sandboxws.PushCompleteReposElem, 0, len(cmd.Repos))
	for _, repoSpec := range cmd.Repos {
		sha, err := h.pushOneRepo(repoSpec)
		if err != nil {
			slog.Warn("sandbox-agent: push failed", "messageId", cmd.MessageId, "repo", repoSpec.Name, "error", err)
			h.sendPushError(cmd, err)
			return
		}
		completed = append(completed, sandboxws.PushCompleteReposElem{
			Name: repoSpec.Name, Branch: repoSpec.Branch, Sha: sha,
		})
	}

	h.sendPushComplete(cmd, completed)
}

// pushOneRepo runs `git push` for exactly one repo, then reads back the
// resulting HEAD sha, both via h.sup (never a bare exec.Command).
//
// Every one of repoSpec.Name/Branch/Remote is session-controlled
// (sandboxws.Push.Repos[], relayed from the control plane) and is
// validated via internal/domain/reposource BEFORE this repo's target
// directory is even computed (filepath.Join) or any Args are built for
// h.sup.Spawn -- the exact same argument-injection/path-traversal
// reasoning internal/sandboxagent/gitclone.CloneAll's own validateRepoSpec
// closes for clone, applied here for push's own two call-site-specific
// fields (remote, in addition to name/branch). See reposource's own
// package doc comment for the full reasoning.
func (h *commandHandler) pushOneRepo(repoSpec sandboxws.PushReposElem) (string, error) {
	if err := reposource.ValidateRepoName(repoSpec.Name); err != nil {
		return "", fmt.Errorf("invalid repo name: %w", err)
	}
	if err := reposource.ValidateBranch(repoSpec.Branch); err != nil {
		return "", fmt.Errorf("invalid repo branch: %w", err)
	}

	remote := "origin"
	if repoSpec.Remote != nil && *repoSpec.Remote != "" {
		remote = *repoSpec.Remote
	}
	if err := reposource.ValidateRemoteName(remote); err != nil {
		return "", fmt.Errorf("invalid remote: %w", err)
	}

	dir := filepath.Join(h.cfg.WorkspaceDir, repoSpec.Name)

	credHelperArg, err := gitclone.CredHelperGitArg()
	if err != nil {
		return "", fmt.Errorf("determine credential helper: %w", err)
	}

	// RepoCloneTimeout is reused here rather than a distinct field:
	// `git push` and `git clone` are both single, network-bound git
	// transport operations against the SAME remote, and a push can carry
	// a comparably large object set (a fresh branch's full diff) -- the
	// same generous, "large repo" budget CloneAll's own calls already use
	// (see RepoCloneTimeout's own doc comment) fits push equally well,
	// matching headSHA's own precedent just below of reusing an existing
	// Timeouts field for a materially identical class of operation rather
	// than inventing a near-duplicate one.
	pushCtx, cancel := context.WithTimeout(h.runCtx, h.timeouts.RepoCloneTimeout)
	defer cancel()

	var stderr bytes.Buffer
	proc, err := h.sup.Spawn(supervisor.Spec{
		Path: "git",
		// "--" ends option parsing for everything after it (verified
		// directly against real `git push` behavior, not assumed) --
		// defense in depth alongside the validation above: even an
		// already-validated remote/branch should never be positionally
		// ambiguous to git's own argument parser.
		Args:   githarden.Args(dir, "-c", "credential.helper="+credHelperArg, "push", "--", remote, repoSpec.Branch),
		Stderr: &stderr,
		// Env is DELIBERATELY left at its zero value (nil, "inherit this
		// process's own environment") -- a reviewed choice, not an
		// oversight. git's own credential.helper mechanism re-execs THIS
		// SAME sandbox-agent binary as `<binary> credential-helper get`,
		// as git's OWN child process, inheriting whatever env git itself
		// received here -- i.e. exactly what this Spec.Env carries,
		// nothing more. runCredentialHelper's own boot.Load() call reads
		// NARVI_SESSION_CONFIG via os.Getenv and fails outright if it is
		// absent (see its own "nothing to fetch credentials for" error),
		// so stripping it here would BREAK git authentication for every
		// push -- a real functional regression, not a hardening win. A
		// hand-built allowlist would also risk silently omitting something
		// the real `git` binary or its transport (http/ssh) legitimately
		// needs (PATH, HOME, an ssh-agent socket, ...) that isn't yet
		// enumerated anywhere in this codebase. See gitclone.cloneOne's own
		// identical comment for the clone-side counterpart of this exact
		// reasoning.
	})
	if err != nil {
		return "", fmt.Errorf("spawn git push for %s: %w", repoSpec.Name, err)
	}

	result, waitErr := proc.Wait(pushCtx)
	if waitErr != nil {
		_ = proc.Stop(h.runCtx, h.timeouts.ProcessStopGracePeriod)
		return "", fmt.Errorf("git push %s: did not complete within %s: %w", repoSpec.Name, h.timeouts.RepoCloneTimeout, waitErr)
	}
	if result.Err != nil {
		return "", fmt.Errorf("git push %s: %w", repoSpec.Name, result.Err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("git push %s: exited %d: %s", repoSpec.Name, result.ExitCode, strings.TrimSpace(stderr.String()))
	}

	sha, err := h.headSHA(dir)
	if err != nil {
		return "", fmt.Errorf("determine head sha for %s: %w", repoSpec.Name, err)
	}
	return sha, nil
}

// headSHA runs `git rev-parse HEAD` in dir and returns its trimmed
// stdout -- a very minor, sub-second local git-plumbing call (matching
// platform.Timeouts.RepoSHADiscoveryTimeout's own existing "boot
// fingerprint" precedent exactly, reused rather than duplicated for a
// materially identical class of operation), still run via h.sup (never a
// bare exec.Command) using supervisor.Spec's own new Stdout field.
func (h *commandHandler) headSHA(dir string) (string, error) {
	ctx, cancel := context.WithTimeout(h.runCtx, h.timeouts.RepoSHADiscoveryTimeout)
	defer cancel()

	var stdout bytes.Buffer
	proc, err := h.sup.Spawn(supervisor.Spec{
		Path:   "git",
		Args:   githarden.Args(dir, "rev-parse", "HEAD"),
		Stdout: &stdout,
		// Unlike pushOneRepo's own git push Spawn call just above (which
		// deliberately keeps full env inheritance -- see its own comment),
		// this is a purely local, no-network, no-credential-helper
		// plumbing command: no `-c credential.helper=...` flag is ever set
		// for it, so it has no structural need for NARVI_SESSION_CONFIG
		// (the sandbox's own plaintext bearer token) at all. Tightened for
		// defense in depth.
		Env: supervisor.EnvWithout(boot.SessionConfigEnvVar),
	})
	if err != nil {
		return "", fmt.Errorf("spawn git rev-parse HEAD: %w", err)
	}

	result, waitErr := proc.Wait(ctx)
	if waitErr != nil {
		_ = proc.Stop(h.runCtx, h.timeouts.ProcessStopGracePeriod)
		return "", fmt.Errorf("did not complete within %s: %w", h.timeouts.RepoSHADiscoveryTimeout, waitErr)
	}
	if result.Err != nil {
		return "", result.Err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("exited %d", result.ExitCode)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// sendPushComplete sends a real, CRITICAL sandboxws.PushComplete event --
// MessageId/AckId are freshly minted here (this event ORIGINATES from
// sandbox-agent; it does not echo cmd's own MessageId), matching
// internal/adapters/outbound/opencode/translate.go's own
// translateExecutionComplete precedent for every agent-originated
// critical event exactly ("Deterministic ackId = '{type}:{messageId}'"
// where messageId is THIS event's own, per contracts/sandbox-ws/v1's own
// doc comments).
func (h *commandHandler) sendPushComplete(cmd sandboxws.Push, repos []sandboxws.PushCompleteReposElem) {
	messageID := uuid.NewString()
	msg := sandboxws.PushComplete{
		Type:      "push_complete",
		MessageId: messageID,
		SessionId: cmd.SessionId,
		Gen:       cmd.Gen,
		AckId:     "push_complete:" + messageID,
		Repos:     repos,
	}
	if err := h.bridge.SendCritical(h.runCtx, msg, msg.AckId); err != nil {
		slog.Warn("sandbox-agent: send push_complete over WS bridge failed",
			"messageId", cmd.MessageId, "ackId", msg.AckId, "error", err)
	}
}

// sendPushError mirrors sendPushComplete for the failure path -- a single
// error string, per PushError's own schema (no per-repo breakdown).
func (h *commandHandler) sendPushError(cmd sandboxws.Push, pushErr error) {
	messageID := uuid.NewString()
	msg := sandboxws.PushError{
		Type:      "push_error",
		MessageId: messageID,
		SessionId: cmd.SessionId,
		Gen:       cmd.Gen,
		AckId:     "push_error:" + messageID,
		Error:     pushErr.Error(),
	}
	if err := h.bridge.SendCritical(h.runCtx, msg, msg.AckId); err != nil {
		slog.Warn("sandbox-agent: send push_error over WS bridge failed",
			"messageId", cmd.MessageId, "ackId", msg.AckId, "error", err)
	}
}

// HandleSnapshot implements the real `snapshot` command (§3.2,
// "snapshots & restore", design decision 4): calls the control plane's own
// new snapshot-mint endpoint (internal/sandboxagent/snapshotclient,
// design decision 2 -- the real TakeSnapshot network call can only be
// made by the control plane itself, which alone holds the provider's own
// credentials) to obtain a real, sandbox-confirmed snapshotId, then
// reports it back as a CRITICAL "snapshot_ready" event over the WS bridge
// (mirroring sendPushComplete's own exact call shape: a fresh MessageId/
// AckId minted here, this event originates from sandbox-agent, it does
// not echo cmd's own MessageId as ITS OWN MessageId) -- except CommandMessageId,
// which IS deliberately set to cmd.MessageId verbatim: the control plane's
// own message-id-correlation fix (sessionactor's triggerSnapshotBestEffort/
// handleSnapshotReadyEvent) needs this snapshot_ready echoed back to the
// exact Snapshot command it answers, to tell two attempts on the same live
// sandbox apart (gen alone can't: a snapshot cycle happens within the same
// gen) -- mirroring how a request-id is normally echoed in a response.
//
// On any failure obtaining the id (no live session configured, an invalid
// ControlPlaneWsUrl, the CP request itself failing, or CP returning a
// non-2xx/malformed response): log and return without sending anything --
// design decision 2's own honest, documented limitation: no NACK-shaped
// event exists on the wire to tell the control plane "never mind". HONEST
// GAP (documented, not fixed by this Step -- see sessionactor's own
// triggerSnapshotBestEffort doc comment for the matching statement of the
// same fact from the control-plane side): the control plane's own
// Snapshotting state is only ever recovered by a real snapshot_ready (this
// function never sending one is exactly the case that can't recover it) or
// by revertSnapshotBestEffort's own SendCommand-failure detection -- a
// mint failure THIS function hits AFTER a Snapshot command was already
// successfully delivered is invisible to that detection, so the sandbox is
// left stuck Snapshotting with no watchdog covering that state until a
// later reconnect/restart cycle, or -- more honestly -- until a future
// Step adds a real NACK/timeout mechanism this Step's own plan-row text
// does not ask for (explicitly out of THIS Step's own scope: no new
// dedicated snapshot-timeout timer, no broader NACK mechanism).
//
// §30.4(3)'s own PRIMARY fix ("snapshots must never contain a token,
// enforced at snapshot mint time") lives here, first, before the mint
// request is even built: purge the credential cache
// (credentials.Cache.PurgeAll, the SAME call runBootSequence already
// makes unconditionally at boot, and gitclone.CleanForImageBuild already
// makes on the image-build path) so no cached credential -- read-only or
// write-capable, live-minted or shadow-substituted -- is on disk when the
// provider goes on to actually take the snapshot.
//
// "Is on disk" rather than "can possibly be captured", because this
// purges at the last moment this process controls, not at the instant of
// capture. The provider captures the filesystem some time AFTER the
// snapshot_ready this function sends, and an agent running a git command
// in that window makes the credential helper mint and cache again. What
// bounds that residue is the layer above, not this call: a snapshot taken
// during a live session may legitimately carry a write token, and
// §30.4(3)'s shadow bit is what refuses to restore it into a shadow
// session. A shadow session's own re-mint can only produce the read-only
// substitute. Stating the window rather than implying it is closed is the
// point -- a reader who believes this call is airtight would see no
// reason to keep that bit fail-closed. This runs regardless of the session's own current egress
// mode: a LIVE session's sandbox may legitimately hold a real write
// credential on disk right now, and that is exactly the case this purge
// exists to remove from the snapshot, since a later restore (§30.4(3)'s
// own fail-closed shadow-bit check, app/sessionactor/dispatch.go) might
// land in a SHADOW session with zero control-plane cooperation.
//
// A failed purge ABORTS the snapshot -- the mint is never even attempted,
// and no snapshot_ready is ever sent. Argued from which direction fails
// safe: this function's own existing "no NACK on the wire" gap (see
// above) already means a real Mint failure leaves the control plane stuck
// Snapshotting until a later reconnect/revert cycle recovers it -- an
// honest, already-accepted cost. Proceeding past a FAILED purge, by
// contrast, risks the one outcome this whole Step exists to close: a
// snapshot that captures a leftover write credential from disk, silently,
// with no later signal anywhere that it happened. A stuck Snapshotting
// state is recoverable (a future reconnect, or an operator's own
// intervention); a token baked into a snapshot is not something any later
// step can un-bake. So: proceeding on a failed purge fails OPEN on the
// one guarantee that matters most here, while aborting fails CLOSED at
// the cost this function's own doc comment already accepts elsewhere --
// abort wins.
func (h *commandHandler) HandleSnapshot(_ context.Context, cmd sandboxws.Snapshot) {
	if h.cfg.SessionConfig == nil {
		slog.Warn("sandbox-agent: received snapshot but no live session is configured", "messageId", cmd.MessageId)
		return
	}

	if err := (&credentials.Cache{Dir: h.cfg.CredentialCacheDir}).PurgeAll(); err != nil {
		slog.Error("sandbox-agent: snapshot: purge credential cache before mint failed; aborting snapshot rather than risking a token captured in it",
			"messageId", cmd.MessageId, "error", err)
		return
	}

	client, err := snapshotclient.New(h.cfg.SessionConfig.ControlPlaneWsUrl, h.timeouts.SnapshotMintTimeout)
	if err != nil {
		slog.Warn("sandbox-agent: snapshot: build CP client failed", "messageId", cmd.MessageId, "error", err)
		return
	}

	snapshotID, err := client.Mint(h.runCtx, h.cfg.SessionConfig.SessionId, h.cfg.SessionConfig.SandboxToken, h.cfg.SessionConfig.Gen)
	if err != nil {
		slog.Warn("sandbox-agent: snapshot: mint request to control plane failed", "messageId", cmd.MessageId, "error", err)
		return
	}

	messageID := uuid.NewString()
	commandMessageID := cmd.MessageId
	msg := sandboxws.SnapshotReady{
		Type:             "snapshot_ready",
		MessageId:        messageID,
		SessionId:        cmd.SessionId,
		Gen:              cmd.Gen,
		AckId:            "snapshot_ready:" + messageID,
		SnapshotId:       snapshotID,
		CommandMessageId: &commandMessageID,
	}
	if err := h.bridge.SendCritical(h.runCtx, msg, msg.AckId); err != nil {
		slog.Warn("sandbox-agent: send snapshot_ready over WS bridge failed",
			"messageId", cmd.MessageId, "ackId", msg.AckId, "error", err)
	}
}

// HandleGitSyncComplete observes the control plane's own best-effort
// acknowledgment of a git_sync event (§3.4 "gitstate in-sandbox").
// This sandbox's own git-sync reconciliation (internal/sandboxagent/
// gitclone.SyncAll, driven from runBootSequence below) is entirely
// sandbox-local -- git doesn't need CP permission to run stash/checkout/
// pop -- and git_sync itself carries no ackId (it is a best-effort event,
// not one of the 6 critical types per events.schema.json), so the sandbox
// runs its own reconciliation sequence at its own pace and never blocks or
// waits for this reply before proceeding to the next phase. Log-only is
// this handler's deliberate, final behavior, not a stub: there is nothing
// further for it to do.
func (*commandHandler) HandleGitSyncComplete(_ context.Context, cmd sandboxws.GitSyncComplete) {
	slog.Info("sandbox-agent: received git_sync_complete (observational only; sandbox-side git-sync does not gate on it)",
		"messageId", cmd.MessageId)
}

// setupSandboxAgentOTel wires this binary's own global OTel MeterProvider
// (and TracerProvider) exactly the way cmd/control-plane/main.go's serve()
// already does, via the SAME platform.SetupOTel bootstrap -- this binary was
// the one production caller that never did (§5.3's own "day one, not
// later" gap this Step closed).
//
// A TracerProvider is registered alongside the MeterProvider (platform.
// SetupOTel always sets up both together, matching control-plane's own
// identical call) even though nothing in this binary emits a span today --
// exactly control-plane's own "bootstrap only, no instruments defined here"
// scope note, unchanged for this binary.
//
// No instrument THIS BINARY owns records through either provider today:
// §33.3 deleted sandbox-agent's last two local histogram files
// (internal/sandboxagent/boot/telemetry.go, internal/sandboxagent/
// gitclone/telemetry.go) and replaced the four data points they used to
// record with a best-effort boot_timing event relayed over the WS bridge --
// the control plane does the recording now (internal/app/sessionactor/
// opsmetrics.go), not this process. This call stays anyway, and that is
// deliberate, not a leftover: see the otlpEndpoint paragraph below for why
// installing a REAL (not no-op) MeterProvider/TracerProvider here is
// load-bearing on its own, independent of any histogram this binary itself
// records.
//
// serviceName is fixed ("narvi-sandbox-agent") rather than parameterized:
// there is exactly one production caller (run(), below) and no reason for a
// second, different value to ever exist -- mirroring control-plane's own
// identical fixed-literal call.
//
// otlpEndpoint is hardcoded "" here, never threaded through from any
// config this process might carry: sandbox-agent runs INSIDE the sandbox
// (§33), and giving it any pathway to an operator's real OTLP collector
// would mean an ingestion credential living where customer-directed,
// model-authored code runs -- the exact secret class this codebase strips
// from every child environment (§27.4's removed kube-credential
// subcommand is the precedent, §33.4). §27.6's server-appended egress
// allowlist floor (allowlistFloorHosts) admits the control-plane host plus
// the session's git hosts and nothing else, so a collector would not even
// be reachable if this call somehow acquired one -- but the point is this
// call site must never acquire one in the first place. A bare "" literal
// is not enforced by the compiler or the type system, so
// main_test.go's own TestSetupSandboxAgentOTel_PassesEmptyEndpoint pins
// it at the boundary that actually matters: it intercepts the exact
// otlpEndpoint value THIS call passes to platform.SetupOTel (via the
// setupOTelFn indirection below) and fails if any future edit threads a
// non-empty one in -- e.g. wiring cfg.OTLPEndpoint here to give
// sandbox-agent its own collector would compile, and would pass
// TestSetupSandboxAgentOTel_InstallsRealMeterProvider below unchanged, but
// would fail that pin. THIS is §33.4's real anchor and the actual reason
// setupOTelFn is still called here for a real, working provider even
// though no in-process instrument uses it today: a future sandbox-side
// instrument then attaches to a live MeterProvider immediately, with the
// endpoint already pinned empty, rather than either silently landing on
// the global no-op (as this binary's own now-deleted instruments once
// did) or reopening "should this thread config in" as a live question
// this pin has already answered.
//
// Factored out of run() specifically so it is unit-testable in isolation
// (see main_test.go's own TestSetupSandboxAgentOTel_InstallsRealMeterProvider):
// run() itself blocks on OS signals / a live WS bridge / a real opencode
// spawn, none of which this seam needs or touches.
//
// setupOTelFn is platform.SetupOTel, indirected through a package-level
// var purely so main_test.go can intercept the arguments THIS call
// actually passes without touching the real global OTel SDK state --
// production code never reassigns it; it is exactly platform.SetupOTel at
// every real call.
var setupOTelFn = platform.SetupOTel

func setupSandboxAgentOTel(ctx context.Context) (shutdown func(context.Context) error, err error) {
	return setupOTelFn(ctx, "narvi-sandbox-agent", "")
}

// shutdownSandboxAgentOTel bounds one call to shutdown (setupSandboxAgentOTel's
// own returned func) by timeout, against a fresh background context -- never
// run()'s own long-lived ctx, which is already canceled by the time run()'s
// deferred call reaches this function (see that defer's own comment).
//
// Audit-remediation batch B7 (MEDIUM finding): run() previously called
// shutdownOTel(context.Background()) directly, with nothing in-process
// bounding it. cmd/control-plane/main.go's serve() had the IDENTICAL
// no-timeout shape at the time and was deliberately left unchanged by that
// fix -- that binary is a long-running daemon that would eventually get
// another periodic metric export anyway even if one flush somehow hung or
// was missed, and a bare stdout write essentially never hangs regardless.
// sandbox-agent had no such fallback even then: its own package doc
// comment (setupSandboxAgentOTel, above run()) calls this shutdown "the
// last chance before the process exits" for a single boot+session process.
// If os.Stdout backpressures (a slow/blocked log collector, a full pipe
// buffer under load, ...) while metric.NewPeriodicReader/tracerProvider.
// Shutdown are flushing, the underlying write blocks synchronously -- with
// no bound of its own, that hangs sandbox teardown indefinitely, past
// whatever grace period the orchestrator expects, and a subsequent
// force-kill would then discard whatever this flush had not yet gotten out
// anyway, defeating the point of flushing at all -- true of batch B7's own
// original hook-rerun-duration histogram back when this reasoning was
// written, and unchanged in shape now that §33.3 has deleted that
// histogram and this binary registers no sandbox-side instrument of its
// own: the same bound still protects whatever a future instrument records
// here, and today it protects nothing but the timing of an already-empty
// flush.
//
// §33 gave control-plane's own identical-looking call this SAME bound
// (cmd/control-plane/main.go's shutdownControlPlaneOTel) once an OTLP
// exporter's own flush became a real network call with a real hang mode a
// stdout write never had -- see that function's own doc comment for why
// the asymmetry this comment used to describe no longer holds. sandbox-
// agent's own otlpEndpoint stays hardcoded "" regardless (setupSandboxAgentOTel's
// own doc comment, above), so this function's own reasoning is unchanged by
// that: still bounding a stdout flush, still "the last chance before the
// process exits" with no periodic-export fallback to lean on.
//
// Factored out of run() specifically so it is unit-testable in isolation
// (see main_test.go's own TestShutdownSandboxAgentOTel_BoundsAHungShutdown),
// exactly like setupSandboxAgentOTel's own precedent just above.
func shutdownSandboxAgentOTel(shutdown func(context.Context) error, timeout time.Duration) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return shutdown(shutdownCtx)
}

// run mirrors cmd/control-plane/main.go's serve() shape: a thin main()
// dispatches to this testable, error-returning function.
// fetchProviderCredentialSpawnEnv ("provider credential
// injection", §25.1/§25.3) fetches this session's own resolved provider
// credentials from CP ONCE (POST /sessions/{id}/provider-credentials) --
// callers split the result by kind: providerCredentialSpawnEnv (api-kind,
// unchanged) and providerCredentialOAuthSets (oauth-kind,
// §29.6) below. A single fetch, not two, so both env injection
// and the post-spawn PUT /auth/{providerID} call see the EXACT SAME
// resolved snapshot -- two independent fetches could race and observe
// different results if a credential changed between them. Only ever
// called when cfg.SessionConfig is non-nil (the caller's own enclosing
// branch already guarantees this).
//
// Deliberately BEST-EFFORT, never fatal to boot: the overwhelming common
// case is zero credentials configured for a session (today's exact,
// unchanged behavior -- `opencode serve` simply inherits sandbox-agent's
// own ambient OS environment, e.g. whatever provider key the sandbox
// image itself was baked with, exactly as before this Step), and a
// network hiccup fetching this OPTIONAL, additive override must never
// turn into a hard boot failure over what is, for most sessions, a no-op
// anyway. A failure here is logged (Warn, never Error -- this is an
// expected, tolerated degraded path, not a genuine server malfunction)
// and returns nil -- both callers already treat nil identically to
// "nothing resolved".
//
// No disk cache (unlike internal/sandboxagent/credentials.Cache, the SCM
// credential-helper's own flock'd cache): a provider credential is needed
// exactly ONCE, at this exact spawn moment, never re-invoked repeatedly
// outside sandbox-agent's own process lifetime the way a git credential
// helper is (git itself calls the credential helper on every fetch/push) --
// there is nothing here for a disk cache to usefully amortize, so an
// in-memory-only fetch is the simpler, sufficient choice.
//
// Never logs any resolved credential VALUE, at any point -- only provider
// NAMES (never secret material) for observability, matching
// tokenencrypt.go's own "never log plaintext, key, or ciphertext"
// discipline exactly.
func fetchProviderCredentials(ctx context.Context, cfg boot.Config, timeout time.Duration) map[string]credentials.AuthValue {
	client, err := credentials.NewCPClient(cfg.SessionConfig.ControlPlaneWsUrl, timeout)
	if err != nil {
		slog.Warn("sandbox-agent: build provider-credentials CP client failed, spawning opencode with no resolved provider credential", "error", err)
		return nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resolved, err := client.FetchProviderCredentials(fetchCtx, cfg.SessionConfig.SessionId, cfg.SessionConfig.SandboxToken, cfg.SessionConfig.Gen)
	if err != nil {
		slog.Warn("sandbox-agent: fetch provider credentials failed, spawning opencode with no resolved provider credential", "error", err)
		return nil
	}

	if len(resolved) > 0 {
		providerNames := make([]string, 0, len(resolved))
		for provider := range resolved {
			providerNames = append(providerNames, provider)
		}
		sort.Strings(providerNames)
		slog.Info("sandbox-agent: resolved provider credentials", "providers", providerNames)
	}
	return resolved
}

// providerCredentialSpawnEnv maps every "api"-kind entry in resolved onto
// its own OpenCode env-var name(s) (internal/domain/providercredential.
// EnvVarNames), building the "NAME=VALUE" entries opencodeproc.Spawn's own
// providerCredentialEnv parameter expects -- §25.1's own original
// behavior, unchanged, now just fed from the shared fetch above rather
// than fetching for itself. An "oauth"-kind entry contributes NOTHING
// here (§29.6: an oauth credential is delivered via PUT /auth/{providerID}
// instead, never an env var -- see providerCredentialOAuthSets below).
func providerCredentialSpawnEnv(resolved map[string]credentials.AuthValue) []string {
	var env []string
	for provider, value := range resolved {
		if value.Type != "api" || value.Key == nil {
			continue
		}
		for _, name := range providercredential.EnvVarNames(providercredential.Provider(provider)) {
			env = append(env, name+"="+*value.Key)
		}
	}
	return env
}

// providerCredentialOAuthSets returns every "oauth"-kind entry in
// resolved, unchanged -- §8.8's own new split (§29.6): the caller
// (run(), below) PUTs each to OpenCode's own auth store via
// agentRuntime.SetOAuthAuth, sequenced after Spawn reports healthy and
// before the WS bridge accepts its first command.
func providerCredentialOAuthSets(resolved map[string]credentials.AuthValue) map[string]credentials.AuthValue {
	oauth := make(map[string]credentials.AuthValue)
	for provider, value := range resolved {
		if value.Type == "oauth" {
			oauth[provider] = value
		}
	}
	return oauth
}

func run() error {
	// boot.Load() is the earliest possible failure -- before any logging
	// setup even -- exactly like control-plane's own platform.Load()
	// failure path.
	cfg, err := boot.Load()
	if err != nil {
		return fmt.Errorf("sandbox-agent: load config: %w", err)
	}

	logger := platform.NewLogger(os.Stdout, cfg.LogLevel)
	slog.SetDefault(logger)

	// sandbox-agent has no env-driven timeout-override mechanism (neither
	// does control-plane's own platform.Load(), which also always uses
	// DefaultTimeouts() verbatim), so reuse the shared defaults directly.
	timeouts := platform.DefaultTimeouts()

	// §5.3: "sandbox-agent logs a boot fingerprint first" -- this MUST be
	// the very first line this binary emits; nothing above this point
	// logs anything. openCodeVersion is necessarily "" here (§7's own
	// discovery requires the OpenCode server to already be running, which
	// hasn't happened yet) -- see the supplementary fingerprint log below,
	// once it has.
	fingerprint := boot.CollectFingerprint(cfg, timeouts.RepoSHADiscoveryTimeout, "")
	slog.Info("sandbox-agent: boot fingerprint",
		"agent_version", fingerprint.AgentVersion,
		"image_digest", fingerprint.ImageDigest,
		"boot_mode", string(fingerprint.BootMode),
		"repo_shas", fingerprint.RepoSHAs,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// §5.3 "day one, not later": cmd/control-plane/main.go used to be the
	// ONLY caller of platform.SetupOTel, so this binary ran its entire
	// life against the no-op global MeterProvider/TracerProvider every
	// process starts with by default. Wired here, exactly mirroring
	// control-plane's own bootstrap precedent (same function, same
	// shutdown/flush shape) -- see this call's own deferred shutdown below
	// for why that shape (not just construction) is load-bearing, and
	// setupSandboxAgentOTel's own doc comment for why a REAL provider is
	// installed at all when this binary owns no instrument today (§33.4:
	// it is the anchor for "sandbox-agent never reaches a collector", not
	// a histogram).
	//
	// Registered BEFORE sup/opencodeproc/gitclone/boot.RunBoot ever run:
	// whatever hook-timing/git-timing data those produce is relayed to the
	// control plane over the WS bridge (onHookRerunTiming/onGitSync below),
	// never recorded against a local OTel instrument -- there is no
	// lazily-resolved histogram in this process for this ordering to
	// protect. Registered first anyway, as early as practical, so that a
	// future sandbox-side instrument (should one ever get added here)
	// finds a working MeterProvider already installed rather than racing
	// its own first use against this call.
	//
	// Factored into its own tiny function (setupSandboxAgentOTel, below)
	// rather than inlined here, specifically so it is unit-testable in
	// isolation: run() itself is not (it blocks on OS signals / a live WS
	// bridge / a real opencode spawn), but this seam alone is exactly the
	// piece §5.3's original audit finding was about ("cmd/sandbox-agent
	// never calls platform.SetupOTel") -- see main_test.go's own
	// TestSetupSandboxAgentOTel_InstallsRealMeterProvider and
	// TestSetupSandboxAgentOTel_PassesEmptyEndpoint.
	shutdownOTel, err := setupSandboxAgentOTel(ctx)
	if err != nil {
		return fmt.Errorf("sandbox-agent: setup otel: %w", err)
	}
	defer func() {
		// Deliberately a fresh background context, not ctx: by the time this
		// deferred call runs, ctx is already canceled (that's exactly what
		// triggers shutdown), and a canceled context would make the flush
		// itself fail immediately -- same reasoning as control-plane's own
		// identical deferred shutdownOTel call.
		//
		// Registered THIRD from the top of run() (after `defer stop()`),
		// meaning Go's own LIFO defer ordering runs it LAST of every defer
		// registered in run() (agentRuntime.Close(), the shutdownCtx cancel,
		// ...) -- i.e. as late as possible, after supervised-process shutdown
		// and WS-bridge drain have both already completed. sandbox-agent's
		// own lifetime is a single boot+session, not control-plane's
		// long-running daemon -- there is no "next periodic export" to rely
		// on if this doesn't flush now: this IS the last chance before the
		// process exits. control-plane's own equivalent call gets another
		// periodic export anyway even if one flush were somehow missed, but
		// (as of §33) is bounded identically to this one regardless, now
		// that its own flush can be a real network call too.
		//
		// Audit-remediation batch B7: bounded by timeouts.OTelShutdownTimeout
		// (a fresh WithTimeout over the same background context above) --
		// see shutdownSandboxAgentOTel's own doc comment for why a hang in
		// the stdout metric/trace exporter's own flush (a slow/blocked log
		// collector, a full pipe buffer under load, ...) must cost this
		// process's exit at most this bounded amount, never an unbounded
		// wait, since there is no future periodic export here to fall back
		// on the way there is for control-plane's own long-running daemon.
		if err := shutdownSandboxAgentOTel(shutdownOTel, timeouts.OTelShutdownTimeout); err != nil {
			slog.Error("sandbox-agent: otel shutdown failed", "error", err)
		}
	}()

	sup := supervisor.New()

	// agentRuntime is nil exactly when cfg.SessionConfig is nil (the
	// common dev/test case with no real session) -- there is nothing to
	// prompt at all, matching this Step's own "no real session, no work"
	// precedent from every prior sandbox-agent Step. Spawned BEFORE the WS
	// bridge starts accepting commands (below): a "prompt" command can
	// arrive as soon as the bridge connects, concurrently with the boot/
	// clone sequence (§6.1's own design), so the adapter must already
	// exist by then -- see this file's own package doc comment for the
	// full reasoning.
	var agentRuntime *opencode.Adapter
	// budgetServer/reviewCostBudgetURL are §26.5's own addition (§26.7/
	// §26.9): budgetServer is sandbox-agent's own FIRST HTTP server (a
	// tiny, loopback-only listener serving GET /review-cost-budget,
	// reviewcostbudgetserver.go) -- nil exactly when cfg.SessionConfig is
	// nil, mirroring agentRuntime's own identical "no real session, no
	// work" precedent immediately above, since there is no turn for it to
	// ever report spend for. reviewCostBudgetURL is that server's own
	// real, resolved http://127.0.0.1:<port>/review-cost-budget URL, kept
	// at this outer scope so HandlePrompt's own commandHandler (constructed
	// further below, once bridge exists) can substitute it for
	// review.ReviewCostBudgetToolURLPlaceholder in a review turn's own
	// prompt text, exactly like SessionConfig/timeouts/sup are already
	// threaded into commandHandler for the SAME reason.
	var budgetServer *reviewCostBudgetServer
	var reviewCostBudgetURL string
	// resolvedCredentials is populated inside the SAME block below and
	// consumed twice: providerCredentialSpawnEnv (api-kind, feeding
	// opencodeproc.Spawn's own env) here, and providerCredentialOAuthSets
	// (oauth-kind §29.6) in the SECOND cfg.SessionConfig != nil
	// block below, once bridge exists -- declared at this outer scope
	// (mirroring agentRuntime's own identical need) so both call sites see
	// the exact same fetched snapshot.
	var resolvedCredentials map[string]credentials.AuthValue
	// sandboxSecretEnv (§27.1, adversarial-review HIGH fix) and
	// bootDegradeNotes (§27.1, adversarial-review LOW fix) are populated
	// inside the SAME block below and consumed AFTER it closes: this
	// binary's own single opencodeproc.Spawn call sits inside this block
	// (sandboxSecretEnv feeds it directly), but runBootSequence (which
	// threads sandboxSecretEnv on into boot.RunBoot for hooks/services.yml,
	// and folds bootDegradeNotes into the generated AGENTS.md manifest) is
	// called LATER, OUTSIDE this block -- both declared at this outer
	// scope for the same reason resolvedCredentials/agentRuntime already
	// are.
	var sandboxSecretEnv []string
	var bootDegradeNotes []string
	// cloudIdentityStates/cloudIdentityMintClient ("cloud
	// identity: sandbox-side consumption + kubeconfig injection", §27.3)
	// are populated inside the SAME block below and consumed LATER,
	// outside it, by the background refresh loop's own group.Go
	// registration (alongside bridge.Run/budgetServer.Serve) -- the exact
	// same "declared at this outer scope so a later, unconditional section
	// of run() can see it" shape sandboxSecretEnv/bootDegradeNotes/
	// resolvedCredentials/agentRuntime already establish, immediately
	// above. cloudIdentityMintClient stays its own zero value when
	// cfg.SessionConfig is nil -- harmless, since cloudIdentityStates is
	// then also nil/empty and the refresh loop's own registration is
	// itself gated on cfg.SessionConfig != nil (see that call site's own
	// comment, below). Since this fix (adversarial-review HIGH), this
	// slice also carries at most one §27.4 AuthKindOIDC cluster-binding
	// entry (kubeconfig.go's own applyClusterBinding, appended at its own
	// call site below) alongside every §27.3 cloud_identity_bindings
	// entry -- ONE refresh loop, ONE slice, for both mechanisms; see
	// applyClusterBinding's own doc comment for why that rung's own token
	// needs the SAME half-life refresh every other entry here already
	// gets.
	var cloudIdentityStates []cloudIdentityBindingState
	var cloudIdentityMintClient credentials.CPClient
	if cfg.SessionConfig != nil {
		// opencodeproc.Spawn's own Dir is cfg.WorkspaceDir -- normally
		// created by gitclone.CloneAll (runBootSequence, below), which
		// hasn't run yet at this point. Ensure it exists NOW instead of
		// letting the spawn fail on a nonexistent chdir target; idempotent
		// (os.MkdirAll no-ops if it already exists) and harmless for
		// CloneAll's own later MkdirAll call.
		if err := os.MkdirAll(cfg.WorkspaceDir, 0o755); err != nil {
			return fmt.Errorf("sandbox-agent: create workspace dir: %w", err)
		}

		// (§27.3/§27.4): wipe cloudIdentityDir ENTIRELY before
		// this boot writes anything else there -- this Step's own gap-2
		// resolution (a snapshot_restore boot's own filesystem can already
		// hold stale token files/a stale kubeconfig from whatever boot
		// last wrote them; see resetCloudIdentityDir's own doc comment,
		// cloudidentity.go, for the full "why unconditional, why every
		// boot mode" reasoning). Ordered before EVERY other boot-time
		// write in this block, including the sentinel-fix/review-sub-agent
		// opencode.json merges immediately below -- none of them touch
		// cloudIdentityDir, so ordering relative to THEM doesn't matter,
		// but this must land before this file's own later cloud-identity/
		// kubeconfig population block (further down this same function).
		resetCloudIdentityDir(cloudIdentityDir)

		// (§17.2): for a sentinel-auto-fix child session
		// (SessionConfig.CapabilityRestricted), write the glob-restricted
		// "sentinel-fix" OpenCode agent config into the workspace BEFORE
		// spawning `opencode serve` below -- OpenCode reads its own config
		// once, at boot (opencode/sentinelfixagent.go's own top doc
		// comment: "verified live... loading path=.../opencode.json" at
		// startup), so this must land before Spawn, not after. Any
		// existing opencode.json at this path (a repo's own committed
		// config -- unusual this early, since gitclone.CloneAll hasn't
		// run yet, but not impossible for a warm/resumed sandbox reusing
		// the same workspace dir) is merged, never clobbered wholesale
		// (MergeSentinelFixAgentConfig's own doc comment). A malformed
		// existing file, or any other write failure, is logged and
		// otherwise NON-FATAL -- this session then simply runs with
		// today's ordinary, unrestricted agent selection instead of a
		// hard boot failure over a defense-in-depth layer (§17.4's own
		// post-hoc diff-scope check is the OTHER, independent layer that
		// still applies regardless).
		if cfg.SessionConfig.CapabilityRestricted {
			configPath := filepath.Join(cfg.WorkspaceDir, "opencode.json")
			existing, readErr := os.ReadFile(configPath)
			if readErr != nil && !os.IsNotExist(readErr) {
				slog.Warn("sandbox-agent: read existing opencode.json for sentinel-fix agent merge failed, proceeding without a restricted agent config", "error", readErr)
			} else {
				merged, mergeErr := opencode.MergeSentinelFixAgentConfig(existing)
				if mergeErr != nil {
					slog.Warn("sandbox-agent: merge sentinel-fix agent config failed, proceeding without a restricted agent config", "error", mergeErr)
				} else if writeErr := os.WriteFile(configPath, merged, 0o644); writeErr != nil {
					slog.Warn("sandbox-agent: write sentinel-fix agent config failed, proceeding without a restricted agent config", "error", writeErr)
				}
			}
		}

		// (§26.4/§26.6): register the three review sub-agents
		// (architecture-scribe, counter-reviewer, fact-check) into the
		// SAME workspace opencode.json, UNCONDITIONALLY -- unlike the
		// sentinel-fix block immediately above (gated on
		// CapabilityRestricted, one kind of session only), every session
		// gets these three custom agent DEFINITIONS: they are inert unless
		// a turn's own prompt actually instructs the agent to spawn one via
		// the "task" tool (review.RenderTurnPrompt's own
		// subAgentOrchestrationInstructions, which ONLY a review turn's
		// prompt ever renders, and only light-path-appropriate ones at
		// that) -- see opencode/reviewsubagents.go's own
		// MergeReviewSubAgentsConfig doc comment for the full "why
		// unconditional is correct and not a rigor leak". Read-merge-write
		// AGAIN here (rather than folding into the block above) because
		// this must apply to EVERY session, review-restricted or not,
		// while the sentinel-fix block above must stay conditioned on
		// CapabilityRestricted alone -- two independent gates, so two
		// independent (but sequential, same file) merges, mirroring how
		// §27.2's own future OpenCode-config-storage injection is already
		// expected to layer a THIRD merge onto this same file later.
		// Same non-fatal-on-failure posture as the block above: a failed
		// read/merge/write here degrades to "this session's review turns
		// simply cannot spawn these three sub-agents", never a boot
		// failure.
		{
			configPath := filepath.Join(cfg.WorkspaceDir, "opencode.json")
			existing, readErr := os.ReadFile(configPath)
			if readErr != nil && !os.IsNotExist(readErr) {
				slog.Warn("sandbox-agent: read existing opencode.json for review sub-agents merge failed, proceeding without them", "error", readErr)
			} else {
				var counterReviewerModel string
				if cfg.SessionConfig.ReviewCounterReviewerModel != nil {
					counterReviewerModel = *cfg.SessionConfig.ReviewCounterReviewerModel
				}
				merged, mergeErr := opencode.MergeReviewSubAgentsConfig(existing, counterReviewerModel)
				if mergeErr != nil {
					slog.Warn("sandbox-agent: merge review sub-agents config failed, proceeding without them", "error", mergeErr)
				} else if writeErr := os.WriteFile(configPath, merged, 0o644); writeErr != nil {
					slog.Warn("sandbox-agent: write review sub-agents config failed, proceeding without them", "error", writeErr)
				}
			}
		}

		// §27.1 ("sandbox secrets & opencode config", §27.1/§27.2):
		// resolve this session's own general sandbox secrets and OpenCode
		// config documents BEFORE spawning `opencode serve` and BEFORE the
		// boot sequence's own first hook run (runBootSequence, below) --
		// §27.1's own explicit ordering requirement ("sandbox-agent
		// fetches before the first hook"). Both fetches are deliberately
		// best-effort (see fetchSandboxSecrets'/fetchOpenCodeConfig's own
		// doc comments) and THREADED explicitly into sandboxSecretEnv
		// (adversarial-review HIGH fix -- an earlier version instead
		// os.Setenv'd onto sandbox-agent's own process; see
		// sandboxsecrets.go's own top doc comment for the full incident
		// this fixes) -- so sandboxSecretEnv must be fully built here,
		// ahead of opencodeproc.Spawn (below) and runBootSequence (which
		// threads it on into boot.RunBoot for hooks/services.yml).
		resolvedSandboxSecrets, sandboxSecretsFetchOK := fetchSandboxSecrets(ctx, cfg, timeouts)
		sandboxSecretEnv = sandboxSecretSpawnEnv(resolvedSandboxSecrets)
		if !sandboxSecretsFetchOK {
			// §27.1: "recorded in the boot log [already done, inside
			// fetchSandboxSecrets itself] and AGENTS.md" (adversarial-review
			// LOW fix) -- folded into the generated manifest by
			// runBootSequence's own gitclone.WriteAgentsManifest call,
			// below, so the agent that boots into this session is told
			// plainly when its secrets are missing rather than silently
			// misbehaving.
			bootDegradeNotes = append(bootDegradeNotes, "sandbox secrets: boot-time fetch failed after retrying; this session booted with NO sandbox secrets injected (warn-and-continue degrade policy, §27.1) -- env vars a repo's setup.sh/start.sh/services.yml or opencode may normally expect from a configured sandbox secret may be missing")
		}

		// The RUNTIME's home, not this process's own. §27.2's global
		// OpenCode configuration document is written here for the agent
		// runtime to read, and the runtime now runs under a different uid
		// (§30.5): sandbox-agent is root in production, its home is /root,
		// and /root is not traversable by another uid -- so a
		// world-readable document written there is unreadable anyway, and
		// the runtime would simply come up without its configuration.
		homeDir, homeErr := boot.EnsureRuntimeHome(cfg.WorkspaceDir, cfg.RuntimeUID, cfg.RuntimeGID)
		if homeErr != nil {
			slog.Warn("sandbox-agent: prepare runtime home failed, skipping opencode global config injection", "error", homeErr)
		} else {
			openCodeConfigDelivery, openCodeConfigFetchOK := fetchOpenCodeConfig(ctx, cfg, timeouts)
			openCodeConfigEnv := applyOpenCodeConfig(openCodeConfigDelivery, openCodeConfigFetchOK, homeDir, openCodeEnvironmentConfigPath)
			// §27.1's own explicit ordering ("appended before
			// providerCredentialEnv") applies to the WHOLE sandboxSecretEnv
			// slice opencodeproc.Spawn receives, not just the general
			// sandbox_secrets rows -- OPENCODE_CONFIG is disjoint from both
			// (internal/domain/sandboxsecret's own OPENCODE_ reservation),
			// so append order here is a documentation nicety, not a
			// collision-avoidance requirement.
			sandboxSecretEnv = append(sandboxSecretEnv, openCodeConfigEnv...)
			if !openCodeConfigFetchOK {
				bootDegradeNotes = append(bootDegradeNotes, "opencode config: boot-time fetch failed after retrying; this session booted with whatever OpenCode config document (if any) was already on disk from a prior boot, never a fresh one (warn-and-continue degrade policy, §27.1/§27.2)")
			}
		}

		// ("cloud identity: sandbox-side consumption + kubeconfig
		// injection", §27.3/§27.4): resolve this session's own cloud-
		// identity bindings + cluster binding, mint one token per binding
		// (plus, for an AuthKindOIDC cluster binding, its own token --
		// applyClusterBinding's own doc comment), and render a kubeconfig
		// -- BEFORE spawning `opencode serve` (below) and BEFORE the boot
		// sequence's own first hook run (runBootSequence), the SAME
		// ordering §27.1 already requires of sandbox secrets/OpenCode
		// config. resolvedSandboxSecrets (already fetched, above) is
		// reused directly for the AuthKindStatic kubeconfig rung's own
		// sandbox_secrets lookup -- no second CP round trip. Every failure
		// along the way is warn-and-continue, folded into bootDegradeNotes
		// exactly like every other boot-time fetch in this block (§27.1's
		// own "recorded in the boot log and AGENTS.md" requirement,
		// extended here).
		cloudIdentityDelivery, cloudIdentityFetchOK := fetchCloudIdentityConfig(ctx, cfg, timeouts)
		if !cloudIdentityFetchOK {
			bootDegradeNotes = append(bootDegradeNotes, "cloud identity/kubeconfig: boot-time fetch failed after retrying; this session booted with NO cloud identity env vars or kubeconfig injected (warn-and-continue degrade policy, §27.3/§27.4)")
		} else {
			client, clientErr := credentials.NewCPClient(cfg.SessionConfig.ControlPlaneWsUrl, timeouts.CloudIdentityTokenMintTimeout)
			if clientErr != nil {
				slog.Warn("sandbox-agent: build cloud-identity-token CP client failed, skipping cloud identity/kubeconfig injection", "error", clientErr)
				bootDegradeNotes = append(bootDegradeNotes, "cloud identity/kubeconfig: could not build a CP client; this session booted with NO cloud identity env vars or kubeconfig injected (warn-and-continue degrade policy, §27.3/§27.4)")
			} else {
				cloudIdentityMintClient = client
				var cloudIdentityEnv []string
				cloudIdentityEnv, cloudIdentityStates = populateCloudIdentityTokenFiles(ctx, cfg, timeouts, client, cloudIdentityDelivery.Bindings, cloudIdentityDir)
				// applyClusterBinding also mints (AuthKindOIDC only) via
				// the SAME client/session identity -- clusterBindingState
				// is non-nil exactly when that rung's own mint succeeded,
				// and MUST be folded into cloudIdentityStates so
				// runCloudIdentityRefreshLoop (below, this SAME function)
				// keeps refreshing it at half-life like every other entry
				// -- see applyClusterBinding's own doc comment.
				kubeconfigEnv, clusterBindingState := applyClusterBinding(ctx, client, cfg.SessionConfig.SessionId, cfg.SessionConfig.SandboxToken, cfg.SessionConfig.Gen, cloudIdentityDelivery.ClusterBinding, resolvedSandboxSecrets, timeouts, cloudIdentityDir)
				if clusterBindingState != nil {
					cloudIdentityStates = append(cloudIdentityStates, *clusterBindingState)
				}
				sandboxSecretEnv = append(sandboxSecretEnv, cloudIdentityEnv...)
				sandboxSecretEnv = append(sandboxSecretEnv, kubeconfigEnv...)
			}
		}

		// §25.1 ("provider credential injection", §25.1/§25.3): resolve
		// this session's own provider credentials (repo/environment/global/
		// user scoped, most-specific-wins) BEFORE spawning `opencode serve`
		// -- see fetchProviderCredentials' own doc comment for why this is
		// deliberately best-effort (nil on any failure, never fatal to
		// boot) and why it is fetched exactly ONCE here for both the
		// api-kind env-var injection below and the oauth-kind PUT
		// /auth/{providerID} call once bridge exists (§29.6).
		resolvedCredentials = fetchProviderCredentials(ctx, cfg, timeouts.ProviderCredentialFetchTimeout)
		providerCredentialEnv := providerCredentialSpawnEnv(resolvedCredentials)

		// runtimeCredential (TECHNICAL_PLAN.md §30.5, "OS-level isolation
		// between sandbox-agent and the agent runtime"): cfg.RuntimeUID/
		// RuntimeGID are boot.Load's own fail-fast-validated values
		// (non-numeric or explicit-0/root already refused there, before
		// this line is ever reached) -- built into a *syscall.Credential
		// HERE, this process's own one and only call site that needs one,
		// rather than in the boot package (see Config.RuntimeUID's own doc
		// comment for why).
		//
		// NoSetGroups is true ONLY when cfg.RuntimeUID/RuntimeGID exactly
		// equal this process's OWN current uid/gid -- i.e. no actual
		// identity change is being requested at all, the documented
		// escape hatch for running the real binary unprivileged (a real
		// dev machine, an integration test spawning this real binary as
		// the ordinary CI/dev user -- see
		// cmd/sandbox-agent/push_integration_test.go's own
		// runSandboxAgent, which sets NARVI_RUNTIME_UID/GID to its own
		// os.Getuid()/os.Getgid() for exactly this reason). Confirmed
		// live: Go's own exec path calls setgroups() whenever Credential
		// is non-nil UNLESS NoSetGroups is true, and setgroups() itself
		// always requires privilege regardless of whether it changes
		// anything -- so even a self-uid/gid Credential fails
		// unprivileged without this (see
		// internal/sandboxagent/supervisor.Spec.Credential's own doc
		// comment for the same finding). In the real, privileged
		// production path, cfg.RuntimeUID/RuntimeGID are (by construction
		// -- see boot's own RuntimeUIDIsRootError/RuntimeGIDIsRootError,
		// which refuse 0/root, and the 65534 default, essentially never
		// sandbox-agent's own real uid) never equal to this process's own
		// identity, so this condition never triggers there:
		// NoSetGroups stays false and supplementary groups are genuinely
		// cleared as part of the real privilege drop, exactly as wanted.
		runtimeCredential := runtimeCredentialFor(cfg)

		// HOME must name the runtime's OWN home, or everything the runtime
		// resolves relative to it lands somewhere it cannot reach: it
		// inherits this process's environment, and this process is root in
		// production. Appended after the other environment slices for the
		// same reason they are ordered that way -- a later assignment wins,
		// and this one has to.
		// Copied rather than appended in place: sandboxSecretEnv is used
		// elsewhere, and appending to it could share or clobber its backing
		// array depending on capacity.
		runtimeEnv := make([]string, 0, len(sandboxSecretEnv)+1)
		runtimeEnv = append(runtimeEnv, sandboxSecretEnv...)
		runtimeEnv = append(runtimeEnv, "HOME="+boot.RuntimeHomePath(cfg.WorkspaceDir))

		result, spawnErr := opencodeproc.Spawn(ctx, sup, cfg.WorkspaceDir, providerCredentialEnv, runtimeEnv,
			runtimeCredential, timeouts.OpenCodeReadinessTimeout, timeouts.OpenCodeReadinessPollInterval)
		if spawnErr != nil {
			// Best-effort cleanup of whatever sup may already be tracking
			// -- opencodeproc.Spawn's own internal Supervisor.Spawn call
			// registers the process before its readiness check even
			// starts, so a timed-out or crashed process is still known to
			// sup here -- before failing fast, the same "boot.Load() is
			// the earliest possible failure" shape as this function's own
			// very first error path.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), timeouts.SupervisorShutdownTimeout)
			_ = sup.StopAll(shutdownCtx, timeouts.ProcessStopGracePeriod)
			cancel()
			return fmt.Errorf("sandbox-agent: spawn opencode: %w", spawnErr)
		}

		agentRuntime = opencode.New(result.BaseURL, timeouts.SSEInactivityTimeout,
			timeouts.OpenCodeSSEReconnectInterval, timeouts.OpenCodeRequestTimeout,
			timeouts.OpenCodeSummarizeTimeout, timeouts.OpenCodeTransientRetryBackoff,
			cfg.SessionConfig.CapabilityRestricted)
		defer agentRuntime.Close()

		// §7: "Pin the OpenCode version in the image; record it in the
		// boot fingerprint" -- the FIRST fingerprint log (above)
		// necessarily reported this as empty; now that OpenCode has
		// actually been spawned, log a SECOND, supplementary line -- the
		// SAME "log first with what's known, then a supplementary line
		// once more is known" pattern §6.4 already established for
		// repo_shas (see runBootSequence's own post-clone fingerprint
		// log).
		postSpawnFingerprint := boot.CollectFingerprint(cfg, timeouts.RepoSHADiscoveryTimeout, result.Version)
		slog.Info("sandbox-agent: boot fingerprint (post-opencode-spawn)",
			"opencode_version", postSpawnFingerprint.OpenCodeVersion)

		// (§26.7/§26.9): start the review-cost-budget loopback
		// server NOW -- agentRuntime already exists (its own
		// CurrentTurnSpentUSD method is this server's one data source), and
		// this must land before ANY "prompt" command can possibly arrive
		// (the bridge below has not started accepting commands yet), so a
		// review turn's own prompt substitution (renderReviewCostBudgetToolPromptText,
		// HandlePrompt below) always has a real, already-bound URL to
		// resolve the placeholder against. A failure here is treated
		// exactly like a failed opencode spawn just above: best-effort
		// cleanup of whatever sup is already tracking, then a hard boot
		// failure -- never a silently-degraded review session that renders
		// the raw, unresolved placeholder into a prompt an agent could
		// never usefully call.
		var startErr error
		budgetServer, startErr = startReviewCostBudgetServer(agentRuntime.CurrentTurnSpentUSD, timeouts.ReviewCostBudgetServerReadHeaderTimeout)
		if startErr != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), timeouts.SupervisorShutdownTimeout)
			_ = sup.StopAll(shutdownCtx, timeouts.ProcessStopGracePeriod)
			cancel()
			return fmt.Errorf("sandbox-agent: start review-cost-budget server: %w", startErr)
		}
		reviewCostBudgetURL = budgetServer.URL()
	}

	// bridge/handler are nil exactly when cfg.SessionConfig is nil --
	// everything below that branches on "bridge != nil" preserves today's
	// original no-bridge behavior unchanged in that case. sandboxID is
	// boot.Load()'s own resolveSandboxID value -- see Config.SandboxID's
	// own doc comment for where it really comes from now.
	//
	// handler is constructed with adapter already set, passed to
	// wsbridge.New as the CommandHandler interface value, THEN gets
	// .bridge set on that SAME pointer once Bridge exists -- see
	// commandHandler's own doc comment for why this two-phase
	// construction is necessary (Bridge and commandHandler each need a
	// reference to the other).
	var bridge *wsbridge.Bridge
	var handler *commandHandler
	if cfg.SessionConfig != nil {
		handler = &commandHandler{adapter: agentRuntime, runCtx: ctx, cfg: cfg, timeouts: timeouts, sup: sup, reviewCostBudgetURL: reviewCostBudgetURL}
		bridge = wsbridge.New(*cfg.SessionConfig, cfg.SandboxID, cfg.AgentVersion, cfg.ImageDigest, handler,
			timeouts.SandboxWSDialTimeout, timeouts.SandboxWSHeartbeatInterval,
			timeouts.SandboxWSReconnectMinBackoff, timeouts.SandboxWSReconnectMaxBackoff)
		handler.bridge = bridge

		// (§29.6): inject every resolved oauth-kind credential
		// into OpenCode's own auth store, ONE PUT /auth/{providerID} call
		// per provider, sequenced strictly HERE -- after Spawn already
		// reported healthy (agentRuntime exists) and bridge already
		// exists (so a failure can be reported as a wire Warning below),
		// but BEFORE bridge.Run(ctx) starts accepting inbound commands
		// (a few lines down): "sequenced inside the spawn/readiness path
		// so a turn can never race an unauthenticated provider" (§29.6).
		// Failure is logged and emitted as a non-fatal wire Warning, NEVER
		// a boot failure (§29.6) -- the credential is delivered
		// independently of whether this session's turns will ever name an
		// openai/... model; a turn that does need it then fails typed
		// (ProviderAuthError) into the ordinary §8.7 recovery UX instead.
		for provider, value := range providerCredentialOAuthSets(resolvedCredentials) {
			if value.Access == nil || value.Expires == nil {
				slog.Warn("sandbox-agent: oauth credential missing access/expires, skipping auth injection", "provider", provider)
				continue
			}
			accountID := ""
			if value.AccountID != nil {
				accountID = *value.AccountID
			}
			if err := agentRuntime.SetOAuthAuth(ctx, provider, opencode.OAuthCredential{
				Access:    *value.Access,
				Expires:   *value.Expires,
				AccountID: accountID,
			}); err != nil {
				slog.Warn("sandbox-agent: set oauth auth failed", "provider", provider, "error", err)
				warnMsg := sandboxws.Warning{
					Type:      "warning",
					MessageId: uuid.NewString(),
					SessionId: cfg.SessionConfig.SessionId,
					Gen:       cfg.SessionConfig.Gen,
					Message:   fmt.Sprintf("failed to inject %s credential into the coding agent; turns naming an %s/... model may fail until this is resolved", provider, provider),
				}
				if sendErr := bridge.SendBestEffort(ctx, warnMsg); sendErr != nil {
					slog.Warn("sandbox-agent: send oauth-auth-failure warning over WS bridge failed", "provider", provider, "error", sendErr)
				}
			}
		}
	}

	// reportBootProgress forwards each §6.1 boot_progress event over the
	// real WS bridge when one exists (a live session) -- AND always logs a
	// service-level failure locally too, regardless of whether a bridge
	// exists: an earlier version of this Step only logged event.Err in the
	// nil-bridge fallback branch, silently dropping the diagnostic reason
	// for a real service-boot failure whenever a live bridge was present
	// (the wire boot_progress event itself has no error field to carry it
	// either, so the local log is the ONLY place this information survives
	// at all on that path).
	reportBootProgress := func(event services.BootProgressEvent) {
		if bridge != nil {
			if sendErr := bridge.SendBootProgress(ctx, event); sendErr != nil {
				slog.Warn("sandbox-agent: send boot_progress over WS bridge failed",
					"service", event.ServiceName, "phase", string(event.Phase), "error", sendErr)
			}
		}
		if event.Phase == services.PhaseFailed {
			slog.Info("sandbox-agent: boot_progress",
				"service", event.ServiceName, "phase", string(event.Phase), "error", event.Err)
			return
		}
		if bridge == nil {
			slog.Info("sandbox-agent: boot_progress", "service", event.ServiceName, "phase", string(event.Phase))
		}
	}

	// Start the WS bridge (or, when there's no live session, the equivalent
	// ctx-wait goroutine) BEFORE cloning/booting -- not after. An earlier
	// version of this Step only launched bridge.Run once runBootSequence
	// below had already returned, which meant NO WS connection existed for
	// the entire boot window: nothing reportBootProgress sent during
	// cloning/hooks/services would reach the control plane until boot was
	// already fully done, silently defeating §3.2's "boot-progress reports
	// re-arm the connecting deadline" rule and resilience scenario §9.3 #3
	// (a slow boot must not cause a false kill). Bridge.Run's own outbound
	// buffer already handles "SendBootProgress called before the connection
	// has finished dialing" gracefully (buffer now, flush once connected),
	// so starting it concurrently with the boot sequence costs nothing and
	// fixes a real bug. A single errgroup.Group either way (no naked `go`
	// statement, §11): the nil-bridge branch's own goroutine does exactly
	// what a direct `<-ctx.Done()` would, just launched through the group
	// so both cases converge identically below.
	var group errgroup.Group
	// budgetSrvGroup is §26.5's own SEPARATE errgroup for
	// budgetServer.Serve() -- see the comment below (right where
	// budgetSrvGroup.Go is actually called) for why this must NOT be the
	// SAME group as the one immediately above.
	var budgetSrvGroup errgroup.Group

	// cloudIdentityRefreshCtx/cancelCloudIdentityRefresh/
	// cloudIdentityRefreshGroup ("cloud identity: sandbox-side
	// consumption + kubeconfig injection", §27.3) give the background
	// token-refresh loop (runCloudIdentityRefreshLoop, cloudidentity.go)
	// its OWN separate errgroup AND its OWN explicitly-canceled derived
	// context -- deliberately NOT a member of "group" immediately above,
	// for the SAME class of reason budgetSrvGroup is kept separate from
	// bridge.Run/the ctx-wait stand-in (§26.5's own comment just below):
	// bridge.Run(ctx) can return via a *wsbridge.FatalConnectError WITHOUT
	// ever canceling ctx itself (wsbridge/run.go's own documented
	// contract), and runCloudIdentityRefreshLoop's own loop has NO other
	// exit condition besides ctx.Done() (unlike a turn goroutine, whose
	// own ProviderHardCap-style ceiling bounds handler.group.Wait() even
	// when ctx is not literally canceled) -- so folding it into "group"
	// would risk blocking group.Wait() forever on exactly that fatal-
	// status path. cancelCloudIdentityRefresh is called explicitly,
	// unconditionally, immediately after group.Wait() returns (below),
	// GUARANTEEING this loop unwinds regardless of which of the three
	// ways group.Wait() itself converged, rather than depending on ctx's
	// own cancellation state at that point -- the deferred call here is
	// only a safety net for an early return above this point (none exists
	// today, since every earlier failure path already returns before this
	// line is reached, but costs nothing to have).
	cloudIdentityRefreshCtx, cancelCloudIdentityRefresh := context.WithCancel(ctx)
	defer cancelCloudIdentityRefresh()
	var cloudIdentityRefreshGroup errgroup.Group
	if cfg.SessionConfig != nil {
		cloudIdentityRefreshGroup.Go(func() error {
			return runCloudIdentityRefreshLoop(cloudIdentityRefreshCtx, cfg, timeouts, cloudIdentityMintClient, cloudIdentityStates)
		})
	}

	if bridge != nil {
		group.Go(func() error {
			return bridge.Run(ctx)
		})
	} else {
		group.Go(func() error {
			<-ctx.Done()
			return nil
		})
	}

	// (§26.7/§26.9): budgetServer's own Accept loop runs on its OWN
	// errgroup (budgetSrvGroup), deliberately NOT the "group" var above --
	// group.Wait() (below) is this function's own convergence signal for
	// "the bridge (or, headless, the ctx-wait stand-in) is done", reached
	// either by ctx being canceled OR by bridge.Run returning entirely on
	// its own (a *FatalConnectError from a 401/403/404/410 handshake,
	// wsbridge/run.go's own doc comment -- notably NOT accompanied by any
	// cancellation of ctx, since Run only ever OBSERVES ctx, never cancels
	// it). Folding budgetServer.Serve() into that SAME group -- an earlier
	// version of this Step's own code did exactly that, gated on a SECOND
	// group member that waited on ctx.Done() before calling Shutdown --
	// would deadlock precisely on that fatal-status path: group.Wait()
	// would then also need THAT watcher goroutine to finish, which itself
	// waits on ctx.Done(), which never fires until run() itself reaches its
	// own deferred stop() call at the very bottom of this function -- but
	// run() can never reach there while still blocked on THIS group.Wait().
	// Kept on its own group instead, Shutdown is called explicitly, always,
	// in the unconditional teardown block below (the SAME place sup.StopAll
	// already runs, reusing its own bounded shutdownCtx) -- reached
	// regardless of which of the three ways group.Wait() (below) converged.
	if budgetServer != nil {
		budgetSrvGroup.Go(func() error {
			return budgetServer.Serve()
		})
	}

	// onGitSync translates each internal/sandboxagent/gitclone.SyncAll phase
	// (§3.4 "gitstate in-sandbox") into an outbound sandboxws.
	// GitSync event, mirroring reportBootProgress's own "forward over the
	// bridge when one exists, always log locally too" shape immediately
	// above. git_sync is a best-effort event with no ackId (events.
	// schema.json's own GitSync def), sent via the SAME Bridge.
	// SendBestEffort every other non-critical event already uses
	// (SendBootProgress just above, HandlePrompt's own sink closure) --
	// SyncAll itself never blocks or waits on this call's own outcome
	// before proceeding to its next phase. Only ever invoked from within
	// runBootSequence's own cfg.SessionConfig != nil branch (below), so
	// cfg.SessionConfig is always non-nil here in practice; the nil guards
	// below mirror reportBootProgress's own defensive style even though,
	// by that exact same construction, neither bridge nor cfg.SessionConfig
	// is ever nil whenever this closure is actually invoked.
	onGitSync := func(repoName, status, branch string) {
		slog.Info("sandbox-agent: git_sync", "repo", repoName, "status", status, "branch", branch)
		if bridge == nil || cfg.SessionConfig == nil {
			return
		}
		msg := sandboxws.GitSync{
			Type:      "git_sync",
			MessageId: uuid.NewString(),
			SessionId: cfg.SessionConfig.SessionId,
			Gen:       cfg.SessionConfig.Gen,
			Status:    sandboxws.GitSyncStatus(status),
			Branch:    branch,
			Repo:      repoName,
		}
		if sendErr := bridge.SendBestEffort(ctx, msg); sendErr != nil {
			slog.Warn("sandbox-agent: send git_sync over WS bridge failed",
				"repo", repoName, "status", status, "error", sendErr)
		}
	}

	// sendBootTiming (§33.3) forwards ONE already-measured
	// sandbox_agent_*_duration_seconds data point as a best-effort
	// boot_timing sandbox-ws event -- mirroring onGitSync's own "forward
	// over the bridge when one exists, always log locally too" shape
	// exactly. This REPLACES what used to be four local OTel histogram
	// recordings (boot.RecordBootDuration/recordHookRerunDuration,
	// gitclone's own recordGitFetchDuration/recordGitCheckoutDuration, all
	// deleted by this Step): §33.1/§33.2 record why recording inside the
	// ephemeral sandbox process, and deriving these durations control-plane
	// -side from other wire signals, both fail -- the fact itself must
	// cross the wire, already measured, and the control plane records the
	// histogram (internal/app/sessionactor/opsmetrics.go). evt.Type/
	// MessageId/SessionId/Gen are filled in here, once, so every call site
	// below only needs to set Metric/Seconds and whichever tags its own
	// metric carries.
	sendBootTiming := func(evt sandboxws.BootTiming) {
		if bridge == nil || cfg.SessionConfig == nil {
			return
		}
		evt.Type = "boot_timing"
		evt.MessageId = uuid.NewString()
		evt.SessionId = cfg.SessionConfig.SessionId
		evt.Gen = cfg.SessionConfig.Gen
		if sendErr := bridge.SendBestEffort(ctx, evt); sendErr != nil {
			slog.Warn("sandbox-agent: send boot_timing over WS bridge failed",
				"metric", string(evt.Metric), "error", sendErr)
		}
	}

	// onHookRerunTiming (§33.3) relays boot.RunHooks/RunBoot's own
	// per-hook timing (formerly boot.recordHookRerunDuration) -- repo rides
	// the event for per-session debugging only (events.schema.json's own
	// BootTiming def), never as a metric attribute (§33.3 point 3).
	onHookRerunTiming := func(repo, hook, bootMode string, workspaceMoved, failed bool, seconds float64) {
		sendBootTiming(sandboxws.BootTiming{
			Metric:         sandboxws.BootTimingMetricHookRerunDuration,
			Seconds:        seconds,
			Repo:           &repo,
			Hook:           &hook,
			BootMode:       &bootMode,
			WorkspaceMoved: &workspaceMoved,
			Failed:         &failed,
		})
	}

	// onGitFetchTiming/onGitCheckoutTiming (§33.3) relay gitclone.SyncAll's
	// own §19.3 fetch-step and checkout timing (formerly gitclone's own
	// recordGitFetchDuration/recordGitCheckoutDuration) -- same repo-rides-
	// the-event-only discipline as onHookRerunTiming above.
	onGitFetchTiming := func(repo string, seconds float64, degraded bool) {
		sendBootTiming(sandboxws.BootTiming{
			Metric:   sandboxws.BootTimingMetricGitFetchDuration,
			Seconds:  seconds,
			Repo:     &repo,
			Degraded: &degraded,
		})
	}
	onGitCheckoutTiming := func(repo string, seconds float64, failed bool) {
		sendBootTiming(sandboxws.BootTiming{
			Metric:  sandboxws.BootTimingMetricGitCheckoutDuration,
			Seconds: seconds,
			Repo:    &repo,
			Failed:  &failed,
		})
	}

	// Audit-remediation batch B7 (Finding 3, HIGH): bracket the WHOLE repo
	// prepare + RunBoot span with a wall-clock timer and relay it as
	// sandbox_agent_boot_duration_seconds's own §33.3 boot_timing event --
	// previously (and still, control-plane-side after this Step) nothing
	// else measures total boot-to-ready latency at all, so §19.6's "is a
	// hook rerun materially eroding the warm-boot latency win" gating
	// question has no denominator to compare sandbox_agent_hook_rerun_
	// duration_seconds against without it. Relayed regardless of bootErr
	// (tagged failed=bootErr!=nil): even a failed boot's own elapsed time
	// is a real data point, not one to discard. No callback threading
	// needed here (unlike the hook/fetch/checkout timings above): this
	// span is measured directly around runBootSequence, in this SAME
	// function, which already has sendBootTiming in scope.
	bootStart := time.Now()
	bootErr := runBootSequence(ctx, sup, cfg, timeouts, sandboxSecretEnv, bootDegradeNotes, reportBootProgress, onGitSync, onGitFetchTiming, onGitCheckoutTiming, onHookRerunTiming)
	bootMode := string(cfg.BootMode)
	bootFailed := bootErr != nil
	sendBootTiming(sandboxws.BootTiming{
		Metric:   sandboxws.BootTimingMetricBootDuration,
		Seconds:  time.Since(bootStart).Seconds(),
		BootMode: &bootMode,
		Failed:   &bootFailed,
	})

	// (TECHNICAL_PLAN.md §30.5): once boot itself succeeded, re-own
	// cfg.WorkspaceDir to the isolated agent runtime's own uid/gid --
	// BEFORE anything below marks the sandbox ready to receive a
	// "prompt" command. gitclone.CloneAll, the repo setup hooks and the
	// AGENTS.md/opencode.json writers folded into runBootSequence all ran
	// as sandbox-agent's own identity, never the runtime's.
	//
	// services.yml is the exception, and this comment used to include it
	// in that list wrongly: those commands ARE dropped to the runtime,
	// which is why RunBoot re-owns each repo before starting them. This
	// pass is still needed for everything written after that point, and
	// is idempotent over what RunBoot already did -- see
	// boot.ChownWorkspaceForRuntime's own doc
	// comment for why leaving that unfixed would leave the runtime
	// locked out of the one surface §30.5 requires stay usable. Guarded
	// on cfg.SessionConfig != nil: that is exactly the condition under
	// which opencodeproc.Spawn (and the os.MkdirAll that creates
	// cfg.WorkspaceDir in the first place, above) ever ran at all --
	// with no live session there is no runtime to re-own anything for,
	// and cfg.WorkspaceDir may not even exist.
	//
	// Folded into bootErr itself, not a separate error path: a chown
	// failure here gets the EXACT same fail-fast treatment as any other
	// boot failure below (cancel ctx, never mark boot complete, surface
	// as this process's own eventual exit error) -- a session that
	// booted but left its own runtime unable to read or write its
	// workspace is not one worth continuing, and letting the runtime
	// discover that itself, per-file, at arbitrary points during a turn,
	// is exactly the silent failure mode this codebase's own conventions
	// refuse to ship. bootFailed/sendBootTiming above are deliberately
	// UNAFFECTED by this: that event already reported runBootSequence's
	// own, narrower outcome before this step ever ran.
	if bootErr == nil && cfg.SessionConfig != nil {
		if chownErr := boot.ChownWorkspaceForRuntime(cfg.WorkspaceDir, cfg.RuntimeUID, cfg.RuntimeGID); chownErr != nil {
			bootErr = fmt.Errorf("sandbox-agent: re-own workspace for isolated runtime: %w", chownErr)
		}
	}

	if bootErr != nil {
		// A boot failure needs the same graceful convergence a fatal WS
		// status or an OS signal gets -- cancel ctx (stop is a genuine
		// context.CancelFunc, signal.NotifyContext's own doc comment;
		// calling it more than once, e.g. again via the deferred call
		// above, is safe) so the bridge/ctx-wait goroutine above actually
		// unwinds instead of running forever waiting for a reason to stop
		// that a boot failure alone would never give it.
		stop()
	} else {
		if bridge != nil {
			bridge.MarkBootComplete()
		}
		slog.Info("sandbox-agent: boot sequence complete")
	}

	runErr := group.Wait()

	// §27.4: unconditionally cancel the refresh loop's own derived
	// context THE MOMENT group.Wait() has converged (whichever of the
	// three ways it did) -- see this loop's own group construction, above,
	// for the full "why a separate context, why explicit cancellation
	// here" reasoning. cloudIdentityRefreshGroup.Wait() immediately after
	// is therefore always bounded: the loop's own select statement reacts
	// to this cancellation on its very next iteration (or is already
	// blocked waiting on exactly this signal).
	cancelCloudIdentityRefresh()
	if err := cloudIdentityRefreshGroup.Wait(); err != nil {
		slog.Warn("sandbox-agent: cloud identity token refresh loop returned an unexpected error", "error", err)
	}

	// Drain every turn goroutine commandHandler.HandlePrompt launched
	// (handler.group) before proceeding to the supervised-process shutdown
	// below -- by this point ctx is already canceled (whatever triggered
	// the convergence above), so any in-flight StartTurn call is already
	// unwinding per its own documented ctx-cancellation contract; this is
	// a bounded wait, not an open-ended one. A nil handler (cfg.
	// SessionConfig was nil) has nothing to drain.
	if handler != nil {
		_ = handler.group.Wait()
	}

	// Always attempt a bounded graceful shutdown of every supervised
	// process, regardless of why the above finished -- a fatal WS status or
	// a boot failure must not skip cleanup and orphan whatever hooks/
	// services already started (Setpgid'd process groups have no
	// Pdeathsig; nothing else reaps them if sandbox-agent exits without
	// running this).
	slog.Info("sandbox-agent: shutting down", "grace_period", timeouts.SupervisorShutdownTimeout.String())

	// Deliberately a fresh background context, not ctx: by this point ctx
	// is already canceled (that's exactly what triggers this shutdown),
	// and a canceled context would make StopAll fail immediately instead
	// of actually bounding the drain -- same reasoning as control-plane's
	// own shutdownOTel deferred call.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeouts.SupervisorShutdownTimeout)
	defer cancel()
	stopErr := sup.StopAll(shutdownCtx, timeouts.ProcessStopGracePeriod)

	// (§26.7/§26.9): shut the review-cost-budget loopback server
	// down here, unconditionally -- this is the ONE place reached
	// regardless of which of the three ways `runErr := group.Wait()` above
	// converged (normal ctx cancellation, a CP-issued shutdown, or a fatal
	// WS status that returns on its own without ever canceling ctx, see
	// budgetSrvGroup's own doc comment above for why Shutdown is
	// deliberately NOT triggered by a ctx-watcher instead). Reuses the SAME
	// bounded shutdownCtx sup.StopAll just used, rather than a second,
	// near-duplicate timeout. budgetSrvGroup.Wait() afterward drains
	// budgetServer's own Serve goroutine (Shutdown makes it return
	// promptly) -- never left running past this function's own return, the
	// same no-orphaned-listener bar the process supervisor already meets
	// for a different subsystem.
	if budgetServer != nil {
		if err := budgetServer.Shutdown(shutdownCtx); err != nil {
			slog.Warn("sandbox-agent: review-cost-budget server shutdown failed", "error", err)
		}
		if err := budgetSrvGroup.Wait(); err != nil {
			slog.Warn("sandbox-agent: review-cost-budget server Serve returned an unexpected error", "error", err)
		}
	}

	// A fatal handshake status (401/403/404/410, §6.1: "no retry") is NOT a
	// normal shutdown trigger -- it must propagate as run()'s own error
	// (main() then os.Exit(1)s) even though StopAll above already ran.
	var fatalErr *wsbridge.FatalConnectError
	if errors.As(runErr, &fatalErr) {
		return fmt.Errorf("sandbox-agent: WS bridge: %w", runErr)
	}

	// Every other outcome -- wsbridge.ErrShutdownRequested (a CP-issued
	// "shutdown" command), nil, or ctx.Err() (an OS signal via
	// signal.NotifyContext, or the boot-failure-triggered stop() above) --
	// is a normal convergence path, not logged as unexpected.
	if runErr != nil && !errors.Is(runErr, wsbridge.ErrShutdownRequested) && !errors.Is(runErr, context.Canceled) {
		// Not expected per Bridge.Run's own documented contract (only nil,
		// ctx.Err(), ErrShutdownRequested, or *FatalConnectError -- already
		// handled above -- are ever returned), but logged defensively
		// rather than silently ignored if that contract is ever violated.
		slog.Warn("sandbox-agent: WS bridge Run returned an unexpected error", "error", runErr)
	}

	if bootErr != nil {
		return fmt.Errorf("sandbox-agent: boot: %w", bootErr)
	}
	return stopErr
}

// logImageManifest logs whatever boot.LoadImageManifest(boot.ImageManifestPath)
// (runBootSequence's own one call site, below) actually found, split three
// ways -- previously, only the manifestErr != nil case logged anything at
// all, leaving the other two cases (a genuinely missing manifest under
// repo_image; a manifest that DID load) completely silent:
//
//   - manifestErr != nil: a real I/O/parse failure (unchanged from before
//     this Step).
//   - !manifestFound (and no error): entirely expected for every boot mode
//     OTHER than repo_image (no image-build step ever ran to bake one), so
//     only logged under repo_image, where a missing manifest is either a
//     pre-Step image or a build-service bug that silently forces EVERY repo
//     to rerun setup.sh via ComputeWorkspaceMoved's own safe default --
//     "working as designed, the repo moved" and "the build service stopped
//     baking manifests" must not look identical in the log.
//   - found: manifest.Fingerprint/BuiltAt (carried for diagnostic/log
//     purposes only, manifest.go's own doc comment on ImageManifest) and
//     BuiltRepoShas are logged -- the one place that purpose is actually
//     fulfilled: which baked image this sandbox is really running, and the
//     built_repo_shas its post-clone/-sync SHAs (logged separately, "boot
//     fingerprint (post-clone)") are about to be compared against for
//     §19.4's workspaceMoved decision. Additionally (audit-remediation
//     batch B7, Finding 5), any repo present in currentSHAs but ABSENT as
//     its own key in manifest.BuiltRepoShas is now named individually, at
//     Warn -- see logRepoMissingFromManifest's own doc comment for why this
//     is a distinct case from a repo whose SHA simply moved, and why it
//     previously had no log signal of its own at all.
//
// Factored out of runBootSequence specifically so it is unit-testable
// without touching the real filesystem at boot.ImageManifestPath's own
// fixed, absolute, /narvi/-rooted location (main_test.go's own
// TestLogImageManifest).
func logImageManifest(bootMode sandboxboot.BootMode, manifest boot.ImageManifest, manifestFound bool, manifestErr error, currentSHAs map[string]string) {
	switch {
	case manifestErr != nil:
		slog.Warn("sandbox-agent: read image manifest failed; treating every repo as workspace-moved (safe default, §19.4)",
			"path", boot.ImageManifestPath, "error", manifestErr)
	case !manifestFound:
		if bootMode == sandboxboot.BootModeRepoImage {
			slog.Warn("sandbox-agent: repo_image boot has no image manifest; treating every repo as workspace-moved (safe default, §19.4)",
				"path", boot.ImageManifestPath)
		}
	default:
		slog.Info("sandbox-agent: image manifest",
			"fingerprint", manifest.Fingerprint,
			"built_at", manifest.BuiltAt,
			"built_repo_shas", manifest.BuiltRepoShas,
		)
		logRepoMissingFromManifest(manifest, currentSHAs)
	}
}

// logRepoMissingFromManifest implements the fix for Finding 5 (LOW,
// audit-remediation batch B7): boot.ComputeWorkspaceMoved's own per-repo
// predicate already folds two genuinely different cases into the identical
// workspaceMoved: true outcome -- (a) a repo whose checked-out SHA simply
// differs from manifest.BuiltRepoShas[name] (a normal, expected image/
// workspace drift), and (b) a repo that is not a key in
// manifest.BuiltRepoShas AT ALL (e.g. added to this session's repo list
// after the image was last baked, so the build service never had a chance
// to record a built SHA for it). Before this fix, an operator could only
// tell the two apart by manually diffing this function's own "image
// manifest" log line's built_repo_shas map against the separate "boot
// fingerprint (post-clone)" log line's repo_shas map -- a partial
// build-service gap (one new repo never gets baked in) read identically to
// routine per-repo drift in every log line that actually exists.
//
// Logs one Warn line per such repo, sorted by name for deterministic
// output -- this is expected to be rare (it only fires for case (b) above,
// not for every ordinary SHA-moved repo, which is still covered by the
// unremarkable "image manifest" Info line plus the existing per-repo
// workspace_moved:true boot_progress signal from hooks.go), so a handful of
// extra Warn lines is a fair cost for making a real, previously-invisible
// build-service gap surface on its own.
func logRepoMissingFromManifest(manifest boot.ImageManifest, currentSHAs map[string]string) {
	if len(currentSHAs) == 0 {
		return
	}
	names := make([]string, 0, len(currentSHAs))
	for name := range currentSHAs {
		if _, ok := manifest.BuiltRepoShas[name]; ok {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		slog.Warn("sandbox-agent: repo absent from image manifest's built_repo_shas (distinct from a genuine SHA move -- likely added to this session's repo list after the image was last baked); treating as workspace-moved (safe default, §19.4)",
			"repo", name)
	}
}

// runBootSequence prepares every repo cfg.SessionConfig names (in order),
// writes the generated AGENTS.md manifest (§6.4), then runs boot.RunBoot
// against the successfully-prepared subset. repos/preparation is skipped
// entirely when cfg.SessionConfig is nil (the common dev/test case) --
// boot.RunBoot's own documented, correct no-op on an empty repo list
// handles that.
//
// §3.4 ("gitstate in-sandbox", §3.4) splits "prepare every repo" on
// cfg.BootMode, the exact, principled dispatch point internal/domain/
// sandboxboot.BootMode already names: BootModeBuild/BootModeFresh mean "no
// prior image/snapshot to build on" -- the workspace does NOT yet exist on
// disk -- so this is the existing gitclone.CloneAll path, UNCHANGED, a
// real `git clone` into an empty directory. BootModeRepoImage/
// BootModeSnapshotRestore mean the workspace's own real git repo ALREADY
// EXISTS on disk (baked into the image at build time, or restored from a
// snapshot), so this instead calls the NEW gitclone.SyncAll (§3.4:
// "stash-if-dirty -> checkout session branch -> stash pop") -- CloneAll is
// never called on this path at all; cloning again into a non-empty
// directory would conflict with what is already there.
//
// secretEnv (§27.1, adversarial-review HIGH fix) is run()'s own
// already-built sandboxSecretEnv slice -- passed straight through to
// boot.RunBoot, which threads it on into every hook/services.yml spawn.
// nil is a correct, safe input (every existing test call site already
// passes nil, matching this parameter's own pre-existing absence).
//
// degradeNotes (§27.1, adversarial-review LOW fix) is run()'s own
// bootDegradeNotes slice -- zero or more human-readable notes about a
// boot-time fetch that degraded via warn-and-continue (a sandbox-secrets
// or opencode-config fetch that exhausted every retry attempt) -- folded
// into the generated AGENTS.md manifest below (gitclone.WriteAgentsManifest),
// §27.1's own explicit "recorded in the boot log and AGENTS.md"
// requirement. nil/empty (the overwhelming common case: every boot-time
// fetch succeeded, or a session has none of this Step's own fetches to
// begin with) omits the manifest's own degrade-notice section entirely.
//
// onGitFetchTiming/onGitCheckoutTiming/onHookRerunTiming (§33.3)
// are threaded straight through to gitclone.SyncAll and boot.RunBoot
// respectively, exactly like reportBootProgress/onGitSync above -- see
// run()'s own sendBootTiming closure (this function's caller) for what
// they relay and why.
func runBootSequence(
	ctx context.Context,
	sup *supervisor.Supervisor,
	cfg boot.Config,
	timeouts platform.Timeouts,
	secretEnv []string,
	degradeNotes []string,
	reportBootProgress services.ProgressReporter,
	onGitSync gitclone.OnGitSync,
	onGitFetchTiming gitclone.OnGitFetchTiming,
	onGitCheckoutTiming gitclone.OnGitCheckoutTiming,
	onHookRerunTiming boot.OnHookRerunTiming,
) error {
	// §30.4(3)'s own boot-time credential-cache purge, unconditional,
	// every mode -- run BEFORE anything in this boot sequence has a chance
	// to write a fresh credential into cfg.CredentialCacheDir. This is
	// explicitly NOT load-bearing on its own: whether the provider's
	// restore is a cold re-boot (this code runs) or a warm resume (it does
	// not) is the open Modal question §30.9 names, and nothing here may
	// rest on the unverified answer. The load-bearing control for a
	// restored snapshot is a purge at snapshot-MINT time, so that no
	// snapshot ever contains a credential in the first place -- THAT purge
	// now IS written (HandleSnapshot, above, purges before ever building
	// the mint request, and aborts the whole snapshot attempt if the purge
	// itself fails), so a restored snapshot is defended primarily by that
	// mint-time purge, with this boot-time call and the forced read-only
	// mint on the build path as its own, independent defense-in-depth.
	//
	// A failure purging a directory that may not even exist yet is
	// unexpected enough to be worth failing loudly rather than silently
	// proceeding into a boot that might reuse a stale credential.
	if err := (&credentials.Cache{Dir: cfg.CredentialCacheDir}).PurgeAll(); err != nil {
		return fmt.Errorf("purge credential cache at boot: %w", err)
	}

	var repos []boot.RepoInfo
	// workspaceMoved (§19.4) stays nil for a nil-SessionConfig boot
	// (the dev/test no-op case, exactly like repos itself) -- boot.RunBoot's
	// own runRepoHooks call treats a nil map as "every repo defaults to
	// workspaceMoved: true" (workspaceMovedFor's own safe default), which is
	// moot anyway since repos is empty in that case too.
	var workspaceMoved map[string]bool
	// setupRerunLadder (§19.6) stays nil for a nil-SessionConfig
	// boot too -- boot.RunBoot's own ladderFor call treats a nil map as
	// "fall through to full setup.sh", the correct floor, and is moot
	// anyway since repos itself is empty in that case (mirroring
	// workspaceMoved's own identical comment just above).
	var setupRerunLadder map[string]boot.SetupRerunLadder
	if cfg.SessionConfig != nil {
		var manifestInput []gitclone.CloneResult

		// pathScope (§14.1) is extracted ONCE, here, before the switch below
		// -- it is NOT fresh-clone-specific. A repo_image/snapshot_restore
		// boot's own fingerprint (domain/imagebuild.Fingerprint, §19.1) is
		// keyed on (base, repos map[name]url, runtimeVersion) -- each
		// repo's normalized clone URL, NOT a resolved SHA -- so it stays
		// scope-independent AND SHA-independent: the exact same prebuilt
		// image (or restored snapshot) is shared across every session with
		// the same repo SET, regardless of path_scope or which commit each
		// repo happened to be at when the image was built or last
		// refreshed (§19.2's freshness pump). That means the on-disk
		// sparse-checkout state a SyncAll boot finds reflects whatever
		// scope (or lack of one) happened to produce that image/snapshot,
		// NOT necessarily THIS session's own scope -- relying on "git
		// preserves a working tree's sparse-checkout config" would
		// silently carry over the WRONG session's scope (or a full,
		// unscoped checkout) rather than enforce this session's own. This
		// is more load-bearing than it was pre-existing: URL-keyed images
		// are shared far more broadly than SHA-keyed ones were, so more
		// sessions with differing path_scope can land on one image. Both
		// switch cases below therefore need pathScope: CloneAll applies it
		// to a brand-new clone, and SyncAll re-applies/re-narrows it against
		// whatever already exists on disk, so the two never drift.
		var pathScope []string
		if cfg.SessionConfig.PathScope != nil {
			pathScope = []string(*cfg.SessionConfig.PathScope)
		}

		switch cfg.BootMode {
		case sandboxboot.BootModeRepoImage, sandboxboot.BootModeSnapshotRestore:
			results, syncErr := gitclone.SyncAll(ctx, sup, cfg.WorkspaceDir, cfg.SessionConfig.Repos, pathScope,
				cfg.SessionConfig.SessionId, timeouts.GitFetchStepTimeout, timeouts.GitSyncStepTimeout, timeouts.ProcessStopGracePeriod, onGitSync,
				onGitFetchTiming, onGitCheckoutTiming)
			if syncErr != nil {
				return fmt.Errorf("sync repos: %w", syncErr)
			}
			manifestInput = make([]gitclone.CloneResult, len(results))
			for i, result := range results {
				manifestInput[i] = result.ToCloneResult()
				if result.Err != nil {
					continue
				}
				repos = append(repos, boot.RepoInfo{Name: result.Repo.Name, Primary: result.Primary})
			}
		default:
			// BootModeBuild/BootModeFresh (and any future mode this switch
			// does not yet know about -- boot.Load()'s own ParseBootMode has
			// already rejected anything outside the four §6.4 values by the
			// time cfg reaches here, so falling through to the existing,
			// pre-existing behavior is the correct, conservative default).
			results, cloneErr := gitclone.CloneAll(ctx, sup, cfg.WorkspaceDir, cfg.SessionConfig.Repos, pathScope,
				timeouts.RepoCloneTimeout, timeouts.ProcessStopGracePeriod)
			if cloneErr != nil {
				return fmt.Errorf("clone repos: %w", cloneErr)
			}
			manifestInput = results
			for _, result := range results {
				if result.Err != nil {
					continue
				}
				repos = append(repos, boot.RepoInfo{Name: result.Repo.Name, Primary: result.Primary})
			}
		}

		if err := gitclone.WriteAgentsManifest(cfg.WorkspaceDir, manifestInput, degradeNotes); err != nil {
			return fmt.Errorf("write AGENTS.md: %w", err)
		}

		// The FIRST boot-fingerprint line (in run(), above this function)
		// necessarily reported repo_shas as empty -- §5.3 requires it
		// logged first, and nothing was cloned/synced yet at that point. Now
		// that this repo preparation has happened (via either branch above),
		// re-collect and log an updated fingerprint so repo_shas actually
		// carries the information §5.3 asks for on this exact path; nothing
		// about the original "logs first" line is changed or replaced, this
		// is a second, supplementary log line. openCodeVersion is passed as
		// "" here -- this log line's own purpose is repo_shas specifically
		// (run()'s own separate "post-opencode-spawn" supplementary line
		// already covers that field, logged before runBootSequence is ever
		// called).
		postCloneFingerprint := boot.CollectFingerprint(cfg, timeouts.RepoSHADiscoveryTimeout, "")
		slog.Info("sandbox-agent: boot fingerprint (post-clone)",
			"repo_shas", postCloneFingerprint.RepoSHAs,
		)

		// §19.4's own workspaceMoved computation: read
		// /narvi/image-manifest.json ONCE per boot (never per-repo -- one
		// manifest covers every repo in the image, §19.1 point 4) and
		// compare each repo's just-collected post-clone/-sync checked-out
		// SHA (postCloneFingerprint.RepoSHAs, computed immediately above --
		// reused rather than re-discovered a second time) against that
		// manifest's own built_repo_shas[name]. A missing/unreadable
		// manifest (every non-repo_image boot mode, or a repo_image image
		// that predates this Step) is handled entirely inside
		// boot.ComputeWorkspaceMoved's own documented safe default (every
		// repo defaults to workspaceMoved: true) -- computed unconditionally
		// here, regardless of cfg.BootMode, since sandboxboot.EvaluateHook
		// only ever actually CONSULTS this value for the one cell that
		// matters (repo_image + HookSetup); every other mode's own hook
		// policy ignores it entirely, so computing it uniformly costs
		// nothing and keeps this call site simple.
		manifest, manifestFound, manifestErr := boot.LoadImageManifest(boot.ImageManifestPath)
		logImageManifest(cfg.BootMode, manifest, manifestFound, manifestErr, postCloneFingerprint.RepoSHAs)
		workspaceMoved = boot.ComputeWorkspaceMoved(manifest, manifestFound, postCloneFingerprint.RepoSHAs)

		// §19.6's own graduated setup-rerun ladder: computed
		// uniformly right alongside workspaceMoved (same manifest, same
		// postCloneFingerprint.RepoSHAs, same "costs nothing to compute for
		// every mode even though only repo_image ever consults it"
		// reasoning as workspaceMoved's own comment just above).
		// RepoSHADiscoveryTimeout is reused for the ladder's own small
		// local git-plumbing check (`git diff --quiet <built_sha> HEAD --
		// setup.sh`) rather than a new timeout constant -- the identical
		// class of operation DiscoverRepoSHAs' own repoHeadSHA already
		// bounds with this exact field.
		//
		// len(pathScope) > 0 (adversarial-review finding B5, §19.7): this
		// exact same pathScope slice was already computed above and passed
		// to CloneAll/SyncAll, so re-using it here (rather than re-deriving
		// it from cfg.SessionConfig.PathScope a second time) keeps the two
		// call sites' own notion of "is this session scoped" structurally
		// unable to drift. See ComputeSetupRerunLadder's own doc comment
		// for why a scoped session must always resolve the digest tier to
		// ineligible.
		setupRerunLadder = boot.ComputeSetupRerunLadder(manifest, manifestFound, len(pathScope) > 0, cfg.WorkspaceDir, postCloneFingerprint.RepoSHAs, timeouts.RepoSHADiscoveryTimeout)
	}

	// §27.5: dockerd is supervised ONCE per boot, before RunBoot's
	// own per-repo loop -- a session-level daemon, not scoped to any one
	// repo, so a repo's own services.yml (started inside RunBoot below)
	// can rely on it already being up if its own commands need Docker.
	// Gated on cfg.SessionConfig.Docker being true (nil-SessionConfig dev/
	// CI boots, and every Docker-false session, never call RunDocker at
	// all -- "the daemon simply never starts when the flag is off",
	// §27.5's own wording). env mirrors RunBoot's own identical
	// supervisor.EnvWithout(SessionConfigEnvVar)+secretEnv construction
	// immediately below -- dockerd has no more legitimate need to see the
	// sandbox's own plaintext bearer token than any other spawned process
	// does, and a customer's own configured HTTP_PROXY/HTTPS_PROXY/
	// NO_PROXY secrets (§27.6) route its own image pulls through a
	// configured proxy the same cooperative way every other spawned
	// process already gets them.
	if cfg.SessionConfig != nil && cfg.SessionConfig.Docker {
		if err := boot.RunDocker(ctx, sup, boot.DefaultDockerdBinary, boot.DefaultDockerSocketPath,
			append(supervisor.EnvWithout(boot.SessionConfigEnvVar), secretEnv...),
			reportBootProgress, timeouts.DockerReadinessTimeout, timeouts.ServiceReadinessPollInterval); err != nil {
			return fmt.Errorf("boot: docker: %w", err)
		}
	}

	// timeouts.SetupRerunRetryBackoff (§19.6) paces the ONE retry
	// of a failed full setup.sh rerun -- see runSetupRerunLadder's own doc
	// comment (hooks.go). It carries the same value as the OpenCode
	// adapter's own transient-retry pause today and is still a separate
	// field on purpose: that one paces a retry inside a live turn, where
	// added delay is felt by a waiting human, while this one paces a
	// package-registry reinstall inside the boot sequence, which has its
	// own separate budget and a slower, network-bound failure profile.
	// Equal values today are a coincidence of tuning, not one concept.
	if err := boot.RunBoot(ctx, sup, cfg.WorkspaceDir, repos, cfg.BootMode, workspaceMoved, setupRerunLadder, secretEnv, reportBootProgress, onHookRerunTiming,
		timeouts.HookTimeout, timeouts.ProcessStopGracePeriod,
		timeouts.ServiceReadinessTimeout, timeouts.ServiceReadinessPollInterval,
		timeouts.SetupRerunRetryBackoff, runtimeCredentialFor(cfg),
		// Re-own each repo before its own services.yml commands start,
		// because those are dropped to the runtime uid and every writer
		// before them ran as this process. Guarded exactly like the
		// post-boot chown below: with no live session there is no runtime
		// to re-own anything for.
		func(repoDir string) error {
			if cfg.SessionConfig == nil {
				return nil
			}
			return boot.ChownWorkspaceForRuntime(repoDir, cfg.RuntimeUID, cfg.RuntimeGID)
		}); err != nil {
		return fmt.Errorf("boot: %w", err)
	}

	// §3.4 ("Image builds must snapshot a clean tree") / §3.4's own
	// Part E: ONLY for a BootModeBuild boot, and ONLY once RunBoot itself
	// has already returned successfully -- a failed setup.sh in build mode
	// is already fatal per BootModeBuild's own existing primary-fatal
	// semantics (sandboxboot.EvaluateHook), so this point is never reached
	// on that failure path at all; there is nothing to gate here beyond
	// "did RunBoot succeed". This runs BEFORE this function returns --
	// i.e. before whatever the existing boot-sequence-complete signal is
	// (run()'s own "boot sequence complete" log line and bridge.
	// MarkBootComplete call, immediately after runBootSequence returns) --
	// so that by the time the provider is free to snapshot, the tree is
	// guaranteed clean. repos is empty (a correct no-op) whenever
	// cfg.SessionConfig was nil, exactly like every other repos-driven step
	// above.
	if cfg.BootMode == sandboxboot.BootModeBuild {
		repoNames := make([]string, len(repos))
		for i, r := range repos {
			repoNames[i] = r.Name
		}
		if err := gitclone.CleanForImageBuild(ctx, sup, cfg.WorkspaceDir, repoNames, cfg.CredentialCacheDir,
			timeouts.GitSyncStepTimeout, timeouts.ProcessStopGracePeriod); err != nil {
			return fmt.Errorf("clean workspace before snapshot: %w", err)
		}
	}

	return nil
}

// runtimeCredentialFor builds the identity every customer-influenced
// process runs under (§30.5): the agent runtime itself, and the
// services.yml commands and setup hooks that come from the customer's own
// repository.
//
// One function rather than a literal at each site, because the
// NoSetGroups condition below is subtle enough to get wrong in a copy, and
// two sites disagreeing about it would mean two different identities in
// one sandbox -- with files written by one that the other cannot touch.
func runtimeCredentialFor(cfg boot.Config) *syscall.Credential {
	// NoSetGroups is true ONLY when the configured identity is exactly
	// this process's own, which is the unprivileged case a test run hits:
	// clearing supplementary groups needs privileges this process does not
	// have, and the drop would fail outright.
	//
	// In the real production path cfg.RuntimeUID/RuntimeGID are never this
	// process's own identity -- boot refuses 0, and the default is 65534
	// -- so this condition does not trigger there, supplementary groups
	// are genuinely cleared, and the drop is the real one.
	selfUID, selfGID := uint32(os.Getuid()), uint32(os.Getgid())
	return &syscall.Credential{
		Uid:         cfg.RuntimeUID,
		Gid:         cfg.RuntimeGID,
		NoSetGroups: cfg.RuntimeUID == selfUID && cfg.RuntimeGID == selfGID,
	}
}
